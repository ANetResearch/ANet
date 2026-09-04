//go:build !no_x402

package x402

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
)

// Checking what the hub says about the money.
//
// Two things a node can do without asking anyone's permission:
//
//	Reconcile   compare the hub's ledger for this account against this
//	            node's own signed record of what it authorized and what
//	            it was paid. Both sides already exist; nothing but the
//	            comparison was missing.
//	Audit       fetch the hub's issuance chain, verify the signatures and
//	            the links, and check it against heads this node saw
//	            before. A hub that rewrote its supply history contradicts
//	            a record this node already holds.
//
// Neither prevents anything. A hub is the custodian of its ledger and can
// issue credit at will — that is what being the issuer means. What these
// establish is that it cannot do so retroactively or silently, provided
// somebody looked.

// EvIssuanceHeadSeen records a hub issuance head this node observed.
//
// On this node's own chain, which is what makes it evidence rather than a
// cache: the hub cannot alter it, and it is signed and ordered alongside
// everything else this node did.
const EvIssuanceHeadSeen = "anet.issuance.head_seen"

// IssuanceEntry mirrors what the hub serves.
type IssuanceEntry struct {
	Seq    uint64 `json:"seq"`
	ID     string `json:"id"`
	PrevID string `json:"prev_id"`
	Kind   string `json:"kind"`
	AID    string `json:"aid"`
	Amount int64  `json:"amount"`
	Reason string `json:"reason,omitempty"`
	At     int64  `json:"at"`
	Record string `json:"record"`
}

// AuditReport is what an audit found.
type AuditReport struct {
	Hub      string   `json:"hub"`
	ChainDID string   `json:"chain_did"`
	Entries  int      `json:"entries"`
	HeadSeq  uint64   `json:"head_seq"`
	HeadID   string   `json:"head_id"`
	Issued   int64    `json:"issued"`
	Retired  int64    `json:"retired"`
	Verified bool     `json:"verified"`
	Problems []string `json:"problems,omitempty"`
	// Witnessed is true when this node recorded the head it just saw.
	Witnessed bool `json:"witnessed,omitempty"`
}

// AuditIssuance fetches and verifies the hub's supply chain.
//
// Verification here means: every record's signature checks against the
// hub's published key history, the records link, and none of them
// contradicts a head this node recorded earlier. The totals are
// recomputed from the signed records rather than read from the hub's
// summary, because the summary is prose the hub writes and the records
// are what it signed.
func (m *Module) auditIssuance(ctx context.Context) (AuditReport, error) {
	rep := AuditReport{Hub: m.hubURL(), ChainDID: m.hubAID()}
	if rep.Hub == "" {
		return rep, fmt.Errorf("x402: no hub configured")
	}
	kel, err := m.hubKEL()
	if err != nil {
		return rep, err
	}
	var out struct {
		ChainDID string          `json:"chain_did"`
		Entries  []IssuanceEntry `json:"entries"`
		HeadID   string          `json:"head_id"`
		HeadSeq  uint64          `json:"head_seq"`
	}
	if err := m.getJSON(ctx, "/x402/issuance?from=0", &out); err != nil {
		return rep, err
	}
	if out.ChainDID != "" && out.ChainDID != rep.ChainDID {
		return rep, fmt.Errorf("x402: the hub served a chain for %s, not for itself (%s)",
			out.ChainDID, rep.ChainDID)
	}
	rep.Entries, rep.HeadID, rep.HeadSeq = len(out.Entries), out.HeadID, out.HeadSeq

	// Every record is decoded and its signature checked here, and the
	// linking is checked separately below.
	//
	// This used to be done by feeding each record to ael.Ledger.Append and
	// treating a nil return as "this record is on the chain". It is not
	// what that return value says. ael's Append is built for out-of-order
	// gossip import: a record whose seq is past the head is verified,
	// parked in the staging buffer, and reported as accepted, because the
	// predecessor is expected to arrive later. So a hub that deleted an
	// entry from the middle of its issuance chain and served the rest
	// unaltered got seq 0 accepted as genesis and everything after the gap
	// accepted into staging, and the audit reported verified:true.
	// Deleting an entry is the most direct way a hub rewrites its supply
	// history, which is the one thing this audit exists to find.
	//
	// The cost of checking here is that this module now states the linking
	// rules itself instead of borrowing ael's. They are part of the
	// evidence wire contract (genesis sentinel, seq+1, prev_id = the id
	// before it) rather than ael's private choices, and TestAuditRejects*
	// build their chains with ael's own signing so a divergence shows up.
	recs := make([]*ael.EventRecord, 0, len(out.Entries))
	seen := map[uint64]string{}
	sound := true
	for _, e := range out.Entries {
		raw, derr := base64.StdEncoding.DecodeString(e.Record)
		if derr != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("seq %d: record not base64", e.Seq))
			sound = false
			continue
		}
		var rec ael.EventRecord
		if uerr := coredet.Unmarshal(raw, &rec); uerr != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("seq %d: record undecodable", e.Seq))
			sound = false
			continue
		}
		if verr := rec.Verify(kel); verr != nil {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("seq %d does not verify: %v", e.Seq, verr))
			sound = false
			continue
		}
		// Verify authenticates the signer against the hub's key history
		// but says nothing about which chain the record belongs to. A
		// record the hub signed for some other chain is not part of its
		// issuance history and must not be counted into the supply.
		if rep.ChainDID != "" && rec.ChainDID != rep.ChainDID {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"seq %d is a record on chain %s, not on this hub's own chain",
				rec.Seq, rec.ChainDID))
			sound = false
			continue
		}
		recs = append(recs, &rec)
		seen[rec.Seq] = rec.ID
		// Totals come from the signed record's own event type and
		// payload, never from the JSON columns beside it. Those are
		// rendered by the hub and are not what it signed; a hub whose
		// summary disagrees with its records is precisely what an audit
		// should surface rather than adopt.
		amount, _ := payloadAmount(rec.Payload)
		switch rec.EventType {
		case evCreditIssued:
			rep.Issued += amount
		case evCreditRetired:
			rep.Retired += amount
		}
	}

	// Linking is checked only over records that all decoded and verified.
	// A record that did not decode leaves a hole whose seq is known only
	// from the hub's own JSON column, so a gap reported across it would be
	// naming the hub for something that could equally be damage in
	// transit. The undecodable record is already a problem in its own
	// right, so nothing is lost by not compounding it.
	if sound {
		rep.Problems = append(rep.Problems, checkChainLinks(recs)...)
	}

	// Against what this node saw before. A hub that rewrote its supply
	// history contradicts a record already on this node's chain, which
	// the hub cannot reach.
	for _, prior := range m.priorHeads() {
		if got, ok := seen[prior.Seq]; ok && got != prior.HeadID {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"seq %d was %s when this node saw it at %s, and is now %s — "+
					"the hub rewrote its issuance history",
				prior.Seq, short(prior.HeadID),
				time.UnixMilli(prior.ObservedAt).UTC().Format(time.RFC3339), short(got)))
		}
	}
	rep.Verified = len(rep.Problems) == 0
	return rep, nil
}

