package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/compat"
	"jellyscale/internal/db"
	"jellyscale/internal/items"
)

// Invariant 37: while Postgres is away a stream replica goes on serving direct
// play, the segments in the transcode store, and the subtitle tracks and fonts in
// the asset store to a token that was seen within snapshotWindow, from a snapshot
// it refreshes every snapshotEvery and adds to when an api replica reports a
// start (startChannel). The numbers are in DOMAINS.md, which the tests read.
const (
	snapshotWindow = 10 * time.Minute
	snapshotEvery  = 5 * time.Second
)

// startChannel is where an api replica says, with the token as the payload, that
// a session has just started a film, so that the stream replicas learn of it
// between refreshes. The payload is a hint only: a replica reads the session
// from Postgres, which has accepted the token, and adds what it finds.
const startChannel = "jellyscale_session_started"

// offlineSession is what the snapshot knows of a token: the film its session plays.
type offlineSession struct {
	item  string // Compact, empty if the session plays nothing.
	films []*film
}

// film is one media source of the item, with what a replica reads from Postgres
// to play it. xmin is the row's version: streams and keyframes (megabytes for a
// long film) are read again only when it changes.
type film struct {
	source    items.Source
	xmin      string
	keyframes []int64
	videoEnd  int64
}

type snapshot struct {
	// reading is held from a read of Postgres to the write of what it said, so
	// that a refresh that began before a start was reported cannot replace the
	// snapshot after the start was added.
	reading  sync.Mutex
	mu       sync.RWMutex
	sessions map[string]*offlineSession
	films    map[string]*film // By media source id.
}

type snapshotKey struct{}

// snapshotOf is the session of the token when the snapshot, not Postgres, is
// standing in for it, else nil.
func snapshotOf(ctx context.Context) *offlineSession {
	session, _ := ctx.Value(snapshotKey{}).(*offlineSession)
	return session
}

// errWait is what a handler stops with, before it has sent anything, when the
// snapshot cannot decide the request.
var errWait = errors.New("waiting for Postgres")

// waitForPostgres stops a handler that runs from the snapshot, so that serveFromSnapshot
// leaves the request for Postgres, which decides it as it would have with no
// outage. It does nothing for a request Postgres is deciding. A refusal, a store
// failure, and a job to start or join are all for Postgres: the snapshot only
// answers what the stores hold.
func waitForPostgres(r *http.Request) {
	if snapshotOf(r.Context()) != nil {
		panic(errWait)
	}
}

// refuse answers as http.Error does, unless it runs from the snapshot, which
// never refuses a request: Postgres may know of a reason to answer otherwise.
func refuse(w http.ResponseWriter, r *http.Request, code int, text string) {
	waitForPostgres(r)
	http.Error(w, text, code)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	refuse(w, r, http.StatusNotFound, "404 page not found")
}

// serveFromSnapshot runs the handler of a request whose token the snapshot holds,
// with the snapshot in Postgres's place, and says whether it answered. A handler
// that waits sent nothing, and the headers it set are taken back.
func (s *Server) serveFromSnapshot(next http.HandlerFunc, w http.ResponseWriter, r *http.Request, info compat.ClientInfo) (answered bool) {
	s.snapshot.mu.RLock()
	session := s.snapshot.sessions[info.Token]
	s.snapshot.mu.RUnlock()
	if session == nil {
		return false
	}
	before := w.Header().Clone()
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered != errWait {
				panic(recovered)
			}
			clear(w.Header())
			maps.Copy(w.Header(), before)
			answered = false
		}
	}()
	ctx := context.WithValue(r.Context(), snapshotKey{}, session)
	ctx = context.WithValue(ctx, contextKey{}, Session{Token: info.Token, ClientInfo: info})
	next(w, r.WithContext(ctx))
	return true
}

