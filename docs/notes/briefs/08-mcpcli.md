简报 · MCP 重组/CLI 命令/安装与发布签名(D2) · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §12(MCP 工具表)、§13 全部、§5.3 与 §8.6 的 TTY 命令(`anet pay`、`anet peers allow`、`anet inbound approve`)、SI-5 的 `anet doctor --json`。
代码:`internal/mcpserv`、`cmd/anet`(全部子命令)、`internal/daemon` 的 `install.go` `agents_install.go` `agents.go`
`identities.go` `paths.go`;`deploy/release/*`、`scripts/build.sh`;ANetHub 的 `deploy/install.sh`、`deploy/nginx-hub.conf`、
`internal/aghub/web/llms.txt`;`/data/projs/anet-website/public/install.sh`;`docs/site`。
行号以本分支 HEAD 为准(ANet `6295665`,ANetHub `fa98276`)。标注:**[实测]** 本机跑过;**[本地源码]** 读本机某份源码得到;
**[推断]** 本机无法核实。

---

## 0. 结论速览

| 条目 | 判定 | 说明 |
|---|---|---|
| `anet mcp` 显式身份走严格解析(§7.8) | done | `cmd/anet/mcp.go:37-41`,测试 `console_cli_test.go:74` |
| `anet peers allow/trust`、`anet inbound approve`、放宽 `inbound policy` 须 `/dev/tty` 确认 | done | `cmd/anet/peers.go:43-60`(`confirmOnTTY`)、`:76-87`、`:117-126`、`:129-136`;测试 `peers_test.go:63` |
| 批准时对当前 KEL 重验 | done | `internal/daemon/pending.go` `ApprovePending` → `reverifyPending` |
| `anet accept on` / `hub-register --accept-delegations true` 拒绝 | done | `peers.go:24-31`、`:202-227` |
| `/inbound/pending` 只返回元数据 | done | `inbound_api.go:127-134`,`pending.go:47-57`(可直接给 MCP `inbound_pending` 用) |
| §12 MCP 13 个新工具 | missing | 仍是 9 个旧工具 `mcpserv.go:42-197`;其中 10 个依赖 C5 的 `/tasks/*`(未开始) |
| §12 `ServerOptions.Instructions`、工具注解 | missing | `mcpserv.go:40` 传 `nil`;全文无 `Annotations` |
| §12 "原样转发投影 JSON" | diverged | 现用 `passthrough map[string]any` 重编码,另有 `findOut`/`resultsOut` 自定义结构 |
| §12 `node_status` 描述与实现对齐 | diverged | 描述称含模块与当前工作(`mcpserv.go:191-192`),`hStatus`(`control_api.go:356-393`)两者都不返回 |
| §8.6 MCP 不调用 gateway 路由 | diverged | `task_delegate` 的 `pay=true` → `/delegate pay:true`(`mcpserv.go:89-91`),按 §8.6 表属 `gateway` 档 |
| §10.5 `list_agents` 自由文本不发给 hub | diverged | `agents_find` 的 `query` 经 `/find` 原样发给 hub(`control_api.go:562-583`) |
| §13.1 `anet init` | missing | 无命令;`EnsureLayoutInit`(`identities.go:316`)可做底座 |
| §13.1 / SI-5 `anet doctor [--json]` | missing | 无命令 |
| §13.1 `anet agents wire|unwire`、`internal/agentwire` | missing | 只有 `anet install --agent`(只写 persona,`main.go:97-98` → `install.go:75-81`) |
| §13.1 persona/SKILL 重写 | missing | `install.go:23-68` 仍写 "v0.1"、"display-only pricing"、"自行上架接活" |
| hermes 探测 `/opt/data` 误报、`writeManagedBlock` 两分支相同 | diverged | `install.go:89`;`agents_install.go:140-145` |
| §8.6 `anet pay <ix>`(TTY → `/tasks/pay-manual`) | missing | 无命令、无路由(C3/C5) |
| §8.6 `anet redeem` 须 TTY | missing | `main.go:1443-1460` 直接调 `/redeem` |
| §8.6 修改支出上限/payees 的 TTY 命令 | missing | 无 `payments` 配置(C3 未开始),无 CLI |
| §13.2 `release.json` + ssh 签名 + `build-release.sh` 加固 | missing | `build-release.sh` 无签名、`BuiltAt` 取当前时间(`:64`)、脏树检查弱(`:63`)、只对宿主平台做符号自检(`:110`) |
| §13.2 install.sh 加固 | missing | 仍先解压后校验(`:134`/`:139`)、校验失败只警告(`:149-154`)、非 https 可用(`:50`/`:65-66`) |
| §13.2 `anet update` | missing | 无命令 |
| §13.2 公钥发布(SECURITY.md、README、官网、install.sh) | missing | `SECURITY.md` 无公钥;hub `llms.txt:115-119` 已要求用户去这两处抄公钥 |
| §13.2 hub `llms.txt` Step 0 | partial | 文本已改(`llms.txt:84-133`),但引用的 `anet update`/`anet agents wire` 尚不存在,且没给 0.1.x 用户退路;`:326`、`:357` 仍写 `anet install --agent` |
| §15 官方清单由发布密钥签署、随二进制打包 | missing | 与发布签名共用密钥与验签代码 |
| §16 CI 符号模式 `internal/mcpserv|internal/agentwire` | missing | `ci.yml:150` 只有 `internal/mcpserv` |
| §17 现有脚本的 MCP 探针改新工具名 | missing | `scripts/mcp-probe.py:60-77`、`joint-fleet.sh:286-302`、`prodtest.sh:1138-1195` |
| SI-8 `go list -deps ./internal/mcpserv ./internal/daemon | grep a2aproject` 为空 | done(暂时) | 本机实测为 0;a2a-go 还没进 ANet `go.mod`,加入后须保持 |

---

## 1. 开工前先看:并行在做的工作树

编写本简报时(17:00 前后),`/data/projs/anet-dev/wt/` 下有按 `mkwt.sh` 建的工作树,分支 `wp/<id>` 均从 `6295665` 分出,
改动**未提交**,文件时间戳为今天 16:54–17:08:

