//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/testenv"
)

// JS-033: what ends when a user is disabled, deleted or changes a password, and the forced races (criteria 9, 12,
// 13, 14 and 17).

// heldUsers is qcSite.held for the users table: it is held in EXCLUSIVE mode, in which plain reads (a token's check
// among them) go on and writes and row locks wait; the requests are sent; once every one has answered or waits on a
// lock in Postgres, and that has stopped changing, it lets go. A build that decides on a plain read and writes later
// has then made every decision from the same state. The settling wait only decides how many requests are held at once,
// never whether a right build passes: every state it waits for ends when this function lets go.
func (u *usersSite) heldUsers(replicas []int, requests ...func() signInReply) []signInReply {
	u.t.Helper()
	ctx := context.Background()
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		u.t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // The commit below has ended it on the way out.
	if _, err := tx.Exec(ctx, "LOCK TABLE users IN EXCLUSIVE MODE"); err != nil {
		u.t.Fatal(err)
	}
	replies := make([]signInReply, len(requests))
	var wg sync.WaitGroup
	var answered atomic.Int64
	for n, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[n] = request()
			answered.Add(1)
		}()
	}
	u.settle(replicas, len(requests), &answered)
	if err := tx.Commit(ctx); err != nil {
		u.t.Fatal(err)
	}
	wg.Wait()
	return replies
}

