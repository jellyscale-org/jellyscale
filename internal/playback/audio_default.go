package playback

import "strings"

// ChooseAudio is the audio track a film starts with for a viewer: Index of the track, and false when the film has no
// audio. It follows what Jellyfin 12.4 answers for DefaultAudioStreamIndex (compat/captures/audio-default-12.4.md),
// with languages compared by language and OriginalLanguage the language of the default audio track.
//
// A request that names an audio track gets it; one that names an index that is no audio stream gets the file's default,
// with no preference applied. Otherwise, with a language saved that names one:
//   - with playDefault on and some track flagged, the first flagged track in that language, else the first flagged;
//   - else the first track in that language;
//   - else the first track in the language of the file's default track, which is the default itself unless an earlier
//     track speaks its language.
//
// With no language saved the file's default (the first flagged track, else the first) is the answer.
func ChooseAudio(streams []Stream, saved string, playDefault bool, named *Number) (int, bool) {
	source := Source{Streams: streams}
	dflt := source.DefaultAudio()
	if dflt == -1 {
		return 0, false
	}
	if named != nil {
		if _, ok := source.audio(int(*named)); ok {
			return int(*named), true
		}
		return dflt, true
	}
	if saved == OriginalLanguageChoice {
		saved = source.OriginalLanguage()
	}
	if languageKey(saved) == "" {
		return dflt, true
	}
	var audio, flagged []Stream
	for _, stream := range streams {
		if stream.Type != "Audio" {
			continue
		}
		audio = append(audio, stream)
		if stream.IsDefault {
			flagged = append(flagged, stream)
		}
	}
	firstIn := func(list []Stream, tag string) (int, bool) {
		for _, stream := range list {
			if SameLanguage(stream.Language, tag) {
				return stream.Index, true
			}
		}
		return 0, false
	}
	if playDefault && len(flagged) > 0 {
		if index, ok := firstIn(flagged, saved); ok {
			return index, true
		}
		return dflt, true
	}
	if index, ok := firstIn(audio, saved); ok {
		return index, true
	}
	own, _ := source.audio(dflt)
	if index, ok := firstIn(audio, own.Language); ok {
		return index, true
	}
	return dflt, true
}

// refusesSecondaryAudio is true when the profile limits the track with an IsSecondaryAudio condition that a track which
// is not the file's own fails (Chrome's profile: Equals false). A client with no such condition, as Fluster's, plays
// any track directly, because it picks the track itself.
func (p DeviceProfile) refusesSecondaryAudio(codec string) bool {
	for _, profile := range p.CodecProfiles {
		if !strings.EqualFold(profile.Type, "VideoAudio") || (profile.Codec != "" && !listHas(profile.Codec, codec)) {
			continue
		}
		for _, c := range profile.Conditions {
			if strings.EqualFold(c.Property, "IsSecondaryAudio") && !c.holds("true") {
				return true
			}
		}
	}
	return false
}
