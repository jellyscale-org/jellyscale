//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-134: the fallback of an HDR copy whose keyframe index was found wrong keeps the picture's range: 10 bits with the
// copy's tags and no tone mapping (compat/captures/hdr-fallback-12.4.md). Every check makes the film, the source and the
// spec as the worker's own path does (fallbackOf and hdrFallbackSpec are the places that spell how the api turns a copy into its
// fallback), runs a real worker and ffmpeg, and reads what the worker stored with ffprobe. Run them under
// test/ffmpeg61.sh as well: the criterion names Ubuntu 24.04's ffmpeg 6.1 and ffmpeg 9.

// hdrKeyframes are the keyframes of the short films (seconds): irregular, so the copy's layout differs from the 4 s grid.
var hdrKeyframes = []int{0, 5, 11, 16, 23, 29}

// filmOf is a synthetic film: a picture of codec (hevc, av1 or h264) at 10 bits (or 8) with the transfer's tags (smpte2084,
// arib-std-b67 or bt709), a keyframe at each of keyframes, AC3 audio, in Matroska.
type filmOf struct {
	name, codec, transfer string
	tenBit                bool
	keyframes             []int
	width, height         int
	rate                  string // As ffmpeg writes a frame rate; "" is 24.
	// primaries ("" is the transfer's usual ones) and fullRange make a film whose tags are not the ones a build could derive from
	// its transfer: a build that tags the fallback per transfer (bt2020 / bt2020nc / tv) changes them mid-film.
	primaries string
	fullRange bool
}

func (f filmOf) tags() string {
	primaries := map[string]string{"smpte2084": "bt2020", "arib-std-b67": "bt2020", "bt709": "bt709"}[f.transfer]
	if f.primaries != "" {
		primaries = f.primaries
	}
	rng := "tv"
	if f.fullRange {
		rng = "pc"
	}
	matrix := map[string]string{"smpte2084": "bt2020nc", "arib-std-b67": "bt2020nc", "bt709": "bt709"}[f.transfer]
	return fmt.Sprintf("setparams=color_primaries=%s:color_trc=%s:colorspace=%s:range=%s", primaries, f.transfer, matrix, rng)
}

func (f filmOf) svtParams() string {
	if f.fullRange {
		return "color-range=1"
	}
	return "color-range=0"
}

// make writes the film into the shared film directory and returns its file name. SVT-AV1 in ffmpeg 6.1 ignores
// -force_key_frames, so an AV1 picture is encoded in pieces that each start at a keyframe time and joined.
func (f filmOf) make(t *testing.T) string {
	t.Helper()
	testenv.FFmpeg(t)
	if f.width == 0 {
		f.width, f.height = 640, 360
	}
	rate := f.rate
	if rate == "" {
		rate = "24"
	}
	pix := map[bool]string{true: "yuv420p10le", false: "yuv420p"}[f.tenBit]
	duration := f.keyframes[len(f.keyframes)-1]
	source := fmt.Sprintf("testsrc2=size=%dx%d:rate=%s:duration=%d", f.width, f.height, rate, duration)
	out := filepath.Join(film(t), f.name+".mkv")
	audio := []string{"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", duration), "-map", "0:v", "-map", "1:a", "-c:a", "ac3"}
	forced := make([]string, len(f.keyframes))
	for i, k := range f.keyframes {
		forced[i] = strconv.Itoa(k)
	}
	switch f.codec {
	case "av1":
		pieces := t.TempDir()
		var list strings.Builder
		frameAt := func(seconds int) int {
			num, den := 24, 1
			if rate == "24000/1001" {
				num, den = 24000, 1001
			} else if rate == "25" {
				num = 25
			}
			return (seconds*num + den - 1) / den
		}
		for i := 0; i+1 < len(f.keyframes); i++ {
			piece := filepath.Join(pieces, strconv.Itoa(i)+".mkv")
			run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", source,
				"-vf", fmt.Sprintf("trim=start_frame=%d:end_frame=%d,setpts=PTS-STARTPTS,%s,format=%s", frameAt(f.keyframes[i]), frameAt(f.keyframes[i+1]), f.tags(), pix),
				"-c:v", "libsvtav1", "-preset", "10", "-g", "2000", "-svtav1-params", f.svtParams(), piece)
			fmt.Fprintf(&list, "file '%s'\n", piece)
		}
		if err := os.WriteFile(filepath.Join(pieces, "list"), []byte(list.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "ffmpeg", append(append([]string{"-nostdin", "-v", "error", "-y", "-f", "concat", "-safe", "0", "-i", filepath.Join(pieces, "list")}, audio...), "-c:v", "copy", out)...)
	case "hevc":
		x265 := "keyint=2000:scenecut=0:open-gop=0:log-level=error"
		if f.fullRange { // ffmpeg 6.1's libx265 writes tv whatever the frames say; the encoder's own flag says pc on 6.1 and 9.
			x265 += ":range=full"
		}
		if f.transfer == "smpte2084" {
			x265 += ":hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
		}
		run(t, "ffmpeg", append(append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", source}, audio...),
			"-vf", f.tags()+",format="+pix, "-c:v", "libx265", "-preset", "veryfast", "-x265-params", x265, "-forced-idr", "1",
			"-force_key_frames", strings.Join(forced, ","), out)...)
	case "h264":
		run(t, "ffmpeg", append(append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", source}, audio...),
			"-vf", f.tags()+",format="+pix, "-c:v", "libx264", "-preset", "ultrafast", "-g", "2000", "-sc_threshold", "0",
			"-force_key_frames", strings.Join(forced, ","), out)...)
	default:
		t.Fatalf("no film of codec %q", f.codec)
	}
	return filepath.Base(out)
}

