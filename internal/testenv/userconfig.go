package testenv

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The capture of what a real Jellyfin 12.4 does with a viewer's settings (JS-116),
// compat/captures/user-configuration-12.4.json, read for the tests that hold jellyscale to it.
// The file's layout is described in test/differential/capture_user_configuration.py.

// ConfigStep is one request of a case: who sends it, by which route, naming whom.
type ConfigStep struct {
	Actor       string  `json:"actor"`  // U, O, A, anonymous or badtoken
	Route       string  `json:"route"`  // path, query or none
	Target      string  `json:"target"` // U, O, A, unknown, malformed, none or one of the spellings of U's own id
	Body        *string `json:"body"`   // null when the body was over 4000 characters (BodyBytes says how long)
	BodyBytes   int     `json:"bodyBytes"`
	ContentType string  `json:"contentType"`
}

// ConfigDelta is a Configuration as its difference from a new user's.
type ConfigDelta struct {
	Set    map[string]any `json:"set"`
	Absent []string       `json:"absent"`
}

// ConfigAfter is what the four places held after a case.
type ConfigAfter struct {
	Me            map[string]*ConfigDelta `json:"me"`
	Agree         bool                    `json:"agree"`
	ByAdmin       map[string]*ConfigDelta `json:"byAdmin"`
	ByUserSelf    *ConfigDelta            `json:"byUserSelf"`
	AuthenticateU *ConfigDelta            `json:"authenticateU"`
	ListLengths   map[string]int          `json:"listLengths"` // only for a body over 64 KB, which Jellyfin stored
}

// ConfigCase is one case of the capture.
type ConfigCase struct {
	Name    string       `json:"name"`
	Before  []ConfigStep `json:"before"`
	Request *ConfigStep  `json:"request"`
	Status  int          `json:"status"`
	After   ConfigAfter  `json:"after"`
}

// Culture is one entry of GET /Localization/Cultures.
type Culture map[string]any

// UserConfigurationCapture is the whole file.
type UserConfigurationCapture struct {
	Fields   []string `json:"fields"`
	Cultures struct {
		Status       int       `json:"status"`
		Entries      []Culture `json:"entries"`
		WithoutToken struct {
			Status int `json:"status"`
		} `json:"withoutToken"`
	} `json:"cultures"`
	NewUser struct {
		Me map[string]any `json:"me"`
	} `json:"newUser"`
	Cases []ConfigCase `json:"cases"`
}

// LoadUserConfigurationCapture reads the capture from path (relative to the test's package).
func LoadUserConfigurationCapture(t *testing.T, path string) *UserConfigurationCapture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c UserConfigurationCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Cases) == 0 || len(c.Fields) != 16 {
		t.Fatalf("%s holds %d cases and %d fields", path, len(c.Cases), len(c.Fields))
	}
	return &c
}

// Config is the Configuration a delta describes, from the new user's.
func (c *UserConfigurationCapture) Config(d *ConfigDelta) map[string]any {
	if d == nil {
		return nil
	}
	config := map[string]any{}
	for k, v := range c.NewUser.Me {
		config[k] = v
	}
	for _, k := range d.Absent {
		delete(config, k)
	}
	for k, v := range d.Set {
		config[k] = v
	}
	return config
}

// Case returns the case with the name.
func (c *UserConfigurationCapture) Case(t *testing.T, name string) ConfigCase {
	t.Helper()
	for _, k := range c.Cases {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("the capture has no case %q", name)
	return ConfigCase{}
}

// View is the Configuration of who ("U", "O" or "A") in one place: "me" (GET /Users/Me), "byAdmin" (GET /Users/{id}
// by A), "byUserSelf" (U's GET /Users/{id}, only U) or "authenticateU" (the User of U's next sign-in, only U).
// A capture whose places agree holds only "me".
func (a ConfigAfter) View(c *UserConfigurationCapture, place, who string) map[string]any {
	if a.Agree || place == "me" {
		return c.Config(a.Me[who])
	}
	switch place {
	case "byAdmin":
		return c.Config(a.ByAdmin[who])
	case "byUserSelf":
		return c.Config(a.ByUserSelf)
	}
	return c.Config(a.AuthenticateU)
}

// BodyText is the body the step sends. A body the capture holds only the size of is built here, the way the script
// built it: 2000 ids in a list, 70000 characters of an unknown field, or 70000 spaces after the object.
func (s ConfigStep) BodyText(caseName string) string {
	if s.Body != nil {
		return *s.Body
	}
	full := `{"AudioLanguagePreference":"spa","PlayDefaultAudioTrack":false,"SubtitleLanguagePreference":"eng","DisplayMissingEpisodes":true,` +
		`"GroupedFolders":["1a2b3c4d5e6f40718293a4b5c6d7e8f4","1a2b3c4d5e6f40718293a4b5c6d7e8f3"],"SubtitleMode":"OnlyForced",` +
		`"DisplayCollectionsView":true,"EnableLocalPassword":true,"OrderedViews":["1a2b3c4d5e6f40718293a4b5c6d7e8f1"],` +
		`"LatestItemsExcludes":["1a2b3c4d5e6f40718293a4b5c6d7e8f4"],"MyMediaExcludes":["1a2b3c4d5e6f40718293a4b5c6d7e8f1","1a2b3c4d5e6f40718293a4b5c6d7e8f2","1a2b3c4d5e6f40718293a4b5c6d7e8f3"],` +
		`"HidePlayedInLatest":false,"RememberAudioSelections":false,"RememberSubtitleSelections":false,"EnableNextEpisodeAutoPlay":false,"CastReceiverId":"WXYZ9876"}`
	switch {
	case strings.Contains(caseName, "(ids in a list)"):
		ids := make([]string, 2000)
		for n := range ids {
			ids[n] = `"` + uuid.UUID{15: byte(n), 14: byte(n >> 8)}.String() + `"`
		}
		return strings.Replace(full, `"LatestItemsExcludes":["1a2b3c4d5e6f40718293a4b5c6d7e8f4"]`, `"LatestItemsExcludes":[`+strings.Join(ids, ",")+`]`, 1)
	case strings.Contains(caseName, "(an unknown field)"):
		return strings.TrimSuffix(full, "}") + `,"Padding":"` + strings.Repeat("x", 70000) + `"}`
	}
	return full + strings.Repeat(" ", 70000)
}
