package transcode

import (
	"math"

	"jellyscale/internal/playback"
)

// JS-161 (D-044, D-062), JS-177 (D-064): the price table is in cores of one named reference machine (docs/transcoding.md),
// and each worker advertises its capacity in those units. It starts from the CPUs it may use, each at the reference speed,
// or from transcodeCores if that is fewer, and learns what it holds from the jobs it runs (capacity_learn.go,
// capacity_sampler.go). It measures nothing of its own: a benchmark at the start moved with the host's load and delayed the
// row a rolling restart's replacement is waited for by.

// dearestRow is the price of the dearest job in the table, the AV1 fallback of a 4K HDR source.
var dearestRow = playback.Spec{Recode: "av1", ToneMap: "hable", SourceHeight: 2160}.Cost()

// Where a worker's capacity comes from, as the log and the labels of /metrics say it.
const (
	sourceAssumed = "assumed" // The CPUs it may use, each at the reference speed.
	sourceCeiling = "ceiling" // transcodeCores, or transcodeSlots, holds it below them.
	sourceLearned = "learned" // Moved by what its jobs showed.
)

// start is the figure the worker begins from and where it comes from: the lesser of the CPUs it may use and transcodeCores,
// which a ceiling above them does not raise. transcodeSlots, which JS-177 leaves as it was, is that many cores.
func (w *Worker) start() (float64, string) {
	if w.Cores <= 0 && w.Slots > 0 {
		return float64(w.Slots), sourceCeiling
	}
	return startFigure(w.kernel().Allotment(), w.Cores)
}

// cores is the figure the worker starts from. What it holds now is in registered.
func (w *Worker) cores() float64 {
	figure, _ := w.start()
	return figure
}

// registered is the cores and slots a worker row carries: the figure the worker holds, which is its start until a move.
// A worker without a transcode store runs no transcode, and has none. Slots are the cores rounded up, so that a replica of
// the release before, which counts a worker by slots > 0, counts this one.
func (w *Worker) registered() (cores float64, slots int) {
	if w.Segments == nil {
		return 0, 0
	}
	w.setID()
	cores, _ = w.heldCapacity()
	return cores, int(math.Ceil(cores))
}

// SayCapacity says once, at the worker's start, what capacity it counts and where it comes from, and says again, once,
// where that is below what the dearest job needs: the worker then refuses or shares that play, and the operator should
// read why from the start and not from a viewer's complaint. The same figures are on /metrics.
func (w *Worker) SayCapacity() {
	w.setID()
	if w.Segments == nil {
		return // No transcode store: no transcode, and no capacity.
	}
	cores, source := w.heldCapacity()
	if source == sourceAssumed {
		w.Log.Info("transcode capacity", "cores", cores, "source", source, "processors", int(math.Ceil(cores)), "note", assumedNote(w.Cores))
	} else if note := slotsNote(w.Cores, w.Slots, w.kernel().Allotment()); note != "" {
		w.Log.Info("transcode capacity", "cores", cores, "source", source, "note", note)
	} else {
		w.Log.Info("transcode capacity", "cores", cores, "source", source)
	}
	if w.belowDearest() {
		w.Log.Warn("transcode capacity below the dearest row", "cost", dearestRow, "cores", cores,
			"note", "plays that cost more than its cores are refused or shared with a second worker")
	}
}

// assumedNote is what the start line tells the operator of an assumed capacity. A transcodeCores that is set, and so at or
// above the CPUs (below them it would hold the figure, source ceiling), is said not to be counted, rather than suggested.
func assumedNote(ceiling float64) string {
	if ceiling > 0 {
		return "transcodeCores is at or above the processors, so it is not counted as written: the worker starts from its processors, each at the reference speed, and learns what it holds from its jobs"
	}
	return "nothing limits it: each processor is counted at the reference speed, which overstates a slower CPU or SMT threads, until the worker's jobs show what it holds; set transcodeCores to cap it"
}

// slotsNote is what the start line tells the operator whose figure comes from transcodeSlots (which counts when transcodeCores
// is not set) above the processors: slots count as that many cores even there, so the worker starts, and learns downward from, a
// figure the processors cannot give. It is "" for every other start.
func slotsNote(cores float64, slots int, allotment float64) string {
	if cores > 0 || float64(slots) <= allotment {
		return ""
	}
	return "transcodeSlots counts as that many cores even above the processors, so learning starts there"
}

// belowDearest says whether the capacity the worker counts is below the dearest row of the price table.
func (w *Worker) belowDearest() bool {
	cores, _ := w.registered()
	return w.Segments != nil && cores < dearestRow
}
