package transcode

import (
	"context"
	"path/filepath"
	"testing"

	"jellyscale/internal/playback"
)

// Two cues on a black picture, one at the top and one at the bottom, so which
// cue a frame carries shows in where its bright pixels are. Times are as the
// track's own file has them, from the source's start.
const burnTrack = `[Script Info]
ScriptType: v4.00+
PlayResX: 640
PlayResY: 360

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Top,Arial,40,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1
Style: Bottom,Arial,40,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:06.00,0:00:10.00,Top,,0,0,0,,HIGH
Dialogue: 0,0:00:22.00,0:00:30.00,Bottom,,0,0,0,,LOW
`

// brightByHalf decodes a joined stream to one small grey frame per second and
// counts the bright pixels in the top and bottom half of each.
func brightByHalf(t *testing.T, file string) [][2]int {
	t.Helper()
	raw := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-an",
		"-vf", "setpts=PTS-STARTPTS,fps=1,scale=160:90,format=gray", "-f", "rawvideo", "-")
	const w, h = 160, 90
	var counts [][2]int
	for frame := []byte(raw); len(frame) >= w*h; frame = frame[w*h:] {
		var half [2]int
		for i, v := range frame[:w*h] {
			if v > 128 {
				half[i/(w*h/2)]++
			}
		}
		counts = append(counts, half)
	}
	return counts
}

// The text is in the picture at the cue's time, from a run that starts at
// zero and from restarts at a far seek, whatever the source's own start time.
func TestBurnedSubtitleIsAtTheRightTime(t *testing.T) {
	needFFmpeg(t)
	work := t.TempDir()
	black := filepath.Join(work, "black.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24000/1001:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "240", "-c:a", "flac", black)
	shifted := filepath.Join(work, "shifted.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", black, "-c", "copy", "-output_ts_offset", "7.5", shifted)

	index := 2
	burnt := testSpec
	burnt.Subtitle = &index
	build := func(input string, spec playback.Spec, start int) string {
		dir := t.TempDir()
		cmd, err := Command(context.Background(), input, spec, start, dir, burnTrack)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, out)
		}
		return dir
	}
	const total = 10
	// Where a frame at this second has bright pixels: the top, the bottom, or
	// neither. Seconds near a cue's edge are left out, because the sampled
	// frame may fall either side.
	const none, top, bottom, edge = 0, 1, 2, 3
	want := func(second int) int {
		switch {
		case second >= 7 && second <= 9:
			return top
		case second >= 23 && second <= 29:
			return bottom
		case second <= 4 || (second >= 12 && second <= 20) || second >= 32:
			return none
		}
		return edge
	}
	for name, input := range map[string]string{"plain": black, "shifted": shifted} {
		t.Run(name, func(t *testing.T) {
			// Without the subtitle the picture is black, so the counts below can fail.
			plain := build(input, testSpec, 0)
			for second, half := range brightByHalf(t, join(t, filepath.Join(plain, "init.mp4"), segmentFiles(plain, 0, total)...)) {
				if half != [2]int{} {
					t.Fatalf("second %d is not black without a subtitle: %v", second, half)
				}
			}
			whole := build(input, burnt, 0)
			for _, restart := range []int{0, 3, 6, 9} {
				dir := whole
				if restart > 0 {
					dir = build(input, burnt, restart)
				}
				spliced := join(t, filepath.Join(whole, "init.mp4"), append(segmentFiles(whole, 0, restart), segmentFiles(dir, restart, total)...)...)
				frames := brightByHalf(t, spliced)
				if len(frames) < 39 {
					t.Fatalf("restart %d: %d frames", restart, len(frames))
				}
				for second, half := range frames {
					got := none
					switch {
					case half[0] >= 20 && half[1] < 20:
						got = top
					case half[1] >= 20 && half[0] < 20:
						got = bottom
					case half != [2]int{}:
						got = edge // Bright in both halves, or too little to be text.
					}
					if expect := want(second); expect != edge && got != expect {
						t.Errorf("restart %d, second %d: bright pixels top %d, bottom %d; want %v (0 none, 1 top, 2 bottom)", restart, second, half[0], half[1], expect)
					}
				}
			}
		})
	}
}
