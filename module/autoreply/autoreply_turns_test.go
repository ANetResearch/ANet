package autoreply

// autoreply_turns_test.go — the turn-level tests of the budget, the gate, the refusals and the sandbox work
// directories, written for the module's own stand-in node (turnfx_test.go) when MOD-4 moved the module out
// of internal/daemon (T-1 EXE F-2).
// Each keeps the name and the rule number of the test it replaces.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/localday"
	"github.com/ANetResearch/ANet/internal/pathsafe"
	"github.com/ANetResearch/ANet/module"
)

const fxPeerA = "bafyreipeera"

// checkBudgetFx checks what the requester of ix was told when the budget turned its task away (BU-4, LEC
// §2.4): rejected, anet.reason=budget, the wait until the provider's next local midnight, and no text.
func checkBudgetFx(t *testing.T, fx *turnFx, ix string) {
	t.Helper()
	x := fx.ix(ix)
	if x.state != "rejected" || x.reason != module.ReasonBudget {
		t.Fatalf("the requester was told state %q reason %q", x.state, x.reason)
	}
	_, end := localday.Bounds(time.Now())
	want := time.Until(end)
	if x.retryAfter <= 0 || x.retryAfter > want+5*time.Second || x.retryAfter < want-time.Minute {
		t.Fatalf("retry after %v, want about %v (to local midnight)", x.retryAfter, want)
	}
	if _, sent := fx.lastSent(ix); sent {
		t.Fatalf("the refusal carried text")
	}
}

// BU-1, BU-3, BU-4 (M-BU-1, M-BU-3, M-BU-4): an untrusted peer's sixth task of the day is refused with
// anet.reason=budget and the wait until local midnight, without starting the agent or calling the model —
// for a sandboxed agent and for the openai backend alike. The refusal is recorded once.
func TestUntrustedRunsAreBudgeted(t *testing.T) {
	for _, backend := range []string{"sandbox", "openai"} {
		t.Run(backend, func(t *testing.T) {
			var fx *turnFx
			var calls func() int64
			switch backend {
			case "sandbox":
				runs := execStub(t)
				fakeSandbox(t, nil)
				fx = newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
				calls = func() int64 { return int64(runs()) }
			default:
				api := &fakeOpenAI{reply: "an answer"}
				fx = newTurnFx(t, AutoReplyConfig{Model: "m"}, api)
				calls = api.calls.Load
			}
			for i := 1; i <= runsPerPeerPerDayDefault; i++ {
				ix := fx.deliver(fxPeerA, false, "task")
				fx.tick()
				if got, _ := fx.lastSent(ix); got == "" {
					t.Fatalf("task %d got no reply", i)
				}
			}
			if n := calls(); n != runsPerPeerPerDayDefault {
				t.Fatalf("%d runs for five tasks", n)
			}
			ix := fx.deliver(fxPeerA, false, "a sixth task today")
			fx.tick()
			fx.tick()
			checkBudgetFx(t, fx, ix)
			if n := calls(); n != runsPerPeerPerDayDefault {
				t.Fatalf("the agent or model ran for the task over the budget (%d runs)", n)
			}
			recs := fx.invoked()
			if len(withExit(recs, exitBudget)) != 1 || len(withExit(recs, exitOK)) != runsPerPeerPerDayDefault {
				t.Fatalf("records: %v", recs)
			}
			if r := withExit(recs, exitBudget)[0]; r["trusted"] != false || r["sandboxed"] != false || r["peer_aid"] != fxPeerA {
				t.Fatalf("the budget record: %v", r)
			}
			if got := fx.m.budget.today(nil); got != runsPerPeerPerDayDefault {
				t.Fatalf("today's count %d", got)
			}
		})
	}
}

// BU-3: the daily budget spans peers: with two runs a day, a third peer is turned away the same day.
func TestTheDailyBudgetSpansPeers(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(2)}}, api)
	var ids []string
	for _, peer := range []string{"bafyreiaaa", "bafyreibbb", "bafyreiccc"} {
		ids = append(ids, fx.deliver(peer, false, "task"))
	}
	fx.tick()
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("%d model calls, want 2", n)
	}
	refused := 0
	for _, ix := range ids {
		if x := fx.ix(ix); x.state == "rejected" {
			if x.reason != module.ReasonBudget {
				t.Fatalf("refused with %q", x.reason)
			}
			refused++
		}
	}
	if refused != 1 {
		t.Fatalf("%d of three peers refused, want 1", refused)
	}
}

