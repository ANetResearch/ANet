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
	// answers, its own configuration block, its own evidence and the
	// never-bind list for its sandbox.
	hostAutoReply struct {
		moduleHost
		turnPart
		evidencePart
		configPart
		sensitivePart
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
	switch {
	case inbound && signer && !transport && !sensitive && !preview && !turns && !ownEv && !cfgw:
		return hostA2A{b, inboundTaskPart{d}, proxySignerPart{d}}, st
	case transport && !inbound && !signer && !sensitive && !preview && !turns && !ownEv && !cfgw:
		return hostTransport{b, transportPart{d}}, st
	case sensitive && turns && ownEv && cfgw && !inbound && !signer && !transport && !preview:
		return hostAutoReply{b, turnPart{d}, evidencePart{d, name}, configPart{d, name}, sensitivePart{d}}, st
	case preview && !inbound && !signer && !transport && !sensitive && !turns && !ownEv && !cfgw:
		return hostPreview{b, previewPart{d}}, st
	case !inbound && !signer && !transport && !sensitive && !preview && !turns && !ownEv && !cfgw:
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
	paths := []string{d.layout.Root, AnetHome(), legacyRuntimeDir(), filepath.Join("/run/user", fmt.Sprint(os.Getuid())),
		d.layout.ControlTokenPath(), filepath.Join(anethome.A2ADir(d.layout.Root), anethome.A2ATokenFile)}
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
