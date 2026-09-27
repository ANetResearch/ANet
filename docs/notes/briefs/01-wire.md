简报 · 线协/封装信封/中继v2/KEL/联邦/p2p · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §3.1–3.10、SI-3/SI-4/SI-10、X1/X3。代码:ANetCore `seal` `relayauth` `identity` `delegation`;ANet `internal/daemon/{seal_send,receive,peerkel,enckeys,transport,relay,retry,hub_client}.go`、`module/p2p`、`tools/anetpeer`、`module/transport.go`;ANetHub `internal/aghub/{relay,auth2,keys,limits,server}.go`、`internal/federation/{federation,keys}.go`。
行号均为本分支 HEAD(ANetCore b4d069f / ANet 6295665 / ANetHub fa98276)。

---

## 0. 结论

线协这一层基本做完,质量高:Core 原语、hub relay v2、daemon 收发流水线、peer_identity、p2p 帧版本化都有实现,也有对应单测(本次复跑 seal/relayauth/identity/delegation/golden、module/p2p、tools/anetpeer、interactions 与 daemon 线协相关用例,全绿)。

剩下的是**边缘缺口和跨仓不一致**,不是主干缺失:

1. **(diverged)** hub 已要求 `/agents/{aid}/balance|ledger|redemptions` 必须签名,但 daemon 的 x402 模块仍发无签名 GET,对真 hub 会得到 401;fake hub 不校验,所以测试发现不了。
2. **(missing)** `/fed/v2/cards` 未实现。进度记录 0013 说 B4 已完成"注册表 API、JWKS、联邦 v2 卡片",但代码里查不到。
3. **(partial)** `DelegateReq.Metadata` 在 daemon 收发两侧都没有用上;`ResultResp.Metadata` 只读 `anet.state`,不落库。
4. **(bug)** `public`/`public_cap` 交互在终态后收到重投的 delegate,无法重封结果;`resendResult` 可能重复入队。
5. **(bug)** daemon 关停时正在执行的短能力调用会被写成 FAILED 并发给请求方,这与 SI-10"取消属于暂时性失败"相违。
6. **(风险)** 暂时性失败(T)不 ack,而 hub 的 poll 按 id 取最老的 100 条且没有游标,两者叠加会造成信箱队头阻塞,任何陌生 AID 都能触发。

---

## 1. 完成度总表(done 的不展开)

