package transcode

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// JS-084's checks that need no Postgres: worker liveness is one state machine
// whose transitions are decided in one place, its timings come from one table
// of the lease, and nothing else in the liveness code writes a duration.
//
// These checks name what the rewrite is to introduce. Until it exists they do
// not compile, which is how they fail today:
//
//	liveState     the states: stRegistering, stLocked, stRelocking, stRowGone, stStopped,
//	              with String() giving "registering", "locked", "relocking", "row gone", "stopped"
//	liveEvent     the events: evRegistered, evLockEnded, evLockRetaken (the lock is held
//	              again, its row not yet looked for), evLockRetakenRowFound, evRowGone,
//	              evOwnLockMissing (own lock absent from pg_locks while the connection stands),
//	              evReadyToRegister (no job of the old session runs and a new lock is held),
//	              evShutdown, evLookFailed (Postgres did not answer within the bound);
//	              String() gives each a short name
//	livenessTransitions  []liveTransition{From liveState; Event liveEvent; To liveState}, the one
//	              place the next state is decided
//	liveNext(state, event) liveState  consults livenessTransitions; a pair with no row stays put
//	liveState.allows(liveAction)  what a state may do: actClaim, actForget, actRefreshSeenAt,
//	              actReadOwnRow, actRetryLock
//	livenessTable []livenessTiming{Name, Derivation string; Of func(lease time.Duration) time.Duration}
//	defaultLease  the 10 s the table is written for
//	liveTiming(name) time.Duration  the table's row of that name at the default lease

var allLiveStates = []liveState{stRegistering, stLocked, stRelocking, stRowGone, stStopped}
var allLiveEvents = []liveEvent{evRegistered, evLockEnded, evLockRetaken, evLockRetakenRowFound, evRowGone,
	evOwnLockMissing, evReadyToRegister, evShutdown, evLookFailed}

// The transitions of today's keepRow and Present, and no others.
var todaysTransitions = []liveTransition{
	{stRegistering, evRegistered, stLocked},
	{stLocked, evLockEnded, stRelocking},
	{stRelocking, evLockRetakenRowFound, stLocked},
	{stLocked, evRowGone, stRowGone},
	{stRelocking, evRowGone, stRowGone},
	{stLocked, evOwnLockMissing, stRowGone},
	{stRowGone, evReadyToRegister, stRegistering},
	{stRegistering, evShutdown, stStopped},
	{stLocked, evShutdown, stStopped},
	{stRelocking, evShutdown, stStopped},
	{stRowGone, evShutdown, stStopped},
	{stStopped, evShutdown, stStopped},
}

func TestLivenessNextStateIsExactlyTodaysTransitions(t *testing.T) {
	for _, s := range allLiveStates {
		for _, e := range allLiveEvents {
			want := s // No transition: the state stays.
			for _, tr := range todaysTransitions {
				if tr.From == s && tr.Event == e {
					want = tr.To
				}
			}
			if got := liveNext(s, e); got != want {
				t.Errorf("%v on %v goes to %v, want %v", s, e, got, want)
			}
		}
	}
}

// A look or a registration Postgres does not answer is no event in any state.
func TestAnUnansweredLookChangesNoState(t *testing.T) {
	for _, s := range allLiveStates {
		if got := liveNext(s, evLookFailed); got != s {
			t.Errorf("%v on an unanswered look goes to %v", s, got)
		}
	}
	// Having the lock back is not having the row looked for: relocking goes on.
	if got := liveNext(stRelocking, evLockRetaken); got != stRelocking {
		t.Errorf("relocking with the lock back and its row not looked for goes to %v", got)
	}
}

