//go:build integration

package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-089: the extraction of the image tracks. A play asks for it (askForTracks)
// without waiting; one worker job reads the film once and stores each track.

// filmBytes is the size of the film file.
func filmBytes(t *testing.T, media string) int64 {
	t.Helper()
	films, _ := filepath.Glob(filepath.Join(media, "film.*"))
	var size int64
	for _, f := range films {
		if ext := filepath.Ext(f); ext == ".mkv" || ext == ".ts" {
			info, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			size = info.Size()
		}
	}
	if size == 0 {
		t.Fatalf("no film in %s", media)
	}
	return size
}

// packetTimes is the time of each packet of a stored track, which says what the
// track holds whatever file format the build stores it in.
func packetTimes(t *testing.T, s *scannedFilm, key string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.assets, key))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "track"+filepath.Ext(key))
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "packet=pts_time", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: the stored track is not a file ffmpeg reads: %v", key, err)
	}
	return strings.TrimSpace(string(out))
}

// The extraction of a film with one image track is one worker job that reads
// the film once, however many viewers ask at once, through two api replicas
// (two pools) and with two workers running, while a seek runs among them; the
// film's store serves the film at most once for it. Two image tracks (two PGS;
// PGS with VobSub) are stored by that one job and one read, and each draws its
// own cues, never another track's.
func TestOneJobAndOneReadStoreEveryImageTrackHoweverManyAsk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tracks [2]string // The codecs; "" for a film of one track.
	}{
		{"one PGS track", [2]string{"copy", ""}},
		{"two PGS tracks", [2]string{"copy", "copy"}},
		{"PGS and VobSub", [2]string{"copy", "dvdsub"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			cues := [2][]testenv.Cue{{top(6, 60)}, {bottom(6, 60)}}
			twoTrackFilm(t, media, "film", tc.tracks, cues, 100)
			size := filmBytes(t, media)
			s := newScanned(t, media, t.TempDir())
			want := 1
			if tc.tracks[1] != "" {
				want = 2
			}
			// What one extraction reads of this film, alone, and what a seek that draws
			// without the stored track reads (JS-071's run, which is the seek's own cost
			// while the track is not stored), so that a second read of the film shows.
			one := s.storeTrack(t, want, 0)
			s.forgetTrack(t)
			s.waitEnd = true
			s.seek(t, 20, 1, true, readRate)
			s.waitEnd = false
			alone := s.total
			s.forgetTrack(t)

			s.fresh(t)
			s.burnt()
			// At readRate, as the seek alone was measured: through a store that is not
			// slowed a seek alone is served 36 to 38 MB of this 14 MB film (what ffmpeg
			// leaves unread in each connection it drops), and the bound would not hold it.
			a, b := newMeters(s, readRate), newMeters(s, readRate)
			stopA, stopB := s.metered(t, a), s.metered(t, b)
			defer stopA()
			defer stopB()
			second, err := db.Open(context.Background(), s.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			var asking sync.WaitGroup
			for i := range 8 {
				asking.Add(1)
				go func() {
					defer asking.Done()
					if i%2 == 0 {
						s.askForTracks(t, s.pool)
					} else {
						s.askForTracks(t, second)
					}
				}()
			}
			// A seek meanwhile, which asks for nothing more of the film than it did.
			if _, err := s.firstSegment(t, s.jobs(t), func() int64 { return a.film.read.Load() + b.film.read.Load() }, 20, 0); err != nil {
				t.Fatalf("the seek made no segment: %v", err)
			}
			asking.Wait()
			s.waitStored(t, want)
			both := quiet(t, a.film) + quiet(t, b.film)
			if rows := s.jobRowsIn(t, ""); rows != 1 {
				t.Errorf("8 viewers asking at once made %d extraction jobs, want one: %v", rows, s.extractionRows(t))
			}
			// The seek's reads vary by about 2 MB with how far ffmpeg reads ahead; a second
			// read of the film adds all of it (a hundred seconds of film).
			if bound := one.film + alone + 2<<20; both > bound {
				t.Errorf("the film's store served %d bytes for the extraction and a seek; one extraction alone reads %d and the seek alone %d (bound %d, the film is %d): it was read more than once", both, one.film, alone, bound, size)
			}
			t.Logf("%s: the film is %d bytes; the stores served %d with eight asks and a seek (an extraction alone %d, the seek alone %d)", tc.name, size, both, one.film, alone)

			// Each track draws its own cues, in a run on a worker of its own (seekStored
			// forgets every worker, and these two would be forgotten alive).
			stopA()
			stopB()
			if tc.tracks[1] != "" {
				for track, want := range map[int]string{2: "top", 3: "bottom"} {
					s.track = track
					s.seekStored(t, 12, 2, true, 0) // 48 s, inside both cues.
					expectPicture(t, "track "+itoa(track)+" from the stored tracks", s.halvesFrom(t, 12, 14), 2, want)
				}
			}
		})
	}
}