| § | 要求 | 状态 | 证据 |
|---|---|---|---|
| 3.1 | EncKeySet/SignedEncKeySet、`VerifyEncKeySet(signed, expectAID, kel, now)`、三分支高水位、发布方 409/200 规则、密钥环 7/14/15 天、fsync 后发布、Restore 无环即新建、发送方选钥 | done | Core `seal/enckeys.go:231` `:286`,`seal/highwater.go:64`;daemon `enckeys.go:156`(maintain)、`:319`、`:335`;hub `aghub/keys.go:87`(PublishKeys)、`:282` |
| 3.2 | 套件 1 实现、2 保留 | done | `seal/suite.go` |
| 3.3 | 外层/内层、0–63 未知键拒收、≥64 入签名、签名原像由收到的通用映射计算(0/30/31/32)、Padmé、KEL 上限 256 事件/64 KiB | done | `seal/envelope.go:142` `:228` `:387` `:441`,`seal/fields.go:142`(sigPreimage)、`:233`(applyPad);金标 `seal/golden_test.go:125` |
| 3.4 | Core:`StatusMsg`、`ChatMsg.Metadata`(键 10)、`DelegateReq.ContextID/Metadata`(7/8)、`ResultResp.Metadata`(5)、`KindCancel`、`*WithKEL` 验证、VEC_* 向量 | done(Core) / **partial(daemon)** | `delegation/status.go`,`delegation/delegation.go:269` `:408`,`golden/a2a_wire_test.go:72-203` |
| 3.5 | 解析收件人公钥(存量优先、10 分钟复核、取不到明确报错)、inner 带 kel+keys、exp=ts+14d、传输列表 p2p 优先 hub 兜底、结果/状态持久化信封重试、过期写证据 | done | `seal_send.go:57/75/102/140/174`,`transport.go:80/103`,`retry.go:59/92/158`,`EvDeliveryExpired` |
| 3.6 | 第 0–10 步与 P/T 分类、未知 ix 10 分钟窗口 + TaskNotFound、ix 碰撞、同事务重放行、拒绝只进内存 LRU、崩溃遗留(短重跑/长不重跑)、启动恢复、嵌套对象用 inner.ts、Replay SupersededAt 修正 | done(边缘缺陷见 §4 G4/G5) | `receive.go:157`(总流程)、`:259/298/332/420/492/540/587/656`,`delegation.go:931`(redeliveredDelegate)、`capability.go:570`(recoverInterrupted),`identity/identity.go` markSuperseded |
| 3.7 | wire 2 头与 426、daemon 拒 hub<2、relayauth v2 头 + PreimageV2 + 重放缓存、`/hub/identity`、relay send/poll/ack、keys GET/POST、/register enc_keys + KEL 须延伸 + 删 guest、限额、relay_message 瘦身 + secure_delete + 迁移 | done | hub `server.go:226`(wireContract)、`:1091/1185/1217`,`auth2.go`,`limits.go:62`,`relay.go`;daemon `hub_client.go:222/341/387` |
| 3.7 | 余额/账本/兑付只对本人签名 GET,**同步改 daemon Balance/Reconcile 与 prodtest 9f** | **diverged** | 见 G1 |
| 3.7 | `/register` 带 `a2a_card` | **partial** | 见 G7 |
| 3.8 | `ExtendsKEL`;`peer_identity` 表;只在授权上下文写;pinned 行不淘汰;活动交互/outbox/pending 引用不淘汰;陌生人只进内存缓存;public 交互的 KEL/keys 存在交互行、终态清空;hub KEL 只接受延伸 | done | `identity/extend.go:39`;`peerkel.go:92/110/201/288/332`;`interactions/peers.go:244/308`;`interactions.go:571/633`;hub `server.go:648`、`card.go:630` |
| 3.9 | forward v2 删 from/kind/ix、`/fed/v2/keys/{aid}`、联邦卡片带 keys、FedReview 无内容字段 | done | `federation/federation.go`(ForwardVersion=2)、`federation/keys.go:76/138`、`aghub/reputation.go:56` |
| 3.9 | `/fed/v2/cards` | **missing** | 见 G2 |
| 3.10 | 帧只带 To/Envelope/ID、recv 帧按 ID 关联 ack、暂时性失败不 ack、V=2、旧帧回 error 且不交给 daemon | done | `module/p2p/transport.go:203`,`tools/anetpeer/main.go:285/306`;测试 `p2p_test.go:337/357`、`anetpeer/peer_test.go:218/256` |
| SI-3 | 只收封装信封,逐字段变异拒收 | done(单测) | `seal/envelope_test.go:77/188/277`,`receive_test.go:136/621` |
| SI-4 | 伪造 from 注入 | **partial**(只有 fake hub 单测,缺真 hub 联调) | `receive_test.go:586` |
| SI-10 | 暂时性失败不 ack、hub 与 p2p 并发只处理一次 | **partial**(单测齐;关停取消的缺陷见 G5;联调缺) | `receive_test.go:502/553/751/971` |
| X1 | 认证发送、不存发送方、按发送方限流、/register 按 IP 限流 | done | hub `server.go:1091`,`wire2_test.go:364/899/933` |
| X3 | EncKeySet 注册与取回、联邦按精确 AID 查询、每条消息带 kel+keys、预期 AID 绑定 | done | 同 3.1/3.5/3.9 |

---

## 2. 现成 API 与调用链(发送一条内层消息)

### 2.1 四层发送原语(`internal/daemon`)

