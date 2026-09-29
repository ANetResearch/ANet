# 0041 · 验证:Claude Code 真实模型经 MCP 调用 anet

日期:2026-09-28 至 29(UTC 09-28 23:45 – 09-29 00:25)。对应:0035 §10 遗留 8("Claude Code 真跑");产品负责人决定"just use yourself"——
用本机已登录的 Claude Code(`/home/ink/.local/bin/claude`,即本会话所用的安装与登录态)做真实模型调用,不改用户的
Claude 配置。

代码:工作树 `wt/ccmcp`,分支 `wp/ccmcp`,基于三仓 main = v0.2.1(ANetCore `528472a`、ANet `32f6005`、ANetHub `0b7f493`)。
只在 ANet 上提交(§7),未推送。环境:本机 Linux 6.8;Go 1.26.6;Claude Code **2.1.284**,模型 **claude-opus-5-5**
(`--model opus`,`--effort high`,即用户设置里给该模型的档位);会话里 39 个工具(Claude Code 自带 25 个 + anet 14 个)。
本机进程只监听 127.0.0.1:47480–47484,工作目录在会话 scratchpad 下,跑完已按路径停掉(§2.4)。

## 1. 结果一览

| 项 | 结果 |
|---|---|
| 真跑 | 3 轮共 26 次 `claude -p`(另有登录探测 1 次、终版脚本自检 2 次,合计 29 次),全部 `terminal_reason: completed`,无 `permission_denials`,没有用 anet 以外的工具(每次先有一次 `ToolSearch`:Claude Code 按需加载 MCP 工具定义) |
| 场景 1 列 agent | 模型 `list_agents` 一次后如实列出 P 的 9 项能力与两项价格。**修前 3/3 次告诉用户"新节点自动付款与代付的限额都是 0,收费能力要你在终端 `anet pay`"**——本节点 auto_max 2、agent_max 10,模型看不到限额,照说明里的缺省值当成了事实(F1)。修后 4/4 不再断言 |
| 场景 2 能力调用 | `text.stats`:`get_agent_card → send_message(skill)`,completed、`effect_status=OK`、`receipt_verified=verified`,报告与记录一致,后端只执行一次(3/3,含终版自检) |
| 场景 3 文本任务 | 45 s 后答复:模型给 `send_message` 设 60–120 s,一次调用内拿到 completed,原文照转(2/2)。330 s 后答复(超过单次等待上限 300 s):`send_message → wait_task(300)` 等到 completed 再答(1/1;另 1 次因脚本触发词写错,P 回了自动确认,模型如实报"没有交付",见 §4.5) |
| 付费(auto 档) | `demo.digest.paid` 价 2 ≤ auto_max 2:模型不调 `submit_payment`,R 余额恰好 −2、P +2,一张 `payment-completed` 收据,后端执行一次(2/2) |
| 付费(agent 档) | `lab.report.priced` 价 5 > auto_max、≤ agent_max 10,用户授权 5 以内:`send_message → submit_payment → wait_task`,`submit_payment` 恰好一次,R −5、P +5,后端一次(2/2) |
| UNVERIFIED | `lab.notes.append` 后端在 1.5 s 截止后才答 → failed、`effect_status=UNVERIFIED`、`anet.reason=timeout`:模型报"不能确认已写入",`success=unknown`,**没有重发**(后端只收到一次),并说明重发可能写两条(2/2) |
| input-required | P 先追问人数与时间:用户没给细节时模型不替用户编,停在 input-required 并转述问题(4/4);给了细节时在同一 `task_id` 上续写,拿到预订号(修后 2/2)。**修前 2/2 次第二个会话的新请求被并进上一个会话仍未结束的任务**:模型用正文与日期拼 `message_id`(`book-meeting-room-20260929-1`),同一 id 到同一 agent、旧任务未终止,节点按重试返回旧任务、新消息没有发出(F2) |
| 修改 | MCP 服务端说明、`send_message`/`submit_payment` 描述与 `message_id` 参数说明、wire 安装的 SKILL 文本(F1、F2、F3、F4);回归测试去掉修复即红;GUIDE §6.4、A2A-DESIGN §12 同步,语料同步 |
| 修后复测 | 14 次 13 通过;唯一未过是脚本自身的触发词错误(§4.5),修正后复跑通过。修前 12 次 7 通过,5 次未过全是 F1(3 次)、F2(2 次) |
| 费用 | 本轮共 **$3.44**(29 次调用);单个场景 $0.05–0.20,3–7 个回合,缓存读 4.8×10⁴–1.4×10⁵ token、写 3.6×10³–1.9×10⁴、输出 0.6×10³–1.4×10³(§5) |
| 用户配置 | `~/.claude.json` 的 `mcpServers` 前后哈希相同(只有 `shadcn`)、`~/.claude/settings.json` 哈希相同、无 `~/.claude/skills/anet`;每个运行目录留下一个空的 `~/.claude/projects/<目录>/memory/`,已 rmdir(§2.4) |

