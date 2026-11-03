package subtitles

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// subtitleFile is text in enc between the ASCII timing and numbers that every
// subtitle file has, which is what the detector sees.
func subtitleFile(t *testing.T, enc encoding.Encoding, text string) []byte {
	t.Helper()
	file := append([]byte("1\n00:00:01,000 --> 00:00:03,000\n"), mustEncode(t, enc, text)...)
	return append(file, "\n\n2\n00:00:04,000 --> 00:00:06,000\nOK\n"...)
}

// Files in the sets releases come in are read as the text they hold. language is
// the one in the file's name, or empty.
func TestLegacyText(t *testing.T) {
	for name, c := range map[string]struct {
		enc      encoding.Encoding
		text     string
		language string
	}{
		"formal Shift_JIS":     {japanese.ShiftJIS, "首相官邸は、外務省と防衛省の幹部を集めて、国際情勢に関する会議を開いた。政府は、経済産業省の見解を踏まえ、対応を決定する方針だ。", ""},
		"kana Japanese":        {japanese.ShiftJIS, "こんにちは、お元気ですか？ずっとあなたを探していました。ここで何をしているのですか。", "jpn"},
		"GBK":                  {simplifiedchinese.GBK, "吃饭了吗？我想去商店买东西。你先回家吧，我马上就到。", ""},
		"GBK named zh":         {simplifiedchinese.GBK, "吃饭了吗？我想去商店买东西。你先回家吧，我马上就到。", "zh"},
		"formal GBK":           {simplifiedchinese.GBK, "国务院总理今天主持召开会议，研究部署经济社会发展的重点工作，强调要坚持稳中求进。", ""},
		"Big5":                 {traditionalchinese.Big5, "吃飯了嗎？我想去商店買東西。你先回家吧，我馬上就到。", ""},
		"Big5 named zh-tw":     {traditionalchinese.Big5, "吃飯了嗎？我想去商店買東西。你先回家吧，我馬上就到。", "zh-tw"},
		"formal Big5":          {traditionalchinese.Big5, "台北市政府表示，將持續推動捷運延伸工程，預計明年底完工通車。", ""},
		"formal Big5 named zh": {traditionalchinese.Big5, "台北市政府表示，將持續推動捷運延伸工程，預計明年底完工通車。", "zh"},
		"KOI8-R Bulgarian":     {charmap.KOI8R, "Здравей, как си? Отдавна не съм те виждал. Какво правиш тук толкова късно през нощта?", "bul"},
		"CP1251 Russian":       {charmap.Windows1251, "Привет, как дела? Я давно тебя не видел. Что ты делаешь здесь так поздно ночью?", ""},
		"CP1252 Icelandic":     {charmap.Windows1252, "Ég veit það ekki. Þetta er þýðing á íslensku, með fallegum orðum, ekki satt?", "isl"},
		"CP1252 French":        {charmap.Windows1252, "Où étais-tu hier soir ? Je t'ai cherché partout, mais tu n'étais pas à la maison.", ""},
		"CP1254 Turkish":       {charmap.Windows1254, "Bugün hava çok güzel, şimdi dışarı çıkıp biraz yürüyelim mi? Ağaçların altında oturalım.", "tur"},
		"CP1253 Greek":         {charmap.Windows1253, "Καλημέρα σας, πώς είστε σήμερα; Δεν σε έχω δει εδώ και πολύ καιρό.", ""},
		"CP1255 Hebrew":        {charmap.Windows1255, "שלום, מה שלומך היום? לא ראיתי אותך הרבה זמן, מה אתה עושה כאן בלילה?", ""},
		"CP1256 Arabic":        {charmap.Windows1256, "مرحبا، كيف حالك اليوم؟ لم أراك منذ وقت طويل، ماذا تفعل هنا في هذا الوقت المتأخر؟", ""},
	} {
		got, ok := legacyText(subtitleFile(t, c.enc, c.text), c.language)
		if !ok || !strings.Contains(string(got), c.text) {
			t.Errorf("%s: %v\n%s", name, ok, got)
		}
	}
}

