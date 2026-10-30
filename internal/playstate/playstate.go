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

func clampTicks(ticks int64) int64 { return min(max(ticks, 0), MaxTicks) }

// Started records that a user began playing an item. A report for an item that
// no longer exists is ignored, not an error: the client can't act on it.
func Started(ctx context.Context, pool *pgxpool.Pool, userID, itemID string) error {
	_, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, last_played_at)
		SELECT $1::uuid, i.id, now() FROM items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE SET last_played_at = now()`, userID, itemID)
	return err
}

// Progress stores a position unless the row was written within CoalesceWindow.
// The age check is in the statement, so it holds across replicas without any
// flush logic. It reports whether the row was written.
func Progress(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, ticks int64) (bool, error) {
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, position_ticks, last_played_at, progress_at)
		SELECT $1::uuid, i.id, $3, now(), now() FROM items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET position_ticks = EXCLUDED.position_ticks, last_played_at = now(), progress_at = now()
		WHERE user_item_data.progress_at IS NULL OR user_item_data.progress_at < now() - make_interval(secs => $4)`,
		userID, itemID, clampTicks(ticks), CoalesceWindow.Seconds())
	return tag.RowsAffected() > 0, err
}

// Stopped always writes, and shields the row from the progress that follows. Near the end (90% or more) the item counts as watched
// and the resume point clears. Within the first 5% there's nothing worth
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
			FROM items i WHERE i.id = $2::uuid)
		INSERT INTO user_item_data (user_id, item_id, position_ticks, played, play_count, last_played_at, progress_at)
		SELECT $1::uuid, id, CASE WHEN result = 'keep' THEN $3::bigint ELSE 0 END,
			result = 'finished', CASE WHEN result = 'finished' THEN 1 ELSE 0 END, now(), now()
		FROM outcome
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET position_ticks = EXCLUDED.position_ticks,
			played = user_item_data.played OR EXCLUDED.played,
			play_count = user_item_data.play_count + EXCLUDED.play_count,
			last_played_at = now(), progress_at = now()`, userID, itemID, clampTicks(ticks))
	return err
}

// SetPlayed marks an item watched or unwatched, clearing the resume point
// either way. A series or season carries its episodes with it, so a show never
// reads as watched while its episodes aren't. It reports whether the item exists.
func SetPlayed(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, played bool) (bool, error) {
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played, play_count, last_played_at)
		SELECT $1::uuid, i.id, $3, CASE WHEN $3 THEN 1 ELSE 0 END, CASE WHEN $3 THEN now() END
		FROM items i WHERE i.id = $2::uuid OR i.series_id = $2::uuid OR i.season_id = $2::uuid OR i.parent_id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE
		SET played = $3, position_ticks = 0,
			play_count = CASE WHEN $3 THEN GREATEST(user_item_data.play_count, 1) ELSE 0 END,
			last_played_at = CASE WHEN $3 THEN now() ELSE user_item_data.last_played_at END`,
		userID, itemID, played)
	return tag.RowsAffected() > 0, err
}

// SetFavorite flags or unflags an item. It reports whether the item exists.
func SetFavorite(ctx context.Context, pool *pgxpool.Pool, userID, itemID string, favorite bool) (bool, error) {
	tag, err := pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, favorite)
		SELECT $1::uuid, i.id, $3 FROM items i WHERE i.id = $2::uuid
		ON CONFLICT (user_id, item_id) DO UPDATE SET favorite = $3`,
		userID, itemID, favorite)
	return tag.RowsAffected() > 0, err
}
