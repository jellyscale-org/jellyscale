//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// JS-147 (D-018): the details page's Audio menu lists every audio track the viewer has not hidden, as Jellyfin 12.4 does
// (compat/captures/audio-menu-12.4.md): GET /Items/{id} and its alias list each audio track of the file as Type Audio at its
// own Index, in MediaStreams and in every source's MediaStreams, with the DisplayTitle PlaybackInfo gives it, a hidden track
// as a placeholder of Type Data (JS-119), and name the track PlaybackInfo names in DefaultAudioStreamIndex. Lists
// (/Items, Resume, Latest) answer as they did. These checks run where a picked track can be delivered: the replicas have a
// transcode store, a worker is alive (rowsEnv keeps its row alive) and the films have a probed duration. The states where
// it cannot (no transcode store, no probed duration, no worker, none with the cores free, a spec that just failed) wait for the
// product owner's decision (brief of JS-147, criterion 4): nothing is written for them here.
//
// The films are rows as an earlier scan wrote them: the item's stored DTO lists the file's default audio track alone and
// media_sources.streams every stream, so a pass also proves a film scanned before this release lists every track with
// no new scan and no migration.

// audioAt is the Index of each stream of Type Audio.
func audioAt(streams []doc) []int {
	out := []int{}
	for _, s := range streams {
		if s["Type"] == "Audio" {
			out = append(out, int(s["Index"].(float64)))
		}
	}
	return out
}

// storedAudio is how many audio streams the stored DTO of a film lists in its first source: the precondition of every
// check below (an earlier scan listed one).
func storedAudio(t *testing.T, f *audioFixture, item string) int {
	t.Helper()
	return scalar[int](t, f.pool, `SELECT count(*)::int FROM items, jsonb_array_elements(dto->'MediaSources'->0->'MediaStreams') s
		WHERE items.id = $1::uuid AND s->>'Type' = 'Audio'`, db.DashedID(item))
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}

// ---- Every audio track is listed, at its own Index, in the file's order.