// The table of transitions the code decides from is the one the checks above
// read: every row is a named transition with an event, none is repeated, and
// each is one of today's (a transition from registering may be left out or
// added, since today's registration is part of the locked iteration).
func TestEveryTransitionIsNamedOnceAndIsTodays(t *testing.T) {
	if len(livenessTransitions) == 0 {
		t.Fatal("no transitions")
	}
	seen := map[[2]int]bool{}
	for _, tr := range livenessTransitions {
		key := [2]int{int(tr.From), int(tr.Event)}
		if seen[key] {
			t.Errorf("%v on %v is decided twice", tr.From, tr.Event)
		}
		seen[key] = true
		if tr.From == stRegistering {
			continue
		}
		if !slices.Contains(todaysTransitions, tr) {
			t.Errorf("%v on %v to %v is not one of today's transitions", tr.From, tr.Event, tr.To)
		}
	}
	for _, tr := range todaysTransitions {
		if tr.From != stRegistering && !slices.Contains(livenessTransitions, tr) {
			t.Errorf("today's %v on %v to %v is missing", tr.From, tr.Event, tr.To)
		}
	}
	for _, s := range allLiveStates {
		if s.String() == "" || strings.HasPrefix(s.String(), "liveState(") {
			t.Errorf("state %d has no name", int(s))
		}
	}
	for _, name := range []string{"locked", "relocking", "row gone"} {
		found := false
		for _, s := range allLiveStates {
			found = found || s.String() == name
		}
		if !found {
			t.Errorf("no state is named %q", name)
		}
	}
}

// Only a locked worker claims, forgets and refreshes seen_at; a relocking one
// reads its own row and retries the lock; a worker without its row does none.
func TestWhatEachStateAllowsIsTodays(t *testing.T) {
	want := map[liveState]map[liveAction]bool{
		stLocked:      {actClaim: true, actForget: true, actRefreshSeenAt: true},
		stRelocking:   {actReadOwnRow: true, actRetryLock: true},
		stRowGone:     {},
		stRegistering: {},
		stStopped:     {},
	}
	for s, allowed := range want {
		for _, a := range []liveAction{actClaim, actForget, actRefreshSeenAt, actReadOwnRow, actRetryLock} {
			if got := s.allows(a); got != allowed[a] {
				t.Errorf("%v allows action %d: %v, want %v", s, int(a), got, allowed[a])
			}
		}
	}
}

// One table names every timing, each an expression of the lease, with today's
// value at the default lease of 10 s.
var todaysTimings = map[string]time.Duration{
	"lease":          10 * time.Second,
	"heartbeat":      3 * time.Second, // Also the seen_at refresh.
	"counted":        10 * time.Second,
	"grace":          time.Second,
	"forgottenLease": 500 * time.Millisecond,
	"confirmGap":     500 * time.Millisecond,
	"bound":          500 * time.Millisecond,
	"lookPeriod":     250 * time.Millisecond,
	"relockRetry":    50 * time.Millisecond,
	"hold":           10 * time.Second,
}

func TestTheTimingTableHasTodaysRowsAtTodaysValues(t *testing.T) {
	if defaultLease != 10*time.Second {
		t.Errorf("the default lease is %v, want 10s", defaultLease)
	}
	seen := map[string]bool{}
	for _, row := range livenessTable {
		if seen[row.Name] {
			t.Errorf("row %q is twice in the table", row.Name)
		}
		seen[row.Name] = true
		// Rows beyond today's ten are allowed (the wait for the old session's jobs, the
		// retry after a failed lock, the bound of the row's delete, the throttle of the
		// relock warning are durations liveness.go writes today and the table must now
		// name); each still has to be an expression of the lease, which the checks below
		// and DOMAINS row for row hold it to.
		if want, ok := todaysTimings[row.Name]; ok {
			if got := row.Of(defaultLease); got != want {
				t.Errorf("%s is %v at the default lease, want exactly %v", row.Name, got, want)
			}
		} else if row.Of(defaultLease) <= 0 {
			t.Errorf("the extra row %q is %v at the default lease", row.Name, row.Of(defaultLease))
		}
		if !strings.Contains(row.Derivation, "lease") {
			t.Errorf("%s is derived as %q, which does not say it is an expression of the lease", row.Name, row.Derivation)
		}
	}
	for name := range todaysTimings {
		if !seen[name] {
			t.Errorf("the table has no row %q", name)
		}
	}
}

