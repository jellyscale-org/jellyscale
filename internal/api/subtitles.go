package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/transcode"
)

// subtitleStream serves one text track of a file as vtt, srt, js (jellyfin-web's
// JSON cues) or ass. The first request for a track asks a worker to read the
// whole file and waits for the result in the asset store (see
// transcode.FetchSubtitle); this role runs no media tools.
func (s *Server) subtitleStream(w http.ResponseWriter, r *http.Request) {
	// Without a start offset in the path, the same name is also the playlist
	// of segments, and the offset may come in the query.
	playlist := r.PathValue("file") == "subtitles.m3u8" && r.PathValue("startPositionTicks") == ""
	format, ok := strings.CutPrefix(r.PathValue("file"), "stream.")
	// A zero Track renders nothing but still says whether it knows the format.
	_, _, known := subtitles.Track{}.Render(format, 0)
	// PGS goes to the client as the file it is, under either name (Jellyfin 12.4
	// answers both).
	sup := format == "sup" || format == "pgssub"
	known = known || sup
	// As Jellyfin 12.4 answers: a format it doesn't know is a bad request, a name
	// that isn't Stream.* is not found, and the playlist is for GET alone.
	switch {
	case playlist && r.Method == http.MethodHead:
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	case !playlist && ok && !known:
		http.Error(w, "unknown subtitle format", http.StatusBadRequest)
		return
	}
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	query := r.URL.Query()
	startText := r.PathValue("startPositionTicks")
	if startText == "" {
		startText = cmp.Or(query.Get("startpositionticks"), "0")
	}
	shift, shiftErr := strconv.ParseInt(startText, 10, 64)
	end, endErr := strconv.ParseInt(cmp.Or(query.Get("endpositionticks"), "0"), 10, 64)
	itemID, sourceID := r.PathValue("itemId"), r.PathValue("sourceId")
	if !playlist && (!ok || !known) || indexErr != nil || shiftErr != nil || endErr != nil || index < 0 ||
		shift < 0 || shift > playback.MaxStartTicks || end < 0 || end > playback.MaxStartTicks || !db.ValidID(itemID) || !db.ValidID(sourceID) {
		http.NotFound(w, r)
		return
	}
	segmentLength, segmentErr := strconv.ParseInt(query.Get("segmentlength"), 10, 64)
	if playlist && (segmentErr != nil || segmentLength < 1 || segmentLength > playback.MaxStartTicks/ticksPerSecond) {
		http.Error(w, "segmentLength is required", http.StatusBadRequest)
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
	// As Jellyfin 12.4 answers: a track asked for in a format it doesn't have is a
	// bad request.
	switch {
	case sup && media.SubtitleCodec(index) == "PGSSUB":
		s.supStream(w, r, itemID, source, index)
		return
	case sup && media.Burnable(index), !playlist && media.ImageSubtitle(index):
		http.Error(w, "the track is not in that format", http.StatusBadRequest)
		return
	case !media.TextSubtitle(index):
		http.NotFound(w, r)
		return
	}
	if playlist {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(subtitlePlaylist(segmentLength, source.RunTimeTicks, sessionOf(r).Token))) //nolint:errcheck // A client that goes away is not a fault.
		return
	}
	assets, err := s.stores.Open(source.AssetStoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if format == "ass" {
		s.startFonts(r.Context(), itemID, source, assets, false)
	}
	track, err := transcode.FetchSubtitle(r.Context(), s.Pool, s.Hub, assets, itemID, source, index)
	if s.extractionFailed(w, r, err, itemID, source, index) {
		return
	}
	// A segment of the playlist asks for the cues in its window, which keep
	// their times (CopyTimestamps) and say how they line up with the video;
	// a client that asks for a window without that has them counted from its
	// start.
	if query.Has("startpositionticks") || query.Has("endpositionticks") {
		track = track.Within(shift, end)
	}
	if query.Get("copytimestamps") == "true" {
		shift = 0
	}
	body, contentType, _ := track.Render(format, shift)
	if query.Get("addvtttimemap") == "true" && format == "vtt" {
		body = subtitles.WithTimeMap(body)
	}
	w.Header().Set("Content-Type", contentType)
	w.Write([]byte(body)) //nolint:errcheck // A client that goes away is not a fault.
}

// extractionFailed answers a request whose subtitle job ended badly, and says
// whether it did.
func (s *Server) extractionFailed(w http.ResponseWriter, r *http.Request, err error, itemID string, source items.Source, index int) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled):
	case errors.Is(err, transcode.ErrNoSubtitleWorker):
		s.Log.Error("subtitle track not extracted", "item", itemID, "source", source.ID, "index", index, "error", err)
		http.Error(w, "subtitles can't be extracted right now", http.StatusServiceUnavailable)
	default:
		s.Log.Error("subtitle extraction failed", "item", itemID, "source", source.ID, "index", index, "error", err)
		http.Error(w, "the subtitle track could not be read", http.StatusInternalServerError)
	}
	return true
}

