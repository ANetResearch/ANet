# 0018 · 互通:Hermes A2A 插件

日期:2026-09-27。对象:hermes-agent `plugins/platforms/a2a`(插件目录最后一次提交 b1bb9031),
读过 `DESIGN.md`、`README.md`、`__init__.py`、`tools.py`、`protocol.py`、`adapter.py`、`security.py`。
依据:设计 §11(本机 A2A 接口)、§11.6(提供侧后端)、§13.1(`agents wire`)、§17 契约行;计划 0014 的 B5-03 与 U1。
配套测试:`internal/a2ashape/hermes_contract_test.go`,Hermes 原代码输出记录在 `internal/a2ashape/testdata/hermes_a2a_call.json`。

## 1. 结论

- **请求侧吻合。** Hermes 取卡片时带 Bearer,从代理卡片里取 JSONRPC 接口 URL,方法名用 v1.0 的 `SendMessage`,
  请求头带 `A2A-Version: 1.0`,消息里不带 `configuration`。按规范这就是阻塞调用。消息里有 contextId、没有 taskId,
  a2a-go 能读,投影类型也能读回(`TestHermesRequestShape`)。
- **答复提取:21 种投影形状里,16 种能取到合适的文本;1 种(canceled 且没有任何消息)取不到文本,但头部已写明状态;
  另外 4 种有缺口**(`TestHermesContract`,详见 §3):
  两种形状 Hermes 会把 `anet.receipt` 回执的 JSON 当成答复,一种形状取到空文本,还有一种只取到
  "Payment is required.",没有金额。投影应做的调整见 §4(按任务要求,这里只报告,不改投影)。
- **移植与 Hermes 一致。** Go 移植版在 40 个响应上的输出与 Hermes 原代码逐字相同(`TestHermesPortMatchesHermes`),
  这 40 个响应是 21 个投影响应加 19 个边界响应。改动移植版中的一行(例如把 `pyStrip` 换成 `strings.TrimSpace`),
  这个测试就会失败。
- **行为差异**(详见 §5):
  - 在 input-required 之后,Hermes 只带 contextId 继续对话,anet 按规范会新建任务;
  - 阻塞调用超时后,Hermes 的报错里没有 contextId,模型重试会建第二个任务;
  - `a2a_discover` 取卡片时不带令牌,会得到 401;
  - Hermes 只读文本,读不到 metadata,SI-6 的两个键对 Hermes 的模型不可见。
- **Hermes 作为提供侧后端**(§6):a2a-go 能读 Hermes 服务端的全部答复;但 Hermes 配了令牌之后,它的卡片用的是
  pre-1.0 的安全方案形状,a2a-go v2 拒收(`TestHermesAsBackend`),所以后端应直接 POST 到配置的 URL,不要解析卡片。
  另外,Hermes 按 contextId 选会话,后端必须把 contextId 按对端 AID 隔开,见 §6.3。

## 2. Hermes 客户端做什么(源码要点)

### 2.1 `a2a_agents` 配置

`tools._peer_from_entry` / `_resolve_peer` 只读下面这些字段:

```yaml
a2a_agents:
  <名字>:                      # a2a_call(agent=<名字>) 的参数;也可以直接给 http(s) URL(此时无令牌、timeout 120)
    url: "http://…"            # 基址;卡片取 <url>/.well-known/agent-card.json
    auth: {type: bearer, token: "…"}   # 只认 type=bearer 且 token 非空
    timeout: 120               # int;缺省 120 秒
    capabilities: [a, b]       # 只供 a2a_orchestrate 按能力匹配
    tenant: "…"                # 可选;卡片接口带 tenant 时以卡片为准
```

`a2a_agents` 非空时,`a2a` 工具集的五个工具才会注册(`_a2a_tools_available`)。只写 `mcp_servers.anet`
不会让模型看到这些工具。

### 2.2 一次 `a2a_call` 在线上做了什么

1. **取卡片**:`GET <url>/.well-known/agent-card.json`,只带 `Authorization`,不带 `A2A-Version`;
   超时取 `min(timeout, 30)`;404 时回退到 `/.well-known/agent.json`;出现任何异常都当作没有卡片。
