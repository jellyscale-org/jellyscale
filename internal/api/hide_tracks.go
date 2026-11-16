package api

import (
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// trackFilterOf is the filter a viewer's saved languages and track preferences make. It is the session user's own,
// read with the token on every request, so a change acts at once on every replica.
func trackFilterOf(user User) playback.TrackFilter {
	prefs := trackPreferencesOf(user.TrackPreferences)
	config := configurationOf(user.Configuration)
	filter := playback.TrackFilter{
		HideAudio: prefs.HideOtherAudio, HideSubtitles: prefs.HideOtherSubtitles, ShowAll: prefs.ShowAllTracks,
		SubtitleLanguage: config.SubtitleLanguagePreference,
		KeepAudio:        prefs.KeepAudioLanguages, KeepSubtitles: prefs.KeepSubtitleLanguages,
	}
	if config.AudioLanguagePreference != nil {
		filter.AudioLanguage = *config.AudioLanguagePreference
	}
	return filter
}

// hideTracks lists the tracks the filter hides as placeholders of Type Data at their own Index, so no index moves, and
// returns how many. Nothing is removed from the file or the library, and every index is still accepted by the routes.
func hideTracks(source *media.MediaSource, filter playback.TrackFilter) int {
	hidden := filter.Hidden(source.MediaStreams)
	if len(hidden) == 0 {
		return 0
	}
	streams := make([]media.MediaStream, len(source.MediaStreams))
	for i, stream := range source.MediaStreams {
		streams[i] = stream
		for _, index := range hidden {
			if stream.Index == index {
				streams[i] = media.MediaStream{Index: index, Type: "Data", Placeholder: true}
			}
		}
	}
	source.MediaStreams = streams
	return len(hidden)
}
