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
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"jellyscale/internal/playback"
	subs "jellyscale/internal/subtitles"
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
//
// When the spec has a subtitle, ass is that track as an ASS file, with times as
// the track's file has them, counted from the source's start time. It is drawn
// into the picture after the scale, so it is sharp at the output size, and
// from a file in dir, so no client text reaches the filter's arguments.
//
// An image track (PGS, VobSub, DVB) is image, a stream of the same input as the
// video, and ass is empty. It is scaled to the output's size and overlaid after
// the video is, so its bitmaps are as sharp as the picture and need no file. The
// track has few packets, and the overlay waits for the next one, but ffmpeg tells
// it the video's time as the video advances, so a gap between cues doesn't hold
// the video back (unless the input is read with -readrate, which waits for the
// track). A restart at start > 0 opens the film at the keyframe before the cut,
// so a cue whose packet came before that is missing from that run though a run
// from zero draws it (DOMAINS 51).
func Command(ctx context.Context, input string, spec playback.Spec, start int, dir, ass string, image *playback.Stream) (*exec.Cmd, error) {
	seek := float64(start * playback.SegmentSeconds)
	hasAudio := spec.AudioIndex >= 0
	burn := spec.Subtitle != nil && image == nil
	var base, frameDuration float64
	if (start > 0 && hasAudio) || burn {
		var err error
		if base, frameDuration, err = sourceTiming(ctx, input); err != nil {
			return nil, err
		}
	}
	var audioSeek float64
	var drop string
	copied := spec.AudioCopy != ""
	if start > 0 && hasAudio && copied {
		var err error
		if audioSeek, drop, err = copiedAudioCut(ctx, input, seek, base, frameDuration); err != nil {
			return nil, err
		}
	}
	if start > 0 && hasAudio && !copied {
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

	args := []string{"-nostdin", "-v", "warning", "-y", "-copyts", "-ss", seconds(seek)}
	if image != nil && image.Width > 0 && image.Height > 0 {
		// The size the track's bitmaps are placed on, which its stream may not say.
		args = append(args, "-canvas_size", fmt.Sprintf("%dx%d", image.Width, image.Height))
	}
	args = append(args, "-i", input)
	if hasAudio {
		args = append(args, "-ss", seconds(audioSeek), "-i", input)
	}
	if image == nil {
		args = append(args, "-map", "0:v:0")
	}
	if hasAudio {
		args = append(args, "-map", "1:"+strconv.Itoa(spec.AudioIndex))
	}
	vb := strconv.FormatInt(spec.VideoBitrate, 10)
	subtitles := ""
	if burn {
		// -copyts keeps the frames' times in the source's own clock, which starts
		// at base, and the track's cues start at 0.
		if err := os.WriteFile(filepath.Join(dir, "subtitles.ass"), []byte(subs.Delay(ass, base)), 0o644); err != nil {
			return nil, err
		}
		// The film's fonts, if the worker put any in fonts, draw the track as they
		// do in the player; libass looks in the system's for the rest.
		if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o755); err != nil {
			return nil, err
		}
		subtitles = ",subtitles=subtitles.ass:fontsdir=fonts"
	}
	// Frames the decoder marks interlaced are deinterlaced, since the output
	// is progressive; the rest pass through untouched. The captured profile
	// asks for progressive H.264.
	picture := fmt.Sprintf("yadif=mode=send_frame:parity=auto:deint=interlaced,scale=%d:%d:flags=bicubic", spec.Width, spec.Height)
	if image == nil {
		args = append(args, "-vf", picture+subtitles+",format=yuv420p")
	} else {
		// eof_action=pass lets the video go on when the track's packets end,
		// as they do before the film does.
		args = append(args, "-filter_complex", fmt.Sprintf("[0:v:0]%s[v];[0:%d]scale=%d:%d[s];[v][s]overlay=eof_action=pass:repeatlast=0,format=yuv420p[o]",
			picture, *spec.Subtitle, spec.Width, spec.Height), "-map", "[o]")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "51",
		"-crf", "23", "-maxrate", vb, "-bufsize", strconv.FormatInt(2*spec.VideoBitrate, 10),
		// Fixed B-frames and a closed GOP, so a restart makes the same frames.
		"-x264-params", "bframes=2:b-adapt=0:open-gop=0:scenecut=0", "-forced-idr", "1",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", playback.SegmentSeconds),
	)
	if hasAudio && copied {
		// -strict -2 lets older ffmpegs write FLAC and Opus into MP4.
		args = append(args, "-c:a", "copy", "-strict", "-2")
	} else if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate))
	}
	if drop != "" {
		args = append(args, "-bsf:a", drop)
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
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if burn {
		cmd.Dir = dir // Where the subtitle filter finds its file; dir is absolute.
	}
	return cmd, nil
}

