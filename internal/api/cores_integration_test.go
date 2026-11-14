//go:build integration

package api

import "testing"

// JS-087: PlaybackInfo offers a transcode whenever some worker has the job's
// cost in cores free, and refuses, with jellyfin-web's dialog (the refusal is
// what shows it: test/e2e's "no worker" scenario drives the dialog itself),
// when no worker has. transcodeEnv's film is a 720p re-encode, which costs 0.8
// cores (docs/transcoding.md). The column transcode_nodes.cores is named in
// internal/transcode/capacity_integration_test.go.
func TestPlaybackInfoComparesTheJobsCostWithTheWorkersCores(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	reencode := playbackDecisions.WithLabelValues("reencode")
	want := func(what string, cores string, offered bool) {
		t.Helper()
		mustExec(t, e.pool, "TRUNCATE transcode_nodes")
		mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, "+cores+") VALUES ('w1', 'worker', "+map[string]string{"cores": "1", "slots": "2"}[cores]+")")
		reencodes, refusals := counterValue(t, reencode), counterValue(t, playbackRefusals)
		e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
		gotOffered := counterValue(t, reencode) == reencodes+1
		if gotOffered != offered || (counterValue(t, playbackRefusals) == refusals+1) == offered {
			t.Errorf("%s: offered %v, want %v", what, gotOffered, offered)
		}
	}
	want("a worker of 1 core and a 0.8 core job", "cores", true)
	want("a worker of the release before, with slots and no cores", "slots", true)
	mustExec(t, e.pool, "TRUNCATE transcode_nodes")
	mustExec(t, e.pool, "INSERT INTO transcode_nodes (id, roles, cores) VALUES ('w1', 'worker', 0.5)")
	reencodes, refusals := counterValue(t, reencode), counterValue(t, playbackRefusals)
	e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	if counterValue(t, reencode) != reencodes || counterValue(t, playbackRefusals) != refusals+1 {
		t.Errorf("a 0.8 core job on a worker of 0.5 cores was offered, not refused")
	}
}
