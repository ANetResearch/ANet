# anet 对齐 A2A:设计文档(v0.2 系列)

状态:r3,2026-09-26。r1 经六视角对抗评审(54 个 agent;确认 45 条、反驳 3 条、次要 23 条),r2 逐条处理;r2 经闭合核对(42 处未闭合或新不一致),r3 逐条处理。评审记录见 scratchpad `review-confirmed.md` / `review-other.md`,文中以 `[Cn]` 引用确认项、`[m]` 引用次要项。
范围:ANetCore(→ v0.15.0)、ANet(→ v0.2.0)、ANetHub(→ wire 2)。
依据:勘察报告 01–11(`[R03 §5.1]` 形式引用),A2A 规范 v1.0.1(`Refs/a2a`),a2a-go v2.6.0(`Refs/a2a-go`),a2a-x402 v0.2(`Refs/a2a-x402-spec-v0.2.md`)。

本文只写"要做成什么样"和"为什么这样取舍"。现状与缺陷的逐行证据在勘察报告与评审记录里,不在此重复。

---

## 0. 四条已定决定(不再讨论)

1. **贡献 A2A,不竞争。** anet 是面向"无法做 HTTPS 服务端的 agent"的 A2A 实现:可达性、身份、目录、审计由 anet 提供,线上对象用 A2A 的。中继绑定、注册表 API、x402 自定义 scheme 以提案/参考实现形式回馈。
2. **hub 只做传输,不得看到任务内容。** daemon 之间端到端加密。不做 hub 侧 A2A 网关。外部 A2A 客户端经本机 daemon 的 127.0.0.1 A2A 接口接入。
3. **默认安全。** 全新安装不接受任何人的委派、不执行任何东西。开放感来自官方运营、任何人可用的公共 agent/工具。
4. **支付主推 a2a-x402**(v0.2,`x402.payment.*` 元数据,同一 Task 内 `input-required` → `payment-submitted` → `payment-completed`)。

---

## 1. 验收不变量

编号用 `SI-n`,避免与现有 `module/inv1`、`module/inv2`(组织数据不变量)混淆。每条都要有测试,测试要做 mutation 验证。

| 编号 | 不变量 | 验收方式 |
|---|---|---|
| SI-1 | hub 进程、hub 磁盘(含 WAL、备份、admin 数据目录)、hub 与 hub admin 的任何 HTTP 响应中,不出现任务内容:TaskDoc 正文、聊天正文、交付物、附件字节、能力参数、x402 `resource`。经中继结算路径到达 hub 的付款对象(`/x402/settle` 请求体、结算行、跨 hub 转发体)中 `resource`、`description`、`extra` 为空或固定值(结构化断言)。hub 网关与凭证路径(§2 冻结项)不在本条范围内 | `joint.sh` canary:hub 与 `anet-hub-admin` 同时运行(采集与快照周期调短),流程含一次对官方 agent 测试实例的调用;对 hub 数据目录、admin 数据目录、`/agents/{aid}`、`/fed/v1/reviews`、admin `/api/sessions*` 做字节搜索,命中 0;断言 admin `/api/official/{id}/insights`、`/acl`、`/monitor/*`、`/ops` 返回 404。mutation:关闭加密、恢复任一采集源,均须命中。反向断言:收件方解出 canary |
| SI-2 | hub 不存储 `from_aid`、`kind`、`interaction_id`;中继行 ack 即删 | hub 单测 + 联调读表 |
| SI-3 | daemon 只接受封装信封;未封装、签名不符、收件人不符、过期、重放一律拒收,不做明文回退 | 逐字段变异测试(§3.6) |
| SI-4 | 交互内每一条入站消息的发送方都经签名证明是该交互的对端 | 伪造 `from` 注入测试(真 hub 与 fake hub 都不校验 `from`) |
| SI-5 | 全新安装(`anet init` 之后):`inbound.policy=closed`;`peers.allow`、`peers.trust` 为空;`public_capabilities` 为空;`auto_reply.untrusted=off`;`payments.auto_max=0`、`agent_max=0`、`agent_daily_max=0`;`payees_file` 启用且为空 | `anet doctor --json` 按键断言;mutation 逐键改回 |
| SI-6 | "不知道"与"知道没问题"不合并:A2A `COMPLETED` 不蕴含效果 `OK`,也不蕴含回执 `verified`;两者以 `anet.*` 元数据原样携带 | 契约测试:控制面(即 MCP)输出反序列化为 a2a-go `a2a.Task`。能力任务在终态缺 `anet.effect_status` 即失败;文本任务在 `completed` 时缺 `anet.receipt_verified` 即失败;`effect_status=UNVERIFIED` 或 `receipt_verified≠verified` 时 state 仍为 COMPLETED,两者不合并为一个字段 |
| SI-7 | 本机 A2A 接口与控制面只接受回环 Host(否则 421),只接受各自的凭据;控制令牌不出现在任何 HTML 中;浏览器会话只能调用白名单路由 | 单测;遍历全部路由断言"在白名单或对会话拒绝" |
| SI-8 | 可插拔:`no_a2a` 构建中 `module/a2a` 与 a2a-go 符号数 == 0,完整构建 > 0;`taskboard` 改为加法方向后默认构建 == 0,`-tags taskboard` > 0;`internal/mcpserv`、`internal/daemon` 的依赖闭包不含 `a2aproject` | CI tag 矩阵 + `go list -deps` 检查 |
| SI-9 | 商户在结算前核对授权条款(收款方、金额、绑定值、scheme/network、有效期);hub 在入口 hub 与账本 hub 两处都核对内层授权与 `paymentRequirements`;同一绑定值最多一次成功扣款 | 少付/错付/挪用/过期/重放/跨 hub 负面用例 |
| SI-10 | 至少一次投递不丢消息:暂时性失败(存储错误、取消、p2p 限速)不 ack;同一信封经 hub 与 p2p 同时到达只处理一次 | 注入存储失败后重投;p2p 与 hub 并发投递 |

---

## 2. 决策记录

### X1 发送方对 hub 是否可见 → 认证发送,hub 不存储

- `/relay/send` 要求发送方以 relayauth v2(§3.7)认证,发送方必须是本 hub 已注册的 AID。hub 用它做按发送方的限流与配额,**不写入** `relay_message`,日志只记收件方与字节数。`/register` 另加按 IP 的限速(AID 可无限生成,否则按发送方限流对轮换 AID 无效)[C15f]。
- 理由:匿名发送下 hub 只能按 IP 限流;发送方身份在 E2E 之后本就可由 IP 与时间关联推出(R03 §4.5)。删除的是**存储中的社交图**。
- 代价(§21):hub 在发送时刻知道"谁发给谁"。sealed sender + 投递令牌列为后续(信封外层保留字段 7)。

### X2 入站默认 → `closed`,并回 `REJECTED`

- 默认 `closed`:允许名单之外的委派拒绝,回签名的 `rejected`,不写 interactions,不存内容。`closed` 下,对非公开能力与自然语言委派,deny 名单中的对端得到与陌生人相同的回复(同一 reason、同一限速桶);deny 对端发往未知 ix 的消息同样回 `TaskNotFound`。节点配置了 `public_capabilities` 时,被 deny 的对端调用公开能力会被拒而陌生人会被服务,对端可以察觉,文档写明 [C15d]。
- `approve`(待批队列)是可选策略。`open`/`approve` 下被 deny 的对端总能察觉自己被拒,文档写明。
- 拒绝的副作用有界:每对端限速 + daemon 全局令牌桶(不超过 hub 每发送方预算的 10%),桶空即静默丢弃通知;通知不进重试队列、不阻塞结果;拒绝证据按时间窗聚合写一条 `anet.delegation.refused_summary`,逐条细节写本地轮转日志,不上链 [C15]。

### X3 纯请求方节点的加密公钥 → `EncKeySet`,每条消息随信封携带

- 加密公钥是独立签名对象 `EncKeySet`(§3.1),注册到 hub,`GET /agents/{aid}/keys` 取回;hub 对非本地 AID 经联邦 `/fed/v2/keys/{aid}` 按精确 AID 查询(不进目录、不进索引)[C32]。
- **每条**内层消息都携带发送方的 `kel` 与 `keys`(几百字节,Padmé 取整后差异不可见)。接收路径因此不做任何网络请求 [C2]。
- `EncKeySet` 验证必须绑定预期 AID:发送侧预期 = 收件人 `to`,接收侧预期 = `inner.from` [C0]。

### X4 交互 id 在评价与结算中的可见性

- **结算**:授权的 `InteractionID` 字段填 `pay_bind = hex(SHA-256("anet/x402-bind/v1" 0x00 ‖ ix ‖ 0x00 ‖ task_nonce))`。发给 hub 的 `paymentRequirements` 中 `description`、`extra`、`resource` 一律为空或固定值,不带能力 id [m]。
- **回执**:格式不变;request CID 与 result CID 的原像各含 16 字节随机数:TaskDoc 的 `Tasks[0].Contexts` 加 `{Key:"anet.nonce", Visibility:"private"}`(`Contexts` 在 TaskDoc 规范原像内);交付物 JSON 加 `nonce`。对话记录交付物改为 v2 对象 `{"v":2,"nonce":"…","messages":[…]}`,所有读取方同时接受 v1 数组与 v2 对象,v2 有金标向量 [m]。
- 如实陈述(§21):hub 不存 ix、不能从 `pay_bind` 反推 ix;但能按付款方、收款方与时间把结算与公开评价关联。

### X5 本机 A2A 令牌 → 与控制令牌分离,只作用于"本机作为请求方、且对端等于路径 AID"的任务

- `a2a_token.txt`(0600)只授权本机 A2A 接口。授权模型:单一本机主体,按远端 agent URL 划分作用域 [C17][C44]。
- 经 A2A 令牌或 MCP 提交的付款属于"agent 档",受 `agent_max`/`agent_daily_max`(默认 0)约束;不视为用户同意 [C24]。

### 其他定调

| 问题 | 决定 | 理由 |
|---|---|---|
| 加密原语 | HPKE Base,Go 1.26 `crypto/hpke`,套件 DHKEM-X25519-HKDF-SHA256 / HKDF-SHA256 / ChaCha20-Poly1305 | 无新依赖;`info` 上下文绑定;可演进 |
| 发送方认证 | 先签名后加密,签名覆盖收件人、类型、交互、消息 id、时间、正文、`kel`、`keys` 与 HPKE `enc` | 防转投、防剥离替换,hub 看不到发送方 |
| 加密密钥 | 独立 X25519;每 7 天新建一把,有效 14 天,私钥保留到失效后 15 天。前向保密窗口:一条消息在发出后至多 29 天内可被持有收件人磁盘的一方解开 [m] | 与签名密钥分离;有界前向保密 |
| 交互级临时密钥 | 不做,保留字段 | 收益不抵复杂度(r1 已述) |
| 大附件 | v1 内联在密文内(单次 AEAD) | 先把"全部加密"做完整 |
| 文本任务完成语义 | provider 单方完成;requester 的结束请求由 provider daemon 自动接受并签回执;取消另立 | 与 A2A 终态模型一致(R02 §13.4) |
| 能力调用交互 | 与文本任务分开处理:不进对话轮次、不触发自动回复、不签对话记录回执;回执只来自能力结果路径 [C6][C29] | 能力路径是确定性执行 |
| A2A 卡片正本 | A2A AgentCard 为签名卡片;ANetCore `a2acard` 以标准库实现 RFC 8785 + JWS EdDSA,不引入 a2a-go(依赖白名单冻结);ANet 契约测试用 a2a-go 交叉验证;ADP 卡迁移期并行 | 一个事实来源;保持 ANetCore 依赖政策 |
| 本机 A2A 接口形态 | `module/a2a` 为 `module.Module`,新增窄接缝 `TaskSeam`;自行实现 `a2asrv.RequestHandler`,套用 SDK 的 JSON-RPC 与 REST 线协;A2A JSON 投影在无 tag 的内核包 `internal/a2ashape`(不导入 a2a-go),控制面与 MCP 直接输出它 [C20][C36] | 长驻 HTTP、需要事件推送;task id 即 interaction id;保住 `no_a2a` 符号判据 |
| taskboard | 由减法 tag 改为加法 tag `taskboard`(daemon 与 hub) | 公共看板把内容存在 hub 并对匿名公开 |
| 访客模式 | 删除(hub 与 daemon) | hub 是会话端点并持有代签身份 |
| hub admin 采集 | 删除全部采集源(hub-relay 与 ai-studio 两路)与 insights 的 `Recent`;admin 删除官方 agent 的 ops/monitor/runtime 路由,官方 agent 在 admin 中只登记 `id/aid/hub/caps` [C39] | hub 主机不应持有能读取官方 agent 内容的通道 |
| 评价 | 只收回执与评价对象,不收内容;内容绑定如实标 `UNVERIFIED` | 决定 2;诚实状态 |
| CAIP-2 网络名 | 本期不改,`hub:<aid>` 保留,scheme 文档写明偏离 | 牵涉三仓已签名对象 |
| x402 版本 | 对象层 x402 v2,承载在 a2a-x402 v0.2 metadata 键下 | anet 已是 v2 对象 |
| facilitator `errorReason` | 保留 x402 层小写常量(已公布取值不变),`module/x402` 内维护 `errorReason → x402.payment.error` 映射表 [m] | 不混淆两层协议 |
| hub 网关/凭证兑付 | 冻结;修 D2;兑付口经内核准入(§5.4);非回环兑付地址只接受 https [m] | 不接触内容,但属第二付款面 |
| 公开发放链 | 本期不改记录格式;§21 如实写明发放链公开金额、时间与 AID [C30] | 发放链是签名哈希链,改格式需要版本化,单独立项 |
| 控制面远程开关 | 删除 `control_allow_remote`;远程访问用 SSH 端口转发 [C10] | 与 Host 白名单矛盾 |
| 本机 A2A 根路径卡片 | 不提供;只提供 `GET /a2a/v1/agents` 列表与每个远端 agent 的代理卡片 [C23] | 根卡片没有可调用端点 |
| `A2A-Version` 缺省 | 缺省按 1.0 处理(文档列为偏离);显式给出非 1.x 版本返回 `VersionNotSupportedError` [m] | 规范要求缺省按 0.3,会拒掉不发该头的客户端 |
| 对外提交与发布 | 本期只写草稿;对外提交、发布、推送 GitHub、部署生产、清理生产数据,执行前征求产品负责人同意 | 对外与破坏性操作 |

