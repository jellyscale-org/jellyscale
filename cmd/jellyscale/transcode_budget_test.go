package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// JS-078: the transcode store's size budget is a config key, transcodeBudget,
// the field config.TranscodeBudget (names the task implies; the key is the one
// docs/transcoding.md's table lists). A size is a whole number of bytes, or a
// whole number with a decimal unit (KB, MB, GB, TB: 1 GB is 10^9 bytes), as
// docs/transcoding.md says. Left out there is no budget.

// start is what a process does with the key before anything else: parse the
// config, then open transcoding for its roles. It returns the first error.
func startBudgetConfig(raw string, roles []string, pool *pgxpool.Pool) error {
	cfg, err := parseConfig([]byte(raw))
	if err != nil {
		return err
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	_, err = openTranscoding(ctx, cfg, roles, pool, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	return err
}

// A value that is not a size, or is negative, stops the start and names the
// key, whatever the role. The config must still parse (an "unknown field"
// error names the key too, and would pass this check on a build that has no
// such key), and the refusal must come before anything is opened: the api and
// stream roles are started with no database here.
func TestABadTranscodeBudgetStopsTheStartNamingTheKeyWhateverTheRole(t *testing.T) {
	for _, value := range []string{`"lots"`, `"-1GB"`, `"-1"`, `"1.5.2GB"`, `"GB"`, `"12 parsecs"`, `"1GB2"`, `true`, `[]`} {
		for _, roles := range [][]string{{"api"}, {"stream"}, {"api", "stream"}, {"worker"}, {"api", "stream", "worker"}} {
			raw := `{"transcodeBudget": ` + value + `}`
			err := startBudgetConfig(raw, roles, nil)
			if err == nil || !strings.Contains(err.Error(), "transcodeBudget") {
				t.Errorf("transcodeBudget %s, roles %v: %v", value, roles, err)
			} else if strings.Contains(err.Error(), "unknown field") {
				t.Errorf("transcodeBudget %s, roles %v: the key is not known: %v", value, roles, err)
			}
		}
	}
}

// A size, or no key at all, starts, as a worker does with a pool that has not
// connected (as cores_test.go does); and the sizes of the doc are accepted.
func TestASizeOrNoTranscodeBudgetStarts(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, raw := range []string{
		`{}`,
		`{"transcodeStore": "file:///nonexistent/segments"}`,
		`{"transcodeStore": "file:///nonexistent/segments", "transcodeBudget": "1500000"}`,
		`{"transcodeStore": "file:///nonexistent/segments", "transcodeBudget": "200GB"}`,
		`{"transcodeStore": "file:///nonexistent/segments", "transcodeBudget": "2TB", "transcodeRetention": "48h"}`,
	} {
		if err := startBudgetConfig(raw, []string{"worker"}, pool); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// docs/transcoding.md's table lists the key, with what no budget means.
func TestTranscodingDocListsTheBudgetKey(t *testing.T) {
	raw, err := os.ReadFile("../../docs/transcoding.md")
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| `transcodeBudget`") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("docs/transcoding.md's table has no row for `transcodeBudget`")
	}
	if !strings.Contains(strings.ToLower(row), "none") {
		t.Errorf("the row does not say that left out there is no budget (default none): %s", row)
	}
	if !strings.Contains(row, "transcodeRetention") {
		t.Errorf("the row does not say how the budget relates to the age sweep, transcodeRetention, which still runs: %s", row)
	}
}

// docs/running.md says how to size the volume, what each sweep removes and
// keeps, and names the size metric; DOMAINS.md invariant 39 says what the
// sweeps remove. The words are the task's: each is one the doc must use.
func TestRunningDocSaysHowToSizeTheTranscodeStore(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, want := range []string{
		"`transcodebudget`",
		"jellyscale_transcode_store_bytes",
		"per hour",           // bytes per hour per Mbps of a re-encode
		"mbps",               //
		"450",                // 1 Mbps for an hour is 450 MB: 10^6 * 3600 / 8
		"own bitrate",        // a copy is stored at the file's own bitrate
		"worked example",     // one for a re-encode and one for a copy
		"sweep period",       // the store can go over by what is watched plus one sweep period of writes
		"watched",            //
		"least recently",     // the budget removes whole specs, least recently requested first
		"missinggrace",       // what is kept: a file hidden as missing within the grace
		"etag",               // the previous version of a changed file goes
		"font",               // a font a remaining manifest names stays
		"chapter",            //
		".sup",               //
		"transcoderetention", // segments by age
	} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/running.md does not say %q", want)
		}
	}
	// The worked examples are numbers: at least two lines that work out a
	// size in GB from a bitrate in Mbps, one of them a copy.
	if n := len(regexp.MustCompile(`(?i)\d+(\.\d+)?\s*mbps[^\n]{0,200}\d+(\.\d+)?\s*gb`).FindAllString(string(raw), -1)); n < 2 {
		t.Errorf("docs/running.md has %d worked examples that turn Mbps into GB, want at least 2 (a re-encode and a copy)", n)
	}
}

// Invariant 39 is the sweeps: segments by age and by budget, and the assets of
// versions that no longer exist.
func TestInvariant39SaysWhatTheSweepsRemove(t *testing.T) {
	raw, err := os.ReadFile("../../DOMAINS.md")
	if err != nil {
		t.Fatal(err)
	}
	var invariant string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "39. ") {
			invariant = strings.ToLower(line)
		}
	}
	if invariant == "" {
		t.Fatal("DOMAINS.md has no invariant 39")
	}
	for _, want := range []string{"transcoderetention", "transcodebudget", "least recently", "asset", "missinggrace", "watch"} {
		if !strings.Contains(invariant, want) {
			t.Errorf("DOMAINS.md invariant 39 does not say %q: %s", want, invariant)
		}
	}
}