// BU-2 (M-BU-2): the day's count survives a restart, rebuilt from the evidence chain.
func TestTheBudgetSurvivesARestart(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerPeerPerDay: Budget(2)}}, api)
	for i := 0; i < 2; i++ {
		ix := fx.deliver(fxPeerA, false, "task")
		fx.tick()
		if got, _ := fx.lastSent(ix); got == "" {
			t.Fatalf("task %d got no reply", i)
		}
	}
	fx.restart()
	if got := fx.m.SandboxBudget(); got.Today != 2 || got.RunsPerDay != 20 || got.RunsPerPeerPerDay != 2 {
		t.Fatalf("after the restart: %+v", got)
	}
	if _, v, _ := fx.m.reserveRun(fx.cfg, fxPeerA); v != budgetSpent {
		t.Fatalf("the peer's third run after the restart: %v", v)
	}
	if tk, v, _ := fx.m.reserveRun(fx.cfg, "bafyreianotherpeer"); v != budgetGranted {
		t.Fatalf("another peer after the restart: %v", v)
	} else {
		tk.release(false)
	}
}

// BU-4 outbound, GT-2: this node's own task over the budget sends nothing and stays in the inbox; the turn
// is not retried every scan, and a refusal for the same reason is logged and recorded once.
func TestAnOutboundTurnOverTheBudgetStaysInTheInbox(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(1)}}, api)
	// Today's only run is already used (a budget of 0 would be untrusted=off, not a refusal).
	if tk, v, _ := fx.m.budget.reserve("someone-else", 1, 1, fx.m.budgetRunsOnChain); v != budgetGranted {
		t.Fatal(v)
	} else {
		tk.release(true)
	}
	out := fx.deliverOutbound(fxPeerA, false, "a question", "an answer that asks for more")
	fx.tick()
	fx.tick()
	fx.say(out, "and another")
	fx.tick()
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("the model was called %d times", n)
	}
	if x := fx.ix(out); x.state == "rejected" || x.state == "completed" || len(x.sent) != 0 || x.refusals != 0 {
		t.Fatalf("the outbound task was answered or ended: %+v", x)
	}
	if recs := withExit(fx.invoked(), exitBudget); len(recs) != 1 {
		t.Fatalf("%d budget records for one interaction, want 1", len(recs))
	}
}

// BU-3: a budget of 0 is untrusted = off. The task stays in the inbox, nothing runs and no refusal is
// recorded, since "try again tomorrow" would not be true.
func TestABudgetOfZeroIsUntrustedOff(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(0)}}, api)
	in := fx.deliver(fxPeerA, false, "task")
	out := fx.deliverOutbound(fxPeerA, false, "a question", "more")
	fx.tick()
	fx.tick()
	if n := api.calls.Load(); n != 0 {
		t.Fatalf("the model was called %d times", n)
	}
	if recs := withExit(fx.invoked(), exitBudget); len(recs) != 0 {
		t.Fatalf("%d budget records for a budget of 0, want none", len(recs))
	}
	for _, ix := range []string{in, out} {
		if x := fx.ix(ix); x.state == "rejected" || x.state == "completed" || x.refusals != 0 || len(x.sent) != 0 {
			t.Fatalf("%s was answered, refused or ended: %+v", ix, x)
		}
	}
}

// BU-2: a turn the sandbox could not take started nothing and does not count: a peer with one run a day is
// not spent by it, and the refusal is recorded once per interaction.
func TestARunTheSandboxRefusedDoesNotCount(t *testing.T) {
	code := noSandbox(t)
	runs := execStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox,
		Sandbox: &AutoReplySandboxConfig{RunsPerPeerPerDay: Budget(1)}}, nil)
	for i := 0; i < 2; i++ {
		ix := fx.deliver(fxPeerA, false, "task")
		fx.tick()
		if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable {
			t.Fatalf("task %d: the requester was told %q %q (a refused run must not spend the budget)", i, x.state, x.reason)
		}
	}
	if runs() != 0 || fx.m.budget.today(nil) != 0 {
		t.Fatalf("%d runs, %d counted", runs(), fx.m.budget.today(nil))
	}
	if recs := withExit(fx.invoked(), exitSandboxUnavailable); len(recs) != 2 || (code != "" && recs[0]["unavailable"] != code) {
		t.Fatalf("records: %v", recs)
	}
}

