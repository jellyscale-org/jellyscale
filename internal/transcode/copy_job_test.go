package transcode

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

// copied makes the rig's film a copied stream: its keyframes, read from the
// file as a scan does, are in the database, and the spec says copy.
func (r *rig) copied(t *testing.T, name string) {
	t.Helper()
	r.useFile(t, name)
	info, err := os.Stat(filepath.Join(film(t), name))
	if err != nil {
		t.Fatal(err)
	}
	keyframes, err := library.ReadKeyframes(context.Background(), store.NewPosix(film(t)), name, info.Size(), strings.TrimPrefix(filepath.Ext(name), "."))
	if err != nil {
		t.Fatal(err)
	}
	end, err := library.ReadVideoEnd(context.Background(), filepath.Join(film(t), name), keyframes[len(keyframes)-1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = $1, video_end = $2, size = $3", keyframes, end, info.Size()); err != nil {
		t.Fatal(err)
	}
	r.spec.Copy = true
	r.spec.VideoBitrate = 0
	r.hash = r.spec.Hash()
}

// Any segment of a copied stream can be asked for first, and a job starts there
// with no wait for the ones before it; the init is the same wherever it came from.
func TestCopiedStreamServesFromAnySegment(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv") // 90 s with a keyframe every 10 s: 9 segments.
	var logs logBuffer
	r.worker(t, func(w *Worker) { w.Log = logs.logger() })
	j := r.jobs(t)
	var inits [][]byte
	for _, n := range []int{5, 0, 8} {
		body, err := r.fetch(t, j, psid(n), n)
		if err != nil || !isMP4Segment(body) {
			t.Fatalf("segment %d: %d bytes, %v", n, len(body), err)
		}
		init, err := r.fetchAt(t, j, psid(n), -1, n)
		if err != nil {
			t.Fatal(err)
		}
		inits = append(inits, init)
	}
	for _, init := range inits[1:] {
		if !bytes.Equal(init, inits[0]) {
			t.Error("init segments differ between jobs")
		}
	}
	waitFor(t, "the jobs to end", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
	if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
		t.Fatalf("%d jobs failed", failed)
	}
	if !strings.Contains(logs.String(), "transcode started") || !strings.Contains(logs.String(), "copy=true") {
		t.Errorf("the log does not say the job is a copy:\n%s", logs.String())
	}
	for n := range 9 { // A keyframe every 10 s in 90 s.
		if _, err := r.segments.Open(context.Background(), SegmentKey(r.hash, n), 0); err != nil {
			t.Errorf("segment %d was not stored: %v", n, err)
		}
	}
	// The last segment is the film's last: nothing past it.
	if body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, 9), 0); err == nil {
		body.Close()
		t.Error("a segment past the last was stored")
	}
}

// A copied job whose input ends early, with the last segment cut or missing
// altogether, fails with the reason rather than store a short segment.
func TestCopiedInputThatEndsEarlyFailsTheJob(t *testing.T) {
	for name, keep := range map[string]float64{"in the middle of a segment": 0.55, "before the last segment": 0.7} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			// The keyframes are read from the whole file, as the scan did before it was cut.
			cut := "cut-" + strings.ReplaceAll(name, " ", "-") + ".mkv"
			if err := os.WriteFile(filepath.Join(film(t), cut), whole, 0o644); err != nil {
				t.Fatal(err)
			}
			r.copied(t, cut)
			if err := os.WriteFile(filepath.Join(film(t), cut), whole[:int(float64(len(whole))*keep)], 0o644); err != nil {
				t.Fatal(err)
			}
			r.worker(t, nil)
			if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
				t.Fatalf("segment 0 is whole and should be served: %v", err)
			}
			waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
			if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); !strings.Contains(reason, "input ended early") {
				t.Fatalf("the reason is %q", reason)
			}
			// The file is what its index says; it was cut short, which is no
			// reason to doubt the index.
			if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
				t.Fatalf("the index has %d keyframes after an input that ended early, want 9", n)
			}
			next := r.count(t, "SELECT next_segment FROM transcode_jobs")
			if body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, next), 0); err == nil {
				body.Close()
				t.Fatalf("segment %d, cut short by the end of the input, was stored", next)
			}
		})
	}
}

