//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// A subtitle file beside the film that a burn-in can't read is hidden from the
// film, as an unreadable text file is (docs/subtitles.md, "Limits to know"):
// the pick that finds out fails, once, and the stream is marked Unreadable
// in the media source, which PlaybackInfo and the details page stop listing.
// A .sup or .sub of random bytes is such a file, and ffmpeg gives two answers
// to it: a .sup is refused at once (exit 183, "Invalid data found when
// processing input") and a .sub beside a good .idx reads as nothing, so ffmpeg
// goes on until the worker's 30 second stall check. Both must be found out
// well inside that, by the worker, not by waiting for the stall.

// randomBytes is noise that is neither a PGS file (which starts with "PG") nor
// MPEG program stream sectors (which start 00 00 01 ba).
func randomBytes(n int) []byte {
	noise := make([]byte, n)
	rand.New(rand.NewSource(27)).Read(noise)
	noise[0], noise[1] = 0x41, 0x42
	return noise
}

// useBesideImage lists files beside the film as its stream 2, the way a scan
// does, with stream list entry json (Index 2 is added), and a dto with the one
// media source that library.MarkUnreadable rewrites.
func (r *rig) useBesideImage(t *testing.T, entry string, files map[string][]byte) {
	t.Helper()
	dir := film(t)
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
	}
	r.useFile(t, "film.mkv")
	if _, err := r.pool.Exec(context.Background(), `UPDATE media_sources SET streams = streams || $1::jsonb`, "["+entry+"]"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), `UPDATE items SET dto = jsonb_build_object('MediaSources', jsonb_build_array(jsonb_build_object()), 'MediaStreams', jsonb_build_array($1::jsonb))`, entry); err != nil {
		t.Fatal(err)
	}
}

// subtitleState says whether stream 2 is still in the streams of the film
// that PlaybackInfo and the item are made from (the dto's MediaStreams) and
// whether the media source marks it Unreadable.
func (r *rig) subtitleState(t *testing.T) (listed, marked bool) {
	t.Helper()
	var dto, streams []byte
	if err := r.pool.QueryRow(context.Background(), `SELECT i.dto, m.streams FROM items i JOIN media_sources m ON m.item_id = i.id`).Scan(&dto, &streams); err != nil {
		t.Fatal(err)
	}
	var item struct{ MediaStreams []map[string]any }
	if err := json.Unmarshal(dto, &item); err != nil {
		t.Fatal(err)
	}
	for _, s := range item.MediaStreams {
		if s["Type"] == "Subtitle" && s["Index"] == float64(2) {
			listed = true
		}
	}
	var all []map[string]any
	if err := json.Unmarshal(streams, &all); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s["Type"] == "Subtitle" && s["Index"] == float64(2) && s["Unreadable"] == true {
			marked = true
		}
	}
	return listed, marked
}

const supEntry = `{"Index": 2, "Type": "Subtitle", "Codec": "PGSSUB", "IsExternal": true, "IsImageSubtitleStream": true, "IsTextSubtitleStream": false,
	"Path": "damaged.sup", "Fingerprint": "f1", "Width": 1920, "Height": 1080}`
const vobEntry = `{"Index": 2, "Type": "Subtitle", "Codec": "DVDSUB", "IsExternal": true, "IsImageSubtitleStream": true, "IsTextSubtitleStream": false,
	"Path": "damaged.idx", "SubPath": "damaged.sub", "Fingerprint": "f1", "Width": 720, "Height": 480}`

// burn picks stream 2 (fingerprint f1) as a viewer, and returns how the pick
// ended and how long it took.
func (r *rig) burn(t *testing.T, j *Jobs, n int) (error, time.Duration) {
	t.Helper()
	index := 2
	r.spec.Subtitle, r.spec.SubtitleFingerprint = &index, "f1"
	r.hash = r.spec.Hash()
	began := time.Now()
	_, err := r.fetch(t, j, psid(n), 0)
	return err, time.Since(began)
}

// The first pick of a file the burn-in can't read fails the job, and the
// worker hides the file: PlaybackInfo and the item no longer list it. The .sub
// is found out at once as well, not by the stall check (30 s by default).
func TestDamagedExternalImageFileIsHiddenByTheBurnThatFindsItOut(t *testing.T) {
	idx, _ := testenv.VobSub(720, 480, testenv.Cue{Start: 6, End: 10, X: 247, Y: 44, W: 225, H: 53})
	for _, tc := range []struct {
		name, entry string
		files       map[string][]byte
	}{
		{"a .sup of random bytes", supEntry, map[string][]byte{"damaged.sup": randomBytes(4096)}},
		{"a .sup that is empty", supEntry, map[string][]byte{"damaged.sup": {}}},
		{"a .sub of random bytes beside a good .idx", vobEntry, map[string][]byte{"damaged.idx": idx, "damaged.sub": randomBytes(8192)}},
		{"a .sub that is empty beside a good .idx", vobEntry, map[string][]byte{"damaged.idx": idx, "damaged.sub": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.FFmpeg(t)
			r := newRig(t, nil)
			r.useBesideImage(t, tc.entry, tc.files)
			r.worker(t, nil) // The default Stall, 30 seconds: a failure that waits for it is the bug.
			j := r.jobs(t)

			err, took := r.burn(t, j, 0)
			var failed FailedError
			if !errors.As(err, &failed) {
				t.Fatalf("the pick: %v after %s, want the job's failure", err, took)
			}
			if took > 15*time.Second {
				t.Errorf("the pick failed after %s, which is the stall check's doing, not the file being found unreadable", took)
			}
			waitFor(t, "the file to be marked unreadable", 10*time.Second, func() bool { _, marked := r.subtitleState(t); return marked })
			if listed, _ := r.subtitleState(t); listed {
				t.Error("the film still lists the damaged file")
			}
			waitFor(t, "no ffmpeg to be left", 10*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
		})
	}
}

// A file the burn can read is not hidden, and neither is one the burn failed
// to get for a reason that isn't the file's (the store lost it): the first
// would hide good subtitles, the second would hide them for good over a blip.
func TestReadableAndMissingExternalImageFilesAreNotHidden(t *testing.T) {
	good := testenv.PGS(1920, 1080, testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120})
	idx, sub := testenv.VobSub(720, 480, testenv.Cue{Start: 6, End: 10, X: 247, Y: 44, W: 225, H: 53})
	for _, tc := range []struct {
		name, entry string
		files       map[string][]byte
		fails       bool
	}{
		{"a good .sup", supEntry, map[string][]byte{"damaged.sup": good}, false},
		{"a good .sub and .idx", vobEntry, map[string][]byte{"damaged.idx": idx, "damaged.sub": sub}, false},
		{"a .sup the store has lost", supEntry, nil, true},
		{"a .sub the store has lost beside its .idx", vobEntry, map[string][]byte{"damaged.idx": idx}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.FFmpeg(t)
			r := newRig(t, nil)
			r.useBesideImage(t, tc.entry, tc.files)
			r.worker(t, nil)
			j := r.jobs(t)
			err, _ := r.burn(t, j, 0)
			var failed FailedError
			if errors.As(err, &failed) != tc.fails {
				t.Fatalf("the pick: %v, fails should be %v", err, tc.fails)
			}
			// The mark, if there is to be one, is made by the time the pick has failed or a second has passed.
			time.Sleep(2 * time.Second)
			if listed, marked := r.subtitleState(t); marked || !listed {
				t.Errorf("hidden (listed %v, marked %v), though the file is not what failed", listed, marked)
			}
		})
	}
}
