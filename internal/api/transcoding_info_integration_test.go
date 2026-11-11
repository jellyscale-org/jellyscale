//go:build integration

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// TranscodingInfo of GET /Sessions (JS-067). The fields and their values are
// those of compat/captures/transcoding-info-12.4.json, which a real Jellyfin
// 12.4 gave jellyfin-web over the same kinds of film. jellyscale adds three
// fields to it, which the dashboard shows an operator and jellyfin-web ignores
// (compat/differences.yaml lists them):
//
//	EncodeSpeed      film seconds the job encodes per second of its own running
//	                 time, as the metrics slice measures it (JS-019); absent
//	                 until it has been measured
//	Worker           the id of the worker that runs the job
//	IsBelowRealTime  true when EncodeSpeed is below 1, so the viewer's playback
//	                 will stall; absent or false otherwise, and while the job
//	                 sits at its lead limit, which is not slow

// jellyfinKeys are the keys of TranscodingInfo in the capture of a re-encode
// that was still running.
func jellyfinKeys(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/transcoding-info-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Scenarios map[string]struct {
			SessionsDuringPlayback struct{ TranscodingInfo map[string]any }
		}
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range capture.Scenarios["slow"].SessionsDuringPlayback.TranscodingInfo {
		keys = append(keys, key)
	}
	if len(keys) < 10 {
		t.Fatalf("the capture has no TranscodingInfo to compare with: %v", keys)
	}
	return keys
}

var improvementKeys = []string{"EncodeSpeed", "Worker", "IsBelowRealTime"}

// transcodingInfo is the TranscodingInfo of the device's session, as the
// replica answers GET /Sessions, or nil when the session has none.
func (e *env) transcodingInfo(t *testing.T, replica int) map[string]any {
	t.Helper()
	response, body := e.do(t, replica, "GET", "/Sessions", e.token, nil)
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil || response.StatusCode != 200 || len(sessions) != 1 {
		t.Fatalf("GET /Sessions: %d %v %s", response.StatusCode, err, body)
	}
	info, _ := sessions[0]["TranscodingInfo"].(map[string]any)
	return info
}

// play makes the device report what jellyfin-web reports when playback starts.
func (e *env) play(t *testing.T, reply map[string]any, method string) {
	t.Helper()
	response, body := e.do(t, 0, "POST", "/Sessions/Playing", e.token, map[string]any{
		"ItemId": e.item, "MediaSourceId": db.CompactID(e.item), "PlayMethod": method,
		"PlaySessionId": reply["PlaySessionId"], "PositionTicks": 0, "IsPaused": false})
	if response.StatusCode != 204 {
		t.Fatalf("Sessions/Playing: %d %s", response.StatusCode, body)
	}
}

func transcodeReasons(url string) []string {
	for _, pair := range strings.Split(url[strings.Index(url, "?")+1:], "&") {
		if value, ok := strings.CutPrefix(pair, "TranscodeReasons="); ok {
			return strings.Split(value, ",")
		}
	}
	return nil
}

// specOf is the spec the server derives from a TranscodingUrl.
func specOf(t *testing.T, e *env, transcodingURL string) (playback.Spec, string) {
	t.Helper()
	values := lowerKeys(mustQuery(t, transcodingURL))
	source, found, err := e.apis[0].Items.Source(context.Background(), e.item, values.Get("mediasourceid"))
	if err != nil || !found {
		t.Fatal(err)
	}
	media, err := playbackSource(source)
	if err != nil {
		t.Fatal(err)
	}
	if media.Keyframes, media.VideoEnd, err = e.apis[0].Items.Keyframes(context.Background(), source.ID); err != nil {
		t.Fatal(err)
	}
	params, err := playback.ParseParams(values, media)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := playback.NewSpec(e.item, params, media)
	if err != nil {
		t.Fatal(err)
	}
	return spec, spec.Hash()
}

// startedJob puts a viewer and the job the player's segment request would have
// made in the database, as a worker that runs it leaves them, without a worker.
func (e *env) startedJob(t *testing.T, reply map[string]any, spec playback.Spec, hash, state string, next int, worker string) {
	t.Helper()
	raw, _ := json.Marshal(spec)
	psid := reply["PlaySessionId"].(string)
	id := scalar[int64](t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ($1, $2, $3, 0, $4, $5, NULLIF($6, ''), now() + interval '1 hour') RETURNING id`, hash, raw, psid, next, state, worker)
	mustExec(t, e.pool, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, item_id, job_id)
		VALUES ($1, $2, $3, 'device-viewer', $4, $5)`, psid, hash, e.user, db.CompactID(e.item), id)
}

