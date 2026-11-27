package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
)

// A running stream ends with its token (JS-033). Each stream replica keeps the requests it is answering, and asks
// Postgres every few seconds, in one statement for all of them, which of their tokens are still there. A sign-out, a
// password change, a delete and a disable all remove the token's row, so none needs a message to the replica, and the
// cost does not grow with the bytes sent.

// revokeEvery is how often a stream replica asks while it is answering anything.
const revokeEvery = 5 * time.Second

type openPlay struct {
	token string
	cut   func()
}

// openPlays is safe to lose: it holds only the requests this process is answering.
type openPlays struct {
	mu    sync.Mutex
	plays map[*openPlay]bool
}

func (o *openPlays) add(play *openPlay) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.plays == nil {
		o.plays = map[*openPlay]bool{}
	}
	o.plays[play] = true
}

func (o *openPlays) remove(play *openPlay) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.plays, play)
}

func (o *openPlays) tokens() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var tokens []string
	for play := range o.plays {
		tokens = append(tokens, play.token)
	}
	slices.Sort(tokens)
	return slices.Compact(tokens)
}

// cutEnded cuts the plays of the tokens that were asked about and are not held. It holds the lock while it does, so
// that no handler returns, and gives up its connection, in the middle of a cut.
func (o *openPlays) cutEnded(asked []string, held map[string]bool) (cut int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for play := range o.plays {
		if _, found := slices.BinarySearch(asked, play.token); !found || held[play.token] {
			continue
		}
		play.cut()
		delete(o.plays, play)
		cut++
	}
	return cut
}

// whileSignedIn runs a stream handler as a play that can be cut: its context ends, and the connection takes no more
// bytes, so a response in the middle of being sent stops short.
func (s *Server) whileSignedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		connection := http.NewResponseController(w)
		play := &openPlay{token: sessionOf(r).Token, cut: func() {
			connection.SetWriteDeadline(time.Unix(1, 0)) //nolint:errcheck // A writer with no connection has nothing to cut.
			cancel()
		}}
		s.plays.add(play)
		defer s.plays.remove(play)
		next(w, r.WithContext(ctx))
	}
}

// endRevokedPlays runs for as long as the pool lives. While Postgres is away nothing is known to have ended, and the
// plays go on (invariant 37).
func (s *Server) endRevokedPlays() {
	for {
		time.Sleep(revokeEvery)
		tokens := s.plays.tokens()
		if len(tokens) == 0 || db.Of(s.Pool).Away() {
			continue
		}
		held, err := s.heldTokens(tokens)
		if err != nil && strings.Contains(err.Error(), "closed pool") {
			return
		}
		if err != nil {
			s.Log.Warn("could not check that the tokens of running streams are still signed in", "error", err)
			continue
		}
		if cut := s.plays.cutEnded(tokens, held); cut > 0 {
			s.Log.Info("ended running streams whose token ended", "streams", cut)
		}
	}
}

func (s *Server) heldTokens(tokens []string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), revokeEvery)
	defer cancel()
	rows, err := s.Pool.Query(ctx, "SELECT token FROM devices WHERE token = ANY($1)", tokens)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	var token string
	_, err = pgx.ForEachRow(rows, []any{&token}, func() error {
		held[token] = true
		return nil
	})
	return held, err
}
