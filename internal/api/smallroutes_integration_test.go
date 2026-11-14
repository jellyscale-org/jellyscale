//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
)

func TestDownloadFromEveryStore(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			e := newEnvOn(t, target)
			path := "/Items/" + db.CompactID(e.item) + "/Download"
			size := len(e.body)

			response, body := e.do(t, 0, "GET", path, e.token, nil)
			if response.StatusCode != 200 || !bytes.Equal(body, e.body) {
				t.Fatalf("whole file: %d, %d bytes", response.StatusCode, len(body))
			}
			if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="film.mkv"; filename*=UTF-8''film.mkv` {
				t.Errorf("Content-Disposition %q", got)
			}
			// The link jellyfin-web builds carries the token in the query.
			response, body = e.do(t, 1, "GET", path+"?ApiKey="+e.token, "", nil, "Range", fmt.Sprintf("bytes=%d-%d", size-100, size-1))
			if response.StatusCode != 206 || !bytes.Equal(body, e.body[size-100:]) ||
				response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", size-100, size-1, size) ||
				response.Header.Get("Content-Disposition") == "" {
				t.Fatalf("tail range: %d %q", response.StatusCode, response.Header)
			}
			if response, _ = e.do(t, 0, "GET", path, e.token, nil, "Range", fmt.Sprintf("bytes=%d-", size+5)); response.StatusCode != 416 {
				t.Errorf("unsatisfiable range: %d", response.StatusCode)
			}
			for name, c := range map[string]struct {
				path, token string
				want        int
			}{
				"no token":     {path, "", 401},
				"bad token":    {path, "nope", 401},
				"missing item": {"/Items/" + uuid.NewString() + "/Download", e.token, 404},
				"not an id":    {"/Items/zzz/Download", e.token, 400},
			} {
				if response, _ := e.do(t, 0, "GET", c.path, c.token, nil); response.StatusCode != c.want {
					t.Errorf("%s: %d, want %d", name, response.StatusCode, c.want)
				}
			}

			// Jellyfin sends the item's file when the source id matches none.
			if response, body := e.do(t, 0, "GET", path+"?mediaSourceId="+uuid.NewString(), e.token, nil); response.StatusCode != 200 || !bytes.Equal(body, e.body) {
				t.Errorf("unknown source: %d, %d bytes", response.StatusCode, len(body))
			}

			// A file gone from the store is a 404 with nothing to save.
			mustExec(t, e.pool, "UPDATE media_sources SET store_key = 'gone-' || store_key")
			response, _ = e.do(t, 0, "GET", path, e.token, nil)
			if response.StatusCode != 404 || response.Header.Get("Content-Disposition") != "" {
				t.Errorf("missing object: %d, Content-Disposition %q", response.StatusCode, response.Header.Get("Content-Disposition"))
			}
		})
	}
}

// Every route takes a valid token in every form compat.ParseAuth reads, a
// deliberate difference from Jellyfin 12.4, which refuses some forms on these.
func TestNewRoutesAcceptEveryTokenForm(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	forms := map[string]func(token string) (query, header, value string){
		"X-Emby-Token":         func(t string) (string, string, string) { return "", "X-Emby-Token", t },
		"X-MediaBrowser-Token": func(t string) (string, string, string) { return "", "X-MediaBrowser-Token", t },
		"api_key":              func(t string) (string, string, string) { return "?api_key=" + t, "", "" },
		"ApiKey":               func(t string) (string, string, string) { return "?ApiKey=" + t, "", "" },
		"Authorization":        func(t string) (string, string, string) { return "", "Authorization", `MediaBrowser Token="` + t + `"` },
		"X-Emby-Authorization": func(t string) (string, string, string) {
			return "", "X-Emby-Authorization", `MediaBrowser Token="` + t + `"`
		},
		"Emby scheme": func(t string) (string, string, string) { return "", "Authorization", `Emby Token="` + t + `"` },
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/Items/" + id + "/Download"},
		{"POST", "/UserItems/" + id + "/UserData"},
		{"GET", "/Artists"},
	} {
		for name, form := range forms {
			for token, want := range map[string]int{e.token: 200, "nope": 401} {
				query, header, value := form(token)
				request, _ := http.NewRequest(route.method, e.servers[0].URL+route.path+query, strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				if header != "" {
					request.Header.Set(header, value)
				}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != want {
					t.Errorf("%s %s with %s and token %q: %d, want %d", route.method, route.path, name, token, response.StatusCode, want)
				}
			}
		}
	}
}

func TestDownloadIsOnTheStreamRole(t *testing.T) {
	e := newEnv(t)
	apiOnly := &Server{Pool: e.pool, Items: &items.Repo{Pool: e.pool, ServerID: "srv"}, Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(apiOnly.Handler())
	defer server.Close()
	e.servers = append(e.servers, server)
	if response, _ := e.do(t, len(e.servers)-1, "GET", "/Items/"+db.CompactID(e.item)+"/Download", e.token, nil); response.StatusCode != 404 {
		t.Fatalf("api role served a download: %d", response.StatusCode)
	}
}

func TestArtistsIsAnEmptyList(t *testing.T) {
	e := newEnv(t)
	response, body := e.do(t, 0, "GET", "/Artists?searchTerm=x&limit=5", e.token, nil)
	if response.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"Items":[],"TotalRecordCount":0,"StartIndex":0}` {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if response, _ := e.do(t, 0, "GET", "/Artists", "", nil); response.StatusCode != 401 {
		t.Errorf("without a token: %d", response.StatusCode)
	}
}

func TestUserDataWritesOnlyTheFieldsSent(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	path := "/UserItems/" + id + "/UserData"
	// Each write goes to the other replica, and the item is read from the first.
	n := 0
	send := func(body string) map[string]any {
		t.Helper()
		n++
		request, _ := http.NewRequest("POST", e.servers[n%2].URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", `MediaBrowser Token="`+e.token+`"`)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var dto map[string]any
		if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&dto) != nil {
			t.Fatalf("%s: %d", body, response.StatusCode)
		}
		return dto
	}

	if dto := send(`{"IsFavorite":true}`); dto["IsFavorite"] != true || dto["Played"] != false || dto["PlaybackPositionTicks"] != float64(0) || dto["ItemId"] != id {
		t.Fatalf("favorite: %v", dto)
	}
	if dto := send(`{"PlaybackPositionTicks":30000000000}`); dto["IsFavorite"] != true || dto["PlaybackPositionTicks"] != float64(30_000_000_000) || dto["PlayedPercentage"] != float64(30) {
		t.Fatalf("position: %v", dto)
	}
	// Played keeps the position and counts no play, as Jellyfin does.
	if dto := send(`{"Played":true}`); dto["Played"] != true || dto["PlaybackPositionTicks"] != float64(30_000_000_000) || dto["PlayCount"] != float64(0) {
		t.Fatalf("played: %v", dto)
	}
	// What is derived, or not stored, is ignored.
	if dto := send(`{"PlayedPercentage":99,"Rating":3,"Key":"x","ItemId":"y"}`); dto["PlaybackPositionTicks"] != float64(30_000_000_000) || dto["ItemId"] != id || dto["Rating"] != nil || dto["Played"] != true {
		t.Fatalf("ignored fields: %v", dto)
	}
	if dto := send(`{"Played":false,"IsFavorite":false}`); dto["Played"] != false || dto["IsFavorite"] != false || dto["PlaybackPositionTicks"] != float64(30_000_000_000) {
		t.Fatalf("both off: %v", dto)
	}
	// Every replica reads what any wrote.
	for replica := range 2 {
		response, body := e.do(t, replica, "GET", "/Items/"+id, e.token, nil)
		var item struct{ UserData map[string]any }
		if response.StatusCode != 200 || json.Unmarshal(body, &item) != nil || item.UserData["PlaybackPositionTicks"] != float64(30_000_000_000) {
			t.Fatalf("replica %d: %d %s", replica, response.StatusCode, body)
		}
	}
	// A position can be written lower; an absurd one is held to the limit.
	if dto := send(`{"PlaybackPositionTicks":10}`); dto["PlaybackPositionTicks"] != float64(10) {
		t.Fatalf("lower position: %v", dto)
	}
	if dto := send(`{"PlaybackPositionTicks":-5}`); dto["PlaybackPositionTicks"] != float64(0) {
		t.Fatalf("negative position: %v", dto)
	}
	if dto := send(`{"PlaybackPositionTicks":9223372036854775807}`); dto["PlaybackPositionTicks"] != float64(int64(1)<<50) {
		t.Fatalf("huge position: %v", dto)
	}
	if scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data WHERE user_id = $1", e.other) != 0 {
		t.Fatal("wrote another user's data")
	}
	if dto := send(`{"IsFavorite":true}`); dto["IsFavorite"] != true {
		t.Fatalf("%v", dto)
	}
}

