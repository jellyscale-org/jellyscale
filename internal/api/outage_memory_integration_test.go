//go:build integration

package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/transcode"
)

// JS-085. Every stream replica refreshes, every few seconds while Postgres
// answers, a snapshot of the tokens of recently active playback sessions, and
// with Postgres away honours a token in its last snapshot for direct-play ranges
// and the segments already in the transcode store, and for nothing else. These
// tests put the replicas' Postgres away for real (testenv.DBProxy) and read and
// write the rows themselves, which a replica cannot: a device signed out "after
// the last refresh" is one the test deletes once the replicas are away.
//
// The window and the refresh interval are the ones DOMAINS invariant 37 states
// (read below), so a build cannot pass by stating one number and using another.
// A token whose session was seen at half the window is inside it; one seen an
// hour ago, or never, is outside it.
//
// A request "answered within 5 seconds" is one given a 5 second deadline. It is
// the criterion's own bound, over ten times what a replica takes (milliseconds),
// so it only stops a hang (test/timing-bounds.txt).

const answerDeadline = 5 * time.Second

// awayFilm is one way the film is played: a copy of its video, or a re-encode.
type awayFilm struct {
	name               string
	index              int
	psid               string
	master, main, init string // With the first viewer's token and play session.
	segment            map[int]string
	hash               string
	stored             map[int]string // What the store holds: segment n, and the init as -1.
}

type playAway struct {
	*outage
	films  []*awayFilm
	tokens []string // One per viewer; the first is the env's own.
	direct func(token string) string
	ranges []byteRange
}

type byteRange struct {
	header string
	from   int
	to     int // Inclusive.
}

// newPlayAway is the outage env with a film that is offered as a copy and, to a
// device that caps its bitrate, as a re-encode, two stream replicas, and a token
// for each viewer.
func newPlayAway(t *testing.T, viewers int) *playAway {
	t.Helper()
	e, segments, source := transcodeEnv(t, "h264", "ac3")
	// The captured fixture of TestPlaybackInfoOffersACopy: keyframes in a 90 s film.
	mustExec(t, e.pool, "UPDATE media_sources SET keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', video_end = 900000000")
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
	o := &outage{env: e, segments: segments}
	for range 2 {
		o.streams = append(o.streams, newReplica(t, e, segments, RoleStream))
	}
	p := &playAway{outage: o, tokens: []string{e.token}}
	for v := 1; v < viewers; v++ {
		_, token := e.addUser(t, fmt.Sprintf("viewer%d", v))
		p.tokens = append(p.tokens, token)
	}
	compact := db.CompactID(e.item)
	p.direct = func(token string) string {
		return "/Videos/" + compact + "/stream.mkv?Static=true&mediaSourceId=" + compact + "&ApiKey=" + token
	}
	// Ranges the first viewer's player never asked for, and the film's last bytes.
	n := len(e.body)
	p.ranges = []byteRange{{"bytes=0-9", 0, 9}, {"bytes=1000-1999", 1000, 1999}, {fmt.Sprintf("bytes=%d-%d", n-100, n-1), n - 100, n - 1}}

	profile := capturedProfile(t)
	for i, flavour := range []struct {
		name string
		cap  int
	}{{"copy", 0}, {"re-encode", 720000}} {
		body := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source}
		if flavour.cap > 0 {
			body["MaxStreamingBitrate"] = flavour.cap
		}
		media := firstSource(e.playbackInfo(t, 0, e.item, body))
		transcodingURL := media["TranscodingUrl"].(string)
		values := mustQuery(t, transcodingURL)
		if (values.Get("VideoCodec") == "copy") != (flavour.name == "copy") {
			t.Fatalf("the %s flavour got VideoCodec=%s", flavour.name, values.Get("VideoCodec"))
		}
		f := &awayFilm{name: flavour.name, index: i, psid: values.Get("PlaySessionId"), master: transcodingURL, segment: map[int]string{}, stored: map[int]string{}}
		path := "/videos/" + db.DashedID(e.item)
		_, master := e.do(t, 0, "GET", transcodingURL, "", nil)
		f.main = path + "/main.m3u8?" + strings.TrimSpace(strings.SplitN(string(master), "main.m3u8?", 2)[1])
		_, main := e.do(t, 1, "GET", f.main, "", nil)
		for _, line := range strings.Split(string(main), "\n") {
			var n int
			if _, err := fmt.Sscanf(line, "hls1/main/%d.mp4?", &n); err == nil {
				f.segment[n] = path + "/" + line
			}
		}
		for _, n := range []int{2, 3, 5} {
			if f.segment[n] == "" {
				t.Fatalf("the %s playlist has no segment %d:\n%s", flavour.name, n, main)
			}
		}
		f.init = strings.Replace(f.segment[2], "hls1/main/2.mp4", "hls1/main/-1.mp4", 1)
		f.hash = p.hashOf(t, lowerKeys(mustQuery(t, f.segment[2])))
		// What a worker would have stored: the init and segments 2, 3 and 5. The viewers asked for 2 only.
		f.stored = map[int]string{-1: flavour.name + " init", 2: flavour.name + " segment two", 3: flavour.name + " segment three", 5: flavour.name + " segment five"}
		for n, text := range f.stored {
			if err := segments.Put(context.Background(), transcode.SegmentKey(f.hash, n), strings.NewReader(text)); err != nil {
				t.Fatal(err)
			}
		}
		p.films = append(p.films, f)
	}
	return p
}