| 函数 | 签名 | 用途 / 行为 |
|---|---|---|
| `sealWith` | `(toAID, typ, ix string, body []byte, set *seal.EncKeySet) ([]byte, error)` `seal_send.go:102` | 纯封装:SelectKey → 组 inner(本节点 KEL + `d.enc.SignedSet()`、新 mid、ts=nowMS、exp=ts+14d)→ `seal.Seal`。不联网、不写库。测试与通知用 |
| `sealEnvelope` | `(ctx, toAID, typ, ix string, body []byte) ([]byte, error)` `:75` | 解析收件人密钥后调用 sealWith。ix 为 inbound 且 trust ∈ {public, public_cap} 时**只用交互行上的 peer_kel/peer_keys**(`interactionKeySet` `:302`),其次用已有的 peer_identity 行,**绝不向 hub 取**;其余走 `recipientKeys`(`:140`,可能 GET `/agents/{aid}/keys` 并写入 peer_identity;outbound 交互时 pin 为 "outbound") |
| `relaySend` | `(ctx, toAID, typ, ix string, body []byte) error` `:57` | sealEnvelope + deliverEnvelope,**一次性、不持久化、不重试**。现用于 delegate(`relay.go:211`、`capability.go:226`)、text message(`delegation.go:371`)、end_request(`:474`) |
| `queueSend` + `deliverQueued` | `queueSend(ctx, toAID, typ, ix string, body []byte, write func(*interactions.Tx) error) (int64, error)` `retry.go:59`;`deliverQueued(ctx, id int64) error` `:92` | **重试队列**,见 2.3 |
| `sendNotice` / `sendNoticeStatus` | `seal_send.go:273`、`inbound.go:721` | 拒绝与 TaskNotFound 通知:限速、只发一次、不入队、只用陌生人缓存里的 key set |
| `deliverEnvelope` | `(ctx, toAID string, env []byte) error` `transport.go:103` | 按 `transports()` 顺序(额外传输在前,hubTransport 在最后),用同一份字节;Reachable=false 的跳过;全部失败时返回最后一个错误 |

`hubTransport.Send`(`transport.go:35`)的调用是:`hubSigned(POST /relay/send, ActionSend, {to_aid, envelope:b64})`,并据响应记录或清除收件方的 quiet 标记。

### 2.2 各 type 的现成高层函数

| 方向 | 内层 type | 现成函数 | 发送方式 | 同事务写入 |
|---|---|---|---|---|
| requester→provider | `anet.delegate/1` | `DelegateIn(ctx, provider, goal, atts, contextID)` `relay.go:150`;`DelegateCapabilityIn(...)` `capability.go:127`;内部走 `delegateCapabilityCtx` `:153` | relaySend | 无(先 `ix.Create`,再发;发送失败会留下孤儿 submitted 行) |
| 双向 | `anet.message/1` text | `SendMessageOpts(ctx, ix, body, atts, meta map[string]any)` `delegation.go:319` | relaySend | 无(先写 message 行与状态,再发) |
| requester→provider | `anet.message/1` end_request | `RequestEnd` `:437` | relaySend | — |
| 双向 | `anet.message/1` cancel,或 provider 的 `status{canceled}` | `CancelTask(ctx, ix)` `:615` | queueSend | 写 MsgCancel 行;requester 在 pay_state≠submitted 时置 canceled;provider 置 canceled |
| provider→requester | `anet.status/1` | `SendStatus(ctx, ix, state interactions.State, text string, meta map[string]any)` `:679` | queueSend | 写 MsgStatus 行,`SetState(state)`,带终态保护 |
| provider→requester | `anet.result/1` 文本任务 | `CompleteTask` `:536` | queueSend | `Finish(completed, transcript v2, receipt)` |
| provider→requester | `anet.result/1` 能力调用 | `deliverCapabilityResult(ctx, ix, capID, ix, capabilityResult, prov, resultOpts{state, reason, retryAfterMS})` `capability.go:456` | queueSend | `Finish` + 一条 MsgText(deliverable) |
| provider→requester | 重发已签结果 | `resendResult(ix, *Interaction)` `delegation.go:1466` | queueSend(每次重新封装) | 无 |

`SendMessageOpts` 的限制:body 与附件不能都为空(`:323`),`IsCapability` 的交互直接拒绝(`:333`)。**所以它不能用来发 x402 付款消息**,见 §3。

### 2.3 结果/状态重试队列(outbox)

- 表:`interactions/outbox.go`,字段 `OutboxItem{ID, IX, ToAID, Type, Body, Envelope, Exp, Attempts, NextAt, LastError, CreatedAt}`。
- 用法固定为两步:
  ```go
  id, err := d.queueSend(ctx, peer, seal.TypeStatus, ixID, payload, func(tx *interactions.Tx) error {
      // 只做 DB 写入:AddMessageRecord / SetState / Finish / 以后的 SetPay…
      return nil
  })
  if err != nil { return err }          // write 出错则不会入队
  _ = d.deliverQueued(ctx, id)          // 立即尝试一次;失败只打日志,由 outboxLoop 接手
  ```
