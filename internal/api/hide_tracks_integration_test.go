//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-119: a viewer who opted in (JS-118) is listed the audio and subtitle tracks outside their languages as a
// placeholder of Type Data at the track's own Index, in PlaybackInfo and in one item's details, and no index moves.
// What a real Jellyfin 12.4 lists for a data track, and what jellyfin-web does with one, is
// compat/captures/data-stream-12.4.md. These checks use the two fixtures of test/fixtures (babel.sh, kino.sh) scanned
// by the real scanner, with a real worker, and films made as rows (the original language checks' addFilm).
//
// The tracks of Babel Film: 0 video; audio 1 eng (default), 2 deu, 3 fra, 4 untagged; subtitles 5 eng, 6 eng forced,
// 7 eng SDH, 8 deu, 9 ger forced, 10 fra, 11 untagged, 12 jpn PGS; files beside it 13 (.de.srt) and 14 (.es.srt).
// Kino Film: 0 video; audio 1 deu (default), 2 eng; subtitles 3 deu, 4 eng, 5 fra.

var (
	babelTypes = append(append([]string{"Video"}, strings.Fields("Audio Audio Audio Audio")...), strings.Fields("Subtitle Subtitle Subtitle Subtitle Subtitle Subtitle Subtitle Subtitle Subtitle Subtitle")...)
	kinoTypes  = strings.Fields("Video Audio Audio Subtitle Subtitle Subtitle")
)

type langFixture struct {
	*subtitleFixture
	babel, kino string // Item ids.
	babelSource string
	profile     json.RawMessage
	stop        func() // Stops the worker.
	segments    store.Store
}

type viewer struct{ id, token, name string }

