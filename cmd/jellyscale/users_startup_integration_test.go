//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// JS-033, D-095: a start that would create the configured administrator with a commonly guessed password stops. An
// account that exists is never checked, so that no upgrade stops.
func TestStartUpRefusesToCreateTheAdministratorWithACommonlyGuessedPassword(t *testing.T) {
	dsn := testDatabaseURL(t)
	pool := connect(t, dsn)
	const guessedPassword = "iloveyou" // On any list, and no part of any other word of the message.
	count := func(name string) (n int) {
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE name = $1", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	launch := func(name, password string) *proc {
		return startProc(t, procSpec{bin: binary(t), dsn: dsn, args: []string{"--roles=api"},
			config: `{"name":"startup","adminUser":"` + name + `"}`, listen: true, user: name, password: password})
	}

	name := "weak-admin-" + uuid.NewString()[:8]
	p := launch(name, guessedPassword)
	for deadline := time.Now().Add(time.Minute); p.alive() && !p.status2xx("/healthz"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("neither stopped nor up after a minute:\n%s", p.log)
		}
	}
	if p.alive() {
		t.Errorf("the start did not stop: it is serving, with %d accounts named %s", count(name), name)
	} else {
		log := p.log.String()
		if p.status == nil || !strings.Contains(log, "JELLYSCALE_ADMIN_PASSWORD") {
			t.Errorf("the stopped start exited %v and its log does not name JELLYSCALE_ADMIN_PASSWORD:\n%s", p.status, log)
		}
		if strings.Contains(log, guessedPassword) {
			t.Errorf("the log holds the password:\n%s", log)
		}
		if n := count(name); n != 0 {
			t.Errorf("%d accounts were created", n)
		}
	}
	p.kill()

	existing := "old-admin-" + uuid.NewString()[:8]
	first := launch(existing, "Qx7!"+uuid.NewString())
	first.until(time.Minute, "a start with a strong password", func() bool { return first.status2xx("/healthz") })
	first.kill()
	again := launch(existing, guessedPassword)
	again.until(time.Minute, "a start over an existing account with a guessed password in the environment", func() bool { return again.status2xx("/healthz") })
}
