//go:build integration

package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-161: every admitted copy and re-encode line of JS-153's decision line says whether the play was admitted on one
// worker or shared, with the job's cost and each capacity counted, in reference cores; a refused line gives the cost
// and the most any worker had. One play admitted alone, one admitted shared, one refused, as the api process writes
// them. Names the build adds to the line: admitted (one_worker or shared), cost, capacities (a comma separated list,
// one for each worker counted), most_capacity (a refused line). The workers are rows, as piSite makes them; the
// capacity a worker's row carries is what it measured or declared.

func (s *piSite) workersWith(cores ...float64) {
	s.t.Helper()
	ctx := context.Background()
	if _, err := s.site.pool.Exec(ctx, "TRUNCATE transcode_nodes"); err != nil {
		s.t.Fatal(err)
	}
	for i, c := range cores {
		if _, err := s.site.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, roles, slots, cores, output_version) VALUES ($1, 'worker', 3, $2, $3)",
			string(rune('a'+i)), c, playback.OutputVersion); err != nil {
			s.t.Fatal(err)
		}
	}
}

// lastLine asks and returns the one decision line the answer wrote.
func (s *piSite) lastLine(item string, body map[string]any) string {
	s.t.Helper()
	if _, err := s.site.pool.Exec(context.Background(), "UPDATE transcode_nodes SET seen_at = now()"); err != nil {
		s.t.Fatal(err)
	}
	before := len(logLines(s.api, decisionLineMsg))
	s.ask(item, body)
	for deadline := time.Now().Add(10 * time.Second); len(logLines(s.api, decisionLineMsg)) <= before && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	lines := logLines(s.api, decisionLineMsg)
	if len(lines) <= before {
		s.t.Fatalf("no decision line:\n%s", s.api.log)
	}
	return lines[len(lines)-1]
}

func TestTheDecisionLineSaysWhetherAPlayWasAdmittedAloneOrSharedAndWhatWasCounted(t *testing.T) {
	s := newPlaybackInfoSite(t)
	film := s.film(piFilm{video: map[string]any{"Codec": "hevc", "Profile": "Main 10", "Level": 153, "Width": 3840, "Height": 2160,
		"VideoRangeType": "HDR10", "ColorTransfer": "smpte2084"}})
	body := s.body(film, nil, nil)

	s.workersWith(8)
	line := s.lastLine(film, body)
	if v, _ := field(line, "admitted"); v != "one_worker" {
		t.Errorf("a play one worker holds: admitted=%q: %s", v, line)
	}
	if c := num(line, "cost"); math.Abs(c-3.2) > 1e-9 {
		t.Errorf("a 4K tone mapped re-encode: cost=%v: %s", c, line)
	}
	if v, _ := field(line, "capacities"); !strings.Contains(v, "8") {
		t.Errorf("capacities=%q does not give the worker's 8 cores: %s", v, line)
	}

	s.workersWith(2.72, 2.72)
	line = s.lastLine(film, body)
	if v, _ := field(line, "admitted"); v != "shared" {
		t.Errorf("a play two workers hold together: admitted=%q: %s", v, line)
	}
	if v, _ := field(line, "capacities"); strings.Count(v, "2.72") != 2 {
		t.Errorf("capacities=%q does not give both workers' 2.72: %s", v, line)
	}

	s.workersWith(2.72)
	line = s.lastLine(film, body)
	if v, _ := field(line, "method"); v != "refused" {
		t.Fatalf("a play no worker holds, alone: %s", line)
	}
	if c := num(line, "cost"); math.Abs(c-3.2) > 1e-9 {
		t.Errorf("refused line: cost=%v: %s", c, line)
	}
	if m := num(line, "most_capacity"); math.Abs(m-2.72) > 1e-9 {
		t.Errorf("refused line: most_capacity=%v: %s", m, line)
	}
}
