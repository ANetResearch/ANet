//go:build !no_cas

package cas

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

type casHost struct{ reg *provider.Registry }

func (h *casHost) AID() string                      { return "node-1" }
func (h *casHost) Providers() *provider.Registry    { return h.reg }
func (h *casHost) RecordEvidence(string, any) error { return nil }
func (h *casHost) ResolveKEL(string) ([]identity.SignedEvent, bool) {
	return nil, false
}

var _ module.Host = (*casHost)(nil)

func started(t *testing.T, cfg Config) (*Module, *casHost) {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	h := &casHost{reg: provider.NewRegistry()}
	m := &Module{cfg: cfg}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m, h
}

func TestCASIsOfferedThroughC1(t *testing.T) {
	_, h := started(t, Config{})
	for _, capID := range []string{CapPut, CapGet, CapHas, CapStat} {
		if _, ok := h.reg.Resolve(capID); !ok {
			t.Errorf("capability %q must be resolvable", capID)
		}
	}
}

// The CID is derived from the bytes, so a peer cannot store content under a
// name that misdescribes it. That is what content addressing is for, and it
// is why the effect can claim V4: the answer was measured from the data.
func TestPutReturnsADerivedCIDAndGetReturnsTheBytes(t *testing.T) {
	_, h := started(t, Config{})
	body := []byte("the exact bytes")

	put, _ := h.reg.Resolve(CapPut)
	eff, err := put.Invoke(context.Background(), provider.Call{
		Capability: CapPut,
		Args:       map[string]any{"body": base64.StdEncoding.EncodeToString(body)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("put failed: %s", eff.Message)
	}
	cidStr := eff.Evidence.ObservedState
	if cidStr == "" {
		t.Fatal("put must report the CID it derived")
	}
	if eff.Evidence.VerifyTrust != 4 {
		t.Errorf("a CID measured from the content is V4, got V%d", eff.Evidence.VerifyTrust)
	}

	get, _ := h.reg.Resolve(CapGet)
	got, err := get.Invoke(context.Background(), provider.Call{
		Capability: CapGet, Args: map[string]any{"cid": cidStr}})
	if err != nil {
		t.Fatal(err)
	}
	back, err := base64.StdEncoding.DecodeString(got.Evidence.ObservedState)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(body) {
		t.Fatalf("got %q, want the exact bytes stored", back)
	}
}

// Storing the same bytes twice yields the same CID and does not duplicate:
// idempotence is a property of content addressing, not an optimisation.
func TestPutIsIdempotentThroughTheCapability(t *testing.T) {
	_, h := started(t, Config{})
	put, _ := h.reg.Resolve(CapPut)
	args := map[string]any{"body": base64.StdEncoding.EncodeToString([]byte("same"))}

	first, err := put.Invoke(context.Background(), provider.Call{Capability: CapPut, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	second, err := put.Invoke(context.Background(), provider.Call{Capability: CapPut, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if first.Evidence.ObservedState != second.Evidence.ObservedState {
		t.Fatal("the same bytes must produce the same CID")
	}
}

// A capability that accepts bytes from the network needs a limit by
// default, not when an operator remembers: without one, a peer fills this
// node's disk.
func TestOversizedBlobIsRefused(t *testing.T) {
	_, h := started(t, Config{MaxBlobBytes: 16})
	put, _ := h.reg.Resolve(CapPut)

	eff, err := put.Invoke(context.Background(), provider.Call{
		Capability: CapPut,
		Args:       map[string]any{"body": base64.StdEncoding.EncodeToString(make([]byte, 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatal("a blob over the limit must be refused")
	}
	if !strings.Contains(eff.Message, "limit") {
		t.Errorf("the refusal should name the limit: %q", eff.Message)
	}
}

// Asking for something absent is an answer, not an error.
func TestGetMissingIsAFailedEffectNotAnError(t *testing.T) {
	_, h := started(t, Config{})
	get, _ := h.reg.Resolve(CapGet)

	// A well-formed CID for content this store has never held.
	eff, err := get.Invoke(context.Background(), provider.Call{
		Capability: CapGet,
		Args: map[string]any{
			"cid": "bafyreidptlyzoo4mbqeo3jwan6k2sncdlesywwws76jbbeuy2x6syyrq4q"},
	})
	if err != nil {
		t.Fatalf("a missing blob is a result, not a transport error: %v", err)
	}
	if eff.Status != effect.Failed {
		t.Fatal("a missing blob must report FAILED")
	}
}

// has and stat answer without moving the bytes.
func TestHasAndStat(t *testing.T) {
	_, h := started(t, Config{})
	put, _ := h.reg.Resolve(CapPut)
	body := []byte("measured")
	eff, err := put.Invoke(context.Background(), provider.Call{
		Capability: CapPut,
		Args:       map[string]any{"body": base64.StdEncoding.EncodeToString(body)},
	})
	if err != nil {
		t.Fatal(err)
	}
	cidStr := eff.Evidence.ObservedState

	has, _ := h.reg.Resolve(CapHas)
	hEff, err := has.Invoke(context.Background(), provider.Call{
		Capability: CapHas, Args: map[string]any{"cid": cidStr}})
	if err != nil {
		t.Fatal(err)
	}
	if hEff.Record.Metrics["present"] != 1 {
		t.Error("has must report a stored blob as present")
	}

	st, _ := h.reg.Resolve(CapStat)
	sEff, err := st.Invoke(context.Background(), provider.Call{
		Capability: CapStat, Args: map[string]any{"cid": cidStr}})
	if err != nil {
		t.Fatal(err)
	}
	if sEff.Record.Metrics["bytes"] != float64(len(body)) {
		t.Fatalf("stat = %v bytes, want %d", sEff.Record.Metrics["bytes"], len(body))
	}
}

// A store must be told where to live: choosing for the operator puts a
// node's content somewhere they did not pick and cannot find.
func TestDirIsRequired(t *testing.T) {
	_, err := module.Build(map[string][]byte{"cas": []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "dir") {
		t.Fatalf("a CAS without a directory must be refused, got %v", err)
	}
}

// benchStarted is the benchmark's counterpart to started(t).
func benchStarted(b *testing.B) (*Module, *casHost) {
	b.Helper()
	h := &casHost{reg: provider.NewRegistry()}
	m := &Module{cfg: Config{Dir: b.TempDir()}}
	if err := m.Start(context.Background(), h); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { m.Stop(context.Background()) })
	return m, h
}

func benchCall(capID string, args map[string]any) provider.Call {
	return provider.Call{Capability: capID, Args: args}
}

// PaymentSeam: none of these modules take money, and a host that offered
// one would be lending them an ability they must not have. False is the
// honest answer and the one a node without a hub gives too.
func (h *casHost) PaymentSeam() (module.PaymentSeam, bool) { return nil, false }
