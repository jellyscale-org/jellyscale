//go:build integration

package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// Image subtitle files beside a film: PGS as .sup, and VobSub as a .sub and
// .idx pair, which are one track. What a real Jellyfin 12.4 lists and delivers
// for them was observed in compat/captures/external-image-subtitles-12.4.md.

// imageFiles are the files put beside film.mkv, which has its own SubRip track
// as stream 2. Each .sup has a cue of its own, so a track served under the wrong
// index is told apart by its bytes.
type imageFiles struct {
	sups map[string][]byte // By file name.
	vob  map[string][]byte
}

func pgsFile(y int) []byte {
	return testenv.PGS(1920, 1080, testenv.Cue{Start: 0.5, End: 2.5, X: 660, Y: y, W: 600, H: 120})
}

// imageFilesEnv scans a film with these files beside it (jellyfin lists the
// .idx of a pair, and a lone .sub or .idx isn't a track):
//
//	film.de.default.sup, film.en.srt, film.en.sup, film.es.sub + film.es.idx,
//	film.fr.forced.sup, film.it.sub (alone), film.ja.forced.sub + film.ja.forced.idx,
//	film.ko.idx (alone), film.qq.sub + film.qq.idx (the .idx is not an index file), film.sup
func imageFilesEnv(t *testing.T, target string) (*externalFixture, imageFiles) {
	t.Helper()
	testenv.FFmpeg(t)
	f := &externalFixture{subtitleFixture: &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}, scanLog: &syncBuffer{}}
	t.Cleanup(func() {
		if t.Failed() {
			fmt.Printf("scan and worker log:\n%s%s", f.scanLog, f.log)
		}
	})
	f.tools = traceTools(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte(testSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	filmPath := filepath.Join(dir, "film.mkv")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-i", srt, "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "flac", "-c:s", "srt",
		"-metadata:s:a:0", "language=eng", "-metadata:s:s:0", "language=eng", filmPath)
	film, err := os.ReadFile(filmPath)
	if err != nil {
		t.Fatal(err)
	}
	f.assetDir = t.TempDir()
	media, assets := "file://"+t.TempDir(), "file://"+f.assetDir
	if target != "" {
		media, assets = s3Root(t, target), s3Root(t, target)
	}
	if f.media, err = store.Open(media); err != nil {
		t.Fatal(err)
	}
	if f.assets, err = store.Open(assets); err != nil {
		t.Fatal(err)
	}
	files := imageFiles{
		sups: map[string][]byte{"film.de.default.sup": pgsFile(100), "film.en.sup": pgsFile(400), "film.fr.forced.sup": pgsFile(800), "film.sup": pgsFile(500)},
		vob:  map[string][]byte{},
	}
	esIdx, esSub := testenv.VobSub(720, 480, testenv.Cue{Start: 0.5, End: 2.5, X: 247, Y: 44, W: 225, H: 53})
	jaIdx, jaSub := testenv.VobSub(720, 480, testenv.Cue{Start: 0.5, End: 2.5, X: 247, Y: 382, W: 225, H: 53})
	files.vob["film.es.idx"], files.vob["film.es.sub"], files.vob["film.ja.forced.idx"], files.vob["film.ja.forced.sub"] = esIdx, esSub, jaIdx, jaSub
	f.put(t, "film.mkv", string(film))
	f.put(t, "film.en.srt", srtCue("ext en"))
	for name, data := range files.sups {
		f.put(t, name, string(data))
	}
	for name, data := range files.vob {
		f.put(t, name, string(data))
	}
	f.put(t, "film.it.sub", string(esSub))
	f.put(t, "film.ko.idx", string(esIdx))
	f.put(t, "film.qq.sub", string(esSub))
	f.put(t, "film.qq.idx", "not an index file")
	f.put(t, "other.en.sup", string(pgsFile(300)))
	libs, err := library.Sync(context.Background(), f.pool, []library.Config{{Name: "Ext", Kind: "movies", Media: media, Assets: assets}})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = libs[0]
	f.scanner = &library.Scanner{Pool: f.pool, Log: slog.New(slog.NewTextHandler(f.scanLog, nil))}
	f.scan(t)
	f.text = scalar[string](t, f.pool, "SELECT id::text FROM items WHERE library_id = $1 AND type = 'Movie'", f.lib.ID)
	for _, s := range f.apis {
		s.Log = slog.New(slog.NewTextHandler(f.log, nil))
	}
	f.startWorker(t, 0)
	return f, files
}

