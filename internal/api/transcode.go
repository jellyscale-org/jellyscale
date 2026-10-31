package api

import (
	"errors"
	"io"
	"net/http"
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
		http.Error(w, "transcoding is not set up on this server", http.StatusServiceUnavailable)
		return req, false
	}
	if !db.ValidID(itemID) || !db.ValidID(sourceID) {
		http.NotFound(w, r)
		return req, false
	}
	source, found, err := s.Items.Source(r.Context(), itemID, sourceID)
	if err != nil {
		s.fail(w, r, err)
		return req, false
	}
	if !found || source.RunTimeTicks <= 0 {
		http.NotFound(w, r)
		return req, false
	}
	req.source = source
	if req.media, err = playbackSource(source); err != nil {
		s.fail(w, r, err)
		return req, false
	}
	if req.params, err = playback.ParseParams(r.URL.Query(), req.media); err == nil {
		req.spec, err = playback.NewSpec(itemID, req.params, req.media)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return req, false
	}
	req.hash = req.spec.Hash()
	req.viewer = transcode.Viewer{PlaySessionID: req.params.PlaySessionID, DeviceID: req.params.DeviceID, UserID: sessionOf(r).User.ID, Item: req.spec.Item}
	if err := s.Transcode.Watch(r.Context(), req.viewer, req.hash); err != nil {
		if errors.Is(err, transcode.ErrStopped) {
			http.Error(w, err.Error(), http.StatusGone)
		} else {
			s.fail(w, r, err)
		}
		return req, false
	}
	return req, true
}

func playbackSource(source items.Source) (playback.Source, error) {
	streams, err := playback.ParseStreams(source.Streams)
	return playback.Source{ID: db.CompactID(source.ID), ETag: source.ETag, Container: source.Container,
		RunTimeTicks: source.RunTimeTicks, Streams: streams}, err
}

// query is what every playlist line carries. The client's token goes in it
// because segment requests have no other way to authenticate.
func (req hlsRequest) query(r *http.Request) string {
	return playback.Query(req.params, sessionOf(r).Token, req.source.ETag, "")
}

func playlist(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	io.WriteString(w, text) //nolint:errcheck
}

func (s *Server) masterPlaylist(w http.ResponseWriter, r *http.Request) {
	if req, ok := s.hlsRequestFor(w, r); ok {
		playlist(w, playback.Master(req.spec, req.query(r), req.media.FrameRate()))
	}
}

func (s *Server) mainPlaylist(w http.ResponseWriter, r *http.Request) {
	if req, ok := s.hlsRequestFor(w, r); ok {
		playlist(w, playback.Main(req.media.Ticks(), req.query(r)))
	}
}

func (s *Server) hlsSegment(w http.ResponseWriter, r *http.Request) {
	req, ok := s.hlsRequestFor(w, r)
	if !ok {
		return
	}
	n, valid := playback.ParseSegment(r.PathValue("segmentId"), playback.Segments(req.media.Ticks()))
	if r.PathValue("playlistId") != "main" || !valid {
		http.NotFound(w, r)
		return
	}
	began := time.Now()
	body, err := s.Transcode.Segment(r.Context(), req.spec, req.hash, req.viewer, n, req.params.StartSegment(playback.Segments(req.media.Ticks())))
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
	case errors.Is(err, transcode.ErrNoWorker), errors.Is(err, transcode.ErrBusy):
		headline := "a transcode job found no worker"
		if errors.Is(err, transcode.ErrBusy) {
			headline = "a transcode job found every slot busy"
		}
		s.Log.Error(headline, "item", req.spec.Item, "spec", req.hash, "segment", n)
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
	w.Header().Set("Content-Type", "video/mp4")
	w.Write(body) //nolint:errcheck // A client that goes away is not a fault.
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
