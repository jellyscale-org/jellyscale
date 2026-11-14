//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/db"
)

// JS-103. JS-085's snapshot kept direct play and stored segments going for a viewer seen in the last 60 seconds and
// known to a replica's last refresh. Two kinds of viewer fell outside it: one whose film was paused in a tab that
// reports rarely, and one who began in the seconds before the outage. These tests put Postgres away for real, as
// JS-085's do (testenv.DBProxy through the playAway env), and read the window from DOMAINS invariant 37.
//
// What a start 1 second before the outage waits is a fixed gap the criterion names, not a duration measured on this
// machine, and a replica that learns of a start with a notification, or by refreshing every second, serves it; one
// that learns on its 5 second refresh serves it only 4 times in 5, which is what these tests are for.
const startedBefore = time.Second

// A viewer who paused the film up to 10 minutes before the outage, with no report from it reaching Postgres since,
// resumes it during the outage from a replica that served it nothing before: any range of the film, and every
// segment the store holds, of a copy and a re-encode. The sessions here were last seen 90 seconds, 5 minutes and
// (window - 1 minute) ago, which a 60 second window does not hold; one last seen half a minute past the window is
// outside it and waits for Postgres.
func TestAViewerPausedForMinutesBeforeTheOutageIsServedFromAnyReplica(t *testing.T) {
	p := newPlayAway(t, 5)
	window, refresh := timings(t)
	ages := map[int]time.Duration{0: 90 * time.Second, 1: 5 * time.Minute, 2: time.Duration(window)*time.Second - time.Minute, 3: time.Duration(window)*time.Second + 30*time.Second}
	for v, ago := range ages {
		p.playing(t, v)
		mustExec(t, p.pool, "UPDATE sessions SET is_paused = true, position_ticks = 1200000000, last_seen_at = now() - make_interval(secs => $2) WHERE token = $1", p.tokens[v], ago.Seconds())
	}
	// Viewer 4 is the control that makes the replicas hold a snapshot at all: playing now.
	p.playing(t, 4)
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	away := time.Now()
	for _, rp := range p.streams {
		for v := 0; v < 3; v++ {
			answeredWithin(t, fmt.Sprintf("a film paused %v before the outage", ages[v]), rp, p.requests(v))
		}
		answeredWithin(t, "the control, playing now", rp, p.requests(4))
	}

	// Past readiness the film still plays (JS-085's own check, for a viewer the window newly holds).
	time.Sleep(max(0, 35*time.Second-time.Since(away)))
	for _, rp := range p.streams {
		answeredWithin(t, "past readiness, for a film paused 5 minutes before", rp, p.requests(1))
	}

	// A session last seen past the window is in no snapshot: it waits, and is answered once Postgres is back.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for i, rp := range p.streams {
		rq := p.requests(3)[0]
		waiting = append(waiting, pending{what: fmt.Sprintf("a film last seen past the window, on replica %d", i), want: 206, ch: ask(ctx, rp, rq)})
	}
	holdAndCheck(t, 5*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}

// justRefreshed returns just after the replica asked Postgres something unasked: its refresh, the only thing it does
// with no request to answer. The bytes it received from Postgres are counted at the proxy, as in JS-085's checks of
// what a refresh reads (the mechanism, not how long anything took).
func justRefreshed(t *testing.T, rp *replica) {
	t.Helper()
	before := rp.proxy.Received()
	waitLong(t, "the replica to refresh its snapshot", func() bool { return rp.proxy.Received() > before })
}

// secondFilm adds a film no viewer plays yet and says how to ask for its bytes.
func (p *playAway) secondFilm(t *testing.T) (item string, direct func(token string) string) {
	t.Helper()
	item = "00000000-0000-4000-8000-00000000c0de"
	mustExec(t, p.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		SELECT $1, library_id, type, 'Second', 'second', 'second', run_time_ticks, dto FROM items WHERE id = $2`, item, p.item)
	mustExec(t, p.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		SELECT $1, $1, store_key, size, object_etag, container, streams FROM media_sources WHERE id = $2`, item, p.item)
	compact := db.CompactID(item)
	return item, func(token string) string {
		return "/Videos/" + compact + "/stream.mkv?Static=true&mediaSourceId=" + compact + "&ApiKey=" + token
	}
}

