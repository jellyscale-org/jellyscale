// Package transcode runs ffmpeg for transcode jobs and coordinates them
// through Postgres, so any stream replica can ask for a segment and any worker
// can make it.
package transcode

import (
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

	"jellyscale/internal/library"
	"jellyscale/internal/mediatool"
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
// which is after the packet of a cue that began earlier and is still showing; so
// the run opens early enough to read that packet (cueStart), and drops the
// frames before the cut, and draws the cue as a run from zero does (DOMAINS 51).
func Command(ctx context.Context, input string, spec playback.Spec, start int, dir, ass string, image *playback.Stream, enc encoder) (*exec.Cmd, error) {
	seek := float64(start * playback.SegmentSeconds)
	hasAudio := spec.AudioIndex >= 0
	copied := spec.AudioCopy != ""
	burn := spec.Subtitle != nil && image == nil
	// A track beside the film is a file of its own, read from its start.
	external := image != nil && image.File != ""
	// A restart draws an image track inside the film from its stored copy, a second input, when the
	// worker has one (JS-089): the film is then opened at the cut like a film with no subtitle.
	stored := image != nil && image.Stored != nil && start > 0
	var base, frameDuration float64
	if start > 0 || burn || external {
		var err error
		if base, frameDuration, err = sourceTiming(ctx, input); err != nil {
			return nil, err
		}
	}
	// A restart's video has to begin at the first frame at or after the cut, as a
	// run from zero's segment does; seamPlan says where to seek for that.
	var keyframe float64
	var grid int64
	trim := ""
	early := math.Inf(1) // The start of the earliest cue still showing at the cut, when there is one
	if start > 0 {
		var err error
		if keyframe, err = firstFrameAt(ctx, input, seek+max(base, 0)); err != nil {
			return nil, err
		}
		if image != nil && !external && !stored {
			// The frames before the cut are decoded and dropped by trim, not -ss,
			// so the first one kept is the same on every ffmpeg.
			trim = fmt.Sprintf("trim=start=%s,", seconds(keyframe-frameDuration/4))
			if early, err = cueStart(image, keyframe); err != nil {
				return nil, err
			}
		}
		if hasAudio && !copied {
			// A run from the start puts its AAC packets on a grid that begins where the
			// track does, which is not on a multiple of a frame for every file, so the
			// restart is asked for where that grid has a packet and cuts on it.
			if grid, err = audioGrid(ctx, input, spec); err != nil {
				return nil, err
			}
		}
	}
	videoSeek, audioSeek, drop := seamPlan(enc, base, frameDuration, keyframe, grid, copied, start)
	if !hasAudio {
		audioSeek, drop = 0, ""
	}
	if early < keyframe {
		// A second before the packet, so that the demuxer's seek lands before it.
		videoSeek = max(min(videoSeek, early-max(base, 0)-1), 0)
	}

	args := []string{"-nostdin", "-v", "warning", "-progress", "pipe:1", "-nostats", "-y", "-copyts", "-ss", seconds(videoSeek)}
	canvas := []string{}
	if image != nil && image.Width > 0 && image.Height > 0 {
		// The size the track's bitmaps are placed on, which its stream may not say.
		canvas = []string{"-canvas_size", fmt.Sprintf("%dx%d", image.Width, image.Height)}
	}
	if !external && !stored {
		args = append(args, canvas...)
	}
	args = append(args, "-i", input)
	inputs := 1
	if hasAudio {
		args = append(args, "-ss", seconds(audioSeek), "-i", input)
		inputs++
	}
	if external {
		// Its times are the cues' own, counted from 0, and the film's clock starts at base.
		args = append(args, canvas...)
		if base != 0 {
			args = append(args, "-itsoffset", seconds(base))
		}
		args = append(args, "-i", image.File)
	}
	var feed io.ReadCloser
	if stored {
		// The track's times are the film's own, which -copyts keeps, so it needs no offset. It is
		// read from standard input, which the worker's store reads feed as ffmpeg takes them.
		var err error
		if feed, err = image.Stored(keyframe); err != nil {
			return nil, err
		}
		args = append(args, canvas...)
		args = append(args, "-i", "pipe:0")
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
	// An HDR picture is tone mapped at the output's size, which is a fifth of the
	// work of a 4K one, and before the subtitles, which would otherwise be dimmed
	// with it.
	if spec.ToneMap != "" {
		picture += "," + playback.ToneMapFilter(spec.ToneMap)
	}
	if image == nil {
		args = append(args, "-vf", picture+subtitles+",format=yuv420p")
	} else {
		// eof_action=pass lets the video go on when the track's packets end,
		// as they do before the film does.
		track := fmt.Sprintf("0:%d", *spec.Subtitle)
		if external || stored {
			track = fmt.Sprintf("%d:0", inputs)
		}
		args = append(args, "-filter_complex", fmt.Sprintf("[0:v:0]%s%s[v];[%s]scale=%d:%d[s];[v][s]overlay=eof_action=pass:repeatlast=0,format=yuv420p[o]",
			trim, picture, track, spec.Width, spec.Height), "-map", "[o]")
	}
	args = append(args, enc.videoArgs...)
	args = append(args,
		"-crf", "23", "-maxrate", vb, "-bufsize", strconv.FormatInt(2*spec.VideoBitrate, 10),
		"-x264-params", enc.params, "-forced-idr", "1",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", playback.SegmentSeconds),
	)
	if spec.ToneMap != "" {
		// The stream says what the picture now is; a player would otherwise guess
		// from its size.
		args = append(args, "-colorspace", "bt709", "-color_primaries", "bt709", "-color_trc", "bt709", "-color_range", "tv")
	}
	if hasAudio && copied {
		// No -strict -2: FLAC and Opus go into MP4 without it from ffmpeg 6.1
		// on, and on 6.1 it lets the muxer copy a Dolby Vision source's dvcC
		// box into the H.264 stream, which Chrome then won't play.
		args = append(args, "-c:a", "copy")
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
	cmd := mediatool.Command(ctx, "ffmpeg", args...)
	if feed != nil {
		cmd.Stdin = feed
	}
	if burn {
		cmd.Dir = dir // Where the subtitle filter finds its file; dir is absolute.
	}
	return cmd, nil
}

// audioGrid is the time, in samples, of the first AAC packet a run from the
// start of the film makes for the audio track: the packets of every run fall
// at that time plus whole frames of 1024 samples. Only a prefix of the track is
// encoded: two frames, because a track whose decoder loses its first frames (a
// damaged head, as DTS-HD MA 5.1 can have) makes no packet from one. The first
// packet is the grid's. The prefix is read the way the run from the start opens it:
// a seek to 0 trims what comes before 0, which moves the grid of a track that
// starts a little early.
func audioGrid(ctx context.Context, input string, spec playback.Spec) (int64, error) {
	res, err := mediatool.Run(ctx, "ffmpeg", []string{"-nostdin", "-v", "error", "-copyts", "-ss", seconds(0), "-i", input, "-map", "0:" + strconv.Itoa(spec.AudioIndex),
		"-frames:a", "2", "-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate), "-f", "framecrc", "-"}, mediatool.Options{})
	if err != nil {
		var failed *mediatool.Error
		if errors.As(err, &failed) && failed.Output != "" {
			return 0, fmt.Errorf("finding where the audio's packets fall: %w: %s", err, failed.Output)
		}
		return 0, fmt.Errorf("finding where the audio's packets fall: %w", err)
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		// stream, dts, pts, duration, size, crc
		if fields := strings.Split(line, ","); len(fields) >= 6 && !strings.HasPrefix(line, "#") {
			if pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64); err == nil {
				return pts, nil
			}
		}
	}
	return 0, errors.New("finding where the audio's packets fall: ffmpeg made no packet")
}

// encoder is what belongs to the video encoder: its ffmpeg arguments, and how
// many frames it holds back before a keyframe, which is tied to its B-frames.
type encoder struct {
	videoArgs     []string // From -c:v to the level
	params        string   // The value of -x264-params: fixed B-frames and a closed GOP, so a restart makes the same frames
	reorderFrames int
}

var x264 = encoder{
	videoArgs:     []string{"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "51"},
	params:        "bframes=2:b-adapt=0:open-gop=0:scenecut=0",
	reorderFrames: 2,
}

// seamPlan is where a run for segment N seeks the video and the audio, and
// which audio packets it drops, so that its segments splice with those of a run
// from zero. It does no I/O: base is the source's start time, frameDuration the
// video's, keyframe the first video frame at or after the cut, and grid the
// time in samples of the first AAC packet a run from zero makes (not read for
// copied audio).
func seamPlan(enc encoder, base, frameDuration, keyframe float64, grid int64, copiedAudio bool, segment int) (videoSeek, audioSeek float64, drop string) {
	if segment == 0 {
		return 0, 0, ""
	}
	seek := float64(segment * playback.SegmentSeconds)
	// -ss drops the frames before its time, but ffmpeg 9 keeps a frame from its
	// own time on and ffmpeg 6 from a half frame before it, so a cut that falls
	// inside a frame gives ffmpeg 6 one frame more. A quarter frame before the
	// first frame is read as that frame by both. (A negative start time doesn't
	// move the cut, a positive one does.)
	videoSeek = keyframe - max(base, 0) - frameDuration/4
	if copiedAudio {
		// The packets are the source's own, so no grid or warm-up is needed: the
		// run starts with the packet a run from zero puts first in the segment,
		// the first at or after the video's cut (the keyframe's DTS), and drops
		// what comes before. It reads from two seconds earlier than the segment,
		// more than a packet or the first keyframe's delay, and the drop compares
		// time in seconds so no timebase is needed.
		cut := (math.Round(keyframe/frameDuration) - float64(enc.reorderFrames)) * frameDuration
		// A packet exactly at the cut stays; a microsecond of slack covers the
		// product's rounding.
		return videoSeek, max(seek-2, 0), fmt.Sprintf("noise=drop=lt(pts*tb\\,%s)", seconds(cut-1e-6))
	}
	audioSeek, first := aacCut(enc.reorderFrames, base, frameDuration, keyframe, grid)
	return videoSeek, audioSeek, fmt.Sprintf("noise=drop=lt(pts\\,%d)", first)
}

// aacCut is where an AAC run seeks (relative to the file's start, as -ss counts) and the time in samples of the first
// packet that belongs to the segment whose first frame is keyframe (seconds on the file's clock), on the grid of a run
// from the start. reorder is how many frames the video encoder holds back before a keyframe.
func aacCut(reorder int, base, frameDuration, keyframe float64, grid int64) (seek float64, first int64) {
	// The grid's phase on the file's own clock, which is what -ss counts.
	phase := float64(grid) - base*audioRate
	// The muxer cuts audio at the keyframe's DTS. The encoder sees frame numbers,
	// so the keyframe's time rounds to a whole frame before the delay is
	// taken off; a source that doesn't start on a frame boundary would
	// otherwise be off by a fraction of a frame, enough to move the cut by
	// an audio packet.
	cut := (math.Round(keyframe/frameDuration)-float64(reorder))*frameDuration - base
	// The run starts warmupFrames before the cut: the encoder emits nothing
	// for a frame's worth, and the delay is more than a frame of 12.5 fps.
	firstFrame := max(int(math.Floor((cut*audioRate-phase)/1024+1e-9))-warmupFrames, int(math.Ceil(-phase/1024)))
	seek = (float64(firstFrame)*1024 + phase) / audioRate
	return seek, int64(math.Ceil((cut*audioRate-phase)/1024-1e-6))*1024 + grid
}

// copyReadRate is how many times the film's speed a copy reads its input.
var copyReadRate = "5"

// CopyCommand builds the ffmpeg run for a copied stream: the video is copied
// and the audio is copied (spec.AudioCopy) or re-encoded, and one fragment is written for each keyframe, as
// g0.m4s, g1.m4s ... with init.mp4 beside them. gather joins the fragments
// into the playlist's segments, which ffmpeg can't cut the same way from every
// start: the hls muxer counts its segment lengths from where it began.
//
// The input is read at copyReadRate times the film's speed, which stops a copy
// from reading, writing and uploading the whole film as fast as the disk and
// the store allow. It does not follow the viewer: a copy of both
// streams opens the file once. A video copy with re-encoded audio opens it twice,
// once for the video and once for the audio, so ten times the film's bitrate is
// read while the job runs (twice the film's, per film), until the film ends or
// the job is stopped.
//
// codec is the source's video codec.
//
// video and audio say where in the source's clock (seconds) to begin, or are
// negative to begin at the start of the file. A run for segment N begins at
// the keyframe before it and its audio two seconds early, on the same AAC
// frame grid as a run from the start. A copied audio is the source's own packets
// and needs no grid or warm-up: it comes from the video's input and so begins at the video's cut. The video then reaches segment N's
// keyframe as it would in a run from the start, with the same decode time,
// and the audio is warm and whole by then, with no need to know which packet
// the muxer will cut at. What comes before segment N is dropped.
func CopyCommand(ctx context.Context, input string, spec playback.Spec, codec string, video, audio float64, dir string) (*exec.Cmd, error) {
	return fragmentCommand(ctx, input, spec, codec, video, audio, dir, nil)
}

// RecodeCommand is CopyCommand for the video of a copy whose index was found wrong (spec.Recode): the
// picture is encoded, with a keyframe at each of cuts (the times, in the source's clock in seconds, at which
// the copy's playlist starts a segment), and the run begins at video, the start of the segment it makes
// first. Every keyframe is a cut as in a copy, so the fragments are the playlist's segments and gather joins
// them as it does for a copy.
func RecodeCommand(ctx context.Context, input string, spec playback.Spec, codec string, video, audio float64, cuts []float64, dir string) (*exec.Cmd, error) {
	if _, err := recodeArgs(codec, spec); err != nil {
		return nil, err
	}
	return fragmentCommand(ctx, input, spec, codec, video, audio, dir, cuts)
}

// fragmentCommand is the run that writes a copy's fragments, with the video as it is (cuts nil) or encoded.
func fragmentCommand(ctx context.Context, input string, spec playback.Spec, codec string, video, audio float64, dir string, cuts []float64) (*exec.Cmd, error) {
	hasAudio := spec.AudioIndex >= 0
	recode := spec.Recode != ""
	var base, frame float64
	if video >= 0 {
		var err error
		if base, frame, err = sourceTiming(ctx, input); err != nil {
			return nil, err
		}
	}
	// Errors only: -readrate logs a warning each time it lets ffmpeg read again.
	args := []string{"-nostdin", "-v", "error", "-progress", "pipe:1", "-nostats", "-y", "-copyts"}
	if video >= 0 {
		if recode {
			// The first frame at or after the segment's start, as Command cuts a restart.
			args = append(args, "-ss", seconds(max(video-base-frame/4, 0)))
		} else {
			// A little past the keyframe: the seek goes to the last one at or before
			// the time, and the file's own times are exact to a millisecond.
			args = append(args, "-ss", seconds(max(video-base, 0)+0.005))
		}
	}
	rate := []string{"-readrate", copyReadRate}
	if recode {
		rate = nil // The encoder holds the run back.
	}
	args = append(args, rate...)
	args = append(args, "-i", input)
	both := spec.AudioCopy != ""
	// A copied audio is the source's own packets and needs no grid of its own, so
	// it comes from the one input; only a re-encoded track opens the file again,
	// to start its AAC frames on their own grid.
	// A restart of an HDR copy's fallback (spec.HDR) begins its audio before the segment, and drops what the muxer of a
	// run from zero puts in the segments before it, as a restart of a re-encode does (seamPlan): the same packets in the
	// first segment as a run from the start makes. The encoder's delay is that of recodeArgs' fixed two B-frames. Every
	// other fallback keeps the audio arguments it had (JS-088, JS-133).
	hdr := recode && spec.HDR != ""
	drop := ""
	if hdr && video >= 0 && hasAudio {
		if both {
			cut := (math.Round(video/frame) - recodeReorder) * frame
			drop = fmt.Sprintf("noise=drop=lt(pts*tb\\,%s)", seconds(cut-1e-6))
		}
	}
	if hasAudio && !both {
		switch {
		case hdr && video >= 0:
			grid, err := audioGrid(ctx, input, spec)
			if err != nil {
				return nil, err
			}
			seek, first := aacCut(recodeReorder, base, frame, video, grid)
			args = append(args, "-ss", seconds(seek))
			drop = fmt.Sprintf("lt(pts\\,%d)", first)
			drop = "noise=drop=" + drop
		case hdr:
			// The run from the start opens the track as audioGrid does, so that its packets are on the grid a restart cuts on.
			args = append(args, "-ss", seconds(0))
		case audio >= 0:
			args = append(args, "-ss", seconds(math.Floor(max(audio-base, 0)/aacFrame)*aacFrame))
		}
		args = append(args, rate...)
		args = append(args, "-i", input)
	}
	args = append(args, "-map", "0:v:0")
	if recode {
		picture := fmt.Sprintf("yadif=mode=send_frame:parity=auto:deint=interlaced,scale=%d:%d:flags=bicubic", spec.Width, spec.Height)
		if spec.ToneMap != "" {
			picture += "," + playback.ToneMapFilter(spec.ToneMap)
		}
		encode, _ := recodeArgs(codec, spec)
		args = append(args, "-vf", picture+","+recodeFormat(spec))
		args = append(args, encode...)
		times := make([]string, len(cuts))
		for i, cut := range cuts {
			times[i] = seconds(cut)
		}
		args = append(args, "-force_key_frames", strings.Join(times, ","))
	} else {
		args = append(args, "-c:v", "copy")
		if codec == "hevc" {
			// The muxer writes hvc1 or hev1 by the ffmpeg version and the input's
			// container, which would give workers different init segments. hvc1 is
			// what the master playlist's CODECS says and what Chrome and Safari take.
			args = append(args, "-tag:v", "hvc1")
		}
	}
	if hasAudio && both {
		args = append(args, "-map", "0:"+strconv.Itoa(spec.AudioIndex), "-c:a", "copy")
	} else if hasAudio {
		args = append(args, "-map", "1:"+strconv.Itoa(spec.AudioIndex),
			"-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate))
	}
	if drop != "" {
		args = append(args, "-bsf:a", drop)
	}
	return mediatool.Command(ctx, "ffmpeg", append(args, fragmentOutput(dir)...)...), nil
}

// recodeReorder is how many frames recodeArgs' encoders hold back before a keyframe (bframes=2).
const recodeReorder = 2

// fragmentOutput is the muxer's end of the command that writes fragments, g0.m4s, g1.m4s ... with init.mp4.
func fragmentOutput(dir string) []string {
	return []string{
		"-output_ts_offset", strconv.Itoa(tsOffset),
		// Every keyframe is a cut: the segment length is so short that the muxer
		// is always past it.
		"-f", "hls", "-hls_time", "0.001", "-hls_list_size", "0",
		"-hls_segment_type", "fmp4", "-hls_flags", "temp_file", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_options", "use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont+skip_sidx",
		"-hls_segment_filename", filepath.Join(dir, "g%d.m4s"), filepath.Join(dir, "index.m3u8"),
	}
}

// AV1SegmentCommand is RecodeCommand for an AV1 copy: SVT-AV1 in ffmpeg 6.1 ignores -force_key_frames, so each
// segment of the playlist is encoded by a run of its own, which starts on a keyframe on any ffmpeg and stops at
// the next segment's start. start and end are the times (the source's clock, in seconds) at which the segment
// and the one after it begin, end negative for the last; start is negative for the first, which opens the file at
// its start. Every segment is a function of its own start and end, so a restart makes what a run from the start
// does. The audio is cut as a run from zero cuts it: copied packets in the same span as the video's, AAC packets
// on the grid of a run from zero (aacCut).
func AV1SegmentCommand(ctx context.Context, input string, spec playback.Spec, start, end float64, dir string) (*exec.Cmd, error) {
	encode, err := recodeArgs("av1", spec)
	if err != nil {
		return nil, err
	}
	hasAudio := spec.AudioIndex >= 0
	base, frame, err := sourceTiming(ctx, input)
	if err != nil {
		return nil, err
	}
	// A quarter frame before the first frame is read as that frame on every ffmpeg (see seamPlan), and a
	// neighbour's end is the same time, so no frame is in two segments or in none.
	var seek float64
	if start >= 0 {
		seek = max(start-base-frame/4, 0)
	}
	args := []string{"-nostdin", "-v", "error", "-progress", "pipe:1", "-nostats", "-y", "-copyts"}
	if start >= 0 {
		args = append(args, "-ss", seconds(seek))
	}
	if end >= 0 {
		// An input's -t counts from its seek, on the file's clock.
		args = append(args, "-t", seconds(end-base-frame/4-seek))
	}
	args = append(args, "-i", input)
	// The audio's own input, when it is re-encoded, comes before any output option.
	var audio []string
	switch {
	case hasAudio && spec.AudioCopy != "":
		// The source's own packets, from the input the video is read from.
		audio = []string{"-map", "0:" + strconv.Itoa(spec.AudioIndex), "-c:a", "copy"}
	case hasAudio:
		var grid int64
		if start >= 0 || end >= 0 {
			if grid, err = audioGrid(ctx, input, spec); err != nil {
				return nil, err
			}
		}
		var audioSeek float64
		drop := ""
		if start >= 0 {
			var first int64
			audioSeek, first = aacCut(0, base, frame, start, grid)
			args = append(args, "-ss", seconds(audioSeek))
			drop = fmt.Sprintf("lt(pts\\,%d)", first)
		}
		if end >= 0 {
			// Two frames more than the last packet needs, which the drop takes off.
			_, last := aacCut(0, base, frame, end, grid)
			args = append(args, "-t", seconds(float64(last)/audioRate-base-audioSeek+2*aacFrame))
			if drop != "" {
				drop += "+"
			}
			drop += fmt.Sprintf("gte(pts\\,%d)", last)
		}
		args = append(args, "-i", input)
		audio = []string{"-map", "1:" + strconv.Itoa(spec.AudioIndex),
			"-c:a", "aac", "-b:a", strconv.FormatInt(spec.AudioBitrate, 10), "-ac", "2", "-ar", strconv.Itoa(audioRate)}
		if drop != "" {
			audio = append(audio, "-bsf:a", "noise=drop="+drop)
		}
	}
	picture := fmt.Sprintf("yadif=mode=send_frame:parity=auto:deint=interlaced,scale=%d:%d:flags=bicubic", spec.Width, spec.Height)
	if spec.ToneMap != "" {
		picture += "," + playback.ToneMapFilter(spec.ToneMap)
	}
	args = append(args, "-map", "0:v:0", "-vf", picture+","+recodeFormat(spec))
	args = append(args, encode...)
	args = append(args, audio...)
	return mediatool.Command(ctx, "ffmpeg", append(args, fragmentOutput(dir)...)...), nil
}

// recodeFormat is the pixel format of a fallback's picture: 8 bits, except for an HDR copy's (spec.HDR), which keeps
// its 10 bits. No tags are named: the decoded frames carry the copy's own and the encoders write them (compat/captures/hdr-fallback-12.4.md).
func recodeFormat(spec playback.Spec) string {
	if spec.HDR != "" {
		return "format=yuv420p10le"
	}
	return "format=yuv420p"
}

// recodeArgs is the encoder of a copy's video that is made again: the codec the copy had, since the
// player's buffer is typed by the master playlist's, with the fixed B-frames and closed GOP that make
// a restart produce the same frames, and a keyframe only where the playlist cuts (scenecut off, keyint far
// off). An AV1 fallback has one run for each segment instead.
func recodeArgs(codec string, spec playback.Spec) ([]string, error) {
	vb := spec.VideoBitrate
	switch codec {
	case "h264":
		return []string{"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-level", "51",
			"-crf", "23", "-maxrate", strconv.FormatInt(vb, 10), "-bufsize", strconv.FormatInt(2*vb, 10),
			"-x264-params", "bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000", "-forced-idr", "1"}, nil
	case "hevc":
		return []string{"-c:v", "libx265", "-preset", "veryfast", "-tag:v", "hvc1", "-crf", "26",
			"-x265-params", fmt.Sprintf("bframes=2:b-adapt=0:open-gop=0:scenecut=0:keyint=100000:vbv-maxrate=%d:vbv-bufsize=%d:log-level=error", vb/1000, 2*vb/1000),
			"-forced-idr", "1"}, nil
	case "av1":
		// One run for each segment (AV1SegmentCommand), so the only keyframe is the first and the others are never wanted.
		return []string{"-c:v", "libsvtav1", "-preset", "10", "-crf", "32", "-g", "100000"}, nil
	}
	return nil, fmt.Errorf("a %s copy whose keyframe index was found wrong cannot go on: its re-encode fallback is not built", codec)
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

// cueLookback is how far before its cut a run begins that draws an image track
// whose cue times are not known: MPEG-TS, a Matroska file without cue points for
// the track. A cue that began earlier is missed until the next one.
const cueLookback = 10.0

// cueStart is the time a run with its first frame at t must open the film at to
// read the picture of the image track showing there: with the track's cue points
// known it is the start of that picture (t if none is showing); without them,
// cueLookback before t.
func cueStart(image *playback.Stream, t float64) (float64, error) {
	if image.OpenAt == nil {
		return t - cueLookback, nil
	}
	open, err := image.OpenAt(t)
	if errors.Is(err, library.ErrUnknownPicture) {
		return t - cueLookback, nil
	}
	return open, err
}

func ffprobe(ctx context.Context, args ...string) ([]byte, error) {
	return ffprobeFrom(ctx, nil, args...)
}

// ffprobeFrom runs ffprobe with stdin as its input, if there is one.
func ffprobeFrom(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	res, err := mediatool.Run(ctx, "ffprobe", append([]string{"-v", "error"}, args...), mediatool.Options{Stdin: stdin})
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, strings.TrimSpace(string(res.Stderr)))
	}
	return res.Stdout, nil
}
