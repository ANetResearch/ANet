package daemon

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
)

// KEL-9 (EXE-13 note, join defect 1): a node that registered and was
// refused later, asked to join again, starts over: what it shows is the
// answer to that attempt — a temporary failure is hub_retrying, not a
// refusal with a retry scheduled behind it.
func TestARefusedJoinStartsOver(t *testing.T) {
	d, srv, fh := opsNode(t)
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 403, body: map[string]any{"reason": "banned", "error": "no"}}}
	fh.mu.Unlock()
	if err := d.RegisterWithHub(context.Background(), srv.URL, "n", nil); err == nil {
		t.Fatal("setup: the hub's refusal was not seen")
	}
	if r := d.joinSnapshot(); r.State != JoinRefused || r.RegisteredAt == 0 {
		t.Fatalf("setup: %+v, want refused after a registration", r)
	}
	fh.mu.Lock()
	fh.registerScript = []fakeRegisterAnswer{{status: 503, body: map[string]any{"reason": "internal", "error": "down"}}}
	fh.mu.Unlock()
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL, ""))
	if code != http.StatusOK || out["state"] != JoinRetrying || out["reason"] != reasonRetrying {
		t.Fatalf("hub-register again after the refusal, the hub down: %d %v, want retrying", code, out)
	}
	if r := d.joinSnapshot(); r.NextAttemptAt == 0 {
		t.Fatalf("no retry is scheduled: %+v", r)
	}
}

// KEL-9 (join defect 2; ux §1.4 F): a registration this node's publication
// screen keeps back (INV-1, INV-2) is refused with hub_publication_blocked
// and not retried; nothing left this node, so the designation built for it
// is dropped.
func TestPublicationBlockedIsRefusedNotRetried(t *testing.T) {
	srv := newFakeHub(t)
	d := opsDaemon(t)
	d.modules = append(d.modules, confidentialModule{"org-secret-7"})
	code, out := controlCall(t, d.hHubRegister, "/hub-register",
		hubRegisterBody(srv.URL, `,"name":"agent of org-secret-7","next_digest":"`+b64std(kelDigest(2))+`"`))
	if code != http.StatusOK || out["state"] != JoinRefused || out["reason"] != reasonPublicationBlocked {
		t.Fatalf("a registration the screen keeps back: %d %v", code, out)
	}
	want := a2ashape.Sentence(reasonPublicationBlocked, a2ashape.Lang(), a2ashape.Args{"hub": srv.URL})
	if out["message"] != want || out["next"] != "anet hub-register "+srv.URL {
		t.Fatalf("the sentence: %v", out)
	}
	if r := d.joinSnapshot(); r.NextAttemptAt != 0 {
		t.Fatalf("a retry is scheduled for a registration the screen keeps back: %+v", r)
	}
	if _, pending := d.self.Pending(); pending {
		t.Fatal("the designation of a registration that never left is still pending")
	}
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	n := len(fh.registerKELs)
	fh.mu.Unlock()
	if n != 0 {
		t.Fatalf("the hub got %d registrations", n)
	}
	// The name fixed, the same command joins.
	code, out = controlCall(t, d.hHubRegister, "/hub-register",
		hubRegisterBody(srv.URL, `,"name":"agent","next_digest":"`+b64std(kelDigest(2))+`"`))
	if code != http.StatusOK || out["state"] != JoinRegistered {
		t.Fatalf("the join with the name fixed: %d %v", code, out)
	}
}

