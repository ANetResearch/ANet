简报 · 任务模型/入站策略/自动回复 · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:A2A-DESIGN-zh.md(r3)§4、§5、§6、SI-5、SI-6,以及它们对 §11.1(事件总线)、§8.3(同任务付款)、§3.6 第 9/10 步与启动恢复的接缝。
方法:逐条读代码与测试,不采信 0013 的"已完成"。行号以 `a2a-redesign-wip` 当前 HEAD(6295665)为准。
环境:`source /data/projs/anet-dev/.anet-env.sh` 后在 `/data/projs/anet-dev/ANet` 下 `go test ./internal/runtime/interactions/ ./internal/transcript/ ./internal/golden/ ./internal/daemon/`,全绿(已复跑相关子集)。

---

## 0. 一页结论

| 章节 | 判定 | 一句话 |
|---|---|---|
| §4.1 列与迁移 | 基本完成 | 全部列已加;`status→state` 迁移、索引、`state_at` 降序分页都在;`pay_*` 只有列与读取,没有任何写入函数 |
| §4.1 状态迁移 | 完成 | `setState`/`finish` 守卫式 UPDATE,`state_seq` 单调;事件→状态表在 `stateOnMessage` 等处落地 |
| §4.1 message 列 | 部分 | `msg_id/metadata/kind(status,payment)` 已加;首条目标消息与 StatusMsg 在线协上没有 msg_id,两侧 id 不一致 |
| §4.2 完成/取消 | 部分 | 语义实现正确并有测试;但控制面没有取消入口;付款相关分支(cancel_requested、终态收据)未做;报价仍是带回执的"结果" |
| §4.3 五态 | 部分 | provider 侧映射正确;requester 侧不保存 `ResultResp.Metadata`,`anet.reason`/`retry_after_ms` 丢失;重发结果只带 `anet.state` |
| §3.6 启动恢复 | 部分 | 只扫 `working`;长调用在"提交后、置 working 前"崩溃会永远停在 `submitted` |
| §11.1 事件总线 | 部分 | `Watch` 的"快照与订阅原子获得"成立;但状态事件可能乱序/重复,事件不带数据 |
| §5.1 配置/迁移/校验 | 完成 | 默认 closed、三份 peers 文件每次重读、`validatePolicy` 三处共用、`accept_delegations` 迁移、`DeclareUntrustedBackend` |
| §5.1 撤销 | 部分 | deny 撤销与扫描完成;openai 后端不按调用重读 deny |
| §5.2 判定顺序 | 基本完成 | 六行顺序与写入时机正确;X2 "deny 对端发往未知 ix 回 TaskNotFound" 未做(现为静默丢弃) |
| §5.3 待批队列 | 基本完成 | 表、上限、TTL、追加、批准重验、TTY 都在;MCP `inbound_pending` 属 D2 未做 |
| §5.4 `Admit` | 完成 | 接口、内核实现、兑付口调用、`Via` 与 priced-caller 测试都在 |
| §6 自动回复/沙箱 | 基本完成 | 信任门控、bwrap 沙箱、失败闭合两方向、exec 加固、证据事件与沙箱内探针测试都在;doctor 项属 D2 |
| X4 nonce/对话记录 v2 | 完成(读取方待接) | TaskDoc `anet.nonce`(private)、交付物 `nonce`、`transcript.EncodeV2/Parse`、金标向量;生产代码中尚无 `Parse` 调用方 |
| SI-5 | 部分 | inbound 键齐;`payments.*`、`payees_file`、`anet init`、`anet doctor --json` 不存在 |
| SI-6 | 未做 | 无 `internal/a2ashape` 投影;且"无结果终态的能力任务"没有 `effect_status` 取值规则 |

---

## 1. interactions 存储(`internal/runtime/interactions/`)

### 1.1 列(全部已存在)

- 新列表 `interactionAdded` `interactions.go:270-289`:`receipt_verified end_req_by end_acc_by state state_at state_seq context_id trust is_capability task_nonce pay_state pay_required pay_auth_ids pay_payload pay_receipts quote_expires_at peer_kel peer_keys`。以 `ALTER TABLE ADD COLUMN` 增量加,重复列错误忽略(`addColumn` `:388`)。
- `message` 表加 `msg_id`、`metadata`(`:337-344`),部分唯一索引 `idx_msg_dedupe(interaction_id,msg_id) WHERE msg_id!=''`(`:345-348`)——这是消息去重的全部机制,`addMessage` `:757` 用 `INSERT OR IGNORE` 判定 `stored`。
- 迁移 `migrateStatusToState` `:425-500`:按设计的 done/failed/queued/ending 规则写 `state`,`state_seq=1`,然后 `DROP INDEX idx_ix_role_status` + `ALTER TABLE DROP COLUMN status`。新索引 `idx_ix_role_state(role,state,seq)`、`idx_ix_state_at(state_at,seq)`、`idx_ix_context(context_id)` `:354-364`。测试 `state_test.go:179 TestStatusColumnMigratesToState`。
- 同库其他表:`replay`、`peer_identity`(`peers.go:333`)、`pending`(`pending.go:66`)、`outbox`(`outbox.go:34`)。四张表同在 `interactions.db`,这是第 10 步同事务写入成立的前提——新增表也必须放这里。
- `Interaction` 结构 `interactions.go:162-203` 已有全部字段;`PayAuthIDs` 以 JSON 数组存 TEXT(`scanRows` `:1101-1120`)。
- 值域常量:`State*` `:62`,`Trust*` `:99`,`Pay*` `:108`(`PayNone/Required/Submitted/Completed/Failed/Rejected`),`Msg*` `:118`(`MsgText/EndRequest/EndAccept/Cancel/Status/Payment`),`Verification*` `:214`(零值即 unknown)。

