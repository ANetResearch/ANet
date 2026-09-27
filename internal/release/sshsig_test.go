package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The vectors in testdata/sshsig were made by OpenSSH 9.6 ssh-keygen with
// two throwaway Ed25519 keys whose private halves were deleted afterwards:
//
//	ssh-keygen -Y sign -f signer -n anet-release@agentnetwork.org.cn release.json
//	ssh-keygen -Y sign -f signer -n anet-release@agentnetwork.org.cn -O hashalg=sha256 …  → release.json.sha256.sig
//	ssh-keygen -Y sign -f signer -n file …                                                → release.json.wrongns.sig
//	ssh-keygen -Y sign -f other  -n anet-release@agentnetwork.org.cn …                    → release.json.otherkey.sig
//
// and each good one checked with `ssh-keygen -Y verify -f allowed_signers`.
// They pin this verifier to the reference implementation without needing
// ssh-keygen at test time. The fingerprints below are what `ssh-keygen -l`
// printed for the two keys.
const (
	vectorSignerFP = "SHA256:yAncyqgBxlTTHfkekTWkNt1XLIJHT3davqjYxLY/zm0"
	vectorOtherFP  = "SHA256:iBzYV5OP99baXqb1rbK0Y+jj/xmMJuLvDnYGYPhs7Ms"
)

func vectorTrust(t *testing.T) Trust {
	t.Helper()
	keys, err := ParseAllowedSigners(string(readVector(t, "allowed_signers")))
	if err != nil {
		t.Fatal(err)
	}
	return Trust{Keys: keys}
}

