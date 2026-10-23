package daemon

// hostgrants.go makes the Host of each module (K-16, MOD-P2): the base Host
// bound to the module's name, composed with exactly the optional Host
// interfaces the name table (module.Known(), Grants) lists for that name.
// A module whose row does not list an interface gets a Host that does not
// implement it, so the module's type assertion fails; the base methods it
// is not granted answer a refusal value (nil, false, "", ErrNotGranted).
//
// Go cannot compose interfaces at run time, so the combinations are
// written out; a row whose combination is not here fails
// TestHostGrantsFollowNameTable, and the way to add one is to add the type
// below, not to widen another.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/module"
)

// Host types: the base, plus the parts a row lists.
type (
	hostBase struct{ moduleHost }
	// hostA2A: the local A2A interface answers tasks delegated to this node
	// (InboundTaskHost) and signs the proxy cards of its local interface
	// (ProxyCardSigner).
	hostA2A struct {
		moduleHost
		inboundTaskPart
		proxySignerPart
	}
	// hostTransport: a delivery path (p2p).
	hostTransport struct {
		moduleHost
		transportPart
	}
	// hostAutoReply: the seams of the auto-reply module (MOD-3): the turns it
	// answers, its own configuration block, its own evidence, the
	// never-bind list for its sandbox and the service calls of its turn
	// tools (design v0.3.9 §B).
	hostAutoReply struct {
		moduleHost
		turnPart
		evidencePart
		configPart
		sensitivePart
		serviceCallerPart
	}
	// hostPreview: the seam that sends a stream_preview frame (preview).
	hostPreview struct {
		moduleHost
		previewPart
	}
)

// sensitivePart is the SensitivePathsHost grant.
type sensitivePart struct{ d *Daemon }

var (
	_ module.Host               = hostBase{}
	_ module.InboundTaskHost    = hostA2A{}
	_ module.ProxyCardSigner    = hostA2A{}
	_ module.TransportHost      = hostTransport{}
	_ module.SensitivePathsHost = hostAutoReply{}
	_ module.TurnHost           = hostAutoReply{}
	_ module.OwnEvidenceHost    = hostAutoReply{}
	_ module.ModuleConfigHost   = hostAutoReply{}
	_ module.ServiceCaller      = hostAutoReply{}
	_ module.PreviewHost        = hostPreview{}
)

// newModuleHost is the Host of the module called name, with the state that
// records the calls it had to refuse.
func newModuleHost(d *Daemon, name string) (module.Host, *hostState) {
	st := &hostState{}
	b := moduleHost{d: d, name: name, st: st}
	has := func(g module.Grant) bool { return grantedTo(name, g) }
	inbound, signer, transport, sensitive, preview := has(module.GrantInboundTaskHost), has(module.GrantProxyCardSigner),
		has(module.GrantTransportHost), has(module.GrantSensitivePaths), has(module.GrantPreviewHost)
	turns, ownEv, cfgw := has(module.GrantTurnHost), has(module.GrantOwnEvidenceHost), has(module.GrantModuleConfigHost)
	svc := has(module.GrantServiceCaller)
	switch {
	case inbound && signer && !transport && !sensitive && !preview && !turns && !ownEv && !cfgw && !svc:
		return hostA2A{b, inboundTaskPart{d}, proxySignerPart{d}}, st
	case transport && !inbound && !signer && !sensitive && !preview && !turns && !ownEv && !cfgw && !svc:
		return hostTransport{b, transportPart{d}}, st
	case sensitive && turns && ownEv && cfgw && svc && !inbound && !signer && !transport && !preview:
		return hostAutoReply{b, turnPart{d}, evidencePart{d, name}, configPart{d, name}, sensitivePart{d},
			serviceCallerPart{d}}, st
	case preview && !inbound && !signer && !transport && !sensitive && !turns && !ownEv && !cfgw && !svc:
		return hostPreview{b, previewPart{d}}, st
	case !inbound && !signer && !transport && !sensitive && !preview && !turns && !ownEv && !cfgw && !svc:
		return hostBase{b}, st
	}
	// A row the combinations above do not cover is a bug in this file;
	// the module gets the base Host and no optional interface (fail closed).
	panic(fmt.Sprintf("daemon: no Host type for the grants of module %q", name))
}

// SensitivePaths implements module.SensitivePathsHost: the paths a sandbox
// never binds (K-5). The fixed part is the kernel's: the data directory, the
// identities container, the legacy and user runtime directories, the control
// and local A2A tokens and the default C1 socket (whether or not anetlink is
// available); the rest is what each available module declares through
// module.LocalPaths.
func (p sensitivePart) SensitivePaths() []string { return p.d.sensitivePaths() }

func (d *Daemon) sensitivePaths() []string {
	paths := []string{d.layout.Root, AnetHome(), d.layout.ControlTokenPath(),
		filepath.Join(anethome.A2ADir(d.layout.Root), anethome.A2ATokenFile)}
	paths = append(paths, runtimeSensitivePaths()...)
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		paths = append(paths, x)
	}
	if s := anethome.DefaultC1Socket(); s != "" {
		paths = append(paths, s)
	}
	for _, m := range d.modules {
		if lp, ok := m.(module.LocalPaths); ok {
			paths = append(paths, lp.LocalPaths()...)
		}
	}
	sort.Strings(paths)
	return resolvedAll(paths)
}

// resolvedAll returns each path in its absolute form and, where it exists, also its symbolic-link
// resolved form, without duplicates. (The auto-reply module keeps the same helper for its own lists.)
func resolvedAll(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		add(filepath.Clean(abs))
		if r, err := pathsafe.ResolveThroughExisting(abs); err == nil {
			add(r)
		}
	}
	return out
}
