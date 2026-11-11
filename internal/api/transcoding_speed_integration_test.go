//go:build integration

package api

import (
	"context"
	"math"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"jellyscale/internal/metrics"
)

// JS-081: the speed GET /Sessions shows is checked against what the worker
// recorded for the job and against itself, not against a range that depends on
// how fast the machine encodes. A throttle (ffmpeg reads at a multiple of the
// film's speed) is a ceiling, so a job under a 0.3 throttle is below 1 on any
// machine; nothing here needs the machine to encode faster than the film.

// recordedSpeed is the speed the worker wrote for the running job, or -1.
func (l *liveTranscode) recordedSpeed(t *testing.T) float64 {
	t.Helper()
	var speed *float64
	l.e.pool.QueryRow(context.Background(), "SELECT encode_speed FROM transcode_jobs WHERE state = 'running'").Scan(&speed) //nolint:errcheck
	if speed == nil {
		return -1
	}
	return *speed
}

var pausedLine = regexp.MustCompile(`(?m)^jellyscale_transcode_job_paused\{job="\d+"\} 1$`)

// pausedAtItsLead waits until the worker says its one job is paused at the lead.
func (l *liveTranscode) pausedAtItsLead(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		metrics.Handler(l.w).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		if pausedLine.MatchString(recorder.Body.String()) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the job never paused at its lead")
}

// A job read slowly shows its measured speed, flagged below real time exactly
// when that speed is below 1, with the film's frame rate times it for
// Framerate. Under a 0.3 throttle the speed cannot reach 1, so the flag is set.
func TestSessionsFlagsAJobExactlyWhenItsShownSpeedIsBelowOne(t *testing.T) {
	l := startLive(t, "0.3", time.Hour)
	info := l.speedy(t)
	speed := info["EncodeSpeed"].(float64)
	if speed >= 1 {
		t.Errorf("EncodeSpeed %v of a job whose ffmpeg reads at 0.3 times the film's speed", speed)
	}
	if flagged, _ := info["IsBelowRealTime"].(bool); flagged != (speed < 1) {
		t.Errorf("IsBelowRealTime %v for EncodeSpeed %v: %v", flagged, speed, info)
	}
	if info["IsBelowRealTime"] != true {
		t.Errorf("a job below 1x is not flagged: %v", info)
	}
	if fps, _ := info["Framerate"].(float64); math.Abs(fps-speed*24) > 0.51 {
		t.Errorf("Framerate %v does not follow EncodeSpeed %v of a 24 fps film", info["Framerate"], speed)
	}
}

// A job under a throttle of 3 shows the flag that follows its speed, whatever
// that is on the machine: set when below 1, clear when not. (On a machine that
// encodes faster than the film, which is any that is not starved, this is the
// job that keeps up and is not flagged.)
func TestSessionsFlagFollowsTheShownSpeedOfAJobThatMayKeepUp(t *testing.T) {
	l := startLive(t, "3", time.Hour)
	info := l.speedy(t)
	speed := info["EncodeSpeed"].(float64)
	if flagged, _ := info["IsBelowRealTime"].(bool); flagged != (speed < 1) {
		t.Errorf("IsBelowRealTime %v for EncodeSpeed %v: %v", flagged, speed, info)
	}
}

// At its lead the job is idle by design: both replicas show the speed the worker
// recorded for it, Framerate follows it, the flag follows the held speed, and the speed after
// a long pause is not lower than when it paused.
func TestSessionsShowTheSpeedTheWorkerRecordedAndHoldItAcrossAPauseAtTheLead(t *testing.T) {
	l := startLive(t, "3", 6*time.Second)
	l.speedy(t)
	l.pausedAtItsLead(t)
	// The record is written with the worker's heartbeat; wait for the first one
	// after the pause, which leaves the value alone (a paused job keeps the speed it had).
	deadline := time.Now().Add(30 * time.Second)
	for l.recordedSpeed(t) < 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	recorded := l.recordedSpeed(t)
	if recorded < 0 {
		t.Fatal("the worker recorded no speed for the job")
	}
	var before float64
	for replica := range 2 {
		info := l.e.transcodingInfo(t, replica)
		if info == nil {
			t.Fatalf("replica %d: no TranscodingInfo for a paused job", replica)
		}
		speed, ok := info["EncodeSpeed"].(float64)
		if !ok || math.Abs(speed-recorded) > 1e-9*max(speed, recorded) {
			t.Errorf("replica %d shows EncodeSpeed %v, the worker recorded %v", replica, info["EncodeSpeed"], recorded)
		}
		if fps, _ := info["Framerate"].(float64); math.Abs(fps-recorded*24) > 0.51 {
			t.Errorf("replica %d: Framerate %v does not follow the recorded speed %v of a 24 fps film", replica, info["Framerate"], recorded)
		}
		// The worker holds the speed it had, so the flag follows it; whether that
		// speed is above 1 is the machine's, not the code's, to say.
		if flagged, _ := info["IsBelowRealTime"].(bool); flagged != (speed < 1) {
			t.Errorf("replica %d: IsBelowRealTime %v for the held EncodeSpeed %v: %v", replica, flagged, speed, info)
		}
		before = speed
	}
	time.Sleep(12 * time.Second) // Paused for as long as the viewer takes.
	after := l.e.transcodingInfo(t, 1)
	if after == nil {
		t.Fatal("no TranscodingInfo for a paused job")
	}
	if speed, ok := after["EncodeSpeed"].(float64); !ok || speed < before {
		t.Errorf("EncodeSpeed %v after the pause, %v when the job paused: pausing counted as slow encoding", after["EncodeSpeed"], before)
	}
	if speed, _ := after["EncodeSpeed"].(float64); after["EncodeSpeed"] != nil {
		if flagged, _ := after["IsBelowRealTime"].(bool); flagged != (speed < 1) {
			t.Errorf("IsBelowRealTime %v for the held EncodeSpeed %v after the pause: %v", flagged, speed, after)
		}
	}
}
