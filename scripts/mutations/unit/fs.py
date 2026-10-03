# fs.py: forward secrecy and local retention (v0.3.0 FS §7.3) — mutation list for scripts/mutations/unitmut.py
# (see its header for the format). The numbered ids are the rows of FS §7.3; the FS packages append theirs.
# FS-1 adds 22 and fs1-a to fs1-h, which hold the ANetCore key set v2 tests to the rules they check: each
# undoes one rule of the key set (the type, the number of keys, the validity bound, the mailbox key, the
# strict decoding, the type read before the other keys, the time rule at signing) and must turn the rule's
# tests red.

SEAL = 'ANetCore:./seal'
IDENTITY = 'ANetCore:./identity'
KEYSET_TESTS = '^(TestEncKeySetV2Rejects|TestENCKEYS_2|TestSealConstantsV030|TestEncKeySetV2RefusesUnregisteredKeys|TestSignEncKeySetRefusesInvalidSet|TestVerifyEncKeySetStructure)$'

MUT22_SEALTO = '''package identity

import (
	"crypto/ed25519"
	"errors"
)

// SealTo and Open are mutation 22: the v0.2 sealed box to a peer's identity key is back.
func SealTo(recipientEdPub ed25519.PublicKey, plaintext []byte) ([]byte, error) {
	return nil, errors.New("identity: sealed box")
}

func (c *Controller) Open(ciphertext []byte) ([]byte, error) {
	return nil, errors.New("identity: sealed box")
}
'''

MUTS = [
 dict(
  id='fs-22', group='FS-C15', repo='ANetCore',
  desc='恢复 identity.SealTo(及 (*Controller).Open)',
  edits=[('identity/encrypt.go', None, MUT22_SEALTO)],
  tests=[(IDENTITY, '^TestIdentityAPIHasNoSealTo$')],
  full=[IDENTITY],
 ),
 dict(
  id='fs1-a', group='FS-C1', repo='ANetCore',
  desc='钥集类型改回 anet.enckeys/1',
  edits=[('seal/enckeys.go', 'const EncKeySetType = "anet.enckeys/2"', 'const EncKeySetType = "anet.enckeys/1"')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-b', group='FS-C2', repo='ANetCore',
  desc='MaxKeysPerSet 改为 33(接受 33 把钥)',
  edits=[('seal/limits.go', '\tMaxKeysPerSet = 32\n', '\tMaxKeysPerSet = 33\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-c', group='FS-C2', repo='ANetCore',
  desc='单把钥有效期上限改回 30 天',
  edits=[('seal/limits.go', '\tMaxKeyValidityMS = 2 * dayMS\n', '\tMaxKeyValidityMS = 30 * dayMS\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-d', group='FS-C4', repo='ANetCore',
  desc='不检查 mbox 的纪元(epoch = 0 被接受)',
  edits=[('seal/enckeys.go', '\tif s.Mbox.Epoch == 0 {\n', '\tif false {\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-e', group='FS-C4', repo='ANetCore',
  desc='不检查 mbox 公钥长度',
  edits=[('seal/enckeys.go', '\tif len(s.Mbox.Pub) != ed25519.PublicKeySize {\n', '\tif len(s.Mbox.Pub) > 1<<20 {\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-f', group='FS-C4', repo='ANetCore',
  desc='钥集用宽松解码(嵌套对象的未登记键被丢弃而不是拒收)',
  edits=[('seal/enckeys.go', '\tif err := coredet.UnmarshalStrict(b, &s); err != nil {\n\t\treturn nil, failWrap(ReasonBadKeySet, err, "decode")\n',
          '\tif err := coredet.Unmarshal(b, &s); err != nil {\n\t\treturn nil, failWrap(ReasonBadKeySet, err, "decode")\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-h', group='FS-C4', repo='ANetCore',
  desc='解码时不先看类型(v0.2 钥集报"缺 mbox"而不是"类型不符")',
  edits=[('seal/enckeys.go', '\t\tif err := coredet.Unmarshal(t, &typ); err != nil || typ != EncKeySetType {\n',
          '\t\tif err := coredet.Unmarshal(t, &typ); err != nil && false {\n')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
 dict(
  id='fs1-g', group='FS-C4', repo='ANetCore',
  desc='SignEncKeySet 不按 issued_at 检查第 5 类(签出无一把钥有效的钥集)',
  edits=[('seal/enckeys.go', '\tif err := set.checkValidAt(set.IssuedAt); err != nil {\n\t\treturn nil, err\n\t}\n', '')],
  tests=[(SEAL, KEYSET_TESTS)],
  full=[SEAL],
 ),
]