### 1.2 写入 API(现有签名)

Store(自带写锁 `s.mu`):
- `Create(n New) error`(`:527`,`INSERT OR IGNORE`,初始 `submitted`、`state_seq=1`);`Put(...)` 是旧签名包装。
- `SetState(id string, st State) (changed bool, err error)`(`:559`):`WHERE state NOT IN terminalSQL AND state != ?`;终态写入同时清 `peer_kel/peer_keys`;同状态不算变化(不推进 `state_seq`);行不存在返回 `ErrNotFound`。
- `Finish(id string, f Finish) error`(`:609`):一次写 `state+result+result_cid+receipt+receipt_verified`,只守"非终态",已终态返回 `ErrTerminal`。**注意:`f.State` 可以是非终态**(报价路径就这么用,见 §4)。`SetResult`/`SetFailed` 是包装。
- `SetLateResult(...) (bool, error)`(`:654`):终态后到达的结果,只在无回执时写,不改状态。
- `SetPeerKeys`、`SetReview`、`SetEndRequested`(历史字段)、`AddMessage/AddMessageID/AddMessageRecord`、`AddAttachment`。
- 读:`Get`、`Messages`(含 `MsgID/Metadata`)、`Attachments`、`AttachmentData`、`List(role,state,sinceSeq,limit)`(按 seq 升序,只给全量扫描用)、`ListPage(ListFilter) (Page, error)`(`:1001`,`state_at DESC, seq DESC`,游标 `"<state_at>.<seq>"`)、`ListAll`、`Count`。
- `ListFilter` `:916`:`Role States Active ExcludeTrust ExcludeCapability ContextID PeerAID UpdatedAfter HasReceipt Cursor Limit(≤1000)`。

事务写法(`peers.go:51 Store.Update(fn func(*Tx) error) error`):
- `fn` 内只允许数据库操作(持有写锁,无网络、无签名、无等待)。`fn` 返回错误即整体回滚并原样返回该错误。
- `Tx` 方法与 Store 一一对应:`Get Create Put AddMessage AddMessageID AddMessageRecord SetResult SetFailed Finish SetState SetPeerKeys SetEndRequested ClaimReplay PutPending GetPending AppendPendingFollowup DeletePending EnqueueOutbox`。
- 接收路径统一走 `daemon.commitRx(m, fn)`(`receive.go:656`):先 `ClaimReplay`,再 `fn`,再测试钩子 `d.rxFault`。`fn` 里返回 `rxPermanent(reason, err)` → 按永久拒绝计数,其他错误 → 暂时性(不 ack)。**所有入站业务写都必须经 `commitRx`**,否则失去第 10 步的恰好一次。
- 出站需重试的写走 `daemon.queueSend(ctx, toAID, typ, ix, body, write func(*Tx) error) (int64, error)`(`retry.go:59`):先在事务外封装信封,再在一个事务里执行 `write` + `EnqueueOutbox`,之后调用方 `deliverQueued(ctx, id)`。封装失败时队列存 body,重试循环再封装。

### 1.3 缺口(给实现者)

1. **`pay_*` 没有写入函数**(C3 必做)。全仓 `grep PayState` 只有读:`delegation.go:345,633,1121,1223,1245`。建议在 `interactions.go` 加(并在 `peers.go` 给 `Tx` 加同名方法):
   - `SetQuote(id string, required []byte, expiresAt int64) (bool, error)`:`WHERE pay_state IN ('', 'required') AND state NOT IN terminalSQL`,写 `pay_state='required'`。
   - `SetPayState(id string, from []string, to string, fields PayUpdate) (bool, error)`:守卫式迁移(与 `setState` 同构),`PayUpdate{AuthID string; Payload []byte; AppendReceipt []byte}`;`pay_auth_ids` 追加去重;`pay_receipts` 以 JSON 数组追加(已终态也要允许追加收据,见 §4.2 "终态收据")。
   - `ListFilter` 加 `PayStates []string`,供重启恢复 `submitted` 行(建索引 `(pay_state)` 或部分索引 `WHERE pay_state='submitted'`)。
2. **结果元数据无处存放**:`ResultResp.Metadata`、`DelegateReq.Metadata` 都不落库(见 §6)。建议新增列 `result_meta TEXT NOT NULL DEFAULT ''`(两侧都写:provider 在 `deliverCapabilityResult`/`CompleteTask` 的 `Finish` 里写自己发出的 meta,requester 在 `ingestResult` 的 `Finish` 里写收到的 meta),`Finish` 结构加 `Meta []byte`。首条消息的 `DelegateReq.Metadata` 存到目标消息行的 `message.metadata`。
3. **按客户端 messageId 去重的查询不存在**(§11.5 C5 需要):加 `FindOutboundByClientMsgID(contextID, clientMsgID string) (*Interaction, error)`,实现为 `message.metadata` 的 `json_extract(metadata,'$."a2a.messageId"')` 查询或新增列 `client_msg_id` + 索引。
4. `Store.now`(`SetClock` `:254`)与 `Daemon.clock`(`daemon.go:34`)是两套时钟,测试若要同时控制 `state_at` 与接收时间窗,需两处都设。