// ?userId= is as in Jellyfin 12.4 (observed): a non-administrator may name only
// themselves, an administrator may name anyone who exists.
func TestUserDataUserID(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	path := "/UserItems/" + id + "/UserData"
	admin, adminToken := e.addUser(t, "admin")
	mustExec(t, e.pool, "UPDATE users SET is_admin = true WHERE id = $1", admin)
	rows := func(user string) int {
		return scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data WHERE user_id = $1", user)
	}
	post := func(token, query string) (int, map[string]any) {
		response, raw := e.do(t, 0, "POST", path+query, token, map[string]any{"IsFavorite": true})
		var dto map[string]any
		json.Unmarshal(raw, &dto)
		return response.StatusCode, dto
	}

	// A non-administrator naming anyone else is refused, known or not.
	for name, query := range map[string]string{"other user": "?userId=" + e.user, "admin": "?userId=" + admin, "unknown": "?userId=" + uuid.NewString()} {
		if code, _ := post(e.otherToken, query); code != 403 {
			t.Errorf("non-admin naming %s: %d, want 403", name, code)
		}
	}
	if rows(e.user)+rows(e.other)+rows(admin) != 0 {
		t.Fatal("a refused write left a row")
	}
	// Naming themselves is the same as naming nobody, in either spelling.
	for _, self := range []string{e.other, db.CompactID(e.other), strings.ToUpper(e.other)} {
		if code, dto := post(e.otherToken, "?userId="+self); code != 200 || dto["IsFavorite"] != true {
			t.Errorf("naming themselves as %s: %d %v", self, code, dto)
		}
	}
	if rows(e.other) != 1 {
		t.Fatal("a non-admin naming themselves did not write their own row")
	}
	// An administrator writes the named user's record and gets it back.
	if code, dto := post(adminToken, "?userId="+e.user); code != 200 || dto["IsFavorite"] != true || dto["ItemId"] != id {
		t.Fatalf("admin naming a user: %d %v", code, dto)
	}
	if rows(e.user) != 1 || rows(admin) != 0 {
		t.Fatalf("rows: viewer %d, admin %d", rows(e.user), rows(admin))
	}
	if code, _ := post(adminToken, "?userId="+uuid.NewString()); code != 404 {
		t.Errorf("admin naming an unknown user: %d, want 404", code)
	}
	if code, _ := post(adminToken, "?userId=zzz"); code != 400 {
		t.Errorf("admin naming a malformed id: %d, want 400", code)
	}
	if code, _ := post(e.otherToken, "?userId=zzz"); code != 400 {
		t.Errorf("non-admin naming a malformed id: %d, want 400", code)
	}
	if rows(admin) != 0 || rows(e.user) != 1 {
		t.Fatal("a refused write left a row")
	}
}

