package differential

// What a request expects of one server's answer, whatever the other says. These
// reference the names JS-069 implies:
//
//	Request{Rows bool `yaml:"rows"`; Status int `yaml:"status"`}
//	    Rows: the answer must hold rows (a non-empty Items list, or a non-empty top-level array).
//	    Status: the status the request expects; 0 means any 2xx.
//	CheckAnswer(r Request, status int, doc any) error   nil, or why the answer is not what r expects

import "testing"

func TestAnswerOfAnErrorFailsEvenWhenNothingElseIsExpected(t *testing.T) {
	for _, status := range []int{301, 400, 401, 404, 500} {
		if err := CheckAnswer(Request{Name: "r"}, status, parse(t, `{}`)); err == nil {
			t.Errorf("status %d passed a request that expects 2xx", status)
		}
	}
	for _, status := range []int{200, 201, 204} {
		if err := CheckAnswer(Request{Name: "r"}, status, parse(t, `{}`)); err != nil {
			t.Errorf("status %d failed a request that expects 2xx: %v", status, err)
		}
	}
}

func TestARequestThatExpectsAStatusFailsOnAnyOther(t *testing.T) {
	r := Request{Name: "r", Status: 404}
	if err := CheckAnswer(r, 404, parse(t, `{}`)); err != nil {
		t.Errorf("the expected 404 failed: %v", err)
	}
	if err := CheckAnswer(r, 200, parse(t, `{}`)); err == nil {
		t.Error("a 200 passed a request that expects 404")
	}
	if err := CheckAnswer(r, 500, parse(t, `{}`)); err == nil {
		t.Error("a 500 passed a request that expects 404")
	}
}

func TestARequestThatExpectsRowsFailsOnAnEmptyList(t *testing.T) {
	r := Request{Name: "r", Rows: true}
	for name, doc := range map[string]string{
		"no items":          `{"Items":[],"TotalRecordCount":0}`,
		"null items":        `{"Items":null}`,
		"no Items key":      `{"TotalRecordCount":3}`,
		"empty array":       `[]`,
		"an empty playlist": `[]`,
	} {
		if err := CheckAnswer(r, 200, parse(t, doc)); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
	for name, doc := range map[string]string{
		"items":       `{"Items":[{"Id":"a"}]}`,
		"array":       `[{"Id":"a"}]`,
		"playlist":    `["#EXTM3U","#EXT-X-STREAM-INF"]`,
		"items+total": `{"Items":[{"Id":"a"}],"TotalRecordCount":0}`,
	} {
		if err := CheckAnswer(r, 200, parse(t, doc)); err != nil {
			t.Errorf("%s: failed: %v", name, err)
		}
	}
}

func TestARequestWithoutRowsMayAnswerAnEmptyList(t *testing.T) {
	if err := CheckAnswer(Request{Name: "r"}, 200, parse(t, `{"Items":[]}`)); err != nil {
		t.Fatalf("an empty list failed a request that does not expect rows: %v", err)
	}
}

func TestRowsAreNotCheckedOnAnErrorButTheErrorStillFails(t *testing.T) {
	if err := CheckAnswer(Request{Name: "r", Rows: true}, 500, parse(t, `{"Items":[{"Id":"a"}]}`)); err == nil {
		t.Fatal("a 500 with rows passed")
	}
}