// BU-5: untrusted peers' turns run beside the scan, at most two at once and one per peer; a turn with no
// free slot is neither refused nor lost but taken by a later scan.
func TestUntrustedRunsTakeTurns(t *testing.T) {
	fx := newTurnFx(t, AutoReplyConfig{Model: "m"}, &fakeOpenAI{reply: "x"})
	r := &slotReplier{peerOf: map[string]string{}, nowPeer: map[string]int{}}
	fx.replier = r
	var ids []string
	for _, from := range []string{"bafyreia", "bafyreia", "bafyreib", "bafyreic"} {
		id := fx.deliver(from, false, "task")
		r.peerOf[id] = from
		ids = append(ids, id)
	}
	fx.tick()
	if len(r.done) != 2 || r.max != 2 {
		t.Fatalf("first scan: %d turns answered, at most %d at once; want 2 and 2", len(r.done), r.max)
	}
	for scan := 2; len(r.done) < 4; scan++ {
		before := len(r.done)
		fx.tick()
		if len(r.done) == before || scan > 4 {
			t.Fatalf("scan %d: %d turns answered in all, want 4", scan, len(r.done))
		}
	}
	if r.maxPeer != 1 || r.max != 2 {
		t.Fatalf("at once: %d in all, %d for one peer; want 2 and 1", r.max, r.maxPeer)
	}
	for _, id := range ids {
		if fx.ix(id).state == "rejected" {
			t.Fatalf("%s was refused for want of a slot", id)
		}
	}
}

// BU-7: `anet autoreply test` is outside the budget.
func TestTheSelfTestIsOutsideTheBudget(t *testing.T) {
	api := &fakeOpenAI{reply: "I am a test"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(1)}}, api)
	for i := 0; i < 3; i++ {
		if _, err := fx.m.TestAutoReply(context.Background(), ""); err != nil {
			t.Fatalf("self-test %d: %v", i, err)
		}
	}
	if got := fx.m.SandboxBudget(); got.Today != 0 || got.RunsPerDay != 1 {
		t.Fatalf("budget after three self-tests: %+v", got)
	}
}

// GT-4, BU-1: on the openai backend a trusted peer's turns do not count either: with no runs a day for
// untrusted peers, a trusted peer is answered every time and nothing is counted.
func TestATrustedPeerOnTheOpenAIBackendIsNotBudgeted(t *testing.T) {
	api := &fakeOpenAI{reply: "an answer"}
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(0)}}, api)
	for i := 0; i < 2; i++ {
		ix := fx.deliver(fxPeerA, true, "task")
		fx.tick()
		if got, _ := fx.lastSent(ix); got != "an answer" {
			t.Fatalf("task %d: reply %q (a budget of 0 does not apply to a trusted peer)", i, got)
		}
	}
	if n := api.calls.Load(); n != 2 {
		t.Fatalf("%d model calls, want 2", n)
	}
	recs := fx.invoked()
	if len(recs) != 2 || recs[0]["mode"] != modeOpenAI || recs[0]["trusted"] != true || len(withExit(recs, exitOK)) != 2 {
		t.Fatalf("records: %v", recs)
	}
	if n := fx.m.budget.today(nil); n != 0 {
		t.Fatalf("a trusted peer's turns counted %d", n)
	}
}

