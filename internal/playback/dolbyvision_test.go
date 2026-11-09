package playback

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// dolbyVisionCapture is compat/captures/dolby-vision-12.4.json: what Jellyfin
// 12.4 did with a film of each kind of HDR (see dolby-vision-12.4.md).
type dolbyVisionCapture struct {
	DeviceProfiles map[string]*DeviceProfile `json:"deviceProfiles"`
	Files          map[string]struct {
		Video        json.RawMessage `json:"video"`
		PlaybackInfo map[string]struct {
			SupportsDirectPlay bool
			TranscodeReasons   *string
			MasterVariants     []map[string]string `json:"masterVariants"`
		} `json:"playbackInfo"`
	} `json:"files"`
}

func loadDolbyVisionCapture(t *testing.T) dolbyVisionCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/dolby-vision-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture dolbyVisionCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// capturedFilm is a copyable MP4 with AAC whose video stream is the one the
// capture lists, as the scan stores it: Jellyfin's own keys (VideoRangeType,
// DvProfile, DvBlSignalCompatibilityId, Hdr10PlusPresentFlag and the like).
func capturedFilm(t *testing.T, video json.RawMessage) Source {
	t.Helper()
	source := copyable("mp4", "aac")
	if err := json.Unmarshal(video, &source.Streams[0]); err != nil {
		t.Fatal(err)
	}
	return source
}

// copiedByJellyfin is true when the first variant of the master playlist is
// not the H.264 re-encode: the file's own video is sent.
func copiedByJellyfin(variants []map[string]string) bool {
	return len(variants) > 0 && !strings.HasPrefix(variants[0]["CODECS"], "avc1")
}

// Decide gives what Jellyfin 12.4 did for each kind of HDR film against each
// kind of jellyfin-web profile: direct play or not, the reasons, and whether
// the video is sent as it is. A Dolby Vision file whose base layer the profile
// plays (DOVIWithHDR10 for a profile with HDR10, DOVIWithSDR for SDR and so on)
// is copied, not re-encoded; profile 5 never is.
func TestDecideMatchesJellyfinForEveryKindOfHDR(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	if len(capture.Files) != 15 || len(capture.DeviceProfiles) != 5 {
		t.Fatalf("the capture has %d films and %d profiles", len(capture.Files), len(capture.DeviceProfiles))
	}
	for file, entry := range capture.Files {
		for name, want := range entry.PlaybackInfo {
			t.Run(file+"/"+name, func(t *testing.T) {
				source := capturedFilm(t, entry.Video)
				d := Decide(Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1))}, source, true)
				if d.DirectPlay != want.SupportsDirectPlay {
					t.Fatalf("direct play %v, Jellyfin %v (%+v)", d.DirectPlay, want.SupportsDirectPlay, d)
				}
				if d.DirectPlay {
					return
				}
				if got, wantReasons := strings.Join(d.Reasons, ","), *want.TranscodeReasons; got != wantReasons {
					t.Errorf("reasons %s, Jellyfin %s", got, wantReasons)
				}
				if wantCopy := copiedByJellyfin(want.MasterVariants); d.Copy != wantCopy {
					t.Errorf("copy %v, Jellyfin %v (%v)", d.Copy, wantCopy, want.MasterVariants)
				}
			})
		}
	}
}

