//go:build integration

package api

// JS-076: what a viewer sees while a file is missing is what they would see if
// it had been deleted, though its item and watch state are kept. The names the
// code lacks: Scanner.Grace and Scanner.MaxLostShare.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/testenv"
)

type missingEnv struct {
	*env
	moviesDir, showsDir string
	movies, shows       library.Library
	scanner             *library.Scanner
	series, season2     string // Compact ids of Quartz Show and its second season.
}

func missingClip(t *testing.T, file string, seconds int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf("testsrc2=size=160x90:rate=10:duration=%d", seconds), "-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// newMissingEnv scans a film library and a show library, and gives the viewer a
// position in every film and episode, so that Resume would list them.
func newMissingEnv(t *testing.T) *missingEnv {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "TRUNCATE libraries, users CASCADE"); err != nil {
		t.Fatal(err)
	}
	m := &missingEnv{env: &env{pool: pool}, moviesDir: t.TempDir(), showsDir: t.TempDir()}
	missingClip(t, filepath.Join(m.moviesDir, "Quartz Alpha (2020).mkv"), 1)
	missingClip(t, filepath.Join(m.moviesDir, "Quartz Beta (2021).mkv"), 2)
	missingClip(t, filepath.Join(m.showsDir, "Quartz Show", "Season 01", "Quartz Show S01E01 Opener.mkv"), 1)
	missingClip(t, filepath.Join(m.showsDir, "Quartz Show", "Season 01", "Quartz Show S01E02 Middle.mkv"), 2)
	missingClip(t, filepath.Join(m.showsDir, "Quartz Show", "Season 02", "Quartz Show S02E01 Return.mkv"), 3)
	missingClip(t, filepath.Join(m.showsDir, "Quartz Gone", "Season 01", "Quartz Gone S01E01 Only.mkv"), 4)
	missingClip(t, filepath.Join(m.showsDir, "Plain", "Season 01", "Plain S01E01.mkv"), 5)
	m.sync(t, true, true)
	m.scanner = &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler), Grace: time.Hour, MaxLostShare: 1}
	for _, lib := range []library.Library{m.movies, m.shows} {
		if err := m.scanner.Scan(ctx, lib); err != nil {
			t.Fatal(err)
		}
	}
	m.series = m.id(t, "series:Quartz Show")
	m.season2 = m.id(t, "season:"+db.DashedID(m.series)+":2")
	userID, token := m.addUser(t, "viewer")
	m.user, m.token = userID, token
	other, _ := m.addUser(t, "other")
	for _, u := range []string{userID, other} {
		mustExec(t, pool, `INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks, last_played_at)
			SELECT $1, id, false, type = 'Series', 1, 5000, now() FROM items`, u)
	}
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleAPI, RoleStream}, Log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	m.servers = append(m.servers, server)
	return m
}

func (m *missingEnv) sync(t *testing.T, movies, shows bool) {
	t.Helper()
	var configs []library.Config
	if movies {
		configs = append(configs, library.Config{Name: "Movies", Kind: "movies", Media: "file://" + m.moviesDir, Assets: "file://" + m.moviesDir})
	}
	if shows {
		configs = append(configs, library.Config{Name: "Shows", Kind: "tvshows", Media: "file://" + m.showsDir, Assets: "file://" + m.showsDir})
	}
	libs, err := library.Sync(context.Background(), m.pool, configs)
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range libs {
		if lib.Name == "Movies" {
			m.movies = lib
		} else {
			m.shows = lib
		}
	}
}

func (m *missingEnv) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	response, body := m.do(t, 0, "GET", path, m.token, nil)
	return response.StatusCode, body
}

// listedNames is the Name of each item in a reply: an Items or SearchHints list, or a bare list.
func listedNames(t *testing.T, body []byte) ([]string, int) {
	t.Helper()
	var list []any
	count := -1
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err == nil {
		for _, key := range []string{"Items", "SearchHints"} {
			if l, ok := doc[key].([]any); ok {
				list = l
			}
		}
		if n, ok := doc["TotalRecordCount"].(float64); ok {
			count = int(n)
		}
	} else if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	var out []string
	for _, entry := range list {
		out = append(out, entry.(map[string]any)["Name"].(string))
	}
	slices.Sort(out)
	return out, count
}

func (m *missingEnv) id(t *testing.T, key string) string {
	t.Helper()
	return db.CompactID(scalar[string](t, m.pool, "SELECT id::text FROM items WHERE natural_key = $1", key))
}

