//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-107: how a job ends comes from where the error came from. The table of that
// mapping is in job_end_test.go; these checks run it against real workers, a real
// Postgres and stores that refuse.
//
// They use one name the task implies and the build must add: the field SegmentsURI of
// Worker and of Jobs, the URI of the transcode store each was given (main sets it from
// transcodeStore). The error a store's failure leaves in transcode_jobs.error holds that
// URI, and for the media store the URI of the source's own store (source.StoreURI).

// refusingStore is a store that refuses what it is told to, as one does whose server
// is down or whose address is wrong: with the error of a connection that was refused.
type refusingStore struct {
	store.Store
	puts  atomic.Bool
	opens atomic.Pointer[string] // The key whose Open is refused.
}

func connectionRefused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, Err: syscall.ECONNREFUSED}
}

func (s *refusingStore) Put(ctx context.Context, key string, body io.Reader) error {
	if s.puts.Load() {
		return connectionRefused()
	}
	return s.Store.Put(ctx, key, body)
}

func (s *refusingStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if refused := s.opens.Load(); refused != nil && *refused == key {
		return nil, connectionRefused()
	}
	return s.Store.Open(ctx, key, offset)
}

// proxied gives the worker a pool of its own through a proxy, and leaves its liveness
// lock and lease keeper connections direct, so that what the test does to the proxy
// is done to the worker's statements and nothing else.
func (r *rig) proxied(t *testing.T) (*testenv.DBProxy, func(*Worker)) {
	t.Helper()
	proxy := testenv.NewDBProxy(t, r.dsn)
	pool, err := db.Open(context.Background(), proxy.DSN()+"&default_query_exec_mode=exec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	direct, err := pgx.ParseConfig(r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	return proxy, func(w *Worker) {
		w.Pool, w.Items, w.LivenessConfig, w.KeeperConfig = pool, &items.Repo{Pool: pool, ServerID: "srv"}, direct, direct
	}
}

// states are the ends of the jobs of a spec, in order.
func (r *rig) ends(t *testing.T, hash string) (states, reasons []string) {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT state, COALESCE(error, '') FROM transcode_jobs WHERE spec_hash = $1 ORDER BY id", hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var state, reason string
		if err := rows.Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		states, reasons = append(states, state), append(reasons, reason)
	}
	return states, reasons
}

// neverAway fails the test if a process took Postgres for away: it answered the whole time.
func neverAway(t *testing.T, who string, log *logBuffer, pools ...interface{ Total() time.Duration }) {
	t.Helper()
	if strings.Contains(strings.ToLower(log.String()), "postgres is away") {
		t.Errorf("%s logged that Postgres is away, which answered the whole time:\n%s", who, log.String())
	}
	for _, p := range pools {
		if total := p.Total(); total != 0 {
			t.Errorf("%s took Postgres for away for %v", who, total)
		}
	}
}

