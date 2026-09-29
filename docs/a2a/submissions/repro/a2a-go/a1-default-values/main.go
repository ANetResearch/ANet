// A1: a2acrypto signs and verifies the card bytes as given, without the
// default-value removal of A2A §8.4.1 rule 1. The same card, spelled with
// and without a default value, needs two different signatures, and a round
// trip through a2a.AgentCard invalidates a signature.
//
// Run: go run ./a1-default-values
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"
)

func main() {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: priv, KeyID: "k"})
	must(err)
	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{
		KeyResolver: a2acrypto.KeyResolverFunc(func(context.Context, string) (crypto.PublicKey, error) { return pub, nil }),
	})

	// The same AgentCard content twice: AgentExtension.required is neither
	// REQUIRED nor `optional`, so rule 1 says its default (false) is omitted
	// before canonicalization; both spellings must give the same payload.
	withDefault := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u","required":false}]}}`)
	stripped := []byte(`{"name":"n","capabilities":{"extensions":[{"uri":"u"}]}}`)

	sig, err := signer.Sign(ctx, withDefault)
	must(err)
	errSame := verifier.Verify(ctx, withDefault, sig)
	errStripped := verifier.Verify(ctx, stripped, sig)

	var card a2a.AgentCard
	must(json.Unmarshal(withDefault, &card))
	roundTrip, err := json.Marshal(&card)
	must(err)
	errRoundTrip := verifier.Verify(ctx, roundTrip, sig)

	fmt.Printf("verify, bytes as signed:                     %v\n", errOrOK(errSame))
	fmt.Printf("verify, same card without the default:       %v\n", errOrOK(errStripped))
	fmt.Printf("a2a.AgentCard round trip:                    %s\n", roundTrip)
	fmt.Printf("verify, after the a2a.AgentCard round trip:  %v\n", errOrOK(errRoundTrip))
	fmt.Println("expected (A2A §8.4.1 rule 1): all three verify")

	if errSame == nil && (errStripped != nil || errRoundTrip != nil) {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
}

func errOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}