---

## 2. 状态机现状(§4.1 事件表 → 代码)

| 事件 | 写入点 |
|---|---|
| requester 建任务 → submitted | `Create`:`relay.go:181`(文本)、`capability.go:198`(能力) |
| requester 发 text → working | `SendMessageOpts` `delegation.go:319-375` 经 `stateOnMessage(fromRequester=true, MsgText)` |
| provider 收到 requester 的 text/payment-submitted → working;payment-rejected 且 `pay_state≠submitted` → canceled | `ingestMessage` `delegation.go:1098-1150` + `stateOnMessage` `:377-402` |
| provider 发消息无 `anet.state=working` → input-required;有则 working | 同上(`fromRequester=false` 分支) |
| StatusMsg → 其状态 | provider:`SendStatus` `:679`;requester:`ingestStatus` `:1394`(终态时直接 ack 不写) |
| 结果 → §4.3 | requester:`ingestResult` `:1285` + `resultState` `:1262`(优先读 `anet.state`,否则由 `Status`/交付物 JSON 推导) |
| requester cancel → canceled(`pay_state≠submitted`) | `CancelTask` `:615`(requester 分支 `cancelState = ix.PayState != PaySubmitted`) |
| 长能力调用开始 → working | `runCapabilityCall` `:1072` |

- 终态判断已全部换成 `IsTerminal()`,`grep` 未见旧 `status` 字符串残留(控制面 `/delegate` 返回的 `"status":"queued"` 字面量 `control_api.go:648,661,669` 除外,C5 替换)。
- 测试:`state_test.go:16/57/103/125`、`tasks_test.go:78-380`。

---

## 3. 完成与取消(§4.2)

已实现且有测试:
- provider 完成:`CompleteTask` `delegation.go:536-613`,签 v2 对话记录回执,`queueSend` 与 `Finish(completed)` 同事务;已被取消则返回 `ErrTaskTerminal`,不签回执(`tasks_test.go:78`)。
- requester `end_request` → provider daemon 自动 `CompleteTask`(`ingestMessage` `:1151-1177`)。
- requester `cancel` → provider `providerCancel` `:1218` → `CancelTask` → `status{canceled}` 经重试队列(`tasks_test.go:140`)。
- 能力调用:`capabilityStopRequest` `:1240`——已付/在付不动;执行中 cancel 只取消 context、end_request 忽略;未开始两者都 canceled(`tasks_test.go:380,419`)。
- 终态后 SendMessage → `ErrTaskTerminal`(对应 A2A UnsupportedOperation,`tasks_test.go:347`);入站 text 到终态 → `dropAfterTerminal` 并 ack。
- 出站重试队列:`retry.go`,5s 起指数退避、最长 24h、重启首轮全量重试、信封 `exp` 过期后删除并写 `anet.delivery.expired`(`tasks_test.go:498,556`)。

缺口:
1. **控制面没有取消入口**:`CancelTask`、`SendStatus`、`SendMessageOpts(meta)`、`DelegateIn(contextID)`、`DelegateCapabilityIn` 只有内部调用者(`grep` 见 `delegation.go:1226,1254`、`inbound.go:1002`、`autoreply.go:637`)。`control_api.go:132-173` 无 `/cancel`、`/tasks/*`;`/end-accept` 仍注册(返回 410,`:849`)。C5 的 `/tasks/cancel|send|reply` 直接包这些函数即可。
2. **`anet.cancel_requested` 没有表示**:requester 在 `pay_state=submitted` 时 `CancelTask` 只追加一条 `MsgCancel` 消息、状态不变(`:630-660`),没有标记。建议 a2ashape 按"存在本方 `MsgCancel` 且 `pay_state=submitted` 且非终态"推导,或加列;二选一写进 C5 任务说明。另:requester 侧 `pay_state=completed` 时 `cancelState` 为真(本地会置 canceled),设计只写了 `submitted`,C3 需确认是否也应为"不变"。
3. **终态交互上的结算收据**:`ingestStatus` 在终态直接 ack(`:1396-1398`);`recordSettlement` `:1426` 只读旧交付物字段 `paid`,不读 `x402.payment.receipts`。C3 需在两处加"终态仍验证并记录收据、不改状态"的分支。
4. **provider 结果后的 late 路径**:`ingestResult` 先在 `commitRx` 里只写重放行(判定 `late`),再在事务外 `SetLateResult`(`:1344-1370`),两步之间崩溃会丢失这份迟到结果与其结算证据(C34 用例要求付款方证据链含该笔结算)。建议把 `SetLateResult` 的 SQL 挪进同一个 `commitRx` 回调(给 `Tx` 加 `SetLateResult`)。

---

## 4. 能力调用执行路径(C3 的主要接缝)

入口:`ingestDelegate` `delegation.go:824-918` 在提交后调用 `runCapabilityCall(ix, capID, args, dr.Payment, release)`。

