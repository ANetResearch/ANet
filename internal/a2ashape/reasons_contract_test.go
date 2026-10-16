package a2ashape

// UX-T1 (ux §6.1, SI-79): the same event gives the same code and the same
// words wherever it shows — the CLI's human line or error line, MCP's tool
// result, the local A2A interface's status.message or error message, and the
// control plane's error. This file holds the assertion every surface test
// uses, and the cases whose producers live in this package: the refusals of
// class E (control-plane body and A2A error), the delivery lines, the payment
// sentences and the status summary. The packages that produce the other rows
// add their cases next to their producers (cmd/anet: cli_removed and
// config_key_removed; the areas: their rows).

import (
	"math"
	"strings"
	"testing"
	"time"
)

// surface is one place that showed a sentence: where, the code it gave
// (anet.reason, the body's reason, the ErrorInfo's metadata.reason; "" when
// the surface carries none), and the words.
type surface struct {
	name, code, text string
}

// sameSentence asserts UX-T1 for one event: every surface shows key's
// sentence in lang, word for word, under key's code.
func sameSentence(t *testing.T, key string, lang Language, args Args, got ...surface) {
	t.Helper()
	want := Sentence(key, lang, args)
	if want == "" {
		t.Errorf("%s: no sentence", key)
	}
	code := ReasonCode(key)
	for _, s := range got {
		if s.text != want {
			t.Errorf("%s on %s:\n got:  %q\n want: %q", key, s.name, s.text, want)
		}
		if s.code != "" && s.code != code {
			t.Errorf("%s on %s: code %q, want %q", key, s.name, s.code, code)
		}
	}
}

// sampleArgs are values for every placeholder, from this node's data.
func sampleArgs() Args {
	return Args{
		"aid": "bafyreiujzdegxdncf32epf3dhodzdocis2jhtlgmxgedn73u557hq2abcd", "peer": "bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz",
		"file_aid": "bafyreinsipzz7fk4zri3r2wyojfljooa7lqsaj2xuid5zzzzg63pq7vsn4", "hub": "https://hub.example",
		"task": "t_9f3c", "retry": 15 * time.Second, "limit": 15 * time.Minute, "dur": 2 * time.Hour, "ago": 4 * time.Minute,
		"time": time.Date(2026, 10, 1, 14, 20, 0, 0, time.Local), "now": time.Date(2026, 10, 1, 14, 5, 0, 0, time.Local),
		"amount": 3, "price": 3, "single": 10, "left": 50, "spent": 40, "daily": 50, "need": 53, "n": 2, "m": 1, "k": 0,
		"code": "some_code", "path": "/home/u/.anet", "name": "coder", "key": "explicit_max", "value": "12",
		"tier": "agent", "why": "timeout", "wait": "mailbox_full", "retention": "30d", "version": "0.3.0",
		"error": "connection refused", "action": "pay", "command": "anet pay t_9f3c", "what": "anet evidence",
		"instead": "evidence", "rule": "a number of days", "hosts": "pypi.org", "agents": "claude, codex",
		"per_peer": 5, "auto_max": 0, "agent_max": 0, "agent_left": 0, "explicit_max": 10, "from": 1, "to": 2,
		"old": "https://old.example", "file": "/home/u/.anet-recovery/x.anetrec", "how": "scheduled",
		"skill": "translate", "state": "verified", "charge": "partial", "backend": "openai", "host": "example.org",
		"field": "tools", "type": "web_search", "paths": "/v1/chat/completions", "auto_reply": "off",
	}
}

// A refusal reaches the control plane and the local A2A interface with the
// same code and words (UX-R9, UX-R10).
func TestRefusalSurfacesSaySameSentence(t *testing.T) {
	for _, lang := range []Language{Lang(), Chinese, English} {
		for _, r := range reasonTable {
			if r.Class&(ClassE|ClassC) == 0 || r.NoSentence {
				continue
			}
			args := sampleArgs()
			ref := Refuse(r.Key(), args)
			body := ref.Body(lang)
			surfaces := []surface{{"control", body.Reason, body.Error}}
			if r.Class&ClassE != 0 {
				e := ref.A2A(lang)
				surfaces = append(surfaces, surface{"a2a", e.Details[1].Metadata["reason"], e.Message})
			}
			sameSentence(t, r.Key(), lang, args, surfaces...)
			if body.Next != Next(r.Key(), lang, args) {
				t.Errorf("%s: body next %q", r.Key(), body.Next)
			}
		}
	}
}

