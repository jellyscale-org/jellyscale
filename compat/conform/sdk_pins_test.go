package conform_test

// The SDKs that decode jellyscale's answers are pinned (JS-070): jellyfin-sdk-kotlin
// 1.9.4's jars from Maven Central by exact version and sha256 in test/sdkjvm/jars.txt
// ("<group:artifact:version> <sha256>" per line, # for comments), the decoder in
// test/sdkjvm/Decode.java (ApiSerializer's settings), test/sdkjvm/decode.sh <Model> <file>
// fetching the jars into a cache and running it; @jellyfin/sdk in test/sdknode.

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestTheKotlinJarsArePinnedByVersionAndHash(t *testing.T) {
	raw, err := os.ReadFile("../../test/sdkjvm/jars.txt")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`^[\w.-]+:[\w.-]+:\d+\.\d+\.\d+ [0-9a-f]{64}$`)
	got := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !pin.MatchString(line) {
			t.Errorf("%q is not group:artifact:version and a sha256", line)
		}
		got[strings.Fields(line)[0]] = true
	}
	for _, want := range []string{"org.jellyfin.sdk:jellyfin-model-jvm:1.9.4"} {
		if !got[want] {
			t.Errorf("jars.txt does not pin %s", want)
		}
	}
	for _, prefix := range []string{"org.jetbrains.kotlin:kotlin-stdlib:", "org.jetbrains.kotlinx:kotlinx-serialization-core-jvm:", "org.jetbrains.kotlinx:kotlinx-serialization-json-jvm:"} {
		found := false
		for k := range got {
			found = found || strings.HasPrefix(k, prefix)
		}
		if !found {
			t.Errorf("jars.txt pins nothing from %s", prefix)
		}
	}
}

func TestTheDecoderUsesApiSerializersSettings(t *testing.T) {
	raw, err := os.ReadFile("../../test/sdkjvm/Decode.java")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"setIgnoreUnknownKeys(true)", "setExplicitNulls(false)", "setCoerceInputValues(true)", "setAllowSpecialFloatingPointValues(true)", "org.jellyfin.sdk.model.api."} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("Decode.java lacks %s", want)
		}
	}
	// A failure names the model, the JSON path and the field.
	if !strings.Contains(string(raw), "FAILED") {
		t.Error("Decode.java never reports FAILED")
	}
}

func TestTheJellyfinNodeSDKIsPinnedToAnExactVersion(t *testing.T) {
	raw, err := os.ReadFile("../../test/sdknode/package.json")
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct{ Dependencies map[string]string }
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatal(err)
	}
	if v := pkg.Dependencies["@jellyfin/sdk"]; !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Errorf("@jellyfin/sdk is %q, want an exact version", v)
	}
	for _, f := range []string{"package-lock.json", "discover.mjs"} {
		if _, err := os.Stat("../../test/sdknode/" + f); err != nil {
			t.Error(err)
		}
	}
}
