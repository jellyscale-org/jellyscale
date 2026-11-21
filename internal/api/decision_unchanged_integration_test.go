//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"jellyscale/internal/db"
)

// JS-153: no viewer-visible change. For the same request the reply (but for its random PlaySessionId), the TranscodingUrl
// with its TranscodeReasons, and the session's TranscodingInfo are what they were: testdata/playbackinfo-before-js153.txt
// was recorded from the build before the decision line (JS153_RECORD_GOLDEN=1 rewrites it). The cases are the line's own
// (decisionCases), among them the track pick and the film with no keyframe index whose reasons only the line and the counter
// hold, so a reason that leaks into TranscodeReasons or the reply fails here.
//
// It passes on the build before the change, by construction: it is the baseline the change must keep.
var randomIDs = regexp.MustCompile(`(PlaySessionId["=: ]+)[0-9a-f-]{16,}`)

func TestTheReplyTheTranscodingUrlAndTheSessionAreWhatTheyWere(t *testing.T) {
	var out []string
	one := func(name string, mk func(t *testing.T) (infoRequest, storedFilm), setup func(e *env)) {
		req, f := mk(t)
		if f.bitrate == -1 {
			f.bitrate = 0
		}
		e, _, log := decisionEnv(t, f)
		if f.bitrate == 0 && strings.Contains(name, "unknown bitrate") {
			mustExec(t, e.pool, `UPDATE items SET dto = dto #- '{MediaSources,0,Bitrate}'`)
		}
		if req.body != nil {
			req.body["MediaSourceId"] = db.CompactID(e.item)
		}
		reply, _, status := e.decide(t, log, req)
		if status != 200 {
			t.Fatalf("%s: %d", name, status)
		}
		raw, _ := json.MarshalIndent(reply, "", " ")
		text := strings.NewReplacer(e.token, "<token>", e.item, "<item>", db.CompactID(e.item), "<item>", e.user, "<user>", db.CompactID(e.user), "<user>").Replace(string(raw))
		text = randomIDs.ReplaceAllString(text, "${1}<id>")
		text = regexp.MustCompile(`PlaySessionId=[0-9a-f]+`).ReplaceAllString(text, "PlaySessionId=<id>")
		out = append(out, "## "+name, text)
		if sources, _ := reply["MediaSources"].([]any); len(sources) > 0 {
			if _, offered := sources[0].(map[string]any)["TranscodingUrl"]; offered {
				e.play(t, reply, "Transcode")
				info, _ := json.MarshalIndent(e.transcodingInfo(t, 0), "", " ")
				out = append(out, "TranscodingInfo "+string(info))
			}
		}
	}
	for _, c := range decisionCases {
		one(c.name, func(t *testing.T) (infoRequest, storedFilm) { return infoRequest{body: c.body(t)}, c.film }, nil)
	}
	one("the GET route", func(t *testing.T) (infoRequest, storedFilm) { return infoRequest{route: "get"}, storedFilm{} }, nil)
	one("the jellyscale route", func(t *testing.T) (infoRequest, storedFilm) {
		return infoRequest{route: "marked", body: chrome(t)}, storedFilm{}
	}, nil)
	text := strings.Join(out, "\n") + "\n"
	golden := filepath.Join("testdata", "playbackinfo-before-js153.txt")
	if os.Getenv("JS153_RECORD_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) == text {
		return
	}
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(text, "\n")
	for i := 0; i < len(wantLines) && i < len(gotLines); i++ {
		if wantLines[i] != gotLines[i] {
			t.Fatalf("the answers changed at line %d:\n was %s\n now %s", i+1, wantLines[i], gotLines[i])
		}
	}
	t.Fatalf("the answers changed in length: %d lines, were %d", len(gotLines), len(wantLines))
}

// Logging adds no read of a source's row to a request: whatever the line holds (the track pick, the keyframe index, the
// resolved subtitle) comes from the row the decision read already. Every case of the line, and every refusal, reads a source's
// row at most once, as the JS-111 checks (playbackinfo_reads_integration_test.go) hold the decision to. It passes before the
// change, by construction.
func TestLoggingAddsNoReadOfASourcesRow(t *testing.T) {
	run := func(t *testing.T, e *env, body map[string]any) {
		log := traceReplicas(t, e)[0]
		log.take()
		e.playbackInfo(t, 0, e.item, body)
		statements := log.take()
		if n := count(statements, namesSource); n > 1 {
			t.Errorf("%d statements name media_sources, want at most 1:\n%s", n, listing(statements))
		}
	}
	for _, c := range decisionCases {
		t.Run(c.name, func(t *testing.T) {
			e, source, _ := decisionEnv(t, c.film)
			body := c.body(t)
			body["MediaSourceId"] = source
			run(t, e, body)
		})
	}
	for _, r := range refusalCases() {
		t.Run(r.name, func(t *testing.T) {
			e, source, _ := decisionEnv(t, r.film())
			r.setup(t, e, func() map[string]any { return r.body(t, source) })
			run(t, e, r.body(t, source))
		})
	}
}
