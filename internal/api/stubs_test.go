package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// Jellyfin 12.4's document types GET /Items/{itemId}/Collections as a BaseItemDtoQueryResult;
// the stub answered a bare array, which Streamyfin's code (data.Items) cannot read (JS-070).
func TestTheCollectionsOfAnItemAreAnItemListResult(t *testing.T) {
	for _, r := range (&Server{}).Routes() {
		if r.Method != "GET" || r.Path != "/Items/{itemId}/Collections" {
			continue
		}
		w := httptest.NewRecorder()
		r.Handler(w, httptest.NewRequest("GET", "/Items/x/Collections", nil))
		var got struct {
			Items            *[]any
			TotalRecordCount *int
			StartIndex       *int
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Items == nil || got.TotalRecordCount == nil || got.StartIndex == nil {
			t.Errorf("answered %s (%v), want Items, TotalRecordCount and StartIndex", w.Body.String(), err)
		}
		return
	}
	t.Fatal("no route GET /Items/{itemId}/Collections")
}
