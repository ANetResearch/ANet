// A4: the JWS "signature" member is decoded with base64.RawURLEncoding,
// which skips CR and LF, so one signature has many accepted spellings
// (Strict() still skips them). "protected" is decoded the same way, but it
// also enters the JWS signing input as written, so a line break there makes
// the signature fail; it is printed as a control.
//
// Run: go run ./a4-base64-linebreaks
// Exit status: 0 = behaviour reproduced, 1 = not reproduced, 2 = setup error.
package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"

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
	card := []byte(`{"name":"n","description":"d"}`)
	sig, err := signer.Sign(ctx, card)
	must(err)

	inSig := *sig
	inSig.Signature = sig.Signature[:10] + "\r\n" + sig.Signature[10:]
	inProtected := *sig
	inProtected.Protected = sig.Protected[:8] + "\n" + sig.Protected[8:]
	errSig := verifier.Verify(ctx, card, &inSig)
	errProt := verifier.Verify(ctx, card, &inProtected)
	fmt.Printf("CR LF inside \"signature\": %v\n", errOrOK(errSig))
	fmt.Printf("LF inside \"protected\":    %v (control)\n", errOrOK(errProt))
	fmt.Println("expected: rejected (not base64url, RFC 7515 §2 / RFC 4648 §5)")

	if errSig == nil {
		fmt.Println("reproduced: yes")
		return
	}
	fmt.Println("reproduced: no")
	os.Exit(1)
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
