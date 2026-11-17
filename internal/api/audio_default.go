package api

import (
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// audioChoice is the audio track a viewer's saved AudioLanguagePreference and PlayDefaultAudioTrack choose for a
// source, or the track the request names. It is read from the user the token names, so a change acts at once on every
// replica. It chooses among every audio track of the file, not the one the stored DTO lists.
func audioChoice(user User, source media.MediaSource, named *playback.Number) (int, bool) {
	config := configurationOf(user.Configuration)
	saved := ""
	if config.AudioLanguagePreference != nil {
		saved = *config.AudioLanguagePreference
	}
	tracks := source.AudioTracks
	if len(tracks) == 0 {
		tracks = source.MediaStreams // A source with no stored row: what the DTO lists.
	}
	return playback.ChooseAudio(tracks, saved, config.PlayDefaultAudioTrack, named)
}

// listAudio puts the track a viewer starts with among the source's streams where the DTO lists the file's default and
// that is another track (invariant 32): it takes the default's place, so the details page's menu offers the track that
// will play and selects it. Where the chosen track is listed already, as in a PlaybackInfo that lists every audio
// stream, nothing changes. original is the language of the film's default track, which marks the track as the original.
func listAudio(source *media.MediaSource, index int, original string) {
	for _, stream := range source.MediaStreams {
		if stream.Type == "Audio" && stream.Index == index {
			return
		}
	}
	var chosen *media.MediaStream
	for i := range source.AudioTracks {
		if source.AudioTracks[i].Index == index {
			chosen = &source.AudioTracks[i]
		}
	}
	if chosen == nil {
		return
	}
	track := *chosen
	track.IsOriginal = playback.SameLanguage(track.Language, original)
	streams := make([]media.MediaStream, 0, len(source.MediaStreams)+1)
	placed := false
	for _, stream := range source.MediaStreams {
		if stream.Type != "Audio" {
			streams = append(streams, stream)
		} else if !placed {
			streams, placed = append(streams, track), true
		}
	}
	if !placed {
		streams = append(streams, track)
	}
	source.MediaStreams = streams
}
