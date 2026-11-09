package testenv

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

// VobSub writes cues as the pair of files a DVD's subtitles are ripped to: an
// .idx text file of the canvas, the palette and where each cue is, and a .sub
// file of MPEG program stream sectors holding the cues' subtitle packets. A
// cue is a solid yellow rectangle, as in PGS; ffmpeg's encoder can write the
// packets but no muxer writes the pair, so fixtures with a VobSub file are
// made from this. The canvas is usually 720x480.
func VobSub(canvasW, canvasH int, cues ...Cue) (idx, sub []byte) {
	var index bytes.Buffer
	fmt.Fprintf(&index, "# VobSub index file, v7 (do not modify this line!)\nsize: %dx%d\npalette: 000000", canvasW, canvasH)
	// Entry 1 is yellow, as the PGS cues are; the rest are never used.
	index.WriteString(", ffeb00")
	for range 14 {
		index.WriteString(", 000000")
	}
	index.WriteString("\nlangidx: 0\n\nid: en, index: 0\n")
	var out bytes.Buffer
	for _, cue := range cues {
		ms := int(math.Round(cue.Start * 1000))
		fmt.Fprintf(&index, "timestamp: %02d:%02d:%02d:%03d, filepos: %09x\n", ms/3600000, ms/60000%60, ms/1000%60, ms%1000, out.Len())
		out.Write(vobSector(cue.Start, vobPacket(cue)))
	}
	return index.Bytes(), out.Bytes()
}

// vobPacket is one subtitle packet: the size, the offset of the control
// sequences, two fields of run-length coded rows and the commands that show the
// rectangle at its start and stop showing it at its end.
func vobPacket(cue Cue) []byte {
	// Every row is the colour 1 up to the end of the line (the code 00 01).
	top, bottom := bytes.Repeat([]byte{0, 1}, (cue.H+1)/2), bytes.Repeat([]byte{0, 1}, cue.H/2)
	const head = 4
	control := head + len(top) + len(bottom)
	var p bytes.Buffer
	be := func(v ...any) {
		for _, x := range v {
			binary.Write(&p, binary.BigEndian, x) //nolint:errcheck // A buffer doesn't fail.
		}
	}
	be(uint16(0), uint16(control))
	p.Write(top)
	p.Write(bottom)
	x2, y2 := cue.X+cue.W-1, cue.Y+cue.H-1
	// Start: palette (colour 1 is entry 1), alpha (only colour 1 is seen), the
	// area, and where the two fields start.
	second := control + 2 + 2 + 1 + 3 + 3 + 7 + 5 + 1
	be(uint16(0), uint16(second), byte(1), byte(3), byte(0), byte(0x10), byte(4), byte(0), byte(0xF0),
		byte(5), byte(cue.X>>4), byte(cue.X&0xF<<4|x2>>8), byte(x2), byte(cue.Y>>4), byte(cue.Y&0xF<<4|y2>>8), byte(y2),
		byte(6), uint16(head), uint16(head+len(top)), byte(0xFF))
	// Stop, after the cue's length in units of 1024/90000 s.
	be(uint16(math.Round((cue.End-cue.Start)*90000/1024)), uint16(second), byte(2), byte(0xFF))
	out := p.Bytes()
	binary.BigEndian.PutUint16(out, uint16(len(out)))
	return out
}

// vobSector wraps a packet in a 2048-byte sector: a pack header, a private
// stream 1 packet with the time and the substream id 0x20 and padding.
func vobSector(at float64, packet []byte) []byte {
	var s bytes.Buffer
	s.Write([]byte{0, 0, 1, 0xBA, 0x44, 0, 4, 0, 4, 1, 0x01, 0x89, 0xC3, 0xF8})
	pts := uint64(math.Round(at * 90000))
	pes := []byte{0x81, 0x80, 5,
		0x21 | byte(pts>>29&0x0E), byte(pts >> 22), byte(pts>>14) | 1, byte(pts >> 7), byte(pts<<1) | 1, 0x20}
	s.Write([]byte{0, 0, 1, 0xBD})
	binary.Write(&s, binary.BigEndian, uint16(len(pes)+len(packet))) //nolint:errcheck // A buffer doesn't fail.
	s.Write(pes)
	s.Write(packet)
	if pad := 2048 - s.Len() - 6; pad >= 0 {
		s.Write([]byte{0, 0, 1, 0xBE})
		binary.Write(&s, binary.BigEndian, uint16(pad)) //nolint:errcheck // A buffer doesn't fail.
		s.Write(bytes.Repeat([]byte{0xFF}, pad))
	}
	return s.Bytes()
}
