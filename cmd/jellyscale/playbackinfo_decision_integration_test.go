//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
)

// JS-153: the decision line and its counters, with a real api process: the line is read from the process's own log and the
// counters over HTTP from its /metrics (the kit of internal/api's checks, decision_kit_integration_test.go, pins the wording:
// the message "playback decision", the keys method, reasons, play_session..., and the counters
// jellyscale_playbackinfo_{decisions,refusals,decision_reasons}_total). Each case is one PlaybackInfo, and asserts that it adds
// exactly one line, with its method and reasons, and that the counters rose by that line and nothing else: its method's
// series and each of its reasons' series by one. The api process is the signInSite's (one replica with a metrics listener),
// with a transcodeStore; the worker is a row in transcode_nodes, as the in-process checks make it, because what is judged here
// is the api's answer and its line, not an ffmpeg.

const decisionLineMsg = "playback decision"

// piFilm is one film's stored row.
type piFilm struct {
	video     map[string]any
	audio     string // the default track's codec; "" is aac
	container string
	keyframes string // "" is evenly spaced, "none" is no index
	bitrate   int
	subs      []map[string]any
}

func (f piFilm) streams() string {
	video := map[string]any{"Index": 0, "Type": "Video", "Codec": "h264", "Profile": "Main", "Level": 41, "VideoRangeType": "SDR", "Width": 1280, "Height": 720, "AverageFrameRate": 24}
	for k, v := range f.video {
		video[k] = v
	}
	audio := f.audio
	if audio == "" {
		audio = "aac"
	}
	subs := f.subs
	if subs == nil {
		subs = []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true}}
	}
	all := []map[string]any{video, {"Index": 1, "Type": "Audio", "Codec": audio, "IsDefault": true, "Channels": 2}, {"Index": 2, "Type": "Audio", "Codec": "aac", "Channels": 2}}
	raw, _ := json.Marshal(append(all, subs...))
	return string(raw)
}

type piSite struct {
	t      *testing.T
	site   *signInSite
	api    *proc
	token  string
	user   string
	repo   *items.Repo
	chrome map[string]any
}