// fallbackOf is the spec of the fallback of the copy of source, as internal/api's goOn makes it: the copy's codec and
// the source's tone map, through hdrFallbackSpec (hdr_fallback_args_test.go), the one place that spells it.
func fallbackOf(copy playback.Spec, source playback.Source) playback.Spec {
	return hdrFallbackSpec(copy, source.VideoCodec(), source.ToneMap())
}

// hdrFallbackRig points the rig at the film as a copy whose index the job found wrong: the index is in dropped_keyframes
// and the spec is the fallback of the copy.
func (r *rig) hdrFallbackRig(t *testing.T, name string) (input string, source playback.Source) {
	t.Helper()
	input = filepath.Join(film(t), name)
	container := strings.TrimPrefix(filepath.Ext(name), ".")
	source = scanned(t, input, container)
	r.useFile(t, name)
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = '{}', dropped_keyframes = $1, video_end = $2, size = $3, container = $4",
		source.Keyframes, source.VideoEnd, mustSize(t, input), container); err != nil {
		t.Fatal(err)
	}
	var width, height int
	for _, s := range source.Streams {
		if s.Type == "Video" {
			width, height = int(s.Width), int(s.Height)
		}
	}
	r.spec.AudioIndex, r.spec.AudioBitrate, r.spec.Width, r.spec.Height, r.spec.Copy = 1, 128_000, width, height, true
	r.spec = fallbackOf(r.spec, source)
	r.hash = r.spec.Hash()
	return input, source
}

// logFFmpeg puts in front of ffmpeg a shim that appends each command line to a file, and returns what it logged so far.
func logFFmpeg(t *testing.T) func() []string {
	t.Helper()
	real := testenv.FFmpeg(t)
	file := filepath.Join(t.TempDir(), "ffmpeg.log")
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "ffmpeg"), []byte("#!/bin/sh\necho \"$*\" >>"+file+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, _ := os.ReadFile(file)
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// encodes are the logged commands that encode the picture (not a copy, not the probes).
func encodes(lines []string) (out []string) {
	for _, l := range lines {
		if strings.Contains(l, "libx265") || strings.Contains(l, "libsvtav1") || strings.Contains(l, "libx264") {
			out = append(out, l)
		}
	}
	return out
}

// hdrRun asks a rig of its own for the segments in order, from a real worker, and returns where what the viewer was
// given is (init.mp4 and n.m4s, the names join and segmentFiles use), the layout and the encoding commands the worker ran.
func hdrRun(t *testing.T, name string, order []int) (dir string, layout playback.Layout, commands []string) {
	t.Helper()
	logged := logFFmpeg(t)
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	_, source := r.hdrFallbackRig(t, name)
	layout = source.Layout(true)
	stop := r.worker(t, nil)
	j := r.jobs(t)
	for _, n := range order {
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d of the fallback of %s, asked in the order %v: %v", n, name, order, err)
		}
	}
	if _, err := r.fetchAt(t, j, psid(0), -1, order[0]); err != nil {
		t.Fatalf("the fallback's init segment: %v", err)
	}
	stop()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "init.mp4"), readFile(t, filepath.Join(segments, SegmentKey(r.hash, -1))), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range order {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.m4s", n)), readFile(t, filepath.Join(segments, SegmentKey(r.hash, n))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, layout, encodes(logged())
}