func TestSSHKeygenVectorsVerify(t *testing.T) {
	msg := readVector(t, "release.json")
	tr := vectorTrust(t)
	for _, name := range []string{"release.json.sig", "release.json.sha256.sig"} {
		s, err := tr.Verify(msg, readVector(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fp := Fingerprint(s.PublicKey); fp != vectorSignerFP {
			t.Errorf("%s: signer %s, want %s", name, fp, vectorSignerFP)
		}
		if s.Namespace != Namespace {
			t.Errorf("%s: namespace %q", name, s.Namespace)
		}
	}
}

func TestFingerprintsMatchSSHKeygen(t *testing.T) {
	for file, want := range map[string]string{"signer_key.pub": vectorSignerFP, "other_key.pub": vectorOtherFP} {
		pub, err := ParseAuthorizedKey(string(readVector(t, file)))
		if err != nil {
			t.Fatal(err)
		}
		if got := Fingerprint(pub); got != want {
			t.Errorf("%s: %s, want %s", file, got, want)
		}
		if got := AuthorizedKey(pub); got != strings.TrimSpace(string(readVector(t, file))) {
			t.Errorf("%s: renders as %q", file, got)
		}
	}
}

func TestSSHKeygenVectorsRejected(t *testing.T) {
	msg := readVector(t, "release.json")
	tr := vectorTrust(t)

	if _, err := tr.Verify(msg, readVector(t, "release.json.wrongns.sig")); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Errorf("a signature made in namespace \"file\" was accepted as a release signature: %v", err)
	}
	if _, err := tr.Verify(msg, readVector(t, "release.json.otherkey.sig")); err == nil ||
		!strings.Contains(err.Error(), "not the release key") {
		t.Errorf("a signature by another key was accepted: %v", err)
	}

	tampered := bytes.Replace(msg, []byte(`"version": "0.2.0"`), []byte(`"version": "0.2.1"`), 1)
	if bytes.Equal(tampered, msg) {
		t.Fatal("test vector changed shape")
	}
	if _, err := tr.Verify(tampered, readVector(t, "release.json.sig")); err == nil {
		t.Error("a changed manifest verified under the original signature")
	}
	if _, err := tr.Verify(append(append([]byte{}, msg...), '\n'), readVector(t, "release.json.sig")); err == nil {
		t.Error("a manifest with one extra byte verified")
	}
}

// The pre-committed next key: a manifest signed by a key the binary has
// never seen is accepted when, and only when, its fingerprint is the one
// committed in advance.
func TestNextKeyFingerprintAcceptsTheRotatedKey(t *testing.T) {
	msg := readVector(t, "release.json")
	sig := readVector(t, "release.json.otherkey.sig")
	tr := vectorTrust(t)
	tr.NextFingerprint = vectorOtherFP
	s, err := tr.Verify(msg, sig)
	if err != nil {
		t.Fatalf("the committed next key was refused: %v", err)
	}
	if Fingerprint(s.PublicKey) != vectorOtherFP {
		t.Fatal("wrong signer reported")
	}
	tr.NextFingerprint = vectorSignerFP // a different commitment
	tr.Keys = nil
	if _, err := tr.Verify(msg, sig); err == nil {
		t.Fatal("a key matching no commitment was accepted")
	}
}

// Structural mutations of a good signature: each must fail to parse or to
// verify, never pass.
func TestSignatureMutationsAreRefused(t *testing.T) {
	msg := readVector(t, "release.json")
	good := readVector(t, "release.json.sig")
	blob, err := dearmor(good)
	if err != nil {
		t.Fatal(err)
	}
	tr := vectorTrust(t)
	if _, err := tr.Verify(msg, armor(blob)); err != nil {
		t.Fatalf("re-armoring changed the signature: %v", err)
	}

	mut := func(f func(b []byte) []byte) []byte {
		return armor(f(append([]byte(nil), blob...)))
	}
	cases := map[string][]byte{
		"bad magic":         mut(func(b []byte) []byte { b[0] = 'X'; return b }),
		"version 2":         mut(func(b []byte) []byte { b[9] = 2; return b }),
		"trailing byte":     mut(func(b []byte) []byte { return append(b, 0) }),
		"truncated":         mut(func(b []byte) []byte { return b[:len(b)-1] }),
		"last sig byte":     mut(func(b []byte) []byte { b[len(b)-1] ^= 1; return b }),
		"key byte":          mut(func(b []byte) []byte { b[10+4+4+11+4] ^= 1; return b }),
		"html page":         []byte("<!doctype html><html>not found</html>"),
		"no armor":          []byte(base64.StdEncoding.EncodeToString(blob)),
		"junk before armor": append([]byte("x\n"), good...),
		"two blocks":        append(append([]byte{}, good...), good...),
		"empty":             nil,
	}
	for name, sig := range cases {
		if _, err := tr.Verify(msg, sig); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReservedAndHashAlgorithmAreChecked(t *testing.T) {
	pub, priv := newKey(t)
	tr := Trust{Keys: []ed25519.PublicKey{pub}}
	msg := []byte("m")
	build := func(reserved, hashAlg string) []byte {
		var b bytes.Buffer
		b.WriteString(sshsigMagic)
		b.Write([]byte{0, 0, 0, 1})
		putString(&b, keyBlob(pub))
		putString(&b, []byte(Namespace))
		putString(&b, []byte(reserved))
		putString(&b, []byte(hashAlg))
		var sb bytes.Buffer
		putString(&sb, []byte(keyTypeEd))
		putString(&sb, ed25519.Sign(priv, []byte("whatever")))
		putString(&b, sb.Bytes())
		return armor(b.Bytes())
	}
	if _, err := tr.Verify(msg, build("x", "sha512")); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("non-empty reserved: %v", err)
	}
	if _, err := tr.Verify(msg, build("", "md5")); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("md5: %v", err)
	}
	if _, err := tr.Verify(msg, signSSHSIG(priv, msg, Namespace)); err != nil {
		t.Errorf("the test signer's own signature: %v", err)
	}
}

func TestParseAllowedSigners(t *testing.T) {
	k1 := strings.TrimSpace(string(readVector(t, "signer_key.pub")))
	k2 := strings.TrimSpace(string(readVector(t, "other_key.pub")))
	text := strings.Join([]string{
		"# comment",
		"",
		"someone@else " + k2, // another principal: skipped
		Identity + ` namespaces="git,file" ` + k2,                     // excludes our namespace: skipped
		"a@b," + Identity + ` namespaces="x,` + Namespace + `" ` + k1, // listed
	}, "\n")
	keys, err := ParseAllowedSigners(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || Fingerprint(keys[0]) != vectorSignerFP {
		t.Fatalf("got %d keys", len(keys))
	}
	if _, err := ParseAllowedSigners(Identity + ` cert-authority ` + k1); err == nil {
		t.Error("an option this parser does not understand was ignored")
	}
	if _, err := ParseAllowedSigners("someone@else " + k1); err == nil {
		t.Error("a file with no key for the release identity was accepted")
	}
}

// Live cross-check against ssh-keygen, both directions: what it signs we
// verify, and what our test signer signs it verifies. Skipped where
// ssh-keygen is not installed; the committed vectors cover the first
// direction without it.
func TestAgainstLiveSSHKeygen(t *testing.T) {
	kg, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if out, err := exec.Command(kg, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	pubLine, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParseAuthorizedKey(string(pubLine))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(kg, "-l", "-f", key+".pub").Output()
	if err != nil {
		t.Fatal(err)
	}
	if f := strings.Fields(string(out)); len(f) < 2 || f[1] != Fingerprint(pub) {
		t.Errorf("ssh-keygen -l says %q, Fingerprint says %s", out, Fingerprint(pub))
	}

	msg := make([]byte, 4096)
	rand.Read(msg)
	file := filepath.Join(dir, "msg")
	if err := os.WriteFile(file, msg, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := Trust{Keys: []ed25519.PublicKey{pub}}
	for _, ns := range []string{Namespace, "file"} {
		os.Remove(file + ".sig")
		if out, err := exec.Command(kg, "-Y", "sign", "-f", key, "-n", ns, file).CombinedOutput(); err != nil {
			t.Fatalf("sign: %v %s", err, out)
		}
		sig, err := os.ReadFile(file + ".sig")
		if err != nil {
			t.Fatal(err)
		}
		_, err = tr.Verify(msg, sig)
		if ns == Namespace && err != nil {
			t.Errorf("ssh-keygen's signature did not verify: %v", err)
		}
		if ns != Namespace && err == nil {
			t.Errorf("a signature in namespace %q verified as a release signature", ns)
		}
	}
}

func TestSSHKeygenAcceptsOurTestSigner(t *testing.T) {
	kg, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	pub, priv := newKey(t)
	dir := t.TempDir()
	msg := []byte(`{"schema": "anet-release/1"}` + "\n")
	allowed := Identity + ` namespaces="` + Namespace + `" ` + AuthorizedKey(pub) + "\n"
	for name, b := range map[string][]byte{"msg": msg, "msg.sig": signSSHSIG(priv, msg, Namespace), "allowed": []byte(allowed)} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(kg, "-Y", "verify", "-f", filepath.Join(dir, "allowed"), "-I", Identity,
		"-n", Namespace, "-s", filepath.Join(dir, "msg.sig"))
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen refused the test signer's signature: %v\n%s", err, out)
	}
}

// The two namespaces the release key signs in do not cross: a release
// signature is not an official-manifest signature, nor the reverse, and a
// key listed for only one of them is trusted for only that one.
func TestReleaseAndOfficialNamespacesDoNotCross(t *testing.T) {
	pub, priv := newKey(t)
	tr := Trust{Keys: []ed25519.PublicKey{pub}}
	msg := []byte("m")
	if _, err := tr.VerifyIn(OfficialNamespace, msg, Sign(priv, msg, OfficialNamespace)); err != nil {
		t.Fatalf("official signature: %v", err)
	}
	if _, err := tr.VerifyIn(OfficialNamespace, msg, Sign(priv, msg, Namespace)); err == nil {
		t.Error("a release signature verified as an official one")
	}
	if _, err := tr.Verify(msg, Sign(priv, msg, OfficialNamespace)); err == nil {
		t.Error("an official signature verified as a release one")
	}

	only := func(ns string) string {
		return Identity + ` namespaces="` + ns + `" ` + AuthorizedKey(pub) + "\n"
	}
	if _, err := ParseAllowedSignersFor(only(Namespace), OfficialNamespace); err == nil {
		t.Error("a key listed for releases only was trusted for the official manifest")
	}
	if keys, err := ParseAllowedSignersFor(only(Namespace+","+OfficialNamespace), OfficialNamespace); err != nil || len(keys) != 1 {
		t.Errorf("a key listed for both: %v", err)
	}
}
