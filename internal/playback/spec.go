package playback

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// Output settings. Changing any of these changes the segments, so the spec
// hash includes them: bump OutputVersion with any change to the ffmpeg
// settings, and old segments simply stop being found.
const (
	OutputVersion  = 2
	SegmentSeconds = 4
	// Family is the encoder family; fMP4 can't splice across families.
	Family = "x264"

	// MaxHeight caps the picture a transcode produces.
	MaxHeight = 1080

	MinVideoBitrate = 50_000
	MinAudioBitrate = 32_000
	MaxAudioBitrate = 640_000

	ticksPerSecond = 10_000_000
	// MaxStartTicks is a century, far past any film.
	MaxStartTicks = 100 * 365 * 86400 * ticksPerSecond
)

// Params are the transcode inputs a client controls, taken from the URL query.
// They're the only client values a Spec is built from, and every request
// parses them again, because clients can edit the URL.
type Params struct {
	SourceID      string // compact hex
	AudioIndex    int    // -1 for none; set from the source when the URL omits it
	VideoBitrate  int64
	AudioBitrate  int64
	PlaySessionID string
	DeviceID      string
	// StartTicks is where the viewer starts, from PlaybackInfo's
	// StartTimeTicks. It is no part of the spec: it only says where a job
	// starts when the init segment, which comes first, is asked for.
	StartTicks int64
}

// StartSegment is the segment the viewer starts at, in a playlist of count.
func (p Params) StartSegment(count int) int {
	return int(min(p.StartTicks/(SegmentSeconds*ticksPerSecond), int64(count-1)))
}

// ParseParams validates the query. Keys must already be lowercase. The source
// supplies the default audio stream and says which audio indexes exist.
func ParseParams(query url.Values, source Source) (Params, error) {
	p := Params{SourceID: strings.ReplaceAll(strings.ToLower(query.Get("mediasourceid")), "-", ""), AudioIndex: source.DefaultAudio()}
	if len(p.SourceID) != 32 || !isHex(p.SourceID) {
		return p, errors.New("invalid MediaSourceId")
	}
	if raw := query.Get("audiostreamindex"); raw != "" {
		index, err := strconv.Atoi(raw)
		if err != nil {
			return p, errors.New("invalid AudioStreamIndex")
		}
		if _, ok := source.audio(index); !ok && !(index == -1 && source.DefaultAudio() == -1) {
			return p, errors.New("AudioStreamIndex is not an audio stream")
		}
		p.AudioIndex = index
	}
	var err error
	if p.VideoBitrate, err = bitrate(query, "videobitrate", Unlimited-192_000, MinVideoBitrate, Unlimited); err != nil {
		return p, err
	}
	if p.AudioBitrate, err = bitrate(query, "audiobitrate", 192_000, MinAudioBitrate, MaxAudioBitrate); err != nil {
		return p, err
	}
	if raw := query.Get("starttimeticks"); raw != "" {
		ticks, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ticks < 0 || ticks > MaxStartTicks {
			return p, errors.New("invalid StartTimeTicks")
		}
		p.StartTicks = ticks
	}
	p.PlaySessionID = query.Get("playsessionid")
	if len(p.PlaySessionID) == 0 || len(p.PlaySessionID) > 64 || !isHex(p.PlaySessionID) {
		return p, errors.New("invalid PlaySessionId")
	}
	// The device ID is free text and reaches only SQL parameters.
	if p.DeviceID = query.Get("deviceid"); len(p.DeviceID) > 1024 {
		return p, errors.New("invalid DeviceId")
	}
	return p, nil
}

func bitrate(query url.Values, key string, fallback, low, high int64) (int64, error) {
	raw := query.Get(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < low || value > high {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return value, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return s != ""
}

// Spec identifies one transcode output. Two requests with equal specs share
// segments and jobs. Bitrate and size are the values ffmpeg will use, not the
// request's, so Auto and a 120 Mbps cap share segments.
type Spec struct {
	Item         string
	Source       string
	ETag         string
	AudioIndex   int
	VideoBitrate int64
	AudioBitrate int64
	Width        int
	Height       int
}

// NewSpec turns validated parameters into the output they ask for.
func NewSpec(item string, p Params, source Source) (Spec, error) {
	video, ok := source.video()
	if !ok || video.Width < 2 || video.Height < 2 {
		return Spec{}, errors.New("source has no video to transcode")
	}
	width, height := outputSize(video, p.VideoBitrate)
	return Spec{
		Item: strings.ReplaceAll(strings.ToLower(item), "-", ""), Source: p.SourceID, ETag: source.ETag,
		AudioIndex: p.AudioIndex, AudioBitrate: p.AudioBitrate, Width: width, Height: height,
		// Six bits per pixel per second is about what 8-bit H.264 needs at its
		// worst, so a larger cap buys nothing.
		VideoBitrate: min(p.VideoBitrate, int64(width)*int64(height)*6),
	}, nil
}

// Hash names the spec in the segment store.
func (s Spec) Hash() string {
	text := fmt.Sprintf("v%d|%s|%s|%s|a%d|vb%d|ab%d|%dx%d|seg%d|%s",
		OutputVersion, s.Item, s.Source, s.ETag, s.AudioIndex, s.VideoBitrate, s.AudioBitrate, s.Width, s.Height, SegmentSeconds, Family)
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:16])
}

