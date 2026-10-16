package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// putHeld stores h as a capability this node holds, with the fields every
// valid row has filled in.
func putHeld(t *testing.T, d *Daemon, h interactions.HeldCap) {
	t.Helper()
	now := d.nowMS()
	if h.Cap == nil {
		h.Cap, h.HolderSeed, h.Epoch, h.Home, h.ReceivedAt = []byte{0xa0}, bytes.Repeat([]byte{1}, 32), 1, "hub-aid-0003", now
	}
	if h.CapExp == 0 {
		h.CapExp = now + capLifeContactMS
	}
	invalid := h.InvalidAt
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if _, err := tx.PutHeldGrant(h); err != nil || invalid == 0 {
			return err
		}
		_, err := tx.MarkHeldInvalid(h.Issuer, h.Scope, h.IX, h.CapID, invalid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// setP2PLAN sets modules.p2p.lan in d's configuration as it runs: the test
// binary has no p2p module to start, so the daemon is built without the block.
func setP2PLAN(d *Daemon, lan bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg.Modules == nil {
		d.cfg.Modules = ModulesConfig{}
	}
	d.cfg.Modules["p2p"] = json.RawMessage(fmt.Sprintf(`{"socket":"/run/anet/peer.sock","lan":%v}`, lan))
}

// SSR-P2, engine ENG-I7: a peer is reachable over p2p only when this node
// holds a valid contact-class peer capability it gave, and the capability
// carries an address — never on a stranger capability, a one-shot one, one
// given up, expiring or under an older mailbox epoch, nor on one without
// an address. The address is the one the capability carries.
func TestReachableNeedsContactGrantWithAddr(t *testing.T) {
	d := newTestDaemon(t, "", false)
	const addr = "203.0.113.9:4001"
	id := byte(0)
	peer := func() string {
		id++
		return "bafyreipeer" + strings.Repeat(string('a'+rune(id)), 8)
	}
	capID := func() []byte { return bytes.Repeat([]byte{id}, seal.CapIDLen) }
	now := d.nowMS()

	cases := []struct {
		name string
		h    func(aid string) interactions.HeldCap
		want bool
	}{
		{"a contact peer capability with an address", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: capID(), Class: seal.CapClassContact, P2P: addr}
		}, true},
		{"without an address", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: capID(), Class: seal.CapClassContact}
		}, false},
		{"a stranger peer capability", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: capID(), Class: seal.CapClassStranger, P2P: addr}
		}, false},
		{"a one-shot capability", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "ix", IX: "ix-1", CapID: capID(), Class: seal.CapClassContact, P2P: addr}
		}, false},
		{"given up", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: capID(), Class: seal.CapClassContact, P2P: addr, InvalidAt: now - 1}
		}, false},
		{"about to expire", func(aid string) interactions.HeldCap {
			return interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: capID(), Class: seal.CapClassContact, P2P: addr,
				CapExp: now + capValidMarginMS/2}
		}, false},
	}
	for _, c := range cases {
		aid := peer()
		h := c.h(aid)
		if h.Scope == "ix" {
			// A one-shot row may not carry an address (core §2.3); stored
			// without one, as caps_held would hold it.
			h.P2P = ""
		}
		allowPeers(t, d, aid)
		putHeld(t, d, h)
		got, ok := p2pHost(d).P2PAddr(aid)
		if ok != c.want || (ok && got != addr) {
			t.Errorf("%s: P2PAddr = %q, %v; want %v", c.name, got, ok, c.want)
		}
	}
	// Nobody else's capability answers for an AID, and this node is not
	// reachable over p2p by itself.
	if _, ok := p2pHost(d).P2PAddr("bafyreinobody"); ok {
		t.Error("an AID that gave this node nothing has an address")
	}
	if _, ok := p2pHost(d).P2PAddr(d.AID()); ok {
		t.Error("this node has a direct address for itself")
	}
}