// What each track is, from the capture: Jellyfin lists the .idx of a pair, takes
// the language of a pair from the "id:" line inside the .idx (the testenv one
// says "en", which is why film.es.idx is English, and film.ja.forced.idx too)
// and the flags from the file name, and gives the size from the file ("size:"
// of the .idx, the canvas of the .sup).
var imageTracks = map[int]map[string]any{
	3: {"Codec": "PGSSUB", "Language": "deu", "DisplayTitle": "German - Default - PGSSUB - External", "IsDefault": true, "IsForced": false, "Width": float64(1920), "Height": float64(1080)},
	4: {"Codec": "subrip", "Language": "eng", "DisplayTitle": "English - SUBRIP - External", "IsDefault": false, "IsForced": false},
	5: {"Codec": "PGSSUB", "Language": "eng", "DisplayTitle": "English - PGSSUB - External", "IsDefault": false, "IsForced": false, "Width": float64(1920), "Height": float64(1080)},
	6: {"Codec": "DVDSUB", "Language": "en", "DisplayTitle": "English - DVDSUB - External", "IsDefault": false, "IsForced": false, "Width": float64(720), "Height": float64(480)},
	7: {"Codec": "PGSSUB", "Language": "fra", "DisplayTitle": "French - Forced - PGSSUB - External", "IsDefault": false, "IsForced": true, "Width": float64(1920), "Height": float64(1080)},
	8: {"Codec": "DVDSUB", "Language": "en", "DisplayTitle": "English - Forced - DVDSUB - External", "IsDefault": false, "IsForced": true, "Width": float64(720), "Height": float64(480)},
	9: {"Codec": "PGSSUB", "DisplayTitle": "Undefined - PGSSUB - External", "IsDefault": false, "IsForced": false, "Width": float64(1920), "Height": float64(1080)},
}

func (f *externalFixture) tracksByIndex(t *testing.T) map[int]map[string]any {
	t.Helper()
	byIndex := map[int]map[string]any{}
	for _, track := range f.subtitles(t) {
		byIndex[int(track["Index"].(float64))] = track
	}
	return byIndex
}

