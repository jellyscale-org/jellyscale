// Package signin bounds failed sign-ins (JS-145, D-020): per name, per known device and per address, in Postgres so that
// every replica counts the same. docs/access.md says what each bound is and why.
//
// An attempt reserves its failure before its password is compared (Begin), so that attempts made at once cannot all be
// checked: the reservation is made under a lock on each key it counts toward, and a right password gives its reservation
// back. A wrong one has been stored before its 401 is sent, and an attempt that cannot reserve is never answered.
package signin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	window       = 900 // Seconds a failure counts, and a refusal lasts.
	NameLimit    = 20  // Failures of devices new to the account, for one name.
	DeviceLimit  = 10  // Failures of one known device for one account.
	AddressLimit = 10  // Failures from one address (a /64 for IPv6).
	KnownDays    = 30  // Days a device stays known after its token ended.
	// UnknownNames is how many names that are no account are remembered.
	UnknownNames = 100_000
	countStripes = 32
)

// Bounds, as logged and counted.
const (
	BoundName     = "name"
	BoundDevice   = "device"
	BoundAddress  = "address"
	BoundApproval = "approval"
	// ApprovalLimit is D-078's ten failed approvals in a quarter of an hour, refused for that long.
	ApprovalLimit = 10
)

// Attempt is who signs in. UserID is "" for a name that is no account.
type Attempt struct {
	Name, UserID, DeviceID string
	// Address is what Key made of the client's address, "" when no address is counted.
	Address string
}

// Refusal says an attempt is refused: by which bound, and for how long (whole seconds, 1 to 900).
type Refusal struct {
	Bound string
	After time.Duration
}

// Ticket is a reservation, held until the password is compared.
type Ticket struct{ ids []int64 }

type key struct {
	bound, key string
	limit      int
}

// nameKey is a hash, so that a name typed into the box (viewers type passwords there) is never stored as typed.
func nameKey(name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(name)))
	return hex.EncodeToString(sum[:])
}

