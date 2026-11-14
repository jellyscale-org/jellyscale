//go:build integration

package transcode

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

// JS-104, failure 2 (TestFaultSchedulesHoldTheLeaseAndFailoverInvariants, seed
// 15803960744879474315 schedule 1, merge queue run 43590049982): a worker began a
// put of segments/<spec>/4.mp4 3.503 s after its job stopped being its own.
//
// What the run's log shows: the put began at 11:32:57.637, which is 90.6 s into
// the schedule, exactly when step 3 (stop-past-lease: SIGSTOP of the worker of a
// waited-on job, 67.8 s + 22.8 s) ended and the worker was continued. The delay
// step (step 4, 12.9 s round trips) began at 1m40s, nine seconds later, so it is
// not what let the put begin. The worker had asked "is the job still mine"
// (Worker.upload) while its lease stood and was stopped between that answer and
// the put; the check is not repeated, and nothing compares the answer with the
// time it is used at. A database that answers late does the same to the same code:
// the answer says "owned" because the lease stood when Postgres looked.
//
// The checks below put the worker in exactly that place, which a schedule can
// only wait for: the proxy of the worker's connection (testenv.DBProxy.OnStatement)
// shortens the job's lease just before the ownership check reaches Postgres, so
// that Postgres answers "yours" with a second of lease left, and then holds
// the answer, for as long as the case says, while the worker is stopped or while
// nothing happens (a database that is slow to answer). The proxy only decides when
// the answer arrives; the worker is a real process, the job a real ffmpeg, and
// the worker runs at its production lease (10 s) and heartbeat (3 s), whose half
// (5 s) is the check's own timeout: a hold of 3 s is inside it, one of 7 s is beyond it.
//
// A put is measured as the fault test measures it: the time a Put began against
// lease_expires_at as Postgres had it when the check ran, within the 350 ms the
// fault test allows (faults_integration_test.go: the check before a put allows 350ms),
// which grows neither with the hold nor with the length of the stop.
const putAllowance = 350 * time.Millisecond

