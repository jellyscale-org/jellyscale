//go:build integration

package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// The dogfood's seek (2026-11-20) failed with the recorded reason "finding where the audio's packets fall: ffmpeg made no
// packet" for a film whose DTS-HD MA 5.1 track (512-sample frames) starts 4 ms after the film and whose first decoded frames are
// lost. audioGrid encodes one frame of the track's head (-frames:a 1) to find where a run from the film's start puts its AAC
// packets, and for such a track that makes no packet at all, where two frames make one before zero (the dogfood, ffmpeg 8.1.3:
// first packet at -832 samples; here -304). The film plays from its start (a run from the start needs no grid) and has its sound.
//
// The kit's row dtshdma-6-head is that film: FATE's DTS-HD MA 5.1 stream, repeated, without its first 100 bytes, starting 192
// samples late. It is made only if the probe of one frame makes no packet and the probe of two makes one before zero, on the
// ffmpeg the test runs under (measured with the probe alone, each ffmpeg 6.1.1 (Ubuntu 24.04, CI's), 8.1.3 (the server image)
// and 9.0.2 (a developer's) makes no packet for one frame and one at -304 for two; the whole stream, with the same 4 ms offset,
// makes one at -832 for one frame on each). The same property with an offset of 0 or of 0.1 s also makes no packet for one frame,
// so it is the lost head and not the negative pts that ends the probe. Which frames the dogfood's decoder lost is unverified:
// the outputs are the ones the lead observed there (no packet for one frame, a packet before zero and one at 192 for two).
//
// A seek into such a film gets its segment as a run from the start gets it: its picture from a keyframe at the segment's time and
// the film's sound from its first frame, made by one job that began at the segment and did not fail; and the same film whole
// (dtshdma-6) does so too, which tells a fault of the probe from one of the film's stream. The whole matrix (one top-level test
// each row, seek_matrix_integration_test.go) holds the seek of 908 seconds, a helper's chunk and a failover for the row; this is
// the short film of the dogfood's failure, plain and burnt.
func TestASeekIntoAFilmWhoseAudioHeadIsLostGetsItsSegmentWithSound(t *testing.T) {
	films := newSeekFilms(t)
	const seconds, segment = 90, 9
	for _, c := range []struct{ row, name string }{
		{"dtshdma-6", "the film whose track is whole"},
		{"dtshdma-6-head", "the same track without its first frames"},
	} {
		a := seekRow(t, c.row)
		t.Run(c.name, func(t *testing.T) {
			s := films.rig(t, films.filmOf(t, a, seconds), a)
			for _, burn := range []bool{false, true} {
				t.Run(map[bool]string{false: "plain", true: "burnt"}[burn], func(t *testing.T) {
					s.apply(a, false, burn)
					reference := s.fromTheStart(t, segment-1, segment+2, t.TempDir())
					got := s.seekTo(t, segment)
					for _, reason := range got.reasons {
						if strings.Contains(reason, "made no packet") {
							t.Logf("the dogfood's reason, recorded: %s", reason)
						}
					}
					checkSegment(t, got, reference, segment)
				})
			}
		})
	}
}

// In one play of the film that fails above, several seeks each get their segment the same way: a short one that starts a new
// job, one past half, one as far in as the dogfood's (1183 seconds) and one into the film's last segment.
func TestSeveralSeeksInOnePlayOfAFilmWhoseAudioHeadIsLostEachGetTheirSegment(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "dtshdma-6-head")
	severalSeeks(t, films, a, films.film(t, a))
}

// Each command that cuts a restart asks where the audio's packets fall, and none may end for a track whose head is lost: the
// restart of a re-encode (Command) runs and its segment has the film's sound, and the restarts of an HDR copy's fallback
// (RecodeCommand with an HDR spec) and of an AV1 fallback (AV1SegmentCommand) build.
func TestEveryRestartCommandBuildsForAFilmWhoseAudioHeadIsLost(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "dtshdma-6-head")
	input := filepath.Join(films.filmOf(t, a, 40), "film.mkv")
	spec := testSpec
	spec.Width, spec.Height = 160, 90
	ctx := context.Background()

	t.Run("a re-encode", func(t *testing.T) {
		dir := t.TempDir()
		cmd, err := Command(ctx, input, spec, 5, dir, "", nil, x264)
		if err != nil {
			t.Fatalf("the restart: %v", err)
		}
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("the restart ran: %v\n%s", err, errOut.String())
		}
		if got := times(t, join(t, filepath.Join(dir, "init.mp4"), filepath.Join(dir, "5.m4s")), "a:0", "packet=pts_time"); len(got) == 0 {
			t.Error("the restart's first segment has no audio packet: the film has sound there")
		}
	})
	t.Run("an HDR copy's fallback", func(t *testing.T) {
		if _, err := RecodeCommand(ctx, input, spec.Recoded("hevc", "smpte2084"), "hevc", 20, 20, []float64{20, 24}, t.TempDir()); err != nil {
			t.Errorf("the restart: %v", err)
		}
	})
	t.Run("an AV1 fallback", func(t *testing.T) {
		if _, err := AV1SegmentCommand(ctx, input, spec.Recoded("av1", "smpte2084"), 20, 24, t.TempDir()); err != nil {
			t.Errorf("the restart: %v", err)
		}
	})
}