// GT-1: a trusted peer runs as auto_reply.trusted says — directly by default, in the sandbox with
// trusted=sandbox — and neither counts against the budget; GT-2: a turn decided for the sandbox never
// falls back to a direct run, for a trusted peer either.
func TestTrustedPeersRunAsTrustedSays(t *testing.T) {
	stub := writeStub(t, "#!/bin/sh\nif [ \"$ANET_FAKE_SANDBOX\" = 1 ]; then echo IN-SANDBOX; else echo DIRECT; fi\n")
	setExecCommand(t, stub)
	sandboxRuns := fakeSandbox(t, nil)
	for _, tc := range []struct {
		trusted, want, mode string
	}{{"", "DIRECT", modeDirect}, {TrustedDirect, "DIRECT", modeDirect}, {TrustedSandbox, "IN-SANDBOX", modeSandbox}} {
		fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Trusted: tc.trusted,
			Sandbox: &AutoReplySandboxConfig{RunsPerDay: Budget(0)}}, nil)
		ix := fx.deliver(fxPeerA, true, "task")
		fx.tick()
		if got, _ := fx.lastSent(ix); got != tc.want {
			t.Fatalf("trusted=%q: reply %q, want %q (a budget of 0 does not apply to a trusted peer)", tc.trusted, got, tc.want)
		}
		recs := fx.invoked()
		if len(recs) != 1 || recs[0]["mode"] != tc.mode || recs[0]["trusted"] != true {
			t.Fatalf("trusted=%q: records %v", tc.trusted, recs)
		}
	}
	if sandboxRuns.Load() != 1 {
		t.Fatalf("%d sandboxed runs", sandboxRuns.Load())
	}

	// trusted=sandbox where the sandbox cannot run: refused, never run directly.
	sandboxRunnerForTest = nil
	code := noSandbox(t)
	runs := execStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Trusted: TrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, true, "task")
	fx.tick()
	if runs() != 0 {
		t.Fatal("a trusted peer's sandboxed turn fell back to a direct run")
	}
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable || len(x.sent) != 0 {
		t.Fatalf("the requester was told %+v", x)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["exit"] != exitSandboxUnavailable || (code != "" && recs[0]["unavailable"] != code) {
		t.Fatalf("records: %v", recs)
	}
}

// EG-7: a sandboxed run's record carries what it did through its egress endpoint.
func TestASandboxedRunRecordsItsEgress(t *testing.T) {
	execStub(t)
	fakeSandbox(t, sampleEgress())
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "task")
	fx.tick()
	if got, _ := fx.lastSent(ix); got == "" {
		t.Fatal("no reply")
	}
	recs := fx.invoked()
	if len(recs) != 1 {
		t.Fatalf("records: %v", recs)
	}
	r := recs[0]
	eg, _ := r["egress"].(map[string]any)
	conn, _ := eg["connect"].(map[string]any)
	allowed, _ := conn["allowed"].(map[string]any)
	refused, _ := eg["policy_refused"].(map[string]any)
	if r["sandboxed"] != true || r["sandbox"] != sandboxKind || r["mode"] != modeSandbox || eg["model_requests"] != float64(3) ||
		allowed["pypi.org"] != float64(2) || refused["policy_server_tool"] != float64(1) || conn["bytes_down"] != float64(1<<20) {
		t.Fatalf("the record: %v", r)
	}
}

// BU-8: a run whose work dir and run directories grow past the limit is killed, both are deleted, and the
// task is refused with sandbox_unavailable; the record says resource, and the run counts.
func TestADiskHogIsKilled(t *testing.T) {
	stub := writeStub(t, "#!/bin/sh\nhead -c 4194304 /dev/zero > hog\nexec sleep 30\n")
	setExecCommand(t, stub)
	fakeSandbox(t, nil)
	oldMax, oldEvery := sandboxDiskMax, sandboxDiskEvery
	sandboxDiskMax, sandboxDiskEvery = 1<<20, 20*time.Millisecond
	t.Cleanup(func() { sandboxDiskMax, sandboxDiskEvery = oldMax, oldEvery })
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "fill the disk")
	start := time.Now()
	fx.tick()
	if el := time.Since(start); el > 15*time.Second {
		t.Fatalf("the run was not killed (the scan took %s)", el)
	}
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable {
		t.Fatalf("the requester was told %+v", x)
	}
	work := filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(fx.n.AID()), execwork.IxDirName(ix))
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the work dir survived: %v", err)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["exit"] != exitResource || recs[0]["sandboxed"] != true {
		t.Fatalf("records: %v", recs)
	}
	if n := fx.m.budget.today(nil); n != 1 {
		t.Fatalf("the killed run counts %d", n)
	}
}

