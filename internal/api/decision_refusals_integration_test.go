//go:build integration

package api

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
)

// JS-153: refusals, fallbacks, privacy and the counters. The kit is decision_kit_integration_test.go.

var version = strconv.Itoa(playback.OutputVersion)

// A refusal: the setup makes the transcode of an HEVC film (which this client re-encodes) impossible in one way, and the
// reply is the refusal, one line, and one reason of the closed set.
type refusalCase struct {
	name   string
	reason string
	// pick asks for the file's second track, which only a transcode plays; copy asks for a film that is copied.
	pick  bool
	copy  bool
	setup func(t *testing.T, e *env, ask func() map[string]any)
}

func refusalCases() []refusalCase {
	return []refusalCase{
		{name: "no worker running", reason: "NoWorkerRunning", setup: func(t *testing.T, e *env, _ func() map[string]any) {
			mustExec(t, e.pool, "TRUNCATE transcode_nodes")
		}},
		{name: "no worker with the cores free", reason: "NoWorkerWithFreeCores", setup: func(t *testing.T, e *env, _ func() map[string]any) {
			mustExec(t, e.pool, "TRUNCATE transcode_nodes")
			mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, cores, output_version) VALUES ('w1', 'worker', 0.04, "+version+")")
		}},
		{name: "a transcode store mismatch", reason: "TranscodeStoreMismatch", setup: func(t *testing.T, e *env, _ func() map[string]any) {
			mustExec(t, e.pool, "UPDATE transcode_nodes SET roles = 'worker', store = 's3://bucket/segments?endpoint=http://s3'")
			mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, store) VALUES ('replica', 'api,stream', 'file:///elsewhere')")
		}},
		{name: "a remembered failure", reason: "RememberedFailure", setup: func(t *testing.T, e *env, ask func() map[string]any) {
			hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, ask()))["TranscodingUrl"].(string))))
			mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, ended_at, output_version)
				VALUES ($1, '{}', 'o', 0, 0, 'failed', 'ffmpeg could not read /media/Distinct Folder/Distinct File.mkv', now(), `+version+`)`, hash)
		}},
		{name: "every worker at its copy bound", reason: "WorkersAtCopyBound", copy: true, setup: func(t *testing.T, e *env, _ func() map[string]any) {
			for i := 0; i < 32; i++ { // internal/transcode.copyBound
				mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker, output_version, cost)
					VALUES ($1, '{"Copy":true}', 'o', 0, 0, 'running', now() + interval '1 hour', 'w1', `+version+`, 0.01)`, fmt.Sprintf("held-copy-%d", i))
			}
		}},
		{name: "a picked track where no worker of this output version runs transcodes", reason: "NoWorkerOfThisOutputVersion", pick: true,
			setup: func(t *testing.T, e *env, _ func() map[string]any) {
				mustExec(t, e.pool, "UPDATE transcode_nodes SET output_version = output_version + 1")
			}},
		{name: "a picked track where no transcodeStore is set", reason: "NoTranscodeStore", pick: true, setup: func(t *testing.T, e *env, _ func() map[string]any) {
			e.apis[0].Transcode = nil
		}},
		{name: "a picked track on a film with no probed duration", reason: "NoProbedDuration", pick: true, setup: func(t *testing.T, e *env, _ func() map[string]any) {
			mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 0")
		}},
	}
}

func (r refusalCase) film() storedFilm {
	switch {
	case r.pick:
		return storedFilm{}
	case r.copy:
		return storedFilm{audio: ac3}
	}
	return hevcFilm
}

func (r refusalCase) body(t *testing.T, source string) map[string]any {
	body := chrome(t)
	body["MediaSourceId"] = source
	if r.pick {
		body["AudioStreamIndex"] = "2"
	}
	return body
}