// Every way of ending a copied job ends ffmpeg, as for an encode.
func TestCopiedKillPathsEndFFmpeg(t *testing.T) {
	slow(t)
	for name, end := range map[string]func(t *testing.T, r *rig, j *Jobs){
		"stop from the player": func(t *testing.T, r *rig, j *Jobs) {
			if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
				t.Fatal(err)
			}
		},
		"DELETE ActiveEncodings, sent twice": func(t *testing.T, r *rig, j *Jobs) {
			for range 2 {
				if err := j.Stop(context.Background(), r.user, "", "device", ""); err != nil {
					t.Fatal(err)
				}
			}
		},
		"a closed tab sends nothing": func(*testing.T, *rig, *Jobs) {},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.copied(t, "film.mkv")
			r.worker(t, func(w *Worker) {
				if strings.HasPrefix(name, "a closed") {
					w.Idle = 2 * time.Second
				}
			})
			j := r.jobs(t)
			if _, err := r.fetch(t, j, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			procs := r.ffmpegProcesses()
			if len(procs) != 1 || !strings.Contains(procs[0], "-c:v copy") {
				t.Fatalf("expected one ffmpeg copying the video: %v", procs)
			}
			end(t, r, j)
			waitFor(t, "ffmpeg to exit", 15*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
			// Stopping a copy says nothing of the index.
			if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
				t.Errorf("the index has %d keyframes after the job was stopped, want 9", n)
			}
		})
	}
}

// Cues and stss are seek indexes: they may not match the fragments ffmpeg cuts.
// A segment that doesn't start on a keyframe of the file fails the job, once,
// and the source's index is dropped so that the next PlaybackInfo re-encodes.
func TestCopiedStreamWithAWrongIndexFailsAndIsReEncoded(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	// Every keyframe but the first, a second late: not where the file has them.
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = ARRAY(SELECT CASE WHEN n = 1 THEN k ELSE k + 10000000 END FROM unnest(keyframes) WITH ORDINALITY AS t(k, n) ORDER BY n)"); err != nil {
		t.Fatal(err)
	}
	var logs logBuffer
	r.worker(t, func(w *Worker) { w.Log = logs.logger() })
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err == nil {
		t.Fatal("a segment was served from a wrong index")
	}
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); !strings.Contains(reason, "keyframe index is wrong") {
		t.Fatalf("the reason is %q", reason)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 0 {
		t.Fatalf("the index has %d keyframes after it was found wrong, want none", n)
	}
	source := mustScalar(t, r, "SELECT id::text FROM media_sources")
	if log := logs.String(); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "keyframe index is wrong") || !strings.Contains(log, "item="+r.spec.Item) || !strings.Contains(log, "source="+r.spec.Source) {
		t.Errorf("the log names neither the item nor the source:\n%s", log)
	}
	// What the next PlaybackInfo decides from: no longer a copy.
	keyframes, end, err := r.items.Keyframes(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if (playback.Source{Keyframes: keyframes, VideoEnd: end, Streams: []playback.Stream{{Type: "Video", Codec: "h264", Width: 1280, Height: 720}}}).CanCopy() {
		t.Error("the source can still be copied")
	}
}

// An MP4 made for streaming, cut at the byte its last keyframe starts: the
// index still lists that keyframe, but ffmpeg has no fragment in the last
// segment. That is an input cut short, which says nothing of the index.
func TestCopiedMP4CutAtTheLastKeyframeKeepsTheIndex(t *testing.T) {
	dir := film(t)
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", filepath.Join(dir, "film.mkv"), "-c", "copy", "-movflags", "+faststart", filepath.Join(dir, "faststart.mp4"))
	cutAtTheLastKeyframe(t, "faststart.mp4", 9)
}

