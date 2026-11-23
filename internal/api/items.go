package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/transcode"
)

const defaultLatestLimit = 16

func emptyResult(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, items.Empty())
}

func (s *Server) userViews(w http.ResponseWriter, r *http.Request) {
	libraries, err := s.Items.Libraries(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]map[string]any, 0, len(libraries))
	for _, library := range libraries {
		views = append(views, s.Items.LibraryDTO(library))
	}
	writeJSON(w, items.Result{Items: views, TotalRecordCount: len(views)})
}

// videoTypes are the item types with a MediaType, which is Video; a folder,
// series or season has none, as in Jellyfin.
var videoTypes = []string{"Movie", "Episode"}

// mediaTypeFilter turns a mediaTypes list into the item types it allows,
// narrowing include. false means nothing can match.
func mediaTypeFilter(mediaTypes, include []string) ([]string, bool) {
	if len(mediaTypes) == 0 {
		return include, true
	}
	if !slices.ContainsFunc(mediaTypes, func(t string) bool { return strings.EqualFold(t, "Video") }) {
		return nil, false
	}
	if len(include) == 0 {
		return videoTypes, true
	}
	var kept []string
	for _, t := range include {
		if slices.ContainsFunc(videoTypes, func(v string) bool { return strings.EqualFold(v, t) }) {
			kept = append(kept, t)
		}
	}
	return kept, len(kept) > 0
}

// itemQuery reads the shared /Items parameters. A false result means the
// request names something that can't match, such as a malformed ID.
func (s *Server) itemQuery(r *http.Request) (items.Query, bool, error) {
	params := r.URL.Query()
	q := items.Query{
		UserID:       sessionOf(r).User.ID,
		ParentID:     params.Get("parentid"),
		Recursive:    params.Get("recursive") == "true",
		IncludeTypes: queryList(r, "includeitemtypes"),
		ExcludeTypes: queryList(r, "excludeitemtypes"),
		Genres:       queryList(r, "genres"),
		SortBy:       queryList(r, "sortby"),
		Search:       params.Get("searchterm"),
		NameStarts:   params.Get("namestartswith"),
		IsFavorite:   queryBool(r, "isfavorite"),
		IsPlayed:     queryBool(r, "isplayed"),
		Start:        queryInt(r, "startindex"),
		Limit:        queryInt(r, "limit"),
	}
	// jellyfin-web's home asks Resume for Video, Audio and Book in turn, one
	// row each; ignoring this put films under Continue Reading.
	types, ok := mediaTypeFilter(queryList(r, "mediatypes"), q.IncludeTypes)
	if !ok {
		return q, false, nil
	}
	q.IncludeTypes = types
	for _, id := range queryList(r, "ids") {
		if !db.ValidID(id) {
			return q, false, nil
		}
		q.IDs = append(q.IDs, id)
	}
	for _, value := range queryList(r, "years") {
		if year, err := strconv.Atoi(value); err == nil {
			q.Years = append(q.Years, year)
		}
	}
	for _, order := range queryList(r, "sortorder") {
		q.Descending = append(q.Descending, strings.EqualFold(order, "Descending"))
	}
	for _, filter := range queryList(r, "filters") {
		value := true
		switch filter {
		case "IsFavorite":
			q.IsFavorite = &value
		case "IsPlayed":
			q.IsPlayed = &value
		case "IsUnplayed":
			value = false
			q.IsPlayed = &value
		case "IsResumable":
			q.IsResumable = true
		}
	}
	if q.ParentID != "" {
		if !db.ValidID(q.ParentID) {
			return q, false, nil
		}
		_, isLibrary, err := s.Items.IsLibrary(r.Context(), q.ParentID)
		if err != nil {
			return q, false, err
		}
		q.ParentIsRoot = isLibrary
	}
	return q, true, nil
}

func (s *Server) listItems(w http.ResponseWriter, r *http.Request) {
	q, ok, err := s.itemQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeJSON(w, items.Empty())
		return
	}
	withCount := r.URL.Query().Get("enabletotalrecordcount") != "false"
	result, err := s.Items.Find(r.Context(), q, items.ParseFields(r.URL.Query().Get("fields")), withCount)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, result)
}