// settle waits until each of n requests has answered or its session waits on a lock, steadily.
func (u *usersSite) settle(replicas []int, n int, answered *atomic.Int64) {
	u.t.Helper()
	var apps []string
	for _, i := range replicas {
		apps = append(apps, u.names[i])
	}
	var waiting, settled int
	var done int64
	deadline := time.Now().Add(30 * time.Second)
	for settled < 20 {
		var held int
		if err := u.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = ANY($1) AND wait_event_type = 'Lock'`, apps).Scan(&held); err != nil {
			u.t.Fatal(err)
		}
		if now := answered.Load(); int(now)+held >= n && held == waiting && now == done {
			settled++
		} else {
			waiting, done, settled = held, now, 0
		}
		if time.Now().After(deadline) {
			u.t.Fatalf("%d of the %d requests came to wait in Postgres and %d answered; the others wait somewhere else", held, n, answered.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Criterion 13. Two administrators, two processes, each acting on the other, both held where they first write. Named
// mutant: counting the administrators before the update, outside its transaction (both count two, and both answer 204).
func TestOfTwoAdministratorsActingOnEachOtherExactlyOneSucceeds(t *testing.T) {
	type action func(u *usersSite, replica int, actor, target qcAccount) signInReply
	demote := func(u *usersSite, replica int, actor, target qcAccount) signInReply {
		return u.policy(replica, actor.token, target.id, map[string]any{"IsAdministrator": false})
	}
	disable := func(u *usersSite, replica int, actor, target qcAccount) signInReply {
		return u.policy(replica, actor.token, target.id, map[string]any{"IsDisabled": true})
	}
	remove := func(u *usersSite, replica int, actor, target qcAccount) signInReply {
		return u.as(replica, actor.token, "DELETE", "/Users/"+target.id, "")
	}
	for _, pair := range []struct {
		name          string
		first, second action
	}{
		{"demote and demote", demote, demote},
		{"disable and disable", disable, disable},
		{"delete and delete", remove, remove},
		{"demote and delete", demote, remove},
	} {
		t.Run(pair.name, func(t *testing.T) {
			u := newUsersSite(t, 2, nil)
			a, b := u.member(true), u.member(true)
			u.onlyThese(a, b)
			replies := u.heldUsers([]int{0, 1},
				func() signInReply { return pair.first(u, 0, a, b) },
				func() signInReply { return pair.second(u, 1, b, a) })
			got := []int{replies[0].status, replies[1].status}
			sort.Ints(got)
			if !slices.Equal(got, []int{204, 403}) {
				t.Fatalf("the two answers are %v (%.100q, %.100q), want one 204 and one 403", got, replies[0].body, replies[1].body)
			}
			winner := a
			if replies[1].status == 204 {
				winner = b
			}
			enabled := 0
			for _, user := range u.listed(0, winner.token, "") {
				if user.Policy["IsAdministrator"] == true && user.Policy["IsDisabled"] == false {
					enabled++
				}
			}
			if enabled != 1 {
				t.Errorf("%d enabled administrators remain, want 1", enabled)
			}
		})
	}
}

func TestTheSoleAdministratorCannotDemoteDisableOrDeleteThemselves(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	u.onlyThese(admin, viewer)
	u.check(403, u.policy(0, admin.token, admin.id, map[string]any{"IsAdministrator": false}), "demoting themselves")
	u.check(403, u.policy(0, admin.token, admin.id, map[string]any{"IsDisabled": true}), "disabling themselves")
	u.check(403, u.as(0, admin.token, "DELETE", "/Users/"+admin.id, ""), "deleting themselves")
	u.wantPolicy(admin, admin.id, "afterwards", map[string]any{"IsAdministrator": true, "IsDisabled": false})
	// An administrator who is disabled does not count as one.
	second := u.member(true)
	u.setPolicy(second, admin.id, map[string]any{"IsDisabled": true})
	u.check(403, u.policy(0, second.token, second.id, map[string]any{"IsAdministrator": false}), "demoting the only enabled administrator")
}

// Criterion 9: the log and the answer of a disabled account.
func TestADisabledAccountsRightPasswordAnswersAsAWrongOneAndTheLogSaysDisabled(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, disabled, enabled := u.member(true), u.member(false), u.member(false)
	u.setPolicy(admin, disabled.id, map[string]any{"IsDisabled": true})
	right := u.try(1, newDevice(), disabled.name, rightPassword)
	wrong := u.wrong(1, enabled.name)
	if right.status != 401 || wrong.status != 401 || string(right.body) != string(wrong.body) {
		t.Errorf("a disabled account's right password: %d %q; a wrong password: %d %q; want the same 401", right.status, right.body, wrong.status, wrong.body)
	}
	for _, header := range []string{"Content-Type", "Retry-After", "Www-Authenticate"} {
		if right.header.Get(header) != wrong.header.Get(header) {
			t.Errorf("%s is %q for the disabled account and %q for a wrong password", header, right.header.Get(header), wrong.header.Get(header))
		}
	}
	if len(u.logLines("", disabled.name, "disabled")) == 0 {
		t.Errorf("no log line names %s and says it is disabled", disabled.name)
	}
}

// A deleted user's name is a name that is no account: it is refused after the stranger's 20 failures, also from the
// device that held the account's token, which as a known device would be refused after 10.
func TestADeletedUsersNameCountsTowardTheBoundsAsAnUnknownName(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin := u.member(true)
	name, password := "gone-"+uuid.NewString()[:8], strong()
	made := u.created(admin, name, password)
	device := newDevice()
	signedIn(t, "before the delete", u.try(0, device, name, password))
	u.must(204, 0, admin.token, "DELETE", "/Users/"+made.Id, "", "deleting")
	unknown := u.wrong(0, invented())
	for i := range 20 {
		got := u.try(i, device, name, password)
		if got.status != 401 || string(got.body) != string(unknown.body) {
			t.Fatalf("attempt %d with the deleted user's old password: %d %q, want the unknown name's %d %q", i+1, got.status, got.body, unknown.status, unknown.body)
		}
	}
	refusedWithin(t, "attempt 21 for the deleted name", u.try(0, device, name, password), 1, 900)
}

// Criterion 12.
func TestQuickConnectGivesNothingToADisabledOrDeletedAccount(t *testing.T) {
	for _, end := range []struct {
		name string
		do   func(u *usersSite, admin, who qcAccount)
	}{
		{"disabled", func(u *usersSite, admin, who qcAccount) {
			u.setPolicy(admin, who.id, map[string]any{"IsDisabled": true})
		}},
		{"deleted", func(u *usersSite, admin, who qcAccount) {
			u.must(204, 0, admin.token, "DELETE", "/Users/"+who.id, "", "deleting")
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			u := newUsersSite(t, 2, nil)
			admin, viewer := u.member(true), u.member(false)
			approved, pending := u.start(0, newTV()), u.start(0, newTV())
			u.approve(0, viewer, approved)
			end.do(u, admin, viewer)
			u.check(401, u.authorize(1, viewer.token, pending.Code, ""), "Authorize by the "+end.name+" account")
			unknown := u.redeem(1, newTV(), "0000deadbeef")
			got := u.redeem(1, approved.device, approved.Secret)
			if got.status != 404 || got.status != unknown.status || string(got.body) != string(unknown.body) {
				t.Errorf("redeeming the code it approved: %d %q, want the unknown secret's %d %q", got.status, got.body, unknown.status, unknown.body)
			}
			if n := u.deviceTokens(approved.device, viewer); n != 0 {
				t.Errorf("%d tokens were issued to the device", n)
			}
		})
	}
}

// userHoldings gives the user a row in every table a delete must empty.
func (u *usersSite) userHoldings(who qcAccount, film usersFilm) {
	u.t.Helper()
	ghost := "users-ghost-" + uuid.NewString()
	u.t.Cleanup(func() {
		if _, err := u.pool.Exec(context.Background(), "DELETE FROM api_replicas WHERE id = $1", ghost); err != nil {
			u.t.Error(err)
		}
	})
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO user_item_data (user_id, item_id, played, favorite) VALUES ($1, $2, true, true)`, []any{who.id, film.item}},
		{`INSERT INTO display_preferences (user_id, preference_id, client, data) VALUES ($1, 'home', 'c', '{}')`, []any{who.id}},
		{`INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version) VALUES ($1, $2, 'c', 'seeded', 'dev', '1')`, []any{who.token, who.id}},
		// The socket is on a replica that beats an hour ahead, so that no sweep takes it away during the test.
		{`INSERT INTO api_replicas (id, beat_at) VALUES ($1, now() + interval '1 hour')`, []any{ghost}},
		{`INSERT INTO session_sockets (connection_id, token, replica) VALUES ($1, $2, $3)`, []any{uuid.NewString(), who.token, ghost}},
	} {
		if _, err := u.pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
			u.t.Fatal(err)
		}
	}
	u.approve(0, who, u.start(0, newTV()))
	for table, rows := range u.rowsOf(who.id) {
		if rows == "" {
			u.t.Fatalf("the user holds no row in %s: the check of the delete would prove nothing", table)
		}
	}
}

