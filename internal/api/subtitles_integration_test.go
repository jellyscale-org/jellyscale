package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/transcode"
)

// These need ffmpeg and a disposable database; test/integration.sh provides both.

const (
	testSRT = "1\n00:00:02,000 --> 00:00:30,000\nEnglish SRT line one\n\n2\n00:00:30,500 --> 00:01:20,000\nEnglish SRT line two\n"
	testASS = `[Script Info]
ScriptType: v4.00+
PlayResX: 1280
PlayResY: 720

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Loud,Arial,48,&H0000FF00,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:02.00,0:00:30.00,Loud,,0,0,0,,ASS styled line one
Dialogue: 0,0:00:30.50,0:01:20.00,Loud,,0,0,0,,ASS styled line two
`
)

type subtitleFixture struct {
	*env
	text, image string // The two films' item IDs; each has its own library.
	textStore   string
	runs        func() int // How many times ffmpeg has started since the scan.
	log         *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func ffmpegRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// subtitleFilm makes a short H.264 and FLAC film with a SubRip track (stream 2,
// the default) and an ASS track (3). With image set, it has only a track that
// says it is PGS, which is all the probe looks at.
func subtitleFilm(t *testing.T, image bool, srt string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	// Not AAC: its negative start moves cues 23 ms on ffmpeg 6 (invariant 43).
	dir := t.TempDir()
	must := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	a, b := must("a.srt", srt), must("b.ass", testASS)
	film := filepath.Join(dir, "film.mkv")
	input := []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3"}
	codecs := []string{"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "flac", "-metadata:s:a:0", "language=eng"}
	if image {
		ffmpegRun(t, append(append(input, "-i", b, "-map", "0", "-map", "1", "-map", "2", "-c:s", "ass"), append(codecs, film)...)...)
	} else {
		ffmpegRun(t, append(append(input, "-i", a, "-i", b, "-map", "0", "-map", "1", "-map", "2", "-map", "3", "-c:s:0", "srt", "-c:s:1", "ass",
			"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=jpn", "-disposition:s:0", "default"), append(codecs, film)...)...)
	}
	data, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	if image {
		data = bytes.ReplaceAll(data, []byte("S_TEXT/ASS"), []byte("S_HDMV/PGS"))
	}
	return data
}

// subtitleEnv scans two films into libraries on POSIX or on the S3 target, with
// the assets there too.
func subtitleEnv(t *testing.T, target string) *subtitleFixture {
	t.Helper()
	f := &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}
	ctx := context.Background()
	films := map[string][]byte{"Text": subtitleFilm(t, false, testSRT), "Image": subtitleFilm(t, true, testSRT)}
	for name, data := range films {
		media, assets := "file://"+t.TempDir(), "file://"+t.TempDir()
		if target != "" {
			media, assets = s3Root(t, target), s3Root(t, target)
		}
		files, err := store.Open(media)
		if err != nil {
			t.Fatal(err)
		}
		if err := files.Put(ctx, "film.mkv", bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		libs, err := library.Sync(ctx, f.pool, []library.Config{{Name: name, Kind: "movies", Media: media, Assets: assets}})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&library.Scanner{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}).Scan(ctx, libs[0]); err != nil {
			t.Fatal(err)
		}
		id := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE library_id = $1 AND type = 'Movie'", libs[0].ID)
		if name == "Text" {
			f.text, f.textStore = id, media
		} else {
			f.image = id
		}
	}
	for _, s := range f.apis {
		s.Log = slog.New(slog.NewTextHandler(f.log, nil))
	}
	f.runs = countFFmpeg(t)
	return f
}

// countFFmpeg puts an ffmpeg in front of the real one that notes each start,
// and returns the count. Real processes, so it counts what really ran.
func countFFmpeg(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >>%q\nexec %q \"$@\"\n", count, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(count)
		return strings.Count(string(data), "run")
	}
}

