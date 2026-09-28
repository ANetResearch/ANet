# anet 0.2.0 发布说明

[English](RELEASE-NOTES-0.2.0.md)

> **状态:定稿前的草稿。** 发布日期、官方公共 agent 是否随版上线、发布签名密钥与各处链接在发布当天确定;
> "安全修复"一节的公开时机见 `docs/notes/0027` 的 G0(0.1.x 线的 hub 热修部署之前不得公开)。
> 发布当天按 `docs/notes/0027` §4 的清单改掉本段。

anet 0.2.0 是"对齐 A2A"的版本:daemon 之间的消息端到端加密后经 hub 中继,任务就是 A2A Task,付款走
a2a-x402,本机提供标准的 A2A 接口,新节点默认谁的任务都不接。它与 **hub wire 2**(ANetHub 0.2.0)同批发布,
内核为 **ANetCore v0.15.0**。

**0.2.0 与 0.1.x 不互通。** 0.1.x 的 daemon 连 wire 2 的 hub 得到 HTTP 426;0.2.0 的 daemon 拒绝 wire 1 的
hub。官方 hub 切到 wire 2 的同时,请把节点升级到 0.2.0(见"升级与迁移")。

设计与取舍见 [A2A-DESIGN-zh.md](A2A-DESIGN-zh.md),用法见 [GUIDE-zh.md](GUIDE-zh.md),这一版仍有的局限见
[KNOWN-LIMITATIONS-zh.md](KNOWN-LIMITATIONS-zh.md)。

---

## 1. 新东西

### 1.1 默认安全

全新安装(`anet init` 之后)不接受任何人的委派、不执行任何东西、不自动付一分钱:

| 配置 | 缺省值 |
|---|---|
| `inbound.policy` | `closed`:名单外的委派在进门时就被拒,回签名的 `rejected`(`anet.reason=not_accepting`),不写任务、不存内容 |
| `peers.allow`、`peers.trust` | 空 |
| `inbound.public_capabilities` | 空 |
| `auto_reply.untrusted` | `off` |
| `payments.auto_max`、`agent_max`、`agent_daily_max` | 0 |
| `payments.payees_file` | 启用且为空(`payees.allow`) |

开放是逐项打开的:`anet peers allow <aid>`(允许名单,终端确认)、`anet peers trust <aid>`(信任名单,可对其启用
exec 自动回复)、`inbound.public_capabilities`(对任何人开放的确定性能力,带配额)、`anet inbound policy approve`
(待批队列)或 `open`。`anet doctor` 逐键列出当前值。

### 1.2 端到端加密

- daemon 之间的每条消息先签名、再以 HPKE(X25519 / HKDF-SHA256 / ChaCha20-Poly1305)封装给收件人,
  签名覆盖收件人、类型、任务、消息 id、时间、正文与发送方的密钥历史;hub 只搬运密文。
- 接收方只接受封装信封:未封装、签名不符、收件人不符、过期、重放一律拒收,没有明文回退。
- 往 hub 投递要以发送方身份签名认证(relayauth v2),hub 按发送方限流,但不把发送方写进中继存储;
  投递成功取走即删。
- 评价不再携带任务内容(只有回执、评分与 ≤280 字的短评)。
- hub 仍然看得到谁在何时给谁发了多大的消息,见已知局限第 1 条;官方公共 agent 是任务的另一端,看得到
  你发给它的内容,见第 4 条。

### 1.3 A2A 任务模型

- 任务状态即 A2A 的 `submitted` `working` `input-required` `completed` `failed` `canceled` `rejected`。
- 文本任务由**提供方单方完成**:提供方 `anet end`(或 MCP `reply_task` 带 `state=completed`、或自动回复判定
  完成)即完成并签回执;委派方的 `anet end` 是"请求完成",提供方的 daemon 自动完成。取消另立(`cancel_task`)。
- `completed` 不等于成功:能力调用的效果在 `anet.effect_status`,回执是否核验在 `anet.receipt_verified`,
  两者原样报告、不合并。回执核验同时绑定本节点发出的请求(request CID)与收到的交付物字节。

### 1.4 本机 A2A 接口