// Criterion 14.
func TestDeleteRemovesEverythingTheUserHoldsAndNoRowOfAnotherUser(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, leaver, stayer := u.member(true), u.member(false), u.member(false)
	film := u.film(4096)
	u.userHoldings(leaver, film)
	u.userHoldings(stayer, film)
	stayed, kept := u.rowsOf(stayer.id), u.rowsOf(admin.id)
	u.must(204, 0, admin.token, "DELETE", "/Users/"+leaver.id, "", "deleting")
	for table, rows := range u.rowsOf(leaver.id) {
		if rows != "" {
			t.Errorf("the deleted user still has rows in %s: %.200s", table, rows)
		}
	}
	for table, rows := range u.rowsOf(stayer.id) {
		if rows != stayed[table] {
			t.Errorf("another user's %s changed:\n%s\nwas\n%s", table, rows, stayed[table])
		}
	}
	for table, rows := range u.rowsOf(admin.id) {
		if table != "users" && rows != kept[table] {
			t.Errorf("the administrator's %s changed", table)
		}
	}
}

// Criterion 14, forced: the delete is held where it writes one of the two places no foreign key joins (the user's row,
// and the Quick Connect codes), and meanwhile everything the user holds is still there. The wrong build: two
// transactions, in either order.
func TestDeleteIsOneTransaction(t *testing.T) {
	for _, held := range []string{"users", "quickconnect_codes"} {
		t.Run("held at "+held, func(t *testing.T) {
			u := newUsersSite(t, 1, nil)
			admin, leaver := u.member(true), u.member(false)
			film := u.film(4096)
			u.userHoldings(leaver, film)
			before := u.rowsOf(leaver.id)
			ctx := context.Background()
			tx, err := u.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			if _, err := tx.Exec(ctx, "LOCK TABLE "+held+" IN EXCLUSIVE MODE"); err != nil {
				t.Fatal(err)
			}
			var answered atomic.Int64
			reply := make(chan signInReply, 1)
			go func() {
				reply <- u.as(0, admin.token, "DELETE", "/Users/"+leaver.id, "")
				answered.Add(1)
			}()
			u.settle([]int{0}, 1, &answered)
			if answered.Load() != 0 {
				got := <-reply
				t.Fatalf("the delete answered %d while %s was held: it never wrote there", got.status, held)
			}
			for table, rows := range u.rowsOf(leaver.id) {
				if rows != before[table] {
					t.Errorf("while the delete waits on %s, %s has already changed", held, table)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if got := <-reply; got.status != 204 {
				t.Errorf("the delete: %d %.100q, want 204", got.status, got.body)
			}
		})
	}
}

// slowPlay is a direct play of the whole file read slowly, on a connection of its own with a small receive buffer, so
// that what the server has not sent stays with the server.
type slowPlay struct {
	conn net.Conn
	body io.ReadCloser
	read int64
}

func startSlowPlay(t *testing.T, stream *proc, film usersFilm, token string) *slowPlay {
	t.Helper()
	conn, err := net.Dial("tcp", stream.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.(*net.TCPConn).SetReadBuffer(32 << 10) //nolint:errcheck
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: replica\r\nConnection: close\r\n\r\n", film.streamPath(token))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || response.StatusCode != 200 || response.ContentLength != film.size {
		t.Fatalf("the direct play did not start: %v %v", response, err)
	}
	play := &slowPlay{conn: conn, body: response.Body}
	play.slowly(t, 500*time.Millisecond)
	return play
}

// slowly reads 16 KiB each 100 ms for d, or until the stream ends.
func (p *slowPlay) slowly(t *testing.T, d time.Duration) {
	buffer := make([]byte, 16<<10)
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		p.conn.SetReadDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
		n, err := io.ReadFull(p.body, buffer)
		p.read += int64(n)
		if err != nil {
			return
		}
	}
}

// rest reads what is left as fast as it comes, and says how much the play got in all.
func (p *slowPlay) rest() int64 {
	p.conn.SetReadDeadline(time.Now().Add(2 * time.Minute)) //nolint:errcheck
	n, _ := io.Copy(io.Discard, p.body)
	p.read += n
	return p.read
}

// Criterion 17. The play is read slowly for the criterion's 15 seconds after the token ends and then as fast as it
// comes: a stream the server closed in time ends short, and one it did not close arrives whole. A bystander's play
// read the same way must arrive whole, so a build that cuts every stream fails. The 15 seconds are the criterion's
// own bound, run at that length.
func TestARunningDirectPlayStopsWhenItsTokenEnds(t *testing.T) {
	const size = 256 << 20
	for _, end := range []struct {
		name string
		do   func(t *testing.T, u *usersSite, admin, who qcAccount, other string)
	}{
		{"sign-out", func(t *testing.T, u *usersSite, admin, who qcAccount, _ string) { u.logout(0, who.token) }},
		{"a password change on another device", func(t *testing.T, u *usersSite, admin, who qcAccount, other string) {
			u.must(204, 0, other, "POST", "/Users/Password?userId="+who.id, body(map[string]any{"CurrentPw": rightPassword, "NewPw": strong()}), "changing the password")
		}},
		{"a delete", func(t *testing.T, u *usersSite, admin, who qcAccount, _ string) {
			u.must(204, 0, admin.token, "DELETE", "/Users/"+who.id, "", "deleting")
		}},
		{"a disable", func(t *testing.T, u *usersSite, admin, who qcAccount, _ string) {
			u.setPolicy(admin, who.id, map[string]any{"IsDisabled": true})
		}},
	} {
		t.Run(strings.ReplaceAll(end.name, " ", "_"), func(t *testing.T) {
			u := newUsersSite(t, 1, nil)
			admin, who, bystander := u.member(true), u.member(false), u.member(false)
			other := signedIn(t, "the user's other device", u.try(0, newDevice(), who.name, rightPassword))
			film := u.film(size)
			stream := start(t, "stream", u.dsn, map[string]any{"adminUser": ""})
			ending := startSlowPlay(t, stream, film, who.token)
			staying := startSlowPlay(t, stream, film, bystander.token)
			var kept *slowPlay
			if end.name == "a password change on another device" {
				kept = startSlowPlay(t, stream, film, other) // The device that changes its own password.
			}
			end.do(t, u, admin, who, other)
			var wg sync.WaitGroup
			for _, play := range []*slowPlay{ending, staying, kept} {
				if play == nil {
					continue
				}
				wg.Add(1)
				go func() { defer wg.Done(); play.slowly(t, 15*time.Second) }()
			}
			wg.Wait()
			if got := ending.rest(); got >= size {
				t.Errorf("15 s after %s the play went on and sent the whole %d bytes", end.name, got)
			}
			if got := staying.rest(); got != size {
				t.Errorf("another user's play got %d of %d bytes", got, size)
			}
			if kept != nil {
				if got := kept.rest(); got != size {
					t.Errorf("the play of the device that changed its password got %d of %d bytes", got, size)
				}
			}
		})
	}
}

// holdAtCommit makes the transaction in which a session of the application name changes a user or ends a device's token
// wait at its commit: every write of it made, none to be seen. It is a deferred trigger waiting on an advisory lock the
// test holds; release gives the lock up. The trigger goes with the test.
func (u *usersSite) holdAtCommit(application string) (release func()) {
	u.t.Helper()
	ctx := context.Background()
	key := rand.Int64()
	exec := func(sql string) {
		u.t.Helper()
		if _, err := u.pool.Exec(ctx, sql); err != nil {
			u.t.Fatal(err)
		}
	}
	u.t.Cleanup(func() {
		for _, sql := range []string{"DROP TRIGGER IF EXISTS users_test_hold ON users", "DROP TRIGGER IF EXISTS users_test_hold ON devices", "DROP FUNCTION IF EXISTS users_test_hold()"} {
			if _, err := u.pool.Exec(ctx, sql); err != nil {
				u.t.Error(err)
			}
		}
	})
	exec(fmt.Sprintf("CREATE FUNCTION users_test_hold() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NULL; END'", key))
	holder, err := u.pool.Begin(ctx)
	if err != nil {
		u.t.Fatal(err)
	}
	// Registered after the drop, so that it runs before it: a failing test lets go first.
	u.t.Cleanup(func() { holder.Rollback(context.Background()) }) //nolint:errcheck
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		u.t.Fatal(err)
	}
	for table, events := range map[string]string{"users": "UPDATE", "devices": "UPDATE OR DELETE"} {
		exec(fmt.Sprintf(`CREATE CONSTRAINT TRIGGER users_test_hold AFTER %s ON %s DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW WHEN (current_setting('application_name') = '%s') EXECUTE FUNCTION users_test_hold()`, events, table, application))
	}
	return func() {
		if err := holder.Rollback(ctx); err != nil {
			u.t.Fatal(err)
		}
	}
}

