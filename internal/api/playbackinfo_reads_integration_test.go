//go:build integration

package api

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// JS-111: one PlaybackInfo reads a source once, writes nothing when nothing
// needs writing, and counts its decisions once per source it lists. These
// checks count what the replica's pool sends, through a pgx tracer, and the
// jellyscale_playbackinfo_* counters; no check looks at how long anything took.
//
// "A statement that names media_sources" is any statement whose text holds the
// word, whether it reads or writes (shown_items and the item's DTO are another
// table). "At most one per source" is counted against the sources the request
// asks for: all of the item's, or the one MediaSourceId names. A refusal lists
// none, but it has read the sources it was refusing, so it is held to the same
// bound.

// statementLog is a pgx tracer that keeps the text of every statement a pool
// sends and hands the pool's own tracer (the outage tracker) what it saw too.
type statementLog struct {
	inner pgx.QueryTracer
	mu    sync.Mutex
	sent  []string
}

func (l *statementLog) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	l.mu.Lock()
	l.sent = append(l.sent, data.SQL)
	l.mu.Unlock()
	if l.inner != nil {
		return l.inner.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}

func (l *statementLog) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if l.inner != nil {
		l.inner.TraceQueryEnd(ctx, conn, data)
	}
}

// take returns the statements sent since the last take.
func (l *statementLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	sent := l.sent
	l.sent = nil
	return sent
}

// traceReplicas gives every api replica a pool whose statements are logged,
// keeping its Items and Transcode (as they are now, nil or not) on it.
func traceReplicas(t *testing.T, e *env) []*statementLog {
	t.Helper()
	var logs []*statementLog
	for _, s := range e.apis {
		log := &statementLog{}
		cfg := s.Pool.Config()
		log.inner, _ = cfg.ConnConfig.Tracer.(pgx.QueryTracer)
		cfg.ConnConfig.Tracer = log
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		s.Pool = pool
		repo := *s.Items
		repo.Pool = pool
		s.Items = &repo
		if s.Transcode != nil {
			jobs := *s.Transcode
			jobs.Pool = pool
			s.Transcode = &jobs
		}
		logs = append(logs, log)
	}
	return logs
}

var writes = regexp.MustCompile(`(?i)\b(delete|update)\b`)

// namesSource is true of a statement that reads a source's row. The list of an item's sources reads the file's audio
// tracks with them (JS-121), from the same row of the item, and is not a read of a source.
func namesSource(sql string) bool {
	sql = strings.ToLower(sql)
	return strings.Contains(sql, "media_sources") && !strings.HasPrefix(sql, "select coalesce(dto->'mediasources'")
}

func count(statements []string, match func(string) bool) (n int) {
	for _, sql := range statements {
		if match(sql) {
			n++
		}
	}
	return n
}

func oneLine(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// decided is the decisions counter by method.
func decided(t *testing.T) map[string]float64 {
	t.Helper()
	metrics := make(chan prometheus.Metric, 16)
	go func() { playbackDecisions.Collect(metrics); close(metrics) }()
	got := map[string]float64{}
	for metric := range metrics {
		var m dto.Metric
		if err := metric.Write(&m); err != nil {
			t.Fatal(err)
		}
		for _, label := range m.GetLabel() {
			if label.GetName() == "method" {
				got[label.GetValue()] += m.GetCounter().GetValue()
			}
		}
	}
	return got
}

// wantDecisions fails unless each method rose by exactly the amount given and
// no other method rose.
func wantDecisions(t *testing.T, what string, before map[string]float64, want map[string]float64) {
	t.Helper()
	after := decided(t)
	for _, method := range []string{"direct_play", "copy", "reencode"} {
		if got := after[method] - before[method]; got != want[method] {
			t.Errorf("%s: %s rose by %v, want %v", what, method, got, want[method])
		}
	}
	for method := range after {
		if _, known := map[string]bool{"direct_play": true, "copy": true, "reencode": true}[method]; !known && after[method] != before[method] {
			t.Errorf("%s: an unknown method %q rose", what, method)
		}
	}
}

const copyKeyframes = "UPDATE media_sources SET keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', video_end = 900000000, size = 25000000"

// withFonts gives the film a subtitle track and a font attachment, in the
// item's DTO and in the stored streams. With stored, the film's fonts manifest
// is in the asset store, as a worker leaves it.
func withFonts(t *testing.T, e *env, stored bool) {
	t.Helper()
	mustExec(t, e.pool, `UPDATE media_sources SET streams = streams || '[{"Index":4,"Type":"Attachment","Codec":"ttf","MimeType":"font/ttf","FileName":"Serif.ttf"}]'::jsonb`)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(jsonb_set(dto, '{MediaSources,0,MediaStreams}',
			'[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","IsDefault":true},{"Index":2,"Type":"Audio"},{"Index":3,"Type":"Subtitle","Codec":"subrip","IsTextSubtitleStream":true}]'),
		'{MediaSources,0,MediaAttachments}', '[{"Index":4,"Codec":"ttf","FileName":"Serif.ttf","MimeType":"font/ttf"}]')`)
	if stored {
		assets, err := store.Open(e.uri)
		if err != nil {
			t.Fatal(err)
		}
		if err := assets.Put(context.Background(), subtitles.FontsKey(e.item, "abc123"), strings.NewReader(`{"4":"abc"}`)); err != nil {
			t.Fatal(err)
		}
	}
}

// secondSource gives the item a second file, with the first's streams.
func secondSource(t *testing.T, e *env) string {
	t.Helper()
	other := uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		SELECT $2::uuid, item_id, 'second.mkv', size, 'def456', container, streams FROM media_sources WHERE id = $1::uuid`, e.item, other)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources,1}', (dto->'MediaSources'->0) || jsonb_build_object('Id', $1::text))`, db.CompactID(other))
	return db.CompactID(other)
}

