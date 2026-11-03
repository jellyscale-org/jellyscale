package subtitles

import (
	"strings"
	"testing"

	"jellyscale/internal/items"
)

// A file beside the film is named by its own fingerprint as well as the film's
// ETag: an edit is read fresh, and another file taking its index is not
// mistaken for it. A track inside the film keeps the key it always had.
func TestKeyOfAFileHasItsFingerprint(t *testing.T) {
	src := items.Source{ID: "s", ETag: "film", Streams: []byte(`[
		{"Index": 2, "Type": "Subtitle"},
		{"Index": 3, "Type": "Subtitle", "IsExternal": true, "Path": "a.srt", "Fingerprint": "one"}]`)}
	inside, beside := Key(src, 2), Key(src, 3)
	if !strings.HasSuffix(inside, "/2.json") || !strings.HasSuffix(beside, "/3-one.json") {
		t.Errorf("keys: %s %s", inside, beside)
	}
	src.Streams = []byte(strings.Replace(string(src.Streams), `"one"`, `"two"`, 1))
	if Key(src, 3) == beside || Key(src, 2) != inside {
		t.Errorf("keys after the file changed: %s %s", Key(src, 3), Key(src, 2))
	}
	if Key(items.Source{ID: "s", ETag: "film"}, 3) == beside {
		t.Error("an index with no stream behind it has the file's key")
	}
}

// A film that can't give up its fonts is remembered as having none under a key
// with its ETag, so the memory ends when the film is replaced.
func TestFontsKeyChangesWithTheFilm(t *testing.T) {
	if FontsKey("s", "one") == FontsKey("s", "two") || FontsKey("s", "one") != FontsKey("s", "one") {
		t.Error("the fonts key does not follow the film's ETag")
	}
}
