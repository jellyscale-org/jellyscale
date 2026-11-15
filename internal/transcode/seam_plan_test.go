package transcode

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// JS-113 pulls the seam arithmetic (where the video and audio seeks fall and
// which audio packets are dropped) out of Command into a function with no I/O,
// and gives Command an encoder description. These checks name what the task
// implies, so they do not compile until it is built:
//
//	type encoder struct {
//		videoArgs     []string // -c:v libx264 -preset veryfast -profile:v high -level 51
//		params        string   // the value of -x264-params
//		reorderFrames int      // frames the encoder holds back before a keyframe
//	}
//	var x264 encoder
//	func seamPlan(enc encoder, base, frameDuration, keyframe float64, grid int64, copiedAudio bool, segment int) (videoSeek, audioSeek float64, drop string)
//	func Command(ctx, input, spec, start, dir, ass, image, enc encoder)
//
// grid is the first AAC packet's time in samples (ignored for copied audio).
// The expected values below are literals, taken from today's arithmetic.

const (
	ntsc = 1001.0 / 24000 // 24000/1001 fps, as sourceTiming divides it
	pal  = 1.0 / 25
)

type seamRow struct {
	name       string
	base, fd   float64
	grid       int64
	segment    int
	keyframe   float64
	video, aac string // The seeks as seconds() writes them
	n          int64  // The drop is noise=drop=lt(pts\,n)
}

var seamRows = []seamRow{
	// A source that starts at 0 (grid -1024).
	{"ntsc segment 1", 0, ntsc, -1024, 1, 4.004, "3.993573", "3.818667", 188416},
	{"ntsc segment 5", 0, ntsc, -1024, 5, 20.02, "20.009573", "19.840000", 957440},
	{"ntsc segment 9", 0, ntsc, -1024, 9, 36.036, "36.025573", "35.861333", 1726464},
	{"pal segment 1", 0, pal, -1024, 1, 4, "3.990000", "3.818667", 188416},
	{"pal segment 5", 0, pal, -1024, 5, 20, "19.990000", "19.818667", 956416},
	{"pal segment 9", 0, pal, -1024, 9, 36, "35.990000", "35.818667", 1724416},
	// 7.5 s is not a whole number of frames: the keyframe's frame number is
	// rounded, not truncated, and the start is in every seek.
	{"shifted segment 1", 7.5, ntsc, 358976, 1, 11.504, "3.993573", "3.840000", 549440},
	{"shifted segment 5", 7.5, ntsc, 358976, 5, 27.52, "20.009573", "19.840000", 1317440},
	{"shifted segment 9", 7.5, ntsc, 358976, 9, 43.536, "36.025573", "35.861333", 2086464},
	// A negative start moves the audio seeks and not the video's.
	{"negative start segment 1", -0.5, ntsc, -25024, 1, 4.004, "3.993573", "4.330667", 188992},
	{"negative start segment 5", -0.5, ntsc, -25024, 5, 20.02, "20.009573", "20.330667", 956992},
	{"negative start segment 9", -0.5, ntsc, -25024, 9, 36.036, "36.025573", "36.352000", 1726016},
	// The audio seek is never before the file's start, and a packet on the cut
	// is kept: the cut is at 0.
	// A packet exactly on the cut is kept, and the audio seek's floor and the
	// drop's ceil each carry a rounding slack (1e-9 and 1e-6) that an on-the-cut
	// row in exact floats cannot see. Both rows are today's values: without the
	// slack, 88 min in drops 254206976 (the packet on the cut is lost) and the
	// second row's audio seek is 1367.843667.
	{"88 min in, the packet on the cut is kept", 0, ntsc, -1024, 1324, 5296.04075, "5296.030323", "5295.872000", 254205952},
	{"a start of 0.083 s, the audio seek's floor", 0.083, ntsc, 358976, 342, 1368.116333, "1368.022906", "1367.865000", 65665600},
	{"2 s frames, the cut at 0", 0, 2, -1024, 1, 4, "3.500000", "0.000000", 0},
}

func TestSeamPlanValuesOfEachSource(t *testing.T) {
	for _, r := range seamRows {
		t.Run(r.name, func(t *testing.T) {
			video, aac, drop := seamPlan(x264, r.base, r.fd, r.keyframe, r.grid, false, r.segment)
			if got := seconds(video); got != r.video {
				t.Errorf("video seek %s, want %s", got, r.video)
			}
			if got := seconds(aac); got != r.aac {
				t.Errorf("audio seek %s, want %s", got, r.aac)
			}
			if want := fmt.Sprintf(`noise=drop=lt(pts\,%d)`, r.n); drop != want {
				t.Errorf("drop %s, want %s", drop, want)
			}
		})
	}
}

