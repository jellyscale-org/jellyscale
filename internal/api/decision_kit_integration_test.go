//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// JS-153: the kit of the decision-line checks (decision_*_integration_test.go). The task leaves the line's wording to the
// build, so this header pins it once; every check reads the line through the helpers below and nowhere else.
//
// THE LINE. One slog record per media source a PlaybackInfo reply lists (one for a refused reply, for the source refused),
// level INFO, message "playback decision", written to the api's logger (the text handler: key=value, a value with a space
// quoted). Its keys, which the build must add:
//
//	method           direct_play | copy | reencode | refused  (the counter's spelling; "refused" has no series in decisions_total)
//	reasons          every reason of the decision, in any rendering that separates names by anything but a letter
//	                 (a comma, a space, a bracketed list); absent or empty on a direct play with no reason, never on the others
//	asked            on a direct play that gave up a transcode (the picked track or burn-in taken back, transcoding off, no
//	                 probed duration): what was asked, copy or reencode. Absent on every other line.
//	asked_reasons    on the same line: the reasons of the transcode that was asked (reasons then holds why none was given)
//	client, client_version, device, device_id   the Authorization header's Client, Version, Device, DeviceId
//	user             the user id, as the other api lines print it (the id the session holds), never the account's name
//	item, source     the ids as a dashed lower-case UUID, whatever form the request used
//	play_session     equal to the reply's PlaySessionId
//	out_codec, out_height, out_range, src_height   copy and reencode lines only
//	tone_mapped, ceiling_lowered                   yes | no, copy and reencode lines only
//	src_range, src_dv_profile                      the file's VideoRangeType; the Dolby Vision profile when it has one
//	audio_asked      the AudioStreamIndex as the request gave it ("1", "7"), or none
//	audio_index, audio_codec, audio_language       the track that plays
//	audio_by         server when the request named none or an index that is no audio track of the file, else client
//	subtitle_asked   the SubtitleStreamIndex as given ("-1" included), or none
//	subtitle_index, subtitle_codec, subtitle_language, subtitle_kind (text | image)   what it resolved to; none for -1 or a
//	                 number that is no subtitle track
//
// THE VOCABULARY. Jellyfin's names are its TranscodeReason enum, as captured in compat/captures/transcode-12.4.json; jellyscale's own are not in it.
// The reasons below are what these checks pin.
//
//	Jellyfin's:  ContainerNotSupported VideoCodecNotSupported AudioCodecNotSupported SubtitleCodecNotSupported
//	             SecondaryAudioNotSupported VideoProfileNotSupported VideoLevelNotSupported VideoResolutionNotSupported
//	             InterlacedVideoNotSupported AudioChannelsNotSupported ContainerBitrateExceedsLimit VideoRangeTypeNotSupported
//	jellyscale's: KeyframeIndexUnusable (a copied codec with no keyframe index, or keyframes too far apart)
//	             BitrateUnknownUnderCap (a cap on a file of unknown bitrate)
//	             VideoRangeNotCopyable (H.264 in HDR, Dolby Vision profile 5)    VideoRangeUnknown (a range not yet scanned)
//	             VideoProfileUnknown (a codec profile that cannot be named)
//	             and the refusals and fallbacks: NoWorkerRunning NoWorkerWithFreeCores TranscodeStoreMismatch RememberedFailure
//	             WorkersAtCopyBound NoWorkerOfThisOutputVersion NoTranscodeStore NoProbedDuration
//
// THE COUNTERS. jellyscale_playbackinfo_decisions_total{method} (kept), jellyscale_playbackinfo_refusals_total{reason}
// (gains the label) and jellyscale_playbackinfo_decision_reasons_total{method,reason} (new). playbackRefusals becomes a
// vec, so no check here names it: counters are read through Collectors() by metric name and labels, and the existing tests
// that read it (metrics_integration_test.go, cores_integration_test.go, playbackinfo_reads_integration_test.go) are the
// implementer's to move to its new type.

const (
	decisionMsg    = "playback decision"
	decisionsName  = "jellyscale_playbackinfo_decisions_total"
	refusalsName   = "jellyscale_playbackinfo_refusals_total"
	reasonsName    = "jellyscale_playbackinfo_decision_reasons_total"
	probeClient    = `Client="Probe Client", Device="Probe Device", DeviceId="probe-dev-1", Version="9.9.9"`
	probeAuthority = `MediaBrowser ` + probeClient + `, Token="%s"`
)

