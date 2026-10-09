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
 dict(id='pay2-subject-nul', group='PAY-2', repo='ANetCore',
  desc='评价 subject 含 U+0000 时按格式错误(receipt_not_reviewable)而非 ErrNUL(bad_request,CORE-E9)',
  edits=[('evidence/evidence.go', '\tif strings.IndexByte(b.Subject, 0) >= 0 {\n\t\treturn fmt.Errorf("%w: review subject", ErrNUL)\n\t}\n', '')],
  tests=[('ANetCore:./evidence', '^TestREVIEW_2$')], full=['ANetCore:./evidence']),
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

# M-PAY-21 to M-PAY-26, M-PAY-33, M-PAY-34, M-PAY-44 and M-PAY-46 are the hub's admission gate, probation,
# trial credit and the one deregistration function (PAY-5, repo ANetHub). P-PAY-11 is
# TestAdmissionGateMatrix: every cell of {registered, listed, window open, window full, open, closed} x
# {proof of work good, bad, missing} x {quota free, full} x {check_only, not} answers as PAY-A1 says, and
# a refusal writes nothing, keeps no place and replays no KEL.

HUB = 'ANetHub:./internal/aghub'
GATE = [(HUB, '^TestAdmissionGateMatrix$')]

MUTS += [
 dict(id='M-PAY-21', group='PAY', repo='ANetHub', desc='恢复注册赠送(首次注册成功后发 100 credits)',
  edits=[('internal/aghub/server.go', '\tif admitted {\n\t\tcommitted = true\n\t\ts.store.gate().committed(hold)\n\t}\n',
          '\tif admitted {\n\t\tcommitted = true\n\t\ts.store.gate().committed(hold)\n\t\t_ = s.store.Credit(req.AID, 100, "registration grant")\n\t}\n')],
  tests=[(HUB, '^(TestRegistrationPaysNothing|TestAdmissionGateMatrix)$')], full=[HUB]),
 dict(id='M-PAY-22', group='PAY', repo='ANetHub', desc='PoW 与准入闸挪到 KEL 回放与验签之后',
  edits=[('internal/aghub/server.go',
          '\tcheckStatus := CheckAdmitted\n\tif !s.store.KnowsAgent(req.AID) {\n\t\tg := s.store.gate()\n\t\tvar refused bool\n\t\tadm, hold, checkStatus, refused = s.admissionGate(w, r, &req, powSeed, powNonce, g, now)\n\t\tif refused {\n\t\t\treturn\n\t\t}\n\t}\n',
          '\tcheckStatus := CheckAdmitted\n'),
         ('internal/aghub/server.go',
          '\tif f := s.verifyV2(r, relayauth.ActionRegister, body, auth, kel); f != nil {\n\t\twriteAuthFailure(w, f)\n\t\treturn\n\t}\n',
          '\tif f := s.verifyV2(r, relayauth.ActionRegister, body, auth, kel); f != nil {\n\t\twriteAuthFailure(w, f)\n\t\treturn\n\t}\n\tif !s.store.KnowsAgent(req.AID) {\n\t\tg := s.store.gate()\n\t\tvar refused bool\n\t\tadm, hold, checkStatus, refused = s.admissionGate(w, r, &req, powSeed, powNonce, g, now)\n\t\tif refused {\n\t\t\treturn\n\t\t}\n\t}\n')],
  tests=GATE, full=[HUB]),
 dict(id='M-PAY-23', group='PAY', repo='ANetHub', desc='报名窗口不计名额(满了也放行,提交时也不检查)',
  edits=[('internal/aghub/server.go', '\t\tif !open || win.Used >= win.Max {\n', '\t\tif !open {\n'),
         ('internal/aghub/admission.go', '\tif w.Used+g.window[w.ID] >= w.Max {\n', '\tif false {\n'),
         ('internal/aghub/admission.go', 'SET used = used + 1 WHERE id=? AND used < max`', 'SET used = used + 1 WHERE id=?`')],
  tests=GATE, full=[HUB]),
 dict(id='M-PAY-24', group='PAY', repo='ANetHub', desc='见习期不限制评价上传',
  edits=[('internal/aghub/server.go', '\tif _, on := s.store.ProbationUntil(rv.ReviewerAID); on {\n', '\tif false {\n')],
  tests=[(HUB, '^TestReviewsWaitForProbation$')], full=[HUB]),
 dict(id='M-PAY-25', group='PAY', repo='ANetHub', desc='从未活跃清理不写 departed_kel(墓碑)',
  edits=[('internal/aghub/deregister.go', '\tif err := writeTombstone(tx, aid, kel, now); err != nil {\n',
          '\tif why == DeregInactive {\n\t} else if err := writeTombstone(tx, aid, kel, now); err != nil {\n')],
  tests=[(HUB, '^TestTheInactiveSweep$')], full=[HUB]),
 dict(id='M-PAY-26', group='PAY', repo='ANetHub', desc='体验额度不查当日预算',
  edits=[('internal/aghub/admission.go', '\thold, ok := s.gate().reserveTrial(ip, now)\n', '\thold, ok := (*gateHold)(nil), true\n')],
  tests=[(HUB, '^TestTheTrialBudget$')], full=[HUB]),
 dict(id='M-PAY-33', group='PAY', repo='ANetHub', desc='check_only 也要求 PoW 并预占名额',
  edits=[('internal/aghub/server.go', '\tif req.CheckOnly {\n\t\treturn nil, nil, CheckOpen, false\n\t}\n', '')],
  tests=GATE, full=[HUB]),
 dict(id='M-PAY-33w', group='PAY', repo='ANetHub', desc='check_only 写 agent 行',
  edits=[('internal/aghub/server.go', '\tif req.CheckOnly {\n\t\twriteJSON(w, http.StatusOK, CheckOnlyResponse{Status: checkStatus})\n',
          '\tif req.CheckOnly {\n\t\t_ = s.store.PutAgent(req.AID, req.Name, req.Caps, kelBytes)\n\t\twriteJSON(w, http.StatusOK, CheckOnlyResponse{Status: checkStatus})\n')],
  tests=GATE, full=[HUB]),
 dict(id='M-PAY-34', group='PAY', repo='ANetHub', desc='invite 成员按一般未知成员处理(回 bad_request)',
  edits=[('internal/aghub/server.go', '\t\tif carriesInvite(body) {\n', '\t\tif false {\n')],
  tests=[(HUB, '^TestWire3Fixtures$')], full=[HUB]),
 dict(id='M-PAY-44', group='PAY', repo='ANetHub', desc='墓碑写入不检查延伸(缩短或分叉的 KEL 可覆盖墓碑)',
  edits=[('internal/aghub/deregister.go', '\t\tif xerr := identity.ExtendsKEL(old, next); xerr != nil {\n',
          '\t\tif xerr := identity.ExtendsKEL(old, next); false && xerr != nil {\n')],
  tests=[(HUB, '^TestDeregTombstoneNeverShrinks$')], full=[HUB]),
 dict(id='M-PAY-46', group='PAY', repo='ANetHub', desc='进程外注销不写 dereg_event',
  edits=[('internal/aghub/deregister.go',
          '\tif _, err := tx.Exec(`INSERT INTO dereg_event(aid, at) VALUES(?,?)\n\t    ON CONFLICT(aid) DO UPDATE SET at=excluded.at`, aid, now.UnixMilli()); err != nil {\n\t\treturn DeregResult{}, err\n\t}\n', '')],
  tests=[(HUB, '^TestOutOfProcessDeregDropsSessions$')], full=[HUB]),
]

