package org_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/org"
)

// rawCBOR is a value already encoded; rawMap writes it as it is.
type rawCBOR []byte

func encOne(t *testing.T, v any) []byte {
	t.Helper()
	if r, ok := v.(rawCBOR); ok {
		return r
	}
	b, err := coredet.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawMap encodes the key/value pairs kv as one CBOR map in the order given (fewer than 24 pairs),
// which is how a test writes a map that is not in CoreDet order or holds a key twice.
func rawMap(t *testing.T, kv ...any) []byte {
	t.Helper()
	out := []byte{0xa0 | byte(len(kv)/2)}
	for _, x := range kv {
		out = append(out, encOne(t, x)...)
	}
	return out
}

// signedWire wraps body in a SignedCredential signed by the conformance identity, so that a body
// is refused for its encoding and not for its signature.
func signedWire(t *testing.T, body []byte) []byte {
	t.Helper()
	c := identity.SuiteController()
	sig, seq := c.Sign(body)
	w, err := coredet.Marshal(&org.SignedCredential{Cred: body, Envelope: &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Core CORE-K26, CORE-E2: a credential decodes only from exactly the bytes this package writes.
// Each case is signed by its issuer, so it is refused for its encoding: a key that is missing, one
// written twice, a value in a longer form than CoreDet's, keys out of order, an epoch of 0 written
// out, a type tag of another type. The wire form is held to the same rule.
func TestCredentialDecodeIsStrict(t *testing.T) {
	const tag, orgID, subject = org.CredentialType, "bafyorg", "bafysubject"
	const issued, until = int64(1767225600000), int64(1798761600000)
	good := rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 5, issued, 6, until)
	c, err := org.UnmarshalCredential(signedWire(t, good))
	if err != nil {
		t.Fatalf("the base case is refused: %v", err)
	}
	if c.OrgID != orgID || c.Subject != subject || c.Role != "member" || c.IssuedAt != issued || c.NotAfter != until || c.Epoch != 0 {
		t.Fatalf("decoded %+v", c)
	}
	role := encOne(t, "member")
	longRole := append([]byte{0x78, byte(len("member"))}, role[1:]...) // text length in two bytes
	for name, body := range map[string][]byte{
		"no key 0":               rawMap(t, 1, orgID, 2, subject, 3, "member", 5, issued, 6, until),
		"another tag":            rawMap(t, 0, "anet.org-cred/2", 1, orgID, 2, subject, 3, "member", 5, issued, 6, until),
		"tag not text":           rawMap(t, 0, 1, 1, orgID, 2, subject, 3, "member", 5, issued, 6, until),
		"no issued_at":           rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 6, until),
		"no subject":             rawMap(t, 0, tag, 1, orgID, 3, "member", 5, issued, 6, until),
		"epoch 0 written out":    rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 4, 0, 5, issued, 6, until),
		"keys out of order":      rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 6, until, 5, issued),
		"key 3 twice":            rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 3, "admin", 5, issued, 6, until),
		"role in a longer form":  rawMap(t, 0, tag, 1, orgID, 2, subject, 3, rawCBOR(longRole), 5, issued, 6, until),
		"an unregistered key 7":  rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 5, issued, 6, until, 7, 0),
		"epoch a text":           rawMap(t, 0, tag, 1, orgID, 2, subject, 3, "member", 4, "1", 5, issued, 6, until),
		"not a map":              encOne(t, []any{tag, orgID}),
		"text keys (0.2 struct)": encOne(t, map[string]any{"OrgID": orgID, "Subject": subject}),
	} {
		if _, err := org.UnmarshalCredential(signedWire(t, body)); !errors.Is(err, org.ErrMalformed) {
			t.Errorf("%s: want ErrMalformed, got %v", name, err)
		}
	}
	// The wire form.
	w := signedWire(t, good)
	var sc org.SignedCredential
	if err := coredet.Unmarshal(w, &sc); err != nil {
		t.Fatal(err)
	}
	env := encOne(t, sc.Envelope)
	for name, wire := range map[string][]byte{
		"envelope first":      rawMap(t, 2, rawCBOR(env), 1, good),
		"no envelope":         rawMap(t, 1, good),
		"envelope null":       rawMap(t, 1, good, 2, nil),
		"no body":             rawMap(t, 2, rawCBOR(env)),
		"an unregistered key": rawMap(t, 1, good, 2, rawCBOR(env), 3, 0),
		"body not bytes":      rawMap(t, 1, rawCBOR(good), 2, rawCBOR(env)),
	} {
		if _, err := org.UnmarshalCredential(wire); !errors.Is(err, org.ErrMalformed) {
			t.Errorf("wire %s: want ErrMalformed, got %v", name, err)
		}
	}
	if _, err := org.UnmarshalCredential(rawMap(t, 1, good, 2, rawCBOR(env))); err != nil {
		t.Fatalf("the base wire, written by hand: %v", err)
	}
}

