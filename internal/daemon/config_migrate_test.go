package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A config without an inbound block gets the closed default (SI-5). The wire-1 accept_delegations key is not
// migrated any more: it stops the daemon from starting (ux §2.3, UX-D3),
// and nothing reads it.
func TestAConfigWithoutAnInboundBlockIsClosed(t *testing.T) {
	for _, c := range []struct{ name, config string }{
		{"minimal", `{"control_addr":"127.0.0.1:0"}`},
		{"inbound block", `{"control_addr":"127.0.0.1:0","inbound":{"policy":"closed"}}`},
	} {
		l := NewLayout(t.TempDir())
		if err := l.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.ConfigPath(), []byte(c.config), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(l)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Inbound == nil || cfg.inbound().Policy != PolicyClosed {
			t.Errorf("%s: inbound %+v, want closed", c.name, cfg.Inbound)
		}
	}
	l := NewLayout(t.TempDir())
	if err := l.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ConfigPath(), []byte(`{"control_addr":"127.0.0.1:0","accept_delegations":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var cr *ConfigRefusal
	if err := CheckConfigForStart(l); !errors.As(err, &cr) || len(cr.Refusals) != 1 ||
		!strings.Contains(cr.Error(), "accept_delegations") {
		t.Fatalf("accept_delegations: %v", err)
	}
}
