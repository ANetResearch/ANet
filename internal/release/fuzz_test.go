package release

// Fuzz targets for what a release mirror controls: the armored SSHSIG in
// release.json.sig and the release.json it covers (docs/notes/0033). Under
// plain `go test` they run their seeds only.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fuzzVector reads a file of testdata/sshsig for a seed; f has no Helper
// that fails a seed run the way t.Fatal does, so a missing vector is fatal
// here too.
func fuzzVector(f *testing.F, name string) []byte {
	f.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sshsig", name))
	if err != nil {
		f.Fatal(err)
	}
	return b
}

// sigBlob is the SSHSIG blob of s, the inverse of ParseSignature.
func sigBlob(s *Signature) []byte {
	var sb bytes.Buffer
	putString(&sb, []byte(keyTypeEd))
	putString(&sb, s.Sig)
	var b bytes.Buffer
	b.WriteString(sshsigMagic)
	putUint32(&b, sshsigVersion)
	putString(&b, keyBlob(s.PublicKey))
	putString(&b, []byte(s.Namespace))
	putString(&b, nil)
	putString(&b, []byte(s.HashAlg))
	putString(&b, sb.Bytes())
	return b.Bytes()
}

// FuzzParseSignature: an armored signature either parses into exactly the
// fields its blob encodes (re-encoding and parsing again gives the same
// signature), or is refused; and a signature the vector trust accepts is by
// a key that trust lists, in the namespace asked for.
func FuzzParseSignature(f *testing.F) {
	for _, n := range []string{"release.json.sig", "release.json.sha256.sig", "release.json.otherkey.sig", "release.json.wrongns.sig"} {
		f.Add(fuzzVector(f, n))
	}
	f.Add([]byte(sshsigBegin + "\n" + sshsigEnd + "\n"))
	f.Add([]byte(sshsigBegin + "\nU1NIU0lH\n" + sshsigEnd))
	msg := fuzzVector(f, "release.json")
	keys, err := ParseAllowedSigners(string(fuzzVector(f, "allowed_signers")))
	if err != nil {
		f.Fatal(err)
	}
	tr := Trust{Keys: keys}
	f.Fuzz(func(t *testing.T, armored []byte) {
		s, err := ParseSignature(armored)
		if err != nil {
			if s != nil {
				t.Fatal("an error with a signature")
			}
			return
		}
		if len(s.PublicKey) != ed25519.PublicKeySize || len(s.Sig) != ed25519.SignatureSize || s.Namespace == "" ||
			(s.HashAlg != "sha512" && s.HashAlg != "sha256") {
			t.Fatalf("parsed a signature that does not hold together: %+v", s)
		}
		again, err := ParseSignature(armorSig(sigBlob(s)))
		if err != nil || !reflect.DeepEqual(again, s) {
			t.Fatalf("re-encoded signature parses as %+v, %v; want %+v", again, err, s)
		}
		if v, err := tr.Verify(msg, armored); err == nil {
			if !tr.accepts(v.PublicKey) || v.Namespace != Namespace {
				t.Fatalf("verified a signature by %s in %q", Fingerprint(v.PublicKey), v.Namespace)
			}
			if !ed25519.Verify(v.PublicKey, signedData(v.Namespace, v.HashAlg, digest(v.HashAlg, msg)), v.Sig) {
				t.Fatal("Verify accepted a signature ed25519 refuses")
			}
		}
	})
}

// FuzzVerifyManifest: release.json and its signature as a mirror serves
// them. A manifest that parses survives a JSON round trip unchanged and
// satisfies what install.sh relies on (validate); one that verifies is
// signed by a trusted key and names that key.
func FuzzVerifyManifest(f *testing.F) {
	raw := fuzzVector(f, "release.json")
	sig := fuzzVector(f, "release.json.sig")
	f.Add(raw, sig)
	f.Add(raw, fuzzVector(f, "release.json.sha256.sig"))
	f.Add(raw, fuzzVector(f, "release.json.otherkey.sig"))
	f.Add([]byte(`{"schema":"anet-release/1"}`), sig)
	f.Add([]byte(`{}{}`), []byte{})
	keys, err := ParseAllowedSigners(string(fuzzVector(f, "allowed_signers")))
	if err != nil {
		f.Fatal(err)
	}
	tr := Trust{Keys: keys}
	f.Fuzz(func(t *testing.T, raw, sig []byte) {
		if m, err := ParseManifest(raw); err == nil {
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			m2, err := ParseManifest(b)
			if err != nil || !reflect.DeepEqual(m, m2) {
				t.Fatalf("re-encoded manifest parses as %+v, %v; want %+v", m2, err, m)
			}
			if _, err := CompareVersions(m.Version, m.Version); err != nil {
				t.Fatalf("a manifest version CompareVersions refuses: %q: %v", m.Version, err)
			}
			for name, a := range m.Assets {
				got, ga, err := m.AssetFor(a.Variant, a.OS, a.Arch)
				if err != nil || got != name || ga != a {
					t.Fatalf("asset %q is not what AssetFor finds: %q %+v %v", name, got, ga, err)
				}
			}
		}
		m, s, err := VerifyManifest(raw, sig, tr)
		if err != nil {
			return
		}
		if !tr.accepts(s.PublicKey) || m.KeyFingerprint != Fingerprint(s.PublicKey) || s.Namespace != Namespace {
			t.Fatalf("verified a manifest naming %s signed by %s in %q", m.KeyFingerprint, Fingerprint(s.PublicKey), s.Namespace)
		}
	})
}

// FuzzCompareVersions: the order install.sh reimplements is a total order
// on what parseVersion accepts.
func FuzzCompareVersions(f *testing.F) {
	f.Add("0.2.0", "0.2.0-rc.1")
	f.Add("1.10.0", "1.9.9")
	f.Add("0.0.0-a", "0.0.0-b")
	f.Add("18446744073709551615.0.0", "0.0.1")
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, err1 := CompareVersions(a, b)
		ba, err2 := CompareVersions(b, a)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("CompareVersions(%q, %q) and its reverse disagree on validity: %v / %v", a, b, err1, err2)
		}
		if err1 == nil && ab != -ba {
			t.Fatalf("CompareVersions(%q, %q)=%d but reversed %d", a, b, ab, ba)
		}
		if err1 == nil && (ab == 0) != (a == b) {
			// Two spellings of one version (leading zeros) compare equal;
			// install.sh compares the numbers too, so that is the rule.
			pa, _ := parseVersion(a)
			pb, _ := parseVersion(b)
			if pa != pb {
				t.Fatalf("CompareVersions(%q, %q)=%d for different versions", a, b, ab)
			}
		}
	})
}

// digest is the message digest an SSHSIG with hash alg signs.
func digest(alg string, msg []byte) []byte {
	if alg == "sha256" {
		d := sha256.Sum256(msg)
		return d[:]
	}
	d := sha512.Sum512(msg)
	return d[:]
}
