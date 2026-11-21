//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
)

// JS-153: what the decision line says. The wording is the kit's (decision_kit_integration_test.go). Each check asks the
// replica as a client does and reads the line from the replica's log. Every check that asserts a thing ABSENT from a line
// (no title, no "direct stream", no line on a failure) is paired with a decision line of the same run that must exist, so a
// build that logs under another message passes none of them.

// shape asserts what every decision line of a reply has, whatever the case: one line per source listed, the method the
// reply shows, the reasons of the TranscodingUrl inside the line's, a non-empty list for a copy, re-encode or refusal, the
// request's identity and no direct stream.
func shape(t *testing.T, e *env, reply map[string]any, lines []decisionLine, item string) {
	t.Helper()
	sources, _ := reply["MediaSources"].([]any)
	want := len(sources)
	if want == 0 {
		want = 1 // A refused reply has one line.
	}
	if len(lines) != want {
		t.Fatalf("the reply lists %d sources: %d decision lines, want %d: %v", len(sources), len(lines), want, lines)
	}
	for i, line := range lines {
		method := line["method"]
		if len(sources) > 0 {
			source := sources[i].(map[string]any)
			wantMethod := "direct_play"
			if _, offered := source["TranscodingUrl"]; offered {
				wantMethod = "reencode"
				if strings.Contains(source["TranscodingUrl"].(string), "VideoCodec=copy") {
					wantMethod = "copy"
				}
			}
			if method != wantMethod {
				t.Errorf("source %d: the line says method=%q, the reply is a %s: %v", i, method, wantMethod, line)
			}
			for _, name := range transcodeReasonsOf(t, source) {
				if !line.has("reasons", name) {
					t.Errorf("source %d: TranscodeReasons names %s, the line's reasons %q do not", i, name, line["reasons"])
				}
			}
			if line["source"] != db.DashedID(source["Id"].(string)) {
				t.Errorf("source %d: the line's source %q is not the reply's %s, dashed", i, line["source"], source["Id"])
			}
		} else if method != "refused" {
			t.Errorf("a refused reply: the line says method=%q: %v", method, line)
		}
		if method != "direct_play" && len(line.reasonList("reasons")) == 0 {
			t.Errorf("a %s line has no reasons: %v", method, line)
		}
		if line["play_session"] != reply["PlaySessionId"] || line["play_session"] == "" {
			t.Errorf("play_session %q, the reply's PlaySessionId %v", line["play_session"], reply["PlaySessionId"])
		}
		for key, want := range map[string]string{"client": "Probe Client", "client_version": "9.9.9", "device": "Probe Device", "device_id": "probe-dev-1",
			"user": e.user, "item": db.DashedID(item)} {
			if line[key] != want {
				t.Errorf("%s=%q, want %q: %v", key, line[key], want, line)
			}
		}
		flat := strings.ToLower(strings.Join(mapValues(line), " "))
		for _, banned := range []string{"direct stream", "direct_stream", "directstream"} {
			if strings.Contains(flat, banned) {
				t.Errorf("the line says %q: jellyscale has no separate direct stream: %v", banned, line)
			}
		}
		if strings.Contains(flat, "viewer") { // The account's name is never logged: the user id is.
			t.Errorf("the line carries the account's name: %v", line)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// One film and one request, and what the line must say. reasons are all required; only lists the names a copy, a re-encode
// or a refusal must have exactly when it is set; not are names the line must lack.
type decisionCase struct {
	name    string
	film    storedFilm
	body    func(t *testing.T) map[string]any
	method  string
	reasons []string
	exact   bool
	not     []string
	fields  map[string]string
}

func chrome(t *testing.T) map[string]any {
	return map[string]any{"DeviceProfile": chromeProfile(t, nil), "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MaxStreamingBitrate": 2147483647}
}

func withProfile(edit func(p map[string]any)) func(t *testing.T) map[string]any {
	return func(t *testing.T) map[string]any {
		b := chrome(t)
		b["DeviceProfile"] = chromeProfile(t, edit)
		return b
	}
}

func withBody(extra map[string]any, edit func(p map[string]any)) func(t *testing.T) map[string]any {
	return func(t *testing.T) map[string]any {
		b := withProfile(edit)(t)
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
}

// anyRange takes away every range condition of the profile, so that only what jellyscale cannot copy is left.
func anyRange(p map[string]any) {
	for _, entry := range p["CodecProfiles"].([]any) {
		c := entry.(map[string]any)
		var kept []any
		for _, condition := range c["Conditions"].([]any) {
			if condition.(map[string]any)["Property"] != "VideoRangeType" {
				kept = append(kept, condition)
			}
		}
		c["Conditions"] = kept
	}
}

var ac3 = []map[string]any{{"Index": 1, "Type": "Audio", "Codec": "ac3", "IsDefault": true, "Channels": 2}}

var hevcFilm = storedFilm{video: map[string]any{"Codec": "hevc", "Profile": "Main", "Level": 93}}

var decisionCases = []decisionCase{
	{name: "direct play", body: func(t *testing.T) map[string]any { return chrome(t) }, method: "direct_play",
		fields: map[string]string{"audio_asked": "1", "audio_index": "1", "audio_codec": "aac", "audio_language": "eng", "audio_by": "client", "subtitle_asked": "-1"}},
	{name: "a copy for the audio codec", film: storedFilm{audio: ac3},
		body: func(t *testing.T) map[string]any { return chrome(t) }, method: "copy", reasons: []string{"AudioCodecNotSupported"}, exact: true,
		fields: map[string]string{"out_codec": "h264", "out_height": "720", "out_range": "SDR", "src_height": "720", "tone_mapped": "no", "ceiling_lowered": "no"}},
	{name: "a copy for the container", film: storedFilm{container: "avi"}, body: func(t *testing.T) map[string]any { return chrome(t) },
		method: "copy", reasons: []string{"ContainerNotSupported"}, exact: true},
	{name: "a copy for the audio channels", film: storedFilm{audio: []map[string]any{{"Index": 1, "Type": "Audio", "Codec": "aac", "IsDefault": true, "Channels": 6}}},
		body: withProfile(func(p map[string]any) {
			p["CodecProfiles"] = append(p["CodecProfiles"].([]any), map[string]any{"Type": "VideoAudio", "Conditions": []any{
				map[string]any{"Condition": "LessThanEqual", "Property": "AudioChannels", "Value": "2", "IsRequired": false}}})
		}), method: "copy", reasons: []string{"AudioChannelsNotSupported"}},
	{name: "a re-encode for the video codec", film: hevcFilm, body: func(t *testing.T) map[string]any { return chrome(t) },
		method: "reencode", reasons: []string{"VideoCodecNotSupported"},
		fields: map[string]string{"out_codec": "h264", "out_height": "720", "out_range": "SDR", "src_height": "720", "tone_mapped": "no", "ceiling_lowered": "no"}},
	{name: "a re-encode for a file over the client profile's cap", film: storedFilm{bitrate: 2000000},
		body: func(t *testing.T) map[string]any { // The request names no cap, so the profile's applies.
			b := withProfile(func(p map[string]any) { p["MaxStreamingBitrate"] = 1000000 })(t)
			delete(b, "MaxStreamingBitrate")
			return b
		}, method: "reencode", reasons: []string{"ContainerBitrateExceedsLimit"}},
	{name: "a re-encode for a file over the request's cap", film: storedFilm{bitrate: 2000000},
		body: withBody(map[string]any{"MaxStreamingBitrate": 1000000}, nil), method: "reencode", reasons: []string{"ContainerBitrateExceedsLimit"}},
	{name: "a re-encode for a cap on a file of unknown bitrate", film: storedFilm{bitrate: -1, audio: ac3},
		body: withBody(map[string]any{"MaxStreamingBitrate": 1000000}, nil), method: "reencode", reasons: []string{"AudioCodecNotSupported", "BitrateUnknownUnderCap"}},
	{name: "a copy of the file's second audio track the client cannot pick itself", body: withBody(map[string]any{"AudioStreamIndex": "2"}, nil),
		method: "copy", reasons: []string{"SecondaryAudioNotSupported"}, not: []string{"AudioCodecNotSupported"},
		fields: map[string]string{"audio_asked": "2", "audio_index": "2", "audio_codec": "aac", "audio_language": "fra", "audio_by": "client"}},
	{name: "a text subtitle burned in", body: withBody(map[string]any{"SubtitleStreamIndex": "3"}, func(p map[string]any) { p["SubtitleProfiles"] = []any{} }),
		method: "reencode", reasons: []string{"SubtitleCodecNotSupported"},
		fields: map[string]string{"subtitle_asked": "3", "subtitle_index": "3", "subtitle_codec": "subrip", "subtitle_language": "eng", "subtitle_kind": "text"}},
	{name: "an image subtitle burned in", film: storedFilm{subs: []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "PGSSUB", "IsImageSubtitleStream": true, "Language": "deu"}}},
		body: withBody(map[string]any{"SubtitleStreamIndex": "3"}, nil), method: "reencode", reasons: []string{"SubtitleCodecNotSupported"},
		fields: map[string]string{"subtitle_asked": "3", "subtitle_index": "3", "subtitle_codec": "PGSSUB", "subtitle_language": "deu", "subtitle_kind": "image"}},
	{name: "a subtitle beside the film burned in", film: storedFilm{subs: []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true, "IsExternal": true, "Language": "spa", "Path": "x/Film.es.srt"}}},
		body: withBody(map[string]any{"SubtitleStreamIndex": "3"}, func(p map[string]any) { p["SubtitleProfiles"] = []any{} }), method: "reencode",
		reasons: []string{"SubtitleCodecNotSupported"}, fields: map[string]string{"subtitle_index": "3", "subtitle_codec": "subrip", "subtitle_language": "spa", "subtitle_kind": "text"}},
	{name: "a subtitle index that is no subtitle track", body: withBody(map[string]any{"SubtitleStreamIndex": "2"}, nil), method: "direct_play",
		fields: map[string]string{"subtitle_asked": "2", "subtitle_index": "none"}},
	{name: "no subtitle asked", body: func(t *testing.T) map[string]any { b := chrome(t); delete(b, "SubtitleStreamIndex"); return b }, method: "direct_play",
		fields: map[string]string{"subtitle_asked": "none", "subtitle_index": "none"}},
	{name: "an audio index that is no audio track", body: withBody(map[string]any{"AudioStreamIndex": "7"}, nil), method: "direct_play",
		fields: map[string]string{"audio_asked": "7", "audio_index": "1", "audio_by": "server"}},
	{name: "no audio index asked", body: func(t *testing.T) map[string]any { b := chrome(t); delete(b, "AudioStreamIndex"); return b }, method: "direct_play",
		fields: map[string]string{"audio_asked": "none", "audio_index": "1", "audio_by": "server"}},
	// What a copy could not have names itself.
	{name: "an HEVC film with no keyframe index under a profile that takes HEVC", film: storedFilm{video: hevcFilm.video, keyframes: "none"},
		body: withProfile(func(p map[string]any) { takesInHLS(p, "hevc") }), method: "reencode", reasons: []string{"KeyframeIndexUnusable"}},
	{name: "an H.264 film with no keyframe index", film: storedFilm{keyframes: "none", audio: ac3}, body: func(t *testing.T) map[string]any { return chrome(t) },
		method: "reencode", reasons: []string{"AudioCodecNotSupported", "KeyframeIndexUnusable"}},
	{name: "an H.264 film whose keyframes are too far apart", film: storedFilm{keyframes: "{0,300000000,600000000}", audio: ac3}, body: func(t *testing.T) map[string]any { return chrome(t) },
		method: "reencode", reasons: []string{"AudioCodecNotSupported", "KeyframeIndexUnusable"}},
	{name: "a film in a codec that is never copied", film: storedFilm{video: map[string]any{"Codec": "mpeg2video", "Profile": "Main"}, keyframes: "none"},
		body: func(t *testing.T) map[string]any { return chrome(t) }, method: "reencode", reasons: []string{"VideoCodecNotSupported"}, not: []string{"KeyframeIndexUnusable"}},
	{name: "H.264 in HDR", film: storedFilm{video: map[string]any{"VideoRangeType": "HDR10", "ColorTransfer": "smpte2084"}, audio: ac3},
		body: withProfile(anyRange), method: "reencode", reasons: []string{"AudioCodecNotSupported", "VideoRangeNotCopyable"}, not: []string{"VideoRangeTypeNotSupported"}},
	{name: "Dolby Vision profile 5", film: storedFilm{video: map[string]any{"Codec": "hevc", "Profile": "Main 10", "Level": 93, "VideoRangeType": "DOVI", "DvProfile": 5, "ColorTransfer": "smpte2084"}, audio: ac3},
		body: withProfile(func(p map[string]any) { takesInHLS(p, "hevc"); anyRange(p) }), method: "reencode", reasons: []string{"AudioCodecNotSupported", "VideoRangeNotCopyable"},
		fields: map[string]string{"tone_mapped": "no"}},
	{name: "a range that is not scanned yet", film: storedFilm{video: map[string]any{"VideoRangeType": ""}, audio: ac3},
		body: func(t *testing.T) map[string]any { return chrome(t) }, method: "reencode", reasons: []string{"AudioCodecNotSupported", "VideoRangeUnknown"}},
	{name: "a codec profile that cannot be named", film: storedFilm{video: map[string]any{"Codec": "hevc", "Profile": "Main 4:4:4 Intra", "Level": 93}, audio: ac3},
		body: withProfile(func(p map[string]any) { takesInHLS(p, "hevc") }), method: "reencode", reasons: []string{"VideoProfileNotSupported"}},
	{name: "a level the client refuses on a film whose container it lacks", film: storedFilm{container: "avi"},
		body: withProfile(func(p map[string]any) { codecLimit(p, [3]string{"VideoLevel", "LessThanEqual", "30"}) }), method: "reencode",
		reasons: []string{"ContainerNotSupported", "VideoLevelNotSupported"}},
	{name: "a resolution the client refuses on a film whose container it lacks", film: storedFilm{container: "avi"},
		body: withProfile(func(p map[string]any) { codecLimit(p, [3]string{"Height", "LessThanEqual", "480"}) }), method: "reencode",
		reasons: []string{"ContainerNotSupported", "VideoResolutionNotSupported"}},
	{name: "a profile the client refuses on a film whose container it lacks", film: storedFilm{container: "avi"},
		body: withProfile(func(p map[string]any) { codecLimit(p, [3]string{"VideoProfile", "EqualsAny", "high"}) }), method: "reencode",
		reasons: []string{"ContainerNotSupported", "VideoProfileNotSupported"}},
	{name: "a range the client refuses on a film whose container it lacks", film: storedFilm{container: "avi", video: map[string]any{"Codec": "hevc", "Profile": "Main 10", "Level": 93, "VideoRangeType": "HDR10", "ColorTransfer": "smpte2084"}},
		body: withProfile(func(p map[string]any) {
			takesInHLS(p, "hevc")
			codecLimit(p, [3]string{"VideoRangeType", "EqualsAny", "SDR"})
		}), method: "reencode",
		reasons: []string{"ContainerNotSupported", "VideoRangeTypeNotSupported"}, fields: map[string]string{"src_range": "HDR10", "tone_mapped": "yes", "out_range": "SDR"}},
	{name: "two reasons are both named", film: storedFilm{bitrate: 2000000, audio: ac3},
		body: withBody(map[string]any{"MaxStreamingBitrate": 1000000}, nil), method: "reencode",
		reasons: []string{"AudioCodecNotSupported", "ContainerBitrateExceedsLimit"}},
}