// languagesEnv scans Babel Film and Kino Film and runs a worker that makes subtitles and segments. transcoding false
// leaves the replicas with no transcode store, as one that cannot offer a transcode.
func languagesEnv(t *testing.T, transcoding bool) *langFixture {
	t.Helper()
	testenv.FFmpeg(t)
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	films := t.TempDir()
	for _, recipe := range []string{"babel.sh", "kino.sh"} {
		if out, err := exec.Command("bash", filepath.Join(repo, "test", "fixtures", recipe), films).CombinedOutput(); err != nil {
			t.Fatalf("test/fixtures/%s: %v\n%s", recipe, err, out)
		}
	}
	f := &langFixture{subtitleFixture: &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}, profile: capturedProfile(t)}
	ctx := context.Background()
	libs, err := library.Sync(ctx, f.pool, []library.Config{{Name: "Languages", Kind: "movies",
		Media: "file://" + filepath.Join(films, "languages"), Assets: "file://" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&library.Scanner{Pool: f.pool, Log: slog.New(slog.DiscardHandler)}).Scan(ctx, libs[0]); err != nil {
		t.Fatal(err)
	}
	f.babel = scalar[string](t, f.pool, "SELECT id::text FROM items WHERE type = 'Movie' AND name = 'Babel Film'")
	f.kino = scalar[string](t, f.pool, "SELECT id::text FROM items WHERE type = 'Movie' AND name = 'Kino Film'")
	f.babelSource = scalar[string](t, f.pool, "SELECT replace(id::text, '-', '') FROM media_sources WHERE item_id = $1", f.babel)
	for _, s := range f.apis {
		s.Log = slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if transcoding {
		f.startWorkers(t)
	}
	return f
}

// startWorkers runs one worker that makes segments and subtitles, and gives the replicas a transcode store.
func (f *langFixture) startWorkers(t *testing.T) {
	t.Helper()
	mustExec(t, f.pool, "TRUNCATE transcode_jobs, transcode_nodes, subtitle_jobs")
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	f.segments = store.NewPosix(t.TempDir())
	var media store.Cache
	log := slog.New(slog.NewTextHandler(f.log, nil))
	w := &transcode.Worker{ID: "worker-" + uuid.NewString()[:8], Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: f.segments, Log: log,
		Media: media.Open, Scratch: t.TempDir(), Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	w.Hub = &transcode.Hub{Pool: pool, Log: log}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, run := range []func(){func() { w.Hub.Run(ctx) }, func() { w.Present(ctx, "worker", "", 200*time.Millisecond) }, func() { w.Run(ctx) }, func() { w.RunSubtitles(ctx) }} {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	var once sync.Once
	f.stop = func() { once.Do(func() { cancel(); wg.Wait(); pool.Close() }) }
	t.Cleanup(f.stop)
	for _, s := range f.apis {
		hub := &transcode.Hub{Pool: s.Pool, Log: log}
		go hub.Run(ctx)
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: f.segments, Hub: hub, QueueTimeout: 10 * time.Second, WaitTimeout: 60 * time.Second}
	}
	waitUntil(t, "the worker to register", func() bool {
		return scalar[int](t, f.pool, "SELECT count(*)::int FROM transcode_nodes WHERE roles = 'worker'") == 1
	})
}

// viewer makes a user who has saved the languages (empty: none) and the track preferences (a PATCH body, empty: none).
func (f *langFixture) viewer(t *testing.T, name, audio, subtitle, prefs string) viewer {
	t.Helper()
	id, token := f.addUser(t, name)
	v := viewer{id, token, name}
	f.saveLanguages(t, 0, v, audio, subtitle)
	if prefs != "" {
		f.patch(t, 0, v, prefs)
	}
	return v
}

// saveLanguages posts the Configuration with the languages named; one left empty is left out, as jellyfin-web posts a
// viewer who chose none, and a POST replaces the whole object.
func (f *langFixture) saveLanguages(t *testing.T, replica int, v viewer, audio, subtitle string) {
	t.Helper()
	body := map[string]any{}
	if audio != "" {
		body["AudioLanguagePreference"] = audio
	}
	if subtitle != "" {
		body["SubtitleLanguagePreference"] = subtitle
	}
	raw, _ := json.Marshal(body)
	if status := f.save(t, replica, "path", v.id, v.token, string(raw)).StatusCode; status != 204 {
		t.Fatalf("saving %s's languages %s: %d", v.name, raw, status)
	}
}

func (f *langFixture) patch(t *testing.T, replica int, v viewer, body string) {
	t.Helper()
	if response, raw := f.do(t, replica, "PATCH", "/jellyscale/v1/track-preferences", v.token, json.RawMessage(body)); response.StatusCode != 200 {
		t.Fatalf("%s: PATCH %s: %d %s", v.name, body, response.StatusCode, raw)
	}
}

func (f *langFixture) auth(v viewer) string {
	return fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="device-%s", Version="1", Token="%s"`, v.name, v.token)
}

// info is PlaybackInfo with the captured Chrome profile, so a transcode is offered when one can be.
func (f *langFixture) info(t *testing.T, replica int, v viewer, film string, extra map[string]any) doc {
	t.Helper()
	body := map[string]any{"MaxStreamingBitrate": 2147483647}
	if f.profile != nil { // Films made as rows have no profile: no transcode is offered, as for a client that sends none.
		body["DeviceProfile"] = f.profile
	}
	for k, val := range extra {
		body[k] = val
	}
	response, raw := f.do(t, replica, "POST", "/Items/"+film+"/PlaybackInfo", v.token, body, "Authorization", f.auth(v))
	if response.StatusCode != 200 {
		t.Fatalf("%s: PlaybackInfo %v: %d %s", v.name, extra, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// item is GET /Items/{id}, or its alias.
func (f *langFixture) item(t *testing.T, replica int, v viewer, film string, alias bool) doc {
	t.Helper()
	path := "/Items/" + db.CompactID(film) + "?userId=" + db.CompactID(v.id)
	if alias {
		path = "/Users/" + db.CompactID(v.id) + "/Items/" + db.CompactID(film)
	}
	response, raw := f.do(t, replica, "GET", path, v.token, nil, "Authorization", f.auth(v))
	if response.StatusCode != 200 {
		t.Fatalf("%s: GET %s: %d %s", v.name, path, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func sourceOf(reply doc) doc { return reply["MediaSources"].([]any)[0].(doc) }

func typesOfStreams(streams []doc) []string {
	var out []string
	for _, s := range streams {
		out = append(out, fmt.Sprint(s["Type"]))
	}
	return out
}

// dataAt is the Index of each stream of Type Data. In a PlaybackInfo list it is also the position; an item's list holds
// the default audio track alone, so there the two differ.
func dataAt(streams []doc) []int {
	out := []int{}
	for _, s := range streams {
		if s["Type"] == "Data" {
			out = append(out, int(s["Index"].(float64)))
		}
	}
	return out
}

// canon is a stream as text with the caller's token replaced, to compare two callers' answers.
func canon(v any, tokens ...string) string {
	raw, _ := json.Marshal(v)
	text := string(raw)
	for _, token := range tokens {
		text = strings.ReplaceAll(text, token, "<token>")
	}
	return text
}

// without is a copy of a document with the named keys removed.
func without(d doc, keys ...string) doc {
	out := doc{}
	for k, v := range d {
		if !slices.Contains(keys, k) {
			out[k] = v
		}
	}
	return out
}

// transcodingParams is a TranscodingUrl's path and query with the session, the device and the token left out.
func transcodingParams(t *testing.T, raw any, tokens ...string) string {
	t.Helper()
	text, _ := raw.(string)
	if text == "" {
		return ""
	}
	parsed, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	values := parsed.Query()
	for key := range values {
		if strings.EqualFold(key, "PlaySessionId") || strings.EqualFold(key, "ApiKey") || strings.EqualFold(key, "api_key") || strings.EqualFold(key, "DeviceId") {
			values.Del(key)
		}
	}
	return canon(parsed.Path+"?"+values.Encode(), tokens...)
}

// captureDataStream is the data stream of compat/captures/data-stream-12.4.json, as Jellyfin 12.4 lists it.
func captureDataStream(t *testing.T) doc {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/data-stream-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture doc
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	for _, s := range streamsOf(t, sourceOf(capture["PlaybackInfo POST"].(map[string]any))["MediaStreams"]) {
		if s["Type"] == "Data" {
			return s
		}
	}
	t.Fatal("the capture has no data stream")
	return nil
}

// checkPlaceholders holds every stream of Type Data in a list to the capture's shape and to saying nothing of the
// track it stands for: any two differ only in Index.
func checkPlaceholders(t *testing.T, label string, streams []doc, capture doc) {
	t.Helper()
	identifying := []string{"Language", "CodecTag", "TimeBase", "BitRate"} // Fields of the capture that come from the track.
	var first doc
	for _, s := range streams {
		if s["Type"] != "Data" {
			continue
		}
		for key, value := range s {
			if _, ok := capture[key]; !ok || key == "Language" {
				t.Errorf("%s: the placeholder at %v has %s=%v, which the capture's data stream does not have (or must not carry)", label, s["Index"], key, value)
			}
			if !slices.Contains(identifying, key) && key != "Index" && !reflect.DeepEqual(value, capture[key]) {
				t.Errorf("%s: the placeholder at %v has %s=%v, the capture has %v", label, s["Index"], key, value, capture[key])
			}
		}
		for key := range capture {
			if _, ok := s[key]; !ok && !slices.Contains(identifying, key) {
				t.Errorf("%s: the placeholder at %v lacks %s, which the capture's data stream has", label, s["Index"], key)
			}
		}
		for _, key := range []string{"Title", "DisplayTitle", "Codec", "DeliveryUrl", "DeliveryMethod", "Path", "Channels", "IsExternalUrl"} {
			if _, ok := s[key]; ok {
				t.Errorf("%s: the placeholder at %v has %s", label, s["Index"], key)
			}
		}
		if first == nil {
			first = s
		} else if a, b := canon(without(first, "Index")), canon(without(s, "Index")); a != b {
			t.Errorf("%s: the placeholders at %v and %v differ in more than Index:\n%s\n%s", label, first["Index"], s["Index"], a, b)
		}
	}
}

// checkDefaults holds DefaultAudioStreamIndex and DefaultSubtitleStreamIndex to naming a stream listed as its kind at
// that same position, or being absent or -1.
func checkDefaults(t *testing.T, label string, source doc) {
	t.Helper()
	streams := streamsOf(t, source["MediaStreams"])
	for key, kind := range map[string]string{"DefaultAudioStreamIndex": "Audio", "DefaultSubtitleStreamIndex": "Subtitle"} {
		value, present := source[key]
		if !present || value == nil {
			continue
		}
		index := int(value.(float64))
		if index == -1 {
			continue
		}
		found := false
		for _, s := range streams {
			if int(s["Index"].(float64)) == index {
				found = true
				if s["Type"] != kind {
					t.Errorf("%s: %s = %d, but the stream with that Index is %v", label, key, index, s["Type"])
				}
			}
		}
		if !found {
			t.Errorf("%s: %s = %d names no listed stream", label, key, index)
		}
	}
}

// checkAs holds an opted-in viewer's source to the baseline source (what a viewer with no settings gets for the same
// request): the same streams at the same positions with Data exactly at the want indexes (those of them that the
// baseline lists), every other stream equal field for field, and everything else of the source and of the reply equal.
func checkAs(t *testing.T, label string, got, baseline doc, data []int, tokens, baseTokens []string) {
	t.Helper()
	g, b := streamsOf(t, got["MediaStreams"]), streamsOf(t, baseline["MediaStreams"])
	if len(g) != len(b) {
		t.Errorf("%s: %d streams, a viewer with no settings gets %d", label, len(g), len(b))
		return
	}
	wantData := []int{}
	for i := range b {
		index := int(b[i]["Index"].(float64))
		if index != int(g[i]["Index"].(float64)) {
			t.Errorf("%s: position %d holds Index %v, a viewer with no settings gets %d", label, i, g[i]["Index"], index)
		}
		if slices.Contains(data, index) && (b[i]["Type"] == "Audio" || b[i]["Type"] == "Subtitle") {
			wantData = append(wantData, index)
			continue
		}
		if canon(g[i], tokens...) != canon(b[i], baseTokens...) {
			t.Errorf("%s: stream %d is not what a viewer with no settings gets:\n%s\nwant\n%s", label, i, canon(g[i], tokens...), canon(b[i], baseTokens...))
		}
	}
	if have := dataAt(g); !slices.Equal(have, wantData) {
		t.Errorf("%s: Type Data at %v, want exactly %v (types %v)", label, have, wantData, typesOfStreams(g))
	}
	checkDefaults(t, label, got)
	if got["TranscodingUrl"] != nil || baseline["TranscodingUrl"] != nil {
		if a, b := transcodingParams(t, got["TranscodingUrl"], tokens...), transcodingParams(t, baseline["TranscodingUrl"], baseTokens...); a != b {
			t.Errorf("%s: TranscodingUrl %s, a viewer with no settings gets %s", label, a, b)
		}
	}
	rest := []string{"MediaStreams", "TranscodingUrl"}
	if canon(without(got, rest...), tokens...) != canon(without(baseline, rest...), baseTokens...) {
		t.Errorf("%s: the source differs beyond its streams:\n%s\nwant\n%s", label, canon(without(got, rest...), tokens...), canon(without(baseline, rest...), baseTokens...))
	}
}

// checkList holds a list of streams (an item's) to the baseline's: the same positions and Indexes, Data exactly at the
// want indexes that the baseline lists, every other stream equal.
func checkList(t *testing.T, label string, got, baseline []doc, data []int, tokens, baseTokens []string) {
	t.Helper()
	checkAs(t, label, doc{"MediaStreams": toAny(got)}, doc{"MediaStreams": toAny(baseline)}, data, tokens, baseTokens)
}

func toAny(streams []doc) []any {
	out := make([]any, len(streams))
	for i, s := range streams {
		out[i] = s
	}
	return out
}

// a viewer with no settings is the baseline of every comparison.
func (f *langFixture) baseline(t *testing.T) viewer { return f.viewer(t, "plain", "", "", "") }

type babelCase struct {
	name            string
	audio, subtitle string
	prefs           string
	film            string
	data            []int
}

const bothOn = `{"HideOtherAudio":true,"HideOtherSubtitles":true}`

func babelCases() []babelCase {
	return []babelCase{
		{name: "audio eng, subtitle eng, both switches", audio: "eng", subtitle: "eng", prefs: bothOn, film: "babel", data: []int{2, 3, 8, 9, 10, 12, 13, 14}},
		{name: "KeepSubtitleLanguages deu", audio: "eng", subtitle: "eng", prefs: `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepSubtitleLanguages":["deu"]}`, film: "babel", data: []int{2, 3, 10, 12, 14}},
		{name: "KeepAudioLanguages fra", audio: "eng", subtitle: "eng", prefs: `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepAudioLanguages":["fra"]}`, film: "babel", data: []int{2, 8, 9, 10, 12, 13, 14}},
		{name: "audio deu, subtitle fra", audio: "deu", subtitle: "fra", prefs: bothOn, film: "babel", data: []int{3, 12, 14}},
		{name: "audio deu, subtitle deu", audio: "deu", subtitle: "deu", prefs: bothOn, film: "babel", data: []int{3, 10, 12, 14}},
		{name: "audio and subtitle saved as ger", audio: "ger", subtitle: "ger", prefs: bothOn, film: "babel", data: []int{3, 10, 12, 14}},
		{name: "audio and subtitle saved as de", audio: "de", subtitle: "de", prefs: bothOn, film: "babel", data: []int{3, 10, 12, 14}},
		{name: "only the audio switch", audio: "eng", subtitle: "eng", prefs: `{"HideOtherAudio":true}`, film: "babel", data: []int{2, 3}},
		{name: "only the subtitle switch", audio: "eng", subtitle: "eng", prefs: `{"HideOtherSubtitles":true}`, film: "babel", data: []int{8, 9, 10, 12, 13, 14}},
		{name: "both languages, both switches off", audio: "eng", subtitle: "eng", prefs: `{"HideOtherAudio":false}`, film: "babel", data: []int{}},
		{name: "Kino Film, audio eng, subtitle eng", audio: "eng", subtitle: "eng", prefs: bothOn, film: "kino", data: []int{5}},
		{name: "Kino Film, audio deu, subtitle deu", audio: "deu", subtitle: "deu", prefs: bothOn, film: "kino", data: []int{2, 4, 5}},
		{name: "Kino Film, AudioLanguagePreference OriginalLanguage, subtitle eng", audio: "OriginalLanguage", subtitle: "eng", prefs: bothOn, film: "kino", data: []int{2, 5}},
	}
}

func (f *langFixture) filmOf(name string) (film string, types []string) {
	if name == "kino" {
		return f.kino, kinoTypes
	}
	return f.babel, babelTypes
}

// The matrix of the criteria: 15 streams for Babel Film at their own positions, Type Data exactly where each setting
// hides a track, in PlaybackInfo and in the item's details (its alias too), in every list of each answer.
func TestAnOptedInViewerIsListedTheTracksOutsideTheirLanguagesAsPlaceholders(t *testing.T) {
	f := languagesEnv(t, true)
	capture := captureDataStream(t)
	plain := f.baseline(t)
	for _, name := range []string{"babel", "kino"} {
		film, types := f.filmOf(name)
		got := streamsOf(t, sourceOf(f.info(t, 0, plain, film, nil))["MediaStreams"])
		if !slices.Equal(typesOfStreams(got), types) {
			t.Fatalf("a viewer with no settings is listed %v for %s, this check assumes %v", typesOfStreams(got), name, types)
		}
	}
	for i, c := range babelCases() {
		t.Run(c.name, func(t *testing.T) {
			film, types := f.filmOf(c.film)
			v := f.viewer(t, fmt.Sprintf("viewer%d", i), c.audio, c.subtitle, c.prefs)
			reply := f.info(t, 0, v, film, nil)
			source, base := sourceOf(reply), sourceOf(f.info(t, 0, plain, film, nil))
			streams := streamsOf(t, source["MediaStreams"])
			if len(streams) != len(types) {
				t.Fatalf("listed %d streams, want %d: %v", len(streams), len(types), typesOfStreams(streams))
			}
			for position, s := range streams {
				if int(s["Index"].(float64)) != position {
					t.Errorf("stream %d has Index %v", position, s["Index"])
				}
				want := types[position]
				if slices.Contains(c.data, position) {
					want = "Data"
				}
				if s["Type"] != want {
					t.Errorf("stream %d is %v, want %s", position, s["Type"], want)
				}
			}
			if have := dataAt(streams); !slices.Equal(have, c.data) {
				t.Errorf("PlaybackInfo: Type Data at %v, want exactly %v", have, c.data)
			}
			checkAs(t, "PlaybackInfo", source, base, c.data, []string{v.token}, []string{plain.token})
			checkPlaceholders(t, "PlaybackInfo", streams, capture)

			// The item's details: both lists, and the alias. The item lists the default audio track alone (invariant 32).
			// The baseline is a viewer with the same languages and both switches off: since JS-121 the item lists the track the
			// viewer's audio language chooses in place of the file's default, hiding hides nothing of what that leaves.
			baseItem := f.item(t, 0, f.viewer(t, fmt.Sprintf("baseline%d", i), c.audio, c.subtitle, ""), film, false)
			for _, alias := range []bool{false, true} {
				answer := f.item(t, 0, v, film, alias)
				for list, got := range listsOf(t, answer) {
					checkList(t, fmt.Sprintf("item (alias %v) %s", alias, list), got, listsOf(t, baseItem)[list], c.data, []string{v.token}, []string{plain.token})
					checkPlaceholders(t, "item "+list, got, capture)
				}
				if answer["OriginalLanguage"] != baseItem["OriginalLanguage"] {
					t.Errorf("OriginalLanguage = %v, a viewer with no settings gets %v", answer["OriginalLanguage"], baseItem["OriginalLanguage"])
				}
				checkDefaults(t, "item", sourceOf(answer))
			}
		})
	}

	t.Run("Babel Film's item details list Data exactly at 8, 9, 10, 12, 13 and 14, and the one audio stream", func(t *testing.T) {
		v := f.viewer(t, "details", "eng", "eng", bothOn)
		for _, alias := range []bool{false, true} {
			answer := f.item(t, 0, v, f.babel, alias)
			for list, streams := range listsOf(t, answer) {
				if have := dataAt(streams); !slices.Equal(have, []int{8, 9, 10, 12, 13, 14}) {
					t.Errorf("alias %v %s: Type Data at %v (types %v), want 8, 9, 10, 12, 13, 14", alias, list, have, typesOfStreams(streams))
				}
				if audio := strings.Count(strings.Join(typesOfStreams(streams), " "), "Audio"); audio != 1 {
					t.Errorf("alias %v %s lists %d audio streams, want the default alone", alias, list, audio)
				}
			}
		}
	})

	t.Run("lists are unchanged: no Type Data in GET /Items", func(t *testing.T) {
		v := f.viewer(t, "lister", "eng", "eng", bothOn)
		path := "/Items?ids=" + db.CompactID(f.babel) + "&Fields=MediaSources,MediaStreams&userId=" + db.CompactID(v.id)
		ask := func(who viewer) string {
			response, raw := f.do(t, 0, "GET", path, who.token, nil, "Authorization", f.auth(who))
			if response.StatusCode != 200 {
				t.Fatalf("GET %s: %d %s", path, response.StatusCode, raw)
			}
			return canon(json.RawMessage(raw), who.token)
		}
		got, want := ask(v), ask(plain)
		if strings.Contains(got, `"Type":"Data"`) {
			t.Errorf("a list holds a stream of Type Data: %s", got)
		}
		if got != want {
			t.Errorf("a list answers the opted-in viewer differently from a viewer with no settings:\n%s\nwant\n%s", got, want)
		}
	})
}

// Off unless asked, a cleared language pauses its kind only, and ShowAllTracks lists what both switches off list.
func TestSwitchesLanguagesAndShowAllTracksDecideWhatIsHidden(t *testing.T) {
	f := languagesEnv(t, true)
	plain := f.baseline(t)
	all := []int{2, 3, 8, 9, 10, 12, 13, 14}
	subs := []int{8, 9, 10, 12, 13, 14}
	check := func(t *testing.T, v viewer, label string, data []int) {
		t.Helper()
		source := sourceOf(f.info(t, 0, v, f.babel, nil))
		checkAs(t, label+": PlaybackInfo", source, sourceOf(f.info(t, 0, plain, f.babel, nil)), data, []string{v.token}, []string{plain.token})
		for list, streams := range listsOf(t, f.item(t, 0, v, f.babel, false)) {
			var want []int
			for _, index := range data {
				if index == 2 || index == 3 { // The item lists the default audio track alone.
					continue
				}
				want = append(want, index)
			}
			if have := dataAt(streams); !slices.Equal(have, want) {
				t.Errorf("%s: item %s: Type Data at %v, want %v", label, list, have, want)
			}
		}
	}
	t.Run("both languages set and both switches off: no Data and the streams of a viewer with no settings", func(t *testing.T) {
		v := f.viewer(t, "off", "eng", "eng", "")
		check(t, v, "off", []int{})
	})
	t.Run("a cleared audio language pauses audio only, and saving it again hides the same tracks without a PATCH", func(t *testing.T) {
		for _, how := range []string{"left out", "empty"} {
			v := f.viewer(t, "clearaudio-"+how, "eng", "eng", bothOn)
			check(t, v, "before", all)
			if how == "empty" {
				if status := f.save(t, 0, "path", v.id, v.token, `{"AudioLanguagePreference":"","SubtitleLanguagePreference":"eng"}`).StatusCode; status != 204 {
					t.Fatalf("saving: %d", status)
				}
			} else {
				f.saveLanguages(t, 0, v, "", "eng")
			}
			check(t, v, "audio language "+how, subs)
			f.saveLanguages(t, 0, v, "eng", "eng")
			check(t, v, "audio language saved again", all)
		}
	})
	t.Run("a cleared subtitle language pauses subtitles only", func(t *testing.T) {
		for _, how := range []string{"left out", "empty"} {
			v := f.viewer(t, "clearsub-"+how, "eng", "eng", bothOn)
			if how == "empty" {
				if status := f.save(t, 0, "path", v.id, v.token, `{"AudioLanguagePreference":"eng","SubtitleLanguagePreference":""}`).StatusCode; status != 204 {
					t.Fatalf("saving: %d", status)
				}
			} else {
				f.saveLanguages(t, 0, v, "eng", "")
			}
			check(t, v, "subtitle language "+how, []int{2, 3})
			f.saveLanguages(t, 0, v, "eng", "eng")
			check(t, v, "subtitle language saved again", all)
		}
	})
	t.Run("ShowAllTracks lists what both switches off list, and false hides the same tracks as before", func(t *testing.T) {
		v := f.viewer(t, "showall", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepSubtitleLanguages":["deu"]}`)
		check(t, v, "before", []int{2, 3, 10, 12, 14})
		f.patch(t, 0, v, `{"ShowAllTracks":true}`)
		check(t, v, "ShowAllTracks", []int{})
		for _, alias := range []bool{false, true} {
			if data := dataAt(streamsOf(t, f.item(t, 0, v, f.babel, alias)["MediaStreams"])); len(data) != 0 {
				t.Errorf("ShowAllTracks: the item lists Type Data at %v", data)
			}
		}
		f.patch(t, 0, v, `{"ShowAllTracks":false}`)
		check(t, v, "ShowAllTracks false again", []int{2, 3, 10, 12, 14})
	})
}

// The track a request names is listed as itself, and every index a route takes is still accepted.
func TestTheTrackARequestNamesIsListedAsItselfAndEveryIndexIsStillAccepted(t *testing.T) {
	f := languagesEnv(t, true)
	plain := f.baseline(t)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	hidden := []int{2, 3, 8, 9, 10, 12, 13, 14}
	without := func(indexes ...int) []int {
		var out []int
		for _, i := range hidden {
			if !slices.Contains(indexes, i) {
				out = append(out, i)
			}
		}
		return out
	}
	t.Run("AudioStreamIndex 2: stream 2 is Audio as for a viewer with no settings, 3 stays Data, and the transcode names it", func(t *testing.T) {
		extra := map[string]any{"AudioStreamIndex": 2}
		source, base := sourceOf(f.info(t, 0, v, f.babel, extra)), sourceOf(f.info(t, 0, plain, f.babel, extra))
		checkAs(t, "AudioStreamIndex 2", source, base, without(2), []string{v.token}, []string{plain.token})
		checkDefaults(t, "AudioStreamIndex 2", source)
		transcodingURL, _ := source["TranscodingUrl"].(string)
		if !strings.Contains(transcodingURL, "AudioStreamIndex=2") {
			t.Fatalf("the TranscodingUrl does not carry AudioStreamIndex=2: %q", transcodingURL)
		}
		if response, raw := f.do(t, 0, "GET", transcodingURL, "", nil); response.StatusCode != 200 {
			t.Errorf("the master playlist: %d %s", response.StatusCode, raw)
		}
	})
	t.Run("SubtitleStreamIndex 8: stream 8 is Subtitle with its DeliveryUrl, 9 and 10 stay Data, and it is the default", func(t *testing.T) {
		extra := map[string]any{"SubtitleStreamIndex": 8}
		source, base := sourceOf(f.info(t, 0, v, f.babel, extra)), sourceOf(f.info(t, 0, plain, f.babel, extra))
		checkAs(t, "SubtitleStreamIndex 8", source, base, without(8), []string{v.token}, []string{plain.token})
		streams := streamsOf(t, source["MediaStreams"])
		if streams[8]["Type"] != "Subtitle" || !strings.Contains(fmt.Sprint(streams[8]["DeliveryUrl"]), "/Subtitles/8/") {
			t.Errorf("stream 8: %v", streams[8])
		}
		if got := source["DefaultSubtitleStreamIndex"]; got != float64(8) {
			t.Errorf("DefaultSubtitleStreamIndex = %v, want 8", got)
		}
	})
	t.Run("naming -1, 99 or an index of the other kind lists what naming nothing lists", func(t *testing.T) {
		want := dataAt(streamsOf(t, sourceOf(f.info(t, 0, v, f.babel, nil))["MediaStreams"]))
		if !slices.Equal(want, hidden) {
			t.Fatalf("naming nothing lists Type Data at %v, want %v", want, hidden)
		}
		for _, extra := range []map[string]any{{"AudioStreamIndex": -1}, {"AudioStreamIndex": 99}, {"SubtitleStreamIndex": 99}, {"AudioStreamIndex": 8}, {"SubtitleStreamIndex": 2}} {
			source := sourceOf(f.info(t, 0, v, f.babel, extra))
			if have := dataAt(streamsOf(t, source["MediaStreams"])); !slices.Equal(have, want) {
				t.Errorf("naming %v: Type Data at %v, want %v", extra, have, want)
			}
			checkDefaults(t, fmt.Sprint(extra), source)
		}
	})
	t.Run("a request naming SubtitleStreamIndex 5 answers DefaultSubtitleStreamIndex 5 although audio 2 and 3 are hidden", func(t *testing.T) {
		source := sourceOf(f.info(t, 0, v, f.babel, map[string]any{"SubtitleStreamIndex": 5}))
		if got := source["DefaultSubtitleStreamIndex"]; got != float64(5) {
			t.Errorf("DefaultSubtitleStreamIndex = %v, want 5", got)
		}
		if have := dataAt(streamsOf(t, source["MediaStreams"])); !slices.Equal(have, hidden) {
			t.Errorf("Type Data at %v, want %v", have, hidden)
		}
	})
	t.Run("a transcode URL naming hidden tracks is accepted", func(t *testing.T) {
		for i, who := range []viewer{v, plain} {
			// Each viewer's own play session: one id shared by two viewers is one session, which the first ends.
			path := fmt.Sprintf("/Videos/%s/master.m3u8?MediaSourceId=%s&AudioStreamIndex=3&SubtitleStreamIndex=10&PlaySessionId=%032x&ApiKey=%s", db.DashedID(f.babel), f.babelSource, 0x107119+i, who.token)
			if response, raw := f.do(t, 0, "GET", path, "", nil); response.StatusCode != 200 {
				t.Errorf("%s: %s: %d %.200s", who.name, path, response.StatusCode, raw)
			}
		}
	})
	t.Run("a hidden subtitle track is reachable by its index", func(t *testing.T) {
		for _, c := range []struct {
			index int
			cue   string
		}{{8, "Babel track 8"}, {9, "Babel track 9"}, {10, "Babel track 10"}, {13, "Babel file de"}, {14, "Babel file es"}} {
			path := fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/0/Stream.vtt?ApiKey=%s", db.DashedID(f.babel), f.babelSource, c.index, v.token)
			if response, raw := f.do(t, 0, "GET", path, "", nil); response.StatusCode != 200 || !strings.Contains(string(raw), c.cue) {
				t.Errorf("track %d: %d %.200s, want the cue %q", c.index, response.StatusCode, raw, c.cue)
			}
		}
		pgs := func(who viewer) (int, string) {
			path := fmt.Sprintf("/Videos/%s/%s/Subtitles/12/0/Stream.pgssub?ApiKey=%s", db.DashedID(f.babel), f.babelSource, who.token)
			response, raw := f.do(t, 0, "GET", path, "", nil)
			return response.StatusCode, string(raw)
		}
		gotStatus, gotBody := pgs(v)
		wantStatus, wantBody := pgs(plain)
		if gotStatus != wantStatus || gotBody != wantBody || wantStatus != 200 {
			t.Errorf("track 12 as pgssub: %d (%d bytes), a viewer with no settings gets %d (%d bytes), want 200", gotStatus, len(gotBody), wantStatus, len(wantBody))
		}
	})
}

// Hiding is for the viewer whose token it is, and a change of setting acts at once on every replica.
func TestEachViewerIsListedByTheirOwnSettingsAtOnceOnEveryReplica(t *testing.T) {
	f := languagesEnv(t, true)
	eng := f.viewer(t, "eng", "eng", "eng", bothOn)
	deu := f.viewer(t, "deu", "deu", "deu", bothOn)
	plain := f.baseline(t)
	engData := []int{2, 3, 8, 9, 10, 12, 13, 14}
	deuData := []int{3, 10, 12, 14}
	t.Run("three viewers at the same time, 20 PlaybackInfo requests each, each get their own list", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan string, 60)
		for _, c := range []struct {
			v    viewer
			want []int
		}{{eng, engData}, {deu, deuData}, {plain, []int{}}} {
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					response, raw := f.do(t, 0, "POST", "/Items/"+f.babel+"/PlaybackInfo", c.v.token, map[string]any{"DeviceProfile": f.profile, "MaxStreamingBitrate": 2147483647}, "Authorization", f.auth(c.v))
					var reply doc
					if response.StatusCode != 200 || json.Unmarshal(raw, &reply) != nil {
						errs <- fmt.Sprintf("%s: %d %.100s", c.v.name, response.StatusCode, raw)
						return
					}
					if have := dataAt(streamsOf(t, sourceOf(reply)["MediaStreams"])); !slices.Equal(have, c.want) {
						errs <- fmt.Sprintf("%s: Type Data at %v, want %v", c.v.name, have, c.want)
					}
				}()
			}
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	})
	t.Run("the settings applied are the token's user's, whoever the request names", func(t *testing.T) {
		got := func(label string, status int, raw []byte, streams func(doc) []doc) {
			if status != 200 {
				return // Refusing to answer for another user is as good as answering for the caller.
			}
			var reply doc
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			if have := dataAt(streams(reply)); !slices.Equal(have, engData) && !(label[:4] == "item" && slices.Equal(have, []int{8, 9, 10, 12, 13, 14})) {
				t.Errorf("%s: the eng viewer got Type Data at %v", label, have)
			}
		}
		source := func(reply doc) []doc { return streamsOf(t, sourceOf(reply)["MediaStreams"]) }
		listed := func(reply doc) []doc { return streamsOf(t, reply["MediaStreams"]) }
		info := func(label, query string, body map[string]any) {
			body["DeviceProfile"], body["MaxStreamingBitrate"] = f.profile, 2147483647
			response, raw := f.do(t, 0, "POST", "/Items/"+f.babel+"/PlaybackInfo"+query, eng.token, body, "Authorization", f.auth(eng))
			got(label, response.StatusCode, raw, source)
		}
		info("PlaybackInfo, UserId in the body", "", map[string]any{"UserId": deu.id})
		info("PlaybackInfo, UserId in the query", "?UserId="+deu.id, map[string]any{})
		info("PlaybackInfo, UserId in the body, compact", "", map[string]any{"UserId": db.CompactID(deu.id)})
		for _, path := range []string{"/Users/" + db.CompactID(deu.id) + "/Items/" + db.CompactID(f.babel), "/Items/" + db.CompactID(f.babel) + "?userId=" + db.CompactID(deu.id)} {
			response, raw := f.do(t, 0, "GET", path, eng.token, nil, "Authorization", f.auth(eng))
			got("item "+path, response.StatusCode, raw, listed)
		}
	})
}