// isTheOwnershipCheck matches the statement a worker asks before it stores a
// segment (Worker.upload): a read of the job's lease and of the worker's row, and
// not the heartbeat, which writes them. Counts the matches, and says true at the nth.
func isTheOwnershipCheck(nth int) func([]byte) bool {
	var mu sync.Mutex
	seen := 0
	check := sentHas("lease_expires_at", "transcode_nodes", "transcode_jobs", "SELECT")
	return func(sent []byte) bool {
		if !check(sent) || sentHas("UPDATE transcode_jobs")(sent) || sentHas("INSERT")(sent) || sentHas("DELETE")(sent) {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		seen++
		return seen == nth
	}
}

func TestAWorkerBeginsNoPutOnceTheLeaseRanOutWhateverMadeItLate(t *testing.T) {
	cases := []struct {
		name string
		nth  int // Which ownership check of the job (1 is the job's first segment, stored with its init segment).
		// leaseLeft is what the lease has left when Postgres answers the check; the
		// answer is then held for hold, with the worker stopped when stop is set.
		leaseLeft, hold time.Duration
		stop            bool
		// putDelay is how long a store takes after it has stored the object (JELLYSCALE_TEST_PUTDELAY_AFTER_MS):
		// the init segment's, so that the segment's put begins that much later.
		putDelay time.Duration
		givesUp  bool // The job is given up, cancelled, with nothing stored once the lease ran out.
	}{
		{"stopped after asking, for longer than the lease had left", 1, time.Second, 3 * time.Second, true, 0, true},
		{"stopped after asking, a later segment", 3, time.Second, 3 * time.Second, true, 0, true},
		{"database answers late, inside the check's timeout", 1, time.Second, 3 * time.Second, false, 0, true},
		{"database answers late, a later segment", 3, time.Second, 3 * time.Second, false, 0, true},
		{"database answers beyond the check's timeout", 1, time.Second, 7 * time.Second, false, 0, true},
		{"the init segment's store takes longer than the lease had left", 1, 1500 * time.Millisecond, 0, false, 2500 * time.Millisecond, true},
		// In time: nothing is given up.
		{"answered in 50 ms with the lease nearly whole", 1, 6 * time.Second, 50 * time.Millisecond, false, 0, false},
		{"stopped for less than the lease less a heartbeat", 1, 0, 4 * time.Second, true, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slowAt(t, "2")
			env := []string{}
			if c.putDelay > 0 {
				env = append(env, fmt.Sprintf("JELLYSCALE_TEST_PUTDELAY_AFTER_MS=%d", c.putDelay.Milliseconds()))
			}
			s := newStack(t, 1, env...)
			r := s.r
			w := s.workers[0]
			ctx := context.Background()

			var mu sync.Mutex
			var job int64
			var leaseEnds time.Time // As Postgres had it when it answered the check.
			before := func() {
				if c.leaseLeft == 0 {
					return
				}
				var id int64
				var ends time.Time
				if err := r.pool.QueryRow(ctx, `UPDATE transcode_jobs SET lease_expires_at = now() + make_interval(secs => $1)
					WHERE state = 'running' RETURNING id, lease_expires_at`, c.leaseLeft.Seconds()).Scan(&id, &ends); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				job, leaseEnds = id, ends
				mu.Unlock()
			}
			after := func() {
				if c.stop {
					w.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
					defer w.cmd.Process.Signal(syscall.SIGCONT)
				}
				time.Sleep(c.hold)
			}
			asked := w.db.OnStatement(isTheOwnershipCheck(c.nth), before, after)

			served := make(chan error, 1)
			go func() { _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); served <- err }()
			select {
			case <-asked:
			case <-time.After(90 * time.Second):
				t.Fatalf("the worker never asked whether job %d was still its own for the %dth time: the check this test waits for is not the one a worker sends", job, c.nth)
			}
			mu.Lock()
			old, ends := job, leaseEnds
			mu.Unlock()
			if c.leaseLeft == 0 {
				if err := r.pool.QueryRow(ctx, "SELECT id, lease_expires_at FROM transcode_jobs WHERE state = 'running'").Scan(&old, &ends); err != nil {
					t.Fatal(err)
				}
			}
			ffmpegBefore, _ := r.ffmpegOf(w)

			if !c.givesUp {
				// A worker that is in time stores as before: the segment is stored, the job is the one it
				// was, and its ffmpeg is the same one.
				if err := <-served; err != nil {
					t.Fatalf("the viewer was not served: %v", err)
				}
				waitFor(t, "the job to record a segment after the held answer", 60*time.Second, func() bool {
					return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", old) > c.nth-1
				})
				time.Sleep(time.Second)
				if got := r.jobState(t, old); got != "running" {
					t.Errorf("a worker that was in time gave its job up: %s", got)
				}
				if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
					t.Errorf("ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
				}
				return
			}

			// Given up: the job ends, cancelled, and bounded only by a hang. A build that stores late
			// goes on instead and the film ends first, so the wait is for any end.
			var cutoff time.Time
			var missing int
			waitFor(t, "the worker to give up the job whose lease ran out", 60*time.Second, func() bool {
				if r.jobState(t, old) != "running" {
					cutoff = time.Now()
					// Nothing the replacement makes is stored yet: its ffmpeg has not run.
					missing = r.firstMissing(r.hash)
					return true
				}
				return false
			})
			// No put began after the lease ran out, by the clock of the database.
			late := 0
			for _, p := range putsOf(t, s.putlog) {
				if p.at.After(ends.Add(putAllowance)) && !p.at.After(cutoff) {
					if late++; late > 3 {
						continue // The first ones say it.
					}
					t.Errorf("worker %s began a put of %s at %v, %v after job %d's lease ran out (%v); the check before a put allows %v",
						p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(ends).Round(time.Millisecond), old, ends.Format("15:04:05.000"), putAllowance)
				}
			}
			if late > 0 {
				t.FailNow()
			}
			if got := r.jobState(t, old); got != "cancelled" {
				t.Errorf("the job a worker gave up because its lease ran out ended %s, want cancelled (a failed job refuses its spec to every viewer for 30 s)", got)
			}
			// PlaybackInfo does not refuse the spec, and the viewer's next request starts a replacement at the first segment not stored.
			r.noFailed(t)
			if reason, err := s.jobs.Unavailable(ctx, r.hash, "", ""); err != nil || reason != "" {
				t.Errorf("PlaybackInfo would refuse the spec after the job was given up: %q %v", reason, err)
			}
			<-served // The first viewer's own wait ends one way or another.
			if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), missing); err != nil {
				t.Fatalf("the viewer's next request for segment %d: %v", missing, err)
			}
			var start int
			if err := r.pool.QueryRow(ctx, "SELECT start_segment FROM transcode_jobs WHERE id > $1 AND spec_hash = $2 ORDER BY id LIMIT 1", old, r.hash).Scan(&start); err != nil {
				t.Fatalf("no replacement job after job %d: %v", old, err)
			}
			if start != missing {
				t.Errorf("the replacement starts at segment %d, want %d, the first not stored", start, missing)
			}
		})
	}
}
