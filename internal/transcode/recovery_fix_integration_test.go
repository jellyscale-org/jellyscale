//go:build integration

package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// recoveryHealingCutMedia cuts only the first film body opened through this
// handle. store.Reader resumes that body at its offset, so ffmpeg never sees
// the cut. Each transcode job gets a fresh handle and therefore a fresh cut.
type recoveryHealingCutMedia struct {
	store.Store
	film  string
	after int64
	cut   bool
}

func (s *recoveryHealingCutMedia) LocalPath(string) (string, bool) { return "", false }

func (s *recoveryHealingCutMedia) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := s.Store.Open(ctx, key, offset)
	if err != nil || key != s.film || offset != 0 || s.cut {
		return body, err
	}
	s.cut = true
	return &recoveryHealedBody{ReadCloser: body, allowance: s.after}, nil
}

type recoveryHealedBody struct {
	io.ReadCloser
	allowance int64
}

func (b *recoveryHealedBody) Read(p []byte) (int, error) {
	if b.allowance <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.allowance)])
	b.allowance -= int64(n)
	return n, err
}

func armHealingCut(t *testing.T, r *rig, key string, after int64, change func(*Worker)) {
	t.Helper()
	info, err := os.Stat(filepath.Join(film(t), key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	r.worker(t, func(w *Worker) {
		opened := w.Media
		w.Media = func(uri string) (store.Store, error) {
			inner, err := opened(uri)
			if err != nil {
				return nil, err
			}
			return &recoveryHealingCutMedia{Store: inner, film: key, after: after}, nil
		}
		change(w)
	})
}

// A cut the Reader healed is not the job's error. In particular it must not
// hide errWrongIndex, which drops the index and enables the copy fallback.
func TestAHealedStoreCutDoesNotHideAWrongIndex(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = ARRAY(SELECT CASE WHEN n = 1 THEN k ELSE k + 10000000 END FROM unnest(keyframes) WITH ORDINALITY AS t(k, n) ORDER BY n)"); err != nil {
		t.Fatal(err)
	}
	armHealingCut(t, r, "film.mkv", recoveryCutAfter, func(*Worker) {})

	_, _ = r.fetch(t, r.jobs(t), psid(0), 0)
	r.idle(t)
	if states := r.states(t); len(states) != 1 || states[0] != "failed" {
		t.Errorf("jobs ended %v, want one failed job whose wrong index is not retried as a store cut", states)
	}
	reason := mustScalar(t, r, "SELECT error FROM transcode_jobs ORDER BY id LIMIT 1")
	if !strings.Contains(reason, "keyframe index is wrong") || strings.Contains(reason, "unexpected EOF") {
		t.Errorf("the failed job's reason is %q, want the wrong index rather than the healed cut", reason)
	}
	if got := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); got != 0 {
		t.Errorf("the wrong index was kept (%d entries): the copy cannot fall back", got)
	}
	if got := r.count(t, "SELECT cardinality(dropped_keyframes) FROM media_sources"); got != 9 {
		t.Errorf("the dropped index has %d entries, want the original 9 for the viewer's playlist", got)
	}
}

// The same healed cut must not turn data ffmpeg rejects into four retryable
// store failures. The file is read through the loopback object-store path.
func TestAHealedStoreCutDoesNotMakeABrokenFileRetryable(t *testing.T) {
	r := newRig(t, nil)
	whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(film(t), "garbled.mkv"), whole[:4096], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = 'garbled.mkv'"); err != nil {
		t.Fatal(err)
	}
	armHealingCut(t, r, "garbled.mkv", 1000, func(*Worker) {})
	j := r.jobs(t)
	j.WaitTimeout = recoveryWait

	_, err = r.fetch(t, j, psid(0), 0)
	var failed FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("got %v, want the file's failure", err)
	}
	r.idle(t)
	if states := r.states(t); len(states) != 1 || states[0] != "failed" {
		t.Errorf("jobs ended %v, want one failed job for a file ffmpeg cannot read", states)
	}
	reason := mustScalar(t, r, "SELECT error FROM transcode_jobs ORDER BY id LIMIT 1")
	if strings.Contains(reason, "unexpected EOF") {
		t.Errorf("the healed cut replaced ffmpeg's error: %q", reason)
	}
	if reason, err := j.Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || !strings.Contains(reason, "the last attempt failed") {
		t.Errorf("PlaybackInfo is not refused for the broken file: %q, %v", reason, err)
	}
}