func TestTheItemListsEveryAudioTrackOfTheFileAtItsOwnIndexForAViewerWhoHidesNothing(t *testing.T) {
	f := audioEnv(t)
	plain := f.listener(t, "plain", "", true)
	for _, tc := range []struct {
		name    string
		sources [][]track
	}{
		{"four tracks, two in German, one untagged, the flag on the third", [][]track{{eng(false), deu(false), {"", true}, deu(false)}}},
		{"a film with one track", [][]track{{eng(true)}}},
		{"a film with no audio", [][]track{nil}},
		{"two versions with their own tracks", [][]track{{eng(true), deu(false), {"fra", false}}, {deu(false), eng(true)}}},
		{"the flag on the second of two", [][]track{{eng(false), deu(true)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			film, _ := f.film(t, tc.name, tc.sources...)
			if tc.sources[0] != nil && storedAudio(t, f, film) != 1 {
				t.Fatalf("the stored item lists %d audio streams, this check is of a film an earlier scan stored with one", storedAudio(t, f, film))
			}
			for replica := range 2 {
				for _, alias := range []bool{false, true} {
					reply := f.item(t, replica, plain, film, alias)
					sources := reply["MediaSources"].([]any)
					if len(sources) != len(tc.sources) {
						t.Fatalf("%d sources, want %d", len(sources), len(tc.sources))
					}
					for i, tracks := range tc.sources {
						source := sources[i].(doc)
						streams := streamsOf(t, source["MediaStreams"])
						if got := audioAt(streams); !slices.Equal(got, seq(len(tracks))) {
							t.Errorf("replica %d alias %v source %d lists Type Audio at %v, want %v", replica, alias, i, got, seq(len(tracks)))
						}
						for position, s := range streams {
							if int(s["Index"].(float64)) != position {
								t.Errorf("source %d: stream %d has Index %v", i, position, s["Index"])
							}
							if s["Type"] == "Data" {
								t.Errorf("source %d lists a placeholder at %d for a viewer who hides nothing", i, position)
							}
						}
						want := fileDefault(tracks)
						if len(tracks) == 0 {
							if _, present := source["DefaultAudioStreamIndex"]; present {
								t.Errorf("a film with no audio names DefaultAudioStreamIndex %v", source["DefaultAudioStreamIndex"])
							}
						} else if got := chosen(source); got != want {
							t.Errorf("source %d names %d, want the file's default %d", i, got, want)
						}
						if i == 0 {
							if got := audioAt(streamsOf(t, reply["MediaStreams"])); !slices.Equal(got, seq(len(tracks))) {
								t.Errorf("replica %d alias %v: the item's MediaStreams lists Type Audio at %v, want %v", replica, alias, got, seq(len(tracks)))
							}
						}
					}
				}
			}
		})
	}
}

// Each listed track carries the DisplayTitle PlaybackInfo gives it, so tracks in one language that differ (here 5.1 and
// stereo German) have different labels in the menu. Two tracks that differ in nothing the label shows (Menu Four's
// two German tracks, labelled alike by Jellyfin 12.4: audio-menu-12.4.md) are labelled alike by both; the scan writes the label.
func TestEachAudioTrackTheItemListsCarriesTheLabelPlaybackInfoGivesIt(t *testing.T) {
	f := audioEnv(t)
	plain := f.listener(t, "plain", "", true)
	label := func(s media.MediaStream, title string) media.MediaStream { s.DisplayTitle = title; return s }
	five := aud(3, "deu", false)
	five.Channels = 6
	film, _ := addFilm(t, f.env, "Labelled", []media.MediaStream{
		vid(""),
		label(aud(1, "eng", true), "English - AAC - Stereo - Default"),
		label(aud(2, "deu", false), "German - AAC - Stereo"),
		label(five, "German - AAC - 5.1"),
		label(aud(4, "", false), "AAC - Stereo"),
		sub(5, "eng", false, false),
	})
	if storedAudio(t, f, film) != 1 {
		t.Fatal("the stored item lists more than the default audio track")
	}
	info := map[int]string{}
	for _, s := range streamsOf(t, sourceOf(f.info(t, 0, plain, film, nil))["MediaStreams"]) {
		if s["Type"] == "Audio" {
			info[int(s["Index"].(float64))], _ = s["DisplayTitle"].(string)
		}
	}
	if len(info) != 4 {
		t.Fatalf("PlaybackInfo lists %d audio tracks, want 4", len(info))
	}
	for _, alias := range []bool{false, true} {
		reply := f.item(t, 0, plain, film, alias)
		for list, streams := range listsOf(t, reply) {
			titles := map[string]bool{}
			for _, s := range streams {
				if s["Type"] != "Audio" {
					continue
				}
				index := int(s["Index"].(float64))
				got, _ := s["DisplayTitle"].(string)
				if got == "" || got != info[index] {
					t.Errorf("alias %v %s: track %d is labelled %q, PlaybackInfo's label is %q", alias, list, index, got, info[index])
				}
				titles[got] = true
			}
			if len(titles) != 4 {
				t.Errorf("alias %v %s lists %d different labels for 4 tracks (%v): two entries of the menu would read alike", alias, list, len(titles), titles)
			}
		}
	}
}

// ---- The item and PlaybackInfo agree, for every captured cell and for generated films and settings.

type audioCase struct {
	label    string
	film     string
	tracks   []track
	v        viewer
	hide     bool
	language string // The audio language the viewer saved.
}

// audioCases are the captured films with each of its viewers (each saved language and PlayDefaultAudioTrack, with the
// audio switch off and on) and 1000 generated films, each with a viewer drawn from every kind there is.
func audioCases(t *testing.T, f *audioFixture) []audioCase {
	t.Helper()
	capture := loadAudioCapture(t)
	type key struct {
		language string
		play     bool
		prefs    string
	}
	viewers := map[key]viewer{}
	viewerOf := func(language string, play bool, prefs string) viewer {
		k := key{language, play, prefs}
		if v, ok := viewers[k]; ok {
			return v
		}
		v := f.listener(t, fmt.Sprintf("viewer%d", len(viewers)), language, play)
		if prefs != "" {
			f.patch(t, 0, v, prefs)
		}
		viewers[k] = v
		return v
	}
	var cases []audioCase
	for _, c := range capture.Films {
		tracks := captureTracks(c.Audio)
		id, _ := f.film(t, c.Name, tracks)
		for _, cell := range c.Cells {
			cases = append(cases, audioCase{cellName(c.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack), id, tracks,
				viewerOf(cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack, ""), false, cell.AudioLanguagePreference})
			if cell.AudioLanguagePreference != "" { // JS-118: the switch needs a language saved.
				cases = append(cases, audioCase{cellName(c.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack) + " | hiding", id, tracks,
					viewerOf(cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack, `{"HideOtherAudio":true}`), true, cell.AudioLanguagePreference})
			}
		}
	}
	if len(cases) < 96 {
		t.Fatalf("%d captured cells, want at least 96", len(cases))
	}
	rng := rand.New(rand.NewSource(1471))
	languages := []string{"", "eng", "deu", "ger", "de", "fra", "OriginalLanguage", "und", "xx-not-a-tag", "jpn"}
	prefs := []string{"", `{"HideOtherAudio":true}`, `{"HideOtherAudio":true,"KeepAudioLanguages":["fra"]}`, `{"HideOtherAudio":true,"ShowAllTracks":true}`}
	for n := range 1000 {
		tracks := generatedTracks(rng)
		id, _ := f.film(t, fmt.Sprintf("Generated %d", n), tracks)
		language, play, pref := languages[rng.Intn(len(languages))], rng.Intn(2) == 0, prefs[rng.Intn(len(prefs))]
		if language == "" {
			pref = ""
		}
		cases = append(cases, audioCase{fmt.Sprintf("generated %d %v saved %q %v %s", n, tracks, language, play, pref), id, tracks,
			viewerOf(language, play, pref), pref != "", language})
	}
	return cases
}

// For the same viewer and source the item lists Type Audio and Type Data at exactly the indexes PlaybackInfo lists them
// (same replica, a DeviceProfile, no track named): the menu is what the player's menu will be.
func TestTheItemListsTypeAudioAndTypeDataAtTheIndexesPlaybackInfoDoes(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	cases := audioCases(t, f)
	for _, c := range cases {
		info := sourceOf(f.ask(t, 0, c.v, c.film, nil))
		reply := f.item(t, 0, c.v, c.film, false)
		item := sourceOf(reply)
		for list, streams := range map[string][]doc{"MediaSources[0]": streamsOf(t, item["MediaStreams"]), "MediaStreams": streamsOf(t, reply["MediaStreams"])} {
			wantAudio, wantData := audioAt(streamsOf(t, info["MediaStreams"])), dataAt(streamsOf(t, info["MediaStreams"]))
			if got := audioAt(streams); !slices.Equal(got, wantAudio) {
				bad.addf("%s, %s: the item lists Type Audio at %v, PlaybackInfo at %v", c.label, list, got, wantAudio)
			}
			if got := dataAt(streams); !slices.Equal(got, wantData) {
				bad.addf("%s, %s: the item lists Type Data at %v, PlaybackInfo at %v", c.label, list, got, wantData)
			}
		}
	}
	t.Logf("%d cells", len(cases))
}

// The chosen one is selected: the item names the index PlaybackInfo names in every cell, it is a listed Type Audio stream
// and never a placeholder, with no language saved it is the file's default (also with the flag on the second track), and
// every film with audio lists its default audio track as Audio whatever is hidden.
func TestTheItemSelectsTheTrackPlaybackInfoNamesAndAlwaysListsItAsAudio(t *testing.T) {
	f := audioEnv(t)
	bad := &problems{}
	defer bad.report(t)
	cases := audioCases(t, f)
	for _, c := range cases {
		info := sourceOf(f.ask(t, 0, c.v, c.film, nil))
		source := sourceOf(f.item(t, 1, c.v, c.film, false))
		got, want := chosen(source), chosen(info)
		if got != want {
			bad.addf("%s: the item names %d, PlaybackInfo %d", c.label, got, want)
		}
		if len(c.tracks) == 0 {
			if got != -2 {
				bad.addf("%s: a film with no audio names %d", c.label, got)
			}
			continue
		}
		streams := streamsOf(t, source["MediaStreams"])
		if got < 1 || got >= len(streams) || streams[got]["Type"] != "Audio" {
			bad.addf("%s: the named track %d is not a listed Type Audio stream (%v)", c.label, got, typesOfStreams(streams))
		}
		if d := fileDefault(c.tracks); d < len(streams) && streams[d]["Type"] != "Audio" {
			bad.addf("%s: the file's default track %d is listed as %v", c.label, d, streams[d]["Type"])
		}
		if c.language == "" && got != fileDefault(c.tracks) {
			bad.addf("%s: with no language saved the item names %d, want the file's default %d", c.label, got, fileDefault(c.tracks))
		}
	}
	// A film whose default flag is on the second track: with no language saved, the item selects it.
	film, _ := f.film(t, "Flag on second", []track{eng(false), deu(true), eng(false)})
	none := f.listener(t, "none", "", true)
	if got := chosen(sourceOf(f.item(t, 0, none, film, false))); got != 2 {
		t.Errorf("flag on the second track, no language saved: the item names %d, want 2", got)
	}
}

// ---- JS-119's viewers and Babel Film, with the real scanner and a real worker.

func TestBabelFilmsItemListsAudioWhereTheViewersSettingsKeepIt(t *testing.T) {
	f := languagesEnv(t, true)
	plain := f.baseline(t)
	for i, tc := range []struct {
		name            string
		audio, subtitle string
		prefs           string
		film            string
		wantAudio       []int
		wantData        []int
		chosen          int
	}{
		{"audio eng, subtitle eng, both switches", "eng", "eng", bothOn, "babel", []int{1, 4}, []int{2, 3, 8, 9, 10, 12, 13, 14}, 1},
		{"KeepAudioLanguages fra", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepAudioLanguages":["fra"]}`, "babel", []int{1, 3, 4}, []int{2, 8, 9, 10, 12, 13, 14}, 1},
		{"ShowAllTracks", "eng", "eng", `{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`, "babel", []int{1, 2, 3, 4}, []int{}, 1},
		{"the audio switch off", "eng", "eng", `{"HideOtherSubtitles":true}`, "babel", []int{1, 2, 3, 4}, []int{8, 9, 10, 12, 13, 14}, 1},
		{"the audio language cleared", "", "eng", bothOn, "babel", []int{1, 2, 3, 4}, []int{8, 9, 10, 12, 13, 14}, 1},
		{"no settings", "", "", "", "babel", []int{1, 2, 3, 4}, []int{}, 1},
		{"Kino Film, audio eng, subtitle eng", "eng", "eng", bothOn, "kino", []int{1, 2}, []int{5}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			film, types := f.filmOf(tc.film)
			name := fmt.Sprintf("babel%d", i)
			var v viewer
			if tc.audio == "" && tc.prefs != "" { // The switch is set with a language saved (JS-118), then the language is cleared.
				v = f.viewer(t, name, "eng", tc.subtitle, tc.prefs)
				f.saveLanguages(t, 0, v, "", tc.subtitle)
			} else {
				v = f.viewer(t, name, tc.audio, tc.subtitle, tc.prefs)
			}
			for _, alias := range []bool{false, true} {
				reply := f.item(t, 0, v, film, alias)
				for list, streams := range listsOf(t, reply) {
					if got := audioAt(streams); !slices.Equal(got, tc.wantAudio) {
						t.Errorf("alias %v %s: Type Audio at %v, want exactly %v", alias, list, got, tc.wantAudio)
					}
					if got := dataAt(streams); !slices.Equal(got, tc.wantData) {
						t.Errorf("alias %v %s: Type Data at %v, want exactly %v", alias, list, got, tc.wantData)
					}
					// Every stream at the position it has for a viewer with no settings.
					if len(streams) != len(types) {
						t.Errorf("alias %v %s lists %d streams, want %d", alias, list, len(streams), len(types))
						continue
					}
					for position, s := range streams {
						if int(s["Index"].(float64)) != position {
							t.Errorf("alias %v %s: stream %d has Index %v", alias, list, position, s["Index"])
						}
						if s["Type"] != "Data" && s["Type"] != types[position] {
							t.Errorf("alias %v %s: stream %d is %v, a viewer with no settings has %s", alias, list, position, s["Type"], types[position])
						}
					}
				}
				if got := chosen(sourceOf(reply)); got != tc.chosen && tc.film == "babel" {
					t.Errorf("alias %v: DefaultAudioStreamIndex %d, want %d", alias, got, tc.chosen)
				}
			}
			// The same viewer's PlaybackInfo lists the same, so the menu is the player's menu.
			info := streamsOf(t, sourceOf(f.info(t, 0, v, film, nil))["MediaStreams"])
			if got := audioAt(info); !slices.Equal(got, tc.wantAudio) {
				t.Errorf("PlaybackInfo lists Type Audio at %v, want %v", got, tc.wantAudio)
			}
		})
	}
	// Kino Film's item lists both of its audio tracks for a viewer who hides nothing too.
	for _, alias := range []bool{false, true} {
		if got := audioAt(streamsOf(t, f.item(t, 0, plain, f.kino, alias)["MediaStreams"])); !slices.Equal(got, []int{1, 2}) {
			t.Errorf("Kino Film for a viewer with no settings lists Type Audio at %v, want 1 and 2", got)
		}
	}
}

// ---- Per viewer, at once, and across api processes.

// Three viewers asking one replica for one film at once each get their own list and selected track (eng with both
// switches, deu with both switches and the file's default off, nothing saved), and a setting saved through one api
// process changes the list in the next answer of another.
func TestEachViewerGetsTheirOwnAudioListAndSelectedTrackAtOnceAndAcrossReplicas(t *testing.T) {
	f := audioEnv(t)
	film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
	hiding := func(name, language string, play bool) viewer {
		v := f.listener(t, name, language, play)
		f.patch(t, 0, v, `{"HideOtherAudio":true}`)
		return v
	}
	english, german, none := hiding("english", "eng", true), hiding("german", "deu", false), f.listener(t, "none", "", true)
	want := map[string]struct {
		audio  []int
		chosen int
	}{"english": {[]int{1, 4}, 1}, "german": {[]int{1, 2, 4}, 2}, "none": {[]int{1, 2, 3, 4}, 1}}
	var start, done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	var wrong []string
	ask := func(v viewer, replica int, alias bool) {
		defer done.Done()
		start.Wait()
		for range 20 {
			reply := f.item(t, replica, v, film, alias)
			got, w := audioAt(streamsOf(t, reply["MediaStreams"])), want[v.name]
			if !slices.Equal(got, w.audio) || chosen(sourceOf(reply)) != w.chosen {
				mu.Lock()
				wrong = append(wrong, fmt.Sprintf("%s got Type Audio at %v and %d, want %v and %d", v.name, got, chosen(sourceOf(reply)), w.audio, w.chosen))
				mu.Unlock()
			}
		}
	}
	for _, v := range []viewer{english, german, none} {
		for replica := range 2 {
			done.Add(1)
			go ask(v, replica, replica == 1)
		}
	}
	start.Done()
	done.Wait()
	if len(wrong) > 0 {
		t.Errorf("%d answers were another viewer's, for example %s", len(wrong), wrong[0])
	}
	// Saved through replica 0, seen at once through replica 1.
	f.patch(t, 0, german, `{"KeepAudioLanguages":["fra"]}`)
	reply := f.item(t, 1, german, film, false)
	if got := audioAt(streamsOf(t, reply["MediaStreams"])); !slices.Equal(got, []int{1, 2, 3, 4}) {
		t.Errorf("after KeepAudioLanguages fra was saved through replica 0, replica 1 lists Type Audio at %v, want 1, 2, 3, 4", got)
	}
	f.saveAudio(t, 1, german, "eng", true)
	if got := chosen(sourceOf(f.item(t, 0, german, film, true))); got != 1 {
		t.Errorf("after the language was saved through replica 1, replica 0 names %d, want 1", got)
	}
}

// ---- Lists answer as before.

// GET /Items with Fields=MediaSources,MediaStreams, Resume and Latest list a film with 4 audio tracks as its stored item does,
// the file's default alone and no Type Data, for every viewer: a menu is only offered for one item.
func TestListsStillListTheDefaultAudioTrackAloneAndNoTypeDataForEveryViewer(t *testing.T) {
	f := audioEnv(t)
	film, _ := f.film(t, "Four tracks", []track{eng(false), deu(false), {"", true}, deu(false)})
	stored := scalar[string](t, f.pool, `SELECT (dto->'MediaSources'->0->'MediaStreams')::text FROM items WHERE id = $1`, db.DashedID(film))
	var storedStreams []doc
	if err := json.Unmarshal([]byte(stored), &storedStreams); err != nil {
		t.Fatal(err)
	}
	if got := audioAt(storedStreams); !slices.Equal(got, []int{3}) {
		t.Fatalf("the stored item lists Type Audio at %v, this check assumes the default 3 alone", got)
	}
	_ = storedStreams
	hide := func(name, language string, play bool) viewer {
		v := f.listener(t, name, language, play)
		f.patch(t, 0, v, `{"HideOtherAudio":true}`)
		return v
	}
	seenLists := map[string]string{}
	for _, v := range []viewer{f.listener(t, "none", "", true), f.listener(t, "german", "deu", false), hide("hiding", "eng", true)} {
		mustExec(t, f.pool, `INSERT INTO user_item_data (user_id, item_id, position_ticks, last_played_at) VALUES ($1, $2, 300000000, now())
			ON CONFLICT (user_id, item_id) DO UPDATE SET position_ticks = 300000000, last_played_at = now()`, v.id, db.DashedID(film))
		user := db.CompactID(v.id)
		for name, path := range map[string]string{
			"items":  "/Items?Recursive=true&IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams&userId=" + user,
			"resume": "/UserItems/Resume?Fields=MediaSources,MediaStreams&userId=" + user,
			"latest": "/Items/Latest?IncludeItemTypes=Movie&Fields=MediaSources,MediaStreams&userId=" + user,
		} {
			response, raw := f.do(t, 0, "GET", path, v.token, nil, "Authorization", f.auth(v))
			if response.StatusCode != 200 {
				t.Fatalf("%s %s: %d %s", v.name, name, response.StatusCode, raw)
			}
			var lists [][]doc
			var walk func(any)
			walk = func(x any) {
				switch x := x.(type) {
				case map[string]any:
					for k, v := range x {
						if list, ok := v.([]any); ok && k == "MediaStreams" {
							streams := []doc{}
							for _, s := range list {
								streams = append(streams, s.(doc))
							}
							lists = append(lists, streams)
						} else {
							walk(v)
						}
					}
				case []any:
					for _, e := range x {
						walk(e)
					}
				}
			}
			var parsed any
			if err := json.Unmarshal(raw, &parsed); err != nil {
				t.Fatal(err)
			}
			walk(parsed)
			if len(lists) == 0 {
				t.Errorf("%s %s lists no MediaStreams: %s", v.name, name, raw)
			}
			var texts []string
			for _, streams := range lists {
				if got := audioAt(streams); len(got) != 1 || len(dataAt(streams)) != 0 {
					t.Errorf("%s %s: a list lists Type Audio at %v and Type Data at %v, want the default alone and no Data", v.name, name, got, dataAt(streams))
				}
				texts = append(texts, canon(streams, v.token))
			}
			// Every viewer is answered alike: a list is not the viewer's.
			slices.Sort(texts)
			if all := strings.Join(texts, "\n"); seenLists[name] != "" && seenLists[name] != all {
				t.Errorf("%s %s answers differently from the first viewer:\n%s\nwant\n%s", v.name, name, all, seenLists[name])
			} else {
				seenLists[name] = all
			}
		}
	}
}

// ---- Cost: the item reads what it did, writes nothing, makes no job and runs no tool.

func TestTheItemSendsNoMoreStatementsForMoreTracksHidingOrAnAudioLanguageAndWritesNothing(t *testing.T) {
	f := audioEnv(t)
	logs := traceReplicas(t, f.env)
	shims := t.TempDir()
	ran := filepath.Join(shims, "ran")
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		script := fmt.Sprintf("#!/bin/sh\necho %s \"$@\" >> %s\nexit 1\n", tool, ran)
		if err := os.WriteFile(filepath.Join(shims, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	one, _ := f.film(t, "One track", []track{eng(true)})
	four, _ := f.film(t, "Four tracks", []track{eng(true), deu(false), {"fra", false}, {"", false}})
	versionsOne, _ := f.film(t, "Two versions of one track", []track{eng(true)}, []track{deu(true)})
	versions, _ := f.film(t, "Two versions", []track{eng(true), deu(false), {"fra", false}, {"", false}}, []track{deu(false), eng(true), {"fra", false}})
	plain := f.listener(t, "plain", "", true)
	german := f.listener(t, "german", "deu", false)
	hiding := f.listener(t, "hiding", "eng", true)
	f.patch(t, 0, hiding, `{"HideOtherAudio":true}`)
	hidingGerman := f.listener(t, "hiding german", "deu", false)
	f.patch(t, 0, hidingGerman, `{"HideOtherAudio":true}`)

	rows := func() string {
		return scalar[string](t, f.pool, `SELECT (SELECT md5(string_agg(i::text, '' ORDER BY id)) FROM items i)
			|| (SELECT md5(string_agg(m::text, '' ORDER BY id)) FROM media_sources m)
			|| (SELECT count(*)::text FROM transcode_jobs) || (SELECT count(*)::text FROM subtitle_jobs)`)
	}
	before := rows()
	for _, alias := range []bool{false, true} {
		sent := func(v viewer, film string) []string {
			f.item(t, 0, v, film, alias) // Warm: a first read of a row is not counted.
			logs[0].take()
			f.item(t, 0, v, film, alias)
			statements := logs[0].take()
			for _, s := range statements {
				if regexp.MustCompile(`(?i)\b(insert|update|delete)\b`).MatchString(s) {
					t.Errorf("alias %v: the item wrote: %s", alias, oneLine(s))
				}
			}
			return statements
		}
		for _, tc := range []struct {
			what       string
			more, base []string
		}{
			{"a film of 4 tracks against 1 (plain viewer)", sent(plain, four), sent(plain, one)},
			{"a film of two versions of 4 and 3 tracks against two of 1", sent(plain, versions), sent(plain, versionsOne)},
			{"hiding on against off", sent(hiding, four), sent(plain, four)},
			{"hiding on against off, two versions", sent(hidingGerman, versions), sent(plain, versions)},
			{"an audio language saved against none", sent(german, four), sent(plain, four)},
			{"an audio language saved against none, two versions", sent(german, versions), sent(plain, versions)},
		} {
			if len(tc.more) > len(tc.base) {
				t.Errorf("alias %v, %s: %d statements against %d:\n%s\nagainst\n%s", alias, tc.what, len(tc.more), len(tc.base), listing(tc.more), listing(tc.base))
			}
		}
	}
	if after := rows(); after != before {
		t.Error("the item wrote to items or media_sources, or made a transcode or subtitle job")
	}
	if text, err := os.ReadFile(ran); err == nil {
		t.Errorf("the item ran a tool: %s", text)
	}
}

// ---- The states where a picked track cannot be delivered for as long as the state lasts (D-029).

// DOMAINS invariant 32: the details page offers no track that cannot be delivered. A browser's direct play plays only the
// file's default track, so a pick is delivered only as a copy of the video with that track, which needs a transcode store, a
// probed duration and a worker with cores that agrees with the replicas on the store. Where one is missing for as long as
// the state lasts, the item lists the viewer's track alone, as it did before JS-147, and PlaybackInfo lists no audio track
// it would not play and answers no plan that plays another track than a pick names (D-029).
type lastingState struct {
	name   string
	break_ func(t *testing.T, f *audioFixture, film string)
}

var lastingStates = []lastingState{
	{"no replica has a transcode store", func(t *testing.T, f *audioFixture, film string) {
		for _, s := range f.apis {
			s.Transcode = nil
		}
	}},
	{"the film has no probed duration", func(t *testing.T, f *audioFixture, film string) {
		mustExec(t, f.pool, "UPDATE items SET run_time_ticks = 0 WHERE id = $1", db.DashedID(film))
	}},
	{"no worker is alive", func(t *testing.T, f *audioFixture, film string) { mustExec(t, f.pool, "TRUNCATE transcode_nodes") }},
	{"the only live worker has no cores", func(t *testing.T, f *audioFixture, film string) {
		mustExec(t, f.pool, "TRUNCATE transcode_nodes")
		mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('w1', 'worker', 0, 0, "+strconv.Itoa(playback.OutputVersion)+")")
	}},
	{"the roles' transcode stores differ", func(t *testing.T, f *audioFixture, film string) {
		mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, output_version, store) VALUES ('a1', 'api', 0, "+strconv.Itoa(playback.OutputVersion)+", 'file:///one'), ('a2', 'api', 0, "+strconv.Itoa(playback.OutputVersion)+", 'file:///two')")
	}},
}

func TestWhereAPickCannotBeDeliveredTheItemStillListsTheViewersAudioTrackAlone(t *testing.T) {
	for _, tc := range lastingStates {
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
			german := f.listener(t, "german", "deu", false)
			plain := f.listener(t, "plain", "", true)
			tc.break_(t, f, film)
			// The track the viewer will hear: a saved language that cannot be delivered here falls back to the file's
			// default, so the German viewer hears track 1 too (D-029: the menu shows what will be heard; lead's correction).
			for who, want := range map[viewer]int{german: 1, plain: 1} {
				for replica := range 2 {
					for _, alias := range []bool{false, true} {
						reply := f.item(t, replica, who, film, alias)
						for list, streams := range listsOf(t, reply) {
							if got := audioAt(streams); !slices.Equal(got, []int{want}) {
								t.Errorf("%s, replica %d, alias %v, %s: Type Audio at %v, want the viewer's track %d alone (a pick of another cannot be delivered here)", who.name, replica, alias, list, got, want)
							}
						}
						if got := chosen(sourceOf(reply)); got != want {
							t.Errorf("%s, replica %d, alias %v: names %d, want %d", who.name, replica, alias, got, want)
						}
					}
				}
			}
		})
	}
}

