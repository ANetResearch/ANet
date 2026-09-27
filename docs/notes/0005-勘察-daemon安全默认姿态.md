# 05 · anet daemon 安全默认姿态调研(secure-by-default)

调研对象:`/data/projs/anet-oss/ANet`,提交 `840b8ea`;引用到的对侧代码:`ANetHub` `c4d08da`、`ANetCore` `59086ea`;A2A 规范 `Refs/a2a`、`Refs/a2a-x402-spec-v0.2.md`。
本阶段只读不改。所有事实性陈述给出 `文件:行`。标记约定:

- 未加标记的陈述:通过阅读代码核实。
- **[推断]**:基于代码结构或外部工具文档的推断,未在本机运行验证。
- **[外部]**:依赖第三方 CLI(cursor / codex / opencode / hermes / openclaw)的行为说明,本机仅装有 `claude`,其余未核实。

---

## 1. 结论摘要

1. **入站委派默认接受。** `AcceptDelegations` 为 nil 时视为接受(`internal/daemon/config.go:94`),而且 `DefaultConfig` 本身就把它设为 `true`(`config.go:114-117`),新数据目录首次加载时会把这个 `true` 显式写进 `config.json`(`config.go:131-139`、`config.go:156-168`)。因此磁盘上的 `"accept_delegations": true` 无法区分"运营者有意打开"和"安装时写入的默认值"。
2. **任何人都能成为委派方。** 委派只校验"签名与请求自带的 KEL 一致"(`delegation.go:496`),AID 是自证明的,不存在按对端的允许名单。唯一带调用方允许名单的是 `shell` 模块(`module/shell/provider.go:85-110`);`service`、`anetlink` 模块没有调用方检查(`module/` 下除 shell、x402 外无 `CallerAID` 引用)。
3. **后续消息没有发送方认证。** hub 的 `/relay/send` 不认证、不校验 `from_aid`(ANetHub `internal/aghub/server.go:1081-1083`、`:882-930`);`ChatMsg` 不带签名字段(ANetCore `delegation/delegation.go:133-179`);daemon 的 `ingestMessage` 只检查 interaction 是否存在,不检查 `fromAID == ix.PeerAID`(`delegation.go:636-686`,检查点在 `:642`)。结果:知道 interaction id 的任何一方(包括 hub 本身)可以向进行中的对话注入"对方消息"或伪造结束握手。按产品决定 hub 只是传输层、不受信任,这一条是允许名单能否成立的前提。
4. **访客模式默认开启,且由 hub 代签。** daemon 默认向 hub 声明接待 5 条访客消息(`config.go:89`、`:98-106`);hub 用自己的 guest broker 身份替匿名浏览器签发委派,随机路由到任何 `guest_quota>0` 的 agent(ANetHub `internal/aghub/guest.go:3-16`、`:41`、`:462-480`、`:594`)。这条路径与"hub 不得看到任务内容"(决定 2)和"默认不接受任何人的委派"(决定 3)同时冲突。
5. **exec 自动回复把对端文本交给可执行 shell 的本地编码 agent。** 默认 exec 提示词明写"you may run shell commands"(`autoreply_exec.go:16-17`);工作目录默认是身份数据目录(`autoreply_exec.go:155-158`,`autoreply.go:151`),该目录含私钥种子 `identity.kel`、`control_token.txt`、`config.json`(`paths.go:83-85`、`identity.go:35`);子进程继承 daemon 全部环境变量(`agents.go:142`);outbox 收集会跟随符号链接(`autoreply.go:84-101` + `attachments.go:46,57`);失败时把含完整参数(含提示词与工作目录)的错误文本发给对端(`agents.go:187-196` → `autoreply.go:390`)。自动回复对入站和出站对称生效(`autoreply.go:14-17`),所以"我委派给的陌生提供方"的回复同样驱动本地 agent。
6. **控制面令牌注入 HTML,且无 Host 校验。** `/console` 不经 bearer,把控制令牌以 `window.__ANET` 注入页面(`console.go:21-45`);`/ping`、`/attachment` 同样不经认证(`control_api.go:159-161`),`/ping` 带 `Access-Control-Allow-Origin: *`(`console.go:52`)。整个控制面没有 Host / Origin 校验(全仓 `grep` 无 `r.Host`、`Origin` 检查),"只在 loopback 所以安全"的前提不覆盖 DNS 重绑定。**[推断]** 浏览器侧攻击者可借 DNS 重绑定读取 `/console` 得到令牌,令牌可通过 `/autoreply` 改写 exec 后端的 `command`/`extra_args`/`work_dir`(`control_api.go:519-538`、`config.go:70-75`),即令牌泄露等同本机代码执行。
7. **控制地址只在默认值上是 loopback。** 默认与自动分配都是 `127.0.0.1`(`config.go:116`、`config.go:137`、`identities.go:277`),但 `listenControl` 对配置值原样 `net.Listen`(`control_api.go:245-246`),不拒绝非 loopback 地址;代码注释自己写了"the control port may bind 0.0.0.0"(`control_api.go:416`)。
8. **默认构建、默认配置下没有非 loopback 监听。** x402 voucher 面仅在配置 `voucher_addr` 时开启(`module/x402/x402.go:32-39`、`voucher.go:329-334`);p2p 模块无配置时不实例化(`module/p2p/p2p.go:38-41`),`anetpeer` 是独立进程,所有监听地址均为必填参数(`tools/anetpeer/main.go:77-87`)。
9. **可复用的正确样板已经存在:`shell` 模块。** 加法 tag、空允许名单即拒绝所有人、空调用方默认拒绝、`allow_file` 每次调用重读以支持即时撤销、每次调用上证据链(`module/shell/shell.go:14-34`、`:144-174`、`:251-283`)。入站策略可直接沿用这套语义。

---

## 2. 配置默认值(`internal/daemon/config.go`)

