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
	"slices"
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
		notFound(w, r)
		return
	}
	s.serveFile(w, r, false)
}

// download sends an item's file to be saved, through the same path as direct
// play. Jellyfin refuses HEAD here; this answers it like GET, harmlessly.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if !sessionOf(r).User.AllowDownload {
		http.Error(w, "this account may not download", http.StatusForbidden)
		return
	}
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
		notFound(w, r)
		return
	}
	source, found, err := s.film(r, itemID, sourceID, s.source)
	if err == nil && !found && attachment && sourceID != "" {
		// Jellyfin sends the item's own file when the source id matches none, and
		// a saved-file link has no better answer than that.
		source, found, err = s.film(r, itemID, "", s.source)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		notFound(w, r)
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
	s.answerPlaybackInfo(w, r, false)
}

// markedPlaybackInfo is PlaybackInfo with every track listed as itself, each audio and subtitle track marked Hidden by
// the rule that makes the others placeholders, and each source carrying its OriginalLanguage (JS-120).
func (s *Server) markedPlaybackInfo(w http.ResponseWriter, r *http.Request) {
	s.answerPlaybackInfo(w, r, true)
}

func (s *Server) answerPlaybackInfo(w http.ResponseWriter, r *http.Request, marked bool) {
	id := r.PathValue("itemId")
	if !db.ValidID(id) {
		s.refuseV1(w, r, http.StatusNotFound, reasonNotFound, "404 page not found")
		return
	}
	var request playback.Request
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			request, err = playback.ParseRequest(body)
		}
		if err != nil {
			s.refuseV1(w, r, http.StatusBadRequest, reasonInvalidBody, "invalid body")
			return
		}
	}
	sources, found, err := s.Items.MediaSources(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		s.refuseV1(w, r, http.StatusNotFound, reasonNotFound, "404 page not found")
		return
	}
	wanted := r.URL.Query().Get("mediasourceid")
	if wanted == "" {
		wanted = request.MediaSourceID
	}
	playSession := randomHex(16)
	readWorkers := s.workersOnce(r.Context())
	var decisions []verdict // Logged and counted once the reply is known not to be a refusal.
	result := make([]media.MediaSource, 0, len(sources))
	for _, source := range sources {
		if wanted != "" && !strings.EqualFold(db.CompactID(wanted), source.ID) {
			continue
		}
		source.SupportsTranscoding = false
		source.RequiredHTTPHeaders = &media.Headers{}
		source.TranscodingSubProtocol = "http"
		source.HasSegments = false
		source.IsInfiniteStream = false
		source.DefaultSubtitleStreamIndex = media.Ptr(-1)
		// The row is read once, by whichever step needs it first.
		stored := readOnce(func() (items.Source, bool, error) {
			return s.Items.PlaybackSource(r.Context(), id, source.ID)
		})
		if err := s.listFonts(r.Context(), id, &source, true, stored); err != nil {
			s.fail(w, r, err)
			return
		}
		deliverAttachments(&source, id, sessionOf(r).Token)
		// The track the viewer starts with. The plan may take it back where the copy it needs cannot start.
		audio, hasAudio := audioChoice(sessionOf(r).User, source, request.AudioStreamIndex)
		planned := request
		if hasAudio {
			planned.AudioStreamIndex = media.Ptr(playback.Number(audio))
		}
		// The subtitle the viewer starts with is the one the request names, else the one their mode and language choose. The
		// plan sees it as a named track: an image track the client does not draw is burnt in, and where nothing can start that
		// burn-in the plan drops it (and deliverSubtitles with it), so the reply names no default then.
		if request.SubtitleStreamIndex == nil {
			if index, ok := subtitleChoice(sessionOf(r).User, source.MediaStreams, languageOfAudio(source, audio)); ok {
				planned.SubtitleStreamIndex = media.Ptr(playback.Number(index))
			}
		}
		// A pick is a track the viewer named that is neither the file's default nor the one their saved language chooses: the
		// plan plays it or refuses, never plays another (D-029). The saved language falls back to the default.
		dflt, _ := fileDefaultAudio(source)
		saved, _ := audioChoice(sessionOf(r).User, source, nil)
		pick := request.DeviceProfile != nil && request.AudioStreamIndex != nil && hasAudio && audio == int(*request.AudioStreamIndex) && audio != dflt && audio != saved
		plan, err := s.planPlayback(r, id, playSession, planned, &source, stored, &audio, pick, readWorkers)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		decision := decisionOf(r, id, playSession, source, request, planned.SubtitleStreamIndex, audio, hasAudio, plan)
		if plan.method == methodRefused {
			// jellyfin-web shows a dialog for this; offering a transcode that
			// can't start instead makes it retry without end. Its "rate limits"
			// text says the media can't play now, where "no compatible stream"
			// would blame the client.
			s.logDecisions(decision) // Alone: no other source of a refused reply is said to play.
			reply := map[string]any{"MediaSources": []media.MediaSource{}, "PlaySessionId": playSession, "ErrorCode": "RateLimitExceeded"}
			if marked {
				// Jellyfin's ErrorCode stays for the player; jellyscale's own API also says what the viewer can do.
				reply["Reason"], reply["Action"] = string(reasonNoCapacity), reasonActions[reasonNoCapacity]
			}
			writeJSON(w, reply)
			return
		}
		burn := request.NoSubtitleProfiles() || (request.AlwaysBurnInSubtitleWhenTranscoding && source.TranscodingURL != "")
		decisions = append(decisions, decision)
		deliverable, err := s.deliverable(r.Context(), source, stored)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		deliverSubtitles(&source, id, sessionOf(r).Token, request, burn, deliverable)
		if hasAudio {
			source.DefaultAudioStreamIndex = media.Ptr(audio)
		}
		if request.SubtitleStreamIndex == nil && planned.SubtitleStreamIndex != nil {
			fillRemoved(&source, storedStreams(stored)) // The mode's choice must sit at its Index, whatever was removed before it.
		}
		if index, ok := chosenSubtitle(source, planned.SubtitleStreamIndex); ok {
			source.DefaultSubtitleStreamIndex = media.Ptr(index)
		}
		original := markOriginal(&source)
		// Last, so that every choice above is made on the real tracks; the placeholders keep every Index.
		filter := trackFilterOf(sessionOf(r).User)
		filter.NamedAudio, filter.NamedSubtitle = request.AudioStreamIndex, request.SubtitleStreamIndex
		if marked {
			source.MediaStreams = everyTrack(source.MediaStreams, storedStreams(stored))
			markHidden(&source, filter)
			source.OriginalLanguage = &original
		} else if hidden := hideTracks(&source, filter); hidden > 0 {
			s.Log.Debug("hid tracks outside the viewer's languages", "user", sessionOf(r).User.ID, "item", id, "source", source.ID, "hidden", hidden)
		}
		if hasAudio {
			listAudio(&source, audio, original)
		}
		result = append(result, source)
	}
	s.logDecisions(decisions...)
	if len(result) == 0 {
		// A source the request names but the item does not have is a refusal as much as an unknown item: a viewer who
		// edits MediaSourceId must be told why nothing plays, not given an empty answer with no reason.
		reply := map[string]any{"MediaSources": []media.MediaSource{}, "PlaySessionId": playSession}
		if marked {
			reply["Reason"], reply["Action"] = string(reasonNotFound), reasonActions[reasonNotFound]
		}
		writeJSON(w, reply)
		return
	}
	writeJSON(w, map[string]any{"MediaSources": result, "PlaySessionId": playSession})
}

