package api

import (
	"testing"

	"jellyscale/internal/items"
)

func TestWebMKeepsItsContentType(t *testing.T) {
	// ffprobe's "matroska,webm" is stored as mkv; the .webm key marks it WebM.
	for key, want := range map[string]string{
		"film.webm": "video/webm", "Film.WEBM": "video/webm",
		"film.mkv": "video/x-matroska", "film": "video/x-matroska",
	} {
		if got := contentType(items.Source{Key: key, Container: "mkv"}); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if got := contentType(items.Source{Key: "film.webm", Container: "mp4"}); got != "video/mp4" {
		t.Errorf("a non-Matroska container follows the container: %q", got)
	}
}
