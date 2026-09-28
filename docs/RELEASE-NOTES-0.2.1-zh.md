# anet 0.2.1 发布说明

[English](RELEASE-NOTES-0.2.1.md)

2026-09-28 定稿;发布日期以推送标签之日为准。

anet 0.2.1 是 0.2.0 的补丁版。它修复了用真实客户端(A2A 官方 Python 与 JS SDK、Hermes、经 MCP 的 Claude Code)
测试与实验室测试网 4.25 小时混合负载长稳中发现的问题,并补上这两轮留下的四个缺口。与 **ANetHub 0.2.1** 同批发布,
内核仍是 **ANetCore v0.15.0**。

**0.2.1 与 0.2.0 线协相同。** 0.2.0 与 0.2.1 的节点和 hub 任意组合都互通,可以按任意顺序升级,不需要手工迁移。
行为上的变化列在第 4 节。

测试记录:源码树中的 docs/notes/0035(真实客户端)与 0036(测试网长稳)。

---

## 1. 真实客户端测试发现并修复的问题

- **MCP `list_tasks` 一页可达数 MB。** 列出的每个任务都整条带着最新一条消息,一条 3 MiB 的回复让列出它的每一页都有数
  MB(20 个任务 9.4 MB),超过 MCP 客户端对单个工具结果的上限,对端也能借此往请求方的模型上下文里灌任意长的文本。现在每个
  列出的任务限在约 8 KB:更长的消息换成注明字节数的说明,标 `anet.truncated`。控制面 `/tasks/list` 以 `max_task_bytes`
  接受这个上限。
- **列表、计数与重试查找随数据增长变慢。** 任务行里存着目标、请求与结果,长消息时各数 MB,SQLite 读排在它们之后的列要走完
  溢出页。真实客户端跑一小时之后,本机 A2A 接口的每次 `ListTasks` 要 14–21 秒,指名 context 的每次 `SendMessage` 约 2 秒。
  现在列表、计数、按对端与按 context 的列表、客户端重试查找(`a2a.messageId`)与 context 归属核对只读索引:同一份数据上
  `ListTasks` 0.13–0.24 秒,之后 25 分钟的长跑不再变慢。索引在 0.2.1 首次启动时建一次(`interactions.db` 每 GB 约 1 秒)。
- 测试工具 `a2aprobe responder` 在几个大任务之后不再作答,已修。
- 写明 JS SDK 的用法:卡片基址须以 `/` 结尾;按卡片的 JSON 形式验签;Node 默认 `fetch` 对阻塞调用 300 秒无应答头即放弃
  (用流式,或 `returnImmediately` 加 `GetTask`)。

## 2. 测试网长稳发现并修复的问题

- **自动付款前先把"请付款"交给了等待者(daemon)。** `auto_max` 以内的报价在本节点付款之前就发布了,阻塞的
  `/tasks/send`、MCP `wait_task`、本机 A2A 阻塞 `SendMessage` 与 A2A 流于是先返回 `input-required` /
  `needs_operator_approval`,流就此结束,而任务几秒后已被自动付款并完成(长稳中 338 次这样的调用有 152 次如此)。现在报价的
  发布推迟到付款尝试之后:付了,等待者只看到付款与 `working`;没付(超档、收款方不在名单)才看到 `input-required`。
- **并发轮询下 hub 对中继写入答"database is locked"(ANetHub)。** 先读后写的事务要升级锁,而每次轮询都在写
  `last_seen_at`,SQLite 不等待直接拒绝升级。现在 `hub.db` 的事务在开始时就取写锁,与单条语句一样在 busy timeout 内等待。
  发送方重试后都已送达,没有任务丢失,但约万分之二的发送失败过一次。
- **联邦去重的过期清理全表扫描(ANetHub)。** 每接受一条联邦转发就删除 `fed_dedupe` 中超过一周的条目,而时间戳上没有索引,
  每小时新增约 1 850 行、每条都要全表扫描。启动时在 `ts` 上建索引。

## 3. 0.2.1 补上的缺口

### 3.1 没人回答的任务不再永远等下去

