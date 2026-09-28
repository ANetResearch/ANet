package daemon

// Red-team finding F9: SetInboundPolicy and SetPublicCapabilities put the
// change in force before saving it and kept it when the save failed. The
// CLI reported the change as failed, config.json and `anet doctor` said
// closed, and the running daemon accepted strangers' tasks under open (and
// the next config write of any kind would have saved it). Policy writes are
// now saved first and put in force after.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// unwritable makes d's data directory read-only for the duration of write,
// so that saving config.json fails. It reports false when the save could
// not be made to fail (running as root).
func unwritable(t *testing.T, d *Daemon, write func() error) (error, bool) {
	t.Helper()
	if err := os.Chmod(d.layout.Root, 0o500); err != nil {
		t.Fatal(err)
	}
	err := write()
	if cerr := os.Chmod(d.layout.Root, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	return err, err != nil
}

func TestAPolicyWriteThatFailedToSaveIsNotInForce(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")

	err, failed := unwritable(t, prov, func() error { return prov.SetInboundPolicy(PolicyOpen) })
	if !failed {
		t.Skip("the save did not fail (running as root?)")
	}
	if p := prov.config().inbound().Policy; p != PolicyClosed {
		t.Fatalf("SetInboundPolicy returned %v and the running policy is %s", err, p)
	}
	st, rerr := ReadPolicy(prov.layout)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if v, _ := st.SI5(); v["inbound.policy"] != PolicyClosed {
		t.Fatalf("doctor view %v", v["inbound.policy"])
	}
	// What the daemon does agrees with what it says: a stranger is still
	// refused.
	id, derr := stranger.Delegate(ctx, prov.AID(), "task from a stranger", nil)
	if derr != nil {
		t.Fatal(derr)
	}
	pollEach(t, prov)
	if ix, gerr := prov.ix.Get(id); gerr == nil {
		t.Fatalf("a stranger's task was accepted (trust %s) after the open write failed", ix.Trust)
	}

	// A later write that saves does not carry the failed one to disk.
	if err := prov.SetPublicCapabilities(nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadPolicy(prov.layout); st.Inbound.Policy != PolicyClosed {
		t.Fatalf("the failed open write reached disk with a later save: %s", st.Inbound.Policy)
	}

	// Saved, the same write is in force.
	if err := prov.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	id, derr = stranger.Delegate(ctx, prov.AID(), "task from a stranger", nil)
	if derr != nil {
		t.Fatal(derr)
	}
	pollEach(t, prov)
	if ix, gerr := prov.ix.Get(id); gerr != nil || ix.Trust != interactions.TrustPublic {
		t.Fatalf("after a saved open write: %+v %v", ix, gerr)
	}
}

func TestPublicCapabilitiesAndAutoReplyWritesThatFailedToSaveAreNotInForce(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")

	_, failed := unwritable(t, prov, func() error {
		return prov.SetPublicCapabilities([]PublicCapability{{ID: "text.stats"}})
	})
	if !failed {
		t.Skip("the save did not fail (running as root?)")
	}
	if caps := prov.config().inbound().PublicCapabilities; len(caps) != 0 {
		t.Fatalf("public capabilities in force after a failed save: %+v", caps)
	}

	_, failed = unwritable(t, prov, func() error {
		return prov.SetAutoReply(&AutoReplyConfig{Backend: "exec", Agent: "cursor"})
	})
	if !failed {
		t.Fatal("the auto-reply save did not fail")
	}
	if ar := prov.config().AutoReply; ar != nil {
		t.Fatalf("auto-reply in force after a failed save: %+v", ar)
	}
}

// Two config writes at once (redteam F9, on review). Each saved a copy of
// the config it read before the other took effect, into one shared temp
// file: config.json could end up a mix of the two that no longer parses
// (the daemon then refuses to start), or name the policy that was not in
// force — a node running open while config.json and doctor say closed,
// with no failed save at all. Every config write is serialized, saved
// first and put in force after, and each has a temp file of its own.
func TestConcurrentConfigWritesLeaveDiskAndMemoryAgreeing(t *testing.T) {
	srv := newFakeHub(t)
	prov := registered(t, srv.URL, "prov")
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		policy, summary := PolicyOpen, fmt.Sprintf("summary %d", i)
		if i%2 == 1 {
			policy = PolicyClosed
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() { defer wg.Done(); errs[0] = prov.SetInboundPolicy(policy) }()
		go func() { defer wg.Done(); errs[1] = prov.SetProfile(ctx, Profile{Summary: summary}) }()
		go func() {
			defer wg.Done()
			errs[2] = prov.SetPublicCapabilities([]PublicCapability{{ID: fmt.Sprintf("cap.%d", i)}})
		}()
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		disk, err := LoadConfig(prov.layout)
		if err != nil {
			t.Fatalf("round %d: config.json after two writes at once: %v", i, err)
		}
		live := prov.config()
		if disk.inbound().Policy != live.inbound().Policy || disk.Summary != live.Summary ||
			len(disk.inbound().PublicCapabilities) != 1 ||
			disk.inbound().PublicCapabilities[0].ID != live.inbound().PublicCapabilities[0].ID {
			t.Fatalf("round %d: disk says policy=%s summary=%q caps=%v, the daemon runs policy=%s summary=%q caps=%v",
				i, disk.inbound().Policy, disk.Summary, disk.inbound().PublicCapabilities,
				live.inbound().Policy, live.Summary, live.inbound().PublicCapabilities)
		}
		if live.inbound().Policy != policy || live.Summary != summary {
			t.Fatalf("round %d: a write was lost: policy=%s summary=%q", i, live.inbound().Policy, live.Summary)
		}
	}
	left, _ := filepath.Glob(filepath.Join(prov.layout.Root, "config.json.tmp*"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
