package daemon

// Red-team PoCs for SI-4 (lens si4): a sender authenticated as the peer of
// an interaction is still bound to what it was admitted for. Each test
// PASSES when the attack SUCCEEDS, i.e. when the defect is present.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"
)

// SI4-RT-3. seal/limits.go sizes MaxKELEvents/MaxKELBytes on the premise
// that "a KEL is replayed on every receive, one Ed25519 verification per
// event, before the sender is known". The receive path actually replays
// the carried KEL several times per envelope (resolveSenderKEL,
// VerifyInnerSig + identity.VerifyObject, VerifyInnerKeys ->
// VerifyEncKeySet, and again for nested objects), so a free stranger AID
// that pads its own KEL with rotations of its own keys to just under 64 KiB
// makes each envelope cost tens of times more than a normal one. The hub
// lets one sender post 20/s and the poll loop handles envelopes one at a
// time under pollMu, so one AID can occupy the victim's receive loop.
func TestRedteamSI4_PaddedKELMakesEachEnvelopeExpensive(t *testing.T) {
	prov := newTestDaemon(t, "", false)
	measure := func(extra int) (time.Duration, int) {
		c := witnessedIdentity(t)
		for i := 0; i < extra; i++ {
			if err := identitytest.Rotate(c, identitytest.InceptTS+2+uint64(i)); err != nil {
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
	big, size := measure(150)
	if size > seal.MaxKELBytes {
		t.Fatalf("setup: KEL %d bytes over the cap", size)
	}
	ratio := float64(big) / float64(small)
	t.Logf("2-event KEL: %s/envelope; 152-event KEL (%d bytes): %s/envelope; x%.0f; 20 env/s from one AID = %s of receive work per second",
		small, size, big, ratio, 20*big)
	if ratio < 20 {
		t.Fatalf("attack failed: ratio only %.1f", ratio)
	}
}

// SI4-RT-4, fixed by engine ENG-V10. §3.6 step 9 used to hold (not
// acknowledge) an authenticated message for an ix this node does not know
// while now - inner.ts <= 10 min. inner.ts is the sender's choice up to
// now + 5 min (step 5), so a stranger held each message ~15 min; the held
// envelopes stayed in the hub mailbox against the per-recipient quota, so
// one free AID kept an ONLINE victim's mailbox full and every other sender
// got 507. There is no window now: the victim refuses them in the round
// that reads them (task_not_found), and the mailbox stays open.
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
		injectEnvelope(t, srv, prov.AID(), env)
	}
	// The victim is online and polls once.
	prov.setClock(func() uint64 { return base })
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	held := 0
	for _, m := range fake.mailbox {
		if m.toAID == prov.AID() {
			held++
		}
	}
	fake.mu.Unlock()
	if held != 0 || counter(prov, dropUnknownIX) != quota {
		t.Fatalf("the victim held %d stranger envelopes (%d refused task_not_found)", held, counter(prov, dropUnknownIX))
	}
	// A legitimate, allowed peer still reaches the victim through the hub.
	injectEnvelope(t, srv, prov.AID(), sealFrom(t, req, prov, seal.TypeMessage, "ix_whatever", chatBody(t, "hello", "")))
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("the legitimate envelope did not get into the mailbox (%d there)", n)
	}
}
