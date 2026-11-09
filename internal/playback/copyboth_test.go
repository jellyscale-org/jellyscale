package playback

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// compat/captures/copy-both-12.4.md says how these were observed.
type copyBothCapture struct {
	DeviceProfiles map[string]*DeviceProfile `json:"deviceProfiles"`
	Results        map[string]copyBothResult `json:"results"`
}

type copyBothResult struct {
	Src struct {
		Container string
		Streams   []struct {
			Type, Codec, Profile string
			Channels             int
			BitRate              int64
			Index                int
		}
	} `json:"src"`
	SupportsDirectPlay bool
	TranscodeReasons   string
	AudioBitrate       string
	VideoBitrate       string
	MasterCodecs       string `json:"masterCodecs"`
	FFmpegVideo        string `json:"ffmpegVideo"`
	FFmpegAudio        string `json:"ffmpegAudio"`
}

func loadCopyBothCapture(t *testing.T) copyBothCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/copy-both-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture copyBothCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// A scanned film shaped like the capture's file: its container, streams and
// bitrates, with the keyframe index a scan reads.
func copyBothFilm(container string, streams []struct {
	Type, Codec, Profile string
	Channels             int
	BitRate              int64
	Index                int
}) Source {
	source := copyable(container)
	source.Streams = nil
	source.Bitrate = 0
	for _, s := range streams {
		stream := Stream{Index: s.Index, Type: s.Type, Codec: s.Codec, Profile: s.Profile, Channels: s.Channels, Bitrate: s.BitRate}
		if s.Type == "Video" {
			stream.Level, stream.VideoRangeType, stream.Width, stream.Height, stream.AverageFrameRate = 41, "SDR", 640, 360, 24
			if s.Codec == "hevc" {
				stream.Level = 93
			}
		} else {
			stream.IsDefault = s.Index == 1
		}
		source.Bitrate += s.BitRate
		source.Streams = append(source.Streams, stream)
	}
	return source
}

// copyBothRun is one file and profile of the capture, decided here.
type copyBothRun struct {
	key, profile string
	cap          int64
	index        int
	want         copyBothResult
	source       Source
	decision     Decision
}

// copyBothRuns decides every file and profile of the capture, except where
// jellyscale differs on purpose, each listed with why.
func copyBothRuns(t *testing.T) []copyBothRun {
	capture := loadCopyBothCapture(t)
	differs := func(key string) bool {
		switch {
		case strings.HasPrefix(key, "H264 Mp3 Mkv/chrome-nomkv"):
			// Jellyfin delivers MP3 for Chrome in MPEG-TS; jellyscale makes fMP4 only
			// (DOMAINS 21), whose Chrome profile lists no MP3.
			return true
		case strings.HasPrefix(key, "H264 Truehd Mkv"):
			// Jellyfin re-encodes the video of a film with TrueHD; jellyscale copies
			// it whatever the audio is (JS-025).
			return true
		}
		return false
	}
	var runs []copyBothRun
	for key, want := range capture.Results {
		_, rest, _ := strings.Cut(key, "/")
		profileName, rest, _ := strings.Cut(rest, "@")
		limit, audioIndex, _ := strings.Cut(rest, "#")
		if differs(key) {
			continue
		}
		cap, _ := strconv.ParseInt(limit, 10, 64)
		index, _ := strconv.Atoi(audioIndex)
		source := copyBothFilm(want.Src.Container, want.Src.Streams)
		d := Decide(Request{DeviceProfile: capture.DeviceProfiles[profileName], AudioStreamIndex: ptr(Number(index)), MaxStreamingBitrate: ptr(Number(cap))}, source, true)
		runs = append(runs, copyBothRun{key, profileName, cap, index, want, source, d})
	}
	slices.SortFunc(runs, func(a, b copyBothRun) int { return strings.Compare(a.key, b.key) })
	return runs
}

