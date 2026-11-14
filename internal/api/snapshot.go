package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
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

// offlineHandler answers a request for a session in the snapshot, and says
// whether it did; if not, the request waits for Postgres as usual.
type offlineHandler func(w http.ResponseWriter, r *http.Request, session offlineSession) bool

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

// serve answers r from the snapshot if it holds the token.
func (sn *snapshot) serve(w http.ResponseWriter, r *http.Request, token string, offline offlineHandler) bool {
	sn.mu.RLock()
	session := sn.sessions[token]
	sn.mu.RUnlock()
	return session != nil && offline(w, r, *session)
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

// streamOffline is serveFile for a token in the snapshot, of the film its session plays.
func (s *Server) streamOffline(w http.ResponseWriter, r *http.Request, session offlineSession) bool {
	if file := r.PathValue("file"); file != "stream" && !strings.HasPrefix(file, "stream.") {
		return false
	}
	f, ok := session.pick(r.PathValue("itemId"), r.URL.Query().Get("mediasourceid"))
	if !ok {
		return false
	}
	media, err := s.stores.Open(f.source.StoreURI)
	if err != nil {
		return false
	}
	s.serveSource(w, r, media, f.source)
	return true
}

// segmentOffline answers a segment the transcode store holds, for the hash the
// request's spec has: no job is started or joined, and no viewer recorded.
func (s *Server) segmentOffline(w http.ResponseWriter, r *http.Request, session offlineSession) bool {
	if s.Transcode == nil {
		return false
	}
	f, ok := session.pick(r.PathValue("itemId"), r.URL.Query().Get("mediasourceid"))
	if !ok || f.source.RunTimeTicks <= 0 {
		return false
	}
	media := playbackSource(f.source)
	params, err := playback.ParseParams(r.URL.Query(), media)
	if err != nil {
		return false
	}
	if params.Copy {
		media.Keyframes, media.VideoEnd = f.keyframes, f.videoEnd
	}
	spec, err := playback.NewSpec(r.PathValue("itemId"), params, media)
	if err != nil {
		return false
	}
	n, valid := playback.ParseSegment(r.PathValue("segmentId"), media.Layout(spec.Copy).Count())
	if !valid {
		return false
	}
	body, err := s.Transcode.Stored(r.Context(), spec.Hash(), n)
	if err != nil {
		return false // Not in the store: a job is Postgres's to decide.
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Write(body) //nolint:errcheck // A client that goes away is not a fault.
	return true
}