// When ffmpeg itself refuses the probe, what it said reaches the reason that is recorded (and so the operator): the reason of a
// restart whose audio stream is not in the film names ffmpeg's own words, not only that something was not found.
func TestARestartsProbeFailureCarriesWhatFFmpegSaid(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "aac-2")
	input := filepath.Join(films.filmOf(t, a, 40), "film.mkv")
	spec := testSpec
	spec.Width, spec.Height, spec.AudioIndex = 160, 90, 7
	_, err := Command(context.Background(), input, spec, 5, t.TempDir(), "", nil, x264)
	if err == nil {
		t.Fatal("the restart of a film with no audio stream 7 was built")
	}
	if !strings.Contains(err.Error(), "matches no streams") {
		t.Errorf("the reason %q does not carry ffmpeg's own words (\"Stream map '0:7' matches no streams\")", err)
	}
}

// nthCutStore cuts the k-th read of the film from its start (a read opened at 0, which is how each ffmpeg that opens the film
// begins: a seek's job runs five of them, of which the probe of the audio is one) after `after` bytes, and no other.
type nthCutStore struct {
	store.Store
	opens atomic.Int64
	k     int64
	after int64
}

func (c *nthCutStore) LocalPath(string) (string, bool) { return "", false }

func (c *nthCutStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := c.Store.Open(ctx, key, offset)
	if err != nil || offset != 0 || c.opens.Add(1) != c.k {
		return body, err
	}
	return &seekCutBody{ReadCloser: body, room: c.after}, nil
}

// A cut read is the read's failure, never the track's: whichever of the reads that start a seek's job is cut (the loopback
// server's response closes after 1 KB, before the film's first audio packet, or after 16 KB), the seek is answered, no job is
// recorded as failed for a track with no packet, and the film is not remembered as one that cannot be played. Measured on
// today's build: none of the 10 cuts ends in a recorded failure (the job is made again), so this holds the behaviour that a
// probe of the audio that gave "no packet" for a cut read would lose.
func TestACutReadIsNotReportedAsATrackWithNoPacket(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "aac-2")
	media := films.filmOf(t, a, 60)
	s := films.rig(t, media, a)
	const segment = 9
	for k := int64(1); k <= 5; k++ {
		for _, after := range []int64{1 << 10, 16 << 10} {
			t.Run(fmt.Sprintf("read %d cut after %d bytes", k, after), func(t *testing.T) {
				s.fresh(t)
				cs := &nthCutStore{Store: newMeter(media, 0), k: k, after: after}
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
				got := s.got(t, segment, err)
				for _, reason := range got.reasons {
					if strings.Contains(reason, "no packet") {
						t.Errorf("a cut read was recorded as the track's fault: %q", reason)
					}
				}
				if err != nil {
					t.Errorf("a seek whose read %d was cut: %v (jobs %+v, reasons %q)", k, err, got.jobs, got.reasons)
				}
				if reason, err := j.Unavailable(context.Background(), s.spec, s.user, "device"); err != nil || reason != "" {
					t.Errorf("the film is refused after a read the store cut: %q, %v", reason, err)
				}
			})
		}
	}
}

// And a store that cuts every read ends the seek in a failure that names the read.
func TestEveryReadCutFailsASeekNamingTheRead(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "aac-2")
	media := films.filmOf(t, a, 60)
	s := films.rig(t, media, a)
	s.fresh(t)
	cs := &seekCutStore{Store: newMeter(media, 0), after: 2 << 10}
	cs.cuts.Store(1000)
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
	_, err := s.fetchAt(t, s.jobs(t), psid(0), 9, 9)
	var failed FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("a store that cuts every read was answered %v, want the failure", err)
	}
	if strings.Contains(failed.Reason, "no packet") || !strings.Contains(failed.Reason, "connection reset") {
		t.Errorf("the failure %q does not name the cut read", failed.Reason)
	}
}
