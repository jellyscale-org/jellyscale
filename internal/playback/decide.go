// Package playback decides how a client plays a media source, and describes a
// transcode as a validated Spec. It does no I/O.
package playback

import (
	"encoding/json"
	"fmt"
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

// ShowsPgs is true when the client renders PGS itself from the file, as
// jellyfin-web does once its viewer has turned that on.
func (r Request) ShowsPgs() bool {
	return r.DeviceProfile != nil && slices.ContainsFunc(r.DeviceProfile.SubtitleProfiles, func(p SubtitleProfile) bool {
		return strings.EqualFold(p.Format, "pgssub") && strings.EqualFold(p.Method, "External")
	})
}

// DeviceProfile is what a client says it can play.
type DeviceProfile struct {
	MaxStreamingBitrate Number
	DirectPlayProfiles  []DirectPlayProfile
	TranscodingProfiles []TranscodingProfile
	CodecProfiles       []CodecProfile
	SubtitleProfiles    []SubtitleProfile
}

// SubtitleProfile is a subtitle format the client shows itself.
type SubtitleProfile struct {
	Format string
	Method string
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
	// MaxAudioChannels is a string in Jellyfin's profiles ("2"); 0 if absent.
	MaxAudioChannels Number
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
	// VideoRangeType is Jellyfin's: SDR, HDR10, HDR10Plus, HLG, DOVI (profile 5)
	// or one of the DOVIWith... types that name the base layer of the other
	// Dolby Vision profiles. It is empty on a source scanned before the scanner
	// recorded it; the level, field order and range then pass their conditions,
	// so direct play continues until a rescan.
	VideoRangeType string
	// ColorTransfer is ffprobe's name for the transfer function: smpte2084 for
	// PQ, arib-std-b67 for HLG. It is empty when the file doesn't say, and on a
	// source scanned before it was recorded, which is never tone mapped.
	ColorTransfer string
	// DvProfile, DvLevel and DvBlSignalCompatibilityId are the Dolby Vision
	// configuration, which name the stream in the master playlist.
	DvProfile, DvLevel, DvBlSignalCompatibilityId int
	// BitDepth is 8, 10 or 12, or 0 on a source scanned before it was recorded.
	BitDepth      int
	Width, Height int
	Channels      int
	// Bitrate is the stream's own bits per second, or 0 when the file doesn't
	// say. A copied track is counted against the bitrate cap by it.
	Bitrate          int64 `json:"BitRate"`
	IsDefault        bool
	AverageFrameRate float64
	// DurationTicks is how long the video stream runs; 0 if the probe didn't
	// say.
	DurationTicks int64
	// IsTextSubtitleStream is true for a subtitle track that can be extracted
	// as text.
	IsTextSubtitleStream bool
	// IsImageSubtitleStream is true for a bitmap track that can be drawn into a
	// transcode (PGS, VobSub, DVB), and so is listed.
	IsImageSubtitleStream bool
	// Fingerprint identifies the version of the subtitle file beside the film
	// that the track is; empty for a track inside the film.
	Fingerprint string
	// Path is the subtitle file's key in the media store, and SubPath the .sub
	// of a VobSub pair whose .idx Path is.
	Path, SubPath string
	// File is where a worker put a copy of the external image track to read it
	// from. It is never stored.
	File string `json:"-"`
	// Unreadable is a subtitle file a worker found it cannot read, which stays
	// so until the file changes.
	Unreadable bool
}

// Source is what the decision knows about a file.
type Source struct {
	ID, ETag     string
	Container    string
	Bitrate      int64
	Size         int64
	RunTimeTicks int64
	Streams      []Stream
	// Keyframes are the video's keyframe times in ticks, ascending, when the
	// caller loaded them. They are what a copied stream is cut on.
	Keyframes []int64
	// VideoEnd is where the last video frame ends, on the clock of Keyframes.
	VideoEnd int64
}

// Layout is how this source is cut into segments.
func (s Source) Layout(copied bool) Layout {
	if copied {
		layout, _ := s.copyLayout()
		return layout
	}
	return EncodedLayout(s.Ticks())
}

// copyLayout cuts the video, which lasts from its first keyframe to its end.
func (s Source) copyLayout() (Layout, bool) {
	if len(s.Keyframes) == 0 {
		return Layout{}, false
	}
	return CopyLayout(s.Keyframes, s.VideoEnd-s.Keyframes[0])
}

// copyCodecs are the video codecs that are copied into fMP4 HLS, and so the
// ones the scan reads a keyframe index for.
var copyCodecs = []string{"h264", "hevc", "av1"}

// CopyCodecs lists the codecs CopyCodec accepts, for a query that selects
// the films a scan reads an index for.
func CopyCodecs() []string { return slices.Clone(copyCodecs) }

// CopyCodec reports whether a video codec (ffprobe's name) can be copied.
func CopyCodec(name string) bool { return slices.Contains(copyCodecs, name) }

// Copyable reports whether the video is in a codec that is copied.
func (s Source) Copyable() bool {
	video, ok := s.video()
	return ok && CopyCodec(video.Codec)
}

// VideoCodec is the codec of the video, or "".
func (s Source) VideoCodec() string {
	video, _ := s.video()
	return video.Codec
}

// CanCopy reports whether Decide may offer the video as it is: a copyable
// stream (canServeCopy) whose picture is SDR, HDR10, HDR10+ or HLG, or a Dolby
// Vision file whose base layer is a picture of its own. Whether the client can
// show the range is the profile's say (Decide). A source scanned before the
// range was recorded is re-encoded until a rescan fills it in, and so is
// H.264 in HDR, which the api never offered a copy of.
func (s Source) CanCopy() bool {
	video, _ := s.video()
	_, dolbyVision := video.baseLayer()
	return s.canServeCopy() && (video.VideoRangeType == "SDR" || dolbyVision || (video.Codec != "h264" && plainHDR(video.VideoRangeType)))
}

// plainHDR is a range type of HDR that is no Dolby Vision.
func plainHDR(rangeType string) bool {
	return rangeType == "HDR10" || rangeType == "HDR10Plus" || rangeType == "HLG"
}

// canServeCopy is what a stream replica honours when a master URL asks for a
// copy: a codec that is copied, keyframes that make a playlist, and a picture
// whose range the master playlist can name: SDR, HDR10, HDR10+, HLG or a Dolby
// Vision base layer. H.264 passes in any range, so an older api replica's copy
// of an HDR film keeps playing during an upgrade. A stream replica of the
// previous release refuses plain HDR, so a rolling update replaces the stream
// replicas first (docs/running.md).
func (s Source) canServeCopy() bool {
	video, _ := s.video()
	_, cut := s.copyLayout()
	if !s.Copyable() || !cut || s.codecs() == "" {
		return false
	}
	_, dolbyVision := video.baseLayer()
	return video.Codec == "h264" || video.VideoRangeType == "SDR" || dolbyVision || plainHDR(video.VideoRangeType)
}

// baseLayer is the range type of the picture a copy of a Dolby Vision file
// sends: its base layer, which a client without Dolby Vision plays. It is false
// for any other file, and for profile 5, whose base layer is no picture.
func (v Stream) baseLayer() (string, bool) {
	switch v.VideoRangeType {
	case "DOVIWithHDR10", "DOVIWithEL":
		return "HDR10", true
	case "DOVIWithHDR10Plus", "DOVIWithELHDR10Plus":
		return "HDR10Plus", true
	case "DOVIWithHLG":
		return "HLG", true
	case "DOVIWithSDR":
		return "SDR", true
	case "DOVIInvalid":
		switch v.ColorTransfer {
		case PQ:
			return "HDR10", true
		case HLG:
			return "HLG", true
		}
		return "SDR", true
	}
	return "", false
}

// copiedRange is what the master playlist says of a copy: its VIDEO-RANGE and,
// for Dolby Vision, the SUPPLEMENTAL-CODECS that name the stream a client with
// Dolby Vision may play it as, as Jellyfin writes them.
func (s Source) copiedRange() (videoRange, supplemental string) {
	video, _ := s.video()
	base, dolbyVision := video.baseLayer()
	if !dolbyVision {
		base = video.VideoRangeType
	}
	videoRange = map[string]string{"HDR10": "PQ", "HDR10Plus": "PQ", "HLG": "HLG"}[base]
	if videoRange == "" {
		videoRange = "SDR"
	}
	if !dolbyVision {
		if base == "HDR10Plus" {
			supplemental = s.codecs() + "/cdm4"
		}
		return videoRange, supplemental
	}
	brand := map[int]string{1: "db1p", 4: "db4h"}[video.DvBlSignalCompatibilityId]
	switch {
	case video.VideoRangeType == "DOVIWithELHDR10Plus":
		supplemental = s.codecs() + "/cdm4"
	case brand != "" && strings.HasPrefix(video.VideoRangeType, "DOVIWith") && video.DvProfile != 7:
		supplemental = fmt.Sprintf("%s.%02d.%02d/%s", map[string]string{"hevc": "dvh1", "av1": "dav1"}[video.Codec], video.DvProfile, video.DvLevel, brand)
	}
	return videoRange, supplemental
}

// Transfer functions ToneMap reports, which are also what the filter is given.
const (
	PQ  = "smpte2084"
	HLG = "arib-std-b67"
)

// ToneMap is the transfer function of an HDR picture that a re-encode maps to
// SDR, or "" for a picture that is left as it is. It follows the transfer the
// file declares and not the range label alone: a Dolby Vision profile 5 file
// has a base layer that is neither HDR10 nor HLG, and mapping it as PQ gives
// green and purple. A profile 8.4 file has an HLG base and is mapped as HLG.
func (s Source) ToneMap() string {
	video, _ := s.video()
	if video.VideoRangeType == "DOVI" {
		return ""
	}
	if video.ColorTransfer == PQ || video.ColorTransfer == HLG {
		return video.ColorTransfer
	}
	return ""
}

// ToneMapFilter is the filter chain, for playback and chapter pictures, that turns a PQ or HLG picture into SDR BT.709,
// in software, with filters every ffmpeg build has (zscale needs libzimg,
// tonemap is ffmpeg's own). libplacebo would be faster but needs a Vulkan
// device, which a worker pod may not have. The input is declared BT.2020 as
// HDR10 and HLG are, since zscale refuses a file that leaves its primaries
// unsaid. hable keeps no state between frames, so a restarted run makes the
// frames an earlier one did; desat=0 and peak left to the frame's own side data
// do the same. npl=100 puts SDR white at 100 nits, and keeps an HLG picture's
// levels as they are (compat/captures/hdr-tonemap-12.4.md).
func ToneMapFilter(transfer string) string {
	return "zscale=tin=" + transfer + ":min=bt2020nc:pin=bt2020:rin=tv:t=linear:m=bt2020nc:p=bt2020:npl=100," +
		"format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"
}

// FileBitrate is the file's average bitrate, audio included, which is close
// enough for a copied stream's BANDWIDTH.
func (s Source) FileBitrate() int64 {
	if ticks := s.Ticks(); ticks > 0 {
		return int64(float64(s.Size) * 8 * ticksPerSecond / float64(ticks))
	}
	return 0
}

// codecs is the RFC 6381 name of the video's codec, profile and level, which
// players use to tell whether they can decode it. It is empty for a profile
// we can't name from what ffprobe says: HEVC other than Main and Main 10, and
// AV1 other than profile 0 at 8 or 10 bits. Those are re-encoded.
func (s Source) codecs() string {
	video, _ := s.video()
	level := video.Level
	profile := strings.ToLower(video.Profile)
	switch video.Codec {
	case "h264":
		code, ok := map[string]string{"constrained baseline": "42E0", "baseline": "4200", "main": "4D00", "high": "6400",
			"high 10": "6E00", "high 4:2:2": "7A00", "high 4:4:4 predictive": "F400"}[profile]
		if !ok {
			code = "6400"
		}
		if level <= 0 {
			level = 51
		}
		return fmt.Sprintf("avc1.%s%02X", code, level)
	case "hevc":
		// Main and Main 10 as ffmpeg writes them: profile, compatibility flags,
		// level (ffprobe's number is the stream's general_level_idc) and no
		// constraint flags. The track carries the hvc1 tag (CopyCommand).
		code, ok := map[string]string{"main": "1.6", "main 10": "2.4"}[profile]
		if !ok || level <= 0 {
			return ""
		}
		return fmt.Sprintf("hvc1.%s.L%d.B0", code, level)
	case "av1":
		// Main tier; ffprobe does not report the tier.
		if profile != "main" || level < 0 || (video.BitDepth != 8 && video.BitDepth != 10) {
			return ""
		}
		return fmt.Sprintf("av01.0.%02dM.%02d", level, video.BitDepth)
	}
	return ""
}

// audioCodecString is the RFC 6381 name of an audio stream's codec in fMP4,
// which players use to tell whether they can decode it, or "" for a stream that
// is not copied: a codec we have no name for, or an AAC profile other than LC
// and the two HE ones.
func audioCodecString(audio Stream) string {
	switch audio.Codec {
	case "aac":
		return map[string]string{"lc": "mp4a.40.2", "he-aac": "mp4a.40.5", "he-aacv2": "mp4a.40.29"}[strings.ToLower(audio.Profile)]
	case "mp3":
		return "mp4a.6B"
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	case "opus":
		return "Opus"
	case "flac":
		return "fLaC"
	}
	return ""
}

// AudioCodecList keeps the entries of a comma-separated codec list that can be
// copied, lower case and in order. A client's URL is the list's source, so
// nothing else gets through.
func AudioCodecList(list string) []string {
	var codecs []string
	for _, entry := range strings.Split(strings.ToLower(list), ",") {
		entry = strings.TrimSpace(entry)
		if audioCodecString(Stream{Codec: entry, Profile: "LC"}) != "" && !slices.Contains(codecs, entry) {
			codecs = append(codecs, entry)
		}
	}
	return codecs
}

// unknownAudioBitrate is what a track whose file doesn't say its bitrate is
// counted as: about the most a lossless 5.1 track takes. It is copied only
// under a cap this size times four, so at Auto and under no realistic quality
// menu choice.
const unknownAudioBitrate = 6_000_000

// copyAudio says whether a re-encode copies the audio the URL selects, and
// returns that stream. The decision is made from the URL's own values and the
// source, so every request for a segment reaches the same answer. The track is
// copied when the client takes its codec (AudioCodecs), it has no more channels
// than the client's limit, ffmpeg can name it in the master playlist, and it
// takes no more than a quarter of the bitrate cap and than the URL can say (maxCopiedAudioBitrate), which is the sum of the
// URL's video and audio bitrates. A copied video copies it too, then.
func (s Source) copyAudio(p Params) (Stream, bool) {
	stream, ok := s.audio(p.AudioIndex)
	if !ok || !slices.Contains(p.AudioCodecs, stream.Codec) || audioCodecString(stream) == "" ||
		stream.Channels < 1 || stream.Channels > p.MaxAudioChannels || stream.Bitrate > maxCopiedAudioBitrate {
		return Stream{}, false
	}
	bitrate := stream.Bitrate
	if bitrate <= 0 {
		bitrate = unknownAudioBitrate
	}
	return stream, bitrate <= (p.VideoBitrate+p.AudioBitrate)/4
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

// TextSubtitle reports whether index is a subtitle track we can extract as text.
func (s Source) TextSubtitle(index int) bool {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index {
			return stream.IsTextSubtitleStream && !stream.Unreadable
		}
	}
	return false
}

// Stream returns the stream with this index, or nil.
func (s Source) Stream(index int) *Stream {
	for i := range s.Streams {
		if s.Streams[i].Index == index {
			return &s.Streams[i]
		}
	}
	return nil
}

// ImageSubtitle reports whether index is a bitmap track that a transcode can draw.
func (s Source) ImageSubtitle(index int) bool {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index {
			return stream.IsImageSubtitleStream
		}
	}
	return false
}

