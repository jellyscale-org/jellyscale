//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// JS-124: jellyscale itself, started over the films test/fixtures/client.sh
// makes, says what docs/client-testing.md says: the same items, the same tracks
// at the same indexes, and the same behaviour a client's CI relies on. test/image.sh
// asks the same of the image; this runs the same questions against the build
// in the checkout with the ffmpeg CI installs, so a recipe or a server change
// that breaks the page fails in the integration tier.

type expectedTrack struct {
	index                  int
	kind, lang, codec      string
	isDefault, forced, ext bool
}

type clientExpect struct {
	libraries [][3]string // name, kind, folder
	films     []string
	episodes  []string
	tracks    map[string][]expectedTrack
	plays     map[string]string
}

func readClientExpect(t *testing.T, repo string) clientExpect {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "test", "image-expect.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	e := clientExpect{tracks: map[string][]expectedTrack{}, plays: map[string]string{}}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(line, "\t")
		switch f[0] {
		case "library":
			e.libraries = append(e.libraries, [3]string{f[1], f[2], f[3]})
		case "film":
			e.films = append(e.films, f[1])
		case "episode":
			e.episodes = append(e.episodes, f[1])
		case "plays":
			e.plays[f[1]] = f[2]
		case "track":
			var i int
			fmt.Sscan(f[2], &i)
			fl := f[6]
			e.tracks[f[1]] = append(e.tracks[f[1]], expectedTrack{i, f[3], f[4], f[5],
				strings.Contains(fl, "default"), strings.Contains(fl, "forced"), strings.Contains(fl, "external")})
		}
	}
	return e
}

type clientAPI struct {
	t     *testing.T
	p     *proc
	token string
	user  string
}