2. **选 RPC URL**:依次取卡片 `supportedInterfaces` 中第一个 `protocolBinding=="JSONRPC"` 且带 url 的接口、
   卡片顶层的 `url`,都没有就用基址本身。
3. **发请求**:`POST`,请求头为 `Content-Type: application/json`、`A2A-Version: 1.0`、`Authorization: Bearer …`,正文:
   ```json
   {"jsonrpc":"2.0","id":"task-<16hex>","method":"SendMessage",
    "params":{"message":{"role":"ROLE_USER","parts":[{"text":"…","mediaType":"text/plain"}],
                         "messageId":"<uuid4 hex>","contextId":"<ctx>"}}}
   ```
   卡片接口或条目给了 tenant 时,再加 `params.tenant`。超时用条目的 `timeout`,它是 urllib 的 socket 超时,
   按读空闲计时,不是总时长。
4. **阻塞,不轮询**:请求不带 `configuration`,按规范就是 `return_immediately=false`,服务端等到终态或中断态才返回。
   Hermes 不调用 GetTask、ListTasks、CancelTask、流式或推送,**一次调用就只有一个结果**,没有重试。
5. **contextId**:调用方没给时,本地铸造 `ctx-<16hex>`。发送前,正文先经 `redact_outbound` 删去凭据形字符串,
   邮箱改成 `[redacted-email]`;然后在 POST 之前写审计日志 `~/.hermes/a2a_audit.jsonl` 和会话文件
   `~/.hermes/a2a_conversations/<ctx>.jsonl`。
6. **错误**:
   - HTTP 401/403 →"rejected auth … Check the configured token.";
   - HTTP 429 →"rate limited us";
   - 其他非 2xx →"failed — HTTP <code>.";
   - JSON-RPC `error` →"Peer '<名字>' returned an error: <message>";
   - 其他异常,包括读超时 →"Error: call to '<名字>' failed — <e>."。

### 2.3 答复提取(`_reply_text_from_result` 的完整逻辑)

```
result = unwrap(result)            # {"task":…} / {"message":…} 取内层;旧式裸 Task 原样
if 不是 dict: return str(result)    # null → "None",list → Python repr
for artifact in result.artifacts or []:
    txt = extract_text(artifact)
    if txt: return txt             # 第一个有文字的 artifact 胜出,其余不看
return extract_text(result.status.message or result)
```

`extract_text(x)` 先取 `x.get("message", x)`,再逐个 part 处理,前一条规则命中就不看后面的:

| part | 渲染 |
|---|---|
| `text` 是字符串 | 原文,包括空串 |
| `url` 非空 | `[file: <filename 或 name>] <url> (<mediaType 或 mimeType>)` |
| v0.3 `file.fileWithUri` | 同上 |
| `raw` 是字符串 | `[file: <filename>] <len(base64 串)> bytes base64-encoded (<mediaType>)`(数的是 base64 串的长度,不是字节数) |
| `data` 不是 null | `[data (<mediaType 或 application/json>)]\n` + `json.dumps(data, ensure_ascii=False)` |

各段用 `\n` 连接,再做 Python 的 `strip()`。它**不读** metadata、history、`kind`,也不读 artifact 的 name/id。

`a2a_call` 交给模型的字符串是:

```
[<名字> · context <响应里的 contextId> · <state 去掉 TASK_STATE_、_ 换成 -、小写>]
<答复,或 "(no text reply)">
```

状态是 `TASK_STATE_INPUT_REQUIRED` 时,末尾再加一句
"(The peer needs more input — answer by calling a2a_call again with context_id '<ctx>'.)"。
v0.3 的 `input-required` 串不会触发这句提示。

### 2.4 其他工具

- `a2a_discover(url)`:取卡片时**不带令牌**。
- `a2a_list`:列出配置的对端,也列出 `a2a_conversations` 下已持久化的 contextId。
- `a2a_history`:读回持久化的会话。
- `a2a_orchestrate(capability, message, mode)`:按条目的 `capabilities` 匹配对端,`*` 匹配全部,最多 6 路并行;
  `first` 模式只取消尚未开始的调用,已经发出的调用不受影响。

## 3. 契约测试结果

