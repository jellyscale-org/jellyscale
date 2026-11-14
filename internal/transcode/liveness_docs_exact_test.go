package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// The looser docs checks match by substring, so a value or event found on
// another row would pass. These compare cell by cell and line by line: DOMAINS
// 25 and the code are the same table and the same transitions, and a change on
// one side alone fails.

func TestInvariant25TableMatchesTheCodeCellByCell(t *testing.T) {
	want := map[string]string{}
	for _, row := range livenessTable {
		want[row.Name] = row.Derivation + " | " + row.Of(defaultLease).String()
	}
	got := map[string]string{}
	for _, line := range strings.Split(invariant25(t), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 3 {
			t.Errorf("table line %q has %d cells, want 3", line, len(cells))
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if cells[0] == "name" {
			continue
		}
		if _, dup := got[cells[0]]; dup {
			t.Errorf("DOMAINS 25 has two rows named %q", cells[0])
		}
		got[cells[0]] = cells[1] + " | " + cells[2]
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("row %q: DOMAINS has %q, the code has %q", name, got[name], w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("DOMAINS 25 has a row %q the code lacks", name)
		}
	}
}

func TestInvariant25TransitionsMatchTheCodeLineByLine(t *testing.T) {
	names := regexp.MustCompile(`, | and `)
	got := map[string]bool{}
	for _, line := range strings.Split(invariant25(t), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		left, to, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
		if !ok {
			continue
		}
		if i := strings.Index(to, " ("); i >= 0 {
			to = to[:i]
		}
		var event string
		for _, e := range []liveEvent{evRegistered, evLockEnded, evLockRetakenRowFound, evRowGone, evOwnLockMissing, evReadyToRegister, evShutdown} {
			if s := e.String(); strings.HasSuffix(left, ", "+s) && len(s) > len(event) {
				event = s
			}
		}
		if event == "" {
			t.Errorf("transition line %q names no known event", line)
			continue
		}
		for _, from := range names.Split(strings.TrimSuffix(left, ", "+event), -1) {
			got[from+"|"+event+"|"+strings.TrimSpace(to)] = true
		}
	}
	want := map[string]bool{}
	for _, tr := range livenessTransitions {
		if tr.From == stStopped {
			continue
		}
		want[tr.From.String()+"|"+tr.Event.String()+"|"+tr.To.String()] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("DOMAINS 25 lacks the transition %s", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("DOMAINS 25 has the transition %s the code lacks", k)
		}
	}
}

// Every duration that decides a wait in Unavailable comes from the table,
// polling included: no time.After or time.Sleep of a literal in jobs.go.
func TestNoLiteralWaitInTheRefusalLoop(t *testing.T) {
	src := doc(t, "internal/transcode/jobs.go")
	if m := regexp.MustCompile(`time\.(After|Sleep)\([^)]*(Millisecond|Second)`).FindString(src); m != "" {
		t.Errorf("jobs.go waits a literal duration outside the table: %s", m)
	}
}
