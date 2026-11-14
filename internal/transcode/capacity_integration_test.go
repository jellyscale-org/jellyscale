//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

// JS-087: a worker has cores, a job costs cores, and capacity is the cores a
// job needs free on one worker. These tests name what the task implies and the
// build must add:
//
//   - Spec.Cost() float64 (internal/playback, jobcost_test.go has its figures);
//   - the column transcode_nodes.cores (a number, 0 for a row written by the
//     release before, which has slots only) and Worker.Cores, which Present
//     writes;
//   - Jobs.Unavailable takes the spec, not its hash: the job's cost is not in a
//     hash;
//   - copyBound, the count of copies one worker holds before it takes no more;
//   - the series jellyscale_transcode_cores, _cores_busy and _queue_cores, with
//     _queue_depth kept.
//
// Rows these tests insert by hand set no cost column of their own, as a replica
// of the release before wrote them: the cost of a job is read from its spec
// (the whole of it, JSON) whoever queued it, so a build that keeps a cost
// beside the row must still count a row without one at the spec's cost, above
// zero. A row the build writes itself (ensure, insertHelper) is made by the
// tests that go through them.

var specSerial atomic.Int64

// costKinds are the sources the table in docs/transcoding.md prices, and the
// cost of a re-encode of each; "copy" is a copy, which costs almost nothing.
var costKinds = map[string]float64{"sdr1080": 0.8, "sdr4k": 1.6, "hdr1080": 2.0, "hdr4k": 3.2}

// costSpec is a spec of the kind that no other spec of the test equals, as the
// rig's film is played: through NewSpec from a source of the size and range the
// kind says, so the cost is what the build reads from it, and pointed at the
// rig's film, so a worker that runs it makes real segments.
func (r *rig) costSpec(t *testing.T, kind string) playback.Spec {
	t.Helper()
	height, hdr, copied := 1080, false, false
	switch kind {
	case "sdr4k":
		height = 2160
	case "hdr1080":
		hdr = true
	case "hdr4k":
		height, hdr = 2160, true
	case "copy":
		copied = true
	case "sdr1080":
	default:
		t.Fatalf("no kind %q", kind)
	}
	video := media.MediaStream{Index: 0, Type: "Video", Codec: "hevc", Profile: "Main 10", Level: 153, VideoRangeType: "SDR",
		Width: height * 16 / 9, Height: height, AverageFrameRate: 24}
	if hdr {
		video.VideoRangeType = "HDR10"
		video.ColorTransfer = media.Ptr(playback.PQ)
	}
	audio := media.MediaStream{Index: 1, Type: "Audio", Codec: "ac3", Channels: 2, IsDefault: true}
	source := playback.Source{ID: r.spec.Source, ETag: "etag1", Container: "mkv", Bitrate: 20_000_000, RunTimeTicks: 900_000_000,
		Streams: []media.MediaStream{video, audio}, Keyframes: fourSecondKeyframes(), VideoEnd: 900_000_000, Size: 22_500_000}
	spec, err := playback.NewSpec(r.spec.Item, playback.Params{SourceID: r.spec.Source, AudioIndex: 1, VideoBitrate: 20_000_000, AudioBitrate: 192_000,
		Copy: copied, AudioCodecs: playback.AudioCodecList("aac"), MaxAudioChannels: 2}, source)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Copy != copied || (spec.ToneMap != "") != hdr {
		t.Fatalf("kind %s made %+v", kind, spec)
	}
	if want, ok := costKinds[kind]; ok && math.Abs(spec.Cost()-want) > 1e-9 {
		t.Fatalf("kind %s costs %v, the table says %v", kind, spec.Cost(), want)
	}
	// What the film's own file is, so that a worker can run it; and a bitrate of
	// its own, so that the spec's hash is nobody else's.
	spec.Item, spec.Source, spec.ETag = r.spec.Item, r.spec.Source, r.spec.ETag
	spec.AudioBitrate = 64_000 + specSerial.Add(1)
	return spec
}

