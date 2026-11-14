//go:build integration

package main

import (
	"context"
	"testing"
)

// JS-087: what a worker registers is the cores it declared, and a worker that
// declares none, or that still has transcodeSlots in its config, registers
// enough to take a re-encode (a 1080p SDR one costs 0.8). Real processes, as an
// operator runs them.
func TestAWorkersRegisteredCoresFollowItsConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots int
		extra string
		check func(float64) bool
		want  string
	}{
		{"no cores declared", 0, "", func(c float64) bool { return c >= 0.8 }, "at least 0.8"},
		{"transcodeSlots still set", 3, "", func(c float64) bool { return c >= 0.8 }, "at least 0.8"},
		{"six cores", 0, `"transcodeCores": 6,`, func(c float64) bool { return c == 6 }, "6"},
		{"cores win over slots", 5, `"transcodeCores": 2.5,`, func(c float64) bool { return c == 2.5 }, "2.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, false, "60s")
			c.slots, c.extra = tc.slots, tc.extra
			w := c.start(newVersion, "worker")
			var cores float64
			var slots int
			if err := c.pool.QueryRow(context.Background(), "SELECT cores::float8, slots FROM transcode_nodes WHERE id = $1", w.workerID).Scan(&cores, &slots); err != nil {
				t.Fatal(err)
			}
			// Replicas of the release before count a worker only by slots > 0 and
			// the runbook starts new workers first, so the row keeps a positive
			// slots for one release beside its cores.
			if slots <= 0 {
				t.Errorf("the worker registered %d slots beside its cores; a replica of the release before would take it for no worker\n%s", slots, c.logs())
			}
			if !tc.check(cores) {
				t.Errorf("the worker registered %v cores, want %s\n%s", cores, tc.want, c.logs())
			}
		})
	}
}