| 字段 | 默认值 | 位置 | 说明 |
|---|---|---|---|
| `control_addr` | `127.0.0.1:39811`(回退值);新目录自动分配 `127.0.0.1:<39811..41810>` | `config.go:114-117`、`:131-139`;`identities.go:32`、`:273-283` | 注释写明控制面只靠明文 HTTP 上的 bearer 令牌保护(`config.go:15-17`)。 |
| `accept_delegations` | `true`,且首次加载即写盘 | `config.go:44`、`:94`、`:115-116`、`:165` | nil 与 true 同义。注释称"Accepting only STORES the task"(`config.go:41-43`)。 |
| `guest_messages` | 未设 ⇒ 5 | `config.go:48`、`:89`、`:98-106` | 每次注册与启动刷新都会发给 hub(`hub_client.go:62-67`、`daemon.go:191`)。 |
| `hub_url` | 空 | `config.go:21` | 为空时不启动 relay 轮询(`daemon.go:146-149`),即全新安装在 `hub-register` 之前收不到任何东西。 |
| `auto_reply` | nil(关闭) | `config.go:55` | 存在时启动即运行(`daemon.go:150-152`)。 |
| `modules` | 空 | `config.go:39` | 各模块无配置块时返回 nil(p2p `p2p.go:39-41`、shell `shell.go:107-109`、service `service.go:48-50`);x402 例外,无配置块也实例化,但不开监听(`x402.go:82-104`)。 |

附带的文档缺陷:`AcceptDelegations` 的注释被 `Providers`、`Modules` 两个字段从中间截断(`config.go:33-44`);`shell.Config` 中 `AllowArbitrary` 的注释与字段被 `AllowLocal` 隔开(`module/shell/shell.go:158-176`)。影响仅限可读性。

README 的推荐上手路径是 `anet autoreply set --backend exec --cmd "cursor"` 加 `anet accept on`,并描述为"receives delegations … while you sleep"(`README.md:90-99`)。

---

## 3. 陌生人向全新节点委派时的完整路径

前提:用户完成一行安装并执行了 `hub-register`(README 与 `install.go:34` 都这样引导)。

### 3.1 发送侧与 hub

1. 陌生人本地生成身份,签 TaskDoc(`daemon.go:281-291`),经 `relaySend` 投递(`relay.go:185-192`、`transport.go:104-131`)。
2. hub `/relay/send` 不认证调用方:注释写明"`relay/send` is intentionally unauthenticated (a mailbox drop-box)"(ANetHub `server.go:1081-1083`);处理函数只检查收件人已注册,`from_aid` 原样入队(`server.go:882-930`,入队在 `:918`)。
3. 载荷是 base64 的 `DelegateReq`(TaskDoc + 附件,`relay.go:185-190`、`transport.go:40-46`),未加密。hub 能读取目标、正文和附件。(E2E 加密另见其它调研篇。)

### 3.2 接收侧 daemon

1. relay 循环轮询 → `pollOnce` → `dispatch`(`relay.go:373-397`、`:410-428`)。
2. `ingestDelegate`(`delegation.go:487-565`):
   - 先看 `AcceptsDelegations()`,为 false 则丢弃(`:488-490`)。注意这一检查发生在验签之前,拿不到请求方 AID,所以当前结构无法按对端决策。
   - `VerifyDelegateReq` 用请求自带的 KEL 校验签名(`:496`),通过后缓存该 KEL(`:501-503`)。这证明"请求方持有该 AID 的密钥",不证明请求方是谁。
   - 以 `RoleInbound`、状态 `queued` 写入本地库(`:541-545`;`Put` 为 `INSERT OR IGNORE`,`internal/runtime/interactions/interactions.go:242-254`),把目标作为第一条对话消息存入(`:551-558`)。
   - 若 TaskDoc 是能力调用,直接交给能力提供者执行(`:561-563` → `runCapabilityCall`,`:585-632`)。调用方身份取 `ix.PeerAID`(`capability.go:374`)。定价能力先走付款闸门(`capability.go:339-372`);未命中提供者且配置了自动回复时,转交自动回复(`capability.go:308-310`)。
3. 处理完一批后若含 delegate/message,唤醒自动回复循环(`relay.go:388-395`、`:401-406`)。

### 3.3 不同配置下陌生人能达到什么

| 节点状态 | 陌生人委派的结果 | 依据 |
|---|---|---|
| 全新安装 + 已注册,无自动回复、无模块 | 任务被存储为 `queued`,出现在 `anet inbox`、MCP `task_inbox`、控制台。不执行任何代码。 | `delegation.go:541-564`;`control_api.go:685-697`;`internal/mcpserv/mcpserv.go:114` |
| 同上,且运行 `anet install --agent …` 装入了人设块 | 人设块要求本地 agent 执行 `anet inbox --pending`、`anet thread`、`anet message` 来"Provide work to others"(`install.go:38-42`)。**[推断]** 陌生人的目标文本因此进入用户交互式编码 agent 的上下文,构成提示注入入口,是否被执行取决于该 agent 自身的权限设置。 | `install.go:23-68` |
| 配置 `auto_reply` openai 后端 | 陌生人文本发往运营者配置的模型端点,回复自动发回。无本地代码执行,但消耗运营者的模型额度,且无按对端配额。 | `autoreply.go:481-543`、`:330-351` |
| 配置 `auto_reply` exec 后端(README 推荐路径) | 陌生人文本成为本地编码 agent 的提示词,agent 在身份数据目录内运行、可按提示词执行 shell(详见第 4 节)。 | `autoreply_exec.go:16-17`、`:154-181` |
| 配置 `service` / `anetlink` 模块 | 陌生人可以直接调用任何已注册能力,模块内无调用方检查。 | `delegation.go:561-563`;`module/service/service.go` 无 `CallerAID` 引用 |
| 配置 `shell` 模块(需 `-tags shell`) | 被 `allow` / `allow_file` 拦截;空名单拒绝所有人。 | `module/shell/provider.go:85-110` |
| `guest_messages` 默认 5 | hub 可把匿名浏览器访客随机路由到本节点,消息以 hub guest broker 身份签发。 | ANetHub `guest.go:3-16`、`:462-480`、`:594` |

### 3.4 后续消息(对话轮次)

`ingestMessage`(`delegation.go:636-686`):

