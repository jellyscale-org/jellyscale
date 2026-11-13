package conform_test

// The checked-in schema (compat/schema-12.4.json), made by compat/gen-schema.sh
// from the OpenAPI document of the jellyfin/jellyfin:12.4 container the
// differential tier starts. Names the task implies:
//
//	(*Schema).Requires(typeName, field string) bool   whether the type's field is required, however the document marks it
//	(*Schema).Operation(method, path string) (route string, ok bool)

import (
	"os"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

func realSchema(t *testing.T) *conform.Schema {
	t.Helper()
	file, err := os.ReadFile("../schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTheCheckedInSchemaRecordsItsImageAndVersion(t *testing.T) {
	s := realSchema(t)
	if s.Image != "jellyfin/jellyfin:12.4" || s.Version != "12.4.0" {
		t.Errorf("image %q, version %q, want jellyfin/jellyfin:12.4 and 12.4.0 (the baseline of compat/endpoints.yaml)", s.Image, s.Version)
	}
}

// The fields that stopped Findroid, each of which the 12.4 document marks
// required in its own way: UserPolicy lists two in "required", the rest are
// non-nullable with no default.
func TestTheCheckedInSchemaRequiresEveryFieldThatStoppedFindroid(t *testing.T) {
	s := realSchema(t)
	for typeName, fields := range map[string][]string{
		"SessionInfoDto":  {"LastPlaybackCheckIn"},                                                  // #47
		"PlayerStateInfo": {"PlaybackOrder"},                                                        // #47
		"UserPolicy":      {"AuthenticationProviderId", "PasswordResetProviderId"},                  // #47
		"MediaSourceInfo": {"Protocol", "Type", "IsRemote", "SupportsDirectPlay", "HasSegments"},    // #94
		"MediaStream":     {"IsInterlaced", "IsDefault", "Type", "Index", "SupportsExternalStream"}, // #94, #95
	} {
		for _, field := range fields {
			if !s.Requires(typeName, field) {
				t.Errorf("%s.%s is not required in the checked-in schema", typeName, field)
			}
		}
	}
	if s.Requires("MediaStream", "Codec") {
		t.Error("MediaStream.Codec is nullable and has no requirement, yet is required")
	}
}

func TestTheRoutesTheClientsCallAreInTheCheckedInSchema(t *testing.T) {
	s := realSchema(t)
	for _, r := range [][2]string{
		{"GET", "/System/Info/Public"}, {"GET", "/Users/Public"}, {"GET", "/Branding/Configuration"},
		{"GET", "/QuickConnect/Enabled"}, {"POST", "/Users/AuthenticateByName"}, {"GET", "/Items/Suggestions"},
		{"GET", "/items/Suggestions"}, {"GET", "/Items/0123456789abcdef0123456789abcdef"}, {"POST", "/Items/abc/PlaybackInfo"},
		{"GET", "/Sessions"}, {"GET", "/UserViews"},
	} {
		if _, ok := s.Operation(r[0], r[1]); !ok {
			t.Errorf("%s %s has no operation", r[0], r[1])
		}
	}
}

func TestTheChecksInTheRealSchemaCatchEachFieldFindroidNeeded(t *testing.T) {
	c := conform.NewChecker(realSchema(t), conform.Rules{})
	vs := c.Check("sign-in", "POST", "/Users/AuthenticateByName", 200, "application/json", []byte(
		`{"User":{"Id":"8f1d6c1e3f0a4a539b0e2b6f5f3a9c11","Policy":{"IsAdministrator":true}},"SessionInfo":{"PlayState":{"CanSeek":false}},"AccessToken":"t","ServerId":"s"}`))
	found := map[string]bool{}
	for _, v := range vs {
		found[v.Route+" "+v.JSONPath+" "+v.Field] = true
		if !strings.Contains(v.String(), "sign-in") {
			t.Errorf("%q does not name the request", v)
		}
	}
	for _, want := range []string{
		"POST /Users/AuthenticateByName $.User.Policy AuthenticationProviderId",
		"POST /Users/AuthenticateByName $.User.Policy PasswordResetProviderId",
		"POST /Users/AuthenticateByName $.SessionInfo LastPlaybackCheckIn",
		"POST /Users/AuthenticateByName $.SessionInfo.PlayState PlaybackOrder",
	} {
		if !found[want] {
			t.Errorf("no violation %q; got %v", want, vs)
		}
	}
}

func TestTheGeneratorScriptAndTheRulesAreCheckedIn(t *testing.T) {
	script, err := os.ReadFile("../gen-schema.sh")
	if err != nil {
		t.Fatal(err)
	}
	// It starts the image the differential tier starts, and writes the checked-in file.
	for _, want := range []string{"jellyfin/jellyfin:12.4", "schema-12.4.json", "/api-docs/openapi.json"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("compat/gen-schema.sh never mentions %s", want)
		}
	}
	if _, err := conform.LoadRules("../conform.yaml"); err != nil {
		t.Error(err)
	}
}
