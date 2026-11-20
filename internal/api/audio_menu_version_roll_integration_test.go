//go:build integration

package api

import (
	"encoding/json"
	"strconv"
	"testing"

	"jellyscale/internal/playback"
)

// A film whose default track needs a transcode whatever the viewer picks (JS-147, D-029): while the only live worker is of
// another output version (a rolling deploy) the job is offered and waits for a worker of the version, as it did, for a viewer
// who saved a language and for one who picked a track. Refusing them would take the film away for the length of the roll.
func TestAFilmThatNeedsATranscodeIsStillOfferedWhenTheOnlyWorkerIsOfAnotherOutputVersion(t *testing.T) {
	f := audioEnv(t)
	film, sources := f.film(t, "Four languages", []track{eng(true), deu(false), {"fra", false}, {"", false}})
	// The default track is one no browser plays, so the film is transcoded whatever is picked.
	var streams []map[string]any
	var raw []byte
	if err := f.pool.QueryRow(t.Context(), "SELECT streams FROM media_sources WHERE id = $1", sources[0]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatal(err)
	}
	streams[1]["Codec"] = "dts"
	raw, _ = json.Marshal(streams)
	mustExec(t, f.pool, "UPDATE media_sources SET streams = $2 WHERE id = $1", sources[0], raw)
	mustExec(t, f.pool, "TRUNCATE transcode_nodes")
	mustExec(t, f.pool, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ('old', 'worker', 2, 4, "+strconv.Itoa(playback.OutputVersion+1)+")")
	german := f.listener(t, "german", "deu", false)
	for replica := range 2 {
		for _, extra := range []map[string]any{nil, {"AudioStreamIndex": 3}} {
			reply := f.ask(t, replica, german, film, extra)
			if refusedPlan(reply) {
				t.Errorf("replica %d, %v: refused (%v), want a transcode offered", replica, extra, reply["ErrorCode"])
			}
		}
	}
}
