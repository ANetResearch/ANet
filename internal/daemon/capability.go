// Capability delegation — C1 over the wire (docs/CONTRACTS-zh.md).
//
// Convention (uses only signed, CID-significant TaskDoc fields):
//   - Tasks[0].Requires carries {ID: <capability-id>, Type: "capability",
//     Necessity: "must"} — the capability being invoked;
//   - Tasks[0].Contexts carries {Key: "args", Value: <JSON>, Format: "json"}
//     — the invocation arguments.
//
// A daemon whose provider registry resolves the capability executes it
// deterministically and answers with the effect as the deliverable plus the
// usual signed receipt — no LLM in the loop. Unresolvable capability calls
// fall through to the normal auto-reply path untouched.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// RequireTypeCapability marks a TaskDoc requirement as a C1 capability call.
const RequireTypeCapability = "capability"

// capabilityInvokeTimeout bounds one provider invocation (providers are
// local: in-process or a UDS hop to anetlinkd).
const capabilityInvokeTimeout = 60 * time.Second

// Providers exposes the daemon's capability registry (config wires anetlink;
// tests and embedders register their own).
func (d *Daemon) Providers() *provider.Registry { return d.providers }

// capabilityCall detects the capability-delegation convention on a verified
// TaskDoc. Malformed args JSON is NOT a capability call (falls through to
// auto-reply rather than failing a possibly-humane task).
func capabilityCall(td *tsir.TaskDoc) (capID string, args map[string]any, ok bool) {
	if td == nil || len(td.Tasks) == 0 {
		return "", nil, false
	}
	t := td.Tasks[0]
	for _, r := range t.Requires {
		if r.Type == RequireTypeCapability && r.ID != "" {
			capID = r.ID
			break
		}
	}
	if capID == "" {
		return "", nil, false
	}
	args = map[string]any{}
	for _, c := range t.Contexts {
		if c.Key == "args" && c.Value != "" {
			if err := json.Unmarshal([]byte(c.Value), &args); err != nil {
				return "", nil, false
			}
			break
		}
	}
	return capID, args, true
}

// DelegateCapability delegates a capability invocation to providerAID. The
// TaskDoc carries the capability in Requires and args in Contexts — both
// inside the signed canonical preimage.
func (d *Daemon) DelegateCapability(ctx context.Context, providerAID, capID string, args map[string]any) (string, error) {
	hub := d.config().HubURL
	if hub == "" {
		return "", fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	if providerAID == d.AID() {
		return "", fmt.Errorf("anet: cannot delegate to yourself")
	}
	argsJSON := "{}"
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return "", err
		}
		argsJSON = string(b)
	}
	goal := "invoke capability " + capID
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{
		Intent:   tsir.Intent{Summary: goal, Body: goal},
		Requires: []tsir.Require{{ID: capID, Type: RequireTypeCapability, Necessity: "must"}},
		Contexts: []tsir.Context{{Key: "args", Value: argsJSON, Format: "json"}},
	}}}
	if err := td.Sign(d.self); err != nil {
		return "", err
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		return "", err
	}
	requestCID, err := anetcid.Sum(doc)
	if err != nil {
		return "", err
	}
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	if err := d.ix.Put(id, interactions.RoleOutbound, providerAID, goal, requestCID, doc); err != nil {
		return "", err
	}
	if _, err := d.ix.AddMessage(id, d.AID(), interactions.MsgText, goal+" args="+argsJSON); err != nil {
		return "", err
	}
	kelB, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		return "", err
	}
	dr := &delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, KEL: kelB, InteractionID: id}
	payload, err := dr.Marshal()
	if err != nil {
		return "", err
	}
	if err := d.relaySend(ctx, providerAID, hubapi.RelayKindDelegate, id, payload); err != nil {
		return "", err
	}
	return id, nil
}

