# 02 · ANet daemon 交互/任务路径盘点与 A2A 映射

## 0. 范围、版本与标注约定

- 被盘点代码:`ANet`(HEAD `840b8ea`),其依赖 `ANetCore v0.14.0`(本地 `ANetCore` 检出即 `v0.14.0`,`ANet/go.mod` 无 replace)。为核对 hub 侧行为,少量引用 `ANetHub`(HEAD `c4d08da`)。
- 对照规范:A2A `Refs/a2a/docs/specification.md` 与 `Refs/a2a/specification/a2a.proto`(HEAD `72b3761`);a2a-go `Refs/a2a-go`(v2);a2a-x402 扩展 `Refs/a2a-x402-spec-v0.2.md`。
- 路径均相对 `/data/projs/anet-oss/`。行号为阅读时的行号。
- 本机(Ink93)无 Go 工具链,本报告未运行任何测试或复现;所有结论来自阅读代码。
  - 未加标注的陈述:已通过阅读代码核实。
  - **[推断]**:由代码路径推导、未经运行验证的结论,或属于设计建议。

---

## 1. 总体数据流

```
CLI (cmd/anet)  ─┐
MCP (mcpserv)   ─┼─► 控制面 HTTP 127.0.0.1(bearer)──► Daemon 方法 ──► interactions 存储(SQLite)
Web console     ─┘                                        │
                                                          └─► relaySend ─► [模块 transport(p2p)] ─► hub /relay/send
                                                                                                        │
对端 daemon: relayLoop 每 1s ─► /relay/poll ─► dispatch ─► ingestDelegate / ingestMessage / ingestResult ─► /relay/ack
```

- 控制面路由注册:`ANet/internal/daemon/control_api.go:124-171`。
- 发送:`ANet/internal/daemon/transport.go:104-131`(按顺序尝试模块注册的 transport,hub 始终最后,`transport.go:82-87`)。
- 接收:`ANet/internal/daemon/relay.go:331-351`(轮询循环)、`relay.go:373-397`(pollOnce)、`relay.go:410-428`(dispatch)。
- 三种中继消息 kind:`delegate` / `result` / `message`(`ANet/internal/hubapi/hubapi.go:9-13`)。

---

## 2. interactions 存储(`ANet/internal/runtime/interactions/interactions.go`)

### 2.1 表结构

| 表 | 列 | 位置 |
|---|---|---|
| `interaction` | seq, id(UNIQUE), role, peer_aid, goal, status, request_cid, request_doc, result_cid, result, receipt, review, end_req_by, end_acc_by, created_at, updated_at, receipt_verified(迁移追加) | `interactions.go:164-181`, `230-235` |
| `message` | seq, interaction_id, sender_aid, kind, body, created_at, msg_id(迁移追加;部分唯一索引 `(interaction_id,msg_id) WHERE msg_id != ''`) | `interactions.go:184-196`, `220-227` |
| `attachment` | seq, interaction_id, msg_seq, name, mime, size, cid, data(内联 BLOB), created_at | `interactions.go:200-211` |

### 2.2 枚举

- `Role`:`inbound`(对方委托给我,我是 provider)/ `outbound`(我委托出去,我是 requester)(`interactions.go:41-44`)。
- `Status`:`queued` / `ending` / `done` / `failed`(`interactions.go:49-54`)。无 cancelled、rejected、expired。
- 消息 kind:`text` / `end_request` / `end_accept`(`interactions.go:57-61`)。
- `Verification`(回执能否被本节点校验):`""`(Unknown)/ `verified` / `unverified`(`interactions.go:125-131`)。三态,零值为 Unknown。

### 2.3 行为要点

- `interaction_id` 由请求方在委托时生成并随线协携带,双方以同一 id 记录(`interactions.go:14-16`)。
- `Put` 为 `INSERT OR IGNORE`,重复 id 被忽略(`interactions.go:242-254`)。该语义在 §8 D7 中有安全后果。
- `SetResult` 强制调用方声明 `Verification`(`interactions.go:256-264`);`SetFailed` 写入 `VerificationUnknown`(`interactions.go:267-269`)。`finish` 不检查角色,也不检查当前状态(`interactions.go:271-289`)。
- `AddMessage` 以空 msg_id 写入(`interactions.go:322-325`);`AddMessageID` 以发送方 msg_id 去重(`interactions.go:334-378`)。`Messages()` 的 SELECT 不返回 msg_id(`interactions.go:381-384`)。
- `SetEndRequested` 仅在 `queued/ending` 时生效并置 `ending`(`interactions.go:452-469`);`SetEndAccepted` 不检查状态(`interactions.go:472-487`)。
- `List(role, status, sinceSeq, limit)`:`ORDER BY seq`(升序),`limit<=0` 取 100(`interactions.go:491-517`)。无 `updated_at` 索引,无按状态时间排序,无上下文分组。
- 无过期/TTL 机制(全文件无相关代码)。

---

## 3. 出站委托(requester 侧)

### 3.1 文本委托 `Delegate` / `DelegateAtts`(`ANet/internal/daemon/relay.go:141-203`)

1. 要求已配置 hub、不得委托给自己(`relay.go:151-157`)。
2. 构造并签名最小 TaskDoc(`ANet/internal/daemon/daemon.go:281-291`),`requestCID = anetcid.Sum(doc)`(`relay.go:162`)。
3. `newInteractionID()` 生成 `ix_` + 32 位十六进制(`ANet/internal/daemon/delegation.go:172-179`)。
4. `ix.Put(outbound, queued)`;把 goal 作为第一条 text 消息写入,并存附件(`relay.go:170-180`)。
5. 组装 `DelegateReq{TaskDoc, Envelope, KEL, InteractionID, Attachments}`(`relay.go:185`),`relaySend(kind=delegate)`(`relay.go:190`)。
6. 证据链记 `anet.delegation.sent`(`relay.go:195-201`)。
7. 立即返回 interaction id;不等待对端(`relay.go:138-140`)。

### 3.2 能力调用委托 `DelegateCapability`(`ANet/internal/daemon/capability.go:120-215`)

- 约定:`Tasks[0].Requires` 含 `{Type:"capability", ID:<capId>}`,`Tasks[0].Contexts` 含 `{Key:"args", Value:<JSON>}`,均在签名范围内(`capability.go:1-12`, `155-159`)。
- 本地第一条消息正文为 `"invoke capability <id> args=<json>"`(`capability.go:174`),即结构化参数以文本形式进入会话记录。
- `DelegateReq.Payment` 可携带 x402 PaymentPayload(`capability.go:182-184`;字段定义 `ANetCore/delegation/delegation.go:46-54`)。

### 3.3 付费委托 `DelegateAndPay` / `PayAndRetry`(`ANet/internal/daemon/paylink.go:197-310`)