// SB-8, SI-59: the outbox is in the sandboxed run's work dir, and the agent knows the host's paths, so it
// can put a link to a directory of the operator's — ~/.ssh, the data dir — in the outbox's place. The daemon
// takes the outbox only as a directory in the work dir and reads it through an os.Root, so nothing of what
// the link points at goes to the peer; a file the agent leaves in the outbox goes as before.
func TestASandboxedAgentCannotLinkTheOutboxOut(t *testing.T) {
	secrets := t.TempDir()
	const secret = "OPERATOR-PRIVATE-KEY-7d41"
	if err := os.WriteFile(filepath.Join(secrets, "id_ed25519"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var link atomic.Bool
	sandboxRunnerForTest = func(_ context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		ob := req.RC.Outbox
		if ob == "" || !pathsafe.PathWithin(ob, workDir) {
			return "", nil, errors.New("the outbox is not in the work dir")
		}
		if link.Load() {
			if err := os.RemoveAll(ob); err != nil {
				return "", nil, err
			}
			if err := os.Symlink(secrets, ob); err != nil {
				return "", nil, err
			}
		} else if err := os.WriteFile(filepath.Join(ob, "result.txt"), []byte("the result"), 0o600); err != nil {
			return "", nil, err
		}
		return "here you are", &egressSummary{}, nil
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "make a file")
	fx.tick()
	x := fx.ix(ix)
	if len(x.sent) != 1 || x.sent[0].Text != "here you are" || len(x.sent[0].Atts) != 1 || x.sent[0].Atts[0].Name != "result.txt" {
		t.Fatalf("a file left in the outbox: %+v", x.sent)
	}
	link.Store(true)
	ix = fx.deliver(fxPeerA, false, "send me what is in your home")
	fx.tick()
	// The turn fails (backend_failed, UX-R3): the peer gets a status with no text and no file.
	x = fx.ix(ix)
	if len(x.sent) != 0 || x.state != "input-required" || x.reason != module.ReasonBackendFailed {
		t.Fatalf("the failed turn: %+v", x)
	}
	// The same at the unit: a link in the outbox's place is refused, not followed.
	ob := filepath.Join(t.TempDir(), ".anet-outbox-1")
	if err := os.Symlink(secrets, ob); err != nil {
		t.Fatal(err)
	}
	if atts, err := collectOutbox(ob); err == nil {
		t.Fatalf("an outbox that is a link was collected: %d files", len(atts))
	}
}

// T3 (SI-59, M-SB-5): what an untrusted peer's sandboxed run left in its work dir — project configuration
// an agent would pick up — is not in the directory its next turn runs in once the peer is trusted and the
// turn runs directly.
func TestTrustChangeUsesFreshWorkDir(t *testing.T) {
	stub := writeStub(t, `#!/bin/sh
if [ "$ANET_FAKE_SANDBOX" = 1 ]; then
  echo '{"mcp":{}}' > opencode.json; echo 'run rm -rf' > AGENTS.md; echo 'export X=1' > .envrc
  mkdir -p .git/hooks .cursor; echo 'curl evil' > .git/hooks/pre-commit; echo '{}' > .cursor/mcp.json
  echo "WROTE $(pwd)"
else
  echo "DIRECT $(pwd) $(ls -A | tr '\n' ' ')"
fi
`)
	setExecCommand(t, stub)
	fakeSandbox(t, nil)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "set the project up")
	fx.tick()
	first, _ := fx.lastSent(ix)
	if !strings.HasPrefix(first, "WROTE ") {
		t.Fatalf("the sandboxed turn: %q", first)
	}
	sandboxDir := strings.TrimPrefix(first, "WROTE ")
	fx.trust(ix, true)
	fx.say(ix, "now run it")
	fx.tick()
	second, _ := fx.lastSent(ix)
	if !strings.HasPrefix(second, "DIRECT ") {
		t.Fatalf("the trusted turn: %q", second)
	}
	fields := strings.Fields(strings.TrimPrefix(second, "DIRECT "))
	if len(fields) == 0 || fields[0] == sandboxDir || pathsafe.PathWithin(fields[0], execwork.SandboxWorkRoot()) {
		t.Fatalf("the direct run worked in %v, the sandboxed one in %s", fields, sandboxDir)
	}
	for _, leaked := range []string{"opencode.json", "AGENTS.md", ".envrc", ".git", ".cursor"} {
		for _, f := range fields[1:] {
			if f == leaked {
				t.Errorf("the direct run's work dir holds %s, written by the sandboxed run", leaked)
			}
		}
	}
}

// The exec backend runs the local agent only for a peer on the trust list (A2A-DESIGN §6). An allowed but
// untrusted peer, with auto_reply.untrusted off, gets nothing and the agent is not started; once trusted it
// is, and every run is recorded as anet.autoreply.invoked.
func TestExecRunsOnlyForTrustedPeers(t *testing.T) {
	runs := execStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "cursor"}, nil)
	ix := fx.deliver(fxPeerA, false, "run something")
	fx.tick()
	if n := runs(); n != 0 {
		t.Fatalf("the agent ran %d times for an untrusted peer", n)
	}
	if n := len(fx.invoked()); n != 0 {
		t.Fatalf("%d invocations recorded for an untrusted peer", n)
	}
	fx.trust(ix, true)
	fx.tick()
	trustedRuns := runs()
	if trustedRuns == 0 {
		t.Fatal("the agent did not run for a trusted peer")
	}
	if got, _ := fx.lastSent(ix); got != "EXEC-OK" {
		t.Fatalf("reply %q", got)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["trusted"] != true || recs[0]["sandboxed"] != false || recs[0]["exit"] != exitOK ||
		recs[0]["agent"] != "cursor" || recs[0]["interaction_id"] != ix || recs[0]["peer_aid"] != fxPeerA {
		t.Fatalf("invocation events: %v", recs)
	}
	// Taken off the trust list, the peer stops reaching the agent at the next turn.
	fx.trust(ix, false)
	fx.say(ix, "again")
	fx.tick()
	if n := runs(); n != trustedRuns {
		t.Fatalf("the agent ran again after the peer left the trust list (%d → %d runs)", trustedRuns, n)
	}
}

