//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// hdrSource is a scanned HDR film, as the scan would store it: the labels, the
// keyframes and the end of the video.
func hdrSource(t *testing.T, film testenv.HDRFilm) (path string, source playback.Source) {
	t.Helper()
	testenv.FFmpeg(t)
	dir := t.TempDir()
	path = filepath.Join(dir, "film.mp4")
	if err := testenv.MakeHDRFilm(path, film); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyframes, err := library.ReadKeyframes(ctx, store.NewPosix(dir), "film.mp4", 1<<40, "mp4")
	if err != nil {
		t.Fatal(err)
	}
	end, err := library.ReadVideoEnd(ctx, path, keyframes[len(keyframes)-1])
	if err != nil {
		t.Fatal(err)
	}
	probe, err := library.ProbeInput(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(probe.Streams)
	streams, err := playback.ParseStreams(raw)
	if err != nil {
		t.Fatal(err)
	}
	return path, playback.Source{Container: "mp4", RunTimeTicks: probe.RunTimeTicks, Keyframes: keyframes, VideoEnd: end, Streams: streams}
}

// Copying HDR video goes through the worker's own ffmpeg path (CopyCommand and
// the gatherer, as a copy job runs them): the init segment has an hvc1 sample
// entry, the Dolby Vision box is dropped for profiles 8.1 and 7 (a client
// without Dolby Vision plays the base layer, as Jellyfin sends it), the
// picture keeps its transfer function, every segment is built, and the master
// playlist says PQ or HLG, never SDR.
func TestCopyOfHDRKeepsTheBaseLayerAndNamesItsRange(t *testing.T) {
	for _, test := range []struct {
		name     string
		film     testenv.HDRFilm
		wantType string // The scan's VideoRangeType.
		wantTRC  string
		wantMeta string // VIDEO-RANGE of the master.
		wantSupp bool   // Dolby Vision brand in SUPPLEMENTAL-CODECS.
	}{
		{"hdr10", testenv.HDRFilm{Base: "pq", Seconds: 12}, "HDR10", "smpte2084", "PQ", false},
		{"hlg", testenv.HDRFilm{Base: "hlg", Seconds: 12}, "HLG", "arib-std-b67", "HLG", false},
		{"dolby vision 8.1", testenv.HDRFilm{Base: "pq", DVProfile: 8, DVCompat: 1, Seconds: 12}, "DOVIWithHDR10", "smpte2084", "PQ", true},
		{"dolby vision 7", testenv.HDRFilm{Base: "pq", DVProfile: 7, DVCompat: 6, DVEnhancement: true, Seconds: 12}, "DOVIWithEL", "smpte2084", "PQ", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, source := hdrSource(t, test.film)
			if data, _ := os.ReadFile(input); test.film.DVProfile > 0 && !bytes.Contains(data, []byte("dvcC")) && !bytes.Contains(data, []byte("dvvC")) {
				t.Fatal("the source has no Dolby Vision box, so this test proves nothing")
			}
			if got := source.Streams[0].VideoRangeType; got != test.wantType {
				t.Fatalf("the scan labels it %q, want %q", got, test.wantType)
			}
			if !source.CanCopy() {
				t.Errorf("CanCopy is false for %s", test.wantType)
			}
			dir, layout := copySegments(t, input, source, 0)
			init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(init, []byte("hvc1")) || bytes.Contains(init, []byte("hev1")) {
				t.Error("the sample entry is not hvc1")
			}
			for _, box := range []string{"dvcC", "dvvC"} {
				if bytes.Contains(init, []byte(box)) {
					t.Errorf("the init segment carries a %s box", box)
				}
			}
			segment, err := os.ReadFile(filepath.Join(dir, "0.m4s"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := ffprobeFrom(context.Background(), bytes.NewReader(append(init, segment...)), "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,pix_fmt,color_transfer,color_primaries", "-of", "default=nw=1", "-")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"codec_name=hevc", "pix_fmt=yuv420p10le", "color_transfer=" + test.wantTRC, "color_primaries=bt2020"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("the copied picture lacks %s:\n%s", want, out)
				}
			}
			for n := 0; n < layout.Count(); n++ {
				if _, err := os.Stat(filepath.Join(dir, strconv.Itoa(n)+".m4s")); err != nil {
					t.Errorf("segment %d: %v", n, err)
				}
			}
			text := playback.Master(playback.Spec{Copy: true, Width: 640, Height: 360, VideoBitrate: 4_000_000, AudioBitrate: 128_000}, source, "q")
			first := strings.Split(strings.SplitN(text, "#EXT-X-STREAM-INF:", 2)[1], "\n")[0]
			if !strings.Contains(first, "VIDEO-RANGE="+test.wantMeta+",") || !regexp.MustCompile(`CODECS="hvc1\.2\.\d+\.L\d+\.B0,mp4a\.40\.2"`).MatchString(first) {
				t.Errorf("master: %s", first)
			}
			if got := strings.Contains(first, "SUPPLEMENTAL-CODECS="); got != test.wantSupp {
				t.Errorf("SUPPLEMENTAL-CODECS present %v, want %v: %s", got, test.wantSupp, first)
			}
		})
	}
}
