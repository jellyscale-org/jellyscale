package main

import (
	"os"
	"strings"
	"testing"
)

// databaseMaxConns sizes the Postgres pool every role in the process shares.
// These reference the names the task implies (config.DatabaseMaxConns) and do
// not compile until it exists.

func TestConfigDatabaseMaxConns(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"databaseMaxConns": 24}`))
	if err != nil || cfg.DatabaseMaxConns != 24 {
		t.Fatalf("%+v: %v", cfg, err)
	}
	// Left out means the default the stress run chose, so the field is zero.
	if cfg, err = parseConfig([]byte(`{}`)); err != nil || cfg.DatabaseMaxConns != 0 {
		t.Fatalf("%+v: %v", cfg, err)
	}
}

// A pool of 0 or less would be read as the default, a typo of a minus sign
// would hide; the start refuses it.
func TestConfigRefusesANegativePool(t *testing.T) {
	_, err := parseConfig([]byte(`{"databaseMaxConns": -3}`))
	if err == nil || !strings.Contains(err.Error(), "databaseMaxConns") {
		t.Fatalf("a negative pool: %v", err)
	}
}

func TestRunningDocsNameThePoolKey(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "databaseMaxConns") {
		t.Error("docs/running.md does not document databaseMaxConns")
	}
}

// Operators turn helpers (parallel transcoding) off with transcodeHelpers.
func TestHelpersCanBeTurnedOffAndTheDocsSayHow(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"transcodeHelpers": false}`))
	if err != nil {
		t.Fatalf("transcodeHelpers is not a config key: %v", err)
	}
	if cfg.TranscodeHelpers == nil || *cfg.TranscodeHelpers {
		t.Errorf("transcodeHelpers: false did not read as off: %v", cfg.TranscodeHelpers)
	}
	if cfg, err := parseConfig([]byte(`{}`)); err != nil || cfg.TranscodeHelpers != nil {
		t.Errorf("helpers are on unless turned off: %v, %v", cfg.TranscodeHelpers, err)
	}
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "transcodeHelpers") {
		t.Error("docs/running.md does not document transcodeHelpers")
	}
}
