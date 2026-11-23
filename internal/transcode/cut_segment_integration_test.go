//go:build integration

package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-159: a segment that ffmpeg cut short because its input ended early is never stored (DOMAINS invariant 40),
// whichever the worker sees first: ffmpeg's exit, or its periodic upload check finding the segment in place.
//
// ffmpeg flushes the segment it was writing when its input ends, and exits 0 either way. The worker's exit path
// judges that segment (checkLast); its upload check, which runs every 100 ms while ffmpeg lives, must not store it
// before the exit is seen. A real ffmpeg exits within milliseconds of the flush, so the check seldom finds the
// segment first: the checks below force it. The ffmpeg on the PATH is a shim that runs the real one and, for the
// job's first run only, holds its own exit until the test lets it go, so that the real ffmpeg has put its cut segment
// in place and the worker still sees ffmpeg alive. The hold is proved to have happened (the shim says the real
// ffmpeg has exited, the worker's work came round well after that, and the cut segment is in the scratch directory),
// and a hold that missed fails the check, never passes it.
//
// The order the other way round (the worker sees the exit first) is the one an unheld run takes nearly always, and
// the "unheld" subtests run it; no hook of the worker would force it, so those runs are not forced. The checks of the
// film's true last segment and of a whole segment held back pin what the fix must not take away.
//
// Mutants a right check fails: store each segment as found (main); keep a segment back once the playlist exists and
// leave it to the exit path (passes the exit hold, fails the playlist hold); keep each segment back until the next
// is in place or ffmpeg has exited (fails the held-right-after-a-segment check); refuse the segment at the upload
// check and fail the job at once (a right build, which the proof of the hold accepts).
//
// The checks name nothing the build must add. They ask Jobs.Segment, Jobs.Unavailable, the transcode store, the
// jobs-ended counter and the worker's log, and read the leased job's turnedAt, which exists today.

// cutFPS is the film's frame rate (film() makes 24 fps).
const cutFPS = 24

// cutCase is one of the 2 x 2 x 2 x 2 ways the input of a job ends early.
type cutCase struct {
	place       int  // The segment the cut falls in: 0, or 3 (at least two whole segments before it).
	copied      bool // A copied stream (10 s segments on the file's keyframes) and not a re-encode (4 s segments).
	sourceFails bool // The source store's read fails for good; otherwise the file itself ends early.
	frameless   bool // The cut leaves the place with no frame; otherwise about half of it.
}

func (c cutCase) name() string {
	parts := []string{"cut at segment " + strconv.Itoa(c.place), "a re-encode", "the file ends early", "half the place is left"}
	if c.copied {
		parts[1] = "a copy"
	}
	if c.sourceFails {
		parts[2] = "the source read fails for good"
	}
	if c.frameless {
		parts[3] = "no frame of the place is left"
	}
	return strings.Join(parts, ", ")
}

func cutCases() []cutCase {
	var cases []cutCase
	for _, place := range []int{0, 3} {
		for _, copied := range []bool{false, true} {
			for _, sourceFails := range []bool{false, true} {
				for _, frameless := range []bool{false, true} {
					cases = append(cases, cutCase{place, copied, sourceFails, frameless})
				}
			}
		}
	}
	return cases
}

// cutPacket is a video packet of the film as ffprobe reads it.
type cutPacket struct {
	at  float64
	pos int64
}

var (
	cutPacketsOnce sync.Once
	cutPackets     []cutPacket
)

func filmPackets(t *testing.T) []cutPacket {
	t.Helper()
	cutPacketsOnce.Do(func() {
		out := run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,pos", "-of", "csv=p=0",
			filepath.Join(film(t), "film.mkv"))
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			at, pos, ok := strings.Cut(strings.TrimSpace(line), ",")
			if !ok {
				continue
			}
			a, err1 := strconv.ParseFloat(at, 64)
			p, err2 := strconv.ParseInt(strings.TrimSuffix(pos, ","), 10, 64)
			if err1 == nil && err2 == nil {
				cutPackets = append(cutPackets, cutPacket{a, p})
			}
		}
		sort.Slice(cutPackets, func(i, j int) bool { return cutPackets[i].at < cutPackets[j].at })
	})
	if len(cutPackets) < 100 {
		t.Fatalf("ffprobe listed %d packets of the film", len(cutPackets))
	}
	return cutPackets
}

// packetAt is the first video packet at or after the time.
func packetAt(t *testing.T, seconds float64) cutPacket {
	t.Helper()
	for _, p := range filmPackets(t) {
		if p.at >= seconds-0.0001 {
			return p
		}
	}
	t.Fatalf("no packet at %.2f s", seconds)
	return cutPacket{}
}

