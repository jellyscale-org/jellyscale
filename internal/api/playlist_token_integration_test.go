//go:build integration

package api

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/transcode"
)

// JS-128. A playlist carries the token only when the request carried it in the query. A client that sends its token
// in a header (Fluster refuses any playlist with a credential in it) gets lines with none, and sends its header on
// every playlist and segment request; a client that put the token in the URL (jellyfin-web) gets today's lines.
// What a real Jellyfin 12.4 does is in compat/captures/playlist-token-12.4.md.

const playlistSegments = 25 // transcodeEnv's film at 6 s a segment.

// The spellings of a header token the server reads (compat.ParseAuth).
var headerSpellings = []struct {
	name string
	set  func(token string) []string
}{
	{"Authorization MediaBrowser", func(t string) []string {
		return []string{"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="dev1", Version="1", Token="` + t + `"`}
	}},
	{"Authorization Emby", func(t string) []string {
		return []string{"Authorization", `Emby Client="c", Device="d", DeviceId="dev1", Version="1", Token="` + t + `"`}
	}},
	{"X-Emby-Authorization", func(t string) []string {
		return []string{"X-Emby-Authorization", `MediaBrowser Client="c", Device="d", DeviceId="dev1", Version="1", Token="` + t + `"`}
	}},
	{"X-Emby-Token", func(t string) []string { return []string{"X-Emby-Token", t} }},
	{"X-MediaBrowser-Token", func(t string) []string { return []string{"X-MediaBrowser-Token", t} }},
}

var credentialParam = regexp.MustCompile(`(?i)[?&;](apikey|api_key|token|access_token|x-emby-token)(=|&|"|$)`)

// withoutPairs drops the named parameters (any letter case) from the query of a URL or playlist line, keeping the
// order of the rest and leaving no empty pair behind.
func withoutPairs(line string, names ...string) string {
	path, query, found := strings.Cut(line, "?")
	if !found {
		return line
	}
	suffix := ""
	if strings.HasPrefix(path, "#EXT-X-MAP:URI=\"") && strings.HasSuffix(query, `"`) {
		query, suffix = strings.TrimSuffix(query, `"`), `"`
	}
	var keep []string
	for _, pair := range strings.Split(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		drop := pair == ""
		for _, name := range names {
			drop = drop || strings.EqualFold(key, name)
		}
		if !drop {
			keep = append(keep, pair)
		}
	}
	return path + "?" + strings.Join(keep, "&") + suffix
}

// uriLines are the lines of a playlist that name a URI: the variant, the init segment of the MAP line, each segment.
func uriLines(playlist string) []string {
	var out []string
	for _, line := range strings.Split(playlist, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:URI=\""):
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(line, "#EXT-X-MAP:URI=\""), `"`))
		case line != "" && !strings.HasPrefix(line, "#"):
			out = append(out, line)
		}
	}
	return out
}

type playlistSite struct {
	*env
	source string
	query  string // The TranscodingUrl, token and all.
	header string // The same without its ApiKey pair.
	path   string // /videos/<item>/
	hash   string
}