| 工作树 | 未提交内容 | 与本领域的关系 |
|---|---|---|
| `wt/agentwire` | `ANet/internal/agentwire/{agentwire,blocks,files,guide,jsonobj}.go` | 就是 §13.1 `agents wire` |
| `wt/cli` | `internal/daemon/{setup,audit,payments_config}.go`,改 `config.go` `control_api.go` | `init`/`doctor`/`audit` 与 C3 的 payments 配置 |
| `wt/release` | `internal/release/{sshsig,manifest,keys,update}.go`、`allowed_signers`、`cmd/anet/update.go`,改 `build-release.sh` `main.go` | §13.2 全部 |
| `wt/tasksd` | `internal/a2ashape/`、`internal/runtime/interactions/tasks.go`,改 `eventbus.go` | C5,`/tasks/*` 的前提 |
| `wt/x402d` | 改 `module/module.go` | C3 |
| `wt/mcp` | 干净 | 预留给 MCP 重组 |

本简报的判定只看 `a2a-redesign-wip`。动手前先与这些分支对齐,不要重复实现;合并时 `cmd/anet/main.go`(dispatch、
`knownFlags`、`usageAllText`)会是三方冲突点(cli/release/agentwire 都要加 `case`)。

---

## 2. 依赖与可立即动手的部分

| 工作 | 依赖 | 现在能做什么 |
|---|---|---|
| MCP `send_message` `get_task` `list_tasks` `wait_task` `cancel_task` `reply_task` | C5:`internal/a2ashape` + 控制面 `/tasks/*`(本分支不存在,全仓无 `/tasks/` 控制面路由) | 先定 MCP 侧的形态与测试(fake 控制面返回 a2ashape 金标 JSON) |
| MCP `submit_payment` `reject_payment`、CLI `anet pay` | C3(`AdmitSpend`、payments 配置)+ C5(`/tasks/pay`、`/tasks/pay-manual`) | CLI 的 TTY 流程与测试可先写,路由名按 §12 |
| MCP `list_agents` `get_agent_card` | ANetHub §10.5 注册表 API(**本分支 hub 没有**,见风险 2)+ C5 `TaskSeam.Agents/Card` | 过渡期 `list_agents` 可接 `/find` 的 `capability` 分支 |
| MCP `get_balance` `audit` `node_status` `inbound_pending`、Instructions、注解、路径白名单 | 无 | **现在就能做** |
| `anet init` | C3 的 `payments` 配置类型 | inbound/peers 部分现在能做,payments 键待 C3 |
| `anet doctor` | D1(`a2a_addr.txt`、`a2a_token.txt`)、C3、agentwire | 框架与 SI-5 的 inbound 部分现在能做 |
| `anet agents wire` | D1(Hermes `--a2a` 需要 A2A 地址与令牌) | MCP 注册与技能文本现在能做 |
| release.json / install.sh / `anet update` | 无(签名私钥保管待产品负责人) | **现在就能做**;部署与发布属阶段 G,须同意 |

---

## 3. MCP 重组(§12)

### 3.1 现有工具 → 新表映射

| 新工具 | 现有 | 控制面路由(现状) | 注解(设计) | 动作 |
|---|---|---|---|---|
| `list_agents` | `agents_find`(`mcpserv.go:42-61`) | `/find` 存在;目标 `TaskSeam.Agents` | readOnly | 自由文本不得发给 hub(§10.5 末段):按 skill/tag 取已验证卡片后本地子串匹配;条目带 `anet.official`(§15) |
| `get_agent_card` | 无 | 无;需新路由(建议 `POST /agents/card {aid}` → `TaskSeam.Card`) | readOnly | 返回原字节卡片 + 验证结论 |
| `send_message` | `task_delegate`+`task_message` | `/tasks/send`(缺) | openWorld | **不得**有 `pay` 参数(gateway 档) |
| `get_task` | `task_results` 单条 | `/tasks/get`(缺) | readOnly | |
| `list_tasks` | `task_results`+`task_inbox` | `/tasks/list`(缺) | readOnly | 过滤 `context_id` `role` `state` |
| `wait_task` | 无 | `/tasks/wait`(缺;`eventbus.go:113` `Watch` 已就绪) | readOnly | 见 3.4 超时 |
| `cancel_task` | 请求方 `task_end` | `/tasks/cancel`(缺) | — | |
| `reply_task` | 提供方 `task_message`/`task_end` | `/tasks/reply`(缺) | openWorld | 始终注册,无可回复任务时明确报错 |
| `submit_payment`/`reject_payment` | `task_delegate.pay` | `/tasks/pay`(缺) | destructive / — | purpose=`task-agent` |
| `get_balance` | `credit_balance`(`:176-187`) | `/balance` 存在 | readOnly | 改名即可 |
| `audit` | `evidence_read`(`:152-174`) | `/evidence` 存在 | readOnly | 摘要视图与 `anet audit` 同一结构(见简报 03 §14) |
| `node_status` | 保留(`:189-197`) | `/status` 存在 | readOnly | 让 `hStatus` 返回 `modules`(`module.Compiled()`,`internal/daemon/modules.go:54` 已在用)与活动交互计数,或删掉描述中的这两项 |
| `inbound_pending` | 无 | `/inbound/pending` 存在,只含元数据 | readOnly | 现在就能加 |

旧名一律删除,不留别名(设计写"取代")。改名会让客户端里按 `mcp__anet__task_delegate` 写的权限规则失效,发布说明写明。

### 3.2 实施要点(`internal/mcpserv/mcpserv.go`)

- 签名保持 `func New(c Control, version string) *mcp.Server`,改为
  `mcp.NewServer(&mcp.Implementation{…}, &mcp.ServerOptions{Instructions: instructions})`。`instructions` 是短文本常量:
  默认拒绝委派、名单由操作者在终端改、支出三档与默认 0、长任务 `send_message` + `wait_task`、"completed 且
  `anet.effect_status=UNVERIFIED` 不等于成功"。
- **原样转发**:返回 A2A 投影的工具用 `mcp.AddTool[In, any]`(Out 类型参数写 `any`),handler 里
  `var raw json.RawMessage; err := c.Call(ctx, path, body, &raw); return nil, raw, err`。
  - go-sdk v1.7.0 在 `Out == any` 时不推导也不校验输出 schema(`server.go:345`),`json.Marshal(json.RawMessage)` 只做压缩,
    字节语义不变;`controlClient.Call`(`cmd/anet/mcp.go:85-88`)对 `*json.RawMessage` 的 `Unmarshal` 是直接拷贝。
  - 现在的 `passthrough map[string]any` 会把整数转成 float64(超过 2^53 失真)并打乱键序,不满足"原样转发"。
  - 陷阱:`raw` 为空时 `any(json.RawMessage(nil))` 非 nil,会序列化成 `null` 塞进 `structuredContent`(规范要求对象)。
    空响应返回错误。