// plan is where the places are and where the input stops.
type cutPlan struct {
	starts []float64 // The start of every segment, in seconds, and the film's end last.
	after  int64     // The bytes of the file that are left.
	at     float64   // The film time of the first frame that is not left.
	whole  int       // The segments that end before the cut: ffmpeg completes them, and the one it flushes is the next.
	place  int       // The segment the cut falls in: the first one that has no frame of its own after the cut.
}

// frames is the fewest frames segment n has when it is whole: its length at the film's rate, less one.
func (p cutPlan) frames(n int) int { return int((p.starts[n+1]-p.starts[n])*cutFPS) - 1 }

func (c cutCase) plan(t *testing.T, r *rig) cutPlan {
	t.Helper()
	var p cutPlan
	if c.copied {
		var kf []int64
		if err := r.pool.QueryRow(context.Background(), "SELECT keyframes FROM media_sources").Scan(&kf); err != nil {
			t.Fatal(err)
		}
		for _, k := range kf {
			p.starts = append(p.starts, float64(k-kf[0])/ticksPerSecond)
		}
		p.starts = append(p.starts, 90)
	} else {
		for s := 0.0; s < 88; s += 4 {
			p.starts = append(p.starts, s)
		}
		p.starts = append(p.starts, 88, 90) // The film's last segment is its 2 s tail.
	}
	start, end := p.starts[c.place], p.starts[c.place+1]
	// A cut inside the first packet of the place leaves no frame of it, only the torn start of one. At segment 0 that
	// would leave ffmpeg nothing to open (it fails on the file's own data, not on an input that ended early), so the
	// cut there is after the first frame. Any cut in the film's first 10 s (its first matroska cluster, which a cut
	// file has no index to) makes ffmpeg's seek to 0 fail, and it then flushes an empty segment 0 of 24 bytes (one
	// styp box, no frame) and exits 0: both cuts at segment 0 leave that segment with no frame.
	packet := packetAt(t, start)
	if c.frameless && c.place == 0 {
		p.after, p.at = packetAt(t, 1.0/cutFPS).pos, 1.0/cutFPS
	} else if c.frameless {
		p.after, p.at = packet.pos+1000, start
	} else {
		half := packetAt(t, start+(end-start)/2)
		p.after, p.at = half.pos, half.at
	}
	for n := 0; n+1 < len(p.starts); n++ {
		if p.starts[n+1] < p.at-0.001 {
			p.whole = n + 1
		}
		if p.starts[n] <= p.at+0.001 {
			p.place = n
		}
	}
	return p
}

// segmentPuts keeps every put's body, as the transcode store.
type segmentPuts struct {
	store.Store
	mu     sync.Mutex
	puts   map[string]int
	bodies map[string][]byte
}

func newSegmentPuts(s store.Store) *segmentPuts {
	return &segmentPuts{Store: s, puts: map[string]int{}, bodies: map[string][]byte{}}
}

func (s *segmentPuts) Put(ctx context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.puts[key]++
	s.bodies[key] = data
	s.mu.Unlock()
	return s.Store.Put(ctx, key, bytes.NewReader(data))
}

// segments is what was put for the spec: the segment numbers with the times each was put, and the init's bytes.
func (s *segmentPuts) segments(hash string) (counts map[int]int, bodies map[int][]byte, init []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts, bodies = map[int]int{}, map[int][]byte{}
	for key, n := range s.puts {
		rest, ok := strings.CutPrefix(key, "segments/"+hash+"/")
		if !ok {
			continue
		}
		if rest == "init.mp4" {
			init = s.bodies[key]
			continue
		}
		i, err := strconv.Atoi(strings.TrimSuffix(rest, ".mp4"))
		if err == nil {
			counts[i], bodies[i] = n, s.bodies[key]
		}
	}
	return counts, bodies, init
}