// Burnable reports whether a transcode can draw subtitle track index into the
// picture: a text track, or an image one.
func (s Source) Burnable(index int) bool {
	return s.TextSubtitle(index) || s.ImageSubtitle(index)
}

// clientShows says whether a client with this request shows subtitle track
// index itself, so a transcode need not draw it. A client with no subtitle
// profile shows none, one that renders PGS shows that, and no client shows the
// other image codecs, which jellyscale doesn't send it.
func (r Request) clientShows(source Source, index int) bool {
	switch {
	case source.TextSubtitle(index):
		return !r.NoSubtitleProfiles()
	case source.SubtitleCodec(index) == "PGSSUB":
		return r.ShowsPgs()
	}
	return false
}

// SubtitleCodec is the codec of subtitle track index, or "" if there is none.
func (s Source) SubtitleCodec(index int) string {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index {
			return stream.Codec
		}
	}
	return ""
}

// Unreadable says whether subtitle track index is a file that was found not to
// be readable (see library.MarkUnreadable).
func (s Source) Unreadable(index int) bool {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index {
			return stream.Unreadable
		}
	}
	return false
}

// SubtitleFingerprint is the fingerprint of the file that subtitle track index
// is, or "" for a track inside the film or one that can't be read.
func (s Source) SubtitleFingerprint(index int) string {
	for _, stream := range s.Streams {
		if stream.Type == "Subtitle" && stream.Index == index && !stream.Unreadable {
			return stream.Fingerprint
		}
	}
	return ""
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
	// AudioCodecs and MaxAudioChannels go in the URL; a re-encode copies the
	// audio when they and the cap allow it (Source.copyAudio).
	AudioCodecs      []string
	MaxAudioChannels int
	// Copy: the video goes to the client as it is. The audio is copied too when
	// Source.copyAudio allows it, else re-encoded.
	Copy bool
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
	audioStream, _ := source.audio(audio)
	if limit > 0 && source.Bitrate > limit {
		reasons = append(reasons, "ContainerBitrateExceedsLimit")
	}
	// A client with no subtitle profile can't show a track, so one that is
	// chosen is drawn into the picture, which takes a transcode. A client that
	// does show them is burnt into only when it transcodes anyway and the
	// viewer asked for it.
	var chosen *int
	shown := true
	if request.SubtitleStreamIndex != nil && source.Burnable(int(*request.SubtitleStreamIndex)) {
		index := int(*request.SubtitleStreamIndex)
		chosen = &index
		if shown = request.clientShows(source, index); !shown {
			reasons = append(reasons, "SubtitleCodecNotSupported")
		}
	}
	// Direct play can't pick a track, so any other than the default transcodes.
	if len(reasons) == 0 && audio == source.DefaultAudio() {
		return direct
	}
	// A codec the client plays from a file but its HLS profile doesn't take
	// can't be copied, which Jellyfin reports as the video codec not supported.
	if !profile.hlsMP4(video.Codec) && !slices.Contains(reasons, "VideoCodecNotSupported") {
		at := 0
		if len(reasons) > 0 && reasons[0] == "ContainerNotSupported" {
			at = 1
		}
		reasons = slices.Insert(reasons, at, "VideoCodecNotSupported")
	}
	// Jellyfin names the channel limit of the transcoding profile when it
	// transcodes anyway (a 5.1 track in a Chrome that lists two) and the
	// profile lists the codec, and before the bitrate reason
	// (compat/captures/audio-copy-12.4.md).
	if hls, _ := profile.hlsProfile("h264"); len(reasons) > 0 && hls.MaxAudioChannels > 0 && int(hls.MaxAudioChannels) < audioStream.Channels &&
		!slices.Contains(reasons, "AudioChannelsNotSupported") && listHas(hls.AudioCodec, audioStream.Codec) {
		at := len(reasons)
		if i := slices.Index(reasons, "ContainerBitrateExceedsLimit"); i >= 0 {
			at = i
		}
		reasons = slices.Insert(reasons, at, "AudioChannelsNotSupported")
	}
	// A file over the cap is re-encoded to fit it. A cap of 0 or Unlimited
	// is no cap, whatever the file.
	fits := !slices.Contains(reasons, "ContainerBitrateExceedsLimit") && (limit <= 0 || limit >= Unlimited || source.Bitrate > 0)
	if limit <= 0 {
		limit = Unlimited
	}
	videoBitrate, audioBitrate := SplitBitrate(limit)
	decision := Decision{Reasons: reasons, AudioIndex: audio, VideoBitrate: videoBitrate, AudioBitrate: audioBitrate}
	if chosen != nil && (!shown || request.AlwaysBurnInSubtitleWhenTranscoding) {
		decision.Subtitle = chosen
		if !slices.Contains(reasons, "SubtitleCodecNotSupported") {
			decision.Reasons = append(reasons, "SubtitleCodecNotSupported")
		}
	}
	// The video is copied when the client takes it as it is, whatever its
	// container, and only the audio or the container forced the stream.
	// A Dolby Vision file is copied without its Dolby Vision, so the profile's
	// conditions see HDR10 whatever its base layer: Jellyfin copies an 8.4 file
	// (HLG base) for a list of SDR and HDR10 and an 8.2 file (SDR base) for no
	// list without HDR10 (compat/captures/dolby-vision-12.4.md).
	sent := video
	if _, ok := video.baseLayer(); ok {
		sent.VideoRangeType = "HDR10"
		if strings.HasSuffix(video.VideoRangeType, "HDR10Plus") {
			sent.VideoRangeType = "HDR10Plus"
		}
	}
	decision.Copy = fits && decision.Subtitle == nil && source.CanCopy() && profile.hlsMP4(video.Codec) &&
		len(profile.limits("Video", video.Codec, sent, audioStream)) == 0
	// The audio is copied by the same test the spec makes, from the same values
	// the URL will carry, so that the cap is split as the spec will see it.
	hls, _ := profile.hlsProfile("h264")
	decision.AudioCodecs, decision.MaxAudioChannels = AudioCodecList(hls.AudioCodec), 2
	if hls.MaxAudioChannels > 0 {
		decision.MaxAudioChannels = int(min(hls.MaxAudioChannels, 8))
	}
	if audioStream.Codec != "" && len(profile.limits("VideoAudio", audioStream.Codec, video, audioStream)) > 0 {
		decision.AudioCodecs = slices.DeleteFunc(decision.AudioCodecs, func(c string) bool { return c == audioStream.Codec })
	}
	params := Params{AudioIndex: audio, AudioCodecs: decision.AudioCodecs, MaxAudioChannels: decision.MaxAudioChannels,
		VideoBitrate: videoBitrate, AudioBitrate: audioBitrate, Copy: decision.Copy}
	if copied, ok := source.copyAudio(params); ok {
		// A track that doesn't say its bitrate takes out of the cap what
		// copyAudio assumed for it, and the URL's value stays within what
		// ParseParams accepts, so a very low-rate track counts as the floor.
		audio := copied.Bitrate
		if audio <= 0 {
			audio = unknownAudioBitrate
		}
		decision.AudioBitrate = min(max(audio, MinAudioBitrate), maxCopiedAudioBitrate)
		decision.VideoBitrate = min(max(limit-decision.AudioBitrate, MinVideoBitrate), Unlimited)
	}
	// An SDR HEVC or AV1 film whose profile can't be named is re-encoded for
	// that, which the client's own CodecProfiles may not have said.
	if !decision.Copy && source.Copyable() && source.codecs() == "" && video.VideoRangeType == "SDR" && profile.hlsMP4(video.Codec) &&
		!slices.Contains(decision.Reasons, "VideoProfileNotSupported") {
		decision.Reasons = append(decision.Reasons, "VideoProfileNotSupported")
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

// canTranscode reports whether the client accepts what this server produces
// when it re-encodes: fMP4 HLS carrying H.264 and AAC.
func (p DeviceProfile) canTranscode() bool { return p.hlsMP4("h264") }

// hlsMP4 reports whether the client takes this video codec, with AAC, in fMP4
// HLS. A copy needs it as a re-encode does.
func (p DeviceProfile) hlsMP4(codec string) bool {
	_, ok := p.hlsProfile(codec)
	return ok
}

// hlsProfile is the transcoding profile hlsMP4 matched. Its audio list and
// channel limit are what the audio of this stream is held to.
func (p DeviceProfile) hlsProfile(codec string) (TranscodingProfile, bool) {
	for _, t := range p.TranscodingProfiles {
		if strings.EqualFold(t.Type, "Video") && strings.EqualFold(t.Protocol, "hls") &&
			strings.EqualFold(t.Container, "mp4") && listHas(t.VideoCodec, codec) && listHas(t.AudioCodec, "aac") {
			return t, true
		}
	}
	return TranscodingProfile{}, false
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
		// Jellyfin names the codecs the container's absence leaves, judged by the
		// first profile that lists the video codec, or the first profile if none
		// does (compat/captures/copy-both-12.4.md).
		reasons := []string{"ContainerNotSupported"}
		var profiles []DirectPlayProfile
		for _, d := range p.DirectPlayProfiles {
			if strings.EqualFold(d.Type, "Video") {
				profiles = append(profiles, d)
			}
		}
		if len(profiles) == 0 {
			return reasons
		}
		chosen := profiles[0]
		if i := slices.IndexFunc(profiles, func(d DirectPlayProfile) bool { return listHas(d.VideoCodec, video.Codec) }); i >= 0 {
			chosen = profiles[i]
		}
		if !listHas(chosen.VideoCodec, video.Codec) {
			reasons = append(reasons, "VideoCodecNotSupported")
		}
		if hasAudio && !listHas(chosen.AudioCodec, audioStream.Codec) {
			reasons = append(reasons, "AudioCodecNotSupported")
		}
		return reasons
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
			var value, reason, orElse string
			switch strings.ToLower(c.Property) {
			case "videolevel":
				value, reason = positive(video.Level), "VideoLevelNotSupported"
			case "isinterlaced":
				value, reason = strconv.FormatBool(video.IsInterlaced), "InterlacedVideoNotSupported"
			case "videorangetype":
				value, reason = strings.ToLower(video.VideoRangeType), "VideoRangeTypeNotSupported"
				if value == "hdr10plus" {
					orElse = "hdr10" // HDR10+ is HDR10 with more, so a client that plays one plays it.
				}
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
			if !c.holds(value) && !(orElse != "" && c.holds(orElse)) && !slices.Contains(reasons, reason) {
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
