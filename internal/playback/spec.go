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
	OutputVersion  = 4
	SegmentSeconds = 4
	// Family is the encoder family; fMP4 can't splice across families.
	Family = "x264"

	// MaxHeight caps the picture a transcode produces.
	MaxHeight = 1080

	MinVideoBitrate = 50_000
	MinAudioBitrate = 32_000
	// MaxAudioBitrate caps what the AAC encoder is asked for.
	MaxAudioBitrate = 640_000
	// maxCopiedAudioBitrate is the most a URL may say a copied track takes:
	// lossless 5.1 at high rates, with room.
	maxCopiedAudioBitrate = 12_000_000

	ticksPerSecond = 10_000_000
	// MaxStartTicks is a century, far past any film.
	MaxStartTicks = 100 * 365 * 86400 * ticksPerSecond
)

// Params are the transcode inputs a client controls, taken from the URL query.
// They're the only client values a Spec is built from, and every request
// parses them again, because clients can edit the URL.
type Params struct {
	SourceID     string // compact hex
	AudioIndex   int    // -1 for none; set from the source when the URL omits it
	Subtitle     *int   // The text track drawn into the picture; nil for none
	VideoBitrate int64
	AudioBitrate int64
	// AudioCodecs are the codecs the client takes in fMP4 HLS, from the
	// matching TranscodingProfile (the URL's AudioCodec), and MaxAudioChannels
	// its channel limit (TranscodingMaxAudioChannels). They decide whether the
	// audio is copied (Source.copyAudio); a URL without them copies nothing.
	AudioCodecs      []string
	MaxAudioChannels int
	// Copy: the video is copied from the source, not re-encoded. A client may
	// ask for it, and the spec refuses it for a source that can't be copied.
	Copy          bool
	PlaySessionID string
	DeviceID      string
	// StartTicks is where the viewer starts, from PlaybackInfo's
	// StartTimeTicks. It is no part of the spec: it only says where a job
	// starts when the init segment, which comes first, is asked for.
	StartTicks int64
}

// StartSegment is the segment the viewer starts at.
func (p Params) StartSegment(layout Layout) int { return layout.At(p.StartTicks) }