# M-PAY-16 and M-PAY-20 are the hub side of removing the public prices, the gateway and the ADP card
# (PAY-8, repo ANetHub). A public A2A card carrying anet-pricing/v1 or anet-price/v1 is refused at
# registration (card_error price_in_public_card) and from a peer (for good); the gateway path is a
# 410 gateway_removed tombstone that reads nothing of the request; /profile and /register decode
# strictly, so pricing and card are 400 bad_request.

PRICE_T = [(HUB, '^(TestAPublicCardWithPricesIsRefusedAtRegistration|TestAFederatedPublicCardWithPricesIsRefusedForGood|TestNoPriceInTheRegistryOrTheDatabase)$')]
TOMB_T = [(HUB, '^(TestTheGatewayIsATombstone|TestWire3Fixtures)$')]

MUTS += [
 dict(id='M-PAY-16', group='PAY', repo='ANetHub', desc='hub 接受带价格的公开卡(不查 anet-pricing/v1、anet-price/v1)',
  edits=[('internal/aghub/a2acard.go', '\tif carriesPrices(raw) {\n\t\treturn nil, ErrPriceInPublicCard\n', '\tif false && carriesPrices(raw) {\n\t\treturn nil, ErrPriceInPublicCard\n')],
  tests=PRICE_T, full=[HUB]),
 dict(id='pay8-price-uri', group='PAY-8', repo='ANetHub', desc='价格拒收只认 v0.2 的 anet-pricing/v1,漏掉扩展卡的 anet-price/v1',
  edits=[('internal/aghub/a2acard.go', '\t\tif uri == extPricingV1URI || uri == extPriceV1URI {\n', '\t\tif uri == extPricingV1URI {\n')],
  tests=PRICE_T, full=[HUB]),
 dict(id='M-PAY-20', group='PAY', repo='ANetHub', desc='恢复网关路由(旧路径再次通到付款处理)',
  edits=[('internal/aghub/server.go', '\tmux.HandleFunc("GET "+gatewayPath, hGatewayRemoved)\n',
          '\tmux.HandleFunc("GET /x402/resource/{aid}/{capability}", s.hX402Settle)\n')],
  tests=TOMB_T, full=[HUB]),
 dict(id='pay8-tombstone-gone', group='PAY-8', repo='ANetHub', desc='网关路径不留墓碑(落到 404 not_found,不说网关已删与去处)',
  edits=[('internal/aghub/server.go', '\tmux.HandleFunc("GET "+gatewayPath, hGatewayRemoved)\n', '')],
  tests=TOMB_T, full=[HUB]),
 dict(id='pay8-profile-lenient', group='PAY-8', repo='ANetHub', desc='/profile 恢复宽松解码(pricing 成员被静默接受)',
  edits=[('internal/aghub/server.go', '\tif err := wire3.DecodeStrict(a.Body, &req); err != nil {\n\t\twire3.RefuseDecode(w, err)\n',
          '\tif err := json.Unmarshal(a.Body, &req); err != nil {\n\t\twire3.RefuseDecode(w, err)\n')],
  tests=[(HUB, '^TestNoPriceInTheRegistryOrTheDatabase$')], full=[HUB]),
]

# M-PAY-27, M-PAY-28 and M-PAY-36 are RC1, the hub's replay protection (PAY-7, repo ANetHub,
# internal/aghub/auth2.go). P-PAY-4 is internal/aghub/rc1_prop_test.go: a time is accepted at most once
# per signer and never at or below its floor, sweeps and hub clock steps notwithstanding; a signer's
# verdicts are the same alone and among any number of others; a MonoClock client is never stuck.
# J-PAY-11's hub part is TestRC1TenThousandSignersPastTheValveRefuseNobody. TestWire3Fixtures runs
# whole: a run filtered to one fixture file fails the error-body coverage check on any code (so M-PAY-34
# above, which named register.json alone, was red without its mutation). The pay7-* rows are the rest
# of RC1 (the class K, the floor left by a sweep and by a restart, the valve, the ts_stale header) and
# the federation client's signer (PAY-R9: MonoClock per peer, one signing again after ts_stale, a floor
# from the future not adopted).

RC1_PROP = [(HUB, '^TestRC1Prop')]
FED = 'ANetHub:./internal/federation'

