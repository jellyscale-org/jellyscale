// Command run is the differential tier (test/differential/run.sh starts the two
// servers): it sets Jellyfin up, signs in to both, sends each request in
// requests.yaml to both, and fails on a difference compat/differences.yaml does not list.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"jellyscale/test/differential"
)

const authHeader = `MediaBrowser Client="differential", Device="differential", DeviceId="differential", Version="1"`

// server is one of the two, signed in as admin.
type server struct {
	name, base, token, userID string
	ids                       map[string]string // item id -> "Type/Name"
	byKey                     map[string]string // "Type/Name" -> item id
}

func main() {
	jfURL := flag.String("jellyfin", "", "base URL of Jellyfin")
	jsURL := flag.String("jellyscale", "", "base URL of jellyscale")
	jfPass := flag.String("jellyfin-password", "", "password of the admin user to create in Jellyfin")
	jsPass := flag.String("jellyscale-password", "", "password of jellyscale's admin")
	requestsFile := flag.String("requests", "test/differential/requests.yaml", "")
	allowFile := flag.String("allowlist", "compat/differences.yaml", "")
	flag.Parse()

	reqs, err := differential.LoadRequests(*requestsFile)
	check(err)
	allow, err := differential.LoadAllowlist(*allowFile)
	check(err)
	check(allow.CheckRequests(reqs))

	jf := &server{name: "jellyfin", base: *jfURL}
	js := &server{name: "jellyscale", base: *jsURL}
	check(jf.setUp(*jfPass))
	check(js.signIn("admin", *jsPass))
	check(jf.waitForScan())
	check(js.waitForScan())
	check(jf.learnItems())
	check(js.learnItems())
	check(startWatching(jf, js))

	// A failure is reported under its route, once, with the requests that differ.
	type failure struct {
		name  string
		diffs []differential.Difference
		err   error
	}
	failures := map[string][]failure{}
	var routes []string
	for _, r := range reqs {
		diffs, err := compare(jf, js, r)
		if err == nil {
			diffs = allow.Filter(r.Name, diffs)
		}
		if err == nil && len(diffs) == 0 {
			fmt.Printf("ok    %s\n", r.Name)
			continue
		}
		if failures[r.Route] == nil {
			routes = append(routes, r.Route)
		}
		failures[r.Route] = append(failures[r.Route], failure{r.Name, diffs, err})
	}
	failed := 0
	for _, route := range routes {
		fmt.Printf("FAIL  %s\n", route)
		for _, f := range failures[route] {
			failed++
			fmt.Printf("      request: %s\n", f.name)
			if f.err != nil {
				fmt.Printf("      %v\n", f.err)
			}
			for _, d := range f.diffs {
				fmt.Printf("      %s: jellyfin %s, jellyscale %s\n", d.Path, show(d.Jellyfin), show(d.Jellyscale))
			}
		}
	}
	ran := len(reqs)
	for _, e := range allow.Unused() {
		fmt.Printf("note  compat/differences.yaml lists %s for the request %q, which no longer differs; remove it\n", e.Path, e.Requests[0])
	}
	if failed > 0 {
		fmt.Printf("FAIL  %d of %d requests differ from Jellyfin\n", failed, ran)
	}
	fmt.Printf("shard 0/1: ran %d of %d\n", ran, len(reqs))
	if failed > 0 {
		os.Exit(1)
	}
}

func check(err error) {
	if err != nil {
		fmt.Println("FAIL ", err)
		os.Exit(1)
	}
}

