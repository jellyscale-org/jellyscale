package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
)

// Invariant 37: while Postgres is away a stream replica goes on serving direct
// play and the segments in the transcode store to a token that was seen within
// snapshotWindow, from a snapshot it refreshes every snapshotEvery. The numbers
// are in DOMAINS.md, which the tests read.
const (
	snapshotWindow = 60 * time.Second
	snapshotEvery  = 5 * time.Second
)

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
		for _, f := range films {
			if compact(f.itemID) == session.item {
				session.films = append(session.films, f.film)
			}
		}
	}
	byID := map[string]*film{}
	for _, f := range films {
		byID[f.film.source.ID] = f.film
	}
	s.snapshot.replace(sessions, byID)
	return nil
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