方法如下。测试按 A2A 接口的实际做法投影任务:`Artifacts: true`、附件内联、`safeName`、带 provider KEL,
与 daemon `taskseam` 相同。投影先经 a2a-go 读写一遍(沿用 `contract`,同时检查 SI-6 与逐字节往返),
再包成 a2a-go JSON-RPC 服务端写出的响应 `{"jsonrpc","id","result":{"task":…}}`,最后用 Go 移植版读取。
fixture 的 contextId 采用 Hermes 的 contextId 格式 `ctx-<16hex>`,检查项包括:Hermes 从响应中取回的正是它发出的
contextId(C22)。

| 形状 | Hermes 的模型读到 | 评价 |
|---|---|---|
| 文本 completed(`anet.reply`) | 回复原文 | 好 |
| 文本 completed,回执 unverified | 回复原文 | 好;是否验证过看不到(见 §5.7) |
| 文本 completed,回复 = 文字 + 文件 | 只有文字 | 文件不可见(文件是后面的 artifact,见 §4 P1) |
| **文本 completed,回复只有文件** | **`anet.receipt` 的 JSON** | **缺口**:`anet.reply` 是空 text part,Hermes 跳过它,取了下一个 artifact |
| **文本 completed,provider 没说过话**(end_request 结束) | **`anet.receipt` 的 JSON** | **缺口**:没有 `anet.reply`,第一个 artifact 就是回执 |
| 文本 failed,有回执(provider 最后一句说明原因) | provider 的原话 | 好 |
| 文本 failed,无回执(`SetFailed` 细节) | 细节文本 | 好 |
| 文本 rejected(入站策略 / 沙箱不可用 status 行) | status 行文本 | 好 |
| 文本 canceled,无任何消息 | `(no text reply)`,头部是 canceled | 可接受 |
| 文本 input-required(追问) | 追问原文 + "needs more input" 提示 | 好;但照提示继续会建新任务(§5.1) |
| 文本 input-required,追问带图 | `like this?\n[file: safe-cat.png] 4 bytes base64-encoded (image/png)` | 可读;内容本身拿不到 |
| 文本 input-required,付款消息带正文 | 付款消息正文 | 好 |
| **能力 input-required,同任务流付款消息** | **`(no text reply)`** | **缺口**:能力调用的付款消息只存 metadata(`ingestMessage` 清空正文),status.message 只剩一个空 text part |
| 能力 input-required,报价答复带 message | `costs 5` | 好 |
| **能力 input-required,报价答复无 message** | **`Payment is required.`** | **缺口**:金额、资产、收款方只在 `x402.payment.required` 里 |
| 能力 completed OK / UNVERIFIED | `[data (application/json)]\n{"capability": "cas.put", …, "status": "UNVERIFIED", …}` | 可读;交付物 JSON 里的 `status` 让模型看得到效果状态 |
| 能力 failed / rejected(busy) | 交付物 JSON,内含 message | 可读;`anet.retry_after_ms` 看不到 |
| 能力 rejected(策略,无答复) | status 行文本 | 好 |

`working` 与 `submitted`(待批)不会被阻塞调用返回,所以不在表内。

移植版逐行对应以下 Hermes 函数:

- `protocol.unwrap_send_message_response`、`_file_note`、`_json_or_str`、`extract_text`;
- `tools._reply_text_from_result`;
- `_send_task` 在 POST 之后的部分,以及 `a2a_call` 的格式化与错误映射。

为了逐字复现 Python,移植版还实现了若干底层细节:

- `json.loads`:保留键序;重复键时保留首次位置、取最后一个值;区分 int 与 float;
- `json.dumps(ensure_ascii=False)`:分隔符为 `, ` 和 `: `;
- `float.__repr__`,以及 `str()`/`repr()`;
- Python 的 `strip()` 空白集合,比 Go 多 `\x1c`–`\x1f`;
- 真值判断与 `or` 的语义;
- Python 异常文本。

边界响应包括:

- 旧式裸 Task / v0.3 part、Message 结果、每种 part;
- 空 artifact 之后才出现文字、`result: null` / list;
- v0.3 的 `input-required`、重复键;
- `artifacts` 不是 list、`parts: null`(Hermes 抛异常,模型读到 "Error: … failed");
- JSON-RPC 错误、HTTP 401/429/404、读超时。

