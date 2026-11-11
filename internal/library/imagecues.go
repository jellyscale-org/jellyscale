package library

import (
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"

	"jellyscale/internal/store"
)

// An image subtitle (PGS, VobSub, DVB) cue is one packet when it appears, and
// the track has no keyframes, so a transcode that begins mid-film needs to know
// where the picture showing at its cut began, to open the film early enough to
// read it. The Cues of a Matroska file point at the packets of the track, but
// say neither what a packet shows nor when it ends; so the scan keeps the
// points (ReadImageCues, one read of the file's head and its Cues, as for the
// keyframes), and a run reads the packet at the last point before its cut, and
// those before it back to the start of its picture (ImageTrack.OpenAt): a
// few small reads per run, however long the film or the gap since the cue.

const (
	mkvCluster          = 0x1F43B675
	mkvBlockGroup       = 0xA0
	mkvBlock            = 0xA1
	mkvSimpleBlock      = 0xA3
	mkvBlockDuration    = 0x9B
	mkvTrackCodec       = 0x86
	mkvEncodings        = 0x6D80
	mkvEncoding         = 0x6240
	mkvCompression      = 0x5034
	mkvCompAlgo         = 0x4254
	mkvCompSettings     = 0x4255
	mkvEncryption       = 0x5035
	mkvCueCluster       = 0xF1
	mkvCueRelative      = 0xF0
	mkvVideoTrack       = 1
	mkvSubtitleTrack    = 17
	mkvCompZlib         = 0
	mkvCompHeaderStrip  = 3
	packetWindow        = 4096 // Enough of a block to tell what its packet shows.
	maxClusterHeaderLen = 16
)

// ImageCues are the image subtitle tracks of a Matroska film, by stream index.
// A track the file lists no cue points for is not among them.
type ImageCues map[int]ImageTrack

// ImageTrack is where the packets of one image track lie.
type ImageTrack struct {
	Codec string `json:"codec"` // "pgs", "vobsub" or "dvb".
	// Zlib says the blocks are compressed with zlib, and Strip is the bytes that
	// header stripping took off the front of each (the two ways Matroska
	// compresses a track).
	Zlib  bool   `json:"zlib,omitempty"`
	Strip []byte `json:"strip,omitempty"`
	// Unit is the length of a timestamp step in ticks (100 ns): a block's
	// duration is counted in steps.
	Unit int64 `json:"unit"`
	// Points are [time in ticks, the cluster's offset in the file, the block's
	// offset in the cluster's data, the time in ticks of the last keyframe the
	// file's Cues place at or before that cluster], sorted by time. The fourth is
	// where a run must open the film for its demuxer to land at or before the
	// packet: a muxer may write a packet well before the video of its time (ffmpeg
	// writes a subtitle when it encodes it), and a seek to the cut then lands after it.
	Points [][4]int64 `json:"points"`
}

// ErrUnknownPicture means a packet of the track can't be read for what it
// shows (an encoding or a codec this reader doesn't know): the caller treats
// the track as one with no cue points.
var ErrUnknownPicture = errors.New("a packet of the image track can't be read")

var imageCodecs = map[string]string{"S_HDMV/PGS": "pgs", "S_VOBSUB": "vobsub", "S_DVBSUB": "dvb"}

