package transcode

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"jellyscale/internal/store"
)

// Which failures are made again (JS-088): a store's, and ffmpeg killed by a signal. ffmpeg failing on its own, or a
// file that is gone, is the file's.
func TestWhichFailuresAreNotTheFiles(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -KILL $$")
	killedErr := killed.Run()
	failed := exec.Command("sh", "-c", "exit 183")
	failedErr := failed.Run()
	if killedErr == nil || failedErr == nil {
		t.Fatal("the shells did not fail")
	}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"a store's refusal":          {fromStore("file:///t", errors.New("connection refused")), true},
		"a store's refusal, wrapped": {fmt.Errorf("storing segment 3: %w", fromStore("file:///t", errors.New("i/o timeout"))), true},
		"ffmpeg killed by a signal":  {fmt.Errorf("ffmpeg: %w: tail", killedErr), true},
		"ffmpeg exiting with 183":    {fmt.Errorf("ffmpeg: %w: tail", failedErr), false},
		"a file the store lacks":     {fromStore("file:///t", store.ErrNotFound), false},
		"the index being wrong":      {fmt.Errorf("segment 3: %w", errWrongIndex), false},
		"an error no one marked":     {errors.New("the input ended early"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := notTheFiles(c.err); got != c.want {
				t.Errorf("notTheFiles(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
