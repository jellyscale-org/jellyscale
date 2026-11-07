package api

import (
	"slices"
	"testing"
)

func TestMediaTypesKeepOnlyFilmsAndEpisodesAsVideo(t *testing.T) {
	for _, c := range []struct {
		name         string
		media, types []string
		want         []string
		ok           bool
	}{
		{"none asked", nil, []string{"Series"}, []string{"Series"}, true},
		{"video", []string{"Video"}, nil, []string{"Movie", "Episode"}, true},
		{"any case", []string{"video"}, nil, []string{"Movie", "Episode"}, true},
		{"books", []string{"Book"}, nil, nil, false},
		{"audio", []string{"Audio"}, nil, nil, false},
		{"video among others", []string{"Audio", "Video"}, nil, []string{"Movie", "Episode"}, true},
		{"video narrows types", []string{"Video"}, []string{"Movie", "Series"}, []string{"Movie"}, true},
		{"folders have none", []string{"Video"}, []string{"Series", "Season"}, nil, false},
	} {
		got, ok := mediaTypeFilter(c.media, c.types)
		if ok != c.ok || !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
