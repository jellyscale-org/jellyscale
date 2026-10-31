package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

// transcodeEnv makes the film an HEVC and AC3 file the captured Chrome profile
// can't play, with a transcode store but no worker: segments are put in the
// store by hand, which is where a worker's output would be.
func transcodeEnv(t *testing.T, codec, audio string) (*env, store.Store, string) {
	t.Helper()
	e := newEnv(t)
	streams := `[{"Index":0,"Type":"Video","Codec":"` + codec + `","Profile":"Main","Level":41,"VideoRangeType":"SDR","Width":1280,"Height":720,"AverageFrameRate":24},
		{"Index":1,"Type":"Audio","Codec":"` + audio + `","IsDefault":true,"Channels":2},
		{"Index":2,"Type":"Audio","Codec":"aac","Channels":2},
		{"Index":3,"Type":"Subtitle","Codec":"subrip"}]`
	mustExec(t, e.pool, "UPDATE media_sources SET streams = $1, container = 'mkv'", streams)
	// 100 seconds at 2 Mbps, with the direct play fields the scanner writes.
	mustExec(t, e.pool, `UPDATE items SET run_time_ticks = 1000000000, dto = jsonb_set(dto, '{MediaSources,0}',
		(dto->'MediaSources'->0) || '{"SupportsDirectPlay":true,"SupportsDirectStream":true,"Bitrate":2000000}')`)
	mustExec(t, e.pool, "TRUNCATE transcode_jobs, transcode_nodes")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, slots) VALUES ('w1', 2)")
	segments := store.NewPosix(t.TempDir())
	for _, s := range e.apis {
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments}
	}
	return e, segments, db.CompactID(e.item)
}

