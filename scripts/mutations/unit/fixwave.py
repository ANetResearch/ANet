# fixwave.py: mutations of the round-5b red-team fix wave (redteam F1..F41, si9), the 0017 Q28-Q33 changes and
# the 0030 N1 fix (service and A2A backends on Unix sockets, internal/backendconn),
# for scripts/mutations/unitmut.py (see its header for the format). Written for docs/notes/0029: each fix's
# guard is taken out (or put back the way it was before the fix) and its regression test must go red.
# A mutation that no longer applies reports APPLY-ERR: fix its edit to the moved code rather than dropping it.

MUTS = [
 dict(
  id='fx5-1', group='F5', repo='ANet',
  desc='第 8½ 步不查持久拒收表(exact 恒 false)',
  edits=[
   ('internal/daemon/receive.go', '\t\tif exact {\n\t\t\td.refused.add(key)', '\t\tif false && exact {\n\t\t\td.refused.add(key)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARefusedDelegationStaysRefusedAfterAllowAndRestart|TestARefusedDelegationStaysRefusedWhenTheListForgetsIt|TestAHubReplayOfARefusedDelegationDoesNotRun')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx5-2', group='F5', repo='ANet',
  desc='拒收下限不起作用(inner.ts <= floor 仍判定)',
  edits=[
   ('internal/daemon/receive.go', '\tif m.ts <= floor {', '\tif false && m.ts <= floor {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnEvictedRefusalIsCoveredByTheFloor|TestOtherSendersRefusalsDoNotDropAPeersDelegation')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx5-3', group='F5', repo='ANet',
  desc='拒收落盘失败仍回 rejected(recordRefusal 忽略写错误)',
  edits=[
   ('internal/daemon/receive.go', '\tif err := d.ix.RecordRefused(m.from, m.mid, m.ts, m.exp); err != nil {\n\t\tr := d.transient(', '\tif err := d.ix.RecordRefused(m.from, m.mid, m.ts, m.exp); false && err != nil {\n\t\tr := d.transient('),
  ],
  tests=[('ANet:./internal/daemon', 'TestARefusalIsStoredBeforeTheRequesterIsTold')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-1', group='F6/F7', repo='ANet',
  desc='第 9 步:已存在 ix 的 delegate 不核对 request CID',
  edits=[
   ('internal/daemon/receive.go', '\tif ix.RequestCID == "" || ix.RequestCID != m.requestCID {', '\tif false && (ix.RequestCID == "" || ix.RequestCID != m.requestCID) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARedeliveryUnderOpenCannotSwapInACapability|TestARegisteredStrangerCannotSwapACapabilityIntoItsOpenTask|TestAnApprovedChatCannotBeSwappedForACapability|TestARedeliveryOnAMigratedRowCannotRunACapability|TestAQuotedPublicCallCannotBeSwappedForAPrivateCapability')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-1b', group='F6/F7', repo='ANet',
  desc='fx7-1 与 fx7-3 两层一起去掉(第 9 步与第 10 步事务内都不核对 request CID)',
  edits=[
   ('internal/daemon/receive.go', '\tif ix.RequestCID == "" || ix.RequestCID != m.requestCID {', '\tif false && (ix.RequestCID == "" || ix.RequestCID != m.requestCID) {'),
   ('internal/daemon/delegation.go', '\t\t\tif prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile', '\t\t\tif false && prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARedeliveryUnderOpenCannotSwapInACapability|TestARegisteredStrangerCannotSwapACapabilityIntoItsOpenTask|TestAnApprovedChatCannotBeSwappedForACapability|TestARedeliveryOnAMigratedRowCannotRunACapability|TestAQuotedPublicCallCannotBeSwappedForAPrivateCapability')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-2', group='F6/F7', repo='ANet',
  desc='重投不再对 trust=peer 交互重查 allow',
  edits=[
   ('internal/daemon/receive.go', '\tif ix.Trust == interactions.TrustPeer && !ps.allowed(m.from) {\n\t\tr := d.drop(dropNotAllowed, nil)\n\t\treturn &r\n\t}\n\tm.existing, m.trust = ix, ix.Trust', '\tif false && ix.Trust == interactions.TrustPeer && !ps.allowed(m.from) {\n\t\tr := d.drop(dropNotAllowed, nil)\n\t\treturn &r\n\t}\n\tm.existing, m.trust = ix, ix.Trust'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARevokedPeerCannotUseItsOldInteraction')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-3', group='F6/F7', repo='ANet',
  desc='第 10 步事务内:并发副本建的交互不核对 request CID',
  edits=[
   ('internal/daemon/delegation.go', '\t\t\tif prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile', '\t\t\tif false && prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile'),
  ],
  tests=[('ANet:./internal/daemon', 'TestConcurrentCopiesOfAPublicCallAreJudgedOnce|TestADelegationSealedTwiceAtTheQuotaEdgeIsJudgedOnce|TestARedeliveryRunsTheRecordedCallNotTheEnvelopes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-3b', group='F6/F7/F26', repo='ANet',
  desc='fx7-3 与 (from, ix) 锁一起去掉(两层)',
  edits=[
   ('internal/daemon/delegation.go', '\t\t\tif prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile', '\t\t\tif false && prior.RequestCID != requestCID {\n\t\t\t\t// Created meanwhile'),
   ('internal/daemon/receive.go', '\t\tunlockIX := d.rxIXLocks.lock(m.from + "\\x00" + m.ix)\n', '\t\tunlockIX := func() {}\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestConcurrentCopiesOfAPublicCallAreJudgedOnce|TestADelegationSealedTwiceAtTheQuotaEdgeIsJudgedOnce|TestARedeliveryRunsTheRecordedCallNotTheEnvelopes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-4', group='F6/F7', repo='ANet',
  desc='重投路径执行新信封 TaskDoc 的能力(不取存储的 request_doc)',
  edits=[
   ('internal/daemon/delegation.go', '\tcapID, args, ok := recordedCall(prior)\n\tif !ok {\n\t\treturn true\n\t}\n\tif d.longCall(capID) {', '\tcapID, args, ok := capabilityCall(m.td)\n\tif !ok {\n\t\treturn true\n\t}\n\tif d.longCall(capID) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARedeliveryRunsTheRecordedCallNotTheEnvelopes|TestAnApprovedCallRedeliveredIsAnsweredAgainNotRun')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx7-5', group='F6/F7', repo='ANet',
  desc='启动恢复重跑前不对 trust=peer 交互重查 allow',
  edits=[
   ('internal/daemon/capability.go', '\t\t\t\tif ix.Trust == interactions.TrustPeer && !ps.allowed(ix.PeerAID) {', '\t\t\t\tif false && ix.Trust == interactions.TrustPeer && !ps.allowed(ix.PeerAID) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestStartupRecoveryRunsApprovedCallsAndLeavesUnpaidOrDeniedOnes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx26-1', group='F26', repo='ANet',
  desc='去掉 (from, ix) 锁:同一委派两个 mid 的并发副本各自判定',
  edits=[
   ('internal/daemon/receive.go', '\t\tunlockIX := d.rxIXLocks.lock(m.from + "\\x00" + m.ix)\n', '\t\tunlockIX := func() {}\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestADelegationSealedTwiceAtTheQuotaEdgeIsJudgedOnce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx22-1', group='F22', repo='ANet',
  desc='直连路径上打不开的信封判永久(drop 并 ack)',
  edits=[
   ('internal/daemon/receive.go', '\t\tif path.direct {\n\t\t\t// [redteam:F22]', '\t\tif false && path.direct {\n\t\t\t// [redteam:F22]'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_P2PMisrouteFallsBackToTheHub')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-1', group='F23', repo='ANet',
  desc='outbox 不按任务 FIFO(去掉队头条件)',
  edits=[
   ('internal/runtime/interactions/outbox.go', 'const outboxHead = `NOT EXISTS (SELECT 1 FROM outbox p WHERE p.ix = o.ix AND p.to_aid = o.to_aid AND p.id < o.id)`', 'const outboxHead = `1=1`'),
  ],
  tests=[('ANet:./internal/runtime/interactions', 'TestOutboxDeliversATasksMessagesInOrder'), ('ANet:./internal/daemon', 'TestRedteamSI10_ACancelWaitsBehindADelegationThatMayHaveArrived')],
  full=['ANet:./internal/runtime/interactions', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-2', group='F23', repo='ANet',
  desc='取消不撤回尚未送出的委派',
  edits=[
   ('internal/daemon/delegation.go', '\tif row == nil || row.MaybeDelivered {\n\t\treturn false, 0, nil\n\t}', '\tif true || row == nil || row.MaybeDelivered {\n\t\treturn false, 0, nil\n\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_ACancelBeforeDeliveryWithdrawsTheDelegation')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-3', group='F23/F12', repo='ANet',
  desc='可能已送达的委派也被取消撤回(不看 MaybeDelivered)',
  edits=[
   ('internal/daemon/delegation.go', '\tif row == nil || row.MaybeDelivered {\n\t\treturn false, 0, nil\n\t}', '\tif row == nil {\n\t\treturn false, 0, nil\n\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_ACancelWaitsBehindADelegationThatMayHaveArrived|TestADelegationWhoseAttemptWasNeverRecordedIsNotWithdrawn')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-3b', group='F23/F12', repo='ANet',
  desc='fx23-3 与事务内的 maybe_delivered 复核一起去掉(两层)',
  edits=[
   ('internal/daemon/delegation.go', '\tif row == nil || row.MaybeDelivered {\n\t\treturn false, 0, nil\n\t}', '\tif row == nil {\n\t\treturn false, 0, nil\n\t}'),
   ('internal/daemon/delegation.go', '\t\tif err != nil || !queued || maybe {', '\t\tif err != nil || !queued || (maybe && false) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_ACancelWaitsBehindADelegationThatMayHaveArrived|TestADelegationWhoseAttemptWasNeverRecordedIsNotWithdrawn')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-4', group='F23', repo='ANet',
  desc='放弃未送达委派时不连带删除其后排队的消息',
  edits=[
   ('internal/daemon/retry.go', '\t\tif it.Type == seal.TypeDelegate && !it.MaybeDelivered {', '\t\tif false && it.Type == seal.TypeDelegate && !it.MaybeDelivered {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnAbandonedDelegationTakesItsQueueWithIt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx23-5', group='F23', repo='ANet',
  desc='重投只催结果行,不催整条答复队列',
  edits=[
   ('internal/daemon/delegation.go', '\t\t\tfor _, r := range queue {\n\t\t\t\td.hurryQueued(r, keys)\n\t\t\t}', '\t\t\tfor _, r := range queue {\n\t\t\t\tif r.Type == seal.TypeResult {\n\t\t\t\t\td.hurryQueued(r, keys)\n\t\t\t\t}\n\t\t\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARedeliveredDelegationHurriesTheWholeAnswerQueue')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx12-1', group='F12', repo='ANet',
  desc='可能已送达后放弃的能力调用判 UNAVAILABLE',
  edits=[
   ('internal/daemon/undelivered.go', '\t\tif it.MaybeDelivered {\n\t\t\tm[a2ashape.KeyEffectStatus] = string(effect.Unverified)', '\t\tif false && it.MaybeDelivered {\n\t\t\tm[a2ashape.KeyEffectStatus] = string(effect.Unverified)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnAbandonedDelegationThatMayHaveArrivedIsNotUnavailable|TestRedteamSI6_AbandonedButDeliveredDelegationIsNotUnavailable')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx12-2', group='F12/F23', repo='ANet',
  desc='外发尝试不先记"尝试中"',
  edits=[
   ('internal/daemon/retry.go', '\tif err := d.ix.BeginOutboxAttempt(it.ID); err != nil {\n\t\treturn err\n\t}\n', ''),
  ],
  tests=[('ANet:./internal/daemon', 'TestADelegationWhoseAttemptWasNeverRecordedIsNotWithdrawn')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx24-1', group='F24', repo='ANet',
  desc='信箱游标只在追上后回头(去掉 relayHeadEvery)',
  edits=[
   ('internal/daemon/relay.go', '\tif c.caughtUp || c.sinceBack >= relayHeadEvery {', '\tif c.caughtUp {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_ASteadyStreamDoesNotPinTheRelayCursor|TestHeldPagesDoNotStarveNewMail')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx24-2', group='F24', repo='ANet',
  desc='回头读每次都从队头开始(不逐页前进)',
  edits=[
   ('internal/daemon/relay.go', '\tcase held || full:\n\t\t// Read on past a page that held something back, and past a full\n\t\t// one, which may have more behind it.\n\t\tc.back = last', '\tcase held || full:\n\t\t// Read on past a page that held something back, and past a full\n\t\t// one, which may have more behind it.\n\t\tc.back = 0'),
  ],
  tests=[('ANet:./internal/daemon', 'TestHeldPagesDoNotStarveNewMail|TestRedteamSI10_ASteadyStreamDoesNotPinTheRelayCursor')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx25-1', group='F25', repo='ANet',
  desc='陌生人的未知 ix 消息也获等待窗',
  edits=[
   ('internal/daemon/receive.go', '\tif !related {\n\t\treturn false, nil\n\t}\n\tif !d.heldEarly.admit(', '\tif false && !related {\n\t\treturn false, nil\n\t}\n\tif !d.heldEarly.admit('),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_AStrangerCannotKeepAnOnlineNodesMailboxFull|TestRedteamUnknownIXFromAStrangerIsNotReworked')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx25-2', group='F25', repo='ANet',
  desc='每发送方暂扣上限失效',
  edits=[
   ('internal/daemon/receive.go', '\tif !d.heldEarly.admit(', '\tif false && !d.heldEarly.admit('),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_AKnownPeerHoldsOnlySoManyEarlyMessages')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx25-3', group='F25', repo='ANet',
  desc='直连路径上指向未知 ix 的 message 不再暂拒(按信箱规则)',
  edits=[
   ('internal/daemon/receive.go', '\t\tif m.direct {\n\t\t\tr := d.transient(transientDirectUnknownIX, nil)', '\t\tif false && m.direct {\n\t\t\tr := d.transient(transientDirectUnknownIX, nil)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestADirectFollowUpThatOvertakesItsDelegationArrivesAfterIt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx27-1', group='F27', repo='ANet',
  desc='入站附件写入错误被忽略(事务照常提交)',
  edits=[
   ('internal/daemon/attachments.go', '\t\tif err := tx.AddAttachment(interactionID, msgSeq, r); err != nil {\n\t\t\treturn err\n\t\t}', '\t\t_ = tx.AddAttachment(interactionID, msgSeq, r)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_AttachmentWriteFailureLeavesTheMessageForRedelivery|TestRedteamSI10_ADelegationsFileWriteFailureLeavesItForRedelivery')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx28-1', group='F28', repo='ANet',
  desc='requester 的报价/付款状态 pay 列写入错误被忽略',
  edits=[
   ('internal/daemon/delegation.go', '\t\tif pp != nil {\n\t\t\treturn pp.applyTx(tx, m.ix)\n\t\t}\n\t\treturn nil\n\t})', '\t\tif pp != nil {\n\t\t\t_ = pp.applyTx(tx, m.ix)\n\t\t}\n\t\treturn nil\n\t})'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRedteamSI10_QuoteWriteFailureLeavesTheStatusForRedelivery')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx29-1', group='F29', repo='ANet',
  desc='已答复 delegate 的重投不限速',
  edits=[
   ('internal/daemon/delegation.go', '\t\tif !d.resends.allow(prior.PeerAID, prior.ID, d.nowMS()) {', '\t\tif false && !d.resends.allow(prior.PeerAID, prior.ID, d.nowMS()) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestReplaysOfAnAnsweredDelegationAreNotEachAnswered|TestTheResendLimitIsPerInteractionAndRefills')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx30-1', group='F30', repo='ANet',
  desc='直连入站不等启动完成(awaitReady 立即放行)',
  edits=[
   ('internal/daemon/transport.go', '\tif d.ready == nil {\n\t\treturn nil // not built by New', '\tif true || d.ready == nil {\n\t\treturn nil // not built by New'),
  ],
  tests=[('ANet:./internal/daemon', 'TestADeliveryDuringStartupWaitsForRecovery|TestRedteamSI10_StartupRecoveryLeavesACallRunningNowAlone')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx30-2', group='F30', repo='ANet',
  desc='直连入站先等启动完成再过限速',
  edits=[
   ('internal/daemon/transport.go', '\tif !in.d.p2pLimit.allow(in.d.nowMS()) {\n\t\tin.d.count(transientP2PRate)\n\t\treturn errP2PRateLimited\n\t}\n\tif err := in.d.awaitReady(ctx); err != nil {\n\t\treturn err\n\t}', '\tif err := in.d.awaitReady(ctx); err != nil {\n\t\treturn err\n\t}\n\tif !in.d.p2pLimit.allow(in.d.nowMS()) {\n\t\tin.d.count(transientP2PRate)\n\t\treturn errP2PRateLimited\n\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheDirectRateLimitHoldsDuringStartUp')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q29-1', group='Q29', repo='ANet',
  desc='直连投递等整条管线结束才 ack(不在第 10 步提交时)',
  edits=[
   ('internal/daemon/transport.go', '\tselect {\n\tcase <-committed:\n\t\treturn nil\n\tcase res := <-done:', '\tselect {\n\tcase <-make(chan struct{}):\n\t\treturn nil\n\tcase res := <-done:'),
  ],
  tests=[('ANet:./internal/daemon', 'TestADirectDeliveryIsAcknowledgedAtItsCommit')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q29-2', group='Q29', repo='ANet',
  desc='已在第 10 步提交的直连投递,管线返回暂时性结果时回 nack',
  edits=[
   ('internal/daemon/transport.go', '\tselect {\n\tcase <-committed:\n\t\treturn nil\n\tdefault:\n\t}\n', ''),
  ],
  tests=[('ANet:./internal/daemon', 'TestACommittedDirectDeliveryIsNeverRefused')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx9-1', group='F9', repo='ANet',
  desc='SetInboundPolicy 先生效后保存(保存失败仍在生效)',
  edits=[
   ('internal/daemon/inbound.go', '\tif err := SaveConfig(d.layout, next); err != nil {\n\t\treturn err\n\t}\n\td.mu.Lock()\n\td.cfg.Inbound = next.Inbound\n\td.mu.Unlock()\n\td.recordPolicyChange("inbound.policy"', '\td.mu.Lock()\n\td.cfg.Inbound = next.Inbound\n\td.mu.Unlock()\n\tif err := SaveConfig(d.layout, next); err != nil {\n\t\treturn err\n\t}\n\td.recordPolicyChange("inbound.policy"'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAPolicyWriteThatFailedToSaveIsNotInForce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx9-2', group='F9', repo='ANet',
  desc='SetInboundPolicy 不取 cfgWrite 锁(与其他 config 写入并发)',
  edits=[
   ('internal/daemon/inbound.go', '\td.cfgWrite.Lock()\n\tdefer d.cfgWrite.Unlock()\n\tnext := d.config()\n\tin := next.inbound()\n\tfrom := in.Policy', '\tnext := d.config()\n\tin := next.inbound()\n\tfrom := in.Policy'),
  ],
  tests=[('ANet:./internal/daemon', 'TestConcurrentConfigWritesLeaveDiskAndMemoryAgreeing')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx10-1', group='F10', repo='ANet',
  desc='网关代答 502/504 报 FAILED(不是"效果未知")',
  edits=[
   ('module/service/service.go', '\tif resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusBadGateway {', '\tif false && (resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusBadGateway) {'),
  ],
  tests=[('ANet:./module/service', 'TestAGatewayAnsweringForTheServiceIsAnUnknownOutcome')],
  full=['ANet:./module/service'],
 ),
 dict(
  id='fx10-2', group='F10', repo='ANet',
  desc='2xx 之后应答体中断报 FAILED',
  edits=[
   ('module/service/service.go', '\tif rerr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {', '\tif false && rerr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {'),
  ],
  tests=[('ANet:./module/service', 'TestAnAnswerThatBreaksOffAfterItsHeadersIsNotFailed')],
  full=['ANet:./module/service'],
 ),
 dict(
  id='fx11-1', group='F11', repo='ANet',
  desc='同一任务的收据核验不加锁(并发两份各记一笔)',
  edits=[
   ('internal/daemon/x402task.go', '\tunlock := d.outboxLocks.lock("receipts:" + ixID)\n\t// Held past', '\tunlock := func() {}\n\t// Held past'),
  ],
  tests=[('ANet:./internal/daemon', 'TestOneSettlementDeliveredTwiceAtOnceIsRecordedOnce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx11-2', group='F11', repo='ANet',
  desc='SplitReceipts 对外发任务也原样采信对端收据',
  edits=[
   ('internal/a2ashape/receipts.go', '\tif !outbound {\n\t\treturn list, nil\n\t}', '\tif true || !outbound {\n\t\treturn list, nil\n\t}'),
  ],
  tests=[('ANet:./internal/a2ashape', ''), ('ANet:./internal/daemon', 'TestRedteamSI6PeerReceiptsAreNotProjectedAsPaymentCompleted|TestAVerifiedSettlementStaysAndAForgedOneIsNotStated|TestAProviderCannotSayItVerifiedAPaymentThisNodeNeverMade')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx15-1', group='F15', repo='ANetCore',
  desc='VerifyResultForRequest 不比回执的 RequestCID',
  edits=[
   ('delegation/delegation.go', '\tif requestCID != "" && rc.RequestCID != requestCID {', '\tif false && requestCID != "" && rc.RequestCID != requestCID {'),
  ],
  tests=[('ANetCore:./delegation', 'TestVerifyResultForRequestBindsTheRequest'), ('ANetCore:./golden', 'TestVEC_RESULT_2_RequestBinding')],
  full=['ANetCore:./delegation', 'ANetCore:./golden'],
 ),
 dict(
  id='fx15-2', group='F15', repo='ANet',
  desc='requester 收结果时不传 request CID',
  edits=[
   ('internal/daemon/delegation.go', 'delegation.VerifyResultForRequest(rr, m.kel, m.ix, d.AID(), ix.PeerAID, ix.RequestCID, m.ts)', 'delegation.VerifyResultForRequest(rr, m.kel, m.ix, d.AID(), ix.PeerAID, "", m.ts)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAReceiptForAnotherRequestIsRefused')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx15-3', group='F15', repo='ANet',
  desc='签评价前不核对回执的 request CID',
  edits=[
   ('internal/daemon/delegation.go', '\tcase ix.RequestCID != "" && rc.RequestCID != ix.RequestCID:', '\tcase false && ix.RequestCID != "" && rc.RequestCID != ix.RequestCID:'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAReviewIsNotAnchoredToAReceiptForAnotherRequest')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx15-4', group='F15', repo='ANet',
  desc='anet verify(在线)不核对 request CID',
  edits=[
   ('cmd/anet/verify.go', 'return report(r.Receipt, r.ProviderKEL, r.ResultCID, "", r.RequestCID, "")', 'return report(r.Receipt, r.ProviderKEL, r.ResultCID, "", "", "")'),
  ],
  tests=[('ANet:./cmd/anet', 'TestVerify|TestOfflineVerificationBindsTheRequest|TestStoredVerificationBindsTheRequestThisNodeSent')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='fx18-1', group='F18', repo='ANet',
  desc='localpeer.Transport 不核实监听者就发令牌',
  edits=[
   ('internal/localpeer/localpeer.go', '\t\t\tif err := Verify(ctx, c, token); err != nil {', '\t\t\tif err := Verify(ctx, c, token); false && err != nil {'),
  ],
  tests=[('ANet:./internal/localpeer', ''), ('ANet:./cmd/anet', 'TestUpDoesNotSendTheControlTokenToAPortSquatter|TestNoCLIPathSendsTheControlTokenToAPortSquatter'), ('ANet:./internal/daemon', 'TestConsoleSwitchDoesNotSendTheTargetTokenToAPortSquatter')],
  full=['ANet:./internal/localpeer', 'ANet:./cmd/anet', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx18-2', group='F18', repo='ANet',
  desc='localpeer.Verify 把他人的监听者当本用户',
  edits=[
   ('internal/localpeer/localpeer.go', '\tcase err == nil:\n\t\treturn &NotOursError{Addr: far.String(), UID: uid}', '\tcase err == nil:\n\t\treturn nil'),
  ],
  tests=[('ANet:./internal/localpeer', ''), ('ANet:./cmd/anet', 'TestUpDoesNotSendTheControlTokenToAPortSquatter|TestNoCLIPathSendsTheControlTokenToAPortSquatter')],
  full=['ANet:./internal/localpeer', 'ANet:./cmd/anet'],
 ),
 dict(
  id='fx18-3', group='F18', repo='ANet',
  desc='控制口被他人占用时不轮换控制令牌',
  edits=[
   ('internal/daemon/control_api.go', '\tif taken && !portHeldByThisUser(from, token) {', '\tif false && taken && !portHeldByThisUser(from, token) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheControlTokenAnotherUserCollectedIsWorthlessOnceTheDaemonMoves|TestTheControlTokenIsReplacedWhenAChosenPortIsTaken')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx18-4', group='F18', repo='ANet',
  desc='本机 A2A 接口换新地址时不换令牌',
  edits=[
   ('module/a2a/module.go', '\tif fresh && hadToken {', '\tif false && fresh && hadToken {'),
  ],
  tests=[('ANet:./module/a2a', 'TestNewAddressComesWithANewToken|TestTakenA2APortLeavesTheInterfaceDownAndTheCapturedTokenWorthless')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='fx19-1', group='F19', repo='ANet',
  desc='anet console 把带票据的 URL 交给浏览器命令行',
  edits=[
   ('cmd/anet/main.go', '\t\tif err := openBrowser(tk.Launcher); err != nil {', '\t\tif err := openBrowser(tk.URL); err != nil {'),
  ],
  tests=[('ANet:./cmd/anet', 'TestConsoleCommandKeepsTheTicketOffEveryCommandLine')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='fx20-1', group='F20', repo='ANet',
  desc='/pull 接受他人可写的子目录',
  edits=[
   ('internal/daemon/attachments.go', '\tcase fi.Mode().Perm()&0o022 != 0:', '\tcase false && fi.Mode().Perm()&0o022 != 0:'),
  ],
  tests=[('ANet:./internal/daemon', 'TestPullRefusesAPreparedSubdirOthersCanWrite')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx20-2', group='F20', repo='ANet',
  desc='/pull 接受不属于本 uid 的子目录',
  edits=[
   ('internal/daemon/attachments.go', 'ok && int(st.Uid) != uid {', 'ok && false && int(st.Uid) != uid {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestPullSubdirMustBelongToThisUser')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx32-1', group='F32', repo='ANet',
  desc='单任务读取内联附件不设上限',
  edits=[
   ('internal/a2ashape/project.go', '\t\tp.inlineLeft = MaxInlineBytes', '\t\tp.inlineLeft = 1 << 40'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestInlineFilesStopAtTheLimit'), ('ANet:./internal/daemon', 'TestTheA2AInterfaceHoldsToTheQ12InlineLimit')],
  full=['ANet:./internal/a2ashape', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx32-2', group='F32', repo='ANet',
  desc='流式事件不按 8 MiB 截断',
  edits=[
   ('internal/a2ashape/stream.go', '\tbudget := MaxStreamEventBytes - 256 - jsonLen(t.ID) - jsonLen(t.ContextID)', '\tbudget := 1 << 40'),
  ],
  tests=[('ANet:./internal/a2ashape', 'TestTaskForStreamHoldsAnEventToTheLimit'), ('ANet:./module/a2a', 'TestAStreamStaysReadableWhateverAPeerSends')],
  full=['ANet:./internal/a2ashape', 'ANet:./module/a2a'],
 ),
 dict(
  id='fx32-3', group='F32', repo='ANet',
  desc='ListTasks 的页面内联附件字节',
  edits=[
   ('internal/daemon/taskseam.go', 't, err := d.taskView(ix, viewOpts{historyLen: f.HistoryLen, artifacts: f.IncludeArtifacts})', 't, err := d.taskView(ix, viewOpts{historyLen: f.HistoryLen, artifacts: f.IncludeArtifacts, inline: sc.inline()})'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheA2AInterfaceHoldsToTheQ12InlineLimit')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx33-1', group='F33', repo='ANet',
  desc='messageId 去重也认对端写入的消息',
  edits=[
   ('internal/runtime/interactions/tasks.go', 'WHERE i.role=? AND m.sender_aid <> i.peer_aid`', 'WHERE i.role=?`'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAProviderCannotPlantTheClientsMessageIDs')],
  full=['ANet:./internal/runtime/interactions', 'ANet:./internal/daemon'],
 ),
 dict(
  id='q32-1', group='Q32', repo='ANet',
  desc='messageId 去重也命中终态任务',
  edits=[
   ('internal/runtime/interactions/tasks.go', '\tif q.OpenOnly {\n', '\tif false && q.OpenOnly {\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAMessageIDSeenOnAFinishedTaskStartsANewOne')],
  full=['ANet:./internal/runtime/interactions', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx39-1', group='F39', repo='ANet',
  desc='/find 在官方标注旁照登 hub 写的文字',
  edits=[
   ('internal/daemon/official.go', '\t\tAgentView: hubapi.AgentView{AID: a.AID, Name: e.Name, Caps: e.Caps, Listed: a.Listed},', '\t\tAgentView: a,'),
   ('internal/daemon/official.go', '\te, official := d.officials.Load().Lookup(a.AID, d.now())', '\t_, official := d.officials.Load().Lookup(a.AID, d.now())'),
  ],
  tests=[('ANet:./internal/daemon', 'TestFindShowsNoHubTextBesideTheOfficialMark')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx39-2', group='F39', repo='ANet',
  desc='/find 不丢弃非法 AID 的条目',
  edits=[
   ('internal/daemon/official.go', '\tif !validAgentID(a.AID) {\n\t\treturn foundAgent{}, false', '\tif false && !validAgentID(a.AID) {\n\t\treturn foundAgent{}, false'),
  ],
  tests=[('ANet:./internal/daemon', 'TestFindShowsNoHubTextBesideTheOfficialMark')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx41-1', group='F41', repo='ANet',
  desc='hub-register 接受 --token(邀请码在 argv)',
  edits=[
   ('cmd/anet/peers.go', '\tif _, ok := flags["token"]; ok {\n\t\treturn "", errInviteOnCommandLine', '\tif _, ok := flags["token"]; false && ok {\n\t\treturn "", errInviteOnCommandLine'),
  ],
  tests=[('ANet:./cmd/anet', 'TestHubRegisterTakesTheInviteOffTheCommandLine')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='fx41-2', group='F41', repo='ANet',
  desc='--token-file 接受他人可读的邀请码文件',
  edits=[
   ('cmd/anet/peers.go', '\tif m&0o044 == 0 {\n\t\treturn nil\n\t}', '\tif true || m&0o044 == 0 {\n\t\treturn nil\n\t}'),
  ],
  tests=[('ANet:./cmd/anet', 'TestHubRegisterRefusesAnInviteFileOthersCanRead')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='fx41-3', group='F41', repo='ANetHub',
  desc='铸码输出教 agent 把邀请码放在命令行上',
  edits=[
   ('cmd/anet-hub/invite_ops.go', '--name <name> --token-file FILE")', '--name <name> --token <invite>")'),
  ],
  tests=[('ANetHub:./cmd/anet-hub', 'TestMintedInviteIsNotTaughtOnACommandLine')],
  full=['ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='fx1-1', group='F1', repo='ANet',
  desc='publish_prices=false 时卡片仍带逐 skill 价格',
  edits=[
   ('internal/daemon/card.go', '\tif d.providers == nil || !d.config().Payments.publishesPrices() {', '\tif d.providers == nil {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestWithheldPricesAreOnNeitherCard')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx3-1', group='F3', repo='ANet',
  desc='收件方密钥/卡片/KEL 查询改回 GET(AID 在请求行)',
  edits=[
   ('internal/daemon/seal_send.go', '\terr := d.hubPost(ctx, hub, postPath, hubapi.KeysLookupRequest{AID: aid}, out)', '\terr := d.hubGet(ctx, hub, getPath, nil, out)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestSendingDoesNotNameTheRecipientInARequestLine|TestReadingAPeersCardDoesNotNameItInARequestLine')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='fx3-2', group='F3', repo='ANetHub',
  desc='随部署的反代恢复访问日志',
  edits=[
   ('deploy/nginx-hub.conf', '    access_log off;\n', '    access_log /var/log/nginx/hub.access.log;\n', 2),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestTheShippedProxyKeepsNoLogOfWhoTalksToWhom')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='q28-1', group='Q28', repo='ANet',
  desc='不可付的 rail 照样签名(不回 rail_not_payable)',
  edits=[
   ('internal/daemon/x402task.go', '\tif home := p.HomeNetwork(); home == "" || opt.Network != home {', '\tif home := p.HomeNetwork(); false && (home == "" || opt.Network != home) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAQuoteIsOrderedByWhatThisNodeCanPayAndAnUnpayableRailIsRefused|TestNothingIsSignedWhileThisNodesLedgerIsUnknown')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q28-2', group='Q28', repo='ANet',
  desc='给本机客户端的付款选项不按可付排序',
  edits=[
   ('internal/daemon/x402task.go', '\t\t\trequired = payableFirst(ix.PayRequired, d.knownHomeNetwork())', '\t\t\trequired = payableFirst(ix.PayRequired, "")'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAQuoteIsOrderedByWhatThisNodeCanPayAndAnUnpayableRailIsRefused')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q30-1', group='Q30', repo='ANet',
  desc='/tasks/reply state=completed 的最后一条回复不带 anet.state=working',
  edits=[
   ('internal/daemon/tasks_api.go', 'd.sendMessage(sctx, ix.ID, text, in.atts, a2ashape.FinalReplyMetadata())', 'd.sendMessage(sctx, ix.ID, text, in.atts, nil)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAFinalReplyKeepsTheRequesterWorkingUntilTheResult|TestReplyCompletedDoesNotEndABlockingSendEarly')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q30-2', group='Q30', repo='ANet',
  desc='自动回复判定完成时最后一条回复不带 final 标记',
  edits=[
   ('internal/daemon/autoreply.go', '\t\tmeta = a2ashape.FinalReplyMetadata()\n', '\t\tmeta, _ = nil, a2ashape.FinalReplyMetadata\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnAutoReplyThatCompletesSendsAFinalReply')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='q31-1', group='Q31', repo='ANet',
  desc='流式调用不预检(错误装在 SSE 里)',
  edits=[
   ('module/a2a/server.go', '\t\tr, answered := s.precheckStream(w, r, aid, jsonrpc)\n', '\t\tanswered := false\n\t\t_ = jsonrpc\n'),
  ],
  tests=[('ANet:./module/a2a', 'TestAStreamThatCannotStartIsAnOrdinaryError')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='q33-1', group='Q33', repo='ANet',
  desc='卡片与列表回 no-store',
  edits=[
   ('module/a2a/server.go', '\th.Set("Cache-Control", "private, max-age="+strconv.Itoa(cardMaxAge))', '\th.Set("Cache-Control", "no-store")'),
  ],
  tests=[('ANet:./module/a2a', 'TestCardsAndTheAgentListAreCacheablePrivately')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='s9-1', group='si9', repo='ANet',
  desc='商户核对放行 > 2^63-1 的金额',
  edits=[
   ('module/x402/check.go', '\tif auth.Amount > math.MaxInt64 {', '\tif false && auth.Amount > math.MaxInt64 {'),
  ],
  tests=[('ANet:./module/x402', 'TestRedteamSI9MerchantCheckRefusesAnOverflowingAmount')],
  full=['ANet:./module/x402'],
 ),
 dict(
  id='s9-2', group='si9', repo='ANet',
  desc='兑付凭证放行 > 2^63-1 的金额',
  edits=[
   ('module/x402/voucher.go', '\tif v.Amount > math.MaxInt64 {', '\tif false && v.Amount > math.MaxInt64 {'),
  ],
  tests=[('ANet:./module/x402', 'TestRedeemRefusesAVoucherForMoreThanALedgerCanHold')],
  full=['ANet:./module/x402'],
 ),
 dict(
  id='s9-3', group='si9', repo='ANet',
  desc='签名放行 > 2^63-1 的金额',
  edits=[
   ('module/x402/pay.go', '\tif amount > math.MaxInt64 {', '\tif false && amount > math.MaxInt64 {'),
  ],
  tests=[('ANet:./module/x402', 'TestAuthorizeRefusesAnAmountNoLedgerCanHold')],
  full=['ANet:./module/x402'],
 ),
 dict(
  id='s9-4', group='si9', repo='ANetHub',
  desc='hub amountInt64 放行 ≥ 2^63 的金额',
  edits=[
   ('internal/aghub/amount.go', '\tif n == 0 || n > math.MaxInt64 {', '\tif n == 0 {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestRedteamSI9SettleOfANegativeAmountDoesNotDrainTheVictim|TestRedteamSI9RedemptionOfANegativeAmountMintsNothing|TestAnUnholdableAmountCannotBeSettled')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='s9-5', group='si9', repo='ANetHub',
  desc='hub 账本加减不检查结果范围',
  edits=[
   ('internal/aghub/amount.go', 'DO UPDATE SET %[3]s = %[3]s + ? WHERE %[3]s %[4]s ?`', 'DO UPDATE SET %[3]s = %[3]s + ? WHERE %[3]s %[4]s ? OR 1=1`'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestPeerReceiptsInRangeCannotSumPastWhatALedgerHolds|TestASettlementThatWouldOverflowThePayeeMovesNothing|TestAGrantPastWhatALedgerHoldsIsRefused')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='s9-6', group='si9', repo='ANetHub',
  desc='对端收据金额可以不等于转发的授权',
  edits=[
   ('internal/aghub/facilitator.go', '\tcase rec.Amount != auth.Amount:', '\tcase false && rec.Amount != auth.Amount:'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAPeerReceiptForMoreThanTheForwardedAuthorizationIsNotCleared')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx4-1', group='F4', repo='ANetHub',
  desc='relayauth 重放缓存不设每签名方份额',
  edits=[
   ('internal/aghub/auth2.go', '\tif held >= c.perSigner {', '\tif false && held >= c.perSigner {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestOneSignerFillsOnlyItsOwnShareOfTheReplayCache|TestOneRegisteredAgentFillingTheReplayCacheStopsOnlyItself|TestThePerSignerBoundCoversTheSendBucket')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx4-2', group='F4', repo='ANetHub',
  desc='邀请制 hub 在验签之后才核邀请(陌生人占重放缓存)',
  edits=[
   ('internal/aghub/server.go', '\tif s.store.InviteRequired() && !s.store.KnowsAgent(req.AID) {\n\t\tif err := s.store.CheckInvite(req.Invite, req.AID); err != nil {', '\tif false && s.store.InviteRequired() && !s.store.KnowsAgent(req.AID) {\n\t\tif err := s.store.CheckInvite(req.Invite, req.AID); err != nil {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAStrangerAtAClosedHubLeavesNothingInTheReplayCache')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx36-1', group='F36', repo='ANetHub',
  desc='/register 不施加 seal.MaxKEL* 上限',
  edits=[
   ('internal/aghub/server.go', '\tkel, err := seal.ParseKEL(kelBytes)\n\tif err != nil {\n\t\tif seal.ReasonOf(err) == seal.ReasonKELTooLarge {', '\tkel, err := identity.UnmarshalKEL(kelBytes)\n\tif err != nil {\n\t\tif seal.ReasonOf(err) == seal.ReasonKELTooLarge {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestARegistrationWithAKELPastTheMessagePathCapIsRefused')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx36-2', group='F36', repo='ANetHub',
  desc='坏签名先回放 KEL 再验(去掉 plausibleSignature)',
  edits=[
   ('internal/aghub/auth2.go', '\tif verr := plausibleSignature(kel, a.Seq, pre, a.Sig); verr != nil {', '\tif verr := plausibleSignature(kel, a.Seq, pre, a.Sig); false && verr != nil {'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestABadSignatureDoesNotReplayTheKELItNames')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx36-3', group='F36', repo='ANetHub',
  desc='hub 不安装进程级 KEL 回放缓存',
  edits=[
   ('internal/aghub/aghub.go', '\tuseReplayCache()\n', ''),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAStoredKELIsReplayedOnceNotOnEveryRequest')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='fx36-4', group='F36', repo='ANetCore',
  desc='ReplayCache 永不命中',
  edits=[
   ('identity/replaycache.go', '\tif el, ok := c.byKEL[key]; ok {', '\tif el, ok := c.byKEL[key]; ok && false {'),
  ],
  tests=[('ANetCore:./identity', 'TestAReplayCache'), ('ANet:./internal/daemon', 'TestRedteamClosedNodeKELReplayExhaustion')],
  full=['ANetCore:./identity', 'ANet:./internal/daemon'],
 ),
 dict(
  id='fx34-1', group='F34', repo='ANetCore',
  desc='Replay 不拒绝长度不是 32 字节的 icp 密钥',
  edits=[
   ('identity/identity.go', '\t\t\tif len(e.Keys) != 1 || len(e.Keys[0]) != ed25519.PublicKeySize {\n\t\t\t\t// Checked before ed25519.Verify', '\t\t\tif len(e.Keys) != 1 {\n\t\t\t\t// Checked before ed25519.Verify'),
  ],
  tests=[('ANetCore:./identity', 'TestAKeyOfTheWrongLengthIsRefusedNotAPanic')],
  full=['ANetCore:./identity'],
 ),
 dict(
  id='fx34-2', group='F34', repo='ANetHub',
  desc='对端 hub KEL 不核对回放到的 AID 就固定',
  edits=[
   ('internal/federation/federation.go', '\tif icp, err := identity.Replay(kel[:1]); err != nil || icp[0].AID != aid {', '\tif icp, err := identity.Replay(kel[:1]); err != nil {'),
   ('internal/federation/federation.go', '\tif got := states[len(states)-1].AID; got != aid {', '\tif got := states[len(states)-1].AID; false && got != aid {'),
  ],
  tests=[('ANetHub:./internal/federation', 'TestAPeerKELIsPinnedOnlyWhenItReplaysToThePeer|TestAMisPinnedPeerKELIsDroppedWhenTheServiceOpens')],
  full=['ANetHub:./internal/federation'],
 ),
 dict(
  id='fx34-3', group='F34', repo='ANetHub',
  desc='对端 KEL 取回失败后不等待(每次使用都再取)',
  edits=[
   ('internal/federation/federation.go', '!f.refused.IsZero() && wait > 0 {', 'false && !f.refused.IsZero() && wait > 0 {'),
  ],
  tests=[('ANetHub:./internal/federation', 'TestAPeerKELThatFailsIsNotFetchedOnEveryUse')],
  full=['ANetHub:./internal/federation'],
 ),
 dict(
  id='fx38-1', group='F38', repo='ANetHub',
  desc='taskboard 写入路由不限请求体大小',
  edits=[
   ('internal/taskboard/http.go', 'raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMutationBody))', 'raw, err := io.ReadAll(r.Body)'),
  ],
  tests=[('ANetHub:./internal/taskboard', 'TestAnOversizedMutationIsRefusedBeforeAuthentication', 'taskboard')],
  full=[],
 ),
 dict(
  id='fx38-2', group='F38', repo='ANetHub',
  desc='taskboard 先解码内容再核签名',
  edits=[
   ('internal/taskboard/http.go', '\t\tif err := s.auth.VerifyAgentChallenge(action, a.AID, a.TS, a.KeyStateSeq, a.Sig); err != nil {\n\t\t\twriteJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})\n\t\t\treturn\n\t\t}\n\t\tvar req mutateReq\n\t\tif err := json.Unmarshal(raw, &req); err != nil {\n\t\t\twriteJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})\n\t\t\treturn\n\t\t}', '\t\tvar req mutateReq\n\t\tif err := json.Unmarshal(raw, &req); err != nil {\n\t\t\twriteJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})\n\t\t\treturn\n\t\t}\n\t\tif err := s.auth.VerifyAgentChallenge(action, a.AID, a.TS, a.KeyStateSeq, a.Sig); err != nil {\n\t\t\twriteJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})\n\t\t\treturn\n\t\t}'),
  ],
  tests=[('ANetHub:./internal/taskboard', 'TestAMutationIsAuthenticatedBeforeItsContentIsDecoded', 'taskboard')],
  full=[],
 ),
 dict(
  id='n1-1', group='N1', repo='ANet',
  desc='外部可写(o+w)的 socket 目录被接受',
  edits=[
   ('internal/backendconn/backendconn.go', '\t\t\tif perm&0o002 != 0 && (!sticky || i == final) {', '\t\t\tif false && perm&0o002 != 0 && (!sticky || i == final) {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestAWritableDirectoryIsRefused|TestJudge')],
  full=['ANet:./internal/backendconn', 'ANet:./module/service'],
 ),
 dict(
  id='n1-2', group='N1', repo='ANet',
  desc='SO_PEERCRED 与期望 uid/socket 属主不符仍连接',
  edits=[
   ('internal/backendconn/backendconn.go', '\tif uid != want {', '\tif false && uid != want {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestAListenerThatIsNotTheExpectedUserIsRefused'), ('ANet:./module/service', 'TestASocketListenerThatIsNotTheExpectedUserIsNotCalled'), ('ANet:./module/a2a', 'TestASocketBackendOfTheWrongUserGetsNothing')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-3', group='N1', repo='ANet',
  desc='allow_tcp 下回环监听者属他人 uid 仍发送',
  edits=[
   ('internal/backendconn/backendconn.go', '\tcase uid != r.self:', '\tcase false && uid != r.self:'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestATCPListenerOfAnotherUserIsRefused'), ('ANet:./module/service', 'TestATCPListenerOfAnotherUserIsNotCalled'), ('ANet:./module/a2a', 'TestATCPBackendHeldByAnotherUserGetsNothing')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-4', group='N1', repo='ANet',
  desc='未设 allow_tcp 的 TCP 后端被接受',
  edits=[
   ('internal/backendconn/backendconn.go', '\tif !t.Unix() && !r.allowTCP {', '\tif false && !t.Unix() && !r.allowTCP {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestTCPIsRefusedByDefault'), ('ANet:./module/service', 'TestTCPServicesNeedAllowTCP'), ('ANet:./module/a2a', 'TestATCPBackendNeedsAllowTCP'), ('ANet:./cmd/anet', 'TestDoctorBackendTransport')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-5', group='N1', repo='ANet',
  desc='socket 属主在他人可写目录下建的目录也被信任(/tmp 抢建)',
  edits=[
   ('internal/backendconn/backendconn.go', '\t\tif e.m.uid == owner && e.parent >= 0 && strict(entries[e.parent].m) {', '\t\tif e.m.uid == owner && e.parent >= 0 && (true || strict(entries[e.parent].m)) {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestJudge')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-6', group='N1', repo='ANet',
  desc='组可写目录不要求 socket_group',
  edits=[
   ('internal/backendconn/backendconn.go', '\t\t\tif perm&0o020 != 0 && !groupOK(e.m) && (!sticky || i == final) {', '\t\t\tif false && perm&0o020 != 0 && !groupOK(e.m) && (!sticky || i == final) {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestAWritableDirectoryIsRefused|TestJudge')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-7', group='N1', repo='ANet',
  desc='可信目录里他人留下的 socket 被接受',
  edits=[
   ('internal/backendconn/backendconn.go', '\tif fd := entries[final].m; !trusted(owner) && fd.uid != owner', '\tif fd := entries[final].m; false && !trusted(owner) && fd.uid != owner'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestJudge')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-8', group='N1', repo='ANet',
  desc='socket 后端的卡片可指向另一个 socket',
  edits=[
   ('module/a2a/backend.go', '\t\t\tif it.Socket == t.Socket {', '\t\t\tif true {'),
  ],
  tests=[('ANet:./module/a2a', 'TestASocketBackendsCardCannotPointElsewhere')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='n1-10', group='N1', repo='ANet',
  desc='allow_tcp 的监听者核实只看回环,本机网卡地址上的监听者放行',
  edits=[
   ('internal/backendconn/backendconn.go', '\tif !onThisHost(ta, c.LocalAddr()) {', '\tif !ta.IP.IsLoopback() {'),
  ],
  tests=[('ANet:./internal/backendconn', 'TestATCPListenerOnAnAddressOfThisHostIsChecked')],
  full=['ANet:./internal/backendconn'],
 ),
 dict(
  id='n1-11', group='N1', repo='ANet',
  desc='socket 后端不可达时把本机 socket 路径回传给请求方',
  edits=[
   ('module/service/service.go', '\t\tcase be.unix:', '\t\tcase false && be.unix:'),
  ],
  tests=[('ANet:./module/service', 'TestAMissingSocketIsUnavailable')],
  full=['ANet:./module/service'],
 ),
 dict(
  id='n1-9', group='N1', repo='ANet',
  desc='anet-official 的 socket 不设 0660',
  edits=[
   ('cmd/anet-official/main.go', '\tif err := os.Chmod(path, 0o660); err != nil {', '\tif err := os.Chmod(path, 0o600); false && err != nil {'),
  ],
  tests=[('ANet:./cmd/anet-official', 'TestListenUnix')],
  full=['ANet:./cmd/anet-official'],
 ),
]
