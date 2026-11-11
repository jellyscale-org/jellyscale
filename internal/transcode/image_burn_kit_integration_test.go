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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-071's checks run a worker's job over a film the library scan has read, as
// production does, so whatever the build keeps about a track's cues (in the
// scan, in the asset store, in the job) reaches the run the way it does for a
// viewer. They read through a meter, which counts what the film's store serves.

// cueFilm is a film with an image track, made with ffmpeg. The cues are on a
// 1920x1080 canvas: Y 100 is a bar in the top half, Y 860 one in the bottom.
type cueFilm struct {
	codec     string // The track's: "copy" (PGS), "dvdsub" or "dvbsub".
	container string // "mkv" (the default) or "ts".
	mpeg2     bool   // MPEG-2 video, as a DVD rip has.
	seconds   int    // At 24000/1001 frames a second, as a Blu-ray's: a keyframe every 2.002 s, so one never falls on a cue's packet.
	noise     string // The video bitrate of a noisy picture ("2M"), so reading it costs bytes; empty is black.
	cues      []testenv.Cue
	sup       []byte // The PGS stream itself, in place of cues.
	beside    bool   // The track is a file beside the film (name.en.sup), not in it.
	noTrack   bool   // The film has no image track at all.
	// remux makes the film again with mkvmerge (in a container, as CI and a
	// developer's machine both have Docker), as a Blu-ray remux is made: ""
	// leaves ffmpeg's file; "default" is mkvmerge's own options, which store the
	// track's blocks compressed and give it cue points; "nocues" writes no Cues;
	// "videocues" writes Cues for the video track only.
	remux string
}

func top(start, end float64) testenv.Cue {
	return testenv.Cue{Start: start, End: end, X: 660, Y: 100, W: 600, H: 120}
}

func bottom(start, end float64) testenv.Cue {
	return testenv.Cue{Start: start, End: end, X: 660, Y: 860, W: 600, H: 120}
}