// ReadImageCues returns the cue points of the image tracks of a Matroska file,
// from its Cues, by the index ffprobe gives the stream (the track's place in
// the file's Tracks). It reads the head and the Cues, which is a small fraction
// of the file.
func ReadImageCues(ctx context.Context, s store.Store, key string, size int64) (ImageCues, error) {
	file := indexFile{ctx: ctx, store: s, key: key, size: size}
	base, where, unit, tracks, err := file.matroskaLayout()
	if errors.Is(err, ErrNoIndex) {
		return ImageCues{}, nil // The track's times are unknown.
	}
	if err != nil {
		return nil, err
	}
	found := ImageCues{}
	byNumber := map[uint64]int{}
	index := 0
	video := uint64(0) // The first video track, whose keyframes the scan keeps.
	ebmlChildren(tracks, func(id uint32, entry []byte) {
		if id != mkvTrackEntry {
			return
		}
		defer func() { index++ }()
		var number, kind uint64
		var codec string
		track := ImageTrack{Unit: unit / 100}
		supported := true
		ebmlChildren(entry, func(id uint32, value []byte) {
			switch id {
			case mkvTrackNumber:
				number = ebmlUint(value)
			case mkvTrackType:
				kind = ebmlUint(value)
			case mkvTrackCodec:
				codec = imageCodecs[string(value)]
			case mkvEncodings:
				supported = track.compression(value)
			}
		})
		if kind == mkvVideoTrack && video == 0 {
			video = number
		}
		if kind == mkvSubtitleTrack && codec != "" && supported {
			track.Codec = codec
			found[index], byNumber[number] = track, index
		}
	})
	if len(found) == 0 {
		return found, nil
	}
	_, cues, err := file.element(where[mkvCues], ebml)
	if err != nil {
		return nil, err
	}
	var keyframes [][2]int64 // [time in ticks, cluster's offset] of the video's.
	ebmlChildren(cues, func(id uint32, point []byte) {
		if id != mkvCuePoint {
			return
		}
		var time uint64
		var positions [][]byte
		ebmlChildren(point, func(id uint32, value []byte) {
			switch id {
			case mkvCueTime:
				time = ebmlUint(value)
			case mkvCuePosition:
				positions = append(positions, value)
			}
		})
		if unit <= 0 || time > uint64(math.MaxInt64/unit) {
			return
		}
		for _, position := range positions {
			var number, cluster, relative uint64
			ebmlChildren(position, func(id uint32, value []byte) {
				switch id {
				case mkvCueTrack:
					number = ebmlUint(value)
				case mkvCueCluster:
					cluster = ebmlUint(value)
				case mkvCueRelative:
					relative = ebmlUint(value)
				}
			})
			if cluster >= math.MaxInt64/2 || relative >= math.MaxInt64/2 {
				continue
			}
			if number == video {
				keyframes = append(keyframes, [2]int64{int64(time) * unit / 100, base + int64(cluster)})
			} else if i, ok := byNumber[number]; ok {
				track := found[i]
				track.Points = append(track.Points, [4]int64{int64(time) * unit / 100, base + int64(cluster), int64(relative), 0})
				found[i] = track
			}
		}
	})
	slices.SortStableFunc(keyframes, func(a, b [2]int64) int { return cmp.Compare(a[1], b[1]) })
	for i, track := range found {
		if len(track.Points) == 0 {
			delete(found, i)
			continue
		}
		slices.SortStableFunc(track.Points, func(a, b [4]int64) int { return cmp.Compare(a[0], b[0]) })
		for p := range track.Points {
			// The last keyframe that lies at or before the packet's cluster; 0 if none
			// does, for a demuxer lands on the first one then. A file whose Cues
			// list no keyframe says nothing of where its video lies: the packet's own time.
			at := sort.Search(len(keyframes), func(k int) bool { return keyframes[k][1] > track.Points[p][1] })
			switch {
			case len(keyframes) == 0:
				track.Points[p][3] = track.Points[p][0]
			case at > 0:
				track.Points[p][3] = keyframes[at-1][0]
			}
		}
		found[i] = track
	}
	return found, nil
}

// compression reads a track's ContentEncodings: false if the track is
// encrypted or compressed some other way.
func (t *ImageTrack) compression(encodings []byte) bool {
	ok := true
	ebmlChildren(encodings, func(id uint32, encoding []byte) {
		if id != mkvEncoding {
			return
		}
		ebmlChildren(encoding, func(id uint32, value []byte) {
			switch id {
			case mkvEncryption:
				ok = false
			case mkvCompression:
				algo := uint64(mkvCompZlib) // The default.
				var settings []byte
				ebmlChildren(value, func(id uint32, v []byte) {
					switch id {
					case mkvCompAlgo:
						algo = ebmlUint(v)
					case mkvCompSettings:
						settings = v
					}
				})
				switch algo {
				case mkvCompZlib:
					t.Zlib = true
				case mkvCompHeaderStrip:
					t.Strip = bytes.Clone(settings)
				default:
					ok = false
				}
			}
		})
	})
	return ok
}

// OpenAt returns the time (seconds on the film's clock) a run whose first frame
// is at time cut must open the film at to read the picture of the track showing
// there; cut itself if nothing is. A picture showing is read from its first
// packet: the packet of the last point at or before the cut and, for a codec
// whose packet may add to the picture before it, those before that, a few
// hundred bytes each. With nothing showing, the run still has to land at or
// before the packet that follows the cut, which a muxer may have written before
// the video of the cut. ErrUnknownPicture says the track can't be read.
func (t ImageTrack) OpenAt(ctx context.Context, s store.Store, key string, size int64, cut float64) (float64, error) {
	file := indexFile{ctx: ctx, store: s, key: key, size: size}
	ticks := int64(math.Floor(cut * 1e7))
	last := sort.Search(len(t.Points), func(i int) bool { return t.Points[i][0] > ticks }) - 1
	first, err := t.pictureStart(&file, last, ticks)
	if err != nil {
		return 0, err
	}
	open := cut
	if first >= 0 {
		open = min(open, float64(t.Points[first][0])/1e7)
	} else {
		first = last + 1
	}
	if first < len(t.Points) {
		open = min(open, float64(t.Points[first][3])/1e7)
	}
	return open, nil
}

// pictureStart returns the index of the first packet of the picture showing at
// time ticks, given the last point at or before it, or -1 if nothing is.
func (t ImageTrack) pictureStart(file *indexFile, last int, ticks int64) (int, error) {
	earliest := -1 // The earliest packet seen that adds to the picture.
	for i := last; i >= 0; i-- {
		packet, err := t.packet(file, t.Points[i])
		if err != nil {
			return -1, err
		}
		if t.Codec == "vobsub" && packet.duration < 0 {
			return -1, fmt.Errorf("%w: a VobSub block with no duration", ErrUnknownPicture)
		}
		shows, starts := t.classify(packet, ticks-t.Points[i][0])
		switch {
		case !shows && earliest < 0:
			return -1, nil
		case !shows:
			return earliest, nil
		case starts:
			return i, nil
		}
		earliest = i
	}
	return earliest, nil
}

