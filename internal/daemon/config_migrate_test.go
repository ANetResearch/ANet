package daemon

import (
	"os"
	"testing"
)

// The accept_delegations notice ("is replaced by inbound.policy … this node
// now runs policy closed") is for an operator whose config carried the old
// key set to accept. A minimal config, with neither the key nor an inbound
// block, replaced nothing: it gets the closed default silently (0024 L4).
func TestTheAcceptDelegationsNoticeNeedsTheOldKey(t *testing.T) {
	for _, c := range []struct {
		name, config string
		notice       bool
	}{
		{"minimal", `{"control_addr":"127.0.0.1:0"}`, false},
		{"old key, accept", `{"control_addr":"127.0.0.1:0","accept_delegations":true}`, true},
		{"old key, refuse", `{"control_addr":"127.0.0.1:0","accept_delegations":false}`, false},
		{"inbound block", `{"control_addr":"127.0.0.1:0","accept_delegations":true,"inbound":{"policy":"closed"}}`, false},
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
		if cfg.migratedInbound != c.notice {
			t.Errorf("%s: notice %v, want %v", c.name, cfg.migratedInbound, c.notice)
		}
		if cfg.inbound().Policy != PolicyClosed {
			t.Errorf("%s: policy %s, want closed", c.name, cfg.inbound().Policy)
		}
	}
}
