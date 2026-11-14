package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/playback"
)

// Parallel transcoding (DOMAINS 58). A re-encode is cut into chunks of
// ChunkSegments segments. A job's ffmpeg starts at the segment the job wants
// and stops at the end of its chunk, and the job goes on into the next chunk
// with a new one unless another job has it, so jobs meet only at chunk starts.
// A job that runs below HelpBelow times real time asks for helpers: ordinary
// jobs in transcode_jobs, flagged helper, for later chunks, which workers with
// a claim of cores, other workers first.
const (
	// defaultChunkSegments is 16 seconds: a seek pays for the part of its chunk
	// before the segment it wants (the encoder has to be warm), a chunk start
	// pays an ffmpeg start, and the chunks should be many in a lead.
	defaultChunkSegments = 4
	// defaultHelpBelow is the speed under which a job gets helpers. Real time is
	// not enough margin: a job at 1.0x has nothing to spare for a seek.
	defaultHelpBelow = 1.2
)

func (w *Worker) chunkSize() int {
	if w.ChunkSegments > 0 {
		return w.ChunkSegments
	}
	return defaultChunkSegments
}

// chunked says whether the job makes its segments in chunks. A copy is cheap
// and cut at the source's keyframes, so it is not.
func (w *Worker) chunked(job claimed) bool { return !job.Spec.Copy }

func (w *Worker) helpBelow() float64 {
	if w.HelpBelow > 0 {
		return w.HelpBelow
	}
	return defaultHelpBelow
}

// live is SQL for a job someone is working on or has yet to claim.
func live(j string) string {
	return `(` + j + `.state = 'queued' AND ` + j + `.created_at > now() - make_interval(secs => ` + fmt.Sprint(queueTimeout.Seconds()) + `)
		OR ` + j + `.state = 'running' AND ` + j + `.lease_expires_at > now())`
}

// startHelper starts a helper job for the first chunk ahead of the job that is
// not made, claimed or about to be, and within the lead. next is the job's
// next segment. It starts none unless room stays for a new viewer.
func (w *Worker) startHelper(ctx context.Context, job claimed, next int, layout playback.Layout) error {
	at, served, _, err := w.furthest(ctx, job, next)
	if err != nil || !served {
		return err
	}
	limit := min(limitFor(layout, at, w.lead()), layout.Count())
	size := w.chunkSize()
	for chunk := next / size; chunk*size < limit; chunk++ {
		start, missing := w.firstMissing(ctx, job.Hash, chunk*size, min((chunk+1)*size, layout.Count()))
		if !missing {
			continue
		}
		if start >= limit {
			return nil
		}
		if done, err := w.insertHelper(ctx, job, chunk, start); done || err != nil {
			return err
		}
	}
	return nil
}

// firstMissing is the first segment of [from, to) that is not stored. A chunk
// whose first and last are stored is taken as whole; a dead helper's leaves a
// gap at its end.
func (w *Worker) firstMissing(ctx context.Context, hash string, from, to int) (int, bool) {
	if w.stored(ctx, hash, from) && w.stored(ctx, hash, to-1) {
		return 0, false
	}
	for n := from; n < to; n++ {
		if !w.stored(ctx, hash, n) {
			return n, true
		}
	}
	return 0, false
}

// insertHelper queues a helper that starts at segment start of the chunk. done
// is false if the chunk is taken, so the next may be tried, and true if the
// helper was queued or nothing more should be: no viewer, or no cores to spare.
// Under the lock that serializes every decision to take cores for a helper, as
// ensure's serializes a spec's jobs.
func (w *Worker) insertHelper(ctx context.Context, job claimed, chunk, start int) (done bool, err error) {
	raw, err := json.Marshal(job.Spec)
	if err != nil {
		return true, err
	}
	size := w.chunkSize()
	err = pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('helpers', 0)), pg_advisory_xact_lock(hashtextextended($1, 0))", job.Hash); err != nil {
			return err
		}
		var taken, watched bool
		err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM transcode_jobs j WHERE j.spec_hash = $1 AND `+live("j")+`
				AND (j.start_segment / $3 = $2 OR (j.start_segment < $2 * $3 AND j.next_segment >= $2 * $3 - 1 AND j.next_segment < ($2 + 1) * $3)))
			-- A helper that failed in the chunk lately is not tried again until
			-- failureMemory has passed: the viewer's own job makes the chunk.
			OR EXISTS (SELECT 1 FROM transcode_jobs f WHERE f.spec_hash = $1 AND f.helper AND f.state = 'failed'
				AND f.start_segment / $3 = $2 AND f.ended_at > now() - make_interval(secs => $5)),
			EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = $1 AND NOT v.stopped AND v.last_seen_at > now() - make_interval(secs => $4))`,
			job.Hash, chunk, size, w.idle().Seconds(), failureMemory.Seconds()).Scan(&taken, &watched)
		if err != nil {
			return err
		}
		if taken {
			return nil
		}
		done = true
		if !watched {
			return nil
		}
		fits, err := roomForHelper(ctx, tx, orDefault(w.QueueTimeout, queueTimeout), job.Spec.Cost())
		if err != nil || !fits {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, cost)
			VALUES ($1, $2, 'helper', $3, $3, true, $4, $5)`, job.Hash, raw, start, playback.OutputVersion, job.Spec.Cost())
		return err
	})
	if err == nil && done {
		Notify(ctx, w.Pool, jobHint)
	}
	return done, err
}

// chunkFree says whether the job may go on into the chunk that begins at
// segment next: no one has made its first segment, and no live job started in
// the chunk, at or before next (a helper, or a seek's job, which then makes it).
func (w *Worker) chunkFree(ctx context.Context, job claimed, next int) (bool, error) {
	if w.stored(ctx, job.Hash, next) {
		return false, nil
	}
	var taken bool
	err := w.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcode_jobs j WHERE j.spec_hash = $1 AND j.id <> $2 AND `+live("j")+`
		AND j.start_segment <= $3 AND $3 < (j.start_segment / $4 + 1) * $4)`, job.Hash, job.ID, next, w.chunkSize()).Scan(&taken)
	return !taken, err
}

// roomForHelper says whether a helper of the cost fits on a worker, with the
// queued jobs counted as placed, and leaves room for a viewer who has not asked
// yet: a job of the same cost still fits somewhere after it. Under the helpers
// lock, so that helpers decided at once do not each see the room the others take.
func roomForHelper(ctx context.Context, tx pgx.Tx, queue time.Duration, cost float64) (bool, error) {
	nodes, jobs, err := readCapacity(ctx, tx, capQuery{Filter: "true", Queue: queue})
	if err != nil {
		return false, err
	}
	all := func(capJob) bool { return true }
	l := newLedger(nodes, jobs, all)
	l.placeQueued(jobs, all)
	helper := capJob{Cost: cost}
	if _, placed := l.place(helper); !placed {
		return false, nil
	}
	_, spare := l.room(helper)
	return spare, nil
}