// decisionLine is one parsed record.
type decisionLine map[string]string

var keyValue = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)

// parseLines reads the records of a text log: key to unquoted value. A line without level and msg is not a record.
func parseLines(log string) []decisionLine {
	var out []decisionLine
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "level=") {
			continue
		}
		record := decisionLine{}
		for _, m := range keyValue.FindAllStringSubmatch(line, -1) {
			value := m[2]
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			if _, seen := record[m[1]]; !seen {
				record[m[1]] = value
			}
		}
		out = append(out, record)
	}
	return out
}

// decisionsOf are the INFO "playback decision" records of a log.
func decisionsOf(log string) []decisionLine {
	var out []decisionLine
	for _, record := range parseLines(log) {
		if record["msg"] == decisionMsg && record["level"] == "INFO" {
			out = append(out, record)
		}
	}
	return out
}

// reasonList is a record's names, however the build renders the list.
func (l decisionLine) reasonList(key string) []string {
	return strings.FieldsFunc(l[key], func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
}

func (l decisionLine) has(key, name string) bool {
	for _, got := range l.reasonList(key) {
		if got == name {
			return true
		}
	}
	return false
}

// A film is the stored row PlaybackInfo decides from. The zero value is a 720p SDR H.264 file in Matroska with an AAC
// default track, a second AAC track and a text subtitle: what Chrome plays as it is.
type storedFilm struct {
	container string
	video     map[string]any
	audio     []map[string]any
	subs      []map[string]any
	bitrate   int
	keyframes string // "" is the evenly spaced index of TestPlaybackInfoCountsACopy; "none" is no index; else a bigint[] literal
	runtime   int64  // ticks; 0 is 90 s
}

func (f storedFilm) streams() string {
	video := map[string]any{"Index": 0, "Type": "Video", "Codec": "h264", "Profile": "Main", "Level": 41, "VideoRangeType": "SDR", "Width": 1280, "Height": 720, "AverageFrameRate": 24}
	for k, v := range f.video {
		video[k] = v
	}
	audio := f.audio
	if audio == nil {
		audio = []map[string]any{{"Index": 1, "Type": "Audio", "Codec": "aac", "IsDefault": true, "Channels": 2, "Language": "eng"}, {"Index": 2, "Type": "Audio", "Codec": "aac", "Channels": 2, "Language": "fra"}}
	}
	subs := f.subs
	if subs == nil {
		subs = []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true, "Language": "eng"}}
	}
	all := append([]map[string]any{video}, audio...)
	raw, _ := json.Marshal(append(all, subs...))
	return string(raw)
}

// decisionEnv is transcodeEnv with the film's row replaced and both replicas logging at INFO into the returned log.
func decisionEnv(t *testing.T, f storedFilm) (*env, string, *syncBuffer) {
	t.Helper()
	e, _, source := transcodeEnv(t, "h264", "aac")
	container := f.container
	if container == "" {
		container = "mkv"
	}
	mustExec(t, e.pool, "UPDATE media_sources SET streams = $1, container = $2, size = 25000000, video_end = 900000000", f.streams(), container)
	keyframes := "'{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}'"
	switch f.keyframes {
	case "":
	case "none":
		keyframes = "NULL"
	default:
		keyframes = "'" + f.keyframes + "'"
	}
	mustExec(t, e.pool, "UPDATE media_sources SET keyframes = "+keyframes)
	runtime := f.runtime
	if runtime == 0 {
		runtime = 900000000
	}
	bitrate := f.bitrate
	if bitrate == 0 {
		bitrate = 2000000
	}
	mustExec(t, e.pool, `UPDATE items SET run_time_ticks = $1, dto = jsonb_set(dto, '{MediaSources,0,Bitrate}', to_jsonb($2::int))`, runtime, bitrate)
	log := &syncBuffer{}
	for _, s := range e.apis {
		s.Log = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return e, source, log
}

// A request is what a client sends; route is "post" (POST /Items/{id}/PlaybackInfo), "get" or "marked" (the jellyscale route).
type infoRequest struct {
	route    string
	item     string // the id in the path, in whatever form; "" is the item's own, dashed
	body     map[string]any
	query    string
	token    string // "" is the viewer's
	replica  int
	noClient bool
}

// profile is the captured Chrome profile with the changes given applied through edit.
func chromeProfile(t *testing.T, edit func(p map[string]any)) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(capturedProfile(t), &p); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(p)
	}
	return p
}