// Begin reserves the attempt's failure toward every bound it counts toward, or says which refuses it and for how long
// (and then counts nothing). An attempt from a known device counts toward the device's bound and not the name's.
func Begin(ctx context.Context, pool *pgxpool.Pool, a Attempt) (Ticket, *Refusal, error) {
	var ticket Ticket
	var refusal *Refusal
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ticket, refusal = Ticket{}, nil
		var keys []key
		if a.Address != "" {
			keys = append(keys, key{BoundAddress, a.Address, AddressLimit})
		}
		known := false
		if a.UserID != "" && a.DeviceID != "" {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE user_id = $1 AND device_id = $2)
				OR EXISTS (SELECT 1 FROM signin_device_ended WHERE user_id = $1 AND device_id = $2
					AND ended_at > clock_timestamp() - $3 * interval '1 day')`, a.UserID, a.DeviceID, KnownDays).Scan(&known); err != nil {
				return err
			}
		}
		if known {
			keys = append(keys, key{BoundDevice, a.UserID + " " + a.DeviceID, DeviceLimit})
		} else {
			keys = append(keys, key{BoundName, nameKey(a.Name), NameLimit})
		}
		if err := lock(ctx, tx, keys); err != nil {
			return err
		}
		for _, k := range keys {
			after, err := refusedFor(ctx, tx, k)
			if err != nil {
				return err
			}
			if after > 0 {
				if refusal == nil {
					refusal = &Refusal{Bound: k.bound}
				}
				refusal.After = max(refusal.After, after)
			}
		}
		if refusal != nil {
			return nil
		}
		// Old failures go with the attempt that finds them, so that nothing grows with the addresses that came and went.
		if _, err := tx.Exec(ctx, `WITH old AS (DELETE FROM signin_failures WHERE id IN (
				SELECT id FROM signin_failures WHERE at < clock_timestamp() - $1 * interval '2 seconds' FOR UPDATE SKIP LOCKED)),
			ended AS (DELETE FROM signin_device_ended WHERE ended_at < clock_timestamp() - $2 * interval '1 day')
			SELECT 1`, window, KnownDays); err != nil {
			return err
		}
		for _, k := range keys {
			id, err := fail(ctx, tx, k.bound, k.key, a.UserID == "" && k.bound == BoundName)
			if err != nil {
				return err
			}
			ticket.ids = append(ticket.ids, id)
		}
		return nil
	})
	if err != nil {
		return Ticket{}, nil, err
	}
	return ticket, refusal, nil
}

// BeginApproval reserves one failed Quick Connect approval toward the approving account (D-078). It is called before
// the code is looked at, so approvals sent at once cannot all check a code. An approval that succeeds gives the
// reservation back with Release; an unknown, expired or already approved code keeps it. Nothing but this account's
// approvals count toward it: a refusal checks no code, stores nothing and lives its own quarter of an hour.
func BeginApproval(ctx context.Context, pool *pgxpool.Pool, userID string) (Ticket, *Refusal, error) {
	var ticket Ticket
	var refusal *Refusal
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		ticket, refusal = Ticket{}, nil
		approval := key{BoundApproval, userID, ApprovalLimit}
		if err := lock(ctx, tx, []key{approval}); err != nil {
			return err
		}
		after, err := refusedFor(ctx, tx, approval)
		if err != nil {
			return err
		}
		if after > 0 {
			refusal = &Refusal{Bound: approval.bound, After: after}
			return nil
		}
		// The same cleanup as Begin: failures still held by another attempt are left for it, and a device long
		// forgotten stops keeping an account's bound alive.
		if _, err := tx.Exec(ctx, `WITH old AS (DELETE FROM signin_failures WHERE id IN (
				SELECT id FROM signin_failures WHERE at < clock_timestamp() - $1 * interval '2 seconds' FOR UPDATE SKIP LOCKED)),
			ended AS (DELETE FROM signin_device_ended WHERE ended_at < clock_timestamp() - $2 * interval '1 day')
			SELECT 1`, window, KnownDays); err != nil {
			return err
		}
		id, err := fail(ctx, tx, approval.bound, approval.key, false)
		if err != nil {
			return err
		}
		ticket.ids = append(ticket.ids, id)
		return nil
	})
	if err != nil {
		return Ticket{}, nil, err
	}
	return ticket, refusal, nil
}

// Release gives the reservation back: the password was right, so nothing failed. A right password must leave no count
// behind, so it does not give up when the client hangs up (the context is detached) and tries again on a failed
// statement: a severed connection is the usual cause, and the delete is the same however often it runs.
func (t Ticket) Release(ctx context.Context, pool *pgxpool.Pool) (err error) {
	ctx = context.WithoutCancel(ctx)
	for range 3 {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = pool.Exec(attempt, `DELETE FROM signin_failures WHERE id = ANY($1)`, t.ids)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// lock serializes the attempts that count toward the same keys, in one order so that none waits on another for good.
func lock(ctx context.Context, tx pgx.Tx, keys []key) error {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.bound + " " + k.key
	}
	slices.Sort(names)
	for _, name := range names {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, name); err != nil {
			return err
		}
	}
	return nil
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// refusedFor is how long the key refuses attempts from now, in whole seconds rounded up, or 0. A refusal begins at the
// failure that brought the last 15 minutes to the limit and lasts 15 minutes from it, whatever age the failures behind it
// have; nothing else begins or lengthens one, as a refused attempt stores nothing.
func refusedFor(ctx context.Context, q queryer, k key) (time.Duration, error) {
	var seconds *int
	err := q.QueryRow(ctx, `SELECT ceil(extract(epoch FROM max(f.at) + $3 * interval '1 second' - clock_timestamp()))::int
		FROM signin_failures f
		WHERE f.kind = $1 AND f.key = $2 AND f.at > clock_timestamp() - $3 * interval '1 second'
		AND (SELECT count(*) FROM signin_failures g WHERE g.kind = f.kind AND g.key = f.key
			AND g.at > f.at - $3 * interval '1 second' AND g.at <= f.at) >= $4`, k.bound, k.key, window, k.limit).Scan(&seconds)
	if err != nil || seconds == nil {
		return 0, err
	}
	return time.Duration(min(max(*seconds, 1), window)) * time.Second, nil
}

// fail stores one failure. A name that is no account is also entered in signin_unknown, which holds the most
// UnknownNames of them: the one with the fewest failures, then the oldest, is forgotten with its failures. A name that
// failed before its account was made is an account now and is never forgotten: its failures are the account's count.
func fail(ctx context.Context, tx pgx.Tx, bound, name string, unknown bool) (id int64, err error) {
	if unknown {
		var added bool
		if err := tx.QueryRow(ctx, `INSERT INTO signin_unknown (key, failures, last_at) VALUES ($1, 1, clock_timestamp())
			ON CONFLICT (key) DO UPDATE SET failures = signin_unknown.failures + 1, last_at = clock_timestamp()
			RETURNING xmax = 0`, name).Scan(&added); err != nil {
			return 0, err
		}
		if added {
			// The count is spread over stripes, so that attempts entering names do not wait for one another's commit; what
			// it sums to is what other attempts have committed and this one's own, which may be a few off while many enter
			// at once, and then the next that finds too many forgets them.
			stripe := rand.IntN(countStripes)
			var n int
			if err := tx.QueryRow(ctx, `WITH added AS (INSERT INTO signin_unknown_count (stripe, n) VALUES ($1, 1)
					ON CONFLICT (stripe) DO UPDATE SET n = signin_unknown_count.n + 1 RETURNING stripe)
				SELECT (coalesce(sum(n), 0) + 1)::int FROM signin_unknown_count`, stripe).Scan(&n); err != nil {
				return 0, err
			}
			if n > UnknownNames {
				// SKIP LOCKED: a name another attempt is entering is not forgotten under it, and none waits for it.
				if _, err := tx.Exec(ctx, `WITH gone AS (DELETE FROM signin_unknown WHERE key IN (
						SELECT key FROM signin_unknown WHERE key <> $1
							AND NOT EXISTS (SELECT 1 FROM users WHERE encode(sha256(convert_to(lower(name), 'UTF8')), 'hex') = key)
							ORDER BY failures, last_at LIMIT $3 FOR UPDATE SKIP LOCKED) RETURNING key),
					failures AS (DELETE FROM signin_failures WHERE kind = $2 AND key IN (SELECT key FROM gone))
					UPDATE signin_unknown_count SET n = n - (SELECT count(*) FROM gone) WHERE stripe = $4`,
					name, BoundName, n-UnknownNames, stripe); err != nil {
					return 0, err
				}
			}
		}
	}
	err = tx.QueryRow(ctx, `INSERT INTO signin_failures (kind, key) VALUES ($1, $2) RETURNING id`, bound, name).Scan(&id)
	return id, err
}

// FailName counts one failed sign-in for the name (any letter case), an account or not, and stores it before it
// returns. It refuses nothing.
func FailName(ctx context.Context, pool *pgxpool.Pool, name string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		k := nameKey(name)
		if err := lock(ctx, tx, []key{{BoundName, k, NameLimit}}); err != nil {
			return err
		}
		var account bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(name) = lower($1))`, name).Scan(&account); err != nil {
			return err
		}
		_, err := fail(ctx, tx, BoundName, k, !account)
		return err
	})
}

// NameRefusal is how long attempts for the name are refused from now, or 0.
func NameRefusal(ctx context.Context, pool *pgxpool.Pool, name string) (time.Duration, error) {
	return refusedFor(ctx, pool, key{BoundName, nameKey(name), NameLimit})
}

// SignedOut ends the token and remembers its device for the account, so that the device stays known.
func SignedOut(ctx context.Context, pool *pgxpool.Pool, token string) error {
	_, err := pool.Exec(ctx, `WITH gone AS (DELETE FROM devices WHERE token = $1 RETURNING user_id, device_id)
		INSERT INTO signin_device_ended (user_id, device_id, ended_at)
		SELECT user_id, device_id, clock_timestamp() FROM gone WHERE device_id <> ''
		ON CONFLICT (user_id, device_id) DO UPDATE SET ended_at = excluded.ended_at`, token)
	return err
}
