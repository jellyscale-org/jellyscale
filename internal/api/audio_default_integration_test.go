//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// JS-121: PlaybackInfo's DefaultAudioStreamIndex is chosen from the viewer's saved AudioLanguagePreference and
// PlayDefaultAudioTrack, as Jellyfin 12.4 does (compat/captures/audio-default-12.4.md, rules 1 to 4 and the named
// index), and a default that is not the file's own track turns a browser's direct play into a copy of the video with
// that audio track (DOMAINS invariant 45). The films are rows with H.264 video, a keyframe index and a worker row, so a
// copy can be offered and none is made; the stream itself is the e2e scenario's. Where the checks say "the file's
// default" they mean the track a browser plays: the first flagged one, else the first.

type audioFixture struct{ *langFixture }

func audioEnv(t *testing.T) *audioFixture {
	t.Helper()
	f := rowsEnv(t)
	f.profile = capturedProfile(t)
	return &audioFixture{f}
}

// A track of a film made as a row: its tag as the file wrote it, and the default flag.
type track struct {
	lang string
	def  bool
}

func eng(def bool) track { return track{"eng", def} }
func deu(def bool) track { return track{"deu", def} }

// film puts a film in the library with one source per list of tracks, as a scan writes it: the item lists the video, the
// file's default audio track and the subtitles, the source row every stream, and a keyframe index so a copy can be
// served. The audio tracks are Index 1 to n, an English and a German subtitle follow. It returns the item's id and its
// sources' ids.
func (f *audioFixture) film(t *testing.T, name string, sources ...[]track) (string, []string) {
	t.Helper()
	item := uuid.NewString()
	library := scalar[string](t, f.pool, "SELECT id::text FROM libraries LIMIT 1")
	mustExec(t, f.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', $3, lower($3), lower($3), 900000000, '{}')`, item, library, name)
	var dtoSources []media.MediaSource
	var ids []string
	for _, tracks := range sources {
		source := uuid.NewString()
		ids = append(ids, db.CompactID(source))
		all := []media.MediaStream{{Index: 0, Type: "Video", Codec: "h264", Profile: "Main", Level: 41, VideoRangeType: "SDR", Width: 1280, Height: 720, AverageFrameRate: 24}}
		for i, tr := range tracks {
			all = append(all, media.MediaStream{Index: i + 1, Type: "Audio", Codec: "aac", Channels: 2, Language: tr.lang, IsDefault: tr.def})
		}
		for i, lang := range []string{"eng", "deu"} {
			all = append(all, sub(len(tracks)+1+i, lang, false, false))
		}
		dflt := -1
		for i, tr := range tracks {
			if dflt == -1 || (tr.def && !tracks[dflt].def) {
				dflt = i
			}
		}
		listed := []media.MediaStream{}
		for _, s := range all {
			if s.Type != "Audio" || s.Index == dflt+1 {
				listed = append(listed, s)
			}
		}
		dtoSources = append(dtoSources, media.MediaSource{ID: db.CompactID(source), Protocol: "File", Container: "mkv", Bitrate: 2000000,
			SupportsTranscoding: true, SupportsDirectPlay: true, SupportsDirectStream: true, MediaStreams: listed})
		raw, _ := json.Marshal(all)
		mustExec(t, f.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end)
			VALUES ($1, $2, 'film.mkv', 25000000, 'abc123', 'mkv', $3, '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', 900000000)`,
			source, item, raw)
	}
	dto, _ := json.Marshal(map[string]any{"Name": name, "MediaSources": dtoSources})
	mustExec(t, f.pool, "UPDATE items SET dto = $2 WHERE id = $1", item, dto)
	return db.CompactID(item), ids
}

// fileDefault is the audio track a browser plays: the first flagged, else the first. 0 when there is none.
func fileDefault(tracks []track) int {
	for i, tr := range tracks {
		if tr.def {
			return i + 1
		}
	}
	if len(tracks) > 0 {
		return 1
	}
	return 0
}

// listener is a viewer with the two settings saved; "" saves no language, as jellyfin-web posts a viewer who chose none.
func (f *audioFixture) listener(t *testing.T, name, language string, play bool) viewer {
	t.Helper()
	id, token := f.addUser(t, name)
	v := viewer{id, token, name}
	f.saveAudio(t, 0, v, language, play)
	return v
}

func (f *audioFixture) saveAudio(t *testing.T, replica int, v viewer, language string, play bool) {
	t.Helper()
	body := map[string]any{"PlayDefaultAudioTrack": play}
	if language != "" {
		body["AudioLanguagePreference"] = language
	}
	raw, _ := json.Marshal(body)
	if status := f.save(t, replica, "path", v.id, v.token, string(raw)).StatusCode; status != 204 {
		t.Fatalf("saving %s's settings %s: %d", v.name, raw, status)
	}
}

// ask is PlaybackInfo as jellyfin-web asks it, or with the body the caller gives (no DeviceProfile: "plain").
func (f *audioFixture) ask(t *testing.T, replica int, v viewer, film string, extra map[string]any) doc {
	t.Helper()
	return f.info(t, replica, v, film, extra)
}

func (f *audioFixture) plain(t *testing.T, replica int, v viewer, film string, extra map[string]any) doc {
	t.Helper()
	body := map[string]any{}
	for k, val := range extra {
		body[k] = val
	}
	response, raw := f.do(t, replica, "POST", "/Items/"+film+"/PlaybackInfo", v.token, body, "Authorization", f.auth(v))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: PlaybackInfo with no profile: %d %s", v.name, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func (f *audioFixture) get(t *testing.T, replica int, v viewer, film string) doc {
	t.Helper()
	response, raw := f.do(t, replica, "GET", "/Items/"+film+"/PlaybackInfo?UserId="+db.CompactID(v.id), v.token, nil, "Authorization", f.auth(v))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: GET PlaybackInfo: %d %s", v.name, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// chosen is DefaultAudioStreamIndex of a source: -2 when the reply has none.
func chosen(source doc) int {
	value, ok := source["DefaultAudioStreamIndex"].(float64)
	if !ok {
		return -2
	}
	return int(value)
}

func sourceNamed(t *testing.T, reply doc, id string) doc {
	t.Helper()
	for _, s := range reply["MediaSources"].([]any) {
		if s.(doc)["Id"] == id {
			return s.(doc)
		}
	}
	t.Fatalf("no source %s in %v", id, reply)
	return nil
}

// copyOf says how a source plays: "direct" when it can be direct played and has no TranscodingUrl, else the video
// handling of its TranscodingUrl ("copy", or the codec a re-encode names), with the audio track it carries.
func copyOf(t *testing.T, source doc) (how string, audio int) {
	t.Helper()
	text, _ := source["TranscodingUrl"].(string)
	if text == "" {
		if source["SupportsDirectPlay"] != true {
			t.Errorf("no TranscodingUrl and no direct play: %v", source)
		}
		return "direct", -2
	}
	if source["SupportsDirectPlay"] != false || source["SupportsDirectStream"] != false {
		t.Errorf("a TranscodingUrl with direct play still offered: %v", source)
	}
	parsed, err := url.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	query := lowerKeys(parsed.Query())
	if _, err := fmt.Sscan(query.Get("audiostreamindex"), &audio); err != nil {
		t.Errorf("TranscodingUrl %s has no AudioStreamIndex", text)
	}
	if query.Get("videocodec") == "copy" {
		return "copy", audio
	}
	return "reencode", audio
}

// problems collects the failures of a loop over many cells, and reports their number and the first few.
type problems []string

func (p *problems) addf(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func (p *problems) report(t *testing.T) {
	t.Helper()
	if len(*p) > 0 {
		t.Errorf("%d answers are wrong; the first:\n%s", len(*p), strings.Join((*p)[:min(len(*p), 10)], "\n"))
	}
}

// ---- The capture, replayed.

// The cells where jellyscale answers another index than Jellyfin 12.4, by the name the named exceptions held in the replay test lists them
// under, and the index jellyscale answers (audio-default-12.4.md, "What jellyscale does"). A cell is only ever excused
// when it is in this table and in the file with a reason; the file may not hold more.
var audioDefaultDifferences = func() map[string]int {
	out := map[string]int{}
	for _, film := range []string{"Audio Short De", "Audio Region De"} {
		for _, saved := range []string{"deu", "ger", "de", "DEU"} {
			out[cellName(film, saved, false)] = 2
		}
	}
	for _, saved := range []string{"und", "xx-not-a-tag"} {
		out[cellName("Audio Dup Second", saved, false)] = 3
	}
	return out
}()

// The cells asked with no DeviceProfile where jellyscale answers another index than Jellyfin 12.4, listed in
// the named exceptions held in the replay test under noProfileName. Jellyfin answers these 6 one way with a profile and another without
// (audio-default-12.4.md); jellyscale answers a request with no profile as it answers one with, so they differ here.
var noProfileDifferences = func() map[string]int {
	out := map[string]int{}
	for _, saved := range []string{"deu", "OriginalLanguage", "ger", "de", "DEU", "fra"} {
		out[noProfileName("Audio Dup Second", saved, false)] = 2
	}
	return out
}()

func noProfileName(film, saved string, play bool) string {
	return cellName(film, saved, play) + " | no profile"
}

func cellName(film, saved string, play bool) string {
	if saved == "" {
		saved = "(none)"
	}
	return fmt.Sprintf("audio default: %s | %s | PlayDefaultAudioTrack %v", film, saved, play)
}

// A captured film as rows: the tags as the file wrote them (ffprobe's, which is what jellyscale stores).
func captureTracks(audio []audioRow) []track {
	var out []track
	for _, a := range audio {
		tr := track{def: a.IsDefault}
		if a.Language != nil {
			tr.lang = *a.Language
		}
		out = append(out, tr)
	}
	return out
}

type audioRow struct {
	Index     int
	Language  *string
	IsDefault bool
}

type auditCapture struct {
	Films []struct {
		Name  string
		Audio []audioRow
		Cells []struct {
			AudioLanguagePreference string
			PlayDefaultAudioTrack   bool
			DefaultAudioStreamIndex any
		}
		// The same cells asked with no DeviceProfile.
		WithoutProfile []struct {
			AudioLanguagePreference string
			PlayDefaultAudioTrack   bool
			DefaultAudioStreamIndex any
		}
	}
	Named []struct {
		Film                    string
		AudioLanguagePreference string
		PlayDefaultAudioTrack   bool
		AudioStreamIndex        any
		DefaultAudioStreamIndex any
	}
	// What the item (jellyfin-web's details page) names, and what Jellyfin answers a client whose DeviceProfile has no
	// IsSecondaryAudio condition (Fluster's mpv plays any track itself): the same index, and always a direct play.
	Details []struct {
		Film                    string
		AudioLanguagePreference string
		PlayDefaultAudioTrack   bool
		DefaultAudioStreamIndex any
		AudioStreamIndexes      []int `json:"audioStreamIndexes"`
	}
	AnyClientProfile json.RawMessage
	AnyClient        []anyClientCell
	AnyClientNamed   []anyClientCell
}

type anyClientCell struct {
	Film                    string
	AudioLanguagePreference string
	PlayDefaultAudioTrack   bool
	AudioStreamIndex        any
	DefaultAudioStreamIndex any
	SupportsDirectPlay      bool
	TranscodingUrl          any
}

func loadAudioCapture(t *testing.T) auditCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/audio-default-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture auditCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

func wantIndex(captured any) int {
	if n, ok := captured.(float64); ok {
		return int(n)
	}
	return -2 // "<absent>"
}

// Every captured cell, on GET and on POST with jellyfin-web's body (the profile of transcode-12.4.json), is answered as
// Jellyfin 12.4 answered it, except the cells audioDefaultDifferences names, which jellyscale answers otherwise on
// purpose. A request with no profile is held to Jellyfin's answer
// with no profile, except the cells noProfileDifferences lists, where it is answered as one with a profile.
func TestPlaybackInfoNamesTheDefaultAudioJellyfinNamesInEveryCapturedCell(t *testing.T) {
	f := audioEnv(t)
	capture := loadAudioCapture(t)
	viewers := map[string]viewer{}
	var wrong []string
	cells := 0
	for _, film := range capture.Films {
		item, _ := f.film(t, film.Name, captureTracks(film.Audio))
		if len(film.WithoutProfile) != len(film.Cells) {
			t.Fatalf("%s: %d cells with no profile, %d with one", film.Name, len(film.WithoutProfile), len(film.Cells))
		}
		for i, cell := range film.Cells {
			plain := film.WithoutProfile[i]
			if plain.AudioLanguagePreference != cell.AudioLanguagePreference || plain.PlayDefaultAudioTrack != cell.PlayDefaultAudioTrack {
				t.Fatalf("%s: cell %d with no profile is %q %v, with one %q %v", film.Name, i, plain.AudioLanguagePreference, plain.PlayDefaultAudioTrack, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
			}
			key := fmt.Sprint(cell.AudioLanguagePreference, "|", cell.PlayDefaultAudioTrack)
			v, ok := viewers[key]
			if !ok {
				v = f.listener(t, fmt.Sprintf("cell%d", len(viewers)), cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
				viewers[key] = v
			}
			name := cellName(film.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
			want := wantIndex(cell.DefaultAudioStreamIndex)
			if index, differs := audioDefaultDifferences[name]; differs {
				want = index
			}
			// With no profile the capture is Jellyfin's own answer to that request, excused only as noProfileDifferences says.
			wantPlain := wantIndex(plain.DefaultAudioStreamIndex)
			if _, differs := audioDefaultDifferences[name]; differs {
				wantPlain = want
			}
			if index, differs := noProfileDifferences[noProfileName(film.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)]; differs {
				wantPlain = index
			}
			cells++
			for _, how := range []string{"POST", "GET", "POST with no profile"} {
				reply := map[string]doc{"POST": f.ask(t, 0, v, item, nil), "GET": f.get(t, 1, v, item), "POST with no profile": f.plain(t, 0, v, item, nil)}[how]
				w, answered := want, cell.DefaultAudioStreamIndex
				if how == "POST with no profile" {
					w, answered = wantPlain, plain.DefaultAudioStreamIndex
				}
				if got := chosen(sourceOf(reply)); got != w {
					wrong = append(wrong, fmt.Sprintf("%s, %s: DefaultAudioStreamIndex %d, want %d (Jellyfin 12.4 answered %v)", name, how, got, w, answered))
				}
			}
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d answers of %d cells are not the captured index (3 requests each); the first:\n%s", len(wrong), cells, strings.Join(wrong[:min(len(wrong), 12)], "\n"))
	}
}

// What the capture records when the request names AudioStreamIndex: a named audio track is the answer, whatever is
// saved; a named index that is no audio stream (a subtitle, the video, 99, -1) is ignored with the saved language, and the
// file's default answers. The decision follows the answer: the file's own track plays directly, any other is a copy.
func TestAnAudioStreamIndexTheRequestNamesIsAnsweredAsJellyfinAnswersIt(t *testing.T) {
	f := audioEnv(t)
	capture := loadAudioCapture(t)
	films := map[string]string{}
	tracks := map[string][]track{}
	for _, film := range capture.Films {
		tracks[film.Name] = captureTracks(film.Audio)
		films[film.Name], _ = f.film(t, film.Name, tracks[film.Name])
	}
	viewers := map[string]viewer{}
	var wrong []string
	for _, n := range capture.Named {
		key := fmt.Sprint(n.AudioLanguagePreference, "|", n.PlayDefaultAudioTrack)
		v, ok := viewers[key]
		if !ok {
			v = f.listener(t, fmt.Sprintf("named%d", len(viewers)), n.AudioLanguagePreference, n.PlayDefaultAudioTrack)
			viewers[key] = v
		}
		extra := map[string]any{}
		if n.AudioStreamIndex != nil {
			extra["AudioStreamIndex"] = n.AudioStreamIndex
		}
		source := sourceOf(f.ask(t, 0, v, films[n.Film], extra))
		want := wantIndex(n.DefaultAudioStreamIndex)
		label := fmt.Sprintf("%s, saved %q, PlayDefaultAudioTrack %v, AudioStreamIndex %v", n.Film, n.AudioLanguagePreference, n.PlayDefaultAudioTrack, n.AudioStreamIndex)
		if got := chosen(source); got != want {
			wrong = append(wrong, fmt.Sprintf("%s: DefaultAudioStreamIndex %d, want %d", label, got, want))
			continue
		}
		how, audio := copyOf(t, source)
		if want == fileDefault(tracks[n.Film]) && how != "direct" {
			wrong = append(wrong, fmt.Sprintf("%s: the file's own track %d plays as %s, want a direct play", label, want, how))
		}
		if want != fileDefault(tracks[n.Film]) && (how != "copy" || audio != want) {
			wrong = append(wrong, fmt.Sprintf("%s: track %d, not the file's %d, plays as %s with audio %d, want a copy of it", label, want, fileDefault(tracks[n.Film]), how, audio))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d of %d named-index answers are wrong; the first:\n%s", len(wrong), len(capture.Named), strings.Join(wrong[:min(len(wrong), 12)], "\n"))
	}
}

// ---- The language of a track, by language.

// With deu saved and PlayDefaultAudioTrack off, a track in German is chosen whatever its tag, and a track with no language
// never is; a saved value that names no language is no language saved.
func TestLanguagesAreComparedByLanguageNotBySpelling(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	german := []string{"deu", "ger", "de", "DE", "GER", "Deu", "de-DE", "de-AT", "deu-DE", "de-CH"}
	savedGerman := []string{"deu", "ger", "de", "DE", "GER", "de-DE", "deu-DE"}
	viewers := map[string]viewer{}
	listener := func(language string, play bool) viewer {
		key := fmt.Sprint(language, play)
		if _, ok := viewers[key]; !ok {
			viewers[key] = f.listener(t, fmt.Sprintf("spelling%d", len(viewers)), language, play)
		}
		return viewers[key]
	}
	for _, tag := range german {
		film, _ := f.film(t, "German as "+tag+" "+uuid.NewString()[:6], []track{eng(true), {tag, false}})
		for _, saved := range savedGerman {
			if got := chosen(sourceOf(f.ask(t, 0, listener(saved, false), film, nil))); got != 2 {
				bad.addf("track tagged %q, saved %q, PlayDefaultAudioTrack off: DefaultAudioStreamIndex %d, want 2 (the track in German, as if tagged deu)", tag, saved, got)
			}
		}
		// With PlayDefaultAudioTrack on and a flagged track, only the flagged tracks are looked at (rule 2).
		if got := chosen(sourceOf(f.ask(t, 0, listener("deu", true), film, nil))); got != 1 {
			bad.addf("track tagged %q, saved deu, PlayDefaultAudioTrack on: %d, want the flagged track 1", tag, got)
		}
		// The same two tags the other way round: the German track flagged, the saved language what the tag names.
		flagged, _ := f.film(t, "Flagged German as "+tag+" "+uuid.NewString()[:6], []track{eng(false), {tag, true}})
		if got := chosen(sourceOf(f.ask(t, 0, listener("deu", true), flagged, nil))); got != 2 {
			bad.addf("flagged track tagged %q, saved deu, PlayDefaultAudioTrack on: %d, want 2", tag, got)
		}
	}
	// A track that names no language is never the saved one, however the saved one is spelled, itself included.
	for _, none := range []string{"", "und", "mis", "mul", "zxx", "qaa", "qtz", "!!not-a-tag", "x"} {
		film, _ := f.film(t, "Nameless "+none+" "+uuid.NewString()[:6], []track{eng(true), {none, false}})
		for _, saved := range []string{"deu", "und", "mis", none} {
			if got := chosen(sourceOf(f.ask(t, 0, listener(saved, false), film, nil))); got != 1 {
				bad.addf("a track tagged %q, saved %q: DefaultAudioStreamIndex %d, want the flagged English track 1: a track with no language matches no language", none, saved, got)
			}
		}
	}
	// A saved value that names no language is no language saved: the flagged track, whatever the tracks are tagged.
	film, _ := f.film(t, "Two German", []track{eng(false), deu(false), deu(true)})
	for _, saved := range []string{"", "und", "mis", "mul", "zxx", "qaa", "!!not-a-tag", "xx-not-a-tag"} {
		for _, play := range []bool{true, false} {
			if got := chosen(sourceOf(f.ask(t, 0, listener(saved, play), film, nil))); got != 3 {
				bad.addf("saved %q, PlayDefaultAudioTrack %v: DefaultAudioStreamIndex %d, want the flagged track 3 (nothing saved)", saved, play, got)
			}
		}
	}
}

// generated films: 1 to 4 audio tracks, tags in several spellings and none, flags on none, some or all.
func generatedTracks(rng *rand.Rand) []track {
	tags := []string{"eng", "en", "en-US", "deu", "ger", "de", "de-DE", "fra", "fre", "spa", "jpn", "und", "mis", "", "nor", "nob"}
	out := make([]track, rng.Intn(4)+1)
	for i := range out {
		out[i] = track{tags[rng.Intn(len(tags))], rng.Intn(3) == 0}
	}
	return out
}

// audioRule is the rule of compat/captures/audio-default-12.4.md as jellyscale reads it: languages by language
// (playback.SameLanguage), a saved value that names no language as none saved, and OriginalLanguage as the tag of the
// file's default track. It returns the index, or -2 for a film with no audio. The oracle of the generated films.
func audioRule(tracks []track, saved string, play bool) int {
	if len(tracks) == 0 {
		return -2
	}
	var flagged []int
	for i, tr := range tracks {
		if tr.def {
			flagged = append(flagged, i)
		}
	}
	x := 0
	if len(flagged) > 0 {
		x = flagged[0]
	}
	if saved == "OriginalLanguage" {
		saved = tracks[x].lang
	}
	if _, names := playback.LanguageCode(saved); !names {
		saved = ""
	}
	matches := func(i int) bool { return saved != "" && playback.SameLanguage(tracks[i].lang, saved) }
	firstOf := func(list []int, keep func(int) bool) (int, bool) {
		for _, i := range list {
			if keep(i) {
				return i, true
			}
		}
		return 0, false
	}
	all := make([]int, len(tracks))
	for i := range all {
		all[i] = i
	}
	if play && len(flagged) > 0 {
		if i, ok := firstOf(flagged, matches); ok {
			return i + 1
		}
		return flagged[0] + 1
	}
	if i, ok := firstOf(all, matches); ok {
		return i + 1
	}
	if saved != "" {
		if i, ok := firstOf(all, func(i int) bool { return playback.SameLanguage(tracks[i].lang, tracks[x].lang) }); ok {
			return i + 1
		}
	}
	return x + 1
}

// For generated films and every saved language, jellyscale answers what the rule says, which is the rule Jellyfin 12.4
// follows (every captured cell is that rule's too: TestTheAudioDefaultCaptureFitsTheRuleWrittenDown).
func TestGeneratedFilmsAreAnsweredByTheRuleForEverySavedLanguage(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	rng := rand.New(rand.NewSource(12100))
	t.Logf("seed 12100")
	var viewers []viewer
	var settings [][2]any
	for i, saved := range []string{"", "eng", "en", "deu", "ger", "de-DE", "fra", "jpn", "OriginalLanguage", "und", "xx-not-a-tag"} {
		for _, play := range []bool{true, false} {
			viewers = append(viewers, f.listener(t, fmt.Sprintf("rule%d%v", i, play), saved, play))
			settings = append(settings, [2]any{saved, play})
		}
	}
	moved := 0
	for n := range 70 {
		tracks := generatedTracks(rng)
		film, _ := f.film(t, fmt.Sprintf("Rule %d", n), tracks)
		for i, v := range viewers {
			saved, play := settings[i][0].(string), settings[i][1].(bool)
			want := audioRule(tracks, saved, play)
			if want != audioRule(tracks, "", play) {
				moved++
			}
			if got := chosen(sourceOf(f.ask(t, 0, v, film, nil))); got != want {
				bad.addf("film %v, saved %q, PlayDefaultAudioTrack %v: DefaultAudioStreamIndex %d, want %d", tracks, saved, play, got, want)
			}
		}
	}
	if moved < 100 {
		t.Errorf("a language moved the default in %d cases of the generated films; the property is not tested", moved)
	}
}

// OriginalLanguage is the film's original language, which is that of its default audio track as the file wrote the tag
// (JS-117): saving it answers what saving that tag answers, for every film and both values of PlayDefaultAudioTrack. A
// film whose default audio track names no language answers as if no language were saved.
func TestOriginalLanguageAnswersWhatSavingTheFilmsOwnTagAnswers(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	rng := rand.New(rand.NewSource(121))
	t.Logf("seed 121")
	viewers := map[string]viewer{}
	listener := func(language string, play bool) viewer {
		key := fmt.Sprint(language, "|", play)
		if _, ok := viewers[key]; !ok {
			viewers[key] = f.listener(t, fmt.Sprintf("original%d", len(viewers)), language, play)
		}
		return viewers[key]
	}
	namesNone := 0
	for n := range 150 {
		tracks := generatedTracks(rng)
		film, _ := f.film(t, fmt.Sprintf("Generated %d", n), tracks)
		original := tracks[fileDefault(tracks)-1].lang // The default audio track's tag, as the file wrote it.
		_, names := playback.LanguageCode(original)
		if !names {
			original, namesNone = "", namesNone+1
		}
		for _, play := range []bool{true, false} {
			got := chosen(sourceOf(f.ask(t, 0, listener("OriginalLanguage", play), film, nil)))
			saved := chosen(sourceOf(f.ask(t, 0, listener(original, play), film, nil)))
			if want := audioRule(tracks, original, play); got != saved || got != want {
				bad.addf("film %v, PlayDefaultAudioTrack %v: OriginalLanguage answers %d, saving %q answers %d, the rule says %d", tracks, play, got, original, saved, want)
			}
		}
	}
	if namesNone < 5 || namesNone > 100 {
		t.Errorf("%d of 150 generated films have a default audio track with no language; the property is not tested", namesNone)
	}
}

// With no language saved (none, empty, or a value that names none) every film's default is the flagged track else the first
// for both values of PlayDefaultAudioTrack, and a film with no audio has none.
func TestWithNoAudioLanguageSavedTheDefaultIsTheFilesOwn(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	rng := rand.New(rand.NewSource(1210))
	var viewers []viewer
	for i, saved := range []string{"", "und", "mis", "mul", "zxx", "qaa", "xx-not-a-tag", " "} {
		for _, play := range []bool{true, false} {
			viewers = append(viewers, f.listener(t, fmt.Sprintf("nothing%d%v", i, play), saved, play))
		}
	}
	// A viewer who never saved the field at all: jellyfin-web's new user has no AudioLanguagePreference.
	id, token := f.addUser(t, "never saved")
	viewers = append(viewers, viewer{id, token, "never saved"})
	check := func(label string, tracks []track, film string) {
		want := fileDefault(tracks)
		if want == 0 {
			want = -2
		}
		for _, v := range viewers {
			if got := chosen(sourceOf(f.ask(t, 0, v, film, nil))); got != want {
				bad.addf("%s: viewer %s: DefaultAudioStreamIndex %d, want %d", label, v.name, got, want)
			}
		}
	}
	for n := range 60 {
		tracks := generatedTracks(rng)
		film, _ := f.film(t, fmt.Sprintf("Plain %d", n), tracks)
		check(fmt.Sprint(tracks), tracks, film)
	}
	none, _ := f.film(t, "Silent", []track(nil))
	check("a film with no audio", nil, none)
	source := sourceOf(f.ask(t, 0, viewers[0], none, nil))
	if v, present := source["DefaultAudioStreamIndex"]; present && v != nil {
		bad.addf("a film with no audio has DefaultAudioStreamIndex %v", v)
	}
}

// ---- The decision.

// A preferred track that is not the file's default is a copy for jellyfin-web, with the track named in the URL and the
// video copied where invariant 45 allows; the file's own default track is a direct play as before, and neither starts a
// job (PlaybackInfo starts none: the job starts when the player asks for the playlist).
func TestADefaultThatIsNotTheFilesIsACopyAndTheFilesOwnIsADirectPlay(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	english := f.listener(t, "english", "eng", false)
	plainViewer := f.listener(t, "plain", "", true)
	film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
	for _, tc := range []struct {
		who    string
		v      viewer
		want   int
		how    string
		counts map[string]float64
	}{
		{"deu viewer", german, 2, "copy", map[string]float64{"copy": 1}},
		{"eng viewer", english, 1, "direct", map[string]float64{"direct_play": 1}},
		{"viewer with nothing saved", plainViewer, 1, "direct", map[string]float64{"direct_play": 1}},
	} {
		before := decided(t)
		source := sourceOf(f.ask(t, 0, tc.v, film, nil))
		if got := chosen(source); got != tc.want {
			t.Errorf("%s: DefaultAudioStreamIndex %d, want %d", tc.who, got, tc.want)
		}
		how, audio := copyOf(t, source)
		if how != tc.how || (tc.how == "copy" && audio != tc.want) {
			t.Errorf("%s: plays as %s with audio %d, want %s of track %d", tc.who, how, audio, tc.how, tc.want)
		}
		wantDecisions(t, tc.who, before, tc.counts)
	}
	if jobs := scalar[int](t, f.pool, "SELECT count(*)::int FROM transcode_jobs"); jobs != 0 {
		t.Errorf("PlaybackInfo started %d jobs", jobs)
	}
	// A film whose video cannot be copied (no keyframe index, invariant 46) is re-encoded with the track in the URL.
	nokeys, _ := f.film(t, "No index", []track{eng(true), deu(false)})
	mustExec(t, f.pool, "UPDATE media_sources SET keyframes = NULL WHERE item_id = $1::uuid", nokeys)
	before := decided(t)
	source := sourceOf(f.ask(t, 0, german, nokeys, nil))
	if how, audio := copyOf(t, source); chosen(source) != 2 || how != "reencode" || audio != 2 {
		t.Errorf("a film with no keyframe index: default %d plays as %s with audio %d, want a re-encode of track 2", chosen(source), how, audio)
	}
	wantDecisions(t, "re-encode", before, map[string]float64{"reencode": 1})
}

// No DeviceProfile: the preferred track is named, the file plays directly, nothing is offered to transcode.
func TestARequestWithNoProfileGetsThePreferredTrackAndADirectPlay(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
	for what, reply := range map[string]doc{"POST {}": f.plain(t, 0, german, film, nil), "GET": f.get(t, 1, german, film)} {
		source := sourceOf(reply)
		before := decided(t)
		if chosen(source) != 2 {
			t.Errorf("%s: DefaultAudioStreamIndex %d, want the preferred track 2", what, chosen(source))
		}
		if how, _ := copyOf(t, source); how != "direct" {
			t.Errorf("%s: plays as %s, want a direct play", what, how)
		}
		wantDecisions(t, what, before, map[string]float64{})
	}
	before := decided(t)
	f.plain(t, 0, german, film, nil)
	wantDecisions(t, "no profile", before, map[string]float64{"direct_play": 1})
	if jobs := scalar[int](t, f.pool, "SELECT count(*)::int FROM transcode_jobs"); jobs != 0 {
		t.Errorf("%d jobs", jobs)
	}
}

// When the copy cannot start, a film that plays directly today still plays: the viewer is not shown "cannot be played", the
// file's default track plays and DefaultAudioStreamIndex names it, also when the request names the saved-language track (a pick of
// another track is refused or played, D-029).
func TestACopyThatCannotStartFallsBackToTheFilesDefaultTrack(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(t *testing.T, f *audioFixture)
	}{
		{"no worker is running", func(t *testing.T, f *audioFixture) { mustExec(t, f.pool, "TRUNCATE transcode_nodes") }},
		{"no replica has a transcodeStore", func(t *testing.T, f *audioFixture) {
			for _, s := range f.apis {
				s.Transcode = nil
			}
			mustExec(t, f.pool, "TRUNCATE transcode_nodes")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			german := f.listener(t, "german", "deu", false)
			film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
			tc.break_(t, f)
			for who, ask := range map[string]func() doc{
				"a deu viewer":                      func() doc { return f.ask(t, 0, german, film, nil) },
				"a deu viewer on the other replica": func() doc { return f.ask(t, 1, german, film, nil) },
				// A pick that is not the saved language no longer falls back (D-029): it plays that track or is refused, which
				// TestAPickThatCannotBeDeliveredNowIsRefusedOrPlayedNeverAnotherTrack and the lasting states of audio_menu_integration_test.go check.
				"a deu viewer who names it": func() doc { return f.ask(t, 0, german, film, map[string]any{"AudioStreamIndex": 2}) },
			} {
				before := decided(t)
				reply := ask()
				if code := reply["ErrorCode"]; code != nil || len(reply["MediaSources"].([]any)) != 1 {
					t.Errorf("%s: %v: the film that plays directly is refused", who, reply)
					continue
				}
				source := sourceOf(reply)
				if how, _ := copyOf(t, source); how != "direct" || chosen(source) != 1 {
					t.Errorf("%s: plays as %s with DefaultAudioStreamIndex %d, want a direct play of the file's default track 1", who, how, chosen(source))
				}
				wantDecisions(t, who, before, map[string]float64{"direct_play": 1})
			}
		})
	}
}

// A film with two sources whose tracks differ gets each source's own default and decision, chosen from its own tracks.
func TestEachSourceOfAFilmIsChosenFromItsOwnTracks(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	// Source A plays English by default and has German second: a copy of track 2. Source B has German first and French
	// flagged: the first German track, 1, is not the flagged 2, so a copy of track 1... and source C has no German at all.
	film, ids := f.film(t, "Three versions", []track{eng(true), deu(false)}, []track{deu(false), {"fra", true}, eng(false)}, []track{{"fra", true}, eng(false)})
	reply := f.ask(t, 0, german, film, nil)
	want := []struct {
		index int
		how   string
	}{{2, "copy"}, {1, "copy"}, {1, "direct"}}
	for i, id := range ids {
		source := sourceNamed(t, reply, id)
		how, audio := copyOf(t, source)
		if chosen(source) != want[i].index || how != want[i].how || (how == "copy" && audio != want[i].index) {
			t.Errorf("source %d: default %d plays as %s with audio %d, want %d as %s", i, chosen(source), how, audio, want[i].index, want[i].how)
		}
		// Asked for alone, the source answers the same.
		alone := sourceOf(f.ask(t, 1, german, film, map[string]any{"MediaSourceId": id}))
		if chosen(alone) != want[i].index {
			t.Errorf("source %d asked for alone: default %d, want %d", i, chosen(alone), want[i].index)
		}
	}
}

// ---- Settings act at once, on every replica, for each viewer.

func TestANewAudioSettingActsOnTheNextPlaybackInfoOfAnotherReplica(t *testing.T) {
	f := audioEnv(t)
	v := f.listener(t, "changing", "", true)
	film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
	steps := []struct {
		language string
		play     bool
		want     int
		how      string
	}{
		{"", true, 1, "direct"},
		{"deu", false, 2, "copy"},
		{"deu", true, 1, "direct"}, // The flagged track only (rule 2): PlayDefaultAudioTrack on keeps the file's.
		{"OriginalLanguage", false, 1, "direct"},
		{"ger", false, 2, "copy"},
		{"eng", false, 1, "direct"},
		{"", false, 1, "direct"},
	}
	for i, step := range steps {
		saver, asker := i%2, (i+1)%2
		f.saveAudio(t, saver, v, step.language, step.play)
		source := sourceOf(f.ask(t, asker, v, film, nil))
		how, _ := copyOf(t, source)
		if chosen(source) != step.want || how != step.how {
			t.Errorf("after saving %q, PlayDefaultAudioTrack %v on replica %d, asked of replica %d: default %d as %s, want %d as %s", step.language, step.play, saver, asker, chosen(source), how, step.want, step.how)
		}
	}
}

// Two viewers with different audio languages asking for one film at once each get their own default and decision.
func TestTwoViewersAskingForOneFilmAtOnceEachGetTheirOwnDefault(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	english := f.listener(t, "english", "eng", false)
	film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
	const rounds = 25
	var start, done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	var wrong []string
	ask := func(v viewer, want int, how string, replica int) {
		defer done.Done()
		start.Wait()
		for range rounds {
			source := sourceOf(f.ask(t, replica, v, film, nil))
			got, gotHow := chosen(source), "direct"
			if text, _ := source["TranscodingUrl"].(string); text != "" {
				gotHow = "transcode"
			}
			if got != want || gotHow != how {
				mu.Lock()
				wrong = append(wrong, fmt.Sprintf("%s got %d as %s, want %d as %s", v.name, got, gotHow, want, how))
				mu.Unlock()
			}
		}
	}
	for replica := range 2 {
		done.Add(2)
		go ask(german, 2, "transcode", replica)
		go ask(english, 1, "direct", replica)
	}
	start.Done()
	done.Wait()
	if len(wrong) > 0 {
		t.Errorf("%d answers were another viewer's, for example %s", len(wrong), wrong[0])
	}
}

// A PlaybackInfo with an audio language saved sends no more statements than the same one with none, where both take the
// same road (the copy: one names the track in the request, the other has it saved) and where both play directly. This
// holds beside TestPlaybackInfoSendsTheSameStatementsWhateverTheSettings, which does for the film of newEnv.
func TestPlaybackInfoSendsNoMoreStatementsWithAnAudioLanguageSaved(t *testing.T) {
	f := audioEnv(t)
	logs := traceReplicas(t, f.env)
	german := f.listener(t, "german", "deu", false)
	named := f.listener(t, "names the track", "", true)
	film, _ := f.film(t, "English and German", []track{eng(true), deu(false)})
	german2 := f.listener(t, "german, default German", "deu", false)
	filmGerman, _ := f.film(t, "German and English", []track{deu(true), eng(false)})
	count := func(v viewer, item string, extra map[string]any) []string {
		f.ask(t, 0, v, item, extra) // Warm, so a first read is not counted.
		logs[0].take()
		f.ask(t, 0, v, item, extra)
		return logs[0].take()
	}
	for _, tc := range []struct {
		what        string
		saved, base []string
	}{
		{"a copy", count(german, film, nil), count(named, film, map[string]any{"AudioStreamIndex": "2"})},
		{"a direct play", count(german2, filmGerman, nil), count(named, filmGerman, nil)},
	} {
		if len(tc.saved) > len(tc.base) {
			t.Errorf("%s: %d statements with an audio language saved, %d without:\n%s\nagainst\n%s", tc.what, len(tc.saved), len(tc.base), listing(tc.saved), listing(tc.base))
		}
	}
}

// The listed streams, IsOriginal and DefaultSubtitleStreamIndex are the baseline viewer's for every setting: only
// DefaultAudioStreamIndex and the decision it brings change.
func TestAnAudioLanguageChangesOnlyTheDefaultAudioAndTheDecision(t *testing.T) {
	f := audioEnv(t)
	baseline := f.listener(t, "baseline", "", true)
	film, _ := f.film(t, "Three languages", []track{eng(true), deu(false), {"fra", false}})
	pick := func(source doc) string {
		var out []string
		for _, s := range streamsOf(t, source["MediaStreams"]) {
			out = append(out, fmt.Sprint(s["Index"], s["Type"], s["Language"], s["IsDefault"], s["IsOriginal"], s["Codec"], s["DisplayTitle"]))
		}
		return strings.Join(out, "; ") + fmt.Sprint(" subtitle ", source["DefaultSubtitleStreamIndex"], " id ", source["Id"], " container ", source["Container"])
	}
	want := pick(sourceOf(f.ask(t, 0, baseline, film, nil)))
	if !strings.Contains(want, "Audio") {
		t.Fatalf("the baseline lists no audio: %s", want)
	}
	for i, settings := range []struct {
		language string
		play     bool
	}{{"deu", false}, {"fra", false}, {"deu", true}, {"OriginalLanguage", false}, {"und", false}, {"eng", false}} {
		v := f.listener(t, fmt.Sprintf("settings%d", i), settings.language, settings.play)
		for _, extra := range []map[string]any{nil, {"SubtitleStreamIndex": 4}, {"AudioStreamIndex": 3, "SubtitleStreamIndex": -1}} {
			if got, base := pick(sourceOf(f.ask(t, 0, v, film, extra))), pick(sourceOf(f.ask(t, 0, baseline, film, extra))); got != base {
				t.Errorf("saved %q, PlayDefaultAudioTrack %v, request %v:\n got %s\nwant %s", settings.language, settings.play, extra, got, base)
			}
		}
	}
}

// ---- Hiding (JS-119): the default is a listed audio stream, never a placeholder.

// With the viewer's other languages hidden, the default is the same index as with hiding off and names a stream listed as Audio: in
// every captured cell with a language saved, and in generated films.
func TestTheDefaultIsAListedAudioStreamWhateverIsHidden(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	capture := loadAudioCapture(t)
	rng := rand.New(rand.NewSource(1211))
	type film struct {
		name, id string
		tracks   []track
	}
	var films []film
	for _, c := range capture.Films {
		tracks := captureTracks(c.Audio)
		id, _ := f.film(t, c.Name, tracks)
		films = append(films, film{c.Name, id, tracks})
	}
	for n := range 80 {
		tracks := generatedTracks(rng)
		id, _ := f.film(t, fmt.Sprintf("Hidden %d", n), tracks)
		films = append(films, film{fmt.Sprint(tracks), id, tracks})
	}
	// A film whose default falls back to a track in neither the viewer's language nor, by tag, the film's: here the viewer's
	// German is not on the file, and what plays is the flagged French track, which is the original language.
	films = append(films, film{"fallback", "", nil})
	films[len(films)-1].tracks = []track{{"jpn", false}, {"fra", true}, eng(false)}
	films[len(films)-1].id, _ = f.film(t, "Fallback", films[len(films)-1].tracks)

	for _, saved := range []string{"eng", "deu", "ger", "fra", "OriginalLanguage", "und", "xx-not-a-tag", "jpn"} {
		for _, play := range []bool{true, false} {
			shown := f.listener(t, fmt.Sprintf("shown %s %v", saved, play), saved, play)
			hiding := f.listener(t, fmt.Sprintf("hiding %s %v", saved, play), saved, play)
			f.patch(t, 0, hiding, `{"HideOtherAudio":true}`) // JS-118 accepts it with any audio language saved.
			for _, fl := range films {
				want := chosen(sourceOf(f.ask(t, 0, shown, fl.id, nil)))
				source := sourceOf(f.ask(t, 0, hiding, fl.id, nil))
				if got := chosen(source); got != want {
					bad.addf("%s, saved %q, PlayDefaultAudioTrack %v: hiding changed the default from %d to %d", fl.name, saved, play, want, got)
				}
				checkDefaults(t, fmt.Sprintf("%s, saved %q, %v", fl.name, saved, play), source)
				if want == -2 {
					continue
				}
				if present, _ := source["DefaultAudioStreamIndex"].(float64); present == 0 {
					bad.addf("%s: DefaultAudioStreamIndex is %v, which is no audio stream", fl.name, source["DefaultAudioStreamIndex"])
				}
				streams := streamsOf(t, source["MediaStreams"])
				if want >= len(streams) || streams[want]["Type"] != "Audio" {
					bad.addf("%s, saved %q, PlayDefaultAudioTrack %v: the default %d is listed as %v, want Audio", fl.name, saved, play, want, streams[want]["Type"])
				}
			}
		}
	}
}

// ---- A client that picks the track itself (finding: Fluster's profile has no IsSecondaryAudio condition).

// Jellyfin 12.4 turns a track that is not the first into a transcode because of the IsSecondaryAudio condition of Chrome's
// CodecProfiles (SecondaryAudioNotSupported); asked with a profile that has none, it direct plays in every one of 86
// captured cells and still names the preferred track (compat/captures/audio-default-12.4.md, "a client with no
// IsSecondaryAudio condition"). A client such as Fluster's mpv switches the track itself, so a viewer's language there
// costs no worker slot: jellyscale names the same index, direct play, no TranscodingUrl, no copy counted, no job. The
// build that passes the chosen default into playback.Decide as the audio index sends these viewers to a transcode.
func TestAClientWithNoSecondaryAudioConditionDirectPlaysTheTrackTheViewerPrefers(t *testing.T) {
	f := audioEnv(t)
	capture := loadAudioCapture(t)
	if len(capture.AnyClient) < 80 || len(capture.AnyClientNamed) < 6 {
		t.Fatalf("the capture holds %d and %d cells of a profile with no IsSecondaryAudio condition, want 80 and 6", len(capture.AnyClient), len(capture.AnyClientNamed))
	}
	f.profile = capture.AnyClientProfile
	var wrong []string
	viewers := map[string]viewer{}
	items := map[string]string{}
	tracks := map[string][]track{}
	for _, film := range capture.Films {
		tracks[film.Name] = captureTracks(film.Audio)
		items[film.Name], _ = f.film(t, film.Name, tracks[film.Name])
	}
	before := decided(t)
	cells := 0
	for named, list := range [][]anyClientCell{capture.AnyClient, capture.AnyClientNamed} {
		for _, c := range list {
			key := fmt.Sprint(c.AudioLanguagePreference, "|", c.PlayDefaultAudioTrack)
			v, ok := viewers[key]
			if !ok {
				v = f.listener(t, fmt.Sprintf("any%d", len(viewers)), c.AudioLanguagePreference, c.PlayDefaultAudioTrack)
				viewers[key] = v
			}
			if !c.SupportsDirectPlay || c.TranscodingUrl != nil {
				t.Fatalf("the capture is not what this check reads: %+v", c)
			}
			want := wantIndex(c.DefaultAudioStreamIndex)
			if index, differs := audioDefaultDifferences[cellName(c.Film, c.AudioLanguagePreference, c.PlayDefaultAudioTrack)]; differs && named == 0 {
				want = index
			}
			extra := map[string]any{}
			if c.AudioStreamIndex != nil {
				extra["AudioStreamIndex"] = c.AudioStreamIndex
			}
			source := sourceOf(f.ask(t, 0, v, items[c.Film], extra))
			cells++
			label := fmt.Sprintf("%s, saved %q, PlayDefaultAudioTrack %v, AudioStreamIndex %v", c.Film, c.AudioLanguagePreference, c.PlayDefaultAudioTrack, c.AudioStreamIndex)
			if got := chosen(source); got != want {
				wrong = append(wrong, fmt.Sprintf("%s: DefaultAudioStreamIndex %d, want %d", label, got, want))
			}
			if how, _ := copyOf(t, source); how != "direct" {
				wrong = append(wrong, fmt.Sprintf("%s: plays as %s, Jellyfin 12.4 direct plays a profile with no IsSecondaryAudio condition (a client that picks the track itself)", label, how))
			}
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d of %d answers are wrong; the first:\n%s", len(wrong), cells, strings.Join(wrong[:min(len(wrong), 12)], "\n"))
	}
	wantDecisions(t, "a client with no IsSecondaryAudio condition", before, map[string]float64{"direct_play": float64(cells)})
	if jobs := scalar[int](t, f.pool, "SELECT count(*)::int FROM transcode_jobs"); jobs != 0 {
		t.Errorf("%d jobs started for clients that switch the track themselves", jobs)
	}
}

// ---- The item, which jellyfin-web's details page is built from (finding: Play there always played the file's default).

// jellyfin-web's details page fills its Audio menu from the item's audio streams, selects the item's
// MediaSources[0].DefaultAudioStreamIndex and its Play button sends that menu value as AudioStreamIndex, which the
// captured rule always answers. So GET /Items/{id} is the viewer's too: it names, for each source, the track PlaybackInfo
// names, and lists every audio track of the file (JS-147, D-018: the item once listed that track alone, which took the
// viewer's other languages out of the menu; the audio-menu check is in audio_menu_integration_test.go), on both routes.
func TestTheItemNamesTheTrackPlaybackInfoNamesAndListsEveryTrackForEveryCapturedCell(t *testing.T) {
	f := audioEnv(t)
	capture := loadAudioCapture(t)
	if len(capture.Details) == 0 {
		t.Fatal("the capture has no item cells")
	}
	viewers := map[string]viewer{}
	var wrong []string
	cells := 0
	for _, film := range capture.Films {
		tracks := captureTracks(film.Audio)
		item, _ := f.film(t, film.Name, tracks)
		for _, cell := range film.Cells {
			key := fmt.Sprint(cell.AudioLanguagePreference, "|", cell.PlayDefaultAudioTrack)
			v, ok := viewers[key]
			if !ok {
				v = f.listener(t, fmt.Sprintf("item%d", len(viewers)), cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
				viewers[key] = v
			}
			want := chosen(sourceOf(f.ask(t, 0, v, item, nil)))
			label := cellName(film.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
			cells++
			for _, alias := range []bool{false, true} {
				reply := f.item(t, 1, v, item, alias)
				for list, streams := range listsOf(t, reply) {
					var audio []int
					for _, s := range streams {
						if s["Type"] == "Audio" {
							audio = append(audio, int(s["Index"].(float64)))
						}
					}
					if slices.Equal(audio, seq(len(tracks))) {
						continue
					}
					wrong = append(wrong, fmt.Sprintf("%s (alias %v) %s: lists audio %v, want every track %v", label, alias, list, audio, seq(len(tracks))))
				}
				if got := chosen(sourceOf(reply)); got != want {
					wrong = append(wrong, fmt.Sprintf("%s (alias %v): the item's DefaultAudioStreamIndex %d, PlaybackInfo's %d", label, alias, got, want))
				}
			}
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d answers of %d cells are wrong; the first:\n%s", len(wrong), cells, strings.Join(wrong[:min(len(wrong), 12)], "\n"))
	}
	// The captured items: 12.4's own answer for the three films and settings jellyfin-web's details page was asked with.
	for _, d := range capture.Details {
		var film []audioRow
		for _, c := range capture.Films {
			if c.Name == d.Film {
				film = c.Audio
			}
		}
		item, _ := f.film(t, "detail "+d.Film+fmt.Sprint(d.AudioLanguagePreference, d.PlayDefaultAudioTrack), captureTracks(film))
		v := f.listener(t, "detail"+d.Film+d.AudioLanguagePreference+fmt.Sprint(d.PlayDefaultAudioTrack), d.AudioLanguagePreference, d.PlayDefaultAudioTrack)
		if got, want := chosen(sourceOf(f.item(t, 0, v, item, false))), wantIndex(d.DefaultAudioStreamIndex); got != want {
			t.Errorf("item of %s, saved %q, PlayDefaultAudioTrack %v: DefaultAudioStreamIndex %d, Jellyfin 12.4 answered %d", d.Film, d.AudioLanguagePreference, d.PlayDefaultAudioTrack, got, want)
		}
	}
}

// What the details page's Play then does: the viewer presses it with the item's default in the menu, as the string
// jellyfin-web sends, and the film starts in their language, copied, where the default is not the file's; the file's own
// default is a direct play. A viewer's saved language also reaches a second api process and each source of the item.
func TestPlayOnTheDetailsPageStartsTheFilmInTheViewersLanguage(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	english := f.listener(t, "english", "eng", false)
	film, sources := f.film(t, "English and German", []track{eng(true), deu(false)}, []track{deu(false), eng(true)})
	for _, tc := range []struct {
		who    string
		v      viewer
		source int
		menu   int
		how    string
	}{
		{"deu viewer, source with English first", german, 0, 2, "copy"},
		{"deu viewer, source with German first", german, 1, 1, "copy"}, // B flags English (track 2): German is not the file's default
		{"eng viewer, source with English first", english, 0, 1, "direct"},
		{"eng viewer, source with German first", english, 1, 2, "direct"},
	} {
		for replica := 0; replica < 2; replica++ {
			reply := f.item(t, replica, tc.v, film, false)
			source := reply["MediaSources"].([]any)[tc.source].(doc)
			if got := chosen(source); got != tc.menu {
				t.Errorf("%s, replica %d: the item's menu selects %d, want %d", tc.who, replica, got, tc.menu)
				continue
			}
			before := decided(t)
			played := sourceOf(f.ask(t, replica, tc.v, film, map[string]any{"MediaSourceId": sources[tc.source], "AudioStreamIndex": fmt.Sprint(chosen(source))}))
			how, audio := copyOf(t, played)
			if how != tc.how || (how == "copy" && audio != tc.menu) {
				t.Errorf("%s, replica %d: Play with the menu's %d plays as %s with audio %d, want %s of track %d", tc.who, replica, tc.menu, how, audio, tc.how, tc.menu)
			}
			wantDecisions(t, tc.who, before, map[string]float64{map[string]string{"copy": "copy", "direct": "direct_play"}[tc.how]: 1})
		}
	}
	// A new setting reaches the item of the next request, asked of the other process.
	f.saveAudio(t, 0, english, "deu", false)
	if got := chosen(sourceOf(f.item(t, 1, english, film, false))); got != 2 {
		t.Errorf("after saving deu the item's menu selects %d through the second process, want 2", got)
	}
	// A film with no audio has no DefaultAudioStreamIndex and lists none.
	silent, _ := f.film(t, "Silent", nil)
	if got := chosen(sourceOf(f.item(t, 0, german, silent, false))); got != -2 {
		t.Errorf("a film with no audio: the item names %d", got)
	}
}
