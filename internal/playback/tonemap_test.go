package playback

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// hdrFilm is a copyable film whose video is the stream given.
func hdrFilm(codec string, change func(*Stream)) Source {
	source := copyable("mkv", "ac3")
	video := &source.Streams[0]
	video.Codec, video.Profile, video.Level, video.BitDepth = codec, "High 10", 41, 10
	change(video)
	return source
}

// A picture is tone mapped by the transfer function the file declares, not by
// the range label alone.
func TestToneMapFollowsTheTransferFunction(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Stream)
		want   string
	}{
		{"sdr", func(v *Stream) {}, ""},
		{"hdr10", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HDR10", PQ }, PQ},
		{"hlg", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HLG", HLG }, HLG},
		{"dolby vision 8.1 with an hdr10 base", func(v *Stream) {
			v.VideoRangeType, v.ColorTransfer, v.DolbyVisionBlCompatibility = "DOVI", PQ, 1
		}, PQ},
		{"dolby vision 8.4 with an hlg base", func(v *Stream) {
			v.VideoRangeType, v.ColorTransfer, v.DolbyVisionBlCompatibility = "DOVI", HLG, 4
		}, HLG},
		// Profile 5's base layer is IPTPQc2, which is neither: mapped as PQ it comes
		// out green and purple, so it is left as before even if the file says PQ.
		{"dolby vision 5", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "DOVI", PQ }, ""},
		{"dolby vision 5 that says nothing", func(v *Stream) { v.VideoRangeType = "DOVI" }, ""},
		{"dolby vision 8.2 with an sdr base", func(v *Stream) {
			v.VideoRangeType, v.ColorTransfer, v.DolbyVisionBlCompatibility = "DOVI", "bt709", 2
		}, ""},
		{"hdr scanned before the transfer was recorded", func(v *Stream) { v.VideoRangeType = "HDR10" }, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hdrFilm("hevc", test.change).ToneMap(); got != test.want {
				t.Errorf("ToneMap %q, want %q", got, test.want)
			}
		})
	}
}