---

## 3. 线协:封装信封与中继 v2(C2/C3)

编码一律 CoreDet-CBOR 整数键映射。时间为 unix 毫秒。

### 3.1 `EncKeySet`(ANetCore `seal` 包)

```
EncKey = {
  1: kid        bstr .size 16   ; SHA-256("anet-enc-kid/v1" ‖ u8(suite) ‖ pub)[:16]
  2: suite      uint
  3: pub        bstr            ; 套件 1 为 32 字节
  4: not_before uint
  5: not_after  uint            ; not_after - not_before ≤ 30 天
}
EncKeySet = {
  0: type       tstr            ; "anet.enckeys/1"
  1: aid        tstr
  2: seq        uint            ; = max(now_ms, last_seq + 1),持久化
  3: keys       [1*4 EncKey]    ; 按 not_before 升序
  4: issued_at  uint
}
SignedEncKeySet = { 1: set bstr ; coredet(EncKeySet)
                    2: ksn uint ; 签名密钥态序号 key_state_seq
                    3: sig bstr .size 64 }
```

`VerifyEncKeySet(signed, expectAID, kel, now) (*EncKeySet, error)`:
1. KEL 回放成功;回放得到的 AID == `set.aid` == `expectAID` [C0]。
2. 签名密钥态为 KEL 顶端活跃态(当前陈述,不用撤销宽限);`identity.VerifyObject(kel, aid, ksn, now, set, sig)`。
3. `keys` 非空;kid 与 pub 一致;有效期规则;至少一把 `not_before ≤ now + 5min < not_after`。

本节及 §3.7、§3.8 中的高水位(消费方三分支、发布方 409 规则、`peer_identity.keyset_seq`)一律比较 `EncKeySet.seq`(`set` 内的 2 号键),不比较 `ksn`。

消费方的 seq 高水位(发送侧取回、接收侧附件、本机代理)用三分支 [C16][C3]:
- `seq < 已见`:忽略(回退)。
- `seq == 已见`:`set` 字节与已存相同 → 视为同一对象,仍做完整验证(第 1–3 步),成功则刷新缓存时间;字节不同 → 分叉,忽略并计数。
- `seq > 已见`:验证成功则替换。

发布方(hub `POST /agents/{aid}/keys`、`/register` 的 `enc_keys`)要求严格大于,但 `seq` 相等且字节相同返回 200 不变;其余返回 409。`/register` 中 keys 不合格不使整次注册失败,按字段报告状态(同 `AdmitCard` 做法)。

密钥环生命周期 [m]:存 `<data>/enc_keys.cbor`(0600);每次变更先写入并 fsync,再发布;每 7 天新建一把;身份导入(`Restore`)不带密钥环时启动即新建,旧密钥封给本节点的在途消息丢失,文档写明。

发送方选钥:在有效期内、`not_before ≤ now` 的键中取 `not_before` 最大者。

### 3.2 套件

| suite | KEM | KDF | AEAD | 状态 |
|---|---|---|---|---|
| 1 | DHKEM(X25519, HKDF-SHA256) 0x0020 | HKDF-SHA256 0x0001 | ChaCha20-Poly1305 0x0003 | 实现 |
| 2 | MLKEM768-X25519 0x647a | HKDF-SHA256 | ChaCha20-Poly1305 | 保留编号 |

### 3.3 `SealedEnvelope` 与 `SealedInner`

外层(hub 可见):

```
SealedEnvelope = { 1: v uint (=1), 2: to tstr, 3: suite uint, 4: kid bstr .size 16,
                   5: enc bstr, 6: ct bstr }        ; 7: access 保留
HPKE info = coredet({1: "anet-relay/v1", 2: to, 3: suite, 4: kid});  aad = 空
```

内层(密文内):

```
SealedInner = {
  1: from tstr   2: ksn uint   3: to tstr   4: type tstr   5: ix tstr
  6: mid bstr .size 16   7: ts uint   8: exp uint   9: body bstr
  10: kel bstr           ; 发送方 KEL,每条必带;上限 256 事件 / 64 KiB
  12: keys bstr          ; 发送方 SignedEncKeySet,每条必带
  20: sig bstr .size 64
  21: pad bstr           ; 全零,不在签名内
}
```

- 键空间:0–63 为必须理解;接收方遇到 0–63 之间的未知键即拒收;64 及以上可忽略但仍在签名内。新增必须理解的字段须递增外层 `v` [m]。
- 签名原像:由**收到的通用映射**计算(不经结构体重编码):去掉 20、21,加入 `0: "anet-relay-sig/v1"`、`30: outer.enc`、`31: outer.kid`、`32: outer.suite`,再 coredet 编码 [m]。
- 填充:Padmé。
- 同一逻辑消息在 hub 与 p2p 上重试时复用同一份信封字节。

### 3.4 内层 `type` 与正文

| type | 正文 | 方向 |
|---|---|---|
| `anet.delegate/1` | `DelegateReq` | requester → provider |
| `anet.message/1` | `ChatMsg`;kind:`text` `end_request` `cancel` `stream_preview`(不进 history) | 双向 |
| `anet.status/1` | `StatusMsg`(新) | provider → requester |
| `anet.result/1` | `ResultResp` | provider → requester |

ANetCore `delegation` 增量(新字段一律 `omitempty`,另立全字段向量 `VEC_CHATMSG_2`、`VEC_DELEGATE_2`、`VEC_RESULT_2`、`VEC_STATUS_1`)[m]:
- `ChatMsg.Metadata []byte`(JSON 对象)。
- `DelegateReq.ContextID string`、`DelegateReq.Metadata []byte`。
- `ResultResp.Metadata []byte` [m]。
- `StatusMsg = {1: state, 2: text, 3: metadata(JSON), 4: at}`,state:`submitted` `working` `input-required` `rejected` `canceled` `failed`。
- `KindCancel`;`KindEndAccept` 删除发送方用法,接收方丢弃。
- `VerifyDelegateReq(r, kel)`、`VerifyResult(r, kel, …)` 改为接收外部解析好的 KEL;正文内 KEL 字段忽略或须是其前缀 [C4d]。
- 保留 metadata 键:`a2a.serviceParameters`(`{"A2A-Extensions":[…],"A2A-Version":"1.0"}`,规范 §12.3 的回退方式)、`anet.a2aError`(StatusMsg 中,取 A2A §3.3.2 错误名)、`anet.state`、`anet.reason`、`anet.retry_after_ms`、`anet.inbound` [C19]。

### 3.5 发送流程

1. **解析收件人公钥**:持久表 `peer_identity`(§3.8)中有有效 keyset 即用;每 10 分钟尽力向 hub 复核,hub 失败或 404 时继续使用已存,直到无有效键 [C2]。无记录时 `GET {hub}/agents/{aid}/keys` → `VerifyEncKeySet(signed, to, kel, now)`,KEL 须与已存延伸(§3.8)→ 写入 `peer_identity`(本节点主动联系对端,属授权上下文)。取不到即以明确错误失败,不降级。
2. 组装 inner(带本节点 `kel` 与 `keys`),签名,填充,HPKE 加密。`exp = ts + 14 天`(等于 hub 未投递 TTL,各 type 相同)。需要重试的外发复用首次信封字节;`now > exp` 时停止重试并写证据事件。
3. 交给传输列表(p2p 优先,hub 兜底)。`module.Transport.Send(ctx, toAID string, envelope []byte) error`;`module.Inbound.Receive(ctx, envelope []byte) error`。
4. 需要重试的外发(结果、状态)持久化信封字节,重试不重新封装。

### 3.6 接收流程

失败分两类 [C1][C33]:**永久**(P):ack 并丢弃,计数原因;p2p 上 `Receive` 返回 nil。**暂时**(T):任一步的存储读写错误、context 取消、第 0 步限速、第 9 步未知 ix 窗口;不 ack,p2p 上返回错误让发送方转走 hub;`now > exp` 后暂时性失败转为永久。

| 步 | 内容 | 失败 |
|---|---|---|
| 0 | (仅 p2p)按连接与全局限速,在解密之前 [C15e] | T(不 ack,`Receive` 返回错误,发送方转走 hub,由 hub 按发送方限流) |
| 1 | 外层解码;`v == 1`;`to == 本节点`;`suite` 已知 | P |
| 2 | 按 `kid` 查私钥(有效 + 保留期内) | P(`sealed-to-unknown-key`) |
| 3 | HPKE Open | P |
| 4 | 内层解码为通用映射;0–63 未知键拒收;`inner.to == outer.to` | P |
| 5 | `now ≤ exp`;`ts ≤ now + 5min`;`exp - ts ≤ 15 天` | P |
| 6 | 解析 KEL(不做网络请求):`inner.kel` 回放成功且推出 `from`;与 `peer_identity` 已存 KEL 比较:内层延伸已存 → 候选更新;已存延伸内层 → 用已存;分叉 → 拒收;无记录 → 用内层(首次信任,§21) | P;读取 `peer_identity` 出错为 T |
| 7 | 签名:顶端活跃密钥态直接接受;非顶端态仅当 `ts < SupersededAt` 且 `now − SupersededAt ≤ rotation_grace`(默认 1 小时)时接受 [C4c] | P |
| 8 | `keys` 附件是建议性的:`VerifyEncKeySet(keys, from, kel, now)` + 三分支高水位;失败不影响本消息 | — |
| 9 | 授权(只做判定;拒绝类回复经限速发出,不写库):按 type 判定(§5 入站策略;message/status/result 须交互存在或在待批表中、`PeerAID == from`、角色正确)。`anet.delegate/1` 的 `ix` 已存在时,仅当该交互 `role=inbound` 且 `PeerAID == from` 才进入第 10 步(重投/幂等路径),否则 P,计数 `ix-collision`,不回复 [m]。已认证发送方指向未知 ix 的 message/cancel:`now − inner.ts ≤ 10 分钟` 时按 T 处理(等待 delegate 先到),超过后回 `status{failed, anet.a2aError: TaskNotFound}`,与拒绝通知共用限速 [C19] | P;未知 ix 窗口内与读库出错为 T |
| 10 | 去重与处理:进程内按 `(from, mid)` 加锁;持久重放表已有该行 → delegate 走"已答复 → 重发结果"分支,其余 ack 不处理 [C33]。否则执行业务写入,**在同一 SQLite 事务内**插入重放行 `(from, mid, exp)`;不能纳入事务的副作用(能力执行、发结果)沿用现有业务幂等检查。重放行已存在、交互非终态且无结果、本进程内也无该 ix 的执行记录时,视为崩溃遗留:短能力调用重新执行(至少一次,沿用现有"无回执即重跑"),长能力调用不重跑(至多一次)。成功后:把候选 KEL/keys 写入 `peer_identity`(仅 §3.8 所列授权上下文)、`noteLivePeer(from)` [m]。§5.2 的写入(交互、待批项)与第 4 行的 `submitted` 回复都在本步与重放行同一事务提交之后进行 | T(存储) |