## 2. 做法

### 2.1 网络(`scripts/claude-mcp-e2e.sh up`)

| 端口 | 用途 |
|---|---|
| 47480 | 新起的 anet-hub(注册即得 100 额度) |
| 47481 / 47483 | R(requester)控制面 / 本机 A2A 接口(`a2a_addr.txt` 预写钉住) |
| 47482 / 47484 | P(provider)控制面 / 本机 A2A 接口 |

- 每个节点各自 `ANET_HOME = ANET_DATA_DIR`、`HOME`、`XDG_RUNTIME_DIR`(0700)在 `$J/run/{r,p}` 下。
- R:`payments.auto_max 2`、`agent_max 10`、`agent_daily_max 1000`,P 在 `payees.allow`。MCP 服务以 R 的身份运行。
- P:`inbound.policy closed`,R 在 `peers.allow`(R 的文本任务被接纳);公开能力由 `anet-official service-config -groups tools,paid`
  生成(`text.stats` 等 6 项与 `demo.digest.paid` 价 2),anet-official 在 Unix socket 上;另加脚本自己的两项(同一脚本的
  `backend`,另一个 socket):`lab.notes.append`(免费,timeout_ms 1500,后端 5 s 后才答,效果未知)与 `lab.report.priced`(价 5)。
  scratchpad 路径使 socket 地址超过 107 字节,脚本此时把两个 socket 放进 `$XDG_RUNTIME_DIR/anet-e2e.XXXXXX`(0700),`down` 删除。
- P 的文本任务由脚本的 `responder` 经 P 控制面 `/tasks/list`、`/tasks/reply` 答复:问 hub 的问题 45 s 后答(超过 `send_message`
  缺省的 30 s);"三档付款说明" 330 s 后答(超过单次 `send_message`/`wait_task` 最多 300 s);订会议室先 input-required 追问,
  续写后给预订号。每条答复记入 `responder.jsonl`(含回复编号,供核对模型是否原样转述)。

### 2.2 Claude Code 的调用

MCP 条目取自 `anet agents wire claude` 在一次性 HOME 里写出的 `mcpServers.anet`(`env -i PATH=/usr/bin:/bin`,claude 不在 PATH 上,
wire 直接写文件而不调 `claude mcp add`;条目为 `{"type":"stdio","command":"<J>/bin/anet","args":["mcp"],"env":{"ANET_DATA_DIR":"<R>"}}`),
技能文本取自同一次 wire 写出的 `SKILL.md`。每个场景:

```sh
cd $J/cc/cwd && claude -p "<提示>" --output-format json --verbose --max-turns 20 --allowedTools 'mcp__anet__*' \
  --mcp-config $J/cc/mcp.json --strict-mcp-config --append-system-prompt "$(cat $J/cc/skill.md)" \
  --setting-sources project --no-session-persistence --permission-prompts none \
  --model opus --effort high --max-budget-usd 1.50
```

- `--setting-sources project` 且运行目录为空:不加载用户设置(用户的 Stop/Notification 钩子会写 `~/.claude-notify/trigger.json`
  触发桌面通知,插件与模型缺省也在其中),所以模型与档位显式给出。
- `--verbose` 使 json 输出为全部消息的数组(system init、assistant 的 tool_use、user 的 tool_result、最后的 result 含 `usage`、
  `total_cost_usd`、`modelUsage`),工具序列与模型看到的每个工具结果都从这里读。
