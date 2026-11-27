package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"jellyscale/internal/playback"
)

// TrackPreferences is a viewer's own settings for which audio and subtitle tracks to show. They are jellyscale's:
// Jellyfin has no place for them, so they are served at /jellyscale/v1/ and never ride in UserConfiguration.
type TrackPreferences struct {
	HideOtherAudio        bool
	HideOtherSubtitles    bool
	KeepAudioLanguages    []string
	KeepSubtitleLanguages []string
	ShowAllTracks         bool
}

const (
	maxTrackPreferencesBody = 1 << 14
	maxKeptLanguages        = 20
	defaultTrackPreferences = `{"HideOtherAudio":false,"HideOtherSubtitles":false,"KeepAudioLanguages":[],"KeepSubtitleLanguages":[],"ShowAllTracks":false}`
)

// trackPreferencesOf is what a stored document says; none is a viewer who has saved nothing. The lists are never nil.
func trackPreferencesOf(stored []byte) TrackPreferences {
	var prefs TrackPreferences
	if len(stored) == 0 {
		stored = []byte(defaultTrackPreferences)
	}
	json.Unmarshal(stored, &prefs) //nolint:errcheck // Only this file writes it.
	if prefs.KeepAudioLanguages == nil {
		prefs.KeepAudioLanguages = []string{}
	}
	if prefs.KeepSubtitleLanguages == nil {
		prefs.KeepSubtitleLanguages = []string{}
	}
	return prefs
}

func (s *Server) getTrackPreferences(w http.ResponseWriter, r *http.Request) {
	var stored []byte
	if err := s.Pool.QueryRow(r.Context(), "SELECT track_preferences FROM users WHERE id = $1::uuid", sessionOf(r).User.ID).Scan(&stored); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, trackPreferencesOf(stored))
}

// patchTrackPreferences changes the fields the body names and answers the document as stored.
func (s *Server) patchTrackPreferences(w http.ResponseWriter, r *http.Request) {
	session := sessionOf(r)
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxTrackPreferencesBody+1))
	if err != nil || len(raw) > maxTrackPreferencesBody {
		s.refuseV1(w, r, http.StatusBadRequest, reasonInvalidBody, "body is unreadable or over 16 KB")
		return
	}
	patch, err := parseTrackPreferencesPatch(raw)
	if err != nil {
		s.refuseV1(w, r, http.StatusBadRequest, reasonInvalidBody, err.Error())
		return
	}
	// A switch is asked for only once its language is saved; a language cleared later leaves the switch as it was.
	config := configurationOf(session.User.Configuration)
	if patch["HideOtherAudio"] == true && (config.AudioLanguagePreference == nil || *config.AudioLanguagePreference == "") {
		s.refuseV1(w, r, http.StatusConflict, reasonLanguageNotSaved, "HideOtherAudio needs AudioLanguagePreference to be saved first, with POST /Users/Configuration")
		return
	}
	if patch["HideOtherSubtitles"] == true && config.SubtitleLanguagePreference == "" {
		s.refuseV1(w, r, http.StatusConflict, reasonLanguageNotSaved, "HideOtherSubtitles needs SubtitleLanguagePreference to be saved first, with POST /Users/Configuration")
		return
	}
	body, _ := json.Marshal(patch)
	// One statement, so two PATCHes at once both land: the second waits for the row and merges into what the first wrote.
	var stored []byte
	err = s.Pool.QueryRow(r.Context(), `UPDATE users SET track_preferences = COALESCE(track_preferences, $2::jsonb) || $3::jsonb
		WHERE id = $1::uuid RETURNING track_preferences`, session.User.ID, defaultTrackPreferences, string(body)).Scan(&stored)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	prefs := trackPreferencesOf(stored)
	if len(patch) > 0 {
		s.Log.Info("track preferences saved", "user", session.User.ID,
			"HideOtherAudio", prefs.HideOtherAudio, "HideOtherSubtitles", prefs.HideOtherSubtitles,
			"KeepAudioLanguages", prefs.KeepAudioLanguages, "KeepSubtitleLanguages", prefs.KeepSubtitleLanguages,
			"ShowAllTracks", prefs.ShowAllTracks)
	}
	writeJSON(w, prefs)
}

// parseTrackPreferencesPatch reads a body that is one object with each field at most once, names exact, and returns
// the fields it names with their stored values. The error says what is wrong.
func parseTrackPreferencesPatch(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("body must be one JSON object")
	}
	patch := map[string]any{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, errors.New("body is not valid JSON")
		}
		name, _ := token.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errors.New("body is not valid JSON")
		}
		if _, repeated := patch[name]; repeated {
			return nil, fmt.Errorf("%s is named twice", name)
		}
		switch name {
		case "HideOtherAudio", "HideOtherSubtitles", "ShowAllTracks":
			var on bool
			if string(value) == "null" || json.Unmarshal(value, &on) != nil {
				return nil, fmt.Errorf("%s must be true or false", name)
			}
			patch[name] = on
		case "KeepAudioLanguages", "KeepSubtitleLanguages":
			codes, err := languageList(name, value)
			if err != nil {
				return nil, err
			}
			patch[name] = codes
		default:
			return nil, fmt.Errorf("unknown field %s", name)
		}
	}
	if _, err := dec.Token(); err != nil { // The closing brace.
		return nil, errors.New("body is not valid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("body has text after the object")
	}
	return patch, nil
}

// languageList is a list of languages as three-letter codes, each once, in the order first sent.
func languageList(name string, value json.RawMessage) ([]string, error) {
	var sent []string
	if string(value) == "null" || json.Unmarshal(value, &sent) != nil {
		return nil, fmt.Errorf("%s must be a list of language codes", name)
	}
	codes := []string{}
	for _, tag := range sent {
		code, ok := playback.LanguageCode(tag)
		if !ok {
			return nil, fmt.Errorf("%s: %q names no language", name, tag)
		}
		if !slices.Contains(codes, code) {
			codes = append(codes, code)
		}
	}
	if len(codes) > maxKeptLanguages {
		return nil, fmt.Errorf("%s names %d languages, at most %d", name, len(codes), maxKeptLanguages)
	}
	return codes, nil
}
