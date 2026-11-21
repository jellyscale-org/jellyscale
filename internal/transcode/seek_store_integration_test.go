//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// seekCutStore is the film's store as a worker sees it: the first refuses reads of the film are refused, and the next cuts are
// cut after after bytes by an error (a connection reset), as an S3 that drops a connection does.
type seekCutStore struct {
	store.Store
	refuses, cuts atomic.Int64
	after         int64
}

func (c *seekCutStore) LocalPath(string) (string, bool) { return "", false }

func (c *seekCutStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if c.refuses.Add(-1) >= 0 {
		return nil, errStoreDown
	}
	body, err := c.Store.Open(ctx, key, offset)
	if err != nil || c.cuts.Add(-1) < 0 {
		return body, err
	}
	return &seekCutBody{ReadCloser: body, room: c.after}, nil
}

type seekCutBody struct {
	io.ReadCloser
	room int64
}

func (b *seekCutBody) Read(p []byte) (int, error) {
	if b.room <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.room)])
	b.room -= int64(n)
	return n, err
}

// A seek's job that ends because the store refused or cut a read is not the film's failure: the waiting request is answered with
// the segment after the job is made again (at most 3 times, maxRestarts), and the film is not remembered as one that cannot be
// played. A store that refuses or cuts every read ends in the failure, after 1 + 3 jobs and no more.
func TestASeeksJobEndedByTheStoreIsMadeAgainAndNotRemembered(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "aac-2")
	media := films.filmOf(t, a, 60)
	s := films.rig(t, media, a)
	const segment = 9
	cases := []struct {
		name string
		set  func(c *seekCutStore, n int64) // makes the store fail the first n reads of the film
	}{
		{"refuses a read", func(c *seekCutStore, n int64) { c.refuses.Store(n) }},
		{"cuts a read", func(c *seekCutStore, n int64) { c.cuts.Store(n) }},
	}
	for _, c := range cases {
		for _, n := range []int{1, 2, 3, 1000} {
			t.Run(fmt.Sprintf("%s: the first %d", c.name, n), func(t *testing.T) {
				s.fresh(t)
				cs := &seekCutStore{Store: newMeter(media, 0), after: 64 << 10}
				c.set(cs, int64(n))
				stop := s.rig.worker(t, func(w *Worker) {
					w.Slots, w.NoHelpers, w.Lead = 1, true, 8*time.Second
					w.Media = func(uri string) (store.Store, error) {
						if strings.TrimPrefix(uri, "file://") == s.media {
							return cs, nil
						}
						return store.Open(uri)
					}
				})
				defer stop()
				j := s.jobs(t)
				_, err := s.fetchAt(t, j, psid(0), segment, segment)
				rows := s.jobsOf(t, s.hash)
				if n == 1000 {
					var failed FailedError
					if !errors.As(err, &failed) {
						t.Fatalf("a store that fails every read was answered %v, want the failure", err)
					}
					if len(rows) > 1+maxRestarts {
						t.Errorf("%d jobs were made for the request, want at most %d: %+v", len(rows), 1+maxRestarts, rows)
					}
					return
				}
				if err != nil {
					t.Fatalf("a seek whose store failed %d reads: %v (jobs %+v)", n, err, rows)
				}
				if len(rows) > 1+maxRestarts {
					t.Errorf("%d jobs were made for the request, want at most %d: %+v", len(rows), 1+maxRestarts, rows)
				}
				if reason, err := j.Unavailable(context.Background(), s.spec, s.user, "device"); err != nil || reason != "" {
					t.Errorf("the film is refused after reads the store failed: %q, %v (jobs %+v)", reason, err, rows)
				}
			})
		}
	}
}