- 第 9 步拒绝的信封只进内存有界 LRU,不写持久重放表 [C15c]。
- 重放表、`pending`、`peer_identity` 与交互表同在 `interactions.db`,使第 10 步的同事务写入成立。
- 启动恢复:非终态且无结果的长能力调用交互置 `failed`,`anet.reason=interrupted`、`anet.effect_status=UNVERIFIED`(效果是否发生未知),经结果重试队列通知请求方 [C1]。
- 嵌套对象(TaskDoc、回执)在第 7 步通过后以 `msgTime = inner.ts` 验证,使用第 6 步解析的 KEL [C4b][C4d]。
- ANetCore `identity.Replay` 修正:每个被后续 rot/dip 取代的密钥态,即使中间隔着 ixn/drt,也得到该 rot/dip 的 `SupersededAt` 并置为非活跃;金标测试覆盖 Incept→drt→rot 与 Incept→drt→dip [C4a]。
- ANet 当前没有触发 KEL 轮换的产品路径,第 7 步的宽限规则本期只由测试覆盖;轮换投入使用时再补"轮换后重签未完成外发"。

### 3.7 中继 HTTP v2

线协版本 `X-ANet-Wire: 2`。hub 对 `< 2` 的 `/relay/*` 返回 426(正文:需要 anet ≥ 0.2.0);daemon 注册时发现 hub `< 2` 即拒绝工作。

**relayauth v2**(ANetCore)[C31][C30]:认证放在请求头 `X-ANet-AID`、`X-ANet-TS`、`X-ANet-Seq`、`X-ANet-Sig`(base64url)。

```
PreimageV2(action, aid, hubAID, ts, method, pathAndQuery, body) =
  "anet-relay/v2/" + action + "/" + aid + "/" + hubAID + "/" + ts + "/" +
  base64url(SHA-256(method ‖ 0x00 ‖ pathAndQuery ‖ 0x00 ‖ body))
```

- action:`send` `poll` `ack` `register` `profile` `visibility` `deregister` `p2p` `balance` `ledger` `redemptions`。
- hub 在大小上限内读取原始请求体字节后先算哈希再解码。
- hub 缓存 `(aid, sig)` 至窗口结束,拒绝重放。
- daemon 在注册前以 `GET /hub/identity`(或现有等价端点)取得 hub AID 与 KEL。

端点:

| 端点 | 请求体 | 响应 | 规则 |
|---|---|---|---|
| `POST /relay/send` | `{to_aid, envelope: b64}` + 认证头 | 200 `{id}`;400 信封结构不符;401;404 收件人未知且联邦也无;413;429 + `Retry-After`;507 信箱满 | 发送方须已注册;hub 检查外层 `v==1`、`to==to_aid`、`suite` 已知、`len(enc)==32`、`ct` 非空;不存 `from_aid` |
| `POST /relay/poll` | `{limit}` + 认证头 | `{messages:[{id, envelope}]}` | — |
| `POST /relay/ack` | `{ids}` + 认证头 | 200 | ack 即删 |
| `GET /agents/{aid}/keys` | — | `{aid, keyset, kel}`(b64) | 本地注册、联邦卡片,或经 `/fed/v2/keys/{aid}` 查询 |
| `POST /agents/{aid}/keys` | `{keyset}` | 200 / 409 | 自证明:hub 以已存 KEL 验证 + 发布方高水位 |
| `POST /register` | 现有 + `enc_keys` + `a2a_card` | 现有 + 各字段状态 | 删 `guest_messages`;KEL 须为已存延伸 |
| `GET /agents/{aid}/ledger`、`/balance`、兑付列表 | — | 本人签名 GET 才返回明细;无签名 401 | 同步修改 daemon `Balance`/`Reconcile` 与 prodtest 9f |

hub 限额(应用层,均为 flag):单条信封 96 MiB;每发送方令牌桶 20/s 突发 200;每收件方未投递 5000 条或 1 GiB;未投递 TTL 14 天;单次 poll 预算 48 MiB;`/register` 按 IP 限速。

`relay_message` 只保留 `id, to_aid, payload, size, created_at`;迁移重建表;`PRAGMA secure_delete=ON`。

### 3.8 KEL 与对端身份的持久记录

- ANetCore `identity.ExtendsKEL(old, new)`:分别返回"回退"与"分叉"两类错误;`old` 为 `new` 的前缀(逐事件原像比对)才算延伸。
- daemon 持久表 `peer_identity(aid PK, kel, kel_len, keyset, keyset_seq, card_seq, pinned_reason, updated_at)` [C2][C12]:
  - 只在授权上下文写入:第 10 步接受且对端 ∈ allow ∪ trust、或交互 role=outbound、或待批项经人工批准之后;本节点主动联系对端时;`anet peers allow <aid>` 时(尽力取回并固定,取不到则在首条有效消息时首次信任)。hub 自身 KEL、组织/黑板发行方 KEL 同样固定。`trust=public`、`trust=public_cap` 交互的对端不写入 `peer_identity`;其 KEL 与 SignedEncKeySet 存在该交互行中,供回复加密使用,交互终态后删除 [C12]。
  - `pinned_reason` 取值 `allow`、`trust`、`hub`、`issuer`、`outbound`;非空的行不淘汰,LRU 只在 `pinned_reason` 为空的行之间进行,且只由上述授权写入触发。
  - KEL 只经 `ExtendsKEL` 更新,keyset/card 只经高水位更新;不因入站流量淘汰;被非终态交互、待批项、结果重试、x402 报价引用的行不淘汰,其余按 LRU,上限大。
  - 陌生人(未授权)的 KEL 与 keys 只进独立的有界内存缓存,仅用于加密 `rejected` 回复,不进入 `ResolveKEL`、不参与高水位。
  - `peerKELs.remember` 必须回放并核对推出的 AID 等于存储键 [C0]。
- hub:`agent.kel`、`fed_card.kel` 只接受延伸(注册方就是本人,较短 KEL 直接拒绝)。

### 3.9 联邦

- `/fed/v1/forward` 信封删除 `from_aid`、`kind`、`interaction_id`,签名原像同步修改,联邦线协版本递增。
- 新增 `GET /fed/v2/keys/{aid}`:由发起 hub 签名、只接受对等表中的 hub;返回任一本地注册 AID 的 `{keyset, kel}`,不论可见性;请求方 hub 只为发起查询的 daemon 缓存,不进目录与索引 [C32]。
- 联邦卡片条目增加 `keys`。
- `FedReview` 删除内容字段;修空串被解码为非 nil 空切片而触发内容绑定的问题(R09 §5 第 9 点)。
- `/fed/v2/cards`(§10.6)。

### 3.10 p2p

帧只携带 `To`、信封字节与 `ID`。anetpeer 为每个 recv 帧铸造唯一 `ID`,按 `ID` 关联 ack;daemon 在 `Receive` 得出"应 ack"结论后回 `{Op: ack, ID}`,暂时性失败不回 ack [C5]。重写 `module/p2p` 的 fake peer 按 ID 关联;并发两条相同 `To` 的投递各自得到自己的 ack(mutation:ack 键改常量)。帧增加 `V`(=2);anetpeer 对缺 `V` 或 `V < 2` 的入站投递帧不交给 daemon,回 `{Op: error, ID, Error: "peer requires anet >= 0.2.0"}`,旧发送方因此不会记为已投递;测试:无 `V` 的帧得到 error 且 daemon `Receive` 未被调用 [m]。

---

## 4. 任务模型(daemon)

### 4.1 存储增量

| 列 | 说明 |
|---|---|
| `state` | 取代现有 `status` 列;取值 `submitted` `working` `input-required` `completed` `failed` `canceled` `rejected`。迁移:`done`→`completed`,`failed`→`failed`;`queued`/`ending` 按最后一条消息的发送方写入(requester 最后发言 `working`,provider 最后发言 `input-required`,无消息 `submitted`)。迁移后删除 `status` 列、`StatusEnding` 与 `idx_ix_role_status`,改建 `(role, state, seq)` 索引;`end_req_by`/`end_acc_by` 只作历史字段 |
| `state_at`、`state_seq` | 状态时间与单调序号(阻塞等待比较序号,不比较当前值)[C35] |
| `context_id` | 客户端给出则原样保存,否则由 requester daemon 铸造 |
| `trust` | `peer` `public` `public_cap` `approved` |
| `is_capability` | 能力调用交互 |
| `task_nonce` | TaskDoc 随机数 |
| `pay_state`、`pay_required`、`pay_auth_ids`、`pay_payload`、`pay_receipts`、`quote_expires_at` | x402(§8)。`pay_state` 取值:空(无报价)、`required`(已报价/已收到报价)、`submitted`(provider:payload 已持久化待结算;requester:已发出 payment-submitted)、`completed`(结算成功)、`failed`(确定失败)、`rejected`(requester 拒付)。provider 侧 `pay_auth_ids` 至多一项,即 §8.4 所称已持久化的 `auth_id` |
| `peer_kel`、`peer_keys` | `trust=public`/`public_cap` 交互的对端 KEL 与 SignedEncKeySet,供回复加密,终态后删除(§3.8) |
| `message.msg_id`、`message.metadata`、`message.kind` | 发送方也存 msg_id;kind 增加 `status`、`payment`;对话轮次只取 kind=text 且无控制元数据 [C29] |

状态由事件显式写入 [C35]:

| 事件 | state |
|---|---|
| requester 建任务 | submitted |
| requester 发 text 消息或 payment-submitted | working |
| requester 发 cancel 或 payment-rejected(`pay_state ≠ submitted`) | canceled |
| requester 在 `pay_state=submitted` 后发 cancel | 不变(§4.2) |
| provider 发消息,未标 `anet.state=working` | input-required |
| provider 发消息标 `anet.state=working`,或 `StatusMsg` | 其携带的状态 |
| 结果到达 | 按 §4.3 映射:completed / failed / rejected |

- `IsTerminal()` 判定 `state ∈ {completed, failed, canceled, rejected}`,替换全部现有终态判断(`autoreply.go:294`、`delegation.go:141/223/243/268/380` 等)。
- 状态迁移用 `UPDATE interaction SET state=?, state_at=?, state_seq=state_seq+1 WHERE id=? AND state NOT IN ('completed','failed','canceled','rejected')` 并返回是否更新,防止终态互相覆盖、防止在已取消交互上签回执。
- 列表按 `state_at` 降序分页(修 R02 D8)。

### 4.2 完成与取消

文本任务:
- provider 完成:`reply_task(state=completed)`、`anet end`(provider 侧)或自动回复判定完成 → 签回执(对话记录 v2)→ `anet.result/1`。
- requester 结束请求(`end_request`)→ provider daemon 自动完成(不需要 provider 的 agent)。
- requester 取消(`cancel`)→ 本地 `canceled`;provider 置 `canceled`、取消长调用、回 `status{canceled}`,不签回执。

能力调用 [C6][C29][C34]:
- 未提交付款(`pay_state` 为空或 `required`)且未开始执行时,收到 `end_request` 或 `cancel` → `canceled`,不签回执。
- 未付费能力执行中:`cancel` → 尽力取消 context;`end_request` 忽略,结果照常交付;两者都不签对话记录回执。
- `pay_state ∈ {submitted, completed}` 之后收到的 `cancel`/`end_request` 按下文"付款已提交"条处理,不取消执行。
- 回执只来自能力结果路径。
- 付款已提交(`pay_state=submitted`)之后,requester 的取消不改变本地状态,返回 `working` + `anet.cancel_requested=true`,等 provider 的 status/result;provider 在结算成功后不接受取消,完成并交付,无法交付时回 `failed` + `x402.payment.receipts`。
- 带 `x402.payment.receipts` 的 status/result 即使到达已终态交互,也验证并记录结算证据(不重开 A2A 状态)。

其他:终态之后只拒绝新的 SendMessage 输入(`UnsupportedOperation`)。出站结果重试队列:持久化信封字节,指数退避最长 24 小时,重启恢复。

### 4.3 能力调用五态

| 效果状态 | state | 附加 |
|---|---|---|
| OK / UNVERIFIED | completed | `anet.effect_status` |
| FAILED | failed | 同上 |
| UNAVAILABLE(暂时性) | rejected | `anet.retry_after_ms` |
| UNAVAILABLE(其他) | rejected | `anet.reason` |
| PAYMENT_REQUIRED | input-required | §8 |
| 中断(崩溃后效果未知) | failed | `anet.effect_status=UNVERIFIED`、`anet.reason=interrupted` |

已结算(`pay_state=completed`)的交互不使用 rejected:UNAVAILABLE 映射为 failed,并带 `x402.payment.receipts`(§4.2、§8.2)。

---

## 5. 入站策略(daemon 内核,无 tag)

### 5.1 配置

```json
"inbound": {
  "policy": "closed",
  "allow_file": "peers.allow",
  "deny_file": "peers.deny",
  "trust_file": "peers.trust",
  "public_capabilities": [
    {"id": "net.echo", "per_caller_per_min": 60, "per_caller_per_day": 2000,
     "global_per_min": 1200, "max_inflight": 16, "max_args_bytes": 4096}
  ],
  "reject_notice": {"per_peer_per_hour": 6, "global_per_min": 60},
  "pending": {"max_total": 200, "max_per_peer": 3, "ttl_hours": 72}
}
```

