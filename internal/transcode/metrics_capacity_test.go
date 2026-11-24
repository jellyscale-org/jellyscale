package transcode

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// unhelpedSeries collects the worker's capacity metrics and returns the unhelped_jobs_total series by their labels.
func unhelpedSeries(t *testing.T, w *Worker) map[unhelpedKey]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	w.collectCapacity(ch)
	close(ch)
	series := map[unhelpedKey]float64{}
	for metric := range ch {
		if metric.Desc() != unhelpedDesc {
			continue
		}
		var out dto.Metric
		if err := metric.Write(&out); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{}
		for _, label := range out.Label {
			labels[label.GetName()] = label.GetValue()
		}
		series[unhelpedKey{labels["reason"], labels["capacity"]}] = out.Counter.GetValue()
	}
	return series
}

// A slow job is counted under the source its worker's capacity had when it was counted, and a later move of the figure
// does not relabel it: the series that tells the operator to set transcodeCores is not replaced by a learned one, which a rate
// would read as a counter reset. Mutant: the label taken from the worker's source at scrape time.
func TestASlowJobStaysCountedUnderTheSourceItHadWhenCounted(t *testing.T) {
	w := &Worker{Kernel: &fakeKernel{cpus: 4}}
	w.countUnhelped("lone_worker")
	before := unhelpedSeries(t, w)
	w.capacity.mu.Lock()
	w.capacity.source = sourceLearned // The worker has learned, a moment later.
	w.capacity.mu.Unlock()
	after := unhelpedSeries(t, w)
	if len(before) != 1 || before[unhelpedKey{"lone_worker", sourceAssumed}] != 1 || len(after) != 1 || after[unhelpedKey{"lone_worker", sourceAssumed}] != 1 {
		t.Fatalf("before the move %v, after it %v: the job counted under assumed stays there", before, after)
	}
	w.countUnhelped("lone_worker")
	if got := unhelpedSeries(t, w); got[unhelpedKey{"lone_worker", sourceAssumed}] != 1 || got[unhelpedKey{"lone_worker", sourceLearned}] != 1 || len(got) != 2 {
		t.Errorf("series %v: want the assumed one unchanged and a learned one for the job counted after the move", got)
	}
}

// An operator who set transcodeCores at or above the processors is told it is not counted, not asked to set it.
func TestTheNoteOfAnAssumedStartDoesNotAskForTheCeilingTheOperatorSet(t *testing.T) {
	if note := assumedNote(8); !strings.Contains(note, "not counted") || strings.Contains(note, "set transcodeCores") {
		t.Errorf("a ceiling at or above the processors: %q", note)
	}
	if note := assumedNote(0); !strings.Contains(note, "set transcodeCores") {
		t.Errorf("no ceiling: %q", note)
	}
}

// A figure that comes from transcodeSlots above the processors is told it is counted as written; every other start has no such note.
func TestTheStartLineSaysSlotsAboveTheProcessorsCountAsWritten(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cores     float64
		slots     int
		allotment float64
		note      bool
	}{
		{"slots above the processors", 0, 50, 4, true},
		{"slots at the processors", 0, 4, 4, false},
		{"slots below the processors", 0, 3, 4, false},
		{"slots above the processors beside transcodeCores, which wins", 2.5, 50, 4, false},
		{"nothing declared", 0, 0, 4, false},
	} {
		if note := slotsNote(tc.cores, tc.slots, tc.allotment); (note != "") != tc.note || (tc.note && !strings.Contains(note, "even above the processors")) {
			t.Errorf("%s: note %q, want one: %v", tc.name, note, tc.note)
		}
	}
}
