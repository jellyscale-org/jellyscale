package library

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"jellyscale/internal/playback"
)

const ticksPerSecond = 10_000_000

// maxDuration is a century in seconds, 3e16 ticks, far below overflow.
const maxDuration = 100 * 365 * 86400

// textSubtitleCodecs are the ffprobe names of the subtitle codecs that hold
// text.
var textSubtitleCodecs = []string{"subrip", "ass", "ssa", "webvtt", "mov_text"}

// imageSubtitleCodecs are the ffprobe names of the image codecs that can be
// drawn into a transcode (PGS, VobSub, DVB), with the names Jellyfin lists them
// under. Any other bitmap codec stays out of the listing: nothing delivers it.
var imageSubtitleCodecs = map[string]string{"hdmv_pgs_subtitle": "PGSSUB", "dvd_subtitle": "DVDSUB", "dvb_subtitle": "DVBSUB"}

// Probe is what a media file says about itself.
type Probe struct {
	Container    string
	RunTimeTicks int64
	Bitrate      int64
	Width        int
	Height       int
	Streams      []map[string]any
	// Keyframes are the video's keyframe times, from ReadKeyframes. Nil: not
	// looked for, or it failed and a later scan tries again. Empty: the file
	// has no index to read.
	Keyframes []int64
	// VideoEnd is where the last video frame ends, from ReadVideoEnd, on the
	// same clock as Keyframes. Set with them.
	VideoEnd int64
}

// wantsKeyframes: only the codecs playback copies need an index, and only
// these containers have one ReadKeyframes reads.
func (p Probe) wantsKeyframes() bool {
	if p.Container != "mkv" && p.Container != "mp4" {
		return false
	}
	for _, stream := range p.Streams {
		if stream["Type"] == "Video" {
			codec, _ := stream["Codec"].(string)
			return playback.CopyCodec(codec)
		}
	}
	return false
}

type sideData struct {
	Type string `json:"side_data_type"`
}

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index       int               `json:"index"`
		CodecType   string            `json:"codec_type"`
		CodecName   string            `json:"codec_name"`
		Profile     string            `json:"profile"`
		Level       int               `json:"level"`
		PixFmt      string            `json:"pix_fmt"`
		FieldOrder  string            `json:"field_order"`
		Transfer    string            `json:"color_transfer"`
		SideData    []sideData        `json:"side_data_list"`
		Width       int               `json:"width"`
		Height      int               `json:"height"`
		Channels    int               `json:"channels"`
		SampleRate  string            `json:"sample_rate"`
		BitRate     string            `json:"bit_rate"`
		Duration    string            `json:"duration"`
		StartTime   string            `json:"start_time"`
		AvgFrame    string            `json:"avg_frame_rate"`
		Tags        map[string]string `json:"tags"`
		Disposition struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
	} `json:"streams"`
}

