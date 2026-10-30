// Package compat holds Jellyfin client conventions that aren't specific to any
// one API domain.
package compat

import (
	"net/http"
	"net/url"
	"strings"
)

// ClientInfo is what a client declares about itself when it authenticates.
type ClientInfo struct {
	Client   string
	Device   string
	DeviceID string
	Version  string
	Token    string
}

// ParseAuth reads credentials from any header or query form clients use.
// Query keys must already be normalized to lowercase.
func ParseAuth(r *http.Request) ClientInfo {
	var info ClientInfo
	for _, name := range []string{"Authorization", "X-Emby-Authorization"} {
		if value := r.Header.Get(name); value != "" {
			if parsed, ok := parseScheme(value); ok {
				info = parsed
				break
			}
		}
	}
	if info.Token != "" {
		return info
	}
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if value := r.Header.Get(name); value != "" {
			info.Token = value
			return info
		}
	}
	query := r.URL.Query()
	for _, name := range []string{"apikey", "api_key"} {
		if value := query.Get(name); value != "" {
			info.Token = value
			return info
		}
	}
	return info
}

func parseScheme(value string) (ClientInfo, bool) {
	scheme, params, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || (!strings.EqualFold(scheme, "MediaBrowser") && !strings.EqualFold(scheme, "Emby")) {
		return ClientInfo{}, false
	}
	var info ClientInfo
	for _, pair := range splitParams(params) {
		key, raw, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		decoded, err := url.QueryUnescape(strings.Trim(strings.TrimSpace(raw), `"`))
		if err != nil {
			decoded = strings.Trim(strings.TrimSpace(raw), `"`)
		}
		// Keys are case-sensitive, matching what clients send.
		switch strings.TrimSpace(key) {
		case "Client":
			info.Client = decoded
		case "Device":
			info.Device = decoded
		case "DeviceId":
			info.DeviceID = decoded
		case "Version":
			info.Version = decoded
		case "Token":
			info.Token = decoded
		}
	}
	return info, true
}

// splitParams splits on commas that sit outside quotes, since a device name
// can contain one.
func splitParams(params string) []string {
	var parts []string
	var current strings.Builder
	quoted := false
	for _, r := range params {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ',' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	return append(parts, current.String())
}