- `policy`:`closed`(默认)、`approve`、`open`。
- `allow_file`(可委派)、`trust_file`(可驱动本机 exec 与 A2A 后端,§6、§11.6)、`deny_file`:每行一个 AID,每次判定重读,文件不存在等于空,deny 优先。`trust_file` 从 `auto_reply` 移到 `inbound`,exec 与后端共用同一判定 [C9]。
- 配置校验集中在一个函数,由加载、`POST /autoreply`、入站策略写入共同调用:`open` 与"对非信任对端启用 exec"或"接受非信任对端的后端"不能同时成立,违反者 409,无论先写哪一个 [C8]。后端部分经 `module.Host` 新增的 `DeclareUntrustedBackend()` 完成(理由写在接口注释):`module/a2a` 构建时若存在 `accept_untrusted: true` 的后端即调用它;内核校验函数只读取这一声明,不解析 `modules.*` 配置。后端只经配置文件设置;与 `open` 冲突时加载即拒绝启动,运行时改为 `open` 的写入按同一声明返回 409。
- 迁移:旧 `accept_delegations` 缺省或 `true` → `closed`,日志提示一次;`false` → `closed`。`anet accept on` 报错并说明三种策略与 `anet peers allow`,非零退出;`accept off` 映射为 `closed`;`hub-register --accept-delegations` 与 `POST /accept` 同样处理 [C8]。
- 撤销对已有交互生效 [m]:第 9 步对所有入站信封先查 deny;`trust=peer` 的入站交互再查 allow;自动回复每次调用重读 trust 与 deny;对端进入 deny 时,其活动交互置 `canceled` 并写 `anet.policy.changed`。

### 5.2 判定顺序(`anet.delegate/1`)

第 9 步只做判定,并发出"回给请求方"列中的拒绝类回复(`rejected`、`TaskNotFound`,经限速,不写库);"动作"列中的写入(交互、待批项)以及第 4 行的 `status{submitted, …}` 回复,都在第 10 步与重放行同一事务提交之后进行。

| 序 | 条件 | 动作 | 回给请求方 |
|---|---|---|---|
| 1 | from ∈ deny | 丢弃 | `closed` 下同第 6 行;其他策略下 `rejected` |
| 2 | 能力调用且能力 ∈ public_capabilities | 内核准入(§5.4)后写入 `trust=public_cap`,不把 Intent 存为对话消息,Goal 记能力 id | 能力结果 / `input-required` / `rejected` |
| 3 | from ∈ allow | 写入 `trust=peer` | — |
| 4 | policy = approve | 写入待批表 | `status{submitted, anet.inbound=pending_approval}` |
| 5 | policy = open | 能力调用 → 拒绝(`capability_not_public`,不存);自然语言任务 → `trust=public` | — |
| 6 | 其他 | 丢弃,不存 | `rejected`,`anet.reason=not_accepting` |

`public_cap` 交互只接受 `cancel`、`end_request`(二者按 §4.2 能力调用规则处理,只取 kind,正文丢弃),以及带 `x402.payment.status` 为 `payment-submitted`/`payment-rejected` 的消息(只保留元数据,正文与附件丢弃不存);其他消息丢弃并计数;不触发自动回复;不出现在 `ActiveThreads`、自动回复循环、§11.6 后端中 [C6]。

### 5.3 待批队列(`approve`)

- 独立表 `pending`:签名 TaskDoc、请求方 AID、KEL 与 SignedEncKeySet、到达时间、附件元数据(不存字节)、后续消息(每条目最多 5 条;第 9 步把指向待批 ix 的消息与取消判定为待批路由,第 10 步在同一事务内追加到待批项并写重放行)[m][C2]。
- 上限超出直接 `rejected`;TTL 到期回 `rejected`。
- 批准只经人工通道:`anet inbound approve <ix>`、`anet peers allow <aid>` 要求从 `/dev/tty` 读取确认,非 TTY 调用拒绝;控制台会话不能批准 [C24][m]。批准时按 §3.6 第 7 步的规则对当前 KEL 重验。TTY 检查在 CLI 进程内完成,daemon 无法区分调用来源,边界见 §21 第 13 条。
- MCP `inbound_pending` 只返回元数据(AID、到达时间、字节数、request CID、能力 id),不返回目标与正文 [m]。文档写明 anet CLI 对 agent 可达这一前提。

### 5.4 内核准入接缝

`module.Host` 增加 `Admit(callerAID, capID string, argsLen int) (release func(), refusal string)`,理由写在 `module.go` 接口注释 [C14]。内核实现:deny(每次重读)、public_capabilities、按调用方与全局配额、`max_inflight`、`max_args_bytes`。`Admit` 只在 §5.2 第 2 行(公开能力)与凭证兑付口 `RedeemVoucher` 调用;第 3 行(allow 名单)与经人工批准的待批项沿用现有执行路径,不要求 capID ∈ public_capabilities(deny 已由第 1 行判定) [C14];兑付口以 hub 证明的 `v.Payer` 作为调用方;`Invoke` 结束后调用 `release`。

兑付口不把出示者当作已认证调用方:`provider.Call` 增加 `Via`(`relay`/`voucher`),文档规定 provider 在 `Via=voucher` 时不得依据 `CallerAID` 授权;测试断言任何实现 `Priced` 的 provider 不读取 `CallerAID`。兑付拒绝沿用现有 `anet.voucher.refused` 事件,加策略原因码。

---

## 6. 自动回复 exec 与 A2A 后端

- exec 只对 `trust_file` 中的对端以现有方式运行。`auto_reply.untrusted`:`off`(默认,不调用本地 agent)或 `sandbox`。判定对象是交互对端,入站与出站一视同仁。能力调用交互一律跳过自动回复。
- `sandbox`(Linux,bubblewrap)[C7]:
  - 只读绑定白名单:`/usr`、`/bin`、`/lib*`、`/etc` 的必要子集、agent 二进制与运行时路径、需要时 `/run/systemd/resolve`。
  - `--tmpfs` 覆盖 `/tmp`、`/var/tmp`、`/dev/shm`、`/run`、家目录;只绑定本交互工作目录为可写。
  - 不绑定:解析后的数据目录(`ANET_DATA_DIR` 实际指向处)、`/tmp/anet-<uid>`、`$XDG_RUNTIME_DIR`、`/run/user/$UID`、`modules.anetlink.socket`。
  - `--unshare-ipc --unshare-pid --unshare-uts --new-session --die-with-parent`。
  - 凭据:家目录为 tmpfs,agent 的登录状态不可见;`sandbox` 模式要求配置 `auto_reply.api_key`(经 `agentExecEnv` 以环境变量传入)。未配置时视为 `sandbox_unavailable`,按下条失败闭合;不绑定 `~/.claude`、`~/.codex` 等 agent 配置目录(其中含会话历史与其他项目的凭据);doctor 报告此项 [C7]。
  - 网络隔离(`--unshare-net` + 只放行模型 API 主机的出站代理)本期不做,§21 写明残余:宿主回环 TCP 服务与抽象 Unix socket 在沙箱内可达;`service` 模块后端须以每后端令牌头认证 daemon。
  - 失败闭合按方向:入站回 `status{rejected, anet.reason=sandbox_unavailable}`;出站(本机是 requester)不发任何消息,任务留在收件箱,写 `anet.autoreply.invoked{sandboxed:false, exit:"sandbox_unavailable"}`。
  - 联调在沙箱内断言失败:读控制令牌与 A2A 令牌、连接 D-Bus、连接 `c1.sock`。
- 所有 exec 统一加固:工作目录在数据目录之外(`$XDG_CACHE_HOME/anet/work/<aid短>/<ix>/`,0700);环境变量白名单;outbox 只收常规文件、拒绝符号链接、规范化后须在 outbox 内、有数量与总量上限;对端只收到通用错误与本地日志关联 id;任务目标作为不可信数据呈现;`ANET_EXEC_COMMAND` 只在测试注入点生效。
- 证据:`anet.autoreply.invoked{agent, interaction_id, peer_aid, trusted, sandboxed, exit, duration_ms}`。

---

## 7. 控制面加固

1. Host 白名单:`127.0.0.1:<port>`、`localhost:<port>`、`[::1]:<port>`,否则 421。删除 `control_allow_remote`;`control_addr` 非回环拒绝启动;远程访问用 SSH 端口转发 [C10]。
2. 控制台票据:`anet console` 用控制令牌取 60 秒单次票据,打开 `/console#t=<ticket>`;页面清除片段后换取会话。cookie 名 `anet_s_<port>`、`HttpOnly; SameSite=Strict; Path=/`;CSRF 值只在页面内存中,无端点可凭 cookie 取回,刷新需新票据 [C10]。
3. 会话路由白名单(与 `console.html` 实际调用一致)[C10][C41]:只读(`/status` `/threads` `/thread` `/inbox` `/results` `/evidence` `/balance` `/identities` `/find`)、`GET /attachment`、multipart 的 `/delegate` 与 `/message`(JSON `attachments` 路径字段与 `pay:true` 拒绝)、`/end`、`/review`、`POST /console/switch`。其余一律 bearer-only,包括 `/pull`、`/autoreply*`、入站/peers/trust 写入、`/hub-register`(移到 CLI)、`/hub-leave`、`/visibility`、`/p2p-advertise`、`/shutdown`、`/x402-authorize`、`/redeem`、`/reconcile`。测试遍历全部路由。
4. 身份切换:`POST /console/switch {aid}`,当前 daemon 从 `RunningDaemons()` 找到目标、读取其控制令牌(同 uid)、向目标取票据,返回目标控制台 URL。
5. `console.html` 改造:注入不含令牌的 `window.__ANET{aid,name,hub,nonce}`;`ctl` 改为 cookie + `X-Anet-CSRF`;删除访客代码、评价的 goal/deliverable 渲染与"附完整交互内容"文案;`tasks_completed` 标签按 §9 改;附件内联判断改用四种图片类型白名单;删除"连接到本 Hub"按钮与 `ctl("/hub-register")`(未注册时只显示 CLI 命令 `anet hub-register`),删除"同意结束"按钮与 `ctl("/end-accept")`,结束 UI 只保留 `/end`。CSP:脚本 nonce,`frame-ancestors 'none'`,`connect-src 'self' <cfg.HubURL 源>`;加载测试断言目录数据仍能显示。
6. `/attachment` [C11]:按 `http.DetectContentType` 嗅探,只有 png/jpeg/gif/webp 内联,其余 `application/octet-stream` + `Content-Disposition: attachment`;所有响应加 `Content-Security-Policy: default-src 'none'; sandbox` 与 `nosniff`;去掉 `immutable` 一年缓存;收到附件时把 `Mime` 改写为嗅探结果。
7. `/pull` [C42]:总是写入新子目录 `<out_dir>/anet-<ix前12>/`(Mkdir + Lstat,拒绝符号链接);文件以 `O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW` 打开,已存在且内容 CID 相同视为已取回,否则换名;`safeName` 中和前导点、去控制与双向字符、限长;空或相对 `out_dir` 返回 400;`out_dir`、数据目录与 exec 工作目录均先经 `filepath.EvalSymlinks` 解析为真实路径后再比较前缀,拒绝落在后两者之内的 `out_dir`。
8. `/ping` 去掉 `Access-Control-Allow-Origin: *`。运行时目录校验非符号链接、属主、0700,优先 `$XDG_RUNTIME_DIR`。`anet mcp` 在显式选定身份时用严格解析。
9. 策略类写入写 `anet.policy.changed{field, from, to}`。

---

## 8. 支付:a2a-x402 同任务流

### 8.1 扩展声明

- URI `https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2`;编入 x402 模块且有标价公开 skill 时声明;全部 skill 收费时 `required: true`。经卡片贡献接缝(§10.4)提供。
- 激活头同时接受 `A2A-Extensions`(按逗号拆分)与 `X-A2A-Extensions`;响应回显"请求的 ∩ 支持的"扩展。

### 8.2 元数据键

| 键 | 位置 | 值 |
|---|---|---|
| `x402.payment.status` | status/message/result metadata | `payment-required` `payment-submitted` `payment-verified` `payment-completed` `payment-failed` `payment-rejected` |
| `x402.payment.required` | provider 的 `input-required` | `PaymentRequired`(x402 v2) |
| `x402.payment.payload` | requester 发往 provider 的付款消息 | `PaymentPayload`(x402 v2) |
| `x402.payment.receipts` | completed/failed/canceled(凡已有结算)与失败 | `[SettlementResponse]`,全部历史;失败项 `{success:false, errorReason, network, transaction:""}`;hub 签名收据在 `extensions["anet.settlement.receipt"]` |
| `x402.payment.error` | 失败 | a2a-x402 错误码(由 `errorReason` 映射) |