1. 先以无付款方式委托(`paylink.go:237`)。
2. `awaitQuote` 在 60 秒内轮询 `Results()` 等待首个答复(`paylink.go:259-310`, `329`)。答复为 `PAYMENT_REQUIRED` 且回执为 `verified` 才继续(`paylink.go:277-303`)。
3. `PayAndRetry` **新铸**一个 interaction id,用它签授权,再以新 id 发起第二次委托(`paylink.go:211-222`)。
4. 结果:一次付费调用 = 两个 interaction(报价 interaction 已 done 并带回执,付费 interaction 另起)。两者之间无关联字段(DelegateReq 无引用字段,`ANetCore/delegation/delegation.go:37-55`)。

---

## 4. 入站委托(provider 侧)

### 4.1 `ingestDelegate`(`ANet/internal/daemon/delegation.go:487-565`)

1. `AcceptsDelegations()` 为假则直接 ack 丢弃,**不回任何应答**(`delegation.go:488-490`)。
2. 解码、`VerifyDelegateReq`(内联 KEL 验 TaskDoc 签名)(`delegation.go:491-508`;`ANetCore/delegation/delegation.go:197-217`)。验证失败也是静默丢弃。
3. 按 `dr.InteractionID` 查已有记录;已有回执则重发已签结果并 ack(`delegation.go:528-539`)。
4. `ix.Put(inbound, requesterAID, goal, requestCID, taskDocBytes)`(`delegation.go:542`)。仅首次写入 goal 作为第一条消息(`delegation.go:551-558`)。
5. 若为能力调用,进入 `runCapabilityCall`;否则留在 `queued`,等待操作者的外部 agent 或 auto-reply 处理(`delegation.go:561-564`)。

### 4.2 能力调用执行(`delegation.go:585-632`, `ANet/internal/daemon/capability.go:284-461`)

- 无 provider 解析该能力:有 auto-reply 配置则交给 auto-reply;否则回 `UNAVAILABLE`(`capability.go:288-321`)。
- provider 声明的超时 ≤60s 在轮询循环内同步执行;更长的在独立 goroutine 执行,并发上限 4,超出时回 `UNAVAILABLE`(`capability.go:51`, `67`, `delegation.go:598-631`)。长调用在接受时即 ack,属于"至多一次"语义(`delegation.go:578-584`)。
- 定价能力:无支付模块 → `UNAVAILABLE`;无付款 → `PAYMENT_REQUIRED` 报价;结算失败 → `PAYMENT_REQUIRED` + 原因;结算成功 → 记 `anet.payment.settled` 后执行(`capability.go:339-372`)。**只有能力调用路径会查价**,文本委托无定价入口(同处)。
- `Invoke` 的 `CallerAID` 取自存储行 `ix.PeerAID`,而非本次委托的验签人(`capability.go:323`, `374`)。
- `deliverCapabilityResult`:结果 JSON 作为 provider 的一条 text 消息写入;签 `evidence.Receipt`;`SetResult(..., StatusDone, VerificationVerified)`;证据链记 `anet.capability.effect`;以 `ResultResp{Status: done}` 中继回请求方(`capability.go:393-461`)。**无论效果状态为 OK/UNVERIFIED/FAILED/UNAVAILABLE/PAYMENT_REQUIRED,interaction 状态一律为 `done`,线协 Status 一律为 `done`**(`capability.go:424-425`, `451-452`)。真实状态只在交付物 JSON 的 `status` 字段中(`capability.go:238-251`)。
- 能力调用路径不经过结束协商:provider 单方完成。

### 4.3 auto-reply(provider 与 requester 两侧通用)(`ANet/internal/daemon/autoreply.go`)

- 每 5 秒(或收到入站消息时被唤醒)扫描 `ActiveThreads()`(`autoreply.go:252-301`)。
- 对端提出结束 → 自动接受(`autoreply.go:307-316`)。
- "欠回复"由"最后一条 text 消息来自对端"推导(`autoreply.go:318-324`)。
- 回复上限默认 30 条,达上限则提出结束(`autoreply.go:330-351`)。
- 后端输出含 `<<ANET_TASK_DONE>>` 时发送回复后提出结束(`autoreply.go:62-66`, `408-418`)。
- exec 后端以无人值守参数启动本地编码 agent:cursor `--force --trust`(`ANet/internal/daemon/agents.go:211`)、claude `--permission-mode dontAsk --bare`(`agents.go:287`)、codex `--full-auto`(`agents.go:313`)。exec 提示词允许 agent 运行 shell 命令(`ANet/internal/daemon/autoreply_exec.go:16-18`)。

---

## 5. 会话消息

### 5.1 发送 `SendMessageAtts`(`ANet/internal/daemon/delegation.go:214-234`)

- 仅拒绝 `done`,不拒绝 `failed`(`delegation.go:223-225`)。
- 本地以 `AddMessage`(空 msg_id)写入(`delegation.go:226`),随后 `relayChat` 另行铸造 `msg_<hex>` 放入线协(`delegation.go:291-312`)。发送方本地记录不含该 msg_id。
- 线协类型 `ChatMsg{Kind, Body, Attachments, Stream*, MsgID}`,**不签名**,注释写明"Hub is trusted as the relay"(`ANetCore/delegation/delegation.go:129-179`)。

### 5.2 接收 `ingestMessage`(`ANet/internal/daemon/delegation.go:636-686`)

- 只检查 interaction 是否存在(`delegation.go:642-644`),**不检查 `fromAID == ix.PeerAID`**。
- `text`:按 msg_id 去重写入,附件经 CID 校验后写入(`delegation.go:646-662`)。
- `end_request` / `end_accept`:写日志行并更新 end_req_by / end_acc_by;`end_accept` 后触发 `maybeFinalize`(`delegation.go:663-681`)。
- 其他 kind(含 ANetCore 已定义的 `stream_preview`,`ANetCore/delegation/delegation.go:123-126`)被丢弃(`delegation.go:682-684`)。daemon 侧无流式消息处理。

---

## 6. 结束协商(`ANet/internal/daemon/delegation.go:236-436`)

### 6.1 流程

- `RequestEnd`:若对端已提出,则转为 `AcceptEnd`;若自己已提出,报错;否则写 `end_request` 日志行、置 `ending`、中继(`delegation.go:238-259`)。
- `AcceptEnd`:要求对端已提出;写 `end_accept`、中继、调用 `maybeFinalize`(`delegation.go:263-287`)。
- `maybeFinalize`:仅在 `EndReqBy != "" && EndAccBy != "" && EndReqBy != EndAccBy` 且本地角色为 inbound(provider)时执行。构建转录(仅 text 消息及附件指纹,`delegation.go:336-369`),`resultCID = Sum(transcript)`,签回执,**先** `SetResult(done)`,**后** 中继 `ResultResp`(`delegation.go:375-436`)。
- requester 收到 `ResultResp` 后在 `ingestResult` 中验证并置 `done`(§7.1)。

