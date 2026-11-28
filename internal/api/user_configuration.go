package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	target := sessionOf(r).Target.ID
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

// maxConfigurationPatchBody is the largest body of PATCH /jellyscale/v1/users/configuration, as POST's.
const maxConfigurationPatchBody = 1 << 16

// patchConfiguration serves PATCH /jellyscale/v1/users/configuration: it changes the caller's fields the body names
// and answers the whole Configuration as stored.
func (s *Server) patchConfiguration(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConfigurationPatchBody+1))
	if err != nil || len(raw) > maxConfigurationPatchBody {
		s.refuseV1(w, r, http.StatusBadRequest, reasonInvalidBody, "body is unreadable or over 65,536 bytes", "body is unreadable or over 65,536 bytes")
		return
	}
	patch, names, err := parseConfigurationPatch(raw)
	if err != nil {
		// The parser's error names the field as sent. It goes to the client's Detail, never to the log.
		s.refuseV1(w, r, http.StatusBadRequest, reasonInvalidBody, "invalid body", err.Error())
		return
	}
	body, _ := json.Marshal(patch)
	// One statement, so a save that lands while this one waits for the row is merged into, not overwritten.
	var stored []byte
	err = s.Pool.QueryRow(r.Context(), `UPDATE users SET configuration = COALESCE(configuration, '{}'::jsonb) || $2::jsonb
		WHERE id = $1::uuid RETURNING configuration`, session.User.ID, string(body)).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		// The account went away between the token check and the write. From the caller's side that is the same state
		// as a token Postgres does not know: sign in again, rather than hunt for a title that never existed.
		s.refuseV1(w, r, http.StatusUnauthorized, reasonUnauthorized, "unauthorized")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(names) > 0 {
		s.Log.Info("user configuration saved", "user", session.User.ID, "fields", strings.Join(names, ","))
	}
	writeJSON(w, configurationOf(stored))
}

// parseConfigurationPatch reads a body that is one object naming each saveable field at most once, names exact, and
// returns the fields with their values as stored, and their names in the order sent. Values are read as POST reads
// them, except that null is refused. The error says what is wrong.
func parseConfigurationPatch(raw []byte) (map[string]any, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, errors.New("body must be one JSON object")
	}
	patch := map[string]any{}
	var names []string
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, nil, errors.New("body is not valid JSON")
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, errors.New("body is not valid JSON")
		}
		if _, repeated := patch[name]; repeated {
			return nil, nil, fmt.Errorf("%s is named twice", name)
		}
		if string(value) == "null" && slices.Contains(configurationFields, name) {
			return nil, nil, fmt.Errorf("%s is null: name a value, and clear a language with an empty string", name)
		}
		var stored any
		switch name {
		case "AudioLanguagePreference", "SubtitleLanguagePreference":
			var text string
			if json.Unmarshal(value, &text) != nil {
				return nil, nil, fmt.Errorf("%s must be a string", name)
			}
			stored = text
		case "PlayDefaultAudioTrack", "DisplayMissingEpisodes", "DisplayCollectionsView", "EnableLocalPassword", "HidePlayedInLatest",
			"RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay":
			var on bool
			if json.Unmarshal(value, &on) != nil {
				return nil, nil, fmt.Errorf("%s must be true or false", name)
			}
			stored = on
		case "GroupedFolders", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes":
			var sent []string
			if json.Unmarshal(value, &sent) != nil {
				return nil, nil, fmt.Errorf("%s must be a list of ids", name)
			}
			ids, ok := compactIDs(sent)
			if !ok {
				return nil, nil, fmt.Errorf("%s holds an entry that is not an id", name)
			}
			stored = ids
		case "SubtitleMode":
			mode, ok := subtitleModeOf(value)
			if !ok {
				return nil, nil, fmt.Errorf("SubtitleMode must be one of %s, or its number", strings.Join(subtitleModes, ", "))
			}
			stored = mode
		default:
			return nil, nil, fmt.Errorf("unknown field %s (CastReceiverId cannot be saved; names are exact)", name)
		}
		patch[name] = stored
		names = append(names, name)
	}
	if _, err := dec.Token(); err != nil { // The closing brace.
		return nil, nil, errors.New("body is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("body has text after the object")
	}
	return patch, names, nil
}

// configurationFields are the 15 fields a client can save.
var configurationFields = []string{
	"AudioLanguagePreference", "PlayDefaultAudioTrack", "SubtitleLanguagePreference", "DisplayMissingEpisodes", "GroupedFolders",
	"SubtitleMode", "DisplayCollectionsView", "EnableLocalPassword", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes",
	"HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay",
}
