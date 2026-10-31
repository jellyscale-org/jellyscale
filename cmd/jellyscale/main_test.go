package main

import (
	"context"
	"io"
	"log/slog"
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
	// A worker with no transcodeStore would run in silence, so it doesn't start.
	if err := open(config{}, "worker"); err == nil || !strings.Contains(err.Error(), "transcodeStore") {
		t.Fatalf("a worker without a store: %v", err)
	}
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
