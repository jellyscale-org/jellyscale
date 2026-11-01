package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"jellyscale/internal/playback"
)

const (
	ticksPerSecond = 10_000_000
	// Times from ffmpeg and from the file's index agree to a millisecond;
	// keyframes are at least a frame apart.
	tolerance = 50_000 // 5 ms in ticks
	// The last fragment ends a little before the video does, 160 ms or so in
	// the files tried; ffmpeg exiting 1.5 s short of the end means the input ended.
	// Only the check that the last segment reached the end uses it.
	endSlack = 1.5 * ticksPerSecond
)

// errWrongIndex means a segment doesn't start or end on a keyframe of the file:
// the index the layout was cut from isn't what the file has.
var errWrongIndex = errors.New("the keyframe index is wrong")

// piece is one fragment ffmpeg wrote: the video from a keyframe to the next,
// with the audio that fell in it.
type piece struct {
	file  string
	start int64 // From the stream's start, in ticks.
	end   int64
}

// gatherer joins a copied run's fragments into the playlist's segments. A
// segment starts and ends on keyframes of the index, and so on fragment
// boundaries, wherever the run began, because the index lists keyframes the
// file really has.
type gatherer struct {
	dir    string
	layout playback.Layout
	// first is the source's first keyframe, which the layout counts from.
	first int64
	// begins is when the run's first fragment starts, or -1 until it is read.
	begins int64
}

func newGatherer(dir string, layout playback.Layout, first int64) *gatherer {
	return &gatherer{dir: dir, layout: layout, first: first, begins: -1}
}

// segment writes segment n as N.m4s if the fragments that make it are done and
// reports whether it did. Once ffmpeg has exited, final is true and what is
// missing never will come.
func (g *gatherer) segment(ctx context.Context, n int, final bool) (bool, error) {
	pieces, err := g.pieces(ctx)
	if err != nil || len(pieces) == 0 {
		return false, err
	}
	start, length := g.layout.Segment(n)
	end := start + length
	last := n == g.layout.Count()-1
	var mine []piece
	for _, p := range pieces {
		if p.start >= start-tolerance && p.start < end-tolerance {
			mine = append(mine, p)
		}
	}
	reached := pieces[len(pieces)-1].end
	switch {
	case last && !final:
		return false, nil // The last fragment is written as the file ends.
	case !last && reached < end-tolerance:
		if final {
			return false, fmt.Errorf("the input ended early: ffmpeg stopped after segment %d of %d", n-1, g.layout.Count())
		}
		return false, nil
	case last && len(mine) == 0 && reached <= start+tolerance:
		// A cut input that stops at the last keyframe has no fragment in the
		// last segment. Fragments are cut at every real keyframe, so with a
		// good index none runs past the last segment's start: output that
		// stopped there says the input ended early, and the index stays. Output
		// that ran past it means the last entry is false (the case below).
		return false, fmt.Errorf("the input ended early: ffmpeg stopped at %.2fs, the last segment starts at %.2fs",
			float64(reached)/ticksPerSecond, float64(start)/ticksPerSecond)
	case len(mine) == 0 || math.Abs(float64(mine[0].start-start)) > tolerance:
		return false, fmt.Errorf("segment %d does not start on a keyframe of the file: %w", n, errWrongIndex)
	case !last && math.Abs(float64(mine[len(mine)-1].end-end)) > tolerance:
		return false, fmt.Errorf("segment %d does not end on a keyframe of the file: %w", n, errWrongIndex)
	case last && math.Abs(float64(reached-end)) > endSlack:
		// As checkLast does: ffmpeg exits cleanly when its input is cut short.
		return false, fmt.Errorf("the input ended early: the last segment is %.2fs, not %.2fs",
			float64(reached-start)/ticksPerSecond, float64(length)/ticksPerSecond)
	}
	if err := g.join(n, mine); err != nil {
		return false, err
	}
	for _, p := range pieces {
		if p.start < end-tolerance {
			os.Remove(p.file)
		}
	}
	return true, nil
}