// auto_reply.untrusted=sandbox fails closed by direction when the sandbox cannot be used: an inbound task is
// answered status{rejected, anet.reason=sandbox_unavailable}; an outbound one sends nothing and is not
// retried until a new message. The agent is never started unconfined.
func TestSandboxUnavailableFailsClosed(t *testing.T) {
	noSandbox(t)
	runs := execStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "cursor", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "run something")
	fx.tick()
	if n := runs(); n != 0 {
		t.Fatalf("the agent ran %d times without a sandbox", n)
	}
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable {
		t.Fatalf("inbound task after a refused sandbox turn: %+v", x)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["exit"] != exitSandboxUnavailable || recs[0]["sandboxed"] != false {
		t.Fatalf("invocation evidence = %v", recs)
	}
	out := fx.deliverOutbound(fxPeerA, false, "a question for you", "an answer that asks the agent to act")
	fx.tick()
	fx.tick()
	if n := runs(); n != 0 {
		t.Fatalf("the agent ran %d times without a sandbox", n)
	}
	if x := fx.ix(out); x.state == "rejected" || x.state == "completed" || len(x.sent) != 0 || x.refusals != 0 {
		t.Fatalf("an outbound task was answered, refused or ended by a refused sandbox turn: %+v", x)
	}
	if n := len(fx.invoked()); n != 2 {
		t.Fatalf("%d invocation events, want 2 (one per refused turn, not per scan)", n)
	}
}

// A failing backend is reported to the requester as a status, input-required with anet.reason backend_failed
// and no text (UX-R3), exactly once: the loop does not retry-storm, and nothing of the failure's detail
// reaches the peer.
func TestAutoReplyBackendFailure(t *testing.T) {
	api := &fakeOpenAI{fail: true}
	fx := newTurnFx(t, AutoReplyConfig{Model: "test"}, api)
	ix := fx.deliver(fxPeerA, true, "hello?")
	fx.tick()
	fx.tick()
	if calls := api.calls.Load(); calls != 1 {
		t.Fatalf("backend called %d times, want exactly 1", calls)
	}
	if x := fx.ix(ix); x.state != "input-required" || x.reason != module.ReasonBackendFailed || len(x.sent) != 0 {
		t.Fatalf("requester's task: %+v", x)
	}
	// The requester writes again: that is a new turn, and the backend is called once more.
	fx.say(ix, "again?")
	fx.tick()
	if calls := api.calls.Load(); calls != 2 {
		t.Fatalf("backend called %d times after the follow-up, want 2", calls)
	}
}

