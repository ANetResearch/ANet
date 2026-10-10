package a2ashape_test

// anet.payment_note and anet.next on the task projection (core §2.13
// revision 7, HPP §2.7, PAY-D9): the stored decision is said as it was
// taken, it is status.message while the task waits on it, and the next step
// is the command of the row the sentence came from — the variant decides.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

const notePeer = "bafyreixtplpft7v4seh2kvj72ceuvw75efr6edt4sywb5wkh7d6kx2wxyz"

// waitingOn is a task this node started that waits on a quote, with n as
// its stored decision and reason as the payment's anet.reason (the
// daemon's PaymentReason).
func waitingOn(t *testing.T, n a2ashape.PayNote, reason string) a2ashape.Task {
	t.Helper()
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	quote := []byte(`{"x402Version":2,"accepts":[{"scheme":"credit","network":"anet-credit:h","amount":"12","asset":"credit","payTo":"` + notePeer + `"}]}`)
	ix := &interactions.Interaction{ID: "ix_9f3c", Role: interactions.RoleOutbound, PeerAID: notePeer,
		State: interactions.StateInputRequired, StateSeq: 4, StateAt: time.Now().UnixMilli(), ContextID: "c",
		IsCapability: true, PayState: interactions.PayRequired, PayRequired: quote, PayNote: b}
	pay := map[string]any{x402a2a.KeyStatus: x402a2a.StatusRequired, x402a2a.KeyRequired: json.RawMessage(quote)}
	if reason != "" {
		pay[x402a2a.KeyReason] = reason
	}
	return a2ashape.Project(a2ashape.Source{Interaction: ix, Payment: pay}, a2ashape.Options{})
}

