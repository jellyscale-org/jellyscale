package main

// compare is where the runner decides one request: it asks both servers and
// returns the differences, or an error. A request fails when compare returns an
// error or any difference (the runner adds the allowlist after). These use
// stand-in servers that answer fixed documents.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"jellyscale/test/differential"
)

func stub(t *testing.T, status int, body string) *server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &server{name: "stub", base: srv.URL, ids: map[string]string{}, byKey: map[string]string{}}
}

func failed(diffs []differential.Difference, err error) bool { return err != nil || len(diffs) > 0 }

func TestBothServersAnsweringAnErrorFailsTheRequest(t *testing.T) {
	r := differential.Request{Name: "r", Method: "GET", Path: "/Items"}
	if !failed(compare(stub(t, 404, `{}`), stub(t, 404, `{}`), r)) {
		t.Fatal("a 404 from both passed")
	}
	if !failed(compare(stub(t, 500, `{}`), stub(t, 500, `{}`), r)) {
		t.Fatal("a 500 from both passed")
	}
}

func TestBothServersAnsweringAnEmptyListFailsARowsRequest(t *testing.T) {
	r := differential.Request{Name: "r", Method: "GET", Path: "/Items", Rows: true}
	if !failed(compare(stub(t, 200, `{"Items":[]}`), stub(t, 200, `{"Items":[]}`), r)) {
		t.Fatal("an empty list from both passed")
	}
	if !failed(compare(stub(t, 200, `{"Items":[{"Id":"a"}]}`), stub(t, 200, `{"Items":[]}`), r)) {
		t.Fatal("an empty list from one passed")
	}
}

func TestRowsFromBothPassAndAnEmptyListPassesWithoutRows(t *testing.T) {
	rows := differential.Request{Name: "r", Method: "GET", Path: "/Items", Rows: true}
	if diffs, err := compare(stub(t, 200, `{"Items":[{"Name":"a"}]}`), stub(t, 200, `{"Items":[{"Name":"a"}]}`), rows); failed(diffs, err) {
		t.Fatalf("rows from both failed: %v %v", diffs, err)
	}
	plain := differential.Request{Name: "r", Method: "GET", Path: "/Items"}
	if diffs, err := compare(stub(t, 200, `{"Items":[]}`), stub(t, 200, `{"Items":[]}`), plain); failed(diffs, err) {
		t.Fatalf("an empty list failed a request that does not expect rows: %v %v", diffs, err)
	}
}

func TestARequestThatExpectsA404PassesOnTwo404s(t *testing.T) {
	r := differential.Request{Name: "r", Method: "GET", Path: "/Items/none", Status: 404}
	if diffs, err := compare(stub(t, 404, `{}`), stub(t, 404, `{}`), r); failed(diffs, err) {
		t.Fatalf("the expected 404 failed: %v %v", diffs, err)
	}
	if !failed(compare(stub(t, 200, `{}`), stub(t, 200, `{}`), r)) {
		t.Fatal("two 200s passed a request that expects 404")
	}
}