func capturedProfile(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/transcode-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		DeviceProfile json.RawMessage `json:"deviceProfile"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture.DeviceProfile
}

const auth = `MediaBrowser Client="c", Device="d", DeviceId="dev1", Version="1", Token="%s"`

func (e *env) playbackInfo(t *testing.T, replica int, item string, body map[string]any) map[string]any {
	t.Helper()
	response, raw := e.do(t, replica, "POST", "/Items/"+item+"/PlaybackInfo", e.token, body,
		"Authorization", strings.Replace(auth, "%s", e.token, 1))
	if response.StatusCode != 200 {
		t.Fatalf("PlaybackInfo: %d %s", response.StatusCode, raw)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func firstSource(reply map[string]any) map[string]any {
	return reply["MediaSources"].([]any)[0].(map[string]any)
}

func TestPlaybackInfoDecidesFromTheDeviceProfile(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	profile := capturedProfile(t)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	reply := e.playbackInfo(t, 0, e.item, body(nil))
	media := firstSource(reply)
	if media["SupportsDirectPlay"] != false || media["SupportsDirectStream"] != false || media["SupportsTranscoding"] != true ||
		media["TranscodingSubProtocol"] != "hls" || media["TranscodingContainer"] != "mp4" || media["Container"] != "mkv" {
		t.Fatalf("hevc should transcode as the capture shows: %v", media)
	}
	transcodingURL := media["TranscodingUrl"].(string)
	parsed, err := url.Parse(transcodingURL)
	if err != nil || !strings.HasPrefix(parsed.Path, "/videos/"+db.DashedID(e.item)+"/master.m3u8") {
		t.Fatalf("TranscodingUrl %q", transcodingURL)
	}
	query := parsed.Query()
	if query.Get("PlaySessionId") != reply["PlaySessionId"] || query.Get("ApiKey") != e.token || query.Get("TranscodeReasons") != "VideoCodecNotSupported" ||
		query.Get("MediaSourceId") != source || query.Get("DeviceId") != "dev1" {
		t.Fatalf("query: %v", query)
	}
	if query.Get("VideoBitrate") != "2147291647" || query.Get("AudioBitrate") != "192000" {
		t.Fatalf("auto bitrate split: %v", query)
	}
	if strings.Contains(transcodingURL, "Signature") {
		t.Fatal("TranscodingUrl is signed")
	}
	// A new PlaybackInfo is a new session.
	if again := e.playbackInfo(t, 1, e.item, body(nil)); again["PlaySessionId"] == reply["PlaySessionId"] {
		t.Fatal("PlaySessionId repeated")
	}
	// All audio is offered; the DTO here lists no subtitle track, so none is offered.
	types := map[string]int{}
	for _, s := range media["MediaStreams"].([]any) {
		types[s.(map[string]any)["Type"].(string)]++
	}
	if types["Audio"] != 2 || types["Subtitle"] != 0 || types["Video"] != 1 {
		t.Fatalf("streams: %v", types)
	}

	// The quality menu's cap.
	capped := firstSource(e.playbackInfo(t, 0, e.item, body(map[string]any{"MaxStreamingBitrate": 720000, "StartTimeTicks": 44550970})))
	cappedQuery := mustQuery(t, capped["TranscodingUrl"].(string))
	if cappedQuery.Get("VideoBitrate") != "576000" || cappedQuery.Get("AudioBitrate") != "144000" {
		t.Fatalf("720 kbps split: %v", cappedQuery)
	}

	// Another audio track is another transcode, with the index as a number too.
	audio := mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, body(map[string]any{"AudioStreamIndex": 2})))["TranscodingUrl"].(string))
	if audio.Get("AudioStreamIndex") != "2" {
		t.Fatalf("audio index: %v", audio)
	}

	// Without a profile, or with transcoding off, nothing changes.
	for name, direct := range map[string]map[string]any{"no profile": {"MaxStreamingBitrate": 1000}, "empty body": nil} {
		media := firstSource(e.playbackInfo(t, 0, e.item, direct))
		if media["SupportsDirectPlay"] != true || media["TranscodingUrl"] != nil || media["TranscodingSubProtocol"] != "http" || media["SupportsTranscoding"] != true {
			t.Fatalf("%s: %v", name, media)
		}
	}
	for _, s := range e.apis {
		s.Transcode = nil
	}
	media = firstSource(e.playbackInfo(t, 0, e.item, body(nil)))
	if media["SupportsDirectPlay"] != true || media["TranscodingUrl"] != nil || media["SupportsTranscoding"] != false {
		t.Fatalf("transcoding off: %v", media)
	}
}

func TestPlaybackInfoKeepsDirectPlayWhenTheProfileAllowsIt(t *testing.T) {
	e, _, source := transcodeEnv(t, "h264", "aac")
	media := firstSource(e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "MediaSourceId": source}))
	if media["SupportsDirectPlay"] != true || media["SupportsDirectStream"] != true || media["TranscodingUrl"] != nil || media["TranscodingSubProtocol"] != "http" {
		t.Fatalf("h264 and aac must direct play: %v", media)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

// The playlists come from the stream replica, are complete, and carry what
// segment requests need to authenticate.
func TestHLSRoutes(t *testing.T) {
	e, segments, source := transcodeEnv(t, "hevc", "aac")
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	transcodingURL := firstSource(reply)["TranscodingUrl"].(string)
	query := mustQuery(t, transcodingURL)
	path := "/videos/" + db.DashedID(e.item)

	response, master := e.do(t, 0, "GET", transcodingURL, "", nil)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		!strings.Contains(string(master), "#EXT-X-STREAM-INF:BANDWIDTH=") || !strings.Contains(string(master), "\nmain.m3u8?") {
		t.Fatalf("master: %d %q\n%s", response.StatusCode, response.Header.Get("Content-Type"), master)
	}
	mainURL := path + "/main.m3u8?" + strings.TrimSpace(strings.SplitN(string(master), "main.m3u8?", 2)[1])
	response, main := e.do(t, 1, "GET", mainURL, "", nil)
	if response.StatusCode != 200 || !strings.Contains(string(main), "#EXT-X-ENDLIST") || strings.Contains(string(main), "DISCONTINUITY") ||
		strings.Count(string(main), "#EXTINF") != 25 || !strings.Contains(string(main), "apikey="+e.token) && !strings.Contains(string(main), "ApiKey="+e.token) {
		t.Fatalf("main: %d\n%s", response.StatusCode, main)
	}
	var segmentURL string
	for _, line := range strings.Split(string(main), "\n") {
		if strings.HasPrefix(line, "hls1/main/2.mp4?") {
			segmentURL = path + "/" + line
		}
	}
	if segmentURL == "" {
		t.Fatalf("no entry for segment 2:\n%s", main)
	}

	// The spec the server derives is what the worker will write under.
	values := mustQuery(t, segmentURL)
	values = lowerKeys(values)
	hash := specHash(t, e, values)
	if err := segments.Put(context.Background(), transcode.SegmentKey(hash, 2), strings.NewReader("segment two")); err != nil {
		t.Fatal(err)
	}
	if err := segments.Put(context.Background(), transcode.SegmentKey(hash, -1), strings.NewReader("the init")); err != nil {
		t.Fatal(err)
	}
	for replica := range 2 {
		response, body := e.do(t, replica, "GET", segmentURL, "", nil)
		if response.StatusCode != 200 || string(body) != "segment two" || response.Header.Get("Content-Type") != "video/mp4" {
			t.Fatalf("segment from replica %d: %d %q", replica, response.StatusCode, body)
		}
	}
	initURL := strings.Replace(segmentURL, "hls1/main/2.mp4", "hls1/main/-1.mp4", 1)
	if response, body := e.do(t, 0, "GET", initURL, "", nil); response.StatusCode != 200 || string(body) != "the init" {
		t.Fatalf("init: %d %q", response.StatusCode, body)
	}

	// Refusals.
	if response, _ := e.do(t, 0, "GET", strings.Replace(segmentURL, "ApiKey="+e.token, "ApiKey=wrong", 1), "", nil); response.StatusCode != 401 {
		t.Errorf("a wrong token got %d", response.StatusCode)
	}
	for name, bad := range map[string]string{
		"past the end":        strings.Replace(segmentURL, "main/2.mp4", "main/25.mp4", 1),
		"a negative number":   strings.Replace(segmentURL, "main/2.mp4", "main/-2.mp4", 1),
		"another extension":   strings.Replace(segmentURL, "main/2.mp4", "main/2.ts", 1),
		"a traversal":         strings.Replace(segmentURL, "main/2.mp4", "main/..%2F..%2F2.mp4", 1),
		"another playlist":    strings.Replace(segmentURL, "hls1/main/", "hls1/other/", 1),
		"another source":      strings.Replace(segmentURL, "MediaSourceId="+source, "MediaSourceId="+strings.Repeat("0", 32), 1),
		"a bad source":        strings.Replace(segmentURL, "MediaSourceId="+source, "MediaSourceId=..%2F..%2Fetc", 1),
		"a bad bitrate":       strings.Replace(segmentURL, "VideoBitrate="+query.Get("VideoBitrate"), "VideoBitrate=1%3B-i%20%2Fetc%2Fpasswd", 1),
		"a video stream":      strings.Replace(segmentURL, "AudioStreamIndex=1", "AudioStreamIndex=0", 1),
		"a subtitle stream":   strings.Replace(segmentURL, "AudioStreamIndex=1", "AudioStreamIndex=3", 1),
		"a bad play session":  strings.Replace(segmentURL, "PlaySessionId="+query.Get("PlaySessionId"), "PlaySessionId=a%20b", 1),
		"no media source":     strings.Replace(segmentURL, "MediaSourceId="+source, "", 1),
		"an unknown item":     strings.Replace(segmentURL, db.DashedID(e.item), "11111111-2222-3333-4444-555555555555", 1),
		"the stream route":    path + "/main.mp4?ApiKey=" + e.token,
		"a bad item":          strings.Replace(segmentURL, db.DashedID(e.item), "not-an-item", 1),
		"the other video url": "/videos/" + db.DashedID(e.item) + "/hls1/main?ApiKey=" + e.token,
	} {
		if response, body := e.do(t, 0, "GET", bad, "", nil); response.StatusCode < 400 || response.StatusCode >= 500 {
			t.Errorf("%s: %d %s", name, response.StatusCode, body)
		}
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_jobs"); n != 0 {
		t.Errorf("refused requests started %d jobs", n)
	}

	// Stopping the session, as the player and as DELETE ActiveEncodings do.
	if response, _ := e.do(t, 0, "DELETE", "/Videos/ActiveEncodings?deviceId=dev1&PlaySessionId="+query.Get("PlaySessionId"), e.token, nil); response.StatusCode != 204 {
		t.Fatalf("DELETE ActiveEncodings: %d", response.StatusCode)
	}
	if response, _ := e.do(t, 1, "DELETE", "/Videos/ActiveEncodings?deviceId=dev1&PlaySessionId="+query.Get("PlaySessionId"), e.token, nil); response.StatusCode != 204 {
		t.Fatalf("the second DELETE: %d", response.StatusCode)
	}
	if response, _ := e.do(t, 0, "GET", segmentURL, "", nil); response.StatusCode != 410 {
		t.Errorf("a request after the session was stopped got %d", response.StatusCode)
	}
	if response, _ := e.do(t, 0, "DELETE", "/Videos/ActiveEncodings?deviceId=dev1", "", nil); response.StatusCode != 401 {
		t.Errorf("DELETE without a token: %d", response.StatusCode)
	}
}

func lowerKeys(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[strings.ToLower(k)] = vs
	}
	return out
}

// specHash derives the hash the way the server does.
func specHash(t *testing.T, e *env, values url.Values) string {
	t.Helper()
	source, found, err := e.apis[0].Items.Source(context.Background(), e.item, values.Get("mediasourceid"))
	if err != nil || !found {
		t.Fatal(err)
	}
	media, err := playbackSource(source)
	if err != nil {
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
	return spec.Hash()
}

// Stopped reports end the transcode of their PlaySessionId and nobody else's.
func TestStoppedEndsTheViewer(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	session := reply["PlaySessionId"].(string)
	mainURL := strings.Replace(firstSource(reply)["TranscodingUrl"].(string), "master.m3u8", "main.m3u8", 1)
	headers := []string{"Authorization", strings.Replace(auth, "%s", e.token, 1)}
	if response, _ := e.do(t, 0, "GET", mainURL, "", nil, headers...); response.StatusCode != 200 {
		t.Fatalf("main: %d", response.StatusCode)
	}
	other := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	otherMain := strings.Replace(firstSource(other)["TranscodingUrl"].(string), "master.m3u8", "main.m3u8", 1)
	if response, _ := e.do(t, 0, "GET", otherMain, "", nil, headers...); response.StatusCode != 200 {
		t.Fatalf("second session's main: %d", response.StatusCode)
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_viewers WHERE NOT stopped"); n != 2 {
		t.Fatalf("%d live viewers, want 2", n)
	}

	// A report with another user's session id changes nothing.
	response, _ := e.do(t, 0, "POST", "/Sessions/Playing/Stopped", e.otherToken,
		map[string]any{"ItemId": e.item, "PositionTicks": 0, "PlaySessionId": session})
	if response.StatusCode != 204 || scalar[int](t, e.pool, "SELECT count(*) FROM transcode_viewers WHERE NOT stopped") != 2 {
		t.Fatalf("another user's Stopped affected the viewer (%d)", response.StatusCode)
	}
	began := time.Now()
	response, _ = e.do(t, 1, "POST", "/Sessions/Playing/Stopped", e.token,
		map[string]any{"ItemId": e.item, "PositionTicks": 0, "PlaySessionId": session})
	if response.StatusCode != 204 {
		t.Fatalf("Stopped: %d", response.StatusCode)
	}
	// Jellyfin took 1.5 to 3.6 s, waiting for ffmpeg to exit; this only marks the viewer.
	t.Logf("Stopped answered in %v", time.Since(began).Round(time.Millisecond))
	if time.Since(began) > time.Second {
		t.Errorf("Stopped took %v", time.Since(began))
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_viewers WHERE NOT stopped"); n != 1 {
		t.Fatalf("%d live viewers after Stopped, want the other session's one", n)
	}
	if response, _ := e.do(t, 0, "GET", mainURL, "", nil, headers...); response.StatusCode != 410 {
		t.Errorf("a stopped session's request got %d", response.StatusCode)
	}
	if response, _ := e.do(t, 0, "GET", otherMain, "", nil, headers...); response.StatusCode != 200 {
		t.Errorf("the other session's request got %d", response.StatusCode)
	}
}

// A segment nobody can make is an error the client sees, not a hang, and the
// log says why.
func TestSegmentWithoutAWorkerAnswers503(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	for _, s := range e.apis {
		s.Transcode.WaitTimeout = 20 * time.Second
		s.Transcode.QueueTimeout = 2 * time.Second
	}
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	transcodingURL := firstSource(reply)["TranscodingUrl"].(string)
	segmentURL := strings.Replace(transcodingURL, "master.m3u8", "hls1/main/0.mp4", 1)
	// The worker is lost after the viewer was offered the stream. A player
	// gives up on a request after 10 seconds, so the answer must come sooner,
	// or it asks again for ever and shows nothing. It comes after the queue
	// timeout, not at once, so a worker that is restarting can be replaced.
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")
	began := time.Now()
	response, _ := e.do(t, 0, "GET", segmentURL, "", nil)
	if response.StatusCode != 503 || response.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d", response.StatusCode)
	}
	if took := time.Since(began); took < time.Second || took > 4*time.Second {
		t.Fatalf("took %v, want about the 2 second queue timeout", took)
	}
}

// transcodeProfileBody is what the captured Chrome sends for the hevc film.
func transcodeProfileBody(t *testing.T, source string) map[string]any {
	return map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source}
}

// jellyfin-web retries a stream that fails without end, so a transcode that
// can't start is refused at PlaybackInfo, with the error it shows a viewer.
func TestPlaybackInfoRefusesATranscodeThatCannotStart(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	refused := func(why string) {
		t.Helper()
		reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
		if reply["ErrorCode"] != "RateLimitExceeded" || len(reply["MediaSources"].([]any)) != 0 {
			t.Fatalf("%s: got %v", why, reply)
		}
	}
	offered := func(why string) {
		t.Helper()
		reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
		if reply["ErrorCode"] != nil || firstSource(reply)["TranscodingUrl"] == nil {
			t.Fatalf("%s: got %v", why, reply)
		}
	}
	offered("a worker is up")
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)))["TranscodingUrl"].(string))))

	// A worker that has stopped reporting is no worker.
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 minute'")
	refused("no live worker")
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now()")
	offered("worker back")

	// All slots busy refuses a spec nobody is already making...
	for i := range 2 {
		mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
			VALUES ($1, '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w1')`, "other"+string(rune('a'+i)))
	}
	refused("slots busy")
	// ...but joins a transcode of its own spec that already runs.
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
		VALUES ($1, '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w1')`, hash)
	offered("same spec already running")

	// A recent failure of the spec is not offered again.
	mustExec(t, e.pool, "TRUNCATE transcode_jobs")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, ended_at)
		VALUES ($1, '{}', 'o', 0, 0, 'failed', 'exec: "ffmpeg": executable file not found', now())`, hash)
	refused("recent failure")
	// ...unless another viewer's job for the spec is running.
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
		VALUES ($1, '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w1')`, hash)
	offered("a live job beats a recent failure")
	mustExec(t, e.pool, "DELETE FROM transcode_jobs WHERE state = 'running'")
	mustExec(t, e.pool, "UPDATE transcode_jobs SET ended_at = now() - interval '5 minutes'")
	offered("old failure")

	// Direct play never needs a worker.
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"MediaSourceId": source})
	if reply["ErrorCode"] != nil || firstSource(reply)["SupportsDirectPlay"] != true {
		t.Fatalf("direct play without a worker: %v", reply)
	}
}

// A replica without transcodeStore answers 503 where it is asked for a stream,
// and its empty store, registered beside the worker's, makes every other
// replica refuse to offer the transcode, so the viewer gets a dialog instead of
// asking again for a stream that fails.
func TestStreamReplicaWithoutTranscodeStoreSaysSo(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	master := firstSource(reply)["TranscodingUrl"].(string)
	e.apis[1].Transcode = nil
	if response, _ := e.do(t, 1, "GET", master, "", nil); response.StatusCode != 503 {
		t.Fatalf("got %d", response.StatusCode)
	}
	mustExec(t, e.pool, "UPDATE transcode_nodes SET roles = 'worker', store = 'file:///segments'")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles) VALUES ('replica', 'stream')")
	if reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("PlaybackInfo still offers it: %v", reply)
	}
}

// The item DTO's own streams, subtitles included, stay in a reply whether the
// source plays as it is or transcodes; only the audio list grows.
func TestPlaybackInfoKeepsTheStreamsTheItemListed(t *testing.T) {
	for _, codec := range []string{"h264", "hevc"} {
		e, _, source := transcodeEnv(t, codec, "aac")
		mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources,0,MediaStreams}',
			'[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Audio","IsDefault":true},{"Index":3,"Type":"Subtitle"}]')`)
		media := firstSource(e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)))
		if (media["TranscodingUrl"] != nil) != (codec == "hevc") {
			t.Fatalf("%s: %v", codec, media)
		}
		var types []string
		for _, entry := range media["MediaStreams"].([]any) {
			stream := entry.(map[string]any)
			// What the decision stores and the item page never listed stays out.
			if stream["Type"] == "Video" && len(stream) != 2 {
				t.Errorf("%s: the video stream gained fields: %v", codec, stream)
			}
			types = append(types, stream["Type"].(string)+":"+strconv.Itoa(int(stream["Index"].(float64))))
		}
		if want := "Video:0 Audio:1 Audio:2 Subtitle:3"; strings.Join(types, " ") != want {
			t.Errorf("%s: streams %v, want %s", codec, types, want)
		}
	}
}