// Once stored, a later play, a pick of the film's other image track, a seek, a
// failover and a helper's chunk start no extraction and read nothing of the
// film for the track, on any worker and after every worker has restarted: the
// jobs are the rows as they were, and the film's store served nothing to a
// worker that was asked to play none of it.
func TestOnceTheTracksAreStoredNothingAsksForThemAgain(t *testing.T) {
	media := t.TempDir()
	twoTrackFilm(t, media, "film", [2]string{"copy", "dvdsub"}, [2][]testenv.Cue{{top(6, 60)}, {bottom(6, 60)}}, 100)
	s := newScanned(t, media, t.TempDir())
	s.storeTrack(t, 2, 0)
	rows, objects := s.extractionRows(t), s.storedObjects()
	if len(rows) != 1 {
		t.Fatalf("storing two tracks took %d jobs: %v", len(rows), rows)
	}
	if _, err := s.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET ended_at = ended_at - interval '1 hour', created_at = created_at - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	rows = s.extractionRows(t)             // The timestamps are not in them; the state is.
	for _, track := range []int{2, 3, 2} { // A play, the pick of the other track, a play again.
		s.track = track
		s.fresh(t)
		s.burnt()
		m := newMeters(s, 0)
		stop := s.metered(t, m)
		s.askForTracks(t, s.pool)
		s.askForTracks(t, s.pool)
		time.Sleep(3 * time.Second) // A worker's poll is a second; whatever would run has begun.
		stop()
		if got := m.film.read.Load(); got != 0 || m.film.opens.Load() != 0 {
			t.Errorf("asking for the stored track %d again had a worker read %d bytes of the film in %d reads", track, got, m.film.opens.Load())
		}
		s.noExtraction(t, "asking again for track "+itoa(track), rows)
	}
	// A seek, a failover restart and a helper's chunk: each on a worker of its own.
	for _, track := range []int{2, 3} {
		s.track = track
		plain := s.seekStored(t, 20, 1, false, readRate).film
		got := s.seekStored(t, 20, 1, true, readRate)
		if got.film > plain+readMargin+plain/20 {
			t.Errorf("a seek with track %d stored read %d bytes of the film, %d without the track", track, got.film, plain)
		}
		s.noExtraction(t, "a seek with track "+itoa(track), rows)
	}
	if after := s.storedObjects(); len(after) != len(objects) {
		t.Errorf("the asset store held %v and holds %v", objects, after)
	}
}

