package subtitles

import (
	"encoding/json"
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
