package release

import (
	"crypto/ed25519"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// Namespace is the SSHSIG namespace every release signature is made in.
//
// `ssh-keygen -Y sign -n anet-release@agentnetwork.org.cn`. A signature
// carries its namespace inside what is signed, so a file the release key
// signed for any other purpose — a git commit, a login challenge — does
// not verify as a release manifest, and the reverse.
const Namespace = "anet-release@agentnetwork.org.cn"

// Identity is the principal the release key is listed under in
// allowed_signers, and what `ssh-keygen -Y verify -I` is given.
const Identity = "anet-release@agentnetwork.org.cn"

// allowedSigners is the release public key in the allowed_signers format
// `ssh-keygen -Y verify -f` reads. The same line is embedded in
// deploy/release/install.sh and printed in SECURITY.md, README.md and
// docs/GUIDE-zh.md; keys_test.go fails when any of them drifts from this
// file.
//
// DEV KEY — 正式发布前由产品负责人替换. The private half lives outside
// every repository (ink93:/data/projs/anet-dev/.release-dev-key). Replacing
// it means replacing this file, NextKeyFingerprint, the line in
// install.sh, SECURITY.md, README.md and docs/GUIDE-zh.md, in one commit.
//
//go:embed allowed_signers
var allowedSigners string

// NextKeyFingerprint is the fingerprint of the key the release key will be
// rotated to, committed in advance.
//
// A binary accepts a manifest signed by a key whose fingerprint equals
// this, so the release after a rotation — signed by the new key, and
// carrying the new key inside the SSHSIG — still updates binaries that
// only knew the old one. The public half is not needed in advance: the
// signature brings it, and the fingerprint is what was promised. The
// manifest repeats the value as next_key_fingerprint so the commitment is
// public before it is used. Empty means no rotation is committed.
//
// DEV KEY — 正式发布前由产品负责人替换
// (ink93:/data/projs/anet-dev/.release-dev-key-next).
const NextKeyFingerprint = "SHA256:Vqbc5UDOJ7cR1ik5Vmn8NecV66MjpP9OteJ6JFkkhpU"

// Trust is the set of keys a release signature may come from.
type Trust struct {
	Keys            []ed25519.PublicKey
	NextFingerprint string
}

// DefaultTrust is the trust built into this binary.
func DefaultTrust() Trust {
	keys, err := ParseAllowedSigners(allowedSigners)
	if err != nil {
		// The file is compiled in and pinned by a test; a binary that
		// cannot read its own release key must not pretend to verify.
		panic("release: embedded allowed_signers: " + err.Error())
	}
	return Trust{Keys: keys, NextFingerprint: NextKeyFingerprint}
}

// AllowedSigners returns the embedded allowed_signers text.
func AllowedSigners() string { return allowedSigners }

// Verify checks sig over msg: well-formed, in Namespace, by a key this
// trust accepts. It returns the parsed signature so a caller can report
// which key signed.
func (t Trust) Verify(msg, armoredSig []byte) (*Signature, error) {
	s, err := ParseSignature(armoredSig)
	if err != nil {
		return nil, err
	}
	if !t.accepts(s.PublicKey) {
		return nil, fmt.Errorf("signed by %s, which is not the release key (%s)",
			Fingerprint(s.PublicKey), t.describe())
	}
	if err := s.Verify(msg, Namespace); err != nil {
		return nil, err
	}
	return s, nil
}

func (t Trust) accepts(pub ed25519.PublicKey) bool {
	for _, k := range t.Keys {
		if k.Equal(pub) {
			return true
		}
	}
	return t.NextFingerprint != "" && Fingerprint(pub) == t.NextFingerprint
}

func (t Trust) describe() string {
	var fps []string
	for _, k := range t.Keys {
		fps = append(fps, Fingerprint(k))
	}
	if t.NextFingerprint != "" {
		fps = append(fps, "next "+t.NextFingerprint)
	}
	return strings.Join(fps, ", ")
}

// ParseAllowedSigners reads the keys listed for Identity, in Namespace,
// from an allowed_signers file:
//
//	anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAA…
//
// Only the options this project writes are understood. A line for another
// principal, or restricted to namespaces that exclude Namespace, is
// skipped the way ssh-keygen would skip it; an option this parser does not
// know is an error rather than something to ignore, because ignoring a
// restriction widens what the key is trusted for.
func ParseAllowedSigners(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("allowed_signers line %d: too few fields", n+1)
		}
		principals, rest := f[0], f[1:]
		nsOK := true
		if !strings.HasPrefix(rest[0], "ssh-") {
			opts := rest[0]
			rest = rest[1:]
			for _, o := range splitOptions(opts) {
				name, val, _ := strings.Cut(o, "=")
				switch strings.ToLower(name) {
				case "namespaces":
					nsOK = false
					for _, ns := range strings.Split(strings.Trim(val, `"`), ",") {
						if ns == Namespace {
							nsOK = true
						}
					}
				default:
					return nil, fmt.Errorf("allowed_signers line %d: unsupported option %q", n+1, name)
				}
			}
		}
		if len(rest) < 2 {
			return nil, fmt.Errorf("allowed_signers line %d: no key", n+1)
		}
		pub, err := ParseAuthorizedKey(strings.Join(rest, " "))
		if err != nil {
			return nil, fmt.Errorf("allowed_signers line %d: %w", n+1, err)
		}
		listed := false
		for _, p := range strings.Split(principals, ",") {
			if p == Identity {
				listed = true
			}
		}
		if listed && nsOK {
			keys = append(keys, pub)
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("allowed_signers: no key for " + Identity + " in namespace " + Namespace)
	}
	return keys, nil
}

// splitOptions splits an allowed_signers option list on the commas that
// are not inside double quotes: namespaces="a,b",valid-after="…".
func splitOptions(s string) []string {
	var out []string
	quoted, start := false, 0
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
