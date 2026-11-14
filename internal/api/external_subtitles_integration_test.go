//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// Subtitle files beside a film, and the fonts attached to it: the scan only
// lists them, a worker converts a file or takes out the fonts when somebody
// asks, and api and stream never run a media tool. They need ffmpeg and a
// disposable database; test/integration.sh provides both.

const fontBytes = "not a real font, but the bytes must come back exactly"

const russian = "\u041f\u0440\u0438\u0432\u0435\u0442, \u043a\u0430\u043a \u0434\u0435\u043b\u0430? \u042f \u0434\u0430\u0432\u043d\u043e \u0442\u0435\u0431\u044f \u043d\u0435 \u0432\u0438\u0434\u0435\u043b."
const japanese_ = "\u3053\u3093\u306b\u3061\u306f\u3001\u304a\u5143\u6c17\u3067\u3059\u304b\uff1f\u305a\u3063\u3068\u63a2\u3057\u3066\u3044\u307e\u3057\u305f\u3002"

// externalFixture is a library whose one film has an embedded SRT and ASS
// track, a font and subtitle files beside it, with a worker running.
type externalFixture struct {
	*subtitleFixture
	media, assets store.Store
	assetDir      string // Where assets is, for a test that breaks an object.
	lib           library.Library
	scanner       *library.Scanner
	scanLog       *syncBuffer
	tools         func() []string // The media tools started since the fixture began.
}

func srtCue(text string) string { return "1\n00:00:02,000 --> 00:00:08,000\n" + text + "\n\n" }

func encoded(t *testing.T, enc interface{ NewEncoder() *encoding.Encoder }, text string) string {
	t.Helper()
	out, err := enc.NewEncoder().String(text)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func externalEnv(t *testing.T, target string) *externalFixture {
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
	srt, ass, font := filepath.Join(dir, "a.srt"), filepath.Join(dir, "b.ass"), filepath.Join(dir, "Serif.ttf")
	for path, text := range map[string]string{srt: testSRT, ass: testASS, font: fontBytes} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	film := filepath.Join(dir, "film.mkv")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-i", srt, "-i", ass, "-map", "0", "-map", "1", "-map", "2", "-map", "3", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "flac",
		"-c:s:0", "srt", "-c:s:1", "ass", "-metadata:s:a:0", "language=eng", "-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=jpn",
		"-attach", font, "-metadata:s:t:0", "mimetype=font/ttf", film)
	data, err := os.ReadFile(film)
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
	f.put(t, "film.mkv", string(data))
	f.put(t, "film.en.srt", srtCue("ext en"))
	f.put(t, "film.es.forced.srt", srtCue("ext es forced"))
	f.put(t, "film.it.vtt", "WEBVTT\n\n00:00:02.000 --> 00:00:08.000\nvtt cue\n")
	f.put(t, "film.ja.ass", testASS)
	f.put(t, "film.ja.srt", encoded(t, japanese.ShiftJIS, srtCue(japanese_)))
	f.put(t, "film.nl.srt", srtCue("caf\xe9 latin1")) // Windows-1252, not UTF-8.
	f.put(t, "film.ru.srt", encoded(t, charmap.Windows1251, srtCue(russian)))
	f.put(t, "other.en.srt", srtCue("another film's"))
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
	return f
}

