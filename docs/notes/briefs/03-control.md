简报 · 控制面/审计/证据 · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §7(控制面加固)、§14(审计)、SI-7、§5.3 的 CLI 部分。代码:`internal/daemon` 的
`control_api.go` `ctlsec.go` `session.go` `console.go` `web/console.html` `attachments.go` `ledger.go`
`inbound.go` `autoreply.go`;`cmd/anet` 的 `main.go` `peers.go` `verify.go`。
行号以本分支 HEAD(ANet `6295665`)为准。

---

## 0. 结论速览

| 条目 | 判定 | 说明 |
|---|---|---|
| §7.1 Host 白名单 421、删除 `control_allow_remote`、非回环 `control_addr` 拒绝启动 | done | `ctlsec.go:183-221`、`:339-349`;`control_api.go:252`;全仓已无 `control_allow_remote` |
| §7.2 控制台票据 / 会话 cookie / CSRF 只在内存 | done | `session.go` 全文 |
| §7.3 会话路由白名单 + 遍历测试 | done | `ctlsec.go:66-83`;`ctlsec_test.go:193`(已做 4 处 mutation 验证,见 §9) |
| §7.4 `POST /console/switch` | done | `session.go:274-343`,测试 `ctlsec_test.go:494` |
| §7.5 console.html 改造 + CSP | done | 见 §6;浏览器加载测试在无 Chromium 时 skip(风险) |
| §7.6 `/attachment` 嗅探、四类图片内联、CSP sandbox、nosniff、去 immutable、入库改写 Mime | done | `control_api.go:782-821`,`attachments.go:136-175` |
| §7.7 `/pull` | done | `attachments.go:201-404`,测试 `console_test.go:240-391` |
| §7.8 `/ping` 去 ACAO、运行时目录校验、`anet mcp` 严格解析 | done | `console.go:99`,`paths.go:48-146`,`cmd/anet/mcp.go:37-40` |
| §7.9 策略类写入写 `anet.policy.changed` | partial | 入站策略/名单/待批/public_capabilities/auto_reply 已写;支出上限(C3 未开始)尚无写入点 |
| §5.3 CLI:`peers allow/trust`、`inbound approve`、放宽 policy 需 `/dev/tty`;会话不能批准 | done | `cmd/anet/peers.go:43-144`,测试 `peers_test.go:63` |
| SI-7 控制面一半 | done | Host/凭据/无令牌 HTML/白名单遍历均有测试 |
| SI-7 本机 A2A 接口一半(421、只收 A2A 令牌、交叉凭据拒绝) | missing | D1 未开始;`hostGuard` 是 `*Daemon` 方法,模块无法复用 |
| §14 新事件:`delegation.received` `refused_summary` `policy.changed` `autoreply.invoked` | done | 见 §4 表 |
| §14 新事件:`message.sent/received` | missing | 无依赖,现在就能做 |
| §14 新事件:`payment.quoted` | missing | 随 C3 |
| §14 新事件:`backend.forwarded` | missing | 随 D1 §11.6 |
| §14 `anet audit [--since|--peer|--interaction|--json]`、`--export DIR`、`anet audit hub` 别名、显示规则 | missing | CLI 只有 `anet evidence`、`anet audit-hub` |
| §14 `anet verify --chain DIR` | missing | `verify.go` 只验回执与见证 |
| §12 控制面 `/tasks/*` 路由(接缝归本领域说明,实现归 C5) | missing | 无任何 `/tasks/` 控制面路由;事件总线 `eventbus.go` 已就绪 |

---

## 1. 控制面结构与路由注册模式

请求经过三层(`ctlsec.go:3-22` 注释写得很清楚):

1. `hostGuard`(`ctlsec.go:202-221`):对所有响应先设 `nosniff`、`apiCSP`(`:40`)、`no-referrer`;Host 不是
   `127.0.0.1|localhost|[::1]` + 本监听端口 → 421;带 Origin 且不是这三个回环源 → 403。端口取自
   `http.LocalAddrContextKey`(`listenerPort`,`:160`),放进 context 供 `requestPort(r)` 使用。
