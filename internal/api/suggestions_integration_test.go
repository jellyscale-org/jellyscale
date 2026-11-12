//go:build integration

package api

import (
	"encoding/json"
	"testing"

	"jellyscale/internal/db"
)

// Findroid's home screen asks /Items/Suggestions for films and shows, with the
// SDK's parameter names (type, mediaType), and stops with an error when it is
// answered 404.
func TestSuggestionsAnswerFindroidsHomeRow(t *testing.T) {
	e := newEnv(t)
	ids := func(path, token string) (int, []string, int) {
		t.Helper()
		response, body := e.do(t, 0, "GET", path, token, nil)
		if response.StatusCode != 200 {
			return response.StatusCode, nil, 0
		}
		var result struct {
			Items []struct {
				ID string `json:"Id"`
			}
			TotalRecordCount int
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, body)
		}
		var got []string
		for _, item := range result.Items {
			got = append(got, item.ID)
		}
		return 200, got, result.TotalRecordCount
	}
	film := db.CompactID(e.item)

	// What Findroid sends.
	path := "/Items/Suggestions?userId=" + db.CompactID(e.user) + "&mediaType=Video&type=Movie&type=Series&limit=6"
	if status, got, total := ids(path, e.token); status != 200 || len(got) != 1 || got[0] != film || total != 1 {
		t.Errorf("Findroid's request: %d %v (total %d), want the film", status, got, total)
	}
	if status, got, _ := ids("/Items/Suggestions?type=Series", e.token); status != 200 || len(got) != 0 {
		t.Errorf("shows only: %d %v, want none", status, got)
	}
	if status, got, _ := ids("/Items/Suggestions?mediaType=Audio", e.token); status != 200 || len(got) != 0 {
		t.Errorf("audio only: %d %v, want none", status, got)
	}
	if status, _, _ := ids("/Items/Suggestions", ""); status != 401 {
		t.Errorf("no token: %d, want 401", status)
	}
}
