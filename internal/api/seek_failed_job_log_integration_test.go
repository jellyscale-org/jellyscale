//go:build integration

package api

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-152: a segment that truly cannot be made (here a seek into a film whose file ends at 25 %, past which the video has no
// frame) is refused, and each failed job leaves one log line that names the item, the source and
// the reason: 20 refused requests for one failed job, through two stream replicas, leave one such line, not 20.
func TestAFailedJobLeavesOneLogLineHoweverManyRequestsItRefuses(t *testing.T) {
	ffmpeg := testenv.FFmpeg(t)
	e, segments, item := transcodeEnv(t, "hevc", "ac3")
	film := filepath.Join(strings.TrimPrefix(e.uri, "file://"), "film.mkv")
	if out, err := exec.Command(ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=60", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "ac3", film).CombinedOutput(); err != nil {
		t.Fatalf("making the film: %v\n%s", err, out)
	}
	whole, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(film, whole[:len(whole)/4], 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(film)
	// A source id of its own, so that a line naming the item does not name the source by it.
	source := uuid.NewString()
	mustExec(t, e.pool, "UPDATE media_sources SET size = $1, id = $2::uuid", info.Size(), source)
	mustExec(t, e.pool, "UPDATE items SET dto = jsonb_set(dto, '{MediaSources,0,Id}', to_jsonb($1::text))", db.CompactID(source))
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 600000000")
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")

	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	var workerLog, replicaLog, otherLog syncBuffer
	w := &transcode.Worker{ID: "worker-" + uuid.NewString()[:8], Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: segments,
		Log: slog.New(slog.NewTextHandler(&workerLog, nil)), Media: media.Open, Scratch: t.TempDir(), Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	w.Hub = &transcode.Hub{Pool: pool, Log: w.Log}
	done := make(chan struct{}, 3)
	for _, run := range []func(){func() { w.Hub.Run(ctx) }, func() { w.Present(ctx, "worker", "", 200*time.Millisecond) }, func() { w.Run(ctx) }} {
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
		s.Transcode = &transcode.Jobs{Pool: s.Pool, Segments: segments, Hub: hub, QueueTimeout: 10 * time.Second, WaitTimeout: 60 * time.Second, Log: s.Log}
	}
	if !eventually(func() bool { return scalar[int](t, e.pool, "SELECT count(*) FROM transcode_nodes") > 0 }) {
		t.Fatal("the worker never registered")
	}

	body := map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": db.CompactID(source)}
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
	if len(entries) < 8 {
		t.Fatalf("the playlist lists %d segments:\n%s", len(entries), playlist)
	}

	// A viewer who seeks to segment 5 first: the job begins there, and fails.
	for i := 0; i < 20; i++ {
		if response, text := e.do(t, i%2, "GET", entries[5+i%3], "", nil); response.StatusCode != 500 {
			t.Fatalf("request %d for a segment of the failed job was answered %d %s, want 500", i, response.StatusCode, text)
		}
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Fatalf("%d jobs exist for 20 requests, want the one that failed", n)
	}
	reason := scalar[string](t, e.pool, "SELECT error FROM transcode_jobs WHERE state = 'failed'")
	if reason == "" {
		t.Fatal("the job's recorded reason is empty")
	}
	// A line that names the item, the source and the reason, in any process's log, at Warn or above.
	named := func(line string) bool {
		lower := strings.ToLower(line)
		has := func(id string) bool {
			return strings.Contains(lower, strings.ToLower(db.CompactID(id))) || strings.Contains(lower, strings.ToLower(db.DashedID(id)))
		}
		return (strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR")) && has(e.item) && has(source) && strings.Contains(line, reason)
	}
	var lines []string
	for who, log := range map[string]*syncBuffer{"the worker": &workerLog, "replica 0": &replicaLog, "replica 1": &otherLog} {
		for _, line := range strings.Split(log.String(), "\n") {
			if named(line) {
				lines = append(lines, who+": "+line)
			}
		}
	}
	if len(lines) != 1 {
		t.Errorf("%d log lines name the item %s, the source %s and the reason %q, want one:\n%s\n--- the worker's log:\n%s\n--- replica 0:\n%s", len(lines), item, source, reason,
			strings.Join(lines, "\n"), workerLog.String(), replicaLog.String())
	}
}
