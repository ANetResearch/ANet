package daemon

// Tests of the module framework: one Host per module and what it is
// granted (K-16), the `without` list and the Availability value (K-1, K-2),
// a configuration block for a module that is not there (MOD-F1), the route
// set and module_absent (MOD-P6, MOD-F2), and the kernel's configuration
// write (K-2).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// startWith starts a daemon whose config.json carries the top-level members
// in extra (and the module blocks in modules), and returns it.
func startWith(t *testing.T, extra map[string]any, modules map[string]any) *Daemon {
	t.Helper()
	root := t.TempDir()
	cfg := map[string]any{"control_addr": "127.0.0.1:0"}
	for k, v := range extra {
		cfg[k] = v
	}
	if modules != nil {
		cfg["modules"] = modules
	}
	writeTestConfig(t, root, cfg)
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// K-16, TestHostGrantsFollowNameTable: for every module of the name table,
// each base Host method and each optional Host interface is available
// exactly as the row says, and what is not granted answers the refusal value.
func TestHostGrantsFollowNameTable(t *testing.T) {
	d := newTestDaemon(t, "", true)
	d.providers = provider.NewRegistry()
	for _, info := range module.Known() {
		t.Run(info.Name, func(t *testing.T) {
			h, st := newModuleHost(d, info.Name)
			has := info.Granted
			if got := h.Providers() != nil; got != has(module.GrantProviders) {
				t.Errorf("Providers granted = %v, row says %v", got, has(module.GrantProviders))
			}
			if _, ok := h.PaymentSeam(); ok != has(module.GrantPaymentSeam) {
				t.Errorf("PaymentSeam = %v, row says %v", ok, has(module.GrantPaymentSeam))
			}
			if _, ok := h.HubSeam(); ok != has(module.GrantHubSeam) {
				t.Errorf("HubSeam = %v, row says %v", ok, has(module.GrantHubSeam))
			}
			if _, ok := h.TaskSeam(); ok != has(module.GrantTaskSeam) {
				t.Errorf("TaskSeam = %v, row says %v", ok, has(module.GrantTaskSeam))
			}
			if _, ok := h.ResolveKEL(d.AID()); ok != has(module.GrantResolveKEL) {
				t.Errorf("ResolveKEL = %v, row says %v", ok, has(module.GrantResolveKEL))
			}
			if dir := h.StateDir(); (dir != "") != has(module.GrantStateDir) {
				t.Errorf("StateDir = %q, row says %v", dir, has(module.GrantStateDir))
			}
			_, inbound := h.(module.InboundTaskHost)
			_, signer := h.(module.ProxyCardSigner)
			_, transport := h.(module.TransportHost)
			_, sensitive := h.(module.SensitivePathsHost)
			for _, c := range []struct {
				name      string
				got, want bool
			}{
				{"InboundTaskHost", inbound, has(module.GrantInboundTaskHost)},
				{"ProxyCardSigner", signer, has(module.GrantProxyCardSigner)},
				{"TransportHost", transport, has(module.GrantTransportHost)},
				{"SensitivePathsHost", sensitive, has(module.GrantSensitivePaths)},
			} {
				if c.got != c.want {
					t.Errorf("%s asserted = %v, row says %v", c.name, c.got, c.want)
				}
			}
			// Evidence: a type in the row is written, one outside it and
			// anything under anet.policy.* is not.
			for _, ty := range info.EvidenceTypes {
				ty = strings.Replace(ty, "*", "probe", 1)
				if err := h.RecordEvidence(ty, map[string]any{"v": 1}); err != nil {
					t.Errorf("RecordEvidence(%s): %v", ty, err)
				}
			}
			for _, ty := range []string{"anet.policy.changed", "anet.unlisted.type", "anet.payment.outside"} {
				if info.MayRecord(ty) {
					continue
				}
				if err := h.RecordEvidence(ty, map[string]any{}); !errors.Is(err, module.ErrNotGranted) {
					t.Errorf("RecordEvidence(%s) = %v, want ErrNotGranted", ty, err)
				}
			}
			// DeclareUntrustedBackend: the grant takes effect; without it the
			// call is a failure the node cannot start on.
			d.untrustedBackend.Store(false)
			h.DeclareUntrustedBackend()
			if has(module.GrantDeclareUntrust) {
				if !d.untrustedBackend.Load() || st.deniedCall() != "" {
					t.Errorf("a granted DeclareUntrustedBackend had no effect")
				}
			} else if d.untrustedBackend.Load() || st.deniedCall() == "" {
				t.Errorf("an ungranted DeclareUntrustedBackend took effect or was not recorded")
			}
			d.untrustedBackend.Store(false)
		})
	}
}

// The name table lists the grants of the optional Host interfaces that have
// a Host type: a combination without one panics, which this test finds
// before a node does.
func TestEveryRowHasAHostType(t *testing.T) {
	d := &Daemon{}
	for _, info := range module.Known() {
		if info.Name == "autoreply" || info.Name == "preview" {
			// Their Host interfaces (TurnHost, PreviewHost) arrive with the
			// packages that build the seams; the base Host stands in.
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: %v", info.Name, r)
				}
			}()
			newModuleHost(d, info.Name)
		}()
	}
}