func newPlaybackInfoSite(t *testing.T) *piSite {
	t.Helper()
	dir := t.TempDir()
	site := newSignInSite(t, 1, map[string]any{"transcodeStore": "file://" + dir + "/segments", "scanInterval": "1h"})
	s := &piSite{t: t, site: site, api: site.apis[0]}
	ctx := context.Background()
	for _, table := range []string{"transcode_jobs", "transcode_nodes", "libraries"} {
		if _, err := site.pool.Exec(ctx, "TRUNCATE "+table+" CASCADE"); err != nil {
			t.Fatal(err)
		}
	}
	// The processes' own rows (the api registers itself) are gone; the worker is a row.
	s.token = s.api.signInAs(t, "probe-dev-1")
	if err := site.pool.QueryRow(ctx, "SELECT id::text FROM users WHERE name = $1", adminUser).Scan(&s.user); err != nil {
		t.Fatal(err)
	}
	if _, err := site.pool.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'L', 'movies', 'file://`+dir+`', 'file://`+dir+`')`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	s.worker(0)
	s.repo = &items.Repo{Pool: site.pool, ServerID: "srv"}
	raw, err := os.ReadFile("../../compat/captures/transcode-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct{ DeviceProfile map[string]any }
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	s.chrome = capture.DeviceProfile
	return s
}

// worker is the one transcode worker, with the cores it has (0 is plenty), and seen just now.
func (s *piSite) worker(cores float64) {
	s.t.Helper()
	ctx := context.Background()
	if _, err := s.site.pool.Exec(ctx, "TRUNCATE transcode_nodes"); err != nil {
		s.t.Fatal(err)
	}
	if cores == 0 {
		cores = 8
	}
	if _, err := s.site.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('w1', 'worker', 2, $1, $2)", cores, playback.OutputVersion); err != nil {
		s.t.Fatal(err)
	}
}

// film stores a film and returns its id.
func (s *piSite) film(f piFilm) string {
	s.t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	container := f.container
	if container == "" {
		container = "mkv"
	}
	bitrate := f.bitrate
	if bitrate == 0 {
		bitrate = 2000000
	}
	keyframes := "'{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}'"
	if f.keyframes == "none" {
		keyframes = "NULL"
	}
	var library string
	if err := s.site.pool.QueryRow(ctx, "SELECT id::text FROM libraries LIMIT 1").Scan(&library); err != nil {
		s.t.Fatal(err)
	}
	dto := fmt.Sprintf(`{"Name":"Film","MediaSources":[{"Id":%q,"Protocol":"File","Container":%q,"SupportsTranscoding":true,"SupportsDirectPlay":true,"SupportsDirectStream":true,"Bitrate":%d,"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","IsDefault":true}]}]}`,
		db.CompactID(id), container, bitrate)
	for _, sql := range []string{
		fmt.Sprintf(`INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto) VALUES ('%s', '%s', 'Movie', 'Film', 'film', 'film-%s', 900000000, $1)`, id, library, id),
		fmt.Sprintf(`INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end) VALUES ('%s', '%s', 'film.mkv', 25000000, 'abc123', '%s', $1, %s, 900000000)`, id, id, container, keyframes),
	} {
		arg := dto
		if strings.Contains(sql, "media_sources") {
			arg = f.streams()
		}
		if _, err := s.site.pool.Exec(ctx, sql, arg); err != nil {
			s.t.Fatal(err)
		}
	}
	return id
}

func (s *piSite) specHash(item string, reply map[string]any) string {
	s.t.Helper()
	sources, _ := reply["MediaSources"].([]any)
	if len(sources) == 0 {
		s.t.Fatalf("no source to take the spec of: %v", reply)
	}
	parsed, err := url.Parse(sources[0].(map[string]any)["TranscodingUrl"].(string))
	if err != nil {
		s.t.Fatal(err)
	}
	values := url.Values{}
	for k, v := range parsed.Query() {
		values[strings.ToLower(k)] = v
	}
	stored, found, err := s.repo.Source(context.Background(), item, values.Get("mediasourceid"))
	if err != nil || !found {
		s.t.Fatal(err, found)
	}
	media := playback.Source{ID: db.CompactID(stored.ID), ETag: stored.ETag, Container: stored.Container, Size: stored.Size, RunTimeTicks: stored.RunTimeTicks, Streams: stored.Streams}
	params, err := playback.ParseParams(values, media)
	if err != nil {
		s.t.Fatal(err)
	}
	spec, err := playback.NewSpec(item, params, media)
	if err != nil {
		s.t.Fatal(err)
	}
	return spec.Hash()
}

// ask is one PlaybackInfo at the api process.
func (s *piSite) ask(item string, body map[string]any) (map[string]any, int) {
	s.t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequest("POST", s.api.url("/Items/"+item+"/PlaybackInfo"), bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="Probe Client", Device="Probe Device", DeviceId="probe-dev-1", Version="9.9.9", Token="%s"`, s.token))
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	var reply map[string]any
	_ = json.Unmarshal(data, &reply)
	return reply, response.StatusCode
}

func playbackSeries(series map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range series {
		if strings.HasPrefix(k, "jellyscale_playbackinfo_") {
			out[k] = v
		}
	}
	return out
}

func piNames(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
}

