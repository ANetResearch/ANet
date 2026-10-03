package a2ashape

// UX-T2 (ux §6.1): the reason table is complete. Every user reason the wire
// registry lists has a row, every row is a key of ux §1.4, §3.4 (g), §4.7 or
// §5.2 and each of those has exactly one row, the codes that ux keeps out of
// the user's view have none, and every C1 refusal maps to a user code.

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/wire"
)

// uxKeys reads testdata/reasons/ux-keys.txt: the keys ux defines, in its order.
func uxKeys(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "reasons", "ux-keys.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(l, "#") {
			keys = append(keys, l)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(keys) < 300 {
		t.Fatalf("ux-keys.txt has %d keys; the list is probably cut", len(keys))
	}
	return keys
}

// Every key of ux is a row, once, in ux's order; no row is anything else.
func TestReasonTableHasExactlyTheUXKeys(t *testing.T) {
	want := uxKeys(t)
	var got []string
	seen := map[string]int{}
	for _, r := range reasonTable {
		got = append(got, r.Key())
		seen[r.Key()]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("%s has %d rows; a key has exactly one (UX-G1)", k, n)
		}
	}
	for _, k := range want {
		if seen[k] == 0 {
			t.Errorf("ux key %s has no row in reasons.go", k)
		}
	}
	for _, k := range got {
		if !slices.Contains(want, k) {
			t.Errorf("reasons.go row %s is not a key of ux §1.4, §3.4 (g), §4.7 or §5.2", k)
		}
	}
	if len(got) == len(want) && !slices.Equal(got, want) {
		t.Errorf("the rows are not in ux's order (other packages append in that order, the release plan)")
	}
}

// The registry's user reasons (CORE-K24, generated from this table) and the
// table's codes are the same set.
func TestReasonTableMatchesTheRegistry(t *testing.T) {
	reg, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, r := range reasonTable {
		if codeRe.MatchString(r.Code) {
			codes[r.Code] = true
		}
	}
	for _, c := range reg.ReasonCodes("user") {
		if !codes[c] {
			t.Errorf("registry reasons.user has %s, the table has no row for it", c)
		}
	}
	for c := range codes {
		if reg.Reason("user", c) == nil {
			t.Errorf("table code %s is not in registry reasons.user (CORE-F4: every code is registered)", c)
		}
	}
	for _, r := range reg.Reasons.Retired {
		if _, ok := rowIndex[r.Code]; ok {
			t.Errorf("%s is retired in the registry (%s) and must not have a row", r.Code, r.Note)
		}
	}
}

// A row's code is a code, an "x402:" code, or a dotted key that is not a code;
// a variant hangs only off a code; every field a row needs is set.
func TestReasonRowsAreWellFormed(t *testing.T) {
	for _, r := range reasonTable {
		k := r.Key()
		switch {
		case codeRe.MatchString(r.Code):
		case strings.HasPrefix(r.Code, "x402:") && strings.ToUpper(r.Code[5:]) == r.Code[5:]:
		case strings.Contains(r.Code, ".") && r.Variant == "":
		default:
			t.Errorf("%s: %q is neither a code, an x402: code nor a dotted sentence key", k, r.Code)
		}
		if r.Class == 0 {
			t.Errorf("%s: no class", k)
		}
		if r.NoSentence {
			if r.Zh != "" || r.En != "" || r.Class != ClassModel {
				t.Errorf("%s: a row with no sentence is a dropped egress connection (ux §1.4 I) and has no words", k)
			}
			continue
		}
		if r.Zh == "" || r.En == "" {
			t.Errorf("%s: both languages are required (zh %q, en %q)", k, r.Zh, r.En)
		}
		if r.Class&(ClassE) != 0 && r.A2A.Err == A2ANone {
			t.Errorf("%s: class E without its A2A error (UX-R10)", k)
		}
		if r.Class&ClassE == 0 && r.A2A.Err != A2ANone {
			t.Errorf("%s: an A2A error on a row that is not class E", k)
		}
		if r.Class&ClassFP != 0 && r.A2A.X402 == "" {
			t.Errorf("%s: class FP without its x402.payment.error", k)
		}
		if r.Class&ClassR != 0 && r.A2A.Effect != string(effect.Unavailable) {
			t.Errorf("%s: class R ends a capability task UNAVAILABLE (ux §1.1), not %q", k, r.A2A.Effect)
		}
		switch r.A2A.Effect {
		case "", string(effect.Unavailable), string(effect.Unverified), string(effect.Failed):
		default:
			t.Errorf("%s: %q is not an effect status", k, r.A2A.Effect)
		}
		switch r.Level {
		case "", LevelOK, LevelInfo, LevelWarn, LevelFail, LevelUnknown:
		default:
			t.Errorf("%s: level %q", k, r.Level)
		}
	}
}

