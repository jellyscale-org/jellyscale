//go:build integration

// JS-037 (D-078): failed Quick Connect approvals are bounded per approving account, with the reservation JS-145 makes for
// a password: an approval reserves its failure before its code is looked at (BeginApproval, the account's id), and one
// that succeeds gives it back (Ticket.Release), so that approvals made at once cannot all be checked. This is the count
// alone; what an operator and a viewer see of it is checked with real api processes in cmd/jellyscale.
package signin_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/signin"
	"jellyscale/internal/testenv"
)

// Two hundred approvals by one account at once, through two pools as two replicas have, reserve ten and no more; the
// rest are told how long, up to 15 minutes. Another account is not touched, and reservations given back count nothing.
func TestTwoHundredApprovalsAtOnceReserveNoMoreThanTen(t *testing.T) {
	ctx := context.Background()
	var pools []*pgxpool.Pool
	for range 2 {
		pool, err := db.Open(ctx, testenv.DatabaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		pools = append(pools, pool)
	}
	clear(t, pools[0])
	t.Cleanup(func() { clear(t, pools[0]) })

	honest := uuid.NewString()
	for i := range 30 {
		ticket, refusal, err := signin.BeginApproval(ctx, pools[i%2], honest)
		if err != nil || refusal != nil {
			t.Fatalf("successful approval %d: %v %v, want it allowed", i+1, refusal, err)
		}
		if err := ticket.Release(ctx, pools[i%2]); err != nil {
			t.Fatal(err)
		}
	}

	guesser := uuid.NewString()
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, refused := 0, 0
	start := make(chan struct{})
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, refusal, err := signin.BeginApproval(ctx, pools[i%2], guesser)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Error(err)
			case refusal == nil:
				allowed++
			default:
				refused++
				if refusal.Bound == "" || refusal.After <= 14*time.Minute || refusal.After > 15*time.Minute {
					t.Errorf("a refusal of bound %q for %v, want a bound and about 15 minutes", refusal.Bound, refusal.After)
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if allowed != 10 || refused != 190 {
		t.Errorf("200 approvals at once: %d allowed and %d refused, want 10 and 190", allowed, refused)
	}
	if _, refusal, err := signin.BeginApproval(ctx, pools[0], honest); err != nil || refusal != nil {
		t.Errorf("another account while one is refused: %v %v, want it allowed", refusal, err)
	}
}