// one asks and asserts what a case of the task asserts: exactly one new line, its method and reasons, and the counters
// moved by exactly that. It returns the line's method and reasons.
func (s *piSite) one(what string, item string, body map[string]any, wantMethod string, wantReasons ...string) (string, []string) {
	s.t.Helper()
	// The worker is alive as of now: a row that is not seen for a few seconds is no worker, and a case may wait.
	if _, err := s.site.pool.Exec(context.Background(), "UPDATE transcode_nodes SET seen_at = now()"); err != nil {
		s.t.Fatal(err)
	}
	before := len(logLines(s.api, decisionLineMsg))
	counters := playbackSeries(s.site.scrape(s.api))
	reply, status := s.ask(item, body)
	if status != 200 {
		s.t.Fatalf("%s: PlaybackInfo %d %v", what, status, reply)
	}
	// The reply is what it was: the case reaches the method it is about (checked before the line, which a build may not have yet).
	replyMethod := "direct_play"
	if sources, _ := reply["MediaSources"].([]any); len(sources) == 0 {
		replyMethod = "refused"
	} else if u, offered := sources[0].(map[string]any)["TranscodingUrl"].(string); offered {
		replyMethod = "reencode"
		if strings.Contains(u, "VideoCodec=copy") {
			replyMethod = "copy"
		}
	}
	if replyMethod != wantMethod {
		s.t.Fatalf("%s: the reply is a %s, the case wants a %s: %v", what, replyMethod, wantMethod, reply)
	}
	var lines []string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) { // The log reaches the test through a copying goroutine: a hang bound.
		if lines = logLines(s.api, decisionLineMsg); len(lines) > before || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(300 * time.Millisecond) // So that a second line the build writes is seen, not raced.
	lines = logLines(s.api, decisionLineMsg)[before:]
	if len(lines) != 1 {
		s.t.Fatalf("%s: %d decision lines, want 1:\n%s", what, len(lines), s.api.log.String())
	}
	line := lines[0]
	method, _ := field(line, "method")
	reasonText, _ := field(line, "reasons")
	reasons := piNames(reasonText)
	if method != wantMethod {
		s.t.Errorf("%s: method %q, want %q: %s", what, method, wantMethod, line)
	}
	for _, r := range wantReasons {
		if !strings.Contains(" "+strings.Join(reasons, " ")+" ", " "+r+" ") {
			s.t.Errorf("%s: reasons %q lack %s", what, reasonText, r)
		}
	}
	if got, _ := field(line, "play_session"); got != reply["PlaySessionId"] {
		s.t.Errorf("%s: play_session %q, the reply's %v", what, got, reply["PlaySessionId"])
	}
	if !strings.Contains(line, "level=INFO") {
		s.t.Errorf("%s: not an Info line: %s", what, line)
	}
	if got, _ := field(line, "user"); got != s.user {
		s.t.Errorf("%s: user %q, want the account's id %s", what, got, s.user)
	}
	want := map[string]float64{}
	if method == "refused" {
		want[fmt.Sprintf(`jellyscale_playbackinfo_refusals_total{reason=%q}`, strings.Join(reasons, ""))] = 1
		if len(reasons) != 1 {
			s.t.Errorf("%s: a refused line carries %q, want one reason", what, reasonText)
		}
	} else {
		want[fmt.Sprintf(`jellyscale_playbackinfo_decisions_total{method=%q}`, method)] = 1
		for _, r := range reasons {
			want[fmt.Sprintf(`jellyscale_playbackinfo_decision_reasons_total{method=%q,reason=%q}`, method, r)] = 1
		}
	}
	after := playbackSeries(s.site.scrape(s.api))
	moved := map[string]float64{}
	for k, v := range after {
		if v != counters[k] {
			moved[k] = v - counters[k]
		}
	}
	if len(moved) != len(want) {
		s.t.Errorf("%s: the counters moved %v, want exactly %v", what, moved, want)
	} else {
		for k, v := range want {
			if moved[k] != v {
				s.t.Errorf("%s: %s moved by %v, want %v (all %v)", what, k, moved[k], v, moved)
			}
		}
	}
	return method, reasons
}

func (s *piSite) profile(edit func(p map[string]any)) map[string]any {
	raw, _ := json.Marshal(s.chrome)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	if edit != nil {
		edit(p)
	}
	return p
}

