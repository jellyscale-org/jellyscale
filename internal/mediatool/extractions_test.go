package mediatool

import (
	"context"
	"testing"
	"time"
)

// JS-177 (D-070): a worker counts an extraction of its own by the runs of tools that are not a transcode's, process-wide
// (Extracting). A run that is counted and never uncounted would make every later job of the process read as beside an
// extraction, so the count is back to zero after each way a Run can end. Extracting(0) is the count alone.

func wantNoRunCounted(t *testing.T, how string) {
	t.Helper()
	if Extracting(0) {
		t.Errorf("%s: the count of runs is above zero after Run returned", how)
	}
}

// While a run goes on it is counted, and a step of a transcode (OfTranscode) is not; one that ended is "beside" for the window.
func TestARunIsCountedWhileItGoesOnAndAStepOfATranscodeIsNot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options Options
		counted bool
	}{{"an extraction", Options{}, true}, {"a step of a transcode", Options{OfTranscode: true}, false}} {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				Run(ctx, "sleep", []string{"5"}, tc.options) //nolint:errcheck
			}()
			time.Sleep(300 * time.Millisecond)
			if got := Extracting(0); got != tc.counted {
				t.Errorf("%s while it runs: counted %v, want %v", tc.name, got, tc.counted)
			}
			cancel()
			<-done
		}()
	}
	if !Extracting(time.Minute) {
		t.Error("an extraction that just ended is not beside the next minute")
	}
	wantNoRunCounted(t, "after both")
}

// Each way a Run can end leaves the count at zero: it succeeds, the tool exits with an error status, the context ends
// while it runs, the binary cannot be started, and the stall limit ends it.
func TestTheCountOfRunsIsZeroAfterEachWayARunEnds(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		if _, err := Run(context.Background(), "true", nil, Options{}); err != nil {
			t.Fatal(err)
		}
		wantNoRunCounted(t, "success")
	})
	t.Run("the tool exits with an error status", func(t *testing.T) {
		if _, err := Run(context.Background(), "false", nil, Options{}); err == nil {
			t.Fatal("false succeeded")
		}
		wantNoRunCounted(t, "an error status")
	})
	t.Run("the context is cancelled mid-run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		if _, err := Run(ctx, "sleep", []string{"30"}, Options{}); err == nil {
			t.Fatal("a cancelled run reported no error")
		}
		wantNoRunCounted(t, "a cancelled context")
	})
	t.Run("the binary cannot be started", func(t *testing.T) {
		if _, err := Run(context.Background(), "/nonexistent/tool-js177", nil, Options{}); err == nil {
			t.Fatal("a missing binary ran")
		}
		wantNoRunCounted(t, "a binary that cannot start")
	})
	t.Run("the stall limit ends it", func(t *testing.T) {
		if _, err := Run(context.Background(), "sleep", []string{"30"}, Options{Stall: 300 * time.Millisecond}); err == nil {
			t.Fatal("a stalled run reported no error")
		}
		wantNoRunCounted(t, "a stall")
	})
	t.Run("background priority", func(t *testing.T) {
		if _, err := Run(context.Background(), "true", nil, Options{Background: true}); err != nil {
			t.Fatal(err)
		}
		wantNoRunCounted(t, "a background run")
	})
}
