//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

// JS-117: a film's original language comes from its file alone, the language of its
// default audio track, and is answered when one item is: OriginalLanguage on the item,
// IsOriginal on the audio streams in that language. compat/captures/original-language-12.4.md
// is what Jellyfin 12.4 answers (no OriginalLanguage, IsOriginal false). The films here are
// rows as a scan before this release wrote them (the item's DTO lists the video, the default
// audio track and the subtitles; media_sources.streams holds every stream, none marked), so a
// check that passes proves the value is made when the item is answered, with no rescan.

func vid(language string) media.MediaStream {
	return media.MediaStream{Index: 0, Type: "Video", Codec: "h264", Language: language}
}

func aud(index int, language string, isDefault bool) media.MediaStream {
	return media.MediaStream{Index: index, Type: "Audio", Codec: "aac", Channels: 2, Language: language, IsDefault: isDefault}
}

func sub(index int, language string, isDefault, external bool) media.MediaStream {
	return media.MediaStream{Index: index, Type: "Subtitle", Codec: "subrip", Language: language, IsDefault: isDefault,
		IsExternal: external, IsTextSubtitleStream: true, SupportsExternalStream: true}
}

// addFilm puts a film with one source per list of streams in the library, as an earlier
// release's scan did, and returns its compact id and its sources' compact ids.
func addFilm(t *testing.T, e *env, name string, sources ...[]media.MediaStream) (string, []string) {
	t.Helper()
	item := uuid.NewString()
	var dtoSources []media.MediaSource
	var ids []string
	for _, streams := range sources {
		source := uuid.NewString()
		ids = append(ids, db.CompactID(source))
		full, _ := json.Marshal(streams)
		// What the scan lists in the item: the video, the default audio track, the subtitles.
		defaultAudio := -1
		for _, s := range streams {
			if s.Type == "Audio" && (defaultAudio == -1 || (s.IsDefault && !streams[defaultAudio].IsDefault)) {
				defaultAudio = s.Index
			}
		}
		listed := []media.MediaStream{}
		for _, s := range streams {
			if s.Type != "Audio" || s.Index == defaultAudio {
				listed = append(listed, s)
			}
		}
		dtoSources = append(dtoSources, media.MediaSource{ID: db.CompactID(source), Protocol: "File", Container: "mkv",
			SupportsTranscoding: true, SupportsDirectPlay: true, SupportsDirectStream: true, MediaStreams: listed})
		if item != "" && len(dtoSources) == 1 {
			library := scalar[string](t, e.pool, "SELECT id::text FROM libraries LIMIT 1")
			dto, _ := json.Marshal(map[string]any{"Name": name, "MediaSources": dtoSources})
			mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
				VALUES ($1, $2, 'Movie', $3, lower($3), lower($3), $4, $5)`, item, library, name, filmTicks, dto)
		}
		mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
			VALUES ($1, $2, 'film.mkv', 65536, 'abc123', 'mkv', $3)`, source, item, full)
	}
	dto, _ := json.Marshal(map[string]any{"Name": name, "MediaSources": dtoSources})
	mustExec(t, e.pool, "UPDATE items SET dto = $2 WHERE id = $1", item, dto)
	return db.CompactID(item), ids
}

// transcodingOn gives every replica a transcode store and a worker, so a PlaybackInfo
// lists every audio stream of a file (invariant 32).
func transcodingOn(t *testing.T, e *env) {
	t.Helper()
	mustExec(t, e.pool, "TRUNCATE transcode_jobs, transcode_nodes")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, slots, output_version) VALUES ('w1', 'worker', 2, "+strconv.Itoa(playback.OutputVersion)+")")
	segments := store.NewPosix(t.TempDir())
	for _, s := range e.apis {
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments}
	}
}

type doc = map[string]any

