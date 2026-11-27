//go:build integration

package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// JS-033, criterion 9: a disabled account's right password does the same password work as a wrong password for an
// enabled one, so the time taken does not tell that the account is disabled or that the password was right. As
// TestAWrongPasswordAndAnUnknownNameDoTheSamePasswordWork (JS-145): a duration cannot be asserted on a shared
// machine, so this holds the mechanism, in a process of its own: a bcrypt comparison allocates its cipher (about
// 4 KB) and costs CPU by its cost. The wrong build it fails: refusing a disabled account before the comparison.
func TestADisabledAccountsRightPasswordDoesTheSamePasswordWorkAsAWrongOne(t *testing.T) {
	if os.Getenv("JELLYSCALE_WORK_ALONE") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestADisabledAccountsRightPasswordDoesTheSamePasswordWorkAsAWrongOne$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "JELLYSCALE_WORK_ALONE=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in a process of its own:\n%s", out)
		if err != nil {
			t.Fatalf("the measurement failed in a process of its own: %v", err)
		}
		return
	}
	e := newEnv(t)
	mustExec(t, e.pool, "UPDATE users SET is_admin = true WHERE id = $1", e.other)
	hash, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	account := func(disabled bool) string {
		id, name := uuid.NewString(), "work-"+uuid.NewString()[:8]
		mustExec(t, e.pool, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, false)`, id, name, string(hash))
		if disabled {
			if response, body := e.do(t, 0, "POST", "/Users/"+id+"/Policy", e.otherToken, map[string]any{"IsDisabled": true}); response.StatusCode != 204 {
				t.Fatalf("disabling: %d %s", response.StatusCode, body)
			}
		}
		return name
	}
	ask := func(user, password string) (int, string) {
		response, body := e.do(t, 0, "POST", "/Users/AuthenticateByName", "", map[string]string{"Username": user, "Pw": password},
			"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="dev-`+uuid.NewString()[:8]+`", Version="1"`)
		return response.StatusCode, string(body)
	}
	cpu := func() float64 {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return float64(usage.Utime.Nano()+usage.Stime.Nano()) / 1e6
	}
	one := func(user, password string) (allocated, milliseconds float64) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := cpu()
		if status, body := ask(user, password); status != 401 {
			t.Fatalf("signing in as %q: %d %q, want 401", user, status, body)
		}
		milliseconds = cpu() - start
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc - before.TotalAlloc), milliseconds
	}
	var wrongBytes, wrongCPU, disabledBytes, disabledCPU []float64
	var enabled, disabled string
	for i := range 60 {
		if i%15 == 0 { // New accounts, so that no bound is reached however the build counts.
			enabled, disabled = account(false), account(true)
		}
		wb, wc := one(enabled, "wrong")
		ab, dc := one(disabled, "right")
		if i%15 != 0 && i >= 5 { // The first requests after the accounts are made warm the pool.
			wrongBytes, wrongCPU = append(wrongBytes, wb), append(wrongCPU, wc)
			disabledBytes, disabledCPU = append(disabledBytes, ab), append(disabledCPU, dc)
		}
	}
	wc, dc := median(wrongCPU), median(disabledCPU)
	t.Logf("a wrong password costs %.1f ms of CPU and a disabled account's right one %.1f ms (medians)", wc, dc)
	if dc < wc/2 || wc < dc/2 {
		t.Errorf("a wrong password costs %.1f ms of CPU and a disabled account's right password %.1f ms: they do not do the same password work", wc, dc)
	}
	wb, ab := median(wrongBytes), median(disabledBytes)
	if wb-ab > 3000 || ab-wb > 3000 {
		t.Errorf("a wrong password allocates %.0f B and a disabled account's right one %.0f B (medians): a comparison allocates about 4 KB", wb, ab)
	}
}

// JS-033, criterion 11. A stream replica's snapshot of playing sessions serves them while Postgres is away
// (invariant 37). A user disabled or deleted before the replica's next refresh is not in it afterwards: their byte
// ranges are sent nothing during the outage and answered 401 once Postgres is back. The third user, still enabled, is
// served from the snapshot meanwhile.
func TestInAnOutageADisabledOrDeletedUserIsNotServedFromTheSnapshot(t *testing.T) {
	p := newPlayAway(t, 3)
	_, refresh := timings(t)
	mustExec(t, p.pool, "UPDATE users SET is_admin = true WHERE id = $1", p.other)
	for viewer := range 3 {
		p.playing(t, viewer)
	}
	rp := p.streams[0]
	refreshed(refresh) // The snapshot holds all three.
	userOf := func(viewer int) string {
		return scalar[string](t, p.pool, "SELECT user_id::text FROM devices WHERE token = $1", p.tokens[viewer])
	}
	disabled, deleted := userOf(0), userOf(1)
	if response, body := p.env.do(t, 0, "POST", "/Users/"+disabled+"/Policy", p.otherToken, map[string]any{"IsDisabled": true}); response.StatusCode != 204 {
		t.Fatalf("disabling: %d %s", response.StatusCode, body)
	}
	if response, body := p.env.do(t, 1, "DELETE", "/Users/"+deleted, p.otherToken, nil); response.StatusCode != 204 {
		t.Fatalf("deleting: %d %s", response.StatusCode, body)
	}
	refreshed(refresh)
	rp.away(t, p.pool)

	answeredWithin(t, "the enabled user during the outage", rp, []served{{what: "a range", path: p.direct(p.tokens[2]), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])}})
	conns := map[string]net.Conn{
		"the disabled user": rawRequest(t, rp.server.URL, p.direct(p.tokens[0]), p.tokens[0]),
		"the deleted user":  rawRequest(t, rp.server.URL, p.direct(p.tokens[1]), p.tokens[1]),
	}
	time.Sleep(3200 * time.Millisecond)
	for who, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) //nolint:errcheck
		buffer := make([]byte, 64)
		n, err := conn.Read(buffer)
		var timeout net.Error
		if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("%s was sent %q (%v) while Postgres was away: the snapshot still serves them", who, strings.SplitN(string(buffer[:n]), "\r\n", 2)[0], err)
			delete(conns, who)
		}
	}
	rp.proxy.Back()
	for who, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second)) //nolint:errcheck
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Errorf("%s was not answered once Postgres was back: %v", who, err)
			continue
		}
		response.Body.Close()
		if response.StatusCode != 401 {
			t.Errorf("%s was answered %d once Postgres was back, want 401", who, response.StatusCode)
		}
	}
}
