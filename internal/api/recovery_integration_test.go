//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-088: what a player is sent when the index of a copy is found wrong under it, and what it can do with it.
//
// jellyfin-web loads the copy's init segment once (EXT-X-MAP) and then the segments of the playlist it holds, which
// the server cut on the index it had then. A fallback to a re-encode must keep three promises that currentTime in
// a browser does not show (Chrome's decoder logs errors on bytes it cannot decode, and the position goes on):
//
//   - every segment the player is sent decodes, under the init the player holds at that point: the one it loaded, or
//     the one a segment starts with (a moov at the start of a media segment is an init segment to MSE; observed on
//     jellyfin-web's hls.js 1.6.16 in Chrome, compat/captures/segment-errors-12.4.md, where a segment of x264 under
//     the init of another x264 stream was a PIPELINE_ERROR_DECODE and the same segment with its own moov in front
//     played and showed the same picture);
//   - that init is of the codec the master playlist names (CODECS), because the SourceBuffer is typed by it when the
//     player adds it, and an HEVC or AV1 copy has no player that takes x264 under it;
//   - segment N starts and ends where the playlist says, on the layout the playlist was cut from (CopyLayout of
//     the index as it was then), not on the 4 s grid of an encode: the e2e films' copies fit that grid, a film
//     with an irregular keyframe gap does not.
//
// The checks drive the api role over HTTP, as a player does, with a real worker and ffmpeg, and name nothing the
// build adds. They fail today because a segment requested after the index is dropped is answered 400 or 500.

// irregularFilm makes a 44 s film in codec with keyframes at 0, 5, 11, 16, 23, 29, 36 and 41 s only, so that the
// copy's layout is segments of 5, 6, 5, 7, 6, 7, 5 and 3 s, which the 4 s grid of an encode does not share a start
// with after the first.
func irregularFilm(t *testing.T, dir, codec string) string {
	t.Helper()
	testenv.FFmpeg(t)
	name := "Irregular " + strings.ToUpper(codec) + " Film (2024)"
	encoder := []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "1000", "-sc_threshold", "0"}
	if codec == "hevc" {
		encoder = []string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=1000:scenecut=0:forced-idr=1:log-level=error"}
	}
	args := append([]string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=44", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=44"}, encoder...)
	args = append(args, "-force_key_frames", "5,11,16,23,29,36,41", "-pix_fmt", "yuv420p", "-c:a", "ac3", "-b:a", "128k")
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	ffmpegRun(t, append(args, filepath.Join(dir, name, name+".mkv"))...)
	return "Irregular " + strings.ToUpper(codec) + " Film"
}

type recoveryRig struct {
	*env
	item, source, dashed string
	segments             store.Store
}