- **注解**:`Annotations: &mcp.ToolAnnotations{…}`(go-sdk `protocol.go:1967`)。MCP 语义里 `destructiveHint`、`openWorldHint`
  **缺省为 true**,`ReadOnlyHint` 缺省 false。设计表里标 "—" 的工具如果不显式写 `DestructiveHint: ptr(false)`,
  客户端会当成破坏性工具。建议:readOnly 工具只设 `ReadOnlyHint: true`;`send_message`/`reply_task` 设
  `OpenWorldHint: ptr(true), DestructiveHint: ptr(false)`;`submit_payment` 设 `DestructiveHint: ptr(true)`;`reject_payment` 设
  `DestructiveHint: ptr(false)`;`cancel_task` 由实现者定,写进测试。
- **路径白名单**:`Control` 接口(`mcpserv.go:34-36`)不限制路径。在 `New` 内包一层 `guarded{c}`,只放行
  `/find /agents/card /tasks/{send,get,list,wait,cancel,reply,pay} /balance /evidence /status /inbound/pending`
  (最终以 C5 的路由名为准)。§8.6 要求 MCP 不调用 `/tasks/pay-manual`、`/x402-authorize`、`/redeem`、`/delegate`
  (带 `pay`),用测试钉住。
- `wait_task` 超时:`controlClient` 固定 15 分钟(`cmd/anet/mcp.go:46-47`)。建议 `wait_task` 参数 `timeout_seconds`
  (上限 300),控制面 `/tasks/wait` 服从它;各客户端的工具超时要大于这个值(Codex `tool_timeout_sec`、Hermes `timeout`,见 §5)。
- `reply_task`:设计要求始终注册(覆盖 0008 "按策略隐藏"的建议)。

### 3.3 测试与夹具

- `mcpserv_test.go:14-40` `fakeControl`(记录 path/body、按 path 回放固定 JSON)与 `:46-60` `connect`(内存传输,走 SDK 的
  schema 与分派)直接复用。
- 要改/加的测试:
  - `TestTheToolSurfaceIsWhatWePromise`(`:65-112`):换成 13 个新名与总数;描述断言改为新文本,并断言含
    "UNVERIFIED" 那句。
  - 注解表测试:逐工具断言 hint。
  - 路径白名单测试:遍历全部工具调用后,`fakeControl.calls` 的 path 都在白名单里,且不含 pay-manual/x402-authorize/redeem/delegate。
    mutation:把 `/tasks/pay-manual` 加进白名单,测试须红。
  - SI-6 契约测试:fake 返回 a2ashape 金标 Task JSON,调用 `get_task`,把 `StructuredContent` 反序列化为 a2a-go `a2a.Task`;
    能力任务终态缺 `anet.effect_status`、文本任务 `completed` 缺 `anet.receipt_verified` 即失败。放在**外部测试包**
    (`package mcpserv_test`)或 a2ashape 的测试里。`go list -deps` 不含测试依赖,SI-8 检查不受影响,但 a2a-go 要先进
    ANet `go.mod`(`wt/a2ashape` 正在改 `go.mod`)。
- 脚本(§17 "MCP 探针改新工具名"):`scripts/mcp-probe.py:60-77`、`scripts/joint-fleet.sh:294-302`、
  `scripts/prodtest.sh:1138-1195`(经 `scripts/mcpcall.py`)。`mcpcall.py list|call <tool> '<json>'` 可直接对真 daemon 验证。
- 文档:`docs/GUIDE-zh.md:241` 的工具清单和"`install --agent claude` 会顺手把 MCP 配置写好"(与代码不符),
  `docs/SUITE-TODO-zh.md`;`docs/site/*.html` 由 `docs/site/build-all.sh` 从 `docs/*-zh.md` 生成,改源文件后重跑,不要手改 HTML。

---

## 4. CLI 命令

### 4.1 cmd/anet 的结构约束(新增任何命令都要满足)

- 顶层分派在 `main.go:57-105`(不需要 daemon 的命令:`mcp` `verify` `logs` `install`)与 `runClient` 的 switch
  (`main.go:1183` 起,需要 daemon)。`init` `doctor` `agents` `update` 放前者;`pay` `payments` `payees` 放后者。
- `knownflags_test.go:182` **按行解析 `main.go`** 的 `case "x":` 与其中的 `flags["…"]`:新命令的 `case` 必须写在
  `main.go`,`knownFlags`(`main.go:1062-1122`)要登记全部 flag;在别的文件里读的 flag 不会被检查,但 `checkFlags` 仍会拒绝未登记的。
- `release_qa_h_test.go:93` 要求每个分派的命令都出现在 `usageAllText()`(`main.go:292-345`);`grpNetwork`(`:148-176`)是引导页清单。
- **陷阱:`splitFlags`(`main.go:1160-1181`)把 `--x` 后面第一个不以 `--` 开头的参数当成它的值。**
  `anet agents wire --all claude` 会得到 `flags["all"]="claude"`、无位置参数。布尔 flag(`--all` `--refresh` `--json`
  `--reject`)要单独解析;可重复的 `--a2a <aid>` 仿照 `extractAttach`(`main.go:919`)。
- **陷阱:未知命令落到 `runClient`,在没有 daemon 时打印"can't reach your daemon",并经 `LocalControlAddr` → `LoadConfig`
  顺手创建 `config.json`。** [实测] 0.1.10 执行 `anet update`、`anet doctor --json` 都是这个结果,数据目录里多出了 config.json。
  这影响 0.1.x 用户按 llms.txt 执行 `anet update`(见 §6.6)。
- TTY 确认复用 `peers.go:39` `openTTY` / `:43` `confirmOnTTY`;测试用 `peers_test.go:27-38` `withTTY`(`""` = 无终端)与
  `:41-58` `recordingDaemon`(记录请求路径的假控制面)。

### 4.2 `anet init`(§13.1、SI-5)

