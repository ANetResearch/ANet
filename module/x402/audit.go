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

	ledger := ael.NewLedger()
	seen := map[uint64]string{}
	for _, e := range out.Entries {
		raw, derr := base64.StdEncoding.DecodeString(e.Record)
		if derr != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("seq %d: record not base64", e.Seq))
			continue
		}
		var rec ael.EventRecord
		if uerr := coredet.Unmarshal(raw, &rec); uerr != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("seq %d: record undecodable", e.Seq))
			continue
		}
		if aerr := ledger.Append(&rec, kel); aerr != nil {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("seq %d does not verify: %v", e.Seq, aerr))
			continue
		}
		seen[rec.Seq] = rec.ID
		// Totals from the signed record, not from the JSON column beside
		// it. The column is rendered by the hub; the record is what it
		// signed, and the two differing is exactly what an audit is for.
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