`runCapabilityCall(interactionID, capID string, args map[string]any, payment []byte, release func()) bool`(`:1006-1087`):
1. `d.running.LoadOrStore(ix, &runningCall{})` 进程内互斥(同一 ix 只执行一次);结束时删除并调用 `release`(公开能力的准入槽)。
2. 再读一次行:**`len(ix.Receipt) > 0 || ix.IsTerminal()` 即不执行**(`:1033`)。
3. `invokeBound(p, capID)`(`capability.go:72`):provider 实现 `provider.LongRunning` 且 `InvokeTimeout > 60s` 才算长调用。
4. 短调用:在接收 goroutine 上同步执行 `tryCapabilityPaid`(接收循环持有 `pollMu`,期间不收信)。
5. 长调用:`d.longCalls`(容量 `maxConcurrentLongCalls=4`)满则立即回 `UNAVAILABLE` + `anet.retry_after_ms=60000`(→ rejected);否则 `SetState(working)` 后起 goroutine,`d.longCallsWG` 计数。
6. `tryCapabilityPaid` `capability.go:324-409`:未解析 → UNAVAILABLE + `anet.reason=capability_not_served`;标价且无 payer → `payments_unavailable`;标价无付款 → `answerPaymentRequired`;有 `DelegateReq.Payment` → `payer.Settle` 后执行(旧预付流,**没有 §8.4 商户核对**)。
7. `deliverCapabilityResult` `capability.go:456-561`:签回执 → `queueSend` 事务内 `Finish(state, result, receipt, verified)` + 追加一条 provider 的 `MsgText`(交付物 JSON)→ 证据 `anet.capability.effect` → `publishResult` → 立即投递一次。
8. 重投:`redeliveredDelegate` `:931-956`——有回执则 `resendResult`;运行中不管;终态不管;短调用重跑;**长调用静默返回**。

**陷阱 A(阻断 C3 同任务付款):报价当前是"带回执的结果"。**
- `answerPaymentRequired`(`paylink.go:149-167`)走 `deliverCapabilityResult`,`stateForEffect(PAYMENT_REQUIRED)` → `input-required`,`Finish` 把**签名回执**写进一个非终态行,并以 `anet.result/1` 发出;requester 的 `ingestResult` 同样 `Finish(input-required)` 存回执。
- 之后所有"有回执 = 已答复"的判断都会把付款后的执行挡掉:provider `runCapabilityCall:1033` 不执行、`redeliveredDelegate:939` 重发报价;requester `ingestResult:1309` 把真正结果当"已接受"丢弃;`Results()` `relay.go:242` 把报价列为结果;`awaitQuote/PayAndRetry/DelegateAndPay`(`paylink.go:179-295`)是"报价后另开新 ix 付款"的旧设计,与 §8.3 冲突。
- 设计 §17 用例"报价后 end_request → canceled 且无回执"在现状下无法通过。
- 建议改法:报价改为 `SendStatus(ix, input-required, text, {x402.payment.status: payment-required, x402.payment.required: …})` + `SetQuote`,不签回执、不 `Finish`;requester 侧 `ingestStatus` 写 `pay_state=required`、存 `pay_required`、`quote_expires_at`。wire 2 下报价的真实性由信封签名(§3.6 第 7 步)保证,旧注释里"报价要靠回执验证"的理由(`paylink.go:262-277`)已不成立。旧 `PayAndRetry` 流程随之删除或改为同 ix。

**陷阱 B:收到 `payment-submitted` 后如何执行。** 现在 `ingestMessage` 只存 `MsgPayment` 并按 `stateOnMessage` 置 working,不触发执行。C3 在 `ingestMessage` 的 `stored && kind==MsgPayment && fromRequester` 分支里:
- 事务内核对(§8.4)并 `SetPayState('' | required → submitted, AuthID, Payload)`(同一 `commitRx` 回调,拒收第二份授权靠 `from` 条件);
- 提交后 `d.runCapabilityCall(ix, capID, args, payload, release)`,`capID/args` 由 `decodeTaskDoc(ix.RequestDoc)` + `capabilityCall(td)` 取得(`recoverInterrupted` 已有同样写法 `capability.go:581-585`);
- `tryCapabilityPaid` 改为读 `pay_state/pay_payload`,结算走 hub,成功先 `SendStatus(working, payment-verified)` 再执行;`stateForEffect(status, paid)` 的 `paid` 改为以 `pay_state==completed` 判定。
- **公开能力准入**:第 2 行的 `Admit` 槽在报价交付后就被 `release` 了(`runCapabilityCall` 的 `finish`)。付款后的执行不会再过 `max_inflight`/配额。建议付款执行前再调一次 `d.admit(from, capID, argsLen)`(或把报价期间的槽保留到过期,不推荐)。
- `SendMessageOpts` 对能力调用直接报错(`delegation.go:334-336`),requester 发 `payment-submitted` 需要单独的发送函数(建议 `SendPayment(ctx, ix, decision, accept)`,经 `queueSend` 以便重试复用同一信封)。

**陷阱 C:启动恢复会误判付款行。** `recoverInterrupted`(`capability.go:570-598`)把所有 `inbound + working + is_capability + 无回执` 置 failed/interrupted。C3 之后,`payment-submitted` 会把短调用置 working(`stateOnMessage`),而 `pay_state=submitted` 的行按 §8.3 应"重启恢复,继续用同一 payload 结算",不能判中断。需按 `pay_state` 分流。

---

## 5. §4.3 五态映射

