package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// JS-128: the rule is written where a client author and a reader of the invariants look for it. A reader can only
// check the words; this holds that each file says it, whatever the sentence around them.
func TestDocsSayAPlaylistCarriesTheTokenOnlyWhenTheRequestDid(t *testing.T) {
	rule := "only when the request carried it in the query"
	for file, words := range map[string][]string{
		"../../docs/access.md":      {rule, "every playlist and segment request", "header"},
		"../../docs/transcoding.md": {rule, "master.m3u8", "main.m3u8", "deliveryurls"},
		"../../DOMAINS.md":          {rule, "master.m3u8", "main.m3u8", "transcodingurl"},
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(raw))
		for _, word := range words {
			if !strings.Contains(text, word) {
				t.Errorf("%s does not say %q", file, word)
			}
		}
	}
}

// JS-040's criteria stay as they are; its notes say what JS-128 already did.
func TestJS040NotesSayHeaderRequestsAlreadyCarryNoToken(t *testing.T) {
	raw, err := os.ReadFile("../../tasks/prd.json")
	if err != nil {
		t.Fatal(err)
	}
	var prd struct {
		Tasks []struct {
			ID                 string   `json:"id"`
			Notes              string   `json:"notes"`
			AcceptanceCriteria []string `json:"acceptanceCriteria"`
		} `json:"userStories"`
	}
	if err := json.Unmarshal(raw, &prd); err != nil {
		t.Fatal(err)
	}
	for _, task := range prd.Tasks {
		if task.ID != "JS-040" {
			continue
		}
		if !strings.Contains(strings.ToLower(task.Notes), "header requests already carry none") {
			t.Errorf("JS-040's notes do not say header requests already carry none: %q", task.Notes)
		}
		if len(task.AcceptanceCriteria) != 2 || task.AcceptanceCriteria[0] != "No viewer token in any playlist or segment URL" || task.AcceptanceCriteria[1] != "Kodi's TranscodingUrl editing still works" {
			t.Errorf("JS-040's criteria changed: %q", task.AcceptanceCriteria)
		}
		return
	}
	t.Error("no JS-040 in tasks/prd.json")
}
