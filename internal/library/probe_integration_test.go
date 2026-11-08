//go:build integration

package library

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"jellyscale/internal/testenv"
)

// A file whose audio outlasts its video must report where the video ends, in
// both containers: mp4 says so per stream, Matroska only in a tag.
func TestProbeReportsVideoDuration(t *testing.T) {
	testenv.FFmpeg(t)
	for _, name := range []string{"long-audio.mp4", "long-audio.mkv"} {
		file := filepath.Join(t.TempDir(), name)
		out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=5",
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", file).CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		var video int64
		for _, s := range probe.Streams {
			if s["Type"] == "Video" {
				video, _ = s["DurationTicks"].(int64)
			}
		}
		if video < 19_000_000 || video > 21_000_000 {
			t.Errorf("%s: video DurationTicks %d, want about 20000000", name, video)
		}
		if probe.RunTimeTicks < 45_000_000 {
			t.Errorf("%s: RunTimeTicks %d should stay the format's", name, probe.RunTimeTicks)
		}
	}
}

// The H.264 profile a browser sends rules out level 5.3, interlaced and HDR
// files, so the probe has to record what shows it.
func TestProbeRecordsLevelFieldOrderAndRange(t *testing.T) {
	testenv.FFmpeg(t)
	cases := []struct {
		name      string
		args      []string
		level     int
		interlace bool
		rangeType string
		transfer  string
	}{
		{"plain", nil, 10, false, "SDR", ""},
		{"level", []string{"-level", "5.2"}, 52, false, "SDR", ""},
		{"interlaced", []string{"-vf", "interlace=scan=tff", "-flags", "+ilme+ildct"}, -1, true, "SDR", ""},
		{"hdr", []string{"-x264-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc"}, 10, false, "HDR10", "smpte2084"},
		{"hlg", []string{"-x264-params", "colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc"}, 10, false, "HLG", "arib-std-b67"},
	}
	for _, c := range cases {
		file := filepath.Join(t.TempDir(), "v.mkv")
		args := append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x92:rate=10:duration=1",
			"-c:v", "libx264", "-preset", "ultrafast"}, c.args...)
		if out, err := exec.Command("ffmpeg", append(args, file)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", c.name, err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		video := probe.Streams[0]
		// Always recorded, even empty, so a scan can tell a film that was never asked.
		if transfer, ok := video["ColorTransfer"]; !ok || transfer != c.transfer {
			t.Errorf("%s: ColorTransfer %v (set %v), want %q", c.name, transfer, ok, c.transfer)
		}
		if (video["Level"] != c.level && c.level >= 0) || video["IsInterlaced"] != c.interlace || video["VideoRangeType"] != c.rangeType {
			t.Errorf("%s: level %v interlaced %v range %v, want %d %v %s", c.name, video["Level"], video["IsInterlaced"], video["VideoRangeType"], c.level, c.interlace, c.rangeType)
		}
	}
}

// The bit depth comes from the pixel format, which is all ffprobe says of it
// for most HEVC and AV1: the master playlist of an AV1 copy names it.
func TestProbeRecordsBitDepth(t *testing.T) {
	testenv.FFmpeg(t)
	for _, c := range []struct {
		pixFmt string
		depth  int
	}{{"yuv420p", 8}, {"yuv420p10le", 10}} {
		file := filepath.Join(t.TempDir(), "v.mkv")
		if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x92:rate=10:duration=1",
			"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", c.pixFmt, file).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", c.pixFmt, err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		if got := probe.Streams[0]["BitDepth"]; got != c.depth {
			t.Errorf("%s: bit depth %v, want %d", c.pixFmt, got, c.depth)
		}
		if !probe.wantsKeyframes() {
			t.Errorf("%s: an HEVC file in Matroska is not asked for an index", c.pixFmt)
		}
	}
}

