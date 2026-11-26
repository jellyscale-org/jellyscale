//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// JS-037: the kit of the Quick Connect checks (quickconnect_integration_test.go and the files beside it). Real api
// processes over the one test Postgres. Two things these tests ask of the build's storage, and nothing else:
//   - the tables Quick Connect keeps its state in are named quickconnect_* or quick_connect_* (as the sign-in bounds'
//     are signin_*), so that the tests can empty them and hold them (a migration seeds no row in them that emptying
//     them would take away);
//   - every time it keeps in them is a timestamptz column, which is how a test lets time pass (it moves every such
//     column, and every one of signin_*, back), as signInSite.shift does. A build that keeps a time in a process, or as
//     a number, fails the expiry and bound checks.
// No test names a column.

type qcSite struct {
	*signInSite
	names []string // Each running replica's Postgres application_name, in the order of apis.
}

// newQCSite starts `replicas` api processes with the config keys in extra, each under an application_name of its own.
// Every Quick Connect and sign-in row is deleted before and after.
func newQCSite(t *testing.T, replicas int, extra map[string]any) *qcSite {
	t.Helper()
	dsn := testDatabaseURL(t)
	q := &qcSite{signInSite: &signInSite{t: t, dsn: dsn, pool: connect(t, dsn), extra: extra, metrics: map[*proc]string{}}}
	t.Cleanup(q.empty) // Registered first, so it runs after the processes have stopped.
	for range replicas {
		q.add(extra)
	}
	// The first replica has migrated the database by now, so the tables are there to empty.
	q.empty()
	return q
}

// add starts one more replica with the config keys in extra and returns its index.
func (q *qcSite) add(extra map[string]any) int {
	q.t.Helper()
	name := "qc-" + uuid.NewString()[:8]
	address, err := url.Parse(q.dsn)
	if err != nil {
		q.t.Fatal(err)
	}
	query := address.Query()
	query.Set("application_name", name)
	address.RawQuery = query.Encode()
	q.via, q.extra = address.String(), extra
	q.apis = append(q.apis, q.startReplica())
	q.names = append(q.names, name)
	return len(q.apis) - 1
}

