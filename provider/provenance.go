package provider

import "github.com/ANetResearch/ANetCore/effect"

// Provenance renders an effect's evidence as the map every surface
// publishes.
//
// One function on purpose, and it is shared rather than copied because
// the chain, the delegated result and the voucher-door result all carry
// it. Evidence that differs depending on who is reading it is worse than
// no evidence, and two field lists that must stay identical are two field
// lists that will not.
//
// A map rather than a struct because encoding/json sorts map keys, so the
// deliverable stays byte-stable for the CID the receipt is taken over.
func Provenance(e *effect.Evidence) map[string]any {
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
