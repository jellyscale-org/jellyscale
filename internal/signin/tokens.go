package signin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Device is the client a device token belongs to.
type Device struct {
	ID, Name, App, Version string
}

// Token is a token just written and the login time written with it.
type Token struct {
	Value   string
	LoginAt time.Time
}

// ErrNoAccount says no token may be issued: the account is gone or disabled, or its password is no longer the one that
// was checked.
var ErrNoAccount = errors.New("no account to issue a token for")

// LockDevice holds the user and device pair until the transaction ends. A transaction takes it before it locks the
// account's row.
func LockDevice(ctx context.Context, tx pgx.Tx, userID, deviceID string) error {
	// Prefixed so it cannot meet one of the bounds' locks, which are named by bound and key.
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "token "+userID+" "+deviceID)
	return err
}

// Issue writes the one token for a user and device pair. Sign-ins from every replica and both sign-in paths meet on a
// Postgres advisory lock on the pair, which waits rather than retries; the old row is deleted and the new one inserted
// inside that lock, in the caller's transaction. The caller can therefore make anything depending on the new token
// (a Quick Connect code's token, an answer's login time) in the same act, and no other path makes a token.
//
// The account's row is written first, which holds it until the commit: a disable, delete or password change either
// waits and then ends this token, or has committed and this issues none. checkedHash is the stored password hash a
// password sign-in was compared with, so that a password changed meanwhile issues none either; "" asks for no password.
func Issue(ctx context.Context, tx pgx.Tx, userID, checkedHash string, device Device) (Token, error) {
	var issued Token
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return issued, err
	}
	issued.Value = hex.EncodeToString(raw[:])
	issued.LoginAt = time.Now().UTC()

	if err := LockDevice(ctx, tx, userID, device.ID); err != nil {
		return issued, err
	}
	tag, err := tx.Exec(ctx, `UPDATE users SET last_login_at = $2
		WHERE id = $1 AND NOT is_disabled AND ($3 = '' OR password_hash = $3)`, userID, issued.LoginAt, checkedHash)
	if err != nil {
		return issued, err
	}
	if tag.RowsAffected() == 0 {
		return issued, ErrNoAccount
	}
	if _, err := tx.Exec(ctx, `DELETE FROM devices WHERE user_id = $1 AND device_id = $2`, userID, device.ID); err != nil {
		return issued, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		issued.Value, userID, device.ID, device.Name, device.App, device.Version); err != nil {
		return issued, err
	}
	return issued, nil
}

// EndTokens ends every token of the user but keep ("" keeps none), and remembers their devices for the account, as a
// sign-out does. The caller's transaction holds the account's row, so that no token is issued beside it.
func EndTokens(ctx context.Context, tx pgx.Tx, userID, keep string) error {
	_, err := tx.Exec(ctx, `WITH gone AS (DELETE FROM devices WHERE user_id = $1 AND token <> $2 RETURNING user_id, device_id)
		INSERT INTO signin_device_ended (user_id, device_id, ended_at)
		SELECT DISTINCT user_id, device_id, now() FROM gone WHERE device_id <> ''
		ON CONFLICT (user_id, device_id) DO UPDATE SET ended_at = excluded.ended_at`, userID, keep)
	return err
}