// codecLimit adds a CodecProfile for every video codec with the conditions given as property, condition, value triples.
func codecLimit(p map[string]any, conditions ...[3]string) {
	var list []any
	for _, c := range conditions {
		list = append(list, map[string]any{"Condition": c[1], "Property": c[0], "Value": c[2], "IsRequired": false})
	}
	p["CodecProfiles"] = append(p["CodecProfiles"].([]any), map[string]any{"Type": "Video", "Conditions": list})
}

// takesInHLS makes the profile's fMP4 HLS take the codec, so that the video can be copied.
func takesInHLS(p map[string]any, codec string) {
	for _, entry := range p["TranscodingProfiles"].([]any) {
		t := entry.(map[string]any)
		if t["Type"] == "Video" && t["Container"] == "mp4" {
			t["VideoCodec"] = t["VideoCodec"].(string) + "," + codec
		}
	}
}

// decide sends the request and returns the reply and the decision lines the replica logged for it.
func (e *env) decide(t *testing.T, log *syncBuffer, r infoRequest) (map[string]any, []decisionLine, int) {
	t.Helper()
	before := len(decisionsOf(log.String()))
	item := r.item
	if item == "" {
		item = e.item
	}
	method, path := "POST", "/Items/"+item+"/PlaybackInfo"
	switch r.route {
	case "get":
		method = "GET"
	case "marked":
		path = "/jellyscale/v1/items/" + item + "/playback-info"
	}
	if r.query != "" {
		path += "?" + r.query
	}
	token := r.token
	if token == "" {
		token = e.token
	}
	var body any
	if method == "POST" {
		body = r.body
		if body == nil {
			body = map[string]any{}
		}
	}
	header := `MediaBrowser Token="` + token + `"`
	if !r.noClient {
		header = fmt.Sprintf(probeAuthority, token)
	}
	response, raw := e.do(t, r.replica, method, path, "", body, "Authorization", header)
	var reply map[string]any
	_ = json.Unmarshal(raw, &reply)
	if response.StatusCode != 200 {
		return reply, decisionsOf(log.String())[before:], response.StatusCode
	}
	return reply, decisionsOf(log.String())[before:], 200
}

// transcodeReasonsOf are the names in a source's TranscodingUrl.
func transcodeReasonsOf(t *testing.T, source map[string]any) []string {
	t.Helper()
	raw, _ := source["TranscodingUrl"].(string)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return strings.FieldsFunc(parsed.Query().Get("TranscodeReasons"), func(r rune) bool { return r == ',' })
}

// series reads every jellyscale_playbackinfo_* series of this process into "name{label=value,...}" -> value, through the
// package's Collectors, so that no check names a collector variable (the refusals counter changes type).
func series(t *testing.T) map[string]float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	for _, c := range Collectors() {
		if err := registry.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "jellyscale_playbackinfo_") {
			continue
		}
		for _, m := range family.GetMetric() {
			out[seriesKey(family.GetName(), m)] = m.GetCounter().GetValue()
		}
	}
	return out
}

func seriesKey(name string, m *dto.Metric) string {
	var labels []string
	for _, l := range m.GetLabel() {
		labels = append(labels, l.GetName()+"="+l.GetValue())
	}
	if len(labels) == 0 {
		return name
	}
	return name + "{" + strings.Join(labels, ",") + "}"
}

// delta is what each series rose by between two readings; a series that did not move is left out.
func delta(before, after map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range after {
		if v != before[k] {
			out[k] = v - before[k]
		}
	}
	return out
}

func dec(method string) string { return decisionsName + "{method=" + method + "}" }

func refusal(reason string) string { return refusalsName + "{reason=" + reason + "}" }

// counted is the key of the reasons counter: the labels in the order a registry sorts them.
func counted(method, reason string) string {
	return reasonsName + "{method=" + method + ",reason=" + reason + "}"
}