// The scan lists the files as Jellyfin 12.4 lists them: after nothing of the
// film's own tracks (jellyscale numbers the film's streams first, a recorded
// difference), in file name order with a pair at the place of its .idx, with
// the codec, language and flags above, IsExternal, and not a lone .sub or .idx,
// a pair whose .idx is no index, or another film's file. It queues no job.
func TestScanListsExternalImageSubtitleFiles(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f, _ := imageFilesEnv(t, target)
			tracks := f.tracksByIndex(t)
			if len(tracks) != 8 {
				t.Fatalf("%d subtitle tracks, want the film's own and 7 files: %s", len(tracks), summary(f.subtitles(t)))
			}
			if own := tracks[2]; own["IsExternal"] == true || own["Codec"] != "subrip" {
				t.Errorf("the film's own track is not stream 2: %v", own)
			}
			for index, want := range imageTracks {
				got := tracks[index]
				if got == nil {
					t.Errorf("track %d is not listed", index)
					continue
				}
				for key, value := range want {
					if got[key] != value {
						t.Errorf("track %d %s: %v, want %v (%v)", index, key, got[key], value, got)
					}
				}
				if _, has := want["Language"]; !has && got["Language"] != nil {
					t.Errorf("track %d has the language %v, which nothing names", index, got["Language"])
				}
				if got["IsExternal"] != true || got["Type"] != "Subtitle" {
					t.Errorf("track %d is not an external subtitle: %v", index, got)
				}
				if index != 4 && (got["IsTextSubtitleStream"] != false || got["DeliveryUrl"] != nil) {
					t.Errorf("track %d is listed as text or with a URL: %v", index, got)
				}
				if codec := got["Codec"]; codec == "PGSSUB" && got["SupportsExternalStream"] != true {
					t.Errorf("track %d: a .sup is a file a client can draw: %v", index, got)
				}
			}
			// Which file each is, by name: a .idx for the pair, never the .sub.
			paths := map[int]string{3: "film.de.default.sup", 5: "film.en.sup", 6: "film.es.", 7: "film.fr.forced.sup", 8: "film.ja.forced.", 9: "film.sup"}
			for index, name := range paths {
				if path, _ := tracks[index]["Path"].(string); !strings.Contains(path, name) {
					t.Errorf("track %d is %q, want a path with %s", index, path, name)
				}
			}
			if f.jobs(t, "track")+f.jobs(t, "fonts") != 0 {
				t.Error("the scan queued a job")
			}
			if strings.Contains(f.scanLog.String(), "level=ERROR") {
				t.Errorf("the scan logged an error:\n%s", f.scanLog)
			}
		})
	}
}

func (f *externalFixture) filesInfo(t *testing.T, profile map[string]any, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": db.CompactID(f.text)}
	for k, v := range extra {
		body[k] = v
	}
	return firstSource(f.playbackInfo(t, 0, f.text, body))
}

// Delivery is as Jellyfin 12.4 does it for files (the capture): with the
// default jellyfin-web profile every image track is drawn into a transcode that
// the pick starts; with PGS rendering on a .sup is the file, fetched and drawn by
// the client with no transcode, and a VobSub file is still drawn into a
// transcode, as Jellyfin does for files (it sends a VobSub file for no client).
func TestExternalImageSubtitlesAreDeliveredAsJellyfinDoes(t *testing.T) {
	f, _ := imageFilesEnv(t, "")
	f.enableTranscoding(t)
	image := []int{3, 5, 6, 7, 8, 9}
	files := []int{3, 5, 7, 9}
	vobsub := []int{6, 8}

	plain := profileWith(t, false)
	direct := f.filesInfo(t, plain, nil)
	if direct["TranscodingUrl"] != nil || direct["SupportsDirectPlay"] != true {
		t.Fatalf("a viewer who picks nothing plays the film as it is: %v", direct)
	}
	for _, index := range image {
		if track := subtitleTracks(direct)[index]; track["DeliveryMethod"] != "Encode" || track["DeliveryUrl"] != nil {
			t.Errorf("track %d for a client that draws no image: %v", index, track)
		}
	}
	if track := subtitleTracks(direct)[4]; track["DeliveryMethod"] != "External" {
		t.Errorf("the SubRip file: %v", track)
	}
	for _, index := range image {
		burnt := f.filesInfo(t, plain, map[string]any{"SubtitleStreamIndex": index})
		var query map[string][]string
		if burnt["TranscodingUrl"] != nil {
			query = mustQuery(t, fmt.Sprint(burnt["TranscodingUrl"]))
		}
		if burnt["TranscodingUrl"] == nil || burnt["SupportsDirectPlay"] != false || query["SubtitleStreamIndex"][0] != fmt.Sprint(index) ||
			!strings.Contains(strings.Join(query["TranscodeReasons"], ","), "SubtitleCodecNotSupported") {
			t.Errorf("track %d chosen by a client that draws none: %v", index, burnt)
		}
	}

	renders := profileWith(t, true)
	for _, index := range files {
		info := f.filesInfo(t, renders, map[string]any{"SubtitleStreamIndex": index})
		track := subtitleTracks(info)[index]
		want := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Subtitles/" + fmt.Sprint(index) + "/0/Stream.pgssub?ApiKey=" + f.token
		if info["TranscodingUrl"] != nil || info["SupportsDirectPlay"] != true || track["DeliveryMethod"] != "External" || track["DeliveryUrl"] != want || track["IsExternalUrl"] != false {
			t.Errorf("a client that draws PGS, track %d chosen: %v", index, info)
		}
	}
	for _, index := range vobsub {
		info := f.filesInfo(t, renders, map[string]any{"SubtitleStreamIndex": index})
		track := subtitleTracks(info)[index]
		if track["DeliveryMethod"] != "Encode" || track["DeliveryUrl"] != nil || info["TranscodingUrl"] == nil ||
			mustQuery(t, fmt.Sprint(info["TranscodingUrl"])).Get("SubtitleStreamIndex") != fmt.Sprint(index) {
			t.Errorf("a VobSub file for a client that draws PGS and VobSub, track %d: %v", index, info)
		}
	}
}

