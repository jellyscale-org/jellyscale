//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-110. A stream route has one request path whether Postgres answers or not (the audit's F4: the online handlers
// and the four twins written for the outage, kept in step by hand). These checks hold what must not change when the
// twins are folded into one: they ask every stream route of Routes() with Postgres up, then with it away of two
// stream replicas, and they ask with Postgres up what only Postgres may decide. They name no function of the build.
//
// Postgres is put away for real (testenv.DBProxy), as JS-085's and JS-103's are. A wait is not a duration measured
// here: a request "waits" when it has no answer until the test says Postgres is back, and the few seconds it is held
// for only bound a hang (test/timing-bounds.txt).
//
// The reference of an answer is the answer of the same route, with Postgres up, on stream replica 0, before the
// outage. Replica 1 serves nothing before it, so it answers from a snapshot it made alone.

type mode int

const (
	same    mode = iota // Answered during the outage as it is with Postgres up: status, every header but Date, body.
	differs             // Answered during the outage, and not as it is with Postgres up (the stopped play session).
	waits               // Not answered while Postgres is away; answered as usual, never 400 or more for being late, once back.
)

// probe is one request of a row of the table.
type probe struct {
	what    string
	method  string // GET when empty.
	path    string
	headers []string
	mode    mode

	refused  bool   // The answer with Postgres up is 400 or more: the request is wrong, not the outage.
	upStatus int    // The status with Postgres up, when it is none of the usual (200 to 399, or 400 or more if refused).
	etagOf   string // Sent as If-None-Match: the ETag of the answer to the ask of this name.

	statusOnly bool // Once back, only the status is the same: the body is a job's, not a request's.
	noRef      bool // Not asked with Postgres up first (it would make what the outage finds missing); wantStatus once back.
	wantStatus int

	outageStatus int    // For differs.
	outageBody   string // For differs.

	up full // The reference.
}

// routeRow is what a route of the stream role is asked.
type routeRow struct {
	route string // "METHOD /path", as Routes() has it.
	asks  []*probe
}

func authHeader(token string) []string {
	return []string{"Authorization", `MediaBrowser Token="` + token + `"`}
}