// quickTables are the tables the build keeps Quick Connect's state in.
func (q *qcSite) quickTables() []string {
	q.t.Helper()
	rows, err := q.pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name ~ '^quick_?connect'`)
	if err != nil {
		q.t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		q.t.Fatal(err)
	}
	return names
}

func (q *qcSite) empty() {
	q.signInSite.clear()
	for _, table := range q.quickTables() {
		if _, err := q.pool.Exec(context.Background(), `DELETE FROM "`+table+`"`); err != nil {
			// A table another refers to is emptied with it.
			if _, err := q.pool.Exec(context.Background(), `TRUNCATE "`+table+`" CASCADE`); err != nil {
				q.t.Error(err)
			}
		}
	}
}

// pass lets d go by: every time Quick Connect and the sign-in bounds hold is moved back by d.
func (q *qcSite) pass(d time.Duration) {
	q.t.Helper()
	rows, err := q.pool.Query(context.Background(), `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name ~ '^quick_?connect' AND data_type = 'timestamp with time zone'`)
	if err != nil {
		q.t.Fatal(err)
	}
	moves := map[string][]string{} // A table's times, moved in one statement so that a CHECK between two of them holds.
	var table, name string
	if _, err := pgx.ForEachRow(rows, []any{&table, &name}, func() error {
		moves[table] = append(moves[table], `"`+name+`" = "`+name+`" - $1 * interval '1 second'`)
		return nil
	}); err != nil {
		q.t.Fatal(err)
	}
	if len(moves) == 0 {
		q.t.Fatal("no timestamptz column in a table named quickconnect_* or quick_connect_*: see the kit's comment")
	}
	for table, columns := range moves {
		if _, err := q.pool.Exec(context.Background(), `UPDATE "`+table+`" SET `+strings.Join(columns, ", "), d.Seconds()); err != nil {
			q.t.Fatal(err)
		}
	}
	q.signInSite.shift(d)
}

// qcDevice is a client as it names itself in the Authorization header.
type qcDevice struct{ id, name, app, version string }

func newTV() qcDevice {
	id := uuid.NewString()[:8]
	return qcDevice{id: "tv-" + id, name: "Television " + id, app: "tv-app", version: "9." + id[:1]}
}

func (d qcDevice) header(token string) string {
	value := `MediaBrowser Client="` + d.app + `", Device="` + d.name + `", DeviceId="` + d.id + `", Version="` + d.version + `"`
	if token != "" {
		value += `, Token="` + token + `"`
	}
	return value
}

// call is one request to replica i. headers are name, value pairs. A transport error is a zero status.
func (q *qcSite) call(i int, method, path, body string, headers ...string) signInReply {
	request, err := http.NewRequest(method, q.apis[i%len(q.apis)].url(path), bytes.NewReader([]byte(body)))
	if err != nil {
		return signInReply{body: []byte(err.Error())}
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for n := 0; n+1 < len(headers); n += 2 {
		request.Header.Set(headers[n], headers[n+1])
	}
	response, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return signInReply{body: []byte(err.Error())}
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return signInReply{response.StatusCode, response.Header, raw}
}

// qcCode is what Initiate gave a device.
type qcCode struct {
	Code, Secret string
	device       qcDevice
}

func (q *qcSite) initiate(i int, device qcDevice, headers ...string) signInReply {
	return q.call(i, "POST", "/QuickConnect/Initiate", "", append([]string{"Authorization", device.header("")}, headers...)...)
}

// start is an Initiate that must succeed.
func (q *qcSite) start(i int, device qcDevice, headers ...string) qcCode {
	q.t.Helper()
	got := q.initiate(i, device, headers...)
	code := qcCode{device: device}
	if got.status != 200 || json.Unmarshal(got.body, &code) != nil || code.Code == "" || code.Secret == "" {
		q.t.Fatalf("Initiate: %d %q, want 200 with a Code and a Secret", got.status, got.body)
	}
	return code
}

func (q *qcSite) connectSecret(i int, secret string) signInReply {
	return q.call(i, "GET", "/QuickConnect/Connect?Secret="+url.QueryEscape(secret), "", "Authorization", newTV().header(""))
}

// authenticated is what Connect says of the code: its status, and Authenticated when it is 200.
func (q *qcSite) authenticated(i int, code qcCode) (int, bool) {
	got := q.connectSecret(i, code.Secret)
	var state struct{ Authenticated bool }
	if got.status == 200 && json.Unmarshal(got.body, &state) != nil {
		q.t.Fatalf("Connect: %q is no JSON", got.body)
	}
	return got.status, state.Authenticated
}

func (q *qcSite) mustBePending(i int, code qcCode, what string) {
	q.t.Helper()
	if status, approved := q.authenticated(i, code); status != 200 || approved {
		q.t.Fatalf("%s: Connect answers %d Authenticated %v, want the code pending", what, status, approved)
	}
}

// authorize is an approval of the code (as typed) with the token; query is what follows the code, such as "&userId=x".
func (q *qcSite) authorize(i int, token, code, query string) signInReply {
	return q.call(i, "POST", "/QuickConnect/Authorize?code="+url.QueryEscape(code)+query, "", "Authorization", approverDevice(token).header(token))
}

func approverDevice(token string) qcDevice {
	return qcDevice{id: "phone-" + token[:min(len(token), 12)], name: "Phone", app: "web", version: "1"}
}

// approve is an approval that must succeed.
func (q *qcSite) approve(i int, who qcAccount, code qcCode) {
	q.t.Helper()
	if got := q.authorize(i, who.token, code.Code, ""); got.status != 200 || strings.TrimSpace(string(got.body)) != "true" {
		q.t.Fatalf("%s approving a pending code: %d %q, want 200 true", who.name, got.status, got.body)
	}
}

func (q *qcSite) redeem(i int, device qcDevice, secret string) signInReply {
	body, _ := json.Marshal(map[string]string{"Secret": secret})
	return q.call(i, "POST", "/Users/AuthenticateWithQuickConnect", string(body), "Authorization", device.header(""))
}

// qcSignIn is the answer of a sign-in.
type qcSignIn struct {
	AccessToken string
	User        struct{ Id, Name string }
	SessionInfo struct{ DeviceId, DeviceName, Client, ApplicationVersion, UserName string }
}

// redeemed is a redemption that must give a token.
func (q *qcSite) redeemed(i int, code qcCode) qcSignIn {
	q.t.Helper()
	got := q.redeem(i, code.device, code.Secret)
	var signed qcSignIn
	if got.status != 200 || json.Unmarshal(got.body, &signed) != nil || signed.AccessToken == "" {
		q.t.Fatalf("redeeming an approved code: %d %q, want a sign-in", got.status, got.body)
	}
	return signed
}

func (q *qcSite) me(i int, token string) int {
	return q.call(i, "GET", "/Users/Me", "", "X-Emby-Token", token).status
}

type qcAccount struct {
	name, id, token string
	admin           bool
}

// member is a new account, signed in by password from a device of its own. headers go with that sign-in.
func (q *qcSite) member(admin bool, headers ...string) qcAccount {
	q.t.Helper()
	who := qcAccount{name: "qc-" + uuid.NewString()[:8], id: uuid.NewString(), admin: admin}
	hash, err := bcrypt.GenerateFromPassword([]byte(rightPassword), bcrypt.MinCost)
	if err != nil {
		q.t.Fatal(err)
	}
	if _, err := q.pool.Exec(context.Background(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, $4)`,
		who.id, who.name, string(hash), admin); err != nil {
		q.t.Fatal(err)
	}
	who.token = signedIn(q.t, "the account's password sign-in", q.try(0, newDevice(), who.name, rightPassword, headers...))
	return who
}