// startsPlaying is a viewer's player reporting the start of an item, to the api.
func (p *playAway) startsPlaying(t *testing.T, viewer int, item string) {
	t.Helper()
	response, body := p.env.do(t, 0, "POST", "/Sessions/Playing", p.tokens[viewer],
		map[string]any{"ItemId": item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	if response.StatusCode/100 != 2 {
		t.Fatalf("viewer %d starting %s: %d %s", viewer, item, response.StatusCode, body)
	}
}

// Films that began a second before Postgres stopped play on: direct play ranges the player had not asked for, and
// every stored segment, from both stream replicas (neither saw the viewers), for several viewers who start within
// that second, and for a viewer who was in the snapshot with another film and starts this one: the new film is
// served and the old one no longer is (it waits, and is answered once Postgres is back).
func TestAFilmStartedASecondBeforeTheOutagePlaysOn(t *testing.T) {
	p := newPlayAway(t, 4)
	_, refresh := timings(t)
	other, otherDirect := p.secondFilm(t)
	// Viewer 3 plays the other film long enough ago to be in every snapshot.
	p.startsPlaying(t, 3, other)
	refreshed(refresh)
	for _, rp := range p.streams {
		if a := get(context.Background(), rp.server.URL, otherDirect(p.tokens[3]), "Range", "bytes=0-9"); a.status != 206 {
			t.Fatalf("viewer 3 before the outage, the film it plays: %d", a.status)
		}
	}
	// The starts come right after replica 0's refresh, the worst moment for a build that learns of a start by
	// refreshing: its next refresh is a whole interval away, so only a build that learns another way serves them.
	// Replica 1's refresh is at some other moment of its interval, which it may or may not have missed.
	justRefreshed(t, p.streams[0])
	for _, v := range []int{0, 1, 2, 3} {
		p.startsPlaying(t, v, p.item)
	}
	time.Sleep(startedBefore)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	for _, rp := range p.streams {
		for v := 0; v < 4; v++ {
			answeredWithin(t, fmt.Sprintf("viewer %d, whose film began %v before the outage", v, startedBefore), rp, p.requests(v))
		}
	}
	// Viewer 3's old film is no longer the one its session plays.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for i, rp := range p.streams {
		waiting = append(waiting, pending{what: fmt.Sprintf("the film viewer 3 stopped playing, on replica %d", i), want: 206,
			ch: ask(ctx, rp, served{path: otherDirect(p.tokens[3]), headers: []string{"Range", "bytes=0-9"}})})
	}
	holdAndCheck(t, 5*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}

// A start is never added to a snapshot without Postgres having accepted its token: a device signed out answers
// the start report 401, and with Postgres away the same token waits for it on both replicas, and is answered 401
// once it is back. A control who starts at the same time is served.
func TestAStartThatPostgresRefusedAddsNobodyToTheSnapshot(t *testing.T) {
	p := newPlayAway(t, 2)
	_, refresh := timings(t)
	_, leaver := p.addUser(t, "leaver")
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", leaver)
	response, _ := p.env.do(t, 0, "POST", "/Sessions/Playing", leaver,
		map[string]any{"ItemId": p.item, "PositionTicks": 0, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	if response.StatusCode != 401 {
		t.Fatalf("the start report of a signed-out device was answered %d, want 401", response.StatusCode)
	}
	// Each replica is asked for the film with the refused token while Postgres answers (and refuses it), so a build that
	// remembers a token from the request that carried it, or from a start report in flight, has it to serve.
	for _, rp := range p.streams {
		if a := get(context.Background(), rp.server.URL, p.direct(leaver), "Range", "bytes=0-9"); a.status != 401 {
			t.Fatalf("%s: the signed-out device's range with Postgres up was answered %d, want 401", rp.server.URL, a.status)
		}
	}
	p.startsPlaying(t, 1, p.item)
	refreshed(refresh)
	justRefreshed(t, p.streams[0])
	p.startsPlaying(t, 0, p.item)
	time.Sleep(startedBefore)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	for _, rp := range p.streams {
		answeredWithin(t, "the control that started with the signed-out one", rp, p.requests(1)[:3])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for i, rp := range p.streams {
		rq := served{path: p.direct(leaver), headers: []string{"Range", "bytes=0-9"}}
		waiting = append(waiting, pending{what: fmt.Sprintf("the signed-out device's range, on replica %d", i), want: 401, ch: ask(ctx, rp, rq)})
		seg := p.as(p.films[1], p.films[1].segment[2], 0)
		waiting = append(waiting, pending{what: fmt.Sprintf("the signed-out device's segment, on replica %d", i), want: 401,
			ch: ask(ctx, rp, served{path: strings.ReplaceAll(seg, p.tokens[0], leaver)})})
	}
	holdAndCheck(t, 5*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}

// When Postgres answers again after an outage past readiness, a film that played through it is not cut or refused
// at the switch back. A viewer reads the film slowly from a replica from before Postgres stops until after it is
// back (a range in flight across the return), and from the moment of the return, for several seconds, both replicas
// are asked every few milliseconds for a range and a stored segment by a viewer the snapshot holds and by one whose
// session is outside the window (which waited all the outage): nothing is answered 400 or more, nothing is cut, the
// slow read ends with every byte of the film, and a token signed out just before the outage is refused by the very
// first request after the return. The outage is held past 30 seconds (readiness), as the e2e scenarios hold it.
func TestAFilmInFlightAndTheRequestsAfterTheReturnAreNeitherCutNorRefused(t *testing.T) {
	p := newPlayAway(t, 4)
	window, refresh := timings(t)
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MB, more than a socket holds, so the slow read stays in flight.
	if err := writeFilm(p.uri, "film3.mkv", big); err != nil {
		t.Fatal(err)
	}
	// Viewer 1 plays a second film that is the big file, so the first film's stored segments keep their hash.
	other, otherDirect := p.secondFilm(t)
	mustExec(t, p.pool, "UPDATE media_sources SET store_key = 'film3.mkv', object_etag = 'big', size = $2 WHERE id = $1", other, len(big))
	for _, v := range []int{0, 2, 3} {
		p.playing(t, v)
	}
	p.startsPlaying(t, 1, other)
	// Viewer 2's session is outside the window, so it waits all the outage; viewer 3 signs out just before it.
	mustExec(t, p.pool, "UPDATE sessions SET last_seen_at = now() - make_interval(secs => $2) WHERE token = $1", p.tokens[2], float64(window+30))
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	mustExec(t, p.pool, "DELETE FROM devices WHERE token = $1", p.tokens[3])
	away := time.Now()

	// The slow read of the big film, begun in the outage from the replica that has viewer 1 in its snapshot.
	type read struct {
		status int
		n      int
		sum    [32]byte
		err    error
	}
	slow := make(chan read, 1)
	go func() {
		response, err := http.Get(p.streams[0].server.URL + otherDirect(p.tokens[1]))
		if err != nil {
			slow <- read{err: err}
			return
		}
		defer response.Body.Close()
		hash := sha256.New()
		buf := make([]byte, 8<<10)
		n := 0
		for {
			m, err := response.Body.Read(buf)
			hash.Write(buf[:m])
			n += m
			if err != nil {
				var sum [32]byte
				copy(sum[:], hash.Sum(nil))
				if err == io.EOF {
					err = nil
				}
				slow <- read{status: response.StatusCode, n: n, sum: sum, err: err}
				return
			}
			time.Sleep(25 * time.Millisecond) // About 300 KB a second: 16 MB take nearly a minute, across the return at 35 seconds.
		}
	}()
	time.Sleep(max(0, 35*time.Second-time.Since(away)))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for i, rp := range p.streams {
		waiting = append(waiting, pending{what: fmt.Sprintf("the viewer outside the window, on replica %d", i), want: 206, ch: ask(ctx, rp, served{path: p.direct(p.tokens[2]), headers: []string{"Range", "bytes=0-9"}})})
	}
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
	backAt := time.Now()
	// Every few milliseconds, for 8 seconds, each replica is asked what a player asks; none may be refused.
	var wrong []string
	asked := 0
	for time.Since(backAt) < 8*time.Second {
		for i, rp := range p.streams {
			for _, rq := range []served{
				{what: "range", path: p.direct(p.tokens[0]), headers: []string{"Range", "bytes=1000-1999"}, status: 206},
				{what: "segment", path: p.as(p.films[1], p.films[1].segment[2], 0), status: 200},
				{what: "range of the viewer outside the window", path: p.direct(p.tokens[2]), headers: []string{"Range", "bytes=0-9"}, status: 206},
			} {
				asked++
				c, stop := context.WithTimeout(context.Background(), 20*time.Second)
				got := get(c, rp.server.URL, rq.path, rq.headers...)
				stop()
				if got.err != nil || got.status != rq.status {
					wrong = append(wrong, fmt.Sprintf("%v after the return, replica %d, %s: %d %v", time.Since(backAt).Round(time.Millisecond), i, rq.what, got.status, got.err))
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(wrong) > 0 {
		t.Errorf("%d of %d requests after the return were refused or cut; the first:\n  %s", len(wrong), asked, strings.Join(wrong[:min(len(wrong), 6)], "\n  "))
	}
	// The device signed out just before the outage was served from the snapshot during it (the owner accepted that) and
	// is refused as soon as the replica has Postgres again, and stays refused.
	for _, rp := range p.streams {
		waitLong(t, "the signed-out token to be refused by "+rp.server.URL, func() bool {
			return get(context.Background(), rp.server.URL, p.direct(p.tokens[3]), "Range", "bytes=0-9").status == 401
		})
		if a := get(context.Background(), rp.server.URL, p.direct(p.tokens[3]), "Range", "bytes=0-9"); a.status != 401 {
			t.Errorf("%s: the signed-out token was answered %d after it had been refused", rp.server.URL, a.status)
		}
	}
	for _, w := range waiting {
		select {
		case got := <-w.ch:
			if got.err != nil || got.status != w.want {
				t.Errorf("%s: answered %d (%v) once Postgres was back, want %d", w.what, got.status, got.err, w.want)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("%s: still not answered 30 seconds after Postgres was back", w.what)
		}
	}
	select {
	case r := <-slow:
		want := sha256.Sum256(big)
		if r.err != nil || r.status != 200 || r.n != len(big) || r.sum != want {
			t.Errorf("the film read across the return: status %d, %d of %d bytes, err %v, intact %v: it was cut or changed", r.status, r.n, len(big), r.err, r.sum == want)
		}
	case <-time.After(60 * time.Second):
		t.Error("the slow read of the film did not end within a minute of the return")
	}
}