### 6.2 状态机(文本委托)

```
queued ──(任一方 end_request)──► ending ──(另一方 end_accept)──► [provider] done + 回执 ──relay──► [requester] done
   ▲                                  │
   └── 双方都可继续发 text ────────────┘(ending 状态下 SendMessage 仍允许)
```

- 任何一方都不能单方完成文本任务;provider 需要 requester 的接受,requester 需要 provider 的接受。
- 双方同时提出结束时:各自本地 `end_req_by` 被对端的 `end_request` 覆盖为对端 AID(`interactions.go:459-461` 的 UPDATE 无条件覆盖 end_req_by),双方都看到"对方已提出",需要再有一次动作才会完成 **[推断]**。auto-reply 开启时下一轮会自动接受(`autoreply.go:307`)。
- 无超时:对端离线时 `ending` 永久保持(无 TTL,§2.3)。

---

## 7. 结果、回执、评价

### 7.1 `ingestResult`(`ANet/internal/daemon/delegation.go:689-786`)

- `Status == failed`:直接 `SetFailed`,**无任何签名校验,也不检查角色**(`delegation.go:695-701`)。ANet 代码中没有任何地方产生 `StatusFailed` 的 ResultResp(`grep StatusFailed` 仅命中此处与常量定义)。
- 已有回执 → 视为重投,ack(`delegation.go:717-719`)。
- `VerifyResult` 将回执绑定到 interaction id、requester=自己、provider=`ix.PeerAID`、`ResultCID == Sum(deliverable)`(`ANetCore/delegation/delegation.go:260-300`)。
  - 通过 → `verified`;
  - `ErrUnverifiable`(结果不带 KEL)→ 接受为 `unverified`(`delegation.go:744-748`;`ANetCore/delegation/delegation.go:264-266`);
  - 其他错误 → 拒收,interaction 保持打开(`delegation.go:749-752`)。
- 写证据 `anet.result.accepted`(含 `receipt_verified`),并尝试记录结算(`delegation.go:773-785`, `801-836`)。

### 7.2 `Results()`(`ANet/internal/daemon/relay.go:207-250`)

- 先 `pollFresh`,再分页列出 **outbound 且 done** 的 interaction(`relay.go:213-224`)。`failed` 的 outbound 不出现。
- 输出含 `receipt`、`receipt_cid`、`provider_kel`、`receipt_verified`(`ANet/internal/daemon/delegation.go:40-62`)。

### 7.3 评价

- `SubmitReview`:仅 outbound 且有回执;签 `evidence.Review`(锚定 receipt CID),存本地(`delegation.go:440-483`)。
- `hReview` 在配置了 hub 时调用 `UploadReview`,把 **receipt、review、request_doc(完整 TaskDoc)、deliverable(完整转录)** 上传 hub(`ANet/internal/daemon/control_api.go:920-947`;`ANet/internal/daemon/hub_client.go:108-135`)。hub 对外展示的 `ReviewView` 含 `goal` 与 `deliverable` 原文(`ANet/internal/hubapi/hubapi.go:44-61`)。

### 7.4 结果投递失败时无重试

- 文本路径:`maybeFinalize` 先 `SetResult(done)`(`delegation.go:413-416`)再 `relaySend`(`delegation.go:427-429`)。若中继失败,后续再调用 `maybeFinalize` 会因 `Status == done` 立即返回(`delegation.go:380-382`)。`ingestMessage` 在 end_accept 分支记录 "retriable on next poll/accept"(`delegation.go:679-681`),但该消息已被 ack(`delegation.go:685`)。
- 能力路径:`deliverCapabilityResult` 中继失败只记日志 "requester can re-poll"(`capability.go:457-459`),但 requester 没有从 provider 拉取结果的通道;委托消息已被 ack(`delegation.go:585-632` 恒返回 true)。
- 唯一的重发路径是**委托消息被重投**时的 `resendResult`(`delegation.go:528-539`, `843-860`),它只在 provider 未 ack 委托时触发。

---

## 8. 附件(`ANet/internal/daemon/attachments.go`)

- 单个附件上限 64 MiB,内联传输(`attachments.go:23-26`)。CLI 传本地路径、控制台传 multipart 字节(`attachments.go:43-90`)。
- 接收时校验 `CID == SumRaw(data)` 与 size(`attachments.go:109-127`),存入 SQLite BLOB(`attachments.go:131-142`)。
- 转录只记录附件元数据与 CID,回执因此绑定附件内容(`delegation.go:318-330`)。
- 取回:`/pull` 写入目录(`control_api.go:745-760`;`attachments.go:155-183`);`GET /attachment` 流式返回(`control_api.go:765-790`)。
- 与 DelegateReq 一同发送的附件不在签名 TaskDoc 内,只由 CID 自证(`ANetCore/delegation/delegation.go:42-45`)。
- 无 URL 引用型附件;无分块;无外部存储(除非另行通过 `cas` 模块的能力调用)。

---

## 9. 中继 send / poll / ack 与 transport

| 环节 | 行为 | 位置 |
|---|---|---|
| send | `POST /relay/send {to_aid, from_aid, kind, interaction_id, payload(base64)}`;payload 为 CoreDet-CBOR 明文 | `ANet/internal/daemon/transport.go:31-72`(字段 `40-46`) |
| send 认证 | hub 端无认证,`from_aid` 由发送方自报 | `ANetHub/internal/aghub/server.go:882-931`, `1081-1083` |
| poll | 每 1 秒;KEL 签名挑战;每次最多 100 条 | `ANet/internal/daemon/relay.go:35`, `266-280` |
| dispatch | 按 kind 分派;返回 false 表示暂存失败、不 ack | `relay.go:410-428` |
| ack | 处理成功后 ack;至少一次投递 | `relay.go:373-397`, `283-294` |
| 去重 | 委托/结果按 interaction id;聊天按发送方 msg_id | `delegation.go:528-539`, `717-719`, `652-659` |
| 其他 transport | 模块注册的 transport 先于 hub;入站经 `inbound.Receive` 进入同一 dispatch | `transport.go:82-131`, `178-191` |
| p2p 帧 | payload 同样为明文,仅"可验证、不可伪造" | `ANet/module/p2p/transport.go:51-53` |
| 大消息 | 单次 poll/send 超时 15 分钟;交互式 freshness poll 12 秒 | `relay.go:37-46` |