// KEL-9 (join defects 3, 4; LEC AD-8): hub-leave whose deregistration gets
// no answer is 502 hub_unreachable.leave and changes nothing; hub-leave of
// a hub this node cannot tell from its witness — no join record names it
// and it does not answer — is refused the same way; the witness hub is
// never left, also when only its answer now says it is the witness.
func TestHubLeaveWhenTheHubCannotBeReachedOrTold(t *testing.T) {
	// No witness: the deregistration's answer is all that matters.
	srv := newFakeHub(t)
	plain := newPlainTestDaemon(t, srv.URL, true)
	if err := plain.HubRegister(context.Background(), srv.URL, "n", nil); err != nil {
		t.Fatal(err)
	}
	cfgBefore := mustRead(t, plain.layout.ConfigPath())
	joinBefore := mustRead(t, plain.layout.hubJoinPath())
	srv.Close()
	code, out := controlCall(t, plain.hHubLeave, "/hub-leave", `{"hub":"`+srv.URL+`"}`)
	if code != http.StatusBadGateway || out["reason"] != "hub_unreachable" ||
		out["error"] != a2ashape.Sentence(reasonHubUnreachableLeave, a2ashape.Lang(), a2ashape.Args{"hub": srv.URL}) {
		t.Fatalf("hub-leave of a hub that does not answer: %d %v", code, out)
	}
	if string(mustRead(t, plain.layout.ConfigPath())) != string(cfgBefore) ||
		string(mustRead(t, plain.layout.hubJoinPath())) != string(joinBefore) {
		t.Fatal("hub-leave that left nothing changed config.json or hub_join.json")
	}

	// A witness: which hub is the target?
	d, wsrv, _ := opsNode(t)
	if code, out := controlCall(t, d.hHubLeave, "/hub-leave", `{"hub":"`+wsrv.URL+`"}`); code != http.StatusConflict ||
		out["reason"] != reasonWitnessHubLeave {
		t.Fatalf("hub-leave of the witness: %d %v", code, out)
	}
	if code, out := controlCall(t, d.hHubLeave, "/hub-leave", `{"hub":"`+deadHubURL+`"}`); code != http.StatusBadGateway ||
		out["reason"] != "hub_unreachable" {
		t.Fatalf("hub-leave of a hub that cannot be told: %d %v", code, out)
	}
	// The join record lost, the witness answering: its answer tells.
	if err := removeJoinRecord(d.layout); err != nil {
		t.Fatal(err)
	}
	d.join.mu.Lock()
	d.join.rec = nil
	d.join.mu.Unlock()
	d.hubIDMu.Lock()
	d.hubIDs = nil
	d.hubIDMu.Unlock()
	if code, out := controlCall(t, d.hHubLeave, "/hub-leave", `{"hub":"`+wsrv.URL+`"}`); code != http.StatusConflict ||
		out["reason"] != reasonWitnessHubLeave {
		t.Fatalf("hub-leave of the witness, its join record lost: %d %v", code, out)
	}
}

// KEL D-45, D-35 (the daemon's part of `anet identity recover`): a new data
// directory holding the KEL and the material of an emergency rot — no
// current key — publishes the rot when it starts, runs E1–E5, and records
// the rotation as a recover; the node the identity was taken from, polling
// with its retired key, stops (J3).
func TestRecoverPublishesAndTheOldNodeStops(t *testing.T) {
	noCheckLimit(t)
	old, srv, _ := opsNode(t)
	waitIdentity(t, old, IdentityOK)
	kel, err := identity.MarshalKEL(old.self.Accepted())
	if err != nil {
		t.Fatal(err)
	}
	f := &identity.IdentityFile{T: identity.IdentityFileType, KEL: kel, State: identity.StateOK,
		Pending: &identity.Pending{Kind: identity.PendingRot, Revealed: kelSeed(2), Next: kelDigest(3), HubURL: srv.URL,
			Emergency: true, Since: uint64(nowMillis())}}
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "hub_url": srv.URL})
	if err := os.WriteFile(NewLayout(root).IdentityPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	n.stopRelayLoop()
	waitIdentity(t, n, IdentityOK)
	if got := rotations(t, n); len(got) != 1 || got[0] != rotationRecover {
		t.Fatalf("anet.identity.rotated of the recovered node: %v", got)
	}
	if s := n.IdentityStatus(); s.LastRotation == nil || s.LastRotation.Mode != rotationRecover {
		t.Fatalf("last_rotation of the recovered node: %+v", s.LastRotation)
	}
	if err := old.pollOnce(context.Background()); err == nil {
		t.Fatal("the old node polled with its retired key")
	}
	waitIdentity(t, old, IdentityStopped)
}

// CORE-E5 (SS-9 review): the seed of a proof-of-work challenge is base64url
// in its one spelling; white space in it is refused, not skipped.
func TestPoWSeedRefusesWhiteSpace(t *testing.T) {
	seed := hubapi.B64(make([]byte, 32))
	if c := parseChallenge([]byte(`{"seed":"` + seed + `","bits":4}`)); c == nil {
		t.Fatal("a well-formed challenge was refused")
	}
	for _, s := range []string{seed[:10] + `\n` + seed[10:], seed[:10] + `\r\n` + seed[10:], " " + seed} {
		if c := parseChallenge([]byte(`{"seed":"` + s + `","bits":4}`)); c != nil {
			t.Fatalf("a seed with white space was taken: %q", s)
		}
	}
}