// checkChainLinks reports the ways a served issuance chain fails to be a
// chain: a first entry that is not the genesis record, a seq that does not
// follow the one before it, or a prev_id that does not name the record
// before it. The records handed to it have already been decoded and had
// their signatures checked.
//
// Kept apart from signature verification because the two answer different
// questions, and the case this exists for passes the first while failing
// the second: when a hub drops one of its own entries, every record it
// still serves is correctly signed and only the shape of what is left
// shows what happened.
//
// The chain was requested with from=0, so a first entry that is not the
// genesis record is the hub declining to serve the start of its history
// rather than a window the caller asked for.
func checkChainLinks(recs []*ael.EventRecord) []string {
	if len(recs) == 0 {
		return nil
	}
	// Order first, and nothing else if it is wrong. The genesis and gap
	// checks below read the served order as the chain order; run against a
	// reordered or duplicated page they would report entries missing where
	// nothing is missing, which is a worse answer than saying the page
	// cannot be followed.
	var problems []string
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		switch {
		case cur.Seq > prev.Seq:
			continue
		case cur.Seq == prev.Seq && cur.ID == prev.ID:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served twice, so its amount is counted twice in the totals below",
				cur.Seq))
		case cur.Seq == prev.Seq:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served as both %s and %s: the hub holds two different records "+
					"for one position on its chain",
				cur.Seq, short(prev.ID), short(cur.ID)))
		default:
			problems = append(problems, fmt.Sprintf(
				"seq %d was served after seq %d: the entries are not in chain order, "+
					"so what the hub holds cannot be followed from here",
				cur.Seq, prev.Seq))
		}
	}
	if len(problems) > 0 {
		return problems
	}
	if first := recs[0]; first.Seq != 0 || first.PrevID != ael.GenesisPrev() {
		problems = append(problems, fmt.Sprintf(
			"the chain begins at seq %d with prev_id %s rather than at the genesis entry: "+
				"the hub was asked for its history from seq 0 and did not serve the start of it",
			first.Seq, short(first.PrevID)))
	}
	for i := 1; i < len(recs); i++ {
		prev, cur := recs[i-1], recs[i]
		if cur.Seq > prev.Seq+1 {
			problems = append(problems, fmt.Sprintf(
				"the chain jumps from seq %d to seq %d: %s missing between them, "+
					"so the totals below are of what the hub chose to serve",
				prev.Seq, cur.Seq, countEntries(cur.Seq-prev.Seq-1)))
			// The prev_id of the entry after a gap names a record that was
			// not served, so checking the link here would only restate the
			// gap under a second heading.
			continue
		}
		if cur.PrevID != prev.ID {
			problems = append(problems, fmt.Sprintf(
				"seq %d names %s as the entry before it, but seq %d is %s",
				cur.Seq, short(cur.PrevID), prev.Seq, short(prev.ID)))
		}
	}
	return problems
}

// countEntries keeps the gap message readable for a gap of one, which is
// the common case: one entry removed.
func countEntries(n uint64) string {
	if n == 1 {
		return "1 entry is"
	}
	return fmt.Sprintf("%d entries are", n)
}