func profileBody(t *testing.T, source string, extra map[string]any) map[string]any {
	body := transcodeProfileBody(t, source)
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// The branches of the first criterion. want is the method the decisions counter
// rises under, or "refused".
type branch struct {
	name    string
	video   string
	audio   string
	prepare func(t *testing.T, e *env)
	noStore bool // The replica has no transcodeStore.
	body    func(t *testing.T, source string) map[string]any
	want    string
	reads   int // The most statements naming media_sources, for the sources asked for.
}

func profile(t *testing.T, source string) map[string]any { return profileBody(t, source, nil) }

func branches() []branch {
	noWorker := func(t *testing.T, e *env) { mustExec(t, e.pool, "TRUNCATE transcode_nodes") }
	second := func(t *testing.T, source string) map[string]any {
		return profileBody(t, source, map[string]any{"MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "2", "SubtitleStreamIndex": "-1"})
	}
	return []branch{
		{name: "no profile, no transcodeStore", video: "hevc", audio: "aac", noStore: true, want: "direct_play",
			body: func(t *testing.T, s string) map[string]any { return map[string]any{"MediaSourceId": s} }},
		{name: "no profile, with a transcodeStore", video: "hevc", audio: "aac", want: "direct_play",
			body: func(t *testing.T, s string) map[string]any { return map[string]any{"MediaSourceId": s} }},
		{name: "a profile that plays directly", video: "h264", audio: "aac", want: "direct_play", body: profile},
		{name: "re-encode", video: "hevc", audio: "aac", want: "reencode", body: profile},
		{name: "copy", video: "h264", audio: "ac3", want: "copy", body: profile,
			prepare: func(t *testing.T, e *env) {
				mustExec(t, e.pool, copyKeyframes)
				mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
			}},
		{name: "a live job joined", video: "hevc", audio: "aac", want: "reencode", body: profile,
			prepare: func(t *testing.T, e *env) { joinLiveJob(t, e) }},
		{name: "a replica without a transcodeStore and no worker anywhere", video: "hevc", audio: "aac", noStore: true, want: "direct_play",
			body: profile, prepare: noWorker},
		{name: "a refusal", video: "hevc", audio: "aac", want: "refused", body: profile,
			prepare: func(t *testing.T, e *env) {
				mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 minute'")
			}},
		// A pick no worker can deliver is refused, never answered with the default track (D-029; it fell back to it before).
		{name: "a pick no worker can deliver", video: "h264", audio: "aac", want: "refused", body: second, prepare: noWorker},
	}
}

// joinLiveJob puts a running job of the spec of the first PlaybackInfo of the
// film in the table, so the next PlaybackInfo joins it.
func joinLiveJob(t *testing.T, e *env) {
	t.Helper()
	source := db.CompactID(e.item)
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)))["TranscodingUrl"].(string))))
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker, output_version)
		VALUES ($1, '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w1', `+strconv.Itoa(playback.OutputVersion)+`)`, hash)
}

func (b branch) setup(t *testing.T) (*env, string, *statementLog) {
	t.Helper()
	e, _, source := transcodeEnv(t, b.video, b.audio)
	if b.prepare != nil {
		b.prepare(t, e)
	}
	if b.noStore {
		e.apis[0].Transcode = nil
	}
	return e, source, traceReplicas(t, e)[0]
}

// One PlaybackInfo reads the source row once, in every branch, as a POST with a
// profile and as a GET, and a POST is counted once under its method.
func TestPlaybackInfoReadsASourceOnceInEveryBranch(t *testing.T) {
	for _, b := range branches() {
		t.Run(b.name, func(t *testing.T) {
			e, source, log := b.setup(t)
			before := decided(t)
			log.take()
			e.playbackInfo(t, 0, e.item, b.body(t, source))
			statements := log.take()
			if n := count(statements, namesSource); n > 1 {
				t.Errorf("POST: %d statements name media_sources, want at most 1:\n%s", n, listing(statements))
			}
			want := map[string]float64{b.want: 1}
			if b.want == "refused" {
				want = map[string]float64{}
			}
			wantDecisions(t, b.name, before, want)

			response, raw := e.do(t, 0, "GET", "/Items/"+e.item+"/PlaybackInfo?MediaSourceId="+source, e.token, nil,
				"Authorization", strings.Replace(auth, "%s", e.token, 1))
			if response.StatusCode != 200 {
				t.Fatalf("GET: %d %s", response.StatusCode, raw)
			}
			if statements = log.take(); count(statements, namesSource) > 1 {
				t.Errorf("GET: %d statements name media_sources, want at most 1:\n%s", count(statements, namesSource), listing(statements))
			}
		})
	}
}

// The source that costs four reads: fonts, a text subtitle track while no
// worker is alive, a copyable video with a keyframe index, a profile, on a
// replica with a transcodeStore. With a worker alive it costs three.
func TestPlaybackInfoReadsTheCostliestSourceOnce(t *testing.T) {
	for _, alive := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker alive %v", alive), func(t *testing.T) {
			e, _, source := transcodeEnv(t, "h264", "aac")
			mustExec(t, e.pool, copyKeyframes)
			mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
			withFonts(t, e, true)
			if !alive {
				mustExec(t, e.pool, "TRUNCATE transcode_nodes")
			}
			log := traceReplicas(t, e)[0]
			// A second audio track is asked for, which the default does not play:
			// a transcode is wanted, and the keyframe index is read for a copy.
			body := profileBody(t, source, map[string]any{"MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "2", "SubtitleStreamIndex": "-1"})
			before := decided(t)
			reply := e.playbackInfo(t, 0, e.item, body)
			statements := log.take()
			if n := count(statements, namesSource); n > 1 {
				t.Errorf("%d statements name media_sources, want at most 1 (4 before the change without a worker, 3 with one):\n%s", n, listing(statements))
			}
			if alive {
				if got := len(firstSource(reply)["MediaAttachments"].([]any)); got != 1 {
					t.Errorf("the stored font is not listed: %v", firstSource(reply)["MediaAttachments"])
				}
				wantDecisions(t, "a worker alive", before, map[string]float64{"copy": 1})
			} else {
				// The pick of track 2 cannot be delivered, so it is refused, not played as the default (D-029).
				if reply["ErrorCode"] != "RateLimitExceeded" {
					t.Errorf("no worker alive: not refused: %v", reply)
				}
				wantDecisions(t, "no worker alive", before, map[string]float64{})
			}
			if got := count(statements, func(s string) bool { return strings.Contains(s, "keyframes") }); got != 1 {
				t.Errorf("%d statements read the keyframe index, want 1:\n%s", got, listing(statements))
			}
		})
	}
}

// An item whose reply lists two sources reads each once, and one when the
// request names one of them; the decisions rise once for each listed source.
func TestPlaybackInfoOfTwoSourcesReadsEachOnce(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	other := secondSource(t, e)
	log := traceReplicas(t, e)[0]
	for _, c := range []struct {
		name   string
		body   map[string]any
		listed int
	}{
		{"both", map[string]any{"DeviceProfile": capturedProfile(t)}, 2},
		{"one named", profileBody(t, other, nil), 1},
		{"the first named", profileBody(t, source, nil), 1},
	} {
		before := decided(t)
		log.take()
		reply := e.playbackInfo(t, 0, e.item, c.body)
		statements := log.take()
		if got := len(reply["MediaSources"].([]any)); got != c.listed {
			t.Fatalf("%s: %d sources listed, want %d", c.name, got, c.listed)
		}
		if n := count(statements, namesSource); n > c.listed {
			t.Errorf("%s: %d statements name media_sources, want at most %d:\n%s", c.name, n, c.listed, listing(statements))
		}
		wantDecisions(t, c.name, before, map[string]float64{"reencode": float64(c.listed)})
	}
}

// A refusal counts one refusal and no decision; with two sources listed the
// decisions rise by two.
func TestPlaybackInfoRefusalCountsNoDecision(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	secondSource(t, e)
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 minute'")
	before, refusals := decided(t), refusedTotal(t)
	reply := e.playbackInfo(t, 0, e.item, profileBody(t, source, nil))
	if reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("not refused: %v", reply)
	}
	wantDecisions(t, "a refusal", before, map[string]float64{})
	if got := refusedTotal(t); got != refusals+1 {
		t.Errorf("refusals rose by %v, want 1", got-refusals)
	}
}

// A reply that is refused lists no source, so it counts no decision, even when
// a source before the one that refused would have played directly. The first
// source plays (h264), the second needs a transcode no worker can start, and no
// MediaSourceId names either. It has read each source once at most.
func TestPlaybackInfoRefusalAfterAPlayableSourceCountsNoDecision(t *testing.T) {
	e, _, _ := transcodeEnv(t, "hevc", "aac")
	secondSource(t, e)
	mustExec(t, e.pool, "UPDATE media_sources SET streams = replace(streams::text, 'hevc', 'h264')::jsonb WHERE id = $1::uuid", e.item)
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 minute'")
	log := traceReplicas(t, e)[0]
	before, refusals := decided(t), refusedTotal(t)
	log.take()
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t)})
	statements := log.take()
	if reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("not refused: %v", reply)
	}
	if sources, _ := reply["MediaSources"].([]any); len(sources) != 0 {
		t.Errorf("a refusal lists %d sources", len(sources))
	}
	wantDecisions(t, "a refusal after a source that plays", before, map[string]float64{})
	if got := refusedTotal(t); got != refusals+1 {
		t.Errorf("refusals rose by %v, want 1", got-refusals)
	}
	if n := count(statements, namesSource); n > 2 {
		t.Errorf("%d statements name media_sources for two sources, want at most 2:\n%s", n, listing(statements))
	}
}

// No answer comes from an earlier call's read: after the file is replaced, the
// next call on the same replica answers from the new row.
func TestPlaybackInfoAnswersFromTheRowAsItIsNow(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	traceReplicas(t, e)
	first := firstSource(e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)))["TranscodingUrl"].(string)
	if !strings.Contains(first, "Tag=abc123") {
		t.Fatalf("the first answer should carry the first tag: %s", first)
	}
	// The file is replaced by an H.264 one with AC3 audio and a keyframe index.
	mustExec(t, e.pool, "UPDATE media_sources SET object_etag = 'newtag', streams = replace(streams::text, 'hevc', 'h264')::jsonb, container = 'mkv'")
	mustExec(t, e.pool, `UPDATE media_sources SET streams = replace(streams::text, '"aac"', '"ac3"')::jsonb`)
	mustExec(t, e.pool, copyKeyframes)
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
	for replica := range 2 {
		second := firstSource(e.playbackInfo(t, replica, e.item, profileBody(t, source, nil)))
		url, _ := second["TranscodingUrl"].(string)
		if !strings.Contains(url, "Tag=newtag") || strings.Contains(url, "abc123") {
			t.Errorf("replica %d answered from the old row: %s", replica, url)
		}
		if !strings.Contains(url, "AudioCodecNotSupported") || strings.Contains(url, "VideoCodecNotSupported") {
			t.Errorf("replica %d did not decide from the new streams: %s", replica, url)
		}
	}
	// And once more, after a change only the keyframe read sees: with its index
	// dropped the film is no longer copied, and is counted as a re-encode.
	mustExec(t, e.pool, "UPDATE media_sources SET keyframes = '{}'")
	before := decided(t)
	e.playbackInfo(t, 0, e.item, profileBody(t, source, nil))
	wantDecisions(t, "a keyframe index dropped since the last call", before, map[string]float64{"reencode": 1})
}

// With nothing queued and the fonts stored (or none), no statement of any
// branch is a DELETE or an UPDATE, on a replica with a transcodeStore and on
// one without.
func TestPlaybackInfoWritesNothingWhenNothingIsQueued(t *testing.T) {
	for _, b := range branches() {
		for _, fonts := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, fonts %v", b.name, fonts), func(t *testing.T) {
				e, _, source := transcodeEnv(t, b.video, b.audio)
				if b.prepare != nil {
					b.prepare(t, e)
				}
				if fonts {
					withFonts(t, e, true)
				}
				if b.noStore {
					e.apis[0].Transcode = nil
				}
				log := traceReplicas(t, e)[0]
				log.take()
				e.playbackInfo(t, 0, e.item, b.body(t, source))
				for _, sql := range log.take() {
					if writes.MatchString(sql) {
						t.Errorf("a statement with DELETE or UPDATE with nothing queued: %s", oneLine(sql))
					}
				}
			})
		}
	}
}

// An offered transcode writes its reasons in one statement, which is the only
// statement of the request that names transcode_reasons (the prune of old rows
// is the replicas' own, cmd/jellyscale).
func TestPlaybackInfoWritesTheReasonsOnce(t *testing.T) {
	for _, b := range branches() {
		if b.want != "reencode" && b.want != "copy" {
			continue
		}
		t.Run(b.name, func(t *testing.T) {
			e, source, log := b.setup(t)
			log.take()
			e.playbackInfo(t, 0, e.item, b.body(t, source))
			if n := count(log.take(), func(s string) bool { return strings.Contains(s, "transcode_reasons") }); n != 1 {
				t.Errorf("%d statements name transcode_reasons, want 1 for the one source offered", n)
			}
		})
	}
}

// Reasons are written once per offered source: two sources, two rows, and the
// playing session lists them.
func TestPlaybackInfoKeepsReasonsOncePerOfferedSource(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	secondSource(t, e)
	log := traceReplicas(t, e)[0]
	log.take()
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t)})
	inserts := count(log.take(), func(s string) bool {
		return strings.Contains(s, "transcode_reasons") && strings.Contains(strings.ToUpper(s), "INSERT")
	})
	if inserts != 2 {
		t.Errorf("%d statements wrote reasons for two offered sources, want 2", inserts)
	}
	psid := reply["PlaySessionId"].(string)
	if rows := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_reasons WHERE play_session_id = $1", psid); rows != 2 {
		t.Errorf("%d reasons rows for the play session, want 2", rows)
	}
	// One source, played: the session lists its reasons.
	one := e.playbackInfo(t, 0, e.item, profileBody(t, source, nil))
	url := firstSource(one)["TranscodingUrl"].(string)
	spec, hash := specOf(t, e, url)
	e.play(t, one, "Transcode")
	e.startedJob(t, one, spec, hash, "running", 5, "w1")
	info := e.transcodingInfo(t, 1)
	if info == nil || fmt.Sprint(info["TranscodeReasons"]) != fmt.Sprint(anyStrings(transcodeReasons(url))) {
		t.Errorf("the session lists %v, PlaybackInfo gave %v", info, transcodeReasons(url))
	}
}

// A source the reply lists as direct play because the item has no runtime, or
// because its row is gone, is counted as direct_play exactly once.
func TestPlaybackInfoCountsADirectPlayWithoutARuntimeOrRow(t *testing.T) {
	for _, c := range []struct {
		name string
		do   string
	}{
		{"no runtime", "UPDATE items SET run_time_ticks = 0"},
		{"the row is gone", "DELETE FROM media_sources"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _, source := transcodeEnv(t, "hevc", "aac")
			mustExec(t, e.pool, c.do)
			before := decided(t)
			reply := e.playbackInfo(t, 0, e.item, profileBody(t, source, nil))
			if firstSource(reply)["TranscodingUrl"] != nil {
				t.Fatalf("offered a transcode: %v", firstSource(reply))
			}
			wantDecisions(t, c.name, before, map[string]float64{"direct_play": 1})
		})
	}
}

// With a source whose fonts are not stored yet, the one write is the enqueue of
// its fonts job, at most once per source per call.
func TestPlaybackInfoQueuesTheFontsJobAsItsOnlyWrite(t *testing.T) {
	for _, b := range branches() {
		t.Run(b.name, func(t *testing.T) {
			e, _, source := transcodeEnv(t, b.video, b.audio)
			if b.prepare != nil {
				b.prepare(t, e)
			}
			withFonts(t, e, false)
			if b.noStore {
				e.apis[0].Transcode = nil
			}
			log := traceReplicas(t, e)[0]
			log.take()
			e.playbackInfo(t, 0, e.item, b.body(t, source))
			var written []string
			for _, sql := range log.take() {
				if writes.MatchString(sql) {
					written = append(written, oneLine(sql))
				}
			}
			// A refusal is made before the fonts are looked at in no branch: fonts come first.
			if len(written) != 1 || !strings.Contains(written[0], "subtitle_jobs") {
				t.Errorf("writes %q, want only the one enqueue of the fonts job", written)
			}
		})
	}
}

func TestPlaybackInfoQueuesOneFontsJobPerSource(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	withFonts(t, e, false)
	secondSource(t, e)
	mustExec(t, e.pool, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources,1,MediaAttachments}', '[{"Index":4,"Codec":"ttf","FileName":"Serif.ttf","MimeType":"font/ttf"}]')`)
	log := traceReplicas(t, e)[0]
	for _, c := range []struct {
		body    map[string]any
		sources int
	}{{profileBody(t, source, nil), 1}, {map[string]any{"DeviceProfile": capturedProfile(t)}, 2}} {
		mustExec(t, e.pool, "TRUNCATE subtitle_jobs")
		log.take()
		e.playbackInfo(t, 0, e.item, c.body)
		n := 0
		for _, sql := range log.take() {
			if writes.MatchString(sql) && strings.Contains(sql, "subtitle_jobs") {
				n++
			}
		}
		if n > c.sources {
			t.Errorf("%d fonts enqueues for %d sources", n, c.sources)
		}
	}
}

