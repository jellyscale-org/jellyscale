//go:build integration

package transcode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/stress"
	"jellyscale/internal/testenv"
)

// A short run of the control-plane stress driver, at real call rates, so the
// driver keeps working while the ladder (stress_test.go, build tag stress) is
// run rarely. runStress and stressConfig are the names the task implies: the
// driver lives in package transcode because Worker.beat and Worker.claim are
// unexported. It calls the real Jobs.Watch, Jobs.Segment, Jobs.Unavailable,
// Worker.beat, Worker.claim, Reap and Notify; none of them are copied SQL.
// They do not compile until it exists.

func TestStressDriverCallsEveryOperationAtItsRate(t *testing.T) {
	step, err := runStress(context.Background(), t, testenv.DatabaseURL(t), stressConfig{Viewers: 8, Workers: 2, Specs: 4, Duration: 6 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if step.Viewers != 8 || step.Workers != 2 {
		t.Errorf("step is for V=%d W=%d", step.Viewers, step.Workers)
	}
	for _, name := range []string{"Watch", "Segment", "beat", "Reap", "claim", "Unavailable", "Notify"} {
		op, found := step.Ops[name]
		if !found || op.Calls == 0 || op.Intended == 0 {
			t.Errorf("%s: %+v, found %v; the driver did not call it", name, op, found)
			continue
		}
		if op.Errors != 0 {
			t.Errorf("%s: %d errors on an idle database", name, op.Errors)
		}
		if op.P50 <= 0 || op.P99 < op.P50 {
			t.Errorf("%s: p50 %v, p99 %v", name, op.P50, op.P99)
		}
	}
	// 8 viewers, a request about every 4 seconds, for 6: about 12 Watch calls
	// (the driver may start a viewer anywhere in its period).
	if w := step.Ops["Watch"].Intended; w < 8 || w > 24 {
		t.Errorf("Watch intended %d calls for 8 viewers over 6 s at one per 4 s", w)
	}
	// Reap about 4 a second per worker: 2 workers, 6 s.
	if r := step.Ops["Reap"].Intended; r < 30 || r > 60 {
		t.Errorf("Reap intended %d calls for 2 workers over 6 s at 4 a second", r)
	}
	if step.Connections < 1 {
		t.Errorf("connections %d: the step did not count Postgres's", step.Connections)
	}
	if err := stress.RateShortfall(step); err != nil {
		t.Error(err)
	}
}

// The run fails loudly when it cannot keep the rate it claims. A table lock held
// for the whole step stops every Watch and beat, so almost none complete; the
// driver must give up on each call, not hang, and return stress.ErrRate.
func TestStressDriverFailsLoudlyWhenItCannotKeepTheRate(t *testing.T) {
	ctx := context.Background()
	holder, err := pgx.Connect(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	// The driver creates the schema and its rows as it starts; the lock is taken
	// once it has, through the hook it gives.
	cfg := stressConfig{Viewers: 8, Workers: 2, Specs: 4, Duration: 4 * time.Second,
		AfterSetup: func(ctx context.Context) {
			tx, err := holder.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(func() { tx.Rollback(context.Background()) }) //nolint:errcheck
			if _, err := tx.Exec(ctx, "LOCK TABLE transcode_viewers IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Error(err)
			}
		}}
	start := time.Now()
	_, err = runStress(ctx, t, testenv.DatabaseURL(t), cfg)
	if !errors.Is(err, stress.ErrRate) {
		t.Fatalf("a run whose calls were all blocked returned %v, want stress.ErrRate", err)
	}
	if took := time.Since(start); took > cfg.Duration+20*time.Second {
		t.Errorf("it took %v to say so; calls must be given up on, not waited for", took)
	}
}