// film is Items.Source for a request: from Postgres, as read says, or from the
// snapshot's session. A film the session does not play is not found, and the
// handler's refusal then waits for Postgres.
func (s *Server) film(r *http.Request, itemID, sourceID string, read func(context.Context, string, string) (items.Source, bool, error)) (items.Source, bool, error) {
	if session := snapshotOf(r.Context()); session != nil {
		f, ok := session.pick(itemID, sourceID)
		if !ok {
			return items.Source{}, false, nil
		}
		return f.source, true, nil
	}
	return read(r.Context(), itemID, sourceID)
}

// keyframes reads the keyframe index of a source, which only a copy asks for.
func (s *Server) keyframes(r *http.Request, itemID string, source items.Source) (keyframes []int64, end int64, err error) {
	if session := snapshotOf(r.Context()); session != nil {
		f, _ := session.pick(itemID, source.ID) // The source came from this session's films.
		return f.keyframes, f.videoEnd, nil
	}
	err = db.Retry(r.Context(), s.Pool, func() (err error) {
		keyframes, end, err = s.Items.Keyframes(r.Context(), source.ID)
		return
	})
	return
}

// pick is items.Source for the session's film: the named source, else the first.
func (o offlineSession) pick(itemID, sourceID string) (*film, bool) {
	if o.item == "" || o.item != compact(itemID) {
		return nil, false
	}
	for _, f := range o.films {
		if sourceID == "" || compact(f.source.ID) == compact(sourceID) {
			return f, true
		}
	}
	return nil, false
}

func compact(id string) string { return strings.ToLower(db.CompactID(id)) }

// keepSnapshot refreshes the snapshot for as long as the pool lives, which is the
// life of the process: a stream replica holds no other state.
func (s *Server) keepSnapshot() {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go s.listenForStarts(ctx)
	for {
		if !db.Of(s.Pool).Away() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*snapshotEvery)
			err := s.refreshSnapshot(ctx)
			cancel()
			switch {
			case err == nil:
			case strings.Contains(err.Error(), "closed pool"):
				return
			case db.IsAway(err) || errors.Is(err, context.DeadlineExceeded):
				// Postgres is away or not answering: what it last said is all there is.
			default:
				// Not knowing is not the same as the outage the owner accepted
				// stale tokens for: keep nothing.
				s.Log.Error("refreshing the snapshot of active sessions failed", "error", err)
				s.snapshot.replace(nil, nil)
			}
		}
		time.Sleep(snapshotEvery)
	}
}

