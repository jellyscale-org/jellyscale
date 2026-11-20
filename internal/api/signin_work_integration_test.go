//go:build integration

package api

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// JS-145 (D-020): before any bound, a wrong password for a real account and any password for an unknown name answer alike
// and do the same password work: one bcrypt comparison each, so the time taken does not tell them apart (Jellyfin 12.4
// was observed at 1 ms against 77 ms: compat/captures/signin-12.4.json). A duration cannot be asserted on a shared
// machine, so this holds the mechanism: a bcrypt comparison allocates the cipher it sets up (about 4 KB, whatever its
// cost), so a request that makes no comparison allocates that much less than one that makes one. The cost of the
// comparison is held by TestTheUnknownNamesDummyHashCostsWhatAnAccountsHashCosts (signin_test.go).
//
// Against a build with the unknown name's comparison removed (the one-line mutant: delete the CompareHashAndPassword
// call under errors.Is(err, pgx.ErrNoRows)) this fails; against today's tree it passes, which is the point of a check
// for what this task must keep (counting, the known-device lookup and the log must not make the two differ).
func TestAWrongPasswordAndAnUnknownNameDoTheSamePasswordWork(t *testing.T) {
	// Allocation is counted for the whole process, so goroutines left by earlier tests in it would count too: the
	// measurement runs in a process of its own (lead's correction: it failed after other tests on the base tree too).
	if os.Getenv("JELLYSCALE_WORK_ALONE") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAWrongPasswordAndAnUnknownNameDoTheSamePasswordWork$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "JELLYSCALE_WORK_ALONE=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in a process of its own:\n%s", out)
		if err != nil {
			t.Fatalf("the measurement failed in a process of its own: %v", err)
		}
		return
	}
	e := newEnv(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("right"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}

	ask := func(user, device string) (int, string) {
		response, body := e.do(t, 0, "POST", "/Users/AuthenticateByName", "", map[string]string{"Username": user, "Pw": "wrong"},
			"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="`+device+`", Version="1"`)
		return response.StatusCode, string(body)
	}
	// A new account with the same hash for each round, so that no account fails more than the bound of 20 per name
	// (handler tests leave trustedProxies absent, so the per-name bound holds) however the build counts. Inserting
	// copies of one hash costs no bcrypt run.
	newReal := func() string {
		n := "work-" + uuid.NewString()[:8]
		mustExec(t, e.pool, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, false)`, uuid.NewString(), n, string(hash))
		return n
	}
	// What one request allocates. The two sides are asked alternately and judged by their medians, so that drift in the
	// pool and the allocations of background goroutines fall on both sides alike and a stray one moves no median.
	// And the CPU it costs the process: a comparison against a cheaper hash allocates the same but costs a fraction
	// of the CPU (bcrypt doubles its work per cost step), so this holds that the unknown name's comparison is not
	// cheapened (lead's correction after Verify: a MinCost hash for unknown names passed the allocation check).
	cpu := func() float64 {
		var u syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
			t.Fatal(err)
		}
		return float64(u.Utime.Nano()+u.Stime.Nano()) / 1e6
	}
	var lastCPU float64
	one := func(user string) float64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		startCPU := cpu()
		// A new device each time, and the same 401.
		if status, _ := ask(user, "dev-"+uuid.NewString()[:8]); status != 401 {
			t.Fatalf("a wrong password for %q: %d, want 401", user, status)
		}
		lastCPU = cpu() - startCPU
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc - before.TotalAlloc)
	}
	// Alike in status and body.
	sReal, bReal := ask(newReal(), "dev-a")
	sFake, bFake := ask("nobody-"+uuid.NewString()[:8], "dev-b")
	if sReal != sFake || strings.TrimSpace(bReal) != strings.TrimSpace(bFake) {
		t.Fatalf("a wrong password: %d %q; an unknown name: %d %q; want the same answer", sReal, bReal, sFake, bFake)
	}
	var reals, fakes, realCPU, fakeCPU []float64
	account, failures := newReal(), 0
	for i := 0; i < 60; i++ {
		if failures == 15 {
			account, failures = newReal(), 0
		}
		failures++
		r := one(account)
		rc := lastCPU
		f := one("nobody-" + uuid.NewString()[:8])
		fc := lastCPU
		if i >= 5 { // the first requests warm the pool
			reals, fakes = append(reals, r), append(fakes, f)
			realCPU, fakeCPU = append(realCPU, rc), append(fakeCPU, fc)
		}
	}
	rc, fc := median(realCPU), median(fakeCPU)
	t.Logf("a wrong password for a real account costs %.1f ms of CPU, an unknown name %.1f ms (medians)", rc, fc)
	// A ratio of the two sides, measured alike in one process, not a time: one cost step less halves the work.
	if fc < rc/2 {
		t.Errorf("an unknown name costs %.1f ms of CPU per request and a wrong password for a real account %.1f ms: the unknown name's comparison is cheaper", fc, rc)
	}
	real, fake := median(reals), median(fakes)
	t.Logf("a wrong password for a real account allocates %.0f B, an unknown name %.0f B (medians)", real, fake)
	if real-fake > 3000 || fake-real > 3000 {
		t.Errorf("a wrong password for a real account allocates %.0f B per request and an unknown name %.0f B: a comparison allocates about 4 KB, so one of them makes none, or they make a different number", real, fake)
	}
}

func median(v []float64) float64 {
	sort.Float64s(v)
	return v[len(v)/2]
}
