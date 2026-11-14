//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"jellyscale/compat/conform"
	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-086: one typed media model from the probe to every reader. These checks
// pin what a client sees, so that moving the model from maps to types changes
// nothing a client can tell (a key gained with a zero value, a key lost, a
// stored-only field leaking), and that every field the schema requires of a
// source or a stream is in every answer.

// modelEnv scans one film with a video, two audio tracks, a SubRip track, a PGS
// track and a font, and a subtitle file beside it, with a worker running that
// takes the font out. The media is synthetic.
func modelEnv(t *testing.T) (*externalFixture, string) {
	t.Helper()
	testenv.FFmpeg(t)
	f := &externalFixture{subtitleFixture: &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}, scanLog: &syncBuffer{}}
	f.tools = traceTools(t)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	srt := write("a.srt", []byte(testSRT))
	sup := write("b.sup", testenv.PGS(1920, 1080, testenv.Cue{Start: 0.5, End: 2.5, X: 660, Y: 860, W: 600, H: 120}))
	font := write("Serif.ttf", []byte(fontBytes))
	film := filepath.Join(dir, "film.mkv")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=3", "-i", srt, "-fix_sub_duration", "-i", sup,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3", "-map", "4", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "flac",
		"-c:s:0", "srt", "-c:s:1", "copy", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=jpn",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=eng", "-attach", font, "-metadata:s:t:0", "mimetype=font/ttf", film)
	data, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	f.assetDir = t.TempDir()
	mediaDir := t.TempDir()
	media, assets := "file://"+mediaDir, "file://"+f.assetDir
	if f.media, err = store.Open(media); err != nil {
		t.Fatal(err)
	}
	if f.assets, err = store.Open(assets); err != nil {
		t.Fatal(err)
	}
	f.put(t, "film.mkv", string(data))
	f.put(t, "film.en.srt", srtCue("beside the film"))
	libs, err := library.Sync(context.Background(), f.pool, []library.Config{{Name: "Model", Kind: "movies", Media: media, Assets: assets}})
	if err != nil {
		t.Fatal(err)
	}
	f.lib = libs[0]
	f.scanner = &library.Scanner{Pool: f.pool, Log: slog.New(slog.NewTextHandler(f.scanLog, nil))}
	f.scan(t)
	f.text = scalar[string](t, f.pool, "SELECT id::text FROM items WHERE library_id = $1 AND type = 'Movie'", f.lib.ID)
	f.startWorker(t, 0)
	f.enableTranscoding(t)
	f.waitFontsAfterPlaybackInfo(t)
	return f, mediaDir
}

// waitFontsAfterPlaybackInfo asks for the film's playback, which has a worker
// take the font out, and waits until the details list it.
func (f *externalFixture) waitFontsAfterPlaybackInfo(t *testing.T) {
	t.Helper()
	f.playbackInfo(t, 0, f.text, map[string]any{"DeviceProfile": map[string]any{}, "MediaSourceId": db.CompactID(f.text)})
	f.waitFonts(t)
}

// modelAnswers asks for every kind of answer that carries a source or a stream,
// returning each as the route it is an answer to and its body.
type modelAnswer struct {
	name, method, route string // route as the schema writes it
	body                []byte
}

func (f *externalFixture) modelAnswers(t *testing.T) []modelAnswer {
	t.Helper()
	id, user := db.CompactID(f.text), db.CompactID(f.user)
	var profile map[string]any
	if err := json.Unmarshal(capturedProfile(t), &profile); err != nil {
		t.Fatal(err)
	}
	get := func(path string) []byte {
		t.Helper()
		response, body := f.do(t, 0, "GET", path, f.token, nil)
		if response.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, response.StatusCode, body)
		}
		return body
	}
	post := func(path string, body map[string]any) []byte {
		t.Helper()
		response, raw := f.do(t, 0, "POST", path, f.token, body, "Authorization", strings.Replace(auth, "%s", f.token, 1))
		if response.StatusCode != 200 {
			t.Fatalf("POST %s: %d %s", path, response.StatusCode, raw)
		}
		return raw
	}
	// Direct play: the Chrome profile of the capture takes this film's H.264.
	direct := post("/Items/"+id+"/PlaybackInfo", map[string]any{"DeviceProfile": profile, "MediaSourceId": id, "UserId": f.user,
		"AudioStreamIndex": 1, "SubtitleStreamIndex": -1})
	// A transcoding profile: it direct plays only WebM, so the offer is an HLS transcode of this Matroska film,
	// which lists every audio track of the full probe.
	var transcoding map[string]any
	if err := json.Unmarshal(capturedProfile(t), &transcoding); err != nil {
		t.Fatal(err)
	}
	transcoding["DirectPlayProfiles"] = []map[string]any{{"Container": "webm", "Type": "Video", "VideoCodec": "vp9", "AudioCodec": "opus"}}
	transcode := post("/Items/"+id+"/PlaybackInfo", map[string]any{"DeviceProfile": transcoding, "MediaSourceId": id, "UserId": f.user,
		"AudioStreamIndex": 1, "SubtitleStreamIndex": -1})
	f.report(t, 0, "", f.text, 0)
	return []modelAnswer{
		{"an item's details", "GET", "/Items/{itemId}", get("/Items/" + id + "?userId=" + user)},
		{"a listing asked for MediaSources and MediaStreams", "GET", "/Items", get("/Items?userId=" + user + "&Recursive=true&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams")},
		{"a session's NowPlayingItem", "GET", "/Sessions", get("/Sessions")},
		{"PlaybackInfo as direct play", "POST", "/Items/{itemId}/PlaybackInfo", direct},
		{"PlaybackInfo with a transcoding profile", "POST", "/Items/{itemId}/PlaybackInfo", transcode},
	}
}

