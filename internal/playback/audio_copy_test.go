package playback

import (
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type audioCopyCapture struct {
	DeviceProfiles map[string]*DeviceProfile `json:"deviceProfiles"`
	SourceAudio    map[string]struct {
		Codec    string
		Channels int
		Profile  string
	} `json:"sourceAudio"`
	Results map[string]map[string]struct {
		Audio struct {
			BitRate int64
		} `json:"audio"`
		TranscodeReasons string
		AudioBitrate     string
		VideoBitrate     string
		MaxChannels      string `json:"TranscodingMaxAudioChannels"`
		FFmpegAudio      string `json:"ffmpegAudio"`
		FFmpegSegments   string `json:"ffmpegSegments"`
	} `json:"results"`
}

// audioFilm is the captured interlaced H.264 film with the audio the capture
// says it has.
func audioFilm(t *testing.T, capture audioCopyCapture, file string, bitrate int64) Source {
	t.Helper()
	container := file[strings.LastIndex(file, ".")+1:]
	source := film(container, "h264", "x")
	source.Streams[0].IsInterlaced = true
	a := capture.SourceAudio[file]
	source.Streams[1] = Stream{Index: 1, Type: "Audio", Codec: a.Codec, Profile: a.Profile, Channels: a.Channels, Bitrate: bitrate, IsDefault: true}
	return source
}

// Decide copies the audio where Jellyfin 12.4 did (its ffmpeg log says `-codec:a:0
// copy`) and names the same reasons and channel limit, except where
// jellyscale deliberately differs, each listed with why
// (compat/captures/audio-copy-12.4.md).
func TestAudioCopyMatchesJellyfin(t *testing.T) {
	raw, err := os.ReadFile("../../compat/captures/audio-copy-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture audioCopyCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	differs := map[string]string{
		// Jellyfin delivers MP3 for Chrome in MPEG-TS, the only profile that lists
		// it; jellyscale makes fMP4 only (invariant 21), whose Chrome profile does not.
		"Mp3 Mkv (3007).mkv/chrome@1440987881": "no mp3 in the fMP4 profile",
		"Mp3 Mkv (3007).mkv/chrome@1500000":    "no mp3 in the fMP4 profile",
		"Mp3 Mkv (3007).mkv/chrome@720000":     "no mp3 in the fMP4 profile",
		// Jellyfin copies a 192 kbps track under a 720 kbps cap; here the track
		// would take more than a quarter of it.
		"Aac2 Mkv (3004).mkv/chrome@720000": "the cap is too low for the track",
		// Jellyfin copies a 5.1 track, and so 960 kbps of FLAC, under no cap; a
		// 1.5 Mbps one it re-encodes, as here.
	}
	// What the capture's profile lists is what Decide must read, so a run with a
	// copy in Jellyfin's log is a copy here.
	n := 0
	for file, runs := range capture.Results {
		for run, want := range runs {
			name, limit, _ := strings.Cut(run, "@")
			cap, _ := strconv.ParseInt(limit, 10, 64)
			t.Run(file+"/"+run, func(t *testing.T) {
				source := audioFilm(t, capture, file, want.Audio.BitRate)
				// MKV files carry no bitrate, and the reply's figure is Jellyfin's guess.
				d := Decide(Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1)), MaxStreamingBitrate: ptr(Number(cap))}, source, true)
				// Under a cap Jellyfin names the bitrate and the audio and leaves the
				// video's reasons out, and which cap a file exceeds depends on its
				// size, so there only the channel reason is compared.
				keep := func(r string) bool {
					return r != "ContainerBitrateExceedsLimit" && (cap == 1440987881 || r == "AudioChannelsNotSupported")
				}
				wantReasons := slices.DeleteFunc(strings.Split(want.TranscodeReasons, ","), func(r string) bool { return !keep(r) })
				gotReasons := slices.DeleteFunc(slices.Clone(d.Reasons), func(r string) bool { return !keep(r) })
				if !reflect.DeepEqual(gotReasons, wantReasons) {
					t.Errorf("reasons %v, Jellyfin %v", gotReasons, wantReasons)
				}
				if strconv.Itoa(d.MaxAudioChannels) != want.MaxChannels {
					t.Errorf("max channels %d, Jellyfin %s", d.MaxAudioChannels, want.MaxChannels)
				}
				params := Params{AudioIndex: d.AudioIndex, AudioCodecs: d.AudioCodecs, MaxAudioChannels: d.MaxAudioChannels, VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate}
				_, copied := source.copyAudio(params)
				jellyfinCopies := strings.HasPrefix(want.FFmpegAudio, "-codec:a:0 copy")
				if why, ok := differs[file+"/"+run]; ok {
					if copied == jellyfinCopies {
						t.Errorf("listed as different (%s) but copy is %v as Jellyfin's", why, copied)
					}
					return
				}
				if copied != jellyfinCopies {
					t.Errorf("copy %v, Jellyfin %v (%s)", copied, jellyfinCopies, want.FFmpegAudio)
				}
				n++
			})
		}
	}
	if n < 40 {
		t.Errorf("only %d runs compared", n)
	}
}

