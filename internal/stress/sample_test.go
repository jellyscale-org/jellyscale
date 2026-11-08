package stress

import (
	"strings"
	"testing"
)

// A step whose Postgres samples all failed must not read as an idle database.
func TestAnUnmeasuredStepSaysSo(t *testing.T) {
	got := Format([]Step{{Viewers: 8, Workers: 2, PostgresCPU: -1, Connections: 0}})
	if !strings.Contains(got, "cpu=n/a conns=n/a") {
		t.Fatalf("an unmeasured step printed as measured:\n%s", got)
	}
	got = Format([]Step{{Viewers: 8, Workers: 2, PostgresCPU: 0, Connections: 15}})
	if !strings.Contains(got, "cpu=0% conns=15") {
		t.Fatalf("a measured idle step:\n%s", got)
	}
}
