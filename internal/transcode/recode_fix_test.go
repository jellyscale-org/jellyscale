package transcode

import (
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

func TestAnAV1FallbackErrorSaysItIsNotBuilt(t *testing.T) {
	_, err := recodeArgs("av1", playback.Spec{})
	if err == nil || !strings.Contains(err.Error(), "fallback is not built") || strings.Contains(err.Error(), "no encoder") {
		t.Fatalf("AV1 fallback error = %v, want the unsupported fallback named, not a missing encoder", err)
	}
}
