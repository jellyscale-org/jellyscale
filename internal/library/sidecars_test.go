package library

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/store"
)

// The names and results are those captured from Jellyfin 12.4
// (compat/captures/subtitles-12.4.md): the language, flags and menu label it
// gave each file beside "Ext Film (2024).mkv".
func TestSubtitleNamesReadLikeJellyfin(t *testing.T) {
	const video = "Movies/Ext Film (2024)/Ext Film (2024).mkv"
	for _, c := range []struct {
		file           string
		codec          string
		language       string
		title          string
		def, forced, h bool
		display        string
	}{
		{"en.srt", "subrip", "eng", "", false, false, false, "English - SUBRIP - External"},
		{"English.srt", "subrip", "eng", "", false, false, false, "English - SUBRIP - External"},
		{"eng.hi.srt", "subrip", "eng", "", false, false, true, "English - Hearing Impaired - SUBRIP - External"},
		{"de.sdh.srt", "subrip", "deu", "", false, false, true, "German - Hearing Impaired - SUBRIP - External"},
		{"deu.srt", "subrip", "deu", "", false, false, false, "German - SUBRIP - External"},
		{"es.forced.srt", "subrip", "spa", "", false, true, false, "Spanish - Forced - SUBRIP - External"},
		{"en.Forced.srt", "subrip", "eng", "", false, true, false, "English - Forced - SUBRIP - External"},
		{"foreign.srt", "subrip", "", "", false, true, false, "Undefined - Forced - SUBRIP - External"},
		{"fr.default.srt", "subrip", "fra", "", true, false, false, "French - Default - SUBRIP - External"},
		{"ja.forced.default.sdh.srt", "subrip", "jpn", "", true, true, true, "Japanese - Hearing Impaired - Default - Forced - SUBRIP - External"},
		{"it.vtt", "webvtt", "ita", "", false, false, false, "Italian - WEBVTT - External"},
		{"ja.ass", "ass", "jpn", "", false, false, false, "Japanese - ASS - External"},
		{"ko.ssa", "ass", "kor", "", false, false, false, "Korean - ASS - External"},
		{"nl.srt", "subrip", "nld", "", false, false, false, "Dutch - SUBRIP - External"},
		{"pt-BR.srt", "subrip", "pt-br", "", false, false, false, "Portuguese (Brazil) - SUBRIP - External"},
		{"srt", "subrip", "", "", false, false, false, "Undefined - SUBRIP - External"},
		{"cc.srt", "subrip", "", "", false, false, true, "Undefined - Hearing Impaired - SUBRIP - External"},
		{"en.commentary.srt", "subrip", "eng", "commentary", false, false, false, "commentary - English - SUBRIP - External"},
		{"commentary.es.srt", "subrip", "spa", "commentary", false, false, false, "commentary - Spanish - SUBRIP - External"},
		{"en_US.srt", "subrip", "", "en_US", false, false, false, "en_US - Undefined - SUBRIP - External"},
		{"zz.srt", "subrip", "", "zz", false, false, false, "zz - Undefined - SUBRIP - External"},
		// Observed on Jellyfin 12.4 (subtitles-12.4.md): a code it has no name for
		// is a word of the title, and a few are languages with names of their own.
		{"cht.srt", "subrip", "", "cht", false, false, false, "cht - Undefined - SUBRIP - External"},
		{"qaa.srt", "subrip", "", "qaa", false, false, false, "qaa - Undefined - SUBRIP - External"},
		{"und.srt", "subrip", "und", "", false, false, false, "Undetermined - SUBRIP - External"},
		{"mis.srt", "subrip", "mis", "", false, false, false, "Uncoded languages - SUBRIP - External"},
		{"mul.srt", "subrip", "mul", "", false, false, false, "Multiple languages - SUBRIP - External"},
		{"zxx.srt", "subrip", "zxx", "", false, false, false, "No linguistic content - SUBRIP - External"},
		{"fil.srt", "subrip", "fil", "", false, false, false, "Filipino - SUBRIP - External"},
	} {
		key := "Movies/Ext Film (2024)/Ext Film (2024)." + c.file
		got, ok := parseSubtitleName(video, key)
		if !ok {
			t.Errorf("%s is not read as a subtitle of the film", c.file)
			continue
		}
		if got.Codec != c.codec || got.Language != c.language || got.Title != c.title || got.Default != c.def || got.Forced != c.forced || got.Hears != c.h || got.displayTitle() != c.display {
			t.Errorf("%s: %+v, label %q", c.file, got, got.displayTitle())
		}
	}
}

