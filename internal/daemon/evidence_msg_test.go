package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// ledgerPayloads returns the payloads of every event of kind on d's chain,
// oldest first.
func ledgerPayloads(t *testing.T, d *Daemon, kind string) []map[string]any {
	t.Helper()
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: kind, Limit: 100000})
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Payload)
	}
	return out
}

func payloadKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// A message between two named peers is on both chains with the same CID,
// the CID of the payload that travelled (not of the text), and nothing of
// its content. A second copy of the same message records nothing.
func TestMessageEvidenceOnBothSides(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "a task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	const reply = "canary-reply-body-51e7"
	if err := prov.SendMessage(ctx, id, reply, nil); err != nil {
		t.Fatal(err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sent := ledgerPayloads(t, prov, EvMessageSent)
	got := ledgerPayloads(t, req, EvMessageReceived)
	if len(sent) != 1 || len(got) != 1 {
		t.Fatalf("sent events %d, received events %d; want one each", len(sent), len(got))
	}
	s, r := sent[0], got[0]
	const want = "attachments,bytes,cid,interaction_id,kind,msg_id"
	if payloadKeys(s) != want || payloadKeys(r) != want {
		t.Fatalf("payload keys: sent %s, received %s; want %s", payloadKeys(s), payloadKeys(r), want)
	}
	for _, k := range []string{"cid", "bytes", "msg_id", "interaction_id", "kind"} {
		if fmt.Sprint(s[k]) != fmt.Sprint(r[k]) {
			t.Errorf("%s differs: sent %v, received %v", k, s[k], r[k])
		}
	}
	if s["interaction_id"] != id || s["kind"] != interactions.MsgText {
		t.Errorf("sent event %v", s)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), reply) {
		t.Fatal("the message body is on the chain")
	}
	if c, _ := messageEvidence(id, "", "", []byte(reply), 0); c["cid"] == s["cid"] {
		t.Fatal("the CID is the CID of the text, which anyone can compute from a guess")
	}

	// The same message sealed again (a retry of the sender): stored once,
	// recorded once.
	msgID, _ := s["msg_id"].(string)
	again := sealFrom(t, prov, req, seal.TypeMessage, id, chatBody(t, reply, msgID))
	if res := receive(t, req, again); res.class != rxAccepted {
		t.Fatalf("second copy: %+v", res)
	}
	if n := chainEvents(t, req, EvMessageReceived); n != 1 {
		t.Fatalf("the second copy was recorded again: %d events", n)
	}
}

// Messages on an interaction with a stranger (policy open, trust public) are
// counted into the summary window, not written one by one.
func TestStrangerMessagesAreAggregated(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	setPolicy(t, prov, PolicyOpen)
	id, err := stranger.Delegate(ctx, prov.AID(), "a public task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ix, err := prov.ix.Get(id); err != nil || ix.Trust != interactions.TrustPublic {
		t.Fatalf("interaction %+v %v, want trust public", ix, err)
	}
	for i := 0; i < 3; i++ {
		if err := stranger.SendMessage(ctx, id, fmt.Sprintf("more %d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "an answer", nil); err != nil {
		t.Fatal(err)
	}
	if n := countMsgs(t, prov, id); n != 5 {
		t.Fatalf("provider stored %d messages, want the task and four", n)
	}
	if a, b := chainEvents(t, prov, EvMessageReceived), chainEvents(t, prov, EvMessageSent); a != 0 || b != 0 {
		t.Fatalf("stranger messages written one by one: received %d, sent %d", a, b)
	}
	// The stranger's own chain records its messages one by one: they are
	// its own, on an interaction it started.
	if n := chainEvents(t, stranger, EvMessageSent); n != 3 {
		t.Fatalf("stranger chain has %d sent events, want 3", n)
	}

	prov.flushInboundSummary(true)
	recv := ledgerPayloads(t, prov, EvMessageReceived)
	sent := ledgerPayloads(t, prov, EvMessageSent)
	if len(recv) != 1 || len(sent) != 1 {
		t.Fatalf("summaries: received %d, sent %d; want one each", len(recv), len(sent))
	}
	if recv[0]["aggregated"] != true || fmt.Sprint(recv[0]["count"]) != "3" || fmt.Sprint(sent[0]["count"]) != "1" {
		t.Fatalf("summaries: received %v, sent %v", recv[0], sent[0])
	}
	if aids := fmt.Sprint(recv[0]["first_aids"]); !strings.Contains(aids, stranger.AID()) {
		t.Fatalf("first_aids %s does not name the stranger", aids)
	}
	if _, ok := recv[0]["cid"]; ok {
		t.Fatal("an aggregated event carries a per-message CID")
	}
}

// A public capability call leaves only the result CID, the metrics and how
// the effect was checked on the chain in the default (cid) mode, whoever
// the caller; the full mode keeps the provenance, observed state included.
func TestPublicCapabilityEvidenceModes(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	stranger := registered(t, srv.URL, "stranger")
	if err := prov.Providers().Register(ctx, quirkyProvider{}); err != nil {
		t.Fatal(err)
	}
	const capID = "sensor.temperature@aqara/th-1"
	call := func() map[string]any {
		t.Helper()
		id, err := stranger.DelegateCapability(ctx, prov.AID(), capID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		ix, err := prov.ix.Get(id)
		if err != nil || ix.Trust != interactions.TrustPublicCap {
			t.Fatalf("interaction %+v %v, want trust public_cap", ix, err)
		}
		ev := lastLedgerPayload(t, prov, EvCapabilityEffect)
		if ev["interaction_id"] != id {
			t.Fatalf("last effect is for %v, want %s", ev["interaction_id"], id)
		}
		return ev
	}

	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: capID}}); err != nil {
		t.Fatal(err)
	}
	ev := call()
	prov2, _ := ev["evidence"].(map[string]any)
	if got := payloadKeys(prov2); got != "native_ack,protocol,verify_trust" {
		t.Fatalf("cid mode keeps provenance keys %q", got)
	}
	if ev["result_cid"] == nil || ev["result_cid"] == "" || ev["metrics"] == nil {
		t.Fatalf("cid mode lost the result CID or the metrics: %v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), "2212.93") || strings.Contains(string(raw), "observed_state") {
		t.Fatalf("cid mode recorded the observed state: %s", raw)
	}

	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: capID, Evidence: EvidenceFull}}); err != nil {
		t.Fatal(err)
	}
	ev = call()
	full, _ := ev["evidence"].(map[string]any)
	if full["observed_state"] != "2212.93" || full["quirk"] != "aqara.temp.scale100" {
		t.Fatalf("full mode lost the provenance: %v", full)
	}

	// A caller on the allow list calling a public capability is admitted
	// at §5.2 row 2 like anyone else (call checks trust public_cap), so the
	// capability's mode applies to it too.
	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: capID}}); err != nil {
		t.Fatal(err)
	}
	allowPeers(t, prov, stranger.AID())
	ev = call()
	if raw, _ := json.Marshal(ev); strings.Contains(string(raw), "observed_state") {
		t.Fatalf("an allowed caller of a public capability got the full record in the cid mode: %s", raw)
	}

	if err := prov.SetPublicCapabilities([]PublicCapability{{ID: capID, Evidence: "none"}}); err == nil {
		t.Fatal("an unknown evidence mode was accepted")
	}
}