// The last group of frames is under 1.5 s, as a film's can be.
func TestCopiedMP4CutAtAShortLastGroupKeepsTheIndex(t *testing.T) {
	dir := film(t)
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "240", "-force_key_frames", "10,20,28.8", "-c:a", "aac",
		"-movflags", "+faststart", filepath.Join(dir, "short-last.mp4"))
	cutAtTheLastKeyframe(t, "short-last.mp4", 4)
}

func cutAtTheLastKeyframe(t *testing.T, name string, keyframes int) {
	t.Helper()
	r := newRig(t, nil)
	dir := film(t)
	r.copied(t, name)
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pos,flags", "-of", "csv=p=0", filepath.Join(dir, name)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var cutAt int64 = -1
	for _, line := range strings.Fields(string(out)) {
		if pos, flags, _ := strings.Cut(line, ","); strings.HasPrefix(flags, "K") {
			cutAt, _ = strconv.ParseInt(pos, 10, 64)
		}
	}
	whole, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || cutAt <= 0 || cutAt >= int64(len(whole)) {
		t.Fatalf("cut at %d of %d: %v", cutAt, len(whole), err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), whole[:cutAt], 0o644); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	r.fetch(t, r.jobs(t), psid(0), 0) //nolint:errcheck
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	// ffmpeg 9.0 reaches the cut and the gatherer says the input ended early;
	// 6.1 fails sooner, with a missing fragment. Either way the job fails and
	// nothing says the index is wrong.
	if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); strings.Contains(reason, "keyframe index is wrong") {
		t.Fatalf("the reason is %q", reason)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != keyframes {
		t.Fatalf("the index has %d keyframes after a cut input, want %d", n, keyframes)
	}
}

// A last index entry the file doesn't have is a wrong index, though the input
// is whole: ffmpeg's output reaches the video's end and no segment ends there.
func TestCopiedStreamWithAFalseLastKeyframeIsDropped(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	mustExec := func(sql string) {
		if _, err := r.pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	// The last keyframe, at 80 s, moved to 85 s.
	mustExec("UPDATE media_sources SET keyframes = keyframes[1:8] || (keyframes[9] + 50000000)")
	r.worker(t, nil)
	r.fetch(t, r.jobs(t), psid(0), 0) //nolint:errcheck
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 0 {
		t.Fatalf("the index has %d keyframes after a false last entry, want none", n)
	}
}

// The same, for a job that begins in the last segment: ffmpeg's last fragment
// ends a little before the video does, and that is still the end.
func TestCopiedStreamWithAFalseLastKeyframeIsDroppedFromTheLastSegment(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = keyframes[1:8] || (keyframes[9] + 50000000)"); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	r.fetch(t, r.jobs(t), psid(8), 8) //nolint:errcheck
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); !strings.Contains(reason, "keyframe index is wrong") {
		t.Fatalf("the reason is %q", reason)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 0 {
		t.Fatalf("the index has %d keyframes after a false last entry, want none", n)
	}
}

// A file rescanned while a job ran has a new index, which a wrong one found
// by the job must not drop.
func TestDropKeyframesLeavesARescannedFile(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	source := mustScalar(t, r, "SELECT id::text FROM media_sources")
	etag := mustScalar(t, r, "SELECT object_etag FROM media_sources")
	if dropped, err := r.items.DropKeyframes(context.Background(), source, "an-older-etag"); err != nil || dropped {
		t.Fatalf("a drop for another version of the file reported %v, %v", dropped, err)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
		t.Fatalf("the index has %d keyframes after a drop for another version of the file, want 9", n)
	}
	if dropped, err := r.items.DropKeyframes(context.Background(), source, etag); err != nil || !dropped {
		t.Fatalf("a drop for this version reported %v, %v", dropped, err)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 0 {
		t.Fatalf("the index has %d keyframes after a drop for this version, want none", n)
	}
}