func (s *piSite) body(item string, edit func(p map[string]any), extra map[string]any) map[string]any {
	b := map[string]any{"DeviceProfile": s.profile(edit), "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MaxStreamingBitrate": 2147483647, "MediaSourceId": db.CompactID(item)}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func (s *piSite) captured(file string) (map[string]map[string]any, map[string]map[string]any) {
	s.t.Helper()
	raw, err := os.ReadFile("../../compat/captures/" + file)
	if err != nil {
		s.t.Fatal(err)
	}
	var c struct {
		DeviceProfiles map[string]map[string]any
		Files          map[string]struct{ Video map[string]any }
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		s.t.Fatal(err)
	}
	videos := map[string]map[string]any{}
	for name, f := range c.Files {
		videos[name] = f.Video
	}
	return c.DeviceProfiles, videos
}

// A direct play, a copy, each case of the reasons list, a film with no usable keyframe index, a request with two reasons,
// and each of the four named refusals: each is one line with its method and reasons, and raises that method's series and each
// reason's series by exactly one and no other.
func TestAnApiProcessLogsOneDecisionLinePerAnswerAndCountsExactlyItsMethodAndReasons(t *testing.T) {
	s := newPlaybackInfoSite(t)
	ac3 := "ac3"
	capped := func(p map[string]any) { p["MaxStreamingBitrate"] = 1000000 }
	noRequestCap := func(b map[string]any) map[string]any { delete(b, "MaxStreamingBitrate"); return b }

	plain := s.film(piFilm{})
	s.one("a direct play", plain, s.body(plain, nil, nil), "direct_play")
	copied := s.film(piFilm{audio: ac3})
	s.one("a copy", copied, s.body(copied, nil, nil), "copy", "AudioCodecNotSupported")

	// The ranges, from the films Jellyfin 12.4 was asked about (compat/captures): HDR10, HDR10+ and Dolby Vision, each under
	// the profile that lists SDR only, so that the range is among the reasons.
	profiles, hdr := s.captured("hdr-copy-12.4.json")
	for name, rangeType := range map[string]string{"Plain Pq Film (1991)": "HDR10", "Plain Plus Film (1993)": "HDR10Plus"} {
		item := s.film(piFilm{video: hdr[name], audio: ac3})
		b := s.body(item, nil, map[string]any{"MaxStreamingBitrate": 1440987881})
		b["DeviceProfile"] = profiles["sdr"]
		s.one("a "+rangeType+" film the client lacks the range of", item, b, "reencode", "VideoRangeTypeNotSupported")
	}
	profiles, dv := s.captured("dolby-vision-12.4.json")
	item := s.film(piFilm{video: dv["Dolby Eight One Film (1996)"], container: "mp4"})
	b := s.body(item, nil, map[string]any{"MaxStreamingBitrate": 1440987881})
	b["DeviceProfile"] = profiles["sdr"]
	s.one("a Dolby Vision film the client lacks the range of", item, b, "reencode", "VideoRangeTypeNotSupported")

	over := s.film(piFilm{bitrate: 2000000})
	s.one("a file over the client profile's cap", over, noRequestCap(s.body(over, capped, nil)), "reencode", "ContainerBitrateExceedsLimit")
	s.one("a file over the request's cap", over, s.body(over, nil, map[string]any{"MaxStreamingBitrate": 1000000}), "reencode", "ContainerBitrateExceedsLimit")
	image := s.film(piFilm{subs: []map[string]any{{"Index": 3, "Type": "Subtitle", "Codec": "PGSSUB", "IsImageSubtitleStream": true}}})
	s.one("an image subtitle burned in", image, s.body(image, nil, map[string]any{"SubtitleStreamIndex": "3"}), "reencode", "SubtitleCodecNotSupported")
	nokeys := s.film(piFilm{audio: ac3, keyframes: "none"})
	s.one("a film with no usable keyframe index", nokeys, s.body(nokeys, nil, nil), "reencode", "AudioCodecNotSupported", "KeyframeIndexUnusable")
	two := s.film(piFilm{audio: ac3})
	_, reasons := s.one("a request with two reasons", two, s.body(two, nil, map[string]any{"MaxStreamingBitrate": 1000000}), "reencode", "AudioCodecNotSupported", "ContainerBitrateExceedsLimit")
	if len(reasons) < 2 {
		t.Errorf("two reasons: the line has %v", reasons)
	}

	// The four named refusals, on an HEVC film this client re-encodes.
	hevc := s.film(piFilm{video: map[string]any{"Codec": "hevc", "Level": 93}})
	hevcBody := func() map[string]any { return s.body(hevc, nil, nil) }
	if _, err := s.site.pool.Exec(context.Background(), "UPDATE transcode_nodes SET seen_at = now()"); err != nil {
		t.Fatal(err)
	}
	reply, _ := s.ask(hevc, hevcBody()) // Healthy: its spec is the one that will have failed.
	hash := s.specHash(hevc, reply)
	pool := s.site.pool
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("TRUNCATE transcode_nodes")
	s.one("no worker running", hevc, hevcBody(), "refused", "NoWorkerRunning")
	s.worker(0.04)
	s.one("no worker with the cores free", hevc, hevcBody(), "refused", "NoWorkerWithFreeCores")
	s.worker(0)
	exec("INSERT INTO transcode_nodes (id, roles, store, output_version) VALUES ('other', 'api,stream', 'file:///elsewhere', $1)", playback.OutputVersion)
	exec("UPDATE transcode_nodes SET store = 's3://bucket/segments?endpoint=http://s3' WHERE id = 'w1'")
	s.one("a transcode store mismatch", hevc, hevcBody(), "refused", "TranscodeStoreMismatch")
	s.worker(0)
	exec(`INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, ended_at, output_version)
		VALUES ($1, '{}', 'o', 0, 0, 'failed', 'ffmpeg could not read /media/Some Folder/Some File.mkv', now(), $2)`, hash, playback.OutputVersion)
	s.one("a remembered failure", hevc, hevcBody(), "refused", "RememberedFailure")
	if text := s.api.log.String(); len(logLines(s.api, decisionLineMsg)) == 0 || strings.Contains(strings.Join(logLines(s.api, decisionLineMsg), "\n"), "Some File") {
		t.Errorf("a decision line names the failed file, or there is none:\n%s", text)
	}
}
