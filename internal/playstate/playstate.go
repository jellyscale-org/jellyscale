// Package playstate records what viewers have watched. Every write is a single
// statement that holds no state in the process, so any replica can take any
// report and a lost or reordered one costs at most 30 seconds of position.
package playstate

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CoalesceWindow is how long a progress write shields the row from the next
// one. Clients report every 10 seconds; writing each would rewrite the same row
// for no gain, since a resume point up to 30 seconds stale is invisible.
const CoalesceWindow = 30 * time.Second

// MaxTicks bounds a client-reported position: 2^50 ticks is over three years,
// longer than any real item. Clamping first keeps every product in SQL far
// from overflowing a bigint.
const MaxTicks = int64(1) << 50

// ClampTicks bounds a client-reported position to [0, MaxTicks].
func ClampTicks(ticks int64) int64 { return min(max(ticks, 0), MaxTicks) }

// Started records that a user began playing an item. A report for an item that
// no longer exists is ignored, not an error: the client can't act on it.
func Started(ctx context.Context, pool *pgxpool.Pool, userID, itemID string) error {
	_, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, last_played_at, data_at)
		SELECT $1::uuid, i.id, now(), now() FROM shown_items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE SET last_played_at = now(), data_at = now()`, userID, itemID)
	return err
}

// Progress stores a position unless the row was written within CoalesceWindow.
// The age check is in the statement, so it holds across replicas without any
// flush logic.
func Progress(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, ticks int64) error {
	_, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, position_ticks, last_played_at, data_at, progress_at)
		SELECT $1::uuid, i.id, $3, now(), now(), now() FROM shown_items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET position_ticks = EXCLUDED.position_ticks, last_played_at = now(), data_at = now(), progress_at = now()
		WHERE user_item_data.progress_at IS NULL OR user_item_data.progress_at < now() - make_interval(secs => $4)`,
		userID, itemID, ClampTicks(ticks), CoalesceWindow.Seconds())
	return err
}

// Stopped always writes, and shields the row from the progress that follows.
// Near the end (90% or more) the item counts as watched and the resume point
// clears. Within the first 5% there's nothing worth
// resuming, so the point clears too. Otherwise the position is kept. An item
// with an unknown runtime keeps whatever position was reported.
func Stopped(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, ticks int64) error {
	_, err := pool.Exec(ctx, `WITH outcome AS (
			SELECT i.id,
				CASE
					WHEN COALESCE(i.run_time_ticks, 0) <= 0 THEN 'keep'
					WHEN $3::bigint::numeric * 10 >= i.run_time_ticks::numeric * 9 THEN 'finished'
					WHEN $3::bigint::numeric * 20 < i.run_time_ticks::numeric THEN 'restart'
					ELSE 'keep'
				END AS result
			FROM shown_items i WHERE i.id = $2::uuid)
		INSERT INTO user_item_data (user_id, item_id, position_ticks, played, play_count, last_played_at, data_at, progress_at)
		SELECT $1::uuid, id, CASE WHEN result = 'keep' THEN $3::bigint ELSE 0 END,
			result = 'finished', CASE WHEN result = 'finished' THEN 1 ELSE 0 END, now(), now(), now()
		FROM outcome
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET position_ticks = EXCLUDED.position_ticks,
			played = user_item_data.played OR EXCLUDED.played,
			play_count = user_item_data.play_count + EXCLUDED.play_count,
			last_played_at = now(), data_at = now(), progress_at = now()`, userID, itemID, ClampTicks(ticks))
	return err
}

