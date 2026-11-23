package subtitles

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-161 (D-044): a worker's capacity is what it measured, with no floor to hide a loss. A subtitle or image-track
// extraction runs on the worker beside its jobs and holds no cores in any ledger (JS-154: a PGS extraction took a job
// from about 0.85x to 0.56-0.63x), so it must take only the cores the jobs leave: it runs at the lowest priority (nice
// 10 or more, or the idle scheduling class). The script is the ffmpeg of the run and reads its own priority.
// Today it runs at its parent's. Not covered: memory bandwidth, which no priority gives back.
func TestAnExtractionRunsBehindTheJobsOfItsWorker(t *testing.T) {
	out := filepath.Join(t.TempDir(), "priority")
	t.Setenv("PRIORITY_OUT", out)
	testenv.Shims(t, map[string]string{"ffmpeg": `awk '{print $19, $41}' /proc/$$/stat > "$PRIORITY_OUT"`})
	if err := runFFmpeg(context.Background(), []string{"-i", "in"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(raw))
	nice, _ := strconv.Atoi(fields[0])
	policy, _ := strconv.Atoi(fields[1])
	const schedIdle = 5
	if nice < 10 && policy != schedIdle {
		t.Errorf("the extraction runs at nice %d, scheduling policy %d: it takes cores from a job beside it", nice, policy)
	}
}
