//go:build integration

package subtitles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// Extraction takes each format from one run over a real file, from SubRip and
// from ASS, and ASS keeps its styles.
func TestExtractFromAFile(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	srt := write("a.srt", "1\n00:00:02,000 --> 00:00:03,000\nOne\n\n2\n01:00:00,500 --> 01:00:04,000\nTwo\n")
	ass := write("b.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n"+
		"Style: Loud,Arial,48,&H0000FF00,&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Loud,,0,0,0,,Styled\n")
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3",
		"-i", srt, "-i", ass, "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:s:0", "srt", "-c:s:1", "ass", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	fromSRT, err := Extract(context.Background(), film, 1)
	if err != nil {
		t.Fatal(err)
	}
	vtt, _, _ := fromSRT.Render("vtt", 0)
	if !strings.Contains(vtt, "00:00:02.000 --> 00:00:03.000\nOne") || !strings.Contains(vtt, "01:00:00.500 --> 01:00:04.000\nTwo") {
		t.Errorf("vtt from SubRip:\n%s", vtt)
	}
	fromASS, err := Extract(context.Background(), film, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fromASS.ASS, "Style: Loud,Arial,48,&H0000FF00") || !strings.Contains(fromASS.ASS, "Styled") {
		t.Errorf("ass lost its style:\n%s", fromASS.ASS)
	}
	// A stream that isn't a subtitle, or isn't there, is an error and not an empty track.
	for _, index := range []int{0, 9} {
		if _, err := Extract(context.Background(), film, index); err == nil {
			t.Errorf("stream %d extracted", index)
		}
	}
}

// A file cut short makes ffmpeg complain and still exit 0, with a track that
// is empty or partial; that is a failure, and the whole file works again.
func TestExtractFromACutFileFails(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:02,000 --> 00:00:03,000\nOne\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=20",
		"-i", srt, "-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "srt", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	whole, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mkv")
	if err := os.WriteFile(cut, whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	if track, err := Extract(context.Background(), cut, 1); err == nil {
		t.Fatalf("a cut file extracted: %+v", track)
	}
	if _, err := Extract(context.Background(), film, 1); err != nil {
		t.Fatalf("the whole file: %v", err)
	}
}

