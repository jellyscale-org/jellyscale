//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"jellyscale/internal/db"
)

// profileWith is the captured profile with these subtitle profiles: Chrome's own
// (vtt, ass, ssa) and, when pgs is set, the two jellyfin-web 12.4 adds once its
// viewer turns PGS rendering on.
func profileWith(t *testing.T, pgs bool) map[string]any {
	t.Helper()
	profile := map[string]any{}
	if err := json.Unmarshal(capturedProfile(t), &profile); err != nil {
		t.Fatal(err)
	}
	if pgs {
		profile["SubtitleProfiles"] = append(profile["SubtitleProfiles"].([]any),
			map[string]any{"Format": "pgssub", "Method": "External"}, map[string]any{"Format": "vobsub", "Container": "mks", "Method": "External"})
	}
	return profile
}

func (f *subtitleFixture) imageInfo(t *testing.T, profile map[string]any, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": db.CompactID(f.image)}
	for k, v := range extra {
		body[k] = v
	}
	return firstSource(f.playbackInfo(t, 0, f.image, body))
}

func (f *subtitleFixture) imageTracks(t *testing.T, replica int) map[int]map[string]any {
	t.Helper()
	response, raw := f.do(t, replica, "GET", "/Items/"+f.image, f.token, nil)
	var item struct{ MediaStreams []any }
	if err := json.Unmarshal(raw, &item); err != nil || response.StatusCode != 200 {
		t.Fatalf("item: %d %s", response.StatusCode, raw)
	}
	return subtitleTracks(map[string]any{"MediaStreams": item.MediaStreams})
}