func TestUserDataNeedsAJSONContentType(t *testing.T) {
	e := newEnv(t)
	path := e.servers[0].URL + "/UserItems/" + db.CompactID(e.item) + "/UserData"
	for name, contentType := range map[string]string{"form": "application/x-www-form-urlencoded", "none": "", "text": "text/plain", "json": "application/json", "json charset": "application/json; charset=utf-8"} {
		request, _ := http.NewRequest("POST", path, strings.NewReader(`{"IsFavorite":true}`))
		request.Header.Set("Authorization", `MediaBrowser Token="`+e.token+`"`)
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var problem struct{ Title string }
		json.NewDecoder(response.Body).Decode(&problem)
		response.Body.Close()
		want := 415
		if strings.HasPrefix(name, "json") {
			want = 200
		}
		if response.StatusCode != want || (want == 415 && problem.Title != "Unsupported Media Type") {
			t.Errorf("%s: %d %q", name, response.StatusCode, problem.Title)
		}
		if want == 415 && scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data") != 0 {
			t.Fatalf("%s: a refused write left a row", name)
		}
		mustExec(t, e.pool, "DELETE FROM user_item_data")
	}
}

func TestUserDataRefusals(t *testing.T) {
	e := newEnv(t)
	path := "/UserItems/" + db.CompactID(e.item) + "/UserData"
	for name, c := range map[string]struct {
		method, path, token string
		body                any
		want                int
	}{
		"no token":     {"POST", path, "", map[string]any{"IsFavorite": true}, 401},
		"bad token":    {"POST", path, "nope", map[string]any{"IsFavorite": true}, 401},
		"empty body":   {"POST", path, e.token, json.RawMessage(""), 400},
		"not json":     {"POST", path, e.token, "nonsense", 400},
		"wrong type":   {"POST", path, e.token, map[string]any{"Played": "x"}, 400},
		"missing item": {"POST", "/UserItems/" + uuid.NewString() + "/UserData", e.token, map[string]any{"IsFavorite": true}, 404},
		"not an id":    {"POST", "/UserItems/zzz/UserData", e.token, map[string]any{"IsFavorite": true}, 400},
		// Nothing reads it, so it stays a non-goal answered 404 (PRODUCT.md).
		"GET": {"GET", path, e.token, nil, 404},
	} {
		body := c.body
		if s, ok := body.(string); ok {
			body = json.RawMessage(s)
		}
		response, _ := e.do(t, 0, c.method, c.path, c.token, body)
		if response.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, response.StatusCode, c.want)
		}
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data"); n != 0 {
		t.Fatalf("a refused write left %d rows", n)
	}
}

