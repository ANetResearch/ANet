//go:build !no_service

package daemon

// Red-team finding F10, end to end: a service-module call whose effect
// happened at the backend but whose answer did not come back in time
// reached the requester as TASK_STATE_REJECTED + anet.effect_status =
// UNAVAILABLE ("nothing was attempted"), which invites a retry that runs
// the effect a second time. It is failed + UNVERIFIED + anet.reason =
// timeout, as for a call a restart interrupted (A2A-DESIGN §4.3). Kept
// apart because it runs the real service module, which -tags no_service
// removes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	_ "github.com/ANetResearch/ANet/module/service"
)

func TestServiceTimeoutReachesRequesterAsFailedUnverified(t *testing.T) {
	var effects atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1) // the effect happens
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer backend.Close()

	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemonCfg(t, srv.URL, map[string]any{
		"service": map[string]any{"capabilities": []any{
			map[string]any{"id": "mail.send", "url": backend.URL, "timeout_ms": 150},
		}},
	})
	allowPeers(t, prov, req.AID())
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "mail.send", map[string]any{"to": "x@example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider's answer", func() bool {
		_ = req.pollOnce(ctx)
		return mustIX(t, req, id).IsTerminal()
	})
	if effects.Load() != 1 {
		t.Fatalf("the backend ran %d times, want 1", effects.Load())
	}

	task, err := req.taskView(mustIX(t, req, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	es, reason := task.Metadata[a2ashape.KeyEffectStatus], task.Metadata[a2ashape.KeyReason]
	if task.Status.State != a2ashape.TaskStateFailed || es != "UNVERIFIED" || reason != "timeout" {
		t.Fatalf("the requester's task says state=%v effect_status=%v reason=%v; want failed, UNVERIFIED, timeout "+
			"(the effect may have happened: never rejected/UNAVAILABLE)", task.Status.State, es, reason)
	}
}
