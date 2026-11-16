package transcode

import (
	"testing"
	"time"
)

// The cut is the table's cutMargin before the lease can lapse (DOMAINS 25), and
// the lease's end when the lease is no longer than the margin.
func TestACutIsTheCutMarginBeforeTheLeaseCanLapse(t *testing.T) {
	for _, c := range []struct{ lease, want time.Duration }{
		{0, 9500 * time.Millisecond}, // The default lease, 10 s.
		{3 * time.Second, 2500 * time.Millisecond},
		{400 * time.Millisecond, 400 * time.Millisecond},
		{500 * time.Millisecond, 500 * time.Millisecond},
	} {
		k := &leaseKeeper{w: &Worker{Lease: c.lease}}
		if got := k.cutAfter(); got != c.want {
			t.Errorf("a lease of %v is cut after %v, want %v", c.lease, got, c.want)
		}
	}
}