// codedPicture is what ffprobe says of how a video is coded and tagged.
type codedPicture struct{ Profile, PixFmt, Primaries, Transfer, Matrix, Range string }

func (p codedPicture) String() string {
	return fmt.Sprintf("%s %s primaries %s transfer %s matrix %s range %s", p.Profile, p.PixFmt, p.Primaries, p.Transfer, p.Matrix, p.Range)
}

func probePicture(t *testing.T, file string) codedPicture {
	t.Helper()
	var out struct {
		Streams []struct {
			Profile   string `json:"profile"`
			PixFmt    string `json:"pix_fmt"`
			Primaries string `json:"color_primaries"`
			Transfer  string `json:"color_transfer"`
			Matrix    string `json:"color_space"`
			Range     string `json:"color_range"`
		} `json:"streams"`
	}
	raw := run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=profile,pix_fmt,color_primaries,color_transfer,color_space,color_range", "-of", "json", file)
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out.Streams) != 1 {
		t.Fatalf("ffprobe of %s: %v %s", file, err, raw)
	}
	s := out.Streams[0]
	return codedPicture{s.Profile, s.PixFmt, s.Primaries, s.Transfer, s.Matrix, s.Range}
}

// framePictures are the distinct pictures (format and tags) of every frame of the file, with how many frames have each.
func framePictures(t *testing.T, file string) map[codedPicture]int {
	t.Helper()
	var out struct {
		Frames []struct {
			PixFmt    string `json:"pix_fmt"`
			Primaries string `json:"color_primaries"`
			Transfer  string `json:"color_transfer"`
			Matrix    string `json:"color_space"`
			Range     string `json:"color_range"`
		} `json:"frames"`
	}
	raw := run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=pix_fmt,color_primaries,color_transfer,color_space,color_range", "-of", "json", file)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	got := map[codedPicture]int{}
	for _, f := range out.Frames {
		got[codedPicture{"", f.PixFmt, f.Primaries, f.Transfer, f.Matrix, f.Range}]++
	}
	return got
}

// A viewer's segment as it is answered has its init in front.
func answered(t *testing.T, dir string, n int) string {
	t.Helper()
	return join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, n, n+1)...)
}

type hdrCase struct {
	name, codec string
	// make returns the name of the film in the film directory.
	make   func(t *testing.T) string
	tenBit bool // Whether the fallback is the picture at 10 bits with the copy's tags.
	// unusual names the one tag a taggedFilm changed ("primaries=smpte432", "range=pc"), so the check cannot pass on a film
	// whose tags came out as the usual ones.
	unusual string
}

func plainFilm(name, codec, transfer string, tenBit bool) hdrCase {
	return hdrCase{name: name, codec: codec, tenBit: tenBit && transfer != "bt709", make: func(t *testing.T) string {
		return filmOf{name: strings.ReplaceAll(name, " ", "-"), codec: codec, transfer: transfer, tenBit: tenBit, keyframes: hdrKeyframes}.make(t)
	}}
}

// taggedFilm is a PQ film whose primaries or range are not the usual ones (the range type is derived from the transfer alone,
// so it is HDR10, is copied and falls back): the fallback must carry the copy's own tags, not ones derived from the transfer.
func taggedFilm(name, codec, primaries string, fullRange bool) hdrCase {
	unusual := "primaries=" + primaries
	if fullRange {
		unusual = "range=pc"
	}
	return hdrCase{name: name, codec: codec, tenBit: true, unusual: unusual, make: func(t *testing.T) string {
		return filmOf{name: strings.ReplaceAll(name, " ", "-"), codec: codec, transfer: "smpte2084", tenBit: true, keyframes: hdrKeyframes, primaries: primaries, fullRange: fullRange}.make(t)
	}}
}

func labelledFilm(name string, f testenv.HDRFilm, tenBit bool) hdrCase {
	codec := "hevc"
	if f.Base == "av1" {
		codec = "av1"
	}
	return hdrCase{name: name, codec: codec, tenBit: tenBit, make: func(t *testing.T) string {
		file := strings.ReplaceAll(name, " ", "-") + ".mp4"
		f.Seconds = 12
		if err := testenv.MakeHDRFilm(filepath.Join(film(t), file), f); err != nil {
			t.Fatal(err)
		}
		return file
	}}
}

