package testenv

import (
	"errors"
	"net"
	"testing"
	"time"
)

// A hold keeps the connections open and passes nothing in either direction,
// on the connections that stand and the ones made after it, until the release,
// and then passes what was kept. The target is a plain TCP server, so this runs
// with no Postgres. Written before Hold existed: it did not compile (JS-105).
//
// The 300 ms waits only bound how long "nothing passes" is watched: a build that
// passes data during a hold fails on any machine, and one that does not never
// fails on a slow one.
func TestAHoldPassesNothingEitherWayUntilItIsReleased(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	speak := make(chan struct{}) // Closed when the server is to say something unasked.
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				go func() { // Echo.
					buf := make([]byte, 64)
					for {
						n, err := c.Read(buf)
						if err != nil {
							return
						}
						c.Write(buf[:n]) //nolint:errcheck
					}
				}()
				<-speak
				c.Write([]byte("S")) //nolint:errcheck
				<-done
				c.Close()
			}()
		}
	}()

	p := NewDBProxy(t, "postgres://u@"+ln.Addr().String()+"/d")
	dial := func() net.Conn {
		t.Helper()
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	read := func(c net.Conn, wait time.Duration) (string, error) {
		buf := make([]byte, 16)
		c.SetReadDeadline(time.Now().Add(wait)) //nolint:errcheck
		n, err := c.Read(buf)
		return string(buf[:n]), err
	}
	silent := func(what string, c net.Conn) {
		t.Helper()
		got, err := read(c, 300*time.Millisecond)
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("%s: read %q, %v during a hold; want nothing and the connection open", what, got, err)
		}
	}

	standing := dial()
	standing.Write([]byte("a")) //nolint:errcheck
	if got, err := read(standing, 5*time.Second); got != "a" || err != nil {
		t.Fatalf("before the hold the echo was %q, %v", got, err)
	}

	p.Hold()
	later := dial()             // Made during the hold: accepted, and held.
	standing.Write([]byte("b")) //nolint:errcheck
	later.Write([]byte("c"))    //nolint:errcheck
	silent("a connection that stood, sent to", standing)
	silent("a connection made during the hold, sent to", later)
	close(speak) // The server speaks on both connections: nothing comes back either.
	silent("a connection that stood, spoken to by the server", standing)
	silent("a connection made during the hold, spoken to by the server", later)

	p.Release()
	for what, want := range map[string]string{"b": "b", "c": "c"} {
		c := standing
		if what == "c" {
			c = later
		}
		// The echo and the server's unasked byte both arrive, in either order; the kept bytes are among them.
		var got string
		for len(got) < 2 {
			more, err := read(c, 5*time.Second)
			if err != nil {
				t.Fatalf("after the release, connection of %q read %q then %v; what was kept must arrive", what, got, err)
			}
			got += more
		}
		if got != want+"S" && got != "S"+want {
			t.Errorf("after the release, connection of %q read %q, want %q and the server's S", what, got, want)
		}
	}
	// A second release, and a hold again, change nothing that ended.
	p.Release()
	p.Hold()
	p.Release()
	standing.Write([]byte("z")) //nolint:errcheck
	if got, err := read(standing, 5*time.Second); got != "z" || err != nil {
		t.Errorf("after a second hold and release the echo was %q, %v", got, err)
	}
}
