// Command jellyscale runs any combination of the server's roles. Production
// runs them as separate Deployments; --roles=all is for development.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/api"
	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

const roleWorker = "worker"

type config struct {
	Name          string           `json:"name"`
	AdminUser     string           `json:"adminUser"`
	AdminPassword string           `json:"adminPassword"`
	WebDir        string           `json:"webDir"`
	ScanInterval  string           `json:"scanInterval"`
	Libraries     []library.Config `json:"libraries"`

	// TranscodeStore is where transcoded segments live, a store URI such as
	// file:///var/lib/jellyscale/segments or s3://bucket/segments?endpoint=...
	// Every role needs it; without it nothing transcodes.
	TranscodeStore string `json:"transcodeStore"`
	// TranscodeScratch is where a worker's ffmpeg writes before uploading.
	TranscodeScratch string `json:"transcodeScratch"`
	// TranscodeSlots bounds concurrent ffmpeg processes per worker.
	TranscodeSlots int `json:"transcodeSlots"`
	// TranscodeIdle is how long a spec may go without a request before its
	// ffmpeg stops, and TranscodeRetention how long its segments stay after.
	TranscodeIdle      string `json:"transcodeIdle"`
	TranscodeRetention string `json:"transcodeRetention"`
}

func main() {
	roles := flag.String("roles", "all", "comma-separated roles: api, stream, worker, or all")
	listen := flag.String("listen", ":8096", "HTTP listen address")
	configPath := flag.String("config", "", "path to the JSON config file")
	printRoutes := flag.Bool("print-routes", false, "print the route manifest and exit")
	flag.Parse()

	if *printRoutes {
		fmt.Print(api.Manifest((&api.Server{}).Routes()))
		return
	}

	level := slog.LevelInfo
	if os.Getenv("JELLYSCALE_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(*roles, *listen, *configPath, log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(roleList, listen, configPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var cfg config
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.Name == "" {
		cfg.Name = "jellyscale"
	}
	// Secrets come from the environment, so config files can live in Git.
	if value := os.Getenv("JELLYSCALE_ADMIN_PASSWORD"); value != "" {
		cfg.AdminPassword = value
	}
	dsn := os.Getenv("JELLYSCALE_DATABASE_URL")
	if dsn == "" {
		return errors.New("JELLYSCALE_DATABASE_URL is required")
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	serverID, err := api.EnsureServerID(ctx, pool)
	if err != nil {
		return err
	}
	if cfg.AdminUser != "" {
		if cfg.AdminPassword == "" {
			return errors.New("adminUser needs JELLYSCALE_ADMIN_PASSWORD")
		}
		if err := api.EnsureAdmin(ctx, pool, cfg.AdminUser, cfg.AdminPassword); err != nil {
			return err
		}
	}
	libraries, err := library.Sync(ctx, pool, cfg.Libraries)
	if err != nil {
		return err
	}

	enabled := parseRoles(roleList)
	transcodes, err := openTranscoding(ctx, cfg, enabled, pool, serverID, log)
	if err != nil {
		return err
	}
	if slices.Contains(enabled, roleWorker) {
		interval := 15 * time.Minute
		if cfg.ScanInterval != "" {
			if interval, err = time.ParseDuration(cfg.ScanInterval); err != nil {
				return fmt.Errorf("scanInterval: %w", err)
			}
		}
		go scanLoop(ctx, pool, libraries, interval, log)
	}
	if !slices.Contains(enabled, api.RoleAPI) && !slices.Contains(enabled, api.RoleStream) {
		<-ctx.Done()
		<-transcodes.stopped
		return nil
	}

	server := &api.Server{
		Pool: pool, Items: &items.Repo{Pool: pool, ServerID: serverID}, ServerID: serverID,
		Name: cfg.Name, WebDir: cfg.WebDir, Roles: enabled, Log: log, Transcode: transcodes.jobs,
	}
	httpServer := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdown) //nolint:errcheck
	}()
	log.Info("listening", "addr", listen, "roles", enabled)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-transcodes.stopped // ffmpeg is gone before we exit.
	return nil
}

