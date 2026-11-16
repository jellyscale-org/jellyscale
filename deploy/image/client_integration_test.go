//go:build integration

package image

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-124: what test/fixtures/client.sh makes, read with ffprobe. They need
// ffmpeg, Go and a serif font, as the script does.

var (
	madeOnce sync.Once
	madeDir  string
	madeOut  string
	madeErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if madeDir != "" {
		os.RemoveAll(madeDir) //nolint:errcheck
	}
	os.Exit(code)
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true, ".m2ts": true, ".mov": true, ".webm": true}

// made runs client.sh once for the whole package.
func made(t *testing.T) string {
	t.Helper()
	testenv.FFmpeg(t)
	testenv.Font(t)
	madeOnce.Do(func() {
		madeDir, madeErr = os.MkdirTemp("", "js124-client-")
		if madeErr != nil {
			return
		}
		cmd := exec.Command("bash", "test/fixtures/client.sh", madeDir)
		cmd.Dir = root(t)
		out, err := cmd.CombinedOutput()
		madeOut = string(out)
		if err != nil {
			madeErr = fmt.Errorf("test/fixtures/client.sh: %v\n%s", err, out)
		}
	})
	if madeErr != nil {
		t.Fatal(madeErr)
	}
	return madeDir
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func videos(list []string) []string {
	var out []string
	for _, f := range list {
		if videoExt[strings.ToLower(filepath.Ext(f))] {
			out = append(out, f)
		}
	}
	return out
}

func TestClientShMakesTwentyThreeFilmsAndThreeEpisodesAndNothingElseThatPlays(t *testing.T) {
	dir := made(t)
	vids := videos(files(t, dir))
	var episodes, films []string
	for _, v := range vids {
		if regexp.MustCompile(`S\d\dE\d\d`).MatchString(v) {
			episodes = append(episodes, v)
		} else {
			films = append(films, v)
		}
	}
	if len(films) != 23 || len(episodes) != 3 || len(vids) != 26 {
		t.Errorf("%d films and %d episodes (%d video files), want 23, 3 and 26:\n%s", len(films), len(episodes), len(vids), strings.Join(vids, "\n"))
	}
	seasons := map[string]bool{}
	for _, e := range episodes {
		seasons[regexp.MustCompile(`S\d\d`).FindString(e)] = true
	}
	if len(seasons) != 2 {
		t.Errorf("the episodes are in %d seasons, want 2 (one series)", len(seasons))
	}
	// Every video's name is a recipe's: it appears in a script of test/fixtures.
	var recipes strings.Builder
	scripts, _ := filepath.Glob(filepath.Join(root(t), "test", "fixtures", "*.sh"))
	for _, s := range scripts {
		raw, _ := os.ReadFile(s)
		recipes.Write(raw)
	}
	for _, v := range vids {
		stem := strings.TrimSuffix(filepath.Base(v), filepath.Ext(v))
		if !strings.Contains(recipes.String(), stem) {
			t.Errorf("%s is named by no recipe in test/fixtures", v)
		}
	}
}

func TestClientShFilesAreUnder300MB(t *testing.T) {
	dir := made(t)
	var total int64
	for _, f := range files(t, dir) {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	t.Logf("the library is %d bytes", total)
	if total >= 300_000_000 {
		t.Errorf("%d bytes, want under 300,000,000", total)
	}
}

type stream struct {
	Index     int    `json:"index"`
	Codec     string `json:"codec_name"`
	Type      string `json:"codec_type"`
	Tags      map[string]string
	Dispo     map[string]int `json:"disposition"`
	hasLang   bool
	lang      string
	isDefault bool
}

func probe(t *testing.T, path string) []stream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var doc struct{ Streams []stream }
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for i := range doc.Streams {
		s := &doc.Streams[i]
		s.lang, s.hasLang = s.Tags["language"]
		s.isDefault = s.Dispo["default"] == 1
	}
	return doc.Streams
}

func find(t *testing.T, dir, name string) string {
	t.Helper()
	var hits []string
	for _, f := range files(t, dir) {
		if filepath.Base(f) == name {
			hits = append(hits, f)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("%d files named %q under %s, want one: %v", len(hits), name, dir, hits)
	}
	return filepath.Join(dir, hits[0])
}

var textCodecs = map[string]bool{"subrip": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true}

func checkBabel(t *testing.T, film string) {
	t.Helper()
	s := probe(t, film)
	if len(s) != 13 {
		t.Fatalf("Babel Film has %d streams, want 13", len(s))
	}
	type want struct {
		typ, lang   string // lang "" means no language tag
		forced, sdh bool
		def         bool
	}
	wants := []want{
		{"video", "", false, false, false},
		{"audio", "eng", false, false, true}, {"audio", "deu", false, false, false}, {"audio", "fra", false, false, false}, {"audio", "", false, false, false},
		{"subtitle", "eng", false, false, false}, {"subtitle", "eng", true, false, false}, {"subtitle", "eng", false, true, false},
		{"subtitle", "deu", false, false, false}, {"subtitle", "ger", true, false, false}, {"subtitle", "fra", false, false, false},
		{"subtitle", "", false, false, false}, {"subtitle", "jpn", false, false, false},
	}
	for i, w := range wants {
		g := s[i]
		if g.Type != w.typ || g.lang != w.lang || (w.lang == "" && g.hasLang) {
			t.Errorf("stream %d is %s %q (tag present %v), want %s %q", i, g.Type, g.lang, g.hasLang, w.typ, w.lang)
		}
		if w.typ == "audio" && g.isDefault != w.def {
			t.Errorf("audio stream %d default is %v, want %v (only 1 is the default audio)", i, g.isDefault, w.def)
		}
		if w.typ == "subtitle" {
			if (g.Dispo["forced"] == 1) != w.forced {
				t.Errorf("subtitle %d forced is %v, want %v", i, g.Dispo["forced"] == 1, w.forced)
			}
			if i >= 5 && i <= 11 && !textCodecs[g.Codec] {
				t.Errorf("subtitle %d is %s, want a text codec", i, g.Codec)
			}
		}
	}
	if (s[7].Dispo["hearing_impaired"] != 1) || s[5].Dispo["hearing_impaired"] == 1 {
		t.Errorf("only 7 is marked SDH (hearing_impaired): 5 is %d, 7 is %d", s[5].Dispo["hearing_impaired"], s[7].Dispo["hearing_impaired"])
	}
	if s[12].Codec != "hdmv_pgs_subtitle" {
		t.Errorf("subtitle 12 is %s, want hdmv_pgs_subtitle", s[12].Codec)
	}
	beside, _ := os.ReadDir(filepath.Dir(film))
	var names []string
	for _, e := range beside {
		if e.Name() != filepath.Base(film) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if strings.Join(names, " ") != "Babel Film.de.srt Babel Film.es.srt" {
		t.Errorf("beside Babel Film: %v, want exactly Babel Film.de.srt and Babel Film.es.srt", names)
	}
}

func checkKino(t *testing.T, film string) {
	t.Helper()
	s := probe(t, film)
	if len(s) != 6 {
		t.Fatalf("Kino Film has %d streams, want 6", len(s))
	}
	for i, w := range []struct{ typ, lang string }{{"video", ""}, {"audio", "deu"}, {"audio", "eng"}, {"subtitle", "deu"}, {"subtitle", "eng"}, {"subtitle", "fra"}} {
		if s[i].Type != w.typ || s[i].lang != w.lang {
			t.Errorf("stream %d is %s %q, want %s %q", i, s[i].Type, s[i].lang, w.typ, w.lang)
		}
		if w.typ == "subtitle" && !textCodecs[s[i].Codec] {
			t.Errorf("subtitle %d is %s, want text", i, s[i].Codec)
		}
	}
	if !s[1].isDefault || s[2].isDefault {
		t.Errorf("audio default: 1 is %v and 2 is %v, want true and false", s[1].isDefault, s[2].isDefault)
	}
	beside, _ := os.ReadDir(filepath.Dir(film))
	if len(beside) != 1 {
		t.Errorf("%d files in Kino Film's folder, want only the film", len(beside))
	}
}

func TestClientShMakesBabelFilmAsTheCriteriaListIt(t *testing.T) {
	checkBabel(t, find(t, made(t), "Babel Film.mkv"))
}

func TestClientShMakesKinoFilmAsTheCriteriaListIt(t *testing.T) {
	checkKino(t, find(t, made(t), "Kino Film.mkv"))
}

// Each language film has one recipe that a check can run without the other
// twenty-one films, and client.sh uses the same.
func TestBabelAndKinoEachComeFromOneRecipeThatRunsAlone(t *testing.T) {
	testenv.FFmpeg(t)
	for name, check := range map[string]func(*testing.T, string){"babel.sh": checkBabel, "kino.sh": checkKino} {
		dir := t.TempDir()
		cmd := exec.Command("bash", filepath.Join("test", "fixtures", name), dir)
		cmd.Dir = root(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("test/fixtures/%s: %v\n%s", name, err, out)
		}
		film := "Babel Film.mkv"
		if name == "kino.sh" {
			film = "Kino Film.mkv"
		}
		check(t, find(t, dir, film))
		if got := videos(files(t, dir)); len(got) != 1 {
			t.Errorf("%s made %d videos, want only its film: %v", name, len(got), got)
		}
	}
}

func TestClientShRunsMakeShForItsFilmsAndWritesNoFfmpegCommandOfItsOwn(t *testing.T) {
	raw := string(must(os.ReadFile(filepath.Join(root(t), "test", "fixtures", "client.sh"))))
	if !strings.Contains(raw, "make.sh") {
		t.Error("client.sh does not run make.sh")
	}
	if regexp.MustCompile(`(^|[^\w.-])ffmpeg\s+-`).MatchString(raw) {
		t.Error("client.sh has an ffmpeg command of its own: each film has one recipe")
	}
	// No other script in test/ carries the Babel or Kino recipe.
	for _, pattern := range []string{"test/*.sh", "test/fixtures/*.sh", "test/e2e/*.sh", "test/differential/*.sh"} {
		list, _ := filepath.Glob(filepath.Join(root(t), pattern))
		for _, f := range list {
			base := filepath.Base(f)
			b := string(must(os.ReadFile(f)))
			if base != "babel.sh" && base != "scripts_test.sh" && strings.Contains(b, "Babel") && strings.Contains(b, "-metadata:s:s") {
				t.Errorf("%s carries a Babel Film ffmpeg command beside babel.sh", f)
			}
			if base != "kino.sh" && base != "scripts_test.sh" && strings.Contains(b, "Kino") && strings.Contains(b, "-metadata:s:s") {
				t.Errorf("%s carries a Kino Film ffmpeg command beside kino.sh", f)
			}
		}
	}
}

// make.sh, posix.sh and e2e.sh make what they made: client.sh's 21 films and 3
// episodes are the files make.sh makes alone.
func TestMakeShStillMakesTwentyOneFilmsAndThreeEpisodes(t *testing.T) {
	dir := made(t)
	alone := t.TempDir()
	cmd := exec.Command("bash", "test/fixtures/make.sh", alone)
	cmd.Dir = root(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make.sh: %v\n%s", err, out)
	}
	want := files(t, alone)
	if n := len(videos(want)); n != 24 {
		t.Errorf("make.sh makes %d videos, want 21 films and 3 episodes", n)
	}
	have := map[string]bool{}
	for _, f := range files(t, dir) {
		have[f] = true
	}
	for _, f := range want {
		if !have[f] {
			t.Errorf("client.sh's library lacks %s, which make.sh makes", f)
		}
	}
	if len(have) != len(want)+4 { // Babel Film: its film and two files; Kino Film: its film
		t.Errorf("client.sh makes %d files, make.sh's %d plus Babel's 3 and Kino's 1 is %d", len(have), len(want), len(want)+4)
	}
}
