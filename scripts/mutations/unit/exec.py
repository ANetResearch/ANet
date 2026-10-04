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
]
