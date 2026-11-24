//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/testenv"
)

// JS-145 (D-020): the kit of the sign-in checks (signin_integration_test.go and the files beside it). Real api
// processes over the one test Postgres; the state the bounds keep is in tables named signin_*, which these tests
// read only as a whole (every timestamptz column moved, every row counted, every row deleted), so that they name no
// column of the build's.

const (
	rightPassword = "right-password"
	invalidBody   = "invalid username or password\n"
)

// signInSite is api replicas as separate processes, each with a metrics listener.
type signInSite struct {
	t       *testing.T
	dsn     string
	pool    *pgxpool.Pool
	extra   map[string]any
	via     string // The database URL the replicas use, when it is not the test's own (a DBProxy).
	apis    []*proc
	metrics map[*proc]string
}

// newSignInSite starts `replicas` api processes with the config keys in extra (the key trustedProxies is absent unless
// extra has it, which is the state in which the per-address bound is off). Every signin_* row is deleted before and after.
func newSignInSite(t *testing.T, replicas int, extra map[string]any) *signInSite {
	t.Helper()
	return newSignInSiteVia(t, replicas, extra, "")
}

// newSignInSiteVia is newSignInSite with the replicas on the database URL via (a proxy's), when it is not "".
func newSignInSiteVia(t *testing.T, replicas int, extra map[string]any, via string) *signInSite {
	t.Helper()
	dsn := testDatabaseURL(t)
	s := &signInSite{t: t, dsn: dsn, via: via, pool: connect(t, dsn), extra: extra, metrics: map[*proc]string{}}
	s.clear()
	t.Cleanup(s.clear)
	for i := 0; i < replicas; i++ {
		s.apis = append(s.apis, s.startReplica())
	}
	return s
}

func (s *signInSite) startReplica() *proc {
	dsn := s.dsn
	if s.via != "" {
		dsn = s.via
	}
	config := map[string]any{"name": "failover", "adminUser": adminUser}
	for k, v := range s.extra {
		config[k] = v
	}
	listen := freeAddr(s.t)
	config["metricsListen"] = listen
	raw, err := json.Marshal(config)
	if err != nil {
		s.t.Fatal(err)
	}
	p := startProc(s.t, procSpec{bin: binary(s.t), dsn: dsn, args: []string{"--roles=api"}, config: string(raw), listen: true, user: adminUser, password: adminPassword})
	// until starts the replica again on a new address when its own was taken, which includes the metrics address chosen
	// just before it: freeAddr gives no guarantee that two calls differ.
	p.until(60*time.Second, "the api replica to answer /healthz", func() bool { return p.status2xx("/healthz") })
	s.metrics[p] = listen
	return p
}

// restart kills replica i (no goodbye) and starts another process in its place.
func (s *signInSite) restart(i int) {
	s.t.Helper()
	s.apis[i].kill()
	s.apis[i] = s.startReplica()
}

func (s *signInSite) tables() []string {
	rows, err := s.pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name LIKE 'signin\_%' ESCAPE '\'`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

// clear deletes every row the bounds keep.
func (s *signInSite) clear() {
	for _, table := range s.tables() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM "`+table+`"`); err != nil {
			s.t.Error(err)
		}
	}
}

// shift moves every time the bounds hold back by d, which is how a test lets time pass: the build keeps its times in
// Postgres and compares them with now(), and no process keeps a copy.
func (s *signInSite) shift(d time.Duration) {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name LIKE 'signin\_%' ESCAPE '\' AND data_type = 'timestamp with time zone'`)
	if err != nil {
		s.t.Fatal(err)
	}
	var cols [][2]string
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			s.t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	for _, c := range cols {
		if _, err := s.pool.Exec(context.Background(), fmt.Sprintf(`UPDATE "%s" SET "%s" = "%s" - $1 * interval '1 second'`, c[0], c[1], c[1]), d.Seconds()); err != nil {
			s.t.Fatal(err)
		}
	}
}

// rowCount is the rows of every signin_* table, together.
func (s *signInSite) rowCount() int {
	n := 0
	for _, table := range s.tables() {
		var c int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&c); err != nil {
			s.t.Fatal(err)
		}
		n += c
	}
	return n
}

// dump is every row of every signin_* table as text, for a test that looks for what must not be there.
func (s *signInSite) dump() string {
	var out strings.Builder
	for _, table := range s.tables() {
		rows, err := s.pool.Query(context.Background(), `SELECT row_to_json(x)::text FROM "`+table+`" x`)
		if err != nil {
			s.t.Fatal(err)
		}
		for rows.Next() {
			var line string
			rows.Scan(&line) //nolint:errcheck
			out.WriteString(line + "\n")
		}
		rows.Close()
	}
	return out.String()
}

