# kel.py: KEL §7.3 — mutations of the KEL v2 rules that must turn their tests red.
# For scripts/mutations/unitmut.py (see its header for the format). The KEL work items add their
# rows here (M1–M47); multi-repository rows say their repo.
#
# KEL-1 (ANetCore identity: replay, retirement, acceptance): M1–M7, M29, M30. The KEL-2N vectors
# (identity/testdata/vec-kel-2n.json) each break one rule of an otherwise valid KEL and record the
# exact error text, so a mutation that lets one through, or refuses it for another reason, turns
# TestKEL2NVectors red.

KEL2N = [('ANetCore:./identity', '^(TestKEL2NVectors|TestKEL2NCasesFailWithTheirText)$')]
ID_FULL = ['ANetCore:./identity']
REPLAY = 'identity/identity.go'

MUTS = [
 dict(id='kel-m1', group='KEL-1', repo='ANetCore', desc='rot 改回由前一把钥验签',
  edits=[(REPLAY, '\t\tsigner := e.Keys[0]\n',
          '\t\tsigner := e.Keys[0]\n\t\tif e.Type == Rotation {\n\t\t\tsigner = prior.CurrentKeys[0]\n\t\t}\n')],
  tests=KEL2N + [('ANetCore:./identity', '^TestDuplicitousRotByStolenPriorKey$')], full=ID_FULL),
 dict(id='kel-m2', group='KEL-1', repo='ANetCore', desc='dip 改回由当前钥验签',
  edits=[(REPLAY, '\t\tsigner := e.Keys[0]\n',
          '\t\tsigner := e.Keys[0]\n\t\tif e.Type == Deactivation {\n\t\t\tsigner = prior.CurrentKeys[0]\n\t\t}\n')],
  tests=KEL2N, full=ID_FULL),
 dict(id='kel-m3', group='KEL-1', repo='ANetCore', desc='回放接受 ixn(v0.2:当前钥签,不换钥)',
  edits=[(REPLAY, '\t\t\tif e.Type != Rotation && e.Type != Deactivation {\n',
          '\t\t\tif ixPre, _ := preimage(e); e.Type == "ixn" && ed25519.Verify(prior.CurrentKeys[0], ixPre, se.Sig) {\n'
          '\t\t\t\tprior.KeyStateSeq, prior.LastEventID = e.Seq, anetcid.MustSum(ixPre)\n'
          '\t\t\t\tstates = append(states, prior)\n\t\t\t\tcontinue\n\t\t\t}\n'
          '\t\t\tif e.Type != Rotation && e.Type != Deactivation {\n')],
  tests=KEL2N, full=ID_FULL),
 dict(id='kel-m4', group='KEL-1', repo='ANetCore', desc='退役态 msgTime == 0 放行',
  edits=[(REPLAY, '\tif msgTime == 0 {\n\t\treturn &VErr{"REVOKED_KEY", "key_state_seq retired by a later event, and the object carries no time"}\n\t}\n', '')],
  tests=[('ANetCore:./identity', '^TestRetiredKeyWithZeroMsgTime$')], full=ID_FULL),
 dict(id='kel-m5', group='KEL-1', repo='ANetCore', desc='去掉 ts 递增检查',
  edits=[(REPLAY, '\t\t\tif e.Timestamp <= lastTS {\n', '\t\t\tif false && e.Timestamp <= lastTS {\n')],
  tests=KEL2N, full=ID_FULL),
 dict(id='kel-m6', group='KEL-1', repo='ANetCore', desc='允许 rot 清空见证',
  edits=[(REPLAY, '\tcase len(e.Witnesses) == 0:\n\t\treturn errors.New("identity: rot of a toad-1 identity must name its witness")\n',
          '\tcase len(e.Witnesses) == 0:\n\t\treturn nil\n')],
  tests=KEL2N + [('ANetCore:./identity', '^TestRotCannotTruncateToWitnessPending$')], full=ID_FULL),
 dict(id='kel-m7', group='KEL-1', repo='ANetCore', desc='允许 rot 改 toad',
  edits=[(REPLAY, '\tif *e.Toad != prior.Toad {\n', '\tif false && *e.Toad != prior.Toad {\n')],
  tests=KEL2N, full=ID_FULL),
 dict(id='kel-m29', group='KEL-1', repo='ANetCore', desc='缺键 0 或标签错的事件被接受',
  edits=[(REPLAY, '\tif f.KELType != KELType {\n\t\treturn ErrKELv1\n\t}\n', ''),
         (REPLAY, '\t\tif e.KELType != KELType {\n\t\t\treturn nil, ErrKELv1\n\t\t}\n', '')],
  tests=KEL2N + [('ANetCore:./identity', '^TestKELv1IsNotAccepted$')], full=ID_FULL),
 dict(id='kel-m30', group='KEL-1', repo='ANetCore', desc='去掉事件的重编码检查',
  edits=[(REPLAY, '\tif err != nil || !bytes.Equal(back, b) {\n', '\tif err != nil || back == nil {\n')],
  tests=KEL2N + [('ANetCore:./identity', '^TestKeyEventDecodingIsStrictEverywhere$')], full=ID_FULL),
]

