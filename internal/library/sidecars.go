package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"jellyscale/internal/media"
	"jellyscale/internal/store"
)

// subtitleCodecs maps the extensions of the subtitle files read beside a film
// to the codec name Jellyfin lists them with.
// A .sub is not here: it is half of a VobSub track, listed by its .idx.
var subtitleCodecs = map[string]string{".srt": "subrip", ".ass": "ass", ".ssa": "ass", ".vtt": "webvtt", ".sup": "PGSSUB", ".idx": "DVDSUB"}

// IsSubtitleFile reports whether key is a subtitle file a scan reads.
func IsSubtitleFile(key string) bool {
	ext := strings.ToLower(path.Ext(key))
	_, ok := subtitleCodecs[ext]
	return ok || ext == ".sub"
}

// subtitleName is what a subtitle file's name says about it, in the way
// Jellyfin 12.4 reads it ("Film (2024).en.forced.srt").
type subtitleName struct {
	Codec                  string
	Language               string // ISO 639-3, or a tag such as pt-br; empty for none
	LanguageName           string
	Title                  string
	Default, Forced, Hears bool
}

// parseSubtitleName reads the name of subtitle against video, which must be
// in the same directory. It is a sidecar of the video when its name is the
// video's name, then dot-separated words, then the extension.
func parseSubtitleName(video, subtitle string) (subtitleName, bool) {
	codec, ok := subtitleCodecs[strings.ToLower(path.Ext(subtitle))]
	stem := strings.TrimSuffix(path.Base(video), path.Ext(video))
	rest := strings.TrimSuffix(path.Base(subtitle), path.Ext(subtitle))
	if !ok || path.Dir(video) != path.Dir(subtitle) || len(rest) < len(stem) || !strings.EqualFold(rest[:len(stem)], stem) {
		return subtitleName{}, false
	}
	rest = rest[len(stem):]
	if rest != "" && rest[0] != '.' {
		return subtitleName{}, false
	}
	name := subtitleName{Codec: codec}
	var title []string
	for _, word := range strings.Split(rest, ".") {
		switch lower := strings.ToLower(word); {
		case word == "":
		case lower == "default":
			name.Default = true
		case lower == "forced" || lower == "foreign":
			name.Forced = true
		case lower == "cc" || lower == "hi" || lower == "sdh":
			name.Hears = true
		case name.Language == "":
			if code, label, ok := parseLanguage(lower); ok {
				name.Language, name.LanguageName = code, label
				continue
			}
			title = append(title, word)
		default:
			title = append(title, word)
		}
	}
	name.Title = strings.Join(title, " ")
	return name, true
}

// languageNames maps English language names to codes, so "Film.English.srt"
// reads like "Film.en.srt". Built once.
var languageNames = sync.OnceValue(func() map[string]string {
	names := map[string]string{}
	for a := 'a'; a <= 'z'; a++ {
		for b := 'a'; b <= 'z'; b++ {
			code := string([]rune{a, b})
			if tag, err := language.Parse(code); err == nil {
				names[strings.ToLower(display.English.Languages().Name(tag))] = code
			}
		}
	}
	return names
})

// specialLanguages are codes Jellyfin 12.4 lists as languages, in its names,
// that the display package has no name for or reads as another language ("und"
// as English).
var specialLanguages = map[string]string{"mis": "Uncoded languages", "und": "Undetermined", "mul": "Multiple languages", "zxx": "No linguistic content"}

// parseLanguage reads a two or three letter code, a code with a region
// ("pt-br") or an English name, and returns the code Jellyfin lists
// (ISO 639-3, or the tag as written for one with a region) and its name.
func parseLanguage(word string) (code, label string, ok bool) {
	if full, found := languageNames()[word]; found {
		word = full
	}
	if label, found := specialLanguages[word]; found {
		return word, label, true
	}
	if len(word) != 2 && len(word) != 3 && !(len(word) == 5 && word[2] == '-') {
		return "", "", false
	}
	tag, err := language.Parse(word)
	if err != nil {
		return "", "", false
	}
	base, _ := tag.Base()
	label = display.English.Languages().Name(language.Make(base.String()))
	if label == "" {
		// A code with no name ("cht", "qaa") is a word of the title, as Jellyfin
		// 12.4 reads it ("cht - Undefined - SUBRIP - External").
		return "", "", false
	}
	code = base.ISO3()
	if region, confidence := tag.Region(); len(word) == 5 && confidence == language.Exact {
		code = word
		label += " (" + display.English.Regions().Name(region) + ")"
	}
	return code, label, true
}

