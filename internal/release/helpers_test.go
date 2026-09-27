package release

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

// signSSHSIG is the signing half of the format (Sign), under the name the
// tests here have always used. It is checked against ssh-keygen itself in
// TestSSHKeygenAcceptsOurTestSigner, so the tests that lean on it are not
// testing the verifier against a copy of its own misunderstanding.
func signSSHSIG(priv ed25519.PrivateKey, msg []byte, namespace string) []byte {
	return Sign(priv, msg, namespace)
}

func armor(blob []byte) []byte { return armorSig(blob) }

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sshsig", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// repoFile reads a file relative to the ANet repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
