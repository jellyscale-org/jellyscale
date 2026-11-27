//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/db"
)

// JS-033, user management: the kit of the checks beside it. Real api processes (and, where media is asked for, a real
// stream process) over the one test Postgres. Everything a test changes about a user it changes over HTTP; Postgres is
// written only to make the first accounts and films, and read where the criterion is about rows.

type usersSite struct {
	*qcSite
}

// newUsersSite starts `replicas` api processes with no configured administrator (unless extra names one).
func newUsersSite(t *testing.T, replicas int, extra map[string]any) *usersSite {
	t.Helper()
	config := map[string]any{"adminUser": ""}
	for key, value := range extra {
		config[key] = value
	}
	return &usersSite{newQCSite(t, replicas, config)}
}

// as is one request to replica i with the token ("" sends none) and a JSON body ("" sends none).
func (u *usersSite) as(replica int, token, method, path, body string, headers ...string) signInReply {
	if token != "" {
		headers = append(headers, "X-Emby-Token", token)
	}
	return u.call(replica, method, path, body, headers...)
}

func (u *usersSite) must(want int, replica int, token, method, path, body, what string) signInReply {
	u.t.Helper()
	got := u.as(replica, token, method, path, body)
	if got.status != want {
		u.t.Fatalf("%s: %s %s answered %d %.200q, want %d", what, method, path, got.status, got.body, want)
	}
	return got
}

func (u *usersSite) check(want int, got signInReply, what string) {
	u.t.Helper()
	if got.status != want {
		u.t.Errorf("%s: %d %.200q, want %d", what, got.status, got.body, want)
	}
}

func body(fields map[string]any) string {
	raw, _ := json.Marshal(fields)
	return string(raw)
}

// strong is a password no list of common ones holds.
func strong() string { return "Qx7!" + uuid.NewString() }

type userDTO struct {
	Name, Id    string
	HasPassword bool
	Policy      map[string]any
}

func decodeUser(t *testing.T, got signInReply, what string) userDTO {
	t.Helper()
	var user userDTO
	if err := json.Unmarshal(got.body, &user); err != nil || user.Id == "" {
		t.Fatalf("%s: %d %.200q is no user", what, got.status, got.body)
	}
	return user
}

// me is GET /Users/Me with the token.
func (u *usersSite) meDTO(replica int, token string) userDTO {
	u.t.Helper()
	return decodeUser(u.t, u.must(200, replica, token, "GET", "/Users/Me", "", "reading the user"), "GET /Users/Me")
}

// listed is GET /Users as the token.
func (u *usersSite) listed(replica int, token, query string) []userDTO {
	u.t.Helper()
	got := u.must(200, replica, token, "GET", "/Users"+query, "", "listing users")
	var users []userDTO
	if err := json.Unmarshal(got.body, &users); err != nil {
		u.t.Fatalf("GET /Users%s: %.200q is no list", query, got.body)
	}
	return users
}

func names(users []userDTO) []string {
	var out []string
	for _, user := range users {
		out = append(out, user.Name)
	}
	sort.Strings(out)
	return out
}

// created is a user made through POST /Users/New by the administrator.
func (u *usersSite) created(admin qcAccount, name, password string) userDTO {
	u.t.Helper()
	fields := map[string]any{"Name": name, "Password": password}
	return decodeUser(u.t, u.must(200, 0, admin.token, "POST", "/Users/New", body(fields), "making a user"), "POST /Users/New")
}

// policy posts the fields to the user's Policy as the token.
func (u *usersSite) policy(replica int, token, userID string, fields map[string]any) signInReply {
	return u.as(replica, token, "POST", "/Users/"+userID+"/Policy", body(fields))
}

func (u *usersSite) setPolicy(admin qcAccount, userID string, fields map[string]any) {
	u.t.Helper()
	if got := u.policy(0, admin.token, userID, fields); got.status != 204 {
		u.t.Fatalf("the administrator's POST /Users/%s/Policy %v: %d %.200q, want 204", userID, fields, got.status, got.body)
	}
}

// storedPolicy is the Policy GET /Users/{id} answers the administrator.
func (u *usersSite) storedPolicy(admin qcAccount, userID string) map[string]any {
	u.t.Helper()
	return decodeUser(u.t, u.must(200, 1%len(u.apis), admin.token, "GET", "/Users/"+userID, "", "reading a user's Policy"), "GET /Users/{id}").Policy
}

func (u *usersSite) wantPolicy(admin qcAccount, userID, what string, want map[string]any) {
	u.t.Helper()
	got := u.storedPolicy(admin, userID)
	for field, value := range want {
		if got[field] != value {
			u.t.Errorf("%s: Policy.%s is %v, want %v", what, field, got[field], value)
		}
	}
}

func (u *usersSite) userCount(name string) int {
	u.t.Helper()
	var n int
	if err := u.pool.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE lower(name) = lower($1)", name).Scan(&n); err != nil {
		u.t.Fatal(err)
	}
	return n
}

