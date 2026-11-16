package differential

// JS-119: the requests of the Kotlin SDK's decoding include an opted-in viewer's PlaybackInfo and a film's details, each
// asking for a stream of Type Data in the answer (a decode of an answer without one shows nothing of placeholders).
//
// Names the task implies, built with the runner:
//
//	Request{OptedIn bool `yaml:"opted_in"`}    the viewer hides the tracks outside their languages for this request
//	Request{ExpectData bool `yaml:"expect_data"`} jellyscale's answer must hold a stream of Type Data
//	HoldsData(doc any) bool                       whether an answer lists a stream of Type Data

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTheKotlinRequestsDecodeAnOptedInViewersPlaybackInfoAndDetails(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var info, details bool
	for _, r := range reqs {
		if !r.OptedIn {
			continue
		}
		if !strings.Contains(r.Path, "{{Movie/Polyglot (2030)}}") || r.Kotlin == "" || !r.ExpectData || !r.DecodeOnly {
			t.Errorf("%s: an opted-in request names Babel Film, decodes with the Kotlin SDK, expects Type Data and is not compared with Jellyfin's answer: %+v", r.Name, r)
		}
		info = info || (r.Kotlin == "PlaybackInfoResponse" && r.Route == "POST /Items/{itemId}/PlaybackInfo" && strings.Contains(r.Body, `"TranscodingProfiles":[{`))
		details = details || (r.Kotlin == "BaseItemDto" && r.Route == "GET /Items/{itemId}")
	}
	if !info || !details {
		t.Errorf("opted-in requests decode PlaybackInfo (%v) and the film's details (%v); want both", info, details)
	}
}

func TestAnAnswerHoldsDataOnlyIfItListsAStreamOfThatType(t *testing.T) {
	parse := func(text string) any {
		var doc any
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	for _, c := range []struct {
		name, answer string
		want         bool
	}{
		{"a placeholder in a source", `{"MediaSources":[{"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Data"}]}]}`, true},
		{"a placeholder in an item's list", `{"MediaStreams":[{"Index":8,"Type":"Data"}]}`, true},
		{"no placeholder", `{"MediaSources":[{"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio"}]}]}`, false},
		{"Data as a word elsewhere", `{"Name":"Data","Type":"Movie"}`, false},
	} {
		if got := HoldsData(parse(c.answer)); got != c.want {
			t.Errorf("%s: HoldsData = %v, want %v", c.name, got, c.want)
		}
	}
}