func (c *clientAPI) do(method, target string, body []byte, headers ...string) (int, http.Header, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.p.url(target), rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="client-films", Version="1", Token="%s"`, c.token))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

func (c *clientAPI) items(kind string) map[string][]string {
	c.t.Helper()
	status, _, raw := c.do("GET", "/Items?Recursive=true&IncludeItemTypes="+kind+"&Limit=500&Fields=Path", nil)
	if status != 200 {
		c.t.Fatalf("GET /Items: %d %s", status, raw)
	}
	var reply struct{ Items []struct{ Id, Name string } }
	if err := json.Unmarshal(raw, &reply); err != nil {
		c.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, it := range reply.Items {
		out[it.Name] = append(out[it.Name], it.Id)
	}
	return out
}

func (c *clientAPI) playbackInfo(id string, body []byte) map[string]any {
	c.t.Helper()
	status, _, raw := c.do("POST", "/Items/"+id+"/PlaybackInfo?UserId="+c.user+"&StartTimeTicks=0&IsPlayback=false&AutoOpenLiveStream=false", body)
	if status != 200 {
		c.t.Fatalf("PlaybackInfo of %s: %d %s", id, status, raw)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		c.t.Fatal(err)
	}
	sources, _ := reply["MediaSources"].([]any)
	if len(sources) != 1 {
		c.t.Fatalf("PlaybackInfo of %s lists %d sources: %s", id, len(sources), raw)
	}
	return sources[0].(map[string]any)
}

// The request the page gives is the first json block of docs/client-testing.md.
func pageRequest(t *testing.T, repo string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repo, "docs", "client-testing.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```json\n(.*?)\n```").FindSubmatch(raw)
	if m == nil || !bytes.Contains(m[1], []byte("DeviceProfile")) {
		t.Fatal("docs/client-testing.md has no json block holding the PlaybackInfo request's DeviceProfile")
	}
	return m[1]
}

func TestJellyscaleOverTheClientFilmsAnswersAsThePageSays(t *testing.T) {
	testenv.FFmpeg(t)
	testenv.Font(t)
	repo, _ := filepath.Abs(filepath.Join("..", ".."))
	films := t.TempDir()
	if out, err := exec.Command("bash", filepath.Join(repo, "test", "fixtures", "client.sh"), films).CombinedOutput(); err != nil {
		t.Fatalf("test/fixtures/client.sh: %v\n%s", err, out)
	}
	expect := readClientExpect(t, repo)
	request := pageRequest(t, repo)

	c := newLibCluster(t)
	segments, assets := t.TempDir(), t.TempDir()
	var libs []map[string]any
	for _, l := range expect.libraries {
		libs = append(libs, map[string]any{"name": l[0], "kind": l[1], "media": "file://" + filepath.Join(films, l[2]), "assets": "file://" + filepath.Join(assets, l[2])})
	}
	rawConfig, _ := json.Marshal(map[string]any{"adminUser": "admin", "libraries": libs, "transcodeStore": "file://" + segments})
	p := c.launch("all", string(rawConfig))
	p.ready()
	api := &clientAPI{t: t, p: p}
	status, raw := p.authenticate(t, "client-films", "admin", "pw-js102")
	var auth struct {
		AccessToken string
		User        struct{ Id string }
	}
	if status != 200 || json.Unmarshal(raw, &auth) != nil {
		t.Fatalf("sign-in: %d %s", status, raw)
	}
	api.token, api.user = auth.AccessToken, auth.User.Id

	// The scan is done when the counts are the documented ones; no bound but a hang's.
	var movies, episodes map[string][]string
	deadline := time.Now().Add(5 * time.Minute)
	for {
		movies, episodes = api.items("Movie"), api.items("Episode")
		if len(movies) == len(expect.films) && len(episodes) == len(expect.episodes) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 5 minutes: %d film names (want %d) and %d episode names (want %d)", len(movies), len(expect.films), len(episodes), len(expect.episodes))
		}
		time.Sleep(time.Second)
	}
	for _, name := range expect.films {
		if len(movies[name]) != 1 {
			t.Errorf("film %q: %d items, want exactly one", name, len(movies[name]))
		}
	}
	for _, name := range expect.episodes {
		if len(episodes[name]) != 1 {
			t.Errorf("episode %q: %d items, want exactly one", name, len(episodes[name]))
		}
	}

	sources := map[string]map[string]any{}
	for _, name := range expect.films {
		if len(movies[name]) != 1 {
			continue
		}
		src := api.playbackInfo(movies[name][0], request)
		sources[name] = src
		streams, _ := src["MediaStreams"].([]any)
		want := expect.tracks[name]
		if len(streams) != len(want) {
			t.Errorf("%s: PlaybackInfo lists %d streams, want %d", name, len(streams), len(want))
			continue
		}
		for i, w := range want {
			s := streams[i].(map[string]any)
			lang := fmt.Sprint(s["Language"])
			if s["Language"] == nil || lang == "" {
				lang = "none"
			}
			got := expectedTrack{int(s["Index"].(float64)), fmt.Sprint(s["Type"]), lang, fmt.Sprint(s["Codec"]), s["IsDefault"] == true, s["IsForced"] == true, s["IsExternal"] == true}
			// Only audio's default is documented; video and subtitles are listed without it.
			if w.kind != "Audio" {
				got.isDefault, w.isDefault = false, false
			}
			if got != w {
				t.Errorf("%s stream %d: PlaybackInfo has %+v, the page %+v", name, i, got, w)
			}
		}
	}

	// A byte range of a film the page says plays as it is.
	for _, name := range expect.films {
		if expect.plays[name] != "direct" || sources[name] == nil {
			continue
		}
		id := movies[name][0]
		src := sources[name]
		target := fmt.Sprintf("/Videos/%s/stream?static=true&mediaSourceId=%v&api_key=%s", id, src["Id"], api.token)
		status, header, body := api.do("GET", target, nil, "Range", "bytes=1000-1999")
		if status != 206 || len(body) != 1000 || !strings.HasPrefix(header.Get("Content-Range"), "bytes 1000-1999/") {
			t.Errorf("%s: Range bytes=1000-1999 gave %d, Content-Range %q, %d bytes; want 206, bytes 1000-1999/N and 1000 bytes", name, status, header.Get("Content-Range"), len(body))
		}
		break
	}

	// Babel Film's text tracks, each as Stream.vtt: the first cue is the track's own.
	babel, ok := sources["Babel Film"]
	if !ok {
		t.Fatal("no Babel Film")
	}
	first := map[int]string{}
	for _, tr := range expect.tracks["Babel Film"] {
		if tr.kind != "Subtitle" || tr.codec == "PGSSUB" {
			continue
		}
		target := fmt.Sprintf("/Videos/%s/%v/Subtitles/%d/Stream.vtt?api_key=%s", movies["Babel Film"][0], babel["Id"], tr.index, api.token)
		var body []byte
		var status int
		for until := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
			status, _, body = api.do("GET", target, nil)
			if status == 200 || time.Now().After(until) {
				break
			}
		}
		if status != 200 || !strings.HasPrefix(string(body), "WEBVTT") {
			t.Errorf("Babel Film track %d as Stream.vtt: %d %.60q", tr.index, status, body)
			continue
		}
		cue := regexp.MustCompile(`(?s)-->[^\n]*\n(.+?)(\n\n|\n*$)`).FindSubmatch(body)
		if cue == nil {
			t.Errorf("Babel Film track %d has no cue: %q", tr.index, body)
			continue
		}
		first[tr.index] = strings.TrimSpace(string(cue[1]))
	}
	seen := map[string]int{}
	for i, text := range first {
		if j, dup := seen[text]; dup {
			t.Errorf("Babel Film tracks %d and %d both begin with the cue %q: each track must name itself", j, i, text)
		}
		seen[text] = i
	}
	if first[5] == "" {
		t.Error("Babel Film track 5 has no first cue")
	}

	// A transcode of an AC3 film: the first segment holds H.264 and AAC, no AC3.
	for _, name := range expect.films {
		if expect.plays[name] != "transcode" || sources[name] == nil {
			continue
		}
		tu, _ := sources[name]["TranscodingUrl"].(string)
		if tu == "" {
			t.Errorf("%s: no TranscodingUrl for a profile that does not play AC3", name)
			break
		}
		segment := firstSegment(t, api, tu)
		dir := t.TempDir()
		file := filepath.Join(dir, "segment.mp4")
		if err := os.WriteFile(file, segment, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,codec_type", "-of", "csv=p=0", file).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe of %s's first segment: %v\n%s", name, err, out)
		}
		got := string(out)
		if !strings.Contains(got, "h264") || !strings.Contains(got, "aac") || strings.Contains(got, "ac3") {
			t.Errorf("%s's first segment holds %q, want h264 and aac and no ac3", name, got)
		}
		break
	}
}

// firstSegment follows a master playlist to its first variant's first segment (with its init part, when it has
// one, in front) and waits for the worker to have made it.
func firstSegment(t *testing.T, api *clientAPI, master string) []byte {
	t.Helper()
	parse := func(ref string) *url.URL {
		u, err := url.Parse(ref)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	fetch := func(u *url.URL) []byte {
		target := u.RequestURI()
		if u.Query().Get("api_key") == "" {
			sep := "?"
			if u.RawQuery != "" {
				sep = "&"
			}
			target += sep + "api_key=" + api.token
		}
		for until := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
			status, _, body := api.do("GET", target, nil)
			if status == 200 {
				return body
			}
			if time.Now().After(until) {
				t.Fatalf("GET %s: %d %.200s", u, status, body)
			}
		}
	}
	lines := func(b []byte) []string {
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	masterURL := parse(master)
	var variant string
	for _, l := range lines(fetch(masterURL)) {
		if !strings.HasPrefix(l, "#") {
			variant = l
			break
		}
	}
	if variant == "" {
		t.Fatalf("the master playlist %s names no variant", master)
	}
	variantURL := masterURL.ResolveReference(parse(variant))
	var init, seg string
	for _, l := range lines(fetch(variantURL)) {
		if m := regexp.MustCompile(`#EXT-X-MAP:URI="([^"]+)"`).FindStringSubmatch(l); m != nil {
			init = m[1]
		} else if !strings.HasPrefix(l, "#") && seg == "" {
			seg = l
		}
	}
	if seg == "" {
		t.Fatalf("the playlist %s lists no segment", variantURL)
	}
	var out []byte
	if init != "" {
		out = append(out, fetch(variantURL.ResolveReference(parse(init)))...)
	}
	return append(out, fetch(variantURL.ResolveReference(parse(seg)))...)
}