// A copied track is the source's own packets: no grid, an audio seek 2 s
// before the segment, and a drop in seconds.
func TestSeamPlanForCopiedAudio(t *testing.T) {
	for _, tc := range []struct {
		name     string
		base     float64
		segment  int
		keyframe float64
		video    string
		aac      string
		drop     string
	}{
		{"at 0, segment 1", 0, 1, 4.004, "3.993573", "2.000000", "3.920582"},
		{"at 0, segment 5", 0, 5, 20.02, "20.009573", "18.000000", "19.936582"},
		{"at 0, segment 9", 0, 9, 36.036, "36.025573", "34.000000", "35.952582"},
		{"shifted, segment 1", 7.5, 1, 11.504, "3.993573", "2.000000", "11.428082"},
		{"shifted, segment 5", 7.5, 5, 27.52, "20.009573", "18.000000", "27.444082"},
		{"shifted, segment 9", 7.5, 9, 43.536, "36.025573", "34.000000", "43.460082"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			video, aac, drop := seamPlan(x264, tc.base, ntsc, tc.keyframe, 0, true, tc.segment)
			if got := seconds(video); got != tc.video {
				t.Errorf("video seek %s, want %s", got, tc.video)
			}
			if got := seconds(aac); got != tc.aac {
				t.Errorf("audio seek %s, want %s", got, tc.aac)
			}
			if want := `noise=drop=lt(pts*tb\,` + tc.drop + `)`; drop != want {
				t.Errorf("drop %s, want %s", drop, want)
			}
		})
	}
}

// Segment 0 is cut nowhere: whatever else is asked, the seeks are 0 and the
// text of a zero seek is "0.000000", never "-0.000000".
func TestSeamPlanForSegmentZeroCutsNothing(t *testing.T) {
	for _, base := range []float64{0, 7.5, -0.5} {
		for _, fd := range []float64{ntsc, pal, 2} {
			for _, keyframe := range []float64{0, 0.5, 4.004, 20} {
				for _, grid := range []int64{-1024, 0, 358976, -25024} {
					for _, copied := range []bool{false, true} {
						video, aac, drop := seamPlan(x264, base, fd, keyframe, grid, copied, 0)
						if seconds(video) != "0.000000" || seconds(aac) != "0.000000" || drop != "" {
							t.Fatalf("base %v frame %v keyframe %v grid %d copied %v: seeks %s, %s and drop %q, want 0, 0 and none",
								base, fd, keyframe, grid, copied, seconds(video), seconds(aac), drop)
						}
					}
				}
			}
		}
	}
}

// The plan is a function of its inputs only: asking others between two asks of
// the same changes nothing.
func TestSeamPlanRemembersNothing(t *testing.T) {
	type answer struct {
		video, aac float64
		drop       string
	}
	ask := func(r seamRow, copied bool) answer {
		v, a, d := seamPlan(x264, r.base, r.fd, r.keyframe, r.grid, copied, r.segment)
		return answer{v, a, d}
	}
	first := make([]answer, len(seamRows))
	for i, r := range seamRows {
		first[i] = ask(r, i%2 == 0)
	}
	for i := len(seamRows) - 1; i >= 0; i-- {
		ask(seamRows[(i+3)%len(seamRows)], true)
		if got := ask(seamRows[i], i%2 == 0); got != first[i] {
			t.Errorf("%s: %v, then %v", seamRows[i].name, first[i], got)
		}
	}
}

// With no ffmpeg or ffprobe to run, the plan is still made.
func TestSeamPlanNeedsNoProgram(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if p, err := exec.LookPath(name); err == nil {
			t.Fatalf("%s is on the PATH at %s", name, p)
		}
	}
	if _, _, drop := seamPlan(x264, 0, ntsc, 4.004, -1024, false, 1); drop == "" {
		t.Error("no drop for segment 1")
	}
}

// seamPlan's source: it takes no context, path, stream or reader, and starts
// and reads nothing.
func TestSeamPlanIsPureInItsSource(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var decl *ast.FuncDecl
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "seamPlan" && f.Recv == nil {
					decl = f
				}
			}
		}
	}
	if decl == nil {
		t.Fatal("no function seamPlan in the package")
	}
	for _, field := range decl.Type.Params.List {
		switch typ := types.ExprString(field.Type); typ {
		case "encoder", "float64", "int64", "int", "bool":
		default:
			t.Errorf("seamPlan takes a %s; it may take the encoder, numbers and a flag", typ)
		}
	}
	// What it may call: arithmetic, formatting and the encoder's own fields.
	allowedPackages := map[string]bool{"math": true, "fmt": true, "strconv": true}
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && !allowedPackages[id.Name] && isPackageName(id.Name) {
				t.Errorf("seamPlan uses %s.%s", id.Name, n.Sel.Name)
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && slices.Contains([]string{"audioGrid", "firstFrameAt", "sourceTiming", "cueStart", "ffprobe", "ffprobeFrom"}, id.Name) {
				t.Errorf("seamPlan calls %s", id.Name)
			}
		}
		return true
	})
}

func isPackageName(name string) bool {
	return slices.Contains([]string{"exec", "os", "io", "ioutil", "context", "filepath", "bytes", "http", "net", "time", "sync", "library"}, name)
}
