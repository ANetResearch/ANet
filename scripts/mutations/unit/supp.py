# supp.py: A2A-DESIGN §17 supplementary rows — mutation list for scripts/mutations/unitmut.py (see its header for the format).
# Written for docs/notes/0026 (each row at least one mutation); a mutation that no longer applies reports APPLY-ERR: fix its
# edit to the moved code rather than dropping it.

MUTS = [
 dict(
  id='c16-1', group='C16/C3', repo='ANetCore',
  desc='DecideHighWater: 同 seq 同字节(==)判为分叉(设计指定 mutation)',
  edits=[
   ('seal/highwater.go', '\tcase bytes.Equal(set, seen.Set):\n\t\treturn Same', '\tcase bytes.Equal(set, seen.Set):\n\t\treturn Fork'),
  ],
  tests=[('ANetCore:./seal', 'TestDecideHighWater'), ('ANet:./internal/daemon', 'TestAnUnchangedKeySetIsAcceptedAgain|TestARestartKeepsTheSameKeySet'), ('ANetHub:./internal/aghub', 'TestKeyPublisherRules|TestTheSameKeySetMustStillVerify')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon', 'ANetHub:./internal/aghub'],
 ),
 dict(
  id='c16-2', group='C16/C3', repo='ANetCore',
  desc='DecideHighWater: 旧 seq 判为替换(回退)',
  edits=[
   ('seal/highwater.go', '\tcase seq < seen.Seq:\n\t\treturn Ignore', '\tcase seq < seen.Seq:\n\t\treturn Replace'),
  ],
  tests=[('ANetCore:./seal', 'TestDecideHighWater'), ('ANet:./internal/daemon', 'TestAHubCannotRollBackOrForkAKnownRecipientsKeys'), ('ANetHub:./internal/aghub', 'TestAPeerCannotForkOrCorruptAStoredKeySet|TestKeyPublisherRules')],
  full=['ANetCore:./seal', 'ANet:./internal/daemon', 'ANetHub:./internal/aghub'],
 ),
 dict(
  id='c16-3', group='C16/C3', repo='ANet',
  desc='mergeKEL: 内层 KEL 短于已存(回退)时拒收而不是用已存',
  edits=[
   ('internal/daemon/peerkel.go', '\tcase errors.Is(err, identity.ErrKELRollback):\n\t\treturn stored, false, nil', '\tcase errors.Is(err, identity.ErrKELRollback):\n\t\treturn nil, false, err'),
  ],
  tests=[('ANet:./internal/daemon', 'TestRotationGrace|TestPeerKELIsExtendedNeverRolledBack|TestATruncatedKELIsRefusedAfterTableChurnAndRestart|TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c16-4', group='C16/C3', repo='ANet',
  desc='mergeKEL: 与已存等长(==)的 KEL 判为分叉',
  edits=[
   ('internal/daemon/peerkel.go', '\tcase err == nil:\n\t\treturn candidate, len(candidate) > len(stored), nil', '\tcase err == nil && len(candidate) == len(stored):\n\t\treturn nil, false, identity.ErrKELFork\n\tcase err == nil:\n\t\treturn candidate, len(candidate) > len(stored), nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAcceptedPeerKELIsRecorded|TestPeerKELIsExtendedNeverRolledBack|TestAnUnchangedKeySetIsAcceptedAgain|TestRelayDelegationRoundTrip')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c1-1', group='C1', repo='ANet',
  desc='leftoverAction: working 的长能力调用也重跑(破坏至多一次)',
  edits=[
   ('internal/daemon/wire_edges.go', '\t\tif resolvable && !long {\n\t\t\treturn leftoverRerun', '\t\tif resolvable {\n\t\t\treturn leftoverRerun'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStopLeavesALongCallToStartupRecovery|TestAnInterruptedLongCallIsReportedAtStart|TestALongCallRecordedButNeverStartedIsReportedAtStart|TestALongCallLeftWorkingIsNotRunAgainWhenItsProviderIsBack')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c1-2', group='C1', repo='ANet',
  desc='recoverInterrupted: 长调用不报 interrupted',
  edits=[
   ('internal/daemon/capability.go', '\t\tcase leftoverInterrupted:\n\t\t\td.reportInterrupted(ix, capID)', '\t\tcase leftoverInterrupted:\n\t\t\t_ = capID'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStopLeavesALongCallToStartupRecovery|TestAnInterruptedLongCallIsReportedAtStart|TestALongCallRecordedButNeverStartedIsReportedAtStart')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c1-3', group='C1', repo='ANet',
  desc='redeliveredDelegate: 短调用不重跑(破坏至少一次)',
  edits=[
   ('internal/daemon/delegation.go', '\treturn d.runCapabilityCall(m.ix, capID, args, m.dr.Payment, nil)\n}', '\t_ = args\n\treturn true\n}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStopDoesNotAnswerAShortCallItCutOff|TestAStopBeforeAnUnservedCallIsAnsweredLeavesItUnacknowledged|TestAStoreFailureThenRedeliveryIsProcessedOnce')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c12-1', group='C12', repo='ANet',
  desc='recordSenderIdentity: public_cap 请求方写入 peer_identity',
  edits=[
   ('internal/daemon/receive.go', '\t\tcase ix.Trust == interactions.TrustApproved:\n\t\t\tauthorized = true', '\t\tcase ix.Trust == interactions.TrustApproved, ix.Trust == interactions.TrustPublicCap:\n\t\t\tauthorized = true'),
  ],
  tests=[('ANet:./internal/daemon', 'TestATruncatedKELIsRefusedAfterTableChurnAndRestart')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c12-2', group='C12', repo='ANet',
  desc='mergeKEL: 已存延伸内层(截断 KEL)时改用内层',
  edits=[
   ('internal/daemon/peerkel.go', '\tcase errors.Is(err, identity.ErrKELRollback):\n\t\treturn stored, false, nil', '\tcase errors.Is(err, identity.ErrKELRollback):\n\t\treturn candidate, false, nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestATruncatedKELIsRefusedAfterTableChurnAndRestart|TestRotationGrace')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c12-3', group='C12', repo='ANet',
  desc='evictPeers: 淘汰不看 pinned_reason 与活动交互',
  edits=[
   ('internal/runtime/interactions/peers.go', "\t\t WHERE p.pinned_reason = '' AND NOT EXISTS (`+activeInteractionSQL+`)\n\t\t   AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.to_aid = p.aid)\n\t\t   AND NOT EXISTS (SELECT 1 FROM pending q WHERE q.from_aid = p.aid)\n", '\t\t WHERE 1\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestATruncatedKELIsRefusedAfterTableChurnAndRestart'), ('ANet:./internal/runtime/interactions', 'TestPeerIdentityPinsAndEviction')],
  full=['ANet:./internal/daemon', 'ANet:./internal/runtime/interactions'],
 ),
 dict(
  id='m-ix-1', group='m(ix 碰撞)', repo='ANet',
  desc='authorizeDelegate: ix 碰撞只看对端不看角色',
  edits=[
   ('internal/daemon/receive.go', '\tif ix != nil && (ix.Role != interactions.RoleInbound || ix.PeerAID != m.from) {', '\tif ix != nil && ix.PeerAID != m.from {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass|TestACollidingDelegationIsRefusedBeforeAnyWrite')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-ix-2', group='m(ix 碰撞)', repo='ANet',
  desc='authorizeDelegate: 完全去掉 ix 碰撞检查',
  edits=[
   ('internal/daemon/receive.go', '\tif ix != nil && (ix.Role != interactions.RoleInbound || ix.PeerAID != m.from) {', '\tif false && ix != nil {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass|TestACollidingDelegationIsRefusedBeforeAnyWrite')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-ix-3', group='m(ix 碰撞)', repo='ANet',
  desc='ix 碰撞两处检查(第 9 步与第 10 步事务内)都去掉',
  edits=[
   ('internal/daemon/receive.go', '\tif ix != nil && (ix.Role != interactions.RoleInbound || ix.PeerAID != m.from) {', '\tif false && ix != nil {'),
   ('internal/daemon/delegation.go', '\t\t\tif prior.Role != interactions.RoleInbound || prior.PeerAID != m.from {', '\t\t\tif false && (prior.Role != interactions.RoleInbound || prior.PeerAID != m.from) {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEachReceiveStepRefusesWithItsClass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c19-1', group='C19', repo='ANet',
  desc='未知 ix 的 message 不等待,立即 TaskNotFound',
  edits=[
   ('internal/daemon/receive.go', '\t\tif int64(now)-int64(m.ts) <= unknownIXWait.Milliseconds() {', '\t\tif false && int64(now)-int64(m.ts) <= unknownIXWait.Milliseconds() {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAMessageForAnUnknownTaskGetsTaskNotFound|TestHeldBackEnvelopesDoNotBlockNewerMail|TestAHeldEnvelopeIsRetriedAtOnePollPerRound')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c19-2', group='C19', repo='ANet',
  desc='未知 ix 等待窗口常量 10 分钟 → 100 分钟',
  edits=[
   ('internal/daemon/receive.go', 'const unknownIXWait = 10 * time.Minute', 'const unknownIXWait = 100 * time.Minute'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAMessageForAnUnknownTaskGetsTaskNotFound|TestHeldBackEnvelopesDoNotBlockNewerMail|TestAHeldEnvelopeIsRetriedAtOnePollPerRound|TestADeniedPeerWritingToAnUnknownTaskIsAnsweredLikeAStranger|TestTheUnknownTaskWindowIsTenMinutes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c6-1', group='C6/C29', repo='ANet',
  desc='public_cap 交互上的 payment-submitted 文本消息被当作文本拒收',
  edits=[
   ('internal/daemon/receive.go', '\tcase x402a2a.StatusSubmitted, x402a2a.StatusRejected:\n\t\treturn true', '\tcase x402a2a.StatusSubmitted, x402a2a.StatusRejected:\n\t\treturn false'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStrangerPaysForAPublicCapability|TestAPublicCapabilityCallTakesNoText')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c6-2', group='C6/C29', repo='ANet',
  desc='能力调用上的 end_request 走文本任务的完成路径(签对话记录回执)',
  edits=[
   ('internal/daemon/delegation.go', '\t\tif ix.IsCapability {\n\t\t\td.capabilityStopRequest(fctx, m.ix, false)\n\t\t\treturn res\n\t\t}', '\t\tif false && ix.IsCapability {\n\t\t\td.capabilityStopRequest(fctx, m.ix, false)\n\t\t\treturn res\n\t\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestALongCapabilityCallUnderEndRequestAndCancel|TestAQuotedTaskEndedByTheRequesterIsCanceledWithoutAReceipt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c6-3', group='C6/C29', repo='ANet',
  desc='自动回复循环不再跳过能力调用与 public_cap 交互',
  edits=[
   ('internal/daemon/autoreply.go', '\t\tif interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {', '\t\tif interactions.State(th.State).IsTerminal() {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall|TestAutoReplyCapProposesEnd')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c6-4', group='C6/C29', repo='ANet',
  desc='自动回复两层过滤都去掉: ActiveThreads 与循环都不排除能力调用/public_cap',
  edits=[
   ('internal/daemon/delegation.go', 'ListFilter{Active: true, ExcludeCapability: true,\n\t\tExcludeTrust: []string{interactions.TrustPublicCap}})', 'ListFilter{Active: true})'),
   ('internal/daemon/autoreply.go', '\t\tif interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {', '\t\tif interactions.State(th.State).IsTerminal() {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall|TestAutoReplyCapProposesEnd|TestAPublicCapabilityCallTakesNoText')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c8-1', group='C8/C9', repo='ANet',
  desc='validatePolicy: open 与 exec untrusted=sandbox 不再冲突',
  edits=[
   ('internal/daemon/inbound.go', '\tif ar := c.AutoReply; ar != nil && ar.Backend == "exec" && ar.UntrustedMode() != UntrustedOff {', '\tif ar := c.AutoReply; false && ar != nil && ar.Backend == "exec" && ar.UntrustedMode() != UntrustedOff {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestOpenAndExecForUntrustedPeersConflict')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c9-1', group='C8/C9', repo='ANet',
  desc='backendTakes: 未声明 untrusted 的后端也接 allow 但不在 trust 的对端',
  edits=[
   ('internal/daemon/inbound_tasks.go', '\treturn false, untrustedDeclared && named', '\treturn false, named'),
  ],
  tests=[('ANet:./internal/daemon', 'TestInboundTasksReachTheBackendOnlyFromTrustedPeers|TestInboundTasksWithAnUntrustedBackend|TestBackendTakes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c9-2', group='C8/C9', repo='ANet',
  desc='exec 自动回复: untrusted=off 时仍为非信任对端运行',
  edits=[
   ('internal/daemon/autoreply.go', '\t\tif !gate.trusted && cfg.UntrustedMode() == UntrustedOff {\n\t\t\treturn nil', '\t\tif false && !gate.trusted && cfg.UntrustedMode() == UntrustedOff {\n\t\t\treturn nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestExecRunsOnlyForTrustedPeers|TestInboundTasksReachTheBackendOnlyFromTrustedPeers')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c9-3', group='C8/C9', repo='ANet',
  desc='backendTakes: open 下的陌生人(trust=public)也交给声明了 untrusted 的后端',
  edits=[
   ('internal/daemon/inbound_tasks.go', '\tnamed := trust == interactions.TrustPeer || trust == interactions.TrustApproved', '\tnamed := trust == interactions.TrustPeer || trust == interactions.TrustApproved || trust == interactions.TrustPublic'),
  ],
  tests=[('ANet:./internal/daemon', 'TestInboundTasksReachTheBackendOnlyFromTrustedPeers|TestInboundTasksWithAnUntrustedBackend|TestBackendTakes')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c22-1', group='C22', repo='ANet',
  desc='SendMessage: 客户端给的 contextId 被新生成的覆盖',
  edits=[
   ('internal/daemon/taskseam.go', '\tif contextID == "" {\n\t\tif contextID, err = newContextID(); err != nil {', '\tif true {\n\t\tif contextID, err = newContextID(); err != nil {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestClientContextIsKeptAndRetriesAreDeduplicated|TestListTasksFilterEdges|TestAContextOnlyMessageContinuesTheTaskWaitingForIt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c25-1', group='C25', repo='ANet',
  desc='settlement_pending 当作已结算(未知 → 成功)',
  edits=[
   ('internal/daemon/x402task.go', '\tcase err != nil || st.Pending:\n', '\tcase err != nil:\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnUnknownSettlementIsRetriedAndChargedOnce|TestASettlementInFlightIsResumedAfterARestart')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c25-2', group='C25', repo='ANet',
  desc='结算结果未知时不启动重试循环',
  edits=[
   ('internal/daemon/x402task.go', 'presenting the same payment again", ixID, why)\n\t\tif first {\n\t\t\td.ensureSettling(ixID)\n\t\t}', 'presenting the same payment again", ixID, why)\n\t\t_ = first'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAnUnknownSettlementIsRetriedAndChargedOnce|TestASettlementInFlightIsResumedAfterARestart')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c25-3', group='C25', repo='ANet',
  desc='ErrorCode: settlement_pending 映射为终结的 SETTLEMENT_FAILED',
  edits=[
   ('module/x402/check.go', '\tif reason == payment.ReasonSettlementPending {\n\t\treturn "", false\n\t}\n', ''),
  ],
  tests=[('ANet:./module/x402', 'TestTheErrorReasonTableIsPinned'), ('ANet:./internal/daemon', 'TestAnUnknownSettlementIsRetriedAndChargedOnce')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='c33-1', group='C33', repo='ANet',
  desc='重投的 delegate 不重发已签回执的结果',
  edits=[
   ('internal/daemon/delegation.go', '\t\tlog.Printf("anet: %s redelivered; re-sending the answer we already signed", m.ix)\n\t\td.resendResult(m, prior)\n', '\t\tlog.Printf("anet: %s redelivered; re-sending the answer we already signed", m.ix)\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestARedeliveredDelegateResendsTheAnswer|TestAQueuedAnswerIsNotQueuedAgainForARedelivery|TestAnEndedPublicCallAnswersARedeliveredDelegateOnce|TestARedeliveredPaidCallResendsItsReceipts')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c33-2', group='C33', repo='ANet',
  desc='resendResult: 队列里已有结果时仍再封装一份(重发两次)',
  edits=[
   ('internal/daemon/delegation.go', '\t\t\tif r := &rows[i]; r.Type == seal.TypeResult {\n\t\t\t\td.hurryQueued(r, keys)\n\t\t\t\treturn\n', '\t\t\tif r := &rows[i]; r.Type == seal.TypeResult {\n\t\t\t\td.hurryQueued(r, keys)\n'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAQueuedAnswerIsNotQueuedAgainForARedelivery|TestARedeliveredDelegateResendsTheAnswer')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c33-3', group='C33', repo='ANet',
  desc='重投时结果已在队列: 既催队列又另排一份(且 outbox 不去重)',
  edits=[
   ('internal/daemon/delegation.go', '\t\t\tif r := &rows[i]; r.Type == seal.TypeResult {\n\t\t\t\td.hurryQueued(r, keys)\n\t\t\t\treturn\n', '\t\t\tif r := &rows[i]; r.Type == seal.TypeResult {\n\t\t\t\td.hurryQueued(r, keys)\n'),
   ('internal/runtime/interactions/outbox.go', '\tif len(it.Digest) > 0 {\n\t\tvar prior int64', '\tif false && len(it.Digest) > 0 {\n\t\tvar prior int64'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAQueuedAnswerIsNotQueuedAgainForARedelivery|TestARedeliveredDelegateResendsTheAnswer')],
  full=[],
 ),
 dict(
  id='c34-1', group='C34', repo='ANet',
  desc='能力调用: payment-submitted/已结算后收到取消仍取消',
  edits=[
   ('internal/daemon/delegation.go', '\tif cur.PayState == interactions.PaySubmitted || cur.PayState == interactions.PayCompleted {\n\t\treturn\n\t}\n\tif _, running := d.running.Load(ixID); running {', '\tif _, running := d.running.Load(ixID); running {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestCancelAndPaymentRaces|TestACancelAfterASettledPaymentLeavesTheTaskOpen|TestACancelPendingOnAPaymentThatFailsIsCarriedOut|TestACancelAfterASettledPaymentLeavesTheTaskOpen')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c34-2', group='C34', repo='ANet',
  desc='文本任务: payment-submitted/已结算后收到取消仍取消',
  edits=[
   ('internal/daemon/delegation.go', '\tif cur.PayState == interactions.PaySubmitted || cur.PayState == interactions.PayCompleted {\n\t\treturn\n\t}\n\tif _, err := d.CancelTask(ctx, ixID); err != nil && !errors.Is(err, ErrNotCancelable) {\n\t\tlog.Printf("anet: %s: cancel at the requester\'s request: %v", ixID, err)', '\tif _, err := d.CancelTask(ctx, ixID); err != nil && !errors.Is(err, ErrNotCancelable) {\n\t\tlog.Printf("anet: %s: cancel at the requester\'s request: %v", ixID, err)'),
  ],
  tests=[('ANet:./internal/daemon', 'TestCancelAndPaymentRaces|TestACancelAfterASettledPaymentLeavesTheTaskOpen|TestACancelPendingOnAPaymentThatFailsIsCarriedOut')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c35-1', group='C35', repo='ANet',
  desc='finish: 终态行也可被改写(取消后 SetResult 改变状态)',
  edits=[
   ('internal/runtime/interactions/interactions.go', '\tq += ` WHERE id=? AND state NOT IN ` + terminalSQL\n\tres, err := e.Exec(q, string(f.State)', '\tq += ` WHERE id=?`\n\tres, err := e.Exec(q, string(f.State)'),
  ],
  tests=[('ANet:./internal/runtime/interactions', 'TestStateWritesAreGuarded|TestTerminalStates'), ('ANet:./internal/daemon', 'TestACanceledTaskGetsNoReceipt|TestAFailureStatusRacingAResultDoesNotUndoIt')],
  full=['ANet:./internal/runtime/interactions', 'ANet:./internal/daemon'],
 ),
 dict(
  id='c35-2', group='C35', repo='ANet',
  desc='SendMessage: 对终态任务不返回 UnsupportedOperation',
  edits=[
   ('internal/daemon/taskseam.go', '\tif ix.IsTerminal() {\n\t\treturn a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s is %s and takes no new input"', '\tif false && ix.IsTerminal() {\n\t\treturn a2ashape.Task{}, a2ashape.Errorf(a2ashape.ErrUnsupportedOperation, "task %s is %s and takes no new input"'),
  ],
  tests=[('ANet:./internal/daemon', 'TestSendingToAFailedTaskIsUnsupported'), ('ANet:./module/a2a', 'TestOperationsThroughBothBindings')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c35-3', group='C35', repo='ANet',
  desc='阻塞式追问不等 provider 的新回复(after 置 0)',
  edits=[
   ('internal/daemon/taskseam.go', '\trelease()\n\treturn d.finishSend(ctx, sc, ix.ID, after, req, wait)\n}', '\trelease()\n\t_ = after\n\treturn d.finishSend(ctx, sc, ix.ID, 0, req, wait)\n}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestABlockingFollowUpWaitsForTheNextReply|TestAFollowUpWaitsForANewerStateEvent')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c35-4', group='C35', repo='ANet',
  desc='自动回复循环不跳过终态(canceled/rejected)交互',
  edits=[
   ('internal/daemon/autoreply.go', '\t\tif interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {', '\t\tif th.IsCapability || th.Trust == interactions.TrustPublicCap {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAutoReply|TestInputAfterTheEndIsRefused')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c35-5', group='C35', repo='ANet',
  desc='自动回复两层过滤都去掉: ActiveThreads 与循环都不排除终态交互',
  edits=[
   ('internal/daemon/delegation.go', 'ListFilter{Active: true, ExcludeCapability: true,', 'ListFilter{ExcludeCapability: true,'),
   ('internal/daemon/autoreply.go', '\t\tif interactions.State(th.State).IsTerminal() || th.IsCapability || th.Trust == interactions.TrustPublicCap {', '\t\tif th.IsCapability || th.Trust == interactions.TrustPublicCap {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAutoReply|TestInputAfterTheEndIsRefused|TestACanceledTaskGetsNoReceipt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c27-1', group='C27', repo='ANet',
  desc='agentTier: auto 档不计入 agent_daily_max',
  edits=[
   ('internal/daemon/spend.go', '\treturn purpose == module.PurposeTaskAuto || purpose == module.PurposeTaskAgent', '\treturn purpose == module.PurposeTaskAgent'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheSpendingPolicyTiers|TestATaskIsPaidAutomaticallyAtMostOnce'), ('ANet:./module/x402', 'TestARefusedSpendSignsAndRecordsNothing')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c27-2', group='C27', repo='ANet',
  desc='AdmitSpend: 检查与记录不在一把锁下',
  edits=[
   ('internal/daemon/spend.go', '\tb := &d.spend\n\tb.mu.Lock()\n\tdefer b.mu.Unlock()\n\td.loadSpendLocked()\n\tnow := time.Now().UnixMilli()', '\tb := &d.spend\n\td.loadSpendLocked()\n\tnow := time.Now().UnixMilli()'),
  ],
  tests=[('ANet:./internal/daemon', 'TestConcurrentSpendsDoNotBothPass'), ('ANet:sh go test -race -count=1 -run TestConcurrentSpendsDoNotBothPass ./internal/daemon', '')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c27-3', group='C27', repo='ANet',
  desc='AdmitSpend: 通过后不记账(日累计永不增长)',
  edits=[
   ('internal/daemon/spend.go', '\tb.records = append(b.records, spendRecord{at: now, amount: amount, purpose: purpose})\n\treturn nil', '\treturn nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheSpendingPolicyTiers|TestConcurrentSpendsDoNotBothPass')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c27-4', group='C27', repo='ANet',
  desc='x402 Authorize: 忽略 AdmitSpend 的拒绝',
  edits=[
   ('module/x402/pay.go', '\tif err := m.seam.AdmitSpend(opt.PayTo, amount, purpose); err != nil {', '\tif err := m.seam.AdmitSpend(opt.PayTo, amount, purpose); err != nil && false {'),
  ],
  tests=[('ANet:./module/x402', 'TestARefusedSpendSignsAndRecordsNothing'), ('ANet:./internal/daemon', 'TestEverySigningSurfaceIsHeldToTheSpendingPolicy|TestTheSpendingPolicyTiers')],
  full=['ANet:./module/x402', 'ANet:./internal/daemon'],
 ),
 dict(
  id='m-deny-1', group='m(deny)', repo='ANet',
  desc='cancelForPolicy: deny 后不取消活动交互',
  edits=[
   ('internal/daemon/inbound.go', '\t\tif _, err := d.CancelTask(ctx, ix.ID); err != nil {\n\t\t\tlog.Printf("anet: cancel %s after denying %s: %v", ix.ID, aid, err)\n\t\t\tcontinue\n\t\t}', '\t\tif _, err := error(nil), error(nil); err != nil || true {\n\t\t\tcontinue\n\t\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestDenyingAPeerCancelsItsInteractions')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-deny-2', group='m(deny)', repo='ANet',
  desc='authorizeMessage: 已有交互上 deny 对端的后续消息不丢弃',
  edits=[
   ('internal/daemon/receive.go', '\tif ps.denied(m.from) {\n\t\tr := d.drop(dropDenied, nil)\n\t\treturn &r\n\t}\n\tif ix.PeerAID != m.from {', '\tif ix.PeerAID != m.from {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestDenyingAPeerCancelsItsInteractions|TestADeniedPeerWritingToAnUnknownTaskIsAnsweredLikeAStranger')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-deny-3', group='m(deny)', repo='ANet',
  desc='自动回复不再每轮读 deny 名单',
  edits=[
   ('internal/daemon/autoreply.go', '\tps := d.readPeers()\n\tif ps.denied(th.Peer) {\n\t\treturn nil\n\t}\n\t// The exec backend', '\tps := d.readPeers()\n\t// The exec backend'),
  ],
  tests=[('ANet:./internal/daemon', 'TestDenyingAPeerCancelsItsInteractions|TestTheOpenAIBackendReadsTheDenyListEveryTurn|TestAReplyIsNotSentToAPeerDeniedWhileTheBackendWorked')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c42-1', group='C42', repo='ANet',
  desc='safeName: 不中和前导点文件名',
  edits=[
   ('internal/daemon/attachments.go', '\tif strings.HasPrefix(name, ".") {\n\t\tname = "_" + strings.TrimLeft(name, ".")', '\tif false && strings.HasPrefix(name, ".") {\n\t\tname = "_" + strings.TrimLeft(name, ".")'),
  ],
  tests=[('ANet:./internal/daemon', 'TestPullNeutralizesDotfilesAndRepullIsANoOp|TestSafeName')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c42-2', group='C42', repo='ANet',
  desc='pullForbiddenRoots: 不保护数据目录与 exec 工作目录',
  edits=[
   ('internal/daemon/attachments.go', '\tfor _, p := range []string{d.layout.Root, execWorkRoot()} {', '\tfor _, p := range []string{} {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestPullRefusesEmptyRelativeAndProtectedOutDirs|TestPullDoesNotFollowASymlinkAtTheDestination')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c2-1', group='C2', repo='ANet',
  desc='recipientKeys: 超过缓存时限即只信 hub(hub 404 时丢掉仍有效的已存密钥)',
  edits=[
   ('internal/daemon/seal_send.go', '\t\t\tif now-uint64(row.KeysCheckedAt) > keysRevalidateMS {\n\t\t\t\td.revalidateKeys(toAID, pin)\n\t\t\t}', '\t\t\tif now-uint64(row.KeysCheckedAt) > keysRevalidateMS {\n\t\t\t\treturn d.fetchRecipientKeys(ctx, d.config().HubURL, toAID, pin)\n\t\t\t}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestAStoredKeySetIsRecheckedAfterTenMinutes|TestAStoredKeySetThatNoLongerVerifiesIsReplaced|TestTheApprovalQueue|TestAStoredKeySetOutlivesAHubThatNoLongerAnswersForIt')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c2-2', group='C2', repo='ANet',
  desc='ApprovePending: 批准时不记录待批项携带的密钥',
  edits=[
   ('internal/daemon/pending.go', '\tif err := d.notePeer(item.FromAID, kel, keys, "", false); err != nil {', '\t_ = keys\n\tif err := d.notePeer(item.FromAID, kel, nil, "", false); err != nil {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestTheApprovalQueue|TestApprovalVerifiesTheDelegationAgain|TestApprovalAppliesTheRotationGraceNow|TestAnApprovedTaskIsAnsweredWithTheKeysItWasHeldWith')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='c32-1', group='C32', repo='ANetHub',
  desc='hKeysGet: 不做 /fed/v2/keys 联邦查询',
  edits=[
   ('internal/aghub/keys.go', '\tif s.fedKeys != nil {\n\t\t// The federated lookup', '\tif false && s.fedKeys != nil {\n\t\t// The federated lookup'),
  ],
  tests=[('ANetHub:./internal/aghub', 'TestAHubLocalAgentOnAPeerCanBeReachedEncrypted|TestKeysLookupSources')],
  full=['ANetHub:./internal/aghub'],
 ),
 dict(
  id='c32-2', group='C32', repo='ANetHub',
  desc='-test-no-fed-key-lookup 开关失效(scenario.sh 8.6 的 mutation 不再生效)',
  edits=[
   ('cmd/anet-hub/wire_federation.go', '\t\t\tif !d.noFedKeyLookup {', '\t\t\tif true {'),
  ],
  tests=[('ANetHub:./cmd/anet-hub', 'TestTheTestSwitchTurnsOffOnlyTheFederatedKeyLookup')],
  full=['ANetHub:./cmd/anet-hub'],
 ),
 dict(
  id='m-p2p-1', group='m(p2p 旧帧)', repo='ANet',
  desc='anetpeer: 无 V 的旧帧也交给 daemon',
  edits=[
   ('tools/anetpeer/main.go', '\tif f.V < wireVersion {\n\t\t// A sender from before sealed envelopes.', '\tif false && f.V < wireVersion {\n\t\t// A sender from before sealed envelopes.'),
  ],
  tests=[('ANet:./tools/anetpeer', 'TestAnUnversionedDeliveryIsRefused')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='m-p2p-2', group='m(p2p 旧帧)', repo='ANet',
  desc='module/p2p: 无 V 的投递帧也调用 Receive',
  edits=[
   ('module/p2p/transport.go', '\tif f.V < WireVersion {\n\t\tlog.Printf("anet: p2p: ignoring a delivery frame of version', '\tif false && f.V < WireVersion {\n\t\tlog.Printf("anet: p2p: ignoring a delivery frame of version'),
  ],
  tests=[('ANet:./module/p2p', 'TestAnUnversionedDeliveryIsNotHandedToTheDaemon')],
  full=['ANet:./module/p2p'],
 ),
 dict(
  id='m-err-1', group='m(errorReason 映射)', repo='ANet',
  desc='映射表: duplicate_binding → SETTLEMENT_FAILED',
  edits=[
   ('module/x402/check.go', '\tpayment.ReasonDuplicateBinding:  x402a2a.CodeDuplicateNonce,', '\tpayment.ReasonDuplicateBinding:  x402a2a.CodeSettlementFailed,'),
  ],
  tests=[('ANet:./module/x402', 'TestTheErrorReasonTableIsPinned')],
  full=['ANet:./module/x402'],
 ),
 dict(
  id='m-err-2', group='m(errorReason 映射)', repo='ANet',
  desc='x402.payment.error 字符串 DUPLICATE_NONCE 改拼写',
  edits=[
   ('internal/x402a2a/x402a2a.go', '\tCodeDuplicateNonce    = "DUPLICATE_NONCE"', '\tCodeDuplicateNonce    = "DUPLICATED_NONCE"'),
  ],
  tests=[('ANet:./module/x402', 'TestTheErrorReasonTableIsPinned'), ('ANet:./internal/x402a2a', ''), ('ANet:./internal/a2ashape', 'TestKeyStrings')],
  full=['ANet:./module/x402'],
 ),
]
