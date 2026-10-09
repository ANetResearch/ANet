# exec.py: local execution and credentials (v0.3.0 LEC §7.3) — mutation list for scripts/mutations/unitmut.py (see its
# header for the format). EXE-1 adds M-SB-1 to M-SB-3 (the Linux sandbox kernel); EXE-2 adds M-BP-1 to M-BP-7 (the
# injection route's request policy, with m-bp-3b, m-bp-4b and m-bp-5b) and the egress endpoint's own mutations (m-eg-*,
# m-up-1); the other EXE packages append theirs.
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
 # --- EXE-3: the CONNECT allowlist (LEC §4.3.5, CN-1 to CN-8; EG-1, EG-4; SI-55), and the injection route's media by
 # URL and small max_tokens (BP-3, BP-5; SI-57) ---
 dict(
  id='m-cn-1', group='CN-4', repo='ANet',
  desc='CONNECT 跳过特殊地址检查(放行解析到回环、元数据服务、内网与本机地址的主机)',
  edits=[('internal/daemon/egress_connect.go', '\t\tif egressAddrRefused(ip) || slices.Contains(local, a) {',
          '\t\tif false && (egressAddrRefused(ip) || slices.Contains(local, a)) {')],
  tests=[(DAEMON, 'TestEgressConnectRefusesSpecialAddresses')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-1b', group='CN-4', repo='ANet',
  desc='不检查本机自己的地址(放行解析到本机公网地址的主机,等于连宿主)',
  edits=[('internal/daemon/egress_connect.go', 'if egressAddrRefused(ip) || slices.Contains(local, a) {',
          'if egressAddrRefused(ip) || false && slices.Contains(local, a) {')],
  tests=[(DAEMON, 'TestEgressConnectRefusesSpecialAddresses')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-1c', group='CN-4', repo='ANet',
  desc='特殊地址表少了 169.254.0.0/16(云元数据服务)',
  edits=[('internal/daemon/hostname.go', '"127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",', '"127.0.0.0/8", "172.16.0.0/12",')],
  tests=[(DAEMON, 'TestEgressSpecialAddresses|TestEgressAddressProperty|TestEgressConnectRefusesSpecialAddresses')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-1d', group='CN-4', repo='ANet',
  desc='IPv4 映射地址不按内嵌的 IPv4 判断',
  edits=[('internal/daemon/hostname.go', '\tif a.Is4In6() {\n\t\ta = a.Unmap()\n\t}\n', '')],
  tests=[(DAEMON, 'TestEgressSpecialAddresses|TestEgressAddressProperty')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-2', group='CN-4', repo='ANet',
  desc='拨号时重新解析主机名(DNS rebinding:第二次解析答内网地址)',
  edits=[('internal/daemon/egress_connect.go', '\tup, err := e.dialTunnel(ctx, addrs)\n',
          '\tagain, _ := e.net.lookup(ctx, host)\n\taddrs = addrs[:0]\n\tfor _, ip := range again {\n'
          '\t\taddrs = append(addrs, netip.AddrPortFrom(ip, 443))\n\t}\n\tup, err := e.dialTunnel(ctx, addrs)\n')],
  tests=[(DAEMON, 'TestEgressConnectDialsTheAddressChecked')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-3', group='CN-5', repo='ANet',
  desc='跳过 SNI 核对(TLS 主机名可以与 CONNECT 的主机不同)',
  edits=[('internal/daemon/clienthello.go', '\tcase info.sni == "" || !asciiOnly(info.sni) || asciiLower(info.sni) != host:',
          '\tcase false && (info.sni == "" || !asciiOnly(info.sni) || asciiLower(info.sni) != host):')],
  tests=[(DAEMON, 'TestEgressConnectChecksTheTLSName|TestClientHelloVerdicts|TestClientHelloFromCryptoTLS')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-4', group='CN-5', repo='ANet',
  desc='放行 Encrypted Client Hello',
  edits=[('internal/daemon/clienthello.go', '\tcase info.ech:\n', '\tcase false && info.ech:\n')],
  tests=[(DAEMON, 'TestEgressConnectChecksTheTLSName|TestClientHelloVerdicts|TestClientHelloFromCryptoTLS')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-5', group='CN-3', repo='ANet',
  desc='CONNECT 读取 daemon 环境里的代理变量并经它串联',
  edits=[
   ('internal/daemon/egress_connect.go', '\treturn d.DialContext(ctx, "tcp", addr.String())\n',
    '\tfor _, k := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {\n'
    '\t\tif p, err := url.Parse(os.Getenv(k)); err == nil && p.Host != "" {\n'
    '\t\t\treturn (&net.Dialer{}).DialContext(ctx, "tcp", p.Host)\n\t\t}\n\t}\n'
    '\treturn d.DialContext(ctx, "tcp", addr.String())\n'),
   ('internal/daemon/egress_connect.go', '\t"net/netip"\n\t"slices"\n', '\t"net/netip"\n\t"net/url"\n\t"os"\n\t"slices"\n'),
  ],
  tests=[(DAEMON, 'TestEgressConnectUsesNoProxy')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-6', group='CN-2', repo='ANet',
  desc='放开 443 以外的端口',
  edits=[('internal/daemon/egress_connect.go', '\tif u.Port() != egressConnectPort {', '\tif false && u.Port() != egressConnectPort {')],
  tests=[(DAEMON, 'TestEgressConnectRefusesWhatIsNotAllowed')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-7', group='CN-2', repo='ANet',
  desc='后缀匹配代替精确匹配(evil.pypi.org 当作 pypi.org)',
  edits=[
   ('internal/daemon/egress_connect.go', '\tif why != "" || !e.up.ConnectAllow[host] {',
    '\tsuffix := false\n\tfor h := range e.up.ConnectAllow {\n\t\tsuffix = suffix || strings.HasSuffix(host, "."+h)\n\t}\n'
    '\tif why != "" || !e.up.ConnectAllow[host] && !suffix {'),
   ('internal/daemon/egress_connect.go', '\t"slices"\n\t"sync"\n', '\t"slices"\n\t"strings"\n\t"sync"\n'),
  ],
  tests=[(DAEMON, 'TestEgressConnectRefusesWhatIsNotAllowed')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-8', group='CN-1', repo='ANet',
  desc='egress_allow 接受模型上游主机(内置的与 api_base 的)',
  edits=[('internal/daemon/hostname.go', '\t\tif model[h] {\n\t\t\treturn nil, a2ashape.Refuse("egress_allow_model_host"',
          '\t\tif false && model[h] {\n\t\t\treturn nil, a2ashape.Refuse("egress_allow_model_host"')],
  tests=[(DAEMON, 'TestEgressAllowList|TestAutoReplyEgressAllowRoute|TestEgressAllowIsCheckedAtStart'),
         ('ANet:./cmd/anet', 'TestEgressAllowIsCheckedFirst')],
  full=[DAEMON, 'ANet:./cmd/anet'],
 ),
 dict(
  id='m-cn-8b', group='CN-1', repo='ANet',
  desc='运行时的放行表不再剔除模型上游主机',
  edits=[('internal/daemon/hostname.go', 'why == "" && !model[h] && len(set) < egressAllowMax {',
          'why == "" && (true || !model[h]) && len(set) < egressAllowMax {')],
  tests=[(DAEMON, 'TestEgressAllowSet')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-tty', group='CN-8', repo='ANet',
  desc='增加放行主机不要求终端确认',
  edits=[('cmd/anet/egress_allow.go', '\t\tif len(added) > 0 {\n\t\t\terr := ttyConfirm(', '\t\tif false && len(added) > 0 {\n\t\t\terr := ttyConfirm(')],
  tests=[('ANet:./cmd/anet', 'TestEgressAllowNeedsATerminalToAdd')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='m-eg-7', group='EG-4', repo='ANet',
  desc='不限 CONNECT 隧道的并发数与累计数',
  edits=[('internal/daemon/egress_connect.go', '\tif t.active >= egressMaxTunnels || t.total >= egressMaxTunnelsTotal {',
          '\tif false && (t.active >= egressMaxTunnels || t.total >= egressMaxTunnelsTotal) {')],
  tests=[(DAEMON, 'TestEgressConnectTunnelLimits')],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-8', group='EG-4', repo='ANet',
  desc='隧道字节超限时只断开当前隧道,不关闭本次运行的全部隧道',
  edits=[('internal/daemon/egress_connect.go', '\te.tunnels.closeAll(false)\n\treturn false\n', '\treturn false\n')],
  tests=[(DAEMON, 'TestEgressConnectByteLimits')],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-9', group='EG-1', repo='ANet',
  desc='出站端点关闭时不关闭 hijack 出来的隧道',
  edits=[('internal/daemon/egress.go', '\t\te.tunnels.closeAll(true)\n', '')],
  tests=[(DAEMON, 'TestEgressCloseClosesTunnels')],
  full=[DAEMON],
 ),
 dict(
  id='m-bp-media-url', group='BP-5', repo='ANet',
  desc='放行 URL 来源的媒体(模型服务方代为抓取,与 web_fetch 同类)',
  edits=[
   ('internal/daemon/egress_policy.go', '\tif r := anthropicMediaByURL(obj); r != nil {', '\tif r := anthropicMediaByURL(obj); false && r != nil {'),
   ('internal/daemon/egress_policy.go', '\tif r := responsesMediaByURL(obj); r != nil {', '\tif r := responsesMediaByURL(obj); false && r != nil {'),
   ('internal/daemon/egress_policy.go', '\tif r := chatMediaByURL(obj); r != nil {', '\tif r := chatMediaByURL(obj); false && r != nil {'),
  ],
  tests=[(DAEMON, INJECT)],
  full=[DAEMON],
 ),
 dict(
  id='m-bp-media-url-b', group='BP-5', repo='ANet',
  desc='任何 URL 都当作内联数据(只认 data: 的判断失效)',
  edits=[('internal/daemon/egress_policy.go', '\treturn len(u) >= 5 && strings.EqualFold(u[:5], "data:")', '\treturn len(u) >= 0')],
  tests=[(DAEMON, INJECT)],
  full=[DAEMON],
 ),
 dict(
  id='m-bp-thinking-small', group='BP-3', repo='ANet',
  desc='max_tokens ≤ 1024 时保留 thinking(没有合法的思考预算)',
  edits=[('internal/daemon/egress_policy.go', '\t\tdelete(obj, "thinking")\n', '\t\t_ = obj\n')],
  tests=[(DAEMON, INJECT)],
  full=[DAEMON],
 ),
 # EXE-3 review: the gaps an adversarial reading found (the tests were tightened to catch them)
 dict(
  id='m-bp-media-url-c', group='BP-5', repo='ANet',
  desc='chat 只看 image_url(放行 video_url、audio_url 与 URL 形式的 file_data,兼容服务会代为抓取)',
  edits=[
   ('internal/daemon/egress_policy.go', 'var chatMediaKeys = []string{"audio_url", "image_url", "video_url"}', 'var chatMediaKeys = []string{"image_url"}'),
   ('internal/daemon/egress_policy.go', 'ok && strings.Contains(d, ":") && !egressInlineData(d) {', 'ok && false && strings.Contains(d, ":") && !egressInlineData(d) {'),
  ],
  tests=[(DAEMON, INJECT)],
  full=[DAEMON],
 ),
 dict(
  id='m-bp-media-url-d', group='BP-5', repo='ANet',
  desc='chat 只在 type 与成员同名的部分里找媒体 URL(兼容服务只凭成员名就认出媒体)',
  edits=[('internal/daemon/egress_policy.go', '\t\t\tif !presentMember(m, k) {\n', '\t\t\tif typ, _ := m["type"].(string); typ != k || !presentMember(m, k) {\n')],
  tests=[(DAEMON, INJECT)],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-8b', group='EG-4', repo='ANet',
  desc='隧道字节先转发后计量(超额的那一块仍被转发)',
  edits=[('internal/daemon/egress_connect.go',
          '\t\t\tif !e.carry(up, n) {\n\t\t\t\treturn\n\t\t\t}\n\t\t\tif _, werr := dst.Write(buf[:n]); werr != nil {\n\t\t\t\treturn\n\t\t\t}\n',
          '\t\t\tif _, werr := dst.Write(buf[:n]); werr != nil {\n\t\t\t\treturn\n\t\t\t}\n\t\t\tif !e.carry(up, n) {\n\t\t\t\treturn\n\t\t\t}\n')],
  tests=[(DAEMON, 'TestEgressConnectByteLimits')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-1e', group='CN-4', repo='ANet',
  desc='解析结果不先去掉 IPv4 映射就与本机地址比较(::ffff: 形式的本机地址被放行)',
  edits=[('internal/daemon/egress_connect.go', '\t\ta := ip.Unmap().WithZone("")\n', '\t\ta := ip\n')],
  tests=[(DAEMON, 'TestEgressConnectRefusesSpecialAddresses')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-5b', group='CN-5', repo='ANet',
  desc='ClientHello 读取不限单条记录的长度(可读入远超 16 KiB)',
  edits=[('internal/daemon/clienthello.go', ' || n > tlsMaxFragment || len(raw)+n > clientHelloMax {', ' {')],
  tests=[(DAEMON, 'TestClientHelloVerdicts')],
  full=[DAEMON],
 ),
 # EXE-3 second review: checks that held without a test noticing their loss (the tests were added to catch them)
 dict(
  id='m-cn-key', group='LEC 2.3.4', repo='ANet',
  desc='/autoreply 的应答带出已存的 api_key(只改 sandbox 的请求合并了当前配置)',
  edits=[('internal/daemon/control_api.go', '\t\tc := *cfg\n\t\tc.APIKey = ""\n', '\t\tc := *cfg\n')],
  tests=[(DAEMON, 'TestAutoReplyAnswerHasNoKey')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-4b', group='CN-4', repo='ANet',
  desc='按相对名解析放行主机(解析器的 search 域可能把它变成内部名字)',
  edits=[('internal/daemon/egress_connect.go', '\tips, err := e.net.lookup(rctx, host+".")\n', '\tips, err := e.net.lookup(rctx, host)\n')],
  tests=[(DAEMON, 'TestEgressConnectTunnel')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-4c', group='CN-4', repo='ANet',
  desc='解析不限 5 秒',
  edits=[('internal/daemon/egress_connect.go', '\trctx, cancel := context.WithTimeout(ctx, e.connectWait.resolve)\n',
          '\trctx, cancel := context.WithCancel(ctx)\n')],
  tests=[(DAEMON, 'TestEgressConnectTimes')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-1f', group='CN-4', repo='ANet',
  desc='生产环境的本机地址表为空(解析到本机网卡地址的主机被放行)',
  edits=[('internal/daemon/egress_connect.go', 'dial: egressDialChecked, local: egressLocalAddrs}',
          'dial: egressDialChecked, local: func() []netip.Addr { return nil }}')],
  tests=[(DAEMON, 'TestEgressLocalAddrs')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-5c', group='CN-5', repo='ANet',
  desc='SNI 按 Unicode 折叠比较(Kelvin 符号当作 k)',
  edits=[('internal/daemon/clienthello.go', '\tcase info.sni == "" || !asciiOnly(info.sni) || asciiLower(info.sni) != host:',
          '\tcase info.sni == "" || !strings.EqualFold(info.sni, host):'),
         ('internal/daemon/clienthello.go', 'import (\n\t"io"\n)', 'import (\n\t"io"\n\t"strings"\n)')],
  tests=[(DAEMON, 'TestClientHelloVerdicts')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-5d', group='CN-5', repo='ANet',
  desc='读 ClientHello 不限 10 秒(不发 hello 的隧道一直占着名额)',
  edits=[('internal/daemon/egress_connect.go', '\t_ = agent.SetReadDeadline(time.Now().Add(e.connectWait.hello))\n', '')],
  tests=[(DAEMON, 'TestEgressConnectTimes')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-7b', group='CN-7', repo='ANet',
  desc='转发的 ClientHello 不计入隧道上行字节',
  edits=[('internal/daemon/egress_connect.go', '\tif !e.carry(true, len(hello)) {\n\t\treturn\n\t}\n', '')],
  tests=[(DAEMON, 'TestEgressConnectByteLimits')],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-8c', group='EG-4', repo='ANet',
  desc='上行额度用尽后不再记住(之后放得下的小块又被转发)',
  edits=[('internal/daemon/egress.go', '\t\tif s.upSpent || s.connUp+int64(n) > egressMaxTunnelUpload {',
          '\t\tif s.connUp+int64(n) > egressMaxTunnelUpload {')],
  tests=[(DAEMON, 'TestEgressConnectByteLimits')],
  full=[DAEMON],
 ),
 dict(
  id='m-eg-10', group='EG-4', repo='ANet',
  desc='agent 停止发送时直接关闭上游连接(不传半关闭,上游的回答丢失)',
  edits=[('internal/daemon/egress_connect.go', '\t\tif cw, ok := up.(interface{ CloseWrite() error }); ok {',
          '\t\tif cw, ok := up.(interface{ CloseWrite() error }); false && ok {')],
  tests=[(DAEMON, 'TestEgressConnectPassesHalfClose')],
  full=[DAEMON],
 ),
 dict(
  id='m-cn-8c', group='CN-1', repo='ANet',
  desc='运行时的放行表不限 64 项',
  edits=[('internal/daemon/hostname.go', '!model[h] && len(set) < egressAllowMax {', '!model[h] {')],
  tests=[(DAEMON, 'TestEgressAllowSet')],
  full=[DAEMON],
 ),
]

# EXE-10: who is on the other end of a Unix socket (LEC §4.3.11 PC-1 to PC-4, §7.3 M-PC-1 to M-PC-3, J10), the
# p2p session's write deadline, and the tests that no longer depend on the machine's load (the retry clock of
# module/a2a, the still daemon clock of the direct rate limit). The cases with a second account (root listening,
# another user connecting) run where peercredtest can use sudo: the CI peercred job; elsewhere the tests that stand
# in for the other account carry the kill.
BACKENDCONN = 'ANet:./internal/backendconn'
MUTS += [
 dict(
  id='m-pc-1', group='PC-2', repo='ANet',
  desc='读不到监听者账户(ErrUnsupported)时放行',
  edits=[('internal/backendconn/backendconn.go', '\t\treturn refusePeerCredUnsupported(sock)', '\t\treturn nil')],
  tests=[(BACKENDCONN, 'TestAPlatformThatCannotTellIsRefusedUnlessPathOnly')],
  full=[BACKENDCONN],
 ),
 dict(
  id='m-pc-2', group='PC-3', repo='ANet',
  desc='module/p2p 直接拨 anetpeer 的 socket,不经 backendconn',
  edits=[('module/p2p/transport.go', '\tc, err := rules.DialUnix(ctx, nil, t.socket)', '\t_ = rules\n\tc, err := (&net.Dialer{}).DialContext(ctx, "unix", t.socket)')],
  tests=[(BACKENDCONN, 'TestNothingDialsAUnixSocketAroundBackendconn'), ('ANet:./module/p2p', 'TestAPeerProcessOfAnotherAccountIsSentNothing')],
  full=['ANet:./module/p2p'],
 ),
 dict(
  id='m-pc-3a', group='§4.4', repo='ANet',
  desc='backendconn.Policy 恢复 expected_uid',
  edits=[('internal/backendconn/backendconn.go', '\tExpectedUser string `json:"expected_user,omitempty"`',
          '\tExpectedUser string `json:"expected_user,omitempty"`\n\tExpectedUID  *int   `json:"expected_uid,omitempty"`')],
  tests=[(BACKENDCONN, 'TestPolicyKeysAreTheSameForEveryModule')],
  full=[BACKENDCONN],
 ),
 dict(
  id='m-pc-3b', group='§3.1', repo='ANet',
  desc='只在 modules.anetlink 拒绝 expected_uid(service、a2a 的删除键行去掉)',
  edits=[('internal/daemon/config.go', '\t{"modules.service.expected_uid", "expected_uid"},\n\t{"modules.a2a.backends[].expected_uid", "expected_uid"},\n', '')],
  tests=[(DAEMON, 'TestExpectedUIDStopsTheDaemonInEveryModule'), ('ANet:./cmd/anet', 'TestRemovedKeysStopAnetDaemon')],
  full=[DAEMON],
 ),
 dict(
  id='exe10-path-only', group='PC-2', repo='ANet',
  desc='peer_check path-only 不起作用(仍问监听者账户)',
  edits=[('internal/backendconn/backendconn.go', '\tif r.pathOnly {\n\t\treturn nil\n\t}\n\tcred, err := peerCredOf(c)', '\tcred, err := peerCredOf(c)')],
  tests=[(BACKENDCONN, 'TestAPlatformThatCannotTellIsRefusedUnlessPathOnly')],
  full=[BACKENDCONN],
 ),
 dict(
  id='exe10-numeric-uid', group='§3.2', repo='ANet',
  desc='expected_user 的十进制数字仍按用户名查找(/etc/passwd 里没有的 uid 不合格)',
  edits=[('internal/backendconn/backendconn.go', '\t\tid, numeric, err := decimalID(name)\n\t\tif !numeric {', '\t\tid, numeric, err := decimalID(name)\n\t\tif true || !numeric {')],
  tests=[(BACKENDCONN, 'TestResolve'), (DAEMON, 'TestBackendPolicyKeysAreCheckedAtStart')],
  full=[BACKENDCONN],
 ),
 dict(
  id='exe10-config-check', group='§3.2', repo='ANet',
  desc='启动时不检查后端的 expected_user/socket_group/peer_check(不报 config_invalid)',
  edits=[('internal/daemon/backend_policy.go', 'func init() { configChecks = append(configChecks, checkBackendPolicies) }', 'func init() {}')],
  tests=[(DAEMON, 'TestBackendPolicyKeysAreCheckedAtStart')],
  full=[DAEMON],
 ),
 dict(
  id='exe10-doctor-path-only', group='§5.5', repo='ANet',
  desc='doctor 对 path-only 的后端报 ok(不报 backends.path_only)',
  edits=[('cmd/anet/doctor_backends.go', '\tcase rules.PathOnly():', '\tcase false && rules.PathOnly():')],
  tests=[('ANet:./cmd/anet', 'TestDoctorBackendsPeerCheck')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='exe10-write-deadline', group='PC-3', repo='ANet',
  desc='p2p 会话写没有期限(anetpeer 停读时 Send 一直卡住)',
  edits=[('module/p2p/transport.go', '\t_ = c.SetWriteDeadline(time.Now().Add(sessionWriteTimeout))\n', '')],
  tests=[('ANet:./module/p2p', 'TestSessionWriteHasADeadline')],
  full=['ANet:./module/p2p'],
 ),
 dict(
  id='exe10-anetpeer-admit', group='PC-4', repo='ANet',
  desc='anetpeer 的 daemon 套接字不核实连接方账户,任何新连接顶替当前 daemon',
  edits=[('tools/anetpeer/main.go', '\t\t\tif err := p.admitDaemon(c); err != nil {', '\t\t\tif err := p.admitDaemon(c); false && err != nil {')],
  tests=[('ANet:./tools/anetpeer', 'TestAConnectionOfAnotherAccountDoesNotReplaceTheDaemon')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='exe10-anetpeer-stale', group='PC-4', repo='ANet',
  desc='anetpeer 无条件删除 daemon 套接字路径上的文件(抢走有人监听的 socket)',
  edits=[('tools/anetpeer/main.go', '\tif err := clearStaleSocket(sock); err != nil {\n\t\treturn nil, err\n\t}', '\t_ = os.Remove(sock)')],
  tests=[('ANet:./tools/anetpeer', 'TestTheDaemonSocketIsListenedOnSafely')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='exe10-anetpeer-path', group='PC-4', repo='ANet',
  desc='anetpeer 在他人可写的目录里也监听 daemon 套接字',
  edits=[('tools/anetpeer/main.go', '\t\t_, _, err = rules.CheckPath(sock)', '\t\t_ = rules')],
  tests=[('ANet:./tools/anetpeer', 'TestTheDaemonSocketIsListenedOnSafely')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='exe10-peercred-error', group='PC-2', repo='ANet',
  desc='读监听者账户出错(ErrUnsupported 以外)时放行',
  edits=[('internal/backendconn/backendconn.go', '\t\treturn refuse(sock, "cannot read who listens on it: %v", err)', '\t\treturn nil')],
  tests=[(BACKENDCONN, 'TestAListenerWhoseAccountCannotBeReadIsRefused')],
  full=[BACKENDCONN],
 ),
 dict(
  id='exe10-anetpeer-probe', group='PC-4', repo='ANet',
  desc='daemon 套接字的连接一被接受就顶替当前 daemon(另一 anetpeer 探测 socket 是否在用后,当前 daemon 收不到投递)',
  edits=[('tools/anetpeer/main.go', '\t\tif first && err == nil {', '\t\tif first {')],
  tests=[('ANet:./tools/anetpeer', 'TestTheDaemonSocketIsListenedOnSafely')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='exe10-retry-giveup', group='load', repo='ANet',
  desc='重试放弃时刻算错(放宽到两倍 give_up_after)',
  edits=[('module/a2a/backend.go', 'since+wait > f.retry.giveUp {', 'since+wait > 2*f.retry.giveUp {')],
  tests=[('ANet:./module/a2a', 'TestRetriesGiveUpAndAnswersAreNotRetried')],
  full=['ANet:./module/a2a'],
 ),
 dict(
  id='exe10-anetpeer-root', group='PC-4', repo='ANet',
  desc='anetpeer 的 daemon 套接字放行 root 的连接(root 顶替当前 daemon)',
  edits=[('tools/anetpeer/main.go', '\tcase cred.UID != os.Getuid():', '\tcase cred.UID != os.Getuid() && cred.UID != 0:')],
  tests=[('ANet:./tools/anetpeer', 'TestAConnectionOfAnotherAccountDoesNotReplaceTheDaemon')],
  full=['ANet:./tools/anetpeer'],
 ),
 dict(
  id='exe10-anetpeer-relative', group='PC-4', repo='ANet',
  desc='anetpeer 不把相对的 --socket 换成绝对路径(包注释里的用法启动即被拒)',
  edits=[('tools/anetpeer/main.go', '\tsock = abs\n', '\t_ = abs\n')],
  tests=[('ANet:./tools/anetpeer', 'TestARelativeDaemonSocketIsTheAbsolutePathItNames')],
  full=['ANet:./tools/anetpeer'],
 ),
]

# EXE-12: the C1 v1 client (LEC §4.3.12 CC-1 to CC-7, §7.3 M-CC-1 to M-CC-3): the caller it tells ANetLink of, the
# socket checks it dials through, the refusals it maps, what the daemon publishes and how a refusal ends the task, the
# start checks of modules.anetlink and doctor's anetlink.* findings. m-cc-3b maps an unknown door to the retired via 2:
# the request's read-back through C1's entrance refuses that by itself, so the mutation removes it as well.
ANETLINK = 'ANet:./provider/anetlink'
MUTS += [
 dict(
  id='m-cc-1', group='CC-2', repo='ANet',
  desc='C1 客户端改回发送 call.CallerAID(不经 VerifiedCaller)',
  edits=[('provider/anetlink/anetlink.go', '\t\taid := call.VerifiedCaller()', '\t\taid := call.CallerAID')],
  tests=[(ANETLINK, 'TestTheClientNeverReadsCallerAID')],
  full=[ANETLINK],
 ),
 dict(
  id='m-cc-2', group='CC-1', repo='ANet',
  desc='C1 客户端绕过 backendconn,直接拨 c1.sock(不核实路径与监听者)',
  edits=[
   ('provider/anetlink/anetlink.go', '\treturn &Provider{id: id, sock: socketPath, cli: rules.Client(t, clientTimeout)}, nil',
    '\t_ = rules\n\treturn &Provider{id: id, sock: socketPath, cli: &http.Client{Timeout: clientTimeout, Transport: &http.Transport{'
    'DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {\n'
    '\t\treturn (&net.Dialer{}).DialContext(ctx, "unix", socketPath)\n\t}}}}, nil'),
   ('provider/anetlink/anetlink.go', '\t"mime"\n', '\t"mime"\n\t"net"\n'),
  ],
  tests=[(ANETLINK, 'TestAnImpostorListenerIsSentNothing'), (BACKENDCONN, 'TestNothingDialsAUnixSocketAroundBackendconn')],
  full=[ANETLINK],
 ),
 dict(
  id='m-cc-3', group='CC-2', repo='ANet',
  desc='未知 Via(含 voucher)映射为 via = 0 并拨号',
  edits=[('provider/anetlink/anetlink.go',
          '\treturn caller{}, fmt.Errorf("a call through %q cannot cross C1 (only a local surface or the relay can); not sent", call.Via)',
          '\tvia := viaLocal\n\treturn caller{Via: &via}, nil')],
  tests=[(ANETLINK, 'TestOtherDoorsAreNotDialled')],
  full=[ANETLINK],
 ),
 dict(
  id='m-cc-3b', group='CC-2', repo='ANet',
  desc='未知 Via 映射为退役的 via = 2,且发送前不再按入口规则回读请求',
  edits=[
   ('provider/anetlink/anetlink.go',
    '\treturn caller{}, fmt.Errorf("a call through %q cannot cross C1 (only a local surface or the relay can); not sent", call.Via)',
    '\tvia := uint64(2)\n\treturn caller{Via: &via}, nil'),
   ('provider/anetlink/wire.go', '\tif _, err := decodeInvokeReq(b); err != nil {\n\t\treturn nil, fmt.Errorf("the request would be refused: %w", err)',
    '\tif _, err := decodeInvokeReq(b); false && err != nil {\n\t\treturn nil, fmt.Errorf("the request would be refused: %w", err)'),
  ],
  tests=[(ANETLINK, 'TestOtherDoorsAreNotDialled')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-c1-reason', group='CC-4', repo='ANet',
  desc='C1 拒绝码原样交给 daemon(不映射为用户码)',
  edits=[('provider/anetlink/anetlink.go', 'Reason: a2ashape.C1Reason(ref.Reason),', 'Reason: ref.Reason,')],
  tests=[(ANETLINK, 'TestRefusalsBecomeTheUsersReasons')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-unsent', group='CC-4', repo='ANet',
  desc='没有发出的调用(连不上或监听者不符)按 backend_failed 失败,而不是 unavailable 拒绝',
  edits=[('provider/anetlink/anetlink.go', '\t\treturn effect.Effect{}, &provider.RefusalError{Reason: reasonUnavailable,',
          '\t\treturn effect.Effect{}, &provider.RefusalError{Reason: reasonBackendFailed,')],
  tests=[(ANETLINK, 'TestNothingThereIsUnavailable|TestAnImpostorListenerIsSentNothing')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-refusal-outcome', group='CC-4', repo='ANet',
  desc='daemon 不按 provider 的拒绝原因结束任务(按普通错误记 FAILED)',
  edits=[('internal/daemon/capability.go', '\tcase errors.As(err, &refused):', '\tcase false && errors.As(err, &refused):')],
  tests=[(DAEMON, 'TestAProviderRefusalEndsTheTaskWithItsReason')],
  full=[DAEMON],
 ),
 dict(
  id='exe12-publish', group='CC-5', repo='ANet',
  desc='hub 注册的 caps 不过滤不接受远端调用的公开能力',
  edits=[('internal/daemon/published_caps.go', 'public[id] && !remote.refused(id) {', 'public[id] {')],
  tests=[(DAEMON, 'TestOnlyCapabilitiesRemoteCallersMayUseArePublished')],
  full=[DAEMON],
 ),
 dict(
  id='exe12-card', group='CC-5', repo='ANet',
  desc='卡片的 skills 不过滤不接受远端调用的公开能力',
  edits=[('internal/daemon/a2a_card.go', '\t\tif remote.refused(id) {', '\t\tif false && remote.refused(id) {')],
  tests=[(DAEMON, 'TestOnlyCapabilitiesRemoteCallersMayUseArePublished')],
  full=[DAEMON],
 ),
 dict(
  id='exe12-remote-flag', group='CC-5', repo='ANet',
  desc='remote 取 1 以外的非零值也当作接受远端调用',
  edits=[('provider/anetlink/anetlink.go', '\t\tout[c.ID] = *c.Remote == viaRelay', '\t\tout[c.ID] = *c.Remote != 0')],
  tests=[(ANETLINK, 'TestRemoteAllowedIsTheRemoteFlag')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-strict', group='§2.1.1', repo='ANet',
  desc='C1 请求入口改用非严格解码(未知键、退役键 4、Caller 键 3 被静默丢弃)',
  edits=[('provider/anetlink/wire.go', '\tif err := coredet.UnmarshalStrict(b, &req); err != nil {', '\tif err := coredet.Unmarshal(b, &req); err != nil {')],
  tests=[(ANETLINK, 'TestC1GoldenNegative')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-via-aid', group='§2.1.1', repo='ANet',
  desc='via = 0 带 aid 不拒收',
  edits=[('provider/anetlink/wire.go', '\t\tif c.AID != nil {\n\t\t\treturn invokeReq{}, errors.New("caller.via is 0 (local) but carries an aid")',
          '\t\tif false && c.AID != nil {\n\t\t\treturn invokeReq{}, errors.New("caller.via is 0 (local) but carries an aid")')],
  tests=[(ANETLINK, 'TestC1GoldenNegative')],
  full=[ANETLINK],
 ),
 dict(
  id='exe12-allow-tcp', group='§3.2', repo='ANet',
  desc='启动时不检查 modules.anetlink(allow_tcp: true 与缺 socket 都不报 config_invalid)',
  edits=[('internal/daemon/anetlink_config.go', 'func init() { configChecks = append(configChecks, checkANetLinkConfig) }', 'func init() {}')],
  tests=[(DAEMON, 'TestTheANetLinkBlockIsCheckedAtStart')],
  full=[DAEMON],
 ),
 dict(
  id='exe12-doctor-version', group='§5.5', repo='ANet',
  desc='doctor anetlink.socket 不把 C1 版本不一致报为 c1_version',
  edits=[('cmd/anet/doctor_anetlink.go', '\tcase linkprov.NotC1v1(err):\n\t\tsay(id, stFail, "c1_version", nil)\n\t\treturn\n', '')],
  tests=[('ANet:./cmd/anet', 'TestDoctorANetLinkSocketFailures')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='exe12-doctor-publish', group='§5.5', repo='ANet',
  desc='doctor 不报 anetlink.publish(公开却不接受远端调用的能力)',
  edits=[('cmd/anet/doctor_anetlink.go', '\tfor _, cid := range refused {\n\t\tsay(', '\tfor _, cid := range refused[:0] {\n\t\tsay(')],
  tests=[('ANet:./cmd/anet', 'TestDoctorANetLinkSocketAndPublish')],
  full=['ANet:./cmd/anet'],
 ),
 dict(
  id='exe12-registry', group='CORE-S3', repo='ANet',
  desc='C1 结构体的 caller 用退役的键 4(登记表扫描应报错,金标也变红)',
  edits=[('provider/anetlink/wire.go', '\tCaller     *caller        `cbor:"5,keyasint,omitempty"`', '\tCaller     *caller        `cbor:"4,keyasint,omitempty"`')],
  tests=[(DAEMON, 'TestWireRegistry')],
  full=[ANETLINK],
 ),
]
