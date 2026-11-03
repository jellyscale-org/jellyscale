package subtitles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// MaxFont is the largest font attachment kept. CJK fonts reach tens of MB.
// A variable for tests.
var MaxFont int64 = 64 << 20

// fontTypes are the MIME types jellyfin-web hands to its ASS renderer; it
// ignores any other attachment.
var fontTypes = []string{"application/vnd.ms-opentype", "application/x-truetype-font", "font/otf", "font/ttf", "font/woff", "font/woff2"}

// IsFont says whether a client draws an attachment of this MIME type.
func IsFont(mimeType string) bool { return slices.Contains(fontTypes, mimeType) }

// FontKey names a font in the library's asset store by the hash of its bytes,
// so the fonts that every episode of a season carries are stored once, not
// once per file.
func FontKey(sum string) string { return "fonts/" + sum }

// FontsKey is the object that lists the fonts of one version of a film, as the
// hash of each by attachment index. It is written last, after the fonts, so its
// being there says they all are.
func FontsKey(sourceID, etag string) string {
	return "attachments/" + sourceID + "/" + hashOf(etag) + ".fonts"
}

// ReadFonts returns the hash of each font of src by attachment index, if the
// fonts have been taken out of the film.
func ReadFonts(ctx context.Context, assets store.Store, src items.Source) (map[int]string, bool, error) {
	body, err := assets.Open(ctx, FontsKey(src.ID, src.ETag), 0)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer body.Close()
	var listed map[int]string
	// A manifest that doesn't parse counts as missing, so the job replaces it.
	return listed, json.NewDecoder(body).Decode(&listed) == nil, nil
}

// attachment is a stream of type Attachment in media_sources.streams, which the
// scan lists from its probe of the film.
type attachment struct {
	Index    int
	Type     string
	MimeType string
}

// Fonts are the fonts attached to src, by index, with their MIME types.
func Fonts(src items.Source) map[int]string {
	var streams []attachment
	if json.Unmarshal(src.Streams, &streams) != nil {
		return nil
	}
	fonts := map[int]string{}
	for _, stream := range streams {
		if stream.Type == "Attachment" && IsFont(stream.MimeType) {
			fonts[stream.Index] = stream.MimeType
		}
	}
	return fonts
}

// FillFonts takes the fonts out of src's file and puts them in the asset store,
// then the object that lists them. One ffmpeg run writes them to
// disk (it maps no frames, so it reads little more than the header), and each
// goes to the store from its file, so no font is held whole in memory. Only a
// worker calls it, through a fonts job. A zero timeout is DefaultTimeout.
func FillFonts(ctx context.Context, src items.Source, media, assets store.Store, timeout time.Duration, log *slog.Logger) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var streams []attachment
	if err := json.Unmarshal(src.Streams, &streams); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "attachments-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// ffmpeg names an attachment by its position among them.
	args := []string{"-nostdin", "-v", "error"}
	position := 0
	for _, stream := range streams {
		if stream.Type != "Attachment" {
			continue
		}
		if IsFont(stream.MimeType) {
			args = append(args, "-dump_attachment:t:"+strconv.Itoa(position), filepath.Join(dir, strconv.Itoa(stream.Index)))
		}
		position++
	}
	// ffmpeg exits with an error for a film the store has lost as it does for one
	// it can't parse, and only the second is the film's doing.
	probe, err := media.Open(ctx, src.Key, 0)
	if err != nil {
		return err
	}
	probe.Close()
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, src.Key, src.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	args = append(args, "-i", input, "-map", "0:v:0", "-frames:v", "0", "-f", "null", "-")
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	// See Extract: Pdeathsig needs the thread to stay.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	DieWithParent(cmd)
	out, err := cmd.CombinedOutput()
	kept := map[int]string{}
	switch {
	case (err != nil || len(bytes.TrimSpace(out)) > 0) && watched.failed.Load():
		// The film reached ffmpeg cut short, which says nothing of the film.
		return fmt.Errorf("the film could not be read from the store: ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	case err == nil:
		// Warnings with a zero exit are not a failure: the fonts that were
		// written are good, and one that wasn't is left out like one over the cap.
		for index := range Fonts(src) {
			sum, err := putFont(ctx, assets, filepath.Join(dir, strconv.Itoa(index)))
			if err != nil {
				return err
			}
			if sum == "" {
				log.Warn("font not kept", "index", index)
				continue
			}
			kept[index] = sum
		}
	case isBadFile(ctx, err, out):
		// The film's header is what it is, so asking again would only fail
		// again. The film is remembered as having no fonts for as long as this
		// version of it lasts (the key has its ETag), as a file that can't be
		// read is for as long as the file does, and a burn-in draws without them.
		log.Error("the film's fonts can't be taken out", "error", err, "output", strings.TrimSpace(string(out)))
	default:
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(string(out)))
	}
	manifest, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return assets.Put(ctx, FontsKey(src.ID, src.ETag), bytes.NewReader(manifest))
}

// watchedStore notes a read of the film that failed, since ffmpeg reports that
// as it does a film it can't parse.
type watchedStore struct {
	store.Store
	failed atomic.Bool
}

func (w *watchedStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := w.Store.Open(ctx, key, offset)
	if err != nil {
		w.failed.Store(true)
		return nil, err
	}
	return &watchedBody{ReadCloser: body, store: w}, nil
}

type watchedBody struct {
	io.ReadCloser
	store *watchedStore
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	// ffmpeg hanging up early cancels the read; that is not the store's fault.
	if err != nil && err != io.EOF && !errors.Is(err, context.Canceled) {
		b.store.failed.Store(true)
	}
	return n, err
}

// putFont puts the font in file in the asset store under the hash of its bytes,
// and returns the hash; "" for a file that is
// missing, empty or over MaxFont. The file is read twice and never held whole in memory.
func putFont(ctx context.Context, assets store.Store, path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil // ffmpeg exits 0 and writes no file for an attachment with no data.
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil || size == 0 || size > MaxFont {
		return "", err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	// Put even if an object is there: one cut short sits under the same key, and
	// the key is a hash, so a whole one is the same bytes.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return sum, assets.Put(ctx, FontKey(sum), file)
}