// audioSource is an interlaced film with one audio track.
func audioSource(codec, profile string, channels int, bitrate int64) Source {
	source := film("mkv", "h264", "x")
	source.Streams[0].IsInterlaced = true
	source.Streams[1] = Stream{Index: 1, Type: "Audio", Codec: codec, Profile: profile, Channels: channels, Bitrate: bitrate, IsDefault: true}
	return source
}

func TestDecideCopiesAudioWhenTheProfileTakesIt(t *testing.T) {
	surround := &DeviceProfile{TranscodingProfiles: []TranscodingProfile{{Type: "Video", Protocol: "hls", Container: "mp4", VideoCodec: "h264",
		AudioCodec: "aac,mp3,ac3,eac3,flac,opus,mp2", MaxAudioChannels: 6}}}
	chrome := capturedProfile(t)
	tests := []struct {
		name    string
		profile *DeviceProfile
		source  Source
		cap     int64
		copies  bool
	}{
		{"aac stereo", chrome, audioSource("aac", "LC", 2, 128_000), Unlimited, true},
		{"aac 5.1, the profile lists two channels", chrome, audioSource("aac", "LC", 6, 384_000), Unlimited, false},
		{"aac 5.1, the profile lists six", surround, audioSource("aac", "LC", 6, 384_000), Unlimited, true},
		{"he-aac", chrome, audioSource("aac", "HE-AAC", 2, 64_000), Unlimited, true},
		{"aac main, which has no codec string", chrome, audioSource("aac", "Main", 2, 128_000), Unlimited, false},
		{"opus", chrome, audioSource("opus", "", 2, 128_000), Unlimited, true},
		{"flac", chrome, audioSource("flac", "", 2, 960_000), Unlimited, true},
		{"ac3, which chrome does not list", chrome, audioSource("ac3", "", 2, 192_000), Unlimited, false},
		{"ac3 in a profile that lists it", surround, audioSource("ac3", "", 6, 448_000), Unlimited, true},
		{"eac3", surround, audioSource("eac3", "", 6, 448_000), Unlimited, true},
		{"mp3, which only the ts profile of chrome lists", chrome, audioSource("mp3", "", 2, 192_000), Unlimited, false},
		{"mp3 in a profile that lists it", surround, audioSource("mp3", "", 2, 192_000), Unlimited, true},
		{"truehd, which no profile can name", surround, audioSource("truehd", "", 6, 4_000_000), Unlimited, false},
		{"a track that fits a quarter of the cap", chrome, audioSource("aac", "LC", 2, 128_000), 2_000_000, true},
		{"a track over a quarter of the cap", chrome, audioSource("aac", "LC", 2, 128_000), 400_000, false},
		{"flac under an 8 Mbps cap", chrome, audioSource("flac", "", 2, 960_000), 8_000_000, true},
		{"flac under a 3 Mbps cap", chrome, audioSource("flac", "", 2, 960_000), 3_000_000, false},
		{"track below the URL's floor", chrome, audioSource("aac", "LC", 1, 24_000), 8_000_000, true},
		{"track above what the URL can say", chrome, audioSource("flac", "", 2, 20_000_000), 100_000_000, false},
		{"unknown bitrate at Auto", chrome, audioSource("aac", "LC", 2, 0), Unlimited, true},
		{"unknown bitrate under a 8 Mbps cap", chrome, audioSource("aac", "LC", 2, 0), 8_000_000, false},
		{"unknown bitrate under a 24 Mbps cap", chrome, audioSource("aac", "LC", 2, 0), 24_000_000, true},
		{"unknown bitrate under a measured Auto of 1.4 Gbps", chrome, audioSource("aac", "LC", 2, 0), 1_440_987_881, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := test.source
			source.Bitrate = 50_000_000 // So the cap is never what forces the stream: the interlaced video is.
			d := Decide(Request{DeviceProfile: test.profile, MaxStreamingBitrate: ptr(Number(test.cap))}, source, true)
			if d.DirectPlay || d.Copy {
				t.Fatalf("want a re-encode: %+v", d)
			}
			params := Params{SourceID: sourceID, PlaySessionID: "abc", AudioIndex: d.AudioIndex, AudioCodecs: d.AudioCodecs, MaxAudioChannels: d.MaxAudioChannels, VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate}
			stream, copied := source.copyAudio(params)
			if copied != test.copies {
				t.Fatalf("copy %v, want %v (%+v)", copied, test.copies, d)
			}
			// The spec, built from the URL alone, decides the same.
			query, err := parseQuery(Query(params, "tok", "etag", ""), source)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := NewSpec("11111111-2222-3333-4444-555555555555", query, source)
			if err != nil {
				t.Fatal(err)
			}
			if (spec.AudioCopy != "") != test.copies {
				t.Fatalf("spec copies %q, Decide %v", spec.AudioCopy, test.copies)
			}
			if !test.copies {
				return
			}
			// A copied track counts against the cap by its own bitrate; the parts
			// still sum to the cap, and the spec has the track's own bitrate.
			if test.cap != Unlimited && d.VideoBitrate+d.AudioBitrate != test.cap {
				t.Errorf("split %d + %d of %d for a %d track", d.VideoBitrate, d.AudioBitrate, test.cap, stream.Bitrate)
			}
			want := stream.Bitrate
			if want == 0 {
				want = unknownAudioBitrate
			}
			if want = min(max(want, MinAudioBitrate), maxCopiedAudioBitrate); d.AudioBitrate != want {
				t.Errorf("the URL's audio bitrate is %d, want %d", d.AudioBitrate, want)
			}
			if d.AudioBitrate < MinAudioBitrate || d.AudioBitrate > maxCopiedAudioBitrate {
				t.Errorf("the URL's audio bitrate %d is out of range", d.AudioBitrate)
			}
			if spec.AudioBitrate != stream.Bitrate || spec.AudioCopy != stream.Codec {
				t.Errorf("spec %+v", spec)
			}
		})
	}
}

