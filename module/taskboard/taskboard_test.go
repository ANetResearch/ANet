//go:build !no_taskboard

package taskboard_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/taskboard"
	"github.com/ANetResearch/ANet/provider"
)

// fakeSeam signs as a real controller, so the hub-side verification a
// test performs is the same arithmetic the hub performs.
type fakeSeam struct {
	c   *identity.Controller
	url string
}

func (s fakeSeam) Sign(pre []byte) ([]byte, uint64) { return s.c.Sign(pre) }
func (s fakeSeam) HubURL() string                   { return s.url }

// fakeHost is the smallest Host that lets a module start.
type fakeHost struct {
	reg  *provider.Registry
	seam module.HubSeam
	aid  string
}

func (h *fakeHost) AID() string                      { return h.aid }
func (h *fakeHost) Providers() *provider.Registry    { return h.reg }
func (h *fakeHost) RecordEvidence(string, any) error { return nil }
func (h *fakeHost) ResolveKEL(string) ([]identity.SignedEvent, bool) {
	return nil, false
}
func (h *fakeHost) PaymentSeam() (module.PaymentSeam, bool) { return nil, false }
func (h *fakeHost) HubSeam() (module.HubSeam, bool) {
	if h.seam == nil {
		return nil, false
	}
	return h.seam, true
}

// start brings the module up against a board and returns the provider.
func start(t *testing.T, boardURL string, ctrl *identity.Controller) provider.CapabilityProvider {
	t.Helper()
	m, err := module.BuildOne("taskboard", json.RawMessage(`{}`))
	if err != nil || m == nil {
		t.Fatalf("module: %v", err)
	}
	h := &fakeHost{reg: provider.NewRegistry(), aid: ctrl.AID(),
		seam: fakeSeam{c: ctrl, url: boardURL}}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatalf("start: %v", err)
	}
	p, ok := h.reg.Resolve(taskboard.CapCreate)
	if !ok {
		t.Fatal("the module registered no provider for task.create")
	}
	return p
}

func ctrlFor(t *testing.T) *identity.Controller {
	t.Helper()
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Every mutation must carry a signature the hub can check.
//
// The board gates each action behind a challenge signed with the caller's
// key, and nothing in this suite could produce one — the board could be
// read and not used. This is the check that the client actually signs,
// and signs the action it is performing: signatures are namespaced task.*
// precisely so one made to create a card cannot be replayed to claim one.
func TestEveryMutationIsSignedForItsOwnAction(t *testing.T) {
	ctrl := ctrlFor(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := "task." + strings.TrimPrefix(r.URL.Path, "/tasks/")
		var body struct {
			AID  string `json:"aid"`
			TS   uint64 `json:"ts"`
			Seq  uint64 `json:"key_state_seq"`
			Sig  string `json:"sig"`
			Card string `json:"card_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sig, err := base64.StdEncoding.DecodeString(body.Sig)
		if err != nil {
			t.Errorf("%s: signature is not base64", action)
		}
		// The hub's own check, performed here: the signature must verify
		// over THIS action's preimage under the caller's key history.
		if err := identity.VerifyObject(ctrl.KEL(), body.AID, body.Seq, body.TS,
			relayauth.Preimage(action, body.AID, body.TS), sig); err != nil {
			t.Errorf("%s: the hub would reject this signature: %v", action, err)
		}
		seen = append(seen, action)
		_, _ = w.Write([]byte(`{"card":{"id":"card-1","state":"created"}}`))
	}))
	defer srv.Close()

	p := start(t, srv.URL, ctrl)
	if _, err := p.Invoke(context.Background(), provider.Call{
		Capability: taskboard.CapCreate,
		Args:       map[string]any{"title": "t", "taskdoc_cid": "bafy-doc"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := p.Invoke(context.Background(), provider.Call{
		Capability: taskboard.CapClaim, Args: map[string]any{"card_id": "card-1"},
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	want := []string{"task.create", "task.claim"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("actions signed = %v, want %v", seen, want)
	}
}

// The hub's refusal has to reach the caller intact.
//
// Its messages say which rule was broken — only ready cards are
// claimable, only the assignee submits — and that is the useful part.
// Replacing them with a status code would throw away the only thing a
// caller can act on.
func TestTheBoardsRefusalIsPassedThrough(t *testing.T) {
	ctrl := ctrlFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"taskboard: transition not allowed: only ready cards are claimable"}`))
	}))
	defer srv.Close()

	p := start(t, srv.URL, ctrl)
	_, err := p.Invoke(context.Background(), provider.Call{
		Capability: taskboard.CapClaim, Args: map[string]any{"card_id": "card-1"},
	})
	if err == nil {
		t.Fatal("a refused claim reported success")
	}
	if !strings.Contains(err.Error(), "only ready cards are claimable") {
		t.Errorf("the hub's reason was lost: %v", err)
	}
}

// A card must name the document that describes the work.
//
// The hub requires it and the requirement is right: a board of titles is
// a board of intentions. Refusing here rather than at the hub means the
// caller learns what is missing instead of reading a validation error
// about a field it never heard of.
func TestACardMustNameItsDocument(t *testing.T) {
	ctrl := ctrlFor(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an incomplete card was sent to the board")
	}))
	defer srv.Close()

	p := start(t, srv.URL, ctrl)
	_, err := p.Invoke(context.Background(), provider.Call{
		Capability: taskboard.CapCreate, Args: map[string]any{"title": "just a title"},
	})
	if err == nil || !strings.Contains(err.Error(), "taskdoc_cid") {
		t.Errorf("a card with no document was accepted: %v", err)
	}
}

// Reading the board reports what it holds, and carries the board's own
// answer as evidence rather than a count this node computed and asks to
// be believed.
func TestReadingTheBoardCarriesWhatItSaid(t *testing.T) {
	ctrl := ctrlFor(t)
	const board = `{"columns":[{"key":"backlog","name":"Backlog","cards":[{"id":"a"},{"id":"b"}]},
	                            {"key":"done","name":"Done","cards":[{"id":"c"}]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading is unsigned: a signature would only say who was looking.
		if r.Header.Get("Content-Type") == "application/json" {
			t.Error("the board was read with a signed body")
		}
		_, _ = w.Write([]byte(board))
	}))
	defer srv.Close()

	p := start(t, srv.URL, ctrl)
	eff, err := p.Invoke(context.Background(), provider.Call{Capability: taskboard.CapBoard})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("status = %v", eff.Status)
	}
	if eff.Record == nil || eff.Record.Metrics["cards"] != 3 ||
		eff.Record.Metrics["backlog"] != 2 {
		t.Errorf("metrics = %+v, want 3 cards with 2 in backlog", eff.Record)
	}
	if eff.Evidence == nil || !strings.Contains(eff.Evidence.ObservedState, `"backlog"`) {
		t.Error("the board's own answer is not carried as evidence")
	}
}

// A node with no hub has no board, and must say so at start rather than
// register capabilities that answer an error to every call.
func TestWithoutAHubTheModuleRefusesToStart(t *testing.T) {
	m, err := module.BuildOne("taskboard", json.RawMessage(`{}`))
	if err != nil || m == nil {
		t.Fatalf("module: %v", err)
	}
	h := &fakeHost{reg: provider.NewRegistry(), aid: "did:anet:x"}
	if err := m.Start(context.Background(), h); err == nil {
		t.Error("the module started with no hub to talk to")
	}
}