- 只检查 interaction 存在(`:642`);发送方 AID 取自 relay 行的 `from_aid`(hub 未校验)或 p2p 帧的 `From`(`tools/anetpeer/main.go:278` 原样转交)。
- `ChatMsg` 无签名(ANetCore `delegation.go:133-179`),所以 daemon 无从校验。
- 本地展示时,任何非本机 AID 都映射为 `"them"`(`delegation.go:124-133`),自动回复把 `"them"` 的消息当作用户轮次(`autoreply.go:447-448`)。
- `ChatEndRequest` / `ChatEndAccept` 同样无认证,后者会触发 `maybeFinalize` 签发回执(`delegation.go:663-681`)。
- 影响:hub、联邦中继 hub,以及任何取得 interaction id 的一方,都可以向已接受的对话注入消息驱动自动回复,或伪造结束握手。`transport.go:172-175` 的注释称"the payload is end-to-end verifiable, so a peer process that lies produces a message that fails verification",该说法对 delegate 与 result 成立,对 message 不成立。
- 发现方式:阅读 `ingestMessage` 与 hub `hRelaySend`、ANetCore `ChatMsg` 定义。

---

## 4. 自动回复 exec 后端

### 4.1 本地 agent 收到的提示词

`execReplier.Reply` 组装为 `system + execRoleContext(rc) + "\n\n## Conversation\n\n" + prompt`(`autoreply_exec.go:104-108`;cursor 会话模式首轮同构,`:138-143`)。

- 系统提示(未配置 `system_prompt` 时)`autoReplyDefaultExecPrompt`(`autoreply_exec.go:16-50`):
  - "do any real work needed (you may run shell commands)"(`:17`)。
  - 附件通过写入 `$ANET_OUTBOX` 交付(`:25-26`)。
  - 作为请求方时要求"obtain or create a suitable file yourself and drop it in $ANET_OUTBOX"(`:30-35`)。
- `## Task` 段:角色 + `Goal: <对端给出的目标原文>`(`autoreply_exec.go:55-69`)。对端文本位于指令区而非数据区。
- `## Conversation` 段:逐轮 `Requester: <文本>` / `You (previous reply): <文本>`,图片落盘到 `<数据目录>/exec-tmp/run-*/` 后给出绝对路径(`autoreply_exec.go:183-223`,目录在 `:186`)。
- 没有任何把对端内容标为不可信数据的包装。

### 4.2 各 agent 的调用参数与权限

| agent | 参数 | 位置 | 权限含义 |
|---|---|---|---|
| cursor | `-p --output-format text --force --trust [--resume] [--model] [--workspace WD]` | `agents.go:211-222` | **[外部]** `--force` 为除显式拒绝外放行所有命令,`--trust` 为免确认信任工作区。 |
| claude | `-p --output-format text --permission-mode dontAsk --bare [--model]` | `agents.go:287-292` | `--bare` 跳过 hooks、CLAUDE.md 自动发现、OAuth/keychain,仅用 `ANTHROPIC_API_KEY`(本机 `claude --help` 核实)。**[外部]** `dontAsk` 对未预先放行的工具自动拒绝;用户 settings 中的 allow 规则仍生效。 |
| codex | `exec --full-auto --ephemeral -o <tmp> [-C WD]` | `agents.go:313-318` | **[外部]** `--full-auto` 对应 `workspace-write` 沙箱:写限于工作区,读不受限。 |
| opencode | `run [--model]` | `agents.go:347-352` | **[外部]** 默认权限对 bash/edit 为放行,取决于用户 opencode 配置。 |
| openclaw | `agent --local --json --agent main --message P` | `agents.go:375-376` | **[外部]** 未核实。 |
| hermes | `-z P` 或 `chat -q P -m M` | `agents.go:410-416` | **[外部]** 未核实。 |

对同一段对端文本,可执行范围从"自动拒绝"(claude)到"全部放行"(cursor)不等,系统提示却统一声称可以运行 shell。

### 4.3 工作目录、环境与文件面

- **工作目录**:`work_dir` 未配置时为 `dataDir`,即身份数据目录(`autoreply_exec.go:155-158`;`dataDir` 来自 `layout.Root`,`autoreply.go:151`)。该目录含 `config.json`(可能含 `auto_reply.api_key`)、`identity.kel`(私钥种子,`identity.go:35`)、`control_token.txt`、`interactions/`(全部其它对话)、`evidence.ael.jsonl`(`paths.go:83-96`)。
- **环境变量**:`runCmd` 以 `os.Environ()` 为底再追加(`agents.go:141-143`),agent 继承 daemon 全部环境变量;另注入 `ANET_DATA_DIR`、`ANET_OUTBOX` 与按 agent 映射的 API key(`autoreply_exec.go:162-166`、`agents.go:168-185`)。
- **outbox 在数据目录内**:`os.MkdirTemp(d.layout.Root, "exec-outbox-")`(`autoreply.go:372`)。
- **outbox 收集跟随符号链接**:`collectOutboxFiles` 只排除目录项(`autoreply.go:88-99`,`DirEntry.IsDir()` 对符号链接为 false),随后 `attachmentFromPath` 用 `os.Stat` / `os.ReadFile`(`attachments.go:46`、`:57`),两者都跟随链接。
  - 影响 **[推断]**:agent 在 outbox 内建立指向 `../identity.kel` 或任意用户可读文件的链接,daemon(未沙箱化)会读取目标并作为附件发给对端。即使 agent 本身处于限制读取的沙箱中,只要能创建链接,这条路径也能绕过沙箱。
- **错误详情外发**:后端失败时回复 `error_reply + "(technical detail: " + err.Error() + ")"`(`autoreply.go:382-391`);exec 错误由 `cmdError` 生成,包含可执行文件名与**完整未截断的参数列表**(`agents.go:187-196`),参数里有整段提示词和 `--workspace`/`-C` 绝对路径(`agents.go:218-220`、`:314-316`)。影响:本机路径、系统提示与对话上下文泄露给对端。
- **测试钩子在生产路径上**:`ANET_EXEC_COMMAND` 环境变量会替换任何 agent 的可执行文件(`agents.go:97-99`、`:253-255`)。影响限于能改 daemon 环境的人,风险低,但不应出现在发行构建中。

### 4.4 调度与额度

- 自动回复循环顺序处理所有活动线程(`autoreply.go:290-300`),单次调用超时默认 180 秒(`autoreply.go:52`)。没有按对端的并发、速率或每日上限;唯一的上限是每个 interaction 最多 30 条自动回复(`autoreply.go:53-56`、`:330-351`)。**[推断]** 多个陌生身份各开若干 interaction 即可长期占满该循环,并消耗运营者的模型额度。
- 对端提议结束时自动接受(`autoreply.go:306-316`)。
- 出站方向同样生效:我方委派出去、对方回复后,对方文本同样进入本地 exec agent(`autoreply.go:14-17`、`autoreply_exec.go:56-59`)。所以"不可信"必须按对端 AID 判定,而不是按入站/出站判定。

