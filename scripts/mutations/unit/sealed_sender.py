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

# ---- the delivery credential and the grant (ANetCore seal/access.go, seal/grant.go): M1-M4, the
# CheckGrant side of M15, and the area's own. ACCESS-1 and GRANT-1 are the vectors that must go red.
ACCESS_T = [('ANetCore:./seal', '^(TestACCESS_1|TestVerifyAccessOrder)$')]
GRANT_T = [('ANetCore:./seal', '^(TestGRANT_1|TestCheckGrantOrder|TestSEALED_2A)$')]

MUTS += [
 # ---- M1: the proof of possession must be checked
 dict(id='ss-m1-proof', group='SS-M1', repo='ANetCore', desc='持有证明验证恒真(凭据可挪到别的信封、换持有钥)',
  edits=[('seal/access.go', '\tif !ed25519.Verify(body.Holder, accessInput(body.CapID, envDigest), a.Proof) {',
          '\tif false && !ed25519.Verify(body.Holder, accessInput(body.CapID, envDigest), a.Proof) {')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 # ---- M2: env_digest must cover ct and key 8
 dict(id='ss-m2-ct', group='SS-M2', repo='ANetCore', desc='env_digest 不含 ct(凭据挪到另一封信仍有效)',
  edits=[('seal/envelope.go', '\tdelete(fields, outerKeyAccess)\n', '\tdelete(fields, outerKeyAccess)\n\tdelete(fields, outerKeyCT)\n')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 dict(id='ss-m2-exp', group='SS-M2', repo='ANetCore', desc='env_digest 不含外层键 8(hub 加上键 8 后凭据仍有效)',
  edits=[('seal/envelope.go', '\tdelete(fields, outerKeyAccess)\n', '\tdelete(fields, outerKeyAccess)\n\tdelete(fields, outerKeyExp)\n')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 # ---- M3: the capability's owner must be the envelope's to
 dict(id='ss-m3-owner', group='SS-M3', repo='ANetCore', desc='VerifyAccess 不比较 owner == to(能力可投给别的收件方)',
  edits=[('seal/access.go', '\tif owner == "" || body.Owner != owner {', '\tif false && (owner == "" || body.Owner != owner) {')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 # ---- M4: the capability's epoch must be the owner's mailbox epoch
 dict(id='ss-m4-epoch', group='SS-M4', repo='ANetCore', desc='VerifyAccess 不比较 Cap 的纪元(换信箱钥后旧能力仍被接受)',
  edits=[('seal/access.go', '\tif mbox.Epoch == 0 || body.Epoch != mbox.Epoch {', '\tif false && (mbox.Epoch == 0 || body.Epoch != mbox.Epoch) {')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 # ---- M15 (the CheckGrant part): p2p only in a contact peer grant
 dict(id='ss-m15-grant', group='SS-M15', repo='ANetCore', desc='CheckGrant 接受 stranger grant 里的 p2p',
  edits=[('seal/grant.go', '\tif g.P2P != nil && (body.Class != CapClassContact || g.IX != nil) {', '\tif g.P2P != nil && g.IX != nil {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 # ---- the area's own: VerifyAccess
 dict(id='ss1-sig', group='SS-1', repo='ANetCore', desc='VerifyAccess 不验 Cap.sig',
  edits=[('seal/access.go', '\tif len(mbox.Pub) != ed25519.PublicKeySize || !ed25519.Verify(mbox.Pub, a.Cap.Body, a.Cap.Sig) {',
          '\tif len(mbox.Pub) != ed25519.PublicKeySize {')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 dict(id='ss1-type', group='SS-1', repo='ANetCore', desc='VerifyAccess 不查类型标签 anet.cap/1',
  edits=[('seal/access.go', '\tif body.Type != CapType {\n\t\treturn bad("type"', '\tif false && body.Type != CapType {\n\t\treturn bad("type"')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 dict(id='ss1-exp', group='SS-1', repo='ANetCore', desc='VerifyAccess 不查 cap_exp 窗口',
  edits=[('seal/access.go', '\tif !capExpInWindow(body.CapExp, now) {\n\t\treturn bad("exp"', '\tif false && !capExpInWindow(body.CapExp, now) {\n\t\treturn bad("exp"')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 dict(id='ss1-window', group='SS-1', repo='ANetCore', desc='cap_exp 窗口没有上界(now + 31 天之外的能力被接受)',
  edits=[('seal/access.go', '\treturn lo <= capExp && capExp <= satAdd(now, CapExpMaxAheadMS)', '\treturn lo <= capExp')],
  tests=ACCESS_T + GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-class', group='SS-1', repo='ANetCore', desc='VerifyAccess 接受 contact、stranger 以外的 class',
  edits=[('seal/access.go', '\tif body.Class != CapClassContact && body.Class != CapClassStranger {\n\t\treturn bad("decode"',
          '\tif false && body.Class != CapClassContact && body.Class != CapClassStranger {\n\t\treturn bad("decode"')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 dict(id='ss1-e2', group='SS-1', repo='ANetCore', desc='Cap.body 不做重编码比对(CORE-E2:非规范编码被接受)',
  edits=[('seal/access.go', '\tif !bytes.Equal(re, b) {\n\t\treturn nil, errors.New("cap body is not the CoreDet',
          '\tif false && !bytes.Equal(re, b) {\n\t\treturn nil, errors.New("cap body is not the CoreDet')],
  tests=ACCESS_T + GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-label', group='SS-1', repo='ANetCore', desc='持有证明的标签改为 anet-access/v1(L5)',
  edits=[('seal/access.go', 'const accessLabel = "anet-access/v2\\x00"', 'const accessLabel = "anet-access/v1\\x00"')],
  tests=ACCESS_T, full=['ANetCore:./seal']),
 # ---- the area's own: CheckGrant
 dict(id='ss1-grant-key', group='SS-1', repo='ANetCore', desc='Grant 接受未登记的键(CORE-E3)',
  edits=[('seal/grant.go', '\tif k, ok := firstKeyOutside(m, 1, 6); ok {', '\tif k, ok := firstKeyOutside(m, 1, 6); ok && false {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-home', group='SS-1', repo='ANetCore', desc='Grant 的 home 接受 U+0000(CORE-E9)',
  edits=[('seal/grant.go', '\tif g.Home == "" || strings.IndexByte(g.Home, 0) >= 0 {', '\tif g.Home == "" || false && strings.IndexByte(g.Home, 0) >= 0 {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-owner', group='SS-1', repo='ANetCore', desc='CheckGrant 不比较 owner == inner.from',
  edits=[('seal/grant.go', '\tif body.Owner != inner.From {', '\tif false && body.Owner != inner.From {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-keys', group='SS-1', repo='ANetCore', desc='CheckGrant 不核对钥集属于发送方',
  edits=[('seal/grant.go', '\tif keys == nil || keys.AID != inner.From {', '\tif keys == nil {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-epoch', group='SS-1', repo='ANetCore', desc='CheckGrant 不比较 Cap 纪元与钥集的 mbox 纪元',
  edits=[('seal/grant.go', '\tif keys.Mbox.Epoch == 0 || body.Epoch != keys.Mbox.Epoch {', '\tif false && (keys.Mbox.Epoch == 0 || body.Epoch != keys.Mbox.Epoch) {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-sig', group='SS-1', repo='ANetCore', desc='CheckGrant 不验 Cap.sig',
  edits=[('seal/grant.go', '\tif len(keys.Mbox.Pub) != ed25519.PublicKeySize || !ed25519.Verify(keys.Mbox.Pub, g.Cap.Body, g.Cap.Sig) {',
          '\tif len(keys.Mbox.Pub) != ed25519.PublicKeySize {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-holder', group='SS-1', repo='ANetCore', desc='CheckGrant 不核对 holder_seed 推出的公钥 == CapBody.holder',
  edits=[('seal/grant.go', '\tif !bytes.Equal(g.HolderKey().Public().(ed25519.PublicKey), body.Holder) {\n\t\treturn bad("holder"',
          '\tif false && !bytes.Equal(g.HolderKey().Public().(ed25519.PublicKey), body.Holder) {\n\t\treturn bad("holder"')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-class', group='SS-1', repo='ANetCore', desc='CheckGrant 接受 contact、stranger 以外的 class',
  edits=[('seal/grant.go', '\tif body.Class != CapClassContact && body.Class != CapClassStranger {\n\t\treturn bad("class"',
          '\tif false && body.Class != CapClassContact && body.Class != CapClassStranger {\n\t\treturn bad("class"')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-exp', group='SS-1', repo='ANetCore', desc='CheckGrant 不查 cap_exp 窗口',
  edits=[('seal/grant.go', '\tif !capExpInWindow(body.CapExp, now) {\n\t\treturn bad("exp"', '\tif false && !capExpInWindow(body.CapExp, now) {\n\t\treturn bad("exp"')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-ix', group='SS-1', repo='ANetCore', desc='一次性 grant 的 ix 不必等于内层 ix',
  edits=[('seal/grant.go', '\tif g.IX != nil && *g.IX != inner.IX {', '\tif false && g.IX != nil && *g.IX != inner.IX {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-pay', group='SS-1', repo='ANetCore', desc='一次性 grant 可以带 pay',
  edits=[('seal/grant.go', '\tif g.Pay != nil && g.IX != nil {', '\tif false && g.Pay != nil && g.IX != nil {')],
  tests=GRANT_T, full=['ANetCore:./seal']),
 dict(id='ss1-grant-null-pay', group='SS-1', repo='ANetCore', desc='Grant 的 pay 为 CBOR null 时当作 false',
  edits=[('seal/grant.go', '\tcase len(raw) == 1 && raw[0] == 0xf4:', '\tcase len(raw) == 1 && (raw[0] == 0xf4 || raw[0] == 0xf6):')],
  tests=[('ANetCore:./seal', '^TestGrantNullIsNotAbsent$')], full=['ANetCore:./seal']),
 dict(id='ss1-grant-null-opt', group='SS-1', repo='ANetCore', desc='Grant 的 ix、p2p 为 CBOR null 时当作缺键(一次性 grant 变成对端 grant)',
  edits=[('seal/grant.go', '\t\tif _, ok := m[k]; !ok {\n\t\t\treturn nil, nil\n',
          '\t\tif raw, ok := m[k]; !ok || len(raw) == 1 && raw[0] == 0xf6 {\n\t\t\treturn nil, nil\n')],
  tests=[('ANetCore:./seal', '^TestGrantNullIsNotAbsent$')], full=['ANetCore:./seal']),
]

# ---- the hub's mailbox key rule, revocations and quotas (SS-3; repo ANetHub, kept in this list so
# that a round's --check finds it): M25 and the area's own. The weekly backup's exclusions
# (deploy/hub-db-roll.sh) need the sqlite3 CLI and are checked by
# TestTheWeeklyBackupKeepsRevocationsAndQuotas under ANET_HUB_SCRIPT_TESTS=require, not here.
MBOX_T = [('ANetHub:./internal/aghub', '^TestPublishKeysMboxEpochMustRise$')]
REVOKE_T = [('ANetHub:./internal/aghub', '^(TestMailboxRevoke|TestRevocationListFull|TestRevocationJanitor31Days|TestCapRevokedFollowsRevocationsAndDeregistration)$')]
QUOTA_T = [('ANetHub:./internal/aghub', '^TestQuotaOnlyRises$'), ('ANetHub:./cmd/anet-hub', '^TestQuotaCommand$')]
B64_T = [('ANetHub:./internal/wire3', '^(TestDecodeB64URLStrict|TestDecodeB64URLSize)$')]

MUTS += [
 # ---- M25: the hub must refuse a new mailbox key under the same epoch
 dict(id='ss-m25-pub', group='SS-M25', repo='ANetHub', desc='hub 接受 mbox.pub 改变而纪元不增',
  edits=[('internal/aghub/keys.go', '\tcase !samePub && next.Epoch == prev.Epoch:', '\tcase false && !samePub && next.Epoch == prev.Epoch:')],
  tests=MBOX_T, full=['ANetHub:./internal/aghub']),
 # ---- the area's own: the rest of SSR-W9
 dict(id='ss3-epoch-down', group='SS-3', repo='ANetHub', desc='hub 接受纪元下降(换回旧钥)',
  edits=[('internal/aghub/keys.go', '\tcase next.Epoch < prev.Epoch:', '\tcase false && next.Epoch < prev.Epoch:')],
  tests=MBOX_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-epoch-same-pub', group='SS-3', repo='ANetHub', desc='hub 接受公钥不变而纪元改变',
  edits=[('internal/aghub/keys.go', '\tcase samePub && next.Epoch != prev.Epoch:', '\tcase false && samePub && next.Epoch != prev.Epoch:')],
  tests=MBOX_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-epoch-bound', group='SS-3', repo='ANetHub', desc='hub 接受纪元 = 2^53',
  edits=[('internal/aghub/keys.go', '\tif next.Epoch >= MaxMboxEpoch {', '\tif next.Epoch > MaxMboxEpoch {')],
  tests=MBOX_T, full=['ANetHub:./internal/aghub']),
 # ---- revocations (SSR-H10, SSR-H11, SSR-D21)
 dict(id='ss3-revoke-dup', group='SS-3', repo='ANetHub', desc='同一请求里重复的 cap_id 计两次',
  edits=[('internal/aghub/revoke.go', '\t\tif seen[string(id)] {\n\t\t\tcontinue\n\t\t}\n', '')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-revoke-again', group='SS-3', repo='ANetHub', desc='已吊销未过期的 cap_id 再次计为新增并重置 31 天',
  edits=[('internal/aghub/revoke.go', '\t\tif live == 0 {\n\t\t\tfresh = append(fresh, id)', '\t\tif true {\n\t\t\tfresh = append(fresh, id)')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-list-full', group='SS-3', repo='ANetHub', desc='吊销表满了仍写入(不回 revocation_list_full)',
  edits=[('internal/aghub/revoke.go', '\tif total+len(fresh) > MaxRevocationsPerOwner {', '\tif false && total+len(fresh) > MaxRevocationsPerOwner {')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-revoke-batch', group='SS-3', repo='ANetHub', desc='/mailbox/revoke 接受 1001 个 cap_id',
  edits=[('internal/aghub/revoke.go', '\tif len(req.CapIDs) == 0 || len(req.CapIDs) > MaxRevokeBatch {', '\tif len(req.CapIDs) == 0 || len(req.CapIDs) > MaxRevokeBatch+1 {')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-revoke-size', group='SS-3', repo='ANetHub', desc='/mailbox/revoke 不核对 cap_id 为 16 字节',
  edits=[('internal/aghub/revoke.go', '\t\tid, err := wire3.DecodeB64URLSize("cap_ids["+strconv.Itoa(i)+"]", c, seal.CapIDLen)',
          '\t\tid, err := wire3.DecodeB64URL("cap_ids["+strconv.Itoa(i)+"]", c)')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-revoke-action', group='SS-3', repo='ANetHub', desc='/mailbox/revoke 收 relayauth poll 的签名',
  edits=[('internal/aghub/revoke.go', '\ta, ok := s.authRegistered(w, r, relayauth.ActionRevoke, revokeBodyLimit)', '\ta, ok := s.authRegistered(w, r, relayauth.ActionPoll, revokeBodyLimit)')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-keep-31d', group='SS-3', repo='ANetHub', desc='吊销保留少 1 毫秒(满 31 天前失效)',
  edits=[('internal/aghub/revoke.go', 'func revocationCutoff(now time.Time) int64 { return now.Add(-RevocationKeep).UnixMilli() }',
          'func revocationCutoff(now time.Time) int64 { return now.Add(-RevocationKeep + time.Millisecond).UnixMilli() }')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-cache-race', group='SS-3', repo='ANetHub', desc='吊销缓存收下与吊销竞争的旧读数(CapRevoked 一直答未吊销)',
  edits=[('internal/aghub/revoke.go', '\tif gen != c.gen {\n\t\treturn\n\t}\n', '')],
  tests=[('ANetHub:./internal/aghub', '^TestRevokedCacheDropsARacedRead$')], full=['ANetHub:./internal/aghub']),
 dict(id='ss3-cache-dereg', group='SS-3', repo='ANetHub', desc='注销后吊销缓存不清空',
  edits=[('internal/aghub/aghub.go', '\t\ts.revoked.forget()\n', '')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-dereg-revocations', group='SS-3', repo='ANetHub', desc='注销不删该 AID 的 cap_revocation(CORE-K27)',
  edits=[('internal/aghub/deregister.go', '\t{"revoked delivery credentials", execAID(`DELETE FROM cap_revocation WHERE owner = ?`)},\n', '')],
  tests=[('ANetHub:./internal/aghub', '^TestDeregRemovesEveryAIDKeyedRow$')], full=['ANetHub:./internal/aghub']),
 dict(id='ss3-register-revocations', group='SS-3', repo='ANetHub', desc='/register 不报 revocations(SSR-D21 无从补推)',
  edits=[('internal/aghub/server.go', '\t\tout.Revocations = uint64(n)\n', '\t\t_ = n\n')],
  tests=REVOKE_T, full=['ANetHub:./internal/aghub']),
 # ---- quotas (SSR-H5, SSR-H22)
 dict(id='ss3-quota-below', group='SS-3', repo='ANetHub', desc='quota 接受低于缺省值',
  edits=[('internal/aghub/quota.go', '\t\tcase *c.v < c.least:', '\t\tcase false && *c.v < c.least:')],
  tests=QUOTA_T, full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub']),
 dict(id='ss3-quota-refresh', group='SS-3', repo='ANetHub', desc='hub 进程读过一次 quota_override 后不再重读(改动永不生效)',
  edits=[('internal/aghub/quota.go', '\tif !c.valid || now.Sub(c.readAt) >= quotaRefresh || now.Before(c.readAt) {', '\tif !c.valid {')],
  tests=QUOTA_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-quota-clamp', group='SS-3', repo='ANetHub', desc='手写的低于缺省值的 quota_override 行压低配额',
  edits=[('internal/aghub/quota.go', '\t\tq.NonContactMsgs = max(q.NonContactMsgs, def.NonContactMsgs)\n', '')],
  tests=QUOTA_T, full=['ANetHub:./internal/aghub']),
 dict(id='ss3-quota-dereg', group='SS-3', repo='ANetHub', desc='运营者撤销准入后保留其调高的配额',
  edits=[('internal/aghub/deregister.go', '\t\tif _, err := tx.Exec(`DELETE FROM quota_override WHERE aid=?`, aid); err != nil {\n\t\t\treturn DeregResult{}, err\n\t\t}\n', '')],
  tests=[('ANetHub:./internal/aghub', '^TestDeregRemovesEveryAIDKeyedRow$')], full=['ANetHub:./internal/aghub']),
 # ---- the removed flags (SSR-H20)
 dict(id='ss3-flag-fixed', group='SS-3', repo='ANetHub', desc='删除的 relay flag 不按 hub_flag_removed.fixed 拒绝',
  edits=[('cmd/anet-hub/main.go', '\t\tif r[0].fixed {', '\t\tif false && r[0].fixed {')],
  tests=[('ANetHub:./cmd/anet-hub', '^TestRemovedRelayFlagsRefuse$')], full=['ANetHub:./cmd/anet-hub']),
 dict(id='ss3-fed-lookup-wired', group='SS-3', repo='ANetHub', desc='anet-hub 不再接上联邦钥查询(取代退役的 c32-2)',
  edits=[('cmd/anet-hub/wire_federation.go', '\t\t\td.srv0.SetFederatedKeyLookup(fed.LookupKeys)\n', '')],
  tests=[('ANetHub:./cmd/anet-hub', '^TestTheHubWiresTheFederatedKeyLookup$')], full=['ANetHub:./cmd/anet-hub']),
 # ---- the one base64url decoder (CORE-E5)
 dict(id='ss3-b64-alphabet', group='SS-3', repo='ANetHub', desc='base64url 助手不先查字母表(标准解码器跳过的 \\r \\n 被接受)',
  edits=[('internal/wire3/b64url.go', '\t\tif !b64urlChar(s[i]) {', '\t\tif false && !b64urlChar(s[i]) {')],
  tests=B64_T, full=['ANetHub:./internal/wire3']),
 dict(id='ss3-b64-trailing', group='SS-3', repo='ANetHub', desc='base64url 助手接受非零的末尾位(一个值两种拼写)',
  edits=[('internal/wire3/b64url.go', 'var b64url = base64.RawURLEncoding.Strict()', 'var b64url = base64.RawURLEncoding')],
  tests=B64_T, full=['ANetHub:./internal/wire3']),
]
