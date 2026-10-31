// Package playback decides how a client plays a media source, and describes a
// transcode as a validated Spec. It does no I/O.
package playback

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Unlimited is what jellyfin-web sends as MaxStreamingBitrate for Auto.
const Unlimited = 2147483647

// Number reads a JSON number or a numeric string, because jellyfin-web sends
// stream indexes as strings on the first call and numbers on later ones.
type Number int64

func (n *Number) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		text = string(data)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return nil // Not a number: treat as unset rather than refuse the call.
	}
	*n = Number(value)
	return nil
}

// Request is the PlaybackInfo POST body, reduced to what the decision needs.
// A nil pointer means the client didn't say.
type Request struct {
	DeviceProfile       *DeviceProfile `json:"DeviceProfile"`
	MaxStreamingBitrate *Number
	AudioStreamIndex    *Number
	SubtitleStreamIndex *Number
	StartTimeTicks      *Number // Where the viewer starts; the TranscodingUrl carries it to the init segment's request.
	MediaSourceID       string  `json:"MediaSourceId"`
	// A transcode draws the chosen subtitle into the picture.
	AlwaysBurnInSubtitleWhenTranscoding bool
}

// NoSubtitleProfiles is true when the client says it can show no subtitle
// itself, as jellyfin-web does when the viewer's setting is to burn them all.
func (r Request) NoSubtitleProfiles() bool {
	return r.DeviceProfile != nil && len(r.DeviceProfile.SubtitleProfiles) == 0
}

// DeviceProfile is what a client says it can play.
type DeviceProfile struct {
	MaxStreamingBitrate Number
	DirectPlayProfiles  []DirectPlayProfile
	TranscodingProfiles []TranscodingProfile
	CodecProfiles       []CodecProfile
	SubtitleProfiles    []struct{} // Only whether there are any matters.
}

// DirectPlayProfile lists containers and codecs the client plays as they are.
// An empty codec list means any.
type DirectPlayProfile struct {
	Container  string
	Type       string
	VideoCodec string
	AudioCodec string
}

// TranscodingProfile is an output the client accepts.
type TranscodingProfile struct {
	Container  string
	Type       string
	VideoCodec string
	AudioCodec string
	Protocol   string
}

// CodecProfile limits a codec the client otherwise plays.
type CodecProfile struct {
	Type       string
	Codec      string
	Conditions []Condition
}

// Condition is one limit, such as VideoProfile EqualsAny "high|main".
type Condition struct {
	Condition  string
	Property   string
	Value      string
	IsRequired bool
}

// ParseRequest reads a PlaybackInfo body. An empty body is a request with no
// profile, which stays direct play.
func ParseRequest(body []byte) (Request, error) {
	var request Request
	if len(strings.TrimSpace(string(body))) == 0 {
		return request, nil
	}
	err := json.Unmarshal(body, &request)
	return request, err
}

// Stream is one probed stream, as the scanner stores it.
type Stream struct {
	Index        int
	Type         string
	Codec        string
	Profile      string
	Level        int
	IsInterlaced bool
	// VideoRangeType is SDR, HDR10, HLG or DOVI. It is empty on a source
	// scanned before the scanner recorded it; the level, field order and range
	// then pass their conditions, so direct play continues until a rescan.
	VideoRangeType   string
	Width, Height    int
	Channels         int
	IsDefault        bool
	AverageFrameRate float64
	// DurationTicks is how long the video stream runs; 0 if the probe didn't
	// say.
	DurationTicks int64
	// IsTextSubtitleStream is true for a subtitle track that can be extracted
	// as text.
	IsTextSubtitleStream bool
}

// Source is what the decision knows about a file.
type Source struct {
	ID, ETag     string
	Container    string
	Bitrate      int64
	RunTimeTicks int64
	Streams      []Stream
}

// Ticks is how long a transcode of this source runs: to the end of the video,
// which can come before the end of the audio and so before RunTimeTicks.
func (s Source) Ticks() int64 {
	video, _ := s.video()
	if video.DurationTicks > 0 && video.DurationTicks < s.RunTimeTicks {
		return video.DurationTicks
	}
	return s.RunTimeTicks
}

// ParseStreams reads media_sources.streams.
func ParseStreams(raw []byte) ([]Stream, error) {
	var streams []Stream
	err := json.Unmarshal(raw, &streams)
	return streams, err
}

// FrameRate is the video's average frame rate, or 0 if unknown.
func (s Source) FrameRate() float64 {
	video, _ := s.video()
	return video.AverageFrameRate
}

