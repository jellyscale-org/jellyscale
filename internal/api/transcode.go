package api

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

// hlsRequest is a transcode URL after validation. The query is the only input
// to a spec and clients can edit it, so every request is parsed from scratch.
type hlsRequest struct {
	spec   playback.Spec
	hash   string
	params playback.Params
	source items.Source
	media  playback.Source
	layout playback.Layout
	viewer transcode.Viewer
	// fellBack is set when the copy's index was found wrong (JS-088): the playlists keep the copy's
	// layout, and a segment is the stored copy one if there is one, else made by a job of recoded,
	// the encode of the copy. hash is then recoded's, which the viewer is recorded on; copyHash is the copy's.
	fellBack bool
	copyHash string
	recoded  playback.Spec
}

// hlsRequestFor validates a stream request and records its viewer. It has
// already answered the client when it returns false.
func (s *Server) hlsRequestFor(w http.ResponseWriter, r *http.Request) (hlsRequest, bool) {
	var req hlsRequest
	itemID := r.PathValue("itemId")
	sourceID := r.URL.Query().Get("mediasourceid")
	if s.Transcode == nil {
		// Another replica offered this stream, because this one has no
		// transcodeStore; PlaybackInfo logs the difference it finds in
		// transcode_nodes.
		refuse(w, r, http.StatusServiceUnavailable, "transcoding is not set up on this server")
		return req, false
	}
	if !db.ValidID(itemID) || !db.ValidID(sourceID) {
		notFound(w, r)
		return req, false
	}
	source, found, err := s.film(r, itemID, sourceID, s.source)
	if err != nil {
		s.fail(w, r, err)
		return req, false
	}
	if !found || source.RunTimeTicks <= 0 {
		notFound(w, r)
		return req, false
	}
	req.source = source
	req.media = playbackSource(source)
	req.params, err = playback.ParseParams(r.URL.Query(), req.media)
	if err == nil && req.params.Copy {
		// The request names the copy, so it is the one place that reads the index.
		var dbErr error
		req.media.Keyframes, req.media.VideoEnd, dbErr = s.keyframes(r, itemID, source)
		if dbErr == nil && len(req.media.Keyframes) == 0 && snapshotOf(r.Context()) == nil {
			// The index was found wrong since this copy was offered: the playlist is cut from the one that was.
			dbErr = s.dropped(r, &req)
		}
		if dbErr != nil {
			s.fail(w, r, dbErr)
			return req, false
		}
	}
	if err == nil {
		req.spec, err = playback.NewSpec(itemID, req.params, req.media)
	}
	if err != nil {
		refuse(w, r, http.StatusBadRequest, err.Error())
		return req, false
	}
	req.hash = req.spec.Hash()
	req.layout = req.media.Layout(req.spec.Copy)
	if req.fellBack {
		req.goOn()
	}
	if snapshotOf(r.Context()) != nil {
		return req, true // No viewer is recorded and no job started: the snapshot only reads.
	}
	req.viewer = transcode.Viewer{PlaySessionID: req.params.PlaySessionID, DeviceID: req.params.DeviceID, UserID: sessionOf(r).User.ID, Item: req.spec.Item}
	// The segment rides on the write every request makes, for the worker to
	// bound the job by.
	segment := -1
	if n, valid := playback.ParseSegment(r.PathValue("segmentId"), req.layout.Count()); valid {
		segment = n
	}
	return req, s.watch(w, r, &req, segment)
}

// watch records that the viewer asked for something on the spec, and answers the client if it cannot.
func (s *Server) watch(w http.ResponseWriter, r *http.Request, req *hlsRequest, segment int) bool {
	if err := s.watching(r, req, segment); err != nil {
		if errors.Is(err, transcode.ErrStopped) {
			http.Error(w, err.Error(), http.StatusGone)
		} else {
			s.fail(w, r, err)
		}
		return false
	}
	return true
}

func (s *Server) watching(r *http.Request, req *hlsRequest, segment int) error {
	first, err := s.Transcode.WatchFirst(r.Context(), req.viewer, req.hash, segment)
	if err == nil && first && req.fellBack {
		// Once for each stream, not for each of its segments.
		playbackFallbacks.Inc()
		s.Log.Warn("a copied stream falls back to a re-encode, going on where the viewer is",
			"item", req.spec.Item, "source", req.spec.Source, "spec", req.copyHash, "reason", "the keyframe index of the file was found wrong")
	}
	return err
}