// SSR-P2: before an address leaves the daemon for the peer process, a
// literal IP in a refused range and the name localhost are left out (and
// counted p2p-addr-refused); a name is the peer process's to resolve and
// check. modules.p2p.lan lets the private ranges through, nothing else.
func TestP2PAddrLiteralPrecheck(t *testing.T) {
	cases := []struct {
		addr     string
		off, lan bool // P2PAddr's answer with LAN mode off, on
	}{
		{"203.0.113.9:4001", true, true},
		{"[2001:db8::1]:4001", true, true},
		{"peer.example.org:4001", true, true},
		{"10.1.2.3:4001", false, true},
		{"192.168.1.5:4001", false, true},
		{"172.16.0.1:4001", false, true},
		{"100.64.0.1:4001", false, true},
		{"[fd00::1]:4001", false, true},
		{"127.0.0.1:4001", false, false},
		{"[::1]:4001", false, false},
		{"[::ffff:127.0.0.1]:4001", false, false},
		{"[::ffff:10.0.0.1]:4001", false, true},
		{"0.0.0.0:4001", false, false},
		{"0.1.2.3:4001", false, false},
		{"[::]:4001", false, false},
		{"169.254.1.1:4001", false, false},
		{"[fe80::1]:4001", false, false},
		{"224.0.0.1:4001", false, false},
		{"[ff02::1]:4001", false, false},
		{"localhost:4001", false, false},
		{"LocalHost.:4001", false, false},
		{"a.localhost:4001", false, false},
	}
	for _, lan := range []bool{false, true} {
		d := newTestDaemon(t, "", false)
		setP2PLAN(d, lan)
		if p2pLAN(d.config()) != lan {
			t.Fatalf("modules.p2p.lan %v read as %v", lan, !lan)
		}
		for i, c := range cases {
			aid := "bafyreipeer" + strings.Repeat("q", i+1)
			allowPeers(t, d, aid)
			putHeld(t, d, interactions.HeldCap{Issuer: aid, Scope: "peer", CapID: bytes.Repeat([]byte{byte(i + 1)}, seal.CapIDLen),
				Class: seal.CapClassContact, P2P: c.addr})
			want := c.off
			if lan {
				want = c.lan
			}
			before := d.ReceiveStats()[cntP2PAddrRefused]
			got, ok := p2pHost(d).P2PAddr(aid)
			if ok != want || (ok && got != c.addr) {
				t.Errorf("lan=%v %s: P2PAddr = %q, %v; want %v", lan, c.addr, got, ok, want)
			}
			if counted := d.ReceiveStats()[cntP2PAddrRefused] - before; counted != map[bool]uint64{true: 0, false: 1}[want] {
				t.Errorf("lan=%v %s: counted %d refusals", lan, c.addr, counted)
			}
		}
	}
}

// modules.p2p is checked before the daemon starts: a relative socket (which
// the daemon would dial from wherever it was started, and fail at every
// retry), a lan that is not a bool and an advertise no contact could dial
// are config_invalid naming the key.
func TestP2PConfigIsCheckedAtStart(t *testing.T) {
	for _, tc := range []struct {
		block map[string]any
		key   string // "" for a block that passes
	}{
		{map[string]any{"socket": "/run/anet/peer.sock"}, ""},
		{map[string]any{"socket": "/run/anet/peer.sock", "lan": true, "advertise": "peer.example.org:39100"}, ""},
		{map[string]any{"socket": "/run/anet/peer.sock", "advertise": "tcp://203.0.113.9:39100"}, ""},
		{map[string]any{"socket": "run/peer.sock"}, "modules.p2p.socket"},
		{map[string]any{"socket": "peer.sock"}, "modules.p2p.socket"},
		{map[string]any{"socket": 7}, "modules.p2p.socket"},
		{map[string]any{"socket": "/run/anet/peer.sock", "lan": "yes"}, "modules.p2p.lan"},
		{map[string]any{"socket": "/run/anet/peer.sock", "advertise": "/run/anet/wire.sock"}, "modules.p2p.advertise"},
		{map[string]any{"socket": "/run/anet/peer.sock", "advertise": "peer.example.org"}, "modules.p2p.advertise"},
		{map[string]any{"socket": "/run/anet/peer.sock", "advertise": "peer.example.org:0"}, "modules.p2p.advertise"},
	} {
		root := t.TempDir()
		b, _ := json.Marshal(map[string]any{"modules": map[string]any{"p2p": tc.block}})
		if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		err := CheckConfigForStart(NewLayout(root))
		cr, _ := err.(*ConfigRefusal)
		switch {
		case tc.key == "" && err != nil:
			t.Errorf("%v: refused: %v", tc.block, err)
		case tc.key != "" && (cr == nil || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != "config_invalid" ||
			cr.Refusals[0].Args["key"] != tc.key):
			t.Errorf("%v: %v, want config_invalid for %s", tc.block, err, tc.key)
		}
	}
}
