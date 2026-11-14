//go:build integration

package transcode

import "context"

// lookForTheDead is one look of a locked worker, and says whether it found this
// worker among the dead.
func (w *Worker) lookForTheDead(ctx context.Context) bool {
	ev := w.look(ctx)
	return ev == evRowGone || ev == evOwnLockMissing
}
