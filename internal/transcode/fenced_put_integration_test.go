//go:build integration

package transcode

import (
	"context"
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

// JS-105 restated these cases. The worker's lease is kept by one lease keeper on
// a connection of its own (the proxy w.keep), and the check before a put is a
// renewal the keeper sends for the asking job after the ask, which is the same
// statement as the heartbeat's. So the statement the cases wait for is the nth
// renewal on that connection, and what a case lives through is the answer to
// it: the renewal ran in Postgres at the moment the hook's step before it runs,
// so the job's lease ends 10 s after that, and the answer is then late
// because the worker was stopped or the connections to Postgres were held for
// longer than the lease. A lease cut by a reap or a cancel, which the answer
// of a renewal cannot show, is the checks of lease_keeper_fence_integration_test.go.

// isTheRenewalNth matches the nth renewal of a transcode job's lease (isTheRenewal,
// lease_keeper_integration_test.go): the keeper's heartbeat for every job it keeps,
// and its renewal before an upload's puts.
func isTheRenewalNth(nth int) func([]byte) bool {
	var mu sync.Mutex
	seen := 0
	return func(sent []byte) bool {
		if !isTheRenewal(sent) {
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
		nth  int // Which renewal on the keeper's connection.
		// The answer to it is held for hold: with the worker stopped when stop is set, with every
		// connection of the keeper held when late is set (the worker is running and cannot hear),
		// and else only delayed.
		hold       time.Duration
		stop, late bool
		givesUp    bool // The job is given up, cancelled, with nothing stored once the lease ran out.
	}{
		{"stopped after the answer, for longer than the lease", 2, 11 * time.Second, true, false, true},
		{"stopped after the answer, a later renewal", 5, 11 * time.Second, true, false, true},
		{"the answer comes after the lease ran out, the worker running", 2, 11 * time.Second, false, true, true},
		{"the answer comes after the lease ran out, a later renewal", 5, 11 * time.Second, false, true, true},
		// In time: nothing is given up.
		{"answered in 50 ms", 2, 50 * time.Millisecond, false, false, false},
		{"stopped for less than the lease less a heartbeat", 2, 4 * time.Second, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			slowAt(t, "2")
			s := newStack(t, 1)
			r := s.r
			w := s.workers[0]
			ctx := context.Background()

			var mu sync.Mutex
			var job int64
			var leaseEnds time.Time // As Postgres had it when it answered the renewal.
			// The renewal has run in Postgres by now; nothing renews the job's lease while the answer is held.
			after := func() {
				var id int64
				var ends time.Time
				if err := r.pool.QueryRow(ctx, "SELECT id, lease_expires_at FROM transcode_jobs WHERE state = 'running' ORDER BY id DESC LIMIT 1").Scan(&id, &ends); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				job, leaseEnds = id, ends
				mu.Unlock()
				if c.stop {
					w.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
					defer w.cmd.Process.Signal(syscall.SIGCONT)
				}
				if c.late {
					w.keep.Hold() // A renewal sent now, or a connection made now, is not answered either.
					defer w.keep.Release()
				}
				time.Sleep(c.hold)
			}
			asked := w.keep.OnStatement(isTheRenewalNth(c.nth), nil, after)

			served := make(chan error, 1)
			go func() { _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); served <- err }()
			select {
			case <-asked:
			case <-time.After(90 * time.Second):
				t.Fatalf("the worker never sent its %dth renewal on the keeper's connection: the statement this test waits for is not the one a worker sends", c.nth)
			}
			mu.Lock()
			old, ends := job, leaseEnds
			mu.Unlock()
			ffmpegBefore, _ := r.ffmpegOf(w)

			if !c.givesUp {
				// A worker that is in time stores as before: the segment is stored, the job is the one it
				// was, and its ffmpeg is the same one.
				if err := <-served; err != nil {
					t.Fatalf("the viewer was not served: %v", err)
				}
				waitFor(t, "the job to record a segment after the held answer", 60*time.Second, func() bool {
					return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", old) >= 1
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
			if reason, err := s.jobs.Unavailable(ctx, r.spec, "", ""); err != nil || reason != "" {
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