// A duration of a timing's own would not follow the lease: the table is the one
// place a duration is written. Comments and the table's own file are left out.
func TestNoLivenessDurationIsWrittenOutsideTheTable(t *testing.T) {
	duration := regexp.MustCompile(`\btime\.(Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)\b|\bDuration\(\d`)
	for _, file := range []string{"liveness.go", "jobs.go", "../../cmd/jellyscale/main.go"} {
		src := codeOf(t, file)
		src = withoutTable(src) // The table is the one place a duration may be written.
		switch file {
		case "liveness.go":
			if m := duration.FindString(src); m != "" {
				t.Errorf("liveness.go writes a duration (%s) outside the table", m)
			}
		case "jobs.go":
			for _, name := range []string{"workerAlive", "reapGrace"} {
				if strings.Contains(src, name) {
					t.Errorf("jobs.go still uses %s: Unavailable's wait and the counted window come from the table", name)
				}
			}
			// Register is a replica's registration: its waits are liveness timings
			// too (the row delete on shutdown is the table's deleteBound).
			if start := strings.Index(src, "\nfunc Register("); start < 0 {
				t.Error("jobs.go has no func Register")
			} else if body := src[start : start+strings.Index(src[start:], "\n}\n")]; duration.MatchString(body) {
				t.Errorf("Register writes a duration (%s) outside the table", duration.FindString(body))
			}
		default:
			if regexp.MustCompile(`\.Present\([^)]*time\.`).MatchString(src) {
				t.Error("cmd/jellyscale/main.go passes Present a duration of its own")
			}
		}
	}
}

// The table is the cluster's: nothing in the liveness code reads the worker's
// Lease or Heartbeat, so a worker run with another Lease changes only its jobs'.
func TestLivenessTimingsDoNotFollowTheWorkersOwnLease(t *testing.T) {
	src := codeOf(t, "liveness.go")
	for _, bad := range []string{".lease()", ".Lease", ".Heartbeat", ".heartbeat()"} {
		if strings.Contains(src, bad) {
			t.Errorf("liveness.go reads the worker's %s: the table is at the default lease whatever Lease the worker runs with", bad)
		}
	}
	unavailable := codeOf(t, "jobs.go")
	if i := strings.Index(unavailable, "func (j *Jobs) Unavailable"); i >= 0 {
		body := unavailable[i:]
		if j := strings.Index(body, "\n}\n"); j >= 0 {
			body = body[:j]
		}
		if strings.Contains(body, ".Lease") {
			t.Error("Unavailable reads a Lease: a replica's liveness timings are the table's")
		}
	}
}

// withoutTable cuts the declaration of livenessTable (from its var line to the
// first closing brace at the start of a line) out of src.
func withoutTable(src string) string {
	i := strings.Index(src, "var livenessTable")
	if i < 0 {
		return src
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		return src[:i]
	}
	return src[:i] + src[i+end+3:]
}

