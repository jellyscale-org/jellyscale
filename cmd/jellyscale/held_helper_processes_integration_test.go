//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-161 (D-046, D-050), with real processes: two workers, an api and the real ffmpeg at the film's speed. The in-process
// checks of internal/transcode judge the ledger on rows; these run the workers, so a build that stops the helper in
// ensure before judging the room, leaves a job behind for a refused play, or starves the helper of the first viewer
// fails here. The film's re-encode costs 0.8 cores. Each worker declares 0.7: too little to hold that job alone (0.7 < 0.8, and
// so under real time at 1.2 as well) and enough to take a share of it (above the 0.78 of its cost the design takes), so the
// play is admitted as shared: the viewer's job on one worker, its helper on the other. A newcomer who needs 0.05 cores, a
// copy of another film, fits on neither worker without stopping that helper, whatever the newcomer costs; a second
// re-encode of the same cost fits nowhere even then, which is plain shortage (a refusal with no helper stopped either).
//
// The first viewer's speed ("does not fall below 1.0") is not asserted: a check never passes or fails on a speed
// measured on the machine it runs on. What it means is that the helper is not stopped, delayed or left unclaimed for
// the newcomer, and that is asserted from the rows.

// infoAt is a PlaybackInfo at the api for the film with a streaming bitrate cap (a different cap is a spec of its own),
// asked by the device whose token and id are given (a device that asks again replaces its own play, which is not a newcomer), and what it answered: whether a transcode was offered, and how long the answer took.
func (c *cluster) infoAt(api *proc, item, token, device string, maxBitrate int) (offered bool, took time.Duration, body string) {
	c.t.Helper()
	c.login(api)
	raw, err := os.ReadFile("../../compat/captures/transcode-12.4.json")
	if err != nil {
		c.t.Fatal(err)
	}
	var capture struct{ DeviceProfile json.RawMessage }
	if err := json.Unmarshal(raw, &capture); err != nil {
		c.t.Fatal(err)
	}
	began := time.Now()
	code, _, answer, err := c.call("POST", api, "/Items/"+item+"/PlaybackInfo",
		fmt.Sprintf(`{"DeviceProfile":%s,"MaxStreamingBitrate":%d,"AudioStreamIndex":"1"}`, capture.DeviceProfile, maxBitrate), "X-Emby-Token", token,
		"Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="%s", Version="1", Token="%s"`, device, token))
	took = time.Since(began)
	if err != nil || code != 200 {
		c.t.Fatalf("PlaybackInfo: %d %v %s", code, err, answer)
	}
	var info struct {
		MediaSources []struct{ TranscodingUrl string }
	}
	if err := json.Unmarshal(answer, &info); err != nil {
		c.t.Fatal(err)
	}
	return len(info.MediaSources) > 0 && info.MediaSources[0].TranscodingUrl != "", took, string(answer)
}

func (c *cluster) liveJobs() (n int) {
	for _, j := range c.rows() {
		if j.State == "queued" || j.State == "running" {
			n++
		}
	}
	return n
}

// itemsByName are the films the api lists, once it lists as many as asked.
func (c *cluster) itemsByName(api *proc, n int) map[string]string {
	c.t.Helper()
	c.login(api)
	var found map[string]string
	waitUntil(c.t, "the api to list both films", func() bool {
		_, _, body, _ := c.call("GET", api, "/Items?Recursive=true&IncludeItemTypes=Movie", "")
		var items struct{ Items []struct{ Id, Name string } }
		found = map[string]string{}
		if json.Unmarshal(body, &items) == nil {
			for _, it := range items.Items {
				found[it.Name] = it.Id
			}
		}
		return len(found) == n
	})
	return found
}

func TestANewViewerWhoFitsOnlyByStoppingAHelperIsRefusedWithRealWorkersAndTheHelperKeepsRunning(t *testing.T) {
	c := newCluster(t, true, "60s")
	// A second film that plays as a copy (H.264 in Matroska with AC-3, which no browser takes): the newcomer, at 0.05 cores.
	dir := filepath.Join(c.dir, "media", "Copy (2021)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=120",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=120", "-c:v", "libx264", "-g", "48", "-c:a", "ac3",
		filepath.Join(dir, "Copy (2021).mkv")).CombinedOutput(); err != nil {
		t.Fatalf("the copy film: %v\n%s", err, out)
	}
	c.extra = `"transcodeCores": 0.7,`
	w1 := c.start(newVersion, "worker")
	w2 := c.start(newVersion, "worker")
	c.extra = ""
	api := c.start(newVersion, "api,stream")
	films := c.itemsByName(api, 2)
	if len(films) != 2 {
		t.Fatalf("the films: %v", films)
	}
	var reencoded, copied string
	for name, id := range films {
		if name == "Copy" {
			copied = id
		} else {
			reencoded = id
		}
	}
	c.item = reencoded
	first := c.startPlayback(api, api)
	var once sync.Once
	end := c.keepPlaying(first)
	stop := func() { once.Do(end) }
	defer stop()

	var helper row
	waitUntil(t, "the first viewer's job to be helped by the other worker", func() bool {
		for _, j := range c.rows() {
			if j.Helper && j.State == "running" && j.Worker != "" {
				helper = j
				return true
			}
		}
		return false
	})
	own, ok := c.rowOf(first)
	if !ok || own.Worker == helper.Worker || (own.Worker != w1.workerID && own.Worker != w2.workerID) {
		t.Fatalf("the viewer's job %+v and its helper %+v are not on the two workers", own, helper)
	}
	before := len(c.rows())
	other := api.signInAs(t, "another-device") // Another viewer, on another device.

	// Three newcomers, each of a spec of its own: the copy, which fits only if the helper stops, and two re-encodes.
	for _, tc := range []struct {
		name, item string
		limit      int
		reason     string
	}{{"a copy", copied, 2147483647, playback.CoresHeldByWatchedFilm}, {"a re-encode", reencoded, 1_500_000, ""}, {"another re-encode", reencoded, 1_000_000, ""}} {
		seen := len(logLines(api, decisionLineMsg))
		offered, took, body := c.infoAt(api, tc.item, other, "another-device", tc.limit)
		if offered {
			t.Fatalf("%s: a newcomer who finds no cores but the first viewer's helper's was offered a transcode (%d chars); the jobs %+v; the decision lines %v", tc.name, len(body), c.rows(), logLines(api, decisionLineMsg))
		}
		// 10 s only bounds a hang: the refusal is the answer of one request, within 5 s on any host the task is built for.
		if took > 10*time.Second {
			t.Errorf("%s: the refusal took %v", tc.name, took)
		}
		waitUntil(t, "the api to write the decision line of the refusal", func() bool { return len(logLines(api, decisionLineMsg)) > seen })
		lines := logLines(api, decisionLineMsg)
		last := lines[len(lines)-1]
		if tc.reason != "" && !strings.Contains(last, tc.reason) {
			t.Errorf("%s: the decision line does not give the reason %s: %s", tc.name, tc.reason, last)
		}
		if tc.reason == "" && strings.Contains(last, playback.CoresHeldByWatchedFilm) {
			t.Errorf("%s: a shortage that stopping the helper would not mend is given as held by a watched film: %s", tc.name, last)
		}
	}
	for _, j := range c.rows() {
		if j.ID == helper.ID && j.State != "running" {
			t.Errorf("the helper is %s (%s) after newcomers were refused", j.State, j.Error)
		}
		if j.ID == own.ID && j.State != "running" {
			t.Errorf("the first viewer's job is %s (%s)", j.State, j.Error)
		}
	}
	if after := len(c.rows()); after != before {
		t.Errorf("refused plays left %d job rows", after-before)
	}
	if n := c.liveJobs(); n != 2 {
		t.Errorf("%d live jobs after the refusals, want the first viewer's job and its helper", n)
	}

	// The first viewer leaves: the same copy is admitted at its next PlaybackInfo.
	stop()
	waitUntil(t, "the first viewer's jobs to end", func() bool { return c.liveJobs() == 0 })
	if offered, _, body := c.infoAt(api, copied, other, "another-device", 2147483647); !offered {
		t.Errorf("after the first viewer stopped, the same play is still refused: %s", body)
	}
	c.checkRan()
}
