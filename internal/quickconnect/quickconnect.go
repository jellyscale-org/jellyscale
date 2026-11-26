// Package quickconnect keeps the codes a device shows on its screen until a signed-in account approves one and the
// device redeems it (JS-037). All of it lives in Postgres: any api replica can answer any step, and a replica killed
// between steps loses nothing. Only the secret reads a code or redeems it; the six digits are what a viewer types.
package quickconnect

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

var (
	ErrUnknownCode     = errors.New("unknown Quick Connect code")
	ErrAlreadyApproved = errors.New("Quick Connect code already approved")
	ErrUnknownSecret   = errors.New("unknown Quick Connect secret")
	ErrNotApproved     = errors.New("Quick Connect code is not approved yet")
	ErrFull            = errors.New("no room for another Quick Connect code")
	ErrAccountGone     = errors.New("the approving account is gone")
)

// Device is how the asking device named itself at Initiate. A redemption uses these values, whatever header it brings.
type Device struct {
	ID, Name, App, Version string
}

// Code is a code's stored state. The secret is the credential; Value is the six digits a viewer types.
type Code struct {
	Secret, Value string
	Device        Device
	Created       time.Time
	Approved      *time.Time
	Approver      string
	Token         string
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

// draw takes both values from crypto/rand. A collision redraws both, as the forced-collision check requires.
func draw() (Code, error) {
	var secretBytes [32]byte
	var number [2]byte
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return Code{}, err
	}
	if _, err := rand.Read(number[:]); err != nil {
		return Code{}, err
	}
	return Code{Secret: hex.EncodeToString(secretBytes[:]),
		Value: fmt.Sprintf("%06d", int(binary.BigEndian.Uint16(number[:]))%1000000)}, nil
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
			_, err := tx.Exec(ctx, `INSERT INTO quickconnect_codes
				(secret, code, device_id, device_name, app_name, app_version) VALUES ($1, $2, $3, $4, $5, $6)`,
				code.Secret, code.Value, device.ID, device.Name, device.App, device.Version)
			return err
		})
		if err == nil {
			code.Device = device
			code.Created = time.Now().UTC()
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
func Approve(ctx context.Context, pool *pgxpool.Pool, value, userID string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
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
// first one made, and devices is written only by the request that set the row's token. An account deleted after it
// approved redeems nothing, as Jellyfin's own token does work but answers 401; jellyscale refuses it at the door.
func Redeem(ctx context.Context, pool *pgxpool.Pool, secret string) (Code, error) {
	var result Code
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		code, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM quickconnect_codes
			WHERE secret = $1 AND `+readable+` FOR UPDATE`, secret))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownSecret
		}
		if err != nil {
			return err
		}
		if code.Approved == nil {
			return ErrNotApproved
		}
		if code.Token != "" {
			result = code
			return nil
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, code.Approver).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrUnknownSecret
		}
		token := randomToken()
		if _, err := tx.Exec(ctx, `DELETE FROM devices WHERE user_id = $1 AND device_id = $2`, code.Approver, code.Device.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version)
			VALUES ($1, $2, $3, $4, $5, $6)`, token, code.Approver, code.Device.ID, code.Device.Name, code.Device.App, code.Device.Version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quickconnect_codes SET token = $2 WHERE secret = $1 AND token IS NULL`, secret, token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = clock_timestamp() WHERE id = $1`, code.Approver); err != nil {
			return err
		}
		code.Token = token
		result = code
		return nil
	})
	return result, err
}

// randomToken is a device token, the same shape as a password sign-in's.
func randomToken() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