// After a PATCH or a save through one api process, the very next PlaybackInfo and item through another list by the new
// settings, with no restart.
func TestAChangedSettingActsAtOnceOnEveryReplica(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "acts", "eng", "eng", "")
	both := []int{2, 3, 8, 9, 10, 12, 13, 14}
	steps := []struct {
		name string
		do   func()
		data []int
	}{
		{"a switch turned on", func() { f.patch(t, 0, v, `{"HideOtherAudio":true}`) }, []int{2, 3}},
		{"the other switch turned on", func() { f.patch(t, 0, v, `{"HideOtherSubtitles":true}`) }, both},
		{"a keep list", func() { f.patch(t, 0, v, `{"KeepAudioLanguages":["deu"],"KeepSubtitleLanguages":["fra"]}`) }, []int{3, 8, 9, 12, 13, 14}},
		{"ShowAllTracks", func() { f.patch(t, 0, v, `{"ShowAllTracks":true}`) }, []int{}},
		{"ShowAllTracks off again", func() { f.patch(t, 0, v, `{"ShowAllTracks":false}`) }, []int{3, 8, 9, 12, 13, 14}},
		{"a switch turned off", func() { f.patch(t, 0, v, `{"HideOtherAudio":false}`) }, []int{8, 9, 12, 13, 14}},
		{"a language saved", func() {
			f.patch(t, 0, v, `{"KeepSubtitleLanguages":[]}`)
			f.saveLanguages(t, 0, v, "eng", "deu")
		}, []int{10, 12, 14}},
	}
	for _, s := range steps {
		s.do()
		source := sourceOf(f.info(t, 1, v, f.babel, nil))
		if have := dataAt(streamsOf(t, source["MediaStreams"])); !slices.Equal(have, s.data) {
			t.Errorf("%s: PlaybackInfo through the second process lists Type Data at %v, want %v", s.name, have, s.data)
		}
		want := []int{}
		for _, index := range s.data {
			if index != 2 && index != 3 {
				want = append(want, index)
			}
		}
		if have := dataAt(streamsOf(t, f.item(t, 1, v, f.babel, false)["MediaStreams"])); !slices.Equal(have, want) {
			t.Errorf("%s: the item through the second process lists Type Data at %v, want %v", s.name, have, want)
		}
	}
}

