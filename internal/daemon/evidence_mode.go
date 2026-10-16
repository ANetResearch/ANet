package daemon

// evidence_mode.go is what the evidence chain keeps of a capability call
// (A2A-DESIGN §15, §21 item 4; CUX-F14), and how long this node keeps the
// content of a task once it has ended (content retention, at the end of the
// file).
//
// The anet.capability.effect event records who called what, the outcome,
// the metrics and the result CID. Of the effect's provenance it keeps only
// what describes how the effect was checked (protocol, verify_trust,
// latency_ms, native_ack), never what it produced: the chain is
// append-only, so a copy of the observed state would stay as long as the
// node exists. The caller holds the result and the receipt signed over its
// CID; anyone can recompute the CID from the result, so the chain does not
// need the result to back the receipt. A discard capability records less
// still (capability.go, discardEvidence).

import (
	"fmt"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// cidModeProvenance is the part of an effect's provenance the chain
// keeps: how the effect was checked, nothing of what it produced.
var cidModeProvenance = []string{"protocol", "verify_trust", "latency_ms", "native_ack"}

// effectEvidence is the provenance the chain records for a capability
// call (FS-L12, CUX-F14): the keys above for a public_cap call, whoever the
// caller; for any other call all of it, except that the observed state,
// which is what the capability read or produced, is kept as its CID. It
// returns nil when nothing is to be recorded.
func (d *Daemon) effectEvidence(ix *interactions.Interaction, prov map[string]any) map[string]any {
	if prov == nil {
		return nil
	}
	out := map[string]any{}
	if ix != nil && ix.Trust != interactions.TrustPublicCap {
		for k, v := range prov {
			out[k] = v
		}
		if s, ok := out["observed_state"].(string); ok {
			delete(out, "observed_state")
			if cid, err := anetcid.SumRaw([]byte(s)); err == nil {
				out["observed_state_cid"] = cid
			}
		}
		return out
	}
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

// --- content retention (forward-secrecy §4.6, FS-L5 to FS-L11) ---
//
// Every maintenance period (S, keyMaintenance) the daemon, at the clock
// gate's level of the period:
//
//   - deletes the finished tasks past the retention of their class
//     (pruneRetentionAt; interactions.PruneTerminal decides), with their
//     data keys, and records one anet.interaction.pruned per class;
//   - removes the exec work directories of the tasks that have ended
//     (pruneEndedWorkDirs, FS-L11);
//   - truncates the store's write-ahead log (checkpointStore, FS-L8), so a
//     key deleted in the period — a task's interaction key at its end, a
//     data key with its task or held item — is gone from every file.
//
// The operator changes the setting with `anet retention set`, which asks on
// a terminal first and calls POST /retention (hRetention): the daemon
// writes config.json and sweeps at once (SetContentRetention).

// EvInteractionPruned records one class of tasks a retention sweep deleted:
// the class (trust), how many tasks with how many messages and attachments,
// the cutoff and the retention it was swept with. It is written only for a
// class the sweep deleted something of.
const EvInteractionPruned = evtypes.InteractionPruned

// EvRetentionChanged records a change of content_retention made with
// `anet retention set`: from, to, and how many tasks the change deleted at
// once (core §2.14).
const EvRetentionChanged = evtypes.RetentionChanged

// pruneRetentionAt is the retention sweep at now (unix ms) at the clock
// gate's level: nothing goes at closed (FS-L6). It reports how many tasks
// it deleted. A sweep that failed part way still records what it deleted
// before the failure: the store commits in batches.
func (d *Daemon) pruneRetentionAt(now int64, level clockLevel) int64 {
	if d.ix == nil {
		return 0
	}
	r := d.config().contentRetention()
	got, err := d.ix.PruneTerminal(now, level, r)
	if err != nil {
		log.Printf("anet: retention: %v", err)
	}
	var total int64
	for _, class := range interactions.ContentClasses() {
		n := got[class]
		if n.Interactions == 0 {
			continue
		}
		total += n.Interactions
		cutoff, _ := interactions.RetentionCutoff(now, class, r)
		applied, _ := interactions.AppliedRetention(class, r)
		log.Printf("anet: retention: deleted %d %s tasks finished before %s (%d messages, %d attachments)",
			n.Interactions, class, time.UnixMilli(cutoff).UTC().Format(time.RFC3339), n.Messages, n.Attachments)
		if d.ledger == nil {
			continue
		}
		if _, err := d.ledger.Append(EvInteractionPruned, map[string]any{
			"trust": class, "before": cutoff, "retention_days": applied.Days(),
			"interactions": n.Interactions, "messages": n.Messages, "attachments": n.Attachments,
		}); err != nil {
			log.Printf("anet: retention evidence: %v", err)
		}
	}
	return total
}

// pruneEndedWorkDirs removes the exec work directories of the tasks that
// have ended (FS-L11): the per-task directory under both work roots, with
// the prompt files, inputs and whatever the agent wrote there, and the
// task's binding to the agent's own session. It runs every maintenance
// period whichever backend is configured, so a directory goes within S of
// its task's end even after auto-reply was switched off or changed. The
// operator's own auto_reply.work_dir is never touched.
func (d *Daemon) pruneEndedWorkDirs() {
	if d.ix == nil {
		return
	}
	open := func() (map[string]bool, error) { return d.ix.OpenIDs() }
	werr, serr := execwork.SweepAll(anethome.ModuleDir(d.layout.Root, "autoreply"), d.AID(), open)
	if werr != nil {
		log.Printf("anet: exec work dirs of ended tasks: %v", werr)
	}
	if serr != nil {
		log.Printf("anet: exec sessions of ended tasks: %v", serr)
	}
}

// counterWALBusy counts the times the write-ahead log stayed busy for
// walBusyReport periods in a row (FS-L8): deleted keys are readable in it
// until it is truncated.
const counterWALBusy = "wal-checkpoint-busy"

// walBusyReport is how many periods in a row the log may stay busy before
// it is counted and logged: an hour.
const walBusyReport = 12

// walBusyDue reports whether a log busy for n periods in a row is counted
// now: once every `every` periods.
func walBusyDue(n, every int) bool { return n > 0 && n%every == 0 }

// checkpointStore truncates the store's write-ahead log, once a period
// (FS-L8). A key deleted from the store is overwritten in the database file
// (secure_delete) but its bytes stay in frames of the log until it is
// truncated. Every period truncates it rather than only one after a key was
// deleted: a task ends — and its interaction key goes — in most periods of
// a working node, and keys go by many paths (a task's end, the close_after
// sweep, a held item's expiry, the retention sweep, an emergency rotation),
// none of which can be missed this way. A log busy with a reader is left to
// the next period.
func (d *Daemon) checkpointStore() {
	if d.ix == nil {
		return
	}
	n, err := d.ix.TruncateWAL()
	if err != nil {
		log.Printf("anet: truncate the store's log: %v", err)
		return
	}
	if walBusyDue(n, walBusyReport) {
		d.countErr(counterWALBusy, fmt.Errorf("the store's log stayed busy for %d maintenance periods in a row", n))
	}
}

// clockLevelNow is the clock gate's level now (FS-K14b), as a maintenance
// period starting now would find it: the latest check against the hub, and
// unverified_run as the last period left it brought up to now by the same
// rule (nextUnverifiedRun). A wall clock that jumped since the last period
// therefore closes the gate here as it would there (P-6 item 4), rather
// than letting a deletion run at the level the period before the jump
// reached. Nothing is stored: the next period makes its own update. It is
// the level a deletion made between periods runs at.
func (d *Daemon) clockLevelNow() clockLevel {
	if d.enc == nil {
		return clockClosed
	}
	wall, mono := d.nowMS(), d.monoMS()
	obs := d.clockObservation()
	d.clk.mu.Lock()
	var prev *cycleMark
	if d.clk.prev != nil {
		p := *d.clk.prev
		prev = &p
	}
	d.clk.mu.Unlock()
	run := nextUnverifiedRun(d.enc.unverifiedRun(), hubGateOpen(obs, wall, mono), prev, wall, mono)
	level, _ := clockGate(obs, run, wall, mono)
	return level
}

// ContentRetention is the content_retention in force.
func (d *Daemon) ContentRetention() interactions.Retention { return d.config().contentRetention() }

// RetentionWouldDelete is how many finished tasks setting content_retention
// to r would delete at once (POST /retention with dry_run, FS §5.2 step 3):
// the ones r lets go and the setting in force does not.
func (d *Daemon) RetentionWouldDelete(r interactions.Retention) (int64, error) {
	if d.ix == nil {
		return 0, nil
	}
	return d.ix.CountNewlyDeletable(int64(d.nowMS()), d.ContentRetention(), r)
}

// SetContentRetention puts content_retention r in force (`anet retention
// set`, FS §5.2 step 5): config.json is written first (0600, replaced at
// once), then a sweep runs at the clock gate's level now — at closed the
// value stands and the deletions wait for the gate — and the change is
// recorded (anet.retention.changed). It reports how many tasks the sweep
// deleted. Setting the value in force again changes and records nothing.
func (d *Daemon) SetContentRetention(r interactions.Retention) (int64, error) {
	if !r.Valid() {
		return 0, fmt.Errorf("anet: content_retention: %w", interactions.ErrBadInput)
	}
	var from interactions.Retention
	if _, err := d.updateConfig(func(c *Config) error {
		from = c.contentRetention()
		c.ContentRetention = RetentionValue(r.String())
		return nil
	}, func(c *Config) { c.ContentRetention = RetentionValue(r.String()) }); err != nil {
		return 0, err
	}
	if from == r {
		return 0, nil
	}
	deleted := d.pruneRetentionAt(int64(d.nowMS()), d.clockLevelNow())
	if d.ledger != nil {
		if _, err := d.ledger.Append(EvRetentionChanged, map[string]any{
			"from": from.String(), "to": r.String(), "deleted_interactions": deleted,
		}); err != nil {
			log.Printf("anet: retention evidence: %v", err)
		}
	}
	return deleted, nil
}