- 提示按负责人给的三句写(§3 各节),每个提示末尾要求单独一行 `REPORT task=… state=… effect=… receipt=… paid=… success=yes|no|unknown`,
  供脚本与记录对照;回答正文不受限制。

### 2.3 核对(`scripts/claude-mcp-e2e.py check`)

每次运行前后各取一份快照:R 的出站任务、R 与 P 的余额(`/balance`)、两个后端各能力的执行次数(anet-official 日志、
`lab-calls.jsonl`)、responder 的答复。断言:只用了 anet 工具(`ToolSearch` 除外);没有工具调用出错;恰好一个新任务、发给 P、
技能正确;模型最后看到的任务状态就是记录里的终态(没看到结果就不作答);`send_message` 返回非终态时随后有 `wait_task`/`get_task`;
REPORT 的 state/effect/receipt 与 R 控制面 `/tasks/get` 一致;UNVERIFIED、failed、input-required 不报 success=yes;付款额等于价格
(R 余额差)且任务上一张 `payment-completed` 收据;auto 档不调 `submit_payment`、agent 档恰好一次且其后等待;后端恰好执行一次
(UNVERIFIED 场景即"没有重发");文本场景的回复编号原样出现在回答里;input-required 场景不替用户续写;`message_id` 不与之前
运行用过的重复;列 agent 的回答不把限额说成 0。

### 2.4 本机状态前后对比

- 前:127.0.0.1:47480–47489 无监听;无 anet 进程属本任务(root 的 `anet-mgmt-py` 与别的工作包的进程不在比较范围内)。
- 后:`down` 经控制面 `/shutdown` 停两个 daemon,再按路径(`stop_under $J/bin`)停 hub、anet-official、backend、responder;
  47480–47489 无监听,`/run/user/1001/anet-e2e.*` 已删。本任务的两个 Monitor 留下的 `tail -F` 按 PID 停掉。
- Claude 配置:`~/.claude.json` 的 `mcpServers` 键与哈希、`~/.claude/settings.json` 哈希前后一致,没有 `~/.claude/skills/anet`。
  `--no-session-persistence` 下没有会话记录,但 Claude Code 仍为运行目录建了空的 `~/.claude/projects/<目录>/memory/`
  (探测、j、j2 三个目录各一个),均为空,已 `rmdir`,`~/.claude/projects` 回到 19 个。脚本不碰 `~/.claude`,脚本头注释写明这一点。

## 3. 修前(r1、r1b:v0.2.1 的说明文字)

| 轮 | 场景 | 结果 | 工具序列(不含 ToolSearch) | 回合 | 缓存写 | 缓存读 | 输出 | 费用 $ | 秒 |
|---|---|---|---|---|---|---|---|---|---|
| r1 | list | 未过 F1 | list_agents | 3 | 12 910 | 47 887 | 1 019 | 0.1333 | 12 |
| r1 | cap | 通过 | get_agent_card → send_message | 4 | 13 498 | 78 666 | 1 389 | 0.1515 | 17 |
| r1 | text | 通过 | send_message(120 s) | 3 | 9 572 | 53 613 | 780 | 0.1029 | 56 |
| r1 | paid-auto | 通过 | get_agent_card → get_balance → send_message | 5 | 16 556 | 80 267 | 1 083 | 0.1702 | 15 |
| r1 | paid-agent | 通过 | get_agent_card → get_balance → send_message → submit_payment → wait_task | 7 | 18 704 | 136 759 | 1 358 | 0.2042 | 17 |
| r1 | unverified | 通过 | get_agent_card → send_message | 4 | 14 223 | 79 938 | 1 068 | 0.1512 | 16 |
| r1 | ask | 通过 | send_message | 3 | 8 281 | 53 579 | 660 | 0.0902 | 12 |
| r1 | ask-answer | 未过 F2 | send_message → send_message | 4 | 10 885 | 75 859 | 1 091 | 0.1241 | 17 |
| r1b | list ×2 | 未过 F1 ×2 | list_agents | 3 | 9 217 / 4 864 | 51 588 / 55 935 | 1 115 / 1 255 | 0.1064 / 0.0752 | 13 / 14 |
| r1b | ask | 通过 | send_message | 3 | 3 702 | 58 121 | 640 | 0.0541 | 12 |
| r1b | ask-answer | 未过 F2 | send_message → send_message | 4 | 6 209 | 80 379 | 1 049 | 0.0868 | 17 |

