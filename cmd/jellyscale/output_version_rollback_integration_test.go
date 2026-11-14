//go:build integration

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// Walking the documented rollback order from a finished upgrade: old workers,
// then the old api, then the old stream, the new workers last. At each step a
// viewer through every running pair of replicas gets a segment, never a 503
// after queueTimeout, and each job is run by a worker of its version.
func TestOutputVersionRollbackOrder(t *testing.T) {
	testenv.FFmpeg(t)
	// Opening the pool migrates a database no other test has used yet.
	migrated, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	migrated.Close()
	dir := t.TempDir()
	// The film and the two builds are made as in TestOutputVersionAcrossProcesses.
	filmFile = filepath.Join(dir, "film.mkv")
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=240",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=240",
		"-c:v", "mpeg4", "-q:v", "6", "-g", "96", "-c:a", "aac", filmFile).CombinedOutput()
	if err != nil {
		t.Fatalf("the film: %v\n%s", err, out)
	}
	builds = map[int]string{}
	for _, v := range []int{oldVersion, newVersion} {
		if builds[v], err = buildWith(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	c := newCluster(t, false, "20s")
	plays := 0
	step := func(name string, pairs ...[2]*proc) {
		for _, pair := range pairs {
			api, stream := pair[0], pair[1]
			p := c.startPlayback(api, stream)
			a := p.segment(plays * 6)
			plays++
			if !a.isSegment() {
				t.Errorf("%s: a viewer on api %s and stream %s got %s, want a segment", name, api.name(), stream.name(), a)
			}
			j, ok := c.rowOf(p)
			switch {
			case !ok:
				t.Errorf("%s: no job row for the viewer on %s", name, stream.name())
			case j.Version != stream.version:
				t.Errorf("%s: the job of a viewer on stream %s records version %d", name, stream.name(), j.Version)
			case c.workers[j.Worker] != j.Version:
				t.Errorf("%s: a job of version %d was run by %q, a worker of version %d", name, j.Version, j.Worker, c.workers[j.Worker])
			}
			p.stop()
		}
	}
	w5 := c.start(newVersion, "worker")
	s5, a5 := c.start(newVersion, "stream"), c.start(newVersion, "api")
	step("after the upgrade", [2]*proc{a5, s5})
	c.start(oldVersion, "worker")
	step("old workers started", [2]*proc{a5, s5})
	a4 := c.start(oldVersion, "api")
	step("old api started beside the new", [2]*proc{a5, s5}, [2]*proc{a4, s5})
	a5.stop()
	step("new api stopped", [2]*proc{a4, s5})
	s4 := c.start(oldVersion, "stream")
	step("old stream started beside the new", [2]*proc{a4, s5}, [2]*proc{a4, s4})
	s5.stop()
	step("new stream stopped", [2]*proc{a4, s4})
	w5.stop()
	step("new workers stopped", [2]*proc{a4, s4})
	c.checkRan()
}
