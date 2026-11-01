package library

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/store"
)

// capturedKeyframes are the irregular keyframes of the captured Jellyfin
// fixture (compat/captures/transcode-12.4.md): a fixed grid would hide any
// mistake in telling them apart.
const capturedKeyframes = "0,3,7,13,18,26,31,39,44,52,57,65,70,78,83"

// makeFilm writes a 90 s film with B-frames, irregular keyframes and AC3 audio
// as the first or second track, in the container the name ends in.
func makeFilm(t *testing.T, name string, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	file := filepath.Join(t.TempDir(), name)
	args := append([]string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=90",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90",
		"-c:v", "libx264", "-preset", "veryfast", "-x264-params", "bframes=2:scenecut=0", "-force_key_frames", capturedKeyframes,
		"-c:a", "ac3"}, extra...)
	if out, err := exec.Command("ffmpeg", append(args, file)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	return file
}

// packetKeyframes lists the keyframes the slow way, by reading every packet.
func packetKeyframes(t *testing.T, file string) []int64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	var keyframes []int64
	for _, line := range strings.Fields(string(out)) {
		if pts, flags, _ := strings.Cut(line, ","); strings.HasPrefix(flags, "K") {
			seconds, _ := strconv.ParseFloat(pts, 64)
			keyframes = append(keyframes, int64(math.Round(seconds*ticksPerSecond)))
		}
	}
	return keyframes
}

func readIndex(t *testing.T, file, container string) ([]int64, error) {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	return ReadKeyframes(context.Background(), store.NewPosix(filepath.Dir(file)), filepath.Base(file), info.Size(), container)
}

// The index must agree with the packets themselves, in both containers and
// however they were laid out: moov after mdat, edit lists, a video that is not
// the first track, Cues in front.
func TestKeyframesMatchPackets(t *testing.T) {
	for _, c := range []struct {
		name, container string
		extra           []string
	}{
		{"cues at the end.mkv", "mkv", nil},
		{"cues in front.mkv", "mkv", []string{"-cues_to_front", "1"}},
		{"video second.mkv", "mkv", []string{"-map", "1:a", "-map", "0:v"}},
		{"moov at the end.mp4", "mp4", nil},
		{"moov in front.mp4", "mp4", []string{"-movflags", "+faststart"}},
		{"no edit list.mp4", "mp4", []string{"-use_editlist", "0"}},
		{"video second.mp4", "mp4", []string{"-map", "1:a", "-map", "0:v"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			file := makeFilm(t, c.name, c.extra...)
			got, err := readIndex(t, file, c.container)
			if err != nil {
				t.Fatal(err)
			}
			want := packetKeyframes(t, file)
			if len(got) != strings.Count(capturedKeyframes, ",")+1 || len(got) != len(want) {
				t.Fatalf("%d keyframes, ffprobe finds %d: %v", len(got), len(want), got)
			}
			for i := range want {
				// Matroska keeps milliseconds; mp4 is exact to ffprobe's six digits.
				if math.Abs(float64(got[i]-want[i])) > 10_000 {
					t.Errorf("keyframe %d at %d ticks, ffprobe says %d", i, got[i], want[i])
				}
			}
		})
	}
}