// A .sup is sent as the bytes of the file, whichever file it is, from any
// replica, with the ranges jellyfin-web asks for, under both names Jellyfin
// answers to. Other formats of a track are a bad request, as in Jellyfin 12.4,
// and a VobSub pair is sent in no format (Jellyfin answers 400 to .sub, .vobsub
// and .mks for a file; it answers 200 to .idx and an empty cue list to .vtt,
// which is not copied).
func TestExternalSupIsServedAsTheFile(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f, files := imageFilesEnv(t, target)
			for index, name := range map[int]string{3: "film.de.default.sup", 5: "film.en.sup", 7: "film.fr.forced.sup", 9: "film.sup"} {
				want := files.sups[name]
				for replica, file := range []string{"Stream.pgssub", "Stream.sup"} {
					response, body := f.do(t, replica, "GET", f.path(f.text, index, 0, file)+"?ApiKey="+f.token, "", nil)
					if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("Accept-Ranges") != "bytes" || !bytes.Equal(body, want) {
						t.Errorf("%s as %s from replica %d: %d %q, %d bytes, want the file's %d", name, file, replica, response.StatusCode, response.Header.Get("Content-Type"), len(body), len(want))
					}
				}
				response, body := f.do(t, 0, "GET", f.path(f.text, index, 0, "Stream.pgssub")+"?ApiKey="+f.token, "", nil, "Range", "bytes=0-0")
				if response.StatusCode != 206 || response.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-0/%d", len(want)) || string(body) != "P" {
					t.Errorf("%s probe: %d %q %q", name, response.StatusCode, response.Header.Get("Content-Range"), body)
				}
				if response, body = f.do(t, 1, "GET", f.path(f.text, index, 0, "Stream.pgssub")+"?ApiKey="+f.token, "", nil, "Range", "bytes=20-"); response.StatusCode != 206 || !bytes.Equal(body, want[20:]) {
					t.Errorf("%s rest: %d, %d bytes", name, response.StatusCode, len(body))
				}
			}
			for name, c := range map[string]struct {
				path string
				want int
			}{
				"a .sup as WebVTT":          {f.path(f.text, 5, 0, "Stream.vtt"), 400},
				"a .sup as ASS":             {f.path(f.text, 5, 0, "Stream.ass"), 400},
				"a VobSub pair as .vobsub":  {f.path(f.text, 6, 0, "Stream.vobsub"), 400},
				"a VobSub pair as .mks":     {f.path(f.text, 6, 0, "Stream.mks"), 400},
				"a VobSub pair as .sub":     {f.path(f.text, 6, 0, "Stream.sub"), 400},
				"a VobSub pair as .sup":     {f.path(f.text, 8, 0, "Stream.sup"), 400},
				"a VobSub pair as WebVTT":   {f.path(f.text, 8, 0, "Stream.vtt"), 400},
				"a SubRip file as .pgssub":  {f.path(f.text, 4, 0, "Stream.pgssub"), 400},
				"past the end":              {f.path(f.text, 10, 0, "Stream.pgssub"), 404},
				"a playlist of a .sup":      {fmt.Sprintf("/Videos/%s/%s/Subtitles/5/subtitles.m3u8?segmentLength=3", db.DashedID(f.text), db.CompactID(f.text)), 404},
				"the film's own text track": {f.path(f.text, 2, 0, "Stream.vtt"), 200},
			} {
				// The playlist path has a query already, so the key joins it with &.
				url := c.path + "?ApiKey=" + f.token
				if strings.Contains(c.path, "?") {
					url = c.path + "&ApiKey=" + f.token
				}
				if response, _ := f.do(t, 0, "GET", url, "", nil); response.StatusCode != c.want {
					t.Errorf("%s: %d, want %d", name, response.StatusCode, c.want)
				}
			}
		})
	}
}