// outputSize shrinks the picture until the bitrate can carry it: about 0.04
// bits per pixel at 24 fps. Steps are common heights, and sizes are even.
// Nothing goes above MaxHeight, because software x264 at 4K needs more CPU
// than a worker usually has to keep up with playback.
func outputSize(video Stream, bitrate int64) (int, int) {
	budget := bitrate * 25 / 24
	heights := []int{min(video.Height&^1, MaxHeight)}
	for _, h := range []int{2160, 1440, 1080, 720, 540, 360, 240} {
		if h < heights[0] {
			heights = append(heights, h)
		}
	}
	var width, height int
	for _, h := range heights {
		width, height = (video.Width*h/video.Height)&^1, h
		if int64(width)*int64(height) <= budget {
			break
		}
	}
	return max(width, 2), height
}

// Segments is how many segments a film of this length has. A tail under a
// second joins the segment before it, because the encoder may not cut one.
func Segments(runTimeTicks int64) int {
	seconds := float64(runTimeTicks) / ticksPerSecond
	count := int(seconds / SegmentSeconds)
	if count == 0 || seconds-float64(count*SegmentSeconds) >= 1 {
		count++
	}
	return count
}

// segmentTicks returns where segment n starts and how long it is.
func segmentTicks(n, count int, runTimeTicks int64) (start, length int64) {
	start = int64(n) * SegmentSeconds * ticksPerSecond
	if n == count-1 {
		return start, runTimeTicks - start
	}
	return start, SegmentSeconds * ticksPerSecond
}

// SegmentLength is how long segment n of the playlist is.
func SegmentLength(n int, runTimeTicks int64) float64 {
	_, length := segmentTicks(n, Segments(runTimeTicks), runTimeTicks)
	return float64(length) / ticksPerSecond
}

// ParseSegment reads "-1.mp4" (the init segment, as -1) or "12.mp4" and
// rejects numbers outside the playlist.
func ParseSegment(name string, count int) (int, bool) {
	number, ok := strings.CutSuffix(name, ".mp4")
	if !ok {
		return 0, false
	}
	if number == "-1" {
		return -1, true
	}
	if number == "" || len(number) > 7 || strings.TrimLeft(number, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(number)
	return n, err == nil && n < count
}

// Query renders parameters the way Jellyfin's TranscodingUrl does, in its
// order. extra is appended as it is.
func Query(p Params, apiKey, tag, extra string) string {
	pairs := [][2]string{
		{"DeviceId", p.DeviceID}, {"MediaSourceId", p.SourceID}, {"VideoCodec", "h264"}, {"AudioCodec", "aac"},
		{"AudioStreamIndex", strconv.Itoa(p.AudioIndex)}, {"VideoBitrate", strconv.FormatInt(p.VideoBitrate, 10)},
		{"AudioBitrate", strconv.FormatInt(p.AudioBitrate, 10)}, {"AudioSampleRate", "48000"},
		{"SegmentContainer", "mp4"}, {"MinSegments", "1"}, {"PlaySessionId", p.PlaySessionID}, {"ApiKey", apiKey},
		{"TranscodingMaxAudioChannels", "2"}, {"RequireAvc", "false"}, {"EnableAudioVbrEncoding", "true"}, {"Tag", tag},
	}
	var out strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			out.WriteByte('&')
		}
		out.WriteString(pair[0] + "=" + url.QueryEscape(pair[1]))
	}
	if p.StartTicks > 0 {
		out.WriteString("&StartTimeTicks=" + strconv.FormatInt(p.StartTicks, 10))
	}
	if extra != "" {
		out.WriteString("&" + extra)
	}
	return out.String()
}

// MasterURL is the TranscodingUrl: relative, unsigned and Jellyfin-shaped.
func MasterURL(itemDashed string, p Params, apiKey, tag string, d Decision, framerate float64) string {
	extra := ""
	if framerate > 0 {
		extra = "MaxFramerate=" + strconv.FormatFloat(framerate, 'f', -1, 64)
	}
	if len(d.Reasons) > 0 {
		if extra != "" {
			extra += "&"
		}
		extra += "TranscodeReasons=" + strings.Join(d.Reasons, ",")
	}
	return "/videos/" + itemDashed + "/master.m3u8?" + Query(p, apiKey, tag, extra)
}

// Master is the one-variant master playlist.
func Master(spec Spec, query string, framerate float64) string {
	rate := ""
	if framerate > 0 {
		rate = ",FRAME-RATE=" + strconv.FormatFloat(framerate, 'f', 3, 64)
	}
	bandwidth := spec.VideoBitrate + spec.AudioBitrate
	return fmt.Sprintf("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,VIDEO-RANGE=SDR,CODECS=\"avc1.640033,mp4a.40.2\",RESOLUTION=%dx%d%s\nmain.m3u8?%s\n",
		bandwidth, bandwidth, spec.Width, spec.Height, rate, query)
}

// Main is the complete VOD playlist, known before any segment exists.
func Main(runTimeTicks int64, query string) string {
	count := Segments(runTimeTicks)
	_, last := segmentTicks(count-1, count, runTimeTicks)
	var out strings.Builder
	fmt.Fprintf(&out, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n",
		int(math.Ceil(max(float64(last)/ticksPerSecond, SegmentSeconds))))
	fmt.Fprintf(&out, "#EXT-X-MAP:URI=\"hls1/main/-1.mp4?%s&runtimeTicks=0&actualSegmentLengthTicks=0\"\n", query)
	for n := range count {
		start, length := segmentTicks(n, count, runTimeTicks)
		fmt.Fprintf(&out, "#EXTINF:%.6f, nodesc\nhls1/main/%d.mp4?%s&runtimeTicks=%d&actualSegmentLengthTicks=%d\n",
			float64(length)/ticksPerSecond, n, query, start, length)
	}
	out.WriteString("#EXT-X-ENDLIST\n")
	return out.String()
}
