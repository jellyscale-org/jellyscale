//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-116. A viewer's settings are saved by POST /Users/{userId}/Configuration and POST /Users/Configuration and
// come back in GET /Users/Me, GET /Users/{userId} and the User of the next sign-in. Every one of the 124 cases of
// compat/captures/user-configuration-12.4.json (a real Jellyfin 12.4, recorded by
// test/differential/capture_user_configuration.py) is replayed against two api processes of jellyscale on one
// Postgres, and both are held to what the capture records. A replica is its own process, so what one process
// saved is read by another that has never seen the user; the processes are then restarted and read again.
//
// Where jellyscale deliberately answers otherwise, expectedOf says so: a body over 64 KB, which Jellyfin stores
// and jellyscale refuses with the status of a body that is not JSON, storing nothing; and an id that is no user, named
// by a non-administrator, which Jellyfin answers 404 and jellyscale 403 (D-075).

const configCapturePath = "../../compat/captures/user-configuration-12.4.json"
const configPassword = "capture-pass"

// Over this many bytes a body is refused.
const bodyLimit = 64 << 10

// configUser is one of the three users a case makes: U and O are not administrators, A is.
type configUser struct {
	id     string // A uuid, as the users table holds it.
	name   string
	tokens [2]string // Two devices that are signed in.
}

func (u configUser) compact() string { return db.CompactID(u.id) }

type configUsers map[string]configUser