// hashOf is specHash for a copy too, which needs the file's keyframes.
func (p *playAway) hashOf(t *testing.T, values url.Values) string {
	t.Helper()
	ctx := context.Background()
	source, found, err := p.env.apis[0].Items.Source(ctx, p.item, values.Get("mediasourceid"))
	if err != nil || !found {
		t.Fatal(err)
	}
	media := playbackSource(source)
	if media.Keyframes, media.VideoEnd, err = p.env.apis[0].Items.Keyframes(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	params, err := playback.ParseParams(values, media)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := playback.NewSpec(p.item, params, media)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Hash()
}

var braces = regexp.MustCompile(`\{[^}]*\}`)

// writeFilm puts a file in the directory a file:// store URI names.
func writeFilm(uri, name string, data []byte) error {
	return os.WriteFile(filepath.Join(strings.TrimPrefix(uri, "file://"), name), data, 0o644)
}

// as is a path of the film with the viewer's token and play session.
func (p *playAway) as(f *awayFilm, path string, viewer int) string {
	return p.asToken(f, path, p.tokens[viewer], 0x100+viewer+0x10*f.index)
}

// asToken is as for any token, with the play session number given.
func (p *playAway) asToken(f *awayFilm, path, token string, session int) string {
	path = strings.ReplaceAll(path, p.token, token)
	return strings.Replace(path, "PlaySessionId="+f.psid, fmt.Sprintf("PlaySessionId=%032x", session), 1)
}

// served is one thing a viewer asks a replica for and what it must be answered.
type served struct {
	what    string
	viewer  int
	path    string
	headers []string
	status  int
	body    string
}

// requests are what a viewer who was playing the film through a replica asks it
// for in an outage: ranges it had not asked for, and every segment in the store,
// the ones it had not asked for as well as the ones it had.
func (p *playAway) requests(viewer int) []served {
	var out []served
	for _, r := range p.ranges {
		out = append(out, served{what: "direct play " + r.header, viewer: viewer, path: p.direct(p.tokens[viewer]), headers: []string{"Range", r.header}, status: 206, body: string(p.body[r.from : r.to+1])})
	}
	for _, f := range p.films {
		for _, n := range []int{-1, 2, 3, 5} {
			path := f.segment[n]
			if n == -1 {
				path = f.init
			}
			out = append(out, served{what: fmt.Sprintf("%s segment %d", f.name, n), viewer: viewer, path: p.as(f, path, viewer), status: 200, body: f.stored[n]})
		}
	}
	return out
}

// timings are the window and the refresh interval DOMAINS invariant 37 states, in seconds.
func timings(t *testing.T) (window, refresh int) {
	t.Helper()
	raw, err := os.ReadFile("../../DOMAINS.md")
	if err != nil {
		t.Fatal(err)
	}
	var invariant string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "37. ") {
			invariant = line
		}
	}
	// The same two phrases cmd/jellyscale/outage_docs_test.go reads. The invariant holds other "N seconds" (the
	// usual 90), so a number is never taken for being near a word.
	number := func(pattern string) int {
		m := regexp.MustCompile(pattern).FindStringSubmatch(invariant)
		if m == nil {
			t.Fatalf("invariant 37 does not match %s: it must say \"window of N seconds\" and \"refresh every N seconds\"", pattern)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	window = number(`(?i)\bwindow of (\d{1,3}) seconds`)
	refresh = number(`(?i)\brefresh(?:es)?,? every (\d{1,2}) seconds`)
	if window < 30 || window > 120 || refresh < 1 || refresh > 10 || window < 3*refresh+5 {
		t.Fatalf("invariant 37 states a window of %d seconds and a refresh every %d seconds: want a window of 30 to 120 (jellyfin-web reports progress every ten seconds, paused or not) and a refresh of 1 to 10, and a window of at least three refreshes and 5 seconds", window, refresh)
	}
	return window, refresh
}

// playing is a viewer's player reporting the film started, to the api replica
// the test's env runs (never to a stream replica): what makes its device a
// recently active playback session.
func (p *playAway) playing(t *testing.T, viewer int) {
	t.Helper()
	response, body := p.env.do(t, 0, "POST", "/Sessions/Playing", p.tokens[viewer],
		map[string]any{"ItemId": p.item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	if response.StatusCode/100 != 2 {
		t.Fatalf("viewer %d starting to play: %d %s", viewer, response.StatusCode, body)
	}
}

// seenAgo sets when the device's session was last seen, and that it plays nothing.
func (p *playAway) seenAgo(t *testing.T, token string, ago time.Duration) {
	t.Helper()
	mustExec(t, p.pool, "UPDATE sessions SET item_id = NULL, is_paused = false, last_seen_at = now() - make_interval(secs => $2) WHERE token = $1", token, ago.Seconds())
}

// playingSeenAgo is a session that still plays the film, as one does whose tab was closed or whose device
// crashed (only a Stopped report clears it), and was last seen so long ago: outside the window all the same.
func (p *playAway) playingSeenAgo(t *testing.T, token string, ago time.Duration) {
	t.Helper()
	mustExec(t, p.pool, "UPDATE sessions SET is_paused = false, last_seen_at = now() - make_interval(secs => $2) WHERE token = $1 AND item_id IS NOT NULL", token, ago.Seconds())
	if scalar[int](t, p.pool, "SELECT count(*)::int FROM sessions WHERE token = $1 AND item_id IS NOT NULL", token) != 1 {
		t.Fatal("the session should still be playing")
	}
}

// refreshed is what the test waits for after it changes a row, so that every
// replica's snapshot has been refreshed from it: two intervals and a margin.
func refreshed(refresh int) { time.Sleep(time.Duration(2*refresh)*time.Second + 2*time.Second) }

// ask sends the request now, and keeps what answered, and when.
func ask(ctx context.Context, rp *replica, rq served) <-chan answer {
	out := make(chan answer, 1)
	go func() { out <- get(ctx, rp.server.URL, rq.path, rq.headers...) }()
	return out
}

// answeredWithin checks every request is answered, as it usually is, inside the deadline.
func answeredWithin(t *testing.T, when string, rp *replica, requests []served) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wrong []string
	for _, rq := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), answerDeadline)
			defer cancel()
			a := get(ctx, rp.server.URL, rq.path, rq.headers...)
			var problem string
			switch {
			case a.err != nil:
				problem = fmt.Sprintf("not answered within %v while Postgres is away", answerDeadline)
			case a.status != rq.status || string(a.body) != rq.body:
				problem = fmt.Sprintf("answered %d with %d bytes %.40q, want %d %.40q", a.status, len(a.body), a.body, rq.status, rq.body)
			}
			if problem != "" {
				mu.Lock()
				wrong = append(wrong, fmt.Sprintf("viewer %d: %s: %s", rq.viewer, rq.what, problem))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(wrong) > 0 {
		t.Errorf("%s, on %s: %d of %d requests were wrong; the first:\n  %s", when, rp.server.URL, len(wrong), len(requests), strings.Join(wrong[:min(len(wrong), 3)], "\n  "))
	}
}

// readyz is the status of the replica's readiness.
func readyz(rp *replica) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return get(ctx, rp.server.URL, "/readyz").status
}