// A file with no index to read is not an error to log: it is re-encoded.
func TestKeyframesNeedAnIndex(t *testing.T) {
	for name, extra := range map[string][]string{
		"no cues.mkv":    {"-live", "1"},
		"fragmented.mp4": {"-movflags", "+frag_keyframe+delay_moov"},
		"not a film.mkv": nil,
	} {
		file := makeFilm(t, name, extra...)
		if name == "not a film.mkv" {
			if err := os.WriteFile(file, []byte("this is not matroska at all, not even close, not at all"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		container := strings.TrimPrefix(filepath.Ext(file), ".")
		if _, err := readIndex(t, file, container); !errors.Is(err, ErrNoIndex) {
			t.Errorf("%s: %v, want ErrNoIndex", name, err)
		}
	}
}

// A Matroska file whose Cues are gone still has its SeekHead pointing to them,
// and its duration; it has no index to read.
func TestKeyframesNeedTheCuesToBeThere(t *testing.T) {
	file := makeFilm(t, "cut cues.mkv")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.LastIndex(data, []byte{0x1c, 0x53, 0xbb, 0x6b}) // ffmpeg writes the Cues last.
	if at < len(data)*9/10 {
		t.Fatalf("Cues at %d of %d", at, len(data))
	}
	if err := os.WriteFile(file, data[:at], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIndex(t, file, "mkv"); !errors.Is(err, ErrNoIndex) {
		t.Errorf("got %v, want ErrNoIndex", err)
	}
}

// A cue time that would overflow when scaled to nanoseconds is no index.
func TestKeyframesRefuseCueTimesThatOverflow(t *testing.T) {
	file := makeFilm(t, "overflow.mkv")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.LastIndex(data, []byte{0x1c, 0x53, 0xbb, 0x6b}) // ffmpeg writes the Cues last.
	if at < len(data)*9/10 {
		t.Fatalf("Cues at %d of %d", at, len(data))
	}
	// One cue point on the video track, at 2^62 timestamp steps.
	track := []byte{0xb7, 0x83, 0xf7, 0x81, 0x01}
	point := append([]byte{0xb3, 0x88, 0x40, 0, 0, 0, 0, 0, 0, 0}, track...)
	cues := append([]byte{0x1c, 0x53, 0xbb, 0x6b, 0x80 | byte(len(point)+2), 0xbb, 0x80 | byte(len(point))}, point...)
	if err := os.WriteFile(file, append(data[:at], cues...), 0o644); err != nil {
		t.Fatal(err)
	}
	if keyframes, err := readIndex(t, file, "mkv"); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("got %v and %v, want ErrNoIndex", keyframes, err)
	}
}

// countingStore adds up what is read from the objects it serves.
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
	return countingBody{body, &c.read}, err
}

// Building the index reads the header and the index and nothing of the
// picture: a small part of the file, which is the point of not using ffprobe.
func TestKeyframesReadLittle(t *testing.T) {
	for name, container := range map[string]string{"a.mkv": "mkv", "b.mp4": "mp4"} {
		file := makeFilm(t, name, "-b:v", "2M", "-minrate", "2M", "-maxrate", "2M", "-bufsize", "4M")
		info, _ := os.Stat(file)
		counter := &countingStore{Store: store.NewPosix(filepath.Dir(file))}
		if _, err := ReadKeyframes(context.Background(), counter, name, info.Size(), container); err != nil {
			t.Fatal(err)
		}
		if fraction := float64(counter.read.Load()) / float64(info.Size()); fraction > 0.02 {
			t.Errorf("%s: read %d of %d bytes (%.1f%%)", name, counter.read.Load(), info.Size(), fraction*100)
		} else {
			t.Logf("%s: read %d of %d bytes (%.2f%%)", name, counter.read.Load(), info.Size(), fraction*100)
		}
	}
}

// s3Stores returns a store on each S3 target test/integration.sh set up.
func s3Stores(t *testing.T) map[string]store.Store {
	t.Helper()
	stores := map[string]store.Store{}
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		name, uri, _ := strings.Cut(entry, "=")
		if uri == "" {
			continue
		}
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		t.Setenv("JELLYSCALE_S3_ACCESS_KEY", query.Get("key"))
		t.Setenv("JELLYSCALE_S3_SECRET_KEY", query.Get("secret"))
		query.Del("key")
		query.Del("secret")
		parsed.RawQuery = query.Encode()
		parsed.Path = "/keyframes-" + uuid.NewString()
		opened, err := store.NewS3(parsed)
		if err != nil {
			t.Fatal(err)
		}
		stores[name] = opened
	}
	return stores
}

// From a real object store the index costs what the file's header and index
// do, though the object is tens of megabytes of picture. The bytes counted are
// those the client reads; an open-ended GET that is closed early may have more
// in flight, which the figure in DOMAINS.md measures at the server.
func TestKeyframesFromS3ReadLittle(t *testing.T) {
	stores := s3Stores(t)
	if len(stores) == 0 {
		t.Skip("JELLYSCALE_TEST_S3 not set")
	}
	// Noise, so that the picture is big: a real film's is.
	file := makeFilm(t, "big.mkv", "-vf", "noise=alls=40:allf=t", "-b:v", "6M")
	moov := makeFilm(t, "big.mp4", "-vf", "noise=alls=40:allf=t", "-b:v", "6M")
	for name, s3 := range stores {
		for _, f := range []struct{ path, container string }{{file, "mkv"}, {moov, "mp4"}} {
			body, err := os.Open(f.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s3.Put(context.Background(), "film."+f.container, body); err != nil {
				t.Fatal(err)
			}
			body.Close()
			info, _ := os.Stat(f.path)
			counter := &countingStore{Store: s3}
			keyframes, err := ReadKeyframes(context.Background(), counter, "film."+f.container, info.Size(), f.container)
			if err != nil || len(keyframes) != 15 {
				t.Fatalf("%s %s: %d keyframes, %v", name, f.container, len(keyframes), err)
			}
			read := counter.read.Load()
			t.Logf("%s %s: read %d of %d bytes (%.2f%%)", name, f.container, read, info.Size(), float64(read)/float64(info.Size())*100)
			if read > info.Size()/20 {
				t.Errorf("%s %s: read %d of %d bytes", name, f.container, read, info.Size())
			}
		}
	}
}

// brokenStore serves the first read of every place beyond the start of the
// file, which is a header's, and fails the second, which is its body's, as a
// store does when the network drops while the index is read.
type brokenStore struct {
	store.Store
	opened map[int64]bool
}

func (b brokenStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if offset > 64<<10 {
		if b.opened[offset] {
			return nil, errors.New("connection reset")
		}
		b.opened[offset] = true
	}
	return b.Store.Open(ctx, key, offset)
}

// A read that fails says nothing of the file: it must come back as the read
// error, so the scan leaves the index unset and tries again, and not as a
// file with no index, which is stored and never looked at again.
func TestKeyframesReadErrorIsNotAMissingIndex(t *testing.T) {
	for _, name := range []string{"a.mkv", "b.mp4"} {
		file := makeFilm(t, name, "-b:v", "2M", "-minrate", "2M", "-maxrate", "2M", "-bufsize", "4M")
		info, _ := os.Stat(file)
		broken := brokenStore{store.NewPosix(filepath.Dir(file)), map[int64]bool{}}
		_, err := ReadKeyframes(context.Background(), broken, name, info.Size(), strings.TrimPrefix(filepath.Ext(name), "."))
		if err == nil || errors.Is(err, ErrNoIndex) {
			t.Errorf("%s: got %v, want the read error", name, err)
		}
	}
}

// patchMP4 changes bytes of the first video track's stts or stss table.
func patchMP4(t *testing.T, file string, change func(data []byte, stts, stss int)) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	stts, stss := bytes.Index(data, []byte("stts")), bytes.Index(data, []byte("stss"))
	if stts < 0 || stss < 0 {
		t.Fatal("no stts or stss")
	}
	change(data, stts, stss)
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Crafted tables must end quickly as a file with no index, not walk billions
// of samples that stts claims and the file does not have.
func TestKeyframesFromCraftedMP4TablesEndQuickly(t *testing.T) {
	for name, change := range map[string]func(data []byte, stts, stss int){
		"a huge sample count and sync samples past the real ones": func(data []byte, stts, stss int) {
			binary.BigEndian.PutUint32(data[stts+12:], 0xFFFFFFF0)
			count := int(binary.BigEndian.Uint32(data[stss+8:]))
			binary.BigEndian.PutUint32(data[stss+12+4*(count-1):], 0xFFFFFFF0)
		},
		"a huge sample count and sync samples out of order": func(data []byte, stts, stss int) {
			binary.BigEndian.PutUint32(data[stts+12:], 0xFFFFFFF0)
			first, second := data[stss+12:stss+16], data[stss+16:stss+20]
			a := append([]byte{}, first...)
			copy(first, second)
			copy(second, a)
		},
	} {
		t.Run(name, func(t *testing.T) {
			file := makeFilm(t, "crafted.mp4")
			patchMP4(t, file, change)
			done := make(chan error, 1)
			go func() {
				_, err := readIndex(t, file, "mp4")
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNoIndex) {
					t.Errorf("got %v, want ErrNoIndex", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("still walking the samples after 3 s")
			}
		})
	}
}

// Sample durations whose times would overflow when converted to ticks are no
// index, not garbage stored as keyframes.
func TestKeyframesRefuseTimesThatOverflowTicks(t *testing.T) {
	file := makeFilm(t, "overflow.mp4")
	patchMP4(t, file, func(data []byte, stts, stss int) {
		binary.BigEndian.PutUint32(data[stts+16:], 0xFFFFFFFF) // The first run's sample duration.
	})
	keyframes, err := readIndex(t, file, "mp4")
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("got %v and %v, want ErrNoIndex", keyframes, err)
	}
}

// One run of 0xFFFFFFF0 samples of 0xFFFFFFFF each: the time of the last
// sync sample, count*delta or partial*delta, wraps int64 unless it is checked.
func TestKeyframesRefuseSampleTimesThatWrapInt64(t *testing.T) {
	// A second run of stts is where the product of a first run's count and delta
	// is added to the time: it wraps there, though the delta alone is in range.
	// A gap in the timestamps gives the film the several runs that needs.
	for name, extra := range map[string][]string{
		"one run":  nil,
		"two runs": {"-vf", `setpts=if(gte(N\,100)\,PTS+200\,PTS)`, "-fps_mode", "passthrough"},
	} {
		t.Run(name, func(t *testing.T) {
			file := makeFilm(t, "wrap.mp4", extra...)
			patchMP4(t, file, func(data []byte, stts, stss int) {
				put := func(at int, v uint32) { binary.BigEndian.PutUint32(data[at:], v) }
				if extra == nil {
					put(stts+8, 1)
					put(stts+12, 0xFFFFFFF0)
					put(stts+16, 0xFFFFFFFF)
					put(stss+8, 2)
					put(stss+12, 1)
					put(stss+16, 0xFFFFFFF0)
				} else {
					put(stts+8, 2)
					put(stts+12, 0xFFFFFFF0)
					put(stts+16, 0xFFFFFFFF)
					put(stts+20, 1)
					put(stts+24, 1)
					put(stss+8, 2)
					put(stss+12, 1)
					put(stss+16, 0xFFFFFFF1) // The first sample of the second run.
				}
				put(bytes.Index(data, []byte("stsz"))+12, 0xFFFFFFFF)
			})
			keyframes, err := readIndex(t, file, "mp4")
			if !errors.Is(err, ErrNoIndex) {
				t.Fatalf("got %v and %v, want ErrNoIndex", keyframes, err)
			}
		})
	}
}

// The edit delay and a sample's time are each in range, and their sum is not:
// a timescale of 1 makes the time of sample 215 almost 2^63 ticks, and the
// longest empty edit ahead of it carries the sum over.
func TestKeyframesRefuseASumThatOverflowsTicks(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	file := filepath.Join(t.TempDir(), "sum.mp4")
	// The late start of the video makes an empty edit; the keyframe at sample 215.
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-itsoffset", "1", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=10",
		"-c:v", "libx264", "-x264-params", "bframes=0:scenecut=0", "-force_key_frames", "8.9166", file).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	patchMP4(t, file, func(data []byte, stts, stss int) {
		binary.BigEndian.PutUint32(data[stts+12:], 240) // One run: every sample...
		binary.BigEndian.PutUint32(data[stts+16:], 0xFFFFFFFF)
		for _, box := range []string{"mvhd", "mdhd"} {
			binary.BigEndian.PutUint32(data[bytes.Index(data, []byte(box))+16:], 1)
		}
		elst := bytes.Index(data, []byte("elst"))
		binary.BigEndian.PutUint32(data[elst+12:], 0xFFFFFFFF) // The empty edit's length.
	})
	keyframes, err := readIndex(t, file, "mp4")
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("got %v and %v, want ErrNoIndex", keyframes, err)
	}
}