// refusedPlan is a PlaybackInfo reply that offers no source: the "cannot be played at this time" dialog of jellyfin-web.
func refusedPlan(reply doc) bool {
	return reply["ErrorCode"] != nil || len(reply["MediaSources"].([]any)) == 0
}

// playedTrack is the audio track a source's plan plays: a direct play plays the file's default (dflt), a copy or re-encode
// the track its TranscodingUrl names.
func playedTrack(t *testing.T, source doc, dflt int) int {
	t.Helper()
	how, audio := copyOf(t, source)
	if how == "direct" {
		return dflt
	}
	return audio
}

// A plan answers a pick (pick > 0) by playing that track or by refusing, never by playing another. fallback is a pick that
// is the viewer's saved language: that one falls back to the file's default and plays. With listedAlone the source also
// lists no audio track but the one it plays (the lasting states; a transient state lists every track, as the item does).
func checkPlan(t *testing.T, what string, reply doc, pick, dflt int, fallback, listedAlone bool) {
	t.Helper()
	if refusedPlan(reply) {
		if pick <= 0 || fallback {
			t.Errorf("%s: refused (%v), want a plan that plays", what, reply["ErrorCode"])
		}
		return
	}
	source := sourceOf(reply)
	played := playedTrack(t, source, dflt)
	if pick > 0 && !fallback && played != pick {
		t.Errorf("%s: the plan plays track %d for a pick of %d (it must play the pick or be refused)", what, played, pick)
	}
	if fallback && played != dflt {
		t.Errorf("%s: plays track %d, want the file's default %d", what, played, dflt)
	}
	if got := chosen(source); got != played {
		t.Errorf("%s: DefaultAudioStreamIndex %d but the plan plays track %d", what, got, played)
	}
	if listedAlone {
		if got := audioAt(streamsOf(t, source["MediaStreams"])); !slices.Equal(got, []int{played}) {
			t.Errorf("%s: lists Type Audio at %v, want the track it plays (%d) alone", what, got, played)
		}
	}
}

