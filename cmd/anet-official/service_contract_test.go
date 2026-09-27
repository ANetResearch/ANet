//go:build !no_service

package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/moduletest"
	_ "github.com/ANetResearch/ANet/module/service"
	"github.com/ANetResearch/ANet/provider"
)

// contractHost is the least module.Host the service module needs.
type contractHost struct {
	moduletest.NopHost
	reg *provider.Registry
}

func (h *contractHost) AID() string                   { return "aid-official" }
func (h *contractHost) Providers() *provider.Registry { return h.reg }

// syncBuffer is a log sink the server goroutine writes while the test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// The whole path the daemon takes: the generated configuration mounts a
// real anet-official server through the real service module, which sends
// the token and the caller headers this server checks; the capabilities
// come out described as A2A skills; and a call's answer is the backend's.
func TestTheServiceModuleMountsThisBackend(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	caps, _ := selectGroups("tools,paid")
	var logs syncBuffer
	srv := &http.Server{Handler: newServer(testEnv(t), caps, testToken, log.New(&logs, "", 0))}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	sc := buildServiceConfig(caps, "http://"+ln.Addr().String(), tokenFile, 0)
	raw, _ := json.Marshal(sc.Modules.Service)
	mods, err := module.Build(map[string][]byte{"service": raw})
	if err != nil {
		t.Fatalf("the service module refuses the generated config: %v", err)
	}
	reg := provider.NewRegistry()
	for _, m := range mods {
		if err := m.Start(context.Background(), &contractHost{reg: reg}); err != nil {
			t.Fatal(err)
		}
	}

	p, ok := reg.Resolve("text.digest")
	if !ok {
		t.Fatal("text.digest did not register")
	}
	si := provider.SkillInfoOf(p, "text.digest")
	if si.Name != "Text digest" || !strings.Contains(si.Description, "git-blob-sha1") || si.Tags[0] != "hash" {
		t.Errorf("skill: %+v", si)
	}
	if lr, ok := p.(provider.LongRunning); !ok {
		t.Error("per-capability timeouts must reach the daemon")
	} else if d, set := lr.InvokeTimeout("text.diff"); !set || d.Seconds() != 5 {
		t.Errorf("text.diff bound %v %v", d, set)
	}
	if price, priced := p.(provider.Priced).Price("demo.digest.paid"); !priced || price != 2 {
		t.Errorf("demo.digest.paid price %d %v", price, priced)
	}

	eff, err := p.Invoke(context.Background(), provider.Call{Capability: "text.digest", Args: map[string]any{"text": "hello"},
		CallID: "ix-contract", CallerAID: "aid-caller", Via: provider.ViaRelay})
	if err != nil || eff.Status != effect.OK {
		t.Fatalf("invoke: %v %+v", err, eff)
	}
	if !strings.Contains(eff.Evidence.ObservedState, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824") {
		t.Errorf("answer: %s", eff.Evidence.ObservedState)
	}
	if l := logs.String(); !strings.Contains(l, "caller=aid-caller") || !strings.Contains(l, "call=ix-contract") || !strings.Contains(l, "via=relay") {
		t.Errorf("the backend did not receive the caller headers: %s", l)
	}

	// At the voucher door the payer is not presented as the caller.
	logs.Reset()
	if _, err := p.Invoke(context.Background(), provider.Call{Capability: "demo.digest.paid", Args: map[string]any{"text": "hello"},
		CallID: "voucher-1", CallerAID: "aid-payer", Via: provider.ViaVoucher}); err != nil {
		t.Fatal(err)
	}
	if l := logs.String(); strings.Contains(l, "aid-payer") || !strings.Contains(l, "caller=- call=voucher-1 via=voucher") {
		t.Errorf("voucher call log: %s", l)
	}

	// A daemon holding another token gets nowhere.
	other := filepath.Join(dir, "other")
	_ = os.WriteFile(other, []byte("another-token-0123456789\n"), 0o600)
	sc2 := buildServiceConfig(caps, "http://"+ln.Addr().String(), other, 0)
	raw2, _ := json.Marshal(sc2.Modules.Service)
	mods2, _ := module.Build(map[string][]byte{"service": raw2})
	reg2 := provider.NewRegistry()
	for _, m := range mods2 {
		_ = m.Start(context.Background(), &contractHost{reg: reg2})
	}
	p2, _ := reg2.Resolve("text.digest")
	eff2, _ := p2.Invoke(context.Background(), provider.Call{Capability: "text.digest", Args: map[string]any{"text": "x"}})
	if eff2.Status != effect.Failed || !strings.Contains(eff2.Message, "401") {
		t.Errorf("wrong token: %s %s", eff2.Status, eff2.Message)
	}
}
