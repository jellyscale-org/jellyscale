// Package api serves the Jellyfin client API.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/compat"
	"jellyscale/internal/contract"
	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/signin"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

// Version is the Jellyfin API baseline clients see. It moves only after the
// contract check passes against that release.
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

// Access is who may call a route.
type Access string

const (
	// AccessAnyone is open to a caller with no token. The route says Why.
	AccessAnyone Access = "anyone"
	// AccessViewer needs a token that Postgres knows.
	AccessViewer Access = "viewer"
	// AccessAdmin needs an administrator's token.
	AccessAdmin Access = "admin"
)

// UserScope says whose account a viewer route acts on. Its zero value is own.
type UserScope string

const (
	UsersOwn      UserScope = "own"
	UsersAdminAny UserScope = "admin-any"
)

// Route is one API operation.
type Route struct {
	Method string
	Path   string
	Status string
	Role   string
	// Access says who may call the route: AccessAnyone (and then Why says why),
	// AccessViewer or AccessAdmin. Every route declares it; docs/access.md is the model.
	Access Access
	// Users is own by default, or admin-any on the routes an administrator may
	// use for another account. It is empty on an anyone route.
	Users UserScope
	// UserIDBody says the body may name a user under UserId, so the scope guard
	// reads it before the handler and leaves it whole for the handler to read.
	UserIDBody bool
	Why        string
	Handler    http.HandlerFunc
	// Snapshot says the handler may run from the snapshot of active sessions
	// while Postgres is away (invariant 37), for a token it holds.
	Snapshot bool
}

func (r Route) userScope() UserScope {
	if r.Users == "" {
		return UsersOwn
	}
	return r.Users
}

// Server holds what handlers need.
type Server struct {
	Pool     *pgxpool.Pool
	Items    *items.Repo
	ServerID string
	Name     string
	// AdminUser is the configured administrator, which no client may delete or
	// rename while it is named (D-095); "" is none.
	AdminUser string
	WebDir    string
	Roles     []string
	Log       *slog.Logger
	// Transcode is nil when no transcode store is configured, and then every
	// source plays as it is.
	Transcode *transcode.Jobs
	// Hub wakes requests waiting on a subtitle extraction; nil means they poll.
	Hub *transcode.Hub
	// Proxies are the proxies in front of the server, from trustedProxies; nil when the key is absent, and then no address
	// is counted toward a sign-in bound (docs/access.md).
	Proxies *signin.Proxies
	// QuickConnectOff is this replica's "quickConnect": false. The zero value is on, as Jellyfin 12.4's default.
	QuickConnectOff bool
	// AuditLog writes the Quick Connect refusals as structured records, so an operator can count them by bound or cap
	// and by the account that was refused. The ordinary Log stays in the human-readable format.
	AuditLog *slog.Logger
	// SocketIdle closes a WebSocket that sends nothing for this long.
	// Defaults to 60 seconds, the interval ForceKeepAlive promises.
	SocketIdle time.Duration
	// Contract is the document this replica publishes and checks its routes against. Nil means the embedded one.
	Contract *contract.Contract

	stores           store.Cache
	servedPatterns   []string
	snapshot         snapshot
	snapshotOnce     sync.Once
	replicaOnce      sync.Once
	forwardedOnce    sync.Once // The one log line about an ingress that is not declared.
	unattributedOnce sync.Once // The one log line about a sign-in through a declared proxy with no usable forwarded address.
	replicaID        string

	draining atomic.Bool // Set once the process has been told to stop: readiness fails.
	// sockets counts the WebSocket handlers still running, and closeSockets, once
	// closed, tells them to end.
	sockets      sync.WaitGroup
	socketsOnce  sync.Once
	closeSockets chan struct{}
	// plays are the stream requests in flight, which end when their token does.
	plays openPlays
	// shutdownCut ends, through CutApprovals, what outlives its request when the shutdown grace does.
	shutdownOnce  sync.Once
	shutdownCut   context.Context
	cutAtShutdown context.CancelFunc
}

// postgresAwayLimit is how long Postgres may be away without a break before
// readiness fails: a switchover is over by then, so a replica that is still cut
// off is the one to stop sending to. Several shorter outages never add up to it.
const postgresAwayLimit = 30 * time.Second

// BeginDrain makes /readyz answer 503 from now on. Requests are still served.
func (s *Server) BeginDrain() { s.draining.Store(true) }

// ServedPatterns are the method and path patterns Handler registered, including any a route table does not name.
func (s *Server) ServedPatterns() []string { return slices.Clone(s.servedPatterns) }

func (s *Server) ready() bool {
	return !s.draining.Load() && db.Of(s.Pool).AwayFor() < postgresAwayLimit
}

type contextKey struct{}

// Session is the authenticated caller.
type Session struct {
	User User
	// Target is the account this request acts on. It is the caller except on
	// an admin-any route where an administrator named another account.
	Target User
	Token  string
	compat.ClientInfo
}

func sessionOf(r *http.Request) Session {
	session, _ := r.Context().Value(contextKey{}).(Session)
	return session
}

