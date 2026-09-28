package daemon

// dos_redteam_test.go — adversarial review, lens=dos (A2A-DESIGN §20 F).
//
// Finding: a node with the default-safe inbound policy (closed — it accepts
// no delegation from anyone) still runs the full receive pipeline on every
// sealed envelope a registered stranger sends it, and that pipeline replays
// the sender's carried KEL several times before step 9 refuses the message.
//
// Two problems compound:
//
//  1. No daemon-side rate limit on the hub-delivered path. Step 0's rate
//     limit is p2p-only (receive.go: "(transport modules only)"); envelopes
//     pulled from the hub mailbox go straight into receiveEnvelope. The hub
//     admits 20 envelopes/s per sender (§3.7) and AIDs are free to mint, so
//     nothing bounds how fast a stranger can make a closed node do this work.
//
//  2. The per-envelope cost is several KEL replays, not one. §3.1/§2 model
//     the cost as "a KEL is replayed on every receive, one Ed25519
//     verification per event" and cap the KEL at seal.MaxKELBytes to bound
//     it. The implementation replays the same KEL in replayedAID (step 6),
//     twice inside VerifyInnerSig (its own identity.Replay plus
//     identity.VerifyObject's), again in VerifyEncKeySet (step 8) and again
//     in the TaskDoc's Verify — ~6x the documented model. The KEL an
//     attacker packs just under the 64 KiB cap is ~185 events, so each
//     envelope a closed node refuses costs ~6 * 185 Ed25519 verifications.
//
// The test asserts the attack succeeds: the closed node refuses the message
// (proving it accepts nothing), yet paid for it, and paid several full KEL
// replays per envelope with no rate limit gating a burst.

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
)

// growSenderKEL grows base's KEL with self-signed drt events (which do not
// change the signing key) until its encoding nears the message-path byte
// cap, so the sealed envelope still passes seal.ParseKEL and reaches the
// authorization step. An attacker builds this offline.
func growSenderKEL(t *testing.T, base sender, targetBytes int) sender {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(nil)
	for {
		b, err := identity.MarshalKEL(base.ctrl.KEL())
		if err != nil {
			t.Fatal(err)
		}
		if len(b) >= targetBytes {
			break
		}
		if err := base.ctrl.Delegate(pub, 1); err != nil {
			t.Fatal(err)
		}
	}
	base.kel = base.ctrl.KEL()
	base.ksn = base.ctrl.CurrentSeq()
	base.keys = signedKeysFor(t, base.ctrl, base.ctrl.CurrentSeq())
	return base
}

func TestRedteamClosedNodeKELReplayExhaustion(t *testing.T) {
	// prov joins the test group with accept=false: policy closed, allow and
	// trust lists empty — the default-safe posture (SI-5).
	_, _, prov := registeredPair(t)
	if prov.config().inbound().Policy != PolicyClosed {
		t.Fatalf("provider is not closed: %q", prov.config().inbound().Policy)
	}

	attacker := growSenderKEL(t, newStranger(t), seal.MaxKELBytes-2000)
	kelB, _ := identity.MarshalKEL(attacker.kel)
	t.Logf("attacker KEL: %d events, %d bytes (cap %d bytes)", len(attacker.kel), len(kelB), seal.MaxKELBytes)

	// Cost baseline: one replay of the attacker KEL.
	st := time.Now()
	if _, err := identity.Replay(attacker.kel); err != nil {
		t.Fatal(err)
	}
	oneReplay := time.Since(st)

	// A burst of distinct delegations (fresh mid + ix each), exactly what a
	// stranger sends to a closed node. All are refused; none is cheaply
	// gated by any rate limiter.
	const burst = 8
	envs := make([][]byte, burst)
	for i := 0; i < burst; i++ {
		ix := "atk-" + time.Now().Format("150405.000000000") + string(rune('a'+i))
		envs[i] = craft(t, attacker, prov, seal.TypeDelegate, ix, delegateBody(t, attacker.ctrl, ix, "hi", ""), nil)
	}

	before := counter(prov, dropNotAccepting)
	st = time.Now()
	for _, env := range envs {
		r := receive(t, prov, env)
		// Attack succeeded per envelope: a closed node ran full verification
		// and only then refused with the stranger reason (not an early,
		// cheap rate-limit/structural drop).
		if r.reason != dropNotAccepting {
			t.Fatalf("expected %q (full pipeline then refuse), got %q", dropNotAccepting, r.reason)
		}
	}
	elapsed := time.Since(st)
	refused := counter(prov, dropNotAccepting) - before

	perEnvelope := elapsed / burst
	replaysPerEnvelope := float64(perEnvelope) / float64(oneReplay)
	t.Logf("closed node processed %d stranger delegations in %v (%v each) => ~%.1f full KEL replays/envelope",
		refused, elapsed, perEnvelope, replaysPerEnvelope)

	if refused != burst {
		t.Fatalf("expected all %d envelopes to reach the not-accepting refusal (no rate-limit gate); got %d", burst, refused)
	}
	// Attack succeeded: the implementation does markedly more replay work per
	// refused envelope than the documented one-replay-per-message model. 3x
	// is a conservative floor (observed ~6x); combined with the absence of a
	// daemon-side inbound rate limit, a stranger pins CPU on a node that
	// accepts nothing.
	if replaysPerEnvelope < 3 {
		t.Fatalf("expected >=3 full KEL replays per refused envelope (worse than the documented model); got %.1f", replaysPerEnvelope)
	}
}

// [redteam:F25] regression (was TestRedteamUnknownIXRedeliveryRework). A
// message (or status/result) for an interaction the node does not hold used
// to fail as class T for the 10-minute unknown-ix window whoever sent it: not
// acknowledged, never reaching step 10's replay table, so every hub
// redelivery ran the full decrypt and several KEL replays again, for up to
// ten minutes, for one envelope a stranger sent. A stranger's message is now
// answered TaskNotFound (rate limited) and acknowledged at once, and the
// refusal is remembered: a redelivery is dropped before step 6, without any
// KEL replay.
func TestRedteamUnknownIXFromAStrangerIsNotReworked(t *testing.T) {
	_, _, prov := registeredPair(t)

	attacker := growSenderKEL(t, newStranger(t), seal.MaxKELBytes-2000)

	// A message for an interaction this node has never seen.
	env := craft(t, attacker, prov, seal.TypeMessage, "no-such-ix", chatBody(t, "hello", "m1"), nil)

	first := receive(t, prov, env)
	if first.class != rxDropped || first.reason != dropUnknownIX || !first.ack() {
		t.Fatalf("first delivery: class=%d reason=%q; want a permanent %q, acknowledged", first.class, first.reason, dropUnknownIX)
	}
	const redeliveries = 3
	for i := 0; i < redeliveries; i++ {
		if r := receive(t, prov, env); r.class != rxDropped || r.reason != dropRefusedReplay {
			t.Fatalf("redelivery %d: class=%d reason=%q; want %q (no KEL replay, no second answer)",
				i, r.class, r.reason, dropRefusedReplay)
		}
	}
	if n := counter(prov, transientUnknownIX); n != 0 {
		t.Fatalf("a stranger's message was held %d times", n)
	}
	if n := counter(prov, noticeTaskNotFound); n > 1 {
		t.Fatalf("%d TaskNotFound notices for one message", n)
	}
}
