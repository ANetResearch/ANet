//go:build shell

package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

type shellHost struct {
	reg *provider.Registry
	mu  sync.Mutex
	ev  []struct {
		kind    string
		payload map[string]any
	}
}

func (h *shellHost) AID() string                   { return "node-1" }
func (h *shellHost) Providers() *provider.Registry { return h.reg }
func (h *shellHost) RecordEvidence(kind string, payload any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, _ := payload.(map[string]any)
	h.ev = append(h.ev, struct {
		kind    string
		payload map[string]any
	}{kind, m})
	return nil
}
func (h *shellHost) ResolveKEL(string) ([]identity.SignedEvent, bool) { return nil, false }
func (h *shellHost) PaymentSeam() (module.PaymentSeam, bool)          { return nil, false }
func (h *shellHost) HubSeam() (module.HubSeam, bool)                  { return nil, false }

func (h *shellHost) events(kind string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.ev {
		if e.kind == kind {
			out = append(out, e.payload)
		}
	}
	return out
}

var _ module.Host = (*shellHost)(nil)

func started(t *testing.T, cfg Config) (*Module, *shellHost) {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	h := &shellHost{reg: provider.NewRegistry()}
	m := &Module{cfg: cfg}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m, h
}

func invoke(t *testing.T, h *shellHost, capID, caller string, args map[string]any) (effect.Effect, error) {
	t.Helper()
	pv, ok := h.reg.Resolve(capID)
	if !ok {
		t.Fatalf("capability %q is not offered", capID)
	}
	return pv.Invoke(context.Background(), provider.Call{
		Capability: capID, Args: args, CallerAID: caller, CallID: "c1",
	})
}

// echoCfg admits local calls, because most cases below exercise what
// runs rather than who may ask. The cases about who may ask set it
// themselves.
func echoCfg() Config {
	return Config{AllowLocal: true, Commands: map[string]Command{
		"hello": {Run: "echo hello"},
		"fail":  {Run: "echo to-stderr >&2; exit 3"},
	}}
}

// ---- who may call ----

// The empty allowlist is the whole safety story for a node that was
// configured but never told who to trust. It must deny, not admit.
func TestNoAllowlistDeniesEveryRemoteCaller(t *testing.T) {
	_, h := started(t, echoCfg())
	eff, err := invoke(t, h, runPrefix+"hello", "did:anet:stranger", nil)
	if err != nil {
		t.Fatalf("a refusal is an effect, not a transport error: %v", err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("an unlisted caller must be refused, got %s: %+v", eff.Status, eff)
	}
	if eff.Evidence != nil && strings.Contains(eff.Evidence.ObservedState, "hello") {
		t.Fatal("the command ran anyway")
	}
	// A refusal nobody can review is a refusal that teaches the operator
	// nothing about who has been knocking.
	refused := h.events(EvCommandRefused)
	if len(refused) != 1 || refused[0]["caller"] != "did:anet:stranger" {
		t.Fatalf("the refusal must name the caller on the chain: %+v", refused)
	}
}

func TestListedCallerIsAllowed(t *testing.T) {
	cfg := echoCfg()
	cfg.Allow = []string{"did:anet:friend"}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"hello", "did:anet:friend", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK || !strings.Contains(eff.Evidence.ObservedState, "hello") {
		t.Fatalf("expected the command to run: %s %+v", eff.Status, eff.Evidence)
	}
}

// A call with no caller identity is refused unless the operator wrote
// the switch that admits one.
//
// The field defaults to the empty string, so treating empty as "local,
// therefore trusted" would make any future call site that forgets to
// populate it a silent bypass of the entire allowlist. Denying by
// default turns that same mistake into a visible refusal.
func TestCallWithNoCallerIdentityIsRefusedByDefault(t *testing.T) {
	cfg := echoCfg()
	cfg.AllowLocal = false
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("an unidentified call must be refused, got %s", eff.Status)
	}
	if len(h.events(EvCommandRefused)) != 1 {
		t.Fatal("the refusal must reach the chain")
	}
}

func TestLocalCallerIsAdmittedWhenTheOperatorSaysSo(t *testing.T) {
	cfg := echoCfg() // allow_local on, no allowlist at all
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("allow_local must admit a local call without an allowlist, got %s", eff.Status)
	}
}

// allow_local admits the local surface and nothing else: it is not a
// master switch that also turns off the network allowlist.
func TestAllowLocalDoesNotAdmitRemoteCallers(t *testing.T) {
	cfg := echoCfg() // allow_local on, allowlist empty
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"hello", "did:anet:stranger", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("allow_local must not admit a remote caller, got %s", eff.Status)
	}
}

