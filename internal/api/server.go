// Package api serves the Jellyfin client API.
package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/compat"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// Version is the Jellyfin API baseline clients see. It moves only after the
// differential suite passes against that release.
const Version = "12.4.0"

// Route statuses, recorded in compat/endpoints.yaml.
const (
	Implemented = "implemented"
	// Stub answers with a valid empty response, for features outside scope or
	// not built yet.
	Stub = "stub"
	// Alias is an obsolete route kept only because jellyfin-web still calls it.
	// Remove each one when the web client stops.
	Alias = "alias"
)

// Server roles select which routes a replica serves.
const (
	RoleAPI    = "api"
	RoleStream = "stream"
)

// Route is one API operation.
type Route struct {
	Method  string
	Path    string
	Status  string
	Role    string
	Public  bool
	Handler http.HandlerFunc
}

// Server holds what handlers need.
type Server struct {
	Pool     *pgxpool.Pool
	Items    *items.Repo
	ServerID string
	Name     string
	WebDir   string
	Roles    []string
	Log      *slog.Logger
	// ReplicaID names this process in sessions.ws_replica. Random if empty.
	ReplicaID string
	// SocketIdle closes a WebSocket that sends nothing for this long.
	// Defaults to 60 seconds, the interval ForceKeepAlive promises.
	SocketIdle time.Duration

	stores    sync.Map // store URI -> store.Store
	replicaID sync.Once
}

type contextKey struct{}

// Session is the authenticated caller.
type Session struct {
	User  User
	Token string
	compat.ClientInfo
}

func sessionOf(r *http.Request) Session {
	session, _ := r.Context().Value(contextKey{}).(Session)
	return session
}

// Handler builds the HTTP handler for this server's roles.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range s.Routes() {
		if !slices.Contains(s.Roles, route.Role) {
			continue
		}
		handler := route.Handler
		if !route.Public {
			handler = s.requireAuth(handler)
		}
		mux.HandleFunc(route.Method+" "+lowerLiterals(route.Path), handler)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Pool.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if s.WebDir != "" {
		web := http.StripPrefix("/web/", http.FileServer(http.Dir(s.WebDir)))
		mux.Handle("GET /web/", web)
		mux.Handle("GET /{$}", http.RedirectHandler("/web/", http.StatusFound))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The backlog of routes clients really call comes from this log.
		s.Log.Warn("unknown route", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	})
	return s.logRequests(normalize(mux))
}

// lowerLiterals lowercases a route's fixed segments and leaves {wildcards}
// alone, because handlers read those by name.
func lowerLiterals(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if !strings.HasPrefix(segment, "{") {
			segments[i] = strings.ToLower(segment)
		}
	}
	return strings.Join(segments, "/")
}

// normalize makes routing case-insensitive, as clients assume: captures show
// /users/public beside /Users/AuthenticateByName, and Static beside static.
// Lowercasing here means nothing downstream has to care.
func normalize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/web" {
			http.Redirect(w, r, "/web/", http.StatusFound)
			return
		}
		// File names under /web are case-sensitive.
		if !strings.HasPrefix(r.URL.Path, "/web/") {
			r.URL.Path = strings.ToLower(r.URL.Path)
			if len(r.URL.Path) > 1 {
				r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
			}
			query := url.Values{}
			for key, values := range r.URL.Query() {
				query[strings.ToLower(key)] = append(query[strings.ToLower(key)], values...)
			}
			r.URL.RawQuery = query.Encode()
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Hijack lets the WebSocket upgrade take the connection.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Flush() {
	http.NewResponseController(w.ResponseWriter).Flush() //nolint:errcheck
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		writer := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(writer, r)
		if !strings.HasPrefix(r.URL.Path, "/web/") {
			s.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", writer.status, "elapsed", time.Since(start))
		}
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := compat.ParseAuth(r)
		if info.Token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		user, err := s.userByToken(r.Context(), info.Token)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		session := Session{User: user, Token: info.Token, ClientInfo: info}
		next(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, session)))
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(value) //nolint:errcheck
}

func queryList(r *http.Request, key string) []string {
	var values []string
	for _, value := range r.URL.Query()[key] {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
	}
	return values
}

func queryInt(r *http.Request, key string) int {
	value, _ := strconv.Atoi(r.URL.Query().Get(key))
	return max(value, 0)
}

func queryBool(r *http.Request, key string) *bool {
	value, err := strconv.ParseBool(r.URL.Query().Get(key))
	if err != nil {
		return nil
	}
	return &value
}

// storeFor returns the store a stored URI names, opening it once per process.
// Stores hold HTTP clients and connection pools, so reopening per request would
// throw away every warm connection.
func (s *Server) storeFor(uri string) (store.Store, error) {
	if cached, ok := s.stores.Load(uri); ok {
		return cached.(store.Store), nil
	}
	opened, err := store.Open(uri)
	if err != nil {
		return nil, err
	}
	cached, _ := s.stores.LoadOrStore(uri, opened)
	return cached.(store.Store), nil
}

// replica returns this process's ID for sessions.ws_replica.
func (s *Server) replica() string {
	s.replicaID.Do(func() {
		if s.ReplicaID == "" {
			random := make([]byte, 6)
			rand.Read(random) //nolint:errcheck
			s.ReplicaID = hex.EncodeToString(random)
		}
	})
	return s.ReplicaID
}