// Jellyfin 12.4 remuxes, with `-codec:v:0 copy -codec:a:0 copy`, when the
// client's profile takes both codecs and only the container (or the track
// choice) forced the stream; jellyscale does the same, names the copied
// codec in the master playlist as Jellyfin does, and keeps copying the video
// alone where Jellyfin does, with the audio re-encoded.
func TestDecideCopiesBothStreamsAsJellyfinDid(t *testing.T) {
	compared, both := 0, 0
	for _, run := range copyBothRuns(t) {
		want, source, d, index := run.want, run.source, run.decision, run.index
		t.Run(run.key, func(t *testing.T) {
			if want.SupportsDirectPlay {
				if !d.DirectPlay {
					t.Errorf("Jellyfin direct plays, got %+v", d)
				}
				return
			}
			if d.DirectPlay {
				t.Fatalf("Jellyfin transcodes (%s), got direct play", want.TranscodeReasons)
			}
			wantVideo, wantAudio := want.FFmpegVideo == "copy", want.FFmpegAudio == "copy"
			if d.Copy != wantVideo {
				t.Fatalf("video copy %v, Jellyfin %v", d.Copy, wantVideo)
			}
			params := Params{SourceID: sourceID, AudioIndex: d.AudioIndex, AudioCodecs: d.AudioCodecs, MaxAudioChannels: d.MaxAudioChannels,
				VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate, Copy: d.Copy}
			spec, err := NewSpec("11111111-2222-3333-4444-555555555555", params, source)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Copy != wantVideo || (spec.AudioCopy != "") != wantAudio {
				t.Fatalf("spec copies video %v and audio %q; Jellyfin copies video %v and audio %v (%s)", spec.Copy, spec.AudioCopy, wantVideo, wantAudio, want.FFmpegAudio)
			}
			// The master playlist names the video, then the audio as it will be.
			master := Master(spec, source, "q")
			codecs := regexp.MustCompile(`CODECS="([^"]*)"`).FindStringSubmatch(master)
			if codecs == nil {
				t.Fatalf("no CODECS in %s", master)
			}
			video, audio, _ := cutLast(codecs[1], ",")
			wantVideoCodec, wantAudioCodec, _ := cutLast(want.MasterCodecs, ",")
			// jellyscale names MP3 mp4a.6B, as TestMasterNamesTheCopiedAudio pins.
			if wantAudioCodec == "mp4a.40.34" {
				wantAudioCodec = "mp4a.6B"
			}
			if audio != wantAudioCodec || video[:4] != wantVideoCodec[:4] {
				t.Errorf("CODECS %q, Jellyfin %q", codecs[1], want.MasterCodecs)
			}
			compared++
			if wantVideo && wantAudio {
				both++
				stream, _ := source.audio(index)
				// The cap is shared as Jellyfin shares it: the copied track's own
				// bitrate comes out of it, and the spec carries that bitrate.
				if strconv.FormatInt(d.AudioBitrate, 10) != want.AudioBitrate || strconv.FormatInt(d.VideoBitrate, 10) != want.VideoBitrate {
					t.Errorf("bitrates video %d audio %d, Jellyfin %s and %s", d.VideoBitrate, d.AudioBitrate, want.VideoBitrate, want.AudioBitrate)
				}
				if spec.AudioCopy != stream.Codec || spec.AudioBitrate != stream.Bitrate {
					t.Errorf("spec audio %q at %d, the track is %s at %d", spec.AudioCopy, spec.AudioBitrate, stream.Codec, stream.Bitrate)
				}
				if bandwidth := regexp.MustCompile(`BANDWIDTH=(\d+)`).FindStringSubmatch(master); bandwidth[1] != strconv.FormatInt(source.FileBitrate()+stream.Bitrate, 10) {
					t.Errorf("BANDWIDTH %s, want the file's %d and the track's %d", bandwidth[1], source.FileBitrate(), stream.Bitrate)
				}
			}
		})
	}
	if compared < 50 || both < 20 {
		t.Errorf("compared %d runs, %d copying both: the check looked at too little", compared, both)
	}
}