// pieces lists the finished fragments, which the playlist ffmpeg writes keeps
// in order, with the time each starts at.
func (g *gatherer) pieces(ctx context.Context) ([]piece, error) {
	text, err := os.ReadFile(filepath.Join(g.dir, "index.m3u8"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pieces []piece
	var length int64
	for _, line := range strings.Split(string(text), "\n") {
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			seconds, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			length = int64(math.Round(seconds * ticksPerSecond))
		case strings.HasSuffix(line, ".m4s") && length > 0:
			pieces = append(pieces, piece{file: filepath.Join(g.dir, line), end: length}) // Its length, until the start is known.
			length = 0
		}
	}
	if len(pieces) == 0 {
		return nil, nil
	}
	if g.begins < 0 {
		if g.begins, err = g.firstKeyframe(ctx, pieces[0].file); err != nil {
			return nil, err
		}
	}
	at := g.begins
	for i := range pieces {
		pieces[i].start, pieces[i].end = at, at+pieces[i].end
		at = pieces[i].end
	}
	return pieces, nil
}

// firstKeyframe is the time of a fragment's first keyframe, from the stream's
// start. The seek for a run lands on a keyframe at or before the one asked for,
// and the fragments carry only the time ffmpeg wrote, so it has to be read.
func (g *gatherer) firstKeyframe(ctx context.Context, file string) (int64, error) {
	init, err := os.ReadFile(filepath.Join(g.dir, "init.mp4"))
	if err != nil {
		return 0, err
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	out, err := ffprobeFrom(ctx, bytes.NewReader(append(init, body...)), "-select_streams", "v:0",
		"-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", "-")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Fields(string(out)) {
		if pts, flags, _ := strings.Cut(line, ","); strings.HasPrefix(flags, "K") {
			seconds, err := strconv.ParseFloat(pts, 64)
			if err != nil {
				return 0, err
			}
			return int64(math.Round((seconds-tsOffset)*ticksPerSecond)) - g.first, nil
		}
	}
	return 0, errors.New("the first fragment has no keyframe")
}

// join writes the fragments as one segment: the first whole, and from the rest
// the moof and mdat, which follow on from it. Each moof's sequence number is
// its fragment's time in milliseconds, not its place in the run, so a run that
// began elsewhere writes the same bytes.
func (g *gatherer) join(n int, pieces []piece) error {
	name := filepath.Join(g.dir, strconv.Itoa(n)+".m4s")
	out, err := os.Create(name + ".tmp")
	if err != nil {
		return err
	}
	defer out.Close()
	for i, p := range pieces {
		data, err := os.ReadFile(p.file)
		if err != nil {
			return err
		}
		for len(data) >= 8 {
			size := int(binary.BigEndian.Uint32(data))
			if size < 8 || size > len(data) {
				return fmt.Errorf("%s is not a fragment", filepath.Base(p.file))
			}
			kind := string(data[4:8])
			if kind == "moof" && size >= 24 { // moof, mfhd, then the sequence number.
				binary.BigEndian.PutUint32(data[20:], uint32(p.start/10_000)+1)
			}
			if kind != "styp" || i == 0 {
				if _, err := out.Write(data[:size]); err != nil {
					return err
				}
			}
			data = data[size:]
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(name+".tmp", name)
}

// copyStart says where the run for segment n begins in the source's clock, in
// seconds: the video at the keyframe before the segment's own, and the audio
// two seconds before it, more than a keyframe's decode time can lead its
// presentation by. Negative is the start of the file.
func copyStart(keyframes []int64, layout playback.Layout, n int) (video, audio float64) {
	if n == 0 {
		return -1, -1
	}
	at := keyframes[0] + layout.Starts[n]
	i, _ := slices.BinarySearch(keyframes, at)
	return float64(keyframes[max(i-1, 0)]) / ticksPerSecond, float64(at)/ticksPerSecond - 2
}
