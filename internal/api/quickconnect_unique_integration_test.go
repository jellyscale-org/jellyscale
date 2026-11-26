//go:build integration

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

// JS-037, criterion 6: no code is live for two devices at once. Random six-digit codes seldom collide, so the collision
// is forced: crypto/rand is seeded the same way before two Initiates, which then draw the same random bytes, a live code
// first and fresh ones after it.
//
// What it asks of a build: codes and secrets come from crypto/rand, and when anything drawn is already taken, all of
// it (code and secret) is drawn again, as the second Initiate here draws the first one's secret too. The tables are
// named quickconnect_* or quick_connect_* (see cmd/jellyscale/quickconnect_kit_integration_test.go).

const qcSeed = 37

type qcUniqueSite struct {
	t           *testing.T
	pool        *pgxpool.Pool // The server's, under an application_name of its own.
	url         string        // The server's address.
	dsn         string
	application string
}

type qcGiven struct{ Code, Secret, DeviceId string }

func newQCUniqueSite(t *testing.T) *qcUniqueSite {
	t.Helper()
	ctx := context.Background()
	site := &qcUniqueSite{t: t, dsn: testenv.DatabaseURL(t), application: "qc-unique"}
	address, err := url.Parse(site.dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := address.Query()
	query.Set("application_name", site.application)
	address.RawQuery = query.Encode()
	site.pool, err = db.Open(ctx, address.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(site.pool.Close)
	site.forget()
	t.Cleanup(site.forget)
	served := &Server{Pool: site.pool, Items: &items.Repo{Pool: site.pool, ServerID: "srv"}, Roles: []string{"api"}, Log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(served.Handler())
	t.Cleanup(server.Close)
	site.url = server.URL
	return site
}

func (s *qcUniqueSite) tables() []string {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name ~ '^quick_?connect'`)
	if err != nil {
		s.t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		s.t.Fatal(err)
	}
	return tables
}

func (s *qcUniqueSite) forget() {
	s.t.Helper()
	for _, table := range s.tables() {
		mustExec(s.t, s.pool, `TRUNCATE "`+table+`" CASCADE`)
	}
}

func (s *qcUniqueSite) send(device string) accReply {
	return accSend("POST", s.url, "/QuickConnect/Initiate", "", -1, "",
		"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="`+device+`", Version="1"`)
}

func (s *qcUniqueSite) given(device string, reply accReply) qcGiven {
	s.t.Helper()
	var got qcGiven
	if reply.err != nil || reply.status != 200 || json.Unmarshal(reply.body, &got) != nil || got.Code == "" || got.Secret == "" {
		s.t.Fatalf("Initiate for %s: %d %q %v, want 200 with a Code and a Secret", device, reply.status, reply.body, reply.err)
	}
	return got
}

func (s *qcUniqueSite) initiate(device string) qcGiven {
	s.t.Helper()
	return s.given(device, s.send(device))
}

// keeps holds the device to its own code: its secret reads that code and that device.
func (s *qcUniqueSite) keeps(device string, given qcGiven) {
	s.t.Helper()
	reply := accSend("GET", s.url, "/QuickConnect/Connect?Secret="+url.QueryEscape(given.Secret), "", -1, "")
	var got qcGiven
	if reply.err != nil || reply.status != 200 || json.Unmarshal(reply.body, &got) != nil || got.Code != given.Code || got.DeviceId != device {
		s.t.Errorf("Connect with %s's secret: %d %q %v, want its code %s", device, reply.status, reply.body, reply.err, given.Code)
	}
}

// The build this fails on gives the code it drew without looking whether it is live.
func TestNoTwoDevicesAreGivenOneLiveCode(t *testing.T) {
	site := newQCUniqueSite(t)
	seeded := func(device string) qcGiven {
		t.Helper()
		cryptotest.SetGlobalRandom(t, qcSeed)
		return site.initiate(device)
	}
	// The pool's connection is open before the first seeded draw, so that signing in to Postgres takes none of it.
	site.initiate("warm-up")
	site.forget()

	first := seeded("first")
	second := seeded("second")
	if second.Code == first.Code || second.Secret == first.Secret {
		t.Errorf("a second device was given a live code or secret: code %s, the first device's is %s", second.Code, first.Code)
	}
	site.keeps("first", first)
	site.keeps("second", second)
	// The control: with nothing live, the same seed gives the same code, so the second Initiate did draw a live one.
	site.forget()
	if again := seeded("again"); again.Code != first.Code {
		t.Fatalf("with no code live the same seed gave %s, then %s: the seed does not decide the code, so nothing was forced", first.Code, again.Code)
	}
}

// Two at once draw the same code, each before the other's is stored: every insert in the quickconnect_* tables is held
// by a trigger, the first Initiate is brought there, the seed is set again and the second is sent after it. The build
// this fails on looks whether the code is live and then stores it, with nothing (a unique index, or a lock taken before
// the look) to stop another doing the same in between. Deleting expired codes first is not held, so a build that does
// that draws its code before it is stopped, as it would in service.
func TestTwoInitiatesAtOnceThatDrawTheSameCodeGiveItToOneDevice(t *testing.T) {
	site := newQCUniqueSite(t)
	ctx := context.Background()
	// Connections enough for both requests are open before the seeded draws, as above.
	var warm []*pgxpool.Conn
	for range 4 {
		conn, err := site.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		warm = append(warm, conn)
	}
	for _, conn := range warm {
		conn.Release()
	}
	site.initiate("warm-up")
	site.forget()

	const holdKey = 370037
	t.Cleanup(func() { mustExec(t, site.pool, `DROP FUNCTION IF EXISTS qc_test_hold() CASCADE`) })
	mustExec(t, site.pool, `CREATE FUNCTION qc_test_hold() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN PERFORM pg_advisory_xact_lock_shared(370037); RETURN NEW; END $$`)
	for _, table := range site.tables() {
		mustExec(t, site.pool, `CREATE TRIGGER qc_test_hold BEFORE INSERT ON "`+table+`" FOR EACH ROW EXECUTE FUNCTION qc_test_hold()`)
	}
	holder, err := pgx.Connect(ctx, site.dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Registered last, so a failure lets the held requests go before the server closes.
	t.Cleanup(func() { holder.Close(ctx) }) //nolint:errcheck // Closing releases the lock either way.
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, holdKey); err != nil {
		t.Fatal(err)
	}

	replies := map[string]chan accReply{}
	// heldAfter seeds crypto/rand, sends the device's Initiate and returns once `waiting` of the server's backends wait.
	heldAfter := func(device string, waiting int) {
		t.Helper()
		cryptotest.SetGlobalRandom(t, qcSeed)
		reply := make(chan accReply, 1)
		replies[device] = reply
		go func() { reply <- site.send(device) }()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var held int
			if err := holder.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND application_name = $1 AND wait_event_type = 'Lock'`, site.application).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if held >= waiting {
				return
			}
			select {
			case got := <-reply:
				t.Fatalf("Initiate for %s answered %d %q %v before it stored a code, so nothing was forced", device, got.status, got.body, got.err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of the server's backends wait in Postgres, want %d", held, waiting)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	heldAfter("first", 1)
	heldAfter("second", 2)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, holdKey); err != nil {
		t.Fatal(err)
	}
	first, second := site.given("first", <-replies["first"]), site.given("second", <-replies["second"])
	if first.Code == second.Code || first.Secret == second.Secret {
		t.Fatalf("two devices asking at once were both given the code %s", first.Code)
	}
	site.keeps("first", first)
	site.keeps("second", second)
}
