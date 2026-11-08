package main

import (
	"strings"
	"testing"
)

// A pool of 1 deadlocks (the hint listener keeps the only connection), and a
// value past int32 would wrap to a small pool; both are refused at start.
func TestConfigRefusesAPoolThatCannotWork(t *testing.T) {
	for _, n := range []string{"1", "10001", "4294967297"} {
		if _, err := parseConfig([]byte(`{"databaseMaxConns": ` + n + `}`)); err == nil || !strings.Contains(err.Error(), "databaseMaxConns") {
			t.Errorf("databaseMaxConns %s: %v", n, err)
		}
	}
	for _, n := range []string{"0", "2", "10000"} {
		if _, err := parseConfig([]byte(`{"databaseMaxConns": ` + n + `}`)); err != nil {
			t.Errorf("databaseMaxConns %s: %v", n, err)
		}
	}
}