// pending is a request left to wait, for the tests that say what must not be
// answered while Postgres is away.
type pending struct {
	what string
	want int // The usual status, once Postgres is back; 0 for any below 500.
	not  int // A status it must not be answered, when want is 0.
	ch   <-chan answer
}

// holdAndCheck waits `hold` of Postgres being away, then says Postgres is back (the
// caller's back does it) and returns when each waiting request was answered. It
// fails for any answered before back() was called, and for an error after.
func holdAndCheck(t *testing.T, hold time.Duration, waiting []pending, back func()) {
	t.Helper()
	time.Sleep(hold)
	backAt := time.Now()
	back()
	var wrong []string
	for _, w := range waiting {
		select {
		case a := <-w.ch:
			switch {
			case a.err != nil:
				wrong = append(wrong, fmt.Sprintf("%s: %v", w.what, a.err))
			case a.at.Before(backAt):
				wrong = append(wrong, fmt.Sprintf("%s was answered %d while Postgres was still away", w.what, a.status))
			case w.want != 0 && a.status != w.want, w.want == 0 && (a.status >= 500 || a.status == w.not):
				wrong = append(wrong, fmt.Sprintf("%s was answered %d once Postgres was back, want %d: %.80s", w.what, a.status, w.want, a.body))
			}
		case <-time.After(90 * time.Second):
			wrong = append(wrong, w.what+" was still not answered 90 seconds after Postgres was back")
		}
	}
	if len(wrong) > 0 {
		t.Errorf("%d of %d waiting requests were wrong; the first:\n  %s", len(wrong), len(waiting), strings.Join(wrong[:min(len(wrong), 12)], "\n  "))
	}
}