MUTS += [
 dict(id='M-PAY-27', group='PAY', repo='ANetHub', desc='RC1 恢复均分池(每段的 K 按签名方个数均分)',
  edits=[('internal/aghub/auth2.go', '\tfor len(s.recent) > max(k, 1) {\n', '\tfor len(s.recent) > max(k*4/len(sh.signers), 1) {\n')],
  tests=[(HUB, '^TestRC1PropASignerIsJudgedOnItsOwnTimesOnly$')], full=[HUB]),
 dict(id='M-PAY-28', group='PAY', repo='ANetHub', desc='RC1(floor)在验签之前判定',
  edits=[('internal/aghub/auth2.go',
          '\tif f := signatureFailure(action, a, pre, kel); f != nil {\n\t\treturn f\n\t}\n\treturn s.rc1Check(a, action, now)\n',
          '\tif f := s.rc1Check(a, action, now); f != nil {\n\t\treturn f\n\t}\n\treturn signatureFailure(action, a, pre, kel)\n')],
  tests=[(HUB, '^(TestRC1RefusalsAndTheFloorComeOnlyAfterTheSignature|TestWire3Fixtures)$')], full=[HUB]),
 dict(id='M-PAY-36', group='PAY', repo='ANetHub', desc='RC1 按最近活跃时刻回收签名方状态',
  edits=[('internal/aghub/auth2.go', '\t// recent holds the accepted times above floor, ascending.\n\trecent []uint64\n',
          '\t// recent holds the accepted times above floor, ascending.\n\trecent []uint64\n\tlastActive uint64\n'),
         ('internal/aghub/auth2.go', '\ts.maxSeen = max(s.maxSeen, ts)\n', '\ts.maxSeen = max(s.maxSeen, ts)\n\ts.lastActive = uint64(now)\n'),
         ('internal/aghub/auth2.go', '\t\tif top := max(s.floor, s.maxSeen); top < uint64(limit) {',
          '\t\tif top := max(s.floor, s.lastActive); top < uint64(limit) {')],
  tests=[(HUB, '^(TestRC1PropAtMostOnceAndNeverAtOrBelowTheFloor|TestRC1DropsASignerOnlyWhenTheWindowCoversItsLargestTime)$')],
  full=[HUB]),
 dict(id='pay7-k-probation', group='PAY-7', repo='ANetHub', desc='RC1 的 K 不看见习期(见习账户也是 64)',
  edits=[('internal/aghub/auth2.go', '\tif _, on := s.store.ProbationUntil(aid); on {\n\t\treturn rc1KNew\n\t}\n', '')],
  tests=[(HUB, '^TestRC1KFollowsTheSignersClass$')], full=[HUB]),
 dict(id='pay7-k-newcomer', group='PAY-7', repo='ANetHub', desc='RC1 的 K 把未注册的注册者当 agent(64)',
  edits=[('internal/aghub/auth2.go', '\tif action == relayauth.ActionRegister && !s.store.KnowsAgent(aid) {',
          '\tif false && action == relayauth.ActionRegister && !s.store.KnowsAgent(aid) {')],
  tests=[(HUB, '^TestRC1KFollowsTheSignersClass$')], full=[HUB]),
 dict(id='pay7-gone', group='PAY-7', repo='ANetHub', desc='回收签名方不留下 floor(hub 时钟回拨后旧时间重新可用)',
  edits=[('internal/aghub/auth2.go', '\t\t\tsh.gone = max(sh.gone, top)\n', '')],
  tests=[(HUB, '^(TestRC1DroppedTimesStayRefusedWhenTheClockStepsBack|TestRC1PropAtMostOnceAndNeverAtOrBelowTheFloor)$')],
  full=[HUB]),
 dict(id='pay7-start', group='PAY-7', repo='ANetHub', desc='RC1 状态从 0 起(重启后重新打开窗口)',
  edits=[('internal/aghub/auth2.go', '\t\tr.shards[i].gone = uint64(max(now-1, 0))\n', '\t\tr.shards[i].gone = 0\n')],
  tests=[(HUB, '^(TestRC1ARestartRefusesTimesSignedBeforeIt|TestWire3Fixtures)$')], full=[HUB]),
 dict(id='pay7-valve-floor', group='PAY-7', repo='ANetHub', desc='内存阀清空 recent 而不把 floor 抬到 maxSeen(被清空的时间可再次接受)',
  edits=[('internal/aghub/auth2.go', '\t\t\ts.floor = max(s.floor, s.maxSeen)\n\t\t\ts.recent = nil\n', '\t\t\ts.recent = nil\n')],
  tests=[(HUB, '^TestRC1PropAtMostOnceAndNeverAtOrBelowTheFloor$')], full=[HUB]),
 dict(id='pay7-valve', group='PAY-7', repo='ANetHub', desc='内存阀不运行',
  edits=[('internal/aghub/auth2.go', '\tr.sweepNext(now)\n\tr.relieve(now)\n', '\tr.sweepNext(now)\n')],
  tests=[(HUB, '^(TestRC1TheValveMakesTheOldestSignersMonotonic|TestRC1TenThousandSignersPastTheValveRefuseNobody)$')],
  full=[HUB]),
 dict(id='pay7-floor-header', group='PAY-7', repo='ANetHub', desc='ts_stale 不带 X-ANet-TS-Floor',
  edits=[('internal/aghub/auth2.go', '\t\t\tw.Header().Set(headerTSFloor, strconv.FormatUint(f.floor, 10))\n', '')],
  tests=[(HUB, '^(TestRC1RefusalsAndTheFloorComeOnlyAfterTheSignature|TestWire3Fixtures)$')], full=[HUB]),
 dict(id='pay7-fed-clock', group='PAY-7', repo='ANetHub', desc='联邦客户端的签名时间取当前时刻,不用 MonoClock',
  edits=[('internal/federation/signer.go', '\tts := uint64(s.clock.Next(peerAID, s.now().UnixMilli()))\n',
          '\tts := uint64(s.now().UnixMilli())\n')],
  tests=[(FED, '^TestTheSignerHandsEachPeerIncreasingTimes$')], full=[FED]),
 dict(id='pay7-fed-raise', group='PAY-7', repo='ANetHub', desc='联邦客户端收到 ts_stale 不抬高重签',
  edits=[('internal/federation/signer.go', '\tfloor, stale := staleFloor(resp)\n\tif !stale {\n',
          '\tfloor, stale := staleFloor(resp)\n\tif _ = floor; true || !stale {\n')],
  tests=[(FED, '^TestTheSignerSignsAgainAboveAStaleFloorOnce$')], full=[FED]),
 dict(id='pay7-fed-clamp', group='PAY-7', repo='ANetHub', desc='联邦客户端不理会 Raise 的拒绝(采纳来自未来的 floor 并重发)',
  edits=[('internal/federation/signer.go', '\tif !s.clock.Raise(peerAID, floor, s.now().UnixMilli()) {\n',
          '\tif s.clock.Raise(peerAID, floor, s.now().UnixMilli()); false {\n')],
  tests=[(FED, '^TestTheSignerRefusesAFloorFromTheFutureAndKeepsPeersApart$')], full=[FED]),
 dict(id='pay7-fed-once', group='PAY-7', repo='ANetHub', desc='联邦客户端对一次请求反复抬高重签(不止一次)',
  edits=[('internal/federation/signer.go', '\treturn s.send(ctx, peerAID, action, r)\n}\n\n// send',
          '\treturn s.Do(ctx, peerAID, action, r)\n}\n\n// send')],
  tests=[(FED, '^TestTheSignerSignsAgainAboveAStaleFloorOnce$')], full=[FED]),
]

# ---- PAY-3: supply epochs and balance proofs; hub.db is 0.3.0 data or refused (repo ANetHub)
#
# M-PAY-3 restores what the per-event issuance chain did: a public record per supply event carrying its
# amount. The code that wrote one is gone, so the mutant has supplyEventTx write a row per event into the
# served epoch chain; the canary TestSI45PublicSurfacesNameNoPayment (cmd/anet-hub, J-PAY-7 in unit form:
# a cross-hub payment and a redemption of 7919 against a characteristic reference) finds the amount.
# M-PAY-4 and M-PAY-5 are killed by P-PAY-2 (TestPropEpochsFollowTheRuleAndThePublicSupplyHoldsStill), M-PAY-6
# by supply_close_test.go. PAY-3's own (pay3-*) cover the supply events, the hub's row, the proofs, the
# 14 days, hub.db's version, the pool, the operator sentences and the federation witness poll.