// PGS, VobSub and DVB tracks are recorded as image tracks under the names
// Jellyfin lists them by, and only PGS can be sent to the client as a file.
func TestProbeRecordsImageSubtitles(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	sup := filepath.Join(dir, "track.sup")
	if err := os.WriteFile(sup, testenv.PGS(1920, 1080, testenv.Cue{Start: 0.2, End: 0.8, X: 10, Y: 10, W: 100, H: 40}), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
		"-fix_sub_duration", "-i", sup, "-map", "0:v", "-map", "1", "-map", "1", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:s:0", "copy", "-c:s:1", "dvdsub", "-c:s:2", "dvbsub", "-metadata:s:s:0", "language=eng", "-disposition:s:0", "default+forced", file).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	probe, err := ProbeInput(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range map[int]map[string]any{
		1: {"Codec": "PGSSUB", "DisplayTitle": "eng - Default - Forced - PGSSUB", "Language": "eng", "Width": 1920, "Height": 1080, "SupportsExternalStream": true},
		2: {"Codec": "DVDSUB", "DisplayTitle": "DVDSUB", "Width": 1920, "Height": 1080, "SupportsExternalStream": false},
		3: {"Codec": "DVBSUB", "DisplayTitle": "DVBSUB", "SupportsExternalStream": false},
	} {
		got := probe.Streams[index]
		want["Index"], want["Type"], want["IsTextSubtitleStream"], want["IsImageSubtitleStream"] = index, "Subtitle", false, true
		for key, value := range want {
			if got[key] != value {
				t.Errorf("stream %d: %s is %v, want %v (%v)", index, key, got[key], value, got)
			}
		}
		if _, has := got["Width"]; index == 3 && has {
			t.Errorf("stream 3: DVB says no size, and Width is %v", got["Width"])
		}
	}
}

// A Matroska file has no bit rate field for a track, but muxers write one as a
// BPS tag; the probe reads it, since a copied audio track is counted against
// the bitrate cap by it. A track with neither stays without a bitrate.
func TestProbeRecordsAudioBitrate(t *testing.T) {
	testenv.FFmpeg(t)
	cases := []struct {
		name string
		args []string
		want int64
	}{
		{"mp4", []string{"-c:a", "aac", "-b:a", "128k", "v.mp4"}, 128_000},
		{"mkv with a BPS tag", []string{"-c:a", "aac", "-metadata:s:a:0", "BPS=123456", "v.mkv"}, 123456},
		{"mkv with a BPS tag for the language", []string{"-c:a", "aac", "-metadata:s:a:0", "language=eng", "-metadata:s:a:0", "BPS-eng=234567", "v.mkv"}, 234567},
		{"mkv with neither", []string{"-c:a", "flac", "-fflags", "+bitexact", "v.mkv"}, 0},
	}
	for _, c := range cases {
		dir := t.TempDir()
		args := append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "libx264", "-preset", "ultrafast"}, c.args...)
		args[len(args)-1] = filepath.Join(dir, args[len(args)-1])
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", c.name, err, out)
		}
		probe, err := ProbeInput(context.Background(), args[len(args)-1])
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		for _, s := range probe.Streams {
			if s["Type"] == "Audio" {
				got, _ = s["BitRate"].(int64)
			}
		}
		// mp4 reports what the encoder wrote, close to the asked-for rate.
		if got < c.want*95/100 || got > c.want*105/100 {
			t.Errorf("%s: BitRate %d, want %d", c.name, got, c.want)
		}
	}
}

// chapterMetadata is an ffmetadata file with three chapters, the last untitled.
const chapterMetadata = `;FFMETADATA1
[CHAPTER]
TIMEBASE=1/1000
START=0
END=20000
title=Opening
[CHAPTER]
TIMEBASE=1/1000
START=20000
END=45500
title=Middle Part
[CHAPTER]
TIMEBASE=1/1000
START=45500
END=60000
`