- 端到端加密:`ANetCore/identity/encrypt.go:71-108` 提供 `SealTo` / `Open`(X25519 派生自 Ed25519,匿名发送方 sealed box),**在 ANet、ANetHub 中均无调用方**(`grep SealTo` 只命中定义与注释)。
- hub 发布 KEL:`ANetHub/internal/aghub/server.go:197`(`GET /agents/{aid}/kel`);daemon 已有拉取与验证代码(`ANet/internal/daemon/paylink.go:368-389`, `78-92`)。这是 requester 获取 provider 加密公钥的现成通道 **[推断:可用于加密寻址,但需处理 hub 返回旧 KEL 前缀的情形]**。

---

## 10. 控制面路由(`ANet/internal/daemon/control_api.go:124-171`)

鉴权:`/console`、`/ping`、`/attachment`、`GET /` 在 bearer 之外;其余全部在 bearer 之内(`control_api.go:155-170`, `326-343`)。

| 路由 | 处理 | 作用 | 与任务路径/A2A 的关系 |
|---|---|---|---|
| GET/POST /status | hStatus `367-402` | 身份、hub、接受开关、auto-reply | AgentCard 来源之一 |
| POST /hub-register | `467-495` | 注册 + 启动轮询 | — |
| POST /hub-leave | `975-994` | 注销 | — |
| POST /p2p-advertise | `997-1013` | 发布直连地址 | — |
| POST /accept | `499-514` | 接受委托开关 | 安全默认值相关 |
| POST /autoreply, /autoreply-test | `519-553` | 配置/自检 auto-reply | provider 侧执行器 |
| POST /shutdown | `407-413` | 停止 | — |
| POST /profile | `434-463` | 自述 | AgentCard description |
| POST /find | `556-577` | 查 hub 目录 | 发现 |
| **POST /delegate** | `580-664` | 新任务(文本/能力/付费) | SendMessage(无 taskId) |
| **POST /inbox** | `685-697` | 入站列表 | ListTasks(部分) |
| **POST /message** | `703-741` | 追加消息 | SendMessage(有 taskId) |
| POST /pull | `745-760` | 附件落盘 | Part 取回 |
| **POST /end** | `793-808` | 提出结束(或接受) | 无 A2A 对应 |
| **POST /end-accept** | `811-826` | 接受结束 | 无 A2A 对应 |
| **POST /results** | `907-916` | 已完成的出站 | ListTasks(部分) |
| POST /review | `920-947` | 签评价并上传 hub | 无 A2A 对应 |
| **POST /threads** | `830-839` | 全部会话 | ListTasks(部分) |
| **POST /thread** | `844-865` | 单个会话 | GetTask(部分) |
| POST /identities | `871-904` | 本机身份列表 | — |
| POST /evidence | `956-972` | 读证据链 | — |
| POST /balance, /redeem, /x402-authorize, /reconcile, /audit-hub | `1107-1137`, `1026-1105` | 支付 | x402 |
| POST /visibility | `1140-1156` | 发布范围 | — |
| GET /console | `159`;`ANet/internal/daemon/console.go:21-45` | 返回页面并**内嵌 bearer token** | 见 D10 |
| GET /ping | `160`;`console.go:50-61` | CORS 开放的存活探测 | — |
| GET /attachment | `161`, `765-790` | 无鉴权附件下载 | 见 D10 |

其他入口:
- MCP(`ANet/internal/mcpserv/mcpserv.go:39-200`):`agents_find`、`task_delegate`、`task_results`、`task_inbox`、`task_message`、`task_end`、`evidence_read`、`credit_balance`、`node_status`。无读取单个会话、`end-accept`、评价、附件的工具。`task_end` 走 `/end`,在对端已提出时等价于接受(`delegation.go:246-248`)。
- CLI 动词到路由:`ANet/cmd/anet/main.go:1326-1524`。

---

## 11. 与四项已定决策的冲突点(daemon 侧)

| 决策 | 现状 | 位置 |
|---|---|---|
| (2) hub 只做传输、不得看到任务内容 | 中继 payload 为 CBOR 明文;goal、消息正文、附件字节、交付物均可被 hub 解码 | `transport.go:40-46`;`ANetCore/delegation/delegation.go:9-11`, `37-55`, `133-136` |
| 同上 | hub 管理端的 harvester 逐条解码 relay 载荷(goal、正文、交付物)写入数据集 | `ANetHub/internal/admin/harvest.go:24-29`, `152-193` |
| 同上 | 评价上传完整请求与交付物,hub 对外展示 | `hub_client.go:108-135`;`hubapi.go:44-61` |
| 同上 | hub 共享任务板模块:任务内容存于 hub | `ANet/module/taskboard/taskboard.go:1-22` |
| 同上 | 聊天消息未签名,信任模型写明依赖 hub | `ANetCore/delegation/delegation.go:129-132`;`interactions.go:63-65` |
| (3) 默认不接受任何委托、不执行任何东西 | `AcceptDelegations` 未设置即为 true;`DefaultConfig` 显式置 true | `ANet/internal/daemon/config.go:41-44`, `91-94`, `114-117` |
| 同上 | 访客额度默认 5;hub 访客代理以自己的身份向任一 `guest_quota>0` 的 agent 发起真实委托 | `config.go:45-48`, `98-106`;`ANetHub/internal/aghub/guest.go:3-16` |
| 同上 | exec auto-reply 以无人值守参数运行编码 agent;与默认接受叠加时,陌生人的委托可驱动本地 agent 执行命令 | `agents.go:211`, `287`, `313`;`autoreply_exec.go:16-18` |
| (4) a2a-x402 为主路径 | 报价后 interaction 即终结,付款另起新 interaction(与 a2a-x402 的同一 task 内 input-required → payment-submitted 不一致) | `paylink.go:185-223`;`capability.go:424-425` |

---

## 12. 缺陷与风险清单

每项给出位置、行为、影响、发现方式。未运行复现者标注。

**D1 中继载荷明文。** 位置:`ANet/internal/daemon/transport.go:40-46`;`ANetCore/delegation/delegation.go:37-55`, `133-179`。行为:DelegateReq/ChatMsg/ResultResp 以 CoreDet-CBOR 明文经 hub 中转。影响:hub 运营方与任何能读 hub 数据库者可读取全部任务内容;与决策 (2) 冲突。发现方式:阅读代码;`ANetHub/internal/admin/harvest.go:152-193` 为 hub 侧实际解码的代码。

**D2 评价上传内容。** 位置:`ANet/internal/daemon/hub_client.go:108-135`。行为:`/review` 在有 hub 时自动上传 request_doc 与 deliverable。影响:决策 (2) 下内容外泄;评价与内容未解耦。发现方式:阅读代码。

**D3 默认接受委托、默认接受访客。** 位置:`ANet/internal/daemon/config.go:91-94`, `114-117`, `98-106`。行为:新安装即接受委托,访客额度 5。影响:与决策 (3) 冲突。发现方式:阅读代码;`TestRelayDelegationRefusedWhenNotAccepting` 只覆盖显式关闭的情形(`ANet/internal/daemon/relay_test.go:194-220`)。

