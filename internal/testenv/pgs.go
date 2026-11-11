package testenv

import (
	"bytes"
	"encoding/binary"
	"math"
)

// Cue is a solid yellow rectangle on a PGS canvas, from Start to End seconds.
// A viewer sees where and when it is drawn, which is all the fixtures need.
type Cue struct {
	Start, End float64
	X, Y, W, H int
}

// PGS writes cues as a .sup file, the stream a Blu-ray's PGS track is. ffmpeg
// can read the format but not write it, so the fixtures with a PGS track are
// made from this. A first display set at 0 keeps ffmpeg from moving the whole
// track earlier by the time of its first cue.
func PGS(canvasW, canvasH int, cues ...Cue) []byte {
	var out bytes.Buffer
	be := func(v ...any) []byte {
		var b bytes.Buffer
		for _, x := range v {
			binary.Write(&b, binary.BigEndian, x) //nolint:errcheck // A buffer doesn't fail.
		}
		return b.Bytes()
	}
	segment := func(at float64, kind byte, payload []byte) {
		out.WriteString("PG")
		out.Write(be(uint32(math.Round(at*90000)), uint32(0), kind, uint16(len(payload))))
		out.Write(payload)
	}
	// The composition: the canvas, 0x80 for the start of an epoch, and the
	// objects on show.
	composition := func(objects ...[]byte) []byte {
		return append(be(uint16(canvasW), uint16(canvasH), byte(0x10), uint16(0), byte(0x80), byte(0), byte(0), byte(len(objects))), bytes.Join(objects, nil)...)
	}
	blank := func(at float64) {
		segment(at, 0x16, composition())
		segment(at, 0x17, []byte{0})
		segment(at, 0x80, nil)
	}
	blank(0)
	for _, cue := range cues {
		segment(cue.Start, 0x16, composition(be(uint16(0), byte(0), byte(0), uint16(cue.X), uint16(cue.Y))))
		segment(cue.Start, 0x17, be(byte(1), byte(0), uint16(cue.X), uint16(cue.Y), uint16(cue.W), uint16(cue.H)))
		// Palette entry 1: yellow as Y, Cr, Cb and alpha.
		segment(cue.Start, 0x14, []byte{0, 0, 1, 219, 138, 16, 255})
		// Each row is a run of colour 1 as wide as the cue, then the end of the line.
		row := []byte{0, 0xC0 | byte(cue.W>>8), byte(cue.W), 1, 0, 0}
		image := bytes.Repeat(row, cue.H)
		size := len(image) + 4
		segment(cue.Start, 0x15, append(be(uint16(0), byte(0), byte(0xC0), byte(size>>16), byte(size>>8), byte(size), uint16(cue.W), uint16(cue.H)), image...))
		segment(cue.Start, 0x80, nil)
		blank(cue.End)
	}
	return out.Bytes()
}

// PGSAdded writes one picture built by two display sets, as a Blu-ray authoring
// tool does when a line is added while the earlier one stays on screen: the
// first line is shown at first.Start (the start of an epoch), the second is
// added at second.Start (a "normal" display set that names the first line's
// object again and a new one), and both go at clearAt. A run that begins after
// second.Start needs both packets to draw the whole picture.
func PGSAdded(canvasW, canvasH int, first, second Cue, clearAt float64) []byte {
	var out bytes.Buffer
	be := func(v ...any) []byte {
		var b bytes.Buffer
		for _, x := range v {
			binary.Write(&b, binary.BigEndian, x) //nolint:errcheck // A buffer doesn't fail.
		}
		return b.Bytes()
	}
	segment := func(at float64, kind byte, payload []byte) {
		out.WriteString("PG")
		out.Write(be(uint32(math.Round(at*90000)), uint32(0), kind, uint16(len(payload))))
		out.Write(payload)
	}
	// state is 0x80 for the start of an epoch and 0 for a display set that
	// goes on from the one before.
	composition := func(state byte, objects ...[]byte) []byte {
		return append(be(uint16(canvasW), uint16(canvasH), byte(0x10), uint16(0), state, byte(0), byte(0), byte(len(objects))), bytes.Join(objects, nil)...)
	}
	object := func(id uint16, cue Cue) []byte {
		return be(id, byte(0), byte(0), uint16(cue.X), uint16(cue.Y))
	}
	picture := func(at float64, id uint16, cue Cue) {
		segment(at, 0x14, []byte{0, 0, 1, 219, 138, 16, 255})
		row := []byte{0, 0xC0 | byte(cue.W>>8), byte(cue.W), 1, 0, 0}
		image := bytes.Repeat(row, cue.H)
		size := len(image) + 4
		segment(at, 0x15, append(be(id, byte(0), byte(0xC0), byte(size>>16), byte(size>>8), byte(size), uint16(cue.W), uint16(cue.H)), image...))
	}
	window := func(at float64) {
		segment(at, 0x17, be(byte(1), byte(0), uint16(0), uint16(0), uint16(canvasW), uint16(canvasH)))
	}
	// A first display set at 0 keeps ffmpeg from moving the whole track earlier.
	segment(0, 0x16, composition(0x80))
	window(0)
	segment(0, 0x80, nil)

	segment(first.Start, 0x16, composition(0x80, object(0, first)))
	window(first.Start)
	picture(first.Start, 0, first)
	segment(first.Start, 0x80, nil)

	segment(second.Start, 0x16, composition(0x00, object(0, first), object(1, second)))
	window(second.Start)
	picture(second.Start, 1, second)
	segment(second.Start, 0x80, nil)

	segment(clearAt, 0x16, composition(0x80))
	window(clearAt)
	segment(clearAt, 0x80, nil)
	return out.Bytes()
}
