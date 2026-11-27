package contract_test

// D-20261127-contract-amend. Only transport reasons are locked; a reason used solely by a preview capability may be
// reworded until its capability is stable. The three capabilities that leave at the cutover can never be stable.
// Each fixture changes one thing in the embedded contract.

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"jellyscale/internal/api"
	"jellyscale/internal/contract"
)

func reasonChange(t *testing.T, code, field, value string) []byte {
	t.Helper()
	return contractWith(t, func(document map[string]any) {
		reasons, _ := document["x-reasons"].(map[string]any)
		entry, _ := reasons[code].(map[string]any)
		entry[field] = value
	})
}

func startupRefusal(t *testing.T, document []byte) string {
	t.Helper()
	parsed, err := contract.Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	refused := ""
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				refused = fmt.Sprint(recovered)
			}
		}()
		(&api.Server{Roles: []string{api.RoleAPI}, Log: slog.New(slog.DiscardHandler), Contract: parsed}).Handler()
	}()
	return refused
}

func TestAPreviewReasonMayBeReworded(t *testing.T) {
	if refuses(t, reasonChange(t, "language-not-saved", "action", "retry")) {
		t.Fatalf("a preview capability's reason could not be reworded before its capability became stable")
	}
}

func TestATransportReasonIsStillLocked(t *testing.T) {
	if !refuses(t, reasonChange(t, "server-failure", "action", "none")) {
		t.Fatalf("a transport reason's action could be changed")
	}
}

func TestALegacyCapabilityCanNeverBeStable(t *testing.T) {
	for _, id := range []string{"playback-info", "user-configuration", "track-preferences"} {
		t.Run(id, func(t *testing.T) {
			document := contractWith(t, func(document map[string]any) {
				capabilities, _ := document["x-capabilities"].(map[string]any)
				capabilities[id].(map[string]any)["state"] = "stable"
			})
			message := startupRefusal(t, document)
			if !strings.Contains(message, "cutover") {
				t.Fatalf("making %s stable was refused with %q, want the cutover rule", id, message)
			}
		})
	}
}

func TestReasonMeaningsUseViewerWords(t *testing.T) {
	var document map[string]any
	if err := yaml.Unmarshal(contract.Document(), &document); err != nil {
		t.Fatal(err)
	}
	reasons, _ := document["x-reasons"].(map[string]any)
	for code, value := range reasons {
		entry, _ := value.(map[string]any)
		meaning, _ := entry["meaning"].(string)
		if meaning == "" {
			t.Errorf("%s has no meaning", code)
			continue
		}
		for _, mechanism := range []string{"token", "database", "worker", "Postgres"} {
			if strings.Contains(strings.ToLower(meaning), strings.ToLower(mechanism)) {
				t.Errorf("%s says %q, which names the mechanism %q", code, meaning, mechanism)
			}
		}
	}
}