// capabilityResult is the deterministic deliverable of a capability call.
//
// Evidence travels with it. It did not, and that made every read in the
// system write-only across the wire: a capability reports what it read in
// Evidence.ObservedState — the CID a store wrote, the bytes it read back,
// the position a camera reported, the org a node serves — and the only
// other channel is Metrics, which is map[string]float64 and cannot hold a
// CID, a blob or a list. So `cas.put` told the caller how many bytes it
// stored but not where, and `cas.get` could not be called at all.
//
// The provenance belongs here for a second reason, which is the one that
// generalises. A provider that corrected a vendor deviation says so in
// Evidence.Quirk, and a corrected reading is not the value the device put
// on the wire. That correction was reaching this node's own chain and
// stopping there — so the peer consuming the result, the one party who
// cannot check for itself, was the only party not told. A correction
// reaches every surface or none.
//
// The deliverable is what the receipt's ResultCID covers, so a provider
// that ships provenance is signing it: claiming V3 for a value it took on
// the device's word is now a signed claim rather than a private note.
type capabilityResult struct {
	Capability string             `json:"capability"`
	Status     string             `json:"status"`
	Verifiable bool               `json:"verifiable"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Message    string             `json:"message,omitempty"`
	Evidence   map[string]any     `json:"evidence,omitempty"`
}

// provenanceOf renders an effect's evidence for both surfaces that carry it.
//
// One function on purpose. The chain and the deliverable have to agree —
// evidence that differs depending on who is reading it is worse than no
// evidence — and two field lists that must stay identical are two field
// lists that will not. A map rather than a struct because encoding/json
// sorts map keys, so the deliverable stays byte-stable for the CID the
// receipt signs.
func provenanceOf(e *effect.Evidence) map[string]any {
	if e == nil {
		return nil
	}
	prov := map[string]any{
		"verify_trust": e.VerifyTrust,
		"auth_trust":   e.AuthTrust,
	}
	if e.Protocol != "" {
		prov["protocol"] = e.Protocol
	}
	if e.Requested != "" {
		prov["requested"] = e.Requested
	}
	if e.ObservedState != "" {
		prov["observed_state"] = e.ObservedState
	}
	if e.Quirk != "" {
		prov["quirk"] = e.Quirk
	}
	if e.NativeAck {
		prov["native_ack"] = true
	}
	if e.LatencyMS > 0 {
		prov["latency_ms"] = e.LatencyMS
	}
	return prov
}

// tryCapability resolves and executes a capability call, answering with the
// effect + signed receipt. Returns false when no provider serves it (the
// task then flows to auto-reply exactly as before).
func (d *Daemon) tryCapability(ctx context.Context, interactionID, capID string, args map[string]any) bool {
	if d.providers == nil {
		return false
	}
	p, ok := d.providers.Resolve(capID)
	if !ok {
		return false
	}
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		log.Printf("anet: capability %s: load interaction: %v", capID, err)
		return false
	}
	res := capabilityResult{Capability: capID}
	eff, err := p.Invoke(ctx, provider.Call{Capability: capID, Args: args, CallID: interactionID, CallerAID: ix.PeerAID})
	if err != nil {
		res.Status, res.Message = "FAILED", err.Error()
	} else {
		res.Status, res.Verifiable, res.Message = string(eff.Status), eff.Verifiable(), eff.Message
		res.Evidence = provenanceOf(eff.Evidence)
		if eff.Record != nil {
			res.Metrics = eff.Record.Metrics
		}
	}
	deliverable, err := json.Marshal(res)
	if err != nil {
		return false
	}
	if _, err := d.ix.AddMessage(interactionID, d.AID(), interactions.MsgText, string(deliverable)); err != nil {
		log.Printf("anet: capability %s: record result: %v", capID, err)
	}
	resultCID, err := anetcid.Sum(deliverable)
	if err != nil {
		return false
	}
	rc := &evidence.Receipt{
		InteractionID: interactionID,
		RequesterAID:  ix.PeerAID,
		ProviderAID:   d.AID(),
		RequestCID:    ix.RequestCID,
		ResultCID:     resultCID,
		CompletedAt:   uint64(nowMillis()),
	}
	if err := rc.Sign(d.self); err != nil {
		log.Printf("anet: capability %s: sign receipt: %v", capID, err)
		return false
	}
	receiptBytes, err := rc.Marshal()
	if err != nil {
		return false
	}
	if err := d.ix.SetResult(interactionID, deliverable, resultCID, receiptBytes); err != nil {
		log.Printf("anet: capability %s: store result: %v", capID, err)
		return false
	}
	// The chain records the provenance, not just the outcome — the same
	// provenance the requester was sent, from the same value, so the two
	// accounts of one effect cannot disagree.
	ev := map[string]any{
		"interaction_id": interactionID, "capability": capID, "caller_aid": ix.PeerAID,
		"status": res.Status, "verifiable": res.Verifiable, "metrics": res.Metrics,
		"result_cid": resultCID,
	}
	if res.Evidence != nil {
		ev["evidence"] = res.Evidence
	}
	if _, lerr := d.ledger.Append(EvCapabilityEffect, ev); lerr != nil {
		log.Printf("anet: capability %s: evidence ledger: %v", capID, lerr)
	}
	// The receipt travels with the key that signed it. A delegation has
	// always carried the requester's KEL inline; the answer carried no
	// provider KEL, so the requester could not check what it accepted.
	selfKEL, err := identity.MarshalKEL(d.self.KEL())
	if err != nil {
		log.Printf("anet: capability %s: marshal KEL: %v", capID, err)
		return false
	}
	rr := &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: deliverable,
		Receipt: receiptBytes, KEL: selfKEL}
	payload, err := rr.Marshal()
	if err != nil {
		return false
	}
	if err := d.relaySend(ctx, ix.PeerAID, hubapi.RelayKindResult, interactionID, payload); err != nil {
		log.Printf("anet: capability %s: relay result: %v (stored locally; requester can re-poll)", capID, err)
	}
	return true
}