// A viewer who picks another audio track while no worker could take the
// transcode keeps the default track if it plays as it is, whichever codec the
// pick has, and gets a refusal otherwise, never a player that can't play.
func TestAudioSwitchWithoutRoomKeepsPlaying(t *testing.T) {
	pick := func(e *env, source string) (map[string]any, string) {
		mustExec(t, e.pool, "TRUNCATE transcode_nodes")
		var logged strings.Builder
		e.apis[0].Log = slog.New(slog.NewTextHandler(&logged, nil))
		body := transcodeProfileBody(t, source)
		body["AudioStreamIndex"] = 2
		return e.playbackInfo(t, 0, e.item, body), logged.String()
	}
	for _, second := range []string{"aac", "ac3"} {
		e, _, source := transcodeEnv(t, "h264", "aac")
		mustExec(t, e.pool, "UPDATE media_sources SET streams = jsonb_set(streams, '{2,Codec}', to_jsonb($1::text))", second)
		reply, log := pick(e, source)
		media := firstSource(reply)
		if reply["ErrorCode"] != nil || media["SupportsDirectPlay"] != true || media["TranscodingUrl"] != nil || media["DefaultAudioStreamIndex"] != float64(1) {
			t.Fatalf("second track %s: got %v", second, reply)
		}
		if !strings.Contains(log, "no transcode worker is running") {
			t.Errorf("second track %s: the log doesn't say why:\n%s", second, log)
		}
	}

	// A film that needs the transcode anyway is refused, whatever track was
	// picked: its default track does not play as it is.
	for _, codec := range []string{"hevc", "h264"} {
		audio := "aac"
		if codec == "h264" {
			audio = "ac3"
		}
		e, _, source := transcodeEnv(t, codec, audio)
		if reply, _ := pick(e, source); reply["ErrorCode"] != "RateLimitExceeded" || len(reply["MediaSources"].([]any)) != 0 {
			t.Fatalf("%s and %s: %v", codec, audio, reply)
		}
	}
}