// ux §1.3: codes this node handles itself, or only counts, have no row.
func TestCodesKeptFromTheUserHaveNoRow(t *testing.T) {
	for _, c := range []string{
		"session_invalid", "access_invalid", "kid_stale", "keys_rollback", "revocation_list_full",
		"bucket_too_narrow", "bad_status_query", "bad_ack_item", "identity_headers_not_allowed", "not_found",
		"internal", "unauthorized", "key_retired", "ts_stale", "replayed", "expired", "bad_exp", "not_peer",
		"source_rate", "unknown_destination", "not_witness", "auto_failed", "receipt_too_old",
		"caller_unwitnessed", "provider_busy", "quota_global_per_min", "max_inflight", "task_nonce_required",
		"peer_refused_key_state", "prepay_removed", "payment_extension_not_activated",
		"recovery_setup_required", "access_revoked", "home_unknown", "stopped_elsewhere", "publishing",
		"admission_required", "pow_required", "registration_quota", "kel_fork", "kel_rollback",
		"kel_ts_out_of_window", "aid_deactivated", "witness_not_this_hub", "kel_invalid",
	} {
		if _, ok := rowIndex[c]; ok {
			t.Errorf("%s is kept from the user (ux §1.2, §1.3) and must not have a row", c)
		}
	}
	for _, k := range []string{"recovery_setup_required.pending_admission", "hub_pending_admission.recovery",
		"card_price_unavailable.note", "receipt.mismatch"} {
		if _, ok := rowIndex[k]; ok {
			t.Errorf("%s was merged away (ux §1.2) and must not have a row", k)
		}
	}
}

// Every C1 refusal of C1 v1 (core §2.15.3) maps to a user code that has a row;
// nothing else is mapped.
func TestC1RefusalsMapToUserCodes(t *testing.T) {
	reg, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	c1 := reg.ReasonCodes("c1")
	if len(c1) == 0 {
		t.Fatal("no c1 namespace in the registry")
	}
	for _, c := range c1 {
		u, ok := c1Reasons[c]
		if !ok {
			t.Errorf("C1 refusal %s has no user code (ux §1.3)", c)
			continue
		}
		if _, ok := Lookup(u); !ok || C1Reason(c) != u {
			t.Errorf("C1 %s → %s, which has no row", c, u)
		}
	}
	for c := range c1Reasons {
		if !slices.Contains(c1, c) {
			t.Errorf("c1Reasons maps %s, which is not a C1 v1 refusal", c)
		}
	}
	want := map[string]string{"not_permitted": "provider_policy", "unknown_capability": "capability_not_served",
		"not_found": "capability_not_served", "too_large": "args_too_large", "bad_request": "backend_failed",
		"unsupported_media": "backend_failed", "internal": "backend_failed", "c1_version": "backend_failed",
		"from_a_later_version": "backend_failed"}
	for c, u := range want {
		if got := C1Reason(c); got != u {
			t.Errorf("C1Reason(%s) = %s, ux §1.3 says %s", c, got, u)
		}
	}
}

// deliveryWaits are the values of anet.delivery_wait (core §2.13, §2.15.1).
var deliveryWaits = []string{"rate_limited", "redelivery_limited", "strangers_busy", "mailbox_full",
	"home_unreachable", "session_unavailable", "credential_refused", "notice_stale", "rotation_pending"}

// The phrases the placeholders take are complete: each delivery wait has a
// wait sentence and its short form (ux §1.4 C2), and every phrase has both
// languages.
func TestPhraseSetsAreComplete(t *testing.T) {
	for _, w := range deliveryWaits {
		if _, ok := phraseSets["wait"][w]; !ok {
			t.Errorf("wait %s has no short form for {wait} (ux §1.4 C2)", w)
		}
		if r, ok := Lookup(waitKey(w)); !ok || r.Class&ClassW == 0 {
			t.Errorf("wait %s has no wait sentence (ux §1.4 C2)", w)
		}
	}
	if len(phraseSets["wait"]) != len(deliveryWaits) {
		t.Errorf("the wait short forms are %d, the waits %d", len(phraseSets["wait"]), len(deliveryWaits))
	}
	for set, ps := range phraseSets {
		for k, p := range ps {
			if p.zh == "" || p.en == "" {
				t.Errorf("phrase %s/%s lacks a language", set, k)
			}
		}
	}
	for k, p := range labels {
		if p.zh == "" || p.en == "" {
			t.Errorf("label %s lacks a language", k)
		}
	}
}
