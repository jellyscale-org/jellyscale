//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-152's checks. A seek far into a re-encode gets its segment, whatever the film's audio. The kit
// here makes films of more than 30 minutes with each audio the dogfood's library holds, and a seek past
// half of one, and compares what the seek made with what a run from the film's start makes.

const (
	// A film of 30 minutes and 10 seconds: 453 segments of 4 seconds, the last of 2.
	seekFilmSeconds = 30*60 + 10
	// 908 seconds in: past half the film.
	seekSegment = 227
	// The picture is small: the checks are about the audio and where the segments fall, and a film
	// of this length at the size of a real one takes minutes to encode.
	seekWidth, seekHeight = 160, 90
)

// seekAudio is one audio track of the matrix, and what the film's probe must say it is: a DTS core
// track is never what stands in for DTS-HD MA.
type seekAudio struct {
	name          string
	codec         string   // ffprobe's codec_name
	profile       string   // ffprobe's profile when the row names one ("" says nothing)
	channels      int      // ffprobe's channels
	encode        []string // ffmpeg's arguments to make it from a tone
	vector        string   // or a DTS-HD MA stream in test/fixtures/dtshd, which no ffmpeg encoder makes
	vectorSeconds float64  // and how long that stream is
	// damagedHead: 60 bytes of the stream's first frame, from its 100th, are zero (its core is damaged and the frame is not
	// decoded, the container's header and the probe of the file still say 5.1), as a DTS-HD MA track whose first frame has lost its core is, and
	// the track starts 4 ms after the film (192 samples at 48 kHz), the dogfood's start_time. The decoder loses its first frames
	// (the first decoded one falls at 720 samples, not 192), and the probe that finds where a run from the start puts its AAC
	// packets (-frames:a 1, see audioGrid) makes no packet, where -frames:a 2 makes one before zero. A track of the same stream
	// that is whole makes its packet (the row without the flag), so this is the property and not DTS-HD MA's.
	damagedHead bool
	copyable    bool // a client that takes the track as it is
}

func tone(channels int) string {
	if channels == 2 {
		return "pan=stereo|FL=c0|FR=c0"
	}
	return "pan=5.1|FL=c0|FR=c0|FC=c0|LFE=c0|BL=c0|BR=c0"
}

// seekAudios are the codecs of the dogfood's library, each in stereo and 5.1, DTS-HD MA in 7.1 and in 5.1 (the dogfood film's: the
// same 512-sample frames), and the 5.1 one again with the damaged head of the flag above. No ffmpeg encoder makes DTS-HD MA, so
// its rows are ffmpeg's own test streams (FATE's dts/master_audio_7.1_24bit, 12.5 seconds, and dts/dcadec-suite/xll_51_24_48_768
// as raw DTS, 64 milliseconds), repeated by bytes to the film's length. There is no DTS-HD MA stereo row: no stream of it is in
// FATE and no ffmpeg encoder makes one (unverified beyond FATE's listing of 2026-11-20). That makes 17 rows and, with a burnt
// image subtitle or not, 34 films, and 10 more with the sound copied.
func seekAudios() []seekAudio {
	var rows []seekAudio
	for _, c := range []struct {
		name, codec, profile string
		args                 func(channels int) []string
		copyable             bool
	}{
		{"aac", "aac", "LC", func(int) []string { return []string{"-c:a", "aac", "-b:a", "192k"} }, true},
		{"ac3", "ac3", "", func(ch int) []string {
			return []string{"-c:a", "ac3", "-b:a", map[int]string{2: "192k", 6: "448k"}[ch]}
		}, true},
		{"eac3", "eac3", "", func(ch int) []string {
			return []string{"-c:a", "eac3", "-b:a", map[int]string{2: "192k", 6: "384k"}[ch]}
		}, true},
		{"dts", "dts", "DTS", func(ch int) []string {
			return []string{"-c:a", "dca", "-strict", "-2", "-b:a", map[int]string{2: "320k", 6: "768k"}[ch]}
		}, false},
		{"truehd", "truehd", "", func(int) []string { return []string{"-c:a", "truehd", "-strict", "-2"} }, false},
		{"flac", "flac", "", func(int) []string { return []string{"-c:a", "flac"} }, true},
		{"opus", "opus", "", func(ch int) []string {
			return []string{"-c:a", "libopus", "-mapping_family", "1", "-b:a", map[int]string{2: "96k", 6: "192k"}[ch]}
		}, true},
	} {
		for _, ch := range []int{2, 6} {
			rows = append(rows, seekAudio{name: fmt.Sprintf("%s-%d", c.name, ch), codec: c.codec, profile: c.profile, channels: ch,
				encode: append([]string{"-af", tone(ch)}, c.args(ch)...), copyable: c.copyable})
		}
	}
	return append(rows,
		seekAudio{name: "dtshdma-8", codec: "dts", profile: "DTS-HD MA", channels: 8, vector: "master_audio_7.1_24bit.dts", vectorSeconds: 12.5},
		seekAudio{name: "dtshdma-6", codec: "dts", profile: "DTS-HD MA", channels: 6, vector: "xll_51_24_48_768.dts", vectorSeconds: 0.064},
		seekAudio{name: "dtshdma-6-head", codec: "dts", profile: "DTS-HD MA", channels: 6, vector: "xll_51_24_48_768.dts", vectorSeconds: 0.064, damagedHead: true})
}