// displayTitle is the menu label: the title, language, then the flags, as
// Jellyfin writes it ("Japanese - Hearing Impaired - Default - Forced - SUBRIP - External").
func (n subtitleName) displayTitle() string {
	var parts []string
	if n.Title != "" {
		parts = append(parts, n.Title)
	}
	if n.Language == "" {
		parts = append(parts, "Undefined")
	} else {
		parts = append(parts, n.LanguageName)
	}
	if n.Hears {
		parts = append(parts, "Hearing Impaired")
	}
	if n.Default {
		parts = append(parts, "Default")
	}
	if n.Forced {
		parts = append(parts, "Forced")
	}
	return strings.Join(append(parts, strings.ToUpper(n.Codec), "External"), " - ")
}

// sidecarSet is the subtitle files beside one video, in file name order, which
// is the order new tracks are numbered in, and a fingerprint of all of them. A
// scan compares the fingerprint to the last one to see that a file was added,
// changed or removed without probing the film again.
//
// A VobSub track is two files. It is listed as its .idx, with the .sub in
// partners; an .idx with no .sub, or a .sub with no .idx, is not a track.
// describe fills in what the image files say.
type sidecarSet struct {
	files       []store.Object
	fingerprint string
	partners    map[string]store.Object // The .sub of an .idx, by the .idx's key.
	images      map[string]imageFile    // By key, for the files describe could read.
}

func sidecarsOf(video string, candidates []store.Object) sidecarSet {
	found := sidecarSet{partners: map[string]store.Object{}}
	subs := map[string]store.Object{} // By key without the extension.
	for _, file := range candidates {
		if strings.EqualFold(path.Ext(file.Key), ".sub") {
			subs[strings.TrimSuffix(file.Key, path.Ext(file.Key))] = file
		}
	}
	for _, file := range candidates {
		if _, ok := parseSubtitleName(video, file.Key); !ok {
			continue
		}
		if strings.EqualFold(path.Ext(file.Key), ".idx") {
			sub, paired := subs[strings.TrimSuffix(file.Key, path.Ext(file.Key))]
			if !paired {
				continue
			}
			found.partners[file.Key] = sub
		}
		found.files = append(found.files, file)
	}
	slices.SortFunc(found.files, func(a, b store.Object) int { return strings.Compare(strings.ToLower(a.Key), strings.ToLower(b.Key)) })
	if len(found.files) > 0 {
		hash := sha256.New()
		for _, file := range found.files {
			fmt.Fprintf(hash, "%s\n", fingerprintOf(file))
			if sub, ok := found.partners[file.Key]; ok {
				fmt.Fprintf(hash, "%s\n", fingerprintOf(sub))
			}
		}
		found.fingerprint = hex.EncodeToString(hash.Sum(nil))
	}
	return found
}

// fingerprintOf identifies one version of a subtitle file: its key, size and
// ETag. It names the converted track in the asset store, and a failure to read
// the file is remembered against it.
func fingerprintOf(file store.Object) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", file.Key, file.Size, file.ETag)))
	return hex.EncodeToString(sum[:8])
}

// externalStreams lists the subtitle files as streams. A file that was listed
// before (old, by key) keeps its index, so a viewer who holds one never finds
// another file's text under it; a new file takes the next index of the film's
// counter, which only grows, so the index of a removed file is never given
// again. floor is the number of the film's own streams, which the files come
// after, and counter the last next. next is the counter to store.
func externalStreams(video store.Object, set sidecarSet, old []media.MediaStream, floor, counter int) (streams []media.MediaStream, next int) {
	before := map[string]media.MediaStream{}
	for _, stream := range old {
		if stream.IsExternal && stream.Path != "" {
			before[stream.Path] = stream
		}
	}
	next = max(counter, floor, nextIndex(old))
	for _, file := range set.files {
		name, _ := parseSubtitleName(video.Key, file.Key)
		image, isImage := set.images[file.Key]
		if (name.Codec == "PGSSUB" || name.Codec == "DVDSUB") && !isImage {
			continue // Unreadable for good (describe), or not yet.
		}
		fingerprint := fingerprintOf(file)
		if sub, paired := set.partners[file.Key]; paired {
			fingerprint = fingerprintOf(store.Object{Key: file.Key, Size: file.Size + sub.Size, ETag: file.ETag + sub.ETag})
		}
		index, unreadable := -1, false
		// A film that was replaced by one with more streams of its own can
		// have taken the index; the file is then numbered again.
		if prev, ok := before[file.Key]; ok && prev.Index >= floor {
			index = prev.Index
			unreadable = prev.Unreadable && prev.Fingerprint == fingerprint
		}
		if index < 0 {
			index, next = next, next+1
		}
		stream := media.MediaStream{
			Index: index, Type: "Subtitle", Codec: name.Codec,
			IsDefault: name.Default, IsForced: name.Forced, IsHearingImpaired: name.Hears,
			IsExternal: true, IsTextSubtitleStream: true, SupportsExternalStream: true,
			Path: file.Key, Fingerprint: fingerprint, DisplayTitle: name.displayTitle(),
		}
		if isImage {
			stream.IsTextSubtitleStream, stream.IsImageSubtitleStream = false, media.Ptr(true)
			stream.FileSize, stream.FileETag = file.Size, file.ETag
			if image.Width > 0 && image.Height > 0 {
				stream.Width, stream.Height = image.Width, image.Height
			}
			if sub, paired := set.partners[file.Key]; paired {
				stream.SubPath, stream.SubSize = sub.Key, sub.Size
				// A pair's language is the one in its .idx, as Jellyfin has it, and
				// not the file name's.
				name.Language, name.LanguageName = image.Language, image.LanguageName
				stream.DisplayTitle = name.displayTitle()
			}
		}
		stream.Language = name.Language
		stream.Title = name.Title
		// A file that can't be read for good stays hidden until it changes,
		// and so does not cost a job at every scan.
		stream.Unreadable = unreadable
		streams = append(streams, stream)
	}
	return streams, next
}