(未缓存输入每次 6–12 token,略。r1 的 list 第一次核对时 `ToolSearch` 被当成非 anet 工具,改脚本后对同一份输出重核;
r1b 第一次 list 的输出被第二次覆盖,只留下 summary 行——之后脚本给重复运行的场景加 `.2`、`.3` 后缀。)

各场景里模型做对的事(修前修后一致):

- 用 `get_agent_card` 看价格与签名再发;`send_message` 自带 `message_id`(模型自己想到的重试保护);按需设 `timeout_seconds`
  60–120,因而多数任务在一次调用里就到终态,`wait_task` 只在确有必要时用(agent 档付款后、330 s 的长任务)。
- `completed`、`effect_status`、`receipt_verified` 分开报告;UNVERIFIED 报"不能确认",并主动说明不重发的原因;
  verified 回执解释为"证明是它签发的,不证明事情做成"。
- 能力结果自己复算核对(text.stats 的字节、词数),对 P 在回复里多说的内容指出"说明里没写"。
- auto 档价格内不调 `submit_payment`;用户给了上限时在 agent 档内 `submit_payment` 一次,`payment-submitted` 后用 `wait_task` 等结算与结果。

## 4. 发现与修复

### 4.1 F1:模型把缺省限额当成本节点的限额告诉用户

修前服务端说明写"submit_payment spends within the operator's agent limits, which are 0 until the operator raises them on a
terminal",`submit_payment` 描述写"(both 0 on a new node)",SKILL 写"The auto and agent limits are 0 on a new node, so expect
`submit_payment` to be refused until the operator raises them"。列 agent 时模型 3/3 次在回答末尾告诉用户"新节点默认不允许自动付款,
也不允许我代为付款,两项限额都是 0。所以调用收费技能时,需要你在自己的终端上决定是否支付,命令是 `anet pay <task_id>`"
(另一次:"需要你本人在终端上放开额度")。本节点 auto_max 2、agent_max 10:这两项收费能力一个会被自动付、一个在 agent 档内。
MCP 没有工具能读限额(`node_status`、`get_balance` 都不含;控制面 `/payments/status` 不在 MCP 白名单),模型只能照说明里的
缺省值说。

修复:说明、`send_message`、`submit_payment` 与 SKILL 都改为"限额归运营者,新节点为 0,**没有工具显示它们**:在节点或
`submit_payment` 答复之前,不要告诉用户它们是多少、也不要说某个价格付不了";`send_message` 改为"发任务,而不是告诉用户付不了"。
修后 list 4/4 次(r2 三次、终版自检一次)不再断言,改为"按你的支付额度,这些付费技能可能由节点自动付款,也可能要等你批准。额度要等实际调用时才知道"。

### 4.2 F2:由正文与日期拼成的 `message_id` 让新请求并进旧任务

修前 `send_message` 描述:"Give a `message_id` of your own to make a retry safe: the same message_id returns the task it already
made."。模型每次都给,但全部由正文与日期拼成(修前 9/9:`book-meeting-room-20260929-1`、`ask-hub-read-body-20260929-1`、
`lab-report-priced-20260929-01`……)。0017 Q32:同一 `message_id` 发给同一 agent、该 id 所在任务**未终止**时即视为重试,返回该任务、
不发新消息,**不比较内容**(单独核对:任务未终止时用同一 id 发一段完全不同的文字,返回的仍是原任务,新文字没有发出)。
于是 ask 场景留下的 input-required 任务,在下一个会话(ask-answer,同样"帮我订会议室")被模型用同一个 id 命中:返回旧任务,
模型在旧任务上续写并完成,用户以为是新预订——2/2 次。

修复:`send_message` 描述与 `message_id` 参数说明改为"每条新消息用新的随机 id(UUID),不要由正文或日期拼成,只在重复这一次调用时
沿用;id 所在任务未终止时,同一 id 发给同一 agent 返回该任务、什么也不发,不管新消息写的是什么";SKILL 第 2 步同样写明。
修后新任务的 id 12/12 为 UUID 形(r2 十一个、终版自检一个),ask → ask-answer 2/2 各自新建任务并完成。

