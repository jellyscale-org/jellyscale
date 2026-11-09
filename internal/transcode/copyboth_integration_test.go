//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

// copyBothSegments runs a copy of both streams from segment start to the end
// of the file as a copy job does (copyStart, CopyCommand, the gatherer) and
// returns the directory of its segments, the layout and the ffmpeg arguments.
func copyBothSegments(t *testing.T, input string, source playback.Source, spec playback.Spec, start int) (string, playback.Layout, []string) {
	t.Helper()
	layout := source.Layout(true)
	dir := t.TempDir()
	video, audio := copyStart(source.Keyframes, layout, start)
	cmd, err := CopyCommand(context.Background(), input, spec, source.VideoCodec(), video, audio, dir)
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, errOut.String())
	}
	gather := newGatherer(dir, layout, source.Keyframes[0])
	for n := start; n < layout.Count(); n++ {
		if built, err := gather.segment(context.Background(), n, true); err != nil || !built {
			t.Fatalf("segment %d of a run from %d: built %v: %v", n, start, built, err)
		}
	}
	return dir, layout, cmd.Args
}

// audioCodecOf is the codec of the first audio stream of a file.
func audioCodecOf(t *testing.T, file string) string {
	t.Helper()
	return strings.Trim(strings.TrimSpace(run(t, "ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", file)), ",")
}

