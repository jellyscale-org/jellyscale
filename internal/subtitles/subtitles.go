// Package subtitles turns a text subtitle track inside a media file into the
// formats clients ask for, reading the file once per track.
package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const ticksPerSecond = 10_000_000

// Track is one extracted subtitle track, in each format. ffmpeg writes them in
// one pass over the file, so a request for any format costs one read of it
// between them.
type Track struct {
	VTT, SRT, ASS string
}

// Extract reads stream index of input, a path or URL, and converts it.
func Extract(ctx context.Context, input string, index int) (Track, error) {
	dir, err := os.MkdirTemp("", "subtitles-")
	if err != nil {
		return Track{}, err
	}
	defer os.RemoveAll(dir)
	args := []string{"-nostdin", "-v", "error", "-i", input}
	for _, ext := range []string{"vtt", "srt", "ass"} {
		args = append(args, "-map", "0:"+strconv.Itoa(index), filepath.Join(dir, "track."+ext))
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// Pdeathsig fires when the thread that started ffmpeg exits, so this
	// goroutine keeps its thread until ffmpeg is done.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	DieWithParent(cmd)
	// A file cut short makes ffmpeg complain and exit 0, with whatever it had
	// read, so any output at all means the tracks can't be trusted.
	out, err := cmd.CombinedOutput()
	if err != nil || len(bytes.TrimSpace(out)) > 0 {
		return Track{}, fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var track Track
	for ext, text := range map[string]*string{"vtt": &track.VTT, "srt": &track.SRT, "ass": &track.ASS} {
		data, err := os.ReadFile(filepath.Join(dir, "track."+ext))
		if err != nil {
			return Track{}, err
		}
		*text = string(data)
	}
	return track, nil
}

// Encode and Decode are how a Track is kept in a store: one object, so a
// reader never sees half of it.
func (t Track) Encode() io.Reader {
	data, _ := json.Marshal(t)
	return bytes.NewReader(data)
}

func Decode(r io.Reader) (Track, error) {
	var t Track
	err := json.NewDecoder(r).Decode(&t)
	return t, err
}

// Render returns the track in a format ("vtt", "srt", "ass" or "js") with every
// cue moved earlier by shift ticks, none going below zero, as a client does to
// start a track part-way in. ok is false for any other format.
func (t Track) Render(format string, shift int64) (body, contentType string, ok bool) {
	switch format {
	case "vtt":
		return shiftCues(t.VTT, shift), "text/vtt", true
	case "srt":
		return shiftCues(t.SRT, shift), "application/x-subrip", true
	case "ass":
		return shiftCues(t.ASS, shift), "text/x-ssa", true
	case "js":
		data, _ := json.Marshal(map[string]any{"TrackEvents": events(shiftCues(t.VTT, shift))})
		return string(data), "application/json", true
	}
	return "", "", false
}

// Delay moves every cue of ASS text later by seconds.
func Delay(ass string, seconds float64) string {
	return shiftCues(ass, -int64(seconds*ticksPerSecond))
}

// clock matches 00:00:02.000 in WebVTT and SubRip (which writes a comma) and
// 0:00:02.00 in ASS. ffmpeg's WebVTT leaves the hours out below one hour, as
// 00:02.000.
var clock = regexp.MustCompile(`(?:(\d+):)?(\d\d):(\d\d)([.,])(\d+)`)

// shiftCues moves the time of every cue earlier by shift ticks. It also gives
// WebVTT times their hours, as Jellyfin's do, whatever the shift.
func shiftCues(text string, shift int64) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.Contains(line, "-->"):
			lines[i] = shiftClocks(line, shift)
		case strings.HasPrefix(line, "Dialogue:"):
			// The start and end are the second and third fields; the text may
			// hold anything after them.
			if parts := strings.SplitN(line, ",", 4); len(parts) == 4 {
				lines[i] = shiftClocks(strings.Join(parts[:3], ","), shift) + "," + parts[3]
			}
		}
	}
	return strings.Join(lines, "\n")
}

func shiftClocks(line string, shift int64) string {
	return clock.ReplaceAllStringFunc(line, func(match string) string {
		p := clock.FindStringSubmatch(match)
		ticks, ok := parseClock(p)
		if !ok {
			return match
		}
		digits := len(p[5])
		unit := int64(ticksPerSecond) / pow10(digits)
		whole := max(ticks-shift, 0) / unit // In the fraction's unit, 1/1000 s or 1/100 s.
		perSecond := pow10(digits)
		seconds := whole / perSecond
		hourWidth := len(p[1]) // ASS writes one digit, WebVTT two.
		if p[1] == "" {
			hourWidth = 2
		}
		return fmt.Sprintf("%0*d:%02d:%02d%s%0*d", hourWidth, seconds/3600, seconds/60%60, seconds%60, p[4], digits, whole%perSecond)
	})
}

// parseClock reads the groups of clock as ticks.
func parseClock(p []string) (int64, bool) {
	var n [4]int64
	for i, text := range []string{p[1], p[2], p[3], p[5]} {
		if text == "" { // No hours.
			continue
		}
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil || len(p[5]) > 7 {
			return 0, false
		}
		n[i] = v
	}
	seconds := (n[0]*60+n[1])*60 + n[2]
	return seconds*ticksPerSecond + n[3]*(ticksPerSecond/pow10(len(p[5]))), true
}

func pow10(n int) int64 {
	result := int64(1)
	for range n {
		result *= 10
	}
	return result
}

type event struct {
	Id                                   string
	Text                                 string
	StartPositionTicks, EndPositionTicks int64
}

// events reads SubRip into the shape jellyfin-web renders: one per cue.
func events(srt string) []event {
	list := []event{}
	for _, block := range strings.Split(strings.ReplaceAll(srt, "\r\n", "\n"), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		for i, line := range lines {
			times := clock.FindAllStringSubmatch(line, 2)
			if !strings.Contains(line, "-->") || len(times) != 2 {
				continue
			}
			start, ok1 := parseClock(times[0])
			end, ok2 := parseClock(times[1])
			if ok1 && ok2 {
				list = append(list, event{strconv.Itoa(len(list) + 1), strings.Join(lines[i+1:], "\n"), start, end})
			}
			break
		}
	}
	return list
}
