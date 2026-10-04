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
  id='m-up-1', group='UP', repo='ANet',
  desc='api_base 主机名末尾的点不归一(openrouter.ai. 绕过 plugins 与 models 的拒绝)',
  edits=[
   ('internal/daemon/egress_upstreams.go', 'host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")', 'host := strings.ToLower(base.Hostname())'),
  ],
  tests=[('ANet:./internal/daemon', 'TestEgressUpstreamSelection')],
  full=['ANet:./internal/daemon'],
 ),
]