// A job whose spec the worker cannot read ends failed, by the worker that claimed it, with
// why. It starts no ffmpeg and stores nothing: its hash is no spec's.
func TestAClaimedJobWhoseSpecDoesNotDecodeEndsFailedAndStartsNothing(t *testing.T) {
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(x *Worker) { w = x; x.Log = new(logBuffer).logger() })
	for name, raw := range badSpecs(t, r) {
		hash := "bad-" + name
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version, cost)
			VALUES ($1, $2, $3, 0, 0, $4, 0.05)`, hash, raw, "owner-"+name, playback.OutputVersion); err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			waitFor(t, "the claimed job to end", 15*time.Second, func() bool {
				states, _ := r.ends(t, hash)
				return len(states) == 1 && states[0] != "queued" && states[0] != "running"
			})
			var state, reason, worker string
			var lapsed bool
			if err := r.pool.QueryRow(context.Background(), `SELECT state, COALESCE(error, ''), COALESCE(worker, ''), COALESCE(lease_expires_at < ended_at, false) FROM transcode_jobs WHERE spec_hash = $1`, hash).
				Scan(&state, &reason, &worker, &lapsed); err != nil {
				t.Fatal(err)
			}
			if state != "failed" {
				t.Errorf("ended %s (%q), want failed", state, reason)
			}
			if !strings.Contains(strings.ToLower(reason), "decode") {
				t.Errorf("the error %q does not say the spec could not be decoded", reason)
			}
			if worker != w.ID {
				t.Errorf("the job's worker is %q, want the claiming worker %s", worker, w.ID)
			}
			if lapsed {
				t.Error("the job ended after its lease had lapsed: the reap or a sweep wrote it, not the worker that claimed it")
			}
			if objects, _ := r.segments.List(context.Background(), "segments/"+hash+"/"); len(objects) != 0 {
				t.Errorf("%d objects were stored under the job's hash", len(objects))
			}
		})
	}
	// No job made a scratch directory or an ffmpeg: both come after the spec is read.
	if entries, _ := os.ReadDir(r.scratch); len(entries) != 0 {
		t.Errorf("the scratch directory holds %d entries after jobs that never started", len(entries))
	}
	if running := r.ffmpegProcesses(); len(running) != 0 {
		t.Errorf("ffmpeg runs for jobs whose spec could not be read: %v", running)
	}
}

// badSpecs are specs of one shape or another that json.Unmarshal into a Spec refuses: a
// whole value of the wrong kind, and a single field of the wrong type where the rest decodes.
func badSpecs(t *testing.T, r *rig) map[string][]byte {
	t.Helper()
	good, err := json.Marshal(r.spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(good, &fields); err != nil {
		t.Fatal(err)
	}
	var numbers []string
	for key, value := range fields {
		if _, ok := value.(float64); ok {
			numbers = append(numbers, key)
		}
	}
	sort.Strings(numbers)
	if len(numbers) == 0 {
		t.Fatalf("the spec has no number to give the wrong type: %s", good)
	}
	fields[numbers[0]] = "not a number"
	oneField, _ := json.Marshal(fields)
	return map[string][]byte{"a-string": []byte(`"just a string"`), "an-array": []byte(`[1, 2]`), "one-wrong-field": oneField}
}

// A job whose spec cannot be read does not stop the claim loop: a good job queued
// after it, which a viewer waits for, is claimed and run.
func TestAGoodJobQueuedBesideOneWhoseSpecDoesNotDecodeIsStillRun(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	j.QueueTimeout = 30 * time.Second
	for name, raw := range badSpecs(t, r) {
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version, cost)
			VALUES ($1, $2, $3, 0, 0, $4, 0.05)`, "bad-"+name, raw, "owner-"+name, playback.OutputVersion); err != nil {
			t.Fatal(err)
		}
	}
	// The viewer's job is queued after the bad ones, before any worker is there to take any.
	type result struct {
		body []byte
		err  error
	}
	served := make(chan result, 1)
	go func() {
		body, err := r.fetch(t, j, psid(0), 0)
		served <- result{body, err}
	}()
	waitFor(t, "the viewer's job to be queued", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash) == 1
	})
	r.worker(t, func(w *Worker) { w.QueueTimeout = 30 * time.Second })
	got := <-served
	if got.err != nil || !isMP4Segment(got.body) {
		t.Fatalf("the good job was not run: %d bytes, %v", len(got.body), got.err)
	}
	for name := range badSpecs(t, r) {
		states, reasons := r.ends(t, "bad-"+name)
		if len(states) != 1 || states[0] != "failed" || !strings.Contains(strings.ToLower(reasons[0]), "decode") {
			t.Errorf("the job with %s ended %v %q, want failed saying the spec could not be decoded", name, states, reasons)
		}
	}
}

// The transcode store refuses the connection of a put, Postgres answering: the job ends failed, never
// cancelled, with the store's URI. One job of the spec is run while the failure is remembered, and no
// ffmpeg of it is left. No process takes Postgres for away.
func TestATranscodeStoreThatRefusesAPutFailsTheJobAndNamesTheStore(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, store.NewPosix(dir))
	uri := "file://" + dir
	refusing := &refusingStore{Store: r.segments}
	refusing.puts.Store(true)
	var workerLog, replicaLog logBuffer
	var w *Worker
	r.worker(t, func(x *Worker) { w, x.Segments, x.SegmentsURI, x.Log = x, refusing, uri, workerLog.logger() })
	j := r.jobs(t)
	j.SegmentsURI, j.Log = uri, replicaLog.logger()

	var failed FailedError
	if _, err := r.fetch(t, j, psid(0), 0); !errors.As(err, &failed) || !strings.Contains(failed.Reason, uri) {
		t.Fatalf("the request waiting on the job got %v, want its failure naming %s", err, uri)
	}
	states, reasons := r.ends(t, r.hash)
	if len(states) != 1 || states[0] != "failed" || !strings.Contains(reasons[0], uri) {
		t.Fatalf("the jobs of the spec ended %v %q, want one job, failed, with %s in transcode_jobs.error", states, reasons, uri)
	}
	// Within the 30 seconds the failure is remembered: a request starts no job, and PlaybackInfo refuses.
	if _, err := r.fetch(t, j, psid(1), 3); !errors.As(err, &failed) || !strings.Contains(failed.Reason, uri) {
		t.Errorf("a request within the failure memory got %v, want the failure naming %s", err, uri)
	}
	if states, _ := r.ends(t, r.hash); len(states) != 1 {
		t.Errorf("%d jobs of the spec ran while the failure was remembered, want 1", len(states))
	}
	if reason, err := j.Unavailable(context.Background(), r.spec, "", ""); err != nil || !strings.Contains(reason, uri) {
		t.Errorf("PlaybackInfo's refusal is %q %v, want the last attempt's error with %s", reason, err, uri)
	}
	waitFor(t, "no ffmpeg to be left", 10*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	neverAway(t, "the worker", &workerLog, db.Of(w.Pool))
	neverAway(t, "the stream replica", &replicaLog, db.Of(j.Pool))
}