// A refused reply logs one line saying refused with one reason, adds one to the refusals counter under it, and counts no
// decision and no reason.
func TestARefusedReplyLogsOneRefusedLineWithOneReasonAndCountsItUnderIt(t *testing.T) {
	for _, r := range refusalCases() {
		t.Run(r.name, func(t *testing.T) {
			e, source, log := decisionEnv(t, r.film())
			r.setup(t, e, func() map[string]any { return r.body(t, source) })
			before := series(t)
			reply, lines, status := e.decide(t, log, infoRequest{body: r.body(t, source)})
			if status != 200 || reply["ErrorCode"] != "RateLimitExceeded" || len(reply["MediaSources"].([]any)) != 0 {
				t.Fatalf("not a refused reply: %d %v", status, reply)
			}
			shape(t, e, reply, lines, e.item)
			line := lines[0]
			if line["method"] != "refused" || line["source"] != e.item {
				t.Errorf("method=%q source=%q: %v", line["method"], line["source"], line)
			}
			if got := line.reasonList("reasons"); len(got) != 1 || got[0] != r.reason {
				t.Errorf("reasons %q, want exactly %s", line["reasons"], r.reason)
			}
			moved := delta(before, series(t))
			if len(moved) != 1 || moved[refusal(r.reason)] != 1 {
				t.Errorf("the counters moved %v, want only %s by 1", moved, refusal(r.reason))
			}
		})
	}
}

// A refusal where an earlier source of the reply was judged playable: one line, for the refused source, and no line saying
// the other plays.
func TestARefusedReplyLogsNoLineSayingAnotherSourceOfItPlays(t *testing.T) {
	e, _, log := decisionEnv(t, storedFilm{})
	second := uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end)
		SELECT $1, item_id, 'second.mkv', size, 'def456', 'mkv', streams, keyframes, video_end FROM media_sources WHERE id = $2`, second, e.item)
	mustExec(t, e.pool, `UPDATE media_sources SET streams = jsonb_set(streams, '{0,Codec}', '"hevc"') WHERE id = $1`, second)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources}', (dto->'MediaSources') || jsonb_build_array((dto->'MediaSources'->0) || jsonb_build_object('Id', $1::text)))`, db.CompactID(second))
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")
	before := series(t)
	reply, lines, _ := e.decide(t, log, infoRequest{body: map[string]any{"DeviceProfile": chromeProfile(t, nil), "AudioStreamIndex": "1"}})
	if reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("the second source needs a transcode and no worker runs: %v", reply)
	}
	shape(t, e, reply, lines, e.item)
	if len(lines) != 1 || lines[0]["method"] != "refused" || lines[0]["source"] != second {
		t.Errorf("want one refused line for the second source %s: %v", second, lines)
	}
	if moved := delta(before, series(t)); moved[dec("direct_play")] != 0 || moved[refusal("NoWorkerRunning")] != 1 {
		t.Errorf("the playable first source was counted: %v", moved)
	}
}

// Where a source that needed a transcode plays directly instead, the line says direct play, what was asked, and why no
// transcode was given.
func TestADirectPlayThatGaveUpATranscodeSaysWhatWasAskedAndWhy(t *testing.T) {
	for _, c := range []struct {
		name   string
		film   storedFilm
		body   func(t *testing.T, source string) map[string]any
		setup  func(t *testing.T, e *env)
		asked  string
		reason string
	}{
		{"a burn-in taken back for want of a worker", storedFilm{}, func(t *testing.T, source string) map[string]any {
			b := withBody(map[string]any{"SubtitleStreamIndex": "3", "MediaSourceId": source}, func(p map[string]any) { p["SubtitleProfiles"] = []any{} })(t)
			return b
		}, func(t *testing.T, e *env) { mustExec(t, e.pool, "TRUNCATE transcode_nodes") }, "reencode", "NoWorkerRunning"},
		{"transcoding off", hevcFilm, func(t *testing.T, source string) map[string]any {
			b := chrome(t)
			b["MediaSourceId"] = source
			return b
		}, func(t *testing.T, e *env) { e.apis[0].Transcode = nil; mustExec(t, e.pool, "TRUNCATE transcode_nodes") }, "reencode", "NoTranscodeStore"},
		{"a film with no probed duration", hevcFilm, func(t *testing.T, source string) map[string]any {
			b := chrome(t)
			b["MediaSourceId"] = source
			return b
		}, func(t *testing.T, e *env) { mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 0") }, "reencode", "NoProbedDuration"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, source, log := decisionEnv(t, c.film)
			c.setup(t, e)
			before := series(t)
			reply, lines, _ := e.decide(t, log, infoRequest{body: c.body(t, source)})
			shape(t, e, reply, lines, e.item)
			line := lines[0]
			if line["method"] != "direct_play" || line["asked"] != c.asked {
				t.Errorf("want a direct play that asked for a %s: %v", c.asked, line)
			}
			if got := line.reasonList("reasons"); len(got) != 1 || got[0] != c.reason {
				t.Errorf("reasons %q, want exactly %s", line["reasons"], c.reason)
			}
			if len(line.reasonList("asked_reasons")) == 0 {
				t.Errorf("the line does not show what the transcode asked was for: %v", line)
			}
			moved := delta(before, series(t))
			if moved[dec("direct_play")] != 1 || moved[counted("direct_play", c.reason)] != 1 {
				t.Errorf("the counters moved %v, want direct_play and its reason %s by 1", moved, c.reason)
			}
		})
	}
}