// A job queued longer than queueTimeout is cancelled by the PlaybackInfo that
// needs a transcode and sees it, and is never offered as live; one queued
// within it is left alone.
func TestPlaybackInfoStillCancelsAStaleQueuedJob(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)))["TranscodingUrl"].(string))))
	traceReplicas(t, e)
	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 hour'")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, created_at, output_version)
		VALUES ($1, '{}', 'o', 0, 0, 'queued', now() - interval '1 minute', `+strconv.Itoa(playback.OutputVersion)+`)`, hash)
	if reply := e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)); reply["ErrorCode"] != "RateLimitExceeded" {
		t.Fatalf("a stale queued job was offered: %v", reply)
	}
	state, why := scalar[string](t, e.pool, "SELECT state FROM transcode_jobs WHERE spec_hash = $1", hash),
		scalar[string](t, e.pool, "SELECT COALESCE(error, '') FROM transcode_jobs WHERE spec_hash = $1", hash)
	if state != "cancelled" || why != "no worker took the job" {
		t.Errorf("the stale job is %q (%q), want cancelled with 'no worker took the job'", state, why)
	}

	// Within queueTimeout it is live, whatever the workers: it is not cancelled.
	mustExec(t, e.pool, "TRUNCATE transcode_jobs")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, created_at, output_version)
		VALUES ($1, '{}', 'o', 0, 0, 'queued', now(), `+strconv.Itoa(playback.OutputVersion)+`)`, hash)
	if reply := e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)); reply["ErrorCode"] != nil || firstSource(reply)["TranscodingUrl"] == nil {
		t.Fatalf("a job queued just now was not joined: %v", reply)
	}
	if state := scalar[string](t, e.pool, "SELECT state FROM transcode_jobs WHERE spec_hash = $1", hash); state != "queued" {
		t.Errorf("a job within the queue timeout is %q, want queued", state)
	}
}

// With a stale job queued, the cancelling UPDATE is the one write and it
// happens once, however many sources the reply lists.
func TestPlaybackInfoCancelsAStaleJobOnce(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	secondSource(t, e)
	log := traceReplicas(t, e)[0]
	hash := specHash(t, e, lowerKeys(mustQuery(t, firstSource(e.playbackInfo(t, 0, e.item, profileBody(t, source, nil)))["TranscodingUrl"].(string))))
	mustExec(t, e.pool, "TRUNCATE transcode_jobs")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, created_at, output_version)
		VALUES ($1, '{}', 'o', 0, 0, 'queued', now() - interval '1 minute', `+strconv.Itoa(playback.OutputVersion)+`)`, hash)
	log.take()
	e.playbackInfo(t, 0, e.item, profileBody(t, source, nil))
	cancels := count(log.take(), func(s string) bool { return strings.Contains(s, "transcode_jobs") && writes.MatchString(s) })
	if cancels != 1 {
		t.Errorf("%d statements wrote transcode_jobs, want 1", cancels)
	}
}

func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func listing(statements []string) string {
	var b strings.Builder
	for _, sql := range statements {
		fmt.Fprintf(&b, "  %s\n", oneLine(sql))
	}
	return b.String()
}