provider 在结算成功后先发 `payment-verified` 状态再执行能力 [m]。

### 8.3 时序(daemon 之间全部在 E2E 信封内)

```
A --delegate(ix)--> B
B: 标价且无付款 → 持久化 ix→requirements(quote_expires_at = now+24h)
   → status{input-required, {status:payment-required, required}}      ; 证据 anet.payment.quoted
A: 验信封签名 → 支出策略(§8.6):
   auto 档内 → 签授权(InteractionID = pay_bind) → message{payment-submitted, payload}
   否则 → 本机 state=input-required,anet.reason=needs_operator_approval
          (agent 档:MCP submit_payment / 本机 A2A 客户端;人工档:TTY 上的 anet pay)
   拒付 → message{payment-rejected};A 本地置 canceled;B 置 canceled 并回 status{canceled}
B: 收到 payment-submitted:分派给能力执行器(不进自动回复,复用 runCapabilityCall 的长短分流与并发上限)[C29]
   → pay_state ∈ {submitted, completed} 时拒收新的付款 [C25]
   → 核对(§8.4)→ 持久化 pay_state=submitted + auth_id + payload
   → POST hub /x402/settle{paymentPayload, paymentRequirements}
      成功 → status{working, payment-verified} → 执行 → result + {payment-completed, receipts}
      确定失败 → status{input-required 或 failed, {payment-failed, error, receipts}}
      未知(传输错误、超时、`settlement_pending`)→ 不回 input-required,用同一 payload 重试直到确定;重启恢复 submitted 行
A: 收据核验:AuthID ∈ 本 ix 已签授权集合,PayTo == PeerAID,金额与授权一致 → 证据;同一 ix 第二张成功收据写证据并在 audit 标出 [m]
```

- 报价 24 小时过期:provider 置 `failed` + `payment-failed`/`EXPIRED_PAYMENT`;未付报价计入按调用方配额;requester 不对过期报价签授权 [m]。
- `DelegateReq.Payment`(预付)保留,走同一核对与结算路径。
- requester 重试:同条款重报价时重发尚未过期的同一授权(hub 按 auth_id 幂等);授权过期或条款变化才签新的;每个 ix 至多一个未决授权;发出 payment-submitted 后,在收到该授权的确定结果(payment-completed,或 payment-failed)之前不签新授权 [C13][C25]。

### 8.4 商户核对(provider,结算前)

`auth.PayTo == 本节点`;`auth.Amount ≥ 报价`;`auth.InteractionID == pay_bind(ix, task_nonce)`;scheme/network ∈ 已报价选项;授权未过期;报价未过期。不符 → `payment-failed` + 错误码(§8.5 映射表;收款方不符、绑定不符、无待付报价均为 `SETTLEMENT_FAILED` + `anet.reason`,金额不足 `INVALID_AMOUNT`,scheme/network 不在已报价选项 `NETWORK_MISMATCH`,授权或报价过期 `EXPIRED_PAYMENT`),不结算、不执行。`anet.replayed=true` 仅当收据 `AuthID` == 本 ix 已持久化的 `auth_id` 时接受。

### 8.5 hub facilitator [C26][C13][C25]

- `CheckRequirements(auth, req)`:比较 `payTo`、`amount ≥ req.amount`、`network`、`scheme`,只解码授权,不需要 KEL。
- `SettlePayment` 拆成内部 `settleAuth`(无 requirements,供已自行核对的调用方)与公开 `SettleWithRequirements`(`hX402Settle` 用)。
- 入口 hub(network ≠ 本 hub):先 `CheckRequirements`,再把 `{x402Version, paymentPayload, paymentRequirements}` 转发给账本 hub;回执 `PayTo/Amount` 与 requirements 一致才 `ClearFromPeer`。账本 hub:`decodeAuth` 后再 `CheckRequirements`,`paymentRequirements` 必填。
- `Redeem` 保留 `payTo == hubAID` 检查并调用 `settleAuth`;网关以 `{payTo: aid, amount: price}` 构造 requirements,凭证金额取结算额。
- `/x402/verify`:账本不在本 hub 时返回 `network_mismatch`(不转发)。
- 重放查找移到签名验证之后、有效期检查之前:已扣款的 auth 任何时候都返回原回执。
- `UNIQUE(payer, interaction_id) WHERE interaction_id != ''`;冲突且 auth_id 不同 → `Success:false`、`errorReason=duplicate_binding`,附原结算交易号;auth_id 相同 → 原 replay 行为。结算失败事务回滚。
- 跨 hub 传输错误、超时、"对端已结算本地未清算" → `settlement_pending`(非终结),保留对端回执并重试 `ClearFromPeer`(按 auth_id 幂等)。
- `errorReason` 取 x402 层小写常量,补齐发出点:`insufficient_funds` `invalid_signature` `expired_payment` `network_mismatch` `unsupported_scheme` `invalid_amount` `payee_mismatch` `duplicate_nonce` `duplicate_binding` `unknown_payer` `settlement_pending` `settlement_failed`。`module/x402` 映射为 a2a-x402 的 `x402.payment.error`,映射表两侧钉字符串:

| hub errorReason / provider 本地核对 | x402.payment.error |
|---|---|
| `insufficient_funds` | `INSUFFICIENT_FUNDS` |
| `invalid_signature` | `INVALID_SIGNATURE` |
| `expired_payment`、报价过期 | `EXPIRED_PAYMENT` |
| `duplicate_nonce`、`duplicate_binding` | `DUPLICATE_NONCE` |
| `network_mismatch` | `NETWORK_MISMATCH` |
| `invalid_amount` | `INVALID_AMOUNT` |
| `payee_mismatch`、`unsupported_scheme`、`unknown_payer`、`settlement_failed`、绑定不符、无待付报价、客户端自带 payload | `SETTLEMENT_FAILED`,原始原因放入 `anet.reason` |
| `settlement_pending` | 不映射(非终结,不发 payment-failed) |

- `/supported` 增加 `extensions`、`signers`。

### 8.6 支出策略(唯一执行点)[C24][C27]

```json
"payments": { "auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
              "explicit_max": 10, "daily_max": 50, "payees_file": "payees.allow" }
```

- 三档:**auto**(daemon 自动,`auto_max`);**agent**(MCP `submit_payment`、本机 A2A 客户端 `payment-submitted`,`agent_max`/`agent_daily_max`);**人工**(`anet pay <ix>`、`anet redeem`,须 `/dev/tty` 交互确认,`explicit_max`/`daily_max`)。修改支出上限的命令同样要求 TTY。控制台会话不能授权付款。
- 执行点:`module.PaymentSeam` 增加 `AdmitSpend(payTo string, amount uint64, purpose string) error`,由内核实现;x402 模块 `Authorize` 在 `seam.Sign` 之前调用它(覆盖模块内部的 `Redeem`)。`module.Payer.Authorize` 改为 `Authorize(opt payment.PaymentOption, ix, bind, purpose string) ([]byte, error)`:授权的 `InteractionID` 填 `bind`;`anet.payment.authorized` 事件增加 `interaction_id`(ix)、`pay_bind`、`purpose` 字段。一把互斥锁覆盖"检查—记录";日累计按已签授权额计(不是已结算额),启动时按 purpose 从该事件重建,读取量覆盖 24 小时 [C27]。
- 档位由控制面路由决定:

| purpose | 调用面(控制面路由) | 单笔上限 | 日累计 |
|---|---|---|---|
| `task-auto` | §8.3 自动 | `auto_max` | 计入 `agent_daily_max` 与 `daily_max` |
| `task-agent` | `POST /tasks/pay`(MCP `submit_payment`/`reject_payment`;本机 A2A 客户端经 `TaskSeam`) | `agent_max` | 计入 `agent_daily_max` 与 `daily_max` |
| `task-manual` | `POST /tasks/pay-manual`(只由 `anet pay` 在 TTY 确认后调用) | `explicit_max` | `daily_max` |
| `gateway` | `/x402-authorize`、`/delegate` 的 `pay:true`(预付) | `explicit_max` | `daily_max` |
| `redeem` | `/redeem` | `explicit_max` | `daily_max`,不受 payees 约束 |

  持控制令牌的进程可以调用任一路由(§21 第 13 条);MCP 服务端不调用 `task-manual`、`gateway`、`redeem` 三类路由。
- `payees_file`:键非空即启用白名单,文件缺失等于空表;`anet init` 创建空文件。`redeem`(收款方为 hub AID)不受 payees 约束,受人工档上限。
- TTY 确认在 CLI 进程内执行,daemon 只校验控制令牌,无法区分调用是否经过 TTY。
- 付费演示需要用户先把演示 AID 加入 payees 并在 TTY 上放开 agent 档上限;install.sh 打印的示例用免费能力。

### 8.7 本机 A2A 客户端的付款消息 [C28]

- 本机 daemon 是 a2a-x402 §5.1 所说的签名服务。本机客户端在同一 taskId 上发 `x402.payment.status: payment-submitted`,**不带** `x402.payment.payload`;以 `anet.payment.accept` 给出从 `x402.payment.required.accepts` 原样复制的所选项(只有一项时可省略)。
- daemon 核对所选项与本 ix 存储的 requirements 逐字节相同,按 agent 档上限签授权,在 E2E 信封内转发标准 x402 v2 `PaymentPayload`。
- 客户端自带 `x402.payment.payload` → `x402.payment.status: payment-failed`、`x402.payment.error: SETTLEMENT_FAILED`、`anet.reason=client_payload_unsupported`(其付款方不是本节点,转发也无法结算);所选项不在 accepts → 同上,`anet.reason=option_not_offered`。
- 代理卡片的 x402 声明 `required` 强制为 false,params `{signer:"anet-daemon", clientPayload:false}`;不论客户端是否激活扩展,代理任务上都出现 x402 状态键。客户端未激活时:auto 档内照常自动付款,超出时 `input-required` + `anet.reason=payment_extension_not_activated`。

---

## 9. hub 内容移除

| 面 | 处置 |
|---|---|
| relay 存储 | §3.7;ack 即删;TTL 14 天;`hub-db-roll.sh` 不再用 `delivered_at`,改为只按 `created_at` 清理或删除该查询,保留周备份与 WAL checkpoint;备份排除 relay 表 [C38] |
| admin 采集 | 删除全部采集路径:`hub-relay` 源与 `RelayRowsSince`;ai-studio 的 `jobRec`、`runHistory`、`runHistoryHTTP`、`appendJob`、`Manifest.Datasets`;`monitorPathWhitelist` 中的 `state`(即 insights `Recent`);`RunAll` 不触碰任何源、不写 datasets(单测 + mutation)[C39] |
| admin 官方 agent | 删除 `POST /api/official/{id}/ops`、`GET /api/official/{id}/monitor/{what}`、`GET /api/official/{id}/insights`、`POST /api/official/{id}/acl` 四条路由,以及 `MonitorProxy`、`buildInsights`;`Manifest` 删除 `runtime`、`monitor`、`ops`、`datasets` 字段;保留 `GET/POST/DELETE /api/official` 登记;官方 agent 只登记 `id/aid/hub/caps`;运维经 hub 主机之外的独立工具 |
| admin 其余读取方 | `AllAgents`、`RecentTasks`、`Totals` 中的 `completed_task` 与 `delivered_at IS NULL` 改为行存在计数或删除 [C38] |
| 评价 | `UploadReviewRequest` 删 `RequestDoc/Deliverable`;验证传 nil;表列删除(迁移重建 + VACUUM);`content_binding: "UNVERIFIED"`;评语 ≤ 280 字符;web UI 删"内容已验证" |
| `completed_task` | 删除;`/stats.tasks_completed` 改为"已公开的有效回执数",文档写明含义变化 |
| 访客模式 | 删除 hub `guest.go`、路由、`guest_quota` 列(含 `recovery.go` 的 `RestoreDeletedAgent`、`FullAgentRow`、`RestoreAgentsFromBackup` 列清单,并测试从旧 schema 备份恢复)[C38]、`AgentView.GuestQuota`、admin `/quota`、web `ChatDialog`/`lib/guest.ts`、首页文案;daemon 删 `--guest-messages`/`GuestMessages`、`scripts/4-guest.sh` |
| taskboard | 加法 tag `taskboard`(daemon 与 hub);web `TasksSection` 在 hub 未编入时隐藏 |
| 静态护栏 | 测试断言 `aghub`、`admin` 不 import `ANetCore/delegation`、`ANetCore/tsir` |

生产数据清理清单(脚本随版本提供,执行前征求同意):emax、fmax 的 `relay_message` 明文行与周备份;`admin/datasets/<全部源>`;`admin.db` 的 `session` 全部行与 `harvest_state`;`data/taskboard.db`(含 `-wal`、`-shm`);已公开评价中的 goal/deliverable;以上在备份中的副本;ai-studio 在 emax/fmax 上的 manifest(删除 `datasets/monitor/ops` 或整体删除)。已经通过 `/fed/v1/reviews` 流出的内容无法收回。