残留风险:模型"随手写"的 UUID 熵很低——修后 12 个 id 里,`7c3e9a52-4f1b-4d8e-9b6a-…` 与 `7c3e9a52-4f1b-4d8e-9b61-…` 前三段相同,
`7c1e4b92-3f6a-4d8e-…` 与 `7c1e4a92-3b5d-4f8e-…`、`6f1c2a8e-…` 与 `6f1c2a9e-…` 只差一两位。碰撞时后果与上面一样(静默返回旧任务)。建议 daemon 在认定重试时比较内容,见 §8 事项 1。

### 4.3 F3:SKILL 说自由文本会发给 hub(与实现相反)

SKILL 第 1 步写"Prefer a skill id … to free text: a free-text query is sent to the hub"。实现与 `list_agents` 描述相反:`query` 只在
本机对 hub 返回的一页卡片做匹配(`taskseam_agents.go` `registryQuery` 只带 skill、tag、cursor、limit;A2A-DESIGN §10.5)。
本轮模型没有用 `query`,未见后果;改为"hub 按 skill id 找发布它的 agent,自由文本只在本机筛 hub 发来的那一页卡片"。

### 4.4 F4:报价上的 `needs_operator_approval` 与说明的字面冲突

报价超过 auto 档时,任务在任何人决定之前就带 `anet.reason=needs_operator_approval`(`x402task.go` `PaymentReason`:未付的报价一律如此)。
修前说明写"If a payment needs the operator (anet.reason needs_operator_approval), tell the user the price and the payee",字面上会让模型
一见报价就停手。本轮模型在用户给了上限时仍正确地 `submit_payment`(2/2),未见误用;顺手把说明、`submit_payment` 与 SKILL 改为
"报价上的 needs_operator_approval 表示待决定;用户要付就 `submit_payment`;只有 `submit_payment` 的答复带 `spend_refusal` 时才是
超出 agent 档,这时告诉用户价格与收款方"。

### 4.5 脚本自身的一处错误(已改)

长任务场景最初按"几分钟"触发 330 s 的延迟,模型转发时去掉了括号里的"对方说这要几分钟",P 于是 45 s 后回了一条自动确认
("收到:…这是 P 的自动回复")。模型的处理是对的:指出"只回了一条自动确认,没有交付说明",`success=no`,没有重发——脚本按
"completed 应报 success" 判了未过。触发词改为请求的主题"三档"后复跑通过。

### 4.6 回归测试

`internal/mcpserv` `TestTheToolSurfaceIsWhatWePromise`:`send_message` 须写明 fresh random `message_id`、不由正文或日期拼、
"returns that task and sends nothing";`send_message`、`submit_payment` 须写"no tool shows";服务端说明须含"no tool shows them"、
"do not tell the user what they are",且不再含"which are 0 until the operator raises them"。`internal/agentwire`
`TestGuideTextIsCurrent`:SKILL 不得再含"free-text query is sent to the hub"、"expect `submit_payment` to be refused",须含
"fresh random id"、"no tool shows their current"、"needs_operator_approval"。把新测试放到 v0.2.1 源码上跑:两个测试共 12 处报错(红);
修后全绿。

## 5. 修后复测(r2)与 token、费用

