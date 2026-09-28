//go:build !no_service

package service_test

// Red-team PoCs for SI-6 ("不知道" 与 "知道" 不合并), lens si6.
//
// Each test passes when the defect is present: it asserts that the attack
// succeeded. A fix makes these tests fail; they are then flipped or removed.
//
// The defect: the service module reports UNAVAILABLE ("the target could not
// be reached; nothing was attempted at the far end", ANetCore effect.go and
// service.go:427-433) for every transport error of http.Client.Do,
// including the ones that happen AFTER the backend received the call and
// performed its effect — the call's own deadline expiring while the backend
// works, or the connection dropping before the response arrives. Whether
// the effect happened is unknown in both cases, the situation A2A-DESIGN
// §4.3 answers with effect UNVERIFIED (as for an interrupted call), but the
// requester is told the definite "nothing was attempted" and the daemon
// maps it to TASK_STATE_REJECTED (capability.go stateForEffect), which
// invites a retry and a second execution of the effect.

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
)

// The backend performs the effect, then answers after the call's bound.
func TestRedteamSI6_TimeoutAfterEffectIsReportedUnavailable(t *testing.T) {
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

	reg := start(t, `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":100}]}`)
	eff := invoke(t, reg, "mail.send", map[string]any{"to": "x@example.org"})

	if effects.Load() != 1 {
		t.Fatalf("precondition: the backend performed the effect %d times, want 1", effects.Load())
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("defect not reproduced: status %s (want the defect's UNAVAILABLE)", eff.Status)
	}
	t.Logf("ATTACK OK: effect executed %d time(s) at the backend, reported %s (%q) — "+
		"\"nothing attempted\" instead of UNVERIFIED", effects.Load(), eff.Status, eff.Message)
}

// The backend performs the effect, then the connection drops before any
// response: not a timing race, just a lost answer.
func TestRedteamSI6_ConnectionLostAfterEffectIsReportedUnavailable(t *testing.T) {
	var effects atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	reg := start(t, `{"capabilities":[{"id":"mail.send","url":"`+svc.URL+`","timeout_ms":2000}]}`)
	eff := invoke(t, reg, "mail.send", nil)

	if effects.Load() != 1 {
		t.Fatalf("precondition: the backend performed the effect %d times, want 1", effects.Load())
	}
	if eff.Status != effect.Unavailable {
		t.Fatalf("defect not reproduced: status %s (want the defect's UNAVAILABLE)", eff.Status)
	}
	t.Logf("ATTACK OK: effect executed, reported %s (%q)", eff.Status, eff.Message)
}

// A 2xx answer with nothing in it is reported OK and Verifiable(): the
// deliverable then says "verifiable": true (daemon capability.go:436) for an
// effect the module itself records as unverified (VerifyTrust 1: "whether
// the answer is right is not something this daemon can establish",
// service.go:475-477). OK is "executed and the effect is verifiable —
// Record carries the metrics that back the claim" (ANetCore effect.go) and
// "做了且读回一致" (docs/GUIDE-zh.md:251); what the module knows is
// UNVERIFIED ("做了但没法读回").
func TestRedteamSI6_BareHTTP200IsReportedVerifiableOK(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`)) // "accepted" — nothing read back, no metric
	}))
	defer svc.Close()
	reg := start(t, `{"capabilities":[{"id":"door.unlock","url":"`+svc.URL+`"}]}`)
	eff := invoke(t, reg, "door.unlock", nil)
	if eff.Status != effect.OK || !eff.Verifiable() || eff.Evidence.VerifyTrust != 1 || len(eff.Record.Metrics) != 0 {
		t.Fatalf("defect not reproduced: status=%s verifiable=%v trust=%d metrics=%v",
			eff.Status, eff.Verifiable(), eff.Evidence.VerifyTrust, eff.Record.Metrics)
	}
	t.Logf("ATTACK OK: status=%s verifiable=%v with verify_trust=%d and no metric backing the claim",
		eff.Status, eff.Verifiable(), eff.Evidence.VerifyTrust)
}