2. `top` 路由表(`secureControlPlane`,`ctlsec.go:128-146`):只有 `publicRoutes`(`:88-93`)四个公开路由
   (`GET /console` `GET /ping` `GET /{$}` `POST /console/session`),其余 `"/"` 全部进 `authGate`。
3. `authGate`(`ctlsec.go:245-299`):
   - 有 `Authorization` 头 → 常数时间比较 `"Bearer "+token`,不符 401;符合则 `limitBody`(JSON 1 GiB,
     multipart 130 MiB,`control_api.go:191/195`)后直接进 `api`。
   - 否则找会话 cookie `anet_s_<port>`;无 → 401。
   - 有会话:`api.Handler(r)` 得到匹配的 **pattern 字符串**,查 `sessionRoutes[pattern]`;不在表里 → 403
     `{"error":…, "refused":"session"}`(`sessionRefusal`,`:239`)。然后 `Sec-Fetch-Site` 只允许
     `same-origin`/`none`;除 `noCSRF` 路由外要求 Origin 与 `X-Anet-CSRF`;multipart 只在 `multipart:true`
     的路由收;JSON 体若规则有 `jsonFields` 则只允许这些键(`restrictJSONFields`,`:304`)。

路由注册:**所有已认证路由都注册在 `api *routeMux` 上**。

- 内核路由:`(*Daemon).ControlHandler(token)`(`control_api.go:130-175`),`api.HandleFunc("POST /xxx", d.hXxx)`。
- 控制台相关三条在 `secureControlPlane` 里补:`GET /attachment`、`POST /console/ticket`、`POST /console/switch`
  (`ctlsec.go:130-132`)。
- `routeMux`(`ctlsec.go:98-115`)记录每个注册的 pattern,测试据此遍历,**任何文件新增的路由自动纳入白名单测试**。
- `ControlHandler` 返回 `*controlPlane`(测试里 `d.ControlHandler(token).(*controlPlane)` 取 `cp.api.patterns`)。
- handler 惯例:`readJSON(r,&req)`(`control_api.go:340`,不拒未知字段)、`writeJSON(w, code, v)`(`:334`)、
  错误体 `{"error": "..."}`;调 hub 时 `context.WithTimeout(r.Context(), hubCallTimeout)`(30 s)。
- CLI 客户端 `client.do/fetch`(`cmd/anet/main.go` 末尾)**只发 POST + JSON**,所以新路由一律用 `POST` pattern。

## 2. 新增 `/tasks/*` 路由的做法(给 C5)

1. 新文件 `internal/daemon/tasks_api.go` 放 handler;在 `ControlHandler`(`control_api.go:173` 之前)注册:
   `POST /tasks/send` `/tasks/get` `/tasks/list` `/tasks/cancel` `/tasks/wait` `/tasks/pay` `/tasks/pay-manual` `/tasks/reply`。
2. **不要**加进 `sessionRoutes`。§7.3 白名单不含 `/tasks/*`;`TestEveryRouteIsAllowlistedOrRefusedForASession`
   (`ctlsec_test.go:193`)会自动断言它们对会话拒绝。建议把 `POST /tasks/pay`、`POST /tasks/pay-manual`、
   `POST /tasks/send`、`POST /tasks/reply` 追加到该测试末尾的显式 bearer-only 列表(`ctlsec_test.go:223-225`),
   把意图钉死(防止以后有人为控制台迁移而顺手放开付款路由)。
3. 如果将来控制台改用 `/tasks/send`,必须给它写 `sessionRule{jsonFields: …}`,且永远不放 `pay`/`decision`/
   附件路径字段;付款类路由永不进会话白名单(§8.6"控制台会话不能授权付款")。
4. 陷阱:白名单测试会**以会话身份真实调用**每个白名单路由(body `"{}"`)。若把阻塞型路由(`/tasks/wait`)放进
   白名单,测试会挂住;要么不放,要么 handler 对空请求立即 400。
