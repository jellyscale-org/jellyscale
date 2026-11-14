//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// JS-103. A viewer in a replica's snapshot who turns on a track the browser has not fetched sees its cues during an
// outage, when the asset store holds the track: every kind of answer the stores can give without a job (a text track
// in each format and cue window the route renders, a PGS .sup with its ranges, a font and its 304) is the same
// answer, status, headers and body, as with Postgres up. Anything the store lacks, another film's, one from a file
// replaced before the last refresh, and any request of a token outside the snapshot wait for Postgres.

// full is an answer with its headers, which answer drops.
type full struct {
	status  int
	header  http.Header
	body    []byte
	err     error
	elapsed time.Duration
}

func fetch(ctx context.Context, method, base, path string, headers ...string) full {
	request, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return full{err: err}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	start := time.Now()
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return full{err: err, elapsed: time.Since(start)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return full{status: response.StatusCode, header: response.Header, body: body, err: err, elapsed: time.Since(start)}
}

// shape is what must be the same: the status, every header but the date, and the body.
func (a full) shape() string {
	var names []string
	for name := range a.header {
		if name != "Date" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out strings.Builder
	fmt.Fprintf(&out, "%d\n", a.status)
	for _, name := range names {
		fmt.Fprintf(&out, "%s: %s\n", name, strings.Join(a.header[name], ", "))
	}
	return out.String() + "\n" + string(a.body)
}

const (
	vttOne = "WEBVTT\n\n00:00:02.000 --> 00:00:30.000\nSnapshot line one\n\n00:00:30.500 --> 00:01:20.000\nSnapshot line two\n"
	srtOne = "1\n00:00:02,000 --> 00:00:30,000\nSnapshot line one\n\n2\n00:00:30,500 --> 00:01:20,000\nSnapshot line two\n"
)

// subtitleAway is the play-away env with a film that has two text tracks, a PGS track and two fonts in the asset
// store, and a viewer in every snapshot.
type subtitleAway struct {
	*playAway
	assets store.Store
	source string // The media source id, as in the paths.
	font   map[int][]byte
}

func newSubtitleAway(t *testing.T) *subtitleAway {
	t.Helper()
	p := newPlayAway(t, 2)
	mustExec(t, p.pool, `UPDATE media_sources SET streams = $1`, `[{"Index":0,"Type":"Video","Codec":"h264","Profile":"Main","Level":41,"VideoRangeType":"SDR","Width":1280,"Height":720,"AverageFrameRate":24},
		{"Index":1,"Type":"Audio","Codec":"ac3","IsDefault":true,"Channels":2},
		{"Index":3,"Type":"Subtitle","Codec":"subrip","IsTextSubtitleStream":true},
		{"Index":4,"Type":"Subtitle","Codec":"ass","IsTextSubtitleStream":true},
		{"Index":5,"Type":"Subtitle","Codec":"PGSSUB","IsImageSubtitleStream":true},
		{"Index":6,"Type":"Attachment","Codec":"ttf","MimeType":"font/ttf"},
		{"Index":7,"Type":"Attachment","Codec":"ttf","MimeType":"application/x-truetype-font"},
		{"Index":8,"Type":"Attachment","Codec":"ttf","MimeType":"font/ttf"}]`)
	source, found, err := p.env.apis[0].Items.Source(context.Background(), p.item, "")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, body io.Reader) {
		if err := assets.Put(context.Background(), key, body); err != nil {
			t.Fatal(err)
		}
	}
	put(subtitles.Key(source, 3), subtitles.Track{VTT: vttOne, SRT: srtOne}.Encode())
	put(subtitles.Key(source, 4), subtitles.Track{VTT: vttOne, SRT: srtOne, ASS: testASS}.Encode())
	put(subtitles.SupKey(source, 5), bytes.NewReader(bytes.Repeat([]byte("PG-image-cue"), 300)))
	a := &subtitleAway{playAway: p, assets: assets, source: db.CompactID(source.ID), font: map[int][]byte{6: bytes.Repeat([]byte("font-six!"), 400), 7: bytes.Repeat([]byte("font-seven"), 300)}}
	// Fonts 6 and 7 are in the store and its manifest; 8 is listed by the film but was never taken out of it.
	manifest := "{"
	for index, data := range a.font {
		sum := sha256.Sum256(data)
		hexSum := hex.EncodeToString(sum[:])
		put(subtitles.FontKey(hexSum), bytes.NewReader(data))
		manifest += fmt.Sprintf(`"%d":"%s",`, index, hexSum)
	}
	put(subtitles.FontsKey(source.ID, source.ETag), strings.NewReader(strings.TrimSuffix(manifest, ",")+"}"))
	return a
}

func (a *subtitleAway) track(index int, ticks int64, file, query, token string) string {
	sep := "?"
	if query != "" {
		sep = "?" + query + "&"
	}
	return fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/%d/%s%sApiKey=%s", db.DashedID(a.item), a.source, index, ticks, file, sep, token)
}

// requests of the stores' answers that need no job.
func (a *subtitleAway) kinds(token string) []served {
	noTicks := func(index int, file, query string) string {
		return fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/%s?%sApiKey=%s", db.DashedID(a.item), a.source, index, file, query, token)
	}
	attachment := func(index int) string {
		return fmt.Sprintf("/Videos/%s/%s/Attachments/%d?ApiKey=%s", db.DashedID(a.item), a.source, index, token)
	}
	return []served{
		{what: "SubRip as vtt", path: a.track(3, 0, "Stream.vtt", "", token)},
		{what: "SubRip as srt", path: a.track(3, 0, "Stream.srt", "", token)},
		{what: "SubRip as js", path: a.track(3, 0, "Stream.js", "", token)},
		{what: "SubRip as vtt, shifted by its start", path: a.track(3, 300000000, "Stream.vtt", "", token)},
		{what: "SubRip as vtt, the window of a playlist segment", path: noTicks(3, "stream.vtt", "CopyTimestamps=true&AddVttTimeMap=true&StartPositionTicks=250000000&EndPositionTicks=700000000&")},
		{what: "SubRip as vtt, a window with no cue in it", path: noTicks(3, "stream.vtt", "StartPositionTicks=900000000&EndPositionTicks=950000000&")},
		{what: "ASS as ass", path: a.track(4, 0, "Stream.ass", "", token)},
		{what: "ASS as ass, shifted", path: a.track(4, 300000000, "Stream.ass", "", token)},
		{what: "ASS as vtt", path: a.track(4, 0, "Stream.vtt", "", token)},
		{what: "ASS as ass, lower case path", path: strings.ToLower(a.track(4, 0, "Stream.ass", "", token))},
		{what: "PGS as sup", path: a.track(5, 0, "Stream.sup", "", token)},
		{what: "PGS as pgssub", path: a.track(5, 0, "Stream.pgssub", "", token)},
		{what: "PGS, the probe of jellyfin-web", path: a.track(5, 0, "Stream.pgssub", "", token), headers: []string{"Range", "bytes=0-0"}},
		{what: "PGS, a range", path: a.track(5, 0, "Stream.sup", "", token), headers: []string{"Range", "bytes=100-1099"}},
		{what: "PGS, the end", path: a.track(5, 0, "Stream.sup", "", token), headers: []string{"Range", "bytes=3500-"}},
		{what: "font 6", path: attachment(6)},
		{what: "font 7", path: attachment(7)},
	}
}

// Each kind is answered, during the outage and from a replica that never served the viewer, as it is with Postgres
// up: the reference is the answer of the same replica before. The font's 304 is asked with the ETag the reference gave.
func TestSubtitlesAndFontsAreAnsweredAsUsualFromTheSnapshotDuringAnOutage(t *testing.T) {
	a := newSubtitleAway(t)
	_, refresh := timings(t)
	a.playing(t, 0)
	token := a.tokens[0]
	ctx := context.Background()
	reference := map[string]full{}
	requests := a.kinds(token)
	for _, rq := range requests {
		got := fetch(ctx, "GET", a.streams[0].server.URL, rq.path, rq.headers...)
		if got.err != nil || got.status/100 != 2 {
			t.Fatalf("%s with Postgres up: %d %v %.80s", rq.what, got.status, got.err, got.body)
		}
		reference[rq.what] = got
	}
	// The font's 304, and a HEAD of the sup, asked with what the reference said.
	etag := reference["font 6"].header.Get("ETag")
	if etag == "" {
		t.Fatal("the font has no ETag with Postgres up")
	}
	fontPath := fmt.Sprintf("/Videos/%s/%s/Attachments/6?ApiKey=%s", db.DashedID(a.item), a.source, token)
	requests = append(requests, served{what: "font 6 not modified", path: fontPath, headers: []string{"If-None-Match", etag}})
	reference["font 6 not modified"] = fetch(ctx, "GET", a.streams[0].server.URL, fontPath, "If-None-Match", etag)
	if reference["font 6 not modified"].status != 304 {
		t.Fatalf("the font's own ETag was answered %d with Postgres up, want 304", reference["font 6 not modified"].status)
	}
	refreshed(refresh)
	for _, rp := range a.streams {
		rp.away(t, a.pool)
	}
	for i, rp := range a.streams {
		var wrong []string
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, rq := range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), answerDeadline)
				defer cancel()
				got := fetch(ctx, "GET", rp.server.URL, rq.path, rq.headers...)
				var problem string
				switch {
				case got.err != nil:
					problem = fmt.Sprintf("%s: not answered within %v while Postgres is away", rq.what, answerDeadline)
				case got.shape() != reference[rq.what].shape():
					problem = fmt.Sprintf("%s: answered\n%.300s\nwant\n%.300s", rq.what, got.shape(), reference[rq.what].shape())
				}
				if problem != "" {
					mu.Lock()
					wrong = append(wrong, problem)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(wrong) > 0 {
			t.Errorf("replica %d (it served the viewer nothing before: %v): %d of %d answers differ from Postgres being up; the first:\n  %s", i, i == 1, len(wrong), len(requests), strings.Join(wrong[:min(len(wrong), 3)], "\n  "))
		}
	}
	for _, rp := range a.streams {
		rp.proxy.Back()
	}
}

