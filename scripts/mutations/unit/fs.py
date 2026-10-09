# fs.py: forward secrecy and local retention (v0.3.0 FS §7.3) — mutation list for scripts/mutations/unitmut.py
# (see its header for the format). The numbered ids are the rows of FS §7.3; the FS packages append theirs.
# FS-6 adds 13 and the secure_delete half of 16, and fs6-a to fs6-f, which hold its acceptance tests to the
# storage rules they check (FS-X7 to FS-X10, FS-R4). Rows 11, 14 and 15 are judged at the daemon by FS-7's
# tests; fs6-c, fs6-d and fs6-e are their storage halves.
# FS-1 adds 22 and fs1-a to fs1-h, which hold the ANetCore key set v2 tests to the rules they check: each
# undoes one rule of the key set (the type, the number of keys, the validity bound, the mailbox key, the
# strict decoding, the type read before the other keys, the time rule at signing) and must turn the rule's
# tests red.
# FS-2 adds 10, 12 and 38 (the second layer in the preimage, the interaction key's (ix, peer), the zero
# pad; 38 is the edit of core2-5, judged here by SEALED-2C alone) and fs2-a to fs2-h, which hold the second
# layer's tests to the rules they check (L2 and its info, the layer-1 ack_key, the ack_key on a refusal
# after layer 1, the key a lookup answers, Opened.Layers, the signature over the second layer) and keep
# bad-pad retired in the registry; fs2-i and fs2-j hold an rk suite above 255 refused rather than
# truncated, and an interaction key printed without its private half.
# FS-3 adds 1, 2, 3 and 20 (the hub's half of 20; repo ANetHub) and fs3-a to fs3-u, which hold the hub's tests to
# the rest of FS-H1 to FS-H8, to FS-T1 at the hub and to the base64url and G1 bodies of the key set routes and
# the poll envelope (see the group at the end).

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
 dict(
  id='fs-10', group='FS-C12', repo='ANetCore',
  desc='签名原像去掉键 33、34(单层与双层同一原像:剥掉第二层、换 enc2、单层改双层都能验签)',
  edits=[('seal/fields.go',
          '\tm[preKeyIKID], m[preKeyEnc2] = cborNull, cborNull\n\tif ikid != nil {\n\t\tif m[preKeyIKID], err = encodeField(ikid); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tif m[preKeyEnc2], err = encodeField(enc2); err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t}\n',
          '')],
  tests=[(SEAL, '^TestSEALED_2C$')],
  full=[SEAL],
 ),
 dict(
  id='fs-12', group='FS-C13', repo='ANetCore',
  desc='不查交互钥的 (ix, peer)(别的交互或别的发送方封给这把钥的信也被收下)',
  edits=[('seal/envelope.go', '\tif key != nil && (inner.IX != key.IX || inner.From != key.PeerAID) {',
          '\tif false && key != nil && (inner.IX != key.IX || inner.From != key.PeerAID) {')],
  tests=[(SEAL, '^TestIKeyMismatchAcrossInteractions$')],
  full=[SEAL],
 ),
 dict(
  id='fs-38', group='FS-C8', repo='ANetCore',
  desc='Wrap 的 pad 含非零字节仍接受(与 core2-5 同一改动,只看 SEALED-2C)',
  edits=[('seal/envelope.go', '\t\tif b != 0 {\n\t\t\treturn nil, fail(ReasonBadWrap, "pad has a non-zero byte")',
          '\t\tif false && b != 0 {\n\t\t\treturn nil, fail(ReasonBadWrap, "pad has a non-zero byte")')],
  tests=[(SEAL, '^TestSEALED_2C$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-a', group='FS-C10', repo='ANetCore',
  desc='第二层 HPKE info 用第一层的标签 anet-seal/v2(L2 不起作用)',
  edits=[('seal/ixkey.go', 'return coredet.Marshal(hpkeInfo{IXLabel, to, suite, ikid})', 'return coredet.Marshal(hpkeInfo{HPKELabel, to, suite, ikid})')],
  tests=[(SEAL, '^(TestSEALED_2B|TestSecondLayerRoundTrip)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-b', group='FS-C10', repo='ANetCore',
  desc='第二层 HPKE info 不绑定收件方 AID',
  edits=[('seal/ixkey.go', 'return coredet.Marshal(hpkeInfo{IXLabel, to, suite, ikid})', 'return coredet.Marshal(hpkeInfo{IXLabel, "", suite, ikid})')],
  tests=[(SEAL, '^(TestSEALED_2B|TestSecondLayerRoundTrip)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-c', group='FS-C14', repo='ANetCore',
  desc='双层时 Seal 的 ack_key 取自第二层上下文',
  edits=[('seal/envelope.go',
          '\t\tif two, err = newLayer2Sender(inner.To, o.PeerRK); err != nil {\n\t\t\treturn nil, ack, err\n\t\t}\n',
          '\t\tif two, err = newLayer2Sender(inner.To, o.PeerRK); err != nil {\n\t\t\treturn nil, ack, err\n\t\t}\n\t\tif e2, err := two.sender.Export(DAckExporterContext, AckKeyLen); err == nil {\n\t\t\tcopy(key[:], e2)\n\t\t}\n')],
  tests=[(SEAL, '^TestSecondLayerRoundTrip$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-d', group='FS-C13', repo='ANetCore',
  desc='第二层解不开时不带 ack_key(发件方得不到令牌)',
  edits=[('seal/envelope.go', '\t\tif innerPT, err = openLayer2(outer.To, key, w); err != nil {\n\t\t\treturn nil, withAck(ack, err)',
          '\t\tif innerPT, err = openLayer2(outer.To, key, w); err != nil {\n\t\t\treturn nil, err')],
  tests=[(SEAL, '^(TestOpenOrder|TestSEALED_2C)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-e', group='FS-C13', repo='ANetCore',
  desc='查询返回的交互钥不核对 ikid(另一把钥被当作这把钥使用)',
  edits=[('seal/envelope.go', '\t\tif err := key.answers(w.ikid); err != nil {', '\t\tif err := key.answers(w.ikid); false && err != nil {')],
  tests=[(SEAL, '^TestOpenOrder$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-f', group='FS-C13', repo='ANetCore',
  desc='双层信的 Opened.Layers 仍报 1',
  edits=[('seal/envelope.go', '\t\tlayers = 2\n', '\t\tlayers = 1\n')],
  tests=[(SEAL, '^(TestSecondLayerRoundTrip|TestSEALED_2B)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-g', group='FS-C17', repo='ANetCore',
  desc='双层时 Seal 按单层原像签名(收件方按 ikid、enc2 验签失败)',
  edits=[('seal/envelope.go', 'pre, err := sigPreimage(fields, enc, recipient.KID, recipient.Suite, ikid, enc2)',
          'pre, err := sigPreimage(fields, enc, recipient.KID, recipient.Suite, nil, nil)')],
  tests=[(SEAL, '^(TestSecondLayerRoundTrip|TestIKeyMismatchAcrossInteractions)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-h', group='CORE-F3', repo='ANetCore',
  desc='bad-pad 从退役表拿回 reasons.seal(退役的码被重新启用)',
  edits=[('wire/registry.json', '      {"code": "to-mismatch"},\n      {"code": "kel-too-large"},',
          '      {"code": "to-mismatch"},\n      {"code": "bad-pad"},\n      {"code": "kel-too-large"},'),
         ('wire/registry.json', ',\n      {"code": "bad-pad", "note": "seal: the pad check of envelope v1 (inner key 21, retired); the pad is in the Wrap now, where a non-zero byte is bad-wrap"}\n', '\n')],
  tests=[('ANetCore:./wire', '^TestRegistryRetiredItemsArePinned$')],
  full=['ANetCore:./wire'],
 ),
 dict(
  id='fs2-i', group='FS-C11', repo='ANetCore',
  desc='rk 的套件号不查是否超过 255(257 截断成 uint8 的 1,被当作套件 1 收下)',
  edits=[('seal/ixkey.go', '\tif suite > 0xff {\n\t\treturn nil, fmt.Errorf("rk suite %d", suite)\n\t}\n', '')],
  tests=[(SEAL, '^(TestOpenOrder|FuzzRKDecode)$')],
  full=[SEAL],
 ),
 dict(
  id='fs2-j', group='SI-34', repo='ANetCore',
  desc='IXKey 按字段打印(私钥随日志或错误信息泄露)',
  edits=[('seal/ixkey.go', 'func (k IXKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }',
          'func (k IXKey) Format(f fmt.State, verb rune) {\n\ttype plain IXKey\n\t_, _ = fmt.Fprintf(f, fmt.FormatString(f, verb), plain(k))\n\t_ = io.EOF\n}')],
  tests=[(SEAL, '^TestGenerateIXKey$')],
  full=[SEAL],
 ),
]

# ---- FS-3: the hub's kid check, more and prev_seq (FS §7.3 rows 1, 2, 3 and 20, and the package's own;
# repo ANetHub, kept in this list so that a round's --check finds it). fs3-a to fs3-t hold the hub's tests
# to the rest of FS-H1 to FS-H8, the base64url and G1 bodies of the key set routes and the poll envelope
# (CORE-E5), and FS-T1 at the hub: fs3-q is the hub half of row 39 (L shortened in ANetCore, judged by the
# hub's TestRelayTTLBelowHardBound); the daemon half is FS-4's.
FS3_HUB = 'ANetHub:./internal/aghub'
FS3_KID_T = [(FS3_HUB, '^(TestRelayRejectsKidNotInSet|TestRandomKidsAreAllRefused|TestPreviewSubjectToKidCheck)$')]
FS3_ROLLBACK_T = [(FS3_HUB, '^(TestPublishKeysRollback409|TestRegisterRollback409|TestPublishKeysNeedsPrevSeq)$')]
FS3_POLL_T = [(FS3_HUB, '^TestPollMore$')]
FS3_FIX_T = [(FS3_HUB, '^TestWire3Fixtures$')]
MUTS += [
 dict(id='fs-1', group='FS-H1', repo='ANetHub', desc='hub 不校验 kid 是否在收件方钥集中(红队 A3)',
  edits=[('internal/aghub/relay.go', '\tif !ok {\n\t\treturn fmt.Errorf("%w: the kid is not in the recipient\'s key set seq %d", ErrKidStale, seq)\n\t}\n',
          '\tif !ok {\n\t\treturn nil\n\t}\n')],
  tests=FS3_KID_T, full=[FS3_HUB]),
 dict(id='fs-2', group='FS-H1', repo='ANetHub', desc='hub 的截止改为 not_after + D + 1h',
  edits=[('internal/aghub/relay.go', '\treturn at <= notAfter || at-notAfter <= uint64(seal.DeliveryDeadlineMS)\n',
          '\treturn at <= notAfter || at-notAfter <= uint64(seal.DeliveryDeadlineMS)+3_600_000\n')],
  tests=[(FS3_HUB, '^TestRelayRejectsAfterDeadline$')], full=[FS3_HUB]),
 dict(id='fs-3', group='FS-H1', repo='ANetHub', desc='hub 在入库事务与锁之外读钥集与时钟(发布与入库并发时收下已换掉的钥)',
  edits=[('internal/aghub/relay.go', '\tif err := s.kidAcceptable(tx, it.To, it.KID, nowMS); err != nil {\n\t\treturn Queued{}, err\n\t}\n', ''),
         ('internal/aghub/relay.go', '\ts.mu.Lock()\n\tdefer s.mu.Unlock()\n\tif enqueueLocked != nil {',
          '\tif err := s.kidAcceptable(s.db, it.To, it.KID, s.now().UnixMilli()); err != nil {\n\t\treturn Queued{}, err\n\t}\n\ts.mu.Lock()\n\tdefer s.mu.Unlock()\n\tif enqueueLocked != nil {')],
  tests=[(FS3_HUB, '^TestKidCheckInsideEnqueueTx$')], full=[FS3_HUB]),
 dict(id='fs-20', group='FS-H4', repo='ANetHub', desc='hub 不比较 prev_seq(已存更新的钥集仍被覆盖)',
  edits=[('internal/aghub/keys.go', '\tif seen.Seq > prevSeq || decision == seal.Ignore', '\tif false && seen.Seq > prevSeq || decision == seal.Ignore')],
  tests=FS3_ROLLBACK_T, full=[FS3_HUB]),
 dict(id='fs3-a', group='FS-H1', repo='ANetHub', desc='收件方没有登记钥集时照收',
  edits=[('internal/aghub/relay.go', '\t\treturn fmt.Errorf("%w: the recipient has registered no key set", ErrKidStale)\n', '\t\treturn nil\n')],
  tests=FS3_KID_T, full=[FS3_HUB]),
 dict(id='fs3-b', group='FS-H1', repo='ANetHub', desc='截止的边界差 1 毫秒(not_after + D 当刻已拒)',
  edits=[('internal/aghub/relay.go', '\treturn at <= notAfter || at-notAfter <= uint64(seal.DeliveryDeadlineMS)\n',
          '\treturn at <= notAfter || at-notAfter < uint64(seal.DeliveryDeadlineMS)\n')],
  tests=[(FS3_HUB, '^TestRelayRejectsAfterDeadline$')], full=[FS3_HUB]),
 dict(id='fs3-c', group='FS-H1', repo='ANetHub', desc='解码钥集的缓存不按 seq 与字节失效(收件方换钥后旧 kid 仍被收下)',
  edits=[('internal/aghub/relay.go', '\tif e := c.entries[aid]; e != nil && e.seq == seq && bytes.Equal(e.raw, raw) {',
          '\tif e := c.entries[aid]; e != nil && bytes.Equal(e.raw, e.raw) {')],
  tests=[(FS3_HUB, '^(TestKidCacheFollowsTheStoredSet|TestRelayRejectsKidNotInSet)$')], full=[FS3_HUB]),
 dict(id='fs3-d', group='FS-H3', repo='ANetHub', desc='来源 hub 把 home 的 409 kid_stale 改写为 502(红队 A4)',
  edits=[('internal/aghub/fed_relay.go', '\twire3.ReasonAccessInvalid: true, wire3.ReasonKidStale: true,', '\twire3.ReasonAccessInvalid: true,')],
  tests=[(FS3_HUB, '^TestFedForwardPassesKidStale$')], full=[FS3_HUB]),
 dict(id='fs3-e', group='FS-H4', repo='ANetHub', desc='prev_seq 缺失时当作 0(不回 400)',
  edits=[('internal/aghub/keys.go', '\tif req.Keys == "" || req.PrevSeq == nil {', '\tif req.PrevSeq == nil {\n\t\treq.PrevSeq = new(uint64)\n\t}\n\tif req.Keys == "" {')],
  tests=FS3_ROLLBACK_T, full=[FS3_HUB]),
 dict(id='fs3-f', group='FS-H4', repo='ANetHub', desc='/register 带 enc_keys 而缺 prev_seq 时当作 0(PAY-A1 第 3 步)',
  edits=[('internal/aghub/server.go', '\t\tif req.PrevSeq == nil {\n\t\t\twire3.Write(', '\t\tif req.PrevSeq == nil {\n\t\t\treq.PrevSeq = new(uint64)\n\t\t}\n\t\tif false {\n\t\t\twire3.Write(')],
  tests=FS3_ROLLBACK_T, full=[FS3_HUB]),
 dict(id='fs3-g', group='FS-H4', repo='ANetHub', desc='/register 的 prev_seq 比较放到写入之后(被拒的注册已改了资料)',
  edits=[('internal/aghub/server.go', 's.store.KeysRollbackOf(req.AID, encKeys, *req.PrevSeq); errors.As(err, &rb) {', 'error(nil); errors.As(err, &rb) {')],
  tests=FS3_ROLLBACK_T, full=[FS3_HUB]),
 dict(id='fs3-h', group='FS-H4', repo='ANetHub', desc='重发已存的同一钥集时也比较 prev_seq(丢了应答的重发得 409)',
  edits=[('internal/aghub/keys.go', '\tif seen == nil || decision == seal.Same {', '\tif seen == nil {')],
  tests=FS3_ROLLBACK_T, full=[FS3_HUB]),
 dict(id='fs3-i', group='FS-H5', repo='ANetHub', desc='按字节预算截断的页报 more = false',
  edits=[('internal/aghub/relay.go', '\t\t\tmore = true\n', '\t\t\tmore = len(msgs) == limit\n')],
  tests=FS3_POLL_T, full=[FS3_HUB]),
 dict(id='fs3-j', group='FS-H5', repo='ANetHub', desc='poll 返回并计入外层 exp 已过的行(只剩过期行时 more = true)',
  edits=[('internal/aghub/relay.go', '(exp IS NULL OR exp>?)', '(exp IS NULL OR exp>? OR 1)', 2)],
  tests=FS3_POLL_T, full=[FS3_HUB]),
 dict(id='fs3-k', group='FS-H5', repo='ANetHub', desc='一页的条数不封顶(超过一次 ack 能清的 1000 条)',
  edits=[('internal/aghub/relay.go', '\tif limit > MaxPollLimit {\n\t\tlimit = MaxPollLimit\n\t}\n', '')],
  tests=FS3_POLL_T, full=[FS3_HUB]),
 dict(id='fs3-l', group='CORE-E5', repo='ANetHub', desc='poll 的 envelope 用标准 base64',
  edits=[('internal/aghub/server.go', 'Envelope: wire3.EncodeB64URL(m.Payload)', 'Envelope: base64.StdEncoding.EncodeToString(m.Payload)')],
  tests=FS3_POLL_T, full=[FS3_HUB]),
 dict(id='fs3-m', group='FS-H5', repo='ANetHub', desc='poll 的 accepted_at 取应答时刻而不是受理时刻',
  edits=[('internal/aghub/server.go', 'AcceptedAt: uint64(m.CreatedAt)', 'AcceptedAt: uint64(time.Now().UnixMilli())')],
  tests=FS3_POLL_T, full=[FS3_HUB]),
 dict(id='fs3-n', group='CORE-E5', repo='ANetHub', desc='GET /agents/{aid}/keys 用标准 base64 编码 keys',
  edits=[('internal/aghub/keys.go', '\t"bytes"\n\t"database/sql"\n', '\t"bytes"\n\t"database/sql"\n\t"encoding/base64"\n'),
         ('internal/aghub/keys.go', 'Keys: wire3.EncodeB64URL(keyset)', 'Keys: base64.StdEncoding.EncodeToString(keyset)')],
  tests=FS3_FIX_T, full=[FS3_HUB]),
 dict(id='fs3-o', group='CORE-E5', repo='ANetHub', desc='POST /agents/{aid}/keys 收标准 base64 的钥集(hb64-keys)',
  edits=[('internal/aghub/keys.go', '\t"bytes"\n\t"database/sql"\n', '\t"bytes"\n\t"database/sql"\n\t"encoding/base64"\n'),
         ('internal/aghub/keys.go', 'raw, err := wire3.DecodeB64URL("keys", req.Keys)', 'raw, err := base64.StdEncoding.DecodeString(req.Keys)')],
  tests=FS3_FIX_T, full=[FS3_HUB]),
 dict(id='fs3-p', group='CORE-E5', repo='ANetHub', desc='/register 收标准 base64 的 enc_keys',
  edits=[('internal/aghub/server.go', 'if encKeys, err = wire3.DecodeB64URL("enc_keys", req.EncKeys); err != nil {',
          'if encKeys, err = base64.StdEncoding.DecodeString(req.EncKeys); err != nil {')],
  tests=FS3_FIX_T, full=[FS3_HUB]),
 dict(id='fs3-q', group='FS-T1', repo='ANetCore', desc='L 改为 14 天(hub 一侧:信封在 hub 存满 TTL 后钥已到硬界)',
  edits=[('seal/limits.go', '\tMaxMessageLifetimeMS = 15 * dayMS', '\tMaxMessageLifetimeMS = 14 * dayMS')],
  tests=[(FS3_HUB, '^TestRelayTTLBelowHardBound$')], full=[FS3_HUB]),
 dict(id='fs3-r', group='FS-T1', repo='ANetHub', desc='-relay-ttl 可设得比 L − D − 时钟容差更长',
  edits=[('internal/aghub/limits.go', '\tcase l.UndeliveredTTL > MaxUndeliveredTTL:', '\tcase false && l.UndeliveredTTL > MaxUndeliveredTTL:')],
  tests=[(FS3_HUB, '^TestRelayTTLBelowHardBound$')], full=[FS3_HUB]),
 dict(id='fs3-s', group='FS-H6', repo='ANetHub', desc='relay_message 加 kid 列',
  edits=[('internal/aghub/relay.go', '\t   cap_id BLOB,\n\t   size INTEGER NOT NULL,', '\t   cap_id BLOB,\n\t   kid BLOB,\n\t   size INTEGER NOT NULL,')],
  tests=[(FS3_HUB, '^TestRelaySchemaHasNoKid$')], full=[FS3_HUB]),
 dict(id='fs3-t', group='FS-H8', repo='ANetHub', desc='中间件去掉 HTTP Date 头',
  edits=[('internal/wire3/wire3.go', '\t\tw.Header().Set(Header, versionText)\n\t\tswitch declaredWire(r) {',
          '\t\tw.Header().Set(Header, versionText)\n\t\tw.Header()["Date"] = nil\n\t\tswitch declaredWire(r) {')],
  tests=[(FS3_HUB, '^TestHubResponsesCarryDate$')], full=[FS3_HUB]),
 dict(id='fs3-u', group='G1', repo='ANetHub', desc='GET /agents/{aid}/keys 对未在本 hub 注册的 AID 回 not_found 而不是 unknown_aid',
  edits=[('internal/aghub/keys.go', '\t\twire3.Write(w, wire3.Refusal{Reason: wire3.ReasonUnknownAID,', '\t\twire3.Write(w, wire3.Refusal{Reason: wire3.ReasonNotFound,')],
  tests=FS3_FIX_T + [(FS3_HUB, '^TestTheWire2RoutesAreServed$')], full=[FS3_HUB]),
]