func chapteredFilm(t *testing.T, file string, extra ...string) {
	t.Helper()
	meta := filepath.Join(t.TempDir(), "chapters.txt")
	if err := os.WriteFile(meta, []byte(chapterMetadata), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-nostdin", "-v", "error", "-y"}, extra...)
	args = append(args, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=60", "-i", meta,
		"-map", "0:v", "-map_metadata", "1", "-map_chapters", "1", "-c:v", "libx264", "-preset", "ultrafast", file)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// The probe that lists the streams reads the chapters, in Matroska and MP4,
// with the names and positions Jellyfin 12.4 gives (compat/captures/chapters-12.4.md),
// on the file's clock: a film whose streams start 2 s late has its chapters
// where the file says, not moved by that.
func TestProbeReadsChapters(t *testing.T) {
	testenv.FFmpeg(t)
	for _, c := range []struct {
		name  string
		extra []string
	}{{"v.mkv", nil}, {"v.mp4", nil}, {"late.mkv", []string{"-itsoffset", "2"}}, {"late.mp4", []string{"-itsoffset", "2"}}} {
		file := filepath.Join(t.TempDir(), c.name)
		chapteredFilm(t, file, c.extra...)
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		want := []struct {
			name  string
			ticks int64
		}{{"Opening", 0}, {"Middle Part", 200_000_000}, {"Chapter 3", 455_000_000}}
		if len(probe.Chapters) != len(want) {
			t.Fatalf("%s: %d chapters: %v", c.name, len(probe.Chapters), probe.Chapters)
		}
		for i, w := range want {
			got := probe.Chapters[i]
			if got["Name"] != w.name || got["StartPositionTicks"] != w.ticks {
				t.Errorf("%s: chapter %d is %v, want %s at %d", c.name, i, got, w.name, w.ticks)
			}
			// jellyfin-web asks for a chapter's image only when it has a tag.
			if _, ok := got["ImageTag"]; ok {
				t.Errorf("%s: chapter %d has an ImageTag", c.name, i)
			}
		}
	}

	// A file with none has an empty list, not nil: that is how a scan knows it looked.
	file := filepath.Join(t.TempDir(), "plain.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	probe, err := ProbeInput(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Chapters == nil || len(probe.Chapters) != 0 {
		t.Errorf("a file without chapters: %#v", probe.Chapters)
	}
}

// QuickTime keeps chapters in a text track that the video's track points to
// (tref/chap). ffmpeg reads the chapters and lists the track as data, so it
// must not appear as a subtitle track a viewer could pick.
func TestProbeReadsQuickTimeChapterTrack(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "chapters.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:20,000\nOpening\n\n2\n00:00:20,000 --> 00:01:00,000\nMiddle Part\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.mp4")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=60",
		"-i", srt, "-map", "0:v", "-map", "1:s", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "mov_text", plain).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	data, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "quicktime.mp4")
	if err := os.WriteFile(file, pointVideoAtTextTrack(t, data), 0o644); err != nil {
		t.Fatal(err)
	}
	probe, err := ProbeInput(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.Chapters) != 2 || probe.Chapters[0]["Name"] != "Opening" || probe.Chapters[1]["StartPositionTicks"] != int64(200_000_000) {
		t.Errorf("chapters %v", probe.Chapters)
	}
	for _, stream := range probe.Streams {
		if stream["Type"] == "Subtitle" {
			t.Errorf("the chapter track is listed as a subtitle track: %v", stream)
		}
	}
}

// pointVideoAtTextTrack adds the video track's tref/chap box naming the text
// track, as QuickTime writes it. ffmpeg's mp4 muxer puts the moov last, so no
// chunk offset moves.
func pointVideoAtTextTrack(t *testing.T, file []byte) []byte {
	t.Helper()
	type box struct {
		at, size int
		kind     string
	}
	children := func(from, to int) (list []box) {
		for at := from; at < to; {
			size := int(binary.BigEndian.Uint32(file[at:]))
			list = append(list, box{at, size, string(file[at+4 : at+8])})
			at += size
		}
		return list
	}
	var moov box
	for _, b := range children(0, len(file)) {
		if b.kind == "moov" {
			moov = b
		}
	}
	var video box
	var textID uint32
	for _, trak := range children(moov.at+8, moov.at+moov.size) {
		if trak.kind != "trak" {
			continue
		}
		var id uint32
		var handler string
		for _, b := range children(trak.at+8, trak.at+trak.size) {
			switch b.kind {
			case "tkhd": // Version 0: the track ID follows 8 bytes of header, version, flags and two dates.
				id = binary.BigEndian.Uint32(file[b.at+20:])
			case "mdia":
				for _, m := range children(b.at+8, b.at+b.size) {
					if m.kind == "hdlr" {
						handler = string(file[m.at+16 : m.at+20])
					}
				}
			}
		}
		switch handler {
		case "vide":
			video = trak
		case "sbtl", "text":
			textID = id
		}
	}
	if video.size == 0 || textID == 0 {
		t.Fatal("the fixture has no video track and text track")
	}
	tref := binary.BigEndian.AppendUint32(nil, 20)
	tref = append(tref, "tref"...)
	tref = binary.BigEndian.AppendUint32(tref, 12)
	tref = append(tref, "chap"...)
	tref = binary.BigEndian.AppendUint32(tref, textID)
	for _, b := range []box{video, moov} {
		binary.BigEndian.PutUint32(file[b.at:], uint32(b.size+len(tref)))
	}
	end := video.at + video.size
	return slices.Concat(file[:end], tref, file[end:])
}

