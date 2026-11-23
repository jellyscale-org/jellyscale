package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// JS-109. Configuration mistakes stop the start, naming what is wrong. run is
// the real start and reads the flags and the config before it needs Postgres,
// so with no database URL a good start gets as far as asking for one and a bad
// one is refused first: that is how these tell a refusal from a start.

func startWith(t *testing.T, roles, config string) error {
	t.Helper()
	t.Setenv("JELLYSCALE_DATABASE_URL", "")
	path := ""
	if config != "" {
		path = filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return run(roles, "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
}

func asksForTheDatabase(err error) bool {
	return err != nil && strings.Contains(err.Error(), "JELLYSCALE_DATABASE_URL")
}

// A role that is not api, stream or worker is refused with its name, whether it
// is alone or among good ones, and before the database is asked for.
func TestARoleThatIsMisspeltIsRefusedNamingIt(t *testing.T) {
	for _, roles := range []string{"wokrer", "api,wokrer,stream", " wokrer ", "Api", "worker,stream,apis"} {
		err := startWith(t, roles, "")
		bad := regexp.MustCompile(`wokrer|Api|apis`).FindString(roles)
		if err == nil || !strings.Contains(err.Error(), bad) || asksForTheDatabase(err) {
			t.Errorf("--roles=%q: want a refusal naming %q, got %v", roles, bad, err)
		}
	}
}

// A role listed twice is refused naming it, as docs/running.md says.
func TestARoleListedTwiceIsRefusedNamingIt(t *testing.T) {
	for roles, twice := range map[string]string{"api,api": "api", "stream,worker,stream": "stream", "worker, worker": "worker"} {
		err := startWith(t, roles, "")
		if err == nil || asksForTheDatabase(err) || !strings.Contains(err.Error(), twice) || !strings.Contains(err.Error(), "twice") {
			t.Errorf("--roles=%q: want a refusal naming %q listed twice, got %v", roles, twice, err)
		}
	}
}

func TestAnEmptyRoleIsRefusedNamingTheFlagAndSayingEmpty(t *testing.T) {
	for _, roles := range []string{"", "api,,stream", "api,", ",api", " "} {
		err := startWith(t, roles, "")
		if err == nil || asksForTheDatabase(err) ||
			!strings.Contains(err.Error(), "--roles") || !strings.Contains(strings.ToLower(err.Error()), "empty") {
			t.Errorf("--roles=%q: want a refusal naming --roles and saying a role is empty, got %v", roles, err)
		}
	}
}

// all is a whole set of roles, not a role: in a list it was dropped as unknown
// and the process ran as api alone.
func TestAllInsideAListIsRefusedNamingIt(t *testing.T) {
	for _, roles := range []string{"api,all", "all,worker", "all,all"} {
		err := startWith(t, roles, "")
		if err == nil || !strings.Contains(err.Error(), "all") || asksForTheDatabase(err) {
			t.Errorf("--roles=%q: want a refusal naming all, got %v", roles, err)
		}
	}
}

func TestEveryListOfTheThreeRolesStartsAsBefore(t *testing.T) {
	for _, roles := range []string{
		"all", "api", "stream", "worker", "api,stream", "stream,api", "api,worker", "worker,stream", "stream,worker",
		"api,stream,worker", "worker,stream,api", "stream,worker,api", "api, stream , worker", " worker,api ",
	} {
		if err := startWith(t, roles, ""); !asksForTheDatabase(err) {
			t.Errorf("--roles=%q should start, and so ask for the database: %v", roles, err)
		}
	}
}

// scanInterval is read at the start, not when the scan loop starts, by every
// role (a worker with no libraries never starts the loop), and a value that is
// not a positive duration stops the start naming the key.
func TestAScanIntervalThatIsNotAPositiveDurationStopsTheStartNamingTheKey(t *testing.T) {
	for _, roles := range []string{"api", "stream", "worker", "all"} {
		for _, bad := range []string{`"soon"`, `"2 minutes"`, `"15"`, `15`, `true`, `"0s"`, `"-1h"`} {
			for _, config := range []string{
				`{"scanInterval": ` + bad + `}`,
				`{"scanInterval": ` + bad + `, "libraries": []}`,
			} {
				err := startWith(t, roles, config)
				if err == nil || !strings.Contains(err.Error(), "scanInterval") ||
					strings.Contains(err.Error(), "unknown field") || asksForTheDatabase(err) {
					t.Errorf("--roles=%s %s: want a refusal naming scanInterval, got %v", roles, config, err)
				}
			}
		}
	}
}

func TestAScanIntervalLeftOutOrPositiveStartsEveryRole(t *testing.T) {
	for _, roles := range []string{"api", "stream", "worker", "all"} {
		for _, config := range []string{`{}`, `{"scanInterval": "200ms"}`, `{"scanInterval": "15m"}`, `{"scanInterval": "1h"}`} {
			if err := startWith(t, roles, config); !asksForTheDatabase(err) {
				t.Errorf("--roles=%s %s should start, and so ask for the database: %v", roles, config, err)
			}
		}
	}
}