// A subtitle file is converted whatever its character set: UTF-8 as it is,
// UTF-16 with or without a byte order mark, and the legacy sets that releases
// come in through the character set chardet finds, converted to UTF-8 here. A
// file no found set decodes is ErrBadFile.
func TestExtractFileReadsTheCharacterSet(t *testing.T) {
	testenv.FFmpeg(t)
	const head = "1\n00:00:02,000 --> 00:00:03,000\n"
	utf16 := func(bom bool, text string) []byte {
		var data []byte
		if bom {
			data = []byte{0xFF, 0xFE}
		}
		for _, r := range text {
			data = append(data, byte(r), byte(r>>8))
		}
		return data
	}
	russian := "\u041f\u0440\u0438\u0432\u0435\u0442, \u043a\u0430\u043a \u0434\u0435\u043b\u0430? \u042f \u0434\u0430\u0432\u043d\u043e \u0442\u0435\u0431\u044f \u043d\u0435 \u0432\u0438\u0434\u0435\u043b."
	jp := "\u3053\u3093\u306b\u3061\u306f\u3001\u304a\u5143\u6c17\u3067\u3059\u304b\uff1f\u305a\u3063\u3068\u63a2\u3057\u3066\u3044\u307e\u3057\u305f\u3002"
	const cue = "caf\u00e9 \u00e0 la cr\u00e8me"
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"utf-8":                        {[]byte(head + cue + "\n"), cue},
		"utf-8 with a byte order mark": {append([]byte{0xEF, 0xBB, 0xBF}, head+cue+"\n"...), cue},
		"windows-1252":                 {mustEncode(t, charmap.Windows1252, head+cue+"\n"), cue},
		"windows-1251":                 {mustEncode(t, charmap.Windows1251, head+russian+"\n"), russian},
		"shift_jis":                    {mustEncode(t, japanese.ShiftJIS, head+jp+"\n"), jp},
		"utf-16 with a mark":           {utf16(true, head+cue+"\n"), cue},
		"utf-16 without a mark":        {utf16(false, head+jp+"\n"), jp},
	} {
		track, err := ExtractFile(context.Background(), bytes.NewReader(c.data), ".srt", "")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if vtt, _, _ := track.Render("vtt", 0); !strings.Contains(vtt, "00:00:02.000 --> 00:00:03.000\n"+c.want) {
			t.Errorf("%s:\n%s", name, vtt)
		}
	}
	// Icelandic in Windows-1252, which an earlier hand-tuned detector read as Turkish.
	const icelandic = "\u00deetta er \u00fd\u00f0ing \u00e1 \u00edslensku, me\u00f0 fallegum or\u00f0um."
	track, err := ExtractFile(context.Background(), bytes.NewReader(mustEncode(t, charmap.Windows1252, head+icelandic+"\n")), ".srt", "")
	if vtt, _, _ := track.Render("vtt", 0); err != nil || !strings.Contains(vtt, icelandic) {
		t.Errorf("Icelandic: %v\n%s", err, vtt)
	}
	ass, err := ExtractFile(context.Background(), strings.NewReader("[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize\nStyle: Loud,Arial,48\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Loud,,0,0,0,,Styled\n"), ".ssa", "")
	if err != nil || !strings.Contains(ass.ASS, "Styled") {
		t.Errorf("ssa: %v %+v", err, ass)
	}
	vtt, err := ExtractFile(context.Background(), strings.NewReader("WEBVTT\n\n00:00:02.000 --> 00:00:03.000\nvtt cue\n"), ".vtt", "")
	if err != nil || !strings.Contains(vtt.SRT, "vtt cue") {
		t.Errorf("vtt: %v %+v", err, vtt)
	}
	// What can't be read is the file's fault, which is what ErrBadFile says:
	// garbage ffmpeg rejects, a file over the limit, and bytes no detected set decodes.
	for name, body := range map[string]io.Reader{
		"garbage":    strings.NewReader("this is not a subtitle file\x00\x01"),
		"over limit": io.LimitReader(zeros{}, MaxFile+10),
		"not text":   strings.NewReader(head + "\x81\x8d\x8f\x90\x9d\xff\xfe\n"),
	} {
		if _, err := ExtractFile(context.Background(), body, ".srt", ""); !errors.Is(err, ErrBadFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A failure that is not the file's is not ErrBadFile: the same file may
	// well be fine at the next try.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractFile(cancelled, strings.NewReader(head+"x\n"), ".srt", ""); err == nil || errors.Is(err, ErrBadFile) {
		t.Errorf("a cancelled read: %v", err)
	}
}