// recoveryEnv scans one irregular film and runs a worker with the real ffmpeg, as a deployment has, and the replicas'
// transcode store.
func recoveryEnv(t *testing.T, codec string) *recoveryRig {
	t.Helper()
	films := t.TempDir()
	title := irregularFilm(t, filepath.Join(films, "films"), codec)
	e := newEnv(t)
	ctx := context.Background()
	libs, err := library.Sync(ctx, e.pool, []library.Config{{Name: "Recovery", Kind: "movies", Media: "file://" + filepath.Join(films, "films"), Assets: "file://" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&library.Scanner{Pool: e.pool, Log: slog.New(slog.DiscardHandler)}).Scan(ctx, libs[0]); err != nil {
		t.Fatal(err)
	}
	r := &recoveryRig{env: e}
	r.item = scalar[string](t, e.pool, "SELECT id::text FROM items WHERE type = 'Movie' AND name = $1", title)
	r.source = scalar[string](t, e.pool, "SELECT replace(id::text, '-', '') FROM media_sources WHERE item_id = $1", r.item)
	r.dashed = r.item
	if n := scalar[int](t, e.pool, "SELECT cardinality(keyframes) FROM media_sources WHERE item_id = $1", r.item); n != 8 {
		t.Fatalf("the scan read %d keyframes of the film, want the 8 it was made with", n)
	}

	mustExec(t, e.pool, "TRUNCATE transcode_jobs, transcode_nodes")
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	r.segments = store.NewPosix(t.TempDir())
	var media store.Cache
	log := slog.New(slog.NewTextHandler(&syncBuffer{}, nil))
	w := &transcode.Worker{ID: "worker-" + uuid.NewString()[:8], Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: r.segments, Log: log,
		Media: media.Open, Scratch: t.TempDir(), Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	w.Hub = &transcode.Hub{Pool: pool, Log: log}
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, run := range []func(){func() { w.Hub.Run(runCtx) }, func() { w.Present(runCtx, "worker", "", 200*time.Millisecond) }, func() { w.Run(runCtx) }} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	t.Cleanup(func() { cancel(); wg.Wait(); pool.Close() })
	for _, s := range e.apis {
		hub := &transcode.Hub{Pool: s.Pool, Log: log}
		go hub.Run(runCtx)
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: r.segments, Hub: hub, QueueTimeout: 10 * time.Second, WaitTimeout: 60 * time.Second}
	}
	waitUntil(t, "the worker to register", func() bool {
		return scalar[int](t, e.pool, "SELECT count(*)::int FROM transcode_nodes WHERE roles = 'worker'") == 1
	})
	return r
}

// get is one request of the player's, which must be answered 200: a player drops the stream on any status of 400 or more.
func (r *recoveryRig) get(t *testing.T, what, path string) []byte {
	t.Helper()
	response, body := r.do(t, 0, "GET", path, "", nil, "Authorization", strings.Replace(auth, "%s", r.token, 1))
	if response.StatusCode != 200 {
		t.Fatalf("%s: answered %d %.200s, where the player would drop the stream", what, response.StatusCode, body)
	}
	return body
}

// A player's model of what it holds: the init it loaded, replaced by a moov at the start of a media segment.
func splitInit(segment []byte) (init, media []byte) {
	at := 0
	for at+8 <= len(segment) {
		size := int(binary.BigEndian.Uint32(segment[at:]))
		kind := string(segment[at+4 : at+8])
		if (kind != "ftyp" && kind != "moov") || size < 8 || at+size > len(segment) {
			break
		}
		at += size
	}
	return segment[:at], segment[at:]
}

type decoded struct {
	codec         string
	first, length float64 // Of the video, in seconds on the clock of the segment's own timestamps: the first packet's time (the keyframe's) and the latest end less it.
	last          float64 // The latest end of a video packet, on the same clock.
	keyFirst      bool
	frames        int
	problems      string
}

// decode is what a decoder makes of the init the player holds followed by one segment.
func decode(t *testing.T, init, media []byte) decoded {
	t.Helper()
	whole := append(append([]byte{}, init...), media...)
	var d decoded
	run := func(name string, args ...string) (string, string) {
		cmd := exec.Command(name, args...)
		cmd.Stdin = bytes.NewReader(whole)
		var out, errs bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := cmd.Run(); err != nil {
			errs.WriteString(err.Error())
		}
		return out.String(), errs.String()
	}
	if _, errs := run("ffmpeg", "-nostdin", "-v", "error", "-i", "-", "-f", "null", "-"); strings.TrimSpace(errs) != "" {
		d.problems = strings.TrimSpace(errs)
		if len(d.problems) > 300 {
			d.problems = d.problems[:300]
		}
	}
	out, _ := run("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,duration_time,flags", "-of", "csv=p=0", "-")
	var last float64
	for i, line := range strings.Fields(out) {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}
		pts, _ := strconv.ParseFloat(fields[0], 64)
		duration, _ := strconv.ParseFloat(fields[1], 64)
		if i == 0 {
			d.first, d.keyFirst = pts, strings.HasPrefix(fields[2], "K")
		}
		d.frames++
		last = max(last, pts+duration)
	}
	d.last, d.length = last, last-d.first
	out, _ = run("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", "-")
	d.codec = strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	return d
}

// playUnderAWrongIndex is a viewer of a copy whose index is wrong from the 5th keyframe on (a second late): the
// playlist is cut from that index, as the player's is when the server's index is wrong, the init is loaded, and the
// segments are fetched in order, the first of which `first` names. The 4th segment of the playlist (index 3) is the
// first that does not end on a keyframe of the file. Every segment is checked as the player would decode it.
func playUnderAWrongIndex(t *testing.T, codec, profileName string, order string) {
	r := recoveryEnv(t, codec)
	mustExec(t, r.pool, "UPDATE media_sources SET keyframes = ARRAY(SELECT CASE WHEN n <= 4 THEN k ELSE k + 10000000 END FROM unnest(keyframes) WITH ORDINALITY AS t(k, n) ORDER BY n) WHERE item_id = $1", r.item)
	keyframes := scalar[[]int64](t, r.pool, "SELECT keyframes FROM media_sources WHERE item_id = $1", r.item)
	end := scalar[int64](t, r.pool, "SELECT video_end FROM media_sources WHERE item_id = $1", r.item)
	layout, ok := playback.CopyLayout(keyframes, end-keyframes[0])
	if !ok || layout.Count() != 8 || layout.Count() == playback.EncodedLayout(end).Count() {
		t.Fatalf("the film's copy layout has %d segments (ok %v): the fixture must differ from the 4 s grid in count and in starts", layout.Count(), ok)
	}

	profile := capturedProfile(t)
	if profileName != "" {
		profile = hevcAV1Profile(t, profileName)
	}
	info := r.playbackInfo(t, 0, r.item, map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "MediaSourceId": r.source, "SubtitleStreamIndex": "-1"})
	url := firstSource(info)["TranscodingUrl"].(string)
	if mustQuery(t, url).Get("VideoCodec") != "copy" {
		t.Fatalf("the film is not offered as a copy: %s", url)
	}
	master := string(r.get(t, "master", url))
	wantCodec := map[string]string{"h264": "h264", "hevc": "hevc"}[codec]
	if codecs := playlistCodec(master); codecs == "" {
		t.Fatalf("the master playlist names no codec:\n%s", master)
	}
	base := "/videos/" + r.dashed + "/"
	mainPlaylist := string(r.get(t, "main playlist", strings.Replace(url, "master.m3u8", "main.m3u8", 1)))
	var mapURL string
	var segmentURLs []string
	for _, line := range strings.Split(mainPlaylist, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:URI=\""):
			mapURL = base + strings.TrimSuffix(strings.TrimPrefix(line, "#EXT-X-MAP:URI=\""), "\"")
		case strings.HasPrefix(line, "hls1/main/"):
			segmentURLs = append(segmentURLs, base+line)
		}
	}
	if len(segmentURLs) != layout.Count() || mapURL == "" {
		t.Fatalf("the playlist has %d segments and map %q, want %d from the index", len(segmentURLs), mapURL, layout.Count())
	}

	// What the player loaded before anything fell back.
	held := r.get(t, "the init segment", mapURL)
	initCodec := decode(t, held, nil).codec
	if initCodec != wantCodec {
		t.Fatalf("the copy's init is %q, want %q", initCodec, wantCodec)
	}

	var sequence []int
	switch order {
	case "the job finds the index wrong while the player is on stored segments":
		// Segment 0 starts a job, which reads on and fails at segment 3; the player is told nothing and plays 1, 2 from the store.
		r.get(t, "segment 0", segmentURLs[0])
		waitUntil(t, "the job to find the index wrong", func() bool {
			return scalar[int](t, r.pool, "SELECT cardinality(keyframes) FROM media_sources WHERE item_id = $1", r.item) == 0
		})
		sequence = []int{0, 1, 2, 3, 4, 5, 6, 7}
	default: // The player seeks to the bad segment, then plays the film from the start.
		sequence = []int{3, 0, 1, 2, 4, 5, 6, 7}
	}

	got := map[int]decoded{}
	for _, n := range sequence {
		segment := r.get(t, fmt.Sprintf("segment %d", n), segmentURLs[n])
		if again, media := splitInit(segment); len(again) > 0 {
			held, segment = again, media // A moov at the start of a segment is an init segment to MSE.
		} else {
			segment = media
		}
		d := decode(t, held, segment)
		got[n] = d
		if d.problems != "" {
			t.Errorf("segment %d does not decode under the init the player holds: %s", n, d.problems)
		}
		if d.codec != wantCodec {
			t.Errorf("segment %d is decoded under an init of %q, the playlist's codec is %q: the player's SourceBuffer is typed by the master playlist", n, d.codec, wantCodec)
		}
		if !d.keyFirst {
			t.Errorf("segment %d does not start on a keyframe: a player that seeks to it cannot decode it", n)
		}
	}
	// The playlist's promise: segment N starts at Starts[N] and lasts what EXTINF says, on the clock the first segment sets.
	// A segment that is the copy's own (the ones before the wrong index) holds the file's bytes, and a file with
	// leading pictures (x265 puts three frames shown before a forced keyframe after it in decode order) has them in the
	// segment of the keyframe they follow, so the segment's last picture is shown a few frames before the next keyframe's
	// time: its length is measured as the copy has it, from its keyframe to the next segment's keyframe. A segment the
	// fallback re-encoded is cut by the layout, with no leading pictures, and is held to the exact end of its pictures
	// and to the frames its length has.
	zero := got[0].first
	copied := keyframes[3] - keyframes[0] // The index is right up to here (playUnderAWrongIndex).
	total := 0
	for n := range layout.Count() {
		start, length := layout.Segment(n)
		d := got[n]
		total += d.frames
		measured := d.length
		if start+length <= copied {
			measured = d.last - d.first
			if n+1 < layout.Count() {
				measured = got[n+1].first - d.first
			}
		} else if want := math.Round(float64(length) / 1e7 * 24); math.Abs(float64(d.frames)-want) > 1 {
			t.Errorf("segment %d is a re-encode of %d frames, its %.3f s hold %.0f", n, d.frames, float64(length)/1e7, want)
		}
		if math.Abs(d.first-zero-float64(start)/1e7) > 0.05 || math.Abs(measured-float64(length)/1e7) > 0.1 {
			t.Errorf("segment %d starts at %.3f s and lasts %.3f s, the playlist says %.3f s and %.3f s (CopyLayout of the index it was cut from)",
				n, d.first-zero, measured, float64(start)/1e7, float64(length)/1e7)
		}
	}
	if want := 44 * 24; math.Abs(float64(total-want)) > float64(2*layout.Count()) {
		t.Errorf("the segments hold %d frames in all, the film has %d: a part of it was skipped or played twice", total, want)
	}
	if n := scalar[int](t, r.pool, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); n < 1 {
		t.Errorf("no job is recorded failed: a recovered failure stays visible")
	}
}