// packetBlock is what is read of a block: the start of its packet and the
// duration its group gives, in ticks, or -1.
type packetBlock struct {
	prefix   []byte
	duration int64
}

// packet reads the block at a point: the cluster's header to find where its
// data begins, then the block.
func (t ImageTrack) packet(file *indexFile, point [4]int64) (packetBlock, error) {
	head, err := file.at(point[1], maxClusterHeaderLen)
	if err != nil {
		return packetBlock{}, err
	}
	id, header, _, ok := ebml(head)
	if !ok || id != mkvCluster {
		return packetBlock{}, fmt.Errorf("%w: no cluster at %d", ErrUnknownPicture, point[1])
	}
	at := point[1] + header + point[2]
	raw, err := file.at(at, packetWindow)
	if err != nil {
		return packetBlock{}, err
	}
	id, header, size, ok := ebml(raw)
	if !ok || size < 0 || (id != mkvSimpleBlock && id != mkvBlockGroup) {
		return packetBlock{}, fmt.Errorf("%w: no block at %d", ErrUnknownPicture, at)
	}
	block, duration := raw[header:], int64(-1)
	if id == mkvBlockGroup {
		// The group's Block, then perhaps its BlockDuration after it, which may
		// lie beyond the window.
		blockID, blockHeader, blockSize, ok := ebml(block)
		if !ok || blockID != mkvBlock || blockSize < 0 || blockHeader+blockSize > size {
			return packetBlock{}, fmt.Errorf("%w: a block group at %d holds no block", ErrUnknownPicture, at)
		}
		if rest := size - blockHeader - blockSize; rest > 0 {
			after, err := file.at(at+header+blockHeader+blockSize, min(rest, 16))
			if err != nil {
				return packetBlock{}, err
			}
			if id, h, n, ok := ebml(after); ok && id == mkvBlockDuration && n >= 0 && h+n <= int64(len(after)) {
				duration = int64(ebmlUint(after[h:h+n])) * t.Unit
			}
		}
		block = block[blockHeader:min(int64(len(block)), blockHeader+blockSize)]
	} else {
		block = block[:min(int64(len(block)), size)]
	}
	// The track number, the timestamp (2 bytes) and the flags; laced blocks are
	// not what a subtitle track has.
	number := 1
	for mask := byte(0x80); number <= 8 && len(block) > 0 && block[0]&mask == 0; mask >>= 1 {
		number++
	}
	if len(block) < number+3 || block[number+2]&0x06 != 0 {
		return packetBlock{}, fmt.Errorf("%w: a laced or short block at %d", ErrUnknownPicture, at)
	}
	frame := block[number+3:]
	switch {
	case t.Zlib:
		reader, err := zlib.NewReader(bytes.NewReader(frame))
		if err != nil {
			return packetBlock{}, fmt.Errorf("%w: %v", ErrUnknownPicture, err)
		}
		// The window cuts the stream off, which is an error after the bytes
		// that were read, and those are all that is wanted.
		frame, _ = io.ReadAll(io.LimitReader(reader, 256))
	case len(t.Strip) > 0:
		frame = append(bytes.Clone(t.Strip), frame...)
	}
	return packetBlock{prefix: frame, duration: duration}, nil
}

// classify says whether a packet shows anything at the cut, age ticks after its
// time, and whether it begins a picture by itself or adds to the one before it.
func (t ImageTrack) classify(p packetBlock, age int64) (shows, starts bool) {
	b := p.prefix
	switch t.Codec {
	case "pgs":
		// Segments: a type, a length of 2 bytes and the body. The first is the
		// composition: its state (0x80 starts an epoch, 0x40 is an acquisition
		// point, 0 adds to the picture) and the number of objects it draws.
		if len(b) < 14 || b[0] != 0x16 {
			return true, false
		}
		return b[13] > 0, b[10] != 0
	case "dvb":
		// Segments: 0x0F, a type, a page and a length of 2 bytes, and the body.
		// The page composition says its state in bits 2 and 3 of its second
		// byte (0 adds to the page), then lists 6 bytes for each region it shows.
		if len(b) >= 2 && b[0] == 0x20 && b[1] == 0 {
			b = b[2:]
		}
		for len(b) >= 6 && b[0] == 0x0F {
			length := int(b[4])<<8 | int(b[5])
			if b[1] == 0x10 && len(b) >= 8 && length >= 2 {
				return length > 2, b[7]&0x0C != 0
			}
			if len(b) < 6+length {
				break
			}
			b = b[6+length:]
		}
		return true, false
	default:
		// A VobSub packet shows its picture for its duration; it needs no other.
		return age < p.duration, true
	}
}