- `stateForEffect(status string, paid bool) (State, map[string]any)` `capability.go:418-445`:OK/UNVERIFIED→completed、FAILED→failed、UNAVAILABLE→rejected(`paid` 时 failed)、PAYMENT_REQUIRED→input-required;meta 恒含 `anet.effect_status` 与 `anet.state`。
- `resultOpts{state, reason, retryAfterMS}` 写 `anet.reason`、`anet.retry_after_ms`。中断:`recoverInterrupted` 以 `UNVERIFIED` + `state=failed` + `reason=interrupted`。
- `resultState`(requester)优先取 `anet.state`,只接受 completed/failed/rejected/input-required。

缺口:
1. **requester 不保存结果元数据**:`rr.Metadata` 只用来取 `anet.state`(`delegation.go:1263`)。`anet.effect_status` 可从交付物 JSON 的 `status` 恢复,但 `anet.reason`、`anet.retry_after_ms`、x402 键全部丢失。见 §1.3 第 2 条。
2. **重发结果丢元数据**:`resendResult` `delegation.go:1466-1488` 只发 `{"anet.state": ix.State}`、`Status` 恒为 `StatusDone`。requester 首次收到的若是重发件,就拿不到 `effect_status/reason/retry_after_ms`。provider 侧存下原 meta 后,重发时原样带上。
3. `resultState` 对 PAYMENT_REQUIRED 的推导(交付物 JSON)在 §4 陷阱 A 改完后应删除。

---

## 6. `anet.*` / `x402.*` 元数据现状

| 载体 | 发送方写入的键 | 接收方是否落库 |
|---|---|---|
| `DelegateReq.Metadata` | **从不设置**(`relay.go:199`、`capability.go:218`) | 不读、不存 |
| `ChatMsg.Metadata` | `SendMessageOpts(meta)` 透传(如 `anet.state=working`);付款消息带 `x402.payment.status` | 存 `message.metadata`(`delegation.go:1128-1131`) |
| `StatusMsg.Metadata` | 拒绝 `{anet.reason[, anet.retry_after_ms]}`(`inbound.go:710`);待批 `{anet.inbound: pending_approval}`(`pending.go:105`);未知 ix `{anet.a2aError: TaskNotFound}`(`receive.go:695`);沙箱 `{anet.reason: sandbox_unavailable}`(`autoreply.go:633`);`SendStatus(meta)` 透传 | requester 存为 `MsgStatus` 行的 metadata(`delegation.go:1405-1408`,msg_id 用合成值 `"st_"+hex(mid)`) |
| `ResultResp.Metadata` | 文本 `{anet.state: completed}`;能力 `{anet.state, anet.effect_status[, anet.reason, anet.retry_after_ms]}`;重发只 `{anet.state}` | **不存**,只读 `anet.state` |

尚无任何代码产生/消费:`a2a.serviceParameters`、`a2a.messageId`、`anet.cancel_requested`、`anet.payment.accept`、`anet.receipt_verified`(列 `receipt_verified` 已有,三态)、`anet.request_cid/result_cid/peer_aid`(列已有)、`x402.payment.required/payload/receipts/error`。
判定"控制消息"的函数 `hasControlMeta` `delegation.go:421`:键以 `x402.` 开头或等于 `anet.state` 即不计入对话轮次与回执(C29)。新增控制键时同步改这里。

---

## 7. 事件总线(§11.1 前置)

实现 `internal/daemon/eventbus.go`:
- `func (d *Daemon) Watch(ix string) (*interactions.Interaction, <-chan Event, func(), error)`(`:113`):在总线锁内读快照并登记订阅,`minState = snap.StateSeq`,之后到达的 `state_seq ≤ minState` 的状态事件丢弃。**"快照与订阅原子获得"成立**(`tasks_test.go:228,298`)。
- `Event{IX, Kind(state|message|result), State, StateSeq, MsgSeq, MsgKind, At}`;缓冲 64,慢订阅者被关闭通道并移除(调用方需重新 `Watch`)。
- 发布点:所有状态写与消息写之后调用 `publishState/publishMessage/publishResult`(`grep` 列表:`capability.go:212,556`、`delegation.go:364,469,599,668,733,901,1075,1141,1165,1193,1299,1386,1414`、`pending.go:222,228`、`relay.go:199`)。`ApprovePending` 追加的后续消息没有逐条 `publishMessage`。

问题与改法:
1. **状态事件可能乱序或重复**:`publishState` 在锁外 `d.ix.Get(ix)` 读"当前"行(`:134-140`),两个并发写的发布者可能都读到较新的 seq,或较旧的后发;`subscriber.minState` 只在订阅时设置、投递后不推进(`:80-95`)。阻塞等待(比较 seq > 快照)不受影响,但 SSE 可能先推 input-required 再推一个旧的 working。改法:`publish` 对 `EventState` 在投递后置 `s.minState = e.StateSeq`(单调过滤),同一 seq 只投一次。
2. 事件不带数据:`EventResult` 无 seq、无产物;SSE 投影需重读行与消息。消息事件可能与快照后读取的 `Messages()` 重复,用 `MsgSeq` 去重(文件头注释已说明)。
3. 快照是 `*interactions.Interaction`,`TaskSeam.Watch` 要的是 a2ashape `Task`:C5 在其上包一层,快照投影时要一并读消息/附件(非原子,靠 `MsgSeq` 去重)。

---

## 8. 入站策略(§5)