// suggestedItems answers /Items/Suggestions, the row of picks Findroid's home
// screen asks for (and stops on when it is missing): films and shows of the
// types asked for, in random order. Its parameters are named type and
// mediaType, not /Items' includeItemTypes and mediaTypes.
func (s *Server) suggestedItems(w http.ResponseWriter, r *http.Request) {
	q := items.Query{
		UserID:    sessionOf(r).User.ID,
		Recursive: true,
		SortBy:    []string{"Random"},
		Start:     queryInt(r, "startindex"),
		Limit:     queryInt(r, "limit"),
	}
	types, ok := mediaTypeFilter(queryList(r, "mediatype"), queryList(r, "type"))
	if !ok {
		writeJSON(w, items.Empty())
		return
	}
	q.IncludeTypes = types
	withCount := r.URL.Query().Get("enabletotalrecordcount") != "false"
	result, err := s.Items.Find(r.Context(), q, items.ParseFields(r.URL.Query().Get("fields")), withCount)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) latestItems(w http.ResponseWriter, r *http.Request) {
	q, ok, err := s.itemQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeJSON(w, []any{})
		return
	}
	q.Recursive = true
	q.SortBy, q.Descending = []string{"DateCreated"}, []bool{true}
	q.Start = 0
	if q.Limit == 0 {
		q.Limit = defaultLatestLimit
	}
	if len(q.IncludeTypes) == 0 {
		q.IncludeTypes = []string{"Movie", "Episode"}
	}
	result, err := s.Items.Find(r.Context(), q, items.ParseFields(r.URL.Query().Get("fields")), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, result.Items)
}