func (m *missingEnv) lists(t *testing.T) map[string][]string {
	t.Helper()
	series := m.series
	all := map[string][]string{}
	for name, path := range map[string]string{
		"items":       "/Items?Recursive=true&IncludeItemTypes=Movie,Series,Season,Episode",
		"search":      "/Items?Recursive=true&searchTerm=quartz",
		"hints":       "/Search/Hints?searchTerm=quartz&userId=" + db.CompactID(m.user),
		"latest":      "/Items/Latest?Limit=50",
		"latestFilms": "/Items/Latest?Limit=50&ParentId=" + db.CompactID(m.movies.ID),
		"suggestions": "/Items/Suggestions?type=Movie,Series,Episode&limit=50",
		"resume":      "/UserItems/Resume",
		"seasons":     "/Shows/" + series + "/Seasons",
		"episodes":    "/Shows/" + series + "/Episodes",
		"episodes2":   "/Shows/" + series + "/Episodes?seasonId=" + m.season2,
	} {
		status, body := m.get(t, path)
		if status != 200 {
			t.Fatalf("%s: %d %s", path, status, body)
		}
		got, count := listedNames(t, body)
		if count >= 0 && count != len(got) {
			t.Fatalf("%s: TotalRecordCount %d but %d items listed: %q", path, count, len(got), got)
		}
		all[name] = got
	}
	return all
}