// newConfigUsers inserts U, O and A with only a name and a password, as every account of today has, and two
// signed-in devices each.
func newConfigUsers(t *testing.T, dsn, tag string) configUsers {
	t.Helper()
	pool := connect(t, dsn)
	hash, err := bcrypt.GenerateFromPassword([]byte(configPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	users := configUsers{}
	for _, who := range []string{"U", "O", "A"} {
		u := configUser{id: uuid.NewString(), name: tag + "-" + who}
		for n := range u.tokens {
			u.tokens[n] = uuid.NewString()
		}
		if _, err := pool.Exec(context.Background(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, $4)`,
			u.id, u.name, string(hash), who == "A"); err != nil {
			t.Fatal(err)
		}
		for n, token := range u.tokens {
			if _, err := pool.Exec(context.Background(), `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version)
				VALUES ($1, $2, $3, 'd', 'capture', '1')`, token, u.id, fmt.Sprintf("%s-device-%d", u.name, n)); err != nil {
				t.Fatal(err)
			}
		}
		users[who] = u
	}
	return users
}

func (users configUsers) target(name string) string {
	u := users["U"]
	dashed, compact := u.id, u.compact()
	switch name {
	case "U", "O", "A":
		return users[name].compact()
	case "unknown":
		return "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
	case "malformed":
		return "not-a-guid"
	case "none":
		return ""
	case "selfDashed", "Udashed":
		return dashed
	case "selfCompact":
		return compact
	case "selfDashedUpper":
		return strings.ToUpper(dashed)
	case "selfCompactUpper", "UcompactUpper":
		return strings.ToUpper(compact)
	}
	panic("the capture names a target this test does not know: " + name)
}

func (users configUsers) token(actor string, device int) string {
	switch actor {
	case "anonymous":
		return ""
	case "badtoken":
		return strings.Repeat("0", 32)
	}
	return users[actor].tokens[device]
}

// doRequest sends a request and returns its status and body. It is called from goroutines too, so a failure to send
// is reported with Errorf and answers 0.
func doRequest(t *testing.T, r *proc, method, path, token, contentType, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, r.url(path), strings.NewReader(body))
	if err != nil {
		t.Errorf("%v", err)
		return 0, nil
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	header := `MediaBrowser Client="capture", Device="capture", DeviceId="capture-device", Version="1"`
	if token != "" {
		header += `, Token="` + token + `"`
	}
	request.Header.Set("Authorization", header)
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return 0, nil
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

func savePath(route, target string) string {
	switch route {
	case "path":
		return "/Users/" + target + "/Configuration"
	case "query":
		return "/Users/Configuration?userId=" + target
	}
	return "/Users/Configuration"
}

// send makes the step's request on r and returns its status and body.
func send(t *testing.T, r *proc, users configUsers, step testenv.ConfigStep, caseName string) (int, []byte) {
	t.Helper()
	contentType := step.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return doRequest(t, r, "POST", savePath(step.Route, users.target(step.Target)), users.token(step.Actor, 0), contentType, step.BodyText(caseName))
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %.300s", err, raw)
	}
	return out
}

func savedConfigOf(t *testing.T, r *proc, token string) map[string]any {
	t.Helper()
	status, raw := doRequest(t, r, "GET", "/Users/Me", token, "", "")
	if status != 200 {
		t.Fatalf("GET /Users/Me on %s: %d %s", r.addr, status, raw)
	}
	config, _ := decode(t, raw)["Configuration"].(map[string]any)
	return config
}

var subtitleModes = []string{"Default", "Always", "OnlyForced", "None", "Smart"}

// seen is what one of the four places answers about one user: GET /Users/Me, GET /Users/{id} by the
// administrator and, for U, by U, and the User of U's next AuthenticateByName.
type seen struct {
	place, who string
	config     map[string]any
}

// observe reads the places on r, with each user's second device (the first request r has had for any of them
// after a save on the other process).
func observe(t *testing.T, r *proc, users configUsers) []seen {
	t.Helper()
	var out []seen
	get := func(place, who, path, token string) {
		status, raw := doRequest(t, r, "GET", path, token, "", "")
		if status != 200 {
			t.Fatalf("%s of %s on %s: %d %s", place, who, r.addr, status, raw)
		}
		config, _ := decode(t, raw)["Configuration"].(map[string]any)
		out = append(out, seen{place, who, config})
	}
	for _, who := range []string{"U", "O", "A"} {
		get("me", who, "/Users/Me", users[who].tokens[1])
		get("byAdmin", who, "/Users/"+users[who].compact(), users["A"].tokens[1])
	}
	get("byUserSelf", "U", "/Users/"+users["U"].compact(), users["U"].tokens[1])
	status, raw := r.authenticate(t, "capture-device", users["U"].name, configPassword)
	if status != 200 {
		t.Fatalf("signing U in: %d %s", status, raw)
	}
	config, _ := decode(t, raw)["User"].(map[string]any)["Configuration"].(map[string]any)
	return append(out, seen{"authenticateU", "U", config})
}

// expectation is what a case must answer and leave.
type expectation struct {
	status int
	after  testenv.ConfigAfter
}

// expectedOf is the capture's answer to a case, except where jellyscale does better on purpose: a body over
// 64 KB is refused with the status of a body that is not JSON and stores nothing, which is what the capture
// records for that route under "body that is not JSON".
func expectedOf(t *testing.T, capture *testenv.UserConfigurationCapture, k testenv.ConfigCase) expectation {
	t.Helper()
	// D-075 (JS-127): a non-administrator naming an id that is no user is answered 403, as for another user's id, so that
	// the answer does not say whether the id is a user. Jellyfin answers 404. Nothing is written either way.
	if strings.HasPrefix(k.Name, "unknown id as a non-administrator, route=") {
		return expectation{http.StatusForbidden, k.After}
	}
	if k.Request != nil && k.Request.Body == nil && k.Request.BodyBytes > bodyLimit {
		route := k.Name[strings.LastIndex(k.Name, "route=")+len("route="):]
		refused := capture.Case(t, "body that is not JSON, route="+route)
		return expectation{refused.Status, refused.After}
	}
	return expectation{k.Status, k.After}
}

// check holds what was seen to the expectation, and every SubtitleMode to the five spellings of the schema.
func check(t *testing.T, capture *testenv.UserConfigurationCapture, name string, want expectation, got []seen) {
	t.Helper()
	for _, s := range got {
		wanted := want.after.View(capture, s.place, s.who)
		if !reflect.DeepEqual(s.config, wanted) {
			t.Errorf("%s: %s of %s differs in %s", name, s.place, s.who, differing(s.config, wanted))
		}
		if mode, _ := s.config["SubtitleMode"].(string); !slices.Contains(subtitleModes, mode) {
			t.Errorf("%s: %s of %s holds SubtitleMode %v, not one of the five spellings", name, s.place, s.who, s.config["SubtitleMode"])
		}
	}
}

// differing names the fields two Configurations disagree on, with both values.
func differing(got, want map[string]any) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(union(got, want))) {
		g, gok := got[k]
		w, wok := want[k]
		if gok != wok || !reflect.DeepEqual(g, w) {
			out = append(out, fmt.Sprintf("%s: got %s, want %s", k, show(g, gok), show(w, wok)))
		}
	}
	return strings.Join(out, "; ")
}

func union(a, b map[string]any) map[string]any {
	out := map[string]any{}
	maps.Copy(out, a)
	maps.Copy(out, b)
	return out
}

func show(v any, present bool) string {
	if !present {
		return "(absent)"
	}
	text, _ := json.Marshal(v)
	if len(text) > 80 {
		text = append(text[:80], "..."...)
	}
	return string(text)
}

func bothProcesses(t *testing.T, dsn string) (a, b *proc) {
	t.Helper()
	return start(t, "api", dsn, nil), start(t, "api", dsn, nil)
}

// rowsHolding names the tables in which some row holds the text somewhere.
func rowsHolding(t *testing.T, dsn, text string) []string {
	t.Helper()
	pool := connect(t, dsn)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var holding []string
	for _, table := range tables {
		var n int
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %q x WHERE x::text LIKE $1`, table), "%"+text+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			holding = append(holding, table)
		}
	}
	return holding
}

