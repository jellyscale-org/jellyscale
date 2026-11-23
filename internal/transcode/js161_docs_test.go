package transcode

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// JS-161 (D-044, D-046, D-050, D-062): the docs say what the criteria name, in any wording. The behaviour is checked by
// speed_test.go, shared_admission_integration_test.go and cmd/jellyscale's worker_speed_integration_test.go.

// The metrics are named in the table of docs/running.md, one row each, and the names are the ones the process tests read.
// The speed metrics of the start-up benchmark D-062 removed are in no document.
func TestRunningDocNamesTheCapacityAndRefusalMetricsAndNoSpeedMeasurement(t *testing.T) {
	text := doc(t, "docs/running.md")
	for _, name := range []string{
		"jellyscale_transcode_capacity_assumed",
		"jellyscale_transcode_capacity_below_dearest_row",
		"jellyscale_transcode_unhelped_jobs_total",
	} {
		if !regexp.MustCompile("(?m)^\\| `" + name + "[`{]").MatchString(text) {
			t.Errorf("docs/running.md's metric table has no row for %s", name)
		}
	}
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs: %v", err)
	}
	for _, path := range append(docs, "../../DOMAINS.md", "../../PRODUCT.md") {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, gone := range []string{"jellyscale_transcode_worker_speed", "jellyscale_transcode_speed_measurement_failures_total", "transcode speed measured"} {
			if strings.Contains(string(raw), gone) {
				t.Errorf("%s still has %q: D-062 removed the start-up measurement", path, gone)
			}
		}
		if regexp.MustCompile(`(?i)measures (its|their) (own )?speed|measurement[^.]*within \d+ seconds`).Match(raw) {
			t.Errorf("%s still says a worker measures its speed", path)
		}
	}
}

// docs/running.md says what an undeclared worker counts (each processor at the reference speed, at once), that it says
// so in its log and on /metrics, when it overstates itself and what a viewer then sees, and how to choose transcodeCores
// from docs/transcoding.md's figures (D-062).
func TestRunningDocSaysWhatAnUndeclaredWorkerAssumesAndWhenItOverstatesItself(t *testing.T) {
	text := flat(doc(t, "docs/running.md"))
	for what, pattern := range map[string]string{
		"that an undeclared worker counts each processor at the reference machine's speed": `(declares (neither|nothing)|undeclared|declare nothing)[^.]*(each|every) (of its )?processors?[^.]*at (the )?reference`,
		"that it registers at once, with no benchmark":                                     `no benchmark`,
		"that its capacity is assumed, in the log and on /metrics":                         `assumed[^.]*(log|/metrics)|(log|/metrics)[^.]*assumed`,
		"that a CPU slower than the reference overstates it":                               `slower (per core )?than the reference[^.]*(overstate|more than)|overstate[^.]*slower`,
		"that SMT or hardware threads overstate it":                                        `(smt|hyper-?thread|hardware thread)[^.]*(overstate|more than)|overstate[^.]*(smt|hyper-?thread|hardware thread)`,
		"what a viewer then sees (a stall)":                                                `(overstate|assumed)[^.]*(stall|stutter|under real time|below real time)|(stall|stutter)[^.]*(overstate|assumed)`,
		"how to choose transcodeCores from docs/transcoding.md's figures":                  `transcodecores[^.]*(docs/transcoding\.md|figures|reference cores)|(docs/transcoding\.md|figures)[^.]*transcodecores`,
	} {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("docs/running.md does not say %s", what)
		}
	}
}

// The transcodeHelpers paragraph no longer says a new viewer stops any helper; it says what the helper-held refusal
// means, and that room for one more film avoids it.
func TestRunningDocSaysWhatTheHelperHeldRefusalMeans(t *testing.T) {
	text := flat(doc(t, "docs/running.md"))
	if strings.Contains(text, "a new viewer whose job fits no worker until helpers stop stops them") {
		t.Error("docs/running.md still says a new viewer stops any helper")
	}
	if !strings.Contains(text, "coresheldbywatchedfilm") {
		t.Error("docs/running.md does not name the helper-held refusal's reason, CoresHeldByWatchedFilm")
	}
	if !regexp.MustCompile(`room for one more film|one more film`).MatchString(text) || !strings.Contains(text, "autoscaler") {
		t.Error("docs/running.md does not say that room for one more film, kept by sizing or by the autoscaler, avoids it")
	}
	if !strings.Contains(text, "1.2") || !regexp.MustCompile(`holds? (it|the job)[^.]*(1\.2|real time)|(1\.2|real time)[^.]*helper[^.]*(is |are )?(never )?stopped`).MatchString(text) {
		t.Error("docs/running.md does not say which helpers a new viewer may still stop (those of a job its own worker holds at 1.2 times real time)")
	}
}

func TestDomains58SaysAWatchedFilmsHelperIsNotStoppedForANewViewer(t *testing.T) {
	text := flat(invariant(t, "58"))
	if !regexp.MustCompile(`(never|not)[^.]*(stop|cancel)[^.]*helper[^.]*(watched|real time|1\.2)|helper[^.]*(watched|keeps)[^.]*(never|not)[^.]*(stop|cancel)`).MatchString(text) {
		t.Error("DOMAINS 58 does not say that a helper a watched job needs to stay at real time is never stopped for a new viewer")
	}
	if !strings.Contains(text, "refus") {
		t.Error("DOMAINS 58 does not say the new viewer is refused instead")
	}
}