// Stray control bytes never make a file unreadable or lose its cues (ffmpeg
// refuses a file that starts with NUL or ESC, and drops every cue from a NUL
// on), and UTF-8 with a bad byte or a cut last character stays UTF-8.
func TestExtractFileToleratesStrayBytes(t *testing.T) {
	testenv.FFmpeg(t)
	const first = "1\n00:00:02,000 --> 00:00:03,000\n\u041f\u0440\u0438\u0432\u0435\u0442, \u043a\u0430\u043a \u0434\u0435\u043b\u0430?\n\n"
	const second = "2\n00:00:05,000 --> 00:00:06,000\n\u042f \u0434\u0430\u0432\u043d\u043e \u0442\u0435\u0431\u044f \u043d\u0435 \u0432\u0438\u0434\u0435\u043b.\n"
	const jp = "\u3053\u3093\u306b\u3061\u306f\u3001\u304a\u5143\u6c17\u3067\u3059\u304b\uff1f\u305a\u3063\u3068\u63a2\u3057\u3066\u3044\u307e\u3057\u305f\u3002"
	const jpFile = "1\n00:00:02,000 --> 00:00:03,000\n" + jp + "\n"
	sjis := string(mustEncode(t, japanese.ShiftJIS, jpFile))
	for name, c := range map[string]struct{ data, want string }{
		"NUL first":              {"\x00" + first + second, "\u0434\u0430\u0432\u043d\u043e"},
		"ESC first":              {"\x1b" + first + second, "\u0434\u0430\u0432\u043d\u043e"},
		"BOM then NUL":           {"\xEF\xBB\xBF\x00" + first + second, "\u0434\u0430\u0432\u043d\u043e"},
		"NUL in the first cue":   {strings.Replace(first, "\u043a\u0430\u043a", "\u043a\x00\u0430\u043a", 1) + second, "\u0434\u0430\u0432\u043d\u043e"},
		"VT and FF":              {"\x0b" + first + "\x0c" + second, "\u0434\u0430\u0432\u043d\u043e"},
		"Shift_JIS, NUL first":   {"\x00" + sjis, jp},
		"cut inside a character": {first + second[:len(second)-2], "\u043d\u0435 \u0432\u0438\u0434\u0435"},
		"stray Latin-1 byte":     {jpFile[:len(jpFile)-1] + "\xe9\n", jp},
		"cut Japanese":           {jpFile[:len(jpFile)-2], jp[:len(jp)-3]},
	} {
		track, err := ExtractFile(context.Background(), strings.NewReader(c.data), ".srt", "")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if vtt, _, _ := track.Render("vtt", 0); !strings.Contains(vtt, c.want) {
			t.Errorf("%s:\n%s", name, vtt)
		}
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// The fonts of a Matroska file are put in the asset store each from its own
// file under the hash of its bytes, with the object that lists them written
// last; an attachment that is not a font is left alone, one over the cap is
// left out with the rest kept, and the same font attached to another film is
// not stored again.
func TestFillFontsPutsTheFontsInTheStore(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	write := func(name string, size int) string {
		path := filepath.Join(dir, name)
		data := bytes.Repeat([]byte(name+"."), size)
		if err := os.WriteFile(path, data[:size], 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	regular, bold, notes := write("Regular.ttf", 3000), write("Bold.otf", 5000), write("notes.txt", 100)
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-c:v", "libx264", "-preset", "ultrafast",
		"-attach", notes, "-attach", regular, "-attach", bold,
		"-metadata:s:t:0", "mimetype=text/plain", "-metadata:s:t:1", "mimetype=font/ttf", "-metadata:s:t:2", "mimetype=font/otf", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	media, assets := store.NewPosix(dir), store.NewPosix(t.TempDir())
	src := items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Streams: []byte(`[
		{"Index": 0, "Type": "Video"},
		{"Index": 1, "Type": "Attachment", "FileName": "notes.txt", "MimeType": "text/plain"},
		{"Index": 2, "Type": "Attachment", "FileName": "Regular.ttf", "MimeType": "font/ttf"},
		{"Index": 3, "Type": "Attachment", "FileName": "Bold.otf", "MimeType": "font/otf"}]`)}
	ctx := context.Background()
	if _, found, _ := ReadFonts(ctx, assets, src); found {
		t.Fatal("done before it began")
	}
	if err := FillFonts(ctx, src, media, assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	read := func(key string) string {
		body, err := assets.Open(ctx, key, 0)
		if err != nil {
			return err.Error()
		}
		defer body.Close()
		data, _ := io.ReadAll(body)
		return fmt.Sprintf("%d:%s", len(data), data[:min(len(data), 9)])
	}
	fonts, found, err := ReadFonts(ctx, assets, src)
	if err != nil || !found || len(fonts) != 2 {
		t.Fatalf("fonts %v %v %v", fonts, found, err)
	}
	if got := read(FontKey(fonts[2])); got != "3000:Regular.t" {
		t.Errorf("regular: %s", got)
	}
	if got := read(FontKey(fonts[3])); got != "5000:Bold.otf." {
		t.Errorf("bold: %s", got)
	}
	if fonts[1] != "" {
		t.Error("an attachment that is no font was kept")
	}
	objects := func() int { all, _ := assets.List(ctx, "fonts/"); return len(all) }
	if objects() != 2 {
		t.Errorf("%d font objects", objects())
	}
	// Another film, or another version of this one, with the same fonts.
	other := src
	other.ID = "episode2"
	if err := FillFonts(ctx, other, media, assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if again, found, _ := ReadFonts(ctx, assets, other); !found || again[2] != fonts[2] || objects() != 2 {
		t.Errorf("the same fonts of another film: %v, %d font objects", again, objects())
	}
	if got := Fonts(src); len(got) != 2 || got[2] != "font/ttf" || got[3] != "font/otf" {
		t.Errorf("fonts: %v", got)
	}

	// A font over the cap is left out and the others kept.
	t.Cleanup(func(old int64) func() { return func() { MaxFont = old } }(MaxFont))
	MaxFont = 4000
	capped := src
	capped.ETag = "v2"
	if err := FillFonts(ctx, capped, media, assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	small, _, _ := ReadFonts(ctx, assets, capped)
	if _, kept := small[3]; kept || small[2] != fonts[2] {
		t.Errorf("under the cap: %v", small)
	}
	missing := src
	missing.Key = "missing.mkv"
	if err := FillFonts(ctx, missing, media, assets, 0, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("a missing file gave fonts")
	}
}

// remote is a store with no filesystem path, as S3 has, so the film reaches
// ffmpeg through a read of the store, which fails after cutAt bytes.
type remote struct {
	store.Store
	cutAt int
}

func (remote) LocalPath(string) (string, bool) { return "", false }

func (r remote) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := r.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	return &cutBody{ReadCloser: body, left: int(max(int64(r.cutAt)-offset, 0))}, nil
}

type cutBody struct {
	io.ReadCloser
	left int
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("the store went away")
	}
	n, err := c.ReadCloser.Read(p[:min(len(p), c.left)])
	c.left -= n
	return n, err
}

// A store that fails while ffmpeg reads the film makes ffmpeg exit with an
// error as a film it can't parse does, but says nothing of the film: the fonts
// job fails to be tried again, and no list of fonts is kept.
func TestFillFontsStoreFailingMidReadIsNotRemembered(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	font := filepath.Join(dir, "Regular.ttf")
	if err := os.WriteFile(font, bytes.Repeat([]byte("font."), 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-c:v", "libx264", "-preset", "ultrafast",
		"-attach", font, "-metadata:s:t:0", "mimetype=font/ttf", filepath.Join(dir, "film.mkv")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	src := items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Size: info.Size(), Streams: []byte(`[
		{"Index": 0, "Type": "Video"},
		{"Index": 1, "Type": "Attachment", "FileName": "Regular.ttf", "MimeType": "font/ttf"}]`)}
	assets := store.NewPosix(t.TempDir())
	ctx := context.Background()
	if err := FillFonts(ctx, src, remote{store.NewPosix(dir), 3000}, assets, 0, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a film cut short in the store gave fonts")
	}
	if _, found, _ := ReadFonts(ctx, assets, src); found {
		t.Error("a store failure was remembered as a film with no fonts")
	}
	// The same store whole gives the fonts.
	if err := FillFonts(ctx, src, remote{store.NewPosix(dir), 1 << 30}, assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if fonts, found, _ := ReadFonts(ctx, assets, src); !found || len(fonts) != 1 {
		t.Errorf("fonts %v %v", fonts, found)
	}
}

// A store that stops answering while a track is read out of the film is the
// job's failure, not the film's: no ErrBadFile, so the track is not given up on.
func TestFillStoreFailingMidReadIsNotTheFilesFault(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	srt := filepath.Join(dir, "a.srt")
	var cues strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&cues, "%d\n00:00:%02d,000 --> 00:00:%02d,500\nLine %d\n\n", i, i%60, i%60, i)
	}
	if err := os.WriteFile(srt, []byte(cues.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-i", srt,
		"-map", "0", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "srt", filepath.Join(dir, "film.mkv")).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	src := items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Size: info.Size(), Streams: []byte(`[{"Index": 0, "Type": "Video"}, {"Index": 1, "Type": "Subtitle", "Codec": "subrip"}]`)}
	assets := store.NewPosix(t.TempDir())
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	for _, cut := range []int64{info.Size() / 3, info.Size() * 2 / 3} {
		err := Fill(ctx, src, remote{store.NewPosix(dir), int(cut)}, assets, 1, 0, log)
		if err == nil || errors.Is(err, ErrBadFile) {
			t.Errorf("cut at %d of %d: %v", cut, info.Size(), err)
		}
	}
	if err := Fill(ctx, src, remote{store.NewPosix(dir), 1 << 30}, assets, 1, 0, log); err != nil {
		t.Errorf("the whole film: %v", err)
	}
}

// An attachment with no data is listed by the scan but ffmpeg exits 0 and writes
// no file for it. That font is left out like one over the cap, the rest are
// kept, and the manifest is still written.
func TestFillFontsSkipsAFontFfmpegDidNotWrite(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	regular, empty := filepath.Join(dir, "Regular.ttf"), filepath.Join(dir, "Empty.ttf")
	for path, size := range map[string]int{regular: 3000, empty: 2} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	film := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-c:v", "libx264", "-preset", "ultrafast",
		"-attach", regular, "-attach", empty, "-metadata:s:t:0", "mimetype=font/ttf", "-metadata:s:t:1", "mimetype=font/ttf", film).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	// The second attachment's data is two bytes (EBML size 0x82); give it none
	// (size 0x80) and a Void element of the same length for the other two.
	data, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data, []byte{0x46, 0x5c, 0x82, 'x', 'x'})
	if at < 0 {
		t.Fatal("the second attachment's data is not in the file")
	}
	copy(data[at:], []byte{0x46, 0x5c, 0x80, 0xec, 0x80})
	if err := os.WriteFile(film, data, 0o644); err != nil {
		t.Fatal(err)
	}
	media, assets := store.NewPosix(dir), store.NewPosix(t.TempDir())
	src := items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Streams: []byte(`[
		{"Index": 0, "Type": "Video"},
		{"Index": 1, "Type": "Attachment", "FileName": "Regular.ttf", "MimeType": "font/ttf"},
		{"Index": 2, "Type": "Attachment", "FileName": "Empty.ttf", "MimeType": "font/ttf"}]`)}
	ctx := context.Background()
	if err := FillFonts(ctx, src, media, assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("a font that was not written failed the job: %v", err)
	}
	fonts, found, err := ReadFonts(ctx, assets, src)
	if err != nil || !found || len(fonts) != 1 || fonts[1] == "" {
		t.Errorf("fonts %v %v %v", fonts, found, err)
	}
}

// A text track ffmpeg refuses (here SubRip that is not UTF-8, which it can't
// decode to text) is left out of the film's pass, and the other text tracks are
// stored all the same.
func TestFillTextLeavesOutATrackFFmpegRefuses(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.srt", []byte("1\n00:00:02,000 --> 00:00:03,000\nGood\n"))
	bad := write("bad.srt", []byte("1\n00:00:02,000 --> 00:00:03,000\n\xff\xfe\xfd caf\xe9\n"))
	other := write("other.srt", []byte("1\n00:00:02,000 --> 00:00:03,000\nOther\n"))
	args := []string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3"}
	for _, srt := range []string{good, bad, other} {
		args = append(args, "-f", "srt", "-i", srt)
	}
	args = append(args, "-map", "0", "-map", "1", "-map", "2", "-map", "3", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "copy", filepath.Join(dir, "film.mkv"))
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		// Copying is what keeps the bad bytes; if ffmpeg won't build the film the premise is gone.
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	streams := `[{"Index": 0, "Type": "Video"}`
	for i := 1; i <= 3; i++ {
		streams += fmt.Sprintf(`, {"Index": %d, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true}`, i)
	}
	src := items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Size: info.Size(), Streams: []byte(streams + "]")}
	assets := store.NewPosix(t.TempDir())
	ctx := context.Background()
	if _, err := ExtractAll(ctx, filepath.Join(dir, "film.mkv"), []int{1, 2, 3}); !errors.Is(err, ErrBadFile) {
		t.Fatalf("the film's pass over all three: %v; want ErrBadFile, or this test proves nothing", err)
	}
	if err := FillText(ctx, src, store.NewPosix(dir), assets, 0, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("FillText: %v", err)
	}
	for index, want := range map[int]bool{1: true, 2: false, 3: true} {
		_, found, err := Read(ctx, assets, Key(src, index))
		if err != nil || found != want {
			t.Errorf("track %d: stored %v, err %v; want stored %v", index, found, err, want)
		}
	}
}