去除内容后 hub 仍然可见的元数据写入对外文档:agent 自述(卡片、KEL、keys、profile;其中 KEL 与 keys 对所有对等 hub 按精确 AID 可查,与 hub-local 可见性无关,见 §3.9;可见性文档同步写明)、评价关系图、p2p 地址、活跃度、付款元数据、公开发放链(§21)。

---

## 10. A2A 卡片与注册表

### 10.1 网络卡片

- 只有至少一个公开 skill 的节点发布网络卡片。
- `supportedInterfaces`:中继绑定 `{url: <hub 中继端点>, protocolBinding: "https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1", protocolVersion: "1.0", tenant: <AID>}`;p2p 启用时追加直连条目。127.0.0.1 不进入网络卡片。
- `securitySchemes` 省略(认证由绑定内的发送方签名承担);不定义新 scheme 类型。
- `capabilities.extensions`:`…/anet-card/v1`(`params{aid, seq:"<字符串>", issuedAt, notBefore}`)、a2a-x402 v0.2、`…/anet-pricing/v1`(`params{network, prices:[{skillId, amount:"<字符串>"}]}`)、`…/anet-evidence/v1`。
- 数值一律字符串;必填切片非 nil;`streaming`、`pushNotifications` 显式输出。

### 10.2 skills 来源

- C1 可选接口 `provider.Described{ SkillInfo(capability) (SkillInfo, bool) }`,未实现时由 id 派生 name 与 tags。
- `service` 模块配置每个能力的 `name/description/tags/examples` 并实现 `Described`。
- 只发布 `public_capabilities` 中的能力。

### 10.3 签名(ANetCore `a2acard`,标准库实现)

- JWS EdDSA,签名钥为 KEL 当前钥;保护头 `{"alg":"EdDSA","jku":"https://<hub>/agents/<AID>/jwks.json","kid":"did:anet:<AID>#<seq>","typ":"JOSE"}`。`jku`/JWKS 是 hub 的陈述,可信度低于 KEL,文档写明 [m]。
- 规范化 RFC 8785(含 ECMAScript 数字序列化),对去掉 `signatures` 的卡片计算;存储与转发用原字节。
- 验证:`signatures` 为空即拒绝;kid 解析;KEL 回放;只接受顶端活跃密钥态;`params.seq` 三分支高水位(相等时规范化载荷须相同);`notBefore ≤ now + 300s`;尺寸与必填项(整卡 ≤ 64 KiB,name ≤ 128,description ≤ 4096,skills ≤ 256,每 skill tags ≤ 16)。
- KEL 解析器(实现 `a2acrypto.KeyResolver`)放在 `module/a2a/kelresolver`,文件带 `//go:build !no_a2a`,只供 `module/a2a` 与契约测试导入;daemon 内核的卡片验证只用 ANetCore `a2acard`。按 kid 取 KEL、回放、核对 AID 后返回顶端公钥 [m]。
- 契约测试:a2a-go 验证 `a2acard` 签出的卡片;a2a-go 解析—再序列化后签名仍有效;a2a-go 自带的 Ed25519 金标向量由 `a2acard` 验证。

### 10.4 模块向卡片贡献内容

```go
type CardContributor interface {
    CardExtensions() []map[string]any   // 内核不 import a2a-go
    CardInterfaces() []map[string]any
}
```

x402 模块贡献 a2a-x402 与 anet-pricing;p2p 贡献直连接口。`no_x402` 构建卡片中无 x402 URI。

### 10.5 hub 注册表 API

| 端点 | 行为 |
|---|---|
| `GET /a2a/v1/agents?skill=&tag=&q=&cursor=&limit=` | 只返回 `Browsable` 且卡片验证 OK 的条目;条目 `{aid, card(原字节), cardVerification, verifiedAt, homeHub, lastSeen, quiet, reviewCount, avgRating}`;包装层是 hub 陈述 |
| `GET /a2a/v1/agents/{aid}/card` | 原字节;`ETag`、`Cache-Control: max-age=300`、`If-None-Match` |
| `GET /agents/{aid}/jwks.json` | 由 KEL 推导,只含活跃密钥态 |

索引 `agent_skill`、`agent_tag` 只在卡片准入成功后重建;现有 `/agents` 形状不变,有 A2A 卡的条目的 name/caps 从已验证卡片派生。

daemon 的 `list_agents` 自由文本查询:daemon 按 skill/tag 从 hub 取已验证卡片后在本地做子串匹配,不把自由文本发给 hub;hub 的 `q` 参数只供 web UI [m]。

### 10.6 联邦 v2 卡片

`GET /fed/v2/cards` 条目 `{format:"a2a-card/1", card, kel, keys, home, fed_seq}`;按 format 分派准入;KEL 延伸规则适用;`home` 与卡片接口 URL 不一致时以卡片为准并记录。v1 端点保留到所有对端升级。

---

## 11. 本机 A2A 接口(`module/a2a`,`//go:build !no_a2a`)

### 11.1 形态

- 配置块缺省时模块照常启用(本机接口是默认产品面);`anet init` 不写 `modules.a2a` 块(否则 `no_a2a` 变体加载配置会失败)[C43]。
- 端口稳定:首次启动从固定基址扫描(同 `AllocControlPort` 的规则,跳过其他身份占用的端口),选定后写入数据目录状态文件 `a2a_addr.txt`;之后每次启动重绑该端口,冲突时按 `listenControl` 规则重新分配并记日志(已写入的 Hermes 配置随之失效,见 §13.1 doctor)。非回环地址拒绝。
- `module.Host` 增加 `StateDir(module string) string`(模块自有状态目录)与 `TaskSeam() (TaskSeam, bool)`,理由写在接口注释。
- 依赖 a2a-go 的 `a2a`、`a2asrv`、`a2aext`、`a2acrypto`;不导入 `a2agrpc`、`a2acompat`。

```go
type TaskSeam interface {
    Send(ctx context.Context, peerAID string, req TaskSend) (Task, error)
    Get(ctx context.Context, peerAID, taskID string, historyLen *int) (Task, error)
    List(ctx context.Context, peerAID string, f TaskFilter) (TaskPage, error) // 含 pageSize、totalSize、nextPageToken
    Cancel(ctx context.Context, peerAID, taskID string) (Task, error)
    Watch(ctx context.Context, peerAID, taskID string) (Task, <-chan TaskEvent, error) // 快照与订阅原子获得
    Agents(ctx context.Context, q AgentQuery) ([]RemoteAgent, error)
    Card(ctx context.Context, aid string) (RemoteAgent, error)
}
```

`TaskFilter{ContextID string; State string; PageSize int /* 1–100,缺省 50 */; PageToken string; HistoryLen *int; UpdatedAfter *time.Time; IncludeArtifacts bool}`;`TaskPage{Tasks, TotalSize, PageSize, NextPageToken /* 无下一页为空串 */}`。付款经 `TaskSeam` 另有 `Pay(ctx, peerAID, taskID string, decision PayDecision) (Task, error)`,purpose 固定为 `task-agent`。

- 所有带 `peerAID` 的操作只作用于 `role=outbound` 且 `peer_aid == peerAID` 的交互,不匹配一律 `TaskNotFound`,比较在存在性检查之前完成 [C17]。`SendMessage` 只带 contextId 时校验其属于本端点的出站交互,属于他人即拒绝,不存在则视为新 context。
- `Task`、`TaskEvent` 等内核类型即 `internal/a2ashape` 的 A2A JSON 投影(不导入 a2a-go);模块以 JSON 往返转换为 a2a-go 类型,往返测试钉住。
- 事件来自 daemon 进程内事件总线(按 ix 订阅)。

### 11.2 路由

| 路由 | 说明 |
|---|---|
| `GET /a2a/v1/agents` | 已知远端 agent 列表(含代理卡片 URL) |
| `GET /a2a/v1/agents/{aid}/.well-known/agent-card.json` | 代理卡片(需 Bearer) |
| `POST /a2a/v1/agents/{aid}/jsonrpc` | JSON-RPC 绑定 |
| `/a2a/v1/agents/{aid}/rest/...` | HTTP+JSON 绑定 |

入站任务与待批项不经本机 A2A 接口暴露;provider 侧由 MCP `reply_task`、CLI 或 §11.6 后端处理。

### 11.3 代理卡片

- 由远端网络卡片(本地验证通过后)生成:复制 name/description/skills/defaultModes;`supportedInterfaces` 指向本机两个绑定 URL;`securitySchemes = {"anetLocal": {"httpAuthSecurityScheme": {"scheme": "Bearer"}}}`,且 `securityRequirements: [{"schemes": {"anetLocal": {}}}]` [C18]。
- `capabilities`:`streaming: true`(描述本机接口,不照抄远端);`pushNotifications: false`;远端声明 x402 或 anet-pricing 时同样声明(x402 `required` 强制 false,§8.7);`anet-origin/v1` params `{originCard, originVerification}` [C19]。
- 远端无网络卡片时以 AID 生成,`originVerification: "UNVERIFIED"`,skills 放一个 `chat` 占位,description 如实说明。
- 由本机 daemon 的密钥签名,不带 `jku`。

### 11.4 中间件

Host 白名单;Bearer(`a2a_token.txt`,常数时间比较);拒绝带非空 `Origin` 的请求;JSON-RPC 只接受 `application/json`;请求体上限 96 MiB;`A2A-Extensions` 与 `X-A2A-Extensions` 按逗号拆分合并后改写请求头;`A2A-Version` 缺省按 1.0,显式非 1.x 返回 `VersionNotSupportedError`(也读查询参数);响应回显"请求的 ∩ 该 agent 支持的"扩展;包装的 `ResponseWriter` 实现 `Flusher`。

### 11.5 操作映射

| A2A 操作 | 实现 |
|---|---|
| SendMessage | 无 taskId → 新建(拒绝客户端指定新任务 id;按 `(contextId, 客户端 messageId)` 去重,重复返回已有任务;客户端 messageId 存入 `message.metadata["a2a.messageId"]`,不作为信封 mid;Hermes 每次调用生成新 messageId,此去重对其超时重试无效);有 taskId → 追加消息。`metadata["anet.skill"]` 或 DataPart `{skill, args}` 表示能力调用。`return_immediately=false` 时等待到终态或中断态,不提前返回 [C22] |
| SendStreamingMessage / SubscribeToTask | `Watch` → SSE;终态前先发 `anet.reply` 的 artifact 更新事件 |
| GetTask / ListTasks | interactions;ListTasks 按 `state_at` 降序,支持 `contextId`、`status`、`pageSize`、`pageToken`、`historyLength`、`statusTimestampAfter`、`includeArtifacts`;`includeArtifacts` 为 false 或缺省时省略 artifacts |
| CancelTask | §4.2 |
| 推送 4 个操作 | `PushNotificationNotSupported` |
| GetExtendedAgentCard | 不支持 |

输入 Part 规则 [C44]:text part 拼接为目标;raw FilePart 经 `attachmentFromBytes` 成为附件(64 MiB 上限、CID、文件名与类型带过);任何 scheme 的 url part(`file:` `http(s):` `data:`)一律 `InvalidParams`,daemon 不抓取、不读本地路径。

Task 表示 [C21]:
- `artifacts`:完成的文本任务首位是 TextPart artifact `anet.reply`(回执覆盖的对话记录中 provider 的最后一条);能力任务首位是交付物 DataPart;其后是 `anet.receipt`(DataPart)与附件(FilePart,文件名经 `safeName`)。
- `status.message`:付款导致的 `input-required` 带 x402 `payment-required` 消息;文本任务的 `input-required` 带 provider 最近一条消息;`working` 可带进度;失败/拒绝带原因。
- `history`:消息表,不含控制行;requester=user、provider=agent。
- `metadata`:`anet.effect_status`(仅能力任务)、`anet.receipt_verified`(`verified`/`unverified`/`unknown`)、`anet.request_cid`、`anet.result_cid`、`anet.peer_aid`、`anet.reason`、`anet.retry_after_ms`、`anet.cancel_requested`、x402 键。

阻塞调用与客户端超时:客户端超时不会取消任务,任务继续运行(auto 档内可能已付款),客户端重试会建第二个任务;找回方式:按 contextId 的 ListTasks,或 MCP `list_tasks` 的 `context_id` 过滤。文档与 §21 写明。

### 11.6 提供侧后端(本期末尾,可选)

