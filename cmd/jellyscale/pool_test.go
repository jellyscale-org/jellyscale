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
