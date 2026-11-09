// Command vobsub writes a VobSub .idx and .sub pair of yellow rectangles, for test/fixtures/make.sh.
// Usage: vobsub <out> <canvas_width> <canvas_height> <start,end,x,y,w,h>...   (writes out.idx and out.sub)
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"jellyscale/internal/testenv"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: vobsub <out> <canvas_width> <canvas_height> <start,end,x,y,w,h>...")
		os.Exit(2)
	}
	width, _ := strconv.Atoi(os.Args[2])
	height, _ := strconv.Atoi(os.Args[3])
	var cues []testenv.Cue
	for _, arg := range os.Args[4:] {
		f := strings.Split(arg, ",")
		if len(f) != 6 {
			fmt.Fprintln(os.Stderr, "a cue is start,end,x,y,w,h:", arg)
			os.Exit(2)
		}
		var cue testenv.Cue
		cue.Start, _ = strconv.ParseFloat(f[0], 64)
		cue.End, _ = strconv.ParseFloat(f[1], 64)
		cue.X, _ = strconv.Atoi(f[2])
		cue.Y, _ = strconv.Atoi(f[3])
		cue.W, _ = strconv.Atoi(f[4])
		cue.H, _ = strconv.Atoi(f[5])
		cues = append(cues, cue)
	}
	idx, sub := testenv.VobSub(width, height, cues...)
	for ext, data := range map[string][]byte{".idx": idx, ".sub": sub} {
		if err := os.WriteFile(os.Args[1]+ext, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