5. `/tasks/wait` 是长轮询:`http.Server` 只设了 `ReadHeaderTimeout`(`control_api.go:315`),无写超时,可用;
   CLI 侧要像 `delegate/message` 那样放宽 `c.timeout`(`cmd/anet/main.go:1199`),MCP 侧同理。
   用 `d.Watch(ix)`(`eventbus.go:115`)得到"快照+订阅"原子对,监听 `r.Context().Done()` 退出并调用 cancel。
6. 返回体用 `internal/a2ashape` 投影;`internal/daemon` 不得导入 `a2aproject`(SI-8)。SI-6 的"控制面输出反序列化为
   a2a-go `a2a.Task`"契约测试放在允许导入 a2a-go 的包(`module/a2a` 的测试或单独测试包),不要放 `internal/daemon`。
7. bearer 路径 body 上限 1 GiB;`/tasks/send` 若接收 base64 FilePart,在 handler 里再套一层较小的
   `http.MaxBytesReader`(附件单个 64 MiB,`maxAttachmentBytes`)。
8. 需要"对会话拒绝但对 bearer 可用"的断言样例:`TestASessionCannotReachABearerOnlyHandler`(`ctlsec_test.go:234`)。

## 3. 会话/凭据机制要点(改动时别破坏)

- 票据:`POST /console/ticket`(bearer)→ 60 s 单次票据,URL `http://<host>:<port>/console#t=<ticket>`
  (`session.go:204-217`)。页面 `startSession()` 读片段、`history.replaceState` 清除、`POST /console/session`
  换会话(`console.html:629-646`);服务端要求 Origin + `application/json`(`session.go:223-261`)。
- 票据与会话 id 以 SHA-256 摘要存 map(`session.go:51-69`),上限各 32,会话 TTL 12 h,daemon 重启全部失效。
- CSRF 只在会话创建响应里出现一次;`TestCSRFIsNotRetrievableWithTheCookie`(`ctlsec_test.go:376`)防回退。
- `GET /attachment` 是唯一 `noCSRF` 会话路由(`<img>`/`<a>` 无法带头),只读。
- 会话 JSON 字段限制只对设了 `jsonFields` 的路由生效;只读路由(`/status` `/threads` `/evidence` …)规则为空,
  **接受任意 JSON**。以后若给只读路由加有副作用的参数,要同时给它加 `jsonFields`。

## 4. 证据链:写入 API、已有事件

写入 API:
- 内核:`d.ledger.Append(eventType string, payload any) (id string, err error)`(`ledger.go:166`)。每次 Append 签名、
  入内存 `ael.Ledger` 校验、写一行 base64 CoreDet-CBOR、**fsync**。惯例:常量 `const EvXxx = "anet.xxx"` 放在写入
  文件旁,失败只 `log.Printf`,不影响业务(`d.ledger` 在 `New` 里总会打开,`nil` 判断是防御性的)。
- 模块:`module.Host.RecordEvidence(eventType, payload)`(`modules.go:193`)。
- payload 用 `map[string]any`,值限 string/整数/bool/[]string/map;避免 struct(键名随编码器)与 `[]byte`
  (`/evidence` 输出里会变成 base64 字符串,`ledger.go:392-396`)。
- 读取:`d.ledger.Evidence(EvidenceQuery{EventType, Since(seq), Limit})`(`ledger.go:316`),最多 1000 条、取最新尾部;
  控制面 `POST /evidence`(`control_api.go:980`),CLI `anet evidence --type --since(seq) --limit`(`main.go:1475`),
  MCP `evidence_read`(`mcpserv.go:153`)。

已有事件(本机链):

