//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-081: checks of what the code does where a test used to time it. A test
// that passes or fails on how long something took passes on a developer's
// machine and fails on a shared runner (JS-067, JS-024, JS-072, JS-071), so
// these look at which worker made which segment, how many jobs ran at once,
// which ffmpeg arguments were given, and what the database recorded. Every wait
// in them is for an event, with a timeout that only stops a hang. They pass
// with the test and its workers held to two CPUs (taskset -c 0,1).

// madeBy is a row of the spec's jobs with the helper flag, which job does not
// read.
type madeBy struct {
	ID          int64
	Worker      string
	Start, Next int
	State       string
	Helper      bool
}

func (r *rig) rows(t *testing.T) []madeBy {
	t.Helper()
	found, err := r.pool.Query(context.Background(), `SELECT id, COALESCE(worker, ''), start_segment, next_segment, state, helper
		FROM transcode_jobs WHERE spec_hash = $1 ORDER BY id`, r.hash)
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	var rows []madeBy
	for found.Next() {
		var row madeBy
		if err := found.Scan(&row.ID, &row.Worker, &row.Start, &row.Next, &row.State, &row.Helper); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

// madeTwice lists the segments that more than one job of the spec made.
func madeTwice(rows []madeBy) (twice []string) {
	made := make([]int, filmSegments)
	for _, row := range rows {
		for n := row.Start; n < min(row.Next, filmSegments); n++ {
			made[n]++
		}
	}
	for n, times := range made {
		if times > 1 {
			twice = append(twice, fmt.Sprintf("segment %d by %d jobs", n, times))
		}
	}
	return twice
}

// peakWorkers samples the spec's running jobs until stop, which returns the
// most workers that had a job running at one moment.
func (r *rig) peakWorkers(t *testing.T) (stop func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var peak atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			var n int
			err := r.pool.QueryRow(ctx, "SELECT count(DISTINCT worker) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running'", r.hash).Scan(&n)
			if err == nil && int64(n) > peak.Load() {
				peak.Store(int64(n))
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	stop = func() int { cancel(); <-done; return int(peak.Load()) }
	t.Cleanup(func() { stop() })
	return stop
}

// playToTheEnd is a viewer who plays the whole film from the start in the
// background; the error it ends with (nil if no request failed) arrives on the
// channel.
func (r *rig) playToTheEnd(t *testing.T, j *Jobs, psid string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, err := r.play(ctx, j, psid, 0, filmSegments)
		ended <- err
	}()
	t.Cleanup(func() { cancel(); <-done })
	return ended
}

// theViewerPlaysOn waits for the viewer's film to end; a hang is the only
// reason it would not. A request that failed fails the test.
func theViewerPlaysOn(t *testing.T, ended <-chan error, rows func() []madeBy) {
	t.Helper()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("a request of the viewer failed: %v (jobs: %+v)", err, rows())
		}
	case <-time.After(5 * time.Minute):
		t.Fatalf("the viewer never got the whole film (jobs: %+v)", rows())
	}
}

// Workers sharing a slow film (a worker reads at the speed of the film, so a
// job gets helpers): with two workers and with three, every worker made
// segments, no segment was made by two jobs, and jobs on different workers ran
// at the same moment. It fails when no helper starts, and when the chunks are
// made one after another.
func TestWorkersSharingASlowFilmAllMakeSegmentsAtTheSameTime(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d workers", n), func(t *testing.T) {
			slow(t)
			r := newRig(t, nil)
			r.workers(t, n, 2, 6, r.ownStore(t))
			j := r.jobs(t)
			peak := r.peakWorkers(t)
			ended := r.playToTheEnd(t, j, psid(0))
			theViewerPlaysOn(t, ended, func() []madeBy { return r.rows(t) })
			most := peak()
			rows := r.rows(t)

			makers := map[string]bool{}
			for _, row := range rows {
				if row.Next > row.Start {
					makers[row.Worker] = true
				}
			}
			if len(makers) != n {
				t.Errorf("%d workers made segments, want %d: %+v", len(makers), n, rows)
			}
			if twice := madeTwice(rows); len(twice) > 0 {
				t.Errorf("segments made twice: %v: %+v", twice, rows)
			}
			if most < n {
				t.Errorf("at most %d workers had a job running at one moment, want %d: the chunks were made one after another (%+v)", most, n, rows)
			}
		})
	}
}