// Every case of the capture, through two processes that take turns saving and reading, then through two new ones.
func TestEveryCaseOfTheCaptureIsAnsweredAsJellyfinDoes(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	run := uuid.NewString()[:8]

	type kept struct {
		name  string
		users configUsers
		want  expectation
	}
	var all []kept
	for n, k := range capture.Cases {
		saver, reader := a, b
		if n%2 == 1 {
			saver, reader = b, a
		}
		users := newConfigUsers(t, dsn, fmt.Sprintf("c%s-%d", run, n))
		// The reader answers for the users before the save: a process that kept what it read would show it.
		observe(t, reader, users)
		for _, step := range k.Before {
			if status, raw := send(t, saver, users, step, k.Name); status != 204 {
				t.Fatalf("%s: a step before the request answered %d %s, want 204", k.Name, status, raw)
			}
		}
		want := expectedOf(t, capture, k)
		if k.Request != nil {
			status, raw := send(t, saver, users, *k.Request, k.Name)
			if status != want.status {
				t.Errorf("%s: answered %d, the capture records %d", k.Name, status, want.status)
			}
			if strings.Contains(string(raw), "secret-value") || strings.Contains(string(raw), "NoSuchField") {
				t.Errorf("%s: the answer repeats the unknown field: %s", k.Name, raw)
			}
		}
		check(t, capture, k.Name, want, observe(t, reader, users))
		check(t, capture, k.Name+" (on the process that saved)", want, observe(t, saver, users))
		if strings.HasPrefix(k.Name, "unknown field beside known ones") {
			for _, text := range []string{"secret-value", "NoSuchField"} {
				if holding := rowsHolding(t, dsn, text); len(holding) > 0 {
					t.Errorf("%s: %q is stored in %v", k.Name, text, holding)
				}
			}
		}
		if t.Failed() {
			t.Fatalf("stopped at the first case that fails: %q", k.Name)
		}
		all = append(all, kept{k.Name, users, want})
	}

	// Both processes restart and every user of every case is read again, the first request each has had.
	a.kill()
	b.kill()
	a, b = bothProcesses(t, dsn)
	for n, k := range all {
		reader := a
		if n%2 == 1 {
			reader = b
		}
		check(t, capture, k.name+" (after a restart)", k.want, observe(t, reader, k.users))
	}
}

// A save by one user leaves each of another user's 16 fields as they were, whichever process saves.
func TestASaveByOneUserLeavesAnotherUsersSixteenFieldsAsTheyWere(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	users := newConfigUsers(t, dsn, "iso"+uuid.NewString()[:8])
	saved := capture.Case(t, "one field after all 16 changed, route=path").Before // U and O each save all 16, differently.
	wantO := capture.Config(capture.Case(t, "one field after all 16 changed, route=path").After.Me["O"])
	for n, step := range saved {
		if status, raw := send(t, []*proc{a, b}[n], users, step, ""); status != 204 {
			t.Fatalf("save %d: %d %s", n, status, raw)
		}
	}
	for n, body := range []string{`{"SubtitleMode":"Smart"}`, *saved[0].Body, `{"SubtitleLanguagePreference":"ita"}`} {
		if status, _ := doRequest(t, []*proc{b, a, b}[n], "POST", "/Users/Configuration", users.token("U", 0), "application/json", body); status != 204 {
			t.Fatalf("U's save %d: %d", n, status)
		}
		for _, r := range []*proc{a, b} {
			if got := savedConfigOf(t, r, users.token("O", 1)); !reflect.DeepEqual(got, wantO) {
				t.Errorf("after U's save %d, O's Configuration on %s is %v, want %v", n, r.addr, got, wantO)
			}
		}
	}
}

