//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// audioPackets lists a file's first audio stream as "time size hash", with the
// time rounded to microseconds. A copy has to give the source's own packets.
func audioPackets(t *testing.T, file string, withTime bool) []string {
	t.Helper()
	out := run(t, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=pts_time,size,data_hash",
		"-show_data_hash", "sha256", "-of", "csv=p=0", file)
	var packets []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(strings.TrimRight(line, ","), ",")
		if len(fields) != 3 {
			t.Fatalf("unexpected packet line %q", line)
		}
		if withTime {
			packets = append(packets, strings.Join(fields, " "))
		} else {
			packets = append(packets, fields[1]+" "+fields[2])
		}
	}
	return packets
}

// A copied track is the source's own packets, and a run that restarts on
// another worker puts the same packets in the same segments as a run from the
// start: the audio of the spliced stream is byte for byte the audio of the
// whole one, in the same segments (unlike the AAC encoder's, which starts warm
// and differs inaudibly; DOMAINS 48). For an MP4 source the packets and
// their times are identical; Matroska's millisecond timestamps can move a restart's
// times by under a millisecond, so there the packets' data are identical and the times within that bound; segment bytes are compared for MP4 only. Every codec a profile can list is run, at sample
// rates that put the packets on different grids to the video's (AAC 1024
// samples, AC3 1536, E-AC3 and MP3 1152, Opus 960, FLAC 4096), with a source
// whose start time is not zero.
func TestCopiedAudioSplicesTheSamePackets(t *testing.T) {
	testenv.FFmpeg(t)
	work := t.TempDir()
	// Matroska stores whole milliseconds, so a restart's times can move by less
	// than one (DOMAINS 52); the second term is the 6 digits ffprobe prints.
	const matroska = 0.001 + 2e-6
	cases := []struct {
		name, codec, rate string
		encode            []string
		container         string
		shift             string
		drift             float64 // Seconds the times may differ by.
	}{
		{"aac", "aac", "48000", []string{"-c:a", "aac"}, "mkv", "", matroska},
		{"aac at 44.1 kHz", "aac", "44100", []string{"-c:a", "aac"}, "mkv", "", matroska},
		{"aac in mp4", "aac", "48000", []string{"-c:a", "aac"}, "mp4", "", 0},
		{"ac3", "ac3", "48000", []string{"-c:a", "ac3"}, "mkv", "", matroska},
		{"eac3", "eac3", "48000", []string{"-c:a", "eac3"}, "mkv", "", matroska},
		{"mp3", "mp3", "44100", []string{"-c:a", "libmp3lame"}, "mkv", "", matroska},
		{"opus", "opus", "48000", []string{"-c:a", "libopus"}, "mkv", "", matroska},
		{"flac", "flac", "48000", []string{"-c:a", "flac"}, "mkv", "", matroska},
		{"flac, source starting late", "flac", "48000", []string{"-c:a", "flac"}, "mkv", "7.5", matroska},
	}
	const total = 10 // 40 seconds in 4 second segments
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := filepath.Join(work, strings.ReplaceAll(c.name, " ", "-")+"."+c.container)
			run(t, "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24000/1001:duration=40",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=" + c.rate + ":duration=40",
				"-c:v", "libx264", "-preset", "ultrafast", "-g", "240"}, append(c.encode, source)...)...)
			if c.shift != "" {
				shifted := strings.TrimSuffix(source, "."+c.container) + "-shifted." + c.container
				run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", source, "-c", "copy", "-output_ts_offset", c.shift, shifted)
				source = shifted
			}
			spec := playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, Width: 640, Height: 360, AudioCopy: c.codec}
			encodeCopy := func(start int) string {
				dir := t.TempDir()
				cmd, err := Command(context.Background(), source, spec, start, dir, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, out)
				}
				return dir
			}
			whole := encodeCopy(0)
			init := filepath.Join(whole, "init.mp4")
			full := join(t, init, segmentFiles(whole, 0, total)...)

			// The output is the source's own packets, in order and unbroken.
			sourcePackets := audioPackets(t, source, false)
			got := audioPackets(t, full, false)
			at := slices.Index(sourcePackets, got[0])
			if at < 0 || at+len(got) > len(sourcePackets) || !slices.Equal(got, sourcePackets[at:at+len(got)]) {
				t.Fatalf("the copied audio is not a run of the source's packets (starts at source packet %d, %d packets)", at, len(got))
			}
			// All of them but the last few: the video ends a frame or so before the audio.
			if lost := len(sourcePackets) - len(got); at > 1 || lost > 3 {
				t.Errorf("the copy starts at source packet %d and has %d of %d", at, len(got), len(sourcePackets))
			}

			for _, restart := range []int{1, 5, 9} {
				resumed := encodeCopy(restart)
				// The same packets fall in the same segments.
				for n := restart; n < total; n++ {
					name := fmt.Sprintf("%d.m4s", n)
					// An MP4 source gives the same audio byte for byte. (The segment as a
					// whole is not compared: its re-encoded video is not byte for byte the
					// same after a restart in this test, as splice_integration_test.go bounds.)
					if c.drift == 0 && !bytes.Equal(audioOnly(t, join(t, init, filepath.Join(whole, name))), audioOnly(t, join(t, init, filepath.Join(resumed, name)))) {
						t.Errorf("restart %d: segment %d has audio that differs in bytes from the run from zero's", restart, n)
					}
					want := audioPackets(t, join(t, init, filepath.Join(whole, name)), false)
					if got := audioPackets(t, join(t, init, filepath.Join(resumed, name)), false); !slices.Equal(want, got) {
						t.Errorf("restart %d: segment %d has %d audio packets, the run from zero's has %d (or they differ)", restart, n, len(got), len(want))
					}
				}
				// And at the same times: exactly, but for a Matroska source, whose
				// timestamps are whole milliseconds, so the first packet of a restart
				// can start up to a millisecond away from where the run from zero
				// computed it.
				spliced := join(t, init, append(segmentFiles(whole, 0, restart), segmentFiles(resumed, restart, total)...)...)
				compare(t, fmt.Sprintf("restart %d audio times", restart), times(t, full, "a:0", "packet=pts_time"), times(t, spliced, "a:0", "packet=pts_time"), c.drift)
				if string(readFile(t, init)) != string(readFile(t, filepath.Join(resumed, "init.mp4"))) {
					t.Errorf("restart %d: init segments differ", restart)
				}
				if warnings := strings.TrimSpace(run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-")); warnings != "" {
					t.Errorf("restart %d: decoder warnings:\n%s", restart, warnings)
				}
			}
		})
	}
}

// audioOnly is the first audio stream of a file, remuxed on its own so the
// video around it doesn't count.
func audioOnly(t *testing.T, file string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "audio.mp4")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", file, "-map", "0:a:0", "-c", "copy", "-bitexact",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", out)
	return readFile(t, out)
}
