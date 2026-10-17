package a2ashape

// The sealed-sender rows of the reason table (SS §5.2–§5.4, SS-12): UX-T2
// for the area's codes — each has its one row, with both languages, of the
// class ux §1.4 gives it, and none of ux §1.3's internal codes has one —
// and UX-T3's check of SSR-U5: a sentence that promises the hub cannot tell
// who sent a message is refused, in either language.

import (
	"strings"
	"testing"
)

// senderPrivacyRows are the area's sentences (SS §5.3 table): the key and
// the class bit ux §1.4 files it under.
var senderPrivacyRows = []struct {
	key   string
	class Class
}{
	// C1: a message that ended (F), or a strict send refused at once (E).
	{"recipient_unknown", ClassF | ClassE},
	{"recipient_unverifiable", ClassE},
	{"too_large_for_stranger", ClassF},
	{"too_large", ClassF},
	{"bad_envelope", ClassF},
	// C2: waits (anet.delivery_wait).
	{"rate_limited", ClassW},
	{"redelivery_limited", ClassW},
	{"strangers_busy", ClassW},
	{"mailbox_full", ClassW},
	{"home_unreachable", ClassW},
	{"session_unavailable", ClassW},
	{"credential_refused", ClassW},
	// M5: the sender identity column of anet peers list.
	{"peers.sender_hidden", ClassS},
	{"peers.sender_visible", ClassS},
	{"peers.sender_visible.paid", ClassS},
	// §5.2: doctor's sender_privacy checks and p2p.lan.
	{"sender_privacy.mailbox_pending", ClassS},
	{"sender_privacy.grants_summary", ClassS},
	{"sender_privacy.withdrawals_pending", ClassS},
	{"sender_privacy.hub_refusals", ClassS},
	{"sender_privacy.hub_small_bucket", ClassS},
	{"sender_privacy.hub_epoch_invalid", ClassS},
	{"p2p.lan_on", ClassS},
}

// senderPrivacyInternal are the area's codes users never see (ux §1.3, SS
// §5.3 "not presented"): no row may give them a sentence.
var senderPrivacyInternal = []string{"access_invalid", "session_invalid", "revocation_list_full", "bucket_too_narrow",
	"identity_headers_not_allowed", "not_peer", "source_rate", "unknown_destination"}

// exactRow is the row whose key is key, and only that one (Lookup would
// fall back to the code of a dotted key).
func exactRow(key string) (Row, int) {
	var found Row
	n := 0
	for _, r := range reasonTable {
		if r.Key() == key {
			found, n = r, n+1
		}
	}
	return found, n
}

func TestSenderPrivacyRowsHaveBothLanguages(t *testing.T) {
	for _, c := range senderPrivacyRows {
		r, n := exactRow(c.key)
		if n != 1 {
			t.Errorf("%s: %d rows, want exactly one (UX-T2)", c.key, n)
			continue
		}
		if strings.TrimSpace(r.Zh) == "" || strings.TrimSpace(r.En) == "" {
			t.Errorf("%s: zh %q, en %q; both languages are required (UX-T2)", c.key, r.Zh, r.En)
		}
		if r.Class&c.class != c.class {
			t.Errorf("%s: class %b, want it to include %b (ux §1.4)", c.key, r.Class, c.class)
		}
		for _, lang := range []Language{Chinese, English} {
			if s := Sentence(c.key, lang, sampleArgs()); s == "" || strings.Contains(s, "{") {
				t.Errorf("%s (%v): sentence %q", c.key, lang, s)
			}
		}
	}
	// The waits recover on their own; only a refused first contact asks
	// for a look at anet status (ux §1.4 C2).
	for _, c := range senderPrivacyRows {
		if c.class != ClassW {
			continue
		}
		r, _ := exactRow(c.key)
		want := NextAuto
		if c.key == "session_unavailable" {
			want = NextCommand
		}
		if r.NextKind != want {
			t.Errorf("%s: next_kind %v, want %v (ux §1.4 C2)", c.key, r.NextKind, want)
		}
	}
	if r, _ := exactRow("session_unavailable"); r.Next != "anet status" {
		t.Errorf("session_unavailable: next %q, want anet status", r.Next)
	}
	for _, code := range senderPrivacyInternal {
		if _, n := exactRow(code); n != 0 {
			t.Errorf("%s is an internal code (ux §1.3) and has a sentence", code)
		}
	}
	// The three sentences of the column are three, and say different things.
	seen := map[string]string{}
	for _, k := range []string{"peers.sender_hidden", "peers.sender_visible", "peers.sender_visible.paid"} {
		for _, lang := range []Language{Chinese, English} {
			s := Sentence(k, lang, nil)
			if other, dup := seen[s]; dup {
				t.Errorf("%s and %s say the same: %q", k, other, s)
			}
			seen[s] = k
		}
	}
}

// forbiddenIn returns the words and promises of forbidden that s uses.
func forbiddenIn(s string) []string {
	var out []string
	for _, w := range forbidden {
		if containsWord(s, w) {
			out = append(out, w)
		}
	}
	return out
}

// UX-T3 against SSR-U5: each unconditional promise is caught wherever it
// stands in a sentence and whatever its case; what the area's sentences say
// instead — that a message carries no sender identity, that the hub may
// still infer it — is not. The column titles keep the rule too.
func TestTheHubCannotTellIsNeverPromised(t *testing.T) {
	for _, s := range []string{
		"信里不带你的身份，hub 看不出是你",
		"看不出发件人。",
		"Your messages carry no identity, so the hub cannot tell it is you.",
		"The hub Cannot Tell Who Sent it",
		"cannot tell who sent this message",
	} {
		if len(forbiddenIn(s)) == 0 {
			t.Errorf("SSR-U5: %q passes the lint", s)
		}
	}
	for _, k := range []string{"peers.sender_hidden", "peers.sender_visible", "peers.sender_visible.paid",
		"sender_privacy.grants_summary", "sender_privacy.hub_refusals", "sender_privacy.hub_small_bucket"} {
		for _, lang := range []Language{Chinese, English} {
			if w := forbiddenIn(Sentence(k, lang, sampleArgs())); len(w) != 0 {
				t.Errorf("%s (%v) uses %v (UX-G2, SSR-U5)", k, lang, w)
			}
		}
	}
	for k, p := range labels {
		for _, s := range []string{p.zh, p.en} {
			if w := forbiddenIn(s); len(w) != 0 {
				t.Errorf("label %s %q uses %v (UX-G2)", k, s, w)
			}
		}
	}
	for _, k := range []string{"peers_list.aid", "peers_list.lists", "peers_list.sender"} {
		if Label(k, Chinese) == "" || Label(k, English) == "" {
			t.Errorf("the anet peers list column %s has no title", k)
		}
	}
}
