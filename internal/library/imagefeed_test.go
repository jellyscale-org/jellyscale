package library

import (
	"bytes"
	"testing"
)

// The Segment of the head a run is given has the size "unknown", whatever the
// length of the size it was written with, and nothing else of the head changes.
func TestUnboundedSegmentGivesTheSegmentNoSize(t *testing.T) {
	ebmlHead := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x84, 0x42, 0x82, 0x81, 0x00} // The header's body is 4 bytes.
	for _, size := range [][]byte{
		{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0xE8}, // Eight bytes, as ffmpeg writes it.
		{0x42, 0x10}, // Two.
		{0x85},       // One.
	} {
		head := append(bytes.Clone(ebmlHead), 0x18, 0x53, 0x80, 0x67)
		head = append(head, size...)
		head = append(head, 0xEC, 0x80) // A Void element after it, which must stay.
		got, err := unboundedSegment(head)
		if err != nil {
			t.Fatal(err)
		}
		at := len(ebmlHead)
		id, header, length, ok := ebml(got[at:])
		if !ok || id != mkvSegment || length != -1 || header != int64(4+len(size)) {
			t.Errorf("size %x: the Segment reads as id %x, header %d, size %d", size, id, header, length)
		}
		if !bytes.Equal(got[:at], ebmlHead) || !bytes.Equal(got[len(got)-2:], []byte{0xEC, 0x80}) {
			t.Errorf("size %x: the rest of the head changed: %x", size, got)
		}
		if !bytes.Equal(head[at+4:at+4+len(size)], size) {
			t.Errorf("size %x: the head that was given was changed", size)
		}
	}
	if _, err := unboundedSegment([]byte{0x1A, 0x45, 0xDF, 0xA3, 0x80}); err == nil {
		t.Error("a head with no Segment was accepted")
	}
}