func (f *externalFixture) put(t *testing.T, key, body string) {
	t.Helper()
	if err := f.media.Put(context.Background(), key, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func (f *externalFixture) scan(t *testing.T) {
	t.Helper()
	if err := f.scanner.Scan(context.Background(), f.lib); err != nil {
		t.Fatal(err)
	}
}

// subtitles returns the item's subtitle tracks in the order listed.
func (f *externalFixture) subtitles(t *testing.T) []map[string]any {
	t.Helper()
	response, raw := f.do(t, 0, "GET", "/Items/"+f.text, f.token, nil)
	var item struct{ MediaStreams []map[string]any }
	if err := json.Unmarshal(raw, &item); err != nil || response.StatusCode != 200 {
		t.Fatalf("item: %d %s", response.StatusCode, raw)
	}
	var tracks []map[string]any
	for _, stream := range item.MediaStreams {
		if stream["Type"] == "Subtitle" {
			tracks = append(tracks, stream)
		}
	}
	return tracks
}

// summary is the paths of the tracks that are files, in index order.
func summary(tracks []map[string]any) string {
	var out []string
	for _, track := range tracks {
		if track["IsExternal"] == true {
			out = append(out, fmt.Sprintf("%v=%v", track["Index"], track["Path"]))
		}
	}
	return strings.Join(out, " ")
}

func (f *externalFixture) vtt(t *testing.T, index int) (int, string) {
	t.Helper()
	status, _, body := f.get(t, 0, f.path(f.text, index, 0, "Stream.vtt"))
	return status, body
}

// conversions counts the ffmpeg runs that converted a subtitle file, which
// write the file out as input.<ext> in a scratch directory of their own.
func (f *externalFixture) conversions() int {
	n := 0
	for _, run := range f.tools() {
		if strings.HasPrefix(run, "ffmpeg") && strings.Contains(run, "/input.") {
			n++
		}
	}
	return n
}

func (f *externalFixture) jobs(t *testing.T, kind string) int {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE kind = $1", kind)
}

// traceTools puts ffmpeg and ffprobe in front of the real ones, noting each
// start with its arguments, and returns the lines noted so far.
func traceTools(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "runs")
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf("#!/bin/sh\necho \"%s $*\" >>%q\nexec %q \"$@\"\n", name, log, real)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, _ := os.ReadFile(log)
		if len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

const allFiles = "4=film.en.srt 5=film.es.forced.srt 6=film.it.vtt 7=film.ja.ass 8=film.ja.srt 9=film.nl.srt 10=film.ru.srt"

// A scan converts no subtitle file, extracts no font and writes nothing to the
// asset store: it lists. The tracks are then converted by the worker when
// somebody asks, whatever their character set, once each, from the file and
// never the film.
func TestScanOnlyListsSubtitleFilesAndFonts(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f := externalEnv(t, target)
			for _, run := range f.tools() {
				if strings.Contains(run, "-dump_attachment") || strings.Contains(run, "/input.") || strings.Contains(run, "-sub_charenc") {
					t.Errorf("the scan converted or extracted:\n%s", run)
				}
			}
			if f.conversions() != 0 {
				t.Errorf("the scan converted %d files", f.conversions())
			}
			for _, prefix := range []string{"subtitles/", "attachments/"} {
				if objects, err := f.assets.List(context.Background(), prefix); err != nil && !errors.Is(err, os.ErrNotExist) || len(objects) != 0 {
					t.Errorf("the scan wrote to the asset store under %s: %v %v", prefix, objects, err)
				}
			}
			if f.jobs(t, "track")+f.jobs(t, "fonts") != 0 {
				t.Error("the scan queued a job")
			}

			tracks := f.subtitles(t)
			if got := summary(tracks); got != allFiles || len(tracks) != 9 {
				t.Fatalf("tracks: %s\n%v", summary(tracks), tracks)
			}
			byIndex := map[int]map[string]any{}
			for _, track := range tracks {
				byIndex[int(track["Index"].(float64))] = track
				if track["IsExternal"] == true && (track["Fingerprint"] != nil || track["Unreadable"] != nil || track["IsTextSubtitleStream"] != true || track["SupportsExternalStream"] != true || track["DeliveryUrl"] != nil) {
					t.Errorf("track: %v", track)
				}
			}
			for index, want := range map[int]map[string]any{
				4: {"Codec": "subrip", "Language": "eng", "IsForced": false, "DisplayTitle": "English - SUBRIP - External"},
				5: {"Codec": "subrip", "Language": "spa", "IsForced": true, "DisplayTitle": "Spanish - Forced - SUBRIP - External"},
				6: {"Codec": "webvtt", "Language": "ita", "DisplayTitle": "Italian - WEBVTT - External"},
				7: {"Codec": "ass", "Language": "jpn", "DisplayTitle": "Japanese - ASS - External"},
				8: {"Codec": "subrip", "Language": "jpn", "DisplayTitle": "Japanese - SUBRIP - External"},
				9: {"Codec": "subrip", "Language": "nld", "DisplayTitle": "Dutch - SUBRIP - External"},
			} {
				for field, value := range want {
					if byIndex[index][field] != value {
						t.Errorf("track %d %s: %v, want %v", index, field, byIndex[index][field], value)
					}
				}
			}

			// Asked for, each is converted by a job, in the character set it is in.
			for index, want := range map[int]string{
				4:  "00:00:02.000 --> 00:00:08.000\next en",
				5:  "ext es forced",
				6:  "vtt cue",
				7:  "ASS styled line one",
				8:  japanese_,
				9:  "caf\u00e9 latin1",
				10: russian,
			} {
				if status, body := f.vtt(t, index); status != 200 || !strings.Contains(body, want) {
					t.Errorf("track %d: %d %q, want %q", index, status, body, want)
				}
			}
			if status, ctype, body := f.get(t, 1, f.path(f.text, 7, 0, "Stream.ass")); status != 200 || ctype != "text/x-ssa" || !strings.Contains(body, "Style: Loud,Arial,48") {
				t.Errorf("ass: %d %s %q", status, ctype, body)
			}
			if status, ctype, body := f.get(t, 1, f.path(f.text, 4, 0, "Stream.js")); status != 200 || ctype != "application/json" || !strings.Contains(body, `"Text":"ext en"`) {
				t.Errorf("js: %d %s %q", status, ctype, body)
			}
			if status, _ := f.vtt(t, 11); status != 404 {
				t.Errorf("a track past the files: %d", status)
			}
			if f.conversions() != 7 || f.jobs(t, "track") != 7 {
				t.Errorf("%d conversions and %d jobs for 7 files requested twice over", f.conversions(), f.jobs(t, "track"))
			}
			for _, run := range f.tools() {
				if strings.Contains(run, "/input.") && strings.Contains(run, "film.mkv") {
					t.Errorf("the film was read for a file's track: %s", run)
				}
			}
			// The worker converts to UTF-8 itself, so ffmpeg is told no character set.
			if strings.Contains(strings.Join(f.tools(), "\n"), "-sub_charenc") {
				t.Errorf("ffmpeg was told a character set:\n%s", strings.Join(f.tools(), "\n"))
			}
		})
	}
}

