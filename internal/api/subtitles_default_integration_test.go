//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-041: PlaybackInfo and the item name as DefaultSubtitleStreamIndex the track a viewer's saved SubtitleMode and
// SubtitleLanguagePreference choose, as Jellyfin 12.4 does (compat/captures/subtitles-default-12.4.md). The films are the
// capture's 35, made as rows in jellyscale's numbering (the film's own tracks, then the files; testenv Rows), so a cell is
// compared through the capture's map from Jellyfin's index to the same track here. A cell where jellyscale answers another
// track is a compat/differences.yaml entry named "subtitle default: <film> | <mode> | <language>"; one it answers as captured
// must not be listed. The reply's DefaultSubtitleStreamIndex is -1 where Jellyfin's is absent.

// sdFixture is a library of the capture's films with a worker (the text tracks are listed and a transcode can start) or
// without one.
type sdFixture struct {
	*langFixture
	capture *testenv.SubtitlesDefaultCapture
	film    map[string]string // The capture's name to the item's id.
	rows    map[string][]media.MediaStream
	indexOf map[string]map[int]int
}

// sdEnv makes every film of the capture. workers says how transcoding is: "on" is a worker row and a transcode store; "no
// worker" is the store with no worker running; "no cores free" is a worker running with less than a copy costs free (as
// under load); "no store" is a replica with no transcode store at all.
func sdEnv(t *testing.T, workers string) *sdFixture {
	t.Helper()
	var l *langFixture
	switch workers {
	case "on":
		l = rowsEnv(t)
	case "no worker":
		l = rowsEnv(t)
		mustExec(t, l.pool, "TRUNCATE transcode_nodes")
	case "no cores free":
		l = rowsEnv(t)
		// Less than playback.CopyCost (0.05): a burn-in cannot start now, though a worker is alive.
		mustExec(t, l.pool, "UPDATE transcode_nodes SET cores = 0.04")
	case "no store":
		l = &langFixture{subtitleFixture: &subtitleFixture{env: newEnv(t), log: &syncBuffer{}}}
	default:
		t.Fatalf("workers %q", workers)
	}
	l.profile = capturedProfile(t)
	f := &sdFixture{langFixture: l, capture: testenv.LoadSubtitlesDefaultCapture(t, "../../compat/captures/subtitles-default-12.4.json"),
		film: map[string]string{}, rows: map[string][]media.MediaStream{}, indexOf: map[string]map[int]int{}}
	for name, captured := range f.capture.Films {
		streams, indexOf := captured.Rows()
		item, _ := addFilm(t, f.env, name, streams)
		// A copy could be offered: the film has a keyframe index and a probed length, as a scanned film does.
		mustExec(t, f.pool, `UPDATE media_sources SET size = 25000000, video_end = 900000000,
			keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}'
			WHERE item_id = $1::uuid`, item)
		mustExec(t, f.pool, "UPDATE items SET run_time_ticks = 900000000 WHERE id = $1::uuid", item)
		f.film[name], f.rows[name], f.indexOf[name] = item, streams, indexOf
	}
	return f
}