// A replica whose transcodeStore differs from the worker's, or is missing,
// is found by comparing what each role registered. PlaybackInfo refuses the
// transcode and the log names both.
func TestTranscodeStoreMismatchIsNamed(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	mustExec(t, e.pool, "UPDATE transcode_nodes SET roles = 'worker', store = 's3://bucket/segments?endpoint=http://s3'")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, store) VALUES ('replica', 'api,stream', 'file:///elsewhere')")
	var logged strings.Builder
	e.apis[0].Log = slog.New(slog.NewTextHandler(&logged, nil))
	if reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("different store: %v", reply)
	}
	for _, want := range []string{"s3://bucket/segments", "file:///elsewhere", "api,stream", "worker", "must be the same"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logged.String())
		}
	}

	// A replica with no transcodeStore at all, which registers an empty one.
	logged.Reset()
	mustExec(t, e.pool, "UPDATE transcode_nodes SET store = '' WHERE id = 'replica'")
	e.apis[0].Transcode = nil
	if reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("no store: %v", reply)
	}
	if !strings.Contains(logged.String(), "api,stream has no transcodeStore") {
		t.Errorf("the log doesn't say the store is missing:\n%s", logged.String())
	}
}

// A queued job older than the queue timeout is not live: no worker took it, so
// offering it would hand the viewer a stream that never starts.
func TestStaleQueuedJobIsNotOffered(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)))["TranscodingUrl"].(string))))
	mustExec(t, e.pool, "DELETE FROM transcode_jobs")
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 hour'")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, created_at)
		VALUES ($1, '{}', 'o', 0, 0, 'queued', now() - interval '1 minute')`, hash)
	if reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("a queued job nobody will take was offered: %v", reply)
	}
}

// A live job is offered by any replica (invariant 37), also one with no
// transcodeStore, and when the stores differ the log names both.
func TestLiveJobIsOfferedWithoutATranscodeStore(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)))["TranscodingUrl"].(string))))
	mustExec(t, e.pool, "UPDATE transcode_nodes SET roles = 'worker', store = 'file:///segments'")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, store) VALUES ('replica', 'api,stream', '')")
	var logged strings.Builder
	e.apis[0].Log = slog.New(slog.NewTextHandler(&logged, nil))
	e.apis[0].Transcode = nil
	if reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("no job, stores differ: %v", reply)
	}
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
		VALUES ($1, '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w1')`, hash)
	logged.Reset()
	reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	if reply["ErrorCode"] != nil || firstSource(reply)["TranscodingUrl"] == nil {
		t.Fatalf("a live job was not offered: %v", reply)
	}
	// jellyfin-web ignores TranscodingUrl unless this is set.
	if firstSource(reply)["SupportsTranscoding"] != true {
		t.Errorf("SupportsTranscoding: %v", firstSource(reply)["SupportsTranscoding"])
	}
	for _, want := range []string{"level=WARN", "file:///segments", "api,stream has no transcodeStore"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logged.String())
		}
	}
}