// framesOf counts the video frames of a segment as a player sees them, joined to the init. A file ffprobe cannot read
// (the 24 bytes of a segment that holds no more than its header) has none.
func framesOf(t *testing.T, init, seg []byte) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.mp4")
	if err := os.WriteFile(path, append(append([]byte{}, init...), seg...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets",
		"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.Trim(fields[0], ","))
	return n
}

// holdKind is where a run's ffmpeg is held, so that the worker's upload check runs while ffmpeg's last segment is in
// place and the worker still sees ffmpeg alive.
type holdKind int

const (
	unheld         holdKind = iota // The run is the real ffmpeg; the worker sees its exit first, as nearly always.
	heldAtExit                     // The real ffmpeg has finished; its shim holds the exit.
	heldAtPlaylist                 // The real ffmpeg is alive, blocked at the rename of its playlist, after its last segment's.
)

func (h holdKind) String() string {
	return map[holdKind]string{
		unheld:         "unheld, the exit seen first",
		heldAtExit:     "the upload check finds the cut segment in place with ffmpeg held at its exit",
		heldAtPlaylist: "the upload check finds the cut segment in place with ffmpeg held before its playlist is written",
	}[h]
}

// The playlist hold is for a re-encode only. Its vod playlist is written once, after its last segment: ffmpeg held
// there has the segment in place and no playlist, which a fix that waits for the playlist does not cover. A copy's
// playlist is rewritten at every fragment, so the last fragment's write cannot be told from the others by a rename.

// cutShim puts an ffmpeg on the PATH. For the first run that writes segments, in a held order, the real ffmpeg runs
// and is held: heldAtExit runs it to its end, writes what is in its output directory to inplace, says reached and
// does not exit until release exists (the shim is asleep, not ffmpeg: it has finished and flushed, and the worker
// sees the process it started alive); heldAtPlaylist runs it with testdata/holdrename.c preloaded, which does the
// same at the rename of its playlist, so that ffmpeg itself is alive. Every other run is the real ffmpeg itself.
type cutShim struct{ dir string }

func newCutShim(t *testing.T, hold holdKind) *cutShim {
	t.Helper()
	film(t) // Made at full speed, before the shim is in the way.
	real := testenv.FFmpeg(t)
	s := &cutShim{dir: t.TempDir()}
	if hold != unheld {
		if err := os.WriteFile(filepath.Join(s.dir, "hold"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	preload := ""
	if hold == heldAtPlaylist {
		src, err := filepath.Abs(filepath.Join("testdata", "holdrename.c"))
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("gcc", "-shared", "-fPIC", "-o", filepath.Join(s.dir, "holdrename.so"), src, "-ldl").CombinedOutput(); err != nil {
			t.Fatalf("building the rename hold: %v\n%s", err, out)
		}
		// exec: the process the worker started is ffmpeg itself, so that its kill reaches it.
		preload = "export LD_PRELOAD=" + filepath.Join(s.dir, "holdrename.so") + " JS159_HOLD_DIR=" + s.dir + "\nexec " + real + ` "$@"` + "\n"
	}
	script := `#!/bin/sh
case "$*" in
*"-f hls"*) ;;
*) exec ` + real + ` "$@";;
esac
n=$(( $(cat ` + s.dir + `/runs 2>/dev/null || echo 0) + 1 ))
echo $n > ` + s.dir + `/runs
if [ "$n" -gt 1 ] || [ ! -e ` + s.dir + `/hold ]; then exec ` + real + ` "$@"; fi
for last; do :; done
out=$(dirname "$last")
` + preload + real + ` "$@"
rc=$?
`
	if hold == heldAtExit {
		script += `ls -1 "$out" > ` + s.dir + `/inplace
touch ` + s.dir + `/reached
while [ ! -e ` + s.dir + `/release ]; do sleep 0.02; done
`
	}
	script += "exit $rc\n"
	if err := os.WriteFile(filepath.Join(s.dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", s.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(s.release) // A failed test must not leave ffmpeg held.
	return s
}

func (s *cutShim) reached() bool {
	_, err := os.Stat(filepath.Join(s.dir, "reached"))
	return err == nil
}
func (s *cutShim) release() {
	os.WriteFile(filepath.Join(s.dir, "release"), nil, 0o644) //nolint:errcheck
}

// prove waits until the worker's upload check has run with ffmpeg held and its last segment in place, and returns
// the names that were in ffmpeg's output directory when it was held (the held ffmpeg, or its shim, wrote them before
// it said it was held, so they are the files the check found). The worker has run that check when its work came
// round a second after the hold (a hundred of its 100 ms checks' worth), or when its job has failed while ffmpeg
// is still held: the worker could only have learned of the cut from the check, since it started the process
// that is alive. A build that refuses the segment at its first check fails the job within 100 ms, and is as right
// as one that keeps polling beside it, so either is proof. Nothing here fails a build; a hold that did not come
// fails the check, never passes it.
func (s *cutShim) prove(t *testing.T, r *rig, w *Worker) []string {
	t.Helper()
	waitFor(t, "ffmpeg to be held with its last segment in place", 90*time.Second, s.reached)
	heldAt := time.Now()
	waitFor(t, "the worker's upload check to have run beside the held ffmpeg (its work came round a second later, or it failed the job)", 30*time.Second,
		func() bool {
			return turnedAt(w).After(heldAt.Add(time.Second)) || r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") > 0
		})
	text, err := os.ReadFile(filepath.Join(s.dir, "inplace"))
	if err != nil {
		t.Fatalf("the hold left no list of what was in place: %v", err)
	}
	return strings.Fields(string(text))
}

// turnedAt is when the worker's job last came round (its upload check and its loop both turn it).
func turnedAt(w *Worker) time.Time {
	k := w.keeper()
	k.mu.Lock()
	defer k.mu.Unlock()
	var at time.Time
	for id, j := range k.jobs {
		if id.key == "" && j.turnedAt.After(at) {
			at = j.turnedAt
		}
	}
	return at
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// armCutAt is armSourceCut with the byte the reads stop at: every read of the film at or past it fails, for every job.
func armCutAt(t *testing.T, r *rig, change *[]func(*Worker), after int64) string {
	t.Helper()
	var key string
	if err := r.pool.QueryRow(context.Background(), "SELECT store_key FROM media_sources").Scan(&key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(film(t), key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	left := new(atomic.Int32)
	*change = append(*change, func(w *Worker) {
		opened := w.Media
		w.Media = func(uri string) (store.Store, error) {
			inner, err := opened(uri)
			if err != nil {
				return nil, err
			}
			return &recoveryCuttingMedia{Store: inner, film: key, after: after, always: true, left: left}, nil
		}
	})
	return "file://" + film(t)
}

// cutFile makes a copy of the film that stops after the bytes, and has the rig's source be it. A copy's keyframe
// index is read from the whole film first, as the scan did before the file was cut.
func (c cutCase) useCutFile(t *testing.T, r *rig, after int64) {
	t.Helper()
	name := fmt.Sprintf("cut-js159-%d.mkv", time.Now().UnixNano())
	src, err := os.Open(filepath.Join(film(t), "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(filepath.Join(film(t), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(filepath.Join(film(t), name)) })
	if _, err := io.CopyN(dst, src, after); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = $1", name); err != nil {
		t.Fatal(err)
	}
}

// cutRun is what one job's early end left.
type cutRun struct {
	r      *rig
	rec    *segmentPuts
	other  *Jobs // Another stream replica.
	plan   cutPlan
	states []string
	log    string
	wait   error // What the request that waited on the segment ffmpeg could not finish was answered with.
}

// run plays a film whose input ends early as a viewer does, from its first segment. hold forces the order in
// which the worker's upload check finds the cut-short segment while ffmpeg is still alive.
func (c cutCase) run(t *testing.T, hold holdKind) *cutRun {
	t.Helper()
	r := newRig(t, nil)
	rec := newSegmentPuts(r.segments)
	r.segments = rec
	logs := &lockedLog{}
	r.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if c.copied {
		r.copied(t, "film.mkv")
	}
	plan := c.plan(t, r)
	var change []func(*Worker)
	if c.sourceFails {
		armCutAt(t, r, &change, plan.after)
	} else {
		c.useCutFile(t, r, plan.after)
	}
	shim := newCutShim(t, hold)
	var worker *Worker
	r.worker(t, func(w *Worker) {
		// Production's lease and heartbeat: the worker's loop is then turned by its own 100 ms check, and a
		// renewal does not come every 200 ms to turn it for the check.
		w.Lease, w.Heartbeat = 10*time.Second, 3*time.Second
		for _, f := range change {
			f(w)
		}
		worker = w
	})
	j, other := r.jobs(t), r.jobs(t) // Two stream replicas.
	for _, jobs := range []*Jobs{j, other} {
		jobs.WaitTimeout = recoveryWait
		jobs.Segments = rec
	}
	failedBefore, doneBefore := failedJobs(t), counterValue(t, jobsEnded.WithLabelValues("done"))
	interruptedBefore := counterValue(t, jobsEnded.WithLabelValues("interrupted"))

	// The viewer asks the segments that end before the cut in order, then the one the cut falls in.
	asked := make(chan error, 1)
	served := make([][]byte, plan.whole)
	go func() {
		for n := 0; n < plan.whole; n++ {
			body, err := r.fetch(t, j, psid(0), n)
			if err != nil {
				asked <- fmt.Errorf("segment %d, before the cut: %w", n, err)
				return
			}
			served[n] = body
		}
		_, err := r.fetch(t, j, psid(0), plan.place)
		asked <- err
	}()

	if hold != unheld {
		inPlace := shim.prove(t, r, worker)
		counts, bodies, init := rec.segments(r.hash)
		cut := strconv.Itoa(plan.whole) + ".m4s"
		inPlaceSegment := false
		for _, name := range inPlace {
			// A re-encode's cut segment is a file of its own; a copy's are fragments the worker builds segments from.
			if name == cut || (c.copied && strings.HasSuffix(name, ".m4s")) {
				inPlaceSegment = true
			}
		}
		if !inPlaceSegment && len(counts) <= plan.whole {
			t.Fatalf("the hold missed: ffmpeg was held, but %v was in its output directory (no %s) and none was stored past segment %d", inPlace, cut, plan.whole-1)
		}
		for n, count := range counts {
			if frames := framesOf(t, init, bodies[n]); count > 0 && (n >= plan.place || frames < plan.frames(n)) {
				t.Errorf("segment %d, ffmpeg's cut-short last (%d frames, %d bytes), was stored while ffmpeg's exit was not yet seen", n, frames, len(bodies[n]))
			}
		}
		shim.release()
	}

	var wait error
	select {
	case wait = <-asked:
	case <-time.After(2 * recoveryWait):
		t.Fatalf("the request for segment %d was not answered", plan.place)
	}
	r.idle(t)
	cr := &cutRun{r: r, rec: rec, other: other, plan: plan, states: r.states(t), log: logs.String(), wait: wait}
	cr.counted(t, failedBefore, doneBefore, interruptedBefore, served)
	return cr
}

// counted checks what every early end must leave, and returns the run for the checks that belong to a case.
func (cr *cutRun) counted(t *testing.T, failedBefore, doneBefore, interruptedBefore float64, served [][]byte) {
	t.Helper()
	failed := failedJobs(t) - failedBefore
	if int(failed) != len(cr.states) {
		t.Errorf("jellyscale_transcode_jobs_ended_total{result=\"failed\"} rose by %v for %d jobs (%v): each failed job is counted once", failed, len(cr.states), cr.states)
	}
	if done := counterValue(t, jobsEnded.WithLabelValues("done")) - doneBefore; done != 0 {
		t.Errorf("%v jobs were counted done: a job whose input ended early is failed", done)
	}
	if n := counterValue(t, jobsEnded.WithLabelValues("interrupted")) - interruptedBefore; n != 0 {
		t.Errorf("%v jobs were counted interrupted: a job whose input ended early is failed", n)
	}
	if n := strings.Count(cr.log, `msg="transcode failed"`); n != len(cr.states) {
		t.Errorf("%d \"transcode failed\" lines in the worker's log for %d failed jobs, each is logged once with its reason", n, len(cr.states))
	}
	// The segments: every whole one before the cut stored once and served whole, nothing at the cut's place or after.
	counts, bodies, init := cr.rec.segments(cr.r.hash)
	full := cr.plan.frames
	stored := 0
	for n := range cr.plan.starts[:len(cr.plan.starts)-1] {
		if counts[n] == 0 {
			break
		}
		stored = n + 1
	}
	for n, count := range counts {
		if count != 1 {
			t.Errorf("segment %d was stored %d times, want once", n, count)
		}
		if n >= stored {
			t.Errorf("segment %d is stored with a gap before it (stored through %d)", n, stored-1)
		}
		if got := framesOf(t, init, bodies[n]); got < full(n) {
			t.Errorf("segment %d is stored with %d frames (%d bytes) and its place holds %d: a segment shorter than its place was stored", n, got, len(bodies[n]), full(n)+1)
		}
	}
	if stored < cr.plan.whole {
		t.Errorf("%d segments stored, want at least the %d that ended before the cut", stored, cr.plan.whole)
	}
	cutPlace := cr.plan.place
	if stored > cutPlace {
		t.Errorf("segments are stored through %d, but the cut fell in segment %d", stored-1, cutPlace)
	}
	for n := cutPlace; n < len(cr.plan.starts)-1; n++ {
		if _, err := cr.rec.Store.Open(context.Background(), SegmentKey(cr.r.hash, n), 0); err == nil {
			t.Errorf("segment %d, at or after the cut, is in the transcode store", n)
		}
	}
	for n, body := range served {
		if got := framesOf(t, init, body); got < full(n) {
			t.Errorf("segment %d was answered with %d frames, its place holds %d", n, got, full(n)+1)
		}
	}
	// Another viewer, through another stream replica, is not served a short segment at the cut or after it.
	for n := stored; n <= cutPlace+1 && n < len(cr.plan.starts)-1; n++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		body, err := bytesOf(cr.other.Segment(ctx, cr.r.spec, cr.r.hash, cr.r.viewer(psid(1)), n, 0))
		cancel()
		if err == nil {
			if got := framesOf(t, init, body); got < full(n) {
				t.Errorf("another viewer was served segment %d with %d frames, its place holds %d", n, got, full(n)+1)
			}
		}
	}
}

// scratchJob is the one job directory of the rig's scratch.
func (r *rig) scratchJob(t *testing.T) string {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(r.scratch, "job-*"))
	if len(dirs) != 1 {
		t.Fatalf("%d job directories in the scratch directory, want 1", len(dirs))
	}
	return dirs[0]
}

// A film whose input ends early, in each of the 16 ways, with the worker's upload check finding the cut-short
// segment while ffmpeg is alive: held at its exit (after its playlist is written), held before its playlist is
// written (a re-encode), and with the exit seen first (natural, not forced).
func TestAnInputThatEndsEarlyNeverLeavesACutShortSegmentStored(t *testing.T) {
	for _, hold := range []holdKind{heldAtExit, heldAtPlaylist, unheld} {
		for _, c := range cutCases() {
			if hold == heldAtPlaylist && c.copied {
				continue // holdsFor says why.
			}
			t.Run(hold.String()+"/"+c.name(), func(t *testing.T) {
				cr := c.run(t, hold)
				var failed FailedError
				if !errors.As(cr.wait, &failed) {
					t.Fatalf("the request for segment %d (the one the cut falls in) got %v, want the failure", cr.plan.place, cr.wait)
				}
				if c.sourceFails {
					// The cause that persists names the source store, after at most three fresh jobs.
					if !strings.Contains(failed.Reason, "file://"+film(t)) || strings.Contains(failed.Reason, "the input ended early") {
						t.Errorf("the failure is %q, want the one that names the source store and not \"the input ended early\"", failed.Reason)
					}
					if len(cr.states) < 2 || len(cr.states) > 1+recoveryFreshJobs {
						t.Errorf("%d jobs %v, want the first and at most %d fresh ones", len(cr.states), cr.states, recoveryFreshJobs)
					}
				} else {
					if !strings.Contains(failed.Reason, "the input ended early") {
						t.Errorf("the failure is %q, want \"the input ended early\"", failed.Reason)
					}
					if len(cr.states) != 1 {
						t.Errorf("%d jobs %v, want exactly one: a file that ends early is not made again within failureMemory", len(cr.states), cr.states)
					}
					reason, err := cr.other.Unavailable(context.Background(), cr.r.spec, cr.r.user, "device")
					if err != nil || !strings.Contains(reason, "the last attempt failed") || !strings.Contains(reason, "the input ended early") {
						t.Errorf("PlaybackInfo refuses the spec with %q, %v, want the memory of the failure", reason, err)
					}
				}
				for _, state := range cr.states {
					if state != "failed" {
						t.Errorf("the jobs ended %v, want every one failed", cr.states)
						break
					}
				}
			})
		}
	}
}

// The film's true last segment is stored and served whichever order the worker sees ffmpeg's exit and its own check,
// for a re-encode and a copy: the playlist's last segment, within 1.5 s of its length. One more than 1.5 s short is
// refused, as invariant 40 says. A re-encode's last place is 2 s (from 88 s), a copy's 10 s (from 80 s); the file ends
// at the cut, and the last frame is one frame before it.
func TestTheFilmsTrueLastSegmentIsStoredAndOneMoreThan1500msShortIsNot(t *testing.T) {
	for _, hold := range []holdKind{heldAtExit, heldAtPlaylist, unheld} {
		for _, tc := range []struct {
			copied     bool
			cut        float64
			storedLast bool
		}{
			{false, 90, true},   // The film's own end, the file whole.
			{false, 88.6, true}, // 1.4 s short of the playlist's 2 s: within 1.5 s.
			{false, 88.2, false},
			{true, 90, true},
			{true, 88.8, true}, // 1.2 s short of the 10 s.
			{true, 88.0, false},
		} {
			if hold == heldAtPlaylist && tc.copied {
				continue // holdsFor says why.
			}
			name := fmt.Sprintf("%s, %s, cut at %.1f s, the last segment stored %v", hold, map[bool]string{false: "a re-encode", true: "a copy"}[tc.copied], tc.cut, tc.storedLast)
			t.Run(name, func(t *testing.T) { runLastSegment(t, tc.copied, hold, tc.cut, tc.storedLast) })
		}
	}
}

func runLastSegment(t *testing.T, copied bool, hold holdKind, cut float64, wantLast bool) {
	t.Helper()
	r := newRig(t, nil)
	rec := newSegmentPuts(r.segments)
	r.segments = rec
	if copied {
		r.copied(t, "film.mkv")
	}
	after := int64(0)
	if cut >= 90 {
		info, err := os.Stat(filepath.Join(film(t), "film.mkv"))
		if err != nil {
			t.Fatal(err)
		}
		after = info.Size()
	} else {
		after = packetAt(t, cut).pos
	}
	cutCase{copied: copied}.useCutFile(t, r, after)
	shim := newCutShim(t, hold)
	var worker *Worker
	r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = 10*time.Second, 3*time.Second; worker = w })
	j := r.jobs(t)
	j.WaitTimeout = recoveryWait
	j.Segments = rec
	count := 23
	if copied {
		count = 9
	}
	got := make(chan error, 1)
	var lastBody []byte
	go func() {
		var err error
		for n := 0; n < count && err == nil; n++ {
			var body []byte
			if body, err = r.fetch(t, j, psid(0), n); n == count-1 {
				lastBody = body
			}
		}
		got <- err
	}()
	if hold != unheld {
		shim.prove(t, r, worker)
		shim.release()
	}
	var err error
	select {
	case err = <-got:
	case <-time.After(2 * recoveryWait):
		t.Fatal("the film's segments were not all answered")
	}
	r.idle(t)
	counts, bodies, init := rec.segments(r.hash)
	for n := 0; n < count-1; n++ {
		if counts[n] != 1 {
			t.Errorf("segment %d stored %d times, want once", n, counts[n])
		}
	}
	if wantLast {
		if err != nil || counts[count-1] != 1 || len(lastBody) == 0 {
			t.Fatalf("the film's last segment (%d of %d) was %d times stored, answered with %d bytes and %v, want it stored once and served", count-1, count, counts[count-1], len(lastBody), err)
		}
		if got := framesOf(t, init, bodies[count-1]); got == 0 {
			t.Errorf("the last segment holds no frame")
		}
		if states := r.states(t); len(states) != 1 || states[0] != "done" {
			t.Errorf("the jobs ended %v, want one done", states)
		}
		return
	}
	var failed FailedError
	if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "the input ended early") || counts[count-1] != 0 {
		t.Errorf("a last segment more than 1.5 s short: stored %d times, answered with %v, want it refused with \"the input ended early\"", counts[count-1], err)
	}
}

// A whole segment is stored and served as soon as ffmpeg has put it in place: a fix that keeps each segment back until
// the next one is made, or until ffmpeg exits, fails here. ffmpeg reads at the speed of the film (-re), so that
// its next segment is seconds away, and is stopped (SIGSTOP) once the first is in place: it is held for as long as
// the check asks, not at its exit.
func TestAWholeSegmentIsStoredAndServedWhileFFmpegIsHeldRightAfterIt(t *testing.T) {
	for _, copied := range []bool{false, true} {
		name := map[bool]string{false: "a re-encode", true: "a copy"}[copied]
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			rec := newSegmentPuts(r.segments)
			r.segments = rec
			if copied {
				r.copied(t, "film.mkv")
			}
			slow(t)
			r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = 10*time.Second, 3*time.Second })
			j := r.jobs(t)
			j.WaitTimeout = recoveryWait
			j.Segments = rec
			asked := make(chan error, 1)
			go func() { _, err := r.fetch(t, j, psid(0), 0); asked <- err }()

			var pid int
			waitFor(t, "ffmpeg's first segment to be in place", 90*time.Second, func() bool {
				if copied {
					text, _ := os.ReadFile(filepath.Join(r.scratchJobOr(), "index.m3u8"))
					var sum float64
					for _, line := range strings.Split(string(text), "\n") {
						if v, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
							f, _ := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
							sum += f
						}
					}
					if sum < 10.0 {
						return false
					}
				} else if _, err := os.Stat(filepath.Join(r.scratchJobOr(), "0.m4s")); err != nil {
					if counts, _, _ := rec.segments(r.hash); counts[0] == 0 {
						return false
					}
				}
				pid = ffmpegPid(r)
				return pid != 0
			})
			if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatalf("stopping ffmpeg: %v", err)
			}
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGCONT) }) //nolint:errcheck
			// The segment is stored and served with ffmpeg stopped. The bound is a hang's (ten polls and a store), and
			// ffmpeg's next segment is 4 s (10 s, a copy) away at the film's speed, which it cannot reach while stopped.
			waitFor(t, "segment 0 to be stored while ffmpeg is stopped", 20*time.Second, func() bool {
				counts, _, _ := rec.segments(r.hash)
				return counts[0] == 1
			})
			select {
			case err := <-asked:
				if err != nil {
					t.Fatalf("segment 0, while ffmpeg is held: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("segment 0 is stored but was not served while ffmpeg is held")
			}
			counts, bodies, init := rec.segments(r.hash)
			if counts[1] != 0 {
				t.Errorf("segment 1 is stored while ffmpeg is stopped before making it")
			}
			if got, want := framesOf(t, init, bodies[0]), map[bool]int{false: 4 * cutFPS, true: 10 * cutFPS}[copied]-1; got < want {
				t.Errorf("segment 0 holds %d frames, want %d", got, want+1)
			}
		})
	}
}