func show(v any) string {
	if v == nil {
		return "(none)"
	}
	b, _ := json.Marshal(v)
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

// do sends a request and returns the status and body. A token of "" sends none.
func (s *server) do(method, path, body string) (int, []byte, error) {
	req, err := http.NewRequest(method, s.base+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	auth := authHeader
	if s.token != "" {
		auth = `MediaBrowser Token="` + s.token + `", ` + strings.TrimPrefix(authHeader, "MediaBrowser ")
	}
	req.Header.Set("Authorization", auth)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (s *server) mustDo(method, path, body string) ([]byte, error) {
	status, data, err := s.do(method, path, body)
	if err == nil && status >= 300 {
		err = fmt.Errorf("status %d: %s", status, data)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s %s: %w", s.name, method, path, err)
	}
	return data, nil
}

func (s *server) getJSON(path string) (any, error) {
	data, err := s.mustDo("GET", path, "")
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(data, &v)
}

// setUp completes Jellyfin's startup wizard, adds the libraries over the
// fixtures mounted at /media and signs in.
func (s *server) setUp(password string) error {
	for i := 0; ; i++ {
		if status, _, err := s.do("GET", "/System/Info/Public", ""); err == nil && status == 200 {
			break
		}
		if i > 120 {
			return fmt.Errorf("jellyfin did not start")
		}
		time.Sleep(time.Second)
	}
	user, _ := json.Marshal(map[string]string{"Name": "admin", "Password": password})
	for _, step := range [][3]string{
		{"POST", "/Startup/Configuration", `{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}`},
		{"GET", "/Startup/User", ""},
		{"POST", "/Startup/User", string(user)},
		{"POST", "/Startup/RemoteAccess", `{"EnableRemoteAccess":true}`},
		{"POST", "/Startup/Complete", ""},
	} {
		if _, err := s.mustDo(step[0], step[1], step[2]); err != nil {
			return err
		}
	}
	if err := s.signIn("admin", password); err != nil {
		return err
	}
	for _, lib := range [][2]string{{"Movies", "movies"}, {"Shows", "tvshows"}} {
		path := "/Library/VirtualFolders?refreshLibrary=false&name=" + lib[0] + "&collectionType=" + lib[1]
		body := `{"LibraryOptions":{"PathInfos":[{"Path":"/media/` + strings.ToLower(lib[0]) + `"}]}}`
		if _, err := s.mustDo("POST", path, body); err != nil {
			return err
		}
	}
	// One scan for both libraries. With refreshLibrary=true on each, a second
	// refresh while the first scan ran sometimes cancelled it, leaving 3 items.
	_, err := s.mustDo("POST", "/Library/Refresh", "")
	return err
}

func (s *server) signIn(user, password string) error {
	body, _ := json.Marshal(map[string]string{"Username": user, "Pw": password})
	var data []byte
	var err error
	for i := 0; i < 60; i++ { // jellyscale may still be starting
		if data, err = s.mustDo("POST", "/Users/AuthenticateByName", string(body)); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	var auth struct {
		AccessToken string
		User        struct{ Id string }
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return err
	}
	s.token, s.userID = auth.AccessToken, auth.User.Id
	return nil
}

// waitForScan waits until the library stops growing and, on Jellyfin, until no
// scheduled task is running. The fixtures hold three films, a show with two seasons and three episodes.
func (s *server) waitForScan() error {
	last, stable := -1, 0
	for i := 0; i < 180; i++ {
		var list struct{ TotalRecordCount int }
		data, err := s.mustDo("GET", "/Items?Recursive=true&IncludeItemTypes=Movie,Series,Season,Episode", "")
		if err == nil && json.Unmarshal(data, &list) == nil {
			if list.TotalRecordCount == last && last >= 3+1+2+3 && !s.settling() {
				if stable++; stable >= 3 {
					return nil
				}
			} else {
				stable = 0
			}
			last = list.TotalRecordCount
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%s: the scan did not finish; %d items", s.name, last)
}

// settling says whether Jellyfin has a scheduled task running (the scan, then
// the metadata refresh). jellyscale has none to ask.
func (s *server) settling() bool {
	if s.name != "jellyfin" {
		return false
	}
	var tasks []struct{ State string }
	data, err := s.mustDo("GET", "/ScheduledTasks", "")
	if err != nil || json.Unmarshal(data, &tasks) != nil {
		return true
	}
	for _, t := range tasks {
		if t.State != "Idle" {
			return true
		}
	}
	return false
}

// learnItems records each item's id and its key. The servers name an item
// differently (Jellyfin keeps "(2021)" in a film's name), so a film or episode is
// keyed by the type and its file's name, which the fixtures fix. The rest (shows,
// seasons, views) have the same name on both and are keyed by it, as
// differential.ItemKeys does.
func (s *server) learnItems() error {
	s.ids, s.byKey = map[string]string{}, map[string]string{}
	for _, path := range []string{"/Items?Recursive=true&Fields=Path&IncludeItemTypes=Movie,Series,Season,Episode", "/UserViews"} {
		v, err := s.getJSON(path)
		if err != nil {
			return err
		}
		for id, key := range differential.ItemKeys(v) {
			s.ids[id] = key
			s.byKey[key] = id
		}
		items, _ := v.(map[string]any)["Items"].([]any)
		for _, item := range items {
			item, _ := item.(map[string]any)
			id, _ := item["Id"].(string)
			typ, _ := item["Type"].(string)
			if path, _ := item["Path"].(string); path != "" && id != "" && (typ == "Movie" || typ == "Episode") {
				key := typ + "/" + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				s.ids[id] = key
				s.byKey[key] = id
			}
		}
	}
	s.ids[s.userID] = "User/admin"
	return nil
}

// startWatching leaves a film half watched on both servers, so that
// the resume lists have a row for a wrong filter to show.
func startWatching(servers ...*server) error {
	for _, s := range servers {
		id, ok := s.byKey["Movie/Alpha Film (2021)"]
		if !ok {
			return fmt.Errorf("%s has no Movie/Alpha Film; it has %v", s.name, s.byKey)
		}
		if _, err := s.mustDo("POST", "/UserItems/"+id+"/UserData", `{"PlaybackPositionTicks":100000000}`); err != nil {
			return err
		}
	}
	return nil
}

var refPattern = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// fill puts this server's ids where the request names items.
func (s *server) fill(text string) (string, error) {
	var missing string
	out := refPattern.ReplaceAllStringFunc(text, func(ref string) string {
		key := ref[2 : len(ref)-2]
		if key == "User/admin" {
			return s.userID
		}
		id, ok := s.byKey[key]
		if !ok {
			missing = key
		}
		return id
	})
	if missing != "" {
		return "", fmt.Errorf("%s has no item %q", s.name, missing)
	}
	return out, nil
}

// answer is what a server said, as a document: JSON as it is, a playlist as its lines.
func (s *server) answer(r differential.Request) (status int, doc any, err error) {
	path, err := s.fill(r.Path)
	if err != nil {
		return 0, nil, err
	}
	body, err := s.fill(r.Body)
	if err != nil {
		return 0, nil, err
	}
	status, data, err := s.do(r.Method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if json.Unmarshal(data, &doc) != nil {
		lines := []any{}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			lines = append(lines, line)
		}
		doc = lines
	}
	return status, doc, nil
}

func compare(jf, js *server, r differential.Request) ([]differential.Difference, error) {
	jfStatus, jfDoc, err := jf.answer(r)
	if err != nil {
		return nil, err
	}
	jsStatus, jsDoc, err := js.answer(r)
	if err != nil {
		return nil, err
	}
	var diffs []differential.Difference
	if jfStatus != jsStatus {
		diffs = append(diffs, differential.Difference{Path: "(status)", Jellyfin: jfStatus, Jellyscale: jsStatus})
	}
	// What the servers said about items elsewhere in the document is mapped by
	// everything listed up front, not just by this answer.
	a := differential.Normalize(jfDoc, merge(jf.ids, differential.ItemKeys(jfDoc)), r.Ordered)
	b := differential.Normalize(jsDoc, merge(js.ids, differential.ItemKeys(jsDoc)), r.Ordered)
	diffs = append(diffs, differential.Diff(a, b)...)
	return append(diffs, differential.IDDifferences(a, b, r.Ordered)...), nil
}

func merge(known, more map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range []map[string]string{more, known} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