- 建议:`cmd/anet` 中 `case "init":` → `daemon.InitLayout(layout) (InitReport, error)`(新文件,`wt/cli` 的 `setup.go` 可能就是它)。
  1. `EnsureLayoutInit(l)`(`identities.go:316`)建配置、分配端口、生成身份。
  2. 读**原始 JSON**(map)判断哪些 SI-5 键缺失:`LoadConfig` 会先在内存里补 inbound(`config.go:194` `migrateInbound`),
     用它判断缺失会把"缺"看成"有"。
  3. 只补缺失键,写回;已有值与安全默认不同则进入报告,不覆盖。
  4. 以 0600 创建空的 `peers.allow` `peers.deny` `peers.trust` `payees.allow`(已存在则不动)。路径解析同
     `d.peerFile`(`inbound.go:263-268`,相对数据目录)——该函数是 `*Daemon` 方法,init 不起 daemon,需导出一个无接收者版本。
- **不要写的键**:
  - `auto_reply`:块存在即启动自动回复循环(`daemon.go:231-233`)。SI-5 的 `auto_reply.untrusted=off` 以"块不存在 ⇒ off"满足,
    doctor 报告有效值(`InboundStatus` 已如此处理,`inbound.go:772-774`)。
  - `modules.a2a`:§11.1 [C43],写了之后 `no_a2a` 变体加载配置会失败。
- `payments` 块等 C3 定义类型后补;同时让 `DefaultConfig()`(`config.go:127-131`)也带上它,使 `anet up`、`anet id new`
  (都走 `freshConfig`)与 `anet init` 得到同一份默认值。
- 测试:临时数据目录连跑两次,第二次报告为空;预置 `inbound.policy=open` 时报告差异、不改值;预置 peers 文件内容不被截断。

### 4.3 `anet doctor [--json]`(§13.1、SI-5)

- 不依赖 daemon:SI-5 的验收是在 `anet init` 之后、未启动 daemon 时断言。数据从磁盘读;daemon 在跑时再加 `/status`
  与 `/peers/list` 的实时值并比较。需要导出:不创建文件的配置读取(`identities.go:155` `loadConfigNoCreate` 现未导出;
  `LoadConfig` 会创建文件)、`readPeerFile`(`inbound.go:273`)。
- `--json` 建议按配置路径嵌套,测试按路径逐键断言,mutation 逐键改回:

```json
{
  "version": {"version":"0.2.0","commit":"…","built_at":"…","modules":["…"],"release_signature":"ok|unverified|absent"},
  "identity": {"aid":"…","data_dir":"…"},
  "control": {"addr":"127.0.0.1:39811","daemon_running":true},
  "a2a": {"addr":"127.0.0.1:…","token_file_mode":"0600"},
  "hub": {"url":"…","registered":true},
  "safe_defaults": {
    "inbound.policy":"closed", "peers.allow":[], "peers.trust":[], "inbound.public_capabilities":[],
    "auto_reply.untrusted":"off", "payments.auto_max":0, "payments.agent_max":0, "payments.agent_daily_max":0,
    "payments.payees_file":"payees.allow", "payments.payees":[]
  },
  "safe_defaults_ok": true,
  "agents": [{"tool":"claude","wired":true,"config":"~/.claude.json","handshake":"ok","tools":13}],
  "warnings": [], "errors": []
}
```

- 各检查项的数据来源:
  - 版本:`daemon.Version/BuildCommit/BuildAt`(`paths.go:166-180`)、`module.Compiled()`。"签名":建议 install.sh 与 `anet update`
    把验过的 `release.json` 与 `.sig` 存到 `AnetHome()/release/`,doctor 重新验签,并把 `os.Executable()` 的 sha256 与清单比对。
  - A2A 地址与令牌(D1):`a2a_addr.txt`、`a2a_token.txt`,`Layout` 里还没有这两个路径方法(`paths.go:205-214`)。
  - 各工具接入与握手:见 §5.7。
  - Hermes 配置文件权限:含令牌时须 0600。
  - "令牌是否过期":设计未定义。按 §13.1 上下文理解为 Hermes `a2a_agents` 里的令牌与当前 `a2a_token.txt` 不一致
    (需 `--refresh`);实现前确认。
  - `a2a_agents` 各 URL 端口与 `a2a_addr.txt` 不一致 → 提示 `anet agents wire --refresh`。
  - `auto_reply.untrusted=sandbox` 且配置了 `auto_reply.api_key` → 警告。
- 人类可读输出参考 0008 §8.3 状态块。有 `errors` 时退出码非 0。
- no_mcp 构建:工具接入部分输出 "mcp not compiled in"。握手代码依赖 go-sdk,放在 `!no_mcp` 文件里,另写桩。

### 4.4 `anet agents wire|unwire [--all|<tool>…] [--refresh] [--a2a <aid>…]`

见 §5。CLI 部分:`case "agents":` 在 `main.go` 顶层(不需要 daemon;`--a2a` 读 D1 状态文件)。`knownFlags["agents"] = {"all","refresh","a2a"}`。
`anet install --agent X` 建议保留一个版本作为 `agents wire X` 的别名并提示弃用:hub `llms.txt:326`、`:357` 与
`cmd/anet/agentlist_test.go:15-60` 仍引用它。OpenClaw 在注册表里(`agents.go:66`)但不在设计的 5 个工具中:定为"只写
persona、不注册 MCP"或从 wire 中去掉,须明确。

### 4.5 `anet pay <ix>`(§8.6 人工档)

- `runClient` 中 `case "pay":`。流程:`POST /tasks/get {task_id}` → 取 `status.message.metadata["x402.payment.required"].accepts`
  → 在 TTY 上显示收款方、金额、网络、scheme、有效期(**从 daemon 存的报价读,不从命令行参数读**)→ 多个选项时在 TTY 上选
  或 `--option N` → `confirmOnTTY` → `POST /tasks/pay-manual {task_id, decision:"submit", accept:<所选项原样>}`。
- `--reject`:不花钱,不必 TTY,走 `/tasks/pay {decision:"reject"}`。
- 测试:`withTTY("")` 时不发任何请求;`withTTY("no")` 不发 `/tasks/pay-manual`;`recordingDaemon` 断言路径。mutation:去掉 TTY 检查须红。

### 4.6 支出上限与 payees(§8.6)

- 设计只写"修改支出上限的命令同样要求 TTY"和"用户先把演示 AID 加入 payees 并在 TTY 上放开 agent 档上限",没给命令名。
  建议 `anet payments [show|set --auto-max N --agent-max N --agent-daily-max N --explicit-max N --daily-max N]`、
  `anet payees list|allow|remove <aid>`;调高上限与加入 payee 须 TTY,调低与删除不需要(与 `inbound policy closed` 一致)。
  路由由 C3 定(建议 `/payments/get` `/payments/set` `/payees/*`),写入须记 `anet.policy.changed`(简报 03 §7.9 列为缺)。