// seekRow is the row of that name.
func seekRow(t *testing.T, name string) seekAudio {
	t.Helper()
	for _, a := range seekAudios() {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("the matrix has no row %s", name)
	return seekAudio{}
}

// probeAudio is what the film's probe says its first audio track is.
func probeAudio(t *testing.T, film string) (codec, profile string, channels int) {
	t.Helper()
	// Standard output alone: a track with a damaged frame makes the demuxer say so on the standard error.
	raw, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name,profile,channels", "-of", "csv=p=0", film).Output()
	if err != nil {
		t.Fatalf("the probe of %s: %v", film, err)
	}
	out := strings.TrimSpace(string(raw))
	fields := strings.Split(out, ",")
	if len(fields) != 3 {
		t.Fatalf("the probe of %s says %q", film, out)
	}
	channels, _ = strconv.Atoi(fields[2])
	return fields[0], fields[1], channels
}

// seekFilms makes the films: one video and one image track (PGS) for all, kept for the test, and an
// audio for each film, which the film's own directory holds until the test ends (the largest of them,
// DTS-HD MA 5.1 and 7.1, are 600 MB to 1 GB: TMPDIR is where they go).
type seekFilms struct {
	dir, video, sup string
}

func newSeekFilms(t *testing.T) *seekFilms {
	t.Helper()
	testenv.FFmpeg(t)
	f := &seekFilms{dir: t.TempDir()}
	f.video = filepath.Join(f.dir, "video.mkv")
	// A keyframe every 2 seconds, as a Blu-ray's 24000/1001 frames at -g 24 are not: no keyframe of the
	// source falls on a segment's time.
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=24000/1001:duration=%d", seekWidth, seekHeight, seekFilmSeconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "38", "-g", "48", f.video)
	var cues []testenv.Cue
	for at := 5.0; at < seekFilmSeconds-10; at += 60 {
		if len(cues)%2 == 0 {
			cues = append(cues, top(at, at+8))
		} else {
			cues = append(cues, bottom(at, at+8))
		}
	}
	f.sup = filepath.Join(f.dir, "track.sup")
	if err := os.WriteFile(f.sup, testenv.PGS(1920, 1080, cues...), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// film makes the film of the audio, seekFilmSeconds long.
func (f *seekFilms) film(t *testing.T, a seekAudio) string {
	t.Helper()
	return f.filmOf(t, a, seekFilmSeconds)
}

// filmOf makes the film of the audio in a directory of its own, as film.mkv, of that many seconds: stream 0 the video, 1 the
// audio, 2 the image track. A tone is encoded for the whole film: an encoded stretch played over again would put its packets off
// the grid at each join, which no film has. A DTS-HD MA stream is the only source there is, a whole number of frames long, and is
// repeated by bytes into a raw stream, never played over with -stream_loop: ffmpeg 6.1 writes one packet with a repeated timestamp
// at each join of a loop and 9.0 and 8.1 do not, which would put a seek's audio off a run from the start's on 6.1 whatever the
// build under test does.
func (f *seekFilms) filmOf(t *testing.T, a seekAudio, seconds int) string {
	t.Helper()
	media := t.TempDir()
	input := filepath.Join(media, "audio.mka")
	if a.vector != "" {
		input = filepath.Join(media, "audio.dts")
		repeatStream(t, filepath.Join("..", "..", "test", "fixtures", "dtshd", a.vector), input, a.vectorSeconds, seconds, a.damagedHead)
	} else {
		run(t, "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + strconv.Itoa(seconds)}, append(a.encode, input)...)...)
	}
	film := filepath.Join(media, "film.mkv")
	audioStart := []string{}
	if a.damagedHead {
		audioStart = []string{"-itsoffset", "0.004"}
	}
	run(t, "ffmpeg", append(append([]string{"-nostdin", "-v", "error", "-y", "-i", f.video}, audioStart...), "-i", input, "-fix_sub_duration", "-i", f.sup,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-c", "copy", "-t", strconv.Itoa(seconds), "-max_interleave_delta", "0",
		"-metadata:s:s:0", "language=eng", film)...)
	os.Remove(input)
	codec, profile, channels := probeAudio(t, film)
	if codec != a.codec || channels != a.channels || a.profile != "" && profile != a.profile {
		t.Fatalf("film %s is %s %q with %d channels, the row says %s %q with %d", a.name, codec, profile, channels, a.codec, a.profile, a.channels)
	}
	if got := gridProbe(t, film, 1); a.damagedHead != (len(got) == 0) {
		t.Fatalf("film %s: the probe of one frame made %v, and the row's damagedHead is %v: the film does not have the property it is made for", a.name, got, a.damagedHead)
	}
	if a.damagedHead {
		if got := gridProbe(t, film, 2); len(got) == 0 || got[0] >= 0 {
			t.Fatalf("film %s: the probe of two frames made %v, want a first packet before zero", a.name, got)
		}
	}
	return media
}

// gridProbe is the pts, in samples, of the packets audioGrid's command makes of the film's audio track (stream 1) with frames
// of it: what that command would be told by a film, run here on the ffmpeg the test runs under.
func gridProbe(t *testing.T, film string, frames int) []int64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "quiet", "-copyts", "-ss", "0.000000", "-i", film, "-map", "0:1", "-frames:a", strconv.Itoa(frames),
		"-c:a", "aac", "-b:a", "192000", "-ac", "2", "-ar", "48000", "-f", "framecrc", "-").Output()
	if err != nil {
		t.Fatalf("the probe of the film's audio: %v", err)
	}
	var pts []int64
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Split(line, ","); len(f) >= 6 && !strings.HasPrefix(line, "#") {
			n, _ := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
			pts = append(pts, n)
		}
	}
	return pts
}

// repeatStream writes the raw stream src, which is one long, to dst over again until it is more than seconds long; damaged zeroes
// 60 bytes of the first frame of the first copy (measured on the 5.1 stream: zeroing from byte 12, 40 or 100 loses the first
// decoded frames and the probe of one frame makes no packet; from byte 400 it does not).
func repeatStream(t *testing.T, src, dst string, one float64, seconds int, damaged bool) {
	t.Helper()
	body := readFile(t, src)
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	first := body
	if damaged {
		first = bytes.Clone(body)
		clear(first[100:160])
	}
	for i := range int(float64(seconds)/one) + 2 {
		chunk := body
		if i == 0 {
			chunk = first
		}
		if _, err := out.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// seekFilm is a rig whose library holds one film, which the scan has read.
func (f *seekFilms) rig(t *testing.T, media string, a seekAudio) *scannedFilm {
	t.Helper()
	s := newScanned(t, media, t.TempDir())
	s.spec.Width, s.spec.Height = seekWidth, seekHeight
	s.spec.VideoBitrate = 300_000
	s.apply(a, false, false)
	return s
}

// apply sets what the viewer's spec is for the film: its sound re-encoded or copied, and the image track
// burnt in or not.
func (s *scannedFilm) apply(a seekAudio, copied, burn bool) {
	s.shortLoop = a.vectorSeconds < 1
	s.spec.AudioCopy, s.spec.AudioBitrate = "", 128_000
	if copied {
		s.spec.AudioCopy, s.spec.AudioBitrate = a.codec, 256_000
	}
	if burn {
		s.burnt()
	} else {
		s.plain()
	}
}

// stored writes the init segment and the segments from..to-1 as the store holds them to dir, as init.mp4
// and n.m4s.
func (s *scannedFilm) storedTo(t *testing.T, dir string, from, to int) {
	t.Helper()
	write := func(name string, n int) {
		body, err := readSegment(context.Background(), s.segments, SegmentKey(s.hash, n))
		if err != nil {
			t.Fatalf("segment %d of the store: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("init.mp4", -1)
	for n := from; n < to; n++ {
		write(fmt.Sprintf("%d.m4s", n), n)
	}
}

// fromTheStart is what a run from the film's first segment makes of segments from to to-1 (and its init
// segment), in a directory under parent, which outlives the subtest that made it, of the spec as it now is.
// It plays the film from 0 as a viewer does, so its job is the one a play has.
func (s *scannedFilm) fromTheStart(t *testing.T, from, to int, parent string) string {
	t.Helper()
	s.fresh(t)
	stop := s.rig.worker(t, func(w *Worker) { w.Slots, w.NoHelpers = 2, true })
	defer stop()
	s.playThrough(t, s.jobs(t), psid(0), 0, to)
	dir, err := os.MkdirTemp(parent, "reference-")
	if err != nil {
		t.Fatal(err)
	}
	s.storedTo(t, dir, from, to)
	return dir
}

// aSeek is what a seek to a segment of a film made: the segment as the store holds it, the init segment
// beside it, and the jobs of the spec.
type aSeek struct {
	dir   string // init.mp4 and n.m4s
	jobs  []job
	reply error
	// reasons are the recorded reasons of the spec's failed jobs.
	reasons []string
	// shortLoop: the film's sound is a stream of 64 ms played over again, see checkSplice.
	shortLoop bool
}

// seekTo makes a viewer who has just seeked to segment n: a fresh worker, and the one request. lead is
// how far the job goes beyond the segment before it waits (8 seconds, as the burn kit's).
func (s *scannedFilm) seekTo(t *testing.T, n int) aSeek {
	t.Helper()
	s.fresh(t)
	stop := s.rig.worker(t, func(w *Worker) { w.Slots, w.NoHelpers, w.Lead = 2, true, 8*time.Second })
	defer stop()
	_, err := s.fetchAt(t, s.jobs(t), psid(0), n, n)
	return s.got(t, n, err)
}

// got is what the store holds of segment n, and the jobs of the spec, after a request for it answered err.
func (s *scannedFilm) got(t *testing.T, n int, err error) aSeek {
	t.Helper()
	out := aSeek{reply: err, jobs: s.jobsOf(t, s.hash), shortLoop: s.shortLoop}
	rows, qerr := s.pool.Query(context.Background(), "SELECT COALESCE(error, '') FROM transcode_jobs WHERE spec_hash = $1 AND state = 'failed' ORDER BY id", s.hash)
	if qerr != nil {
		t.Fatal(qerr)
	}
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatal(err)
		}
		out.reasons = append(out.reasons, reason)
	}
	rows.Close()
	if err == nil {
		out.dir = t.TempDir()
		s.storedTo(t, out.dir, n, n+1)
	}
	return out
}

// pcmLevel is the root mean square of the file's first audio track decoded to mono samples, 0 to 1.
func pcmLevel(t *testing.T, file string) float64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-i", file, "-map", "0:a:0", "-ac", "1", "-ar", "48000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("decoding the sound of %s: %v", file, err)
	}
	if len(out) < 2 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(out); i += 2 {
		v := float64(int16(uint16(out[i]) | uint16(out[i+1])<<8))
		sum += v * v
	}
	return math.Sqrt(sum/float64(len(out)/2)) / 32768
}

// checkSegment says whether a seek's segment n is the one a run from the film's start makes (reference
// holds its init segment and segments around n): it starts with a keyframe at the segment's time, its
// sound is the film's and its video frames and audio packets fall where the run from the start's do, so
// that it joins that run's neighbouring segments with nothing missing, doubled or shifted. jobs are the
// ones that made it: one, begun at n, and not failed. The film's last segment is left out of the audio
// comparison past the picture's end, where a run that began later flushes one more padded AAC frame
// (the seam test leaves it out too).
func checkSegment(t *testing.T, got aSeek, reference string, n int) {
	t.Helper()
	if got.reply != nil {
		t.Fatalf("the seek to segment %d got no segment: %v (jobs %+v, their recorded reasons %q)", n, got.reply, got.jobs, got.reasons)
	}
	if len(got.jobs) != 1 || got.jobs[0].Start != n || got.jobs[0].State == "failed" || got.jobs[0].State == "cancelled" {
		t.Errorf("the seek's segment came from jobs %+v, want one job that began at segment %d and did not fail", got.jobs, n)
	}
	checkSplice(t, got, reference, n)
}

// checkSplice is checkSegment without the question of the jobs.
func checkSplice(t *testing.T, got aSeek, reference string, n int) {
	t.Helper()
	if got.reply != nil {
		t.Fatalf("no segment %d: %v (jobs %+v)", n, got.reply, got.jobs)
	}
	ref := func(m int) string { return filepath.Join(reference, fmt.Sprintf("%d.m4s", m)) }
	exists := func(m int) bool { _, err := os.Stat(ref(m)); return err == nil }
	init := filepath.Join(reference, "init.mp4")
	mine := filepath.Join(got.dir, fmt.Sprintf("%d.m4s", n))
	if a, b := readFile(t, init), readFile(t, filepath.Join(got.dir, "init.mp4")); string(a) != string(b) {
		t.Error("the segment's init segment differs from the run from the start's")
	}
	seek := join(t, init, mine)
	frames := times(t, seek, "v:0", "frame=pts_time")
	if len(frames) == 0 {
		t.Fatalf("segment %d has no picture", n)
	}
	if first := frames[0] - (tsOffset + float64(n*playback.SegmentSeconds)); first < -0.001 || first > 0.1 {
		t.Errorf("segment %d's picture starts %.3f s from the segment's time in the playlist", n, first)
	}
	if flags := run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=flags", "-of", "csv=p=0", "-read_intervals", "%+#1", seek); !strings.HasPrefix(strings.TrimSpace(flags), "K") {
		t.Errorf("segment %d's first video packet is not a keyframe: %q", n, strings.TrimSpace(flags))
	}
	// The level of a segment's sound is the film's to within a quarter. The 5.1 stream the kit has is 64 ms long, played over again,
	// and the sound that the same command makes of it depends on where the run began: measured with Command alone on a film of 50
	// seconds, the level of segment 11 is 0.284 for a run from segment 0, 0.373 from 1, 0.360 from 2, 0.291 from 5, 0.488 from 9
	// and 0.356 from 12, so it is the stream's and not the build's (the 7.1 stream, 12.5 s, gives the same level from any start).
	// For that stream the level may differ by a factor of 4 either way: silence and sound that is not the film's still fail.
	low, high := 0.8, 1.25
	if got.shortLoop {
		low, high = 0.25, 4
	}
	// Every film of the kit has sound from its start to its end, so a run from the start must have made some: a segment of a seek
	// that has none is a film that plays on without sound, which no film here asks for.
	if len(times(t, join(t, init, ref(n)), "a:0", "packet=pts_time")) == 0 {
		t.Fatalf("segment %d of the run from the start has no sound: the kit's film is wrong", n)
	}
	if got := times(t, seek, "a:0", "packet=pts_time"); len(got) == 0 {
		t.Errorf("segment %d has no audio packet, and the film has sound there", n)
	} else if level, wanted := pcmLevel(t, seek), pcmLevel(t, join(t, init, ref(n))); level == 0 || level < wanted*low || level > wanted*high {
		t.Errorf("segment %d's sound is at level %.4f, the run from the start's %.4f: silent, absent or not the film's", n, level, wanted)
	}
	// Spliced between the run from the start's own neighbours: what comes out is that run's.
	whole, spliced := []string{init}, []string{init}
	if exists(n - 1) {
		whole, spliced = append(whole, ref(n-1)), append(spliced, ref(n-1))
	}
	whole, spliced = append(whole, ref(n)), append(spliced, mine)
	last := !exists(n + 1)
	if !last {
		whole, spliced = append(whole, ref(n+1)), append(spliced, ref(n+1))
	}
	w, sp := join(t, whole[0], whole[1:]...), join(t, spliced[0], spliced[1:]...)
	compare(t, "video", times(t, w, "v:0", "frame=pts_time"), times(t, sp, "v:0", "frame=pts_time"), 0.001)
	end := math.Inf(1)
	if last {
		end = tsOffset + seekFilmSeconds - 0.05
	}
	compare(t, "audio", before(times(t, w, "a:0", "packet=pts_time"), end), before(times(t, sp, "a:0", "packet=pts_time"), end), 0.0005)
}
