// Command jellyscale runs any combination of the server's roles. Production
// runs them as separate Deployments; --roles=all is for development.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/prometheus/client_golang/prometheus"

	"jellyscale/internal/api"
	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/metrics"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

const roleWorker = "worker"

type config struct {
	Name          string `json:"name"`
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`
	WebDir        string `json:"webDir"`
	ScanInterval  string `json:"scanInterval"`
	// MissingGrace is how long a file or library that is gone keeps its watch
	// state, hidden, before it is removed (a Go duration, default 168h).
	MissingGrace string `json:"missingGrace"`
	// MaxLostShare is the most of a library's files a scan may find missing
	// before it fails and changes nothing (0 to 1, default 0.2).
	MaxLostShare *float64         `json:"maxLostShare"`
	Libraries    []library.Config `json:"libraries"`

	// TranscodeStore is where transcoded segments live, a store URI such as
	// file:///var/lib/jellyscale/segments or s3://bucket/segments?endpoint=...
	// Every role needs it; without it nothing transcodes.
	TranscodeStore string `json:"transcodeStore"`
	// TranscodeScratch is where a worker's ffmpeg writes before uploading.
	TranscodeScratch string `json:"transcodeScratch"`
	// TranscodeSlots bounds concurrent transcode jobs per worker, paused ones
	// included.
	TranscodeSlots int `json:"transcodeSlots"`
	// TranscodeIdle is how long a spec may go without a request before its
	// ffmpeg stops, and TranscodeRetention how long its segments stay after.
	TranscodeIdle      string `json:"transcodeIdle"`
	TranscodeRetention string `json:"transcodeRetention"`
	// TranscodeLead is how far, in film time, a job may get ahead of the
	// furthest segment a viewer has asked for before its ffmpeg pauses.
	TranscodeLead string `json:"transcodeLead"`

	// TranscodeHelpers is false to stop slow jobs from asking other workers to
	// help; nil means on.
	TranscodeHelpers *bool `json:"transcodeHelpers"`

	// DatabaseMaxConns is the most Postgres connections the process opens, for
	// every role in it together. Zero means db.DefaultMaxConns.
	DatabaseMaxConns int `json:"databaseMaxConns"`

	// ShutdownGrace is how long a replica that got SIGTERM takes to stop, counted
	// from the signal (a Go duration, default 30s): readiness fails at once, new
	// requests are still taken for drainLead, and what is in flight is then
	// given the rest. Set the pod's termination grace period above it.
	ShutdownGrace string `json:"shutdownGrace"`
	grace         time.Duration

	// MetricsListen is where /metrics is served, such as ":9090", by every role
	// in the process and apart from the public port. Empty serves none.
	MetricsListen string `json:"metricsListen"`
}

// parseConfig refuses a key it doesn't know, so a mistyped option is an error
// at startup and not an option silently left off.
func parseConfig(raw []byte) (config, error) {
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	// Anything after the config, such as a second object pasted in, is refused too.
	if _, err := decoder.Token(); err != io.EOF {
		return cfg, errors.New("unexpected data after the config")
	}
	// The hint listener holds one connection for good, so a pool of 1 leaves
	// nothing for queries and the process hangs; the cap keeps the int32 pgx
	// takes from wrapping.
	if n := cfg.DatabaseMaxConns; n < 0 || n == 1 || n > maxDatabaseConns {
		return cfg, fmt.Errorf("databaseMaxConns must be 0 (the default, %d) or 2 to %d, not %d", db.DefaultMaxConns, maxDatabaseConns, n)
	}
	cfg.grace = defaultShutdownGrace
	if cfg.ShutdownGrace != "" {
		grace, err := time.ParseDuration(cfg.ShutdownGrace)
		if err != nil || grace < 0 {
			return cfg, fmt.Errorf("shutdownGrace: want a duration such as 30s, not %q", cfg.ShutdownGrace)
		}
		cfg.grace = grace
	}
	return cfg, nil
}

const (
	defaultShutdownGrace = 30 * time.Second
	// drainLead is how long, after readiness first answers 503, a replica
	// still takes new requests, so that a router polling every second or two has
	// stopped sending before any is refused. It is part of the grace.
	drainLead = 5 * time.Second
)

// maxDatabaseConns is far above any Postgres max_connections worth running.
const maxDatabaseConns = 10000

// openDatabase opens the pool every role in the process shares.
func openDatabase(ctx context.Context, dsn string, cfg config) (*pgxpool.Pool, error) {
	if cfg.DatabaseMaxConns == 0 {
		return db.Open(ctx, dsn)
	}
	return db.Open(ctx, dsn, db.WithMaxConns(int32(cfg.DatabaseMaxConns)))
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
	signalled, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// What the process runs (the hub, the worker, its registration) outlives the
	// signal while requests drain, and ends after them.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var cfg config
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		if cfg, err = parseConfig(raw); err != nil {
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

	pool, err := openDatabase(ctx, dsn, cfg)
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
	// Every role reads the keys, so a bad one stops every start.
	scanner, err := newScanner(cfg, pool, log)
	if err != nil {
		return err
	}
	enabled := parseRoles(roleList)
	// Only a worker uses the libraries, so only a worker writes them (a bad
	// library entry stops only a worker); any other role's list would hide
	// every library it leaves out from every viewer. A worker listing none
	// changes none, so emptying the list does not remove the last library.
	var libraries []library.Library
	if slices.Contains(enabled, roleWorker) {
		if len(cfg.Libraries) == 0 {
			log.Info("config lists no libraries; the libraries in the database were kept")
		} else if libraries, err = library.Sync(ctx, pool, cfg.Libraries); err != nil {
			return err
		}
	}
	transcodes, err := openTranscoding(ctx, cfg, enabled, pool, serverID, log)
	if err != nil {
		return err
	}
	if cfg.MetricsListen != "" {
		// Before the worker-only return below: a worker is where most of them are.
		collectors := append([]prometheus.Collector{metrics.Pool{Pool: pool}, scanDuration, scanOK, scanTime}, api.Collectors()...)
		collectors = append(collectors, transcode.Collectors()...)
		collectors = append(collectors, transcodes.collectors...)
		if _, err := metrics.Serve(ctx, cfg.MetricsListen, log, collectors...); err != nil {
			return fmt.Errorf("metricsListen: %w", err)
		}
	}
	if len(libraries) > 0 {
		interval := 15 * time.Minute
		if cfg.ScanInterval != "" {
			if interval, err = time.ParseDuration(cfg.ScanInterval); err != nil {
				return fmt.Errorf("scanInterval: %w", err)
			}
		}
		go scanLoop(ctx, scanner, libraries, interval, log)
	}
	if !slices.Contains(enabled, api.RoleAPI) && !slices.Contains(enabled, api.RoleStream) {
		<-signalled.Done()
		cancel()
		<-transcodes.stopped
		return nil
	}

	server := &api.Server{
		Pool: pool, Items: &items.Repo{Pool: pool, ServerID: serverID}, ServerID: serverID,
		Name: cfg.Name, WebDir: cfg.WebDir, Roles: enabled, Log: log, Transcode: transcodes.jobs, Hub: transcodes.hub,
	}
	if slices.Contains(enabled, api.RoleAPI) {
		go server.KeepReplica(ctx)
	}
	httpServer := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-signalled.Done()
		drain(httpServer, server, cfg.grace, log)
	}()
	log.Info("listening", "addr", listen, "roles", enabled)
	if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-drained
	cancel()
	<-transcodes.stopped // ffmpeg is gone before we exit.
	return nil
}

// drain stops serving within grace of the signal. Readiness fails first and
// requests are taken as before for drainLead, so the router has stopped
// sending when the listener closes. Then the sockets are closed with their rows
// removed, and the responses in flight are sent whole if they end in time, and
// cut at the end of the grace if not.
func drain(httpServer *http.Server, server *api.Server, grace time.Duration, log *slog.Logger) {
	deadline, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	server.BeginDrain()
	log.Info("stopping: readiness fails, and requests are taken for a moment longer", "grace", grace)
	select {
	case <-time.After(drainLead):
	case <-deadline.Done():
	}
	socketsClosed := make(chan struct{})
	go func() {
		server.CloseSockets(deadline)
		close(socketsClosed)
	}()
	if err := httpServer.Shutdown(deadline); err != nil {
		log.Warn("responses still in flight at the end of the grace are cut", "error", err)
		httpServer.Close() //nolint:errcheck
	}
	<-socketsClosed
}

// transcoding is what this process runs for transcodes.
type transcoding struct {
	jobs *transcode.Jobs // nil when transcoding is off
	hub  *transcode.Hub  // nil unless this process waits for hints
	// stopped closes once the process has left transcode_nodes and the worker,
	// if any, has ended every ffmpeg.
	stopped chan struct{}
	// collectors report on what this process runs.
	collectors []prometheus.Collector
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
	var segments store.Store
	if cfg.TranscodeStore != "" {
		var err error
		if segments, err = store.Open(cfg.TranscodeStore); err != nil {
			return result, fmt.Errorf("transcodeStore: %w", err)
		}
	} else if len(roles) == 1 && worker {
		log.Info("transcodeStore is not set: this worker extracts subtitles and runs no transcodes")
	} else {
		log.Warn("transcodeStore is not set: this process cannot run, serve or offer transcodes, so set it, to the same value, on every role or none")
	}
	idle, err := optionalDuration(cfg.TranscodeIdle, "transcodeIdle")
	if err != nil {
		return result, err
	}
	lead, err := optionalDuration(cfg.TranscodeLead, "transcodeLead")
	if err != nil {
		return result, err
	}
	retention, err := optionalDuration(cfg.TranscodeRetention, "transcodeRetention")
	if err != nil {
		return result, err
	}

	id := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	if worker {
		// The id is what transcode_nodes and transcode_jobs name a worker by; the
		// hostname (a pod's name) says where it runs.
		host, _ := os.Hostname()
		log.Info("worker started", "worker", id, "host", host)
	}
	slots := 0
	if worker && segments != nil {
		slots = cmp.Or(cfg.TranscodeSlots, 2)
	}
	var running sync.WaitGroup
	// Called once nothing more will be added to running.
	finish := func() {
		go func() {
			running.Wait()
			close(result.stopped)
		}()
	}
	// Only roles that wait for hints listen for them.
	if slices.Contains(roles, api.RoleStream) || worker {
		result.hub = &transcode.Hub{Pool: pool, Log: log}
		go result.hub.Run(ctx)
	}
	scratch := cfg.TranscodeScratch
	if scratch == "" {
		scratch = filepath.Join(os.TempDir(), "jellyscale-transcode")
	}
	var media store.Cache
	w := &transcode.Worker{
		ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: serverID}, Segments: segments, Hub: result.hub, Log: log,
		Media: media.Open, Scratch: scratch, Slots: slots, Idle: idle, Lead: lead, Retention: retention,
		NoHelpers: cfg.TranscodeHelpers != nil && !*cfg.TranscodeHelpers,
	}
	if worker {
		result.collectors = []prometheus.Collector{w}
	}
	// A worker proves it is alive with a lock of its own, so that a replica
	// notices its death in a second; the others need no more than the row.
	running.Add(1)
	go func() {
		defer running.Done()
		if worker {
			w.Present(ctx, strings.Join(roles, ","), cfg.TranscodeStore, 3*time.Second)
		} else {
			transcode.Register(ctx, pool, log, id, strings.Join(roles, ","), cfg.TranscodeStore, slots, 3*time.Second)
		}
	}()
	if worker {
		// Subtitle extraction needs no transcode store.
		running.Add(1)
		go func() {
			defer running.Done()
			w.RunSubtitles(ctx)
		}()
		running.Add(1)
		go func() {
			defer running.Done()
			w.RunChapterImages(ctx)
		}()
	}
	if segments == nil {
		finish()
		return result, nil
	}
	result.jobs = &transcode.Jobs{Pool: pool, Segments: segments, Hub: result.hub, Log: log}
	if !worker {
		finish()
		return result, nil
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

// The last scan this process ran of each library, by library ID. Only the
// replica that wins a library's lock has its series, and a replica that loses
// it leaves its series as they were: scanTime says how old they are. The
// libraries are fixed at startup, so no series outlives its library.
var (
	scanTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jellyscale_scan_last_timestamp_seconds", Help: "When this process's last scan of the library ended, in seconds since the epoch."}, []string{"library"})
	scanDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jellyscale_scan_last_duration_seconds", Help: "How long this process's last scan of the library took."}, []string{"library"})
	scanOK = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jellyscale_scan_last_success", Help: "1 if this process's last scan of the library succeeded, 0 if it failed."}, []string{"library"})
)

// newScanner reads the scan's grace and loss share from the config.
func newScanner(cfg config, pool *pgxpool.Pool, log *slog.Logger) (*library.Scanner, error) {
	scanner := &library.Scanner{Pool: pool, Log: log, Grace: library.DefaultGrace, MaxLostShare: library.DefaultMaxLostShare}
	if cfg.MissingGrace != "" {
		grace, err := time.ParseDuration(cfg.MissingGrace)
		if err != nil || grace < 0 {
			return nil, fmt.Errorf("missingGrace: want a duration such as 72h, not %q", cfg.MissingGrace)
		}
		scanner.Grace = grace
	}
	if cfg.MaxLostShare != nil {
		if share := *cfg.MaxLostShare; !(share >= 0 && share <= 1) {
			return nil, fmt.Errorf("maxLostShare: want a number from 0 to 1, not %v", share)
		}
		scanner.MaxLostShare = *cfg.MaxLostShare
	}
	return scanner, nil
}

func scanLoop(ctx context.Context, scanner *library.Scanner, libraries []library.Library, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := library.RemoveGone(ctx, scanner.Pool, scanner.Grace); err != nil && ctx.Err() == nil {
			log.Error("removing the libraries left out of config failed", "error", err)
		}
		for _, lib := range libraries {
			began := time.Now()
			err := scanner.Scan(ctx, lib)
			if errors.Is(err, library.ErrElsewhere) || ctx.Err() != nil {
				continue // Another replica's scan is the one to report.
			}
			scanDuration.WithLabelValues(lib.ID).Set(time.Since(began).Seconds())
			scanTime.WithLabelValues(lib.ID).SetToCurrentTime()
			result := 1.0
			if err != nil {
				log.Error("scan failed", "library", lib.Name, "error", err)
				result = 0
			}
			scanOK.WithLabelValues(lib.ID).Set(result)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