// Only a file named like the film, in its folder, with a known extension, is
// one of its subtitles.
func TestOnlyFilesNamedLikeTheFilmAreItsSubtitles(t *testing.T) {
	const video = "Movies/Film (2024)/Film (2024).mkv"
	for key, want := range map[string]bool{
		"Movies/Film (2024)/Film (2024).en.srt":      true,
		"Movies/Film (2024)/film (2024).EN.SRT":      true, // Case does not matter.
		"Movies/Film (2024)/Film (2024).en.txt":      false,
		"Movies/Film (2024)/Film (2024).en.sub":      false,
		"Movies/Film (2024)/Film (2024)en.srt":       false, // No dot after the name.
		"Movies/Film (2024)/Film (2024) 2.en.srt":    false, // Another film's.
		"Movies/Film (2024)/Other.en.srt":            false,
		"Movies/Film (2024)/Subs/Film (2024).en.srt": false, // Not beside it.
		"Movies/Other/Film (2024).en.srt":            false,
	} {
		if _, ok := parseSubtitleName(video, key); ok != want {
			t.Errorf("%s: %v, want %v", key, ok, want)
		}
	}
}

// The tracks are numbered in file name order, whatever order the store lists
// them in, and the fingerprint moves when a file is added, changed or removed
// and when nothing else does.
func TestSidecarFingerprintFollowsTheFiles(t *testing.T) {
	const video = "Film/Film.mkv"
	b := store.Object{Key: "Film/Film.en.srt", Size: 10, ETag: "1"}
	a := store.Object{Key: "Film/Film.de.srt", Size: 20, ETag: "2"}
	other := store.Object{Key: "Film/Other.en.srt", Size: 30, ETag: "3"}
	base := sidecarsOf(video, []store.Object{b, a, other})
	if len(base.files) != 2 || base.files[0].Key != a.Key || base.files[1].Key != b.Key {
		t.Fatalf("files: %+v", base.files)
	}
	if again := sidecarsOf(video, []store.Object{a, b}); again.fingerprint != base.fingerprint || base.fingerprint == "" {
		t.Errorf("the same files gave %q and %q", base.fingerprint, again.fingerprint)
	}
	changed := b
	changed.ETag = "9"
	resized := b
	resized.Size = 11
	for name, files := range map[string][]store.Object{
		"an ETag changed": {a, changed}, "a size changed": {a, resized}, "a file removed": {a}, "a file added": {a, b, {Key: "Film/Film.fr.srt", Size: 1, ETag: "4"}},
	} {
		if got := sidecarsOf(video, files); got.fingerprint == base.fingerprint {
			t.Errorf("%s and the fingerprint did not move", name)
		}
	}
	if none := sidecarsOf(video, []store.Object{other}); none.fingerprint != "" {
		t.Errorf("no files gave %q", none.fingerprint)
	}
}

func objects(keys ...string) []store.Object {
	var files []store.Object
	for _, key := range keys {
		files = append(files, store.Object{Key: "Film/Film." + key + ".srt", Size: 10, ETag: "e-" + key})
	}
	return files
}

// listing is externalStreams as the scan uses it: the paths and indices it gave
// in index order, the streams to keep for the next scan, and the counter.
func listing(files []store.Object, old []map[string]any, own, counter int) (string, []map[string]any, int) {
	streams, next := externalStreams(store.Object{Key: "Film/Film.mkv"}, files, old, own, counter)
	slices.SortFunc(streams, func(a, b map[string]any) int { return indexOf(a) - indexOf(b) })
	var out []string
	for _, stream := range streams {
		out = append(out, fmt.Sprintf("%d=%s", stream["Index"], strings.TrimSuffix(strings.TrimPrefix(stream["Path"].(string), "Film/Film."), ".srt")))
	}
	return strings.Join(out, " "), streams, next
}