// The video's end is where its last frame ends, not what the container's
// duration says: whichever stream starts late or ends early, in both
// containers.
func TestVideoEndIsWhereTheLastFrameEnds(t *testing.T) {
	for _, container := range []string{"mkv", "mp4"} {
		for name, input := range map[string][]string{
			"video late":      {"-itsoffset", "0.5", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=90", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90"},
			"audio late":      {"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=90", "-itsoffset", "0.5", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90"},
			"video shorter":   {"-itsoffset", "1.5", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=80", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90"},
			"everything late": {"-itsoffset", "0.5", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=90", "-itsoffset", "0.5", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90"},
		} {
			t.Run(container+", "+name, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "film."+container)
				args := append([]string{"-nostdin", "-v", "error", "-y"}, input...)
				args = append(args, "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "veryfast", "-x264-params", "bframes=2:scenecut=0",
					"-force_key_frames", capturedKeyframes, "-c:a", "ac3", file)
				if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg: %v: %s", err, out)
				}
				keyframes := packetKeyframes(t, file)
				got, err := ReadVideoEnd(context.Background(), file, keyframes[len(keyframes)-1])
				if err != nil {
					t.Fatal(err)
				}
				// Every packet, the slow way: the last frame shown plus one frame.
				out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", file).Output()
				if err != nil {
					t.Fatal(err)
				}
				var last float64
				for _, field := range strings.Fields(string(out)) {
					pts, _ := strconv.ParseFloat(field, 64)
					last = math.Max(last, pts)
				}
				want := int64((last + 1.0/24) * ticksPerSecond)
				if math.Abs(float64(got-want)) > ticksPerSecond/24 {
					t.Errorf("the video ends at %.3f s, want %.3f s", float64(got)/ticksPerSecond, float64(want)/ticksPerSecond)
				}
			})
		}
	}
}