// A scan after a file changes notices it, without reading the film: a .sub
// that has no .idx yet is not a track and the scan goes on; its .idx makes it
// one, at the next index, and the tracks listed before keep theirs; removing
// either half takes the track away; a .sup that is replaced is served as the new
// file at the same index.
func TestRescanNoticesImageSubtitleFiles(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f, files := imageFilesEnv(t, target)
			rescan := func(why string) {
				t.Helper()
				before := len(f.tools())
				f.scan(t)
				for _, run := range f.tools()[before:] {
					if strings.Contains(run, "film.mkv") {
						t.Errorf("%s: the film was read again: %s", why, run)
					}
				}
			}
			count := func() int { return len(f.tracksByIndex(t)) }
			idx, sub := testenv.VobSub(720, 480, testenv.Cue{Start: 1, End: 2, X: 247, Y: 213, W: 225, H: 53})

			f.put(t, "film.fr.sub", string(sub))
			rescan("a .sub alone")
			if count() != 8 {
				t.Errorf("a .sub with no .idx was listed: %s", summary(f.subtitles(t)))
			}
			f.put(t, "film.fr.idx", string(idx))
			rescan("its .idx")
			tracks := f.tracksByIndex(t)
			if len(tracks) != 9 || tracks[10]["Codec"] != "DVDSUB" || tracks[10]["IsExternal"] != true || !strings.Contains(fmt.Sprint(tracks[10]["Path"]), "film.fr.") {
				t.Fatalf("after the .idx came: %s", summary(f.subtitles(t)))
			}
			if status, _, body := f.get(t, 0, f.path(f.text, 5, 0, "Stream.pgssub")); status != 200 || body != string(files.sups["film.en.sup"]) {
				t.Errorf("a track listed before kept its index: %d", status)
			}

			if err := f.media.Delete(context.Background(), "film.fr.sub"); err != nil {
				t.Fatal(err)
			}
			rescan("the .sub removed")
			if count() != 8 || f.tracksByIndex(t)[10] != nil {
				t.Errorf("a pair with no .sub was listed: %s", summary(f.subtitles(t)))
			}

			replaced := pgsFile(900)
			f.put(t, "film.en.sup", string(replaced))
			rescan("a .sup replaced")
			if status, _, body := f.get(t, 1, f.path(f.text, 5, 0, "Stream.pgssub")); status != 200 || body != string(replaced) {
				t.Errorf("the replaced .sup: %d, %d bytes, want the new file's %d", status, len(body), len(replaced))
			}
			if err := f.media.Delete(context.Background(), "film.en.sup"); err != nil {
				t.Fatal(err)
			}
			rescan("a .sup removed")
			if f.tracksByIndex(t)[5] != nil {
				t.Errorf("a removed .sup is still listed: %s", summary(f.subtitles(t)))
			}
			if status, _, _ := f.get(t, 0, f.path(f.text, 5, 0, "Stream.pgssub")); status != 404 {
				t.Errorf("the removed .sup's index answers %d, want 404", status)
			}
		})
	}
}