// A control byte in the text is no reason to refuse a file, and a file cut in
// the middle of its last character is read up to it.
func TestLegacyTextToleratesStrayBytes(t *testing.T) {
	const text = "こんにちは、お元気ですか？ずっとあなたを探していました。ここで何をしているのですか。"
	file := subtitleFile(t, japanese.ShiftJIS, text)
	for name, c := range map[string][]byte{
		"NUL": append([]byte("\x00"), file...), "VT": append([]byte("\x0b"), file...),
		"FF": append([]byte("\x0c"), file...), "ESC": append([]byte("\x1b"), file...),
		"cut inside a character": append(mustEncode(t, japanese.ShiftJIS, "1\n00:00:01,000 --> 00:00:03,000\n"+text), mustEncode(t, japanese.ShiftJIS, "あ")[0]),
	} {
		got, ok := legacyText(c, "")
		if !ok || !strings.Contains(string(got), text) {
			t.Errorf("%s: %v\n%s", name, ok, got)
		}
	}
}

// The language in the file's name breaks a near-tie but does not override a
// clear result.
func TestLegacyTextLanguageOnlyBreaksTies(t *testing.T) {
	// Plainly Shift_JIS, whatever the name says.
	const text = "こんにちは、お元気ですか？ずっとあなたを探していました。ここで何をしているのですか。"
	if got, ok := legacyText(subtitleFile(t, japanese.ShiftJIS, text), "zho"); !ok || !strings.Contains(string(got), text) {
		t.Errorf("a clear result was overridden: %v %s", ok, got)
	}
	// Within tieMargin of each other, French as Western and as Turkish both
	// decode, and the name picks; a bare file is Western.
	french := subtitleFile(t, charmap.Windows1252, "Ça va? Où est-il? Là-bas, à côté. ý")
	for language, want := range map[string]string{"": "ý", "tur": "ı"} {
		if got, ok := legacyText(french, language); !ok || !strings.Contains(string(got), want) {
			t.Errorf("French named %q: %v\n%s", language, ok, got)
		}
	}
}

// Windows-1250 is what chardet reads as Windows-1252 (Polish, Czech) or
// Windows-1254 (Serbian Latin), and Thai (Windows-874) as a CJK set, and the
// file's name does not help, as the results are not close. README (Subtitle files and fonts beside the film) lists this as a limit, with
// the remedy of converting the file to UTF-8; this test pins the behaviour so a
// change of the detector shows.
func TestLegacyTextKnownLimit(t *testing.T) {
	for name, c := range map[string]struct{ text, language string }{
		"Serbian Latin": {"Dobar dan, kako ste? Nisam vas video već dugo. Šta radite ovde tako kasno noću, đače?", "srp"},
		"Thai":          {"สวัสดีครับ วันนี้อากาศดีมาก", "tha"},
		"Polish":        {"Dzień dobry, jak się masz? Nie widziałem cię od dawna. Co tu robisz o tak późnej porze?", "pol"},
	} {
		enc := encoding.Encoding(charmap.Windows1250)
		if name == "Thai" {
			enc = charmap.Windows874
		}
		got, ok := legacyText(subtitleFile(t, enc, c.text), c.language)
		if !ok || strings.Contains(string(got), c.text) {
			t.Errorf("%s is now read correctly, update README: %v\n%s", name, ok, got)
		}
	}
}

// Bytes that no ranked set decodes are refused.
func TestLegacyTextRefusesWhatNoSetDecodes(t *testing.T) {
	if got, ok := legacyText([]byte("1\n00:00:01,000 --> 00:00:03,000\n\x81\x8d\x8f\x90\x9d\xff\xfe\n"), ""); ok {
		t.Errorf("read as %q", got)
	}
}

