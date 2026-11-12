package transcode

import (
	"testing"
	"time"
)

// playerGivesUp is how long jellyfin-web's hls.js waits for a segment's first
// byte before it abandons the request (fragLoadPolicy maxTimeToFirstByteMs in
// the bundled hls.js 1.6.16; DOMAINS 37).
const playerGivesUp = 10 * time.Second

// A segment request that no worker takes is refused with 503 after
// queueTimeout (DOMAINS 37). The refusal has to reach the player before it
// abandons the request, with room for the request's own round trip and a
// busy replica, or the viewer gets a frozen picture instead of the retry a
// 503 brings. This replaces the e2e bar on the answer's wall-clock time
// (JS-081), which failed on slow runners: the wait is this constant.
func TestASegmentNoWorkerTakesIsRefusedBeforeThePlayerGivesUp(t *testing.T) {
	if margin := 2 * time.Second; queueTimeout > playerGivesUp-margin {
		t.Errorf("queueTimeout is %v: a refusal after it can reach the player later than %v before its %v limit", queueTimeout, margin, playerGivesUp)
	}
}