// withNoPassword is an account that already exists with no password: its stored hash is the empty password's. No route
// makes one (D-100; a reset leaves one no password signs in to), so it is written to the table.
func (u *usersSite) withNoPassword() (id, name string) {
	u.t.Helper()
	id, name = uuid.NewString(), "bare-"+uuid.NewString()[:8]
	hash, err := bcrypt.GenerateFromPassword(nil, bcrypt.MinCost)
	if err != nil {
		u.t.Fatal(err)
	}
	if _, err := u.pool.Exec(context.Background(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, false)`, id, name, string(hash)); err != nil {
		u.t.Fatal(err)
	}
	return id, name
}

// onlyThese deletes every other user, so that "the last administrator" means one of the accounts the test made.
func (u *usersSite) onlyThese(keep ...qcAccount) {
	u.t.Helper()
	var ids []string
	for _, account := range keep {
		ids = append(ids, account.id)
	}
	if _, err := u.pool.Exec(context.Background(), "DELETE FROM users WHERE NOT (id = ANY($1::uuid[]))", ids); err != nil {
		u.t.Fatal(err)
	}
}

// rowsOf is what a user holds, one text per table, so that "unchanged" is byte for byte.
func (u *usersSite) rowsOf(userID string) map[string]string {
	u.t.Helper()
	queries := map[string]string{
		"users":               `SELECT x::text FROM users x WHERE x.id = $1::uuid`,
		"user_item_data":      `SELECT x::text FROM user_item_data x WHERE x.user_id = $1::uuid`,
		"display_preferences": `SELECT x::text FROM display_preferences x WHERE x.user_id = $1::uuid`,
		"devices":             `SELECT x::text FROM devices x WHERE x.user_id = $1::uuid`,
		"sessions":            `SELECT x::text FROM sessions x WHERE x.user_id = $1::uuid`,
		"session_sockets":     `SELECT x::text FROM session_sockets x WHERE x.token IN (SELECT token FROM devices WHERE user_id = $1::uuid)`,
		"quickconnect_codes":  `SELECT x::text FROM quickconnect_codes x WHERE x.approved_by = $1::uuid`,
	}
	out := map[string]string{}
	for table, query := range queries {
		rows, err := u.pool.Query(context.Background(), query, userID)
		if err != nil {
			u.t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			u.t.Fatal(err)
		}
		sort.Strings(texts)
		out[table] = strings.Join(texts, "\n")
	}
	return out
}

// usersFilm is one synthetic film in a POSIX library: an item row, a media source and a file of size bytes.
type usersFilm struct {
	item, source string
	size         int64
}

func (u *usersSite) film(size int64) usersFilm {
	u.t.Helper()
	dir := u.t.TempDir()
	file, err := os.Create(filepath.Join(dir, "film.mkv"))
	if err != nil {
		u.t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil { // Sparse: it takes no disk.
		u.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		u.t.Fatal(err)
	}
	library, item := uuid.NewString(), uuid.NewString()
	exec := func(sql string, args ...any) {
		if _, err := u.pool.Exec(context.Background(), sql, args...); err != nil {
			u.t.Fatal(err)
		}
	}
	exec(`INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, $2, 'movies', $3, $3)`, library, "users-"+library, "file://"+dir)
	u.t.Cleanup(func() {
		if _, err := u.pool.Exec(context.Background(), "DELETE FROM libraries WHERE id = $1", library); err != nil {
			u.t.Error(err)
		}
	})
	dto := `{"Name":"Users Film","Type":"Movie","MediaType":"Video","CanDownload":true,"MediaSources":[{"Id":"` + db.CompactID(item) +
		`","Protocol":"File","Container":"mkv","SupportsTranscoding":true,"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","IsDefault":true}]}]}`
	exec(`INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', 'Users Film', 'users film', $3, 900000000, $4)`, item, library, "users-"+item, dto)
	exec(`INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		VALUES ($1, $1, 'film.mkv', $2, 'users-etag', 'mkv', '[]')`, item, size)
	return usersFilm{item: db.CompactID(item), source: db.CompactID(item), size: size}
}

func (f usersFilm) streamPath(token string) string {
	return "/Videos/" + f.item + "/stream?Static=true&mediaSourceId=" + f.source + "&ApiKey=" + token
}

// rangeStatus asks the stream process for the film's first ten bytes.
func rangeStatus(stream *proc, film usersFilm, token string) int {
	request, _ := http.NewRequest("GET", stream.url(film.streamPath(token)), nil)
	request.Header.Set("Range", "bytes=0-9")
	response, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body) //nolint:errcheck
	return response.StatusCode
}

func procGet(p *proc, token, path string) signInReply {
	request, _ := http.NewRequest("GET", p.url(path), bytes.NewReader(nil))
	request.Header.Set("X-Emby-Token", token)
	response, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return signInReply{body: []byte(err.Error())}
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return signInReply{response.StatusCode, response.Header, raw}
}

// warnLines are the lines any api process logged at warn that hold every one of words (case ignored).
func (u *usersSite) logLines(level string, words ...string) []string {
	var out []string
	for _, p := range u.apis {
		for _, line := range strings.Split(p.log.String(), "\n") {
			lower := strings.ToLower(line)
			if level != "" && !strings.Contains(line, "level="+level) && !strings.Contains(line, `"level":"`+level+`"`) {
				continue
			}
			if !slices.ContainsFunc(words, func(word string) bool { return !strings.Contains(lower, strings.ToLower(word)) }) {
				out = append(out, line)
			}
		}
	}
	return out
}