// The A2A errors of core G1 and UX-R10: the standard error where one fits,
// −32602 / 400 for a lasting state and −32603 / 503 with Retry-After for a
// passing one; two ErrorInfo, A2A's then anet's.
func TestRefusalA2AErrors(t *testing.T) {
	for _, c := range []struct {
		key    string
		name   string
		rpc    int
		http   int
		status string
		first  string
		retry  bool
	}{
		{"no_local_task", "TaskNotFoundError", -32001, 404, "NOT_FOUND", "TASK_NOT_FOUND", false},
		{"extended_card_not_offered", "UnsupportedOperationError", -32004, 400, "FAILED_PRECONDITION", "UNSUPPORTED_OPERATION", false},
		{"extended_card_not_configured", "ExtendedAgentCardNotConfiguredError", -32007, 400, "FAILED_PRECONDITION", "EXTENDED_AGENT_CARD_NOT_CONFIGURED", false},
		{"extended_card_unavailable", "InternalError", -32603, 503, "UNAVAILABLE", "INTERNAL_ERROR", true},
		{"extended_card_unavailable.bad_signature", "InvalidAgentResponseError", -32006, 500, "INTERNAL", "INVALID_AGENT_RESPONSE", false},
		{"message_id_reused.open", "InvalidParamsError", -32602, 400, "INVALID_ARGUMENT", "INVALID_PARAMS", false},
		{"recipient_unknown", "InvalidParamsError", -32602, 400, "INVALID_ARGUMENT", "INVALID_PARAMS", false},
		{"recipient_unverifiable", "InternalError", -32603, 503, "UNAVAILABLE", "INTERNAL_ERROR", true},
		{"hub_pending_admission", "InternalError", -32603, 503, "UNAVAILABLE", "INTERNAL_ERROR", true},
		{"hub_retrying", "InternalError", -32603, 503, "UNAVAILABLE", "INTERNAL_ERROR", true},
		{"hub_refused", "InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS", false},
		{"no_hub_yet", "InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS", false},
		{"wire_too_old", "InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS", false},
		{"identity_stopped", "InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS", false},
		{"identity_deactivated", "InvalidParamsError", -32602, 400, "FAILED_PRECONDITION", "INVALID_PARAMS", false},
	} {
		e := Refuse(c.key, sampleArgs()).A2A(English)
		if e.Name != c.name || e.RPCCode != c.rpc || e.HTTPStatus != c.http || e.Status != c.status {
			t.Errorf("%s: %s %d %d %s, want %s %d %d %s", c.key, e.Name, e.RPCCode, e.HTTPStatus, e.Status, c.name, c.rpc, c.http, c.status)
		}
		if len(e.Details) != 2 {
			t.Fatalf("%s: %d ErrorInfo, want 2 (core G1)", c.key, len(e.Details))
		}
		a, n := e.Details[0], e.Details[1]
		if a.Type != ErrorInfoType || a.Domain != DomainA2A || a.Reason != c.first || len(a.Metadata) != 0 {
			t.Errorf("%s: the A2A ErrorInfo is %+v", c.key, a)
		}
		code := ReasonCode(c.key)
		if n.Type != ErrorInfoType || n.Domain != DomainANet || n.Reason != strings.ToUpper(code) || n.Metadata["reason"] != code {
			t.Errorf("%s: the anet ErrorInfo is %+v", c.key, n)
		}
		if next := Next(c.key, English, sampleArgs()); n.Metadata["next"] != next {
			t.Errorf("%s: metadata.next %q, want %q", c.key, n.Metadata["next"], next)
		}
		if (e.RetryAfter >= 1) != c.retry {
			t.Errorf("%s: Retry-After %d (a passing state has one, nothing else)", c.key, e.RetryAfter)
		}
	}
	bad := Refuse("extended_card_unavailable.bad_signature", nil).A2A(English).Details[1].Metadata
	if bad["variant"] != "bad_signature" || bad["reason"] != "extended_card_unavailable" {
		t.Errorf("a variant is a metadata member, never the reason (CORE-K23, K25): %v", bad)
	}
	r := Refuse("hub_pending_admission", nil)
	r.RetryAfter = 1500 * time.Millisecond
	if got := r.A2A(English).RetryAfter; got != 2 {
		t.Errorf("Retry-After rounds up to a whole second: %d", got)
	}
	r.RetryAfter = time.Duration(math.MaxInt64)
	if got := r.A2A(English).RetryAfter; got != maxRetryAfter {
		t.Errorf("the largest wait does not wrap around: Retry-After %d", got)
	}
}

