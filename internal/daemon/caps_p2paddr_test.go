package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// Grant key 5 is host:port with a decimal port, at most 262 bytes (core
// §2.3), and seal refuses to write a grant whose p2p is anything else. The
// p2p module also takes "tcp://host:port" and surrounding space, so the
// advertise value is written in the grant's form; one that is not an
// address even then is left out of the grant rather than stopping it.
func TestGrantP2PAddrIsTheGrantsForm(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1:4710":                   "192.0.2.1:4710",
		"tcp://192.0.2.1:4710":             "192.0.2.1:4710",
		" node.example:4710\n":             "node.example:4710",
		"tcp://[2001:db8::1]:4710":         "[2001:db8::1]:4710",
		"192.0.2.1:+80":                    "192.0.2.1:80",
		"192.0.2.1:0080":                   "192.0.2.1:80",
		"":                                 "",
		"192.0.2.1":                        "",
		"192.0.2.1:0":                      "",
		"192.0.2.1:65536":                  "",
		"unix:///run/anet/p2p":             "",
		strings.Repeat("h", 300) + ":4710": "",
	} {
		got := grantP2PAddr(in)
		if got != want {
			t.Errorf("grantP2PAddr(%q) = %q, want %q", in, got, want)
		}
		if got != "" && !seal.ValidP2PAddr(got) {
			t.Errorf("grantP2PAddr(%q) = %q, which a grant cannot carry", in, got)
		}
		cfg := Config{Modules: ModulesConfig{"p2p": json.RawMessage(`{"advertise":` + strconvQuote(in) + `}`)}}
		if p := p2pAdvertise(cfg); p != want {
			t.Errorf("p2pAdvertise with advertise %q = %q, want %q", in, p, want)
		}
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