- 语义:
  1. 封装在事务之外进行,可能联网取 key。若封装失败,本次只把 Body 入队,由循环稍后再封装。
  2. write 与 `EnqueueOutbox` 在同一个 SQLite 事务里提交。
  3. 每次重试都发同一份信封字节,即同一个 mid,接收方按 `(from, mid)` 去重。
  4. 退避从 5s 翻倍,最长 24h;启动时的第一轮无视退避,全部重发。
  5. `now > exp` 时删除该行,并写 `anet.delivery.expired`。
  6. 有 outbox 行引用的 peer_identity 行不会被淘汰。
- 约束:write 回调里**不要联网、不要签名**(`Store.Update` 在 store 锁下执行)。
- 已知不足:
  - 不区分永久错误:400、404、413 也会一直重试到 exp。
  - 忽略 hub 的 `Retry-After`。
  - 同一 `(ix, type)` 不去重,`resendResult` 可能与尚未送达的原行并存。

### 2.4 接收链

`hub poll(relay.go:383 pollOnce) / p2p(transport.go:182 inbound.Receive,先过第 0 步限速)` → `receiveEnvelope(receive.go:157)`:

1. `seal.Open`(第 1–4 步)。
2. `CheckTime`(第 5 步);然后查内存拒绝集 `d.refused`。
3. `resolveSenderKEL`(第 6 步,`:259`)。
4. `VerifyInnerSig`(第 7 步)。
5. `VerifyInnerKeys` + `DecideHighWater`(第 8 步,只作建议)。
6. `authorize`(第 9 步,`:298`,按 type 分到 authorizeDelegate/Message/Reply)。
7. `process`(第 10 步,`:540`):按 `(from, mid)` 加锁 → `ReplaySeen` → `ingestDelegate/Message/Result/Status`(`delegation.go:824/1098/1285/1394`)。各 ingest 用 `commitRx(m, fn)`(`receive.go:656`)把业务写入与 `ClaimReplay` 放进同一事务。
8. 成功后调用 `recordSenderIdentity`(`:587`)与 `noteLivePeer`。

`rxResult.ack()`:只有 T 类不 ack。

ingest 里做提交后副作用的规矩:先 `commitRx`,拿到 `rxAccepted` 以后才做 publish*、执行能力、发回复。回复一律走 queueSend,或走限速通知。

事件总线:`publishMessage(ix, seq, kind)`、`publishState(ix)`、`publishResult(ix)`(`eventbus.go:134-148`)。凡是改了状态的路径都要调用,D1 的 `Watch` 依赖它们。

---

## 3. C3(x402 同任务流)在线协层该怎么接

### 3.1 provider 发 `status{input-required, payment-required}`、`status{working, payment-verified}`、`status{failed/input-required, payment-failed}`

- 用 `SendStatus` 的机制,但**需要把 pay_* 列与状态放进同一事务**。现有 `SendStatus` 没有 write 钩子,而 `interactions.Tx` 也还没有 pay 列的 setter(`interactions.go:189-193` 只有字段和扫描)。
- 建议:
  1. 把 `SendStatus` 拆成 `sendStatusTx(ctx, ixID, state, text, meta, extra func(*interactions.Tx) error)`,公开的 `SendStatus` 直接调用它。
  2. 在 `interactions` 包新增 `Tx.SetPay(id, PayUpdate{State, Required, AuthIDs, Payload, Receipts, QuoteExpiresAt})`。
  3. meta 用 `map[string]any{"x402.payment.status": "...", "x402.payment.required": <PaymentRequired>}`。它会原样进入 `StatusMsg.Metadata`(JSON),并存进 message.metadata。
- `ValidStatusState` 不含 `completed`:`payment-completed` 只能跟 `anet.result/1` 一起发。
- `public_cap` 交互:sealEnvelope 只用交互行上的 key set。queueSend 在事务**之前**封装,所以即使同一事务把状态置为终态、清掉 peer_keys,这一条也能发出去。但如果首次封装失败而只存了 Body,**终态之后就再也封不上了**(G4 同一根因)。所以发终态 status 前应确认 `interactionKeySet` 可用。
- **陷阱 A**:现有的报价路径 `answerPaymentRequired`(`paylink.go:149`)走的是 `deliverCapabilityResult`,即 **result + 签名回执 + state=input-required**,仍是旧的"二次委派"模型(`PayAndRetry`,`pay_test.go:130` 按这个模型写死)。它带来三个问题:
  - `runCapabilityCall` 见到 `len(ix.Receipt) > 0` 就直接返回(`delegation.go:1031`),付款后无法在同一任务里执行;
  - `redeliveredDelegate` 会把报价当作"已答复"重发;
  - requester 的 `ingestResult` 会把它当作带回执的结果。

  C3 必须把报价改成 status,不签回执,并同步重写 `TestThePaidLoopClosesEndToEnd`。
