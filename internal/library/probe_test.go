package library

import (
	"testing"
)

// Two tracks in one language must read differently in a client's menu.
func TestSubtitleLabelsTellTracksApart(t *testing.T) {
	for _, c := range []struct {
		language, codec     string
		isDefault, isForced bool
		want                string
	}{
		{"eng", "subrip", true, false, "eng - Default - SUBRIP"},
		{"eng", "subrip", false, true, "eng - Forced - SUBRIP"},
		{"", "ass", false, false, "ASS"},
	} {
		if got := displaySubtitle(c.language, c.codec, c.isDefault, c.isForced); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}