// The body carries hub_code on hub_refused only, and never a variant as its
// reason.
func TestRefusalBody(t *testing.T) {
	r := Refuse("hub_refused", Args{"hub": "https://hub.example", "code": "kel_invalid"})
	r.HubCode = "kel_invalid"
	if b := r.Body(English); b.HubCode != "kel_invalid" || b.Reason != "hub_refused" || b.Next != "" {
		t.Errorf("hub_refused: %+v", b)
	}
	r = Refuse("busy", nil)
	r.HubCode = "x"
	if b := r.Body(English); b.HubCode != "" {
		t.Errorf("hub_code on another code: %+v", b)
	}
	b := Refuse("message_id_reused.followup", Args{"task": "t_1"}).Body(Chinese)
	if b.Reason != "message_id_reused" {
		t.Errorf("a variant is not a reason (CORE-K25): %+v", b)
	}
	if got := Refuse("no_hub_yet", nil).Error(); got != Sentence("no_hub_yet", English, nil) {
		t.Errorf("Error is the English sentence: %q", got)
	}
	// UX-R9: a key the table lacks never puts a reason outside the table in
	// a body; the code is the one of the sentence shown (UX-G3, UX-G9).
	for key, want := range map[string]string{"brand_new_code": "unknown_code", "brand_new_code.v": "unknown_code",
		"{0}": "refused", "Not A Code": "refused"} {
		r := Refuse(key, Args{"task": "t_1"})
		b := r.Body(English)
		if b.Reason != want || r.Code() != want || b.Error != Sentence(key, English, nil) {
			t.Errorf("%q: body %+v, code %q; want reason %s with its sentence", key, b, r.Code(), want)
		}
		md := r.A2A(English).Details[1].Metadata
		if md["reason"] != want || md["variant"] != "" {
			t.Errorf("%q: anet ErrorInfo metadata %v; want reason %s and no variant", key, md, want)
		}
	}
}

// The delivery line is the sentence of its key, the tail joined as UX-R11
// says.
func TestDeliveryTextIsTheTable(t *testing.T) {
	end := time.Date(2026, 10, 1, 14, 20, 0, 0, time.Local)
	for _, lang := range []Language{Chinese, English} {
		tail := Sentence("delivery.ends_at", lang, Args{"time": end, "now": end})
		sep := " "
		if lang == Chinese {
			sep = ""
		}
		for _, c := range []struct {
			delivery, detail, wait string
			ordered                bool
			key                    string
		}{
			{"pending", "not_sent", "", false, "delivery.not_sent"},
			{"pending", "not_sent", "", true, "delivery.not_sent.ordered"},
			{"pending", "sent", "", false, "delivery.sent"},
			{"pending", "queued_at_hub", "", false, "delivery.queued_at_hub"},
			{"pending", "sent", "mailbox_full", false, "mailbox_full"},
			{"pending", "not_sent", "notice_stale", false, "notice_stale.wait"},
			{"pending", "not_sent", "rotation_pending", false, "rotation_pending.wait"},
			{"pending", "sent", "redelivery_limited", false, "redelivery_limited"},
		} {
			got := DeliveryText(c.delivery, c.detail, c.wait, c.ordered, end, lang)
			want := Sentence(c.key, lang, nil) + sep + tail
			if got != want {
				t.Errorf("%v: %q, want %q", c, got, want)
			}
		}
		for _, c := range []struct{ delivery, detail, key string }{
			{"confirmed", "", "delivery.confirmed"},
			{"unconfirmed", "not_sent", "delivery.unconfirmed.not_sent"},
			{"unconfirmed", "sent", "delivery.unconfirmed.sent"},
			{"unconfirmed", "queued_at_hub", "delivery.unconfirmed.sent"},
		} {
			if got := DeliveryText(c.delivery, c.detail, "", false, end, lang); got != Sentence(c.key, lang, nil) {
				t.Errorf("%v: %q; a delivery that is not pending has no tail", c, got)
			}
		}
	}
}