func TestTheDecisionLineSaysHowEachCasePlaysAndWhy(t *testing.T) {
	for _, c := range decisionCases {
		t.Run(c.name, func(t *testing.T) {
			f := c.film
			if f.bitrate == -1 { // Unknown: the source row carries no bitrate.
				f.bitrate = 0
			}
			e, _, log := decisionEnv(t, f)
			if c.film.bitrate == -1 {
				mustExec(t, e.pool, `UPDATE items SET dto = dto #- '{MediaSources,0,Bitrate}'`)
			}
			body := c.body(t)
			body["MediaSourceId"] = db.CompactID(e.item)
			reply, lines, status := e.decide(t, log, infoRequest{body: body})
			if status != 200 {
				t.Fatalf("PlaybackInfo: %d", status)
			}
			shape(t, e, reply, lines, e.item)
			line := lines[0]
			if line["method"] != c.method {
				t.Errorf("method %q, want %q: %v", line["method"], c.method, line)
			}
			for _, name := range c.reasons {
				if !line.has("reasons", name) {
					t.Errorf("reasons %q lack %s", line["reasons"], name)
				}
			}
			if c.exact && len(line.reasonList("reasons")) != len(c.reasons) {
				t.Errorf("reasons %q, want exactly %v", line["reasons"], c.reasons)
			}
			for _, name := range c.not {
				if line.has("reasons", name) {
					t.Errorf("reasons %q hold %s", line["reasons"], name)
				}
			}
			for key, want := range c.fields {
				if got := line[key]; got != want && !(want == "none" && got == "") {
					t.Errorf("%s=%q, want %q", key, line[key], want)
				}
			}
			if line["audio_asked"] == "0" && body["AudioStreamIndex"] == nil {
				t.Errorf("an absent audio index is printed as 0")
			}
		})
	}
}