// The reasons of a stream Jellyfin copies are the ones it names, at Auto: the
// container, and the audio codec the direct-play profile lacks even where the
// stream copies it, as in compat/captures/audio-copy-12.4.md. (Under a cap
// Jellyfin leaves the video's reasons out, and which cap a file exceeds
// depends on its size, so only Auto is compared.) A transcode forced only by
// the viewer's pick of another track has no reason here, where Jellyfin says
// SecondaryAudioNotSupported; jellyscale kept that (the "another audio track"
// case of TestDecideCopiesVideoTheClientPlays) and this task does not change it.
func TestCopyReasonsAreJellyfinsAtAuto(t *testing.T) {
	n := 0
	for _, run := range copyBothRuns(t) {
		if run.cap != 1440987881 || run.want.SupportsDirectPlay {
			continue
		}
		t.Run(run.key, func(t *testing.T) {
			want := slices.DeleteFunc(strings.Split(run.want.TranscodeReasons, ","), func(r string) bool { return r == "SecondaryAudioNotSupported" || r == "" })
			if !slices.Equal(run.decision.Reasons, want) && (len(run.decision.Reasons) > 0 || len(want) > 0) {
				t.Errorf("reasons %v, Jellyfin %v", run.decision.Reasons, want)
			}
		})
		n++
	}
	if n < 40 {
		t.Errorf("compared %d runs", n)
	}
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// Copying both is its own output: it never shares segments with a copy whose
// audio is re-encoded or with a re-encode that copies the audio, and a copy
// with the audio re-encoded keeps the hash it had.
func TestCopyOfBothIsItsOwnOutput(t *testing.T) {
	base := Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, VideoBitrate: 1, AudioBitrate: 2, Width: 3, Height: 4}
	videoOnly, audioOnly, both := base, base, base
	videoOnly.Copy = true
	audioOnly.AudioCopy = "aac"
	both.Copy, both.AudioCopy = true, "aac"
	hashes := map[string]string{"plain": base.Hash(), "video copy": videoOnly.Hash(), "audio copy": audioOnly.Hash(), "both": both.Hash()}
	for a, x := range hashes {
		for b, y := range hashes {
			if a < b && x == y {
				t.Errorf("%s and %s share a hash", a, b)
			}
		}
	}
	// Pinned: a video copy that re-encodes its audio is what it was before
	// this task, so segments stored by an earlier release are still served.
	if got, want := videoOnly.Hash(), "f0dd2d5eb36ff71ec23c7ddba87d993d"; got != want {
		t.Errorf("a video copy's hash is %s, was %s", got, want)
	}
	ac3, eac3 := both, both
	ac3.AudioCopy, eac3.AudioCopy = "ac3", "eac3"
	if ac3.Hash() == eac3.Hash() || ac3.Hash() == both.Hash() {
		t.Error("two codecs of copied audio share segments")
	}
}