func eachBackend(t *testing.T, test func(t *testing.T, f *subtitleFixture)) {
	for _, target := range append([]string{""}, s3Targets()...) {
		t.Run("backend="+target, func(t *testing.T) { test(t, subtitleEnv(t, target)) })
	}
}

func (f *subtitleFixture) path(item string, index int, ticks int64, file string) string {
	return fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/%d/%s", db.DashedID(item), db.CompactID(item), index, ticks, file)
}

func (f *subtitleFixture) get(t *testing.T, replica int, path string) (int, string, string) {
	t.Helper()
	response, body := f.do(t, replica, "GET", path+"?ApiKey="+f.token, "", nil)
	return response.StatusCode, response.Header.Get("Content-Type"), string(body)
}

func TestSubtitleRoutesAnswerAsCaptured(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *subtitleFixture) {
		for _, c := range []struct {
			index int
			ticks int64
			file  string
			ctype string
			want  string // Contained in the body.
		}{
			{2, 0, "Stream.vtt", "text/vtt", "WEBVTT\n\n00:00:02.000 --> 00:00:30.000\nEnglish SRT line one\n\n00:00:30.500 --> 00:01:20.000\nEnglish SRT line two\n"},
			{2, 300000000, "Stream.vtt", "text/vtt", "WEBVTT\n\n00:00:00.000 --> 00:00:00.000\nEnglish SRT line one\n\n00:00:00.500 --> 00:00:50.000\nEnglish SRT line two\n"},
			{2, 0, "Stream.srt", "application/x-subrip", "1\n00:00:02,000 --> 00:00:30,000\nEnglish SRT line one\n\n2\n00:00:30,500 --> 00:01:20,000\nEnglish SRT line two\n"},
			{2, 0, "Stream.js", "application/json", `{"TrackEvents":[{"Id":"1","Text":"English SRT line one","StartPositionTicks":20000000,"EndPositionTicks":300000000},{"Id":"2","Text":"English SRT line two","StartPositionTicks":305000000,"EndPositionTicks":800000000}]}`},
			{3, 0, "Stream.ass", "text/x-ssa", "Style: Loud,Arial,48,&H0000FF00"},
			{3, 300000000, "Stream.ass", "text/x-ssa", "Dialogue: 0,0:00:00.50,0:00:50.00,Loud,,0,0,0,,ASS styled line two"},
			{3, 0, "Stream.vtt", "text/vtt", "00:00:02.000 --> 00:00:30.000\n<b>ASS styled line one</b>"}, // The tags are ffmpeg's, kept as markup.
			{2, 0, "STREAM.VTT", "text/vtt", "English SRT line one"},                                      // Case-insensitive, like the other routes.
		} {
			status, ctype, body := f.get(t, 0, f.path(f.text, c.index, c.ticks, c.file))
			if status != 200 || ctype != c.ctype || !strings.Contains(body, c.want) {
				t.Errorf("%d/%d/%s: %d %q\n%s", c.index, c.ticks, c.file, status, ctype, body)
			}
		}
		// The api path's own case: every segment lowercase.
		if status, _, _ := f.get(t, 1, strings.ToLower(f.path(f.text, 2, 0, "Stream.vtt"))); status != 200 {
			t.Errorf("lowercase path: %d", status)
		}
		for name, path := range map[string]string{
			"no extension":          f.path(f.text, 2, 0, "Stream"),
			"unknown extension":     f.path(f.text, 2, 0, "Stream.txt"),
			"the video stream":      f.path(f.text, 0, 0, "Stream.vtt"),
			"the audio stream":      f.path(f.text, 1, 0, "Stream.vtt"),
			"a stream past the end": f.path(f.text, 9, 0, "Stream.vtt"),
			"a negative index":      f.path(f.text, -1, 0, "Stream.vtt"),
			"a negative start":      f.path(f.text, 2, -5, "Stream.vtt"),
			"a start that is text":  strings.Replace(f.path(f.text, 2, 0, "Stream.vtt"), "/2/0/", "/2/x/", 1),
		} {
			if status, _, _ := f.get(t, 0, path); status != 404 {
				t.Errorf("%s: %d, want 404", name, status)
			}
		}
		wrongSource := fmt.Sprintf("/Videos/%s/%s/Subtitles/2/0/Stream.vtt", db.DashedID(f.text), db.CompactID(f.image))
		if status, _, _ := f.get(t, 0, wrongSource); status != 404 {
			t.Errorf("another film's source: %d", status)
		}
		// Authorization as for the other media routes: header or query, else 401.
		bare := f.path(f.text, 2, 0, "Stream.vtt")
		if response, _ := f.do(t, 0, "GET", bare, "", nil); response.StatusCode != 401 {
			t.Errorf("no token: %d", response.StatusCode)
		}
		if response, _ := f.do(t, 0, "GET", bare, f.token, nil); response.StatusCode != 200 {
			t.Errorf("token in a header: %d", response.StatusCode)
		}
		if response, _ := f.do(t, 0, "GET", bare+"?ApiKey=nope", "", nil); response.StatusCode != 401 {
			t.Errorf("a wrong token: %d", response.StatusCode)
		}
	})
}