// A helper that dies mid-chunk is replaced by a job on another worker that
// begins at the first segment of the chunk that is missing, and the viewer's
// requests all succeed.
func TestAKilledHelpersChunkIsMadeByAReplacementOnAnotherWorkerFromTheFirstMissingSegment(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	set := r.workers(t, 3, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	ended := r.playToTheEnd(t, j, psid(0))

	var victim madeBy
	waitFor(t, "a helper to be encoding a later chunk", 60*time.Second, func() bool {
		for _, row := range r.rows(t) {
			if row.Helper && row.State == "running" && row.Next > row.Start {
				victim = row
				return true
			}
		}
		return false
	})
	set.kill(victim.Worker)
	theViewerPlaysOn(t, ended, func() []madeBy { return r.rows(t) })

	rows := r.rows(t)
	var dead madeBy
	for _, row := range rows {
		if row.ID == victim.ID {
			dead = row
		}
	}
	chunkEnd := (victim.Start/6 + 1) * 6
	var replacement *madeBy
	for _, row := range rows {
		if row.ID > victim.ID && row.Worker != victim.Worker && row.Start >= victim.Start && row.Start < chunkEnd && row.Next > row.Start {
			replacement = &row
			break
		}
	}
	if replacement == nil {
		t.Fatalf("no job on another worker made segments of the dead helper's chunk %d to %d: %+v", victim.Start, chunkEnd-1, rows)
	}
	if replacement.Start < dead.Next {
		t.Errorf("the replacement began at segment %d, but the dead helper had made up to %d: it made them again", replacement.Start, dead.Next-1)
	}
	if twice := madeTwice(rows); len(twice) > 0 {
		t.Errorf("segments made twice: %v: %+v", twice, rows)
	}
	for n := victim.Start; n < chunkEnd && n < filmSegments; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d of the dead helper's chunk was never made", n)
		}
	}
}

// A helper stopped for another viewer leaves its chunk to the jobs already
// making the film, not to a new helper: every segment of it is made by
// another job, none twice, and the viewer's requests all succeed.
func TestAPreemptedHelpersChunkIsMadeByTheJobThatAskedForIt(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 2, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	ended := r.playToTheEnd(t, j, psid(0))
	waitFor(t, "two helpers to run beside the main job", 60*time.Second, func() bool {
		return len(running(r.jobsOf(t, r.hash))) == 3 && r.helperRows(t, "running") == 2
	})
	for i, bitrate := range []int64{800_000, 900_000} {
		spec, hash := r.otherStream(bitrate)
		if _, err := r.firstSegment(j, spec, hash, psid(10+i)); err != nil {
			t.Fatalf("new viewer %d: %v", i, err)
		}
	}
	theViewerPlaysOn(t, ended, func() []madeBy { return r.rows(t) })

	rows := r.rows(t)
	stopped := 0
	for _, row := range rows {
		if !row.Helper || row.State != "cancelled" {
			continue
		}
		stopped++
		chunkEnd := min((row.Start/6+1)*6, filmSegments)
		// The chunk is left to the jobs already making the film: no new helper
		// starts in it, and each of its segments is made by another job. Which
		// one depends on timing (the job before the chunk goes on into it, a
		// helper or not, or the viewer's next request starts a job there), so
		// only that is checked (JS-081's lead correction: requiring one job to
		// span the chunk, or a non-helper to make it, failed a right build).
		for _, other := range rows {
			if other.ID > row.ID && other.Helper && other.Start >= row.Start && other.Start < chunkEnd {
				t.Errorf("helper job %d started at segment %d in the stopped helper's chunk %d to %d: %+v", other.ID, other.Start, row.Start, chunkEnd-1, rows)
			}
		}
		for n := row.Start; n < chunkEnd; n++ {
			made := false
			for _, other := range rows {
				if other.ID != row.ID && other.Start <= n && n < other.Next {
					made = true
				}
			}
			if !made {
				t.Errorf("segment %d of the stopped helper's chunk %d to %d was made by no other job: %+v", n, row.Start, chunkEnd-1, rows)
			}
		}
	}
	if stopped == 0 {
		t.Fatalf("no helper was stopped for the newcomers, so there is nothing to show: %+v", rows)
	}
	if twice := madeTwice(rows); len(twice) > 0 {
		t.Errorf("segments made twice: %v: %+v", twice, rows)
	}
	for n := 0; n < filmSegments; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d was never made", n)
		}
	}
}