CMD = 'ANetHub:./cmd/anet-hub'
SUPPLY_PROP = [(HUB, '^(TestPropEpochsFollowTheRuleAndThePublicSupplyHoldsStill|TestThePublicSupplyIsTheSameBytesBetweenPublications)$')]
SUPPLY_EVENTS = [(HUB, '^TestEverySupplyEventCountsAndATransferDoesNot$')]
PROOFS = [(HUB, '^(TestALiabilityProofReachesTheSignedRoot|TestTheLiabilityProofRefusals|TestWire3Fixtures)$')]

MUTS += [
 dict(id='M-PAY-3', group='PAY', repo='ANetHub', desc='恢复逐事件写链(每个供应事件写一条带金额的公开链记录)',
  edits=[('internal/aghub/supply.go',
          '\t\t\tcol, amt, int64(math.MaxInt64))\n\t}\n\treturn nil\n}\n',
          '\t\t\tcol, amt, int64(math.MaxInt64))\n\t}\n'
          '\tif _, err := tx.Exec(`INSERT INTO supply_chain(seq, id, prev_id, from_day, to_day, root, outstanding, depth, record)\n'
          '\t    SELECT COALESCE(MAX(seq), -1) + 1, hex(randomblob(16)), \'\', 0, -2 - COALESCE(MAX(seq), -1), \'\', ?, 4, x\'00\'\n'
          '\t    FROM supply_chain`, amt); err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n')],
  tests=[(CMD, '^TestSI45PublicSurfacesNameNoPayment$')], full=[CMD]),
 dict(id='M-PAY-4', group='PAY', repo='ANetHub', desc='/x402/supply 改回实时计算(outstanding 取当前账面)',
  edits=[('internal/aghub/supply.go', '\t\tout.Record = e.Record\n\t}\n\twriteJSON(w, http.StatusOK, out)\n',
          '\t\tout.Record = e.Record\n\t\tif sup, err := s.store.Supply(s.hubAID); err == nil {\n'
          '\t\t\tout.Epoch.Outstanding = uint64(sup.Outstanding)\n\t\t}\n\t}\n\twriteJSON(w, http.StatusOK, out)\n')],
  tests=SUPPLY_PROP, full=[HUB]),
 dict(id='M-PAY-5', group='PAY', repo='ANetHub', desc='去掉纪元发布规则(每个 UTC 日都发布)',
  edits=[('internal/aghub/supply.go', '\tif events < epochEvents && d-last.Int64 < epochDays {\n\t\treturn nil, nil\n\t}\n', '')],
  tests=SUPPLY_PROP + [(HUB, '^TestTheEpochRule$')], full=[HUB]),
 dict(id='M-PAY-6', group='PAY', repo='ANetHub', desc='关账时不校验根的和 == outstanding',
  edits=[('internal/aghub/supply.go', '\tif outstanding < 0 || tree.Outstanding() != uint64(outstanding) || hubRow != -outstanding {\n',
          '\tif outstanding < 0 || hubRow != -outstanding {\n')],
  tests=[(HUB, '^TestTheCloseRefusesALedgerThatDoesNotAddUp$')], full=[HUB]),
 dict(id='pay3-event-out', group='PAY-3', repo='ANetHub', desc='向对端 agent 的付款不记供应事件(redeemed 不增)',
  edits=[('internal/aghub/facilitator.go', '\tif auth.PayTo == hubAID || !local {\n', '\tif auth.PayTo == hubAID {\n')],
  tests=SUPPLY_EVENTS, full=[HUB]),
 dict(id='pay3-event-redeem', group='PAY-3', repo='ANetHub', desc='兑付不记供应事件',
  edits=[('internal/aghub/facilitator.go', '\tif auth.PayTo == hubAID || !local {\n', '\tif !local {\n')],
  tests=SUPPLY_EVENTS, full=[HUB]),
 dict(id='pay3-event-clear', group='PAY-3', repo='ANetHub', desc='对端结算入账不记供应事件',
  edits=[('internal/aghub/facilitator.go',
          '\tif err := s.supplyEventTx(tx, supplyIssued, amt); err != nil {\n\t\treturn err\n\t}\n\treturn tx.Commit()\n',
          '\treturn tx.Commit()\n')],
  tests=SUPPLY_EVENTS, full=[HUB]),
 dict(id='pay3-hubrow', group='PAY-3', repo='ANetHub', desc='发放不动 hub 自己的行(−hub 行 ≠ issued − redeemed)',
  edits=[('internal/aghub/facilitator.go',
          '\t\tif err := addToRow(tx, "credit_balance", "aid", "credits", s.hubAID, -amount); err != nil {\n\t\t\treturn err\n\t\t}\n',
          '')],
  tests=SUPPLY_EVENTS, full=[HUB]),
 dict(id='pay3-retention', group='PAY-3', repo='ANetHub', desc='发布满 14 天的纪元仍给出负债证明',
  edits=[('internal/aghub/supply.go', '\tif now.UnixMilli()-at >= proofKeptFor {\n', '\tif now.UnixMilli()-at >= proofKeptFor && false {\n')],
  tests=PROOFS, full=[HUB]),
 dict(id='pay3-prune', group='PAY-3', repo='ANetHub', desc='发布满 14 天的纪元不删叶',
  edits=[('internal/aghub/supply.go', '\t\tif now-pub >= proofKeptFor {\n', '\t\tif now-pub >= proofKeptFor && false {\n')],
  tests=PROOFS, full=[HUB]),
 dict(id='pay3-proof-today', group='PAY-3', repo='ANetHub', desc='负债证明忽略 ?to_day,总给最新纪元',
  edits=[('internal/aghub/supply.go', '\tp, err := s.store.LiabilityProof(a.AID, toDay)\n', '\t_ = toDay\n\tp, err := s.store.LiabilityProof(a.AID, nil)\n')],
  tests=PROOFS, full=[HUB]),
 dict(id='pay3-proof-self', group='PAY-3', repo='ANetHub', desc='负债证明不要求签名方是路径中的 AID(给出签名方自己的)',
  edits=[('internal/aghub/supply.go', '\ta, ok := s.authSelf(w, r, relayauth.ActionLiabProof, signedBodyLimit)\n',
          '\ta, ok := s.authRegistered(w, r, relayauth.ActionLiabProof, signedBodyLimit)\n')],
  tests=PROOFS, full=[HUB]),
 dict(id='pay3-idle', group='PAY-3', repo='ANetHub', desc='不设 SetMaxIdleConns(缺省 2)',
  edits=[('internal/aghub/aghub.go', '\tdb.SetMaxIdleConns(dbIdleConns)\n', '')],
  tests=[(HUB, '^TestThePoolKeepsItsConnections$')], full=[HUB]),
 dict(id='pay3-v-new', group='PAY-3', repo='ANetHub', desc='较新版本写的 hub.db 当作 0.2 数据(下一步变成移走目录)',
  edits=[('internal/aghub/aghub.go', '\tcase version > SchemaVersion:\n', '\tcase false:\n')],
  tests=[(HUB, '^TestHubDataOfAnotherReleaseIsRefusedUntouched$'), (CMD, '^TestHubRefusesNewerDB$')], full=[HUB, CMD]),
 dict(id='pay3-v-two', group='PAY-3', repo='ANetHub', desc='user_version 低于 3 而没有表的库当作新库',
  edits=[('internal/aghub/aghub.go', '\tcase version == SchemaVersion, version == 0 && objects == 0:\n',
          '\tcase version == SchemaVersion, objects == 0 && version < SchemaVersion:\n')],
  tests=[(HUB, '^TestHubDataOfAnotherReleaseIsRefusedUntouched$')], full=[HUB]),
 dict(id='pay3-checkdata', group='PAY-3', repo='ANetHub', desc='拒绝之前先以读写方式打开(被拒的目录被改动)',
  edits=[('internal/aghub/aghub.go', '\tif err := CheckData(dir); err != nil {\n\t\treturn nil, err\n\t}\n', '')],
  tests=[(HUB, '^TestHubDataOfAnotherReleaseIsRefusedUntouched$'), (CMD, '^(TestHubRefusesOldDB|TestHubRefusesNewerDB)$')],
  full=[HUB, CMD]),
 dict(id='pay3-sentence-new', group='PAY-3', repo='ANetHub', desc='较新版本的数据在 anet-hub 报 hub_identity_v02',
  edits=[('cmd/anet-hub/main.go', '\t\treturn c.say("hub_data_too_new", nil)\n', '\t\treturn c.say("hub_identity_v02", nil)\n')],
  tests=[(CMD, '^TestHubRefusesNewerDB$')], full=[CMD]),
 dict(id='pay3-quota-bad', group='PAY-3', repo='ANetHub', desc='quota 值大于 2^31−1 不报 quota_bad_value',
  edits=[('cmd/anet-hub/quota.go', '\tif err != nil || n > math.MaxInt32 {\n', '\tif err != nil || n > math.MaxInt64 {\n')],
  tests=[(CMD, '^TestQuotaBadValueSentence$')], full=[CMD]),
 dict(id='pay3-witness-poll', group='PAY-3', repo='ANetHub', desc='联邦见证轮询仍读旧的 head_id(对端的纪元链头从不被见证)',
  edits=[('internal/federation/federation.go', '\t\t\tID  string `json:"id"`\n', '\t\t\tID  string `json:"head_id"`\n')],
  tests=[(FED, '^TestAOneRecordChainIsStillWitnessed$')], full=[FED]),
]

