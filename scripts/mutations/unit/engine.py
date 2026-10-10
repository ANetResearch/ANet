# engine.py: mutations of the outbound engine (engine §8.2, ENG-M) for scripts/mutations/unitmut.py (see its
# header for the format). Each takes out, or turns around, one rule of the engine; the cases named in §8.2
# for it must go red. Group ENG-1a is the outbound state machine's; the others are added by the packages
# that bring theirs (ENG-1b re-sealing and one-shot envelopes, ENG-1c receiving, ENG-1d closing).
# A mutation that no longer applies reports APPLY-ERR: fix its edit to the moved code rather than dropping it.

D = 'ANet:./internal/daemon'
IX = 'ANet:./internal/runtime/interactions'

MUTS = [
 dict(
  id='ENG-M1', group='ENG-1a', repo='ANet',
  desc='A8 不先落盘：版本写入时不置 attempting，字节离开时行未标"尝试中"',
  edits=[
   ('internal/runtime/interactions/outbox.go', '''	   sealed_notice_at=?, attempting=1 WHERE id=?`''', '''	   sealed_notice_at=?, attempting=0 WHERE id=?`'''),
   ('internal/runtime/interactions/outbox.go', '''	_, err := t.tx.Exec(`UPDATE outbox SET attempting=1 WHERE id=?`, id)''', '''	_, err := t.tx.Exec(`UPDATE outbox SET attempting=0 WHERE id=?`, id)'''),
  ],
  tests=[(D, 'TestOutboxStateMachineModel|TestADelegationWhoseAttemptWasNeverRecordedIsNotWithdrawn')],
  full=[D],
 ),
 dict(
  id='ENG-M2', group='ENG-1a', repo='ANet',
  desc='传输非当前版本：新版本不替换行上的信封，重传仍发最早的字节',
  edits=[
   ('internal/runtime/interactions/outbox.go', '''`UPDATE outbox SET envelope=?, cur_ver=?, sealed_kid=?''', '''`UPDATE outbox SET envelope=COALESCE(envelope, ?), cur_ver=?, sealed_kid=?'''),
  ],
  tests=[(D, 'TestET9_ARetransmissionSendsTheCurrentVersion')],
  full=[D],
 ),
 dict(
  id='ENG-M5', group='ENG-1a', repo='ANet',
  desc='hub 回 queued 的版本仍被判为需要经 hub 重传',
  edits=[
   ('internal/daemon/outbox_step.go', '''		if hubStateOf(v) != interactions.OutboxUnknown {
			return false // queued, or not asked yet''', '''		if hubStateOf(v) == "" {
			return false // queued, or not asked yet'''),
  ],
  tests=[(D, 'TestDLVT9_AQueuedVersionIsAskedAboutNotSentAgain|TestOutboxStateMachineModel')],
  full=[D],
 ),
 dict(
  id='ENG-M14', group='ENG-1a', repo='ANet',
  desc='hub 4xx 回答把版本改回"未交出"(恢复 mayHaveDelivered)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		res, handed, serr := d.relaySendV3(ctx, wire, choice)
		if handed {
			a.hand(ver)
		}''', '''		res, handed, serr := d.relaySendV3(ctx, wire, choice)
		if handed && hubStatus(serr)/100 != 4 {
			a.hand(ver)
		}'''),
  ],
  tests=[(D, 'TestDLVT37_A404AfterHandingOverIsUnverified')],
  full=[D],
 ),
 dict(
  id='ENG-M15', group='ENG-1a', repo='ANet',
  desc='拨号、DNS 或 TLS 失败也判交出',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		res, handed, serr := d.relaySendV3(ctx, wire, choice)
		if handed {
			a.hand(ver)
		}''', '''		res, handed, serr := d.relaySendV3(ctx, wire, choice)
		if handed || serr != nil {
			a.hand(ver)
		}'''),
  ],
  tests=[(D, 'TestDLVT25_NeverHandedOver|TestAnAbandonedDelegationThatReachedNobodyIsUnavailable|TestRedteamSI10_ACancelBeforeDeliveryWithdrawsTheDelegation')],
  full=[D],
 ),
 dict(
  id='ENG-M16', group='ENG-1a', repo='ANet',
  desc='冻结且从未交出的委派在尝试结束后不撤回',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''	// A cancel that came while the attempt ran froze the row: if none of
	// it left, it is withdrawn now (A12, ENG-O5).
	if err := d.withdrawFrozen(row); err != nil {''', '''	// A cancel that came while the attempt ran froze the row: if none of
	// it left, it is withdrawn now (A12, ENG-O5).
	if err := error(nil); err != nil {'''),
  ],
  tests=[(D, 'TestSpotACancelRacingAnAttemptGoesOutBehindIt')],
  full=[D],
 ),
 dict(
  id='ENG-M19', group='ENG-1a', repo='ANet',
  desc='事后通知不经出站队列：发一次即从队列删去',
  edits=[
   ('internal/daemon/outbox_queue.go', '''	if _, err := d.queueSendAs(ctx, wireSend{to: to, typ: seal.TypeStatus, ix: ix, kind: kind, body: body}, nil); err != nil {''', '''	if id, err := d.queueSendAs(ctx, wireSend{to: to, typ: seal.TypeStatus, ix: ix, kind: kind, body: body}, nil); err == nil {
		_ = d.deliverQueued(ctx, id)
		_ = d.ix.Update(func(tx *interactions.Tx) error { _, err := tx.DeleteOutboxRow(id); return err })
	} else {'''),
  ],
  tests=[(D, 'TestET8_ANoticeTheHubLostGoesAgain')],
  full=[D],
 ),
 dict(
  id='ENG-M20', group='ENG-1a', repo='ANet',
  desc='事后通知在没有能力时改走 kind 0(首次联系)',
  edits=[
   ('internal/daemon/outbox_queue.go', '''	choice, err := d.accessChoiceFor(to, ix, false, accessNotice, now)''', '''	choice, err := d.accessChoiceFor(to, ix, false, accessOrdinary, now)'''),
   ('internal/daemon/outbox_attempt.go', '''	if row.Type == seal.TypeStatus && isNoticeKind(row.Kind) {
		return accessNotice
	}''', '''	if row.Type == seal.TypeStatus && isNoticeKind(row.Kind) {
		return accessOrdinary
	}'''),
  ],
  tests=[(D, 'TestET15_ANoticeWithoutACapabilityIsNotSent')],
  full=[D],
 ),
 dict(
  id='ENG-M23', group='ENG-1a', repo='ANet',
  desc='放弃时任务另写一次状态，不只经统一终态写入一次',
  edits=[
   ('internal/daemon/undelivered.go', '''	err = tx.Finish(ix.ID, interactions.Finish{State: interactions.StateFailed,''', '''	_, _ = tx.SetState(ix.ID, interactions.StateFailed, cause)
	err = tx.Finish(ix.ID, interactions.Finish{State: interactions.StateFailed,'''),
  ],
  tests=[(D, 'TestET31_EveryEndGoesThroughTheTerminalWrite')],
  full=[D],
 ),
 dict(
  id='ENG-M24', group='ENG-1a', repo='ANet',
  desc='确认或放弃删除行时不删除其版本',
  edits=[
   ('internal/runtime/interactions/outbox.go', '''	if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=?`, id); err != nil {
		return false, err
	}''', '''	if _, err := t.tx.Exec(`SELECT 1 WHERE ?`, id); err != nil {
		return false, err
	}'''),
  ],
  tests=[(IX, 'TestARowGoesWithItsVersionsAndTheirKeys')],
  full=[IX],
 ),
 dict(
  id='ENG-M25', group='ENG-1a', repo='ANet',
  desc='产生新版本时不删除被取代的未交出版本',
  edits=[
   ('internal/runtime/interactions/outbox.go', '''	if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=? AND handed_at IS NULL`, id); err != nil {''', '''	if _, err := t.tx.Exec(`DELETE FROM outbox_ver WHERE row_id=? AND handed_at IS NULL AND 0`, id); err != nil {'''),
  ],
  tests=[(IX, 'TestANewVersionReplacesOnlyOneNeverHanded|TestARowGoesWithItsVersionsAndTheirKeys')],
  full=[IX],
 ),
 dict(
  id='ENG-M26', group='ENG-1a', repo='ANet',
  desc='版本上限溢出时逐出最新的版本(当前版本之前的那个)而不是最早的',
  edits=[
   ('internal/runtime/interactions/outbox.go', '''	   ORDER BY handed_at, ver LIMIT MAX(0,''', '''	   ORDER BY ver DESC LIMIT MAX(0,'''),
  ],
  tests=[(IX, 'TestTheVersionCapKeepsTheCurrentOne')],
  full=[IX],
 ),
 dict(
  id='ENG-M27', group='ENG-1a', repo='ANet',
  desc='accessFor 的结果按行记下并在后续尝试沿用',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		choice, err = d.accessChoiceFor(row.ToAID, row.IX, row.Type == seal.TypeDelegate, accessModeOf(row), a.now)''', '''		if c, ok := keptChoice[row.ID]; ok {
			choice, err = c, nil
		} else {
			choice, err = d.accessChoiceFor(row.ToAID, row.IX, row.Type == seal.TypeDelegate, accessModeOf(row), a.now)
			keptChoice[row.ID] = choice
		}'''),
   ('internal/daemon/outbox_attempt.go', '''// noKeysDetail is what -no_keys says of why''', '''var keptChoice = map[int64]AccessChoice{}

// noKeysDetail is what -no_keys says of why'''),
  ],
  tests=[(D, 'TestET19_ARefusedCapability')],
  full=[D],
 ),
 dict(
  id='ENG-M29', group='ENG-1a', repo='ANet',
  desc='同一尝试内 hub 发送另封装的一个版本(与 p2p 的不同)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		// A11: the hub.
		res, handed, serr := d.relaySendV3(ctx, wire, choice)''', '''		// A11: the hub.
		if b, _, e := d.ix.OutboxContent(row.ID); e == nil {
			if sv, e := d.sealVersion(ctx, row, b, false); e == nil {
				if w, e := seal.SetAccess(sv.env, access); e == nil {
					wire = w
				}
			}
		}
		res, handed, serr := d.relaySendV3(ctx, wire, choice)'''),
  ],
  tests=[(D, 'TestET21_BothPathsCarryOneVersion')],
  full=[D],
 ),
 dict(
  id='ENG-M30', group='ENG-1a', repo='ANet',
  desc='状态查询并入别的行(别的交互)的版本',
  edits=[
   ('internal/daemon/dack_status.go', '''	now := d.nowMS()
	views := d.verViews(row.ID, stored, hub, now)''', '''	now := d.nowMS()
	_ = d.ix.Update(func(tx *interactions.Tx) error {
		all, _ := tx.AllOutboxRows()
		for _, r := range all {
			if r.ID != row.ID {
				vs, _ := tx.OutboxVersions(r.ID)
				stored = append(stored, vs...)
			}
		}
		return nil
	})
	views := d.verViews(row.ID, stored, hub, now)'''),
  ],
  tests=[(D, 'TestDLVT33_AStatusQueryIsAboutOneRow')],
  full=[D],
 ),
 dict(
  id='ENG-M35', group='ENG-1a', repo='ANet',
  desc='status 的 done 不经 VerifyDAck 即确认',
  edits=[
   ('internal/daemon/dack_status.go', '''				if _, o, n, noteOK, ok := seal.VerifyDAck(keys, dack); ok {''', '''				if _, o, n, noteOK, ok := seal.VerifyDAck(keys, dack); ok || o == 0 {'''),
  ],
  tests=[(D, 'TestET24_AForgedDoneConfirmsNothing|FuzzDecodeStatusAnswer')],
  full=[D],
 ),
 dict(
  id='ENG-M36', group='ENG-1a', repo='ANet',
  desc='last_error 不写等待码前缀(写 -hub_<状态码>)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		case ansWait:
			a.fail(lastErrorOf(ans.code))''', '''		case ansWait:
			a.fail(errHubPrefix + strconv.Itoa(ans.status))'''),
  ],
  tests=[(D, 'TestET25_AHubWaitIsTheRowsWait')],
  full=[D],
 ),
 dict(
  id='ENG-M42', group='ENG-1a', repo='ANet',
  desc='deliveryWait 忽略闸门、只读 last_error',
  edits=[
   ('internal/daemon/outbox_step.go', '''	if w := gateDeliveryWait(gate); w != "" && needsTransmit(row, vers) {''', '''	if w := gateDeliveryWait(gate); false && w != "" && needsTransmit(row, vers) {'''),
  ],
  tests=[(D, 'TestET32_TheGateIsTheWaitWhileItIsClosed|TestGateReopensFromPoll')],
  full=[D],
 ),
 dict(
  id='ENG-M44a', group='ENG-1a', repo='ANet',
  desc='closeInteractionsTx(identity_stopped) 也入队通知',
  edits=[
   ('internal/daemon/outbox_seams.go', '''func (c closeCause) notifies() bool { return c == closeKeyCompromise || c == closeLocalStateLost }''', '''func (c closeCause) notifies() bool { return true }'''),
  ],
  tests=[(D, 'TestET35_AStopClosesEverythingAndTellsNobody')],
  full=[D],
 ),
 dict(
  id='ENG-M44b', group='ENG-1a', repo='ANet',
  desc='closeInteractionsTx 不删交互私钥',
  edits=[
   ('internal/daemon/outbox_seams.go', '''	if _, err := tx.DeleteAllIXKeys(); err != nil {''', '''	if _, err := int64(0), error(nil); err != nil {'''),
  ],
  tests=[(D, 'TestET35_AStopClosesEverythingAndTellsNobody')],
  full=[D],
 ),
 dict(
  id='ENG-M45', group='ENG-1a', repo='ANet',
  desc='hub 404 时不刷新即放弃',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''			if !resentForHome && d.refreshOnUnknown(ctx, row.ID, row.ToAID, capHome) {''', '''			if false && !resentForHome && d.refreshOnUnknown(ctx, row.ID, row.ToAID, capHome) {'''),
  ],
  tests=[(D, 'TestUnknownRecipientRefreshesOnce')],
  full=[D],
 ),
]

# ENG-1a review: rules the integration cases reached only through other guards, each now held by a test of its own.
MUTS += [
 dict(
  id='eng1a-c2-peer', group='ENG-1a', repo='ANet',
  desc='隐式确认不看发信方是否为该交互的对端(ENG-C2)',
  edits=[
   ('internal/daemon/outbox_seams.go', '''	if cur.Role != interactions.RoleOutbound || cur.PeerAID != from {''', '''	if cur.Role != interactions.RoleOutbound {'''),
   ('internal/daemon/outbox_seams.go', '''		if r.Type != seal.TypeDelegate || r.ToAID != from {''', '''		if r.Type != seal.TypeDelegate {'''),
  ],
  tests=[(D, 'TestOnlyThePeerConfirmsADelegationImplicitly')],
  full=[D],
 ),
 dict(
  id='eng1a-a12-freeze', group='ENG-1a', repo='ANet',
  desc='同一尝试内交出过之后遇到冻结即返回,不做 A12(不记交出、不清 attempting)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''			if !a.marked {
				return d.withdrawFrozen(row)
			}
			break rounds''', '''			return d.withdrawFrozen(row)'''),
  ],
  tests=[(D, 'TestAFreezeBetweenTwoSendsOfAnAttemptRecordsWhatLeft')],
  full=[D],
 ),
 dict(
  id='eng1a-join-2xx-wire', group='ENG-1a', repo='ANet',
  desc='加入流程把"2xx 却带另一线协号"当作成功(CORE-V1)',
  edits=[
   ('internal/daemon/hubjoin.go', '''	if we := hubWireCheck("", r); we != nil && r.Status != 426 {''', '''	if we := hubWireCheck("", r); we != nil && r.Status != 426 && (r.Status < 200 || r.Status >= 300) {'''),
  ],
  tests=[(D, 'TestHubAnswersMapAsUXR6')],
  full=[D],
 ),
 dict(
  id='eng1a-revoke-2xx-wire', group='ENG-1a', repo='ANet',
  desc='吊销推送把"2xx 却带另一线协号"当作已推送(CORE-V1)',
  edits=[
   ('internal/daemon/revoke_push.go', '''		case rep.Status != 0 && hubWireCheck(hub, rep) != nil:''', '''		case false && rep.Status != 0 && hubWireCheck(hub, rep) != nil:'''),
  ],
  tests=[(D, 'TestARevokePushAnsweredOverAnotherWireIsNotPushed')],
  full=[D],
 ),
 dict(
  id='eng1a-status-wire', group='ENG-1a', repo='ANet',
  desc='状态查询收下另一线协号的 200 答复(CORE-V1)',
  edits=[
   ('internal/daemon/dack_status.go', '''	if we := hubWireCheck(hub, rep); we != nil {
		return failed(nil, 0)
	}''', '''	_ = hubWireCheck'''),
  ],
  tests=[(D, 'TestAStatusAnswerOverAnotherWireIsNoAnswer')],
  full=[D],
 ),
 dict(
  id='eng1a-status-via', group='ENG-1a', repo='ANet',
  desc='同一行在不同 via_hub 的版本合进一个状态查询(ENG-D3)',
  edits=[
   ('internal/daemon/dack_status.go', '''		for j < len(out) && out[j].viaHub != v.ViaHub {''', '''		for j < len(out) && false {'''),
  ],
  tests=[(D, 'TestStatusQueriesGoOneViaHubAtATime')],
  full=[D],
 ),
 dict(
  id='eng1a-stale-hub', group='ENG-1a', repo='ANet',
  desc='版本所记 hub 已不是当前 hub 时不视同 unknown(ENG-D5)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''		if v.RID != nil && (v.Hub != hub || (failing != 0''', '''		if v.RID != nil && ((failing != 0'''),
  ],
  tests=[(D, 'TestAVersionAtAnotherHubOrUnansweredForAnHourIsUnknown')],
  full=[D],
 ),
 dict(
  id='eng1a-query-hubok', group='ENG-1a', repo='ANet',
  desc='hub 不可询问(线协不符、无 hub)时仍决定状态查询(A4)',
  edits=[
   ('internal/daemon/outbox_step.go', '''	if row.CheckAt != 0 && row.CheckAt <= in.Now && in.HubOK {''', '''	if row.CheckAt != 0 && row.CheckAt <= in.Now {'''),
  ],
  tests=[(D, 'TestOutboxStepOrder')],
  full=[D],
 ),
 dict(
  id='eng1a-close-notice-confirmed', group='ENG-1a', repo='ANet',
  desc='紧急关闭不通知委派已确认的交互的对端(ENG-K15)',
  edits=[
   ('internal/daemon/outbox_seams.go', '''		peerKnows := o.Role == interactions.RoleInbound || !queued || handed''', '''		peerKnows := o.Role == interactions.RoleInbound || handed'''),
  ],
  tests=[(D, 'TestAnEmergencyCloseTellsThePeersThatMayKnow')],
  full=[D],
 ),
 dict(
  id='eng1a-close-notice-all', group='ENG-1a', repo='ANet',
  desc='紧急关闭也为从未交出的委派通知对端(ENG-K15)',
  edits=[
   ('internal/daemon/outbox_seams.go', '''		if cause.notifies() && peerKnows {''', '''		if cause.notifies() && (peerKnows || true) {'''),
  ],
  tests=[(D, 'TestAnEmergencyCloseTellsThePeersThatMayKnow')],
  full=[D],
 ),
 dict(
  id='eng1a-c6-retry', group='ENG-1a', repo='ANet',
  desc='带 retry_after 的拒绝,句子不代入等待时间(ENG-C6)',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''			args = a2ashape.Args{"retry": time.Duration(n.RetryAfterS) * time.Second}''', '''			args = nil'''),
  ],
  tests=[(D, 'TestARefusalWithAWaitSaysWhenToSendAgain')],
  full=[D],
 ),
 dict(
  id='eng1a-notice-limit', group='ENG-1a', repo='ANet',
  desc='事后通知不经 noticeLimiter 即入队(ENG-Q7)',
  edits=[
   ('internal/daemon/outbox_queue.go', '''	if !d.notices.allow(to, now) {''', '''	if false {'''),
  ],
  tests=[(D, 'TestANoticePastTheLimitIsNotQueued')],
  full=[D],
 ),
 dict(
  id='eng1a-gate-reread', group='ENG-1a', repo='ANet',
  desc='尝试内交出字节之前不再读闸门(ENG-K13):会话期间关闭的闸门挡不住信封',
  edits=[
   ('internal/daemon/outbox_attempt.go', '''	return d.frozenNow(id) || !d.identityGate().Open''', '''	return d.frozenNow(id)'''),
  ],
  tests=[(D, 'TestAGateThatClosesMidAttemptHoldsTheEnvelope')],
  full=[D],
 ),
 # ---- ENG-1c: the receive pipeline v3 (engine §4, §8.2)
 dict(
  id='ENG-M17', group='ENG-1c', repo='ANet',
  desc='重投授权失败的重复件给 tok(2)(而不是 tok(1) 只抑制副作用)',
  edits=[
   ('internal/daemon/receive.go', '''	if !d.mayRedeliver(m, ix) {
		d.count(redeliveryNotAuthorized)
		return dup
	}''', '''	if !d.mayRedeliver(m, ix) {
		d.count(redeliveryNotAuthorized)
		return d.drop(dropNotAllowed, nil).at(rxStep9)
	}'''),
  ],
  tests=[(D, 'TestET14_ADelegationTakenThenUnlistedIsStillTaken')],
  full=[D],
 ),
 dict(
  id='ENG-M18', group='ENG-1c', repo='ANet',
  desc='重复件不给令牌',
  edits=[
   ('internal/daemon/rx_token.go', '''	dropDuplicate:     tokProcessed,''', '''	dropDuplicate:     tokNone,'''),
  ],
  tests=[(D, 'TestRxTokenForTable|TestRxTokenForReplayAndOneShotProperty|TestET14_ADelegationTakenThenUnlistedIsStillTaken')],
  full=[D],
 ),
 dict(
  id='ENG-M21', group='ENG-1c', repo='ANet',
  desc='重投的已答复委派在答复已不在队列时重新封装一份新结果',
  edits=[
   ('internal/daemon/delegation.go', '''		if !d.answerQueued(prior) {
			return true
		}''', '''		if !d.answerQueued(prior) {
			if payload, err := (&delegation.ResultResp{Status: delegation.StatusDone, Deliverable: prior.Result,
				Receipt: prior.Receipt}).Marshal(); err == nil {
				qctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
				if id, err := d.queueSendAs(qctx, wireSend{to: prior.PeerAID, typ: seal.TypeResult, ix: prior.ID, body: payload}, nil); err == nil {
					_ = d.deliverQueued(qctx, id)
				}
				cancel()
			}
			return true
		}'''),
  ],
  tests=[(D, 'TestET16_AnAnsweredDelegationRedeliveredIsAnsweredOnce')],
  full=[D],
 ),
 dict(
  id='ENG-M22', group='ENG-1c', repo='ANet',
  desc='隐式确认也确认委派以外的行(text、cancel)',
  edits=[
   ('internal/daemon/outbox_seams.go', '''		if r.Type != seal.TypeDelegate || r.ToAID != from {''', '''		if r.ToAID != from {'''),
  ],
  tests=[(D, 'TestET17_AStatusConfirmsOnlyTheDelegation')],
  full=[D],
 ),
 dict(
  id='ENG-M31', group='ENG-1c', repo='ANet',
  desc='第一层成功之后的失败在直连路径按 T',
  edits=[
   ('internal/daemon/receive.go', '''		d.misused(issued, step, reason, oneShot, "")
		if reason == seal.ReasonSealedToClosedInteraction {''', '''		d.misused(issued, step, reason, oneShot, "")
		if path.direct {
			return d.transient(transientDirectUnopened, err).at(step)
		}
		if reason == seal.ReasonSealedToClosedInteraction {'''),
  ],
  tests=[(D, 'TestEachFailureIsAnsweredAlikeOnBothPaths')],
  full=[D],
 ),
 dict(
  id='ENG-M32', group='ENG-1c', repo='ANet',
  desc='恢复未知交互消息的 T 暂扣(不 ack、不给令牌)',
  edits=[
   ('internal/daemon/receive.go', '''			m.cm, m.tombstone = cm, true
			return nil
		}
		r := d.drop(dropUnknownIX, nil)''', '''			m.cm, m.tombstone = cm, true
			return nil
		}
		r := d.transient("t-unknown-ix", nil)'''),
  ],
  tests=[(D, 'TestRedteamSI10_AStrangerCannotKeepAnOnlineNodesMailboxFull|TestAMessageForAnUnknownTaskGetsTaskNotFound')],
  full=[D],
 ),
 dict(
  id='ENG-M33', group='ENG-1c', repo='ANet',
  desc='ixLookup 读库错误按 sealed-to-closed-interaction(ack 并给 tok(1))',
  edits=[
   ('internal/daemon/receive.go', '''			return d.transient(transientStore, err).at(rxStep3a)''', '''			return rxResult{class: rxAccepted, reason: seal.ReasonSealedToClosedInteraction, step: rxStep3a}'''),
  ],
  tests=[(D, 'TestET22_AStoreErrorAtTheSecondLayerIsTemporary')],
  full=[D],
 ),
 dict(
  id='ENG-M34', group='ENG-1c', repo='ANet',
  desc='第 1a 步复核失败的能力仍按"经能力送达"处理,其后的失败即吊销它',
  edits=[
   ('internal/daemon/receive.go', '''	if k.Class == 0 {
		return nil, k, true
	}''', '''	if k.Class == 0 {
		return issued, k, true
	}'''),
  ],
  tests=[(D, 'TestET23_ACredentialThatFailsAtTheRecipientIsNoCapability')],
  full=[D],
 ),
 dict(
  id='ENG-M41', group='ENG-1c', repo='ANet',
  desc='合并对端紧急 rot 时不吊销 caps_issued 中给该对端的行(与 SS M46 同一处)',
  edits=[('internal/runtime/interactions/caps.go', '\t_, err := revokeIssued(e, IssuedFilter{Grantee: aid}, now)\n\treturn err\n}', '\treturn nil\n}')],
  tests=[(D, 'TestPeerEmergencyRotRevokesIssuedCaps|TestET29_APeersEmergencyRotationGoesWithTheMessage')],
  full=[D, IX],
 ),
 dict(
  id='eng1c-tok-before-record', group='ENG-1c', repo='ANet',
  desc='第 9 步的拒收不记入内存拒收表(重投的副本再走一遍并可能得到别的结论)',
  edits=[
   ('internal/daemon/receive.go', '''	d.refused.add(replayKey(m.from, m.mid), tok.Note.Reason)
	return res''', '''	return res'''),
  ],
  tests=[(D, 'TestAMessageForAnUnknownTaskGetsTaskNotFound|TestRedteamUnknownIXFromAStrangerIsNotReworked')],
  full=[D],
 ),
 dict(
  id='eng1c-step6half', group='ENG-1c', repo='ANet',
  desc='去掉第 6½ 步(不核对见证通告)',
  edits=[
   ('internal/daemon/receive.go', '''	err := seal.CheckNotice(in, kel, d.hubKELs, arrival)''', '''	var err error'''),
  ],
  tests=[(D, 'TestStep6HalfChecksTheNoticeOfEveryEnvelope|TestEachFailureIsAnsweredAlikeOnBothPaths')],
  full=[D],
 ),
 dict(
  id='eng1c-arrival-now', group='ENG-1c', repo='ANet',
  desc='信箱路径的 arrival 取 now,不取 hub 的 accepted_at(KEL D-20)',
  edits=[
   ('internal/daemon/receive.go', '''		arrival = hubArrival(path.acceptedAt, in.TS, now)''', '''		arrival = now'''),
  ],
  tests=[(D, 'TestAHeldDelegationKeepsItsArrival')],
  full=[D],
 ),
 dict(
  id='eng1c-r9', group='ENG-1c', repo='ANet',
  desc='第 6 步合并新事件时不执行 R-9(KEL D-23)',
  edits=[
   ('internal/daemon/receive.go', '''		if err := identity.CheckNewEvents(stored, inner, now, nil); err != nil {''', '''		if err := identity.CheckNewEvents(stored, inner, now, nil); false && err != nil {'''),
  ],
  tests=[(D, 'TestStep6RefusesAKeyEventFromTheFuture')],
  full=[D],
 ),
 dict(
  id='eng1c-4a', group='ENG-1c', repo='ANet',
  desc='去掉第 4a 步(按类型的键出现规则)',
  edits=[
   ('internal/daemon/receive.go', '''	if !kindKeysOK(op, preview) {''', '''	if false && !kindKeysOK(op, preview) {'''),
  ],
  tests=[(D, 'TestStepFourAChecksTheKeysOfEachType|TestEachFailureIsAnsweredAlikeOnBothPaths')],
  full=[D],
 ),
 dict(
  id='eng1c-stick-under-lock', group='ENG-1c', repo='ANet',
  desc='拒收在释放 (from, mid) 锁之后才落盘(等锁的副本看不到它,可能被判定两次)',
  edits=[
   ('internal/daemon/receive.go', '''	// A refusal is made to stick before the locks are released, so the copy
	// waiting on them finds it [redteam:F5][redteam:F26].
	defer func() { result = d.stickRefusal(result, m) }()
''', ''),
   ('internal/daemon/receive.go', '''			result = d.answerEnvelope(result, ack, oneShot)''', '''			if m != nil {
				result = d.stickRefusal(result, m)
			}
			result = d.answerEnvelope(result, ack, oneShot)'''),
  ],
  tests=[(D, 'TestSpotCopiesRacingAnAllowAreDecidedOnce')],
  full=[D],
 ),
 dict(
  id='eng1c-tombstone', group='ENG-1c', repo='ANet',
  desc='委派不查取消墓碑(DLV M11)',
  edits=[
   ('internal/daemon/receive.go', '''	} else if tomb {
		r := d.drop(dropCanceled, nil)''', '''	} else if false && tomb {
		r := d.drop(dropCanceled, nil)'''),
  ],
  tests=[(D, 'TestDLVT11_ACancelAheadOfItsDelegationStopsIt')],
  full=[D],
 ),
 dict(
  id='eng1c-closed-ix', group='ENG-1c', repo='ANet',
  desc='封给已关闭交互的信按 P 处理、给 tok(2)(DLV M13)',
  edits=[
   ('internal/daemon/receive.go', '''		if reason == seal.ReasonSealedToClosedInteraction {
			// Sealed to an interaction this node has closed''', '''		if false && reason == seal.ReasonSealedToClosedInteraction {
			// Sealed to an interaction this node has closed'''),
  ],
  tests=[(D, 'TestDLVT13_AResultToAClosedInteractionIsTaken|TestRxTokenForTable')],
  full=[D],
 ),
 dict(
  id='eng1c-direct-dack', group='ENG-1c', repo='ANet',
  desc='直连 ack 不带提交时的 tok(1)(ENG-V2)',
  edits=[
   ('internal/daemon/receive.go', '''	m.ackNow(b)
	m.ackNow = nil''', '''	_ = b
	m.ackNow(nil)
	m.ackNow = nil'''),
  ],
  tests=[(D, 'TestEachFailureIsAnsweredAlikeOnBothPaths|TestDLVT4_AHubThatLosesWhatItTook')],
  full=[D],
 ),
 dict(
  id='eng1c-card-no-grant', group='ENG-1c', repo='ANet',
  desc='卡片请求没有带回信 grant 也照样回答(ENG-K2)',
  edits=[
   ('internal/daemon/receive.go', '''	grant, ok := d.cardRequestGrant(in, keys, now)
	if !ok {
		return d.drop(cardGetNoGrant, nil).at(rxStep9)
	}''', '''	grant, _ := d.cardRequestGrant(in, keys, now)'''),
  ],
  tests=[(D, 'TestACardRequestIsAnsweredUnderItsOwnGrant')],
  full=[D],
 ),
 dict(
  id='eng1c-sender-tx', group='ENG-1c', repo='ANet',
  desc='发送方 KEL 与钥集在第 10 步事务之外先行写入(事务回滚时它们与连带的吊销留下)',
  edits=[
   ('internal/daemon/receive.go', '''	ps := d.readPeers()
	err := d.ix.Update(func(tx *interactions.Tx) error {
		claimed, err := tx.ClaimReplay(m.from, m.mid, m.exp)''', '''	ps := d.readPeers()
	_ = d.ix.Update(func(tx *interactions.Tx) error { return d.recordSenderTx(tx, m, ps) })
	err := d.ix.Update(func(tx *interactions.Tx) error {
		claimed, err := tx.ClaimReplay(m.from, m.mid, m.exp)'''),
   ('internal/daemon/receive.go', '''		if err := d.recordSenderTx(tx, m, ps); err != nil {
			return err
		}
''', ''),
  ],
  tests=[(D, 'TestET29_APeersEmergencyRotationGoesWithTheMessage')],
  full=[D],
 ),
]