- **陷阱 B**:`recoverInterrupted`(`capability.go:570`)会把所有 `state=working`、没有回执的 inbound 能力调用置为 failed/interrupted。C3 在收到 payment-submitted 后置 working(`stateOnMessage` 已经这样做),如果随后崩溃,待结算的行会被误判为中断,与 §8.3"重启恢复 submitted 行"冲突。需要排除 `pay_state=submitted` 的行,改走结算重试。

### 3.2 requester 发 `message{payment-submitted | payment-rejected}`

- 目前**没有可用函数**。`SendMessageOpts` 会拒绝能力调用和空 body。建议新增:
  ```go
  func (d *Daemon) sendPaymentMessage(ctx context.Context, ixID string, meta map[string]any,
      extra func(*interactions.Tx) error) error
  ```
  实现要点:
  1. `msgID := newMessageID()`;
  2. `cm := &delegation.ChatMsg{Kind: delegation.ChatText, MsgID: msgID, Metadata: json(meta)}`(Body 为空);
  3. 用 `queueSend(ctx, ix.PeerAID, seal.TypeMessage, ixID, payload, write)`,**不要用 relaySend**。这样付款消息会以同一份字节重试,provider 按 mid 去重;
  4. write 内:`tx.AddMessageRecord(MessageRecord{Kind: interactions.MsgPayment, MsgID, Metadata})`、`tx.SetState(ix, stateOnMessage(true, MsgPayment, meta, ix.PayState))`、`tx.SetPay(...)`;
  5. 提交后调用 `publishMessage/publishState` 与 `deliverQueued`。
- 接收侧已经就位:
  - `authorizeMessage` 在 `public_cap` 上只放行 `Kind==ChatText` 且 `x402.payment.status ∈ {payment-submitted, payment-rejected}` 的消息(`receive.go:481`);
  - `ingestMessage` 把带 `x402.payment.status` 的消息存为 `MsgPayment`,在能力交互上丢弃 body 和附件,并按 `stateOnMessage` 迁移状态(`delegation.go:1113-1133`)。
- provider 结算的钩子放在 `ingestMessage` 的 ChatText 分支 `res.class == rxAccepted && stored && kind == MsgPayment && fromRequester` 之后:
  - 读取 `x402.payment.payload`,做商户核对,调用结算;
  - 用 `goBackground` 或能力执行器异步执行,**不要**在 poll 循环里同步做网络结算;
  - 拒收重复付款([C25])的判断也放这里。
- 注意:`ingestMessage` 对终态交互上的 ChatText 会计数 `input-after-terminal` 并直接 ack(`:1106`)。付款消息如果晚于终态到达,按设计应给出 payment-failed 或记录证据,要在这之前单独分流。

### 3.3 provider 发 `result + {payment-completed, receipts}`

- 在 `resultOpts`(`capability.go:439`)增加 `meta map[string]any`,由 `deliverCapabilityResult` 合并进 `ResultResp.Metadata`。
- 把 `res.Paid`(deliverable 里的 `paid`)换成 `x402.payment.receipts`,hub 收据放在 `extensions["anet.settlement.receipt"]`。

### 3.4 requester 收 status/result 的钩子

- `ingestStatus`(`delegation.go:1394`)目前只做"存 message + SetState",而且**终态时直接 ack、不看内容**(`:1396`)。C3 需要:
  - 在同一个 `commitRx` 里解析 `x402.payment.required` 并写入 `pay_state=required`、`pay_required`、`quote_expires_at`;
  - 提交后异步跑支出策略(AdmitSpend),再调用 `sendPaymentMessage`;
  - 对终态交互上带 `x402.payment.receipts` 的 status,也要验证并记录证据(§4.2)。
- `ingestResult`(`:1285`)目前通过 `recordSettlement`(`:1426`)读取 deliverable 的 `paid`。要改为读 `ResultResp.Metadata["x402.payment.receipts"]`,并落库 `pay_receipts`,因为 Metadata 目前不持久化(G3)。

### 3.5 hub 结算请求

