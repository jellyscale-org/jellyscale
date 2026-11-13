package main

// How the runner holds every JSON answer of jellyscale to the schema (JS-070).
// Names the task implies:
//
//	(*server).conform *conform.Checker   set by the runner for jellyscale; every do() of that server is checked
//	(*server).violations() []conform.Violation   what the checker found so far
//
// The runner keeps violations apart from the differences: compat/differences.yaml
// never filters one (test/differential/acceptance.sh shows it).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"jellyscale/compat/conform"
)

func schemaChecker(t *testing.T) *conform.Checker {
	t.Helper()
	file, err := os.ReadFile("../../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	return conform.NewChecker(s, conform.Rules{})
}

func TestEveryAnswerOfTheCheckedServerIsHeldToTheSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ServerName":5}`)) // A number where PublicSystemInfo has a string (its fields are all nullable, so leaving them out is valid).
	}))
	defer srv.Close()
	s := &server{name: "jellyscale", base: srv.URL, ids: map[string]string{}, byKey: map[string]string{}, conform: schemaChecker(t)}
	if _, _, err := s.do("GET", "/System/Info/Public", ""); err != nil {
		t.Fatal(err)
	}
	vs := s.violations()
	if len(vs) == 0 {
		t.Fatal("an answer that breaks the schema was not reported")
	}
	if n, routes := s.conform.Counts(); n != 1 || routes != 1 {
		t.Errorf("counted %d on %d routes", n, routes)
	}
}
