package library

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"jellyscale/internal/store"
)

// What a packet shows is read from its first bytes: PGS by the composition's
// state and object count, DVB by the page composition's state and regions,
// VobSub by the block's duration.
func TestClassifyReadsWhatAPacketShows(t *testing.T) {
	pgs := func(state, objects byte) packetBlock {
		return packetBlock{prefix: []byte{0x16, 0, 11, 7, 128, 4, 56, 0x10, 0, 0, state, 0, 0, objects}}
	}
	// A page composition after a display definition; a region is 6 bytes.
	dvb := func(state byte, regions int) packetBlock {
		b := []byte{0x0F, 0x14, 0, 1, 0, 5, 1, 2, 3, 4, 5, 0x0F, 0x10, 0, 1, 0, byte(2 + 6*regions), 30, state}
		return packetBlock{prefix: append(b, make([]byte, 6*regions)...)}
	}
	for _, tc := range []struct {
		name          string
		track         ImageTrack
		packet        packetBlock
		age           int64
		shows, starts bool
	}{
		{"PGS clear", ImageTrack{Codec: "pgs"}, pgs(0x80, 0), 0, false, true},
		{"PGS epoch start", ImageTrack{Codec: "pgs"}, pgs(0x80, 1), 0, true, true},
		{"PGS acquisition point", ImageTrack{Codec: "pgs"}, pgs(0x40, 2), 0, true, true},
		{"PGS normal case", ImageTrack{Codec: "pgs"}, pgs(0, 2), 0, true, false},
		{"DVB clear", ImageTrack{Codec: "dvb"}, dvb(0x0b, 0), 0, false, true},
		{"DVB mode change", ImageTrack{Codec: "dvb"}, dvb(0x0b, 1), 0, true, true},
		{"DVB normal case", ImageTrack{Codec: "dvb"}, dvb(0x01, 1), 0, true, false},
		{"VobSub within its duration", ImageTrack{Codec: "vobsub"}, packetBlock{duration: 80}, 79, true, true},
		{"VobSub after its duration", ImageTrack{Codec: "vobsub"}, packetBlock{duration: 80}, 80, false, true},
	} {
		shows, starts := tc.track.classify(tc.packet, tc.age)
		if shows != tc.shows || starts != tc.starts {
			t.Errorf("%s: shows %v and starts %v, want %v and %v", tc.name, shows, starts, tc.shows, tc.starts)
		}
	}
}

// A muxer may write a subtitle packet well before the video of its time (ffmpeg
// does when it encodes a track it reads from another file), and a seek to the
// cut then lands after the packet. Each point says where to open instead: at the
// last keyframe the Cues place at or before the packet's cluster.
func TestImageCuesSayWhereToOpenForAPacketWrittenAheadOfItsTime(t *testing.T) {
	// Sizes are 4-byte vints and numbers 4 bytes, so offsets can be counted.
	el := func(id uint32, body ...[]byte) []byte {
		idBytes := []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
		for idBytes[0] == 0 {
			idBytes = idBytes[1:]
		}
		content := bytes.Join(body, nil)
		size := 0x10000000 | len(content)
		return append(append(idBytes, byte(size>>24), byte(size>>16), byte(size>>8), byte(size)), content...)
	}
	num := func(v int) []byte { return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)} }
	cluster := func(ms int) []byte { return el(mkvCluster, el(0xE7, num(ms)), bytes.Repeat([]byte{0}, 100)) }
	tracks := el(mkvTracks,
		el(mkvTrackEntry, el(mkvTrackNumber, num(1)), el(mkvTrackType, num(1))),
		el(mkvTrackEntry, el(mkvTrackNumber, num(2)), el(mkvTrackType, num(17)), el(mkvTrackCodec, []byte("S_HDMV/PGS"))))
	a, b := cluster(0), cluster(14000)
	seekHead := func(tracksAt, cuesAt int) []byte {
		seek := func(id uint32, at int) []byte {
			return el(mkvSeek, el(mkvSeekID, num(int(id))), el(mkvSeekPosition, num(at)))
		}
		return el(mkvSeekHead, seek(mkvTracks, tracksAt), seek(mkvCues, cuesAt))
	}
	headLen := len(seekHead(0, 0))
	tracksAt := headLen
	atA := tracksAt + len(tracks)
	atB := atA + len(a)
	cuesAt := atB + len(b)
	point := func(ms, track, at int) []byte {
		return el(mkvCuePoint, el(mkvCueTime, num(ms)), el(mkvCuePosition, el(mkvCueTrack, num(track)), el(mkvCueCluster, num(at)), el(mkvCueRelative, num(0))))
	}
	cues := el(mkvCues, point(0, 1, atA), point(14000, 1, atB), point(18000, 2, atA), point(20000, 2, atB))
	segment := bytes.Join([][]byte{seekHead(tracksAt, cuesAt), tracks, a, b, cues}, nil)
	file := append(el(ebmlHeader, el(0x4282, []byte("matroska"))), el(mkvSegment, segment)...)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "film.mkv"), file, 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := ReadImageCues(context.Background(), store.NewPosix(dir), "film.mkv", int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	points := found[1].Points
	if len(points) != 2 {
		t.Fatalf("points: %v", points)
	}
	// The packet at 18 s lies in the first cluster, the keyframe at 0 is the last
	// one at or before it; the packet at 20 s lies in the second, at 14 s.
	if points[0][0] != 18000*10000 || points[0][3] != 0 || points[1][0] != 20000*10000 || points[1][3] != 14000*10000 {
		t.Errorf("points: %v, want open times 0 and 14 s for packets at 18 s and 20 s", points)
	}
}
