//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
)

// JS-120: POST /jellyscale/v1/items/{itemId}/playback-info is PlaybackInfo with every track listed as itself, a Hidden
// mark on each audio and subtitle track (exactly the tracks Jellyfin's route lists as Type Data placeholders for the
// same request and settings, JS-119), and OriginalLanguage on each source. It is one handler with a different listing:
// the decision, the TranscodingUrl, the default indexes, the counters and the refusals are PlaybackInfo's. Fixtures and
// helpers are those of JS-119's checks (hide_tracks_integration_test.go).

const markedPath = "/jellyscale/v1/items/%s/playback-info"

// marked is the route's answer for a viewer, with the status.
func (f *langFixture) marked(t *testing.T, replica int, v viewer, film string, extra map[string]any) (int, doc) {
	t.Helper()
	body := map[string]any{"MaxStreamingBitrate": 2147483647}
	if f.profile != nil {
		body["DeviceProfile"] = f.profile
	}
	for k, val := range extra {
		body[k] = val
	}
	response, raw := f.do(t, replica, "POST", fmt.Sprintf(markedPath, film), v.token, body, "Authorization", f.auth(v))
	var reply doc
	if response.StatusCode == 200 {
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("%s: %v\n%s", v.name, err, raw)
		}
	}
	return response.StatusCode, reply
}

func (f *langFixture) markedOK(t *testing.T, replica int, v viewer, film string, extra map[string]any) doc {
	t.Helper()
	status, reply := f.marked(t, replica, v, film, extra)
	if status != 200 {
		t.Fatalf("%s: the marked playback info %v: %d", v.name, extra, status)
	}
	return reply
}

// hiddenAt is the Index of each stream marked Hidden true. It fails when an audio or subtitle stream has no boolean
// Hidden, or any other stream has one.
func hiddenAt(t *testing.T, label string, streams []doc) []int {
	t.Helper()
	out := []int{}
	for _, s := range streams {
		hidden, present := s["Hidden"]
		kind := s["Type"]
		if kind != "Audio" && kind != "Subtitle" {
			if present {
				t.Errorf("%s: the %v stream %v has Hidden %v", label, kind, s["Index"], hidden)
			}
			continue
		}
		b, ok := hidden.(bool)
		if !ok {
			t.Errorf("%s: the %v stream %v has Hidden %v, want a boolean", label, kind, s["Index"], hidden)
		}
		if b {
			out = append(out, int(s["Index"].(float64)))
		}
	}
	return out
}

// listedAsThemselves checks a marked source against a viewer with no settings' PlaybackInfo: the same streams at the
// same positions, each equal but for the Hidden mark, none of Type Data, and Hidden true at exactly the indexes the
// opted-in viewer's own PlaybackInfo lists as Type Data (which is Jellyfin's route's decision for this request).
func listedAsThemselves(t *testing.T, label string, marked, placeholders, plain doc, tokens []string) {
	t.Helper()
	m, p, base := streamsOf(t, marked["MediaStreams"]), streamsOf(t, placeholders["MediaStreams"]), streamsOf(t, plain["MediaStreams"])
	if len(m) != len(base) {
		t.Fatalf("%s: %d streams, a viewer with no settings is listed %d", label, len(m), len(base))
	}
	if data := dataAt(m); len(data) != 0 {
		t.Errorf("%s: Type Data at %v, want none", label, data)
	}
	if have, want := hiddenAt(t, label, m), dataAt(p); !slices.Equal(have, want) {
		t.Errorf("%s: Hidden at %v, but PlaybackInfo lists Type Data at %v for the same request", label, have, want)
	}
	for i := range m {
		if canon(without(m[i], "Hidden"), tokens[0]) != canon(base[i], tokens[1]) {
			t.Errorf("%s: stream %d is not listed as itself:\n%s\nwant\n%s", label, i, canon(without(m[i], "Hidden"), tokens[0]), canon(base[i], tokens[1]))
		}
	}
}