// The reasons a copy could not have: a re-encode line holds one of them and says why the video was not sent as it is.
func TestAReencodeLineAlwaysHoldsAReasonACopyCouldNotHave(t *testing.T) {
	canHave := []string{"AudioCodecNotSupported", "ContainerNotSupported", "AudioChannelsNotSupported", "SecondaryAudioNotSupported"}
	for _, c := range decisionCases {
		if c.method != "reencode" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			e, _, log := decisionEnv(t, c.film)
			body := c.body(t)
			body["MediaSourceId"] = db.CompactID(e.item)
			_, lines, _ := e.decide(t, log, infoRequest{body: body})
			if len(lines) != 1 || lines[0]["method"] != "reencode" {
				t.Fatalf("no re-encode line: %v", lines)
			}
			only := true
			for _, name := range lines[0].reasonList("reasons") {
				only = only && slices.Contains(canHave, name)
			}
			if only {
				t.Errorf("a re-encode whose reasons %q are all ones a copy could have", lines[0]["reasons"])
			}
		})
	}
}

// The viewer's saved language chooses a track that is not the file's default: the line says the server chose it, and that a
// client that cannot pick it itself needs the transcode for it.
func TestASavedLanguageThatChoosesANonDefaultTrackIsTheServersChoiceAndASecondaryAudioReason(t *testing.T) {
	e, _, log := decisionEnv(t, storedFilm{})
	mustExec(t, e.pool, `UPDATE users SET configuration = '{"AudioLanguagePreference":"fra","PlayDefaultAudioTrack":false}' WHERE id = $1`, e.user)
	body := chrome(t)
	delete(body, "AudioStreamIndex")
	body["MediaSourceId"] = db.CompactID(e.item)
	reply, lines, _ := e.decide(t, log, infoRequest{body: body})
	shape(t, e, reply, lines, e.item)
	line := lines[0]
	if line["audio_index"] != "2" || line["audio_by"] != "server" || line["audio_asked"] != "none" || !line.has("reasons", "SecondaryAudioNotSupported") {
		t.Errorf("a saved language that picks track 2: %v", line)
	}
}