`module/x402/pay.go:215` 发往 `/x402/settle` 的请求体只有 `{x402Version, paymentPayload}`。真 hub 要求必须带 `paymentRequirements`(`ANetHub facilitator.go:1133`,缺了返回 `invalid_requirements`)。fake hub(`hubfake_test.go:1022`)不校验这一项,所以 daemon 测试是绿的。C3 必须补上,并让 fake 同样拒绝(见 G6)。

---

## 4. 缺口清单(可直接动手)

**G1 · §3.7 余额/账本/兑付签名 GET(diverged)**

- 现状:
  - hub:`facilitator.go:1167` hBalance、`server.go:1453` hLedger、`redemption.go:480` hRedemptions 都调用 `authSelf`;
  - daemon:`module/x402/pay.go:277` Balance 用裸 `http.Get` 取 `/balance` 与 `/ledger`;`audit.go:385` getJSON 被 `reconcile.go:70/85` 用来做无签名 GET;
  - 结果:对真 hub 得到 401。fake 的 `hubfake_test.go:1113/1124` 不验签。
- 改法:
  1. 模块拿不到 `hubSigned`。建议在 `module.PaymentSeam`(`module/module.go:171`)加一个方法,例如 `HubRequest(ctx, method, path, action string, body, out any) error`,由 daemon 用 `hubSigned`(`hub_client.go:222`)实现,并按"扩接口须写理由"的约定写接口注释。
  2. 不建议模块自己用 `relayauth.PreimageV2` 签名:daemon 的 `lastSignTS` 保证同一毫秒内的时间戳严格递增,而 Ed25519 是确定性签名,两个完全相同的请求会被 hub 的重放缓存拒掉。
  3. fake 的 balance/ledger 改为调用 `h.verifyAuth(w, r, body, relayauth.ActionBalance/ActionLedger, nil)`。
  4. `scripts/prodtest.sh:826-850`(9f)目前用 `viafmax` 做无签名查询,按设计改走 dmax 控制面。可能需要在控制面的 `POST /balance` 输出里带上兑付列表,或新增一条 bearer-only 路由。

**G2 · §3.9/§10.6 `/fed/v2/cards`(missing)**

- `federation.go:346` 只有 `/fed/v1/cards`(条目已带 `keys`)。代码里查不到 `a2a-card/1`、`/a2a/v1/agents`、`jwks.json`。`aghub/a2acard.go` 只把卡片原样存为 `unverified`。
- 与 0013 的"B4 已完成注册表 API、JWKS、联邦 v2 卡片"不符,应由 C5 或 B4 补做。
- 条目格式:`{format:"a2a-card/1", card, kel, keys, home, fed_seq}`。准入要复用 Core `a2acard` 与 KEL 延伸规则,v1 保留。

**G3 · §3.4 daemon 侧 metadata 通路(partial)**

- `DelegateReq.Metadata` 发送侧从不设置(`relay.go:205`、`capability.go:~283`);接收侧 `ingestDelegate`(`delegation.go:824`)不保存 `m.dr.Metadata`。
- `ResultResp.Metadata` 只在 `resultState`(`:1262`)里读 `anet.state`,不落库。
- `a2a.serviceParameters` 没有任何实现。
- 改法:
  - delegate:把 Metadata 写进首条 message 的 `metadata`。`public_cap` 不存 Intent,可以只保留 `x402.*` 与 `a2a.serviceParameters`。
  - result:新增列,例如 `result_meta`,或并入 `pay_receipts`。
  - D1 负责把 `A2A-Extensions`/`A2A-Version` 写入 `a2a.serviceParameters`。

**G4 · §3.6 第 10 步 × §3.8:终态 public 交互重投后无法重封(bug)**

- 过程:
  1. `setState`/`finish` 在终态时清空 `peer_kel/peer_keys`(`interactions.go:571-575, 633-635`);
  2. 之后收到 `public_cap` 或 `public` 的重投 delegate,走 `process` → `redeliveredDelegate` → `resendResult`(`delegation.go:1466`)→ `queueSend` → `sealEnvelope`;
  3. 取不到 key,只能存 Body,重试 14 天后丢弃。
- 另有 C33 的问题:原 outbox 行还没送达时,重投又会再入队一份新信封(新 mid)。
- 改法:
  - `resendResult` 接收 `m *rxMsg`,用 `m.noticeKeys.set` 直接调用 `sealWith`。`noticeKeys` 在第 8 步已从本信封附带的 keys 验证过。
  - 入队前先查 `d.ix.Outbox(ix)`,已有 `TypeResult` 行就只调用 `kickOutbox()`。
  - 补测试:`public_cap` 调用完成后清空请求方信箱,重投 delegate,断言结果恰好再送达一次。