// Revocation has to take effect on the next call, not the next restart:
// an operator revoking access is usually doing it because something is
// happening right now.
func TestAllowFileIsRereadSoAccessCanBeRevokedLive(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "allow")
	write := func(body string) {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("# who may run commands here\ndid:anet:friend\n")

	cfg := echoCfg()
	cfg.AllowFile = file
	_, h := started(t, cfg)

	if eff, _ := invoke(t, h, runPrefix+"hello", "did:anet:friend", nil); eff.Status != effect.OK {
		t.Fatalf("listed caller should run, got %s", eff.Status)
	}
	write("# revoked\n")
	eff, _ := invoke(t, h, runPrefix+"hello", "did:anet:friend", nil)
	if eff.Status != effect.Unavailable {
		t.Fatalf("removing the AID must take effect without a restart, got %s", eff.Status)
	}
	// And back again, so revocation is not a one-way trip that needs a
	// restart to undo either.
	write("did:anet:friend\n")
	if eff, _ := invoke(t, h, runPrefix+"hello", "did:anet:friend", nil); eff.Status != effect.OK {
		t.Fatalf("re-adding the AID must also take effect live, got %s", eff.Status)
	}
}

// A missing file is an empty allowlist, not an open one and not an error
// that takes the node down: deleting the file is a plausible way for an
// operator to revoke everything at once.
func TestMissingAllowFileDeniesRatherThanAdmits(t *testing.T) {
	cfg := echoCfg()
	cfg.AllowFile = filepath.Join(t.TempDir(), "never-written")
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"hello", "did:anet:friend", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("an absent allowlist must deny, got %s", eff.Status)
	}
}

// ---- what may be run ----

// The named commands are the operator's list. Arbitrary execution is a
// second, separate decision, and a node that made only the first one must
// not offer or accept the second.
func TestArbitraryExecutionIsAbsentUnlessEnabled(t *testing.T) {
	_, h := started(t, echoCfg())
	if _, ok := h.reg.Resolve(CapExec); ok {
		t.Fatal("shell.exec must not be offered when allow_arbitrary is off")
	}
	caps, err := (&shellProvider{m: &Module{cfg: echoCfg()}}).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c == CapExec {
			t.Fatal("shell.exec must not appear on the capability card")
		}
	}
	// Not merely unadvertised: a caller that asks for it by name anyway
	// is refused, because a card is not an enforcement point.
	m := &Module{cfg: echoCfg(), host: h}
	if _, err := (&shellProvider{m: m}).Invoke(context.Background(), provider.Call{
		Capability: CapExec, Args: map[string]any{"command": "echo pwned"},
	}); err == nil {
		t.Fatal("shell.exec must be refused when it was never enabled")
	}
}

func TestArbitraryExecutionWorksWhenEnabled(t *testing.T) {
	cfg := echoCfg()
	cfg.AllowArbitrary = true
	_, h := started(t, cfg)
	eff, err := invoke(t, h, CapExec, "", map[string]any{"command": "echo from-exec"})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK || !strings.Contains(eff.Evidence.ObservedState, "from-exec") {
		t.Fatalf("got %s %+v", eff.Status, eff.Evidence)
	}
}

// The command line belongs to the operator; the arguments come off the
// network. Quoting is what keeps the two from becoming the same thing.
func TestCallerArgumentsCannotBecomeCommands(t *testing.T) {
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		"say": {Run: "echo", Args: true},
	}}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"say", "", map[string]any{
		"argv": []any{"hi; touch /tmp/anet-shell-injection", "&& whoami", "$(id)", "`id`"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := eff.Evidence.ObservedState
	// Every metacharacter came back as text, which is only possible if
	// the shell treated the whole thing as one word.
	for _, want := range []string{"hi; touch", "&& whoami", "$(id)", "`id`"} {
		if !strings.Contains(out, want) {
			t.Fatalf("argument %q was interpreted rather than passed: %q", want, out)
		}
	}
	if _, err := os.Stat("/tmp/anet-shell-injection"); err == nil {
		os.Remove("/tmp/anet-shell-injection")
		t.Fatal("an injected argument executed")
	}
	if strings.Contains(out, "uid=") {
		t.Fatalf("command substitution ran: %q", out)
	}
}

// Dropping the arguments silently would run something other than what
// was asked for and then report success for it.
func TestArgumentsToACommandThatTakesNoneAreRefused(t *testing.T) {
	_, h := started(t, echoCfg())
	if _, err := invoke(t, h, runPrefix+"hello", "", map[string]any{"argv": []any{"x"}}); err == nil {
		t.Fatal("a command declared without args must refuse them, not ignore them")
	}
}

// ---- what is reported back ----

// A command that failed is not a call that failed: the node did what it
// was asked and the answer is the failure. Reporting OK with the output
// attached would have the caller act on a result that never happened.
func TestNonZeroExitIsFailedAndCarriesTheCodeAndOutput(t *testing.T) {
	_, h := started(t, echoCfg())
	eff, err := invoke(t, h, runPrefix+"fail", "", nil)
	if err != nil {
		t.Fatalf("a failing command is an effect, not a transport error: %v", err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("exit 3 must be FAILED, got %s", eff.Status)
	}
	if eff.Record.Metrics["exit_code"] != 3 {
		t.Fatalf("the exit code must survive: %+v", eff.Record.Metrics)
	}
	// stderr is where a failing command says why. Losing it leaves the
	// caller with a number.
	if !strings.Contains(eff.Evidence.ObservedState, "to-stderr") {
		t.Fatalf("stderr must be captured: %q", eff.Evidence.ObservedState)
	}
}

// A command that was killed must not look like one that finished with no
// output, which is what a caller would otherwise act on.
func TestTimeoutIsFailedAndKillsTheWholeProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survivor")
	cfg := Config{AllowLocal: true, Commands: map[string]Command{
		// A grandchild that outlives its shell unless the group is killed.
		"slow": {Run: "(sleep 2 && touch " + marker + ") & sleep 10", TimeoutS: 1},
	}}
	_, h := started(t, cfg)

	start := time.Now()
	eff, err := invoke(t, h, runPrefix+"slow", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("a killed command must be FAILED, got %s", eff.Status)
	}
	if !strings.Contains(eff.Message, "killed") {
		t.Fatalf("the message must say it was killed, got %q", eff.Message)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the timeout did not take effect: %s", d)
	}
	// The grandchild would have created the marker at t+2s.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a process survived the timeout: only the shell was killed, not its group")
	}
}