// Whatever the number of requests, a track is read out of its file once, by
// one ffmpeg, across replicas; the second request reads the cache.
func TestSubtitleExtractionRunsOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, f *subtitleFixture) {
		const requests = 16
		bodies := make([]string, requests)
		var wg sync.WaitGroup
		for i := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				status, _, body := f.get(t, i%2, f.path(f.text, 3, 0, "Stream.ass"))
				if status != 200 {
					body = fmt.Sprint("status ", status)
				}
				bodies[i] = body
			}()
		}
		wg.Wait()
		for i, body := range bodies {
			if body != bodies[0] || !strings.Contains(body, "ASS styled line one") {
				t.Fatalf("request %d: %q", i, body)
			}
		}
		if runs := f.runs(); runs != 1 {
			t.Fatalf("%d concurrent requests ran ffmpeg %d times, want 1", requests, runs)
		}
		// Another format, and the other replica, come from the same cache.
		if status, _, _ := f.get(t, 1, f.path(f.text, 3, 0, "Stream.vtt")); status != 200 {
			t.Fatal("vtt after ass failed")
		}
		if runs := f.runs(); runs != 1 {
			t.Fatalf("a second request ran ffmpeg: %d runs", runs)
		}
		// A different track is a different extraction.
		f.get(t, 0, f.path(f.text, 2, 0, "Stream.vtt"))
		if runs := f.runs(); runs != 2 {
			t.Fatalf("a second track: %d runs, want 2", runs)
		}
		// A changed file has another ETag, and so fresh text.
		mustExec(t, f.pool, "UPDATE media_sources SET object_etag = 'changed' WHERE item_id = $1", f.text)
		f.get(t, 0, f.path(f.text, 3, 0, "Stream.ass"))
		if runs := f.runs(); runs != 3 {
			t.Fatalf("a changed file: %d runs, want 3", runs)
		}
	})
}

// A track that can't be read ends in a clear error, logged, at once, and the
// failure doesn't leave the next request waiting on it.
func TestSubtitleFailuresDoNotHang(t *testing.T) {
	f := subtitleEnv(t, "")
	media, err := store.Open(f.textStore)
	if err != nil {
		t.Fatal(err)
	}
	for name, damage := range map[string]func(){
		"the file is garbage": func() { media.Put(context.Background(), "film.mkv", strings.NewReader("not a film")) }, //nolint:errcheck
		"the file is gone":    func() { media.Delete(context.Background(), "film.mkv") },                               //nolint:errcheck
	} {
		damage()
		mustExec(t, f.pool, "UPDATE media_sources SET object_etag = $1 WHERE item_id = $2", name, f.text)
		for range 2 { // The second finds the lock free again.
			began := time.Now()
			status, _, body := f.get(t, 0, f.path(f.text, 2, 0, "Stream.vtt"))
			if status != 500 || !strings.Contains(body, "could not be read") || time.Since(began) > 10*time.Second {
				t.Fatalf("%s: %d %q after %s", name, status, body, time.Since(began))
			}
		}
	}
	if !strings.Contains(f.log.String(), "subtitle extraction failed") {
		t.Errorf("not logged:\n%s", f.log)
	}
}

