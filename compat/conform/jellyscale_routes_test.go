package conform_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

// JS-123: the answer of PATCH /jellyscale/v1/users/configuration is a UserConfiguration, and compat/conform.yaml holds it
// to that schema, whatever shape the rule takes. These checks use the checked-in schema and rules, as the gates do.

const patchRoute = "/jellyscale/v1/users/configuration"

const newUsersConfiguration = `{"PlayDefaultAudioTrack":true,"SubtitleLanguagePreference":"","DisplayMissingEpisodes":false,"GroupedFolders":[],
	"SubtitleMode":"Default","DisplayCollectionsView":false,"EnableLocalPassword":false,"OrderedViews":[],"LatestItemsExcludes":[],
	"MyMediaExcludes":[],"HidePlayedInLatest":true,"RememberAudioSelections":true,"RememberSubtitleSelections":true,
	"EnableNextEpisodeAutoPlay":true,"CastReceiverId":"F007D354"}`

func realChecker(t *testing.T) *conform.Checker {
	t.Helper()
	file, err := os.ReadFile("../schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := conform.LoadRules("../conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return conform.NewChecker(s, rules)
}

func patchAnswer(c *conform.Checker, body string) []conform.Violation {
	return c.Check("patch", "PATCH", patchRoute, 200, "application/json; charset=utf-8", []byte(body))
}

func TestAnAnswerOfThePatchThatIsAUserConfigurationHasNoViolationAndIsCounted(t *testing.T) {
	c := realChecker(t)
	if vs := patchAnswer(c, newUsersConfiguration); len(vs) != 0 {
		t.Fatalf("a new user's configuration is a violation: %v", vs)
	}
	if n, _ := c.Counts(); n != 1 {
		t.Errorf("the answer was counted as %d JSON responses, want 1: it is checked, not skipped", n)
	}
	var withAudio map[string]any
	json.Unmarshal([]byte(newUsersConfiguration), &withAudio) //nolint:errcheck
	withAudio["AudioLanguagePreference"] = "deu"
	raw, _ := json.Marshal(withAudio)
	if vs := patchAnswer(realChecker(t), string(raw)); len(vs) != 0 {
		t.Errorf("a configuration with AudioLanguagePreference is a violation: %v", vs)
	}
}

func TestAnAnswerOfThePatchLackingARequiredFieldOrWithASubtitleModeOutsideTheEnumIsAViolation(t *testing.T) {
	var full map[string]any
	if err := json.Unmarshal([]byte(newUsersConfiguration), &full); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"PlayDefaultAudioTrack", "SubtitleMode", "GroupedFolders", "HidePlayedInLatest", "EnableNextEpisodeAutoPlay"} {
		without := map[string]any{}
		for k, v := range full {
			if k != field {
				without[k] = v
			}
		}
		raw, _ := json.Marshal(without)
		found := false
		for _, v := range patchAnswer(realChecker(t), string(raw)) {
			found = found || (v.Field == field && v.Message != "" && !strings.Contains(v.Message, "no operation"))
		}
		if !found {
			t.Errorf("an answer lacking %s is not a violation naming the field", field)
		}
	}
	for _, mode := range []any{"Sometimes", "always", float64(1)} {
		bad := map[string]any{}
		for k, v := range full {
			bad[k] = v
		}
		bad["SubtitleMode"] = mode
		raw, _ := json.Marshal(bad)
		found := false
		for _, v := range patchAnswer(realChecker(t), string(raw)) {
			found = found || v.Field == "SubtitleMode"
		}
		if !found {
			t.Errorf("a SubtitleMode of %v is not a violation", mode)
		}
	}
}