- 配置 `InboundConfig` `inbound.go:57-114`,缺省 `defaultInbound()` `:119`,`normalize` 填默认值;`Config.inbound()` `:182`。`DefaultConfig`/`freshConfig`(`config.go`)写出显式 `inbound` 块;`migrateInbound` 处理 `accept_delegations`(任意值 → closed,首次启动记一行日志并重写 config)。测试 `inbound_test.go:689,728,756`。
- peers 文件:`readPeers()` `:308` 每次判定重读;deny 读失败 = 拒绝所有人;`allowed = !denied && (allow||trust)`;`trusted = !denied && trust`。写入 `editPeerFile` `:846`(原子替换、保留注释)。
- 校验:`validatePolicy(c Config, untrustedBackend bool) error` `:217`,由 `New`(模块启动之后,`daemon.go:219`)、`SetAutoReply`(`autoreply.go:176`)、`SetInboundPolicy`/`SetPublicCapabilities`(`inbound.go:783,804`)调用;冲突返回 `ErrPolicyConflict` → 409(`inbound_api.go:118`)。
- 判定:`decideDelegate(from, capID string, argsLen int) inboundDecision` `:377`,返回 `{action accept|hold|refuse, trust, reason, retryAfterMS, release}`;调用点 `authorizeDelegate` `receive.go:332-410`(第 9 步,只判定并发拒绝回复)。接受 → `ingestDelegate`,暂存 → `holdDelegate`(`pending.go:63`),都在 `commitRx` 内。
- 拒绝回复:`replyRejected` → `sendNoticeStatus` `inbound.go:721` → `sendNotice` `seal_send.go:273`(一次性、后台、不入队、不写 peer_identity);限速 `noticeLimiter` 默认每对端 6/h、全局 60/min(`receive.go:948`)。聚合证据 `flushInboundSummary`(每分钟检查、10 分钟窗)、明细 `inbound-refused.log` 轮转 1 MiB。
- 撤销:`DenyPeer` → `cancelForPolicy`(两种角色的活动交互都 `CancelTask`)+ `anet.policy.changed`;`revocationSweep` 每分钟一次(`receive.go:1016-1020`)覆盖手工编辑的 deny 文件。`authorizeMessage` 对 `trust=peer` 入站交互再查 allow(`receive.go:463-466`)。
- 待批:见 `pending.go` 全文;`PendingView` 只含元数据;`ApprovePending` 用 `reverifyPending` + `keyStateUsable` 以"现在"重验轮换宽限;批准后 `trust=approved`,能力调用走 `runCapabilityCall`(不过 `Admit`)。CLI 的 approve/allow/trust/approve|open 策略需 `/dev/tty`(`cmd/anet/peers.go:36-56`)。
- `Admit`:接口 `module/module.go:136`,内核 `admitAt` `inbound.go:482-543`(先检查全部限额再扣减,`release` 由 `sync.Once` 保护),`moduleHost.Admit` `:546`;兑付口 `module/x402/voucher.go:275`,`provider.Call.Via` `provider/provider.go:31-50`,`provider/priced_caller_test.go`。
- 注意 `receiveEnvelope` 在第 10 步后若 `m.release` 仍非空(重复、存储错误)会归还准入槽(`receive.go:219-227`)。

缺口:
1. **X2:deny 对端发往未知 ix 的消息**。`authorizeMessage` 先查 deny 就 `dropDenied` 静默丢弃(`receive.go:430-434`),而陌生人发往未知 ix 会在 10 分钟后收到 TaskNotFound(`:449-455`)。`closed` 下两者可区分,与 X2 冲突。改法:deny 判定挪到"ix 存在"之后;ix 不存在时 deny 对端与陌生人走同一分支(窗口内 T、超窗回 TaskNotFound、同一限速桶)。`approve/open` 下是否也这样,按 X2 "`open`/`approve` 下 deny 可察觉"可保持现状。补测试:`inbound_test.go` 仿 `TestTheInboundDecisionOrder` 的"denied 与 stranger 收到相同回复"写法。
2. **openai 后端不按调用重读 deny**:`autoReplyThread` 只在 `cfg.Backend=="exec"` 时读 peers(`autoreply.go:309-321`),openai 后端在 deny 生效到 `revocationSweep` 之间(至多 1 分钟)仍会回复。把 `ps.denied(th.Peer)` 检查移到 exec 分支之外。
3. §5.3 MCP `inbound_pending`(D2):控制面 `/inbound/pending` 已有(`inbound_api.go:128`),工具未注册。

---

## 9. 自动回复与沙箱(§6)

已完成并有测试:
- 门控:`autoReplyThread` `autoreply.go:301`——exec 后端对非 trust 对端在 `untrusted=off` 时不运行、`sandbox` 时走 `runSandboxed`;能力调用与 `public_cap` 在 `ActiveThreads`(`delegation.go:161`,SQL 过滤)与循环内双重排除;入站/出站同一判定。测试 `autoreply_test.go:435,486`。
- 沙箱:`sandbox_linux.go`(bwrap 参数、never-bind 冲突即不可用、探针缓存 5 分钟)、`sandbox_other.go`(非 Linux 失败闭合)、`runSandboxed` 要求 `auto_reply.api_key`(`autoreply_exec.go:663-697`);失败闭合两方向 `sandboxRefusedTurn` `autoreply.go:633`;沙箱内探针测试断言读不到控制令牌/A2A 令牌、连不上数据目录 socket 与 system bus(`sandbox_linux_test.go:216`)。
- exec 加固:工作目录 `$XDG_CACHE_HOME/anet/work/<aid短>/<ix>/` 0700 且须在数据目录外、环境变量白名单、outbox 只收常规文件(16 个/64 MiB)、失败只回通用错误 + ref、目标以不可信数据呈现、`ANET_EXEC_COMMAND` 被 `execCommandForTest` 取代(`exec_hardening_test.go` 全文件)。
- 证据 `anet.autoreply.invoked` 只对 exec 记录(`autoreply.go:581-620`)。
- 完成判定:哨兵 `<<ANET_TASK_DONE>>` 或达上限 → `RequestEnd`,provider 侧即 `CompleteTask`。