// PlaybackInfo hands the tracks and fonts over with the caller's token, and the
// server chooses no default subtitle.
func TestPlaybackInfoDeliversFilesAndFontsWithTheToken(t *testing.T) {
	f := externalEnv(t, "")
	f.enableTranscoding(t)
	var profile map[string]any
	if err := json.Unmarshal(capturedProfile(t), &profile); err != nil {
		t.Fatal(err)
	}
	source := firstSource(f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": profile, "MediaSourceId": db.CompactID(f.text)}))
	delivered := subtitleTracks(source)
	prefix := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Subtitles/"
	if len(delivered) != 9 || delivered[4]["DeliveryMethod"] != "External" || delivered[4]["IsExternal"] != true ||
		delivered[4]["DeliveryUrl"] != prefix+"4/0/Stream.vtt?ApiKey="+f.token || delivered[7]["DeliveryUrl"] != prefix+"7/0/Stream.ass?ApiKey="+f.token {
		t.Errorf("delivered: %v", delivered)
	}
	// A font attachment and the first subtitle file share an Index in the
	// listing; each is listed once.
	seen := map[string]int{}
	streams, _ := source["MediaStreams"].([]any)
	for _, entry := range streams {
		stream := entry.(map[string]any)
		seen[fmt.Sprint(stream["Type"], stream["Index"])]++
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("%s is listed %d times", key, n)
		}
	}
	// The last file is still at the position its Index names.
	last := firstSource(f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": profile, "MediaSourceId": db.CompactID(f.text), "SubtitleStreamIndex": 7}))
	if last["DefaultSubtitleStreamIndex"] != float64(7) {
		t.Errorf("naming track 7 gave %v", last["DefaultSubtitleStreamIndex"])
	}
	// The server chooses none: the viewer's subtitle mode is not stored yet.
	if source["DefaultSubtitleStreamIndex"] != float64(-1) {
		t.Errorf("DefaultSubtitleStreamIndex %v, want -1", source["DefaultSubtitleStreamIndex"])
	}
	// A client that asked for none (-1) gets none.
	off := firstSource(f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": profile, "MediaSourceId": db.CompactID(f.text), "SubtitleStreamIndex": -1}))
	if off["DefaultSubtitleStreamIndex"] != float64(-1) {
		t.Errorf("a client that turned subtitles off got %v", off["DefaultSubtitleStreamIndex"])
	}
	// The details page chooses none either.
	sources, _, err := f.apis[0].Items.MediaSources(context.Background(), f.text)
	if err != nil || sources[0].DefaultSubtitleStreamIndex != nil {
		t.Errorf("the stored item has a default: %v %v", sources, err)
	}
	_, raw := f.do(t, 0, "GET", "/Items/"+f.text, f.token, nil)
	var item struct{ MediaSources []map[string]any }
	if err := json.Unmarshal(raw, &item); err != nil || item.MediaSources[0]["DefaultSubtitleStreamIndex"] == float64(4) {
		t.Errorf("the details page default: %v %v", item.MediaSources, err)
	}

	// A font is listed once it is stored, and PlaybackInfo has it stored.
	if got, _ := source["MediaAttachments"].([]any); len(got) != 0 {
		t.Errorf("a font is listed before a worker stored it: %v", got)
	}
	if got := f.listedFonts(t); len(got) != 0 {
		t.Errorf("the details list a font before it is stored: %v", got)
	}
	f.waitFonts(t)
	source = firstSource(f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": profile, "MediaSourceId": db.CompactID(f.text)}))
	attachments, _ := source["MediaAttachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("attachments: %v", source["MediaAttachments"])
	}
	wantURL := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/4"
	font := attachments[0].(map[string]any)
	if font["Index"] != float64(4) || font["FileName"] != "Serif.ttf" || font["MimeType"] != "font/ttf" || font["DeliveryUrl"] != wantURL+"?ApiKey="+f.token {
		t.Errorf("attachment: %v", font)
	}
	// The details list the font without the URL, as Jellyfin's do.
	if listed := f.listedFonts(t); len(listed) != 1 || listed[0].(map[string]any)["DeliveryUrl"] != nil {
		t.Errorf("listed fonts: %v", listed)
	}
	// Only the fonts in the film's manifest are listed.
	source2, _, err := f.apis[0].Items.Source(context.Background(), f.text, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.assets.Put(context.Background(), subtitles.FontsKey(source2.ID, source2.ETag), strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if got := f.listedFonts(t); len(got) != 0 {
		t.Errorf("a font left out of the manifest is listed: %v", got)
	}
}

// listedFonts is the MediaAttachments in the film's details.
func (f *externalFixture) listedFonts(t *testing.T) []any {
	t.Helper()
	_, raw := f.do(t, 0, "GET", "/Items/"+f.text, f.token, nil)
	var item struct{ MediaSources []map[string]any }
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("item: %s", raw)
	}
	listed, _ := item.MediaSources[0]["MediaAttachments"].([]any)
	return listed
}

// waitFonts waits for a worker to store the film's fonts.
func (f *externalFixture) waitFonts(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for len(f.listedFonts(t)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no worker stored the fonts")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The font is served to a caller with a token only. A PlaybackInfo, however
// many ask, has a worker take all the fonts out of the film once; the font is
// listed and served after that, and a font or a list that is lost is made again.
func TestFontsAreExtractedOnceAndServedWithAToken(t *testing.T) {
	f := externalEnv(t, "")
	attachment := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/4"
	if response, _ := f.do(t, 1, "GET", attachment, "", nil); response.StatusCode != 401 {
		t.Errorf("without a token: %d", response.StatusCode)
	}
	if n := f.jobs(t, "fonts"); n != 0 {
		t.Fatalf("a refused request queued %d jobs", n)
	}
	dumps := func() (n int) {
		for _, run := range f.tools() {
			if strings.Contains(run, "-dump_attachment") {
				n++
			}
		}
		return n
	}
	sum := sha256.Sum256([]byte(fontBytes))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	var done sync.WaitGroup
	for i := range 6 {
		done.Add(1)
		go func() {
			defer done.Done()
			f.playbackInfo(t, i%2, f.text, map[string]any{"MediaSourceId": db.CompactID(f.text)})
		}()
	}
	done.Wait()
	f.waitFonts(t)
	for i := range 6 {
		done.Add(1)
		go func() {
			defer done.Done()
			response, body := f.do(t, i%2, "GET", attachment+"?ApiKey="+f.token, "", nil)
			if response.StatusCode != 200 || response.Header.Get("Content-Type") != "font/ttf" || string(body) != fontBytes ||
				response.Header.Get("Cache-Control") != "no-cache" || response.Header.Get("ETag") != etag {
				t.Errorf("font: %d %s %q", response.StatusCode, response.Header.Get("Content-Type"), body)
			}
		}()
	}
	done.Wait()
	if f.jobs(t, "fonts") != 1 || dumps() != 1 {
		t.Errorf("%d fonts jobs and %d extractions for 6 requests", f.jobs(t, "fonts"), dumps())
	}
	// A client that holds the font is told so by its ETag, with no body.
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil, "If-None-Match", etag); response.StatusCode != 304 || len(body) != 0 {
		t.Errorf("a font the client holds: %d, %d bytes", response.StatusCode, len(body))
	}
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil, "If-None-Match", `"another"`); response.StatusCode != 200 || string(body) != fontBytes {
		t.Errorf("another font held: %d", response.StatusCode)
	}
	for name, path := range map[string]string{
		"an index that is no attachment": "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/3",
		"a stream past the end":          "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/99",
		"text for an index":              "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/x",
		"a negative index":               "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/-1",
		"another source":                 "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.other) + "/Attachments/4",
		"another item":                   "/Videos/" + db.DashedID(f.other) + "/" + db.CompactID(f.text) + "/Attachments/4",
	} {
		if response, _ := f.do(t, 0, "GET", path+"?ApiKey="+f.token, "", nil); response.StatusCode != 404 {
			t.Errorf("%s: %d, want 404", name, response.StatusCode)
		}
	}
	if f.jobs(t, "fonts") != 1 {
		t.Errorf("a request for no font queued a job")
	}

	// A font gone from the store is answered with an empty body at once, and the fonts job is
	// run again without waiting out the failure memory.
	ctx := context.Background()
	source, _, err := f.apis[0].Items.Source(ctx, f.text, "")
	if err != nil {
		t.Fatal(err)
	}
	fontKey := subtitles.FontKey(hex.EncodeToString(sum[:]))
	if err := f.assets.Delete(ctx, fontKey); err != nil {
		t.Fatal(err)
	}
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil); response.StatusCode != 200 || len(body) != 0 ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("ETag") != "" {
		t.Errorf("a font gone from the store: %d, %d bytes, %q", response.StatusCode, len(body), response.Header)
	}
	for deadline := time.Now().Add(30 * time.Second); !subtitles.Exists(ctx, f.assets, fontKey) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil); response.StatusCode != 200 || string(body) != fontBytes || dumps() != 2 {
		t.Errorf("after losing the font: %d %q, %d extractions", response.StatusCode, body, dumps())
	}

	// The list lost, nothing is listed until a request makes the fonts again, once.
	if err := f.assets.Delete(ctx, subtitles.FontsKey(source.ID, source.ETag)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, f.pool, "UPDATE subtitle_jobs SET ended_at = now() - interval '1 hour'")
	if got := f.listedFonts(t); len(got) != 0 {
		t.Errorf("fonts listed with no manifest: %v", got)
	}
	// An error status would stop jellyfin-web drawing the track that listed it.
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil); response.StatusCode != 200 || len(body) != 0 || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a font with no manifest: %d, %d bytes", response.StatusCode, len(body))
	}
	f.waitFonts(t)
	if response, body := f.do(t, 0, "GET", attachment+"?ApiKey="+f.token, "", nil); response.StatusCode != 200 || string(body) != fontBytes || dumps() != 3 {
		t.Errorf("after losing the list: %d %q, %d extractions", response.StatusCode, body, dumps())
	}
}