// Two saves for one user sent at the same moment through two api processes, 24 times: all 16 fields are those
// of one of the two bodies. Here the processes race for real; TestAnInterleavedSaveIsNotMixed forces the
// interleaving that would mix them.
func TestTwoSavesAtOnceThroughTwoProcessesAreNeverMixed(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	users := newConfigUsers(t, dsn, "race"+uuid.NewString()[:8])
	bodyA, bodyB := *capture.Cases[2].Before[0].Body, *capture.Cases[2].Before[1].Body
	wantA := capture.Config(capture.Case(t, "full body, route=path").After.Me["U"])
	wantB := capture.Config(capture.Case(t, "one field after all 16 changed, route=path").After.Me["O"])
	if reflect.DeepEqual(wantA, wantB) {
		t.Fatal("the two bodies leave the same Configuration")
	}
	routes := []string{"path", "query", "none"}
	for round := 0; round < 24; round++ {
		path := savePath(routes[round%3], users["U"].compact())
		first, second := a, b
		if round%2 == 1 {
			first, second = b, a
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		for n, one := range []struct {
			r    *proc
			body string
		}{{first, bodyA}, {second, bodyB}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				if status, raw := doRequest(t, one.r, "POST", path, users.token("U", n), "application/json", one.body); status != 204 {
					t.Errorf("round %d: a save answered %d %s", round, status, raw)
				}
			}()
		}
		close(gate)
		wg.Wait()
		got := [2]map[string]any{savedConfigOf(t, a, users.token("U", 1)), savedConfigOf(t, b, users.token("U", 1))}
		if !reflect.DeepEqual(got[0], got[1]) {
			t.Fatalf("round %d: the two processes read different Configurations:\n%v\n%v", round, got[0], got[1])
		}
		if !reflect.DeepEqual(got[0], wantA) && !reflect.DeepEqual(got[0], wantB) {
			t.Fatalf("round %d: the Configuration is a mix of the two bodies:\n%v\nbody A %v\nbody B %v", round, got[0], wantA, wantB)
		}
	}
}

// The interleaving that mixes a build which saves a user's fields in more than one statement, forced: the first
// statement that writes the users table from the first process is answered by Postgres and then held until the
// second process's save has finished (or can go no further, because the row is locked). Whatever the build,
// the Configuration the user ends with is one body's, whole. A one-line mutant this fails every run: one
// UPDATE per column, outside a transaction. The hold and the second save's wait are bounded (30 s, 5 s): hangs,
// not measures; nothing here depends on how long a save takes.
func TestAnInterleavedSaveIsNotMixed(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	dsn := testenv.DatabaseURL(t)
	// Exec mode sends each statement's text and its arguments in one round trip, so the
	// hook's "after" runs when Postgres has answered the UPDATE itself and not a prepare of it. With pgx's default
	// statement cache the prepare is what the hook would hold, before any column was written, and the mutant above
	// would pass.
	proxy := testenv.NewDBProxy(t, dsn)
	first := start(t, "api", proxy.DSN()+"&default_query_exec_mode=exec", nil)
	second := start(t, "api", dsn, nil)
	users := newConfigUsers(t, dsn, "forced"+uuid.NewString()[:8])
	bodyA, bodyB := *capture.Cases[2].Before[0].Body, *capture.Cases[2].Before[1].Body
	wantA := capture.Config(capture.Case(t, "full body, route=path").After.Me["U"])
	wantB := capture.Config(capture.Case(t, "one field after all 16 changed, route=path").After.Me["O"])

	// Both processes have answered for the user, so what the hook holds is the save and not a first connection.
	observe(t, first, users)
	observe(t, second, users)

	held, release := make(chan struct{}), make(chan struct{})
	writesUsers := func(sent []byte) bool {
		text := strings.ToLower(string(sent))
		return (strings.Contains(text, "update") || strings.Contains(text, "insert")) && strings.Contains(text, "users") && !strings.Contains(text, "last_login_at")
	}
	hooked := proxy.OnStatement(writesUsers, nil, func() {
		close(held)
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
	})
	firstStatus := make(chan int, 1)
	go func() {
		status, _ := doRequest(t, first, "POST", savePath("path", users["U"].compact()), users.token("U", 0), "application/json", bodyA)
		firstStatus <- status
	}()
	select {
	case <-held:
	case status := <-firstStatus:
		t.Fatalf("the first save answered %d and sent no statement that writes the users table (an INSERT or UPDATE naming it)", status)
	case <-time.After(30 * time.Second):
		t.Fatal("the first save sent no statement that writes the users table (an INSERT or UPDATE naming it)")
	}
	// What is held is an executed write: its connection is idle, or idle in a transaction, and not running a
	// statement. Were the hook to hold a prepare or a statement not yet run, the connection would be active or
	// the hook would not have fired, and the test fails here instead of passing on a build it cannot catch.
	assertHeldAfterExecution(t, dsn, proxy.DSN())
	secondClient := &http.Client{Timeout: 5 * time.Second}
	request, _ := http.NewRequest("POST", second.url(savePath("query", users["U"].compact())), strings.NewReader(bodyB))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", `MediaBrowser Token="`+users.token("U", 1)+`"`)
	if response, err := secondClient.Do(request); err == nil {
		response.Body.Close()
	}
	close(release)
	if status := <-firstStatus; status != 204 {
		t.Fatalf("the first save answered %d, want 204", status)
	}
	<-hooked
	for _, r := range []*proc{first, second} {
		if got := savedConfigOf(t, r, users.token("U", 1)); !reflect.DeepEqual(got, wantA) && !reflect.DeepEqual(got, wantB) {
			t.Errorf("the Configuration on %s is a mix of the two bodies:\n%v\nbody A %v\nbody B %v", r.addr, got, wantA, wantB)
		}
	}
}