// ProbeInput runs ffprobe on a path or URL.
func ProbeInput(ctx context.Context, input string) (Probe, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_format", "-show_streams",
		"-of", "json", "--", input)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	var parsed ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return Probe{}, fmt.Errorf("parse ffprobe output: %w", err)
	}

	duration, _ := strconv.ParseFloat(parsed.Format.Duration, 64)
	// Garbage or absurd durations (NaN, Inf, 1e300) must not wrap when
	// converted, or later arithmetic on the runtime would overflow.
	if !(duration > 0) {
		duration = 0
	}
	duration = min(duration, maxDuration)
	bitrate, _ := strconv.ParseInt(parsed.Format.BitRate, 10, 64)
	probe := Probe{
		Container:    containerName(parsed.Format.FormatName),
		RunTimeTicks: int64(duration * ticksPerSecond),
		Bitrate:      bitrate,
		Streams:      []map[string]any{},
	}
	for _, stream := range parsed.Streams {
		if stream.CodecType == "attachment" {
			// Fonts, mostly. The scan lists them; a worker takes them out when
			// somebody asks (subtitles.FillFonts).
			probe.Streams = append(probe.Streams, map[string]any{
				"Index": stream.Index, "Type": "Attachment", "FileName": stream.Tags["filename"], "MimeType": stream.Tags["mimetype"],
			})
			continue
		}
		kind, ok := map[string]string{"video": "Video", "audio": "Audio", "subtitle": "Subtitle"}[stream.CodecType]
		if !ok {
			continue
		}
		entry := map[string]any{
			"Index":                  stream.Index,
			"Type":                   kind,
			"Codec":                  stream.CodecName,
			"IsDefault":              stream.Disposition.Default == 1,
			"IsForced":               stream.Disposition.Forced == 1,
			"IsExternal":             false,
			"SupportsExternalStream": false,
		}
		if stream.Profile != "" {
			entry["Profile"] = stream.Profile
		}
		if language := stream.Tags["language"]; language != "" && language != "und" {
			entry["Language"] = language
		}
		// Matroska has no bit rate field; mkvmerge and MakeMKV write it as a BPS tag
		// ("BPS-eng" when the track has a language), which ffprobe leaves in the
		// tags. A copied audio track is counted against the bitrate cap by it.
		bps := cmp.Or(stream.BitRate, stream.Tags["BPS"], stream.Tags["BPS-"+stream.Tags["language"]])
		if rate, err := strconv.ParseInt(bps, 10, 64); err == nil && rate > 0 {
			entry["BitRate"] = rate
		}
		switch kind {
		case "Video":
			entry["Width"], entry["Height"] = stream.Width, stream.Height
			// What a client's CodecProfile can rule out. Level is ffprobe's
			// number, 52 for H.264 level 5.2, as profiles write it.
			entry["Level"] = max(stream.Level, 0)
			entry["IsInterlaced"] = slices.Contains([]string{"tt", "bb", "tb", "bt"}, stream.FieldOrder)
			entry["VideoRangeType"] = rangeType(stream.Transfer, stream.SideData)
			entry["BitDepth"] = bitDepth(stream.PixFmt)
			// The video can end before the audio does, and an HLS playlist
			// has to stop where the video does. Matroska keeps the duration
			// in a tag, as the end on the file's clock, so a video that
			// starts late has that much less than the tag says.
			ticks := streamTicks(stream.Duration, stream.Tags["DURATION"])
			if _, err := strconv.ParseFloat(stream.Duration, 64); err != nil && probe.Container == "mkv" {
				start, _ := strconv.ParseFloat(stream.StartTime, 64)
				if ticks == 0 {
					ticks = int64(duration * ticksPerSecond)
				}
				ticks -= int64(max(start, 0) * ticksPerSecond)
			}
			if ticks > 0 {
				entry["DurationTicks"] = ticks
			}
			entry["DisplayTitle"] = fmt.Sprintf("%dp %s", stream.Height, strings.ToUpper(stream.CodecName))
			if fps := frameRate(stream.AvgFrame); fps > 0 {
				entry["AverageFrameRate"] = fps
			}
			if probe.Width == 0 {
				probe.Width, probe.Height = stream.Width, stream.Height
			}
		case "Audio":
			entry["Channels"] = stream.Channels
			entry["DisplayTitle"] = displayAudio(stream.Tags["language"], stream.CodecName, stream.Channels)
			if rate, err := strconv.Atoi(stream.SampleRate); err == nil {
				entry["SampleRate"] = rate
			}
		case "Subtitle":
			text := slices.Contains(textSubtitleCodecs, stream.CodecName)
			name, image := imageSubtitleCodecs[stream.CodecName]
			if image {
				entry["Codec"] = name
				// The canvas the bitmaps are placed on, which burning needs.
				if stream.Width > 0 && stream.Height > 0 {
					entry["Width"], entry["Height"] = stream.Width, stream.Height
				}
			}
			entry["DisplayTitle"] = displaySubtitle(stream.Tags["language"], cmp.Or(name, stream.CodecName), stream.Disposition.Default == 1, stream.Disposition.Forced == 1)
			// Text tracks are extracted and PGS ones copied out for the client;
			// VobSub and DVB are only drawn into a transcode, and every other
			// image codec stays in media_sources.streams alone.
			entry["IsTextSubtitleStream"], entry["IsImageSubtitleStream"] = text, image
			entry["SupportsExternalStream"] = text || stream.CodecName == "hdmv_pgs_subtitle"
		}
		probe.Streams = append(probe.Streams, entry)
	}
	return probe, nil
}

// bitDepth reads the depth from ffprobe's pixel format name, "yuv420p10le"
// for 10 bits and "yuv420p" for 8; ffprobe leaves bits_per_raw_sample out of
// most HEVC and AV1 streams.
func bitDepth(pixFmt string) int {
	for _, depth := range []int{16, 14, 12, 10, 9} {
		if strings.Contains(pixFmt, strconv.Itoa(depth)+"le") || strings.Contains(pixFmt, strconv.Itoa(depth)+"be") {
			return depth
		}
	}
	return 8
}