// A film whose client plays both codecs and not the container is remuxed: the
// video and the audio in the segments are the source's own packets, and a run
// that restarts on another worker (at any segment) puts the same packets in the
// same segments as a run from the start, so the stream splices. This is the
// audio-copy test (TestCopiedAudioSplicesTheSamePackets) and the video-copy
// one (TestCopiedRestartSplicesWithEarlierSegments) at once: neither stream is
// re-encoded, so for an MP4 source the segments are the same bytes, and for a
// Matroska source the packets are the same and their times differ by under a
// millisecond (Matroska stores whole milliseconds; accepted, DOMAINS 52).
// Codecs run on different packet grids to the video's (AAC 1024 samples, AC3
// 1536, E-AC3 and MP3 1152, Opus 960, FLAC 4096) and in H.264, HEVC and AV1.
func TestCopyOfBothSplicesTheSameSegments(t *testing.T) {
	const matroska = 0.001 + 2e-6 // As TestCopiedAudioSplicesTheSamePackets.
	for _, c := range []struct {
		name, file, codec string
		video             []string
		extra             []string
		d                 delays
		drift             float64 // Seconds the times may differ by.
	}{
		{"aac, h264, mkv", "film.mkv", "aac", h264Fixture, []string{"-c:a", "aac"}, delays{video: 90, audio: 90}, matroska},
		{"aac, h264, mp4", "film.mp4", "aac", h264Fixture, []string{"-c:a", "aac"}, delays{video: 90, audio: 90}, 0},
		{"ac3, h264, mkv", "film.mkv", "ac3", h264Fixture, nil, delays{video: 90, audio: 90}, matroska},
		{"ac3, h264, mp4", "film.mp4", "ac3", h264Fixture, nil, delays{video: 90, audio: 90}, 0},
		{"eac3, h264, mkv", "film.mkv", "eac3", h264Fixture, []string{"-c:a", "eac3"}, delays{video: 90, audio: 90}, matroska},
		{"mp3 at 44.1 kHz, h264, mkv", "film.mkv", "mp3", h264Fixture, []string{"-c:a", "libmp3lame", "-ar", "44100"}, delays{video: 90, audio: 90}, matroska},
		{"opus, h264, mkv", "film.mkv", "opus", h264Fixture, []string{"-c:a", "libopus"}, delays{video: 90, audio: 90}, matroska},
		{"flac, h264, mkv", "film.mkv", "flac", h264Fixture, []string{"-c:a", "flac"}, delays{video: 90, audio: 90}, matroska},
		{"flac, h264, mkv, streams starting late", "film.mkv", "flac", h264Fixture, []string{"-c:a", "flac"}, delays{videoStart: 0.5, audioStart: 0.5, video: 90, audio: 90}, matroska},
		{"aac, h264, mp4, audio starting after the video", "film.mp4", "aac", h264Fixture, []string{"-c:a", "aac"}, delays{audioStart: 0.5, video: 90, audio: 90}, 0},
		{"ac3, hevc, mkv", "film.mkv", "ac3", hevcFixture, nil, delays{video: 90, audio: 90}, matroska},
		{"aac, hevc, mp4 tagged hev1", "film.mp4", "aac", hevcFixture, []string{"-c:a", "aac", "-tag:v", "hev1"}, delays{video: 90, audio: 90}, 0},
		{"opus, av1, mkv", "film.mkv", "opus", av1Fixture, []string{"-c:a", "libopus"}, delays{video: 90, audio: 90}, matroska},
	} {
		t.Run(c.name, func(t *testing.T) {
			input, source := codecSource(t, c.file, c.video, c.d, c.extra...)
			if got := audioCodecOf(t, input); got != c.codec {
				t.Fatalf("the fixture's audio is %s, want %s", got, c.codec)
			}
			spec := playback.Spec{AudioIndex: 1, AudioBitrate: 192_000, Copy: true, AudioCopy: c.codec}
			whole, layout, args := copyBothSegments(t, input, source, spec, 0)
			count := layout.Count()
			if count < 10 {
				t.Fatalf("%d segments, want enough to restart at 1, 4, 8 and the last", count)
			}

			// The film is opened once: the audio is the source's own packets and
			// needs neither a second input nor a grid of its own.
			if inputs := strings.Count(" "+strings.Join(args, " "), " -i "); inputs != 1 {
				t.Errorf("%d inputs, want 1: %v", inputs, args)
			}
			if !slices.Contains(args, "0:"+fmt.Sprint(spec.AudioIndex)) || slices.Contains(args, "1:"+fmt.Sprint(spec.AudioIndex)) {
				t.Errorf("the audio is not mapped from the one input: %v", args)
			}

			// Neither stream goes through an encoder.
			if i := slices.Index(args, "-c:a"); i < 0 || args[i+1] != "copy" {
				t.Errorf("the audio is not copied: %v", args)
			}
			if i := slices.Index(args, "-c:v"); i < 0 || args[i+1] != "copy" {
				t.Errorf("the video is not copied: %v", args)
			}

			init := filepath.Join(whole, "init.mp4")
			full := join(t, init, segmentFiles(whole, 0, count)...)
			if got := audioCodecOf(t, full); got != c.codec {
				t.Errorf("the audio of the stream is %s, want the source's %s", got, c.codec)
			}
			// The picture is the file's own, byte for byte.
			if a, b := videoMD5(t, input), videoMD5(t, full); a != b {
				t.Errorf("the video is not the source's: %s against %s", a, b)
			}
			// The sound is a run of the source's own packets, in order and unbroken
			// (all but the last few: the video ends a frame or so before the audio).
			sourcePackets := audioPackets(t, input, false)
			got := audioPackets(t, full, false)
			at := slices.Index(sourcePackets, got[0])
			if at < 0 || at+len(got) > len(sourcePackets) || !slices.Equal(got, sourcePackets[at:at+len(got)]) {
				t.Fatalf("the copied audio is not a run of the source's packets (starts at source packet %d, %d packets)", at, len(got))
			}
			if lost := len(sourcePackets) - len(got); at > 1 || lost > 3 {
				t.Errorf("the copy starts at source packet %d and has %d of %d", at, len(got), len(sourcePackets))
			}

			// Matroska keeps milliseconds, so a 23.976 fps film has decode times that
			// repeat; that is the source's, and a splice must add no warning to it.
			warnings := func(file string) string {
				out := run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", file, "-f", "null", "-")
				return regexp.MustCompile(` @ 0x[0-9a-f]+`).ReplaceAllString(out, "")
			}
			baseline := warnings(full)
			for _, restart := range []int{1, 4, 8, count - 1} {
				resumed, _, _ := copyBothSegments(t, input, source, spec, restart)
				if !bytes.Equal(readFile(t, init), readFile(t, filepath.Join(resumed, "init.mp4"))) {
					t.Errorf("restart %d: init segments differ", restart)
				}
				for n := restart; n < count; n++ {
					name := fmt.Sprintf("%d.m4s", n)
					a, b := filepath.Join(whole, name), filepath.Join(resumed, name)
					// The same packets fall in the same segments.
					if want, got := audioPackets(t, join(t, init, a), false), audioPackets(t, join(t, init, b), false); !slices.Equal(want, got) {
						t.Errorf("restart %d: segment %d has %d audio packets, the run from zero's has %d (or they differ)", restart, n, len(got), len(want))
					}
					if want, got := videoMD5(t, join(t, init, a)), videoMD5(t, join(t, init, b)); want != got {
						t.Errorf("restart %d: segment %d has other video than the run from zero's", restart, n)
					}
					// From an MP4 nothing differs at all, the audio times included.
					if c.drift == 0 && !bytes.Equal(readFile(t, a), readFile(t, b)) {
						t.Errorf("restart %d: segment %d differs in bytes from the run from zero's", restart, n)
					}
				}
				spliced := join(t, init, append(segmentFiles(whole, 0, restart), segmentFiles(resumed, restart, count)...)...)
				compare(t, fmt.Sprintf("restart %d video times", restart), times(t, full, "v:0", "packet=pts_time"), times(t, spliced, "v:0", "packet=pts_time"), 0)
				compare(t, fmt.Sprintf("restart %d audio times", restart), times(t, full, "a:0", "packet=pts_time"), times(t, spliced, "a:0", "packet=pts_time"), c.drift)
				if got := warnings(spliced); got != baseline {
					t.Errorf("restart %d: decoder warnings differ from a run from the start:\n%s\nagainst\n%s", restart, got, baseline)
				}
			}
		})
	}
}