// fourSecondKeyframes is the index of a 90 second film with a keyframe every
// 4 seconds, which a copy is cut on.
func fourSecondKeyframes() (ticks []int64) {
	for at := int64(0); at < 900_000_000; at += 40_000_000 {
		ticks = append(ticks, at)
	}
	return ticks
}

// addNode inserts a worker of the version with cores, and keeps it seen until
// the test ends: the tests run longer than the 10 seconds a node counts for.
func (r *rig) addNode(t *testing.T, id string, cores float64, version int) {
	t.Helper()
	r.insertNodeRow(t, id, version, "INSERT INTO transcode_nodes (id, roles, cores, output_version) VALUES ($1, 'worker', "+strconv.FormatFloat(cores, 'f', -1, 64)+", $2)")
}

// addOldNode is a worker of the release before: slots, and no cores.
func (r *rig) addOldNode(t *testing.T, id string, slots, version int) {
	t.Helper()
	r.insertNodeRow(t, id, version, "INSERT INTO transcode_nodes (id, roles, slots, output_version) VALUES ($1, 'worker', "+strconv.Itoa(slots)+", $2)")
}

func (r *rig) insertNodeRow(t *testing.T, id string, version int, insert string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := r.pool.Exec(ctx, insert, id, version); err != nil {
		cancel()
		t.Fatalf("node %s: %v", id, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			r.pool.Exec(ctx, "UPDATE transcode_nodes SET seen_at = now() WHERE id = $1", id) //nolint:errcheck
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// addDeadNode is a worker that was last seen a minute ago.
func (r *rig) addDeadNode(t *testing.T, id string, cores float64) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), "INSERT INTO transcode_nodes (id, roles, cores, seen_at) VALUES ($1, 'worker', "+strconv.FormatFloat(cores, 'f', -1, 64)+", now() - interval '1 minute')", id); err != nil {
		t.Fatal(err)
	}
}

// addJob inserts a job of the spec in the state, as the release before wrote
// it (no cost of its own). A running one is on worker, with a lease.
func (r *rig) addJob(t *testing.T, spec playback.Spec, state, worker string, helper bool, age time.Duration) int64 {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf("owner-%d", specSerial.Add(1))
	if helper {
		owner = "helper"
	}
	var id int64
	err = r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, state, worker, lease_expires_at, created_at)
		VALUES ($1, $2, $3, 0, 0, $4, $5, $6, NULLIF($7, ''), CASE WHEN $6 = 'running' THEN now() + interval '1 hour' END, now() - make_interval(secs => $8)) RETURNING id`,
		spec.Hash(), raw, owner, helper, playback.OutputVersion, state, worker, age.Seconds()).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *rig) running(t *testing.T, kind, worker string) (playback.Spec, int64) {
	t.Helper()
	spec := r.costSpec(t, kind)
	return spec, r.addJob(t, spec, "running", worker, false, 0)
}

// watchedBy records a viewer of the job's spec: of the asking device, or
// another's.
func (r *rig) watchedBy(t *testing.T, spec playback.Spec, device string) string {
	t.Helper()
	psid := fmt.Sprintf("%032x", specSerial.Add(1))
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id) VALUES ($1, $2, $3::uuid, $4)`,
		psid, spec.Hash(), r.user, device); err != nil {
		t.Fatal(err)
	}
	return psid
}

func (r *rig) state(t *testing.T, id int64) string {
	t.Helper()
	return mustScalar(t, r, fmt.Sprintf("SELECT state FROM transcode_jobs WHERE id = %d", id))
}

// unavailable is what PlaybackInfo says to a viewer on "device".
func (r *rig) unavailable(t *testing.T, spec playback.Spec) string {
	t.Helper()
	return r.unavailableTo(t, spec, "device")
}

