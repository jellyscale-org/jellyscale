package signin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Issue writes the one token for a user and device pair. Sign-ins from every replica and both sign-in paths meet on a
// Postgres advisory lock on the pair, which waits rather than retries; the old row is deleted and the new one inserted
// inside that lock, in the caller's transaction. The caller can therefore make anything depending on the new token
// (a Quick Connect code's token, an answer's login time) in the same act, and no other path makes a token.
func Issue(ctx context.Context, tx pgx.Tx, userID string, device Device) (Token, error) {
	var issued Token
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return issued, err
	}
	issued.Value = hex.EncodeToString(raw[:])
	issued.LoginAt = time.Now().UTC()

	// Prefixed so it cannot meet one of the bounds' locks, which are named by bound and key.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"token "+userID+" "+device.ID); err != nil {
		return issued, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM devices WHERE user_id = $1 AND device_id = $2`, userID, device.ID); err != nil {
		return issued, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		issued.Value, userID, device.ID, device.Name, device.App, device.Version); err != nil {
		return issued, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, userID, issued.LoginAt); err != nil {
		return issued, err
	}
	return issued, nil
}
