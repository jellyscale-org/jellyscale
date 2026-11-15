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

// docs/running.md says what a process started without --config does, in the
// words of the task (compared without case or line breaks): it is the same as
// a config of {}, so that every key left out is at the default the doc gives.
func TestRunningDocSaysWhatNoConfigFlagDoes(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ToLower(strings.Join(strings.Fields(string(raw)), " "))
	for _, want := range []string{
		"without `--config`",
		"every key at the default",
		"the same as a config of `{}`",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/running.md does not say %q", want)
		}
	}
}

// Without --config the doc must say what the settings are: it names every key
// of the config that no other doc covers, with its default, and says that a
// scanInterval that is not a positive duration stops the start.
func TestRunningDocGivesTheDefaultOfEveryKeyNoOtherDocCovers(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.ToLower(strings.Join(strings.Fields(string(raw)), " "))
	for _, key := range []string{"name", "adminUser", "webDir", "scanInterval"} {
		if !strings.Contains(doc, "`"+strings.ToLower(key)+"`") {
			t.Errorf("docs/running.md does not name the key `%s`", key)
		}
	}
	// The default is said in the sentence about the key, not elsewhere.
	i := strings.Index(doc, "`scaninterval`")
	if i < 0 {
		return
	}
	sentence := doc[i:]
	if end := strings.Index(sentence, ". "); end >= 0 {
		sentence = sentence[:end]
	}
	for _, want := range []string{"15m", "positive duration", "stops the start"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("docs/running.md on `scanInterval` does not say %q: %s", want, sentence)
		}
	}
}

// Every json key of the config struct is named in docs/running.md or
// docs/transcoding.md, so that the doc can give the default of each.
func TestEveryConfigKeyIsNamedInTheDocs(t *testing.T) {
	var docs string
	for _, f := range []string{"running.md", "transcoding.md"} {
		raw, err := os.ReadFile("../../docs/" + f)
		if err != nil {
			t.Fatal(err)
		}
		docs += string(raw)
	}
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "type config struct {")
	body := string(src)[start:]
	body = body[:strings.Index(body, "\n}\n")]
	keys := regexp.MustCompile("`json:\"([A-Za-z]+)").FindAllStringSubmatch(body, -1)
	if len(keys) < 10 {
		t.Fatalf("found only %d config keys in main.go", len(keys))
	}
	for _, k := range keys {
		if k[1] == "adminPassword" {
			continue // Held in the environment, not the file.
		}
		if !strings.Contains(docs, "`"+k[1]+"`") {
			t.Errorf("config key %s is named in neither docs/running.md nor docs/transcoding.md", k[1])
		}
	}
}