// A job that copies both streams, started from any segment in any order, by
// any worker, stores segments whose video and audio are the source's own
// packets and whose init segment is the same whichever job made it: the viewer
// who seeks, and the one whose worker died, get one stream. (The rig's film is
// 90 s of H.264 and AAC in Matroska with a keyframe every 10 s.)
func TestCopiedBothStreamsServeFromAnySegmentAsTheSourcesPackets(t *testing.T) {
	r := newRig(t, nil)
	r.copied(t, "film.mkv")
	r.spec.AudioCopy, r.spec.AudioBitrate = "aac", 128_000
	r.hash = r.spec.Hash()
	r.worker(t, nil)
	j := r.jobs(t)
	var inits [][]byte
	for _, n := range []int{5, 0, 8} {
		body, err := r.fetch(t, j, psid(n), n)
		if err != nil || !isMP4Segment(body) {
			t.Fatalf("segment %d: %d bytes, %v", n, len(body), err)
		}
		init, err := r.fetchAt(t, j, psid(n), -1, n)
		if err != nil {
			t.Fatal(err)
		}
		inits = append(inits, init)
	}
	for _, init := range inits[1:] {
		if !bytes.Equal(init, inits[0]) {
			t.Error("init segments differ between jobs")
		}
	}
	waitFor(t, "the jobs to end", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
	if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
		t.Fatalf("%d jobs failed", failed)
	}
	dir := t.TempDir()
	var files []string
	for n := range 9 {
		body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, n), 0)
		if err != nil {
			t.Fatalf("segment %d was not stored: %v", n, err)
		}
		data, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, filepath.Join(dir, fmt.Sprintf("%d.m4s", n)))
		if err := os.WriteFile(files[n], data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	initFile := filepath.Join(dir, "init.mp4")
	if err := os.WriteFile(initFile, inits[0], 0o644); err != nil {
		t.Fatal(err)
	}
	all := join(t, initFile, files...)
	source := filepath.Join(film(t), "film.mkv")
	if a, b := videoMD5(t, source), videoMD5(t, all); a != b {
		t.Errorf("the stored video is not the source's: %s against %s", b, a)
	}
	sourcePackets := audioPackets(t, source, false)
	got := audioPackets(t, all, false)
	at := slices.Index(sourcePackets, got[0])
	if at < 0 || at+len(got) > len(sourcePackets) || !slices.Equal(got, sourcePackets[at:at+len(got)]) {
		t.Fatalf("the stored audio is not a run of the source's packets (%d packets, starting at source packet %d)", len(got), at)
	}
}

// countingStore counts the bytes read from its objects.
type countingStore struct {
	store.Store
	read atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (c *countingStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := c.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	return countingBody{body, &c.read}, nil
}

// A job that copies both streams from S3 reads the film once: the worker's
// loopback server for the object serves one stream of its bytes, where two
// inputs doubled it (the film's size twice per full watch, DOMAINS 48). Real
// ffmpeg, a whole job from segment 0.
func TestACopyOfBothStreamsFromS3ReadsTheFilmOnce(t *testing.T) {
	media, segments := s3Segments(t), s3Segments(t)
	for name := range segments {
		t.Run(name, func(t *testing.T) {
			whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if err := media[name].Put(context.Background(), "film.mkv", bytes.NewReader(whole)); err != nil {
				t.Fatal(err)
			}
			counted := &countingStore{Store: media[name]}
			r := newRig(t, segments[name])
			r.copied(t, "film.mkv")
			r.spec.AudioCopy, r.spec.AudioBitrate = "aac", 128_000
			r.hash = r.spec.Hash()
			r.worker(t, func(w *Worker) {
				w.Media = func(string) (store.Store, error) { return counted, nil }
			})
			j := r.jobs(t)
			for n := range 9 {
				if body, err := r.fetch(t, j, psid(0), n); err != nil || !isMP4Segment(body) {
					t.Fatalf("segment %d: %d bytes, %v", n, len(body), err)
				}
			}
			waitFor(t, "the job to end", 30*time.Second, func() bool {
				return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
			})
			// A little over the film's size is the container's index and a probe
			// of its head; two reads of it are about twice.
			size := int64(len(whole))
			if got := counted.read.Load(); got < size*9/10 || got > size*5/4 {
				t.Errorf("%d bytes read of a %d byte film (%.2f times), want one read of it", got, size, float64(got)/float64(size))
			}
		})
	}
}