// PGS, VobSub and DVB tracks are listed with Jellyfin's fields, and delivered as
// Jellyfin 12.4 does: PGS as the file when the client says it draws PGS, and
// every other track, and PGS for a client that doesn't, as part of a transcode.
func TestImageSubtitlesAreListedAndDelivered(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *subtitleFixture) {
		f.enableTranscoding(t)
		tracks := f.imageTracks(t, 0)
		for index, want := range map[int]map[string]any{
			2: {"Codec": "PGSSUB", "DisplayTitle": "eng - Default - PGSSUB", "Language": "eng", "Width": float64(1920), "Height": float64(1080), "SupportsExternalStream": true},
			3: {"Codec": "DVDSUB", "DisplayTitle": "DVDSUB", "SupportsExternalStream": false},
			4: {"Codec": "DVBSUB", "DisplayTitle": "DVBSUB", "SupportsExternalStream": false},
		} {
			// ffmpeg flags the first track of a Matroska file as the default.
			want["IsTextSubtitleStream"], want["IsExternal"], want["IsDefault"], want["IsForced"] = false, false, index == 2, false
			for key, value := range want {
				if tracks[index][key] != value {
					t.Errorf("item track %d: %s is %v, want %v (%v)", index, key, tracks[index][key], value, tracks[index])
				}
			}
			if tracks[index]["DeliveryUrl"] != nil {
				t.Errorf("item track %d has a DeliveryUrl", index)
			}
		}
		if len(tracks) != 3 {
			t.Errorf("tracks listed: %v", tracks)
		}

		// A client that draws no image tracks: all three are drawn into a transcode,
		// and with none chosen the film plays as it is.
		plain := profileWith(t, false)
		direct := f.imageInfo(t, plain, nil)
		if direct["TranscodingUrl"] != nil || direct["SupportsDirectPlay"] != true {
			t.Fatalf("expected direct play: %v", direct)
		}
		for index, track := range subtitleTracks(direct) {
			if track["DeliveryMethod"] != "Encode" || track["DeliveryUrl"] != nil {
				t.Errorf("a client that draws none: track %d: %v", index, track)
			}
		}
		// Picking one is a transcode that draws it, for any of them.
		for _, index := range []int{2, 3, 4} {
			burnt := f.imageInfo(t, plain, map[string]any{"SubtitleStreamIndex": index})
			query := mustQuery(t, fmt.Sprint(burnt["TranscodingUrl"]))
			if burnt["TranscodingUrl"] == nil || burnt["SupportsDirectPlay"] != false || query.Get("SubtitleStreamIndex") != fmt.Sprint(index) ||
				!strings.Contains(query.Get("TranscodeReasons"), "SubtitleCodecNotSupported") {
				t.Errorf("track %d chosen: %v", index, burnt)
			}
		}

		// A client that draws PGS gets the file, and chooses it without a transcode.
		renders := profileWith(t, true)
		info := f.imageInfo(t, renders, map[string]any{"SubtitleStreamIndex": 2})
		pgs := subtitleTracks(info)[2]
		want := "/Videos/" + db.DashedID(f.image) + "/" + db.CompactID(f.image) + "/Subtitles/2/0/Stream.pgssub?ApiKey=" + f.token
		if info["TranscodingUrl"] != nil || info["SupportsDirectPlay"] != true || pgs["DeliveryMethod"] != "External" || pgs["DeliveryUrl"] != want || pgs["IsExternalUrl"] != false {
			t.Errorf("a client that draws PGS: %v", info)
		}
		if info["DefaultSubtitleStreamIndex"] != float64(2) {
			t.Errorf("the chosen track isn't echoed: %v", info["DefaultSubtitleStreamIndex"])
		}
		// VobSub is only ever drawn in, whatever the profile says it can do.
		for _, index := range []int{3, 4} {
			if track := subtitleTracks(info)[index]; track["DeliveryMethod"] != "Encode" || track["DeliveryUrl"] != nil {
				t.Errorf("track %d for a client that draws PGS: %v", index, track)
			}
		}
		vobsub := f.imageInfo(t, renders, map[string]any{"SubtitleStreamIndex": 3})
		if vobsub["TranscodingUrl"] == nil || mustQuery(t, vobsub["TranscodingUrl"].(string)).Get("SubtitleStreamIndex") != "3" {
			t.Errorf("VobSub for a client that draws PGS: %v", vobsub)
		}
		// A request that sends no profile is only ever answered with the file as it
		// is, so there is nothing that could draw a track in.
		bare := firstSource(f.playbackInfo(t, 0, f.image, map[string]any{"MediaSourceId": db.CompactID(f.image)}))
		if listed := subtitleTracks(bare); len(listed) != 0 || bare["TranscodingUrl"] != nil {
			t.Errorf("a request with no profile: %v", listed)
		}
		// The viewer's setting draws PGS in when the film is transcoded anyway, and not otherwise.
		always := map[string]any{"AlwaysBurnInSubtitleWhenTranscoding": true, "SubtitleStreamIndex": 2}
		if source := f.imageInfo(t, renders, always); source["TranscodingUrl"] != nil || subtitleTracks(source)[2]["DeliveryMethod"] != "External" {
			t.Errorf("always burn, direct play: %v", source)
		}
		always["MaxStreamingBitrate"] = 1000
		source := f.imageInfo(t, renders, always)
		if source["TranscodingUrl"] == nil || mustQuery(t, source["TranscodingUrl"].(string)).Get("SubtitleStreamIndex") != "2" || subtitleTracks(source)[2]["DeliveryMethod"] != "Encode" {
			t.Errorf("always burn, transcode: %v", source)
		}
		// Without transcoding nothing can be drawn, so only what is fetched is listed.
		for _, s := range f.apis {
			s.Transcode = nil
		}
		if listed := subtitleTracks(f.imageInfo(t, renders, nil)); len(listed) != 1 || listed[2]["DeliveryMethod"] != "External" {
			t.Errorf("with no transcoding: %v", listed)
		}
		if listed := subtitleTracks(f.imageInfo(t, plain, nil)); len(listed) != 0 {
			t.Errorf("tracks that only a transcode could draw: %v", listed)
		}
	})
}

