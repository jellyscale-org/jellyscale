package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// JS-078: a sweep removes the old object of an item (its old image) at the
// moment the scan puts the new one beside it. Delete removes a directory that
// the removal leaves empty, and Put makes the directory and then its file, so
// the directory going between the two used to fail the Put: the item's new
// image was lost.
//
// The window is two system calls wide, and waiting for two goroutines to meet
// in it passed the store as it is on most runs of a 2-CPU machine. So the
// interleaving is forced: one goroutine does nothing but remove the item's
// directory whenever it is empty (os.Remove, as Delete's last step does, and
// which fails while a file is in it) and the others put a file and delete it
// at once, so that the directory is empty at the start of each Put and the
// remover is spinning on exactly that. Every Put of a right build succeeds,
// whatever the interleaving (MkdirAll itself can fail with "file exists" when the
// directory goes during it, as measured on a build that retried only CreateTemp).
// The one-line mutant it must fail: the store as it
// is today (MkdirAll then CreateTemp, no retry). Measured against that store
// on 1 and 2 CPUs (taskset), it fails in every run.
func TestAPutIntoADirectoryThatIsRemovedBeforeItsFileIsMadeSucceeds(t *testing.T) {
	root := t.TempDir()
	st := NewPosix(root)
	ctx := context.Background()
	dir := filepath.Join(root, "images", "item")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var removers sync.WaitGroup
	for range 2 {
		removers.Add(1)
		go func() {
			defer removers.Done()
			runtime.LockOSThread() // A thread of its own, so it spins in the system call beside the Put's.
			for {
				select {
				case <-stop:
					return
				default:
					os.Remove(dir) //nolint:errcheck // Fails whenever there is a file in it.
				}
			}
		}()
	}
	const writers, rounds = 4, 20000
	var failed atomic.Int64
	var first atomic.Value
	var puts sync.WaitGroup
	for w := range writers {
		puts.Add(1)
		go func() {
			defer puts.Done()
			for i := range rounds {
				if failed.Load() > 0 {
					return // One is enough to fail it; a right build never gets here.
				}
				key := fmt.Sprintf("images/item/new-%d-%d", w, i)
				if err := st.Put(ctx, key, strings.NewReader("new")); err != nil {
					failed.Add(1)
					first.CompareAndSwap(nil, err.Error())
					return
				}
				if err := st.Delete(ctx, key); err != nil && !errors.Is(err, fs.ErrNotExist) {
					failed.Add(1)
					first.CompareAndSwap(nil, "delete: "+err.Error())
					return
				}
			}
		}()
	}
	puts.Wait()
	close(stop)
	removers.Wait()
	if failed.Load() > 0 {
		t.Fatalf("a put into a directory that was removed meanwhile failed: %v", first.Load())
	}
}

// ...and the put object is whole and there, which a Put that had to retry
// must not lose: after the removers have gone, every key put and not deleted
// can be read back.
func TestAPutThatMeetsARemovedDirectoryStoresItsObject(t *testing.T) {
	root := t.TempDir()
	st := NewPosix(root)
	ctx := context.Background()
	dir := filepath.Join(root, "images", "item")
	stop := make(chan struct{})
	var removers sync.WaitGroup
	removers.Add(1)
	go func() {
		defer removers.Done()
		runtime.LockOSThread()
		for {
			select {
			case <-stop:
				return
			default:
				os.Remove(dir) //nolint:errcheck
			}
		}
	}()
	const rounds = 5000
	for i := range rounds {
		key := fmt.Sprintf("images/item/keep-%d", i)
		if err := st.Put(ctx, key, strings.NewReader("new")); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		if size, err := st.Stat(ctx, key); err != nil || size != 3 {
			t.Fatalf("object %d after its put: %d bytes, %v", i, size, err)
		}
		if err := st.Delete(ctx, key); err != nil { // Empties the directory again for the next put.
			t.Fatal(err)
		}
	}
	close(stop)
	removers.Wait()
}