// cacheEveryOtherTextTrack puts into the asset store a worker would have filled the first, third, fifth... text track of
// every film inside it or beside it, so that with no worker running some tracks are listed (as cached ones are) and the others
// are dropped as undeliverable.
func (f *sdFixture) cacheEveryOtherTextTrack(t *testing.T) {
	t.Helper()
	assets, err := store.Open(f.uri)
	if err != nil {
		t.Fatal(err)
	}
	for name, item := range f.film {
		sourceID := scalar[string](t, f.pool, "SELECT replace(id::text, '-', '') FROM media_sources WHERE item_id = $1::uuid", item)
		stored, found, err := f.apis[0].Items.Source(context.Background(), item, sourceID)
		if err != nil || !found {
			t.Fatalf("%s: the stored source: %v %v", name, found, err)
		}
		n := 0
		for _, stream := range f.rows[name] {
			if stream.Type != "Subtitle" || stream.IsImage() {
				continue
			}
			if n++; n%2 == 1 {
				if err := assets.Put(context.Background(), subtitles.Key(stored, stream.Index), strings.NewReader(`{"cues":[]}`)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// settings is a viewer's Configuration as jellyfin-web posts it, with only what the check saves.
type settings struct {
	mode, subtitle, audio string
	playDefaultAudio      bool
}

func (s settings) body() string {
	body := map[string]any{"SubtitleMode": s.mode, "SubtitleLanguagePreference": s.subtitle, "PlayDefaultAudioTrack": s.playDefaultAudio}
	if s.audio != "" {
		body["AudioLanguagePreference"] = s.audio
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func (f *sdFixture) saveSettings(t *testing.T, replica int, v viewer, s settings) {
	t.Helper()
	if status := f.env.save(t, replica, "path", v.id, v.token, s.body()).StatusCode; status != http.StatusNoContent {
		t.Fatalf("saving %s's settings %s: %d", v.name, s.body(), status)
	}
}

// viewerWith is a new user who has saved the settings (and none when s is nil: a viewer who never posted a Configuration).
func (f *sdFixture) viewerWith(t *testing.T, name string, s *settings) viewer {
	t.Helper()
	id, token := f.addUser(t, name)
	v := viewer{id, token, name}
	if s != nil {
		f.saveSettings(t, 0, v, *s)
	}
	return v
}

// ask is PlaybackInfo with jellyfin-web's profile (when profile) or no profile, naming the subtitle track given (nil: none).
func (f *sdFixture) ask(t *testing.T, replica int, v viewer, film string, profile bool, named *int) doc {
	t.Helper()
	body := map[string]any{"UserId": v.id, "StartTimeTicks": 0, "IsPlayback": true, "AutoOpenLiveStream": true, "MaxStreamingBitrate": 2147483647}
	if profile {
		body["DeviceProfile"] = f.profile
	}
	if named != nil {
		body["SubtitleStreamIndex"] = *named
	}
	response, raw := f.do(t, replica, "POST", "/Items/"+film+"/PlaybackInfo", v.token, body, "Authorization", f.auth(v))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: PlaybackInfo of %s: %d %s", v.name, film, response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	return sourceOf(reply)
}

// defaultSubtitle is a source's DefaultSubtitleStreamIndex, and -2 when the source has none.
func defaultSubtitle(source doc) int {
	if n, ok := source["DefaultSubtitleStreamIndex"].(float64); ok {
		return int(n)
	}
	return -2
}

// want is the index jellyscale gives the track the capture's cell names, or -1 for none.
func (f *sdFixture) want(film string, captured *int) int {
	if captured == nil {
		return -1
	}
	return f.indexOf[film][*captured]
}

func subCellName(film, mode, language string) string {
	if language == "" {
		language = "(none)"
	}
	return fmt.Sprintf("subtitle default: %s | %s | %s", film, mode, language)
}

func smartName(film, subtitle, audio string, play bool) string {
	for _, p := range []*string{&subtitle, &audio} {
		if *p == "" {
			*p = "(none)"
		}
	}
	return fmt.Sprintf("subtitle default: %s | Smart | %s | audio %s | PlayDefaultAudioTrack %v", film, subtitle, audio, play)
}

const (
	nameNamed99    = "subtitle default: named 99"
	nameNoProfile  = "subtitle default: named with no profile"
	nameNeverSaved = "subtitle default: a viewer who never saved a Configuration"
)

// listedSubtitleDifferences reads the entries compat/differences.yaml lists for the subtitle default, and holds each to its
// form: a path of MediaSources[].DefaultSubtitleStreamIndex, a kind and a reason of some words, and a name that is one of
// the three that are no cell or a cell of the capture (so an entry for a film, mode or language that is none fails).
func listedSubtitleDifferences(t *testing.T, c *testenv.SubtitlesDefaultCapture) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Requests []string `yaml:"requests"`
		Path     string   `yaml:"path"`
		Paths    []string `yaml:"paths"`
		Kind     string   `yaml:"kind"`
		Reason   string   `yaml:"reason"`
	}
	if err := yaml.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{nameNamed99: true, nameNoProfile: true, nameNeverSaved: true}
	for film, captured := range c.Films {
		for _, cell := range captured.Results {
			known[subCellName(film, cell.Mode, cell.LanguagePreference)] = true
		}
	}
	for _, cell := range c.Smart {
		known[smartName(cell.Film, cell.LanguagePreference, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)] = true
	}
	listed := map[string]bool{}
	for _, entry := range entries {
		for _, name := range entry.Requests {
			if !strings.HasPrefix(name, "subtitle default: ") {
				continue
			}
			if !known[name] {
				t.Errorf("compat/differences.yaml lists %q, which is no cell of the capture and none of the three named differences", name)
			}
			if !slices.Contains(append([]string{entry.Path}, entry.Paths...), "MediaSources[].DefaultSubtitleStreamIndex") {
				t.Errorf("%s: the path is %v, want MediaSources[].DefaultSubtitleStreamIndex", name, entry.Paths)
			}
			if !slices.Contains([]string{"improvement", "gap", "non-goal"}, entry.Kind) || len(strings.Fields(entry.Reason)) < 4 {
				t.Errorf("%s: kind %q and reason %q: a cell Jellyfin answers otherwise needs its kind and its reason", name, entry.Kind, entry.Reason)
			}
			listed[name] = true
		}
	}
	return listed
}

// cellCheck is the one rule for a cell: where compat/differences.yaml excuses it, jellyscale must answer another track (an
// entry whose cell is answered as captured fails); where it does not, the capture's track.
func cellCheck(listed map[string]bool, name string, got, want int, wrong *problems) {
	switch {
	case listed[name] && got == want:
		wrong.addf("%s: compat/differences.yaml lists this cell, but jellyscale answers %d, as Jellyfin does; remove the entry", name, got)
	case !listed[name] && got != want:
		wrong.addf("%s: jellyscale answers %d, Jellyfin's track is %d here, and the cell is not in compat/differences.yaml", name, got, want)
	}
}

// Every captured cell, with jellyfin-web's profile and with none, with transcoding on: the track Jellyfin starts. Where it
// names one, MediaStreams at that position is the track with that Index, whether it is inside the film, a file or a PGS
// track; an image track the profile does not draw is a transcode that burns it (DeliveryMethod Encode, the number in the
// TranscodingUrl), and a text track changes no method of the film.
func TestPlaybackInfoStartsOnTheSubtitleJellyfinStartsOnInEveryCapturedCell(t *testing.T) {
	f := sdEnv(t, "on")
	listed := listedSubtitleDifferences(t, f.capture)
	baseline := f.viewerWith(t, "none", &settings{mode: "None", playDefaultAudio: true})
	var wrong problems
	cells := 0
	for _, mode := range []string{"Default", "Always", "OnlyForced", "None", "Smart"} {
		for _, language := range []string{"", "eng", "deu"} {
			v := f.viewerWith(t, "v-"+mode+"-"+language, &settings{mode: mode, subtitle: language, playDefaultAudio: true})
			for name, captured := range f.capture.Films {
				var cell testenv.SubtitleCell
				for _, c := range captured.Results {
					if c.Mode == mode && c.LanguagePreference == language {
						cell = c
					}
				}
				cells++
				want := f.want(name, cell.DefaultSubtitleStreamIndex)
				source := f.ask(t, 0, v, f.film[name], true, nil)
				got := defaultSubtitle(source)
				cellCheck(listed, subCellName(name, mode, language), got, want, &wrong)
				if got != want {
					continue
				}
				// With no profile an image track cannot be drawn, so it is not listed and no default is: -1.
				plainGot := defaultSubtitle(f.ask(t, 0, v, f.film[name], false, nil))
				if image := want >= 0 && f.rows[name][want].IsImage(); plainGot != want && !(image && plainGot == -1) {
					wrong.addf("%s: with no profile the answer is %d, with jellyfin-web's %d", subCellName(name, mode, language), plainGot, want)
				}
				if want == -1 {
					continue
				}
				streams := streamsOf(t, source["MediaStreams"])
				if want >= len(streams) || streams[want]["Type"] != "Subtitle" || streams[want]["Index"] != float64(want) {
					wrong.addf("%s: MediaStreams at %d is not the subtitle with Index %d: %v", subCellName(name, mode, language), want, want, streams)
					continue
				}
				base := f.ask(t, 0, baseline, f.film[name], true, nil)
				if f.rows[name][want].IsImage() {
					url, _ := source["TranscodingUrl"].(string)
					if source["SupportsDirectPlay"] != false || !strings.Contains(strings.ToLower(url), "subtitlestreamindex="+strconv.Itoa(want)) || streams[want]["DeliveryMethod"] != "Encode" {
						wrong.addf("%s: the default %d is an image track the profile does not draw, and the reply is not a transcode that burns it: direct play %v, TranscodingUrl %q, DeliveryMethod %v",
							subCellName(name, mode, language), want, source["SupportsDirectPlay"], url, streams[want]["DeliveryMethod"])
					}
				} else if source["SupportsDirectPlay"] != base["SupportsDirectPlay"] || (source["TranscodingUrl"] == nil) != (base["TranscodingUrl"] == nil) {
					wrong.addf("%s: a text default changes how the film plays: direct play %v and TranscodingUrl %v, where with no default they are %v and %v",
						subCellName(name, mode, language), source["SupportsDirectPlay"], source["TranscodingUrl"], base["SupportsDirectPlay"], base["TranscodingUrl"])
				}
			}
		}
	}
	if cells != 525 {
		t.Errorf("%d cells asked, want 525", cells)
	}
	wrong.report(t)
}

// Smart with each subtitle language x each AudioLanguagePreference: the audio that plays decides, and the audio preference
// moves the audio where PlayDefaultAudioTrack is off.
func TestPlaybackInfoStartsOnTheSubtitleJellyfinStartsOnForSmartAndEveryAudioLanguage(t *testing.T) {
	f := sdEnv(t, "on")
	listed := listedSubtitleDifferences(t, f.capture)
	viewers := map[settings]viewer{}
	var wrong problems
	for _, cell := range f.capture.Smart {
		s := settings{mode: "Smart", subtitle: cell.LanguagePreference, audio: cell.AudioLanguagePreference, playDefaultAudio: cell.PlayDefaultAudioTrack}
		if _, ok := viewers[s]; !ok {
			viewers[s] = f.viewerWith(t, fmt.Sprintf("smart%d", len(viewers)), &s)
		}
		source := f.ask(t, 0, viewers[s], f.film[cell.Film], true, nil)
		want := f.want(cell.Film, cell.DefaultSubtitleStreamIndex)
		cellCheck(listed, smartName(cell.Film, cell.LanguagePreference, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack), defaultSubtitle(source), want, &wrong)
		if audio, ok := source["DefaultAudioStreamIndex"].(float64); !ok || int(audio) != cell.DefaultAudioStreamIndex-f.capture.Films[cell.Film].Files() {
			wrong.addf("%s, audio %q, PlayDefaultAudioTrack %v: the audio that plays is %v, Jellyfin's is %d (%d files first there)", cell.Film, cell.AudioLanguagePreference,
				cell.PlayDefaultAudioTrack, source["DefaultAudioStreamIndex"], cell.DefaultAudioStreamIndex, f.capture.Films[cell.Film].Files())
		}
	}
	wrong.report(t)
}

// With transcoding off (no transcode store, or a store and no worker running) nothing is listed that cannot be delivered,
// so a default is -1 or the position of a Subtitle stream that is listed with that Index: never a track the reply dropped as
// undeliverable, never another stream's position.
func TestWithTranscodingOffTheDefaultSubtitleIsNoneOrAListedTrackAtItsOwnPosition(t *testing.T) {
	for _, workers := range []string{"no store", "no worker", "no cores free"} {
		t.Run(workers, func(t *testing.T) {
			f := sdEnv(t, workers)
			f.cacheEveryOtherTextTrack(t)
			var wrong problems
			named := 0
			for _, mode := range []string{"Default", "Always", "OnlyForced", "None", "Smart"} {
				for _, language := range []string{"", "eng", "deu"} {
					v := f.viewerWith(t, "v-"+mode+"-"+language, &settings{mode: mode, subtitle: language, playDefaultAudio: true})
					for name := range f.capture.Films {
						for _, profile := range []bool{true, false} {
							source := f.ask(t, 0, v, f.film[name], profile, nil)
							got := defaultSubtitle(source)
							if got == -1 {
								continue
							}
							streams := streamsOf(t, source["MediaStreams"])
							if got < 0 || got >= len(streams) || streams[got]["Type"] != "Subtitle" || streams[got]["Index"] != float64(got) {
								wrong.addf("%s %s %q (profile %v): the default is %d, and MediaStreams there is not the Subtitle with that Index", name, mode, language, profile, got)
								continue
							}
							// A burnt-in default is a burn-in the reply starts: with none to start (no worker, or none with the cores
							// free) the default is -1, as a named track is answered today (the plan falls back to a direct play and
							// drops it). Choosing the default after the plan lists a track selected that nothing draws.
							if streams[got]["DeliveryMethod"] == "Encode" {
								url, _ := source["TranscodingUrl"].(string)
								if !strings.Contains(strings.ToLower(url), "subtitlestreamindex="+strconv.Itoa(got)) {
									wrong.addf("%s %s %q (profile %v): the default %d is DeliveryMethod Encode, and TranscodingUrl %q does not burn it in", name, mode, language, profile, got, url)
									continue
								}
							}
							named++
						}
					}
				}
			}
			if named == 0 {
				t.Error("no cell answered a track with transcoding off: files and tracks of film that are cached would still be listed")
			}
			wrong.report(t)
		})
	}
}

// P1 has a PGS track that mode Always starts on. With nothing to burn it in the answer is -1 and the film direct plays,
// whether there is no transcode store, no worker, or a worker with no cores free.
func TestAnImageDefaultNothingCanBurnInIsNoDefaultAndADirectPlay(t *testing.T) {
	for _, workers := range []string{"no store", "no worker", "no cores free"} {
		t.Run(workers, func(t *testing.T) {
			f := sdEnv(t, workers)
			v := f.viewerWith(t, "always", &settings{mode: "Always", playDefaultAudio: true})
			source := f.ask(t, 0, v, f.film["P1"], true, nil)
			if got := defaultSubtitle(source); got != -1 {
				t.Errorf("P1 with mode Always and nothing to burn the PGS track in: default %d, want -1", got)
			}
			if source["SupportsDirectPlay"] != true || source["TranscodingUrl"] != nil {
				t.Errorf("P1 with nothing to burn in: direct play %v, TranscodingUrl %v; want a direct play", source["SupportsDirectPlay"], source["TranscodingUrl"])
			}
		})
	}
}

// The item names the track PlaybackInfo names for the viewer, where Jellyfin's item carries one, at a listed Subtitle stream,
// and the alias says the same, so jellyfin-web's details page selects it and its Play sends it.
func TestTheItemNamesTheDefaultSubtitlePlaybackInfoNamesForEveryCapturedCell(t *testing.T) {
	f := sdEnv(t, "on")
	listed := listedSubtitleDifferences(t, f.capture)
	viewers := map[[2]string]viewer{}
	var wrong problems
	for _, cell := range f.capture.Items {
		key := [2]string{cell.Mode, cell.LanguagePreference}
		if _, ok := viewers[key]; !ok {
			viewers[key] = f.viewerWith(t, "v-"+cell.Mode+"-"+cell.LanguagePreference, &settings{mode: cell.Mode, subtitle: cell.LanguagePreference, playDefaultAudio: true})
		}
		v := viewers[key]
		info := defaultSubtitle(f.ask(t, 0, v, f.film[cell.Film], true, nil))
		var captured *int
		if n, ok := cell.PlaybackInfo.(float64); ok {
			captured = media.Ptr(int(n))
		}
		want := f.want(cell.Film, captured)
		if listed[subCellName(cell.Film, cell.Mode, cell.LanguagePreference)] {
			continue // Another track than Jellyfin's is the entry's business; the item must still say what PlaybackInfo says.
		}
		for _, alias := range []bool{false, true} {
			item := sourceOf(f.item(t, 0, v, f.film[cell.Film], alias))
			got := defaultSubtitle(item)
			switch {
			case captured == nil && got != -2 && got != -1:
				wrong.addf("the item of %s, %s %q (alias %v): Jellyfin's carries none, this one %d", cell.Film, cell.Mode, cell.LanguagePreference, alias, got)
			case captured != nil && got != want:
				wrong.addf("the item of %s, %s %q (alias %v): DefaultSubtitleStreamIndex %d, Jellyfin's track is %d here", cell.Film, cell.Mode, cell.LanguagePreference, alias, got, want)
			case captured != nil && got != info:
				wrong.addf("the item of %s, %s %q (alias %v): %d, PlaybackInfo names %d", cell.Film, cell.Mode, cell.LanguagePreference, alias, got, info)
			case captured != nil:
				listedHere := false
				for _, s := range streamsOf(t, item["MediaStreams"]) {
					listedHere = listedHere || (s["Type"] == "Subtitle" && s["Index"] == float64(got))
				}
				if !listedHere {
					wrong.addf("the item of %s, %s %q (alias %v): names %d, which its MediaStreams do not list as a Subtitle", cell.Film, cell.Mode, cell.LanguagePreference, alias, got)
				}
			}
		}
	}
	wrong.report(t)
}

// A viewer who never posted a Configuration is answered no default for every film; one who saved mode Default and no
// language is answered as Jellyfin's Default; mode None starts nothing in any language unless a track is named.
func TestOnlyAViewerWhoSavedAModeIsStartedOnASubtitle(t *testing.T) {
	f := sdEnv(t, "on")
	listed := listedSubtitleDifferences(t, f.capture)
	if !listed[nameNeverSaved] {
		t.Errorf("compat/differences.yaml does not list %q: a new Jellyfin user is Default and a new jellyscale viewer starts on nothing", nameNeverSaved)
	}
	never := f.viewerWith(t, "never", nil)
	saved := f.viewerWith(t, "saved default", &settings{mode: "Default", playDefaultAudio: true})
	var wrong problems
	startedOne := 0
	for name, captured := range f.capture.Films {
		for _, profile := range []bool{true, false} {
			if got := defaultSubtitle(f.ask(t, 0, never, f.film[name], profile, nil)); got != -1 {
				wrong.addf("%s: a viewer who never saved a Configuration is answered %d, want -1 (profile %v)", name, got, profile)
			}
		}
		for _, cell := range captured.Results {
			if cell.Mode == "Default" && cell.LanguagePreference == "" {
				want := f.want(name, cell.DefaultSubtitleStreamIndex)
				if want >= 0 {
					startedOne++
				}
				cellCheck(listed, subCellName(name, "Default", ""), defaultSubtitle(f.ask(t, 0, saved, f.film[name], true, nil)), want, &wrong)
			}
		}
	}
	if startedOne < 15 {
		t.Errorf("Default with no language starts a subtitle on %d films of the capture, want at least 15", startedOne)
	}
	for _, language := range []string{"", "eng", "deu"} {
		none := f.viewerWith(t, "none-"+language, &settings{mode: "None", subtitle: language, playDefaultAudio: true})
		for name := range f.capture.Films {
			if got := defaultSubtitle(f.ask(t, 0, none, f.film[name], true, nil)); got != -1 {
				wrong.addf("%s: mode None and language %q is answered %d, want -1", name, language, got)
			}
		}
	}
	wrong.report(t)
}

// A track the request names wins over every mode: inside the film, a file, a PGS track, and -1 (what the details page's Off
// sends) answer as sent; 99, which is no stream, answers -1 here where Jellyfin echoes it (listed in
// compat/differences.yaml); with no profile the named track is answered too, where Jellyfin ignores it (listed as well).
func TestATrackTheRequestNamesWinsOverEveryMode(t *testing.T) {
	f := sdEnv(t, "on")
	listed := listedSubtitleDifferences(t, f.capture)
	for _, name := range []string{nameNamed99, nameNoProfile} {
		if !listed[name] {
			t.Errorf("compat/differences.yaml does not list %q", name)
		}
	}
	viewers := map[[2]string]viewer{}
	var wrong problems
	asked := 0
	for _, cell := range f.capture.Named {
		if cell.SubtitleStreamIndex == nil {
			continue
		}
		key := [2]string{cell.Mode, cell.LanguagePreference}
		if _, ok := viewers[key]; !ok {
			viewers[key] = f.viewerWith(t, "v-"+cell.Mode+"-"+cell.LanguagePreference, &settings{mode: cell.Mode, subtitle: cell.LanguagePreference, playDefaultAudio: true})
		}
		sent := f.want(cell.Film, cell.SubtitleStreamIndex)
		switch *cell.SubtitleStreamIndex {
		case -1, 99:
			sent = *cell.SubtitleStreamIndex
		}
		want := sent
		if *cell.SubtitleStreamIndex == 99 {
			want = -1
		}
		asked++
		got := defaultSubtitle(f.ask(t, 0, viewers[key], f.film[cell.Film], true, &sent))
		if got != want {
			wrong.addf("%s, mode %s, %q, naming %d: answered %d, want %d", cell.Film, cell.Mode, cell.LanguagePreference, sent, got, want)
		}
		// With no profile, a text track named is answered (an image track cannot be drawn without a transcode, so it is not listed).
		if want >= 0 && !f.rows[cell.Film][want].IsImage() {
			if got := defaultSubtitle(f.ask(t, 0, viewers[key], f.film[cell.Film], false, &sent)); got != want {
				wrong.addf("%s, mode %s, %q, naming %d with no profile: answered %d, want %d", cell.Film, cell.Mode, cell.LanguagePreference, sent, got, want)
			}
		}
	}
	if asked < 100 {
		t.Errorf("%d named cells replayed, want at least 100", asked)
	}
	wrong.report(t)
}

// ruleOver is Jellyfin's rule over the subtitle tracks a reply lists (a placeholder of Type Data is no track), for a viewer
// who saved the language, with the language of the audio track the reply says plays.
func ruleOver(t *testing.T, source doc, mode, saved string) (int, bool) {
	t.Helper()
	var tracks []testenv.SubtitleTrack
	audio := ""
	played, _ := source["DefaultAudioStreamIndex"].(float64)
	for _, s := range streamsOf(t, source["MediaStreams"]) {
		language, _ := s["Language"].(string)
		switch {
		case s["Type"] == "Audio" && s["Index"] == played:
			audio = language
		case s["Type"] == "Subtitle":
			track := testenv.SubtitleTrack{Index: int(s["Index"].(float64)), IsDefault: s["IsDefault"] == true, IsForced: s["IsForced"] == true, IsExternal: s["IsExternal"] == true}
			if language != "" {
				track.Language = &language
			}
			tracks = append(tracks, track)
		}
	}
	return testenv.JellyfinSubtitleDefault(tracks, mode, saved, audio)
}

// A viewer who hides the tracks outside their languages (JS-119) is started on the mode's rule applied to the tracks listed
// to them alone, never on a placeholder of Type Data and never on a track the menu does not show; with the switches off, or
// ShowAllTracks on, the answers are the capture's.
func TestTheDefaultSubtitleIsChosenAmongTheTracksAHidingViewerIsListed(t *testing.T) {
	modes := []string{"Default", "Always", "OnlyForced", "None", "Smart"}
	t.Run("A5", func(t *testing.T) {
		f := sdEnv(t, "on")
		a5 := f.capture.Films["A5"]
		french, german := f.indexOf["A5"][0], f.indexOf["A5"][4] // The French file and the German track flagged default.
		hiding := f.viewer(t, "hiding", "eng", "eng", bothOn)
		showAll := f.viewer(t, "show all", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`)
		off := f.viewer(t, "off", "eng", "eng", "")
		for _, v := range []viewer{hiding, showAll, off} {
			for _, mode := range modes {
				f.saveSettings(t, 0, v, settings{mode: mode, subtitle: "eng", audio: "eng", playDefaultAudio: true})
				source := f.ask(t, 0, v, f.film["A5"], true, nil)
				got := defaultSubtitle(source)
				if v.name == "hiding" {
					want, ok := ruleOver(t, source, mode, "eng")
					if !ok {
						want = -1
					}
					if got != want {
						t.Errorf("A5, mode %s: answered %d, the rule over the listed tracks says %d", mode, got, want)
					}
					streams := streamsOf(t, source["MediaStreams"])
					if got >= 0 && (got >= len(streams) || streams[got]["Type"] != "Subtitle") {
						t.Errorf("A5, mode %s: %d is not a listed Subtitle (%v)", mode, got, streams[min(got, len(streams)-1)]["Type"])
					}
					if mode == "Default" && (got == french || got == german) {
						t.Errorf("A5 with eng and mode Default answers %d, the French file (%d) or the hidden German track (%d)", got, french, german)
					}
					if mode == "Default" && got != -1 {
						t.Errorf("A5 with eng, hiding, mode Default answers %d: the only track listed is a plain English one inside the film, which Default does not start", got)
					}
					continue
				}
				var captured *int
				for _, cell := range a5.Results {
					if cell.Mode == mode && cell.LanguagePreference == "eng" {
						captured = cell.DefaultSubtitleStreamIndex
					}
				}
				if want := f.want("A5", captured); got != want {
					t.Errorf("A5, %s, mode %s: answered %d, the capture's track is %d", v.name, mode, got, want)
				}
			}
		}
	})
	t.Run("Babel Film", func(t *testing.T) {
		f := languagesEnv(t, true)
		hiding := f.viewer(t, "hiding", "eng", "eng", bothOn)
		showAll := f.viewer(t, "show all", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`)
		sd := &sdFixture{langFixture: f}
		answers := map[string]map[string]int{}
		for _, v := range []viewer{hiding, showAll} {
			answers[v.name] = map[string]int{}
			for _, mode := range modes {
				sd.saveSettings(t, 0, v, settings{mode: mode, subtitle: "eng", audio: "eng", playDefaultAudio: true})
				source := sd.ask(t, 0, v, f.babel, true, nil)
				got := defaultSubtitle(source)
				answers[v.name][mode] = got
				streams := streamsOf(t, source["MediaStreams"])
				if got >= 0 && (got >= len(streams) || streams[got]["Type"] != "Subtitle" || streams[got]["Index"] != float64(got)) {
					t.Errorf("Babel Film, %s, mode %s: %d is not a listed Subtitle at its own position (%v)", v.name, mode, got, streams[min(got, len(streams)-1)])
				}
				want, ok := ruleOver(t, source, mode, "eng")
				if !ok {
					want = -1
				}
				if got != want {
					t.Errorf("Babel Film, %s, mode %s: answered %d, the rule over the tracks listed to this viewer says %d", v.name, mode, got, want)
				}
			}
		}
		// A viewer who hides is never started on what only the other would see: where they differ, the hiding one's track is listed.
		for _, mode := range modes {
			if h, a := answers["hiding"][mode], answers["show all"][mode]; h != a {
				source := sd.ask(t, 0, hiding, f.babel, true, nil)
				if h >= 0 && streamsOf(t, source["MediaStreams"])[h]["Type"] == "Data" {
					t.Errorf("Babel Film, mode %s: the hiding viewer is started on a hidden track, %d", mode, h)
				}
			}
		}
	})
}

// A hidden track the request names is the answer: the details page lists it as itself once picked.
func TestAHiddenTrackTheRequestNamesIsStartedOn(t *testing.T) {
	f := sdEnv(t, "on")
	hiding := f.viewer(t, "hiding", "eng", "eng", bothOn)
	f.saveSettings(t, 0, hiding, settings{mode: "Always", subtitle: "eng", audio: "eng", playDefaultAudio: true})
	german := f.indexOf["A5"][4]
	source := f.ask(t, 0, hiding, f.film["A5"], true, &german)
	if got := defaultSubtitle(source); got != german {
		t.Errorf("A5 naming the hidden German track %d: answered %d", german, got)
	}
	if streams := streamsOf(t, source["MediaStreams"]); streams[german]["Type"] != "Subtitle" {
		t.Errorf("the track the request names is listed as %v", streams[german]["Type"])
	}
}

// Two viewers with different modes asking the same api process for the same film each get their own default, at once, and a
// mode saved through one api process changes the next PlaybackInfo on another, with no restart.
func TestEveryViewerIsStartedOnTheirOwnSubtitleOnEveryReplicaAtOnce(t *testing.T) {
	f := sdEnv(t, "on")
	b3 := f.film["B3"]
	always := f.want("B3", media.Ptr(2)) // Jellyfin's 2 is the first track inside the film: the English one.
	if always < 0 {
		t.Fatal("B3's English track is not in the rows")
	}
	viewers := []struct {
		v    viewer
		mode string
		want int
	}{
		{f.viewerWith(t, "always", &settings{mode: "Always", playDefaultAudio: true}), "Always", always},
		{f.viewerWith(t, "none", &settings{mode: "None", playDefaultAudio: true}), "None", -1},
		{f.viewerWith(t, "deu", &settings{mode: "Always", subtitle: "deu", playDefaultAudio: true}), "Always deu", f.want("B3", media.Ptr(3))},
	}
	var wg sync.WaitGroup
	for round := range 24 {
		for _, who := range viewers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := defaultSubtitle(f.ask(t, round%2, who.v, b3, true, nil)); got != who.want {
					t.Errorf("viewer %s (mode %s), replica %d, round %d: answered %d, want %d", who.v.name, who.mode, round%2, round, got, who.want)
				}
			}()
		}
	}
	wg.Wait()

	// Saved through one replica, asked of the other, in turns and for three viewers at once.
	for _, v := range []viewer{f.viewerWith(t, "walker0", nil), f.viewerWith(t, "walker1", nil), f.viewerWith(t, "walker2", nil)} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := defaultSubtitle(f.ask(t, 1, v, b3, true, nil)); got != -1 {
				t.Errorf("%s never saved a Configuration: %d", v.name, got)
			}
			for round := range 20 {
				saver, asker := round%2, 1-round%2
				mode, want := "Always", always
				if round%4 >= 2 {
					mode, want = "None", -1
				}
				if status := f.env.save(t, saver, "path", v.id, v.token, settings{mode: mode, playDefaultAudio: true}.body()).StatusCode; status != http.StatusNoContent {
					t.Errorf("%s: saving %s through replica %d: %d", v.name, mode, saver, status)
					return
				}
				if got := defaultSubtitle(f.ask(t, asker, v, b3, true, nil)); got != want {
					t.Errorf("%s: %s saved through replica %d, asked of replica %d: answered %d, want %d", v.name, mode, saver, asker, got, want)
				}
			}
		}()
	}
	wg.Wait()
}

// The playback decision line (JS-153) names the mode's default as the subtitle that plays, with no track asked for; a viewer
// whose mode starts none says none. An image default is the burn-in of a re-encode that the line says too.
func TestTheDecisionLineNamesTheSubtitleTheModeStartsOn(t *testing.T) {
	f := sdEnv(t, "on")
	for _, s := range f.apis {
		s.Log = slog.New(slog.NewTextHandler(f.log, nil))
	}
	line := func(film string, v viewer) decisionLine {
		before := len(decisionsOf(f.log.String()))
		f.ask(t, 0, v, f.film[film], true, nil)
		lines := decisionsOf(f.log.String())
		if len(lines) != before+1 {
			t.Fatalf("%s asked %s: %d decision lines, want one more than %d", v.name, film, len(lines), before)
		}
		return lines[len(lines)-1]
	}
	always := f.viewerWith(t, "always", &settings{mode: "Always", playDefaultAudio: true})
	none := f.viewerWith(t, "none", &settings{mode: "None", playDefaultAudio: true})
	l := line("B3", always)
	if want := strconv.Itoa(f.want("B3", media.Ptr(2))); l["subtitle_asked"] != "none" || l["subtitle_index"] != want || l["subtitle_codec"] != "subrip" || l["subtitle_language"] != "eng" || l["subtitle_kind"] != "text" {
		t.Errorf("a text default, the line says %v, want subtitle_asked none and subtitle_index %s, subrip, eng, text", l, want)
	}
	if l := line("B3", none); l["subtitle_asked"] != "none" || l["subtitle_index"] != "none" {
		t.Errorf("mode None, the line says subtitle_asked %q and subtitle_index %q, want none and none", l["subtitle_asked"], l["subtitle_index"])
	}
	l = line("P1", always)
	want := strconv.Itoa(f.want("P1", media.Ptr(2)))
	if l["method"] != "reencode" || l["subtitle_index"] != want || l["subtitle_kind"] != "image" || !l.has("reasons", "SubtitleCodecNotSupported") {
		t.Errorf("an image default, the line says %v, want a reencode of SubtitleCodecNotSupported with subtitle_index %s of kind image", l, want)
	}
}

// An image default the client does not draw is the burn-in of a re-encode, and a viewer can still pick Off: the reply to the
// details page's Off (-1) is a direct play with no default.
func TestAnImageDefaultIsABurnInAndOffStillPlaysDirect(t *testing.T) {
	f := sdEnv(t, "on")
	v := f.viewerWith(t, "always", &settings{mode: "Always", playDefaultAudio: true})
	p1 := f.film["P1"]
	pgs := f.want("P1", media.Ptr(2))
	source := f.ask(t, 0, v, p1, true, nil)
	url, _ := source["TranscodingUrl"].(string)
	if defaultSubtitle(source) != pgs || source["SupportsDirectPlay"] != false || !strings.Contains(strings.ToLower(url), "subtitlestreamindex="+strconv.Itoa(pgs)) {
		t.Fatalf("P1 with mode Always: default %d, direct play %v, TranscodingUrl %q; want the PGS track %d burnt in", defaultSubtitle(source), source["SupportsDirectPlay"], url, pgs)
	}
	off := f.ask(t, 0, v, p1, true, media.Ptr(-1))
	if defaultSubtitle(off) != -1 || off["SupportsDirectPlay"] != true || off["TranscodingUrl"] != nil {
		t.Errorf("P1 naming -1: default %d, direct play %v, TranscodingUrl %v; want -1 and a direct play", defaultSubtitle(off), off["SupportsDirectPlay"], off["TranscodingUrl"])
	}
}

// The item is the viewer's too: asked by two viewers at once with different modes, each is told their own.
func TestTheItemIsEachViewersOwnToo(t *testing.T) {
	f := sdEnv(t, "on")
	always := f.viewerWith(t, "always", &settings{mode: "Always", playDefaultAudio: true})
	none := f.viewerWith(t, "none", &settings{mode: "None", playDefaultAudio: true})
	var wg sync.WaitGroup
	for round := range 12 {
		for _, who := range []struct {
			v    viewer
			want int
		}{{always, f.want("B3", media.Ptr(2))}, {none, -1}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got := defaultSubtitle(sourceOf(f.item(t, round%2, who.v, f.film["B3"], round%3 == 0)))
				if got != who.want && !(who.want == -1 && got == -2) {
					t.Errorf("the item for %s, replica %d: %d, want %d", who.v.name, round%2, got, who.want)
				}
			}()
		}
	}
	wg.Wait()
}