## 4. 投影调整建议(只报告,未改代码)

- **P1 回复的文件放进 `anet.reply`。** 回复有附件时,把 file part 放在同一个 `anet.reply` artifact 里,跟在
  text part 之后;正文为空时不放空 text part。这样"只有文件"和"文字 + 文件"两种形状下,Hermes 都能读到
  `[file: …]`,也不会退到回执。改动位置:`projector.artifacts` 的文本任务分支。
  顺带一提,内联附件对 Hermes 没用:它只报告 base64 串长度。按 Q12 限制内联量即可。
- **P2 回执不应被读成答复。** 优先方案:`anet.receipt` 不作为 artifact,改放到 task metadata(例如
  `anet.receipt` 键,内容与现在的 DataPart 相同)。理由:回执是证据,不是产出。凡是"取第一个或全部 artifact"的
  简单客户端(Hermes 就是这样),都会把它当成答复,流式的 artifactUpdate 也同样如此。
  这是 §11.5 "Task 表示"的改动,交 B6-02 决定。如果保留 artifact 形式,最小改法是:文本任务 completed
  而对话记录里没有 provider 发言时,仍然放一个 `anet.reply`,正文用合成的说明,例如
  "(provider 未给出文字答复即完成)"。但这偏离了 `anet.reply` 的定义("回执覆盖的对话记录中 provider 的最后一条")。
- **P3 付款的 status.message 必须有文字。**
  - (a) status.message 取自没有正文、也没有附件的存储行时(能力调用的付款行,或只有 metadata 的 status 行),
    合成一段 text part,不要输出空 text part。
  - (b) `paymentRequiredMessage` 以及付款行合成的文字,从 `x402.payment.required.accepts` 摘出金额、资产、
    收款方和网络,并说明不认识 x402 的客户端怎样处理:在支出档位内会自动付款;超出档位时,用 anet 的
    `submit_payment` 或 `anet pay`。
  - metadata 保持不变。a2a-x402 允许 payment-required 消息带 text part。
  - 改动位置:`projector.statusMessage` 的 InputRequired 分支、`paymentRequiredMessage`、`fromRow` 的空正文路径。
- **P4(低优先级)** failed/rejected/canceled 既没有消息、又有 `anet.reason` 时,合成一句
  "<state>: <reason>"。现在 canceled 显示 `(no text reply)`,头部已经写明 canceled,可以接受。

以下问题不属于投影,留给 module/a2a 和 agentwire:

- input-required 的续写(§5.1);
- 超时后的找回(§5.2);
- 卡片取不到时的回退路径(§5.10);
- `a2a_discover` 返回 401(§5.4)。

## 5. 已知差异与风险(Hermes 作为客户端)

1. **input-required 后的续写会建新任务。** 规范要求续写时带同一个 taskId。Hermes 只带 contextId,
   按规范 §3.4.3,这就是"在同一 context 里新建任务",anet 照此处理。于是原任务一直停在 input-required,
   provider 收到的是同 context 的另一个委派(context_id 会随委派送达,provider 是否把两者关联起来取决于它自己)。
   而且 anet 的文本任务里,provider 只要回一句普通消息(没有 `reply_task(state=completed)`),任务就进入
   input-required:Hermes 的头部会写 input-required,并提示 "needs more input",即使那句话其实就是答案。
   建议:
   - persona/SKILL 写明:多轮对话用 anet MCP 的 `send_message(task_id=…)`;`a2a_call` 适合一问一答;
   - provider 侧给最终答复时用 `reply_task(state=completed)`;
   - 不建议让 module/a2a 把只带 contextId 的消息并入旧任务:这违反规范语义,也会误伤合规客户端。
2. **阻塞与超时。** 超时指 socket 读空闲。anet 在完成前什么也不发,所以 agentwire 写的 `timeout: 3600`
   就是最长等待时间。超时之后:
   - Hermes 的报错里没有 contextId;
   - 任务在 anet 里继续运行,auto 档内可能已经付款;
   - 模型重试会带新的 messageId,`(contextId, messageId)` 去重无效,于是建出第二个任务;
     如果模型没带 context_id,第二个任务还在新的 context 里。

   找回办法:contextId 在 POST 之前已经写盘,`a2a_list` 能列出它;再用 anet MCP
   `list_tasks(context_id=…)`,然后 `get_task` 或 `wait_task`。persona 应写明:超时不要直接重试,先按上面找回;
   可能很长的任务改用 `send_message` + `wait_task`。