// An extraction that ends for a reason that is not the film's stores nothing,
// never a track cut short, is not remembered as the film's (the track stays
// listed and nothing is marked unreadable for good), and is taken up again.
// A store error at each of the first opens of the film, a graceful stop of the
// worker, and a lease taken from it. Runs meanwhile draw as JS-071 does, which
// its own checks hold.
func TestAnExtractionThatEndsForAReasonNotTheFilmsStoresNothingAndIsTakenUpAgain(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 60, noise: "1M", cues: []testenv.Cue{top(6, 50)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	good := s.storeTrack(t, 1, 0)
	var goodPackets string
	for key := range s.storedObjects() {
		goodPackets = packetTimes(t, s, key)
	}
	if goodPackets == "" {
		t.Fatal("the stored track has no packets")
	}
	t.Logf("a whole extraction read %d bytes and stored %d", good.film, good.size)

	// settled says whether no extraction is queued or running.
	settled := func() bool { return s.jobRowsIn(t, "queued")+s.jobRowsIn(t, "running") == 0 }
	// intact fails the test for an object that is not the whole track.
	intact := func(what string) {
		t.Helper()
		for key := range s.storedObjects() {
			if got := packetTimes(t, s, key); got != goodPackets {
				t.Errorf("%s: the stored track %s has packets %q; the whole track has %q", what, key, got, goodPackets)
			}
		}
	}
	// taken up again: after the failure memory has passed, a play asks and a worker stores it.
	takenUp := func(what string) {
		t.Helper()
		s.forgetFailures(t)
		stop := s.metered(t, newMeters(s, 0))
		defer stop()
		s.askForTracks(t, s.pool)
		s.waitStored(t, 1)
		intact(what + ", taken up again")
		if subtitles.Unreadable(s.source(t), s.track) {
			t.Errorf("%s: the track is marked unreadable for good", what)
		}
	}

	for opens := int64(1); opens <= 3; opens++ {
		what := "a store error at open " + itoa(int(opens)) + " of the film"
		t.Run(what, func(t *testing.T) {
			s.forgetTrack(t)
			s.fresh(t)
			failing := newMeters(s, 0)
			failing.film.failing.Store(opens)
			stop := s.metered(t, failing)
			s.askForTracks(t, s.pool)
			// The meter does not count the Open it fails, so the failure is seen by its
			// counter having run out; opens says a build with fewer opens has begun.
			waitFor(t, "the extraction to end", time.Minute, func() bool {
				return (failing.film.failing.Load() == 0 || failing.film.opens.Load() >= 1) && settled()
			})
			stop()
			intact(what)
			for _, row := range s.extractionRows(t) {
				if strings.HasSuffix(row, " true") {
					t.Errorf("%s: the job is remembered as the film's own failure: %s", what, row)
				}
			}
			takenUp(what)
		})
	}

	t.Run("a graceful stop", func(t *testing.T) {
		s.forgetTrack(t)
		held := newMeters(s, 0)
		held.film.gate, held.film.first = make(chan struct{}), &atomic.Bool{}
		stop := s.roleWorker(t, "subtitles", held)
		s.askForTracks(t, s.pool)
		waitFor(t, "the extraction to begin its read", time.Minute, held.film.gated.Load)
		stop() // The worker is told to go while its extraction waits on the store.
		if got := s.storedObjects(); len(got) != 0 {
			t.Errorf("a worker that was stopped mid-read stored %v", got)
		}
		if s.jobRowsIn(t, "failed") != 0 {
			t.Errorf("a graceful stop failed the job: %v", s.extractionRows(t))
		}
		takenUp("a graceful stop")
	})

	t.Run("a lease taken", func(t *testing.T) {
		s.forgetTrack(t)
		held := newMeters(s, 0)
		held.film.gate, held.film.first = make(chan struct{}), &atomic.Bool{}
		stop := s.roleWorker(t, "subtitles", held)
		defer stop()
		s.askForTracks(t, s.pool)
		waitFor(t, "the extraction to begin its read", time.Minute, held.film.gated.Load)
		// Another worker has the job now, as after a partition.
		if _, err := s.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET worker = 'another', lease_expires_at = now() + interval '1 hour'"); err != nil {
			t.Fatal(err)
		}
		close(held.film.gate)
		time.Sleep(4 * time.Second) // Longer than a lease: whatever the first worker was to do with its read, it has.
		if got := s.storedObjects(); len(got) != 0 {
			t.Errorf("a worker that lost its lease stored %v", got)
		}
		if rows := s.extractionRows(t); len(rows) != 1 || !strings.Contains(rows[0], " running ") {
			t.Errorf("the job of the worker that has the lease was changed by the one that lost it: %v", rows)
		}
		if _, err := s.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET lease_expires_at = now() - interval '1 second'"); err != nil {
			t.Fatal(err)
		}
		s.waitStored(t, 1) // The still-running worker takes the lapsed job.
		intact("a lease taken")
	})
}