func (s *Server) resumeItems(w http.ResponseWriter, r *http.Request) {
	q, ok, err := s.itemQuery(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeJSON(w, items.Empty())
		return
	}
	q.Recursive = true
	q.IsResumable = true
	q.SortBy, q.Descending = []string{"DatePlayed"}, []bool{true}
	result, err := s.Items.Find(r.Context(), q, items.ParseFields(r.URL.Query().Get("fields")), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) getItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("itemId")
	if !db.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	library, isLibrary, err := s.Items.IsLibrary(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if isLibrary {
		writeJSON(w, s.Items.LibraryDTO(library))
		return
	}
	dto, runtime, err := s.Items.GetWithRuntime(r.Context(), id, sessionOf(r).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if dto == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.hideUndeliverable(r.Context(), id, dto, runtime, sessionOf(r).User); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, dto)
}

// markOriginal sets IsOriginal on the audio streams of source that are in the
// language of its default audio, and returns that language's tag, or "" if none.
// It never marks a video or a subtitle.
func markOriginal(source *media.MediaSource) string {
	original := playback.Source{Streams: source.MediaStreams}.OriginalLanguage()
	for i := range source.MediaStreams {
		stream := &source.MediaStreams[i]
		stream.IsOriginal = stream.Type == "Audio" && playback.SameLanguage(stream.Language, original)
	}
	return original
}

// hideUndeliverable drops the text tracks from an item's DTO that no worker is
// there to extract, and the fonts not stored yet (invariant 32). The DTO lists the first source's streams
// twice, in MediaStreams and in MediaSources. The repo leaves no attachments in
// a DTO, so it is the stored sources that are made over: they have them, as
// the candidates for listFonts.
// workersOnce reads transcode_nodes at its first call, so one request sends one statement however many sources ask.
func (s *Server) workersOnce(ctx context.Context) func() (transcode.Workers, error) {
	var workers transcode.Workers
	var err error
	var once sync.Once
	return func() (transcode.Workers, error) {
		once.Do(func() { workers, err = transcode.ReadWorkers(ctx, s.Pool) })
		return workers, err
	}
}

func (s *Server) hideUndeliverable(ctx context.Context, itemID string, dto map[string]any, runtime int64, user User) error {
	if _, ok := dto["MediaSources"]; !ok {
		return nil
	}
	sources, _, err := s.Items.MediaSources(ctx, itemID)
	if err != nil {
		return err
	}
	// One read of transcode_nodes serves every source, and is made only where something asks.
	readWorkers := s.workersOnce(ctx)
	deliverables := make([]func(int) bool, len(sources))
	reads := make([]sourceReader, len(sources))
	for i := range sources {
		source := &sources[i]
		read := readOnce(func() (items.Source, bool, error) { return s.Items.Source(ctx, itemID, source.ID) })
		deliverable, err := s.deliverableBy(ctx, *source, read, func() (bool, error) {
			w, err := readWorkers()
			return w.Alive, err
		})
		if err != nil {
			return err
		}
		if err := s.listFonts(ctx, itemID, source, false, read); err != nil {
			return err
		}
		deliverables[i], reads[i] = deliverable, read
	}
	// The original language is made here, from the streams the item lists, so that a
	// film scanned earlier has it with no rescan. Each source is marked by its own
	// default audio; the item answers the first source's language.
	filter := trackFilterOf(user)
	// The Audio menu lists every track where a pick of one can be delivered: as a copy of the video with that track, which
	// needs a transcode store, a probed duration and a worker (a browser's direct play plays the file's default alone), so
	// elsewhere it lists the viewer's track alone (invariant 32, D-018).
	pickable := false
	if s.Transcode != nil && runtime > 0 {
		w, err := readWorkers()
		if err != nil {
			return err
		}
		pickable = w.Transcodes
	}
	for i := range sources {
		if pickable {
			listEveryAudio(&sources[i])
		}
		original := markOriginal(&sources[i])
		hideTracks(&sources[i], filter)
		// The details page's Play sends the track its menu selects, so the item names and lists the viewer's track.
		// Where no pick can be delivered, a saved language cannot be either, so the file's default is what they hear (D-029).
		audio, ok := fileDefaultAudio(sources[i])
		if pickable {
			audio, ok = audioChoice(user, sources[i], nil)
		}
		if ok {
			sources[i].DefaultAudioStreamIndex = media.Ptr(audio)
			listAudio(&sources[i], audio, original)
		}
		// The mode chooses among every track, as PlaybackInfo does, and only then is a track no worker can extract dropped, so a
		// choice that cannot be delivered is no default here as there, never a lower-ranked track (invariant 32).
		index, chosen := subtitleChoice(user, sources[i].MediaStreams, languageOfAudio(sources[i], audio))
		image := chosen && slices.ContainsFunc(sources[i].MediaStreams, func(stream media.MediaStream) bool { return stream.Index == index && stream.IsImage() })
		sources[i].MediaStreams = slices.DeleteFunc(slices.Clone(sources[i].MediaStreams), func(stream media.MediaStream) bool {
			return stream.Type == "Subtitle" && !deliverables[i](stream.Index)
		})
		if chosen && image {
			// Only a transcode draws it, as PlaybackInfo's plan decides: with none that can start there is no default.
			chosen = runtime > 0 && s.burnInCanStart(ctx, itemID, sources[i], reads[i], index, audio, user)
		}
		if chosen {
			nameSubtitle(&sources[i], index)
		}
		if i == 0 && original != "" {
			dto["OriginalLanguage"] = original
		}
	}
	dto["MediaSources"] = sources
	if len(sources) > 0 {
		dto["MediaStreams"] = sources[0].MediaStreams
	}
	return nil
}

// nameSubtitle gives the source the subtitle the viewer's mode and language chose, where the item lists that track at its own
// position (jellyfin-web reads the number both ways, so a track that is not there is no default). The details page then
// selects it and its Play sends it, as PlaybackInfo does with no track named.
func nameSubtitle(source *media.MediaSource, index int) {
	if index < len(source.MediaStreams) && source.MediaStreams[index].Type == "Subtitle" && source.MediaStreams[index].Index == index {
		source.DefaultSubtitleStreamIndex = media.Ptr(index)
	}
}

func (s *Server) itemAncestors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("itemId")
	if !db.ValidID(id) {
		writeJSON(w, []any{})
		return
	}
	ancestors, err := s.Items.Ancestors(r.Context(), id, sessionOf(r).User.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, ancestors)
}