# PAY-3 review: the epoch schedule (RunSupplyEpochs, the only caller that publishes in a running hub), the
# proof's rebuild check and the witness endpoint's unsigned attestation.
SCHEDULE = [(HUB, '^TestTheSchedulePublishesAtStartAfterMidnightAndAfterAFailure$')]

MUTS += [
 dict(id='pay3-run-start', group='PAY-3', repo='ANetHub', desc='启动时不补查(停机跨零点的 hub 要等到下一个零点)',
  edits=[('internal/aghub/supply.go', '\tagain := check()\n\tprune()\n', '\tagain := false\n\tprune()\n')],
  tests=SCHEDULE, full=[HUB]),
 dict(id='pay3-run-midday', group='PAY-3', repo='ANetHub', desc='每个 tick 都检查(事件使纪元到期后当天就发布)',
  edits=[('internal/aghub/supply.go', '\t\t\tif d := utcDay(s.now()); d != day || again {\n',
          '\t\t\tif d := utcDay(s.now()); true || d != day || again {\n')],
  tests=SCHEDULE, full=[HUB]),
 dict(id='pay3-run-retry', group='PAY-3', repo='ANetHub', desc='检查失败后不在下一个 tick 重试(纪元晚一天)',
  edits=[('internal/aghub/supply.go', '\t\t\tif d := utcDay(s.now()); d != day || again {\n',
          '\t\t\tif d := utcDay(s.now()); d != day || false && again {\n')],
  tests=SCHEDULE, full=[HUB]),
 dict(id='pay3-run-prune', group='PAY-3', repo='ANetHub', desc='只在换日的检查时删旧叶(余额快照多留近一天)',
  edits=[('internal/aghub/supply.go', '\t\t\t\tagain = check()\n\t\t\t}\n\t\t\tprune()\n',
          '\t\t\t\tagain = check()\n\t\t\t\tprune()\n\t\t\t}\n')],
  tests=SCHEDULE, full=[HUB]),
 dict(id='pay3-proof-rebuild', group='PAY-3', repo='ANetHub', desc='负债证明不核对重建的根(改过的叶照样给出证明)',
  edits=[('internal/aghub/supply.go',
          '\tif liabtree.Hex(t.Root()) != e.Root || t.Outstanding() != e.Outstanding || t.Depth() != e.depth {\n',
          '\tif t.Outstanding() != e.Outstanding || t.Depth() != e.depth {\n')],
  tests=[(HUB, '^TestALeafChangedAfterTheCloseIsNotServed$')], full=[HUB]),
 dict(id='pay3-witness-unsigned', group='PAY-3', repo='ANetHub', desc='无签名的见证当作存储失败(500 而不是 400)',
  edits=[('internal/aghub/witness.go', '\tif a.Envelope == nil {\n', '\tif false {\n')],
  tests=[(HUB, '^TestAHubRefusesAnUnsignedAttestation$')], full=[HUB]),
]

