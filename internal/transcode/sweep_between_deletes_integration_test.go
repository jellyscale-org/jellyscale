//go:build integration

package transcode

import (
	"context"
	"fmt"
	"path"
	"sync"
	"testing"
	"time"
)

// JS-078: a sweep asks again for a viewer or a running job before every object
// it deletes, not only before a spec's first. The sweep that Verify found
// wanting checked once and then removed the rest of the spec, so a viewer who
// asked for it after the first delete, and whose job stored segments, lost them
// to the sweep that went on (adversarial review, 2026-11-17).
//
// The sweep is held between its second and third delete of one spec, in the
// store's Delete (the spy's afterDelete), and then a viewer on another replica
// (its own pool) asks for the spec, and its job stores segments. Then the sweep
// is released. A build that holds no lock across its deletes lets the viewer
// through whole, and the check waits for the viewer's job to end (bounded at a
// minute, a hang's bound) before it releases the sweep. A build whose delete
// runs under the spec's lock (as it does now) makes the viewer wait on it: the
// sweep is released as soon as the viewer waits, as in the single-delete check,
// and the viewer's job stores its segments after the sweep went on, where the
// sweep's next check must see the viewer. The check asserts nothing about how
// long anything took.
//
// A mutant it must fail every run: deleting the rest of the spec without asking
// again for a viewer or a job (evict's loop calling Segments.Delete outside
// locked()).
func TestAViewerWhoAsksForASpecBetweenTwoDeletesOfItsSweepKeepsEverySegmentTheirJobStores(t *testing.T) {
	for _, tg := range targets(t) {
		for _, ask := range []string{"a segment already deleted", "the segment due next", "a segment still to be deleted"} {
			t.Run(tg.name+"/"+ask, func(t *testing.T) {
				tg := tg.fresh(t)
				st := tg.open(t)
				var mu sync.Mutex
				var stored []string
				writer := &spyStore{Store: st}
				writer.afterPut = func(key string) {
					mu.Lock()
					stored = append(stored, key)
					mu.Unlock()
				}
				r := newRig(t, writer)
				r.worker(t, nil)
				ctx := context.Background()
				first := r.jobs(t)
				if _, err := r.fetch(t, first, psid(0), 0); err != nil {
					t.Fatal(err)
				}
				idle := func() bool {
					return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
				}
				waitFor(t, "the film's job to end", time.Minute, idle)
				if err := first.Stop(ctx, r.user, psid(0), "device", ""); err != nil {
					t.Fatal(err)
				}
				keys := keysUnder(t, st, "segments/"+r.hash)
				if len(keys) < 6 {
					t.Fatalf("the job stored %v: too few to take a viewer's request between the third delete and the rest", keys)
				}
				// The sweep removes in key order: keys[0] and keys[1] first, then keys[2] (held before it).
				// The last key is init.mp4, so the last segment is the one before it.
				asked := map[string]string{"a segment already deleted": keys[0], "the segment due next": keys[2], "a segment still to be deleted": keys[len(keys)-2]}[ask]
				number := func(key string) int {
					var n int
					if _, err := fmt.Sscanf(path.Base(key), "%d.mp4", &n); err != nil {
						t.Fatalf("the key to ask for is %s", key)
					}
					return n
				}
				n := number(asked)
				// A segment that is still stored is served from the store, with no job, so the
				// viewer who asks for one then seeks back to a segment the sweep has deleted:
				// that request starts the job whose segments must survive.
				asks := []int{n}
				if asked != keys[0] {
					asks = append(asks, number(keys[0]))
				}
				mu.Lock()
				stored = nil
				mu.Unlock()

				reached, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				letGo := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(letGo)
				spy := &spyStore{Store: st}
				var deletes int
				spy.afterDelete = func(string) {
					deletes++ // The sweep's deletes of one spec run one after another.
					if deletes == 2 {
						close(reached)
						<-release
					}
				}
				log, _ := newLogs()
				swept := make(chan error, 1)
				go func() { swept <- r.sweeper(spy, 1, log).Sweep(ctx) }()
				select {
				case <-reached:
				case err := <-swept:
					t.Fatalf("the sweep ended (%v) before its second delete of the unwatched spec: nothing was held", err)
				case <-time.After(time.Minute):
					t.Fatal("timed out waiting for the sweep to make its second delete")
				}
				if got := keysUnder(t, st, "segments/"+r.hash); len(got) != len(keys)-2 {
					t.Fatalf("held before the third delete the store should hold %d of the spec's objects, it holds %d", len(keys)-2, len(got))
				}

				viewerPool := openNamed(t, r.dsn, "viewer-between-deletes")
				vctx, stop := context.WithCancel(ctx)
				defer stop()
				hub := &Hub{Pool: viewerPool, Log: r.log}
				go hub.Run(vctx)
				viewer := &Jobs{Pool: viewerPool, Segments: st, Hub: hub, QueueTimeout: 5 * time.Second, WaitTimeout: 90 * time.Second}
				type answer struct {
					data []byte
					err  error
				}
				answered := make(chan answer, 1)
				go func() {
					var last answer
					for _, ask := range asks {
						last.data, last.err = r.fetchAt(t, viewer, psid(1), ask, 0)
						if last.err != nil {
							break
						}
					}
					answered <- last
				}()
				viewerDone := make(chan struct{})
				var got answer
				go func() { got = <-answered; close(viewerDone) }()
				r.untilEither(t, "the viewer to be answered or to wait on a lock of the held sweep", viewerDone, "viewer-between-deletes")
				select {
				case <-viewerDone:
					// Nothing of the sweep's was locked: the job is the viewer's, and it ends before the sweep goes on.
					waitFor(t, "the viewer's job to end while the sweep is held", time.Minute, idle)
				default:
				}
				letGo()
				select {
				case <-viewerDone:
				case <-time.After(time.Minute):
					t.Fatal("the viewer asking for a spec in the middle of its removal was not answered in a minute")
				}
				if got.err != nil {
					t.Fatalf("the viewer asking for %s in the middle of the spec's removal got an error: %v", asked, got.err)
				}
				waitFor(t, "the viewer's job to end", time.Minute, idle)
				select {
				case err := <-swept:
					if err != nil {
						t.Fatalf("the sweep: %v", err)
					}
				case <-time.After(time.Minute):
					t.Fatal("the released sweep did not return in a minute")
				}

				mu.Lock()
				written := append([]string{}, stored...)
				mu.Unlock()
				if len(written) == 0 {
					t.Fatal("the viewer's request started no job that stored a segment, so nothing was checked")
				}
				var lost []string
				for _, key := range written {
					if _, err := st.Stat(ctx, key); err != nil {
						lost = append(lost, key)
					}
				}
				if len(lost) > 0 {
					t.Errorf("the sweep removed %d of the %d objects the viewer's job stored after the request (it checked for a viewer only before its first delete): %v", len(lost), len(written), lost)
				}
				if _, err := st.Stat(ctx, asked); err != nil {
					t.Errorf("%s, which the viewer asked for while the sweep was held, is gone: %v", asked, err)
				}
				for _, ask := range []int{n, n + 1} {
					if data, err := r.fetchAt(t, viewer, psid(1), ask, 0); err != nil || len(data) == 0 {
						t.Errorf("segment %d after the sweep: %d bytes, %v", ask, len(data), err)
					}
				}
			})
		}
	}
}