func (s Source) video() (Stream, bool) {
	for _, stream := range s.Streams {
		if stream.Type == "Video" {
			return stream, true
		}
	}
	return Stream{}, false
}

// DefaultAudio is the flagged audio stream, else the first, which is the one
// a browser plays from a file. It returns -1 when there is no audio.
func (s Source) DefaultAudio() int {
	first := -1
	for _, stream := range s.Streams {
		if stream.Type != "Audio" {
			continue
		}
		if stream.IsDefault {
			return stream.Index
		}
		if first == -1 {
			first = stream.Index
		}
	}
	return first
}

func (s Source) audio(index int) (Stream, bool) {
	for _, stream := range s.Streams {
		if stream.Type == "Audio" && stream.Index == index {
			return stream, true
		}
	}
	return Stream{}, false
}

// TextSubtitle reports whether index is a subtitle track we can deliver.
func (s Source) TextSubtitle(index int) bool {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index {
			return stream.IsTextSubtitleStream
		}
	}
	return false
}

// Decision is how to play one source.
type Decision struct {
	DirectPlay bool
	// Reasons are Jellyfin's TranscodeReasons names. A transcode forced only by
	// the audio track choice has none.
	Reasons    []string
	AudioIndex int
	// Subtitle is the text track to draw into the picture, or nil.
	Subtitle *int
	// The split of the bitrate cap, set when transcoding.
	VideoBitrate, AudioBitrate int64
}

// Decide chooses direct play or transcode. canTranscode is false when this
// server can't transcode this source, and then direct play is all that's left.
func Decide(request Request, source Source, canTranscode bool) Decision {
	audio := source.DefaultAudio()
	if request.AudioStreamIndex != nil {
		if _, ok := source.audio(int(*request.AudioStreamIndex)); ok {
			audio = int(*request.AudioStreamIndex)
		}
	}
	direct := Decision{DirectPlay: true, AudioIndex: audio}
	profile := request.DeviceProfile
	video, hasVideo := source.video()
	if profile == nil || !hasVideo || !canTranscode || !profile.canTranscode() {
		return direct
	}

	limit := int64(profile.MaxStreamingBitrate)
	if request.MaxStreamingBitrate != nil && *request.MaxStreamingBitrate > 0 {
		limit = int64(*request.MaxStreamingBitrate)
	}
	reasons := profile.reasons(source, video, audio)
	if limit > 0 && source.Bitrate > limit {
		reasons = append(reasons, "ContainerBitrateExceedsLimit")
	}
	// A client with no subtitle profile can't show a track, so one that is
	// chosen is drawn into the picture, which takes a transcode. A client that
	// does show them is burnt into only when it transcodes anyway and the
	// viewer asked for it.
	var chosen *int
	if request.SubtitleStreamIndex != nil && source.TextSubtitle(int(*request.SubtitleStreamIndex)) {
		index := int(*request.SubtitleStreamIndex)
		chosen = &index
		if request.NoSubtitleProfiles() {
			reasons = append(reasons, "SubtitleCodecNotSupported")
		}
	}
	// Direct play can't pick a track, so any other than the default transcodes.
	if len(reasons) == 0 && audio == source.DefaultAudio() {
		return direct
	}
	if limit <= 0 {
		limit = Unlimited
	}
	videoBitrate, audioBitrate := SplitBitrate(limit)
	decision := Decision{Reasons: reasons, AudioIndex: audio, VideoBitrate: videoBitrate, AudioBitrate: audioBitrate}
	if chosen != nil && (request.NoSubtitleProfiles() || request.AlwaysBurnInSubtitleWhenTranscoding) {
		decision.Subtitle = chosen
		if !slices.Contains(reasons, "SubtitleCodecNotSupported") {
			decision.Reasons = append(reasons, "SubtitleCodecNotSupported")
		}
	}
	return decision
}

// SplitBitrate divides the cap between video and audio so they sum to it, as
// Jellyfin's URLs do. A cap too small for the floors goes over rather than
// produce unusable streams.
func SplitBitrate(limit int64) (video, audio int64) {
	audio = min(max(limit/5, MinAudioBitrate), 192_000)
	return max(limit-audio, MinVideoBitrate), audio
}