// A film with a subtitle file beside it that is an image (a .sup), burnt in, is a job that reads the
// media store (the file) and writes the transcode store (the segments): two stores whose URIs differ, so
// that an error names the one that failed.
func TestTheErrorOfAJobNamesTheStoreThatFailedNotTheOtherOne(t *testing.T) {
	good := testenv.PGS(1920, 1080, testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120})
	for _, tc := range []struct {
		name       string
		breakStore func(media, segments *refusingStore)
		failed     func(mediaURI, segmentsURI string) (named, notNamed string)
	}{
		{"the media store refuses the read of the image subtitle file",
			func(media, _ *refusingStore) { file := "damaged.sup"; media.opens.Store(&file) },
			func(media, segments string) (string, string) { return media, segments }},
		{"the transcode store refuses the put of the first segment",
			func(_, segments *refusingStore) { segments.puts.Store(true) },
			func(media, segments string) (string, string) { return segments, media }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := newRig(t, store.NewPosix(dir))
			r.useBesideImage(t, supEntry, map[string][]byte{"damaged.sup": good})
			segmentsURI, mediaURI := "file://"+dir, "file://"+film(t)
			var segments, media *refusingStore = &refusingStore{Store: r.segments}, nil
			var workerLog logBuffer
			var w *Worker
			r.worker(t, func(x *Worker) {
				var cache store.Cache
				w, x.Segments, x.SegmentsURI, x.Log = x, segments, segmentsURI, workerLog.logger()
				x.Media = func(uri string) (store.Store, error) {
					opened, err := cache.Open(uri)
					if err != nil || uri != mediaURI {
						return opened, err
					}
					media = &refusingStore{Store: opened}
					tc.breakStore(media, segments)
					return media, nil
				}
			})
			j := r.jobs(t)
			j.SegmentsURI = segmentsURI
			err, _ := r.burn(t, j, 0)
			named, notNamed := tc.failed(mediaURI, segmentsURI)
			var failed FailedError
			if !errors.As(err, &failed) || !strings.Contains(failed.Reason, named) || strings.Contains(failed.Reason, notNamed) {
				t.Fatalf("the request got %v, want the failure naming %s and not %s", err, named, notNamed)
			}
			states, reasons := r.ends(t, r.hash)
			if len(states) != 1 || states[0] != "failed" || !strings.Contains(reasons[0], named) || strings.Contains(reasons[0], notNamed) {
				t.Errorf("the jobs ended %v %q, want one failed naming %s and not %s", states, reasons, named, notNamed)
			}
			if listed, marked := r.subtitleState(t); !listed || marked {
				t.Errorf("a store's refusal hid the subtitle file (listed %v, marked unreadable %v): it is the store's failure, not the file's", listed, marked)
			}
			neverAway(t, "the worker", &workerLog, db.Of(w.Pool))
		})
	}
}