func TestTaskProjectionCarriesNext(t *testing.T) {
	t.Setenv("LANG", "C")
	twelve := uint64(12)
	three := uint64(3)
	at := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		note   a2ashape.PayNote
		reason string
		next   string
	}{
		{a2ashape.PayNote{Code: "needs_operator_approval.manual_limit", Amount: 12, Tier: "auto", Single: 10, Left: 50,
			Key: "explicit_max", Value: &twelve, At: at}, "needs_operator_approval", "anet payments set explicit_max=12"},
		{a2ashape.PayNote{Code: "needs_operator_approval.over_limit", Amount: 12, Tier: "agent", Single: 5, Left: 44, At: at},
			"needs_operator_approval", "anet pay ix_9f3c"},
		{a2ashape.PayNote{Code: "needs_operator_approval.off", Amount: 12, Tier: "auto", At: at},
			"needs_operator_approval", "anet pay ix_9f3c"},
		{a2ashape.PayNote{Code: "needs_operator_approval.payee_not_allowed", Amount: 12, Tier: "auto", Peer: notePeer, At: at},
			"needs_operator_approval", "anet pay ix_9f3c"},
		{a2ashape.PayNote{Code: "needs_operator_approval.payees_unreadable", Amount: 12, Tier: "auto", At: at},
			"needs_operator_approval", "anet pay ix_9f3c"},
		{a2ashape.PayNote{Code: "quote_above_card_price", Amount: 12, Price: &three, Tier: "auto", At: at},
			"quote_above_card_price", "anet pay ix_9f3c"},
		{a2ashape.PayNote{Code: "card_price_unavailable", Amount: 12, Tier: "auto", Why: "timeout", At: at},
			"card_price_unavailable", "anet pay ix_9f3c"},
		// A payment that did not settle: the row of ux D3 decides.
		{a2ashape.PayNote{Code: "x402:INSUFFICIENT_FUNDS", Amount: 12, At: at}, "needs_operator_approval", "anet balance"},
		{a2ashape.PayNote{Code: "binding_mismatch", Amount: 12, At: at}, "needs_operator_approval", ""},
	} {
		task := waitingOn(t, c.note, c.reason)
		c.note.Task = "ix_9f3c"
		want := a2ashape.PaymentNote(c.note, a2ashape.English)
		if task.Metadata[a2ashape.KeyPaymentNote] != want {
			t.Errorf("%s: anet.payment_note %q, want %q", c.note.Code, task.Metadata[a2ashape.KeyPaymentNote], want)
		}
		m := task.Status.Message
		if m == nil || len(m.Parts) != 1 || m.Parts[0].Text != want || m.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentRequired ||
			m.Metadata[a2ashape.KeyX402Required] == nil {
			t.Errorf("%s: status.message %+v, want the sentence alone with the quote", c.note.Code, m)
		}
		next, has := task.Metadata[a2ashape.KeyNext]
		if c.next == "" {
			if has {
				t.Errorf("%s: anet.next %v for a sentence with no command", c.note.Code, next)
			}
			continue
		}
		if next != c.next {
			t.Errorf("%s: anet.next %v, want %q", c.note.Code, next, c.next)
		}
		if !strings.Contains(want, c.next) {
			t.Errorf("%s: the sentence %q does not name its command %q", c.note.Code, want, c.next)
		}
	}
	// A paid snapshot is no reason to wait: it is the payment note, never
	// status.message, and has no command.
	paid := waitingOn(t, a2ashape.PayNote{Code: "paid.auto", Amount: 3, Tier: "auto", Single: 5, Left: 44, At: at}, "")
	if _, has := paid.Metadata[a2ashape.KeyNext]; has || paid.Metadata[a2ashape.KeyPaymentNote] == nil ||
		(paid.Status.Message != nil && len(paid.Status.Message.Parts) == 1 &&
			paid.Status.Message.Parts[0].Text == paid.Metadata[a2ashape.KeyPaymentNote]) {
		t.Errorf("a paid snapshot on a waiting task: %+v", paid)
	}
	// Without a decision, a reason whose row has a command carries it with
	// the task's values; one whose rows differ by variant carries none.
	ended := &interactions.Interaction{ID: "ix_r", Role: interactions.RoleOutbound, PeerAID: notePeer,
		State: interactions.StateRejected, StateSeq: 2, ContextID: "c", ResultMeta: `{"anet.reason":"no_local_task"}`}
	task := a2ashape.Project(a2ashape.Source{Interaction: ended}, a2ashape.Options{})
	if task.Metadata[a2ashape.KeyNext] != a2ashape.Next("no_local_task", a2ashape.English, a2ashape.Args{"task": "ix_r"}) ||
		task.Metadata[a2ashape.KeyNext] == "" {
		t.Errorf("no_local_task: anet.next %v", task.Metadata[a2ashape.KeyNext])
	}
	if _, has := task.Metadata[a2ashape.KeyPaymentNote]; has {
		t.Error("a payment note on a task with no payment decision")
	}
	bare := waitingOn(t, a2ashape.PayNote{}, "needs_operator_approval")
	if _, has := bare.Metadata[a2ashape.KeyNext]; has {
		t.Errorf("needs_operator_approval with no variant: anet.next %v", bare.Metadata[a2ashape.KeyNext])
	}
	// A provider-side task never shows a payment note.
	inbound := &interactions.Interaction{ID: "ix_in", Role: interactions.RoleInbound, PeerAID: notePeer,
		State: interactions.StateInputRequired, ContextID: "c", PayNote: []byte(`{"code":"paid.auto","amount":1,"tier":"auto","single":1,"left":1,"at":"2026-10-10T09:00:00Z"}`)}
	if v, has := a2ashape.Project(a2ashape.Source{Interaction: inbound}, a2ashape.Options{}).Metadata[a2ashape.KeyPaymentNote]; has {
		t.Errorf("an inbound task's payment note: %v", v)
	}
}

// ParsePayNote takes its own form and nothing else.
func TestParsePayNoteIsStrict(t *testing.T) {
	good := `{"code":"paid.auto","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`
	if n, err := a2ashape.ParsePayNote([]byte(good)); err != nil || n.Code != "paid.auto" || n.Left != 44 {
		t.Fatalf("%+v %v", n, err)
	}
	for _, bad := range []string{
		``, `null`, `[]`, `{}`, good + ` {}`,
		`{"code":"paid.auto","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z","x":1}`,
		`{"code":"paid.auto","Code":"quote_expired","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"hub_refused","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"paid.auto","amount":3,"tier":"boss","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"paid.auto","amount":-3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z"}`,
		`{"code":"paid.auto","amount":3,"tier":"auto","single":5,"left":44,"at":"2026-10-10T09:00:00Z","task":"t"}`,
	} {
		if n, err := a2ashape.ParsePayNote([]byte(bad)); err == nil {
			t.Errorf("%s read as %+v", bad, n)
		}
	}
}
