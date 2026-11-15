//go:build integration

package main

import (
	"context"
	"testing"
	"time"
)

// JS-111: the transcode reasons PlaybackInfo keeps for the session list are
// pruned by the replicas themselves, with no statement of PlaybackInfo to do it
// (internal/api, TestPlaybackInfoWritesNothingWhenNothingIsQueued). With only an
// api replica running, a row older than a day goes, and one younger stays.
//
// Timing: the task names no cadence, so this waits as long as a hang bound
// (a minute, which is ten times a heartbeat of production) for the old row to
// go; a build that prunes on its first heartbeat passes at once, and one that
// prunes hourly fails. A shorter timing is not used, so nothing is hidden.
func TestOnlyAPIReplicasPruneOldTranscodeReasons(t *testing.T) {
	c := newCluster(t, false, "20s")
	ctx := context.Background()
	if _, err := c.pool.Exec(ctx, "TRUNCATE transcode_reasons"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(ctx, `INSERT INTO transcode_reasons (play_session_id, spec_hash, reasons, created_at) VALUES
		('old-session', 'h1', '{VideoCodecNotSupported}', now() - interval '25 hours'),
		('young-session', 'h2', '{AudioCodecNotSupported}', now() - interval '23 hours')`); err != nil {
		t.Fatal(err)
	}
	c.start(newVersion, "api")
	remaining := func() (n int, young bool) {
		rows, err := c.pool.Query(ctx, "SELECT play_session_id FROM transcode_reasons")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			n++
			young = young || id == "young-session"
		}
		return n, young
	}
	deadline := time.Now().Add(hang)
	for {
		if n, _ := remaining(); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the old reasons row was not pruned by an api replica within %s:\n%s", hang, c.logs())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, young := remaining(); !young {
		t.Errorf("the row of 23 hours was pruned as well")
	}
}