| 事件 | 写入点 | payload 键 |
|---|---|---|
| `anet.capability.effect` | `capability.go:553`;`module/x402/voucher.go:334` | interaction_id, capability, caller_aid, status(OK/UNVERIFIED/FAILED/…), verifiable, metrics, result_cid, state, [evidence] |
| `anet.interaction.receipt` | `delegation.go:594` | interaction_id, requester_aid, result_cid |
| `anet.delegation.sent` | `relay.go:216`;`capability.go:243` | interaction_id, provider_aid, request_cid, [capability] |
| `anet.result.accepted` | `delegation.go:1382` | interaction_id, result_cid, receipt_bytes, receipt_verified(**bool**), state, [after_terminal] |
| `anet.delegation.received` | 逐条 `delegation.go:803`;聚合 `inbound.go:665` | 逐条:interaction_id, requester_aid, trust, request_cid, [capability];聚合:aggregated, window_start/end, count, by_trust, first_aids |
| `anet.delegation.refused_summary` | `inbound.go:657` | window_start/end, count, by_reason, first_aids(≤10) |
| `anet.policy.changed` | `recordPolicyChange`(`inbound.go:824`) | field, from, to,[canceled/detected/requester_aid];field 取值:`inbound.policy` `inbound.public_capabilities` `peers.allow|deny|trust` `inbound.pending` `auto_reply` |
| `anet.autoreply.invoked` | `autoreply.go:608-618` | agent, interaction_id, peer_aid, trusted, sandboxed, exit, duration_ms |
| `anet.delivery.expired` | `retry.go:116` | interaction_id, type, to_aid, attempts, last_error |
| `anet.payment.settled` | provider `capability.go:385`;requester `delegation.go:1458` | interaction_id, capability/transaction/amount/network;requester 侧另有 verified, payee, auth_id, receipt |
| `anet.payment.authorized` | `module/x402/pay.go:186` | interaction_id, authorization_id, pay_to, amount, network(§8.6 要加 pay_bind、purpose) |
| `anet.credit.redeemed`、`anet.issuance.head_seen`、`anet.voucher.redeemed|refused` | `module/x402/*` | — |
| `anet.shell.command|refused` | `module/shell/provider.go` | — |
| `anet.evidence.gap` | `ledger.go:139` | reason, decode_error, lost_bytes |

`anet.credit.issued|retired` 是 hub 发放链上的事件(`module/x402/audit.go:405`),不在本机链。
`EvCapabilityEffect`、`EvPaymentSettled` 在 daemon 与 `module/x402` 各定义一份,改名时两处同改。

## 5. §14 缺项的实施要点

### 5.1 `anet.message.sent/received`(现在就能做)

- 发送:`SendMessageOpts`(`delegation.go:319`)在 `d.relaySend(...)` 返回 nil 之后写。payload 建议
  `{interaction_id, msg_id, kind, cid, bytes, attachments}`,`cid = anetcid.Sum(payload)`(`payload` 即 `cm.Marshal()`
  的 CoreDet 字节,含随机 `MsgID`,所以短消息的 CID 不可被穷举)。不要对正文单独算 CID。
- 接收:`ingestMessage`(`delegation.go:1098`)`ChatText` 分支里 `res.class == rxAccepted && stored` 之后写,
  `cid = anetcid.Sum(m.body)`、`bytes = len(m.body)`(`m.body` 即对端 payload 原字节,两侧 CID 相同,可对账)。
  **只在 `stored` 为真时写**,重投不重复记。
- `trust=public`/`public_cap` 交互:仿 `noteReceivedPublic`(`inbound.go:622`)做窗口聚合,否则 `open` 策略下陌生人
  每条消息都会触发一次签名 + fsync。
- 测试:`registeredPair(t)`(`sealtest_helpers_test.go:176`)+ `chainEvents(t, d, typ)`(`capability_test.go:695`);
  断言两侧 CID 相等、payload 不含正文;mutation:把 CID 改成正文 CID 应被"不含正文/CID 相等"断言抓到。

### 5.2 `anet.payment.quoted`(C3)、`anet.backend.forwarded`(D1)

- quoted:C3 把 `capability.go:380` 附近的 `PAYMENT_REQUIRED` 结果改为 `status{input-required, payment-required}`
  时,在发出报价的同一处写;建议 `{interaction_id, capability, amount, asset, network, pay_to, expires_at}`,
  不写 `pay_bind` 原像。
- forwarded:`module/a2a` 经 `Host.RecordEvidence`,payload 按 §11.6 `{backend, interaction_id, peer_aid, trusted}`。
- §7.9:C3 新增的支出上限写入(TTY 保护的路由)必须调用 `d.recordPolicyChange("payments.<key>", from, to, nil)`。
- §8.6 启动时"按 purpose 从 `anet.payment.authorized` 重建日累计,覆盖 24 小时":`Evidence()` 只给最新 1000 条,
  需要在 `ledger.go` 加一个持锁遍历接口,例如 `func (l *evidenceLedger) Scan(sinceMS int64, fn func(*ael.EventRecord) bool)`,
  直接走 `l.led.Events(l.did)`。