### 4.5 openai 后端

仅把对话以 JSON 发往运营者配置的 `api_base`(`autoreply.go:481-543`),无本地执行。风险限于额度消耗与失败信息外发(同 `autoreply.go:390`)。

---

## 5. 控制面(`internal/daemon/control_api.go`、`console.go`)

### 5.1 路由与认证

| 路由 | 认证 | 位置 | 返回/作用 |
|---|---|---|---|
| `GET /console` | 无 | `control_api.go:159`;`console.go:21-45` | 返回注入 `window.__ANET = {base, token, aid, name, hub}` 的整页 HTML(`console.go:29-41`)。 |
| `GET /ping` | 无,`ACAO: *` | `control_api.go:160`;`console.go:50-62` | AID、名称、版本、提交、构建时间、构建 tags(`console.go:58-61`)。 |
| `GET /attachment?interaction_id&cid` | 无 | `control_api.go:161`、`:762-790` | 任意已存附件的原始字节。 |
| `GET /` | 无 | `control_api.go:166-168` | 302 到 `/console`。 |
| 其余 27 个 `POST`/`GET` 路由 | `Authorization: Bearer <token>`,常量时间比较 | `control_api.go:125-154`、`:169`、`:326-343` | 含 `/autoreply`(写入完整 `AutoReplyConfig`,`:519-538`)、`/x402-authorize`(用本节点密钥签付款授权,`:1026-1069`)、`/pull`(把附件写到任意 `out_dir`,`:745-760`)、`/delegate` 的本地路径附件(`:603-606`)、`/shutdown` 等。 |

- 令牌:24 字节随机数 hex,`control_token.txt` 0600(`control_api.go:33-56`)。
- 请求体上限 1 GiB(`control_api.go:187`),multipart 130 MiB(`:191`)。
- 无 Host 校验、无 Origin 校验、无安全响应头(`grep` 全仓无 `r.Host`、`Origin`、`X-Frame-Options`、`Content-Security-Policy`)。

### 5.2 令牌如何到达浏览器

- `consoleHandler` 把令牌写入 `<script>window.__ANET = …</script>` 替换 `</head>`(`console.go:40-41`)。注释给出的理由是"a browser navigation cannot set an Authorization header"且"the control address is loopback-only — the same trust boundary as the on-disk control token"(`console.go:3-7`;同见 `control_api.go:155-157`、`:763-764`)。
- 页面脚本用 `Authorization: Bearer` + `A.token` 调 API(`web/console.html:630-636`)。
- 该理由不成立的两种情况:
  1. **DNS 重绑定 [推断]**:外部页面先把自有域名解析到公网、再改解析到 `127.0.0.1`,此时对 `http://<该域名>:39811/console` 的请求在浏览器看来是同源,服务端因不校验 Host 照常返回含令牌的 HTML。端口可预测(自动分配从 39811 起,`identities.go:273`),且 `/ping` 的 `ACAO: *` 让任意网页能跨源确认 daemon 是否在某端口运行、读出 AID 与构建 tags。
  2. **控制地址被配置为非 loopback**:`listenControl` 原样监听(`control_api.go:245-246`),此时 `/console` 对局域网任何人返回令牌。
- 令牌能力 **[推断]**:读取所有对话、以本节点身份委派/回复/签付款授权、改 hub、以及通过 `/autoreply` 设置 exec 后端的 `command`、`extra_args`、`work_dir`(`config.go:70-75`)。后者使令牌泄露可升级为本机代码执行。
- 典型 CSRF 不成立:API 需要自定义 `Authorization` 头,表单或 `<img>` 无法携带;未设置 CORS,跨源 `fetch` 带自定义头会被预检拦下。这一点是现有设计中正确的部分,新方案需保留。

### 5.3 CORS

只有 `/ping` 设置 `Access-Control-Allow-Origin: *`(`console.go:52`,全仓唯一一处)。控制台页面自身不调用 `/ping`;官方 hub 页面明确不驱动本地 daemon(`web/console.html:617` `daemonDetected = false`)。现存消费者是脚本中的 `curl`(`scripts/lib.sh:43`、`scripts/joint.sh:116-117`),不需要 CORS。

### 5.4 控制台对对端内容的渲染

控制台大量使用 `innerHTML` 拼接(`web/console.html` 共 27 处),有 `esc()` 转义(`:686`),markdown 渲染先转义再加标签、链接限定 `https?://`(`:1436-1443`)。本次只抽查了 markdown 与消息渲染,未做完整 XSS 审计。由于页面同源持有令牌,任何一处漏转义都等同令牌泄露;这是移除令牌注入的另一个理由。

### 5.5 本机多进程协调文件

- `/tmp/anet-<uid>/daemon.json` 与 `/tmp/anet-<uid>/daemons/*.json` 用固定 `/tmp` 路径(`paths.go:26-41`),创建时 `MkdirAll(…, 0o700)`(`control_api.go:68`、`registry.go:36`),不校验已存在目录的属主与权限。**[推断]** 同机其它用户可预先创建该目录,令 CLI 回退路径(`control_api.go:89-102`)把命令发往其伪造的 daemon。多用户主机才相关,优先级低。
- `/identities` 列出本机所有 daemon 的控制地址(`control_api.go:871-904`),控制台据此切换身份;切换目标的 `/console` 同样注入令牌。

---

## 6. 模块默认暴露面

