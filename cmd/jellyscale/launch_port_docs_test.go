package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-171: docs/testing.md records the two merge-queue runs whose causes the task names, with what each figure times.
func TestTestingDocRecordsTheTwoQueueRunsOfJS171(t *testing.T) {
	raw, err := os.ReadFile("../../docs/testing.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, want := range []struct{ what, text string }{
		{"the run of the lost port", "44983050791"},
		{"its test", "TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries"},
		{"its shard and go test process", "process 2 of 4"},
		{"its log line", "bind: address already in use"},
		{"its cause", "closes its listener before"},
		{"the run of the slow seek checks", "45008394219"},
		{"the lost-head check", "TestSeveralSeeksInOnePlayOfAFilmWhoseAudioHeadIsLostEachGetTheirSegment"},
		{"where it was cut", "354 of 453"},
		{"the store check", "TestASeekPastHalfIntoAFilmReadFromAStoreWithNoPathGetsItsSegment/truehd-6"},
		{"where it was cut", "218 of 229"},
		{"JS-152's own queue run", "44909282472"},
		{"the whole test's time there", "197.7"},
		{"the subtest's time there", "178.9"},
	} {
		if !strings.Contains(doc, want.text) {
			t.Errorf("docs/testing.md does not record %s (%q)", want.what, want.text)
		}
	}
	// What took the port is recorded only if it was shown; otherwise the record says it is unverified, next to the run.
	if i := strings.Index(doc, "44983050791"); i >= 0 {
		near := doc[i:min(len(doc), i+2500)]
		if !strings.Contains(near, "unverified") {
			t.Error("the record of run 44983050791 does not say that what took the port is unverified (or show it)")
		}
		if !strings.Contains(near, "18") {
			t.Error("the record of run 44983050791 does not say which of the 18 processes exited")
		}
	}
	if i := strings.Index(doc, "45008394219"); i >= 0 {
		if near := doc[i:min(len(doc), i+2500)]; !strings.Contains(near, "180 s") && !strings.Contains(near, "180 seconds") {
			t.Error("the record of run 45008394219 does not name the 180 s bound")
		}
	}
	if i := strings.Index(doc, "44909282472"); i >= 0 {
		near := doc[i:min(len(doc), i+1500)]
		if !strings.Contains(near, "whole test") || !strings.Contains(near, "subtest") {
			t.Error("the record of run 44909282472 does not say what each figure times (the whole test: 197.7 s, the truehd-6 subtest: 178.9 s)")
		}
	}
}

// JS-171: docs/testing.md states how many starts a launch makes of a process whose address is taken every time (the number the
// check in launch_port_integration_test.go bounds by 10), and records the measurements the criteria ask for, which no test makes.
func TestTestingDocStatesTheLaunchBoundAndRecordsTheMeasurements(t *testing.T) {
	raw, err := os.ReadFile("../../docs/testing.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if m := regexp.MustCompile(`(?i)at most (\d+) starts`).FindStringSubmatch(doc); m == nil {
		t.Error(`docs/testing.md does not state the most starts of a launch whose address is taken ("at most N starts")`)
	} else if n, _ := strconv.Atoi(m[1]); n < 2 || n > 10 {
		t.Errorf("docs/testing.md says a launch makes at most %d starts, want between 2 and 10", n)
	}
	for _, want := range []struct{ what, text string }{
		{"the 18-process measurement's shape", "INTEGRATION_PROCS=4"},
		{"its 50 runs", "50 runs"},
		{"how many launches met a taken port", "taken port"},
		{"the seek checks' 10 runs of 10", "10 runs of 10"},
		{"the step the 180 s bound times", "the run from the film's start"},
	} {
		if !strings.Contains(doc, want.text) {
			t.Errorf("docs/testing.md does not record %s (%q)", want.what, want.text)
		}
	}
}
