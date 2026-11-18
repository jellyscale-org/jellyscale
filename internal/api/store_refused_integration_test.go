//go:build integration

package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-107: a store that refuses is not Postgres being away. The transcode package's checks (job_end_*) hold
// what a job ends as; these hold what the viewer and the log see. They use one name the task implies and the
// build must add: the field SegmentsURI of transcode.Worker and transcode.Jobs, the URI of the transcode store.

// refusingStore refuses what it is told to with the error of a connection that was refused.
type refusingStore struct {
	store.Store
	puts, reads atomic.Bool
}

func refused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, Err: syscall.ECONNREFUSED}
}

func (s *refusingStore) Put(ctx context.Context, key string, body io.Reader) error {
	if s.puts.Load() {
		return refused()
	}
	return s.Store.Put(ctx, key, body)
}

func (s *refusingStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if s.reads.Load() {
		return nil, refused()
	}
	return s.Store.Open(ctx, key, offset)
}

func (s *refusingStore) Stat(ctx context.Context, key string) (int64, error) {
	if s.reads.Load() {
		return 0, refused()
	}
	return s.Store.Stat(ctx, key)
}

func (s *refusingStore) LocalPath(string) (string, bool) { return "", false }

const refusedStoreURI = "s3://transcode-bucket/segments?endpoint=http://store.invalid"