| 模块 | 默认 | 暴露 | 位置 |
|---|---|---|---|
| shell | 加法 tag,默认构建不含;含了也需配置块 | 调用方允许名单,空即拒绝;无身份调用默认拒绝;`allow_arbitrary` 默认关;每次调用写证据链 | `shell.go:14-34`、`:106-118`、`:144-176`、`:251-283`;`provider.go:85-110` |
| p2p | 减法 tag,默认构建含;无配置块不启动 | 本身只连 Unix socket(`p2p.go:54-64`)。对外监听在独立进程 `anetpeer`,地址为必填参数(`tools/anetpeer/main.go:77-87`、`:129-133`)。对端连接无认证,帧的 `From` 原样交给 daemon(`main.go:245-278`),与 3.4 节的消息伪造问题叠加。 | 同左 |
| x402 | 减法 tag,默认构建含;无配置也实例化 | voucher 面仅当 `voucher_addr`+`voucher_url` 同时配置才监听(`x402.go:93-100`、`voucher.go:329-352`),按设计是公网面;兑换需 hub 签名的 voucher 且能力有定价(`voucher.go:223-263`)。未与"哪些能力对外公开"联动。 | 同左 |
| service / anetlink | 减法 tag,默认含;无配置块不启动 | 配置后,能力对所有能发来委派的人开放,无调用方检查。 | `service.go:46-57`;`module/` 下 `CallerAID` 仅见于 shell 与 x402 |
| mcp | 减法 tag | stdio 短进程,不监听(`internal/mcpserv/mcpserv.go:11-14`) | 同左 |

## 7. 默认监听清单

| 监听 | 默认地址 | 条件 | 位置 |
|---|---|---|---|
| 控制面 HTTP | `127.0.0.1:<39811..41810>` | 始终 | `config.go:131-139`;`control_api.go:244-268` |
| x402 voucher HTTP | 无 | 配置 `voucher_addr` | `voucher.go:329-334` |
| anetpeer 对端监听 | 无(独立进程,必填) | 运行 `anetpeer --peer` | `tools/anetpeer/main.go:129-133` |
| anetpeer daemon socket | 无(独立进程,必填) | 同上 | `tools/anetpeer/main.go:137-138` |

默认构建与默认配置下不存在非 loopback 监听。风险不来自监听,而来自 hub relay 这条出站轮询带回的入站内容,以及浏览器对 loopback 的可达性。

---

## 8. 设计方案

### 8.1 原则

1. 入站默认拒绝:全新安装不执行任何来自他人的内容,不把他人内容放进可执行上下文。
2. 判定依据是**经签名验证的对端 AID**,不是 hub 给出的 `from_aid`,也不是"入站/出站"方向。
3. 公开性来自显式声明的公开能力(官方公共 agent/工具即走这条),而不是全局开关。
4. 失败闭合:沙箱不可用时不降级为无沙箱执行。
5. 每一种拒绝、待批、超时都给请求方一个诚实状态,不静默挂起(对应项目的 honest effect status 规则)。

### 8.2 入站策略配置

替换 `accept_delegations`,新增 `inbound` 块(内核代码,不挂 tag:它决定内核是否接受 C2 投递,属于内核职责):

```json
"inbound": {
  "policy": "approve",
  "allow_file": "peers.allow",
  "deny_file": "peers.deny",
  "public_capabilities": [],
  "pending": {"max_total": 200, "max_per_peer": 3, "ttl_hours": 72, "max_goal_bytes": 65536},
  "notify_requester": true,
  "agent_may_approve": false
}
```

- `policy`:`closed`(只收允许名单)、`approve`(默认:允许名单直接接受,其余进待批队列)、`open`(接受任何人,仅供官方公共 agent;启动时要求 `auto_reply.remote_exec` 不是 `full`,否则拒绝启动)。
- `allow_file` / `deny_file`:语义照搬 shell 模块(`shell.go:144-157`、`:251-283`):每行一个 AID,`#` 注释,每次判定重读,文件不存在等于空;deny 优先于 allow。默认路径在数据目录内。
- `public_capabilities`:任何人可调用的能力 id(含定价能力)。空表示没有公开能力。

判定顺序(在 `ingestDelegate` 中,**移到 `VerifyDelegateReq` 之后**,替换 `delegation.go:488-490`):

| 序 | 条件 | 动作 | 回给请求方 |
|---|---|---|---|
| 1 | 验签失败 | 丢弃并 ack(现状) | 无 |
| 2 | AID ∈ deny | 丢弃并 ack | 可选 `REJECTED`(按 `notify_requester`,限速) |
| 3 | 能力调用且 capID ∈ `public_capabilities` | 进入现有能力执行路径,价格闸门照旧;**未命中提供者时不得转交自动回复**(改 `capability.go:308-310`,对非允许名单调用方直接答 `UNAVAILABLE`) | 能力结果 / `PAYMENT_REQUIRED` |
| 4 | AID ∈ allow | 现状:写入 interactions,状态 queued,标记 `trust=peer` | 无额外消息 |
| 5 | `policy=approve` | 写入**独立的待批存储**,不进 interactions 表 | 一次性签名状态:A2A `TASK_STATE_SUBMITTED`(`Refs/a2a/specification/a2a.proto:191`),附 `anet.inbound=pending_approval` |
| 6 | `policy=closed` | 丢弃并 ack | `TASK_STATE_REJECTED`(`a2a.proto:205`),限速 |
| 7 | `policy=open` | 写入 interactions,标记 `trust=public` | 无 |

x402 付费路径与 A2A 对接:公开定价能力对未知调用方回 `input-required` + `x402.payment.status: payment-required`(`Refs/a2a-x402-spec-v0.2.md:135`、`:161`、`:177-184`),付款后执行。付款证明的是"已付",不是"能力对外公开",所以 voucher 面兑换也应要求 capID ∈ `public_capabilities`(在 `voucher.go:252` 附近加检查)。

### 8.3 前置条件:消息真实性

没有这一步,允许名单只约束第一条委派,后续轮次仍可被 hub 或任何知道 interaction id 的一方注入(3.4 节)。

1. `ingestMessage` 丢弃 `fromAID != ix.PeerAID` 的消息(`delegation.go:642` 之后)。
2. `ChatMsg` 由发送方签名(或由 E2E 会话密钥 AEAD 认证),接收方用已缓存的对端 KEL(`delegation.go:501-503` 写入的 `d.peers`)验证;`ChatEndRequest` / `ChatEndAccept` 同样要求。与 E2E 加密篇的设计合并实现,线协变更需同步 ANetCore 与 ANetHub,并补 `internal/hubapi/hubapi_test.go` 一类的契约测试。
3. p2p 路径同理:`anetpeer` 的 `From` 仅作路由提示,真实性以签名为准。

### 8.4 待批队列