提供方的拒绝通知按设计限速(`inbound.reject_notice`,每对端每小时 6 条),超出的拒绝静默丢弃;已经不在了的提供方在请求方
看来也一样。对一个提供方的调用方配额做 300 个并发调用时,64 个任务一直停在 `submitted`,直到各客户端自己超时——Hermes 的
条目要等一小时。

现在,本节点发出的任务仍为 `submitted`、对端什么都没有回(没有状态、消息或结果)、本任务已没有排队待发的消息,且委派送达后
已超过 **`no_response_after`**(缺省 15 分钟)时,判为失败:状态 `failed`,`anet.reason=no_response`,
`anet.effect_status=UNVERIFIED`——对端可能已拒绝、可能没读到、也可能正在执行。之后到达的结果仍会验签并记录在这个任务上,
不重开任务。失败记为证据 `anet.task.no_response`。

- 配置:`config.json` 顶层 `"no_response_after": "15m"`(Go 时长,`"0"` 关闭)。新的 `anet init` 会写上它;没有这个键
  的配置按 15 分钟。`anet doctor` 报告它(`tasks.no_response`),读不懂的值让 daemon 拒绝启动。
- 提供方执行长能力调用时,开始执行即发 `status{working}`,请求方不会把执行中的调用当作没有回答。0.2.0 的提供方不发:它的
  长调用超过期限时,请求方报 `no_response`,结果到达后作为迟到的结果记录。
- 文本任务同理:交给 A2A 后端(§3.2)或自动回复的任务,首轮一分钟仍未作答时,提供方发 `status{working}`;一分钟内作答的不多发
  消息。由人或运营者的 agent 手工作答、需要较久的,可以先用 MCP `reply_task`(`state` 为 `working`)告知在办。
- 见已知局限第 27 条:这个期限分不清"拒绝了但没说"与"只是慢"。

### 3.2 A2A 后端:转发失败后重试

转发给提供侧 A2A 后端(`modules.a2a.backends`)的文本任务,如果后端当时不可用(尚未监听、正在重启),要到 daemon 重启
才会再转发,期间一直留在收件箱。

现在,后端没有接下消息的转发会重试:载有消息的请求还没有完整写出就失败(socket 还不存在、拒绝连接、连接超时,或取卡片时
后端答 5xx),或后端答 503。首次等 5 秒,每次翻倍,两次之间最多 **`retry.max_interval`**(2 分钟),自首次尝试起超过
**`retry.give_up_after`**(10 分钟)即放弃。消息一经完整写给后端,后端就可能已在执行:此后连接中断或超时、后端答 500、502、
504 都不重试,免得请求方的任务执行两次。期间任务状态不变;等待重试的任务让出并发名额,一个停掉的后端不会挡住其他后端的
任务。每次重试之前 daemon 重新判定这个任务还能不能交给后端:对端不再在信任名单、任务已被作答、请求方有了更新的
消息,都会结束重试。每次尝试带同一个消息 id,后端可以识别重试。socket 路径与监听者核验不过、4xx、A2A 错误、答复无内容的
失败不重试。

- 配置:`"modules": {"a2a": {"retry": {"max_interval": "2m", "give_up_after": "10m"}, "backends": […]}}`(Go 时长;
  `"give_up_after": "0"` 不重试)。`anet doctor` 报告它们(`a2a.backends.retry`)。`give_up_after` 宜短于请求方的
  `no_response_after`。
- 证据:`anet.backend.forwarded` 在后端接下消息时记一次;转发最终没有得到回答时记一次 `anet.backend.failed`(尝试次数、
  最后的错误、是否因重试用尽而放弃)。

### 3.3 MCP:单个任务限在约 24 KB

`send_message`、`get_task`、`wait_task`、`cancel_task` 与 `reply_task` 原先整条返回任务,带一个 3 MiB 回复的任务就是
Claude Code 拒收的工具结果(缺省 25 000 token),模型连任务 id 都拿不到。

