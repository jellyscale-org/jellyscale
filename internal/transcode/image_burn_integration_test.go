//go:build integration

package transcode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// PGS cues on a 1920x1080 canvas over a 640x360 black film: a bar in the top
// half from 6 to 10 seconds and one in the bottom half from 22 to 30, as
// burnTrack has for text. The film runs ten seconds past the last packet, which
// a stalled overlay would not get through.
func imageBurnSource(t *testing.T, codec string) string {
	t.Helper()
	return imageFilm(t, t.TempDir(), "film.mkv", codec)
}

// imageFilm writes the film into dir and returns its path. codec is the one the
// track is stored in: "copy" for PGS, "dvdsub" or "dvbsub".
func imageFilm(t *testing.T, dir, name, codec string) string {
	t.Helper()
	testenv.FFmpeg(t)
	sup := filepath.Join(t.TempDir(), "track.sup")
	if err := os.WriteFile(sup, testenv.PGS(1920, 1080,
		testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120},
		testenv.Cue{Start: 22, End: 30, X: 660, Y: 860, W: 600, H: 120}), 0o644); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(dir, name)
	// ffmpeg writes VobSub and DVB from bitmaps, so those are made from the PGS
	// track; the fixed durations give each cue the end the clearing set had.
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24000/1001:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
		"-fix_sub_duration", "-i", sup, "-map", "0:v", "-map", "1:a", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48",
		"-c:a", "flac", "-c:s", codec, "-metadata:s:s:0", "language=eng", film)
	return film
}

// The track is in the picture at its cue's time, from a run that starts at zero
// and from a restart where no cue is showing, and the run gets to the end of a
// film whose track has no packets for its last ten seconds. This calls Command
// as it is, with no times of the track's cues, so the restarts that land inside
// a cue (which need them) are the worker-level checks of JS-071, where the cue
// times reach the run as they do in production
// (TestAMidFilmRunDrawsTheCueAFilmFromZeroShowsAtItsCut, TestASeekInto...).
func TestBurnedImageSubtitleIsAtTheRightTime(t *testing.T) {
	for _, tc := range []struct{ name, codec string }{{"PGS", "copy"}, {"VobSub", "dvdsub"}, {"DVB", "dvbsub"}} {
		t.Run(tc.name, func(t *testing.T) {
			film := imageBurnSource(t, tc.codec)
			index := 2
			spec := testSpec
			spec.Subtitle = &index
			image := &playback.Stream{Index: 2, Type: "Subtitle", IsImageSubtitleStream: media.Ptr(true)}
			if tc.name != "DVB" {
				image.Width, image.Height = 1920, 1080
			}
			build := func(spec playback.Spec, start int) string {
				dir := t.TempDir()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				track := image
				if spec.Subtitle == nil {
					track = nil
				}
				cmd, err := Command(ctx, film, spec, start, dir, "", track)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, out)
				}
				return dir
			}
			const total = 10
			const none, top, bottom, edge = 0, 1, 2, 3
			want := func(second int) int {
				switch {
				case second >= 7 && second <= 9:
					return top
				case second >= 23 && second <= 29:
					return bottom
				case second <= 4 || (second >= 12 && second <= 20) || second >= 32:
					return none
				}
				return edge
			}
			whole := build(spec, 0)
			for _, restart := range []int{0, 3} {
				dir := whole
				if restart > 0 {
					dir = build(spec, restart)
				}
				spliced := join(t, filepath.Join(whole, "init.mp4"), append(segmentFiles(whole, 0, restart), segmentFiles(dir, restart, total)...)...)
				frames := brightByHalf(t, spliced)
				if len(frames) < 39 {
					t.Fatalf("restart %d: %d frames", restart, len(frames))
				}
				for second, half := range frames {
					got := none
					switch {
					case half[0] >= 20 && half[1] < 20:
						got = top
					case half[1] >= 20 && half[0] < 20:
						got = bottom
					case half != [2]int{}:
						got = edge
					}
					if expect := want(second); expect != edge && got != expect {
						t.Errorf("restart %d, second %d: bright pixels top %d, bottom %d; want %v (0 none, 1 top, 2 bottom)", restart, second, half[0], half[1], expect)
					}
				}
			}
			// Without the track the picture is black, so the counts above can fail.
			plain := build(testSpec, 0)
			for second, half := range brightByHalf(t, join(t, filepath.Join(plain, "init.mp4"), segmentFiles(plain, 0, total)...)) {
				if half != [2]int{} {
					t.Fatalf("second %d is not black without a subtitle: %v", second, half)
				}
			}
		})
	}
}

// slowFile is a file that is read at rate bytes a second, counting what it has
// sent (all the requests together). It has no WriteTo, which would let the copy skip Read.
type slowFile struct {
	file *os.File
	rate int
	sent *atomic.Int64
}

func (f slowFile) Read(p []byte) (int, error) {
	n, err := f.file.Read(p)
	f.sent.Add(int64(n))
	time.Sleep(time.Duration(n) * time.Second / time.Duration(f.rate))
	return n, err
}

func (f slowFile) Seek(offset int64, whence int) (int64, error) { return f.file.Seek(offset, whence) }

// A track with one cue and no packets after it, in a film that arrives slowly,
// doesn't hold the picture back until the film has all arrived: ffmpeg's overlay
// waits for the track's next packet, and what lets the video through meanwhile
// is that ffmpeg tells it the video's time. Half way through the read, far more
// of the film than the cue's few seconds has been made.
func TestBurnedImageSubtitleDoesNotWaitForTheNextCue(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	sup := filepath.Join(dir, "track.sup")
	if err := os.WriteFile(sup, testenv.PGS(1920, 1080, testenv.Cue{Start: 10, End: 14, X: 660, Y: 100, W: 600, H: 120}), 0o644); err != nil {
		t.Fatal(err)
	}
	// Noise, so the film is big enough to be read slowly: about 4 Mbps for 120 s.
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=120",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=120", "-i", sup, "-map", "0:v", "-map", "1:a", "-map", "2",
		"-vf", "noise=alls=40:allf=t", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4M", "-minrate", "4M", "-maxrate", "4M", "-bufsize", "4M", "-g", "48",
		"-c:a", "flac", "-c:s", "copy", filepath.Join(dir, "film.mkv"))
	info, err := os.Stat(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, err := os.Open(filepath.Join(dir, "film.mkv")) // Each request has its own place in the file.
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "video/x-matroska")
		http.ServeContent(w, r, "", time.Time{}, slowFile{file, int(info.Size()) / 12, &sent})
	}))
	defer server.Close()

	index := 2
	spec := testSpec
	spec.Subtitle = &index
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd, err := Command(ctx, server.URL+"/film.mkv", spec, 0, out, "", &playback.Stream{Index: 2, Width: 1920, Height: 1080, IsImageSubtitleStream: media.Ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	made := func() int {
		files, _ := filepath.Glob(filepath.Join(out, "*.m4s"))
		return len(files)
	}
	// ffmpeg opens the film twice, for the video and for the audio, so the
	// requests have sent the film's size once each have read half of it.
	halfway := false
	for !halfway {
		select {
		case err := <-finished:
			t.Fatalf("ffmpeg ended before half the film was read: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		halfway = sent.Load() >= info.Size()
	}
	if n := made(); n < 10 {
		t.Errorf("%d segments made when half the film (about a minute of it) had been read, and the cue is in the first four; want about 14", n)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if n := made(); n != 30 {
		t.Errorf("%d segments in the end, want 30", n)
	}
}