// hls.js abandons a request and asks again. The three-fresh-job bound and the
// failure memory therefore belong to the spec, not to one request's lifetime.
func TestTheRestartBoundAndFailureMemoryHoldAcrossRequests(t *testing.T) {
	r := newRig(t, nil)
	var change []func(*Worker)
	mustHold := armPutRefused(t, r, &change, 0)
	r.worker(t, func(w *Worker) {
		for _, apply := range change {
			apply(w)
		}
	})
	j := r.jobs(t)
	j.WaitTimeout = recoveryWait
	j.Segments = r.segments

	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan error, 1)
	go func() {
		_, err := j.Segment(ctx, r.spec, r.hash, r.viewer(psid(0)), 0, 0)
		answer <- err
	}()
	waitFor(t, "the first request's job to fail", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") >= 1
	})
	cancel() // What hls.js does at ten seconds; no later request-local reset can run.
	if err := <-answer; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned request ended with %v, want context cancellation", err)
	}

	_, err := r.fetch(t, j, psid(1), 0)
	var failed FailedError
	if !errors.As(err, &failed) || !strings.Contains(failed.Reason, mustHold) {
		t.Fatalf("the next request got %v, want the persistent cause", err)
	}
	r.idle(t)
	if states := r.states(t); len(states) > 1+recoveryFreshJobs {
		t.Errorf("%d jobs across two requests, want at most the first and %d fresh ones: %v", len(states), recoveryFreshJobs, states)
	}
	if reason, err := j.Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || !strings.Contains(reason, "the last attempt failed") {
		t.Errorf("PlaybackInfo does not refuse the persistent cause: %q, %v", reason, err)
	}
	before := r.count(t, "SELECT count(*) FROM transcode_jobs")
	if _, err := r.fetch(t, j, psid(2), 0); !errors.As(err, &failed) || !strings.Contains(failed.Reason, "the last attempt failed") {
		t.Errorf("another request got %v, want the spec's remembered failure", err)
	}
	if after := r.count(t, "SELECT count(*) FROM transcode_jobs"); after != before {
		t.Errorf("a request within failureMemory started another job: %d before, %d after", before, after)
	}
}

// A file cut short in the store, read as an object store through a cut that heals: the input ending early is the
// file's failure, so there is one job, no fresh one, and the copy keeps its index.
func TestAHealedStoreCutDoesNotMakeAnInputThatEndsEarlyRetryable(t *testing.T) {
	r := newRig(t, nil)
	whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	file := "cut-healed.mkv"
	if err := os.WriteFile(filepath.Join(film(t), file), whole, 0o644); err != nil {
		t.Fatal(err)
	}
	r.copied(t, file)
	if err := os.WriteFile(filepath.Join(film(t), file), whole[:len(whole)*2/5], 0o644); err != nil {
		t.Fatal(err)
	}
	armHealingCut(t, r, file, 1000, func(*Worker) {})
	j := r.jobs(t)
	j.WaitTimeout = recoveryWait
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatalf("segment 0 is whole and should be served: %v", err)
	}
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	next := r.count(t, "SELECT next_segment FROM transcode_jobs")
	var failed FailedError
	if _, err := r.fetch(t, j, psid(0), next); !errors.As(err, &failed) || strings.Contains(failed.Reason, "unexpected EOF") {
		t.Errorf("the segment the cut input never gave (%d) was answered with %v, want the input's end", next, err)
	}
	r.idle(t)
	if states := r.states(t); len(states) != 1 || states[0] != "failed" {
		t.Errorf("the jobs ended %v, want the one failed job and no fresh one within failureMemory", states)
	}
	if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
		t.Errorf("the index has %d keyframes after an input that ended early, want 9", n)
	}
}