现在它们按 `list_tasks` 同样的规则把任务限在约 24 KB:放不下的部分换成注明字节数的说明,标 `anet.truncated`,并写明读全文
的办法——终端 `anet task get <task_id> --full`,文件 `anet pull <task_id>`。工具结果以文本与 structuredContent 各带一份
任务,合计仍在 Claude Code 缺省上限之内。

- 任务 metadata 放不下时(例如对方给了很长的 `anet.reason`),只去掉过大的值,本节点对任务的陈述照旧保留:
  `anet.effect_status`、`anet.reason`(不太长时)、`anet.role`、`anet.receipt_verified`、`anet.peer_aid` 等。此前
  `list_tasks` 在这种情况下只留下截断标记。

- 新 CLI 命令:`anet task get <task_id> [--full] [--history N]`,输出任务的 A2A 投影;不带 `--full` 时按同一上限截断。
- 控制面:`/tasks/send`、`/tasks/get`、`/tasks/wait`、`/tasks/cancel`、`/tasks/reply` 接受可选的 `max_task_bytes`
  (0 为缺省,返回整个任务;负数 400)。

### 3.4 放在自己私有组目录里的 Unix socket 后端

Ubuntu 缺省 umask 002 下,用户建的目录都是 0775、组为该用户自己的组,socket 路径核验拒绝放在这里的后端 socket
("writable by group …, set socket_group")。

现在,组可写的目录在其组是 socket 属主或 daemon 用户的用户私有组时接受:组名与用户名相同、是该用户的主组、组里没有别人、
也不是别的账户的主组(读 `/etc/passwd` 与 `/etc/group`;LDAP 等 NSS 来源的账户不算,`/etc/nsswitch.conf` 让账户或组
还从 files、systemd 以外的来源查找的主机上不认私有组)。其他组可写的目录仍然拒绝,除非
`socket_group` 指名该组。它信任了什么,见已知局限第 26 条。

## 4. 需要知道的行为变化

| 位置 | 0.2.0 | 0.2.1 |
|---|---|---|
| 对端什么都没回的任务 | 一直 `submitted` | 超过 `no_response_after`(15 分钟)后 `failed`、`no_response`、效果 `UNVERIFIED` |
| 长能力调用(提供方) | 请求方在结果到达前一直看到 `submitted` | 提供方开始执行时发 `status{working}` |
| A2A 后端或自动回复的首轮超过一分钟(提供方) | 请求方在回复前一直看到 `submitted` | 提供方发 `status{working}` |
| 返回单个任务的 MCP 工具 | 整个任务 | 限在约 24 KB,截掉的部分有说明 |
| 转发 A2A 后端失败 | 留在收件箱直到重启 | 后端没接下消息(没连上、503)时按退避重试,最多 10 分钟;接下之后的失败不重试 |
| 证据 | — | 新类型 `anet.task.no_response`、`anet.backend.failed` |
| 由用户私有组可写的 socket 目录 | 拒绝 | 接受 |
| `anet init` 写的 `config.json` | — | 带 `no_response_after` |
| 模块:`module.InboundTaskHost` | `InboundTasks`、`ReplyTask` | 另有 `InboundTask`;`ReplyTask` 另接受不带消息的 `working` |

## 5. 已知局限

新增第 27 条:`no_response_after` 内什么都没收到的任务,即使对方只是慢(人还没回、0.2.0 提供方的长调用、离线更久的 agent)
也判失败,之后到达的追问不保存;提供方可以先回 `working` 避免。第 26 条补充了用户私有组。完整列表见
[KNOWN-LIMITATIONS-zh.md](KNOWN-LIMITATIONS-zh.md)。

## 6. 版本与构件

| 组件 | 版本 |
|---|---|
| anet(daemon 与 CLI) | 0.2.1 |
| ANetHub | 0.2.1(wire 2;仍接受 anet ≥ 0.2.0) |
| ANetCore | v0.15.0(未变) |

发布构件:darwin-arm64、darwin-amd64、linux-amd64、linux-arm64,每个平台两个变体(缺省构建不含 shell 模块,
`anet-shell-*` 含)。每个构件的 sha256 与模块集合写在签名的 `release.json` 里。`anet update` 把 0.2.0 节点升到 0.2.1。
