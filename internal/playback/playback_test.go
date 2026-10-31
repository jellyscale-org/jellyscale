package playback

import (
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

// capturedProfile is the DeviceProfile jellyfin-web 12.4 sent from Chrome,
// recorded in compat/captures/transcode-12.4.json.
func capturedProfile(t *testing.T) *DeviceProfile {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/transcode-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		DeviceProfile *DeviceProfile `json:"deviceProfile"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil || capture.DeviceProfile == nil {
		t.Fatalf("no deviceProfile in the capture: %v", err)
	}
	return capture.DeviceProfile
}

func stream(kind, codec string, index int) Stream {
	return Stream{Index: index, Type: kind, Codec: codec}
}

func film(container, videoCodec string, audioCodecs ...string) Source {
	video := Stream{Index: 0, Type: "Video", Codec: videoCodec, Profile: "High", Level: 41, VideoRangeType: "SDR", Width: 1280, Height: 720, AverageFrameRate: 24}
	source := Source{Container: container, Bitrate: 2_000_000, RunTimeTicks: 900_000_000, Streams: []Stream{video}}
	for i, codec := range audioCodecs {
		audio := stream("Audio", codec, i+1)
		audio.IsDefault = i == 0
		audio.Channels = 2
		source.Streams = append(source.Streams, audio)
	}
	return source
}

func request(t *testing.T, body string) Request {
	t.Helper()
	r, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	r.DeviceProfile = capturedProfile(t)
	return r
}

func TestDecideAgainstTheCapturedProfile(t *testing.T) {
	hdr := film("mkv", "h264", "aac")
	hdr.Streams[0].Profile = "High 4:4:4 Predictive"
	heavy := film("mp4", "h264", "aac")
	heavy.Bitrate = 8_000_000
	twoTracks := film("mkv", "h264", "aac", "aac")
	with := func(change func(*Stream)) Source {
		source := film("mp4", "h264", "aac")
		change(&source.Streams[0])
		return source
	}
	level53 := with(func(v *Stream) { v.Level = 53 })
	level52 := with(func(v *Stream) { v.Level = 52 })
	interlaced := with(func(v *Stream) { v.IsInterlaced = true })
	hdr10 := with(func(v *Stream) { v.VideoRangeType = "HDR10" })
	// Stored before level, field order and range were probed: plays directly
	// until the next scan fills them in.
	unprobed := with(func(v *Stream) { v.Level, v.VideoRangeType = 0, "" })

	tests := []struct {
		name    string
		source  Source
		body    string
		direct  bool
		reasons []string
		audio   int
	}{
		{"h264 and aac in mp4", film("mp4", "h264", "aac"), `{}`, true, nil, 1},
		{"h264 and aac in mkv", film("mkv", "h264", "aac"), `{}`, true, nil, 1},
		{"hevc", film("mkv", "hevc", "aac"), `{}`, false, []string{"VideoCodecNotSupported"}, 1},
		{"ac3 audio", film("mkv", "h264", "ac3"), `{}`, false, []string{"AudioCodecNotSupported"}, 1},
		{"hevc and ac3", film("mkv", "hevc", "ac3"), `{}`, false, []string{"VideoCodecNotSupported", "AudioCodecNotSupported"}, 1},
		{"container the profile lacks", film("avi", "h264", "aac"), `{}`, false, []string{"ContainerNotSupported"}, 1},
		{"a profile h264 doesn't allow", hdr, `{}`, false, []string{"VideoProfileNotSupported"}, 1},
		{"level 5.3", level53, `{}`, false, []string{"VideoLevelNotSupported"}, 1},
		{"level 5.2 is the limit", level52, `{}`, true, nil, 1},
		{"interlaced", interlaced, `{}`, false, []string{"InterlacedVideoNotSupported"}, 1},
		{"hdr10", hdr10, `{}`, false, []string{"VideoRangeTypeNotSupported"}, 1},
		{"scanned before the probe saw level and range", unprobed, `{}`, true, nil, 1},
		{"bitrate over the cap", heavy, `{"MaxStreamingBitrate": 720000}`, false, []string{"ContainerBitrateExceedsLimit"}, 1},
		{"bitrate under the cap", heavy, `{"MaxStreamingBitrate": 20000000}`, true, nil, 1},
		{"auto means no cap", heavy, `{"MaxStreamingBitrate": 2147483647}`, true, nil, 1},
		{"cap from the profile when the request has none", heavy, `{}`, true, nil, 1},
		{"audio track by number", twoTracks, `{"AudioStreamIndex": 2}`, false, nil, 2},
		{"audio track by string", twoTracks, `{"AudioStreamIndex": "2"}`, false, nil, 2},
		{"the default track by string", twoTracks, `{"AudioStreamIndex": "1"}`, true, nil, 1},
		{"a track that doesn't exist", twoTracks, `{"AudioStreamIndex": 9}`, true, nil, 1},
		{"subtitle index is ignored", film("mp4", "h264", "aac"), `{"SubtitleStreamIndex": "3"}`, true, nil, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := Decide(request(t, test.body), test.source, true)
			if d.DirectPlay != test.direct || !reflect.DeepEqual(d.Reasons, test.reasons) || d.AudioIndex != test.audio {
				t.Fatalf("got %+v", d)
			}
			if !d.DirectPlay && (d.VideoBitrate <= 0 || d.AudioBitrate <= 0) {
				t.Fatalf("a transcode needs bitrates: %+v", d)
			}
		})
	}
}

func TestDirectPlayStaysWhenThereIsNothingToDecideWith(t *testing.T) {
	hevc := film("mkv", "hevc", "aac")
	if d := Decide(Request{}, hevc, true); !d.DirectPlay {
		t.Error("a body with no DeviceProfile must stay direct play")
	}
	if d := Decide(request(t, `{}`), hevc, false); !d.DirectPlay {
		t.Error("with transcoding off the file is all there is")
	}
	noOutput := request(t, `{}`)
	noOutput.DeviceProfile.TranscodingProfiles = nil
	if d := Decide(noOutput, hevc, true); !d.DirectPlay {
		t.Error("a client that accepts no output we make gets the file")
	}
	audioOnly := Source{Container: "mkv", Streams: []Stream{stream("Audio", "flac", 0)}}
	if d := Decide(request(t, `{}`), audioOnly, true); !d.DirectPlay {
		t.Error("a file with no video can't be transcoded here")
	}
}

// The bitrate split is the shape the capture shows: the parts sum to the cap,
// and Auto leaves 2147483647 minus the audio.
func TestBitrateSplit(t *testing.T) {
	d := Decide(request(t, `{"MaxStreamingBitrate": 720000}`), func() Source { s := film("mp4", "h264", "aac"); s.Bitrate = 5_000_000; return s }(), true)
	if d.VideoBitrate+d.AudioBitrate != 720000 {
		t.Errorf("720k cap split into %d + %d", d.VideoBitrate, d.AudioBitrate)
	}
	d = Decide(request(t, `{"MaxStreamingBitrate": 2147483647}`), film("mkv", "hevc", "aac"), true)
	if d.VideoBitrate != 2147291647 || d.AudioBitrate != 192000 {
		t.Errorf("auto split into %d + %d, the capture has 2147291647 + 192000", d.VideoBitrate, d.AudioBitrate)
	}
}

func TestParseRequestAcceptsNumbersAndStrings(t *testing.T) {
	r, err := ParseRequest([]byte(`{"AudioStreamIndex":"1","SubtitleStreamIndex":-1,"StartTimeTicks":44550970,"MaxStreamingBitrate":720000,"MediaSourceId":"abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	if *r.AudioStreamIndex != 1 || *r.SubtitleStreamIndex != -1 || *r.StartTimeTicks != 44550970 || *r.MaxStreamingBitrate != 720000 || r.MediaSourceID != "abc" {
		t.Fatalf("got %+v", r)
	}
	if r, err := ParseRequest(nil); err != nil || r.DeviceProfile != nil {
		t.Fatalf("empty body: %+v %v", r, err)
	}
}

func params(t *testing.T, query string, source Source) (Params, error) {
	t.Helper()
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	return ParseParams(values, source)
}

const sourceID = "0123456789abcdef0123456789abcdef"

func TestParseParamsRejectsWhatIsNotAnInteger(t *testing.T) {
	source := film("mkv", "h264", "aac", "aac")
	good := "mediasourceid=" + sourceID + "&playsessionid=abc123&audiostreamindex=2&videobitrate=2000000&audiobitrate=128000"
	if p, err := params(t, good, source); err != nil || p.AudioIndex != 2 || p.VideoBitrate != 2000000 {
		t.Fatalf("good query: %+v %v", p, err)
	}
	for name, query := range map[string]string{
		"source id":       "mediasourceid=../../etc/passwd&playsessionid=abc",
		"source id dash":  "mediasourceid=" + sourceID + "z&playsessionid=abc",
		"audio index":     strings.Replace(good, "audiostreamindex=2", "audiostreamindex=1%3B-i+/etc/passwd", 1),
		"video stream":    strings.Replace(good, "audiostreamindex=2", "audiostreamindex=0", 1),
		"video bitrate":   strings.Replace(good, "videobitrate=2000000", "videobitrate=1000+-f+x", 1),
		"huge bitrate":    strings.Replace(good, "videobitrate=2000000", "videobitrate=9999999999999", 1),
		"tiny bitrate":    strings.Replace(good, "videobitrate=2000000", "videobitrate=1", 1),
		"audio bitrate":   strings.Replace(good, "audiobitrate=128000", "audiobitrate=-5", 1),
		"play session id": strings.Replace(good, "playsessionid=abc123", "playsessionid=abc%3Brm", 1),
		"no play session": "mediasourceid=" + sourceID,
		"start text":      good + "&starttimeticks=0%3Brm",
		"negative start":  good + "&starttimeticks=-1",
		"huge start":      good + "&starttimeticks=99999999999999999999",
	} {
		if p, err := params(t, query, source); err == nil {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
}

// Where the viewer starts is no part of the spec, but it says which segment a
// job should start at when the init segment, which comes first, is asked for.
func TestStartTimeTicksPicksTheSegmentAndLeavesTheSpec(t *testing.T) {
	source := film("mkv", "h264", "aac")
	at := func(query string) (Params, Spec) {
		p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&"+query, source)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := NewSpec("11111111-2222-3333-4444-555555555555", p, source)
		if err != nil {
			t.Fatal(err)
		}
		return p, spec
	}
	start, specA := at("starttimeticks=400000000") // 40 s.
	_, specB := at("")
	if start.StartSegment(Segments(source.Ticks())) != 10 || specA.Hash() != specB.Hash() {
		t.Errorf("segment %d, hashes %s %s", start.StartSegment(Segments(source.Ticks())), specA.Hash(), specB.Hash())
	}
	if far := (Params{StartTicks: MaxStartTicks}); far.StartSegment(5) != 4 {
		t.Errorf("a start past the end gave segment %d of 5", far.StartSegment(5))
	}
	if !strings.Contains(Query(start, "tok", "etag", ""), "&StartTimeTicks=400000000") || strings.Contains(Query(Params{}, "tok", "etag", ""), "StartTimeTicks") {
		t.Error("the query carries the start only when there is one")
	}
}

func TestSpecHash(t *testing.T) {
	source := film("mkv", "h264", "aac", "aac")
	source.ETag = "e1"
	spec := func(query string) Spec {
		p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&"+query, source)
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewSpec("11111111-2222-3333-4444-555555555555", p, source)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := spec("audiostreamindex=1")
	if base.Hash() != spec("audiostreamindex=1").Hash() {
		t.Error("the same request must hash the same")
	}
	// Auto and a 120 Mbps cap encode alike, so they share segments.
	if spec("videobitrate=2147291647").Hash() != spec("videobitrate=119808000").Hash() {
		t.Error("caps above what the picture can use must share a hash")
	}
	for name, other := range map[string]Spec{
		"audio track":   spec("audiostreamindex=2"),
		"video bitrate": spec("videobitrate=1000000"),
		"audio bitrate": spec("audiobitrate=64000"),
		"resolution":    spec("videobitrate=400000"),
	} {
		if other.Hash() == base.Hash() {
			t.Errorf("a different %s must hash differently", name)
		}
	}
	changed := source
	changed.ETag = "e2"
	p, _ := params(t, "mediasourceid="+sourceID+"&playsessionid=abc", changed)
	if s, _ := NewSpec("11111111-2222-3333-4444-555555555555", p, changed); s.Hash() == base.Hash() {
		t.Error("a changed file must not reuse old segments")
	}
}

func TestOutputSizeFollowsTheBitrate(t *testing.T) {
	video := Stream{Width: 1920, Height: 1080}
	for bitrate, want := range map[int64][2]int{
		50_000_000: {1920, 1080},
		1_500_000:  {1280, 720}, // A 1.56 Mpixel budget holds 720p, not 1080p.
		576_000:    {960, 540},
		200_000:    {426, 240},
	} {
		if w, h := outputSize(video, bitrate); w != want[0] || h != want[1] {
			t.Errorf("%d bps: %dx%d, want %dx%d", bitrate, w, h, want[0], want[1])
		}
	}
	if w, h := outputSize(Stream{Width: 853, Height: 481}, 50_000_000); w != 850 || h != 480 {
		t.Errorf("odd sizes: %dx%d", w, h)
	}
}

func TestOutputSizeNeverExceedsMaxHeight(t *testing.T) {
	if w, h := outputSize(Stream{Width: 3840, Height: 2160}, Unlimited); w != 1920 || h != 1080 {
		t.Errorf("4K at no cap: %dx%d, want 1920x1080", w, h)
	}
	if w, h := outputSize(Stream{Width: 4096, Height: 1716}, Unlimited); h != 1080 || w != 2576 {
		t.Errorf("scope 4K: %dx%d", w, h)
	}
}

func TestTicksStopAtTheEndOfTheVideo(t *testing.T) {
	// 20 s of video under 28 s of audio: the format runs 28 s, and ffmpeg
	// cuts segments only while video lasts, so the playlist must stop at 20 s.
	source := Source{RunTimeTicks: 280_000_000, Streams: []Stream{{Type: "Video", DurationTicks: 200_000_000}}}
	if got := Segments(source.Ticks()); got != 5 {
		t.Errorf("%d segments, want 5", got)
	}
	for name, s := range map[string]Source{
		"no video duration":     {RunTimeTicks: 280_000_000, Streams: []Stream{{Type: "Video"}}},
		"video longer than all": {RunTimeTicks: 280_000_000, Streams: []Stream{{Type: "Video", DurationTicks: 290_000_000}}},
	} {
		if s.Ticks() != 280_000_000 {
			t.Errorf("%s: %d", name, s.Ticks())
		}
	}
}

func TestSegmentCount(t *testing.T) {
	for seconds, want := range map[float64]int{0.5: 1, 4: 1, 4.5: 1, 5: 2, 8: 2, 90.021: 23, 90.9: 23} {
		if got := Segments(int64(seconds * ticksPerSecond)); got != want {
			t.Errorf("%.3f s: %d segments, want %d", seconds, got, want)
		}
	}
}

func TestParseSegment(t *testing.T) {
	for name, want := range map[string]struct {
		n  int
		ok bool
	}{
		"-1.mp4": {-1, true}, "0.mp4": {0, true}, "9.mp4": {9, true}, "10.mp4": {0, false}, "-2.mp4": {0, false},
		"1.ts": {0, false}, "../1.mp4": {0, false}, "1.mp4/x": {0, false}, ".mp4": {0, false}, "99999999.mp4": {0, false}, "+1.mp4": {0, false},
	} {
		if n, ok := ParseSegment(name, 10); ok != want.ok || (ok && n != want.n) {
			t.Errorf("%q: %d %v", name, n, ok)
		}
	}
}

func TestMainPlaylistIsCompleteVODWithoutDiscontinuities(t *testing.T) {
	text := Main(880_210_000, "q=1") // 88.021 s: 22 segments and a 21 ms tail
	for _, line := range []string{"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-VERSION:7", "#EXT-X-TARGETDURATION:5", "#EXT-X-ENDLIST", "#EXT-X-MAP:URI=\"hls1/main/-1.mp4?q=1&runtimeTicks=0&actualSegmentLengthTicks=0\""} {
		if !strings.Contains(text, line+"\n") && !strings.Contains(text, line) {
			t.Errorf("missing %q", line)
		}
	}
	for _, banned := range []string{"DISCONTINUITY", "BYTERANGE", "PROGRAM-DATE-TIME"} {
		if strings.Contains(text, banned) {
			t.Errorf("playlist has %s", banned)
		}
	}
	if got := strings.Count(text, "#EXTINF:"); got != 22 {
		t.Errorf("%d entries", got)
	}
	if !strings.Contains(text, "#EXTINF:4.000000, nodesc\nhls1/main/0.mp4?q=1&runtimeTicks=0&actualSegmentLengthTicks=40000000\n") {
		t.Errorf("first entry: %s", text)
	}
	// The 0.021 s tail joins the segment before it.
	if !strings.Contains(text, "#EXTINF:4.021000, nodesc\nhls1/main/21.mp4?q=1&runtimeTicks=840000000&actualSegmentLengthTicks=40210000\n") {
		t.Errorf("last entry: %s", text)
	}
	if !strings.Contains(Main(880_000_000, "q"), "#EXTINF:4.000000, nodesc\nhls1/main/21.mp4?q&runtimeTicks=840000000&actualSegmentLengthTicks=40000000") {
		t.Error("an exact multiple ends on a full segment")
	}
}

func TestMasterURLIsJellyfinShaped(t *testing.T) {
	p := Params{SourceID: sourceID, AudioIndex: 1, VideoBitrate: 650544, AudioBitrate: 69456, PlaySessionID: "abc", DeviceID: "dev 1"}
	got := MasterURL("11111111-2222-3333-4444-555555555555", p, "tok", "etag", Decision{Reasons: []string{"ContainerBitrateExceedsLimit"}}, 24)
	for _, part := range []string{"/videos/11111111-2222-3333-4444-555555555555/master.m3u8?DeviceId=dev+1&MediaSourceId=" + sourceID,
		"AudioStreamIndex=1&VideoBitrate=650544&AudioBitrate=69456", "SegmentContainer=mp4", "PlaySessionId=abc&ApiKey=tok", "MaxFramerate=24&TranscodeReasons=ContainerBitrateExceedsLimit"} {
		if !strings.Contains(got, part) {
			t.Errorf("missing %q in %s", part, got)
		}
	}
	if strings.Contains(got, "Signature") || strings.Contains(got, "Expires") {
		t.Error("TranscodingUrl must stay unsigned")
	}
}

func withSubtitles(source Source) Source {
	text, image := stream("Subtitle", "subrip", 10), stream("Subtitle", "hdmv_pgs_subtitle", 11)
	text.IsTextSubtitleStream = true
	source.Streams = append(source.Streams, text, image)
	return source
}

func TestDecideBurnsInSubtitlesOnlyWhenTheClientCantShowThem(t *testing.T) {
	source := withSubtitles(film("mkv", "h264", "aac"))
	none := request(t, `{"SubtitleStreamIndex": 10}`)
	none.DeviceProfile.SubtitleProfiles = nil // jellyfin-web sends none when the viewer burns all.
	always := request(t, `{"SubtitleStreamIndex": "10", "AlwaysBurnInSubtitleWhenTranscoding": true}`)
	capped := request(t, `{"SubtitleStreamIndex": 10, "MaxStreamingBitrate": 1000000, "AlwaysBurnInSubtitleWhenTranscoding": true}`)

	// A client that shows subtitles itself gets the file as it is.
	if d := Decide(request(t, `{"SubtitleStreamIndex": 10}`), source, true); !d.DirectPlay || d.Subtitle != nil {
		t.Errorf("client-rendered: %+v", d)
	}
	// With none to show, a chosen text track is drawn in, which takes a transcode.
	d := Decide(none, source, true)
	if d.DirectPlay || d.Subtitle == nil || *d.Subtitle != 10 || !reflect.DeepEqual(d.Reasons, []string{"SubtitleCodecNotSupported"}) {
		t.Errorf("no subtitle profile: %+v", d)
	}
	// No track chosen, or an image one, or one that doesn't exist: nothing to draw.
	for _, body := range []string{`{"SubtitleStreamIndex": -1}`, `{"SubtitleStreamIndex": 11}`, `{"SubtitleStreamIndex": 99}`, `{}`} {
		none := request(t, body)
		none.DeviceProfile.SubtitleProfiles = nil
		if d := Decide(none, source, true); !d.DirectPlay || d.Subtitle != nil {
			t.Errorf("%s: %+v", body, d)
		}
	}
	// The viewer's setting burns in only when the film is transcoded anyway.
	if d := Decide(always, source, true); !d.DirectPlay || d.Subtitle != nil {
		t.Errorf("always burn, direct play: %+v", d)
	}
	d = Decide(capped, source, true)
	if d.DirectPlay || d.Subtitle == nil || !reflect.DeepEqual(d.Reasons, []string{"ContainerBitrateExceedsLimit", "SubtitleCodecNotSupported"}) {
		t.Errorf("always burn, transcode: %+v", d)
	}
}

func TestSubtitleIsInTheSpecAndTheURL(t *testing.T) {
	source := withSubtitles(film("mkv", "h264", "aac"))
	source.ETag = "e1"
	spec := func(query string) (Spec, Params, error) {
		p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&"+query, source)
		if err != nil {
			return Spec{}, p, err
		}
		s, err := NewSpec("11111111-2222-3333-4444-555555555555", p, source)
		return s, p, err
	}
	plain, _, _ := spec("audiostreamindex=1")
	burnt, p, err := spec("audiostreamindex=1&subtitlestreamindex=10")
	if err != nil || burnt.Hash() == plain.Hash() {
		t.Fatalf("a burnt subtitle must change the hash: %v", err)
	}
	if off, _, _ := spec("audiostreamindex=1&subtitlestreamindex=-1"); off.Hash() != plain.Hash() {
		t.Error("-1 means none, as in the capture")
	}
	if other, _, _ := spec("audiostreamindex=1&subtitlestreamindex=10"); other.Hash() != burnt.Hash() {
		t.Error("the same request must hash the same")
	}
	// Without a subtitle the URL and hash are what they were before subtitles.
	if !strings.Contains(Query(p, "tok", "etag", ""), "AudioStreamIndex=1&SubtitleStreamIndex=10&VideoBitrate=") {
		t.Errorf("query: %s", Query(p, "tok", "etag", ""))
	}
	p.Subtitle = nil
	if strings.Contains(Query(p, "tok", "etag", ""), "SubtitleStreamIndex") {
		t.Error("a subtitle in the query that isn't one")
	}
	for _, query := range []string{"subtitlestreamindex=11", "subtitlestreamindex=1", "subtitlestreamindex=99", "subtitlestreamindex=x"} {
		if _, _, err := spec(query); err == nil {
			t.Errorf("%s accepted: only a text subtitle track can be drawn", query)
		}
	}
}
