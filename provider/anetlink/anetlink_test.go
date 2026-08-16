package anetlink

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/provider"
)

// fakeLinkd serves the documented C1 wire — the shim is tested against the
// wire contract, never against ANetLink code (apps must not import apps).
func fakeLinkd(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "l.sock")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /v0/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"light.onoff@sim/lamp-1"}})
	})
	mux.HandleFunc("GET /v0/describe", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"cid": "bafyexample"})
	})
	mux.HandleFunc("POST /v0/invoke", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Capability string         `json:"capability"`
			Args       map[string]any `json:"args"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Capability != "light.onoff@sim/lamp-1" {
			http.Error(w, "unknown capability", http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "OK", "metrics": map[string]float64{"power_state": 1},
		})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	deadline := time.Now().Add(2 * time.Second)
	p := New("probe", sock)
	for p.Health(context.Background()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("fake linkd did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return sock
}

func TestShimImplementsC1(t *testing.T) {
	sock := fakeLinkd(t)
	p := New("anetlink", sock)
	ctx := context.Background()

	caps, err := p.Capabilities(ctx)
	if err != nil || len(caps) != 1 || caps[0] != "light.onoff@sim/lamp-1" {
		t.Fatalf("capabilities: %v %v", caps, err)
	}
	cid, err := p.Describe(ctx)
	if err != nil || cid != "bafyexample" {
		t.Fatalf("describe: %v %v", cid, err)
	}
	eff, err := p.Invoke(ctx, provider.Call{Capability: caps[0], Args: map[string]any{"on": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Verifiable() || eff.Record.Metrics["power_state"] != 1 {
		t.Fatalf("effect lost on the wire: %+v", eff)
	}

	if _, err := p.Invoke(ctx, provider.Call{Capability: "ghost"}); err == nil {
		t.Fatal("wire error must surface as error")
	}
}

func TestRegistryIntegration(t *testing.T) {
	sock := fakeLinkd(t)
	r := provider.NewRegistry()
	if err := r.Register(context.Background(), New("anetlink", sock)); err != nil {
		t.Fatal(err)
	}
	p, ok := r.Resolve("light.onoff@sim/lamp-1")
	if !ok || p.ID() != "anetlink" {
		t.Fatal("registry cannot resolve wire capability id")
	}
	eff, err := p.Invoke(context.Background(), provider.Call{Capability: "light.onoff@sim/lamp-1", Args: map[string]any{"on": true}})
	if err != nil || eff.Status != effect.OK {
		t.Fatalf("end-to-end through registry failed: %+v %v", eff, err)
	}
}