// Unbounded output is how one command takes down the daemon holding it.
func TestOutputIsBounded(t *testing.T) {
	cfg := Config{
		AllowLocal:     true,
		MaxOutputBytes: 1024,
		Commands:       map[string]Command{"flood": {Run: "yes anet | head -c 100000"}},
	}
	_, h := started(t, cfg)
	eff, err := invoke(t, h, runPrefix+"flood", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := eff.Evidence.ObservedState
	if len(out) > 1024+64 {
		t.Fatalf("output was not bounded: %d bytes", len(out))
	}
	// Silent truncation would have the caller read a cut-off result as a
	// complete one.
	if !strings.Contains(out, "truncated") {
		t.Fatalf("truncation must be stated in the output: %q", out[max(0, len(out)-100):])
	}
}

// Every execution is on this node's own chain before the caller hears
// back, so the record exists whether or not the answer ever arrives.
func TestEveryExecutionIsRecordedAsEvidence(t *testing.T) {
	cfg := echoCfg()
	cfg.Allow = []string{"did:anet:friend"}
	_, h := started(t, cfg)
	if _, err := invoke(t, h, runPrefix+"fail", "did:anet:friend", nil); err != nil {
		t.Fatal(err)
	}
	runs := h.events(EvCommandRun)
	if len(runs) != 1 {
		t.Fatalf("expected one record, got %d", len(runs))
	}
	r := runs[0]
	if r["caller"] != "did:anet:friend" || r["command"] != "fail" {
		t.Fatalf("the record must name who asked and what ran: %+v", r)
	}
	if r["exit_code"] != 3 {
		t.Fatalf("a failed run must be recorded as failed: %+v", r)
	}
}

// The list is what an operator reads to answer "what can this node be
// made to do, and by whom" without logging into it.
func TestListReportsCommandsAndCallerCount(t *testing.T) {
	cfg := echoCfg()
	cfg.Allow = []string{"did:anet:friend", "did:anet:other"}
	_, h := started(t, cfg)

	type listing struct {
		Commands       map[string]string `json:"commands"`
		AllowArbitrary bool              `json:"allow_arbitrary"`
		AllowedCallers int               `json:"allowed_callers"`
		AllowedAIDs    []string          `json:"allowed_caller_aids"`
	}
	read := func(caller string) listing {
		t.Helper()
		eff, err := invoke(t, h, CapList, caller, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got listing
		if err := json.Unmarshal([]byte(eff.Evidence.ObservedState), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	remote := read("did:anet:friend")
	if _, ok := remote.Commands["hello"]; !ok || remote.AllowArbitrary {
		t.Fatalf("unexpected listing: %+v", remote)
	}
	if remote.AllowedCallers != 2 {
		t.Fatalf("the caller count must be reported: %+v", remote)
	}
	// One accepted caller must not be able to read out every other
	// machine's operator.
	if len(remote.AllowedAIDs) != 0 {
		t.Fatalf("the roster must not go to a remote caller: %+v", remote.AllowedAIDs)
	}

	local := read("")
	if len(local.AllowedAIDs) != 2 || local.AllowedAIDs[0] != "did:anet:friend" {
		t.Fatalf("a local caller reads their own config in full: %+v", local.AllowedAIDs)
	}
}

// ---- configuration ----

func TestConfigRejectsWhatCannotBeEnforced(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no commands and no arbitrary switch", Config{}},
		{"a command with nothing to run", Config{Commands: map[string]Command{"x": {}}}},
		{"a name that collides with the capability grammar",
			Config{Commands: map[string]Command{"a b": {Run: "true"}}}},
		{"both an inline list and a file", Config{
			Commands: map[string]Command{"x": {Run: "true"}},
			Allow:    []string{"a"}, AllowFile: "/tmp/x",
		}},
		{"a timeout past the ceiling", Config{
			Commands: map[string]Command{"x": {Run: "true"}}, TimeoutS: 999999,
		}},
	} {
		if err := tc.cfg.validate(); err == nil {
			t.Errorf("%s must be rejected at load, not discovered at call time", tc.name)
		}
	}
}

// An unconfigured module is not a module that runs everything.
func TestAbsentConfigYieldsNoModule(t *testing.T) {
	m, err := newModule(nil)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("with no configuration the module must not start at all")
	}
}