// whileHeldAtCommit starts the change, which a session of the application name makes, holds it at its commit, sends
// the racers to the replica, waits until each has answered or waits on a lock, and lets the change commit.
func (u *usersSite) whileHeldAtCommit(application string, change func() signInReply, replica int, racers ...func() signInReply) (changed signInReply, raced []signInReply) {
	u.t.Helper()
	release := u.holdAtCommit(application)
	done := make(chan signInReply, 1)
	go func() { done <- change() }()
	deadline := time.Now().Add(30 * time.Second)
	for held := 0; held == 0; time.Sleep(20 * time.Millisecond) {
		select {
		case got := <-done:
			u.t.Fatalf("the disable answered %d %.100q without its transaction reaching a commit that changes a user or a token", got.status, got.body)
		default:
		}
		if err := u.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'`, application).Scan(&held); err != nil {
			u.t.Fatal(err)
		}
		if time.Now().After(deadline) {
			u.t.Fatal("the disable did not come to its commit")
		}
	}
	raced = make([]signInReply, len(racers))
	var wg sync.WaitGroup
	var answered atomic.Int64
	for n, racer := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raced[n] = racer()
			answered.Add(1)
		}()
	}
	u.settle([]int{replica}, len(racers), &answered)
	release()
	wg.Wait()
	return <-done, raced
}

