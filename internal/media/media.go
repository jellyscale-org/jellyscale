// Package media is the one description of a film's file that every part of the
// server shares: what the probe finds in it, what the scan stores, what the
// playback decision reads and what a client is answered with.
//
// Jellyfin's SDK-based clients decode strictly: an answer that lacks a field
// the schema requires (compat/schema-12.4.json) fails to decode and the client
// stops. So each such field is a field of the type that is always sent, with
// the value Jellyfin sends for a file played from disk when a stored row has
// none. Every other field is sent only when it has a value.
package media

import "encoding/json"

// MediaSource is one playable file of an item, as stored in media_sources and
// in the item's DTO, and as PlaybackInfo answers it.
type MediaSource struct {
	ID        string `json:"Id,omitempty"`
	Name      string `json:"Name,omitempty"`
	Path      string `json:"Path,omitempty"`
	Container string `json:"Container,omitempty"`
	ETag      string `json:"ETag,omitempty"`
	VideoType string `json:"VideoType,omitempty"`

	Size         int64 `json:"Size"`
	RunTimeTicks int64 `json:"RunTimeTicks"`
	Bitrate      int64 `json:"Bitrate"`

	// Protocol, Type and TranscodingSubProtocol are enums: never empty.
	Protocol               string `json:"Protocol"`
	Type                   string `json:"Type"`
	TranscodingSubProtocol string `json:"TranscodingSubProtocol"`

	IsRemote              bool `json:"IsRemote"`
	ReadAtNativeFramerate bool `json:"ReadAtNativeFramerate"`
	IgnoreDts             bool `json:"IgnoreDts"`
	IgnoreIndex           bool `json:"IgnoreIndex"`
	GenPtsInput           bool `json:"GenPtsInput"`
	SupportsTranscoding   bool `json:"SupportsTranscoding"`
	SupportsDirectStream  bool `json:"SupportsDirectStream"`
	SupportsDirectPlay    bool `json:"SupportsDirectPlay"`
	IsInfiniteStream      bool `json:"IsInfiniteStream"`
	RequiresOpening       bool `json:"RequiresOpening"`
	RequiresClosing       bool `json:"RequiresClosing"`
	RequiresLooping       bool `json:"RequiresLooping"`
	SupportsProbing       bool `json:"SupportsProbing"`
	HasSegments           bool `json:"HasSegments"`

	MediaStreams     []MediaStream     `json:"MediaStreams"`
	MediaAttachments []MediaAttachment `json:"MediaAttachments"`
	Formats          []string          `json:"Formats"`

	// Only PlaybackInfo sets these.
	RequiredHTTPHeaders        *Headers `json:"RequiredHttpHeaders,omitempty"`
	DefaultAudioStreamIndex    *int     `json:"DefaultAudioStreamIndex,omitempty"`
	DefaultSubtitleStreamIndex *int     `json:"DefaultSubtitleStreamIndex,omitempty"`
	TranscodingURL             string   `json:"TranscodingUrl,omitempty"`
	TranscodingContainer       string   `json:"TranscodingContainer,omitempty"`
}

// Headers are the HTTP headers a client must send to fetch a source. A source
// of this server needs none, so it holds none: it is sent as an empty object.
type Headers struct{}

// UnmarshalJSON presets what a stored source may lack, so that a row of any
// release decodes to a source that is answered with every required field.
func (s *MediaSource) UnmarshalJSON(data []byte) error {
	type plain MediaSource
	p := plain{Protocol: "File", Type: "Default", SupportsProbing: true, TranscodingSubProtocol: "http"}
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*s = MediaSource(p)
	return nil
}

// MarshalJSON gives a source built in code the same enum values, and never
// sends a list as null.
func (s MediaSource) MarshalJSON() ([]byte, error) {
	type plain MediaSource
	if s.Protocol == "" {
		s.Protocol = "File"
	}
	if s.Type == "" {
		s.Type = "Default"
	}
	if s.TranscodingSubProtocol == "" {
		s.TranscodingSubProtocol = "http"
	}
	if s.MediaStreams == nil {
		s.MediaStreams = []MediaStream{}
	}
	if s.MediaAttachments == nil {
		s.MediaAttachments = []MediaAttachment{}
	}
	if s.Formats == nil {
		s.Formats = []string{}
	}
	return json.Marshal(plain(s))
}

