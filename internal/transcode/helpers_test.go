package transcode

import "testing"

func TestDefaultChunkSize(t *testing.T) {
	if got := (&Worker{}).chunkSize(); got != defaultChunkSegments {
		t.Errorf("the default chunk is %d segments, want %d", got, defaultChunkSegments)
	}
}