// A listed font that the store fails to give is answered with an empty font,
// not an error that would stop the viewer's renderer.
func TestListedFontThatCannotBeReadIsAnsweredEmpty(t *testing.T) {
	f := externalEnv(t, "")
	f.playbackInfo(t, 0, f.text, map[string]any{"MediaSourceId": db.CompactID(f.text)})
	f.waitFonts(t)
	attachment := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Attachments/4?ApiKey=" + f.token
	sum := sha256.Sum256([]byte(fontBytes))
	var path string
	filepath.WalkDir(f.assetDir, func(p string, d os.DirEntry, err error) error { //nolint:errcheck // Only the font is looked for.
		if err == nil && !d.IsDir() && filepath.Base(p) == hex.EncodeToString(sum[:]) {
			path = p
		}
		return nil
	})
	if path == "" {
		t.Fatal("the font is not in the asset store")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) }) //nolint:errcheck // The directory is removed anyway.
	if response, body := f.do(t, 0, "GET", attachment, "", nil); response.StatusCode != 200 || len(body) != 0 || response.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("a font the store can't read: %d, %d bytes", response.StatusCode, len(body))
	}
}

// Only the details and PlaybackInfo list fonts, which they list once they are
// stored; a list of items, and the sessions, never do (invariant 32).
func TestListsAndSessionsDoNotListFonts(t *testing.T) {
	f := externalEnv(t, "")
	f.playbackInfo(t, 0, f.text, map[string]any{"MediaSourceId": db.CompactID(f.text)})
	f.waitFonts(t)
	for _, path := range []string{
		"/Items?Recursive=true&IncludeItemTypes=Movie&Fields=MediaSources",
		"/Items/Latest?IncludeItemTypes=Movie&Fields=MediaSources",
	} {
		_, raw := f.do(t, 0, "GET", path, f.token, nil)
		var found struct{ Items []map[string]any }
		var latest []map[string]any
		if json.Unmarshal(raw, &found) != nil || found.Items == nil {
			if err := json.Unmarshal(raw, &latest); err != nil {
				t.Fatalf("%s: %s", path, raw)
			}
			found.Items = latest
		}
		var films int
		for _, item := range found.Items {
			sources, _ := item["MediaSources"].([]any)
			for _, entry := range sources {
				films++
				if attachments, _ := entry.(map[string]any)["MediaAttachments"].([]any); len(attachments) != 0 {
					t.Errorf("%s lists fonts: %v", path, attachments)
				}
			}
		}
		if films == 0 {
			t.Errorf("%s: no media sources in %s", path, raw)
		}
	}
	if len(f.listedFonts(t)) != 1 {
		t.Errorf("the details lost the fonts")
	}
}