// What two bodies of one field each do is in the capture: the later one replaces everything, Jellyfin does not
// merge. The check follows the capture: were it a merge, two one-field bodies that name different fields would
// both have to take effect; as it is, two at once leave one body alone, whole.
func TestTwoOneFieldSavesAtOnceAreOneBodyOrBoth(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	users := newConfigUsers(t, dsn, "one"+uuid.NewString()[:8])
	later := capture.Case(t, "two bodies of one field each, route=path")
	merges := capture.Config(later.After.Me["U"])["SubtitleLanguagePreference"] == "ita" // The earlier body's field survived the later one.
	with := func(fields map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range capture.NewUser.Me {
			out[k] = v
		}
		for k, v := range fields {
			out[k] = v
		}
		return out
	}
	oneA, oneB := with(map[string]any{"SubtitleLanguagePreference": "ita"}), with(map[string]any{"HidePlayedInLatest": false})
	both := with(map[string]any{"SubtitleLanguagePreference": "ita", "HidePlayedInLatest": false})
	for round := 0; round < 20; round++ {
		first, second := a, b
		if round%2 == 1 {
			first, second = b, a
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		for n, one := range []struct {
			r    *proc
			body string
		}{{first, `{"SubtitleLanguagePreference":"ita"}`}, {second, `{"HidePlayedInLatest":false}`}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				if status, raw := doRequest(t, one.r, "POST", "/Users/Configuration", users.token("U", n), "application/json", one.body); status != 204 {
					t.Errorf("round %d: %d %s", round, status, raw)
				}
			}()
		}
		close(gate)
		wg.Wait()
		for _, r := range []*proc{a, b} {
			got := savedConfigOf(t, r, users.token("U", 1))
			switch {
			case merges && !reflect.DeepEqual(got, both):
				t.Fatalf("round %d: the capture merges and the two saves did not both take effect: %v", round, got)
			case !merges && !reflect.DeepEqual(got, oneA) && !reflect.DeepEqual(got, oneB):
				t.Fatalf("round %d: the capture replaces, and the result is neither body alone: %v", round, got)
			}
		}
	}
}

// assertHeldAfterExecution fails unless a session of the proxy at dsn proxied is idle or idle in transaction and
// its last statement writes the users table: Postgres has run the write the proxy is holding the answer of.
func assertHeldAfterExecution(t *testing.T, dsn, proxied string) {
	t.Helper()
	u, err := url.Parse(proxied)
	if err != nil {
		t.Fatal(err)
	}
	name := u.Query().Get("application_name")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND state IN ('idle', 'idle in transaction')
		AND query ILIKE '%users%' AND (query ILIKE '%update%' OR query ILIKE '%insert%')`, name).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("while the first save is held, no session of its proxy has run a write of the users table: the hook holds something other than an executed write")
	}
}
