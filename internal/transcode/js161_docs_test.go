package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-161 (D-044, D-046, D-050): the docs say what the criteria name, in any wording. The behaviour is checked by
// speed_test.go, shared_admission_integration_test.go and cmd/jellyscale's worker_speed_integration_test.go.

// The metrics are named in the table of docs/running.md, one row each, and the names are the ones the process tests read.
func TestRunningDocNamesTheSpeedAndRefusalMetrics(t *testing.T) {
	text := doc(t, "docs/running.md")
	for _, name := range []string{
		"jellyscale_transcode_worker_speed",
		"jellyscale_transcode_speed_measurement_failures_total",
		"jellyscale_transcode_capacity_below_dearest_row",
		"jellyscale_transcode_unhelped_jobs_total",
	} {
		if !regexp.MustCompile("(?m)^\\| `" + name + "[`{]").MatchString(text) {
			t.Errorf("docs/running.md's metric table has no row for %s", name)
		}
	}
}

func TestRunningDocSaysWhatTheMeasurementIsAndWhatAWorkerCountsAs(t *testing.T) {
	text := flat(doc(t, "docs/running.md"))
	for what, pattern := range map[string]string{
		"the bound of the measurement, in seconds":  `measurement[^.]*within \d+ seconds`,
		"what a worker counts as until it ends":     `until (the|its) (speed )?measurement`,
		"what it counts as when it fails":           `(measurement fails|fails or overruns|measurement that fails)[^.]*(counts|count)`,
		"the tolerance of a reference-speed worker": `within \d+ ?%`,
		"that nothing is declared by the operator":  `(nothing|no setting) (is )?(declared|to declare|to set)|operator declares nothing|measures its own speed`,
	} {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("docs/running.md does not say %s", what)
		}
	}
	if !regexp.MustCompile(`never (falls back|counts)[^.]*(silently|processor count|its processors)|not (silently )?(fall|falls) back`).MatchString(text) {
		t.Error("docs/running.md does not say a failed measurement is never a silent fall back to the processor count")
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
