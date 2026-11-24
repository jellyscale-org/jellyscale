package playback

import "slices"

// ChooseSubtitle is the subtitle track a film starts with for a viewer's SubtitleMode and SubtitleLanguagePreference, as
// Jellyfin 12.4 answers (compat/captures/subtitles-default-12.4.md): Index of the track, and false for none. audioLanguage is
// the language of the audio track that plays. Only the tracks in streams are chosen among, so a caller that lists fewer
// tracks to the viewer passes those. A mode other than Default, Always, OnlyForced and Smart starts nothing.
//
// The tracks are put in an order: files before the film's own tracks; those flagged default; those in the saved language
// (when one is saved); not forced before forced, except that where no track of the film is in the saved language forced
// comes first; then by Index. Each mode takes the first track of the order that it admits.
func ChooseSubtitle(streams []Stream, mode, saved, audioLanguage string) (int, bool) {
	var tracks []Stream
	for _, stream := range streams {
		if stream.Type == "Subtitle" {
			tracks = append(tracks, stream)
		}
	}
	// A track with no language is in none, and with no language saved every track is in it.
	inSaved := func(s Stream) bool { return languageKey(saved) == "" || SameLanguage(s.Language, saved) }
	nothingInSaved := languageKey(saved) != "" && !slices.ContainsFunc(tracks, func(s Stream) bool { return SameLanguage(s.Language, saved) })
	rank := func(s Stream) [5]int {
		plainFirst := b2i(s.IsForced)
		if nothingInSaved {
			plainFirst = 1 - plainFirst
		}
		return [5]int{b2i(!s.IsExternal), b2i(!s.IsDefault), b2i(!inSaved(s) || languageKey(saved) == ""), plainFirst, s.Index}
	}
	slices.SortStableFunc(tracks, func(a, b Stream) int {
		ra, rb := rank(a), rank(b)
		return slices.Compare(ra[:], rb[:])
	})
	first := func(keep func(Stream) bool) (int, bool) {
		for _, s := range tracks {
			if keep(s) {
				return s.Index, true
			}
		}
		return 0, false
	}
	plainInSaved := func(s Stream) bool { return inSaved(s) && !s.IsForced }
	forcedInSaved := func(s Stream) bool { return inSaved(s) && s.IsForced }
	switch mode {
	case "Default":
		return first(func(s Stream) bool { return s.IsExternal || s.IsForced || s.IsDefault })
	case "Always":
		if index, ok := first(plainInSaved); ok {
			return index, true
		}
		return first(forcedInSaved)
	case "OnlyForced":
		return first(forcedInSaved)
	case "Smart":
		if languageKey(saved) == "" {
			return first(func(Stream) bool { return true })
		}
		if !SameLanguage(audioLanguage, saved) {
			if index, ok := first(plainInSaved); ok {
				return index, true
			}
		}
		return first(forcedInSaved)
	}
	return 0, false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