// deviceTokens is how many tokens the database holds for the DeviceId and the account. A token nobody was told of is
// live all the same, so "never two live tokens" is read here as well as asked.
func (q *qcSite) deviceTokens(device qcDevice, who qcAccount) int {
	q.t.Helper()
	var n int
	if err := q.pool.QueryRow(context.Background(), `SELECT count(*) FROM devices WHERE device_id = $1 AND user_id = $2`, device.id, who.id).Scan(&n); err != nil {
		q.t.Fatal(err)
	}
	return n
}

// together makes the requests meet inside Postgres: it holds every Quick Connect table in EXCLUSIVE mode (plain reads
// go on; writes and row locks wait), sends the requests to the replicas named, waits until each request has either
// answered (a redemption of a pending code need not write) or waits on a lock, and that this has stopped changing, then
// lets go. A build that reads a code's state and writes it later without a lock or a condition has then read the same
// state in each request. The settling wait only decides how many requests are held at once, never whether a right build
// passes: every state it waits for ends when this function lets go.
func (q *qcSite) together(replicas []int, requests ...func() signInReply) []signInReply {
	q.t.Helper()
	ctx := context.Background()
	tables := q.quickTables()
	if len(tables) == 0 {
		q.t.Fatal("no table named quickconnect_* or quick_connect_*: see the kit's comment")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		q.t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // The commit below has ended it on the way out.
	if _, err := tx.Exec(ctx, `LOCK TABLE "`+strings.Join(tables, `", "`)+`" IN EXCLUSIVE MODE`); err != nil {
		q.t.Fatal(err)
	}
	replies := make([]signInReply, len(requests))
	var wg sync.WaitGroup
	var answered atomic.Int64
	for n, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[n] = request()
			answered.Add(1)
		}()
	}
	var apps []string
	for _, i := range replicas {
		apps = append(apps, q.names[i])
	}
	var waiting, settled int
	var done int64
	deadline := time.Now().Add(30 * time.Second)
	for settled < 20 {
		var held int
		if err := q.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = ANY($1) AND wait_event_type = 'Lock'`, apps).Scan(&held); err != nil {
			q.t.Fatal(err)
		}
		if now := answered.Load(); int(now)+held >= len(requests) && held == waiting && now == done {
			settled++
		} else {
			waiting, done, settled = held, now, 0
		}
		if time.Now().After(deadline) {
			q.t.Fatalf("%d of the %d requests came to wait in Postgres on the held Quick Connect tables and %d answered; the others wait somewhere else", held, len(requests), answered.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		q.t.Fatal(err)
	}
	wg.Wait()
	return replies
}

func statuses(replies []signInReply) string {
	var out []string
	for _, reply := range replies {
		raw, _ := json.Marshal(reply.status)
		out = append(out, string(raw))
	}
	return strings.Join(out, " ")
}

// series reads every series of the replicas' metrics, summed.
func (q *qcSite) series() map[string]float64 {
	sum := map[string]float64{}
	for _, p := range q.apis {
		for series, value := range q.scrape(p) {
			sum[series] += value
		}
	}
	return sum
}

var boundOrCapLabel = regexp.MustCompile(`^[a-z0-9_]+\{(bound|cap)="[a-z0-9_-]+"\}$`)

// wantRefusalsCounted holds the metrics to n refusals since before: one series labelled by bound or cap and nothing else
// went up by n, and no other such series moved.
func (q *qcSite) wantRefusalsCounted(before map[string]float64, n int, what string) {
	q.t.Helper()
	grown := map[string]float64{}
	for series, value := range q.series() {
		if delta := value - before[series]; delta != 0 && boundOrCapLabel.MatchString(series) {
			grown[series] = delta
		}
	}
	for _, delta := range grown {
		if len(grown) == 1 && delta == float64(n) {
			return
		}
	}
	q.t.Errorf("%s: want one metric labelled by bound or cap alone to count the %d refusals; those that grew: %v", what, n, grown)
}

// refusalLines are the log records, of every replica, that name a bound or a cap and have every one of words in a
// field.
func (q *qcSite) refusalLines(words ...string) int {
	n := 0
	for _, p := range q.apis {
	records:
		for _, record := range p.log.records() {
			_, bound := record["bound"]
			_, capped := record["cap"]
			if !bound && !capped {
				continue
			}
			raw, _ := json.Marshal(record)
			for _, word := range words {
				if !strings.Contains(string(raw), word) {
					continue records
				}
			}
			n++
		}
	}
	return n
}

// wantNoSecretLogged fails if any replica's log holds any of the values.
func (q *qcSite) wantNoSecretLogged(values ...string) {
	q.t.Helper()
	for i, p := range q.apis {
		text := p.log.String()
		for _, value := range values {
			if value != "" && strings.Contains(text, value) {
				q.t.Errorf("replica %d logs a secret or a token (%s...)", i, value[:min(8, len(value))])
			}
		}
	}
}