// HDR is never copied to a client that was told it is SDR, in H.264 as in
// HEVC, whatever the client's CodecProfiles say about the range.
func TestHDRH264IsNotCopied(t *testing.T) {
	profile := capturedProfile(t)
	for _, c := range profile.CodecProfiles {
		c.Conditions = slicesDeleteRange(c.Conditions)
	}
	hdr10 := hdrFilm("h264", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HDR10", PQ })
	hlg := hdrFilm("h264", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HLG", HLG })
	unscanned := hdrFilm("h264", func(v *Stream) { v.VideoRangeType = "" })
	sdr := hdrFilm("h264", func(v *Stream) { v.VideoRangeType = "SDR" })
	for name, test := range map[string]struct {
		source Source
		copy   bool
	}{"hdr10": {hdr10, false}, "hlg": {hlg, false}, "range not scanned yet": {unscanned, false}, "sdr": {sdr, true}} {
		t.Run(name, func(t *testing.T) {
			if test.source.CanCopy() != test.copy {
				t.Errorf("CanCopy %v", !test.copy)
			}
			d := Decide(Request{DeviceProfile: profile, AudioStreamIndex: ptr(Number(1))}, test.source, true)
			if d.DirectPlay || d.Copy != test.copy {
				t.Errorf("got %+v, copy want %v", d, test.copy)
			}
		})
	}
}

// slicesDeleteRange is a profile's conditions without the one on the range.
func slicesDeleteRange(conditions []Condition) []Condition {
	var kept []Condition
	for _, c := range conditions {
		if !strings.EqualFold(c.Property, "VideoRangeType") {
			kept = append(kept, c)
		}
	}
	return kept
}

// The tone mapping is in the hash for an HDR film, so segments that were made
// without it aren't served in its place, and absent from an SDR film's, so
// none already stored is orphaned. It comes from the source and not the URL.
func TestToneMapIsInTheSpecFromTheSource(t *testing.T) {
	spec := func(source Source, query string) Spec {
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
	sdr := hdrFilm("hevc", func(v *Stream) { v.VideoRangeType = "SDR" })
	pq := hdrFilm("hevc", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HDR10", PQ })
	hlg := hdrFilm("hevc", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HLG", HLG })
	if got := spec(pq, "").ToneMap; got != PQ {
		t.Errorf("an HDR10 film has ToneMap %q", got)
	}
	if got := spec(sdr, "").ToneMap; got != "" {
		t.Errorf("an SDR film has ToneMap %q", got)
	}
	// An SDR spec hashes as it did before tone mapping existed.
	plain := Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, VideoBitrate: 1_000_000, AudioBitrate: 128_000, Width: 640, Height: 360}
	if got, want := plain.Hash(), "8a5c05a0cc82bbb42c6cfc5d90a76724"; got != want {
		t.Errorf("the hash of an SDR spec changed: %s, was %s", got, want)
	}
	hashes := map[string]bool{}
	for name, s := range map[string]Spec{"sdr": spec(sdr, ""), "pq": spec(pq, ""), "hlg": spec(hlg, "")} {
		if hashes[s.Hash()] {
			t.Errorf("%s shares a hash with another kind of picture", name)
		}
		hashes[s.Hash()] = true
	}
	// A client can't turn it on or off by editing the URL.
	if spec(pq, "tonemap=false&tonemapping=none").Hash() != spec(pq, "").Hash() || spec(sdr, "tonemap=smpte2084").Hash() != spec(sdr, "").Hash() {
		t.Error("the URL changes the tone mapping")
	}
	// A copy is not tone mapped, and an HDR source can't be copied anyway.
	if _, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, Copy: true}, pq); err == nil {
		t.Error("an HDR film was copied")
	}
	copied := hdrFilm("h264", func(v *Stream) { v.VideoRangeType = "SDR" })
	if s := spec(copied, "videocodec=copy"); !s.Copy || s.ToneMap != "" {
		t.Errorf("a copy: %+v", s)
	}
}

// A master URL that asks for a copy of an H.264 film in any range is served as
// a copy, so an older api replica's copy keeps playing during an upgrade, while
// the new api's Decide never offers one for an HDR film (TestHDRH264IsNotCopied).
func TestStreamReplicaServesAnOlderAPIsH264HDRCopy(t *testing.T) {
	hdr := hdrFilm("h264", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HDR10", PQ })
	p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&videocodec=copy", hdr)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := NewSpec("11111111-2222-3333-4444-555555555555", p, hdr)
	if err != nil {
		t.Fatalf("an H.264 PQ copy is refused: %v", err)
	}
	if !spec.Copy || spec.ToneMap != "" {
		t.Errorf("not a copy: %+v", spec)
	}
	if !strings.Contains(Master(spec, hdr, "q"), "CODECS=\"avc1.") {
		t.Errorf("master doesn't name the copied H.264:\n%s", Master(spec, hdr, "q"))
	}
}

// Jellyfin 12.4 gave an HDR10 and an HLG film, and an H.264 HDR10 one, the
// reasons Decide gives under jellyfin-web's Chrome profile, and a master
// playlist that calls the re-encode SDR (compat/captures/hdr-tonemap-12.4.md).
func TestDecideMatchesJellyfinForHDR(t *testing.T) {
	raw, err := os.ReadFile("../../compat/captures/hdr-tonemap-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		PlaybackInfo map[string]struct {
			Video struct {
				Codec, Profile, VideoRangeType, ColorTransfer string
				Level, BitDepth                               int
			} `json:"video"`
			SupportsDirectPlay bool
			TranscodingUrl     string
		} `json:"playbackInfo"`
		MasterPlaylists map[string]string
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.PlaybackInfo) != 3 {
		t.Fatalf("%d films in the capture, want 3", len(capture.PlaybackInfo))
	}
	for file, want := range capture.PlaybackInfo {
		t.Run(file, func(t *testing.T) {
			source := copyable("mkv", "aac")
			video := &source.Streams[0]
			video.Codec, video.Profile, video.Level, video.BitDepth = want.Video.Codec, want.Video.Profile, want.Video.Level, want.Video.BitDepth
			video.VideoRangeType, video.ColorTransfer = want.Video.VideoRangeType, want.Video.ColorTransfer
			d := Decide(request(t, `{"AudioStreamIndex": 1}`), source, true)
			if d.DirectPlay != want.SupportsDirectPlay {
				t.Fatalf("direct play %v, Jellyfin %v", d.DirectPlay, want.SupportsDirectPlay)
			}
			_, reasons, _ := strings.Cut(want.TranscodingUrl, "TranscodeReasons=")
			if !reflect.DeepEqual(d.Reasons, strings.Split(reasons, ",")) {
				t.Errorf("reasons %v, Jellyfin %s", d.Reasons, reasons)
			}
			if d.Copy {
				t.Error("an HDR film was copied; Jellyfin tone maps it")
			}
			if !strings.Contains(capture.MasterPlaylists[file], "VIDEO-RANGE=SDR") {
				t.Fatalf("Jellyfin's master doesn't call the tone-mapped video SDR:\n%s", capture.MasterPlaylists[file])
			}
			if source.ToneMap() == "" {
				t.Error("no tone mapping for an HDR film")
			}
			spec := Spec{Width: 640, Height: 360, ToneMap: source.ToneMap()}
			if !strings.Contains(Master(spec, source, "q"), "VIDEO-RANGE=SDR") {
				t.Error("jellyscale's master doesn't call the tone-mapped video SDR")
			}
		})
	}
}

// The worker reads the spec the API stored as JSON. A worker whose Spec has no
// ToneMap drops it and encodes the picture unmapped under the mapped hash, so the
// field must survive the round trip, and be what the hash was made from.
func TestToneMapSurvivesTheJobsJSON(t *testing.T) {
	spec := Spec{Item: "film", Width: 1920, Height: 1080, VideoBitrate: 4000000, ToneMap: "PQ"}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var read Spec
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if read.ToneMap != "PQ" || read.Hash() != spec.Hash() {
		t.Fatalf("worker read ToneMap %q, hash %s, want PQ, %s", read.ToneMap, read.Hash(), spec.Hash())
	}
	spec.ToneMap = ""
	if read.Hash() == spec.Hash() {
		t.Fatal("the hash does not include the tone mapping")
	}
}