// The worker's put is refused by the transcode store while Postgres answers. The refusal is not the file's, so
// the request waiting on the job is served by fresh jobs (JS-088), three at most, and then, as the cause
// persists, answered 500 and its log line gives the job's error with the store's URI; a request within the 30
// seconds the failure is remembered is answered 500 and starts no job; PlaybackInfo answers RateLimitExceeded
// with no sources and logs the same error; no ffmpeg is left, and no process took Postgres for away.
func TestARefusedPutOfTheTranscodeStoreIsA500AndARefusalThatNameTheStore(t *testing.T) {
	ffmpeg := testenv.FFmpeg(t)
	e, _, source := transcodeEnv(t, "hevc", "ac3")
	film := filepath.Join(strings.TrimPrefix(e.uri, "file://"), "film.mkv")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=60", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", film).CombinedOutput(); err != nil {
		t.Fatalf("making the film: %v\n%s", err, out)
	}
	info, _ := os.Stat(film)
	mustExec(t, e.pool, "UPDATE media_sources SET size = $1", info.Size())
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 600000000")
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")

	segments := store.NewPosix(t.TempDir())
	refusing := &refusingStore{Store: segments}
	refusing.puts.Store(true)
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	scratch := t.TempDir()
	var workerLog, replicaLog, otherLog syncBuffer
	w := &transcode.Worker{ID: "worker-" + uuid.NewString()[:8], Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: refusing, SegmentsURI: refusedStoreURI,
		Log: slog.New(slog.NewTextHandler(&workerLog, nil)), Media: media.Open, Scratch: scratch, Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	w.Hub = &transcode.Hub{Pool: pool, Log: w.Log}
	done := make(chan struct{}, 3)
	for _, run := range []func(){func() { w.Hub.Run(ctx) }, func() { w.Present(ctx, "worker", refusedStoreURI, 200*time.Millisecond) }, func() { w.Run(ctx) }} {
		go func() { run(); done <- struct{}{} }()
	}
	t.Cleanup(func() {
		cancel()
		for range 3 {
			<-done
		}
		pool.Close()
	})
	logs := []*syncBuffer{&replicaLog, &otherLog}
	for i, s := range e.apis {
		hub := &transcode.Hub{Pool: s.Pool, Log: w.Log}
		go hub.Run(ctx)
		s.Log = slog.New(slog.NewTextHandler(logs[i], nil))
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments, SegmentsURI: refusedStoreURI, Hub: hub, QueueTimeout: 10 * time.Second, WaitTimeout: 60 * time.Second, Log: s.Log}
	}
	if !eventually(func() bool { return scalar[int](t, e.pool, "SELECT count(*) FROM transcode_nodes") > 0 }) {
		t.Fatal("the worker never registered")
	}

	body := map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source}
	reply := e.playbackInfo(t, 0, e.item, body)
	if sources, _ := reply["MediaSources"].([]any); len(sources) == 0 {
		t.Fatalf("PlaybackInfo offered no source: %v", reply)
	}
	master := firstSource(reply)["TranscodingUrl"].(string)
	e.play(t, reply, "Transcode")
	_, masterBody := e.do(t, 0, "GET", master, "", nil)
	path := "/videos/" + db.DashedID(e.item)
	main := path + "/main.m3u8?" + strings.TrimSpace(strings.SplitN(string(masterBody), "main.m3u8?", 2)[1])
	_, playlist := e.do(t, 1, "GET", main, "", nil)
	var entries []string
	for _, line := range strings.Split(string(playlist), "\n") {
		if strings.HasPrefix(line, "hls1/main/") && strings.Contains(line, ".mp4?") {
			entries = append(entries, path+"/"+line)
		}
	}
	if len(entries) < 3 {
		t.Fatalf("the playlist lists %d segments:\n%s", len(entries), playlist)
	}

	// The request that waits on the job whose put is refused.
	if response, text := e.do(t, 1, "GET", entries[0], "", nil); response.StatusCode != 500 {
		t.Fatalf("the segment request waiting on the failed jobs was answered %d %s, want 500", response.StatusCode, text)
	}
	if states := scalar[string](t, e.pool, "SELECT string_agg(state, ',' ORDER BY id) FROM transcode_jobs"); states != "failed,failed,failed,failed" {
		t.Fatalf("the jobs ended %q, want the first job and its three fresh ones, failed, never cancelled", states)
	}
	if reason := scalar[string](t, e.pool, "SELECT error FROM transcode_jobs"); !strings.Contains(reason, refusedStoreURI) {
		t.Errorf("transcode_jobs.error is %q, want it to hold %s", reason, refusedStoreURI)
	}
	// The waiting request went to replica 1, so its line is in either log, as the refusal's below.
	if line := lineWith(otherLog.String()+replicaLog.String(), "transcode failed"); !strings.Contains(line, refusedStoreURI) {
		t.Errorf("the log line of the failed request %q does not hold %s:\n%s", line, refusedStoreURI, otherLog.String()+replicaLog.String())
	}

	// Within the failure memory: 500, and no job is started.
	if response, text := e.do(t, 0, "GET", entries[2], "", nil); response.StatusCode != 500 {
		t.Errorf("a segment request within the failure memory was answered %d %s, want 500", response.StatusCode, text)
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_jobs"); n != 4 {
		t.Errorf("%d jobs exist after a request within the failure memory, want the four that failed", n)
	}
	// PlaybackInfo for the spec refuses, and says why.
	again := e.playbackInfo(t, 0, e.item, body)
	if sources, _ := again["MediaSources"].([]any); again["ErrorCode"] != "RateLimitExceeded" || len(sources) != 0 {
		t.Errorf("PlaybackInfo within the failure memory answered %v, want RateLimitExceeded with no sources", again)
	}
	if line := lineWith(otherLog.String()+replicaLog.String(), "refusing to offer a transcode"); !strings.Contains(line, refusedStoreURI) {
		t.Errorf("the refusal's log line %q does not hold %s", line, refusedStoreURI)
	}

	if !eventually(func() bool { return len(ffmpegsWriting(scratch)) == 0 }) {
		t.Errorf("ffmpeg is left running: %v", ffmpegsWriting(scratch))
	}
	for who, log := range map[string]*syncBuffer{"the worker": &workerLog, "replica 0": &replicaLog, "replica 1": &otherLog} {
		if strings.Contains(strings.ToLower(log.String()), "postgres is away") {
			t.Errorf("%s logged that Postgres is away, which answered the whole time", who)
		}
	}
	for who, pool := range map[string]*pgxpool.Pool{"the worker": pool, "replica 0": e.apis[0].Pool, "replica 1": e.apis[1].Pool} {
		if total := db.Of(pool).Total(); total != 0 {
			t.Errorf("%s took Postgres for away for %v", who, total)
		}
	}
}