**G5 · SI-10"取消"类暂时性失败(bug)**

- 短能力调用在 poll 路径里同步执行,ctx 派生自 `d.ctx`(`delegation.go:1047`)。daemon 关停时 `Invoke` 返回错误,`tryCapabilityPaid` 写入 `"FAILED"`(`capability.go:398`),接着 `Finish(failed)`、入队。请求方因此收到一个确定性的失败。
- 改法:
  - 在 `tryCapabilityPaid` 与 `deliverCapabilityResult` 开头判断 `d.ctx.Err() != nil`,成立就不写结果、不入队。
  - 这样交互保持非终态。关停时 poll 的 ack 在整批处理后才发,所以信封未被 ack,重启后按"崩溃遗留"规则重跑。长调用由 `recoverInterrupted` 按设计置为 interrupted。
  - 补测试:注入一个能感知 ctx 的 provider,在执行中调用 `d.Close()`,断言没有 result 行、outbox 为空。

**G6 · §17 fake 完整性(partial)**

fake hub 与真 hub 有两处不一致:

- `/x402/settle` 不要求 `paymentRequirements`(`hubfake_test.go:1022`);
- `/balance`、`/ledger` 不验签。

这两处正好让 G1 和 §3.5 的跨仓不一致在单测里显示为绿。按真 hub 的拒绝路径补齐,并在 `hubapi/wirecontract` 钉住请求体字段。

**G7 · §3.7 `/register` 的 `a2a_card`(partial)**

daemon 的 `hubapi.RegisterRequest`(`internal/hubapi/hubapi.go:200`)没有 `A2ACard` 字段;hub 能存但不验证。归 C5(0012 的"默认值剥离"缺陷一并处理)。

**G8 · SI-4/SI-10 联调(partial)**

`scripts/joint.sh` 还没有以下步骤:

- 往真 hub 注入伪造 from 的信封;
- 同一信封经 p2p 与 hub 并发投递;
- SI-1 canary。

目前只有 fake hub 单测(`receive_test.go:553/586`)。阶段 E/F 需要补上。注入可以直接用 relay v2 签名以攻击者身份 POST `/relay/send`,载荷内层 `from` 冒充允许名单中的对端;断言计数 `bad-sig` 或 `from-mismatch`,且不落库。

---

## 5. 测试夹具怎么用(`internal/daemon/*_test.go`)

- **两节点加 fake hub**:`registeredPair(t)` 返回 `(srv, req, prov)`,两者都已注册,prov 通过 `joinTestGroup` 把 req 加入 allow(`sealtest_helpers_test.go:176`)。自定义组合时用 `newFakeHub(t)` 加 `newTestDaemon(t, srv.URL, accept)`(`relay_test.go:32`,已停掉后台 poll,由测试显式调用 `d.pollOnce(ctx)`)。
- **直接驱动接收流水线**:
  - 按真实发送路径产生信封:`sealFrom(t, from, to, typ, ix, body)`;
  - 送进流水线:`receive(t, d, env)`,返回 `rxResult{class, reason}`;
  - 读原因计数:`counter(d, reason)`;
  - 可用的常量在 `receive.go:70-110`,例如 `dropDuplicate`、`transientUnknownIX`。
- **构造恶意发送方**:
  - `newStranger(t)` 得到带自签 key set 的新身份;`senderOf(d)` 从已有 daemon 取发送方;
  - `craft(t, s, to, typ, ix, body, func(in *seal.SealedInner){...})` 可在签名前改任意 inner 字段;
  - `retiredSigner(c)` 在 Rotate 前截获旧签名密钥;`signedKeysWith(t, aid, sign, seq)` 签一个指定 seq 的 key set。
- **构造正文**:`delegateBody(t, ctrl, ix, goal, capID)`(capID 非空即为能力调用);`chatBody(t, text, msgID)`。StatusMsg、ResultResp 直接 `(&delegation.StatusMsg{...}).Marshal()`。
- **操作 fake hub 信箱**:`onlyQueuedEnvelope(t, srv, aid)`、`queuedFor`、`injectEnvelope`、`clearMailbox`;`backdateLastSeen`;付款相关有 `grantOn(url, aid, n)`、`balanceOf`、`hubAIDOf`、`hubKELOf`。
- **故障与时钟**:
  - `d.rxFault = func(typ string) error {...}`:在 commitRx 事务内注入存储失败,得到 T;
  - `d.clock = func() uint64 {...}`:移动 seal 线协时钟(hubSigned 用墙钟,不受影响);
  - `d.ix.SetPeerIdentityCap(n)`:测试淘汰。