// copiedAudioCut says where a restarted run reads copied audio from and which
// packets it drops. The packets are the source's own, so no grid or warm-up is
// needed: the run only has to start with the packet a run from zero puts first
// in the segment, the first at or after the video's cut (the keyframe's DTS, as
// in Command's AAC case), and drop what comes before. It reads from two seconds
// earlier than the segment, more than a packet or the first keyframe's delay,
// and the drop compares time in seconds so no timebase is needed.
func copiedAudioCut(ctx context.Context, input string, seek, base, frameDuration float64) (audioSeek float64, drop string, err error) {
	keyframe, err := firstFrameAt(ctx, input, seek+base)
	if err != nil {
		return 0, "", err
	}
	cut := (math.Round(keyframe/frameDuration) - reorderFrames) * frameDuration
	// A packet exactly at the cut stays; a microsecond of slack covers the
	// product's rounding.
	return max(seek-2, 0), fmt.Sprintf("noise=drop=lt(pts*tb\\,%s)", seconds(cut-1e-6)), nil
}

// copyReadRate is how many times the film's speed a copy reads its input.
var copyReadRate = "5"

// CopyCommand builds the ffmpeg run for a copied stream: the video is copied
// and the audio re-encoded, and one fragment is written for each keyframe, as
// g0.m4s, g1.m4s ... with init.mp4 beside them. gather joins the fragments
// into the playlist's segments, which ffmpeg can't cut the same way from every
// start: the hls muxer counts its segment lengths from where it began.
//
// The input is read at copyReadRate times the film's speed, which stops a copy
// from reading, writing and uploading the whole film as fast as the disk and
// the store allow. It does not follow the viewer: the file is opened twice, once
// for the video and once for the audio, so ten times the film's bitrate is read
// while the job runs (twice the film's, per film), until the film ends or the job is stopped.
//
// codec is the source's video codec.
//
// video and audio say where in the source's clock (seconds) to begin, or are
// negative to begin at the start of the file. A run for segment N begins at
// the keyframe before it and its audio two seconds early, on the same AAC
// frame grid as a run from the start. The video then reaches segment N's
// keyframe as it would in a run from the start, with the same decode time,
// and the audio is warm and whole by then, with no need to know which packet
// the muxer will cut at. What comes before segment N is dropped.
func CopyCommand(ctx context.Context, input string, spec playback.Spec, codec string, video, audio float64, dir string) (*exec.Cmd, error) {
	hasAudio := spec.AudioIndex >= 0
	var base float64
	if video >= 0 {
		var err error
		if base, _, err = sourceTiming(ctx, input); err != nil {
			return nil, err
		}
	}
	// Errors only: -readrate logs a warning each time it lets ffmpeg read again.
	args := []string{"-nostdin", "-v", "error", "-y", "-copyts"}
	if video >= 0 {
		// A little past the keyframe: the seek goes to the last one at or before
		// the time, and the file's own times are exact to a millisecond.
		args = append(args, "-ss", seconds(max(video-base, 0)+0.005))
	}
	args = append(args, "-readrate", copyReadRate, "-i", input)
	if hasAudio {
		if audio >= 0 {
			args = append(args, "-ss", seconds(math.Floor(max(audio-base, 0)/aacFrame)*aacFrame))
		}
		args = append(args, "-readrate", copyReadRate, "-i", input)
	}
	args = append(args, "-map", "0:v:0", "-c:v", "copy")
	if codec == "hevc" {
		// The muxer writes hvc1 or hev1 by the ffmpeg version and the input's
		// container, which would give workers different init segments. hvc1 is
		// what the master playlist's CODECS says and what Chrome and Safari take.
		args = append(args, "-tag:v", "hvc1")
	}
	if hasAudio {
		args = append(args, "-map", "1:"+strconv.Itoa(spec.AudioIndex),
			"-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate))
	}
	args = append(args,
		"-output_ts_offset", strconv.Itoa(tsOffset),
		// Every keyframe is a cut: the segment length is so short that the muxer
		// is always past it.
		"-f", "hls", "-hls_time", "0.001", "-hls_list_size", "0",
		"-hls_segment_type", "fmp4", "-hls_flags", "temp_file", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_options", "use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont+skip_sidx",
		"-hls_segment_filename", filepath.Join(dir, "g%d.m4s"), filepath.Join(dir, "index.m3u8"),
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
	return ffprobeFrom(ctx, nil, args...)
}

// ffprobeFrom runs ffprobe with stdin as its input, if there is one.
func ffprobeFrom(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffprobe", append([]string{"-v", "error"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}