func TestTranscodingDocNamesTheReferenceMachineAndTheDogfoodsCapacity(t *testing.T) {
	text := doc(t, "docs/transcoding.md")
	flatText := flat(text)
	if !regexp.MustCompile(`reference machine[^.]*(ryzen|intel|amd|core i|xeon)`).MatchString(flatText) {
		t.Error("docs/transcoding.md does not name the reference machine's processor")
	}
	if !regexp.MustCompile(`reference machine[^|]*threads?`).MatchString(flatText) || !regexp.MustCompile(`reference machine[^|]*ffmpeg \d`).MatchString(flatText) {
		t.Error("docs/transcoding.md does not give the reference machine's threads and ffmpeg version")
	}
	if !regexp.MustCompile(`i5-6500[^.]*\d+(\.\d+)? reference cores`).MatchString(flatText) {
		t.Error("docs/transcoding.md does not give the i5-6500's capacity in reference cores")
	}
	// D-062: the reference machine and the i5-6500's figures, the 2.7 marked as computed (it was never measured).
	for what, pattern := range map[string]string{
		"the reference machine, the AMD Ryzen 7 7800X3D":     `7800x3d`,
		"the reference machine's ffmpeg, 9.0.2":              `ffmpeg 9\.0\.2`,
		"the i5-6500's 0.81 to 0.87 times real time":         `0\.81[^.|]*0\.87`,
		"the i5-6500's 3.77 to 4.07 CPU-seconds per second":  `3\.77[^|]*4\.07`,
		"the i5-6500's 2.7 reference cores, marked computed": `2\.7 reference cores.{0,200}computed`,
	} {
		if !regexp.MustCompile(pattern).MatchString(flatText) {
			t.Errorf("docs/transcoding.md does not give %s", what)
		}
	}
	if strings.Contains(flatText, "worker measures") || strings.Contains(flatText, "measured at its start") {
		t.Error("docs/transcoding.md still says a worker measures its speed")
	}
	for _, leak := range []string{"/mnt/", "/media/", "/home/"} {
		if strings.Contains(text, leak) {
			t.Errorf("docs/transcoding.md has a path (%s)", leak)
		}
	}
}

func TestStatusDocSaysWhatTheDogfoodsLoneWorkerNowDoes(t *testing.T) {
	var gap string
	for _, line := range strings.Split(doc(t, "docs/status.md"), "\n") {
		if strings.HasPrefix(line, "- A gap") && strings.Contains(line, "i5-6500") {
			gap = flat(line)
		}
	}
	if gap == "" {
		t.Fatal("docs/status.md has no slow-worker gap line")
	}
	if strings.Contains(gap, "js-161 refuses") || strings.Contains(gap, "stalls, with no reason given") {
		t.Errorf("the gap line still speaks of JS-161 as a plan, or of a stall with no reason: %s", gap)
	}
	if !strings.Contains(gap, "refus") || !strings.Contains(gap, "reason") {
		t.Errorf("the gap line does not say the lone worker refuses such a play with its reason: %s", gap)
	}
}

// D-062: a declared i5-6500 refuses the 4K HDR re-encode with its reason and two share it, an undeclared one counts its
// processors and overstates itself; and no gap of D-060 (a restart that waits for a measurement, JS-176) is left in the docs.
func TestStatusAndRunningDocsHaveNoRestartGapAndSayWhatAnUndeclaredWorkerDoes(t *testing.T) {
	for _, name := range []string{"docs/status.md", "docs/running.md", "DOMAINS.md"} {
		for _, gone := range []string{"D-060", "JS-176"} {
			if strings.Contains(doc(t, name), gone) {
				t.Errorf("%s still names %s: D-062 superseded it and its gap never ships", name, gone)
			}
		}
	}
	var gap string
	for _, line := range strings.Split(doc(t, "docs/status.md"), "\n") {
		if strings.HasPrefix(line, "- A gap") && strings.Contains(line, "i5-6500") {
			gap = flat(line)
		}
	}
	if !regexp.MustCompile(`declared[^.]*refus[^.]*reason|refus[^.]*reason[^.]*declared`).MatchString(gap) ||
		!regexp.MustCompile(`two[^.]*(share|together)|(share|together)[^.]*two`).MatchString(gap) ||
		!regexp.MustCompile(`undeclared[^.]*(count|processors)[^.]*overstat`).MatchString(gap) {
		t.Errorf("the gap line does not say a declared i5-6500 refuses that play with its reason, that two share it, and that an undeclared one counts its processors and overstates itself: %s", gap)
	}
	running := flat(doc(t, "docs/running.md"))
	if !regexp.MustCompile(`transcodecores[^.]*2\.7|2\.7[^.]*transcodecores`).MatchString(running) {
		t.Error("docs/running.md does not give the i5-6500's 2.7 as the example of choosing transcodeCores")
	}
}