// What the stores cannot answer without a job waits, and is answered as usual once Postgres is back: a font the film
// lists that was never taken out of it, another film's track (which the store holds), a track of the session's own
// film extracted from a file since replaced, and every subtitle request of a token that is not in the snapshot. None
// is answered 400 or more because Postgres is away, and nothing is answered before it is back.
func TestSubtitlesTheStoreLacksWaitForPostgres(t *testing.T) {
	a := newSubtitleAway(t)
	_, refresh := timings(t)
	a.playing(t, 0)
	token := a.tokens[0]
	// Another film with its own text track in the store, which the viewer's session does not play.
	other, _ := a.secondFilm(t)
	otherSource, _, err := a.env.apis[0].Items.Source(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	otherStreams := `[{"Index":0,"Type":"Video","Codec":"h264"},{"Index":3,"Type":"Subtitle","Codec":"subrip","IsTextSubtitleStream":true}]`
	mustExec(t, a.pool, `UPDATE media_sources SET streams = $2 WHERE id = $1`, other, otherStreams)
	if err := json.Unmarshal([]byte(otherStreams), &otherSource.Streams); err != nil {
		t.Fatal(err)
	}
	if err := a.assets.Put(context.Background(), subtitles.Key(otherSource, 3), subtitles.Track{VTT: vttOne, SRT: srtOne}.Encode()); err != nil {
		t.Fatal(err)
	}
	otherPath := fmt.Sprintf("/Videos/%s/%s/Subtitles/3/0/Stream.vtt?ApiKey=%s", db.DashedID(other), db.CompactID(other), token)
	// A film replaced before the last refresh: its tracks were extracted from the old file, under its old ETag.
	mustExec(t, a.pool, "UPDATE media_sources SET object_etag = 'replaced' WHERE id = $1", a.item)
	replaced := a.track(3, 0, "Stream.vtt", "", token)
	missingFont := fmt.Sprintf("/Videos/%s/%s/Attachments/8?ApiKey=%s", db.DashedID(a.item), a.source, token)
	// A worker that is alive and never answers: what a track the store lacks is waited on, not refused, and the job it
	// starts once Postgres is back is the usual answer to it.
	mustExec(t, a.pool, "UPDATE transcode_nodes SET roles = 'worker', seen_at = now() + interval '1 hour'")
	refreshed(refresh)
	for _, rp := range a.streams {
		rp.away(t, a.pool)
	}
	jobs := func() int { return scalar[int](t, a.pool, "SELECT count(*)::int FROM subtitle_jobs") }
	before := jobs()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	var needJob []pending
	for i, rp := range a.streams {
		// Answered when Postgres is back, with no job: the font the film lists and the store lacks gets the empty font
		// the route sends with Postgres up, and a token outside the snapshot is refused.
		for what, path := range map[string]string{
			"the font the store lacks":                missingFont,
			"a track of a token outside the snapshot": strings.Replace(a.track(3, 0, "Stream.vtt", "", token), token, "not-a-token-at-all", 1),
			"a font of a token outside the snapshot":  strings.Replace(fmt.Sprintf("/Videos/%s/%s/Attachments/6?ApiKey=%s", db.DashedID(a.item), a.source, token), token, "not-a-token-at-all", 1),
			"a sup of a token outside the snapshot":   strings.Replace(a.track(5, 0, "Stream.sup", "", token), token, "not-a-token-at-all", 1),
		} {
			w := pending{what: fmt.Sprintf("%s, on replica %d", what, i), want: 401, ch: ask(ctx, rp, served{path: path})}
			if strings.Contains(what, "font the store lacks") {
				w.want = 200
			}
			waiting = append(waiting, w)
		}
		// A job's answers: the other film's track is in the store but is not the session's film, and the track of the
		// replaced file is under the old ETag. Neither is answered during the outage, and once Postgres is back each
		// starts the extraction it would with Postgres up, which nobody finishes here.
		for what, path := range map[string]string{"a track of another film": otherPath, "a track extracted from a replaced file": replaced} {
			needJob = append(needJob, pending{what: fmt.Sprintf("%s, on replica %d", what, i), ch: ask(ctx, rp, served{path: path})})
		}
	}
	holdAndCheck(t, 8*time.Second, waiting, func() {
		for _, w := range needJob {
			select {
			case got := <-w.ch:
				t.Errorf("%s was answered %d (%v) while Postgres was away", w.what, got.status, got.err)
			default:
			}
		}
		if after := jobs(); after != before {
			t.Errorf("a subtitle or fonts job was started or joined while Postgres was away: %d jobs before, %d after", before, after)
		}
		for _, rp := range a.streams {
			rp.proxy.Back()
		}
	})
	waitLong(t, "the extraction the usual answer starts, once Postgres is back", func() bool { return jobs() > before })
	for _, w := range needJob {
		select {
		case got := <-w.ch:
			if got.err == nil && got.status >= 400 {
				t.Errorf("%s was answered %d once Postgres was back, want it to wait for its extraction: %.80s", w.what, got.status, got.body)
			}
		default:
		}
	}
}
