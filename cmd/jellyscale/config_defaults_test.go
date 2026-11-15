package main

import (
	"testing"
	"time"
)

// docs/running.md gives these defaults for a process without --config, which
// takes a config of {}; the values are written out here so that a changed
// default fails this test and not only the doc.
func TestTheDefaultsOfAnEmptyConfigAreTheOnesTheDocGives(t *testing.T) {
	cfg, err := parseConfig([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "jellyscale" || cfg.scan != 15*time.Minute || cfg.grace != 30*time.Second ||
		cfg.AdminUser != "" || cfg.WebDir != "" {
		t.Errorf("defaults are name %q, scan %v, grace %v, adminUser %q, webDir %q", cfg.Name, cfg.scan, cfg.grace, cfg.AdminUser, cfg.WebDir)
	}
	cfg, err = parseConfig([]byte(`{"name": "home", "scanInterval": "1h"}`))
	if err != nil || cfg.Name != "home" || cfg.scan != time.Hour {
		t.Errorf("set values are kept: %+v, %v", cfg, err)
	}
}
