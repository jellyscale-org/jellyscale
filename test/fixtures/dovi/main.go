// Command dovi makes a synthetic HDR film (HDR10, HDR10+, HLG, SDR, with or
// without a Dolby Vision profile on top) for test/fixtures/e2e.sh. See
// testenv.HDRFilm for what is and is not in the file.
// Usage: dovi <out.mp4> <seconds> <pq|hlg|sdr|av1> [hdr10plus] [dv=<profile>.<bl_compatibility_id>[.el]]
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
		fmt.Fprintln(os.Stderr, "usage: dovi <out.mp4> <seconds> <pq|hlg|sdr|av1> [hdr10plus] [dv=<profile>.<bl_compatibility_id>[.el]]")
		os.Exit(2)
	}
	seconds, _ := strconv.Atoi(os.Args[2])
	film := testenv.HDRFilm{Base: os.Args[3], Seconds: seconds}
	for _, arg := range os.Args[4:] {
		if arg == "hdr10plus" {
			film.HDR10Plus = true
		} else if dv, ok := strings.CutPrefix(arg, "dv="); ok {
			var el string
			if n, _ := fmt.Sscanf(dv, "%d.%d.%s", &film.DVProfile, &film.DVCompat, &el); n < 2 || film.DVProfile == 0 {
				fmt.Fprintln(os.Stderr, "dovi: bad", arg)
				os.Exit(2)
			}
			film.DVEnhancement = el == "el"
		} else {
			fmt.Fprintln(os.Stderr, "dovi: unknown argument", arg)
			os.Exit(2)
		}
	}
	if err := testenv.MakeHDRFilm(os.Args[1], film); err != nil {
		fmt.Fprintln(os.Stderr, "dovi:", err)
		os.Exit(1)
	}
}