// A name the table does not have is granted nothing.
func TestUnknownModuleIsGrantedNothing(t *testing.T) {
	d := newTestDaemon(t, "", true)
	h, _ := newModuleHost(d, "no-such-module")
	if h.Providers() != nil || h.StateDir() != "" {
		t.Fatal("an unknown name was granted something")
	}
	if _, ok := h.TaskSeam(); ok {
		t.Fatal("an unknown name was granted the task seam")
	}
	if err := h.RecordEvidence("anet.shell.command", nil); !errors.Is(err, module.ErrNotGranted) {
		t.Fatalf("RecordEvidence = %v", err)
	}
}

// K-1: the Availability value belongs to its daemon. Two daemons in one
// process with different `without` do not see each other's, and neither is
// a process-wide setting a later test inherits.
func TestAvailabilityIsPerDaemon(t *testing.T) {
	a := startWith(t, map[string]any{"without": []string{"service", "x402"}}, nil)
	b := startWith(t, nil, nil)
	for _, c := range []struct {
		d    *Daemon
		name string
		want bool
	}{
		{a, "service", false}, {a, "x402", false}, {a, "p2p", true},
		{b, "service", true}, {b, "x402", true}, {b, "p2p", true},
	} {
		h, _ := newModuleHost(c.d, "blackboard")
		if got := h.Available(c.name); got != c.want {
			t.Errorf("daemon with without=%v: Available(%s) = %v, want %v", c.d.avail.Without(), c.name, got, c.want)
		}
	}
	c := startWith(t, nil, nil)
	if got := c.avail.Without(); len(got) != 0 {
		t.Fatalf("a later daemon inherited without=%v", got)
	}
}

// MOD-P5: a module in `without` is not built, not registered and not
// started, and /status.build says what the node contains and what it turned
// off.
func TestWithoutDisablesTheModule(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"x402"}}, nil)
	for _, m := range d.modules {
		if m.Name() == "x402" {
			t.Fatal("x402 was started although it is in without")
		}
	}
	if d.payer() != nil {
		t.Fatal("a payer is set although x402 is in without")
	}
	st := statusJSON(t, d)
	build, _ := st["build"].(map[string]any)
	if got := toStrings(build["without"]); !slices.Equal(got, []string{"x402"}) {
		t.Errorf("/status.build.without = %v", got)
	}
	if !slices.Contains(toStrings(build["modules"]), "x402") {
		t.Errorf("/status.build.modules = %v, which lacks the compiled module", build["modules"])
	}
	// And without the list, x402 starts.
	e := startWith(t, nil, nil)
	if !slices.ContainsFunc(e.modules, func(m module.Module) bool { return m.Name() == "x402" }) {
		t.Fatal("x402 was not started by default")
	}
	st = statusJSON(t, e)
	build, _ = st["build"].(map[string]any)
	if w, ok := build["without"].([]any); !ok || len(w) != 0 {
		t.Errorf("/status.build.without = %#v, want []", build["without"])
	}
}

func statusJSON(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest("GET", "/status", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status: %v: %s", err, rec.Body)
	}
	return out
}

func toStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		out = append(out, x.(string))
	}
	return out
}

// `without` is validated: an unknown name, a name twice and a module whose
// absent path has not landed are config_invalid, and the node does not start.
func TestWithoutIsValidated(t *testing.T) {
	for name, without := range map[string][]string{
		"unknown":   {"nothing"},
		"duplicate": {"x402", "x402"},
		"not ready": {"autoreply"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": without})
		_, err := New(NewLayout(root))
		var ref *a2ashape.Refusal
		if !errors.As(err, &ref) || ref.Key != "config_invalid" || ref.Args["key"] != "without" {
			t.Errorf("%s: New = %v, want config_invalid naming without", name, err)
		}
		err = CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		if !errors.As(err, &cr) || cr.Refusals[0].Key != "config_invalid" {
			t.Errorf("%s: CheckConfigForStart = %v", name, err)
		}
	}
}

// TestWithoutNamesOnlyReadyModules: the three modules whose absent path has
// not landed are refused in `without`, and every other name is accepted.
// MOD-3, MOD-5 and MOD-8 each flip their module in the same commit that
// lands the path, and change the list here.
func TestWithoutNamesOnlyReadyModules(t *testing.T) {
	notReady := []string{"autoreply", "preview"}
	for _, info := range module.Known() {
		_, err := module.NewAvailability([]string{info.Name})
		if want := slices.Contains(notReady, info.Name); (err != nil) != want {
			t.Errorf("without [%s]: err = %v, refused should be %v", info.Name, err, want)
		}
		if info.AbsenceReady == slices.Contains(notReady, info.Name) {
			t.Errorf("%s: AbsenceReady = %v", info.Name, info.AbsenceReady)
		}
	}
	// The sentence names the module.
	_, err := module.NewAvailability([]string{"autoreply"})
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) {
		t.Fatal(err)
	}
	if s := a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args); !strings.Contains(s, "autoreply") || strings.Contains(s, "{") {
		t.Errorf("sentence %q", s)
	}
}

// MOD-F1, TestConfiguredButUnavailableRefusesStart: a block for a module
// that is turned off or not in this binary stops the node with
// module_absent.configured, the same sentence from the start check and from
// the daemon.
func TestConfiguredButUnavailableRefusesStart(t *testing.T) {
	// Turned off.
	root := t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "without": []string{"x402"},
		"modules": map[string]any{"x402": map[string]any{}}})
	_, err := New(NewLayout(root))
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["key"] != "modules.x402" ||
		ref.Args["reason"] != module.AbsentWithout {
		t.Fatalf("New = %v, want module_absent.configured (without)", err)
	}
	startErr := a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args)
	check := CheckConfigForStart(NewLayout(root))
	var cr *ConfigRefusal
	if !errors.As(check, &cr) || len(cr.Refusals) != 1 ||
		a2ashape.Sentence(cr.Refusals[0].Key, a2ashape.English, cr.Refusals[0].Args) != startErr {
		t.Fatalf("the start check says %v, the daemon says %q", check, startErr)
	}
	if !strings.Contains(startErr, "anet init --with x402") {
		t.Errorf("the sentence does not say how to turn it back on: %q", startErr)
	}
	// Not in this binary: shell is additive and this test binary has none.
	if slices.Contains(module.Compiled(), "shell") {
		t.Skip("this binary contains shell")
	}
	root = t.TempDir()
	writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0",
		"modules": map[string]any{"shell": map[string]any{}}})
	_, err = New(NewLayout(root))
	if !errors.As(err, &ref) || ref.Key != "module_absent.configured" || ref.Args["reason"] != module.AbsentNotCompiledAdd {
		t.Fatalf("New = %v, want module_absent.configured (additive, not compiled)", err)
	}
	if s := a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args); !strings.Contains(s, "--shell") {
		t.Errorf("an additive module's sentence does not point at --shell: %q", s)
	}
}

