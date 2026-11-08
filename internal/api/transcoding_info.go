package api

import (
	"encoding/json"
	"math"

	"jellyscale/internal/playback"
)

// transcodeRow is what GET /Sessions reads from Postgres about the transcode a
// session watches: its newest job, and the reasons PlaybackInfo gave.
type transcodeRow struct {
	Spec        []byte
	Streams     []byte // the streams of the spec's source
	State       string
	Worker      *string
	NextSegment int
	EncodeSpeed *float64
	Reasons     []string
}

// transcodingInfo is Jellyfin's TranscodingInfo for a session, plus what only
// jellyscale knows: the job's EncodeSpeed (film seconds per second of its running
// time), its Worker, and IsBelowRealTime when the viewer's playback will stall.
// It is the one place the info is made.
func transcodingInfo(row transcodeRow, runTimeTicks int64) (map[string]any, error) {
	var spec playback.Spec
	if err := json.Unmarshal(row.Spec, &spec); err != nil {
		return nil, err
	}
	streams, err := playback.ParseStreams(row.Streams)
	if err != nil {
		return nil, err
	}
	videoCodec, audioCodec, channels, framerate := "h264", "aac", 2, 0.0
	for _, stream := range streams {
		switch {
		case stream.Type == "Video":
			framerate = stream.AverageFrameRate
			if spec.Copy {
				videoCodec = stream.Codec
			}
		case stream.Type == "Audio" && stream.Index == spec.AudioIndex && stream.Channels > 0:
			// A re-encode downmixes to stereo; a copy keeps the source's channels.
			channels = stream.Channels
			if spec.AudioCopy == "" {
				channels = min(channels, 2)
			}
		}
	}
	if spec.AudioCopy != "" {
		audioCodec = spec.AudioCopy
	}
	// The segments of a copy follow the source's keyframes, so this is an
	// estimate for them.
	done := 0.0
	if runTimeTicks > 0 {
		done = min(float64(row.NextSegment*playback.SegmentSeconds)*1e7*100/float64(runTimeTicks), 100)
	}
	info := map[string]any{
		"VideoCodec": videoCodec, "AudioCodec": audioCodec, "Container": "mp4",
		"IsVideoDirect": spec.Copy, "IsAudioDirect": spec.AudioCopy != "",
		"Bitrate": spec.VideoBitrate + spec.AudioBitrate, "Width": spec.Width, "Height": spec.Height,
		"AudioChannels": channels, "HardwareAccelerationType": "none",
		"CompletionPercentage": done, "Framerate": 0.0, "TranscodeReasons": append([]string{}, row.Reasons...),
	}
	// Only a running job has a worker and a speed worth showing.
	if row.State == "running" {
		if row.Worker != nil {
			info["Worker"] = *row.Worker
		}
		if speed := row.EncodeSpeed; speed != nil {
			info["EncodeSpeed"] = *speed
			info["Framerate"] = math.Round(*speed * framerate)
			info["IsBelowRealTime"] = *speed < 1
		}
	}
	return info, nil
}
