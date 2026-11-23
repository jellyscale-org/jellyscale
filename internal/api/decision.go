package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/transcode"
)

// The methods of a decision. refused has no series in the decisions counter: a refusal is counted by its reason.
const (
	methodDirectPlay = "direct_play"
	methodCopy       = "copy"
	methodReencode   = "reencode"
	methodRefused    = "refused"
)

// outcome is what planPlayback verdict about one source, for the decision line and its counters.
type outcome struct {
	method string
	// reasons are every reason of the decision (playback.Decision.Why). A refusal has one, and so has a direct play that
	// gave up a transcode: why none was given. A direct play that asked for none has none.
	reasons []string
	// asked is, on a direct play that gave up a transcode, what was asked (copy or reencode), and askedReasons its reasons.
	asked        string
	askedReasons []string
	// output is what a copy or a re-encode gives the viewer.
	output *playback.Output
	// streams are the file's tracks where the row was read, else the DTO's, and video the file's video, nil where not read.
	streams []media.MediaStream
	video   *media.MediaStream
	// admission is how the workers were counted for a transcode: nil where none was asked (a direct play, a live job joined).
	admission *transcode.Admission
}

func (p outcome) with(method string, reasons ...string) outcome {
	p.method, p.reasons = method, reasons
	return p
}

// A verdict waits for the reply to be known: a refusal anywhere in it drops the lines of the sources before it.
type verdict struct {
	method  string
	reasons []string
	attrs   []any
}

// decisionOf is the line and counts of one source. audio is the track that plays, if the file has one.
func decisionOf(r *http.Request, itemID, playSession string, source media.MediaSource, request playback.Request, audio int, hasAudio bool, p outcome) verdict {
	user := sessionOf(r)
	attrs := []any{"method", p.method}
	if len(p.reasons) > 0 {
		attrs = append(attrs, "reasons", strings.Join(p.reasons, ","))
	}
	if p.asked != "" {
		attrs = append(attrs, "asked", p.asked, "asked_reasons", strings.Join(p.askedReasons, ","))
	}
	attrs = append(attrs,
		"client", user.Client, "client_version", user.Version, "device", user.Device, "device_id", user.DeviceID,
		"user", user.User.ID, "item", dashed(itemID), "source", dashed(source.ID), "play_session", playSession)
	if p.video != nil {
		attrs = append(attrs, "src_range", p.video.VideoRangeType)
		if p.video.DvProfile != nil {
			attrs = append(attrs, "src_dv_profile", *p.video.DvProfile)
		}
	}
	if out := p.output; out != nil {
		height := 0
		if p.video != nil {
			height = p.video.Height
		}
		attrs = append(attrs, "out_codec", out.Codec, "out_height", out.Height, "out_range", out.Range, "src_height", height,
			"tone_mapped", yesNo(out.ToneMapped), "ceiling_lowered", yesNo(out.CeilingLowered))
	}
	attrs = append(attrs, admissionAttrs(p)...)
	attrs = append(attrs, audioAttrs(source, request, audio, hasAudio)...)
	attrs = append(attrs, subtitleAttrs(request, p.streams)...)
	return verdict{method: p.method, reasons: p.reasons, attrs: attrs}
}

// admissionAttrs say how the workers were counted (JS-161): an admitted copy or re-encode says whether one worker held it
// or two shared it, with the job's cost and the capacity of each worker counted, in reference cores; a refusal gives the
// cost and the most any worker had.
func admissionAttrs(p outcome) []any {
	a := p.admission
	if a == nil || a.Cost == 0 {
		return nil
	}
	switch {
	case p.method == methodRefused:
		if a.Most == 0 {
			return nil // Refused before any worker was counted (no worker, the stores differ, a remembered failure).
		}
		return []any{"cost", a.Cost, "most_capacity", a.Most}
	case (p.method == methodCopy || p.method == methodReencode) && a.How != "":
		capacities := make([]string, len(a.Capacities))
		for i, c := range a.Capacities {
			capacities[i] = strconv.FormatFloat(c, 'g', -1, 64)
		}
		return []any{"admitted", a.How, "cost", a.Cost, "capacities", strings.Join(capacities, ",")}
	}
	return nil
}

// audioAttrs says what the request asked of the audio and the track that plays. The server chose it unless the request named
// that track.
func audioAttrs(source media.MediaSource, request playback.Request, audio int, hasAudio bool) []any {
	asked, named := "none", false
	tracks := audioTracksOf(source)
	if request.AudioStreamIndex != nil {
		asked = strconv.FormatInt(int64(*request.AudioStreamIndex), 10)
		named = true
	}
	attrs := []any{"audio_asked", asked}
	if !hasAudio {
		return attrs
	}
	by := "server"
	if named && int(*request.AudioStreamIndex) == audio {
		by = "client"
	}
	attrs = append(attrs, "audio_index", audio, "audio_by", by)
	for _, track := range tracks {
		if track.Type == "Audio" && track.Index == audio {
			attrs = append(attrs, "audio_codec", track.Codec)
			if track.Language != "" {
				attrs = append(attrs, "audio_language", track.Language)
			}
		}
	}
	return attrs
}

// subtitleAttrs says what the request asked of the subtitles and the track that is. The server picks none, so none is marked
// as its choice.
func subtitleAttrs(request playback.Request, streams []media.MediaStream) []any {
	if request.SubtitleStreamIndex == nil {
		return []any{"subtitle_asked", "none", "subtitle_index", "none"}
	}
	index := int(*request.SubtitleStreamIndex)
	at := slices.IndexFunc(streams, func(stream media.MediaStream) bool { return stream.Type == "Subtitle" && stream.Index == index })
	if at < 0 {
		return []any{"subtitle_asked", strconv.Itoa(index), "subtitle_index", "none"}
	}
	stream, kind := streams[at], "text"
	if stream.IsImage() {
		kind = "image"
	}
	attrs := []any{"subtitle_asked", strconv.Itoa(index), "subtitle_index", index, "subtitle_codec", stream.Codec, "subtitle_kind", kind}
	if stream.Language != "" {
		attrs = append(attrs, "subtitle_language", stream.Language)
	}
	return attrs
}

func dashed(id string) string { return strings.ToLower(db.DashedID(id)) }

func yesNo(yes bool) string {
	if yes {
		return "yes"
	}
	return "no"
}

// logDecisions writes each source's line and counts it, once the reply is known to list them.
func (s *Server) logDecisions(decisions ...verdict) {
	for _, d := range decisions {
		s.Log.Info("playback decision", d.attrs...)
		if d.method == methodRefused {
			playbackRefusals.WithLabelValues(d.reasons[0]).Inc()
			continue
		}
		playbackDecisions.WithLabelValues(d.method).Inc()
		for _, reason := range d.reasons {
			playbackDecisionReasons.WithLabelValues(d.method, reason).Inc()
		}
	}
}
