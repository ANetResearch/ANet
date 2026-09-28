//go:build !no_service

package service_test

// What a transport error says about the effect (A2A-DESIGN §4.3; red-team
// finding F10). UNAVAILABLE says nothing was attempted at the far end, and
// is only true when the request never went out. A call that went out and
// lost its answer — the deadline passed while the service worked, or the
// connection dropped before the reply — may have had its effect: it is a
// provider.OutcomeUnknownError, which the daemon reports as failed with
// effect UNVERIFIED, never as UNAVAILABLE (which invites a retry that runs
// the effect again).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/provider"
)

// invokeUnknown invokes capID and requires the outcome-unknown error.
func invokeUnknown(t *testing.T, capID, cfg string) (effect.Effect, *provider.OutcomeUnknownError) {
	t.Helper()
	reg := start(t, cfg)
	p, ok := reg.Resolve(capID)
	if !ok {
		t.Fatalf("capability %q did not register", capID)
	}
	eff, err := p.Invoke(context.Background(), provider.Call{Capability: capID, Args: map[string]any{"to": "x@example.org"}})
	var unknown *provider.OutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("status %s, err %v: want a provider.OutcomeUnknownError (never UNAVAILABLE, \"nothing attempted\")",
			eff.Status, err)
	}
	if eff.Status == effect.Unavailable {
		t.Fatalf("an effect that may have happened is reported UNAVAILABLE")
	}
	return eff, unknown
}

// The service performs the effect, then answers after the call's bound.
func TestTimeoutAfterTheCallWentOutIsNotUnavailable(t *testing.T) {
	var effects atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effects.Add(1) // the side effect happens here (mail sent, order placed, ...)
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"sent":true}`))
	}))
	defer svc.Close()

	_, unknown := invokeUnknown(t, "mail.send", `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":100}]}`)
	if effects.Load() != 1 {
		t.Fatalf("the service ran %d times, want 1", effects.Load())
	}
	if unknown.Reason != provider.ReasonTimeout || !errors.Is(unknown, context.DeadlineExceeded) {
		t.Fatalf("reason %q, err %v: want timeout", unknown.Reason, unknown.Err)
	}
}

// The service performs the effect and the connection drops before any
// response: not a timing race, just a lost answer.
func TestConnectionLostAfterTheCallWentOutIsNotUnavailable(t *testing.T) {
	var effects atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close() // effect done, answer lost
	}))
	defer svc.Close()

	_, unknown := invokeUnknown(t, "mail.send", `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":2000}]}`)
	if effects.Load() != 1 {
		t.Fatalf("the service ran %d times, want 1", effects.Load())
	}
	if unknown.Reason != provider.ReasonConnectionLost {
		t.Fatalf("reason %q, want connection_lost", unknown.Reason)
	}
}

// A service that cannot be reached at all had nothing attempted: that is
// still UNAVAILABLE, the answer that tells a requester trying elsewhere is
// safe.
func TestUnreachableServiceIsStillUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there now: the dial is refused

	reg := start(t, `{"capabilities":[{"id":"mail.send","url":"http://`+addr+`/send","timeout_ms":2000}]}`)
	eff := invoke(t, reg, "mail.send", nil)
	if eff.Status != effect.Unavailable {
		t.Fatalf("a refused connection: status %s (%s), want UNAVAILABLE", eff.Status, eff.Message)
	}
}
