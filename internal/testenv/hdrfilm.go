package testenv

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// HDRFilm describes a synthetic HEVC (or AV1) film in HDR, for the labels a
// server reads from it: HDR10, HDR10+, HLG or SDR, with a Dolby Vision profile
// on top when DVProfile is not 0. There are no RPUs (no tool here makes them),
// and a server reads the labels from the configuration box alone. Used by
// test/fixtures/dovi, which e2e.sh calls, and by the integration tests.
type HDRFilm struct {
	// Base is the picture: "pq" (HDR10), "hlg", "sdr" or "av1" (10-bit PQ).
	Base      string
	HDR10Plus bool
	// DVProfile is the Dolby Vision profile to claim, with the base layer's
	// signal compatibility id and whether there is an enhancement layer.
	DVProfile, DVCompat int
	DVEnhancement       bool
	Seconds             int
}

const (
	x265PQ  = "colorprim=bt2020:colormatrix=bt2020nc:transfer=smpte2084:hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
	x265HLG = "colorprim=bt2020:colormatrix=bt2020nc:transfer=arib-std-b67"
	x265SDR = "colorprim=bt709:colormatrix=bt709:transfer=bt709"
)

// MakeHDRFilm writes the film to out, an .mp4 path, with ffmpeg.
func MakeHDRFilm(out string, f HDRFilm) error {
	seconds := max(f.Seconds, 1)
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=" + strconv.Itoa(seconds),
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + strconv.Itoa(seconds),
		"-map", "0:v", "-map", "1:a", "-pix_fmt", "yuv420p10le"}
	x265 := map[string]string{"pq": x265PQ, "hlg": x265HLG, "sdr": x265SDR}[f.Base]
	if f.HDR10Plus {
		dir, err := os.MkdirTemp("", "hdr10plus")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		metadata := filepath.Join(dir, "hdr10plus.json")
		if err := WriteHDR10PlusMetadata(metadata, seconds*24); err != nil {
			return err
		}
		x265 += ":dhdr10-info=" + metadata
	}
	switch f.Base {
	case "av1":
		args = append(args, "-c:v", "libsvtav1", "-preset", "12", "-g", "48",
			"-svtav1-params", "color-primaries=9:transfer-characteristics=16:matrix-coefficients=9")
	case "sdr":
		args[len(args)-1] = "yuv420p"
		fallthrough
	case "pq", "hlg":
		args = append(args, "-c:v", "libx265", "-preset", "veryfast", "-tag:v", "hvc1", "-x265-params", "keyint=48:log-level=error:"+x265)
	default:
		return fmt.Errorf("unknown base %q", f.Base)
	}
	args = append(args, "-c:a", "aac", "-metadata:s:a:0", "language=eng")
	target := out
	if f.DVProfile > 0 {
		target = out + ".base.mp4"
		defer os.Remove(target)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if output, err := exec.Command("ffmpeg", append(args, target)...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, output)
	}
	if f.DVProfile == 0 {
		return nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	data, err = AddDolbyVisionConfig(data, f.DVProfile, f.DVCompat, f.DVEnhancement)
	if err != nil {
		return err
	}
	return os.WriteFile(out, data, 0o644)
}

// WriteHDR10PlusMetadata writes the JSON x265's --dhdr10-info reads: the same
// metadata on every frame.
func WriteHDR10PlusMetadata(path string, frames int) error {
	scenes := make([]map[string]any, frames)
	for i := range scenes {
		scenes[i] = map[string]any{
			"BezierCurveData": map[string]any{"Anchors": []int{102, 205, 307, 410, 512, 614, 717, 819, 921}, "KneePointX": 100, "KneePointY": 100},
			"LuminanceParameters": map[string]any{
				"AverageRGB": 1000,
				"LuminanceDistributions": map[string]any{
					"DistributionIndex": []int{1, 5, 10, 25, 50, 75, 90, 95, 99}, "DistributionValues": []int{100, 200, 300, 400, 500, 600, 700, 800, 900}},
				"MaxScl": []int{5000, 5000, 5000}},
			"NumberOfWindows": 1, "TargetedSystemDisplayMaximumLuminance": 400, "SceneFrameNumber": i, "SequenceFrameNumber": i,
		}
	}
	data, err := json.Marshal(map[string]any{"JSONInfo": map[string]any{"HDR10plusProfile": "B", "Version": "1.0"}, "SceneInfo": scenes})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// box is where a box starts in an MP4 and how long it is.
type box struct{ start, size int }

func children(data []byte, from, to int) []box {
	var boxes []box
	for from+8 <= to {
		size := int(binary.BigEndian.Uint32(data[from:]))
		if size < 8 || from+size > to {
			break
		}
		boxes = append(boxes, box{from, size})
		from += size
	}
	return boxes
}

func find(data []byte, boxes []box, name string) (box, bool) {
	for _, b := range boxes {
		if string(data[b.start+4:b.start+8]) == name {
			return b, true
		}
	}
	return box{}, false
}

// AddDolbyVisionConfig inserts a dvcC (profile 7 and below) or dvvC box after
// the codec's configuration box (hvcC or av1C) of the first track of an MP4,
// and grows the boxes around it. The moov must come after the mdat, as ffmpeg
// writes it, so that no chunk offset moves.
func AddDolbyVisionConfig(data []byte, profile, compat int, enhancement bool) ([]byte, error) {
	var chain []box
	boxes := children(data, 0, len(data))
	for _, name := range []string{"moov", "trak", "mdia", "minf", "stbl", "stsd"} {
		b, ok := find(data, boxes, name)
		if !ok {
			return nil, fmt.Errorf("no %s box", name)
		}
		chain = append(chain, b)
		boxes = children(data, b.start+8, b.start+b.size)
	}
	stsd := chain[len(chain)-1]
	// stsd has a version, flags and an entry count before its entries.
	entries := children(data, stsd.start+16, stsd.start+stsd.size)
	if len(entries) == 0 {
		return nil, fmt.Errorf("no sample entry")
	}
	entry := entries[0]
	kind := string(data[entry.start+4 : entry.start+8])
	config := map[string]string{"hvc1": "hvcC", "hev1": "hvcC", "av01": "av1C"}[kind]
	if config == "" {
		return nil, fmt.Errorf("the first sample entry is %s, not HEVC or AV1", kind)
	}
	// A video sample entry has 78 bytes after its header before its child boxes.
	configBox, ok := find(data, children(data, entry.start+8+78, entry.start+entry.size), config)
	if !ok {
		return nil, fmt.Errorf("no %s box", config)
	}
	name := "dvvC"
	if profile <= 7 {
		name = "dvcC"
	}
	record := make([]byte, 24)
	record[0], record[1] = 1, 0                    // version 1.0
	flags := uint16(profile)<<9 | 10<<3 | 1<<2 | 1 // level 10, rpu present, base layer present
	if enhancement {
		flags |= 1 << 1
	}
	binary.BigEndian.PutUint16(record[2:], flags)
	record[4] = byte(compat) << 4
	added := binary.BigEndian.AppendUint32(nil, uint32(8+len(record)))
	added = append(append(added, name...), record...)

	at := configBox.start + configBox.size
	out := append(append(append([]byte{}, data[:at]...), added...), data[at:]...)
	for _, b := range append(chain, entry) {
		binary.BigEndian.PutUint32(out[b.start:], binary.BigEndian.Uint32(out[b.start:])+uint32(len(added)))
	}
	return out, nil
}
