package a2ashape_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/x402a2a"
)

// A hold keeps the quote and the task's state, gives the reason in the
// message and the task, and says in text what happens now (§8.3).
func TestPaymentHold(t *testing.T) {
	quote := map[string]any{"accepts": []any{map[string]any{"amount": "5"}}}
	task := a2ashape.Task{ID: "ix_1", ContextID: "c", Status: a2ashape.TaskStatus{State: a2ashape.TaskStateInputRequired},
		Metadata: map[string]any{a2ashape.KeyX402Status: a2ashape.PaymentRequired, a2ashape.KeyX402Required: quote}}
	held := a2ashape.PaymentHold(task, "m1", x402a2a.ReasonNeedsOperatorApproval, "run `anet pay ix_1`")
	m := held.Status.Message
	if m == nil || m.Role != a2ashape.RoleAgent || m.TaskID != "ix_1" || len(m.Parts) != 1 || m.Parts[0].Text != "run `anet pay ix_1`" ||
		m.Metadata[a2ashape.KeyX402Status] != a2ashape.PaymentRequired || m.Metadata[a2ashape.KeyX402Required] == nil ||
		m.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval {
		t.Fatalf("hold message: %+v", m)
	}
	if held.Metadata[a2ashape.KeyReason] != x402a2a.ReasonNeedsOperatorApproval || !a2ashape.IsPaymentHold(held) {
		t.Fatalf("hold: %+v", held.Metadata)
	}
	if _, ok := task.Metadata[a2ashape.KeyReason]; ok {
		t.Fatal("PaymentHold changed the task it was given")
	}
	if a2ashape.IsPaymentHold(task) || a2ashape.IsPaymentRefusal(held) {
		t.Fatal("a waiting task is not a hold, and a hold is not a refusal")
	}
}

// IsPaymentHold knows every sentence of ux D1 a decision leaves a quote to
// a person with, and nothing else.
func TestIsPaymentHoldKnowsEveryD1Code(t *testing.T) {
	waiting := func(reason string) a2ashape.Task {
		msg := &a2ashape.Message{ID: "m", Metadata: map[string]any{a2ashape.KeyReason: reason}}
		return a2ashape.Task{ID: "ix_1", Status: a2ashape.TaskStatus{State: a2ashape.TaskStateInputRequired, Message: msg}}
	}
	for _, r := range []string{"needs_operator_approval", "quote_above_card_price", "card_price_unavailable"} {
		if !a2ashape.IsPaymentHold(waiting(r)) {
			t.Errorf("%s is a hold", r)
		}
	}
	for _, r := range []string{"quote_expired", "rail_not_payable", "paid", ""} {
		if a2ashape.IsPaymentHold(waiting(r)) {
			t.Errorf("%q is not a hold", r)
		}
	}
}

// HPP PAY-D9, M-PAY-49: the code a client that did not activate a2a-x402
// was once told instead of needs_operator_approval is gone from the kernel,
// the local A2A interface and every other Go file of this repository; the
// reason table has no row for it.
func TestNoPaymentExtensionNotActivated(t *testing.T) {
	needle := "payment_extension" + "_not_activated"
	if _, ok := a2ashape.Lookup(needle); ok {
		t.Errorf("the reason table has %s", needle)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, dir := range []string{"internal", "module", "cmd", "tools"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n++
			if strings.Contains(string(b), needle) {
				t.Errorf("%s still says %s", path, needle)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n < 100 {
		t.Fatalf("only %d Go files scanned", n)
	}
}

// The sentence of a local refusal is the reason table's, in the node's
// language: the same one the CLI and the status line give, not a text of its own.
func TestPaymentRefusalDetailIsTheTablesSentence(t *testing.T) {
	for _, lang := range []string{"zh_CN.UTF-8", "en_US.UTF-8"} {
		t.Run(lang, func(t *testing.T) {
			t.Setenv("ANET_LANG", lang)
			for _, reason := range []string{x402a2a.ReasonClientPayloadUnsupported, x402a2a.ReasonOptionNotOffered, x402a2a.ReasonRailNotPayable} {
				got, want := a2ashape.PaymentRefusalDetail(reason), a2ashape.Sentence(reason, a2ashape.Lang(), nil)
				if got != want || got == "" || got == reason {
					t.Errorf("%s: detail %q, the table says %q", reason, got, want)
				}
			}
		})
	}
}

// A first join that has no recovery passphrase yet is told by this sentence
// too, so it must not read as if the user were switching hubs.
func TestTheRotationNeededSentenceCoversAFirstJoin(t *testing.T) {
	args := a2ashape.Args{"hub": "https://hub.example"}
	if s := a2ashape.Sentence("hub_switch_requires_rotation", a2ashape.English, args); !strings.Contains(s, "for the first time") ||
		!strings.Contains(s, "anet hub-register https://hub.example") {
		t.Errorf("en: %q", s)
	}
	if s := a2ashape.Sentence("hub_switch_requires_rotation", a2ashape.Chinese, args); !strings.Contains(s, "首次加入") {
		t.Errorf("zh: %q", s)
	}
}
