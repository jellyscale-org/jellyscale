package playback

// What a job costs a worker, in cores. The figures are the measured table in
// docs/transcoding.md; a copy remuxes and decodes nothing, so it costs a little
// and is bounded by a count of its own as well (transcode.copyBound).
const (
	CopyCost = 0.05
	// fourKFrom is the height above which a source costs its 4K figure: a
	// 1080 line film is coded as 1088 lines, and anything taller is decoded and
	// scaled at more than that.
	fourKFrom = 1088
)

// Cost is the cores the job takes from a worker, fixed by the spec alone, so
// every replica and worker reads the same number from a stored row. A re-encode
// costs what it does to its source, not to its output, which is at most
// MaxHeight lines; a spec stored before the source's height was recorded is
// priced by its output's.
func (s Spec) Cost() float64 {
	if s.Copy {
		return CopyCost
	}
	fourK := max(s.SourceHeight, s.Height) > fourKFrom
	switch {
	case s.ToneMap != "" && fourK:
		return 3.2
	case s.ToneMap != "":
		return 2.0
	case fourK:
		return 1.6
	}
	return 0.8
}
