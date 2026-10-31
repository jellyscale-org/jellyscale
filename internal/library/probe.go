package library

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

const ticksPerSecond = 10_000_000

// maxDuration is a century in seconds, 3e16 ticks, far below overflow.
const maxDuration = 100 * 365 * 86400

// textSubtitleCodecs are the ffprobe names of the subtitle codecs that hold
// text.
var textSubtitleCodecs = []string{"subrip", "ass", "ssa", "webvtt", "mov_text"}

// Probe is what a media file says about itself.
type Probe struct {
	Container    string
	RunTimeTicks int64
	Bitrate      int64
	Width        int
	Height       int
	Streams      []map[string]any
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
		FieldOrder  string            `json:"field_order"`
		Transfer    string            `json:"color_transfer"`
		SideData    []sideData        `json:"side_data_list"`
		Width       int               `json:"width"`
		Height      int               `json:"height"`
		Channels    int               `json:"channels"`
		SampleRate  string            `json:"sample_rate"`
		BitRate     string            `json:"bit_rate"`
		Duration    string            `json:"duration"`
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
		if rate, err := strconv.ParseInt(stream.BitRate, 10, 64); err == nil {
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
			// The video can end before the audio does, and an HLS playlist
			// has to stop where the video does. Matroska keeps the duration
			// in a tag.
			if ticks := streamTicks(stream.Duration, stream.Tags["DURATION"]); ticks > 0 {
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
			entry["DisplayTitle"] = displaySubtitle(stream.Tags["language"], stream.CodecName, stream.Disposition.Default == 1, stream.Disposition.Forced == 1)
			// Only text tracks can be extracted and delivered; image ones
			// (PGS, VobSub, DVB) stay in media_sources.streams alone.
			text := slices.Contains(textSubtitleCodecs, stream.CodecName)
			entry["IsTextSubtitleStream"], entry["SupportsExternalStream"] = text, text
		}
		probe.Streams = append(probe.Streams, entry)
	}
	return probe, nil
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
