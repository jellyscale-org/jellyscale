package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
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
	itemID := r.PathValue("itemId")
	sourceID := r.URL.Query().Get("mediasourceid")
	if !db.ValidID(itemID) || (sourceID != "" && !db.ValidID(sourceID)) {
		http.NotFound(w, r)
		return
	}
	source, found, err := s.Items.Source(r.Context(), itemID, sourceID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	media, err := s.storeFor(source.StoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.serveSource(w, r, media, source)
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
		for _, name := range []string{"Content-Range", "Content-Length", "Accept-Ranges", "ETag", "Content-Type"} {
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

// playbackInfo describes how an item can be played. Until transcoding exists
// every source is direct play only, so the DeviceProfile in the body is unused.
func (s *Server) playbackInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("itemId")
	if !db.ValidID(id) {
		http.NotFound(w, r)
		return
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
	result := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		if wanted != "" && db.CompactID(wanted) != source["Id"] {
			continue
		}
		source["SupportsTranscoding"] = false
		source["RequiredHttpHeaders"] = map[string]string{}
		source["TranscodingSubProtocol"] = "http"
		source["HasSegments"] = false
		source["IsInfiniteStream"] = false
		source["DefaultSubtitleStreamIndex"] = -1
		if index, ok := defaultAudio(source); ok {
			source["DefaultAudioStreamIndex"] = index
		}
		if source["MediaAttachments"] == nil {
			source["MediaAttachments"] = []any{}
		}
		if source["Formats"] == nil {
			source["Formats"] = []any{}
		}
		result = append(result, source)
	}
	writeJSON(w, map[string]any{"MediaSources": result, "PlaySessionId": randomHex(16)})
}

// defaultAudio picks the flagged audio stream, else the first.
func defaultAudio(source map[string]any) (int, bool) {
	streams, _ := source["MediaStreams"].([]any)
	first, found := 0, false
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		if stream["Type"] != "Audio" {
			continue
		}
		index, _ := stream["Index"].(float64)
		if isDefault, _ := stream["IsDefault"].(bool); isDefault {
			return int(index), true
		}
		if !found {
			first, found = int(index), true
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