// The payment note is the sentence of its code, with the snapshot's numbers
// (HPP PAY-U6).
func TestPaymentNoteIsTheTable(t *testing.T) {
	price, value := uint64(3), uint64(12)
	for _, lang := range []Language{Chinese, English} {
		for _, n := range []PayNote{
			{Code: "needs_operator_approval.off", Amount: 3, Tier: "auto", Task: "t_9f3c"},
			{Code: "needs_operator_approval.over_limit", Amount: 30, Tier: "agent", Single: 5, Left: 44, Task: "t_9f3c"},
			{Code: "needs_operator_approval.manual_limit", Amount: 12, Tier: "auto", Single: 10, Left: 50, Key: "explicit_max", Value: &value, Task: "t_9f3c"},
			{Code: "needs_operator_approval.payee_not_allowed", Amount: 3, Tier: "auto", Peer: "bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz", Task: "t_9f3c"},
			{Code: "needs_operator_approval.payees_unreadable", Amount: 3, Tier: "auto", Task: "t_9f3c"},
			{Code: "quote_above_card_price", Amount: 5, Price: &price, Tier: "auto", Task: "t_9f3c"},
			{Code: "card_price_unavailable", Amount: 5, Tier: "auto", Why: "strangers_busy", Task: "t_9f3c"},
			{Code: "paid.auto", Amount: 3, Tier: "auto", Single: 5, Left: 44},
			{Code: "paid.agent", Amount: 3, Tier: "agent", Single: 5, Left: 44},
			{Code: "paid.manual", Amount: 3, Tier: "manual", Single: 10, Left: 47},
			{Code: "quote_expired", Amount: 3, Task: "t_9f3c"},
			{Code: "x402:INVALID_SIGNATURE.rotated", Amount: 3, Task: "t_9f3c"},
		} {
			args := Args{"amount": n.Amount, "price": n.Price, "tier": n.Tier, "single": n.Single, "left": n.Left,
				"key": n.Key, "value": n.Value, "why": n.Why, "peer": n.Peer, "task": n.Task}
			got := PaymentNote(n, lang)
			sameSentence(t, n.Code, lang, args, surface{"payment_note", "", got})
			if strings.ContainsAny(got, "{}—") {
				t.Errorf("%s: a value is missing: %q", n.Code, got)
			}
		}
	}
	if PaymentNote(PayNote{}, English) != "" {
		t.Error("no decision, no note")
	}
}

