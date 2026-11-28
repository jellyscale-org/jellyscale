package media

// ClientFields is the one list of fields a strict client reads from every source,
// stream and attachment. A value is "bool", "integer", or the allowed values of
// an enum string. They are always present and never null; a field a client does
// not read is absent unless the film has a value for it. The unit and integration
// checks of this model both read this list.
var ClientFields = map[string]map[string]string{
	"source": {
		"GenPtsInput": "bool", "HasSegments": "bool", "IgnoreDts": "bool", "IgnoreIndex": "bool",
		"IsInfiniteStream": "bool", "IsRemote": "bool", "ReadAtNativeFramerate": "bool",
		"RequiresClosing": "bool", "RequiresLooping": "bool", "RequiresOpening": "bool",
		"SupportsDirectPlay": "bool", "SupportsDirectStream": "bool", "SupportsProbing": "bool",
		"SupportsTranscoding": "bool",
		"Protocol":            "File Http Rtmp Rtsp Udp Rtp Ftp", "TranscodingSubProtocol": "http hls", "Type": "Default Grouping Placeholder",
	},
	"stream": {
		"IsDefault": "bool", "IsExternal": "bool", "IsForced": "bool", "IsHearingImpaired": "bool",
		"IsInterlaced": "bool", "IsOriginal": "bool", "IsTextSubtitleStream": "bool", "SupportsExternalStream": "bool",
		"Index": "integer", "Type": "Audio Video Subtitle EmbeddedImage Data Lyric",
	},
	"attachment": {"Index": "integer"},
}