// Handler builds the HTTP handler for this server's roles.
func (s *Server) Handler() http.Handler {
	db.Of(s.Pool).SetLog(s.Log)                                // Where it says Postgres went away, and came back.
	if s.Pool != nil && slices.Contains(s.Roles, RoleStream) { // Handler tests of routes run with no database.
		s.snapshotOnce.Do(func() {
			go s.keepSnapshot()
			go s.endRevokedPlays()
		})
	}
	if s.Proxies != nil {
		signinAddressBound.Set(1)
	} else {
		signinAddressBound.Set(0)
	}
	s.checkContract()
	mux := http.NewServeMux()
	patterns := []string{}
	// Whichever roles this server has, a route nobody decided about stops it.
	for _, route := range s.Routes() {
		if route.Access != AccessAnyone && route.Access != AccessViewer && route.Access != AccessAdmin {
			panic(fmt.Sprintf("route %s %s declares Access %q, want %q, %q or %q", route.Method, route.Path, route.Access, AccessAnyone, AccessViewer, AccessAdmin))
		}
		if route.Access == AccessAnyone {
			if route.Users != "" {
				panic(fmt.Sprintf("open route %s %s declares user scope %q", route.Method, route.Path, route.Users))
			}
			continue
		}
		if route.userScope() != UsersOwn && route.userScope() != UsersAdminAny {
			panic(fmt.Sprintf("route %s %s declares Users %q, want %q or %q", route.Method, route.Path, route.Users, UsersOwn, UsersAdminAny))
		}
	}
	for _, route := range s.Routes() {
		if !slices.Contains(s.Roles, route.Role) {
			continue
		}
		handler := route.Handler
		if route.Access != AccessAnyone {
			if route.Role == RoleStream {
				handler = s.whileSignedIn(handler)
			}
			handler = s.requireAuth(handler, route)
		}
		pattern := route.Method + " " + lowerLiterals(route.Path)
		mux.HandleFunc(pattern, handler)
		patterns = append(patterns, pattern)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// The process alone: a probe that failed for Postgres being away would
		// take every replica out of service, or restart them, at once.
		w.WriteHeader(http.StatusNoContent)
	})
	// Readiness is for the router: whether to send this replica requests. Unlike
	// liveness it fails in a drain, and when Postgres has been away for good.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok")) //nolint:errcheck
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
	patterns = append(patterns, "GET /healthz", "GET /readyz", "/")
	if s.WebDir != "" {
		patterns = append(patterns, "GET /web/", "GET /{$}")
	}
	s.servedPatterns = patterns
	var next http.Handler = mux
	if slices.Contains(s.Roles, RoleAPI) {
		next = s.v1Dispatch(mux)
	}
	return s.logRequests(s.noteUndeclaredIngress(normalize(cors(mux, next))))
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

// Unwrap lets the WebSocket upgrade find the real connection to hijack.
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

func (s *Server) requireAuth(next http.HandlerFunc, route Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info := compat.ParseAuth(r)
		if info.Token == "" {
			s.refuseV1(w, r, http.StatusUnauthorized, reasonUnauthorized, "unauthorized")
			return
		}
		// Postgres decides, unless it is away and the route can go on from the snapshot.
		var user User
		var err error
		away := db.Of(s.Pool).Away()
		if !away {
			user, err = s.queryUser(r.Context(), info.Token)
			away = db.IsAway(err)
		}
		if away {
			if route.Snapshot && s.serveFromSnapshot(next, route, w, r, info) {
				return
			}
			user, err = s.userByToken(r.Context(), info.Token)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			s.refuseV1(w, r, http.StatusUnauthorized, reasonUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		// Before any id the request names is looked at.
		if route.Access == AccessAdmin && !user.IsAdmin {
			s.refuseV1(w, r, http.StatusForbidden, reasonForbidden, "forbidden")
			return
		}
		session := Session{User: user, Token: info.Token, ClientInfo: info}
		if !s.requireUserScope(w, r, route, &session) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, session)))
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	waitForPostgres(r) // From the snapshot, a failure is not an answer.
	if r.Context().Err() != nil {
		return // The client gave up, as a player does on a request that waits for Postgres; nobody is left to hear.
	}
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	s.refuseV1(w, r, http.StatusInternalServerError, reasonServerFailure, "internal error")
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

// replica returns this process's ID for sessions.ws_replica.
func (s *Server) replica() string {
	s.replicaOnce.Do(func() { s.replicaID = randomHex(6) })
	return s.replicaID
}

// randomHex returns n random bytes as hex. crypto/rand.Read never fails.
func randomHex(n int) string {
	random := make([]byte, n)
	rand.Read(random) //nolint:errcheck
	return hex.EncodeToString(random)
}

// noteUndeclaredIngress logs, once per process, a forwarded header that arrives from a private or loopback peer when no
// proxy is declared: likely an ingress the operator forgot to declare. Any request carries one, not only a sign-in.
func (s *Server) noteUndeclaredIngress(next http.Handler) http.Handler {
	if s.Proxies == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Proxies.UndeclaredIngress(r) {
			s.forwardedOnce.Do(func() {
				s.Log.Warn("forwarded header from an undeclared proxy", "peer", signin.Peer(r),
					"hint", "if an ingress sits in front, declare it in trustedProxies (docs/running.md)")
			})
		}
		next.ServeHTTP(w, r)
	})
}