// A real scan: after a subtitle file is added beside a film, or the file that was its default is removed, and the library is
// scanned again, the next PlaybackInfo answers the rule over the new set of tracks, never the removed file and never a number
// from the old numbering.
func TestTheDefaultSubtitleFollowsTheFilesBesideTheFilmThroughARescan(t *testing.T) {
	f := externalEnv(t, "")
	id, token := f.addUser(t, "rescan")
	if status := f.env.save(t, 0, "path", id, token, settings{mode: "Default", playDefaultAudio: true}.body()).StatusCode; status != http.StatusNoContent {
		t.Fatalf("saving the mode: %d", status)
	}
	profile := capturedProfile(t)
	type answer struct {
		index    int
		language string
		external bool
		isFlag   bool
	}
	ask := func(step string) answer {
		t.Helper()
		body := map[string]any{"UserId": id, "StartTimeTicks": 0, "IsPlayback": true, "MaxStreamingBitrate": 2147483647, "DeviceProfile": profile}
		response, raw := f.do(t, 0, "POST", "/Items/"+f.text+"/PlaybackInfo", token, body, "Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="rescan", Version="1", Token="%s"`, token))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: PlaybackInfo: %d %s", step, response.StatusCode, raw)
		}
		var reply doc
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatal(err)
		}
		source := sourceOf(reply)
		got := defaultSubtitle(source)
		want, ok := ruleOver(t, source, "Default", "")
		if !ok {
			want = -1
		}
		if got != want {
			t.Fatalf("%s: answered %d, the rule over the tracks listed now says %d", step, got, want)
		}
		if got == -1 {
			t.Fatalf("%s: a film with subtitle files beside it and mode Default starts on none", step)
		}
		streams := streamsOf(t, source["MediaStreams"])
		if got >= len(streams) || streams[got]["Type"] != "Subtitle" || streams[got]["Index"] != float64(got) {
			t.Fatalf("%s: %d is not a listed Subtitle at its own position", step, got)
		}
		language, _ := streams[got]["Language"].(string)
		return answer{got, language, streams[got]["IsExternal"] == true, streams[got]["IsDefault"] == true}
	}
	first := ask("before any change")
	if !first.external {
		t.Errorf("a film with files beside it and mode Default starts on %+v, want the first file", first)
	}

	f.put(t, "film.de.default.srt", srtCue("added, flagged default")) // Sorts before every file there.
	f.scan(t)
	second := ask("a default file added")
	if second.language != "deu" || !second.isFlag || !second.external {
		t.Errorf("after film.de.default.srt was added the film starts on %+v, want that German file flagged default", second)
	}

	for _, name := range []string{"film.de.default.srt", "film.en.srt"} {
		if err := f.media.Delete(context.Background(), name); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(t)
	third := ask("the default file and the first one removed")
	if third == first || third == second || third.language == "deu" || third.language == "eng" && third.external {
		t.Errorf("after both files were removed the film still starts on %+v (earlier %+v, then %+v)", third, first, second)
	}
	// The removed files leave their Indexes unused, and the item names a track by its position as well as its Index: it must
	// fill the gap with a placeholder and name what PlaybackInfo names, not no track at all (invariant 32).
	response, raw := f.do(t, 0, "GET", "/Items/"+db.CompactID(f.text)+"?userId="+db.CompactID(id), token, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the item after the removal: %d %s", response.StatusCode, raw)
	}
	var reply doc
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	item := defaultSubtitle(sourceOf(reply))
	if item == -2 {
		item = -1
	}
	if item != third.index {
		t.Errorf("after both files were removed the item names %d and PlaybackInfo %d", item, third.index)
	}
}