// The ranges of the captured Jellyfin films (compat/captures/hdr-copy-12.4.json, dolby-vision-12.4.json): for each film and
// profile Jellyfin named reasons; every one is in the line, beside the file's range (and its Dolby Vision profile), and the
// line says tone mapped for a PQ or HLG picture that is re-encoded, but not Dolby Vision profile 5, and for nothing else.
func TestTheDecisionLineNamesTheRangeOfEachCapturedFilmAndWhatJellyfinSaid(t *testing.T) {
	for _, capture := range []struct{ file, container, audio string }{{"hdr-copy-12.4.json", "mkv", "ac3"}, {"dolby-vision-12.4.json", "mp4", "aac"}} {
		raw, err := os.ReadFile("../../compat/captures/" + capture.file)
		if err != nil {
			t.Fatal(err)
		}
		var parsed struct {
			DeviceProfiles map[string]json.RawMessage
			Files          map[string]struct {
				Video        map[string]any
				PlaybackInfo map[string]struct{ TranscodeReasons *string }
			}
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		for file, entry := range parsed.Files {
			e, _, log := decisionEnv(t, storedFilm{container: capture.container, video: entry.Video, audio: []map[string]any{{"Index": 1, "Type": "Audio", "Codec": capture.audio, "IsDefault": true, "Channels": 2}}})
			for name, want := range entry.PlaybackInfo {
				t.Run(capture.file+"/"+file+"/"+name, func(t *testing.T) {
					var p map[string]any
					if err := json.Unmarshal(parsed.DeviceProfiles[name], &p); err != nil {
						t.Fatal(err)
					}
					reply, lines, _ := e.decide(t, log, infoRequest{body: map[string]any{"DeviceProfile": p, "MaxStreamingBitrate": 1440987881, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": db.CompactID(e.item), "IsPlayback": true}})
					shape(t, e, reply, lines, e.item)
					line := lines[0]
					rangeType, _ := entry.Video["VideoRangeType"].(string)
					if line.has("reasons", "VideoRangeTypeNotSupported") && line["src_range"] != rangeType {
						t.Errorf("src_range %q, the file's VideoRangeType %q", line["src_range"], rangeType)
					}
					if profile, ok := entry.Video["DvProfile"].(float64); ok && line.has("reasons", "VideoRangeTypeNotSupported") && line["src_dv_profile"] != strconv.Itoa(int(profile)) {
						t.Errorf("src_dv_profile %q, the file's %v", line["src_dv_profile"], profile)
					}
					if want.TranscodeReasons != nil {
						for _, reason := range strings.Split(*want.TranscodeReasons, ",") {
							if !line.has("reasons", reason) {
								t.Errorf("Jellyfin named %s, the line's reasons are %q", reason, line["reasons"])
							}
						}
					}
					wantTone := "no"
					// The encoder follows the transfer, not the range label: a PQ or HLG picture is mapped unless it is Dolby Vision
					// profile 5 (VideoRangeType DOVI), so HDR10+ and the Dolby Vision films with an HDR10 or HLG base are tone mapped too.
					transfer, _ := entry.Video["ColorTransfer"].(string)
					if line["method"] == "reencode" && (transfer == "smpte2084" || transfer == "arib-std-b67") && rangeType != "DOVI" {
						wantTone = "yes"
					}
					if line["method"] != "direct_play" && line["tone_mapped"] != wantTone {
						t.Errorf("tone_mapped %q for a %s %s, want %s", line["tone_mapped"], line["method"], rangeType, wantTone)
					}
				})
			}
		}
	}
}

// The ceiling: 1080 lines is the most a re-encode makes. The line says it lowered the picture exactly when the same request
// would have got more lines without it.
func TestTheDecisionLineSaysTheCeilingLoweredThePictureExactlyWhenItDid(t *testing.T) {
	tall := func(h int) map[string]any {
		return map[string]any{"Codec": "hevc", "Profile": "Main", "Level": 153, "Width": h * 16 / 9, "Height": h}
	}
	for _, c := range []struct {
		name         string
		height, cap  int
		out, lowered string
	}{
		{"2160 at Auto", 2160, 2147483647, "1080", "yes"},
		{"1440 at Auto", 1440, 2147483647, "1080", "yes"},
		{"2160 under a cap that carries 1440", 2160, 5000000, "1080", "yes"},
		{"1080 at Auto", 1080, 2147483647, "1080", "no"},
		{"1080 under a cap that carries only 720", 1080, 1200000, "720", "no"},
		{"2160 under a cap that carries only 720", 2160, 1200000, "720", "no"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, log := decisionEnv(t, storedFilm{video: tall(c.height), bitrate: 1000})
			reply, lines, _ := e.decide(t, log, infoRequest{body: map[string]any{"DeviceProfile": chromeProfile(t, nil), "MaxStreamingBitrate": c.cap, "AudioStreamIndex": "1", "MediaSourceId": db.CompactID(e.item)}})
			shape(t, e, reply, lines, e.item)
			line := lines[0]
			if line["method"] != "reencode" || line["src_height"] != strconv.Itoa(c.height) || line["out_height"] != c.out || line["ceiling_lowered"] != c.lowered {
				t.Errorf("want a re-encode of %d lines to %s, ceiling_lowered=%s: %v", c.height, c.out, c.lowered, line)
			}
		})
	}
}

// A copy reads as the file's own video: 2160 stays 2160, and nothing lowered it.
func TestTheDecisionLineOfACopyReadsAsTheFilesOwnVideo(t *testing.T) {
	e, _, log := decisionEnv(t, storedFilm{video: map[string]any{"Width": 3840, "Height": 2160}, audio: []map[string]any{{"Index": 1, "Type": "Audio", "Codec": "ac3", "IsDefault": true, "Channels": 2}}})
	reply, lines, _ := e.decide(t, log, infoRequest{body: chrome(t)})
	shape(t, e, reply, lines, e.item)
	for key, want := range map[string]string{"method": "copy", "out_codec": "h264", "out_height": "2160", "src_height": "2160", "out_range": "SDR", "ceiling_lowered": "no", "tone_mapped": "no"} {
		if lines[0][key] != want {
			t.Errorf("%s=%q, want %q", key, lines[0][key], want)
		}
	}
}

// The ids are printed in one form whatever form the request used, and the route and the way of naming a source change
// nothing of the line.
func TestTheDecisionLineIsOneLinePerSourceOnEveryRouteWithTheIdsInOneForm(t *testing.T) {
	e, _, log := decisionEnv(t, storedFilm{})
	compact := db.CompactID(e.item)
	for _, c := range []struct {
		name string
		req  infoRequest
	}{
		{"POST dashed", infoRequest{body: chrome(t)}},
		{"POST compact", infoRequest{item: compact, body: chrome(t)}},
		{"POST upper case", infoRequest{item: strings.ToUpper(e.item), body: chrome(t)}},
		{"GET with the source in the query", infoRequest{route: "get", query: "mediasourceid=" + strings.ToUpper(compact)}},
		{"jellyscale route", infoRequest{route: "marked", body: chrome(t)}},
		{"the source named in the body, upper case", infoRequest{body: func() map[string]any { b := chrome(t); b["MediaSourceId"] = strings.ToUpper(compact); return b }()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reply, lines, status := e.decide(t, log, c.req)
			if status != 200 {
				t.Fatalf("status %d", status)
			}
			shape(t, e, reply, lines, e.item)
			if lines[0]["item"] != e.item || lines[0]["source"] != e.item {
				t.Errorf("item=%q source=%q, want the dashed lower case %s", lines[0]["item"], lines[0]["source"], e.item)
			}
		})
	}
}

// An item of two sources: a reply that lists both logs two lines, each with its own source id and decision; a request that
// names one logs one.
func TestAReplyListingTwoSourcesLogsTwoLinesEachWithItsOwnDecision(t *testing.T) {
	e, source, log := decisionEnv(t, storedFilm{})
	second := uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end)
		SELECT $1, item_id, 'second.mkv', size, 'def456', 'avi', streams, keyframes, video_end FROM media_sources WHERE id = $2`, second, e.item)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources}', (dto->'MediaSources') || jsonb_build_array((dto->'MediaSources'->0) || jsonb_build_object('Id', $1::text)))`, db.CompactID(second))
	reply, lines, _ := e.decide(t, log, infoRequest{body: map[string]any{"DeviceProfile": chromeProfile(t, nil), "AudioStreamIndex": "1"}})
	if len(reply["MediaSources"].([]any)) != 2 {
		t.Fatalf("the reply lists %d sources, want 2: %v", len(reply["MediaSources"].([]any)), reply)
	}
	shape(t, e, reply, lines, e.item)
	byID := map[string]decisionLine{}
	for _, l := range lines {
		byID[l["source"]] = l
	}
	if byID[e.item]["method"] != "direct_play" || byID[second]["method"] != "copy" || !byID[second].has("reasons", "ContainerNotSupported") {
		t.Errorf("each source has its own decision (a direct play, and a copy for the container): %v", lines)
	}
	reply, lines, _ = e.decide(t, log, infoRequest{body: map[string]any{"DeviceProfile": chromeProfile(t, nil), "MediaSourceId": source}})
	shape(t, e, reply, lines, e.item)
	if len(lines) != 1 || lines[0]["source"] != e.item {
		t.Errorf("a request naming one source: %v", lines)
	}
}

// A request that gets no reply logs no decision line. The positive control is the decision line of a good request just before.
func TestARequestThatGetsNoReplyLogsNoDecisionLineAndMovesNoCounter(t *testing.T) {
	e, _, log := decisionEnv(t, storedFilm{})
	reply, lines, _ := e.decide(t, log, infoRequest{body: chrome(t)})
	shape(t, e, reply, lines, e.item)
	counters := series(t)
	check := func(what string, status, want int) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: status %d, want %d", what, status, want)
		}
		if n := len(decisionsOf(log.String())); n != 1 {
			t.Errorf("%s: %d decision lines in the log, want only the control's", what, n)
		}
	}
	status, _ := e.rawPost(t, "{not json")
	check("a body that is not JSON", status, 400)
	_, _, status = e.decide(t, log, infoRequest{token: "nope", body: chrome(t)})
	check("an unknown token", status, 401)
	_, _, status = e.decide(t, log, infoRequest{item: uuid.NewString(), body: chrome(t)})
	check("an unknown item", status, 404)
	_, _, status = e.decide(t, log, infoRequest{item: "not-an-id", body: chrome(t)})
	check("an id that is no id", status, 404)
	// A replica whose database is gone answers no PlaybackInfo (a failure of the store, not a decision).
	e.apis[1].Pool.Close()
	_, _, status = e.decide(t, log, infoRequest{replica: 1, body: chrome(t)})
	if status == 200 {
		t.Fatal("a replica with its pool closed answered 200")
	}
	if n := len(decisionsOf(log.String())); n != 1 {
		t.Errorf("a database failure: %d decision lines, want only the control's", n)
	}
	if moved := delta(counters, series(t)); len(moved) != 0 {
		t.Errorf("requests with no reply moved %v", moved)
	}
}

// rawPost posts a body as it is, as the viewer.
func (e *env) rawPost(t *testing.T, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest("POST", e.servers[0].URL+"/Items/"+e.item+"/PlaybackInfo", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", fmt.Sprintf(probeAuthority, e.token))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

// Every name of the TranscodeReasons that GET /Sessions lists for a playing source is in that source's line.
func TestTheNamesGetSessionsListsAreAllInTheSourcesLine(t *testing.T) {
	for _, c := range decisionCases {
		if c.method != "copy" && c.method != "reencode" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			e, _, log := decisionEnv(t, c.film)
			body := c.body(t)
			body["MediaSourceId"] = db.CompactID(e.item)
			reply, lines, _ := e.decide(t, log, infoRequest{body: body})
			if len(lines) != 1 {
				t.Fatalf("%d lines, want 1: %v", len(lines), lines)
			}
			e.play(t, reply, "Transcode")
			info := e.transcodingInfo(t, 0)
			names, _ := info["TranscodeReasons"].([]any)
			for _, name := range names {
				if !lines[0].has("reasons", name.(string)) {
					t.Errorf("GET /Sessions lists %v, the line's reasons are %q", name, lines[0]["reasons"])
				}
			}
		})
	}
}
