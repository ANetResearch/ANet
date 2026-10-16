//go:build mod4_pending

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/localday"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// fakeSandbox makes a sandboxed turn run its agent stub (execCommandForTest) directly in the turn's
// sandbox work dir, with ANET_FAKE_SANDBOX=1 in its environment: everything around the start inside
// bubblewrap — the gate, the budget, the work dir, the disk watch, the record — is the real path, on a
// host without a usable bubblewrap. egress, when set, is what the run reports through its endpoint.
func fakeSandbox(t *testing.T, egress *egressSummary) *atomic.Int64 {
	t.Helper()
	var runs atomic.Int64
	sandboxRunnerForTest = func(ctx context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		runs.Add(1)
		cmd := exec.CommandContext(ctx, execCommandForTest)
		cmd.Dir = workDir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "ANET_FAKE_SANDBOX=1"}
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		e := egress
		if e == nil {
			e = &egressSummary{}
		}
		return strings.TrimSpace(string(out)), e, err
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	return &runs
}

// invoked is the provider's anet.autoreply.invoked records, oldest first, each checked against the
// registered payload (G-EVID).
func invoked(t *testing.T, d *Daemon) []map[string]any {
	t.Helper()
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvAutoReplyInvoked, Limit: 1000})
	var out []map[string]any
	for _, r := range recs {
		assertPayloadShape(t, EvAutoReplyInvoked, r.Payload)
		b, _ := json.Marshal(r.Payload)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		out = append(out, p)
	}
	return out
}

// withExit keeps the records that ended so.
func withExit(recs []map[string]any, exit string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["exit"] == exit {
			out = append(out, r)
		}
	}
	return out
}

// lastStatusText is the text of the last status message ix holds.
func lastStatusText(t *testing.T, d *Daemon, ix string) string {
	t.Helper()
	msgs, err := d.ix.Messages(ix)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == interactions.MsgStatus {
			return msgs[i].Body
		}
	}
	t.Fatalf("%s has no status message", ix)
	return ""
}

// checkBudgetRefusal checks what the requester of ix was told when the budget turned its task away
// (BU-4, LEC §2.4): rejected, anet.reason=budget, the wait until the provider's next local midnight,
// and no text.
func checkBudgetRefusal(t *testing.T, req *Daemon, ix string) {
	t.Helper()
	meta := awaitState(t, req, ix, interactions.StateRejected)
	if meta[a2ashape.KeyReason] != "budget" {
		t.Fatalf("the requester was told %v", meta)
	}
	wait, _ := meta[a2ashape.KeyRetryAfterMS].(float64)
	_, end := localday.Bounds(time.Now())
	want := float64(time.Until(end).Milliseconds())
	if wait <= 0 || wait > want+5000 || wait < want-60000 {
		t.Fatalf("anet.retry_after_ms %v, want about %v (to local midnight)", meta[a2ashape.KeyRetryAfterMS], want)
	}
	if text := lastStatusText(t, req, ix); text != "" {
		t.Fatalf("the refusal carried text: %q", text)
	}
}





// slotReplier answers after a while and records how many turns it answers at once, in all and per
// interaction's peer.
type slotReplier struct {
	peerOf  map[string]string // interaction id -> peer
	mu      sync.Mutex
	now     int
	nowPeer map[string]int
	max     int
	maxPeer int
	done    []string
}

func (r *slotReplier) Reply(ctx context.Context, rc replyContext, _ []chatTurn) (string, error) {
	r.mu.Lock()
	p := r.peerOf[rc.InteractionID]
	r.now++
	r.nowPeer[p]++
	r.max = max(r.max, r.now)
	r.maxPeer = max(r.maxPeer, r.nowPeer[p])
	r.mu.Unlock()
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.now--
	r.nowPeer[p]--
	r.done = append(r.done, rc.InteractionID)
	r.mu.Unlock()
	return "answered", nil
}
















func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// LEC §3.2, BU-3: budgets and trusted are checked wherever they are written: SetAutoReply and the
// control plane refuse with budget_invalid or config_invalid and the reason, the start check and New
// refuse a config.json edited by hand.
func TestAutoReplySettingsAreChecked(t *testing.T) {
	d := newBareDaemon(t)
	for body, key := range map[string]string{
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":2000}}`:                        "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":3,"runs_per_peer_per_day":4}}`: "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_peer_per_day":"many"}}`:             "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":{"n":1}}}`:                     "budget_invalid",
		`{"backend":"exec","agent":"claude","trusted":"always"}`:                                                  "config_invalid",
	} {
		rec := httptest.NewRecorder()
		d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
		var eb a2ashape.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &eb)
		if rec.Code != http.StatusBadRequest || eb.Reason != key {
			t.Errorf("%s: %d %s, want 400 %s", body, rec.Code, rec.Body, key)
		}
		if AutoReplyOf(d.config()) != nil {
			t.Fatalf("%s was saved", body)
		}
	}
	rec := httptest.NewRecorder()
	d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","trusted":"sandbox","sandbox":{"runs_per_day":7,"runs_per_peer_per_day":2}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid write: %d %s", rec.Code, rec.Body)
	}
	if got := d.SandboxBudget(); got.RunsPerDay != 7 || got.RunsPerPeerPerDay != 2 || AutoReplyOf(d.config()).TrustedMode() != TrustedSandbox {
		t.Fatalf("after the write: %+v %+v", got, AutoReplyOf(d.config()))
	}
	for _, ar := range []map[string]any{
		{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m", "sandbox": map[string]any{"runs_per_day": -1}},
		{"backend": "exec", "agent": "claude", "trusted": "yes"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": ar})
		var cr *ConfigRefusal
		if err := CheckConfigForStart(NewLayout(root)); !errors.As(err, &cr) {
			t.Errorf("start check for %v: %v", ar, err)
		}
		if n, err := New(NewLayout(root)); err == nil {
			n.Close()
			t.Errorf("the daemon started with %v", ar)
		}
	}
}

// LEC §2.5: a change of auto_reply.trusted or of a budget is recorded as anet.policy.changed with the
// values before and after; turning auto-reply on carries them in the auto_reply record.
func TestTrustedAndBudgetChangesAreRecorded(t *testing.T) {
	d := newBareDaemon(t)
	base := AutoReplyConfig{Model: "m", APIBase: "http://127.0.0.1:1/v1"}
	if err := d.SetAutoReply(&base); err != nil {
		t.Fatal(err)
	}
	next := base
	next.Trusted = TrustedSandbox
	next.Sandbox = &AutoReplySandboxConfig{RunsPerDay: Budget(9), RunsPerPeerPerDay: Budget(3)}
	if err := d.SetAutoReply(&next); err != nil {
		t.Fatal(err)
	}
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged, Limit: 100})
	got := map[string]string{}
	for _, r := range recs {
		f, _ := r.Payload["field"].(string)
		b, _ := json.Marshal([]any{r.Payload["from"], r.Payload["to"]})
		got[f] = string(b)
	}
	for field, want := range map[string]string{
		"auto_reply.trusted":                       `["direct","sandbox"]`,
		"auto_reply.sandbox.runs_per_day":          `[20,9]`,
		"auto_reply.sandbox.runs_per_peer_per_day": `[5,3]`,
	} {
		if got[field] != want {
			t.Errorf("%s: %s, want %s", field, got[field], want)
		}
	}
	if !strings.Contains(got["auto_reply"], `"runs_per_day":9`) || !strings.Contains(got["auto_reply"], `"trusted":"sandbox"`) {
		t.Errorf("the auto_reply record: %s", got["auto_reply"])
	}
}