func (e *env) json(t *testing.T, method, path string, body any) doc {
	t.Helper()
	response, raw := e.do(t, 0, method, path, e.token, body, "Authorization", strings.Replace(auth, "%s", e.token, 1))
	if response.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// The four routes that answer one item, as a name and a function of the item.
type route struct {
	name string
	ask  func(t *testing.T, e *env, item string) doc
}

func (e *env) routes() []route {
	user := db.CompactID(e.user)
	return []route{
		{"GET /Items/{itemId}", func(t *testing.T, e *env, item string) doc {
			return e.json(t, "GET", "/Items/"+item+"?userId="+user, nil)
		}},
		{"GET /Users/{userId}/Items/{itemId}", func(t *testing.T, e *env, item string) doc {
			return e.json(t, "GET", "/Users/"+user+"/Items/"+item, nil)
		}},
		{"GET PlaybackInfo", func(t *testing.T, e *env, item string) doc {
			return e.json(t, "GET", "/Items/"+item+"/PlaybackInfo?UserId="+user, nil)
		}},
		{"POST PlaybackInfo", func(t *testing.T, e *env, item string) doc {
			return e.json(t, "POST", "/Items/"+item+"/PlaybackInfo?UserId="+user, map[string]any{})
		}},
	}
}

func streamsOf(t *testing.T, list any) []doc {
	t.Helper()
	var out []doc
	for _, entry := range list.([]any) {
		out = append(out, entry.(doc))
	}
	return out
}

// marks is "Type index" of each stream whose IsOriginal is true, and fails when a stream
// lacks IsOriginal or holds anything but a boolean.
func marks(t *testing.T, where string, streams []doc) []string {
	t.Helper()
	var marked []string
	for _, s := range streams {
		original, ok := s["IsOriginal"].(bool)
		if !ok {
			t.Errorf("%s: stream %v has no boolean IsOriginal: %v", where, s["Index"], s["IsOriginal"])
			continue
		}
		if original {
			marked = append(marked, fmt.Sprint(s["Type"], " ", s["Index"]))
		}
	}
	return marked
}

// listsOf is every stream list in an answer: the item's MediaStreams and each source's.
func listsOf(t *testing.T, reply doc) map[string][]doc {
	t.Helper()
	lists := map[string][]doc{}
	if streams, ok := reply["MediaStreams"]; ok {
		lists["MediaStreams"] = streamsOf(t, streams)
	}
	for i, source := range reply["MediaSources"].([]any) {
		lists[fmt.Sprintf("MediaSources[%d].MediaStreams", i)] = streamsOf(t, source.(doc)["MediaStreams"])
	}
	return lists
}

// wantOriginal checks an answer to the film: the item's OriginalLanguage (language, "" for
// absent) when the answer is an item's, and exactly the streams of want marked, in every list.
func wantOriginal(t *testing.T, name string, reply doc, language string, want ...string) {
	t.Helper()
	if _, isItem := reply["Name"]; isItem {
		got, present := reply["OriginalLanguage"]
		switch {
		case language == "" && present:
			t.Errorf("%s: OriginalLanguage = %v, want the field absent (not null, not empty, not und)", name, got)
		case language != "" && got != language:
			t.Errorf("%s: OriginalLanguage = %v (present %v), want %q", name, got, present, language)
		}
	}
	for list, streams := range listsOf(t, reply) {
		if got := marks(t, name+" "+list, streams); !slices.Equal(got, want) {
			t.Errorf("%s %s: streams marked IsOriginal = %v, want %v", name, list, got, want)
		}
	}
}

func TestOriginalLanguageIsTheDefaultAudiosOwnTagOnTheItemAndItsAlias(t *testing.T) {
	e := newEnv(t)
	transcodingOn(t, e) // A worker is there, so a subtitle track in a film is not warned about as hidden.
	for _, c := range []struct {
		name     string
		streams  []media.MediaStream
		language string   // "" when the film has none.
		marked   []string // The streams the item lists that are marked.
	}{
		{"eng default then deu", []media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "deu", false)}, "eng", []string{"Audio 1"}},
		{"deu then eng default", []media.MediaStream{vid(""), aud(1, "deu", false), aud(2, "eng", true)}, "eng", []string{"Audio 2"}},
		{"deu and eng with no flag", []media.MediaStream{vid(""), aud(1, "deu", false), aud(2, "eng", false)}, "deu", []string{"Audio 1"}},
		{"the first flagged of two", []media.MediaStream{vid(""), aud(1, "eng", false), aud(2, "jpn", true), aud(3, "eng", true)}, "jpn", []string{"Audio 2"}},
		{"the tag as the file wrote it", []media.MediaStream{vid(""), aud(1, "ger", true), aud(2, "eng", false)}, "ger", []string{"Audio 1"}},
		{"an untagged default and a later eng", []media.MediaStream{vid(""), aud(1, "", true), aud(2, "eng", false)}, "", nil},
		{"an untagged first and a later eng", []media.MediaStream{vid(""), aud(1, "", false), aud(2, "eng", false)}, "", nil},
		{"und on the default", []media.MediaStream{vid(""), aud(1, "und", true), aud(2, "eng", false)}, "", nil},
		{"mis on the default", []media.MediaStream{vid(""), aud(1, "mis", true), aud(2, "eng", false)}, "", nil},
		{"mul on the default", []media.MediaStream{vid(""), aud(1, "mul", true), aud(2, "eng", false)}, "", nil},
		{"zxx on the default", []media.MediaStream{vid(""), aud(1, "zxx", true), aud(2, "eng", false)}, "", nil},
		{"a tag that does not parse", []media.MediaStream{vid(""), aud(1, "???", true), aud(2, "eng", false)}, "", nil},
		{"no audio", []media.MediaStream{vid("eng")}, "", nil},
		{"a video and subtitles in the language mark nothing", []media.MediaStream{vid("eng"), aud(1, "eng", true),
			sub(2, "eng", true, false), sub(3, "eng", false, true), sub(4, "deu", false, true)}, "eng", []string{"Audio 1"}},
		{"a video and a subtitle with a language and a default flag do not decide", []media.MediaStream{
			{Index: 0, Type: "Video", Codec: "h264", Language: "jpn", IsDefault: true},
			aud(1, "deu", false), aud(2, "eng", false), sub(3, "fra", true, false)}, "deu", []string{"Audio 1"}},
		{"only a subtitle in a language", []media.MediaStream{vid(""), aud(1, "", true), sub(2, "eng", true, true)}, "", nil},
	} {
		item, _ := addFilm(t, e, c.name, c.streams)
		for _, r := range e.routes()[:2] {
			reply := r.ask(t, e, item)
			wantOriginal(t, c.name+" on "+r.name, reply, c.language, c.marked...)
			// IsOriginal is still sent, false, on every stream; a stream is marked only when it is audio.
			for list, streams := range listsOf(t, reply) {
				for _, s := range streams {
					if s["Type"] != "Audio" && s["IsOriginal"] != false {
						t.Errorf("%s on %s %s: a %v stream has IsOriginal %v", c.name, r.name, list, s["Type"], s["IsOriginal"])
					}
				}
			}
		}
	}
}

