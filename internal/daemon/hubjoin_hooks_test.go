package daemon

import (
	"context"
	"fmt"
	"sync"
)

// fakeJoinHooks stands in for the identity area's JoinHooks (KEL-9) in
// tests (LEC AD-4). It keeps what KEL-9 keeps on disk: a rot built for a
// hub and a key event (pending.event) survives a "restart" of the join loop
// and is reused, and Adopt makes it the accepted tip. It counts what a test
// asserts on: the rots it built per (hub AID, tip seq), the Prepare calls
// and the answers adopted.
type fakeJoinHooks struct {
	mu sync.Mutex
	// kel is the KEL Prepare returns: the node's own unless a test sets
	// another.
	kel []byte
	// tip is the seq of the identity's accepted tip; a rot built for it
	// has seq tip+1, and Adopt moves the tip there.
	tip uint64
	// pending is the rot built and saved per (hub AID, tip seq).
	pending map[string][]byte
	// built counts the rots built per (hub AID, tip seq): more than one is
	// a second rot for the same key event (P-JOIN (b)).
	built map[string]int
	// calls lists the hub AIDs Prepare was called for, in order, and
	// adopted the answers Adopt was given.
	calls   []string
	adopted []RegisterResult
	// prepareErr and adoptErr, when set, are returned instead.
	prepareErr, adoptErr error
}

func newFakeJoinHooks(kel []byte) *fakeJoinHooks {
	return &fakeJoinHooks{kel: kel, pending: map[string][]byte{}, built: map[string]int{}}
}

func (f *fakeJoinHooks) key(hubAID string) string { return fmt.Sprintf("%s|%d", hubAID, f.tip) }

func (f *fakeJoinHooks) Prepare(_ context.Context, hubAID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hubAID)
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	k := f.key(hubAID)
	if kel, ok := f.pending[k]; ok {
		return kel, nil
	}
	f.built[k]++
	f.pending[k] = append([]byte(nil), f.kel...)
	return f.pending[k], nil
}

func (f *fakeJoinHooks) Adopt(_ context.Context, resp RegisterResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.adoptErr != nil {
		return f.adoptErr
	}
	f.adopted = append(f.adopted, resp)
	if _, ok := f.pending[f.key(resp.HubAID)]; ok {
		delete(f.pending, f.key(resp.HubAID))
		f.tip++
	}
	return nil
}

// stats are the counts a test asserts on, read under the lock.
func (f *fakeJoinHooks) stats() (calls int, adopted int, maxBuilt int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.built {
		if n > maxBuilt {
			maxBuilt = n
		}
	}
	return len(f.calls), len(f.adopted), maxBuilt
}