// refreshSnapshot reads the sessions seen within the window, and the sources of
// the films they play. A source's streams and keyframes are read only when the
// replica holds none or its row changed.
func (s *Server) refreshSnapshot(ctx context.Context) error {
	s.snapshot.reading.Lock()
	defer s.snapshot.reading.Unlock()
	rows, err := s.Pool.Query(ctx, `SELECT token, COALESCE(item_id::text, '')
		FROM sessions
		WHERE last_seen_at > now() - make_interval(secs => $1)`, snapshotWindow.Seconds())
	if err != nil {
		return err
	}
	sessions := map[string]*offlineSession{}
	var itemIDs []string
	for rows.Next() {
		var token, item string
		if err := rows.Scan(&token, &item); err != nil {
			rows.Close()
			return err
		}
		sessions[token] = &offlineSession{item: compact(item)}
		if item != "" {
			itemIDs = append(itemIDs, item)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	films, err := s.refreshFilms(ctx, itemIDs)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		session.films = filmsOf(films, session.item)
	}
	byID := map[string]*film{}
	for _, f := range films {
		byID[f.film.source.ID] = f.film
	}
	s.snapshot.replace(sessions, byID)
	return nil
}

// filmsOf is the films of an item, which is compact.
func filmsOf(films []itemFilm, item string) []*film {
	var found []*film
	for _, f := range films {
		if compact(f.itemID) == item {
			found = append(found, f.film)
		}
	}
	return found
}

// listenForStarts adds a session to the snapshot when an api replica says it
// started a film, until ctx ends. A notification is lost while the connection is
// down, and the next refresh then has the session instead. The connection is its
// own, not the pool's, so that closing the pool does not wait for it.
func (s *Server) listenForStarts(ctx context.Context) {
	for ctx.Err() == nil {
		s.listenOnce(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) listenOnce(ctx context.Context) {
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		return
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+startChannel); err != nil {
		return
	}
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		if db.Of(s.Pool).Away() {
			continue
		}
		read, cancel := context.WithTimeout(ctx, 2*snapshotEvery)
		s.addStarted(read, notification.Payload)
		cancel()
	}
}

// addStarted reads the session of token, if Postgres has one seen within the
// window, and the sources of the film it plays, and sets it in the snapshot.
// What it cannot read waits for the next refresh.
func (s *Server) addStarted(ctx context.Context, token string) {
	s.snapshot.reading.Lock()
	defer s.snapshot.reading.Unlock()
	var item string
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(item_id::text, '') FROM sessions
		WHERE token = $1 AND last_seen_at > now() - make_interval(secs => $2)`, token, snapshotWindow.Seconds()).Scan(&item)
	if err != nil {
		return
	}
	session := &offlineSession{item: compact(item)}
	var films []itemFilm
	if item != "" {
		if films, err = s.refreshFilms(ctx, []string{item}); err != nil {
			return
		}
		session.films = filmsOf(films, session.item)
	}
	s.snapshot.mu.Lock()
	defer s.snapshot.mu.Unlock()
	if s.snapshot.sessions == nil {
		s.snapshot.sessions, s.snapshot.films = map[string]*offlineSession{}, map[string]*film{}
	}
	s.snapshot.sessions[token] = session
	for _, f := range films {
		s.snapshot.films[f.film.source.ID] = f.film
	}
}

type itemFilm struct {
	itemID string
	film   *film
}

func (s *Server) refreshFilms(ctx context.Context, itemIDs []string) ([]itemFilm, error) {
	rows, err := s.Pool.Query(ctx, `SELECT m.id::text, m.item_id::text, m.store_key, m.object_etag, m.container, l.media_store_uri, l.asset_store_uri,
			m.size, COALESCE(i.run_time_ticks, 0), m.xmin::text
		FROM media_sources m JOIN shown_items i ON i.id = m.item_id JOIN libraries l ON l.id = i.library_id
		WHERE m.item_id = ANY($1::uuid[]) ORDER BY m.id`, itemIDs)
	if err != nil {
		return nil, err
	}
	s.snapshot.mu.RLock()
	held := s.snapshot.films
	s.snapshot.mu.RUnlock()
	var found []itemFilm
	var unread []string
	for rows.Next() {
		var f film
		var itemID string
		if err := rows.Scan(&f.source.ID, &itemID, &f.source.Key, &f.source.ETag, &f.source.Container, &f.source.StoreURI, &f.source.AssetStoreURI,
			&f.source.Size, &f.source.RunTimeTicks, &f.xmin); err != nil {
			rows.Close()
			return nil, err
		}
		if old := held[f.source.ID]; old != nil && old.xmin == f.xmin {
			f.source.Streams, f.keyframes, f.videoEnd = old.source.Streams, old.keyframes, old.videoEnd
		} else {
			unread = append(unread, f.source.ID)
		}
		found = append(found, itemFilm{itemID, &f})
	}
	if err := rows.Err(); err != nil || len(unread) == 0 {
		return found, err
	}
	rows, err = s.Pool.Query(ctx, "SELECT id::text, streams, keyframes, video_end FROM media_sources WHERE id = ANY($1::uuid[])", unread)
	if err != nil {
		return nil, err
	}
	read := map[string]*film{}
	for rows.Next() {
		var id string
		var f film
		if err := rows.Scan(&id, &f.source.Streams, &f.keyframes, &f.videoEnd); err != nil {
			rows.Close()
			return nil, err
		}
		read[id] = &f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range found {
		if r := read[f.film.source.ID]; r != nil {
			f.film.source.Streams, f.film.keyframes, f.film.videoEnd = r.source.Streams, r.keyframes, r.videoEnd
		}
	}
	return found, nil
}

func (sn *snapshot) replace(sessions map[string]*offlineSession, films map[string]*film) {
	sn.mu.Lock()
	sn.sessions, sn.films = sessions, films
	sn.mu.Unlock()
}
