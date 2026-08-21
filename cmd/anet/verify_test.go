package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// A receipt and the key that signed it are useless apart, so the flags go
// together. Verifying with only one would either check nothing, or check a
// signature against a key of the caller's own choosing.
func TestVerifyRequiresBothHalves(t *testing.T) {
	for _, argv := range [][]string{{"--receipt", "abc"}, {"--kel", "abc"}} {
		err := verify(daemon.Layout{}, argv)
		if err == nil || !strings.Contains(err.Error(), "go together") {
			t.Errorf("%v: expected a refusal explaining why, got %v", argv, err)
		}
	}
	if err := verify(daemon.Layout{}, nil); err == nil ||
		!strings.Contains(err.Error(), "needs no daemon") {
		t.Errorf("bare usage must say the offline form exists, got %v", err)
	}
}

// The offline path is the claim itself: a stranger with the files and no
// network reaches a verdict.
func TestOfflineVerification(t *testing.T) {
	provider, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := []byte(`{"answer":42}`)
	result := filepath.Join(dir, "result.bin")
	if err := os.WriteFile(result, body, 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.bin")
	if err := os.WriteFile(other, []byte("something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := receiptFor(t, provider, body)

	if err := verifyOffline(b64(t, rc), kelOf(t, provider), result); err != nil {
		t.Fatalf("a genuine receipt must verify offline: %v", err)
	}
	if err := verifyOffline(b64(t, rc), kelOf(t, provider), other); err == nil {
		t.Error("a receipt that does not cover the bytes must not pass")
	}
	if err := verifyOffline(b64(t, rc), kelOf(t, stranger), result); err == nil {
		t.Error("a receipt checked against the wrong key must not pass")
	}
	// Without the bytes, the signature still checks and the content does
	// not. That is a real answer; the note the command prints is the
	// honest part of it.
	if err := verifyOffline(b64(t, rc), kelOf(t, provider), ""); err != nil {
		t.Errorf("a signature-only check is a real answer: %v", err)
	}
}

func receiptFor(t *testing.T, c *identity.Controller, body []byte) *evidence.Receipt {
	t.Helper()
	cid, err := anetcidSum(body)
	if err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{
		InteractionID: "ix-1", RequesterAID: "did:anet:requester", ProviderAID: c.AID(),
		RequestCID: "bafyrequest", ResultCID: cid, CompletedAt: 1767225600000,
	}
	if err := rc.Sign(c); err != nil {
		t.Fatal(err)
	}
	return rc
}

func b64(t *testing.T, rc *evidence.Receipt) string {
	t.Helper()
	b, err := rc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return base64Std(b)
}

func kelOf(t *testing.T, c *identity.Controller) string {
	t.Helper()
	b, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	return base64Std(b)
}
