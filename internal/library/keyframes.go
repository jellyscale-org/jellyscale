package library

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"

	"jellyscale/internal/store"
)

// ErrNoIndex means the file has no keyframe index that can be read cheaply, so
// a copied stream can't be cut from it and it is re-encoded instead.
var ErrNoIndex = errors.New("the file has no usable keyframe index")

// noIndex is ErrNoIndex with the reason, so a scan can say why a film is
// re-encoded. A read that fails is not one: it is returned as it is, because
// the file may well have an index that a later scan can read.
func noIndex(why string) error { return fmt.Errorf("%w: %s", ErrNoIndex, why) }

// maxIndexBytes bounds what is read into memory for one file's index: a
// Matroska Cues element or an MP4 moov box. A film's is a few megabytes at most.
const maxIndexBytes = 64 << 20

// ReadKeyframes returns the times of the first video track's keyframes, in
// ticks, from the index the file keeps itself: Matroska Cues or the MP4 sample
// tables. It reads the header and the index, a small fraction of the file,
// where listing the keyframes with ffprobe reads the whole of it, which from
// S3 is the whole object. A file without such an index gives ErrNoIndex.
func ReadKeyframes(ctx context.Context, s store.Store, key string, size int64, container string) ([]int64, error) {
	file := indexFile{ctx: ctx, store: s, key: key, size: size}
	var keyframes []int64
	var err error
	switch container {
	case "mkv":
		keyframes, err = file.matroskaKeyframes()
	case "mp4":
		keyframes, err = file.mp4Keyframes()
	default:
		return nil, noIndex("the container is " + container)
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(keyframes)
	keyframes = slices.Compact(keyframes)
	if len(keyframes) == 0 {
		return nil, noIndex("it lists no video keyframes")
	}
	return keyframes, nil
}

type indexFile struct {
	ctx   context.Context
	store store.Store
	key   string
	size  int64
	// head is the start of the file, read once, since headers usually sit in it.
	head []byte
}

// at reads n bytes from off, or fewer at the end of the file. A range that
// starts in head costs nothing.
func (f *indexFile) at(off, n int64) ([]byte, error) {
	n = min(n, f.size-off)
	if off < 0 || n <= 0 || n > maxIndexBytes {
		return nil, noIndex("an element lies outside the file or is over 64 MiB")
	}
	if off+n <= int64(len(f.head)) {
		return f.head[off : off+n], nil
	}
	// The stream is cut off once n bytes are read, which ends an S3 GET.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	body, err := f.store.Open(ctx, f.key, off)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (f *indexFile) readHead() error {
	head, err := f.at(0, 64<<10)
	f.head = head
	return err
}

// element reads an EBML element or MP4 box at off in full.
func (f *indexFile) element(off int64, parse func([]byte) (uint32, int64, int64, bool)) (id uint32, body []byte, err error) {
	prefix, err := f.at(off, 16)
	if err != nil {
		return 0, nil, err
	}
	id, header, length, ok := parse(prefix)
	if !ok || length < 0 {
		return 0, nil, noIndex("an element header is not valid")
	}
	whole, err := f.at(off, header+length)
	if err != nil {
		return 0, nil, err
	}
	if int64(len(whole)) < header+length {
		return 0, nil, noIndex("an element runs past the end of the file")
	}
	return id, whole[header:], nil
}

// Matroska.

const (
	ebmlHeader      = 0x1A45DFA3
	mkvSegment      = 0x18538067
	mkvSeekHead     = 0x114D9B74
	mkvSeek         = 0x4DBB
	mkvSeekID       = 0x53AB
	mkvSeekPosition = 0x53AC
	mkvInfo         = 0x1549A966
	mkvTimecodeUnit = 0x2AD7B1
	mkvTracks       = 0x1654AE6B
	mkvTrackEntry   = 0xAE
	mkvTrackNumber  = 0xD7
	mkvTrackType    = 0x83
	mkvCues         = 0x1C53BB6B
	mkvCuePoint     = 0xBB
	mkvCueTime      = 0xB3
	mkvCuePosition  = 0xB7
	mkvCueTrack     = 0xF7
)

// ebml reads an element header: its ID, the header's length and the body's
// length, which is -1 when unknown.
func ebml(b []byte) (id uint32, header, size int64, ok bool) {
	if len(b) == 0 {
		return 0, 0, 0, false
	}
	idLen := 1
	for mask := byte(0x80); idLen <= 4 && b[0]&mask == 0; mask >>= 1 {
		idLen++
	}
	if idLen > 4 || len(b) < idLen+1 {
		return 0, 0, 0, false
	}
	for _, c := range b[:idLen] {
		id = id<<8 | uint32(c)
	}
	sizeLen := 1
	for mask := byte(0x80); sizeLen <= 8 && b[idLen]&mask == 0; mask >>= 1 {
		sizeLen++
	}
	if sizeLen > 8 || len(b) < idLen+sizeLen {
		return 0, 0, 0, false
	}
	value := uint64(b[idLen]) & (0xFF >> sizeLen)
	allOnes := value == 0xFF>>sizeLen
	for _, c := range b[idLen+1 : idLen+sizeLen] {
		value = value<<8 | uint64(c)
		allOnes = allOnes && c == 0xFF
	}
	if allOnes || value > 1<<62 {
		return id, int64(idLen + sizeLen), -1, true
	}
	return id, int64(idLen + sizeLen), int64(value), true
}

// ebmlChildren calls fn for each element of b that is wholly inside it.
func ebmlChildren(b []byte, fn func(id uint32, body []byte)) {
	for len(b) > 0 {
		id, header, size, ok := ebml(b)
		if !ok || size < 0 || header+size > int64(len(b)) {
			return
		}
		fn(id, b[header:header+size])
		b = b[header+size:]
	}
}

func ebmlUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// matroskaLayout reads the head of a Matroska file for where its Segment's
// children are, and the length of a timestamp step in nanoseconds. Its Tracks
// are read, and its Cues named, or the file has no index.
func (f *indexFile) matroskaLayout() (base int64, where map[uint32]int64, unit int64, tracks []byte, err error) {
	if err := f.readHead(); err != nil {
		return 0, nil, 0, nil, err
	}
	// The Segment follows the EBML header; its children are placed by the
	// SeekHead, which every muxer writes first.
	id, header, size, ok := ebml(f.head)
	if !ok || id != ebmlHeader || size < 0 {
		return 0, nil, 0, nil, noIndex("it does not start with an EBML header")
	}
	if header+size > int64(len(f.head)) {
		return 0, nil, 0, nil, noIndex("the EBML header runs past the start of the file that was read")
	}
	id, segmentHeader, _, ok := ebml(f.head[header+size:])
	if !ok || id != mkvSegment {
		return 0, nil, 0, nil, noIndex("no Segment follows the EBML header")
	}
	base = header + size + segmentHeader
	where = map[uint32]int64{}
	ebmlChildren(f.head[base:], func(id uint32, body []byte) {
		if id != mkvSeekHead {
			return
		}
		ebmlChildren(body, func(id uint32, entry []byte) {
			var target uint32
			var position int64
			if id != mkvSeek {
				return
			}
			ebmlChildren(entry, func(id uint32, value []byte) {
				switch id {
				case mkvSeekID:
					target = uint32(ebmlUint(value))
				case mkvSeekPosition:
					position = int64(ebmlUint(value))
				}
			})
			if _, seen := where[target]; !seen {
				where[target] = base + position
			}
		})
	})
	if _, ok := where[mkvCues]; !ok {
		return 0, nil, 0, nil, noIndex("the SeekHead names no Cues")
	}

	unit = 1_000_000 // Nanoseconds per timestamp step, by default.
	if at, ok := where[mkvInfo]; ok {
		_, info, err := f.element(at, ebml)
		if err != nil {
			return 0, nil, 0, nil, err
		}
		ebmlChildren(info, func(id uint32, value []byte) {
			if id == mkvTimecodeUnit {
				unit = int64(ebmlUint(value))
			}
		})
	}
	at, ok := where[mkvTracks]
	if !ok {
		return 0, nil, 0, nil, noIndex("the SeekHead names no Tracks")
	}
	_, tracks, err = f.element(at, ebml)
	if err != nil {
		return 0, nil, 0, nil, err
	}
	return base, where, unit, tracks, nil
}

func (f *indexFile) matroskaKeyframes() ([]int64, error) {
	_, where, unit, tracks, err := f.matroskaLayout()
	if err != nil {
		return nil, err
	}
	video := uint64(0)
	ebmlChildren(tracks, func(id uint32, entry []byte) {
		if id != mkvTrackEntry || video != 0 {
			return
		}
		var number, kind uint64
		ebmlChildren(entry, func(id uint32, value []byte) {
			switch id {
			case mkvTrackNumber:
				number = ebmlUint(value)
			case mkvTrackType:
				kind = ebmlUint(value)
			}
		})
		if kind == 1 {
			video = number
		}
	})
	if video == 0 {
		return nil, noIndex("no video track")
	}

	_, cues, err := f.element(where[mkvCues], ebml)
	if err != nil {
		return nil, err
	}
	var keyframes []int64
	tooLarge := false
	ebmlChildren(cues, func(id uint32, point []byte) {
		if id != mkvCuePoint {
			return
		}
		var time uint64
		onVideo := false
		ebmlChildren(point, func(id uint32, value []byte) {
			switch id {
			case mkvCueTime:
				time = ebmlUint(value)
			case mkvCuePosition:
				ebmlChildren(value, func(id uint32, track []byte) {
					if id == mkvCueTrack && ebmlUint(track) == video {
						onVideo = true
					}
				})
			}
		})
		if !onVideo {
			return
		}
		// So that time*unit can't overflow.
		if unit <= 0 || time > uint64(math.MaxInt64/unit) {
			tooLarge = true
			return
		}
		keyframes = append(keyframes, int64(time)*unit/100)
	})
	if tooLarge {
		return nil, noIndex("a cue time is too large to count in ticks")
	}
	return keyframes, nil
}

// MP4.

// box reads a box header: its type as a number, the header's length and the
// body's length, which is -1 for a box that runs to the end of the file.
func box(b []byte) (kind uint32, header, size int64, ok bool) {
	if len(b) < 8 {
		return 0, 0, 0, false
	}
	length := int64(binary.BigEndian.Uint32(b))
	kind = binary.BigEndian.Uint32(b[4:])
	header = 8
	if length == 1 {
		if len(b) < 16 {
			return 0, 0, 0, false
		}
		length, header = int64(binary.BigEndian.Uint64(b[8:])), 16
	}
	if length == 0 {
		return kind, header, -1, true
	}
	if length < header {
		return 0, 0, 0, false
	}
	return kind, header, length - header, true
}

func fourcc(s string) uint32 { return binary.BigEndian.Uint32([]byte(s)) }

// boxes calls fn for each box of b that is wholly inside it.
func boxes(b []byte, fn func(kind uint32, body []byte)) {
	for len(b) > 0 {
		kind, header, size, ok := box(b)
		if !ok || size < 0 || header+size > int64(len(b)) {
			return
		}
		fn(kind, b[header:header+size])
		b = b[header+size:]
	}
}

// inside returns the body of the first box of the given type, or nil.
func inside(b []byte, kind string) []byte {
	var found []byte
	boxes(b, func(k uint32, body []byte) {
		if k == fourcc(kind) && found == nil {
			found = body
		}
	})
	return found
}

// maxMediaTicks is the most a time in a file's own units may be for its
// conversion to ticks to fit an int64: far past any film.
const maxMediaTicks = math.MaxInt64 / ticksPerSecond

func (f *indexFile) mp4Keyframes() ([]int64, error) {
	if err := f.readHead(); err != nil {
		return nil, err
	}
	// Walk the top-level boxes by their headers to the moov, which a film not
	// made for streaming keeps after its mdat.
	var moov []byte
	for off := int64(0); off < f.size; {
		prefix, err := f.at(off, 16)
		if err != nil {
			return nil, err
		}
		kind, header, size, ok := box(prefix)
		if !ok {
			return nil, noIndex("a top-level box header is not valid")
		}
		if size < 0 {
			size = f.size - off - header
		}
		if kind == fourcc("moov") {
			whole, err := f.at(off, header+size)
			if err != nil {
				return nil, err
			}
			if int64(len(whole)) < header+size {
				return nil, noIndex("the moov box runs past the end of the file")
			}
			moov = whole[header:]
			break
		}
		off += header + size
	}
	if moov == nil {
		return nil, noIndex("no moov box")
	}

	movieScale := timescale(inside(moov, "mvhd"))
	var video []byte
	boxes(moov, func(kind uint32, trak []byte) {
		if kind == fourcc("trak") && video == nil {
			if hdlr := inside(inside(trak, "mdia"), "hdlr"); len(hdlr) >= 12 && string(hdlr[8:12]) == "vide" {
				video = trak
			}
		}
	})
	if video == nil || movieScale == 0 {
		return nil, noIndex("no video track")
	}
	mdia := inside(video, "mdia")
	scale := timescale(inside(mdia, "mdhd"))
	table := inside(inside(mdia, "minf"), "stbl")
	stss, stts, ctts := inside(table, "stss"), inside(table, "stts"), inside(table, "ctts")
	stsz := inside(table, "stsz")
	if stsz == nil {
		stsz = inside(table, "stz2")
	}
	if scale == 0 || len(stss) < 8 || len(stts) < 8 || len(stsz) < 12 {
		return nil, noIndex("the video has no stss, stts or stsz table")
	}

	// Edit lists shift the timeline. One edit that starts at a media time,
	// perhaps after an empty one, is what muxers write; anything else isn't
	// worth guessing at.
	var shift, delay int64 // In media and movie ticks.
	if elst := inside(inside(video, "edts"), "elst"); elst != nil {
		entries, err := editList(elst)
		if err != nil {
			return nil, err
		}
		if len(entries) == 2 && entries[0].mediaTime == -1 {
			delay, entries = entries[0].duration, entries[1:]
		}
		if len(entries) != 1 || entries[0].mediaTime < 0 {
			return nil, noIndex("the edit list is not one the reader understands")
		}
		shift = entries[0].mediaTime
	}

	sync := binary.BigEndian.Uint32(stss[4:])
	if int(sync) > (len(stss)-8)/4 {
		return nil, noIndex("stss is shorter than it says")
	}
	// Sample numbers, from 1, ascending and no more than the file has: a
	// table that says otherwise is not one to walk.
	samples := binary.BigEndian.Uint32(stsz[8:])
	wanted := make([]uint32, sync)
	for i := range wanted {
		wanted[i] = binary.BigEndian.Uint32(stss[8+4*i:])
		if wanted[i] == 0 || wanted[i] > samples || i > 0 && wanted[i] <= wanted[i-1] {
			return nil, noIndex("stss is not ascending sample numbers within the file")
		}
	}

	// Decode time from stts, by runs of samples with the same delta, and the
	// offset to presentation time from ctts, at each sync sample.
	times := make([]int64, 0, len(wanted))
	runs := stts[8:]
	runs = runs[:len(runs)/8*8]
	var dts int64      // Of the first sample of the run in use.
	first := uint64(1) // That sample's number.
	offsets := newOffsets(ctts)
	for _, sample := range wanted {
		for len(runs) >= 8 && uint64(sample) >= first+uint64(binary.BigEndian.Uint32(runs)) {
			count, delta := int64(binary.BigEndian.Uint32(runs)), int64(binary.BigEndian.Uint32(runs[4:]))
			if count > 0 && delta > (1<<55-dts)/count {
				return nil, noIndex("stts has implausible times")
			}
			dts += count * delta
			first += uint64(count)
			runs = runs[8:]
		}
		if len(runs) < 8 {
			return nil, noIndex("stts has fewer samples than stss names")
		}
		delta := int64(binary.BigEndian.Uint32(runs[4:]))
		partial := int64(uint64(sample) - first)
		if delta > 0 && partial > (1<<55-dts)/delta {
			return nil, noIndex("stts has implausible times")
		}
		pts := dts + partial*delta + offsets.at(sample) - shift
		// So that the conversion to ticks below can't overflow.
		if pts < -maxMediaTicks || pts > maxMediaTicks || delay < -maxMediaTicks || delay > maxMediaTicks {
			return nil, noIndex("the times are too large to count in ticks")
		}
		// Each term is within int64 on its own; so must their sum be.
		at, shifted := pts*ticksPerSecond/scale, delay*ticksPerSecond/movieScale
		if at < -math.MaxInt64/2 || at > math.MaxInt64/2 || shifted < -math.MaxInt64/2 || shifted > math.MaxInt64/2 {
			return nil, noIndex("the times are too large to count in ticks")
		}
		times = append(times, at+shifted)
	}
	return times, nil
}

// timescale reads the ticks per second from an mvhd or mdhd box, or 0.
func timescale(b []byte) int64 {
	if len(b) < 24 { // Too short to hold a timescale, in either version.
		return 0
	}
	if b[0] == 1 { // Version 1 has 64-bit times.
		return int64(binary.BigEndian.Uint32(b[20:]))
	}
	return int64(binary.BigEndian.Uint32(b[12:]))
}

type edit struct{ duration, mediaTime int64 }

func editList(elst []byte) ([]edit, error) {
	if len(elst) < 8 {
		return nil, noIndex("the edit list is cut short")
	}
	version, count := elst[0], int(binary.BigEndian.Uint32(elst[4:]))
	width := 12
	if version == 1 {
		width = 20
	}
	if count > (len(elst)-8)/width {
		return nil, noIndex("the edit list is shorter than it says")
	}
	entries := make([]edit, count)
	for i := range entries {
		e := elst[8+i*width:]
		if version == 1 {
			entries[i] = edit{int64(binary.BigEndian.Uint64(e)), int64(binary.BigEndian.Uint64(e[8:]))}
		} else {
			entries[i] = edit{int64(binary.BigEndian.Uint32(e)), int64(int32(binary.BigEndian.Uint32(e[4:])))}
		}
	}
	return entries, nil
}

// offsets walks a ctts box, which is optional, by sample number.
type offsets struct {
	runs   []byte
	signed bool
	index  int    // The run in use.
	last   uint32 // The last sample of that run.
}

func newOffsets(ctts []byte) *offsets {
	if len(ctts) < 8 {
		return &offsets{}
	}
	runs := ctts[8:]
	runs = runs[:min(int(binary.BigEndian.Uint32(ctts[4:])), len(runs)/8)*8]
	r := &offsets{runs: runs, signed: ctts[0] == 1}
	r.last = r.count(0)
	return r
}

func (r *offsets) count(i int) uint32 {
	if i*8 >= len(r.runs) {
		return 0
	}
	return binary.BigEndian.Uint32(r.runs[i*8:])
}

// at returns the offset for a sample; samples must be asked for in order.
func (r *offsets) at(sample uint32) int64 {
	for sample > r.last && (r.index+1)*8 < len(r.runs) {
		r.index++
		r.last += r.count(r.index)
	}
	if r.index*8 >= len(r.runs) || sample > r.last {
		return 0
	}
	v := binary.BigEndian.Uint32(r.runs[r.index*8+4:])
	if r.signed {
		return int64(int32(v))
	}
	return int64(v)
}
