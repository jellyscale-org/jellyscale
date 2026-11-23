package api

import (
	"context"
	"slices"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// subtitleChoice is the subtitle track a viewer's saved SubtitleMode and SubtitleLanguagePreference start a film with, chosen
// among the tracks the viewer is listed (the ones their hiding switches keep), and false for none. A viewer who never posted a
// Configuration has chosen nothing, so they start on nothing, where Jellyfin's new user is in mode Default. It is read from the
// user the token names, so a change acts at once on every replica. audio is the language of the audio track that plays.
func subtitleChoice(user User, streams []media.MediaStream, audio string) (int, bool) {
	if len(user.Configuration) == 0 {
		return 0, false
	}
	config := configurationOf(user.Configuration)
	hidden := trackFilterOf(user).Hidden(streams)
	shown := slices.DeleteFunc(slices.Clone(streams), func(stream media.MediaStream) bool { return slices.Contains(hidden, stream.Index) })
	return playback.ChooseSubtitle(shown, config.SubtitleMode, config.SubtitleLanguagePreference, audio)
}

// languageOfAudio is the language of the audio track with this Index, "" when the file has none such.
func languageOfAudio(source media.MediaSource, index int) string {
	for _, track := range audioTracksOf(source) {
		if track.Type == "Audio" && track.Index == index {
			return track.Language
		}
	}
	return ""
}

// fillRemoved puts a placeholder of Type Data at each Index the file's tracks lack below their last, where a subtitle file was
// removed and its Index, which is never given again, leaves a gap. jellyfin-web reads DefaultSubtitleStreamIndex as a position
// in MediaStreams as well as an Index, so without it every track after the gap could not be a default (as hiding does, the
// placeholders keep every other Index where it is).
func fillRemoved(source *media.MediaSource, stored []media.MediaStream) {
	have := map[int]bool{}
	last := -1
	for _, stream := range stored {
		if stream.Type != "Attachment" {
			have[stream.Index] = true
			last = max(last, stream.Index)
		}
	}
	for index := 0; index < last; index++ {
		if have[index] {
			continue
		}
		at := slices.IndexFunc(source.MediaStreams, func(stream media.MediaStream) bool { return stream.Index > index })
		if at < 0 {
			continue
		}
		source.MediaStreams = slices.Insert(slices.Clone(source.MediaStreams), at, media.MediaStream{Index: index, Type: "Data", Placeholder: true})
	}
}

// burnInCanStart is whether a transcode that draws image track index could start now, as the plan asks of a play that names
// it. The item has no device profile, so the burn-in is priced at the film's own size, which no bitrate cap lowers.
func (s *Server) burnInCanStart(ctx context.Context, itemID string, source media.MediaSource, read sourceReader, index, audio int, user User) bool {
	if s.Transcode == nil {
		return false
	}
	stored, found, err := read()
	if err != nil || !found {
		return false
	}
	spec, err := playback.NewSpec(itemID, playback.Params{SourceID: db.CompactID(source.ID), AudioIndex: audio, Subtitle: &index}, playbackSource(stored))
	if err != nil {
		return false
	}
	reason, _, err := s.Transcode.Refusal(ctx, spec, user.ID, "")
	return err == nil && reason == ""
}
