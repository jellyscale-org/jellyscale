package playback

import "slices"

// MaxCopySegment is the longest segment a copied stream may have. A segment
// can't be shorter than the gap between two keyframes, and a client must
// download a whole segment before it shows any of it, so a long one delays a
// seek: at 40 Mbps, 12 seconds is 60 MB. It still lets through x264's and
// x265's default of a keyframe every 250 frames at 21 fps and up. A file with
// a longer gap is re-encoded instead.
const MaxCopySegment = 12 * ticksPerSecond

// Layout says where a stream's segments start and how long they are. It is
// known before anything is encoded, and the playlist is built from it.
type Layout struct {
	// Starts is where each segment starts, in ticks from the stream's start.
	Starts []int64
	// End is where the last segment ends.
	End int64
}

// Count is the number of segments.
func (l Layout) Count() int { return len(l.Starts) }

// Segment returns where segment n starts and how long it is.
func (l Layout) Segment(n int) (start, length int64) {
	end := l.End
	if n+1 < len(l.Starts) {
		end = l.Starts[n+1]
	}
	return l.Starts[n], end - l.Starts[n]
}

// At is the segment that holds the given time, or the last one if the time is
// past the end.
func (l Layout) At(ticks int64) int {
	i, found := slices.BinarySearch(l.Starts, ticks)
	if !found {
		i--
	}
	return max(i, 0)
}

// longest is the length of the longest segment.
func (l Layout) longest() int64 {
	var longest int64
	for n := range l.Starts {
		_, length := l.Segment(n)
		longest = max(longest, length)
	}
	return longest
}

// EncodedLayout is the layout of a re-encoded stream: a fixed grid of
// SegmentSeconds. A tail under a second joins the segment before it, because
// the encoder may not cut one.
func EncodedLayout(runTimeTicks int64) Layout {
	seconds := float64(runTimeTicks) / ticksPerSecond
	count := int(seconds / SegmentSeconds)
	if count == 0 || seconds-float64(count*SegmentSeconds) >= 1 {
		count++
	}
	starts := make([]int64, count)
	for n := range starts {
		starts[n] = int64(n) * SegmentSeconds * ticksPerSecond
	}
	return Layout{Starts: starts, End: runTimeTicks}
}

// CopyLayout is the layout of a copied stream, cut on the source's keyframes
// (times in ticks, ascending). A segment ends at the first keyframe at least
// SegmentSeconds after it started, or earlier, at the last keyframe that keeps
// it within MaxCopySegment. Times count from the first keyframe, and length is
// how long the video lasts from its start. It reports false when the keyframes
// can't make a playlist: there are none, or a segment can't be kept within
// MaxCopySegment because two keyframes, or the last keyframe and the end, are
// further apart than that.
func CopyLayout(keyframes []int64, length int64) (Layout, bool) {
	if len(keyframes) == 0 || length <= 0 {
		return Layout{}, false
	}
	layout := Layout{Starts: []int64{0}, End: length}
	var previous int64
	for _, k := range keyframes[1:] {
		k -= keyframes[0]
		if k >= length {
			break
		}
		start := layout.Starts[len(layout.Starts)-1]
		if k-start > MaxCopySegment && previous > start {
			layout.Starts = append(layout.Starts, previous)
			start = previous
		}
		if k-start >= SegmentSeconds*ticksPerSecond {
			layout.Starts = append(layout.Starts, k)
		}
		previous = k
	}
	if last := layout.Starts[len(layout.Starts)-1]; length-last > MaxCopySegment && previous > last {
		layout.Starts = append(layout.Starts, previous)
	}
	return layout, layout.longest() <= MaxCopySegment
}
