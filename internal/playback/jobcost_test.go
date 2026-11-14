package playback

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"jellyscale/internal/media"
)

// JS-087: what a job costs a worker, in cores, is fixed by its spec alone, so
// every replica and worker reads the same number from the row. These tests use
// one name the task implies and the build must add: Spec.Cost() float64. The
// source's own height is not in a re-encode's Spec today (Height is the
// output's, at most 1080), so the build records it in the Spec; these tests
// make specs through NewSpec and so do not name the field. The figures are the
// measured table in docs/transcoding.md.

const costItem = "11111111-2222-3333-4444-555555555555"

// costSource is a film of the given height, 16:9, HDR10 (PQ) when hdr.
func costSource(height int, hdr bool) Source {
	source := copyable("mkv", "ac3")
	video := &source.Streams[0]
	video.Codec, video.Profile, video.Level, video.Height, video.Width = "hevc", "Main 10", 153, height, height*16/9
	if hdr {
		video.VideoRangeType = "HDR10"
		video.ColorTransfer = media.Ptr(PQ)
	}
	return source
}

// costOf makes the spec of a request that accepts the audio codecs listed.
func costOf(t *testing.T, source Source, copied bool, audio ...string) (Spec, float64) {
	t.Helper()
	spec, err := NewSpec(costItem, Params{SourceID: sourceID, AudioIndex: 1, VideoBitrate: 40_000_000, AudioBitrate: 192_000, Copy: copied,
		AudioCodecs: AudioCodecList(strings.Join(audio, ",")), MaxAudioChannels: 2}, source)
	if err != nil {
		t.Fatal(err)
	}
	return spec, spec.Cost()
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A re-encode costs what it does to the source it reads: the table's four
// figures, by the source's size and whether it is tone mapped.
func TestAReencodesCostFollowsItsSource(t *testing.T) {
	for _, c := range []struct {
		name   string
		height int
		hdr    bool
		want   float64
	}{
		{"1080p SDR", 1080, false, 0.8},
		{"4K SDR", 2160, false, 1.6},
		{"1080p tone mapped", 1080, true, 2.0},
		{"4K tone mapped", 2160, true, 3.2},
		{"720p SDR is no cheaper than the 1080p figure", 720, false, 0.8},
		{"720p tone mapped is no cheaper than the 1080p figure", 720, true, 2.0},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, got := costOf(t, costSource(c.height, c.hdr), false)
			if spec.Copy || (spec.ToneMap != "") != c.hdr {
				t.Fatalf("the spec is copy %v, tone map %q", spec.Copy, spec.ToneMap)
			}
			if !near(got, c.want) {
				t.Errorf("costs %v cores, the table says %v", got, c.want)
			}
		})
	}
}

// The output of a 4K source is at most 1080 lines, and the cost is still the 4K
// figure: the decode and the scale are of the source. A source taller than
// 1080 lines never costs less than the 1080p figure.
func TestAFourKSourceCostsItsFigureThoughItsOutputIsAtMost1080(t *testing.T) {
	spec, got := costOf(t, costSource(2160, false), false)
	if spec.Height > MaxHeight {
		t.Fatalf("the output is %d lines", spec.Height)
	}
	if !near(got, 1.6) {
		t.Errorf("a 4K source with a %d line output costs %v, not 1.6", spec.Height, got)
	}
	for _, height := range []int{1088, 1440, 1600, 2160, 4320} {
		for _, hdr := range []bool{false, true} {
			_, got := costOf(t, costSource(height, hdr), false)
			if floor := map[bool]float64{false: 0.8, true: 2.0}[hdr]; got < floor-1e-9 {
				t.Errorf("a %d line source (HDR %v) costs %v, below the 1080p figure %v", height, hdr, got, floor)
			}
		}
	}
	// The 4K figures stay ordered above the 1080p ones.
	_, sdr1080 := costOf(t, costSource(1080, false), false)
	_, hdr4k := costOf(t, costSource(2160, true), false)
	if !(sdr1080 < 1.6 && 1.6 < hdr4k) {
		t.Errorf("1080p SDR %v, 4K SDR 1.6, 4K tone mapped %v are not in order", sdr1080, hdr4k)
	}
}