// ParseParams validates the query. Keys must already be lowercase. The source
// supplies the default audio stream and says which audio indexes exist.
func ParseParams(query url.Values, source Source) (Params, error) {
	p := Params{SourceID: strings.ReplaceAll(strings.ToLower(query.Get("mediasourceid")), "-", ""), AudioIndex: source.DefaultAudio()}
	if len(p.SourceID) != 32 || !isHex(p.SourceID) {
		return p, errors.New("invalid MediaSourceId")
	}
	if raw := query.Get("subtitlestreamindex"); raw != "" && raw != "-1" {
		index, err := strconv.Atoi(raw)
		switch {
		case err == nil && source.Unreadable(index):
			// A file a worker found it can't read, after this URL was made. The
			// film plays without it, as it would for a client that asks again,
			// rather than failing every segment for the rest of the session.
		case err != nil || !source.Burnable(index):
			return p, errors.New("SubtitleStreamIndex is not a subtitle stream that can be drawn")
		default:
			p.Subtitle = &index
		}
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
	if p.AudioBitrate, err = bitrate(query, "audiobitrate", 192_000, MinAudioBitrate, maxCopiedAudioBitrate); err != nil {
		return p, err
	}
	p.AudioCodecs = AudioCodecList(query.Get("audiocodec"))
	p.MaxAudioChannels = channels(query.Get("transcodingmaxaudiochannels"))
	if raw := query.Get("starttimeticks"); raw != "" {
		ticks, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ticks < 0 || ticks > MaxStartTicks {
			return p, errors.New("invalid StartTimeTicks")
		}
		p.StartTicks = ticks
	}
	p.Copy = strings.EqualFold(query.Get("videocodec"), "copy")
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

// channels reads the channel limit; a missing, non-numeric or out-of-range
// value is the default of 2, not a reason to refuse the stream.
func channels(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 8 {
		return 2
	}
	return n
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
// segments and jobs. A burnt-in subtitle changes the picture, so it joins the
// hash; the field is a pointer so a spec stored before it existed reads as
// having none. Bitrate and size are the values ffmpeg will use, not the
// request's, so Auto and a 120 Mbps cap share segments. A copied stream has the
// source's own size, and a bitrate estimated from its file, none of which
// change the output; Copy does, so the two kinds never share segments.
type Spec struct {
	Item       string
	Source     string
	ETag       string
	AudioIndex int
	Subtitle   *int // Drawn into the picture; nil for none
	// SubtitleFingerprint is the fingerprint of the subtitle file when the track is one
	// beside the film, so an edited file is drawn anew.
	SubtitleFingerprint string
	VideoBitrate        int64
	AudioBitrate        int64
	Width               int
	Height              int
	Copy                bool
	// ToneMap is the transfer function (PQ or HLG) of an HDR source that the
	// encode maps to SDR BT.709, or "" for a picture left as it is. It comes from
	// the source and never from the URL. A copy has none: it sends the picture
	// as it is.
	ToneMap string
	// AudioCopy is the codec of the audio when it is copied, not re-encoded,
	// and "" when it is re-encoded to AAC. It is the codec's name so AC3 and
	// E-AC3 never share segments. A copied video always has the audio
	// re-encoded (CopyCommand), so Copy and AudioCopy are never both set.
	AudioCopy string
}

// NewSpec turns validated parameters into the output they ask for.
func NewSpec(item string, p Params, source Source) (Spec, error) {
	video, ok := source.video()
	if !ok || video.Width < 2 || video.Height < 2 {
		return Spec{}, errors.New("source has no video to transcode")
	}
	if p.Copy {
		if !source.canServeCopy() {
			return Spec{}, errors.New("source can't be copied")
		}
		if p.Subtitle != nil {
			return Spec{}, errors.New("a copied video can't have a subtitle burnt into it")
		}
		return Spec{
			Item: strings.ReplaceAll(strings.ToLower(item), "-", ""), Source: p.SourceID, ETag: source.ETag,
			AudioIndex: p.AudioIndex, AudioBitrate: min(p.AudioBitrate, MaxAudioBitrate), Width: video.Width, Height: video.Height,
			VideoBitrate: source.FileBitrate(), Copy: true,
		}, nil
	}
	width, height := outputSize(video, p.VideoBitrate)
	subtitleFingerprint := ""
	if p.Subtitle != nil {
		subtitleFingerprint = source.SubtitleFingerprint(*p.Subtitle)
	}
	// A copied track has the source's own bitrate (0 if the file doesn't say),
	// whatever the URL says, so a change of cap that leaves the track alone
	// doesn't make new segments.
	audioBitrate, audioCopy := min(p.AudioBitrate, MaxAudioBitrate), ""
	if stream, ok := source.copyAudio(p); ok {
		audioBitrate, audioCopy = stream.Bitrate, stream.Codec
	}
	return Spec{
		Item: strings.ReplaceAll(strings.ToLower(item), "-", ""), Source: p.SourceID, ETag: source.ETag,
		AudioIndex: p.AudioIndex, Subtitle: p.Subtitle, SubtitleFingerprint: subtitleFingerprint, AudioBitrate: audioBitrate, AudioCopy: audioCopy, Width: width, Height: height,
		// Six bits per pixel per second is about what 8-bit H.264 needs at its
		// worst, so a larger cap buys nothing.
		VideoBitrate: min(p.VideoBitrate, int64(width)*int64(height)*6),
		ToneMap:      source.ToneMap(),
	}, nil
}

// Hash names the spec in the segment store.
func (s Spec) Hash() string {
	text := fmt.Sprintf("v%d|%s|%s|%s|a%d|vb%d|ab%d|%dx%d|seg%d|%s",
		OutputVersion, s.Item, s.Source, s.ETag, s.AudioIndex, s.VideoBitrate, s.AudioBitrate, s.Width, s.Height, SegmentSeconds, Family)
	if s.Subtitle != nil {
		text += fmt.Sprintf("|sub%d", *s.Subtitle)
		// Only a track beside the film has one, so the hash of a track inside
		// it is what it was before these existed.
		if s.SubtitleFingerprint != "" {
			text += "|subfile" + s.SubtitleFingerprint
		}
	}
	if s.Copy {
		text += "|copy"
	}
	// Only an HDR source has one, so the hash of an SDR film is what it was.
	if s.ToneMap != "" {
		text += "|tonemap" + s.ToneMap
	}
	// Only a copy has one, so the hash of a re-encoded track is what it was.
	if s.AudioCopy != "" {
		text += "|acopy" + s.AudioCopy
	}
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

func videoCodec(p Params) string {
	if p.Copy {
		return "copy"
	}
	return "h264"
}

// Query renders parameters the way Jellyfin's TranscodingUrl does, in its
// order. extra is appended as it is.
func Query(p Params, apiKey, tag, extra string) string {
	pairs := [][2]string{
		{"DeviceId", p.DeviceID}, {"MediaSourceId", p.SourceID}, {"VideoCodec", videoCodec(p)}, {"AudioCodec", strings.Join(p.AudioCodecs, ",")},
		{"AudioStreamIndex", strconv.Itoa(p.AudioIndex)},
	}
	if p.Subtitle != nil {
		pairs = append(pairs, [2]string{"SubtitleStreamIndex", strconv.Itoa(*p.Subtitle)})
	}
	pairs = append(pairs, [][2]string{
		{"VideoBitrate", strconv.FormatInt(p.VideoBitrate, 10)},
		{"AudioBitrate", strconv.FormatInt(p.AudioBitrate, 10)}, {"AudioSampleRate", "48000"},
		{"SegmentContainer", "mp4"}, {"MinSegments", "1"}, {"PlaySessionId", p.PlaySessionID}, {"ApiKey", apiKey},
		{"TranscodingMaxAudioChannels", strconv.Itoa(max(p.MaxAudioChannels, 1))}, {"RequireAvc", "false"}, {"EnableAudioVbrEncoding", "true"}, {"Tag", tag},
	}...)
	var out strings.Builder
	for i, pair := range pairs {
		if i > 0 {
			out.WriteByte('&')
		}
		// Jellyfin writes the codec list with plain commas.
		out.WriteString(pair[0] + "=" + strings.ReplaceAll(url.QueryEscape(pair[1]), "%2C", ","))
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
func Master(spec Spec, source Source, query string) string {
	rate := ""
	if framerate := source.FrameRate(); framerate > 0 {
		rate = ",FRAME-RATE=" + strconv.FormatFloat(framerate, 'f', 3, 64)
	}
	codecs, audio, videoRange, supplemental := "avc1.640033", "mp4a.40.2", "SDR", ""
	if spec.Copy {
		codecs = source.codecs()
		videoRange, supplemental = source.copiedRange()
	}
	if supplemental != "" {
		supplemental = fmt.Sprintf(",SUPPLEMENTAL-CODECS=\"%s\"", supplemental)
	}
	if spec.AudioCopy != "" {
		stream, _ := source.audio(spec.AudioIndex)
		audio = audioCodecString(stream)
	}
	// A copied track whose file doesn't say its bitrate counts as Decide counted it.
	audioBitrate := spec.AudioBitrate
	if spec.AudioCopy != "" && audioBitrate <= 0 {
		audioBitrate = unknownAudioBitrate
	}
	bandwidth := spec.VideoBitrate + audioBitrate
	return fmt.Sprintf("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,VIDEO-RANGE=%s,CODECS=\"%s,%s\"%s,RESOLUTION=%dx%d%s\nmain.m3u8?%s\n",
		bandwidth, bandwidth, videoRange, codecs, audio, supplemental, spec.Width, spec.Height, rate, query)
}

// Main is the complete VOD playlist, known before any segment exists.
func Main(layout Layout, query string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n",
		int(math.Ceil(max(float64(layout.longest())/ticksPerSecond, SegmentSeconds))))
	fmt.Fprintf(&out, "#EXT-X-MAP:URI=\"hls1/main/-1.mp4?%s&runtimeTicks=0&actualSegmentLengthTicks=0\"\n", query)
	for n := range layout.Count() {
		start, length := layout.Segment(n)
		fmt.Fprintf(&out, "#EXTINF:%.6f, nodesc\nhls1/main/%d.mp4?%s&runtimeTicks=%d&actualSegmentLengthTicks=%d\n",
			float64(length)/ticksPerSecond, n, query, start, length)
	}
	out.WriteString("#EXT-X-ENDLIST\n")
	return out.String()
}
