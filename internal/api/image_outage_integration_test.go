//go:build integration

package api

import (
	"net/http"
	"testing"
	"time"

	"jellyscale/internal/db"
)

// A stream replica answers an item's image as usual once Postgres is back, and
// never 500 while it is away: the route is public, so requireAuth's wait does
// not cover it (JS-029, found by the UX review).
func TestAnItemImageWaitsForPostgresInsteadOfFailing(t *testing.T) {
	o := newOutage(t)
	rp := o.streams[0]
	path := rp.server.URL + "/Items/" + db.CompactID(o.item) + "/Images/Primary"
	get := func() int {
		response, err := http.Get(path)
		if err != nil {
			t.Error(err)
			return 0
		}
		response.Body.Close()
		return response.StatusCode
	}
	before := get()

	rp.away(t, o.pool)
	answered := make(chan int, 1)
	go func() { answered <- get() }()
	select {
	case status := <-answered:
		t.Fatalf("answered %d while Postgres was away, before it came back (before the outage: %d)", status, before)
	case <-time.After(2 * time.Second):
	}
	rp.proxy.Back()
	select {
	case status := <-answered:
		if status != before {
			t.Errorf("answered %d once Postgres was back, %d before the outage", status, before)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no answer 30 s after Postgres came back")
	}
}