剩余:doctor 报告"sandbox 模式是否配置 api_key"属 D2。

---

## 10. X4:nonce 与对话记录 v2

- TaskDoc:`signTaskDoc(goal, nonce)` `daemon.go:369` 与能力路径 `capability.go:185` 都加 `{Key:"anet.nonce", Visibility:"private"}`;`task_nonce` 列保存;provider 侧 `taskNonce(td)` 取出。
- 交付物:`buildTranscript` `delegation.go:491-530` 产出 v2(过滤非 text 与控制元数据的消息);`capabilityResult.Nonce`(`capability.go:280`)。旧请求无 nonce 时现铸一个。
- `internal/transcript`:`EncodeV2(nonce, msgs)`、`Parse(b) (Transcript, error)` 同时接受 v1/v2;金标 `internal/golden/golden_test.go`(字节与 CID);`relay_test.go:214` 断言对话记录 nonce == 任务 nonce。
- 待接:生产代码中没有任何 `transcript.Parse` 调用方。C5 的 `anet.reply` artifact("回执覆盖的对话记录中 provider 的最后一条")必须经 `transcript.Parse` 读 `ix.Result`,不要自行 `json.Unmarshal` 成数组。
- 小注意:编码用 `encoding/json`,会把 `<>&` 转义为 `<` 等;第三方只需对收到的原字节求 CID,不应重编码。

---

## 11. SI-5 与 SI-6

SI-5 现状:`inbound.policy=closed`、空 `public_capabilities` 显式写入 config;`peers.*` 缺失即空(未创建文件);`auto_reply` 块缺省即等价 `untrusted=off`(`UntrustedMode()`);**无 `payments` 配置块、无 `payees_file`、无 `anet init`、无 `anet doctor`**。现有测试 `TestAFreshInstallIsClosed` 只覆盖 inbound。待做(C3 + D2):`Config.Payments *PaymentsConfig{AutoMax, AgentMax, AgentDailyMax, ExplicitMax, DailyMax uint64; PayeesFile string}`,`DefaultConfig` 写出 0/0/0/10/50/"payees.allow";`anet init` 创建空 `peers.allow/peers.trust/peers.deny/payees.allow`;`doctor --json` 键名与 SI-5 列表一一对应,mutation 逐键改回。

SI-6 现状:无 `internal/a2ashape`。可用数据:`receipt_verified` 三态列(`ResultItem.ReceiptVerified` 用 `omitempty`,为空时字段缺失,投影需输出 `unknown`,`delegation.go:88`)、能力任务交付物 JSON 的 `status`。缺:
- requester 侧 `result_meta`(§1.3-2);
- **没有结果的终态能力任务的 `effect_status` 取值**:策略拒绝(`status{rejected, not_accepting}`)、`TaskNotFound`(failed)、执行前取消(canceled)、待批过期(rejected)都没有交付物。SI-6 要求"能力任务在终态缺 `anet.effect_status` 即失败",§4.3 只定义了结果路径。建议规则写入 a2ashape:rejected/failed 且无结果 → `UNAVAILABLE`;执行前 canceled → `UNAVAILABLE`;执行中被取消后由 provider 结果决定;中断 → `UNVERIFIED`。需产品负责人确认或在设计中补一行。

---

## 12. 测试夹具

- 两节点 + fake hub:`registeredPair(t) (srv, req, prov)`(`sealtest_helpers_test.go:176`,prov 已 allow req);`newTestDaemon(t, hubURL, accept bool)`(`relay_test.go:32`,停掉后台轮询,用 `d.pollOnce(ctx)` 手动收信);`registered(t, srvURL, name)`(`inbound_test.go:28`)。
- 名单:`allowPeers/trustPeers/denyPeers(t, d, aids...)`(`relay_test.go:92-110`,直接写文件,与脚本一致);`setPolicy(t, d, p)`(`inbound_test.go:79`)。
- 直接喂信封:`sealFrom(t, from, to, typ, ix, body)`、`receive(t, d, env) rxResult`、`craft(t, sender, to, typ, ix, body, edit)`(可改任何内层字段)、`newStranger(t)`、`delegateBody(t, ctrl, ix, goal, capID)`、`chatBody(t, text, msgID)`、`counter(d, reason)`(按 `drop*`/`notice*` 常量读计数)。
- fake hub 观察:`queuedFor(t, srv, aid) [][]byte`、`onlyQueuedEnvelope`、`injectEnvelope`、`clearMailbox`、`grantOn/balanceOf`(`hubfake_test.go:930-1150`);fake 只按 `to_aid` 存不透明信封。
- 故障注入:`d.rxFault = func(typ string) error`(第 10 步事务内回滚,`receive_test.go:330`);时间:`d.clock`(daemon)与 `d.ix.SetClock`(store `state_at`)。
- 长调用:`newGateProvider()`(`tasks_test.go:34`,可控开始/结束/被取消);状态断言 `stateOf(t, d, ix)`、`lastStatusMeta(t, d, ix)`;等待 `waitUntil(t, why, cond)`。
- 纯存储测试:`interactions_test` 包内 `open(t)`(`state_test.go`)。

