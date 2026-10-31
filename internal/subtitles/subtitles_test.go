package subtitles

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shapes are those captured from Jellyfin 12.4 (compat/captures/transcode-12.4.md).
var track = Track{
	VTT: "WEBVTT\n\n00:02.000 --> 00:30.000\nEnglish SRT line one\n\n00:30.500 --> 01:20.000\nEnglish SRT line two\n",
	SRT: "1\n00:00:02,000 --> 00:00:30,000\nEnglish SRT line one\n\n2\n00:00:30,500 --> 00:01:20,000\nEnglish SRT line two\n\n",
	ASS: "[Script Info]\nPlayResX: 1280\n\n[V4+ Styles]\nStyle: Default,Arial,48\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:02.00,0:00:30.00,Default,,0,0,0,,Line, with a comma 0:00:05.00\nDialogue: 0,0:00:30.50,1:01:20.00,Default,,0,0,0,,Two\n",
}

func TestRenderUnshifted(t *testing.T) {
	vtt, contentType, _ := track.Render("vtt", 0)
	want := "WEBVTT\n\n00:00:02.000 --> 00:00:30.000\nEnglish SRT line one\n\n00:00:30.500 --> 00:01:20.000\nEnglish SRT line two\n"
	if vtt != want || contentType != "text/vtt" {
		t.Errorf("vtt %q %s", vtt, contentType)
	}
	srt, contentType, _ := track.Render("srt", 0)
	if srt != track.SRT || contentType != "application/x-subrip" {
		t.Errorf("srt %q %s", srt, contentType)
	}
	ass, contentType, _ := track.Render("ass", 0)
	if ass != track.ASS || contentType != "text/x-ssa" {
		t.Errorf("ass %q %s", ass, contentType)
	}
	js, contentType, _ := track.Render("js", 0)
	if js != `{"TrackEvents":[{"Id":"1","Text":"English SRT line one","StartPositionTicks":20000000,"EndPositionTicks":300000000},`+
		`{"Id":"2","Text":"English SRT line two","StartPositionTicks":305000000,"EndPositionTicks":800000000}]}` || contentType != "application/json" {
		t.Errorf("js %s %s", js, contentType)
	}
	if _, _, ok := track.Render("txt", 0); ok {
		t.Error("an unknown format rendered")
	}
}

// 300000000 ticks is the capture's 30 seconds: cues move earlier by that and
// stop at zero.
func TestRenderShiftsAndClamps(t *testing.T) {
	vtt, _, _ := track.Render("vtt", 300000000)
	if want := "WEBVTT\n\n00:00:00.000 --> 00:00:00.000\nEnglish SRT line one\n\n00:00:00.500 --> 00:00:50.000\nEnglish SRT line two\n"; vtt != want {
		t.Errorf("vtt:\n%s", vtt)
	}
	srt, _, _ := track.Render("srt", 300000000)
	if !strings.Contains(srt, "00:00:00,500 --> 00:00:50,000") {
		t.Errorf("srt:\n%s", srt)
	}
	ass, _, _ := track.Render("ass", 300000000)
	// Only the two time fields move: the text keeps its comma and its clock.
	if !strings.Contains(ass, "Dialogue: 0,0:00:00.00,0:00:00.00,Default,,0,0,0,,Line, with a comma 0:00:05.00\n") ||
		!strings.Contains(ass, "Dialogue: 0,0:00:00.50,1:00:50.00,Default,,0,0,0,,Two") || !strings.Contains(ass, "Style: Default,Arial,48") {
		t.Errorf("ass:\n%s", ass)
	}
	js, _, _ := track.Render("js", 300000000)
	var parsed struct {
		TrackEvents []struct{ StartPositionTicks, EndPositionTicks int64 }
	}
	if err := json.Unmarshal([]byte(js), &parsed); err != nil || len(parsed.TrackEvents) != 2 ||
		parsed.TrackEvents[0].EndPositionTicks != 0 || parsed.TrackEvents[1].StartPositionTicks != 5000000 {
		t.Errorf("js %s %v", js, err)
	}
}

func TestDelayMovesCuesLater(t *testing.T) {
	if got := Delay("Dialogue: 0,0:00:02.00,0:00:30.00,Default,,0,0,0,,x", 7.5); !strings.Contains(got, "0,0:00:09.50,0:00:37.50,") {
		t.Errorf("got %q", got)
	}
}

// Extraction takes each format from one run over a real file, from SubRip and
// from ASS, and ASS keeps its styles.
func TestExtractFromAFile(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
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
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
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
