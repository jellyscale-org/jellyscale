//go:build stress

package transcode

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/stress"
)

// TestControlPlaneLadder is the stress run itself: V = 50 to 1600 viewers with
// W = V/4 workers, a fixed time per step, against the disposable Postgres that
// test/stress.sh starts (Docker-assigned port, container name in
// JELLYSCALE_STRESS_CONTAINER for docker stats). It is behind the stress tag, so
// no CI tier lists or runs it. It writes the report (stress.Format) to the file
// named by JELLYSCALE_STRESS_REPORT and fails if a step could not keep its rate.
// The pool-size comparison is the same ladder at several JELLYSCALE_STRESS_POOL.
func TestControlPlaneLadder(t *testing.T) {
	dsn, container, out := os.Getenv("JELLYSCALE_TEST_DATABASE_URL"), os.Getenv("JELLYSCALE_STRESS_CONTAINER"), os.Getenv("JELLYSCALE_STRESS_REPORT")
	if dsn == "" || container == "" || out == "" {
		t.Fatal("run this through test/stress.sh: JELLYSCALE_TEST_DATABASE_URL, JELLYSCALE_STRESS_CONTAINER and JELLYSCALE_STRESS_REPORT are not all set")
	}
	// JELLYSCALE_STRESS_STEPS replaces the ladder, as for the rows past 1600 in
	// ARCHITECTURE.md.
	ladder := []int{50, 100, 200, 400, 800, 1600}
	if raw := os.Getenv("JELLYSCALE_STRESS_STEPS"); raw != "" {
		ladder = nil
		for _, field := range strings.Split(raw, ",") {
			v, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil || v < 4 {
				t.Fatalf("JELLYSCALE_STRESS_STEPS: %q is not a number of viewers of 4 or more", field)
			}
			ladder = append(ladder, v)
		}
	}
	var steps []stress.Step
	for _, v := range ladder {
		step, err := runStress(context.Background(), t, dsn, stressConfig{Viewers: v, Workers: v / 4, Specs: v / 4, Duration: 60 * time.Second, Container: container})
		if err != nil {
			t.Fatalf("V=%d: %v", v, err)
		}
		steps = append(steps, step)
	}
	if err := os.WriteFile(out, []byte(stress.Format(steps)), 0o644); err != nil {
		t.Fatal(err)
	}
}