// rangeType is Jellyfin's VideoRangeType. HDR10+ looks like HDR10 here, and
// either is simply not SDR to the profiles that matter.
func rangeType(transfer string, sideData []sideData) string {
	for _, d := range sideData {
		if strings.Contains(d.Type, "DOVI") || strings.Contains(d.Type, "Dolby Vision") {
			return "DOVI"
		}
	}
	switch transfer {
	case "smpte2084":
		return "HDR10"
	case "arib-std-b67":
		return "HLG"
	}
	return "SDR"
}

// streamTicks reads a stream's duration from ffprobe's "12.5" or a Matroska
// tag's "00:00:12.500000000", or returns 0 if it has none.
func streamTicks(seconds, tag string) int64 {
	value, err := strconv.ParseFloat(seconds, 64)
	if err != nil {
		var h, m int
		var s float64
		if _, err := fmt.Sscanf(tag, "%d:%d:%f", &h, &m, &s); err != nil {
			return 0
		}
		value = float64(h*3600+m*60) + s
	}
	if !(value > 0) {
		return 0
	}
	return int64(min(value, maxDuration) * ticksPerSecond)
}

// containerName picks the conventional name from ffprobe's comma-separated
// demuxer list, such as "matroska,webm" or "mov,mp4,m4a,3gp,3g2,mj2".
func containerName(format string) string {
	switch first, _, _ := strings.Cut(format, ","); first {
	case "matroska":
		return "mkv"
	case "mov":
		return "mp4"
	case "mpegts":
		return "ts"
	default:
		return first
	}
}

func frameRate(value string) float64 {
	numerator, denominator, found := strings.Cut(value, "/")
	if !found {
		return 0
	}
	n, err1 := strconv.ParseFloat(numerator, 64)
	d, err2 := strconv.ParseFloat(denominator, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}

func displayAudio(language, codec string, channels int) string {
	layout := fmt.Sprintf("%d ch", channels)
	switch channels {
	case 1:
		layout = "Mono"
	case 2:
		layout = "Stereo"
	case 6:
		layout = "5.1"
	case 8:
		layout = "7.1"
	}
	return strings.TrimSpace(fmt.Sprintf("%s %s %s", language, strings.ToUpper(codec), layout))
}

// displaySubtitle is the menu label, which clients show as it is. Like
// Jellyfin's ("English - Default - SUBRIP") it says whether the track is the
// default or forced, so two tracks in one language can be told apart.
func displaySubtitle(language, codec string, isDefault, isForced bool) string {
	var parts []string
	if language != "" {
		parts = append(parts, language)
	}
	if isDefault {
		parts = append(parts, "Default")
	}
	if isForced {
		parts = append(parts, "Forced")
	}
	return strings.Join(append(parts, strings.ToUpper(codec)), " - ")
}

// ReadVideoEnd returns where the last video frame ends on the file's clock, in
// ticks. Neither a container's duration nor its tags say: they count from the
// file's first stream, not from where the video begins, and can be the audio's
// instead. So it reads the packets of the last group of pictures, from the last
// keyframe to the end of the file. ffprobe opens the start of the file (a few
// MB, more with some versions) and asks for that stretch twice, so it reads up to twice the last
// keyframe's distance to the end (at most MaxCopySegment of film; 120 MB at
// 40 Mbps), whatever the length of the film. Matroska and MP4 alike.
func ReadVideoEnd(ctx context.Context, input string, lastKeyframe int64) (int64, error) {
	// A little past the keyframe: the seek goes to the last one at or before it.
	from := strconv.FormatFloat(float64(lastKeyframe)/ticksPerSecond+0.005, 'f', 3, 64)
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-read_intervals", from+"%",
		"-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", "--", input)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	// The last frame shown ends the video. A packet without a duration is
	// given the gap before it.
	var last, before, duration float64
	found := false
	for _, line := range strings.Fields(out.String()) {
		pts, length, _ := strings.Cut(line, ",")
		t, err := strconv.ParseFloat(pts, 64)
		if err != nil {
			continue
		}
		if t > last || !found {
			before, last, found = last, t, true
			duration, _ = strconv.ParseFloat(length, 64)
		}
	}
	if !found {
		return 0, errors.New("ffprobe found no video packets after the last keyframe")
	}
	if duration <= 0 {
		duration = last - before
	}
	return int64((last + duration) * ticksPerSecond), nil
}
