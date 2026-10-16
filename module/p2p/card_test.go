//go:build !no_p2p

package p2p

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/p2paddr"
	"github.com/ANetResearch/ANet/module"
)

// SSR-P1: the network card carries no direct interface, with an advertise
// address or without one. The card is public; the address goes only to
// contacts, in the peer capability they are given.
func TestCardHasNoP2PInterface(t *testing.T) {
	for _, adv := range []string{"", "203.0.113.7:4001", "tcp://peer.example:4001"} {
		m := &Module{cfg: Config{Socket: "/tmp/p.sock", Advertise: adv}}
		if got := m.CardInterfaces(module.CardContext{AID: "aid1", Skills: []string{"x"}}); len(got) != 0 {
			t.Errorf("advertise %q: the card carries %v", adv, got)
		}
		if ext := m.CardExtensions(module.CardContext{AID: "aid1"}); len(ext) != 0 {
			t.Errorf("advertise %q: p2p adds no extension, got %v", adv, ext)
		}
	}
}

func TestCheckAdvertise(t *testing.T) {
	CheckAdvertise := p2paddr.CheckAdvertise
	for _, ok := range []string{"", "tcp://peer.example:4001", "peer.example:4001", "[2001:db8::1]:4001",
		"127.0.0.1:4001", " peer.example:4001 "} {
		if err := CheckAdvertise(ok); err != nil {
			t.Errorf("CheckAdvertise(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"/run/a/wire.sock", "unix:///run/a.sock", "peer.example", ":4001", "tcp://",
		"tcp://peer.example:4001/path", "peer.example:http", "peer.example:0", "peer.example:70000",
		"peer.example:+80", string(make([]byte, 300)) + ":1"} {
		if err := CheckAdvertise(bad); err == nil {
			t.Errorf("CheckAdvertise(%q) took it", bad)
		}
	}
}

// A socket path in advertise, and a relative socket for the peer process,
// are refused at start rather than found out at every dial.
func TestTheModuleRefusesWhatCannotWorkAtStart(t *testing.T) {
	for _, cfg := range []string{
		`{"socket":"/tmp/p.sock","advertise":"/tmp/wire.sock"}`,
		`{"socket":"run/peer.sock"}`,
		`{"socket":"peer.sock","advertise":"peer.example:4001"}`,
	} {
		if _, err := module.BuildOne(name, []byte(cfg)); err == nil {
			t.Errorf("%s: built", cfg)
		}
	}
	if _, err := module.BuildOne(name, []byte(`{"socket":"/tmp/p.sock","advertise":"peer.example:4001","lan":true}`)); err != nil {
		t.Fatal(err)
	}
}
