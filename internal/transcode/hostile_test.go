package transcode

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// Everything a client can put in a transcode URL, however hostile, either is
// refused or reaches ffmpeg as an integer in a place a spec leaves for one.
func TestHostileQueryCannotShapeFFmpegArguments(t *testing.T) {
	source := playback.Source{ETag: "e", Container: "mkv", Streams: []playback.Stream{
		{Index: 0, Type: "Video", Codec: "h264", Width: 1280, Height: 720},
		{Index: 1, Type: "Audio", Codec: "aac", IsDefault: true},
		{Index: 2, Type: "Audio", Codec: "aac"},
	}}
	const id = "0123456789abcdef0123456789abcdef"
	// A hostile entry replaces the same key in the base query, so the value is
	// the one the parser sees.
	argv := func(base, hostile string) ([]string, error) {
		values, err := url.ParseQuery(base)
		if err != nil {
			t.Fatal(err)
		}
		extra, err := url.ParseQuery(hostile)
		if err != nil {
			t.Fatal(err)
		}
		for key, v := range extra {
			values[key] = v
		}
		params, err := playback.ParseParams(values, source)
		if err != nil {
			return nil, err
		}
		spec, err := playback.NewSpec(id, params, source)
		if err != nil {
			return nil, err
		}
		cmd, err := Command(context.Background(), "/media/film.mkv", spec, 0, "/scratch/job")
		if err != nil {
			return nil, err
		}
		return cmd.Args, nil
	}
	base := "mediasourceid=" + id + "&playsessionid=abc123"
	baseline, err := argv(base, "")
	if err != nil {
		t.Fatal(err)
	}

	// What may differ from the baseline: a bare integer, the audio map, or the
	// scale filter's two integers.
	allowed := regexp.MustCompile(`^(\d+|1:\d+|scale=\d+:\d+:flags=bicubic,format=yuv420p)$`)
	marker := regexp.MustCompile(`passwd|/etc|\.\./|;|\||\$|` + "`" + `|-f x|-i /|concat|file:|http`)

	hostile := []string{
		"audiostreamindex=1%3B-i%20/etc/passwd",
		"audiostreamindex=-i",
		"audiostreamindex=2%20-f%20x",
		"audiostreamindex=99999999999999999999",
		"videobitrate=1000000%20-i%20/etc/passwd",
		"videobitrate=0x10",
		"videobitrate=1e6",
		"videobitrate=-1",
		"audiobitrate=128000%0A-i%20/etc/passwd",
		"mediasourceid=../../../etc/passwd",
		"mediasourceid=..%2F..%2F..%2F..%2Fetc%2Fpasswd0000000000", // Right length, not hex.
		"mediasourceid=%3Bx%20/etc/passwd000000000000000000",
		"playsessionid=abc%3Brm%20-rf%20/",
		"videocodec=copy%20-i%20/etc/passwd",
		"audiocodec=copy",
		"segmentcontainer=ts%20-f%20x",
		"subtitlestreamindex=1%3B-i%20/etc/passwd",
		"subtitlemethod=Encode",
		"transcodereasons=x%20-i%20/etc/passwd",
		"tag=%24(id)",
		"deviceid=%24(id)%60id%60",
		"maxframerate=24%20-i%20/etc/passwd",
		"runtimeticks=1%3B-i%20/etc/passwd",
		"start=%3B-ss%2010",
		"-i=/etc/passwd",
	}
	for _, extra := range hostile {
		args, err := argv(base, extra)
		if err != nil {
			continue // Refused outright.
		}
		if len(args) != len(baseline) {
			t.Errorf("%s: %d arguments, baseline has %d", extra, len(args), len(baseline))
			continue
		}
		for i, arg := range args {
			if marker.MatchString(arg) && arg != baseline[i] {
				t.Errorf("%s: hostile text reached ffmpeg: %q", extra, arg)
			}
			if arg != baseline[i] && !allowed.MatchString(arg) {
				t.Errorf("%s: argument %d changed to %q", extra, i, arg)
			}
		}
	}

	// The paths ffmpeg reads and writes never come from the query.
	var inputs []string
	for i, arg := range baseline {
		if arg == "-i" {
			inputs = append(inputs, baseline[i+1])
		}
	}
	if !slices.Equal(inputs, []string{"/media/film.mkv", "/media/film.mkv"}) {
		t.Errorf("inputs: %v", inputs)
	}
	if last := baseline[len(baseline)-1]; !strings.HasPrefix(last, "/scratch/job/") {
		t.Errorf("output: %q", last)
	}
}