### 5.3 `anet audit`

现状:`cmd/anet/main.go` 没有 `audit` 分支(会落到 `unknown command`);`audit-hub` 在 `:1436`;`evidence` 在 `:1475`。

建议实现(daemon 与 CLI、MCP 共用一个路由):
1. daemon 新增 `POST /audit`(bearer-only,不进会话白名单)。扩展 `EvidenceQuery`(`ledger.go:284`,`readJSON` 不拒未知
   字段,向后兼容)增加 `InteractionID`、`PeerAID`、`SinceMS`、`UntilMS`。过滤:
   - interaction:payload `interaction_id`。
   - peer:payload 里 AID 键不统一(`provider_aid` `requester_aid` `caller_aid` `peer_aid` `to_aid` `payee` `first_aids`,
     `peers.*` 的 `from/to`),而 `anet.result.accepted` 根本没有对端键。做法:先 `d.ix.ListAll(interactions.ListFilter{PeerAID: aid})`
     得到该对端的 ix 集合,再按 `interaction_id ∈ 集合` 或 payload AID 键命中过滤。
   - 分页:`Evidence()` 取尾部、最多 1000;`/audit` 应按 seq 顺序迭代全链或返回 `next_since`。
2. 显示规则(§14):`anet.result.accepted.receipt_verified=false` 与 `anet.payment.settled.verified=false` 显示"未能核验";
   `anet.capability.effect.status` 只有 `OK` 计入成功,`UNVERIFIED` 单列;未知事件类型原样打印 type + payload JSON;
   §8.3 要求"同一 ix 第二张成功收据在 audit 标出"——同一 interaction_id 出现 ≥2 条 `verified=true` 的
   `anet.payment.settled` 时标红。
3. 每段标明来源:`本机证据链 · did:anet:<aid> · head <id> len N state ACTIVE|QUARANTINED`;若同时附 hub 数据,
   单列 `hub 发放链 · <hub url>`(`/audit-hub`)与 `hub 账本对账`(`/reconcile`)。QUARANTINED 必须醒目。
4. `--json`:直接输出 `/audit` 的结构;MCP `audit` 工具(§12,取代 `evidence_read`)原样转发同一结构。
5. `anet audit hub` = `c.do("/audit-hub", {})`。在 `runClient` 的 `switch cmd` 加 `case "audit":`,`pos[0]=="hub"` 时走别名。
6. `--since` 语义设计未定:`anet evidence --since` 是 seq,而其帮助文字写 `--since TS`(`main.go` usageAllText)。
   建议 audit 的 `--since` 接受时长(`24h`)或 RFC3339,另留 `--from-seq`;在帮助里写清。
7. CLI 约束(测试会检查):`knownFlags["audit"] = {"since","peer","interaction","json","export"}`(`main.go:1062`);
   `usageAllText()`(`main.go:292`)必须出现 `anet audit ` 行(`TestEveryDispatchedCommandIsInTheFullHelp`);帮助里写的
   每个 `--flag` 必须被 `checkFlags` 接受(`knownflags_test.go:90-120`);`main.go` 里读取的 `flags["x"]` 必须在表里
   (`TestEveryDocumentedFlagIsAcceptedBySomeCommand`,只扫 `main.go`)。

### 5.4 `anet audit --export DIR` 与 `anet verify --chain DIR`

- 导出不能用 `/evidence` 的 JSON:`plainMap` 已把 CBOR 字节串转成 base64 字符串,记录 id 无法再推导。需要新 bearer
  路由(如 `POST /evidence/export`),在 ledger 锁内对 `l.led.Events(l.did)` 逐条 `encodeRecord(rec)`(`ledger.go:226`)
  返回 base64 行,附 `identity.MarshalKEL(self.KEL())` 与 head。CLI 写出:`DIR/chain.ael`(每行一条)、`DIR/kel.b64`、
  `DIR/manifest.json`(chain_did、head_id、length、state、导出时间、anet 版本)。不要让 CLI 直接读
  `evidence.ael.jsonl`(daemon 正在追加,可能读到撕裂尾行)。
