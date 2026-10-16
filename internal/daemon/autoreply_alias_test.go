package daemon

// autoreply_alias_test.go: the kernel's tests that build an auto-reply configuration or drive the module
// through the control plane name its exported types here. The module is registered by its import.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/autoreply"
)

type (
	AutoReplyConfig        = autoreply.AutoReplyConfig
	AutoReplySandboxConfig = autoreply.AutoReplySandboxConfig
	BudgetValue            = autoreply.BudgetValue
)

const (
	UntrustedOff       = autoreply.UntrustedOff
	UntrustedSandbox   = autoreply.UntrustedSandbox
	TrustedSandbox     = autoreply.TrustedSandbox
	EvAutoReplyInvoked = autoreply.EvAutoReplyInvoked
)

var ParseAutoReply = autoreply.ParseAutoReply

// rawAR is cfg as the auto_reply block (nil: no block).
func rawAR(cfg *AutoReplyConfig) json.RawMessage {
	if cfg == nil {
		return nil
	}
	b, _ := json.Marshal(cfg)
	return b
}

// AutoReplyOf is the auto-reply configuration c holds.
func AutoReplyOf(c Config) *AutoReplyConfig { return ParseAutoReply(c.AutoReply) }

// autoReplyRoute calls the module's control handler for route with body, as the control plane does.
func (d *Daemon) autoReplyRoute(route string, body []byte) (int, any, error) {
	for _, m := range d.modules {
		if h, ok := m.(module.ControlHandler); ok && m.Name() == "autoreply" {
			return h.HandleControl(context.Background(), route, body)
		}
	}
	return 0, nil, errors.New("the auto-reply module is not running")
}

// SetAutoReply makes cfg the auto-reply configuration (nil: off), as POST /autoreply does: the members cfg
// leaves out go back to their defaults (null), so the block ends up equal to cfg.
func (d *Daemon) SetAutoReply(cfg *AutoReplyConfig) error {
	if cfg == nil {
		_, _, err := d.autoReplyRoute("POST /autoreply", []byte(`{"off":true}`))
		return err
	}
	members := map[string]json.RawMessage{}
	b, _ := json.Marshal(cfg)
	_ = json.Unmarshal(b, &members)
	if c, err := LoadConfig(d.layout); err == nil && c.AutoReply != nil {
		var old map[string]json.RawMessage
		if json.Unmarshal(c.AutoReply, &old) == nil {
			for k := range old {
				if _, ok := members[k]; !ok {
					members[k] = json.RawMessage("null")
				}
			}
		}
	}
	b, _ = json.Marshal(members)
	_, _, err := d.autoReplyRoute("POST /autoreply", b)
	return err
}

func (d *Daemon) hAutoReply(w http.ResponseWriter, r *http.Request) {
	d.moduleRoute("autoreply", "POST /autoreply")(w, r)
}