// dolbyVisionFilms are the films of compat/captures/dolby-vision-12.4.json, by
// the name they have there (test/fixtures/e2e.sh makes the same ones).
var dolbyVisionFilms = map[string]testenv.HDRFilm{
	"Plain Sdr Film (1990)":            {Base: "sdr"},
	"Plain Pq Film (1991)":             {Base: "pq"},
	"Plain Hlg Film (1992)":            {Base: "hlg"},
	"Plain Plus Film (1993)":           {Base: "pq", HDR10Plus: true},
	"Dolby Five Film (1994)":           {Base: "pq", DVProfile: 5},
	"Dolby Seven Film (1995)":          {Base: "pq", DVProfile: 7, DVCompat: 6, DVEnhancement: true},
	"Dolby Eight One Film (1996)":      {Base: "pq", DVProfile: 8, DVCompat: 1},
	"Dolby Eight Two Film (1997)":      {Base: "sdr", DVProfile: 8, DVCompat: 2},
	"Dolby Eight Four Film (1998)":     {Base: "hlg", DVProfile: 8, DVCompat: 4},
	"Dolby Eight Zero Film (1999)":     {Base: "pq", DVProfile: 8},
	"Dolby Eight Three Film (2000)":    {Base: "pq", DVProfile: 8, DVCompat: 3},
	"Dolby Ten Film (2001)":            {Base: "av1", DVProfile: 10, DVCompat: 1},
	"Dolby Eight One Plus Film (2002)": {Base: "pq", HDR10Plus: true, DVProfile: 8, DVCompat: 1},
	"Dolby Seven Plus Film (2003)":     {Base: "pq", HDR10Plus: true, DVProfile: 7, DVCompat: 6, DVEnhancement: true},
	"Dolby Five Plus Film (2004)":      {Base: "pq", HDR10Plus: true, DVProfile: 5},
}

// dolbyVisionKeys are the fields of Jellyfin's video stream that say what kind
// of HDR a film is. A key Jellyfin leaves out is left out here too.
var dolbyVisionKeys = []string{"VideoRange", "VideoRangeType", "VideoDoViTitle", "DisplayTitle", "DvVersionMajor", "DvVersionMinor",
	"DvProfile", "DvLevel", "RpuPresentFlag", "ElPresentFlag", "BlPresentFlag", "DvBlSignalCompatibilityId", "Hdr10PlusPresentFlag"}

// The probe labels a film as Jellyfin 12.4 does: the range and its type for
// SDR, HDR10, HDR10+, HLG and each Dolby Vision profile with the base layer it
// has, the Dolby Vision fields, the HDR10+ flag, and the display title that
// carries them.
func TestProbeLabelsHDRAndDolbyVisionAsJellyfinDoes(t *testing.T) {
	testenv.FFmpeg(t)
	raw, err := os.ReadFile("../../compat/captures/dolby-vision-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Files map[string]struct{ Video map[string]any }
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	for name, film := range dolbyVisionFilms {
		t.Run(name, func(t *testing.T) {
			film.Seconds = 1
			file := filepath.Join(t.TempDir(), "film.mp4")
			if err := testenv.MakeHDRFilm(file, film); err != nil {
				t.Fatal(err)
			}
			probe, err := ProbeInput(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(probe.Streams[0])
			var got map[string]any
			_ = json.Unmarshal(encoded, &got)
			want := capture.Files[name].Video
			for _, key := range dolbyVisionKeys {
				if !reflect.DeepEqual(got[key], want[key]) {
					t.Errorf("%s is %v, Jellyfin %v", key, got[key], want[key])
				}
			}
			// Jellyfin gives the audio stream the same two keys, as "Unknown".
			encoded, _ = json.Marshal(probe.Streams[1])
			var audio map[string]any
			_ = json.Unmarshal(encoded, &audio)
			for _, key := range []string{"VideoRange", "VideoRangeType"} {
				if audio[key] != "Unknown" {
					t.Errorf("the audio stream's %s is %v, Jellyfin %q", key, audio[key], "Unknown")
				}
			}
		})
	}
}