# PAY-3, second review: a database another release left only in its write-ahead log (the read-write
# open is the one that sees it), the ledger's high-water mark once the 14-day window has emptied the
# ledger, and the bound on the supply totals themselves (the one Credit has, beside issuanceRoom).
MUTS += [
 dict(id='pay3-wal-judge', group='PAY-3', repo='ANetHub', desc='读写打开后不再判定版本(只在 WAL 里的 0.2 库被当作新库并写成版本 3)',
  edits=[('internal/aghub/aghub.go',
          '\tif err := judgeSchema(version, objects); err != nil {\n\t\treturn err\n\t}\n\tfor _, part := range schema {\n',
          '\tif err := judgeSchema(version, objects); err != nil && false {\n\t\treturn err\n\t}\n\tfor _, part := range schema {\n')],
  tests=[(HUB, '^TestADatabaseKnownOnlyThroughItsLogIsRefused$')], full=[HUB]),
 dict(id='pay3-ledger-hw', group='PAY-3', repo='ANetHub', desc='ledger_hw 取现存账目的 MAX(seq)(窗口删空账目后回到 0)',
  edits=[('internal/aghub/supply.go', "SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'credit_entry'), 0)",
          'SELECT COALESCE(MAX(seq), 0) FROM credit_entry')],
  tests=[(HUB, '^TestTheLedgerHighWaterMarkOutlivesTheEntries$')], full=[HUB]),
 dict(id='pay3-total-bound', group='PAY-3', repo='ANetHub', desc='供应累计值不设 2^63−1 的界(超界时 SQLite 存成 REAL)',
  edits=[('internal/aghub/supply.go', '  WHERE id = 1 AND `+col+` <= ?`, amt,', '  WHERE id = 1 AND ? >= 0`, amt,')],
  tests=[(HUB, '^TestASupplyTotalThatCannotTakeTheEventRefusesIt$')], full=[HUB]),
]

# PAY-9: the voucher door, the ADP card, public prices and the gateway tier are gone from the daemon.
# M-PAY-37 is the door coming back, three ways: the Via value that named it, a listener in the payment
# module, and the config key it was opened with no longer refused at start (UX-T6). The pay9-* rows
# hold the rest: no price on the card, the proxy card or the profile; the control plane refuses the
# members it no longer serves; the gateway purpose and the routes stay gone.
DAEMON = 'ANet:./internal/daemon'
X402 = 'ANet:./module/x402'
MUTS += [
 dict(id='M-PAY-37', group='PAY', repo='ANet', desc='恢复 provider.ViaVoucher(凭证兑付门的入口值)',
  edits=[('provider/provider.go', 'const (\n\tViaRelay = "relay"\n)', 'const (\n\tViaRelay   = "relay"\n\tViaVoucher = "voucher"\n)')],
  tests=[(X402, '^TestTheOnlyDoorIsTheRelay$')], full=[X402]),
 dict(id='M-PAY-37v', group='PAY', repo='ANet', desc='以变量形式恢复 provider.ViaVoucher',
  edits=[('provider/provider.go', 'const (\n\tViaRelay = "relay"\n)', 'const (\n\tViaRelay = "relay"\n)\n\nvar ViaVoucher = "voucher"')],
  tests=[(X402, '^TestTheOnlyDoorIsTheRelay$')], full=[X402]),
 dict(id='M-PAY-37b', group='PAY', repo='ANet', desc='恢复节点侧兑付门:x402 模块按 voucher_addr 开监听',
  edits=[('module/x402/x402.go', '\t"log"\n\t"strings"\n', '\t"log"\n\t"net"\n\t"net/http"\n\t"strings"\n'),
         ('module/x402/x402.go', '\tWitnessEverySeconds int `json:"witness_every_seconds,omitempty"`\n}\n',
          '\tWitnessEverySeconds int `json:"witness_every_seconds,omitempty"`\n\tVoucherAddr         string `json:"voucher_addr,omitempty"`\n}\n'),
         ('module/x402/x402.go', '\t\tgo m.witnessLoop(ctx)\n\t}\n\treturn nil\n}',
          '\t\tgo m.witnessLoop(ctx)\n\t}\n\tif a := m.cfg.VoucherAddr; a != "" {\n\t\tif ln, err := net.Listen("tcp", a); err == nil {\n\t\t\tgo func() { _ = http.Serve(ln, http.NotFoundHandler()) }()\n\t\t}\n\t}\n\treturn nil\n}')],
  tests=[(X402, '^(TestTheVoucherDoorDoesNotOpen|TestThePaymentModuleServesNoHTTP)$')], full=[X402]),
 dict(id='M-PAY-37c', group='PAY', repo='ANet', desc='modules.x402.voucher_addr 不再是删除项(带它的配置照常启动)',
  edits=[('internal/daemon/config.go', '\t{"modules.x402.voucher_addr", "voucher"},\n', '')],
  tests=[(DAEMON, '^TestFindRemovedSettings$'), ('ANet:./cmd/anet', '^TestRemovedKeysStopAnetDaemon$')],
  full=[DAEMON]),
 dict(id='pay9-card-price', group='PAY-9', repo='ANet', desc='公开卡的 x402 贡献带上逐 skill 价格',
  # Re-anchored by PAY-10, which rewrote CardExtensions: the public card's contribution
  # carrying the price list again, in the 0.2 form.
  edits=[('module/x402/card.go', '\tout := []map[string]any{x402}\n',
          '\tout := []map[string]any{x402, {"uri": "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1", "params": map[string]any{"prices": prices}}}\n')],
  tests=[(X402, '^(TestCardExtensionsCarryNoPrice|TestTheHubCannotNameThePaidSkillFromWhatItIsSent)$')], full=[X402]),
 dict(id='pay9-proxy-price', group='PAY-9', repo='ANet', desc='代理卡照搬远端公开卡的 anet-pricing',
  edits=[('module/a2a/card.go', '\t\tif x402 {\n\t\t\t// This node is the signing service',
          '\t\tfor _, e := range remote.Capabilities.Extensions {\n\t\t\tif strings.HasSuffix(e.URI, "/anet-pricing/v1") {\n\t\t\t\texts = append(exts, map[string]any{"uri": e.URI, "params": e.Params})\n\t\t\t}\n\t\t}\n\t\tif x402 {\n\t\t\t// This node is the signing service')],
  tests=[('ANet:./module/a2a', '^TestProxyCard$')], full=['ANet:./module/a2a']),
 dict(id='pay9-profile-hub', group='PAY-9', repo='ANet', desc='发布 profile 时仍带 pricing 成员',
  edits=[('internal/daemon/hub_client.go', '\t\t"readme":  readme,\n\t}', '\t\t"readme":  readme,\n\t\t"pricing": "",\n\t}')],
  tests=[(DAEMON, '^TestNoPriceLeavesThisNode$')], full=[DAEMON]),
 dict(id='pay9-register-card', group='PAY-9', repo='ANet', desc='注册请求恢复 ADP card 字段',
  edits=[('internal/hubapi/hubapi.go', '\tKEL  string   `json:"kel"`\n', '\tKEL  string   `json:"kel"`\n\tCard json.RawMessage `json:"card,omitempty"`\n')],
  tests=[('ANet:./internal/hubapi', '^TestTheWireFieldNamesArePinned$')], full=['ANet:./internal/hubapi']),
 dict(id='pay9-delegate-pay', group='PAY-9', repo='ANet', desc='/delegate 的 pay:true 被静默忽略(照常发出委派)',
  edits=[('internal/daemon/control_api.go', '\tif req.Pay {\n\t\twriteRefusal(w, http.StatusBadRequest, removedDelegatePay)',
          '\tif false && req.Pay {\n\t\twriteRefusal(w, http.StatusBadRequest, removedDelegatePay)')],
  tests=[(DAEMON, '^TestTheControlPlaneRefusesTheRemovedPaymentMembers$')], full=[DAEMON]),
 dict(id='pay9-profile-pricing', group='PAY-9', repo='ANet', desc='/profile 的 pricing 成员被静默忽略',
  edits=[('internal/daemon/control_api.go', '\t\tif strings.EqualFold(k, "pricing") {', '\t\tif false && strings.EqualFold(k, "pricing") {')],
  tests=[(DAEMON, '^TestTheControlPlaneRefusesTheRemovedPaymentMembers$')], full=[DAEMON]),
 dict(id='pay9-profile-pricing-case', group='PAY-9', repo='ANet', desc='/profile 只按小写匹配 pricing("Pricing" 被静默忽略)',
  edits=[('internal/daemon/control_api.go', '\t\tif strings.EqualFold(k, "pricing") {', '\t\tif k == "pricing" {')],
  tests=[(DAEMON, '^TestTheControlPlaneRefusesTheRemovedPaymentMembers$')], full=[DAEMON]),
 dict(id='pay9-find-cli', group='PAY-9', repo='ANet', desc='anet find 仍发 v1 /find 的成员名(query),/agents/list 静默忽略后列出全部',
  edits=[('cmd/anet/main.go', '\t\tbody["q"] = q\n', '\t\tbody["query"] = q\n')],
  tests=[('ANet:./cmd/anet', '^TestFindAsksTheAgentList$')], full=['ANet:./cmd/anet']),
 dict(id='pay9-find-route', group='PAY-9', repo='ANet', desc='恢复控制面 /find 路由',
  edits=[('internal/daemon/control_api.go', '\tapi.HandleFunc("POST /delegate", d.hDelegate)\n',
          '\tapi.HandleFunc("POST /delegate", d.hDelegate)\n\tapi.HandleFunc("POST /find", d.hAgentsList)\n')],
  tests=[(DAEMON, '^TestTheControlPlaneRefusesTheRemovedPaymentMembers$')], full=[DAEMON]),
 dict(id='pay9-gateway-purpose', group='PAY-9', repo='ANet', desc='支出策略仍接受 gateway 档',
  edits=[('internal/daemon/spend.go', '\tcase module.PurposeTaskManual, module.PurposeRedeem:', '\tcase module.PurposeTaskManual, module.PurposeRedeem, "gateway":')],
  tests=[(DAEMON, '^TestTheSpendingPolicyTiers$')], full=[DAEMON]),
]

