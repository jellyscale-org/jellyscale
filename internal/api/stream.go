package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

// maxBitrateTest bounds /Playback/BitrateTest. The web client asks for at most
// 3 MB; the cap keeps the route from being a free bandwidth amplifier.
const maxBitrateTest = 10_000_000

// stream direct-plays an item's file. Route patterns can't match part of a
// segment, so one wildcard takes "stream" or "stream.mkv" and is checked here.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	if file != "stream" && !strings.HasPrefix(file, "stream.") {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, false)
}

// download sends an item's file to be saved, through the same path as direct
// play. Jellyfin refuses HEAD here; this answers it like GET, harmlessly.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if !db.ValidID(r.PathValue("itemId")) {
		http.Error(w, "invalid item id", http.StatusBadRequest)
		return
	}
	s.serveFile(w, r, true)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, attachment bool) {
	itemID := r.PathValue("itemId")
	sourceID := r.URL.Query().Get("mediasourceid")
	if !db.ValidID(itemID) || (sourceID != "" && !db.ValidID(sourceID)) {
		http.NotFound(w, r)
		return
	}
	source, found, err := s.source(r.Context(), itemID, sourceID)
	if err == nil && !found && attachment && sourceID != "" {
		// Jellyfin sends the item's own file when the source id matches none, and
		// a saved-file link has no better answer than that.
		source, found, err = s.source(r.Context(), itemID, "")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	media, err := s.stores.Open(source.StoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if attachment {
		w.Header().Set("Content-Disposition", contentDisposition(path.Base(source.Key)))
	}
	s.serveSource(w, r, media, source)
}

// source is Items.Source for a request that plays: it waits for Postgres when it is
// away. The row is read for every request, not kept, so a replaced file is served
// from its new row at once.
func (s *Server) source(ctx context.Context, itemID, sourceID string) (source items.Source, found bool, err error) {
	err = db.Retry(ctx, s.Pool, func() (err error) {
		source, found, err = s.Items.Source(ctx, itemID, sourceID)
		return
	})
	return
}

// contentDisposition is Jellyfin's form: an ASCII fallback with each other
// character as an underscore, and the real name percent-encoded after it.
func contentDisposition(name string) string {
	var fallback, encoded strings.Builder
	for _, r := range name {
		switch {
		case r == '"':
			// Jellyfin 12.4 drops it from both forms.
			continue
		case r > 0x7e || r < 0x20:
			fallback.WriteByte('_')
		case r == '\\':
			fallback.WriteByte('\\')
			fallback.WriteRune(r)
		default:
			fallback.WriteRune(r)
		}
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$&+-.^_`|~", r)) {
			encoded.WriteRune(r)
			continue
		}
		for _, b := range []byte(string(r)) {
			fmt.Fprintf(&encoded, "%%%02X", b)
		}
	}
	return `attachment; filename="` + fallback.String() + `"; filename*=UTF-8''` + encoded.String()
}

// serveSource answers Range, If-Range and HEAD from the store. The reader
// requests nothing until bytes are read, so HEAD and a ranged seek cost only
// what they return, and its context is the request's: a client that goes away
// cancels the store read.
func (s *Server) serveSource(w http.ResponseWriter, r *http.Request, media store.Store, source items.Source) {
	reader := &failureReader{Reader: store.NewReader(r.Context(), media, source.Key, source.Size)}
	defer reader.Close()
	// Headers wait for the first body byte, so an object that has gone missing
	// can still answer 404 instead of a 206 with nothing behind it.
	lazy := &lazyWriter{ResponseWriter: w}
	w = lazy
	digest := sha256.Sum256([]byte(source.ETag + "|" + strconv.FormatInt(source.Size, 10) + "|" + source.Key))
	// Strong, so If-Range can trust it. Content-Type is set because
	// http.ServeContent would otherwise read the object to sniff one.
	w.Header().Set("ETag", `"`+hex.EncodeToString(digest[:16])+`"`)
	w.Header().Set("Content-Type", contentType(source))
	http.ServeContent(w, r, "", time.Time{}, reader)
	// A client that goes away cancels the request context; that isn't a fault.
	if err := reader.failure; err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("media read failed", "key", source.Key, "error", err)
	}
	if lazy.committed || lazy.status == 0 {
		return
	}
	switch err := reader.failure; {
	case err == nil:
		lazy.commit() // HEAD, 304, 412, 416: no body to wait for.
	case errors.Is(err, context.Canceled):
	default:
		status := http.StatusBadGateway
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		for _, name := range []string{"Content-Range", "Content-Length", "Accept-Ranges", "ETag", "Content-Type", "Content-Disposition"} {
			w.Header().Del(name)
		}
		http.Error(lazy.ResponseWriter, http.StatusText(status), status)
	}
}

// lazyWriter holds the status line back until the body starts.
type lazyWriter struct {
	http.ResponseWriter
	status    int
	committed bool
}

func (l *lazyWriter) WriteHeader(status int) {
	if !l.committed && l.status == 0 {
		l.status = status
	}
}

func (l *lazyWriter) commit() {
	if l.committed {
		return
	}
	l.committed = true
	if l.status == 0 {
		l.status = http.StatusOK
	}
	l.ResponseWriter.WriteHeader(l.status)
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	l.commit()
	return l.ResponseWriter.Write(p)
}

// failureReader remembers the first store failure, which ServeContent would
// otherwise turn into a silently truncated body.
type failureReader struct {
	*store.Reader
	failure error
}

func (l *failureReader) Read(p []byte) (int, error) {
	n, err := l.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && l.failure == nil {
		l.failure = err
	}
	return n, err
}

func contentType(source items.Source) string {
	// The probed container is what the bytes are; the URL's extension and the
	// store key's are whatever the client or the file's owner wrote.
	switch strings.ToLower(source.Container) {
	case "mkv":
		// ffprobe reports WebM as "matroska,webm", which containerName folds
		// into mkv. The key's extension is the only thing left that tells
		// them apart, and WebM is a Matroska profile, so a .webm name on a
		// Matroska container is WebM.
		if strings.EqualFold(path.Ext(source.Key), ".webm") {
			return "video/webm"
		}
		return "video/x-matroska"
	case "mp4", "m4v", "mov":
		return "video/mp4"
	case "avi":
		return "video/x-msvideo"
	case "ts":
		return "video/mp2t"
	default:
		return "application/octet-stream"
	}
}

// playbackInfo describes how an item can be played. A request with a
// DeviceProfile gets a direct play or a transcode, whichever the profile
// needs; one without stays direct play.
func (s *Server) playbackInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("itemId")
	if !db.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	var request playback.Request
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			request, err = playback.ParseRequest(body)
		}
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
	}
	sources, found, err := s.Items.MediaSources(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	wanted := r.URL.Query().Get("mediasourceid")
	if wanted == "" {
		wanted = request.MediaSourceID
	}
	playSession := randomHex(16)
	result := make([]media.MediaSource, 0, len(sources))
	for _, source := range sources {
		if wanted != "" && db.CompactID(wanted) != source.ID {
			continue
		}
		source.SupportsTranscoding = false
		source.RequiredHTTPHeaders = &media.Headers{}
		source.TranscodingSubProtocol = "http"
		source.HasSegments = false
		source.IsInfiniteStream = false
		source.DefaultSubtitleStreamIndex = media.Ptr(-1)
		if err := s.listFonts(r.Context(), id, &source, true); err != nil {
			s.fail(w, r, err)
			return
		}
		deliverAttachments(&source, id, sessionOf(r).Token)
		refused, err := s.planPlayback(r, id, playSession, request, &source)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if refused {
			// jellyfin-web shows a dialog for this; offering a transcode that
			// can't start instead makes it retry without end. Its "rate limits"
			// text says the media can't play now, where "no compatible stream"
			// would blame the client.
			writeJSON(w, map[string]any{"MediaSources": []media.MediaSource{}, "PlaySessionId": playSession, "ErrorCode": "RateLimitExceeded"})
			return
		}
		burn := request.NoSubtitleProfiles() || (request.AlwaysBurnInSubtitleWhenTranscoding && source.TranscodingURL != "")
		deliverable, err := s.deliverable(r.Context(), id, source)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		deliverSubtitles(&source, id, sessionOf(r).Token, request, burn, deliverable)
		if index, ok := defaultAudio(source); ok {
			source.DefaultAudioStreamIndex = media.Ptr(index)
		}
		if index, ok := chosenSubtitle(source, request.SubtitleStreamIndex); ok {
			source.DefaultSubtitleStreamIndex = media.Ptr(index)
		}
		result = append(result, source)
	}
	writeJSON(w, map[string]any{"MediaSources": result, "PlaySessionId": playSession})
}

// planPlayback fills in the transcoding fields of one source in the reply.
// Where transcoding is on, the source lists every audio stream, because
// transcoding can deliver any of them, and a stream change then asks for one.
// refused is true when the source needs a transcode that can't start now.
func (s *Server) planPlayback(r *http.Request, itemID, playSession string, request playback.Request, source *media.MediaSource) (refused bool, err error) {
	if s.Transcode == nil && request.DeviceProfile == nil {
		playbackDecisions.WithLabelValues("direct_play").Inc()
		return false, nil
	}
	stored, found, err := s.Items.Source(r.Context(), itemID, source.ID)
	if err != nil || !found || stored.RunTimeTicks <= 0 {
		return false, err
	}
	media := playbackSource(stored)
	media.Bitrate = source.Bitrate
	if media.Copyable() {
		if media.Keyframes, media.VideoEnd, err = s.Items.Keyframes(r.Context(), stored.ID); err != nil {
			return false, err
		}
	}
	decision := playback.Decide(request, media, true)
	if s.Transcode != nil {
		offerTranscoding(source, stored.Streams)
	}
	if decision.DirectPlay {
		playbackDecisions.WithLabelValues("direct_play").Inc()
		return false, nil
	}
	user := sessionOf(r)
	params := playback.Params{SourceID: media.ID, AudioIndex: decision.AudioIndex, Subtitle: decision.Subtitle, VideoBitrate: decision.VideoBitrate,
		AudioCodecs: decision.AudioCodecs, MaxAudioChannels: decision.MaxAudioChannels,
		AudioBitrate: decision.AudioBitrate, Copy: decision.Copy, PlaySessionID: playSession, DeviceID: user.DeviceID}
	if request.StartTimeTicks != nil {
		params.StartTicks = min(max(int64(*request.StartTimeTicks), 0), playback.MaxStartTicks)
	}
	spec, err := playback.NewSpec(itemID, params, media)
	if err != nil {
		return false, err
	}
	// A live job is always offered (invariant 37), by any replica.
	live, err := transcode.LiveJob(r.Context(), s.Pool, spec.Hash())
	if err != nil {
		return false, err
	}
	var reason string
	switch {
	case live:
		// jellyfin-web ignores TranscodingUrl unless SupportsTranscoding is set.
		offerTranscoding(source, stored.Streams)
		if mismatch, mismatchErr := transcode.StoreMismatch(r.Context(), s.Pool); mismatch != "" || mismatchErr != nil {
			s.Log.Warn("offering a running transcode though the stores may differ", "item", spec.Item, "reason", mismatch, "error", mismatchErr)
		}
	case s.Transcode != nil:
		reason, err = s.Transcode.Unavailable(r.Context(), spec.Hash(), user.User.ID, user.DeviceID)
	default: // A replica with no transcodeStore asks only whether the stores differ.
		if reason, err = transcode.StoreMismatch(r.Context(), s.Pool); err == nil && reason == "" {
			playbackDecisions.WithLabelValues("direct_play").Inc()
			return false, nil // No worker anywhere: transcoding is simply off.
		}
	}
	if err != nil {
		return false, err
	}
	if reason != "" {
		// A viewer whose default track plays as it is keeps it, whichever track
		// they picked, rather than lose the player; a subtitle that was to be
		// drawn is then missing. Otherwise there is nothing to fall back on.
		request.AudioStreamIndex, request.SubtitleStreamIndex = nil, nil
		if playback.Decide(request, media, true).DirectPlay {
			s.Log.Warn("cannot switch audio tracks or burn in subtitles, so the default plays on", "item", spec.Item, "reason", reason)
			// So no subtitle is listed that only a transcode could draw.
			source.SupportsTranscoding = false
			playbackDecisions.WithLabelValues("direct_play").Inc()
			return false, nil
		}
		s.Log.Error("refusing to offer a transcode: "+reason, "item", spec.Item, "spec", spec.Hash())
		playbackRefusals.Inc()
		return true, nil
	}
	if decision.Copy {
		playbackDecisions.WithLabelValues("copy").Inc()
	} else {
		playbackDecisions.WithLabelValues("reencode").Inc()
	}
	source.SupportsDirectPlay = false
	source.SupportsDirectStream = false
	source.TranscodingURL = playback.MasterURL(db.DashedID(itemID), params, user.Token, stored.ETag, decision, media.FrameRate())
	source.TranscodingSubProtocol = "hls"
	source.TranscodingContainer = "mp4"
	// The session lists these reasons later (TranscodingInfo). They only label
	// the dashboard, so failing to keep them never fails the play.
	if _, err := s.Pool.Exec(r.Context(), `WITH pruned AS (DELETE FROM transcode_reasons WHERE created_at < now() - interval '1 day')
		INSERT INTO transcode_reasons (play_session_id, spec_hash, reasons) VALUES ($1, $2, $3)
		ON CONFLICT (play_session_id, spec_hash) DO UPDATE SET reasons = EXCLUDED.reasons, created_at = now()`,
		playSession, spec.Hash(), append([]string{}, decision.Reasons...)); err != nil {
		s.Log.Warn("could not keep the transcode reasons for the session list", "error", err)
	}
	return false, nil
}

// offerTranscoding marks the source transcodable, and puts all of the file's
// audio streams in the reply, which the item's own DTO lists only the default
// of (invariant 32). Everything else the DTO lists, subtitles included, stays
// as it was.
func offerTranscoding(source *media.MediaSource, stored []media.MediaStream) {
	// The DTO's own entries are kept as they are: the stored ones carry what
	// the decision needs and the item page never showed.
	type position struct {
		kind  string
		index int
	}
	listed := map[position]media.MediaStream{}
	for _, stream := range source.MediaStreams {
		listed[position{stream.Type, stream.Index}] = stream
	}
	streams := []media.MediaStream{}
	for _, stream := range stored {
		if entry, ok := listed[position{stream.Type, stream.Index}]; ok {
			streams = append(streams, entry)
		} else if stream.Type == "Audio" {
			streams = append(streams, stream)
		}
	}
	source.MediaStreams = streams
	source.SupportsTranscoding = true
}

// chosenSubtitle returns the subtitle track the client asked for, if the source
// lists it. jellyfin-web reads the reply's DefaultSubtitleStreamIndex as a
// position in MediaStreams, so it is only given where position and Index agree;
// otherwise the client would find another stream, or none. The track is still
// in the menu and can be picked by hand.
func chosenSubtitle(source media.MediaSource, wanted *playback.Number) (int, bool) {
	if wanted == nil {
		return 0, false
	}
	if position := int(*wanted); position >= 0 && position < len(source.MediaStreams) {
		stream := source.MediaStreams[position]
		if stream.Type == "Subtitle" && stream.Index == position {
			return position, true
		}
	}
	return 0, false
}

// defaultAudio picks the flagged audio stream, else the first.
func defaultAudio(source media.MediaSource) (int, bool) {
	first, found := 0, false
	for _, stream := range source.MediaStreams {
		if stream.Type != "Audio" {
			continue
		}
		if stream.IsDefault {
			return stream.Index, true
		}
		if !found {
			first, found = stream.Index, true
		}
	}
	return first, found
}

// bitrateTest sends Size bytes so the client can time the download.
func (s *Server) bitrateTest(w http.ResponseWriter, r *http.Request) {
	size, err := strconv.Atoi(r.URL.Query().Get("size"))
	if err != nil || size <= 0 {
		http.Error(w, "Size must be a positive number", http.StatusBadRequest)
		return
	}
	size = min(size, maxBitrateTest)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(size))
	w.Header().Set("Cache-Control", "no-store")
	chunk := make([]byte, 32<<10)
	for sent := 0; sent < size; sent += len(chunk) {
		if _, err := w.Write(chunk[:min(len(chunk), size-sent)]); err != nil {
			return
		}
	}
}
