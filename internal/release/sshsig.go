package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// The SSHSIG format, as `ssh-keygen -Y sign` writes it (OpenSSH
// PROTOCOL.sshsig). Implemented here with the standard library rather than
// golang.org/x/crypto/ssh: the verifier needs about a hundred lines of it,
// and x/crypto/ssh is not otherwise linked into the anet binary. Only
// Ed25519 keys are accepted, because the release key is one and a verifier
// that accepts more than the signer uses is a verifier with more to get
// wrong.
//
//	byte[6]  "SSHSIG"
//	uint32   1
//	string   publickey        ("ssh-ed25519", 32-byte key)
//	string   namespace
//	string   reserved         (empty)
//	string   hash_algorithm   ("sha512" or "sha256")
//	string   signature        ("ssh-ed25519", 64-byte signature)
//
// What is signed is not the message but a second blob that repeats the
// namespace and carries the message's hash:
//
//	byte[6]  "SSHSIG"
//	string   namespace
//	string   reserved
//	string   hash_algorithm
//	string   H(message)
//
// The namespace is what keeps a signature the release key made for one
// purpose from being accepted for another; see Namespace.

const (
	sshsigMagic   = "SSHSIG"
	sshsigVersion = 1
	sshsigBegin   = "-----BEGIN SSH SIGNATURE-----"
	sshsigEnd     = "-----END SSH SIGNATURE-----"
	keyTypeEd     = "ssh-ed25519"
)

// Signature is a parsed SSHSIG.
type Signature struct {
	PublicKey ed25519.PublicKey // the key the signature says made it; trust is a separate question
	Namespace string
	HashAlg   string
	Sig       []byte
}

// ParseSignature decodes an armored SSHSIG. It checks structure only; it
// does not verify anything. Anything it does not recognise is an error:
// a second key type, a non-empty reserved field, trailing bytes.
func ParseSignature(armored []byte) (*Signature, error) {
	blob, err := dearmor(armored)
	if err != nil {
		return nil, err
	}
	r := &reader{b: blob}
	if magic := r.raw(len(sshsigMagic)); string(magic) != sshsigMagic {
		return nil, errors.New("sshsig: bad magic")
	}
	ver := r.uint32()
	pubBlob := r.str()
	ns := r.str()
	reserved := r.str()
	hashAlg := r.str()
	sigBlob := r.str()
	if r.err != nil {
		return nil, fmt.Errorf("sshsig: truncated: %w", r.err)
	}
	if ver != sshsigVersion {
		return nil, fmt.Errorf("sshsig: unsupported version %d", ver)
	}
	if len(r.b) != 0 {
		return nil, fmt.Errorf("sshsig: %d trailing bytes", len(r.b))
	}
	if len(reserved) != 0 {
		return nil, errors.New("sshsig: reserved field is not empty")
	}
	pub, err := parseKeyBlob(pubBlob)
	if err != nil {
		return nil, err
	}
	sig, err := parseSigBlob(sigBlob)
	if err != nil {
		return nil, err
	}
	switch string(hashAlg) {
	case "sha512", "sha256":
	default:
		return nil, fmt.Errorf("sshsig: unsupported hash algorithm %q", hashAlg)
	}
	if len(ns) == 0 {
		return nil, errors.New("sshsig: empty namespace")
	}
	return &Signature{PublicKey: pub, Namespace: string(ns), HashAlg: string(hashAlg), Sig: sig}, nil
}

// Verify checks that s is a signature by s.PublicKey over msg in the
// given namespace. Whether s.PublicKey is a key anyone should believe is
// the caller's question — see Trust.Verify, which is the entry point that
// asks it.
func (s *Signature) Verify(msg []byte, namespace string) error {
	if namespace == "" {
		return errors.New("sshsig: empty namespace requested")
	}
	if s.Namespace != namespace {
		return fmt.Errorf("sshsig: signature is for namespace %q, not %q", s.Namespace, namespace)
	}
	var h []byte
	switch s.HashAlg {
	case "sha512":
		d := sha512.Sum512(msg)
		h = d[:]
	case "sha256":
		d := sha256.Sum256(msg)
		h = d[:]
	default:
		return fmt.Errorf("sshsig: unsupported hash algorithm %q", s.HashAlg)
	}
	if len(s.PublicKey) != ed25519.PublicKeySize || len(s.Sig) != ed25519.SignatureSize {
		return errors.New("sshsig: malformed key or signature")
	}
	if !ed25519.Verify(s.PublicKey, signedData(s.Namespace, s.HashAlg, h), s.Sig) {
		return errors.New("sshsig: signature does not verify")
	}
	return nil
}

