package playback

// TrackFilter is what decides which audio and subtitle tracks of a film a viewer is listed as themselves (JS-119). It
// is made from the viewer's saved languages and track preferences, and from the tracks the request names.
type TrackFilter struct {
	HideAudio, HideSubtitles bool
	// AudioLanguage is the saved audio language: a tag, or OriginalLanguage for the film's own.
	AudioLanguage, SubtitleLanguage string
	KeepAudio, KeepSubtitles        []string
	ShowAll                         bool
	// NamedAudio and NamedSubtitle are the indexes the request names, nil when it names none.
	NamedAudio, NamedSubtitle *Number
}

// OriginalLanguageChoice is the saved audio language that means the language of the film's default audio track.
const OriginalLanguageChoice = "OriginalLanguage"

// Hidden returns the Index of every track of streams that is to be listed as a placeholder. Video is never hidden, nor is
// a track that names no language, the film's original language, the viewer's audio language (for subtitles too), the
// languages they keep, or the track the request names. A kind whose switch is off or whose language is not saved
// hides nothing. The default audio track is the original one, so a film is
// never left without sound.
func (f TrackFilter) Hidden(streams []Stream) []int {
	var hidden []int
	if f.ShowAll {
		return hidden
	}
	original := Source{Streams: streams}.OriginalLanguage()
	audioLanguage := f.AudioLanguage
	if audioLanguage == OriginalLanguageChoice {
		audioLanguage = original
	}
	audioKept := append([]string{audioLanguage, original}, f.KeepAudio...)
	subtitleKept := append([]string{f.SubtitleLanguage, audioLanguage, original}, f.KeepSubtitles...)
	// A language that names none is a language not saved.
	hideAudio := f.HideAudio && languageKey(audioLanguage) != ""
	hideSubtitles := f.HideSubtitles && languageKey(f.SubtitleLanguage) != ""

	for _, stream := range streams {
		var on bool
		var kept []string
		var named *Number
		switch stream.Type {
		case "Audio":
			on, kept, named = hideAudio, audioKept, f.NamedAudio
		case "Subtitle":
			on, kept, named = hideSubtitles, subtitleKept, f.NamedSubtitle
		}
		if !on || languageKey(stream.Language) == "" || (named != nil && int(*named) == stream.Index) {
			continue
		}
		listed := false
		for _, language := range kept {
			listed = listed || SameLanguage(stream.Language, language)
		}
		if !listed {
			hidden = append(hidden, stream.Index)
		}
	}
	return hidden
}