func newPlaylistSite(t *testing.T) *playlistSite {
	t.Helper()
	e, segments, source := transcodeEnv(t, "hevc", "aac")
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	p := &playlistSite{env: e, source: source, query: firstSource(reply)["TranscodingUrl"].(string), path: "/videos/" + db.DashedID(e.item) + "/"}
	if !strings.Contains(p.query, "ApiKey="+e.token) {
		t.Fatalf("PlaybackInfo's TranscodingUrl has no ApiKey: %s", p.query)
	}
	p.header = withoutPairs(p.query, "ApiKey")
	// Every segment and the init segment, under the hash the spec of these URIs gives.
	p.hash = specHash(t, e, lowerKeys(mustQuery(t, p.header)))
	for n := -1; n < playlistSegments; n++ {
		if err := segments.Put(context.Background(), transcode.SegmentKey(p.hash, n), strings.NewReader(fmt.Sprintf("segment %d", n))); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// get asks one replica, with the headers given (and no other credential).
func (p *playlistSite) get(t *testing.T, replica int, uri string, headers ...string) (int, string, string) {
	t.Helper()
	response, body := p.do(t, replica, "GET", uri, "", nil, headers...)
	return response.StatusCode, response.Header.Get("Content-Type"), string(body)
}

// playlists asks for the master with the request given and then for the main playlist at the master's one line,
// as the player does, with the same headers and no query token of its own.
func (p *playlistSite) playlists(t *testing.T, masterReplica, mainReplica int, master string, headers ...string) (masterBody, mainURI, mainBody string) {
	t.Helper()
	status, ctype, masterBody := p.get(t, masterReplica, master, headers...)
	lines := uriLines(masterBody)
	if status != 200 || ctype != "application/vnd.apple.mpegurl" || len(lines) != 1 || !strings.HasPrefix(lines[0], "main.m3u8?") {
		t.Fatalf("master %s: %d %q\n%s", master, status, ctype, masterBody)
	}
	mainURI = p.path + lines[0]
	status, ctype, mainBody = p.get(t, mainReplica, mainURI, headers...)
	if status != 200 || ctype != "application/vnd.apple.mpegurl" || len(uriLines(mainBody)) != playlistSegments+1 {
		t.Fatalf("main %s: %d %q\n%s", mainURI, status, ctype, mainBody)
	}
	return masterBody, mainURI, mainBody
}

// carries says what in a playlist is a credential: a parameter of that name in any case, even with no value, or the
// token's value as written or percent-encoded (every byte, in either case of the hex).
func (p *playlistSite) carries(playlist string) string {
	if found := credentialParam.FindString(playlist); found != "" {
		return "the parameter " + found
	}
	var upper, lower strings.Builder
	for _, b := range []byte(p.token) {
		fmt.Fprintf(&upper, "%%%02X", b)
		fmt.Fprintf(&lower, "%%%02x", b)
	}
	for _, form := range []string{p.token, url.QueryEscape(p.token), url.PathEscape(p.token), upper.String(), lower.String()} {
		if strings.Contains(playlist, form) {
			return "the token as " + form
		}
	}
	return ""
}

// Today every line carries ApiKey whatever carried the token in, so each spelling fails today.
func TestAPlaylistRequestedWithAHeaderTokenCarriesNoCredential(t *testing.T) {
	p := newPlaylistSite(t)
	for _, spelling := range headerSpellings {
		t.Run(spelling.name, func(t *testing.T) {
			master, _, main := p.playlists(t, 0, 1, p.header, spelling.set(p.token)...)
			if found := p.carries(master); found != "" {
				t.Errorf("master.m3u8 carries %s:\n%s", found, master)
			}
			if found := p.carries(main); found != "" {
				t.Errorf("main.m3u8 carries %s", found)
			}
		})
	}
}

// Header or no header, an empty ApiKey in the query is no token.
func TestAPlaylistRequestedWithAnEmptyQueryTokenCarriesNoCredential(t *testing.T) {
	p := newPlaylistSite(t)
	header := headerSpellings[0].set(p.token)
	for _, key := range []string{"ApiKey", "api_key"} {
		master, _, main := p.playlists(t, 0, 0, p.header+"&"+key+"=", header...)
		if found := p.carries(master + main); found != "" {
			t.Errorf("with an empty %s, the playlists carry %s", key, found)
		}
	}
}

// ParseAuth reads a header before the query, so a header client that also sends another, non-empty ApiKey or api_key
// (a stale one, another user's) is authenticated by its header. The query did not carry the token the request was
// authenticated with, so the lines carry no token, and the foreign value is not echoed either: no ApiKey pair at all.
// A build that writes sessionOf(r).Token whenever the query has a non-empty ApiKey puts the header's token in.
func TestAPlaylistRequestedWithAHeaderTokenAndAForeignQueryTokenCarriesNone(t *testing.T) {
	p := newPlaylistSite(t)
	const foreign = "stale-0123456789abcdef"
	for _, spelling := range headerSpellings {
		for _, key := range []string{"ApiKey", "api_key"} {
			t.Run(spelling.name+" "+key, func(t *testing.T) {
				header := spelling.set(p.token)
				status, _, master := p.get(t, 0, p.header+"&"+key+"="+foreign, header...)
				lines := uriLines(master)
				if status != 200 || len(lines) != 1 {
					t.Fatalf("master: %d\n%s", status, master)
				}
				_, _, main := p.get(t, 1, p.path+lines[0]+"&"+key+"="+foreign, header...)
				if len(uriLines(main)) != playlistSegments+1 {
					t.Fatalf("main: %d URIs\n%s", len(uriLines(main)), main)
				}
				for name, body := range map[string]string{"master": master, "main": main} {
					if found := p.carries(body); found != "" {
						t.Errorf("%s carries %s", name, found)
					}
					if strings.Contains(body, foreign) {
						t.Errorf("%s echoes the foreign query value", name)
					}
				}
			})
		}
	}
}

// The header-requested playlist is the query one minus its ApiKey pair: nothing else is dropped, moved or changed.
func TestAHeaderPlaylistIsTheQueryPlaylistMinusItsApiKey(t *testing.T) {
	p := newPlaylistSite(t)
	queryMaster, queryMainURI, queryMain := p.playlists(t, 0, 1, p.query)
	headerMaster, headerMainURI, headerMain := p.playlists(t, 1, 0, p.header, headerSpellings[3].set(p.token)...)
	for _, c := range []struct{ name, query, header string }{{"master.m3u8", queryMaster, headerMaster}, {"main.m3u8", queryMain, headerMain}} {
		want := strings.Split(c.query, "\n")
		got := strings.Split(c.header, "\n")
		if len(got) != len(want) {
			t.Fatalf("%s: %d lines, the query request's has %d", c.name, len(got), len(want))
		}
		for i := range want {
			if stripped := withoutPairs(want[i], "ApiKey"); got[i] != stripped {
				t.Errorf("%s line %d:\n got %s\nwant %s", c.name, i, got[i], stripped)
				break
			}
		}
	}
	if want := withoutPairs(queryMainURI, "ApiKey"); headerMainURI != want {
		t.Errorf("the master's line: %s, want %s", headerMainURI, want)
	}
}

// Each URI a header-requested playlist lists, asked for as written with the same header, answers as it does with the
// token added to its query: the variant's playlist (which then carries the token, as a query request's does, so it
// is held to the header playlist plus an ApiKey pair), the init segment and each of the 25 segments.
func TestEveryUriOfAHeaderPlaylistAnswersWithTheHeaderAlone(t *testing.T) {
	p := newPlaylistSite(t)
	for _, spelling := range []int{0, 3} {
		header := headerSpellings[spelling].set(p.token)
		_, mainURI, main := p.playlists(t, 0, 1, p.header, header...)

		status, ctype, tokened := p.get(t, 0, mainURI+"&ApiKey="+p.token)
		_, _, plain := p.get(t, 0, mainURI, header...)
		if status != 200 || ctype != "application/vnd.apple.mpegurl" || withoutPairs(tokened, "ApiKey") == "" {
			t.Fatalf("main with a query token: %d %q", status, ctype)
		}
		got, want := strings.Split(plain, "\n"), strings.Split(tokened, "\n")
		for i := range want {
			if i >= len(got) || got[i] != withoutPairs(want[i], "ApiKey") {
				t.Errorf("%s: the playlist with the header alone is not the one with the token minus its pair, at line %d", headerSpellings[spelling].name, i)
				break
			}
		}

		uris := uriLines(main)
		if len(uris) != playlistSegments+1 {
			t.Fatalf("main lists %d URIs, want an init and %d segments", len(uris), playlistSegments)
		}
		for i, uri := range uris {
			replica := i % 2
			status, ctype, body := p.get(t, replica, p.path+uri, header...)
			wantStatus, wantType, wantBody := p.get(t, 1-replica, p.path+uri+"&ApiKey="+p.token)
			if status != 200 || wantStatus != 200 || ctype != "video/mp4" || ctype != wantType || body != wantBody || !strings.HasPrefix(body, "segment ") {
				t.Errorf("%s: %s: %d %q %q, with the token in the query %d %q %q", headerSpellings[spelling].name, uri[:min(len(uri), 22)], status, ctype, body, wantStatus, wantType, wantBody)
			}
		}
	}
}

// A playlist follows its own request: the master's decides nothing for main, on either replica, in either order.
func TestEachPlaylistFollowsItsOwnRequest(t *testing.T) {
	p := newPlaylistSite(t)
	header := headerSpellings[2].set(p.token)

	// A master asked for with ApiKey, then its line asked for with the header alone.
	_, queryMainURI, _ := p.playlists(t, 0, 0, p.query)
	if !strings.Contains(queryMainURI, "ApiKey="+p.token) {
		t.Fatalf("the query master's line has no token: %s", queryMainURI)
	}
	_, main := func() (int, string) {
		s, _, b := p.get(t, 1, withoutPairs(queryMainURI, "ApiKey"), header...)
		return s, b
	}()
	if found := p.carries(main); found != "" || len(uriLines(main)) != playlistSegments+1 {
		t.Errorf("main with the header alone, after a master with ApiKey, carries %q (%d URIs)", found, len(uriLines(main)))
	}

	// A master asked for with the header alone, then its line asked for with ApiKey.
	_, headerMainURI, _ := p.playlists(t, 1, 1, p.header, header...)
	status, _, main := p.get(t, 0, headerMainURI+"&ApiKey="+p.token)
	uris := uriLines(main)
	if status != 200 || len(uris) != playlistSegments+1 {
		t.Fatalf("main with ApiKey: %d, %d URIs", status, len(uris))
	}
	for _, uri := range uris {
		if strings.Count(uri, "ApiKey="+p.token) != 1 {
			t.Errorf("main with ApiKey, after a master with a header, has a URI without it exactly once: %s", uri)
		}
	}
}

// What a query request gets is what it always got: every URI line has ApiKey=<token> once, spelled ApiKey whichever
// spelling the request used, and a header beside the same token in the query changes nothing.
func TestAQueryTokenPlaylistIsAsItWas(t *testing.T) {
	p := newPlaylistSite(t)
	queryMaster, _, queryMain := p.playlists(t, 0, 0, p.query)
	for _, uri := range append(uriLines(queryMaster), uriLines(queryMain)...) {
		if strings.Count(uri, "ApiKey="+p.token) != 1 || strings.Contains(strings.ToLower(uri), "api_key") {
			t.Errorf("a query request's line has no single ApiKey=<token>: %s", uri)
		}
	}
	for _, uri := range uriLines(queryMain) {
		if !strings.Contains(uri, "&ApiKey="+p.token+"&") {
			t.Errorf("ApiKey is not where it was, between PlaySessionId and TranscodingMaxAudioChannels: %s", uri)
		}
	}

	underscore := strings.Replace(p.query, "ApiKey=", "api_key=", 1)
	master, _, main := p.playlists(t, 1, 1, underscore)
	if master != queryMaster || main != queryMain {
		t.Errorf("a request with api_key got other playlists than one with ApiKey")
	}
	for _, spelling := range headerSpellings {
		master, _, main := p.playlists(t, 0, 1, p.query, spelling.set(p.token)...)
		if master != queryMaster || main != queryMain {
			t.Errorf("%s beside the same token in the query: the playlists are not the query request's", spelling.name)
		}
	}
}

// Fluster and every header client still get the URLs a URL-loading player needs from PlaybackInfo.
func TestPlaybackInfoWithAHeaderTokenStillCarriesApiKeyInItsUrls(t *testing.T) {
	p := newPlaylistSite(t)
	if !strings.Contains(p.query, "&ApiKey="+p.token+"&") {
		t.Errorf("TranscodingUrl: %s", p.query)
	}

	site := subtitleEnv(t, "")
	reply := site.playbackInfo(t, 0, site.text, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": db.CompactID(site.text)})
	var urls []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if s, ok := x.(string); ok && k == "DeliveryUrl" {
					urls = append(urls, s)
				}
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(reply)
	var subtitle, attachment bool
	for _, u := range urls {
		subtitle = subtitle || strings.Contains(u, "/Subtitles/")
		attachment = attachment || strings.Contains(u, "/Attachments/")
		if !strings.HasSuffix(u, "ApiKey="+site.token) {
			t.Errorf("a DeliveryUrl without ApiKey=<token>: %s", u)
		}
	}
	// The attachment half needs a film with a font in it, which this fixture's text film may not have.
	t.Logf("DeliveryUrls offered: %d, a subtitle: %v, an attachment: %v", len(urls), subtitle, attachment)
	if !subtitle {
		t.Errorf("PlaybackInfo offered %v: want a subtitle DeliveryUrl", urls)
	}
}

// The URIs a header-requested playlist lists are still behind the token: none, or a wrong one in a header, is a 401.
func TestTheUrisOfAHeaderPlaylistRefuseARequestWithoutTheToken(t *testing.T) {
	p := newPlaylistSite(t)
	header := headerSpellings[0].set(p.token)
	_, mainURI, main := p.playlists(t, 0, 0, p.header, header...)
	uris := uriLines(main)
	asked := []string{p.path + "master.m3u8?" + strings.SplitN(p.header, "?", 2)[1], mainURI, p.path + uris[0], p.path + uris[1], p.path + uris[len(uris)-1]}
	for _, uri := range asked {
		for replica := range 2 {
			for name, headers := range map[string][]string{"no token": nil, "a wrong token": headerSpellings[0].set("wrong-" + p.token), "a wrong X-Emby-Token": {"X-Emby-Token", "wrong"}} {
				status, _, body := p.get(t, replica, uri, headers...)
				if status != 401 || strings.TrimSpace(body) != "unauthorized" {
					t.Errorf("%s with %s on replica %d: %d %q", uri[:min(len(uri), 40)], name, replica, status, body)
				}
			}
		}
	}
}
