package compat

import (
	"net/http/httptest"
	"testing"
)

func TestParseAuth(t *testing.T) {
	const web = `MediaBrowser Client="Jellyfin Web", Device="Firefox, on Linux", DeviceId="abc%3D%3D", Version="10.0", Token="tok"`
	cases := []struct {
		name    string
		target  string
		headers map[string]string
		want    ClientInfo
	}{
		{"authorization", "/", map[string]string{"Authorization": web},
			ClientInfo{"Jellyfin Web", "Firefox, on Linux", "abc==", "10.0", "tok"}},
		{"emby header", "/", map[string]string{"X-Emby-Authorization": web},
			ClientInfo{"Jellyfin Web", "Firefox, on Linux", "abc==", "10.0", "tok"}},
		{"legacy scheme", "/", map[string]string{"Authorization": `Emby Token="tok"`}, ClientInfo{Token: "tok"}},
		{"unquoted", "/", map[string]string{"Authorization": `MediaBrowser Token=tok`}, ClientInfo{Token: "tok"}},
		{"token header", "/", map[string]string{"X-Emby-Token": "tok"}, ClientInfo{Token: "tok"}},
		{"mediabrowser token header", "/", map[string]string{"X-MediaBrowser-Token": "tok"}, ClientInfo{Token: "tok"}},
		{"apikey query", "/?apikey=tok", nil, ClientInfo{Token: "tok"}},
		{"api_key query", "/?api_key=tok", nil, ClientInfo{Token: "tok"}},
		{"bearer is not a client scheme", "/", map[string]string{"Authorization": "Bearer tok"}, ClientInfo{}},
		{"client info without token falls through", "/?api_key=tok",
			map[string]string{"Authorization": `MediaBrowser Client="c"`}, ClientInfo{Client: "c", Token: "tok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.target, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := ParseAuth(r); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
