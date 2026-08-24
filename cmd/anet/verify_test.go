package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// A receipt and the key that signed it are useless apart. A receipt alone
// is checkable only if the key can be fetched, so the refusal has to say
// which of the two ways forward the caller has — not merely that
// something is missing.
func TestVerifyRequiresBothHalves(t *testing.T) {
	err := verify(daemon.Layout{}, []string{"--receipt", "abc"})
	if err == nil || !strings.Contains(err.Error(), "--hub") {
		t.Errorf("a lone receipt must be told it can fetch the key, got %v", err)
	}
	if err := verify(daemon.Layout{}, []string{"--kel", "abc"}); err == nil ||
		!strings.Contains(err.Error(), "needs a --receipt") {
		t.Errorf("a lone key has nothing to check, got %v", err)
	}
	if err := verify(daemon.Layout{}, nil); err == nil ||
		!strings.Contains(err.Error(), "nothing at all") {
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

// A witness attestation must be checkable with the same command a
// receipt is, and by a stranger.
//
// A hub's /x402/witnesses publishes signed attestations and tells the
// reader to resolve each witness and verify the signature. This command
// knew only receipts, so the instruction named a step with no tool behind
// it — and witnessing is the entire basis for believing a hub has not
// rewritten its own issuance chain.
func TestVerifyingAWitnessAttestation(t *testing.T) {
	witness, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	att := &ael.HeadAttestation{
		ChainDID: "bafyreihubchaindidplaceholderxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		Seq:      7, HeadID: "bafyreiheadidplaceholderxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		ObservedAt: time.Now().UnixMilli(),
	}
	if err := att.Sign(witness); err != nil {
		t.Fatal(err)
	}
	raw, err := att.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	attB64 := base64.StdEncoding.EncodeToString(raw)
	kelRaw, err := identity.MarshalKEL(witness.KEL())
	if err != nil {
		t.Fatal(err)
	}
	kelB64 := base64.StdEncoding.EncodeToString(kelRaw)

	if err := verify(daemon.Layout{}, []string{
		"--attestation", attB64, "--kel", kelB64}); err != nil {
		t.Errorf("a genuine attestation was refused: %v", err)
	}

	// The signature is what is checked, so another party's key must not
	// pass. Accepting any key would make the command say "verified" about
	// arithmetic it never did.
	other, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	otherKEL, err := identity.MarshalKEL(other.KEL())
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(daemon.Layout{}, []string{"--attestation", attB64,
		"--kel", base64.StdEncoding.EncodeToString(otherKEL)}); err == nil {
		t.Error("an attestation verified under the wrong key history")
	}

	// And with neither a key nor a hub it must say which of the two ways
	// forward the caller has, naming the witness so they can go find it.
	err = verify(daemon.Layout{}, []string{"--attestation", attB64})
	if err == nil || !strings.Contains(err.Error(), "--hub") {
		t.Errorf("a lone attestation must be told it can fetch the key, got %v", err)
	}
}

// Fetching a key history from a hub must be said out loud.
//
// "I checked this with nothing" and "I asked a hub for the key" are
// different claims, and a verifier that cannot tell them apart will
// record the stronger. The line existed on the receipt path and had no
// test; splitting the fetch out for the attestation path dropped it, and
// the whole suite stayed green. Production caught it.
func TestVerifySaysWhereTheKeyCameFrom(t *testing.T) {
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	kelRaw, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	var asked string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{
			"kel": base64.StdEncoding.EncodeToString(kelRaw)})
	}))
	defer hub.Close()

	out := captureStdout(t, func() {
		if _, err := fetchKELFor(hub.URL, c.AID()); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(asked, c.AID()) {
		t.Errorf("the hub was asked for %q, not the AID", asked)
	}
	if !strings.Contains(out, "fetched from") || !strings.Contains(out, hub.URL) {
		t.Errorf("the fetch was silent, so a reader cannot tell it from an offline check: %q", out)
	}
	if !strings.Contains(out, c.AID()) {
		t.Errorf("the line does not say whose key was fetched: %q", out)
	}
}

// captureStdout collects what a function prints.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
