// Package library turns stored objects into items.
package library

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var videoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".webm": true, ".ts": true, ".m2ts": true, ".wmv": true, ".mpg": true, ".mpeg": true,
}

var (
	titleYear   = regexp.MustCompile(`^(.*?)[\s._]*[\(\[](\d{4})[\)\]]`)
	bareYear    = regexp.MustCompile(`^(.*?)[\s._]+((?:19|20)\d{2})(?:[\s._]|$)`)
	episodeTag  = regexp.MustCompile(`(?i)\bs(\d{1,3})[\s._-]*e(\d{1,4})\b`)
	episodeAltX = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,4})\b`)
	seasonDir   = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._-]*(\d{1,3})$`)
	leadArticle = regexp.MustCompile(`(?i)^(the|a|an)\s+`)
)

// IsVideo reports whether key looks like a playable video, skipping samples.
func IsVideo(key string) bool {
	if !videoExtensions[strings.ToLower(path.Ext(key))] {
		return false
	}
	stem := strings.ToLower(strings.TrimSuffix(path.Base(key), path.Ext(key)))
	return stem != "sample" && !strings.HasSuffix(stem, "-sample") && !strings.HasSuffix(stem, ".sample") &&
		!strings.HasSuffix(stem, "-trailer")
}

// Movie is what a path says about a film.
type Movie struct {
	Title string
	Year  int
}

// ParseMovie reads "Title (Year)/Title (Year).mkv", preferring the folder,
// which is usually the better-curated name.
func ParseMovie(key string) Movie {
	stem := strings.TrimSuffix(path.Base(key), path.Ext(key))
	if dir := path.Base(path.Dir(key)); dir != "." {
		if movie, ok := titleWithYear(dir); ok {
			return movie
		}
	}
	if movie, ok := titleWithYear(stem); ok {
		return movie
	}
	return Movie{Title: cleanTitle(stem)}
}

func titleWithYear(name string) (Movie, bool) {
	for _, pattern := range []*regexp.Regexp{titleYear, bareYear} {
		if match := pattern.FindStringSubmatch(name); match != nil && strings.TrimSpace(match[1]) != "" {
			year, _ := strconv.Atoi(match[2]) // Four digits, so it parses.
			return Movie{Title: cleanTitle(match[1]), Year: year}, true
		}
	}
	return Movie{}, false
}

// Episode is what a path says about one TV episode.
type Episode struct {
	Series string
	Season int
	Number int
	Title  string
}

// ParseEpisode reads "Show/Season 01/Show S01E02 Title.mkv". The series name is
// the top folder, so a file sitting directly in the library isn't an episode.
func ParseEpisode(key string) (Episode, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return Episode{}, false
	}
	stem := strings.TrimSuffix(parts[len(parts)-1], path.Ext(key))

	var match []int
	for _, pattern := range []*regexp.Regexp{episodeTag, episodeAltX} {
		if match = pattern.FindStringSubmatchIndex(stem); match != nil {
			break
		}
	}
	if match == nil {
		return Episode{}, false
	}
	season, _ := strconv.Atoi(stem[match[2]:match[3]])
	number, _ := strconv.Atoi(stem[match[4]:match[5]])
	rest := stem[match[1]:]

	// A season folder wins over the filename, which is often the less reliable.
	if len(parts) >= 3 {
		if match := seasonDir.FindStringSubmatch(parts[len(parts)-2]); match != nil {
			season, _ = strconv.Atoi(match[1])
		}
	}
	series := parts[0]
	if movie, ok := titleWithYear(series); ok {
		series = movie.Title
	}
	return Episode{
		Series: cleanTitle(series),
		Season: season,
		Number: number,
		Title:  cleanTitle(strings.TrimLeft(rest, " ._-")),
	}, true
}

func cleanTitle(name string) string {
	if !strings.Contains(name, " ") {
		name = strings.NewReplacer(".", " ", "_", " ").Replace(name)
	}
	return strings.TrimFunc(name, func(r rune) bool { return unicode.IsSpace(r) || r == '-' })
}

// SortName lowercases and moves a leading article out of the way.
func SortName(name string) string {
	return leadArticle.ReplaceAllString(strings.ToLower(name), "")
}