// lineWith is the last line of the log that holds text, "" if none.
func lineWith(log, text string) string {
	lines := strings.Split(log, "\n")
	slices.Reverse(lines)
	for _, line := range lines {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}

// ffmpegsWriting lists the ffmpeg command lines that write into the directory.
func ffmpegsWriting(dir string) []string {
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, entry := range entries {
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if filepath.Base(args[0]) == "ffmpeg" && strings.Contains(string(raw), dir) {
			found = append(found, strings.Join(args, " "))
		}
	}
	return found
}

// newRefusingReplica is a stream replica, behind a proxy of its own, whose reads of the transcode
// store the test refuses.
func newRefusingReplica(t *testing.T, e *env, segments store.Store) (*replica, *refusingStore, *pgxpool.Pool) {
	t.Helper()
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	pool, err := db.Open(context.Background(), proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	refusing := &refusingStore{Store: segments}
	rp := &replica{role: RoleStream, proxy: proxy, log: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(rp.log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleStream}, Log: log, SocketIdle: time.Minute}
	s.Transcode = &transcode.Jobs{Pool: pool, Segments: refusing, SegmentsURI: refusedStoreURI, QueueTimeout: 2 * time.Second, Log: log}
	rp.server = httptest.NewServer(s.Handler())
	t.Cleanup(rp.server.Close)
	return rp, refusing, pool
}

// A stream replica whose own read of the transcode store is refused, with Postgres answering, answers the
// segment 500 at once with the store's URI in the logged error, and does not take Postgres for away. While
// Postgres is away the same request waits for Postgres and logs nothing, as DOMAINS documents.
func TestAStreamReplicasRefusedReadOfTheTranscodeStoreIsA500AndWaitsWhenPostgresIsAway(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	p.playing(t, 0)
	rp, refusing, pool := newRefusingReplica(t, p.env, p.segments)
	film := p.films[1]
	segment := p.as(film, film.segment[2], 0)
	refreshed(refresh)

	// Postgres answers, the store does not.
	refusing.reads.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), answerDeadline)
	a := get(ctx, rp.server.URL, segment)
	cancel()
	if a.err != nil || a.status != 500 {
		t.Fatalf("the segment was answered %d %v within %v, want 500 without waiting", a.status, a.err, answerDeadline)
	}
	if line := lineWith(rp.log.String(), refusedStoreURI); line == "" {
		t.Errorf("the replica's log does not hold %s:\n%s", refusedStoreURI, rp.log.String())
	}
	if strings.Contains(strings.ToLower(rp.log.String()), "postgres is away") || db.Of(pool).Total() != 0 {
		t.Errorf("the replica took Postgres for away because a store refused:\n%s", rp.log.String())
	}

	// Postgres away: the request waits, and the log says nothing of the store.
	logged := len(rp.log.String())
	rp.away(t, p.pool)
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	waiting := ask(ctx, rp, served{path: segment})
	time.Sleep(3 * time.Second)
	select {
	case a := <-waiting:
		t.Fatalf("the segment was answered %d %v while Postgres was away", a.status, a.err)
	default:
	}
	if during := rp.log.String()[logged:]; strings.Contains(during, refusedStoreURI) || strings.Contains(during, "level=ERROR") {
		t.Errorf("the replica logged while it waited for Postgres:\n%s", during)
	}
	holdAndCheck(t, 0, []pending{{what: "the segment asked for while Postgres was away", want: 500, ch: waiting}}, func() { rp.proxy.Back() })
}