3. **支付。** Hermes 不认识 x402,也不发 `A2A-Extensions`,所以按 §8.7:
   - 在 auto 档之内,照常自动付款;
   - 超出 auto 档时,任务进入 `input-required`,`anet.reason=payment_extension_not_activated`。
   这时 Hermes 只能读到文字(文字质量见 §4 P3)。它随后发出的文本会落在新任务里,不会被当作付款。
   付款决定只能走 anet MCP 或 CLI。
4. **`a2a_discover` 不带令牌**,访问 anet 的代理卡片会得到 `HTTP 401`。查看卡片请用 MCP `get_agent_card`。
   `a2a_list` 不访问网络,不受影响。
5. **`a2a_orchestrate`。** agentwire 不写 `capabilities`,所以只有 `*` 能匹配到 anet 条目,而 `*` 会扇出到
   全部条目,可能产生多笔付费任务;`first` 模式下,已经发出的调用照常运行。建议保持不写 `capabilities`,
   并在 persona 里提醒:不要对 anet 条目使用 `*`。
6. **跨 agent 复用 contextId。** 模型把 A 的 context_id 用在 B 上时,anet 会拒绝("属于他人")。
   Hermes 显示为 "Peer '<B>' returned an error: …"。
7. **metadata 不可见。** `anet.effect_status`、`anet.receipt_verified`、`anet.reason`、`anet.retry_after_ms`、
   `x402.*` 都进不了模型读到的文本。
   - 能力任务的效果状态还能从交付物 JSON 的 `status` 里看出来;
   - 文本任务的回执是否验证过,完全看不到。
   需要这些信息时,用 MCP `get_task`。
8. **出站脱敏。** Hermes 发出消息前会删掉邮箱和凭据形字符串。任务本身需要邮箱时,内容会被改写。
9. **本地明文留存。** 每次往来都以明文写入 `~/.hermes/a2a_conversations/` 和 `a2a_audit.jsonl`。
   anet 的端到端加密只覆盖 daemon 与 daemon 之间,对外表述时要把这一点说清楚。
10. **取不到卡片时 POST 到基址。** 取卡片出现任何异常,Hermes 都会改为向 `/a2a/v1/agents/<aid>` 发 POST,
    anet 在这个路径上没有 JSON-RPC 路由,于是返回非 2xx(404 或 405)。可选的加固:module/a2a 在这个基址上也接受
    JSON-RPC POST,作为 `/jsonrpc` 的别名。概率低:代理卡片由本机生成,远端没有卡片时也按 AID 生成。
11. **端口或令牌变化**会导致 401 或连不上,由 doctor 检查(Q13),修复用 `anet agents wire --refresh`。
12. **Hermes 用不到的部分。** 流式、推送、GetTask、ListTasks、Cancel,Hermes 客户端都不调用。
    anet 在这些方面的能力对 Hermes 没有影响,也帮不上忙。

## 6. Hermes 作为提供侧后端(§11.6)

### 6.1 Hermes 服务端怎样接入入站任务

- **服务**:标准库 `http.server`,默认端口 9900。不配令牌时只绑定 `127.0.0.1`,调用者身份记为 `ip:<addr>`。
  `A2A_PEER_TOKENS="anet:<tok>"` 按令牌给出身份 `anet`;共享的 `A2A_BEARER_TOKEN` 仍按 IP 记身份。
  `A2A_TRUSTED_PEERS` 可以进一步限定身份。
- **方法与请求头**:接受 `SendMessage`,也接受旧名 `message/send`。`A2A-Version` 出现时只接受 1.0 或 1.0.0
  (a2a-go 客户端发的是 `1.0`)。请求体上限 1 MiB,限流默认每个身份 60 次/分钟。
  用 v1.0 方法名时,答复是 `{"task":…}`;用旧名时是裸 Task。