// Every stream replica serves a token of a recently active playback session
// whichever replica, or only the api, it was seen through: any range of the file
// and any init or media segment in the store, of a copy and of a re-encode,
// within 5 seconds, from the request that is first to find Postgres gone and
// those in flight, and again at 40 seconds, after /readyz of the replica answers
// 503, from several viewers on each of two replicas at once. Viewer 0's requests
// before the outage went to stream replica 0 only, viewer 1's to the api only,
// viewer 2's to stream replica 1 only; each is asked of both stream replicas.
func TestAnyStreamReplicaServesAViewerWhoseRequestsWentElsewhere(t *testing.T) {
	p := newPlayAway(t, 3)
	_, refresh := timings(t)
	for v := range p.tokens {
		p.playing(t, v) // The api: what makes the device a recently active session.
	}
	for v, rp := range map[int]*replica{0: p.streams[0], 2: p.streams[1]} {
		if a := get(context.Background(), rp.server.URL, p.direct(p.tokens[v]), "Range", "bytes=100-299"); a.status != 206 {
			t.Fatalf("direct play for viewer %d before the outage: %d", v, a.status)
		}
	}
	refreshed(refresh)
	// One replica finds Postgres away through a request that meets its closed
	// connections (what the first request of an outage does); the other is ended
	// the way a switchover does, with Postgres's own FATAL.
	p.streams[0].proxy.Away()
	away := time.Now()
	var first sync.WaitGroup
	for i, rp := range p.streams {
		if i == 1 {
			rp.away(t, p.pool)
		}
		for v := range p.tokens {
			first.Add(1)
			go func() {
				defer first.Done()
				answeredWithin(t, "at the start of the outage", rp, p.requests(v))
			}()
		}
	}
	first.Wait()

	// 40 seconds in: readiness has failed, and the film still plays.
	time.Sleep(max(0, 40*time.Second-time.Since(away)))
	var late sync.WaitGroup
	for _, rp := range p.streams {
		if got := readyz(rp); got != http.StatusServiceUnavailable {
			t.Errorf("%s answered /readyz %d at 40 seconds, want 503 (it has been cut off for 30)", rp.server.URL, got)
		}
		for v := range p.tokens {
			late.Add(1)
			go func() {
				defer late.Done()
				answeredWithin(t, fmt.Sprintf("%v into the outage, past readiness", time.Since(away).Round(time.Second)), rp, p.requests(v))
			}()
		}
	}
	late.Wait()
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
}