// Sign makes an armored SSHSIG over msg in namespace, with SHA-512, the
// form `ssh-keygen -Y sign` writes. Nothing in the anet binary calls it:
// releases and the official manifest are signed with ssh-keygen
// (deploy/release/build-release.sh). It is here for tests — this
// package's and internal/official's — which sign with keys they generate,
// and it is checked against ssh-keygen itself
// (TestSSHKeygenAcceptsOurTestSigner).
func Sign(priv ed25519.PrivateKey, msg []byte, namespace string) []byte {
	pub := priv.Public().(ed25519.PublicKey)
	d := sha512.Sum512(msg)
	sig := ed25519.Sign(priv, signedData(namespace, "sha512", d[:]))
	var sb bytes.Buffer
	putString(&sb, []byte(keyTypeEd))
	putString(&sb, sig)
	var b bytes.Buffer
	b.WriteString(sshsigMagic)
	putUint32(&b, sshsigVersion)
	putString(&b, keyBlob(pub))
	putString(&b, []byte(namespace))
	putString(&b, nil)
	putString(&b, []byte("sha512"))
	putString(&b, sb.Bytes())
	return armorSig(b.Bytes())
}

// armorSig wraps an SSHSIG blob the way ssh-keygen does: 70 columns of
// base64 between the BEGIN and END lines.
func armorSig(blob []byte) []byte {
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

// signedData is the blob the signer actually signs.
func signedData(namespace, hashAlg string, digest []byte) []byte {
	var b bytes.Buffer
	b.WriteString(sshsigMagic)
	putString(&b, []byte(namespace))
	putString(&b, nil) // reserved
	putString(&b, []byte(hashAlg))
	putString(&b, digest)
	return b.Bytes()
}

// ParseAuthorizedKey reads one Ed25519 public key in the one-line OpenSSH
// form, "ssh-ed25519 AAAA… [comment]" — the form of a .pub file and of the
// key part of an allowed_signers line.
func ParseAuthorizedKey(line string) (ed25519.PublicKey, error) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return nil, errors.New("public key: expected \"ssh-ed25519 <base64> [comment]\"")
	}
	if f[0] != keyTypeEd {
		return nil, fmt.Errorf("public key: type %q, want %s", f[0], keyTypeEd)
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	return parseKeyBlob(blob)
}

// Fingerprint is the key's fingerprint as `ssh-keygen -l` prints it:
// "SHA256:" and the unpadded base64 of the SHA-256 of the wire-format key.
func Fingerprint(pub ed25519.PublicKey) string {
	d := sha256.Sum256(keyBlob(pub))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(d[:])
}

// AuthorizedKey renders pub in the one-line OpenSSH form, without comment.
func AuthorizedKey(pub ed25519.PublicKey) string {
	return keyTypeEd + " " + base64.StdEncoding.EncodeToString(keyBlob(pub))
}

func keyBlob(pub ed25519.PublicKey) []byte {
	var b bytes.Buffer
	putString(&b, []byte(keyTypeEd))
	putString(&b, pub)
	return b.Bytes()
}

func parseKeyBlob(blob []byte) (ed25519.PublicKey, error) {
	r := &reader{b: blob}
	typ := r.str()
	key := r.str()
	if r.err != nil || len(r.b) != 0 {
		return nil, errors.New("public key: malformed blob")
	}
	if string(typ) != keyTypeEd {
		return nil, fmt.Errorf("public key: type %q, want %s", typ, keyTypeEd)
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key: %d bytes, want %d", len(key), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(append([]byte(nil), key...)), nil
}

func parseSigBlob(blob []byte) ([]byte, error) {
	r := &reader{b: blob}
	typ := r.str()
	sig := r.str()
	if r.err != nil || len(r.b) != 0 {
		return nil, errors.New("sshsig: malformed signature blob")
	}
	if string(typ) != keyTypeEd {
		return nil, fmt.Errorf("sshsig: signature type %q, want %s", typ, keyTypeEd)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("sshsig: signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	return append([]byte(nil), sig...), nil
}

// dearmor strips the BEGIN/END lines and decodes the base64 between them.
// Text before BEGIN or after END is refused: a .sig file is the armor and
// nothing else, and a parser that skips leading junk is how an HTML error
// page served in place of a signature gets as far as the base64 decoder.
func dearmor(in []byte) ([]byte, error) {
	s := strings.TrimSpace(strings.ReplaceAll(string(in), "\r\n", "\n"))
	if !strings.HasPrefix(s, sshsigBegin+"\n") || !strings.HasSuffix(s, "\n"+sshsigEnd) {
		return nil, errors.New("sshsig: not an armored SSH signature")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(s, sshsigBegin+"\n"), "\n"+sshsigEnd)
	if strings.Contains(body, "-----") {
		return nil, errors.New("sshsig: more than one armored block")
	}
	body = strings.Join(strings.Fields(body), "")
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("sshsig: %w", err)
	}
	return blob, nil
}

func putString(b *bytes.Buffer, s []byte) {
	putUint32(b, uint32(len(s)))
	b.Write(s)
}

func putUint32(b *bytes.Buffer, v uint32) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], v)
	b.Write(n[:])
}

// reader consumes SSH wire-format fields; the first short read sets err
// and every later read returns nil.
type reader struct {
	b   []byte
	err error
}

func (r *reader) raw(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.err = errors.New("short read")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) uint32() uint32 {
	b := r.raw(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *reader) str() []byte {
	n := r.uint32()
	if r.err != nil {
		return nil
	}
	if uint64(n) > uint64(len(r.b)) {
		r.err = errors.New("string length exceeds input")
		return nil
	}
	return r.raw(int(n))
}
