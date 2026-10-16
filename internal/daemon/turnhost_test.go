package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/execwork"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// turnsOf is the owed turns of d, by interaction.
func turnsOf(t *testing.T, d *Daemon) map[string]module.Turn {
	t.Helper()
	h := newTestHost(t, d)
	got, err := h.OwedTurns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]module.Turn{}
	for _, tu := range got {
		out[tu.Ix] = tu
	}
	return out
}

// newTestHost is the auto-reply module's Host on d.
func newTestHost(t *testing.T, d *Daemon) module.TurnHost {
	t.Helper()
	h, _ := newModuleHost(d, "autoreply")
	th, ok := h.(module.TurnHost)
	if !ok {
		t.Fatal("the autoreply Host grants no TurnHost")
	}
	return th
}

// MOD-3 (2), TestOwedTurnsExcludeBackendTasks: a task an A2A backend is given is not an owed turn, one
// that nobody has taken is, and so is a stranger's under policy open; a peer on the deny list is not.
func TestOwedTurnsExcludeBackendTasks(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	trusted := registered(t, srv.URL, "trusted")
	allowed := registered(t, srv.URL, "allowed")
	denied := registered(t, srv.URL, "denied")
	trustPeers(t, prov, trusted.AID())
	allowPeers(t, prov, allowed.AID(), denied.AID())
	if err := prov.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	if _, err := a2aHost(prov).InboundTasks(fctx); err != nil { // a backend takes the trusted peer's tasks
		t.Fatal(err)
	}
	idT, err := trusted.Delegate(ctx, prov.AID(), "from the trusted peer", nil)
	if err != nil {
		t.Fatal(err)
	}
	idA, err := allowed.Delegate(ctx, prov.AID(), "from an allowed peer", nil)
	if err != nil {
		t.Fatal(err)
	}
	idD, err := denied.Delegate(ctx, prov.AID(), "from a peer to be denied", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	denyPeers(t, prov, denied.AID())

	got := turnsOf(t, prov)
	if _, ok := got[idT]; ok {
		t.Errorf("the backend's task %s is owed to auto-reply", idT)
	}
	if _, ok := got[idD]; ok {
		t.Errorf("the denied peer's task %s is owed", idD)
	}
	tu, ok := got[idA]
	if !ok {
		t.Fatalf("the allowed peer's task is not owed; owed: %v", got)
	}
	if tu.Role != "inbound" || tu.Peer != allowed.AID() || tu.Trusted || tu.Goal != "from an allowed peer" ||
		len(tu.Messages) == 0 || tu.Messages[len(tu.Messages)-1].From != "them" {
		t.Errorf("turn = %+v", tu)
	}
}

// MOD-3 (2), TestOwedTurnsNeverCapabilityOrTerminal: the decision itself, on rows of each kind.
func TestOwedTurnsNeverCapabilityOrTerminal(t *testing.T) {
	d := newBareDaemon(t)
	ps := peerSets{deny: map[string]bool{}, allow: map[string]bool{}, trust: map[string]bool{"T": true}}
	last := func(from string) []ThreadMsg {
		return []ThreadMsg{{From: from, Kind: interactions.MsgText, Body: "hi"}}
	}
	row := func(mod func(*Thread)) Thread {
		th := Thread{InteractionID: "ix", Role: "inbound", Peer: "P", State: string(interactions.StateSubmitted), Messages: last("them")}
		mod(&th)
		return th
	}
	for name, c := range map[string]struct {
		th   Thread
		owed bool
	}{
		"plain":                {row(func(*Thread) {}), true},
		"outbound, peer wrote": {row(func(th *Thread) { th.Role = "outbound" }), true},
		"capability call":      {row(func(th *Thread) { th.IsCapability = true }), false},
		"public_cap":           {row(func(th *Thread) { th.Trust = interactions.TrustPublicCap }), false},
		"completed":            {row(func(th *Thread) { th.State = string(interactions.StateCompleted) }), false},
		"failed":               {row(func(th *Thread) { th.State = string(interactions.StateFailed) }), false},
		"our word is last":     {row(func(th *Thread) { th.Messages = last("me") }), false},
		"no text yet":          {row(func(th *Thread) { th.Messages = nil }), false},
		"control only": {row(func(th *Thread) {
			th.Messages = []ThreadMsg{{From: "them", Kind: interactions.MsgText, Body: "end", Metadata: json.RawMessage(`{"` + a2ashape.KeyState + `":"working"}`)}}
		}), false},
	} {
		if _, got := d.owedTurn(c.th, ps); got != c.owed {
			t.Errorf("%s: owed = %v, want %v", name, got, c.owed)
		}
	}
	if tu, ok := d.owedTurn(row(func(th *Thread) { th.Peer = "T" }), ps); !ok || !tu.Trusted {
		t.Errorf("a peer on the trust list: %+v %v", tu, ok)
	}
}

// MOD-3 (2), TestSendTurnRechecksDeny: the module asks for a send after the peer was denied: nothing goes
// out, ErrTurnGone.
func TestSendTurnRechecksDeny(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	req := registered(t, srv.URL, "req")
	allowPeers(t, prov, req.AID())
	id, err := req.Delegate(ctx, prov.AID(), "please", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	h := newTestHost(t, prov)
	if _, ok := turnsOf(t, prov)[id]; !ok {
		t.Fatal("the turn is not owed")
	}
	denyPeers(t, prov, req.AID())
	if err := h.SendTurn(ctx, id, "late reply", nil, false); !errors.Is(err, module.ErrTurnGone) {
		t.Fatalf("SendTurn to a denied peer = %v, want ErrTurnGone", err)
	}
	if got := lastFromMe(t, prov, id); got != "" {
		t.Fatalf("a message went out: %q", got)
	}
	if err := h.RefuseTurn(ctx, id, module.ReasonBackendFailed, 0); !errors.Is(err, module.ErrTurnGone) {
		t.Fatalf("RefuseTurn to a denied peer = %v", err)
	}
	if err := h.EndTurn(ctx, id); !errors.Is(err, module.ErrTurnGone) {
		t.Fatalf("EndTurn to a denied peer = %v", err)
	}
}

// MOD-3 (2), TestRefuseTurnReasonAllowlist: three reasons are taken, any other is refused before anything
// is sent; the refusal is a status without text.
func TestRefuseTurnReasonAllowlist(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	req := registered(t, srv.URL, "req")
	allowPeers(t, prov, req.AID())
	h := newTestHost(t, prov)
	for _, bad := range []string{"", "denied", "not_accepting", "anything", "rate_limited"} {
		id, err := req.Delegate(ctx, prov.AID(), "task "+bad, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.RefuseTurn(ctx, id, bad, 0); !errors.Is(err, module.ErrRefuseReason) {
			t.Errorf("RefuseTurn(%q) = %v, want ErrRefuseReason", bad, err)
		}
		if st := stateOf(t, prov, id); st != interactions.StateSubmitted {
			t.Errorf("a refused reason %q moved the task to %s", bad, st)
		}
	}
	for reason, want := range map[string]interactions.State{
		module.ReasonSandboxUnavailable: interactions.StateRejected,
		module.ReasonBudget:             interactions.StateRejected,
		module.ReasonBackendFailed:      interactions.StateInputRequired,
	} {
		id, err := req.Delegate(ctx, prov.AID(), "task "+reason, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.RefuseTurn(ctx, id, reason, time.Hour); err != nil {
			t.Fatalf("RefuseTurn(%s): %v", reason, err)
		}
		if st := stateOf(t, prov, id); st != want {
			t.Errorf("%s: state %s, want %s", reason, st, want)
		}
		if got := lastFromMe(t, prov, id); got != "" {
			t.Errorf("%s: a text went out: %q", reason, got)
		}
	}
}

// MOD-3 (2), TestNoTurnsBeforeStartup: before the daemon has finished starting nothing is owed.
func TestNoTurnsBeforeStartup(t *testing.T) {
	d := &Daemon{ready: make(chan struct{})}
	got, err := (turnPart{d}).OwedTurns(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("OwedTurns before ready = %v, %v", got, err)
	}
}

// MOD-3 (2), TestConfigRewritePreservesModuleBlocks: a write the kernel makes of config.json (a policy
// write) carries the auto_reply block and `without` byte for byte, and so does the module's own write the
// other keys.
func TestConfigRewritePreservesModuleBlocks(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"x402"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m", "api_key": "k"}})
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	read := func() map[string]json.RawMessage {
		b, err := os.ReadFile(filepath.Join(root, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	before := read()
	if err := d.SetInboundPolicy(PolicyApprove); err != nil {
		t.Fatal(err)
	}
	after := read()
	for _, k := range []string{"auto_reply", "without"} {
		if !bytes.Equal(before[k], after[k]) {
			t.Errorf("%s changed under a policy write:\n%s\n%s", k, before[k], after[k])
		}
	}
	// The module's write replaces only auto_reply.
	if _, err := d.ar().apply(func(cur *AutoReplyConfig) (*AutoReplyConfig, error) {
		c := *cur
		c.Model = "m2"
		return &c, nil
	}); err != nil {
		t.Fatal(err)
	}
	final := read()
	if !bytes.Equal(after["without"], final["without"]) || !bytes.Equal(after["inbound"], final["inbound"]) {
		t.Errorf("the module's write changed another key")
	}
	if !strings.Contains(string(final["auto_reply"]), `"m2"`) {
		t.Errorf("auto_reply = %s", final["auto_reply"])
	}
}

// MOD-3 (2), TestReadOwnEvidenceOnlyDeclaredTypes: the module reads back the types its row lists and no
// other.
func TestReadOwnEvidenceOnlyDeclaredTypes(t *testing.T) {
	d := newTestDaemon(t, "", true)
	h, _ := newModuleHost(d, "autoreply")
	ev := h.(module.OwnEvidenceHost)
	if err := h.RecordEvidence(EvAutoReplyInvoked, map[string]any{"agent": "claude"}); err != nil {
		t.Fatal(err)
	}
	got, err := ev.ReadOwnEvidence(EvAutoReplyInvoked, time.Now().Add(-time.Hour))
	if err != nil || len(got) != 1 || got[0].Payload["agent"] != "claude" {
		t.Fatalf("read back %v, %v", got, err)
	}
	for _, ty := range []string{evtypes.AutoReplyInvoked + ".x", EvPolicyChanged, "anet.payment.signed", ""} {
		if _, err := ev.ReadOwnEvidence(ty, time.Time{}); !errors.Is(err, module.ErrNotGranted) {
			t.Errorf("ReadOwnEvidence(%q) = %v, want ErrNotGranted", ty, err)
		}
	}
	// Another module's Host does not have the interface at all.
	if _, ok := mustHost(d, "cas").(module.OwnEvidenceHost); ok {
		t.Error("cas holds OwnEvidenceHost")
	}
}

func mustHost(d *Daemon, name string) module.Host { h, _ := newModuleHost(d, name); return h }

// MOD-3 (2), TestModuleConfigWriteRecordsPolicyChange: each audited field changed on its own gets its own
// anet.policy.changed under the key's prefix; the module cannot write that event itself.
func TestModuleConfigWriteRecordsPolicyChange(t *testing.T) {
	d := newTestDaemon(t, "", true)
	base := AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox,
		Sandbox: &AutoReplySandboxConfig{EgressAllow: []string{"example.com"}}, APIBase: "https://api.anthropic.com", APIKey: "k"}
	if err := d.SetAutoReply(&base); err != nil {
		t.Fatal(err)
	}
	fields := func() []string {
		var out []string
		d.ledger.scan(EvPolicyChanged, 0, func(_ int64, p map[string]any) { out = append(out, p["field"].(string)) })
		return out
	}
	mark := len(fields())
	change := func(name string, mod func(c *AutoReplyConfig)) {
		t.Helper()
		next := base
		if next.Sandbox != nil {
			sb := *next.Sandbox
			next.Sandbox = &sb
		}
		mod(&next)
		if err := d.SetAutoReply(&next); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := fields()[mark:]
		mark += len(got)
		want := "auto_reply." + name
		if !slices.Contains(got, want) {
			t.Errorf("%s changed; events %v lack %s", name, got, want)
		}
		base = next
	}
	n := 7
	change("trusted", func(c *AutoReplyConfig) { c.Trusted = TrustedSandbox })
	change("sandbox.egress_allow", func(c *AutoReplyConfig) { c.Sandbox.EgressAllow = []string{"example.com", "example.org"} })
	change("sandbox.runs_per_day", func(c *AutoReplyConfig) { c.Sandbox.RunsPerDay = budgetOf(n) })
	change("sandbox.runs_per_peer_per_day", func(c *AutoReplyConfig) { c.Sandbox.RunsPerPeerPerDay = budgetOf(3) })
	change("api_key_set", func(c *AutoReplyConfig) { c.APIKey = "" })
	// Writing the same block again records nothing.
	if err := d.SetAutoReply(&base); err != nil {
		t.Fatal(err)
	}
	if got := fields()[mark:]; len(got) != 0 {
		t.Errorf("an unchanged write recorded %v", got)
	}
	// The module may not write the event.
	h, _ := newModuleHost(d, "autoreply")
	if err := h.RecordEvidence(EvPolicyChanged, map[string]any{"field": "auto_reply"}); !errors.Is(err, module.ErrNotGranted) {
		t.Errorf("RecordEvidence(anet.policy.changed) = %v, want ErrNotGranted", err)
	}
}

func budgetOf(n int) *BudgetValue {
	var v BudgetValue
	_ = json.Unmarshal([]byte(string(rune('0'+n))), &v)
	return &v
}

// MOD-3 (2), TestPreviewTurnOnlyWhenAvailable: a sink is given only while preview is available and the
// round asked for it; a daemon built without preview never gives one.
func TestPreviewTurnOnlyWhenAvailable(t *testing.T) {
	d := newTestDaemon(t, "", true)
	h := newTestHost(t, d)
	if sink, ok := h.PreviewTurn("ix-none"); ok || sink != nil {
		t.Fatalf("a sink for a round that asked for nothing: %v %v", sink, ok)
	}
	off := newBareDaemon(t)
	off.avail = mustAvail(t, "x402")
	if _, ok := (turnPart{off}).PreviewTurn("ix"); ok {
		t.Fatal("a sink without the preview module")
	}
}

func mustAvail(t *testing.T, without ...string) module.Availability {
	t.Helper()
	av, err := module.NewAvailability(without)
	if err != nil {
		t.Fatal(err)
	}
	return av
}

// MOD §7.1, TestOnlyAutoReplyAssertsTurnHost: no file of the kernel asserts a Host to module.TurnHost; the
// module does, and the grants are what hands the interface to it alone.
func TestOnlyAutoReplyAssertsTurnHost(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	exec := map[string]bool{}
	for _, f := range execFiles {
		exec[f] = true
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ta, ok := n.(*ast.TypeAssertExpr)
			if !ok || ta.Type == nil {
				return true
			}
			if se, ok := ta.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "TurnHost" && !exec[name] {
				t.Errorf("%s asserts module.TurnHost; only the auto-reply module may", name)
			}
			return true
		})
	}
}

// MOD-3 (3), TestRetentionPrunesExecDirsInEveryBuild: with auto-reply turned off the work directories of
// ended tasks and the session bindings are still removed by the maintenance period.
func TestRetentionPrunesExecDirsInEveryBuild(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if d.ar() != nil {
		t.Fatal("the auto-reply module runs although it is in without")
	}
	aid := d.AID()
	direct := filepath.Join(execwork.WorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	sandboxed := filepath.Join(execwork.SandboxWorkRoot(), execwork.AidShort(aid), execwork.IxDirName("ix-gone"))
	for _, p := range []string{direct, sandboxed} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sess := execwork.Sessions(anethome.ModuleDir(d.layout.Root, "autoreply"))
	sess.Set("ix-gone", "s")
	d.pruneEndedWorkDirs()
	for _, p := range []string{direct, sandboxed} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was not removed (%v)", p, err)
		}
	}
	if sess.Get("ix-gone") != "" {
		t.Error("the session binding of an ended task stayed")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "exec_sessions.json")); err == nil {
		t.Error("the old session file exists")
	}
}

// MOD-3 (4): without the module nothing answers: its routes say module_absent, /status has no auto_reply
// member, no loop runs, and the task stays in the inbox under each inbound policy.
func TestWithoutAutoReplyIsAbsent(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"autoreply"}}, nil)
	if d.ar() != nil {
		t.Fatal("module started")
	}
	for _, route := range []string{"POST /autoreply", "POST /autoreply-test"} {
		rec := httptest.NewRecorder()
		h := d.moduleRouteGuard(route, d.moduleRoute("autoreply", route))
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/x", strings.NewReader(`{"off":true}`)))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 409 || body["reason"] != "module_absent" && !strings.Contains(rec.Body.String(), "module_absent") {
			t.Errorf("%s: %d %s", route, rec.Code, rec.Body)
		}
	}
	st := statusJSON(t, d)
	if _, ok := st["auto_reply"]; ok {
		t.Error("/status has an auto_reply member")
	}
	if got, _ := st["build"].(map[string]any)["without"].([]any); len(got) != 1 || got[0] != "autoreply" {
		t.Errorf("/status.build.without = %v", got)
	}
	// A block with the module turned off is the module_absent.configured refusal at start.
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"autoreply"},
		"auto_reply": map[string]any{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m"}})
	_, err := New(NewLayout(root))
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["key"] != "auto_reply" {
		t.Errorf("New = %v, want module_absent.configured for auto_reply", err)
	}
}

