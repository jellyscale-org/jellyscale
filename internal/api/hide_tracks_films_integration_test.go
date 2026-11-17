//go:build integration

package api

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/media"
)

// JS-119, with films made as rows (the original language checks' addFilm) and no ffmpeg: what a track with no language
// is, a film is never without sound, and generated films and settings never move an index.

// rowsEnv is an env with a worker row, so every audio track and every text track is listed, and a way to make viewers.
func rowsEnv(t *testing.T) *langFixture {
	t.Helper()
	e := newEnv(t)
	transcodingOn(t, e)
	// A worker that has not reported for a lease is not alive, and then the text tracks of a film made as rows (stored
	// nowhere) are dropped. The row is renewed as a worker's heartbeat would.
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Millisecond):
				e.pool.Exec(context.Background(), "UPDATE transcode_nodes SET seen_at = now()") //nolint:errcheck
			}
		}
	}()
	t.Cleanup(func() { close(done); <-finished })
	return &langFixture{subtitleFixture: &subtitleFixture{env: e, log: &syncBuffer{}}}
}

// noLanguage are the tags of a track that names no language.
var noLanguage = []string{"", "und", "mis", "mul", "zxx", "qaa", "!!not-a-tag"}

func subtitleRow(index int, language string) media.MediaStream {
	return sub(index, language, false, false)
}

// A track with no language is never hidden, audio or subtitle, whatever the settings: untagged, und, mis, mul, zxx and a
// tag that does not parse are each listed as themselves to a deu viewer who has both switches on. The film is German, so
// a tag read as English (the parser's reading of und and of a malformed tag) would be hidden for matching neither.
func TestATrackWithNoLanguageIsNeverHidden(t *testing.T) {
	f := rowsEnv(t)
	deu := f.viewer(t, "deu", "deu", "deu", bothOn)
	for _, tag := range noLanguage {
		film, _ := addFilm(t, f.env, "No language "+tag, []media.MediaStream{
			vid(""), aud(1, "deu", true), aud(2, tag, false), aud(3, "eng", false), subtitleRow(4, tag), subtitleRow(5, "eng"),
		})
		source := sourceOf(f.info(t, 0, deu, film, nil))
		if have := dataAt(streamsOf(t, source["MediaStreams"])); !slices.Equal(have, []int{3, 5}) {
			t.Errorf("tag %q: Type Data at %v, want exactly [3 5]: the English tracks are hidden and the track with no language is not", tag, have)
		}
	}
}

// Never without sound: the film whose only audio is German is listed that audio to an English viewer, as the default
// audio track, and a film whose audio is all untagged lists all of it.
func TestAFilmIsNeverWithoutSound(t *testing.T) {
	f := rowsEnv(t)
	eng := f.viewer(t, "eng", "eng", "eng", bothOn)
	t.Run("the only audio track is German", func(t *testing.T) {
		film, _ := addFilm(t, f.env, "German only", []media.MediaStream{vid(""), aud(1, "deu", true), subtitleRow(2, "deu"), subtitleRow(3, "eng")})
		source := sourceOf(f.info(t, 0, eng, film, nil))
		streams := streamsOf(t, source["MediaStreams"])
		if streams[1]["Type"] != "Audio" || source["DefaultAudioStreamIndex"] != float64(1) {
			t.Errorf("stream 1 is %v, DefaultAudioStreamIndex %v: want the audio listed and named", streams[1]["Type"], source["DefaultAudioStreamIndex"])
		}
	})
	t.Run("three audio tracks, all untagged", func(t *testing.T) {
		film, _ := addFilm(t, f.env, "Untagged audio", []media.MediaStream{vid(""), aud(1, "", true), aud(2, "", false), aud(3, "", false)})
		streams := streamsOf(t, sourceOf(f.info(t, 0, eng, film, nil))["MediaStreams"])
		if got := strings.Join(typesOfStreams(streams), " "); got != "Video Audio Audio Audio" {
			t.Errorf("listed %s, want every untagged audio track as Audio", got)
		}
	})
}