// parseQuery reads a URL as a stream request does, with the keys lower case.
func parseQuery(raw string, source Source) (Params, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return Params{}, err
	}
	lower := url.Values{}
	for key, v := range values {
		lower[strings.ToLower(key)] = v
	}
	return ParseParams(lower, source)
}

func TestAudioCopyIsInTheHashAndOnlyThere(t *testing.T) {
	source := audioSource("aac", "LC", 2, 128_000)
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
	encoded, copied := spec(""), spec("audiocodec=aac,flac")
	if encoded.AudioCopy != "" || copied.AudioCopy != "aac" {
		t.Fatalf("specs %+v %+v", encoded, copied)
	}
	if encoded.Hash() == copied.Hash() {
		t.Error("a copied track and an encoded one must not share segments")
	}
	// A cap that changes nothing about the track doesn't make new segments.
	if copied.Hash() != spec("audiocodec=aac&audiobitrate=999999&videobitrate=2147291647").Hash() {
		t.Error("the URL's audio bitrate must not key a copy")
	}
	// Codecs of the same name family don't collide.
	ac3, eac3 := Spec{AudioCopy: "ac3"}, Spec{AudioCopy: "eac3"}
	if ac3.Hash() == eac3.Hash() {
		t.Error("ac3 and eac3 share a hash")
	}
	// A spec with no copied audio hashes only its own fields and OutputVersion,
	// pinned so that a change to the hash is never an accident. OutputVersion 3
	// changed it, for the restart cut (DOMAINS 24).
	if got, want := (Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, VideoBitrate: 1, AudioBitrate: 2, Width: 3, Height: 4}).Hash(), "5892759c3c1b9804ace2ea2ad3180f60"; got != want {
		t.Errorf("hash %s, want %s", got, want)
	}
}