// A rescan sees a subtitle file added, changed or removed, keeps the item, does
// not read the film again, and does not give a removed file's index to another.
func TestRescanNoticesSubtitleFiles(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f := externalEnv(t, target)
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
			tag := func() string {
				return scalar[string](t, f.pool, "SELECT dto->>'Etag' || COALESCE(primary_image_tag, '') FROM items WHERE id = $1", f.text)
			}
			before := tag()

			runs := len(f.tools())
			rescan("nothing changed")
			if len(f.tools()) != runs {
				t.Fatalf("a scan with nothing new ran:\n%s", strings.Join(f.tools()[runs:], "\n"))
			}

			f.put(t, "film.fr.srt", srtCue("ext fr"))
			rescan("added")
			if got, want := summary(f.subtitles(t)), allFiles+" 11=film.fr.srt"; !strings.HasSuffix(got, "9=film.nl.srt 10=film.ru.srt 11=film.fr.srt") {
				t.Fatalf("after adding: %s, want %s", got, want)
			}
			// The files that were there keep their indices, which a viewer who has
			// the menu open depends on, and the new one follows them.
			for index, want := range map[int]string{4: "ext en", 6: "vtt cue", 9: "caf\u00e9 latin1", 11: "ext fr"} {
				if status, body := f.vtt(t, index); status != 200 || !strings.Contains(body, want) {
					t.Errorf("after adding, track %d: %d %q, want %q", index, status, body, want)
				}
			}

			trackKey := func() string {
				source, _, err := f.apis[0].Items.Source(context.Background(), f.text, "")
				if err != nil {
					t.Fatal(err)
				}
				return subtitles.Key(source, 5)
			}
			if status, body := f.vtt(t, 5); status != 200 || !strings.Contains(body, "ext es forced") {
				t.Fatalf("before editing: %d %q", status, body)
			}
			oldKey := trackKey()
			f.put(t, "film.es.forced.srt", srtCue("the spanish was edited"))
			rescan("edited")
			// The edit has a key of its own, so a viewer reading the old text is not cut off.
			old, found, err := subtitles.Read(context.Background(), f.assets, oldKey)
			if oldKey == trackKey() || err != nil || !found || !strings.Contains(old.VTT, "ext es forced") {
				t.Errorf("the edited file kept its key %s: %v %v", oldKey, found, err)
			}
			if status, body := f.vtt(t, 5); status != 200 || !strings.Contains(body, "the spanish was edited") {
				t.Errorf("after editing: %d %q", status, body)
			}

			if err := f.media.Delete(context.Background(), "film.en.srt"); err != nil {
				t.Fatal(err)
			}
			rescan("removed")
			if got := summary(f.subtitles(t)); strings.Contains(got, "film.en.srt") || !strings.HasPrefix(got, "5=film.es.forced.srt 6=film.it.vtt") {
				t.Fatalf("after removing: %s", got)
			}
			if status, body := f.vtt(t, 5); status != 200 || !strings.Contains(body, "the spanish was edited") {
				t.Errorf("after removing, track 5: %d %q", status, body)
			}
			if status, _ := f.vtt(t, 4); status != 404 {
				t.Errorf("the removed file's index: %d", status)
			}

			// A file added later does not take the removed file's index.
			f.put(t, "film.de.srt", srtCue("ext de"))
			rescan("added after removing")
			if got := summary(f.subtitles(t)); !strings.HasSuffix(got, "11=film.fr.srt 12=film.de.srt") || strings.Contains(got, "4=") {
				t.Errorf("after adding later: %s", got)
			}
			if status, body := f.vtt(t, 12); status != 200 || !strings.Contains(body, "ext de") {
				t.Errorf("the new file: %d %q", status, body)
			}
			if status, _ := f.vtt(t, 4); status != 404 {
				t.Errorf("the removed file's index after another was added: %d", status)
			}
			// The item is the same one, with its poster and etag.
			if tag() != before {
				t.Errorf("the item changed: %s, was %s", tag(), before)
			}

			// The file with the highest index is removed, then the film itself is
			// replaced (which scans it again and writes its row anew), then another
			// file is added: only the counter that outlives the row keeps the
			// removed file's index from going to the new one.
			if err := f.media.Delete(context.Background(), "film.de.srt"); err != nil {
				t.Fatal(err)
			}
			rescan("highest removed")
			body, err := f.media.Open(context.Background(), "film.mkv", 0)
			if err != nil {
				t.Fatal(err)
			}
			film, err := io.ReadAll(body)
			body.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.put(t, "film.mkv", string(film)+"\x00") // Another ETag.
			f.put(t, "film.pt.srt", srtCue("ext pt"))
			f.scan(t)
			if got := summary(f.subtitles(t)); !strings.HasSuffix(got, "11=film.fr.srt 13=film.pt.srt") || strings.Contains(got, "12=") {
				t.Errorf("after replacing the film: %s", got)
			}
			if status, body := f.vtt(t, 13); status != 200 || !strings.Contains(body, "ext pt") {
				t.Errorf("the file added after the film was replaced: %d %q", status, body)
			}
		})
	}
}