// "That viewer" on three kinds of replica: the answers differ from a viewer with no settings in the placeholders alone.
// A track dropped today stays dropped and is not listed as a placeholder.
func TestHidingChangesNothingButThePlaceholdersWhateverTheReplicaCanOffer(t *testing.T) {
	for _, mode := range []string{"a worker running", "a replica with transcoding off", "no worker alive"} {
		t.Run(mode, func(t *testing.T) {
			f := languagesEnv(t, mode != "a replica with transcoding off")
			plain := f.baseline(t)
			v := f.viewer(t, "eng", "eng", "eng", bothOn)
			switch mode {
			case "a replica with transcoding off":
				mustExec(t, f.pool, "TRUNCATE transcode_nodes")
				for _, s := range f.apis {
					s.Transcode = nil
				}
			case "no worker alive":
				f.stop()
				mustExec(t, f.pool, "TRUNCATE transcode_nodes")
			}
			for _, extra := range []map[string]any{nil, {"AudioStreamIndex": 2}, {"SubtitleStreamIndex": 8}, {"AudioStreamIndex": 3, "SubtitleStreamIndex": 10}} {
				base := sourceOf(f.info(t, 0, plain, f.babel, extra))
				got := sourceOf(f.info(t, 0, v, f.babel, extra))
				baseStreams := streamsOf(t, base["MediaStreams"])
				var hidden []int
				for _, s := range baseStreams {
					index := int(s["Index"].(float64))
					if slices.Contains([]int{2, 3, 8, 9, 10, 12, 13, 14}, index) && !(extra["AudioStreamIndex"] == float64(index) || extra["AudioStreamIndex"] == index || extra["SubtitleStreamIndex"] == index) {
						hidden = append(hidden, index)
					}
				}
				checkAs(t, fmt.Sprintf("%s, %v", mode, extra), got, base, hidden, []string{v.token}, []string{plain.token})
				if len(baseStreams) == 0 {
					t.Fatal("no streams listed")
				}
				switch mode {
				case "a replica with transcoding off":
					if n := strings.Count(strings.Join(typesOfStreams(baseStreams), " "), "Audio"); n != 1 {
						t.Errorf("with transcoding off a viewer with no settings is listed %d audio tracks, want the default alone", n)
					}
				case "no worker alive":
					if len(baseStreams) >= len(babelTypes) {
						t.Errorf("with no worker alive %d streams are listed, the uncached text tracks should be dropped (of %d)", len(baseStreams), len(babelTypes))
					}
				}
			}
		})
	}
}

