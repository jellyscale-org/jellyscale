package differential

import "testing"

// A masked time key hides when a value was made, never whether it is a real
// time: an empty, zero or garbage date from jellyscale still differs.
func TestAMaskedTimeKeyStillComparesValuesThatAreNotRealTimes(t *testing.T) {
	jf := map[string]any{"DateCreated": "2026-11-07T16:15:41.1234567Z"}
	for _, bad := range []string{"", "garbage", "0001-01-01T00:00:00Z"} {
		js := map[string]any{"DateCreated": bad}
		if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) == 0 {
			t.Errorf("DateCreated %q passed as a real time", bad)
		}
	}
	js := map[string]any{"DateCreated": "2025-01-02T03:04:05Z"}
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 0 {
		t.Errorf("two real times differ after masking: %v", d)
	}
}

// A request that failed (an error, no rows) was never filtered, so its entries
// must not be reported as no longer needed: following that note would delete them.
func TestEntriesOfAFailedRequestAreNotReportedUnused(t *testing.T) {
	a, err := ParseAllowlist("- {requests: [search], path: 'Items[].Name', kind: gap, task: JS-068, reason: r}\n")
	if err != nil {
		t.Fatal(err)
	}
	a.Failed("search")
	if got := a.Unused(); len(got) != 0 {
		t.Fatalf("the entry of a failed request was reported unused: %+v", got)
	}
}