func TestPlaybackInfoMarksEveryAudioStreamInTheOriginalLanguage(t *testing.T) {
	e := newEnv(t)
	commentary := []media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "eng", false), aud(3, "deu", false)}
	ger := []media.MediaStream{vid(""), aud(1, "ger", true), aud(2, "deu", false), aud(3, "eng", false)}
	commentaryItem, _ := addFilm(t, e, "commentary", commentary)
	gerItem, _ := addFilm(t, e, "ger", ger)
	plain, _ := addFilm(t, e, "nolang", []media.MediaStream{vid(""), aud(1, "", true), aud(2, "eng", false)})
	for _, r := range e.routes()[2:] {
		// Transcoding off: only the default track is listed.
		wantOriginal(t, "off: "+r.name, r.ask(t, e, commentaryItem), "eng", "Audio 1")
		wantOriginal(t, "off, ger: "+r.name, r.ask(t, e, gerItem), "ger", "Audio 1")
	}
	transcodingOn(t, e)
	for _, r := range e.routes()[2:] {
		reply := r.ask(t, e, commentaryItem)
		wantOriginal(t, "on: "+r.name, reply, "eng", "Audio 1", "Audio 2")
		if n := len(listsOf(t, reply)["MediaSources[0].MediaStreams"]); n != 4 {
			t.Errorf("on: %s lists %d streams, the premise is 4 (video and all three audio)", r.name, n)
		}
		// For [ger default, deu, eng] it is the ger and the deu stream, which share a language.
		wantOriginal(t, "on, ger: "+r.name, r.ask(t, e, gerItem), "ger", "Audio 1", "Audio 2")
		wantOriginal(t, "on, no language: "+r.name, r.ask(t, e, plain), "")
	}
}