// The matrix of JS-119's checks, asked of the new route: 15 streams of Babel Film, none of Type Data, Hidden at the
// indexes that route lists as placeholders, and false on every other audio and subtitle stream. It includes the
// criteria's named cases: KeepSubtitleLanguages deu, both switches off, no settings.
func TestTheMarkedPlaybackInfoListsEveryTrackAsItselfAndMarksTheHiddenOnes(t *testing.T) {
	f := languagesEnv(t, true)
	plain := f.baseline(t)
	for i, c := range babelCases() {
		t.Run(c.name, func(t *testing.T) {
			film, types := f.filmOf(c.film)
			v := f.viewer(t, fmt.Sprintf("viewer%d", i), c.audio, c.subtitle, c.prefs)
			reply := f.markedOK(t, 0, v, film, nil)
			source := sourceOf(reply)
			if got := len(streamsOf(t, source["MediaStreams"])); got != len(types) {
				t.Fatalf("lists %d streams, want %d", got, len(types))
			}
			listedAsThemselves(t, "marked", source, sourceOf(f.info(t, 0, v, film, nil)), sourceOf(f.info(t, 0, plain, film, nil)), []string{v.token, plain.token})
			if have := hiddenAt(t, "marked", streamsOf(t, source["MediaStreams"])); !slices.Equal(have, c.data) {
				t.Errorf("Hidden at %v, want exactly %v", have, c.data)
			}
		})
	}
	t.Run("a viewer with no settings: Hidden false on every audio and subtitle stream", func(t *testing.T) {
		source := sourceOf(f.markedOK(t, 0, plain, f.babel, nil))
		if have := hiddenAt(t, "plain", streamsOf(t, source["MediaStreams"])); len(have) != 0 {
			t.Errorf("Hidden at %v", have)
		}
		if got := strings.Count(canon(source["MediaStreams"]), `"Hidden":false`); got != 14 {
			t.Errorf("%d streams are marked Hidden false, want the 14 audio and subtitle streams of Babel Film", got)
		}
	})
}

// A request naming a track, ShowAllTracks and both switches off list nothing as Hidden for what they name or cover.
func TestTheMarkedPlaybackInfoHidesWhatJellyfinsRouteHidesForTheSameRequestAndSettings(t *testing.T) {
	f := languagesEnv(t, true)
	plain := f.baseline(t)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	hidden := []int{2, 3, 8, 9, 10, 12, 13, 14}
	for _, c := range []struct {
		name  string
		extra map[string]any
		want  []int
	}{
		{"nothing named", nil, hidden},
		{"AudioStreamIndex 2 names the track", map[string]any{"AudioStreamIndex": 2}, []int{3, 8, 9, 10, 12, 13, 14}},
		{"SubtitleStreamIndex 8 names the track", map[string]any{"SubtitleStreamIndex": 8}, []int{2, 3, 9, 10, 12, 13, 14}},
		{"a name of the other kind or none names nothing", map[string]any{"AudioStreamIndex": 8, "SubtitleStreamIndex": 99}, hidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			source := sourceOf(f.markedOK(t, 0, v, f.babel, c.extra))
			listedAsThemselves(t, c.name, source, sourceOf(f.info(t, 0, v, f.babel, c.extra)), sourceOf(f.info(t, 0, plain, f.babel, c.extra)), []string{v.token, plain.token})
			if have := hiddenAt(t, c.name, streamsOf(t, source["MediaStreams"])); !slices.Equal(have, c.want) {
				t.Errorf("Hidden at %v, want %v", have, c.want)
			}
		})
	}
	t.Run("ShowAllTracks true, then false again", func(t *testing.T) {
		w := f.viewer(t, "showall", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepSubtitleLanguages":["deu"]}`)
		hiddenFor := func() []int {
			return hiddenAt(t, "showall", streamsOf(t, sourceOf(f.markedOK(t, 0, w, f.babel, nil))["MediaStreams"]))
		}
		if have, want := hiddenFor(), []int{2, 3, 10, 12, 14}; !slices.Equal(have, want) {
			t.Errorf("KeepSubtitleLanguages deu: Hidden at %v, want %v (false at 8, 9 and 13)", have, want)
		}
		f.patch(t, 0, w, `{"ShowAllTracks":true}`)
		if have := hiddenFor(); len(have) != 0 {
			t.Errorf("ShowAllTracks: Hidden at %v", have)
		}
		f.patch(t, 0, w, `{"ShowAllTracks":false}`)
		if have, want := hiddenFor(), []int{2, 3, 10, 12, 14}; !slices.Equal(have, want) {
			t.Errorf("ShowAllTracks false again: Hidden at %v, want %v", have, want)
		}
	})
	t.Run("the settings are read at once, on any replica", func(t *testing.T) {
		w := f.viewer(t, "replicas", "eng", "eng", bothOn)
		f.patch(t, 0, w, `{"HideOtherAudio":false}`)
		for replica := range f.servers {
			have := hiddenAt(t, "replica", streamsOf(t, sourceOf(f.markedOK(t, replica, w, f.babel, nil))["MediaStreams"]))
			if want := []int{8, 9, 10, 12, 13, 14}; !slices.Equal(have, want) {
				t.Errorf("replica %d: Hidden at %v, want %v", replica, have, want)
			}
		}
	})
}