// The retention sweep deletes public_cap calls that ended more than seven
// days before the start of the day, and says how many on the chain; a
// sweep that deletes nothing writes nothing.
func TestRetentionSweepPrunesOldPublicCapCalls(t *testing.T) {
	d := newBareDaemon(t)
	now := time.Date(2026, 9, 27, 15, 0, 0, 0, time.UTC).UnixMilli()
	cutoff := retentionCutoff(now)
	if want := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC).UnixMilli(); cutoff != want {
		t.Fatalf("cutoff %s, want %s", time.UnixMilli(cutoff).UTC(), time.UnixMilli(want).UTC())
	}
	d.ix.SetClock(func() int64 { return cutoff - 1 })
	if err := d.ix.Create(interactions.New{ID: "ix_pc", Role: interactions.RoleInbound, PeerAID: "did:anet:x",
		Goal: "c", Trust: interactions.TrustPublicCap, IsCapability: true}); err != nil {
		t.Fatal(err)
	}
	if err := d.ix.SetResult("ix_pc", []byte("r"), "cid", nil, interactions.VerificationVerified); err != nil {
		t.Fatal(err)
	}
	d.ix.SetClock(nil)
	d.pruneRetentionAt(now)
	if _, err := d.ix.Get("ix_pc"); err == nil {
		t.Fatal("the old public_cap call is still stored")
	}
	evs := ledgerPayloads(t, d, EvInteractionPruned)
	if len(evs) != 1 || fmt.Sprint(evs[0]["interactions"]) != "1" || evs[0]["trust"] != interactions.TrustPublicCap {
		t.Fatalf("prune evidence %v", evs)
	}
	d.pruneRetentionAt(now + 3600_000)
	if n := chainEvents(t, d, EvInteractionPruned); n != 1 {
		t.Fatalf("an empty sweep wrote evidence (%d events)", n)
	}
}

// A message id is recorded as it is when it looks like an identifier, and
// as its CID otherwise: a peer chooses its ids, and the chain carries no
// content.
func TestEvidenceMsgIDIsAnIdentifier(t *testing.T) {
	mine, err := newMessageID()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mine, "", "a2a:0b7e-11.x"} {
		if got := evidenceMsgID(id); got != id {
			t.Errorf("evidenceMsgID(%q) = %q, want it unchanged", id, got)
		}
	}
	for _, id := range []string{"the secret plan is in here", strings.Repeat("a", maxEvidenceMsgID+1)} {
		got := evidenceMsgID(id)
		if !strings.HasPrefix(got, "cid:") || strings.Contains(got, "secret") || len(got) > maxEvidenceMsgID {
			t.Errorf("evidenceMsgID(%q) = %q, want the CID of the id", id, got)
		}
	}
}
