// A1 (cross-SDK part): signs one Agent Card in several spellings with
// a2a-go's a2acrypto and writes the signed cards and the JWKS, so that the
// same files can be verified with a2a-python (../a2a-python/a1x_verify.py)
// and @a2a-js/sdk (../a2a-js/a1x-verify.mjs).
//
// Each variant differs from "base" in one default value. Under A2A §8.4.1
// rule 1 a default value is omitted before canonicalization unless the field
// is REQUIRED or `optional`:
//   - AgentExtension.required / .description and AgentSkill.examples are
//     neither, so a conformant verifier drops them and a signature over the
//     bytes as given (a2a-go) does not verify there;
//   - AgentCapabilities.streaming / .pushNotifications are `optional bool`,
//     so an explicit false is kept and the signature should verify;
//   - AgentCard.description is REQUIRED, so "" is kept; the inside of a
//     google.protobuf.Struct (AgentExtension.params) is not subject to the
//     rule at all: both should verify.
//
// One more file, base-reserved-by-a2a-go.json, is the signed "base" card
// after a round trip through a2a.AgentCard (what a registry or proxy built
// on a2a-go serves): encoding/json adds "streaming":false and
// "pushNotifications":false, which the card never had (issue draft A2), so
// the card no longer verifies anywhere.
//
// The key is a test key derived from a public string, so the output is the
// same on every run.
//
// Run: go run ./a1x-cross-sdk-vectors <output dir>
package main

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"
)

// One card, written out as a template so that each variant is spelled
// exactly as intended (encoding/json would reorder or drop members).
const card = `{"name":"repro","description":%DESC%,"version":"1.0.0",` +
	`"supportedInterfaces":[{"url":"https://agent.example/a2a","protocolBinding":"JSONRPC","protocolVersion":"1.0"}],` +
	`"capabilities":{%CAPS%"extensions":[{"uri":"https://example.org/ext/v1"%EXT%}]},` +
	`"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],` +
	`"skills":[{"id":"s","name":"S","description":"d","tags":["t"]%SKILL%}]}`

const desc = `"cross-SDK default-value vectors"`

var variants = []struct{ name, desc, caps, ext, skill, note string }{
	{"base", desc, "", "", "", "no default values"},
	{"ext-required-false", desc, "", `,"required":false`, "", "non-optional bool at its default: rule 1 drops it"},
	{"ext-description-empty", desc, "", `,"description":""`, "", "non-optional string at its default: rule 1 drops it"},
	{"skill-examples-empty", desc, "", "", `,"examples":[]`, "empty repeated field: rule 1 drops it"},
	{"caps-streaming-false", desc, `"streaming":false,`, "", "", "`optional bool` set to false: rule 1 keeps it"},
	{"caps-push-false", desc, `"pushNotifications":false,`, "", "", "`optional bool` set to false: rule 1 keeps it"},
	{"streaming-and-required-false", desc, `"streaming":false,`, `,"required":false`, "", "both (the case first seen)"},
	{"card-description-empty", `""`, "", "", "", "REQUIRED string at its default: rule 1 keeps it"},
	{"ext-params-empty-string", desc, "", `,"params":{"note":""}`, "", "empty string inside a Struct (params): rule 1 does not touch it"},
}

const kid = "a2a-cross-sdk-repro-key"

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: go run ./a1x-cross-sdk-vectors <output dir>")
		os.Exit(2)
	}
	out := os.Args[1]
	must(os.MkdirAll(out, 0o755))

	seed := sha256.Sum256([]byte("a2a cross-SDK repro test key; not secret"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	signer, err := a2acrypto.NewSigner(a2acrypto.SignerConfig{PrivateKey: priv, KeyID: kid})
	must(err)

	jwks := map[string]any{"keys": []any{map[string]any{
		"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(pub),
	}}}
	writeJSON(filepath.Join(out, "jwks.json"), jwks)

	var index []map[string]string
	for _, v := range variants {
		body := strings.NewReplacer("%DESC%", v.desc, "%CAPS%", v.caps, "%EXT%", v.ext, "%SKILL%", v.skill).Replace(card)
		if !json.Valid([]byte(body)) {
			must(fmt.Errorf("variant %s is not valid JSON", v.name))
		}
		sig, err := signer.Sign(context.Background(), json.RawMessage(body))
		must(err)
		sigJSON, err := json.Marshal([]any{sig})
		must(err)
		signed := strings.TrimSuffix(body, "}") + `,"signatures":` + string(sigJSON) + "}"
		must(os.WriteFile(filepath.Join(out, v.name+".json"), []byte(signed), 0o644))
		index = append(index, map[string]string{"file": v.name + ".json", "note": v.note})
		fmt.Printf("wrote %s.json (%s)\n", v.name, v.note)

		if v.name == "base" {
			var parsed a2a.AgentCard
			must(json.Unmarshal([]byte(signed), &parsed))
			reserved, err := json.Marshal(&parsed)
			must(err)
			const note = "base, re-served through a2a.AgentCard: optional booleans invented (A2)"
			must(os.WriteFile(filepath.Join(out, "base-reserved-by-a2a-go.json"), reserved, 0o644))
			index = append(index, map[string]string{"file": "base-reserved-by-a2a-go.json", "note": note})
			fmt.Printf("wrote base-reserved-by-a2a-go.json (%s)\n", note)
		}
	}
	writeJSON(filepath.Join(out, "index.json"), index)

	// For the table: what a2a-go's own verifier says about the same files.
	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{
		KeyResolver: a2acrypto.KeyResolverFunc(func(context.Context, string) (crypto.PublicKey, error) { return pub, nil }),
	})
	fmt.Println()
	for _, e := range index {
		raw, err := os.ReadFile(filepath.Join(out, e["file"]))
		must(err)
		var parsed a2a.AgentCard
		must(json.Unmarshal(raw, &parsed))
		got := "VERIFIED"
		if err := verifier.Verify(context.Background(), raw, &parsed.Signatures[0]); err != nil {
			got = "FAILED"
		}
		fmt.Printf("%-30s a2a-go: %s\n", strings.TrimSuffix(e["file"], ".json"), got)
	}
}

func writeJSON(path string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	must(err)
	must(os.WriteFile(path, append(b, '\n'), 0o644))
}

func must(err error) {
	if err != nil {
		fmt.Println("setup error:", err)
		os.Exit(2)
	}
}