// A seek into a helper's chunk is served by the helper itself: no job starts in
// the chunk, the segment asked for is one the same helper job records having
// made, and the request succeeds.
func TestASeekIntoAHelpersChunkIsMadeByTheHelperItselfAndStartsNoJob(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	var helper madeBy
	waitFor(t, "a helper to make its first segment", 60*time.Second, func() bool {
		for _, row := range r.rows(t) {
			if row.Helper && row.State == "running" && row.Next > row.Start {
				helper = row
				return true
			}
		}
		return false
	})
	target := helper.Next + 1
	if target >= (helper.Start/6+1)*6 {
		t.Fatalf("the helper had already made %d..%d of its chunk by the time the test looked: no segment is left to seek to inside it", helper.Start, helper.Next-1)
	}
	before := len(r.rows(t))
	if _, err := r.fetch(t, j, psid(0), target); err != nil {
		t.Fatalf("the seek into the helper's chunk failed: %v", err)
	}
	waitFor(t, "the helper's row to record the segment", 30*time.Second, func() bool {
		for _, row := range r.rows(t) {
			if row.ID == helper.ID {
				return row.Next > target
			}
		}
		return false
	})
	for _, started := range r.rows(t)[before:] {
		if started.Start >= helper.Start && started.Start <= target {
			t.Errorf("a seek inside a running helper's chunk started a job at segment %d: %+v", started.Start, r.rows(t))
		}
	}
	for _, row := range r.rows(t) {
		if row.ID == helper.ID && (row.Worker != helper.Worker || row.State != "running") {
			t.Errorf("the helper changed hands or ended during the seek: %+v", row)
		}
	}
}

// A killed worker's job is claimed by the other worker before the dead
// worker's own lease would have run out (the viewer's wait does not include the
// lease), and the viewer's requests are all answered. The dead worker's lease
// is 30 s here, three times production's, so a build that waits it out claims
// the job after the lease ends, whatever the machine.
func TestAKilledWorkersJobIsClaimedBeforeItsLeaseRunsOut(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	slowAt(t, "2")
	const lease = "JELLYSCALE_TEST_LEASE=30s"
	a := r.workerProcess(t, "worker-a", segments, lease)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if got := r.owner(t); got != "worker-a" {
		t.Fatalf("the job is worker-a's, not %q", got)
	}
	r.workerProcess(t, "worker-b", segments, lease)
	owned := r.watchOwners(t)
	for n := 1; n <= 2; n++ {
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	var first int64
	var oldLease time.Time
	if err := r.pool.QueryRow(context.Background(), "SELECT id, lease_expires_at FROM transcode_jobs WHERE state = 'running' AND worker = 'worker-a'").Scan(&first, &oldLease); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := r.fetch(t, j, psid(0), 3); waited <- err }() // The viewer waits on a segment the dead worker was making.
	a.Process.Kill()                                                   //nolint:errcheck
	waitFor(t, "worker-b to claim the job while the dead worker's lease was still running", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id > $1 AND worker = 'worker-b' AND state = 'running' AND now() < $2", first, oldLease) == 1
	})
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("the viewer's request failed: %v", err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the viewer's request was never answered")
	}
	for n := 4; n <= 7; n++ {
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	owned.check(t)
}