func subtitleTracks(source map[string]any) map[int]map[string]any {
	tracks := map[int]map[string]any{}
	for _, entry := range source["MediaStreams"].([]any) {
		if stream := entry.(map[string]any); stream["Type"] == "Subtitle" {
			tracks[int(stream["Index"].(float64))] = stream
		}
	}
	return tracks
}

func (f *subtitleFixture) enableTranscoding(t *testing.T) {
	t.Helper()
	mustExec(t, f.pool, "TRUNCATE transcode_jobs, transcode_nodes")
	mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, slots) VALUES ('w1', 2)")
	segments := store.NewPosix(t.TempDir())
	for _, s := range f.apis {
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments}
	}
}

// Text tracks are listed in the item and in PlaybackInfo as the capture shows,
// and image tracks are listed nowhere.
func TestSubtitlesAreListedAsCaptured(t *testing.T) {
	f := subtitleEnv(t, "")
	f.enableTranscoding(t)

	response, raw := f.do(t, 0, "GET", "/Items/"+f.text, f.token, nil)
	var item struct{ MediaStreams []map[string]any }
	if err := json.Unmarshal(raw, &item); err != nil || response.StatusCode != 200 {
		t.Fatalf("item: %d %s", response.StatusCode, raw)
	}
	listed := map[string]bool{}
	for _, s := range item.MediaStreams {
		listed[fmt.Sprint(s["Type"], s["Codec"])] = true
		if s["Type"] == "Subtitle" && (s["IsTextSubtitleStream"] != true || s["SupportsExternalStream"] != true || s["DeliveryUrl"] != nil) {
			t.Errorf("item subtitle: %v", s)
		}
	}
	if !listed["Subtitlesubrip"] || !listed["Subtitleass"] || len(item.MediaStreams) != 4 {
		t.Errorf("item streams: %v", item.MediaStreams)
	}
	response, raw = f.do(t, 0, "GET", "/Items/"+f.image, f.token, nil)
	if err := json.Unmarshal(raw, &item); err != nil || response.StatusCode != 200 {
		t.Fatalf("image item: %d %s", response.StatusCode, raw)
	}
	for _, s := range item.MediaStreams {
		if s["Type"] == "Subtitle" {
			t.Errorf("an image track is listed: %v", s)
		}
	}

	profile := map[string]any{}
	if err := json.Unmarshal(capturedProfile(t), &profile); err != nil {
		t.Fatal(err)
	}
	info := func(item string, profile map[string]any, extra map[string]any) map[string]any {
		body := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": db.CompactID(item)}
		for k, v := range extra {
			body[k] = v
		}
		return firstSource(f.playbackInfo(t, 0, item, body))
	}
	checkExternal := func(name string, source map[string]any) {
		t.Helper()
		tracks := subtitleTracks(source)
		srt, ass := tracks[2], tracks[3]
		wantSRT := "/Videos/" + db.DashedID(f.text) + "/" + db.CompactID(f.text) + "/Subtitles/2/0/Stream.vtt?ApiKey=" + f.token
		wantASS := strings.Replace(wantSRT, "/2/0/Stream.vtt", "/3/0/Stream.ass", 1)
		if len(tracks) != 2 || srt["DeliveryMethod"] != "External" || srt["DeliveryUrl"] != wantSRT || srt["IsExternalUrl"] != false ||
			srt["IsTextSubtitleStream"] != true || srt["SupportsExternalStream"] != true ||
			ass["DeliveryMethod"] != "External" || ass["DeliveryUrl"] != wantASS {
			t.Errorf("%s: %v", name, tracks)
		}
		for index, track := range tracks { // And what is offered can be fetched.
			if status, _, _ := f.get(t, 1, strings.SplitN(track["DeliveryUrl"].(string), "?", 2)[0]); status != 200 {
				t.Errorf("%s: track %d: %d", name, index, status)
			}
		}
	}

	// Direct play, and a transcode, hand the tracks to the client alike, and a
	// transcode is not told about them.
	direct := info(f.text, profile, nil)
	if direct["TranscodingUrl"] != nil {
		t.Fatalf("expected direct play: %v", direct)
	}
	checkExternal("direct play", direct)
	capped := info(f.text, profile, map[string]any{"MaxStreamingBitrate": 1000, "SubtitleStreamIndex": 2})
	if capped["TranscodingUrl"] == nil || strings.Contains(capped["TranscodingUrl"].(string), "SubtitleStreamIndex") {
		t.Fatalf("a client-rendered subtitle reached the transcode: %v", capped["TranscodingUrl"])
	}
	checkExternal("transcode", capped)
	if capped["DefaultSubtitleStreamIndex"] != float64(2) {
		t.Errorf("the chosen track isn't echoed: %v", capped["DefaultSubtitleStreamIndex"])
	}
	// The image film plays as before: no track, nothing else changed.
	if image := info(f.image, profile, nil); len(subtitleTracks(image)) != 0 || image["SupportsDirectPlay"] != true {
		t.Errorf("image film: %v", image)
	}

	// Burn-in: a profile with no subtitle formats, or the viewer's setting.
	none := map[string]any{}
	for k, v := range profile {
		none[k] = v
	}
	none["SubtitleProfiles"] = []any{}
	checkEncode := func(name string, source map[string]any, chosen string) {
		t.Helper()
		tracks := subtitleTracks(source)
		for index, track := range tracks {
			if track["DeliveryMethod"] != "Encode" || track["DeliveryUrl"] != nil || track["IsTextSubtitleStream"] != true {
				t.Errorf("%s: track %d: %v", name, index, track)
			}
		}
		transcodingURL, _ := source["TranscodingUrl"].(string)
		query := mustQuery(t, transcodingURL)
		if len(tracks) != 2 || query.Get("SubtitleStreamIndex") != chosen {
			t.Errorf("%s: %d tracks, TranscodingUrl %s", name, len(tracks), transcodingURL)
		}
		if chosen != "" && !strings.Contains(query.Get("TranscodeReasons"), "SubtitleCodecNotSupported") {
			t.Errorf("%s: reasons %q", name, query.Get("TranscodeReasons"))
		}
	}
	burnt := info(f.text, none, map[string]any{"SubtitleStreamIndex": 2})
	checkEncode("empty SubtitleProfiles", burnt, "2")
	if burnt["SupportsDirectPlay"] != false || burnt["TranscodingUrl"] == nil {
		t.Errorf("burn-in needs a transcode: %v", burnt)
	}
	// Choosing another track, or none, is another spec: a new job, as an audio switch is.
	other := info(f.text, none, map[string]any{"SubtitleStreamIndex": "3"})
	checkEncode("the other track", other, "3")
	if mustQuery(t, burnt["TranscodingUrl"].(string)).Get("PlaySessionId") == mustQuery(t, other["TranscodingUrl"].(string)).Get("PlaySessionId") {
		t.Error("PlaySessionId repeated")
	}
	off := info(f.text, none, nil)
	if off["TranscodingUrl"] != nil || len(subtitleTracks(off)) != 2 || subtitleTracks(off)[2]["DeliveryMethod"] != "Encode" {
		t.Errorf("subtitles off while burning: %v", off)
	}
	// The setting alone burns only when the film is being transcoded anyway.
	always := map[string]any{"AlwaysBurnInSubtitleWhenTranscoding": true, "SubtitleStreamIndex": 2}
	checkExternal("always burn, direct play", info(f.text, profile, always))
	always["MaxStreamingBitrate"] = 1000
	checkEncode("always burn, transcode", info(f.text, profile, always), "2")
	// Without transcoding nothing can be drawn, so no track is offered.
	for _, s := range f.apis {
		s.Transcode = nil
	}
	if source := info(f.text, none, map[string]any{"SubtitleStreamIndex": 2}); len(subtitleTracks(source)) != 0 {
		t.Errorf("tracks offered that can't be delivered: %v", subtitleTracks(source))
	}
	// A transcode store with no worker behind it can't draw either, and the
	// default plays on without a menu of dead choices.
	f.enableTranscoding(t)
	mustExec(t, f.pool, "TRUNCATE transcode_nodes")
	if source := info(f.text, none, map[string]any{"SubtitleStreamIndex": 2}); len(subtitleTracks(source)) != 0 || source["TranscodingUrl"] != nil {
		t.Errorf("burn-in tracks offered with no worker: %v", subtitleTracks(source))
	}
	// A request for a track the film hasn't is ignored.
	f.enableTranscoding(t)
	if source := info(f.text, none, map[string]any{"SubtitleStreamIndex": 9}); source["TranscodingUrl"] != nil {
		t.Errorf("an unknown track forced a transcode: %v", source["TranscodingUrl"])
	}
}

