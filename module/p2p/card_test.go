//go:build !no_p2p

package p2p

import (
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

func TestTheDirectInterfaceIsTheAdvertisedAddressUnderTheNodesAID(t *testing.T) {
	m := &Module{cfg: Config{Socket: "/tmp/p.sock", Advertise: "203.0.113.7:4001"}}
	got := m.CardInterfaces(module.CardContext{AID: "aid1", Skills: []string{"x"}})
	want := []map[string]any{{
		"url": "tcp://203.0.113.7:4001", "protocolBinding": module.BindingP2PURI,
		"protocolVersion": "1.0", "tenant": "aid1",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ext := m.CardExtensions(module.CardContext{AID: "aid1"}); ext != nil {
		t.Fatalf("p2p adds no extension, got %v", ext)
	}
}

// Without an advertise address there is nothing a remote reader could
// dial, so the card has no direct entry.
func TestNoAdvertiseMeansNoDirectInterface(t *testing.T) {
	m := &Module{cfg: Config{Socket: "/tmp/p.sock"}}
	if got := m.CardInterfaces(module.CardContext{AID: "aid1", Skills: []string{"x"}}); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestDirectURL(t *testing.T) {
	for in, want := range map[string]string{
		"tcp://peer.example:4001": "tcp://peer.example:4001",
		"peer.example:4001":       "tcp://peer.example:4001",
		"[2001:db8::1]:4001":      "tcp://[2001:db8::1]:4001",
		// Accepted here; the kernel keeps loopback off the network card.
		"127.0.0.1:4001": "tcp://127.0.0.1:4001",
	} {
		got, err := directURL(in)
		if err != nil || got != want {
			t.Errorf("directURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/run/a/wire.sock", "unix:///run/a.sock", "peer.example", ":4001", "tcp://",
		"tcp://peer.example:4001/path", "peer.example:http", "peer.example:0", "peer.example:70000"} {
		if got, err := directURL(bad); err == nil {
			t.Errorf("directURL(%q) = %q, want an error", bad, got)
		}
	}
}

// A socket path in advertise is refused at start rather than silently
// missing from the card.
func TestAnAdvertisedSocketIsRefusedAtStart(t *testing.T) {
	if _, err := module.BuildOne(name, []byte(`{"socket":"/tmp/p.sock","advertise":"/tmp/wire.sock"}`)); err == nil {
		t.Fatal("a local socket cannot be dialled from another machine")
	}
	if _, err := module.BuildOne(name, []byte(`{"socket":"/tmp/p.sock","advertise":"peer.example:4001"}`)); err != nil {
		t.Fatal(err)
	}
}
