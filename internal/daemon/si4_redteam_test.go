package daemon

// Red-team PoCs for SI-4 (lens si4): a sender authenticated as the peer of
// an interaction is still bound to what it was admitted for. Each test
// PASSES when the attack SUCCEEDS, i.e. when the defect is present.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// SI4-RT-2. A delegation refused in step 9 is remembered only in the
// in-memory refused LRU (4096 entries, lost on restart), never in the
// replay table. A hub keeps a copy of the acknowledged envelope and hands
// it in again (up to exp = 14 days later) after the operator changed the
// policy and the daemon restarted: the same signed envelope, which the
// requester was told was rejected, is now accepted and executed.
func TestRedteamSI4_RefusedDelegationReplayedByHubAfterRestartExecutes(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	ix := "ix_redteam_replay_0001"
	env := sealFrom(t, stranger, prov, seal.TypeDelegate, ix, delegateBody(t, stranger.self, ix, "lamp on", lampCap))
	// Delivered once: refused (closed, not allowed) and acknowledged.
	if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropNotAccepting {
		t.Fatalf("first delivery: %+v", r)
	}
	// Replayed immediately: the LRU catches it.
	if r := receive(t, prov, env); r.reason != dropRefusedReplay {
		t.Fatalf("immediate replay: %+v", r)
	}
	// Later the operator allows this peer for some other purpose, and the
	// daemon restarts (update, reboot).
	allowPeers(t, prov, stranger.AID())
	prov = rtReopen(t, prov)
	lamp2 := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp2); err != nil {
		t.Fatal(err)
	}
	// The hub replays the old envelope bytes.
	r := receive(t, prov, env)
	if len(lamp2.invoked) != 1 {
		t.Fatalf("attack failed: %+v invoked %d", r, len(lamp2.invoked))
	}
	t.Logf("DEFECT: a refused, acknowledged delegation replayed by the hub was executed (rx=%+v)", r)
}

// rtReopen closes d and starts a daemon on the same data directory (a copy
// of reopen, which lives in a !no_x402 file).
func rtReopen(t *testing.T, d *Daemon) *Daemon {
	t.Helper()
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	if n.relayStop != nil {
		n.relayStop()
		n.relayStop = nil
	}
	n.mu.Unlock()
	t.Cleanup(func() { n.Close() })
	return n
}

// SI4-RT-3. seal/limits.go sizes MaxKELEvents/MaxKELBytes on the premise
// that "a KEL is replayed on every receive, one Ed25519 verification per
// event, before the sender is known". The receive path actually replays
// the carried KEL several times per envelope (resolveSenderKEL,
// VerifyInnerSig + identity.VerifyObject, VerifyInnerKeys ->
// VerifyEncKeySet, and again for nested objects), so a free stranger AID
// that pads its own KEL with self-signed drt events to just under 64 KiB
// makes each envelope cost tens of times more than a normal one. The hub
// lets one sender post 20/s and the poll loop handles envelopes one at a
// time under pollMu, so one AID can occupy the victim's receive loop.
func TestRedteamSI4_PaddedKELMakesEachEnvelopeExpensive(t *testing.T) {
	prov := newTestDaemon(t, "", false)
	measure := func(extra int) (time.Duration, int) {
		c, err := identity.Incept()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < extra; i++ {
			pub, _, _ := ed25519.GenerateKey(nil)
			if err := c.Delegate(pub, uint64(time.Now().UnixMilli())); err != nil {
				t.Fatal(err)
			}
		}
		s := sender{aid: c.AID(), kel: c.KEL(), keys: signedKeysFor(t, c, 0), ksn: c.CurrentSeq(), sign: c.Sign, ctrl: c}
		kb, _ := identity.MarshalKEL(c.KEL())
		const reps = 10
		var total time.Duration
		for i := 0; i < reps; i++ {
			env := craft(t, s, prov, seal.TypeMessage, "ix_rt_unknown", chatBody(t, "x", ""), nil)
			st := time.Now()
			prov.receiveEnvelope(context.Background(), env)
			total += time.Since(st)
		}
		return total / reps, len(kb)
	}
	small, _ := measure(0)
	big, size := measure(180)
	if size > seal.MaxKELBytes {
		t.Fatalf("setup: KEL %d bytes over the cap", size)
	}
	ratio := float64(big) / float64(small)
	t.Logf("1-event KEL: %s/envelope; 181-event KEL (%d bytes): %s/envelope; x%.0f; 20 env/s from one AID = %s of receive work per second",
		small, size, big, ratio, 20*big)
	if ratio < 20 {
		t.Fatalf("attack failed: ratio only %.1f", ratio)
	}
}

// SI4-RT-4. §3.6 step 9 holds (does not acknowledge) an authenticated
// message for an ix this node does not know while now - inner.ts <= 10 min.
// inner.ts is the sender's choice up to now + 5 min (step 5), so a stranger
// holds each message ~15 min. The held envelopes stay in the hub mailbox and
// count against the per-recipient quota (5000 envelopes / 1 GiB; 1 GiB is
// eleven 96 MiB envelopes), so one free AID keeps an ONLINE victim's
// mailbox full: every other sender gets 507 and nothing reaches it.
func TestRedteamSI4_UnknownIXWaitPinsTheMailboxFull(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	stranger := registered(t, srv.URL, "stranger")
	fake := fakeHubAt(t, srv.URL)
	const quota = 4 // stands for the hub's 5000 / 1 GiB per-recipient cap
	fake.mu.Lock()
	fake.mailboxCap = quota
	fake.mu.Unlock()

	s := senderOf(stranger)
	base := uint64(time.Now().UnixMilli())
	for i := 0; i < quota; i++ {
		env := craft(t, s, prov, seal.TypeMessage, fmt.Sprintf("ix_no_such_task_%d", i), chatBody(t, "x", ""),
			func(in *seal.SealedInner) { in.TS = base + 4*60*1000 }) // ts in the future, inside the 5 min skew
		if err := stranger.deliverEnvelope(ctx, prov.AID(), env); err != nil {
			t.Fatal(err)
		}
	}
	// The victim is online and polls; 14 minutes later it still holds them.
	for _, at := range []uint64{0, 5, 10, 14} {
		now := base + at*60*1000
		prov.setClock(func() uint64 { return now })
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil { // the cursor round back to the head
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	held := 0
	for _, m := range fake.mailbox {
		if m.toAID == prov.AID() {
			held++
		}
	}
	fake.mu.Unlock()
	// A legitimate, allowed peer now cannot reach the victim through the hub.
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix_whatever", chatBody(t, "hello", ""))
	err := req.hubSigned(ctx, srv.URL, http.MethodPost, "/relay/send", relayauth.ActionSend,
		hubapi.RelaySendRequest{ToAID: prov.AID(), Envelope: base64.StdEncoding.EncodeToString(env)}, nil)
	if held != quota || hubStatus(err) != http.StatusInsufficientStorage {
		t.Fatalf("attack failed: held %d, legit send %v", held, err)
	}
	t.Logf("DEFECT: %d stranger envelopes held for 14+ min by an online victim (t-unknown-ix=%d); legit sender gets %v",
		held, counter(prov, transientUnknownIX), err)
}