// ParseGenesis (core CORE-K26): only the encoding Marshal writes; a 0.2 genesis (no type tag) is
// ErrGenesisV02, anything else that is not a genesis ErrBadGenesis.
func TestParseGenesisIsStrict(t *testing.T) {
	g := &org.Genesis{GovernanceRoot: []string{"bafyb", "bafya", "bafyb"}, M: 2, Nonce: "n"}
	good, err := g.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(good, rawMap(t, 0, org.GenesisType, 1, []string{"bafya", "bafyb"}, 2, 2, 3, "n")) {
		t.Fatalf("Marshal does not sort and deduplicate the founders: %x", good)
	}
	if back, err := org.ParseGenesis(good); err != nil || back.M != 2 || back.Nonce != "n" || len(back.GovernanceRoot) != 2 {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	for name, b := range map[string][]byte{
		"founders not sorted":     rawMap(t, 0, org.GenesisType, 1, []string{"bafyb", "bafya"}, 2, 2, 3, "n"),
		"a founder twice":         rawMap(t, 0, org.GenesisType, 1, []string{"bafya", "bafya", "bafyb"}, 2, 2, 3, "n"),
		"an empty nonce written":  rawMap(t, 0, org.GenesisType, 1, []string{"bafya", "bafyb"}, 2, 2, 3, ""),
		"an unregistered key 4":   rawMap(t, 0, org.GenesisType, 1, []string{"bafya", "bafyb"}, 2, 2, 3, "n", 4, 0),
		"another tag":             rawMap(t, 0, "anet.org-genesis/2", 1, []string{"bafya", "bafyb"}, 2, 2, 3, "n"),
		"keys out of order":       rawMap(t, 0, org.GenesisType, 2, 2, 1, []string{"bafya", "bafyb"}, 3, "n"),
		"no key 0 and no key 1":   rawMap(t, 2, 2),
		"not a map":               encOne(t, "genesis"),
		"m a text":                rawMap(t, 0, org.GenesisType, 1, []string{"bafya", "bafyb"}, 2, "2", 3, "n"),
		"governance_root a text":  rawMap(t, 0, org.GenesisType, 1, "bafya", 2, 1),
		"truncated":               good[:len(good)-1],
		"trailing bytes":          append(bytes.Clone(good), 0),
		"a key 0 that is not tag": rawMap(t, 0, 7, 1, []string{"bafya"}, 2, 1),
	} {
		if _, err := org.ParseGenesis(b); !errors.Is(err, org.ErrBadGenesis) {
			t.Errorf("%s: want ErrBadGenesis, got %v", name, err)
		}
	}
	if _, err := org.ParseGenesis(rawMap(t, 1, []string{"bafya"}, 2, 1, 3, "n")); !errors.Is(err, org.ErrGenesisV02) {
		t.Errorf("a 0.2 genesis: want ErrGenesisV02, got %v", err)
	}
}
