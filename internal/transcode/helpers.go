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
// a slot claim, other workers first.
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

// freeSlots is SQL for the worker slots no job holds or waits for. alive and
// queue are the placeholders of the seconds a node and a queued job count for.
func freeSlots(alive, queue string) string {
	nodes := `SELECT id FROM transcode_nodes WHERE seen_at > now() - make_interval(secs => ` + alive + `)`
	return `((SELECT COALESCE(sum(slots), 0) FROM transcode_nodes WHERE seen_at > now() - make_interval(secs => ` + alive + `))
		- (SELECT count(*) FROM transcode_jobs a WHERE (a.state = 'queued' AND a.created_at > now() - make_interval(secs => ` + queue + `))
			OR (a.state = 'running' AND a.lease_expires_at > now() AND a.worker IN (` + nodes + `))))`
}

// live is SQL for a job someone is working on or has yet to claim.
func live(j string) string {
	return `(` + j + `.state = 'queued' AND ` + j + `.created_at > now() - make_interval(secs => ` + fmt.Sprint(queueTimeout.Seconds()) + `)
		OR ` + j + `.state = 'running' AND ` + j + `.lease_expires_at > now())`
}

// startHelper starts a helper job for the first chunk ahead of the job that is
// not made, claimed or about to be, and within the lead. next is the job's
// next segment. It starts none unless a slot stays free for a new viewer.
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
// helper was queued or nothing more should be: no viewer, or no slot to spare. Under the lock that serializes every decision to
// take a slot for a helper, as ensure's serializes a spec's jobs.
func (w *Worker) insertHelper(ctx context.Context, job claimed, chunk, start int) (done bool, err error) {
	raw, err := json.Marshal(job.Spec)
	if err != nil {
		return true, err
	}
	size := w.chunkSize()
	alive := workerAlive.Seconds()
	err = pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('helpers', 0)), pg_advisory_xact_lock(hashtextextended($1, 0))", job.Hash); err != nil {
			return err
		}
		var taken, watched bool
		var free int
		err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM transcode_jobs j WHERE j.spec_hash = $1 AND `+live("j")+`
				AND (j.start_segment / $3 = $2 OR (j.start_segment < $2 * $3 AND j.next_segment >= $2 * $3 - 1 AND j.next_segment < ($2 + 1) * $3)))
			-- A helper that failed in the chunk lately is not tried again until
			-- failureMemory has passed: the viewer's own job makes the chunk.
			OR EXISTS (SELECT 1 FROM transcode_jobs f WHERE f.spec_hash = $1 AND f.helper AND f.state = 'failed'
				AND f.start_segment / $3 = $2 AND f.ended_at > now() - make_interval(secs => $7)),
			EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = $1 AND NOT v.stopped AND v.last_seen_at > now() - make_interval(secs => $4)),
			`+freeSlots("$5", "$6")+`::int`,
			job.Hash, chunk, size, w.idle().Seconds(), alive, orDefault(w.QueueTimeout, queueTimeout).Seconds(), failureMemory.Seconds()).Scan(&taken, &watched, &free)
		if err != nil {
			return err
		}
		switch {
		case taken:
			return nil
		case !watched, free < 2:
			done = true // A slot stays free for a viewer who has not asked yet.
			return nil
		}
		done = true
		_, err = tx.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper)
			VALUES ($1, $2, 'helper', $3, $3, true)`, job.Hash, raw, start)
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

// preemptHelper cancels one helper if no slot is free for the viewer's job
// about to be inserted, so a viewer never waits behind a helper. A queued
// helper goes first, else the running one furthest ahead, and only one whose
// slot freeSlots counts (queued, or running on a live worker): cancelling a
// dead worker's helper would free nothing. Only helpers are cancelled; a
// viewer's job is never. It reports whether it cancelled one, so the caller
// can send preemptHint once the transaction commits and the helper stops at
// once rather than at its next heartbeat. What the helper stored stays, and
// the rest of its chunk is made when a viewer reaches it, as after a seek.
// Called under the helpers lock. The outer state test is not redundant: a
// helper that finished after the subquery chose it is re-checked on its new
// row, so it stays done rather than turning cancelled.
func preemptHelper(ctx context.Context, tx pgx.Tx, queue time.Duration) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'cancelled', error = 'a new viewer needed its slot', ended_at = now() WHERE id = (
			SELECT h.id FROM transcode_jobs h WHERE h.helper AND (h.state = 'queued' AND h.created_at > now() - make_interval(secs => $2)
				OR h.state = 'running' AND h.lease_expires_at > now()
					AND h.worker IN (SELECT id FROM transcode_nodes WHERE seen_at > now() - make_interval(secs => $1)))
			ORDER BY h.state = 'queued' DESC, h.start_segment DESC LIMIT 1)
		AND state IN ('queued', 'running') AND `+freeSlots("$1", "$2")+` < 1`, workerAlive.Seconds(), queue.Seconds())
	return tag.RowsAffected() > 0, err
}