func TestMarksFollowTheFileNotTheRequest(t *testing.T) {
	e := newEnv(t)
	transcodingOn(t, e)
	item, sources := addFilm(t, e, "request", []media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "eng", false), aud(3, "deu", false)})
	user := db.CompactID(e.user)
	baseline := e.json(t, "POST", "/Items/"+item+"/PlaybackInfo?UserId="+user, map[string]any{})
	wantOriginal(t, "baseline", baseline, "eng", "Audio 1", "Audio 2")
	for name, reply := range map[string]doc{
		"POST naming the deu track":    e.json(t, "POST", "/Items/"+item+"/PlaybackInfo?UserId="+user, map[string]any{"AudioStreamIndex": 3}),
		"POST naming the source":       e.json(t, "POST", "/Items/"+item+"/PlaybackInfo?UserId="+user, map[string]any{"MediaSourceId": sources[0], "AudioStreamIndex": "3"}),
		"GET naming the deu track":     e.json(t, "GET", "/Items/"+item+"/PlaybackInfo?UserId="+user+"&AudioStreamIndex=3", nil),
		"GET naming the source":        e.json(t, "GET", "/Items/"+item+"/PlaybackInfo?UserId="+user+"&MediaSourceId="+sources[0]+"&AudioStreamIndex=3", nil),
		"POST naming a subtitle index": e.json(t, "POST", "/Items/"+item+"/PlaybackInfo?UserId="+user, map[string]any{"SubtitleStreamIndex": -1}),
	} {
		wantOriginal(t, name, reply, "eng", "Audio 1", "Audio 2")
	}
}

func TestEachSourceIsMarkedByItsOwnDefaultAudio(t *testing.T) {
	e := newEnv(t)
	item, _ := addFilm(t, e, "two files",
		[]media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "deu", false)},
		[]media.MediaStream{vid(""), aud(1, "deu", true), aud(2, "eng", false)})
	check := func(label string, e *env) {
		for _, r := range e.routes() {
			reply := r.ask(t, e, item)
			lists := listsOf(t, reply)
			if _, isItem := reply["Name"]; isItem {
				if reply["OriginalLanguage"] != "eng" {
					t.Errorf("%s %s: the item's OriginalLanguage = %v, want that of the first source, eng", label, r.name, reply["OriginalLanguage"])
				}
				if got := marks(t, r.name, lists["MediaStreams"]); !slices.Equal(got, []string{"Audio 1"}) {
					t.Errorf("%s %s: the item's MediaStreams (the first source's) marked %v, want [Audio 1]", label, r.name, got)
				}
			}
			// The first source's default is eng (index 1), the second's deu (index 1): each lists its own
			// default track, or both audio tracks when transcoding is on, and marks the ones in its language.
			first := marks(t, r.name, lists["MediaSources[0].MediaStreams"])
			second := marks(t, r.name, lists["MediaSources[1].MediaStreams"])
			if !slices.Equal(first, []string{"Audio 1"}) {
				t.Errorf("%s %s: source 0 marked %v, want [Audio 1] (its default, eng)", label, r.name, first)
			}
			if !slices.Equal(second, []string{"Audio 1"}) {
				t.Errorf("%s %s: source 1 marked %v, want [Audio 1] (its default, deu)", label, r.name, second)
			}
		}
	}
	check("off", e)
	transcodingOn(t, e)
	check("on", e)
}

func TestTheOriginalLanguageIsMadeWhenTheItemIsAnswered(t *testing.T) {
	e := newEnv(t)
	item, sources := addFilm(t, e, "replaced", []media.MediaStream{vid(""), aud(1, "eng", true)})
	for _, r := range e.routes() {
		wantOriginal(t, "before: "+r.name, r.ask(t, e, item), "eng", "Audio 1")
	}
	// The file is replaced by one whose default audio is German, rescanned as a scan writes it.
	streams, _ := json.Marshal([]media.MediaStream{vid(""), aud(1, "deu", true)})
	mustExec(t, e.pool, "UPDATE media_sources SET streams = $2 WHERE id = $1::uuid", sources[0], streams)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources,0,MediaStreams}', $2::jsonb) WHERE id = $1::uuid`, item, streams)
	for _, r := range e.routes() {
		wantOriginal(t, "after: "+r.name, r.ask(t, e, item), "deu", "Audio 1")
	}
}