# KEL-3 (ANetCore identity: the recovery seed and the recovery file, KEL §2.3, §2.4, C-30–C-37).
# The KEL §7.3 table has no row of its own for these; each mutation below breaks one rule of L19,
# L20 or C-33–C-37 and must turn RECOVERY-1 (identity/testdata/vec-recovery-1.json, checked by
# TestRECOVERY1Vector and TestRECOVERY1Cases) or a unit test of identity/recovery_test.go red.

REC = 'identity/recovery.go'
RECVEC = [('ANetCore:./identity', '^(TestRECOVERY1Vector|TestRECOVERY1Cases)$')]
RECUNIT = lambda names: [('ANetCore:./identity', '^(' + '|'.join(names) + ')$')]  # noqa: E731

MUTS += [
 dict(id='kel3-1', group='KEL-3', repo='ANetCore', desc='L19:info 去掉 aid 之后的 0x00',
  edits=[(REC, '\tinfo = append(info, 0)\n', '')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-2', group='KEL-3', repo='ANetCore', desc='L19:标签从 HKDF salt 挪进 info',
  edits=[(REC, 'return hkdf.Key(sha256.New, rs[:], []byte(nextKeyLabel), string(info), ed25519.SeedSize)',
          'return hkdf.Key(sha256.New, rs[:], nil, nextKeyLabel+string(info), ed25519.SeedSize)')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-3', group='KEL-3', repo='ANetCore', desc='L19 不拒含 U+0000 的 AID(CORE-E9)',
  edits=[(REC, '\tcase strings.IndexByte(aid, 0) >= 0:\n\t\treturn nil, fmt.Errorf("%w: the AID holds U+0000", ErrNextKeyInput)\n', '')],
  tests=[('ANetCore:./wire', '^TestPreimageRejectsNUL$')] + RECUNIT(['TestRecoverySeedDerivation', 'TestRECOVERY1Cases']),
  full=ID_FULL + ['ANetCore:./wire']),
 dict(id='kel3-4', group='KEL-3', repo='ANetCore', desc='L20:aad 不含 kdf(两种模式可互相冒充)',
  edits=[(REC, '\tc.CT = nil\n', '\tc.CT = nil\n\tc.KDF = recoveryKDF{}\n')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-5', group='KEL-3', repo='ANetCore', desc='口令不做 NFC',
  edits=[(REC, 'func nfc(p []byte) []byte { return norm.NFC.Append(nil, p...) }',
          'func nfc(p []byte) []byte { _ = norm.NFC; return append([]byte(nil), p...) }')],
  tests=RECUNIT(['TestPassphraseNFC', 'TestPassphraseLengthBoundary']), full=ID_FULL),
 dict(id='kel3-6', group='KEL-3', repo='ANetCore', desc='口令用 NFKC 而不是 NFC',
  edits=[(REC, 'return norm.NFC.Append(nil, p...)', 'return norm.NFKC.Append(nil, p...)')],
  tests=RECUNIT(['TestPassphraseNFC']), full=ID_FULL),
 dict(id='kel3-7', group='KEL-3', repo='ANetCore', desc='口令最短改为 11 个码点',
  edits=[(REC, 'const MinPassphraseRunes = 12\n', 'const MinPassphraseRunes = 11\n')],
  tests=RECUNIT(['TestPassphraseLengthBoundary']), full=ID_FULL),
 dict(id='kel3-8', group='KEL-3', repo='ANetCore', desc='口令长度按字节而不是码点计',
  edits=[(REC, 'if c := utf8.RuneCount(n); c < MinPassphraseRunes {', 'if c := len(n); c < MinPassphraseRunes {')],
  tests=RECUNIT(['TestPassphraseLengthBoundary']), full=ID_FULL),
 dict(id='kel3-9', group='KEL-3', repo='ANetCore', desc='C-33:不比较文本头与块内的 AID',
  edits=[(REC, '\tif hAID != blob.AID || hc.Seq != blob.Commits.Seq', '\tif hAID == "\\x00" || hc.Seq != blob.Commits.Seq')],
  tests=RECVEC + RECUNIT(['TestRecoveryHeaderMustMatchTheBlock']), full=ID_FULL),
 dict(id='kel3-10', group='KEL-3', repo='ANetCore', desc='C-33:不比较文本头与块内的 commits',
  edits=[(REC, 'hc.Seq != blob.Commits.Seq || !bytes.Equal(hc.Digest[:], blob.Commits.Digest) ||', 'len(hc.Digest) == 0 ||')],
  tests=RECVEC + RECUNIT(['TestRecoveryHeaderMustMatchTheBlock']), full=ID_FULL),
 dict(id='kel3-11', group='KEL-3', repo='ANetCore', desc='不比较文本头与块内的 hub 提示',
  edits=[(REC, 'seen["hub"] && hHub != blob.Hub {', 'seen["hub"] && hHub == "\\x00" {')],
  tests=RECVEC + RECUNIT(['TestRecoveryHeaderMustMatchTheBlock']), full=ID_FULL),
 dict(id='kel3-12', group='KEL-3', repo='ANetCore', desc='C-35:OpenRecoveryFile 不拒本机钥块(应为 ErrRecoveryUnprotected)',
  edits=[(REC, '\tif f.blob.KDF.Alg != kdfArgon2id {\n\t\treturn rs, "", "", Commit{}, ErrRecoveryUnprotected\n\t}\n', '')],
  tests=RECVEC + RECUNIT(['TestRecoveryP7']), full=ID_FULL),
 dict(id='kel3-13', group='KEL-3', repo='ANetCore', desc='C-35:OpenRecoveryFileLocal 接受口令块',
  edits=[(REC, '\tif f.blob.KDF.Alg != kdfLocal {\n', '\tif false && f.blob.KDF.Alg != kdfLocal {\n')],
  tests=RECVEC + RECUNIT(['TestRecoveryP7']), full=ID_FULL),
 dict(id='kel3-14', group='KEL-3', repo='ANetCore', desc='不核对 argon2id 参数(文件可指定 t、m、p)',
  edits=[(REC, 'return k.T == argon2Time && k.MKiB == argon2MemKiB && k.P == argon2Lanes && len(k.Salt) == recoverySalt',
          'return len(k.Salt) == recoverySalt')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-15', group='KEL-3', repo='ANetCore', desc='块不要求规范编码',
  edits=[(REC, '\tif err != nil || !bytes.Equal(back, raw) {\n', '\tif err != nil || back == nil {\n')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-16', group='KEL-3', repo='ANetCore', desc='打开后不核对种子派生出文件所写的 commits',
  edits=[(REC, '\tif err != nil || !bytes.Equal(d[:], f.blob.Commits.Digest) {\n', '\tif err != nil || d == [32]byte{} {\n')],
  tests=RECVEC, full=ID_FULL),
 dict(id='kel3-17', group='KEL-3', repo='ANetCore', desc='C-35:口令块 AEAD 失败不报 ErrRecoveryPassphrase',
  edits=[(REC, '\t\tif errors.Is(err, errAEAD) {\n\t\t\terr = ErrRecoveryPassphrase\n\t\t}\n', '')],
  tests=RECVEC + RECUNIT(['TestRecoveryFileRealKDF']), full=ID_FULL),
 dict(id='kel3-18', group='KEL-3', repo='ANetCore', desc='C-34:SealRecoveryFile 不检查口令长度',
  edits=[(REC, '\tif err := CheckPassphrase(passphrase); err != nil {\n\t\treturn nil, err\n\t}\n', '')],
  tests=RECUNIT(['TestPassphraseLengthBoundary']), full=ID_FULL),
 dict(id='kel3-19', group='KEL-3', repo='ANetCore', desc='Seal 不核对 commits 由该种子派生',
  edits=[(REC, '\tif want != commits.Digest {\n', '\tif want == [32]byte{} {\n')],
  tests=RECUNIT(['TestSealRefuses']), full=ID_FULL),
 dict(id='kel3-20', group='KEL-3', repo='ANetCore', desc='C-34:每次写文件不换 nonce',
  edits=[(REC, '\tif _, err := rand.Read(nonce); err != nil {\n\t\treturn nil, err\n\t}\n\treturn sealRecoveryPassphrase(',
          '\treturn sealRecoveryPassphrase(')],
  tests=RECUNIT(['TestRecoveryFileRealKDF']), full=ID_FULL),
 dict(id='kel3-21', group='KEL-3', repo='ANet', desc='SI-38、C-35:daemon 引用 OpenRecoveryFile',
  edits=[('internal/daemon/kel3_scope_mutation.go', None,
          'package daemon\n\nimport "github.com/ANetResearch/ANetCore/identity"\n\nvar _ = identity.OpenRecoveryFile\n')],
  tests=[('ANetCore:./identity', '^TestRecoveryAPIScope$')], full=ID_FULL),
]
