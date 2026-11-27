// Package quickconnect keeps the codes a device shows on its screen until a signed-in account approves one and the
// device redeems it (JS-037). All of it lives in Postgres: any api replica can answer any step, and a replica killed
// between steps loses nothing. Only the secret reads a code or redeems it; the six digits are what a viewer types.
package quickconnect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/signin"
)

const (
	// CodeLife is both the time a pending code may be approved and the time an approved one may be redeemed,
	// counted from the approval (the capture's expiry rows).
	CodeLife = 10 * time.Minute
	// MaxLive is D-079's fixed cap; it is not configurable.
	MaxLive = 100
	// capLock is the advisory lock that makes counting the live codes and storing a new one one act.
	capLock = 370038
)

// codeLimit is all six-digit codes, leading zeros included.
var codeLimit = big.NewInt(1_000_000)

var (
	ErrUnknownCode     = errors.New("unknown Quick Connect code")
	ErrAlreadyApproved = errors.New("Quick Connect code already approved")
	ErrUnknownSecret   = errors.New("unknown Quick Connect secret")
	ErrNotApproved     = errors.New("Quick Connect code is not approved yet")
	ErrFull            = errors.New("no room for another Quick Connect code")
	ErrSignedOut       = errors.New("the approving token is signed out, or its account disabled")
)

// Device is how the asking device named itself at Initiate. A redemption uses these values, whatever header it brings.
// It is signin's device: the token a code makes is issued by the same path a password sign-in uses.
type Device = signin.Device

// Code is a code's stored state. The secret is the credential; Value is the six digits a viewer types.
type Code struct {
	Secret, Value string
	Device        Device
	Created       time.Time
	Approved      *time.Time
	Approver      string
	Token         string
}

// Account is the approving account as a redemption signs it in. It is read in the transaction that issues the token,
// so a sign-in or sign-out that ends the token after the commit cannot leave the answer without a user.
type Account struct {
	ID, Name      string
	IsAdmin       bool
	AllowDownload bool
	LastLoginAt   *time.Time
	Configuration []byte
}

var (
	codeWindow = strconv.Itoa(int(CodeLife.Seconds()))

	// expired returns the predicate for a code whose time is up. An approved code runs from its approval; a pending
	// one from its Initiate. A redeemed code is kept for the rest of that window so a second redemption finds its token.
	expired = `CASE WHEN approved_at IS NOT NULL THEN approved_at ELSE created_at END
		+ interval '` + codeWindow + ` seconds' <= clock_timestamp()`

	// live is a code that still counts toward the cap: neither redeemed nor past its window.
	live = `token IS NULL AND CASE WHEN approved_at IS NOT NULL THEN approved_at ELSE created_at END
		+ interval '` + codeWindow + ` seconds' > clock_timestamp()`

	// pending or approved, whether or not it was redeemed, and not past its window.
	readable = `CASE WHEN approved_at IS NOT NULL THEN approved_at ELSE created_at END
		+ interval '` + codeWindow + ` seconds' > clock_timestamp()`
)

// draw takes both values from crypto/rand. The code is drawn uniformly over all a million six-digit values, not from
// two bytes reduced modulo a million, which would never give a viewer most of them. A collision redraws both, as the
// forced-collision check requires.
func draw() (Code, error) {
	var secretBytes [32]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return Code{}, err
	}
	number, err := rand.Int(rand.Reader, codeLimit)
	if err != nil {
		return Code{}, err
	}
	return Code{Secret: hex.EncodeToString(secretBytes[:]),
		Value: fmt.Sprintf("%06d", number.Int64())}, nil
}

func scan(row interface{ Scan(...any) error }) (Code, error) {
	var code Code
	var approved *time.Time
	err := row.Scan(&code.Secret, &code.Value, &code.Device.ID, &code.Device.Name, &code.Device.App,
		&code.Device.Version, &code.Created, &approved, &code.Approver, &code.Token)
	code.Approved = approved
	return code, err
}

const columns = `secret, code, device_id, device_name, app_name, app_version, created_at, approved_at, coalesce(approved_by::text, ''), coalesce(token, '')`

// Initiate stores a fresh code, or refuses when the cluster already has MaxLive of them. Expired codes are removed on
// the way in, which is also the expiry cleanup: nothing grows with the codes that came and went.
func Initiate(ctx context.Context, pool *pgxpool.Pool, device Device) (Code, error) {
	for range 32 {
		code, err := draw()
		if err != nil {
			return Code{}, err
		}
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, capLock); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM quickconnect_codes WHERE `+expired); err != nil {
				return err
			}
			var liveCount int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM quickconnect_codes WHERE `+live).Scan(&liveCount); err != nil {
				return err
			}
			if liveCount >= MaxLive {
				return ErrFull
			}
			// One clock for the time Initiate sends and the time Connect reads back: the row is the answer's source.
			return tx.QueryRow(ctx, `INSERT INTO quickconnect_codes
				(secret, code, device_id, device_name, app_name, app_version) VALUES ($1, $2, $3, $4, $5, $6)
				RETURNING created_at`, code.Secret, code.Value, device.ID, device.Name, device.App, device.Version).
				Scan(&code.Created)
		})
		if err == nil {
			code.Device = device
			return code, nil
		}
		if errors.Is(err, ErrFull) {
			return Code{}, err
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // a drawn code or secret is still live: draw both again
			continue
		}
		return Code{}, err
	}
	return Code{}, errors.New("could not draw an unused Quick Connect code")
}