// Finding where the video ends reads the start of the file and the last group
// of pictures, and no more: not the film. The bytes counted
// are those the client reads from the store, as in the test above.
func TestVideoEndFromS3ReadsLittle(t *testing.T) {
	stores := s3Stores(t)
	if len(stores) == 0 {
		t.Skip("JELLYSCALE_TEST_S3 not set")
	}
	files := map[string]string{
		"mkv": makeFilm(t, "big.mkv", "-vf", "noise=alls=40:allf=t", "-b:v", "6M"),
		"mp4": makeFilm(t, "big.mp4", "-vf", "noise=alls=40:allf=t", "-b:v", "6M"),
	}
	for name, s3 := range stores {
		for container, path := range files {
			body, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s3.Put(context.Background(), "film."+container, body); err != nil {
				t.Fatal(err)
			}
			body.Close()
			info, _ := os.Stat(path)
			keyframes := packetKeyframes(t, path)
			counter := &countingStore{Store: s3}
			input, closeInput, err := store.Input(context.Background(), counter, "film."+container, info.Size())
			if err != nil {
				t.Fatal(err)
			}
			end, err := ReadVideoEnd(context.Background(), input, keyframes[len(keyframes)-1])
			closeInput()
			if err != nil || end < 89*ticksPerSecond {
				t.Fatalf("%s %s: the video ends at %d, %v", name, container, end, err)
			}
			read := counter.read.Load()
			t.Logf("%s %s: read %d of %d bytes (%.1f%%); the last keyframe is %.0f s before the end", name, container, read, info.Size(),
				float64(read)/float64(info.Size())*100, 90-float64(keyframes[len(keyframes)-1])/ticksPerSecond)
			// ffprobe opens the start of the file (a few MB; the amount varies with its version) and asks twice
			// for the stretch from the last keyframe on. In a film hours long
			// that is a small fraction; in this 90 s one it is not.
			gop := info.Size() * (90*ticksPerSecond - keyframes[len(keyframes)-1]) / (90 * ticksPerSecond)
			if limit := 12<<20 + 2*gop; read > limit {
				t.Errorf("%s %s: read %d bytes, want at most %d", name, container, read, limit)
			}
		}
	}
}