// Hiding is not hiding from this route: a hidden text track has its DeliveryUrl and it answers 200.
func TestAHiddenTextTrackOfTheMarkedPlaybackInfoHasADeliveryUrlThatAnswers(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	streams := streamsOf(t, sourceOf(f.markedOK(t, 0, v, f.babel, nil))["MediaStreams"])
	for _, index := range []int{8, 9, 10, 13, 14} {
		s := streams[index]
		if s["Hidden"] != true || s["Type"] != "Subtitle" {
			t.Fatalf("stream %d is %v Hidden %v, this check needs a hidden subtitle", index, s["Type"], s["Hidden"])
		}
		delivery, _ := s["DeliveryUrl"].(string)
		if delivery == "" {
			t.Errorf("stream %d: no DeliveryUrl", index)
			continue
		}
		if response, raw := f.do(t, 0, "GET", delivery, "", nil); response.StatusCode != 200 {
			t.Errorf("stream %d: GET %s: %d %.200s", index, delivery, response.StatusCode, raw)
		}
	}
}

// Each source has OriginalLanguage, a string: the default audio's language, "" when that track is untagged. The field
// is present (as "") rather than left out.
func TestEachSourceOfTheMarkedPlaybackInfoHasItsOriginalLanguage(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	if got := sourceOf(f.markedOK(t, 0, v, f.babel, nil))["OriginalLanguage"]; got != "eng" {
		t.Errorf("Babel Film: OriginalLanguage = %v, want eng", got)
	}
	if got := sourceOf(f.markedOK(t, 0, v, f.kino, nil))["OriginalLanguage"]; got != "deu" {
		t.Errorf("Kino Film: OriginalLanguage = %v, want deu (its default audio)", got)
	}
	r := rowsEnv(t)
	w := r.viewer(t, "eng", "eng", "eng", bothOn)
	film, _ := addFilm(t, r.env, "Untagged default", []media.MediaStream{vid(""), aud(1, "", true), aud(2, "deu", false), aud(3, "eng", false)})
	source := sourceOf(r.markedOK(t, 0, w, film, nil))
	if got, present := source["OriginalLanguage"]; !present || got != "" {
		t.Errorf("a film whose default audio is untagged: OriginalLanguage = %v (present %v), want \"\"", got, present)
	}
	base := r.baseline(t)
	listedAsThemselves(t, "untagged default", source, sourceOf(r.info(t, 0, w, film, nil)), sourceOf(r.info(t, 0, base, film, nil)), []string{w.token, base.token})
	second := streamsOf(t, source["MediaStreams"])
	if have := hiddenAt(t, "untagged default", second); !slices.Equal(have, []int{2}) {
		t.Errorf("Hidden at %v, want [2]: the untagged default is kept, deu is not the viewer's and the original is unknown", have)
	}
}