var hdrCases = []hdrCase{
	plainFilm("hdr10 hevc", "hevc", "smpte2084", true),
	plainFilm("hlg hevc", "hevc", "arib-std-b67", true),
	plainFilm("hdr10 av1", "av1", "smpte2084", true),
	plainFilm("hlg av1", "av1", "arib-std-b67", true),
	// Tags no build can derive from the transfer: P3 primaries, and full range (the copy's own tags go on across the change).
	taggedFilm("pq smpte432 primaries hevc", "hevc", "smpte432", false),
	taggedFilm("pq smpte432 primaries av1", "av1", "smpte432", false),
	taggedFilm("pq full range hevc", "hevc", "", true),
	taggedFilm("pq full range av1", "av1", "", true),
	labelledFilm("hdr10plus hevc", testenv.HDRFilm{Base: "pq", HDR10Plus: true}, true),
	// Dolby Vision: the base layer decides (profiles 8.1 and 7 in HEVC, 10.1 in AV1 are HDR10, 8.4 is HLG, 8.2 is SDR).
	labelledFilm("dolby vision 8.1", testenv.HDRFilm{Base: "pq", DVProfile: 8, DVCompat: 1}, true),
	labelledFilm("dolby vision 7", testenv.HDRFilm{Base: "pq", DVProfile: 7, DVCompat: 6, DVEnhancement: true}, true),
	labelledFilm("dolby vision 10.1 av1", testenv.HDRFilm{Base: "av1", DVProfile: 10, DVCompat: 1}, true),
	labelledFilm("dolby vision 8.4", testenv.HDRFilm{Base: "hlg", DVProfile: 8, DVCompat: 4}, true),
	labelledFilm("dolby vision 8.2", testenv.HDRFilm{Base: "sdr", DVProfile: 8, DVCompat: 2}, false),
	// An SDR copy (8 or 10 bit; H.264, HEVC or AV1) goes on at 8 bits as it did.
	plainFilm("sdr 8-bit hevc", "hevc", "bt709", false),
	plainFilm("sdr 10-bit hevc", "hevc", "bt709", true),
	plainFilm("sdr 8-bit av1", "av1", "bt709", false),
	plainFilm("sdr 10-bit av1", "av1", "bt709", true),
	plainFilm("sdr 8-bit h264", "h264", "bt709", false),
}

// The fallback of each kind of copy, with a real worker and ffmpeg: an HDR picture (HDR10, HLG, HDR10+ as its HDR10, and
// the Dolby Vision kinds whose base layer is HDR10 or HLG) is 10 bits in the profile the master playlist's CODECS names
// (HEVC Main 10, AV1 profile 0 at 10 bits) with the copy's own primaries, transfer, matrix and range for the init segment
// alone and for the frames of every fallback segment as it is answered (its init in front); anything else (SDR at 8 or 10
// bits, a Dolby Vision 8.2) is the 8-bit picture it was, with the ffmpeg arguments it had. (A Dolby Vision kind the build
// leaves out must be named in docs/transcoding.md with what the viewer gets instead, and then this check and the docs
// check are for the owner to settle: see the unchecked list of the task.)
func TestTheFallbackOfACopyIsTheCopysOwnPictureAtTenBitsForHDRAndAsItWasOtherwise(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, c := range hdrCases {
		t.Run(c.name, func(t *testing.T) {
			name := c.make(t)
			input := filepath.Join(film(t), name)
			own := probePicture(t, input)
			if c.unusual != "" && c.unusual != "primaries="+own.Primaries && c.unusual != "range="+own.Range {
				t.Fatalf("the film was made with %s but is tagged %s: the check would not tell the copy's tags from derived ones", c.unusual, own)
			}
			container := strings.TrimPrefix(filepath.Ext(name), ".")
			order := make([]int, scanned(t, input, container).Layout(true).Count())
			for n := range order {
				order[n] = n
			}
			dir, layout, commands := hdrRun(t, name, order)
			init := probePicture(t, filepath.Join(dir, "init.mp4"))
			if c.tenBit {
				if init.PixFmt != "yuv420p10le" {
					t.Errorf("the fallback's init is %s, want yuv420p10le (10 bits)", init)
				}
				if strings.Contains(string(readFile(t, filepath.Join(dir, "init.mp4"))), "hev1") {
					t.Error("the init segment's sample entry is hev1, not the hvc1 the master playlist names")
				}
				if want := map[string]string{"hevc": "Main 10", "av1": "Main"}[c.codec]; init.Profile != want {
					t.Errorf("the fallback's profile is %q, want %q (what the master playlist's CODECS names)", init.Profile, want)
				}
				if init.Primaries != own.Primaries || init.Transfer != own.Transfer || init.Matrix != own.Matrix || init.Range != own.Range {
					t.Errorf("the init segment's tags are %s, the copy's own are %s", init, own)
				}
			} else {
				// ffprobe reads an H.264 init's pixel format from no frame, so the frames below say it for that codec.
				if init.PixFmt != "yuv420p" && !(init.PixFmt == "" && c.codec == "h264") {
					t.Errorf("the fallback of an SDR copy is %s, want 8 bits (yuv420p) as it was", init)
				}
				if init.Transfer != own.Transfer {
					t.Errorf("the SDR fallback's transfer is %q, the copy's %q", init.Transfer, own.Transfer)
				}
			}
			for n := range layout.Count() {
				frames := framePictures(t, answered(t, dir, n))
				if len(frames) != 1 {
					t.Errorf("segment %d, with its init in front, has frames of %d kinds: %v", n, len(frames), frames)
				}
				for p := range frames {
					if !c.tenBit && p.PixFmt != "yuv420p" {
						t.Errorf("segment %d's frames are %s, want 8 bits (yuv420p)", n, p)
					}
					if (init.PixFmt != "" && p.PixFmt != init.PixFmt) || p.Primaries != init.Primaries || p.Transfer != init.Transfer || p.Matrix != init.Matrix || p.Range != init.Range {
						t.Errorf("segment %d's frames are %s, its init's %s", n, p, init)
					}
				}
			}
			// The command: an HDR picture is not tone mapped and has no 8-bit format on the way; an SDR one has today's.
			if len(commands) == 0 {
				t.Fatal("no ffmpeg encoded the fallback")
			}
			for _, line := range commands {
				if c.tenBit && (regexp.MustCompile(`tonemap|zscale`).MatchString(line) || eightBit.MatchString(line)) {
					t.Errorf("the HDR fallback's command has a tone-mapping filter or an 8-bit format: %s", line)
				}
				if !c.tenBit {
					if vf := option(strings.Fields(line), "-vf"); !strings.HasSuffix(vf, ",format=yuv420p") || strings.Contains(vf, "tonemap") || strings.Contains(line, "yuv420p10") {
						t.Errorf("the SDR fallback's picture is %q, want the 8-bit one it had", vf)
					}
				}
			}
		})
	}
}