**D4 聊天与结束协商可由第三方注入。** 位置:`ANet/internal/daemon/delegation.go:636-686`(仅检查 interaction 存在,`642-644`);`ANetHub/internal/aghub/server.go:882-931`, `1081-1083`(`/relay/send` 无认证)。行为:任何知道 interaction id 的一方,可以任意 `from_aid` 向双方注入 text、end_request、end_accept。影响:(a) 注入的 text 进入 provider 签名的转录,并被标为 "requester"(`delegation.go:362-365`);(b) 在 provider 已提出结束时,伪造的 end_accept 满足 `EndReqBy != EndAccBy`,provider 即签发回执(`delegation.go:383-385`);(c) hub 在 relay 信封中持有全部 interaction id(`transport.go:44`),因此 hub 具备该能力。`relay.go:8-9` 的"the Hub cannot forge an interaction"对委托与结果成立,对聊天与结束协商不成立。发现方式:阅读代码 **[利用路径为推断,未运行复现]**。

**D5 failed 结果无校验、无角色检查。** 位置:`delegation.go:695-701`;`interactions.go:271-289`。行为:任何 `Status=failed` 的 ResultResp 都会把同 id 的 interaction(含 inbound)置为 failed。影响:第三方可终止他人任务;ANet 自身从不产生该状态,此分支只接收外部构造的载荷。发现方式:阅读代码与 grep。

**D6 不带 KEL 的结果被接受。** 位置:`delegation.go:744-748`。行为:无 KEL 时跳过全部绑定检查,以 `unverified` 置 done,且不检查 interaction 角色。影响:第三方可以任意交付物完成出站(或入站)任务;状态虽标 `unverified`,但 `Results()` 与 MCP 均列为已完成(另见 D9)。`paylink.go:288-302` 仅在付款路径防护了这一点。发现方式:阅读代码 **[对 inbound 行的影响为推断]**。

**D7 复用已有 interaction id 的委托以原请求方身份执行能力调用。** 位置:`delegation.go:528-563`;`interactions.go:249-252`(`INSERT OR IGNORE`);`capability.go:323`, `374`, `457`;`ANet/module/shell/provider.go:85-110`。行为:新委托的验签人 `requesterAID` 未与已存行的 `PeerAID` 比较;`Put` 被忽略;随后能力调用以已存行的 `PeerAID` 作为 `CallerAID`,结果发给原请求方。影响:知道某个未完成 inbound interaction id 的签名者,可以该 interaction 原请求方的身份调用能力;shell 模块的调用方白名单按 `CallerAID` 判断,因此可被绕过;回执的 `RequestCID` 仍是原请求。发现方式:阅读代码 **[推断,未运行复现;需要一条变异验证过的测试]**。

**D8 Inbox / Threads 只取最旧的 N 条。** 位置:`delegation.go:136`(`List(role, "", 0, 1000)`)、`delegation.go:187`(limit 0 → 100);`interactions.go:491-502`(`ORDER BY seq` 升序 `LIMIT`)。行为:`Inbox` 返回最旧 100 条;`Threads`/`ActiveThreads` 每个角色返回最旧 1000 条,`ActiveThreads` 在 Go 中过滤终态(`delegation.go:141-143`)。影响:入站超过 100 条后,`anet inbox` 与 MCP `task_inbox`(非 pending)看不到新任务;超过 1000 条后,`/threads`、`/thread`(对新任务返回 404)、auto-reply 循环均看不到新任务。`Results()` 已按游标分页(`relay.go:213-224`,测试 `relay_test.go:155`),两处实现不一致。发现方式:由 SQL 与调用参数推导 **[未运行复现]**。

**D9 MCP `task_results` 丢弃 `receipt_verified`。** 位置:`ANet/internal/mcpserv/mcpserv.go:100-111`(描述称每条结果带可否验证)与 `mcpserv.go:259-267`(`resultView` 无该字段)。行为:JSON 解码时该字段被丢弃。影响:通过 MCP 使用 anet 的 agent 无法区分 verified / unverified / unknown,与"不知道 ≠ 知道没问题"的约定冲突。发现方式:grep `receipt_verified` 仅命中描述文本。

**D10 本地控制面的无鉴权路由泄露 token 与内容。** 位置:`control_api.go:155-161`;`console.go:21-45`;`control_api.go:765-790`。行为:`GET /console` 不经 bearer 即返回内嵌 token 的页面;`GET /attachment` 不经 bearer 返回附件字节;无 Host 头校验。影响:本机任意用户的进程可经回环端口取得控制 token,绕过 `control_token.txt` 的 0600 权限(`control_api.go:52`);**[推断]** 浏览器 DNS 重绑定可读取该页面。若 A2A 接口放在同一 127.0.0.1 监听上,其鉴权同样受此影响。发现方式:阅读代码。

**D11 拒绝与验证失败不回应答。** 位置:`delegation.go:488-490`, `491-508`。行为:不接受或验签失败的委托被 ack 丢弃,请求方不收到任何消息。影响:请求方的 interaction 永久 `queued`,无法区分"被拒绝"、"对端离线"、"仍在处理"。发现方式:阅读代码。

**D12 发送方不保存自己铸造的 msg_id。** 位置:`delegation.go:226`, `291-312`;`interactions.go:322-325`, `381-384`。行为:同一条消息只在接收方有 msg_id。影响:A2A `Message.messageId` 为必填、由创建方生成(`Refs/a2a/specification/a2a.proto:262`);当前存储无法在发送方回放带 id 的 history,也无法接受客户端提供的 messageId 做幂等(`Refs/a2a/docs/specification.md:492-497`)。首条 goal 消息在线协上没有 msg_id(`relay.go:185`)。发现方式:阅读代码。

**D13 结果投递失败后无重试。** 位置:§7.4 所列行。行为:provider 本地已置 done,中继失败后无出站重试队列。影响:hub 短暂不可用即可导致 requester 永久收不到结果与回执。`delegation.go:680` 与 `capability.go:458` 的日志文字与实际行为不符。发现方式:阅读代码 **[未运行复现]**。

**D14 能力调用的五种效果状态都落为 `done`。** 位置:`capability.go:424-425`, `451-452`;`relay.go:215`。行为:FAILED、UNAVAILABLE、PAYMENT_REQUIRED 也作为"已完成的结果"出现在 `Results()`。影响:任何只读 interaction 状态的消费者会把失败读作完成;A2A 映射时必须解析交付物 JSON。发现方式:阅读代码。

