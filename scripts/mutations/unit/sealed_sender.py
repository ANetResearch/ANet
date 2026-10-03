# sealed_sender.py: sealed-sender mutations (M1-M46 and the area's own) that must turn their tests
# red. For scripts/mutations/unitmut.py (see its header for the format). The ANetCore side
# (relayauth sessions, the directory box) comes first; the daemon side is added by the packages that
# implement it, the hub side lives in ANetHub/scripts/mutations/sealed_sender.py.

SESSION_T = [('ANetCore:./relayauth', '^(TestSessionMACBindsEveryInput|TestSessionVectorMAC)$')]
DIRBOX_T = [('ANetCore:./dirbox', '^(TestDirVector|TestBoxHidesTheEntry)$')]
NUL_T = [('ANetCore:./wire', '^TestPreimageRejectsNUL$')]
REGISTRY_T = [('ANetCore:./wire', '^TestWireRegistry$'), ('ANetCore:./relayauth', '^TestEveryActionConstantIsValid$')]

MUTS = [
 # ---- M16: the session MAC must cover the body digest and X-ANet-Home
 dict(id='ss-m16-body', group='SS-M16', repo='ANetCore', desc='会话 MAC 不覆盖体摘要(换体的请求被接受)',
  edits=[('relayauth/session.go', '\treturn append(in, digest[:]...), nil', '\t_ = digest\n\treturn in, nil')],
  tests=SESSION_T, full=['ANetCore:./relayauth']),
 dict(id='ss-m16-home', group='SS-M16', repo='ANetCore', desc='会话 MAC 不覆盖 X-ANet-Home(改提示的请求被接受)',
  edits=[('relayauth/session.go', '\tin = append(in, home...)\n', '')],
  tests=SESSION_T, full=['ANetCore:./relayauth']),
 # ---- M20: the directory box must encrypt, and h must carry its label
 dict(id='ss-m20-plain', group='SS-M20', repo='ANetCore', desc='目录 box 不加密(明文加零标签)',
  edits=[('dirbox/dirbox.go', '\treturn a.Seal(box, box[:NonceLen], pt, h), nil',
          '\t_, _ = a, h\n\treturn append(append(box, pt...), make([]byte, 16)...), nil')],
  tests=DIRBOX_T, full=['ANetCore:./dirbox']),
 dict(id='ss-m20-label', group='SS-M20', repo='ANetCore', desc='目录哈希 h 不带标签(h = SHA-256(aid))',
  edits=[('dirbox/dirbox.go', '\td.Write([]byte(hashLabel))\n\td.Write([]byte{0})\n', '')],
  tests=DIRBOX_T, full=['ANetCore:./dirbox']),
 # ---- the area's own
 dict(id='ss-hmac-equal', group='SS-2', repo='ANetCore', desc='VerifySessionMAC 用 bytes.Equal 比较(非常数时间)',
  edits=[('relayauth/session.go', '\tif !hmac.Equal(want, mac) {', '\tif string(want) != string(mac) {')],
  tests=[('ANetCore:./relayauth', '^TestVerifySessionMACUsesHMACEqual$')], full=['ANetCore:./relayauth']),
 dict(id='ss-nul-dir', group='SS-2', repo='ANetCore', desc='dirbox 的 h 接受含 U+0000 的 AID(CORE-E9)',
  edits=[('dirbox/dirbox.go', '\tcase strings.IndexByte(aid, 0) >= 0:', '\tcase false && strings.IndexByte(aid, 0) >= 0:')],
  tests=NUL_T, full=['ANetCore:./dirbox']),
 dict(id='ss-nul-mac', group='SS-2', repo='ANetCore', desc='会话 MAC 接受含 U+0000 的 method、目标或提示(CORE-E9)',
  edits=[('relayauth/session.go', '\t\tif strings.IndexByte(f.v, 0) >= 0 {', '\t\tif false && strings.IndexByte(f.v, 0) >= 0 {')],
  tests=NUL_T, full=['ANetCore:./relayauth']),
 dict(id='ss-nul-sesskey', group='SS-2', repo='ANetCore', desc='会话钥投递接受含 U+0000 的 hub AID(CORE-E9)',
  edits=[('relayauth/session.go', '\tif strings.IndexByte(hubAID, 0) >= 0 {', '\tif false && strings.IndexByte(hubAID, 0) >= 0 {')],
  tests=NUL_T, full=['ANetCore:./relayauth']),
 dict(id='ss-retired-send', group='SS-2', repo='ANetCore', desc='relayauth 恢复退役的 ActionSend',
  edits=[('relayauth/relayauth.go', '\tActionRevoke = "revoke"\n', '\tActionRevoke = "revoke"\n\t// ActionSend is back.\n\tActionSend = "send"\n')],
  tests=REGISTRY_T, full=['ANetCore:./wire']),
]