// In each lasting state PlaybackInfo lists no audio track it would not play, and a pick of a track it cannot deliver is
// refused or played as that track: a build that lists the four tracks and plays track 1 whatever is picked fails (the
// reviewer's finding on JS-147, D-029). German's saved language (track 2) is not a pick: it falls back to the default.
func TestWhereAPickCannotBeDeliveredPlaybackInfoListsNoTrackItWouldNotPlay(t *testing.T) {
	for _, tc := range lastingStates {
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
			german := f.listener(t, "german", "deu", false)
			plain := f.listener(t, "plain", "", true)
			tc.break_(t, f, film)
			for replica := range 2 {
				for _, tr := range []struct {
					who      viewer
					pick     int
					fallback bool
				}{
					{plain, 0, false}, {german, 0, true},
					{plain, 1, false}, {plain, 2, false}, {plain, 3, false}, {plain, 4, false},
					{german, 3, false}, {german, 4, false}, {german, 2, true},
				} {
					extra := map[string]any{}
					if tr.pick > 0 {
						extra["AudioStreamIndex"] = tr.pick
					}
					what := fmt.Sprintf("replica %d, %s picks %d", replica, tr.who.name, tr.pick)
					checkPlan(t, what, f.ask(t, replica, tr.who, film, extra), tr.pick, 1, tr.fallback, true)
					// The same with the pick as jellyfin-web writes it, a string.
					if tr.pick > 0 {
						extra["AudioStreamIndex"] = strconv.Itoa(tr.pick)
						checkPlan(t, what+" (as text)", f.ask(t, replica, tr.who, film, extra), tr.pick, 1, tr.fallback, true)
					}
				}
			}
		})
	}
}

