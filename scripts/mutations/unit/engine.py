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
