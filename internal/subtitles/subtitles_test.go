package subtitles

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// Only an ffmpeg that ran to the end and found the data wrong makes a file
// permanently unreadable. One killed by a signal (the OOM killer's), or short
// of disk, says nothing about the file, so the same file is tried again later.
func TestOnlyAnErrorExitOnTheDataIsABadFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, c := range map[string]struct {
		script string
		bad    bool
	}{
		"killed by a signal": {"kill -9 $$", false},
		"disk full":          {"echo 'Error writing trailer: No space left on device' >&2; exit 1", false},
		"error status":       {"echo 'Invalid data found when processing input' >&2; exit 1", true},
		"complaint, exit 0":  {"echo 'truncated' >&2", true},
	} {
		if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Extract(context.Background(), "input.srt", 0)
		if err == nil || errors.Is(err, ErrBadFile) != c.bad {
			t.Errorf("%s: bad file %v, want %v (%v)", name, errors.Is(err, ErrBadFile), c.bad, err)
		}
	}
}

// A window keeps the cues that touch it, both ends included, as Jellyfin 12.4
// does (observed with cues ending at 6 s and starting at 12 s against the
// windows 0-6 s and 6-12 s).
func TestWithinKeepsTheCuesThatTouchTheWindow(t *testing.T) {
	cues := Track{
		VTT: "WEBVTT\n\n00:00:04.000 --> 00:00:06.000\nends at six\n\n00:00:12.000 --> 00:00:13.000\nstarts at twelve\n\n00:00:07.000 --> 00:00:09.000\ninside\n",
		SRT: "1\n00:00:04,000 --> 00:00:06,000\nends at six\n\n2\n00:00:12,000 --> 00:00:13,000\nstarts at twelve\n\n",
		ASS: "Dialogue: 0,0:00:30.00,0:00:31.00,Default,,0,0,0,,untouched\n",
	}
	const sec = ticksPerSecond
	for _, c := range []struct {
		start, end int64
		want       []string
	}{
		{0, 6 * sec, []string{"ends at six"}},
		{6 * sec, 12 * sec, []string{"ends at six", "starts at twelve", "inside"}},
		{6*sec + 1, 12*sec - 1, []string{"inside"}},
		{13*sec + 1, 0, nil},
		{12 * sec, 0, []string{"starts at twelve"}},
	} {
		got := cues.Within(c.start, c.end)
		for _, text := range []string{"ends at six", "starts at twelve", "inside"} {
			if has := strings.Contains(got.VTT, text); has != slices.Contains(c.want, text) {
				t.Errorf("window %d-%d: %q kept %v:\n%s", c.start, c.end, text, has, got.VTT)
			}
		}
		if !strings.HasPrefix(got.VTT, "WEBVTT") || got.ASS != cues.ASS {
			t.Errorf("window %d-%d lost the header or the ASS text: %q", c.start, c.end, got)
		}
	}
	if !strings.Contains(cues.Within(0, 6*sec).SRT, "ends at six") || strings.Contains(cues.Within(0, 6*sec).SRT, "twelve") {
		t.Errorf("SRT: %q", cues.Within(0, 6*sec).SRT)
	}
	if got := WithTimeMap("WEBVTT\n\n00:00:02.000 --> 00:00:04.000\nHello"); got != "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n00:00:02.000 --> 00:00:04.000\nHello" {
		t.Errorf("time map: %q", got)
	}
}
