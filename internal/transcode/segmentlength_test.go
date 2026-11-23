package transcode

import (
	"encoding/binary"
	"testing"
)

func box(kind string, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(out, kind...), body...)
}

func u32s(values ...uint32) []byte {
	var out []byte
	for _, v := range values {
		out = binary.BigEndian.AppendUint32(out, v)
	}
	return out
}

func TestSegmentTicksSumsTheSampleDurationsOfEveryFragment(t *testing.T) {
	// Version 0 mdhd: flags, creation, modification, timescale, duration, language.
	init := box("moov", box("trak", box("mdia", box("mdhd", u32s(0, 0, 0, 1000, 0, 0)))))
	// A trun with a duration and a size per sample (flags 0x300), and one with the durations in tfhd (0x8).
	perSample := box("moof", box("traf", box("tfhd", u32s(0x20000, 1)), box("trun", u32s(0x300, 2, 40, 5, 60, 5))))
	byDefault := box("moof", box("traf", box("tfhd", u32s(0x20008, 1, 40)), box("trun", u32s(0, 3))))
	segment := append(append(append(perSample, box("mdat", []byte{1, 2, 3})...), byDefault...), box("mdat")...)

	got, err := segmentTicks(init, segment)
	if err != nil {
		t.Fatal(err)
	}
	// 40+60+3*40 = 220 ms.
	if want := int64(220 * ticksPerSecond / 1000); got != want {
		t.Fatalf("got %d ticks, want %d", got, want)
	}
	if got, _ := segmentTicks(init, perSample[:20]); got != 0 {
		t.Fatalf("a fragment cut short measured %d ticks, want none", got)
	}
}
