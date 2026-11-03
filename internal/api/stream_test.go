package api

import "testing"

// The expected values for the first two are what Jellyfin 12.4 sent
// (compat/captures/small-routes-12.4.json and a non-ASCII and punctuation follow-up).
func TestContentDisposition(t *testing.T) {
	for name, want := range map[string]string{
		"Test (2020).mp4":     `attachment; filename="Test (2020).mp4"; filename*=UTF-8''Test%20%282020%29.mp4`,
		"Café Ünï (2021).mp4": `attachment; filename="Caf_ _n_ (2021).mp4"; filename*=UTF-8''Caf%C3%A9%20%C3%9Cn%C3%AF%20%282021%29.mp4`,
		`a"b\c $'+;.mkv`:      `attachment; filename="ab\\c $'+;.mkv"; filename*=UTF-8''ab%5Cc%20$%27+%3B.mkv`,
	} {
		if got := contentDisposition(name); got != want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
}