// No decision line carries a title, a file name or a path: not of the film, the folder, the file, an audio track titled as the
// film, a subtitle file beside it, nor the stored error of a remembered failure that names the file.
func TestNoDecisionLineCarriesATitleAFileNameOrAPath(t *testing.T) {
	// An HEVC film, as the refusal checks use, so a burn-in that cannot start is refused rather than taken back to the file's
	// own direct play (lead's correction of the implementer's dispute: the H.264 film played directly).
	e, source, log := decisionEnv(t, storedFilm{
		video: hevcFilm.video,
		audio: []map[string]any{{"Index": 1, "Type": "Audio", "Codec": "aac", "IsDefault": true, "Channels": 2, "Title": "Distinct Title Marker", "Language": "eng"}, {"Index": 2, "Type": "Audio", "Codec": "aac", "Channels": 2}},
		subs:  []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true, "IsExternal": true, "Title": "Distinct Title Marker", "Path": "Distinct Folder/Distinct File.en.srt"}},
	})
	mustExec(t, e.pool, `UPDATE items SET name = 'Distinct Title Marker', dto = jsonb_set(dto, '{Name}', '"Distinct Title Marker"')`)
	mustExec(t, e.pool, `UPDATE media_sources SET store_key = 'Distinct Folder/Distinct File.mkv'`)
	hevc := func() map[string]any {
		b := withBody(map[string]any{"SubtitleStreamIndex": "3", "MediaSourceId": source}, func(p map[string]any) { p["SubtitleProfiles"] = []any{} })(t)
		return b
	}
	var all []decisionLine
	for _, step := range []func(){
		func() { // A re-encode that burns the file beside the film.
			reply, lines, _ := e.decide(t, log, infoRequest{body: hevc()})
			shape(t, e, reply, lines, e.item)
			all = append(all, lines...)
			// Then the same spec fails, and a later request is refused for it.
			hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(reply)["TranscodingUrl"].(string))))
			mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, ended_at, output_version)
				VALUES ($1, '{}', 'o', 0, 0, 'failed', 'ffmpeg could not read /media/Distinct Folder/Distinct File.mkv (Distinct Title Marker)', now(), `+version+`)`, hash)
		},
		func() {
			reply, lines, _ := e.decide(t, log, infoRequest{body: hevc()})
			if reply["ErrorCode"] != "RateLimitExceeded" {
				t.Fatalf("a remembered failure did not refuse: %v", reply)
			}
			shape(t, e, reply, lines, e.item)
			all = append(all, lines...)
		},
	} {
		step()
	}
	if len(all) != 2 || all[0]["method"] != "reencode" || all[1]["method"] != "refused" {
		t.Fatalf("the controls are missing: %v", all)
	}
	for _, line := range decisionsOf(log.String()) {
		for key, value := range line {
			for _, banned := range []string{"Distinct", "Marker", "srt", ".mkv", "Folder"} {
				if strings.Contains(value, banned) {
					t.Errorf("the line's %s=%q holds %q", key, value, banned)
				}
			}
		}
	}
}

// The three counters keep their labels to method and reason, move by the line, and requests that differ only in account,
// device, client, client version or item move the same series and add none.
func TestTheCountersMoveByTheLineAndCarryNoLabelBeyondMethodAndReason(t *testing.T) {
	e, source, log := decisionEnv(t, storedFilm{audio: ac3})
	ask := func(extra map[string]any, edit func(p map[string]any)) {
		t.Helper()
		b := withBody(extra, edit)(t)
		b["MediaSourceId"] = source
		e.decide(t, log, infoRequest{body: b})
	}
	step := func(what string, want map[string]float64, do func()) {
		t.Helper()
		before := series(t)
		do()
		got := delta(before, series(t))
		if len(got) != len(want) {
			t.Errorf("%s: the counters moved %v, want %v", what, got, want)
			return
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s moved by %v, want %v (all: %v)", what, k, got[k], v, got)
			}
		}
	}
	step("a direct play", map[string]float64{dec("direct_play"): 1}, func() {
		e.decide(t, log, infoRequest{body: map[string]any{"MediaSourceId": source}}) // No profile: the file as it is.
	})
	step("a copy with one reason", map[string]float64{dec("copy"): 1, counted("copy", "AudioCodecNotSupported"): 1}, func() { ask(nil, nil) })
	step("a re-encode with two reasons: both reasons, one decision",
		map[string]float64{dec("reencode"): 1, counted("reencode", "AudioCodecNotSupported"): 1, counted("reencode", "ContainerBitrateExceedsLimit"): 1},
		func() { ask(map[string]any{"MaxStreamingBitrate": 1000000}, nil) })
	// The same request from other accounts, devices, clients, versions, and the other replica: the same series, no new one.
	keys := func() map[string]bool {
		out := map[string]bool{}
		for k := range series(t) {
			out[k] = true
		}
		return out
	}
	known := keys()
	other, otherToken := e.addUser(t, "someone-else")
	_ = other
	for i, who := range []string{"Client=\"Other One\", Device=\"Phone\", DeviceId=\"d-%d\", Version=\"1.%d\"", "Client=\"Second\", Device=\"TV\", DeviceId=\"d-%d\", Version=\"2.%d\""} {
		b := withBody(map[string]any{"MediaSourceId": source}, nil)(t)
		header := "MediaBrowser " + fmt.Sprintf(who, i, i) + `, Token="` + otherToken + `"`
		e.do(t, i, "POST", "/Items/"+e.item+"/PlaybackInfo", "", b, "Authorization", header)
	}
	for key := range keys() {
		if !known[key] {
			t.Errorf("a request that differs only in who asked added the series %s", key)
		}
	}
	// Every label there is is method or reason, and every value is a name of the vocabulary.
	for key := range series(t) {
		_, labels, found := strings.Cut(key, "{")
		if !found {
			continue
		}
		labels = strings.TrimSuffix(labels, "}")
		for _, pair := range strings.Split(labels, ",") {
			name, value, _ := strings.Cut(pair, "=")
			if name != "method" && name != "reason" {
				t.Errorf("%s carries the label %s", key, name)
			}
			if name == "reason" && !knownReason(value) {
				t.Errorf("%s: %q is no name of the vocabulary", key, value)
			}
			if name == "method" && value != "direct_play" && value != "copy" && value != "reencode" {
				t.Errorf("%s: method %q", key, value)
			}
		}
	}
}

// The names the kit pins.
func knownReason(name string) bool {
	for _, known := range strings.Fields(`ContainerNotSupported VideoCodecNotSupported AudioCodecNotSupported SubtitleCodecNotSupported
		SecondaryAudioNotSupported VideoProfileNotSupported VideoLevelNotSupported VideoResolutionNotSupported InterlacedVideoNotSupported
		AudioChannelsNotSupported ContainerBitrateExceedsLimit VideoRangeTypeNotSupported KeyframeIndexUnusable BitrateUnknownUnderCap
		VideoRangeNotCopyable VideoRangeUnknown NoWorkerRunning NoWorkerWithFreeCores TranscodeStoreMismatch RememberedFailure
		WorkersAtCopyBound NoWorkerOfThisOutputVersion NoTranscodeStore NoProbedDuration`) {
		if name == known {
			return true
		}
	}
	return false
}