// The PGS track is copied out of the film once, by a worker, and sent as the file
// it is, with the ranges jellyfin-web asks for; nothing else reads it as the
// wrong format.
func TestPgsTrackIsServedAsAFile(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *subtitleFixture) {
		path := func(index int, file string) string { return f.path(f.image, index, 0, file) }
		before := f.runs()
		var whole []byte
		for _, name := range []string{"Stream.pgssub", "Stream.sup"} {
			response, body := f.do(t, 1, "GET", path(2, name)+"?ApiKey="+f.token, "", nil)
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("Accept-Ranges") != "bytes" ||
				len(body) < 100 || string(body[:2]) != "PG" {
				t.Fatalf("%s: %d %q, %d bytes", name, response.StatusCode, response.Header.Get("Content-Type"), len(body))
			}
			if whole != nil && string(whole) != string(body) {
				t.Errorf("%s is not the same file", name)
			}
			whole = body
		}
		// What jellyfin-web asks first, to learn whether ranges work, and then a part.
		response, body := f.do(t, 0, "GET", path(2, "Stream.pgssub")+"?ApiKey="+f.token, "", nil, "Range", "bytes=0-0")
		if response.StatusCode != 206 || response.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-0/%d", len(whole)) || string(body) != "P" {
			t.Errorf("a probe: %d %q %q", response.StatusCode, response.Header.Get("Content-Range"), body)
		}
		response, body = f.do(t, 0, "GET", path(2, "Stream.pgssub")+"?ApiKey="+f.token, "", nil, "Range", "bytes=20-")
		if response.StatusCode != 206 || string(body) != string(whole[20:]) {
			t.Errorf("the rest: %d, %d bytes", response.StatusCode, len(body))
		}
		// One ffmpeg for all of that: one worker job, whoever asked.
		if n := f.runs() - before; n != 1 {
			t.Errorf("%d ffmpeg runs, want 1", n)
		}
		if n := scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE kind = 'sup' AND state = 'done'"); n != 1 {
			t.Errorf("%d sup jobs", n)
		}

		// A track in another format is a bad request, as in Jellyfin 12.4; one that
		// isn't there is not found.
		for name, c := range map[string]struct {
			path string
			want int
		}{
			"PGS as WebVTT":        {path(2, "Stream.vtt"), 400},
			"PGS as ASS":           {path(2, "Stream.ass"), 400},
			"VobSub as a file":     {path(3, "Stream.sup"), 400},
			"VobSub as WebVTT":     {path(3, "Stream.vtt"), 400},
			"DVB as a file":        {path(4, "Stream.pgssub"), 400},
			"the audio as a file":  {path(1, "Stream.sup"), 404},
			"past the end":         {path(9, "Stream.sup"), 404},
			"a text track as .sup": {f.path(f.text, 2, 0, "Stream.sup"), 400},
		} {
			if status, _, _ := f.get(t, 0, c.path); status != c.want {
				t.Errorf("%s: %d, want %d", name, status, c.want)
			}
		}
		playlist := fmt.Sprintf("/Videos/%s/%s/Subtitles/2/subtitles.m3u8?segmentLength=3&ApiKey=%s", db.DashedID(f.image), db.CompactID(f.image), f.token)
		if response, _ := f.do(t, 0, "GET", playlist, "", nil); response.StatusCode != 404 {
			t.Errorf("a playlist of image cues: %d, want 404", response.StatusCode)
		}
		if n := scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs"); n != 1 {
			t.Errorf("%d subtitle jobs after the bad requests, want only the one", n)
		}
	})
}

// With no worker, only what is already stored can be listed: the PGS file once
// copied, and never a track that only a transcode draws.
func TestImageSubtitlesWithNoWorker(t *testing.T) {
	f := subtitleEnv(t, "")
	f.stopWorker()
	mustExec(t, f.pool, "TRUNCATE transcode_nodes")
	if tracks := f.imageTracks(t, 0); len(tracks) != 0 {
		t.Errorf("tracks listed with no worker and nothing stored: %v", tracks)
	}
	f.startWorker(t, 0)
	if status, _, _ := f.get(t, 0, f.path(f.image, 2, 0, "Stream.pgssub")); status != 200 {
		t.Fatalf("the PGS track: %d", status)
	}
	f.stopWorker()
	mustExec(t, f.pool, "TRUNCATE transcode_nodes")
	if tracks := f.imageTracks(t, 0); len(tracks) != 1 || tracks[2]["Codec"] != "PGSSUB" {
		t.Errorf("tracks listed with no worker and the PGS file stored: %v", tracks)
	}
	// Served from the store with no worker behind it.
	if status, _, body := f.get(t, 1, f.path(f.image, 2, 0, "Stream.pgssub")); status != 200 || len(body) < 100 {
		t.Errorf("the stored PGS file: %d, %d bytes", status, len(body))
	}
}
