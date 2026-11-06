package items

import "testing"

func TestTagChaptersTagsOnlyChaptersWithAPicture(t *testing.T) {
	chapters := func() map[string]any {
		return map[string]any{"Chapters": []any{map[string]any{"Name": "a"}, map[string]any{"Name": "b"}}}
	}
	tagOf := func(dto map[string]any, i int) any {
		return dto["Chapters"].([]any)[i].(map[string]any)["ImageTag"]
	}

	dto := chapters()
	tagChapters(dto, []string{"chapters/x/0.jpg", ""})
	if tagOf(dto, 0) != ChapterTag("chapters/x/0.jpg") || tagOf(dto, 1) != nil {
		t.Errorf("tags %v %v", tagOf(dto, 0), tagOf(dto, 1))
	}
	if len(ChapterTag("k")) != 32 || ChapterTag("k") == ChapterTag("l") {
		t.Errorf("tag %q", ChapterTag("k"))
	}
	for name, keys := range map[string][]string{"nothing stored": nil, "chapters changed in number": {"chapters/x/0.jpg"}} {
		dto := chapters()
		tagChapters(dto, keys)
		if tagOf(dto, 0) != nil || tagOf(dto, 1) != nil {
			t.Errorf("%s: tagged", name)
		}
	}
	tagChapters(map[string]any{}, []string{"k"}) // A DTO without chapters is left alone.
}
