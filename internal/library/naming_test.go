package library

import "testing"

func TestParseMovie(t *testing.T) {
	cases := []struct {
		key  string
		want Movie
	}{
		{"Blade Runner (1982)/Blade Runner (1982).mkv", Movie{"Blade Runner", 1982}},
		{"Blade Runner (1982).mkv", Movie{"Blade Runner", 1982}},
		{"Blade Runner [1982]/movie.mkv", Movie{"Blade Runner", 1982}},
		{"Blade.Runner.1982.1080p.BluRay.mkv", Movie{"Blade Runner", 1982}},
		{"Tenet.mkv", Movie{Title: "Tenet"}},
		{"2001 A Space Odyssey (1968)/film.mp4", Movie{"2001 A Space Odyssey", 1968}},
	}
	for _, tc := range cases {
		if got := ParseMovie(tc.key); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.key, got, tc.want)
		}
	}
}

func TestParseEpisode(t *testing.T) {
	cases := []struct {
		key  string
		want Episode
		ok   bool
	}{
		{"Severance/Season 02/Severance S02E03 Who Is Alive.mkv", Episode{"Severance", 2, 3, "Who Is Alive"}, true},
		{"The Wire (2002)/Season 1/The.Wire.S01E05.The.Pager.mkv", Episode{"The Wire", 1, 5, "The Pager"}, true},
		{"Show/s01e02.mkv", Episode{"Show", 1, 2, ""}, true},
		{"Show/1x07 Title.mkv", Episode{"Show", 1, 7, "Title"}, true},
		{"Show/Season 3/Show S01E04.mkv", Episode{"Show", 3, 4, ""}, true},
		{"Show S01E02.mkv", Episode{}, false},
		{"Show/extras/behind the scenes.mkv", Episode{}, false},
	}
	for _, tc := range cases {
		got, ok := ParseEpisode(tc.key)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got %+v %v, want %+v %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsVideo(t *testing.T) {
	for key, want := range map[string]bool{
		"a/film.mkv": true, "a/FILM.MP4": true, "a/poster.jpg": false, "a/film.srt": false,
		"a/sample.mkv": false, "a/film-sample.mkv": false, "a/film-trailer.mp4": false,
	} {
		if got := IsVideo(key); got != want {
			t.Errorf("%s: got %v", key, got)
		}
	}
}

func TestSortName(t *testing.T) {
	for name, want := range map[string]string{"The Wire": "wire", "A Quiet Place": "quiet place", "Theory": "theory"} {
		if got := SortName(name); got != want {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestPlayableStreams(t *testing.T) {
	stream := func(kind string, isDefault bool) map[string]any {
		return map[string]any{"Type": kind, "IsDefault": isDefault}
	}
	video, first, flagged, subtitle := stream("Video", true), stream("Audio", false), stream("Audio", true), stream("Subtitle", true)
	cases := []struct {
		name string
		in   []map[string]any
		want int
	}{
		{"flagged audio wins", []map[string]any{video, first, flagged, subtitle}, 2},
		{"first audio without a flag", []map[string]any{video, first, stream("Audio", false)}, 2},
		{"no audio", []map[string]any{video, subtitle}, 1},
	}
	for _, tc := range cases {
		got := playableStreams(tc.in)
		if len(got) != tc.want {
			t.Errorf("%s: got %d streams", tc.name, len(got))
		}
		for _, entry := range got {
			if entry["Type"] == "Subtitle" {
				t.Errorf("%s: subtitle offered", tc.name)
			}
		}
	}
	probed := map[string]any{"Type": "Video", "Level": 41, "IsInterlaced": false, "VideoRangeType": "SDR", "DurationTicks": 5, "Width": 1}
	if got := playableStreams([]map[string]any{probed}); len(got[0]) != 2 || probed["Level"] != 41 {
		t.Errorf("the DTO's video stream carries the probe's decision fields, or the stored one lost them: %v %v", got[0], probed)
	}
	if got := playableStreams([]map[string]any{video, first, flagged}); got[1]["IsDefault"] != true {
		t.Error("kept the wrong audio stream")
	}
}
