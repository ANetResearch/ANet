# fs.py: forward secrecy and local retention (v0.3.0 FS §7.3) — mutation list for scripts/mutations/unitmut.py
# (see its header for the format). The numbered ids are the rows of FS §7.3; the FS packages append theirs.
# FS-6 adds 13 and the secure_delete half of 16, and fs6-a to fs6-f, which hold its acceptance tests to the
# storage rules they check (FS-X7 to FS-X10, FS-R4). Rows 11, 14 and 15 are judged at the daemon by FS-7's
# tests; fs6-c, fs6-d and fs6-e are their storage halves.
# FS-1 adds 22 and fs1-a to fs1-h, which hold the ANetCore key set v2 tests to the rules they check: each
# undoes one rule of the key set (the type, the number of keys, the validity bound, the mailbox key, the
# strict decoding, the type read before the other keys, the time rule at signing) and must turn the rule's
# tests red.

IX = 'ANet:./internal/runtime/interactions'

MUT13_SOLO = '''package interactions

import "database/sql"

// soloDB and soloSettle are mutation 13: the key is settled in a transaction of its own, committed before
// the one that ends the task.
var soloDB *sql.DB

func soloSettle(id string, cause Cause) error {
	tx, err := soloDB.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := closeIXKey(tx, id, cause, 0); err != nil {
		return err
	}
	return tx.Commit()
}
'''

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
  id='fs-13', group='FS-X7', repo='ANet',
  desc='closeIXKey 移到提交之前的独立事务(终态写入、迟到结果、CloseIXKey 的事务回滚后钥已删)',
  edits=[
   ('internal/runtime/interactions/zz_mut13.go', None, MUT13_SOLO),
   ('internal/runtime/interactions/interactions.go', '\ts := &Store{db: db}\n', '\ts := &Store{db: db}\n\tsoloDB = db\n'),
   ('internal/runtime/interactions/peers.go',
    'func (t *Tx) Finish(id string, f Finish, cause Cause) error { return finish(t.tx, id, f, cause, t.now) }',
    'func (t *Tx) Finish(id string, f Finish, cause Cause) error {\n\tif err := soloSettle(id, cause); err != nil {\n\t\treturn err\n\t}\n\treturn finish(t.tx, id, f, cause, t.now)\n}'),
   ('internal/runtime/interactions/peers.go',
    '\treturn setLateResult(t.tx, id, result, resultCID, receipt, verified, cause, t.now)',
    '\tif err := soloSettle(id, cause); err != nil {\n\t\treturn false, err\n\t}\n\treturn setLateResult(t.tx, id, result, resultCID, receipt, verified, cause, t.now)'),
   ('internal/runtime/interactions/peers.go',
    '\treturn setState(t.tx, id, st, cause, t.now)\n',
    '\tif err := soloSettle(id, cause); err != nil {\n\t\treturn false, err\n\t}\n\treturn setState(t.tx, id, st, cause, t.now)\n'),
   ('internal/runtime/interactions/ixkey.go',
    '\treturn closeIXKey(t.tx, ix, cause, t.now)\n}',
    '\treturn soloSettle(ix, cause)\n}'),
  ],
  tests=[(IX, 'TestIXKeyDeletedOnlyWithCommit')],
  full=[IX],
 ),
 dict(
  id='fs-16a', group='FS-X8', repo='ANet',
  desc='interactions.db 不开 secure_delete(删除的私钥字节留在库文件的空闲页)',
  edits=[
   ('internal/runtime/interactions/interactions.go', '&_pragma=secure_delete(ON)', ''),
  ],
  tests=[(IX, 'TestDeletedKeyBytesAbsentFromDBFiles')],
  full=[IX],
 ),
 dict(
  id='fs6-a', group='FS-X9', repo='ANet',
  desc='close_after 清扫不看时钟门(门级 closed 时照删)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif level != ClockFull && level != ClockLocal {\n\t\treturn 0, nil\n\t}\n', ''),
  ],
  tests=[(IX, 'TestCloseAfterSweepGated')],
  full=[IX],
 ),
 dict(
  id='fs6-b', group='FS-X10', repo='ANet',
  desc='DeleteAllIXKeys 漏掉 close_after 中的钥',
  edits=[
   ('internal/runtime/interactions/ixkey.go', 'res, err := t.tx.Exec(`DELETE FROM ix_key`)',
    'res, err := t.tx.Exec(`DELETE FROM ix_key WHERE close_after = 0`)'),
  ],
  tests=[(IX, 'TestCloseInteractionsDeletesAllIXKeys')],
  full=[IX],
 ),
 dict(
  id='fs6-c', group='FS-X7', repo='ANet',
  desc='写终态时接受 CauseNotTerminal(终态路径不处置交互钥;14 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif terminal != (cause != CauseNotTerminal) {\n',
    '\tif !terminal && cause != CauseNotTerminal {\n'),
  ],
  tests=[(IX, 'TestTerminalWritesRequireCause|TestIXKeyLifecycleRandom|FuzzIXKeyLifecycle')],
  full=[IX],
 ),
 dict(
  id='fs6-d', group='FS-X7', repo='ANet',
  desc='请求方本地终态即删 E_r(15 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\t\tif !provider {\n\t\t\treturn fateCloseAfter, nil\n',
    '\t\tif !provider {\n\t\t\treturn fateDelete, nil\n'),
  ],
  tests=[(IX, 'TestIXKeyFateTable|TestIXKeyFollowsEachTerminalWrite')],
  full=[IX],
 ),
 dict(
  id='fs6-e', group='FS-R4', repo='ANet',
  desc='peer_rk 被不同的 rk 覆盖(11 的存储一半)',
  edits=[
   ('internal/runtime/interactions/ixkey.go', '\tif len(cur) > 0 {\n\t\tif RKConflict(cur, rk) {\n\t\t\treturn ErrRKMismatch\n\t\t}\n\t\treturn nil\n\t}\n', ''),
   ('internal/runtime/interactions/ixkey.go', '`UPDATE interaction SET peer_rk=? WHERE id=? AND peer_rk IS NULL`',
    '`UPDATE interaction SET peer_rk=? WHERE id=?`'),
  ],
  tests=[(IX, 'TestRecordPeerRK')],
  full=[IX],
 ),
 dict(
  id='fs6-f', group='FS-X7', repo='ANet',
  desc='只有改变了状态的终态写入才处置交互钥(本地取消后对端终态消息不删 E_r)',
  edits=[
   ('internal/runtime/interactions/interactions.go',
    '\tn, _ := res.RowsAffected()\n\tif st.IsTerminal() {\n',
    '\tn, _ := res.RowsAffected()\n\tif st.IsTerminal() && n > 0 {\n'),
  ],
  tests=[(IX, 'TestIXKeyFollowsEachTerminalWrite')],
  full=[IX],
 ),
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
