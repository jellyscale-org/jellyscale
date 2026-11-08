package differential

// Timestamps are masked only under the keys whose value changes from run to
// run, listed in one place and documented in docs/testing.md. They reference:
//
//	MaskedTimeKeys map[string]bool   the keys whose timestamp value Normalize masks

import (
	"os"
	"strings"
	"testing"
)

// The timestamps the two servers returned for the synthetic fixtures when JS-069 was
// written (every request of requests.yaml, Jellyfin 12.4 and jellyscale); none other
// appears. Neither server returns a PremiereDate for them.
var observedTimeKeys = []string{"DateCreated", "DateLastMediaAdded", "LastPlayedDate", "LastActivityDate", "LastLoginDate"}

func TestEveryTimestampKeyTheServersReturnIsMasked(t *testing.T) {
	for _, key := range observedTimeKeys {
		if !MaskedTimeKeys[key] {
			t.Errorf("%s changes from run to run and is not in MaskedTimeKeys", key)
		}
		jf := parse(t, `{"`+key+`":"2026-11-07T16:15:41.8158016Z"}`)
		js := parse(t, `{"`+key+`":"2026-11-08T00:58:52.684480066Z"}`)
		if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 0 {
			t.Errorf("%s: two run times still differ: %v", key, paths(d))
		}
	}
}

func TestMaskedTimeKeysAreMaskedWhereverTheySit(t *testing.T) {
	jf := parse(t, `{"Items":[{"UserData":{"LastPlayedDate":"2026-11-07T16:15:41Z"},"DateCreated":"2026-11-07T16:15:41Z"}]}`)
	js := parse(t, `{"Items":[{"UserData":{"LastPlayedDate":"2020-01-01T00:00:00.5+01:00"},"DateCreated":"2020-01-01T00:00:00Z"}]}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 0 {
		t.Fatalf("nested run times differ: %v", paths(d))
	}
}

func TestAMaskedTimeKeyOneServerLeavesOutStillDiffers(t *testing.T) {
	jf := parse(t, `{"DateCreated":"2026-11-07T16:15:41Z"}`)
	js := parse(t, `{}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 1 {
		t.Fatalf("want one difference for the missing key, got %v", paths(d))
	}
}

func TestAWrongStableDateIsADifference(t *testing.T) {
	for _, key := range []string{"PremiereDate", "EndDate", "DateModified", "Note"} {
		if MaskedTimeKeys[key] {
			t.Errorf("%s is masked; it does not change from run to run", key)
		}
		jf := parse(t, `{"`+key+`":"2021-01-01T00:00:00.0000000Z"}`)
		js := parse(t, `{"`+key+`":"2021-01-02T00:00:00.0000000Z"}`)
		d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false))
		if got := paths(d); len(got) != 1 || got[0] != key {
			t.Errorf("%s: a day's shift gave %v", key, got)
		}
	}
}

func TestATimestampInsideAnOtherwiseUnmaskedStringIsCompared(t *testing.T) {
	jf := parse(t, `{"Overview":"aired 2021-01-01T00:00:00Z"}`)
	js := parse(t, `{"Overview":"aired 2021-01-02T00:00:00Z"}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 1 {
		t.Fatalf("a timestamp in free text was masked: %v", paths(d))
	}
}

func TestDocsListEveryMaskedTimeKey(t *testing.T) {
	doc, err := os.ReadFile("../../docs/testing.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(MaskedTimeKeys) == 0 {
		t.Fatal("MaskedTimeKeys is empty")
	}
	for key := range MaskedTimeKeys {
		if !strings.Contains(string(doc), "`"+key+"`") {
			t.Errorf("docs/testing.md does not list the masked timestamp key `%s`", key)
		}
	}
}
