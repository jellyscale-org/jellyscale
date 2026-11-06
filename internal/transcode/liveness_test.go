package transcode

import (
	"context"
	"testing"
)

// Between losing its lock and letting go of it, a worker may not claim: a
// job claimed then would run on a context that has already ended.
func TestALostLockIsNotHeldBeforeItIsLetGo(t *testing.T) {
	w := &Worker{}
	if _, ok := w.jobContext(); ok {
		t.Fatal("held with no lock taken")
	}
	held, lose := context.WithCancelCause(context.Background())
	w.setHeld(held)
	if _, ok := w.jobContext(); !ok {
		t.Fatal("not held while the lock is held")
	}
	lose(errLockLost)
	if _, ok := w.jobContext(); ok {
		t.Error("held after the lock was lost, before setHeld(nil)")
	}
}
