//go:build integration

package api

import (
	"bufio"
	"context"
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
// enabled one, so the time taken does not tell that the account is disabled or that the password was right. The wrong
// build it fails: refusing a disabled account before the comparison.
func TestADisabledAccountsRightPasswordDoesTheSamePasswordWorkAsAWrongOne(t *testing.T) {
	if measuredElsewhere(t) {
		return
	}
	w := newPasswordWork(t)
	w.sameAsAWrongPassword(t, "a disabled account's right password", "right", func() string {
		id, name := w.account()
		if response, body := w.e.do(t, 0, "POST", "/Users/"+id+"/Policy", w.e.otherToken, map[string]any{"IsDisabled": true}); response.StatusCode != 204 {
			t.Fatalf("disabling: %d %s", response.StatusCode, body)
		}
		return name
	})
}

// D-105: a sign-in to an account with no password, with the empty password or any other, does the same password work as
// a wrong password for an account that has one. The wrong builds it fails: refusing before the comparison, or comparing
// with a stored value that is no hash, or with a cheaper one.
func TestAPasswordlessAccountsSignInDoesTheSamePasswordWorkAsAWrongOne(t *testing.T) {
	if measuredElsewhere(t) {
		return
	}
	w := newPasswordWork(t)
	for what, password := range map[string]string{"the empty password for an account with none": "", "a password for an account with none": "something"} {
		w.sameAsAWrongPassword(t, what, password, func() string {
			name := "bare-" + uuid.NewString()[:8]
			if response, body := w.e.do(t, 0, "POST", "/Users/New", w.e.otherToken, map[string]any{"Name": name, "Password": ""}); response.StatusCode != 200 {
				t.Fatalf("making a user with no password: %d %s", response.StatusCode, body)
			}
			return name
		})
	}
}

// measuredElsewhere runs the test in a process of its own, where nothing else allocates or uses CPU, and says whether
// this is the process that asked for it.
func measuredElsewhere(t *testing.T) bool {
	t.Helper()
	if os.Getenv("JELLYSCALE_WORK_ALONE") != "" {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "JELLYSCALE_WORK_ALONE=1")
	out, err := cmd.CombinedOutput()
	t.Logf("in a process of its own:\n%s", out)
	if err != nil {
		t.Fatalf("the measurement failed in a process of its own: %v", err)
	}
	return true
}

type passwordWork struct {
	e    *env
	hash string
}

// newPasswordWork is an env whose other user is an administrator, and the hash of "right" that its accounts hold.
func newPasswordWork(t *testing.T) passwordWork {
	t.Helper()
	e := newEnv(t)
	mustExec(t, e.pool, "UPDATE users SET is_admin = true WHERE id = $1", e.other)
	hash, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	return passwordWork{e, string(hash)}
}

func (w passwordWork) account() (id, name string) {
	id, name = uuid.NewString(), "work-"+uuid.NewString()[:8]
	if _, err := w.e.pool.Exec(context.Background(), `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, false)`, id, name, w.hash); err != nil {
		panic(err)
	}
	return id, name
}

// sameAsAWrongPassword holds a refused sign-in (the password, for the accounts the refused side makes) to the work of a
// wrong password. As TestAWrongPasswordAndAnUnknownNameDoTheSamePasswordWork (JS-145): a duration cannot be asserted on
// a shared machine, so this holds the mechanism: a bcrypt comparison allocates its cipher (about 4 KB) and costs CPU by
// its cost.
func (w passwordWork) sameAsAWrongPassword(t *testing.T, what, password string, refusedAccount func() string) {
	t.Helper()
	ask := func(user, password string) (int, string) {
		response, body := w.e.do(t, 0, "POST", "/Users/AuthenticateByName", "", map[string]string{"Username": user, "Pw": password},
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
	var wrongBytes, wrongCPU, refusedBytes, refusedCPU []float64
	var enabled, refused string
	for i := range 60 {
		if i%15 == 0 { // New accounts, so that no bound is reached however the build counts.
			_, enabled = w.account()
			refused = refusedAccount()
		}
		wb, wc := one(enabled, "wrong")
		rb, rc := one(refused, password)
		if i%15 != 0 && i >= 5 { // The first requests after the accounts are made warm the pool.
			wrongBytes, wrongCPU = append(wrongBytes, wb), append(wrongCPU, wc)
			refusedBytes, refusedCPU = append(refusedBytes, rb), append(refusedCPU, rc)
		}
	}
	wc, rc := median(wrongCPU), median(refusedCPU)
	t.Logf("a wrong password costs %.1f ms of CPU and %s %.1f ms (medians)", wc, what, rc)
	if rc < wc/2 || wc < rc/2 {
		t.Errorf("a wrong password costs %.1f ms of CPU and %s %.1f ms: they do not do the same password work", wc, what, rc)
	}
	wb, rb := median(wrongBytes), median(refusedBytes)
	if wb-rb > 3000 || rb-wb > 3000 {
		t.Errorf("a wrong password allocates %.0f B and %s %.0f B (medians): a comparison allocates about 4 KB", wb, what, rb)
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
