package transcode

import (
	"context"
	"testing"
)

// Between losing its lock and letting go of it, a worker may not claim: a
// job claimed then would run on a context that has already ended. Without the
// lock, or with a lock that has ended, it claims nothing, though the context its
// running jobs share is still alive.
func TestALostLockIsNotHeldBeforeItIsLetGo(t *testing.T) {
	w := &Worker{}
	if _, ok := w.jobContext(); ok {
		t.Fatal("held with no lock taken")
	}
	held, lose := context.WithCancelCause(context.Background())
	lock := &liveness{lost: make(chan struct{})}
	w.setHeld(held, lock)
	if _, ok := w.jobContext(); !ok {
		t.Fatal("not held while the lock is held")
	}
	close(lock.lost)
	if _, ok := w.jobContext(); ok {
		t.Error("a worker whose lock connection ended claims a job")
	}
	w.setLock(nil)
	if _, ok := w.jobContext(); ok {
		t.Error("a worker taking its lock again claims a job")
	}
	w.setLock(&liveness{lost: make(chan struct{})})
	lose(errLockLost)
	if _, ok := w.jobContext(); ok {
		t.Error("held after the lock was lost, before setHeld(nil)")
	}
}
