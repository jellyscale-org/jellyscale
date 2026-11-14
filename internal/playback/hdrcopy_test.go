package playback

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"jellyscale/internal/media"
)

// hdrCopyCapture is compat/captures/hdr-copy-12.4.json: what Jellyfin 12.4 did
// with plain HDR10, HLG and HDR10+ films that need a transcode only for their
// audio (see hdr-copy-12.4.md).
func loadHDRCopyCapture(t *testing.T) dolbyVisionCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/hdr-copy-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture dolbyVisionCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// plainHDRFilm is a copyable MKV with AC3 whose video is the stream the capture lists.
func plainHDRFilm(t *testing.T, video json.RawMessage) Source {
	t.Helper()
	source := copyable("mkv", "ac3")
	if err := json.Unmarshal(video, &source.Streams[0]); err != nil {
		t.Fatal(err)
	}
	return source
}

// A client whose profile lists the range of an HDR10, HLG or HDR10+ film gets
// the file's own video and a re-encoded audio track, as Jellyfin 12.4 sends it;
// a client whose profile does not list the range gets the tone-mapped
// re-encode, with VideoRangeTypeNotSupported among the reasons.
func TestDecideCopiesPlainHDRAsJellyfinDid(t *testing.T) {
	capture := loadHDRCopyCapture(t)
	if len(capture.Files) != 3 || len(capture.DeviceProfiles) != 6 {
		t.Fatalf("the capture has %d films and %d profiles", len(capture.Files), len(capture.DeviceProfiles))
	}
	copies := 0
	for file, entry := range capture.Files {
		for name, want := range entry.PlaybackInfo {
			t.Run(file+"/"+name, func(t *testing.T) {
				d := Decide(Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1))}, plainHDRFilm(t, entry.Video), true)
				if d.DirectPlay != want.SupportsDirectPlay {
					t.Fatalf("direct play %v, Jellyfin %v (%+v)", d.DirectPlay, want.SupportsDirectPlay, d)
				}
				if got, wantReasons := strings.Join(d.Reasons, ","), *want.TranscodeReasons; got != wantReasons {
					t.Errorf("reasons %s, Jellyfin %s", got, wantReasons)
				}
				wantCopy := copiedByJellyfin(want.MasterVariants)
				if d.Copy != wantCopy {
					t.Errorf("copy %v, Jellyfin %v (%v)", d.Copy, wantCopy, want.MasterVariants)
				}
				if wantCopy {
					copies++
				}
			})
		}
	}
	if copies != 14 {
		t.Errorf("%d decisions copy, the capture has 14", copies)
	}
}

// The master playlist of a copied HDR film says what the client will be shown:
// PQ or HLG, not SDR (a copy labelled SDR is shown washed out), the codec
// string, and for HDR10+ the cdm4 supplemental codec, as Jellyfin writes them.
func TestMasterNamesTheRangeOfACopiedPlainHDRFile(t *testing.T) {
	capture := loadHDRCopyCapture(t)
	main := strings.NewReplacer("hvc1.1.4.", "hvc1.1.6.")
	for file, entry := range capture.Files {
		t.Run(file, func(t *testing.T) {
			variants := entry.PlaybackInfo["hdr10"].MasterVariants
			source := plainHDRFilm(t, entry.Video)
			text := Master(Spec{Copy: true, Width: 640, Height: 360, VideoBitrate: 4_000_000, AudioBitrate: 128_000}, source, "q")
			first := strings.Split(strings.SplitN(text, "#EXT-X-STREAM-INF:", 2)[1], "\n")[0]
			got := map[string]string{}
			for _, key := range []string{"VIDEO-RANGE", "CODECS", "SUPPLEMENTAL-CODECS"} {
				if m := regexp.MustCompile(`[,:]` + key + `=("[^"]*"|[^,]*)`).FindStringSubmatch(":" + first); m != nil {
					got[key] = strings.Trim(m[1], `"`)
				}
			}
			for key, want := range variants[0] {
				if got[key] != main.Replace(want) {
					t.Errorf("%s is %q, Jellyfin writes %q\n%s", key, got[key], want, text)
				}
			}
			if len(got) != len(variants[0]) {
				t.Errorf("attributes %v, Jellyfin %v", got, variants[0])
			}
		})
	}
}

// Tone mapping stays for a client that cannot show the range: the decision is a
// re-encode, and the spec made for it maps the picture by the transfer function
// the file declares.
func TestPlainHDRIsToneMappedForAClientThatCannotShowIt(t *testing.T) {
	capture := loadHDRCopyCapture(t)
	for file, tone := range map[string]string{"Plain Pq Film (1991)": PQ, "Plain Plus Film (1993)": PQ, "Plain Hlg Film (1992)": HLG} {
		for _, name := range []string{"sdr", "hdr10only"} {
			if name == "hdr10only" && tone != HLG {
				continue // The profile lists HDR10 and HDR10+, so it copies those.
			}
			t.Run(file+"/"+name, func(t *testing.T) {
				source := plainHDRFilm(t, capture.Files[file].Video)
				d := Decide(Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1))}, source, true)
				if d.Copy || !strings.Contains(strings.Join(d.Reasons, ","), "VideoRangeTypeNotSupported") {
					t.Fatalf("got %+v", d)
				}
				spec, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, VideoBitrate: d.VideoBitrate, AudioBitrate: d.AudioBitrate}, source)
				if err != nil {
					t.Fatal(err)
				}
				if spec.Copy || spec.ToneMap != tone {
					t.Errorf("spec copy %v, tone map %q, want a re-encode mapped from %q", spec.Copy, spec.ToneMap, tone)
				}
			})
		}
	}
}