// SetPlayed marks an item watched or unwatched, clearing the resume point
// either way. A series or season carries its episodes with it, so a show never
// reads as watched while its episodes aren't. It reports whether the item exists.
func SetPlayed(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, played bool) (bool, error) {
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played, play_count, last_played_at, data_at)
		SELECT $1::uuid, i.id, $3, CASE WHEN $3 THEN 1 ELSE 0 END, CASE WHEN $3 THEN now() END, now()
		FROM shown_items i WHERE i.id = $2::uuid OR i.series_id = $2::uuid OR i.season_id = $2::uuid OR i.parent_id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET played = $3, position_ticks = 0,
			play_count = CASE WHEN $3 THEN GREATEST(user_item_data.play_count, 1) ELSE 0 END,
			last_played_at = CASE WHEN $3 THEN now() ELSE user_item_data.last_played_at END, data_at = now()`,
		userID, itemID, played)
	return tag.RowsAffected() > 0, err
}

// SetFavorite flags or unflags an item. It reports whether the item exists.
func SetFavorite(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, favorite bool) (bool, error) {
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, favorite)
		SELECT $1::uuid, i.id, $3 FROM shown_items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE SET favorite = $3`,
		userID, itemID, favorite)
	return tag.RowsAffected() > 0, err
}

// UserDataUpdate is a partial write; a nil field is left as it is.
type UserDataUpdate struct {
	Played, Favorite *bool
	Position         *int64
	LastPlayed       *time.Time
}

// SetUserData applies an update for a client that syncs state it kept itself,
// such as an offline player. LastPlayed is when that state was true, held to
// the database's now so a fast client clock cannot outrank every later write.
// If the row's data_at (when its played state and position were last true) is
// later, the update is older than what the server knows (the viewer watched or
// changed more since), so its Played, Position and LastPlayed are dropped and
// only Favorite, which has no time, is kept. An update with no LastPlayed is a
// deliberate change made now: it always applies, and when it sets Played or
// Position it moves data_at to now, so a later older-dated sync cannot undo it.
// A favourite-only update moves neither data_at nor last_played_at, the latter
// being what clients show as LastPlayedDate. The rule is in the statement so it
// holds across replicas. Unlike SetPlayed it neither clears the position nor
// counts a play. It reports whether the item exists.
func SetUserData(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, u UserDataUpdate) (bool, error) {
	var position *int64
	if u.Position != nil {
		clamped := ClampTicks(*u.Position)
		position = &clamped
	}
	// LEAST ignores NULL, so an undated update needs the CASE to stay NULL.
	const dated = `(CASE WHEN $6::timestamptz IS NULL THEN NULL ELSE LEAST($6::timestamptz, now()) END)`
	const fresh = `(` + dated + ` IS NULL OR user_item_data.data_at IS NULL OR user_item_data.data_at <= ` + dated + `)`
	const dataAt = `COALESCE(` + dated + `, CASE WHEN $3::boolean IS NOT NULL OR $5::bigint IS NOT NULL THEN now() END)`
	const lastPlayed = `COALESCE(` + dated + `, CASE WHEN $3::boolean IS TRUE OR $5::bigint IS NOT NULL THEN now() END)`
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played, favorite, position_ticks, last_played_at, data_at)
		SELECT $1::uuid, i.id, COALESCE($3::boolean, false), COALESCE($4::boolean, false), COALESCE($5::bigint, 0), `+lastPlayed+`, `+dataAt+`
		FROM shown_items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET favorite = COALESCE($4::boolean, user_item_data.favorite),
			played = CASE WHEN `+fresh+` THEN COALESCE($3::boolean, user_item_data.played) ELSE user_item_data.played END,
			position_ticks = CASE WHEN `+fresh+` THEN COALESCE($5::bigint, user_item_data.position_ticks) ELSE user_item_data.position_ticks END,
			last_played_at = CASE WHEN `+fresh+` THEN COALESCE(`+lastPlayed+`, user_item_data.last_played_at) ELSE user_item_data.last_played_at END,
			data_at = CASE WHEN `+fresh+` THEN COALESCE(`+dataAt+`, user_item_data.data_at) ELSE user_item_data.data_at END`,
		userID, itemID, u.Played, u.Favorite, position, u.LastPlayed)
	return tag.RowsAffected() > 0, err
}