- **会话**:入站文本加上 "untrusted peer" 前缀,并过滤注入标记后,**注入到正在运行的 gateway 会话**:
  以 contextId 为 `chat_id`,由服务用户的同一个 agent 处理,记忆和工具齐全。
  `platforms.a2a.extra.agents` 可以配置"服务 agent":按路径前缀(如 `/anet`)或 tenant 路由到另一个 profile,
  以 `hermes chat -q … --source a2a` 子进程运行,每个 context 续用一个会话。服务 agent 的 profile 必须不同于
  运行中的 profile;相同的话它仍按 local 处理,注入运行中的会话。
- **阻塞与超时**:请求阻塞到 agent 回复,最长 `A2A_REPLY_TIMEOUT`,默认 300 秒,超时得到 `failed "[agent did not reply in time]"`。
- **状态**:
  - 正常回复 → `completed`,`status.message` 和一个 text artifact 里都是回复文本;
  - 回复以 `[INPUT_REQUIRED]` 开头 → `input-required`,问题放在 `status.message`;
  - 处理失败、关停、孤儿任务 → `failed`,文本是方括号说明;
  - 空回复 → `completed`,既没有 message 也没有 artifact。
- **不按 taskId 续写**:Hermes 为每条消息新建一个 Hermes 任务,不检查 message 里的 taskId(不会回 TaskNotFound),
  会话连续性只靠 contextId。
- **防循环**:同一 context 在 1 小时内超过 `A2A_MAX_PINGPONG_TURNS` 轮(默认 5,上限 20)时,
  直接回 `rejected "Anti-loop protection…"`。
- **只读文本**:Hermes 只读 part 的文本;raw 文件只显示长度,message metadata 一概不读,
  所以 `anet.peer_aid` 和 `anet.trusted` 进不到 Hermes 的 agent。回复只有文本,而且经过出站脱敏。

### 6.2 接入配置(建议)

```yaml
# anet
modules:
  a2a:
    backends:
      - match: "*"
        url: "http://127.0.0.1:9900/anet"      # 服务 agent 的路径前缀,不要用根路径(根路径就是运行中的会话)
        token_file: "/…/hermes_backend_token"  # 与 Hermes 的 A2A_PEER_TOKENS 中 anet 的令牌相同
        accept_untrusted: false                # Hermes 不满足 toolless,不要打开
        toolless: false
```

```yaml
# ~/.hermes/config.yaml
gateway:
  platforms:
    a2a:
      enabled: true
      extra:
        port: 9900
        agents:
          anet: {profile: anet-backend, description: "anet 信任对端的任务"}
```

Hermes 的环境变量:

- `A2A_PEER_TOKENS=anet:<tok>`,`A2A_TRUSTED_PEERS=anet`;
- 不设 `A2A_HOST`,保持只在回环上监听;
- `A2A_MAX_PINGPONG_TURNS=20`,`A2A_RATE_LIMIT` 按 anet 的并发量调高;
- `A2A_REPLY_TIMEOUT` 要小于 anet 后端的 HTTP 超时。

### 6.3 后端实现须知(B5-06)

- **不要解析 Hermes 的卡片。** 配了令牌后,Hermes 卡片的 `securitySchemes` 是 `{"bearer":{"type":"http","scheme":"bearer"}}`,
  这是 pre-1.0 形状,a2a-go v2 报 `unknown security scheme type`(已由测试钉住)。后端应直接对配置的 URL
  建 JSON-RPC 传输。不带令牌时的卡片 a2a-go 能读。
- **contextId 必须按对端隔开。** Hermes 用 contextId 选会话,所有 anet 流量又都以同一个身份 `anet` 到达。
  如果把远端对端给的 contextId 原样转发,两个不同的对端只要用同一个 contextId,就会进入同一个 Hermes 会话,
  能读到彼此的内容。后端发给 Hermes 的 contextId 应当由 `(peer_aid, 交互的 context_id)` 派生,例如取哈希,
  并且只在本机使用。
- **不要把根路径接到后端。** 根 agent 就是运行中的会话,它的长期记忆和工具会暴露给对端任务。
  应该用服务 agent profile,并且只给这个 profile 需要的工具。Hermes 无法经 A2A 声明"无工具",
  所以 Hermes 后端永远不能满足 `toolless`。
