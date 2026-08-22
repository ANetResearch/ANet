package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
)

// The fixture had no tests, which was tolerable while it only helped a
// human paste a base64 string. It stopped being tolerable when the joint
// run started depending on it: the gateway-payment section of
// scenario.sh pays with `anetfixture x402-authorize`, so a fixture that
// silently minted an unverifiable authorization would make that section
// fail with the hub blamed for it — a test tool that lies is worse than
// no test tool, because its failures look like the system's.

func withIdentity(t *testing.T) (string, *identity.Controller) {
	t.Helper()
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.kel"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

// capture runs a command and returns what it printed.
func capture(t *testing.T, run func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := run()
	w.Close()
	os.Stdout = old
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := r.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	if runErr != nil {
		t.Fatalf("command failed: %v", runErr)
	}
	return strings.TrimSpace(sb.String())
}

// The authorization the fixture mints must verify against the identity it
// signed with — the same check the hub performs before moving credit.
func TestTheFixtureMintsAnAuthorizationTheHubWouldAccept(t *testing.T) {
	dir, c := withIdentity(t)
	out := capture(t, func() error {
		return cmdX402Authorize([]string{
			"--home", dir, "--pay-to", "did:anet:provider",
			"--amount", "120", "--network", "hub:did:anet:hub",
			"--interaction", "ix-1",
		})
	})
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("output is not base64: %v", err)
	}
	var pp payment.PaymentPayload
	if err := json.Unmarshal(raw, &pp); err != nil {
		t.Fatalf("output is not a PaymentPayload: %v", err)
	}
	// The accepted terms must match what was asked for; a gateway checks
	// them before settling, and a mismatch here would read as the gateway
	// refusing a correct payment.
	if pp.Accepted.PayTo != "did:anet:provider" || pp.Accepted.Amount != "120" {
		t.Errorf("accepted terms = %+v", pp.Accepted)
	}
	if pp.X402Version != payment.Version {
		t.Errorf("x402Version = %d, want %d", pp.X402Version, payment.Version)
	}

	enc, _ := pp.Payload["authorization"].(string)
	authRaw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("authorization is not base64: %v", err)
	}
	auth, err := payment.UnmarshalAuthorization(authRaw)
	if err != nil {
		t.Fatalf("authorization does not decode: %v", err)
	}
	if err := auth.Verify(c.KEL(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("the fixture's authorization does not verify: %v", err)
	}
	if auth.Payer != c.AID() {
		t.Errorf("payer = %s, want the loaded identity", auth.Payer)
	}
	if auth.InteractionID != "ix-1" {
		t.Errorf("interaction = %q — the binding is what stops reuse", auth.InteractionID)
	}
	if auth.NotAfter <= auth.IssuedAt {
		t.Error("the authorization has no window, so it never expires")
	}
}

// Two payments in a row must not share a nonce, or the second settles as
// a replay of the first and the buyer pays once for two jobs.
func TestTwoAuthorizationsAreNotTheSameAuthorization(t *testing.T) {
	dir, _ := withIdentity(t)
	mint := func() string {
		return capture(t, func() error {
			return cmdX402Authorize([]string{
				"--home", dir, "--pay-to", "did:anet:p", "--amount", "10",
				"--network", "hub:did:anet:hub", "--interaction", "ix",
			})
		})
	}
	a, b := mint(), mint()
	if a == b {
		t.Fatal("two authorizations came out identical — the second would settle as a replay")
	}
}

// Bad input must be refused with something an operator can act on, not a
// panic and not an empty line that looks like success.
func TestTheFixtureRefusesIncompleteArguments(t *testing.T) {
	dir, _ := withIdentity(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no payee", []string{"--home", dir, "--amount", "10", "--network", "hub:x"}, "pay-to"},
		{"no amount", []string{"--home", dir, "--pay-to", "p", "--network", "hub:x"}, "amount"},
		{"no network", []string{"--home", dir, "--pay-to", "p", "--amount", "10"}, "network"},
		{"no identity", []string{"--pay-to", "p", "--amount", "10", "--network", "hub:x"}, "home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdX402Authorize(tc.args)
			if err == nil {
				t.Fatal("accepted, and would have produced an unusable payment")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the operator is not told what is missing: %v", err)
			}
		})
	}
}

// load must not accept an identity file that is not one.
func TestLoadRefusesSomethingThatIsNotAnIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "identity.kel"), []byte("not a kel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir); err == nil {
		t.Error("a file that is not a key history was loaded as one")
	}
}
