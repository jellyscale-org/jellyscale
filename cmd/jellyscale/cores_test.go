package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// JS-087: a worker declares its cores in the config, as transcodeSlots was
// declared. The key and the field are names the task implies: transcodeCores,
// config.TranscodeCores.

// A negative number is refused at startup, naming the key, as transcodeSlots
// is.
func TestNegativeCoresAreRefusedAtStartup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := openTranscoding(context.Background(), config{TranscodeCores: -1}, []string{"api"}, nil, "", log)
	if err == nil || !strings.Contains(err.Error(), "transcodeCores") {
		t.Fatalf("negative cores: %v", err)
	}
}

// A number too large for the row's slots column is refused at startup too,
// rather than failing every registration of a worker that started.
func TestAbsurdCoresAreRefusedAtStartup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, cores := range []float64{10001, 3e9} {
		_, err := openTranscoding(context.Background(), config{TranscodeCores: cores}, []string{"api"}, nil, "", log)
		if err == nil || !strings.Contains(err.Error(), "transcodeCores") {
			t.Errorf("%v cores: %v", cores, err)
		}
	}
}

// A config that declares no cores starts, and so does one that still sets
// transcodeSlots; both are parsed, and neither stops a worker from starting.
func TestAConfigWithNoCoresOrWithOldSlotsStarts(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for name, raw := range map[string]string{
		"no cores":                 `{"transcodeStore": "file:///nonexistent/segments"}`,
		"transcodeSlots still set": `{"transcodeStore": "file:///nonexistent/segments", "transcodeSlots": 3}`,
		"cores declared":           `{"transcodeStore": "file:///nonexistent/segments", "transcodeCores": 6}`,
		"cores and slots both set": `{"transcodeStore": "file:///nonexistent/segments", "transcodeCores": 2.5, "transcodeSlots": 3}`,
		"fractional cores":         `{"transcodeCores": 0.5}`,
		"a worker with no store":   `{}`,
		"transcodeSlots, no store": `{"transcodeSlots": 1}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := parseConfig([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if _, err := openTranscoding(ctx, cfg, []string{"worker"}, pool, "", log); err != nil {
				t.Fatalf("the worker did not start: %v", err)
			}
		})
	}
}