- daemon 在 `127.0.0.1` 上提供标准 A2A 接口(JSON-RPC 与 HTTP+JSON 两种绑定,含流式),端口首次从 43811 起
  选定后固定,写在 `<数据目录>/modules/a2a/a2a_addr.txt`。
- 每个远端 agent 一个端点与代理卡片:`/a2a/v1/agents/<aid>`;`GET /a2a/v1/agents` 列出可用的 agent。
- 凭据是独立的 `a2a_token.txt`(0600),与控制令牌分离、只授权"本机作为请求方";控制面不认它,它也不认
  控制令牌。未经修改的 a2a-go 客户端经本机接口 → hub → 对端的全流程在联调中验证;a2a-tck 的结果见
  `docs/notes/0023`。
- `anet agents wire hermes --a2a <aid>…` 把指定远端 agent 写进 Hermes 的 `a2a_agents`;
  可选的提供侧后端可以把入站文本任务转给本机的 A2A 服务。
- 不需要本机接口的,可用 `-tags no_a2a` 构建(a2a-go 符号为 0)。

### 1.5 MCP 工具按 A2A 概念重组

14 个工具取代旧的 9 个:`list_agents` `get_agent_card` `send_message` `get_task` `list_tasks` `wait_task`
`cancel_task` `reply_task` `submit_payment` `reject_payment` `get_balance` `audit` `node_status`
`inbound_pending`。每个工具声明 readOnly/destructive/idempotent/openWorld 提示;任务输出可直接按 A2A `Task`
解析。**旧名删除,不留别名**(见 2.4)。

### 1.6 付款:a2a-x402 同任务流与支出三档

- 经中继的付费调用按 a2a-x402 v0.2 在**同一个任务**里完成:报价是任务的 `input-required`,
  付款、结算、交付依次在同一任务里推进(`payment-required` → `payment-submitted` → `payment-completed`)。
- 提供方在交给 hub 结算前核对授权的全部条款(收款方、金额、绑定到这件活的绑定值、网络、有效期),
  hub 在入口 hub 与账本 hub 两处再核;同一绑定值最多扣款一次。
- 支出分三档,daemon 是唯一执行点:**auto**(自动付,`auto_max`)、**agent**(MCP `submit_payment` 与本机
  A2A 客户端,`agent_max`/`agent_daily_max`)、**人工**(`anet pay <任务>`,终端确认,`explicit_max`/`daily_max`)。
  缺省前两档为 0;收款方须在 `payees.allow` 里(`anet payees add`)。上限用 `anet payments set` 在终端上改。
- 付款选项按"本节点付得了"排序;选了本节点付不了的选项时,在签名前以 `rail_not_payable` 拒绝并说明。
- `payments.publish_prices=false` 可以不在卡片上公布逐项价格(取舍见已知局限第 9 条)。

### 1.7 安装、接入与自检

- `anet init`:写出显式的安全缺省(已有配置只补缺省键、保留未知键,报告差异)。
- `anet doctor [--json]`:只读自检,不需要 daemon:版本与发布签名、官方清单、入站策略、支出上限、各编码
  工具接入状态、本机 A2A 端口占用者等,只有失败项才非零退出。
- `anet agents [status] | wire | unwire`:把 anet 接进 Claude Code、Codex、Cursor、opencode、Hermes
  (写前备份、幂等、有冲突即停);`anet install --agent` 保留为旧名。
- `anet update`:按内嵌发布公钥验签后原子替换,拒绝降级、过期与模块集合不符。
- 发布签名:`release.json` 与 `install.sh` 都以 SSH 签名发布,`install.sh` 在安装前验签;手动核验方法见
  README 与 SECURITY.md。
- `anet audit`、`anet verify --chain`:在 daemon 不运行时读取并逐条验签本机证据链。
- 邀请码不再上命令行:`anet hub-register` 与 install.sh 从 `ANET_INVITE` 或 `--token-file` 读取。

### 1.8 官方公共 agent

