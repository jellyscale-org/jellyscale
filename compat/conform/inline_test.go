package conform_test

import (
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

// BaseItemDto.ImageBlurHashes is an object written inline whose thirteen image types the document
// leaves non-nullable with no default; Jellyfin sends only the types an item has, and the Kotlin SDK
// models it as a map. Applying the implicit rule to it would hold jellyscale to something Jellyfin
// breaks (the first build of this checker reported 2561 violations of it on Jellyfin's own answers).
func TestAnInlineObjectRequiresOnlyWhatItLists(t *testing.T) {
	c := conform.NewChecker(realSchema(t), conform.Rules{})
	body := `{"Items":[{"Id":"8f1d6c1e3f0a4a539b0e2b6f5f3a9c11","ImageBlurHashes":{"Primary":{"tag":"hash"}}}],"TotalRecordCount":1,"StartIndex":0}`
	vs := c.Check("listing", "GET", "/Items", 200, "application/json", []byte(body))
	for _, v := range vs {
		if strings.Contains(v.JSONPath, "ImageBlurHashes") || v.Field == "ImageBlurHashes" {
			t.Errorf("a violation inside ImageBlurHashes: %v", v)
		}
	}
	if len(vs) == 0 {
		t.Error("the item has none of the fields BaseItemDto requires, yet nothing was reported: the check did not run")
	}
}
