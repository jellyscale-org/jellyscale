//go:build integration

package subtitles

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// Extraction takes each format from one run over a real file, from SubRip and
// from ASS, and ASS keeps its styles.
func TestExtractFromAFile(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	srt := write("a.srt", "1\n00:00:02,000 --> 00:00:03,000\nOne\n\n2\n01:00:00,500 --> 01:00:04,000\nTwo\n")
	ass := write("b.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n"+
		"Style: Loud,Arial,48,&H0000FF00,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Loud,,0,0,0,,Styled\n")
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3",
		"-i", srt, "-i", ass, "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:s:0", "srt", "-c:s:1", "ass", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	fromSRT, err := Extract(context.Background(), film, 1)
	if err != nil {
		t.Fatal(err)
	}
	vtt, _, _ := fromSRT.Render("vtt", 0)
	if !strings.Contains(vtt, "00:00:02.000 --> 00:00:03.000\nOne") || !strings.Contains(vtt, "01:00:00.500 --> 01:00:04.000\nTwo") {
		t.Errorf("vtt from SubRip:\n%s", vtt)
	}
	fromASS, err := Extract(context.Background(), film, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fromASS.ASS, "Style: Loud,Arial,48,&H0000FF00") || !strings.Contains(fromASS.ASS, "Styled") {
		t.Errorf("ass lost its style:\n%s", fromASS.ASS)
	}
	// A stream that isn't a subtitle, or isn't there, is an error and not an empty track.
	for _, index := range []int{0, 9} {
		if _, err := Extract(context.Background(), film, index); err == nil {
			t.Errorf("stream %d extracted", index)
		}
	}
}

// A file cut short makes ffmpeg complain and still exit 0, with a track that
// is empty or partial; that is a failure, and the whole file works again.
func TestExtractFromACutFileFails(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:02,000 --> 00:00:03,000\nOne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=20",
		"-i", srt, "-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "srt", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	whole, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mkv")
	if err := os.WriteFile(cut, whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if track, err := Extract(context.Background(), cut, 1); err == nil {
		t.Fatalf("a cut file extracted: %+v", track)
	}
	if _, err := Extract(context.Background(), film, 1); err != nil {
		t.Fatalf("the whole file: %v", err)
	}
}
