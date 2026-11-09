package library

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"regexp"
	"strconv"
	"strings"

	"jellyscale/internal/store"
)

// imageFile is what a PGS (.sup) file or a VobSub .idx says about its track.
type imageFile struct {
	Width, Height int    // The canvas; 0 when the file says none.
	Language      string // Of a VobSub pair, as its .idx writes it ("en"); empty for a .sup.
	LanguageName  string
}

// maxIndexFile bounds how much of an .idx is read; real ones are far smaller.
const maxIndexFile = 16 << 20

// describe reads the image subtitle files of set, which are small, and not the
// film. A .sup is listed whatever it holds, with a size if it is PGS; a pair
// whose .idx is no index file is not listed, and neither is its .sub. A file the
// store could not be asked for is left out of this scan's listing and the set's
// fingerprint says so, so the next scan reads it again.
func (s *Scanner) describe(ctx context.Context, media store.Store, set sidecarSet) sidecarSet {
	set.images = map[string]imageFile{}
	for _, file := range set.files {
		var image imageFile
		var ok bool
		var err error
		switch {
		case strings.HasSuffix(strings.ToLower(file.Key), ".sup"):
			image, ok, err = readSup(ctx, media, file.Key)
		case strings.HasSuffix(strings.ToLower(file.Key), ".idx"):
			image, ok, err = readVobsubIndex(ctx, media, file.Key)
		default:
			continue
		}
		if err != nil {
			s.Log.Warn("reading a subtitle file failed", "key", file.Key, "error", err)
			set.fingerprint = "unread"
		} else if ok {
			set.images[file.Key] = image
		}
	}
	return set
}

// readSup gives the canvas from the first segment of a PGS stream, a
// presentation composition segment: "PG", two times, the type 0x16, its length,
// then the width and height. Anything else is listed without a size.
func readSup(ctx context.Context, media store.Store, key string) (image imageFile, ok bool, err error) {
	body, err := media.Open(ctx, key, 0)
	if err != nil {
		return image, false, err
	}
	defer body.Close()
	var head [15 + 4]byte
	if _, err := io.ReadFull(body, head[:]); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return image, false, err
	}
	if string(head[:2]) == "PG" && head[10] == 0x16 {
		image.Width, image.Height = int(binary.BigEndian.Uint16(head[13:])), int(binary.BigEndian.Uint16(head[15:]))
	}
	return image, true, nil
}

var (
	indexSize     = regexp.MustCompile(`^size:\s*(\d+)x(\d+)`)
	indexLanguage = regexp.MustCompile(`^id:\s*([A-Za-z]{2,3})\b`)
)

// readVobsubIndex reads the canvas ("size: 720x480") and the language of the first
// track ("id: en, index: 0") from a VobSub .idx. A file with no "id:" line is no
// index file.
func readVobsubIndex(ctx context.Context, media store.Store, key string) (image imageFile, ok bool, err error) {
	body, err := media.Open(ctx, key, 0)
	if err != nil {
		return image, false, err
	}
	defer body.Close()
	lines := bufio.NewScanner(io.LimitReader(body, maxIndexFile))
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if m := indexSize.FindStringSubmatch(line); m != nil && image.Width == 0 {
			image.Width, _ = strconv.Atoi(m[1])
			image.Height, _ = strconv.Atoi(m[2])
		}
		if m := indexLanguage.FindStringSubmatch(line); m != nil {
			image.Language = strings.ToLower(m[1])
			_, image.LanguageName, _ = parseLanguage(image.Language)
			return image, true, nil
		}
	}
	return image, false, lines.Err()
}