// A copied video re-encodes its audio (CopyCommand's fragment joiner relies
// on the AAC grid), whatever the URL lists, and its hash is what it was.
func TestVideoCopyNeverCopiesAudio(t *testing.T) {
	source := copyable("mkv", "aac")
	source.Streams[1].Profile, source.Streams[1].Bitrate = "LC", 128_000
	p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&videocodec=copy&audiocodec=aac,flac", source)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := NewSpec("11111111-2222-3333-4444-555555555555", p, source)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Copy || spec.AudioCopy != "" {
		t.Fatalf("spec %+v", spec)
	}
	without, _ := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&videocodec=copy", source)
	plain, _ := NewSpec("11111111-2222-3333-4444-555555555555", without, source)
	if spec.Hash() != plain.Hash() {
		t.Error("the audio list must not change a video copy's hash")
	}
	if !strings.Contains(Master(spec, source, "q"), `,mp4a.40.2"`) {
		t.Error("a video copy's audio is AAC")
	}
}

func TestMasterNamesTheCopiedAudio(t *testing.T) {
	for _, c := range []struct {
		codec, profile, want string
	}{
		{"aac", "LC", "mp4a.40.2"}, {"aac", "HE-AAC", "mp4a.40.5"}, {"aac", "HE-AACv2", "mp4a.40.29"}, {"mp3", "", "mp4a.6B"},
		{"ac3", "", "ac-3"}, {"eac3", "", "ec-3"}, {"opus", "", "Opus"}, {"flac", "", "fLaC"},
	} {
		source := audioSource(c.codec, c.profile, 2, 128_000)
		p := Params{SourceID: sourceID, AudioIndex: 1, AudioCodecs: []string{c.codec}, MaxAudioChannels: 2, VideoBitrate: Unlimited - 192_000, AudioBitrate: 192_000}
		spec, err := NewSpec("11111111-2222-3333-4444-555555555555", p, source)
		if err != nil {
			t.Fatal(err)
		}
		if master := Master(spec, source, "q"); !strings.Contains(master, `,`+c.want+`"`) {
			t.Errorf("%s: %s", c.codec, master)
		}
	}
	// An encoded track is AAC LC whatever the source was.
	source := audioSource("ac3", "", 2, 192_000)
	spec, _ := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2, VideoBitrate: 1_000_000, AudioBitrate: 192_000}, source)
	if !strings.Contains(Master(spec, source, "q"), `,mp4a.40.2"`) {
		t.Error("an encoded track is AAC")
	}
}

// Only codecs that can be copied get through from the URL, and the channel
// limit is a small integer.
func TestAudioCodecListIsWhitelisted(t *testing.T) {
	if got := AudioCodecList("AAC, flac,aac,truehd;-i x,opus,,mp2"); !reflect.DeepEqual(got, []string{"aac", "flac", "opus"}) {
		t.Errorf("got %v", got)
	}
	source := audioSource("aac", "LC", 2, 128_000)
	if p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&audiobitrate=13000000", source); err == nil {
		t.Errorf("accepted %+v", p)
	}
	// A channel limit that is out of range or not a number is the default.
	for _, q := range []string{"", "transcodingmaxaudiochannels=0", "transcodingmaxaudiochannels=9", "transcodingmaxaudiochannels=-1", "transcodingmaxaudiochannels=2%3B-i", "transcodingmaxaudiochannels=x"} {
		p, err := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&"+q, source)
		if err != nil || p.MaxAudioChannels != 2 {
			t.Errorf("%q: %+v %v", q, p, err)
		}
	}
	if p, _ := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&transcodingmaxaudiochannels=6", source); p.MaxAudioChannels != 6 {
		t.Errorf("six channels read as %d", p.MaxAudioChannels)
	}
	// A URL that edits the list can only ask for what the source is and the limits allow.
	p, _ := params(t, "mediasourceid="+sourceID+"&playsessionid=abc&audiocodec=flac,aac&transcodingmaxaudiochannels=1", audioSource("aac", "LC", 2, 128_000))
	if spec, _ := NewSpec("11111111-2222-3333-4444-555555555555", p, audioSource("aac", "LC", 2, 128_000)); spec.AudioCopy != "" {
		t.Error("two channels over a limit of one were copied")
	}
	// A copied bitrate above the encoder's ceiling is no reason to ask the encoder for it.
	big := audioSource("ac3", "", 2, 192_000)
	p, _ = params(t, "mediasourceid="+sourceID+"&playsessionid=abc&audiobitrate=900000", big)
	if spec, _ := NewSpec("11111111-2222-3333-4444-555555555555", p, big); spec.AudioBitrate != MaxAudioBitrate {
		t.Errorf("encoded audio bitrate %d", spec.AudioBitrate)
	}
}

