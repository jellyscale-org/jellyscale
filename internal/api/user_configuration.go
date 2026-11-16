package api

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"jellyscale/internal/db"
)

// UserConfiguration is a viewer's settings as Jellyfin 12.4 names them, in the order it answers them. It is
// what the users table holds in `configuration`, and what every answer carries.
type UserConfiguration struct {
	AudioLanguagePreference    *string `json:",omitempty"` // Absent until the viewer picks one.
	PlayDefaultAudioTrack      bool
	SubtitleLanguagePreference string
	DisplayMissingEpisodes     bool
	GroupedFolders             []string
	SubtitleMode               string
	DisplayCollectionsView     bool
	EnableLocalPassword        bool
	OrderedViews               []string
	LatestItemsExcludes        []string
	MyMediaExcludes            []string
	HidePlayedInLatest         bool
	RememberAudioSelections    bool
	RememberSubtitleSelections bool
	EnableNextEpisodeAutoPlay  bool
	CastReceiverId             string
}

// subtitleModes are the schema's five spellings, in the order a number names them.
var subtitleModes = []string{"Default", "Always", "OnlyForced", "None", "Smart"}

// A new Jellyfin user's settings. Jellyfin answers every user the same CastReceiverId and cannot save another.
func newUserConfiguration() UserConfiguration {
	return UserConfiguration{
		PlayDefaultAudioTrack: true, GroupedFolders: []string{}, SubtitleMode: "Default", OrderedViews: []string{},
		LatestItemsExcludes: []string{}, MyMediaExcludes: []string{}, HidePlayedInLatest: true,
		RememberAudioSelections: true, RememberSubtitleSelections: true, EnableNextEpisodeAutoPlay: true,
		CastReceiverId: "F007D354",
	}
}

// configurationOf is what a stored document says; none is a user who has saved nothing.
func configurationOf(stored []byte) UserConfiguration {
	config := newUserConfiguration()
	if len(stored) > 0 {
		json.Unmarshal(stored, &config) //nolint:errcheck // Only this file writes it.
	}
	config.CastReceiverId = "F007D354"
	return config
}

// configurationBody is what a client posts. A body replaces the whole Configuration, as Jellyfin's does: a field
// it lacks goes back to the new user's value. Unknown fields are dropped, so they are stored nowhere.
type configurationBody struct {
	AudioLanguagePreference    *string
	PlayDefaultAudioTrack      *bool
	SubtitleLanguagePreference *string
	DisplayMissingEpisodes     *bool
	GroupedFolders             []string
	SubtitleMode               json.RawMessage
	DisplayCollectionsView     *bool
	EnableLocalPassword        *bool
	OrderedViews               []string
	LatestItemsExcludes        []string
	MyMediaExcludes            []string
	HidePlayedInLatest         *bool
	RememberAudioSelections    *bool
	RememberSubtitleSelections *bool
	EnableNextEpisodeAutoPlay  *bool
}

// parseConfiguration validates a posted body and returns the settings it holds.
func parseConfiguration(raw []byte) (UserConfiguration, bool) {
	var body *configurationBody
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return UserConfiguration{}, false
	}
	config := newUserConfiguration()
	config.AudioLanguagePreference = body.AudioLanguagePreference
	if body.SubtitleLanguagePreference != nil {
		config.SubtitleLanguagePreference = *body.SubtitleLanguagePreference
	}
	for field, value := range map[*bool]*bool{
		&config.PlayDefaultAudioTrack: body.PlayDefaultAudioTrack, &config.DisplayMissingEpisodes: body.DisplayMissingEpisodes,
		&config.DisplayCollectionsView: body.DisplayCollectionsView, &config.EnableLocalPassword: body.EnableLocalPassword,
		&config.HidePlayedInLatest: body.HidePlayedInLatest, &config.RememberAudioSelections: body.RememberAudioSelections,
		&config.RememberSubtitleSelections: body.RememberSubtitleSelections, &config.EnableNextEpisodeAutoPlay: body.EnableNextEpisodeAutoPlay,
	} {
		if value != nil {
			*field = *value
		}
	}
	var ok bool
	for field, ids := range map[*[]string][]string{
		&config.GroupedFolders: body.GroupedFolders, &config.OrderedViews: body.OrderedViews,
		&config.LatestItemsExcludes: body.LatestItemsExcludes, &config.MyMediaExcludes: body.MyMediaExcludes,
	} {
		if *field, ok = compactIDs(ids); !ok {
			return UserConfiguration{}, false
		}
	}
	if body.SubtitleMode != nil {
		if config.SubtitleMode, ok = subtitleModeOf(body.SubtitleMode); !ok {
			return UserConfiguration{}, false
		}
	}
	return config, true
}

// compactIDs spells each id as Jellyfin answers it: 32 lower-case hex digits, in the order sent.
func compactIDs(ids []string) ([]string, bool) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil {
			return nil, false
		}
		out = append(out, db.CompactID(parsed.String()))
	}
	return out, true
}

// subtitleModeOf takes what Jellyfin takes: a listed word in any case, or its position as a number.
func subtitleModeOf(raw json.RawMessage) (string, bool) {
	var word string
	if json.Unmarshal(raw, &word) == nil {
		for _, mode := range subtitleModes {
			if strings.EqualFold(word, mode) {
				return mode, true
			}
		}
		return "", false
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number >= 0 && int(number) < len(subtitleModes) && number == float64(int(number)) {
		return subtitleModes[int(number)], true
	}
	return "", false
}

// saveConfiguration serves POST /Users/Configuration, naming a user in its userId query or nobody, and the
// obsolete POST /Users/{userId}/Configuration jellyfin-web calls.
func (s *Server) saveConfiguration(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	target := r.PathValue("userId")
	if target == "" {
		target = r.URL.Query().Get("userid")
	}
	if target != "" && !db.ValidID(target) {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if target == "" || strings.EqualFold(db.CompactID(target), db.CompactID(session.User.ID)) {
		target = session.User.ID
	} else if !session.User.IsAdmin {
		// Another user's settings are not an unknown user's to be told about.
		var exists bool
		if err := s.Pool.QueryRow(r.Context(), "SELECT EXISTS (SELECT 1 FROM users WHERE id = $1::uuid)", target).Scan(&exists); err != nil {
			s.fail(w, r, err)
			return
		}
		if !exists {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// A body over 64 KB is refused like one that is not JSON; Jellyfin stores any size.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16+1))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	config, ok := parseConfiguration(raw)
	if !ok || len(raw) > 1<<16 {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	stored, _ := json.Marshal(config)
	// One statement, so two saves at once leave one body whole.
	tag, err := s.Pool.Exec(r.Context(), "UPDATE users SET configuration = $2::jsonb WHERE id = $1::uuid", target, string(stored))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

//go:embed cultures.json
var cultures []byte

// listCultures answers Jellyfin 12.4's list entry for entry (compat/captures/user-configuration-12.4.json).
func listCultures(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(cultures) //nolint:errcheck
}