---

## 13. 按后续任务的动手清单

C3(付款同任务流)涉及本领域的部分:
1. `interactions`:`SetQuote`、`SetPayState`、`PayStates` 过滤、`result_meta` 列与 `Finish.Meta`、`Tx.SetLateResult`(§1.3、§3-4)。
2. 报价改为 StatusMsg,去掉报价回执;删除/改写 `PayAndRetry`/`DelegateAndPay`/`awaitQuote`(§4 陷阱 A)。
3. `ingestMessage` 付款分支:核对 → `SetPayState` → 再 `Admit`(公开能力)→ `runCapabilityCall`(§4 陷阱 B)。
4. `recoverInterrupted` 按 `pay_state` 分流;`pay_state=submitted` 行重启后恢复结算(§4 陷阱 C)。
5. `ingestStatus`/`ingestResult` 终态收据记录;`recordSettlement` 改读 `x402.payment.receipts`(§3 缺口 3)。
6. requester 侧 `SendPayment` 经 `queueSend`,`stateOnMessage` 已支持 `payment-submitted → working`、`payment-rejected → canceled`。

C5(a2ashape + /tasks/*):
1. 投影读 `result_meta`、`receipt_verified`(空 → unknown)、`transcript.Parse`;effect_status 无结果规则(§11)。
2. `/tasks/cancel|send|reply|wait` 直接包 `CancelTask`、`DelegateIn`/`DelegateCapabilityIn`/`SendMessageOpts`、`SendStatus`/`CompleteTask`、`Watch`。
3. 首条消息与 StatusMsg 的 messageId:要么在 ANetCore `DelegateReq`/`StatusMsg` 加 `MsgID`(新字段 `omitempty` + 新向量),要么在投影中定义确定性派生(如 `ix+":goal"`、`"st_"+mid`),两侧一致。
4. `DelegateReq.Metadata` 发送与落库(`a2a.serviceParameters`、客户端 metadata)。

D1(module/a2a):事件总线单调过滤修复(§7-1)后再做 SSE;注意慢订阅者被关闭需重订阅。

小修(可随任一任务):X2 deny/未知 ix(§8-1)、openai 后端 deny(§8-2)、`resendResult` 带原 meta(§5-2)、`ApprovePending` 追加后续消息时把带 `x402.payment.status` 的记为 `MsgPayment` 并逐条 publish(`pending.go:198-216`)、长调用"已提交未置 working"崩溃恢复(见风险)。

---

## 14. 风险与需要决定的点

1. **报价带回执**(§4 陷阱 A):不先改,C3 的同任务付款在 provider 执行、requester 收结果两端都会被"已有回执"判断挡住。
2. **长调用崩溃窗口**:`ingestDelegate` 提交后到 `runCapabilityCall` 置 working 之间崩溃,行停在 `submitted`;`recoverInterrupted` 只扫 working,`redeliveredDelegate` 的长调用分支静默返回(`delegation.go:950-954`),请求方永远收不到结果。改法:恢复时同时扫 `submitted` 且 `d.longCall(capID)` 的入站能力行(排除 `pay_state` 为 required/submitted),重投分支对"非运行、无回执、非终态"的长调用直接走 interrupted 结果。
3. **出站队列里封不上的行永不过期**:`OutboxItem.Exp` 在封装成功前为 0,`deliverQueued` 只在 `Exp != 0` 时判过期(`retry.go:100-121`)。public/public_cap 交互进入终态后 `peer_kel/peer_keys` 被清空(`interactions.go:574-577`),此后排队的重发(例如对重投委托的 `resendResult`)永远无法封装,按 24h 退避无限重试。改法:入队时记录 `created_at + messageLifetimeMS` 作为无信封行的截止时间;或 `resendResult` 对 public 交互用本次消息携带的 `m.noticeKeys` 直接 `sealWith`。
4. **requester 外发不入队**:`DelegateIn`、`SendMessageOpts`、`RequestEnd` 先写本地再 `relaySend`,发送失败时本地已是 `submitted/working` 或多了一条未送达消息,调用方重试会产生第二个 ix 或第二条消息。设计 §3.5 只要求结果/状态入队;但 A2A `SendMessage` 的返回语义需要 C5 决定:要么这三类也经 `queueSend`(复用同一信封字节),要么失败时回滚本地写。
5. **付款后执行绕过公开能力准入**(§4 陷阱 B):`Admit` 的槽在报价交付后已释放。
6. **SI-6 与 §4.3 的缺口**:无结果终态能力任务的 `effect_status` 没有规则(§11),需在设计里补一行,否则契约测试无法定义。
7. **requester `pay_state=completed` 时的取消**:代码会本地置 canceled(`delegation.go:633`),设计只对 `submitted` 规定"不变",C3 需定。
8. **事件乱序**(§7-1):不修会让 SSE 客户端看到状态回退。
9. **`anet.cancel_requested` 的来源**未定(§3 缺口 2):列还是推导,C5 前定。