// Each source has the language of its own default audio, not the item's: an item's OriginalLanguage is the first
// source's (GET /Items/{id}), and a build that stamps it on every source answers eng twice here. A film with no audio
// stream has "" (present), and so does a source of a film with sound whose own file has none.
func TestEachSourceOfAnItemWithTwoSourcesHasTheOriginalLanguageOfItsOwnDefaultAudio(t *testing.T) {
	r := rowsEnv(t)
	v := r.viewer(t, "eng", "eng", "eng", bothOn)
	film, ids := addFilm(t, r.env, "Two files",
		[]media.MediaStream{vid(""), aud(1, "eng", true), aud(2, "deu", false)},
		[]media.MediaStream{vid(""), aud(1, "fra", false), aud(2, "deu", true)},
		[]media.MediaStream{vid("")})
	want := map[string]string{ids[0]: "eng", ids[1]: "deu", ids[2]: ""}
	reply := r.markedOK(t, 0, v, film, nil)
	sources, _ := reply["MediaSources"].([]any)
	if len(sources) != 3 {
		t.Fatalf("%d sources, want 3", len(sources))
	}
	for _, raw := range sources {
		source := raw.(doc)
		id, _ := source["Id"].(string)
		got, present := source["OriginalLanguage"]
		if wantLanguage, known := want[id]; !known || !present || got != wantLanguage {
			t.Errorf("source %s: OriginalLanguage %v (present %v), want %q", id, got, present, wantLanguage)
		}
	}
	// With one source named, the answer is that source's own.
	for id, language := range want {
		one := sourceOf(r.markedOK(t, 0, v, film, map[string]any{"MediaSourceId": id}))
		if got, present := one["OriginalLanguage"]; !present || got != language {
			t.Errorf("source %s named: OriginalLanguage %v (present %v), want %q", id, got, present, language)
		}
	}
	none, _ := addFilm(t, r.env, "Silent", []media.MediaStream{vid("")})
	silent := sourceOf(r.markedOK(t, 0, v, none, nil))
	if got, present := silent["OriginalLanguage"]; !present || got != "" {
		t.Errorf("a film with no audio: OriginalLanguage %v (present %v), want \"\"", got, present)
	}
	if have := hiddenAt(t, "silent", streamsOf(t, silent["MediaStreams"])); len(have) != 0 {
		t.Errorf("a film with no audio: Hidden at %v", have)
	}
}

// Every other field of the answer is PlaybackInfo's for the same body and query.
func normalizedAnswer(t *testing.T, reply doc, token string) string {
	t.Helper()
	out := without(reply, "PlaySessionId", "MediaSources")
	var sources []any
	for _, raw := range reply["MediaSources"].([]any) {
		source := raw.(doc)
		rest := without(source, "OriginalLanguage", "TranscodingUrl", "MediaStreams")
		rest["TranscodingParams"] = transcodingParams(t, source["TranscodingUrl"], token)
		var streams []any
		for _, s := range streamsOf(t, source["MediaStreams"]) {
			streams = append(streams, without(s, "Hidden"))
		}
		rest["MediaStreams"] = streams
		sources = append(sources, rest)
	}
	out["MediaSources"] = sources
	return canon(out, token)
}

func (e *env) markedPost(t *testing.T, item string, body any) (int, []byte) {
	t.Helper()
	response, raw := e.do(t, 0, "POST", fmt.Sprintf(markedPath, item), e.token, body, "Authorization", strings.Replace(auth, "%s", e.token, 1))
	return response.StatusCode, raw
}