// LEC §3.2, SB-8: auto_reply.work_dir may not lie in, or contain, the data dir or the sandbox work root:
// refused with work_dir_invalid when written, at start, and by a direct run; the sandbox never binds it.
func TestTheWorkDirStaysOutOfTheSandboxRoot(t *testing.T) {
	fx := newTurnFx(t, AutoReplyConfig{Model: "m", APIBase: "http://127.0.0.1:1/v1"}, nil)
	elsewhere := t.TempDir()
	for _, tc := range []struct {
		dir string
		bad bool
	}{
		{elsewhere, false},
		{filepath.Join(execwork.SandboxWorkRoot(), "x"), true},
		{filepath.Dir(execwork.SandboxWorkRoot()), true},
		{filepath.Join(fx.n.layout.Root, "work"), true},
	} {
		ref := checkWorkDir(&AutoReplyConfig{Backend: "exec", Agent: "claude", WorkDir: tc.dir}, fx.n.layout.Root)
		if tc.bad != (ref != nil && ref.Key == "work_dir_invalid") || (!tc.bad && ref != nil) {
			t.Errorf("work_dir %s: %v", tc.dir, ref)
		}
	}
	// A direct run refuses it too.
	r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: "claude", WorkDir: filepath.Join(execwork.SandboxWorkRoot(), "z")},
		dataDir: fx.n.layout.Root, aid: fx.n.AID()}
	if _, err := r.baseOpts(replyContext{InteractionID: "ix"}); err == nil {
		t.Fatal("a direct run took a work dir in the sandbox root")
	}
	// The configured work_dir is on the sandbox's never-bind list.
	fx.m.mu.Lock()
	fx.m.cfg = &AutoReplyConfig{Backend: "exec", Agent: "claude", WorkDir: elsewhere}
	fx.m.mu.Unlock()
	real, _ := filepath.EvalSymlinks(elsewhere)
	found := false
	for _, n := range fx.n.sandboxNeverBind() {
		found = found || n == real || n == elsewhere
	}
	if !found {
		t.Fatalf("the configured work_dir %s is not on the never-bind list", elsewhere)
	}
}

// LEC AV-4 (EXE-6): an agent the sandbox does not run, and a configuration whose model service does not
// carry the agent's API, fail closed at run time. The inbound task is answered status{rejected,
// anet.reason=sandbox_unavailable}, the local detail is agent_unsupported, and the agent never starts.
func TestUnsupportedAgentsFailClosedInTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  AutoReplyConfig
		base string
	}{
		{"cursor", AutoReplyConfig{Agent: agentCursor, APIKey: "k"}, ""},
		// a local model service: codex speaks only the Responses API, which the custom route does not carry;
		// claude only the Anthropic API, which only api.anthropic.com's route carries
		{"codex-custom", AutoReplyConfig{Agent: agentCodex, APIKey: "k", Model: "qwen3"}, "http://127.0.0.1:11434/v1"},
		{"claude-custom", AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "qwen3"}, "http://127.0.0.1:11434/v1"},
		{"opencode-anthropic", AutoReplyConfig{Agent: agentOpenCode, APIKey: "k", Model: "m"}, "https://api.anthropic.com"},
		{"hermes-anthropic", AutoReplyConfig{Agent: agentHermes, APIKey: "k", Model: "m"}, "https://api.anthropic.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := execStub(t)
			cfg := tc.cfg
			cfg.Backend, cfg.Untrusted, cfg.APIBase = "exec", UntrustedSandbox, tc.base
			fx := newTurnFx(t, cfg, nil)
			if _, err := resolveEgressUpstream(fx.cfg); err == nil {
				t.Fatalf("%+v resolves", fx.cfg)
			} else if pe := (*sandboxProbeError)(nil); !errors.As(err, &pe) || pe.Code != "agent_unsupported" {
				t.Fatalf("%+v: %v", fx.cfg, err)
			}
			ix := fx.deliver(fxPeerA, false, "run something")
			fx.tick()
			if n := runs(); n != 0 {
				t.Fatalf("the agent ran %d times", n)
			}
			if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable || len(x.sent) != 0 {
				t.Fatalf("inbound task: %+v", x)
			}
			recs := fx.invoked()
			if len(recs) != 1 || recs[0]["exit"] != exitSandboxUnavailable || recs[0]["unavailable"] != "agent_unsupported" {
				t.Fatalf("invocation evidence %v", recs)
			}
		})
	}
}