`modules.a2a.backends: [{"match": "*"|"<cap>", "url": "http://127.0.0.1:9900", "token_file": "…", "accept_untrusted": false, "toolless": false}]`[C9]:
- 只转发 `trust_file` 中对端的已接受文本任务;其余留在收件箱。
- `accept_untrusted: true` 仅当运营者同时声明 `toolless: true` 才接受,doctor 给出警告;经 `DeclareUntrustedBackend()` 计入 `open` 策略的配置校验(§5.1)。
- 转发的 metadata 带 `anet.peer_aid`、`anet.trusted`、`a2a.serviceParameters` 还原为请求头;文档写明后端因共用一个令牌而无法自行区分对端。
- 证据 `anet.backend.forwarded{backend, interaction_id, peer_aid, trusted}`。

---

## 12. 控制面任务路由与 MCP

控制面新增任务路由(返回 `internal/a2ashape` 投影)[C20]:`POST /tasks/send`、`/tasks/get`、`/tasks/list`、`/tasks/cancel`、`/tasks/wait`、`/tasks/pay`(`{task_id, decision: submit|reject, accept?}`,purpose=`task-agent`)、`/tasks/pay-manual`(purpose=`task-manual`,只由 `anet pay` 调用),provider 侧另有 `/tasks/reply`。与 `TaskSeam` 共用实现,但不按 peerAID 限定(控制令牌是全权凭据)。

| MCP 工具 | 对应 | 取代 | 注解 |
|---|---|---|---|
| `list_agents` | 发现 | `agents_find` | readOnly |
| `get_agent_card` | 卡片 | 新 | readOnly |
| `send_message` | `/tasks/send` | `task_delegate` + `task_message` | openWorld |
| `get_task` | `/tasks/get` | `task_results` 单条 | readOnly |
| `list_tasks` | `/tasks/list`(含 `context_id`、`role`、`state` 过滤) | `task_results` + `task_inbox` | readOnly |
| `wait_task` | `/tasks/wait` | 新 | readOnly |
| `cancel_task` | `/tasks/cancel` | 新 | — |
| `reply_task` | `/tasks/reply` | provider 的 `task_message`/`task_end` | openWorld;始终注册,无可回复任务时返回明确错误 [m] |
| `submit_payment` / `reject_payment` | `/tasks/pay`(§8.6 agent 档) | `task_delegate.pay` | destructive / — |
| `get_balance` | — | `credit_balance` | readOnly |
| `audit` | — | `evidence_read` | readOnly |
| `node_status` | — | 保留,描述与实现对齐 | readOnly |
| `inbound_pending` | — | 新,只返回元数据 | readOnly |

mcpserv 原样转发控制面的投影 JSON;描述写明"completed 且 effect_status=UNVERIFIED 不等于成功";`ServerOptions.Instructions` 下发短说明。

---

## 13. 安装与生态接入

### 13.1 命令

- `anet init`:幂等写出显式安全默认值(SI-5 的全部键),创建空的 `peers.*`、`payees.allow`;已有配置只补缺省键并报告差异。
- `anet doctor [--json]`:版本与签名、模块、身份、控制与 A2A 地址、hub 注册、入站策略、支出上限、各编码工具接入与握手、Hermes 配置文件权限、令牌是否过期、`a2a_agents` 各 URL 的端口是否等于 `a2a_addr.txt`(不一致时提示 `anet agents wire --refresh`)、sandbox 模式是否配置了 `auto_reply.api_key`。
- `anet agents wire|unwire [--all|<tool>…] [--refresh]`(`internal/agentwire`,`//go:build !no_mcp`):
  - Claude Code:`claude mcp add -s user anet -- <abs>/anet mcp`;技能写 `~/.claude/skills/anet/SKILL.md`。
  - Codex:`~/.codex/config.toml` 托管块;非托管同名表报告冲突并停止。
  - Cursor:`~/.cursor/mcp.json`;opencode:`~/.config/opencode/opencode.json`;Hermes:`~/.hermes/config.yaml` 托管块 `mcp_servers.anet`(不用 `sh -c`)。
  - Hermes `a2a_agents`:不默认写。`--a2a <aid>…` 时为每个指定远端 agent 写一条 `{url: http://<a2a_addr>/a2a/v1/agents/<aid>, auth:{type: bearer, token}, timeout: 3600}`,文件保持 0600;`--refresh` 在端口或令牌变化后重写;`unwire` 删除令牌条目 [C18][C22][C43]。
  - 写前备份,写绝对路径与 `ANET_DATA_DIR`;fake HOME 往返测试;各工具配置位置以当前版本文档或实测为准。
- persona/SKILL 文本重写:删除过时表述;说明默认拒绝、允许名单、信任名单、支出档位;长任务用 `send_message` + `wait_task`。安装后该文本是本机权威操作说明,agent 不需要重读 hub 的 llms.txt [C40]。

### 13.2 发布签名与 install.sh [C40]

- `release.json`:版本、提交、发布时间、失效时间、各资产 `.gz` 与原始 sha256、各变体模块集合、`next_key_fingerprint`。以 `ssh-keygen -Y sign -n anet-release@agentnetwork.org.cn` 签名;`install.sh` 本身也签名(`install.sh.sig`)。
- 公钥与指纹发布在 GitHub(`SECURITY.md`、README)、官网文档与 install.sh 内;emax `/dl` 是镜像。
- 手动核验路径写入文档:下载 `install.sh` 与 `.sig` → `ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh` → 执行。
- install.sh:逻辑在 `main()`;只用 https;验签失败、过期、降级、sha256 不符、模块集合不符任一即退出且不触碰已装版本;安装后 `anet init`;`--agents` 时 `anet agents wire`;打印 doctor 状态块与一个免费官方 agent 示例。
- hub `llms.txt` Step 0:已安装时执行 `anet update`;新机器才用 curl|sh 并给出手动核验路径(`ANetHub/internal/aghub/web/llms.txt` 列入改动)。
- `build-release.sh`:生成并签名清单;`.gz` 哈希;`BuiltAt` 取提交时间;严格脏树检查;全平台符号自检。`anet update`:`crypto/ed25519` 验清单后原子替换。
- 签名私钥:本期在 ink93 生成开发用钥,正式发布前由产品负责人决定保管方式。

---

## 14. 审计

- 新证据事件:`anet.delegation.received`(`open` 策略下聚合)、`anet.delegation.refused_summary`、`anet.policy.changed`、`anet.autoreply.invoked`、`anet.backend.forwarded`、`anet.payment.quoted`、`anet.message.sent/received`(只记 CID 与字节数)。
- `anet audit [--since|--peer|--interaction|--json]`、`--export DIR`、`anet verify --chain DIR`;`anet audit hub` 为 `audit-hub` 别名。规则:`receipt_verified=false` 显示"未能核验";UNVERIFIED 不计入成功;未知事件原样列出;每段标明来源。

---

## 15. 官方公共 agent

- 后端 `ANet/cmd/anet-official`,独立二进制,每类能力一组 127.0.0.1 路由,daemon 用 `service` 模块挂载;后端以每后端令牌头认证 daemon。
- 第一批能力:`net.echo`;`text.stats` `text.digest` `text.diff` `json.validate` `a2a.card.validate` `a2a.x402.check`;`docs.search` `docs.get`(构建时打包语料并计算 CID);`demo.digest.paid`。全部确定性、纯计算、不执行命令、不访问外网、不接受 URL。
- 身份:A1 `anet-echo-e`(emax)、A2 `anet-echo-f`(fmax)、B `anet-tools`、C `anet-docs`、E `anet-paid-demo`;`inbound.policy=closed` + `public_capabilities`。
- 官方身份由客户端验证:发布签名密钥签署官方清单,随二进制打包;`list_agents` 与代理卡片据此标注 `anet.official: true`。hub admin 只登记 `id/aid/hub/caps`,不登记 runtime/ops/monitor/harvest;运维经 dmax 上的专用非 root 账户与独立工具 [C39]。
- `service` 模块把已验证调用方与 ix 以 `X-ANet-Caller`、`X-ANet-Call` 传给后端;按能力覆盖超时。公共能力的证据可配置为只记 `result_cid` 与指标(C5 契约文档写明两种模式)。
- 部署属于生产变更,执行前征求同意。

---

## 16. 可插拔编译与 CI

| 项 | 方向 | 符号模式 |
|---|---|---|
| `no_a2a` | 减法 | 特例 `module/a2a\|a2aproject/a2a-go`(派生模式不匹配 SDK) |
| `taskboard`(原 `no_taskboard`) | 加法 | daemon `module/taskboard`;hub `internal/taskboard` |
| `no_mcp` | 减法 | `internal/mcpserv\|internal/agentwire` |

- 单 tag `no_a2a` 行保留 MCP;另加 `go list -deps ./internal/mcpserv ./internal/daemon | grep a2aproject` 为空的检查。
- CI 修正:组合行逐个检查全部模块;`build.sh --check` 增加减法方向;ANetHub CI 增加 `go test -tags` 与符号数双向检查;`joint.sh`、`joint-shell.sh` 进 CI。
- ANet CI 固定 ANetHub 的 checkout ref,与第一个 wire-2 变更同批推送 [C37]。
- `CLAUDE.md` tag 列表同步(`no_a2a` 加入减法,`taskboard` 移到加法)。

---

## 17. 测试计划

| 层 | 内容 |
|---|---|
| ANetCore | `seal`:金标在解封侧——以 `DeriveKeyPair(ikm)` 固定收件人私钥,钉住整份信封字节,验证 Open + 验签结果;HPKE info 与签名原像单独钉字节;RFC 9180 向量只跑 `NewRecipient`/`Open`;封装侧用往返与变异测试 [m]。逐字段变异(`to type ix mid ts exp enc kid suite body kel keys`)全部拒收;0–63 未知键拒收。`VerifyEncKeySet` 的预期 AID 替换测试。`ExtendsKEL` 回退/分叉。`Replay` 的 `SupersededAt` 金标。`relayauth` v2 钉字节。`a2acard`:RFC 8785 附录向量、金标卡片、a2a-go 向量。`delegation` 新向量 |
| 契约 | hubapi / wirecontract:relay v2、keys、`/fed/v2/keys`、评价无内容字段、注册表、`paymentRequirements` 在 daemon→hub 与 hub→hub 两段请求体中;扩展 URI 与 metadata 键两侧钉字符串;控制面投影反序列化为 a2a-go `a2a.Task`;Hermes `_reply_text_from_result` 移植版能从阻塞 SendMessage 结果取到答复文本 |
| fake 完整性 | daemon 测试的 hub fake 只按 `to_aid` 存取不透明信封,实现 relay v2 全部拒绝路径与 keys/fed keys 端点;fake peer 按帧 ID 关联 ack |
| daemon 单测 | §3.6 每一步的失败分类;hub 以他人 KEL+keys 冒充收件人(mutation:去掉预期 AID);600 个新 AID 后截断 KEL 被拒;重启后仍被拒;存储失败后重投只处理一次;p2p 与 hub 并发只处理一次;入站六条分支;`public_cap` 交互拒收文本;配额;准入接缝覆盖兑付口;沙箱失败闭合两方向;控制面 Host/票据/CSRF/白名单;`/attachment` SVG;`/pull` 覆盖与符号链接;支出三档与 AdmitSpend 各调用面;商户核对;结算未知与 replayed;取消与付款竞态;状态迁移原子性;A2A 作用域(跨 AID、入站任务、他人 contextId);url part 拒收 |
| 现有脚本改动(C 阶段随内核改动同批)[m] | `joint.sh`、`joint-fleet.sh`、`scenario.sh`、`onboard.sh`、`lib.sh`:直接写 `peers.allow`/`peers.trust` 文件(CLI 的 `anet peers allow` 要求 TTY,脚本不走 CLI);能力 provider 写 `public_capabilities`;MCP 探针改新工具名;`/end-accept` 调用方与控制台结束 UI 更新;删除 `4-guest.sh`;prodtest 9c 删除、9f 改走 dmax 控制面、9n 改为统计 `delivered`;`joint-shell.sh` 与 `container-shell-test.sh` 改允许名单 |
| 联调 | `joint.sh`:SI-1 canary(含 admin 与官方 agent)+ 陌生节点被拒 + 允许名单路径 + hub 伪造注入被丢弃 + p2p 并发;`scenario.sh`:两 hub 跨 hub 加密委派(含 hub-local 可见性的只允许名单 provider,经 `/fed/v2/keys`)、provider 重启后超过缓存时限回复、跨 hub 付费(含少付与错收款方负面用例、正确付款仍只产生一份 credit);`joint-fleet.sh`:新完成语义;`joint-a2a.sh`:未修改的 a2a-go 客户端(`AuthInterceptor` + `CredentialsService`,卡片请求带 Bearer)经本机接口 → hub → 对端,覆盖 SendMessage/流式/GetTask/ListTasks/Cancel、x402 同任务流(夹具构造 §8.7 的付款消息;超上限、选项不在 accepts、外来 payload 三个负面用例)、缺 `securityRequirements` 的卡片得 401、daemon 重启后同一客户端配置仍可用;a2a-tck 结果记录(不作门禁);`joint-official.sh`:五层防护 |
| 实网 | `prodtest.sh` 在双 hub 上重跑(部署后,需同意) |
| 安全对抗评审 | 实现完成后按 SI 逐条尝试反驳 |