// TestHiddenItemsAreInvisibleToViewers: a film, an episode that is a season's
// only one, and a series with a single episode are missing. Nothing lists them,
// counts them or answers for their ids, as if they had been deleted, though the
// items and the viewers' rows are kept.
func TestMissingItemsAreInvisibleToViewers(t *testing.T) {
	m := newMissingEnv(t)
	ctx := context.Background()
	film := m.id(t, "video:Quartz Alpha (2020).mkv")
	episode := m.id(t, "video:Quartz Show/Season 02/Quartz Show S02E01 Return.mkv")
	gone := m.id(t, "video:Quartz Gone/Season 01/Quartz Gone S01E01 Only.mkv")
	goneSeries := m.id(t, "series:Quartz Gone")
	rowsBefore := scalar[int](t, m.pool, "SELECT count(*) FROM user_item_data")

	before := m.lists(t)
	for name, want := range map[string]string{
		"items": "Quartz Alpha", "search": "Quartz Alpha", "hints": "Quartz Alpha", "latest": "Quartz Alpha", "latestFilms": "Quartz Alpha",
		"suggestions": "Quartz Alpha", "resume": "Quartz Alpha", "episodes": "Return", "episodes2": "Return",
	} {
		if !slices.Contains(before[name], want) {
			t.Fatalf("before the file goes, %s lists %q, not %q: the check cannot tell", name, before[name], want)
		}
	}
	if !slices.Contains(before["seasons"], "Season 2") {
		t.Fatalf("seasons %q", before["seasons"])
	}
	status, _ := m.get(t, "/Items/"+film)
	if status != 200 {
		t.Fatalf("GET /Items/{film} is %d", status)
	}

	for _, file := range []string{
		filepath.Join(m.moviesDir, "Quartz Alpha (2020).mkv"),
		filepath.Join(m.showsDir, "Quartz Show", "Season 02", "Quartz Show S02E01 Return.mkv"),
		filepath.Join(m.showsDir, "Quartz Gone", "Season 01", "Quartz Gone S01E01 Only.mkv"),
	} {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	for _, lib := range []library.Library{m.movies, m.shows} {
		if err := m.scanner.Scan(ctx, lib); err != nil {
			t.Fatal(err)
		}
	}
	if n := scalar[int](t, m.pool, "SELECT count(*) FROM user_item_data"); n != rowsBefore {
		t.Fatalf("%d rows of watch state, was %d", n, rowsBefore)
	}

	after := m.lists(t)
	for name, list := range after {
		for _, hidden := range []string{"Quartz Alpha", "Return", "Only", "Quartz Gone", "Season 2"} {
			if slices.Contains(list, hidden) {
				t.Errorf("%s still lists %q: %q", name, hidden, list)
			}
		}
	}
	// What is left is listed, and counted without the hidden.
	if want := len(before["items"]) - 6; len(after["items"]) != want {
		t.Errorf("/Items lists %d, want %d (the film, the episode, its season, the series, its season and episode are gone)", len(after["items"]), want)
	}
	if !slices.Contains(after["items"], "Quartz Beta") || !slices.Contains(after["items"], "Quartz Show") {
		t.Errorf("/Items lost what is still there: %q", after["items"])
	}
	if !slices.Equal(after["seasons"], []string{"Season 1"}) {
		t.Errorf("the show's seasons are %q, want Season 1 only", after["seasons"])
	}
	if slices.Contains(after["episodes"], "Return") || !slices.Contains(after["episodes"], "Opener") {
		t.Errorf("the show's episodes are %q", after["episodes"])
	}

	// GET /Items/{id} answers as for an id that was never there.
	unknownStatus, unknownBody := m.get(t, "/Items/"+db.CompactID(uuid.NewString()))
	if unknownStatus != 404 {
		t.Fatalf("an unknown id is %d", unknownStatus)
	}
	for name, id := range map[string]string{"film": film, "episode": episode, "season": m.season2, "series with every episode missing": goneSeries, "its episode": gone} {
		status, body := m.get(t, "/Items/"+id)
		if status != unknownStatus || string(body) != string(unknownBody) {
			t.Errorf("GET /Items/{%s} is %d %q; an unknown id is %d %q", name, status, body, unknownStatus, unknownBody)
		}
	}
}

// A library left out of config disappears from /UserViews and GET /Items, with
// its items, and returns inside the grace.
func TestLibraryLeftOutOfConfigIsNotShownToViewers(t *testing.T) {
	m := newMissingEnv(t)
	film := m.id(t, "video:Quartz Alpha (2020).mkv")
	lib := db.CompactID(m.movies.ID)
	status, body := m.get(t, "/UserViews")
	if got, _ := listedNames(t, body); status != 200 || !slices.Equal(got, []string{"Movies", "Shows"}) {
		t.Fatalf("/UserViews before: %d %q", status, got)
	}
	m.sync(t, false, true)
	status, body = m.get(t, "/UserViews")
	if got, count := listedNames(t, body); status != 200 || !slices.Equal(got, []string{"Shows"}) || count != 1 {
		t.Fatalf("/UserViews after the library is left out: %d %q (%d)", status, got, count)
	}
	unknownStatus, unknownBody := m.get(t, "/Items/"+db.CompactID(uuid.NewString()))
	for name, id := range map[string]string{"library": lib, "its film": film} {
		status, body := m.get(t, "/Items/"+id)
		if status != unknownStatus || string(body) != string(unknownBody) {
			t.Errorf("GET /Items/{%s} is %d; an unknown id is %d", name, status, unknownStatus)
		}
	}
	_, body = m.get(t, "/Items?Recursive=true&searchTerm=quartz+alpha")
	if got, _ := listedNames(t, body); len(got) != 0 {
		t.Errorf("search finds %q", got)
	}
	_, body = m.get(t, "/Items?ParentId="+lib)
	if got, _ := listedNames(t, body); len(got) != 0 {
		t.Errorf("the library's children are %q", got)
	}
	m.sync(t, true, true)
	if status, _ := m.get(t, "/Items/"+film); status != 200 {
		t.Fatalf("the film after the library is back: %d", status)
	}
	if status, _ := m.get(t, "/Items/"+lib); status != 200 {
		t.Fatalf("the library after it is back: %d", status)
	}
}

// byIDRequests is every by-id route a client holding an item id can reach:
// the ones a listing filter does not guard. item is the id, source its media
// source's.
func byIDRequests(item, source string) []byIDRequest {
	dashed := db.DashedID(item)
	play := map[string]any{"ItemId": item, "MediaSourceId": source, "PlayMethod": "DirectPlay", "PositionTicks": 10, "PlaySessionId": "ps"}
	return []byIDRequest{
		{name: "Ancestors", method: "GET", path: "/Items/" + item + "/Ancestors", mustDiffer: true},
		{name: "PlaybackInfo GET", method: "GET", path: "/Items/" + item + "/PlaybackInfo?MediaSourceId=" + source, mustDiffer: true},
		{name: "PlaybackInfo POST", method: "POST", path: "/Items/" + item + "/PlaybackInfo", body: map[string]any{"MediaSourceId": source}, mustDiffer: true},
		{name: "Image", method: "GET", path: "/Items/" + item + "/Images/Primary", mustDiffer: true},
		{name: "Download", method: "GET", path: "/Items/" + item + "/Download", mustDiffer: true},
		{name: "master.m3u8", method: "GET", path: "/Videos/" + dashed + "/master.m3u8?MediaSourceId=" + source},
		{name: "main.m3u8", method: "GET", path: "/Videos/" + dashed + "/main.m3u8?MediaSourceId=" + source},
		{name: "stream", method: "GET", path: "/Videos/" + dashed + "/stream?static=true&MediaSourceId=" + source, mustDiffer: true},
		{name: "subtitle", method: "GET", path: "/Videos/" + dashed + "/" + source + "/Subtitles/0/Stream.srt"},
		{name: "attachment", method: "GET", path: "/Videos/" + dashed + "/" + source + "/Attachments/0"},
		{name: "favourite", method: "POST", path: "/UserFavoriteItems/" + item, mustDiffer: true},
		{name: "unfavourite", method: "DELETE", path: "/UserFavoriteItems/" + item},
		{name: "legacy favourite", method: "POST", path: "/Users/{user}/FavoriteItems/" + item},
		{name: "played", method: "POST", path: "/UserPlayedItems/" + item, mustDiffer: true},
		{name: "unplayed", method: "DELETE", path: "/UserPlayedItems/" + item},
		{name: "UserData", method: "POST", path: "/UserItems/" + item + "/UserData", body: map[string]any{"IsFavorite": true, "PlaybackPositionTicks": 7}},
		{name: "Sessions/Playing", method: "POST", path: "/Sessions/Playing", body: play},
		{name: "Sessions/Playing/Progress", method: "POST", path: "/Sessions/Playing/Progress", body: play},
		{name: "Sessions/Playing/Stopped", method: "POST", path: "/Sessions/Playing/Stopped", body: play},
	}
}

type byIDRequest struct {
	name, method, path string
	body               any
	mustDiffer         bool // A visible item must be answered differently from an unknown id, or the check cannot tell.
}

// answer is the status and body of a request, with the item's id (in either
// spelling) replaced, so that an unknown id and a hidden one compare.
func (m *missingEnv) answer(t *testing.T, r byIDRequest, id string) string {
	t.Helper()
	path := strings.ReplaceAll(r.path, "{user}", db.CompactID(m.user))
	response, body := m.do(t, 0, r.method, path, m.token, r.body)
	text := string(body)
	for _, form := range []string{id, db.DashedID(id)} {
		text = strings.ReplaceAll(text, form, "ID")
	}
	return fmt.Sprintf("%d %s", response.StatusCode, text)
}

func (m *missingEnv) rowSnapshot(t *testing.T) []string {
	t.Helper()
	rows, err := m.pool.Query(context.Background(), `SELECT user_id::text || ' ' || item_id::text || ' ' || played::text || ' ' || favorite::text || ' ' ||
		play_count::text || ' ' || position_ticks::text || ' ' || coalesce(last_played_at::text, '') FROM user_item_data ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// Hiding is not only a filter on the listings: a hidden item is as unknown to
// PlaybackInfo, an image, Ancestors, a download, a stream and every playstate
// write as to GET /Items/{id}, whether its file is missing or its library left
// config (whose store is still there to stream from for the whole grace), and no
// request changes a row of watch state.
func TestHiddenItemIsUnknownToEveryByIDRoute(t *testing.T) {
	for _, why := range []string{"file missing", "library left out of config"} {
		t.Run(why, func(t *testing.T) {
			m := newMissingEnv(t)
			ctx := context.Background()
			alpha := m.id(t, "video:Quartz Alpha (2020).mkv")
			beta := m.id(t, "video:Quartz Beta (2021).mkv")
			// Both films have a poster, so an image is one answer for a film that is
			// shown and another for an unknown id.
			for _, name := range []string{"Quartz Alpha (2020)", "Quartz Beta (2021)"} {
				poster := name + "-poster.jpg"
				if err := os.WriteFile(filepath.Join(m.moviesDir, poster), []byte("jpeg"), 0o644); err != nil {
					t.Fatal(err)
				}
				mustExec(t, m.pool, "UPDATE items SET primary_image_key = $1 WHERE natural_key = $2", poster, "video:"+name+".mkv")
			}
			source := func(item string) string {
				return db.CompactID(scalar[string](t, m.pool, "SELECT id::text FROM media_sources WHERE item_id = $1::uuid", item))
			}
			unknown := db.CompactID(uuid.NewString())

			// Control: the film that stays is answered unlike an unknown id by the routes
			// marked, so a pass below means more than "every answer is an error".
			betaSource, src := source(beta), source(alpha)
			for i, r := range byIDRequests(beta, betaSource) {
				if !r.mustDiffer {
					continue
				}
				visible := m.answer(t, r, beta)
				if visible == m.answer(t, byIDRequests(unknown, betaSource)[i], unknown) {
					t.Fatalf("%s answers a film that is shown as it answers an unknown id (%.80q): the check cannot tell", r.name, visible)
				}
			}

			if why == "file missing" {
				if err := os.Remove(filepath.Join(m.moviesDir, "Quartz Alpha (2020).mkv")); err != nil {
					t.Fatal(err)
				}
				if err := m.scanner.Scan(ctx, m.movies); err != nil {
					t.Fatal(err)
				}
			} else {
				m.sync(t, false, true)
			}
			rows := m.rowSnapshot(t)
			for i, r := range byIDRequests(alpha, src) {
				want := m.answer(t, byIDRequests(unknown, src)[i], unknown)
				if got := m.answer(t, r, alpha); got != want {
					t.Errorf("%s on a hidden item answers %.200q; an unknown id answers %.200q", r.name, got, want)
				}
			}
			sameLinesAPI(t, "user_item_data after requests for a hidden item", m.rowSnapshot(t), rows)
			if scalar[int](t, m.pool, "SELECT count(*) FROM items WHERE id = $1::uuid", alpha) != 1 {
				t.Fatal("the hidden item was removed")
			}
		})
	}
}

func sameLinesAPI(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s changed:\n got %q\nwant %q", what, got, want)
	}
}
