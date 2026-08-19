// Capability delegation — C1 over the wire (K207/K209).
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
	"github.com/ANetResearch/ANetCore/effect"
	"log"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/protocol/delegation"
	"github.com/ANetResearch/ANet/internal/protocol/evidence"
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
type capabilityResult struct {
	Capability string             `json:"capability"`
	Status     string             `json:"status"`
	Verifiable bool               `json:"verifiable"`
	Metrics    map[string]float64 `json:"metrics,omitempty"`
	Message    string             `json:"message,omitempty"`
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
	var provenance *effect.Evidence
	eff, err := p.Invoke(ctx, provider.Call{Capability: capID, Args: args, CallID: interactionID, CallerAID: ix.PeerAID})
	if err != nil {
		res.Status, res.Message = "FAILED", err.Error()
	} else {
		provenance = eff.Evidence
		res.Status, res.Verifiable, res.Message = string(eff.Status), eff.Verifiable(), eff.Message
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
	// The chain records the provenance, not just the outcome.
	//
	// It used to store status, verifiability, metrics and the result CID —
	// everything about *what happened* and nothing about *how we know*. A
	// provider that corrected a vendor deviation says so in Evidence.Quirk,
	// and a corrected reading is not the value the device put on the wire:
	// dropping the label here breaks the rule the correction layer is built
	// on — a correction reaches every surface or none — precisely at the
	// process boundary where it is hardest to notice. The trust levels
	// belong here for the same reason: an effect verified by an independent
	// read (V3) and one taken on the device's word (V2) are different
	// evidence, and the chain is where that distinction has to survive.
	ev := map[string]any{
		"interaction_id": interactionID, "capability": capID, "caller_aid": ix.PeerAID,
		"status": res.Status, "verifiable": res.Verifiable, "metrics": res.Metrics,
		"result_cid": resultCID,
	}
	if e := provenance; e != nil {
		prov := map[string]any{}
		if e.Protocol != "" {
			prov["protocol"] = e.Protocol
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
		prov["verify_trust"] = e.VerifyTrust
		prov["auth_trust"] = e.AuthTrust
		ev["evidence"] = prov
	}
	if _, lerr := d.ledger.Append(EvCapabilityEffect, ev); lerr != nil {
		log.Printf("anet: capability %s: evidence ledger: %v", capID, lerr)
	}
	rr := &delegation.ResultResp{Status: delegation.StatusDone, Deliverable: deliverable, Receipt: receiptBytes}
	payload, err := rr.Marshal()
	if err != nil {
		return false
	}
	if err := d.relaySend(ctx, ix.PeerAID, hubapi.RelayKindResult, interactionID, payload); err != nil {
		log.Printf("anet: capability %s: relay result: %v (stored locally; requester can re-poll)", capID, err)
	}
	return true
}
