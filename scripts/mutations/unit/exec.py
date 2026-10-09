# exec.py: local execution and credentials (v0.3.0 LEC §7.3) — mutation list for scripts/mutations/unitmut.py (see its
# header for the format). EXE-1 adds M-SB-1 to M-SB-3 (the Linux sandbox kernel); EXE-2 adds M-BP-1 to M-BP-7 (the
# injection route's request policy, with m-bp-3b, m-bp-4b and m-bp-5b) and the egress endpoint's own mutations (m-eg-*,
# m-up-1); EXE-4 adds M-BU-1 to M-BU-5 and M-SB-5 (the run budget, its slots, the work roots) with the
# rest of its rules (m-bu-*, m-sb-*, m-gt-*, m-eg-7, m-evid-1, m-ld-1); the other EXE packages append theirs.
#
# The probe tests (TestSandboxProbe*, TestSandboxIntegrationConfinesTheAgent) need a bubblewrap that can create user
# namespaces: on a host without one they skip, and the bwrap-free tests listed with them (G-BWRAP, G-SECCOMP, the
# rule table and the in-kernel filter test) carry the kill. The CI sandbox job, where a skip is a failure, and a
# Debian 12 host run the probe tests themselves red for each of these mutations.

PROBE = 'TestSandboxProbeFiveItems|TestSandboxProbeSeccompAloneRefusesUserNamespaces|TestSandboxIntegrationConfinesTheAgent'

