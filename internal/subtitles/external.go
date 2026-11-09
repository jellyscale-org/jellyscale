package subtitles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"jellyscale/internal/items"
)

// MaxFile is the largest subtitle file read. Real ones are kilobytes; this only
// stops a stray large file from filling a worker's memory.
const MaxFile = 16 << 20

// ExternalStream is what media_sources.streams says about a subtitle file
// beside the film: where it is, and a fingerprint of the version that was
// listed (its key, size and ETag).
type ExternalStream struct {
	Index       int
	IsExternal  bool
	Path        string
	Fingerprint string
	Unreadable  bool
	// FileSize and FileETag are the version of a .sup that was listed.
	FileSize int64
	FileETag string
	Language string // ISO 639-3 from the file's name, or empty
}

// External finds stream index of src if it is a file beside the film.
func External(src items.Source, index int) (ExternalStream, bool) {
	var streams []ExternalStream
	if json.Unmarshal(src.Streams, &streams) != nil {
		return ExternalStream{}, false
	}
	for _, stream := range streams {
		if stream.IsExternal && stream.Index == index {
			return stream, true
		}
	}
	return ExternalStream{}, false
}

// ExtractFile converts the text of a subtitle file; ext is its extension,
// which tells ffmpeg the format. UTF-16 is converted here, because ffmpeg
// reads it unreliably; UTF-8 goes to ffmpeg as it is; anything else is
// converted to UTF-8 in the character set legacyText finds, unless it is
// UTF-8 with a few bad bytes (mostlyUTF8). language is the one
// in the file's name, which settles a near-tie between sets. A file that no
// detected set decodes is ErrBadFile.
func ExtractFile(ctx context.Context, body io.Reader, ext, language string) (Track, error) {
	data, err := io.ReadAll(io.LimitReader(body, MaxFile+1))
	if err != nil {
		return Track{}, err
	}
	if len(data) > MaxFile {
		return Track{}, fmt.Errorf("%w: it is larger than %d MB", ErrBadFile, MaxFile>>20)
	}
	data, ok := toUTF8(data, language)
	if !ok {
		return Track{}, fmt.Errorf("%w: its character set is not one this server recognises", ErrBadFile)
	}
	dir, err := os.MkdirTemp("", "subtitles-")
	if err != nil {
		return Track{}, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input"+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return Track{}, err
	}
	return Extract(ctx, path, 0)
}

// dropControl removes NUL, VT, FF and ESC, which are never subtitle text:
// ffmpeg refuses a file that starts with NUL or ESC and drops the cues from a
// NUL on. No legacy multi-byte set this server reads has them as a trail byte.
// ISO-2022 uses ESC and so is not read (docs/subtitles.md: known limits).
func dropControl(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		if b != 0 && b != '\v' && b != '\f' && b != 0x1b {
			out = append(out, b)
		}
	}
	return out
}

// toUTF8 is the text of a subtitle file in UTF-8 with the control bytes gone.
// ok is false when no character set decodes it.
func toUTF8(data []byte, language string) (text []byte, ok bool) {
	if text, ok := utf8Text(data); ok {
		return dropControl(text), true
	}
	// Dropped before the character set is guessed, so a stray byte inside a
	// multi-byte character can't change the guess. UTF-16 has NULs of its own,
	// which is why this is not done before utf8Text.
	data = dropControl(data)
	if utf8.Valid(data) {
		return data, true
	}
	if text, ok := mostlyUTF8(data); ok {
		return text, true
	}
	text, ok = legacyText(data, language)
	return dropControl(text), ok
}
