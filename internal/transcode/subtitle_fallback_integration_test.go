//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-090: a text track ffmpeg hangs on no longer costs the film's other tracks.
// A pass ended by a limit is followed, in the same job, by one run per track not
// yet stored; a track whose own run is ended by a limit while another run of the
// fallback ran to its end is marked unreadable.
//
// These checks name SubtitleStall, the Worker's stall limit (no output growth, no
// read of the film, for this long; zero is 60 seconds), and the environment
// variable JELLYSCALE_TEST_SUBTITLE_STALL that a worker process of
// failover_integration_test.go reads into it. They do not compile until the
// change names them. A short stall limit in a test could hide a build that ends
// a run too early on a slow machine; TestAHangingTrackIsFoundAtTheDefaultStallLimit
// runs at the default, and TestARunThatIsStillReadingIsNeverEndedByTheStallLimit
// runs a read several stall limits long.

func shortStall(d time.Duration) func(*Worker) {
	return func(w *Worker) { w.SubtitleStall = d }
}

// fallbackShim stands in front of ffmpeg and notes each extraction run: which
// streams it maps (one line per run, in the order they started), and any run that
// started while an earlier one was alive. cond is shell, run for every extraction
// with has N true when the run maps stream N; a run may hang there
// (exec sleep 600), as a track ffmpeg hangs on does, or wait first.
type fallbackShim struct{ dir string }

