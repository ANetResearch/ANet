//go:build !no_x402

package x402

import (
	"context"
	"sync"
	"testing"
)

// Many reads at once, in one process: the module's requests and the node's
// share one clock per hub, and the hub accepts a signing time once. No
// request may be refused as replayed (RC1, PAY-R10); run with -race.
func TestConcurrentSignedReadsNeverReplay(t *testing.T) {
	f := newFakeHub(t)
	f.balance = 5
	m, h := f.hubbed()
	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			var bal struct {
				Credits int64 `json:"credits"`
			}
			errs <- m.signedGet(context.Background(), "/agents/"+m.AID()+"/balance", "balance", &bal)
		}()
		go func() {
			defer wg.Done()
			// The node's own requests take their times from the same clock.
			errs <- h.clockTick()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.replays != 0 || f.unsigned != 0 {
		t.Errorf("replayed %d, unsigned %d", f.replays, f.unsigned)
	}
}