// Each kind of transcode answers with Jellyfin's fields and values, from either
// replica, and with the reasons PlaybackInfo gave for the same request.
func TestSessionsCarryTranscodingInfo(t *testing.T) {
	copyable := func(e *env) {
		mustExec(t, e.pool, "UPDATE media_sources SET keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', video_end = 900000000, size = 25000000")
		mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
	}
	cases := []struct {
		name         string
		video, audio string
		prepare      func(*env)
		copy         bool // Jellyfin's IsVideoDirect, from the capture's copy
		reasons      []string
	}{
		{"re-encode of HEVC with AC3", "hevc", "ac3", nil, false, []string{"VideoCodecNotSupported", "AudioCodecNotSupported"}},
		{"re-encode of HEVC with AAC", "hevc", "aac", nil, false, []string{"VideoCodecNotSupported"}},
		{"copy of H.264 with AC3", "h264", "ac3", copyable, true, []string{"AudioCodecNotSupported"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, source := transcodeEnv(t, c.video, c.audio)
			if c.prepare != nil {
				c.prepare(e)
			}
			reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647,
				"AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source})
			url := firstSource(reply)["TranscodingUrl"].(string)
			reasons := transcodeReasons(url)
			if !slices.Equal(reasons, c.reasons) {
				t.Fatalf("the decision's reasons are %v, this test expects %v", reasons, c.reasons)
			}
			spec, hash := specOf(t, e, url)
			e.play(t, reply, "Transcode")
			// 5 of the 4 s segments are stored: 20 s of the film's run time.
			e.startedJob(t, reply, spec, hash, "running", 5, "w1")

			for replica := range 2 {
				info := e.transcodingInfo(t, replica)
				if info == nil {
					t.Fatalf("replica %d: no TranscodingInfo for a session that transcodes", replica)
				}
				for _, key := range jellyfinKeys(t) {
					if _, ok := info[key]; !ok {
						t.Errorf("replica %d: TranscodingInfo lacks %s, which Jellyfin 12.4 sends: %v", replica, key, info)
					}
				}
				for key := range info {
					if !slices.Contains(jellyfinKeys(t), key) && !slices.Contains(improvementKeys, key) {
						t.Errorf("replica %d: TranscodingInfo has %s, which neither Jellyfin nor this task's improvement has", replica, key)
					}
				}
				wantVideo, wantAudio := "h264", "aac"
				if c.copy {
					wantVideo = c.video
				}
				audioDirect := spec.AudioCopy != ""
				if audioDirect {
					wantAudio = spec.AudioCopy
				}
				if info["VideoCodec"] != wantVideo || info["AudioCodec"] != wantAudio || info["Container"] != "mp4" ||
					info["IsVideoDirect"] != c.copy || info["IsAudioDirect"] != audioDirect || info["HardwareAccelerationType"] != "none" {
					t.Errorf("replica %d: codecs and direct flags: %v", replica, info)
				}
				if info["Width"] != float64(spec.Width) || info["Height"] != float64(spec.Height) || info["AudioChannels"] != float64(2) {
					t.Errorf("replica %d: size and channels, want %dx%d and 2: %v", replica, spec.Width, spec.Height, info)
				}
				if info["Bitrate"] != float64(spec.VideoBitrate+spec.AudioBitrate) {
					t.Errorf("replica %d: Bitrate %v, want the %d the encode is told to use", replica, info["Bitrate"], spec.VideoBitrate+spec.AudioBitrate)
				}
				runSeconds := float64(scalar[int64](t, e.pool, "SELECT run_time_ticks FROM items LIMIT 1")) / 1e7
				if got, _ := info["CompletionPercentage"].(float64); math.Abs(got-20*100/runSeconds) > 0.5 {
					t.Errorf("replica %d: CompletionPercentage %v, want the stored share of the run time", replica, info["CompletionPercentage"])
				}
				// The same reasons as the PlaybackInfo for this request, in its order.
				var got []string
				for _, reason := range info["TranscodeReasons"].([]any) {
					got = append(got, reason.(string))
				}
				if !slices.Equal(got, reasons) {
					t.Errorf("replica %d: TranscodeReasons %v, PlaybackInfo gave %v", replica, got, reasons)
				}
				if info["Worker"] != "w1" {
					t.Errorf("replica %d: Worker %v, the job runs on w1", replica, info["Worker"])
				}
			}
		})
	}
}