**D15 注释与代码不一致(影响后续设计判断)。**
- `ANet/module/module.go:71-73` 称 hub 不发布 KEL;`ANetHub/internal/aghub/server.go:197` 与 `paylink.go:368-389` 使用 `GET /agents/{aid}/kel`。
- `ANet/module/x402/voucher.go:26-31` 称 relay 具有"hub 从不看到请求与结果"的性质,与 D1 不符;`voucher.go:38-42` 称该文件在内核中而非模块 tag 后,而文件首行为 `//go:build !no_x402`,位于 `module/x402`。
- `ANet/internal/daemon/daemon.go:21-26` 称 daemon 不持有 P2P transport,与 `transport.go:89-95` 的模块 transport 注册不符。
- `delegation.go:840-842` 称 `resendResult` 失败时消息保持未 ack,而调用方 `delegation.go:537-538` 恒返回 true(ack)。
发现方式:阅读代码。

**D16 其他。**
- `SendMessage`/`RequestEnd`/`AcceptEnd` 只拒绝 `done`,允许在 `failed` 上继续(`delegation.go:223-225`, `243-245`, `268-270`)。
- `hThread` 为取一个会话构建全部会话(`control_api.go:853-863`),GetTask 成本随历史线性增长。
- interaction 无过期;长调用崩溃后请求方永久 `queued`(`delegation.go:582-584` 明确接受此结果)。

---

## 13. A2A 映射

### 13.1 前提:两种角色的非对称

依据决策 (2),外部 A2A 客户端通过自己本机的 daemon 在 127.0.0.1 上的 A2A 接口进入网络。据此:

- **请求侧**(本机 A2A 客户端 → 本机 daemon):本机 daemon 对客户端而言就是 A2A server。它代表远端 agent 接受 SendMessage、铸造 task id、汇报状态。此方向与 A2A 模型直接对应。
- **提供侧**(远端委托 → 本机 daemon → 本机 agent):A2A 没有"server 拉取待办任务"的操作。本机 agent(Claude Code、Codex 等 CLI)通常不是 A2A server。现有接入方式是 MCP/CLI 拉取(`task_inbox` 等)与 exec auto-reply(`autoreply_exec.go`)。只有当本机 agent 自身暴露 A2A server 时,daemon 才能以 A2A client 身份转发 **[推断]**。a2a-go 中与 exec auto-reply 对应的抽象是 `AgentExecutor`(`Refs/a2a-go/a2asrv/agentexec.go:97-122`)。
- 远端两 daemon 之间的线协是 anet 自有的加密中继,不是 A2A 绑定。A2A 允许以 URI 标识自定义绑定(`Refs/a2a/docs/specification.md:1277-1302`)**[推断:可作为向 A2A 贡献的方向之一]**。

### 13.2 Task 对象

| A2A 字段 | anet 现有对应 | 差异 |
|---|---|---|
| `id`(server 生成,`a2a.proto:170`;`specification.md:610-615`) | `interaction.id`,由 requester daemon 铸造(`delegation.go:172-179`) | 从本机客户端视角,本机 daemon 就是 server,由它铸造符合规范 **[推断]**。须拒绝客户端为新任务指定 id(`specification.md:615`)。提供侧若转发给本机 A2A agent,该 agent 会另铸 task id,需要 interaction↔本地 task id 的映射列 **[推断]** |
| `contextId`(`a2a.proto:173`;语义 `specification.md:584-602`) | 无。存储无列(`interactions.go:164-181`),DelegateReq 无字段(`ANetCore/delegation/delegation.go:37-55`) | 需新增:本机 daemon 在缺省时铸造;经加密载荷携带给 provider,供其按上下文分组(例如 exec 会话按上下文复用,当前按 interaction 绑定 `autoreply_exec.go:115-150`)**[推断]** |
| `status.state` | `queued/ending/done/failed` + 交付物内效果状态 + 回执可验证性 | 见 13.3 |
| `status.message` | 无独立状态消息;最后一条消息即状态说明 | 需要 provider→requester 的状态更新线协类型 **[推断]** |
| `status.timestamp` | 无;`updated_at` 在每条消息写入时都会更新(`interactions.go:365`, `375`) | ListTasks 要求按状态时间降序(`specification.md:257-259`),需独立的状态时间列 **[推断]** |
| `history` | `message` 表(不含 end 行);`Messages()` 不返回 msg_id | 见 13.5;D12 |
| `artifacts` | 无独立概念。文本任务的"结果"是转录(`delegation.go:336-369`),能力任务的结果是交付物 JSON(`capability.go:238-251`) | 见 13.5 |
| `metadata` | 无 | 回执、回执验证态、request/result CID、效果状态、证据应放入扩展命名空间的 metadata **[推断]** |

### 13.3 TaskState(9 态)映射

anet 当前有三条相互独立的轴,A2A 只有一条 TaskState:

1. interaction 生命周期:`queued/ending/done/failed`(`interactions.go:49-54`)。
2. 效果状态:`OK/UNVERIFIED/FAILED/UNAVAILABLE/PAYMENT_REQUIRED`(`ANetCore/effect/effect.go:17-36`),只存在于能力调用交付物中。
3. 回执可验证性:`verified/unverified/unknown`(`interactions.go:125-131`)。

轴 2 的 UNVERIFIED("执行了,但效果不可验证")与轴 3 的 unverified("回执签名无法检查")含义不同,不得合并。

| A2A TaskState(`a2a.proto:189-207`) | anet 来源 | 说明 |
|---|---|---|
| UNSPECIFIED | — | 不使用 |
| SUBMITTED | requester 侧:`queued` 且尚无任何来自 provider 的消息;provider 侧:刚入库 | hub 接收(`/relay/send` 200)不代表 provider 已取件;anet 无送达回执 |
| WORKING | `queued`,最后一条 text 来自 requester;或 requester 已提出结束、待 provider 接受 | anet 无显式"处理中"信号,只能由会话推导(与 `autoreply.go:318-324` 的推导同源)**[推断]** |
| INPUT_REQUIRED | (a) `queued`,最后一条 text 来自 provider;(b) provider 已提出结束、待 requester 接受;(c) 能力调用返回 PAYMENT_REQUIRED(a2a-x402 要求) | (a) 无法区分"provider 发了进度"与"provider 在提问";(c) 与 anet 当前"报价即终结"冲突(13.8) |
| AUTH_REQUIRED | 无 | shell 白名单拒绝当前返回 UNAVAILABLE(`module/shell/provider.go:100-110`) |
| COMPLETED | 文本任务:双方结束并收到回执;能力任务:效果 OK 或 UNVERIFIED | metadata 必须同时给出效果状态与回执验证态;否则 COMPLETED 会被读作"已验证成功" |
| FAILED | `failed`;能力效果 FAILED | 文本任务无产生 FAILED 的发送方代码(D5) |
| CANCELED | 无 | 无取消操作(13.7 CancelTask) |
| REJECTED | 能力效果 UNAVAILABLE(不服务、无支付模块、白名单拒绝);不接受委托;委托验签失败 | 后两者当前不回应答(D11);"并发已满"的 UNAVAILABLE 属暂时性(`delegation.go:605-621`),映射为终态 REJECTED 会丢失"可重试"信息 **[推断]** |

