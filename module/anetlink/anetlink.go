//go:build !no_anetlink

// Package anetlink is the device-capability module: it registers the
// ANetLink provider so this node can offer the capabilities of whatever
// ANetLink reaches.
//
// It is the first subsystem through the module seam, and a real one rather
// than a demonstration: a daemon distributed alongside a hub, for an agent
// that delegates and receives work but touches no hardware, has no use for
// it. `-tags no_anetlink` and it is not in the binary — not disabled at
// runtime, not dead code behind a flag, absent.
//
// Note what this module does NOT hand the daemon: any notion of a device.
// It registers a provider that offers capability ids, and the daemon
// resolves and invokes them exactly as it would any other provider. That is
// C1's red line, and it is what stops ANetLink from becoming the second org.
package anetlink

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ANetResearch/ANet/module"
	linkprov "github.com/ANetResearch/ANet/provider/anetlink"
)

const name = "anetlink"

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if cfg.Socket == "" {
			return nil, fmt.Errorf("socket is required")
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config is the module's configuration block.
type Config struct {
	// Socket is the C1 provider socket an ANetLink runtime is serving.
	Socket string `json:"socket"`
	// TimeoutS bounds the initial registration handshake.
	TimeoutS int `json:"timeout_s"`
}

// Module registers the ANetLink provider.
type Module struct {
	cfg Config
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	timeout := 5 * time.Second
	if m.cfg.TimeoutS > 0 {
		timeout = time.Duration(m.cfg.TimeoutS) * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Registration failure is fatal, unlike the old inline wiring which
	// logged and carried on. A module that was compiled in AND configured
	// and cannot start is an operator error; coming up without the
	// capabilities they asked for is the silent kind of failure that gets
	// discovered by a delegation that mysteriously finds no provider.
	if err := h.Providers().Register(rctx, linkprov.New(name, m.cfg.Socket)); err != nil {
		return fmt.Errorf("register provider on %s: %w", m.cfg.Socket, err)
	}
	return nil
}

func (m *Module) Stop(context.Context) error { return nil }
