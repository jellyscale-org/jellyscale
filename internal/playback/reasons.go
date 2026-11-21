package playback

import "slices"

// The reasons that only jellyscale gives. They label the decision line and its counters and are in no TranscodeReasons: a
// client reads those, and Jellyfin has no name for them. Jellyfin's own names are written where Decide gives them.
const (
	// A copied codec whose keyframe index is missing or leaves a gap too long to cut segments on.
	KeyframeIndexUnusable = "KeyframeIndexUnusable"
	// A cap on a file that does not say its bitrate, so that nothing shows the file fits it.
	BitrateUnknownUnderCap = "BitrateUnknownUnderCap"
	// A range a copy cannot send (H.264 in HDR, Dolby Vision profile 5), and one the scan has not recorded yet.
	VideoRangeNotCopyable = "VideoRangeNotCopyable"
	VideoRangeUnknown     = "VideoRangeUnknown"

	// Why a transcode cannot be given: the refusals of PlaybackInfo, and the fallbacks that play the file as it is instead.
	NoWorkerRunning             = "NoWorkerRunning"
	NoWorkerWithFreeCores       = "NoWorkerWithFreeCores"
	TranscodeStoreMismatch      = "TranscodeStoreMismatch"
	RememberedFailure           = "RememberedFailure"
	WorkersAtCopyBound          = "WorkersAtCopyBound"
	NoWorkerOfThisOutputVersion = "NoWorkerOfThisOutputVersion"
	NoTranscodeStore            = "NoTranscodeStore"
	NoProbedDuration            = "NoProbedDuration"
)

// Reasons is the whole vocabulary of the decision line, its counters and TranscodeReasons: Jellyfin's names where it has
// one, and jellyscale's own. docs/running.md lists each, and a test holds the list to the code.
func Reasons() []string {
	return []string{
		"ContainerNotSupported", "VideoCodecNotSupported", "AudioCodecNotSupported", "SubtitleCodecNotSupported",
		"SecondaryAudioNotSupported", "VideoProfileNotSupported", "VideoLevelNotSupported", "VideoResolutionNotSupported",
		"InterlacedVideoNotSupported", "AudioChannelsNotSupported", "ContainerBitrateExceedsLimit", "VideoRangeTypeNotSupported",
		KeyframeIndexUnusable, BitrateUnknownUnderCap, VideoRangeNotCopyable, VideoRangeUnknown,
		NoWorkerRunning, NoWorkerWithFreeCores, TranscodeStoreMismatch, RememberedFailure,
		WorkersAtCopyBound, NoWorkerOfThisOutputVersion, NoTranscodeStore, NoProbedDuration,
	}
}

// withReason appends a reason a list lacks.
func withReason(list []string, reason string) []string {
	if slices.Contains(list, reason) {
		return list
	}
	return append(list, reason)
}