// Hiding never splits a job: a viewer for whom track 2 is hidden and a viewer for whom it is listed, each on a device of
// their own, both transcode Babel Film with AudioStreamIndex 2, and one job makes it.
func TestHidingNeverSplitsATranscodeJob(t *testing.T) {
	f := languagesEnv(t, true)
	eng := f.viewer(t, "eng", "eng", "eng", bothOn)
	deu := f.viewer(t, "deu", "deu", "deu", bothOn)
	// The case is only one if track 2 is hidden for the eng viewer when no request names it, and listed for the deu viewer.
	if have := dataAt(streamsOf(t, sourceOf(f.info(t, 0, eng, f.babel, nil))["MediaStreams"])); !slices.Contains(have, 2) {
		t.Errorf("track 2 is not hidden from the eng viewer (Type Data at %v)", have)
	}
	if have := dataAt(streamsOf(t, sourceOf(f.info(t, 0, deu, f.babel, nil))["MediaStreams"])); slices.Contains(have, 2) {
		t.Errorf("track 2 is hidden from the deu viewer (Type Data at %v)", have)
	}
	for _, v := range []viewer{eng, deu} {
		source := sourceOf(f.info(t, 0, v, f.babel, map[string]any{"AudioStreamIndex": 2}))
		transcodingURL, _ := source["TranscodingUrl"].(string)
		if transcodingURL == "" {
			t.Fatalf("%s: no TranscodingUrl", v.name)
		}
		_, master := f.do(t, 0, "GET", transcodingURL, "", nil)
		path := "/videos/" + db.DashedID(f.babel)
		_, rest, ok := strings.Cut(string(master), "main.m3u8?")
		if !ok {
			t.Fatalf("%s: no main playlist in the master playlist:\n%s", v.name, master)
		}
		_, playlist := f.do(t, 1, "GET", path+"/main.m3u8?"+strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]), "", nil)
		var first string
		for _, line := range strings.Split(string(playlist), "\n") {
			if strings.HasPrefix(line, "hls1/main/0.mp4?") {
				first = path + "/" + line
			}
		}
		if first == "" {
			t.Fatalf("%s: no entry for segment 0:\n%s", v.name, playlist)
		}
		if response, raw := f.do(t, 0, "GET", first, "", nil); response.StatusCode != 200 {
			t.Fatalf("%s: segment 0: %d %.200s", v.name, response.StatusCode, raw)
		}
	}
	if rows := scalar[int](t, f.pool, "SELECT count(*)::int FROM transcode_jobs"); rows != 1 {
		t.Errorf("transcode_jobs holds %d rows for the film, want exactly 1 (hiding must not enter the spec)", rows)
	}
}