// For a direct play, a copy and a re-encode (and a live job joined, a replica with no store)
// the answer is PlaybackInfo's: the same decision, TranscodingUrl, defaults, and the decisions counter
// rises by one, under the same method. A refusal is PlaybackInfo's refusal and counts no decision.
func TestTheMarkedPlaybackInfoDecidesAsPlaybackInfoDoes(t *testing.T) {
	for _, b := range branches() {
		t.Run(b.name, func(t *testing.T) {
			e, source, _ := b.setup(t)
			body := b.body(t, source)
			before := decided(t)
			baseline := e.playbackInfo(t, 0, e.item, body)
			if b.want != "refused" {
				wantDecisions(t, "PlaybackInfo", before, map[string]float64{b.want: 1})
			}
			before = decided(t)
			status, raw := e.markedPost(t, db.CompactID(e.item), body)
			if status != 200 {
				t.Fatalf("%d %s", status, raw)
			}
			var reply doc
			if err := json.Unmarshal(raw, &reply); err != nil {
				t.Fatal(err)
			}
			if b.want == "refused" {
				if reply["ErrorCode"] != "RateLimitExceeded" {
					t.Errorf("not refused: %s", raw)
				}
				if sources, _ := reply["MediaSources"].([]any); len(sources) != 0 {
					t.Errorf("a refusal lists %d sources", len(sources))
				}
				wantDecisions(t, "a refusal", before, map[string]float64{})
			} else {
				wantDecisions(t, "the marked route", before, map[string]float64{b.want: 1})
				for _, raw := range reply["MediaSources"].([]any) {
					if _, ok := raw.(doc)["OriginalLanguage"].(string); !ok {
						t.Errorf("a source has no string OriginalLanguage: %v", raw.(doc)["OriginalLanguage"])
					}
				}
			}
			if got, want := normalizedAnswer2(t, listedOnly(t, reply, baseline), e.token), normalizedAnswer2(t, baseline, e.token); got != want {
				t.Errorf("not PlaybackInfo's answer:\n%s\nwant\n%s", got, want)
			}
			if b.want != "refused" {
				if hidden := hiddenAt(t, b.name, streamsOf(t, sourceOf(reply)["MediaStreams"])); len(hidden) != 0 {
					t.Errorf("a viewer with no settings has Hidden at %v", hidden)
				}
			}
		})
	}
}

// listedOnly is the marked reply with each source's streams cut to the ones PlaybackInfo lists for it, matched by
// Index: the route lists every stored track (a worker down or a replica that cannot transcode makes PlaybackInfo drop
// some), and a track PlaybackInfo drops must have no DeliveryUrl here. Every other field is compared whole.
func listedOnly(t *testing.T, reply, baseline doc) doc {
	t.Helper()
	got, _ := reply["MediaSources"].([]any)
	want, _ := baseline["MediaSources"].([]any)
	if len(got) == 0 || len(got) != len(want) {
		return reply
	}
	out := without(reply)
	var sources []any
	for i := range got {
		listed := map[any]bool{}
		for _, s := range streamsOf(t, want[i].(doc)["MediaStreams"]) {
			listed[s["Index"]] = true
		}
		source := without(got[i].(doc))
		var streams []any
		for _, s := range streamsOf(t, source["MediaStreams"]) {
			if listed[s["Index"]] {
				streams = append(streams, s)
			} else if s["DeliveryUrl"] != nil {
				t.Errorf("stream %v, which PlaybackInfo does not list here, has a DeliveryUrl", s["Index"])
			}
		}
		source["MediaStreams"] = streams
		sources = append(sources, source)
	}
	out["MediaSources"] = sources
	return out
}

// normalizedAnswer2 is normalizedAnswer that accepts a refusal, which lists no source.
func normalizedAnswer2(t *testing.T, reply doc, token string) string {
	t.Helper()
	if sources, _ := reply["MediaSources"].([]any); len(sources) == 0 {
		// JS-183 adds the contract's reason and action beside Jellyfin's ErrorCode on this route's refusal.
		return canon(without(reply, "PlaySessionId", "reason", "action"), token)
	}
	return normalizedAnswer(t, reply, token)
}