- 这些路由不得进入控制台会话白名单(`ctlsec.go` `sessionRoutes`;遍历测试会自动覆盖新路由)。

### 4.7 `anet redeem` 加 TTY

`main.go:1443-1460` 在 `c.do("/redeem", …)` 前加 `confirmOnTTY("Redeem N credits to your hub …?")`。`x402-authorize`
属 gateway 档(`explicit_max`),设计没要求 TTY;它把头值打到 stdout 供管道使用,如加 TTY 提示须写到 `/dev/tty` 而非 stdout。

### 4.8 `anet update`

见 §6.5。`case "update":` 在顶层(不需要 daemon)。

### 4.9 `anet audit`

属 §14,缺口与方案见简报 `03-control.md`。

---

## 5. `anet agents wire`:五个工具的配置要点

### 5.1 公共框架(`internal/agentwire`,`//go:build !no_mcp`)

- 建议接口:

```go
type Env struct {
    Home, AnetBin, DataDir string        // AnetBin、DataDir 均为绝对路径
    A2AAddr, A2AToken      string        // D1;--a2a 才用
    LookPath func(string) (string, error)
    Run      func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
    Now      func() time.Time
}
type Tool interface {
    ID() string
    Detect(Env) Detection                 // 可执行文件在 PATH 且配置目录存在
    Wire(Env, Options) ([]Change, error)
    Unwire(Env) ([]Change, error)
    Status(Env) Status                    // 供 doctor
}
```

  `Run`/`LookPath` 可注入,测试用 fake HOME + 桩程序。
- 检测:可执行文件在 PATH **且**配置目录存在(去掉 `install.go:89` 的 `/opt/data`)。`--all` 只接已检测到的;显式点名的工具,
  配置目录缺失时可创建,二进制缺失时 Claude(要用它的 CLI)报错,其他报告后继续。
- 写入前备份为 `<file>.anet-bak-<UTC 时间戳>`,保留原权限;所有写入走临时文件 + rename(`paths.go:226` `writeFileAtomic`
  写的是固定名 `path+".tmp"`,并发两个 wire 会互相覆盖,agentwire 用 `os.CreateTemp` 更稳)。
- 所有权记录:JSON 与 YAML 放不了(或保不住,见 5.6)标记注释。建议在 `AnetHome()/agentwire.json` 记录每处写入
  `{tool, file, key, value_sha256, backup, at}`。`unwire` 只删"当前值 == 记录值"的条目,被用户改过的报告后跳过。
- 写入内容一律为绝对路径 `AnetBin` 与 `env.ANET_DATA_DIR=<abs layout.Root>`。`runMCP` 对 `ANET_DATA_DIR` 视为显式选择
  (`identities.go:148` `SelectionIsExplicit`)并走严格解析,不会连到别的身份。
  陷阱:`os.Executable()` 在 Linux 上返回解析过符号链接的路径。若用户经 `/usr/local/bin/anet → …/versions/x/anet` 安装,
  写进配置的会是版本目录,升级后仍指向旧版。优先用 `exec.LookPath("anet")`,且仅当它与 `os.Executable()` 指向同一文件时采用。
- 不引入 YAML/TOML 依赖(ANet `go.mod` 直接依赖只有 5 个)。TOML/YAML 用托管块做行级编辑,遇到非托管的同名条目就报告冲突并停止。
- `ci.yml:150` 的 mcp 符号模式改为 `internal/mcpserv|internal/agentwire`;`cmd/anet` 增加 `agents_stub.go`(`no_mcp`)。

### 5.2 Claude Code — [实测] 本机 2.1.283,fake HOME

- 注册:`claude mcp add -s user anet -e ANET_DATA_DIR=<abs> -- <abs>/anet mcp`。写入 `~/.claude.json` 顶层
  `mcpServers.anet = {"type":"stdio","command":"<abs>/anet","args":["mcp"],"env":{"ANET_DATA_DIR":"<abs>"}}`;文件为 0600;
  Claude 自己另存备份到 `~/.claude/backups/.claude.json.backup.<ms>`。
- 已存在时 `add` **退出码 1**,输出 "MCP server anet already exists in user config"。幂等做法:先读 `~/.claude.json`,
  与期望值相同就不动;不同(或 `--refresh`)时先 `claude mcp remove -s user anet` 再 `add`。
- 不存在时 `remove` **退出码 1**,输出 `No MCP server named "anet" in user scope`,unwire 要把它当成功。
- `claude mcp get anet` 在连接失败时**退出码仍为 0**(输出 "Status: ✘ Failed to connect")。doctor 不能用它的退出码,
  直接读 `~/.claude.json` 比对,握手自己做(5.7)。
- `-e` 是可变参数(`--env <env...>`),命令前必须有 `--`。
- 技能:`~/.claude/skills/anet/SKILL.md`,frontmatter 为 `name:`、`description:`(本机 `~/.claude/plugins/.../SKILL.md` 同格式)。
- 迁移:删除旧版 `anet install --agent claude` 写进 `~/.claude/CLAUDE.md` 的托管块(标记 `install.go:16-17`);
  每个会话都会加载这约 3.3 KB。

### 5.3 Codex — `~/.codex/config.toml`(`$CODEX_HOME` 优先)

- 本机有 `~/.codex/config.toml`(含 `[projects."…"]`、`[plugins."…"]` 表),但 PATH 上没有 `codex`,CLI 行为无法实测。
- 字段依据 [本地源码] `/data/projs/anetchat/refs/hermes-agent/hermes_cli/codex_runtime_plugin_migration.py:18-25,251-300`
  (Hermes 把自身 MCP 配置迁移到 Codex 的代码):stdio 为 `command/args/env`,超时为 `startup_timeout_sec`、`tool_timeout_sec`。
  托管块:

```toml
# >>> anet (managed by `anet agents wire`; edits inside are overwritten) >>>
[mcp_servers.anet]
command = "/abs/anet"
args = ["mcp"]
env = { ANET_DATA_DIR = "/abs/.anet" }
startup_timeout_sec = 30
tool_timeout_sec = 600
# <<< anet <<<
```

