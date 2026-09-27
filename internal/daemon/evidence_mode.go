package daemon

// evidence_mode.go is what the evidence chain keeps of a public capability
// call (A2A-DESIGN §15, §21 item 4; decision Q15), and how long the
// interaction store keeps the call itself.
//
// A public capability answers anyone. Its anet.capability.effect event
// records who called what, the outcome, the metrics and the result CID in
// both modes. In the full mode it also records the effect's provenance,
// whose observed_state is the capability's whole answer: the chain is
// append-only, so it would keep a copy of every stranger's result for as
// long as the node exists. The cid mode, the default, keeps from the
// provenance only what describes how the effect was checked (protocol,
// verify_trust, latency_ms, native_ack). The caller holds the result and
// the receipt signed over its CID; anyone can recompute the CID from the
// result, so the chain does not need the result to back the receipt.
//
// The mode applies to calls under trust public_cap only. A caller this node
// named (allow or trust list) gets the full record whatever the mode.

import (
	"fmt"
	"log"
	"time"

	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Evidence modes of a public capability.
const (
	EvidenceCID  = "cid"
	EvidenceFull = "full"
)

func validEvidenceMode(p PublicCapability) error {
	switch p.Evidence {
	case "", EvidenceCID, EvidenceFull:
		return nil
	}
	return fmt.Errorf("anet: inbound.public_capabilities %q: evidence %q is not one of cid, full", p.ID, p.Evidence)
}

// cidModeProvenance is the part of an effect's provenance the cid mode
// keeps: how the effect was checked, nothing of what it produced.
var cidModeProvenance = []string{"protocol", "verify_trust", "latency_ms", "native_ack"}

// effectEvidence is the provenance the chain records for a capability call
// on ix: all of it, or in the cid mode only the keys above. It returns nil
// when nothing is to be recorded.
func (d *Daemon) effectEvidence(ix *interactions.Interaction, capID string, prov map[string]any) map[string]any {
	if prov == nil {
		return nil
	}
	if ix == nil || ix.Trust != interactions.TrustPublicCap || d.publicEvidenceMode(capID) == EvidenceFull {
		return prov
	}
	out := map[string]any{}
	for _, k := range cidModeProvenance {
		if v, ok := prov[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// publicEvidenceMode is the evidence mode configured for a public
// capability. A capability no longer public (the list changed while the
// call ran) takes the default.
func (d *Daemon) publicEvidenceMode(capID string) string {
	if pc, ok := d.config().inbound().publicCapability(capID); ok {
		return pc.Evidence
	}
	return EvidenceCID
}

// --- retention of public_cap interactions (Q15) ---

// EvInteractionPruned records one retention sweep that deleted
// interactions: how many, with how many messages and attachments, and the
// cutoff. It is written only when the sweep deleted something.
const EvInteractionPruned = evtypes.InteractionPruned

// publicCapRetention is how long a finished public_cap interaction is kept.
const publicCapRetention = 7 * 24 * time.Hour

// retentionCutoff is the cutoff of the sweep run at now (unix ms): the
// start of now's UTC day, less the retention. Rounding to the day makes the
// sweep a daily one whichever hour it runs at: a later run on the same day
// has the same cutoff and finds nothing new.
func retentionCutoff(now int64) int64 {
	const day = int64(24 * time.Hour / time.Millisecond)
	return now - now%day - publicCapRetention.Milliseconds()
}

// pruneRetention deletes the public_cap interactions that reached a
// terminal state before the cutoff, with their messages and attachments,
// and records the counts on the chain. The receive maintenance loop calls
// it every hour.
func (d *Daemon) pruneRetention() { d.pruneRetentionAt(int64(d.nowMS())) }

// pruneRetentionAt is pruneRetention at now (unix ms).
func (d *Daemon) pruneRetentionAt(now int64) {
	if d.ix == nil {
		return
	}
	cutoff := retentionCutoff(now)
	n, err := d.ix.PruneTerminal(interactions.TrustPublicCap, cutoff)
	if err != nil {
		log.Printf("anet: retention: prune public_cap interactions: %v", err)
		return
	}
	if n.Interactions == 0 {
		return
	}
	log.Printf("anet: retention: deleted %d public_cap interactions finished before %s (%d messages, %d attachments)",
		n.Interactions, time.UnixMilli(cutoff).UTC().Format(time.RFC3339), n.Messages, n.Attachments)
	if d.ledger == nil {
		return
	}
	if _, err := d.ledger.Append(EvInteractionPruned, map[string]any{
		"trust": interactions.TrustPublicCap, "before": cutoff,
		"retention_days": int(publicCapRetention / (24 * time.Hour)),
		"interactions":   n.Interactions, "messages": n.Messages, "attachments": n.Attachments,
	}); err != nil {
		log.Printf("anet: retention evidence: %v", err)
	}
}
