package subtitles

// JS-112: ffmpeg runs of this package (the track extraction as text, as sup,
// and the fonts) go through internal/mediatool. What a viewer or an operator
// sees does not change: the arguments, stdin and working directory
// (testdata/tool_runs.golden, recorded on the code before the change;
// UPDATE_GOLDEN=1 rewrites it), the errors and the log lines. With the
// extraction's policy, any output is a failure; ErrBadFile is for an exit status
// above 0 (or complaining at exit 0) and not for a signal, a context that
// ended, a missing ffmpeg or a full disk. None of these needs ffmpeg.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jellyscale/internal/items"
	"jellyscale/internal/media"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

func TestRunFFmpegSaysWhetherTheFileIsToBlame(t *testing.T) {
	for _, c := range []struct {
		name, script string
		bad, fails   bool
	}{
		{"exit status 1", `echo "Invalid data found when processing input" >&2; exit 1`, true, true},
		{"exit 0 with a complaint", `echo "Truncating packet of size 3" >&2`, true, true},
		{"exit 0 with whitespace", `echo "  " >&2`, false, false},
		{"killed by a signal", `kill -9 $$`, false, true},
		{"no space left on device", `echo "No space left on device" >&2; exit 1`, false, true},
		{"nothing", `exit 0`, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			testenv.Shims(t, map[string]string{"ffmpeg": c.script})
			err := runFFmpeg(context.Background(), []string{"-i", "in"})
			if (err != nil) != c.fails || errors.Is(err, ErrBadFile) != c.bad {
				t.Errorf("error %v: ErrBadFile %v, want failure %v and ErrBadFile %v", err, errors.Is(err, ErrBadFile), c.fails, c.bad)
			}
		})
	}
	t.Run("context ended", func(t *testing.T) {
		ctx := testenv.Fault(t, "ffmpeg", "context")
		if err := runFFmpeg(ctx, nil); err == nil || errors.Is(err, ErrBadFile) {
			t.Errorf("error %v", err)
		}
	})
	t.Run("ffmpeg missing", func(t *testing.T) {
		testenv.NoTools(t)
		if err := runFFmpeg(context.Background(), nil); err == nil || errors.Is(err, ErrBadFile) {
			t.Errorf("error %v", err)
		}
	})
}

// logged is what a handler kept of a log: level, message and attributes, never
// the time or a duration.
type logged struct{ lines []string }

func (l *logged) log() *slog.Logger {
	return slog.New(slog.NewTextHandler(writerFunc(func(p []byte) { l.lines = append(l.lines, strings.TrimSpace(string(p))) }),
		&slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "time" || a.Key == "duration" {
				return slog.Attr{}
			}
			return a
		}}))
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func font() media.MediaStream {
	return media.MediaStream{Index: 3, Type: "Attachment", MimeType: "font/ttf", FileName: "a.ttf"}
}

// subtitleRuns is each run, called with a film in dir; a run says what it
// returned and logged as one text.
func subtitleRuns(dir string) []struct {
	name string
	run  func(ctx context.Context) string
} {
	media := store.NewPosix(dir)
	src := items.Source{ID: "item-1", Key: "in.mkv", ETag: "etag-1", Size: 3, Streams: []mediaStream{font()}}
	return []struct {
		name string
		run  func(ctx context.Context) string
	}{
		{"text", func(ctx context.Context) string {
			_, err := Extract(ctx, filepath.Join(dir, "in.mkv"), 2)
			return fmt.Sprint(err)
		}},
		{"sup", func(ctx context.Context) string {
			var l logged
			err := FillSup(ctx, src, media, store.NewPosix(t0(dir)), 2, 0, l.log())
			return fmt.Sprint(err) + "\n" + strings.Join(l.lines, "\n")
		}},
		{"fonts", func(ctx context.Context) string {
			var l logged
			err := FillFonts(ctx, src, media, store.NewPosix(t0(dir)), 0, l.log())
			return fmt.Sprint(err) + "\n" + strings.Join(l.lines, "\n")
		}},
	}
}

type mediaStream = media.MediaStream

// t0 is a directory for the asset store, beside the film's.
func t0(dir string) string {
	assets := filepath.Join(dir, "assets")
	os.MkdirAll(assets, 0o755) //nolint:errcheck
	return assets
}

func filmDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.mkv"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSubtitleToolRunsGetTheArgumentsStdinAndDirectoryTheyAlwaysDid(t *testing.T) {
	dir := filmDir(t)
	recorded := testenv.Recorder(t, map[string]string{"ffmpeg": ""}, map[string]string{dir: "<dir>"})
	for _, run := range subtitleRuns(dir) {
		run.run(context.Background())
	}
	// ffmpeg's own temp directories are named by the run.
	got := scrubTemp(recorded())
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

var tempName = regexp.MustCompile(`(subtitles|sup|attachments)-\d+`)

func scrubTemp(text string) string {
	text = tempName.ReplaceAllString(text, "$1-<n>")
	return strings.ReplaceAll(text, os.TempDir()+"/", "<os tmp>/")
}

// What each run returns and logs when ffmpeg exits 1 with a line on stderr, is
// killed by a signal, or has its context end.
func TestSubtitleToolRunsFailWithTheTextAndLogsTheyAlwaysDid(t *testing.T) {
	want := subtitleWant
	for _, run := range subtitleRuns(filmDir(t)) {
		for i, kind := range []string{"exit1", "signal", "context"} {
			t.Run(run.name+" "+kind, func(t *testing.T) {
				ctx := testenv.Fault(t, "ffmpeg", kind)
				got := scrubTemp(run.run(ctx))
				if got != want[run.name][i] {
					t.Errorf("got:\n%s\nwant:\n%s", got, want[run.name][i])
				}
			})
		}
	}
}