// supStream sends a PGS track as the .sup file a worker copied out of the film,
// answering ranges as jellyfin-web asks (it probes with bytes=0-0). The bytes
// go from the asset store to the client as they are; this role runs no media
// tools and holds none of the file in memory.
func (s *Server) supStream(w http.ResponseWriter, r *http.Request, itemID string, source items.Source, index int) {
	assets, err := s.stores.Open(source.AssetStoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	key, err := transcode.FetchSup(r.Context(), s.Pool, s.Hub, assets, itemID, source, index)
	if s.extractionFailed(w, r, err, itemID, source, index) {
		return
	}
	// The object's size and ETag, which the client's ranges and caches need, from
	// the listing of the film's tracks: a store lists directories, not one key.
	objects, err := assets.List(r.Context(), path.Dir(key))
	i := slices.IndexFunc(objects, func(o store.Object) bool { return o.Key == key })
	if err != nil || i < 0 {
		s.Log.Error("the copied PGS track is not in the asset store", "item", itemID, "index", index, "error", err)
		http.Error(w, "the subtitle track could not be read", http.StatusInternalServerError)
		return
	}
	s.serveSource(w, r, assets, items.Source{Key: key, Size: objects[i].Size, ETag: objects[i].ETag})
}

// ticksPerSecond is the unit of every position in the API.
const ticksPerSecond = 10_000_000

// subtitlePlaylist is the VOD playlist of WebVTT segments that Jellyfin 12.4
// answers for a track: one segment per segmentLength seconds of the film, each
// a request for the cues in its window, with the caller's token. The last is
// the rest of the film.
func subtitlePlaylist(segmentLength, runtime int64, token string) string {
	var list strings.Builder
	list.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:" + strconv.FormatInt(segmentLength, 10) + "\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	step := segmentLength * ticksPerSecond
	for start := int64(0); start < runtime; start += step {
		end := min(start+step, runtime)
		list.WriteString("#EXTINF:" + strconv.FormatFloat(float64(end-start)/ticksPerSecond, 'f', -1, 64) + ",\n")
		list.WriteString("stream.vtt?CopyTimestamps=true&AddVttTimeMap=true&StartPositionTicks=" + strconv.FormatInt(start, 10) +
			"&EndPositionTicks=" + strconv.FormatInt(end, 10) + "&ApiKey=" + url.QueryEscape(token) + "\n")
	}
	list.WriteString("#EXT-X-ENDLIST\n")
	return list.String()
}

// attachment serves a font attached to the film. Only fonts in the film's
// fonts manifest are listed (listFonts), so this reads what a worker already
// stored and runs no media tools. A listed font that can't be delivered, such
// as one whose object is gone from the store, is answered with an empty font
// (emptyFont), and for a gone object the fonts job is run again to put it back.
func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	itemID, sourceID := r.PathValue("itemId"), r.PathValue("sourceId")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 || !db.ValidID(itemID) || !db.ValidID(sourceID) {
		http.NotFound(w, r)
		return
	}
	source, found, err := s.Items.Source(r.Context(), itemID, sourceID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mimeType, listed := subtitles.Fonts(source)[index]
	if !found || !listed {
		http.NotFound(w, r)
		return
	}
	assets, err := s.stores.Open(source.AssetStoreURI)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fonts, manifest, err := subtitles.ReadFonts(r.Context(), assets, source)
	if err != nil {
		s.emptyFont(w, r, "the fonts of the film could not be read", err, mimeType)
		return
	}
	sum, kept := fonts[index]
	if !manifest {
		s.startFonts(r.Context(), itemID, source, assets, false)
	}
	if !kept { // The list went after it was listed, or the font was left out.
		s.emptyFont(w, r, "a listed font is not in the film's fonts", nil, mimeType)
		return
	}
	// The URL names an attachment index, not a font: the same index is another
	// font when the film is replaced. So it is not cached unasked, and the font's
	// hash is its ETag, which makes the client's check a 304 with no read of the font.
	etag := `"` + sum + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, err := assets.Open(r.Context(), subtitles.FontKey(sum), 0)
	if errors.Is(err, store.ErrNotFound) {
		s.startFonts(r.Context(), itemID, source, assets, true)
	}
	if err != nil {
		w.Header().Del("ETag")
		s.emptyFont(w, r, "a listed font could not be read", err, mimeType)
		return
	}
	defer body.Close()
	s.sendFont(w, r, body, sum, mimeType)
}

// emptyFont answers for a listed font that can't be delivered right now. A
// client's renderer waits for every listed font and draws nothing if one fails
// to load (an error status stalls it), but it draws with an empty one as it
// does with none. No store, so no client keeps the empty body.
func (s *Server) emptyFont(w http.ResponseWriter, r *http.Request, why string, err error, mimeType string) {
	if r.Context().Err() != nil {
		return
	}
	s.Log.Warn(why+", so an empty font is sent", "path", r.URL.Path, "error", err)
	w.Header().Del("ETag")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", mimeType)
}

// startFonts asks a worker to take the film's fonts out of it, without waiting
// (see transcode.StartFonts). A failure is only logged: the film plays without
// its fonts, and the next request asks again.
func (s *Server) startFonts(ctx context.Context, itemID string, source items.Source, assets store.Store, lost bool) {
	if len(subtitles.Fonts(source)) == 0 {
		return
	}
	if err := transcode.StartFonts(ctx, s.Pool, assets, itemID, source, lost); err != nil {
		s.Log.Warn("fonts job not started", "item", itemID, "source", source.ID, "error", err)
	}
}

// listFonts leaves in the MediaAttachments of a DTO source only the fonts that
// can be delivered (invariant 32): those in the film's fonts manifest, which is
// there once a worker has stored them. Until then the list is empty, and
// jellyfin-web draws ASS in its fallback font. With start, the fonts job is
// started if the manifest is missing, without waiting for it. A store that
// can't be asked lists none.
func (s *Server) listFonts(ctx context.Context, itemID string, source map[string]any, start bool) error {
	attachments, _ := source["MediaAttachments"].([]any)
	if len(attachments) == 0 {
		return nil
	}
	source["MediaAttachments"] = []any{}
	sourceID, _ := source["Id"].(string)
	stored, found, err := s.Items.Source(ctx, itemID, sourceID)
	if err != nil || !found {
		return err
	}
	assets, err := s.stores.Open(stored.AssetStoreURI)
	if err != nil {
		s.Log.Warn("cannot check the stored fonts", "item", itemID, "error", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, storeCheckTimeout)
	defer cancel()
	fonts, manifest, err := subtitles.ReadFonts(ctx, assets, stored)
	if err != nil {
		s.Log.Warn("cannot check the stored fonts", "item", itemID, "error", err)
		return nil
	}
	if !manifest {
		if start {
			s.startFonts(ctx, itemID, stored, assets, false)
		}
		return nil
	}
	source["MediaAttachments"] = slices.DeleteFunc(attachments, func(entry any) bool {
		attachment, _ := entry.(map[string]any)
		return fonts[int(number(attachment["Index"]))] == ""
	})
	return nil
}

// sendFont reads the font whole (it is at most subtitles.MaxFont) before any
// header goes out, and checks it against its hash, which names it in the store.
// A font the store cut short, with an error or without, is answered as a font
// that can't be delivered (emptyFont): ending the response cleanly would let a
// client keep the cut font, and failing it would stop jellyfin-web drawing.
func (s *Server) sendFont(w http.ResponseWriter, r *http.Request, body io.Reader, sum, mimeType string) {
	font, err := io.ReadAll(io.LimitReader(body, subtitles.MaxFont+1))
	if err == nil {
		if got := sha256.Sum256(font); hex.EncodeToString(got[:]) != sum {
			err = errors.New("the font read does not match its hash")
		}
	}
	if err != nil {
		s.emptyFont(w, r, "a listed font was cut short", err, mimeType)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Write(font) //nolint:errcheck // The client went away.
}

// deliverAttachments gives each attachment of a PlaybackInfo source the URL
// to fetch it from, with the caller's token like a subtitle's: Jellyfin sends
// none and allows the fetch without, and this server does not (DOMAINS 50).
func deliverAttachments(source map[string]any, itemID, token string) {
	attachments, _ := source["MediaAttachments"].([]any)
	for _, entry := range attachments {
		attachment, _ := entry.(map[string]any)
		attachment["DeliveryUrl"] = "/Videos/" + db.DashedID(itemID) + "/" + source["Id"].(string) + "/Attachments/" +
			strconv.Itoa(int(number(attachment["Index"]))) + "?ApiKey=" + url.QueryEscape(token)
	}
}

// deliverSubtitles says how each subtitle track of a PlaybackInfo source reaches
// the client. Normally the client fetches it from the stream role: a text track
// as cues, a PGS track as the file, if the client draws PGS itself. When it is
// to be drawn into the picture, or is an image track the client can't draw
// (VobSub, DVB, or PGS for a client that doesn't render it), it is not offered
// for download, and if this server can't transcode, it is not offered at all,
// because no track may be listed that can't be delivered. deliverable says
// whether a track can be fetched (see s.deliverable).
func deliverSubtitles(source map[string]any, itemID, token string, request playback.Request, burn bool, deliverable func(index int) bool) {
	streams, _ := source["MediaStreams"].([]any)
	kept := make([]any, 0, len(streams))
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		index := int(number(stream["Index"]))
		codec, _ := stream["Codec"].(string)
		fetched := !burn && (stream["IsImageSubtitleStream"] != true || codec == "PGSSUB" && request.ShowsPgs())
		// A request with no profile is never given a transcode (playback.Decide).
		canBurn := source["SupportsTranscoding"] == true && request.DeviceProfile != nil
		switch {
		case stream["Type"] != "Subtitle":
		case !fetched && !canBurn, fetched && !deliverable(index):
			continue
		case !fetched:
			stream["DeliveryMethod"] = "Encode"
		default:
			// ass and ssa keep their styles in the one format jellyfin-web
			// renders them from, PGS is the file, and everything else is plain
			// cues.
			format := "vtt"
			switch {
			case codec == "ass" || codec == "ssa":
				format = "ass"
			case codec == "PGSSUB":
				format = "pgssub"
			}
			stream["DeliveryMethod"] = "External"
			stream["IsExternalUrl"] = false
			stream["DeliveryUrl"] = "/Videos/" + db.DashedID(itemID) + "/" + source["Id"].(string) + "/Subtitles/" +
				strconv.Itoa(index) + "/0/Stream." + format + "?ApiKey=" + url.QueryEscape(token)
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
		if stream["Type"] != "Subtitle" {
			continue
		}
		index := int(number(stream["Index"]))
		switch codec, _ := stream["Codec"].(string); {
		case codec == "PGSSUB":
			checked[index] = subtitles.Exists(ctx, assets, subtitles.SupKey(stored, index))
		case stream["IsImageSubtitleStream"] == true:
			// Only a transcode draws it, and with no worker none can run.
		default:
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