诚实状态约束 **[推断,供设计阶段决定]**:TaskState 只表示协议层生命周期;anet 的效果状态与回执验证态以扩展 metadata 原样携带,并在 A2A 扩展规范中写明"COMPLETED 不蕴含 OK"。

### 13.4 结束协商与 server 宣告完成

- A2A:server 单方把任务推进到终态;终态任务不可再接收消息、不可重启(`specification.md:176`;`Refs/a2a/docs/topics/life-of-a-task.md:79-82`)。
- anet 文本任务:双方同意才完成,完成时刻 = provider 签发回执(§6)。requester 能阻止 provider 完成;provider 离线时 requester 也无法完成。
- anet 能力任务:provider 单方完成(§4.2),与 A2A 一致。
- 对应关系 **[推断]**:
  - provider 的 `end_request` ≈ A2A server 声明"交付完毕,等待确认",最接近 `INPUT_REQUIRED` + metadata(例如 `anet.end=proposed`);requester 的 `end_accept` ≈ 带确认 metadata 的 SendMessage。
  - requester 的 `end_request` 不等于 CancelTask(取消表示未完成);它是"我已满意"的提议。
  - 若采用 A2A 的 server 单方完成语义,则回执需改为 provider 单方签发;"requester 确认"可以移到评价环节或回执的第二个签名 **[推断]**。

### 13.5 Message / Part / Artifact

| A2A | anet | 差异 |
|---|---|---|
| `Message.messageId`(必填,创建方生成) | 线协 `ChatMsg.MsgID`;发送方本地不存(D12);首条 goal 无 id | 需存储并回放 |
| `Message.role` user/agent | 转录里的 requester/provider(`delegation.go:318-322`);控制台的 me/them(`delegation.go:71-79`) | requester=user、provider=agent,两侧一致 |
| `Message.taskId/contextId` | interaction id 在 relay 信封中(`transport.go:44`);无 contextId | — |
| `Message.referenceTaskIds`(`a2a.proto:276`) | 无 | 报价 interaction 与付费 interaction 之间也无引用(§3.3) |
| `Message.metadata`/`extensions` | 无 | ChatMsg 需新增字段 |
| `Part.text` | `Body` | 直接对应 |
| `Part.raw` + filename + mediaType(`a2a.proto:224-243`) | `Attachment{Name, Mime, Size, CID, Data}` | 直接对应;CID/Size 放入 Part.metadata |
| `Part.url` | 无 | 仅内联,上限 64 MiB;URL 引用需要端到端可达的存储 **[推断]** |
| `Part.data`(结构化 JSON) | 能力 args 在 TaskDoc Contexts(`capability.go:155-159`);能力结果 JSON | 当前以文本形式进入会话(`capability.go:174`, `399`) |
| Artifact(`a2a.proto:280-294`;结果应走 artifact 而非 message,`specification.md:758`) | 无。provider "交付"即发消息(`control_api.go:699-702`) | 可选映射 **[推断]**:能力交付物 → DataPart artifact;provider 发出的附件 → artifact;转录 + 回执 → 带扩展 metadata 的独立 artifact |
| end_request / end_accept 行 | 作为消息存储 | 在 A2A 中应映射为状态迁移,不进入 history |

### 13.6 id 铸造方的冲突与消解

- A2A 要求 task id 由 server 生成(`specification.md:610-615`)。anet 由 requester 铸造,原因是付款授权要绑定 id(`paylink.go:211-213`)和回执要绑定共享 id(`interactions.go:14-16`)。
- 在"本机 daemon 作为 A2A server"的结构下,铸造 id 的 requester daemon 就是客户端眼中的 server,规范要求被满足 **[推断]**。
- 冲突出现在两处 **[推断]**:(1) 提供侧转发给本机 A2A agent 时,该 agent 另铸 id;(2) a2a-x402 要求付款消息引用**同一** task id(`Refs/a2a-x402-spec-v0.2.md:269-290`),而 anet 付费重试会新铸 id。
- a2a-go 的 `taskstore.Store.Create` 在 id 已存在时返回 `ErrTaskAlreadyExists`(`Refs/a2a-go/a2asrv/taskstore/api.go:79-91`),其 `NewTaskID` 生成 UUID(`Refs/a2a-go/a2a/core.go:270-272`);`ix_` 前缀的 id 作为不透明字符串可用 **[推断]**。

### 13.7 11 个 A2A 操作逐项对照

| 操作 | 现有 | 缺失 | 语义冲突 |
|---|---|---|---|
| **SendMessage** | 无 taskId → `/delegate`(`control_api.go:580-664`);有 taskId → `/message`(`control_api.go:703-741`) | contextId、messageId 透传、referenceTaskIds、metadata、acceptedOutputModes、historyLength;返回 Message(非任务)形态;A2A 错误码(TaskNotFound、终态 UnsupportedOperation、ContentTypeNotSupported) | A2A 默认阻塞至终态或中断态(`specification.md:444-446`;`a2a.proto:155-160`),anet 立即返回 id(`relay.go:138-140`),provider 可能离线数日;终态判定 anet 只拒绝 done(D16);能力调用的表示方式需约定(skill id + DataPart **[推断]**) |
| **SendStreamingMessage** | 无 | 事件源、SSE/流、状态与 artifact 增量事件 | daemon 丢弃 `stream_preview`(D 见 §5.2);中继粒度为 1 秒轮询(`relay.go:35`) |
| **GetTask** | `/thread`(`control_api.go:844-865`);`ix.Get`(`interactions.go:313-316`) | artifacts、historyLength、状态时间、回执 metadata;`/thread` 不含 `receipt_verified` 与结果 | `/thread` 为单条构建全部会话(D16);超过 1000 条后返回 404(D8) |
| **ListTasks** | `/threads`、`/inbox`、`/results` 分别覆盖一部分 | pageToken、contextId/status/时间过滤、按状态时间降序、totalSize、includeArtifacts | `/results` 只含 outbound done;`/inbox` 只含 inbound;D8 的取最旧 N 条 |
| **CancelTask** | 无 | 路由、状态、线协消息、长调用取消(当前只有 daemon 退出时取消 ctx,`daemon.go:237`) | requester 的结束提议不是取消;长调用在接受时已 ack(至多一次),取消只能尽力而为 **[推断]** |
| **SubscribeToTask** | 无 | 同 SendStreamingMessage | 同上 |
| **Create/Get/List/Delete PushNotificationConfig** | 无 | 配置存储、webhook 投递 | 对本机客户端的 webhook 在技术上可行;daemon 当前除 hub、provider、auto-reply 后端外没有出站 HTTP **[推断]** |
| **GetExtendedAgentCard** | 无 A2A AgentCard。已有 ADP AgentCard:签名、含能力列表、`anet.pricing` 扩展、`x402-redeem` 端点(`ANet/internal/daemon/card.go:32-58`, `185-220`),注册时上传 hub(`hub_client.go:75-82`),hub 提供 `GET /agents/{aid}/card`(`ANetHub/internal/aghub/server.go:198`) | A2A 必填字段:description、supportedInterfaces、version、capabilities、defaultInput/OutputModes、skills(`a2a.proto:362-399`);本机 `/.well-known` 发现路径 | ADP 卡签名为 detached JWS(`card.go:28-31`),A2A 卡签名为 JWS(`a2a.proto:457-467`),规范化规则需核对 **[推断]**。远端 agent 的卡由本机 daemon 从 ADP 卡翻译并在本机提供,`AgentInterface.tenant`(`a2a.proto:351`)或按 AID 分路径可用于区分远端 agent **[推断]** |

