# si.py: A2A-DESIGN §1 SI-1..SI-10 — mutation list for scripts/mutations/unitmut.py (see its header for the format).
# Written for docs/notes/0026 (each SI at least three mutations); a mutation that no longer applies reports APPLY-ERR: fix its
# edit to the moved code rather than dropping it.

MUTS = [
 dict(
  id='si1-1', group='SI-1', repo='ANetCore',
  desc='关闭加密: 密文即明文(scripts/mutations/si1-plaintext-envelope.patch)',
  patch=MUTATIONS + '/si1-plaintext-envelope.patch',
  tests=[('ANetCore:./seal', 'TestOpenRecipientAndKey|TestVEC_SEALED_1|TestRFC9180'), ('ANet:./internal/daemon', 'TestTheHubHoldsOnlyCiphertext')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si1-2', group='SI-1', repo='ANet',
  desc='商户送 hub 的 paymentRequirements 带上报价 extra(描述工作)',
  edits=[
   ('module/x402/check.go', '\t\tPayTo: o.PayTo, MaxTimeoutSeconds: o.MaxTimeoutSeconds,\n\t}', '\t\tPayTo: o.PayTo, MaxTimeoutSeconds: o.MaxTimeoutSeconds, Extra: o.Extra,\n\t}'),
  ],
  tests=[('ANet:./module/x402', 'TestSettleSendsRequirementsAndNothingAboutTheWork'), ('ANet:./internal/daemon', 'TestAQuotedTaskIsPaidAndCompletedOnTheSameTask')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si1-3', group='SI-1', repo='ANetHub',
  desc='hub /reviews 不再拒收 request_doc/deliverable',
  edits=[
   ('internal/aghub/server.go', '\tif req.RequestDoc != "" || req.Deliverable != "" {', '\tif false && (req.RequestDoc != "" || req.Deliverable != "") {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAReviewUploadCarryingContentIsRefused|TestAReviewIsServedWithoutContent')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si1-4', group='SI-1', repo='ANetHub',
  desc='hub CheckEnvelope 恒放行(明文/错投的载荷也入库)',
  edits=[
   ('internal/aghub/relay.go', 'func CheckEnvelope(toAID string, envelope []byte) error {\n', 'func CheckEnvelope(toAID string, envelope []byte) error {\n\tif true {\n\t\treturn nil\n\t}\n'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestTheRelayRefusesAnythingButASealedEnvelopeForTheRecipient')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si1-5', group='SI-1', repo='ANetHub',
  desc='恢复 admin 官方 agent 采集与路由(scripts/mutations/si1-restore-official-harvest.patch)',
  patch=MUTATIONS + '/si1-restore-official-harvest.patch',
  tests=[('ANetHub:./internal/admin', 'TestOfficialAgentOperationsAreGone|TestNoAdminSurfaceServesRelayContent|TestAManifestWithARemovedSectionIsRefused')],
  full=['ANetHub:./internal/admin'],
 ),
 dict(
  id='si1-6', group='SI-1', repo='ANet',
  desc='canary.py 不再生成 base64url 形式(扫描器漏检)',
  edits=[
   ('scripts/canary.py', '        for i, b in enumerate(_b64_aligned(raw, base64.urlsafe_b64encode)):\n            add(label, "base64url/%d" % i, b)\n', ''),
  ],
  tests=[('ANet:./scripts', 'TestTheCanarySearchSeesEveryEncoding|TestCanaryHitsFindsEncodedCopies|TestTheCanarySearchSeesURLSafeBase64')],
  full=['ANet:./scripts'],
 ),
 dict(
  id='si1-7', group='SI-1', repo='ANet',
  desc='canary.py settle 检查不查工作字段(resource/description/extra)',
  edits=[
   ('scripts/canary.py', '    walk(d, "body")\n    return out', '    return out'),
  ],
  tests=[('ANet:./scripts', 'TestTheSettlementCheck')],
  full=['ANet:./scripts'],
 ),
 dict(
  id='si1-8', group='SI-1', repo='ANetHub',
  desc='admin 恢复 /api/official/{id}/insights 路由',
  edits=[
   ('internal/admin/server.go', '\tapi("DELETE "+b+"/api/official/{id}", s.hDeleteOfficial)\n', '\tapi("DELETE "+b+"/api/official/{id}", s.hDeleteOfficial)\n\tapi("GET "+b+"/api/official/{id}/insights", s.hOfficials)\n'),
  ],
  tests=[('ANetHub:./internal/admin', 'TestOfficialAgentOperationsAreGone')],
  full=['ANetHub:./internal/admin'],
 ),
 dict(
  id='si2-1', group='SI-2', repo='ANetHub',
  desc='RelayAck 不删行(ack 即删 失效)',
  edits=[
   ('internal/aghub/relay.go', 'res, err := tx.Exec(`DELETE FROM relay_message WHERE id=? AND to_aid=?`, id, toAID)', 'res, err := tx.Exec(`UPDATE relay_message SET size=size WHERE id=? AND to_aid=?`, id, toAID)'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAnAckDeletesTheRow|TestAnAckedEnvelopeLeavesNoBytesOnDisk')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si2-2', group='SI-2', repo='ANetHub',
  desc='hub 库关闭 secure_delete',
  edits=[
   ('internal/aghub/aghub.go', '&_pragma=secure_delete(ON)")', '&_pragma=secure_delete(OFF)")'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAnAckedEnvelopeLeavesNoBytesOnDisk|TestAnAckDeletesTheRow')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si2-3', group='SI-2', repo='ANetHub',
  desc='relay_message 表加回 from_aid 列',
  edits=[
   ('internal/aghub/relay.go', '   to_aid TEXT NOT NULL,\n   size INTEGER NOT NULL,', '   to_aid TEXT NOT NULL,\n   from_aid TEXT,\n   size INTEGER NOT NULL,'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestTheRelayDoesNotStoreTheSender')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si2-4', group='SI-2', repo='ANetHub',
  desc='hRelaySend 把每条信封的发送方写进 hub_meta(换个表存 from)',
  edits=[
   ('internal/aghub/server.go', '\tid, err := s.store.RelayEnqueue(req.ToAID, envelope)\n', '\tid, err := s.store.RelayEnqueue(req.ToAID, envelope)\n\tif err == nil {\n\t\t_, _ = s.store.db.Exec(`INSERT OR REPLACE INTO hub_meta(key, value) VALUES(?, ?)`, fmt.Sprintf("relay_from/%d", id), a.AID)\n\t}\n'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestTheRelayDoesNotStoreTheSender|TestASendLeavesTheSenderInNoTable')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si2-5', group='SI-2', repo='ANetHub',
  desc='PurgeExpiredRelay 从不删除过期行',
  edits=[
   ('internal/aghub/relay.go', '`DELETE FROM relay_message WHERE created_at < ?`', '`DELETE FROM relay_message WHERE created_at < ? AND 0`'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestUndeliveredEnvelopesExpire')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si2-6', group='SI-2', repo='ANet',
  desc='requester 授权的 InteractionID 填 ix 而不是 pay_bind(hub 存交互 id)',
  edits=[
   ('internal/daemon/x402task.go', 'signed, err := p.Authorize(*opt, ix.ID, x402a2a.PayBind(ix.ID, ix.TaskNonce), req.Purpose)', 'signed, err := p.Authorize(*opt, ix.ID, ix.ID, req.Purpose)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAQuotedTaskIsPaidAndCompletedOnTheSameTask|TestThePaidLoopClosesEndToEnd')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si3-1', group='SI-3', repo='ANetCore',
  desc='seal.Open: 不比较外层 to 与本节点 AID(收件人不符照收)',
  edits=[
   ('seal/envelope.go', '\tif outer.To != selfAID {', '\tif false && outer.To != selfAID {'),
  ],
  tests=[('ANetCore:./seal', 'TestOpenRecipientAndKey'), ('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si3-2', group='SI-3', repo='ANetCore',
  desc='seal.CheckTime: 去掉 now > exp 检查(过期照收)',
  edits=[
   ('seal/envelope.go', '\tif now > inner.Exp {', '\tif false && now > inner.Exp {'),
  ],
  tests=[('ANetCore:./seal', 'TestCheckTime'), ('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si3-3', group='SI-3', repo='ANetCore',
  desc='seal.decodeInner: 放过 0–63 未知内层键',
  edits=[
   ('seal/envelope.go', 'if k, ok := firstUnknownCritical(fields, innerKnown); ok {', 'if k, ok := firstUnknownCritical(fields, innerKnown); ok && false {'),
  ],
  tests=[('ANetCore:./seal', 'TestInnerKeySpace|TestSignedInnerFieldMutations')],
  full=['ANetCore:./seal'],
 ),
 dict(
  id='si3-4', group='SI-3', repo='ANetCore',
  desc='seal.VerifyInnerSig: 忽略签名校验失败',
  edits=[
   ('seal/envelope.go', '\tif err := identity.VerifyObject(kel, inner.From, seq, inner.TS, preimage, inner.Sig); err != nil {\n\t\treturn fromVErr(err)', '\tif err := identity.VerifyObject(kel, inner.From, seq, inner.TS, preimage, inner.Sig); err != nil && false {\n\t\treturn fromVErr(err)'),
  ],
  tests=[('ANetCore:./seal', 'TestSignedInnerFieldMutations|TestOuterFieldsAreSigned'), ('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass|TestAForgedSenderInjectedAtTheHubIsDropped')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si3-5', group='SI-3', repo='ANetCore',
  desc='seal.Open: 不比较内层 to 与外层 to',
  edits=[
   ('seal/envelope.go', '\tif inner.To != outer.To {', '\tif false && inner.To != outer.To {'),
  ],
  tests=[('ANetCore:./seal', 'TestSignedInnerFieldMutations|TestInnerStructure|TestOpenRecipientAndKey')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si3-6', group='SI-3', repo='ANetCore',
  desc='seal.ParseOuter: 不检查信封版本 v',
  edits=[
   ('seal/envelope.go', '\tif v != EnvelopeVersion {\n\t\treturn nil, fail(ReasonBadVersion', '\tif false && v != EnvelopeVersion {\n\t\treturn nil, fail(ReasonBadVersion'),
  ],
  tests=[('ANetCore:./seal', 'TestParseOuter'), ('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass')],
  full=['ANetCore:./seal'],
 ),
 dict(
  id='si3-7', group='SI-3', repo='ANet',
  desc='daemon process: 跳过持久重放表查询(seen 恒 false)',
  edits=[
   ('internal/daemon/receive.go', '\tif seen {\n\t\t// A redelivered delegation', '\tif false && seen {\n\t\t// A redelivered delegation'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass|TestARedeliveredDelegateResendsTheAnswer|TestARedeliveredChatMessageIsStoredOnce|TestARedeliveredResultIsRecordedOnce|TestAnAnsweredCapabilityCallIsNotRunAgain')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si3-8', group='SI-3', repo='ANet',
  desc='daemon commitRx: 重放行已存在仍提交业务写入(去掉 claim 结果判断)',
  edits=[
   ('internal/daemon/receive.go', '\t\tif !claimed {\n\t\t\treturn errReplayDuplicate', '\t\tif !claimed && false {\n\t\t\treturn errReplayDuplicate'),
  ],
  tests=[('ANet:./internal/daemon', 'TestADelegationSealedTwiceArrivingTogetherIsRecordedOnce|TestAResultSealedTwiceArrivingTogetherIsRecordedOnce|TestTheSameEnvelopeOverP2PAndHubIsProcessedOnce|TestAStoreFailureThenRedeliveryIsProcessedOnce|TestCommitRxRunsTheBusinessWriteOncePerMessage')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si4-1', group='SI-4', repo='ANet',
  desc='daemon 第 7 步: 签名不符(bad-sig)仍放行(= scripts/mutations/si4-skip-step7.patch)',
  edits=[
   ('internal/daemon/receive.go', 'if err := seal.VerifyInnerSig(in, op.Preimage, use, now, d.rotationGrace()); err != nil {', 'if err := seal.VerifyInnerSig(in, op.Preimage, use, now, d.rotationGrace()); err != nil && seal.ReasonOf(err) != seal.ReasonBadSig {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAForgedSenderInjectedAtTheHubIsDropped|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si4-2', group='SI-4', repo='ANet',
  desc='authorizeMessage: 不核对交互对端 == from',
  edits=[
   ('internal/daemon/receive.go', '\tif ps.denied(m.from) {\n\t\tr := d.drop(dropDenied, nil)\n\t\treturn &r\n\t}\n\tif ix.PeerAID != m.from {', '\tif ps.denied(m.from) {\n\t\tr := d.drop(dropDenied, nil)\n\t\treturn &r\n\t}\n\tif false && ix.PeerAID != m.from {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAForgedSenderInjectedAtTheHubIsDropped|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si4-3', group='SI-4', repo='ANet',
  desc='authorizeReply: 不核对 status/result 发送方是该交互的 provider',
  edits=[
   ('internal/daemon/receive.go', '\tif ix.Role != interactions.RoleOutbound {\n\t\tr := d.drop(dropWrongRole, fmt.Errorf("%s is %s", m.ix, ix.Role))\n\t\treturn &r\n\t}\n\tif ix.PeerAID != m.from {', '\tif ix.Role != interactions.RoleOutbound {\n\t\tr := d.drop(dropWrongRole, fmt.Errorf("%s is %s", m.ix, ix.Role))\n\t\treturn &r\n\t}\n\tif false && ix.PeerAID != m.from {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAForgedSenderInjectedAtTheHubIsDropped|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si4-4', group='SI-4', repo='ANet',
  desc='authorizeDelegate: TaskDoc 签名方不必是 inner.from',
  edits=[
   ('internal/daemon/receive.go', '\tif signer != m.from {', '\tif false && signer != m.from {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAForgedSenderInjectedAtTheHubIsDropped|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si4-5', group='SI-4', repo='ANetCore',
  desc='VerifyEncKeySet: 去掉预期 AID(以 KEL 推出的 AID 代替)',
  edits=[
   ('seal/enckeys.go', '\tif kelAID != expectAID {', '\tif expectAID = kelAID; false {'),
  ],
  tests=[('ANetCore:./seal', 'TestVerifyEncKeySetRecipientSubstitution'), ('ANet:./internal/daemon', 'TestAHubSubstitutingKeysForTheRecipientIsRefused')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si4-6', group='SI-4', repo='ANet',
  desc='resolveSenderKEL: 内层 KEL 推出的 AID 不必等于 inner.from',
  edits=[
   ('internal/daemon/receive.go', '\tif aid != from {\n\t\tr := d.drop(seal.ReasonFromMismatch', '\tif false && aid != from {\n\t\tr := d.drop(seal.ReasonFromMismatch'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAForgedSenderInjectedAtTheHubIsDropped|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si5-1', group='SI-5', repo='ANet',
  desc='defaultInbound: 全新安装 inbound.policy=open',
  edits=[
   ('internal/daemon/inbound.go', 'c := InboundConfig{Policy: PolicyClosed, PublicCapabilities: []PublicCapability{}}', 'c := InboundConfig{Policy: PolicyOpen, PublicCapabilities: []PublicCapability{}}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-2', group='SI-5', repo='ANet',
  desc='InboundConfig.normalize: 未写 policy 的配置按 open',
  edits=[
   ('internal/daemon/inbound.go', '\tif c.Policy == "" {\n\t\tc.Policy = PolicyClosed', '\tif c.Policy == "" {\n\t\tc.Policy = PolicyOpen'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults|TestAnInboundBlockWithoutAPolicyIsClosed'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-3', group='SI-5', repo='ANet',
  desc='defaultInbound: public_capabilities 默认非空',
  edits=[
   ('internal/daemon/inbound.go', 'c := InboundConfig{Policy: PolicyClosed, PublicCapabilities: []PublicCapability{}}', 'c := InboundConfig{Policy: PolicyClosed, PublicCapabilities: []PublicCapability{{ID: "cas.get"}}}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-4', group='SI-5', repo='ANet',
  desc='UntrustedMode: auto_reply 块未写 untrusted 时按 sandbox',
  edits=[
   ('internal/daemon/config.go', '\tif c.Untrusted == UntrustedSandbox {\n\t\treturn UntrustedSandbox', '\tif c.Untrusted != UntrustedOff {\n\t\treturn UntrustedSandbox'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults|TestExecRunsOnlyForTrustedPeers|TestOpenAndExecForUntrustedPeersConflict'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-5', group='SI-5', repo='ANet',
  desc='defaultPayments: auto_max=5',
  edits=[
   ('internal/daemon/spend.go', 'return PaymentsConfig{ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}', 'return PaymentsConfig{AutoMax: 5, ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-6', group='SI-5', repo='ANet',
  desc='defaultPayments: agent_max=5',
  edits=[
   ('internal/daemon/spend.go', 'return PaymentsConfig{ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}', 'return PaymentsConfig{AgentMax: 5, ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-7', group='SI-5', repo='ANet',
  desc='defaultPayments: agent_daily_max=20',
  edits=[
   ('internal/daemon/spend.go', 'return PaymentsConfig{ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}', 'return PaymentsConfig{AgentDailyMax: 20, ExplicitMax: &em, DailyMax: &dm, PayeesFile: &pf}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-8', group='SI-5', repo='ANet',
  desc='defaultPayments: payees_file 为空串(收款方白名单关闭)',
  edits=[
   ('internal/daemon/spend.go', 'em, dm, pf := uint64(defaultExplicitMax), uint64(defaultDailyMax), defaultPayeesFile', 'em, dm, pf := uint64(defaultExplicitMax), uint64(defaultDailyMax), ""'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-9', group='SI-5', repo='ANet',
  desc='PaymentsConfig.limits: 无 payments 块时收款方白名单关闭',
  edits=[
   ('internal/daemon/spend.go', 'l := SpendLimits{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: defaultPayeesFile}', 'l := SpendLimits{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: ""}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si5-10', group='SI-5', repo='ANet',
  desc='doctor SI5(): agent_daily_max 改动不报告',
  edits=[
   ('internal/daemon/setup.go', '"payments.agent_daily_max":    st.Payments.AgentDailyMax == 0,', '"payments.agent_daily_max":    true,'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFreshInstallIsClosed|TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent|TestInitOnlyAddsMissingKeys|TestPaymentsBlockDefaults|TestAConfigWithoutPaymentsIsGivenTheDefaults'), ('ANet:./cmd/anet', 'TestAFreshInitIsSafeByDoctorJSON|TestDoctorReportsEachSI5KeyThatChanged')],
  full=['ANet:./internal/daemon', 'ANet:./cmd/anet'],
 ),
 dict(
  id='si6-1', group='SI-6', repo='ANet',
  desc='投影: completed 文本任务(无回执)不带 anet.receipt_verified',
  edits=[
   ('internal/a2ashape/project.go', 'if len(ix.Receipt) > 0 || ix.State == interactions.StateCompleted {', 'if len(ix.Receipt) > 0 {'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestSI6|TestTextTaskCompleted|TestUnverifiedReceiptStaysCompleted'), ('ANet:./internal/mcpserv', 'TestSI6CheckerRefuses|TestTasksThroughMCPAreA2ATasksThatKeepSI6')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/mcpserv'],
 ),
 dict(
  id='si6-2', group='SI-6', repo='ANet',
  desc='effectStatus: 终态能力任务无答复时报 OK(不知道→没问题)',
  edits=[
   ('internal/a2ashape/project.go', '\treturn string(effect.Unverified)\n}', '\treturn string(effect.OK)\n}'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestSI6|TestCapabilityWithoutAnswer|TestEffectStatesTable|TestCapabilityUnverifiedIsCompletedNotOK'), ('ANet:./internal/mcpserv', 'TestTasksThroughMCPAreA2ATasksThatKeepSI6')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/mcpserv'],
 ),
 dict(
  id='si6-3', group='SI-6', repo='ANet',
  desc='receiptVerified: unverified 投影为 verified',
  edits=[
   ('internal/a2ashape/project.go', '\tcase interactions.VerificationUnverified:\n\t\treturn ReceiptUnverified', '\tcase interactions.VerificationUnverified:\n\t\treturn ReceiptVerified'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestSI6|TestUnverifiedReceiptStaysCompleted'), ('ANet:./internal/mcpserv', 'TestTasksThroughMCPAreA2ATasksThatKeepSI6')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/mcpserv'],
 ),
 dict(
  id='si6-4', group='SI-6', repo='ANet',
  desc='metadata: UNVERIFIED 时不写 anet.effect_status',
  edits=[
   ('internal/a2ashape/project.go', '\t\tif es != "" {\n\t\t\tm[KeyEffectStatus] = es', '\t\tif es != "" && es != string(effect.Unverified) {\n\t\t\tm[KeyEffectStatus] = es'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestSI6|TestCapabilityUnverifiedIsCompletedNotOK|TestEffectStatesTable'), ('ANet:./internal/mcpserv', 'TestTasksThroughMCPAreA2ATasksThatKeepSI6')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/mcpserv'],
 ),
 dict(
  id='si6-5', group='SI-6', repo='ANet',
  desc='Project: 效果 UNVERIFIED 或回执未验证时 state 改为 FAILED(合并两字段)',
  edits=[
   ('internal/a2ashape/project.go', '\tt.History = p.history()\n', '\tt.History = p.history()\n\tif t.Metadata[KeyEffectStatus] == string(effect.Unverified) || t.Metadata[KeyReceiptVerified] == ReceiptUnverified {\n\t\tt.Status.State = TaskStateFailed\n\t}\n'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestSI6|TestUnverifiedReceiptStaysCompleted|TestCapabilityUnverifiedIsCompletedNotOK'), ('ANet:./internal/mcpserv', 'TestTasksThroughMCPAreA2ATasksThatKeepSI6')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/mcpserv'],
 ),
 dict(
  id='si7-1', group='SI-7', repo='ANet',
  desc='loopguard.AllowedHost: 只核端口,任何主机名都放行(DNS 重绑定)',
  edits=[
   ('internal/loopguard/loopguard.go', '\treturn port != "" && p == port && LoopbackName(h)', '\treturn port != "" && p == port && (LoopbackName(h) || h != "")'),
  ],
  tests=[('ANet:./internal/daemon', 'TestHostAllowlistRefusesOtherNames|TestForeignOriginIsRefused'), ('ANet:./module/a2a', 'TestRequestChecks|TestEachLocalSurfaceTakesOnlyItsOwnCredential'), ('ANet:./internal/loopguard', '')],
  full=['ANet:./internal/daemon', 'ANet:./module/a2a'],
 ),
 dict(
  id='si7-2', group='SI-7', repo='ANet',
  desc='authGate: 不在会话白名单的路由也放行会话',
  edits=[
   ('internal/daemon/ctlsec.go', '\t\trule, ok := sessionRoutes[pattern]\n\t\tif !ok {', '\t\trule, ok := sessionRoutes[pattern]\n\t\tif !ok && false {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEveryRouteIsAllowlistedOrRefusedForASession|TestASessionCannotReachABearerOnlyHandler')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si7-3', group='SI-7', repo='ANet',
  desc='sessionRoutes: 白名单放宽加入 POST /pull',
  edits=[
   ('internal/daemon/ctlsec.go', '\t"POST /console/switch": {jsonFields: []string{"aid"}},\n', '\t"POST /console/switch": {jsonFields: []string{"aid"}},\n\t"POST /pull":           {},\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEveryRouteIsAllowlistedOrRefusedForASession|TestASessionCannotReachABearerOnlyHandler')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si7-4', group='SI-7', repo='ANet',
  desc='authGate: 任意 Bearer 都当作控制令牌',
  edits=[
   ('internal/daemon/ctlsec.go', '\t\t\tif subtle.ConstantTimeCompare([]byte(auth), want) != 1 {', '\t\t\tif false && subtle.ConstantTimeCompare([]byte(auth), want) != 1 {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAuthenticatedRoutesNeedACredential|TestTheLocalA2ATokenIsRefusedByTheControlPlane')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si7-5', group='SI-7', repo='ANet',
  desc='module/a2a authorized: 任意非空 Bearer 都放行(含控制令牌)',
  edits=[
   ('module/a2a/server.go', '\treturn subtle.ConstantTimeCompare([]byte(cred), []byte(s.cfg.token)) == 1', '\treturn cred != "" || subtle.ConstantTimeCompare([]byte(cred), []byte(s.cfg.token)) == 1'),
  ],
  tests=[('ANet:./module/a2a', 'TestClientWithoutCredentialsIsRefused|TestEachLocalSurfaceTakesOnlyItsOwnCredential|TestRequestChecks')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='si7-6', group='SI-7', repo='ANet',
  desc='控制台页面 HTML 末尾带控制令牌',
  edits=[
   ('internal/daemon/ctlsec.go', '\ttop.HandleFunc("GET /console", d.consoleHandler())', '\ttop.HandleFunc("GET /console", func(w http.ResponseWriter, r *http.Request) {\n\t\td.consoleHandler()(w, r)\n\t\tfmt.Fprint(w, "<!-- "+token+" -->")\n\t})'),
  ],
  tests=[('ANet:./internal/daemon', 'TestConsolePageHasNoTokenAndStrictHeaders')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si7-7', group='SI-7', repo='ANet',
  desc='loopguard.LoopbackName: 0.0.0.0 也算回环(控制口可绑全网卡)',
  edits=[
   ('internal/loopguard/loopguard.go', '\tcase "127.0.0.1", "localhost", "::1":', '\tcase "127.0.0.1", "localhost", "::1", "0.0.0.0":'),
  ],
  tests=[('ANet:./internal/daemon', 'TestNonLoopbackControlAddrIsRefused|TestTheControlTokenIsResolvedOnlyForALoopbackAddress'), ('ANet:./internal/loopguard', '')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si8-1', group='SI-8', repo='ANet',
  desc='cmd/anet 无 tag 文件直接引入 a2a-go(no_a2a 构建仍带 SDK)',
  edits=[
   ('cmd/anet/zz_mut.go', None, 'package main\n\nimport _ "github.com/a2aproject/a2a-go/v2/a2a"\n'),
  ],
  tests=[('ANet:./cmd/anet', 'TestABuildWithoutA2ADoesNotClaimIt', 'no_a2a'), ('ANet:sh bash scripts/tagcheck.sh check no_a2a', '')],
  full=[],
 ),
 dict(
  id='si8-2', group='SI-8', repo='ANet',
  desc='internal/mcpserv 引入 a2a-go',
  edits=[
   ('internal/mcpserv/zz_mut.go', None, '//go:build !no_mcp\n\npackage mcpserv\n\nimport _ "github.com/a2aproject/a2a-go/v2/a2a"\n'),
  ],
  tests=[('ANet:./internal/mcpserv', 'TestSI8NoA2AGoInTheClosure'), ('ANet:./internal/loopguard', 'TestSharedPackagesStayFreeOfTheDaemonAndTheSDK')],
  full=[],
 ),
 dict(
  id='si8-3', group='SI-8', repo='ANet',
  desc='internal/daemon 引入 a2a-go',
  edits=[
   ('internal/daemon/zz_mut.go', None, 'package daemon\n\nimport _ "github.com/a2aproject/a2a-go/v2/a2a"\n'),
  ],
  tests=[('ANet:./internal/mcpserv', 'TestSI8NoA2AGoInTheClosure'), ('ANet:./internal/loopguard', 'TestSharedPackagesStayFreeOfTheDaemonAndTheSDK')],
  full=[],
 ),
 dict(
  id='si8-4', group='SI-8', repo='ANet',
  desc='taskboard 改回减法 tag(module_taskboard.go 与 module/taskboard 默认编入)',
  edits=[
   ('cmd/anet/module_taskboard.go', '//go:build taskboard\n', '//go:build !no_taskboard\n'),
   ('module/taskboard/taskboard.go', '//go:build taskboard\n', '//go:build !no_taskboard\n'),
  ],
  tests=[('ANet:./cmd/anet', 'TestTheDefaultBuildHasNoTaskboard'), ('ANet:sh bash scripts/tagcheck.sh check taskboard', '')],
  full=[],
 ),
 dict(
  id='si8-5', group='SI-8', repo='ANetHub',
  desc='anet-hub wire_taskboard.go 改回默认编入',
  edits=[
   ('cmd/anet-hub/wire_taskboard.go', '//go:build taskboard\n', '//go:build !no_taskboard\n'),
   ('internal/taskboard/http.go', '//go:build taskboard\n', '//go:build !no_taskboard\n'),
   ('internal/taskboard/taskboard.go', '//go:build taskboard\n', '//go:build !no_taskboard\n'),
  ],
  tests=[('ANetHub:./cmd/anet-hub', 'TestTheDefaultBuildHasNoTaskboard')],
  full=[],
 ),
 dict(
  id='si8-6', group='SI-8', repo='ANet',
  desc='internal/a2ashape 引入 a2a-go(控制面/MCP 经它间接依赖)',
  edits=[
   ('internal/a2ashape/zz_mut.go', None, 'package a2ashape\n\nimport _ "github.com/a2aproject/a2a-go/v2/a2a"\n'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestNoSDKImport'), ('ANet:./internal/mcpserv', 'TestSI8NoA2AGoInTheClosure')],
  full=[],
 ),
 dict(
  id='si8-7', group='SI-8', repo='ANet',
  desc='module/a2a 各文件去掉 !no_a2a tag,且 cmd/anet/module_a2a.go 恒编入',
  edits=[
   ('cmd/anet/module_a2a.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/addr.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/backend.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/card.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/convert.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/handler.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/module.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/server.go', '//go:build !no_a2a\n', ''),
   ('module/a2a/kelresolver/kelresolver.go', '//go:build !no_a2a\n', ''),
  ],
  tests=[('ANet:./cmd/anet', 'TestABuildWithoutA2ADoesNotClaimIt', 'no_a2a'), ('ANet:sh bash scripts/tagcheck.sh check no_a2a', '')],
  full=[],
 ),
 dict(
  id='si9-1', group='SI-9', repo='ANetHub',
  desc='CheckRequirements: 少付 1 仍通过',
  edits=[
   ('internal/aghub/facilitator.go', '\tif auth.Amount < want {', '\tif auth.Amount+1 < want {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestSettleRefusesPaymentsThatDoNotMeetTheRequirements|TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded'), ('ANetHub:./cmd/anet-hub', 'TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce')],
  full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='si9-2', group='SI-9', repo='ANetHub',
  desc='CheckRequirements: 不比较授权收款方与 requirements',
  edits=[
   ('internal/aghub/facilitator.go', '\tif auth.PayTo != req.PayTo {', '\tif false && auth.PayTo != req.PayTo {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestSettleRefusesPaymentsThatDoNotMeetTheRequirements|TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded'), ('ANetHub:./cmd/anet-hub', 'TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce')],
  full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='si9-3', group='SI-9', repo='ANetHub',
  desc='入口 hub 转发前不核对 requirements',
  edits=[
   ('internal/aghub/facilitator.go', '\t\tif rf := CheckRequirements(p, auth, req); rf != nil {\n\t\t\treturn refusedSettlement(rf, auth, p.Accepted.Network)', '\t\tif rf := CheckRequirements(p, auth, req); rf != nil && false {\n\t\t\treturn refusedSettlement(rf, auth, p.Accepted.Network)'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded'), ('ANetHub:./cmd/anet-hub', 'TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce')],
  full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='si9-4', group='SI-9', repo='ANetHub',
  desc='账本 hub 不核对 requirements',
  edits=[
   ('internal/aghub/facilitator.go', '\tif rf := CheckRequirements(p, auth, req); rf != nil {\n\t\treturn refusedSettlement(rf, auth, own)', '\tif rf := CheckRequirements(p, auth, req); rf != nil && false {\n\t\treturn refusedSettlement(rf, auth, own)'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestSettleRefusesPaymentsThatDoNotMeetTheRequirements'), ('ANetHub:./cmd/anet-hub', 'TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce')],
  full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='si9-5', group='SI-9', repo='ANetHub',
  desc='新结算不查授权有效期与当前密钥(settle 路径的 currentAuth)',
  edits=[
   ('internal/aghub/facilitator.go', '\tif rf := currentAuth(auth, kel, time.Now().UnixMilli()); rf != nil {\n\t\treturn refusedSettlement(rf, auth, own)', '\tif rf := currentAuth(auth, kel, time.Now().UnixMilli()); rf != nil && false {\n\t\treturn refusedSettlement(rf, auth, own)'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestPaymentRefusals|TestSettleRefusesPaymentsThatDoNotMeetTheRequirements|TestAChargedAuthorizationGetsItsReceiptAfterItsWindow')],
  full=['ANetHub:./internal/aghub', 'ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='si9-6', group='SI-9', repo='ANetHub',
  desc='绑定唯一索引改为普通索引(同一绑定可两次扣款)',
  edits=[
   ('internal/aghub/facilitator.go', 'CREATE UNIQUE INDEX IF NOT EXISTS idx_settled_binding', 'CREATE INDEX IF NOT EXISTS idx_settled_binding'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestASecondAuthorizationForOneBindingMovesNothing|TestUpgradingAHubWithDuplicateBindingsKeepsTheFirstAsHolder')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='si9-7', group='SI-9', repo='ANet',
  desc='商户核对: 不查收款方是本节点',
  edits=[
   ('module/x402/check.go', '\tif auth.PayTo != self || pp.Accepted.PayTo != auth.PayTo {', '\tif false && (auth.PayTo != self || pp.Accepted.PayTo != auth.PayTo) {'),
  ],
  tests=[('ANet:./module/x402', 'TestTheMerchantCheckRefusesEachWrongTerm'), ('ANet:./internal/daemon', 'TestAWrongPaymentIsRefusedBeforeSettlement')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si9-8', group='SI-9', repo='ANet',
  desc='商户核对: 不查绑定值 pay_bind',
  edits=[
   ('module/x402/check.go', '\tif t.Bind == "" || auth.InteractionID != t.Bind {', '\tif t.Bind == "" {'),
  ],
  tests=[('ANet:./module/x402', 'TestTheMerchantCheckRefusesEachWrongTerm'), ('ANet:./internal/daemon', 'TestAWrongPaymentIsRefusedBeforeSettlement')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si9-9', group='SI-9', repo='ANet',
  desc='商户核对: 不查金额 ≥ 报价',
  edits=[
   ('module/x402/check.go', 'if err != nil || accepted != auth.Amount || auth.Amount < want {', 'if err != nil || accepted != auth.Amount {'),
  ],
  tests=[('ANet:./module/x402', 'TestTheMerchantCheckRefusesEachWrongTerm'), ('ANet:./internal/daemon', 'TestAWrongPaymentIsRefusedBeforeSettlement')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si9-10', group='SI-9', repo='ANet',
  desc='商户核对: 不查授权过期',
  edits=[
   ('module/x402/check.go', '\tif auth.NotAfter <= auth.IssuedAt || t.Now > auth.NotAfter+payment.ClockSkew ||', '\tif auth.NotAfter <= auth.IssuedAt ||'),
  ],
  tests=[('ANet:./module/x402', 'TestTheMerchantCheckRefusesEachWrongTerm'), ('ANet:./internal/daemon', 'TestAWrongPaymentIsRefusedBeforeSettlement')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si9-11', group='SI-9', repo='ANet',
  desc='商户核对: 不查报价过期',
  edits=[
   ('module/x402/check.go', '\tif t.QuoteExpiresAt > 0 && t.Now > t.QuoteExpiresAt {', '\tif false && t.QuoteExpiresAt > 0 && t.Now > t.QuoteExpiresAt {'),
  ],
  tests=[('ANet:./module/x402', 'TestTheMerchantCheckRefusesEachWrongTerm'), ('ANet:./internal/daemon', 'TestALapsedQuoteFailsTheTask')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='si9-12', group='SI-9', repo='ANet',
  desc='provider 接受他笔授权的结算回执(不比 AuthID)',
  edits=[
   ('internal/daemon/x402task.go', '\t\tif len(ix.PayAuthIDs) == 0 || facts.AuthID != ix.PayAuthIDs[0] {', '\t\tif len(ix.PayAuthIDs) == 0 || (facts.AuthID != ix.PayAuthIDs[0] && false) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAReceiptForAnotherAuthorizationIsNotVerified|TestASecondSettlementForOneTaskIsMarked|TestAnUnknownSettlementIsRetriedAndChargedOnce|TestAReplayedSettlementOfAnotherPaymentIsRefused')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-1', group='SI-10', repo='ANet',
  desc='rxResult.ack 恒 true(暂时性失败也 ack)',
  edits=[
   ('internal/daemon/receive.go', 'func (r rxResult) ack() bool { return r.class != rxTransient }', 'func (r rxResult) ack() bool { return true }'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStoreFailureThenRedeliveryIsProcessedOnce|TestTheP2PRateLimitIsTemporary|TestAPeerRecordReadErrorIsTemporary|TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-2', group='SI-10', repo='ANet',
  desc='p2p deliverInbound: Receive 失败仍回 ack',
  edits=[
   ('module/p2p/transport.go', '\t\tlog.Printf("anet: p2p: inbound delivery %s not acknowledged: %v", f.ID, err)\n\t\treturn\n', '\t\tlog.Printf("anet: p2p: inbound delivery %s not acknowledged: %v", f.ID, err)\n'),
  ],
  tests=[('ANet:./module/p2p', 'TestATemporaryRefusalIsNotAcked')],
  full=['ANet:./module/p2p'],
 ),
 dict(
  id='si10-3', group='SI-10', repo='ANet',
  desc='p2p ack 键改常量(设计 §3.10 指定的 mutation)',
  edits=[
   ('module/p2p/transport.go', '_ = t.write(c, frame{Op: opAck, V: WireVersion, ID: f.ID})', '_ = t.write(c, frame{Op: opAck, V: WireVersion, ID: "1"})'),
  ],
  tests=[('ANet:./module/p2p', 'TestConcurrentDeliveriesAreAckedByTheirOwnID')],
  full=['ANet:./module/p2p'],
 ),
 dict(
  id='si10-4', group='SI-10', repo='ANet',
  desc='process: 去掉 (from, mid) 进程内锁',
  edits=[
   ('internal/daemon/receive.go', '\tunlock := d.rxLocks.lock(replayKey(m.from, m.mid))\n', '\tunlock := func() {}\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheSameEnvelopeOverP2PAndHubIsProcessedOnce|TestADelegationSealedTwiceArrivingTogetherIsRecordedOnce|TestAResultSealedTwiceArrivingTogetherIsRecordedOnce|TestASecondCopyWaitsForTheFirstUnderTheMessageLock')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-5', group='SI-10', repo='ANet',
  desc='commitRx: 存储错误按永久失败(ack 并丢弃)',
  edits=[
   ('internal/daemon/receive.go', '\t\treturn d.drop(perm.reason, perm.err)\n\tdefault:\n\t\treturn d.transient(transientStore, err)', '\t\treturn d.drop(perm.reason, perm.err)\n\tdefault:\n\t\treturn d.drop(transientStore, err)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStoreFailureThenRedeliveryIsProcessedOnce|TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-6', group='SI-10', repo='ANet',
  desc='pollOnce: 暂缓的信封也 ack',
  edits=[
   ('internal/daemon/relay.go', '\t\tif d.receiveEnvelope(ctx, env).ack() {', '\t\tif d.receiveEnvelope(ctx, env).ack() || true {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStoreFailureThenRedeliveryIsProcessedOnce|TestHeldBackEnvelopesDoNotBlockNewerMail|TestAHeldEnvelopeIsRetriedAtOnePollPerRound|TestAMessageForAnUnknownTaskGetsTaskNotFound')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-7', group='SI-10', repo='ANet',
  desc='p2p 第 0 步限速返回 nil(被限速的投递被 ack)',
  edits=[
   ('internal/daemon/transport.go', '\t\tin.d.count(transientP2PRate)\n\t\treturn errP2PRateLimited', '\t\tin.d.count(transientP2PRate)\n\t\treturn nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheP2PRateLimitIsTemporary')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='si10-8', group='SI-10', repo='ANet',
  desc='anetpeer: 收到任一 ack 释放全部等待中的投递',
  edits=[
   ('tools/anetpeer/main.go', '\t\t\tif ch, ok := p.acks[f.ID]; ok {\n\t\t\t\tclose(ch)\n\t\t\t\tdelete(p.acks, f.ID)\n\t\t\t}', '\t\t\tfor id, ch := range p.acks {\n\t\t\t\tclose(ch)\n\t\t\t\tdelete(p.acks, id)\n\t\t\t}'),
  ],
  tests=[('ANet:./tools/anetpeer', 'TestConcurrentDeliveriesGetTheirOwnOutcome')],
  full=['ANet:./tools/anetpeer'],
 ),
]
