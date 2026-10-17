package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// RegisterTransport adds a direct delivery path. Called by a transport
// module at start; the hub is not one of them: the outbound engine sends to
// it itself (outbox_attempt.go, A11).
func (d *Daemon) RegisterTransport(t module.Transport) {
	d.transportMu.Lock()
	defer d.transportMu.Unlock()
	d.extraTransports = append(d.extraTransports, t)
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
// Three things differ from the hub path (engine §4.8). Step 0: an envelope
// that arrives directly has not passed the hub's per-sender rate limit, so
// the direct buckets apply before any decryption work is spent on it — the
// one of the capability it came under, when this node issued it and the
// holder's proof checks, otherwise the shared one, and a global one. An
// envelope over a limit is refused with an error, which tells the transport
// not to acknowledge it; the sender then falls back to the hub, where its
// own budget applies. Steps 1-3: an envelope this node cannot open may have
// been meant for another node the sender's address led astray, so it is
// refused the same way rather than acknowledged and lost (receiveEnvelopeVia,
// [redteam:F22]). And the answer: the delivery token travels back in the
// ack itself — tok(1) the moment step 10 commits, tok(2) when the pipeline
// refuses for good (engine ENG-V2).
type inbound struct{ d *Daemon }

// Receive is module.Inbound: nil and the delivery token (nil for none) to
// acknowledge the delivery, an error to refuse it for now.
func (in inbound) Receive(ctx context.Context, envelope []byte) ([]byte, error) {
	return in.receive(ctx, envelope)
}

// errP2PRateLimited refuses a direct delivery over a step-0 bucket.
var errP2PRateLimited = errors.New("anet: inbound transport rate limit reached; use the hub")

// receive is the pipeline's answer to a direct delivery: the token to
// acknowledge it with, or an error to refuse it for now.
func (in inbound) receive(ctx context.Context, envelope []byte) ([]byte, error) {
	// Step 0 comes before the wait for start-up: a delivery that waits
	// holds its envelope and a goroutine, and without the limit first a slow
	// start-up let anyone who reaches the peer process pile them up
	// ([redteam:F30] bypass).
	path := rxPath{direct: true}
	if !in.d.rxStep0(envelope, &path) {
		in.d.count(transientP2PRate)
		return nil, errP2PRateLimited
	}
	if in.d.identityRefusesInbound() {
		// A stopped or deactivated identity takes nothing in (KEL D-1):
		// the sender's transport hears a nack.
		return nil, errIdentityGone
	}
	if err := in.d.awaitReady(ctx); err != nil {
		return nil, err
	}
	// Acknowledged as soon as step 10 has committed, not after what follows
	// it (an internal note item, an internal note N3). The sender waits on this answer
	// with a timeout; a capability call run, or an answer sent over a path
	// that has to time out before it falls back to the hub, used to hold
	// the ack past it, and the sender sent the same envelope again through
	// the hub. The pipeline keeps running on its own goroutine after the
	// ack, and the answer goes out through the outbox as before. What the
	// commit leaves undone if the process dies then is startup recovery's:
	// a short capability call accepted this way is recorded working
	// (ingestDelegate), so it is run again at the next start.
	committed := make(chan []byte, 1)
	var once sync.Once
	path.ack = func(dack []byte) { once.Do(func() { committed <- dack }) }
	done := make(chan rxResult, 1)
	if !in.d.goBackground(func() {
		done <- in.d.receiveEnvelopeVia(ctx, envelope, path)
	}) {
		in.d.count(transientNotReady)
		return nil, errNotReady
	}
	select {
	case dack := <-committed:
		return dack, nil
	case res := <-done:
		return directAnswer(committed, res)
	}
}

// rxStep0 is step 0 of a direct delivery (engine §4.2, SS §4.3.10): the
// capability this node issued that the envelope came under, checked before
// anything is decrypted (capUnder), and whether its bucket and the global one
// have room. It records the capability in path for the steps after it.
func (d *Daemon) rxStep0(env []byte, path *rxPath) bool {
	now := d.nowMS()
	var key rxBucketKey
	if outer, err := seal.ParseOuter(env); err == nil {
		path.issued, key, _ = d.capUnder(outer, env, now)
	}
	return d.caps.rx.allow(key, now)
}

// directAnswer is what a direct delivery answers once its pipeline returned
// res. An envelope whose step 10 committed is acknowledged, with its tok(1),
// whatever the pipeline returned after that — a stop cutting the call or the
// hurried answer short returns a temporary result — and the pipeline can
// finish before Receive has seen the commit, both then ready at once. A
// refusal there would reach the sender as a nack, and a delegation this
// node holds would then be one the sender falls back to the hub for, may
// withdraw on a cancel, or report as never run ([redteam:F12][redteam:F23];
// Q29 bypass). Otherwise a permanent result is acknowledged with the token
// the pipeline made (tok(2), or none), a temporary one refused.
func directAnswer(committed <-chan []byte, res rxResult) ([]byte, error) {
	select {
	case dack := <-committed:
		return dack, nil
	default:
	}
	if !res.ack() {
		return nil, fmt.Errorf("anet: envelope not accepted yet (%s); not acknowledging", res.reason)
	}
	if res.dack == nil {
		return nil, nil
	}
	return res.dack.dack[:], nil
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

// transportPart is the TransportHost grant (hostgrants.go): with the base
// Host it is module.TransportHost.
type transportPart struct{ d *Daemon }

// Inbound on moduleHost so a transport module reaches it through the same
// narrow Host every other module gets.
func (h transportPart) Inbound() module.Inbound { return h.d.Inbound() }

// RegisterTransport lets a transport module add its path.
func (h transportPart) RegisterTransport(t module.Transport) { h.d.RegisterTransport(t) }

// counterDAckBadFrame counts answers to a direct send whose delivery
// confirmation was malformed (not base64url, or not 80 bytes).
const counterDAckBadFrame = "dack-bad-frame"

// NoteBadDAck lets a transport module count a malformed confirmation.
func (h transportPart) NoteBadDAck() { h.d.count(counterDAckBadFrame) }

// noteQuietPeer reports a recipient that has stopped collecting its mail.
//
// Once per peer per state change, like noteTransport above and for the
// same reason: a chat with a quiet peer sends a message every few
// seconds, and a line each would bury the log in exactly the situation
// where the operator most needs to read it.
func (d *Daemon) noteQuietPeer(aid string) {
	d.mu.Lock()
	if d.quietPeers == nil {
		d.quietPeers = map[string]bool{}
	}
	already := d.quietPeers[aid]
	d.quietPeers[aid] = true
	d.mu.Unlock()
	if !already {
		log.Printf("anet: the hub says %s has not collected its mail for a long time; it may not be running", aid)
	}
}

// QuietPeer returns the sentence for a recipient the hub last said has
// stopped collecting its mail, in this process's language , or
// "" when the hub has said no such
// thing (or has since answered without it). The hub states only the fact
// (recipient_quiet); the words are this node's (ux §1.4 C3,
// delivery.queued_at_hub), as for every other delivery state.
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
	quiet := d.quietPeers[aid]
	d.mu.Unlock()
	if !quiet {
		return ""
	}
	return a2ashape.Sentence("delivery.queued_at_hub", a2ashape.Lang(), nil)
}

// noteLivePeer clears the quiet mark when a peer answers, so a later
// silence is reported again rather than assumed already known.
func (d *Daemon) noteLivePeer(aid string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.quietPeers, aid)
}
