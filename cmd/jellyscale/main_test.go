package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/library"
)

// What can never work is refused before anything is registered or started, so
// these need no database.
func TestOpenTranscodingRefusesWhatCannotWork(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func(cfg config, roles ...string) error {
		_, err := openTranscoding(context.Background(), cfg, roles, nil, "", log)
		return err
	}
	// A worker with no transcodeStore still extracts subtitles, so it starts.
	// The pool connects lazily and nothing here needs it to.
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, stop := context.WithCancel(context.Background())
	if _, err := openTranscoding(ctx, config{}, []string{"worker"}, pool, "", log); err != nil {
		t.Fatalf("a worker without a store: %v", err)
	}
	stop()
	if err := open(config{TranscodeSlots: -1}, "api"); err == nil || !strings.Contains(err.Error(), "transcodeSlots") {
		t.Fatalf("negative slots: %v", err)
	}
	// A store URI with credentials would reach Postgres and the log.
	for _, uri := range []string{"s3://AKIA:hunter2@bucket/x", "s3://bucket/x?secret=hunter2", "s3://bucket/x?access_key=hunter2"} {
		err := open(config{TranscodeStore: uri}, "api", "stream", "worker")
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Fatalf("%s: %v", uri, err)
		}
	}
}

// The same goes for a library's stores, which are written to the database.
func TestLibrariesWithCredentialsAreRefused(t *testing.T) {
	_, err := library.Sync(context.Background(), nil, []library.Config{{
		Name: "Movies", Kind: "movies", Media: "file:///media", Assets: "s3://bucket/a?secret=hunter2"}})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
}

// A mistyped key must stop the start and name itself, or an option such as
// chapterImages would be silently left off.
func TestConfigRefusesUnknownKeys(t *testing.T) {
	_, err := parseConfig([]byte(`{"libraries": [{"name": "Movies", "chapterImagess": true}]}`))
	if err == nil || !strings.Contains(err.Error(), "chapterImagess") {
		t.Fatalf("a mistyped library key: %v", err)
	}
	_, err = parseConfig([]byte(`{"scanIntervall": "1h"}`))
	if err == nil || !strings.Contains(err.Error(), "scanIntervall") {
		t.Fatalf("a mistyped key: %v", err)
	}
}

