package official

// Fuzz target for the official manifest (docs/notes/0033). The manifest is
// compiled in and signed, so what reaches parse has passed the release
// key; the target still holds parse to its own rules for any bytes, since
// a parse that panics or lets a malformed entry through would do so for a
// signed manifest too.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/internal/release"
)

// FuzzManifest: a manifest parses into entries that each pass validate and
// that Lookup finds by AID alone, survives a JSON round trip unchanged, and
// verifies only under the signing key in the official namespace.
func FuzzManifest(f *testing.F) {
	f.Add(manifestJSON("KEYFP"), false)
	f.Add(manifestJSON("KEYFP", toolsEntry(officialAID),
		`{"id": "anet-echo-f", "name": "anet echo (fmax)", "aid": "bechof", "hub": "http://39.107.76.243:4001", "caps": ["net.echo"]}`), true)
	f.Add(embeddedManifest, false)
	f.Add([]byte(`{"schema":"anet-official/1","seq":1,"agents":[{"aid":"x","AID":"y"}]}`), false)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		f.Fatal(err)
	}
	tr := release.Trust{Keys: []ed25519.PublicKey{pub}}
	fp := []byte(release.Fingerprint(pub))
	f.Fuzz(func(t *testing.T, raw []byte, releaseNS bool) {
		// KEYFP stands for this run's key, so a mutated manifest can still
		// name the key that signs it and reach parse through Verify.
		raw = bytes.ReplaceAll(raw, []byte("KEYFP"), fp)
		m, err := parse(raw)
		if err == nil {
			for _, e := range m.Agents {
				if e.validate() != nil {
					t.Fatalf("parsed an entry validate refuses: %+v", e)
				}
				got, ok := m.Lookup(e.AID, m.expires.Add(-1))
				if !ok || got.AID != e.AID || got.ID != e.ID {
					t.Fatalf("Lookup(%q) = %+v, %v", e.AID, got, ok)
				}
				if _, ok := m.Lookup(e.AID, m.expires); ok {
					t.Fatal("Lookup found an entry at expires_at")
				}
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			m2, err := parse(b)
			if err != nil || !reflect.DeepEqual(m, m2) {
				t.Fatalf("re-encoded manifest parses as %+v, %v; want %+v", m2, err, m)
			}
		}
		ns := release.OfficialNamespace
		if releaseNS {
			ns = release.Namespace
		}
		v, verr := Verify(raw, release.Sign(priv, raw, ns), tr)
		switch {
		case releaseNS && verr == nil:
			t.Fatal("a release-namespace signature verified an official manifest")
		case !releaseNS && err == nil && len(raw) <= MaxManifestBytes && m.KeyFingerprint == release.Fingerprint(pub) && verr != nil:
			t.Fatalf("a manifest that parses, names the key and is signed by it did not verify: %v", verr)
		case verr == nil && v.KeyFingerprint != release.Fingerprint(pub):
			t.Fatalf("verified a manifest naming %s", v.KeyFingerprint)
		}
	})
}
