//go:build integration

package transcode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// previousRelease is the release before JS-071, whose scan stores the film as
// it was then. A film in a library when the new release is deployed was
// scanned by it, and its file does not change.
const previousRelease = "e5bf05e9bd5870065b1e3fc2452ac1d7ebf99d3a"

const oldScan = `package main

import (
	"context"
	"log/slog"
	"os"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
)

func main() {
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Args[1])
	if err != nil {
		panic(err)
	}
	libs, err := library.Sync(ctx, pool, []library.Config{{Name: "cues", Kind: "movies", Media: "file://" + os.Args[2], Assets: "file://" + os.Args[3]}})
	if err != nil {
		panic(err)
	}
	if err := (&library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler)}).Scan(ctx, libs[0]); err != nil {
		panic(err)
	}
}
`

// scannedByThePreviousRelease scans media with a build of the previous release,
// into a database of its own, and returns the rig of this release on that
// database, whose library has not been scanned by this release.
func scannedByThePreviousRelease(t *testing.T, media, assets string) *scannedFilm {
	t.Helper()
	ctx := context.Background()
	export := t.TempDir()
	if out, err := exec.Command("git", "-C", "../..", "archive", "--format=tar", "-o", filepath.Join(export, "release.tar"), previousRelease).CombinedOutput(); err != nil {
		t.Fatalf("git archive %s (the previous release; CI fetches the whole history): %v\n%s", previousRelease, err, out)
	}
	if out, err := exec.Command("tar", "-x", "-C", export, "-f", filepath.Join(export, "release.tar")).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(export, "cmd", "oldscan"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(export, "cmd", "oldscan", "main.go"), []byte(oldScan), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "oldscan")
	build := exec.Command("go", "build", "-o", binary, "./cmd/oldscan")
	build.Dir = export
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the previous release's scan: %v\n%s", err, out)
	}

	// A database of its own, so that the previous release's schema is what this
	// release's migrations find.
	admin, err := pgxpool.New(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "js071_" + uuid.NewString()[:8]
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") }) //nolint:errcheck
	config := admin.Config().ConnConfig
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s", config.User, config.Password, config.Host, config.Port, name)
	if out, err := exec.Command(binary, dsn, media, assets).CombinedOutput(); err != nil {
		t.Fatalf("the previous release's scan: %v\n%s", err, out)
	}

	pool, err := db.Open(ctx, dsn) // This release's migrations.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	user := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash) VALUES ($1, 'viewer', 'x')`, user); err != nil {
		t.Fatal(err)
	}
	r := &rig{dsn: dsn, pool: pool, items: &items.Repo{Pool: pool, ServerID: "srv"}, scratch: t.TempDir(), user: user,
		log:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		spec: playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 1280, Height: 720}}
	libs, err := library.Sync(ctx, pool, []library.Config{{Name: "cues", Kind: "movies", Media: "file://" + media, Assets: "file://" + assets}})
	if err != nil {
		t.Fatal(err)
	}
	return &scannedFilm{rig: r, media: media, assets: assets, lib: libs[0]}
}

// A film already in the library when the release is deployed (scanned by the
// previous release, its file unchanged) meets the criteria by the next scan at
// the latest, with no step by the operator: after one scan by this release, a
// run inside a cue 56 s old draws it from its first frame, and reads what a
// cue that old allows, in the first mid-film run on the film as in a later one.
func TestAFilmScannedByThePreviousReleaseMeetsTheCriteriaByTheNextScan(t *testing.T) {
	cues := []testenv.Cue{top(4, 8), bottom(56, 120)}
	const cut = 28 // 112 s: the second cue is 56 s old.
	t.Run("draws", func(t *testing.T) {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: 128, cues: cues}.write(t, media, "film")
		s := scannedByThePreviousRelease(t, media, t.TempDir())
		s.rescan(t) // The next scan, by this release.
		s.seek(t, cut, 2, true, 0)
		expectPicture(t, "a run at segment 28 of a film the previous release scanned", s.halvesFrom(t, cut, cut+2), 1, "bottom")
		expectPicture(t, "a run at segment 28 of a film the previous release scanned", s.halvesFrom(t, cut, cut+2), 5, "bottom")
	})
	t.Run("reads", func(t *testing.T) {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: cues}.write(t, media, "film")
		s := scannedByThePreviousRelease(t, media, t.TempDir())
		s.rescan(t)
		burnt := []int64{s.seek(t, cut, 1, true, readRate)}
		plain := []int64{s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate)}
		burnt = append(burnt, s.seek(t, cut, 1, true, readRate))
		limit := insideLimit(plain, 56)
		t.Logf("a film scanned by the previous release: %v bytes read burnt in (the first run, then a later one), %v without the track; the limit is %d", burnt, plain, limit)
		for _, got := range burnt {
			if got > limit {
				t.Errorf("a run read %d bytes before its first segment, over the %d that a cue 56 s old allows", got, limit)
			}
		}
	})
}

// The same film already in the library when the release is deployed is read by
// the next scan, which has to learn what the first scan of a new film does; it
// may not do it with a read that grows with the film, nor with one that no
// first scan makes (a whole-film read of every film with an image track, on
// the first scan after the deploy: the cost the operator would find out
// from the store's bill). Films of 1 Mbps noise, 64 s and 256 s, each scanned
// by the previous release and then by this one through a meter, with the image
// track and without it; the extra for the track is the same in both and no more
// than the track itself costs the first scan (scanTrackCost).
func TestTheNextScanOfAFilmTheLastReleaseScannedReadsNothingThatGrowsWithTheFilm(t *testing.T) {
	rescanRead := func(seconds int, withTrack bool) int64 {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: seconds, noise: "1M", noTrack: !withTrack, cues: []testenv.Cue{top(4, 20), bottom(30, 50)}}.write(t, media, "film")
		s := scannedByThePreviousRelease(t, media, t.TempDir())
		m := newMeter(media, 0)
		s.lib.Media = m
		s.rescan(t)
		return m.read.Load()
	}
	extra := map[int]int64{}
	for _, seconds := range []int{64, 256} {
		with, without := rescanRead(seconds, true), rescanRead(seconds, false)
		extra[seconds] = with - without
		t.Logf("a %d s film: the next scan read %d bytes with the image track and %d without it", seconds, with, without)
	}
	if extra[256] > scanTrackCost {
		t.Errorf("the next scan of a 256 s film read %d bytes more for its image track, over the %d the track itself costs", extra[256], int64(scanTrackCost))
	}
	if diff := extra[256] - extra[64]; diff > scanTrackCost || diff < -scanTrackCost {
		t.Errorf("the next scan read %d bytes more for the image track of the 256 s film and %d more for the 64 s one: what it reads grows with the film (by %d)", extra[256], extra[64], diff)
	}
}