// segmentOf makes segment n of the request. A stream that fell back is sent the copy's own segment where the store
// holds one, and the encode's where not; each leads with its init segment, so that the player, which holds the copy's
// or the encode's, takes either: a moov at the start of a media segment is an init segment to MSE. A copy whose job
// finds the index wrong under this very request goes on at once as the encode.
func (s *Server) segmentOf(r *http.Request, req *hlsRequest, n int) (*transcode.StoredSegment, error) {
	for {
		if req.fellBack {
			segment, err := s.Transcode.Stored(r.Context(), req.copyHash, n)
			switch {
			case err == nil:
				return s.startWithInit(r, segment, req.copyHash)
			case !errors.Is(err, store.ErrNotFound):
				return nil, err
			}
		}
		spec := req.spec
		if req.fellBack {
			spec = req.recoded
		}
		segment, err := s.Transcode.Segment(r.Context(), spec, req.hash, req.viewer, n, req.params.StartSegment(req.layout))
		var failed transcode.FailedError
		if errors.As(err, &failed) && spec.Copy {
			if err := s.dropped(r, req); err != nil || !req.fellBack {
				return nil, errors.Join(err, failed)
			}
			req.goOn()
			if err := s.watching(r, req, n); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil || !req.fellBack {
			return segment, err
		}
		return s.startWithInit(r, segment, req.hash)
	}
}

func (s *Server) startWithInit(r *http.Request, segment *transcode.StoredSegment, hash string) (*transcode.StoredSegment, error) {
	init, err := s.Transcode.Init(r.Context(), hash)
	if err != nil {
		segment.Close()
		return nil, err
	}
	segment.StartWith(init)
	return segment, nil
}

// dropped reads the index a job found wrong, if there is one, as the one the playlist is cut from, and goes on as the encode of the copy.
func (s *Server) dropped(r *http.Request, req *hlsRequest) error {
	var keyframes []int64
	var end int64
	err := db.Retry(r.Context(), s.Pool, func() (err error) {
		keyframes, end, err = s.Items.DroppedKeyframes(r.Context(), req.source.ID)
		return
	})
	if err == nil && len(keyframes) > 0 {
		req.useDropped(keyframes, end)
	}
	return err
}

// useDropped makes the index a job found wrong the one the playlist is cut from. The request goes on as an encode
// only where the copy's codec has one: an AV1 copy is served as it was, and its segments the store lacks fail as
// before, with no fallback logged or counted for a recovery that cannot happen.
func (req *hlsRequest) useDropped(keyframes []int64, end int64) {
	req.media.Keyframes, req.media.VideoEnd = keyframes, end
	req.fellBack = playback.CanRecode(req.media.VideoCodec())
}

// goOn turns the request into the one that makes its segments with the encode of the copy.
func (req *hlsRequest) goOn() {
	req.copyHash = req.hash
	req.recoded = req.spec.Recoded(req.media.VideoCodec(), req.media.ToneMap())
	req.hash = req.recoded.Hash()
}

func playbackSource(source items.Source) playback.Source {
	return playback.Source{ID: db.CompactID(source.ID), ETag: source.ETag, Container: source.Container,
		Size: source.Size, RunTimeTicks: source.RunTimeTicks, Streams: source.Streams}
}

// query is what every playlist line carries. The client's token goes in it only
// when the request carried that token in its query (jellyfin-web loads the
// lines as URLs); a client that sends a header sends it on every request, and
// Fluster refuses a playlist with a credential in it. A query value that is not
// the request's token is never echoed.
func (req hlsRequest) query(r *http.Request) string {
	token := sessionOf(r).Token
	carried := false
	for _, name := range []string{"ApiKey", "api_key"} {
		for key, values := range r.URL.Query() {
			if strings.EqualFold(key, name) && slices.Contains(values, token) {
				carried = true
			}
		}
	}
	if !carried {
		token = ""
	}
	return playback.Query(req.params, token, req.source.ETag, "")
}

func playlist(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	io.WriteString(w, text) //nolint:errcheck
}

func (s *Server) masterPlaylist(w http.ResponseWriter, r *http.Request) {
	if req, ok := s.hlsRequestFor(w, r); ok {
		s.startImageTracks(r.Context(), req.spec.Item, req.source, req.spec.Subtitle)
		playlist(w, playback.Master(req.spec, req.media, req.query(r)))
	}
}

func (s *Server) mainPlaylist(w http.ResponseWriter, r *http.Request) {
	if req, ok := s.hlsRequestFor(w, r); ok {
		s.startImageTracks(r.Context(), req.spec.Item, req.source, req.spec.Subtitle)
		playlist(w, playback.Main(req.layout, req.query(r)))
	}
}

func (s *Server) hlsSegment(w http.ResponseWriter, r *http.Request) {
	req, ok := s.hlsRequestFor(w, r)
	if !ok {
		return
	}
	n, valid := playback.ParseSegment(r.PathValue("segmentId"), req.layout.Count())
	if !valid {
		notFound(w, r)
		return
	}
	if snapshotOf(r.Context()) != nil {
		// A segment the store holds, for the hash the request's spec has: no job
		// is started or joined. Not in the store, or the store failed, is
		// Postgres's to decide.
		segment, err := s.Transcode.Stored(r.Context(), req.hash, n)
		if err != nil {
			waitForPostgres(r)
		}
		defer segment.Close()
		s.sendSegment(w, r, segment)
		return
	}
	began := time.Now()
	segment, err := s.segmentOf(r, &req, n)
	var failed transcode.FailedError
	switch {
	case err == nil:
	case r.Context().Err() != nil:
		// The client gave up, as it does when it seeks, or after 10 seconds
		// without a reply. Only the second is worth a line.
		if wait := time.Since(began); wait > 5*time.Second {
			s.Log.Warn("a viewer gave up waiting for a segment", "item", req.spec.Item, "spec", req.hash, "segment", n, "waited", wait.Round(time.Second))
		}
		return
	case errors.Is(err, transcode.ErrStopped):
		http.Error(w, err.Error(), http.StatusGone)
		return
	case errors.As(err, &failed):
		// The worker logs the failed job once, with its source; a line per refused request would repeat it.
		s.Log.Debug("a viewer's transcode failed", "item", req.spec.Item, "spec", req.hash, "segment", n, "reason", failed.Reason)
		http.Error(w, "transcode failed", http.StatusInternalServerError)
		return
	case errors.Is(err, transcode.ErrNoWorker), errors.Is(err, transcode.ErrBusy), errors.Is(err, transcode.ErrOtherVersion):
		headline := "a transcode job found no worker"
		if errors.Is(err, transcode.ErrBusy) {
			headline = "a transcode job found the cores of every worker short"
		}
		if errors.Is(err, transcode.ErrOtherVersion) {
			headline = "a transcode job waited for a worker of its output version, and only workers of another version run"
		}
		s.Log.Error(headline, "item", req.spec.Item, "spec", req.hash, "segment", n, "cost", req.spec.Cost())
		w.Header().Set("Retry-After", "5")
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case errors.Is(err, transcode.ErrTimeout):
		s.Log.Warn("timed out waiting for a segment", "item", req.spec.Item, "spec", req.hash, "segment", n)
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	default:
		s.fail(w, r, err)
		return
	}
	defer segment.Close()
	s.sendSegment(w, r, segment)
}

// sendSegment sends a stored segment while it is read. One that fails part of
// the way ends the response short of its Content-Length, so the player never
// takes it for whole; a viewer who left is not a fault.
func (s *Server) sendSegment(w http.ResponseWriter, r *http.Request, segment *transcode.StoredSegment) {
	if err := segment.Send(w, r.Method == http.MethodHead); err != nil && r.Context().Err() == nil {
		s.Log.Warn("a segment was cut short", "path", r.URL.Path, "error", err)
	}
}

// deleteActiveEncodings is how jellyfin-web says it is replacing a stream. It
// sends it twice with the old PlaySessionId, and the second finds nothing to do.
func (s *Server) deleteActiveEncodings(w http.ResponseWriter, r *http.Request) {
	if s.Transcode != nil {
		query := r.URL.Query()
		if err := s.Transcode.Stop(r.Context(), sessionOf(r).User.ID, query.Get("playsessionid"), query.Get("deviceid"), ""); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
