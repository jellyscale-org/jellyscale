package transcode

import (
	"encoding/binary"
	"errors"
)

// segmentTicks is the length in ticks of the first track's samples in a fragmented MP4 segment, from its
// init segment's timescale and the sample durations of its fragments. It reads the boxes and starts no
// process: it runs beside every segment a job stores.
func segmentTicks(init, segment []byte) (int64, error) {
	mdhd := findBox(findBox(findBox(findBox(init, "moov"), "trak"), "mdia"), "mdhd")
	if len(mdhd) < 24 {
		return 0, errors.New("the init segment has no mdhd box")
	}
	at := 12 // After the version, flags, creation and modification times.
	if mdhd[0] == 1 {
		at = 20
	}
	timescale := int64(binary.BigEndian.Uint32(mdhd[at:]))
	if timescale == 0 {
		return 0, errors.New("the init segment's timescale is zero")
	}
	var units int64
	for rest := segment; ; {
		kind, body, after, ok := nextBox(rest)
		if !ok {
			break
		}
		rest = after
		if kind != "moof" {
			continue
		}
		traf := findBox(body, "traf")
		var perSample int64
		if tfhd := findBox(traf, "tfhd"); len(tfhd) >= 8 {
			at := 8 // After the version, flags and track id.
			flags := binary.BigEndian.Uint32(tfhd)
			if flags&0x1 != 0 {
				at += 8 // The base data offset.
			}
			if flags&0x2 != 0 {
				at += 4 // The sample description index.
			}
			if flags&0x8 != 0 && len(tfhd) >= at+4 {
				perSample = int64(binary.BigEndian.Uint32(tfhd[at:]))
			}
		}
		trun := findBox(traf, "trun")
		if len(trun) < 8 {
			return 0, errors.New("a fragment has no trun box")
		}
		flags, count := binary.BigEndian.Uint32(trun), int(binary.BigEndian.Uint32(trun[4:]))
		if flags&0x100 == 0 {
			units += perSample * int64(count)
			continue
		}
		at := 8
		if flags&0x1 != 0 {
			at += 4
		}
		if flags&0x4 != 0 {
			at += 4
		}
		size := 0
		for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if flags&bit != 0 {
				size += 4
			}
		}
		if len(trun) < at+count*size {
			return 0, errors.New("a trun box is cut short")
		}
		for i := 0; i < count; i++ {
			units += int64(binary.BigEndian.Uint32(trun[at+i*size:]))
		}
	}
	return units * ticksPerSecond / timescale, nil
}

// nextBox splits the first box off data: its type, its contents and what follows it.
func nextBox(data []byte) (kind string, body, rest []byte, ok bool) {
	if len(data) < 8 {
		return "", nil, nil, false
	}
	size := int(binary.BigEndian.Uint32(data))
	if size < 8 || size > len(data) {
		return "", nil, nil, false // A box cut short, or one with a 64-bit size, which a fragment never has.
	}
	return string(data[4:8]), data[8:size], data[size:], true
}

// findBox is the contents of the first box of the given type in data, or nil.
func findBox(data []byte, want string) []byte {
	for {
		kind, body, rest, ok := nextBox(data)
		if !ok {
			return nil
		}
		if kind == want {
			return body
		}
		data = rest
	}
}