// storeStreams replaces the streams of the film's media source and rebuilds the
// item's lists of them from the new ones. next is the media source's counter.
// It does not touch the source's probe version: only a probe raises that.
func storeStreams(ctx context.Context, tx pgx.Tx, itemID string, streams []media.MediaStream, next int) error {
	var raw []byte
	if err := tx.QueryRow(ctx, "SELECT dto FROM items WHERE id = $1 FOR UPDATE", itemID).Scan(&raw); err != nil {
		return err
	}
	var dto map[string]json.RawMessage
	if err := json.Unmarshal(raw, &dto); err != nil {
		return err
	}
	var sources []media.MediaSource
	if err := json.Unmarshal(dto["MediaSources"], &sources); err != nil {
		return err
	}
	if len(sources) != 1 {
		return fmt.Errorf("the item has %d media sources, not 1", len(sources))
	}
	playable := playableStreams(streams)
	sources[0].MediaStreams = playable
	var err error
	if dto["MediaStreams"], err = json.Marshal(playable); err != nil {
		return err
	}
	if dto["MediaSources"], err = json.Marshal(sources); err != nil {
		return err
	}
	newStreams, err := json.Marshal(streams)
	if err != nil {
		return err
	}
	newDTO, err := json.Marshal(dto)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE media_sources SET streams = $2, next_index = $3 WHERE item_id = $1", itemID, newStreams, next); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE items SET dto = $2 WHERE id = $1", itemID, newDTO)
	return err
}

// MarkUnreadable hides the subtitle that is stream index of the film's media
// source, if it is still the version with that fingerprint (empty for a track
// inside the film) and, when etag is not empty, the version of the file with
// that ETag, compared under the row lock the update is made in: a worker found it can't be read, and reading it again would
// only find that again. The mark stays until the file changes, which a scan
// sees as a new fingerprint or a new ETag.
func MarkUnreadable(ctx context.Context, pool *pgxpool.Pool, itemID string, index int, fingerprint, etag string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var raw []byte
		var next int
		var have string
		err := tx.QueryRow(ctx, "SELECT streams, next_index, COALESCE(object_etag, '') FROM media_sources WHERE item_id = $1 FOR UPDATE", itemID).Scan(&raw, &next, &have)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if etag != "" && have != etag {
			return nil
		}
		var streams []media.MediaStream
		if err := json.Unmarshal(raw, &streams); err != nil {
			return err
		}
		changed := false
		for i, stream := range streams {
			if stream.Index == index && stream.Fingerprint == fingerprint && !stream.Unreadable {
				streams[i].Unreadable = true
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return storeStreams(ctx, tx, itemID, streams, next)
	})
}

// nextIndex is the index after the last of streams that are listed as tracks.
// Attachments are not: ffprobe numbers them after the tracks, they are listed
// apart (MediaAttachments), and counting them would leave a gap in the
// MediaStreams a client reads DefaultSubtitleStreamIndex from by position.
func nextIndex(streams []media.MediaStream) int {
	next := 0
	for _, stream := range streams {
		if stream.Type != "Attachment" {
			next = max(next, stream.Index+1)
		}
	}
	return next
}
