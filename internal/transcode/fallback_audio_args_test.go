package transcode

import (
	"context"
	"slices"
	"testing"

	"jellyscale/internal/playback"
)

// JS-134: only an HDR copy's fallback has the audio arguments of the restart work (a grid -ss, a -bsf:a drop); the SDR and
// H.264 fallbacks with audio keep the arguments they had.
func TestOnlyAnHDRFallbackHasTheAudioArgumentsOfRestartWork(t *testing.T) {
	for name, c := range map[string]struct {
		codec, transfer string
		audioCopy       string
		hdr             bool
	}{
		"sdr hevc, aac":          {"hevc", "", "", false},
		"sdr hevc, copied audio": {"hevc", "", "aac", false},
		"sdr av1":                {"av1", "", "", false},
		"h264 in PQ":             {"h264", playback.PQ, "", false},
		"h264, copied audio":     {"h264", "", "aac", false},
		"hdr hevc, aac":          {"hevc", playback.PQ, "", true},
		"hdr av1, aac":           {"av1", playback.HLG, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			spec := hdrFallbackSpec(smallCopy, c.codec, c.transfer)
			spec.AudioCopy = c.audioCopy
			cmd, err := RecodeCommand(context.Background(), "film.mkv", spec, c.codec, -1, -1, []float64{0, 5, 11}, "/out")
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(cmd.Args, "-ss"); got != (c.hdr && c.audioCopy == "") {
				t.Errorf("-ss present is %v in %v", got, cmd.Args)
			}
			// A run from the start drops nothing; a drop would be a restart's.
			if slices.Contains(cmd.Args, "-bsf:a") {
				t.Errorf("a run from the start has an audio bitstream filter: %v", cmd.Args)
			}
		})
	}
}
