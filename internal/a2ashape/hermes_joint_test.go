package a2ashape_test

// hermes_joint_test.go feeds the Hermes contract (hermes_contract_test.go)
// with responses that came over the wire: scripts/joint-a2a.sh has a2a-go's
// client (tools/a2aprobe) make blocking SendMessage calls through a real
// requester daemon, hub and provider daemon, and records each raw JSON-RPC
// response with what the answer was (tools/a2aprobe --record). The port of
// Hermes' client reads them here exactly as it reads the projected ones:
// the §17 contract row "Hermes' _reply_text_from_result, ported, finds the
// answer in what a blocking SendMessage returns", on the real thing.
//
// Without ANET_HERMES_JOINT the test has nothing to read and is skipped;
// the script sets it (with `go test`, or with a test binary built by
// scripts/testnet/build.sh where there is no Go).

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// jointCase is one case of the record tools/a2aprobe writes.
type jointCase struct {
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	ContextID string `json:"context_id"`
	Response  string `json:"response"`
	// WantState is the state the probe saw, as Hermes shortens it.
	WantState string `json:"want_state"`
	// WantReplyContains is text the answer holds: the provider's reply,
	// the digest a capability computed.
	WantReplyContains string `json:"want_reply_contains"`
	// Soft marks an expectation the projection may not meet yet: a miss
	// is logged as a gap.
	Soft bool   `json:"soft"`
	Why  string `json:"why"`
}

func TestHermesReadsJointRecord(t *testing.T) {
	path := os.Getenv("ANET_HERMES_JOINT")
	if path == "" {
		t.Skip("ANET_HERMES_JOINT is not set: it names the record scripts/joint-a2a.sh writes")
	}
	b, err := os.ReadFile(path)
	must(t, err)
	var rec struct {
		Cases []jointCase `json:"cases"`
	}
	must(t, json.Unmarshal(b, &rec))
	if len(rec.Cases) == 0 {
		t.Fatalf("%s holds no case", path)
	}
	for _, c := range rec.Cases {
		t.Run(c.Name, func(t *testing.T) {
			resp, err := pyLoads([]byte(c.Response))
			if err != nil {
				t.Fatalf("json.loads fails on the response: %v", err)
			}
			reply, ctx, state, err := pySendTask(c.Agent, c.ContextID, resp)
			if err != nil {
				t.Fatalf("Hermes' _send_task fails on the response: %v", err)
			}
			if c.ContextID != "" && pyStr(ctx) != c.ContextID {
				t.Errorf("Hermes continues in context %q, not the one of the call, %q", pyStr(ctx), c.ContextID)
			}
			short := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(pyStr(state), "TASK_STATE_"), "_", "-"))
			if short != c.WantState {
				t.Errorf("Hermes sees the state %q, the probe saw %q", short, c.WantState)
			}
			out := pyA2ACall(c.Agent, c.ContextID, 0, "", []byte(c.Response))
			t.Logf("what Hermes' model reads:\n%s", out)
			if want := "[" + c.Agent + " · context "; !strings.HasPrefix(out, want) {
				t.Errorf("a2a_call output does not start with %q", want)
			}
			var miss string
			switch {
			case c.WantReplyContains == "" && reply == "":
				miss = "no text at all"
			case c.WantReplyContains != "" && !strings.Contains(reply, c.WantReplyContains):
				miss = "the answer " + strings.TrimSpace(c.WantReplyContains) + " is not in what it reads"
			case hermesReceiptText(reply):
				miss = "it reads the anet.receipt artifact as the answer"
			}
			if strings.HasPrefix(c.Name, "text-") && short == "input-required" {
				// The call ended at the provider's final message, before
				// its result (tools/a2aprobe judgeAnswer): the answer is
				// there, under a header that asks for more input.
				t.Logf("gap: a finished text answer reaches Hermes as input-required, with its 'needs more input' hint")
			}
			switch {
			case miss == "":
			case c.Soft:
				t.Logf("gap (%s): %s", c.Why, miss)
			default:
				t.Errorf("%s; Hermes reads %q", miss, reply)
			}
		})
	}
}