// The copy of both is decided from the URL and the source alone, so a stream
// replica that reads a master URL reaches the spec the api decided on, and the
// cap decides whether the track is copied as it does for a re-encode: the
// track has to take no more than a quarter of it. The video is copied whenever
// the file fits the cap, so a film the cap forces to carry a heavy track
// re-encodes the audio and still copies the video.
func TestCopyOfBothFollowsTheCapAndTheURL(t *testing.T) {
	capture := loadCopyBothCapture(t)
	chrome := capture.DeviceProfiles["chrome"]
	for _, test := range []struct {
		name      string
		cap       int64
		wantAudio bool
	}{
		{"no cap", 2147483647, true},
		{"a cap four times the track", 3_840_000, true},
		{"a cap just under four times the track", 3_839_996, false},
		{"a cap above the file that leaves too little for the track", 2_500_000, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 2 Mbps in an AVI with 960 kbps of FLAC: the container is what the client lacks.
			source := copyable("avi", "flac")
			source.Streams[1].Profile, source.Streams[1].Bitrate = "", 960_000
			d := Decide(Request{DeviceProfile: chrome, AudioStreamIndex: ptr(Number(1)), MaxStreamingBitrate: ptr(Number(test.cap))}, source, true)
			if d.DirectPlay || !d.Copy {
				t.Fatalf("got %+v", d)
			}
			params := Params{SourceID: sourceID, PlaySessionID: "abc", AudioIndex: 1, AudioCodecs: d.AudioCodecs, MaxAudioChannels: d.MaxAudioChannels,
				VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate, Copy: true}
			spec, err := NewSpec("11111111-2222-3333-4444-555555555555", params, source)
			if err != nil {
				t.Fatal(err)
			}
			if !spec.Copy || (spec.AudioCopy == "flac") != test.wantAudio {
				t.Fatalf("spec %+v: audio copied %v, want %v", spec, spec.AudioCopy != "", test.wantAudio)
			}
			// What a stream replica makes of the URL the api gave.
			link := MasterURL("11111111-2222-3333-4444-555555555555", params, "key", "tag", d, 24)
			read, err := parseQuery(link[strings.Index(link, "?")+1:], source)
			if err != nil {
				t.Fatal(err)
			}
			again, err := NewSpec("11111111-2222-3333-4444-555555555555", read, source)
			if err != nil {
				t.Fatal(err)
			}
			if again.Hash() != spec.Hash() {
				t.Errorf("the replica's spec %+v is not the api's %+v", again, spec)
			}
		})
	}
}

// A copy of both streams takes the audio the client asks for in its list and
// no other: a track the list lacks, or one with more channels than the client
// takes, is re-encoded with the video still copied.
func TestCopyOfBothLeavesAnAudioTheClientCannotTake(t *testing.T) {
	capture := loadCopyBothCapture(t)
	for _, test := range []struct {
		name, profile, codec string
		channels             int
		wantAudio            string
	}{
		{"opus in a list with opus", "chrome", "opus", 2, "opus"},
		{"ac3 not in the list", "chrome", "ac3", 2, ""},
		{"ac3 in the list", "surround", "ac3", 6, "ac3"},
		{"six channels of aac for two", "chrome", "aac", 6, ""},
		{"six channels of aac for six", "surround", "aac", 6, "aac"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := copyable("avi", test.codec)
			source.Streams[1].Channels, source.Streams[1].Bitrate = test.channels, 128_000
			if test.codec == "aac" {
				source.Streams[1].Profile = "LC"
			}
			d := Decide(Request{DeviceProfile: capture.DeviceProfiles[test.profile], AudioStreamIndex: ptr(Number(1))}, source, true)
			if !d.Copy {
				t.Fatalf("the video is not copied: %+v", d)
			}
			spec, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, AudioCodecs: d.AudioCodecs,
				MaxAudioChannels: d.MaxAudioChannels, VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate, Copy: true}, source)
			if err != nil {
				t.Fatal(err)
			}
			if spec.AudioCopy != test.wantAudio {
				t.Errorf("audio copied as %q, want %q", spec.AudioCopy, test.wantAudio)
			}
		})
	}
}

// docs/status.md says copying both is built, and no longer says it is not.
func TestStatusSaysCopyingBothIsBuilt(t *testing.T) {
	raw, err := os.ReadFile("../../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, stale := range []string{"Copying both the video and the audio is not built", "Copying audio with a copied video: **not built**"} {
		if strings.Contains(text, stale) {
			t.Errorf("docs/status.md still says %q", stale)
		}
	}
	if !regexp.MustCompile(`(?i)cop(y|ies|ying|ied) (of )?both`).MatchString(text) {
		t.Error("docs/status.md does not say that copying both the video and the audio is built")
	}
}