// restartWithout restarts d on its data directory with names written into `without`.
func restartWithout(t *testing.T, d *Daemon, names ...string) *Daemon {
	t.Helper()
	layout := d.layout
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(layout.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["without"] = names
	b, _ = json.Marshal(m)
	if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	n.stopRelayLoop()
	t.Cleanup(func() { n.Close() })
	return n
}

// MOD-3 (4), TestNoAutoReplyLeavesTasksInInbox: with the module turned off a delegated task is kept and
// nothing is sent, whatever the inbound policy, and no local program is started (a canary on PATH).
func TestNoAutoReplyLeavesTasksInInbox(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "claude")
	mark := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(canary, []byte("#!/bin/sh\ntouch "+mark+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(canary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, policy := range []string{PolicyOpen, PolicyApprove, PolicyClosed} {
		srv := newFakeHub(t)
		ctx := context.Background()
		prov := registered(t, srv.URL, "prov")
		req := registered(t, srv.URL, "req")
		allowPeers(t, prov, req.AID())
		trustPeers(t, prov, req.AID())
		if err := prov.SetInboundPolicy(policy); err != nil {
			t.Fatal(err)
		}
		prov = restartWithout(t, prov, "autoreply")
		if prov.ar() != nil {
			t.Fatal("the module started although it is in without")
		}
		id, err := req.Delegate(ctx, prov.AID(), "please run", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = prov.pollOnce(ctx)
		prov.kickTurns()
		time.Sleep(100 * time.Millisecond)
		if got := lastFromMe(t, prov, id); got != "" {
			t.Errorf("policy %s: the node answered %q", policy, got)
		}
		if st := stateOf(t, prov, id); policy != PolicyClosed && st != interactions.StateSubmitted {
			t.Errorf("policy %s: state %s", policy, st)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Error("a local program was started")
	}
}

// MOD-3 (6), TestDarwinPendingFailsClosed: with the macOS sandbox not part of the build
// (seatbeltEnabled false, stood in for on this host), a turn that would run in the sandbox is turned away
// before the agent starts with sandbox_unavailable and the local reason platform_unsupported.pending; the
// direct and openai paths are not touched; and the constant is true.
func TestDarwinPendingFailsClosed(t *testing.T) {
	if !seatbeltEnabled {
		t.Fatal("seatbeltEnabled must default to true")
	}
	prev := sandboxPrepareForTest
	sandboxPrepareForTest = func(*sandboxPlan) error { return seatbeltGate(false) }
	t.Cleanup(func() { sandboxPrepareForTest = prev })
	srv, req, prov := quietPair(t)
	_ = srv
	allowPeers(t, prov, req.AID())
	cfg := AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox,
		APIKey: "k", APIBase: "https://api.anthropic.com"}
	if err := prov.SetAutoReply(&cfg); err != nil {
		t.Fatal(err)
	}
	prov.ar().haltLoop()
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), "needs the sandbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	replier, err := newAutoReplier(cfg, testEnv(prov.layout))
	if err != nil {
		t.Fatal(err)
	}
	prov.autoReplyOnce(ctx, cfg, replier)
	state, meta := lastStatusMeta(t, req, id)
	if state != interactions.StateRejected || meta[a2ashape.KeyReason] != reasonSandboxUnavailble {
		t.Fatalf("state %s, meta %v", state, meta)
	}
	// The local reason is the one a status or doctor says.
	err = seatbeltGate(false)
	var pe *sandboxProbeError
	if !errors.As(err, &pe) || pe.Code != "platform_unsupported.pending" || !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("gate = %v", err)
	}
	if s := a2ashape.Sentence(pe.Code, a2ashape.English, nil); !strings.Contains(s, "v0.3.x") {
		t.Errorf("sentence %q", s)
	}
	// The openai backend and a direct run are not affected by the switch.
	if g := gateFor(AutoReplyConfig{Backend: "exec", Agent: "claude"}, true); g.mode != modeDirect {
		t.Errorf("a trusted peer's turn: %+v", g)
	}
	if g := gateFor(AutoReplyConfig{Model: "m", APIBase: "http://x"}, false); g.mode != modeOpenAI {
		t.Errorf("openai turn: %+v", g)
	}
}

// MOD-3 (3), TestModuleStopWaitsForRuns: Stop returns only when the turns under way have ended and
// recorded, which is before the evidence ledger closes, and no run starts after it.
func TestModuleStopWaitsForRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once, runs atomic.Int64
	sandboxRunnerForTest = func(ctx context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		runs.Add(1)
		if once.Add(1) == 1 {
			close(started)
		}
		<-release
		return "a late answer", &egressSummary{}, nil
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	f := newAutoReplyFixture(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	cfg := f.cfg
	if err := f.prov.SetAutoReply(&cfg); err != nil { // the real loop
		t.Fatal(err)
	}
	if _, err := f.req.Delegate(f.ctx, f.prov.AID(), "a long task", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.pollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.prov.kickTurns()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not start")
	}
	done := make(chan struct{})
	go func() { _ = f.prov.ar().Stop(context.Background()); close(done) }()
	select {
	case <-done:
		t.Fatal("Stop returned while a run was under way")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the run ended")
	}
	if got := withExit(invoked(t, f.prov), exitOK); len(got) != 1 {
		t.Fatalf("the run was not recorded before Stop returned: %v", invoked(t, f.prov))
	}
	// Nothing starts after Stop.
	if _, err := f.req.Delegate(f.ctx, f.prov.AID(), "a second task", nil); err != nil {
		t.Fatal(err)
	}
	_ = f.prov.pollOnce(f.ctx)
	f.prov.kickTurns()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d runs; one started after Stop", n)
	}
	if err := f.prov.ar().startLoop(cfg); err != nil {
		t.Fatal(err)
	}
	f.prov.kickTurns()
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("a stopped module started a loop again (%d runs)", n)
	}
}

// MOD-3 (5), TestModuleConfigCheckRefusesStart: a block the module does not take stops the daemon with the
// module's own sentence, at the start check and in New — not a log line and a node that runs without it.
func TestModuleConfigCheckRefusesStart(t *testing.T) {
	for name, c := range map[string]struct {
		block map[string]any
		key   string
	}{
		"untrusted":  {map[string]any{"untrusted": "maybe", "api_base": "http://127.0.0.1:1/v1", "model": "m"}, "config_invalid"},
		"budget":     {map[string]any{"api_base": "http://127.0.0.1:1/v1", "model": "m", "sandbox": map[string]any{"runs_per_day": "many"}}, "budget_invalid"},
		"work_dir":   {map[string]any{"backend": "exec", "agent": "claude", "work_dir": "/"}, "work_dir_invalid"},
		"wrong type": {map[string]any{"api_base": "http://127.0.0.1:1/v1", "model": 7}, "config_invalid"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": c.block})
		var cr *ConfigRefusal
		if err := CheckConfigForStart(NewLayout(root)); !errors.As(err, &cr) || cr.Refusals[0].Key != c.key {
			t.Errorf("%s: CheckConfigForStart = %v, want %s", name, err, c.key)
		}
		if d, err := New(NewLayout(root)); err == nil {
			d.Close()
			t.Errorf("%s: the daemon started", name)
		}
	}
}

// MOD-3 (5): a block that passes the start check but that its backend cannot run with (no model) stops the
// daemon at the module's start with the module's sentence, not a log line.
func TestModuleStartRefusesABackendWithoutWhatItNeeds(t *testing.T) {
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": map[string]any{"backend": "openai"}})
	d, err := New(NewLayout(root))
	if err == nil {
		d.Close()
		t.Fatal("the daemon started with an openai backend that has no address and no model")
	}
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "config_invalid" || ref.Args["key"] != "auto_reply.api_base" {
		t.Fatalf("New = %v", err)
	}
}