func (s *Server) rootFolder(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"Name": "Media Folders", "ServerId": s.ServerID, "Id": s.ServerID, "Type": "UserRootFolder",
		"IsFolder": true, "ImageTags": map[string]string{}, "BackdropImageTags": []string{},
		"UserData": map[string]any{"PlaybackPositionTicks": 0, "PlayCount": 0, "IsFavorite": false, "Played": false},
	})
}

func (s *Server) seasons(w http.ResponseWriter, r *http.Request) {
	s.showChildren(w, r, "Season", []string{"IndexNumber"})
}

func (s *Server) episodes(w http.ResponseWriter, r *http.Request) {
	s.showChildren(w, r, "Episode", []string{"ParentIndexNumber", "IndexNumber"})
}

func (s *Server) showChildren(w http.ResponseWriter, r *http.Request, kind string, sortBy []string) {
	id := r.PathValue("seriesId")
	if !db.ValidID(id) {
		writeJSON(w, items.Empty())
		return
	}
	q := items.Query{
		UserID: sessionOf(r).User.ID, ParentID: id, Recursive: true,
		IncludeTypes: []string{kind}, SortBy: sortBy,
		Start: queryInt(r, "startindex"), Limit: queryInt(r, "limit"),
	}
	if season := r.URL.Query().Get("seasonid"); season != "" && db.ValidID(season) {
		q.ParentID = season
		q.Recursive = false
	}
	result, err := s.Items.Find(r.Context(), q, items.ParseFields(r.URL.Query().Get("fields")), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) itemFilters(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"Genres": []any{}, "Tags": []any{}, "Years": []any{}, "OfficialRatings": []any{}})
}

func (s *Server) themeMedia(w http.ResponseWriter, r *http.Request) {
	owner := db.CompactID(r.PathValue("itemId"))
	result := func(id string) map[string]any {
		return map[string]any{"OwnerId": id, "Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0}
	}
	writeJSON(w, map[string]any{
		"ThemeVideosResult":     result(owner),
		"ThemeSongsResult":      result(owner),
		"SoundtrackSongsResult": result(strings.Repeat("0", 32)),
	})
}

// itemImage serves the stored image. Clients fetch these from <img> tags, which
// can't send headers, so the route is public; item IDs aren't guessable.
// Pictures are served as stored: maxWidth and quality are ignored, because
// resizing needs ffmpeg or a decoder and this role has neither.
func (s *Server) itemImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("itemId")
	kind := strings.ToLower(r.PathValue("imageType"))
	if !db.ValidID(id) || !slices.Contains([]string{"primary", "thumb", "chapter"}, kind) {
		http.NotFound(w, r)
		return
	}
	var uri, key string
	var found bool
	var err error
	if kind == "chapter" {
		// Jellyfin answers a bare /Images/Chapter with the first one, and the tag
		// it is given isn't checked, only used for caching.
		n := 0
		if index := r.PathValue("imageIndex"); index != "" {
			if n, err = strconv.Atoi(index); err != nil || n < 0 {
				http.NotFound(w, r)
				return
			}
		}
		// A stream replica waits for a database that is briefly away (DOMAINS 37).
		err = db.Retry(r.Context(), s.Pool, func() (err error) {
			uri, key, found, err = s.Items.ChapterImage(r.Context(), id, n)
			return err
		})
	} else {
		err = db.Retry(r.Context(), s.Pool, func() (err error) {
			uri, key, found, err = s.Items.Image(r.Context(), id)
			return err
		})
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	assets, err := s.stores.Open(uri)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reader, err := assets.Open(r.Context(), key, 0)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer reader.Close()
	// The key is a content hash (a chapter's has the file's ETag in it), so a
	// changed image gets a new tag and URL. A chapter asked for with no tag
	// can't be told from the next file's, so it isn't kept, as Jellyfin has it;
	// nor is one asked for with a tag that isn't this picture's (a stale DTO).
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if kind == "chapter" && r.URL.Query().Get("tag") != items.ChapterTag(key) {
		w.Header().Set("Cache-Control", "public")
	}
	w.Header().Set("Content-Type", "image/jpeg")
	io.Copy(w, reader) //nolint:errcheck
}