// A pick that cannot be delivered right now, because no worker has the cores free or the transcode of that very spec just
// failed, plays the picked track or is refused at PlaybackInfo (jellyfin-web then shows "cannot be played at this time"
// within seconds: test/e2e shows the dialog), never another track; the viewer's saved language still falls back to the
// file's default and plays. The item lists every track in these states (D-029), PlaybackInfo too. This is the server's
// half; the e2e scenario's is jellyfin-web's.
func TestAPickThatCannotBeDeliveredNowIsRefusedOrPlayedNeverAnotherTrack(t *testing.T) {
	// A worker with 0.04 cores has less than the 0.05 a copy costs (playback.CopyCost): the cores are taken, as when every
	// worker runs a full load. The state differs from "no cores" above, where the worker declares none and is not counted.
	busy := func(t *testing.T, f *audioFixture, film string) {
		mustExec(t, f.pool, "TRUNCATE transcode_nodes")
		mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, cores, output_version) VALUES ('w1', 'worker', 0.04, "+strconv.Itoa(playback.OutputVersion)+")")
	}
	failed := func(t *testing.T, f *audioFixture, film string) {
		// The spec of each pick asked below failed just now: its hash is that of the plan a healthy server offers for it.
		for _, track := range []int{2, 3, 4} {
			plain := f.listener(t, fmt.Sprintf("probe%d", track), "", true)
			source := sourceOf(f.ask(t, 0, plain, film, map[string]any{"AudioStreamIndex": track}))
			values := lowerKeys(mustQuery(t, source["TranscodingUrl"].(string)))
			stored, found, err := f.apis[0].Items.PlaybackSource(context.Background(), film, values.Get("mediasourceid"))
			if err != nil || !found {
				t.Fatal(err, found)
			}
			media := playbackSource(stored)
			media.Keyframes, media.VideoEnd = stored.Keyframes, stored.VideoEnd
			params, err := playback.ParseParams(values, media)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := playback.NewSpec(film, params, media)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, f.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, ended_at, output_version)
				VALUES ($1, '{}', 'o', 0, 0, 'failed', 'ffmpeg exited 1', now(), `+strconv.Itoa(playback.OutputVersion)+`)`, spec.Hash())
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *audioFixture, film string)
	}{{"no worker has the cores free", busy}, {"the transcode of that spec just failed", failed}} {
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
			german := f.listener(t, "german", "deu", false)
			plain := f.listener(t, "plain", "", true)
			tc.setup(t, f, film)
			for replica := range 2 {
				for _, tr := range []struct {
					who      viewer
					pick     int
					fallback bool
				}{
					{plain, 2, false}, {plain, 3, false}, {plain, 4, false}, {german, 3, false}, {german, 4, false}, {german, 2, true}, {plain, 1, false},
				} {
					what := fmt.Sprintf("replica %d, %s picks %d", replica, tr.who.name, tr.pick)
					checkPlan(t, what, f.ask(t, replica, tr.who, film, map[string]any{"AudioStreamIndex": tr.pick}), tr.pick, 1, tr.fallback, false)
					checkPlan(t, what+" (as text)", f.ask(t, replica, tr.who, film, map[string]any{"AudioStreamIndex": strconv.Itoa(tr.pick)}), tr.pick, 1, tr.fallback, false)
				}
				// Nothing was picked: the file's default plays, or the saved language as a copy is refused only where it cannot be made.
				checkPlan(t, fmt.Sprintf("replica %d, plain viewer picks nothing", replica), f.ask(t, replica, plain, film, nil), 0, 1, false, false)
			}
		})
	}
}

// ---- Where the item and PlaybackInfo could disagree about whether a pick can be delivered (review of the first checks).

// The item and PlaybackInfo give one answer to "can a transcode deliver a picked track" in every state a worker's row can
// be in, not only the states D-029 names: for the same viewer, the Type Audio indexes the item lists and the track it
// names are those PlaybackInfo lists and names with no track picked. The state that told them apart was a version roll:
// the only live worker is of another output version, which Jobs.Unavailable does not refuse (the job is offered and waits
// for a worker of the version) while the item took it as no worker, so a German viewer's menu listed track 1 alone and
// played English while PlaybackInfo listed all four and named 2. Which side is right is not decided here (D-029 does not
// name the state): the check pins that they do not differ, so a build that moves either predicate alone fails.
func TestTheItemAndPlaybackInfoAnswerAlikeWhenTheOnlyWorkerIsOfAnotherOutputVersion(t *testing.T) {
	other := strconv.Itoa(playback.OutputVersion + 1)
	current := strconv.Itoa(playback.OutputVersion)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *audioFixture, film string)
	}{
		{"the only live worker is of another output version", func(t *testing.T, f *audioFixture, film string) {
			mustExec(t, f.pool, "TRUNCATE transcode_nodes")
			mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('old', 'worker', 2, 4, "+other+")")
		}},
		{"a worker of another version and one of this version are alive", func(t *testing.T, f *audioFixture, film string) {
			mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('old', 'worker', 2, 4, "+other+")")
			mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('new', 'worker', 2, 4, "+current+")")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
			german := f.listener(t, "german", "deu", false)
			plain := f.listener(t, "plain", "", true)
			tc.setup(t, f, film)
			for replica := range 2 {
				for _, who := range []viewer{german, plain} {
					info := sourceOf(f.ask(t, replica, who, film, nil))
					reply := f.item(t, replica, who, film, false)
					item := sourceOf(reply)
					want := audioAt(streamsOf(t, info["MediaStreams"]))
					for list, streams := range listsOf(t, reply) {
						if got := audioAt(streams); !slices.Equal(got, want) {
							t.Errorf("replica %d, %s, %s: the item lists Type Audio at %v, PlaybackInfo at %v", replica, who.name, list, got, want)
						}
					}
					if got, want := chosen(item), chosen(info); got != want {
						t.Errorf("replica %d, %s: the item names %d, PlaybackInfo %d", replica, who.name, got, want)
					}
					// Whatever the menu lists, a pick of it plays that track or is refused: never another track.
					for _, pick := range want {
						checkPlan(t, fmt.Sprintf("replica %d, %s picks %d", replica, who.name, pick), f.ask(t, replica, who, film, map[string]any{"AudioStreamIndex": pick}), pick, 1, false, false)
					}
				}
			}
		})
	}
}

// A client that switches tracks itself (the capture's AnyClientProfile: no IsSecondaryAudio condition, as docs/transcoding.md
// says of mpv clients) direct plays the saved language's track wherever the file holds it, so in the lasting states PlaybackInfo
// still names the viewer's track 2 and plays it directly, as the capture and Jellyfin 12.4 do (in the fifth state, a film
// with no probed duration, PlaybackInfo plays the file's default for every client, which is not changed here); only the item, which
// sees no profile, lists the file's default alone for browsers (D-029). This pins PlaybackInfo's half so that no build
// repairs the item's by taking the saved language from a client that plays it. Whether the item should name that track
// for such a client in these states is a trade-off the lead brings to the product owner (D-029 names browser viewers
// alone): no check here decides it.
func TestInTheLastingStatesAClientThatSwitchesTracksItselfStillDirectPlaysTheSavedLanguage(t *testing.T) {
	for _, tc := range lastingStates {
		if tc.name == "the film has no probed duration" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			f := audioEnv(t)
			f.profile = loadAudioCapture(t).AnyClientProfile
			film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
			german := f.listener(t, "german", "deu", false)
			tc.break_(t, f, film)
			for replica := range 2 {
				source := sourceOf(f.ask(t, replica, german, film, nil))
				if how, _ := copyOf(t, source); how != "direct" {
					t.Errorf("replica %d: the plan is a %s, want a direct play of the file", replica, how)
				}
				if got := chosen(source); got != 2 {
					t.Errorf("replica %d: names %d, want the saved language's track 2", replica, got)
				}
				if got := audioAt(streamsOf(t, source["MediaStreams"])); !slices.Contains(got, 2) {
					t.Errorf("replica %d: lists Type Audio at %v, want the track it names (2) among them", replica, got)
				}
			}
		})
	}
}

// While the only live worker is of another output version (a rolling deploy out of order, docs/running.md) no transcode
// can be made, so a pick of a track other than the file's default is refused (D-029: a pick no transcode can deliver plays
// that track or is refused, never another track, and never a TranscodingUrl no worker of this version can serve), and a
// viewer whose saved language cannot be delivered falls back to the file's default and plays it directly. With a worker of
// this version alive as well the same pick is planned, so a build that refuses always fails the last half. A build that
// never takes the "no worker of this version" branch of planPlayback offers the old worker's version a job for the pick.
func TestAPickIsRefusedAndASavedLanguageFallsBackWhenOnlyAWorkerOfAnotherOutputVersionIsAlive(t *testing.T) {
	other := strconv.Itoa(playback.OutputVersion + 1)
	current := strconv.Itoa(playback.OutputVersion)
	f := audioEnv(t)
	film, _ := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
	german := f.listener(t, "german", "deu", false)
	plain := f.listener(t, "plain", "", true)
	mustExec(t, f.pool, "TRUNCATE transcode_nodes")
	mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('old', 'worker', 2, 4, "+other+")")
	for replica := range 2 {
		for _, pick := range []int{2, 3, 4} {
			for _, value := range []any{pick, strconv.Itoa(pick)} {
				what := fmt.Sprintf("replica %d, plain viewer picks %v (%T), only another version's worker", replica, value, value)
				reply := f.ask(t, replica, plain, film, map[string]any{"AudioStreamIndex": value})
				if refusedPlan(reply) {
					continue
				}
				if url, _ := sourceOf(reply)["TranscodingUrl"].(string); url != "" {
					t.Errorf("%s: offers %s, which no worker of this version can serve", what, url)
				}
				checkPlan(t, what, reply, pick, 1, false, false)
			}
		}
		reply := f.ask(t, replica, german, film, nil)
		what := fmt.Sprintf("replica %d, german viewer picks nothing, only another version's worker", replica)
		if refusedPlan(reply) {
			t.Errorf("%s: refused (%v), want the file's default direct played", what, reply["ErrorCode"])
		} else if how, _ := copyOf(t, sourceOf(reply)); how != "direct" {
			t.Errorf("%s: the plan is a %s, want a direct play of the file's default", what, how)
		} else {
			checkPlan(t, what, reply, 0, 1, true, false)
		}
	}
	mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('new', 'worker', 2, 4, "+current+")")
	for replica := range 2 {
		what := fmt.Sprintf("replica %d, plain viewer picks 3, a worker of this version alive", replica)
		reply := f.ask(t, replica, plain, film, map[string]any{"AudioStreamIndex": 3})
		if refusedPlan(reply) {
			t.Errorf("%s: refused (%v), want the pick planned", what, reply["ErrorCode"])
			continue
		}
		if url, _ := sourceOf(reply)["TranscodingUrl"].(string); url == "" {
			t.Errorf("%s: no TranscodingUrl, want a transcode of track 3", what)
		}
		checkPlan(t, what, reply, 3, 1, false, false)
	}
}
