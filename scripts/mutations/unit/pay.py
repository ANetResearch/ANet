# pay.py: mutations of the hub and payment privacy work that must turn their tests red (M-PAY-*).
# For scripts/mutations/unitmut.py (see its header for the format); entries name the repository
# they edit. Later PAY work items append their rows here.
#
# M-PAY-1 and M-PAY-2 are the ANetCore liability tree (package liabtree). Their key tests are the
# LIAB-1 refusals the golden vector carries for them: a path whose sums were moved between two
# levels (the root sum unchanged), two sibling accounts shown different splits of one parent sum
# (which also catches a node hash over the parent's total alone), and a tree whose sums wrap
# around 2^64.

LIAB_MOVED = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_moved_(sum|split)$')]
LIAB_WRAP = [('ANetCore:./liabtree', '^TestLIAB1$/^refuse_sum_wraps$')]

MUTS = [
 dict(id='M-PAY-1', group='PAY', repo='ANetCore', desc='负债树节点哈希不覆盖子和(L26 去掉 u64be(L.sum)、u64be(R.sum))',
  edits=[('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], l.Sum)\n', ''),
         ('liabtree/liabtree.go', '\tbinary.BigEndian.PutUint64(b[n:], r.Sum)\n', '')],
  tests=LIAB_MOVED, full=['ANetCore:./liabtree']),
 dict(id='M-PAY-2', group='PAY', repo='ANetCore', desc='负债树去掉和的溢出检查(addSum 直接相加)',
  edits=[('liabtree/liabtree.go', '\tif a > MaxSum || b > MaxSum || a+b > MaxSum {\n\t\treturn 0, false\n\t}\n', '')],
  tests=LIAB_WRAP, full=['ANetCore:./liabtree']),
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
  tests=[(HUB, '^TestWire3Fixtures$/^register\\.json$')], full=[HUB]),
 dict(id='M-PAY-44', group='PAY', repo='ANetHub', desc='墓碑写入不检查延伸(缩短或分叉的 KEL 可覆盖墓碑)',
  edits=[('internal/aghub/deregister.go', '\t\tif xerr := identity.ExtendsKEL(old, next); xerr != nil {\n',
          '\t\tif xerr := identity.ExtendsKEL(old, next); false && xerr != nil {\n')],
  tests=[(HUB, '^TestDeregTombstoneNeverShrinks$')], full=[HUB]),
 dict(id='M-PAY-46', group='PAY', repo='ANetHub', desc='进程外注销不写 dereg_event',
  edits=[('internal/aghub/deregister.go',
          '\tif _, err := tx.Exec(`INSERT INTO dereg_event(aid, at) VALUES(?,?)\n\t    ON CONFLICT(aid) DO UPDATE SET at=excluded.at`, aid, now.UnixMilli()); err != nil {\n\t\treturn DeregResult{}, err\n\t}\n', '')],
  tests=[(HUB, '^TestOutOfProcessDeregDropsSessions$')], full=[HUB]),
]