// An item with two sources lists both and counts a decision for each, as PlaybackInfo does.
func TestTheMarkedPlaybackInfoCountsOneDecisionPerSource(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	secondSource(t, e)
	body := map[string]any{"DeviceProfile": capturedProfile(t)}
	for _, c := range []struct {
		name   string
		body   map[string]any
		listed int
	}{{"both", body, 2}, {"one named", profileBody(t, source, nil), 1}} {
		before := decided(t)
		status, raw := e.markedPost(t, db.CompactID(e.item), c.body)
		if status != 200 {
			t.Fatalf("%s: %d %s", c.name, status, raw)
		}
		var reply doc
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatal(err)
		}
		if got := len(reply["MediaSources"].([]any)); got != c.listed {
			t.Fatalf("%s: %d sources, want %d", c.name, got, c.listed)
		}
		wantDecisions(t, c.name, before, map[string]float64{"reencode": float64(c.listed)})
	}
}

// The refusals are PlaybackInfo's: 404 for an unknown item (and for an id that is none), 400 for a bad body, 401 with no
// token or an unknown one.
func TestTheMarkedPlaybackInfoRefusesAsPlaybackInfoDoes(t *testing.T) {
	e, _, _ := transcodeEnv(t, "hevc", "aac")
	item := db.CompactID(e.item)
	ask := func(path string, token string, body any) int {
		response, _ := e.do(t, 0, "POST", path, token, body, "Authorization", strings.Replace(auth, "%s", token, 1))
		return response.StatusCode
	}
	for _, c := range []struct {
		name  string
		item  string
		token string
		body  any
		want  int
	}{
		{"an unknown item", db.CompactID(uuid.NewString()), e.token, map[string]any{}, 404},
		{"an id that is none", "not-an-id", e.token, map[string]any{}, 404},
		{"a body that is not JSON", item, e.token, "{", 400},
		{"a body that is not an object", item, e.token, []any{1}, 400},
		{"no token", item, "", map[string]any{}, 401},
		{"an unknown token", item, "no-such-token", map[string]any{}, 401},
	} {
		got := ask(fmt.Sprintf(markedPath, c.item), c.token, c.body)
		base := ask("/Items/"+c.item+"/PlaybackInfo", c.token, c.body)
		if got != c.want || base != c.want {
			t.Errorf("%s: the marked route answers %d, PlaybackInfo %d, want %d", c.name, got, base, c.want)
		}
	}
}

// Jellyfin's routes never carry the mark, for any settings, and the marked route does not make them: PlaybackInfo's
// POST and GET, an item's details and its alias, asked before and after the marked route.
func TestJellyfinsRoutesNeverCarryAHiddenField(t *testing.T) {
	f := languagesEnv(t, true)
	viewers := []viewer{f.baseline(t)}
	for i, c := range babelCases()[:4] {
		viewers = append(viewers, f.viewer(t, fmt.Sprintf("marked%d", i), c.audio, c.subtitle, c.prefs))
	}
	for _, v := range viewers {
		f.markedOK(t, 0, v, f.babel, nil)
		answers := map[string]string{
			"POST PlaybackInfo": canon(f.info(t, 0, v, f.babel, nil)),
			"item":              canon(f.item(t, 0, v, f.babel, false)),
			"item alias":        canon(f.item(t, 0, v, f.babel, true)),
		}
		response, raw := f.do(t, 0, "GET", "/Items/"+f.babel+"/PlaybackInfo?UserId="+db.CompactID(v.id), v.token, nil, "Authorization", f.auth(v))
		if response.StatusCode != 200 {
			t.Fatalf("GET PlaybackInfo: %d %s", response.StatusCode, raw)
		}
		answers["GET PlaybackInfo"] = string(raw)
		for name, text := range answers {
			if strings.Contains(text, `"Hidden"`) {
				t.Errorf("%s: %s carries a Hidden field", v.name, name)
			}
		}
	}
}