// A worker killed in the middle of its extraction stores nothing and another
// worker takes the job (invariant 43): two worker processes, the first killed
// while it reads the film at a tenth of the speed it could.
func TestAnExtractionOfAKilledWorkerIsTakenUpByAnotherWorker(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 100, noise: "2M", cues: []testenv.Cue{top(6, 90)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.fresh(t)
	s.burnt()
	slowAt(t, "10") // Set before the processes start: they read the film at ten times its speed.
	dir := t.TempDir()
	env := []string{"JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_LEASE=3s", "JELLYSCALE_TEST_HEARTBEAT=200ms"}
	a := s.workerProcess(t, "extract-a", dir, env...)
	s.askForTracks(t, s.pool)
	waitFor(t, "worker a to run the extraction", time.Minute, func() bool {
		return s.count(t, "SELECT count(*) FROM subtitle_jobs WHERE state = 'running' AND worker = 'extract-a'") == 1
	})
	a.Process.Signal(syscall.SIGKILL) //nolint:errcheck
	if got := s.storedObjects(); len(got) != 0 {
		t.Fatalf("the extraction had stored %v before it was killed: it was too fast for the test", got)
	}
	s.workerProcess(t, "extract-b", dir, env...)
	waitFor(t, "worker b to take the job", time.Minute, func() bool {
		return s.count(t, "SELECT count(*) FROM subtitle_jobs WHERE worker = 'extract-b' AND state IN ('running', 'done')") == 1
	})
	s.waitStored(t, 1)
	if rows := s.extractionRows(t); len(rows) != 1 || !strings.Contains(rows[0], " done ") {
		t.Errorf("the job after the kill is %v, want one that is done", rows)
	}
}

// A film whose image track cannot be extracted for a reason that is the film's
// (here a track the film lists and does not have, which ffmpeg answers with
// "matches no streams") keeps the track listed, is not read again for it until
// its file changes, and its other image track is stored all the same, with at
// most a second read of the film, as the text pass does.
func TestATrackThatCannotBeExtractedIsKeptListedNotReadAgainAndTheOtherIsStored(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 60, noise: "1M", cues: []testenv.Cue{top(6, 50)}}.write(t, media, "film")
	size := filmBytes(t, media)
	s := newScanned(t, media, t.TempDir())
	if _, err := s.pool.Exec(context.Background(), `UPDATE media_sources SET streams = streams || jsonb_build_array((SELECT elem || '{"Index": 9}'::jsonb
		FROM jsonb_array_elements(streams) elem WHERE elem->>'Index' = '2'))`); err != nil {
		t.Fatal(err)
	}
	if len(s.source(t).Streams) < 4 {
		t.Fatalf("the film has no phantom track: %+v", s.source(t).Streams)
	}
	m := newMeters(s, 0)
	stop := s.metered(t, m)
	s.fresh(t)
	s.burnt()
	s.askForTracks(t, s.pool)
	s.waitStored(t, 1) // Only track 2 is there.
	read := quiet(t, m.film)
	stop()
	if read > 2*size+(4<<20) {
		t.Errorf("the film's store served %d bytes for a track that is stored and one that cannot be: over two reads of the film (%d)", read, size)
	}
	if subtitles.Unreadable(s.source(t), 9) {
		t.Error("the track the film cannot be read for is hidden: an image track stays listed")
	}
	s.forgetFailures(t)
	again := newMeters(s, 0)
	stop = s.metered(t, again)
	s.askForTracks(t, s.pool)
	time.Sleep(3 * time.Second)
	stop()
	if got := again.film.read.Load(); got != 0 {
		t.Errorf("a later play read %d bytes of the film for a track that cannot be extracted: it is read again until its file changes", got)
	}
}

