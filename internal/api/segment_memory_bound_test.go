package api

// JS-077. What the stream role holds for each viewer who is mid-segment.
// TestStreamMemoryDoesNotGrowWithViewersOrSegments (integration tier) fails above this bound.
const (
	memoryViewers          = 16
	memorySegmentSize      = 16 << 20 // A copied segment of a 4K film: tens of MB.
	segmentMemoryPerViewer = 1 << 20  // At most a sixteenth of the segment, and the same for a segment of any size.
)