// MediaStream is one track of a file: a video, audio or subtitle track, or an
// attachment as the probe lists it, or a subtitle file beside the film.
type MediaStream struct {
	Index int    `json:"Index"`
	Type  string `json:"Type"`

	IsDefault              bool `json:"IsDefault"`
	IsForced               bool `json:"IsForced"`
	IsHearingImpaired      bool `json:"IsHearingImpaired"`
	IsOriginal             bool `json:"IsOriginal"`
	IsExternal             bool `json:"IsExternal"`
	IsInterlaced           bool `json:"IsInterlaced"`
	IsTextSubtitleStream   bool `json:"IsTextSubtitleStream"`
	SupportsExternalStream bool `json:"SupportsExternalStream"`

	Codec        string `json:"Codec,omitempty"`
	Profile      string `json:"Profile,omitempty"`
	Language     string `json:"Language,omitempty"`
	Title        string `json:"Title,omitempty"`
	DisplayTitle string `json:"DisplayTitle,omitempty"`

	Width         int   `json:"Width,omitempty"`
	Height        int   `json:"Height,omitempty"`
	Level         int   `json:"Level,omitempty"`
	BitDepth      int   `json:"BitDepth,omitempty"`
	Channels      int   `json:"Channels,omitempty"`
	SampleRate    int   `json:"SampleRate,omitempty"`
	BitRate       int64 `json:"BitRate,omitempty"`
	DurationTicks int64 `json:"DurationTicks,omitempty"`

	AverageFrameRate   float64 `json:"AverageFrameRate,omitempty"`
	ReferenceFrameRate float64 `json:"ReferenceFrameRate,omitempty"`

	// The colour labels. ColorTransfer is sent, empty or not, for a video the probe read.
	ColorTransfer  *string `json:"ColorTransfer,omitempty"`
	VideoRange     string  `json:"VideoRange,omitempty"`
	VideoRangeType string  `json:"VideoRangeType,omitempty"`
	VideoDoViTitle string  `json:"VideoDoViTitle,omitempty"`

	// The Dolby Vision configuration and the HDR10+ flag, which a file has or
	// has not, and whose zero is a value.
	DvVersionMajor            *int  `json:"DvVersionMajor,omitempty"`
	DvVersionMinor            *int  `json:"DvVersionMinor,omitempty"`
	DvProfile                 *int  `json:"DvProfile,omitempty"`
	DvLevel                   *int  `json:"DvLevel,omitempty"`
	RpuPresentFlag            *int  `json:"RpuPresentFlag,omitempty"`
	ElPresentFlag             *int  `json:"ElPresentFlag,omitempty"`
	BlPresentFlag             *int  `json:"BlPresentFlag,omitempty"`
	DvBlSignalCompatibilityID *int  `json:"DvBlSignalCompatibilityId,omitempty"`
	Hdr10PlusPresentFlag      *bool `json:"Hdr10PlusPresentFlag,omitempty"`

	// IsImageSubtitleStream is sent for a subtitle track the probe read, true
	// for a bitmap track that can be drawn into a transcode (PGS, VobSub, DVB).
	IsImageSubtitleStream *bool `json:"IsImageSubtitleStream,omitempty"`

	// How a client fetches a subtitle track; PlaybackInfo sets them.
	DeliveryMethod string `json:"DeliveryMethod,omitempty"`
	DeliveryURL    string `json:"DeliveryUrl,omitempty"`
	IsExternalURL  *bool  `json:"IsExternalUrl,omitempty"`

	// Path is a subtitle file's key in the media store, and SubPath the .sub of
	// a VobSub pair whose .idx Path is. Fingerprint identifies the version of the
	// file; FileSize and FileETag are the version of a .sup. Unreadable is a file
	// a worker found it cannot read. They are stored with the source and are
	// never in an answer.
	Path        string `json:"Path,omitempty"`
	SubPath     string `json:"SubPath,omitempty"`
	SubSize     int64  `json:"SubSize,omitempty"`
	Fingerprint string `json:"Fingerprint,omitempty"`
	FileSize    int64  `json:"FileSize,omitempty"`
	FileETag    string `json:"FileETag,omitempty"`
	Unreadable  bool   `json:"Unreadable,omitempty"`

	// FileName and MimeType are an attachment's, listed as a stream of type
	// Attachment in what is stored, and as a MediaAttachment in an answer.
	FileName string `json:"FileName,omitempty"`
	MimeType string `json:"MimeType,omitempty"`

	// File is where a worker put a copy of the external image track to read it
	// from. It is never stored.
	File string `json:"-"`
	// OpenAt is set by a worker for a track inside the film whose cue points
	// the scan read. For a run whose first frame is at time t (seconds on the
	// film's clock) it says when the run must open the film to read the picture
	// showing there, t if none is. Nil: the times are not known, and the run
	// opens the film a fixed time early. It is never stored.
	OpenAt func(t float64) (float64, error) `json:"-"`
}

// MediaAttachment is a font or another file attached to a film.
type MediaAttachment struct {
	Index       int    `json:"Index"`
	CodecTag    string `json:"CodecTag,omitempty"`
	FileName    string `json:"FileName,omitempty"`
	MimeType    string `json:"MimeType,omitempty"`
	DeliveryURL string `json:"DeliveryUrl,omitempty"`
}

// IsImage says whether the stream is a bitmap subtitle track.
func (s MediaStream) IsImage() bool {
	return s.IsImageSubtitleStream != nil && *s.IsImageSubtitleStream
}

// Transfer is ffprobe's name for the stream's transfer function: smpte2084
// for PQ, arib-std-b67 for HLG. Empty when the file doesn't say, and on a
// source scanned before it was recorded.
func (s MediaStream) Transfer() string {
	if s.ColorTransfer == nil {
		return ""
	}
	return *s.ColorTransfer
}

// Ptr is a pointer to a copy of v, for the fields that are sent only when set.
func Ptr[T any](v T) *T { return &v }

// Val is what p points to, or the zero value when it points to nothing.
func Val[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
