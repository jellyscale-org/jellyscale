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
	if err := s.Transcode.Watch(r.Context(), req.viewer, req.hash, segment); err != nil {
		if errors.Is(err, transcode.ErrStopped) {
			http.Error(w, err.Error(), http.StatusGone)
		} else {
			s.fail(w, r, err)
		}
		return req, false
	}
	return req, true
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
		playlist(w, playback.Master(req.spec, req.media, req.query(r)))
	}
}

func (s *Server) mainPlaylist(w http.ResponseWriter, r *http.Request) {
	if req, ok := s.hlsRequestFor(w, r); ok {
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
	segment, err := s.Transcode.Segment(r.Context(), req.spec, req.hash, req.viewer, n, req.params.StartSegment(req.layout))
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
		s.Log.Warn("a viewer's transcode failed", "item", req.spec.Item, "spec", req.hash, "segment", n, "reason", failed.Reason)
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