- 块以表头开始,追加在文件末尾是安全的。上述 Hermes 代码指出的"根键写在表后会变成表内键"只影响根键,本块没有根键。
- `env` 用内联表写在同一个表里,不另开 `[mcp_servers.anet.env]`,避免块外残留子表。
- 冲突检测(块外出现任一即报告并停止):`[mcp_servers.anet]`、`[mcp_servers."anet"]`、`[mcp_servers.anet.*]`、
  `[mcp_servers]` 表内的 `anet =`/`anet.`、根级 `mcp_servers.anet…` 点号键。TOML 重复表头会让 Codex 整个配置解析失败。
  判断表头时借鉴同文件 `_looks_like_table_header` 的做法,排除多行数组续行。
- 字符串转义:路径含 `"` 或 `\` 时用基本字符串加转义。
- 说明文本:[本地源码] `~/.codex/skills/.system/skill-installer/SKILL.md:48` 表明 Codex 从 `$CODEX_HOME/skills/<name>/SKILL.md`
  读技能(frontmatter `name/description`)。可写 `~/.codex/skills/anet/SKILL.md`,并把 `~/.codex/AGENTS.md` 的旧块
  (`agents_install.go:46-61`)压缩为短指针或删除。

### 5.4 Cursor — `~/.cursor/mcp.json` [推断]

- `{"mcpServers":{"anet":{"command":"/abs/anet","args":["mcp"],"env":{"ANET_DATA_DIR":"/abs/.anet"}}}}`。本机无 Cursor。
- 按 JSON 对象合并,保留其他键;`encoding/json` 往返会丢键序(内容不变)。`wt/agentwire` 的 `jsonobj.go` 看起来是保序编辑器。
- `~/.cursor/rules/agentnetwork-anet.mdc`(`agents_install.go:12-26`)很可能不会被 Cursor 读取(用户级规则在设置界面里,0008 §4.2 第 5 条)。
  以 MCP `Instructions` 为主要说明渠道。

### 5.5 opencode — `~/.config/opencode/opencode.json` [推断]

- `{"mcp":{"anet":{"type":"local","command":["/abs/anet","mcp"],"enabled":true,"environment":{"ANET_DATA_DIR":"/abs/.anet"}}}}`。
  注意 `command` 是含参数的数组,环境键名是 `environment`。本机无 opencode,也找不到样例配置。
- 存在 `opencode.jsonc` 或文件带注释时 `encoding/json` 会失败:报告并打印片段让用户手工添加,不要重写(会丢注释)。
- 说明文本:`~/.config/opencode/AGENTS.md` 托管块(`agents_install.go:67-82`)。

### 5.6 Hermes — `$HERMES_HOME/config.yaml`,否则 `~/.hermes/config.yaml`

- 位置与字段依据 [本地源码] `/data/projs/anetchat/refs/hermes-agent`(2026-05-25):`hermes_constants.get_hermes_home`
  (`HERMES_HOME` 否则 `~/.hermes`);`hermes_cli/mcp_config.py:226-290` 的条目为 `command/args/env`,另有
  `timeout`、`connect_timeout`、`enabled`、`tools.include`。`hermes mcp add` 是交互式的(`_confirm`、选工具),只能直接写文件。

```yaml
mcp_servers:
  anet:
    command: /abs/anet
    args: [mcp]
    env:
      ANET_DATA_DIR: /abs/.anet
    connect_timeout: 30
    timeout: 600
