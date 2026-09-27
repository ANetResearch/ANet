package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// signSSHSIG is the signing half of the format, for tests only: it lets
// the update tests sign manifests without ssh-keygen. It is checked
// against ssh-keygen itself in TestSSHKeygenAcceptsOurTestSigner, so the
// tests that lean on it are not testing the verifier against a copy of its
// own misunderstanding.
func signSSHSIG(priv ed25519.PrivateKey, msg []byte, namespace string) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	d := sha512.Sum512(msg)
	sig := ed25519.Sign(priv, signedData(namespace, "sha512", d[:]))
	var sb bytes.Buffer
	putString(&sb, []byte(keyTypeEd))
	putString(&sb, sig)
	var b bytes.Buffer
	b.WriteString(sshsigMagic)
	b.Write([]byte{0, 0, 0, 1})
	putString(&b, keyBlob(pub))
	putString(&b, []byte(namespace))
	putString(&b, nil)
	putString(&b, []byte("sha512"))
	putString(&b, sb.Bytes())
	return armor(b.Bytes())
}

func armor(blob []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(blob)
	var out strings.Builder
	out.WriteString(sshsigBegin + "\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n" + sshsigEnd + "\n")
	return []byte(out.String())
}

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