anet 项目运营一组任何人都能调用的公共 agent:`net.echo`(两个身份,分别在两个官方 hub)、确定性的文本/JSON/
A2A 校验工具、文档检索,以及一个 a2a-x402 付费演示。全部是确定性纯计算,不执行命令、不访问外网、不接受 URL;
公共能力的证据只记 CID,交互 7 天后清理(保存策略见 `deploy/official/README.md` §5)。官方身份由随二进制内嵌、
发布密钥签名的清单按 AID 标注为 `"anet.official": true`;标注只是标签,不给任何权限。

> 发布当天:按 `docs/notes/0027` 写明哪些官方 agent 已上线;若随版清单为空,改为"随后的版本加入"。

### 1.9 其他

- 任务板(`taskboard`)改为加法 tag:默认构建不含(它把卡片标题与备注明文存在 hub 并对匿名公开)。
- 控制台改用 60 秒一次性票据换会话,页面里不再有控制令牌;控制面只接受回环 Host。
- p2p 帧带版本号;直连投递在接收方提交后才确认,不确定的失败按"可能已送达"处理。
- 公共能力的证据缺省只记结果 CID(`"evidence": "cid"`),可逐项改为 `full`。

---

## 2. 破坏性变更与迁移

### 2.1 wire 2:hub 与节点同批升级

- 0.1.x 的 daemon 对 wire 2 hub 的 `/relay/*` 得到 **426**(`requires anet >= 0.2.0`);0.2.0 的 daemon 拒绝
  wire 1 的 hub。没有过渡期的双栈。
- hub 首次以 wire 2 启动时,**尚未取走的 wire-1 消息全部丢弃**(它们是明文,新 daemon 也打不开)。
  切换前请把进行中的任务收尾、把信箱取空。
- 升级方法:已装 0.1.x 的机器没有 `anet update`,请重跑一次安装脚本(它原地替换二进制);之后用
  `anet update`。升级后 `anet version` 应显示 `0.2.0`,`anet doctor` 无失败项。

### 2.2 配置

| 旧 | 0.2.0 | 做法 |
|---|---|---|
| `accept_delegations`(缺省或 `true`) | 首次启动迁移为 `inbound.policy=closed` 并从 `config.json` 删除 | 把要接单的对端写进 `peers.allow`(`anet peers allow <aid>`,脚本可直接写文件) |
| `anet accept on` / `POST /accept {"enabled":true}` | **400**,说明三种策略 | 同上;`accept off` 等同 `closed`,仍可用 |
| `modules.taskboard` | 默认二进制**拒绝启动** | 删掉该块,或改用 `-tags taskboard` 构建 |
| `control_allow_remote`、非回环 `control_addr` | 删除;非回环拒绝启动 | 远程访问用 SSH 端口转发 |
| `modules.x402.voucher_url` 为非回环 http | daemon 拒绝启动 | 前置 TLS 终端改为 https,或删除该键 |

### 2.3 控制面 API(直接调用 daemon 的程序)

下游程序的逐项迁移见 [docs/notes/0020](notes/0020-下游消费者迁移清单.md)。要点:

- `POST /end-accept` → **410 Gone**:提供方 `/end` 即完成;委派方 `/end` 是请求完成,提供方 daemon 自动完成。
- `/threads`:`status` 改为 A2A 状态(`done` → `completed`,不再有 `queued`/`ending`),`end_acc_by` 删除;新增
  `state`、`state_seq`、`trust`、`capability`、`context_id`,消息新增 `msg_id`、`metadata`,`kind` 增加
  `status`、`payment`。终态判断改为 `state ∈ {completed, failed, canceled, rejected}`。
- `/pull`:`out_dir` 必须是数据目录之外的绝对路径,文件写进新子目录 `anet-<任务 id 前 12 位>/`;以响应里
  `files[].path` 为准。
- 控制面只接受 `127.0.0.1` / `localhost` / `[::1]` 的 Host,否则 421。
- 控制台不再嵌入控制令牌:程序一律用 `control_token.txt` 的 Bearer;给人用 `anet console`。
- `/delegate` 的 `pay:true` 属人工/网关档(`explicit_max`、`daily_max`),收款方须在 `payees.allow`。
- 访客端点(`/guest/*`)与 `--guest-messages` 删除。

### 2.4 MCP