// Connect reads a code by its secret. It carries no token and never makes one.
func Connect(ctx context.Context, pool *pgxpool.Pool, secret string) (Code, error) {
	code, err := scan(pool.QueryRow(ctx, `SELECT `+columns+` FROM quickconnect_codes
		WHERE secret = $1 AND `+readable, secret))
	if errors.Is(err, pgx.ErrNoRows) {
		return Code{}, ErrUnknownSecret
	}
	return code, err
}

// Approve sets the code's approving account once. A second approval, from anyone, is a conflict: the first one wins
// and leaves the code as it is. The caller has already reserved its failure and gives it back on success.
//
// token is what the approver signed this request with. It was checked before this transaction, so it is checked again
// while the account's row is held: a change that ends the account's tokens holds that row before it removes the codes
// the account approved, so it either removes this approval or has ended the token this looks for.
func Approve(ctx context.Context, pool *pgxpool.Pool, value, userID, token string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		enabled, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 AND NOT is_disabled FOR SHARE`, userID)
		if err != nil {
			return err
		}
		// A statement of its own, so that it sees what was committed while the lock was waited for.
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE token = $1 AND user_id = $2)`, token, userID).Scan(&held); err != nil {
			return err
		}
		if enabled.RowsAffected() == 0 || !held {
			return ErrSignedOut
		}
		tag, err := tx.Exec(ctx, `UPDATE quickconnect_codes SET approved_at = clock_timestamp(), approved_by = $2
			WHERE code = $1 AND approved_at IS NULL AND `+readable, value, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM quickconnect_codes WHERE code = $1 AND `+readable+`)`, value).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrUnknownCode
			}
			return ErrAlreadyApproved
		}
		return nil
	})
}

// Redeem signs the asking device in. The code's row decides the one token: a second redemption is given the token the
// first one made, and devices is written only by the request that set the row's token. The token itself is issued by
// signin.Issue, the same path a password sign-in uses, so the two cannot give one device two tokens. A code whose token
// no longer names a device and an account, whatever ended it, is worth no more than an unknown secret: no replacement
// is made. An account deleted after it approved redeems nothing, as Jellyfin's own token does work but answers 401;
// jellyscale refuses it at the door. The account is read here, under the row lock, so the answer does not depend on
// the token still naming a device after the commit.
//
// Every transaction that locks both an account's row and one of its codes takes the account's first, so none can
// deadlock with another.
func Redeem(ctx context.Context, pool *pgxpool.Pool, secret string) (Code, Account, error) {
	var result Code
	var account Account
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		approver, err := lockApprover(ctx, tx, secret)
		if err != nil {
			return err
		}
		code, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM quickconnect_codes
			WHERE secret = $1 AND `+readable+` FOR UPDATE`, secret))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownSecret
		}
		if err != nil {
			return err
		}
		// One approved since lockApprover looked is answered as it was then: its account's row is not held.
		if code.Approved == nil || code.Approver != approver {
			return ErrNotApproved
		}
		if code.Token != "" {
			// Sign-out, a password sign-in, a newer code or a deleted account takes the device row away. The stored
			// token is then dead: answering with it would give the device a token nothing accepts, and making a new
			// one here would leave two tokens in devices. Both wrong answers are the unknown secret's 404.
			var held bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM devices d JOIN users u ON u.id = d.user_id
				WHERE d.token = $1 AND d.user_id = $2 AND d.device_id = $3)`,
				code.Token, code.Approver, code.Device.ID).Scan(&held); err != nil {
				return err
			}
			if !held {
				return ErrUnknownSecret
			}
		} else {
			issued, err := signin.Issue(ctx, tx, code.Approver, "", code.Device)
			if errors.Is(err, signin.ErrNoAccount) {
				return ErrUnknownSecret
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE quickconnect_codes SET token = $2 WHERE secret = $1 AND token IS NULL`, secret, issued.Value); err != nil {
				return err
			}
			code.Token = issued.Value
		}
		account, err = readAccount(ctx, tx, code.Approver)
		if err != nil {
			return err
		}
		result = code
		return nil
	})
	return result, account, err
}

// lockApprover holds what a redemption needs before it locks the code: the pair its token is for and the approving
// account's row. It returns the account, "" for a code that is not approved or not there.
func lockApprover(ctx context.Context, tx pgx.Tx, secret string) (string, error) {
	var approver, device string
	err := tx.QueryRow(ctx, `SELECT coalesce(approved_by::text, ''), device_id FROM quickconnect_codes
		WHERE secret = $1 AND `+readable, secret).Scan(&approver, &device)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && approver == "" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := signin.LockDevice(ctx, tx, approver, device); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, approver)
	return approver, err
}

// readAccount reads the account a redemption signs in. A row gone between the checks above and this read is the unknown
// secret's answer, never a server error.
func readAccount(ctx context.Context, tx pgx.Tx, userID string) (Account, error) {
	var account Account
	err := tx.QueryRow(ctx, `SELECT id::text, name, is_admin, allow_download, last_login_at, configuration
		FROM users WHERE id = $1`, userID).
		Scan(&account.ID, &account.Name, &account.IsAdmin, &account.AllowDownload, &account.LastLoginAt, &account.Configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrUnknownSecret
	}
	return account, err
}

// Forget removes the codes the account approved, so that none signs a device in later. keep is the token that stays
// signed in ("" keeps none). The caller's transaction already holds the account's row, so that no approval of the
// account's can be written beside it (see Approve).
func Forget(ctx context.Context, tx pgx.Tx, userID, keep string) error {
	_, err := tx.Exec(ctx, `DELETE FROM quickconnect_codes WHERE approved_by = $1 AND token IS DISTINCT FROM $2`, userID, keep)
	return err
}