// Anything after the config is refused, as json.Unmarshal did: a second object
// pasted in could carry the very key that is mistyped.
func TestConfigRefusesTrailingData(t *testing.T) {
	for _, raw := range []string{`{} {"scanIntervall": "1h"}`, `{} trailing`, `{}}`} {
		if _, err := parseConfig([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if _, err := parseConfig([]byte("{}\n")); err != nil {
		t.Errorf("a config with a trailing newline: %v", err)
	}
}

// The configs the repo ships must still load.
func TestShippedConfigsLoad(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/compose/config.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig(raw)
	if err != nil || len(cfg.Libraries) != 2 {
		t.Fatalf("%+v: %v", cfg, err)
	}
}

// metricsListen is a config key, and off when it is left out.
func TestConfigMetricsListen(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"metricsListen": ":9090"}`))
	if err != nil || cfg.MetricsListen != ":9090" {
		t.Fatalf("%+v: %v", cfg, err)
	}
	if cfg, err = parseConfig([]byte(`{}`)); err != nil || cfg.MetricsListen != "" {
		t.Fatalf("%+v: %v", cfg, err)
	}
}

// JS-076. The grace a missing file or library keeps its watch state for, and
// the share of a library a scan may lose, are config keys; a value that can't
// be read stops the start naming the key. newScanner is what the worker builds
// its scanner with (the names it and the keys use are the task's).
func TestScannerReadsTheGraceAndTheShareFromConfig(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	read := func(raw string) (*library.Scanner, error) {
		cfg, err := parseConfig([]byte(raw))
		if err != nil {
			return nil, err
		}
		return newScanner(cfg, nil, log)
	}
	scanner, err := read(`{"missingGrace": "72h", "maxLostShare": 0.4}`)
	if err != nil || scanner.Grace != 72*time.Hour || scanner.MaxLostShare != 0.4 {
		t.Fatalf("%+v: %v", scanner, err)
	}
	scanner, err = read(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if scanner.Grace != library.DefaultGrace || scanner.MaxLostShare != library.DefaultMaxLostShare {
		t.Fatalf("defaults %v and %v, want %v and %v", scanner.Grace, scanner.MaxLostShare, library.DefaultGrace, library.DefaultMaxLostShare)
	}
	if library.DefaultGrace < 48*time.Hour {
		t.Fatalf("the default grace is %v, want at least two days", library.DefaultGrace)
	}
	if library.DefaultMaxLostShare <= 0 || library.DefaultMaxLostShare >= 1 {
		t.Fatalf("the default share is %v, want between 0 and 1", library.DefaultMaxLostShare)
	}
}

func TestUnreadableOrNegativeGraceStopsTheStartNamingTheKey(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if _, err := parseConfig([]byte(`{"missingGrace": "72h"}`)); err != nil {
		t.Fatalf("a grace of 72h: %v", err)
	}
	for _, value := range []string{`"soon"`, `"-1h"`, `"-1ns"`, `5`, `true`, `"2 days"`} {
		cfg, err := parseConfig([]byte(`{"missingGrace": ` + value + `}`))
		if err == nil {
			_, err = newScanner(cfg, nil, log)
		}
		if err == nil || !strings.Contains(err.Error(), "missingGrace") {
			t.Errorf("missingGrace %s: %v", value, err)
		}
	}
}

// JS-031. The drain grace is one config setting, shutdownGrace (the name is the
// task's choice), and a value that is not a duration or is negative stops the
// start naming the key. run is the real start: it reads the config before it
// needs Postgres, so with no database URL a good grace gets as far as asking for
// one, and a bad one is refused first.
func TestUnreadableOrNegativeShutdownGraceStopsTheStartNamingTheKey(t *testing.T) {
	t.Setenv("JELLYSCALE_DATABASE_URL", "")
	start := func(value string) error {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"shutdownGrace": `+value+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return run("api", "127.0.0.1:0", path, slog.New(slog.DiscardHandler))
	}
	for _, good := range []string{`"30s"`, `"2m"`, `"0s"`} {
		if err := start(good); err == nil || !strings.Contains(err.Error(), "JELLYSCALE_DATABASE_URL") {
			t.Errorf("shutdownGrace %s should be accepted and the start go on to ask for the database: %v", good, err)
		}
	}
	for _, bad := range []string{`"soon"`, `"-1s"`, `"-1ns"`, `30`, `true`, `"2 minutes"`} {
		err := start(bad)
		if err == nil || !strings.Contains(err.Error(), "shutdownGrace") || strings.Contains(err.Error(), "unknown field") || strings.Contains(err.Error(), "JELLYSCALE_DATABASE_URL") {
			t.Errorf("shutdownGrace %s: want a refusal naming the key, got %v", bad, err)
		}
	}
}

// The shipped ingress is told to do what the failover guarantees rest on: poll
// /readyz, stop sending to a replica that fails it, and retry a GET that failed
// before its response header on another replica. Without it a killed replica is
// a 502 on a segment and a player reset to 0:00. Each reverse_proxy of the
// Caddyfile (stream and api) carries it, and the doc says what any other ingress
// must do.
func TestShippedIngressPollsReadinessAndRetriesOnAnotherReplica(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/compose/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	config := regexp.MustCompile(`(?m)#.*$`).ReplaceAllString(string(raw), "")
	proxies := len(regexp.MustCompile(`(?m)^\s*reverse_proxy\s`).FindAllString(config, -1))
	if proxies < 2 {
		t.Fatalf("expected a reverse_proxy for stream and one for api, found %d", proxies)
	}
	for directive, pattern := range map[string]string{
		"health_uri /readyz":      `(?m)^\s*health_uri\s+/readyz\s*$`,
		"health_interval (<= 2s)": `(?m)^\s*health_interval\s+(\d+ms|[12]s)\s*$`,
		"lb_try_duration":         `(?m)^\s*lb_try_duration\s+\d+(ms|s)\s*$`,
		"lb_retries":              `(?m)^\s*lb_retries\s+[1-9]\d*\s*$`,
	} {
		if n := len(regexp.MustCompile(pattern).FindAllString(config, -1)); n != proxies {
			t.Errorf("deploy/compose/Caddyfile has %d of %s for %d reverse_proxy blocks", n, directive, proxies)
		}
	}
}