# The injection route's policy tests (EXE-2): the SI-57 corpus through the endpoint, the rules one by one, G-BP, and the
# endpoint's credential test.
INJECT = 'TestInjectPolicyAsserts|TestInjectBodyPolicy|TestInjectPolicyGolden|TestEgressForwardsWithTheOperatorKeyOnly'

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
 # --- EXE-2: the egress endpoint and the injection route (LEC §4.3.3, §4.3.4; SI-57) ---
 dict(
  id='m-bp-1', group='BP', repo='ANet',
  desc='注入路由不删入站的 Authorization / x-api-key(agent 自带的 key 到达上游)',
  edits=[
   ('internal/daemon/egress_policy.go', '\t"Authorization": true, "X-Api-Key": true, "Api-Key": true,\n', '\t"Api-Key": true,\n'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-2', group='BP', repo='ANet',
  desc='不把 model 改写为钉死的模型',
  edits=[
   ('internal/daemon/egress_policy.go', '\tobj["model"] = up.Model // BP-2', '\t_ = up.Model // BP-2'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-3', group='BP', repo='ANet',
  desc='不钳制 max_tokens 等输出上限',
  edits=[
   ('internal/daemon/egress_policy.go', 'if n, ok := jsonInteger(obj[k]); ok && n >= 1 && n <= egressMaxOutputTokens {',
    'if n, ok := jsonInteger(obj[k]); ok || n <= egressMaxOutputTokens {'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-4', group='BP', repo='ANet',
  desc='放行未知的工具类型(web_fetch、code_execution、托管工具)',
  edits=[
   ('internal/daemon/egress_policy.go', '\t\t\treturn typ, true\n\t\t}\n\t}\n\treturn typ, false\n}', '\t\t\treturn typ, true\n\t\t}\n\t}\n\treturn typ, true\n}'),
   ('internal/daemon/egress_policy.go', 'return typ, egressResponsesClientTools[typ]', 'return typ, true'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-5', group='BP', repo='ANet',
  desc='不拒绝 mcp_servers',
  edits=[
   ('internal/daemon/egress_policy.go', '\tif presentMember(obj, "mcp_servers") {', '\tif false && presentMember(obj, "mcp_servers") {'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-5b', group='BP', repo='ANet',
  desc='不拒绝 previous_response_id',
  edits=[
   ('internal/daemon/egress_policy.go', 'refuseFields(obj, "conversation", "prompt", "previous_response_id")', 'refuseFields(obj, "conversation", "prompt")'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-6', group='BP', repo='ANet',
  desc='OpenAI 请求不强制 store:false',
  edits=[
   ('internal/daemon/egress_policy.go', '\tobj["store"] = false\n', ''),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-7', group='BP', repo='ANet',
  desc='anthropic-beta 白名单加入 mcp-client-*',
  edits=[
   ('internal/daemon/egress_policy.go', '\t"computer-use-2025-01-24":                true,\n}',
    '\t"computer-use-2025-01-24":                true,\n\t"mcp-client-2025-11-20": true,\n}'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressBetaAllowlists|' + INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-1', group='EG', repo='ANet',
  desc='absolute-form 不核对目标是端点自身(任何主机的同名路径都当作注入路由)',
  edits=[
   ('internal/daemon/egress.go', '!egressSelf[strings.ToLower(u.Host)]', 'false'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressRefusesWhatIsNotTheRoute')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-2', group='EG', repo='ANet',
  desc='流式响应不逐块 flush(EG-6)',
  edits=[
   ('internal/daemon/egress.go', '\t\t\t\tflusher.Flush() // EG-6', '\t\t\t\t_ = flusher // EG-6'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressStreamsChunkByChunk')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-3', group='EG', repo='ANet',
  desc='出站端点 accept 时不核对对端 uid',
  edits=[
   ('internal/daemon/sandbox_linux.go', 'se.ep.peerCheck = egressPeerIsUser(egressPeerUID())', 'se.ep.peerCheck = nil'),
  ],
  tests=[('ANet:./internal/daemon', 'TestSandboxEgressPeerMustBeTheDaemonUser')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-4', group='UP', repo='ANet',
  desc='沙箱环境保留 daemon 的 key 与代理变量(UP-4 占位值失效)',
  edits=[
   ('internal/daemon/egress_upstreams.go', '\t\tif !drop[k] {', '\t\tif !drop[k] || true {'),
  ],
  tests=[('ANet:./internal/daemon', 'TestSandboxEgressEnv|TestSandboxCommandKeepsTheAgentOffTheCommandLine|TestSandboxCommandOpensTheEgressEndpoint|' + PROBE)],
  full=['ANet:./internal/daemon'],
 ), dict(
  id='m-bp-3b', group='BP', repo='ANet',
  desc='零或负的输出上限原样转发(本机 Ollama、llama.cpp 视之为不设上限)',
  edits=[
   ('internal/daemon/egress_policy.go', 'ok && n >= 1 && n <= egressMaxOutputTokens', 'ok && n <= egressMaxOutputTokens'),
   ('internal/daemon/egress_policy.go', '\ts := string(n)\n\tif s == "" || len(s) > 9', '\ts := strings.TrimPrefix(string(n), "-")\n\tif s == "" || len(s) > 9'),
   ('internal/daemon/egress_policy.go', 'i, err := strconv.ParseInt(s, 10, 64)', 'i, err := strconv.ParseInt(string(n), 10, 64)'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-bp-4b', group='BP', repo='ANet',
  desc='chat 的 tool_choice allowed_tools.tools 不检查工具类型',
  edits=[
   ('internal/daemon/egress_policy.go', 'm["allowed_tools"].(map[string]any); isObj {', 'm["allowed_tools"].(map[string]any); isObj && false {'),
  ],
  tests=[('ANet:./internal/daemon', INJECT)],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-5', group='EG', repo='ANet',
  desc='请求体不再逐个读取与改写(32 个连接同时解码 8 MiB 请求体)',
  edits=[
   ('internal/daemon/egress.go', '\t\tcase e.bodySlot <- struct{}{}:\n', '\t\tdefault:\n'),
   ('internal/daemon/egress.go', '\t\t<-e.bodySlot\n', ''),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressReadsOneBodyAtATime')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-eg-6', group='EG', repo='ANet',
  desc='Close 不等处理函数返回(运行的计数在读取后才落定)',
  edits=[
   ('internal/daemon/egress.go', '\t\te.handlers.Wait()\n', ''),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressCloseWaitsForItsHandlers')],
  full=['ANet:./internal/daemon'],
 ),
 dict(
  id='m-up-1', group='UP', repo='ANet',
  desc='api_base 主机名末尾的点不归一(openrouter.ai. 绕过 plugins 与 models 的拒绝)',
  edits=[
   ('internal/daemon/egress_upstreams.go', 'host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")', 'host := strings.ToLower(base.Hostname())'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressUpstreamSelection')],
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
          '\t\tif kel, ok = e.prepared.get(key); !ok || true {')],
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
  edits=[('internal/daemon/hubjoin_daemon.go', '\tc, jerr := d.contactHub(ctx, req.Hub, true)\n\tif jerr != nil {\n\t\treturn hubRegisterResp{}, jerr\n\t}',
          '\tc, jerr := d.contactHub(ctx, req.Hub, true)\n\tif jerr != nil {\n\t\t_, _ = d.adoptJoinTarget(req, "", nil)\n\t\treturn hubRegisterResp{}, jerr\n\t}')],
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
  edits=[('internal/daemon/hubjoin_daemon.go', '\t\td.join.rec = &cp\n\t}\n\td.join.mu.Unlock()\n\td.wakeJoin()\n}',
          '\t\td.join.rec = &cp\n\t}\n\td.join.mu.Unlock()\n}')],
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

 # The review of EXE-13: checks whose removal no test saw before.
 dict(
  id='exe13-skew', group='AD-4', repo='ANet',
  desc='hub 拒绝 rot 的时间(kel_ts_out_of_window)后仍复用旧 KEL,不再请身份区重签(KEL D-36)',
  edits=[('internal/daemon/hubjoin.go', '\t\t// area signs it again on the next Prepare (KEL D-36).\n\t\te.prepared.drop(key)\n',
          '\t\t// area signs it again on the next Prepare (KEL D-36).\n')],
  tests=[(DAEMON, 'TestARefusedRotTimeIsPreparedAgain')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-adopt', group='AD-4', repo='ANet',
  desc='Adopt 失败被当作成功(丢掉已准备的 rot,不排下一次尝试)',
  edits=[('internal/daemon/hubjoin.go', '\t\tif err := e.hooks.Adopt(ctx, res); err != nil {',
          '\t\tif err := e.hooks.Adopt(ctx, res); false && err != nil {')],
  tests=[(DAEMON, 'TestAFailedAdoptIsRetriedWithTheSameRot')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-gate-followup', group='AD-6', repo='ANet',
  desc='未注册时已有任务上的后续消息照常发送',
  edits=[('internal/daemon/taskseam.go', '\tif msg.TaskID != "" {\n\t\tif ref := d.sendGate(); ref != nil {',
          '\tif msg.TaskID != "" {\n\t\tif ref := d.sendGate(); false && ref != nil {')],
  tests=[(DAEMON, 'TestTheOtherSendRoutesWaitForTheHub')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-gate-console', group='AD-6', repo='ANet',
  desc='控制台的 multipart /delegate 不过发送闸门',
  edits=[('internal/daemon/control_api.go', '\t\tif !d.passSendGate(w) {\n\t\t\treturn\n\t\t}\n\t\tctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)\n\t\tdefer cancel()\n\t\tid, err := d.DelegateAtts(ctx, provider, goal, atts)',
          '\t\tctx, cancel := context.WithTimeout(r.Context(), relayCallTimeout)\n\t\tdefer cancel()\n\t\tid, err := d.DelegateAtts(ctx, provider, goal, atts)')],
  tests=[(DAEMON, 'TestTheOtherSendRoutesWaitForTheHub')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-relay', group='AD-2', repo='ANet',
  desc='待准入时中继循环照常轮询(hub 上没有信箱,每秒被拒)',
  edits=[('internal/daemon/relay.go', '\t\t\tif !d.joinRegistered() {\n\t\t\t\t// Until', '\t\t\tif false && !d.joinRegistered() {\n\t\t\t\t// Until')],
  tests=[(DAEMON, 'TestTheRelayLoopWaitsForRegistration')],
  full=[DAEMON],
 ),
 dict(
  id='exe13-dryrun-pin', group='§2.3.2', repo='ANet',
  desc='dry_run 把 hub 的 KEL 写进 peer_identity(dry_run 不得写任何东西)',
  edits=[('internal/daemon/hubjoin_daemon.go', '\tc, jerr := d.contactHub(ctx, hubURL, false)', '\tc, jerr := d.contactHub(ctx, hubURL, true)')],
  tests=[(DAEMON, 'TestDryRunActions')],
  full=[DAEMON],
 ),
]

# --- EXE-4: the run budget, its slots, the work roots, the user scope and the records (LEC §4.3.1, SB-7 to
# SB-9, §4.3.6, EG-7, §2.5; SI-58, SI-59). The daemon tests run a sandboxed turn through fakeSandbox where the
# host has no usable bubblewrap; TestSandboxTrustChangeUsesFreshWorkDir and TestSandboxDiskWatchKillsTheRun are
# the real-bwrap forms (Debian 12 container, CI sandbox job). ---
BU = 'TestRunBudget|TestUntrustedRunsAreBudgeted|TestTheDailyBudgetSpansPeers|TestTheBudgetSurvivesARestart|TestTheBudgetAndTheSpendingLimitsShareTheLocalDay'
MUTS += [
 dict(
  id='m-bu-1', group='BU-2', repo='ANet',
  desc='预算不计数(放行的运行不计入当日次数)',
  edits=[('internal/daemon/sandbox_budget.go', '\tday.add(peer, 1)\n\tb.running++', '\tb.running++')],
  tests=[(DAEMON, BU)],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-2', group='BU-2', repo='ANet',
  desc='重启不回放证据链(当日计数从零开始)',
  edits=[('internal/daemon/sandbox_budget.go', '\tfor _, r := range runs(start.Add(-48 * time.Hour)) {', '\tfor _, r := range runs(start.Add(-48 * time.Hour))[:0] {')],
  tests=[(DAEMON, 'TestTheBudgetSurvivesARestart|TestRunBudgetReplaysTheChain|TestRunBudgetProperty')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-3', group='BU-4', repo='ANet',
  desc='超限仍启动 agent / 调用模型',
  edits=[('internal/daemon/autoreply.go', '\tcase budgetSpent:\n\t\treturn d.refuseTurn(', '\tcase budgetSpent + 100:\n\t\treturn d.refuseTurn(')],
  tests=[(DAEMON, 'TestUntrustedRunsAreBudgeted|TestTheDailyBudgetSpansPeers|TestAnOutboundTurnOverTheBudgetStaysInTheInbox')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-4', group='BU-1', repo='ANet',
  desc='openai 后端不计入预算(core Q-LEC-6)',
  edits=[('internal/daemon/autoreply.go', 'return execGate{trusted: trusted, mode: modeOpenAI, counted: !trusted}', 'return execGate{trusted: trusted, mode: modeOpenAI, counted: false}')],
  tests=[(DAEMON, 'TestUntrustedRunsAreBudgeted/openai|TestTheDailyBudgetSpansPeers|TestTheBudgetSurvivesARestart')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-5', group='BU-2', repo='ANet',
  desc='预算按固定 24 小时窗口(UTC 日)而不是 localDayBounds',
  edits=[('internal/daemon/sandbox_budget.go', '\tb.loadLocked(now, runs)\n\tstart, end := localDayBounds(now)', '\tb.loadLocked(now, runs)\n\tstart, end := now.Truncate(24*time.Hour), now.Truncate(24*time.Hour).Add(24*time.Hour)')],
  tests=[(DAEMON, 'TestRunBudgetProperty|TestRunBudgetCountsTheLocalDay|TestTheBudgetAndTheSpendingLimitsShareTheLocalDay')],
  full=[DAEMON],
 ),
 dict(
  id='m-ld-1', group='BU-2', repo='ANet',
  desc='localDayBounds 的结束取 time.Date(日+1) 并按 24 小时推开始(夏令时与跨零点的日子算错)',
  edits=[('internal/daemon/localday.go', '\tend, ok = firstOnOrAfterDate(y, m, d+1, now, now.Add(50*time.Hour))', '\tend, ok = time.Date(y, m, d+1, 0, 0, 0, 0, now.Location()), true\n\tstart = end.Add(-24 * time.Hour)')],
  tests=[(DAEMON, 'TestLocalDayBounds|TestRunBudgetProperty')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-6', group='BU-5', repo='ANet',
  desc='没有空位时拒绝(budget)而不是留给下一次扫描',
  edits=[('internal/daemon/autoreply.go', '\tcase budgetBusy:\n\t\treturn nil', '\tcase budgetBusy:\n\t\treturn d.refuseTurn(ctx, th, autoReplyRun{InteractionID: th.InteractionID, Peer: th.Peer, Mode: gate.mode, Exit: exitBudget}, wait)')],
  tests=[(DAEMON, 'TestUntrustedRunsTakeTurns')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-7', group='BU-5', repo='ANet',
  desc='未信任对端的并发放宽到 3',
  edits=[('internal/daemon/sandbox_budget.go', '\tuntrustedRunsAtOnce        = 2', '\tuntrustedRunsAtOnce        = 3')],
  tests=[(DAEMON, 'TestRunBudgetSlots|TestUntrustedRunsTakeTurns|TestRunBudgetProperty')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-8', group='BU-2', repo='ANet',
  desc='沙箱不可用(什么都没启动)的运行也计入预算',
  edits=[('internal/daemon/autoreply.go', '\t\t// refusal\'s (once per interaction).\n\t\tcounted = false', '\t\t// refusal\'s (once per interaction).\n\t\tcounted = true')],
  tests=[(DAEMON, 'TestARunTheSandboxRefusedDoesNotCount')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-5', group='SB-8', repo='ANet',
  desc='沙箱与 direct 共用工作根',
  edits=[('internal/daemon/autoreply_exec.go', '\tif root := execWorkRoot(); root != "" {\n\t\treturn root + "-sandbox"', '\tif root := execWorkRoot(); root != "" {\n\t\treturn root')],
  tests=[(DAEMON, 'TestTrustChangeUsesFreshWorkDir|TestSandboxTrustChangeUsesFreshWorkDir')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-5b', group='SB-8', repo='ANet',
  desc='auto_reply.work_dir 可以落在沙箱工作根之内',
  edits=[('internal/daemon/autoreply_exec.go', '\tabs, err := filepath.Abs(c.WorkDir)\n\tif err == nil {\n\t\terr = checkOutsideSandboxRoot(abs)\n\t}', '\tabs, err := filepath.Abs(c.WorkDir)'),
         ('internal/daemon/autoreply_exec.go', '\t\tif err := checkOutsideSandboxRoot(abs); err != nil {\n\t\t\treturn execInvokeOpts{}, fmt.Errorf("auto_reply.work_dir: %w", err)\n\t\t}\n', '')],
  tests=[(DAEMON, 'TestTheWorkDirStaysOutOfTheSandboxRoot')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-7', group='SB-7', repo='ANet',
  desc='用户级 systemd 在运行时也不包 systemd-run(没有内存与进程数上限)',
  edits=[('internal/daemon/sandbox_linux.go', '\tpath, full := sandboxCommandLine(p.Bwrap, p.SystemdRun, args)', '\tpath, full := sandboxCommandLine(p.Bwrap, "", args)')],
  tests=[(DAEMON, 'TestRunsGetAUserScopeWhenTheUserSystemdRuns')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8', group='BU-8', repo='ANet',
  desc='磁盘看门狗不杀超限的运行',
  edits=[('internal/daemon/autoreply_exec.go', '\t\t\tif dirsSize(dirs(), limit) > limit {', '\t\t\tif false && dirsSize(dirs(), limit) > limit {')],
  tests=[(DAEMON, 'TestADiskHogIsKilled|TestSandboxDiskWatchKillsTheRun')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-9', group='SB-9', repo='ANet',
  desc='启动后不清理上一个进程留下的运行目录',
  edits=[('internal/daemon/autoreply_exec.go', '\td.sandboxSwept.Do(func() { removeSandboxLeftovers(d.AID()) })', '\td.sandboxSwept.Do(func() {})')],
  tests=[(DAEMON, 'TestSandboxLeftoversAreSweptBeforeTheFirstRun')],
  full=[DAEMON],
 ),
 dict(
  id='m-gt-2', group='GT-2', repo='ANet',
  desc='trusted=sandbox 的一轮按 direct 运行(退回 direct)',
  edits=[('internal/daemon/autoreply.go', '\tcase trusted && cfg.TrustedMode() == TrustedSandbox:\n\t\tg.mode = modeSandbox', '\tcase trusted && cfg.TrustedMode() == TrustedSandbox:\n\t\tg.mode = modeDirect')],
  tests=[(DAEMON, 'TestTrustedPeersRunAsTrustedSays')],
  full=[DAEMON],
 ),
 dict(
  id='m-gt-2b', group='GT-2', repo='ANet',
  desc='拒绝的状态消息带正文(UX-R1:只带 anet.reason)',
  edits=[('internal/daemon/autoreply.go', 'interactions.StateRejected, "", meta)', 'interactions.StateRejected, "turned away: "+reason, meta)')],
  tests=[(DAEMON, 'TestUntrustedRunsAreBudgeted|TestTrustedPeersRunAsTrustedSays')],
  full=[DAEMON],
 ),
 dict(
  id='m-gt-2c', group='GT-2', repo='ANet',
  desc='同一交互同一原因的拒绝每次都记日志与证据',
  edits=[('internal/daemon/autoreply.go', '\tif d.sbRefused.first(th.InteractionID, run.Exit) {', '\tif d.sbRefused.first(th.InteractionID, run.Exit) || true {')],
  tests=[(DAEMON, 'TestAnOutboundTurnOverTheBudgetStaysInTheInbox')],
  full=[DAEMON],
 ),
 dict(
  id='m-gt-5', group='GT-5', repo='ANet',
  desc='恢复 policy open 与 exec untrusted=sandbox 的互斥',
  edits=[('internal/daemon/inbound.go', '\tif in.Policy != PolicyOpen {\n\t\treturn nil\n\t}\n\tif untrustedBackend {', '\tif in.Policy != PolicyOpen {\n\t\treturn nil\n\t}\n\tif ar := c.AutoReply; ar != nil && ar.Backend == "exec" && ar.UntrustedMode() != UntrustedOff {\n\t\treturn ErrPolicyConflict\n\t}\n\tif untrustedBackend {')],
  tests=[(DAEMON, 'TestOpenWithTheSandboxForUntrustedPeers')],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-7', group='EG-7', repo='ANet',
  desc='anet.autoreply.invoked 不带沙箱运行的 egress 汇总',
  edits=[('internal/daemon/autoreply.go', '\t\treply, run.Sandboxed, run.Egress = res.Reply, res.Sandboxed, res.Egress', '\t\treply, run.Sandboxed = res.Reply, res.Sandboxed')],
  tests=[(DAEMON, 'TestASandboxedRunRecordsItsEgress|TestSandboxTrustChangeUsesFreshWorkDir')],
  full=[DAEMON],
 ),
 dict(
  id='m-evid-1', group='§2.5', repo='ANet',
  desc='anet.autoreply.invoked 多出一个成员(工作目录路径)',
  edits=[('internal/daemon/autoreply.go', '\t\t"mode": r.Mode, "sandbox": r.Kind, "exit": r.Exit,\n', '\t\t"mode": r.Mode, "sandbox": r.Kind, "exit": r.Exit, "work_dir": "/work/" + r.InteractionID,\n')],
  tests=[(DAEMON, 'TestAutoReplyEvidenceGolden|TestAutoReplyRecordHoldsNoContent|TestUntrustedRunsAreBudgeted')],
  full=[DAEMON],
 ),
 # EXE-4 review: the halves of the disk watch, the budget's trusted exemption on the openai backend, the
 # start-up sweep's owner and its first run, and the work-tree removal staying inside the tree.
 dict(
  id='m-sb-8b', group='BU-8', repo='ANet',
  desc='磁盘看门狗只统计工作目录,不统计每次运行目录(沙箱内 /tmp、$HOME)',
  edits=[('internal/daemon/autoreply_exec.go', 'return append(plan.liveRunDirs(), workDir)', 'return []string{workDir}')],
  tests=[(DAEMON, 'TestSandboxDiskWatchCountsTheRunDirectory')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8c', group='BU-8', repo='ANet',
  desc='磁盘看门狗只按文件大小计(fallocate --keep-size 占的块不计)',
  edits=[('internal/daemon/autoreply_exec.go', 'total += max(fi.Size(), allocatedBytes(fi))', 'total += fi.Size()')],
  tests=[(DAEMON, 'TestDirsSizeCountsAllocatedBlocks')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8d', group='SB-8', repo='ANet',
  desc='删除工作目录时按路径 chmod(可被仍在运行的 agent 换成指向树外的链接)',
  edits=[('internal/daemon/autoreply_exec.go', '\t\t\t\t\t_ = r.Chmod(p, 0o700)', '\t\t\t\t\t_ = os.Chmod(filepath.Join(dir, p), 0o700)')],
  tests=[(DAEMON, 'TestRemoveWorkTreeStaysInTheTree')],
  full=[DAEMON],
 ),
 dict(
  id='m-bu-9', group='BU-1', repo='ANet',
  desc='openai 后端把信任对端的轮次也计入预算',
  edits=[('internal/daemon/autoreply.go', 'counted: !trusted}', 'counted: true}')],
  tests=[(DAEMON, 'TestATrustedPeerOnTheOpenAIBackendIsNotBudgeted')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-9b', group='SB-9', repo='ANet',
  desc='启动清理删掉临时目录里别的用户的同名目录',
  edits=[('internal/daemon/sandbox_linux.go', 'if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != leftoverOwner() {', 'if st, ok := fi.Sys().(*syscall.Stat_t); !ok || false && int(st.Uid) != leftoverOwner() {')],
  tests=[(DAEMON, 'TestSandboxLeftoversAreSweptBeforeTheFirstRun')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-9c', group='SB-9', repo='ANet',
  desc='开启 exec 自动回复时不清理残留(推迟到第一次沙箱运行)',
  edits=[('internal/daemon/autoreply.go', '\tif cfg.Backend == "exec" {\n\t\td.sweepSandboxLeftovers()\n\t}\n', '')],
  tests=[(DAEMON, 'TestSandboxLeftoversAreSweptWhenAutoReplyStarts')],
  full=[DAEMON],
 ),
]
# EXE-4 second review: the outbox a sandboxed agent can put a link in the place of, and the trees the disk watch
# and the removal cannot read through.
MUTS += [
 dict(
  id='m-sb-10', group='SB-8', repo='ANet',
  desc='outbox 换成指向运营者目录的链接时照样读取(把运营者的文件发给对端)',
  edits=[('internal/daemon/autoreply_exec.go', '\t} else if !fi.IsDir() {\n\t\treturn nil, fmt.Errorf("the outbox is not a directory', '\t} else if false && !fi.IsDir() {\n\t\treturn nil, fmt.Errorf("the outbox is not a directory'),
         ('internal/daemon/autoreply_exec.go', '\tob, err := parent.OpenRoot(base)', '\tob, err := os.OpenRoot(dir)')],
  tests=[(DAEMON, 'TestASandboxedAgentCannotLinkTheOutboxOut|TestSandboxOutboxLinkSendsNothing')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8e', group='BU-8', repo='ANet',
  desc='磁盘看门狗跳过读不了的目录(mode 000、超过最长路径),其中的内容不计',
  edits=[('internal/daemon/autoreply_exec.go', '\t\t\tcase err != nil:\n\t\t\t\ttotal = limit + 1 // unreadable: fail closed', '\t\t\tcase err != nil:\n\t\t\t\treturn nil')],
  tests=[(DAEMON, 'TestTheDiskWatchCannotBeHiddenFrom')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8f', group='BU-8', repo='ANet',
  desc='磁盘看门狗不给每一项计最少一个块(大量空文件不计)',
  edits=[('internal/daemon/autoreply_exec.go', 'total += max(fi.Size(), allocatedBytes(fi), sandboxDiskEntryMin)', 'total += max(fi.Size(), allocatedBytes(fi))')],
  tests=[(DAEMON, 'TestTheDiskWatchCannotBeHiddenFrom')],
  full=[DAEMON],
 ),
 dict(
  id='m-sb-8g', group='SB-8', repo='ANet',
  desc='删除工作目录时不先恢复顶层目录的权限(agent 把工作目录设为 000 后删不掉)',
  edits=[('internal/daemon/autoreply_exec.go', '\t\tif fi.Mode().Perm()&0o700 != 0o700 {\n\t\t\tif pr, err := os.OpenRoot(filepath.Dir(dir)); err == nil {', '\t\tif false && fi.Mode().Perm()&0o700 != 0o700 {\n\t\t\tif pr, err := os.OpenRoot(filepath.Dir(dir)); err == nil {')],
  tests=[(DAEMON, 'TestRemoveWorkTreeTakesAnUnreadableTop')],
  full=[DAEMON],
 ),
]
