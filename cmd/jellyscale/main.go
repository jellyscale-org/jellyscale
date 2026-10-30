// Command jellyscale runs any combination of the server's roles. Production
// runs them as separate Deployments; --roles=all is for development.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/api"
	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
)

const roleWorker = "worker"

type config struct {
	Name          string           `json:"name"`
	AdminUser     string           `json:"adminUser"`
	AdminPassword string           `json:"adminPassword"`
	WebDir        string           `json:"webDir"`
	ScanInterval  string           `json:"scanInterval"`
	Libraries     []library.Config `json:"libraries"`
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

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	if err := run(*roles, *listen, *configPath, log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	if os.Getenv("JELLYSCALE_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
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
		return nil
	}

	server := &api.Server{
		Pool: pool, Items: &items.Repo{Pool: pool, ServerID: serverID}, ServerID: serverID,
		Name: cfg.Name, WebDir: cfg.WebDir, Roles: enabled, Log: log,
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
	return nil
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
