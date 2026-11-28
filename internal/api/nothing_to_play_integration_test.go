//go:build integration

package api

// JS-183 review, class 3. A title that exists but has no file of its own (a series, a season) is not a title that does not exist:
// PlaybackInfo for it answers the reason that says there is nothing to play here, and its action, which is to choose another
// title. not-found stays for an id the server does not have.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
)

func TestAnExistingTitleWithNothingToPlayIsNotSaidToNotExist(t *testing.T) {
	kit := newEnv(t)
	series := uuid.NewString()
	mustExec(t, kit.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, (SELECT id FROM libraries ORDER BY name LIMIT 1), 'Series', 'Show', 'show', $2, 0, '{"Name":"Show","Type":"Series"}')`,
		series, "series-"+series)
	reasons := reasonsOf(t, servedContract(t, kit))

	response, raw := kit.do(t, 0, http.MethodGet, "/Items/"+db.CompactID(series), kit.token, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /Items for the series: %d %.120q, want 200: the viewer's app can show that it exists", response.StatusCode, raw)
	}

	response, raw = kit.do(t, 0, http.MethodPost, "/jellyscale/v1/items/"+series+"/playback-info", kit.token, map[string]any{})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo for the series: %d %.200q, want 200 with no sources and a reason", response.StatusCode, raw)
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("PlaybackInfo is not a JSON object: %v", err)
	}
	var sources []json.RawMessage
	if err := json.Unmarshal(reply["MediaSources"], &sources); err != nil || len(sources) != 0 {
		t.Fatalf("PlaybackInfo for the series lists sources: %.200q", raw)
	}
	if _, ok := reply["Reason"]; !ok {
		t.Fatalf("PlaybackInfo for the series has no Reason: %.200q", raw)
	}
	reason, action := stringField(t, reply, "Reason"), stringField(t, reply, "Action")
	if reason == "not-found" {
		t.Fatalf("PlaybackInfo for an existing series says not-found, which tells the viewer it does not exist: %.200q", raw)
	}
	if reason != "nothing-to-play" || action != "choose-another" {
		t.Fatalf("PlaybackInfo for the series says reason %q and action %q, want nothing-to-play and choose-another", reason, action)
	}
	if listed, ok := reasons[reason]; !ok || listed.Kind != "refusal" || listed.Action != action {
		t.Fatalf("the contract does not list %q as a refusal with action %q: %v", reason, action, listed)
	}
}

// An id the server does not have is still told not-found: nothing here is what the viewer named.
func TestAnUnknownTitleIsStillSaidToNotExist(t *testing.T) {
	kit := newEnv(t)
	response, raw := kit.do(t, 0, http.MethodPost, "/jellyscale/v1/items/"+uuid.NewString()+"/playback-info", kit.token, map[string]any{})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("PlaybackInfo for an unknown title: %d %.120q, want 404", response.StatusCode, raw)
	}
	fields := exactKeys(t, raw, []string{"Reason", "Action"}, "RetryAfterSeconds")
	if reason := stringField(t, fields, "Reason"); reason != "not-found" {
		t.Fatalf("PlaybackInfo for an unknown title says %q, want not-found", reason)
	}
}
