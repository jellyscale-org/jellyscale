package transcode

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// JS-084's checks that need no Postgres: worker liveness is one state machine
// whose transitions are decided in one place, its timings come from one table
// of the lease, and nothing else in the liveness code writes a duration.
//
// The names below are the rewrite's:
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
	}
	for name := range todaysTimings {
		if !seen[name] {
			t.Errorf("the table has no row %q", name)
		}
	}
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