// setQuery sets a query parameter of a path, or adds it.
func setQuery(path, key, value string) string {
	pattern := regexp.MustCompile(`(?i)([?&]` + key + `)=[^&]*`)
	if pattern.MatchString(path) {
		return pattern.ReplaceAllString(path, "${1}="+value)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + key + "=" + value
}

// oneRoute is the subtitle env (a film as a copy and a re-encode with stored segments, text tracks, a PGS track and
// fonts in the stores, two stream replicas) with a second film that another viewer plays, a token that is signed in but
// plays nothing and has been seen by nobody, and one that will sign out.
type oneRoute struct {
	*subtitleAway
	film2       string
	film2Direct func(token string) string
	film2Hash   string
	outsider    string // Signed in, but with no session: in no snapshot.
	leaver      string // Plays the film; signs out when a test says so.
}

func reportStart(t *testing.T, a *subtitleAway, token, item string) {
	t.Helper()
	response, body := a.env.do(t, 0, "POST", "/Sessions/Playing", token,
		map[string]any{"ItemId": item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	if response.StatusCode/100 != 2 {
		t.Fatalf("starting %s: %d %s", item, response.StatusCode, body)
	}
}

// hashOfItem is hashOf for another item of the env.
func (p *playAway) hashOfItem(t *testing.T, item string, values url.Values) string {
	t.Helper()
	ctx := context.Background()
	source, found, err := p.env.apis[0].Items.Source(ctx, item, values.Get("mediasourceid"))
	if err != nil || !found {
		t.Fatal(found, err)
	}
	media := playbackSource(source)
	if media.Keyframes, media.VideoEnd, err = p.env.apis[0].Items.Keyframes(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	params, err := playback.ParseParams(values, media)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := playback.NewSpec(item, params, media)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Hash()
}

func newOneRoute(t *testing.T) *oneRoute {
	t.Helper()
	a := newSubtitleAway(t)
	film2, film2Direct := a.secondFilm(t)
	o := &oneRoute{subtitleAway: a, film2: film2, film2Direct: film2Direct}
	_, o.outsider = a.addUser(t, "outsider")
	_, o.leaver = a.addUser(t, "leaver")
	a.playing(t, 0)
	reportStart(t, a, a.tokens[1], film2) // Viewer 1 plays the other film.
	reportStart(t, a, o.leaver, a.item)
	// What a worker stored for the second film (a re-encode of it): its init and segment 2.
	for n, text := range map[int]string{-1: "film2 init", 2: "film2 segment two"} {
		hash := a.hashOfItem(t, film2, lowerKeys(mustQuery(t, o.film2Segment(2, a.tokens[1]))))
		o.film2Hash = hash
		if err := a.segments.Put(context.Background(), transcode.SegmentKey(hash, n), strings.NewReader(text)); err != nil {
			t.Fatal(err)
		}
	}
	_, refresh := timings(t)
	refreshed(refresh)
	return o
}

// seg is the path of segment n of the re-encode (films[1]) or copy (films[0]) of the first film, as viewer v asks.
func (o *oneRoute) seg(film, n, viewer int) string {
	f := o.films[film]
	path := f.segment[2]
	if n == -1 {
		path = f.init
	} else if n != 2 {
		path = strings.Replace(path, "hls1/main/2.mp4", fmt.Sprintf("hls1/main/%d.mp4", n), 1)
	}
	return o.as(f, path, viewer)
}

// film2Segment is segment n of the re-encode of the second film, for the token.
func (o *oneRoute) film2Segment(n int, token string) string {
	path := o.seg(1, n, 1)
	path = strings.ReplaceAll(path, db.DashedID(o.item), db.DashedID(o.film2))
	path = strings.ReplaceAll(path, db.CompactID(o.item), db.CompactID(o.film2))
	return strings.ReplaceAll(path, o.tokens[1], token)
}

func (o *oneRoute) fontPath(index int, token string) string {
	return fmt.Sprintf("/Videos/%s/%s/Attachments/%d?ApiKey=%s", db.DashedID(o.item), o.source, index, token)
}

// otherSource names the second film's source in the path of the first film: the item is the session's, the source
// is not one of its sources. A resolver that ignores the source id would serve it during the outage.
func (o *oneRoute) otherSource(path string) string {
	return strings.Replace(path, "/"+o.source+"/", "/"+db.CompactID(o.film2)+"/", 1)
}

func (o *oneRoute) sub(index int, file, query, token string) string {
	sep := "?"
	if query != "" {
		sep = "?" + query + "&"
	}
	return fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/%s%sApiKey=%s", db.DashedID(o.item), o.source, index, file, sep, token)
}

// outsiderOf is the ask as a token in no snapshot makes it.
func (o *oneRoute) outsiderOf(rq *probe) *probe {
	out := *rq
	out.what = "a token outside the snapshot: " + rq.what
	out.mode = waits
	out.path = strings.ReplaceAll(rq.path, o.tokens[0], o.outsider)
	out.headers = nil
	for _, h := range rq.headers {
		out.headers = append(out.headers, strings.ReplaceAll(h, o.tokens[0], o.outsider))
	}
	out.outageStatus, out.outageBody = 0, ""
	if out.etagOf != "" {
		out.etagOf = "a token outside the snapshot: " + out.etagOf
	}
	// Its play session is its own: another user's is refused as stopped.
	out.path = psidOf.ReplaceAllString(out.path, "${1}"+fmt.Sprintf("%032x", 0x9999))
	return &out
}

var psidOf = regexp.MustCompile(`(?i)(playsessionid=)[0-9a-f]+`)

var (
	ticksRoute = "GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{startPositionTicks}/{file}"
	trackRoute = "GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}"
	fontRoute  = "GET /Videos/{itemId}/{sourceId}/Attachments/{index}"
	streamKey  = "GET /Videos/{itemId}/{file}"
	segmentKey = "GET /Videos/{itemId}/hls1/main/{segmentId}"
	withTicks  = regexp.MustCompile(`/Subtitles/\d+/\d+/`)
)

// stopped makes the play session a segment is asked for one that was stopped, with Postgres up, and says its path.
func (o *oneRoute) stopped(t *testing.T) string {
	t.Helper()
	path := o.asToken(o.films[1], o.films[1].segment[2], o.tokens[0], 0x777)
	if got := fetch(context.Background(), "GET", o.streams[0].server.URL, path); got.status != 200 {
		t.Fatalf("a segment of a play session about to be stopped: %d", got.status)
	}
	psid := fmt.Sprintf("%032x", 0x777)
	got := fetch(context.Background(), "DELETE", o.streams[0].server.URL, "/Videos/ActiveEncodings?deviceId=dev1&playSessionId="+psid, authHeader(o.tokens[0])...)
	if got.status != 204 {
		t.Fatalf("stopping the play session: %d", got.status)
	}
	return path
}

// table is every stream route of Routes() and what is asked of it. A route with no row fails
// TestEveryStreamRouteIsAskedWithPostgresUpAndAway.
func (o *oneRoute) table(t *testing.T) []routeRow {
	t.Helper()
	tok0, tok1 := o.tokens[0], o.tokens[1]
	compact := db.CompactID(o.item)
	direct := o.direct(tok0)
	unnamed := strings.Replace(direct, "&mediaSourceId="+compact, "", 1)
	if unnamed == direct {
		t.Fatalf("the direct play path %q names no source", direct)
	}
	size := len(o.body)
	rows := map[string][]*probe{}
	add := func(route string, rq ...*probe) { rows[route] = append(rows[route], rq...) }
	withOutsider := func(route string, rq ...*probe) {
		for _, one := range rq {
			add(route, one, o.outsiderOf(one))
		}
	}

	// The encoding configuration jellyfin-web reads before an ASS track.
	withOutsider("GET /System/Configuration/encoding",
		&probe{what: "encoding configuration, viewer 0", path: "/System/Configuration/encoding", headers: authHeader(tok0)})
	add("GET /System/Configuration/encoding",
		&probe{what: "encoding configuration, viewer 1 (another film)", path: "/System/Configuration/encoding", headers: authHeader(tok1)})

	// Direct play of the session's film.
	whole := &probe{what: "direct play, the whole file", path: direct}
	pastEnd := &probe{what: "direct play, a range past the end", path: direct, headers: []string{"Range", fmt.Sprintf("bytes=%d-%d", size+10, size+20)}, upStatus: 416}
	notModified := &probe{what: "direct play, not modified", path: direct, etagOf: whole.what, upStatus: 304}
	withOutsider(streamKey,
		&probe{what: "direct play, the source named", path: direct, headers: []string{"Range", "bytes=100-299"}},
		&probe{what: "direct play, the source unnamed", path: unnamed, headers: []string{"Range", "bytes=0-9"}},
		whole, pastEnd, notModified,
		&probe{what: "direct play, HEAD", method: "HEAD", path: direct},
		&probe{what: "direct play, HEAD of a range", method: "HEAD", path: direct, headers: []string{"Range", "bytes=0-9"}},
	)
	add(streamKey,
		&probe{what: "direct play, viewer 1's own film", path: o.film2Direct(tok1), headers: []string{"Range", "bytes=0-99"}},
		&probe{what: "direct play, another film than the session's", mode: waits, path: o.film2Direct(tok0), headers: []string{"Range", "bytes=0-9"}},
		&probe{what: "direct play, the first film of viewer 1", mode: waits, path: o.direct(tok1), headers: []string{"Range", "bytes=0-9"}},
		&probe{what: "direct play, a source id that is not the film's", mode: waits, refused: true, upStatus: 404, path: strings.Replace(direct, "mediaSourceId="+compact, "mediaSourceId="+db.CompactID(o.film2), 1), headers: []string{"Range", "bytes=0-9"}},
		&probe{what: "direct play, a name that is not stream", mode: waits, refused: true, path: strings.Replace(direct, "stream.mkv", "notstream.mkv", 1)},
		&probe{what: "direct play, a malformed item id", mode: waits, refused: true, path: strings.Replace(direct, "/Videos/"+compact, "/Videos/zzz", 1)},
	)

	// A segment of the transcode store.
	copySeg, reSeg := o.seg(0, 2, 0), o.seg(1, 2, 0)
	withOutsider(segmentKey, &probe{what: "a media segment of the re-encode", path: reSeg})
	add(segmentKey,
		&probe{what: "the init segment of the re-encode", path: o.seg(1, -1, 0)},
		&probe{what: "a media segment of the copy", path: copySeg},
		&probe{what: "the init segment of the copy", path: o.seg(0, -1, 0)},
		&probe{what: "a media segment, HEAD", method: "HEAD", path: reSeg},
		&probe{what: "a segment of viewer 1's film", path: o.film2Segment(2, tok1)},
		&probe{what: "the init segment of viewer 1's film", path: o.film2Segment(-1, tok1)},
		&probe{what: "a segment of another film than the session's", mode: waits, path: o.seg(1, 2, 1)},
		&probe{what: "a segment of a play session that was stopped", mode: differs, path: o.stopped(t), upStatus: 410, outageStatus: 200, outageBody: o.films[1].stored[2]},
		&probe{what: "a segment the store lacks", mode: waits, noRef: true, wantStatus: 200, path: o.seg(1, 9, 0)},
		&probe{what: "a segment number outside the playlist", mode: waits, refused: true, path: strings.Replace(reSeg, "hls1/main/2.mp4", "hls1/main/999.mp4", 1)},
		&probe{what: "a spec that cannot be made", mode: waits, refused: true, upStatus: 400, path: setQuery(reSeg, "AudioStreamIndex", "99")},
		&probe{what: "a malformed item id", mode: waits, refused: true, path: strings.Replace(reSeg, db.DashedID(o.item), "zzz", 1)},
	)

	// Playlists and what needs Postgres to decide.
	for _, name := range []string{"master", "main"} {
		path := o.films[1].master
		if name == "main" {
			path = o.films[1].main
		}
		withOutsider("GET /Videos/{itemId}/"+name+".m3u8", &probe{what: name + " playlist", mode: waits, path: o.as(o.films[1], path, 0)})
	}
	add("DELETE /Videos/ActiveEncodings",
		&probe{what: "DELETE ActiveEncodings", method: "DELETE", mode: waits, upStatus: 204, headers: authHeader(tok0),
			path: "/Videos/ActiveEncodings?deviceId=dev1&playSessionId=" + fmt.Sprintf("%032x", 0xfeed)},
		&probe{what: "DELETE ActiveEncodings of a token outside the snapshot", method: "DELETE", mode: waits, upStatus: 204, headers: authHeader(o.outsider),
			path: "/Videos/ActiveEncodings?deviceId=dev1&playSessionId=" + fmt.Sprintf("%032x", 0xfeed)})
	add("GET /Items/{itemId}/Download",
		&probe{what: "Download", mode: waits, path: "/Items/" + compact + "/Download?ApiKey=" + tok0},
		&probe{what: "Download of a token outside the snapshot", mode: waits, path: "/Items/" + compact + "/Download?ApiKey=" + o.outsider})
	add("GET /Playback/BitrateTest",
		&probe{what: "BitrateTest", mode: waits, path: "/Playback/BitrateTest?size=1000", headers: authHeader(tok0)},
		&probe{what: "BitrateTest of a token outside the snapshot", mode: waits, path: "/Playback/BitrateTest?size=1000", headers: authHeader(o.outsider)})
	for _, route := range []string{"GET /Items/{itemId}/Images/{imageType}", "GET /Items/{itemId}/Images/{imageType}/{imageIndex}"} {
		path := "/Items/" + compact + "/Images/Primary"
		if strings.HasSuffix(route, "{imageIndex}") {
			path += "/0"
		}
		add(route, &probe{what: "an item image (public)", mode: waits, upStatus: 404, path: path})
	}

	// Text tracks, a PGS track and fonts, every kind the stores answer without a job.
	for _, rq := range o.kinds(tok0) {
		route := trackRoute
		if withTicks.MatchString(rq.path) {
			route = ticksRoute
		}
		if strings.Contains(rq.path, "/Attachments/") {
			route = fontRoute
		}
		add(route, &probe{what: rq.what, path: rq.path, headers: rq.headers})
	}
	fontTag := func(index int) string {
		sum := sha256.Sum256(o.font[index])
		return `"` + hex.EncodeToString(sum[:]) + `"`
	}
	add(fontRoute,
		&probe{what: "font 6, not modified", path: o.fontPath(6, tok0), headers: []string{"If-None-Match", fontTag(6)}, upStatus: 304},
		&probe{what: "a font of the film, a token outside the snapshot", mode: waits, path: o.fontPath(6, o.outsider)},
		&probe{what: "a font listed but never taken out of the film", mode: waits, statusOnly: true, path: o.fontPath(8, tok0)},
		&probe{what: "a font the film does not list", mode: waits, refused: true, path: o.fontPath(9, tok0)},
		&probe{what: "a font index that is not a number", mode: waits, refused: true, path: strings.Replace(o.fontPath(6, tok0), "Attachments/6", "Attachments/x", 1)},
		&probe{what: "a font of a malformed item id", mode: waits, refused: true, path: strings.Replace(o.fontPath(6, tok0), db.DashedID(o.item), "zzz", 1)},
		&probe{what: "a font of the film, a source id that is not its", mode: waits, refused: true, upStatus: 404, path: o.otherSource(o.fontPath(6, tok0))},
		&probe{what: "a font of another film than the session's", mode: waits, path: o.fontPath(6, tok1)},
	)
	add(trackRoute,
		&probe{what: "PGS as sup, HEAD", method: "HEAD", path: o.sub(5, "Stream.sup", "", tok0)},
		&probe{what: "the playlist of subtitles", mode: waits, path: o.sub(3, "subtitles.m3u8", "segmentLength=6", tok0)},
		&probe{what: "the playlist of subtitles, a token outside the snapshot", mode: waits, path: o.sub(3, "subtitles.m3u8", "segmentLength=6", o.outsider)},
		&probe{what: "a track of a token outside the snapshot", mode: waits, path: o.sub(3, "Stream.vtt", "", o.outsider)},
		&probe{what: "a track of another film than the session's", mode: waits, path: o.sub(3, "Stream.vtt", "", tok1)},
		&probe{what: "a track of the film, a source id that is not its", mode: waits, refused: true, upStatus: 404, path: o.otherSource(o.sub(3, "Stream.vtt", "", tok0))},
		&probe{what: "an unknown format", mode: waits, refused: true, upStatus: 400, path: o.sub(3, "Stream.xyz", "", tok0)},
		&probe{what: "HEAD of the playlist of subtitles", method: "HEAD", mode: waits, refused: true, upStatus: 405, path: o.sub(3, "subtitles.m3u8", "segmentLength=6", tok0)},
		&probe{what: "the playlist without segmentLength", mode: waits, refused: true, upStatus: 400, path: o.sub(3, "subtitles.m3u8", "", tok0)},
		&probe{what: "an image track as text", mode: waits, refused: true, upStatus: 400, path: o.sub(5, "Stream.vtt", "", tok0)},
		&probe{what: "a text track as sup", mode: waits, refused: true, path: o.sub(3, "Stream.sup", "", tok0)},
		&probe{what: "an audio stream as text", mode: waits, refused: true, upStatus: 404, path: o.sub(2, "Stream.vtt", "", tok0)},
		&probe{what: "a track the film does not have", mode: waits, refused: true, upStatus: 404, path: o.sub(99, "Stream.vtt", "", tok0)},
		&probe{what: "a name that is not Stream", mode: waits, refused: true, upStatus: 404, path: o.sub(3, "Other.vtt", "", tok0)},
		&probe{what: "a malformed item id", mode: waits, refused: true, upStatus: 404, path: strings.Replace(o.sub(3, "Stream.vtt", "", tok0), db.DashedID(o.item), "zzz", 1)},
		&probe{what: "a cue window that is not a number", mode: waits, refused: true, upStatus: 404, path: o.sub(3, "Stream.vtt", "StartPositionTicks=x", tok0)},
	)
	add(ticksRoute,
		&probe{what: "a start that is not a number", mode: waits, refused: true, upStatus: 404, path: o.sub(3, "x/Stream.vtt", "", tok0)},
		&probe{what: "a track of another film than the session's, shifted", mode: waits, path: o.sub(3, "300000000/Stream.vtt", "", tok1)},
		&probe{what: "an unknown format, shifted", mode: waits, refused: true, upStatus: 400, path: o.sub(3, "300000000/Stream.xyz", "", tok0)},
		&probe{what: "a negative start", mode: waits, refused: true, upStatus: 404, path: o.sub(3, "-5/Stream.vtt", "", tok0)},
	)
	var out []routeRow
	for route, asks := range rows {
		out = append(out, routeRow{route: route, asks: asks})
	}
	slices.SortFunc(out, func(a, b routeRow) int { return strings.Compare(a.route, b.route) })
	return out
}

// streamRoutes are the routes the stream role serves, as Routes() says.
func streamRoutes() []string {
	var routes []string
	for _, route := range (&Server{}).Routes() {
		if route.Role == RoleStream {
			routes = append(routes, route.Method+" "+route.Path)
		}
	}
	return routes
}

// dump is every row the stream role could write: jobs, viewers and the subtitle and fonts jobs.
func (o *oneRoute) dump(t *testing.T) string {
	t.Helper()
	return scalar[string](t, o.pool, `SELECT
		coalesce((SELECT string_agg(j::text, E'\n' ORDER BY j::text) FROM transcode_jobs j), '') || E'\n--\n' ||
		coalesce((SELECT string_agg(v::text, E'\n' ORDER BY v::text) FROM transcode_viewers v), '') || E'\n--\n' ||
		coalesce((SELECT string_agg(s::text, E'\n' ORDER BY s::text) FROM subtitle_jobs s), '')`)
}

// madeByAWorker does what a worker does once Postgres is back: it makes a segment for each queued job.
func (o *oneRoute) madeByAWorker(ctx context.Context) {
	for ctx.Err() == nil {
		rows, err := o.pool.Query(ctx, "SELECT id, spec_hash, start_segment FROM transcode_jobs WHERE state = 'queued'")
		if err == nil {
			type queued struct {
				id    int
				hash  string
				start int
			}
			var found []queued
			for rows.Next() {
				var q queued
				if rows.Scan(&q.id, &q.hash, &q.start) == nil {
					found = append(found, q)
				}
			}
			rows.Close()
			for _, q := range found {
				o.segments.Put(ctx, transcode.SegmentKey(q.hash, q.start), strings.NewReader("made"))                                             //nolint:errcheck
				o.pool.Exec(ctx, `UPDATE transcode_jobs SET state = 'done', worker = 'w1', next_segment = start_segment + 1 WHERE id = $1`, q.id) //nolint:errcheck
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (rq *probe) verb() string {
	if rq.method == "" {
		return "GET"
	}
	return rq.method
}

// reference asks everything once with Postgres up, on stream replica 0, and holds the row itself to what it says.
func (o *oneRoute) reference(t *testing.T, asks []*probe) {
	t.Helper()
	ctx := context.Background()
	base := o.streams[0].server.URL
	for pass := 0; pass < 2; pass++ {
		for _, rq := range asks {
			if rq.noRef || (rq.etagOf != "") != (pass == 1) {
				continue
			}
			if rq.etagOf != "" {
				var tag string
				for _, other := range asks {
					if other.what == rq.etagOf && other.path == rq.path {
						tag = other.up.header.Get("ETag")
					}
				}
				if tag == "" {
					t.Fatalf("%s: the ask %q has no ETag with Postgres up", rq.what, rq.etagOf)
				}
				rq.headers = append(slices.Clone(rq.headers), "If-None-Match", tag)
			}
			c, stop := context.WithTimeout(ctx, 30*time.Second)
			rq.up = fetch(c, rq.verb(), base, rq.path, rq.headers...)
			stop()
			switch {
			case rq.up.err != nil:
				t.Errorf("%s with Postgres up: %v", rq.what, rq.up.err)
			case rq.upStatus != 0 && rq.up.status != rq.upStatus:
				t.Errorf("%s with Postgres up: %d, the row says %d: %.100s", rq.what, rq.up.status, rq.upStatus, rq.up.body)
			case rq.upStatus == 0 && rq.refused && rq.up.status < 400:
				t.Errorf("%s with Postgres up: %d, but the row says the request is refused", rq.what, rq.up.status)
			case rq.upStatus == 0 && !rq.refused && rq.up.status >= 400:
				t.Errorf("%s with Postgres up: %d %.100s", rq.what, rq.up.status, rq.up.body)
			}
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// Every stream route of Routes() is asked with Postgres up and then, of two stream replicas one of which served the
// viewers nothing before, with Postgres away, by two viewers playing different films at once. A route with no row
// fails here, so a route added later cannot go without its answer in an outage being decided.
//
// What the snapshot answers today is the same answer, status, every header but Date, and body, as with Postgres up
// (the one answer that differs is the stopped play session's, in the table as it is). What it cannot answer, and what
// a token it holds has refused with Postgres up, waits: nothing is answered while Postgres is away, nothing is
// answered 400 or more because of it, and the late answer is the usual one with no header left over from the attempt.
// Nothing served or held writes a row, and nothing logs a failed request.
func TestEveryStreamRouteIsAskedWithPostgresUpAndAway(t *testing.T) {
	o := newOneRoute(t)
	rows := o.table(t)

	routes := streamRoutes()
	if len(routes) < 13 {
		t.Fatalf("only %d stream routes in Routes(); there were thirteen, two of them the public image routes", len(routes))
	}
	have := map[string]bool{}
	for _, row := range rows {
		have[row.route] = true
	}
	for _, route := range routes {
		if !have[route] {
			t.Errorf("%s has no row in the table: say what is asked of it, up and away", route)
		}
	}
	for route := range have {
		if !slices.Contains(routes, route) {
			t.Errorf("the table has a row for %s, which is no stream route", route)
		}
	}
	var asks []*probe
	for _, row := range rows {
		asks = append(asks, row.asks...)
	}
	o.reference(t, asks)
	count := map[mode]int{}
	for _, rq := range asks {
		count[rq.mode]++
	}
	t.Logf("%d routes, %d requests: %d answered during the outage, %d differently, %d held", len(routes), len(asks), count[same], count[differs], count[waits])
	o.outage(t, asks, false)
}

// outage puts Postgres away from both stream replicas, asks, and checks what the table says. With servedOnly it asks
// nothing that waits, and holds the outage no longer than the answers need, then checks that being back adds a row.
func (o *oneRoute) outage(t *testing.T, asks []*probe, servedOnly bool) {
	t.Helper()
	_, refresh := timings(t)
	before := o.dump(t)
	for _, rp := range o.streams {
		rp.away(t, o.pool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	type held struct {
		rq      *probe
		replica int
		ch      chan full
	}
	var holding []held
	if !servedOnly {
		for _, rq := range asks {
			if rq.mode != waits {
				continue
			}
			for i, rp := range o.streams {
				ch := make(chan full, 1)
				go func() { ch <- fetch(ctx, rq.verb(), rp.server.URL, rq.path, rq.headers...) }()
				holding = append(holding, held{rq, i, ch})
			}
		}
	}
	var mu sync.Mutex
	var wrong []string
	var wg sync.WaitGroup
	for i, rp := range o.streams {
		for _, rq := range asks {
			if rq.mode == waits {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, stop := context.WithTimeout(context.Background(), answerDeadline)
				defer stop()
				got := fetch(c, rq.verb(), rp.server.URL, rq.path, rq.headers...)
				var problem string
				switch {
				case got.err != nil:
					problem = fmt.Sprintf("not answered within %v while Postgres is away: %v", answerDeadline, got.err)
				case rq.mode == differs && (got.status != rq.outageStatus || string(got.body) != rq.outageBody):
					problem = fmt.Sprintf("answered %d %.60q, want %d %.60q", got.status, got.body, rq.outageStatus, rq.outageBody)
				case rq.mode == same && got.shape() != rq.up.shape():
					problem = fmt.Sprintf("answered\n%.300s\nwant, as with Postgres up\n%.300s", got.shape(), rq.up.shape())
				}
				if problem != "" {
					mu.Lock()
					wrong = append(wrong, fmt.Sprintf("replica %d, %s: %s", i, rq.what, problem))
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(wrong) > 0 {
		t.Errorf("%d answers during the outage are not what Postgres up gives; the first:\n  %s", len(wrong), strings.Join(wrong[:min(len(wrong), 4)], "\n  "))
	}
	if !servedOnly {
		time.Sleep(3 * time.Second) // Only bounds the hang: a held request has no answer to come.
		for _, h := range holding {
			select {
			case got := <-h.ch:
				t.Errorf("replica %d, %s: answered %d while Postgres was away", h.replica, h.rq.what, got.status)
				h.ch <- got
			default:
			}
		}
	}
	if during := o.dump(t); during != before {
		t.Errorf("a request served or held during the outage wrote a row:\nbefore\n%s\nduring\n%s", before, during)
	}
	for i, rp := range o.streams {
		if log := rp.log.String(); strings.Contains(log, "request failed") {
			t.Errorf("replica %d logged a failed request during the outage:\n%s", i, log)
		}
	}

	workers, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go o.madeByAWorker(workers)
	for _, rp := range o.streams {
		rp.proxy.Back()
	}
	var late []string
	for _, h := range holding {
		select {
		case got := <-h.ch:
			want := h.rq.up
			switch {
			case got.err != nil:
				late = append(late, fmt.Sprintf("replica %d, %s: %v", h.replica, h.rq.what, got.err))
			case h.rq.noRef && got.status != h.rq.wantStatus:
				late = append(late, fmt.Sprintf("replica %d, %s: %d once back, want %d", h.replica, h.rq.what, got.status, h.rq.wantStatus))
			case h.rq.noRef:
			case h.rq.statusOnly && got.status != want.status:
				late = append(late, fmt.Sprintf("replica %d, %s: %d once back, %d with Postgres up", h.replica, h.rq.what, got.status, want.status))
			case !h.rq.statusOnly && got.shape() != want.shape():
				late = append(late, fmt.Sprintf("replica %d, %s: once back\n%.300s\nwith Postgres up\n%.300s", h.replica, h.rq.what, got.shape(), want.shape()))
			}
		case <-time.After(90 * time.Second):
			late = append(late, fmt.Sprintf("replica %d, %s: not answered 90 seconds after Postgres was back", h.replica, h.rq.what))
		}
	}
	if len(late) > 0 {
		t.Errorf("%d late answers are not the usual ones; the first:\n  %s", len(late), strings.Join(late[:min(len(late), 4)], "\n  "))
	}
	if servedOnly {
		stopWorker()
		refreshed(refresh) // Two refreshes, and the notifications, have run since Postgres came back.
		if after := o.dump(t); after != before {
			t.Errorf("a request the snapshot served added a row once Postgres was back:\nbefore\n%s\nafter\n%s", before, after)
		}
	}
}

// What the snapshot served leaves nothing to be written afterwards: no job, no viewer, no subtitle or fonts job is
// added by the requests the replicas answered while Postgres was away, once it is back (a viewer is recorded by the
// request Postgres decides, and the snapshot's was not).
func TestARequestTheSnapshotServedAddsNoRowOnceTheOutageIsOver(t *testing.T) {
	o := newOneRoute(t)
	var asks []*probe
	for _, row := range o.table(t) {
		for _, rq := range row.asks {
			if rq.mode != waits {
				asks = append(asks, rq)
			}
		}
	}
	o.reference(t, asks)
	o.outage(t, asks, true)
}

// With Postgres up, Postgres alone decides, also for a token the snapshot holds. A signed-in token is served any film
// it asks for, whether or not its session plays it: before a Playing report, another film, after Stopped. A signed-out
// token is refused on its very next request, though a snapshot holds it. A file replaced since the last refresh is
// answered from its new row on its next request, on every route that serves a film.
func TestWithPostgresUpPostgresAloneDecidesForATokenTheSnapshotHolds(t *testing.T) {
	o := newOneRoute(t)
	tok0 := o.tokens[0]
	ctx := context.Background()
	each := func(what, path string, status int, body string, headers ...string) {
		t.Helper()
		for i, rp := range o.streams {
			got := fetch(ctx, "GET", rp.server.URL, path, headers...)
			if got.err != nil || got.status != status || (body != "" && string(got.body) != body) {
				t.Errorf("replica %d, %s: %d %v %.60q, want %d %.60q", i, what, got.status, got.err, got.body, status, body)
			}
		}
	}

	// The file is replaced after the last refresh: each route answers from the new row at once.
	replaced := bytes.Repeat([]byte("replaced!"), 400)
	if err := writeFilm(o.uri, "replaced.mkv", replaced); err != nil {
		t.Fatal(err)
	}
	mustExec(t, o.pool, "UPDATE media_sources SET store_key = 'replaced.mkv', object_etag = 'replaced', size = $2 WHERE id = $1", o.item, len(replaced))
	each("direct play of a replaced file", o.direct(tok0), 206, string(replaced[:10]), "Range", "bytes=0-9")
	mustExec(t, o.pool, "UPDATE items SET run_time_ticks = 100000000 WHERE id = $1", o.item) // 10 seconds: two segments, not 15.
	each("a segment of a film that got shorter", o.seg(1, 5, 0), 404, "")
	mustExec(t, o.pool, `UPDATE media_sources SET streams = $1 WHERE id = $2`, `[{"Index":0,"Type":"Video","Codec":"h264","Profile":"Main","Level":41,"VideoRangeType":"SDR","Width":1280,"Height":720,"AverageFrameRate":24},
		{"Index":1,"Type":"Audio","Codec":"ac3","IsDefault":true,"Channels":2},
		{"Index":7,"Type":"Attachment","Codec":"ttf","MimeType":"application/x-truetype-font"}]`, o.item)
	each("a text track the replaced file lacks", o.sub(3, "Stream.vtt", "", tok0), 404, "")
	each("a PGS track the replaced file lacks", o.sub(5, "Stream.sup", "", tok0), 404, "")
	each("a font the replaced file lacks", o.fontPath(6, tok0), 404, "")

	// A token asks for a film its session does not play: viewer 1 plays the second film.
	each("a film the session does not play", o.direct(o.tokens[1]), 206, string(replaced[:10]), "Range", "bytes=0-9")
	each("a segment of a film the session does not play", strings.ReplaceAll(o.film2Segment(2, tok0), "hls1/main/2", "hls1/main/-1"), 200, "film2 init")
	// Before a Playing report: a token that has none.
	each("a token that reported nothing", o.direct(o.outsider), 206, string(replaced[:10]), "Range", "bytes=0-9")
	// After Stopped.
	response, body := o.env.do(t, 0, "POST", "/Sessions/Playing/Stopped", tok0, map[string]any{"ItemId": o.item, "PositionTicks": 0, "PlaySessionId": ""})
	if response.StatusCode/100 != 2 {
		t.Fatalf("Stopped: %d %s", response.StatusCode, body)
	}
	each("direct play after Stopped", o.direct(tok0), 206, string(replaced[:10]), "Range", "bytes=0-9")
	each("a film the session no longer plays", o.film2Direct(tok0), 206, "", "Range", "bytes=0-9")

	// Signed out: refused on the next request, on every route that serves a film, though the snapshot holds the token.
	mustExec(t, o.pool, "DELETE FROM devices WHERE token = $1", o.leaver)
	leaver := o.leaver
	for what, path := range map[string]string{
		"direct play":     o.direct(leaver),
		"a segment":       o.asToken(o.films[1], o.films[1].segment[2], leaver, 0x300),
		"a text track":    o.sub(3, "Stream.vtt", "", leaver),
		"a PGS track":     o.sub(5, "Stream.sup", "", leaver),
		"a font":          o.fontPath(7, leaver),
		"the encoding":    "/System/Configuration/encoding",
		"the init":        o.asToken(o.films[0], o.films[0].init, leaver, 0x301),
		"a second source": strings.Replace(o.direct(leaver), "stream.mkv", "stream", 1),
	} {
		each("a signed-out token's "+what, path, 401, "", authHeader(leaver)...)
	}
}

// markedLog counts the statements sent for a request (a context carrying requestMark) and keeps their text. The
// snapshot's own refresh, which runs on no request, is not counted.
type requestMark struct{}

type markedLog struct {
	mu   sync.Mutex
	sent []string
}

func (l *markedLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if ctx.Value(requestMark{}) != nil {
		l.mu.Lock()
		l.sent = append(l.sent, oneLine(data.SQL))
		l.mu.Unlock()
	}
	return ctx
}
func (l *markedLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (l *markedLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	sent := l.sent
	l.sent = nil
	return sent
}

// A request on the stream role makes no more statements to Postgres than it did before JS-110: a direct play range
// two (the token, and the source row), the encoding configuration one (the token), and the rest the numbers below,
// which are what the build before made, measured on it: a stored segment three (the token, the source row, the viewer), a text track, a PGS track and a font two. A segment request still records its viewer and the segment before it is
// answered, so a stop right after finds it.
func TestWithPostgresUpAStreamRequestMakesNoMoreStatementsThanBefore(t *testing.T) {
	o := newOneRoute(t)
	cfg, err := pgxpool.ParseConfig(testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	log := &markedLog{}
	cfg.ConnConfig.Tracer = log
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Not opened by db.Open, so no outage tracker: this replica never goes away.
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleStream}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), SocketIdle: time.Minute}
	s.Transcode = &transcode.Jobs{Pool: pool, Segments: o.segments, QueueTimeout: 2 * time.Second}
	handler := s.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestMark{}, true)))
	}))
	t.Cleanup(server.Close)

	tok := o.tokens[0]
	segment := o.asToken(o.films[1], o.films[1].segment[2], tok, 0x555)
	for _, c := range []struct {
		what    string
		path    string
		headers []string
		most    int // What the build before made.
	}{
		{"the encoding configuration", "/System/Configuration/encoding", authHeader(tok), 1},
		{"a direct play range", o.direct(tok), []string{"Range", "bytes=100-299"}, 2},
		{"a direct play range of a film the session does not play", o.film2Direct(tok), []string{"Range", "bytes=0-9"}, 2},
		{"a stored segment", segment, nil, 3},
		{"a text track", o.sub(3, "Stream.vtt", "", tok), nil, 2},
		{"a PGS track", o.sub(5, "Stream.sup", "", tok), nil, 2},
		{"a font", o.fontPath(6, tok), nil, 2},
	} {
		log.take()
		got := fetch(context.Background(), "GET", server.URL, c.path, c.headers...)
		time.Sleep(200 * time.Millisecond) // What a handler does after it has answered is counted too.
		statements := log.take()
		if got.err != nil || got.status/100 != 2 {
			t.Errorf("%s: %d %v", c.what, got.status, got.err)
			continue
		}
		t.Logf("%s: %d statements: %s", c.what, len(statements), strings.Join(statements, " | "))
		if len(statements) > c.most {
			t.Errorf("%s made %d statements, the build before made %d:\n  %s", c.what, len(statements), c.most, strings.Join(statements, "\n  "))
		}
	}
	// The segment's viewer is recorded by the time the answer is: asked for right after it.
	hash := scalar[string](t, o.pool, "SELECT spec_hash FROM transcode_viewers WHERE play_session_id = $1", fmt.Sprintf("%032x", 0x555))
	if n := scalar[int](t, o.pool, "SELECT segment FROM transcode_viewers WHERE play_session_id = $1 AND spec_hash = $2 AND NOT stopped", fmt.Sprintf("%032x", 0x555), hash); n != 2 {
		t.Errorf("the viewer of the segment was recorded as asking for segment %d, want 2", n)
	}
}

// A replica with no transcode store answers a segment 503 with Postgres up, as it does once Postgres is back: while it
// is away the request waits, the snapshot never answering for what Postgres has refused.
func TestASegmentOnAReplicaWithNoTranscodeStoreIsRefusedAsUsualAfterAWait(t *testing.T) {
	o := newOneRoute(t)
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	pool, err := db.Open(context.Background(), proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rp := &replica{role: RoleStream, proxy: proxy, log: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(rp.log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleStream}, Log: log, SocketIdle: time.Minute}
	rp.server = httptest.NewServer(s.Handler())
	t.Cleanup(rp.server.Close)
	_, refresh := timings(t)
	refreshed(refresh)

	path := o.seg(1, 2, 0)
	up := fetch(context.Background(), "GET", rp.server.URL, path)
	if up.status != 503 {
		t.Fatalf("a segment on a replica with no transcode store, Postgres up: %d, want 503", up.status)
	}
	rp.away(t, o.pool)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ch := make(chan full, 1)
	go func() { ch <- fetch(ctx, "GET", rp.server.URL, path) }()
	select {
	case got := <-ch:
		t.Fatalf("answered %d while Postgres was away", got.status)
	case <-time.After(3 * time.Second):
	}
	rp.proxy.Back()
	select {
	case got := <-ch:
		if got.shape() != up.shape() {
			t.Errorf("once back:\n%.300s\nwith Postgres up:\n%.300s", got.shape(), up.shape())
		}
	case <-time.After(60 * time.Second):
		t.Error("not answered 60 seconds after Postgres was back")
	}
}

// A store failure while Postgres is away is not answered from the snapshot: the request waits, with no byte sent, and
// nothing is logged as a failed request. A segment whose Stat or Open fails, a track, the fonts list or a font the
// asset store cannot give whole, and a store that cannot be opened.
func TestAStoreFailureDuringTheOutageWaitsForPostgresAndLogsNothing(t *testing.T) {
	// Segments: the kit's replicas over a store a test makes fail.
	k := newSegKit(t)
	k.reset(t)
	k.put(t, 2, []byte("stored segment two"))
	k.remote.store.set(func(s *segStore) {
		s.statErr = func(key string) error {
			if key == k.key(2) {
				return fmt.Errorf("the store says no")
			}
			return nil
		}
	})
	k.local.store.set(func(s *segStore) {
		s.open = func(context.Context, string, int64) (io.ReadCloser, error) {
			return nil, fmt.Errorf("the store cannot open it")
		}
	})
	k.goOffline()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	statFails, openFails := make(chan full, 1), make(chan full, 1)
	go func() { statFails <- fetch(ctx, "GET", k.remote.server.URL, k.url(2)) }()
	go func() { openFails <- fetch(ctx, "GET", k.local.server.URL, k.url(2)) }()
	time.Sleep(3 * time.Second)
	for what, ch := range map[string]chan full{"a segment whose Stat fails": statFails, "a segment whose Open fails": openFails} {
		select {
		case got := <-ch:
			t.Errorf("%s was answered %d from the snapshot while Postgres was away", what, got.status)
		default:
		}
	}
	for _, rp := range []*segReplica{k.remote, k.local} {
		if log := rp.log.String(); strings.Contains(log, "request failed") {
			t.Errorf("%s logged a failed request during the outage:\n%s", rp.server.URL, log)
		}
		rp.store.reset() // The store works again, so what waited is answered as usual.
		rp.proxy.Back()
	}
	for _, ch := range []chan full{statFails, openFails} {
		select {
		case <-ch:
		case <-time.After(2 * time.Minute):
			t.Error("a request held for Postgres was not answered once it was back")
		}
	}
}

func TestAnAssetStoreThatCannotGiveAnObjectWholeWaitsForPostgresAndLogsNothing(t *testing.T) {
	o := newOneRoute(t)
	ctx := context.Background()
	source, found, err := o.env.apis[0].Items.Source(ctx, o.item, "")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	// The track is cut, the manifest of the second film's fonts is not JSON, and the font of the first is another font's bytes.
	if err := o.assets.Put(ctx, subtitles.Key(source, 3), strings.NewReader("not a track")); err != nil {
		t.Fatal(err)
	}
	source2, found, err := o.env.apis[0].Items.Source(ctx, o.film2, "")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := o.assets.Put(ctx, subtitles.FontsKey(source2.ID, source2.ETag), strings.NewReader("{not json")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(o.font[7])
	if err := o.assets.Put(ctx, subtitles.FontKey(hex.EncodeToString(sum[:])), strings.NewReader("other bytes")); err != nil {
		t.Fatal(err)
	}
	tok0, tok1 := o.tokens[0], o.tokens[1]
	film2Font := fmt.Sprintf("/Videos/%s/%s/Attachments/6?ApiKey=%s", db.DashedID(o.film2), db.CompactID(o.film2), tok1)
	cases := map[string]string{
		"a track that is not whole":       o.sub(3, "Stream.vtt", "", tok0),
		"a font that is not whole":        o.fontPath(7, tok0),
		"a fonts list that is not whole":  film2Font,
		"the same, a track shifted":       o.sub(3, "300000000/Stream.srt", "", tok0),
		"a track, a cue window":           o.sub(3, "Stream.vtt", "StartPositionTicks=0&EndPositionTicks=900000000", tok0),
		"a font of the fonts list, again": film2Font,
	}
	o.waitsWhileAway(t, cases)
}

// waitsWhileAway puts both stream replicas away and shows that none of the requests is answered, or logged as
// failed, until Postgres is back.
func (o *oneRoute) waitsWhileAway(t *testing.T, cases map[string]string) {
	t.Helper()
	for _, rp := range o.streams {
		rp.away(t, o.pool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	type one struct {
		what string
		ch   chan full
	}
	var held []one
	for what, path := range cases {
		for _, rp := range o.streams {
			ch := make(chan full, 1)
			go func() { ch <- fetch(ctx, "GET", rp.server.URL, path) }()
			held = append(held, one{what, ch})
		}
	}
	time.Sleep(3 * time.Second)
	for _, h := range held {
		select {
		case got := <-h.ch:
			t.Errorf("%s was answered %d while Postgres was away", h.what, got.status)
		default:
		}
	}
	for i, rp := range o.streams {
		if log := rp.log.String(); strings.Contains(log, "request failed") {
			t.Errorf("replica %d logged a failed request during the outage:\n%s", i, log)
		}
		rp.proxy.Back()
	}
	for _, h := range held {
		select {
		case <-h.ch:
		case <-time.After(2 * time.Minute):
			t.Errorf("%s was not answered once Postgres was back", h.what)
		}
	}
}

// A store that cannot be opened (a URI no store answers) leaves the request to wait, whichever store it is.
func TestAStoreThatCannotBeOpenedWaitsForPostgres(t *testing.T) {
	o := newOneRoute(t)
	mustExec(t, o.pool, "UPDATE libraries SET media_store_uri = 'nostore://x', asset_store_uri = 'nostore://x'")
	_, refresh := timings(t)
	refreshed(refresh)
	tok0 := o.tokens[0]
	o.waitsWhileAway(t, map[string]string{
		"direct play":  o.direct(tok0),
		"a text track": o.sub(3, "Stream.vtt", "", tok0),
		"a PGS track":  o.sub(5, "Stream.sup", "", tok0),
		"a font":       o.fontPath(6, tok0),
	})
}

// An object gone from the media store is answered 404 during the outage, as it is with Postgres up: the byte serving of
// direct play answers it itself.
func TestADirectPlayObjectGoneFromTheStoreIsAnswered404DuringTheOutage(t *testing.T) {
	o := newOneRoute(t)
	data := bytes.Repeat([]byte("gone soon"), 300)
	if err := writeFilm(o.uri, "gone.mkv", data); err != nil {
		t.Fatal(err)
	}
	mustExec(t, o.pool, "UPDATE media_sources SET store_key = 'gone.mkv', object_etag = 'gone', size = $2 WHERE id = $1", o.film2, len(data))
	_, refresh := timings(t)
	refreshed(refresh) // The snapshots hold the row of a file that is there.
	if err := os.Remove(filepath.Join(strings.TrimPrefix(o.uri, "file://"), "gone.mkv")); err != nil {
		t.Fatal(err)
	}
	var asks []*probe
	for _, rq := range []*probe{
		{what: "a range of a gone object", path: o.film2Direct(o.tokens[1]), headers: []string{"Range", "bytes=0-9"}, upStatus: 404},
		{what: "a gone object", path: o.film2Direct(o.tokens[1]), upStatus: 404},
	} {
		asks = append(asks, rq)
	}
	o.reference(t, asks)
	o.outage(t, asks, false)
}