// priorHead is a head this node recorded earlier.
type priorHead struct {
	Seq        uint64
	HeadID     string
	ObservedAt int64
}

// priorHeads reads back what this node has recorded about the hub's
// chain.
func (m *Module) priorHeads() []priorHead {
	if m.seam == nil {
		return nil
	}
	var out []priorHead
	for _, p := range m.seam.ReadEvidence(EvIssuanceHeadSeen, 1000) {
		id, _ := p["head_id"].(string)
		if id == "" {
			continue
		}
		out = append(out, priorHead{
			Seq: uint64(asInt64(p["seq"])), HeadID: id,
			ObservedAt: asInt64(p["observed_at"]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// WitnessHub records the hub's current issuance head on this node's own
// chain, and optionally signs and submits an attestation.
//
// Off by default, and that is deliberate. A node may be a trimmed build
// that offers nothing and only talks to its hub; making such a node do
// work for the network is not something it asked for. A node that does
// witness gets the most direct benefit — it holds independent evidence
// about the ledger its own balance lives on.
func (m *Module) WitnessHub(ctx context.Context) (uint64, string, error) {
	if m.seam == nil {
		return 0, "", fmt.Errorf("x402: no hub, so nothing to witness")
	}
	var head struct {
		ChainDID string `json:"chain_did"`
		Seq      uint64 `json:"seq"`
		HeadID   string `json:"head_id"`
	}
	if err := m.getJSON(ctx, "/x402/issuance/head", &head); err != nil {
		return 0, "", err
	}
	if head.HeadID == "" {
		return 0, "", nil // nothing issued yet
	}
	if head.ChainDID != m.hubAID() {
		return 0, "", fmt.Errorf("x402: the hub served a head for %s, not itself", head.ChainDID)
	}
	// On this node's own chain first. That copy is the one the hub cannot
	// reach, and it is the reason any of this is worth doing.
	if err := m.record(EvIssuanceHeadSeen, map[string]any{
		"chain_did": head.ChainDID, "seq": head.Seq, "head_id": head.HeadID,
		"observed_at": time.Now().UnixMilli(),
	}); err != nil {
		return 0, "", err
	}
	// Then the signed attestation, offered back to the hub so a reader
	// has somewhere to start looking. Best-effort: the copy that matters
	// is already recorded.
	a := &ael.HeadAttestation{
		ChainDID: head.ChainDID, Seq: head.Seq, HeadID: head.HeadID,
		ObservedAt: time.Now().UnixMilli(),
	}
	pre, err := a.CanonicalPreimage()
	if err == nil {
		sig, seq := m.seam.Sign(pre)
		a.Envelope = &aobj.Envelope{SignerAID: m.AID(), KeyStateSeq: seq,
			Alg: aobj.AlgEdDSA, Sig: sig}
		if raw, merr := a.Marshal(); merr == nil {
			m.submitAttestation(ctx, raw)
		}
	}
	return head.Seq, head.HeadID, nil
}

func (m *Module) submitAttestation(ctx context.Context, raw []byte) {
	body, err := json.Marshal(map[string]string{
		"attestation": base64.StdEncoding.EncodeToString(raw)})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.hubURL()+"/x402/witness", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req); err == nil {
		resp.Body.Close()
	}
}

// getJSON fetches from this node's hub.
func (m *Module) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.hubURL()+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: hubCallTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("x402: hub answered %s for %s", resp.Status, path)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

// The hub's event types, duplicated as constants rather than imported:
// this module must not depend on the hub package, and the strings are the
// wire contract either way.
const (
	evCreditIssued  = "anet.credit.issued"
	evCreditRetired = "anet.credit.retired"
)

// payloadAmount reads the amount out of a chain record's payload.
//
// The payload is `any` and its concrete shape depends on how it was
// decoded: CBOR yields map[any]any, JSON yields map[string]any, and the
// numbers arrive as whichever width the encoder chose. Assuming one shape
// silently produced zero for every entry, which made an audit report a
// supply of nothing and call it verified.
func payloadAmount(p any) (int64, string) {
	switch m := p.(type) {
	case map[string]any:
		return asInt64(m["amount"]), ""
	case map[any]any:
		return asInt64(m["amount"]), ""
	}
	return 0, ""
}

// asInt64 coerces whatever numeric type a decoder produced.
//
// Needed in more than one place for the same reason: values that pass
// through CBOR or JSON come back as int64, uint64 or float64 depending on
// the encoder, the width, and the sign. A single type assertion fails
// silently and yields zero, which reads as a real value.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func short(id string) string {
	if len(id) > 14 {
		return id[:10] + "…"
	}
	return id
}

// AuditIssuance and Reconcile satisfy module.Payer. They return `any` so
// the kernel does not have to know the report shapes: it carries them to
// a caller and never reads them, and a typed contract would put this
// module's vocabulary in the kernel for no gain.
func (m *Module) AuditIssuance(ctx context.Context) (any, error) { return m.auditIssuance(ctx) }
func (m *Module) Reconcile(ctx context.Context) (any, error)     { return m.reconcile(ctx) }