// The status summary (ux §4.7): its lines are the sentences of their keys, in
// the table's order, and its rules decide which lines show.
func TestStatusSummary(t *testing.T) {
	base := func() StatusInput {
		return StatusInput{
			IdentityState: "ok", Identity: &Said{Key: "identity.ok", Args: sampleArgs()},
			Recovery: "protected", Hub: Said{Key: "registered", Args: sampleArgs()}, HubRegistered: true,
			Inbound:   Said{Key: "inbound.summary.closed", Args: sampleArgs()},
			Payments:  Said{Key: "limits_summary.off", Args: sampleArgs()},
			Retention: Said{Key: "retention.summary", Args: sampleArgs()},
		}
	}
	topics := func(ls []SummaryLine) string {
		var t []string
		for _, l := range ls {
			t = append(t, l.Topic)
		}
		return strings.Join(t, ",")
	}
	for _, c := range []struct {
		name string
		edit func(*StatusInput)
		want string
	}{
		{"fresh, admitted", func(*StatusInput) {}, "identity,hub,inbound,payments,privacy"},
		{"no hub: no identity line (UX-C9)", func(in *StatusInput) { in.IdentityState = "no_hub" }, "hub,inbound,payments,privacy"},
		{"no passphrase yet: the recovery line, no hub included", func(in *StatusInput) {
			in.IdentityState, in.Recovery = "no_hub", "unprotected"
		}, "recovery,hub,inbound,payments,privacy"},
		{"no passphrase yet, joined", func(in *StatusInput) { in.Recovery = "unprotected" }, "identity,recovery,hub,inbound,payments,privacy"},
		{"stopped: no recovery line", func(in *StatusInput) {
			in.IdentityState, in.Recovery = "stopped", "unprotected"
			in.Identity = &Said{Key: "identity_stopped", Args: sampleArgs()}
		}, "identity,hub,inbound,payments,privacy"},
		{"deactivated: no recovery line", func(in *StatusInput) {
			in.IdentityState, in.Recovery = "deactivated", "unprotected"
			in.Identity = &Said{Key: "identity_deactivated"}
		}, "identity,hub,inbound,payments,privacy"},
		{"everything", func(in *StatusInput) {
			in.Delivery = &Said{Key: "outbox.ok", Args: sampleArgs()}
			in.Sandbox = &Said{Key: "sandbox.ready"}
			in.SandboxBudget = &Said{Key: "sandbox.budget", Args: sampleArgs()}
			in.Preview = &Said{Key: "preview.on"}
		}, "identity,hub,delivery,inbound,sandbox,payments,privacy,preview"},
	} {
		in := base()
		c.edit(&in)
		for _, lang := range []Language{Chinese, English} {
			got := StatusSummary(in, lang)
			if topics(got) != c.want {
				t.Errorf("%s: %s, want %s", c.name, topics(got), c.want)
			}
			for _, l := range got {
				if l.Text == "" || l.Level == "" {
					t.Errorf("%s: an empty line %+v", c.name, l)
				}
			}
		}
	}

	in := base()
	in.Recovery = "unprotected"
	for _, lang := range []Language{Chinese, English} {
		l := StatusSummary(in, lang)[1]
		sameSentence(t, "identity.unprotected", lang, nil, surface{"summary", "", l.Text})
		if l.Level != LevelWarn || l.Next != "anet identity protect" {
			t.Errorf("the recovery line is warn with its command: %+v", l)
		}
		in.RecoveryResume = true
		sameSentence(t, "identity.unprotected.resume", lang, nil, surface{"summary", "", StatusSummary(in, lang)[1].Text})
		in.RecoveryResume = false
	}

	// The hub line: wire_too_old and wire_too_new take its place.
	in = base()
	in.HubWire = &Said{Key: "wire_too_old", Args: sampleArgs()}
	if l := StatusSummary(in, English)[1]; l.Topic != "hub" || l.Text != Sentence("wire_too_old", English, sampleArgs()) || l.Level != LevelFail {
		t.Errorf("wire_too_old first: %+v", l)
	}
	// The privacy line: repeated, detected, the clock (only when registered),
	// the retention.
	privacy := func(in StatusInput) string {
		ls := StatusSummary(in, English)
		return ls[len(ls)-1].Text
	}
	in = base()
	in.Clock = &Said{Key: "clock.skewed", Args: sampleArgs()}
	if privacy(in) != Sentence("clock.skewed", English, sampleArgs()) {
		t.Error("a skewed clock replaces the retention line")
	}
	in.HubRegistered = false
	if privacy(in) != Sentence("retention.summary", English, sampleArgs()) {
		t.Error("no clock sentence while the hub is not registered (FS-U7)")
	}
	in.LocalStateDetected = &Said{Key: "local_state.detected", Args: sampleArgs()}
	if privacy(in) != Sentence("local_state.detected", English, sampleArgs()) {
		t.Error("a detected rollback comes before the clock and the retention")
	}
	in.LocalStateRepeated = &Said{Key: "local_state.repeated", Args: sampleArgs()}
	if privacy(in) != Sentence("local_state.repeated", English, sampleArgs()) {
		t.Error("a repeated rollback comes first")
	}
	// The sandbox line is the sandbox sentence and the budget, joined.
	in = base()
	in.Sandbox = &Said{Key: "sandbox.ready"}
	in.SandboxBudget = &Said{Key: "sandbox.budget", Args: sampleArgs()}
	for _, lang := range []Language{Chinese, English} {
		sep := " "
		if lang == Chinese {
			sep = ""
		}
		for _, l := range StatusSummary(in, lang) {
			if l.Topic == "sandbox" && l.Text != Sentence("sandbox.ready", lang, nil)+sep+Sentence("sandbox.budget", lang, sampleArgs()) {
				t.Errorf("sandbox line: %q", l.Text)
			}
		}
	}
}

// The cases above hold in whichever language this process speaks (UX-T1 is
// run once per language by lang_test.go, UX-T13).
func runContract(t *testing.T) {
	t.Run("refusals", TestRefusalSurfacesSaySameSentence)
	t.Run("delivery", TestDeliveryTextIsTheTable)
	t.Run("payment", TestPaymentNoteIsTheTable)
	t.Run("summary", TestStatusSummary)
}
