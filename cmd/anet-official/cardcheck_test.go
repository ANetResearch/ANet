package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
)

// a2aCard is a minimal A2A 1.0 AgentCard with every required field.
func a2aCard() map[string]any {
	return map[string]any{
		"name":        "Recipe Agent",
		"description": "Helps with recipes.",
		"version":     "1.0.0",
		"supportedInterfaces": []any{
			map[string]any{"url": "https://recipes.example.com/a2a/v1", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"},
		},
		"capabilities":       map[string]any{"streaming": true, "pushNotifications": false},
		"defaultInputModes":  []any{"text/plain"},
		"defaultOutputModes": []any{"application/json"},
		"skills": []any{
			map[string]any{"id": "find", "name": "Find recipes", "description": "Finds recipes.", "tags": []any{"food"}},
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signWith appends a JWS signature made by sign over payload, with the
// given header, to the card.
func signWith(t *testing.T, card map[string]any, hdr map[string]string, payload []byte, sign func([]byte) []byte) {
	t.Helper()
	protected := b64u(mustJSON(t, hdr))
	input := protected + "." + b64u(payload)
	sigs, _ := card["signatures"].([]any)
	card["signatures"] = append(sigs, map[string]any{"protected": protected, "signature": b64u(sign([]byte(input)))})
}

func payloadAsGiven(t *testing.T, card map[string]any) []byte {
	t.Helper()
	p, err := a2acard.SigningPayload(mustJSON(t, card))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func payloadWithoutDefaults(t *testing.T, card map[string]any) []byte {
	t.Helper()
	s, _ := stripDefaults(card, "AgentCard", "")
	delete(s, "signatures")
	p, err := a2acard.Canonicalize(mustJSON(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func codes(res *cardResult) map[string]string {
	out := map[string]string{}
	for _, i := range res.Issues {
		out[i.Code] = i.Severity
	}
	return out
}

func TestCardStructure(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		valid  bool
		codes  map[string]string // code -> severity that must be reported
	}{
		{"minimal card", func(map[string]any) {}, true, nil},
		{"missing name", func(c map[string]any) { delete(c, "name") }, false, map[string]string{"required": sevError}},
		{"missing skills", func(c map[string]any) { delete(c, "skills") }, false, map[string]string{"required": sevError}},
		{"skill without tags", func(c map[string]any) {
			delete(c["skills"].([]any)[0].(map[string]any), "tags")
		}, false, map[string]string{"required": sevError}},
		{"wrong type", func(c map[string]any) { c["version"] = 1 }, false, map[string]string{"type": sevError}},
		{"A2A 0.3 fields", func(c map[string]any) { c["url"] = "https://x"; c["preferredTransport"] = "JSONRPC" }, true,
			map[string]string{"a2a_0_3_field": sevWarning, "unknown_field": sevWarning}},
		{"unknown member", func(c map[string]any) { c["capabilities"].(map[string]any)["stream"] = true }, true,
			map[string]string{"unknown_field": sevWarning}},
		{"undefined security scheme", func(c map[string]any) {
			c["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"oauth": map[string]any{"list": []any{}}}}}
		}, false, map[string]string{"undefined_scheme": sevError}},
		{"security scheme oneof", func(c map[string]any) {
			c["securitySchemes"] = map[string]any{"k": map[string]any{
				"apiKeySecurityScheme":   map[string]any{"location": "header", "name": "X-Key"},
				"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"},
			}}
		}, false, map[string]string{"oneof": sevError}},
		{"good security scheme", func(c map[string]any) {
			c["securitySchemes"] = map[string]any{"k": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
			c["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"k": map[string]any{"list": []any{}}}}}
		}, true, nil},
		{"deprecated oauth flow", func(c map[string]any) {
			c["securitySchemes"] = map[string]any{"o": map[string]any{"oauth2SecurityScheme": map[string]any{
				"flows": map[string]any{"implicit": map[string]any{"authorizationUrl": "https://a"}}}}}
		}, true, map[string]string{"deprecated": sevWarning}},
		{"duplicate skill", func(c map[string]any) {
			c["skills"] = append(c["skills"].([]any), map[string]any{"id": "find", "name": "n", "description": "d", "tags": []any{"x"}})
		}, false, map[string]string{"duplicate_skill": sevError}},
		{"http url", func(c map[string]any) {
			c["supportedInterfaces"].([]any)[0].(map[string]any)["url"] = "http://recipes.example.com/a2a"
		}, true, map[string]string{"insecure_url": sevWarning}},
		{"relative url", func(c map[string]any) {
			c["supportedInterfaces"].([]any)[0].(map[string]any)["url"] = "/a2a"
		}, false, map[string]string{"url": sevError}},
		{"not a media type", func(c map[string]any) { c["defaultInputModes"] = []any{"text"} }, true,
			map[string]string{"media_type": sevWarning}},
		{"relay binding without tenant", func(c map[string]any) {
			c["supportedInterfaces"] = []any{map[string]any{"url": "https://hub.example/relay", "protocolBinding": a2acard.BindingRelayURI, "protocolVersion": "1.0"}}
		}, false, map[string]string{"tenant": sevError}},
		{"extension without uri", func(c map[string]any) {
			c["capabilities"].(map[string]any)["extensions"] = []any{map[string]any{"required": true}}
		}, false, map[string]string{"extension_uri": sevError}},
		{"optional x402", func(c map[string]any) {
			c["capabilities"].(map[string]any)["extensions"] = []any{map[string]any{"uri": x402ExtensionURI}}
		}, true, map[string]string{"x402_optional": sevInfo}},
	}
	for _, c := range cases {
		card := a2aCard()
		c.change(card)
		res := checkCard(mustJSON(t, card), nil, nil, 0)
		if res.Valid != c.valid {
			t.Errorf("%s: valid=%v, want %v; issues %+v", c.name, res.Valid, c.valid, res.Issues)
		}
		got := codes(res)
		for code, sev := range c.codes {
			if got[code] != sev {
				t.Errorf("%s: code %s reported as %q, want %s; issues %+v", c.name, code, got[code], sev, res.Issues)
			}
		}
		if res.Profile != "a2a" {
			t.Errorf("%s: profile %s", c.name, res.Profile)
		}
	}
}

func TestCardSyntax(t *testing.T) {
	res := checkCard([]byte(`{"name":"a","name":"b"}`), nil, nil, 0)
	if res.Valid || codes(res)["not_i_json"] != sevError {
		t.Errorf("duplicate names: %+v", res.Issues)
	}
	res = checkCard([]byte(`[1]`), nil, nil, 0)
	if res.Valid {
		t.Error("an array is not a card")
	}
}

// Default values: the checker reports them and computes both payloads;
// a card without defaults has one payload.
func TestCardDefaults(t *testing.T) {
	card := a2aCard()
	if res := checkCard(mustJSON(t, card), nil, nil, 0); res.Payload == nil || res.Payload.Differ {
		t.Fatalf("a card without defaults: %+v", res.Payload)
	}
	card["capabilities"].(map[string]any)["extensions"] = []any{}
	card["supportedInterfaces"].([]any)[0].(map[string]any)["tenant"] = ""
	card["skills"].([]any)[0].(map[string]any)["examples"] = []any{}
	res := checkCard(mustJSON(t, card), nil, nil, 0)
	if res.Payload == nil || !res.Payload.Differ || codes(res)["defaults_present"] != sevWarning {
		t.Fatalf("defaults: %+v %+v", res.Payload, res.Issues)
	}
	want := "/capabilities/extensions,/skills/0/examples,/supportedInterfaces/0/tenant"
	if got := strings.Join(res.Payload.DefaultsPresent, ","); got != want {
		t.Errorf("defaults present = %s, want %s", got, want)
	}
	// Required fields and explicit-presence fields stay even at their
	// default: streaming false is kept, like the A2A §8.4.1 example.
	stripped, _ := stripDefaults(card, "AgentCard", "")
	if _, ok := stripped["capabilities"].(map[string]any)["pushNotifications"]; !ok {
		t.Error("an optional (explicit presence) field set to false must be kept")
	}
}

func TestCardSignatures(t *testing.T) {
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecBytes, _ := ecKey.PublicKey.Bytes() // 0x04 || x || y
	jwks := func(keys ...map[string]any) []jwk {
		raw := mustJSON(t, map[string]any{"keys": keys})
		out, err := parseJWKS(raw)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	edJWK := map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "ed", "x": b64u(edPub)}
	ecJWK := map[string]any{"kty": "EC", "crv": "P-256", "kid": "ec", "x": b64u(ecBytes[1:33]), "y": b64u(ecBytes[33:])}
	rsaJWK := map[string]any{"kty": "RSA", "kid": "rsa", "n": b64u(rsaKey.N.Bytes()), "e": b64u(big.NewInt(int64(rsaKey.E)).Bytes())}
	keys := jwks(edJWK, ecJWK, rsaJWK)

	edSign := func(in []byte) []byte { return ed25519.Sign(edPriv, in) }
	ecSign := func(in []byte) []byte {
		d := sha256.Sum256(in)
		r, s, err := ecdsa.Sign(rand.Reader, ecKey, d[:])
		if err != nil {
			t.Fatal(err)
		}
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out
	}
	rsaSign := func(pss bool) func([]byte) []byte {
		return func(in []byte) []byte {
			d := sha256.Sum256(in)
			var sig []byte
			var err error
			if pss {
				sig, err = rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, d[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
			} else {
				sig, err = rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, d[:])
			}
			if err != nil {
				t.Fatal(err)
			}
			return sig
		}
	}
	withDefaults := func() map[string]any {
		c := a2aCard()
		c["capabilities"].(map[string]any)["extensions"] = []any{}
		return c
	}

	cases := []struct {
		name        string
		card        func() map[string]any
		alg, kid    string
		stripped    bool // sign the payload without defaults
		sign        func([]byte) []byte
		keys        []jwk
		wantResult  string
		wantPayload string
	}{
		{"EdDSA as given", a2aCard, "EdDSA", "ed", false, edSign, keys, "verified", "as_given"},
		{"ES256 as given", a2aCard, "ES256", "ec", false, ecSign, keys, "verified", "as_given"},
		{"RS256 as given", a2aCard, "RS256", "rsa", false, rsaSign(false), keys, "verified", "as_given"},
		{"PS256 as given", a2aCard, "PS256", "rsa", false, rsaSign(true), keys, "verified", "as_given"},
		{"ES256 without defaults", withDefaults, "ES256", "ec", true, ecSign, keys, "verified", "defaults_removed"},
		{"EdDSA as given with defaults", withDefaults, "EdDSA", "ed", false, edSign, keys, "verified", "as_given"},
		{"wrong key", a2aCard, "EdDSA", "ec", false, edSign, keys, "invalid", ""},
		{"alg and key disagree", a2aCard, "ES256", "rsa", false, ecSign, keys, "invalid", ""},
		{"no key for kid", a2aCard, "EdDSA", "other", false, edSign, keys, "unverifiable", ""},
		{"no keys at all", a2aCard, "EdDSA", "ed", false, edSign, nil, "unverifiable", ""},
		{"single key without kid", a2aCard, "EdDSA", "anything", false, edSign,
			jwks(map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64u(edPub)}), "verified", "as_given"},
	}
	for _, c := range cases {
		card := c.card()
		payload := payloadAsGiven(t, card)
		if c.stripped {
			payload = payloadWithoutDefaults(t, card)
		}
		signWith(t, card, map[string]string{"alg": c.alg, "kid": c.kid, "typ": "JOSE"}, payload, c.sign)
		res := checkCard(mustJSON(t, card), c.keys, nil, 0)
		if len(res.Signatures) != 1 {
			t.Fatalf("%s: %d signature reports", c.name, len(res.Signatures))
		}
		s := res.Signatures[0]
		if s.Result != c.wantResult || s.Payload != c.wantPayload {
			t.Errorf("%s: result %s payload %s (%s), want %s %s", c.name, s.Result, s.Payload, s.Detail, c.wantResult, c.wantPayload)
		}
		if (c.wantResult == "invalid") == res.Valid {
			t.Errorf("%s: an invalid signature must make the card invalid, and only that: valid=%v", c.name, res.Valid)
		}
	}

	// A tampered card no longer verifies.
	card := a2aCard()
	signWith(t, card, map[string]string{"alg": "EdDSA", "kid": "ed"}, payloadAsGiven(t, card), edSign)
	card["name"] = "Recipe Agent 2"
	if res := checkCard(mustJSON(t, card), keys, nil, 0); res.Signatures[0].Result != "invalid" {
		t.Errorf("tampered card: %+v", res.Signatures[0])
	}

	// Malformed headers.
	for name, hdr := range map[string]string{
		"not base64": "!!!",
		"alg none":   b64u([]byte(`{"alg":"none","kid":"ed"}`)),
		"not json":   b64u([]byte(`nope`)),
	} {
		card := a2aCard()
		card["signatures"] = []any{map[string]any{"protected": hdr, "signature": b64u([]byte("x"))}}
		res := checkCard(mustJSON(t, card), keys, nil, 0)
		if res.Signatures[0].Result != "malformed" || res.Valid {
			t.Errorf("%s: %+v", name, res.Signatures[0])
		}
	}
}

func TestJWKSParsing(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":        `{"keys":[]}`,
		"short ed":     `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA"}]}`,
		"x25519":       `{"keys":[{"kty":"OKP","crv":"X25519","x":"` + b64u(make([]byte, 32)) + `"}]}`,
		"off curve":    `{"keys":[{"kty":"EC","crv":"P-256","x":"` + b64u(make([]byte, 32)) + `","y":"` + b64u(make([]byte, 32)) + `"}]}`,
		"weak rsa":     `{"keys":[{"kty":"RSA","n":"` + b64u(make([]byte, 128)) + `","e":"AQAB"}]}`,
		"unknown kty":  `{"keys":[{"kty":"oct","k":"AAAA"}]}`,
		"not an array": `{"keys":{}}`,
	} {
		if _, err := parseJWKS(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// anetCard builds an anet network card for c at time now, signed with its
// current key.
func anetCard(t *testing.T, c *identity.Controller, now int64) []byte {
	t.Helper()
	aid := c.AID()
	n := strconv.FormatInt(now, 10)
	card := map[string]any{
		"name": "anet-tools", "description": "Official tools.", "version": "0.2.0",
		"supportedInterfaces": []any{map[string]any{"url": "https://hub.example/relay/v2",
			"protocolBinding": a2acard.BindingRelayURI, "protocolVersion": "1.0", "tenant": aid}},
		"capabilities": map[string]any{"streaming": false, "pushNotifications": false, "extensions": []any{
			map[string]any{"uri": a2acard.ExtCardURI, "params": map[string]any{"aid": aid, "seq": n, "issuedAt": n, "notBefore": n}},
		}},
		"defaultInputModes": []any{"application/json"}, "defaultOutputModes": []any{"application/json"},
		"skills": []any{map[string]any{"id": "text.digest", "name": "Text digest", "description": "Hashes text.", "tags": []any{"hash"}}},
	}
	signed, err := a2acard.SignWithController(mustJSON(t, card), c, "https://hub.example/agents/"+aid+"/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestAnetCard(t *testing.T) {
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	const now = 1_790_000_000_000
	card := anetCard(t, c, now)
	kelBytes, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	kel, _ := identity.UnmarshalKEL(kelBytes)

	res := checkCard(card, nil, kel, now)
	if res.Profile != "anet" || res.Anet == nil || res.Anet.Result != "accepted" || res.Anet.AID != c.AID() {
		t.Fatalf("anet card with its KEL: %+v", res.Anet)
	}
	if s := res.Signatures[0]; s.Result != "verified" || s.KeySource != "kel" || s.Payload != "as_given" {
		t.Errorf("signature via KEL: %+v", s)
	}
	if !res.Valid {
		t.Errorf("issues: %+v", res.Issues)
	}

	res = checkCard(card, nil, nil, now)
	if res.Anet.Result != "unverifiable" || res.Signatures[0].Result != "unverifiable" || !res.Valid {
		t.Errorf("without a KEL the card is unverifiable, not rejected: %+v %+v", res.Anet, res.Signatures[0])
	}

	// Signed for a time far ahead: an anet hub refuses it.
	future := anetCard(t, c, now+3_600_000)
	res = checkCard(future, nil, kel, now)
	if res.Anet.Result != "rejected" || res.Anet.Code != string(a2acard.CodeNotYetValid) || res.Valid {
		t.Errorf("future card: %+v", res.Anet)
	}

	// Another identity's KEL does not verify it.
	other, _ := identity.Incept()
	res = checkCard(card, nil, other.KEL(), now)
	if res.Anet.Result != "rejected" || res.Signatures[0].Result == "verified" {
		t.Errorf("wrong KEL: %+v %+v", res.Anet, res.Signatures[0])
	}

	// Through the handler, with the KEL as base64.
	args := mustJSON(t, map[string]any{"card_json": string(card), "kel": base64.StdEncoding.EncodeToString(kelBytes), "now_ms": now})
	out, err := handleCardValidate(context.Background(), testEnv(t), args)
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*cardResult); r.Anet.Result != "accepted" {
		t.Errorf("handler: %+v", r.Anet)
	}
}

func TestCardArguments(t *testing.T) {
	e := testEnv(t)
	for _, bad := range []string{`{}`, `{"card":{},"card_json":"{}"}`, `{"card_json":"{}","kel":"%%"}`,
		`{"card_json":"{}","kel":"AAAA"}`, `{"card_json":"{}","jwks":{"keys":[]}}`} {
		if _, err := handleCardValidate(context.Background(), e, []byte(bad)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
	// A card sent as an object (re-encoded by the daemon) is checked too.
	out, err := handleCardValidate(context.Background(), e, mustJSON(t, map[string]any{"card": a2aCard()}))
	if err != nil || !out.(*cardResult).Valid {
		t.Errorf("card object: %+v %v", out, err)
	}
}

// The work of one check is bounded: at most a2acard.MaxSignatures
// signatures are verified (700 signatures against 16 keys of one kid held
// a core for fifteen seconds), a KEL is at most maxKELEvents long, and a
// passed deadline stops the check between signatures.
func TestCardWorkIsBounded(t *testing.T) {
	e := testEnv(t)
	var keys []any
	for i := 0; i < 16; i++ {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		keys = append(keys, map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "k", "x": b64u(pub)})
	}
	card := a2aCard()
	var sigs []any
	for i := 0; i < 300; i++ {
		sigs = append(sigs, map[string]any{"protected": b64u([]byte(`{"alg":"EdDSA","kid":"k"}`)), "signature": b64u(make([]byte, 64))})
	}
	card["signatures"] = sigs
	card["description"] = strings.Repeat("d", 100<<10)
	args := mustJSON(t, map[string]any{"card": card, "jwks": map[string]any{"keys": keys}})

	out, err := handleCardValidate(context.Background(), e, args)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(*cardResult)
	if len(res.Signatures) != maxCheckedSignatures || codes(res)["signatures_not_checked"] != sevWarning {
		t.Errorf("%d signature reports, issues %v", len(res.Signatures), codes(res))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handleCardValidate(ctx, e, args); err != errBudget {
		t.Errorf("past the deadline: err = %v, want the budget error", err)
	}

	c, _ := identity.Incept()
	for i := 0; i < maxKELEvents; i++ {
		if err := c.Rotate(uint64(1_790_000_000_000 + i)); err != nil {
			t.Fatal(err)
		}
	}
	kel, err := identity.MarshalKEL(c.KEL())
	if err != nil {
		t.Fatal(err)
	}
	long := mustJSON(t, map[string]any{"card": a2aCard(), "kel": base64.StdEncoding.EncodeToString(kel)})
	if _, err := handleCardValidate(context.Background(), e, long); err == nil || !strings.Contains(err.Error(), "events") {
		t.Errorf("a KEL of %d events: err = %v", len(c.KEL()), err)
	}
}

// An anet card with more signatures than a hub admits is rejected by the
// anet rules, and still only the first signatures are verified.
func TestAnetCardWithTooManySignatures(t *testing.T) {
	c, _ := identity.Incept()
	const now = 1_790_000_000_000
	var card map[string]any
	if err := json.Unmarshal(anetCard(t, c, now), &card); err != nil {
		t.Fatal(err)
	}
	sigs := card["signatures"].([]any)
	for len(sigs) <= a2acard.MaxSignatures {
		sigs = append(sigs, sigs[0])
	}
	card["signatures"] = sigs
	res := checkCard(mustJSON(t, card), nil, c.KEL(), now)
	if res.Anet == nil || res.Anet.Result != "rejected" || res.Anet.Code != string(a2acard.CodeTooLarge) {
		t.Errorf("anet: %+v", res.Anet)
	}
	if len(res.Signatures) != maxCheckedSignatures || res.Signatures[0].Result != "verified" {
		t.Errorf("signatures: %d, first %+v", len(res.Signatures), res.Signatures[0])
	}
}

// The example published on the card is a card this checker passes: it is
// the first thing an agent tries, and it should show a clean report.
func TestCardExampleIsClean(t *testing.T) {
	for _, c := range allCapabilities() {
		if c.ID != "a2a.card.validate" {
			continue
		}
		for _, ex := range c.Examples {
			out, err := handleCardValidate(context.Background(), testEnv(t), []byte(ex))
			if err != nil {
				t.Fatal(err)
			}
			if res := out.(*cardResult); !res.Valid || res.Warnings != 0 {
				t.Errorf("example %s: %+v", ex, res.Issues)
			}
		}
	}
}
