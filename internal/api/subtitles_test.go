package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"jellyscale/internal/playback"
)

// jellyfin-web reads DefaultSubtitleStreamIndex as a position in MediaStreams,
// so it is only given where the position holds that track.
func TestChosenSubtitleNeedsItsPosition(t *testing.T) {
	stream := func(index int, kind string) any { return map[string]any{"Index": float64(index), "Type": kind} }
	// A second audio track the list leaves out puts the subtitle at position 2, Index 3.
	source := map[string]any{"MediaStreams": []any{stream(0, "Video"), stream(1, "Audio"), stream(3, "Subtitle")}}
	three, two := playback.Number(3), playback.Number(2)
	if index, ok := chosenSubtitle(source, &three); ok {
		t.Errorf("track 3 sits at position 2, yet answered %d", index)
	}
	if index, ok := chosenSubtitle(source, &two); ok {
		t.Errorf("position 2 isn't track 2, yet answered %d", index)
	}
	source["MediaStreams"] = []any{stream(0, "Video"), stream(1, "Audio"), stream(2, "Subtitle")}
	if index, ok := chosenSubtitle(source, &two); !ok || index != 2 {
		t.Errorf("track at its own position: %d %v", index, ok)
	}
}

// The playlist is what Jellyfin 12.4 answered for a film of 30.023 s (observed
// at segment lengths 6, 30 and 31): a segment per segmentLength seconds, the
// last the remainder, and the caller's token on each.
func TestSubtitlePlaylist(t *testing.T) {
	const runtime = 300230000
	segment := func(from, to int64) string {
		return "stream.vtt?CopyTimestamps=true&AddVttTimeMap=true&StartPositionTicks=" + strconv.FormatInt(from, 10) +
			"&EndPositionTicks=" + strconv.FormatInt(to, 10) + "&ApiKey=tok%2F1\n"
	}
	head := func(length string) string {
		return "#EXTM3U\n#EXT-X-TARGETDURATION:" + length + "\n#EXT-X-VERSION:3\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n"
	}
	for length, want := range map[int64]string{
		30: head("30") + "#EXTINF:30,\n" + segment(0, 300000000) + "#EXTINF:0.023,\n" + segment(300000000, runtime) + "#EXT-X-ENDLIST\n",
		31: head("31") + "#EXTINF:30.023,\n" + segment(0, runtime) + "#EXT-X-ENDLIST\n",
	} {
		if got := subtitlePlaylist(length, runtime, "tok/1"); got != want {
			t.Errorf("segment length %d:\n%s\nwant\n%s", length, got, want)
		}
	}
	if got := subtitlePlaylist(6, runtime, "tok/1"); strings.Count(got, "#EXTINF:6,\n") != 5 || !strings.Contains(got, "#EXTINF:0.023,\n"+segment(300000000, runtime)) {
		t.Errorf("segment length 6:\n%s", got)
	}
}

// A font the store cuts short, by an error or silently, is answered with the
// empty 200 of a font that can't be delivered, with no ETag for a client to
// keep it under.
func TestCutFontBodyIsAnsweredEmpty(t *testing.T) {
	s := &Server{Log: slog.New(slog.DiscardHandler)}
	const font = "a whole font"
	sum := sha256.Sum256([]byte(font))
	for name, c := range map[string]struct {
		body io.Reader
		want string
	}{
		"whole":        {strings.NewReader(font), font},
		"error":        {io.MultiReader(strings.NewReader(font[:6]), iotest.ErrReader(io.ErrUnexpectedEOF)), ""},
		"silent short": {strings.NewReader(font[:6]), ""},
	} {
		w := httptest.NewRecorder()
		w.Header().Set("ETag", "kept")
		s.sendFont(w, httptest.NewRequest("GET", "/font", nil), c.body, hex.EncodeToString(sum[:]), "font/ttf")
		if w.Code != 200 || w.Body.String() != c.want || (c.want == "" && (w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-store")) {
			t.Errorf("%s: %d %q %v", name, w.Code, w.Body.String(), w.Header())
		}
	}
}