旧工具 `agents_find` `task_delegate` `task_message` `task_results` `task_inbox` `task_end` `evidence_read`
`credit_balance` 等删除,不留别名。按旧名写的客户端权限规则(例如 `mcp__anet__task_delegate`)、提示词与脚本
随之失效。重新运行 `anet agents wire <工具>` 会重写受管的 MCP 配置与技能说明。

### 2.5 hub 运营者

- hub 首次以 0.2.0 启动会**不可逆地迁移 hub.db**:丢弃全部 wire-1 中继行,删除评价内容列与
  `completed_task`,然后 VACUUM。需要约等于库大小的空闲磁盘,迁移期间持独占锁;先停服务、整库备份。
- 前置 nginx 的 `client_max_body_size` 须为 `129m`;随仓库的 nginx 配置不再保留 hub 的访问日志。
- 旧数据(周备份里的中继内容、评价内容、采集数据集、访客身份私钥等)由 `deploy/cleanup-content-v0.2.sh`
  清理(先 dry-run)。
- hub admin 删除了中继与官方 agent 的数据采集、官方 agent 的远程运维路由;官方 agent 只登记 `id/aid/hub/caps`。

---

## 3. 安全修复

0.2.0 的实现经过一轮按验收不变量(SI-1…SI-10)组织的对抗评审:48 条原始发现,去重 41 条,确认 30 条
(另有一条因排序问题漏出确认表、经复核为严重),其余被反驳。确认项均已修复并以回归测试钉住,
或作为设计取舍写进已知局限。下面按对外可披露的粒度列出。

### 3.1 同样影响 0.1.x 的问题

| 问题 | 影响 | 处理 |
|---|---|---|
| hub 结算未限制金额范围 | 超出账本整数范围的授权金额会被错误记账,可使余额在账户之间被错误转移 | hub 只接受 1..2^63−1 的金额、合计越界即拒;0.1.x 线的官方 hub 已单独修复并核查;0.2.0 的节点在自己一侧(商户核对、凭证兑付、签名、发放链审计)同样拒绝 |
| hub x402 网关不核对签名授权本身;同一笔付款可重复换凭证 | 可以低价或零成本换取卖家的全价凭证;一次付款可让卖家多次干活 | 网关核对授权的收款方与金额;同一笔付款的重试只返回当初那张凭证 |
| 提供方在结算前不核对付款条款 | 请求方可以用付给别人、少付或绑定在别的任务上的授权换取付费工作 | 提供方在交给 hub 之前核对全部条款(1.6) |
| 中继明文 | hub 能读到全部任务内容 | 端到端加密(1.2) |
| hub 前置代理的访问日志 | 访问日志记下"谁在何时查谁、取了多大",足以重建通信关系 | 随仓库的 nginx 配置不留访问日志;daemon 查对端密钥、卡片与 KEL 时把对端放在请求体里 |
| 控制令牌发往回环端口上的任意监听者 | 同机其他用户占住 daemon 的端口即可收走控制令牌 | anet 自己的客户端先核实监听者是本用户的 daemon(Linux),本机 A2A 接口端口被占时不换端口并轮换令牌 |
| 邀请码出现在进程命令行上 | 同机其他用户可读到并抢先使用 | 只经 `ANET_INVITE` 或权限收紧的 `--token-file` 传入 |
| 控制台页面嵌入控制令牌 | 能读到页面的一方即拿到全权凭据 | 一次性票据换会话,票据也不经浏览器命令行传递 |
| 任务板写入在鉴权前解码不限大小的请求体 | 未鉴权即可让 hub 缓冲任意大的请求 | 先核签名再解码;任务板默认不编入 |

### 3.2 0.2 开发期间发现、发布前已修复的问题

这些问题只存在于 0.2 的预发布代码中,已发布的 0.1.x 不受影响;列出是为了说明评审覆盖了什么。

- **入站授权**:对已有任务重发的委派可以绕过入站策略与公开能力的限制执行别的能力(严重);被拒的委派在重放
  后可能按变化了的策略被接受;并发重复投递的判重在授权之前。现在同一任务的重发必须绑定同一份请求,拒收持久
  记录,判重先于授权。