// Jellyfin sends no TranscodingInfo for a direct play, and none once the
// player has stopped (compat/captures/transcoding-info-12.4.json).
func TestSessionsHaveNoTranscodingInfoWithoutATranscode(t *testing.T) {
	e, _, source := transcodeEnv(t, "h264", "aac")
	mustExec(t, e.pool, "UPDATE media_sources SET container = 'mp4'")
	direct := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "MediaSourceId": source})
	if firstSource(direct)["SupportsDirectPlay"] != true {
		t.Fatalf("the film should direct play: %v", firstSource(direct))
	}
	e.play(t, direct, "DirectPlay")
	if info := e.transcodingInfo(t, 1); info != nil {
		t.Errorf("direct play has TranscodingInfo: %v", info)
	}

	// Now a transcode, stopped.
	mustExec(t, e.pool, "UPDATE media_sources SET container = 'mkv', streams = replace(streams::text, 'h264', 'hevc')::jsonb")
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "MediaSourceId": source})
	url := firstSource(reply)["TranscodingUrl"].(string)
	spec, hash := specOf(t, e, url)
	e.play(t, reply, "Transcode")
	e.startedJob(t, reply, spec, hash, "running", 1, "w1")
	if e.transcodingInfo(t, 0) == nil {
		t.Fatal("no TranscodingInfo while it transcodes")
	}
	if response, body := e.do(t, 1, "POST", "/Sessions/Playing/Stopped", e.token, map[string]any{"ItemId": e.item, "PlaySessionId": reply["PlaySessionId"], "PositionTicks": 0}); response.StatusCode != 204 {
		t.Fatalf("Stopped: %d %s", response.StatusCode, body)
	}
	if info := e.transcodingInfo(t, 0); info != nil {
		t.Errorf("TranscodingInfo after the player stopped: %v", info)
	}
}

// A real worker, with real ffmpeg, and the session read from api replicas that
// share nothing with it but Postgres.
type liveTranscode struct {
	e     *env
	w     *transcode.Worker
	reply map[string]any
}

// throttledFFmpeg makes ffmpeg read its input at rate times the speed of the
// film, so that a job runs at about that speed whatever the machine is.
func throttledFFmpeg(t *testing.T, rate string) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + real + " -readrate " + rate + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// startLive plays a real 60 second film (the record says HEVC and AC3, so it
// is a re-encode) and asks for its first segment, which a worker makes.
func startLive(t *testing.T, rate string, lead time.Duration) *liveTranscode {
	t.Helper()
	ffmpeg := testenv.FFmpeg(t)
	e, segments, source := transcodeEnv(t, "hevc", "ac3")
	dir := strings.TrimPrefix(e.uri, "file://")
	film := filepath.Join(dir, "film.mkv")
	out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=60", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", film).CombinedOutput()
	if err != nil {
		t.Fatalf("making the film: %v\n%s", err, out)
	}
	info, _ := os.Stat(film)
	mustExec(t, e.pool, "UPDATE media_sources SET size = $1", info.Size())
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 600000000")
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")
	throttledFFmpeg(t, rate)

	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	w := &transcode.Worker{ID: "worker-" + uuid.NewString()[:8], Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: segments,
		Log: nil, Media: media.Open, Scratch: t.TempDir(), Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond, Lead: lead}
	w.Log = slog.New(slog.DiscardHandler)
	ctx, cancel := context.WithCancel(context.Background())
	w.Hub = &transcode.Hub{Pool: pool, Log: w.Log}
	var wg sync.WaitGroup
	for _, run := range []func(){func() { w.Hub.Run(ctx) }, func() { w.Present(ctx, "worker", "", 200*time.Millisecond) }, func() { w.Run(ctx) }} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	t.Cleanup(func() { cancel(); wg.Wait(); pool.Close() })
	for _, s := range e.apis {
		hub := &transcode.Hub{Pool: s.Pool, Log: w.Log}
		go hub.Run(ctx)
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments, Hub: hub, QueueTimeout: 10 * time.Second, WaitTimeout: 60 * time.Second}
	}

	// PlaybackInfo offers a transcode only once a worker has registered, which
	// the worker does a moment after it starts.
	if !eventually(func() bool { return scalar[int](t, e.pool, "SELECT count(*) FROM transcode_nodes") > 0 }) {
		t.Fatal("the worker never registered")
	}
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647,
		"AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source})
	if sources, _ := reply["MediaSources"].([]any); len(sources) == 0 {
		t.Fatalf("PlaybackInfo offered no source: %v", reply)
	}
	url := firstSource(reply)["TranscodingUrl"].(string)
	e.play(t, reply, "Transcode")
	_, master := e.do(t, 0, "GET", url, "", nil)
	path := "/videos/" + db.DashedID(e.item)
	main := path + "/main.m3u8?" + strings.TrimSpace(strings.SplitN(string(master), "main.m3u8?", 2)[1])
	_, playlist := e.do(t, 1, "GET", main, "", nil)
	var first string
	for _, line := range strings.Split(string(playlist), "\n") {
		if strings.HasPrefix(line, "hls1/main/0.mp4?") {
			first = path + "/" + line
		}
	}
	if first == "" {
		t.Fatalf("no entry for segment 0:\n%s", playlist)
	}
	if response, body := e.do(t, 0, "GET", first, "", nil); response.StatusCode != 200 {
		t.Fatalf("segment 0: %d %s", response.StatusCode, body)
	}
	return &liveTranscode{e: e, w: w, reply: reply}
}

