package main

import (
	"context"
	"io"
	"log/slog"
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