- **存储**:独立 SQLite 表(或独立文件),不复用 `interaction` 表加状态位。理由:现有 `Threads`、`ActiveThreads`、`Inbox`、控制台、自动回复都直接遍历 `interaction` 表(`delegation.go:122-199`),加状态位要求每个消费方都记得过滤,漏一处就把未批准内容送进 agent;独立存储使这些路径天然看不到它。代价是批准时要做一次迁移写入。
- **内容**:原始签名 TaskDoc 字节、请求方 AID 与 KEL、到达时间、经由的 hub、附件元数据(字节按 `max_goal_bytes` 与现有附件上限截断或丢弃)、同一 interaction 的后续消息(每条目上限若干条)。
- **上限**:`max_total`、`max_per_peer`、`ttl_hours`;超限的新条目直接 `REJECTED`,不挤掉旧条目(避免攻击者冲掉合法待批)。TTL 到期回 `REJECTED`,理由"not approved within TTL"。
- **批准语义**:
  - `approve <ix>`:仅此一次;从待批存储移入 interactions(重验签名后 `Put`),`trust=approved_once`,唤醒自动回复。
  - `allow <aid>`:写入 `allow_file`,同时批准该 AID 的全部待批项。
  - `reject <ix>` / `deny <aid>`:回 `REJECTED`;后者写 `deny_file`。
- **呈现面**:
  - CLI:`anet inbound pending|approve|reject|allow|deny|list`。
  - 控制 API:`/inbound/*`,bearer 保护。
  - 控制台:待批面板。
  - MCP:只读工具 `inbound_pending`。不默认提供批准工具,因为待批内容由攻击者撰写、由同一个 agent 阅读,让它自行批准等于让注入内容自我授权。`agent_may_approve=true` 时才注册批准工具,并在 MCP 工具注解上标记 `destructiveHint`,由客户端逐次向用户确认。
  - 列表输出把目标文本作为带长度上限的引用数据呈现,并附 AID 的可核实信息(是否有过带回执的往来、hub 上的签名卡片名称,名称标注为未核实的自述)。
- **本地提示**:daemon 日志一行;`anet` 无参引导(`cmd/anet/main.go` 的状态提示)显示待批数量。

### 8.5 自动回复执行沙箱

在 `auto_reply` 增加:

```json
"auto_reply": {
  "remote_exec": "sandbox",
  "full_trust_file": "peers.fulltrust",
  "per_peer": {"max_active": 2, "max_replies_per_hour": 30}
}
```

- `remote_exec` 取值 `sandbox`(默认)、`text-only`、`off`。只有列在 `full_trust_file` 中的 AID 才以现有方式(无沙箱)运行。即允许名单决定"能否委派",`full_trust_file` 决定"能否在我机器上不受限执行",两者分开。
- 判定对象是 `ix.PeerAID`,对入站和出站一视同仁(4.4 节)。

`sandbox` 的最低要求:

1. **工作目录**:每个 interaction 一个新目录,位于数据目录之外(如 `$XDG_CACHE_HOME/anet/sandbox/<ix>/`),0700,interaction 结束或被 `pruneExcept` 清理时删除。`outbox` 与 `exec-tmp` 都放在该目录内,不再放在 `layout.Root`(改 `autoreply.go:372`、`autoreply_exec.go:186`)。
2. **环境变量**:不再以 `os.Environ()` 为底(改 `agents.go:141-143`),改为白名单:`PATH`、`LANG`、`TERM=dumb`、`HOME=<workdir>/home`、`TMPDIR=<workdir>/tmp`、该 agent 所需的 API key 变量。不传 `ANET_DATA_DIR`。
3. **OS 级隔离**:Linux 用 bubblewrap(或 landlock):系统目录只读绑定,用户家目录以 tmpfs 覆盖(`~/.anet`、`~/.ssh` 不可见),仅工作目录可写,独立 PID/IPC 命名空间。macOS 用 `sandbox-exec` profile **[推断,需验证可用性]**。其它平台无可用隔离时,`sandbox` 视为不可用。
4. **失败闭合**:隔离运行时不可用时,不执行 exec;若配置了 `text-only` 回退则走 `text-only`,否则任务留在收件箱,向请求方回 `UNAVAILABLE`(说明原因)。不得回落为无沙箱执行。
5. **提示词**:沙箱模式下使用另一份系统提示,不含"you may run shell commands";`Goal` 移入 `## Conversation` 数据区并声明其为不可信输入(改 `autoreply_exec.go:55-69`、`:104-108`)。
6. **agent 参数**:沙箱模式按 agent 收紧,例如去掉 cursor 的 `--force`、codex 用只读或工作区沙箱。具体标志因 CLI 版本而异 **[外部,需逐一核实]**;无法确认"无工具"语义的 agent,`text-only` 对其不可用,启动时报错。
7. **outbox**:收集时用 `Lstat`,只接受常规文件,拒绝符号链接与设备文件;打开用 `O_NOFOLLOW`;规范化后的路径必须位于 outbox 内;数量与总大小设上限(改 `autoreply.go:84-101`,不走通用的 `attachmentFromPath`)。
8. **错误外发**:对端只收到通用错误文本与一个本地日志关联 id;`cmdError` 不再拼接完整参数(改 `agents.go:187-196`、`autoreply.go:382-391`)。
9. **调度**:按对端的并发与速率上限;顺序循环改为有界 worker 池并按对端轮转,避免一个对端占满循环(`autoreply.go:290-300`)。
10. **测试钩子**:`ANET_EXEC_COMMAND` 移到 `_test.go` 注入点或仅在测试构建 tag 下生效(`agents.go:97-99`、`:253-255`)。

已知代价(需在文档中写明):

- 沙箱内 agent 仍需联网访问模型 API,因此仍可把工作目录内容或其自身的模型凭据发往外部;在不引入出站代理的前提下无法消除。**[推断]** 使用 OAuth/订阅登录的 agent CLI 需要把其凭据文件只读暴露给沙箱,等于把该凭据交给受对端提示词驱动的进程;沙箱模式应只支持 API key 认证,或明确告知这一暴露。
- bubblewrap 在部分发行版需要 unprivileged user namespace;容器内运行 daemon 时通常不可用,届时 `sandbox` 不可用、按失败闭合处理。

### 8.6 控制面令牌处理

