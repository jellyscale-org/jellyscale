package playback

import (
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"slices"
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
	if start.StartSegment(EncodedLayout(source.Ticks())) != 10 || specA.Hash() != specB.Hash() {
		t.Errorf("segment %d, hashes %s %s", start.StartSegment(EncodedLayout(source.Ticks())), specA.Hash(), specB.Hash())
	}
	if far := (Params{StartTicks: MaxStartTicks}); far.StartSegment(EncodedLayout(200_000_000)) != 4 {
		t.Errorf("a start past the end gave segment %d of 5", far.StartSegment(EncodedLayout(200_000_000)))
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
	if got := EncodedLayout(source.Ticks()).Count(); got != 5 {
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
		if got := EncodedLayout(int64(seconds * ticksPerSecond)).Count(); got != want {
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
	text := Main(EncodedLayout(880_210_000), "q=1") // 88.021 s: 22 segments and a 21 ms tail
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
	if !strings.Contains(Main(EncodedLayout(880_000_000), "q"), "#EXTINF:4.000000, nodesc\nhls1/main/21.mp4?q&runtimeTicks=840000000&actualSegmentLengthTicks=40000000") {
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

// withSubtitles adds a text track (10), a bitmap one that nothing draws (11), and
// the image tracks a transcode can draw: PGS (12) and VobSub (13).
func withSubtitles(source Source) Source {
	text, unlisted := stream("Subtitle", "subrip", 10), stream("Subtitle", "hdmv_text_subtitle", 11)
	pgs, vobsub := stream("Subtitle", "PGSSUB", 12), stream("Subtitle", "DVDSUB", 13)
	text.IsTextSubtitleStream = true
	pgs.IsImageSubtitleStream, vobsub.IsImageSubtitleStream = true, true
	source.Streams = append(source.Streams, text, unlisted, pgs, vobsub)
	return source
}

// An image track is drawn into a transcode unless the client draws it: only
// PGS, and only for a client whose profile says it renders PGS.
func TestDecideBurnsInImageSubtitles(t *testing.T) {
	source := withSubtitles(film("mkv", "h264", "aac"))
	renders := func(body string) Request {
		r := request(t, body)
		r.DeviceProfile.SubtitleProfiles = append(r.DeviceProfile.SubtitleProfiles,
			SubtitleProfile{Format: "pgssub", Method: "External"}, SubtitleProfile{Format: "vobsub", Method: "External"})
		return r
	}
	burnt := func(name string, r Request, index int) {
		t.Helper()
		d := Decide(r, source, true)
		if d.DirectPlay || d.Subtitle == nil || *d.Subtitle != index || !reflect.DeepEqual(d.Reasons, []string{"SubtitleCodecNotSupported"}) {
			t.Errorf("%s: %+v", name, d)
		}
	}
	burnt("PGS for a client that doesn't render it", request(t, `{"SubtitleStreamIndex": 12}`), 12)
	burnt("VobSub for a client that doesn't render it", request(t, `{"SubtitleStreamIndex": "13"}`), 13)
	// Jellyscale sends VobSub as nothing but a picture, whatever the profile says.
	burnt("VobSub for a client that renders it", renders(`{"SubtitleStreamIndex": 13}`), 13)
	if d := Decide(renders(`{"SubtitleStreamIndex": 12}`), source, true); !d.DirectPlay || d.Subtitle != nil {
		t.Errorf("PGS for a client that renders it: %+v", d)
	}
	// A client that renders PGS gets it drawn in when the viewer's setting says so
	// and the film is transcoded anyway.
	always := renders(`{"SubtitleStreamIndex": 12, "MaxStreamingBitrate": 1000000, "AlwaysBurnInSubtitleWhenTranscoding": true}`)
	if d := Decide(always, source, true); d.DirectPlay || d.Subtitle == nil || *d.Subtitle != 12 {
		t.Errorf("always burn, PGS: %+v", d)
	}
	// With no subtitle profile at all it is drawn in, as a text track is.
	none := request(t, `{"SubtitleStreamIndex": 12}`)
	none.DeviceProfile.SubtitleProfiles = nil
	burnt("PGS for a client with no subtitle profile", none, 12)
	// A bitmap codec that isn't listed has nothing to draw it with.
	if d := Decide(request(t, `{"SubtitleStreamIndex": 11}`), source, true); !d.DirectPlay || d.Subtitle != nil {
		t.Errorf("an unlisted bitmap codec: %+v", d)
	}
	// A chosen image track forces a re-encode even when the client plays the video.
	copied := withSubtitles(copyable("mkv", "ac3"))
	if d := Decide(request(t, `{"SubtitleStreamIndex": 12}`), copied, true); d.Copy || d.Subtitle == nil {
		t.Errorf("a copied stream can't have a track drawn in: %+v", d)
	}
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
			t.Errorf("%s accepted: only a text or listed image subtitle track can be drawn", query)
		}
	}
	// An image track is another picture from the text one, and from the other image track.
	pgs, _, err := spec("audiostreamindex=1&subtitlestreamindex=12")
	vobsub, _, err2 := spec("audiostreamindex=1&subtitlestreamindex=13")
	if err != nil || err2 != nil || pgs.Subtitle == nil || *pgs.Subtitle != 12 || pgs.Hash() == burnt.Hash() || pgs.Hash() == vobsub.Hash() || pgs.Hash() == plain.Hash() {
		t.Errorf("image tracks: %v %v %+v", err, err2, pgs)
	}
}

// seconds turns keyframe times into ticks.
func seconds(times ...float64) []int64 {
	ticks := make([]int64, len(times))
	for i, t := range times {
		ticks[i] = int64(t * ticksPerSecond)
	}
	return ticks
}

// capturedKeyframes are those of the fixture in compat/captures/transcode-12.4.md.
var capturedKeyframes = seconds(0, 3, 7, 13, 18, 26, 31, 39, 44, 52, 57, 65, 70, 78, 83)

func TestCopyLayoutCutsOnKeyframes(t *testing.T) {
	layout, ok := CopyLayout(capturedKeyframes, 900_000_000)
	if !ok {
		t.Fatal("the captured keyframes make a layout")
	}
	// The captured playlist: the 3 s keyframe skipped, every later one a cut.
	var lengths []float64
	for n := range layout.Count() {
		_, length := layout.Segment(n)
		lengths = append(lengths, float64(length)/ticksPerSecond)
	}
	if want := []float64{7, 6, 5, 8, 5, 8, 5, 8, 5, 8, 5, 8, 5, 7}; !reflect.DeepEqual(lengths, want) {
		t.Errorf("lengths %v, want %v", lengths, want)
	}
	// Everything counts from the first keyframe, which need not be 0.
	late, _ := CopyLayout(seconds(600, 603, 607), 100_000_000)
	if start, length := late.Segment(1); start != 70_000_000 || length != 30_000_000 {
		t.Errorf("shifted layout: segment 1 at %d for %d", start, length)
	}
	// The playlist says so: the sum of lengths is the video, and the target is
	// the longest segment rounded up.
	text := Main(layout, "q")
	if !strings.Contains(text, "#EXT-X-TARGETDURATION:8\n") || strings.Count(text, "#EXTINF:") != 14 {
		t.Errorf("playlist:\n%s", text)
	}
	if !strings.Contains(text, "#EXTINF:8.000000, nodesc\nhls1/main/3.mp4?q&runtimeTicks=180000000&actualSegmentLengthTicks=80000000\n") {
		t.Errorf("segment 3:\n%s", text)
	}
	if layout.At(0) != 0 || layout.At(69_999_999) != 0 || layout.At(70_000_000) != 1 || layout.At(1<<60) != 13 {
		t.Errorf("At: %d %d %d %d", layout.At(0), layout.At(69_999_999), layout.At(70_000_000), layout.At(1<<60))
	}
}

func TestCopyLayoutRefusesWhatItCannotCut(t *testing.T) {
	for name, c := range map[string]struct {
		keyframes []int64
		end       int64
		ok        bool
	}{
		"no keyframes":           {nil, 900_000_000, false},
		"a gap over the limit":   {seconds(0, 3, 20, 24), 300_000_000, false},
		"a tail over the limit":  {seconds(0, 4, 8), 300_000_000, false},
		"the limit itself":       {seconds(0, 12), 240_000_000, true},
		"gaps 3 and 11":          {seconds(0, 3, 14, 17, 28), 300_000_000, true},
		"a scene cut and a GOP":  {seconds(0, 2, 12.4, 22.8, 33.2), 440_000_000, true},
		"keyframes 3.75 s apart": {seconds(0, 3.75, 7.5, 11.25, 15), 150_000_000, true},
	} {
		if _, ok := CopyLayout(c.keyframes, c.end); ok != c.ok {
			t.Errorf("%s: ok %v", name, ok)
		}
	}
}

// A segment is cut short of 4 s only to stay within MaxCopySegment, and then no
// segment is over it, though one can be longer than any gap between keyframes.
func TestCopyLayoutKeepsSegmentsWithinTheLimit(t *testing.T) {
	layout, ok := CopyLayout(seconds(0, 3, 14, 17, 28), 300_000_000)
	var lengths []float64
	for n := range layout.Count() {
		_, length := layout.Segment(n)
		lengths = append(lengths, float64(length)/ticksPerSecond)
	}
	if want := []float64{3, 11, 3, 11, 2}; !ok || !reflect.DeepEqual(lengths, want) {
		t.Errorf("ok %v, lengths %v, want %v", ok, lengths, want)
	}
	// Two gaps under the limit make a segment longer than either.
	layout, _ = CopyLayout(seconds(0, 3.75, 7.5, 11.25), 150_000_000)
	if _, length := layout.Segment(0); length != 75_000_000 {
		t.Errorf("segment 0 is %d ticks, want 7.5 s", length)
	}
}

func copyable(container string, audio ...string) Source {
	source := film(container, "h264", audio...)
	source.Keyframes = capturedKeyframes
	source.VideoEnd = 900_000_000
	source.RunTimeTicks = 900_000_000
	source.Size = 22_500_000
	return source
}

func TestDecideCopiesVideoTheClientPlays(t *testing.T) {
	hevc := copyable("mkv", "ac3")
	hevc.Streams[0].Codec = "hevc"
	level53 := copyable("mkv", "ac3")
	level53.Streams[0].Level = 53
	interlaced := copyable("mkv", "ac3")
	interlaced.Streams[0].IsInterlaced = true
	hdr := copyable("mkv", "ac3")
	hdr.Streams[0].VideoRangeType = "HDR10"
	noIndex := copyable("mkv", "ac3")
	noIndex.Keyframes = nil
	emptyIndex := copyable("mkv", "ac3")
	emptyIndex.Keyframes = []int64{}
	longGap := copyable("mkv", "ac3")
	longGap.Keyframes = seconds(0, 30, 60, 90)
	heavy := copyable("mkv", "ac3")
	heavy.Bitrate = 8_000_000
	noBitrate := copyable("mkv", "ac3")
	noBitrate.Bitrate = 0

	for _, test := range []struct {
		name    string
		source  Source
		body    string
		copy    bool
		reasons []string
	}{
		{"ac3 in mkv", copyable("mkv", "ac3"), `{}`, true, []string{"AudioCodecNotSupported"}},
		{"ac3 in mp4", copyable("mp4", "ac3"), `{}`, true, []string{"AudioCodecNotSupported"}},
		{"a container the client lacks", copyable("avi", "aac"), `{}`, true, []string{"ContainerNotSupported"}},
		{"another audio track", copyable("mkv", "aac", "aac"), `{"AudioStreamIndex": 2}`, true, nil},
		{"hevc is re-encoded", hevc, `{}`, false, []string{"VideoCodecNotSupported", "AudioCodecNotSupported"}},
		{"a level the profile refuses", level53, `{}`, false, []string{"AudioCodecNotSupported", "VideoLevelNotSupported"}},
		{"interlaced", interlaced, `{}`, false, []string{"AudioCodecNotSupported", "InterlacedVideoNotSupported"}},
		{"hdr", hdr, `{}`, false, []string{"AudioCodecNotSupported", "VideoRangeTypeNotSupported"}},
		{"no index", noIndex, `{}`, false, []string{"AudioCodecNotSupported"}},
		{"a file with no index to read", emptyIndex, `{}`, false, []string{"AudioCodecNotSupported"}},
		{"keyframes too far apart", longGap, `{}`, false, []string{"AudioCodecNotSupported"}},
		{"a cap above the file", heavy, `{"MaxStreamingBitrate": 20000000}`, true, []string{"AudioCodecNotSupported"}},
		{"a cap below the file", heavy, `{"MaxStreamingBitrate": 4000000}`, false, []string{"AudioCodecNotSupported", "ContainerBitrateExceedsLimit"}},
		{"a cap and a file of unknown bitrate", noBitrate, `{"MaxStreamingBitrate": 4000000}`, false, []string{"AudioCodecNotSupported"}},
		{"no cap and a file of unknown bitrate", noBitrate, `{"MaxStreamingBitrate": 2147483647}`, true, []string{"AudioCodecNotSupported"}},
		{"a text subtitle drawn in", copyable("mkv", "ac3"), `{"SubtitleStreamIndex": 2, "AlwaysBurnInSubtitleWhenTranscoding": true}`, false, []string{"AudioCodecNotSupported", "SubtitleCodecNotSupported"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := test.source
			source.Streams = append(source.Streams, Stream{Index: 2, Type: "Subtitle", Codec: "subrip", IsTextSubtitleStream: true})
			d := Decide(request(t, test.body), source, true)
			if d.DirectPlay || d.Copy != test.copy || !reflect.DeepEqual(d.Reasons, test.reasons) {
				t.Fatalf("got %+v", d)
			}
		})
	}
	// A client with no subtitle profiles can't show a chosen track, so it is drawn in.
	r := request(t, `{"SubtitleStreamIndex": 2}`)
	r.DeviceProfile.SubtitleProfiles = nil
	source := copyable("mkv", "ac3")
	source.Streams = append(source.Streams, Stream{Index: 2, Type: "Subtitle", Codec: "subrip", IsTextSubtitleStream: true})
	if d := Decide(r, source, true); d.Copy || d.Subtitle == nil {
		t.Errorf("a burnt subtitle: %+v", d)
	}
	// What plays as it is stays direct play.
	if d := Decide(request(t, `{}`), copyable("mp4", "aac"), true); !d.DirectPlay || d.Copy {
		t.Errorf("direct play: %+v", d)
	}
}

func TestCopySpecNeverMixesWithAnEncodedOne(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.ID, source.ETag = "0123456789abcdef0123456789abcdef", "etag"
	query := "mediasourceid=" + source.ID + "&playsessionid=abc"
	encoded, err := ParseParams(mustQuery(query+"&videocodec=h264"), source)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := ParseParams(mustQuery(query+"&videocodec=copy"), source)
	if err != nil || !copied.Copy || encoded.Copy {
		t.Fatalf("copy %v %v, error %v", copied.Copy, encoded.Copy, err)
	}
	a, err := NewSpec("item", encoded, source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSpec("item", copied, source)
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() == b.Hash() || !b.Copy || b.Width != 1280 || b.Height != 720 {
		t.Errorf("%+v %+v", a, b)
	}
	// A cap changes nothing in a copy, so Auto and a cap share segments.
	capped := copied
	capped.VideoBitrate = 6_000_000
	if c, _ := NewSpec("item", capped, source); c.Hash() != b.Hash() {
		t.Error("a cap above the file split the copied segments")
	}
	// A client that edits the URL to copy what can't be copied is refused.
	source.Keyframes = nil
	if _, err := NewSpec("item", copied, source); err == nil {
		t.Error("copy asked for a file with no index")
	}
	source.Keyframes = capturedKeyframes
	source.Streams[0].Codec = "hevc"
	if _, err := NewSpec("item", copied, source); err == nil {
		t.Error("copy asked for HEVC")
	}
	if !strings.Contains(Query(copied, "tok", "etag", ""), "&VideoCodec=copy&") || !strings.Contains(Query(encoded, "tok", "etag", ""), "&VideoCodec=h264&") {
		t.Error("the URL says which")
	}
}

// The video lasts from its first keyframe to where its last frame ends, which
// is not the film's run time when the video starts late.
func TestCopyLayoutCountsTheVideoFromItsFirstKeyframe(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.Keyframes = seconds(5, 8, 12)
	source.VideoEnd = 95 * ticksPerSecond
	source.RunTimeTicks = 95 * ticksPerSecond
	layout := source.Layout(true)
	if _, length := layout.Segment(layout.Count() - 1); layout.Starts[layout.Count()-1]+length != 90*ticksPerSecond {
		t.Errorf("the playlist is %d ticks long, want 90 s", layout.Starts[layout.Count()-1]+length)
	}
}

// A 130 GB film is 1.7 Gbps over 10 minutes, and a file over 107 GiB overflowed
// the product of its size and the tick rate.
func TestFileBitrateDoesNotOverflow(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.Size = 130_000_000_000
	source.RunTimeTicks = 600 * ticksPerSecond
	source.Streams[0].DurationTicks = 0
	if got, want := source.FileBitrate(), int64(1_733_333_333); got < want-1 || got > want+1 {
		t.Errorf("FileBitrate is %d, want %d", got, want)
	}
}

// A subtitle can't be burnt into a copy; a URL that asks for both is refused
// like any other invalid copy.
func TestCopyWithABurntSubtitleIsRefused(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.ID, source.ETag = "0123456789abcdef0123456789abcdef", "etag"
	source.Streams = append(source.Streams, Stream{Index: 2, Type: "Subtitle", Codec: "subrip", IsTextSubtitleStream: true})
	params, err := ParseParams(mustQuery("mediasourceid="+source.ID+"&playsessionid=abc&videocodec=copy&subtitlestreamindex=2"), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpec("item", params, source); err == nil {
		t.Error("a copy that also burns a subtitle was accepted")
	}
}

func TestMasterNamesTheCopiedProfile(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.Streams[0].Profile, source.Streams[0].Level = "Main", 40
	spec := Spec{Copy: true, Width: 1280, Height: 720, VideoBitrate: 5_000_000, AudioBitrate: 192_000}
	if text := Master(spec, source, "q"); !strings.Contains(text, `CODECS="avc1.4D0028,mp4a.40.2"`) || !strings.Contains(text, "RESOLUTION=1280x720") || !strings.Contains(text, "BANDWIDTH=5192000") {
		t.Errorf("master:\n%s", text)
	}
	if text := Master(Spec{Width: 960, Height: 540, VideoBitrate: 1_000_000, AudioBitrate: 128_000}, source, "q"); !strings.Contains(text, `CODECS="avc1.640033,mp4a.40.2"`) {
		t.Errorf("an encoded stream is High 5.1:\n%s", text)
	}
}

func mustQuery(raw string) url.Values {
	values, err := url.ParseQuery(raw)
	if err != nil {
		panic(err)
	}
	return values
}

// A subtitle file beside the film has its own ETag in the hash, so segments
// drawn from its old text are not served after it is edited; a track inside the
// film hashes as it did before files could be tracks.
func TestSubtitleFingerprintIsInTheSpec(t *testing.T) {
	source := film("mkv", "h264", "aac")
	inside, beside := stream("Subtitle", "subrip", 10), stream("Subtitle", "ass", 12)
	inside.IsTextSubtitleStream, beside.IsTextSubtitleStream = true, true
	beside.Fingerprint = "f1"
	source.Streams = append(source.Streams, inside, beside)
	spec := func(src Source, index string) Spec {
		p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&audiostreamindex=1&subtitlestreamindex="+index, src)
		if err != nil {
			t.Fatal(err)
		}
		s, err := NewSpec("11111111-2222-3333-4444-555555555555", p, src)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := spec(source, "12"), spec(source, "10")
	if a.SubtitleFingerprint != "f1" || b.SubtitleFingerprint != "" {
		t.Fatalf("ETags %q and %q", a.SubtitleFingerprint, b.SubtitleFingerprint)
	}
	edited := source
	edited.Streams = append([]Stream(nil), source.Streams...)
	edited.Streams[len(edited.Streams)-1].Fingerprint = "f2"
	if spec(edited, "12").Hash() == a.Hash() {
		t.Error("an edited subtitle file kept its spec hash")
	}
	if spec(edited, "10").Hash() != b.Hash() {
		t.Error("editing another file moved the hash of a track inside the film")
	}
}

// A burnt-in track that a worker has since found unreadable is dropped from the
// request, so the film plays without it; a track that is no subtitle at all is
// still refused.
func TestBurntSubtitleThatBecameUnreadableIsDropped(t *testing.T) {
	source := Source{Streams: []Stream{
		{Index: 0, Type: "Video"}, {Index: 1, Type: "Audio"},
		{Index: 2, Type: "Subtitle", Codec: "subrip", IsTextSubtitleStream: true, Fingerprint: "ab", Unreadable: true},
	}}
	got, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&subtitlestreamindex=2", source)
	if err != nil || got.Subtitle != nil {
		t.Errorf("an unreadable file: %v %v", got.Subtitle, err)
	}
	if _, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&subtitlestreamindex=1", source); err == nil {
		t.Error("an audio stream was accepted as a subtitle")
	}
	source.Streams[2].Unreadable = false
	if got, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&subtitlestreamindex=2", source); err != nil || got.Subtitle == nil {
		t.Errorf("a readable file: %v %v", got.Subtitle, err)
	}
}

// hevcAV1Capture is Jellyfin 12.4's reply for HEVC and AV1 files under three
// profiles (compat/captures/hevc-av1-12.4.md).
type hevcAV1Capture struct {
	DeviceProfiles map[string]*DeviceProfile `json:"deviceProfiles"`
	PlaybackInfo   map[string]map[string]struct {
		Video struct {
			Codec, Profile, VideoRangeType string
			Level, BitDepth                int
		} `json:"video"`
		SupportsDirectPlay bool
		TranscodeReasons   *string
	} `json:"playbackInfo"`
}

func loadHEVCAV1Capture(t *testing.T) hevcAV1Capture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/hevc-av1-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture hevcAV1Capture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// Decide gives what Jellyfin 12.4 reported for each captured file: direct play
// or not, and the TranscodeReasons in order. Whether the video is copied is
// the capture's ffmpeg log (DirectStream copies, Transcode encodes); the one
// difference is HDR, which jellyscale re-encodes and Jellyfin copies.
func TestDecideMatchesJellyfinForHEVCAndAV1(t *testing.T) {
	capture := loadHEVCAV1Capture(t)
	// copied: Jellyfin ran FFmpeg.DirectStream for it, and jellyscale copies too.
	copied := map[string]bool{
		"Hevc Mkv (2001).mkv/hevc-client": true, "Hevc Ten (2003).mkv/hevc-client": true,
		"Av1 Mkv (2005).mkv/chrome": true, "Av1 Ten (2008).mkv/chrome": true,
		"Av1 Mkv (2005).mkv/hevc-client": true, "Av1 Ten (2008).mkv/hevc-client": true,
		// The H.264 control.
		"H264 Ac3 (2009).mkv/chrome": true, "H264 Ac3 (2009).mkv/hevc-client": true, "H264 Ac3 (2009).mkv/hevc-direct-only": true,
	}
	for file, byProfile := range capture.PlaybackInfo {
		for name, want := range byProfile {
			t.Run(file+"/"+name, func(t *testing.T) {
				container := strings.TrimPrefix(file[strings.LastIndex(file, "."):], ".")
				audio := "ac3"
				if container == "mp4" {
					audio = "aac"
				}
				source := copyable(container, audio)
				video := &source.Streams[0]
				video.Codec, video.Profile, video.Level = want.Video.Codec, want.Video.Profile, want.Video.Level
				video.VideoRangeType, video.BitDepth = want.Video.VideoRangeType, want.Video.BitDepth
				r := Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1))}
				d := Decide(r, source, true)
				if d.DirectPlay != want.SupportsDirectPlay {
					t.Fatalf("direct play %v, Jellyfin %v (%+v)", d.DirectPlay, want.SupportsDirectPlay, d)
				}
				var wantReasons []string
				if want.TranscodeReasons != nil {
					wantReasons = strings.Split(*want.TranscodeReasons, ",")
				}
				if !reflect.DeepEqual(d.Reasons, wantReasons) {
					t.Errorf("reasons %v, Jellyfin %v", d.Reasons, wantReasons)
				}
				if !d.DirectPlay && d.Copy != copied[file+"/"+name] {
					t.Errorf("copy %v, want %v", d.Copy, copied[file+"/"+name])
				}
			})
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestCopyOfHEVCAndAV1(t *testing.T) {
	capture := loadHEVCAV1Capture(t)
	source := func(codec, profile string, level, depth int, mutate ...func(*Stream)) Source {
		s := copyable("mkv", "ac3")
		v := &s.Streams[0]
		v.Codec, v.Profile, v.Level, v.BitDepth = codec, profile, level, depth
		for _, m := range mutate {
			m(v)
		}
		return s
	}
	for _, test := range []struct {
		name    string
		profile string
		source  Source
		copy    bool
		codecs  string
		reasons []string
	}{
		{"hevc main", "hevc-client", source("hevc", "Main", 120, 8), true, "hvc1.1.6.L120.B0", []string{"AudioCodecNotSupported"}},
		{"hevc main 10", "hevc-client", source("hevc", "Main 10", 153, 10), true, "hvc1.2.4.L153.B0", []string{"AudioCodecNotSupported"}},
		{"av1 8 bit", "chrome", source("av1", "Main", 8, 8), true, "av01.0.08M.08", []string{"AudioCodecNotSupported"}},
		{"av1 10 bit", "chrome", source("av1", "Main", 9, 10), true, "av01.0.09M.10", []string{"AudioCodecNotSupported"}},
		// The client's HLS profile doesn't list the codec: re-encoded, as Jellyfin does.
		{"hevc, hls profile lists h264 only", "hevc-direct-only", source("hevc", "Main", 120, 8), false, "hvc1.1.6.L120.B0", []string{"VideoCodecNotSupported", "AudioCodecNotSupported"}},
		{"av1, hls profile lists h264 only", "hevc-direct-only", source("av1", "Main", 8, 8), false, "av01.0.08M.08", []string{"VideoCodecNotSupported", "AudioCodecNotSupported"}},
		// HDR is labelled SDR in the master playlist, so it is never copied. A source
		// scanned before the range was recorded is treated alike.
		{"hevc hdr10", "hevc-client", source("hevc", "Main 10", 153, 10, func(v *Stream) { v.VideoRangeType = "HDR10" }), false, "hvc1.2.4.L153.B0", []string{"AudioCodecNotSupported"}},
		{"av1 hdr10", "chrome", source("av1", "Main", 8, 10, func(v *Stream) { v.VideoRangeType = "HDR10" }), false, "av01.0.08M.10", []string{"AudioCodecNotSupported"}},
		{"hevc, range not scanned yet", "hevc-client", source("hevc", "Main", 120, 8, func(v *Stream) { v.VideoRangeType = "" }), false, "hvc1.1.6.L120.B0", []string{"AudioCodecNotSupported"}},
		// Profiles the master playlist can't name are re-encoded, and say why.
		{"hevc rext", "hevc-client", source("hevc", "Rext", 120, 12), false, "", []string{"AudioCodecNotSupported", "VideoProfileNotSupported"}},
		{"av1 12 bit", "chrome", source("av1", "Main", 8, 12), false, "", []string{"AudioCodecNotSupported", "VideoProfileNotSupported"}},
		{"av1 depth not scanned yet", "chrome", source("av1", "Main", 8, 0), false, "", []string{"AudioCodecNotSupported", "VideoProfileNotSupported"}},
		{"av1 professional", "chrome", source("av1", "Professional", 8, 10), false, "", []string{"AudioCodecNotSupported", "VideoProfileNotSupported"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The captured profiles' codec limits (level, profile) allow these.
			r := Request{DeviceProfile: capture.DeviceProfiles[test.profile], AudioStreamIndex: ptr(Number(1))}
			if test.source.codecs() != test.codecs {
				t.Errorf("codecs %q, want %q", test.source.codecs(), test.codecs)
			}
			d := Decide(r, test.source, true)
			if d.DirectPlay || d.Copy != test.copy {
				t.Fatalf("got %+v, copy want %v", d, test.copy)
			}
			if test.source.CanCopy() && !test.copy && test.codecs == "" {
				t.Error("CanCopy for a profile with no name")
			}
			if !reflect.DeepEqual(d.Reasons, test.reasons) {
				t.Errorf("reasons %v, want %v", d.Reasons, test.reasons)
			}
		})
	}
}

func TestMasterNamesTheCopiedCodec(t *testing.T) {
	source := copyable("mkv", "ac3")
	source.Streams[0].Codec, source.Streams[0].Profile, source.Streams[0].Level = "hevc", "Main 10", 153
	spec := Spec{Copy: true, Width: 1280, Height: 720, VideoBitrate: 5_000_000, AudioBitrate: 128_000}
	if text := Master(spec, source, "q"); !strings.Contains(text, `CODECS="hvc1.2.4.L153.B0,mp4a.40.2"`) {
		t.Errorf("master playlist %s", text)
	}
}

func TestCopyCodecsAreTheOnesTheScanIndexes(t *testing.T) {
	for _, codec := range []string{"h264", "hevc", "av1"} {
		if !CopyCodec(codec) || !slices.Contains(CopyCodecs(), codec) {
			t.Errorf("%s is not copied", codec)
		}
	}
	for _, codec := range []string{"vp9", "mpeg4", "vc1", ""} {
		if CopyCodec(codec) {
			t.Errorf("%s is copied", codec)
		}
	}
}