// The stream role accepts the copy of every plain HDR film in HEVC and AV1 that
// the api offers, and still refuses what no client could show: a film whose
// range was not scanned yet and a Dolby Vision profile 5 file.
func TestStreamAcceptsTheCopyOfPlainHDR(t *testing.T) {
	capture := loadHDRCopyCapture(t)
	for file, entry := range capture.Files {
		t.Run(file, func(t *testing.T) {
			source := plainHDRFilm(t, entry.Video)
			if !source.CanCopy() {
				t.Fatal("CanCopy is false")
			}
			if _, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, Copy: true}, source); err != nil {
				t.Errorf("copy refused: %v", err)
			}
		})
	}
	unscanned := plainHDRFilm(t, capture.Files["Plain Pq Film (1991)"].Video)
	unscanned.Streams[0].VideoRangeType = ""
	if unscanned.CanCopy() {
		t.Error("a film whose range is not scanned yet is copied")
	}
}

// Rolling update, same release: whatever Decide offers as a copy a stream
// replica of the same release serves, for every film and profile in the
// captures (a guard: it passes before JS-025, because nothing plain HDR was
// offered then, and fails if the decision outgrows what the spec accepts).
func TestStreamServesEveryCopyDecideOffers(t *testing.T) {
	var films []struct {
		name  string
		video json.RawMessage
		d     map[string]*DeviceProfile
	}
	for _, capture := range []dolbyVisionCapture{loadDolbyVisionCapture(t), loadHDRCopyCapture(t)} {
		for file, entry := range capture.Files {
			films = append(films, struct {
				name  string
				video json.RawMessage
				d     map[string]*DeviceProfile
			}{file, entry.Video, capture.DeviceProfiles})
		}
	}
	offered := 0
	for _, f := range films {
		for name, profile := range f.d {
			source := plainHDRFilm(t, f.video)
			d := Decide(Request{DeviceProfile: profile, AudioStreamIndex: ptr(Number(1))}, source, true)
			if !d.Copy {
				continue
			}
			offered++
			if _, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, Copy: true}, source); err != nil {
				t.Errorf("%s under %s: Decide offers a copy and the spec refuses it: %v", f.name, name, err)
			}
		}
	}
	if offered < 30 {
		t.Errorf("only %d copies were offered, so the check looked at too little", offered)
	}
}

// Rolling update, the previous release's api: every copy it offered (SDR, a Dolby
// Vision base layer, and H.264 in any range) is still served by this release's
// stream replica, whatever the order the replicas are replaced in.
func TestStreamServesEveryCopyThePreviousReleaseOffered(t *testing.T) {
	for name, change := range map[string]struct {
		codec string
		set   func(*Stream)
	}{
		"hevc sdr":                 {"hevc", func(v *Stream) { v.VideoRangeType = "SDR" }},
		"av1 sdr":                  {"av1", func(v *Stream) { v.VideoRangeType = "SDR" }},
		"h264 sdr":                 {"h264", func(v *Stream) { v.VideoRangeType = "SDR" }},
		"h264 hdr10":               {"h264", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HDR10", media.Ptr(PQ) }},
		"h264 hlg":                 {"h264", func(v *Stream) { v.VideoRangeType, v.ColorTransfer = "HLG", media.Ptr(HLG) }},
		"dolby vision 8.1":         {"hevc", func(v *Stream) { v.VideoRangeType = "DOVIWithHDR10" }},
		"dolby vision 8.4":         {"hevc", func(v *Stream) { v.VideoRangeType = "DOVIWithHLG" }},
		"dolby vision 8.2":         {"hevc", func(v *Stream) { v.VideoRangeType = "DOVIWithSDR" }},
		"dolby vision 7":           {"hevc", func(v *Stream) { v.VideoRangeType = "DOVIWithEL" }},
		"dolby vision 10.1 in av1": {"av1", func(v *Stream) { v.VideoRangeType = "DOVIWithHDR10" }},
	} {
		t.Run(name, func(t *testing.T) {
			source := copyable("mkv", "ac3")
			video := &source.Streams[0]
			video.Codec, video.Profile, video.Level, video.BitDepth = change.codec, "Main 10", 153, 10
			if change.codec == "av1" {
				video.Profile, video.Level = "Main", 8
			}
			if change.codec == "h264" {
				video.Profile, video.Level = "High 10", 41
			}
			change.set(video)
			if _, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, Copy: true}, source); err != nil {
				t.Errorf("copy refused: %v", err)
			}
		})
	}
}

// An api of this release offers a copy of plain HDR that a stream replica of the
// previous release refuses (it copies SDR, Dolby Vision and H.264 only), so the
// order of a rolling update matters, and docs/running.md must give it:
// stream replicas first, then api replicas.
func TestRunningDocGivesTheRollOrder(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	var section string
	for _, part := range strings.Split("\n"+string(raw), "\n## ")[1:] {
		if heading, _, _ := strings.Cut(part, "\n"); regexp.MustCompile(`(?i)roll|upgrad|update`).MatchString(heading) {
			section = strings.ToLower(part)
		}
	}
	if section == "" {
		t.Fatal("docs/running.md has no section on a rolling update")
	}
	stream, api := strings.Index(section, "stream"), strings.Index(section, "api")
	if stream < 0 || api < 0 || stream > api || !regexp.MustCompile(`first|before`).MatchString(section) {
		t.Errorf("the section does not say stream replicas are replaced before api replicas:\n%s", section)
	}
	if !strings.Contains(section, "hdr") {
		t.Errorf("the section does not say why (the copy of HDR video):\n%s", section)
	}
}
