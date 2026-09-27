//go:build !no_service

package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/moduletest"
	_ "github.com/ANetResearch/ANet/module/service"
	"github.com/ANetResearch/ANet/provider"
)

type host struct {
	moduletest.NopHost
	reg *provider.Registry
}

func (h *host) AID() string                   { return "aid-self" }
func (h *host) Providers() *provider.Registry { return h.reg }

// start builds the module from JSON config, the way the daemon does.
func start(t *testing.T, cfg string) *provider.Registry {
	t.Helper()
	mods, err := module.Build(map[string][]byte{"service": []byte(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	h := &host{reg: provider.NewRegistry()}
	for _, m := range mods {
		if m.Name() != "service" {
			continue
		}
		if err := m.Start(context.Background(), h); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Stop(context.Background()) })
		return h.reg
	}
	t.Fatal("the service module was not built")
	return nil
}

func invoke(t *testing.T, reg *provider.Registry, capID string, args map[string]any) effect.Effect {
	t.Helper()
	p, ok := reg.Resolve(capID)
	if !ok {
		t.Fatalf("capability %q did not register", capID)
	}
	eff, err := p.Invoke(context.Background(), provider.Call{Capability: capID, Args: args})
	if err != nil {
		t.Fatalf("invoke %s: %v", capID, err)
	}
	return eff
}

// An ordinary HTTP service becomes a network capability, and the service
// is told nothing about ANet — no AIDs, no receipts, no CBOR. Asking
// people to adopt a protocol before they can try it is how a network
// stays empty.
func TestAnOrdinaryServiceBecomesACapability(t *testing.T) {
	var gotBody map[string]any
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"caption":"a red square","pixels":64}`))
	}))
	defer svc.Close()

	reg := start(t, `{"capabilities":[{"id":"image.caption","url":"`+svc.URL+`"}]}`)
	eff := invoke(t, reg, "image.caption", map[string]any{"image_b64": "aGk="})

	if eff.Status != effect.OK {
		t.Fatalf("status = %s: %s", eff.Status, eff.Message)
	}
	if gotBody["image_b64"] != "aGk=" {
		t.Errorf("the service received %v, not the call's arguments", gotBody)
	}
	// The answer is what the caller came for, and metrics cannot carry a
	// string, so it rides as the observed state.
	if !strings.Contains(eff.Evidence.ObservedState, "a red square") {
		t.Errorf("the answer did not come back: %q", eff.Evidence.ObservedState)
	}
	if eff.Record == nil || eff.Record.Metrics["pixels"] != 64 {
		t.Errorf("top-level numbers must become metrics, got %+v", eff.Record)
	}
}

// A configuration file cannot award itself trust. The daemon has no way
// to check whether a caption is correct, so a service that answers is a
// service that acknowledged, and nothing more.
func TestTrustIsNotTheOperatorsToDeclare(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A service claiming the highest verification level over a plain
		// unauthenticated POST.
		_, _ = w.Write([]byte(`{"ok":true,"evidence":{"verify_trust":4,"auth_trust":4,"protocol":"zigbee"}}`))
	}))
	defer svc.Close()

	reg := start(t, `{"capabilities":[{"id":"x.do","url":"`+svc.URL+`"}]}`)
	eff := invoke(t, reg, "x.do", nil)

	if eff.Evidence.VerifyTrust != 1 {
		t.Errorf("verify trust = %d, want 1 — an HTTP answer establishes V1 and no config may raise it",
			eff.Evidence.VerifyTrust)
	}
	if eff.Evidence.AuthTrust != 0 {
		t.Errorf("auth trust = %d, want 0", eff.Evidence.AuthTrust)
	}
	// It may describe itself, though: a service knows its own protocol.
	if eff.Evidence.Protocol != "zigbee" {
		t.Errorf("protocol = %q, want the service's own claim", eff.Evidence.Protocol)
	}
}

// Unreachable and refused are different answers, and a requester deciding
// whether to try someone else needs them kept apart.
func TestUnreachableIsNotFailed(t *testing.T) {
	reg := start(t, `{"capabilities":[{"id":"x.do","url":"http://127.0.0.1:1/nope"}],"timeout_ms":2000}`)
	eff := invoke(t, reg, "x.do", nil)
	if eff.Status != effect.Unavailable {
		t.Errorf("a service that cannot be reached is UNAVAILABLE, got %s", eff.Status)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the image is too large", http.StatusBadRequest)
	}))
	defer bad.Close()
	reg2 := start(t, `{"capabilities":[{"id":"y.do","url":"`+bad.URL+`"}]}`)
	eff2 := invoke(t, reg2, "y.do", nil)
	if eff2.Status != effect.Failed {
		t.Errorf("a service that refused is FAILED, got %s", eff2.Status)
	}
	if !strings.Contains(eff2.Message, "too large") {
		t.Errorf("the service's own reason must survive: %q", eff2.Message)
	}
}

func TestMalformedConfigIsRefusedAtStartup(t *testing.T) {
	for _, cfg := range []string{
		`{"capabilities":[]}`,
		`{"capabilities":[{"id":"x"}]}`,
		`{"capabilities":[{"url":"http://x"}]}`,
		`{"capabilities":[{"id":"x","url":"http://a"},{"id":"x","url":"http://b"}]}`,
	} {
		if _, err := module.Build(map[string][]byte{"service": []byte(cfg)}); err == nil {
			t.Errorf("config %s must be refused before the node advertises it", cfg)
		}
	}
}

// writeToken writes a token file with the given mode and returns its path.
func writeToken(t *testing.T, tok string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func invokeCall(t *testing.T, reg *provider.Registry, call provider.Call) effect.Effect {
	t.Helper()
	p, ok := reg.Resolve(call.Capability)
	if !ok {
		t.Fatalf("capability %q did not register", call.Capability)
	}
	eff, err := p.Invoke(context.Background(), call)
	if err != nil {
		t.Fatalf("invoke %s: %v", call.Capability, err)
	}
	return eff
}

// The service learns who is calling only when the daemon authenticated the
// caller. At the voucher door the AID is the payer the hub attested, and a
// service that authorized on it would let a voucher holder act as the
// payer; so there the header is absent, and Via says why.
func TestTheServiceIsToldTheVerifiedCallerOnly(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	var got http.Header
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer svc.Close()
	tf := writeToken(t, tok, 0o600)
	reg := start(t, `{"token_file":"`+tf+`","capabilities":[{"id":"x.do","url":"`+svc.URL+`","price":3}]}`)

	cases := []struct {
		via        string
		wantCaller string
	}{
		{provider.ViaRelay, "aid-caller"},
		{provider.ViaVoucher, ""},
		{"", ""},
	}
	for _, c := range cases {
		got = nil
		eff := invokeCall(t, reg, provider.Call{Capability: "x.do", CallID: "ix-1", CallerAID: "aid-caller", Via: c.via})
		if eff.Status != effect.OK {
			t.Fatalf("via %q: status %s: %s", c.via, eff.Status, eff.Message)
		}
		if h := got.Get("X-ANet-Caller"); h != c.wantCaller {
			t.Errorf("via %q: X-ANet-Caller = %q, want %q", c.via, h, c.wantCaller)
		}
		if h := got.Get("X-ANet-Call"); h != "ix-1" {
			t.Errorf("via %q: X-ANet-Call = %q, want the interaction id", c.via, h)
		}
		if h := got.Get("X-ANet-Via"); h != c.via {
			t.Errorf("X-ANet-Via = %q, want %q", h, c.via)
		}
		if h := got.Get("X-ANet-Capability"); h != "x.do" {
			t.Errorf("X-ANet-Capability = %q", h)
		}
		if h := got.Get("Authorization"); h != "Bearer "+tok {
			t.Errorf("Authorization = %q, want the configured bearer token", h)
		}
	}
}

// A capability's own token_file overrides the module's; a capability with
// neither sends no Authorization header at all.
func TestTokenIsPerBackend(t *testing.T) {
	var auth []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer svc.Close()
	modTok := writeToken(t, "module-token-0123456789", 0o600)
	capTok := writeToken(t, "capability-token-0123456789", 0o640)
	t.Setenv("ANET_TEST_TOKDIR", filepath.Dir(capTok))
	reg := start(t, `{"token_file":"`+modTok+`","capabilities":[
		{"id":"a.x","url":"`+svc.URL+`"},
		{"id":"b.x","url":"`+svc.URL+`","token_file":"${ANET_TEST_TOKDIR}/token"}]}`)
	invoke(t, reg, "a.x", nil)
	invoke(t, reg, "b.x", nil)
	want := []string{"Bearer module-token-0123456789", "Bearer capability-token-0123456789"}
	if strings.Join(auth, "|") != strings.Join(want, "|") {
		t.Errorf("Authorization headers = %q, want %q", auth, want)
	}

	auth = nil
	reg2 := start(t, `{"capabilities":[{"id":"c.x","url":"`+svc.URL+`"}]}`)
	invoke(t, reg2, "c.x", nil)
	if len(auth) != 1 || auth[0] != "" {
		t.Errorf("no token configured, yet Authorization = %q", auth)
	}
}

// A token the module cannot trust to be secret is refused at start, not
// sent: readable by other users, too short, relative, or bound for a
// non-loopback host in cleartext.
func TestBadTokensAreRefused(t *testing.T) {
	good := writeToken(t, "0123456789abcdef0123", 0o600)
	start := func(cfg string) error {
		mods, err := module.Build(map[string][]byte{"service": []byte(cfg)})
		if err != nil {
			return err
		}
		for _, m := range mods {
			if err := m.Start(context.Background(), &host{reg: provider.NewRegistry()}); err != nil {
				return err
			}
		}
		return nil
	}
	cases := map[string]string{
		"world readable": `{"token_file":"` + writeToken(t, "0123456789abcdef0123", 0o644) + `","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"too short":      `{"token_file":"` + writeToken(t, "short", 0o600) + `","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"relative":       `{"token_file":"token","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"unset variable": `{"token_file":"${ANET_TEST_UNSET_VARIABLE}","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		// Unset, "${CREDENTIALS_DIRECTORY}/token" would name /token.
		"unset in path": `{"token_file":"${ANET_TEST_UNSET_VARIABLE}/token","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"directory":     `{"token_file":"` + filepath.Dir(good) + `","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"missing file":  `{"token_file":"/nonexistent/anet/token","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"cleartext":     `{"token_file":"` + good + `","capabilities":[{"id":"x","url":"http://example.com/x"}]}`,
		"cap cleartext": `{"capabilities":[{"id":"x","url":"http://10.0.0.1/x","token_file":"` + good + `"}]}`,
	}
	for name, cfg := range cases {
		if err := start(cfg); err == nil {
			t.Errorf("%s: a token that is not safe to send must stop the module", name)
		}
	}
	for name, cfg := range map[string]string{
		"loopback": `{"token_file":"` + good + `","capabilities":[{"id":"x","url":"http://127.0.0.1:1/"}]}`,
		"https":    `{"token_file":"` + good + `","capabilities":[{"id":"x","url":"https://example.com/x"}]}`,
		"no token": `{"capabilities":[{"id":"x","url":"http://example.com/x"}]}`,
	} {
		if err := start(cfg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A capability's timeout overrides the module's, and the daemon is told
// the bound the operator set, so the shorter of two limits is not the only
// one that ever applies.
func TestTimeoutIsPerCapability(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer slow.Close()
	reg := start(t, `{"timeout_ms":180000,"capabilities":[
		{"id":"fast.x","url":"`+slow.URL+`","timeout_ms":50},
		{"id":"long.x","url":"`+slow.URL+`"}]}`)

	began := time.Now()
	eff := invoke(t, reg, "fast.x", nil)
	if time.Since(began) > time.Second {
		t.Errorf("fast.x ran %v; its own 50 ms bound did not apply", time.Since(began))
	}
	if eff.Status != effect.Unavailable {
		t.Errorf("a call cut off by its bound: status %s", eff.Status)
	}

	p, _ := reg.Resolve("long.x")
	lr, ok := p.(provider.LongRunning)
	if !ok {
		t.Fatal("the service provider must report its bounds (provider.LongRunning)")
	}
	if d, set := lr.InvokeTimeout("long.x"); !set || d != 3*time.Minute {
		t.Errorf("long.x bound = %v %v, want the module's 3m", d, set)
	}
	if d, set := lr.InvokeTimeout("fast.x"); !set || d != 50*time.Millisecond {
		t.Errorf("fast.x bound = %v %v, want its own 50ms", d, set)
	}

	reg2 := start(t, `{"capabilities":[{"id":"plain.x","url":"`+slow.URL+`"}]}`)
	p2, _ := reg2.Resolve("plain.x")
	if _, set := p2.(provider.LongRunning).InvokeTimeout("plain.x"); set {
		t.Error("with no timeout configured the daemon's own bound must apply")
	}
}

// A redirect is not followed: it would resend the call, token included, to
// a URL the operator never configured.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	reg := start(t, `{"capabilities":[{"id":"x.do","url":"`+redir.URL+`"}]}`)
	eff := invoke(t, reg, "x.do", nil)
	if hits != 0 {
		t.Error("the redirect was followed")
	}
	if eff.Status != effect.Failed {
		t.Errorf("a redirect answer is not a success: %s", eff.Status)
	}
}

// The configuration describes each capability as an A2A skill.
func TestCapabilitiesAreDescribedAsSkills(t *testing.T) {
	reg := start(t, `{"capabilities":[
		{"id":"text.digest","url":"http://127.0.0.1:1/","name":"Text digest","description":"SHA-256 of text",
		 "tags":["hash","text"],"examples":["{\"text\":\"hi\"}"],"output_modes":["application/json"]},
		{"id":"bare.x","url":"http://127.0.0.1:1/"}]}`)
	p, _ := reg.Resolve("text.digest")
	d, ok := p.(provider.Described)
	if !ok {
		t.Fatal("the service provider must implement provider.Described")
	}
	si, ok := d.SkillInfo("text.digest")
	if !ok || si.Name != "Text digest" || si.Description != "SHA-256 of text" ||
		strings.Join(si.Tags, ",") != "hash,text" || len(si.Examples) != 1 ||
		strings.Join(si.OutputModes, ",") != "application/json" {
		t.Errorf("SkillInfo = %+v, %v", si, ok)
	}
	if _, ok := d.SkillInfo("bare.x"); ok {
		t.Error("a capability with no description must report false, so the id-derived one applies")
	}
	if full := provider.SkillInfoOf(p, "bare.x"); full.Name != "bare.x" || len(full.Tags) == 0 {
		t.Errorf("derived skill for bare.x = %+v", full)
	}
	if _, ok := d.SkillInfo("not.here"); ok {
		t.Error("an undeclared capability has no skill")
	}
}

func TestSkillLimitsAreCheckedAtStartup(t *testing.T) {
	long := strings.Repeat("n", provider.MaxSkillNameBytes+1)
	tags := `"a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"`
	for _, cfg := range []string{
		`{"capabilities":[{"id":"x","url":"http://127.0.0.1:1/","name":"` + long + `"}]}`,
		`{"capabilities":[{"id":"x","url":"http://127.0.0.1:1/","tags":[` + tags + `]}]}`,
		`{"capabilities":[{"id":"x","url":"http://127.0.0.1:1/","tags":["ok",""]}]}`,
		`{"capabilities":[{"id":"x","url":"http://127.0.0.1:1/","timeout_ms":-1}]}`,
		`{"capabilities":[{"id":"x","url":"ftp://127.0.0.1/"}]}`,
		`{"capabilities":[{"id":"x","url":"127.0.0.1:8080"}]}`,
	} {
		if _, err := module.Build(map[string][]byte{"service": []byte(cfg)}); err == nil {
			t.Errorf("config %.120s must be refused", cfg)
		}
	}
}

// A service that says "not now" (503, 429) is UNAVAILABLE, so the requester
// can retry; any other refusal is FAILED.
func TestBusyIsUnavailable(t *testing.T) {
	for code, want := range map[int]effect.Status{
		http.StatusServiceUnavailable:  effect.Unavailable,
		http.StatusTooManyRequests:     effect.Unavailable,
		http.StatusUnprocessableEntity: effect.Failed,
		http.StatusUnauthorized:        effect.Failed,
		http.StatusInternalServerError: effect.Failed,
	} {
		svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", code)
		}))
		reg := start(t, `{"capabilities":[{"id":"x.do","url":"`+svc.URL+`"}]}`)
		if eff := invoke(t, reg, "x.do", nil); eff.Status != want {
			t.Errorf("HTTP %d: %s, want %s", code, eff.Status, want)
		}
		svc.Close()
	}
}
