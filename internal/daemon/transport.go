package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sync"

	"github.com/ANetResearch/ANet/module"
)

// hubTransport is the always-available delivery path.
//
// It is the transport the daemon has always had, now behind the same
// contract everything else uses. Making the existing path implement the
// seam first — rather than writing the seam for a peer-to-peer module that
// does not exist yet — is what keeps the contract honest: it was shaped by
// something real before anything speculative touched it.
type hubTransport struct{ d *Daemon }

func (h hubTransport) Name() string { return "hub" }

// Reachable: a configured hub can reach any AID. That is the property that
// makes it the fallback — it holds a mailbox, so the peer does not have to
// be online, or routable, or even running.
func (h hubTransport) Reachable(context.Context, string) bool {
	return h.d.config().HubURL != ""
}

func (h hubTransport) Send(ctx context.Context, toAID, kind, interactionID string, payload []byte) error {
	hub := h.d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	return h.d.hubPost(ctx, hub, "/relay/send", map[string]any{
		"to_aid":         toAID,
		"from_aid":       h.d.AID(),
		"kind":           kind,
		"interaction_id": interactionID,
		"payload":        base64.StdEncoding.EncodeToString(payload),
	}, nil)
}

// transports returns the delivery paths in preference order.
//
// Direct paths first, hub last. A module that can reach a peer without a
// round trip through someone else's server should, and the hub is what
// catches everything it cannot: an offline peer, a peer behind a NAT that
// hole-punching lost, a peer this node has never seen. The hub is never
// removed from the list — "sometimes you only distribute alongside a hub"
// is the ordinary case, not a degraded one.
func (d *Daemon) transports() []module.Transport {
	d.transportMu.RLock()
	extra := append([]module.Transport(nil), d.extraTransports...)
	d.transportMu.RUnlock()
	return append(extra, hubTransport{d})
}

// RegisterTransport adds a delivery path. Called by a transport module at
// start; the hub path is always present and is not registered this way.
func (d *Daemon) RegisterTransport(t module.Transport) {
	d.transportMu.Lock()
	defer d.transportMu.Unlock()
	d.extraTransports = append(d.extraTransports, t)
}

// relaySend delivers one payload, trying each transport in order.
//
// A transport that reports itself unreachable is skipped without being
// asked to try; one that fails is logged and the next is tried. Only when
// every path has failed does the caller see an error, and that error names
// the last failure rather than a summary — an operator debugging delivery
// wants to know what the hub said, not that "all transports failed".
func (d *Daemon) relaySend(ctx context.Context, toAID, kind, interactionID string, payload []byte) error {
	var lastErr error
	for _, t := range d.transports() {
		if !t.Reachable(ctx, toAID) {
			continue
		}
		err := t.Send(ctx, toAID, kind, interactionID, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		// Worth a line: a direct transport quietly failing on every
		// delegation, with the hub silently covering for it, is the kind of
		// degradation that goes unnoticed until the hub is down too.
		log.Printf("anet: transport %s: %v (falling through)", t.Name(), err)
	}
	if lastErr == nil {
		return fmt.Errorf("anet: no transport can reach %s", toAID)
	}
	return lastErr
}

// transportState is the daemon's registry of extra delivery paths.
type transportState struct {
	transportMu     sync.RWMutex
	extraTransports []module.Transport
}