func TestListsAnswerNoOriginalLanguage(t *testing.T) {
	e := newEnv(t)
	item, _ := addFilm(t, e, "listed", []media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "deu", false)})
	// The details say it, so the lists below are not merely the same before and after.
	wantOriginal(t, "details", e.routes()[0].ask(t, e, item), "eng", "Audio 1")
	user := db.CompactID(e.user)
	for _, path := range []string{
		"/Items?userId=" + user + "&Ids=" + item + "&Fields=MediaSources,MediaStreams",
		"/Items?userId=" + user + "&Recursive=true&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams",
		"/Users/" + user + "/Items?Ids=" + item + "&Fields=MediaSources,MediaStreams",
	} {
		reply := e.json(t, "GET", path, nil)
		rows := reply["Items"].([]any)
		if len(rows) == 0 {
			t.Fatalf("%s lists nothing", path)
		}
		for _, row := range rows {
			row := row.(doc)
			if _, present := row["OriginalLanguage"]; present {
				t.Errorf("%s: a list row answers OriginalLanguage %v", path, row["OriginalLanguage"])
			}
			for list, streams := range listsOf(t, row) {
				if got := marks(t, path+" "+list, streams); len(got) != 0 {
					t.Errorf("%s %s: a list marks %v IsOriginal", path, list, got)
				}
			}
		}
	}
}

func TestAnItemWithNoSourceAnswersAsBefore(t *testing.T) {
	e := newEnv(t)
	library := scalar[string](t, e.pool, "SELECT id::text FROM libraries LIMIT 1")
	series := uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto) VALUES ($1, $2, 'Series', 'Show', 'show', 'show', '{"Name":"Show"}')`, series, library)
	for _, r := range e.routes()[:2] {
		reply := r.ask(t, e, db.CompactID(series))
		for _, key := range []string{"OriginalLanguage", "MediaSources", "MediaStreams"} {
			if _, present := reply[key]; present {
				t.Errorf("%s: an item with no media source answers %s", r.name, key)
			}
		}
	}
	// The library's own details (a CollectionFolder) are not a film either.
	response, _ := e.do(t, 0, "GET", "/Items/"+db.CompactID(library)+"?userId="+db.CompactID(e.user), e.token, nil)
	if response.StatusCode != 200 && response.StatusCode != 404 {
		t.Errorf("a library's details: %d", response.StatusCode)
	}
}

// The statements one item's answer sends are what they were before the task: the value is
// computed from what the answer already reads. testdata/original-language-statements.txt is
// what the replica's pool sent on the tree before JS-117 (a pgx tracer, as JS-111's checks).
// A build that reads one more row to find the language adds a line and fails here. JS-147 (D-018) added one statement
// to the item details with transcoding on: the read of transcode_nodes that says whether a picked audio track can be
// delivered, which is also the worker check a film with subtitles made; PlaybackInfo with transcoding on, for a film of several audio tracks, sends the same one read, so that it lists no track it would not play (D-029). It holds
// for two sources and for transcoding off and on.
func TestAnItemsAnswersSendPostgresNoNewStatement(t *testing.T) {
	e := newEnv(t)
	item, sources := addFilm(t, e, "statements",
		[]media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "deu", false)},
		[]media.MediaStream{vid(""), aud(1, "deu", true), aud(2, "eng", false)})
	logs := traceReplicas(t, e)
	var got strings.Builder
	record := func(label string) {
		for _, r := range e.routes() {
			logs[0].take()
			r.ask(t, e, item)
			sent := logs[0].take()
			fmt.Fprintf(&got, "-- %s, %s\n", label, r.name)
			for _, sql := range sent {
				fmt.Fprintln(&got, oneLine(sql))
			}
			if n := count(sent, namesSource); strings.Contains(r.name, "PlaybackInfo") && n > len(sources) {
				t.Errorf("%s, %s: %d statements name media_sources for %d sources", label, r.name, n, len(sources))
			}
		}
	}
	record("transcoding off")
	transcodingOn(t, e)
	logs = traceReplicas(t, e)
	record("transcoding on")
	want, err := os.ReadFile("testdata/original-language-statements.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("the statements sent differ from testdata/original-language-statements.txt:\n%s", got.String())
	}
}
