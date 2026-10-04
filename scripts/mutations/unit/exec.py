# exec.py: local execution and credentials (v0.3.0 LEC §7.3) — mutation list for scripts/mutations/unitmut.py (see its
# header for the format). EXE-1 adds M-SB-1 to M-SB-3 (the Linux sandbox kernel); the other EXE packages append theirs.
#
# The probe tests (TestSandboxProbe*, TestSandboxIntegrationConfinesTheAgent) need a bubblewrap that can create user
# namespaces: on a host without one they skip, and the bwrap-free tests listed with them (G-BWRAP, G-SECCOMP, the
# rule table and the in-kernel filter test) carry the kill. The CI sandbox job, where a skip is a failure, and a
# Debian 12 host run the probe tests themselves red for each of these mutations.

PROBE = 'TestSandboxProbeFiveItems|TestSandboxProbeSeccompAloneRefusesUserNamespaces|TestSandboxIntegrationConfinesTheAgent'

MUTS = [
 dict(
  id='m-sb-1', group='SB-2', repo='ANet',
  desc='bwrap 参数删掉 --unshare-net(沙箱与宿主共用网络)',
  edits=[
   ('internal/daemon/sandbox_linux.go', '"--unshare-user", "--unshare-net", "--unshare-ipc",', '"--unshare-user", "--unshare-ipc",'),
  ],
  tests=[('ANet:./internal/daemon', PROBE), ('ANet:./internal/daemon', 'TestBwrapGolden|TestBwrapArgsHaveTheSandboxShape')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-sb-2', group='SB-2', repo='ANet',
  desc='清空 seccomp 过滤器(程序只剩 ret ALLOW)',
  edits=[
   ('internal/daemon/seccomp_linux.go', '\tvar a bpfAssembler\n\t// Architecture first',
    '\tvar a bpfAssembler\n\tif true {\n\t\ta.ret(seccompRetAllow)\n\t\treturn a.assemble()\n\t}\n\t// Architecture first'),
  ],
  tests=[('ANet:./internal/daemon', PROBE), ('ANet:./internal/daemon', 'TestSeccompGolden|TestSeccompRules|TestSeccompFilterInKernel')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-sb-3', group='SB-2', repo='ANet',
  desc='seccomp 不过滤 CLONE_NEWUSER,同时当作 bwrap 不支持 --disable-userns',
  edits=[
   ('internal/daemon/seccomp_linux.go', '\ta.jeq(abi.nr["clone"], "newuser", "")\n\ta.jeq(abi.nr["unshare"], "newuser", "")\n',
    '\ta.jeq(abi.nr["clone"], "allow", "")\n\ta.jeq(abi.nr["unshare"], "allow", "")\n'),
   ('internal/daemon/sandbox_linux.go', 'return bwrapFeatures{DisableUserns: has("--disable-userns"), Size: has("--size")}',
    'return bwrapFeatures{DisableUserns: false, Size: has("--size")}'),
  ],
  tests=[('ANet:./internal/daemon', PROBE), ('ANet:./internal/daemon', 'TestSeccompGolden|TestSeccompRules|TestSeccompFilterInKernel')],
  full=['ANet:./internal/daemon'],
 ),
]

# EXE-13: the node side of joining a hub (LEC §4.3.10 AD-1 to AD-9, §7.3 M-AD-1 to M-AD-6), and the
# surfaces it reaches (MC-2, LA-2, §4.10 install.sh, SKILL.md, nginx). The P-JOIN cases run 400 random
# sequences of hub answers and restarts; the daemon cases run a daemon against the fake hub.
DAEMON = 'ANet:./internal/daemon'
MUTS += [
 dict(
  id='m-ad-1', group='AD-4', repo='ANet',
  desc='未确认准入即调用 Prepare(check_only 的任何回答之后都去构造 rot)',
  edits=[('internal/daemon/hubjoin.go', '\t\tif act != actPrepare {\n\t\t\treturn joinOutcome{rec: r}\n\t\t}',
          '\t\tif false && act != actPrepare {\n\t\t\treturn joinOutcome{rec: r}\n\t\t}')],
  tests=[(DAEMON, 'TestPJoin$|TestJoinAClosedHubWaitsForAdmissionThenFinishes')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-2', group='AD-4', repo='ANet',
  desc='重试时重复构造 rot(不记住本进程已为 (hub, 顶端 seq) 准备过的 KEL)',
  edits=[('internal/daemon/hubjoin.go', '\t\tif kel, ok = e.prepared.get(key); !ok {',
          '\t\tif kel, ok = e.prepared.get(key); true {')],
  tests=[(DAEMON, 'TestPJoin$')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-3', group='AD-6', repo='ANet',
  desc='待准入时发送照常进队列(发送闸门总是放行)',
  edits=[('internal/daemon/hubjoin_daemon.go', '\tif v.State == JoinRegistered {\n\t\treturn nil\n\t}',
          '\tif true || v.State == JoinRegistered {\n\t\treturn nil\n\t}')],
  tests=[(DAEMON, 'TestSendingWaitsForTheHub|TestARegisteredNodeKeepsSendingWhileItsHubIsAway')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-4', group='AD-5', repo='ANet',
  desc='已注册节点的暂时失败把 hub.state 改为 retrying',
  edits=[('internal/daemon/hubjoin.go', '\t\tif r.RegisteredAt == 0 {\n\t\t\treason, why := reasonRetrying, a.why',
          '\t\tif true {\n\t\t\treason, why := reasonRetrying, a.why')],
  tests=[(DAEMON, 'TestATemporaryFailureAfterRegistrationChangesNothing|TestPJoin$|TestARegisteredNodeKeepsSendingWhileItsHubIsAway')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-5', group='AD-1', repo='ANet',
  desc='首次连接失败仍写 config.json 与 hub_join.json',
  edits=[('internal/daemon/hubjoin_daemon.go', '\tc, jerr := d.contactHub(ctx, req.Hub)\n\tif jerr != nil {\n\t\treturn hubRegisterResp{}, jerr\n\t}',
          '\tc, jerr := d.contactHub(ctx, req.Hub)\n\tif jerr != nil {\n\t\t_, _ = d.adoptJoinTarget(req, "", nil)\n\t\treturn hubRegisterResp{}, jerr\n\t}')],
  tests=[(DAEMON, 'TestJoinAnUnreachableHubWritesNothing')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-6a', group='AD-9', repo='ANet',
  desc='unprotected 改变了应答的加入状态码',
  edits=[('internal/daemon/hubjoin_daemon.go', 'State: v.State, Reason: v.Reason, Message: v.Message,',
          'State: v.State, Reason: map[bool]string{true: "recovery_setup_required", false: v.Reason}[unprotected], Message: v.Message,')],
  tests=[(DAEMON, 'TestUnprotectedChangesOnlyTheRecoveryReminder|TestGJoin')],
  full=[DAEMON],
 ),
 dict(
  id='m-ad-6b', group='AD-9', repo='ANet',
  desc='没有终端的 hub-register 停在等待口令(不带 next_digest 接入)',
  edits=[('cmd/anet/hubjoin.go', '\tif dry.Action == "join" && dry.RecoveryNeeded {',
          '\tif dry.Action == "join" && dry.RecoveryNeeded && h.terminal() {')],
  tests=[('ANet:./cmd/anet', 'TestHubRegisterWithoutATerminal')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='exe13-wake', group='AD-3', repo='ANet',
  desc='hub-register 之后后台循环不知道下一次尝试的时刻(不再按退避重试)',
  edits=[('internal/daemon/hubjoin_daemon.go', '\td.join.rec = &cp\n\td.join.mu.Unlock()\n\td.wakeJoin()',
          '\td.join.rec = &cp\n\td.join.mu.Unlock()')],
  tests=[(DAEMON, 'TestTheLoopRetriesWhenDue')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-uxr6', group='HN-4', repo='ANet',
  desc='admission_required 当作暂时失败(不进入 pending_admission)',
  edits=[('internal/daemon/hubjoin.go', '\tcase "admission_required":\n\t\ta.kind = ansAdmissionRequired',
          '\tcase "admission_required":\n\t\ta.kind, a.why = ansTransient, whyInternal')],
  tests=[(DAEMON, 'TestHubAnswersMapAsUXR6')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-mc2', group='MC-2', repo='ANet',
  desc='MCP 工具错误在原因表的句子后面附加标签',
  edits=[('internal/mcpserv/mcpserv.go', '\tif e.Message != "" && isReasonCode(e.Reason) {',
          '\tif false && e.Message != "" && isReasonCode(e.Reason) {')],
  tests=[('ANet:./internal/mcpserv', 'TestASendRefusalIsTheSentence')],
  full=['ANet:./internal/mcpserv'],
 ),
 dict(
  id='exe13-la2', group='LA-2', repo='ANet',
  desc='本机 A2A 不在绑定之前回答发送闸门',
  edits=[('module/a2a/server.go', '\t\tif s.precheckSendGate(w, r, aid, jsonrpc) {',
          '\t\tif false && s.precheckSendGate(w, r, aid, jsonrpc) {')],
  tests=[('ANet:./module/a2a', 'TestSendsWaitForTheHub')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='exe13-install', group='AV-5', repo='ANet',
  desc='install.sh 的 cli_removed 句子与原因表不再逐字相同',
  edits=[('deploy/release/install.sh', "remove this flag, run anet hub-register <url>", "remove the flag, run anet hub-register <url>")],
  tests=[('ANet:./internal/release', 'TestInstallShRefusesTheRemovedInviteFlags')],
  full=['ANet:./internal/release'],
 ),
 dict(
  id='exe13-skill', group='§4.10', repo='ANet',
  desc='SKILL.md 的 agent_hint.join 改了一个词',
  edits=[('deploy/release/apex/SKILL.md', 'Join with anet hub-register even without a terminal;', 'Join with anet hub-register even without any terminal;')],
  tests=[('ANet:./cmd/anet', 'TestSiteJSONAgentHintMatchesSkill')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='exe13-nginx', group='§4.10', repo='ANet',
  desc='/install.sh 改回 add_header(丢掉 server 块的安全头)',
  edits=[('deploy/release/apex/nginx-locations.conf', '        types { } default_type "text/x-shellscript; charset=utf-8";\n        expires epoch;',
          '        types { } default_type "text/x-shellscript; charset=utf-8";\n        add_header Cache-Control "no-cache, must-revalidate" always;')],
  tests=[('ANet:./cmd/anet', 'TestNginxServesSiteJSONNoCache')],
  full=['ANet:./cmd/anet'],
 ),
]