func newFallbackShim(t *testing.T, cond string) *fallbackShim {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in *lavfi*) exec %[2]q "$@";; esac
ARGS="$*"
has() { case " $ARGS " in *" 0:$1 "*) return 0;; esac; return 1; }
for p in $(cat %[1]s/pids 2>/dev/null); do kill -0 "$p" 2>/dev/null && echo "$p|$$" >> %[1]s/overlap; done
echo $$ >> %[1]s/pids
maps=$(for k in 0 1 2 3 4 5 6 7 8 9; do has $k && printf '%%s ' $k; done)
echo "$maps" >> %[1]s/runs
%[3]s
exec %[2]q "$@"
`, dir, real, cond)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fallbackShim{dir: dir}
}

// runs is the streams each run mapped, in the order the runs started.
func (s *fallbackShim) runs() [][]int {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "runs"))
	var runs [][]int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var run []int
		for _, f := range strings.Fields(line) {
			n, _ := strconv.Atoi(f)
			run = append(run, n)
		}
		if len(run) > 0 {
			runs = append(runs, run)
		}
	}
	return runs
}

func (s *fallbackShim) overlaps() string {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "overlap"))
	return strings.TrimSpace(string(raw))
}

// alone is how many runs mapped only stream n.
func (s *fallbackShim) alone(n int) (count int) {
	for _, run := range s.runs() {
		if len(run) == 1 && run[0] == n {
			count++
		}
	}
	return count
}

// lastAlive says whether the newest run's process still exists.
func (s *fallbackShim) lastAlive() bool {
	raw, _ := os.ReadFile(filepath.Join(s.dir, "pids"))
	pids := strings.Fields(string(raw))
	if len(pids) == 0 {
		return false
	}
	_, err := os.Stat("/proc/" + pids[len(pids)-1])
	return err == nil
}

// check is what every fallback keeps whatever the film: at most 1 + n runs (2 + n
// when the pass first named a stream, extra), none alone twice, and one ffmpeg at a time.
func (s *fallbackShim) check(t *testing.T, n, extra int) {
	t.Helper()
	runs := s.runs()
	if len(runs) > 1+n+extra {
		t.Errorf("%d ffmpeg runs for a film of %d text tracks, want at most %d: %v", len(runs), n, 1+n+extra, runs)
	}
	for _, run := range runs {
		if len(run) == 1 && s.alone(run[0]) > 1 {
			t.Errorf("track %d was run alone %d times: %v", run[0], s.alone(run[0]), runs)
		}
	}
	if o := s.overlaps(); o != "" {
		t.Errorf("an ffmpeg started while another was alive (a worker may run at most four extractions, each one ffmpeg at a time): %s", o)
	}
}

// pickFor is pick with a wait of its own, for a job that takes minutes.
func (r *rig) pickFor(t *testing.T, j *Jobs, assets store.Store, source items.Source, index int, wait time.Duration) (subtitles.Track, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return FetchSubtitle(ctx, r.pool, j.Hub, assets, r.spec.Item, source, index)
}

// wantStored reads what the pass or the fallback put in the asset store, in
// every format, and checks it is the text of stream index.
func (r *rig) wantStored(t *testing.T, assets store.Store, index int) {
	t.Helper()
	track, found, err := subtitles.Read(context.Background(), assets, subtitles.Key(r.source(t), index))
	if err != nil || !found {
		t.Errorf("track %d is not in the asset store: %v %v", index, found, err)
		return
	}
	wantTrack(t, track, index)
	if track.SRT == "" || track.ASS == "" {
		t.Errorf("track %d lacks a format: SRT %q, ASS %q", index, track.SRT, track.ASS)
	}
}

func (r *rig) wantMarked(t *testing.T, marked, unmarked []int) {
	t.Helper()
	src := r.source(t)
	for _, i := range marked {
		if !subtitles.Unreadable(src, i) {
			t.Errorf("track %d is not marked unreadable", i)
		}
	}
	for _, i := range unmarked {
		if subtitles.Unreadable(src, i) {
			t.Errorf("track %d is marked unreadable", i)
		}
	}
}

func (r *rig) trackJob(t *testing.T) (state string, attempts int, permanent bool, reason string) {
	t.Helper()
	if err := r.pool.QueryRow(context.Background(), `SELECT state, attempts, permanent, COALESCE(error, '') FROM subtitle_jobs WHERE kind = 'track'`).Scan(&state, &attempts, &permanent, &reason); err != nil {
		t.Fatalf("the film's job: %v", err)
	}
	return
}

// A film with one text track ffmpeg hangs on, whichever it is and whichever
// track is asked for first, has every other text track stored with its own text
// in every format, the request that queued the job is answered, and the bad one
// is marked and costs nothing again, also after the failure memory.
func TestAHangingTextTrackCostsTheFilmsOtherTracksNothing(t *testing.T) {
	testenv.FFmpeg(t)
	for _, hang := range []int{2, 3, 4} {
		for _, first := range []int{2, 3, 4} {
			t.Run(fmt.Sprintf("track %d hangs and track %d is asked first", hang, first), func(t *testing.T) {
				r := newRig(t, nil)
				_, assets := r.textTracks(t)
				shim := newFallbackShim(t, fmt.Sprintf("if has %d; then exec sleep 600; fi", hang))
				stop := r.worker(t, shortStall(2*time.Second))
				defer stop()
				j := r.jobs(t)
				track, err := r.pick(t, j, assets, r.source(t), first)
				if first == hang {
					if !errors.Is(err, ErrUnreadable) {
						t.Fatalf("the hanging track asked first: %v, want ErrUnreadable", err)
					}
				} else if err != nil {
					t.Fatalf("track %d asked first, beside a hanging track: %v", first, err)
				} else {
					wantTrack(t, track, first)
				}
				var good, bad []int
				for _, i := range []int{2, 3, 4} {
					if i == hang {
						bad = append(bad, i)
					} else {
						good = append(good, i)
						r.wantStored(t, assets, i)
					}
				}
				r.wantMarked(t, bad, good)
				runs := shim.runs()
				if len(runs) == 0 || len(runs[0]) != 3 {
					t.Errorf("the first run was not the pass over all three tracks: %v", runs)
				}
				shim.check(t, 3, 0)
				if state, attempts, _, _ := r.trackJob(t); state != "done" || attempts != 1 || r.subtitleJobRows(t) != 1 {
					t.Errorf("the job ended %s after %d attempts, %d rows: want one job, done, claimed once", state, attempts, r.subtitleJobRows(t))
				}
				// After the failure memory: the hanging track answers unreadable at once with no job and no
				// ffmpeg, and the good ones come from the store.
				r.forgetFailures(t)
				var endedBefore time.Time
				r.pool.QueryRow(context.Background(), "SELECT ended_at FROM subtitle_jobs WHERE kind = 'track'").Scan(&endedBefore) //nolint:errcheck
				before := len(shim.runs())
				if _, err := r.pick(t, j, assets, r.source(t), hang); !errors.Is(err, ErrUnreadable) {
					t.Errorf("the hanging track after the failure memory: %v, want ErrUnreadable", err)
				} else if !strings.Contains(err.Error(), "could not be read") {
					t.Errorf("the error says %q, want it to say the track could not be read", err)
				}
				for _, i := range good {
					if _, err := r.pick(t, j, assets, r.source(t), i); err != nil {
						t.Errorf("good track %d after the failure memory: %v", i, err)
					}
				}
				var endedAfter time.Time
				r.pool.QueryRow(context.Background(), "SELECT ended_at FROM subtitle_jobs WHERE kind = 'track'").Scan(&endedAfter) //nolint:errcheck
				if n := len(shim.runs()) - before; n != 0 || !endedAfter.Equal(endedBefore) || r.subtitleJobRows(t) != 1 {
					t.Errorf("asking again after the failure memory ran %d ffmpegs and queued a job (rows %d)", n, r.subtitleJobRows(t))
				}
			})
		}
	}
}

// Several bad tracks in one film: two that hang, and one that hangs beside tracks
// ffmpeg refuses to decode. Every remaining track is stored and each bad one is marked.
func TestSeveralBadTracksInOneFilmCostTheGoodOnesNothing(t *testing.T) {
	testenv.FFmpeg(t)
	t.Run("two that hang", func(t *testing.T) {
		r := newRig(t, nil)
		_, assets := r.textTracks(t)
		shim := newFallbackShim(t, "if has 2 || has 4; then exec sleep 600; fi")
		stop := r.worker(t, shortStall(2*time.Second))
		defer stop()
		j := r.jobs(t)
		if _, err := r.pick(t, j, assets, r.source(t), 3); err != nil {
			t.Fatalf("the good track beside two hanging ones: %v", err)
		}
		r.wantStored(t, assets, 3)
		r.wantMarked(t, []int{2, 4}, []int{3})
		shim.check(t, 3, 0)
		for _, i := range []int{2, 4} {
			if _, err := r.pick(t, j, assets, r.source(t), i); !errors.Is(err, ErrUnreadable) {
				t.Errorf("hanging track %d: %v, want ErrUnreadable", i, err)
			}
		}
	})
	t.Run("one that hangs and two ffmpeg refuses to decode", func(t *testing.T) {
		r := newRig(t, nil)
		assets := r.twoBrokenTracksFilm(t) // 2 English, 3 and 4 not UTF-8, 5 ASS.
		shim := newFallbackShim(t, "if has 2; then exec sleep 600; fi")
		stop := r.worker(t, shortStall(2*time.Second))
		defer stop()
		j := r.jobs(t)
		track, err := r.pick(t, j, assets, r.source(t), 5)
		if err != nil || !strings.Contains(track.ASS, assStyle) {
			t.Fatalf("the ASS track beside a hanging and two broken ones: %v %q", err, track.ASS)
		}
		r.wantMarked(t, []int{2, 3, 4}, []int{5})
		for _, i := range []int{2, 3, 4} {
			if _, err := r.pick(t, j, assets, r.source(t), i); !errors.Is(err, ErrUnreadable) {
				t.Errorf("bad track %d: %v, want ErrUnreadable", i, err)
			}
		}
		shim.check(t, 4, 0)
	})
}

// When the pass first names streams (the two ffmpeg refuses) and the second pass
// hangs, the cost is 2 + n runs at most, and the named tracks are not run alone.
func TestAPassThatNamedStreamsAndThenHungFallsBackWithoutTheNamedTracks(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	assets := r.twoBrokenTracksFilm(t)
	// Hangs only the pass without 3 (the second), and track 5 alone.
	shim := newFallbackShim(t, "if has 5 && ! has 3; then exec sleep 600; fi")
	stop := r.worker(t, shortStall(2*time.Second))
	defer stop()
	j := r.jobs(t)
	if _, err := r.pick(t, j, assets, r.source(t), 2); err != nil {
		t.Fatalf("the English track: %v", err)
	}
	r.wantStored(t, assets, 2)
	r.wantMarked(t, []int{3, 4, 5}, []int{2})
	shim.check(t, 4, 1)
	if n := shim.alone(3) + shim.alone(4); n != 0 {
		t.Errorf("a track ffmpeg named was run alone %d times: %v", n, shim.runs())
	}
}

// With every run ended by a limit (nothing ran to its end), no track is marked:
// the job fails as one that may pass, is remembered, and the next ask after the
// memory runs the film again.
func TestWhenNoRunOfTheFallbackEndsNoTrackIsMarked(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	shim := newFallbackShim(t, "exec sleep 600")
	stop := r.worker(t, shortStall(2*time.Second))
	defer stop()
	j := r.jobs(t)
	_, err := r.pick(t, j, assets, r.source(t), 2)
	if !errors.Is(err, ErrJobFailed) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("a film every run of which hangs: %v, want ErrJobFailed and not ErrUnreadable", err)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
	if _, _, permanent, _ := r.trackJob(t); permanent {
		t.Error("the job is recorded as one that fails for good")
	}
	shim.check(t, 3, 0)
	settled := len(shim.runs())
	if _, err := r.pick(t, j, assets, r.source(t), 3); !errors.Is(err, ErrJobFailed) {
		t.Errorf("asking again within the failure memory: %v, want ErrJobFailed", err)
	}
	if n := len(shim.runs()) - settled; n != 0 {
		t.Errorf("the failure was not remembered: %d more ffmpeg runs", n)
	}
	r.forgetFailures(t)
	r.pick(t, j, assets, r.source(t), 3) //nolint:errcheck
	if runs := shim.runs(); len(runs) == settled || len(runs[settled]) != 3 {
		t.Errorf("the ask after the failure memory did not run the film again with a pass: %v", runs)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// A hang found at the default stall limit (about 60 s) with the extraction limit
// at its hour: the job ends after about two stall limits. A build whose stall
// limit is short enough to hide in the other checks, or that waits for the hour,
// fails: the bounds are 100 s from below and 15 minutes from above.
func TestAHangingTrackIsFoundAtTheDefaultStallLimit(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	newFallbackShim(t, "if has 3; then exec sleep 600; fi")
	stop := r.worker(t, nil)
	defer stop()
	began := time.Now()
	track, err := r.pickFor(t, r.jobs(t), assets, r.source(t), 2, 15*time.Minute)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("a good track beside a hanging one, at the default stall limit: %v after %s", err, took)
	}
	wantTrack(t, track, 2)
	t.Logf("the job took %s", took)
	if took < 100*time.Second {
		t.Errorf("the job took %s: two stall limits of about 60 s were not waited for", took)
	}
	r.wantStored(t, assets, 4)
	r.wantMarked(t, []int{3}, []int{2, 4})
}

// slowFilm hands the film to ffmpeg through the loopback server (no local path)
// a few kilobytes at a time, so a read takes several stall limits and ffmpeg
// writes nothing the while.
type slowFilm struct {
	store.Store
	every time.Duration
}

func (s *slowFilm) LocalPath(string) (string, bool) { return "", false }

func (s *slowFilm) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := s.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	return &slowBody{ReadCloser: body, every: s.every}, nil
}

type slowBody struct {
	io.ReadCloser
	every time.Duration
}

func (b *slowBody) Read(p []byte) (int, error) {
	if len(p) > 4096 {
		p = p[:4096]
	}
	time.Sleep(b.every)
	return b.ReadCloser.Read(p)
}

// A run that is still reading the film is never ended by the stall limit, however
// long it writes nothing: a read of several stall limits is one run, all tracks stored.
func TestARunThatIsStillReadingIsNeverEndedByTheStallLimit(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	shim := newFallbackShim(t, "")
	const stall = 3 * time.Second
	// About 12 s of reading, each read well inside the stall limit.
	reads := time.Duration(source.Size/4096 + 1)
	every := 12 * time.Second / reads
	if every >= stall/3 {
		t.Fatalf("the film is too big (%d bytes) for a slow read inside the stall limit", source.Size)
	}
	stop := r.worker(t, func(w *Worker) {
		w.SubtitleStall = stall
		var cache store.Cache
		w.Media = func(uri string) (store.Store, error) {
			s, err := cache.Open(uri)
			if err != nil {
				return nil, err
			}
			return &slowFilm{Store: s, every: every}, nil
		}
	})
	defer stop()
	began := time.Now()
	if _, err := r.pickFor(t, r.jobs(t), assets, source, 3, 2*time.Minute); err != nil {
		t.Fatalf("a film that takes several stall limits to read: %v", err)
	}
	if took := time.Since(began); took < 3*stall {
		t.Fatalf("the read took %s, less than three stall limits: the fixture does not test the stall limit", took)
	}
	if n := len(shim.runs()); n != 1 {
		t.Errorf("%d ffmpeg runs, want one: the read was ended by the stall limit", n)
	}
	for _, i := range []int{2, 3, 4} {
		r.wantStored(t, assets, i)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// A pass ended by the extraction limit, not the stall limit, falls back the same
// way, and every run of the fallback has the whole limit: here two good tracks
// each take 4 s before ffmpeg starts under a limit of 6 s, the pass and the
// hanging track use 6 s each, so a limit shared by the job or by the fallback fails.
func TestAPassEndedByTheExtractionLimitFallsBackAndEveryRunHasTheWholeLimit(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	shim := newFallbackShim(t, "if has 3; then exec sleep 600; fi; if has 2 && ! has 4; then sleep 4; fi; if has 4 && ! has 2; then sleep 4; fi")
	stop := r.worker(t, func(w *Worker) {
		w.SubtitleTimeout = 6 * time.Second
		w.SubtitleStall = time.Hour // Only the extraction limit can end a run here.
	})
	defer stop()
	j := r.jobs(t)
	if _, err := r.pick(t, j, assets, r.source(t), 2); err != nil {
		t.Fatalf("the good track after a pass ended by the extraction limit: %v", err)
	}
	r.wantStored(t, assets, 2)
	r.wantStored(t, assets, 4)
	r.wantMarked(t, []int{3}, []int{2, 4})
	shim.check(t, 3, 0)
}

// Each run of the fallback has a stall limit of its own, never what an earlier run
// left: after a hung pass, a good track that does nothing for 2 s of a 3 s limit
// before ffmpeg starts still runs to its end.
func TestEveryRunOfTheFallbackHasAStallLimitOfItsOwn(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	shim := newFallbackShim(t, "if has 3; then exec sleep 600; fi; if has 2 && ! has 4; then sleep 2; fi; if has 4 && ! has 2; then sleep 2; fi")
	stop := r.worker(t, shortStall(3*time.Second))
	defer stop()
	if _, err := r.pick(t, r.jobs(t), assets, r.source(t), 2); err != nil {
		t.Fatalf("a good track whose run waited 2 s of a 3 s stall limit: %v", err)
	}
	r.wantStored(t, assets, 2)
	r.wantStored(t, assets, 4)
	r.wantMarked(t, []int{3}, []int{2, 4})
	shim.check(t, 3, 0)
}

// refusingFilm is a store that refuses to open the film, as one that is down does.
type refusingFilm struct{ store.Store }

func (refusingFilm) LocalPath(string) (string, bool) { return "", false }
func (refusingFilm) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return nil, errors.New("injected: the store refuses the read")
}

// A run ended by anything but a limit starts no fallback and marks nothing: here a
// store that refuses the read. The job fails as one that may pass, after one run.
func TestARunEndedByAStoreThatRefusesStartsNoFallbackAndMarksNothing(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	shim := newFallbackShim(t, "")
	stop := r.worker(t, func(w *Worker) {
		w.SubtitleStall = 2 * time.Second
		var cache store.Cache
		w.Media = func(uri string) (store.Store, error) {
			s, err := cache.Open(uri)
			return refusingFilm{s}, err
		}
	})
	defer stop()
	_, err := r.pick(t, r.jobs(t), assets, r.source(t), 2)
	if !errors.Is(err, ErrJobFailed) {
		t.Fatalf("a store that refuses the film: %v, want ErrJobFailed", err)
	}
	if n := len(shim.runs()); n > 1 {
		t.Errorf("%d ffmpeg runs after a refused read, want at most the pass: %v", n, shim.runs())
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// hungInFallback starts a worker on a film whose track 3 hangs, asks for track 2
// and waits until the fallback's run for track 3 alone is hanging. It returns the
// number of runs then.
func hungInFallback(t *testing.T, r *rig, change func(*Worker)) (shim *fallbackShim, stop func(), runs int) {
	t.Helper()
	_, assets := r.textTracks(t)
	shim = newFallbackShim(t, "if has 3; then exec sleep 600; fi")
	stop = r.worker(t, func(w *Worker) {
		w.SubtitleStall = 2 * time.Second
		if change != nil {
			change(w)
		}
	})
	j := r.jobs(t)
	go r.pick(t, j, assets, r.source(t), 2) //nolint:errcheck
	waitFor(t, "the fallback's run for track 3 alone", 60*time.Second, func() bool { return shim.alone(3) == 1 })
	return shim, stop, len(shim.runs())
}

// The worker shutting down during a fallback run ends the job as it does today
// (put back in the queue, attempts given back), starts no further run, marks nothing.
func TestTheWorkerShuttingDownDuringAFallbackStartsNoFurtherRunAndMarksNothing(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	shim, stop, runs := hungInFallback(t, r, nil)
	stop()
	if n := len(shim.runs()); n != runs {
		t.Errorf("%d runs started after the shutdown: %v", n-runs, shim.runs())
	}
	if state, attempts, _, _ := r.trackJob(t); state != "queued" || attempts != 0 {
		t.Errorf("the job is %s with %d attempts after the shutdown, want queued with none", state, attempts)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// A worker whose lease is taken during a fallback run starts no further run and
// marks nothing; the job is left to its new owner.
func TestAWorkerThatLosesItsLeaseDuringAFallbackStartsNoFurtherRun(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	shim, stop, runs := hungInFallback(t, r, nil)
	defer stop()
	r.exec(t, "UPDATE subtitle_jobs SET worker = 'another' WHERE kind = 'track'")
	waitFor(t, "the run of the worker that lost its lease to end", 60*time.Second, func() bool { return !shim.lastAlive() })
	// Absence has to be waited for: a build that went on would start its next run within moments.
	time.Sleep(3 * time.Second)
	if n := len(shim.runs()); n != runs {
		t.Errorf("%d runs started after the lease was lost: %v", n-runs, shim.runs())
	}
	var worker, state string
	r.pool.QueryRow(context.Background(), "SELECT worker, state FROM subtitle_jobs WHERE kind = 'track'").Scan(&worker, &state) //nolint:errcheck
	if worker != "another" || state != "running" {
		t.Errorf("the row is %s on %q: the worker that lost its lease wrote to it", state, worker)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// A fallback over a version of the film replaced meanwhile marks nothing on the
// new version; the new version (a new ETag) is read afresh by a pass of its own,
// and the mark of the old one is gone.
func TestAFallbackOverAReplacedVersionMarksNothingOnTheNewOne(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	_, assets := r.textTracks(t)
	flag := filepath.Join(t.TempDir(), "hang")
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	shim := newFallbackShim(t, fmt.Sprintf("if [ -e %q ] && has 3; then exec sleep 600; fi", flag))
	stop := r.worker(t, shortStall(2*time.Second))
	defer stop()
	j := r.jobs(t)
	done := make(chan error, 1)
	go func() { _, err := r.pick(t, j, assets, r.source(t), 2); done <- err }()
	waitFor(t, "the fallback's run for track 3 alone", 60*time.Second, func() bool { return shim.alone(3) == 1 })
	r.exec(t, "UPDATE media_sources SET object_etag = 'v2'")
	if err := <-done; err != nil {
		t.Fatalf("the pick during the fallback over v1: %v", err)
	}
	r.wantMarked(t, nil, []int{3})
	// The new version is read afresh by a pass of its own, and its track 3 no longer hangs.
	os.Remove(flag) //nolint:errcheck
	before := len(shim.runs())
	if _, err := r.pick(t, j, assets, r.source(t), 3); err != nil {
		t.Fatalf("track 3 of the new version: %v", err)
	}
	runs := shim.runs()
	if len(runs) != before+1 || len(runs[before]) != 3 {
		t.Errorf("the new version was not read by one pass of its own: %v", runs[before:])
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// With two workers and two api replicas asking for a good track and the hanging
// one at the same moment, at the default stall limit (60 s, so the pass and the
// fallback each outlast the 30 s lease): one job claimed once, one pass, each
// track run alone at most once, the good track answered, the hanging one unreadable.
func TestOneJobKeepsItsLeaseThroughAFallbackLongerThanALease(t *testing.T) {
	shim := newFallbackShim(t, "if has 3; then exec sleep 600; fi")
	s := newStack(t, 2, withEnv(thirtySecondLease, "JELLYSCALE_TEST_SUBTITLES=1", productionPool)...)
	r := s.r
	_, assets := r.textTracks(t)
	replicas := []*Jobs{s.jobs, s.replicaJobs()}
	type result struct {
		track subtitles.Track
		err   error
	}
	answers := make(chan result, 2)
	for i, index := range []int{2, 3} {
		go func() {
			track, err := r.pickFor(t, replicas[i], assets, r.source(t), index, 10*time.Minute)
			answers <- result{track, err}
		}()
	}
	var good, hung int
	for range 2 {
		got := <-answers
		switch {
		case got.err == nil:
			good++
			wantTrack(t, got.track, 2)
		case errors.Is(got.err, ErrUnreadable):
			hung++
		default:
			t.Errorf("a request answered with %v", got.err)
		}
	}
	if good != 1 || hung != 1 {
		t.Errorf("%d good answers and %d unreadable, want one each", good, hung)
	}
	if state, attempts, _, _ := r.trackJob(t); state != "done" || attempts != 1 || r.subtitleJobRows(t) != 1 {
		t.Errorf("the job ended %s after %d attempts (%d rows): its lease was lost during the pass or the fallback", state, attempts, r.subtitleJobRows(t))
	}
	passes := 0
	for _, run := range shim.runs() {
		if len(run) == 3 {
			passes++
		}
	}
	if passes != 1 {
		t.Errorf("%d passes, want one: %v", passes, shim.runs())
	}
	shim.check(t, 3, 0)
	r.wantStored(t, assets, 2)
	r.wantStored(t, assets, 4)
	r.wantMarked(t, []int{3}, []int{2, 4})
}

// A worker killed during the stalled pass, or during a fallback run, has the job
// taken by another before its lease would have run out, and the other leaves the
// good tracks stored and the hanging one marked. The stall limit is 5 s so the
// kill can land in the fallback; the lease is production's 30 s.
func TestAWorkerKilledDuringAStalledPassOrAFallbackRunLeavesTheJobToAnother(t *testing.T) {
	for _, phase := range []string{"the pass", "a fallback run"} {
		t.Run(phase, func(t *testing.T) {
			shim := newFallbackShim(t, "if has 3; then exec sleep 600; fi")
			s := newStack(t, 3, withEnv(thirtySecondLease, "JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_SUBTITLE_STALL=5s", productionPool)...)
			r := s.r
			_, assets := r.textTracks(t)
			answered := make(chan error, 1)
			var got subtitles.Track
			go func() {
				var err error
				got, err = r.pickFor(t, s.jobs, assets, r.source(t), 2, 10*time.Minute)
				answered <- err
			}()
			waitFor(t, "the "+phase+" to hang", 60*time.Second, func() bool {
				if phase == "the pass" {
					return len(shim.runs()) == 1
				}
				return shim.alone(3) == 1
			})
			var owner string
			var lease time.Time
			if err := r.pool.QueryRow(context.Background(), "SELECT worker, lease_expires_at FROM subtitle_jobs WHERE state = 'running' AND kind = 'track'").Scan(&owner, &lease); err != nil {
				t.Fatalf("no subtitle job is running: %v", err)
			}
			victim := s.worker(owner)
			if victim == nil {
				t.Fatalf("the job is held by %q, not one of the workers", owner)
			}
			killAll(victim)
			select {
			case err := <-answered:
				if err != nil {
					t.Fatalf("the request waiting on the killed worker's job: %v", err)
				}
				wantTrack(t, got, 2)
			case <-time.After(5 * time.Minute):
				t.Fatal("the job was not taken over")
			}
			var early bool
			r.pool.QueryRow(context.Background(), "SELECT now() < $1", lease).Scan(&early) //nolint:errcheck
			if !early {
				t.Errorf("the request was answered only after the lease the row had at the kill (%v) ran out", lease)
			}
			if state, attempts, _, _ := r.trackJob(t); state != "done" || attempts != 2 {
				t.Errorf("the job ended %s after %d attempts, want done after 2", state, attempts)
			}
			r.wantStored(t, assets, 2)
			r.wantStored(t, assets, 4)
			r.wantMarked(t, []int{3}, []int{2, 4})
		})
	}
}

// pipedFilm is a store whose film has a local path, as a POSIX library does, that
// is a named pipe fed a few kilobytes at a time. ffmpeg then reads the film with
// no loopback server in between, so the worker sees none of the reads, and writes
// no subtitle output until it has read it all.
type pipedFilm struct {
	store.Store
	t     *testing.T
	every time.Duration
	feeds int
}

func (p *pipedFilm) LocalPath(key string) (string, bool) {
	p.t.Helper()
	body, err := p.Store.Open(context.Background(), key, 0)
	if err != nil {
		p.t.Error(err)
		return "", false
	}
	path := filepath.Join(p.t.TempDir(), "film.mkv")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		p.t.Fatal(err)
	}
	// O_RDWR does not wait for ffmpeg to open the pipe; the feeder closes it at the
	// end of the film, which is the reader's end of file.
	pipe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	p.feeds++
	p.t.Cleanup(func() { pipe.Close(); body.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				time.Sleep(p.every)
				if _, werr := pipe.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				// Let ffmpeg drain the pipe before the end of file.
				time.Sleep(time.Second)
				pipe.Close()
				return
			}
		}
	}()
	return path, true
}

// The same as TestARunThatIsStillReadingIsNeverEndedByTheStallLimit, for a film on
// a local path (a POSIX library), where the worker sees no read: ffmpeg reads the
// film for several stall limits, writes nothing, and is still reading. A build that
// counts only the output and the reads it sees ends it as stalled, and with it every
// single-track run, so no track of the film is ever stored.
func TestAFilmOnALocalPathThatIsStillBeingReadIsNeverEndedByTheStallLimit(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	shim := newFallbackShim(t, "")
	const stall = 3 * time.Second
	reads := time.Duration(source.Size/4096 + 1)
	every := 12 * time.Second / reads
	if every >= stall/3 {
		t.Fatalf("the film is too big (%d bytes) for a slow read inside the stall limit", source.Size)
	}
	var film *pipedFilm
	stop := r.worker(t, func(w *Worker) {
		w.SubtitleStall = stall
		var cache store.Cache
		w.Media = func(uri string) (store.Store, error) {
			s, err := cache.Open(uri)
			if err != nil || uri != source.StoreURI {
				return s, err
			}
			film = &pipedFilm{Store: s, t: t, every: every}
			return film, nil
		}
	})
	defer stop()
	began := time.Now()
	if _, err := r.pickFor(t, r.jobs(t), assets, source, 3, 2*time.Minute); err != nil {
		t.Fatalf("a film on a local path that takes several stall limits to read: %v", err)
	}
	if took := time.Since(began); took < 3*stall {
		t.Fatalf("the read took %s, less than three stall limits: the fixture does not test the stall limit", took)
	}
	if n := len(shim.runs()); n != 1 {
		t.Errorf("%d ffmpeg runs, want one: the read was ended by the stall limit", n)
	}
	for _, i := range []int{2, 3, 4} {
		r.wantStored(t, assets, i)
	}
	r.wantMarked(t, nil, []int{2, 3, 4})
}

// failingPuts is an asset store whose write of a subtitle track hangs until its
// context ends, as one that has stalled does (the error is then the deadline's,
// the same as a run ended by the limit), or fails at once.
type failingPuts struct {
	store.Store
	hang bool
}

func (f failingPuts) Put(ctx context.Context, key string, body io.Reader) error {
	if !strings.HasPrefix(key, "subtitles/") {
		return f.Store.Put(ctx, key, body)
	}
	if f.hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return errors.New("injected: the store refuses the write")
}

// A store write that fails or never returns is not a limit on a run: the pass's
// ffmpeg ran to its end, so no fallback starts, no track is marked (a good track
// hidden on every replica because the asset store stalled once), and the job fails
// as one that may pass after one run. The hung write ends with the extraction
// limit's own error, so a build that takes any deadline error for a limit fails here.
func TestAStoreWriteThatFailsOrNeverReturnsStartsNoFallbackAndMarksNothing(t *testing.T) {
	testenv.FFmpeg(t)
	for _, hang := range []bool{true, false} {
		name := "a write that fails"
		if hang {
			name = "a write that never returns"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			_, assets := r.textTracks(t)
			shim := newFallbackShim(t, "")
			stop := r.worker(t, func(w *Worker) {
				// The film is read in well under the stall limit; the write waits for the limit.
				w.SubtitleTimeout = 5 * time.Second
				w.SubtitleStall = 3 * time.Second
				var cache store.Cache
				w.Media = func(uri string) (store.Store, error) {
					s, err := cache.Open(uri)
					return failingPuts{Store: s, hang: hang}, err
				}
			})
			defer stop()
			_, err := r.pick(t, r.jobs(t), assets, r.source(t), 2)
			if !errors.Is(err, ErrJobFailed) || errors.Is(err, ErrUnreadable) {
				t.Fatalf("%s: %v, want ErrJobFailed and not ErrUnreadable", name, err)
			}
			if runs := shim.runs(); len(runs) != 1 || len(runs[0]) != 3 {
				t.Errorf("ffmpeg runs after %s: %v, want the one pass over the three tracks", name, runs)
			}
			r.wantMarked(t, nil, []int{2, 3, 4})
			if _, _, permanent, _ := r.trackJob(t); permanent {
				t.Error("the job is recorded as one that fails for good")
			}
			shim.check(t, 3, 0)
		})
	}
}

// refusedFirstFilm is a film whose stream 2 is an SRT ffmpeg refuses to decode
// (not UTF-8) and whose streams 3 up to 2+others are English.
func (r *rig) refusedFirstFilm(t *testing.T, others int) store.Store {
	t.Helper()
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.srt")
	en := filepath.Join(dir, "en.srt")
	if err := os.WriteFile(bad, []byte("1\n00:00:02,000 --> 00:00:04,000\n\xff\xfe\xfd caf\xe9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(en, []byte("1\n00:00:02,000 --> 00:00:04,000\n"+englishLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=30",
		"-f", "srt", "-i", bad}
	for i := 0; i < others; i++ {
		args = append(args, "-f", "srt", "-i", en)
	}
	args = append(args, "-map", "0:v", "-map", "1:a", "-map", "2")
	for i := 0; i < others; i++ {
		args = append(args, "-map", strconv.Itoa(3+i))
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "48", "-c:a", "aac", "-c:s", "copy",
		filepath.Join(film(t), "refusedfirst.mkv"))
	run(t, "ffmpeg", args...)
	r.useFile(t, "refusedfirst.mkv")
	info, err := os.Stat(filepath.Join(film(t), "refusedfirst.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	assets, err := store.Open(r.source(t).AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	return assets
}

// A track ffmpeg's decoder refused is the file's doing, and is marked whatever
// the rest of the pass came to. The tracks whose runs ended by a limit, with
// no run of the fallback coming to its end, are not marked (a stalled store
// looks the same), and the job fails as one that may pass.
func TestATrackTheDecoderRefusedIsMarkedWhenTheRestOfThePassEndsByALimit(t *testing.T) {
	testenv.FFmpeg(t)
	for _, c := range []struct {
		name   string
		others int
		runs   [][]int
	}{
		{"one other track, whose single run hangs", 1, [][]int{{2, 3}, {3}}},
		{"two other tracks, whose runs all hang", 2, [][]int{{2, 3, 4}, {3, 4}, {3}, {4}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			assets := r.refusedFirstFilm(t, c.others)
			shim := newFallbackShim(t, "if (has 3 || has 4) && ! has 2; then exec sleep 600; fi")
			stop := r.worker(t, shortStall(2*time.Second))
			defer stop()
			j := r.jobs(t)
			_, err := r.pick(t, j, assets, r.source(t), 3)
			if !errors.Is(err, ErrJobFailed) || errors.Is(err, ErrUnreadable) {
				t.Fatalf("pick: %v, want ErrJobFailed and not ErrUnreadable", err)
			}
			if got := shim.runs(); fmt.Sprint(got) != fmt.Sprint(c.runs) {
				t.Errorf("runs %v, want %v", got, c.runs)
			}
			unmarked := []int{3}
			if c.others == 2 {
				unmarked = append(unmarked, 4)
			}
			r.wantMarked(t, []int{2}, unmarked)
			if _, _, permanent, _ := r.trackJob(t); permanent {
				t.Error("the job is recorded as one that fails for good")
			}
		})
	}
}