// The properties of the rule for 1000 generated films and settings: a film with audio lists an audio track and its
// default one, video and tracks with no language and the track a request names are never hidden, nothing is hidden for
// a kind whose switch is off, its language unset or ShowAllTracks on, and the streams and their Index at each position
// are those a viewer with no settings is listed.
func TestGeneratedFilmsAndSettingsNeverMoveAnIndexOrHideTheSoundOrAnUntaggedTrack(t *testing.T) {
	f := rowsEnv(t)
	rng := rand.New(rand.NewSource(119))
	t.Logf("seed 119")
	spellings := []string{"eng", "en", "en-US", "ENG", "deu", "ger", "de", "de-DE", "fra", "fre", "spa", "jpn", "nor", "nob", "zho", "chi", "por"}
	pick := func(list []string) string { return list[rng.Intn(len(list))] }
	tagOf := func() string {
		if rng.Intn(4) == 0 {
			return pick(noLanguage)
		}
		return pick(spellings)
	}

	type settings struct {
		viewer, twin                    viewer // The twin has the same languages and hides nothing.
		audioOn, subtitleOn, showAll    bool
		audioLanguage, subtitleLanguage string
	}
	plain := f.baseline(t)
	var viewers []settings
	for i := range 40 {
		s := settings{audioLanguage: pick([]string{"", "eng", "deu", "ger", "fra", "OriginalLanguage"}), subtitleLanguage: pick([]string{"", "eng", "deu", "spa", "jpn"})}
		prefs := map[string]any{}
		if s.audioLanguage != "" && rng.Intn(3) > 0 {
			s.audioOn, prefs["HideOtherAudio"] = true, true
		}
		if s.subtitleLanguage != "" && rng.Intn(3) > 0 {
			s.subtitleOn, prefs["HideOtherSubtitles"] = true, true
		}
		for _, key := range []string{"KeepAudioLanguages", "KeepSubtitleLanguages"} {
			if rng.Intn(2) == 0 {
				prefs[key] = []string{pick(spellings), pick(spellings)}
			}
		}
		if rng.Intn(6) == 0 {
			s.showAll, prefs["ShowAllTracks"] = true, true
		}
		raw := "{}"
		if len(prefs) > 0 {
			raw = canon(prefs)
		}
		s.viewer = f.viewer(t, fmt.Sprintf("generated%d", i), s.audioLanguage, s.subtitleLanguage, raw)
		s.twin = f.viewer(t, fmt.Sprintf("twin%d", i), s.audioLanguage, s.subtitleLanguage, "{}")
		viewers = append(viewers, s)
	}

	hiddenSeen := 0
	for n := range 1000 {
		var streams []media.MediaStream
		streams = append(streams, vid(tagOf()))
		audio, subs := rng.Intn(7), rng.Intn(7)
		flags := rng.Intn(3) // None, one or several tracks carry the default flag.
		for i := range audio {
			isDefault := flags == 2 && rng.Intn(2) == 0 || flags == 1 && i == rng.Intn(audio)
			streams = append(streams, aud(len(streams), tagOf(), isDefault))
		}
		for range subs {
			streams = append(streams, subtitleRow(len(streams), tagOf()))
		}
		film, _ := addFilm(t, f.env, fmt.Sprintf("Generated %d", n), streams)

		extra := map[string]any{}
		// A request names a track only through the parameter of its own kind: AudioStreamIndex naming a
		// subtitle, or SubtitleStreamIndex naming audio, names nothing and the track may stay hidden.
		namedAudio, namedSubtitle := map[int]bool{}, map[int]bool{}
		for _, key := range []string{"AudioStreamIndex", "SubtitleStreamIndex"} {
			switch rng.Intn(4) {
			case 0, 1: // Naming nothing.
			case 2:
				index := rng.Intn(len(streams) + 2)
				extra[key] = index
				if key == "AudioStreamIndex" {
					namedAudio[index] = true
				} else {
					namedSubtitle[index] = true
				}
			default:
				extra[key] = []int{-1, 99}[rng.Intn(2)]
			}
		}
		s := viewers[rng.Intn(len(viewers))]
		label := fmt.Sprintf("film %d %v, viewer %s (audio %q on %v, subtitle %q on %v, showAll %v), asking %v", n, streams, s.viewer.name,
			s.audioLanguage, s.audioOn, s.subtitleLanguage, s.subtitleOn, s.showAll, extra)

		base := sourceOf(f.info(t, 0, plain, film, extra))
		got := sourceOf(f.info(t, 0, s.viewer, film, extra))
		// The audio language moves the default and the decision (JS-121), so those are compared with a viewer of the same languages.
		twin := sourceOf(f.info(t, 0, s.twin, film, extra))
		bs, gs := streamsOf(t, base["MediaStreams"]), streamsOf(t, got["MediaStreams"])
		if len(bs) != len(gs) {
			t.Fatalf("%s: %d streams, a viewer with no settings gets %d", label, len(gs), len(bs))
		}
		listedAudio := 0
		for i := range gs {
			if gs[i]["Index"] != bs[i]["Index"] {
				t.Fatalf("%s: position %d holds Index %v, a viewer with no settings gets %v", label, i, gs[i]["Index"], bs[i]["Index"])
			}
			if gs[i]["Type"] == "Audio" {
				listedAudio++
			}
			if gs[i]["Type"] != "Data" {
				if canon(gs[i], s.viewer.token) != canon(bs[i], plain.token) {
					t.Fatalf("%s: stream %d is not what a viewer with no settings gets: %s", label, i, canon(gs[i], s.viewer.token))
				}
				continue
			}
			hiddenSeen++
			index := int(gs[i]["Index"].(float64))
			kind := bs[i]["Type"]
			language := ""
			if index < len(streams) {
				language = streams[index].Language
			}
			switch {
			case kind != "Audio" && kind != "Subtitle":
				t.Fatalf("%s: stream %d is a %v listed as Data", label, i, kind)
			case slices.Contains(noLanguage, language):
				t.Fatalf("%s: stream %d has the tag %q, which names no language, and is hidden", label, i, language)
			case kind == "Audio" && namedAudio[index], kind == "Subtitle" && namedSubtitle[index]:
				t.Fatalf("%s: stream %d is named by the request and hidden", label, i)
			case s.showAll:
				t.Fatalf("%s: stream %d is hidden with ShowAllTracks on", label, i)
			case kind == "Audio" && !s.audioOn, kind == "Subtitle" && !s.subtitleOn:
				t.Fatalf("%s: a %v is hidden for a viewer whose switch for it is off or whose language is not saved", label, kind)
			}
		}
		baseAudio := 0
		for _, b := range bs {
			if b["Type"] == "Audio" {
				baseAudio++
			}
		}
		if baseAudio > 0 && listedAudio == 0 {
			t.Fatalf("%s: the film has %d audio tracks and none is listed as Audio", label, baseAudio)
		}
		if value, ok := got["DefaultAudioStreamIndex"].(float64); ok && value >= 0 && int(value) < len(gs) && gs[int(value)]["Type"] != "Audio" {
			t.Fatalf("%s: the default audio track %v is not listed as Audio", label, value)
		}
		checkDefaults(t, label, got)
		for _, key := range []string{"DefaultAudioStreamIndex", "DefaultSubtitleStreamIndex", "SupportsDirectPlay", "SupportsTranscoding", "IsOriginal"} {
			if got[key] != twin[key] {
				t.Fatalf("%s: %s = %v, a viewer with the same languages and no hiding gets %v", label, key, got[key], twin[key])
			}
		}
	}
	if hiddenSeen < 100 {
		t.Errorf("only %d tracks were hidden in 1000 films: the generated settings hide too little to test anything", hiddenSeen)
	}
}
