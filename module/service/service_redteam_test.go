//go:build !no_service

package service_test

// Red-team PoCs for SI-6 ("不知道" 与 "知道" 不合并), lens si6.
//
// Each test passes when the defect is present: it asserts that the attack
// succeeded. A fix makes these tests fail; they are then flipped or removed.
//
// (The two PoCs of F10 — a timeout or a lost connection after the call
// went out reported UNAVAILABLE — are fixed; their regression tests are in
// outcome_test.go.)

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
)

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
	reg := start(t, `{"allow_tcp":true,"capabilities":[{"id":"door.unlock","url":"`+svc.URL+`"}]}`)
	eff := invoke(t, reg, "door.unlock", nil)
	if eff.Status != effect.OK || !eff.Verifiable() || eff.Evidence.VerifyTrust != 1 || len(eff.Record.Metrics) != 0 {
		t.Fatalf("defect not reproduced: status=%s verifiable=%v trust=%d metrics=%v",
			eff.Status, eff.Verifiable(), eff.Evidence.VerifyTrust, eff.Record.Metrics)
	}
	t.Logf("ATTACK OK: status=%s verifiable=%v with verify_trust=%d and no metric backing the claim",
		eff.Status, eff.Verifiable(), eff.Evidence.VerifyTrust)
}
