package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

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