- **投递可靠性**:误投到直连地址的消息被确认后丢失;同一任务的取消先于委派送达,导致已取消的调用被执行;
  信箱游标可被陌生人钉住,陌生人发往未知任务的消息可让在线节点的信箱长期满载;附件与报价/付款状态在确认之后
  才写入,出错即永久丢失;已经直连送达的委派在 hub 路径失败后被判为"没送到";被重放的已答复委派让提供方无限次
  重发结果;启动期间到达的直连投递先于恢复被处理。现在按任务顺序投递、取消会撤回尚未送出的委派、写入与确认同
  事务、不确定的失败按"可能已送达"处理、重发限速、恢复完成后才收信。
- **结果与付款的呈现**:对端伪造的收据字段可让未付款的任务显示为已付;效果未知的调用被报成"没做";回执没有
  绑定本节点发出的请求。现在逐项核验后才呈现,效果未知按 `UNVERIFIED` 报告,回执绑定 request CID。
- **本机边界**:控制台票据经浏览器命令行可被同机用户读到;`/pull` 接受他人拥有的同名目录;配置写入先生效后
  保存,保存失败时运行状态与磁盘不一致;本机 A2A 接口对远端附件不设上限;远端提供方可以在自己的消息里埋入
  消息 id,使本机客户端的后续消息被静默归到别的任务。
- **hub 的可用性**:任一注册 agent 可以填满全局重放缓存,使全 hub 的签名请求失败;未鉴权的 JWKS 读取可触发
  无上限的 KEL 重放;联邦固定对端 KEL 时不核对其 AID。现在重放缓存按签名方分片,KEL 有大小上限且只回放一次,
  对端 KEL 按配置的 AID 核对。
- **发现**:`/find` 把 hub 提供的自由文本与"官方"标注并列。现在官方 AID 的条目只给 AID、标注与签名清单里的
  名称与能力。

### 3.3 作为已知局限写明的

- 结算金额加上公开的逐项价格可以指向所买的能力(已知局限第 9 条;可用 `publish_prices=false` 取舍)。
- 文本任务的回执覆盖的是提供方交付的对话记录,不证明请求方说过记录里的话(第 24 条)。
- 本机 A2A 接口的第三方客户端不核实监听者(第 13 条、"另外需要知道的")。

---

## 4. 已知局限

完整列表见 [KNOWN-LIMITATIONS-zh.md](KNOWN-LIMITATIONS-zh.md)(与设计 §21 编号一一对应)。最需要先知道的:

1. hub 知道谁在什么时候给谁发了多大的消息、来源 IP;连发送方也对 hub 隐藏(sealed sender)未做。
2. 前向保密以加密密钥寿命为界:一条消息在发出后至多 29 天内可被取得收件人磁盘的一方解开。
3. 官方公共 agent 是任务的另一端,看得到调用内容。
4. 对端身份首次见面即信任;没有持久记录的对端每条消息都按首次见面处理(第 6、15 条)。
5. 沙箱里的本地 agent 仍可联网。
6. 付款与评价可被关联;公开发放链显示跨 hub 付款的金额、时间与 AID。
7. 终端确认只约束"只经 MCP 或本机 A2A 接口行事"的 agent;同一系统用户下没有更强的边界。
8. 与 A2A、a2a-x402 规范有几处有意的偏离:缺省 `A2A-Version` 按 1.0、只带 contextId 的宽松续写、
   `hub:<aid>` 不符合 CAIP-2、`payment-verified` 表示已扣款等(第 7、12、19 条)。
9. 证据链通过校验不代表末尾没被截掉;证据写在业务事务之后(第 16 条)。

---

## 5. 版本与构件

| 组件 | 版本 |
|---|---|
| anet(daemon 与 CLI) | 0.2.0 |
| ANetHub | 0.2.0(wire 2) |
| ANetCore | v0.15.0 |

发布构件:darwin-arm64、darwin-amd64、linux-amd64、linux-arm64,每个平台两个变体(默认构建不含 shell 模块;
`anet-shell-*` 含)。每个构件的 sha256 与模块集合写在签名的 `release.json` 里。
