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
  edits=[(REPLAY, '\tif err != nil || !bytes.Equal(back, b) {\n', '\tif err != nil {\n')],
  tests=KEL2N + [('ANetCore:./identity', '^TestKeyEventDecodingIsStrictEverywhere$')], full=ID_FULL),
]