// account makes a real account with a name no other test uses.
func (s *signInSite) account(prefix string) string {
	s.t.Helper()
	name := prefix + "-" + uuid.NewString()[:8]
	hash, err := bcrypt.GenerateFromPassword([]byte(rightPassword), bcrypt.MinCost)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, false)`,
		uuid.NewString(), name, string(hash)); err != nil {
		s.t.Fatal(err)
	}
	return name
}

// invented is a name that is no account.
func invented() string { return "nobody-" + uuid.NewString()[:8] }

// try is one sign-in attempt on replica i (round-robin over i), from a device new to everything unless it is named.
func (s *signInSite) try(replica int, device, name, password string, headers ...string) signInReply {
	s.t.Helper()
	got, err := s.apis[replica%len(s.apis)].signInReply(device, name, password, headers...)
	if err != nil {
		s.t.Fatal(err)
	}
	return got
}

func newDevice() string { return "dev-" + uuid.NewString()[:8] }

// wrong is one wrong password from a device new to the account.
func (s *signInSite) wrong(replica int, name string, headers ...string) signInReply {
	s.t.Helper()
	return s.try(replica, newDevice(), name, "wrong-"+uuid.NewString()[:6], headers...)
}

// failN makes n wrong passwords from new devices, each of which must answer 401, and fails the test otherwise.
func (s *signInSite) failN(name string, n int, headers ...string) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		if got := s.wrong(i, name, headers...); got.status != 401 {
			s.t.Fatalf("wrong password %d of %d for %q: %d %q, want 401", i+1, n, name, got.status, got.body)
		}
	}
}

// refused says what the answer must be for an attempt the bounds refuse: 403 with Retry-After in [min, max] whole seconds.
func refusedWithin(t *testing.T, what string, got signInReply, min, max int) {
	t.Helper()
	if got.status != 403 {
		t.Errorf("%s: %d %q, want 403", what, got.status, got.body)
		return
	}
	value := got.header.Get("Retry-After")
	seconds, err := strconv.Atoi(value)
	if err != nil || strconv.Itoa(seconds) != value {
		t.Errorf("%s: Retry-After %q, want whole seconds", what, value)
		return
	}
	if seconds < min || seconds > max {
		t.Errorf("%s: Retry-After %d, want %d to %d", what, seconds, min, max)
	}
}

// signedIn checks the answer is a sign-in and returns its token.
func signedIn(t *testing.T, what string, got signInReply) string {
	t.Helper()
	var reply struct{ AccessToken string }
	if got.status != 200 || json.Unmarshal(got.body, &reply) != nil || reply.AccessToken == "" {
		t.Fatalf("%s: %d %q, want a sign-in", what, got.status, got.body)
	}
	return reply.AccessToken
}

// parallel runs n attempts at once (each is fn(i)) and returns the statuses counted. A transport error is counted under 0.
func parallel(n int, fn func(i int) signInReply) map[int]int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got := fn(i)
			mu.Lock()
			counts[got.status]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return counts
}

// quiet makes an attempt from goroutines: a transport error is a zero-status answer.
func (s *signInSite) quiet(replica int, device, name, password string, headers ...string) signInReply {
	got, err := s.apis[replica%len(s.apis)].signInReply(device, name, password, headers...)
	if err != nil {
		return signInReply{}
	}
	return got
}

// --- the process's own answers -------------------------------------------------------------------------------

// get is a GET with the token, on replica i.
func (s *signInSite) get(replica int, path, token string) (int, []byte) {
	s.t.Helper()
	request, _ := http.NewRequest("GET", s.apis[replica%len(s.apis)].url(path), nil)
	request.Header.Set("X-Emby-Token", token)
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

// scrape reads a process's /metrics into "name{labels}" -> value.
func (s *signInSite) scrape(p *proc) map[string]float64 {
	s.t.Helper()
	response, err := http.Get("http://" + s.metrics[p] + "/metrics")
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, " ") {
			continue
		}
		cut := strings.LastIndex(line, " ")
		if v, err := strconv.ParseFloat(line[cut+1:], 64); err == nil {
			out[line[:cut]] = v
		}
	}
	return out
}

// total is one series summed over the replicas that are running.
func (s *signInSite) total(series string) float64 {
	sum := 0.0
	for _, p := range s.apis {
		sum += s.scrape(p)[series]
	}
	return sum
}

const (
	failuresMetric = "jellyscale_signin_failures_total"
	nameRefusals   = `jellyscale_signin_refusals_total{bound="name"}`
	deviceRefusals = `jellyscale_signin_refusals_total{bound="device"}`
	addrRefusals   = `jellyscale_signin_refusals_total{bound="address"}`
)

var retryAfterSecond = regexp.MustCompile(`^[0-9]+$`)

// randomV4 is an address of the documentation-adjacent benchmarking range, new each time.
func randomV4() string {
	return fmt.Sprintf("198.18.%d.%d", rand.IntN(250)+1, rand.IntN(250)+1)
}

// randomNet64 is a /64 of the documentation range, new each time.
func randomNet64() string {
	return fmt.Sprintf("2001:db8:%x:%x", rand.IntN(0xffff)+1, rand.IntN(0xffff)+1)
}

// behindProxy is the headers of a request that the declared proxy 127.0.0.1 (the test's own address) forwards for addr.
func behindProxy(addr string) []string { return []string{"X-Forwarded-For", addr} }

func testDatabaseURL(t *testing.T) string { return testenv.DatabaseURL(t) }

// logout ends the token on replica i.
func (s *signInSite) logout(replica int, token string) {
	s.t.Helper()
	request, _ := http.NewRequest("POST", s.apis[replica%len(s.apis)].url("/Sessions/Logout"), nil)
	request.Header.Set("X-Emby-Token", token)
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		s.t.Fatalf("logout: %d", response.StatusCode)
	}
}

func atoi(t *testing.T, value string) int {
	t.Helper()
	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%q is not a number", value)
	}
	return n
}

// failSpread is n wrong passwords for the name from n addresses (declared proxies) and devices, each answered 401.
func (s *signInSite) failSpread(name string, n int) {
	s.t.Helper()
	for i := 0; i < n; i++ {
		if got := s.wrong(i, name, behindProxy(randomV4())...); got.status != 401 {
			s.t.Fatalf("wrong password %d of %d for %q: %d %q, want 401", i+1, n, name, got.status, got.body)
		}
	}
}