// The HDR fallback is the copy's own picture: its decoded frames are nearer the source's frames at the same times than the
// frames of the tone-mapped fallback today's build makes of the same film (JS-088, JS-133), both read as 10-bit code
// values; and its ffmpeg command has no tone-mapping filter and no 8-bit pixel format on the way (HEVC and AV1).
func TestAnHDRFallbackIsNearerTheSourceThanTheToneMappedFallbackOfToday(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, c := range []struct{ codec, encode string }{
		{"hevc", "-c:v libx265 -preset veryfast -crf 26 -x265-params log-level=error"},
		{"av1", "-c:v libsvtav1 -preset 10 -crf 32"},
	} {
		t.Run(c.codec, func(t *testing.T) {
			name := filmOf{name: "nearer-" + c.codec, codec: c.codec, transfer: "smpte2084", tenBit: true, keyframes: hdrKeyframes}.make(t)
			input := filepath.Join(film(t), name)
			source := scanned(t, input, "mkv")
			order := make([]int, source.Layout(true).Count())
			for n := range order {
				order[n] = n
			}
			dir, layout, commands := hdrRun(t, name, order)
			fallback := join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, 0, layout.Count())...)
			// Today's: the picture tone mapped to SDR BT.709 at 8 bits, with the same encoder.
			today := filepath.Join(t.TempDir(), "today.mp4")
			args := append([]string{"-nostdin", "-v", "error", "-y", "-i", input, "-map", "0:v:0", "-vf", "scale=640:360:flags=bicubic," + playback.ToneMapFilter(playback.PQ) + ",format=yuv420p"}, strings.Fields(c.encode)...)
			run(t, "ffmpeg", append(args, today)...)
			psnr := func(file string) float64 {
				out := run(t, "ffmpeg", "-nostdin", "-i", input, "-i", file, "-lavfi",
					"[0:v]setpts=PTS-STARTPTS,format=yuv420p10le[a];[1:v]setpts=PTS-STARTPTS,format=yuv420p10le[b];[a][b]psnr", "-f", "null", "-")
				m := regexp.MustCompile(`PSNR y:\S+ u:\S+ v:\S+ average:(\S+)`).FindStringSubmatch(out)
				if m == nil {
					t.Fatalf("no PSNR in:\n%s", out)
				}
				v, _ := strconv.ParseFloat(m[1], 64)
				return v
			}
			own, mapped := psnr(fallback), psnr(today)
			t.Logf("%s: PSNR of the fallback against the source %.1f dB, of today's tone-mapped fallback %.1f dB", c.codec, own, mapped)
			if own < mapped+3 {
				t.Errorf("the fallback is not nearer the source's picture: %.1f dB against the tone-mapped fallback's %.1f dB (want at least 3 dB nearer)", own, mapped)
			}
			if len(commands) == 0 {
				t.Fatal("no ffmpeg encoded the fallback")
			}
			for _, line := range commands {
				if regexp.MustCompile(`tonemap|zscale|gbrpf32le`).MatchString(line) || eightBit.MatchString(line) {
					t.Errorf("the command has a tone-mapping filter or an 8-bit pixel format: %s", line)
				}
			}
		})
	}
}