// mediaSubtrees returns the sources, the item's own streams and its attachments
// of an answer, wherever the answer keeps them.
func mediaSubtrees(t *testing.T, name string, body []byte) map[string]any {
	t.Helper()
	var reply any
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string]any{}
	var holder map[string]any
	switch v := reply.(type) {
	case map[string]any:
		holder = v
		if items, ok := v["Items"].([]any); ok && len(items) > 0 {
			holder = items[0].(map[string]any)
		}
	case []any:
		session, _ := v[0].(map[string]any)
		holder, _ = session["NowPlayingItem"].(map[string]any)
	}
	for _, key := range []string{"MediaSources", "MediaStreams", "MediaAttachments"} {
		if v, ok := holder[key]; ok {
			out[key] = v
		}
	}
	return out
}

// volatile fields differ from one run to the next with the bytes ffmpeg
// writes and the directories the test makes; the golden keeps their type only.
var volatile = map[string]bool{"Size": true, "ETag": true, "Bitrate": true, "BitRate": true, "RunTimeTicks": true, "DurationTicks": true}

// lines lists each leaf of v as "path = value", keys in order, so two answers
// with the same keys and values give the same lines whatever order the keys
// were written in. The key order inside an object is not a client's to read;
// the order of a list is.
func lines(v any, path string, scrub *strings.Replacer, out *[]string) {
	switch v := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			*out = append(*out, path+" = {}")
		}
		for _, k := range keys {
			if volatile[k] {
				*out = append(*out, fmt.Sprintf("%s.%s = <%T>", path, k, v[k]))
				continue
			}
			lines(v[k], path+"."+k, scrub, out)
		}
	case []any:
		if len(v) == 0 {
			*out = append(*out, path+" = []")
		}
		for i, e := range v {
			lines(e, path+"["+strconv.Itoa(i)+"]", scrub, out)
		}
	case string:
		*out = append(*out, fmt.Sprintf("%s = %q", path, scrub.Replace(v)))
	default:
		raw, _ := json.Marshal(v)
		*out = append(*out, fmt.Sprintf("%s = %s", path, raw))
	}
}