| 场景 | 结果 | 工具序列(不含 ToolSearch) | 回合 | 缓存写 | 缓存读 | 输出 | 费用 $ | 秒 |
|---|---|---|---|---|---|---|---|---|
| list ×3 | 通过 ×3 | list_agents | 3 | 13 288 / 4 886 / 3 737 | 48 607 / 57 019 / 58 166 | 1 044 / 1 012 / 1 116 | 0.1369 / 0.0708 / 0.0639 | 12–14 |
| cap | 通过 | get_agent_card → send_message | 4 | 14 360 | 81 389 | 1 360 | 0.1584 | 19 |
| text | 通过 | send_message(60 s) | 3 | 9 595 | 54 457 | 744 | 0.1026 | 57 |
| text-long | 未过(§4.5) | send_message → wait_task | 4 | 12 307 | 78 262 | 1 414 | 0.1424 | 63 |
| text-long.2 | 通过 | send_message(60 s,submitted)→ wait_task(300 s,completed) | 4 | 10 381 | 76 696 | 953 | 0.1175 | 340 |
| paid-auto | 通过 | get_agent_card → send_message | 4 | 15 926 | 80 213 | 1 112 | 0.1657 | 15 |
| paid-agent | 通过 | get_agent_card → send_message → submit_payment → wait_task | 6 | 18 544 | 137 921 | 1 387 | 0.2037 | 18 |
| unverified | 通过 | get_agent_card → send_message | 4 | 13 822 | 80 221 | 1 080 | 0.1483 | 16 |
| ask ×2 | 通过 ×2 | send_message | 3 | 8 280 / 3 650 | 54 431 / 59 076 | 650 / 694 | 0.0902 / 0.0549 | 12–13 |
| ask-answer ×2 | 通过 ×2 | send_message → send_message(同一 task_id) | 4 | 11 773 / 6 974 | 78 590 / 83 209 | 1 204 / 1 070 | 0.1340 / 0.0939 | 21–22 |

合计(json 输出的 `usage`、`total_cost_usd`):

| 轮 | 次数 | 缓存写 | 缓存读 | 输出 | 费用 $ |
|---|---|---|---|---|---|
| 登录探测("Reply OK",无工具) | 1 | 2 080 | 531 | 4 | 0.0168 |
| r1(修前) | 8 | 104 629 | 606 568 | 8 448 | 1.1276 |
| r1b(修前,补样本) | 4 | 23 992 | 246 023 | 4 059 | 0.3224 |
| r2(修后) | 14 | 147 523 | 1 028 257 | 14 840 | 1.6831 |
| 终版脚本自检(`all list cap`) | 2 | 27 017 | 128 832 | 2 483 | 0.2916 |
| **合计** | 29 | | | | **3.44** |

(r1b 被覆盖的那次 list 的 token 按 summary 行补入。)

- 缓存读大头是 Claude Code 自己的系统提示与 39 个工具定义,每个回合都读一遍;缓存写随会话内首次出现的内容(首个运行冷,后续
  1 小时缓存命中,同一场景第二、三次运行的缓存写降到 3.6–7×10³)。
- anet 自己的部分:服务端说明 1 365 → 1 683 字符,14 个工具定义(tools/list JSON)19 242 → 19 975 字符,SKILL 5 458 → 6 260 字节,
  合计增加约 1.9 KB(约 500 token)。单个工具结果最大 7.7 KB(`list_agents` 一个 VERIFIED 条目带卡片原字节;`get_agent_card` 7.6 KB),
  远低于 Claude Code 的单结果上限(0035 §7.4 之后 `list_tasks` 每任务 8 KB、单任务 24 KB)。
- 付款场景最贵(6–7 回合,$0.20);最便宜的是重复运行的 ask($0.05)。

## 6. 其他观察(未改)

1. **完成答复上的 `anet.state=working`**。provider 以 completed 结束文本任务时,最后一条消息按 0017 Q30 带 `anet.state=working`、
   `anet.final=true`(让请求方的阻塞等待不在中间返回)。这条 metadata 原样出现在模型看到的 history 里;text-long 那次模型写道
   "这条消息标的是 working,任务却已是完成状态"。本次未造成误判,但它是投影给读者的噪声,见 §8 事项 3。
2. **每次多一个回合**:Claude Code 2.1.284 把 MCP 工具定义延迟加载,模型先 `ToolSearch` 再调用(`select:mcp__anet__…`),每个场景多 1 回合。
3. **模型不怎么用 `wait_task`**:它倾向给 `send_message` 设 60–120 s 的 `timeout_seconds`,因此 45 s 的答复一次拿到。等待超过
   300 s 的任务,模型按说明接 `wait_task(300)`,没有重发、没有在 working 时作答。
4. **没有测的**:不带 SKILL(只有服务端说明与工具描述,`CLAUDE_E2E_SKILL=0`)、provider 侧(`list_tasks role=provider` + `reply_task`)、
   超出 agent 档的拒付、对端文本里的注入。脚本可以直接加场景。