1. **取消 HTML 注入**,改为一次性票据换会话:
   1. `anet console`(CLI 持有 bearer)调用 `POST /console/ticket`,得到单次有效、60 秒过期的随机票据。
   2. CLI 打开 `http://127.0.0.1:<port>/console#t=<ticket>`。片段不出现在请求行、服务器日志和 Referer 中。
   3. 页面脚本读取片段后立即 `history.replaceState` 清除,`POST /console/session` 提交票据;服务端校验后设置 `HttpOnly; SameSite=Strict; Path=/` 会话 cookie,并在响应体返回 CSRF 值。
   4. 控制台后续请求携带 cookie + `X-Anet-CSRF` 头;服务端同时校验两者与 `Origin`。
   - 会话凭据与控制令牌分离、可撤销、有过期时间。可以把 `/autoreply` 配置变更、`/x402-authorize` 排除在浏览器会话权限之外,或要求在 CLI 侧再确认。
   - 代价:控制台不能再通过手敲地址直接打开,必须经 `anet console`(或 `anet console --url` 打印一次性地址);身份切换器需要改为"经目标 daemon 的 CLI 取票据",或由当前 daemon 代取票据(同 uid,可读对方令牌文件)。
2. **Host 白名单中间件**包住整个顶层 mux(`control_api.go:158-170`):Host 必须是 `127.0.0.1:<port>`、`localhost:<port>` 或 `[::1]:<port>`,否则 403/421。这是防 DNS 重绑定的必要条件,对 `/ping`、`/attachment` 同样适用。
3. **`/ping`**:去掉 `Access-Control-Allow-Origin: *`(`console.go:52`)。已知消费者均为 `curl`(5.3 节),不受影响。
4. **`/attachment`**:改为要求会话 cookie 或 bearer。
5. **非 loopback 控制地址**:`listenControl` 拒绝非 loopback 地址,除非显式设置 `control_allow_remote: true`;即便设置,也关闭 `/console` 与会话接口(改 `control_api.go:244-268`)。
6. **响应头**:`/console` 加 `Content-Security-Policy`(内联脚本改用 nonce;`frame-ancestors 'none'`;`connect-src` 限定自身与配置的 hub)、`Referrer-Policy: no-referrer`、`X-Content-Type-Options: nosniff`、`Cache-Control: no-store`。
7. **运行时目录**:创建或使用 `/tmp/anet-<uid>` 前 `Lstat` 校验非符号链接、属主为当前 uid、权限 0700,否则拒绝使用;优先 `$XDG_RUNTIME_DIR`(改 `paths.go:32-34`)。
8. 保留现状中正确的部分:API 仍只接受自定义头认证,不设 CORS。

### 8.7 访客模式

- daemon:`GuestDefaultMessages` 改为 0(`config.go:89`);已有配置不带该键的节点升级后即退出访客接待。
- hub:guest broker 由 hub 签名并中转访客明文(ANetHub `guest.go:3-12`、`:594`),与"hub 只做传输"冲突。建议从 hub 移除面向第三方 agent 的访客路由;如需网页试用,只路由到官方公共 agent,并在页面上说明该通道由运营方处理内容。
- daemon 侧不给 guest broker AID 任何特殊信任,它按普通未知发送方进入待批队列。

### 8.8 能力可见性与官方公共 agent

- 默认所有能力只对允许名单开放;对外公开必须列入 `public_capabilities`。
- 官方公共 agent/工具 = `policy=open` 或 `public_capabilities` 非空 + `remote_exec=sandbox|text-only|off` + 定价(可为 0)。这就是决定 3 中"通过自行开放公共工具让其他 agent 有感觉"的承载方式。
- A2A 侧:本地 A2A 接口(127.0.0.1)需按规范对每个请求认证、按调用方裁剪可见任务(`Refs/a2a/docs/specification.md:1891-1905`、`:3081-3108`);这与 8.6 的 Host 校验和 bearer/会话机制共用。

### 8.9 迁移

- 旧 `accept_delegations`:缺省或 `true` 一律映射为 `policy=approve`,`false` 映射为 `closed`。理由见 1.1:磁盘上的 `true` 多数由 `freshConfig` 写入,不代表运营者意图。升级时日志说明一次,并把 `anet accept on|off` 改为 `inbound policy approve|closed` 的别名。
- 旧 exec 自动回复配置:升级后对远端一律走 `sandbox`;沙箱不可用的主机上,exec 对远端停止执行并回 `UNAVAILABLE`。这是行为变更,需写入发布说明,并提供 `anet peers fulltrust <aid>`。
- README 上手段落(`README.md:90-99`)与 `install.go` 人设块(`install.go:38-42`)需要改写:人设块不再要求 agent 自行领取收件箱任务,而是说明待批队列需由用户批准。
- hub 注册载荷可选择公布 `inbound.policy`,以便 `find` 显示"仅接受允许名单",减少请求方盲发。

### 8.10 可插拔编译归属

- `inbound` 策略、消息真实性校验、控制面会话与 Host 校验:属于内核契约(C2 投递的接受判定、控制面安全),不加 tag。
- exec 沙箱运行器:与 exec 自动回复同处内核(`autoreply_exec.go` 当前无 build tag)。是否把 exec 自动回复整体拆为减法 tag 模块(如 `no_autoexec`)是一个独立问题,见第 11 节。若拆分,按 CLAUDE.md 要求在 `.github/workflows/ci.yml` 的 tag 矩阵(`ci.yml:95-108`)中增加该 tag 并双向验证符号数。

---

## 9. 需要修改的现有测试与脚本

