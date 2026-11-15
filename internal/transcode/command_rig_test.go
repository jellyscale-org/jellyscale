package transcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// JS-113 runs Command against stand-ins for ffprobe and ffmpeg, which answer
// from the environment and log what they were asked, so the arguments Command
// builds are tested with the probes' answers held fixed and with no ffmpeg or
// ffprobe installed: the unit tier needs neither.
//
// A stand-in answers like the real one for the one thing Command asks of it:
// ffprobe for the timing (start_time and r_frame_rate), ffprobe for the packets
// from a time on (the first frame), and ffmpeg for the audio grid (framecrc).
const probeShim = `#!/bin/sh
case "$0" in
*ffprobe)
	case " $* " in
	*format=start_time*)
		echo "ffprobe timing" >> "$SHIM_LOG"
		[ -n "$SHIM_TIMING_FAILS" ] && { echo "boom" >&2; exit 1; }
		[ -n "$SHIM_NO_STREAM" ] && { echo '{"format":{"start_time":"'"$SHIM_START"'"},"streams":[]}'; exit 0; }
		echo '{"format":{"start_time":"'"$SHIM_START"'"},"streams":[{"r_frame_rate":"'"$SHIM_FPS"'"}]}'
		;;
	*-read_intervals*)
		for a in "$@"; do case "$a" in *%*) echo "ffprobe frame $a" >> "$SHIM_LOG" ;; esac; done
		[ -n "$SHIM_FRAME_FAILS" ] && { echo "boom" >&2; exit 1; }
		[ -n "$SHIM_NO_FRAME" ] && exit 0
		# One packet well before the cut, which is not a candidate, then the first frame and a later one.
		echo "0.000000,"
		echo "$SHIM_FIRST,"
		echo "$SHIM_FIRST_LATER,"
		;;
	*) echo "unexpected ffprobe: $*" >&2; exit 2 ;;
	esac
	;;
*ffmpeg)
	case " $* " in
	*framecrc*)
		echo "ffmpeg grid" >> "$SHIM_LOG"
		[ -n "$SHIM_GRID_FAILS" ] && { echo "boom" >&2; exit 1; }
		echo "#format: frame checksums"
		[ -n "$SHIM_NO_PACKET" ] && exit 0
		echo "1, $SHIM_GRID, $SHIM_GRID, 1024, 100, 0x00000000"
		;;
	*) echo "unexpected ffmpeg: $*" >&2; exit 2 ;;
	esac
	;;
esac
`

// probes is what the stand-ins answer.
type probes struct {
	start, fps   string // "7.5", "24000/1001"
	first, later string // The first frame at or after the cut, and the next
	grid         string // The first AAC packet's time in samples
	fail         string // timing, frame, grid, nostream, noframe or nopacket
}

// shim puts the stand-ins first on PATH and returns the log path.
func (p probes) shim(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"ffprobe", "ffmpeg"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(probeShim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHIM_LOG", log)
	t.Setenv("SHIM_START", p.start)
	t.Setenv("SHIM_FPS", p.fps)
	t.Setenv("SHIM_FIRST", p.first)
	t.Setenv("SHIM_FIRST_LATER", p.later)
	t.Setenv("SHIM_GRID", p.grid)
	for name, set := range map[string]string{
		"SHIM_TIMING_FAILS": "timing", "SHIM_FRAME_FAILS": "frame", "SHIM_GRID_FAILS": "grid",
		"SHIM_NO_STREAM": "nostream", "SHIM_NO_FRAME": "noframe", "SHIM_NO_PACKET": "nopacket",
	} {
		v := ""
		if p.fail == set {
			v = "1"
		}
		t.Setenv(name, v)
	}
	return log
}

// asked is the probes Command started, one line each, in order.
func asked(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// callCommand is the one place the tests call Command, so that a change of its
// parameters is one edit here. JS-113 gives it an encoder description, which
// goes in at this call as x264.
func callCommand(ctx context.Context, input string, spec playback.Spec, start int, dir, ass string, image *playback.Stream) (*exec.Cmd, error) {
	return Command(ctx, input, spec, start, dir, ass, image, x264)
}

const sampleASS = "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
	"Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,First\n" +
	"Dialogue: 0,0:00:21.00,0:00:23.00,Default,,0,0,0,,Later\n"

// describe is everything Command decided, with the scratch directory named <dir>.
func describe(cmd *exec.Cmd, dir string) string {
	var b strings.Builder
	for _, a := range cmd.Args {
		fmt.Fprintf(&b, "arg %s\n", strings.ReplaceAll(a, dir, "<dir>"))
	}
	fmt.Fprintf(&b, "dir %s\n", strings.ReplaceAll(cmd.Dir, dir, "<dir>"))
	return b.String()
}