// storedStreams is the file's own track list, empty when the row is gone.
func storedStreams(read sourceReader) []media.MediaStream {
	stored, _, _ := read() // Read already, so no new error; a row gone leaves PlaybackInfo's list as it is.
	return stored.Streams
}

// everyTrack is the file's whole track list in its order: a track PlaybackInfo lists is that listing, and one it drops
// (it cannot be delivered here) is the stored track with no DeliveryUrl. Fluster's mpv sees every track in the file and
// maps them to indexes partly by order, so none may go missing whatever a worker or a replica can do.
func everyTrack(listed, stored []media.MediaStream) []media.MediaStream {
	if len(stored) == 0 {
		return listed
	}
	all := make([]media.MediaStream, 0, len(stored))
	for _, stream := range stored {
		if i := slices.IndexFunc(listed, func(l media.MediaStream) bool { return l.Type == stream.Type && l.Index == stream.Index }); i >= 0 {
			stream = listed[i]
		} else {
			stream.DeliveryURL = ""
		}
		all = append(all, stream)
	}
	return all
}

// planPlayback fills in the transcoding fields of one source in the reply.
// Where transcoding is on, the source lists every audio stream, because
// transcoding can deliver any of them, and a stream change then asks for one.
// The outcome is how it will play and why, for the decision line and the counters,
// which the caller writes once no source of the reply is refused. A refused outcome
// is a source that needs a transcode that can't start now. audio is the track the viewer starts with, which the plan sets to the
// file's default where the film then plays that. pick is true when the request names an audio track that only a transcode
// can play: where none can start, the source is refused rather than play another track (D-029).
func (s *Server) planPlayback(r *http.Request, itemID, playSession string, request playback.Request, source *media.MediaSource, read func() (items.Source, bool, error), audio *int, pick bool, readWorkers func() (transcode.Workers, error)) (outcome, error) {
	if s.Transcode == nil && request.DeviceProfile == nil {
		return outcome{method: methodDirectPlay, streams: source.MediaStreams}, nil
	}
	stored, found, err := read()
	if err != nil {
		return outcome{}, err
	}
	if !found {
		return outcome{method: methodDirectPlay, streams: source.MediaStreams}, nil
	}
	base := outcome{streams: stored.Streams}
	if i := slices.IndexFunc(stored.Streams, func(stream playback.Stream) bool { return stream.Type == "Video" }); i >= 0 {
		base.video = &stored.Streams[i]
	}
	media := playbackSource(stored)
	media.Bitrate = source.Bitrate
	if media.Copyable() {
		media.Keyframes, media.VideoEnd = stored.Keyframes, stored.VideoEnd
	}
	decision := playback.Decide(request, media, true)
	// A direct play that was to be a transcode says what was asked and why it was not given.
	gaveUp := func(why string) outcome {
		out := base.with(methodDirectPlay, why)
		if !decision.DirectPlay {
			out.asked, out.askedReasons = methodReencode, decision.Why()
			if decision.Copy {
				out.asked = methodCopy
			}
		}
		return out
	}
	if stored.RunTimeTicks <= 0 {
		// A film with no probed duration can't be copied or transcoded, so what plays is the file's default track.
		if pick {
			return base.with(methodRefused, s.refusePick("the film has no probed duration", playback.NoProbedDuration)), nil
		}
		*audio = media.DefaultAudio()
		return gaveUp(playback.NoProbedDuration), nil
	}
	if s.Transcode != nil {
		offerTranscoding(source, stored.Streams)
	}
	if decision.DirectPlay {
		return base.with(methodDirectPlay), listPlayed(readWorkers, request, media, source, *audio)
	}
	// Whether or not a worker can transcode it now, so that the track is stored by the time one can.
	s.startImageTracks(r.Context(), itemID, stored, decision.Subtitle)
	user := sessionOf(r)
	params := playback.Params{SourceID: media.ID, AudioIndex: decision.AudioIndex, Subtitle: decision.Subtitle, VideoBitrate: decision.VideoBitrate,
		AudioCodecs: decision.AudioCodecs, MaxAudioChannels: decision.MaxAudioChannels,
		AudioBitrate: decision.AudioBitrate, Copy: decision.Copy, PlaySessionID: playSession, DeviceID: user.DeviceID}
	if request.StartTimeTicks != nil {
		params.StartTicks = min(max(int64(*request.StartTimeTicks), 0), playback.MaxStartTicks)
	}
	spec, err := playback.NewSpec(itemID, params, media)
	if err != nil {
		return outcome{}, err
	}
	// A live job is always offered (invariant 37), by any replica.
	live, err := transcode.LiveJob(r.Context(), s.Pool, spec.Hash())
	if err != nil {
		return outcome{}, err
	}
	var kind, reason string // Why no transcode can start: a reason of the vocabulary, and the words for the log.
	switch {
	case live:
		// jellyfin-web ignores TranscodingUrl unless SupportsTranscoding is set.
		offerTranscoding(source, stored.Streams)
		if mismatch, mismatchErr := transcode.StoreMismatch(r.Context(), s.Pool); mismatch != "" || mismatchErr != nil {
			s.Log.Warn("offering a running transcode though the stores may differ", "item", spec.Item, "reason", mismatch, "error", mismatchErr)
		}
	case s.Transcode != nil:
		var admission transcode.Admission
		admission, err = s.Transcode.Admit(r.Context(), spec, user.User.ID, user.DeviceID)
		kind, reason = admission.Reason, admission.Text
		base.admission = &admission
		if err == nil && reason == "" && *audio != media.DefaultAudio() {
			// The item lists the track alone where no worker of this output version runs transcodes, so a track other
			// than the file's default is asked of the same node read (D-029); Unavailable stays as it is for the rest.
			// A film that needs a transcode whatever the track keeps its job, which waits for a worker of the version
			// as it did, rather than be refused for the length of a rolling deploy.
			plain := request
			plain.AudioStreamIndex, plain.SubtitleStreamIndex = nil, nil
			var w transcode.Workers
			if w, err = readWorkers(); err == nil && !w.Transcodes && playback.Decide(plain, media, true).DirectPlay {
				kind, reason = playback.NoWorkerOfThisOutputVersion, "no transcode worker of this output version is running"
			}
		}
	default: // A replica with no transcodeStore asks only whether the stores differ.
		if reason, err = transcode.StoreMismatch(r.Context(), s.Pool); err == nil && reason == "" {
			if pick {
				return base.with(methodRefused, s.refusePick("no transcodeStore is set", playback.NoTranscodeStore)), nil
			}
			*audio = media.DefaultAudio()
			return gaveUp(playback.NoTranscodeStore), nil // No worker anywhere: transcoding is simply off.
		}
		kind = playback.TranscodeStoreMismatch
	}
	if err != nil {
		return outcome{}, err
	}
	if reason != "" {
		// A viewer whose default track plays as it is keeps it, whichever track
		// they picked, rather than lose the player; a subtitle that was to be
		// drawn is then missing. Otherwise there is nothing to fall back on.
		request.AudioStreamIndex, request.SubtitleStreamIndex = nil, nil
		if pick {
			return base.with(methodRefused, s.refusePick(reason, kind)), nil
		}
		if playback.Decide(request, media, true).DirectPlay {
			s.Log.Warn("cannot switch audio tracks or burn in subtitles, so the default plays on", "item", spec.Item, "reason", reason)
			*audio = media.DefaultAudio()
			if err := listPlayed(readWorkers, request, media, source, *audio); err != nil {
				return outcome{}, err
			}
			// So no subtitle is listed that only a transcode could draw.
			source.SupportsTranscoding = false
			return gaveUp(kind), nil
		}
		s.Log.Error("refusing to offer a transcode: "+reason, "item", spec.Item, "spec", spec.Hash())
		return base.with(methodRefused, kind), nil
	}
	out := base.with(methodReencode, decision.Why()...)
	if decision.Copy {
		out.method = methodCopy
	}
	output := media.Output(decision)
	out.output = &output
	source.SupportsDirectPlay = false
	source.SupportsDirectStream = false
	source.TranscodingURL = playback.MasterURL(db.DashedID(itemID), params, user.Token, stored.ETag, decision, media.FrameRate())
	source.TranscodingSubProtocol = "hls"
	source.TranscodingContainer = "mp4"
	// The session lists these reasons later (TranscodingInfo). They only label
	// the dashboard, so failing to keep them never fails the play. The session
	// id is new to every request, so no row is there to replace; KeepReplica
	// prunes the old ones.
	if _, err := s.Pool.Exec(r.Context(), `INSERT INTO transcode_reasons (play_session_id, spec_hash, reasons) VALUES ($1, $2, $3)`,
		playSession, spec.Hash(), append([]string{}, decision.Reasons...)); err != nil {
		s.Log.Warn("could not keep the transcode reasons for the session list", "error", err)
	}
	return out, nil
}

// listPlayed lists no audio track but the one that plays where no worker will deliver another and the client cannot direct
// play another itself (D-029: the reply lists no track it would not play). A client that picks tracks itself keeps them all.
func listPlayed(readWorkers func() (transcode.Workers, error), request playback.Request, file playback.Source, source *media.MediaSource, played int) error {
	other := slices.IndexFunc(source.MediaStreams, func(stream media.MediaStream) bool { return stream.Type == "Audio" && stream.Index != played })
	if other < 0 {
		return nil
	}
	workers, err := readWorkers()
	if err != nil || workers.Transcodes {
		return err
	}
	request.AudioStreamIndex = media.Ptr(playback.Number(source.MediaStreams[other].Index))
	if !playback.Decide(request, file, true).DirectPlay {
		keepAudio(source, played)
	}
	return nil
}

// refusePick logs why a picked track cannot be delivered, and returns the reason of the refusal for the caller, which answers
// it as a refused source.
func (s *Server) refusePick(text, reason string) string {
	s.Log.Warn("refusing a picked audio track that cannot be delivered: " + text)
	return reason
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