// The master playlist of a copied file names the base layer the client will
// play: its range, the plain codec string, and for Dolby Vision the codec
// string and the compatibility brand of the Dolby Vision stream it also is
// (SUPPLEMENTAL-CODECS), exactly as Jellyfin writes them.
func TestMasterNamesTheBaseLayerOfACopiedDolbyVisionFile(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	// jellyscale writes hvc1.1.6 for Main, where Jellyfin writes hvc1.1.4: the
	// same stream (compat/captures/hevc-av1-12.4.md).
	main := strings.NewReplacer("hvc1.1.4.", "hvc1.1.6.")
	copies := 0
	for file, entry := range capture.Files {
		var variants []map[string]string
		for _, decision := range entry.PlaybackInfo {
			if copiedByJellyfin(decision.MasterVariants) {
				variants = decision.MasterVariants
			}
		}
		if variants == nil {
			continue
		}
		copies++
		t.Run(file, func(t *testing.T) {
			source := capturedFilm(t, entry.Video)
			spec := Spec{Copy: true, Width: 640, Height: 360, VideoBitrate: 4_000_000, AudioBitrate: 128_000}
			text := Master(spec, source, "q")
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
	if copies != 8 {
		t.Errorf("%d films are copied in the capture, want 8", copies)
	}
}

// What is encoded for a Dolby Vision file is the tone-mapped SDR picture, and
// the playlist says so, however the file is labelled.
func TestMasterOfAnEncodedDolbyVisionFileIsSDR(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	source := capturedFilm(t, capture.Files["Dolby Eight One Film (1996)"].Video)
	text := Master(Spec{Width: 640, Height: 360, VideoBitrate: 4_000_000, AudioBitrate: 128_000}, source, "q")
	for _, want := range []string{"VIDEO-RANGE=SDR", `CODECS="avc1.640033,mp4a.40.2"`} {
		if !strings.Contains(text, want) {
			t.Errorf("master lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SUPPLEMENTAL-CODECS") {
		t.Errorf("an encoded stream is not Dolby Vision:\n%s", text)
	}
}

// A client that edits the URL to ask for the file's own video gets it only
// where Jellyfin would send it: not for profile 5, whose base layer is no
// picture a client without Dolby Vision can show (it comes out green and
// purple). HDR10 without Dolby Vision is copied since JS-025.
func TestCopyOfDolbyVisionIsOnlyForABaseLayerThatIsAPicture(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	for file, copy := range map[string]bool{
		"Dolby Eight One Film (1996)":      true,
		"Dolby Eight One Plus Film (2002)": true,
		"Dolby Eight Four Film (1998)":     true,
		"Dolby Eight Two Film (1997)":      true,
		"Dolby Ten Film (2001)":            true,
		"Dolby Seven Film (1995)":          true,
		"Dolby Eight Zero Film (1999)":     true,
		"Dolby Five Film (1994)":           false,
		"Dolby Five Plus Film (2004)":      false,
		"Plain Pq Film (1991)":             true,
	} {
		t.Run(file, func(t *testing.T) {
			source := capturedFilm(t, capture.Files[file].Video)
			_, err := NewSpec("11111111-2222-3333-4444-555555555555", Params{SourceID: sourceID, AudioIndex: 1, Copy: true}, source)
			if (err == nil) != copy {
				t.Errorf("copy spec: %v, want allowed %v", err, copy)
			}
			if source.CanCopy() != copy {
				t.Errorf("CanCopy %v, want %v", !copy, copy)
			}
		})
	}
}

// Profile 5 is never sent as it is, or direct played, to a client whose profile
// lists no Dolby Vision, whatever else the profile allows; with HDR, SDR and
// HLG in its list a profile 5 file is still re-encoded, and tone mapped as before.
func TestDolbyVisionProfile5IsNeverSentToAClientWithoutDolbyVision(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	for _, file := range []string{"Dolby Five Film (1994)", "Dolby Five Plus Film (2004)"} {
		for _, name := range []string{"sdr", "hdr10", "hdr10only"} {
			t.Run(file+"/"+name, func(t *testing.T) {
				d := Decide(Request{DeviceProfile: capture.DeviceProfiles[name], AudioStreamIndex: ptr(Number(1))}, capturedFilm(t, capture.Files[file].Video), true)
				if d.DirectPlay || d.Copy || !slices.Contains(d.Reasons, "VideoRangeTypeNotSupported") {
					t.Errorf("got %+v", d)
				}
			})
		}
	}
}

// The labels jellyfin-web writes into its profiles' VideoRangeType conditions
// (the hevc CodecProfile of a browser with no HDR, with HDR10, with HLG, with
// Dolby Vision, and of a TV with every type) are matched against every
// VideoRangeType Jellyfin gives a file. The video is not copyable here, so the
// answer is the condition's alone: direct play when the label is listed, else
// VideoRangeTypeNotSupported. HDR10+ is HDR10 with more, so a list with HDR10 takes it.
func TestEveryVideoRangeTypeAgainstJellyfinWebsConditions(t *testing.T) {
	capture := loadDolbyVisionCapture(t)
	listed := map[string]string{
		"no hdr":       "SDR",
		"hdr10":        "SDR|HDR10|HDR10Plus",
		"hdr10, hlg":   "SDR|HDR10|HDR10Plus|HLG",
		"hdr10 alone":  "SDR|HDR10",
		"dolby vision": "SDR|HDR10|HDR10Plus|HLG|DOVI|DOVIWithHDR10|DOVIWithHLG|DOVIWithSDR|DOVIWithHDR10Plus",
		"a tv":         "SDR|DOVIWithSDR|HDR10|HDR10Plus|DOVIWithHDR10|DOVIWithHDR10Plus|DOVIWithEL|DOVIWithELHDR10Plus|DOVIInvalid|HLG|DOVIWithHLG",
	}
	types := []string{"SDR", "HDR10", "HDR10Plus", "HLG", "DOVI", "DOVIWithHDR10", "DOVIWithHDR10Plus", "DOVIWithHLG", "DOVIWithSDR",
		"DOVIWithEL", "DOVIWithELHDR10Plus", "DOVIInvalid", "Unknown"}
	for name, ranges := range listed {
		for _, rangeType := range types {
			t.Run(name+"/"+rangeType, func(t *testing.T) {
				raw, _ := json.Marshal(capture.DeviceProfiles["hdr10"])
				var profile DeviceProfile
				_ = json.Unmarshal(raw, &profile)
				for _, codec := range profile.CodecProfiles {
					for i, c := range codec.Conditions {
						if c.Property == "VideoRangeType" {
							codec.Conditions[i].Value = ranges
						}
					}
				}
				source := capturedFilm(t, capture.Files["Plain Pq Film (1991)"].Video)
				source.Keyframes = nil // a copy cannot be cut, so it is direct play or a re-encode
				source.Streams[0].VideoRangeType = rangeType
				d := Decide(Request{DeviceProfile: &profile, AudioStreamIndex: ptr(Number(1))}, source, true)
				// Jellyfin plays HDR10+ where HDR10 is listed (Plain Plus Film, profile hdr10only).
				want := slices.Contains(strings.Split(ranges, "|"), rangeType) || rangeType == "HDR10Plus" && strings.Contains(ranges+"|", "|HDR10|")
				if d.DirectPlay != want {
					t.Fatalf("direct play %v, want %v: %+v", d.DirectPlay, want, d)
				}
				if !d.DirectPlay && (d.Copy || !slices.Contains(d.Reasons, "VideoRangeTypeNotSupported")) {
					t.Errorf("got %+v", d)
				}
			})
		}
	}
}
