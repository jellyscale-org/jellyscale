package api

import (
	"math"
	"strings"
	"testing"

	"jellyscale/internal/media"
)

// checkMediaFieldKinds fails unless every field a client reads is present, non-null and of
// the kind it reads it as. The fields are typed once, in internal/media.ClientFields.
func checkMediaFieldKinds(t *testing.T, kind string, obj map[string]any, where string) {
	t.Helper()
	for field, want := range media.ClientFields[kind] {
		value, ok := obj[field]
		if !ok {
			t.Errorf("%s: %s has no %s", where, kind, field)
			continue
		}
		switch want {
		case "bool":
			if _, ok := value.(bool); !ok {
				t.Errorf("%s: %s.%s is %v, want a boolean", where, kind, field, value)
			}
		case "integer":
			n, ok := value.(float64)
			if !ok || n != math.Trunc(n) {
				t.Errorf("%s: %s.%s is %v, want an integer", where, kind, field, value)
			}
		default:
			s, ok := value.(string)
			if !ok {
				t.Errorf("%s: %s.%s is %v, want a string", where, kind, field, value)
				continue
			}
			if !strings.Contains(" "+want+" ", " "+s+" ") {
				t.Errorf("%s: %s.%s is %q, want one of %s", where, kind, field, s, want)
			}
		}
	}
}