```

- **陷阱 1:Hermes 保存配置时整体重写 YAML。** `hermes_cli/config.py:4603` `save_config` → `utils.atomic_yaml_write`
  (PyYAML dump,`sort_keys=False`)会丢掉全部注释,anet 的托管标记在用户下一次 `hermes config …` 后就没了。
  `unwire`/`--refresh` 不能只靠标记,要按 5.1 的所有权记录(或 `command == AnetBin && args == [mcp]`)识别自己的条目。
  `_preserve_file_mode` 会保留文件权限,所以一旦设为 0600 会一直保持。
- **陷阱 2:重复顶层键。** 文件已有 `mcp_servers:` 时再追加一个顶层 `mcp_servers:` 块,PyYAML 会**静默只保留后一个**,
  用户原有的 MCP 服务器从 Hermes 视野里消失。必须插入到已有映射之内(按其子项缩进),`mcp_servers: {}` 这类流式写法或无法识别的
  结构一律报告冲突并打印片段。`a2a_agents` 同理。
- 不用 `sh -c`(Hermes `mcp_security.py` 会把"shell 解释器 + 网络/持久化"判为可疑并置 `enabled: false`,0008 §7.2);
  doctor 报告 `enabled: false`。
- `--a2a <aid>…`:设计给出 `{url: http://<a2a_addr>/a2a/v1/agents/<aid>, auth:{type: bearer, token}, timeout: 3600}`。
  **本机三份 hermes-agent 副本都没有 a2a 插件**(`plugins/platforms` 下无 `a2a`;0011 §2.5 引用的 `Refs/hermes-agent/plugins/platforms/a2a/`
  已不在本机)。`a2a_agents` 是列表还是映射、键名、`auth` 结构都无法核实,实现前须对照 Hermes 上游当前版本。写令牌后文件设 0600。
- 说明文本:`SOUL.md` 托管块(`install.go:103-116` `applyHermes` 可复用)。

### 5.7 握手与 fake HOME 测试

- doctor 握手:用 go-sdk `mcp.CommandTransport{Command: exec.Command(AnetBin, "mcp")}`(`cmd.go:20`),`Env` 设为配置里写的
  `ANET_DATA_DIR`,完成 initialize → `ListTools`,核对工具数与名字。daemon 不在时 `anet mcp` 会拒绝启动(`mcp.go:49-52`),
  doctor 报告"daemon 未运行"而非"握手失败"。
- 测试:`t.Setenv("HOME", t.TempDir())`(参考 `release_qa_install_test.go:19-65`);Claude 用 PATH 中的桩 `claude` 脚本
  (模拟 add 已存在时退出码 1、remove 缺失时退出码 1)。每种格式都做 写入 → 读回(按该工具的解析规则:TOML 的表头唯一性、
  YAML 顶层键唯一性、JSON 可解析)→ 再次 wire 不变 → unwire 恢复,以及预置非托管同名条目时报冲突不写入。
  现有 `release_qa_install_test.go`、`install_test.go` 只断言"写了含 anet 的文件",persona 改写后要同步更新。

---

## 6. 发布签名与 install.sh(§13.2)

### 6.1 现状

- `deploy/release/build-release.sh`:`:63` 只做 `git diff --quiet`(已暂存或未跟踪的改动能通过);`:64` `BUILT=$(date …)`
  使构建不可复现;`:85` 只记原始二进制的 sha256;`:110` 只对宿主平台做 `module/shell` 符号自检;全文无签名。
- `deploy/release/install.sh`(254 行):顶层语句直接执行(未包进函数);`:50`/`:65-66` 下载源接受任意 scheme,curl 无
  `--proto '=https'`;`:130` `VERSION` 不受校验;`:134` 先 gunzip,`:139-154` 再校验,缺文件、缺条目、缺工具都只警告;
  `:74` `--help` 在 `curl|sh` 下无输出;无降级、过期、模块集合检查;安装后不跑 init/wire/doctor。
- **install.sh 有四份不同内容**:`ANet/deploy/release/install.sh`;`ANetHub/deploy/install.sh`(166 行旧版,无 `--hub/--shell`);
  `/data/projs/anet-website/public/install.sh`(与 `site2.0/public`、`site2.0/dist` 相同,从 npm registry 装 `@agentnetwork`,
  是另一代安装器);`anet-website/deploy/nginx.conf:99-103` 从站点 `public/` 提供 apex 的 `/install.sh`。
  `ANetHub/deploy/nginx-hub.conf:34-45` 注释说 hub 的 `/install.sh` 与 `/dl/` "由 deploy/release/publish.sh 填充",该脚本在三仓都不存在。
  以 `ANet/deploy/release/install.sh` 为唯一来源,发布脚本把它连同 `.sig` 复制到 apex 与 hub 两处,删除 ANetHub 的副本。
  生产上 apex 实际提供的是哪一份,本机无法核实。
- `scripts/build.sh:35` 用 `-s -w` 去掉了符号表,这种二进制上的 `go tool nm` 符号检查会空过(发布脚本 `:53` 只用 `-w`)。

### 6.2 `release.json` 格式(建议)

```json
{"schema":"anet-release/1","version":"0.2.0","commit":"<40位>","built_at":"<提交时间 UTC>",
 "published_at":"…","expires_at":"…","expires_epoch":1790000000,"go":"go1.26.6",
 "next_key_fingerprint":"SHA256:…",
 "variants":{"default":{"tags":"","modules":["…"]},"shell":{"tags":"shell","modules":["…"]}},
 "assets":[
{"name":"anet-linux-amd64","variant":"default","os":"linux","arch":"amd64","gz_sha256":"…","sha256":"…","gz_size":0,"size":0},
 …]}
```

- 由 Go 小工具生成,每个资产对象独占一行、键序固定。install.sh 没有 jq,只能用 `grep '"name":"anet-linux-amd64"'` 加 `sed`
  取字段;签名覆盖原字节,固定排版本身受签名保护,解析要严格(整行匹配,匹配不到或匹配到多行都失败)。
- 带 `expires_epoch`:POSIX sh 里解析 RFC3339 不可移植(GNU `date -d` 与 BSD `date -j -f` 不兼容),`date +%s` 比整数可移植。
- `modules` 必须与 `anet version` 的 `modules:` 行一致。当前默认构建输出 `anetlink,blackboard,cas,mcp,org,p2p,service,taskboard,x402`
  [实测]。§16 把 taskboard 改为加法、D1 加入 `a2a` 后会变,清单从构建产物里读出,不要手写。
- 签名:`ssh-keygen -Y sign -f "$ANET_RELEASE_KEY" -n anet-release@agentnetwork.org.cn release.json` 生成 `release.json.sig`;
  `install.sh` 同样签出 `install.sh.sig`。签完立即用仓库里的 `allowed_signers` 做 `ssh-keygen -Y verify` 自检,防止用错钥。
- `allowed_signers` 一行:`anet-release@agentnetwork.org.cn namespaces="anet-release@agentnetwork.org.cn" ssh-ed25519 AAAA…`
  (`wt/release/ANet/internal/release/allowed_signers` 已是这种格式)。

### 6.3 `build-release.sh` 改动

1. 脏树:`git diff --quiet HEAD && test -z "$(git status --porcelain)"`。
2. `BUILT=$(TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd)`;清单写完整 commit。
3. `checksums.txt` 与 `release.json` 同时记录 `.gz` 与原始文件的 sha256。
4. 去掉 `:110` 的平台过滤,`go tool nm` 能读任意平台的 Go 二进制。
5. 各变体的模块集合:构建一份宿主平台二进制执行 `version` 取 `modules:`(同一 tag 集跨平台一致)。
6. 生成并签名 `release.json`、`install.sh`;缺 `ANET_RELEASE_KEY` 时拒绝产出正式发布(可留 `--unsigned-dev` 只供本地)。
7. 新增 `deploy/release/publish.sh`(或修正 nginx 注释):把 `dist/` 推到 hub 与 apex 两处。部署属阶段 G,执行前征求同意。

### 6.4 install.sh(逐条对应 §13.2)

- 全部逻辑放进函数,最后一行 `main "$@"`,防止 `curl|sh` 下载中断时执行半截。
- curl 一律 `--proto '=https' --tlsv1.2 -fsSL`;`--base`/`ANET_INSTALL_BASE` 只接受 `https://`。离线与测试用 `--from-dir DIR`
  (本地文件,仍验签);不要提供跳过验签的开关。
- 内置 `allowed_signers` 一行与指纹,写入临时文件后执行
  `ssh-keygen -Y verify -f "$tmp/allowed_signers" -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s release.json.sig < release.json`。
  没有 `ssh-keygen` 或版本低于 8.1(不支持 `-Y`)时失败退出,并提示安装 openssh-client(见风险 7)。
- 依次检查,任一失败即退出且不触碰已装版本:验签 → `expires_epoch > $(date +%s)` → 版本不低于已装版本(已装版本取
  `"$DEST" version` 第一行;按 MAJOR.MINOR.PATCH 用 `IFS=.` 逐段比较,`sort -V` 不是 POSIX)→ 按平台与变体取资产行 →
  下载 `.gz` 并校验 `gz_sha256` → gunzip → 校验 `sha256` → 执行临时文件的 `version`,`modules:` 与清单一致,默认变体不含 `shell`
  → 原子替换(沿用 `:156-171`)。没有 sha256 工具即失败,不再警告放行。
- 保存 `release.json` 与 `.sig` 到 `~/.anet/release/`,供 doctor 复核(4.3)。
- 安装后:`anet init`;有 `--hub` 时保留现有 `up` + `hub-register` 流程(`:206-240`);`--agents[=list]` 时 `anet agents wire`;
  最后 `anet doctor` 打印状态块,再给一个免费官方 agent 示例(官方 AID 依赖阶段 E)。
- `--help` 改为打印脚本内的 heredoc,不再 `sed "$0"`。

### 6.5 `anet update`

- 标准库实现 SSHSIG 验证:解 armor → 检查 magic `SSHSIG`、版本 1、公钥 blob、namespace、hash 算法 → 签名原文为
  `"SSHSIG" ‖ string(namespace) ‖ string("") ‖ string("sha512") ‖ string(SHA-512(msg))` → `ed25519.Verify`。
  公钥与 install.sh 内置的是同一把;`wt/release/internal/release/sshsig.go` 正在做。
- 流程:取 `release.json`(+`.sig`)→ 验签 → 过期 → 版本必须高于当前 → 按 `runtime.GOOS/GOARCH` 选资产,变体按当前二进制判断
  (`module.Compiled()` 含 `shell` 即 shell 变体)→ 下载、两级 sha256 → 写到可执行文件同目录的临时文件 → 执行其 `version`
  核对模块集合 → `rename` 覆盖 `os.Executable()` → 提示 `anet stop --all && anet up --all`。
- 密钥轮换:清单的 `next_key_fingerprint` 要在本地持久化(如 `~/.anet/release/trust.json`),下一次清单若由该指纹的新钥签名则接受。
- 测试:`httptest` 提供清单与资产;篡改签名、过期、降级、gz 哈希错、原始哈希错、模块集合不符各一例,均断言原文件未被替换。

### 6.6 公钥分发与 llms.txt

- 公钥与指纹写入 `SECURITY.md`、`README.md`(安装段在 `:62-77`、`:142-143`)、官网文档与 install.sh。hub
  `llms.txt:115-119` 已经要求用户从 SECURITY.md 和 README 抄 `allowed_signers` 行,这两处目前都没有这一行。
- **0.1.x → 0.2.0 的鸡生蛋问题**:`llms.txt:84-106` 让已安装用户执行 `anet update`,但 0.1.x 没有这个命令,只会得到误导性的
  "can't reach your daemon"(见 4.1)。Step 0 要补一句:`anet update` 报未知命令或连不上 daemon(anet < 0.2.0)时,走安装器。
- `ANet/internal/version` 的 `V` 仍是 `0.1.10`,而 hub 页面已写"需要 anet >= 0.2.0"(`llms.txt:7`);版本号随发布一起改。
- 签名私钥:设计定为"本期在 ink93 生成开发用钥,正式发布前由产品负责人决定保管方式"。本机 `~/.ssh` 下没有专用发布钥,
  `wt/release` 的公钥对应的私钥在哪里需要向实现者确认;私钥不得进仓库。

### 6.7 §15 官方清单

"发布签名密钥签署官方清单,随二进制打包;`list_agents` 与代理卡片据此标注 `anet.official: true`"。与 6.5 共用验签代码:
`internal/release` 提供 `VerifyOfficials(raw, sig)`,清单用 `go:embed` 打包。本分支无任何实现。

---

## 7. persona / SKILL 文本(§13.1 末条)

- 现文本 `install.go:23-68`:"In v0.1 every agent connects through the official Hub"、"pricing is display-only text in v0.1
  (no settlement yet)"、"Describe YOURSELF… don't wait for a human"、"Provide work to others (earn…)",与默认安全相反。
- 重写要点:默认拒绝一切委派;允许名单与信任名单只能由操作者在终端改(agent 不要尝试,`anet peers allow` 无 TTY 会失败);
  支出三档与默认 0,`submit_payment` 在操作者放开 agent 档之前会被拒;长任务用 `send_message` + `wait_task`;"completed 且
  `effect_status=UNVERIFIED` 不等于成功";`receipt_verified` 三值的含义;CLI 作为后备。
- 同一份文本三处复用:MCP `Instructions`(短版)、Claude/Codex 的 SKILL.md、AGENTS.md/SOUL.md 托管块(短指针,不超过约 600 字节)。
- 关于"hub 看不到内容"的表述:按 §20 对外陈述更正表,只能在 E2E 部署后出现。persona 随 0.2.0 二进制发布,而 0.2.0 只连
  wire-2 hub,可以写,但措辞与 §21 已知局限一致(hub 仍看得到谁发给谁、何时、多大)。

---

## 8. 测试夹具速查

| 夹具 | 位置 | 用途 |
|---|---|---|
| `fakeControl` + `connect` | `internal/mcpserv/mcpserv_test.go:14-60` | MCP 工具面,走 SDK 的 schema 与分派 |
| `withTTY` / `fakeTTY` | `cmd/anet/peers_test.go:14-38` | 模拟有终端(回答任意字符串)或无终端 |
| `recordingDaemon` | `cmd/anet/peers_test.go:41-58` | 记录 CLI 发出的控制面路径 |
| fake HOME | `internal/daemon/release_qa_install_test.go:19-65`(`t.Setenv("HOME")`) | agentwire 往返 |
| 帮助/flag 一致性 | `cmd/anet/knownflags_test.go:88,130,182`,`release_qa_h_test.go:93` | 新命令必须登记 |
| MCP 线上探针 | `scripts/mcpcall.py list|call`,`scripts/mcp-probe.py` | 对真 daemon 验工具名 |
| no_mcp 声明 | `cmd/anet/nomcp_test.go:12`,`mcp_present_test.go:18` | agentwire 桩也要遵守 |

---

## 9. 风险与矛盾

见结构化返回的 `risks`。要点:并行工作树未提交且会在 `main.go` 冲突;hub 注册表 API 与 JWKS 实际不存在(与 0013 的
"B4 已完成"不符);MCP 大半依赖 C5/C3;Hermes `a2a_agents` 格式无法本地核实,且 Hermes 重写 YAML 会抹掉托管标记;
install.sh 依赖 `ssh-keygen -Y`;0.1.x 没有 `anet update`;`anet init` 不能写 `auto_reply` 块。
