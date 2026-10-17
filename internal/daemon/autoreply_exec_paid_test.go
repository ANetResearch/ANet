//go:build !no_x402

package daemon

// The exec auto-reply backend against a paid capability call: it needs the metered-work fixtures of the
// x402 task tests, so it is built with them.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Section 17 C6/C29: both sides run the exec auto-reply backend and trust each other; a quoted, paid,
// completed capability call never reaches it. A plain task on the same nodes does (the control).
func TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "agent"), []byte("#!/bin/sh\necho called >> "+calls+"\necho a reply\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := &meteredWork{price: 30}
	_, req, prov := paidPair(t, work)
	payPolicy(t, req, PaymentsConfig{AutoMax: 100, AgentDailyMax: 100, DailyMax: u64(100)}, prov.AID())
	trustPeers(t, prov, req.AID())
	trustPeers(t, req, prov.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "cursor"}
	for _, d := range []*Daemon{req, prov} {
		c := cfg
		if err := d.SetAutoReply(&c); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	id, err := req.DelegateCapability(ctx, prov.AID(), "work.do", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the paid call to complete", func() bool {
		poll(t, prov, req)
		return getIX(t, req, id).State == interactions.StateCompleted
	})
	req.kickTurns()
	prov.kickTurns()
	time.Sleep(300 * time.Millisecond)
	if work.invoked.Load() != 1 {
		t.Fatalf("the paid call ran %d times", work.invoked.Load())
	}
	if b, err := os.ReadFile(calls); err == nil && len(b) > 0 {
		t.Fatalf("the exec backend ran %d times during a paid call", strings.Count(string(b), "\n"))
	}
	// The control: the stub is the agent on these nodes, and a plain task from a trusted peer reaches it.
	plain, err := req.Delegate(ctx, prov.AID(), "a plain task", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the plain task to be answered", func() bool {
		poll(t, prov, req)
		return lastFromMe(t, prov, plain) == "a reply"
	})
	if b, err := os.ReadFile(calls); err != nil || len(b) == 0 {
		t.Fatalf("the exec backend did not run for a plain task: %v", err)
	}
}