// Fluster's mpv plays the original file and sees every track in it, whatever the server can deliver, and maps them
// to Jellyfin's indexes partly by order. So the route lists every stored track and marks it in every mode, even when
// a worker is down or the replica cannot transcode; a track PlaybackInfo cannot deliver there is listed without a
// DeliveryUrl, never dropped (lead, 2026-11-16).
func TestTheMarkedPlaybackInfoListsEveryTrackWhateverTheReplicaCanOffer(t *testing.T) {
	for _, mode := range []string{"a worker running", "a replica with transcoding off", "no worker alive"} {
		t.Run(mode, func(t *testing.T) {
			f := languagesEnv(t, mode != "a replica with transcoding off")
			v := f.viewer(t, "eng", "eng", "eng", bothOn)
			all := f.viewer(t, "all", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`)
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
			for _, extra := range []map[string]any{nil, {"AudioStreamIndex": 2}, {"SubtitleStreamIndex": 10}} {
				// Where no worker can deliver track 2 the pick is refused, not answered (D-029; TestAPickThatCannotBeDelivered...).
				if _, picked := extra["AudioStreamIndex"]; picked && mode != "a worker running" {
					continue
				}
				got := streamsOf(t, sourceOf(f.markedOK(t, 0, v, f.babel, extra))["MediaStreams"])
				if len(got) != len(babelTypes) {
					t.Fatalf("%v: %d streams, the file has %d", extra, len(got), len(babelTypes))
				}
				for i, s := range got {
					if s["Type"] != babelTypes[i] {
						t.Errorf("%v: stream %d is %v, the file's is %s", extra, i, s["Type"], babelTypes[i])
					}
				}
				want := []int{2, 3, 8, 9, 10, 12, 13, 14}
				if idx, ok := extra["AudioStreamIndex"]; ok {
					want = slices.DeleteFunc(slices.Clone(want), func(n int) bool { return n == idx })
				}
				if idx, ok := extra["SubtitleStreamIndex"]; ok {
					want = slices.DeleteFunc(slices.Clone(want), func(n int) bool { return n == idx })
				}
				if h := hiddenAt(t, mode, got); !slices.Equal(h, want) {
					t.Errorf("%v: Hidden at %v, want %v", extra, h, want)
				}
				// A stream PlaybackInfo lists (with ShowAllTracks) is listed the same here; one it drops has no DeliveryUrl.
				listed := map[float64]doc{}
				for _, s := range streamsOf(t, sourceOf(f.info(t, 0, all, f.babel, extra))["MediaStreams"]) {
					listed[s["Index"].(float64)] = s
				}
				for _, s := range got {
					if w, ok := listed[s["Index"].(float64)]; ok {
						if canon(without(s, "Hidden"), v.token) != canon(w, all.token) {
							t.Errorf("%v: stream %v is not the one PlaybackInfo lists:\n%s\nwant\n%s", extra, s["Index"], canon(without(s, "Hidden"), v.token), canon(w, all.token))
						}
					} else if s["DeliveryUrl"] != nil {
						t.Errorf("%v: stream %v, which PlaybackInfo cannot deliver here, has a DeliveryUrl", extra, s["Index"])
					}
				}
			}
		})
	}
}

// The marks come from the token's user: a UserId naming another viewer, in the query or the body, is refused with 403
// and changes nothing (JS-127, D-075); naming the caller, in any spelling, answers the caller's own marks.
func TestTheMarkedPlaybackInfoRefusesAUserIdThatNamesAnotherViewerAndReadsTheCallers(t *testing.T) {
	f := languagesEnv(t, true)
	mine := f.viewer(t, "eng", "eng", "eng", bothOn)
	other := f.viewer(t, "stranger", "deu", "deu", bothOn)
	plain := f.baseline(t)
	want := hiddenAt(t, "own", streamsOf(t, sourceOf(f.markedOK(t, 0, mine, f.babel, nil))["MediaStreams"]))
	if !slices.Equal(want, []int{2, 3, 8, 9, 10, 12, 13, 14}) {
		t.Fatalf("own marks %v", want)
	}
	ask := func(where string, id string) (int, doc) {
		if where == "body" {
			return f.marked(t, 0, mine, f.babel, map[string]any{"UserId": id})
		}
		response, raw := f.do(t, 0, "POST", fmt.Sprintf(markedPath, f.babel)+"?UserId="+id, mine.token,
			map[string]any{"MaxStreamingBitrate": 2147483647, "DeviceProfile": f.profile}, "Authorization", f.auth(mine))
		var reply doc
		if response.StatusCode == 200 {
			_ = json.Unmarshal(raw, &reply)
		}
		return response.StatusCode, reply
	}
	for _, named := range []viewer{other, plain} {
		for _, where := range []string{"body", "query"} {
			if status, _ := ask(where, db.CompactID(named.id)); status != 403 {
				t.Errorf("%s names %s: %d, want 403", where, named.name, status)
			}
		}
	}
	for _, id := range []string{db.CompactID(mine.id), mine.id, strings.ToUpper(db.CompactID(mine.id)), strings.ToUpper(mine.id)} {
		for _, where := range []string{"body", "query"} {
			status, reply := ask(where, id)
			if status != 200 {
				t.Errorf("%s names the caller (%s): %d", where, id, status)
				continue
			}
			if have := hiddenAt(t, where, streamsOf(t, sourceOf(reply)["MediaStreams"])); !slices.Equal(have, want) {
				t.Errorf("a UserId in the %s naming the caller (%s): Hidden at %v, want %v", where, id, have, want)
			}
		}
	}
}

// OriginalLanguage is what GET /Items/{id} answers for the film, and PlaybackInfo's sources never carry it; neither
// does a Hidden mark ever leave this route, whatever the viewer's settings (including ShowAllTracks and
// KeepSubtitleLanguages).
func TestOriginalLanguageIsTheItemsAndOnlyTheMarkedRouteCarriesIt(t *testing.T) {
	f := languagesEnv(t, true)
	viewers := []viewer{f.baseline(t),
		f.viewer(t, "both", "eng", "eng", bothOn),
		f.viewer(t, "keep", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepSubtitleLanguages":["deu"]}`),
		f.viewer(t, "showall", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`)}
	for _, v := range viewers {
		for _, film := range []string{f.babel, f.kino} {
			want := f.item(t, 0, v, film, false)["OriginalLanguage"]
			if got := sourceOf(f.markedOK(t, 0, v, film, nil))["OriginalLanguage"]; got != want || got == nil {
				t.Errorf("%s: OriginalLanguage %v, GET /Items answers %v", v.name, got, want)
			}
			if _, present := sourceOf(f.info(t, 0, v, film, nil))["OriginalLanguage"]; present {
				t.Errorf("%s: PlaybackInfo's source carries OriginalLanguage", v.name)
			}
			if text := canon(f.item(t, 0, v, film, true)); strings.Contains(text, `"Hidden"`) {
				t.Errorf("%s: the item alias carries Hidden", v.name)
			}
		}
	}
}

// The route's path is matched without regard to case, as the namespace's other routes are.
func TestTheMarkedPlaybackInfoPathIsMatchedWithoutRegardToCase(t *testing.T) {
	f := languagesEnv(t, true)
	v := f.viewer(t, "eng", "eng", "eng", bothOn)
	path := strings.ToUpper(fmt.Sprintf(markedPath, db.CompactID(f.babel)))
	response, raw := f.do(t, 0, "POST", path, v.token, map[string]any{"DeviceProfile": f.profile}, "Authorization", f.auth(v))
	if response.StatusCode != 200 {
		t.Fatalf("POST %s: %d %.200s", path, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if have := hiddenAt(t, "upper case", streamsOf(t, sourceOf(reply)["MediaStreams"])); len(have) != 8 {
		t.Errorf("Hidden at %v", have)
	}
}