// An offline player syncs data it kept, possibly after the viewer has watched
// more elsewhere. The older body must never undo the newer position.
func TestUserDataOlderThanStoredCannotUndoANewerPosition(t *testing.T) {
	e := newEnv(t)
	path := "/UserItems/" + db.CompactID(e.item) + "/UserData"
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	write := func(body map[string]any) map[string]any {
		t.Helper()
		response, raw := e.do(t, 0, "POST", path, e.token, body)
		var dto map[string]any
		if response.StatusCode != 200 || json.Unmarshal(raw, &dto) != nil {
			t.Fatalf("%v: %d %s", body, response.StatusCode, raw)
		}
		return dto
	}
	stamp := func(d time.Duration) string { return at.Add(d).Format(time.RFC3339) }

	write(map[string]any{"PlaybackPositionTicks": 500, "LastPlayedDate": stamp(0)})
	// Older, with a position, played and a date: all three dropped; the favourite, which has no time, applies.
	dto := write(map[string]any{"PlaybackPositionTicks": 100, "Played": true, "LastPlayedDate": stamp(-time.Hour), "IsFavorite": true})
	if dto["PlaybackPositionTicks"] != float64(500) || dto["Played"] != false || dto["IsFavorite"] != true || dto["LastPlayedDate"] != at.Format(time.RFC3339Nano) {
		t.Fatalf("older body: %v", dto)
	}
	// The same moment is a retry, and applies.
	if dto := write(map[string]any{"PlaybackPositionTicks": 600, "LastPlayedDate": stamp(0)}); dto["PlaybackPositionTicks"] != float64(600) {
		t.Fatalf("same moment: %v", dto)
	}
	// Newer applies and moves the date.
	if dto := write(map[string]any{"PlaybackPositionTicks": 700, "LastPlayedDate": stamp(time.Hour)}); dto["PlaybackPositionTicks"] != float64(700) {
		t.Fatalf("newer: %v", dto)
	}
	// No date is a change made now.
	if dto := write(map[string]any{"PlaybackPositionTicks": 50}); dto["PlaybackPositionTicks"] != float64(50) {
		t.Fatalf("undated: %v", dto)
	}
	// Watching after the sync's date also wins over it: a Stopped sets the time to now.
	if code := e.report(t, 0, "/Stopped", db.CompactID(e.item), filmTicks/2); code != 204 {
		t.Fatalf("stopped: %d", code)
	}
	dto = write(map[string]any{"PlaybackPositionTicks": 1, "LastPlayedDate": stamp(2 * time.Hour)})
	if dto["PlaybackPositionTicks"] != float64(filmTicks/2) {
		t.Fatalf("a body dated before the last watch undid it: %v", dto)
	}
}