// unavailableTo is PlaybackInfo for a viewer on the device: its own jobs, those
// of "device" (r.viewer's), are not counted against it.
func (r *rig) unavailableTo(t *testing.T, spec playback.Spec, device string) string {
	t.Helper()
	reason, err := r.jobs(t).Unavailable(context.Background(), spec, r.user, device)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

func (r *rig) wantOffered(t *testing.T, spec playback.Spec) {
	t.Helper()
	if reason := r.unavailable(t, spec); reason != "" {
		t.Errorf("a %.1f core job is refused: %s", spec.Cost(), reason)
	}
}

// wantRefused says the spec is refused, and the reason has every word in has
// and none in not. A reason never speaks of slots, which are gone.
func (r *rig) wantRefused(t *testing.T, spec playback.Spec, has []string, not ...string) {
	t.Helper()
	reason := r.unavailable(t, spec)
	if reason == "" {
		t.Errorf("a %.1f core job is offered", spec.Cost())
		return
	}
	for _, word := range has {
		if pattern, ok := strings.CutPrefix(word, "re:"); ok {
			if !regexp.MustCompile(pattern).MatchString(reason) {
				t.Errorf("the refusal %q does not match %s", reason, pattern)
			}
		} else if !strings.Contains(reason, word) {
			t.Errorf("the refusal %q does not say %q", reason, word)
		}
	}
	for _, word := range append(not, "slot") {
		if strings.Contains(strings.ToLower(reason), word) {
			t.Errorf("the refusal %q says %q", reason, word)
		}
	}
}

// PlaybackInfo offers a transcode whenever some worker of the viewer's version
// has the job's cost free. Each case is a cluster, a job and what is said.
func TestPlaybackInfoOffersATranscodeWhereTheJobsCostIsFree(t *testing.T) {
	cases := map[string]func(t *testing.T, r *rig){
		// 0.8 five times is 4.0 in decimal and not in a float sum: the fifth fits.
		"a 4 core worker holding four 1080p SDR re-encodes takes a fifth": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			for range 4 {
				r.running(t, "sdr1080", "w")
			}
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
		"a 4 core worker holding a 4K tone mapped re-encode takes a 1080p SDR one": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			r.running(t, "hdr4k", "w")
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
		"a worker holding more copies than transcodeSlots admitted takes a re-encode": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			for range 5 {
				r.running(t, "copy", "w")
			}
			r.wantOffered(t, r.costSpec(t, "sdr4k"))
		},
		"a spec with a live job is offered whatever is free": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 1, playback.OutputVersion)
			spec, _ := r.running(t, "hdr4k", "w")
			r.wantOffered(t, spec)
		},
		"a queued job past queueTimeout counts for nothing": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, time.Minute)
			r.wantOffered(t, r.costSpec(t, "hdr4k"))
		},
		"the viewer's own job does not count": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 1, playback.OutputVersion)
			own, id := r.running(t, "sdr1080", "w")
			var owner string
			if err := r.pool.QueryRow(context.Background(), "SELECT owner FROM transcode_jobs WHERE id = $1", id).Scan(&owner); err != nil {
				t.Fatal(err)
			}
			if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id) VALUES ($1, $2, $3::uuid, 'device')`, owner, own.Hash(), r.user); err != nil {
				t.Fatal(err)
			}
			// What the viewer asks for is the stream that replaces the one it has
			// (jellyfin-web asks before it deletes the old).
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
		"several viewers of one spec cost one job": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			spec, _ := r.running(t, "hdr4k", "w")
			for range 3 {
				r.watchedBy(t, spec, "another device")
			}
			r.wantOffered(t, r.costSpec(t, "sdr1080")) // 3.2 and 0.8: three viewers at 3.2 each would not fit.
		},
		"a worker of the release before, with slots and no cores, is offered a re-encode": func(t *testing.T, r *rig) {
			r.addOldNode(t, "old", 2, playback.OutputVersion)
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
		"a copy is offered a worker that holds fewer copies than the bound": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			for range copyBound - 1 {
				r.running(t, "copy", "w")
			}
			r.wantOffered(t, r.costSpec(t, "copy"))
		},
		"a copy is offered while another worker is under the bound": func(t *testing.T, r *rig) {
			r.addNode(t, "full", 4, playback.OutputVersion)
			r.addNode(t, "free", 4, playback.OutputVersion)
			for range copyBound {
				r.running(t, "copy", "full")
			}
			r.wantOffered(t, r.costSpec(t, "copy"))
		},
		"a re-encode is offered where the copies are at their bound": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			for range copyBound {
				r.running(t, "copy", "w")
			}
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { run(t, newRig(t, nil)) })
	}
}

// PlaybackInfo refuses when no single worker has the cost free, and says so in
// cores, with the job's cost.
func TestPlaybackInfoRefusesWhereNoWorkerHasTheJobsCostFree(t *testing.T) {
	cases := map[string]func(t *testing.T, r *rig){
		"a 4 core worker holding what leaves less than the job needs": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			r.running(t, "hdr4k", "w")
			r.running(t, "sdr1080", "w")
			r.wantRefused(t, r.costSpec(t, "sdr1080"), []string{"cores", "0.8"}, "no transcode worker")
		},
		// 2 + 2 free cores are more than 3.2, and no one worker has 3.2.
		"two 4 core workers each holding a 2.0 re-encode refuse a 3.2 one": func(t *testing.T, r *rig) {
			for _, id := range []string{"a", "b"} {
				r.addNode(t, id, 4, playback.OutputVersion)
				r.running(t, "hdr1080", id)
			}
			r.wantRefused(t, r.costSpec(t, "hdr4k"), []string{"cores", "3.2"})
		},
		"a job that costs more than every worker has gives its cost and the most cores any has": func(t *testing.T, r *rig) {
			r.addNode(t, "small", 2, playback.OutputVersion)
			r.addNode(t, "bigger", 3, playback.OutputVersion)
			r.wantRefused(t, r.costSpec(t, "hdr4k"), []string{"cores", "3.2", `re:(^|[^.\d])3([^.\d]|\.\s|$)`})
		},
		"a queued job not yet claimed counts": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 4, playback.OutputVersion)
			r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 0)
			r.wantRefused(t, r.costSpec(t, "hdr4k"), []string{"cores", "3.2"})
		},
		"a queued job of the release before counts at its cost, above zero": func(t *testing.T, r *rig) {
			r.addNode(t, "w", 1, playback.OutputVersion)
			r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
			r.wantRefused(t, r.costSpec(t, "sdr1080"), []string{"cores"})
		},
		"the cores of a worker of another output version are none": func(t *testing.T, r *rig) {
			r.addNode(t, "own", 1, playback.OutputVersion)
			r.running(t, "sdr1080", "own")
			r.addNode(t, "other", 8, anotherVersion)
			r.wantRefused(t, r.costSpec(t, "sdr1080"), []string{"cores"})
		},
		"a job of another output version holds no cores of this one": func(t *testing.T, r *rig) {
			r.addNode(t, "own", 1, playback.OutputVersion)
			id := r.addJob(t, r.costSpec(t, "sdr1080"), "running", "own", false, 0)
			if _, err := r.pool.Exec(context.Background(), "UPDATE transcode_jobs SET output_version = $2 WHERE id = $1", id, anotherVersion); err != nil {
				t.Fatal(err)
			}
			r.wantOffered(t, r.costSpec(t, "sdr1080"))
		},
		"the cores of a worker that has gone are none": func(t *testing.T, r *rig) {
			r.addNode(t, "live", 1, playback.OutputVersion)
			r.running(t, "sdr1080", "live")
			r.addDeadNode(t, "gone", 8)
			r.wantRefused(t, r.costSpec(t, "sdr1080"), []string{"cores"})
		},
		"a copy is refused when every worker is at the bound, and the refusal names it": func(t *testing.T, r *rig) {
			for _, id := range []string{"a", "b"} {
				r.addNode(t, id, 16, playback.OutputVersion)
				for range copyBound {
					r.running(t, "copy", id)
				}
			}
			r.wantRefused(t, r.costSpec(t, "copy"), []string{strconv.Itoa(copyBound)})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) { run(t, newRig(t, nil)) })
	}
}

// A new viewer never waits behind helpers: when the job fits a worker once
// helpers stop, PlaybackInfo offers it, and the jobs that start take the helpers
// of one worker as it takes, and never a viewer's job.
func TestANewViewerStopsAsManyHelpersAsItTakesOnOneWorker(t *testing.T) {
	cancelled := func(t *testing.T, r *rig, ids ...int64) (n int) {
		t.Helper()
		for _, id := range ids {
			if r.state(t, id) == "cancelled" {
				n++
			}
		}
		return n
	}
	ask := func(t *testing.T, r *rig, spec playback.Spec) {
		t.Helper()
		r.wantOffered(t, spec)
		if _, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), psid(0), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("several helpers when one is not enough", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "w", 4, playback.OutputVersion)
		_, viewer := r.running(t, "sdr1080", "w")
		h1 := r.addJob(t, r.costSpec(t, "sdr4k"), "running", "w", true, 0)
		h2 := r.addJob(t, r.costSpec(t, "sdr4k"), "running", "w", true, 0)
		ask(t, r, r.costSpec(t, "hdr4k")) // 0.8 + 1.6 + 1.6 = 4.0 held; 3.2 needs both helpers gone.
		if n := cancelled(t, r, h1, h2); n != 2 {
			t.Errorf("%d of the two helpers were stopped for a 3.2 core job", n)
		}
		if r.state(t, viewer) != "running" {
			t.Errorf("a viewer's job is %s", r.state(t, viewer))
		}
	})
	t.Run("one helper when one is enough", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "w", 4, playback.OutputVersion)
		_, viewer := r.running(t, "sdr1080", "w")
		h1 := r.addJob(t, r.costSpec(t, "sdr4k"), "running", "w", true, 0)
		h2 := r.addJob(t, r.costSpec(t, "sdr4k"), "running", "w", true, 0)
		ask(t, r, r.costSpec(t, "sdr4k"))
		if n := cancelled(t, r, h1, h2); n != 1 {
			t.Errorf("%d helpers were stopped for a 1.6 core job that one frees room for", n)
		}
		if r.state(t, viewer) != "running" {
			t.Errorf("a viewer's job is %s", r.state(t, viewer))
		}
	})
	t.Run("the helpers of one worker, not of every worker", func(t *testing.T) {
		r := newRig(t, nil)
		var helpers []int64
		for _, id := range []string{"a", "b"} {
			r.addNode(t, id, 4, playback.OutputVersion)
			r.running(t, "sdr1080", id)
			helpers = append(helpers, r.addJob(t, r.costSpec(t, "hdr4k"), "running", id, true, 0))
		}
		ask(t, r, r.costSpec(t, "hdr4k"))
		if n := cancelled(t, r, helpers...); n != 1 {
			t.Errorf("%d helpers were stopped, on two workers, for a job that fits on either once one helper is gone", n)
		}
	})
	t.Run("a helper is left running when stopping every helper would not make room", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "w", 4, playback.OutputVersion)
		r.running(t, "hdr4k", "w")
		helper := r.addJob(t, r.costSpec(t, "sdr1080"), "running", "w", true, 0)
		// 3.2 viewer + 0.8 helper = 4.0 held; a 1.6 job needs more than the helper frees.
		r.wantRefused(t, r.costSpec(t, "sdr4k"), []string{"cores", "1.6"})
		asked := r.costSpec(t, "sdr4k")
		if _, _, err := r.jobs(t).ensure(context.Background(), asked, asked.Hash(), psid(0), 0, 0); err != nil {
			t.Fatal(err)
		}
		if r.state(t, helper) != "running" {
			t.Errorf("the helper was %s though stopping it frees too little", r.state(t, helper))
		}
	})
}

// A helper is neither queued nor claimed where it would put a worker over its cores.
func TestAHelperIsNeitherQueuedNorClaimedWhereItWouldOverrunAWorker(t *testing.T) {
	ctx := context.Background()
	t.Run("not queued", func(t *testing.T) {
		for cores, wantRows := range map[float64]int{4: 0, 16: 1} {
			t.Run(fmt.Sprint(cores, " cores"), func(t *testing.T) {
				r := newRig(t, nil)
				r.addNode(t, "w", cores, playback.OutputVersion)
				helped, id := r.running(t, "sdr4k", "w")
				r.running(t, "sdr4k", "w") // 3.2 held; a 1.6 helper would be 4.8.
				r.watchedBy(t, helped, "device")
				w := &Worker{Pool: r.pool, ID: "w", Cores: cores}
				if _, err := w.insertHelper(ctx, claimed{ID: id, Hash: helped.Hash(), Spec: helped}, 3, 12); err != nil {
					t.Fatal(err)
				}
				if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper"); n != wantRows {
					t.Errorf("%d helper rows on a %v core worker holding 3.2, want %d", n, cores, wantRows)
				}
			})
		}
	})
	t.Run("not claimed", func(t *testing.T) {
		for cores, want := range map[float64]bool{4: false, 16: true} {
			t.Run(fmt.Sprint(cores, " cores"), func(t *testing.T) {
				r := newRig(t, nil)
				r.addNode(t, "w", cores, playback.OutputVersion)
				r.running(t, "sdr4k", "w")
				r.running(t, "sdr4k", "w")
				helper := r.addJob(t, r.costSpec(t, "sdr4k"), "queued", "", true, 2*time.Second)
				_, ok, err := (&Worker{Pool: r.pool, ID: "w", Cores: cores}).claim(ctx)
				if err != nil || ok != want {
					t.Errorf("claim of helper %d on a %v core worker holding 3.2: %v, %v; want claimed %v", helper, cores, ok, err, want)
				}
			})
		}
	})
}

// A worker takes a job only if its cores hold it after the jobs it has.
func TestAWorkerClaimsOnlyWhatItsCoresHold(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, r *rig, id string, cores float64) (int64, bool) {
		t.Helper()
		job, ok, err := (&Worker{Pool: r.pool, ID: id, Cores: cores}).claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return job.ID, ok
	}
	t.Run("the second of two 4K tone mapped jobs is left", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 4, playback.OutputVersion)
		first := r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 2*time.Second)
		second := r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, time.Second)
		if id, ok := claim(t, r, "a", 4); !ok || id != first {
			t.Fatalf("the first claim is %d, %v; want job %d", id, ok, first)
		}
		if id, ok := claim(t, r, "a", 4); ok {
			t.Fatalf("a worker of 4 cores claimed a second 3.2 core job, %d", id)
		}
		if r.state(t, second) != "queued" {
			t.Errorf("the second job is %s", r.state(t, second))
		}
		// With a second worker, each runs one.
		r.addNode(t, "b", 4, playback.OutputVersion)
		if id, ok := claim(t, r, "b", 4); !ok || id != second {
			t.Errorf("the other worker claimed %d, %v; want job %d", id, ok, second)
		}
	})
	t.Run("a worker with too few cores free for the oldest job claims a later one that fits, at once", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 4, playback.OutputVersion)
		r.running(t, "hdr4k", "a")
		oldest := r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 3*time.Second)
		later := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, time.Second)
		if id, ok := claim(t, r, "a", 4); !ok || id != later {
			t.Fatalf("claimed %d, %v; want the later job %d that fits", id, ok, later)
		}
		if r.state(t, oldest) != "queued" {
			t.Errorf("the oldest job is %s: it was taken, or cancelled, though it did not fit", r.state(t, oldest))
		}
		r.addNode(t, "b", 4, playback.OutputVersion)
		if id, ok := claim(t, r, "b", 4); !ok || id != oldest {
			t.Errorf("the worker with the cores claimed %d, %v; want the oldest %d", id, ok, oldest)
		}
	})
	t.Run("costs that add up to its cores fit", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 4, playback.OutputVersion)
		for range 4 {
			r.running(t, "sdr1080", "a")
		}
		queued := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
		if id, ok := claim(t, r, "a", 4); !ok || id != queued {
			t.Errorf("claimed %d, %v; want the fifth 0.8 core job %d", id, ok, queued)
		}
		r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
		if id, ok := claim(t, r, "a", 4); ok {
			t.Errorf("a sixth 0.8 core job, %d, went to a worker of 4", id)
		}
	})
	t.Run("a job queued by the release before is claimed, and counted", func(t *testing.T) {
		r := newRig(t, nil)
		r.addOldNode(t, "old", 2, playback.OutputVersion)
		r.addNode(t, "a", 1, playback.OutputVersion)
		queued := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
		if id, ok := claim(t, r, "a", 1); !ok || id != queued {
			t.Fatalf("claimed %d, %v; want the job %d of the release before", id, ok, queued)
		}
		r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
		if id, ok := claim(t, r, "a", 1); ok {
			t.Errorf("a second 0.8 core job, %d, went to a worker of 1 core holding a job that counted for nothing", id)
		}
	})
	t.Run("a worker at the bound on copies takes no copy and still takes a re-encode", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 64, playback.OutputVersion)
		for range copyBound {
			r.running(t, "copy", "a")
		}
		copied := r.addJob(t, r.costSpec(t, "copy"), "queued", "", false, 2*time.Second)
		reencode := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, time.Second)
		if id, ok := claim(t, r, "a", 64); !ok || id != reencode {
			t.Fatalf("claimed %d, %v; want the re-encode %d", id, ok, reencode)
		}
		if r.state(t, copied) != "queued" {
			t.Errorf("the copy is %s", r.state(t, copied))
		}
	})
}

// The first segment of a re-encode is made, with the worker's ffmpeg, beside
// more copies than transcodeSlots admitted: offered, claimed, made.
func TestAReencodeIsMadeBesideMoreCopiesThanSlotsAdmitted(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.spec = r.costSpec(t, "sdr1080")
	r.hash = r.spec.Hash()
	var w *Worker
	r.worker(t, func(worker *Worker) { w, worker.Cores = worker, 4 })
	for range 5 {
		r.running(t, "copy", w.ID)
	}
	if reason := r.unavailable(t, r.spec); reason != "" {
		t.Fatalf("the re-encode is refused: %s", reason)
	}
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatalf("the re-encode made no first segment: %v", err)
	}
}

// A watched re-encode paused at its lead holds its cores, as one that is
// reading does: PlaybackInfo's answer is the same either way.
func TestAPausedReencodeHoldsItsCoresSoTheAnswerDoesNotChange(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.spec = r.costSpec(t, "sdr1080")
	r.hash = r.spec.Hash()
	r.worker(t, func(w *Worker) { w.Cores, w.Lead, w.Idle = 1, 12*time.Second, 5*time.Second })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	other := r.costSpec(t, "sdr1080")
	answer := func(when string) string {
		reason := r.unavailableTo(t, other, "a second viewer's device")
		if reason == "" || !strings.Contains(reason, "cores") {
			t.Errorf("%s: a 0.8 core job beside a 0.8 core re-encode on a worker of 1 core: %q", when, reason)
		}
		return reason
	}
	reading := answer("while it reads")
	waitFor(t, "the job to pause", 40*time.Second, func() bool { return r.paused() })
	if paused := answer("while it is paused"); paused != reading {
		t.Errorf("the answer was %q and is %q now that the job is paused", reading, paused)
	}
}

// Two 4K tone mapped re-encodes queued at once for one 4 core worker: one runs,
// the other starts no ffmpeg, waits queueTimeout and is answered as an untaken
// job is, with the cores short in the reason and no worker missing.
func TestTwo4KToneMappedJobsOnOneFourCoreWorkerRunOneAndRefuseTheOther(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	specs := []playback.Spec{r.costSpec(t, "hdr4k"), r.costSpec(t, "hdr4k")}
	j := r.jobs(t)
	ctx := context.Background()
	for i, spec := range specs {
		if err := j.Watch(ctx, r.viewer(psid(i)), spec.Hash(), -1); err != nil {
			t.Fatal(err)
		}
	}
	r.worker(t, func(w *Worker) { w.Cores = 4; w.NoHelpers = true })
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			_, results[i] = j.Segment(c, spec, spec.Hash(), r.viewer(psid(i)), 0, 0)
		}()
	}
	wg.Wait()
	var refused []int
	for i, err := range results {
		if err != nil {
			refused = append(refused, i)
		}
	}
	if len(refused) != 1 {
		t.Fatalf("results %v: want exactly one of the two refused", results)
	}
	err := results[refused[0]]
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "cores") || strings.Contains(err.Error(), "slot") || strings.Contains(err.Error(), "no transcode worker") {
		t.Errorf("the refused job was answered %q: want ErrBusy, in cores, with a worker running", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND worker IS NOT NULL", specs[refused[0]].Hash()); n != 0 {
		t.Errorf("the refused job ran on a worker %d times", n)
	}
	if procs := r.ffmpegProcesses(); len(procs) != 1 {
		t.Errorf("%d ffmpeg processes on a worker of 4 cores with one 3.2 core job", len(procs))
	}
}

// A dead worker's job that fails over to a worker that cannot hold it beside
// its own is not started, and its viewer is answered in cores, not as if no
// worker ran.
func TestAFailingOverJobIsNotStartedOverAWorkersCores(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	var survivor *Worker
	r.worker(t, func(w *Worker) { survivor, w.Cores, w.NoHelpers = w, 4, true })
	r.running(t, "hdr4k", survivor.ID) // Its own 3.2 cores, held.
	spec := r.costSpec(t, "hdr4k")
	if _, err := r.pool.Exec(context.Background(), "INSERT INTO transcode_nodes (id, roles, cores, locked) VALUES ('dead', 'worker', 4, true)"); err != nil {
		t.Fatal(err)
	}
	failing := r.addJob(t, spec, "running", "dead", false, 0)
	if _, _, _, err := Reap(context.Background(), r.pool, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the dead worker's lease to run out", 10*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND lease_expires_at < now()", failing) == 1
	})
	j := r.jobs(t)
	if err := j.Watch(context.Background(), r.viewer(psid(0)), spec.Hash(), -1); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := j.Segment(c, spec, spec.Hash(), r.viewer(psid(0)), 0, 0)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "cores") || strings.Contains(err.Error(), "no transcode worker") {
		t.Errorf("answered %v: want ErrBusy in cores", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND worker = $2", spec.Hash(), survivor.ID); n != 0 {
		t.Errorf("the survivor took the failing job over its cores")
	}
	if procs := r.ffmpegProcesses(); len(procs) != 0 {
		t.Errorf("%d ffmpeg processes: the survivor holds a job that has no ffmpeg here, and the failing one must start none", len(procs))
	}
}

// With two 4 core workers, each of two 4K tone mapped jobs runs on its own.
// Real processes: the two workers claim at the same moment.
func TestTwoFourCoreWorkersRunOneFourKToneMappedJobEach(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 2, 2, 6, r.ownStore(t), "JELLYSCALE_TEST_CORES=4", "JELLYSCALE_TEST_NO_HELPERS=1")
	j := r.jobs(t)
	ctx := context.Background()
	specs := []playback.Spec{r.costSpec(t, "hdr4k"), r.costSpec(t, "hdr4k")}
	for i, spec := range specs {
		if err := j.Watch(ctx, r.viewer(psid(i)), spec.Hash(), -1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.ensure(ctx, spec, spec.Hash(), psid(i), 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	Notify(ctx, j.Pool, jobHint)
	waitFor(t, "both jobs to run, one on each worker", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(DISTINCT worker) FROM transcode_jobs WHERE state = 'running'") == 2
	})
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'"); n != 2 {
		t.Errorf("%d jobs running on two workers of 4 cores", n)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'queued'"); n != 0 {
		t.Errorf("%d jobs still queued", n)
	}
}

// What a worker registers: the cores it declared.
func TestAWorkerRegistersTheCoresItDeclared(t *testing.T) {
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(worker *Worker) { w, worker.Cores = worker, 3.5 })
	var cores float64
	var slots int
	if err := r.pool.QueryRow(context.Background(), "SELECT cores::float8, slots FROM transcode_nodes WHERE id = $1", w.ID).Scan(&cores, &slots); err != nil || cores != 3.5 {
		t.Errorf("the worker's row says %v cores (%v), it declared 3.5", cores, err)
	}
	// The runbook starts new workers first, and a replica of the release before
	// counts a worker only by slots > 0: a row with cores and no slots would
	// make it refuse every transcode until it rolls. So the row keeps a
	// positive slots for one release.
	if slots <= 0 {
		t.Errorf("the worker's row says %d slots beside its cores; replicas of the release before would take it for no worker", slots)
	}
}