// No token outlives a disable. The disable is held at its commit, every write made and none to be seen, while through
// another process the user signs in, a code they approved is redeemed and their password is changed by a device that
// keeps its token. Afterwards no token any of them holds is accepted, also once the account is enabled again. The wrong
// build: reading "enabled" without the user's row locked, and writing the token afterwards.
func TestNoTokenOutlivesADisableThatRacesASignInARedemptionAndAPasswordChange(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, who := u.member(true), u.member(false)
	code := u.start(1, newTV())
	u.approve(1, who, code)
	disabled, raced := u.whileHeldAtCommit(u.names[0],
		func() signInReply { return u.policy(0, admin.token, who.id, map[string]any{"IsDisabled": true}) }, 1,
		func() signInReply { return u.quiet(1, newDevice(), who.name, rightPassword) },
		func() signInReply { return u.redeem(1, code.device, code.Secret) },
		func() signInReply {
			return u.as(1, who.token, "POST", "/Users/Password?userId="+who.id, body(map[string]any{"CurrentPw": rightPassword, "NewPw": strong()}))
		})
	if disabled.status != 204 {
		t.Fatalf("the disable: %d %.200q, want 204", disabled.status, disabled.body)
	}
	tokens := map[string]string{"the device that changed its password": who.token}
	for n, name := range []string{"the racing sign-in", "the racing redemption"} {
		var reply struct{ AccessToken string }
		if raced[n].status >= 500 || raced[n].status == 0 {
			t.Errorf("%s: %d %.200q", name, raced[n].status, raced[n].body)
		}
		if raced[n].status == 200 && json.Unmarshal(raced[n].body, &reply) == nil {
			tokens[name] = reply.AccessToken
		}
	}
	if got := raced[2].status; got >= 500 || got == 0 {
		t.Errorf("the racing password change: %d %.200q", got, raced[2].body)
	}
	u.wantPolicy(admin, who.id, "after the race", map[string]any{"IsDisabled": true})
	for name, token := range tokens {
		u.check(401, u.as(1, token, "GET", "/Users/Me", ""), "the token of "+name+" after the disable")
	}
	u.setPolicy(admin, who.id, map[string]any{"IsDisabled": false})
	for name, token := range tokens {
		u.check(401, u.as(1, token, "GET", "/Users/Me", ""), "the token of "+name+" once the account is enabled again")
	}
}