- `verify --chain` 放在 `verify()`(`cmd/anet/verify.go:38`)里,走离线路径(不需要 daemon);`knownFlags["verify"]` 加 `chain`。
  步骤:逐行解码(`decodeRecord` 目前在 daemon 包未导出,需导出一个 `daemon.DecodeEvidenceRecord` 或挪到小包)→
  `ael.NewLedger().ImportBatch(recs, kel)`(ANetCore `ael/ael.go:333`)→ 断言 `Staged == 0`(有缺口时 Import 不报错,只 stage)、
  `State != QUARANTINED`、`Head()` 与 manifest 一致、每条 `ChainDID == "did:anet:"+AID`,AID 由 KEL 推导而非取自 manifest。
- 可选 `--hub URL`:用现成的 `fetchKELFor`(`verify.go:232`)从 hub 取 KEL,而不信任导出包自带的 KEL,并照现有风格打印
  "key history fetched from …"。
- 如实输出:导出包自洽不代表未被截尾;截尾只能靠外部锚点(见证 `anet verify --attestation`、hub 记录的链头)发现。

## 6. console.html(§7.5)完成度

全部完成,且有测试钉住(`console_test.go:26-114`):
- 注入 `window.__ANET = {aid,name,hub,nonce}`,无令牌(`console.go:71-84`);所有 `<script>` 带 nonce;无内联事件属性。
- `ctl()` 用 cookie + `X-Anet-CSRF`(`console.html:656-667`),`ctlUpload()` 同(`:923-930`),401 显示"重新运行 `anet console`"。
- 访客代码、"附完整交互内容"、`ctl("/hub-register")`、`ctl("/end-accept")`、"内容已验证"、`indexOf("image/")` 均已删除(测试断言不存在)。
- 评价只显示 request_cid/result_cid/completed(`console.html:813-827`);`tasks_completed` 标签为"已公开的有效回执"(`:489`,`:1118-1127`)。
- 附件内联用四类白名单 `INLINE_IMG`(`:880`)。未注册 hub 时只显示 CLI 命令 `anet hub-register`(`:746`、`:1376`)。
- CSP:`default-src 'none'; script-src 'nonce-…'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self' <hub 源>;
  base-uri 'none'; form-action 'none'; frame-ancestors 'none'`(`console.go:46-53`),hub 源经 `hubOrigin` 过滤(`:31`)。
- 页面实际调用的控制面路由:`/delegate` `/message`(JSON 与 multipart)`/end` `/review` `/threads` `/identities`
  `/console/switch` `/attachment` `/console/session`,都在白名单/公开表内。
- 加载测试 `TestConsoleLoadsTheDirectoryUnderItsCSP`(`console_test.go:423`)用无头 Chromium 真实加载并断言目录渲染;
  本机有 Playwright 缓存,已通过。**没有 Chromium 时 skip**(见风险)。
- 残留:`esc()` 不转义单引号(`console.html:710`);目前没有单引号属性插值,CSP 也拦住了内联脚本,暂不构成问题。

## 7. SI-7 的本机 A2A 接口一半(给 D1)

- 需要的判定函数已存在但绑在 daemon 里:`loopbackName`、`allowedHost`、`allowedOrigin`、`checkLoopbackControlAddr`
  (`ctlsec.go:172-198`、`:339`),`listenerPort`(`:160`)。`module/a2a` 只拿得到 `module.Host`,无法调用 `(*Daemon).hostGuard`。
  建议把这几个纯函数抽到无依赖小包(如 `internal/loopguard`),daemon 与 `module/a2a` 共用;注意该包不能引入 a2a-go(SI-8)。
- A2A 接口另有:拒绝任何带非空 Origin 的请求(比控制面更严,§11.4);Bearer 为 `a2a_token.txt`,常数时间比较。
- 交叉凭据测试(目前缺):控制面拿 A2A 令牌 → 401;A2A 接口拿控制令牌 → 401;A2A 接口带控制台会话 cookie → 401。
  控制面侧可直接加在 `ctlsec_test.go`,读 `a2a_token.txt` 的位置等 D1 定。

