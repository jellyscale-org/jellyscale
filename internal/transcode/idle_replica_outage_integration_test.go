//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// A replica whose pool sat idle through a switchover meets the returned
// Postgres on a fresh connection and sees no failure, so it cannot know there
// was an outage. Its PlaybackInfo must not forget a live worker that has not
// yet taken its lock again: the worker would find its row gone and stop every
// ffmpeg (JS-029, found by the architecture and adversarial reviews).
func TestAReplicaThatMissedTheOutageForgetsNoLiveWorker(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	proxy := testenv.NewDBProxy(t, r.dsn)
	pool, err := db.Open(ctx, proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	j := &Jobs{Pool: pool, Segments: r.segments, QueueTimeout: 5 * time.Second, WaitTimeout: 60 * time.Second}

	// A worker whose lock connection ended with the old primary and that has not
	// locked again yet: its row says locked, and Postgres shows no lock.
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ('retaking', 'worker', '', 2, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Unavailable(ctx, playback.Spec{}, "u", "d"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // Past pgxpool's idle ping: the next acquire replaces the connection quietly.
	proxy.Sever()
	if _, err := j.Unavailable(ctx, playback.Spec{}, "u", "d"); err != nil {
		t.Fatal(err)
	}
	if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = 'retaking'") != 1 {
		t.Fatal("a replica that saw no outage forgot a worker that was taking its lock again")
	}
}