// A subtitle asked for while chapter pictures fill their slots is read at once:
// when it is done, as many chapter jobs as there are slots are still running and
// none has finished since it was asked for. The chapter jobs' ffmpeg waits for a
// file the test writes, so nothing finishes by itself on a slow machine.
func TestASubtitleIsReadWhileChapterPicturesStillFillTheirSlots(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	const films = 5
	for i := range films {
		id := uuid.NewString()
		r.exec(t, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto)
			SELECT $1, library_id, 'Movie', $2, $2, $2, '{}' FROM items LIMIT 1`, id, fmt.Sprint("Chaptered ", i))
		r.exec(t, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, chapters)
			VALUES ($1, $1, 'film.mkv', 1, $2, 'mkv', '[{"Type":"Video"}]', '[{"StartPositionTicks":0,"Name":"a"}]')`, id, fmt.Sprint("etag-", i))
	}
	r.exec(t, "UPDATE libraries SET chapter_images = true")

	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *image2pipe*) while [ ! -e %q ]; do sleep 0.1; done;; esac\nexec %q \"$@\"\n", gate, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	open := func() {
		if err := os.WriteFile(gate, nil, 0o644); err != nil && !errors.Is(err, os.ErrExist) {
			t.Error(err)
		}
	}
	t.Cleanup(open)
	r.worker(t, nil)
	j := r.jobs(t)

	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	running := func() int {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'running'")
	}
	finished := func() int {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state IN ('done', 'failed', 'cancelled')")
	}
	waitFor(t, "chapter jobs to fill their slots", 30*time.Second, func() bool { return running() == chapterSlots })
	finishedBefore := finished()

	asked := make(chan error, 1)
	go func() {
		_, err := FetchSubtitle(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2)
		asked <- err
	}()
	select {
	case err := <-asked:
		if err != nil {
			t.Fatalf("the subtitle was not read while chapter pictures held the slots: %v", err)
		}
	case <-time.After(time.Minute):
		open()
		t.Fatal("the subtitle was not read while chapter pictures held the slots")
	}
	if n := running(); n != chapterSlots {
		t.Errorf("%d chapter jobs running when the subtitle was read, want %d", n, chapterSlots)
	}
	if n := finished(); n != finishedBefore {
		t.Errorf("%d chapter jobs finished while the subtitle was being read (%d before), so it did not go past them", n, finishedBefore)
	}

	open()
	most := 0
	waitFor(t, "every chapter job to end", 2*time.Minute, func() bool {
		most = max(most, running())
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state IN ('queued', 'running')") == 0
	})
	if most > chapterSlots {
		t.Errorf("%d chapter jobs ran at once, the slots are %d", most, chapterSlots)
	}
}