// slowFFmpeg puts an ffmpeg in front of the others that waits until release is
// called, standing in for a track in a very large file. started says whether
// one has begun.
func slowFFmpeg(t *testing.T) (started func() bool, release func()) {
	t.Helper()
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	begun, proceed := filepath.Join(dir, "begun"), filepath.Join(dir, "go")
	// sleep's output goes nowhere, so one left behind by a killed shim doesn't
	// hold the pipe ffmpeg's caller reads.
	script := fmt.Sprintf("#!/bin/sh\ntouch %q\nwhile [ ! -e %q ]; do sleep 0.1 >/dev/null 2>&1; done\nexec %q \"$@\"\n", begun, proceed, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	started = func() bool { _, err := os.Stat(begun); return err == nil }
	release = func() { os.WriteFile(proceed, nil, 0o644) } //nolint:errcheck
	return started, release
}

// A slow extraction must not take a connection from the pool: a replica whose
// pool is used up by extractions can't serve anyone else.
func TestSlowExtractionLeavesThePoolFree(t *testing.T) {
	f := subtitleEnv(t, "")
	config, err := pgxpool.ParseConfig(os.Getenv("JELLYSCALE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 1
	tiny, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tiny.Close)
	f.apis[0].Pool, f.apis[0].Items = tiny, &items.Repo{Pool: tiny, ServerID: "srv"}
	started, release := slowFFmpeg(t)
	t.Cleanup(release)

	extracted := make(chan int, 1)
	go func() {
		status, _, _ := f.get(t, 0, f.path(f.text, 2, 0, "Stream.vtt"))
		extracted <- status
	}()
	waitUntil(t, "the extraction to start", started)
	answered := make(chan int, 1)
	go func() {
		response, _ := f.do(t, 0, "GET", "/Items/"+f.text, f.token, nil)
		answered <- response.StatusCode
	}()
	select {
	case status := <-answered:
		if status != 200 {
			t.Errorf("item: %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request waited on the pool while a subtitle track was being extracted")
	}
	release()
	if status := <-extracted; status != 200 {
		t.Errorf("extraction: %d", status)
	}
}

// An extraction that outlasts its limit says so, and caches nothing.
func TestSlowExtractionNamesItsLimit(t *testing.T) {
	f := subtitleEnv(t, "")
	started, _ := slowFFmpeg(t)
	source, _, err := f.apis[0].Items.Source(context.Background(), f.text, db.CompactID(f.text))
	if err != nil {
		t.Fatal(err)
	}
	media, err := store.Open(source.StoreURI)
	if err != nil {
		t.Fatal(err)
	}
	cache := subtitles.Cache{Pool: f.pool, Timeout: time.Second}
	began := time.Now()
	_, err = cache.Track(context.Background(), source, media, media, 2)
	if err == nil || !strings.Contains(err.Error(), "took longer than 1s") || time.Since(began) > 10*time.Second || !started() {
		t.Fatalf("after %s: %v", time.Since(began), err)
	}
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if done() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Many requests for one uncached track share one extraction and one
// connection, so a default Postgres, which allows 100, is not exhausted.
func TestManyRequestsForOneTrackAllAnswer(t *testing.T) {
	f := subtitleEnv(t, "")
	const requests = 150
	statuses := make([]int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = f.get(t, i%2, f.path(f.text, 2, 0, "Stream.vtt"))
		}()
	}
	wg.Wait()
	for i, status := range statuses {
		if status != 200 {
			t.Fatalf("request %d: %d", i, status)
		}
	}
	if runs := f.runs(); runs != 1 {
		t.Errorf("%d runs of ffmpeg, want 1", runs)
	}
}

// A burst of distinct tracks runs at most four ffmpegs at once, and all finish.
func TestDistinctTracksRunFourAtATime(t *testing.T) {
	const tracks = 20
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	// The film is Text's, with 20 SubRip tracks: streams 2 to 21.
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte(testSRT), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3"}
	for range tracks {
		args = append(args, "-i", srt)
	}
	args = append(args, "-map", "0", "-map", "1")
	for i := range tracks {
		args = append(args, "-map", fmt.Sprint(i+2))
	}
	film := filepath.Join(dir, "film.mkv")
	ffmpegRun(t, append(args, "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-c:s", "srt", film)...)
	data, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	f := &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}
	ctx := context.Background()
	media, assets := "file://"+t.TempDir(), "file://"+t.TempDir()
	files, err := store.Open(media)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Put(ctx, "film.mkv", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	libs, err := library.Sync(ctx, f.pool, []library.Config{{Name: "Many", Kind: "movies", Media: media, Assets: assets}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&library.Scanner{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}).Scan(ctx, libs[0]); err != nil {
		t.Fatal(err)
	}
	item := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE library_id = $1 AND type = 'Movie'", libs[0].ID)

	// Each ffmpeg notes its start and end; it waits first so that runs overlap.
	real, _ := exec.LookPath("ffmpeg")
	shim := t.TempDir()
	events := filepath.Join(shim, "events")
	script := fmt.Sprintf("#!/bin/sh\necho + >>%[1]q\nsleep 0.3 >/dev/null 2>&1\n%[2]q \"$@\"\nstatus=$?\necho - >>%[1]q\nexit $status\n", events, real)
	if err := os.WriteFile(filepath.Join(shim, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	statuses := make([]int, tracks)
	var wg sync.WaitGroup
	for i := range tracks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _, _ = f.get(t, i%2, f.path(item, i+2, 0, "Stream.vtt"))
		}()
	}
	wg.Wait()
	for i, status := range statuses {
		if status != 200 {
			t.Fatalf("track %d: %d", i+2, status)
		}
	}
	log, _ := os.ReadFile(events)
	running, most := 0, 0
	for _, c := range strings.Fields(string(log)) {
		if c == "+" {
			running++
		} else {
			running--
		}
		most = max(most, running)
	}
	if strings.Count(string(log), "+") != tracks || most != 4 {
		t.Errorf("%d runs, at most %d at once; want %d and 4", strings.Count(string(log), "+"), most, tracks)
	}
}