### 13.8 a2a-x402 映射

| a2a-x402 v0.2 | anet 现状 | 差异 |
|---|---|---|
| 报价:task 置 `input-required`,`status.message.metadata` 含 `x402.payment.status=payment-required` 与 `x402.payment.required`(`Refs/a2a-x402-spec-v0.2.md:159-188`, `400-407`) | `capabilityResult{status: PAYMENT_REQUIRED, payment_required: PaymentRequired}`,interaction 置 done 并签回执(`paylink.go:167-183`;`capability.go:393-461`) | anet 报价是终态、带签名回执;a2a-x402 报价是中断态 |
| 付款:同一 taskId 的新 Message,metadata 含 `payment-submitted` 与 `x402.payment.payload`(`:267-290`) | 付款放在**新委托**的 `DelegateReq.Payment` 中,授权绑定新 interaction id(`paylink.go:211-222`;`ANetCore/delegation/delegation.go:46-54`) | 需要 provider 接受"对已有 interaction 的付款消息"并在同一 interaction 内继续;或 A2A 层把两个 interaction 合成一个 task **[推断]** |
| 完成:`completed`,metadata 含 `payment-completed` 与 `x402.payment.receipts`(`:355-382`, `409`) | 交付物 `paid{transaction, amount, network, receipt}`,receipt 为 hub 签名的结算回执(`capability.go:253-266`) | 字段可映射;anet 的 hub 签名结算回执可放入 SettleResponse 的扩展字段 **[推断]** |
| 失败:`payment-failed` + `x402.payment.error`(`:430-472`) | 结算失败 → `PAYMENT_REQUIRED` + message(`capability.go:353-363`) | 错误码集合不同(`ANetCore/payment/payment.go:410-426` vs 规范 `:434-444`) |
| x402 版本:示例为 `x402Version: 1`、`maxAmountRequired`(`:185`, `:241`) | anet 为 x402 v2 形态:`Version = 2`、字段 `amount`、载荷 `accepted`(`ANetCore/payment/payment.go:48`, `89-105`) | 版本差异需在扩展声明中说明 |
| scheme:规范面向链上支付(`:5`) | `anet-credit`,hub 账本作为 facilitator(`payment.go:56-60`) | 非链上 scheme;hub 因结算会看到付款元数据(金额、付款方、收款方、interaction id),不含任务内容 **[推断:需确认是否符合决策 (2)]** |
| 激活:`X-A2A-Extensions` 头(`:426-428`) | 无 | A2A v1 规范的服务参数名为 `A2A-Extensions`(`Refs/a2a/docs/specification.md:485`) |
| 适用范围:任意 skill | 仅能力调用路径查价(`capability.go:339`) | 文本委托无定价 |
| SDK 支持 | a2a-go 无 x402 代码(`grep x402 Refs/a2a-go` 无结果) | 需自行实现扩展 |

---

## 14. 对设计阶段的输入 [推断]

以下为基于本次盘点的建议,不是已核实事实。

1. 线协:DelegateReq / ChatMsg / ResultResp 整体端到端加密(发送方签名 + 接收方 sealed box);`kind` 与 `interaction_id` 是否仍留在 relay 信封中需要明确取舍。ChatMsg 需加发送方签名,并在接收时校验签名人等于 `ix.PeerAID`。
2. 入站校验:`ingestMessage`/`ingestResult` 校验发送方与角色;`ingestDelegate` 对已存在 id 比较验签人与 `PeerAID`;failed 结果需要签名。
3. 安全默认值:`AcceptDelegations` 与 `GuestMessages` 默认关闭;评价上传只传回执与评价、不传内容;`/console` 不再无鉴权返回 token,加 Host 校验。
4. 存储:新增 context_id、状态时间、A2A 状态(或可推导的状态字段)、本机 A2A task id 映射、发送方 msg_id、artifact 表、push 配置表;修复取最旧 N 条的列表。
5. 状态:新增 provider→requester 的状态/进度线协类型;明确拒绝(REJECTED)与取消(CANCELED)的线协;加出站结果重试队列。
6. A2A 接口:作为可插拔模块(`//go:build !no_a2a`)挂在 `module.Host` 上。现有 `module.Host` 不暴露任务生命周期(`ANet/module/module.go:51-116`;`module/transport.go:51-66`),需要新增一个窄的任务接缝,并按 CLAUDE.md 的符号数判据验证两个方向。
7. 付款:把报价改为中断态,付款消息进入同一 interaction;或在 A2A 层合并两个 interaction。
8. 结束协商:在"保留双方确认"与"改为 server 单方完成 + 确认移至评价"之间做决定,并据此确定回执签发时刻。
9. MCP:补 `receipt_verified`、单会话读取、结束接受、附件取回。

---

## 15. 开放问题

1. relay 信封中的 `kind`、`interaction_id`、`from_aid` 属于路由元数据还是"任务内容"?决策 (2) 的边界需要明确。
2. 文本任务的完成语义:保留双方确认,还是向 A2A 的 server 单方完成对齐?
3. 提供侧接入:A2A 没有"拉取待办"操作。提供侧是否继续以 MCP/CLI/exec 为主,A2A 只覆盖请求侧与"本机 agent 自身是 A2A server"的情形?
4. hub 作为 `anet-credit` 结算方会看到付款元数据与 interaction id,是否可接受?
5. 访客模式(hub 以自身身份代浏览器发起委托)在决策 (2)(3) 下是否保留?若保留,hub 必然看到访客内容。
6. 转录(receipt 覆盖对象)在 A2A 中以何种形式呈现:artifact、metadata,还是只在 anet 扩展中提供?
7. D4、D6、D7 的利用路径需要在联调环境中用变异验证过的测试确认。
