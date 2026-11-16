package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"jellyscale/internal/store"
)

// JS-107: a put has two ends, the scratch file it reads and the store it writes. The
// error of a put names the store only when the store failed.

// passingStore reads the whole body and gives the body's read error on, in the form told.
type passingStore struct {
	store.Store
	form func(error) error
}

func (s passingStore) Put(_ context.Context, key string, body io.Reader) error {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return s.form(err)
	}
	return nil
}

// resettingStore reads the whole body, which is sound, and then loses its connection.
type resettingStore struct{ store.Store }

func (resettingStore) Put(_ context.Context, _ string, body io.Reader) error {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	return &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
}

// A scratch file that opens and cannot be read (here a directory, as a file on a failing
// disk) fails the job as a local file does: the recorded error is the file's, and the
// transcode store, which only passed the error on, is not named, whatever form the store
// gave the error on in.
func TestAScratchFileThatCannotBeReadDuringAPutIsNotBlamedOnTheStore(t *testing.T) {
	for name, s := range map[string]store.Store{
		"the posix store":                               store.NewPosix(t.TempDir()),
		"a store that gives the error on as it is":      passingStore{form: func(err error) error { return err }},
		"a store that wraps the error":                  passingStore{form: func(err error) error { return fmt.Errorf("put segments/h/0: %w", err) }},
		"a store that tells the error in its own words": passingStore{form: func(err error) error { return errors.New("put failed: " + err.Error()) }},
	} {
		t.Run(name, func(t *testing.T) {
			unreadable := t.TempDir()
			w := &Worker{Segments: s, SegmentsURI: transcodeStore}
			err := w.put(context.Background(), "segments/h/0", unreadable)
			if err == nil {
				t.Fatal("a put of a file that cannot be read succeeded")
			}
			state, reason := endState(context.Background(), err)
			if state != "failed" {
				t.Errorf("ended %s (%q), want failed", state, reason)
			}
			if strings.Contains(reason, transcodeStore) {
				t.Errorf("a local read error is recorded as the transcode store's failure: %q", reason)
			}
			if !strings.Contains(reason, unreadable) {
				t.Errorf("the recorded error %q does not name the file that could not be read, %s", reason, unreadable)
			}
		})
	}
}

// The other way round: the file is read to its end, and the store fails. That is the store's.
func TestAStoreThatFailsAfterReadingASoundFileIsNamed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "0.m4s")
	if err := os.WriteFile(file, make([]byte, 1<<16), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Segments: resettingStore{}, SegmentsURI: transcodeStore}
	state, reason := endState(context.Background(), w.put(context.Background(), "segments/h/0", file))
	if state != "failed" || !strings.Contains(reason, transcodeStore) {
		t.Errorf("ended %s %q, want failed with %s", state, reason, transcodeStore)
	}
}