// Nothing extracts or writes because of hiding.
func TestHidingExtractsAndWritesNothing(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	jobs := func() int { return scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs") }
	rows := func() string {
		return scalar[string](t, f.pool, `SELECT (SELECT md5(string_agg(i::text, '' ORDER BY id)) FROM items i)
			|| (SELECT md5(string_agg(m::text, '' ORDER BY id)) FROM media_sources m)`)
	}
	before := rows()
	if have := dataAt(streamsOf(t, sourceOf(f.info(t, 0, v, f.babel, nil))["MediaStreams"])); !slices.Contains(have, 8) {
		t.Fatalf("no track is hidden from the viewer (Type Data at %v), so nothing here is tested", have)
	}
	f.item(t, 0, v, f.babel, false)
	f.item(t, 1, v, f.babel, true)
	if n := jobs(); n != 0 {
		t.Errorf("PlaybackInfo and the item's details made %d subtitle_jobs rows, want 0", n)
	}
	if after := rows(); after != before {
		t.Error("PlaybackInfo and the item's details wrote to items or media_sources")
	}
	fetch := func(index int) int {
		path := fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/0/Stream.vtt?ApiKey=%s", db.DashedID(f.babel), f.babelSource, index, v.token)
		response, _ := f.do(t, 0, "GET", path, "", nil)
		return response.StatusCode
	}
	if status := fetch(5); status != 200 || jobs() != 1 {
		t.Errorf("fetching track 5: %d, %d rows, want 200 and exactly 1", status, jobs())
	}
	for _, index := range []int{8, 9, 10} {
		if status := fetch(index); status != 200 || jobs() != 1 {
			t.Errorf("fetching hidden track %d: %d, %d rows, want 200 and no further row (the one pass wrote every text track)", index, status, jobs())
		}
	}
	if status := fetch(13); status != 200 || jobs() != 2 {
		t.Errorf("fetching hidden file 13: %d, %d rows, want 200 and exactly 1 more (2)", status, jobs())
	}
}

// PlaybackInfo logs one line at Debug for each source it hid a track in.
func TestPlaybackInfoLogsWhatItHidAtDebugOnly(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	plain := f.baseline(t)
	f.info(t, 0, plain, f.babel, nil)
	mark := len(f.log.String())
	f.info(t, 0, v, f.babel, nil)
	lines := strings.Split(f.log.String()[mark:], "\n")
	var hid []string
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "hid") || strings.Contains(strings.ToLower(line), "placeholder") {
			hid = append(hid, line)
			if !strings.Contains(line, "level=DEBUG") {
				t.Errorf("a line about hiding is not at Debug: %s", line)
			}
		}
	}
	if len(hid) != 1 {
		t.Fatalf("%d lines about hiding for one source, want 1:\n%s", len(hid), strings.Join(lines, "\n"))
	}
	line := hid[0]
	for _, want := range []string{v.id, f.babel} {
		if !strings.Contains(line, want) && !strings.Contains(line, strings.ReplaceAll(want, "-", "")) {
			t.Errorf("the line does not carry %s: %s", want, line)
		}
	}
	if !strings.Contains(line, f.babelSource) && !strings.Contains(line, db.DashedID(f.babelSource)) {
		t.Errorf("the line does not carry the source %s: %s", f.babelSource, line)
	}
	if !strings.Contains(line, "=8") && !strings.Contains(line, " 8 ") && !strings.HasSuffix(line, " 8") {
		t.Errorf("the line does not say 8 tracks were hidden: %s", line)
	}
}
