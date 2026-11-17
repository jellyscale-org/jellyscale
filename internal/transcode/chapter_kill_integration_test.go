//go:build integration

package transcode

// JS-112: a worker process killed with SIGKILL while two chapter-picture jobs
// (chapterSlots) each have ffmpeg running leaves none of those ffmpeg
// processes. The worker is this test binary run again (workerProcess), so the
// kill is real. The ffmpeg here is a stand-in that notes its pid and waits, and
// only the processes whose parent is the killed worker are counted, so another
// task's ffmpeg on the machine neither passes nor fails the test.

import (
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/library"
	"jellyscale/internal/testenv"
)

func TestAKilledWorkerLeavesNoFfmpegOfItsChapterPictureJobs(t *testing.T) {
	r := newRig(t, nil)
	r.track(t)
	for i := range chapterSlots + 1 { // One more than the slots, so a job waits.
		id := uuid.NewString()
		r.exec(t, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto)
			SELECT $1, library_id, 'Movie', $2, $2, $2, '{}' FROM items LIMIT 1`, id, fmt.Sprint("Chaptered ", i))
		r.exec(t, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, chapters)
			VALUES ($1, $1, 'film.mkv', 1, $2, 'mkv', '[{"Type":"Video"}]', '[{"StartPositionTicks":0,"Name":"a"}]')`, id, fmt.Sprint("etag-", i))
	}
	r.exec(t, "UPDATE libraries SET chapter_images = true")

	held := testenv.Hold(t, "ffmpeg") // The jobs' ffmpeg never ends of itself.
	worker := r.workerProcess(t, "w1", t.TempDir(), "JELLYSCALE_TEST_CHAPTERS=1", held.Env())
	if err := library.EnqueueChapterImages(t.Context(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}

	var running []int
	waitFor(t, "an ffmpeg for each of the chapter jobs the worker has slots for", 60*time.Second, func() bool {
		running = held.ChildrenOf(worker.Process.Pid)
		return len(running) >= chapterSlots
	})
	if len(running) != chapterSlots {
		t.Fatalf("%d ffmpeg running under the worker, want chapterSlots (%d)", len(running), chapterSlots)
	}
	if err := worker.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	testenv.GoneWithin(t, time.Second, running...)
}
