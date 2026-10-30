package library

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

const ticksPerSecond = 10_000_000

// maxDuration is a century in seconds, 3e16 ticks, far below overflow.
const maxDuration = 100 * 365 * 86400

// Probe is what a media file says about itself.
type Probe struct {
	Container    string
	RunTimeTicks int64
	Bitrate      int64
	Width        int
	Height       int
	Streams      []map[string]any
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
		Width       int               `json:"width"`
		Height      int               `json:"height"`
		Channels    int               `json:"channels"`
		SampleRate  string            `json:"sample_rate"`
		BitRate     string            `json:"bit_rate"`
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
			entry["DisplayTitle"] = displayVideo(stream.Height, stream.CodecName)
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
			entry["DisplayTitle"] = strings.TrimSpace(stream.Tags["language"] + " " + strings.ToUpper(stream.CodecName))
		}
		probe.Streams = append(probe.Streams, entry)
	}
	return probe, nil
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

func displayVideo(height int, codec string) string {
	return fmt.Sprintf("%dp %s", height, strings.ToUpper(codec))
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