// A client's clock, or a typo, must not pin the stale-write rule: a far-future
// date is held to now, so a later honest sync still applies.
func TestUserDataFutureDateDoesNotPinTheRule(t *testing.T) {
	e := newEnv(t)
	path := "/UserItems/" + db.CompactID(e.item) + "/UserData"
	if response, _ := e.do(t, 0, "POST", path, e.token, map[string]any{"PlaybackPositionTicks": 1, "LastPlayedDate": "9999-12-31T23:59:59Z"}); response.StatusCode != 200 {
		t.Fatalf("future date: %d", response.StatusCode)
	}
	if scalar[bool](t, e.pool, "SELECT last_played_at > now() FROM user_item_data") {
		t.Fatal("stored a date in the future")
	}
	response, raw := e.do(t, 0, "POST", path, e.token, map[string]any{"PlaybackPositionTicks": 7_000_000, "Played": true, "LastPlayedDate": time.Now().UTC()})
	var dto map[string]any
	if response.StatusCode != 200 || json.Unmarshal(raw, &dto) != nil || dto["PlaybackPositionTicks"] != float64(7_000_000) || dto["Played"] != true {
		t.Fatalf("an honest sync after a future-dated body was dropped: %d %s", response.StatusCode, raw)
	}
}

// A change made now, with no date, is newer than any older-dated sync that
// arrives after it.
func TestUserDataUndatedChangesStampNow(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	path := "/UserItems/" + id + "/UserData"
	old := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond) // Postgres keeps microseconds.
	older := old.Add(time.Hour)
	write := func(body map[string]any) map[string]any {
		t.Helper()
		response, raw := e.do(t, 0, "POST", path, e.token, body)
		var dto map[string]any
		if response.StatusCode != 200 || json.Unmarshal(raw, &dto) != nil {
			t.Fatalf("%v: %d %s", body, response.StatusCode, raw)
		}
		return dto
	}

	t.Run("position then older dated", func(t *testing.T) {
		write(map[string]any{"PlaybackPositionTicks": 100, "LastPlayedDate": old})
		write(map[string]any{"PlaybackPositionTicks": 500})
		if got := scalar[time.Time](t, e.pool, "SELECT last_played_at FROM user_item_data"); time.Since(got) > time.Minute {
			t.Fatalf("an undated position left LastPlayedDate at %s", got)
		}
		if dto := write(map[string]any{"PlaybackPositionTicks": 900, "Played": true, "LastPlayedDate": older}); dto["PlaybackPositionTicks"] != float64(500) || dto["Played"] != false {
			t.Fatalf("an older body undid an undated position: %v", dto)
		}
	})
	t.Run("played then older dated", func(t *testing.T) {
		mustExec(t, e.pool, "DELETE FROM user_item_data")
		write(map[string]any{"PlaybackPositionTicks": 100, "LastPlayedDate": old})
		write(map[string]any{"Played": true})
		if dto := write(map[string]any{"Played": false, "PlaybackPositionTicks": 900, "LastPlayedDate": older}); dto["Played"] != true || dto["PlaybackPositionTicks"] != float64(100) {
			t.Fatalf("an older body undid an undated Played: %v", dto)
		}
	})
	t.Run("unplay then older dated played", func(t *testing.T) {
		mustExec(t, e.pool, "DELETE FROM user_item_data")
		write(map[string]any{"Played": true, "LastPlayedDate": old})
		if response, _ := e.do(t, 0, "DELETE", "/UserPlayedItems/"+id, e.token, nil); response.StatusCode != 200 {
			t.Fatalf("unplay: %d", response.StatusCode)
		}
		// Unplaying is not playing: clients still see when it was last played.
		if got := scalar[time.Time](t, e.pool, "SELECT last_played_at FROM user_item_data"); !got.Equal(old) {
			t.Fatalf("unplay changed LastPlayedDate to %s, was %s", got, old)
		}
		if dto := write(map[string]any{"Played": true, "LastPlayedDate": older}); dto["Played"] != false {
			t.Fatalf("an older Played undid the unplay: %v", dto)
		}
		// A never-played item marked unplayed has no LastPlayedDate to show.
		mustExec(t, e.pool, "DELETE FROM user_item_data")
		if response, _ := e.do(t, 0, "DELETE", "/UserPlayedItems/"+id, e.token, nil); response.StatusCode != 200 {
			t.Fatalf("unplay: %d", response.StatusCode)
		}
		if scalar[bool](t, e.pool, "SELECT last_played_at IS NOT NULL FROM user_item_data") {
			t.Fatal("unplaying a never-played item gave it a LastPlayedDate")
		}
	})
	t.Run("favourite alone leaves the date", func(t *testing.T) {
		mustExec(t, e.pool, "DELETE FROM user_item_data")
		write(map[string]any{"PlaybackPositionTicks": 100, "LastPlayedDate": old})
		dto := write(map[string]any{"IsFavorite": true})
		if dto["LastPlayedDate"] != old.Format(time.RFC3339Nano) {
			t.Fatalf("a favourite-only write changed LastPlayedDate: %v, was %s", dto["LastPlayedDate"], old.Format(time.RFC3339Nano))
		}
		// And a body that never mentions it does not clear it on a fresh row either.
		mustExec(t, e.pool, "DELETE FROM user_item_data")
		if dto := write(map[string]any{"IsFavorite": true}); dto["LastPlayedDate"] != nil {
			t.Fatalf("a favourite-only write dated a new row: %v", dto["LastPlayedDate"])
		}
	})
}

// Writers on both replicas race with bodies of different ages. Whatever order
// they land in, the newest body's position is what remains.
func TestConcurrentStaleSyncsKeepTheNewestPosition(t *testing.T) {
	e := newEnv(t)
	path := "/UserItems/" + db.CompactID(e.item) + "/UserData"
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const writers = 40
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{"PlaybackPositionTicks": 1000 + i, "LastPlayedDate": at.Add(time.Duration(i) * time.Minute)})
			request, _ := http.NewRequest("POST", e.servers[i%2].URL+path, bytes.NewReader(body))
			request.Header.Set("Authorization", `MediaBrowser Token="`+e.token+`"`)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
				return
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				// Say what the server answered, so a rare failure names its cause (BL-123).
				answer, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
				t.Errorf("writer %d: %d %s", i, response.StatusCode, answer)
				return
			}
		}()
	}
	wg.Wait()
	if r := e.row(t, e.user); r.position != 1000+writers-1 {
		t.Fatalf("position %d, want %d", r.position, 1000+writers-1)
	}
}