// Stopping a running play costs Postgres a poll, not a question for each piece sent. Two stream processes, each on a
// proxy that keeps every statement, one of them serving a play read at 160 KB/s for 15 seconds (some 75 pieces of
// 32 KB): what the playing one sends beyond what the idle one sends in the same time is at most a handful. Then the
// token is signed out and the play must stop as in TestARunningDirectPlayStopsWhenItsTokenEnds, so a build with no
// check at all does not pass.
func TestStoppingARunningPlayCostsPostgresNoMoreThanAPoll(t *testing.T) {
	const size, handful = 256 << 20, 10
	u := newUsersSite(t, 1, nil)
	viewer := u.member(false)
	film := u.film(size)
	processes := map[string]*proc{}
	proxies := map[string]*testenv.DBProxy{}
	for _, name := range []string{"idle", "playing"} {
		proxies[name] = testenv.NewDBProxy(t, u.dsn)
		// Every statement's text is sent each time, so that the proxy sees each one.
		processes[name] = start(t, "stream", proxies[name].DSN()+"&default_query_exec_mode=exec", map[string]any{"adminUser": ""})
	}
	play := startSlowPlay(t, processes["playing"], film, viewer.token)
	statements := func(name string) (count int) {
		for _, sql := range proxies[name].Statements() {
			switch strings.ToLower(strings.TrimSpace(sql)) {
			case "begin", "commit", "rollback":
			default:
				count++
			}
		}
		return count
	}
	idleBefore, playingBefore := statements("idle"), statements("playing")
	play.slowly(t, 15*time.Second)
	idle, playing := statements("idle")-idleBefore, statements("playing")-playingBefore
	t.Logf("in 15 s the idle stream process sent %d statements and the playing one %d", idle, playing)
	if idle == 0 {
		t.Fatal("the proxy saw no statement of the idle process, which refreshes its snapshot every 5 s: it is not counting")
	}
	if playing-idle > handful {
		t.Errorf("15 s of a slow play added %d statements, want at most %d", playing-idle, handful)
	}
	u.logout(0, viewer.token)
	play.slowly(t, 15*time.Second)
	if got := play.rest(); got >= size {
		t.Errorf("15 s after sign-out the play went on and sent the whole %d bytes", got)
	}
}