| 位置 | 现状依赖 | 需要的改动 |
|---|---|---|
| `internal/daemon/relay_test.go:20-44` `newTestDaemon(t, hub, accept bool)` | `accept=true` 即接受任何人 | 改为接收允许名单参数,或提供 `allowPeer(t, prov, req.AID())` 辅助函数。受影响调用 38 处:`capability_test.go` 12、`release_qa_test.go` 9、`card_seq_test.go` 4、`pay_test.go` 3、`relay_test.go` 2、`release_qa_h_test.go` 2、`release_qa_longcall_test.go` 2、`autoreply_test.go` 1(`:62-63`,影响所有自动回复测试)、`paylink_test.go` 1、`peerkel_test.go` 1、`transport_test.go` 1。 |
| `relay_test.go:194-220` `TestRelayDelegationRefusedWhenNotAccepting` | `accept=false` 时收件箱为空 | 语义保留,改名并扩展为 `closed`/`approve`/`deny` 三种情形;`approve` 下断言收件箱为空且待批队列有 1 条。 |
| `capability_test.go:817-851` | 直接构造无签名 `ChatMsg` 调 `ingestMessage` | 消息签名落地后需改为签名消息;另增"发送方非 PeerAID 被丢弃"的断言。 |
| `autoreply_exec_test.go:12-37`、`:137-156`;`release_qa_agents_test.go:41-95` | exec 走无沙箱路径、工作目录可任意指定、`ANET_EXEC_COMMAND` 钩子 | 区分 `full_trust` 与 `sandbox` 两条路径;沙箱测试在无 bwrap 的 CI 上断言"失败闭合 + UNAVAILABLE";钩子改为测试专用注入。`TestEveryRegisteredAgentRunsInTheRequestedWorkDir` 仅适用于 full_trust 路径。 |
| `config_test.go:34-44` | `DefaultConfig()` 可直接保存 | 增断言:默认 `inbound.policy == "approve"`、允许名单为空、`GuestQuota()==0`。 |
| `release_qa_port_test.go:80`、`:124` | 使用 `DefaultConfig()` | 核对默认值变化后是否仍成立。 |
| `hubfake_test.go:458-` fake hub `/relay/send` | 与真 hub 一样不校验 `from_aid` | 保持(fake 需实现真 hub 的完整契约,真 hub 不校验);新增测试利用这一点构造伪造 `from_aid` 的消息,断言 daemon 丢弃。 |
| `scripts/lib.sh:55`(被 `scripts/2-provider.sh` 使用) | 配置只有 `control_addr`,依赖默认接受 | 需写入请求方 AID 的允许名单,或改为演示待批流程。 |
| `scripts/joint.sh:69`、`scripts/joint-fleet.sh:81`、`scripts/scenario.sh:101`、`:578`、`scripts/onboard.sh:78` | 显式 `accept_delegations: true` | 迁移后该值映射为 `approve`,这些联调会停在待批。需改为写允许名单(推荐,覆盖新路径),或显式 `policy=open`(只用于公共 agent 场景的联调)。 |
| `scripts/prodtest.sh:1639` | `accept_delegations: false` | 映射为 `closed`,语义不变;确认断言文本。 |
| `scripts/4-guest.sh` | 依赖默认访客配额 5 | 访客默认关闭后该脚本需指定开启了访客的官方节点,或删除。 |
| 控制面 | 当前无任何 `/console`、`/ping`、`/attachment`、`bearer` 的单元测试(`grep` 无匹配);脚本对 `/ping` 用 `curl`(`scripts/lib.sh:43`、`scripts/joint.sh:116-117`、`scripts/joint-shell.sh:120-121`、`scripts/joint-fleet.sh:101`) | 脚本不受影响(curl 默认 Host 为 `127.0.0.1:<port>`,满足白名单)。需新增测试,见第 10 节。 |

## 10. 需要新增的测试(每条都应做 mutation 验证:改坏实现,确认测试失败)

1. 未知 AID 委派在 `approve` 下进入待批、不出现在 `Inbox`/`Threads`/`ActiveThreads`、不触发自动回复;请求方收到一次 `SUBMITTED`。变异:把判定顺序中的待批分支改为接受。
2. `allow_file` 修改后下一次判定即生效(撤销无需重启)。变异:改为启动时加载一次。
3. deny 优先于 allow。
4. 公开能力可被未知 AID 调用;非公开能力被拒;未命中提供者时对非允许名单调用方回 `UNAVAILABLE`,不进自动回复。
5. `ingestMessage` 丢弃 `fromAID != PeerAID` 的消息与未签名/签名无效的消息;伪造的 `ChatEndAccept` 不触发 `maybeFinalize`。
6. 沙箱:工作目录不在数据目录内;子进程环境不含 `ANET_DATA_DIR` 与 daemon 环境中的任意哨兵变量;outbox 内的符号链接不被发送;错误回复不含参数与路径。
7. 沙箱不可用时失败闭合:不调用 agent,请求方收到 `UNAVAILABLE`。
8. 控制面:错误 Host 返回 403(含 `/console`、`/ping`、`/attachment`);`/console` 响应体不含控制令牌;`/ping` 无 `Access-Control-Allow-Origin`;票据单次有效、过期失效;无 CSRF 头的会话请求被拒;非 loopback `control_addr` 启动失败。
9. 迁移:旧配置 `accept_delegations: true` 加载后 `policy == approve`。
10. 联调:`scripts/joint.sh` 增加"陌生第三节点委派 → 待批 → CLI 批准 → 自动回复执行"与"hub 伪造 `from_aid` 注入消息被丢弃"两段;按 CLAUDE.md 要求在 `scenario.sh`(两 hub)覆盖跨 hub 情形。

## 11. 未决问题

1. `policy=approve` 下是否向请求方发送 `SUBMITTED` 状态:发送满足诚实状态,但向任意陌生人确认"该节点在线且在 relay 上可达";不发送则请求方无法区分"待批"与"离线"。默认值需产品决定。
2. 旧配置中的 `accept_delegations: true` 是否有一部分确属运营者有意打开(如已上线的公开服务节点),需要逐台确认后显式改为 `policy=open`。
3. 沙箱模式下 OAuth/订阅登录的 agent CLI(cursor、claude 订阅)如何提供凭据;是否只允许 API key。
4. exec 自动回复是否拆为可裁剪模块(减法 tag),以便"不需要本地 agent 自动执行"的发行版在编译期去掉 `os/exec` 路径。
5. hub 访客模式是删除,还是只保留给官方公共 agent;若保留,如何在界面上说明该通道内容对运营方可见。
6. MCP 是否提供批准工具(`agent_may_approve`)及其默认值;依赖各 MCP 客户端对 `destructiveHint` 的处理是否一致 **[外部,需核实]**。
7. 各 agent CLI 的"只读 / 无工具"参数需逐一在目标版本上核实(4.2 节标注 [外部] 的各项)。
8. 本次未对控制台做完整 XSS 审计(5.4 节);令牌移出页面后其影响降低,但会话 cookie 仍可被同源脚本使用,仍需审计。