// Postgres going away at a statement of the job's own (the read of the source row, of a copy's keyframe
// index, of an image track's cues, the check at a chunk's start, the commit of a segment) cuts the
// statement, refuses connections for a moment, and never ends the job failed: it goes on, or is cancelled
// and the viewer's next request is served. This holds before and after the change; it guards the mapping
// of every one of these calls as a pool call.
func TestPostgresGoingAwayAtAJobsOwnStatementNeverFailsIt(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
		prepare         func(*testing.T, *rig)
		through         int // The segment the viewer plays to.
	}{
		{"the read of the source row", "m.object_etag", func(*testing.T, *rig) {}, 3},
		{"the read of a copy's keyframe index", "SELECT m.keyframes, m.video_end", func(t *testing.T, r *rig) { r.copied(t, "film.mkv") }, 3},
		{"the read of the image cues", "SELECT image_cues FROM media_sources", embeddedImageTrack, 3},
		{"the check at a chunk's start", "j.start_segment <= $3", func(*testing.T, *rig) {}, 5},
		{"the commit of a segment", "SET next_segment", func(*testing.T, *rig) {}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slow(t)
			r := newRig(t, nil)
			tc.prepare(t, r)
			proxy, viaProxy := r.proxied(t)
			r.worker(t, func(w *Worker) { viaProxy(w); w.ChunkSegments = 2 })
			j := r.jobs(t)
			tripped := proxy.SeverOn(tc.statement)
			played := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				_, _, err := r.play(ctx, j, psid(0), 0, tc.through)
				played <- err
			}()
			select {
			case <-tripped:
			case err := <-played:
				t.Fatalf("the viewer was served (%v) before the worker ran the statement %q", err, tc.statement)
			case <-time.After(90 * time.Second):
				t.Fatalf("the worker never ran a statement with %q", tc.statement)
			}
			proxy.Away() // New connections are refused too, as when Postgres restarts.
			time.Sleep(time.Second)
			proxy.Back()
			if err := <-played; err != nil {
				t.Errorf("the viewer's requests were not served after the outage: %v", err)
			}
			r.noFailed(t)
			if reason, err := j.Unavailable(context.Background(), r.spec, "", ""); err != nil || reason != "" {
				t.Errorf("PlaybackInfo refuses the spec after the outage: %q %v", reason, err)
			}
		})
	}
}

// embeddedImageTrack makes the rig's film one with a PGS track inside it, burnt in: such a run reads the
// track's cues from the media source's row.
func embeddedImageTrack(t *testing.T, r *rig) {
	t.Helper()
	dir := film(t)
	cueFilm{codec: "copy", seconds: 20, cues: []testenv.Cue{top(2, 6)}}.write(t, dir, "embedded")
	t.Cleanup(func() { os.Remove(dir + "/embedded.mkv") })
	source := r.useFile(t, "embedded.mkv")
	for _, stream := range source.Streams {
		if stream.IsImage() {
			index := stream.Index
			r.spec.Subtitle, r.spec.SubtitleFingerprint = &index, source.SubtitleFingerprint(index)
			r.hash = r.spec.Hash()
			return
		}
	}
	t.Fatalf("the film has no image track: %+v", source.Streams)
}

