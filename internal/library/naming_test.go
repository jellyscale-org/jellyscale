package library

import (
	"slices"
	"testing"

	"jellyscale/internal/media"
)

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
	stream := func(kind string, isDefault bool) media.MediaStream {
		return media.MediaStream{Type: kind, IsDefault: isDefault}
	}
	video, first, flagged, subtitle := stream("Video", true), stream("Audio", false), stream("Audio", true), stream("Subtitle", true)
	cases := []struct {
		name string
		in   []media.MediaStream
		want int
	}{
		{"flagged audio wins", []media.MediaStream{video, first, flagged, subtitle}, 2},
		{"first audio without a flag", []media.MediaStream{video, first, stream("Audio", false)}, 2},
		{"no audio", []media.MediaStream{video, subtitle}, 1},
	}
	for _, tc := range cases {
		got := playableStreams(tc.in)
		if len(got) != tc.want {
			t.Errorf("%s: got %d streams", tc.name, len(got))
		}
		for _, entry := range got {
			if entry.Type == "Subtitle" {
				t.Errorf("%s: subtitle offered", tc.name)
			}
		}
	}
	probed := media.MediaStream{Type: "Video", Level: 41, VideoRangeType: "SDR", DurationTicks: 5, Width: 1}
	got := playableStreams([]media.MediaStream{probed})
	if got[0].Level != 0 || got[0].DurationTicks != 0 || got[0].VideoRangeType != "SDR" || got[0].Width != 1 || probed.Level != 41 {
		t.Errorf("the DTO's video stream carries the probe's decision fields (the range labels it lists), or the stored one lost them: %v %v", got[0], probed)
	}
	if got := playableStreams([]media.MediaStream{video, first, flagged}); !got[1].IsDefault {
		t.Error("kept the wrong audio stream")
	}
}

// Subtitle tracks are listed when something delivers them: text ones, and the
// image ones a transcode draws. Any other bitmap codec stays out.
func TestPlayableStreamsListsTheImageTracksThatAreDelivered(t *testing.T) {
	text := media.MediaStream{Type: "Subtitle", Index: 1, IsTextSubtitleStream: true, IsImageSubtitleStream: media.Ptr(false)}
	pgs := media.MediaStream{Type: "Subtitle", Index: 2, Codec: "PGSSUB", IsImageSubtitleStream: media.Ptr(true)}
	dvb := media.MediaStream{Type: "Subtitle", Index: 3, Codec: "DVBSUB", IsImageSubtitleStream: media.Ptr(true)}
	other := media.MediaStream{Type: "Subtitle", Index: 4, Codec: "hdmv_text_subtitle", IsImageSubtitleStream: media.Ptr(false)}
	// Stored by a version that didn't know image tracks.
	old := media.MediaStream{Type: "Subtitle", Index: 5, Codec: "hdmv_pgs_subtitle"}
	var listed []int
	for _, stream := range playableStreams([]media.MediaStream{text, pgs, dvb, other, old}) {
		listed = append(listed, stream.Index)
	}
	if !slices.Equal(listed, []int{1, 2, 3}) {
		t.Errorf("listed tracks %v, want 1, 2 and 3", listed)
	}
}
