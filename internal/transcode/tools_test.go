package transcode

// JS-112: the runs of ffmpeg and ffprobe in this package that are not the
// encode's own command (that is command.golden's): the audio grid and ffprobe,
// with and without stdin. They get the arguments, stdin and working directory
// they always did (testdata/tool_runs.golden, recorded on the code before the
// change; UPDATE_GOLDEN=1 rewrites it) and fail with the same text. Command and
// CopyCommand keep command.golden and command_golden_test.go as they are.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

func toolRuns() []struct {
	name, tool string
	run        func(ctx context.Context) error
} {
	spec := playback.Spec{AudioIndex: 1, AudioBitrate: 128000}
	return []struct {
		name, tool string
		run        func(ctx context.Context) error
	}{
		{"audio grid", "ffmpeg", func(ctx context.Context) error { _, err := audioGrid(ctx, "in.mkv", spec); return err }},
		{"ffprobe", "ffprobe", func(ctx context.Context) error {
			_, err := ffprobe(ctx, "-select_streams", "v:0", "-of", "json", "in.mkv")
			return err
		}},
		{"ffprobe with stdin", "ffprobe", func(ctx context.Context) error {
			_, err := ffprobeFrom(ctx, strings.NewReader("fragment bytes"), "-select_streams", "v:0", "-of", "csv=p=0", "-")
			return err
		}},
	}
}

func TestTranscodeToolRunsGetTheArgumentsStdinAndDirectoryTheyAlwaysDid(t *testing.T) {
	recorded := testenv.Recorder(t, map[string]string{"ffprobe": "{}", "ffmpeg": ""}, nil)
	for _, run := range toolRuns() {
		run.run(context.Background()) //nolint:errcheck // The recording is the point.
	}
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

func TestTranscodeToolRunsFailWithTheTextTheyAlwaysDid(t *testing.T) {
	want := map[string][3]string{ // exit1, signal, context
		"audio grid":         {"finding where the audio's packets fall: exit status 1: boom: the file is not a film", "finding where the audio's packets fall: signal: killed", "finding where the audio's packets fall: signal: killed"},
		"ffprobe":            {"ffprobe: exit status 1: boom: the file is not a film", "ffprobe: signal: killed: ", "ffprobe: signal: killed: "},
		"ffprobe with stdin": {"ffprobe: exit status 1: boom: the file is not a film", "ffprobe: signal: killed: ", "ffprobe: signal: killed: "},
	}
	for _, run := range toolRuns() {
		for i, kind := range []string{"exit1", "signal", "context"} {
			t.Run(run.name+" "+kind, func(t *testing.T) {
				err := run.run(testenv.Fault(t, run.tool, kind))
				if err == nil || err.Error() != want[run.name][i] {
					t.Errorf("error %v, want %q", err, want[run.name][i])
				}
			})
		}
	}
}
