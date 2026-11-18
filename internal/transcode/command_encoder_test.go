package transcode

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// after returns the value that follows the nth (from 0) occurrence of flag.
func after(args []string, flag string, nth int) string {
	for i, a := range args {
		if a == flag {
			if nth == 0 {
				return args[i+1]
			}
			nth--
		}
	}
	return "<none>"
}

// Command's seeks and drop are the plan's, for each kind of source and audio,
// with the probes held at the values the plan's table uses.
func TestCommandCutsWhereThePlanSays(t *testing.T) {
	for _, tc := range []struct {
		name              string
		p                 probes
		spec              playback.Spec
		video, aac, drops string
	}{
		{"ntsc, re-encoded audio", atZero, base, "20.009573", "19.840000", `noise=drop=lt(pts\,957440)`},
		{"shifted, re-encoded audio", shifted, base, "20.009573", "19.840000", `noise=drop=lt(pts\,1317440)`},
		{"negative start, re-encoded audio", early, base, "20.009573", "20.330667", `noise=drop=lt(pts\,956992)`},
		{"ntsc, copied audio", atZero, copied, "20.009573", "18.000000", `noise=drop=lt(pts*tb\,19.936582)`},
		{"shifted, copied audio", shifted, copied, "20.009573", "18.000000", `noise=drop=lt(pts*tb\,27.444082)`},
		{"ntsc, no audio", atZero, silent, "20.009573", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.p.shim(t)
			cmd, err := callCommand(context.Background(), "/media/film.mkv", tc.spec, 5, t.TempDir(), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := after(cmd.Args, "-ss", 0); got != tc.video {
				t.Errorf("video -ss %s, want %s", got, tc.video)
			}
			if tc.aac == "" {
				if n := strings.Count(strings.Join(cmd.Args, " "), "-i /media/film.mkv"); n != 1 {
					t.Errorf("the film is opened %d times, want 1 for a spec with no audio", n)
				}
				if slices.Contains(cmd.Args, "-bsf:a") || strings.Count(strings.Join(cmd.Args, " "), " -ss ") != 1 {
					t.Errorf("no audio, yet: %v", cmd.Args)
				}
				return
			}
			if got := after(cmd.Args, "-ss", 1); got != tc.aac {
				t.Errorf("audio -ss %s, want %s", got, tc.aac)
			}
			if got := after(cmd.Args, "-bsf:a", 0); got != tc.drops {
				t.Errorf("-bsf:a %s, want %s", got, tc.drops)
			}
		})
	}
}

// Segment 0 starts at 0 and drops nothing, for a source that starts anywhere,
// including when a burned track makes Command read the timing.
func TestCommandSegmentZeroSeeksToZeroAndDropsNothing(t *testing.T) {
	for _, p := range []probes{atZero, shifted, early} {
		for name, spec := range map[string]playback.Spec{"re-encoded": withSubtitle(base, 2), "copied": withSubtitle(copied, 2)} {
			t.Run(p.start+" "+name, func(t *testing.T) {
				p.shim(t)
				cmd, err := callCommand(context.Background(), "/media/film.mkv", spec, 0, t.TempDir(), sampleASS, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := after(cmd.Args, "-ss", 0); got != "0.000000" {
					t.Errorf("video -ss %s, want 0.000000", got)
				}
				if got := after(cmd.Args, "-ss", 1); got != "0.000000" {
					t.Errorf("audio -ss %s, want 0.000000", got)
				}
				if slices.Contains(cmd.Args, "-bsf:a") {
					t.Errorf("a drop at segment 0: %v", cmd.Args)
				}
				if strings.Contains(strings.Join(cmd.Args, " "), "-0.000000") {
					t.Errorf("a negative zero: %v", cmd.Args)
				}
			})
		}
	}
}

// What belongs to x264 is the encoder description's: a description with other
// arguments or another delay changes the arguments and the audio cut.
func TestCommandTakesTheEncoderFromItsDescription(t *testing.T) {
	// Today's description makes today's arguments.
	atZero.shim(t)
	if want := []string{"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "51"}; !slices.Equal(x264.videoArgs, want) {
		t.Errorf("x264's video arguments are %v, want %v", x264.videoArgs, want)
	}
	if want := "bframes=2:b-adapt=0:open-gop=0:scenecut=0"; x264.params != want {
		t.Errorf("x264's parameters are %q, want %q", x264.params, want)
	}
	if x264.reorderFrames != 2 {
		t.Errorf("x264 holds back %d frames, want 2", x264.reorderFrames)
	}

	other := encoder{
		videoArgs:     []string{"-c:v", "libx264", "-preset", "fast", "-profile:v", "main", "-level", "41"},
		params:        "bframes=3:b-adapt=0:open-gop=0:scenecut=0",
		reorderFrames: 3,
	}
	for _, copiedAudio := range []bool{false, true} {
		t.Run(fmt.Sprintf("copied audio %v", copiedAudio), func(t *testing.T) {
			spec := base
			if copiedAudio {
				spec = copied
			}
			plain, err := callCommand(context.Background(), "/media/film.mkv", spec, 5, t.TempDir(), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			cmd, err := Command(context.Background(), "/media/film.mkv", spec, 5, t.TempDir(), "", nil, other)
			if err != nil {
				t.Fatal(err)
			}
			if !containsRun(cmd.Args, other.videoArgs) {
				t.Errorf("the description's video arguments are not in %v", cmd.Args)
			}
			if slices.Contains(cmd.Args, "veryfast") {
				t.Errorf("x264's preset is still in %v", cmd.Args)
			}
			if got := after(cmd.Args, "-x264-params", 0); got != other.params {
				t.Errorf("-x264-params %s, want %s", got, other.params)
			}
			// The audio is cut where this encoder's keyframe is, the plan's answer.
			// The first frame at the cut and the grid are what the stand-ins say.
			_, aac, drop := seamPlan(other, 0, ntsc, 20.02, -1024, copiedAudio, 5)
			if got := after(cmd.Args, "-bsf:a", 0); got != drop {
				t.Errorf("-bsf:a %s, want %s", got, drop)
			}
			if got := after(cmd.Args, "-ss", 1); got != seconds(aac) {
				t.Errorf("audio -ss %s, want %s", got, seconds(aac))
			}
			// And it is a different cut from x264's, so the description is read.
			if got, was := after(cmd.Args, "-bsf:a", 0), after(plain.Args, "-bsf:a", 0); got == was {
				t.Errorf("another delay cut the audio where x264's does: %s", got)
			}
		})
	}
}

func containsRun(args, run []string) bool {
	for i := 0; i+len(run) <= len(args); i++ {
		if slices.Equal(args[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

// The version and the hashes of the specs change only with a deliberate bump
// of OutputVersion (invariant 59): 5 is JS-088's fallback encode (Spec.Recode).
func TestSpecHashesAndOutputVersionAreUnchanged(t *testing.T) {
	if playback.OutputVersion != 5 {
		t.Errorf("OutputVersion is %d, want 5", playback.OutputVersion)
	}
	plain := playback.Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 640, Height: 360}
	if got, want := plain.Hash(), "6a60a2da1a31a8d1b1a3c87cb6976b7e"; got != want {
		t.Errorf("hash %s, want %s", got, want)
	}
	rich := withSubtitle(toneMap, 2)
	rich.Item, rich.Source, rich.ETag, rich.AudioCopy = "i", "s", "e", "flac"
	if got, want := rich.Hash(), "eebcc0807fdb1c502aa62bb8b3235aa4"; got != want {
		t.Errorf("hash %s, want %s", got, want)
	}
}