// A film whose file is replaced is read again for its own track, and only once.
// The new file has its cue at other times; a mid-film run draws and reads by
// them, never from the old file's track (JS-071's replacement checks, with the
// track stored).
func TestAfterTheFileIsReplacedAMidFilmRunDrawsAndReadsByTheNewFilesTrack(t *testing.T) {
	media := t.TempDir()
	long := cueFilm{codec: "copy", seconds: 128, noise: "2M", dim: true, cues: []testenv.Cue{top(4, 120)}}
	short := cueFilm{codec: "copy", seconds: 128, noise: "2M", dim: true, cues: []testenv.Cue{bottom(96, 128)}}
	long.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	old := s.storeTrack(t, 1, 0)
	const cut = 28 // 112 s: the old cue is 108 s old and the new one 16 s.
	if got := s.seekStored(t, cut, 2, true, 0); got.assets <= 0 {
		t.Fatalf("a run before the file was replaced read nothing from the stored track")
	}
	expectPicture(t, "a run at 112 s in the first file", s.halvesFrom(t, cut, cut+2), 1, "top")
	before := s.storedObjects()

	s.replace(t, short)
	// Until its own track is stored, the old one is not used (the old file's cue
	// would draw the top bar: no bar is right, as JS-071's replacement checks hold).
	m := newMeters(s, 0)
	stop := s.metered(t, m)
	s.fresh(t)
	s.burnt()
	s.askForTracks(t, s.pool)
	waitFor(t, "the new file's track", time.Minute, func() bool { return len(s.storedObjects()) == len(before)+1 })
	again := quiet(t, m.film)
	stop()
	if again > old.film+(4<<20) || again < old.film/2 {
		t.Errorf("the new file was read for %d bytes for its own track; the old one's took %d: it should be read once", again, old.film)
	}
	plain := []int64{s.seekStored(t, cut, 1, false, readRate).film, s.seekStored(t, cut, 1, false, readRate).film}
	_, most, width := spread(plain...)
	for range 2 {
		got := s.seekStored(t, cut, 2, true, readRate)
		if got.film > most+width+readMargin {
			t.Errorf("a run in the new file read %d bytes of the film, %d more than without the track: it read by the old file's cue", got.film, got.film-most)
		}
		expectPicture(t, "a run at 112 s in the new file", s.halvesFrom(t, cut, cut+2), 1, "bottom")
	}
	var oldKey string
	for key := range before {
		oldKey = key
	}
	if _, kept := s.storedObjects()[oldKey]; !kept {
		t.Logf("the old file's track was removed already") // The sweep's, not this check's.
	}
}

// twoTrackFilm writes a film of seconds with one or two image tracks (stream 2
// and 3) of the codecs, each with its cues on a 1920x1080 canvas.
func twoTrackFilm(t *testing.T, dir, name string, codecs [2]string, cues [2][]testenv.Cue, seconds int) {
	t.Helper()
	testenv.FFmpeg(t)
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24000/1001:duration=" + itoa(seconds),
		// White noise as FLAC: the film is big enough that reading it twice shows, and the picture stays black.
		"-f", "lavfi", "-i", "anoisesrc=d=" + itoa(seconds) + ":c=white:r=48000:a=0.5"}
	maps := []string{"-map", "0:v", "-map", "1:a"}
	for i, codec := range codecs {
		if codec == "" {
			continue
		}
		sup := filepath.Join(t.TempDir(), "track.sup")
		if err := os.WriteFile(sup, testenv.PGS(1920, 1080, cues[i]...), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-fix_sub_duration", "-i", sup)
		maps = append(maps, "-map", itoa(i+2), "-c:s:"+itoa(i), codec, "-metadata:s:s:"+itoa(i), "language=eng")
	}
	args = append(args, maps...)
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-g", "48",
		"-max_interleave_delta", "0", "-c:a", "flac", filepath.Join(dir, name+".mkv"))
	run(t, "ffmpeg", args...)
}
