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

// The service takes the call — 200 and the start of its reply — and the
// answer breaks off: the deadline passes while the body is still coming,
// or the connection drops mid-reply. Its effect has happened as surely as
// one whose reply never started, so it is the same unknown outcome; it was
// FAILED ("reply is not a JSON object"), which says the effect did not
// happen (a bypass of F10 found on review).
func TestAnAnswerThatBreaksOffAfterItsHeadersIsNotFailed(t *testing.T) {
	for _, c := range []struct {
		name   string
		cut    func(w http.ResponseWriter, r *http.Request)
		reason string
	}{
		{"deadline", func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		}, provider.ReasonTimeout},
		{"connection", func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}, provider.ReasonConnectionLost},
	} {
		t.Run(c.name, func(t *testing.T) {
			var effects atomic.Int32
			svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				effects.Add(1) // the effect happens
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "64") // more than will come
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"sen`))
				w.(http.Flusher).Flush()
				c.cut(w, r)
			}))
			defer svc.Close()

			_, unknown := invokeUnknown(t, "mail.send",
				`{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":300}]}`)
			if effects.Load() != 1 {
				t.Fatalf("the service ran %d times, want 1", effects.Load())
			}
			if unknown.Reason != c.reason {
				t.Fatalf("reason %q (%v), want %s", unknown.Reason, unknown.Err, c.reason)
			}
		})
	}
}

// A refusal is its status whatever happens to its body: a 503 whose body
// breaks off is still UNAVAILABLE (the service said "not now").
func TestARefusalWhoseBodyBreaksOffIsItsStatus(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("busy"))
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer svc.Close()
	reg := start(t, `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":2000}]}`)
	if eff := invoke(t, reg, "mail.send", nil); eff.Status != effect.Unavailable {
		t.Fatalf("status %s (%s), want UNAVAILABLE", eff.Status, eff.Message)
	}
}

// A reverse proxy in front of the service answers for it when the service
// is slow (504) or its answer breaks (502): the call reached the service,
// which may have acted. Not FAILED — a proxy_read_timeout shorter than the
// work would make every slow call "did not happen" (F10, on review).
func TestAGatewayAnsweringForTheServiceIsAnUnknownOutcome(t *testing.T) {
	for _, c := range []struct {
		status int
		reason string
	}{{http.StatusGatewayTimeout, provider.ReasonTimeout}, {http.StatusBadGateway, provider.ReasonConnectionLost}} {
		var effects atomic.Int32
		svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			effects.Add(1) // the service behind the gateway acted
			http.Error(w, "upstream timed out", c.status)
		}))
		_, unknown := invokeUnknown(t, "mail.send", `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`"}]}`)
		svc.Close()
		if unknown.Reason != c.reason || effects.Load() != 1 {
			t.Fatalf("HTTP %d: reason %q (effects %d), want %s", c.status, unknown.Reason, effects.Load(), c.reason)
		}
	}
}