// transcoding is what this process runs for transcodes.
type transcoding struct {
	jobs *transcode.Jobs // nil when transcoding is off
	// stopped closes once the process has left transcode_nodes and the worker,
	// if any, has ended every ffmpeg.
	stopped chan struct{}
}

// openTranscoding registers this process in transcode_nodes, where the other
// roles compare transcodeStore with theirs, and starts the worker and the hint
// listener its roles need. The context ends them.
func openTranscoding(ctx context.Context, cfg config, roles []string, pool *pgxpool.Pool, serverID string, log *slog.Logger) (transcoding, error) {
	result := transcoding{stopped: make(chan struct{})}
	if cfg.TranscodeSlots < 0 {
		return result, errors.New("transcodeSlots: want a positive number, or leave it out for the default")
	}
	worker := slices.Contains(roles, roleWorker)
	if cfg.TranscodeStore == "" && worker && !slices.Contains(roles, api.RoleStream) && !slices.Contains(roles, api.RoleAPI) {
		// A worker with nothing to share segments through would sit idle and
		// say nothing, so a process that is only a worker doesn't start.
		return result, errors.New("transcodeStore is not set, and the worker role needs it: set it, to the same value, on every role")
	}
	var segments store.Store
	if cfg.TranscodeStore != "" {
		var err error
		if segments, err = store.Open(cfg.TranscodeStore); err != nil {
			return result, fmt.Errorf("transcodeStore: %w", err)
		}
	} else {
		log.Warn("transcodeStore is not set: this process cannot run, serve or offer transcodes, so set it, to the same value, on every role or none")
	}
	idle, err := optionalDuration(cfg.TranscodeIdle, "transcodeIdle")
	if err != nil {
		return result, err
	}
	retention, err := optionalDuration(cfg.TranscodeRetention, "transcodeRetention")
	if err != nil {
		return result, err
	}

	id := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	slots := 0
	if worker && segments != nil {
		slots = cmp.Or(cfg.TranscodeSlots, 2)
	}
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		transcode.Register(ctx, pool, log, id, strings.Join(roles, ","), cfg.TranscodeStore, slots, 3*time.Second)
	}()
	// Called once nothing more will be added to running.
	finish := func() {
		go func() {
			running.Wait()
			close(result.stopped)
		}()
	}
	if segments == nil {
		finish()
		return result, nil
	}
	// Only roles that wait for hints listen for them.
	var hub *transcode.Hub
	if slices.Contains(roles, api.RoleStream) || worker {
		hub = &transcode.Hub{Pool: pool, Log: log}
		go hub.Run(ctx)
	}
	result.jobs = &transcode.Jobs{Pool: pool, Segments: segments, Hub: hub}
	if !worker {
		finish()
		return result, nil
	}
	scratch := cfg.TranscodeScratch
	if scratch == "" {
		scratch = filepath.Join(os.TempDir(), "jellyscale-transcode")
	}
	var media store.Cache
	w := &transcode.Worker{
		ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: serverID}, Segments: segments, Hub: hub, Log: log,
		Media: media.Open, Scratch: scratch, Slots: slots, Idle: idle, Retention: retention,
	}
	running.Add(1)
	go func() {
		defer running.Done()
		w.Run(ctx)
	}()
	finish()
	return result, nil
}

func optionalDuration(value, name string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: want a positive duration such as 60s", name)
	}
	return d, nil
}

func parseRoles(value string) []string {
	if value == "all" {
		return []string{api.RoleAPI, api.RoleStream, roleWorker}
	}
	var roles []string
	for _, role := range strings.Split(value, ",") {
		roles = append(roles, strings.TrimSpace(role))
	}
	return roles
}

func scanLoop(ctx context.Context, pool *pgxpool.Pool, libraries []library.Library, interval time.Duration, log *slog.Logger) {
	scanner := &library.Scanner{Pool: pool, Log: log}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for _, lib := range libraries {
			if err := scanner.Scan(ctx, lib); err != nil && ctx.Err() == nil {
				log.Error("scan failed", "library", lib.Name, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