// codeOf is the file without its // comments and string literals' contents of
// comments, so a duration written in prose is not one written in code.
func codeOf(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "//"); i >= 0 && !strings.Contains(line[:i], `"`) {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// A row written as a literal ("lease/20" in a string beside a function that returns
// 500 ms) does not follow the lease. Each row, evaluated at other leases, scales
// with it, so Of is an expression of its argument and not a duration of its own.
func TestEveryTimingRowScalesWithTheLease(t *testing.T) {
	for _, row := range livenessTable {
		base := row.Of(defaultLease)
		for _, k := range []time.Duration{2, 3, 5} {
			if got := row.Of(k * defaultLease); got != k*base {
				t.Errorf("%s at %d times the lease is %v, want %v: it is not an expression of the lease", row.Name, k, got, k*base)
			}
		}
		if got := row.Of(defaultLease / 2); got != base/2 {
			t.Errorf("%s at half the lease is %v, want %v", row.Name, got, base/2)
		}
	}
}

// liveTiming(name) is the table's value at the default lease: what the code asks
// for, so the table is read by the code that waits. It agrees with the table.
func TestLiveTimingIsTheTablesValueAtTheDefaultLease(t *testing.T) {
	for _, row := range livenessTable {
		if got := liveTiming(row.Name); got != row.Of(defaultLease) {
			t.Errorf("liveTiming(%q) is %v, the table says %v", row.Name, got, row.Of(defaultLease))
		}
	}
}

// The state machine is the one the loop runs, and the table is the one the waits
// read; a machine and a table beside code that does what it did before are not
// the rewrite. These read the source, since the alternative (a hook that records
// transitions) would put test code in the app. A wrong build that adds the machine
// and leaves keepRow as it is fails here: it still has the old constants, still
// assigns no state from liveNext, and jobContext does not ask the state.
func TestTheLoopDecidesFromTheMachineAndTheWaitsReadTheTable(t *testing.T) {
	live := codeOf(t, "liveness.go")
	if !strings.Contains(live, "liveNext(") {
		t.Error("liveness.go never calls liveNext: the loop does not decide its next state from the machine")
	}
	// Outside livenessTransitions, a state is never assigned directly: the next state
	// is liveNext's answer. (An initial state is declared with :=.)
	body := withoutTable(withoutTransitions(live))
	if m := regexp.MustCompile(`[^:=!<>]=\s*st(Registering|Locked|Relocking|RowGone|Stopped)\b`).FindString(body); m != "" {
		t.Errorf("liveness.go assigns a state directly (%q) instead of taking liveNext's answer", strings.TrimSpace(m))
	}
	for _, name := range []string{"forgottenLease", "confirmGap", "bound", "lookPeriod", "relockRetry", "hold"} {
		if !strings.Contains(live, `liveTiming("`+name+`")`) {
			t.Errorf("liveness.go never reads the table's %q", name)
		}
	}
	jobs := codeOf(t, "jobs.go")
	for _, name := range []string{"counted", "grace"} {
		if !strings.Contains(jobs, `liveTiming("`+name+`")`) {
			t.Errorf("jobs.go never reads the table's %q (the counted window and Unavailable's wait)", name)
		}
	}
	i := strings.Index(live, "func (w *Worker) jobContext")
	if i < 0 {
		t.Fatal("no jobContext in liveness.go")
	}
	jc := live[i:]
	if j := strings.Index(jc, "\n}\n"); j >= 0 {
		jc = jc[:j]
	}
	if !strings.Contains(jc, "allows(actClaim)") {
		t.Error("jobContext does not ask the state whether it may claim (allows(actClaim))")
	}
}

// The five durations liveness.go writes today beside the ten (the wait for the old
// session's jobs, its poll, the retry after a failed lock, the bound of the row's
// DELETE, the throttle of the relock warning) are named in the table too, and no
// arithmetic on the lease stands in for a row either: outside the table liveness.go
// neither divides nor multiplies a duration by a number, nor does arithmetic on
// defaultLease.
func TestNoLivenessTimingIsComputedOutsideTheTable(t *testing.T) {
	body := withoutTable(codeOf(t, "liveness.go"))
	for _, re := range []string{`defaultLease\s*[/*]|[/*]\s*defaultLease`, `[A-Za-z_)\]]\s*[/*]\s*[0-9]`} {
		if m := regexp.MustCompile(re).FindString(body); m != "" {
			t.Errorf("liveness.go computes a timing outside the table (%q)", m)
		}
	}
}

// The constants of today's liveness code are gone from the whole package: a
// renamed alias in another file would keep their literals where the table is not.
func TestTheOldLivenessConstantsAreGoneFromThePackage(t *testing.T) {
	old := regexp.MustCompile(`\b(reapGrace|reapLease|relockEvery|lockCheck|reapHold|workerAlive)\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if m := old.FindString(codeOf(t, f)); m != "" {
			t.Errorf("%s still uses %s", f, m)
		}
	}
}

// withoutTransitions cuts the declaration of livenessTransitions out of src.
func withoutTransitions(src string) string {
	i := strings.Index(src, "var livenessTransitions")
	if i < 0 {
		return src
	}
	end := strings.Index(src[i:], "\n}\n")
	if end < 0 {
		return src[:i]
	}
	return src[:i] + src[i+end+3:]
}