// MOD-P3, TestModuleBlockForTopLevelKeysRefused: autoreply and preview keep
// their historical top-level keys; a block under modules is config_invalid
// in every build.
func TestModuleBlockForTopLevelKeysRefused(t *testing.T) {
	for _, m := range []string{"autoreply", "preview"} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0",
			"modules": map[string]any{m: map[string]any{}}})
		err := CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		if !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Key != "config_invalid" ||
			cr.Refusals[0].Args["key"] != "modules."+m || cr.Refusals[0].Args["rule"] != "top_level_key" {
			t.Fatalf("%s: %v", m, err)
		}
		if s := a2ashape.Sentence("config_invalid", a2ashape.Chinese, cr.Refusals[0].Args); !strings.Contains(s, "请写顶层 auto_reply / stream_preview") {
			t.Errorf("%s: %q", m, s)
		}
		if _, err := New(NewLayout(root)); err == nil {
			t.Errorf("%s: a node started with modules.%s", m, m)
		}
	}
	// A module that takes no block (mcp, console) and one that does not exist.
	for _, m := range []string{"mcp", "console", "nothing"} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0",
			"modules": map[string]any{m: map[string]any{}}})
		if err := CheckConfigForStart(NewLayout(root)); err == nil {
			t.Errorf("modules.%s was accepted", m)
		}
	}
}

// K-2, TestKernelWritePreservesOnDiskWithout: a write the kernel makes
// carries `without` and the blocks it does not parse from the disk, not from
// what it read at start, so a hand edit made while the daemon runs is not
// undone by the next peers or policy write.
func TestKernelWritePreservesOnDiskWithout(t *testing.T) {
	d := startWith(t, nil, nil)
	path := d.layout.ConfigPath()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	onDisk["without"] = []string{"cas"}
	onDisk["modules"] = map[string]any{"blackboard": map[string]any{"note": "hand-added"}}
	out, _ := json.MarshalIndent(onDisk, "", "  ")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	// A kernel write: the profile.
	if _, err := d.updateConfig(func(c *Config) error { c.Summary = "changed"; return nil },
		func(c *Config) { c.Summary = "changed" }); err != nil {
		t.Fatal(err)
	}
	// And the other writes the kernel makes: a policy write, a limit write,
	// an auto-reply write and leaving a hub.
	if err := d.SetInboundPolicy(d.config().inbound().Policy); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSpendLimits(map[string]uint64{"auto_max": 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetAutoReply(nil); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	var after Config
	if err := json.Unmarshal(b, &after); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.Without, []string{"cas"}) {
		t.Errorf("without after a kernel write = %v, want the disk's [cas]", after.Without)
	}
	if !strings.Contains(string(after.Modules["blackboard"]), "hand-added") {
		t.Errorf("modules after a kernel write = %v, want the disk's block", after.Modules)
	}
	if after.Summary != "changed" {
		t.Errorf("the kernel's own key was not written: %q", after.Summary)
	}
	// An unreadable file refuses the write rather than overwrite it.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.updateConfig(func(c *Config) error { return nil }, func(*Config) {}); err == nil {
		t.Error("a write over an unreadable config.json went through")
	}
	if got, _ := os.ReadFile(path); string(got) != "{not json" {
		t.Errorf("the unreadable file was overwritten: %q", got)
	}
}

