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
	var out struct {
		Warning string `json:"warning"`
		Quiet   bool   `json:"recipient_quiet"`
	}
	if err := h.d.hubPost(ctx, hub, "/relay/send", map[string]any{
		"to_aid":         toAID,
		"from_aid":       h.d.AID(),
		"kind":           kind,
		"interaction_id": interactionID,
		"payload":        base64.StdEncoding.EncodeToString(payload),
	}, &out); err != nil {
		return err
	}
	// The hub knows the recipient has not collected its mail in a long
	// time, and said so. What it said is kept against the recipient's AID
	// so the caller that just sent can report it (see QuietPeer): the
	// daemon log is where the operator of THIS node reads, and the person
	// who needs the fact is whoever typed `anet delegate`.
	//
	// Recorded on every send rather than only logged: an operator watching
	// a delegation sit unanswered otherwise has no way to learn that the
	// other end stopped running last Tuesday.
	switch {
	case out.Quiet && out.Warning != "":
		h.d.noteQuietPeer(toAID, out.Warning)
	case !out.Quiet:
		// The hub answered and did not raise the mark, so a mark left over
		// from an earlier send is no longer what the hub says. Dropping it
		// keeps the reported state to what was last actually observed —
		// otherwise a peer that came back would be reported quiet forever,
		// since only inbound traffic clears it.
		h.d.noteLivePeer(toAID)
	}
	// Quiet with no sentence to pass on is left alone: nothing to report
	// and nothing to retract. The hub sets both fields together.
	return nil
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
			d.noteTransport(t.Name(), nil)
			return nil
		}
		lastErr = err
		// Logged on the transition, not on every message.
		//
		// A direct transport quietly failing while the hub covers for it is
		// degradation worth knowing about — but a node behind NAT fails
		// this way on every single delegation, and a line each would bury
		// the log in the steady state. A benchmark made that concrete: the
		// fall-through path emitted 200,000 lines and never finished
		// reporting. So: one line when it starts failing, one when it
		// recovers, silence in between.
		d.noteTransport(t.Name(), err)
	}
	if lastErr == nil {
		return fmt.Errorf("anet: no transport can reach %s", toAID)
	}
	return lastErr
}

// noteTransport reports a transport's health only when it changes.
func (d *Daemon) noteTransport(name string, err error) {
	d.transportMu.Lock()
	defer d.transportMu.Unlock()
	if d.transportFailing == nil {
		d.transportFailing = map[string]string{}
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	prev, seen := d.transportFailing[name]
	switch {
	case err != nil && (!seen || prev != msg):
		log.Printf("anet: transport %s: %v (falling through; hub is carrying delivery)", name, err)
		d.transportFailing[name] = msg
	case err == nil && seen:
		log.Printf("anet: transport %s: recovered", name)
		delete(d.transportFailing, name)
	}
}

// transportState is the daemon's registry of extra delivery paths.
type transportState struct {
	transportMu     sync.RWMutex
	extraTransports []module.Transport
	// transportFailing remembers which transports are currently failing and
	// with what, so health is logged on change rather than per message.
	transportFailing map[string]string
}

// Inbound gives transports somewhere to deliver what they receive.
func (d *Daemon) Inbound() module.Inbound { return inbound{d} }

// inbound routes a message that arrived over any transport into exactly the
// same path as one pulled from the hub mailbox.
//
// This is the property that matters: a delegation that came over a direct
// peer connection and one that came from the hub are the same delegation,
// verified the same way, and the daemon cannot tell them apart. A transport
// is a route, not a trust boundary — the payload is end-to-end verifiable,
// so a peer process that lies produces a message that fails verification
// rather than one that gets believed because of how it arrived.
type inbound struct{ d *Daemon }

func (in inbound) Receive(_ context.Context, fromAID, kind, interactionID string, payload []byte) error {
	ok := in.d.dispatch(relayMsg{
		FromAID:       fromAID,
		Kind:          kind,
		InteractionID: interactionID,
	}, payload)
	if !ok {
		// dispatch returns false for a transient failure it wants retried.
		// Reporting it lets the transport decline to ack, so the sender
		// re-delivers rather than the message being lost.
		return fmt.Errorf("anet: %s from %s not accepted", kind, fromAID)
	}
	return nil
}

var _ module.TransportHost = moduleHost{}

// Inbound on moduleHost so a transport module reaches it through the same
// narrow Host every other module gets.
func (h moduleHost) Inbound() module.Inbound { return h.d.Inbound() }

// RegisterTransport lets a transport module add its path.
func (h moduleHost) RegisterTransport(t module.Transport) { h.d.RegisterTransport(t) }

// noteQuietPeer reports a recipient that has stopped collecting its mail.
//
// Once per peer per state change, like noteTransport above and for the
// same reason: a chat with a quiet peer sends a message every few
// seconds, and a line each would bury the log in exactly the situation
// where the operator most needs to read it.
func (d *Daemon) noteQuietPeer(aid, warning string) {
	d.mu.Lock()
	if d.quietPeers == nil {
		d.quietPeers = map[string]string{}
	}
	_, already := d.quietPeers[aid]
	d.quietPeers[aid] = warning
	d.mu.Unlock()
	if !already {
		log.Printf("anet: %s", warning)
	}
}

// QuietPeer returns the hub's most recent statement that aid has stopped
// collecting its mail, or "" when the hub has made none (or has since
// answered without it).
//
// This is how a control-plane handler reports what the hub said about the
// recipient of the send it just made. The alternative — returning the flag
// up through relaySend — would have to travel through module.Transport,
// which is a delivery contract shared with transports that have no notion
// of a mailbox to be quiet about. The cost of reading it back out of the
// daemon instead is that the answer is "the last thing the hub said about
// this peer", not "what the hub said about this exact send"; those differ
// only if another send to the same peer interleaves.
func (d *Daemon) QuietPeer(aid string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quietPeers[aid]
}

// noteLivePeer clears the quiet mark when a peer answers, so a later
// silence is reported again rather than assumed already known.
func (d *Daemon) noteLivePeer(aid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.quietPeers, aid)
}