// canTranscode reports whether the client accepts what this server produces:
// fMP4 HLS carrying H.264 and AAC.
func (p DeviceProfile) canTranscode() bool {
	for _, t := range p.TranscodingProfiles {
		if strings.EqualFold(t.Type, "Video") && strings.EqualFold(t.Protocol, "hls") &&
			strings.EqualFold(t.Container, "mp4") && listHas(t.VideoCodec, "h264") && listHas(t.AudioCodec, "aac") {
			return true
		}
	}
	return false
}

// reasons names what stops the client playing the file as it is, apart from
// the bitrate cap.
func (p DeviceProfile) reasons(source Source, video Stream, audio int) []string {
	audioStream, hasAudio := source.audio(audio)
	var matching []DirectPlayProfile
	for _, d := range p.DirectPlayProfiles {
		if strings.EqualFold(d.Type, "Video") && listHas(d.Container, source.Container) {
			matching = append(matching, d)
		}
	}
	if len(matching) == 0 {
		return []string{"ContainerNotSupported"}
	}
	videoOK := slices.ContainsFunc(matching, func(d DirectPlayProfile) bool { return listHas(d.VideoCodec, video.Codec) })
	audioOK := !hasAudio || slices.ContainsFunc(matching, func(d DirectPlayProfile) bool { return listHas(d.AudioCodec, audioStream.Codec) })
	var reasons []string
	if !videoOK {
		reasons = append(reasons, "VideoCodecNotSupported")
	}
	if !audioOK {
		reasons = append(reasons, "AudioCodecNotSupported")
	}
	if len(reasons) == 0 && !slices.ContainsFunc(matching, func(d DirectPlayProfile) bool {
		return listHas(d.VideoCodec, video.Codec) && (!hasAudio || listHas(d.AudioCodec, audioStream.Codec))
	}) {
		reasons = append(reasons, "ContainerNotSupported") // Each codec plays, but not together.
	}
	if videoOK {
		reasons = append(reasons, p.limits("Video", video.Codec, video, audioStream)...)
	}
	if hasAudio && audioOK {
		reasons = append(reasons, p.limits("VideoAudio", audioStream.Codec, video, audioStream)...)
	}
	return reasons
}

// limits checks the CodecProfiles that apply to a codec. Only properties the
// scanner records are checked; the rest pass, as an unknown value does when
// the condition isn't required.
func (p DeviceProfile) limits(kind, codec string, video, audio Stream) []string {
	var reasons []string
	for _, profile := range p.CodecProfiles {
		if !strings.EqualFold(profile.Type, kind) || (profile.Codec != "" && !listHas(profile.Codec, codec)) {
			continue
		}
		for _, c := range profile.Conditions {
			var value string
			var reason string
			switch strings.ToLower(c.Property) {
			case "videolevel":
				value, reason = positive(video.Level), "VideoLevelNotSupported"
			case "isinterlaced":
				value, reason = strconv.FormatBool(video.IsInterlaced), "InterlacedVideoNotSupported"
			case "videorangetype":
				value, reason = strings.ToLower(video.VideoRangeType), "VideoRangeTypeNotSupported"
			case "videoprofile":
				value, reason = strings.ToLower(video.Profile), "VideoProfileNotSupported"
			case "width":
				value, reason = positive(video.Width), "VideoResolutionNotSupported"
			case "height":
				value, reason = positive(video.Height), "VideoResolutionNotSupported"
			case "audiochannels":
				value, reason = positive(audio.Channels), "AudioChannelsNotSupported"
			default:
				continue
			}
			if value == "" && !c.IsRequired {
				continue
			}
			if !c.holds(value) && !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
	}
	return reasons
}

func positive(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// holds evaluates the condition against a value. Values that must be numbers
// but aren't fail.
func (c Condition) holds(value string) bool {
	want := strings.ToLower(c.Value)
	switch strings.ToLower(c.Condition) {
	case "equals":
		return value == want
	case "notequals":
		return value != want
	case "equalsany":
		return slices.Contains(strings.Split(want, "|"), value)
	case "lessthanequal", "greaterthanequal":
		have, err1 := strconv.ParseFloat(value, 64)
		limit, err2 := strconv.ParseFloat(want, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if strings.EqualFold(c.Condition, "lessthanequal") {
			return have <= limit
		}
		return have >= limit
	}
	return true // A condition we don't know can't be held against the file.
}

// listHas reports whether a comma-separated list names item, ignoring case.
// An empty list names everything, as profiles use it.
func listHas(list, item string) bool {
	if strings.TrimSpace(list) == "" {
		return true
	}
	for _, entry := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(entry), item) {
			return true
		}
	}
	return false
}
