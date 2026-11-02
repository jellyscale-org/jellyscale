package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/transcode"
)

// subtitleStream serves one text track of a file as vtt, srt, js (jellyfin-web's
// JSON cues) or ass. The first request for a track asks a worker to read the
// whole file and waits for the result in the asset store (see
// transcode.FetchSubtitle); this role runs no media tools.
func (s *Server) subtitleStream(w http.ResponseWriter, r *http.Request) {
	format, ok := strings.CutPrefix(r.PathValue("file"), "stream.")
	// A zero Track renders nothing but still says whether it knows the format.
	_, _, known := subtitles.Track{}.Render(format, 0)
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	shift, shiftErr := strconv.ParseInt(r.PathValue("startPositionTicks"), 10, 64)
	itemID, sourceID := r.PathValue("itemId"), r.PathValue("sourceId")
	if !ok || !known || indexErr != nil || shiftErr != nil || index < 0 || shift < 0 || shift > playback.MaxStartTicks ||
		!db.ValidID(itemID) || !db.ValidID(sourceID) {
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
	media, err := playbackSource(source)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !media.TextSubtitle(index) {
		http.NotFound(w, r)
		return
	}
	assets, err := s.stores.Open(source.AssetStoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	track, err := transcode.FetchSubtitle(r.Context(), s.Pool, s.Hub, assets, itemID, source, index)
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, transcode.ErrNoSubtitleWorker):
		s.Log.Error("subtitle track not extracted", "item", itemID, "source", sourceID, "index", index, "error", err)
		http.Error(w, "subtitles can't be extracted right now", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.Log.Error("subtitle extraction failed", "item", itemID, "source", sourceID, "index", index, "error", err)
		http.Error(w, "the subtitle track could not be read", http.StatusInternalServerError)
		return
	}
	body, contentType, _ := track.Render(format, shift)
	w.Header().Set("Content-Type", contentType)
	w.Write([]byte(body)) //nolint:errcheck // A client that goes away is not a fault.
}

// deliverSubtitles says how each text track of a PlaybackInfo source reaches
// the client. Normally the client fetches it from the stream role. When it is
// to be drawn into the picture it is not offered for download, and if this
// server can't transcode, it is not offered at all, because no track may be
// listed that can't be delivered. deliverable says whether a track can be
// fetched (see s.deliverable).
func deliverSubtitles(source map[string]any, itemID, token string, burn bool, deliverable func(index int) bool) {
	streams, _ := source["MediaStreams"].([]any)
	kept := make([]any, 0, len(streams))
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		switch {
		case stream["Type"] != "Subtitle":
		case burn && source["SupportsTranscoding"] != true, !burn && !deliverable(int(number(stream["Index"]))):
			continue
		case burn:
			stream["DeliveryMethod"] = "Encode"
		default:
			// ass and ssa keep their styles in the one format jellyfin-web
			// renders them from; everything else is plain cues.
			format := "vtt"
			if codec, _ := stream["Codec"].(string); codec == "ass" || codec == "ssa" {
				format = "ass"
			}
			stream["DeliveryMethod"] = "External"
			stream["IsExternalUrl"] = false
			stream["DeliveryUrl"] = "/Videos/" + db.DashedID(itemID) + "/" + source["Id"].(string) + "/Subtitles/" +
				strconv.Itoa(int(number(stream["Index"]))) + "/0/Stream." + format + "?ApiKey=" + url.QueryEscape(token)
		}
		kept = append(kept, entry)
	}
	source["MediaStreams"] = kept
}

// deliverable returns whether a text track of the DTO's source can be fetched:
// when a worker is running any can, and otherwise only those already in the
// asset store, since nothing can extract the rest. The answer can go stale
// when a worker returns, which only hides a track for a moment. Callers list
// one item's source; a list would cost a store request for every track.
func (s *Server) deliverable(ctx context.Context, itemID string, source map[string]any) (func(index int) bool, error) {
	yes := func(int) bool { return true }
	streams, _ := source["MediaStreams"].([]any)
	if !slices.ContainsFunc(streams, func(entry any) bool { stream, _ := entry.(map[string]any); return stream["Type"] == "Subtitle" }) {
		return yes, nil
	}
	if alive, err := transcode.WorkerAlive(ctx, s.Pool); alive || err != nil {
		return yes, err
	}
	sourceID, _ := source["Id"].(string)
	stored, found, err := s.Items.Source(ctx, itemID, sourceID)
	if err != nil || !found {
		return func(int) bool { return false }, err
	}
	assets, err := s.stores.Open(stored.AssetStoreURI)
	if err != nil {
		// A replica that can't open the store (no credentials) can't tell what
		// is cached, and details must still answer, so no text track is listed.
		s.Log.Warn("cannot check cached subtitles", "item", itemID, "error", err)
		return func(int) bool { return false }, nil
	}
	// One bound for all the checks, so a store that never answers hides the
	// tracks rather than hanging the request.
	ctx, cancel := context.WithTimeout(ctx, storeCheckTimeout)
	defer cancel()
	checked := map[int]bool{}
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		if stream["Type"] == "Subtitle" {
			index := int(number(stream["Index"]))
			checked[index] = subtitles.Exists(ctx, assets, subtitles.Key(stored, index))
		}
	}
	if slices.Contains(slices.Collect(maps.Values(checked)), false) {
		s.Log.Warn("no worker is running: hiding subtitle tracks that aren't cached", "item", itemID)
	}
	return func(index int) bool { return checked[index] }, nil
}

// storeCheckTimeout bounds finding which subtitle tracks are cached.
const storeCheckTimeout = 5 * time.Second

// encodingConfiguration is what jellyfin-web reads before it renders ASS: only
// whether to fetch fallback fonts, which this server doesn't have.
func encodingConfiguration(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"EnableFallbackFont": false})
}
