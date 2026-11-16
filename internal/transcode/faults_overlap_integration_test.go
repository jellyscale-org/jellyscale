//go:build integration

package transcode

import (
	"regexp"
	"testing"
	"time"
)

// Two restarts of Postgres that overlap are one outage, which ends when the last of them ends. The
// seeds that failed the queue (BL-137, and the run of 15139295734148324583) planned these two
// shapes: a short restart that ends inside a long one, and a long one begun inside a short one.
// A harness that brought Postgres back when the first of them ended judged the long restart for an
// outage that was never longer than a lease, and the job rightly went on.
func TestRestartsOfPostgresThatOverlapAreOneOutage(t *testing.T) {
	plan := faultSchedule{Window: 100 * time.Second, Steps: []faultStep{
		{Kind: faultPGShort, At: 12 * time.Second, Length: 10600 * time.Millisecond},
		{Kind: faultPGLong, At: 15600 * time.Millisecond, Length: 22300 * time.Millisecond},
		{Kind: faultPGLong, At: 70 * time.Second, Length: 23100 * time.Millisecond},
		{Kind: faultPGShort, At: 72200 * time.Millisecond, Length: 9600 * time.Millisecond},
	}}
	c := runFaultSchedule(t, 0, 0, plan)
	if n := c.judged["restart-long"]; n < 2 {
		t.Errorf("the long restarts judged %d jobs, want one for each", n)
	}
	// Each worker was away from Postgres for the whole of each outage, not for the short restart only.
	away := regexp.MustCompile(`Postgres is back.*"was away"=(\S+)`)
	for _, w := range c.workers {
		long := 0
		for _, m := range away.FindAllStringSubmatch(w.log.String(), -1) {
			if d, err := time.ParseDuration(m[1]); err == nil && !earlier(d, 20*time.Second) {
				long++
			}
		}
		if long != 2 {
			t.Errorf("worker %s logged %d outages of a lease or more, want 2 (one for each pair of restarts):\n%s", w.id, long, w.log.String())
		}
	}
}