var runIDs = regexp.MustCompile(`PlaySessionId=[0-9a-f]+|Tag=[0-9a-f-]+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// The sources, streams and attachments a film with a video, two audio tracks,
// a text, a PGS and an external subtitle track and a font is answered with have
// the same keys and values, in the same order, as before JS-086
// (testdata/media-model-before.txt, recorded from the build before it): no key
// gained with a zero value, none lost, no stored-only field. A difference is
// allowed only where the new value is what Jellyfin 12.4 answers, and then the
// golden is changed with the evidence beside it. JELLYSCALE_RECORD_GOLDEN=1
// rewrites it.
func TestAnswersCarryTheSameSourcesStreamsAndAttachmentsAsBefore(t *testing.T) {
	f, mediaDir := modelEnv(t)
	scrub := strings.NewReplacer(f.token, "<token>", db.CompactID(f.text), "<item>", db.DashedID(f.text), "<item>",
		db.CompactID(f.user), "<user>", mediaDir, "<dir>")
	var got []string
	for _, answer := range f.modelAnswers(t) {
		got = append(got, "## "+answer.name)
		for _, key := range []string{"MediaSources", "MediaStreams", "MediaAttachments"} {
			sub, ok := mediaSubtrees(t, answer.name, answer.body)[key]
			if !ok {
				got = append(got, key+" absent")
				continue
			}
			var part []string
			lines(sub, key, scrub, &part)
			for _, line := range part {
				got = append(got, runIDs.ReplaceAllStringFunc(line, func(s string) string {
					if strings.HasPrefix(s, "PlaySessionId=") || strings.HasPrefix(s, "Tag=") {
						return s[:strings.Index(s, "=")+1] + "<id>"
					}
					return "<uuid>"
				}))
			}
		}
	}
	text := strings.Join(got, "\n") + "\n"
	golden := filepath.Join("testdata", "media-model-before.txt")
	if os.Getenv("JELLYSCALE_RECORD_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) == text {
		return
	}
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(text, "\n")
	seen := map[string]int{}
	for _, l := range wantLines {
		seen[l]++
	}
	for _, l := range gotLines {
		seen[l]--
	}
	for l, n := range seen {
		switch {
		case n > 0:
			t.Errorf("lost: %s", l)
		case n < 0:
			t.Errorf("gained: %s", l)
		}
	}
	if !t.Failed() {
		t.Error("the same lines in another order")
	}
}

// Every field the schema requires of a source, a stream and an attachment is in
// each answer, non-null and valid for its type, on every element: each
// source's streams, the item's own list, the subtitle file beside the film, and
// the audio streams a transcode offer adds from the full probe.
func TestEveryAnswerHoldsEverySourceAndStreamFieldTheSchemaRequires(t *testing.T) {
	f, _ := modelEnv(t)
	raw, err := os.ReadFile("../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := conform.ParseSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := conform.LoadRules("../../compat/conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The counts the criterion states, so a schema that changes under it is noticed.
	for typeName, want := range map[string]int{"MediaSourceInfo": 17, "MediaStream": 10, "MediaAttachment": 1} {
		n := 0
		for _, field := range schema.Types[typeName].Required {
			if schema.Requires(typeName, field) {
				n++
			}
		}
		if n != want {
			t.Fatalf("the schema requires %d fields of %s, the criteria say %d", n, typeName, want)
		}
	}
	checker := conform.NewChecker(schema, rules)
	for _, answer := range f.modelAnswers(t) {
		violations := checker.Check(answer.name, answer.method, strings.Replace(answer.route, "{itemId}", db.CompactID(f.text), 1), 200, "application/json", answer.body)
		for _, v := range violations {
			// Only what concerns a source, a stream or an attachment: other fields are other tasks'.
			if strings.Contains(v.JSONPath, "Media") || strings.Contains(v.Field, "Media") {
				t.Errorf("%s", v)
			}
		}
		// Not only the first element: count what was held to the schema.
		sub := mediaSubtrees(t, answer.name, answer.body)
		streams := 0
		var count func(any)
		count = func(v any) {
			if list, ok := v.([]any); ok {
				for _, e := range list {
					if m, ok := e.(map[string]any); ok {
						if _, isStream := m["Type"]; isStream && m["Index"] != nil && m["Container"] == nil {
							streams++
						}
						count(m["MediaStreams"])
					}
				}
			}
		}
		count(sub["MediaSources"])
		count(sub["MediaStreams"])
		if answer.name == "PlaybackInfo with a transcoding profile" {
			// 1 video, 2 audio, 2 subtitle tracks and one file beside the film, in the source; and the same
			// again in the item's own list when it has one.
			if streams < 6 {
				t.Errorf("%s: only %d streams were checked, want the film's 6 at least", answer.name, streams)
			}
			if !strings.Contains(string(answer.body), `"TranscodingUrl"`) {
				t.Errorf("%s: the offer is not a transcode: %s", answer.name, answer.body)
			}
		}
	}
}

// A source stored by the release before this one and not yet probed again
// (its stored DTO lacks the eight source fields and the stream fields the
// fill-in used to give) is answered with every required field at a valid value
// before any scan has run: newEnv stores exactly such a DTO.
func TestASourceStoredByThePreviousReleaseIsAnsweredWithValidRequiredFields(t *testing.T) {
	e := newEnv(t)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaStreams}', '[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Audio","IsDefault":true}]')`)
	id := db.CompactID(e.item)
	// Two audio streams in the stored sources of the probe, as the transcode offer adds from the full probe.
	mustExec(t, e.pool, `UPDATE media_sources SET streams = '[{"Index":0,"Type":"Video","Codec":"h264"},{"Index":1,"Type":"Audio","Codec":"aac"},{"Index":2,"Type":"Audio","Codec":"aac"}]'`)
	raw, err := os.ReadFile("../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := conform.ParseSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := conform.LoadRules("../../compat/conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checker := conform.NewChecker(schema, rules)
	_, details := e.do(t, 0, "GET", "/Items/"+id+"?userId="+db.CompactID(e.user), e.token, nil)
	_, info := e.do(t, 0, "POST", "/Items/"+id+"/PlaybackInfo", e.token, map[string]any{"UserId": e.user, "DeviceProfile": map[string]any{}})
	for what, c := range map[string]struct {
		method, route string
		body          []byte
	}{"details": {"GET", "/Items/" + id, details}, "PlaybackInfo": {"POST", "/Items/" + id + "/PlaybackInfo", info}} {
		for _, v := range checker.Check(what, c.method, c.route, 200, "application/json", c.body) {
			if strings.Contains(v.JSONPath, "Media") {
				t.Errorf("%s", v)
			}
		}
		var reply struct{ MediaSources []map[string]any }
		if err := json.Unmarshal(c.body, &reply); err != nil || len(reply.MediaSources) == 0 {
			t.Fatalf("%s: %v %s", what, err, c.body)
		}
		for _, source := range reply.MediaSources {
			// The values a file played from disk has, never an empty string for an enum.
			for key, want := range map[string]any{"Protocol": "File", "Type": "Default", "SupportsProbing": true, "TranscodingSubProtocol": "http"} {
				if source[key] != want {
					t.Errorf("%s: source %s is %v, want %v", what, key, source[key], want)
				}
			}
		}
	}
}
