package api

import (
	"slices"

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
	return playback.ChooseAudio(audioTracksOf(source), saved, config.PlayDefaultAudioTrack, named)
}

// audioTracksOf is every audio track of the file; a source with no stored row has what the DTO lists.
func audioTracksOf(source media.MediaSource) []media.MediaStream {
	if len(source.AudioTracks) == 0 {
		return source.MediaStreams
	}
	return source.AudioTracks
}

// fileDefaultAudio is the track a browser's direct play plays: the file's default, whatever the viewer saved.
func fileDefaultAudio(source media.MediaSource) (int, bool) {
	return playback.ChooseAudio(audioTracksOf(source), "", false, nil)
}

// keepAudio lists no audio track but index (D-029: where a pick cannot be delivered, no track is listed that would not play).
func keepAudio(source *media.MediaSource, index int) {
	source.MediaStreams = slices.DeleteFunc(slices.Clone(source.MediaStreams), func(stream media.MediaStream) bool {
		return stream.Type == "Audio" && stream.Index != index
	})
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

// listEveryAudio lists each audio track of the file at its own Index among the source's streams, in the file's order, as
// PlaybackInfo does where transcoding can deliver any of them. A source with no stored row lists what it did.
func listEveryAudio(source *media.MediaSource) {
	if len(source.AudioTracks) == 0 {
		return
	}
	streams := make([]media.MediaStream, 0, len(source.MediaStreams)+len(source.AudioTracks))
	for _, stream := range source.MediaStreams {
		if stream.Type != "Audio" {
			streams = append(streams, stream)
		}
	}
	streams = append(streams, source.AudioTracks...)
	slices.SortStableFunc(streams, func(a, b media.MediaStream) int { return a.Index - b.Index })
	source.MediaStreams = streams
}
