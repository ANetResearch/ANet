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
//
// Configuration, modules.anetlink in config.json: the block being there
// turns the module on, and `"anetlink": {}` is a complete one. socket is the
// C1 socket anetlinkd serves, by default $XDG_RUNTIME_DIR/anetlink/c1.sock,
// which is anetlinkd's own default, so neither side needs it set. The keys
// every Unix socket backend takes (internal/backendconn) say who may listen
// on it: expected_user (by default the socket file's owner), socket_group,
// peer_check. allow_tcp is refused: C1 runs over a Unix socket only.
package anetlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
	"github.com/ANetResearch/ANet/module"
	linkprov "github.com/ANetResearch/ANet/provider/anetlink"
)

const name = "anetlink"

// registerTimeout bounds the registration handshake at start. A constant,
// not a setting: modules.anetlink.timeout_s was removed (CC-7).
const registerTimeout = 10 * time.Second

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, nil // compiled in, not configured; "anetlink": null is the same as no block
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		prov, err := cfg.provider()
		if err != nil {
			return nil, err
		}
		return &Module{prov: prov}, nil
	})
}

// Config is the module's configuration block.
type Config struct {
	// Socket is the C1 socket an ANetLink runtime serves; empty means
	// linkprov.DefaultSocket.
	Socket string `json:"socket,omitempty"`
	// Policy says who may be on the far side of the socket: expected_user,
	// socket_group, peer_check (internal/backendconn). Its allow_tcp must
	// stay unset.
	backendconn.Policy
}

// errTCP is the refusal of allow_tcp: true. The daemon refuses the same
// configuration before it gets here, as config_invalid naming the key.
var errTCP = errors.New("modules.anetlink.allow_tcp: C1 runs over a Unix socket only, never TCP; remove allow_tcp")

// provider checks cfg and builds the provider it describes.
func (cfg Config) provider() (*linkprov.Provider, error) {
	if cfg.AllowTCP {
		return nil, errTCP
	}
	sock := cfg.Socket
	if sock == "" {
		var err error
		if sock, err = linkprov.DefaultSocket(); err != nil {
			return nil, err
		}
	}
	rules, err := cfg.Policy.Resolve()
	if err != nil {
		return nil, fmt.Errorf("modules.anetlink.%w", err)
	}
	return linkprov.New(name, sock, rules)
}

// Module registers the ANetLink provider.
type Module struct {
	prov *linkprov.Provider
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	rctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()

	// Registration failure is fatal, unlike the old inline wiring which
	// logged and carried on. A module that was compiled in AND configured
	// and cannot start is an operator error; coming up without the
	// capabilities they asked for is the silent kind of failure that gets
	// discovered by a delegation that mysteriously finds no provider.
	if err := h.Providers().Register(rctx, m.prov); err != nil {
		if linkprov.NotC1v1(err) {
			// The one cause an operator can only fix by upgrading: say so in
			// the reason table's words (ux §1.4 J, c1_version).
			return fmt.Errorf("register provider on %s: %s (%w)", m.prov.Socket(),
				a2ashape.Sentence("c1_version", a2ashape.Lang(), nil), err)
		}
		return fmt.Errorf("register provider on %s: %w", m.prov.Socket(), err)
	}
	return nil
}

func (m *Module) Stop(context.Context) error { return nil }

// LocalPaths implements module.LocalPaths: the C1 socket, which a sandbox
// must never be able to open (K-5). The default socket is the kernel's to
// list whether or not this module is available (anethome.DefaultC1Socket);
// a configured one is listed here.
func (m *Module) LocalPaths() []string {
	if m.prov == nil {
		return nil
	}
	return []string{m.prov.Socket()}
}

var _ module.LocalPaths = (*Module)(nil)
