package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
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

func (h hubTransport) Send(ctx context.Context, toAID string, envelope []byte) error {
	hub := h.d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	var out hubapi.RelaySendResponse
	if err := h.d.hubSigned(ctx, hub, http.MethodPost, "/relay/send", relayauth.ActionSend, hubapi.RelaySendRequest{
		ToAID:    toAID,
		Envelope: base64.StdEncoding.EncodeToString(envelope),
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
	case out.RecipientQuiet && out.Warning != "":
		h.d.noteQuietPeer(toAID, out.Warning)
	case !out.RecipientQuiet:
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

// deliverEnvelope delivers one sealed envelope, trying each transport in
// order with the same bytes.
//
// A transport that reports itself unreachable is skipped without being
// asked to try; one that fails is logged and the next is tried. Only when
// every path has failed does the caller see an error, and that error names
// the last failure rather than a summary — an operator debugging delivery
// wants to know what the hub said, not that "all transports failed".
func (d *Daemon) deliverEnvelope(ctx context.Context, toAID string, envelope []byte) error {
	_, err := d.deliverEnvelopeTracked(ctx, toAID, envelope)
	return err
}

// deliverEnvelopeTracked is deliverEnvelope that also says, when every
// path failed, whether one of the failures may have delivered the envelope
// anyway (mayHaveDelivered). The retry queue keeps that on the row: a
// message that may have arrived is not reported as one that never did
// ([redteam:F12]).
func (d *Daemon) deliverEnvelopeTracked(ctx context.Context, toAID string, envelope []byte) (maybe bool, err error) {
	var lastErr error
	for _, t := range d.transports() {
		if !t.Reachable(ctx, toAID) {
			continue
		}
		err := t.Send(ctx, toAID, envelope)
		if err == nil {
			d.noteTransport(t.Name(), nil)
			return false, nil
		}
		maybe = maybe || mayHaveDelivered(t, err)
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
		return false, fmt.Errorf("anet: no transport can reach %s", toAID)
	}
	return maybe, lastErr
}

// mayHaveDelivered reports whether a failed Send may still have delivered
// the envelope. module.Transport has a Send that partially succeeded report
// failure — a p2p round trip that timed out while the far side was at work
// — so a failure counts as "may have" unless it is known to have reached
// nobody: one the transport marks module.ErrNotDelivered, an answer from
// the hub itself (it stored nothing; a gateway's 502 or 504 speaks for a
// hub that may have), or a hub that could not be dialled.
func mayHaveDelivered(t module.Transport, err error) bool {
	if errors.Is(err, module.ErrNotDelivered) {
		return false
	}
	if _, hub := t.(hubTransport); !hub {
		return true
	}
	var he *hubError
	if errors.As(err, &he) {
		return he.code == http.StatusBadGateway || he.code == http.StatusGatewayTimeout
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return false
	}
	return true
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

// inbound routes an envelope that arrived over a transport module into the
// same receive pipeline as one pulled from the hub mailbox (receive.go).
//
// A transport is a route, not a trust boundary: the envelope is sealed to
// this node and signed by its sender, so a peer process that lies produces
// an envelope that fails to open or verify, not one that is believed
// because of how it arrived.
//
// Two things differ from the hub path. Step 0 of §3.6: an envelope that
// arrives directly has not passed the hub's per-sender rate limit, so a
// daemon-wide limit applies before any decryption work is spent on it. An
// envelope over the limit is refused with an error, which tells the
// transport not to acknowledge it; the sender then falls back to the hub,
// where its own sender budget applies. And steps 1-4: an envelope this node
// cannot open may have been meant for another node the sender's address led
// astray, so it is refused the same way rather than acknowledged and lost
// (receiveEnvelopeVia, [redteam:F22]).
type inbound struct{ d *Daemon }

func (in inbound) Receive(ctx context.Context, envelope []byte) error {
	// The rate limit comes before the wait for start-up: a delivery that
	// waits holds its envelope and a goroutine, and without the limit
	// first a slow start-up let anyone who reaches the peer process pile
	// them up ([redteam:F30] bypass).
	if !in.d.p2pLimit.allow(in.d.nowMS()) {
		in.d.count(transientP2PRate)
		return errP2PRateLimited
	}
	if err := in.d.awaitReady(ctx); err != nil {
		return err
	}
	// Acknowledged as soon as step 10 has committed, not after what follows
	// it (0017 Q29, docs/notes/0025 N3). The sender waits on this answer
	// with a timeout; a capability call run, or an answer sent over a path
	// that has to time out before it falls back to the hub, used to hold
	// the ack past it, and the sender sent the same envelope again through
	// the hub. The pipeline keeps running on its own goroutine after the
	// ack, and the answer goes out through the retry queue as before. What
	// the commit leaves undone if the process dies then is startup
	// recovery's: a short capability call accepted this way is recorded
	// working (ingestDelegate), so it is run again at the next start.
	committed := make(chan struct{})
	var once sync.Once
	ack := func() { once.Do(func() { close(committed) }) }
	done := make(chan rxResult, 1)
	if !in.d.goBackground(func() {
		done <- in.d.receiveEnvelopeVia(ctx, envelope, rxPath{direct: true, ack: ack})
	}) {
		in.d.count(transientNotReady)
		return errNotReady
	}
	select {
	case <-committed:
		return nil
	case res := <-done:
		if !res.ack() {
			return fmt.Errorf("anet: envelope not accepted yet (%s); not acknowledging", res.reason)
		}
		return nil
	}
}

// errNotReady refuses a delivery that arrived while the daemon was starting
// and could not wait for it: the delivery's own context ended, or the daemon
// stopped before start-up finished.
var errNotReady = errors.New("anet: this node is starting or stopping; not acknowledging")

// awaitReady holds a delivery from a transport module until New has finished
// ([redteam:F30]). A transport module starts delivering as soon as its Start
// runs, which is before the modules after it have started and before
// startup recovery (recoverInterrupted) has told the previous process's
// leftovers from new work; the hub relay loop starts after both, and a
// direct delivery now waits for the same point. A daemon that never gets
// there (a failed start, a stop) refuses it temporarily, so the sender
// falls back to the hub.
func (d *Daemon) awaitReady(ctx context.Context) error {
	if d.ready == nil {
		return nil // not built by New (unit tests of other parts)
	}
	select {
	case <-d.ready:
		return nil
	default:
	}
	select {
	case <-d.ready:
		return nil
	case <-ctx.Done():
	case <-d.ctx.Done():
	}
	d.count(transientNotReady)
	return errNotReady
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
// up through deliverEnvelope — would have to travel through module.Transport,
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
