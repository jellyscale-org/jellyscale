package testenv

import (
	"encoding/binary"
	"reflect"
	"testing"
)

// The proxy's record of what clients sent: the SQL of a simple Query and of a
// Parse, however the network cut the bytes, and nothing from the startup. No
// Postgres: the bytes are the protocol's own.
func TestTheStatementsAClientSentAreReadWhateverWayTheBytesAreCut(t *testing.T) {
	message := func(kind byte, body ...byte) []byte {
		out := []byte{kind, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(out[1:], uint32(4+len(body)))
		return append(out, body...)
	}
	untyped := func(code uint32, rest ...byte) []byte {
		out := make([]byte, 8)
		binary.BigEndian.PutUint32(out, uint32(8+len(rest)))
		binary.BigEndian.PutUint32(out[4:], code)
		return append(out, rest...)
	}
	var stream []byte
	stream = append(stream, untyped(80877103)...)                               // SSLRequest
	stream = append(stream, untyped(196608, []byte("user\x00x\x00\x00")...)...) // StartupMessage
	stream = append(stream, message('Q', []byte("SELECT 1\x00")...)...)
	stream = append(stream, message('P', []byte("\x00UPDATE t SET a = $1\x00\x00\x00")...)...)
	stream = append(stream, message('B', []byte("\x00\x00")...)...)
	stream = append(stream, message('S')...)
	want := []string{"SELECT 1", "UPDATE t SET a = $1"}
	for cut := 1; cut < len(stream); cut++ {
		var f frontend
		got := append(f.feed(stream[:cut]), f.feed(stream[cut:])...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cut at %d: got %q, want %q", cut, got, want)
		}
	}
}