// write makes the film at dir/name and returns its size in bytes.
func (c cueFilm) write(t *testing.T, dir, name string) int64 {
	t.Helper()
	testenv.FFmpeg(t)
	sup := filepath.Join(t.TempDir(), "track.sup")
	body := c.sup
	if body == nil {
		body = testenv.PGS(1920, 1080, c.cues...)
	}
	if err := os.WriteFile(sup, body, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-v", "error", "-y"}
	if c.noise == "" {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("color=c=black:size=640x360:rate=24000/1001:duration=%d", c.seconds))
	} else {
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=640x360:rate=24000/1001:duration=%d", c.seconds))
	}
	args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", c.seconds))
	maps := []string{"-map", "0:v", "-map", "1:a"}
	if !c.beside && !c.noTrack {
		args = append(args, "-fix_sub_duration", "-i", sup)
		maps = append(maps, "-map", "2", "-c:s", c.codec, "-metadata:s:s:0", "language=eng")
	}
	args = append(args, maps...)
	switch {
	case c.mpeg2:
		args = append(args, "-c:v", "mpeg2video", "-b:v", "2M", "-g", "24")
	case c.noise != "":
		args = append(args, "-vf", "noise=alls=40:allf=t", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", c.noise, "-minrate", c.noise, "-maxrate", c.noise, "-bufsize", c.noise, "-g", "48")
	default:
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-g", "48")
	}
	// Interleaved strictly by time: ffmpeg 6.1 otherwise writes a sparse
	// track's packets well ahead of their video, where no real film has them.
	args = append(args, "-max_interleave_delta", "0")
	audio, ext := "flac", "mkv"
	if c.container == "ts" {
		audio, ext = "aac", "ts"
	}
	path := filepath.Join(dir, name+"."+ext)
	switch {
	case c.remux != "":
		src := filepath.Join(t.TempDir(), "src.mkv")
		run(t, "ffmpeg", append(args, "-c:a", audio, src)...)
		remux(t, src, path, c.remux)
	case c.container == "ts":
		// Through Matroska first: ffmpeg 6.1's mpegts muxer, fed the .sup
		// directly, writes every cue packet at the head of the file, ahead of
		// its video, where no broadcast puts it and where `-ss 0` skips it.
		// Copied from a Matroska film, the packets land near their time.
		src := filepath.Join(t.TempDir(), "src.mkv")
		run(t, "ffmpeg", append(args, "-c:a", audio, src)...)
		run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", src, "-map", "0", "-c", "copy", path)
	default:
		run(t, "ffmpeg", append(args, "-c:a", audio, path)...)
	}
	if c.beside {
		if err := os.WriteFile(filepath.Join(dir, name+".en.sup"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// palette is a run of bytes of the PGS cues testenv.PGS writes, which ffmpeg
// stores as they are and mkvmerge stores compressed.
var palette = []byte{0, 0, 1, 219, 138, 16, 255}

// remux makes src (ffmpeg's film: video 0, audio 1, the image track 2) into dst
// with mkvmerge, as in how, and fails the test unless the result is what a
// remux is: the track's blocks compressed, so that a build which reads the
// packets' bytes where they lie sees what a real film has, and the Cues as
// how says.
func remux(t *testing.T, src, dst, how string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Link(src, filepath.Join(dir, "in.mkv")); err != nil {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "in.mkv"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	options := map[string]string{"default": "", "nocues": "--no-cues", "videocues": "--cues 2:none"}[how]
	// mkvtoolnix is installed in the container each time: no image of it is
	// kept, and the film is made once a test.
	script := "apk add --no-cache mkvtoolnix >/dev/null 2>&1 && mkvmerge -o out.mkv " + options + " in.mkv >/dev/null"
	if out, err := exec.Command("docker", "run", "--rm", "-v", dir+":/w", "-w", "/w", "alpine:3", "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("mkvmerge in a container (a missing dependency fails the test): %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if original, _ := os.ReadFile(src); bytes.Contains(data, palette) && bytes.Contains(original, palette) {
		t.Fatal("mkvmerge stored the image track's blocks as they were: the fixture is not a remux")
	}
}

// meter is the film's store as a worker sees it: it counts the bytes it serves
// (all readers together), serves each reader at rate bytes a second so that
// ffmpeg is waiting for the film and what has been read when a segment
// appears is what the segment needed, and can fail, hold or count reads.
type meter struct {
	store.Store
	rate    int
	read    atomic.Int64
	opens   atomic.Int64
	failing atomic.Int64 // The nth Open from now fails once, with a store error; 0 for none.
	hold    atomic.Bool  // Reads wait until their Open's context ends.
	held    atomic.Int64 // Reads waiting now.
	live    atomic.Int64 // Opens whose body has not been closed.
	// gate holds the reads of the meter whose Open comes first among the meters
	// that share first, until gate is closed; gated says this meter's do.
	gate  chan struct{}
	first *atomic.Bool
	gated atomic.Bool
	// cut is a byte position in the film; before counts the bytes read from
	// below it. Unlike read, it does not depend on how far ffmpeg reads ahead
	// while it encodes (which grows when the machine is busy): read-ahead goes
	// forward past the cut. 0 counts nothing.
	cut    atomic.Int64
	before atomic.Int64
}

func newMeter(dir string, rate int) *meter {
	return &meter{Store: store.NewPosix(dir), rate: rate}
}

// LocalPath hides the file, so that every read goes through Open as it does
// from S3.
func (m *meter) LocalPath(string) (string, bool) { return "", false }

var errStoreDown = errors.New("the store is down")

type meterBody struct {
	io.ReadCloser
	m     *meter
	ctx   context.Context
	gated bool
	pos   int64 // The film's byte position of the next read.
}

func (m *meter) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if m.failing.Load() > 0 && m.failing.Add(-1) == 0 {
		return nil, errStoreDown
	}
	m.opens.Add(1)
	body, err := m.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	m.live.Add(1)
	b := &meterBody{ReadCloser: body, m: m, ctx: ctx, pos: offset}
	if m.gate != nil && m.first.CompareAndSwap(false, true) {
		b.gated = true
		m.gated.Store(true)
	}
	return b, nil
}

func (b *meterBody) Read(p []byte) (int, error) {
	if b.gated {
		select {
		case <-b.m.gate:
			b.gated = false
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	if b.m.hold.Load() {
		b.m.held.Add(1)
		defer b.m.held.Add(-1)
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	n, err := b.ReadCloser.Read(p)
	b.m.read.Add(int64(n))
	if cut := b.m.cut.Load(); b.pos < cut {
		b.m.before.Add(min(int64(n), cut-b.pos))
	}
	b.pos += int64(n)
	if b.m.rate > 0 {
		time.Sleep(time.Duration(n) * time.Second / time.Duration(b.m.rate))
	}
	return n, err
}

func (b *meterBody) Close() error {
	b.m.live.Add(-1)
	return b.ReadCloser.Close()
}

// scannedFilm is a rig whose film the library scan has read, with its image track
// (stream 2) burnt in.
type scannedFilm struct {
	*rig
	media, assets string
	lib           library.Library
	refTimes      [][]float64 // The frame times of each segment of the run from zero.
	waitEnd       bool        // seek waits until the store is quiet, and keeps what the meter served by then in total.
	total         int64
	track         int    // The stream burnt in.
	served        int64  // What the meter served in all by the last seek's first segment.
	fingerprint   string // Of the track, if it is a file beside the film.
}

func newScanned(t *testing.T, media, assets string) *scannedFilm {
	t.Helper()
	return newScannedThrough(t, media, assets, nil)
}

// newScannedThrough scans the film with the library's store as m, so that m
// counts what the scan reads (and what every later scan does); with nil it is
// the plain store.
func newScannedThrough(t *testing.T, media, assets string, m *meter) *scannedFilm {
	t.Helper()
	r := newRig(t, nil)
	if _, err := r.pool.Exec(context.Background(), "DELETE FROM libraries"); err != nil {
		t.Fatal(err)
	}
	libs, err := library.Sync(context.Background(), r.pool, []library.Config{{Name: "cues", Kind: "movies", Media: "file://" + media, Assets: "file://" + assets}})
	if err != nil {
		t.Fatal(err)
	}
	s := &scannedFilm{rig: r, media: media, assets: assets, lib: libs[0]}
	if m != nil {
		s.lib.Media = m
	}
	s.rescan(t)
	return s
}

// rescan is the next scan of the library, and the rig's spec becomes the one a
// viewer's PlaybackInfo would make for the film as it now is.
func (s *scannedFilm) rescan(t *testing.T) {
	t.Helper()
	scanner := &library.Scanner{Pool: s.pool, Log: slog.New(slog.DiscardHandler)}
	if err := scanner.Scan(context.Background(), s.lib); err != nil {
		t.Fatal(err)
	}
	var item, source, etag string
	if err := s.pool.QueryRow(context.Background(), "SELECT item_id::text, id::text, object_etag FROM media_sources").Scan(&item, &source, &etag); err != nil {
		t.Fatalf("the scan stored no film: %v", err)
	}
	s.spec.Item, s.spec.Source, s.spec.ETag = db.CompactID(item), db.CompactID(source), etag
	if s.track == 0 {
		s.track = 2
	}
	s.burnt()
}

// besideTrack makes the rig's track the image file beside the film, as the
// scan lists it.
func (s *scannedFilm) besideTrack(t *testing.T) {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(), "SELECT streams FROM media_sources").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	streams, err := playback.ParseStreams(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		if stream.IsImageSubtitleStream && stream.Fingerprint != "" {
			s.track, s.fingerprint = stream.Index, stream.Fingerprint
			s.burnt()
			return
		}
	}
	t.Fatalf("the scan listed no image file beside the film: %s", raw)
}

// plain makes the spec the film's without the track.
func (s *scannedFilm) plain() {
	s.spec.Subtitle, s.spec.SubtitleFingerprint = nil, ""
	s.hash = s.spec.Hash()
}

func (s *scannedFilm) burnt() {
	index := s.track
	s.spec.Subtitle, s.spec.SubtitleFingerprint = &index, s.fingerprint
	s.hash = s.spec.Hash()
}

// fresh forgets the jobs, the viewers and the segments of the earlier runs.
func (s *scannedFilm) fresh(t *testing.T) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE transcode_jobs, transcode_viewers, transcode_nodes"); err != nil {
		t.Fatal(err)
	}
	s.segments = store.NewPosix(t.TempDir())
}

// worker starts a worker that reads the film through m. It makes no helpers
// and stops 8 seconds past the segment asked for (production: 60). The shorter
// lead could hide a read that goes on in the background of a long job and costs
// the store only after the first segment, which these checks do not count, and
// the 200 ms heartbeat of the rig hides nothing here, because no check waits
// on one.
func (s *scannedFilm) worker(t *testing.T, m *meter) (stop func()) {
	t.Helper()
	return s.rig.worker(t, func(w *Worker) {
		w.Slots, w.NoHelpers, w.Lead = 1, true, 8*time.Second
		w.Media = func(uri string) (store.Store, error) {
			if strings.TrimPrefix(uri, "file://") == s.media {
				return m, nil
			}
			return store.Open(uri)
		}
	})
}

// firstSegment asks for segment n as a viewer who has just seeked there, and
// returns when it is stored, with the bytes m had served by then and the error
// of the request.
func (s *scannedFilm) firstSegment(t *testing.T, j *Jobs, read func() int64, n, viewer int) (int64, error) {
	t.Helper()
	failed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := j.Watch(ctx, s.viewer(psid(viewer)), s.hash, n); err != nil {
			failed <- err
			return
		}
		_, err := j.Segment(ctx, s.spec, s.hash, s.viewer(psid(viewer)), n, n)
		failed <- err
	}()
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case err := <-failed:
			return read(), err
		case <-deadline:
			return read(), fmt.Errorf("segment %d was not stored in two minutes", n)
		case <-time.After(2 * time.Millisecond):
		}
		if body, err := s.segments.Open(context.Background(), SegmentKey(s.hash, n), 0); err == nil {
			body.Close()
			return read(), nil
		}
	}
}

// seek is a run that begins at segment cut, on a worker reading through a new
// meter, and what it read to make its first segment: the bytes from below the
// end of that segment's place in the film (re-reads count again), when the
// segment was stored. The meter's whole count also holds what ffmpeg reads
// ahead while it encodes, which grows when the machine is busy (JS-071's lead
// correction: checks on that count failed under load with nothing wrong), so
// it is kept in s.served and only logged. It then asks for the next count-1
// segments.
func (s *scannedFilm) seek(t *testing.T, cut, count int, burnt bool, rate int) int64 {
	t.Helper()
	s.fresh(t)
	if burnt {
		s.burnt()
	} else {
		s.plain()
	}
	m := newMeter(s.media, rate)
	m.cut.Store(filmPosition(t, s.media, (cut+1)*4)) // Segments are 4 s.
	stop := s.worker(t, m)
	defer stop()
	j := s.jobs(t)
	served, err := s.firstSegment(t, j, m.read.Load, cut, 0)
	if err != nil {
		t.Fatalf("segment %d: %v", cut, err)
	}
	s.served = served
	for n := cut + 1; n < cut+count; n++ {
		if _, err := s.fetchAt(t, j, psid(0), n, cut); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	if s.waitEnd {
		// The job makes the segments it was asked for and a few more, and waits
		// at its lead limit; what is read after that is something else's. Wait
		// until the store has been quiet for two seconds (a hang bound of 2 minutes).
		last, quiet := m.read.Load(), time.Now()
		for time.Since(quiet) < 2*time.Second {
			if time.Since(quiet) > 2*time.Minute {
				t.Fatal("the film is still being read after two minutes")
			}
			time.Sleep(250 * time.Millisecond)
			if now := m.read.Load(); now != last {
				last, quiet = now, time.Now()
			}
		}
		s.total = last
	}
	return m.before.Load()
}

// halvesFrom is the bright pixels by half in each second of the segments
// from..to-1 of the last run, from second from*4 of the film.
func (s *scannedFilm) halvesFrom(t *testing.T, from, to int) [][2]int {
	t.Helper()
	return brightByHalf(t, s.storedFrom(t, from, to))
}

// fromZero is the picture a run from zero draws, every second of the film, and
// keeps the times of its segments' frames in refTimes.
func (s *scannedFilm) fromZero(t *testing.T, seconds int) [][2]int {
	t.Helper()
	s.fresh(t)
	s.burnt()
	stop := s.rig.worker(t, func(w *Worker) {
		w.Slots, w.NoHelpers = 1, true
		w.Media = func(uri string) (store.Store, error) { return store.Open(uri) }
	})
	defer stop()
	segments := (seconds + 3) / 4
	s.playThrough(t, s.jobs(t), psid(0), 0, segments)
	s.refTimes = nil
	for n := 0; n < segments; n++ {
		s.refTimes = append(s.refTimes, video(t, s.segments, s.hash, n))
	}
	return s.halvesFrom(t, 0, segments)
}

// picture says which bars a second shows: "", "top", "bottom" or "both".
func picture(half [2]int) string {
	switch {
	case half[0] >= 20 && half[1] >= 20:
		return "both"
	case half[0] >= 20 && half[1] < 20:
		return "top"
	case half[1] >= 20 && half[0] < 20:
		return "bottom"
	}
	return ""
}

// expectPicture fails the test unless the film's second at shows want.
func expectPicture(t *testing.T, what string, halves [][2]int, at int, want string) {
	t.Helper()
	if at >= len(halves) {
		t.Fatalf("%s: %d seconds, want second %d", what, len(halves), at)
	}
	if got := picture(halves[at]); got != want {
		t.Errorf("%s, second %d: bright pixels top %d, bottom %d, which is %q; want %q", what, at, halves[at][0], halves[at][1], got, want)
	}
}

// likeReference fails the test for each second of a run from segment cut that
// shows other than the run from zero shows there, except seconds that are
// within one of a cue's edge, where the frame the second falls on may differ
// by a frame between encodes.
func likeReference(t *testing.T, what string, reference, run [][2]int, cut int, edges ...float64) {
	t.Helper()
	if len(run) < 3 {
		t.Fatalf("%s: the run has %d seconds", what, len(run))
	}
	for i, half := range run {
		at := cut*4 + i
		if at >= len(reference) {
			break
		}
		near := false
		for _, edge := range edges {
			near = near || (float64(at) > edge-1.5 && float64(at) < edge+1.5)
		}
		if near {
			continue
		}
		if got, want := picture(half), picture(reference[at]); got != want {
			t.Errorf("%s, second %d: shows %q (bright pixels top %d, bottom %d); a run from zero shows %q there", what, at, got, half[0], half[1], want)
		}
	}
}

// spread is the largest minus the smallest of the readings.
func spread(readings ...int64) (low, high, width int64) {
	low, high = readings[0], readings[0]
	for _, r := range readings {
		low, high = min(low, r), max(high, r)
	}
	return low, high, high - low
}

func itoa(n int) string { return fmt.Sprint(n) }

// storedFrom joins the init segment and segments from..to-1 as the store has
// them, as the player sees them.
func (r *rig) storedFrom(t *testing.T, from, to int) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, n int) string {
		body, err := readSegment(context.Background(), r.segments, SegmentKey(r.hash, n))
		if err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var files []string
	for n := from; n < to; n++ {
		files = append(files, write(fmt.Sprintf("%d.m4s", n), n))
	}
	return join(t, write("init.mp4", -1), files...)
}
