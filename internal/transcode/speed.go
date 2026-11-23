package transcode

import (
	"runtime"

	"jellyscale/internal/playback"
)

// JS-161 (D-044, D-062): the price table is in cores of one named reference machine (docs/transcoding.md), and each worker
// advertises its capacity in those units: what it declares, else each processor it may use at the reference speed. A
// worker measures nothing: a benchmark at the start moved with the host's load and delayed the row a rolling restart's
// replacement is waited for by.

// dearestRow is the price of the dearest job in the table, the AV1 fallback of a 4K HDR source.
var dearestRow = playback.Spec{Recode: "av1", ToneMap: "hable", SourceHeight: 2160}.Cost()

func processors() int { return runtime.GOMAXPROCS(0) }

// SayCapacity says once, at the worker's start, what capacity it counts and whether it declared it, and says again, once,
// where that is below what the dearest job needs: the worker then refuses or shares that play, and the operator should
// read why from the start and not from a viewer's complaint. The same figures are on /metrics.
func (w *Worker) SayCapacity() {
	w.setID()
	if w.Segments == nil {
		return // No transcode store: no transcode, and no capacity.
	}
	if w.assumed {
		w.Log.Info("transcode capacity", "cores", w.Cores, "source", "assumed", "processors", int(w.Cores),
			"note", "nothing declared: each processor is counted at the reference speed, which overstates a slower CPU or SMT threads; set transcodeCores to correct it")
	} else {
		w.Log.Info("transcode capacity", "cores", w.Cores, "source", "declared")
	}
	if w.belowDearest() {
		w.Log.Warn("transcode capacity below the dearest row", "cost", dearestRow, "cores", w.Cores,
			"note", "plays that cost more than its cores are refused or shared with a second worker")
	}
}

// capacitySource is the word for where the worker's capacity comes from, as the log and the labels of /metrics say it.
func (w *Worker) capacitySource() string {
	if w.assumed {
		return "assumed"
	}
	return "declared"
}

// belowDearest says whether the capacity the worker counts is below the dearest row of the price table.
func (w *Worker) belowDearest() bool {
	cores, _ := w.registered()
	return w.Segments != nil && cores < dearestRow
}
