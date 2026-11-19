package transcode

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// JS-134: the ffmpeg arguments of a fallback. The SDR fallbacks and the H.264 one keep the arguments and the 8-bit picture
// they have today; the HEVC fallback of an HDR copy has no tone-mapping filter and no 8-bit pixel format on the way.

// hdrFallbackSpec is playback's hdrFallback: the spec of the encode that stands in for a copy, as internal/api's goOn spells
// it today. If the build spells it another way, this and its twin in internal/playback/hdr_fallback_test.go follow it.
func hdrFallbackSpec(copy playback.Spec, codec, transfer string) playback.Spec {
	return copy.Recoded(codec, transfer)
}

// fallbackArgs is the command RecodeCommand builds for a copy's fallback with no audio, from the start of the file (so no
// ffprobe is run).
func fallbackArgs(t *testing.T, spec playback.Spec, codec string) []string {
	t.Helper()
	spec.AudioIndex = -1
	cmd, err := RecodeCommand(context.Background(), "film.mkv", spec, codec, -1, -1, []float64{0, 5, 11}, "/out")
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Args
}

func option(args []string, name string) string {
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

var eightBit = regexp.MustCompile(`(^|[ ,=:])yuv420p([ ,:]|$)`)

const (
	scaled = "yadif=mode=send_frame:parity=auto:deint=interlaced,scale=640:360:flags=bicubic"
	// Today's x265 and x264 arguments of a fallback, with the bitrate of the copy below (capped at 6 bits a pixel by Recoded: 1 382 400 b/s).
	x265Today = "-c:v libx265 -preset veryfast -tag:v hvc1 -crf 26 -x265-params bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000:vbv-maxrate=1382:vbv-bufsize=2764:log-level=error -forced-idr 1"
	x264Today = "-c:v libx264 -preset veryfast -profile:v high -level 51 -crf 23 -maxrate 1382400 -bufsize 2764800 -x264-params bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000 -forced-idr 1"
)

var smallCopy = playback.Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, Width: 640, Height: 360, VideoBitrate: 1_500_000, Copy: true}

// The fallback of an SDR copy (8 or 10 bit) and of an H.264 copy in any range has the arguments and the 8-bit picture it
// has today. An SDR HEVC copy of 10 bits is as the 8-bit one: the fallback is Main at 8 bits.
func TestTheFallbackOfAnSDRCopyAndOfAnH264CopyKeepsItsArguments(t *testing.T) {
	for name, c := range map[string]struct {
		codec, transfer string
		vf, encode      string
	}{
		"sdr hevc":                  {"hevc", "", scaled + ",format=yuv420p", x265Today},
		"sdr h264":                  {"h264", "", scaled + ",format=yuv420p", x264Today},
		"h264 in PQ (tone mapped)":  {"h264", playback.PQ, scaled + "," + playback.ToneMapFilter(playback.PQ) + ",format=yuv420p", x264Today},
		"h264 in HLG (tone mapped)": {"h264", playback.HLG, scaled + "," + playback.ToneMapFilter(playback.HLG) + ",format=yuv420p", x264Today},
	} {
		t.Run(name, func(t *testing.T) {
			args := fallbackArgs(t, hdrFallbackSpec(smallCopy, c.codec, c.transfer), c.codec)
			if got := option(args, "-vf"); got != c.vf {
				t.Errorf("-vf is\n%s\nwant\n%s", got, c.vf)
			}
			line := strings.Join(args, " ")
			if !strings.Contains(line, c.encode) {
				t.Errorf("the encoder's arguments are not today's:\n%s\nwant them to contain\n%s", line, c.encode)
			}
			if strings.Contains(line, "yuv420p10") || strings.Contains(line, "-color_primaries") || strings.Contains(line, "-color_trc") {
				t.Errorf("an 8-bit fallback names a 10-bit format or colour tags: %s", line)
			}
		})
	}
}

// The HEVC fallback of an HDR copy (PQ or HLG) is the copy's own picture at 10 bits: no tone-mapping filter and no 8-bit
// pixel format on the way, the encoder at 10 bits (Main 10, which the master playlist's CODECS names).
func TestTheHEVCFallbackOfAnHDRCopyHasNoToneMapAndNo8BitFormat(t *testing.T) {
	for _, transfer := range []string{playback.PQ, playback.HLG} {
		t.Run(transfer, func(t *testing.T) {
			args := fallbackArgs(t, hdrFallbackSpec(smallCopy, "hevc", transfer), "hevc")
			line := strings.Join(args, " ")
			for _, banned := range []string{"tonemap", "zscale", "gbrpf32le", "-pix_fmt yuv420p "} {
				if strings.Contains(line, banned) {
					t.Errorf("the fallback of an HDR copy has %q: %s", banned, line)
				}
			}
			if eightBit.MatchString(line) {
				t.Errorf("an 8-bit pixel format is on the way: %s", line)
			}
			if !strings.Contains(line, "yuv420p10le") {
				t.Errorf("the picture is not made 10-bit: %s", line)
			}
			if !strings.Contains(line, "libx265") {
				t.Errorf("not an x265 encode: %s", line)
			}
		})
	}
}
