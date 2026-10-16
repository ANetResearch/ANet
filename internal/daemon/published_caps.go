package daemon

// published_caps.go limits what a registration tells the hub this node
// serves to its public capabilities (an internal note item, A2A-DESIGN §10.2): the caps
// list of /register, like the A2A network card signed with it, names only
// entries of inbound.public_capabilities. A capability this node serves to
// its allowed or trusted peers only is theirs to know about; listing it in
// a public directory would advertise a private surface and invite calls
// that the inbound policy refuses anyway.
//
// The same goes for a public capability whose provider refuses remote
// callers (provider.RemotePolicy; LEC CC-5): ANetLink lets a remote call
// reach only a capability its c1.remote matches, and none by default.
// Listed, it would be called and every call answered provider_policy, so
// it is left off the registration and the card, and doctor's
// anetlink.publish says why. The daemon learns from the provider which
// ids to leave out, and nothing about what stands behind them.
//
// The operator's declared list (config caps, `hub-register --caps`) is kept
// as it is in the config; it is filtered where it is published.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/provider"
)

// remotePolicyTimeout bounds asking a provider which capabilities remote
// callers may use; the answer comes from a local process.
const remotePolicyTimeout = 5 * time.Second

// publicOnly returns the members of caps that are public capabilities a
// remote caller may use, in order. An allow_only capability is not among
// them: the hub never sees one (CUX-F32).
func (d *Daemon) publicOnly(caps []string) []string {
	public := map[string]bool{}
	for _, p := range d.config().inbound().PublicCapabilities {
		if id := strings.TrimSpace(p.ID); id != "" && !p.AllowOnly {
			public[id] = true
		}
	}
	remote := d.remoteCheck()
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if id := strings.TrimSpace(c); public[id] && !remote.refused(id) {
			out = append(out, c)
		}
	}
	return out
}

// remoteCheck is a fresh check for one publication: each provider that
// answers for remote callers is asked at most once.
func (d *Daemon) remoteCheck() *remoteCheck {
	return &remoteCheck{reg: d.providers, ctx: d.ctx, asked: map[string]map[string]bool{}}
}

// remoteCheck answers "may a remote caller use this capability?" for one
// publication.
type remoteCheck struct {
	reg *provider.Registry
	ctx context.Context
	// asked holds each RemotePolicy provider's answer by provider id; nil
	// for one that could not answer, which allows nothing.
	asked map[string]map[string]bool
}

// refused reports whether id's provider refuses remote callers for it. A
// capability no provider serves, or whose provider does not implement
// RemotePolicy, is not refused here (the other filters judge it).
func (rc *remoteCheck) refused(id string) bool {
	if rc.reg == nil {
		return false
	}
	p, ok := rc.reg.Resolve(id)
	if !ok {
		return false
	}
	rp, ok := p.(provider.RemotePolicy)
	if !ok {
		return false
	}
	allowed, seen := rc.asked[p.ID()]
	if !seen {
		ctx := rc.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, remotePolicyTimeout)
		var err error
		allowed, err = rp.RemoteAllowed(ctx)
		cancel()
		if err != nil {
			// Not published while it cannot say: a call would fail as well.
			log.Printf("anet: provider %s cannot say which capabilities remote callers may use, so none of its are published: %v", p.ID(), err)
			allowed = nil
		}
		rc.asked[p.ID()] = allowed
	}
	return !allowed[id]
}