## 8. §5.3 CLI 现状

- `anet peers allow|trust <aid>`、`anet inbound approve <ix>`、`anet inbound policy approve|open` 调 `confirmOnTTY`
  (`peers.go:43-60`),打开 `/dev/tty` 失败即 `errNoTTY`,未输入 `yes` 不调用 daemon。`deny/remove/reject/closed` 不需要确认。
- `openTTY` 是包级变量(`peers.go:39`),测试用 `withTTY(t, "yes"|"no"|"")`(`peers_test.go:26`)+ `recordingDaemon(t)`
  (`:40`,记录被调用的路径)断言"无终端时没有请求到达 daemon"。新增支出上限命令、`anet pay`(C3/D2)照此写。
- 控制台会话不能批准:`/inbound/*`、`/peers/*` 不在 `sessionRoutes`,遍历测试覆盖。
- `anet inbound pending` 只返回元数据(`PendingView`,`pending.go:47-57`),与 MCP `inbound_pending` 同一路由。
- `hub-register --accept-delegations true`、`accept on` 被拒并给出三种策略说明(`peers.go:26-31`、`:187-228`)。

## 9. 测试夹具与已做的验证

- 控制面:`newPlane(t)`(`ctlsec_test.go:57`,真实 loopback 监听 + 磁盘令牌)、`p.bearer`、`p.session(t)` → `s.as(p)`
  (像页面那样带 cookie/CSRF/Origin)、`refusedForSession(resp, body)`、`splitPattern(pat)`、`newBareDaemon(t)`(无 hub)。
- CLI 端到端:`servedDaemon(t)`(`console_cli_test.go:17`)起真实控制面,返回 layout 与地址。
- 证据:`chainEvents(t, d, typ)`、`chainLength(t, d)`(`capability_test.go:695-706`);两节点 + 假 hub:`registeredPair(t)`;
  直接注入信封:`sealFrom` / `receive`(`sealtest_helpers_test.go`)。
- 本次用 `go test -overlay`(不改仓库文件)做了 4 处 mutation,全部被测试抓到:去掉白名单查表 → `TestEveryRoute…`、
  `TestASessionCannotReachABearerOnlyHandler` 失败;把 `POST /pull` 加进白名单 → `TestEveryRoute…` 失败;去掉 Host 校验 →
  `TestHostAllowlistRefusesOtherNames` 失败;跳过 `jsonFields` 检查 → `TestSessionJSONBodiesCarryOnlyConsoleFields` 失败。
- 命令:`source /data/projs/anet-dev/.anet-env.sh && go test ./internal/daemon/ -run 'TestEveryRoute|TestSession|TestConsole|TestHost|TestPull|TestAttachment' -count=1`。

## 10. 陷阱

1. 证据在 SQLite 事务提交之后才 Append;两者之间崩溃会丢事件且不留 gap 标记。audit 不能把"链上没有"当作"没发生"。
2. 每次 Append 都 fsync;按消息写的事件必须对陌生人(`public`/`public_cap`)聚合。
3. `/evidence` 输出的 payload 是展示形(字节已 base64),不能用来重算 id;导出另走原始编码。
4. `ImportBatch` 遇到缺口只 stage 不报错,`verify --chain` 必须检查 `Staged`。
5. 新 CLI 命令必须同时改 `knownFlags`、`usageAllText`,否则 `cmd/anet` 测试失败;`verify` 在 top-level switch(`main.go:89`),
   不走 `runClient`,不需要 daemon。
6. MCP `task_delegate` 目前可发 `pay:true` 到 `/delegate`(`mcpserv.go:80-90`),这是 §8.6 的 `gateway` 档,
   设计规定 MCP 不得调用;D2 重组 MCP 时删掉。
7. 下游消费者(ai-studio、Research-Galaxy agent-runtime、ANetOS-Web hubkeeper,见 0011 §2.4)仍调用 `/end-accept`、`/accept`,
   现在分别得到 410、400。