// speedy waits for the first TranscodingInfo the replicas give that has an
// EncodeSpeed and some progress, asking each in turn. A segment is in the store
// a moment before the job's next_segment says so, and a heartbeat can write the
// speed in between, so the speed alone can come with 0%.
func (l *liveTranscode) speedy(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for replica := 0; time.Now().Before(deadline); replica++ {
		if info := l.e.transcodingInfo(t, replica%2); info != nil && info["EncodeSpeed"] != nil && info["CompletionPercentage"].(float64) > 0 {
			return info
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no EncodeSpeed within 45 s; the job: %v", l.e.transcodingInfo(t, 0))
	return nil
}

// belowOne is whether a speed is under 1, as its whole part being 0, so that
// what is compared is the flag with the speed in the same answer and no test
// holds a speed to a bound (transcoding_speed_integration_test.go has the rest).
func belowOne(speed float64) bool { return int(speed) == 0 }

// A job that runs at a third of real time shows its measured speed and worker
// on both replicas, and is flagged. ffmpeg's throttle is a ceiling, so on any
// machine the job is below 1 and flagged; how far below is the machine's.
func TestSessionsShowTheRealEncodeSpeedAndFlagASlowJob(t *testing.T) {
	l := startLive(t, "0.3", time.Hour)
	info := l.speedy(t)
	speed := info["EncodeSpeed"].(float64)
	t.Logf("a job throttled to 0.3 times the film shows EncodeSpeed %.2fx", speed)
	if info["IsBelowRealTime"] != true {
		t.Errorf("a job at %.2fx is not flagged: %v", speed, info)
	}
	if info["Worker"] != l.w.ID {
		t.Errorf("Worker %v, want %s", info["Worker"], l.w.ID)
	}
	// jellyfin-web shows Framerate against the film's 24 fps as "N fps (Mx)".
	if fps, _ := info["Framerate"].(float64); math.Abs(fps-speed*24) > 2 {
		t.Errorf("Framerate %v does not follow EncodeSpeed %v of a 24 fps film", info["Framerate"], speed)
	}
	if done, _ := info["CompletionPercentage"].(float64); done <= 0 || done >= 100 {
		t.Errorf("CompletionPercentage %v of a film that is still being encoded", info["CompletionPercentage"])
	}
	// Both replicas answer from Postgres, so they agree on who runs it.
	for replica := range 2 {
		if got := l.e.transcodingInfo(t, replica); got == nil || got["Worker"] != l.w.ID {
			t.Errorf("replica %d: %v", replica, got)
		}
	}
}

// A job whose ffmpeg may keep ahead of the viewer shows a speed and the flag
// that goes with it. Whether the machine reaches 1 is not the code's to say, so
// the flag is compared with the speed in the same answer.
func TestSessionsDoNotFlagAJobThatKeepsUp(t *testing.T) {
	l := startLive(t, "3", time.Hour)
	info := l.speedy(t)
	t.Logf("a job throttled to 3 times the film shows EncodeSpeed %.2fx", info["EncodeSpeed"].(float64))
	if flagged, _ := info["IsBelowRealTime"].(bool); flagged != belowOne(info["EncodeSpeed"].(float64)) {
		t.Errorf("IsBelowRealTime %v for EncodeSpeed %v: %v", flagged, info["EncodeSpeed"], info)
	}
}

// A job that waits at its lead limit for the viewer is idle by design: the
// speed it had stays, so the flag still follows that speed and is not set
// because the job encodes nothing. Not lower after the pause than before it is
// checked in transcoding_speed_integration_test.go.
func TestSessionsDoNotFlagAJobPausedAtItsLead(t *testing.T) {
	l := startLive(t, "3", 6*time.Second)
	l.speedy(t)
	l.pausedAtItsLead(t)
	info := l.e.transcodingInfo(t, 1)
	if info == nil {
		t.Fatal("no TranscodingInfo for a paused job")
	}
	speed, ok := info["EncodeSpeed"].(float64)
	if !ok {
		t.Fatalf("a paused job lost its EncodeSpeed: %v", info)
	}
	t.Logf("a job paused at its lead holds EncodeSpeed %.2fx", speed)
	if flagged, _ := info["IsBelowRealTime"].(bool); flagged != belowOne(speed) {
		t.Errorf("a job paused at its lead limit is flagged %v at the held EncodeSpeed %v: %v", flagged, speed, info)
	}
}
