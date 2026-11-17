//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// JS-078: a worker process reads transcodeBudget from its config and its first
// sweep brings the store to it: the key reaches the worker. Three specs of
// 3000 bytes each, none watched (their viewers were last seen 1, 2 and 3 hours
// ago, inside the 24 hours of transcodeRetention), and a budget of 6000 bytes:
// the least recently requested one goes whole and the others stay.
func TestAWorkerProcessSweepsTheTranscodeStoreToTheBudgetOfItsConfig(t *testing.T) {
	c := newCluster(t, false, "60s")
	ctx := context.Background()
	user := uuid.NewString()
	if _, err := c.pool.Exec(ctx, "INSERT INTO users (id, name, password_hash) VALUES ($1, 'viewer', 'x')", user); err != nil {
		t.Fatal(err)
	}
	hashes := map[string]time.Duration{strings.Repeat("a", 32): 3 * time.Hour, strings.Repeat("b", 32): 2 * time.Hour, strings.Repeat("c", 32): time.Hour}
	for hash, seen := range hashes {
		for _, name := range []string{"init.mp4", "0.mp4", "1.mp4"} {
			path := filepath.Join(c.dir, "segments", "segments", hash, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, make([]byte, 1000), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.pool.Exec(ctx, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, last_seen_at)
			VALUES ($1, $2, $3, 'd', now() - make_interval(secs => $4))`, hash[:8], hash, user, seen.Seconds()); err != nil {
			t.Fatal(err)
		}
	}
	c.extra = `"transcodeBudget": "6000", `
	c.start(newVersion, "worker")
	oldest := filepath.Join(c.dir, "segments", "segments", strings.Repeat("a", 32))
	deadline := time.Now().Add(hang)
	for {
		if _, err := os.Stat(oldest); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker's sweep did not remove the least recently requested spec within %s of its start:\n%s", hang, c.logs())
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, hash := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		entries, err := os.ReadDir(filepath.Join(c.dir, "segments", "segments", hash))
		if err != nil || len(entries) != 3 {
			t.Errorf("spec %.1s has %d objects (%v), want all 3: %s", hash, len(entries), err, fmt.Sprint(c.logs()))
		}
	}
}