// A Stopped report with no PlaySessionId ends the device's transcode of that
// item, and nothing else.
func TestStoppedWithoutASessionEndsTheDevicesViewer(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	reply := e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	headers := []string{"Authorization", strings.Replace(auth, "%s", e.token, 1)}
	mainURL := strings.Replace(firstSource(reply)["TranscodingUrl"].(string), "master.m3u8", "main.m3u8", 1)
	if response, _ := e.do(t, 0, "GET", mainURL, "", nil, headers...); response.StatusCode != 200 {
		t.Fatalf("main: %d", response.StatusCode)
	}
	mustExec(t, e.pool, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, item_id)
		VALUES ('elsewhere', 'h', $1, 'other-device', $2), ('other-item', 'h2', $1, 'dev1', 'ffffffffffffffffffffffffffffffff')`, e.user, db.CompactID(e.item))
	live := func() int { return scalar[int](t, e.pool, "SELECT count(*) FROM transcode_viewers WHERE NOT stopped") }
	if live() != 3 {
		t.Fatalf("%d live viewers, want 3", live())
	}
	response, _ := e.do(t, 0, "POST", "/Sessions/Playing/Stopped", e.token, map[string]any{"ItemId": e.item, "PositionTicks": 0},
		headers...)
	if response.StatusCode != 204 {
		t.Fatalf("Stopped: %d", response.StatusCode)
	}
	if live() != 2 || scalar[int](t, e.pool, "SELECT count(*) FROM transcode_viewers WHERE stopped AND play_session_id = $1",
		reply["PlaySessionId"]) != 1 {
		t.Fatalf("%d live viewers: the device's viewer of this item should be the one stopped", live())
	}
}

// A resume's TranscodingUrl carries where the viewer starts, so the init
// segment, which jellyfin-web asks for before any media segment, starts the
// job there and not at the beginning.
func TestInitSegmentStartsTheJobWhereTheViewerResumes(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	for _, s := range e.apis {
		s.Transcode.WaitTimeout = 500 * time.Millisecond // No worker runs in this test.
	}
	body := transcodeProfileBody(t, source)
	body["StartTimeTicks"] = 400000000 // 40 s, segment 10.
	transcodingURL := firstSource(e.playbackInfo(t, 0, e.item, body))["TranscodingUrl"].(string)
	if mustQuery(t, transcodingURL).Get("StartTimeTicks") != "400000000" {
		t.Fatalf("TranscodingUrl: %s", transcodingURL)
	}
	headers := []string{"Authorization", strings.Replace(auth, "%s", e.token, 1)}
	mainPlaylist, _ := e.do(t, 0, "GET", strings.Replace(transcodingURL, "master.m3u8", "main.m3u8", 1), "", nil, headers...)
	if mainPlaylist.StatusCode != 200 {
		t.Fatalf("main: %d", mainPlaylist.StatusCode)
	}
	e.do(t, 0, "GET", strings.Replace(transcodingURL, "master.m3u8", "hls1/main/-1.mp4", 1), "", nil, headers...)
	if start := scalar[int](t, e.pool, "SELECT start_segment FROM transcode_jobs"); start != 10 {
		t.Fatalf("the init segment started a job at %d, want the resume point, 10", start)
	}
	// Without it, a job starts at the film's beginning.
	mustExec(t, e.pool, "TRUNCATE transcode_jobs")
	plain := firstSource(e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source)))["TranscodingUrl"].(string)
	e.do(t, 0, "GET", strings.Replace(plain, "master.m3u8", "hls1/main/-1.mp4", 1), "", nil, headers...)
	if start := scalar[int](t, e.pool, "SELECT start_segment FROM transcode_jobs"); start != 0 {
		t.Fatalf("started at %d", start)
	}
}