// A film whose only keyframe is at 0 can't be cut into segments, so the scan
// must not read to its end to find where the video stops, which for it is the
// whole film, twice.
func TestScanSkipsTheVideoEndOfAFilmItCannotCut(t *testing.T) {
	for _, container := range []string{"mkv", "mp4"} {
		t.Run(container, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "long."+container)
			if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=24:duration=60",
				"-c:v", "libx264", "-preset", "ultrafast", "-g", "100000", "-keyint_min", "100000", "-sc_threshold", "0", file).CombinedOutput(); err != nil {
				t.Skipf("ffmpeg: %v: %s", err, out)
			}
			probe, err := ProbeInput(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(file)
			var log strings.Builder
			s := &Scanner{Log: slog.New(slog.NewTextHandler(&log, nil))}
			s.readIndex(context.Background(), Library{Media: store.NewPosix(dir)}, store.Object{Key: filepath.Base(file), Size: info.Size()}, file, &probe)
			if probe.Keyframes == nil || len(probe.Keyframes) != 0 || probe.VideoEnd != 0 {
				t.Fatalf("keyframes %v, video end %d: want an empty index and no read of the video's end", probe.Keyframes, probe.VideoEnd)
			}
			if !strings.Contains(log.String(), "more than 12 seconds") {
				t.Errorf("the log doesn't say why: %s", log.String())
			}
		})
	}
}

// An EBML header that claims to be longer than the start of the file that is
// read must be refused, not run off the end of it: the scan has no recover,
// and a panic there ends the worker at every scan.
func TestKeyframesRefuseAnEBMLHeaderLongerThanTheHead(t *testing.T) {
	file := makeFilm(t, "film.mkv")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	copy(data[4:8], []byte{0x10, 0x10, 0x00, 0x00}) // A size of 1 MiB.
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readIndex(t, file, "mkv"); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("got %v, want ErrNoIndex", err)
	}
}