// A deadlock (40P01) is Postgres answering for the other transaction's sake: it must never end a job failed.
// The job's first commit of a segment and the retry that follows it are each made the victim of a deadlock,
// one after the other, by one other session of the test's. That session holds an advisory lock from before
// the worker starts. A BEFORE UPDATE trigger on a segment commit (which runs once the commit holds the job's
// row) finds the lock taken and sleeps until the session that holds it is waiting for this very transaction,
// and only then asks for the lock. So the commit completes the cycle itself, and the timer that finds it is
// the commit's own: the other session's deadlock_timeout is ten minutes.
//
// Nothing but the other session lets a commit out of the trigger: the lease keeper, a sweep or a reap that
// queues for the row is not the lock's holder, and until the commit is let out it waits for no lock, so no
// one can close a cycle through it. The other session asks for the row only when it has seen a commit asleep
// in the trigger, which stays so until it asks. No window decides the victim, however slow the machine.
//
// The keeper cannot renew the job while the commit holds its row (deadlock_timeout, a second, and however
// long this test takes to ask). The lease here is 30 seconds so that this never gives the job up; the rig's
// 3 seconds left about a second for the other session to ask. One job of the spec (no helpers), so that the
// commit in the trigger is of the row the other session asks for.
func TestASegmentCommitThatPostgresAbortsForADeadlockNeverFailsTheJob(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	ctx := context.Background()
	const lock = 1070107
	// Removed first, whatever happens: a trigger left on transcode_jobs would slow every later test.
	t.Cleanup(func() {
		r.pool.Exec(ctx, "DROP TRIGGER IF EXISTS js107_gate ON transcode_jobs") //nolint:errcheck
		r.pool.Exec(ctx, "DROP FUNCTION IF EXISTS js107_gate()")                //nolint:errcheck
	})
	for _, sql := range []string{
		// The bound of the loop (60 s) only keeps a broken test from hanging: the job is given up before it.
		fmt.Sprintf(`CREATE FUNCTION js107_gate() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF pg_try_advisory_xact_lock(%[1]d) THEN RETURN NEW; END IF;
				FOR i IN 1..6000 LOOP
					EXIT WHEN EXISTS (SELECT 1 FROM pg_locks holds JOIN pg_locks waits USING (pid)
						WHERE holds.locktype = 'advisory' AND holds.granted AND holds.classid = 0 AND holds.objid = %[1]d
							AND waits.locktype = 'transactionid' AND NOT waits.granted
							AND waits.transactionid = pg_current_xact_id()::xid);
					PERFORM pg_sleep(0.01);
				END LOOP;
				PERFORM pg_advisory_xact_lock(%[1]d);
				RETURN NEW;
			END $$`, lock),
		`CREATE TRIGGER js107_gate BEFORE UPDATE ON transcode_jobs FOR EACH ROW
			WHEN (NEW.next_segment IS DISTINCT FROM OLD.next_segment) EXECUTE FUNCTION js107_gate()`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	deadlocks := func() int {
		return r.count(t, "SELECT deadlocks::int FROM pg_stat_database WHERE datname = current_database()")
	}
	before := deadlocks()

	// The other session, with the advisory lock, before there is a worker to commit anything.
	other, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := other.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, "SET LOCAL deadlock_timeout = '10min'")
	}
	if err == nil {
		_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lock)
	}
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			tx.Rollback(ctx) //nolint:errcheck
			other.Release()
		}
	}
	t.Cleanup(release)

	r.worker(t, func(w *Worker) { w.NoHelpers, w.Lease = true, 30*time.Second })
	j := r.jobs(t)
	played := make(chan error, 1)
	go func() {
		playCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		_, _, err := r.play(playCtx, j, psid(0), 0, 5)
		played <- err
	}()

	var victims []int
	for round := 1; round <= 2; round++ {
		// A commit asleep in the trigger holds the job's row, and stays there until this session asks for it.
		deadline := time.Now().Add(90 * time.Second)
		for r.count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'PgSleep' AND query LIKE 'UPDATE transcode_jobs SET next_segment%'`) == 0 {
			select {
			case err := <-played:
				t.Fatalf("round %d: the viewer's play ended (%v) before the worker's commit was made the deadlock's victim again", round, err)
			case <-time.After(10 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the worker never committed a segment", round)
			}
		}
		id := r.jobOf(t, r.hash)
		var victim int
		if err := r.pool.QueryRow(ctx, "SELECT next_segment FROM transcode_jobs WHERE id = $1", id).Scan(&victim); err != nil {
			t.Fatal(err)
		}
		// In a savepoint: its end lets the row go and keeps the advisory lock for the next round.
		row, err := tx.Begin(ctx)
		if err == nil {
			_, err = row.Exec(ctx, "SELECT 1 FROM transcode_jobs WHERE id = $1 FOR UPDATE", id)
		}
		if err != nil {
			t.Fatalf("round %d: the other session's statement failed, so it, not the commit, was the victim: %v", round, err)
		}
		if err := row.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		victims = append(victims, victim)
	}
	release()
	// Each time the other session was answered, the commit's transaction had ended: by a deadlock, and not
	// by the job being given up, if Postgres counted two. At least: the count is the database's, and an
	// earlier test's deadlock may be reported late.
	waitFor(t, "the two deadlocks to be counted", 30*time.Second, func() bool { return deadlocks() >= before+2 })
	// The trigger goes now: what follows is the viewer's play to its end.
	r.pool.Exec(ctx, "DROP TRIGGER js107_gate ON transcode_jobs") //nolint:errcheck
	if err := <-played; err != nil {
		t.Errorf("the viewer's requests were not served after the deadlocks: %v", err)
	}
	r.noFailed(t)
	for _, v := range victims {
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND start_segment <= $2 AND next_segment > $2", r.hash, v); n == 0 {
			t.Errorf("no job of the spec recorded segment %d, which a deadlock aborted the commit of", v)
		}
	}
	if reason, err := j.Unavailable(ctx, r.spec, "", ""); err != nil || reason != "" {
		t.Errorf("PlaybackInfo refuses the spec after the deadlocks: %q %v", reason, err)
	}
	t.Logf("deadlock victims: segments %v", victims)
}