- **能力 provider**:`lampProvider`(`capability_test.go:25`,记录 `invoked`);`pricedProvider`(`pay_test.go:45`,`//go:build !no_x402`,导入 x402 模块即自动有 payer;`withoutPayments(d)` 模拟 no_x402 构建)。
- **p2p**:`module/p2p/p2p_test.go` 的 fake peer 按帧 ID 关联;`tools/anetpeer/peer_test.go` 在同一进程里起两个 anetpeer 加真 module/p2p,覆盖 TCP 与 hub rendezvous。
- **hub 侧**:`ANetHub/internal/aghub/wire2_helpers_test.go` 与 `wire2_test.go`(426、v2 签名、限额、`relay_message` 不存发送方、迁移);`federation/wire2_test.go`、`keys_refusals_test.go`。
- 运行:先 `source /data/projs/anet-dev/.anet-env.sh`。daemon 线协相关用例可以用 `go test ./internal/daemon/ -run 'Receive|Seal|Hub|Relay|Key|KEL|Transport|P2P|Redeliver|Forged|Plaintext|Rotation'` 挑出来跑,约 20 秒。

---

## 6. 风险与设计/代码矛盾

1. **信箱队头阻塞(设计缺口)**
   - 成因:§3.6 规定未知 ix 的 message 在 10 分钟内算 T、不 ack;而 hub `RelayPoll` 按 `ORDER BY id LIMIT ?` 取最老的消息(`aghub/relay.go:137`),daemon 每次取 100 条(`relay.go:289`),没有游标。
   - 触发:任何注册过的陌生 AID(注册免费)发来 ≥100 条指向随机 ix 的已签 message,受害节点就会在 10 分钟内收不到更新的消息。每发送方 20/s、突发 200 的限流挡不住;同样,超过 48 MiB 的 poll 预算也会引发。
   - 建议:`/relay/poll` 增加 `after_id`,daemon 在一轮内跳过本轮已判为 T 的 id;或者对陌生人(peer_identity 无记录、不在 allow/trust 名单)的未知 ix 消息直接判 P。
2. **0013 的完成度与代码不符**:B4 的"注册表/JWKS/联邦 v2 卡片"查不到(G2)。后续实现者不要以进度记录为准。
3. **跨仓线协不一致被 fake 掩盖**:settle 缺 `paymentRequirements`、余额类接口无签名(G1、G6)。wire-2 首次联调时一定会失败。
4. **delegate/message 一次性发送**:`relaySend` 失败时,message 行与状态迁移已经写入(`SendMessageOpts`),delegate 留下 submitted 孤儿行(`DelegateIn`),且都不重试。设计只要求结果和状态进重试队列,但 D1 的 SendMessage 语义(客户端看到的是已提交任务)和 C3 的付款消息都需要可靠投递,建议统一改走 queueSend。
5. **p2p 暂时性拒绝的延迟**:daemon 不 ack 时,anetpeer 要等 `handOffTimeout=10s`(`anetpeer/main.go:341`)才回错。发送方每条消息多等 10 秒才转走 hub。符合设计"不回 ack",但可以考虑增加显式 nack 帧,例如 `{op:"nack", id}`。
6. **outbox 不区分永久错误**:hub 返回 400/404/413 的信封会重试 14 天;429 的 `Retry-After` 被忽略。
7. **C3 的两个隐藏陷阱**(见 §3.1):
   - 报价走 result + 回执,会阻断同任务内执行;
   - `recoverInterrupted` 会把待结算的交互误判为中断。
8. **首次信任只对有持久记录的对端有回退保护**:陌生人与 public 交互的对端每条消息都重新首次信任。这是设计 §21 第 6 条已承认的局限,代码与之一致。对外文档必须写明。
9. **KEL 轮换无产品路径**:第 7 步的宽限规则只由单测覆盖(`receive_test.go:702`)。轮换投入使用前,还需要补"轮换后重签未完成的外发",因为 outbox 里的旧签名信封过了宽限期会被拒收。