func mustEncode(t *testing.T, enc encoding.Encoding, text string) []byte {
	t.Helper()
	data, err := enc.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// UTF-16 is recognised with a byte order mark, and without one by every other
// byte being zero.
func TestUTF16Text(t *testing.T) {
	const text = "1\n00:00:02,000 --> 00:00:03,000\ncafé à la crème\n"
	var le, be []byte
	for _, r := range text {
		le = append(le, byte(r), byte(r>>8))
		be = append(be, byte(r>>8), byte(r))
	}
	// Text with no ASCII letters at all is told by the markup around it.
	const cjk = "1\n00:00:02,000 --> 00:00:03,000\n你好，你最近怎么样？\n"
	for _, order := range []string{"LE", "BE"} {
		var wide []byte
		for _, r := range cjk {
			if order == "LE" {
				wide = append(wide, byte(r), byte(r>>8))
			} else {
				wide = append(wide, byte(r>>8), byte(r))
			}
		}
		if got, ok := utf8Text(wide); !ok || string(got) != cjk {
			t.Errorf("CJK %s without a mark: %q %v", order, got, ok)
		}
	}
	for name, data := range map[string][]byte{
		"LE with a mark": append([]byte{0xFF, 0xFE}, le...), "BE with a mark": append([]byte{0xFE, 0xFF}, be...),
		"LE without": le, "BE without": be,
	} {
		if got, ok := utf8Text(data); !ok || string(got) != text {
			t.Errorf("%s: %q %v", name, got, ok)
		}
	}
	for name, data := range map[string]string{"empty": "", "ASCII": text, "UTF-8": "café", "a few zeros": "a\x00b" + strings.Repeat("c", 100)} {
		if got, ok := utf8Text([]byte(data)); ok {
			t.Errorf("%s was read as UTF-16: %q", name, got)
		}
	}
}

// UTF-8 with a bad byte or a cut last character is still UTF-8; a Latin-1 file
// is not, and neither is one with more bad bytes than text.
func TestMostlyUTF8(t *testing.T) {
	text := strings.Repeat("Привет, как дела? ", 5)
	for name, c := range map[string]struct {
		data string
		want bool
	}{
		"cut":        {text + "\xd0", true},
		"stray":      {text + "\xe9 " + text, true},
		"Latin-1":    {"caf\xe9 \xe0 la cr\xe8me", false},
		"mostly bad": {"\xe9\xe9\xe9\xe9\xe9\xe9\xe9\xe9\xe9\xe9ж", false},
	} {
		if got, ok := mostlyUTF8([]byte(c.data)); ok != c.want || ok && !strings.Contains(string(got), "как дела?") {
			t.Errorf("%s: %v %q", name, ok, got)
		}
	}
}

// NUL, VT, FF and ESC are not subtitle text, so one anywhere in a legacy file,
// even inside a multi-byte character, reads as the file without it.
func TestToUTF8IgnoresControlBytes(t *testing.T) {
	for name, c := range map[string]struct {
		enc  encoding.Encoding
		text string
	}{
		"Shift_JIS": {japanese.ShiftJIS, "首相官邸は、外務省と防衛省の幹部を集めて、国際情勢に関する会議を開いた。"},
		"GBK":       {simplifiedchinese.GBK, "吃饭了吗？我想去商店买东西。你先回家吧，我马上就到。"},
		"Big5":      {traditionalchinese.Big5, "吃飯了嗎？我想去商店買東西。你先回家吧，我馬上就到。"},
		"CP1251":    {charmap.Windows1251, "Привет, как дела? Я давно тебя не видел. Что ты делаешь здесь так поздно ночью?"},
	} {
		file := subtitleFile(t, c.enc, c.text)
		clean, ok := toUTF8(file, "")
		if !ok || !strings.Contains(string(clean), c.text) {
			t.Fatalf("%s: the clean file reads as %v %s", name, ok, clean)
		}
		for _, b := range []byte{0, '\v', '\f', 0x1b} {
			for i := 0; i <= len(file); i++ {
				dirty := append(append(append([]byte{}, file[:i]...), b), file[i:]...)
				if got, ok := toUTF8(dirty, ""); !ok || string(got) != string(clean) {
					t.Fatalf("%s: byte %#x at %d reads as %v %q", name, b, i, ok, got)
				}
			}
		}
	}
}

// A stray ESC never changes how a file is read, even one that looks like an
// ISO-2022 designator: such files are not supported.
func TestToUTF8IgnoresESCBeforeDesignator(t *testing.T) {
	file := []byte("1\n00:00:01,000 --> 00:00:03,000\nこんにちは(B and $B here\n")
	clean, ok := toUTF8(file, "")
	if !ok {
		t.Fatal("clean file refused")
	}
	for _, d := range []string{"(B", "$B"} {
		dirty := []byte(strings.Replace(string(file), d, "\x1b"+d, 1))
		if got, ok := toUTF8(dirty, ""); !ok || string(got) != string(clean) {
			t.Errorf("ESC before %s reads as %v %q", d, ok, got)
		}
	}
}
