package release

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"testing"
)

// install.sh prints the release key's fingerprint as a constant ("signature
// ok (release key SHA256:...)"). It must be the fingerprint of the key the
// script verifies with, or a key rotation leaves an install reporting the
// old key as the signer.
func TestInstallPrintsTheFingerprintOfItsOwnKey(t *testing.T) {
	script := repoFile(t, "deploy/release/install.sh")
	key := regexp.MustCompile(`ssh-ed25519 (AAAA[A-Za-z0-9+/=]+)`).FindStringSubmatch(script)
	fp := regexp.MustCompile(`release_key_fingerprint\(\) \{\n\s*echo '(SHA256:[^']+)'`).FindStringSubmatch(script)
	if key == nil || fp == nil {
		t.Fatal("install.sh: no release key or fingerprint found")
	}
	blob, err := base64.StdEncoding.DecodeString(key[1])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(blob)
	if want := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]); fp[1] != want {
		t.Errorf("install.sh prints %s as the release key, the key it verifies with is %s", fp[1], want)
	}
}