// A copy costs almost nothing, at one figure whatever its source and whether its
// audio is copied or re-encoded (a copy of a 4K HDR film included: nothing is
// decoded). The figure is not zero, so a worker's copies are counted, and the
// docs state it.
func TestACopyCostsAlmostNothingWhateverItsAudioOrSize(t *testing.T) {
	var figure float64
	for _, c := range []struct {
		name        string
		source      Source
		audio       []string // What the client plays.
		audioCopied bool
	}{
		{"audio re-encoded", copyable("mkv", "ac3"), []string{"aac"}, false},
		{"audio copied", copyable("mkv", "ac3"), []string{"ac3", "aac"}, true},
		{"4K HDR", costSource(2160, true), []string{"aac"}, false},
	} {
		spec, got := costOf(t, c.source, true, c.audio...)
		if !spec.Copy || (spec.AudioCopy != "") != c.audioCopied {
			t.Fatalf("%s: copy %v, audio copy %q", c.name, spec.Copy, spec.AudioCopy)
		}
		if got <= 0 || got > 0.1 {
			t.Errorf("%s: a copy costs %v cores, want above 0 and at most 0.1", c.name, got)
		}
		if figure != 0 && !near(figure, got) {
			t.Errorf("%s: a copy costs %v, another copy %v", c.name, got, figure)
		}
		figure = got
	}
}

// The cost is the same number on every replica and worker: it is read from the
// spec as stored in the job's row, which is JSON.
func TestACostSurvivesTheSpecBeingStored(t *testing.T) {
	for _, c := range []struct {
		height int
		hdr    bool
		copied bool
	}{{1080, false, false}, {2160, false, false}, {1080, true, false}, {2160, true, false}, {2160, true, true}} {
		source := costSource(c.height, c.hdr)
		if c.copied {
			source = copyable("mkv", "ac3")
		}
		spec, want := costOf(t, source, c.copied)
		raw, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var back Spec
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if got := back.Cost(); got != want {
			t.Errorf("height %d HDR %v copy %v: %v before it was stored, %v after: %s", c.height, c.hdr, c.copied, want, got, raw)
		}
	}
}

// A job stored by the release before has no record of its source's size. It
// still costs something, never nothing, so a worker beside the older replicas
// does not take it as free.
func TestASpecStoredBeforeTheCostWasKnownCostsAboveZero(t *testing.T) {
	for name, raw := range map[string]string{
		"a re-encode":  `{"Item":"a","Source":"b","ETag":"e","AudioIndex":1,"VideoBitrate":1500000,"AudioBitrate":128000,"Width":1280,"Height":720}`,
		"tone mapped":  `{"Item":"a","Source":"b","ETag":"e","AudioIndex":1,"VideoBitrate":1500000,"AudioBitrate":128000,"Width":1920,"Height":1080,"ToneMap":"smpte2084"}`,
		"a copy":       `{"Item":"a","Source":"b","ETag":"e","AudioIndex":1,"Width":1920,"Height":1080,"Copy":true}`,
		"an empty one": `{}`,
	} {
		var spec Spec
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			t.Fatal(err)
		}
		if got := spec.Cost(); got <= 0 {
			t.Errorf("%s costs %v cores", name, got)
		}
	}
	var tone Spec
	if err := json.Unmarshal([]byte(`{"Width":1920,"Height":1080,"ToneMap":"smpte2084"}`), &tone); err != nil {
		t.Fatal(err)
	}
	if got := tone.Cost(); got < 2.0-1e-9 {
		t.Errorf("a tone mapped spec with no recorded source costs %v, below the 1080p tone mapped figure 2.0", got)
	}
}