// A fallback run of an HDR10 HEVC and of an HDR10 AV1 copy begun at segment 1, at a middle segment and at the last, as a
// restart begins, splices with the earlier segments of a run from the start: a byte-identical init segment (so the same
// bit depth and tags), the same number of frames, frame times within 1 ms, audio packet times within 0.5 ms and no
// decoder warning across the join that the run from the start lacks. Under test/ffmpeg61.sh and on ffmpeg 9. SVT-AV1 in
// ffmpeg 6.1 ignores -force_key_frames, which the shim makes true on any ffmpeg.
func TestAnHDRFallbackRestartSplicesWithEarlierSegments(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, codec := range []string{"hevc", "av1"} {
		t.Run(codec, func(t *testing.T) {
			name := filmOf{name: "restart-" + codec, codec: codec, transfer: "smpte2084", tenBit: true, keyframes: av1Keyframes, rate: "24000/1001"}.make(t)
			count := scanned(t, filepath.Join(film(t), name), "mkv").Layout(true).Count()
			from := func(start int) (string, playback.Layout) {
				order := make([]int, 0, count-start)
				for n := start; n < count; n++ {
					order = append(order, n)
				}
				dir, layout, _ := hdrRun(t, name, order)
				return dir, layout
			}
			whole, layout := from(0)
			init := filepath.Join(whole, "init.mp4")
			if p := probePicture(t, init); p.PixFmt != "yuv420p10le" || p.Transfer != "smpte2084" {
				t.Errorf("the run from the start is %s, want 10 bits PQ", p)
			}
			full := join(t, init, segmentFiles(whole, 0, count)...)
			last, _ := layout.Segment(count - 1)
			warnings := func(file string) string {
				out := run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", file, "-f", "null", "-")
				return regexp.MustCompile(` @ 0x[0-9a-f]+`).ReplaceAllString(out, "")
			}
			baseline := warnings(full)
			endOfAudio := tsOffset + float64(last)/ticksPerSecond - 1 // The last segment's padded AAC frame is no seam.
			for _, start := range []int{1, count / 2, count - 1} {
				resumed, _ := from(start)
				if a, b := readFile(t, init), readFile(t, filepath.Join(resumed, "init.mp4")); !bytes.Equal(a, b) {
					t.Errorf("restart %d: init segments differ", start)
				}
				spliced := join(t, init, append(segmentFiles(whole, 0, start), segmentFiles(resumed, start, count)...)...)
				compare(t, fmt.Sprintf("restart %d video times (and their number: no frame skipped or played twice)", start),
					times(t, full, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
				compare(t, fmt.Sprintf("restart %d audio packet times", start),
					before(times(t, full, "a:0", "packet=pts_time"), endOfAudio), before(times(t, spliced, "a:0", "packet=pts_time"), endOfAudio), 0.0005)
				if got := warnings(spliced); got != baseline {
					t.Errorf("restart %d: decoder warnings differ from a run from the start:\n%s\nagainst\n%s", start, got, baseline)
				}
				if frames := framePictures(t, spliced); len(frames) != 1 {
					t.Errorf("restart %d: the spliced film has frames of %d kinds: %v", start, len(frames), frames)
				}
			}
		})
	}
}

// A worker killed in the middle of an HDR copy's fallback is replaced by another, which makes the rest from the first
// missing segment, and the request that waited is answered with the segment, not an error (invariant 22). The segments
// of the two workers join as a run from the start's do. Three runs in a row on two CPUs under ffmpeg 6.1
// (test/ffmpeg61.sh with JELLYSCALE_TEST_CPUS=0-1, -count=3), for HEVC and for AV1.
func TestAKilledWorkerMakingAnHDRFallbackIsReplacedAndTheSegmentsJoin(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, codec := range []string{"hevc", "av1"} {
		t.Run(codec, func(t *testing.T) {
			name := filmOf{name: "failover-" + codec, codec: codec, transfer: "smpte2084", tenBit: true, keyframes: av1Keyframes, rate: "24000/1001"}.make(t)
			count := scanned(t, filepath.Join(film(t), name), "mkv").Layout(true).Count()
			all := make([]int, count)
			for n := range all {
				all[n] = n
			}
			reference, layout, _ := hdrRun(t, name, all) // Before this test's rig: a rig truncates the tables of the one before.
			segments := t.TempDir()
			r := newRig(t, store.NewPosix(segments))
			r.hdrFallbackRig(t, name)
			ffmpeg := testenv.FFmpeg(t) // Before the shim, which would slow the check of the result.
			slowAt(t, "2")
			// Slots enough for what the fallback costs, whatever the build prices it at: the check is of the splice, and the price is the
			// docs check's.
			a := r.workerProcess(t, "worker-a", segments, "JELLYSCALE_TEST_SLOTS=8")
			j := r.jobs(t)
			if _, err := r.fetch(t, j, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			if got := r.owner(t); got != "worker-a" {
				t.Fatalf("the job is worker-a's, not %q", got)
			}
			r.workerProcess(t, "worker-b", segments, "JELLYSCALE_TEST_SLOTS=8")
			owned := r.watchOwners(t)
			for n := 1; n < count; n++ {
				if n == 3 {
					a.Process.Kill() //nolint:errcheck // The viewer has segment 2 and waits for 3, which worker-a is making.
				}
				if _, err := r.fetch(t, j, psid(0), n); err != nil {
					t.Fatalf("segment %d: the request that waited was answered with an error: %v", n, err)
				}
			}
			var latest string
			r.pool.QueryRow(context.Background(), "SELECT COALESCE(worker, '') FROM transcode_jobs ORDER BY id DESC LIMIT 1").Scan(&latest) //nolint:errcheck
			if latest != "worker-b" {
				t.Errorf("the job is %q's, want worker-b's", latest)
			}
			owned.check(t)
			stored := t.TempDir()
			for n := range count {
				body, err := os.ReadFile(filepath.Join(segments, SegmentKey(r.hash, n)))
				if err != nil {
					t.Fatalf("segment %d was not stored: %v", n, err)
				}
				if err := os.WriteFile(filepath.Join(stored, fmt.Sprintf("%d.m4s", n)), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			init := readFile(t, filepath.Join(segments, SegmentKey(r.hash, -1)))
			if !bytes.Equal(init, readFile(t, filepath.Join(reference, "init.mp4"))) {
				t.Error("the init segment differs from an uninterrupted run's")
			}
			initFile := filepath.Join(stored, "init.mp4")
			if err := os.WriteFile(initFile, init, 0o644); err != nil {
				t.Fatal(err)
			}
			whole := join(t, filepath.Join(reference, "init.mp4"), segmentFiles(reference, 0, count)...)
			spliced := join(t, initFile, segmentFiles(stored, 0, count)...)
			compare(t, "video", times(t, whole, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
			last, _ := layout.Segment(count - 1)
			end := tsOffset + float64(last)/ticksPerSecond - 1
			compare(t, "audio", before(times(t, whole, "a:0", "packet=pts_time"), end), before(times(t, spliced, "a:0", "packet=pts_time"), end), 0.0005)
			if warnings := bytes.TrimSpace([]byte(run(t, ffmpeg, "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-"))); len(warnings) > 0 {
				t.Errorf("decoder warnings:\n%s", warnings)
			}
			if p := probePicture(t, initFile); p.PixFmt != "yuv420p10le" || p.Transfer != "smpte2084" {
				t.Errorf("the two workers' fallback is %s, want 10 bits PQ", p)
			}
		})
	}
}

// A worker of the release before (output version 6) never claims the fallback job of an HDR copy: its job records the
// OutputVersion of the replica that queued it, above 6 (invariant 59), so no 8-bit or tone-mapped segment is stored under
// the new fallback's hash.
func TestAnHDRFallbackJobIsQueuedAtAnOutputVersionTheReleaseBeforeDoesNotMake(t *testing.T) {
	name := filmOf{name: "version-hevc", codec: "hevc", transfer: "smpte2084", tenBit: true, keyframes: []int{0, 5, 11}}.make(t)
	r := newRig(t, nil)
	r.hdrFallbackRig(t, name)
	id, _, err := r.jobs(t).ensure(context.Background(), r.spec, r.hash, psid(0), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.versionOf(t, id); got <= 6 || got != playback.OutputVersion {
		t.Errorf("the HDR fallback's job is queued at output version %d, want the replica's %d, above the release before's 6", got, playback.OutputVersion)
	}
}

// The price of the fallback is measured with the commands the worker runs for it: the CPU time of the ffmpeg processes it
// ran (user plus system, from the test process's children) for each second of film, for a 1080p and a 4K 24 fps HDR10
// source, HEVC and AV1, logged beside what Cost says; docs/transcoding.md's rows must cover it
// (internal/playback's TestTheHDRFallbacksPriceCoversTheCPUItsFfmpegUses). It asserts only that the fallback runs on the
// sources the rows name, is 10 bits PQ, encodes with the copy's codec, and that a 4K copy's fallback is 1920x1080: what a
// CPU takes depends on the machine, so the figures are logged, to be copied into the docs by whoever measures them on
// ffmpeg 6.1 (test/ffmpeg61.sh) and on ffmpeg 9.
func TestTheHDRFallbacksCommandsAreMeasuredOnA1080pAndA4KSource(t *testing.T) {
	testenv.FFmpeg(t)
	const seconds = 6
	for _, codec := range []string{"hevc", "av1"} {
		for _, size := range []struct {
			name          string
			width, height int
		}{{"1080p", 1920, 1080}, {"4k", 3840, 2160}} {
			t.Run(codec+" "+size.name, func(t *testing.T) {
				name := filmOf{name: "cost-" + codec + "-" + size.name, codec: codec, transfer: "smpte2084", tenBit: true, keyframes: []int{0, 3, seconds}, width: size.width, height: size.height}.make(t)
				logged := logFFmpeg(t)
				segments := t.TempDir()
				r := newRig(t, store.NewPosix(segments))
				r.hdrFallbackRig(t, name)
				stop := r.worker(t, nil)
				j := r.jobs(t)
				var before, after syscall.Rusage
				syscall.Getrusage(syscall.RUSAGE_CHILDREN, &before) //nolint:errcheck
				began := time.Now()
				layout := scanned(t, filepath.Join(film(t), name), "mkv").Layout(true)
				for n := range layout.Count() {
					if _, err := r.fetch(t, j, psid(0), n); err != nil {
						t.Fatalf("segment %d: %v", n, err)
					}
				}
				wall := time.Since(began)
				stop()
				syscall.Getrusage(syscall.RUSAGE_CHILDREN, &after) //nolint:errcheck
				cpu := time.Duration(after.Utime.Nano()-before.Utime.Nano()) + time.Duration(after.Stime.Nano()-before.Stime.Nano())
				t.Logf("HDR fallback row: %s %s cpu %.2f s/s wall %.2f s/s (the price is %.2f cores; %d CPUs, %s)",
					codec, size.name, cpu.Seconds()/seconds, wall.Seconds()/seconds, r.spec.Cost(), numCPU(), ffmpegVersion(t))
				if len(encodes(logged())) == 0 || !strings.Contains(strings.Join(encodes(logged()), "\n"), map[string]string{"hevc": "libx265", "av1": "libsvtav1"}[codec]) {
					t.Errorf("no ffmpeg of the fallback encoded with the copy's codec: %v", logged())
				}
				if _, err := r.fetchAt(t, j, psid(0), -1, 0); err != nil {
					t.Fatalf("the fallback's init segment: %v", err)
				}
				initFile := filepath.Join(segments, SegmentKey(r.hash, -1))
				var dims struct {
					Streams []struct{ Width, Height int } `json:"streams"`
				}
				if err := json.Unmarshal([]byte(run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "json", initFile)), &dims); err != nil || len(dims.Streams) != 1 {
					t.Fatalf("ffprobe of the init: %v", err)
				}
				if got := dims.Streams[0]; got.Width != 1920 || got.Height != 1080 {
					t.Errorf("the fallback of the %s copy is %dx%d, want 1920x1080", size.name, got.Width, got.Height)
				}
				if p := probePicture(t, initFile); p.PixFmt != "yuv420p10le" || p.Transfer != "smpte2084" {
					t.Errorf("the fallback of the %s copy is %s, want 10 bits PQ", size.name, p)
				}
				if r.spec.Cost() <= 0 || math.IsNaN(r.spec.Cost()) {
					t.Errorf("the price is %v", r.spec.Cost())
				}
			})
		}
	}
}
