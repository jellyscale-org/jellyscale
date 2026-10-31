package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// A worker's Cache (no KeepGoing). Caller A starts the extraction and is then
// cancelled while caller B waits on the same one. B's context is live, so B
// should still get the track.
func TestCancelledCallerDoesNotFailAnother(t *testing.T) {
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("no database")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:02,000 --> 00:00:03,000\nOne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	media, assets := filepath.Join(dir, "media"), filepath.Join(dir, "assets")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(media, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3",
		"-i", srt, "-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "srt", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(film)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := store.Open("file://" + media)
	if err != nil {
		t.Fatal(err)
	}
	as, err := store.Open("file://" + assets)
	if err != nil {
		t.Fatal(err)
	}
	src := items.Source{ID: "flight", Key: "film.mkv", ETag: "e", Size: info.Size()}

	// Each ffmpeg notes its start, then waits a second, so B can join A's run.
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "started")
	script := fmt.Sprintf("#!/bin/sh\necho run >>%q\nsleep 1\nexec %q \"$@\"\n", started, real)
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cache := Cache{Pool: pool}
	ctxA, cancelA := context.WithCancel(ctx)
	doneA := make(chan error, 1)
	go func() {
		_, err := cache.Track(ctxA, src, ms, as, 1)
		doneA <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("A's ffmpeg never started")
		}
	}
	type outcome struct {
		track Track
		err   error
	}
	doneB := make(chan outcome, 1)
	go func() {
		track, err := cache.Track(ctx, src, ms, as, 1)
		doneB <- outcome{track, err}
	}()
	time.Sleep(300 * time.Millisecond)
	cancelA()
	t.Logf("A: %v", <-doneA)
	b := <-doneB
	t.Logf("B: err=%v srt=%q", b.err, b.track.SRT)
	if b.err != nil {
		t.Errorf("B, whose context is live, failed when A was cancelled: %v", b.err)
	}
}

// keyFor is Track's key for a source and index, so a test can hold its lock.
func keyFor(src items.Source, index int) string {
	sum := sha256.Sum256([]byte(src.ETag))
	return "subtitles/" + src.ID + "/" + hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(index) + ".json"
}

// Four tracks are being extracted by another replica, so this process's four
// requests for them only poll the lock. A fifth request for a free track has
// nothing to wait for, so it should answer quickly. A poller must not hold a slot
// while it polls.
func TestPollersDoNotHoldTheSlots(t *testing.T) {
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("no database")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:02,000 --> 00:00:03,000\nOne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	media, assets := filepath.Join(dir, "media"), filepath.Join(dir, "assets")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(media, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3",
		"-i", srt, "-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "srt", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(film)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := store.Open("file://" + media)
	if err != nil {
		t.Fatal(err)
	}
	as, err := store.Open("file://" + assets)
	if err != nil {
		t.Fatal(err)
	}

	// Another replica's extraction, as far as this process can tell: its lock is held.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	cache := Cache{Pool: pool}
	stop, cancelPollers := context.WithCancel(ctx)
	defer cancelPollers()
	for i := range 4 {
		held := items.Source{ID: fmt.Sprintf("held-%d", i), Key: "absent.mkv", ETag: "e", Size: 1}
		if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", keyFor(held, 1)); err != nil {
			t.Fatal(err)
		}
		go cache.Track(stop, held, ms, as, 1) //nolint:errcheck // Ends when the test does.
	}
	time.Sleep(500 * time.Millisecond)

	free := items.Source{ID: "free", Key: "film.mkv", ETag: "f", Size: info.Size()}
	began := time.Now()
	type outcome struct {
		track Track
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		track, err := cache.Track(ctx, free, ms, as, 1)
		done <- outcome{track, err}
	}()
	select {
	case r := <-done:
		t.Logf("free track answered after %s with the holder still holding: err=%v", time.Since(began).Round(time.Millisecond), r.err)
		if r.err != nil {
			t.Errorf("free track failed: %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("free track had no answer after 3 s while four other tracks were only polling for their locks")
		holder.Exec(ctx, "SELECT pg_advisory_unlock_all()") //nolint:errcheck
		r := <-done
		t.Logf("after the holder let go, free track answered after %s: err=%v", time.Since(began).Round(time.Millisecond), r.err)
	}
}
