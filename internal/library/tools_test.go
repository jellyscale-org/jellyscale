package library

// JS-112: the five runs of ffmpeg and ffprobe in this package (the probe, the
// HDR10+ read, the video-end read, the scan's frame and the chapter frame) give
// the tools the same arguments, stdin and working directory, and fail with the
// same error text, whoever starts them. Both are pinned to what the code did
// before the runs went through internal/mediatool: testdata/tool_runs.golden
// was recorded on that code (UPDATE_GOLDEN=1 rewrites it), and the error texts
// below were observed on it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// libraryRuns is each run, and the tool it starts, called with a file in dir.
func libraryRuns(dir string) []struct {
	name, tool string
	run        func(ctx context.Context) error
} {
	media := store.NewPosix(dir)
	scanner := &Scanner{Log: slog.New(slog.DiscardHandler)}
	return []struct {
		name, tool string
		run        func(ctx context.Context) error
	}{
		{"probe", "ffprobe", func(ctx context.Context) error { _, err := ProbeInput(ctx, filepath.Join(dir, "in.mkv")); return err }},
		{"video end", "ffprobe", func(ctx context.Context) error {
			_, err := ReadVideoEnd(ctx, filepath.Join(dir, "in.mkv"), 5*ticksPerSecond)
			return err
		}},
		{"scan frame", "ffmpeg", func(ctx context.Context) error {
			return scanner.extractFrame(ctx, Library{Media: media}, &record{}, store.Object{Key: "in.mkv", Size: 3}, Probe{RunTimeTicks: 100 * ticksPerSecond})
		}},
		{"chapter frame", "ffmpeg", func(ctx context.Context) error {
			_, err := chapterFrame(ctx, filepath.Join(dir, "in.mkv"), 30*ticksPerSecond, "")
			return err
		}},
	}
}

func mediaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.mkv"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLibraryToolRunsGetTheArgumentsStdinAndDirectoryTheyAlwaysDid(t *testing.T) {
	dir := mediaDir(t)
	recorded := testenv.Recorder(t, map[string]string{"ffprobe": "{}", "ffmpeg": ""}, map[string]string{dir: "<dir>"})
	ctx := context.Background()
	for _, run := range libraryRuns(dir) {
		run.run(ctx) //nolint:errcheck // The recording is the point; the stand-ins' output is not a film.
	}
	hasHDR10Plus(ctx, filepath.Join(dir, "in.mkv"))
	got := recorded()
	golden := filepath.Join("testdata", "tool_runs.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the tools were started differently than before.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A tool that exits 1 with a line on stderr, one killed by a signal and one
// whose context ends: the error each run returns.
func TestLibraryToolRunsFailWithTheTextTheyAlwaysDid(t *testing.T) {
	want := map[string][3]string{ // exit1, signal, context
		"probe":         {"ffprobe: exit status 1: boom: the file is not a film", "ffprobe: signal: killed: ", "ffprobe: signal: killed: "},
		"video end":     {"ffprobe: exit status 1: boom: the file is not a film", "ffprobe: signal: killed: ", "ffprobe: signal: killed: "},
		"scan frame":    {"ffmpeg: exit status 1", "ffmpeg: signal: killed", "ffmpeg: signal: killed"},
		"chapter frame": {"ffmpeg: exit status 1: boom: the file is not a film", "ffmpeg: signal: killed: ", "ffmpeg: context canceled"},
	}
	for _, run := range libraryRuns(mediaDir(t)) {
		for i, kind := range []string{"exit1", "signal", "context"} {
			t.Run(run.name+" "+kind, func(t *testing.T) {
				ctx := testenv.Fault(t, run.tool, kind)
				err := run.run(ctx)
				if err == nil {
					t.Fatal("no error")
				}
				if err.Error() != want[run.name][i] {
					t.Errorf("error %q, want %q", err, want[run.name][i])
				}
			})
		}
	}
}

// The probe reads its stdout alone: a warning on stderr with exit 0 is not a
// failure and not part of the answer.
func TestAProbeWhoseFfprobeWarnsOnStderrStillParsesStdout(t *testing.T) {
	testenv.Shims(t, map[string]string{"ffprobe": `echo '{"format":{"format_name":"matroska,webm","duration":"12.5"}}'
echo "[matroska] Unknown-sized element" >&2`})
	probe, err := ProbeInput(context.Background(), "in.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if probe.RunTimeTicks != 125_000_000 || probe.Container != "mkv" {
		t.Errorf("the probe read %+v", probe)
	}
}

// A chapter frame whose context ends (the 2-minute frame timeout, shortened)
// is killed, and the call returns even though a process the tool started still
// holds its output open: the call waits no longer than the tool's own wait
// delay, 5 seconds, and not for that process.
func TestAChapterFrameEndsWhenItsContextEndsEvenIfAnotherProcessHoldsItsOutput(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "holder")
	testenv.Shims(t, map[string]string{"ffmpeg": "sleep 600 &\necho $! >" + pids + "\nexec sleep 600"})
	t.Cleanup(func() {
		data, _ := os.ReadFile(pids)
		var pid int
		if _, err := fmt.Sscan(strings.TrimSpace(string(data)), &pid); err == nil {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
		}
	})
	defer SetChapterFrameTimeout(time.Second)()
	began := time.Now()
	_, err := chapterFrame(context.Background(), "in.mkv", 0, "")
	took := time.Since(began)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v, want the context's", err)
	}
	// The call takes the timeout and the wait delay, about 6 seconds; the bound
	// only catches a call that waits for the holder, which sleeps 10 minutes.
	if took > 60*time.Second {
		t.Errorf("the call took %v", took)
	}
	t.Logf("the call returned %v after it began", took.Round(100*time.Millisecond))
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("error %v does not name the tool", err)
	}
}