# PAY-10: the extended card, provider side. M-PAY-15, 38, 47, 51 and 52 are HPP §7.3's: the kernel's
# price filter on the public card, the general variant's skills, the public card's extendedAgentCard,
# what "the same as the public card" means, and each variant's own a2a-x402 declaration (52 in the
# kernel, 52b through the skills the modules are asked about). The pay10-* rows hold the rest of
# PAY-C3/C4/C8: the per-sender limit, deny gets the general variant, what a signed card is kept for,
# its seq and jku, the price list's checks, doctor's payments.card and the payment module's
# contribution.
NETCARD = 'ANet:./internal/netcard'
NC_PROPS = '^(TestExtendedCardProperties|TestThePublicCardDropsEveryPriceList)$'
MUTS += [
 dict(id='M-PAY-15', group='PAY', repo='ANet', desc='内核不过滤公开卡的价格扩展(netcard.Build 收下 anet-price/v1 与 anet-pricing/v1 贡献)',
  edits=[('internal/netcard/netcard.go', '\treturn build(in, in.Skills, publicExtensions(in.Extensions))', '\treturn build(in, in.Skills, in.Extensions)')],
  tests=[(NETCARD, NC_PROPS)], full=[NETCARD]),
 dict(id='M-PAY-38', group='PAY', repo='ANet', desc='通用变体带上 allow_only 能力',
  edits=[('internal/netcard/extended.go', '\tcase module.VariantGeneral:\n\t\treturn sortedSkills(in.Skills), nil',
          '\tcase module.VariantGeneral:\n\t\treturn sortedSkills(append(append([]Skill(nil), in.Skills...), in.ListOnly...)), nil')],
  tests=[(NETCARD, '^(TestExtendedCardProperties|TestTheTwoVariants|TestVariantSkillsKeepsTheListsApart)$'),
         (DAEMON, '^TestCardAnswerOfANode$')], full=[NETCARD, DAEMON]),
 dict(id='M-PAY-47', group='PAY', repo='ANet', desc='公开卡按是否声明 a2a-x402 决定 extendedAgentCard',
  edits=[('internal/netcard/netcard.go', '\tvar buf bytes.Buffer\n',
          '\tc.Capabilities.ExtendedAgentCard = false\n\tfor _, e := range c.Capabilities.Extensions {\n\t\tif e["uri"] == module.ExtX402URI {\n\t\t\tc.Capabilities.ExtendedAgentCard = true\n\t\t}\n\t}\n\tvar buf bytes.Buffer\n')],
  tests=[(NETCARD, '^(TestPublicCardAlwaysDeclaresExtended|TestExtendedCardProperties)$')], full=[NETCARD]),
 dict(id='M-PAY-51', group='PAY', repo='ANet', desc='cardAnswer 只看有没有价格(变体只多出 allow_only 能力时回 not_configured)',
  edits=[('internal/netcard/extended.go', '\treturn card, bytes.Equal(card, public), nil', '\treturn card, len(prices) == 0 && bytes.Equal(public, public), nil')],
  tests=[(DAEMON, '^(TestCardAnswerOfANode|TestTheExtendedCardIsSignedAgainAfterARotation)$'),
         (NETCARD, '^(TestExtendedCardProperties|TestVariantsOfFreeAndListPricedNodes)$')], full=[DAEMON, NETCARD]),
 dict(id='M-PAY-52', group='PAY', repo='ANet', desc='扩展卡变体沿用公开卡的 a2a-x402 声明,不按变体计算(内核换上公开卡的声明)',
  edits=[('internal/netcard/extended.go', '\t\texts = append(exts, e)\n\t}\n',
          '\t\tif u, _ := e["uri"].(string); u != module.ExtX402URI {\n\t\t\texts = append(exts, e)\n\t\t}\n\t}\n\tfor _, e := range in.Extensions {\n\t\tif u, _ := e["uri"].(string); u == module.ExtX402URI {\n\t\t\texts = append(exts, e)\n\t\t}\n\t}\n')],
  tests=[(NETCARD, '^(TestExtendedCardProperties|TestVariantsOfFreeAndListPricedNodes|TestExtCard1)$')], full=[NETCARD]),
 dict(id='M-PAY-52b', group='PAY', repo='ANet', desc='扩展卡变体向模块要贡献时给的是公开卡的 skill(声明与价格都按公开卡)',
  edits=[('internal/daemon/extcard.go', '\t\tSkills: netcard.SkillIDs(skills), Extended: true, Variant: v}', '\t\tSkills: netcard.SkillIDs(append(skills[:0:0], in.Skills...)), Extended: true, Variant: v}')],
  tests=[(DAEMON, '^(TestCardAnswerOfANode|TestPaymentsCardCheck)$')], full=[DAEMON]),
 dict(id='pay10-limit', group='PAY-10', repo='ANet', desc='cardAnswer 不按发送方限速(每分钟第 7 次照样回卡)',
  edits=[('internal/daemon/extcard.go', '\tcase in.Recent > cardRequestsPerWindow:', '\tcase false && in.Recent > cardRequestsPerWindow:')],
  tests=[(DAEMON, '^(TestCardAnswerTable|TestTheSeventhCardRequestIsBusy)$')], full=[DAEMON]),
 dict(id='pay10-deny', group='PAY-10', repo='ANet', desc='deny 名单中的对端若也在 allow 名单里,拿到名单变体',
  edits=[('internal/daemon/extcard.go', '\tlisted := d.readPeers().allowed(sender)', '\tps := d.readPeers()\n\tlisted := ps.allow[sender] || ps.trust[sender]')],
  tests=[(DAEMON, '^TestCardAnswerOfANode$')], full=[DAEMON]),
 dict(id='pay10-cache-content', group='PAY-10', repo='ANet', desc='签好的扩展卡只按公开卡 seq、变体与钥态缓存(名单能力变了仍给旧卡)',
  edits=[('internal/daemon/extcard.go', 'digest: sha256.Sum256(payload), keySeq: keySeq}', 'digest: sha256.Sum256(payload[:0]), keySeq: keySeq}')],
  tests=[(DAEMON, '^TestTheExtendedCardIsKeptUntilWhatItSaysChanges$')], full=[DAEMON]),
 dict(id='pay10-seq', group='PAY-10', repo='ANet', desc='扩展卡的 anet-card seq 不等于同一时刻公开卡的 seq',
  edits=[('internal/daemon/extcard.go', '\tfull.Seq = pubSeq\n', '\tfull.Seq = pubSeq + 1\n')],
  tests=[(DAEMON, '^TestCardAnswerOfANode$')], full=[DAEMON]),
 dict(id='pay10-jku', group='PAY-10', repo='ANet', desc='扩展卡带 jku',
  edits=[('internal/daemon/extcard.go', '\tsigned, err := a2acard.SignWithSnapshot(unsigned, signer, "")', '\tsigned, err := a2acard.SignWithSnapshot(unsigned, signer, netcard.JKU(in.HubURL, d.AID()))')],
  tests=[(DAEMON, '^TestCardAnswerOfANode$')], full=[DAEMON]),
 dict(id='pay10-two-lists', group='PAY-10', repo='ANet', desc='扩展卡收下两份价格表(按排序留下其中一份)',
  edits=[('internal/netcard/extended.go', '\t\t\tif lists++; lists > 1 {', '\t\t\tif lists++; false && lists > 1 {')],
  tests=[(NETCARD, '^TestAStrayPriceListIsKeptOffThePublicCardAndRefusedElsewhere$'), (DAEMON, '^TestPaymentsCardCheck$')],
  full=[NETCARD, DAEMON]),
 dict(id='pay10-price-bound', group='PAY-10', repo='ANet', desc='价格表接受大于 2^63−1 的金额',
  edits=[('internal/netcard/extended.go', '\tif err != nil || n > maxAmount {', '\tif err != nil {')],
  tests=[(NETCARD, '^TestBuildExtendedRefusesAPriceListThatDoesNotHold$')], full=[NETCARD]),
 dict(id='pay10-doctor-price', group='PAY-10', repo='ANet', desc='doctor payments.card 不先查价格范围(报成 card_failed 而不是 price_invalid)',
  edits=[('internal/daemon/extcard.go', '\t\tif !netcard.ValidPrice(prices[id]) {', '\t\tif false && !netcard.ValidPrice(prices[id]) {')],
  tests=[(DAEMON, '^TestPaymentsCardCheck$')], full=[DAEMON]),
 dict(id='pay10-x402-public-price', group='PAY-10', repo='ANet', desc='x402 模块对公开卡也贡献价格表',
  edits=[('module/x402/card.go', '\tif c.Extended {', '\tif true {')],
  tests=[(X402, '^TestCardExtensionsCarryNoPrice$')], full=[X402]),
 dict(id='pay10-cache-seq', group='PAY-10', repo='ANet', desc='签好的扩展卡不按公开卡 seq 缓存(公开卡以新 seq 重签后仍给旧 seq 的卡)',
  edits=[('internal/daemon/extcard.go', 'key := extCardKey{pubSeq: pubSeq, variant: v,', 'key := extCardKey{pubSeq: pubSeq * 0, variant: v,')],
  tests=[(DAEMON, '^TestTheExtendedCardFollowsANewPublicSeq$')], full=[DAEMON]),
 dict(id='pay10-published-name', group='PAY-10', repo='ANet', desc='扩展卡用配置里的名字而不是已发布公开卡的名字(应答时另签一张公开卡)',
  edits=[('internal/daemon/extcard.go', 'd.issueNetworkCardLocked(hub, d.issuedCardNameLocked(cfg.Name), false)',
          'd.issueNetworkCardLocked(hub, cfg.Name+d.issuedCardNameLocked("")[:0], false)')],
  tests=[(DAEMON, '^TestTheExtendedCardRepeatsThePublishedName$')], full=[DAEMON]),
 dict(id='pay10-clock-back', group='PAY-10', repo='ANet', desc='限速窗口收下晚于 now 的时刻(墙钟回拨后发送方一直 busy)',
  edits=[('internal/daemon/extcard.go', '\t\t\tif t > since && t <= now {', '\t\t\tif t > since {')],
  tests=[(DAEMON, '^TestCardRequestLimitSurvivesAClockStepBack$')], full=[DAEMON]),
]
