package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/subtitles"
)

// subtitleStream serves one text track of a file as vtt, srt, js (jellyfin-web's
// JSON cues) or ass. The first request for a track reads the whole file; the
// result is cached (see subtitles.Cache).
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
	mediaStore, err := s.stores.Open(source.StoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	assets, err := s.stores.Open(source.AssetStoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	track, err := subtitles.Cache{Pool: s.Pool, KeepGoing: true, Log: s.Log}.Track(r.Context(), source, mediaStore, assets, index)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.Log.Error("subtitle extraction failed", "item", itemID, "source", sourceID, "index", index, "error", err)
			http.Error(w, "the subtitle track could not be read", http.StatusInternalServerError)
		}
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
// listed that can't be delivered.
func deliverSubtitles(source map[string]any, itemID, token string, burn bool) {
	streams, _ := source["MediaStreams"].([]any)
	kept := make([]any, 0, len(streams))
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		switch {
		case stream["Type"] != "Subtitle":
		case burn && source["SupportsTranscoding"] != true:
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

// encodingConfiguration is what jellyfin-web reads before it renders ASS: only
// whether to fetch fallback fonts, which this server doesn't have.
func encodingConfiguration(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"EnableFallbackFont": false})
}