func (r *rig) scratchJobOr() string {
	dirs, _ := filepath.Glob(filepath.Join(r.scratch, "job-*"))
	if len(dirs) == 0 {
		return filepath.Join(r.scratch, "no-job")
	}
	return dirs[0]
}

// ffmpegPid is the pid of the one ffmpeg that writes into the rig's scratch directory.
func ffmpegPid(r *rig) int {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if filepath.Base(args[0]) == "ffmpeg" && strings.Contains(string(raw), r.scratch) {
			return pid
		}
	}
	return 0
}

// A healthy film played to its end stores every segment of its playlist once and fails no job, as a re-encode and as
// a copy, with ffmpeg held after its last segment (at its exit, and for a re-encode before its playlist) and without.
func TestAHealthyFilmPlayedToItsEndStoresEverySegmentOnceInBothOrders(t *testing.T) {
	for _, hold := range []holdKind{heldAtExit, heldAtPlaylist, unheld} {
		for _, copied := range []bool{false, true} {
			if hold == heldAtPlaylist && copied {
				continue // holdsFor says why.
			}
			name := fmt.Sprintf("%s, copied %v", hold, copied)
			t.Run(name, func(t *testing.T) {
				r := newRig(t, nil)
				rec := newSegmentPuts(r.segments)
				r.segments = rec
				if copied {
					r.copied(t, "film.mkv")
				}
				shim := newCutShim(t, hold)
				var worker *Worker
				r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = 10*time.Second, 3*time.Second; worker = w })
				j := r.jobs(t)
				j.WaitTimeout = recoveryWait
				j.Segments = rec
				failedBefore := failedJobs(t)
				count := map[bool]int{false: 23, true: 9}[copied]
				done := make(chan error, 1)
				go func() {
					var err error
					for n := 0; n < count && err == nil; n++ {
						_, err = r.fetch(t, j, psid(0), n)
					}
					done <- err
				}()
				if hold != unheld {
					shim.prove(t, r, worker)
					shim.release()
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("playing to the end: %v", err)
					}
				case <-time.After(2 * recoveryWait):
					t.Fatal("the film was not played to its end")
				}
				r.idle(t)
				counts, bodies, init := rec.segments(r.hash)
				for n := 0; n < count; n++ {
					if counts[n] != 1 {
						t.Errorf("segment %d was stored %d times, want once", n, counts[n])
					}
				}
				if len(counts) != count {
					t.Errorf("%d segments stored, the playlist has %d", len(counts), count)
				}
				if framesOf(t, init, bodies[count-1]) == 0 {
					t.Errorf("the last segment holds no frame")
				}
				if states := r.states(t); len(states) != 1 || states[0] != "done" {
					t.Errorf("the jobs ended %v, want one done", states)
				}
				if got := failedJobs(t) - failedBefore; got != 0 {
					t.Errorf("%v jobs failed on a healthy film", got)
				}
			})
		}
	}
}

// The time to a first segment, for the record (docs/testing.md, Mechanisms not speeds): logged, never judged.
func TestTimeToFirstSegmentIsLoggedForAHealthyFilm(t *testing.T) {
	for _, copied := range []bool{false, true} {
		for _, seek := range []bool{false, true} {
			name := fmt.Sprintf("%s, %s", map[bool]string{false: "a re-encode", true: "a copy"}[copied], map[bool]string{false: "segment 0", true: "a seek"}[seek])
			t.Run(name, func(t *testing.T) {
				r := newRig(t, nil)
				if copied {
					r.copied(t, "film.mkv")
				}
				r.worker(t, nil)
				j := r.jobs(t)
				n := 0
				if seek {
					n = map[bool]int{false: 14, true: 6}[copied]
				}
				began := time.Now()
				body, err := r.fetch(t, j, psid(0), n)
				took := time.Since(began)
				if err != nil || !isMP4Segment(body) {
					t.Fatalf("segment %d: %d bytes, %v", n, len(body), err)
				}
				t.Logf("time to first segment: %s: %d ms", name, took.Milliseconds())
			})
		}
	}
}