func playlistCodec(master string) string {
	_, rest, _ := strings.Cut(master, `CODECS="`)
	codecs, _, _ := strings.Cut(rest, `"`)
	return strings.SplitN(codecs, ",", 2)[0]
}

func TestAFallbackFromACopyIsDecodableUnderTheInitThePlayerHoldsAndKeepsThePlaylistsLayout(t *testing.T) {
	for _, codec := range []struct{ name, profile string }{{"h264", ""}, {"hevc", "hevc-client"}} {
		for _, order := range []string{"the job finds the index wrong while the player is on stored segments", "the player seeks to the bad segment first"} {
			t.Run(codec.name+", "+order, func(t *testing.T) { playUnderAWrongIndex(t, codec.name, codec.profile, order) })
		}
	}
}

// The check above can fail: it is shown here what decode and splitInit say of the three ways a build could serve a
// re-encode after a copy's init is out of the player's hands. A re-encode under the init of another stream is the
// case jellyfin-web's Chrome failed on (PIPELINE_ERROR_DECODE) while currentTime went on and corruptedVideoFrames stayed 0.
func TestTheDecodeCheckSeesASegmentThatTheHeldInitCannotDecode(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	hls := func(name string, encoder ...string) (init, segment []byte) {
		args := append([]string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=8"}, encoder...)
		args = append(args, "-pix_fmt", "yuv420p", "-f", "hls", "-hls_time", "4", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", name+"-init.mp4",
			"-hls_segment_filename", filepath.Join(dir, name+"-%d.m4s"), filepath.Join(dir, name+".m3u8"))
		ffmpegRun(t, args...)
		read := func(file string) []byte {
			data, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		return read(name + "-init.mp4"), read(name + "-1.m4s")
	}
	copyInit, copySegment := hls("copy", "-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "baseline", "-g", "96", "-sc_threshold", "0")
	ownInit, ownSegment := hls("own", "-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-g", "96", "-sc_threshold", "0")
	hevcInit, _ := hls("hevc", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=96:scenecut=0:log-level=error")

	if d := decode(t, copyInit, copySegment); d.problems != "" || d.codec != "h264" || !d.keyFirst {
		t.Errorf("a segment under its own init: %+v", d)
	}
	if d := decode(t, copyInit, ownSegment); d.problems == "" {
		t.Errorf("a re-encode's segment under the copy's init decoded with no error: %+v", d)
	}
	// A moov in front of the segment is the player's new init (splitInit), and the segment then decodes.
	if again, media := splitInit(append(append([]byte{}, ownInit...), ownSegment...)); len(again) == 0 || len(media) == 0 {
		t.Errorf("splitInit found no init in front of a segment that has one")
	} else if d := decode(t, again, media); d.problems != "" || d.codec != "h264" {
		t.Errorf("a re-encode's segment under its own init: %+v", d)
	}
	if again, _ := splitInit(ownSegment); len(again) != 0 {
		t.Errorf("splitInit took %d bytes of a bare segment for an init", len(again))
	}
	// An x264 re-encode under the init of an HEVC copy: the codec of the init says it.
	if d := decode(t, hevcInit, ownSegment); d.problems == "" && d.codec == "h264" {
		t.Errorf("an x264 segment under an HEVC init was taken for good: %+v", d)
	}
}
