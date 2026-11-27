package contract_test

// JS-183, criterion 3. A client generated from the contract is not broken by a reason it does not know. The server can say
// only the reasons of its own contract, so the reason a copy lacks is said by a stand-in server here. The copy is the
// Refusal type oapi-codegen generates from the contract's envelope (types only, no runtime import).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"jellyscale/internal/contract/client"
)

const reasonAddedLater = "a-reason-added-after-this-copy"

func TestAGeneratedClientDecodesAReasonItDoesNotKnow(t *testing.T) {
	stand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"Reason":"` + reasonAddedLater + `","Action":"retry"}`))
	}))
	defer stand.Close()
	response, err := http.Get(stand.URL + "/jellyscale/v1/track-preferences")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var refusal client.Refusal
	if err := json.NewDecoder(response.Body).Decode(&refusal); err != nil {
		t.Fatalf("the generated client could not decode a refusal with a reason it does not know: %v", err)
	}
	if refusal.Reason != reasonAddedLater || refusal.Action != "retry" {
		t.Fatalf("decoded %+v, want the reason and action the server said", refusal)
	}
}