// TestNoRouteWritesWithout: no control-plane route changes `without`, whatever
// it is sent. Every registered route is called with a body that names it.
func TestNoRouteWritesWithout(t *testing.T) {
	d := startWith(t, map[string]any{"without": []string{"cas"}}, nil)
	before, _ := os.ReadFile(d.layout.ConfigPath())
	var want struct{ Without []string }
	_ = json.Unmarshal(before, &want)
	p := newPlaneFor(t, d, "tok")
	body := `{"without":["x402"],"modules":{"x402":{}},"value":"x","interaction_id":"x","aid":"x","hub":"x"}`
	for _, pat := range p.cp.api.patterns {
		method, path, _ := strings.Cut(pat, " ")
		if path == "/shutdown" || strings.Contains(path, "{") {
			continue
		}
		// A route that waits for something (tasks/wait, pull) is cut short:
		// the question is what was written, not what was answered.
		r, err := http.NewRequest(method, p.srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer tok")
		if resp, err := (&http.Client{Timeout: time.Second}).Do(r); err == nil {
			resp.Body.Close()
		}
	}
	after, _ := os.ReadFile(d.layout.ConfigPath())
	var got struct{ Without []string }
	if err := json.Unmarshal(after, &got); err != nil || !slices.Equal(got.Without, want.Without) {
		t.Fatalf("without after calling every route = %v (%v), want %v", got.Without, err, want.Without)
	}
}

// MOD-P6, MOD-F2: a route the name table gives to a module answers 409
// module_absent while the module is not available, before the handler, and
// the handler when it is. The route is registered either way.
func TestAbsentRoutesAnswerModuleAbsent(t *testing.T) {
	absent := startWith(t, map[string]any{"without": []string{"x402"}}, nil)
	present := startWith(t, nil, nil)
	routes := module.Known()
	for _, info := range routes {
		if info.Name != "x402" {
			continue
		}
		for _, route := range info.Routes {
			method, path, _ := strings.Cut(route, " ")
			for name, d := range map[string]*Daemon{"absent": absent, "present": present} {
				p := newPlaneFor(t, d, "tok")
				if !slices.Contains(p.cp.api.patterns, route) {
					t.Fatalf("%s: %s is not registered", name, route)
				}
				resp, raw := p.req(t, method, path, `{}`, p.bearer)
				var body struct {
					Error  string `json:"error"`
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal(raw, &body)
				isAbsent := resp.StatusCode == http.StatusConflict && body.Reason == "module_absent"
				if (name == "absent") != isAbsent {
					t.Errorf("%s %s: %d %s", name, route, resp.StatusCode, raw)
				}
				if name == "absent" && !strings.Contains(body.Error, "payment module (x402") &&
					!strings.Contains(body.Error, "付款模块（x402") {
					t.Errorf("%s: sentence %q", route, body.Error)
				}
			}
		}
	}
}

// K-3: a module that implements module.ControlHandler is handed the routes
// the table gives it, and its answer is written; a module without the
// interface, or a route not its own, gets a 404.
type fakeStartOnly struct{ name string }

func (f fakeStartOnly) Name() string                             { return f.name }
func (f fakeStartOnly) Start(context.Context, module.Host) error { return nil }
func (f fakeStartOnly) Stop(context.Context) error               { return nil }

type fakeControlModule struct{ name string }

func (f fakeControlModule) Name() string                             { return f.name }
func (f fakeControlModule) Start(context.Context, module.Host) error { return nil }
func (f fakeControlModule) Stop(context.Context) error               { return nil }
func (f fakeControlModule) HandleControl(_ context.Context, route string, body []byte) (int, any, error) {
	return 201, map[string]string{"route": route, "body": string(body)}, nil
}

type fakeStatusModule struct{ name, key string }

func (f fakeStatusModule) Name() string                             { return f.name }
func (f fakeStatusModule) Start(context.Context, module.Host) error { return nil }
func (f fakeStatusModule) Stop(context.Context) error               { return nil }
func (f fakeStatusModule) StatusMember() (string, any)              { return f.key, "v" }
func (f fakeStatusModule) SI5() map[string]any                      { return nil }

type fakeLocalPaths struct{ path string }

func (f fakeLocalPaths) Name() string                             { return "service" }
func (f fakeLocalPaths) Start(context.Context, module.Host) error { return nil }
func (f fakeLocalPaths) Stop(context.Context) error               { return nil }
func (f fakeLocalPaths) LocalPaths() []string                     { return []string{f.path} }

func TestModuleRouteDispatchesToControlHandler(t *testing.T) {
	d := &Daemon{}
	d.modules = []module.Module{fakeControlModule{"autoreply"}}
	h := d.moduleRoute("autoreply", "POST /autoreply")
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/autoreply", strings.NewReader(`{"a":1}`)))
	if rec.Code != 201 || strings.TrimSpace(rec.Body.String()) != `{"body":"{\"a\":1}","route":"POST /autoreply"}` {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	// A route that is not the module's own.
	rec = httptest.NewRecorder()
	d.moduleRoute("autoreply", "POST /elsewhere")(rec, httptest.NewRequest("POST", "/elsewhere", nil))
	if rec.Code != 404 {
		t.Fatalf("a route outside the table answered %d", rec.Code)
	}
	// A module that does not implement it.
	d.modules = []module.Module{fakeStartOnly{"autoreply"}}
	rec = httptest.NewRecorder()
	d.moduleRoute("autoreply", "POST /autoreply")(rec, httptest.NewRequest("POST", "/autoreply", nil))
	if rec.Code != 404 {
		t.Fatalf("a module without ControlHandler answered %d", rec.Code)
	}
}

// K-4: the kernel takes only the /status member the table lists for the
// module's name.
func TestStatusContributorKeysFollowTheTable(t *testing.T) {
	d := &Daemon{}
	d.modules = []module.Module{fakeStatusModule{"autoreply", "auto_reply"}, fakeStatusModule{"cas", "auto_reply"},
		fakeStatusModule{"autoreply", "stolen"}}
	out := map[string]any{}
	d.statusMembers(out)
	if len(out) != 1 || out["auto_reply"] != "v" {
		t.Fatalf("members taken: %v", out)
	}
}

// K-5: the sensitive paths hold the kernel's fixed part, the default C1
// socket whatever the availability of anetlink, and the paths available
// modules declare.
func TestSensitivePaths(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, without := range [][]string{nil, {"anetlink"}} {
		d := startWith(t, map[string]any{"without": without}, nil)
		d.modules = append(d.modules, fakeLocalPaths{"/var/run/svc-test.sock"})
		got := d.sensitivePaths()
		for _, want := range []string{d.layout.Root, d.layout.ControlTokenPath(), "/var/run/svc-test.sock"} {
			if !slices.Contains(got, want) {
				t.Errorf("without=%v: %q missing from %v", without, want, got)
			}
		}
		// The sandbox's list holds the default C1 socket whether or not the
		// anetlink module is available (TestNeverBindIndependentOfModules).
		if never := d.sandboxNeverBind(); !slices.ContainsFunc(never, func(p string) bool { return strings.HasSuffix(p, "anetlink/c1.sock") }) {
			t.Errorf("without=%v: the sandbox never-bind list lacks the default C1 socket: %v", without, never)
		}
		if s := os.Getenv("XDG_RUNTIME_DIR"); !slices.Contains(got, s) {
			t.Errorf("without=%v: %q missing", without, s)
		}
		found := false
		for _, p := range got {
			if strings.HasSuffix(p, "anetlink/c1.sock") {
				found = true
			}
		}
		if !found {
			t.Errorf("without=%v: the default C1 socket is not in the list: %v", without, got)
		}
	}
}

// routeLines are the routes of a control plane, one line each, sorted:
// "<method and pattern>\t<owner>", the owner being "kernel" or the module
// the name table gives the route to.
func routeLines(cp *controlPlane) []string {
	seen := map[string]bool{}
	var out []string
	for _, pat := range append(append([]string{}, cp.top.patterns...), cp.api.patterns...) {
		if seen[pat] {
			continue
		}
		seen[pat] = true
		owner, ok := module.RouteOwner(pat)
		if !ok {
			owner = "kernel"
		}
		out = append(out, pat+"\t"+owner)
	}
	slices.Sort(out)
	return out
}

const routesGolden = "testdata/control_routes.txt"

// MOD-P6, TestRouteSetIsBuildInvariant: the control-plane routes are the
// same in every build and under every `without` (CI runs this test in each
// tagged build too). testdata/control_routes.txt is the list, with the
// owner of each route (R-13: a route is added here, and given to its module
// in module/known.go, in the same commit). ANET_UPDATE_GOLDEN=1 rewrites it.
func TestRouteSetIsBuildInvariant(t *testing.T) {
	var ready []string
	for _, k := range module.Known() {
		if k.AbsenceReady && k.Name != "shell" {
			ready = append(ready, k.Name)
		}
	}
	got := routeLines(startWith(t, nil, nil).ControlHandler("tok").(*controlPlane))
	for name, d := range map[string]*Daemon{
		"every ready module turned off": startWith(t, map[string]any{"without": ready}, nil),
		"x402 turned off":               startWith(t, map[string]any{"without": []string{"x402"}}, nil),
	} {
		if other := routeLines(d.ControlHandler("tok").(*controlPlane)); !slices.Equal(other, got) {
			t.Errorf("%s: the route set differs from the default one:\n%v\nvs\n%v", name, other, got)
		}
	}
	text := strings.Join(got, "\n") + "\n"
	if os.Getenv("ANET_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(routesGolden, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(routesGolden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != text {
		t.Errorf("%s is out of date (ANET_UPDATE_GOLDEN=1 rewrites it):\nhave %d routes, file has %d", routesGolden,
			len(got), strings.Count(string(want), "\n"))
	}
	// Every route the table gives a module is a route, and the other way round.
	inFile := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(want)), "\n") {
		pat, owner, _ := strings.Cut(l, "\t")
		inFile[pat] = owner
	}
	for _, k := range module.Known() {
		for _, r := range k.Routes {
			if inFile[r] != k.Name {
				t.Errorf("%s: the table gives %s to it, the file says %q", k.Name, r, inFile[r])
			}
		}
	}
}

// TestModuleRoutesAreBearerOnly: a route the name table gives to a module is
// not a console session route, and not a public one.
func TestModuleRoutesAreBearerOnly(t *testing.T) {
	for _, k := range module.Known() {
		if k.Name == "console" {
			continue // the console's routes are the session path itself (MOD-8)
		}
		for _, r := range k.Routes {
			if _, ok := sessionRoutes[r]; ok {
				t.Errorf("%s is a console session route, owned by %s", r, k.Name)
			}
			if publicRoutes[r] {
				t.Errorf("%s is a public route, owned by %s", r, k.Name)
			}
		}
	}
}

// SS-D8 and MOD-1: Grant key 5 carries this node's direct address, a fact
// the kernel decides from modules.p2p. When the p2p module is not available
// — turned off, or not in this binary — the advertised address is empty,
// whether or not the block is still in the configuration (the refusal to
// start does not carry this alone).
func TestGrantCarriesNoP2PWhenP2PAbsent(t *testing.T) {
	cfg := Config{Modules: ModulesConfig{"p2p": json.RawMessage(`{"socket":"/tmp/p.sock","advertise":"peer.example.org:4001"}`)}}
	off, err := module.NewAvailability([]string{"p2p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p2pAdvertise(cfg, off); got != "" {
		t.Errorf("p2p turned off: advertise %q", got)
	}
	var here module.Availability
	got := p2pAdvertise(cfg, here)
	if here.Available("p2p") {
		if got != "peer.example.org:4001" {
			t.Errorf("p2p available: advertise %q", got)
		}
	} else if got != "" { // a -tags no_p2p build
		t.Errorf("p2p not compiled in: advertise %q", got)
	}
}

// TestKernelReadsOnlyAllowedModuleBlocks (§3.4): the kernel reads a module's
// configuration block in four places and no other: the two checks that run
// whatever the build is, the module projection, and the one place a module's
// block decides what goes on the wire (caps.go, p2pAdvertise). A fifth is a
// module's configuration leaking into the kernel.
func TestKernelReadsOnlyAllowedModuleBlocks(t *testing.T) {
	allowed := map[string]string{
		"anetlink_config.go": "checkANetLinkConfig, before the module starts",
		"backend_policy.go":  "checkBackendPolicies, before the module starts",
		"modules.go":         "moduleConfig, the projection handed to module.Build",
		"caps.go":            "p2pAdvertise, which decides Grant key 5",
		"p2p_addr.go":        "checkP2PConfig, before the module starts",
		"setup.go":           "SI5, which reports modules.p2p.lan (UX-D5); the P2P module work item moves it",
		// MOD-3 removes the next line: autoreply reads its own block then.
		"autoreply_exec.go": "anetlinkSockets, the sandbox's never-bind list",
	}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(name)
		src := string(b)
		reads := strings.Contains(src, `Modules["`) || strings.Contains(src, `["modules"]`) || strings.Contains(src, `cfg.Modules`) ||
			strings.Contains(src, `range cfg.Modules`)
		if reads {
			if _, ok := allowed[name]; !ok {
				t.Errorf("%s reads a module's configuration block; the kernel may not (§3.4)", name)
			}
		}
	}
}
