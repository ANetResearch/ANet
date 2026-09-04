package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The property AllocControlPort cannot have: what it hands back is still
// held when it returns. AllocControlPort binds to test a port and closes
// immediately, so two daemons scanning at the same moment both get the
// same answer, both write it into their own config, and the one that
// binds second dies with "address already in use". Found by
// scripts/joint.sh, which starts two daemons in the same instant.
func TestAllocControlListenerHoldsWhatItHandsBack(t *testing.T) {
	first, p1, err := AllocControlListener()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, p2, err := AllocControlListener()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if p1 == p2 {
		t.Fatalf("two allocations returned the same port %d while the first was still open", p1)
	}
	// And what it returned is genuinely bound, not merely reserved.
	if _, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p1))); err == nil {
		t.Errorf("port %d was still bindable, so the listener is not holding it", p1)
	}
}

// Moving a port is only ever right for one this daemon chose. A port an
// operator wrote into config.json is a decision with other things
// pointing at it, so a collision there has to be reported, not worked
// around.
func TestOnlyAnAutoAssignedControlAddressCountsAsMovable(t *testing.T) {
	for _, tc := range []struct {
		addr string
		auto bool
	}{
		{"127.0.0.1:39811", true},  // the base of the scan range
		{"127.0.0.1:40000", true},  // inside it
		{"127.0.0.1:41810", true},  // last port in range
		{"127.0.0.1:41811", false}, // one past the end
		{"127.0.0.1:8080", false},  // below the range: an operator's choice
		{"0.0.0.0:39811", false},   // not loopback: deliberately exposed
		{"192.168.1.5:39900", false},
		{"garbage", false},
		{"", false},
	} {
		if got := autoAssignedControlAddr(tc.addr); got != tc.auto {
			t.Errorf("autoAssignedControlAddr(%q) = %v, want %v", tc.addr, got, tc.auto)
		}
	}
}

// listenControl moves off a taken auto-assigned port and writes the new
// address to config.json before using it, so the CLI — which reads the
// file to find the daemon — follows it.
func TestAControlPortLostToAnotherDaemonIsRecoveredAndPersisted(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	// Somebody else already holds the port this node has configured.
	squatter, port, err := AllocControlListener()
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	taken := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	cfg := DefaultConfig()
	cfg.ControlAddr = taken
	if err := SaveConfig(l, cfg); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{layout: l, cfg: cfg}

	ln, err := d.listenControl()
	if err != nil {
		t.Fatalf("the daemon died on a port it had only auto-assigned itself: %v", err)
	}
	defer ln.Close()
	if got := ln.Addr().String(); got == taken {
		t.Fatalf("listener is on the taken address %s", got)
	}
	if d.config().ControlAddr == taken {
		t.Error("the in-memory config still names the address it could not bind")
	}
	// Persisted, or the CLI would keep dialling the old port.
	onDisk, err := LoadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.ControlAddr != d.config().ControlAddr {
		t.Errorf("config.json says %s but the daemon is on %s",
			onDisk.ControlAddr, d.config().ControlAddr)
	}
	if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
		t.Errorf("config.json missing: %v", err)
	}
}

// The other direction: a port the operator pinned is not silently moved.
func TestAPinnedControlPortIsNotSilentlyMoved(t *testing.T) {
	root := t.TempDir()
	l := Layout{Root: root}

	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squatter.Close()
	pinned := squatter.Addr().String() // an ephemeral port, far outside the scan range

	cfg := DefaultConfig()
	cfg.ControlAddr = pinned
	if err := SaveConfig(l, cfg); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{layout: l, cfg: cfg}

	ln, err := d.listenControl()
	if err == nil {
		ln.Close()
		t.Fatal("a pinned control port that was taken was silently moved instead of reported")
	}
	if got := d.config().ControlAddr; got != pinned {
		t.Errorf("the pinned address was rewritten to %s", got)
	}
}