补充用例(r3;每条 mutation 验证):

| 来源 | 用例 |
|---|---|
| C16/C3 | 高水位三分支:TTL 过期后重取未变 keyset 成功;首次联系连续三条消息全部送达;同一卡片重复发现成功;同 seq 不同载荷判为分叉并计数;新 keyset 之后到达的旧 keyset 消息照常处理;内层 KEL 短于已存且 ts 早于轮换时接受,分叉拒收(mutation:`==` 分支改为拒绝)。hub:`POST /agents/{aid}/keys` 与 `/register` 以相同 keyset 重发返回 200,同 seq 不同字节 409;daemon 重启后以相同 keyset 重新注册不失败 |
| C1 | 在第 9 步与第 10 步提交之间杀掉 daemon 后重启:短能力调用恰好处理一次;长能力调用置 failed/interrupted 并送达请求方 |
| C12 | 600 个新 AID 委派、以及 600 个新 AID 调用公开能力之后,允许名单对端的截断 KEL 均被拒 |
| m | ix 碰撞:B 以 A 的出站 ix 发 delegate(能力与文本各一例),A 不执行、不改变该交互的 role/state/result |
| C19 | 未知 ix 的 message/cancel 在 10 分钟窗口内不 ack,delegate 随后到达时正常处理;超窗回 TaskNotFound |
| C6/C29 | 长能力调用中收到 `end_request`:不签对话记录回执,结果送达;`public_cap` 交互上带 text part 的 `payment-submitted` 被接受并结算,正文不存;双方开启 exec 自动回复走完报价→付款→完成,后端调用 0 次;报价后 end_request → canceled 且无回执 |
| C8/C9 | `open` 与"对非信任对端启用 exec/后端"按两种顺序在运行时写入均得 409;allow 但不在 trust 中的对端、`open` 下的陌生人都不到达 fake 后端 |
| C22 | 客户端给出的 contextId 原样保存并可由 ListTasks 找回 |
| C25 | 结算未知三例:hub 在 daemon 超时后提交(恰好一次扣款且执行)、provider 在结算成功与写入之间崩溃(重启后执行)、provider 结算后回 payment-failed(requester 与 hub 均拒绝第二份授权) |
| C33 | receipt 已存、结果未发出时崩溃,重投后只重发一次结果 |
| C34 | 取消三种顺序(payment-submitted 后结算前、结算后结果前、本地取消后结果到达),均断言付款方证据链含该笔结算(daemon 单测 + `scenario.sh`) |
| C35 | 阻塞式追问在 provider 再次回复前不返回;自动回复跳过 canceled/rejected;取消后 SetResult 不改变状态;对 failed 任务 SendMessage 返回 UnsupportedOperation |
| C27 | AdmitSpend 各调用面超上限被拒且不写 `anet.payment.authorized`;并发两笔不同时通过;auto 档多笔拆分受 `agent_daily_max` 约束 |
| m | 对端移入 deny 后其活动交互置 canceled、后续消息丢弃、自动回复不再调用 |
| C42 | `/pull` 前导点文件名被中和;同一交互二次 pull 不写文件;空 out_dir 返回 400;指向数据目录的符号链接 out_dir 被拒 |
| C2 | `scenario.sh`:approve 队列项在超过缓存时限后批准,回复仍送达跨 hub 纯请求方 |
| C32 | `scenario.sh`:关闭 `/fed/v2/keys` 查询后,hub-local provider 与超时限回复两例均失败(mutation) |
| m | p2p:无 `V` 的旧帧得到 error,daemon `Receive` 未被调用 |
| m | errorReason → `x402.payment.error` 映射表两侧钉字符串 |

---

## 18. 迁移与版本

- 破坏性升级:ANet v0.2.0 与 ANetHub wire 2 同时部署。旧 daemon 连新 hub 得到 426;新 daemon 连旧 hub 拒绝工作。
- ANetCore v0.15.0:新增 `seal`、`a2acard`、`identity.ExtendsKEL`、`Replay` 修正、`relayauth` v2、`delegation` 增量;不新增依赖;`docs/scope.md` 登记 `crypto/hpke`(标准库)与 `a2acard` 的归属理由。
- 开发期:专用 `GOWORK=/data/projs/anet-oss/.anet-work/go.work`(只含三仓,不放在共享目录根,不含 ANetLink/ANetMock)[C37]。a2a-go 在 ANet 中用 `go get github.com/a2aproject/a2a-go/v2@v2.6.0` 写入 go.mod/go.sum;`go mod tidy` 延后到 Core 打 tag 之后。
- CI 策略:发布前不向 ANet、ANetHub 的 main 推送;ANetCore 打 tag 后,两仓的 go.mod 升级、hub ref 固定与全部改动同批推送(推送前征求同意)。本地以 `GOWORK=off` + 临时 replace 复核单仓构建。
- 下游消费者(ai-studio anetbridge、Research-Galaxy agent-runtime、ANetOS-Web hubkeeper)本期只列影响清单。
- p2p 路径上,新 anetpeer 拒收无版本帧并回错误(§3.10)。
- `modules.x402.voucher_url` 的主机不是回环地址时只接受 https;违反时 daemon 启动失败,错误信息说明兑付口需由运营者前置 TLS 终端。`docs/GUIDE-zh.md:230` 与 `docs/site/guide.html:177` 的 http 示例改为 https。升级 dmax 前检查其 `voucher_url`:为 http 时改为经 TLS 终端的 https 地址或删除该键(生产变更,执行前征求同意)。

---

## 19. 对 A2A 社区的贡献(草稿,`ANet/docs/a2a/`)

1. 中继绑定规范草案:tenant=AID 路由、E2E 信封、发送方签名认证、store-and-forward 下的流式语义;写明经中继的操作(SendMessage、CancelTask)、由请求方 daemon 以本地状态回答的操作(GetTask、ListTasks、SubscribeToTask)、不支持的操作(推送 4 个、GetExtendedAgentCard);服务参数经 `a2a.serviceParameters` metadata 携带。
2. 注册表 API 草案。
3. `anet-credit` x402 scheme 文档(托管性质、payload、签名、nonce 与窗口、`hub:<aid>` 与 CAIP-2 的偏离;本地签名服务接受未签名的所选项这一偏离)。
4. a2a-go issue 草稿;5. a2a-x402 issue 草稿;6. `SecurityScheme` 新变体提议。

前置条件:贡献部分(规范文本与参考实现)的许可证由产品负责人决定;对外提交前征求同意。

---

## 20. 实施阶段

| 阶段 | 内容 | 依赖 |
|---|---|---|
| A | ANetCore:`seal`、`a2acard`、`ExtendsKEL`、`Replay` 修正、`relayauth` v2、`delegation` 增量与新向量 | — |
| B | ANetHub:relay v2 + 认证发送 + 限额 + keys + `/fed/v2/keys` + KEL 延伸 + 联邦转发;内容移除(访客、全部采集、评价、completed_task、admin 官方 ops、恢复与留存脚本、webui);facilitator 拆分与核对;账本读取鉴权;注册表 + JWKS + fed v2;taskboard 加法;llms.txt;CI | A |
| C | ANet 内核:密钥环、封装/解封与接收流水线、`peer_identity`、传输接口、p2p/anetpeer;任务模型与状态机、完成/取消、结果重试;入站策略、准入接缝、待批;自动回复加固与沙箱;控制面与 console.html 改造、`/attachment`、`/pull`;支付同任务流、AdmitSpend、支出三档;`a2ashape` 与控制面任务路由;评价无内容;nonce 与对话记录 v2;证据事件;A2A 网络卡片;现有脚本与测试同批改动 | A |
| D | ANet 北向:`module/a2a`、`TaskSeam`、事件总线、代理卡片;MCP 重组;`agentwire`、`init`、`doctor`、`audit`、`update`、`pay`、`peers`、`inbound`;install.sh 与发布签名 | C |
| E | 官方 agent 后端与配置;文档与对外陈述更正表中"E2E 部署后"各行;贡献草稿;新联调脚本;CI 矩阵;`CLAUDE.md` tag 列表 | C、D |
| F | 全量验证:gofmt/vet/test/race、tag 矩阵、全部联调、a2a-tck;安全对抗评审与修复 | 全部 |
| G | 需征求同意:推送与发布、部署两台 hub 与官方 agent、生产数据清理、对外提交 | F |

B 与 C 在 A 完成后并行;同一仓库内按文件归属串行推进。

对外陈述更正表(E2E 部署前不出现"hub 看不到内容"的表述):

| 位置 | 现有表述 | 改后表述 | 时机 |
|---|---|---|---|
| `ANet/README.md:192` | "The Hub relays bytes and keeps an index — it is *not* a trusted party." | 立即:"In v0.1 the hub relays task contracts, chat messages and results unencrypted and can read them; signatures let either party detect forgery but do not stop the hub from reading. End-to-end encryption is planned for v0.2." E2E 部署后:"The hub relays encrypted envelopes and cannot read task content; it still sees sender, recipient, time and size (see Known limitations)." | 立即单独改;E2E 部署后再改 |
| `ANet/docs/ARCHITECTURE-zh.md:313` | "Hub 只搬运不透明字节" | 立即:"Hub 搬运的 delegate 与 result 带签名,可检测伪造;v0.1 中这些字节对 hub 可读"。E2E 后:"Hub 搬运 HPKE 密文,可见元数据见已知局限" | 同上 |
| `ARCHITECTURE-zh.md:38` | "普通聊天消息则以明文中继" | "daemon 之间全部消息以 HPKE 封装" | E2E 部署后 |
| `ARCHITECTURE-zh.md:170`、`:316` | `/relay/send` "开放"、"刻意不鉴权" | "relayauth v2 发送方认证,按发送方限流(X1)" | E2E 部署后 |
| 官网、hub web UI、llms.txt、skill.md 中的同类表述 | 阶段 E 检索后补入本表 | — | — |

---

## 21. 已知局限(写入对外文档)

1. hub 在发送时刻知道"谁发给谁"、何时、多大;来源 IP 可见。
2. 前向保密以加密密钥生命周期为界:一条消息在发出后至多 29 天内,可被取得收件人磁盘的一方解开。
3. 大附件整体缓冲、单次 AEAD。
4. 官方公共 agent 是端点,能看到调用内容;其保存策略公开写明。
5. 沙箱内的本地 agent 仍可联网,可外发工作目录内容或自身凭据;宿主回环 TCP 服务与抽象 Unix socket 在沙箱内可达。
6. 对端 KEL 首次信任:第一次看到某 AID 时接受其自证明 KEL;回退防护只覆盖已有持久记录的对端。hub 可以提供旧卡片与截断 KEL,daemon 只拒绝回退到本机已见状态之前。
7. `hub:<aid>` 不满足 CAIP-2。
8. `return_immediately=false` 的 SendMessage 在对端离线时可能等待很久;客户端超时不取消任务,重试会建第二个任务。
9. hub 能按付款方、收款方与时间把结算与公开评价关联;公开发放链显示每笔跨 hub 付款、清算与兑付的金额、时间与 AID。经 hub 网关购买的凭证与报价含能力 id 与收款方,hub 可见。
10. 通过 curl|sh 从 agentnetwork.org.cn 或 hub 域名首次安装时,信任提供脚本的主机(当前与官方 hub 同机);按 hub 的 llms.txt 行事的 agent 执行的是该 hub 提供的指令。安装后 `anet update` 只依赖发布密钥。
11. KEL 轮换没有产品触发路径;轮换宽限默认 1 小时,超过宽限仍在信箱中的旧密钥消息会被拒收。
12. `A2A-Version` 缺省按 1.0 处理,偏离规范的"缺省按 0.3"。
13. TTY 门槛(`anet pay`、`anet peers allow`、`anet inbound approve`、修改支出上限)在 CLI 进程内检查,对应的控制面路由凭控制令牌即可调用。它只约束只能经 MCP 工具或本机 A2A 接口行事的 agent。任何能以本用户身份执行命令的 agent(包括 Claude Code 等工具的 Bash,不论有无 TTY),都可以读取控制令牌直接调用这些路由,或直接改 `peers.*`、`config.json` 并重启 daemon。同 uid 下不存在更强的边界,文档如实写明。
14. 本期不处理(归属与理由):ANetLink `c1.sock` 权限与 `SO_PEERCRED`、按 `caller_aid` 授权(跨仓,ANetLink 单独立项);联邦按卡片 home hub 定向转发(当前按对等表顺序尝试,功能正确);交互级临时密钥;大附件分块;sealed sender;发放链隐私格式;沙箱网络隔离;非 Linux 沙箱。