## 7. 本轮提交(ANet,`wp/ccmcp`)

| 提交 | 内容 |
|---|---|
| `3302836` fix(mcp) | 服务端说明、`send_message`(含 `message_id` 参数说明)、`submit_payment` 的描述;wire 的 SKILL(F1–F4);两处回归测试;GUIDE §6.4、A2A-DESIGN §12 同步;`cmd/anet-official/corpus` 同步 |
| `20fa88c` test(scripts) | `scripts/claude-mcp-e2e.sh`、`scripts/claude-mcp-e2e.py`:本机网络、Claude Code 调用、核对与汇总;需 `ANET_CLAUDE_E2E=1`,CI 与其他脚本不调用 |
| (本文提交)docs(notes) | 本文 |

提交前在工作树上:`gofmt -l` 为空;`go vet ./...` 通过;`go test -count=1 ./...` 39 包 ok(第一次全量时 `module/a2a` 失败一次,
输出被截断看不到用例名;随后单独 4 次与第二次全量均通过,本机当时另有进程占着 43811,与该包分配 A2A 端口的起点相同,判断为
同机干扰,记录备查);`refresh-corpus.sh --check` 通过;`scripts` 包的令牌检查覆盖新脚本。

## 8. 遗留与需要负责人决定的事项

1. **重试判定是否比较内容(F2 的根)**。现在同一 `message_id` 到同一 agent、任务未终止即返回原任务,不看内容(0017 Q32)。
   描述已改,但模型自拟的 UUID 熵低,仍可能碰撞。建议:认定重试时比较消息内容(或其 CID),不同则拒绝并说明
   "该 message_id 已用于任务 X 上另一条消息",而不是静默返回旧任务。改的是 Q32 的语义,需负责人定。
2. **是否让 MCP 读到支出上限**。现在模型只能"不断言";若把控制面 `/payments/status`(auto_max、agent_max、agent_daily_max、
   24 小时已付)只读地并进 `get_balance` 或 `node_status`,模型可以如实回答"这个价能不能付"。要在 MCP 白名单加一条只读路由(§12)。
3. **完成答复的 `anet.state=working`(§6.1)**。是否在投影里对 `FinalReply` 消息去掉 `anet.state`(保留 `anet.final`),A2A 客户端与
   MCP 看到的 history 都会变。
4. **费用**:完整 9 个场景一次约 $1.3、7 分钟(长任务场景占 5.5 分钟);按次计费到本机登录的账户。脚本默认不跑,是否另设定期
   运行由负责人定。
5. 0035 §10 遗留 8 已由本文关闭。

## 9. 复现

```sh
source /data/projs/anet-dev/wt/ccmcp/env.sh
J=<私有目录>                                                   # 0700;路径长时 socket 自动放到 $XDG_RUNTIME_DIR 下
ANET_CLAUDE_E2E=1 J=$J bash scripts/claude-mcp-e2e.sh all      # 构建、起网络、9 个场景、停;或 all list cap 只跑指定的
J=$J bash scripts/claude-mcp-e2e.sh up                          # 只起网络(E2E_BIN=<含 anet anet-hub anet-official 的目录> 用现成二进制)
ANET_CLAUDE_E2E=1 J=$J bash scripts/claude-mcp-e2e.sh run list ask ask-answer
J=$J bash scripts/claude-mcp-e2e.sh summary $J/out/<时间>
J=$J bash scripts/claude-mcp-e2e.sh down
rmdir ~/.claude/projects/<运行目录对应的名字>/memory ~/.claude/projects/<运行目录对应的名字>   # Claude Code 留下的空目录
```

可调:`CLAUDE_BIN`、`CLAUDE_E2E_MODEL`(opus)、`CLAUDE_E2E_EFFORT`(high)、`CLAUDE_E2E_BUDGET`(每次 1.50 美元上限)、
`CLAUDE_E2E_MAX_TURNS`(20)、`CLAUDE_E2E_SKILL`(1)、`E2E_PORT_BASE`(47480)、`E2E_REPLY_DELAY`(45)、`E2E_LONG_DELAY`(330)。
