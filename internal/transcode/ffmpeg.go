// Package transcode runs ffmpeg for transcode jobs and coordinates them
// through Postgres, so any stream replica can ask for a segment and any worker
// can make it.
package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"jellyscale/internal/playback"
)

const (
	audioRate = 48000
	// An AAC frame is 1024 samples.
	aacFrame = 1024.0 / audioRate
	// A restarted audio run begins this many AAC frames early, on the same grid
	// as a run from the start, so the encoder is warm at the cut.
	warmupFrames = 4
	// The encoder holds back this many frames before it emits a keyframe; it is
	// tied to bframes below.
	reorderFrames = 2
	// Keeps timestamps away from zero, where the muxer would clamp negatives.
	tsOffset = 100
)

// Command builds the ffmpeg run that writes segments start, start+1, ... of a
// spec into dir as N.m4s, with init.mp4 beside them. The settings are the
// required ones from spikes/seam/README.md, and every value that comes from a
// client is an integer by now.
//
// A restart has to splice with the segments before it, so its audio is cut on
// the same AAC frame grid as a run from zero; that takes the source's start
// time, frame rate and the first frame at the restart point.
func Command(ctx context.Context, input string, spec playback.Spec, start int, dir string) (*exec.Cmd, error) {
	seek := float64(start * playback.SegmentSeconds)
	hasAudio := spec.AudioIndex >= 0
	var audioSeek float64
	var drop string
	if start > 0 && hasAudio {
		base, frameDuration, err := sourceTiming(ctx, input)
		if err != nil {
			return nil, err
		}
		keyframe, err := firstFrameAt(ctx, input, seek+base)
		if err != nil {
			return nil, err
		}
		firstFrame := max(int((seek+1e-9)/aacFrame)-warmupFrames, 0)
		audioSeek = float64(firstFrame) * aacFrame
		// The muxer cuts audio at the keyframe's DTS. x264 sees frame numbers,
		// so the keyframe's time rounds to a whole frame before the delay is
		// taken off; a source that doesn't start on a frame boundary would
		// otherwise be off by a fraction of a frame, enough to move the cut by
		// an audio packet.
		cut := (math.Round(keyframe/frameDuration)-reorderFrames)*frameDuration - base
		cutFrame := math.Ceil((cut+aacFrame)/aacFrame - 1e-6)
		threshold := (int64(cutFrame)-1)*1024 + int64(math.Round(base*audioRate))
		drop = fmt.Sprintf("noise=drop=lt(pts\\,%d)", threshold)
	}

	args := []string{"-nostdin", "-v", "warning", "-y", "-copyts", "-ss", seconds(seek), "-i", input}
	if hasAudio {
		args = append(args, "-ss", seconds(audioSeek), "-i", input)
	}
	args = append(args, "-map", "0:v:0")
	if hasAudio {
		args = append(args, "-map", "1:"+strconv.Itoa(spec.AudioIndex))
	}
	vb := strconv.FormatInt(spec.VideoBitrate, 10)
	args = append(args,
		// Frames the decoder marks interlaced are deinterlaced, since the output
		// is progressive; the rest pass through untouched. The captured profile
		// asks for progressive H.264.
		"-vf", fmt.Sprintf("yadif=mode=send_frame:parity=auto:deint=interlaced,scale=%d:%d:flags=bicubic,format=yuv420p", spec.Width, spec.Height),
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "51",
		"-crf", "23", "-maxrate", vb, "-bufsize", strconv.FormatInt(2*spec.VideoBitrate, 10),
		// Fixed B-frames and a closed GOP, so a restart makes the same frames.
		"-x264-params", "bframes=2:b-adapt=0:open-gop=0:scenecut=0", "-forced-idr", "1",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", playback.SegmentSeconds),
	)
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate))
		if drop != "" {
			args = append(args, "-bsf:a", drop)
		}
	}
	args = append(args,
		"-output_ts_offset", strconv.Itoa(tsOffset),
		"-f", "hls", "-hls_time", strconv.Itoa(playback.SegmentSeconds), "-hls_playlist_type", "vod", "-hls_list_size", "0",
		"-hls_segment_type", "fmp4", "-hls_flags", "temp_file", "-start_number", strconv.Itoa(start),
		"-hls_fmp4_init_filename", "init.mp4",
		// Without these the muxer rebases to zero behind an edit list, and
		// segments lose their absolute time.
		"-hls_segment_options", "use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont",
		"-hls_segment_filename", filepath.Join(dir, "%d.m4s"), filepath.Join(dir, "index.m3u8"),
	)
	return exec.CommandContext(ctx, "ffmpeg", args...), nil
}

func seconds(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }

// sourceTiming returns the source's start time and the duration of one video
// frame.
func sourceTiming(ctx context.Context, input string) (base, frameDuration float64, err error) {
	out, err := ffprobe(ctx, "-select_streams", "v:0", "-show_entries", "format=start_time:stream=r_frame_rate", "-of", "json", input)
	if err != nil {
		return 0, 0, err
	}
	var parsed struct {
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
		Streams []struct {
			RFrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Streams) == 0 {
		return 0, 0, errors.New("ffprobe found no video stream")
	}
	base, _ = strconv.ParseFloat(parsed.Format.StartTime, 64)
	numerator, denominator, _ := strings.Cut(parsed.Streams[0].RFrameRate, "/")
	n, _ := strconv.ParseFloat(numerator, 64)
	d, _ := strconv.ParseFloat(denominator, 64)
	if n <= 0 || d <= 0 {
		return 0, 0, fmt.Errorf("source frame rate %q", parsed.Streams[0].RFrameRate)
	}
	return base, d / n, nil
}

// firstFrameAt returns the time of the first source video frame at or after t.
// ffprobe starts reading at the keyframe before t, and counts the interval's
// end as an absolute time, so the end is given as t plus two seconds.
func firstFrameAt(ctx context.Context, input string, t float64) (float64, error) {
	out, err := ffprobe(ctx, "-select_streams", "v:0", "-read_intervals", seconds(t)+"%"+seconds(t+2),
		"-show_entries", "packet=pts_time", "-of", "csv=p=0", input)
	if err != nil {
		return 0, err
	}
	first := math.Inf(1)
	for _, line := range strings.Fields(string(out)) {
		if pts, err := strconv.ParseFloat(strings.TrimRight(line, ","), 64); err == nil && pts >= t-1e-6 {
			first = min(first, pts)
		}
	}
	if math.IsInf(first, 1) {
		return 0, fmt.Errorf("no video frame at %.3fs", t)
	}
	return first, nil
}

func ffprobe(ctx context.Context, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", append([]string{"-v", "error"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}