- **状态映射**:
  - Hermes `completed`(回复非空)→ provider 最终答复(`reply_task(state=completed)`,签回执);
  - `input-required` → provider 消息(不完成任务);
  - `failed` / `rejected` → 对应的 status,附上 Hermes 的文本;
  - **空的 `completed`** 不要签成"完成"的回执,当作失败处理,或留在收件箱。
- **轮次**:多轮对话里,每一轮都作为同一 contextId 下的新消息发出。Hermes 要靠 `[INPUT_REQUIRED]` 才会停在
  input-required,其余回复都是 `completed`,所以大多数任务一轮就结束。防循环上限计入每条转发;
  Hermes 的 `tasks/cancel` 只有在取消未终结的任务时才重置该 context 的计数,而同步调用返回时任务都已终结,
  所以这条路基本用不上,应当调高 `A2A_MAX_PINGPONG_TURNS`。
- **附件**:1 MiB 请求体上限,Hermes 也看不到 raw 内容。附件只转发元数据(文件名、类型、大小、CID)的文字说明,
  不内联字节。
- **识别对端**:Hermes 无法区分对端,§11.6 已写明;metadata 也到不了 Hermes 的 agent。

## 7. agentwire 写 `a2a_agents` 的依据

`internal/agentwire/hermes.go` 当前写的条目是
`"<aid>": {url: http://<a2a_addr>/a2a/v1/agents/<aid>, auth: {type: bearer, token}, timeout: 3600}`。
各字段与源码的对应关系如下:

- **键写 AID**:`a2a_call(agent=…)` 的参数就是这个键;`a2a_list` 也按键列出条目。
- **url 写代理基址**:Hermes 在它后面拼 `/.well-known/agent-card.json`,用令牌取回代理卡片,
  再从卡片的 `supportedInterfaces` 里取 JSONRPC 接口 URL(`…/jsonrpc`),见 `TestHermesRequestShape`。
  基址本身没有 JSON-RPC 路由,只在取不到卡片时才会被 POST(§5.10)。
- **`auth.type: bearer`**:Hermes 只认这一种类型,而且要求 token 非空;取卡片和 POST 时都会带上它。
  令牌就是 `a2a_token.txt`,所以文件保持 0600。
- **`timeout: 3600`**:阻塞调用期间 anet 什么都不发,缺省的 120 秒会让大多数跨节点任务超时,
  然后被重试成第二个任务(§5.2)。取卡片的超时由 Hermes 自己封顶在 30 秒。
- **不写 `capabilities`**:避免 `a2a_orchestrate` 按能力或 `*` 扇出(§5.5)。
- **不写 `tenant`**:代理卡片不带 tenant;要写的话必须等于 AID,否则 module/a2a 以 InvalidParams 拒绝。
- **`--refresh`、`unwire` 与 doctor**:端口或令牌变化后,由 `--refresh` 重写;`unwire` 删除带令牌的条目;
  doctor 比对 URL 端口与 `a2a_addr.txt`、令牌与 `a2a_token.txt`(Q13)。
- **是否写 `a2a_agents` 决定工具是否出现**:写了才会出现 Hermes 的 `a2a` 工具集(§2.1),
  只写 `mcp_servers.anet`(timeout 900,供 `wait_task` 用)时不会出现。

## 8. 复现

```sh
source /data/projs/anet-dev/wt/hermestck/env.sh
go test ./internal/a2ashape -run TestHermes -count=1 -v   # 契约、与 Hermes 一致、请求形状、后端、Python 细节
```

投影或用例有变化时,重新生成 Hermes 原代码的记录。脚本只用 Python 标准库:它把插件对 Hermes 其余部分的
import 换成桩,只替换 `tools._http_json`,插件本身的代码不动。

```sh
ANET_HERMES_DUMP=/tmp/hermes_cases.json go test ./internal/a2ashape -run TestHermesContract -count=1
python3 internal/a2ashape/testdata/hermes_golden.py /data/projs/anet-dev/Refs/hermes-agent /tmp/hermes_cases.json \
    > internal/a2ashape/testdata/hermes_a2a_call.json
```

这次没有做的:没有起真实的 Hermes gateway 联调,那样需要装 Hermes 的依赖;这一项留给 B5-03 的联调脚本。
本记录的结论来自对插件源码的逐行移植,以及在桩环境里运行插件原代码得到的输出。