// A file keeps its index while it exists, a removed file's index is not given
// to another, however much later it is added, and the counter is what keeps
// that when the stored streams no longer remember the file.
func TestIndicesAreNeverReused(t *testing.T) {
	// The film has four streams of its own; the files follow in name order.
	got, streams, counter := listing(objects("de", "en"), nil, 4, 0)
	if got != "4=de 5=en" || counter != 6 {
		t.Fatalf("first scan: %s, counter %d", got, counter)
	}
	// A file that sorts before the others takes the next index, not 4.
	got, streams, counter = listing(objects("ar", "de", "en"), streams, 4, counter)
	if got != "4=de 5=en 6=ar" || counter != 7 {
		t.Fatalf("added: %s, counter %d", got, counter)
	}
	// The first file is removed.
	got, streams, counter = listing(objects("ar", "en"), streams, 4, counter)
	if got != "5=en 6=ar" || counter != 7 {
		t.Fatalf("removed: %s, counter %d", got, counter)
	}
	// A scan later, with the removed file gone from the stored streams too, a
	// new file does not take its index.
	got, streams, counter = listing(objects("ar", "en", "fr"), streams, 4, counter)
	if got != "5=en 6=ar 7=fr" || counter != 8 {
		t.Fatalf("added later: %s, counter %d", got, counter)
	}
	// The removed file coming back is a new file.
	got, streams, counter = listing(objects("ar", "de", "en", "fr"), streams, 4, counter)
	if got != "5=en 6=ar 7=fr 8=de" || counter != 9 {
		t.Fatalf("returned: %s, counter %d", got, counter)
	}
	// The file with the highest index is removed, and a later scan, which has
	// no stored stream with that index, adds another: only the counter keeps
	// the removed index from being given again.
	got, streams, counter = listing(objects("ar", "en", "fr"), streams, 4, counter)
	if got != "5=en 6=ar 7=fr" || counter != 9 {
		t.Fatalf("highest removed: %s, counter %d", got, counter)
	}
	got, streams, counter = listing(objects("ar", "en", "fr", "pt"), streams, 4, counter)
	if got != "5=en 6=ar 7=fr 9=pt" || counter != 10 {
		t.Fatalf("added after the highest was removed: %s, counter %d", got, counter)
	}
	// A row from before the counter existed (0) still does not reuse an index
	// the stored streams hold.
	if got, _, _ := listing(objects("en", "xx"), streams, 4, 0); got != "5=en 10=xx" {
		t.Errorf("with no counter: %s", got)
	}
	// A film replaced by one with more streams of its own moves the files out of
	// the way.
	if got, _, _ := listing(objects("en"), streams, 7, counter); got != "10=en" {
		t.Errorf("film with 7 streams: %s", got)
	}
}

// A file that can't be read stays hidden as long as the file is the version
// that was found unreadable, and is listed again when it is edited.
func TestUnreadableLastsUntilTheFileChanges(t *testing.T) {
	files := objects("en")
	_, streams, counter := listing(files, nil, 2, 0)
	streams[0]["Unreadable"] = true
	again, next := externalStreams(store.Object{Key: "Film/Film.mkv"}, files, streams, 2, counter)
	if again[0]["Unreadable"] != true || indexOf(again[0]) != 2 || next != counter {
		t.Errorf("same file: %v", again)
	}
	edited := []store.Object{{Key: files[0].Key, Size: 11, ETag: files[0].ETag}}
	changed, _ := externalStreams(store.Object{Key: "Film/Film.mkv"}, edited, streams, 2, counter)
	if changed[0]["Unreadable"] != nil || indexOf(changed[0]) != 2 || changed[0]["Fingerprint"] == again[0]["Fingerprint"] {
		t.Errorf("edited file: %v", changed)
	}
	// It is out of the lists clients get.
	if got := playableStreams(again); len(got) != 0 {
		t.Errorf("an unreadable file is listed: %v", got)
	}
	if got := playableStreams(changed); len(got) != 1 || got[0]["Fingerprint"] != nil {
		t.Errorf("a changed file: %v", got)
	}
}

// An attached font takes an ffprobe index but is listed apart, so the file
// beside the film is numbered where the tracks end. A client reads the default
// subtitle as a position in MediaStreams as well as an Index, and the two have
// to agree for the file to be started with.
func TestFilesAreNumberedWhereTheListedTracksEnd(t *testing.T) {
	film := []map[string]any{
		{"Index": 0, "Type": "Video"}, {"Index": 1, "Type": "Audio"},
		{"Index": 2, "Type": "Attachment", "MimeType": "font/ttf"}, {"Index": 3, "Type": "Attachment", "MimeType": "font/ttf"},
	}
	external, next := externalStreams(store.Object{Key: "Film/Film.mkv"}, objects("en", "fr"), nil, nextIndex(film), 0)
	if got := indexOf(external[0]); got != 2 || next != 4 {
		t.Fatalf("first file %d, counter %d, want 2 and 4", got, next)
	}
	listed := playableStreams(append(film, external...))
	for position, stream := range listed {
		if indexOf(stream) != position {
			t.Errorf("stream %v is at position %d", stream, position)
		}
	}
}