// While Postgres is away a replica sends no playlist, segment or film byte to a
// token that is not in its snapshot: one Postgres never knew, one whose session
// was last seen an hour ago or a few seconds past the window (whether it still
// plays the film or not: a session abandoned mid-film stays "playing" for ever),
// one whose device
// has no session at all (never played), one signed out before the last refresh.
// Each waits, on either replica, and is answered when Postgres is back as
// Postgres says: its usual answer if the device is signed in, 401 if not. Two
// controls are served at once: a session seen just before, and one seen just
// before and signed out only after the outage began (the owner accepted that).
func TestAReplicaServesNothingToATokenOutsideItsSnapshot(t *testing.T) {
	p := newPlayAway(t, 1)
	window, refresh := timings(t)
	names := []string{"old", "justpast", "stillplaying", "abandoned", "neverplayed", "leftbefore", "fresh", "paused", "leftafter"}
	tokens := map[string]string{"stranger": "not-a-token-at-all"}
	for _, name := range names {
		_, tokens[name] = p.addUser(t, name)
		if name != "neverplayed" {
			response, body := p.env.do(t, 0, "POST", "/Sessions/Playing", tokens[name],
				map[string]any{"ItemId": p.item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
			if response.StatusCode/100 != 2 {
				t.Fatalf("%s starting to play: %d %s", name, response.StatusCode, body)
			}
		}
	}
	p.seenAgo(t, tokens["old"], time.Hour)
	p.seenAgo(t, tokens["justpast"], time.Duration(window+5)*time.Second)
	// Playing is not enough: a session still marked as playing that nothing has reported on for longer than the
	// window is outside it (jellyfin-web reports progress every ten seconds, paused or not, so a live player never is).
	p.playingSeenAgo(t, tokens["stillplaying"], time.Duration(window+5)*time.Second)
	p.playingSeenAgo(t, tokens["abandoned"], 7*24*time.Hour)
	// A paused player is a recent session that plays: served.
	mustExec(t, p.pool, "UPDATE sessions SET is_paused = true WHERE token = $1", tokens["paused"])
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", tokens["leftbefore"])
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", tokens["leftafter"])

	// What each asks for, with a play session of its own.
	film := p.films[1]
	session := 0x900
	kinds := func(token string) []served {
		session++
		path := func(s string) string { return p.asToken(film, s, token, session) }
		return []served{
			{what: "master", path: path(film.master), status: 200}, {what: "main playlist", path: path(film.main), status: 200},
			{what: "init segment", path: path(film.init), status: 200, body: film.stored[-1]},
			{what: "a stored segment", path: path(film.segment[2]), status: 200, body: film.stored[2]},
			{what: "direct play", path: p.direct(token), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])},
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	signedIn := map[string]bool{"old": true, "justpast": true, "stillplaying": true, "abandoned": true, "neverplayed": true}
	for _, name := range []string{"old", "justpast", "stillplaying", "abandoned", "neverplayed", "leftbefore", "stranger"} {
		for i, rp := range p.streams {
			for _, rq := range kinds(tokens[name]) {
				w := pending{what: fmt.Sprintf("%s on replica %d, %s", name, i, rq.what), ch: ask(ctx, rp, rq)}
				if signedIn[name] {
					w.not = 401
				} else {
					w.want = 401
				}
				waiting = append(waiting, w)
			}
		}
	}
	// The controls: served at once, on both replicas (not the playlists, which the criterion leaves out).
	for _, name := range []string{"fresh", "paused", "leftafter"} {
		for _, rp := range p.streams {
			var control []served
			for _, rq := range kinds(tokens[name]) {
				if rq.what != "master" && rq.what != "main playlist" {
					control = append(control, rq)
				}
			}
			answeredWithin(t, "the token of a session seen just before the outage ("+name+")", rp, control)
		}
	}
	holdAndCheck(t, 8*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}

// A snapshot is replaced by the next refresh, and being served from it is not a
// validation. A token signed out after the last refresh, served through an
// outage longer than the window and not asked about once Postgres was back,
// gets nothing in a second outage once a refresh has run in between: it waits,
// and is answered 401 when Postgres is back.
func TestBeingServedFromTheSnapshotIsNotAValidation(t *testing.T) {
	p := newPlayAway(t, 2)
	window, refresh := timings(t)
	film := p.films[1]
	segment := func(token string) string {
		return strings.ReplaceAll(strings.Replace(film.segment[2], "PlaySessionId="+film.psid, fmt.Sprintf("PlaySessionId=%032x", 0x777), 1), p.token, token)
	}
	direct := served{what: "direct play", path: p.direct(p.tokens[1]), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])}
	stored := served{what: "a stored segment", path: segment(p.tokens[1]), status: 200, body: film.stored[2]}
	p.playing(t, 1)
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	away := time.Now()
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", p.tokens[1]) // After the last refresh, which was before the outage.
	for _, rp := range p.streams {
		answeredWithin(t, "at the start of the first outage", rp, []served{direct, stored})
	}
	time.Sleep(max(0, time.Duration(window+10)*time.Second-time.Since(away))) // Longer than the window: only the snapshot can have served it so far.
	for _, rp := range p.streams {
		answeredWithin(t, "past the window into the first outage", rp, []served{direct, stored})
		rp.proxy.Back()
	}
	// Postgres is back, and each replica is told so by a request that needs it
	// (the film's own token, which Postgres knows). Nobody asks about the leaver.
	for _, rp := range p.streams {
		waitLong(t, "the replica to find Postgres back", func() bool {
			return get(context.Background(), rp.server.URL, p.direct(p.token), "Range", "bytes=0-9").status == 206
		})
	}
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for i, rp := range p.streams {
		waiting = append(waiting,
			pending{what: fmt.Sprintf("replica %d, direct play", i), want: 401, ch: ask(ctx, rp, direct)},
			pending{what: fmt.Sprintf("replica %d, a stored segment", i), want: 401, ch: ask(ctx, rp, stored)})
	}
	holdAndCheck(t, 8*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}

// Once Postgres answers again every request is decided by Postgres. A token
// signed out after the last refresh and served through the outage is answered
// 401 by each stream replica soon after (the criterion says as soon as Postgres is
// back; the wait is bounded at 100 seconds, which only stops a hang, and logs how
// long each took), and with Postgres up again nothing is answered from the
// snapshot: a token of an active session that is signed out is refused by its
// very next request, and a replaced file is served from its new row.
func TestOncePostgresIsBackEveryRequestIsDecidedByPostgres(t *testing.T) {
	p := newPlayAway(t, 2)
	_, refresh := timings(t)
	p.playing(t, 1)
	refreshed(refresh)
	leaver := p.tokens[1]
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", leaver)
	for _, rp := range p.streams {
		answeredWithin(t, "during the outage", rp, []served{{what: "direct play", path: p.direct(leaver), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])}})
	}
	time.Sleep(2 * time.Second)
	back := time.Now()
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
	for _, rp := range p.streams {
		waitLong(t, "the signed-out token to be refused by "+rp.server.URL, func() bool {
			return get(context.Background(), rp.server.URL, p.direct(leaver), "Range", "bytes=0-9").status == 401
		})
		t.Logf("%s refused the signed-out token %v after Postgres was back", rp.server.URL, time.Since(back).Round(100*time.Millisecond))
	}
	// Settled, with a snapshot that holds an active session's token: signed out now, the next request is refused.
	_, second := p.addUser(t, "second")
	response, body := p.env.do(t, 0, "POST", "/Sessions/Playing", second,
		map[string]any{"ItemId": p.item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	if response.StatusCode/100 != 2 {
		t.Fatalf("second starting to play: %d %s", response.StatusCode, body)
	}
	refreshed(refresh)
	for _, rp := range p.streams {
		if a := get(context.Background(), rp.server.URL, p.direct(second), "Range", "bytes=0-9"); a.status != 206 {
			t.Fatalf("after: %d", a.status)
		}
	}
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", second)
	for _, rp := range p.streams {
		if a := get(context.Background(), rp.server.URL, p.direct(second), "Range", "bytes=0-9"); a.status != 401 {
			t.Errorf("%s: the next request of a token signed out after the outage was answered %d, want 401 (nothing is answered from the snapshot with Postgres up)", rp.server.URL, a.status)
		}
	}
	replacement := bytes.Repeat([]byte("replaced-file!!!"), 4096)
	if err := writeFilm(p.uri, "film2.mkv", replacement); err != nil {
		t.Fatal(err)
	}
	mustExec(t, p.pool, "UPDATE media_sources SET store_key = 'film2.mkv', object_etag = 'new', size = $1", len(replacement))
	for _, rp := range p.streams {
		if a := get(context.Background(), rp.server.URL, p.direct(p.token), "Range", "bytes=0-9"); a.status != 206 || !bytes.Equal(a.body, replacement[:10]) {
			t.Errorf("%s: the next range after the row was replaced was answered %d %q, want the new file's bytes", rp.server.URL, a.status, a.body)
		}
	}
}

// Nothing else is answered from the snapshot, and none of it is answered 400 or
// more because of the outage, in a process that runs both roles: a segment that
// is not in the store, a film the session is not playing, a new login,
// DELETE /Videos/ActiveEncodings, and every route of the api role wait, and are
// answered as usual when Postgres is back. No job is started or changed
// meanwhile. The control, in the same outage and process, is a range of the film
// the token's session plays and a segment in the store, which are served.
func TestNothingElseIsAnsweredFromTheSnapshotAndEveryApiRouteWaits(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	both := newReplica(t, p.env, p.segments, "api,stream")
	// A second film, which no session plays.
	other := "00000000-0000-4000-8000-000000000b0b"
	mustExec(t, p.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		SELECT $1, library_id, type, 'Other', 'other', 'other', run_time_ticks, dto FROM items WHERE id = $2`, other, p.item)
	mustExec(t, p.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		SELECT $1, $1, store_key, size, object_etag, container, streams FROM media_sources WHERE id = $2`, other, p.item)
	film := p.films[1]
	p.playing(t, 0)
	if a := get(context.Background(), both.server.URL, "/Users/Me", "Authorization", `MediaBrowser Token="`+p.token+`"`); a.status != 200 {
		t.Fatalf("/Users/Me before the outage: %d", a.status)
	}
	refreshed(refresh)
	token := `MediaBrowser Token="` + p.token + `"`
	// Each api route as the player or the web client would call it with nothing but the token: what it
	// answers with Postgres up is what it must answer once Postgres is back (a request with no body is
	// refused by some, and the one that signs the device out is left out).
	type call struct{ route, method, path string }
	var calls []call
	for _, route := range (&Server{}).Routes() {
		if route.Role != RoleAPI || route.Public || strings.HasSuffix(route.Path, "/Logout") {
			continue
		}
		path := route.Path
		for _, name := range []string{"{itemId}", "{userId}", "{seriesId}", "{preferenceId}", "{id}"} {
			path = strings.ReplaceAll(path, name, p.item)
		}
		calls = append(calls, call{route.Method + " " + route.Path, route.Method, braces.ReplaceAllString(path, "x")})
	}
	if len(calls) < 20 {
		t.Fatalf("only %d api routes found", len(calls))
	}
	usualStatus := map[string]int{}
	for _, c := range calls {
		usualStatus[c.route] = send(context.Background(), c.method, both.server.URL+c.path, "Authorization", token).status
	}
	jobs := func() string {
		return scalar[string](t, p.pool, "SELECT coalesce(string_agg(id || ':' || state || ':' || start_segment || ':' || next_segment, ',' ORDER BY id), '') FROM transcode_jobs")
	}
	before := jobs()
	both.away(t, p.pool)

	// The control: served.
	answeredWithin(t, "the film the session plays", both, p.requests(0)[:3])
	answeredWithin(t, "the film the session plays", both, []served{{what: "re-encode segment 3", path: p.as(film, film.segment[3], 0), status: 200, body: film.stored[3]}})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var waiting []pending
	add := func(what string, want int, rq served) {
		waiting = append(waiting, pending{what: what, want: want, ch: ask(ctx, both, rq)})
	}
	add("a segment that is not in the store", 200, served{path: p.as(film, film.segment[7], 0)})
	add("a film no session plays", 206, served{path: strings.Replace(p.direct(p.token), db.CompactID(p.item), db.CompactID(other), 2), headers: []string{"Range", "bytes=0-9"}})
	query := "/Videos/ActiveEncodings?deviceId=dev1&playSessionId=" + film.psid
	waiting = append(waiting, pending{what: "DELETE ActiveEncodings", want: 204, ch: func() <-chan answer {
		out := make(chan answer, 1)
		go func() { out <- send(ctx, "DELETE", both.server.URL+query, "Authorization", token) }()
		return out
	}()})
	// A new token is never made from memory: a login while Postgres is away is answered no 2xx. (What it is
	// answered instead, 401 or 500, is not this task's; the api routes above are the ones that must wait.)
	loginCtx, stopLogin := context.WithTimeout(ctx, 20*time.Second)
	defer stopLogin()
	if a := sendBody(loginCtx, "POST", both.server.URL+"/Users/AuthenticateByName", `{"Username":"nobody","Pw":"x"}`, "Content-Type", "application/json",
		"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="new", Version="1"`); a.err == nil && a.status/100 == 2 {
		t.Errorf("a login while Postgres was away was answered %d: %s", a.status, a.body)
	}
	for _, c := range calls {
		want := usualStatus[c.route]
		if want >= 500 {
			want = 0
		}
		waiting = append(waiting, pending{what: c.route, want: want, ch: func() <-chan answer {
			out := make(chan answer, 1)
			go func() { out <- send(ctx, c.method, both.server.URL+c.path, "Authorization", token) }()
			return out
		}()})
	}
	// The usual answers once Postgres is back: the segment that was missing is made by a worker.
	stopWorker := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopWorker:
				return
			default:
			}
			rows, err := p.pool.Query(context.Background(), "SELECT id, spec_hash, start_segment FROM transcode_jobs WHERE state = 'queued'")
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
					p.segments.Put(context.Background(), transcode.SegmentKey(q.hash, q.start), strings.NewReader("made"))                                             //nolint:errcheck
					p.pool.Exec(context.Background(), `UPDATE transcode_jobs SET state = 'done', worker = 'w1', next_segment = start_segment + 1 WHERE id = $1`, q.id) //nolint:errcheck
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	defer close(stopWorker)
	holdAndCheck(t, 8*time.Second, waiting, func() {
		if after := jobs(); after != before {
			t.Errorf("a job was started or changed while Postgres was away:\nbefore %s\nafter  %s", before, after)
		}
		both.proxy.Back()
	})
}

// What a refresh reads is small rows per playing session; a film's stream list
// and keyframe index (megabytes for a long film, and changed only by a rescan)
// are read when the replica does not hold them or they changed, not every few
// seconds. The film gets an index of 200000 keyframes (1.6 MB as bigints), then
// each stream replica idles through three refresh intervals and more: it may
// read less than a sixteenth of one copy of the index in all (100 KB), which a
// refresh that reads the index again every time, or the stream list and index
// of every playing film, exceeds several times. Bytes are counted at the proxy
// (the mechanism, not how long a refresh took). Then the replicas go away, to
// show they did read the index once: the copy and the re-encode are served.
func TestARefreshDoesNotReadAFilmsIndexAgainWhileItIsUnchanged(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	const keyframes = 200000
	mustExec(t, p.pool, "UPDATE media_sources SET keyframes = (SELECT array_agg(g::bigint * 10000000) FROM generate_series(0, $1::int - 1) g), video_end = $1::bigint * 10000000", keyframes)
	p.playing(t, 0)
	refreshed(refresh) // The replica reads the new index once, in the first refresh after the change.
	var before []int64
	for _, rp := range p.streams {
		before = append(before, rp.proxy.Received())
	}
	time.Sleep(time.Duration(3*refresh+1) * time.Second)
	for i, rp := range p.streams {
		read := rp.proxy.Received() - before[i]
		t.Logf("replica %d read %d bytes from Postgres over %d seconds while nothing changed", i, read, 3*refresh+1)
		if limit := int64(keyframes * 8 / 16); read > limit {
			t.Errorf("replica %d read %d bytes from Postgres in %d idle seconds, which is more than %d (a sixteenth of one copy of the film's keyframe index): a refresh reads what has not changed", i, read, 3*refresh+1, limit)
		}
	}
	for _, rp := range p.streams {
		rp.away(t, p.pool)
		answeredWithin(t, "with Postgres away, after refreshes that read the index once", rp, p.requests(0))
	}
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
}

// A refresh that reads a film's stream list and keyframe index only when they
// changed must still notice a change: a rescan that finds more keyframes (the
// source row's etag unchanged), then a replaced file (a new etag, so every
// stored segment of it is under a new hash). With Postgres away afterwards each
// stream replica serves the stored segments of the film as it is now, not as it
// was when the replica first read it: the last segment of the longer copy, which
// the old index has no segment for, and the segments under the new hash (the old
// hash's are in the store too, with other bytes).
func TestARefreshPicksUpAFilmThatChanged(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	copyFilm, encodeFilm := p.films[0], p.films[1]
	p.playing(t, 0)
	refreshed(refresh) // Every replica holds the film as newPlayAway left it.
	backUp := func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
		for _, rp := range p.streams {
			waitLong(t, "the replica to find Postgres back", func() bool {
				return get(context.Background(), rp.server.URL, p.direct(p.token), "Range", "bytes=0-9").status == 206
			})
		}
	}

	// A rescan finds a longer index: the copy has more segments. The etag is the same.
	var index []int64
	for k := range 30 {
		index = append(index, int64(k)*50000000)
	}
	end := int64(30) * 50000000
	layout, ok := playback.CopyLayout(index, end-index[0])
	if !ok {
		t.Fatal("the longer index makes no copy layout")
	}
	last := layout.Count() - 1
	if last <= 8 {
		t.Fatalf("the longer index has only %d segments: the test needs more than the old index's", last+1)
	}
	mustExec(t, p.pool, "UPDATE media_sources SET keyframes = $1, video_end = $2", index, end)
	lastPath := strings.Replace(copyFilm.segment[2], "hls1/main/2.mp4", fmt.Sprintf("hls1/main/%d.mp4", last), 1)
	if err := p.segments.Put(context.Background(), transcode.SegmentKey(copyFilm.hash, last), strings.NewReader("copy last segment")); err != nil {
		t.Fatal(err)
	}
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	for _, rp := range p.streams {
		answeredWithin(t, "after a rescan found more keyframes, with Postgres away", rp,
			[]served{{what: fmt.Sprintf("copy segment %d, which the first index has no segment for", last), path: p.as(copyFilm, lastPath, 0), status: 200, body: "copy last segment"}})
	}
	backUp()

	// The file is replaced: a new etag, so a new hash for each film, with segments stored under it.
	mustExec(t, p.pool, "UPDATE media_sources SET object_etag = 'replaced-in-place'")
	for _, f := range []*awayFilm{copyFilm, encodeFilm} {
		hash := p.hashOf(t, lowerKeys(mustQuery(t, f.segment[2])))
		if hash == f.hash {
			t.Fatalf("the %s hash did not change with the etag", f.name)
		}
		if err := p.segments.Put(context.Background(), transcode.SegmentKey(hash, 2), strings.NewReader(f.name+" segment two, replaced file")); err != nil {
			t.Fatal(err)
		}
	}
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	for _, rp := range p.streams {
		var want []served
		for _, f := range []*awayFilm{copyFilm, encodeFilm} {
			want = append(want, served{what: f.name + " segment 2", path: p.as(f, f.segment[2], 0), status: 200, body: f.name + " segment two, replaced file"})
		}
		answeredWithin(t, "after the file was replaced, with Postgres away", rp, want)
	}
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
}

// send is get with a method.
func send(ctx context.Context, method, url string, headers ...string) answer {
	return sendBody(ctx, method, url, "", headers...)
}

// sendBody is send with a body.
func sendBody(ctx context.Context, method, url, payload string, headers ...string) answer {
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(payload))
	if err != nil {
		return answer{err: err, at: time.Now()}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return answer{err: err, at: time.Now()}
	}
	defer response.Body.Close()
	var body bytes.Buffer
	body.ReadFrom(response.Body) //nolint:errcheck
	return answer{status: response.StatusCode, body: body.Bytes(), at: time.Now()}
}
