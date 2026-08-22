//go:build !no_service

package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	_ "github.com/ANetResearch/ANet/module/service"
	"github.com/ANetResearch/ANet/provider"
)

type host struct{ reg *provider.Registry }

func (h *host) AID() string                                      { return "aid-self" }
func (h *host) Providers() *provider.Registry                    { return h.reg }
func (h *host) RecordEvidence(string, any) error                 { return nil }
func (h *host) ResolveKEL(string) ([]identity.SignedEvent, bool) { return nil, false }

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