// The speed a worker shows for a job (jellyscale_transcode_job_speed_ratio) is
// the one it recorded for the job in the database, and a pause at the lead does
// not lower it: it is the same after the pause as when the job paused.
func TestAWorkersShownSpeedIsTheOneItRecordedAndHoldsAcrossAPause(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(worker *Worker) {
		w = worker
		worker.Lead = 12 * time.Second
		worker.Idle = 30 * time.Second
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	waitFor(t, "the job to pause at its lead", 60*time.Second, func() bool { return r.paused() })

	shown := func() float64 {
		match := speedLine.FindStringSubmatch(scrape(t, w))
		if match == nil {
			t.Fatal("the paused job shows no speed")
		}
		speed, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return speed
	}
	recorded := func() float64 {
		var speed *float64
		r.pool.QueryRow(context.Background(), "SELECT encode_speed FROM transcode_jobs WHERE state = 'running'").Scan(&speed) //nolint:errcheck
		if speed == nil {
			return -1
		}
		return *speed
	}
	same := func(a, b float64) bool { return a >= 0 && b >= 0 && a-b <= 1e-9*max(a, b) && b-a <= 1e-9*max(a, b) }
	// What the worker recorded is what the last heartbeat before the pause
	// wrote, which can be a beat older than the speed it holds, so the two agree
	// closely and not to the digit.
	var before float64
	waitFor(t, "the worker to have recorded a speed for the job", 30*time.Second, func() bool { return recorded() >= 0 })
	before = shown()
	if recorded := recorded(); before < recorded*0.75 || before > recorded*1.25 {
		t.Errorf("shown speed %v, the worker recorded %v for the job", before, recorded)
	}
	time.Sleep(6 * time.Second) // Idle at the lead, as the job is until the viewer moves on.
	if !r.paused() {
		t.Fatal("the job did not stay paused")
	}
	if after := shown(); after < before && !same(after, before) {
		t.Errorf("the job's speed fell from %v to %v while it was paused: pausing counted as slow encoding", before, after)
	}
}

// A 4K HDR10 film with TrueHD is copied without decoding or encoding a frame of
// its picture, and ffmpeg is given the arguments that keep the TrueHD's decoding
// off the video's path: the sound is its own input, not mapped from the input
// the picture is copied from.
func TestCopyOf4KHDR10WithTrueHDDecodesNoVideoAndTakesTheSoundFromItsOwnInput(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "film.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=8",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=8",
		"-map", "0:v", "-map", "1:a", "-pix_fmt", "yuv420p10le",
		"-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "keyint=48:log-level=error:colorprim=bt2020:colormatrix=bt2020nc:transfer=smpte2084:hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400",
		"-ac", "6", "-c:a", "truehd", "-strict", "-2", input)
	source := scanned(t, input, "mkv")
	if audio := source.Streams[1]; audio.Codec != "truehd" || audio.Channels != 6 {
		t.Fatalf("the source's sound is %s with %d channels, so this test proves nothing", audio.Codec, audio.Channels)
	}
	if !source.CanCopy() {
		t.Fatal("CanCopy is false for 4K HDR10")
	}
	layout := source.Layout(true)
	video, audioStart := copyStart(source.Keyframes, layout, 0)
	command, err := CopyCommand(context.Background(), input, copySpec, source.VideoCodec(), video, audioStart, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args := command.Args[1:]

	// The arguments: the picture is copied from the first input, and the TrueHD
	// is mapped from another, so its decoder is not in the picture's path.
	inputs := 0
	maps := map[string]string{} // output stream index to the input stream mapped
	for i, arg := range args {
		switch {
		case arg == "-i":
			inputs++
		case arg == "-map" && i+1 < len(args):
			maps[strconv.Itoa(len(maps))] = args[i+1]
		}
	}
	if inputs != 2 {
		t.Errorf("ffmpeg was given %d inputs, want 2 (the picture's and the sound's): %v", inputs, args)
	}
	pictureInput, _, _ := strings.Cut(maps["0"], ":")
	soundInput, _, _ := strings.Cut(maps["1"], ":")
	if pictureInput != "0" || soundInput == "" || soundInput == pictureInput {
		t.Errorf("the picture is mapped from input %q and the TrueHD from input %q, want different inputs: %v", pictureInput, soundInput, args)
	}
	if !strings.Contains(strings.Join(args, " "), "-c:v copy") {
		t.Errorf("the picture is not copied: %v", args)
	}

	// The run: ffmpeg's own account of what it does with each stream.
	loud := slicesWithLogLevel(args, "info")
	out, err := exec.Command("taskset", append([]string{"-c", twoCPUs(t), "ffmpeg"}, loud...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	var mapping []string
	inMapping := false
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "Stream mapping:"):
			inMapping = true
		case inMapping && strings.HasPrefix(line, "  Stream #"):
			mapping = append(mapping, strings.TrimSpace(line))
		default:
			inMapping = false
		}
	}
	if len(mapping) != 2 {
		t.Fatalf("ffmpeg's stream mapping has %d lines, want the picture and the sound:\n%s", len(mapping), out)
	}
	if !strings.HasSuffix(mapping[0], "(copy)") {
		t.Errorf("the picture is not a stream copy, so frames are decoded and encoded: %s", mapping[0])
	}
	if !strings.Contains(mapping[1], "truehd") || !strings.Contains(mapping[1], "aac") {
		t.Errorf("the sound is not the TrueHD decoded and encoded as AAC: %s", mapping[1])
	}
}

// slicesWithLogLevel is args with "-v error" replaced by the level.
func slicesWithLogLevel(args []string, level string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-v" {
			out[i+1] = level
		}
	}
	return out
}

// twoCPUs is up to two CPUs this process may run on, as the small worker the
// copy is meant for has. A machine with fewer is not a failure: the run then
// takes the one it has.
func twoCPUs(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("taskset", "-pc", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	list := strings.TrimSpace(string(out[strings.LastIndex(string(out), ":")+1:]))
	var cpus []string
	for _, part := range strings.Split(list, ",") {
		lo, hi, found := strings.Cut(part, "-")
		if !found {
			hi = lo
		}
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil {
			t.Fatalf("taskset affinity list %q", list)
		}
		for cpu := a; cpu <= b && len(cpus) < 2; cpu++ {
			cpus = append(cpus, strconv.Itoa(cpu))
		}
	}
	return strings.Join(cpus, ",")
}
