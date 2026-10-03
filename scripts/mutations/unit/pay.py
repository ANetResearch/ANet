# pay.py: mutations of the hub and payment privacy work that must turn their tests red (M-PAY-*).
# For scripts/mutations/unitmut.py (see its header for the format); entries name the repository
# they edit. Later PAY work items append their rows here.
#
# PAY-2's own (pay2-*) follow: the ANetCore evidence v2 objects and commitments, the card and task
# binding bodies, the v2 result check, the supply epoch payload, MonoClock and the strict carried
# decoders. Each names the test that kills it.
#
# M-PAY-1 and M-PAY-2 are the ANetCore liability tree (package liabtree). Their key tests are the
# LIAB-1 refusals the golden vector carries for them: a path whose sums were moved between two
# levels (the root sum unchanged), two sibling accounts shown different splits of one parent sum
# (which also catches a node hash over the parent's total alone), and a tree whose sums wrap
# around 2^64.

LIAB_MOVED = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_moved_(sum|split)$')]
LIAB_WRAP = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_sum_wraps$')]
NUL_T = [('ANetCore:./wire', '^TestPreimageRejectsNUL$'), ('ANetCore:./evidence', '^(TestTheCommitmentsAreTheirPreimages|TestEV2_1)$')]
RESULT_T = [('ANetCore:./delegation', '^(TestAResultMustBeTheAnswerToTheRequestItClaims|TestTheReceiptsDayWindow)$')]
BINDING_T = [('ANetCore:./delegation', '^TestCheckTaskBindingRefusals$'), ('ANetCore:./golden', '^TestTASKDOC_3$')]
CARRIED_T = [('ANetCore:./wire', '^TestCarriedDecodersRefuseUnregisteredKeys$')]

