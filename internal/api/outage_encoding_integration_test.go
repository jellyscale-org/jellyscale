//go:build integration

package api

import (
	"context"
	"testing"
	"time"
)

// jellyfin-web reads the encoding configuration before it asks for an ASS track, so a viewer in the snapshot
// sees the track in an outage only if a stream replica answers it too (JS-103). A token outside the snapshot
// still waits for Postgres, and is refused once it is back.
func TestTheEncodingConfigurationIsAnsweredFromTheSnapshotDuringAnOutage(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	p.playing(t, 0)
	_, stranger := p.addUser(t, "stranger")
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	path := func(token string) string { return "/System/Configuration/encoding?api_key=" + token }
	for _, rp := range p.streams {
		answeredWithin(t, "a viewer in the snapshot", rp, []served{{what: "encoding configuration", path: path(p.tokens[0]), status: 200, body: `{"EnableFallbackFont":false}` + "\n"}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var waiting []pending
	for _, rp := range p.streams {
		waiting = append(waiting, pending{what: "encoding configuration of a token outside the snapshot", ch: ask(ctx, rp, served{path: path(stranger)}), want: 200})
	}
	holdAndCheck(t, 3*time.Second, waiting, func() {
		for _, rp := range p.streams {
			rp.proxy.Back()
		}
	})
}
