package differential

import (
	"reflect"
	"testing"
)

func items(ids ...string) any {
	list := []any{}
	for _, id := range ids {
		list = append(list, map[string]any{"Id": id})
	}
	return map[string]any{"Items": list}
}

func TestIDDifferencesSeeADroppedExtraOrReorderedItem(t *testing.T) {
	for name, tc := range map[string]struct {
		jellyscale any
		ordered    bool
		want       bool
	}{
		"same":                    {items("a", "b"), true, false},
		"dropped":                 {items("a"), false, true},
		"extra":                   {items("a", "b", "c"), false, true},
		"reordered when ordered":  {items("b", "a"), true, true},
		"reordered when unsorted": {items("b", "a"), false, false},
	} {
		got := IDDifferences(items("a", "b"), tc.jellyscale, tc.ordered)
		if (len(got) > 0) != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
		if len(got) > 0 && got[0].Path != "Items (ids)" {
			t.Errorf("%s: path %q", name, got[0].Path)
		}
	}
}

// Hiding every field of a listing (as a known gap does) still leaves the order:
// an entry that pins the Ids hides that answer only, so a new wrong order fails.
func TestPinnedIDsHideOnlyThePinnedAnswer(t *testing.T) {
	a, err := ParseAllowlist(`
- {requests: [r], path: "Items (ids)", jellyscale: [x, y], kind: gap, task: JS-068, reason: r}
`)
	if err != nil {
		t.Fatal(err)
	}
	known := IDDifferences(items("y", "x"), items("x", "y"), true)
	if left := a.Filter("r", known); len(left) != 0 {
		t.Errorf("the pinned answer is not allowed: %v", left)
	}
	worse := IDDifferences(items("y", "x"), items("y", "z"), true)
	if left := a.Filter("r", worse); !reflect.DeepEqual(left, worse) {
		t.Errorf("a different answer was allowed: %v", left)
	}
	if left := a.Filter("other", known); len(left) != 1 {
		t.Errorf("the entry hid another request: %v", left)
	}
}

func TestAnIDsEntryMustPinAndOnlyAnIDsEntryMay(t *testing.T) {
	for _, text := range []string{
		`- {requests: [r], path: "Items (ids)", kind: improvement, reason: r}`,
		`- {requests: [r], path: Foo, jellyscale: [x], kind: improvement, reason: r}`,
	} {
		if _, err := ParseAllowlist(text); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
}