// The profile's own limits on the audio stream keep a track from being copied.
func TestCodecProfileLimitsKeepAudioFromBeingCopied(t *testing.T) {
	profile := &DeviceProfile{
		TranscodingProfiles: []TranscodingProfile{{Type: "Video", Protocol: "hls", Container: "mp4", VideoCodec: "h264", AudioCodec: "aac,flac", MaxAudioChannels: 6}},
		CodecProfiles:       []CodecProfile{{Type: "VideoAudio", Codec: "flac", Conditions: []Condition{{Condition: "LessThanEqual", Property: "AudioChannels", Value: "2"}}}},
	}
	for channels, copies := range map[int]bool{2: true, 6: false} {
		source := audioSource("flac", "", channels, 960_000)
		d := Decide(Request{DeviceProfile: profile}, source, true)
		_, copied := source.copyAudio(Params{AudioIndex: 1, AudioCodecs: d.AudioCodecs, MaxAudioChannels: d.MaxAudioChannels, VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate})
		if copied != copies && !d.DirectPlay {
			t.Errorf("%d channels: copy %v, want %v (%+v)", channels, copied, copies, d)
		}
	}
}

// The URL carries the profile's audio list with plain commas and the channel
// limit, as Jellyfin's does, and the list round trips.
func TestQueryCarriesTheAudioList(t *testing.T) {
	source := audioSource("aac", "LC", 2, 128_000)
	p := Params{SourceID: sourceID, PlaySessionID: "abc", AudioIndex: 1, AudioCodecs: []string{"aac", "opus", "flac"}, MaxAudioChannels: 6, VideoBitrate: 1_000_000, AudioBitrate: 128_000}
	query := Query(p, "tok", "etag", "")
	if !strings.Contains(query, "&AudioCodec=aac,opus,flac&") || !strings.Contains(query, "&TranscodingMaxAudioChannels=6&") {
		t.Fatalf("query: %s", query)
	}
	back, err := parseQuery(query, source)
	if err != nil || !reflect.DeepEqual(back.AudioCodecs, p.AudioCodecs) || back.MaxAudioChannels != 6 {
		t.Errorf("round trip: %+v %v", back, err)
	}
}

// A copied track whose file doesn't say its bitrate counts as 6 Mbps in the
// master's BANDWIDTH, as it did for the cap.
func TestMasterBandwidthCountsAnUnknownCopiedTrack(t *testing.T) {
	for bitrate, want := range map[int64]int64{0: unknownAudioBitrate, 128_000: 128_000} {
		source := audioSource("aac", "LC", 2, bitrate)
		spec, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2, VideoBitrate: 100_000_000, AudioBitrate: 192_000}, source)
		if err != nil || spec.AudioCopy == "" {
			t.Fatalf("%+v %v", spec, err)
		}
		want += spec.VideoBitrate
		if master := Master(spec, source, "q"); !strings.Contains(master, "BANDWIDTH="+strconv.FormatInt(want, 10)+",") {
			t.Errorf("bitrate %d: want BANDWIDTH %d: %s", bitrate, want, master)
		}
	}
}

// The quarter-of-the-cap rule is deliberate (DOMAINS 52): a track of exactly a
// quarter copies, one bit more is encoded.
func TestAudioCopyQuarterOfTheCapBoundary(t *testing.T) {
	for _, c := range []struct {
		bitrate int64
		copies  bool
	}{{500_000, true}, {500_001, false}} {
		source := audioSource("flac", "", 2, c.bitrate)
		p := Params{AudioIndex: 1, AudioCodecs: []string{"flac"}, MaxAudioChannels: 2, VideoBitrate: 1_500_000, AudioBitrate: 500_000}
		if _, ok := source.copyAudio(p); ok != c.copies {
			t.Errorf("%d bits in a cap of 2 Mbps: copy %v, want %v", c.bitrate, ok, c.copies)
		}
	}
}
