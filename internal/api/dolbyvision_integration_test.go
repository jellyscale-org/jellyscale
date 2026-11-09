//go:build integration

package api

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// PlaybackInfo decides for each kind of HDR film and each kind of jellyfin-web
// profile as Jellyfin 12.4 did (compat/captures/dolby-vision-12.4.json): direct
// play, or the file's own video in an HLS stream when the profile plays the base
// layer of a Dolby Vision file, or a tone-mapped re-encode, with the same
// TranscodeReasons; and a copy's master playlist names the base layer, its range
// and the Dolby Vision brand. Profile 5 is only ever played as it is by a profile
// that lists DOVI.
func TestPlaybackInfoDecidesForDolbyVisionAsJellyfinDid(t *testing.T) {
	playbackInfoDecidesAsJellyfinDid(t, "dolby-vision-12.4.json", "mp4", "aac")
}

// The same for the plain HDR10, HLG and HDR10+ films of hdr-copy-12.4.json, which
// need a transcode for their AC3 audio: a client that lists the range is sent the
// file's own video (copied) and a master playlist that says PQ or HLG.
func TestPlaybackInfoCopiesPlainHDRAsJellyfinDid(t *testing.T) {
	playbackInfoDecidesAsJellyfinDid(t, "hdr-copy-12.4.json", "mkv", "ac3")
}

func playbackInfoDecidesAsJellyfinDid(t *testing.T, captureFile, container, audio string) {
	raw, err := os.ReadFile("../../compat/captures/" + captureFile)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		DeviceProfiles map[string]json.RawMessage
		Files          map[string]struct {
			Video        json.RawMessage
			PlaybackInfo map[string]struct {
				SupportsDirectPlay bool
				TranscodeReasons   *string
				MasterVariants     []map[string]string
			}
		}
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	e, _, source := transcodeEnv(t, "hevc", audio)
	mustExec(t, e.pool, `UPDATE media_sources SET container = '`+container+`', keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', video_end = 900000000, size = 25000000`)
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
	// jellyscale writes hvc1.1.6 for Main, where Jellyfin writes hvc1.1.4: the same stream.
	main := strings.NewReplacer("hvc1.1.4.", "hvc1.1.6.")
	for file, entry := range capture.Files {
		mustExec(t, e.pool, `UPDATE media_sources SET streams = jsonb_build_array($1::jsonb,
			jsonb_build_object('Index', 1, 'Type', 'Audio', 'Codec', $2::text, 'IsDefault', true, 'Channels', 2),
			jsonb_build_object('Index', 2, 'Type', 'Audio', 'Codec', $2::text, 'Channels', 2))`, string(entry.Video), audio)
		for name, want := range entry.PlaybackInfo {
			t.Run(file+"/"+name, func(t *testing.T) {
				body := map[string]any{"DeviceProfile": capture.DeviceProfiles[name], "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source}
				media := firstSource(e.playbackInfo(t, 0, e.item, body))
				if media["SupportsDirectPlay"] != want.SupportsDirectPlay {
					t.Fatalf("SupportsDirectPlay %v, Jellyfin %v: %v", media["SupportsDirectPlay"], want.SupportsDirectPlay, media)
				}
				if want.SupportsDirectPlay {
					if media["TranscodingUrl"] != nil {
						t.Errorf("a direct play with a TranscodingUrl: %v", media["TranscodingUrl"])
					}
					return
				}
				transcodingURL := media["TranscodingUrl"].(string)
				query := mustQuery(t, transcodingURL)
				if query.Get("TranscodeReasons") != *want.TranscodeReasons {
					t.Errorf("TranscodeReasons %s, Jellyfin %s", query.Get("TranscodeReasons"), *want.TranscodeReasons)
				}
				copied := !strings.HasPrefix(want.MasterVariants[0]["CODECS"], "avc1")
				if got := query.Get("VideoCodec") == "copy"; got != copied {
					t.Fatalf("VideoCodec %s, Jellyfin copies: %v", query.Get("VideoCodec"), copied)
				}
				_, master := e.do(t, 1, "GET", transcodingURL, "", nil)
				first := strings.Split(strings.SplitN(string(master), "#EXT-X-STREAM-INF:", 2)[1], "\n")[0]
				got := map[string]string{}
				for _, key := range []string{"VIDEO-RANGE", "CODECS", "SUPPLEMENTAL-CODECS"} {
					if m := regexp.MustCompile(`[,:]` + key + `=("[^"]*"|[^,]*)`).FindStringSubmatch(":" + first); m != nil {
						got[key] = strings.Trim(m[1], `"`)
					}
				}
				for key, value := range want.MasterVariants[0] {
					if got[key] != main.Replace(value) {
						t.Errorf("the master's %s is %q, Jellyfin writes %q:\n%s", key, got[key], value, master)
					}
				}
				if len(got) != len(want.MasterVariants[0]) {
					t.Errorf("the master's first variant has %v, Jellyfin %v", got, want.MasterVariants[0])
				}
			})
		}
	}
}