MUTS = [
 dict(id='M-PAY-1', group='PAY', repo='ANetCore', desc='负债树节点哈希不覆盖子和(L26 去掉 u64be(L.sum)、u64be(R.sum))',
  edits=[('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], l.Sum)\n', ''),
         ('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], r.Sum)\n', '')],
  tests=LIAB_MOVED, full=['ANetCore:./liabtree']),
 dict(id='M-PAY-2', group='PAY', repo='ANetCore', desc='负债树去掉和的溢出检查(addSum 直接相加)',
  edits=[('liabtree/liabtree.go', '\tif a > MaxSum || b > MaxSum || a+b > MaxSum {\n\t\treturn 0, false\n\t}\n', '')],
  tests=LIAB_WRAP, full=['ANetCore:./liabtree']),
 # ---- PAY-2: evidence v2
 dict(id='pay2-nul', group='PAY-2', repo='ANetCore', desc='证据承诺函数接受含 U+0000 的字段(CORE-E9,L27/L28/L31/L33)',
  edits=[('evidence/commit.go', '\tif strings.IndexByte(v, 0) >= 0 {', '\tif false && strings.IndexByte(v, 0) >= 0 {')],
  tests=NUL_T, full=['ANetCore:./evidence', 'ANetCore:./wire']),
 dict(id='pay2-ix-label', group='PAY-2', repo='ANetCore', desc='ix_commit 不带 L27 标签',
  edits=[('evidence/commit.go', '\th.Write([]byte(labelIXCommit))\n', '')],
  tests=[('ANetCore:./evidence', '^(TestEV2_1|TestTheCommitmentsAreTheirPreimages)$')], full=['ANetCore:./evidence']),
 dict(id='pay2-canonical', group='PAY-2', repo='ANetCore', desc='证据对象解码不要求规范编码(CORE-E2)',
  edits=[('evidence/evidence.go', '\tif err != nil || !bytes.Equal(back, raw) {', '\tif _ = back; err != nil {')],
  tests=[('ANetCore:./evidence', '^(TestTheReceiptDecodersAreStrict|TestReviewRatingAndCommentBounds)$')], full=['ANetCore:./evidence']),
 dict(id='pay2-receipt-signer', group='PAY-2', repo='ANetCore', desc='回执验签不要求签名方等于 provider',
  edits=[('evidence/evidence.go', '\tif w.Env.SignerAID != body.Provider {', '\tif false && w.Env.SignerAID != body.Provider {')],
  tests=[('ANetCore:./evidence', '^(TestOnlyTheProviderSignsItsReceipt|TestEveryWayOfManufacturingARating)$')], full=['ANetCore:./evidence']),
 dict(id='pay2-review-pub', group='PAY-2', repo='ANetCore', desc='评价验签不要求 pub 等于回执的 review_key',
  edits=[('evidence/evidence.go', '\tif !bytes.Equal(w.Pub, reviewKey) {', '\tif false && !bytes.Equal(w.Pub, reviewKey) {')],
  tests=[('ANetCore:./evidence', '^(TestAReviewIsSignedByTheReceiptsReviewKeyOnly|TestREVIEW_2)$')], full=['ANetCore:./evidence']),
 dict(id='pay2-interlock-cid', group='PAY-2', repo='ANetCore', desc='CheckV2 不核对 review.receipt_cid',
  edits=[('evidence/interlock.go', '\tif rv.ReceiptCID != receiptCID {', '\tif false && rv.ReceiptCID != receiptCID {')],
  tests=[('ANetCore:./evidence', '^TestEveryWayOfManufacturingARating$')], full=['ANetCore:./evidence']),
 dict(id='pay2-interlock-subject', group='PAY-2', repo='ANetCore', desc='CheckV2 不核对 review.subject 等于 provider',
  edits=[('evidence/interlock.go', '\tif rv.Subject != rc.Provider {', '\tif false && rv.Subject != rc.Provider {')],
  tests=[('ANetCore:./evidence', '^TestEveryWayOfManufacturingARating$')], full=['ANetCore:./evidence']),
 dict(id='pay2-interlock-day', group='PAY-2', repo='ANetCore', desc='CheckV2 不按回执自身的日期验签(SI-36)',
  edits=[('evidence/interlock.go', '\tif _, err := receipt.Verify(providerKEL, at); err != nil {', '\tif _, err := receipt.Verify(providerKEL, at-at+1); err != nil {')],
  tests=[('ANetCore:./evidence', '^TestCheckV2JudgesTheReceiptAtItsOwnDay$')], full=['ANetCore:./evidence']),
 # ---- PAY-2: the requester's check, the task binding, the card answer
 dict(id='pay2-result-ix', group='PAY-2', repo='ANetCore', desc='VerifyResultForRequest 不核对 ix_commit',
  edits=[('delegation/delegation.go', '\tcase !equal32(rc.IXCommit, ixc):', '\tcase false && !equal32(rc.IXCommit, ixc):')],
  tests=RESULT_T, full=['ANetCore:./delegation']),
 dict(id='pay2-result-reviewkey', group='PAY-2', repo='ANetCore', desc='VerifyResultForRequest 不核对 review_key',
  edits=[('delegation/delegation.go', '\tcase !equal32(rc.ReviewKey, binding.ReviewKey):', '\tcase false && !equal32(rc.ReviewKey, binding.ReviewKey):')],
  tests=RESULT_T, full=['ANetCore:./delegation']),
 dict(id='pay2-result-day', group='PAY-2', repo='ANetCore', desc='VerifyResultForRequest 不核对 completed_day 窗口',
  edits=[('delegation/delegation.go', '\tif rc.CompletedDay+1 < want.DelegatedDay || rc.CompletedDay > want.Today+1 {',
          '\tif false && (rc.CompletedDay+1 < want.DelegatedDay || rc.CompletedDay > want.Today+1) {')],
  tests=RESULT_T, full=['ANetCore:./delegation']),
 dict(id='pay2-binding-private', group='PAY-2', repo='ANetCore', desc='CheckTaskBinding 接受非 private 的绑定上下文',
  edits=[('delegation/binding.go', '\t\tif c.Visibility != bindingVisibility {', '\t\tif false && c.Visibility != bindingVisibility {')],
  tests=BINDING_T, full=['ANetCore:./delegation', 'ANetCore:./golden']),
 dict(id='pay2-binding-dup', group='PAY-2', repo='ANetCore', desc='CheckTaskBinding 接受重复的绑定上下文',
  edits=[('delegation/binding.go', '\t\tif _, dup := found[c.Key]; dup {', '\t\tif _, dup := found[c.Key]; false && dup {')],
  tests=BINDING_T, full=['ANetCore:./delegation']),
 dict(id='pay2-card-both', group='PAY-2', repo='ANetCore', desc='CardResp 接受 card 与 error 同在',
  edits=[('delegation/card.go', '\tcase len(r.Card) > 0 && r.Error != "":', '\tcase false && len(r.Card) > 0 && r.Error != "":')],
  tests=[('ANetCore:./delegation', '^TestACardAnswerIsExactlyOneThing$'), ('ANetCore:./golden', '^TestCARD_1$')], full=['ANetCore:./delegation']),
 # ---- PAY-2: supply epochs, MonoClock, the carried decoders
 dict(id='pay2-supply-members', group='PAY-2', repo='ANetCore',
  desc='纪元记录载荷不核对成员集合(缺成员被接受;成员集合与重编码两道检查互为备份,两道一起去掉)',
  edits=[('ael/supply.go', '\tif strings.Join(names, ",") != strings.Join(supplyMembers, ",") {', '\tif false && strings.Join(names, ",") != strings.Join(supplyMembers, ",") {'),
         ('ael/supply.go', '\tif back, err := coredet.Marshal(p); err != nil || !bytes.Equal(back, raw) {', '\tif back, err := coredet.Marshal(p); err != nil || false && !bytes.Equal(back, raw) {')],
  tests=[('ANetCore:./ael', '^TestParseSupplyEpochIsStrict$')], full=['ANetCore:./ael', 'ANetCore:./golden']),
 dict(id='pay2-monoclock-clamp', group='PAY-2', repo='ANetCore', desc='MonoClock.Raise 不限幅 floor(PAY-R11,对应 M-PAY-30 的内核部分)',
  edits=[('relayauth/monoclock.go', '\tif floor > limit {\n\t\treturn false\n\t}\n', '')],
  tests=[('ANetCore:./relayauth', '^TestMonoClockClampsTheFloor$')], full=['ANetCore:./relayauth']),
 dict(id='pay2-monoclock-global', group='PAY-2', repo='ANetCore', desc='MonoClock 全部目标共用一个计数器(PAY-R12)',
  edits=[('relayauth/monoclock.go', 'c.last[target]', 'c.last[""]', 4)],
  tests=[('ANetCore:./relayauth', '^TestMonoClockKeepsHubsApart$')], full=['ANetCore:./relayauth']),
 dict(id='pay2-strict-payment', group='PAY-2', repo='ANetCore', desc='payment 授权与收据解码恢复宽松(丢弃未登记键)',
  edits=[('payment/payment.go', 'coredet.UnmarshalStrict(b, &w)', 'coredet.Unmarshal(b, &w)', 2)],
  tests=CARRIED_T, full=['ANetCore:./wire']),
 dict(id='pay2-strict-ael', group='PAY-2', repo='ANetCore', desc='见证头证明解码恢复宽松',
  edits=[('ael/witness.go', 'coredet.UnmarshalStrict(b, &w)', 'coredet.Unmarshal(b, &w)')],
  tests=CARRIED_T, full=['ANetCore:./wire']),
]