// A file that can't be read for good costs one job for as long as it is the
// same file: it is hidden when the job finds out, stays hidden over rescans and
// requests, and comes back with its index when it is edited. The warning names
// it.
func TestBadSubtitleFileCostsOneJobPerFingerprint(t *testing.T) {
	binary := "1\n00:00:01,000 --> 00:00:03,000\n\x02\xe3\x81\xff\n"
	// The second is refused before ffmpeg is run.
	for name, bad := range map[string]struct {
		content string
		runs    int
	}{
		"ffmpeg rejects it":                 {"this is not a subtitle file\x00\x01\x02", 1},
		"no character set reads it as text": {binary, 0},
	} {
		t.Run(name, func(t *testing.T) { badSubtitleFile(t, bad.content, bad.runs) })
	}
}

// badSubtitleFile does the sequence for a file with content that costs runs
// ffmpeg runs to find out it is bad.
func badSubtitleFile(t *testing.T, bad string, runs int) {
	f := externalEnv(t, "")
	f.put(t, "film.xx.srt", bad)
	f.scan(t)
	index := 11
	if got := summary(f.subtitles(t)); !strings.HasSuffix(got, "10=film.ru.srt 11=film.xx.srt") {
		t.Fatalf("a file nobody has read is listed: %s", got)
	}
	jobsOfIt := func() int {
		return scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE key LIKE '%/"+fmt.Sprint(index)+"-%'")
	}

	if status, _ := f.vtt(t, index); status != 500 {
		t.Errorf("the first request: %d, want the job's failure", status)
	}
	if f.conversions() != runs || jobsOfIt() != 1 {
		t.Fatalf("%d conversions, %d jobs after the first request", f.conversions(), jobsOfIt())
	}
	if !strings.Contains(f.log.String(), "film.xx.srt") || !strings.Contains(f.log.String(), "level=WARN") {
		t.Errorf("no warning names the file:\n%s", f.log)
	}
	hidden := func(why string) {
		t.Helper()
		if got := summary(f.subtitles(t)); strings.Contains(got, "film.xx.srt") {
			t.Errorf("%s: it is still listed: %s", why, got)
		}
		var profile map[string]any
		if err := json.Unmarshal(capturedProfile(t), &profile); err != nil {
			t.Fatal(err)
		}
		f.enableTranscoding(t)
		if _, listed := subtitleTracks(firstSource(f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": profile, "MediaSourceId": db.CompactID(f.text)})))[index]; listed {
			t.Errorf("%s: PlaybackInfo lists it", why)
		}
		if status, _ := f.vtt(t, index); status != 404 {
			t.Errorf("%s: a request for it: %d, want 404", why, status)
		}
	}
	hidden("after its job")

	f.scan(t)
	hidden("after a rescan")
	f.scan(t)
	hidden("after another")
	if f.conversions() != runs || jobsOfIt() != 1 {
		t.Errorf("%d conversions, %d jobs for a file that did not change", f.conversions(), jobsOfIt())
	}
	// The other files were not affected.
	if status, body := f.vtt(t, 4); status != 200 || !strings.Contains(body, "ext en") {
		t.Errorf("a good file: %d %q", status, body)
	}

	// Editing it is another file.
	f.put(t, "film.xx.srt", srtCue("fixed"))
	f.scan(t)
	if got := summary(f.subtitles(t)); !strings.HasSuffix(got, "10=film.ru.srt 11=film.xx.srt") {
		t.Fatalf("after the edit: %s", got)
	}
	if status, body := f.vtt(t, index); status != 200 || !strings.Contains(body, "fixed") {
		t.Errorf("the edited file: %d %q", status, body)
	}
	if f.conversions() != runs+2 { // The bad file, the good file, the edit.
		t.Errorf("%d conversions", f.conversions())
	}
}

// A file that fails for a reason that is not its own (here, the store has lost
// it for a moment) stays listed, keeps its index, and is read again at a later
// request.
func TestTransientFailureKeepsTheTrack(t *testing.T) {
	f := externalEnv(t, "")
	if err := f.media.Delete(context.Background(), "film.en.srt"); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.vtt(t, 4); status != 500 {
		t.Errorf("a file the store can't open: %d", status)
	}
	if got := summary(f.subtitles(t)); !strings.HasPrefix(got, "4=film.en.srt") {
		t.Errorf("a track whose read failed once is not listed: %s", got)
	}
	f.put(t, "film.en.srt", srtCue("ext en"))
	mustExec(t, f.pool, "DELETE FROM subtitle_jobs") // Past the failure memory.
	if status, body := f.vtt(t, 4); status != 200 || !strings.Contains(body, "ext en") {
		t.Errorf("when the store has it again: %d %q", status, body)
	}
}

// A conversion that ffmpeg did not finish (the OOM killer's SIGKILL, say) says
// nothing about the file: the track stays listed, through a rescan too, and is
// converted at a later request.
func TestKilledConversionKeepsTheTrack(t *testing.T) {
	f := externalEnv(t, "")
	next, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "ffmpeg")
	write := func(script string) {
		t.Helper()
		if err := os.WriteFile(shim+".tmp", []byte("#!/bin/sh\n"+script+"\nexec "+next+" \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(shim+".tmp", shim); err != nil {
			t.Fatal(err)
		}
	}
	write(`case "$*" in */input.*) kill -9 $$;; esac`)
	t.Setenv("PATH", filepath.Dir(shim)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if status, _ := f.vtt(t, 4); status != 500 {
		t.Errorf("a killed conversion: %d", status)
	}
	f.scan(t)
	if got := summary(f.subtitles(t)); !strings.HasPrefix(got, "4=film.en.srt") {
		t.Errorf("a track whose conversion was killed is not listed: %s", got)
	}
	write("")
	mustExec(t, f.pool, "DELETE FROM subtitle_jobs") // Past the failure memory.
	if status, body := f.vtt(t, 4); status != 200 || !strings.Contains(body, "ext en") {
		t.Errorf("at a later request: %d %q", status, body)
	}
}

// A film scanned before subtitle files and attachments were listed has no
// fingerprint, and its next scan reads the film again to list them.
func TestRescanListsTheFilesOfAFilmScannedBefore(t *testing.T) {
	f := externalEnv(t, "")
	// As the previous release left it: JS-086 gives every source it stored a probe version below the binary's.
	mustExec(t, f.pool, "UPDATE media_sources SET sidecars = NULL, probe_version = 0")
	mustExec(t, f.pool, `UPDATE media_sources SET streams = jsonb_path_query_array(streams, '$[*] ? (@.IsExternal != true && @.Type != "Attachment")'), next_index = 0`)
	mustExec(t, f.pool, `UPDATE items SET dto = jsonb_set(jsonb_set(dto, '{MediaSources,0,MediaAttachments}', '[]'), '{MediaSources,0,MediaStreams}',
		jsonb_path_query_array(dto, '$.MediaSources[0].MediaStreams[*] ? (@.IsExternal != true)'))`)
	if summary(f.subtitles(t)) != "" {
		t.Fatalf("setup: %v", summary(f.subtitles(t)))
	}
	f.scan(t)
	if got := summary(f.subtitles(t)); got != allFiles {
		t.Errorf("after the scan: %s", got)
	}
	sources, _, err := f.apis[0].Items.MediaSources(context.Background(), f.text)
	if err != nil || len(sources[0].MediaAttachments) != 1 {
		t.Errorf("attachments %v: %v", sources[0].MediaAttachments, err)
	}
	before := len(f.tools())
	f.scan(t)
	if runs := f.tools()[before:]; len(runs) != 0 {
		t.Errorf("a scan with nothing to list ran:\n%s", strings.Join(runs, "\n"))
	}
}

// Replicas of two versions share the database while one is replaced. An older
// version's scan deletes and rewrites a film's media source and only updates
// its scan_objects row in place, so a fingerprint kept in scan_objects would
// outlive the rewrite and the new scan would find nothing to list. The
// statements below are the old scan's, for a film whose files were listed.
func TestRescanAfterAnOlderVersionsScanListsTheFilesAgain(t *testing.T) {
	f := externalEnv(t, "")
	if got := summary(f.subtitles(t)); got != allFiles {
		t.Fatalf("setup: %s", got)
	}
	mustExec(t, f.pool, `UPDATE items SET dto = jsonb_set(jsonb_set(dto, '{MediaSources,0,MediaAttachments}', '[]'), '{MediaSources,0,MediaStreams}',
		jsonb_path_query_array(dto, '$.MediaSources[0].MediaStreams[*] ? (@.IsExternal != true)'))`)
	mustExec(t, f.pool, `WITH old AS (DELETE FROM media_sources RETURNING *)
		INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end)
		SELECT id, item_id, store_key, size, object_etag, container,
			jsonb_path_query_array(streams, '$[*] ? (@.IsExternal != true && @.Type != "Attachment")'), keyframes, video_end FROM old`)
	mustExec(t, f.pool, "UPDATE scan_objects SET size = size, etag = etag")
	if summary(f.subtitles(t)) != "" {
		t.Fatalf("setup: %v", summary(f.subtitles(t)))
	}
	f.scan(t)
	if got := summary(f.subtitles(t)); got != allFiles {
		t.Errorf("after the scan: %s", got)
	}
}

// The scan after the upgrade probes only the Matroska films scanned before
// attachments were listed (the one container whose attachments matter), keeps
// the keyframe index it stored for them, stores each film as its probe ends so
// that a scan cut short keeps the films done, and lists the files of the
// others without opening them.
func TestUpgradeScanProbesMatroskaAndKeepsItsWork(t *testing.T) {
	f := externalEnv(t, "")
	dir := t.TempDir()
	for _, name := range []string{"b.mp4", "slow.mkv"} {
		path := filepath.Join(dir, name)
		ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-c:v", "libx264", "-preset", "ultrafast", path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f.put(t, name, string(data))
	}
	f.put(t, "b.en.srt", srtCue("beside the mp4"))
	f.scan(t)

	// As the previous release left them: no fingerprints, and an index that
	// would show if it were read again. Other tests' rows share the database,
	// so the checks below name the scanned item, not just the object key.
	// The Matroska films were stored by the previous release, so they are older than this binary's probe;
	// the mp4, which that release listed the files of without opening, is what a scan lists again without a probe.
	mustExec(t, f.pool, "UPDATE media_sources SET sidecars = NULL, probe_version = CASE WHEN container = 'mkv' THEN 0 ELSE probe_version END")
	mustExec(t, f.pool, "UPDATE media_sources SET keyframes = '{0,5000000}', video_end = 777")
	keyframes := func(key string) string {
		return scalar[string](t, f.pool, "SELECT m.keyframes::text || ' ' || m.video_end FROM media_sources m JOIN items i ON i.id = m.item_id WHERE m.store_key = $1 AND i.natural_key = 'video:' || $1", key)
	}
	listed := func(key string) bool {
		return scalar[bool](t, f.pool, "SELECT m.sidecars IS NOT NULL FROM media_sources m JOIN items i ON i.id = m.item_id WHERE m.store_key = $1 AND i.natural_key = 'video:' || $1", key)
	}

	// A ffprobe that holds slow.mkv for as long as the marker file exists.
	shim, hold := t.TempDir(), filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *slow.mkv*) while [ -e %q ]; do sleep 0.1; done;; esac\nexec %q \"$@\"\n", hold, real)
	if err := os.WriteFile(filepath.Join(shim, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The scan is cut short while slow.mkv is held, once film.mkv is stored.
	cut, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.scanner.Scan(cut, f.lib) //nolint:errcheck // Cut short on purpose.
	}()
	for began := time.Now(); !listed("film.mkv"); time.Sleep(50 * time.Millisecond) {
		if time.Since(began) > 30*time.Second {
			t.Fatal("film.mkv was not stored while slow.mkv was still being probed")
		}
	}
	cancel()
	os.Remove(hold)
	<-finished
	if listed("slow.mkv") {
		t.Fatal("slow.mkv was stored by a scan that was cut short")
	}
	if got := keyframes("film.mkv"); got != "{0,5000000} 777" {
		t.Errorf("film.mkv's index after its probe: %s", got)
	}

	// The next scan has only slow.mkv left to probe, and opens neither film.mkv
	// nor the mp4 for it.
	before := len(f.tools())
	f.scan(t)
	// Its image is made again (one frame), but the index is not read.
	var ffprobes, others int
	for _, run := range f.tools()[before:] {
		switch {
		case strings.HasPrefix(run, "ffprobe") && strings.Contains(run, "slow.mkv"):
			ffprobes++
		case strings.HasPrefix(run, "ffmpeg") && strings.Contains(run, "image2pipe") && strings.Contains(run, "slow.mkv"):
		default:
			others++
			t.Errorf("the second scan ran %s", run)
		}
	}
	if ffprobes != 1 || others != 0 {
		t.Errorf("the second scan probed slow.mkv %d times and ran %d other tools", ffprobes, others)
	}
	if !listed("slow.mkv") || !listed("b.mp4") {
		t.Error("the second scan did not finish the listing")
	}
	if got := keyframes("film.mkv"); got != "{0,5000000} 777" {
		t.Errorf("film.mkv's index after the second scan: %s", got)
	}
	if got := scalar[int](t, f.pool, `SELECT jsonb_array_length(jsonb_path_query_array(streams, '$[*] ? (@.IsExternal == true)')) FROM media_sources WHERE store_key = 'b.mp4'`); got != 1 {
		t.Errorf("the mp4 lists %d subtitle files, want 1", got)
	}
}
