//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// A chapter is listed with an ImageTag only while its picture can be served:
// the library's option is on, the job has finished (not while it is making
// them), the picture is of the file the item has now, and the chapter has one. The tag is made when the DTO is, so turning the option
// off stops it at once, and the picture is served by the stream role as stored
// (compat/captures/chapter-images-12.4.md).
func TestChapterImagesAreListedAndServedOnlyWhileTheyCanBe(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) { checkChapterImages(t, newEnvOn(t, target)) })
	}
}

func checkChapterImages(t *testing.T, e *env) {
	assets, err := store.Open(e.uri)
	if err != nil {
		t.Fatal(err)
	}
	picture := append([]byte("\xff\xd8 not decoded by the server "), bytes.Repeat([]byte("x"), 100)...)
	first := "chapters/" + e.item + "/dir/0.jpg"
	if err := assets.Put(context.Background(), first, bytes.NewReader(picture)); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.pool, `UPDATE items SET dto = dto || '{"Chapters":[
		{"StartPositionTicks":0,"Name":"Opening","ImageDateModified":"0001-01-01T00:00:00.0000000Z"},
		{"StartPositionTicks":200000000,"Name":"Chapter 2","ImageDateModified":"0001-01-01T00:00:00.0000000Z"}]}'`)
	// A job is making the pictures: the row says making, whatever keys it has.
	mustExec(t, e.pool, "TRUNCATE chapter_images")
	mustExec(t, e.pool, `INSERT INTO chapter_images (source_id, library_id, etag, state, attempts, keys)
		SELECT $1, library_id, 'abc123', 'making', 1, $2 FROM items WHERE id = $1`, e.item, []string{first, ""})

	tags := func(path string) []any {
		t.Helper()
		_, raw := e.do(t, 0, "GET", path, e.token, nil)
		var found struct {
			Chapters []map[string]any
			Items    []struct{ Chapters []map[string]any }
		}
		if err := json.Unmarshal(raw, &found); err != nil {
			t.Fatalf("%s: %s", path, raw)
		}
		chapters := found.Chapters
		if len(found.Items) > 0 {
			chapters = found.Items[0].Chapters
		}
		got := make([]any, len(chapters))
		for i, chapter := range chapters {
			got[i] = chapter["ImageTag"]
		}
		return got
	}
	paths := []string{"/Items/" + e.item + "?userId=" + e.user, "/Items?Recursive=true&Fields=Chapters"}
	image := func(path string, replica int) (int, []byte, string) {
		response, body := e.do(t, replica, "GET", "/Items/"+e.item+"/Images/"+path, "", nil)
		return response.StatusCode, body, response.Header.Get("Cache-Control")
	}

	// The option is off, which is the default: a stored picture is not listed or served.
	for _, path := range paths {
		if got := tags(path); fmt.Sprint(got) != "[<nil> <nil>]" {
			t.Errorf("%s with the option off: tags %v", path, got)
		}
	}
	if status, _, _ := image("Chapter/0", 0); status != 404 {
		t.Errorf("a picture served with the option off: %d", status)
	}

	mustExec(t, e.pool, "UPDATE libraries SET chapter_images = true")
	for _, path := range paths {
		if got := tags(path); fmt.Sprint(got) != "[<nil> <nil>]" {
			t.Errorf("%s while a job is making the pictures: tags %v", path, got)
		}
	}
	if status, _, _ := image("Chapter/0", 0); status != 404 {
		t.Errorf("a picture served while a job is making them: %d", status)
	}
	mustExec(t, e.pool, "UPDATE chapter_images SET state = 'done'")
	wantTag := items.ChapterTag(first)
	for _, path := range paths {
		if got := tags(path); fmt.Sprint(got) != fmt.Sprintf("[%s <nil>]", wantTag) {
			t.Errorf("%s: tags %v, want [%s <nil>]: only the chapter with a picture", path, got, wantTag)
		}
	}
	for replica := range 2 {
		status, body, cache := image("Chapter/0?maxWidth=400&tag="+wantTag+"&quality=90", replica)
		if status != 200 || !bytes.Equal(body, picture) || cache != "public, max-age=31536000, immutable" {
			t.Errorf("replica %d: picture %d, %d bytes, Cache-Control %q", replica, status, len(body), cache)
		}
	}
	if status, body, cache := image("Chapter", 0); status != 200 || !bytes.Equal(body, picture) || cache != "public" {
		t.Errorf("a bare /Images/Chapter: %d, %d bytes, Cache-Control %q; Jellyfin answers it with the first picture, not kept", status, len(body), cache)
	}
	if status, _, cache := image("Chapter/0?tag=0123456789abcdef0123456789abcdef", 0); status != 200 || cache != "public" {
		t.Errorf("a picture asked for under another tag: %d, Cache-Control %q; it must not be kept for good under that tag", status, cache)
	}
	for name, path := range map[string]string{"no picture for it": "Chapter/1", "past the last": "Chapter/2", "negative": "Chapter/-1", "not a number": "Chapter/first", "2^31": "Chapter/2147483648", "2^31-1": "Chapter/2147483647", "2^63": "Chapter/9223372036854775808"} {
		if status, _, _ := image(path, 0); status != 404 {
			t.Errorf("%s: %d, want 404", name, status)
		}
	}
	if got := tags("/Items?Recursive=true"); got == nil || len(got) != 0 {
		t.Errorf("a list that didn't ask for chapters has %v", got)
	}

	// The pictures are of another file than the item has now (it was replaced
	// and the job has not run), or its chapters changed in number.
	mustExec(t, e.pool, "UPDATE media_sources SET object_etag = 'replaced'")
	for _, path := range paths {
		if got := tags(path); fmt.Sprint(got) != "[<nil> <nil>]" {
			t.Errorf("%s for another file's pictures: tags %v", path, got)
		}
	}
	if status, _, _ := image("Chapter/0", 0); status != 404 {
		t.Errorf("another file's picture served: %d", status)
	}
	mustExec(t, e.pool, "UPDATE media_sources SET object_etag = 'abc123'")
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{Chapters}', dto->'Chapters' || '{"StartPositionTicks":300000000,"Name":"Chapter 3"}')`)
	if got := tags(paths[0]); fmt.Sprint(got) != "[<nil> <nil> <nil>]" {
		t.Errorf("chapters changed in number: tags %v", got)
	}
}
