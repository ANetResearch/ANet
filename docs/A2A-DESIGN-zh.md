# anet 对齐 A2A:设计文档(v0.2 系列)

状态:r4,2026-09-27。r4 把剩余阶段的已定决定(`docs/notes/0017` Q1–Q27 与 Q24 补充)与实现中记录的偏离(三仓 `integ/round4b` 各合并提交与复核提交的说明,以及其后合入 `integ/round4c` 的 `wp/fix5`)逐节写回正文,使本文描述实现后的真实设计;每处改动在行尾以 `[Qn]` 或 `[impl:<分支或提交>]` 标注来源,已定而尚未落地的写明"(实现待补)"。r3→r4 的逐处清单与仍待产品负责人决定的事项见 `docs/notes/0022-设计r4修订清单.md`。[B6-02] r4 经独立复核对照 `integ/round4c` 代码抽查,修正处标 `[B6-02 复核]`,清单见 `0022` 附。
沿革:r3,2026-09-26。r1 经六视角对抗评审(54 个 agent;确认 45 条、反驳 3 条、次要 23 条),r2 逐条处理;r2 经闭合核对(42 处未闭合或新不一致),r3 逐条处理。评审记录见 scratchpad `review-confirmed.md` / `review-other.md`(已随 `/tmp` 丢失,结论已写入正文,见 `docs/notes/0012`),文中以 `[Cn]` 引用确认项、`[m]` 引用次要项。
范围:ANetCore(→ v0.15.0)、ANet(→ v0.2.0)、ANetHub(→ wire 2)。
依据:勘察报告 01–11(`[R03 §5.1]` 形式引用),A2A 规范 v1.0.1(`Refs/a2a`),a2a-go v2.6.0(`Refs/a2a-go`),a2a-x402 v0.2(`Refs/a2a-x402-spec-v0.2.md`)。

本文只写"要做成什么样"和"为什么这样取舍"。现状与缺陷的逐行证据在勘察报告与评审记录里,不在此重复。r4 起,实现细节以代码与合并提交说明为准,本文只收录影响线协、语义、安全边界或对外陈述的部分。[B6-02]

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
| SI-1 | hub 进程、hub 磁盘(含 WAL、备份、admin 数据目录)、hub 与 hub admin 的任何 HTTP 响应中,不出现任务内容:TaskDoc 正文、聊天正文、交付物、附件字节、能力参数、x402 `resource`。经中继结算路径到达 hub 的付款对象(`/x402/settle` 请求体、结算行、跨 hub 转发体)中 `resource`、`description`、`extra` 为空或固定值(结构化断言)。本条是字节与结构断言;收款方与金额加上公开的逐 skill 价格仍可推出所买 skill,见 §21 第 9 条 [redteam:F1]。hub 网关与凭证路径(§2 冻结项)不在本条范围内 | `joint.sh` canary:hub 与 `anet-hub-admin` 同时运行(采集与快照周期调短),流程含一次对官方 agent 测试实例的调用;对 hub 数据目录、admin 数据目录、`/agents/{aid}`、`/fed/v1/reviews`、admin `/api/sessions*` 做字节搜索,命中 0;断言 admin `/api/official/{id}/insights`、`/acl`、`/monitor/*`、`/ops` 返回 404。mutation:关闭加密、恢复任一采集源,均须命中。反向断言:收件方解出 canary |
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

- `/relay/send` 要求发送方以 relayauth v2(§3.7)认证,发送方必须是本 hub 已注册的 AID。hub 用它做按发送方的限流与配额,**不写入** `relay_message`,hub 进程也不逐条记录中继。hub 前面的反向代理同样不留下"谁发给谁":随仓库下发的 nginx 配置(ANetHub `deploy/nginx-hub.conf`、`.example`)对 hub 虚拟主机关闭访问日志,错误日志只记 `crit`(`error` 级的行带客户端地址与请求行),由主机 logrotate 至多保留 14 天;daemon 在给对端写之前查的密钥、卡片与验卡用的 KEL,请求行都不含对端(§3.5 第 1 步、§3.7 端点表)[redteam:F3]。`/register` 另加按 IP 的限速(AID 可无限生成,否则按发送方限流对轮换 AID 无效)[C15f]。
- 理由:匿名发送下 hub 只能按 IP 限流;发送方身份在 E2E 之后本就可由 IP 与时间关联推出(R03 §4.5)。删除的是**存储中的社交图**。
- 代价(§21):hub 在发送时刻知道"谁发给谁"。sealed sender + 投递令牌列为后续(信封外层保留字段 7)。

### X2 入站默认 → `closed`,并回 `REJECTED`

- 默认 `closed`:允许名单之外的委派拒绝,回签名的 `rejected`,不写 interactions,不存内容。`closed` 下,对非公开能力与自然语言委派,deny 名单中的对端得到与陌生人相同的回复(同一 reason、同一限速桶);deny 对端发往未知 ix 的消息与陌生人走同一分支(10 分钟窗口内按 T 不 ack,超窗回 `TaskNotFound`,同一通知限速)[impl:wp/wire 1591718]。节点配置了 `public_capabilities` 时,被 deny 的对端调用公开能力会被拒而陌生人会被服务,对端可以察觉,文档写明 [C15d]。
- `approve`(待批队列)是可选策略。`open`/`approve` 下被 deny 的对端总能察觉自己被拒,文档写明。
- 拒绝的副作用有界:每对端限速 + daemon 全局令牌桶(不超过 hub 每发送方预算的 10%),桶空即静默丢弃通知;通知不进重试队列、不阻塞结果;拒绝证据按时间窗聚合写一条 `anet.delegation.refused_summary`,逐条细节写本地轮转日志,不上链 [C15]。

### X3 纯请求方节点的加密公钥 → `EncKeySet`,每条消息随信封携带

- 加密公钥是独立签名对象 `EncKeySet`(§3.1),注册到 hub,`POST /agents/keys:lookup`(AID 在请求体)取回,`GET /agents/{aid}/keys` 为旧 daemon 与读者保留 [redteam:F3];hub 对非本地 AID 经联邦 `/fed/v2/keys/{aid}` 按精确 AID 查询(不进目录、不进索引)[C32]。
- **每条**内层消息都携带发送方的 `kel` 与 `keys`(几百字节,Padmé 取整后差异不可见)。接收路径因此不做任何网络请求 [C2]。
- `EncKeySet` 验证必须绑定预期 AID:发送侧预期 = 收件人 `to`,接收侧预期 = `inner.from` [C0]。

### X4 交互 id 在评价与结算中的可见性

- **结算**:授权的 `InteractionID` 字段填 `pay_bind = hex(SHA-256("anet/x402-bind/v1" 0x00 ‖ ix ‖ 0x00 ‖ task_nonce))`,`task_nonce` 取 TaskDoc 中 `anet.nonce` 的原文(base64url,不解码),实现为无 tag 包 `internal/x402a2a` 的 `PayBind`,三条独立计算的金标向量钉住 [impl:wp/x402d 2cc279b]。标价能力的调用必须带 `anet.nonce`,缺失即 `rejected`、`anet.reason=task_nonce_required`,不报价(否则付款无法绑定到这件活)[Q19]。发给 hub 的 `paymentRequirements` 中 `description`、`extra`、`resource` 一律为空或固定值,不带能力 id [m];结算请求体的 `paymentPayload.payload` 只保留 `authorization`,付款方附带的其他键不转发给 hub [impl:wp/x402d fe49f7a]。
- **回执**:格式不变;request CID 与 result CID 的原像各含 16 字节随机数:TaskDoc 的 `Tasks[0].Contexts` 加 `{Key:"anet.nonce", Visibility:"private"}`(`Contexts` 在 TaskDoc 规范原像内);交付物 JSON 加 `nonce`。对话记录交付物改为 v2 对象 `{"v":2,"nonce":"…","messages":[…]}`,所有读取方同时接受 v1 数组与 v2 对象,v2 有金标向量 [m]。
- 如实陈述(§21):hub 不存 ix、不能从 `pay_bind` 反推 ix;但能按付款方、收款方与时间把结算与公开评价关联。
- **回执核验的范围** [redteam:F15]:requester 收到结果时核对回执的签名者、provider、requester、交互 id、request CID(须等于本地存的、自己发出的 TaskDoc 的 CID;ANetCore `delegation.VerifyResultForRequest`)与 result CID(须等于收到的交付物字节的 CID);任一不符即丢弃结果(`result-refused`),不存、不能评价。`anet.receipt_verified=verified` 只表示这些绑定成立。对话记录回执覆盖的是 provider 签名交付的那份对话记录字节:其中 `from=requester` 的条目是 provider 的记录,requester 不拿自己的消息日志逐条比对,也不核对记录里的 `nonce`;`verified` 不表示"requester 说过这些话"。据此,评价锚定的是"收到了这份由 provider 签名的记录",第三方要核对请求方原话,应以请求方自己的消息日志为准。签评价之前(`SubmitReview`)再按本地存的交互核对一次回执的交互 id、requester、provider、request CID 与 result CID,不符即拒签——覆盖本规则之前已存的行;`anet verify <ix>` 同样核对 request CID,离线形式可用 `--request FILE` 按请求字节核对 [redteam:F15 复核]。

### X5 本机 A2A 令牌 → 与控制令牌分离,只作用于"本机作为请求方、且对端等于路径 AID"的任务

- `a2a_token.txt`(0600,在 `<数据目录>/modules/a2a/`,§11.1)只授权本机 A2A 接口;控制面对它一律 401 [impl:c497e4b][impl:wp/loopguard ef748a4]。授权模型:单一本机主体,按远端 agent URL 划分作用域 [C17][C44]。
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
| 本机 A2A 接口形态 | `module/a2a` 为 `module.Module`,新增窄接缝 `TaskSeam`;自行实现 `a2asrv.RequestHandler`,套用 SDK 的 JSON-RPC 与 REST 线协;A2A JSON 投影在无 tag 的内核包 `internal/a2ashape`(不导入 a2a-go),控制面与 MCP 直接输出它 [C20][C36]。代理卡片签名与入站任务交付是 Host 上按类型断言取得的可选接口 `ProxyCardSigner`、`InboundTaskHost`,不扩大 `module.Host`(§11.1)[impl:wp/moda2a 85afc91、6195f26] | 长驻 HTTP、需要事件推送;task id 即 interaction id;保住 `no_a2a` 符号判据 |
| taskboard | 由减法 tag 改为加法 tag `taskboard`(daemon 与 hub) | 公共看板把内容存在 hub 并对匿名公开 |
| 访客模式 | 删除(hub 与 daemon) | hub 是会话端点并持有代签身份 |
| hub admin 采集 | 删除全部采集源(hub-relay 与 ai-studio 两路)与 insights 的 `Recent`;admin 删除官方 agent 的 ops/monitor/runtime 路由,官方 agent 在 admin 中只登记 `id/aid/hub/caps` [C39] | hub 主机不应持有能读取官方 agent 内容的通道 |
| 评价 | 只收回执与评价对象,不收内容;内容绑定如实标 `UNVERIFIED` | 决定 2;诚实状态 |
| CAIP-2 网络名 | 本期不改,`hub:<aid>` 保留,scheme 文档写明偏离 | 牵涉三仓已签名对象 |
| x402 版本 | 对象层 x402 v2,承载在 a2a-x402 v0.2 metadata 键下;a2a-x402 规范示例与官方 python 参考库都是 x402 v1 对象(`maxAmountRequired`),不经 anet daemon 的标准 a2a-x402 客户端读不懂 anet 的 `x402.payment.required`,§19 草稿写明 [impl:简报 04 §2] | anet 已是 v2 对象 |
| facilitator `errorReason` | 保留 x402 层小写常量(已公布取值不变),`module/x402` 内维护 `errorReason → x402.payment.error` 映射表 [m];表覆盖 ANetCore 定义的全部 errorReason(含 hub 另发的 `malformed_payment`、`invalid_payment_requirements` 与旧拼写 `expired`)及 provider 本地核对的原因,见 §8.5 [impl:wp/x402d 2cc279b] | 不混淆两层协议 |
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
- 保留 metadata 键:`a2a.serviceParameters`(`{"A2A-Extensions":[…],"A2A-Version":"1.0"}`,规范 §12.3 的回退方式)、`anet.a2aError`(StatusMsg 中,取 A2A §3.3.2 错误名,如 `TaskNotFoundError`)、`anet.state`、`anet.final`(§4.2 [Q30])、`anet.reason`、`anet.retry_after_ms`、`anet.inbound` [C19][impl:wp/a2ashape 7863633]。首条消息的 metadata 随 `DelegateReq.Metadata` 送达,provider 存储时去掉保留的 `anet.*`/`x402.*` 键 [impl:wp/tasksd ee09872]。
- 消息 id 不改 ANetCore 线协:发送方先铸内层 `mid`,本地 message 行 `msg_id = hex(mid)`,再以该 `mid` 封装(晚封装的出站行用已记的 `outbox.mid`);接收方对 delegate 的首条目标消息与 `StatusMsg` 记 `hex(inner mid)`,对 `ChatMsg` 记正文 `MsgID`(缺省回退 `hex(mid)`)。两侧 id 一致,A2A 投影的 `messageId` 随之一致;待批项同样保存 `msg_id`,批准后首条消息与跟随消息沿用 [Q9][impl:wp/wire be2fdb3、4f0141b]。

### 3.5 发送流程

1. **解析收件人公钥**:持久表 `peer_identity`(§3.8)中有有效 keyset 即用;每 10 分钟尽力向 hub 复核(复核成功的时刻记在 `peer_identity.keys_checked_at`),hub 失败或 404 时继续使用已存,直到无有效键 [C2][impl:wp/scenario 28808b8]。无记录时 `POST {hub}/agents/keys:lookup`(`{aid}` 在请求体,请求行不含收件方;hub 以不带 JSON 错误的 405/404 表示没有该路由时,回退 `GET {hub}/agents/{aid}/keys`)[redteam:F3] → `VerifyEncKeySet(signed, to, kel, now)`,KEL 须与已存延伸(§3.8)→ 写入 `peer_identity`(本节点主动联系对端,属授权上下文)。取不到即以明确错误失败,不降级。
2. 组装 inner(带本节点 `kel` 与 `keys`),签名,填充,HPKE 加密。`exp = ts + 14 天`(等于 hub 未投递 TTL,各 type 相同)。需要重试的外发复用首次信封字节;`now > exp` 时停止重试并写证据事件。
3. 交给传输列表(p2p 优先,hub 兜底)。`module.Transport.Send(ctx, toAID string, envelope []byte) error`;`module.Inbound.Receive(ctx, envelope []byte) error`。
4. 需要重试的外发持久化信封字节,重试不重新封装。r4:除 provider 的结果与状态外,requester 的 delegate(文本与能力)、text、end_request 与付款消息一律经同一出站队列(outbox)可靠投递:任务、首条消息与出站行同一事务写入,本地写入成功即返回 `submitted`,暂时性失败按退避重试 [Q5][impl:wp/wire be2fdb3]。出站队列规则 [impl:wp/wire be2fdb3、4f0141b]:
   - 同一 `(ix, type, 正文摘要)` 再入队返回已有行,不产生第二份;每行都有截止(= 信封 `exp`),下次尝试时间不越过截止:指数退避自 5 秒起、上限 24 小时;hub 给了 `Retry-After` 时按它等,取值限在 1 秒–24 小时 [impl:wp/wire be2fdb3][B6-02 复核];
   - 同一交互发往同一对端的外发按入队顺序投递:前一行仍在队列时后一行等待,投递后一行时先尝试前一行(不论其退避),前一行送出或被放弃后后一行才发 [redteam:F23]。provider 因重投的 delegate 重发答复时,让该任务发往 requester 的整条队列立即到期(不只是结果行),否则结果等在前面状态行的退避之后 [redteam:F23 复核];
   - `/relay/send` 的 400/404/413 与收件人 keys 的 404 判为永久拒绝:删行、写 `anet.delivery.expired{reason}`,不再重试;
   - 一次失败的尝试若可能已经送达(直连传输的失败未标明"未送达任何 daemon",或 hub 请求中途断开、网关 502/504),该行记为"可能已送达";每次尝试在发出任何字节之前先在该行记"尝试中",结果记下时才清除;结果没有记下的尝试(进程在 p2p 等待应答时停止、记录写入失败)同样按"可能已送达"处理,否则重启后取消会把已送达的委派当作未送出而撤回(§4.2)[redteam:F12][redteam:F12/F23 复核];
   - 行被放弃(过期或永久拒绝)时,与"删行"同一事务把出站任务置 `failed`、`anet.reason=undeliverable`(能力任务另带 `anet.effect_status=UNAVAILABLE`;曾有尝试可能已送达的行放弃时不判"未送达",带 `UNVERIFIED`,§4.3 [redteam:F12]),delegate 被放弃时去掉首条消息的客户端 `a2a.messageId`,客户端重试算新的尝试;未送达的 cancel 不改任务;
   - 首次尝试即被永久拒绝时调用方得到错误(A2A 接口与 `/tasks/send` 返回该 failed 任务);取不到或验不过收件人 keys(不是 hub 不可达)时 delegate 直接报错、不写任何行;
   - 刷新循环只读到期行的 id,逐行在锁内重读信封,避免一次载入大量大信封。

### 3.6 接收流程

失败分两类 [C1][C33]:**永久**(P):ack 并丢弃,计数原因;p2p 上 `Receive` 返回 nil。**暂时**(T):任一步的存储读写错误、context 取消、第 0 步限速、第 9 步未知 ix 窗口、直连路径上第 1–4 步的失败 [redteam:F22];不 ack,p2p 上返回错误(daemon 回 nack)让发送方转走 hub;`now > exp` 后暂时性失败转为永久。

| 步 | 内容 | 失败 |
|---|---|---|
| 0 | (仅 p2p)按连接与全局限速,在解密之前 [C15e] | T(不 ack,`Receive` 返回错误,发送方转走 hub,由 hub 按发送方限流) |
| 1 | 外层解码;`v == 1`;`to == 本节点`;`suite` 已知 | P;直连(p2p)路径上第 1–4 步的失败为 T:经直连到达、本节点打不开的信封可能是发给别人的(过期或被复用的 rendezvous 地址),ack 会让发送方视为已投递、不再走 hub [redteam:F22] |
| 2 | 按 `kid` 查私钥(有效 + 保留期内) | P(`sealed-to-unknown-key`) |
| 3 | HPKE Open | P |
| 4 | 内层解码为通用映射;0–63 未知键拒收;`inner.to == outer.to` | P |
| 5 | `now ≤ exp`;`ts ≤ now + 5min`;`exp - ts ≤ 15 天` | P |
| 6 | 解析 KEL(不做网络请求):`inner.kel` 回放成功且推出 `from`;与 `peer_identity` 已存 KEL 比较:内层延伸已存 → 候选更新;已存延伸内层 → 用已存;分叉 → 拒收;无记录 → 用内层(首次信任,§21) | P;读取 `peer_identity` 出错为 T |
| 7 | 签名:顶端活跃密钥态直接接受;非顶端态仅当 `ts < SupersededAt` 且 `now − SupersededAt ≤ rotation_grace`(默认 1 小时)时接受 [C4c] | P |
| 8 | `keys` 附件是建议性的:`VerifyEncKeySet(keys, from, kel, now)` + 三分支高水位;失败不影响本消息 | — |
| 8½ | 判重(在第 9 步之前)[redteam:F26]:进程内按 `(from, mid)` 加锁(delegate 另按 `(from, ix)` 加锁,使同一委派以两个 mid 封装的副本也只判定一次:后到的一份见到交互已存在,按重投处理),锁持有到第 10 步结束;先查拒收表,命中 → P(`refused-replay`),不回复;再查持久重放表,命中 → 不再判定:delegate 按第 9 步的重投条件(同一交互、同一 request CID、deny/allow)核对后进入第 10 步的"已答复 → 重发结果"分支,其余 ack 不处理。同一信封经 hub 与 p2p 并发到达时,后到的一份等前一份判定并提交后按重复处理,不占准入、不发矛盾的 `rejected` | 读库出错为 T |
| 9 | 授权(只做判定;拒绝类回复经限速发出;不写业务表,被拒的 `anet.delegate/1` 只在回复前写入拒收表,见表下 [redteam:F5]):按 type 判定(§5 入站策略;message/status/result 须交互存在或在待批表中、`PeerAID == from`、角色正确)。`anet.delegate/1` 的 `ix` 已存在时,仅当该交互 `role=inbound`、`PeerAID == from` 且新 TaskDoc 的 CID 等于该交互存储的 `request_cid`(同一请求的重投)才进入第 10 步(重投/幂等路径),否则 P,计数 `ix-collision`,不回复 [m];进入重投路径前同样先查 deny,`trust=peer` 的交互再查 allow(§5.1),不满足为 P。待批表中的 ix 同理:TaskDoc 与待批项的 request CID 不同为 `ix-collision` [redteam:F6][redteam:F7]。已认证发送方指向未知 ix 的 message/cancel:发送方与本节点有关系(有持久 `peer_identity` 行、在 allow/trust 名单、或本节点对其有出站交互)且其正在等待的此类消息少于 32 条时,`now − inner.ts ≤ 10 分钟` 按 T 处理(等待 delegate 先到);陌生人、deny 中的对端(与陌生人不可区分,X2)与超出上限者,以及超过窗口的,回 `status{failed, anet.a2aError: TaskNotFound}` 并 ack,与拒绝通知共用限速 [C19][redteam:F25]。经直连(p2p)到达、指向未知 ix 的 message 一律 T(不论发送方、不论窗口,不计入每发送方上限):发送方转 hub,落在已进信箱的 delegate 之后;否则 delegate 被直连拒收转 hub、随后的消息走直连抢先到达时,陌生人的消息被判 TaskNotFound 丢失而 delegate 照常执行 [redteam:F25 复核] | P;有关系的发送方在未知 ix 窗口内、直连路径上的未知 ix、读库出错为 T |
| 10 | 处理(`(from, mid)` 锁与重放表查询已在第 8½ 步完成;持久重放表已有该行的 delegate 走"已答复 → 重发结果"分支,其余 ack 不处理 [C33];结果重发按(对端, ix)限速——每份答复突发 2 次、此后每 5 分钟 1 次,全节点每分钟 60 次,超出只 ack 不重发,重投不能消耗本节点的 hub 发送预算 [redteam:F29])。执行业务写入,**在同一 SQLite 事务内**插入重放行 `(from, mid, exp)`;不能纳入事务的副作用(能力执行、发结果)沿用现有业务幂等检查。重放行已存在、交互非终态且无结果、本进程内也无该 ix 的执行记录时,视为崩溃遗留:短能力调用重新执行(至少一次,沿用现有"无回执即重跑"),长能力调用不重跑(至多一次)。重投路径执行的只是该交互受理时记录的能力调用(`is_capability` 且从存储的 `request_doc` 读回),从不取重投信封里的 TaskDoc;文本任务的重投不执行任何能力 [redteam:F7]。成功后:把候选 KEL/keys 写入 `peer_identity`(仅 §3.8 所列授权上下文)、`noteLivePeer(from)` [m]。§5.2 的写入(交互、待批项)与第 4 行的 `submitted` 回复都在本步与重放行同一事务提交之后进行。入站附件行 [redteam:F27]、requester 收到的报价/付款失败/收据写入的 pay 列(`pay_state`、`pay_required`、`quote_expires_at`、`pay_payload`、`pay_receipts`)[redteam:F28] 都是业务写入,在同一事务内;付款证据账本是独立存储,在提交后写 | T(存储) |

- 第 9 步拒绝的信封进内存有界 LRU(第 5 步后即查,省去重放的验签);被拒的 `anet.delegate/1` 另写入持久拒收表 `refused(from, mid, ts, exp)`(与重放表同在 `interactions.db`,按 exp 清理,不回复)[C15c][redteam:F5]:拒绝是终局,重启、LRU 被挤出或策略/配额此后改变,同一信封再到都按拒收处理,不再判定、不再回复。拒收先落盘再回 `rejected`:写不进拒收表时不回复、不 ack(T),下次投递重新判定,请求方不会收到一份重启后可能被推翻的 `rejected` [redteam:F5]。拒收表有界:每发送方 1024 行、总计 10 万行,超出时最旧的行(按 `inner.ts`)删除,并把**被删行各自发送方**的下限抬到其被删行的最大 `ts`;重放表未收录且 `inner.ts ≤` 本发送方下限的 delegate 按拒收处理(P,`refused-floor`,不回复)。不设全局下限:`inner.ts` 由发送方决定(可比现在超前一个时钟偏差),全局下限会让一批一次性身份的拒收把它抬过所有其他发送方下一条 delegate 的发送时间,把它们全部无回复地丢掉 [redteam:F5]。下限表也有界(10 万个发送方),超出时先忘掉最早到期的下限。message/status/result 的拒收仍只进 LRU(其第 9 步不写库,至多一条限速的 TaskNotFound)。
- 直连路径的 ack 在第 10 步事务提交时给出,不等其后的副作用(能力执行、答复发送);答复照常经重试队列发出。临时拒绝回 nack(带原因),发送方立即转 hub。经直连接受的短能力调用在该事务内记为 `working`,进程在执行前停止时由启动恢复重跑(至少一次),因为不会再有重投 [0017 Q29]。
- 信箱轮询从游标之后读(0017 Q1);游标之后总有新信时也至少每 10 轮回头读一次暂扣的消息,使其得以重试、窗口到期的得以转 P [redteam:F24]。回头读有自己的位置,从队头逐页向后,读到前向游标处或信箱末尾再回到队头;前向游标从不后退,其余各轮照常读新信。只用一个游标时,回头读把前向读也拉回队头,暂扣超过约 10 页(大信封按 hub 的轮询字节预算可一页一条)就再也读不到其后的新信 [redteam:F24 复核]。每轮仍只发一次请求。
- 第 10 步"已答复 → 重发结果"分支用本信封第 8 步验过的 `keys` 封装(终态 `public`/`public_cap` 交互已清空 `peer_keys` 也能封上);出站队列已有该 ix 的结果行时只把它置为立即到期,不入第二份;重发带首次发出的结果元数据(`result_meta`:`anet.reason`、`anet.retry_after_ms`、`anet.effect_status`)与当前的 x402 收据 [impl:wp/wire be2fdb3][impl:wp/a2ashape 7863633]。
- 重放表、`pending`、`peer_identity` 与交互表同在 `interactions.db`,使第 10 步的同事务写入成立。
- 启动恢复:非终态且无结果的长能力调用交互置 `failed`,`anet.reason=interrupted`、`anet.effect_status=UNVERIFIED`(效果是否发生未知),经结果重试队列通知请求方 [C1]。本进程正在执行的调用不属遗留;传输模块交来的入站在 `New` 全部完成(所有模块启动、启动恢复与付款恢复)之后才处理,此前到达的等待,等不到(其 context 结束、daemon 停止)则判 T [redteam:F30]。第 0 步限速在等待之前:超限的直连投递启动中也立即拒收,等待中的投递(各持信封与协程)至多为限速放行的量 [redteam:F30 复核]。r4 按实现细化为逐行分类(`leftoverAction`)[impl:wp/wire 1591718、e5e1d8a]:
  - 已登记、未置 `working` 的长调用 → `failed`/`interrupted`/`UNVERIFIED` 并送达;启动时 provider 尚未注册而无法分类的长调用,由重投路径在交互"早于本进程创建"时同样报告;
  - `working` 的短调用(`pay_state` 为空或 `completed`)按至少一次重跑,不报 interrupted(已付短调用的委派与付款都已 ack,无重投可依赖);`submitted` 的已批准(`trust=approved`)短调用同样在启动时重跑(它在入待批表时已 ack);只登记的短调用留给重投;
  - `pay_state ∈ {required, failed}` 的行不动(标价工作只在付款后执行,未付即从未执行),由报价过期扫描收尾;收到付款消息、取款前停机的,重启时从消息表取回该 `payment-submitted` 重新处理,不误报中断 [impl:wp/x402d fe49f7a];
  - 重跑前重读 deny(`trust=peer` 的交互另查 allow):未付工作的对端已在 deny 中(或已不在 allow 中)则不重跑,留给撤销扫描取消;已付工作照跑(§5.1)[Q10][redteam:F7]。
- 关停不产生确定结果(SI-10):daemon 关停开始后不再开始能力执行;执行被关停截断(调用报错,或结果不是 OK/UNVERIFIED 且 daemon 上下文已结束)时不写结果、不入队,交互保持非终态,信封按 T 不 ack,重投命中重放行后重跑;长调用留给下次启动恢复;关停中已发生的效果(OK/UNVERIFIED)照常记录 [impl:wp/wire 1591718]。迟到结果在接收事务内与重放行同一事务写入 [impl:wp/wire be2fdb3]。
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
- hub 缓存 `(aid, sig)` 至窗口结束,拒绝重放。缓存按签名方分片,条目按窗口终点排序、到期即出,窗口结束前不淘汰;每个 AID 至多 32768 条活跃条目(且不少于发送令牌桶在最长窗口内可签数的两倍),超出只对该 AID 回 429 + `Retry-After`;全局上限(1<<20)满时只拒绝持有不少于平均份额(上限 ÷ 持有条目的签名方数,至少 1)的签名方(503 + `Retry-After`),其余照常准入。原先全局满即对所有签名方回 503,一个注册 AID 以未来时间戳的签名 poll 就能让全 hub 停止中继 [redteam:F4]。邀请制 hub 上,本 hub 不认识且没有可用邀请的注册在回放 KEL 与验签之前即 403,不进重放缓存:否则陌生人每次换一个新 AID 注册,都在验签后占一个签名方再被拒,压低所有人的份额 [redteam:F4]。份额随持有条目的签名方数下降,这一点不变:开放注册的 hub 上,一方持有 n 个已注册 AID 并让它们合计填满缓存(约 1,700 个签名请求/秒),就把份额压到 1<<20 ÷ n,窗口内用量高于此的 agent 在缓存满期间得到 503;n 的上限是每 IP(IPv6 为每 /64)的注册限速 [C15f]。
- hub 对它持有的 KEL 验签(每个签名请求对签名方的存储 KEL;未鉴权路由上 `/x402/verify`、`/x402/settle` 的付款方、`/reviews` 的双方、联邦卡片 keyset 与 `/x402/resource` 卡片的所属方)时,进程内每条 KEL 只回放一次:hub 打开存储时安装 ANetCore `identity.ReplayCache`(按 KEL 编码的 SHA-256 键控,64 MiB、LRU),建于 `identity.Replay` 的每个 Verify 都经它。请求说明了所签内容时(relayauth v2、taskboard 挑战、支付授权、评价),先用 KEL 在所声明 `key_state_seq` 处给出的密钥核签名,核不过即以 `VerifyObject` 对它的同一拒绝返回,不回放 KEL。原先 `/register` 限了 KEL 长度之后,这些路由仍在每个请求上先整条回放 KEL 再看签名:带全零签名、点名一个上限处 KEL 的未鉴权 `POST /relay/poll` 让 hub 付出整条回放(约 40 ms)后才 401 [redteam:F36]。
- daemon 在注册前以 `GET /hub/identity`(或现有等价端点)取得 hub AID 与 KEL。

端点:

| 端点 | 请求体 | 响应 | 规则 |
|---|---|---|---|
| `POST /relay/send` | `{to_aid, envelope: b64}` + 认证头 | 200 `{id}`;400 信封结构不符;401;404 收件人未知且联邦也无;413;429 + `Retry-After`;507 信箱满 | 发送方须已注册;hub 检查外层 `v==1`、`to==to_aid`、`suite` 已知、`len(enc)==32`、`ct` 非空;不存 `from_aid` |
| `POST /relay/poll` | `{limit, after_id?}` + 认证头 | `{messages:[{id, envelope}]}` | 只返回 `id > after_id` 的行,按 id 升序;`limit` 与 48 MiB 预算不变(预算从游标处起算,首条照旧必返);缺省或 0 与旧行为相同,负数 400 [Q1][impl:ANetHub wp/relayhol 8a5755c] |
| `POST /relay/ack` | `{ids}` + 认证头 | 200 | ack 即删 |
| `GET /agents/{aid}/keys` | — | `{aid, keyset, kel}`(b64) | 本地注册、联邦卡片,或经 `/fed/v2/keys/{aid}` 查询 |
| `POST /agents/keys:lookup` | `{aid}`(≤ 4 KiB) | 同上;400 无 `aid`;413 | 与 GET 相同,AID 在请求体而不在请求行:发送方从自己的地址查收件方,代理日志若记请求行就留下"该地址写给该 AID"[redteam:F3] |
| `POST /agents/kel:lookup`、`POST /a2a/v1/agents/card:lookup` | `{aid}`(≤ 4 KiB) | 同 `GET /agents/{aid}/kel`、`GET /a2a/v1/agents/{aid}/card`;400、413 同上 | daemon 取对端卡片(A2A 代理卡片、MCP `get_agent_card`)与验卡用的 KEL,AID 在请求体;没有该路由的 hub 以不带 JSON 错误的 405/404 答复时回退 GET [redteam:F3] |
| `POST /agents/{aid}/keys` | `{keyset}` | 200 / 409 | 自证明:hub 以已存 KEL 验证 + 发布方高水位 |
| `POST /register` | 现有 + `enc_keys` + `a2a_card` | 现有 + 各字段状态 | 删 `guest_messages`;KEL 须为已存延伸;KEL 不超过 `seal.MaxKELEvents`/`seal.MaxKELBytes`(256 事件 / 64 KiB,与发送方对所收 KEL 的上限一致),超出 400。`GET`/`POST /agents/{aid}/keys` 与 JWKS 对上限之前存下的超长 KEL 一律拒绝,不回放 [redteam:F36]。`card_status` 取值 `ok` `unchanged` `absent` `invalid` `conflict` `withdrawn`(删去 `unverified`),`card_error` 以 a2acard 错误码开头 [impl:ANetHub wp/hubreg 23ed2c8];`a2a_card` 缺省表示不变,JSON `null` 表示撤回(§10.1)[Q6] |
| `GET /agents/{aid}/ledger`、`/balance`、`/redemptions` | — | 本人签名 GET 才返回明细;无签名 401 | daemon 的 `Balance`、`Reconcile` 与兑付列表一律以 relayauth v2 签名读取(action `balance`/`ledger`/`redemptions`);对账按 hub 兑付列表的 `auth_id` 匹配,列表被截断时计入 `redemptions_unchecked` 而不报缺失 [impl:wp/c3more 9a34c97] |

daemon 的信箱游标(防队头阻塞)[Q1][impl:wp/relayhol 374dd12、6060020]:游标按 hub URL 记在内存。本页有判为 T、不 ack 的行,或整页取满时,下一轮从本页最大 id 之后继续;游标之后为空时只把游标归零,下一轮从信箱头部读(每轮恒为一次签名 poll,T 行至少隔轮重试一次);T 行在窗口到期或转为 P 后照常 ack。重启后游标归零,等同旧行为。

hub 限额(应用层,均为 flag):单条信封 96 MiB;每发送方令牌桶 20/s 突发 200;每收件方未投递 5000 条或 1 GiB;未投递 TTL 14 天;单次 poll 预算 48 MiB;`/register` 按 IP 限速。前置 nginx 的 `client_max_body_size` 须不小于 hub 的 `/relay/send` 体上限(base64(96 MiB) + 64 KiB),部署样例为 129m [impl:ANetHub wp/hubops 030f22d]。taskboard(加法 tag,挂在 hub 根 mux 上,不经内核的 `limitBody`)的写入路由自带 64 KiB 请求体上限,先只解码鉴权字段核签名,通过后才解码内容 [redteam:F38]。

`relay_message` 只保留 `id, to_aid, payload, size, created_at`;迁移重建表;`PRAGMA secure_delete=ON`。迁移时 wire-1 的行(含未投递的明文载荷)一行不复制,新表为空、自增计数延续;丢弃条数(未投递/已投递)与迁移时刻在同一事务写入 `hub_meta`(键 `relay_v2_*`,只有计数与时刻)并记一行日志,清理脚本据此报告 [Q17][impl:ANetHub wp/hubops 030f22d]。

### 3.8 KEL 与对端身份的持久记录

- ANetCore `identity.ExtendsKEL(old, new)`:分别返回"回退"与"分叉"两类错误;`old` 为 `new` 的前缀(逐事件原像比对)才算延伸。
- daemon 持久表 `peer_identity(aid PK, kel, kel_len, keyset, keyset_seq, keys_checked_at, card_seq, card_hash, pinned_reason, updated_at)` [C2][C12];`card_seq`/`card_hash` 是该对端网络卡片的持久高水位(§10.3 三分支,重启不丢;只有 `card_seq` 的旧行,seq 仍作下限,同 seq 的卡接纳并补记 hash),不动 `updated_at` [impl:wp/proj 78b4963、a1feeff]:
  - 只在授权上下文写入:第 10 步接受且对端 ∈ allow ∪ trust、或交互 role=outbound、或待批项经人工批准之后;本节点主动联系对端时;`anet peers allow <aid>` 时(尽力取回并固定,取不到则在首条有效消息时首次信任)。hub 自身 KEL、组织/黑板发行方 KEL 同样固定。`trust=public`、`trust=public_cap` 交互的对端不写入 `peer_identity`;其 KEL 与 SignedEncKeySet 存在该交互行中,供回复加密使用,交互终态后删除 [C12]。
  - `pinned_reason` 取值 `allow`、`trust`、`hub`、`issuer`、`outbound`;非空的行不淘汰,LRU 只在 `pinned_reason` 为空的行之间进行,且只由上述授权写入触发。
  - KEL 只经 `ExtendsKEL` 更新,keyset/card 只经高水位更新;不因入站流量淘汰;被非终态交互、待批项、结果重试、x402 报价引用的行不淘汰,其余按 LRU,上限大。
  - 陌生人(未授权)的 KEL 与 keys 只进独立的有界内存缓存,仅用于加密 `rejected` 回复,不进入 `ResolveKEL`、不参与高水位。没有 `peer_identity` 行的对端,其卡片高水位同样只在内存(每进程有上限),重启即忘(§21)[impl:wp/proj 78b4963]。
  - `peerKELs.remember` 必须回放并核对推出的 AID 等于存储键 [C0]。
- hub:`agent.kel`、`fed_card.kel` 只接受延伸(注册方就是本人,较短 KEL 直接拒绝)。

### 3.9 联邦

- `/fed/v1/forward` 信封删除 `from_aid`、`kind`、`interaction_id`,签名原像同步修改,联邦线协版本递增。
- 新增 `GET /fed/v2/keys/{aid}`:由发起 hub 签名、只接受对等表中的 hub;返回任一本地注册 AID 的 `{keyset, kel}`,不论可见性;请求方 hub 只为发起查询的 daemon 缓存,不进目录与索引 [C32]。
- 联邦卡片条目增加 `keys`。
- 对端 hub 的 KEL 在首次经 `/hub/identity` 取回时解码并回放,推出的 AID 须等于配置的对端 AID 才写入 `fed_peer_kel` 固定(对端自报的 `aid` 字段只是声明);打开库时丢弃不回放到其键 AID 的旧固定,下次使用时重新取回。对端端点仍可配置为 `http://`:首次取回时的中间人能让取回失败,但不能再让本 hub 固定并公开他人的 KEL [redteam:F34]。未固定的对端 KEL 每次使用都会重新取回,而一次使用可以是内核上未鉴权的 `GET /agents/{对端}/kel`:因此同一对端的取回一次一个,等待中的使用共享那次取回的失败而不各自再取;对端发来不能证明其 AID 的 KEL 之后 1 分钟内不再取回(对端宕机等传输失败不设等待,恢复后的下一次使用即固定);取回的 KEL 以 `seal.ParseKEL` 的上限解码,先以 icp 核对 AID(一次验签)再回放其余事件。`identity.Replay` 对长度不是 32 字节的 icp/rot 密钥返回错误而不是在 `ed25519.Verify` 中 panic——修正之前固定下的这种 KEL 曾会让 hub 在打开库时 panic [redteam:F34]。
- `FedReview` 删除内容字段;修空串被解码为非 nil 空切片而触发内容绑定的问题(R09 §5 第 9 点)。
- `/fed/v2/cards`(§10.6)。

### 3.10 p2p

帧只携带 `To`、信封字节与 `ID`。anetpeer 为每个 recv 帧铸造唯一 `ID`,按 `ID` 关联 ack;daemon 在 `Receive` 得出"应 ack"结论后回 `{Op: ack, ID}`(第 10 步提交即给出,§3.6),暂时性失败回 `{Op: nack, ID, Error}`,anetpeer 立即以该原因结束交接 [C5][0017 Q29]。anetpeer 对 `To` 不是本进程承载的 AID 的投递直接回 error,不交给 daemon [redteam:F22]。失败的 send 回帧带 `not_delivered` 表示确知未送到任何 daemon(无地址、拨号失败、对端在交接前或交接时拒收);交接超时等不确定的失败不带,daemon 按"可能已送达"处理 [redteam:F12]。超时以 anetpeer 为准对齐:拨号 3 s、交接 10 s、回帧余量 5 s(一次投递至多 18 s),daemon 的 p2p 发送默认等 20 s,总能等到 anetpeer 明确的 ack/nack,不会在 anetpeer 仍可能送达时转 hub 造成重复投递(docs/notes/0025 N3)[0017 Q29]。重写 `module/p2p` 的 fake peer 按 ID 关联;并发两条相同 `To` 的投递各自得到自己的 ack(mutation:ack 键改常量)。帧增加 `V`(=2);anetpeer 对缺 `V` 或 `V < 2` 的入站投递帧不交给 daemon,回 `{Op: error, ID, Error: "peer requires anet >= 0.2.0"}`,旧发送方因此不会记为已投递;测试:无 `V` 的帧得到 error 且 daemon `Receive` 未被调用 [m]。
- anetpeer 以 hub 作 rendezvous,但它不持密钥、不能自己发布地址:p2p 直连地址由 daemon 在注册后以 `/p2p-advertise <拨号地址>:<p2p 端口>` 发布(网络卡片的 p2p 接口同样取运营者配置的 `advertise`,§10.1);未发布时对端一律走 hub,结果照样正确。跨 hub 的 p2p 要等本方 hub 从目录学到对方的 home hub 之后才会被使用 [impl:wp/testrun 1904859][impl:0021 §2]。

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
| `result_meta` | 结果随附的元数据(`anet.state`、`anet.effect_status`、`anet.reason`、`anet.retry_after_ms`),provider 发出与 requester 收到时都落库,供投影与重发使用(此前两侧都丢失 reason 与 retry_after_ms)[impl:wp/a2ashape 65fcc61、7863633] |
| `message.msg_id`、`message.metadata`、`message.kind` | 发送方也存 msg_id,取值由信封内层 `mid` 派生(§3.4)[Q9];kind 增加 `status`、`payment`;对话轮次只取 kind=text 且无控制元数据(任何 `x402.*` 键或 `anet.state`)[C29][impl:wp/a2ashape 65fcc61](`anet.final=true` 的最后回复例外,见 §4.2 [Q30]) |
| `anet.cancel_requested`(不设列) | 由付款列与消息表推导:requester 一侧 `pay_state ∈ {submitted, completed}`、任务非终态、且存在本方发出的 cancel 消息。0017 Q3 写的是新列,实现以推导满足同一语义,不加列 [Q3][impl:wp/wire 1591718] |
| 出站队列 `outbox` | 另增 `mid`、`digest` 列(§3.5 第 4 步);待批表 `pending` 增 `msg_id` 列(§3.4)[impl:wp/wire be2fdb3、4f0141b] |

状态由事件显式写入 [C35]:

| 事件 | state |
|---|---|
| requester 建任务 | submitted |
| requester 发 text 消息或 payment-submitted | working |
| requester 发 cancel 或 payment-rejected(`pay_state ∉ {submitted, completed}`) | canceled |
| requester 在 `pay_state ∈ {submitted, completed}` 后发 cancel | 不变,`anet.cancel_requested=true`(§4.2)[Q3] |
| provider 发消息,未标 `anet.state=working` | input-required |
| provider 发消息标 `anet.state=working`,或 `StatusMsg` | 其携带的状态 |
| provider 以 completed 结束文本任务时的最后一条回复(`anet.state=working` + `anet.final=true`)[Q30] | working(随后结果置 completed) |
| 结果到达 | 按 §4.3 映射:completed / failed / rejected |

- `IsTerminal()` 判定 `state ∈ {completed, failed, canceled, rejected}`,替换全部现有终态判断(`autoreply.go:294`、`delegation.go:141/223/243/268/380` 等)。
- 状态迁移用 `UPDATE interaction SET state=?, state_at=?, state_seq=state_seq+1 WHERE id=? AND state NOT IN ('completed','failed','canceled','rejected')` 并返回是否更新,防止终态互相覆盖、防止在已取消交互上签回执。
- 列表按 `state_at` 降序分页(修 R02 D8)。另建 `interaction(role, pay_state)` 索引,供每分钟的报价过期扫描 [impl:wp/x402d fe49f7a]。
- 带付款元数据的状态先存报价、再宣布状态:requester 收到 `payment-required` 时,付款列(`pay_state`、`pay_required`、报价过期)写好之后才发布状态事件,等待者(`/tasks/wait`、本机 A2A 阻塞发送)醒来读到的 `input-required` 必带报价与 `anet.reason` [impl:integ/round4b 3173f69]。

### 4.2 完成与取消

文本任务:
- provider 完成:`reply_task(state=completed)`、`anet end`(provider 侧)或自动回复判定完成 → 签回执(对话记录 v2)→ `anet.result/1`。带回复的完成(`reply_task`/`/tasks/reply` 带文本或文件、§11.6 后端的完成答复、自动回复判定完成)先发这条回复,metadata 为 `anet.state=working` 与 `anet.final=true`,再发结果:requester 在两个信封之间保持 working,阻塞等待不会在中间的 `input-required` 返回(0017 Q30,TCK DM-ART-001、Hermes "needs more input")。带 `anet.final=true` 的回复不按控制消息处理:仍是对话轮次,进入回执覆盖的对话记录、history 与 `anet.reply`(§4.1 [C29] 的例外)。`anet end` 不带消息,不涉及此规则 [Q30]。`/tasks/reply` 的 completed 最终答复经对话记录回执送达,在请求方投影为 `anet.reply` artifact(§11.5)[Q4]。
- requester 结束请求(`end_request`)→ provider daemon 自动完成(不需要 provider 的 agent)。A2A 面以 SendMessage 的 `metadata["anet.end_request"]=true`(可无正文)表达(§11.5)[Q4](实现待补:本机 A2A 接口与内核都还不识别该键,A2A 客户端目前无法请求完成;控制面 `/end` 与 CLI 可用)[B6-02 复核]。
- requester 取消(`cancel`)→ 本地 `canceled`;provider 置 `canceled`、取消长调用、回 `status{canceled}`,不签回执。
- requester 取消时 delegate 仍在本节点重试队列中、且没有任何尝试可能已送达(§3.5 第 4 步)、也没有投递尝试正在进行:同一事务删除该任务的整条外发队列、记录取消、置 `canceled`,不向 provider 发送任何东西(记 `anet.delivery.expired`,reason=`withdrawn`);否则 cancel 排在 delegate 之后发出。未送达即被放弃的 delegate 连同其后排队的外发一起删除 [redteam:F23]。

能力调用 [C6][C29][C34]:
- 未提交付款(`pay_state` 为空或 `required`)且未开始执行时,收到 `end_request` 或 `cancel` → `canceled`,不签回执。
- 未付费能力执行中:`cancel` → 尽力取消 context;`end_request` 忽略,结果照常交付;两者都不签对话记录回执。
- `pay_state ∈ {submitted, completed}` 之后收到的 `cancel`/`end_request` 按下文"付款已提交"条处理,不取消执行。
- 回执只来自能力结果路径。
- 付款已提交(`pay_state ∈ {submitted, completed}`)之后,requester 的取消不改变本地状态,返回 `working` + `anet.cancel_requested=true`,并照常发出 cancel,等 provider 的 status/result;provider 在结算成功后不接受取消,完成并交付,无法交付时回 `failed` + `x402.payment.receipts` [Q3]。
- 挂起的取消在付款未成时补做:付款 `submitted` 时发出的取消被 provider 忽略后,provider 报 `payment-failed` 或重新报价(付款未被收取)时,requester 检测本方挂起的 cancel 并执行取消(此时可取消,两侧置 `canceled`),不再自动付款 [impl:wp/wire e5e1d8a]。
- 付款与取消只能有一方赢:provider 取款(`pay_state → submitted`)在写事务内检查终态,读行后任务已取消或报价已过期即不送结算;provider 的 CancelTask 在写事务内重读 `pay_state`,付款已取则拒绝取消 [impl:wp/x402d fe49f7a]。
- 带 `x402.payment.receipts` 的 status/result 即使到达已终态交互,也验证并记录结算证据(不重开 A2A 状态);只带收据的状态(如报价后的 `canceled`)同样记录 [impl:wp/x402d fe49f7a]。对端给的收据列表只取最新 128 项(provider 自己只保留 64 项),每个成功项验签后写一条 `anet.payment.settled` [impl:wp/c3more 2b8f8af]。

其他:终态之后只拒绝新的 SendMessage 输入(`UnsupportedOperation`)。出站结果重试队列:持久化信封字节,指数退避最长 24 小时,重启恢复。

### 4.3 能力调用五态

| 效果状态 | state | 附加 |
|---|---|---|
| OK / UNVERIFIED | completed | `anet.effect_status` |
| FAILED | failed | 同上 |
| UNAVAILABLE(暂时性) | rejected | `anet.retry_after_ms`(如长调用槽满,60 秒)|
| UNAVAILABLE(其他) | rejected | `anet.reason`(如 `capability_not_served`);结果既无原因也无重试时间时,投影补通用原因 `unavailable` [impl:wp/a2ashape 65fcc61] |
| PAYMENT_REQUIRED | input-required | §8。报价是状态不是结果:provider 发 `StatusMsg{input-required, payment-required}`,不签回执、不写结果;投影不把报价当作 artifact,报价文本也不充当之后终态的 `status.message` [impl:wp/x402d 2cc279b][impl:wp/a2ashape 7863633] |
| 中断(崩溃后效果未知) | failed | `anet.effect_status=UNVERIFIED`、`anet.reason=interrupted` |
| 委派被放弃(过期或 hub 永久拒绝)且无尝试可能已送达 | failed | `anet.effect_status=UNAVAILABLE`、`anet.reason=undeliverable` |
| 委派被放弃,但曾有尝试可能已送达(如 p2p 送到后应答超时,hub 又 413/404) | failed | `anet.effect_status=UNVERIFIED`、`anet.reason=undeliverable`(效果未知)[redteam:F12] |
| 效果未知(调用已发出、应答丢失:超时或连接中断)[redteam:F10] | failed | `anet.effect_status=UNVERIFIED`、`anet.reason=timeout` / `connection_lost` |

已结算(`pay_state=completed`)的交互不使用 rejected:UNAVAILABLE 映射为 failed,并带 `x402.payment.receipts`(§4.2、§8.2)。

UNAVAILABLE 只给"确定没发出"的调用(连接建立失败、拨号被拒、写出请求头之前出错);请求一旦写出,之后的超时、断连一律按"效果未知"报告。provider 以 `provider.OutcomeUnknownError` 向内核表达这一情形(service 模块按请求头是否写出区分)[redteam:F10]。2xx 应答头之后应答体中断(读体时超时或断连)同样是"效果未知",不是 FAILED;ANetLink shim 与 service 模块同样按请求头是否写出区分;任何 provider 的调用在截止时间内未返回(错误含 `context.DeadlineExceeded`)时,内核与凭证入口一律按"效果未知"(`anet.reason=timeout`)报告,不依赖 provider 自己表达(`provider.OutcomeOf`)[redteam:F10 复核]。service 模块收到网关代答的 504(上游未及时应答)或 502(上游应答中断)同样按"效果未知"(`timeout` / `connection_lost`)报告:请求已到达后端;503/429 仍为 UNAVAILABLE [redteam:F10 复核]。

无结果终态的 `anet.effect_status`(SI-6 不许终态能力任务对效果沉默;从不推成 OK)[Q2][impl:wp/a2ashape 65fcc61]:

| 终态,无交付物 | `anet.effect_status` | 说明 |
|---|---|---|
| rejected(策略拒绝、未公开、未服务、缺 nonce 等) | UNAVAILABLE | 什么都没做 |
| failed,结果元数据写明了效果 | 取所写值 | 未送达(`undeliverable`,请求方放弃出站行时自己写入)→ UNAVAILABLE [Q5],曾有尝试可能已送达的 → UNVERIFIED [redteam:F12];`interrupted`(启动恢复,§3.6)→ UNVERIFIED |
| canceled,以及其余没有写明效果的终态(含 failed) | UNVERIFIED | 请求方无法知道对端是否已开始执行 |

有结果时一律取结果(交付物的 `status`,其次结果元数据)。注:0017 Q2 的文字把"无结果的 failed"与"执行前 canceled"写作 UNAVAILABLE;Q2 的决定是"追认 wp/a2ashape 的规则",本表按该规则的实现写:未写明效果的 failed 与 canceled 为 UNVERIFIED。措辞差异登记在 `0022`,待确认。

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
     "global_per_min": 1200, "max_inflight": 16, "max_args_bytes": 4096,
     "evidence": "cid"}
  ],
  "reject_notice": {"per_peer_per_hour": 6, "global_per_min": 60},
  "pending": {"max_total": 200, "max_per_peer": 3, "ttl_hours": 72}
}
```

- `policy`:`closed`(默认)、`approve`、`open`。`open` 节点在网络卡片上发布 `chat` skill(§10.1),进出 `open` 时重新发布卡片 [Q27][impl:wp/fix5 598755c]。
- `public_capabilities[].evidence`:`cid`(缺省)或 `full`,其他值配置校验拒绝。`cid` 模式下 `trust=public_cap` 调用的 `anet.capability.effect` 只记 `result_cid`、指标与 provenance 的协议字段,不记 `observed_state`;`full` 照记完整 provenance。按 §5.2 判定顺序,允许名单里的对端调用公开能力同样是 `public_cap`、按该能力的模式记录;只有非公开能力(`trust=peer`/`approved`)记完整 provenance [Q15][impl:wp/evid fb013f8、74ba580]。
- 保存期:`trust=public_cap` 的终态交互(连同消息、附件、`peer_kel/peer_keys`)在 `state_at` 早于"当天 UTC 0 点减 7 天"后删除;每小时检查、按天生效(实际保存 7–8 天),启动时先清理一次;每批 500 条一个事务,不在一个事务里持锁删完;出站队列仍有行的、`pay_state=submitted` 的(付款载荷是结算与对账所需)不删;删除了东西才写 `anet.interaction.pruned` [Q15][impl:wp/evid fb013f8、74ba580]。
- `allow_file`(可委派)、`trust_file`(可驱动本机 exec 与 A2A 后端,§6、§11.6)、`deny_file`:每行一个 AID,每次判定重读,文件不存在等于空,deny 优先。`trust_file` 从 `auto_reply` 移到 `inbound`,exec 与后端共用同一判定 [C9]。
- 配置校验集中在一个函数,由加载、`POST /autoreply`、入站策略写入共同调用:`open` 与"对非信任对端启用 exec"或"接受非信任对端的后端"不能同时成立,违反者 409,无论先写哪一个 [C8]。后端部分经 `module.Host` 新增的 `DeclareUntrustedBackend()` 完成(理由写在接口注释):`module/a2a` 构建时若存在 `accept_untrusted: true` 的后端即调用它;内核校验函数只读取这一声明,不解析 `modules.*` 配置。后端只经配置文件设置;与 `open` 冲突时加载即拒绝启动,运行时改为 `open` 的写入按同一声明返回 409。
- 迁移:旧 `accept_delegations` 缺省或 `true` → `closed`;配置里确有 `accept_delegations: true` 时日志提示一次(缺省——含最小配置——不提示,以免误导)[0024 L4];`false` → `closed`。`anet accept on` 报错并说明三种策略与 `anet peers allow`,非零退出;`accept off` 映射为 `closed`;`hub-register --accept-delegations` 与 `POST /accept` 同样处理 [C8]。
- 撤销对已有交互生效 [m]:第 9 步对所有入站信封都查 deny(新 delegate 按 §5.2 第 1 行,已接受交互的重投同样先查;message 先查交互:指向本节点不持有的 ix 时与陌生人同走未知 ix 分支,不因 deny 另答,§2 X2)[impl:wp/wire 1591718][B6-02 复核];`trust=peer` 的入站交互再查 allow(message 与同一请求的 delegate 重投都查;启动恢复重跑被中断的短能力调用前同样查 deny 与 allow,已付款的除外,0017 Q10)[redteam:F7];自动回复每轮对所有后端(不只 exec)重读 trust 与 deny,后端调用返回后再读一次,调用期间被 deny 的对端收不到回复 [impl:wp/wire 1591718、e5e1d8a];对端进入 deny 时,其活动交互置 `canceled` 并写 `anet.policy.changed`。
- deny 撤销遇到已付工作 [Q10]:`pay_state ∈ {submitted, completed}` 的非终态交互不取消(§4.2:付款提交后任一方都不取消),写 `anet.policy.changed{…, skipped_paid:[ix…]}`;该交互照常完成并交付,requester 对已付出站任务照收被 deny 的 provider 的 status/result。0017 Q10 写的是 `completed`,实现把 `submitted` 一并跳过(否则每分钟的撤销扫描会对已提交付款的出站任务反复发 cancel)[impl:wp/x402d fe49f7a][impl:wp/wire 1591718]。`skipped_paid` 在 CLI 与扫描两条路径上每个交互只报告一次;`anet audit --interaction/--peer` 识别它。

### 5.2 判定顺序(`anet.delegate/1`)

第 9 步只做判定,并发出"回给请求方"列中的拒绝类回复(`rejected`、`TaskNotFound`,经限速;不写业务表,被拒的 delegate 先写入拒收表再回复,§3.6 [redteam:F5]);"动作"列中的写入(交互、待批项)以及第 4 行的 `status{submitted, …}` 回复,都在第 10 步与重放行同一事务提交之后进行。

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
- 批准时跟随消息按接收路径的规则落库:带 `x402.payment.status` 的记为付款消息(能力调用只留元数据),状态按 §4.1 的事件表推进,批准后逐条发布;首条消息与跟随消息沿用待批项记下的消息 id(§3.4)[impl:wp/wire 1591718、4f0141b]。
- 批准只经人工通道:`anet inbound approve <ix>`、`anet peers allow <aid>` 要求从 `/dev/tty` 读取确认,非 TTY 调用拒绝;控制台会话不能批准 [C24][m]。批准时按 §3.6 第 7 步的规则对当前 KEL 重验;执行的是待批项列出的那份请求(request CID 须与列出时相同),请求方已在 deny 名单中则不批准 [redteam:F7]。TTY 检查在 CLI 进程内完成,daemon 无法区分调用来源,边界见 §21 第 13 条。
- MCP `inbound_pending` 只返回元数据(AID、到达时间、字节数、request CID、能力 id),不返回目标与正文 [m]。文档写明 anet CLI 对 agent 可达这一前提。

### 5.4 内核准入接缝

`module.Host` 增加 `Admit(callerAID, capID string, argsLen int) (release func(), refusal string)`,理由写在 `module.go` 接口注释 [C14]。内核实现:deny(每次重读)、public_capabilities、按调用方与全局配额、`max_inflight`、`max_args_bytes`。`Admit` 只在 §5.2 第 2 行(公开能力)与凭证兑付口 `RedeemVoucher` 调用;第 3 行(allow 名单)与经人工批准的待批项沿用现有执行路径,不要求 capID ∈ public_capabilities(deny 已由第 1 行判定) [C14];兑付口以 hub 证明的 `v.Payer` 作为调用方;`Invoke` 结束后调用 `release`。

兑付口不把出示者当作已认证调用方:`provider.Call` 增加 `Via`(`relay`/`voucher`),文档规定 provider 在 `Via=voucher` 时不得依据 `CallerAID` 授权;测试断言任何实现 `Priced` 的 provider 不读取 `CallerAID`。需要把调用方转给后端的 provider 经 `Call.VerifiedCaller()` 读取,它只在 `Via=relay` 时返回 `CallerAID`,兑付口得到空串,付款方不会被当成调用方 [impl:wp/official 6f1deca]。兑付拒绝沿用现有 `anet.voucher.refused` 事件,加策略原因码。

---

## 6. 自动回复 exec 与 A2A 后端

- exec 只对 `trust_file` 中的对端以现有方式运行。`auto_reply.untrusted`:`off`(默认,不调用本地 agent)或 `sandbox`。判定对象是交互对端,入站与出站一视同仁。能力调用交互一律跳过自动回复。
- 与 §11.6 后端互斥:有模块订阅入站任务时,自动回复先问内核该任务是否交给后端,交给后端的不再处理;转发失败的任务留在收件箱(MCP `reply_task`、CLI),不落回自动回复。只配置了按 skill 匹配、没有 `"*"` 的后端时模块不订阅,自动回复照常 [impl:wp/backends dee9d78、845024f]。
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
2. 控制台票据:`anet console` 用控制令牌取 60 秒单次票据,打开 `/console#t=<ticket>`;页面清除片段后换取会话。票据不经浏览器命令行传递 [redteam:F19]:进程参数对所有本机用户可读(`/proc/<pid>/cmdline`),读到的一方可抢先兑换。`POST /console/ticket {"launcher":true}` 时 daemon 在私有运行时目录(0700)写一个 0600 的一次性 HTML 重定向页 `console-<随机>.html`(`O_EXCL|O_NOFOLLOW`),CLI 只把该文件路径交给 xdg-open/open;票据兑换、到期(定时器,不依赖后续请求)或被挤出时 daemon 删除该文件,下次写入时清掉上次 daemon 留下的过期页。`anet console --url` 仍把 URL 打到标准输出供手动打开;不支持 launcher 的旧 daemon 不退回把 URL 放进命令行,而是报错提示重启或用 `--url`。cookie 名 `anet_s_<port>`、`HttpOnly; SameSite=Strict; Path=/`;CSRF 值只在页面内存中,无端点可凭 cookie 取回,刷新需新票据 [C10]。
3. 会话路由白名单(与 `console.html` 实际调用一致)[C10][C41]:只读(`/status` `/threads` `/thread` `/inbox` `/results` `/evidence` `/balance` `/identities` `/find`)、`GET /attachment`、multipart 的 `/delegate` 与 `/message`(JSON `attachments` 路径字段与 `pay:true` 拒绝)、`/end`、`/review`、`POST /console/switch`。其余一律 bearer-only,包括 `/pull`、`/autoreply*`、入站/peers/trust 写入、`/hub-register`(移到 CLI)、`/hub-leave`、`/visibility`、`/p2p-advertise`、`/shutdown`、`/x402-authorize`、`/redeem`、`/reconcile`。测试遍历全部路由。
4. 身份切换:`POST /console/switch {aid}`,当前 daemon 从 `RunningDaemons()` 找到目标、读取其控制令牌(同 uid)、向目标取票据,返回目标控制台 URL。注册表的 `/ping` 只说明该端口有人应答(崩溃后留下的条目可能已被其他本机用户占用),取票据的连接先经第 10 条的核实 [redteam:F18]。
5. `console.html` 改造:注入不含令牌的 `window.__ANET{aid,name,hub,nonce}`;`ctl` 改为 cookie + `X-Anet-CSRF`;删除访客代码、评价的 goal/deliverable 渲染与"附完整交互内容"文案;`tasks_completed` 标签按 §9 改;附件内联判断改用四种图片类型白名单;删除"连接到本 Hub"按钮与 `ctl("/hub-register")`(未注册时只显示 CLI 命令 `anet hub-register`),删除"同意结束"按钮与 `ctl("/end-accept")`,结束 UI 只保留 `/end`。CSP:脚本 nonce,`frame-ancestors 'none'`,`connect-src 'self' <cfg.HubURL 源>`;加载测试断言目录数据仍能显示。
6. `/attachment` [C11]:按 `http.DetectContentType` 嗅探,只有 png/jpeg/gif/webp 内联,其余 `application/octet-stream` + `Content-Disposition: attachment`;所有响应加 `Content-Security-Policy: default-src 'none'; sandbox` 与 `nosniff`;去掉 `immutable` 一年缓存;收到附件时把 `Mime` 改写为嗅探结果。
7. `/pull` [C42]:总是写入新子目录 `<out_dir>/anet-<ix前12>/`(Mkdir + Lstat,拒绝符号链接);已存在的子目录须属于本 uid、不是符号链接、group/other 不可写,否则拒绝(`out_dir` 可能是 `/tmp` 这类共享目录,别的本机用户可先建同名目录)[redteam:F20];子目录只打开一次,文件相对该目录 fd 创建(`os.Root`,openat 语义),并核对打开的目录与检查的是同一文件,消除"检查后按路径写"期间被替换成符号链接的窗口 [redteam:F20];`out_dir` 本身也要确认打开的就是检查过的目录:其已存在的最深一级在 Linux 上按路径打开后以内核给出的该目录名(`/proc/self/fd`)核对须等于解析后的路径(不需要途经目录的读权限),其他平台从根目录逐级相对打开、任何一级是符号链接或打开前后不是同一文件即拒绝;缺的各级相对已打开的上级逐级创建——否则 `out_dir` 位于他人可写的目录时,检查之后把其中一级换成指向数据目录的链接,子目录与文件就落进检查排除的地方 [redteam:F20];文件以 `O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW` 打开,已存在且内容 CID 相同视为已取回,否则换名;`safeName` 中和前导点、去控制与双向字符、限长;空或相对 `out_dir` 返回 400;`out_dir`、数据目录与 exec 工作目录均先经 `filepath.EvalSymlinks` 解析为真实路径后再比较前缀,拒绝落在后两者之内的 `out_dir`。
8. `/ping` 去掉 `Access-Control-Allow-Origin: *`。运行时目录校验非符号链接、属主、0700,优先 `$XDG_RUNTIME_DIR`。`anet mcp` 在显式选定身份时用严格解析。
9. 策略类写入写 `anet.policy.changed{field, from, to}`。策略类写入(入站策略、公开能力、自动回复,以及支出上限)先保存 `config.json`、成功后才生效:保存失败时运行中的 daemon 保持原状,与 `config.json`、`anet doctor` 一致,之后任何一次成功的配置写入也不会把失败的改动带进磁盘 [redteam:F9]。所有写 `config.json` 的路径(上述策略写入、资料、hub 注册与离开、控制地址)经同一把写锁串行:读取 → 改副本 → 保存 → 成功后只把所改字段写回内存,两次写入同时发生时磁盘与内存仍一致、互不丢失;原子写文件每次用独立的临时文件,并发写不会拼出无法解析的 `config.json` [redteam:F9 复核]。
10. 客户端在确认监听者是本机本用户的 daemon 之前不发送控制令牌(`internal/localpeer`)[redteam:F18]:回环端口任何本机用户都能绑,daemon 未运行时(`anet up` 之前、崩溃或重启后)端口是空的,且端口可预测。Linux 上从 `/proc/net/tcp`、`/proc/net/tcp6` 查**本连接**服务端 socket 的 uid,须等于本进程 uid(检查与发送在同一连接上,无窗口;不需要 daemon 配合,旧 daemon 也能通过);其他平台及读不到套接字表时用挑战-应答:在同一连接上 `GET /ping?proof=<nonce>`,daemon 回 `HMAC(由令牌派生的钥, nonce ‖ 接受该连接的监听地址)`,客户端核对后才发令牌——绑定监听地址使把挑战转给别处真 daemon 的中继无效。CLI 全部命令(含 `anet up` 第一步的探测、不带参数的 `anet`、`doctor`)、`anet mcp`、`/console/switch`(第 4 条)与 a2aprobe 都经此。控制口被占时 daemon 仍换到空闲端口并写 `config.json`(anet 的客户端随之跟随,令牌未外发);占用者不能证明是本用户时(同一连接上的同一核实:Linux 看套接字表 uid,其他平台要求占用者证明持有本令牌——即同一身份的 daemon 被重复启动)同时轮换 `control_token.txt`,运营者选定的端口被占、daemon 因此不启动时也轮换——并非所有控制面客户端都是 anet 的(脚本里的 curl、旧版 CLI),它们在停机期间交给占用者的令牌在新端口上、以及占用者放开后的原端口上都不再有效 [redteam:F18]。
11. r4 新增的控制面路由一律 bearer-only,不在会话白名单内:`/tasks/send|get|list|cancel|wait|reply|pay|pay-manual`、`/agents/list`、`/agents/card`(§12)、`/payments/status`、`/payments/limits`、`/payees/list|add|remove`(§8.6)、`POST /card`(查看本节点网络卡片与 hub 最近一次答复,§10.1)[impl:wp/tasksd ee09872][impl:wp/x402d 2cc279b][impl:wp/cli2 bb43e37][impl:wp/cardgen d1cf8f8]。`/tasks/send`、`/tasks/reply` 请求体上限 96 MiB [impl:wp/tasksd a8396fb]。
12. 回环判定只有一份(无 tag 包 `internal/loopguard`),控制面与本机 A2A 接口共用;身份目录遍历与模块状态路径在 `internal/anethome`。CLI 只把控制令牌发往回环地址:`config.json` 里残留的非回环 `control_addr`(r3 之前 `control_allow_remote` 留下的)一律报错或视为未运行,不带令牌探测。控制面对本机 A2A 令牌一律 401,本机 A2A 接口对控制令牌与控制台 cookie 一律 401,两侧对称测试(SI-7)[impl:wp/loopguard ef748a4、d2b3d40]。

---

## 8. 支付:a2a-x402 同任务流

### 8.1 扩展声明

- URI `https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2`;编入 x402 模块且有标价公开 skill 时声明;全部 skill 收费时 `required: true`,否则省略 `required` 字段(不写 `false`,§10.1 发布形)。经卡片贡献接缝(§10.4)提供。本节点收不了款时(`-tags no_x402` 构建或没有付款模块),标价的公开能力不上卡片(该能力路径会拒绝每一次调用)[impl:wp/cardgen 0fb65f3];`open` 节点的 `chat` skill 从不标价,所以这类卡片不会声明 `required: true` [Q27]。扩展 URI 只在无 tag 包 `internal/x402a2a` 定义一次,卡片声明与任务流的激活识别用同一字符串 [impl:84bc004、f0d81c0]。
- 激活头同时接受 `A2A-Extensions`(按逗号拆分)与 `X-A2A-Extensions`;响应回显"请求的 ∩ 支持的"扩展。激活识别同时认官方参考库(python `x402_a2a`)使用的 v0.1 URI `https://github.com/google-a2a/a2a-x402/v0.1`,只用于识别:认出后按 v0.2 激活,内核收到的与响应回显的一律是 v0.2 URI;anet 只声明 v0.2 [Q18][impl:wp/c3more 9a34c97][impl:wp/fix5 598755c]。

### 8.2 元数据键

| 键 | 位置 | 值 |
|---|---|---|
| `x402.payment.status` | status/message/result metadata | `payment-required` `payment-submitted` `payment-verified` `payment-completed` `payment-failed` `payment-rejected` |
| `x402.payment.required` | provider 的 `input-required` | `PaymentRequired`(x402 v2) |
| `x402.payment.payload` | requester 发往 provider 的付款消息 | `PaymentPayload`(x402 v2) |
| `x402.payment.receipts` | `pay_state` 非空的交互的每条终态消息(含拒付的 canceled、报价过期的 failed、已结算后的 completed/failed/canceled),以及每条 `payment-failed` | `[SettlementResponse]`,全部历史,可为空列表(报价后未付即结束:带 `[]`,不写 `x402.payment.status`)[Q18][impl:wp/c3more 9a34c97、2b8f8af];失败项 `{success:false, errorReason, network, transaction:""}`,hub 失败回执的 `transaction`(被拒授权的 id)移到 `extensions["anet.auth_id"]`,provider 发出的列表与 requester 存下的对端列表都经此规范化 [Q18][impl:wp/c3more 9a34c97];hub 签名收据在 `extensions["anet.settlement.receipt"]` |
| `x402.payment.error` | 失败;已结算后的终态消息同样带(a2a-x402 §9)| a2a-x402 错误码(由 `errorReason` 或 provider 本地核对原因映射,§8.5)[impl:wp/a2ashape 7863633] |
| `anet.payment.accept` | 本机客户端的 `payment-submitted` | 从 `x402.payment.required.accepts` 原样复制的所选项(§8.7) |
| `anet.quote_expires_at` | 报价等待中 | 报价失效时刻(unix 毫秒)[impl:wp/x402d 2cc279b] |
| `anet.reason` | 等待付款或付款失败 | 等待原因 `needs_operator_approval`、`payment_extension_not_activated`(§8.3、§8.7),或失败的原始原因(§8.5)[impl:wp/x402d 2cc279b] |
| `anet.cancel_requested` | requester 的非终态任务 | 付款提交后发出的取消(§4.2)[Q3] |

provider 在结算成功后先发 `payment-verified` 状态再执行能力 [m]。与规范的语义差异:a2a-x402 规范与官方参考实现里 `payment-verified` 表示"验过、尚未扣款"(参考实现的顺序是 verify → 执行 → settle),anet 的 `payment-verified` 表示"已结算、已扣款",商户核对失败时直接从 submitted 到 `payment-failed`,不经 verified(§8.3 的时序是结算成功之后才执行)。对外文档与 §19 草稿写明这一差异 [impl:简报 04 §2][impl:wp/docs 1f59722]。

requester 对本机客户端陈述的是本节点的核验结论,不是 provider 的说法 [redteam:F11]:provider 发来的收据列表在逐项核验(§8.3)之后才存为 `pay_receipts`,每个成功项的 `extensions["anet.settlement_verified"]` 写本节点结论(`verified`/`unverified`,覆盖对端自填的值);投影与状态消息中的 `x402.payment.receipts` 只含失败项(`success` 恰为 `false`)与本节点核验通过的成功项,核验未通过或 `success` 不是布尔值的项列在任务 metadata 的 `anet.unverified_receipts`;`payment-completed` 与 "Payment completed." 只由本节点 `pay_state=completed` 决定,不读交付物的 `paid`,provider 状态消息里的 receipts 与 `payment-completed` 在作为 status.message 呈现时换成本节点的值;对端更长的列表不能挤掉本节点已核验的结算;`pay_state` 为空的任务不输出结算键(provider 状态行作为 status.message 呈现时同样去掉其 `x402.payment.receipts`,不换成空列表);付过款、任务完成而本节点没有核验通过的结算(`pay_state` 仍为 submitted)时,最终消息写"本节点未核验到所付款项的结算",不写"没有结算"[redteam:F11]。复核补充 [redteam:F11]:核验通过要求结算响应的 `transaction` 就是 hub 收据的 AuthID(anet-credit:transaction = auth_id)、`network` 就是收据的 network——同一张真实收据换个交易号重发不再被记成第二笔已核验结算(之前证据链记 `second_receipt`、视图陈述两笔、reconcile 把编造的交易号报成"hub 漏记");已核验项整条由本节点记录的 hub 收据重述(transaction、network、amount、payer、收据字节),不保留 provider 写在旁边的字段;已存列表中带本节点 `verified` 标记的项同样保留(不只依赖核验前读到的证据快照,p2p 与 hub 并发投递时不会被挤掉);同一任务的收据核验逐个进行(读证据、核验、记证据、存列表作为一步),同一结算经两条消息同时送达只记一次;存储的列表最多 128 项,已核验项总在其中;`anet.settlement_verified` 只由本节点写,provider 写在失败项上的会被去掉,`success` 非布尔的项标为 `unverified`;provider 在本节点没有待结算付款(`pay_state` 不是 submitted/completed)时说 `payment-verified`,作为 status.message 呈现时同样换成本节点的状态;`anet reconcile` 不把本节点未核验的对端收据当作 hub 应记的付款。

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
A: 收据核验:AuthID ∈ 本 ix 已签授权集合,PayTo == PeerAID,金额与授权一致,结算响应的 transaction == 收据 AuthID 且 network == 收据 network → 证据;同一 ix 第二张成功收据(另一个 AuthID)写证据并在 audit 标出 [m]
   核验之后才存列表,每项带本节点结论;只有核验通过才置 pay_state=completed(§8.2 末段)[redteam:F11]
   本节点此刻取不到自己 hub 的身份(重启后 hub 未应答、消息经 p2p 到达)时收据无法核验:不记证据、不算"核验未通过",存为 unverified,下次送达时再核验;之前记成未通过,之后同一交易号的送达都按"已记录"跳过,已结算的任务永远停在 submitted [redteam:F11]
```

- 报价 24 小时过期:provider 置 `failed` + `payment-failed`/`EXPIRED_PAYMENT`;未付报价计入按调用方配额;requester 不对过期报价签授权 [m]。
- 标价能力的 TaskDoc 缺 `anet.nonce` → `rejected`、`anet.reason=task_nonce_required`,不报价;长调用槽满时带预付的分支同样先做这项检查 [Q19][impl:wp/c3more 9a34c97、2b8f8af]。
- provider 忙(长调用槽满)时不结算:`payment-failed`、`anet.reason=provider_busy`(映射 `SETTLEMENT_FAILED`),报价仍在;预付调用在忙时先静默记下报价,使这条 `payment-failed` 带上 `x402.payment.required` [impl:wp/x402d 2cc279b、fe49f7a]。`payment-failed` 的状态迁移按调用方给定的起始 `pay_state` 做 CAS:忙拒、商户核对拒绝、报价过期只作用于未取的报价,结算拒绝只作用于 `submitted`,未命中即整笔回滚、不发状态 [impl:wp/x402d fe49f7a]。
- `DelegateReq.Payment`(预付)保留,走同一核对与结算路径。
- requester 重试:同条款重报价时重发尚未过期的同一授权(hub 按 auth_id 幂等);授权过期或条款变化才签新的;每个 ix 至多一个未决授权;发出 payment-submitted 后,在收到该授权的确定结果(payment-completed,或 payment-failed)之前不签新授权 [C13][C25]。已提交付款后收到重报价:auto 档只重发同一授权,条款变化不自动另签,留给人工或 agent 档决定 [impl:wp/x402d fe49f7a]。
- 每个 ix 至多自动付款一次 [Q11][impl:wp/c3more 9a34c97]:auto 档在该任务已签过授权(`pay_auth_ids` 非空)且需要签新授权时拒绝;重发同一授权不算第二次。requester 收到 `payment-failed` 时清空 `pay_payload`,确定失败的授权不再"同条款重发";任务停在 `input-required`,`anet.reason=needs_operator_approval`,由人工档(`anet pay`)或 agent 档(`submit_payment`、本机 A2A 客户端)决定。
- requester 只为付给对端的选项签名:所选项的 `payTo` 不是该任务的对端即拒绝(收据按"PayTo == 对端"核验,付给别人的钱无法证明付的是这件活)[impl:wp/x402d 2cc279b]。
- agent 档超出上限(`agent_max`、`agent_daily_max`、日累计)或收款方不在 payees 名单时,不报错:任务仍为 `input-required`,`anet.reason=needs_operator_approval`(客户端未激活 a2a-x402 时为 `payment_extension_not_activated`,§8.7),`status.message` 按当前策略列出运营者在终端上要先做的步骤(`anet payees add <aid>`、`anet payments set explicit_max=…`/`daily_max=…`、修复不可读的 payees 文件),再接 `anet pay <ix>`;`/tasks/pay` 以 200 返回任务并带 `spend_refusal` 与说明。只有运营者能放行的拒绝这样处理,`zero_amount` 仍作错误;人工、网关、兑付档超限仍 403 [Q26][impl:wp/cli2 bb43e37、4beaf1d]。

### 8.4 商户核对(provider,结算前)

`auth.PayTo == 本节点`;`报价 ≤ auth.Amount ≤ 2^63-1`(账本以 int64 记账,超出的金额在未修复的 hub 上反向记账;凭证兑付同样拒绝超出的金额)[redteam:si9];`auth.InteractionID == pay_bind(ix, task_nonce)`;scheme/network ∈ 已报价选项;授权未过期;报价未过期。不符 → `payment-failed` + 错误码(§8.5 映射表;收款方不符、绑定不符、无待付报价均为 `SETTLEMENT_FAILED` + `anet.reason`,金额不足 `INVALID_AMOUNT`,scheme/network 不在已报价选项 `NETWORK_MISMATCH`,授权或报价过期 `EXPIRED_PAYMENT`),不结算、不执行。`anet.replayed=true` 仅当收据 `AuthID` == 本 ix 已持久化的 `auth_id` 时接受。

实现的核对顺序与补充项(`module/x402` `CheckPayment`)[impl:wp/x402d 2cc279b][impl:简报 04 §5]:scheme 不是 `anet-credit` → `network_mismatch`;载荷解不开 → `malformed_payment`;无待付报价 → `no_pending_quote`;报价已过期 → `quote_expired`;收款方(授权与 `accepted` 两处)不是本节点 → `payee_mismatch`;**授权的付款方不是该任务的请求方 → `payer_mismatch`**(设计原文未列,实现增加:付款须由请求方本人签);绑定不符 → `binding_mismatch`;网络不在已报价选项 → `network_mismatch`;`accepted.amount` 与授权金额不一致或低于报价 → `invalid_amount`;授权不在有效窗口 → `expired_payment`。结算用的 `paymentRequirements` 就是命中的已报价选项,只保留 hub 比较的条款(scheme、network、amount、asset、payTo、maxTimeoutSeconds),不带描述工作的字段(SI-1)。

### 8.5 hub facilitator [C26][C13][C25]

- `CheckRequirements(auth, req)`:比较 `payTo`、`amount ≥ req.amount`、`network`、`scheme`,只解码授权,不需要 KEL。
- 金额范围 [redteam:si9]:线上金额(授权、收据、requirements、discharge)是 uint64,账本是 int64。所有入口只接受 1..2^63-1:`parseAuth`(verify、settle 本地与转发、redeem、网关共用;转发前即拒)、`CheckRequirements`(required amount 超范围为 `invalid_payment_requirements`)、对端收据(`ClearPeerSettlement`、`ClearFromPeer`)、对端 discharge(`SettleOwed`)、`IssueOwedSettlement`、`DischargeDue`、`anet-hub -clear`、网关价格;拒绝为 `invalid_amount`。每处换算经同一个函数(`internal/aghub/amount.go`),不写裸 `int64(…)`;反向换算(已结算重放、兑付回显)遇到库中 ≤ 0 的旧行不重签收据、不回显溢出值。单笔在范围内的金额加上已有值也不得超出 int64:余额、`hub_due`、`hub_owed` 的加减在同一语句里检查结果范围(SQLite 整数溢出不报错而是存成 REAL,账户随后读不出),超出时 `invalid_amount`、整笔回滚;运营者发放同样先检查;入口 hub 对转发结算的对端收据要求金额等于转发出去的授权金额(账本 hub 只按付款方签的金额结算),不按对端写的更大金额记账 [redteam:si9]。部署附只读核查脚本 `deploy/audit-amount-overflow.sql`。节点侧:商户核对、凭证兑付、签名(`Authorize`)同样拒绝 > 2^63-1,支出策略的日累计按饱和加法比较,发放链审计把不是正 int64 的金额列为问题而不计入合计。
- `SettlePayment` 拆成内部 `settleAuth`(无 requirements,供已自行核对的调用方)与公开 `SettleWithRequirements`(`hX402Settle` 用)。
- 入口 hub(network ≠ 本 hub):先 `CheckRequirements`,再把 `{x402Version, paymentPayload, paymentRequirements}` 转发给账本 hub;回执 `PayTo/Amount` 与 requirements 一致才 `ClearFromPeer`。账本 hub:`decodeAuth` 后再 `CheckRequirements`,`paymentRequirements` 必填。
- `Redeem` 保留 `payTo == hubAID` 检查并调用 `settleAuth`;网关以 `{payTo: aid, amount: price}` 构造 requirements,凭证金额取结算额。
- `/x402/verify`:账本不在本 hub 时返回 `network_mismatch`(不转发)。
- 重放查找移到签名验证之后、有效期检查之前:已扣款的 auth 任何时候都返回原回执。
- `UNIQUE(payer, interaction_id) WHERE interaction_id != ''`;冲突且 auth_id 不同 → `Success:false`、`errorReason=duplicate_binding`,附原结算交易号;auth_id 相同 → 原 replay 行为。结算失败事务回滚。
- 跨 hub 传输错误、超时、"对端已结算本地未清算" → `settlement_pending`(非终结),保留对端回执并重试 `ClearFromPeer`(按 auth_id 幂等)。
- `errorReason` 取 x402 层小写常量,补齐发出点:`insufficient_funds` `invalid_signature` `expired_payment` `network_mismatch` `unsupported_scheme` `invalid_amount` `payee_mismatch` `duplicate_nonce` `duplicate_binding` `unknown_payer` `settlement_pending` `settlement_failed`。hub 实际还会发 `malformed_payment`(载荷或授权解不开)与 `invalid_payment_requirements`(缺或坏的 `paymentRequirements`),ANetCore 另保留已废弃的旧拼写 `expired`(现行 hub 不发)[impl:简报 04 §1.1、§2]。失败的 `SettlementResponse.transaction` 是被拒授权的 id(不是空串),daemon 按 §8.2 规范化 [Q18]。`module/x402` 映射为 a2a-x402 的 `x402.payment.error`,映射表覆盖 ANetCore 的全部 errorReason 与 provider 本地核对的原因,未列出的原因一律 `SETTLEMENT_FAILED`,两侧钉字符串 [impl:wp/x402d 2cc279b、fe49f7a]:

| hub errorReason / provider 本地核对 | x402.payment.error |
|---|---|
| `insufficient_funds` | `INSUFFICIENT_FUNDS` |
| `invalid_signature` | `INVALID_SIGNATURE` |
| `expired_payment`、`expired`(旧拼写)、`quote_expired`(报价过期) | `EXPIRED_PAYMENT` |
| `duplicate_nonce`、`duplicate_binding` | `DUPLICATE_NONCE` |
| `network_mismatch` | `NETWORK_MISMATCH` |
| `invalid_amount` | `INVALID_AMOUNT` |
| hub:`payee_mismatch`、`unsupported_scheme`、`unknown_payer`、`settlement_failed`、`malformed_payment`、`invalid_payment_requirements`;provider:`binding_mismatch`、`no_pending_quote`、`payer_mismatch`、`provider_busy`;本机客户端(§8.7):`client_payload_unsupported`、`option_not_offered`、`rail_not_payable`(Q28)| `SETTLEMENT_FAILED`,原始原因放入 `anet.reason` |
| `settlement_pending` | 不映射(非终结,不发 payment-failed) |

结算应答非 2xx 且不带 `errorReason` 时按"结果未知"处理,用同一 payload 重试;首次结算成功而本地记账失败时同样进入重试循环 [impl:wp/x402d fe49f7a]。

- `/supported` 增加 `extensions`、`signers`。

### 8.6 支出策略(唯一执行点)[C24][C27]

```json
"payments": { "auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
              "explicit_max": 10, "daily_max": 50, "payees_file": "payees.allow" }
```

可选键 `publish_prices`(缺省 `true`):为 `false` 时网络卡片不带 anet-pricing、ADP 卡片不带价格表,价格只在 E2E 报价中给出;取舍见 §21 第 9 条 [redteam:F1]。

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
- `payees_file`:键非空即启用白名单,文件缺失等于空表;`anet init` 创建空文件。`redeem`(收款方为 hub AID)不受 payees 约束,受人工档上限。名单经 `anet payees list|add|remove`(控制面 `/payees/*`)管理,`add` 要求 TTY 确认,每次变更写 `anet.policy.changed`;名单关闭(`payees_file` 为空)时编辑一律 409,CLI 在提问前就说明 [impl:wp/cli2 bb43e37、566709d]。
- 拒绝的形状:签名路由(`/tasks/pay-manual`、`/x402-authorize`、`/delegate` 的 `pay:true`、`/redeem`)的支出策略拒绝为 403 + `reason`;agent 档的 `/tasks/pay` 例外,按 §8.3 答为仍在等待的任务(200 + `spend_refusal`)[impl:wp/c3more 9a34c97][Q26]。
- 兑付的收款方核对:`/redeem` 必带 `pay_to`(`anet redeem` 在终端上给运营者看的 hub AID),与本节点此刻要签给的 hub 不一致即 409(`payee_mismatch`)、不签名;x402 模块的 hub 身份与可清算网络按 `HubURL` 缓存,换 hub 后签给新 hub。`anet redeem` 需 TTY 确认,提示金额、hub AID、人工档上限与 24 小时已签额;未编入支付模块的构建在打开终端之前就报错 [impl:wp/fix5 598755c][impl:wp/cli2 bb43e37][impl:integ/round4c 66875ed]。
- 上限写入(`/payments/limits`,`anet payments set <limit>=<n>…`)保存失败时回滚内存中的上限,不写 `anet.policy.changed`;确认提示逐项给出"旧值 → 新值";CLI 在打开终端之后才向 daemon 读报价或当前上限,无终端时不发任何请求 [impl:1b131c2]。
- TTY 确认在 CLI 进程内执行,daemon 只校验控制令牌,无法区分调用是否经过 TTY。
- 付费演示需要用户先把演示 AID 加入 payees(`anet payees add`)并在 TTY 上放开 agent 档上限;install.sh 打印的示例用免费能力。

### 8.7 本机 A2A 客户端的付款消息 [C28]

- 本机 daemon 是 a2a-x402 §5.1 所说的签名服务。本机客户端在同一 taskId 上发 `x402.payment.status: payment-submitted`,**不带** `x402.payment.payload`;以 `anet.payment.accept` 给出从 `x402.payment.required.accepts` 原样复制的所选项(只有一项时可省略)。
- daemon 核对所选项与本 ix 存储的某个 `accepts` 项相同,按 agent 档上限签授权,在 E2E 信封内转发标准 x402 v2 `PaymentPayload`。"相同"的实现口径:两边都经规范化 JSON(对象键排序、数字按原文保留、不做任何增删)后逐字节相等,即逐字段相同且不多出成员;只有一项时可省略所选项,多项且省略时由节点按可用通道选择 [impl:wp/x402d 2cc279b(`offeredOption`)]。
- 客户端自带 `x402.payment.payload` → `x402.payment.status: payment-failed`、`x402.payment.error: SETTLEMENT_FAILED`、`anet.reason=client_payload_unsupported`(其付款方不是本节点,转发也无法结算);所选项不在 accepts → 同上,`anet.reason=option_not_offered`。这两种拒绝返回任务本身(带上述 `payment-failed` 状态消息),不是协议错误;什么都不签、不发、不存,报价仍在 [impl:65cbcdb]。无报价、已有未决付款、报价已过期、任务已终止、未编入付款模块 → `UnsupportedOperation`。
- 付款选项顺序与不可付选项(0017 Q28):daemon 转交本机客户端(A2A/MCP、控制面任务视图)的 `x402.payment.required.accepts` 按本节点可付排序——本节点账本所在网络(hub 身份已知时)的选项在前,其余按原顺序在后;每个选项的内容不改,存储的报价仍是 provider 的原样与原序。客户端选了本节点不能付的选项(不在本节点账本上的网络)时,在签名前拒绝:`payment-failed`、`SETTLEMENT_FAILED`、`anet.reason=rail_not_payable`,`status.message` 写明本节点账本与可付的选项(没有则写明无法支付此报价);未签名、未发送,报价仍可再付。自动档与未指定选项时同样只选本节点账本上的选项。本节点此刻取不到自己的账本(自启动以来 hub 未应答)时,任何选项都按不可付处理:同样 `rail_not_payable`,`status.message` 说明原因,不签名,hub 应答后可再付;之前这种情况下照客户端所选(未指定时是 provider 排在第一的选项)签名,交给那个账本的 hub 决定。预付路径(`PayAndRetry`)同样检查 [redteam:Q28]。
- 付款消息只走内核一条路:module/a2a 不截获 x402 消息,SendMessage/SendStreamingMessage 一律交给 `TaskSeam.Send`,内核识别付款消息后调用与 `/tasks/pay` 同一实现(purpose `task-agent`)。因此客户端的 `(contextId, messageId)` 去重对付款消息同样生效:同一 messageId 的付款消息重发只签一次授权(客户端 messageId 记在本地付款消息的元数据,不随信封外发)。流式发送遇到拒绝或被搁置(§8.3 agent 档超限)时发出该任务后即结束流 [impl:65cbcdb][impl:wp/cli2 bb43e37]。付款消息的阻塞等待以决定前的 `state_seq` 为界(提交后转 working 不算中断态)[impl:dc3eefa]。
- 代理卡片的 x402 声明不设为必需:省略 `required` 字段,不写 `"required": false`(proto3 普通 bool 的默认值,A2A §8.4.1 要求省略;写出会使按 proto 语义重建载荷的验证方得到另一份签名原像,见 note 0012),params `{signer:"anet-daemon", clientPayload:false}`;不论客户端是否激活扩展,代理任务上都出现 x402 状态键。客户端未激活时:auto 档内照常自动付款,超出时 `input-required` + `anet.reason=payment_extension_not_activated`(内核给的是 `needs_operator_approval`,本机 A2A 接口对未激活的请求改说成前者;对付款消息本身的应答除外)[impl:wp/cli2 bb43e37]。激活识别见 §8.1(同时认 v0.1 URI)[Q18]。

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

生产数据清理清单(脚本随版本提供,执行前征求同意):emax、fmax 的 `relay_message` 明文行与周备份;`admin/datasets/<全部源>`;`admin.db` 的 `session` 全部行与 `harvest_state`;`data/taskboard.db`(含 `-wal`、`-shm`);已公开评价中的 goal/deliverable;以上在备份中的副本;ai-studio 在 emax/fmax 上的 manifest(删除 `datasets/monitor/ops` 或整体删除)。已经通过 `/fed/v1/reviews` 流出的内容无法收回。实现为 ANetHub `deploy/cleanup-content-v0.2.sh`(九步,缺省只报告;`--apply` 先出完整报告,再要求在 `/dev/tty` 键入 yes 或给 `--yes`),第 9 步报告并可删除旧 admin 的运维凭证通道(登录官方 agent 主机的 ssh 私钥、`ADMIN_MONITOR_TOKEN`),需在其他主机上做的事项打印成清单;已迁移库的 wire-1 丢弃计数读 `hub_meta`(§3.7)[impl:ANetHub wp/hubops 030f22d、e6df2ec]。

去除内容后 hub 仍然可见的元数据写入对外文档:agent 自述(卡片、KEL、keys、profile;其中 KEL 与 keys 对所有对等 hub 按精确 AID 可查,与 hub-local 可见性无关,见 §3.9;可见性文档同步写明)、评价关系图、p2p 地址、活跃度、付款元数据、公开发放链(§21)。

---

## 10. A2A 卡片与注册表

### 10.1 网络卡片

- 只有至少一个 skill 的节点发布网络卡片。skill 有两种来源:本节点在服务的公开能力(`public_capabilities`,§10.2);`inbound.policy=open` 时再加一个 `chat` skill,表示接受自然语言任务(`inputModes`/`outputModes` 为 `text/plain`,从不标价,description 如实说明谁能发这类任务、hub 只搬运密文;本节点恰有 id 为 `chat` 的能力时不加)[Q27][impl:wp/fix5 598755c]。全新安装(closed、无公开能力)不发布。卡片签名、发布与复用:注册时签发,内容不变时复用上次签出的卡(重复注册答 `unchanged`,`seq` 不增长);hub 答 `conflict` 时以新 `seq` 重签一次;修改公开能力、简介或进出 `open` 时重新发布;`POST /card` 查看本节点的卡与 hub 最近一次答复 [impl:wp/cardgen d1cf8f8、0fb65f3]。
- 撤回:发过卡的节点失去全部 skill(公开能力清空且不再是 `open`)时,注册请求带 `"a2a_card": null`,hub 删除卡片与索引行,名称与 caps 回到本次注册声明的值,答 `card_status: "withdrawn"`(可重复);联邦可见的 agent 另在 `/fed/v2/cards` 写撤回条目(§10.6)。`a2a_card` 缺省仍表示"不变",刷新注册不会误撤。节点把 hub 的确认记在 `a2a_card.json`(`withdrawn_from`),重启不重发;再次发卡时清除。撤回只随注册发出,hub 不可达时不单独重试,等下一次注册(§21)[Q6][impl:wp/proj 78b4963][impl:ANetHub wp/proj 6300c7f]。
- 注册的 `caps` 与 ADP 卡(及价目)同样只含 `public_capabilities` 中的能力,不暴露私有能力清单;配置里运营者声明的 caps 原样保留 [Q14][impl:wp/proj 78b4963]。
- `supportedInterfaces`:中继绑定 `{url: <hub 公网基址>/relay, protocolBinding: "https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1", protocolVersion: "1.0", tenant: <AID>}`;p2p 启用且运营者配置了 `advertise` 地址时追加直连条目 `{url: "tcp://<host>:<port>"(由 `advertise` 规范化;本地 socket 与缺 host/port 的值启动即拒绝)[impl:wp/cardgen d1cf8f8][B6-02 复核], protocolBinding: "https://agentnetwork.org.cn/a2a/bindings/anet-p2p/v1", protocolVersion: "1.0", tenant: <AID>}`(与中继同样按 tenant 路由、承载同样的封装信封)[Q8][impl:wp/cardgen d1cf8f8]。"127.0.0.1 不进入网络卡片"只约束 p2p 直连地址,不约束 hub URL(本机联调的 hub 可以是回环)[Q8]。内核丢弃回环、他人 tenant 或畸形的模块贡献,拒绝第二个中继接口;p2p `advertise` 的端口须为数字且在范围内 [impl:wp/cardgen 0fb65f3]。
- `securitySchemes` 省略(认证由绑定内的发送方签名承担);不定义新 scheme 类型。
- `capabilities.extensions`:`…/anet-card/v1`(`params{aid, seq:"<字符串>", issuedAt, notBefore}`)、a2a-x402 v0.2、`…/anet-pricing/v1`(`params{network, prices:[{skillId, amount:"<字符串>"}]}`;`payments.publish_prices=false` 时不发布,只在 E2E 报价中给出价格,见 §21 第 9 条 [redteam:F1])、`…/anet-evidence/v1`。anet-card 与 anet-evidence 由内核写入,模块贡献中出现这两个 URI 或中继绑定一律丢弃 [impl:wp/cardgen d1cf8f8]。
- 数值一律字符串;必填切片非 nil;`streaming`、`pushNotifications` 显式输出(网络卡片写 `false`)[impl:wp/cardgen d1cf8f8]。
- 卡片 `name` 取本次注册所用的名字(显式 `hub-register` 的名字在 hub 接受后才写入配置,卡与同一次注册的 ADP 卡必须一致);首次 `hub-register` 在配置记下 hub 之后再发布一次,使付款模块能按 hub 找到账本、把 a2a-x402 与价目写进卡 [impl:wp/cardgen 0fb65f3]。
- 发布形(`a2acard.CheckPublishForm`,`Sign` 只接受发布形,不自动改写):REQUIRED 字段必须出现且非空(REQUIRED 数组至少一项:`supportedInterfaces`、`defaultInputModes`、`defaultOutputModes`、`skills`、每个 skill 的 `tags`;`name`/`description`/`version`、接口的 `url`/`protocolBinding`/`protocolVersion` 非空);`optional` 字段仅在显式设置时出现,且不为空串;其余字段处于默认值(`false`、`""`、`[]`、`{}`、`null`)时一律省略;不出现 schema 以外的成员;扩展 `params` 内部不出现 `null`/`""`/`[]`/`{}`(a2a-python 会在 Struct 内部删除它们,规范不删,两边原像不同);oneof 消息(`SecurityScheme`、`OAuthFlows`)恰好设置一个成员(a2a-python 与 a2a-go 都拒绝解析设置了两个的对象);a2a-go 每次序列化都写出的成员必须出现(`capabilities.streaming`/`pushNotifications`,以及已弃用的 implicit/password OAuth 流的 `authorizationUrl`/`tokenUrl`/`scopes`),否则 a2a-go 解析—再序列化后载荷改变;`extendedAgentCard` 出现时只能为 `true`。扩展声明用 `a2acard.ExtensionDecl` 构造。

### 10.2 skills 来源

- C1 可选接口 `provider.Described{ SkillInfo(capability) (SkillInfo, bool) }`,未实现时由 id 派生 name 与 tags(`a2acard.DefaultSkillName`/`DefaultSkillTags`)。`description` 是 REQUIRED,不得为空串:缺省取 `a2acard.DefaultSkillDescription(id)`(如实写明提供方未给描述);`a2acard.Skill.WithDefaults` 一并补齐。`provider.SkillInfoOf` 按 a2acard 的卡片上限截断,保证结果总能进卡片;`provider.MaxSkill*` 与 a2acard 上限由测试防漂移 [impl:wp/official 6f1deca、79127ac]。
- `service` 模块配置每个能力的 `name/description/tags/examples/input_modes/output_modes` 并实现 `Described`,加载时按卡片上限校验 [impl:wp/official 6f1deca]。
- 只发布 `public_capabilities` 中、本节点在服务的能力;标价能力在本节点收不了款时不发布(§8.1)[Q14][impl:wp/cardgen 0fb65f3]。`open` 节点另加 `chat` skill(§10.1)[Q27]。

### 10.3 签名(ANetCore `a2acard`,标准库实现)

- JWS EdDSA,签名钥为 KEL 当前钥;保护头 `{"alg":"EdDSA","jku":"https://<hub>/agents/<AID>/jwks.json","kid":"did:anet:<AID>#<seq>","typ":"JOSE"}`。`jku`/JWKS 是 hub 的陈述,可信度低于 KEL,文档写明 [m]。
- 规范化(A2A §8.4.1):先按 a2a.proto 的字段存在性去掉默认值(隐式存在字段的 `""`/`false`/`[]`/`{}` 与非 REQUIRED 字段的 `null`;REQUIRED 字段保留;`optional` 与消息字段设置了就保留;`google.protobuf.Struct` 内部不动;未知成员保留以受签名覆盖),再去掉顶层 `signatures`,再 RFC 8785(含 ECMAScript 数字序列化)。字段表在 `a2acard/schema.go`,与 a2a-python 的 proto 描述符导出结果对照测试。签发只接受发布形(§10.1),发布形上"去默认值"与"原字节"两种载荷相同。验签先按去默认值形式,失败再回退到原字节形式(只去 `signatures`,兼容 a2a-go 签发、线上保留默认值的卡片),结果记录 `CanonicalForm`(`proto-stripped`/`raw`)。存储与转发用原字节;同一陈述的不同字节形(例如多了 `"required": false`)`PayloadHash` 相同(取去默认值形式的哈希),高水位与去重按 `PayloadHash` 比较,不按字节。
- 验证:`signatures` 为空即拒绝;kid 解析;KEL 回放;只接受顶端活跃密钥态;`params.seq` 三分支高水位(相等时规范化载荷须相同);`notBefore ≤ now + 300s`;尺寸与必填项(整卡 ≤ 64 KiB,name ≤ 128 字节,description ≤ 4096 字节,skills ≤ 256,每 skill tags ≤ 16,签名 ≤ 8 个;REQUIRED 字符串非空、REQUIRED 数组至少一项,同 §10.1 发布形所列)。拒绝以 a2acard 错误码报告(`UNSIGNED`、`BINDING_MISMATCH`、`KEL_UNAVAILABLE`、`KEY_NOT_CURRENT`、`INVALID_SIGNATURE`、`CARD_NOT_YET_VALID`、`SEQ_ROLLBACK`、`SEQ_FORK`、`NOT_PUBLISH_FORM` 等)[impl:ANetCore wp/a2acard]。
- 偏离规范的 a2a-python 卡片(Struct 内空值、空 scope 列表、REQUIRED 空串)按规范拒收,不做 python 形回退(在 issue 草稿中向上游报告);hub 准入只收发布形卡片 [Q7]。
- daemon 发现卡片时同样执行 `params.seq` 三分支高水位:有 `peer_identity` 行的对端读写该行(重启不丢),其余对端按节点与 agent 记在内存;hub 重放更旧的卡或同 seq 另一张卡时该卡按 UNVERIFIED 处理,分叉计数并记日志 [impl:wp/tasksd a8396fb][impl:wp/proj 78b4963]。
- 验证不通过的卡片不外传:daemon 只保留 AID、`UNVERIFIED` 与 a2acard 错误码(不带错误细节,它可能引用卡片或 hub 的文字),卡片字节、名称与 hub 的其余陈述一概丢弃(§10.5)[Q24][impl:wp/fix5 598755c]。
- KEL 解析器(实现 `a2acrypto.KeyResolver`)放在 `module/a2a/kelresolver`,文件带 `//go:build !no_a2a`,只供 `module/a2a` 与契约测试导入;daemon 内核的卡片验证只用 ANetCore `a2acard`。按 kid 取 KEL、回放、核对 AID 后返回顶端公钥 [m]。
- 契约测试:a2a-go 验证 `a2acard` 签出的卡片;a2a-go 解析—再序列化后签名仍有效;a2a-go 自带的 Ed25519 金标向量由 `a2acard` 验证。a2a-python 金标向量(`a2acard/testdata/python-vectors.json`,由同目录 `gen_python_vectors.py` 经 proto → `MessageToDict` → 签名生成):网络卡片(含扩展 params、多个 skill)、同一卡片"线上带默认值"的变体、a2a-go 式原字节签名变体、代理卡片形状,以及三张 a2a-python 与规范分歧的卡片(Struct 内空值、空 scope 列表、REQUIRED 空串;`a2acard` 按规范拒绝,发布形排除)。

### 10.4 模块向卡片贡献内容

```go
type CardContext struct {
    AID    string   // 本节点身份,接口按它路由(tenant)
    HubURL string   // 卡片发布到的 hub,不带尾部斜杠
    Skills []string // 卡片发布的 skill id,已排序;从不为空
}

type CardContributor interface {
    CardExtensions(c CardContext) []map[string]any   // 内核不 import a2a-go
    CardInterfaces(c CardContext) []map[string]any
}
```

与 r3 的偏离:两个方法带 `CardContext` 参数。x402 模块要知道卡片发布哪些 skill,才能判断"全部收费则 `required: true`"(§8.1)并只为公开 skill 报价;哪些能力公开是内核配置(`inbound.public_capabilities`),传参使模块不读内核配置、内核也不必知道价目扩展的形状;另一做法(给 `Host` 加 `PublicCapabilities()`)扩大 Host 接口,比传参宽 [impl:wp/cardgen d1cf8f8][impl:简报 06]。贡献是普通 JSON 对象:扩展 `{"uri", "description"?, "required"?, "params"?}`,接口 `{"url", "protocolBinding", "protocolVersion", "tenant"?}`;内核按发布形放入卡片,处于默认值的成员删除,`params` 内每个值须为非空字符串、布尔或它们组成的非空对象/数组 [impl:wp/cardgen d1cf8f8]。

x402 模块贡献 a2a-x402 与 anet-pricing;p2p 贡献直连接口。`no_x402` 构建卡片中无 x402 URI。

### 10.5 hub 注册表 API

| 端点 | 行为 |
|---|---|
| `GET /a2a/v1/agents?skill=&tag=&q=&cursor=&limit=` | 只返回 `Browsable` 且卡片验证 OK 的条目;条目 `{aid, card(原字节), cardVerification, verifiedAt, homeHub, lastSeen, quiet, reviewCount, avgRating}`(`cardVerification` 取值 `"ok"`);包装层是 hub 陈述。列表 `{agents, nextCursor}`;`limit` 缺省 50、上限 200;`cursor` 不透明;`q` ≤ 256 字节(只供 web UI);空的 `skill=`/`tag=`、非法 `limit`/`cursor` 回 400;`homeHub` 取 hub 的 `-public-url`(缺省回退请求来源)[impl:ANetHub wp/hubreg 23ed2c8、c3204c8][impl:ANetHub wp/proj 6300c7f] |
| `GET /a2a/v1/agents/{aid}/card` | 原字节;`ETag`(sha256 hex)、`Cache-Control: max-age=300`、`If-None-Match` 命中 304;无卡或未验证 404。`POST /a2a/v1/agents/card:lookup`(`{aid}` 在请求体)答复相同,daemon 用它,请求行不含对端 [redteam:F3] |
| `GET /agents/{aid}/jwks.json` | 由 KEL 推导,只含活跃密钥态;同样带 ETag 与 304;推导结果按存储 KEL 字节的 SHA-256 缓存(KEL 变了即另一个键),未鉴权的重复读取不再逐次回放 KEL [redteam:F36] |

索引 `agent_skill`、`agent_tag` 只在卡片准入成功后重建;现有 `/agents` 形状不变,有 A2A 卡的条目的 name/caps 从已验证卡片派生(准入时覆盖 `agent.name` 与 `agent_cap`)。每次注册都用刚写入的 KEL 重验已存卡片:KEL 轮换后旧钥签的卡得 `KEY_NOT_CURRENT`,从注册表下架直到 agent 重签;卡片中继接口指向的 hub 与本 hub 基址不一致只记日志,不拒收 [impl:ANetHub wp/hubreg 23ed2c8][impl:ANetHub wp/proj 6300c7f]。卡片与 JWKS 端点允许浏览器条件请求(CORS 暴露 `ETag`、放行 `If-None-Match`)[impl:ANetHub wp/hubreg c3204c8]。

daemon 侧的发现(控制面 `/agents/list`、`/agents/card`,即 MCP `list_agents`、`get_agent_card`,以及本机 A2A 接口的 `GET /a2a/v1/agents`)[impl:wp/tasksd ee09872]:
- 自由文本查询:daemon 按 skill/tag 从 hub 取卡片、在本机验证并与所列 AID 绑定后,本地做子串匹配,不把自由文本发给 hub;hub 的 `q` 参数只供 web UI [m]。因为逐页本地过滤,一页可能为空而 `nextCursor` 未尽。
- 输出字段:本节点自己的核验结果写作 `verification`(`VERIFIED` / `UNVERIFIED` / `NONE`),hub 的 `cardVerification` 另写作 `hubVerification`,后者不能代替前者 [impl:wp/tasksd ee09872][Q24 补充]。`aid` 不是合法 agent id 的条目一律丢弃,防止 hub 在 aid 位置注入文字 [impl:wp/fix5 7113a2e][Q24 补充]。
- 按验证结果决定外传什么 [Q24][impl:wp/fix5 598755c]:
  - `VERIFIED`:卡片原字节、名称,以及 hub 对该 agent 的陈述(`homeHub`、`lastSeen`、`quiet`、评价数与评分);
  - `UNVERIFIED`(有卡而本节点验不过):只给 AID、`verification`、`verificationError`(a2acard 错误码,或本节点的固定说明如 "no card"、"the card is signed by <kid AID>, not <AID>",从不带错误细节)与按 AID 判定的 `anet.official` [impl:wp/fix5 598755c][B6-02 复核];卡片内容(name/description/skills 等)与 hub 的其余陈述一概不给;自由文本只匹配已验证卡片;这样官方标注不会出现在 hub 伪造的内容旁边;
  - `NONE`(不发布网络卡片):只在查询带 `include_uncarded`(缺省 false)时列出,见下条。
- `include_uncarded`(Q27 (b)):为 true 时,在注册表最后一页之后附上 hub `/agents` 目录中注册表未列出的已注册条目,`verification: "NONE"`,`name`/`caps`/`summary` 等字段只取 hub 陈述并标明"除 aid 外都是 hub 的陈述";官方 AID 的无卡条目不带 hub 文字(同 Q24 的理由);判断"有卡片"时注册表只按 skill 遍历,不按 tag(目录只按 capability 查、没有 tag);分页游标形如 `uncarded:<n>`;无注册表的旧 hub 只在 `include_uncarded` 时列目录。本机 A2A 接口从不设它 [Q27][impl:wp/fix5 598755c、7113a2e]。
- v1 的 `/find`(hub 目录的子串查询与按 capability 查询)同样适用 Q24:hub 目录的 name/summary 不与 `anet.official` 并列出现,官方 AID 条目只给 AID 与标注,或只在其卡片 VERIFIED 时给内容;沿用 `verification`/`hubVerification` 字段名;非法 AID 条目丢弃 [Q24 补充](实现待补:当前 `/find` 原样返回 hub 目录条目并按 AID 加标注)。
- 官方标注:AID 在本二进制内嵌、以发布钥签名的官方清单上时,条目带 `"anet.official": true`(键缺省而非 false),只按 AID 判定,hub 的任何说法都不构成官方(§15)[impl:wp/manifest 2ef6c44]。

### 10.6 联邦 v2 卡片

`GET /fed/v2/cards` 条目 `{format:"a2a-card/1", card, kel, keys, home, fed_seq}`;按 format 分派准入;KEL 延伸规则适用;`home` 与卡片接口 URL 不一致时以卡片为准并记录。v1 端点保留到所有对端升级。

实现补充 [impl:ANetHub wp/hubreg 8aa7012、f37a65d][impl:ANetHub wp/proj 6300c7f、3cf9523]:
- 第二种 format `withdrawal/1`(对本节的扩展):本 hub 停止发布某张卡时的撤回条目(注销、可见性收窄、KEL 轮换后卡片不再验证、agent 以 `a2a_card: null` 撤回、运营者删除),`card` 为撤回对象,无 `kel`/`keys`。format 由发布 hub 写、不在 agent 签名内,agent 无法把自己的卡伪装成撤回。撤回只由教授该卡的对端生效,且不删行:置未列出、清 keys 与索引,保留高水位与 KEL,使该对端不能把更旧的卡或更短的 KEL 当作新卡重发。拉取方对未知 format 跳过且游标前进。
- 流位置单调:卡片与撤回共用一个只前进的 `fed_seq` 流头(单条 `UPDATE … RETURNING` 取号)。每页至多 100 条且按 4 MiB 截断,拉取方读取上限 16 MiB;对端 404(未升级)不算拒绝、游标不动。
- 准入顺序:KEL 解码回放出 AID → 与本地准入共用的卡片验证(他人卡片 `BINDING_MISMATCH`,skill id 过能力名校验)→ 该 AID 在本 hub 注册则暂拒(`ErrRefusedForNow`)→ KEL 延伸 → 卡片高水位 → 密钥集入库。验证放在"本地已注册"检查之前,伪造条目是永久拒绝而不会停住对端的流。
- `home` 取卡片 anet 中继接口的 url 去掉 `/relay`(只留 scheme/host/path,须为 http(s));卡片无中继接口时用条目 `home`(须为绝对 http(s) URL、有 host、无用户信息、≤ 2048 字节,否则改用对端 endpoint)。
- 联邦卡进入注册表与目录:discovery 联邦开启时 `/a2a/v1/agents` 与卡片端点并入已列出的联邦 A2A 卡(本地已注册的 AID 不列联邦副本),`/agents` 的 name/caps 取卡片;`GET /agents/{aid}/keys` 的第二来源先查联邦 A2A 卡的 keys。只经 v2 认识的外来 agent 的评价同样联邦出去;本地注册的被评方只按自身可见性判断是否外发。KEL 在 v1、v2 两个来源中取较长者。

---

## 11. 本机 A2A 接口(`module/a2a`,`//go:build !no_a2a`)

### 11.1 形态

- 配置块缺省时模块照常启用(本机接口是默认产品面);`anet init` 不写 `modules.a2a` 块(否则 `no_a2a` 变体加载配置会失败)[C43]。`modules.a2a` 严格解码(未知键报错);宿主不给 `TaskSeam` 时不监听、不报错;`StateDir` 为空时拒绝启动 [impl:wp/moda2a 85afc91]。
- 端口稳定:首次启动从固定基址 **43811** 起扫描 2000 个端口(43811–45810;控制面的扫描段是 39811–41810),跳过 `ANET_HOME` 下其他身份的 `modules/a2a/a2a_addr.txt` 记录的端口(与控制面分配器同一遍历 `anethome.Identities`),绑定即占用;选定后写入模块状态目录的 `a2a_addr.txt`(0600,rename 原子替换);之后每次启动重绑该端口;记录的端口被占时不换端口(下条)[redteam:F18],非回环地址拒绝启动 [impl:wp/moda2a 85afc91、f0b763b][impl:wp/loopguard ef748a4]。基址由实现初稿的 41811(紧接控制面扫描段)改为 43811,与简报 07 §7.7、计划 0014 B3-07 一致 [impl:wp/moda2a f0b763b]。联调脚本与测试网在各自端口段内预写 `a2a_addr.txt` 钉住端口(`scripts/lib.sh` 的 `pin_a2a`),否则 daemon 会在段外自选 [impl:wp/testrun 87d2048]。
- 记录的端口被占时**不换端口** [redteam:F18]:已写入的客户端(Hermes `a2a_agents`、脚本)不核实监听者,换端口后它们会一直把 Bearer 令牌发给旧端口上的占用者,而令牌在新端口上仍然有效。改为:本机 A2A 接口不启动(daemon 其余部分照常运行——否则占一个端口就能让节点停摆),写 `a2a_port_conflict.txt`(端口、占用者、令牌是否已轮换)并记 ERROR 日志,`anet up` 在终端上警告,`anet doctor` 以 fail 报告;占用者不能证明是本 uid(套接字表显示其他 uid,或本系统无法判定)时同时轮换 `a2a_token.txt`,客户端可能已发出的旧令牌作废。处理顺序:释放端口 → 重启节点(`anet stop && anet up`)→ `anet agents wire --refresh`。成功绑定时删除冲突记录。套接字表显示记录端口上有其他 uid 的监听者时,或冲突记录存在而端口此刻不由本 uid 持有时(套接字表显示本 uid 持有即视为已恢复,例如同一节点的重复启动留下的记录),`anet agents wire`(含 `--refresh`)拒绝写入 A2A 条目 [redteam:F18]:Hermes 下一次调用就会把令牌发往该端口,先刷新会把刚轮换的新令牌交给占用者,占用者放开端口、节点重启后接口正以这枚令牌起来,轮换形同虚设。
- 端口必须改时(`a2a_addr.txt` 缺失而令牌已存在)新选端口并同时轮换令牌 [redteam:F18]。
- 状态文件在 `<数据目录>/modules/a2a/`(即 `Host.StateDir("a2a")`):`a2a_addr.txt` 与令牌 `a2a_token.txt`(32 字节随机 hex,`O_EXCL` 首次生成,0600,拒绝符号链接)。`StateDir` 按 `modules/`、`modules/<name>/` 两级建为 0700,遇符号链接或他人 uid 的目录拒绝并返回空串 [impl:wp/moda2a 85afc91][impl:wp/a2ashape 7863633]。路径只在无 tag 包 `internal/anethome` 定义一次(`A2ADir`、`A2AAddrFile`、`A2ATokenFile`、端口冲突记录 `A2AConflictFile`),写方(模块)与不经 daemon 的读方(`anet doctor`、`anet agents wire`)用同一定义 [impl:c497e4b][impl:wp/loopguard ef748a4]。
- `module.Host` 增加 `StateDir(module string) string`(模块自有状态目录)与 `TaskSeam() (TaskSeam, bool)`,理由写在接口注释。另有两个按类型断言取得的可选接口,不扩大 Host [impl:wp/moda2a 85afc91、6195f26]:
  - `ProxyCardSigner.SignProxyCard(card []byte) ([]byte, error)`:以本节点 KEL 当前钥签代理卡片(kid `did:anet:<本节点 AID>#<seq>`,无 `jku`);只签 AgentCard,拒签带 anet-card 扩展、中继绑定或非回环 http 接口的卡,模块因此造不出 hub 会当作本节点身份的卡;
  - `InboundTaskHost`:入站文本任务交给提供侧后端(§11.6)。
  测试用的 Host 桩统一嵌入 `module/moduletest.NopHost`,Host 加方法只改一处 [impl:wp/hyg 4428bc3]。daemon `Close` 在长调用排空之后、关闭证据账本之前停止各模块,模块停止时仍可记证据 [impl:wp/hyg 4428bc3、f897d66]。
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
    Pay(ctx context.Context, peerAID, taskID string, decision PayDecision) (Task, error) // purpose 固定 task-agent
}
```

`TaskFilter{ContextID string; State string; PageSize int /* 1–100,缺省 50 */; PageToken string; HistoryLen *int; UpdatedAfter *time.Time; IncludeArtifacts bool}`;`TaskPage{Tasks, TotalSize, PageSize, NextPageToken /* 无下一页为空串 */}`。`AgentQuery{Skill, Tag, Query, Limit, Cursor, IncludeUncarded}`;`RemoteAgent` 按 `Verification`(`VERIFIED`/`UNVERIFIED`/`NONE`)决定带哪些字段(§10.5)[Q24][Q27][impl:wp/fix5 598755c]。

- 所有带 `peerAID` 的操作只作用于 `role=outbound` 且 `peer_aid == peerAID` 的交互,不匹配一律 `TaskNotFound`(一次查询同时比较角色与对端),比较在存在性检查之前完成 [C17]。`SendMessage` 只带 contextId 时校验其属于本端点的出站交互:属于他人与"他人的任务 id"同样回 `TaskNotFound`,不存在则视为新 context [impl:wp/tasksd ee09872]。
- `Task`、`TaskEvent` 等内核类型即 `internal/a2ashape` 的 A2A JSON 投影(不导入 a2a-go);模块以 JSON 往返转换为 a2a-go 类型,往返测试钉住。
- 事件来自 daemon 进程内事件总线(按 ix 订阅);总线为每个订阅者抬升状态下限,状态事件对每个订阅者严格递增;慢订阅者被踢后由模块重订,不丢终态 [impl:wp/tasksd ee09872]。

### 11.2 路由

| 路由 | 说明 |
|---|---|
| `GET /a2a/v1/agents` | 已知远端 agent 列表(`skill`、`tag`、`q`、`cursor`、`limit`,同 §10.5 的 daemon 侧发现,不设 `include_uncarded`)。条目 `{aid, url, cardUrl, verification, verificationError?, anet.official?}`,仅 `VERIFIED` 条目另带 `name`、`hubVerification`、`homeHub`、`lastSeen`、`quiet`、`reviewCount`、`avgRating`;不列本节点自己与非法 AID [Q24][impl:wp/fix5 598755c] |
| `GET /a2a/v1/agents/{aid}/.well-known/agent-card.json` | 代理卡片(需 Bearer) |
| `GET /a2a/v1/agents/{aid}` | 代理卡片的别名:Hermes 配置写的是基址,a2a-go 的解析器对非根路径不追加 well-known [impl:wp/moda2a 85afc91] |
| `POST /a2a/v1/agents/{aid}/jsonrpc` | JSON-RPC 绑定 |
| `/a2a/v1/agents/{aid}/rest/...` | HTTP+JSON 绑定(请求体中的 `tenant` 须等于路径 AID) |

卡片与列表中的 URL 用监听的实际地址(手写 `[::1]` 端口时写 `[::1]`)[impl:wp/moda2a f0b763b]。入站任务与待批项不经本机 A2A 接口暴露;provider 侧由 MCP `reply_task`、CLI 或 §11.6 后端处理。

### 11.3 代理卡片

- 由远端网络卡片(本地验证通过后)生成:复制 name/description/version/skills/defaultModes;`supportedInterfaces` 指向本机两个绑定 URL;`securitySchemes = {"anetLocal": {"httpAuthSecurityScheme": {"scheme": "Bearer"}}}`,`securityRequirements: [{"schemes": {"anetLocal": {"list": ["a2a"]}}}]` [C18]。scope 用非空列表 `["a2a"]`(HTTP Bearer 忽略 scope):空 scope `{}` 不在发布形内——a2a-python 在签名前会删除空对象,连带删掉整条 requirement,与规范的原像不同,`a2acard.Sign` 会拒签 [Q7][impl:wp/moda2a 85afc91]。
- `capabilities`:`streaming: true`(描述本机接口,不照抄远端);`pushNotifications: false`;远端声明 x402 或 anet-pricing 时声明 x402(params `{signer:"anet-daemon", clientPayload:false}`,不设为必需、省略 `required` 字段,§8.7),anet-pricing 清掉空值后带过;`anet-origin/v1` params `{aid, originVerification, originCard?, anet.official?}` [C19]:`originCard` 是远端网络卡片原字节(base64url 无填充,≤ 64 KiB),**只在 `originVerification` 为 `VERIFIED` 时出现**;VERIFIED 只在字节存在且能解析时写 [Q24][impl:wp/fix5 598755c][impl:wp/moda2a f0b763b];`anet.official` 按本节点官方清单判定(§15)[impl:wp/manifest 2ef6c44]。
- 远端无网络卡片,或有卡而本节点验不过时,两者同样以 AID 生成占位卡(Q24):`name` 为 "anet agent <AID 前缀>",description 如实说明"没有本节点能验证的卡片,这里没有任何内容来自该 agent,名字由 AID 生成,skills 未知",`version: "unknown"`,skills 放一个 `chat` 占位,`originVerification: "UNVERIFIED"`,不带 `originCard`;hub 对该 agent 的任何陈述(目录名字、描述、伪造卡片的文字)都不放进本节点签名的卡里,也不放在官方标注旁边 [Q24][impl:wp/fix5 598755c]。
- 由本机 daemon 的密钥签名(`ProxyCardSigner`),不带 `jku`;按发布形输出(三种卡片经 `CheckPublishForm` 与严格 `Sign` 核对)。签好的卡缓存 5 分钟,占位卡 30 秒 [impl:wp/moda2a 85afc91]。

### 11.4 中间件

按此顺序:Host 白名单(`127.0.0.1`/`localhost`/`[::1]` + 本端口,否则 421,判定与控制面共用 `internal/loopguard`);拒绝带非空 `Origin` 的请求(403);Bearer(`a2a_token.txt`,常数时间比较;失败 401 + `WWW-Authenticate`,错误体为 `google.rpc.Status`)[impl:wp/moda2a 85afc91][B6-02 复核];JSON-RPC 与带体的 REST POST 只接受 `application/json`(415);请求体上限 96 MiB(413);`A2A-Extensions` 与 `X-A2A-Extensions` 按逗号拆分合并后改写请求头;`A2A-Version` 缺省按 1.0,显式非 1.x 返回 `VersionNotSupportedError`(也读查询参数,两种绑定各自渲染);响应回显"请求的 ∩ 该 agent 代理卡片声明的"扩展(在处理器之前写出,流式响应的头先于首个事件),a2a-x402 的 v0.1 URI 按 v0.2 激活与回显(§8.1)[Q18];包装的 `ResponseWriter` 实现 `Flusher`;所有响应带 `X-Content-Type-Options: nosniff`、`Cache-Control: no-store`、`Referrer-Policy: no-referrer`。错误按 a2ashape 错误名映射为 a2a 哨兵、文案固定,内部错误只进日志,不外泄包装文字 [impl:wp/moda2a 85afc91、f0b763b][impl:wp/fix5 598755c]。

流式调用的预检(0017 Q31)[Q31]:a2a-go 在调用 handler 之前就写出 SSE 头(`a2asrv/jsonrpc.go:150`、`a2asrv/rest.go:277`),handler 先返回的错误只能装在流里。因此 `module/a2a` 在进入 a2a-go 之前,对 SubscribeToTask 做请求、存在性、作用域(属于本端点)与状态(非终态)检查,对 SendStreamingMessage 直接完成这次发送(内核 `Send`,立即返回),把得到的任务交给随后的流,不再发第二次;不满足时以绑定自身形式的普通错误应答、不开流:JSON-RPC 为带请求 id 的 error 对象,REST 为 google.rpc.Status,HTTP 状态取 a2a-go HTTP+JSON 绑定对同一错误的状态(如 TaskNotFound 404、UnsupportedOperation 400)。不用 200 的原因:a2a-go 的 JSON-RPC 客户端把流式调用的 200 应答体按 SSE 解析,普通 JSON 错误会被读成"空流、无错误";非 200 时它至少报出 HTTP 状态(`docs/a2a/issue-a2a-go.md` A12)。预检与绑定以同样方式读请求体(`json.Decoder` 只取第一个 JSON 值,忽略其后的字节),否则带尾随字节的请求会越过预检、错误又回到流里 [Q31 复核]。

卡片缓存(0017 Q33)[Q33]:代理卡片(两个路径)与 `GET /a2a/v1/agents` 列表响应 `Cache-Control: private, max-age=300` 与按字节计算的 `ETag`,`If-None-Match` 命中回 304;其余 JSON-RPC/REST 响应与错误保持 `no-store`。

### 11.5 操作映射

| A2A 操作 | 实现 |
|---|---|
| SendMessage | 无 taskId → 新建:客户端给出的 contextId 原样保存,否则由本节点铸造;客户端为新任务指定 id → `TaskNotFound`(从不新建);按 `(contextId, 客户端 messageId)` 去重,重复返回已有任务,未给 contextId 时按 `(agent, messageId)`(客户端无法重复本节点铸造的 context);去重只针对非终态任务,命中终态任务(包括投递被放弃而失败即 `undeliverable` 的任务)时按新任务处理,客户端重试算新尝试 [Q5][impl:wp/tasksd a8396fb](0017 Q32);客户端 messageId 存入 `message.metadata["a2a.messageId"]`,不作为信封 mid;去重与投影的消息 id 只认本机写入的消息(发送方不是对端)上的该键,对端消息里的 `a2a.messageId` 不参与去重、不作为投影的消息 id [redteam:F33];Hermes 每次调用生成新 messageId,此去重对其超时重试无效。有 taskId → 追加消息,经出站队列可靠投递;终态任务 → `UnsupportedOperation`。`metadata["anet.skill"]` 或 DataPart `{skill, args}` 表示能力调用。`metadata["anet.end_request"]=true`(可无正文)表示请求方请求完成,等同 `end_request`(§4.2)[Q4](实现待补)。带 `x402.payment.status` 的消息按 §8.7 处理。消息的 role 须是请求方一侧(`ROLE_USER`)[impl:wp/tasksd a8396fb]。`return_immediately=false` 时等待到终态或中断态(`input-required`),且须是 `state_seq` 高于本次写入的状态,不提前返回、不把旧问题当答复 [C22][C35] |
| 只带 contextId 的续写(宽松处理)| 当该 contextId 下恰有一个属于本端点、对端相同、状态为 `input-required` 的出站任务时,续写追加到该任务:文本任务接受任何续写,能力任务只接受带 `x402.*` 元数据的付款消息(等待付款的标价调用因此可只凭 contextId 付款)[impl:wp/proj 78b4963][B6-02 复核];能力调用、发往能力任务的文本、多个候选,以及控制面与 MCP 一律按规范新建。重试先经 `(contextId, messageId)` 去重。这是对规范的宽松处理,改善 Hermes 等只带 contextId 的客户端,文档写明 [Q22][impl:wp/proj 78b4963] |
| SendStreamingMessage / SubscribeToTask | `Send`(立即返回)+ `Watch` → SSE;首个事件为 Task;终态状态之前先发尚未发过的 artifact 更新(`anet.reply` 在前);被总线踢掉后重订;对终态任务 SubscribeToTask → `UnsupportedOperation`;新任务的"之后"以 `Send` 返回视图的 `state_seq` 为界,对端在 `Send` 返回前已追问或报价时流立即给出该状态;付款消息被拒或被搁置时发出该任务后结束流 [impl:wp/moda2a 85afc91、f0b763b][impl:65cbcdb] |
| GetTask / ListTasks | interactions;ListTasks 按 `state_at` 降序,支持 `contextId`、`status`、`pageSize`(1–100,缺省 50)、`pageToken`、`historyLength`、`statusTimestampAfter`(按毫秒"不早于")、`includeArtifacts`;`includeArtifacts` 为 false 或缺省时省略 artifacts 与回执;无任务处于的合法状态(如 `auth-required`)匹配为空而非 `InvalidParams` [impl:wp/tasksd a8396fb] |
| CancelTask | §4.2 |
| 推送 4 个操作 | `PushNotificationNotSupported`(对任意 task id,先于存在性检查) |
| GetExtendedAgentCard | `UnsupportedOperation`(卡片不声明 `extendedAgentCard`,A2A §3.3.4)[impl:wp/moda2a 85afc91][impl:wp/a2adocs 15cbff3] |

输入 Part 规则 [C44]:text part 拼接为目标;raw FilePart 经 `attachmentFromBytes` 成为附件(64 MiB 上限、CID、文件名与类型带过);任何 scheme 的 url part(`file:` `http(s):` `data:`)一律 `InvalidParams`,daemon 不抓取、不读本地路径。raw part 的 mediaType 不按代理卡片的 `defaultInputModes`/`skills[].inputModes` 检查,任何类型都收为附件;a2a-tck 的 CORE-SEND-003(不支持的 mediaType 应回 `ContentTypeNotSupportedError`)因此预期失败,列为有意偏离 [impl:0019 §4 第 8 条][B6-02]。

Task 表示 [C21][Q21]:
- `artifacts` 只放产出:完成的文本任务是一个 `anet.reply` artifact,内含回复正文(非空时,TextPart,回执覆盖的对话记录中 provider 的最后一条)与该回复的文件(FilePart,文件名经 `safeName`),不再有空 text part,也不再有单独的附件 artifact;provider 未发言即完成的文本任务没有 artifact;能力任务是交付物 artifact `anet.result`(DataPart)[Q21 P1][impl:wp/proj 78b4963]。
- 回执不是产出:`metadata["anet.receipt"]` 是回执对象(回执本体 base64 CoreDet-CBOR、解码后的字段、本节点是否核验通过、核验所用的 provider KEL),随 artifacts 一同出现(GetTask;ListTasks 只在 `includeArtifacts` 时)[Q21 P2][impl:wp/proj 78b4963]。
- `status.message`:文本任务的 `input-required` 带 provider 最近一条消息;`working` 可带进度;付款相关的消息必有文字——provider 原话(或合成的首句)为第一段,其后从 `x402.payment.required.accepts` 摘出金额、资产、收款方、网络(至多 3 项,去掉控制字符、双向覆盖/隔离与零宽字符,限长),请求方一侧再说明付款方式:auto 档内由节点自动付(每个报价至多一次,失败后不再自动付);超出时用 MCP `submit_payment` 或终端 `anet pay <task>`;a2a-x402 客户端在本任务上发不带 payload 的 `payment-submitted`、多个选项时用 `anet.payment.accept`(§8.7)[Q21 P3][impl:wp/proj 78b4963、a1feeff];无消息但有 `anet.reason` 的 failed/rejected/canceled 合成一句 `"<state>: <reason>"` [Q21 P4];`pay_state` 非空的终态消息按 §8.2 带 `x402.payment.receipts`(先合成原因句,再附收据)[Q18][impl:d653c03]。只有元数据的存储行合成说明文字,从不输出空 text part。
- `history`:消息表,不含控制行;requester=user、provider=agent;能力任务的 history 是 DataPart `{skill, args}` [impl:wp/a2ashape 65fcc61]。
- `metadata`:`anet.effect_status`(仅能力任务,终态必带,§4.3)、`anet.receipt_verified`(`verified`/`unverified`/`unknown`;completed 与任何带回执的任务必带)、`anet.receipt`、`anet.request_cid`、`anet.result_cid`、`anet.peer_aid`、`anet.role`、`anet.skill`、`anet.state_seq`(供 `/tasks/wait` 的 `after_seq`)、`anet.trust`(仅入站任务)、`anet.reason`、`anet.retry_after_ms`、`anet.cancel_requested`、`anet.quote_expires_at`、x402 键 [impl:wp/a2ashape 65fcc61][impl:dc3eefa]。对端写入的 metadata 与交付物中超出 float64 的数(如 `1e400`)转为字符串,否则 a2a-go 解码失败会使整页 ListTasks 不可读 [impl:wp/a2ashape 7863633]。
- 文件(0017 Q12)[redteam:F32]:`history` 与流式事件(首个 Task 快照、状态更新、产物更新)只给附件元数据——url part `anet:attachment?interaction_id=…&cid=…`,metadata `anet.cid`、`anet.size`、`anet.attachment_cid`,以及文件名与类型,不带字节;单个任务的读取(GetTask、SendMessage 的应答、CancelTask、付款应答)按"先产物、后 `status.message`"内联,合计至多 8 MiB(`a2ashape.MaxInlineBytes`),超出的文件给同样的占位,且不读入内存;ListTasks 一律不内联;控制面 `/tasks/*` 一律占位,字节经 `GET /attachment` 或 `anet pull` 取。`module/a2a` 对流中每个事件再做一次 `EventForStream`(含 `EventByReference`),保证单条 SSE 行不因对端内容超过客户端的读取上限(a2a-go 为 10 MB):除文件外,事件按 JSON 大小整体限于 8 MiB(`a2ashape.MaxStreamEventBytes`,按 encoding/json 的转义计,`<`、`>`、`&` 与控制字符各 6 字节)——任务快照依次放 metadata、`status.message`、产物,放得下的原样保留,放不下的换成一条说明(`anet.truncated=true`、`anet.size`),再按从新到旧放 history,其余省略并在任务 metadata 标 `anet.truncated`;状态/产物更新事件同理。单任务读取不受此限,完整内容用 GetTask 取 [redteam:F32 复核:2 MiB 的 `<` 文本、大 metadata、6 万个附件或长 history 同样使 a2a-go 流失效]。

阻塞调用与客户端超时:客户端超时不会取消任务,任务继续运行(auto 档内可能已付款),客户端重试会建第二个任务;找回方式:按 contextId 的 ListTasks,或 MCP `list_tasks` 的 `context_id` 过滤。文档与 §21 写明。

### 11.6 提供侧后端(可选)

`modules.a2a.backends: [{"match": "*"|"<cap>", "url": "http://127.0.0.1:9900", "token_file": "…", "accept_untrusted": false, "toolless": false}]`[C9]:
- 配置:`match` 加载时去空白,先精确 skill、后 `"*"`,同一 `match` 只许一个;`url` 为后端卡片所在(无路径时读 `/.well-known/agent-card.json`),http 只许回环、https 不限;`token_file` 给出时不可读或为空即模块启动失败;`accept_untrusted: true` 仅当运营者同时声明 `toolless: true` 才接受 [impl:wp/moda2a 85afc91、6195f26、f0b763b]。
- 内核交付(`InboundTaskHost`,一处判定)[impl:wp/backends dee9d78、845024f]:只交付入站、已接纳(待批项在批准前不是交互)、非能力调用、非 `public_cap`、非终态、且最后一条是请求方消息的文本任务;对端须在 `trust_file` 且不在 deny(每次判定重读)。非信任对端只在有模块经 `DeclareUntrustedBackend()` 声明(该声明与 `open` 策略互斥,§5.1)时交付,且只限有人点过名的对端(`trust=peer` 的允许名单或 `approved`),`open` 期接受的陌生人任务(`trust=public`)不交付。请求方每追加一条消息再交付一次(按"历史长度:末条消息 id"去重);只内联最新一条消息的文件,其余保留引用;daemon 启动完成前不交付;定时扫描跳过 5 秒内有变化的任务(防附件未落盘就交出);交付给后端的任务不再交给自动回复(§6)。
- 转发给后端的 contextId 不是请求方给的,而是由 `(peer_aid, 交互 context_id)` 经 SHA-256 派生:同一对端同一 context 的任务共用,不同对端永不相同(Hermes 等按 contextId 选会话的后端因此不会让两个对端进入同一会话);派生放在内核接缝,任何订阅者都得到隔离。文档建议后端使用单独 profile 的服务 agent,不接用户正在使用的根会话 [Q23][impl:wp/backends dee9d78]。
- 模块转发器:按后端卡片建 a2a-go 客户端(`token_file` 的 Bearer 用于取卡与每次调用),卡片 `supportedInterfaces` 按与 `url` 相同的规则过滤,一个不剩即不转发;HTTP 客户端不跟随重定向;同一 context 发阻塞 SendMessage;metadata 带 `anet.peer_aid`、`anet.trusted`,去掉 `a2a.serviceParameters` 并把其中的 `A2A-Extensions` 还原为请求头;网络任务与后端任务一一对应(内存映射,后端遗忘时在同一 context 重开);同一任务串行转发,转发期间到达的新消息排队不丢;并发上限 8,单次 30 分钟 [impl:wp/moda2a 6195f26、f0b763b]。
- 回答:后端的 Message 与 completed 的产物为完成,`input-required`/`auth-required` 为追问,`rejected` 为拒绝,其余为失败;`ReplyTask` 只接受可交付给后端的任务(其余一律 `TaskNotFound`),状态限 `input-required`/`completed`/`failed`/`rejected`,经 `/tasks/reply` 的同一实现发出,后端回答自带的 metadata 不外发 [impl:wp/backends dee9d78]。
- 证据 `anet.backend.forwarded{backend, interaction_id, peer_aid, trusted}`,成功送达后才写;任何失败都留在收件箱、不回复、不记证据。
- 文档写明后端因共用一个令牌而无法自行区分对端。没有 `"*"` 后端时模块不订阅(内核交付的都是不带 skill 的文本任务)。doctor 检查:`accept_untrusted` 而无 `toolless`、`accept_untrusted` 与 `policy=open` 同时出现为 fail;`accept_untrusted`+`toolless` 为 warn;只服务信任对端的后端报告会收到什么,信任名单为空时提示无任何转发;无 `"*"` 后端为 info [impl:wp/backends dee9d78、845024f]。
- 已知互通缺口:Hermes 配了令牌之后,它的卡片用 pre-1.0 的 `securitySchemes` 形状,a2a-go v2 拒绝解析(`TestHermesAsBackend` 钉住);转发器按后端卡片建客户端,所以带令牌的 Hermes 目前不能作为后端,0018 §6.3 建议的"直接对配置的 URL 建 JSON-RPC 传输、不解析卡片"尚未实现 [impl:0018 §6.3]。

---

## 12. 控制面任务路由与 MCP

控制面新增任务路由(返回 `internal/a2ashape` 投影)[C20]:`POST /tasks/send`、`/tasks/get`、`/tasks/list`、`/tasks/cancel`、`/tasks/wait`、`/tasks/pay`(`{task_id, decision: submit|reject, accept?}`,purpose=`task-agent`)、`/tasks/pay-manual`(purpose=`task-manual`,只由 `anet pay` 调用),provider 侧另有 `/tasks/reply`;发现另有 `/agents/list`、`/agents/card`(§10.5)。与 `TaskSeam` 共用实现,但不按 peerAID 限定(控制令牌是全权凭据)[impl:wp/tasksd ee09872]。控制面的投影与本机 A2A 接口的差别:附件给 `anet:attachment?…` 引用而不内联(§11.5)——这是 `d145175` 的做法,早于 0017 Q12;Q12 规定 `/tasks/get` 与 GetTask 一样内联总上限 8 MiB、超出给 `metadata["anet.attachment_cid"]` 占位,控制面"一律引用"与之不符,是保留为控制面的设计(给模型的任务更小)还是按 Q12 改,待确认,登记在 `0022` [Q12][B6-02 复核];错误映射为 HTTP 状态码并带 A2A 错误名;`/tasks/list` 同时列出两侧任务,`metadata["anet.role"]` 标明本节点一侧 [impl:wp/tasksd d145175][impl:wp/a2ashape 65fcc61]。`/tasks/wait`:终态总是结束等待;调用方的截止时间到时按当时状态答复(标明超时),只有取消的上下文才是错误;对发给本节点的任务,请求方作答(除 `input-required` 外任何状态)即结束等待,provider 因此可以 `reply_task` 后 `wait_task` [impl:wp/tasksd a8396fb]。

MCP 实际注册 14 个工具,取代旧的 9 个,旧名删除不留别名(`task_delegate` 的 `pay=true` 属 gateway 档、`agents_find` 把自由文本发给 hub,两者都违反设计)[impl:wp/mcp 9309ea7]。每个工具显式给出 readOnly/destructive/idempotent/openWorld 四个提示(MCP 缺省 destructive、openWorld 为 true):

| MCP 工具 | 对应 | 取代 | 注解 |
|---|---|---|---|
| `list_agents` | `/agents/list`(`skill`、`tag`、`query`、`limit` 1–100 缺省 20、`cursor`、`include_uncarded`);`query` 只在本机逐页匹配 | `agents_find` | readOnly、openWorld |
| `get_agent_card` | `/agents/card`;只有 VERIFIED 才返回卡片,UNVERIFIED 只给原因,NONE 表示对端不发布卡片 [Q24] | 新 | readOnly、openWorld |
| `send_message` | `/tasks/send`(文本、文件以 raw part 发送、`skill`+`args` 能力调用、可带 `message_id` 防重发、`context_id`、`return_immediately`);`skill` 不是 `open` 节点的 `chat`(那是纯文本,发 text)[Q27] | `task_delegate` + `task_message` | 非 destructive、openWorld |
| `get_task` | `/tasks/get` | `task_results` 单条 | readOnly(本机) |
| `list_tasks` | `/tasks/list`(含 `context_id`、`role`=requester\|provider 映射为 outbound\|inbound、`state`、`peer`(AID)过滤 [impl:wp/mcp 9309ea7][B6-02 复核]);未给 `history_length` 时缺省 1(每个任务只带最新一条,免得一页任务灌入大量不可信的对端文本),`page_size` 1–100 | `task_results` + `task_inbox` | readOnly(本机) |
| `wait_task` | `/tasks/wait`(缺省 30 秒,上限 300 秒) | 新 | readOnly(本机) |
| `cancel_task` | `/tasks/cancel` | 新 | destructive、idempotent、openWorld(取消不可撤销,按默认安全取保守值;同一任务再取消不改变什么)[Q25][impl:3af0a2c] |
| `reply_task` | `/tasks/reply` | provider 的 `task_message`/`task_end` | 非 destructive、openWorld;始终注册;不给 `task_id` 时按 submitted/working/input-required 逐一查询可回复的入站文本任务(剔除能力调用),超过一页时说明"还有更多",没有时明确说明并提示待批项见 `inbound_pending` [m][impl:wp/mcp 9d02ee4] |
| `submit_payment` | `/tasks/pay` decision=submit(§8.6 agent 档);返回的是决定,`payment-submitted` 不等于已结算,随后 `wait_task`;超出 agent 档时任务仍在等待(§8.3)[Q26] | `task_delegate.pay` | destructive、openWorld |
| `reject_payment` | `/tasks/pay` decision=reject | 新 | 非 destructive、idempotent、openWorld |
| `get_balance` | `/balance` | `credit_balance` | readOnly、openWorld(读 hub) |
| `audit` | `/evidence` | `evidence_read` | readOnly(本机) |
| `node_status` | `/status` | 保留,描述与实现对齐 | readOnly(本机) |
| `inbound_pending` | `/inbound/pending` | 新,按字段白名单只转元数据 | readOnly(本机) |

mcpserv 原样转发控制面的投影 JSON(不经 map 重编码,大整数与键序不变);描述写明"completed 且 effect_status=UNVERIFIED 不等于成功"、`send_message` 在 auto 档内的报价由节点自动支付、`anet.official` 只按 AID 判定且不授予任何东西;`ServerOptions.Instructions` 下发短说明 [impl:wp/mcp 9309ea7、9d02ee4][impl:wp/manifest 2ef6c44]。MCP 服务端只调用白名单内的控制面路由(`internal/mcpserv/paths.go`:`/agents/list|card`、`/tasks/send|get|list|wait|cancel|reply|pay`、`/balance`、`/evidence`、`/status`、`/inbound/pending`),§8.6 的 `/tasks/pay-manual`、`/x402-authorize`、`/delegate`、`/redeem` 不在其中;非 2xx 返回带状态码、A2A 错误名与 reason 的错误,非 JSON 错误体取首行 [impl:wp/mcp 9309ea7、9d02ee4]。`internal/mcpserv` 与 `internal/daemon` 的依赖闭包不含 a2aproject(SI-8),有 `go list -deps` 测试 [impl:wp/mcp 9d02ee4]。

---

## 13. 安装与生态接入

### 13.1 命令

- `anet init`:幂等写出显式安全默认值(SI-5 的全部键;`inbound` 与 `payments` 块显式写出,`payments` 六个键齐全),创建空的 `peers.allow|deny|trust` 与 `payees.allow`(0600);已有配置只补缺省键、保留未知键、去掉 `accept_delegations` 并报告差异;不写 `modules.a2a`,也不建 `auto_reply` 块(该块存在即开启自动回复);缺 `control_addr` 时写回退地址而不新分配端口 [impl:wp/cli 971543a、9c6e786][impl:1b131c2]。
- `anet doctor [--json]`:只读数据目录,不需要 daemon,不写任何文件;只有 fail 才非零退出 [impl:wp/cli 971543a、9c6e786]。检查项:版本与签名(读安装记录 `<二进制>.release.json` 并用内嵌公钥重新验签,比对本二进制 sha256,报 verified/unverified/unknown,§13.2)[impl:wp/cli2 bb43e37];官方清单(验签、有效期、条目)[impl:wp/manifest 2ef6c44];模块、身份、控制与 A2A 地址(A2A 状态文件经 `internal/anethome` 读取)、hub 注册、入站策略、支出上限(SI-5 键名与当前有效值)、各编码工具接入(与 `anet agents` 同一套 `agentwire.Inspect`:wired/current/pending/conflict;`-tags no_mcp` 构建用读配置文件的等价实现)、Hermes 配置文件权限、Hermes `a2a_agents` 的令牌与端口是否过期(含义按 Q13:其中写入的令牌与当前 `a2a_token.txt` 不一致,或端口与 `a2a_addr.txt` 不一致,只比对本节点端口上的条目、不打印令牌;提示 `anet agents wire --refresh`)[Q13]、sandbox 模式是否配置了 `auto_reply.api_key`、§11.6 后端的 `accept_untrusted` 组合(§11.6)[impl:wp/backends dee9d78]、本机 A2A 端口冲突记录(`a2a_port_conflict.txt`,fail)与 `a2a_addr.txt` 端口的占用者(Linux 套接字表显示其他 uid 时 fail——daemon 停机期间也能发现正在收集令牌的占用者)[redteam:F18]。
- `anet agents [status] | wire | unwire [--all|<tool>…] [--refresh] [--a2a <aid>…]`(`internal/agentwire`,`//go:build !no_mcp`;`-tags no_mcp` 构建明确报错)[impl:wp/agentwire 2188ee1、554ba16]:
  - Claude Code:`claude` 在 PATH 上时 `claude mcp add -s user anet -e ANET_DATA_DIR=<dir> -- <abs>/anet mcp`(旧条目先 remove,之后读回 `~/.claude.json` 核对),不在 PATH 上时直接改 `~/.claude.json` 的 `mcpServers`;技能写 `~/.claude/skills/anet/SKILL.md`(frontmatter 为合法 YAML);认 `CLAUDE_CONFIG_DIR`;删除 `~/.claude/CLAUDE.md` 里的旧块。
  - Codex:`$CODEX_HOME/config.toml` 中以注释标记起止的托管块 `[mcp_servers.anet]`;托管块之外任何写法的同名定义(表头、带引号、`.env` 子表、点号键、`[mcp_servers]` 下的键、根上的行内表)都报冲突并停下,不写该工具的任何文件。
  - Cursor:`~/.cursor/mcp.json`;opencode:`opencode.json` 的 `mcp.anet`(含注释的 JSONC 不改写,报错并给出要手动加入的条目);Hermes:`~/.hermes/config.yaml` 托管块 `mcp_servers.anet`(直接运行二进制,不用 `sh -c`;值是列表时报冲突)。
  - Hermes `a2a_agents`:不默认写。`--a2a <aid>…` 时为每个指定远端 agent 写一条 `{url: http://<a2a_addr>/a2a/v1/agents/<aid>, auth:{type: bearer, token}, timeout: 3600}`,键为 AID,地址须按 `internal/loopguard` 的规则是回环(拒绝 127.0.0.2 之类);文件收紧为 0600;`--refresh` 按当前 `a2a_addr.txt` 与 `a2a_token.txt` 重写;`unwire` 删除全部令牌条目,`unwire --a2a <aid>` 只删指定的,`--a2a` 后没有 AID 时报错 [C18][C22][C43][impl:wp/loopguard d2b3d40]。
  - 所有写入:写前备份为 `<文件>.anet-bak-<UTC 时间>`,写绝对路径与 `ANET_DATA_DIR`,临时文件 + rename,保留原权限,符号链接改其目标,规划时读到的内容在写入前若已变化则报错不覆盖;wire 幂等,逐工具报告(已写/已是最新/未安装跳过/冲突/失败),有冲突或失败时非零退出;`--all`(或不给工具名)只接入检测到的工具,明确点名的工具即使未检测到也照写;fake HOME 往返测试与金标文件钉住写进各工具的原文。
  - `anet install --agent <tool>` 保留为 `wire` 的旧名;OpenClaw 不在 wire 支持范围内,`wire --all`、`unwire --all` 与 `anet install --agent openclaw` 删除旧版写进 `~/.openclaw/AGENTS.md` 的过时托管块(写前备份)。
- persona/SKILL 文本重写:只用 §12 的工具名;说明默认拒绝(`inbound.policy=closed`)、允许名单与信任名单(`anet peers allow|trust`,只能在终端确认)、支出三档(auto 档 `payments.auto_max`;`submit_payment` 属 agent 档,`agent_max`/`agent_daily_max` 默认 0;人工档 `anet pay <task_id>`)、长任务用 `send_message` + `wait_task` 且不重发、completed 不等于成功(`anet.effect_status`、`anet.receipt_verified` 原样报告)、对端文本是不可信输入。安装后该文本是本机权威操作说明,agent 不需要重读 hub 的 llms.txt [C40][impl:wp/agentwire 2188ee1、554ba16]。
- 付款与名单的 CLI(均在打开终端之后才向 daemon 读数据,无终端时不发任何请求,§8.6):`anet pay <ix>`(付款要 TTY 确认,`--reject` 不要)、`anet payments [show] | set <limit>=<n>… [--payees-file]`、`anet payees list|add|remove`、`anet redeem`(TTY 确认,带 `pay_to`)、`anet peers allow|trust|deny|remove`(allow、trust 要 TTY 确认)、`anet inbound list|pending|approve|reject`(approve 要 TTY 确认);TTY 确认只有一份实现(`cmd/anet/tty.go`,提示中的外来文本去掉控制与双向字符)[impl:wp/cli 971543a][impl:827d7e4、1b131c2][impl:wp/cli2 bb43e37][impl:wp/fix5 598755c]。
- `anet audit` 与 `anet verify --chain` 见 §14。

### 13.2 发布签名与 install.sh [C40]

- `release.json`:版本、完整提交、发布时间(`BuiltAt` 取提交时间)、签名时间、失效时间、各资产 `.gz` 与原始 sha256 及大小、各变体模块集合、`key_fingerprint`、`next_key_fingerprint`。以 `ssh-keygen -Y sign -n anet-release@agentnetwork.org.cn` 签名并当场按 `allowed_signers` 复验;`install.sh` 本身也签名(`install.sh.sig`)[impl:wp/release 4454858]。
- 公钥与指纹发布在 GitHub(`SECURITY.md`、README)、官网文档与 install.sh 内,`internal/release/allowed_signers` 与各处由测试钉成一致;emax `/dl` 是镜像,安装器与 `anet update` 不信任镜像本身 [impl:wp/release 4454858][impl:ANetHub wp/release c62228e]。`allowed_signers` 同一行列出两个命名空间:发布清单用 `anet-release@agentnetwork.org.cn`,官方清单(§15)用独立命名空间 `anet-official@agentnetwork.org.cn`,两种签名互不可冒用 [impl:wp/manifest 2ef6c44]。
- 手动核验路径写入文档:下载 `install.sh` 与 `.sig` → `ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh` → 执行。
- 邀请码不上命令行 [redteam:F41](进程参数对所有本机用户可读,先出示邀请码者被接纳,单次码随即作废):`anet hub-register` 的邀请码经环境变量 `ANET_INVITE` 或 `--token-file FILE`(`-` 为标准输入)传入,命令行上的 `--token` 拒绝;`anet up` 启动 daemon 时从其环境中去掉 `ANET_INVITE`。install.sh 同样读 `ANET_INVITE`(`… | ANET_INVITE=anetinv_… sh -s -- --hub …`)或 `--token-file`,`--token` 在解析参数时即退出;读取后 `unset ANET_INVITE`,只以环境变量交给 `anet hub-register` 这一条命令,不进其他子进程与任何命令行。hub 铸码时打印的命令与拒绝注册时的提示同样用 `ANET_INVITE`。`--token-file` 是邀请码离开命令行后的去处,他人可读的文件同样泄露 [redteam:F41]:文件对 group/other 可读、且其所在目录该类用户可进入时,CLI 与 install.sh 在发出任何请求前拒绝(提示 `chmod 600`)。agent 经 shell 工具执行的命令串本身就在 shell 进程的命令行上(`ANET_INVITE=…` 前缀也在内),拒绝信息、铸码输出与使用说明都写明:agent 以文件工具把邀请码写进只有本用户能进入的目录,再用 `--token-file`。
- install.sh:逻辑在 `main()`;只用 https(含重定向,`--proto '=https'`);取清单与签名(有大小上限),`ssh-keygen -Y verify` 验签(无 ssh-keygen 时说明并退出);验签失败、过期、降级、sha256 不符、版本或模块集合不符任一即退出且不触碰已装版本;模块/版本自检在目标目录的暂存副本上执行再 rename(`/tmp` 挂 noexec 的主机也能装);过期或降级的报错提示 `--base` 换源;macOS 只在 arm64 且现有签名验不过时才 ad-hoc 重签。安装后 `anet init`;`--agents` 时 `anet agents wire`;打印 doctor 状态块与一个免费官方 agent 示例 [impl:wp/release 4454858、2083aac][impl:wp/cli2 566709d]。
- 安装记录:install.sh 与 `anet update` 安装后在二进制旁写 `<二进制>.release.json` 与 `<二进制>.release.json.sig`(清单与签名的原字节);`anet doctor` 据此重新验签并比对本二进制的 sha256(§13.1)。`anet update` 在已是最新版时,只有本二进制的 sha256 在清单里才写记录,否则同版本的源码构建会得到一份无法担保它的记录 [impl:wp/cli2 bb43e37、566709d]。
- hub `llms.txt` Step 0:已安装时执行 `anet update`(说明它拒绝的情形,以及 0.2.0 之前没有 update 命令时改跑一次安装脚本);新机器才用 curl|sh(全部 `--proto '=https' --tlsv1.2`)并给出手动核验路径;hub 仓库里旧的未签名 `deploy/install.sh` 删除,对外的安装脚本只有 ANet 那一份 [impl:ANetHub wp/release c62228e]。
- `build-release.sh`:生成并签名清单;`.gz` 哈希(`gzip -n`);严格脏树检查(对 HEAD、含未跟踪文件);签名构建 `GOWORK=off`;全平台符号自检,各平台链接的模块包集合须与写进清单的宿主构建一致;签名钥只经 `ANET_RELEASE_KEY` 给出且须与仓内公钥一致;`--resign` 重签日期并同时改写两个指纹;`--unsigned` 为开发构建;`--official` 生成并签名官方清单(§15)。`anet update`:`crypto/ed25519` 验清单后下载、双重 sha256、探测模块集合,同目录临时文件 + rename 原子替换,失败不碰旧文件;预承诺的下一把钥按指纹接受;下载源 URL 含账号密码时拒绝 [impl:wp/release 4454858、2083aac]。
- 签名私钥:本期用开发钥(`DEV KEY — 正式发布前由产品负责人替换`),文档与 install.sh 明确标注;正式发布钥的保管方式**留待产品负责人决定**,换钥时须同时用新钥重签官方清单,否则嵌入清单验不过、不标任何人 [Q16][impl:wp/manifest 38b552a]。

---

## 14. 审计

- 新证据事件:`anet.delegation.received`(`open` 策略下聚合)、`anet.delegation.refused_summary`、`anet.policy.changed`、`anet.autoreply.invoked`、`anet.backend.forwarded`、`anet.payment.quoted`、`anet.message.sent/received`(只记 CID 与字节数)。
- `anet.message.sent/received` 的载荷只有 `{interaction_id, msg_id, kind, cid, bytes, attachments}`:`cid` 是信封内层 ChatMsg 编码的 CID,收发两侧相同,不对正文单独取 CID;`kind` 按 `x402.payment.status` 分为 text/payment;经出站队列发送时与入队同一写入之后记录,接收侧重投不重复记。`msg_id` 由对端自选:形如标识符(≤ 128 字节,`[A-Za-z0-9_.:-]`)原样记录,否则记 `"cid:"+SumRaw(id)`,对端不能借它把内容写进永久证据链。`trust=public`/`public_cap` 的消息不逐条签名落盘,计入入站汇总窗口,窗口结束时写两条聚合事件(计数、字节、按 trust 分布、首批 AID)[impl:wp/evid fb013f8、74ba580]。
- r4 另有的事件:`anet.delivery.expired{reason}`(出站行过期或被永久拒绝,§3.5)[impl:wp/wire be2fdb3]、`anet.interaction.pruned{trust, before, retention_days, interactions, messages, attachments}`(§5.1 保存期清理)[impl:wp/evid fb013f8]、`anet.payment.settled`(每张验签通过的成功收据)、`anet.payment.authorized` 增 `interaction_id`、`pay_bind`、`purpose`(§8.6)[impl:wp/x402d 2cc279b]。
- 事件名只在 `internal/evtypes` 登记一次(连同标签与来源);`anet audit` 的已知事件表由它生成;测试以 `go/ast` 扫全仓非测试源码(全部 build tag),每处写证据的事件名参数必须解析为已登记常量,运行时拼出的事件名同样判失败 [impl:wp/evid fb013f8]。
- `anet audit [--since|--peer|--interaction|--json]`、`--export DIR`、`anet verify --chain DIR [--kel <base64>|--hub <url>] [--head <id>]`;`anet audit hub` 为 `audit-hub` 别名(参数按 `audit-hub` 校验)。规则:`receipt_verified=false` 显示"未能核验";UNVERIFIED 不计入成功;未知事件原样列出;每段标明来源。实现:`anet audit` 从磁盘读取并逐条验签证据链,daemon 不在也可用;`--peer` 同时列出该对端交互的记录(`result.accepted`、`payment.settled` 不带对端键);`--interaction` 能找到待批项的批准/拒绝与 deny 留下的 `skipped_paid`;同一 ix 的第二笔已核验结算按 §8.3 标出(按全链计数,过滤不影响判定)[impl:wp/cli 971543a、9c6e786][impl:wp/wire 1591718]。
- `verify --chain` 的"通过"只表示每条记录由该 KEL 的 AID 签名、id 可重算、逐条链接回创世;不表示末尾没有被截掉——提前结束的链仍是有效的链——除非 `--head` 给出从别处得知的记录(见证、早先的导出)并被找到;从导出目录本身取的 KEL 只证明链属于它所写的 AID。结论与这两点随结果一起打印;导出目录是外来文件,清单与错误信息经 `printable` 输出,不能改写终端上的结论;`--hub` 拒绝不像 AID 的 `signer_aid`,非 ACTIVE 链明确报错 [impl:wp/cli 971543a、9c6e786]。
- 证据写在业务事务之后:两者之间崩溃会丢事件且不留缺口记录(`anet.evidence.gap` 只记录写坏而解不开的尾部记录,不覆盖这种情形),`anet audit` 不能把"链上没有"当作"没发生"(§21)[impl:0014 §10 风险 13]。

---

## 15. 官方公共 agent

- 后端 `ANet/cmd/anet-official`,独立二进制,每类能力一组 127.0.0.1 路由(`/v1/<group>/<capability>`,按 `-groups` 只开本身份的组),daemon 用 `service` 模块挂载;只监听回环、只应答回环 Host;后端以每后端令牌头认证 daemon(两侧取 SHA-256 后常数时间比较),令牌检查在路由之前,无令牌一律 401,不能借 404/401 的差别探测开了哪些能力 [impl:wp/official 893fb8a、79127ac]。日志只记调用坐标(能力、状态、调用方、交互 id、入口、字节数、耗时),不记参数与结果。
- 第一批能力:`net.echo`;`text.stats` `text.digest` `text.diff` `json.validate` `a2a.card.validate` `a2a.x402.check`;`docs.search` `docs.get`(构建时打包语料并计算 CID;`refresh-corpus.sh [--check]` 从仓库同步语料);`demo.digest.paid`。全部确定性、纯计算、不执行命令、不访问外网、不接受 URL。每个能力有参数上限(= `public_capabilities.max_args_bytes`)与超时;计算预算按工作量计并按步检查截止时间(大数只在需要算术时精确计算且有位数与指数上限,超出报 unknown;卡片最多验 8 个签名、KEL 最多 256 个事件),结果超过 1 MiB 回 422 `result_too_large` [impl:wp/official 893fb8a、79127ac]。
- 身份:A1 `anet-echo-e`(emax)、A2 `anet-echo-f`(fmax)、B `anet-tools`、C `anet-docs`、E `anet-paid-demo`;`inbound.policy=closed` + `public_capabilities`;五个身份的 daemon 配置、后端实例参数与两个 systemd 单元模板(daemon 以专用非 root 账户运行,后端 `DynamicUser` 且只许回环网络,令牌经 `LoadCredential`)在 `deploy/official/`,配置由 `anet-official service-config` 生成,测试比对样例与生成结果 [impl:wp/official 893fb8a]。
- 官方身份由客户端验证:发布签名密钥签署官方清单,随二进制打包;`/agents/list`、`/agents/card`(即 MCP `list_agents`、`get_agent_card`)、v1 `/find`、代理卡片的 `anet-origin` params 与本机 A2A 接口的 agent 列表据此标注 `"anet.official": true`(键缺省而非 false)。清单(`internal/official/manifest.json`,schema `anet-official/1`:`seq`、`issued_at`、`expires_at`、`key_fingerprint`、`agents[{id,name,aid,hub,caps}]`)与发布清单同一把钥、独立 SSHSIG 命名空间 `anet-official@agentnetwork.org.cn`(`allowed_signers` 同一行列出两个命名空间),两种签名互不可冒用;先验签再严格解析(未知成员、重复 AID/id、坏字段、`key_fingerprint` 与签名钥不符一律拒绝);只按 AID 判定,验签失败或过期即不标任何人;`seq` 只供排序与 doctor 展示,防回退靠 `anet update` 拒绝降级与 `expires_at`;标注只是标签,不给准入、信任、付款或通道(结构测试钉住只有 `internal/daemon` 与 `cmd/anet` 导入清单包、只有标注处读取它;官方请求方在 closed 下与陌生人同样被拒)[impl:wp/manifest 2ef6c44、76b3ffe、d5b10e3]。hub 在注册表或目录里的"官方"说法不构成官方;标注不与 hub 伪造的内容并列(Q24:未验证卡片只给 AID 与标注,无卡的官方 AID 不带 hub 文字)[Q24][impl:wp/fix5 598755c]。`/find`(`anet find`)同样按 0017 Q24 及其 `/find` 补充处理 [redteam:F39]:`/find` 的目录条目不带卡片、不经核验,官方 AID 的条目只给 AID、标注与签名清单中的 `name`/`caps`(附 `anet.note` 说明来源),不给 hub 的 name/summary/readme/pricing/home_hub;`aid` 不是合法 AID 的条目一律丢弃;查询词只与展示出的内容匹配。由 `build-release.sh --official` 从 `deploy/official/official-agents.txt` 生成并签名后提交(被 Go 读取器拒绝时回滚为原清单),release 构建核对清单验签、签名钥、有效期,且与源表一致并能被 Go 读取器读回 [impl:wp/manifest f764045]。当前已提交的清单 `agents` 为空(`seq 1`,开发钥签名,2027-09-27 过期),AID 待官方 agent 上线时写入;联调用 `scripts/official-testbin.sh` 以 `go build -overlay` 造把指定 AID 视为官方的测试二进制(一次性钥),产品代码无开关 [impl:wp/manifest 2ef6c44、5b59e5d]。hub admin 只登记 `id/aid/hub/caps`,不登记 runtime/ops/monitor/harvest;运维经 dmax 上的专用非 root 账户与独立工具 [C39]。
- `service` 模块把已验证调用方与 ix 以 `X-ANet-Caller`(仅已验证调用方,经 `Call.VerifiedCaller()`,兑付口为空)、`X-ANet-Call` 传给后端,另带 `X-ANet-Via`、`X-ANet-Capability`;`token_file` 为绝对路径,可展开环境变量(如 `${CREDENTIALS_DIRECTORY}/token`),变量未设置即拒绝,须是普通文件、其他用户不可读、至少 16 字节、最多读 4 KiB;令牌只发往回环或 https 地址,不跟随重定向;按能力覆盖超时(实现 `provider.LongRunning`,运营者设的上限不被 daemon 的 60 秒缺省截断);后端回 503/429 映射为 UNAVAILABLE(可重试),网关代答的 502/504 与请求写出后的超时、断连按"效果未知"报告(§4.3 [redteam:F10]),其余非 2xx 为 FAILED [impl:wp/official 6f1deca、79127ac]。
- 公开能力证据与保存策略 [Q15][impl:wp/evid fb013f8、74ba580]:公开能力的证据缺省 `cid` 模式(只记 `result_cid` 与指标,§5.1),官方配置全部为 `"evidence": "cid"`;`trust=public_cap` 的终态交互 7 天后清理(按天生效);后端不保存参数与结果;保存策略写进官方 agent 的公开说明(`deploy/official/README.md` §5/§6)与对外已知局限,随官方 agent 上线一并公开。这也是任何 anet 节点对公共能力的缺省做法,运营者可把某个能力改为 `full`。
- 部署属于生产变更,执行前征求同意。

---

## 16. 可插拔编译与 CI

| 项 | 方向 | 符号模式 |
|---|---|---|
| `no_a2a` | 减法 | 特例 `module/a2a\|a2aproject/a2a-go`(派生模式不匹配 SDK) |
| `taskboard`(原 `no_taskboard`) | 加法 | daemon `module/taskboard`;hub `internal/taskboard` |
| `no_mcp` | 减法 | `internal/mcpserv\|internal/agentwire\|modelcontextprotocol/go-sdk`(MCP SDK 是 `no_mcp` 省下体积的大头,经别的 import 仍被链接时只看树内包的检查会照样通过;SDK 只在精简构建里要求为 0)[impl:wp/tagsci 0eaf00d] |

- 全部构建 tag 及方向只写在 `scripts/tagcheck.sh` 一处:减法 `no_anetlink no_p2p no_blackboard no_org no_cas no_service no_mcp no_x402 no_a2a`,加法 `shell taskboard`。`tagcheck.sh check <tags>` 按每个 tag 自己的方向检查(减法:默认 > 0、带 tag == 0;加法:默认 == 0、带 tag > 0),未知 tag 报错(拼错的 tag 会静默构建出默认二进制);`deps [tags]` 断言 `go list -deps ./internal/mcpserv ./internal/daemon` 不含 a2aproject(SI-8),`go list` 本身失败不当作通过;`all` 跑每个减法 tag 单独、全部减法合在一起、每个加法 tag 与两次依赖闭包检查。`build.sh --check` 与 CI 共用它 [impl:wp/tagsci 8b4b20a]。
- 单 tag `no_a2a` 行保留 MCP;SI-8 依赖闭包检查在 CI 的 test 作业与 `tagcheck.sh deps` 中(默认与 `-tags no_a2a`),`internal/mcpserv`、`internal/daemon`、`internal/loopguard`、`internal/anethome` 另有 `go list -deps` 单测 [impl:wp/tagsci 8b4b20a][impl:wp/mcp 9d02ee4][impl:wp/loopguard ef748a4]。
- CI(`.github/workflows/ci.yml`):pluggable 矩阵 19 行——每个减法 tag 单独一行、按模块逐层叠加的减法组合四行(`no_p2p,no_anetlink` 起至 `no_cas,…,no_anetlink`)[B6-02 复核]、全部减法一行、发行档位各一行(含 `anet-min`:`no_x402,no_service,no_mcp,no_cas,no_org,no_blackboard,no_p2p,no_anetlink`,它在 `no_a2a` 加入全部减法行后不再与该行相同),组合行逐个检查全部模块并 `go vet`;optin 矩阵等于加法列表;race 作业另跑 `-tags shell,taskboard`;sandbox 作业装 bubblewrap、放开 user namespace,测试出现 SKIP 即失败(`pipefail`,并要求沙箱集成用例的 PASS 行);`joint-shell.sh`、`joint.sh`(仅手动触发)进 CI 且先 `continue-on-error`。`cmd/anet/tags_test.go` 核对四处 tag 列表(`module_<m>.go` 开关文件、`tagcheck.sh`、`optin_tags.go`、CI 矩阵)互相一致 [impl:wp/tagsci 8b4b20a、0eaf00d]。ANetHub CI 的 unplug/optin 矩阵做双向符号检查,optin 作业以 `go list -tags taskboard -deps` 核对不链入 `ANetCore/delegation|tsir`,test 作业装 sqlite3 且部署脚本测试不得跳过,webui 作业比对嵌入页与构建产物 [impl:ANetHub wp/hubops 030f22d、e6df2ec]。
- ANet CI 固定 ANetHub 的 checkout ref(暂为 `a2a-redesign-wip`),与第一个 wire-2 变更同批推送时改为 wire-2 hub 的 tag 或 commit [C37]。ANetCore 打 tag 之前,两仓 CI 用同一份复合 action `.github/actions/anetcore`:以 `GOWORK=off go list -deps ./...` 探测 `go.mod` 的 ANetCore 能否编译本树,不能时 checkout ANetCore 到工作区外并写临时 `go.work`(不持久化令牌);打 tag、`go.mod` 升级后自动回到只按 `go.mod` 构建 [impl:wp/tagsci 8b4b20a、0eaf00d][impl:ANetHub wp/tagsci 255d477、106624c]。
- 迁移注意:taskboard 改加法后,配置里残留 `modules.taskboard` 的节点,新默认构建拒绝启动,错误指向 `-tags taskboard`;升级前检查生产节点配置(§20 G)[impl:wp/tagsci 8b4b20a]。
- tag 列表的对外说明不新建 `CLAUDE.md`,以 README 与 `docs/DISTRIBUTIONS-zh.md` 为准(`no_a2a` 在减法、`taskboard` 在加法;发行档位是否带 `no_a2a` 是 DISTRIBUTIONS 的待决问题五)[Q20][impl:wp/tagsci 8b4b20a]。

---

## 17. 测试计划

| 层 | 内容 |
|---|---|
| ANetCore | `seal`:金标在解封侧——以 `DeriveKeyPair(ikm)` 固定收件人私钥,钉住整份信封字节,验证 Open + 验签结果;HPKE info 与签名原像单独钉字节;RFC 9180 向量只跑 `NewRecipient`/`Open`;封装侧用往返与变异测试 [m]。逐字段变异(`to type ix mid ts exp enc kid suite body kel keys`)全部拒收;0–63 未知键拒收。`VerifyEncKeySet` 的预期 AID 替换测试。`ExtendsKEL` 回退/分叉。`Replay` 的 `SupersededAt` 金标。`relayauth` v2 钉字节。`a2acard`:RFC 8785 附录向量、金标卡片、a2a-go 向量。`delegation` 新向量 |
| 契约 | hubapi / wirecontract:relay v2、keys、`/fed/v2/keys`、评价无内容字段、注册表、`paymentRequirements` 在 daemon→hub 与 hub→hub 两段请求体中;扩展 URI 与 metadata 键两侧钉字符串;控制面投影反序列化为 a2a-go `a2a.Task`(MCP 契约测试把投影的全部状态 × 结果组合经 MCP 取回再按 SI-6 检查)[impl:wp/mcp 9309ea7];Hermes `_reply_text_from_result` 移植版能从阻塞 SendMessage 结果取到答复文本——实现为逐行移植 Hermes 插件答复提取路径的契约测试,与 Hermes 原代码在 40 个响应上逐字一致,并断言任何投影形状都不会让 Hermes 把回执读成答复(0018)[impl:wp/hermestck 614c278][impl:2c1c126] |
| fake 完整性 | daemon 测试的 hub fake 只按 `to_aid` 存取不透明信封,实现 relay v2 全部拒绝路径与 keys/fed keys 端点;fake peer 按帧 ID 关联 ack。实现补充:fake hub 的 facilitator 按真 hub 契约(requirements 必填、`CheckRequirements`、`duplicate_binding`、重放回原收据、失败回执带被拒授权 id)并可注入结算故障,余额/流水/兑付读取验签,`/relay/poll` 支持 `after_id`,A2A 卡片准入、撤回与注册表同真 hub [impl:wp/x402d 2cc279b][impl:wp/c3more 9a34c97][impl:wp/relayhol 374dd12][impl:wp/cardgen d1cf8f8][impl:wp/proj 78b4963] |
| daemon 单测 | §3.6 每一步的失败分类;hub 以他人 KEL+keys 冒充收件人(mutation:去掉预期 AID);600 个新 AID 后截断 KEL 被拒;重启后仍被拒;存储失败后重投只处理一次;p2p 与 hub 并发只处理一次;入站六条分支;`public_cap` 交互拒收文本;配额;准入接缝覆盖兑付口;沙箱失败闭合两方向;控制面 Host/票据/CSRF/白名单;`/attachment` SVG;`/pull` 覆盖与符号链接;支出三档与 AdmitSpend 各调用面;商户核对;结算未知与 replayed;取消与付款竞态;状态迁移原子性;A2A 作用域(跨 AID、入站任务、他人 contextId);url part 拒收 |
| 现有脚本改动(C 阶段随内核改动同批)[m] | `joint.sh`、`joint-fleet.sh`、`scenario.sh`、`onboard.sh`、`lib.sh`:直接写 `peers.allow`/`peers.trust` 文件(CLI 的 `anet peers allow` 要求 TTY,脚本不走 CLI);能力 provider 写 `public_capabilities`;MCP 探针改新工具名;`/end-accept` 调用方与控制台结束 UI 更新;删除 `4-guest.sh`;prodtest 9c 删除、9f 改走 dmax 控制面、9n 改为统计 `delivered`;`joint-shell.sh` 与 `container-shell-test.sh` 改允许名单 |
| 联调 | `joint.sh`:SI-1 canary(含 admin 与官方 agent)+ 陌生节点被拒 + 允许名单路径 + hub 伪造注入被丢弃 + p2p 并发;`scenario.sh`:两 hub 跨 hub 加密委派(含 hub-local 可见性的只允许名单 provider,经 `/fed/v2/keys`)、provider 重启后超过缓存时限回复、跨 hub 付费(含少付与错收款方负面用例、正确付款仍只产生一份 credit);`joint-fleet.sh`:新完成语义;`joint-a2a.sh`:未修改的 a2a-go 客户端(`AuthInterceptor` + `CredentialsService`,卡片请求带 Bearer)经本机接口 → hub → 对端,覆盖 SendMessage/流式/GetTask/ListTasks/Cancel、x402 同任务流(夹具构造 §8.7 的付款消息;超上限、选项不在 accepts、外来 payload 三个负面用例)、缺 `securityRequirements` 的卡片得 401、daemon 重启后同一客户端配置仍可用;a2a-tck 结果记录(不作门禁);`joint-official.sh`:五层防护 |
| 联调(实现补充)| 联调脚本自包含:`JOINT_BIN` 接收预编译二进制、`JOINT_PORT_BASE` 给定端口段且被占即退出、工作目录须归本用户(`own_dir`)、私有 `XDG_RUNTIME_DIR`、按路径停进程(`stop_under`,不按进程名);本机 A2A 接口在段内钉端口(`pin_a2a`)[impl:wp/jointbase fde473f、d015c53][impl:wp/testrun 87d2048、e50b749]。`joint.sh` 段号 /11:陌生节点被拒与允许名单、伪造发送方(SI-4,含"真签名但不是该交互对端")、重放、同一信封经 hub 与 p2p 并发(SI-10)[impl:wp/jointbase 9bd8e1d、6042901];SI-1 canary 段 C/C′:记录式反向代理 tap 记下 hub 收发的每个字节,对 hub 与 admin 数据目录(含 WAL、备份)、tap 记录、HTTP 应答、日志与联邦对端按原文、hex、三种对齐的 base64 与 gzip 搜索,正向对照要求在当事节点自己的数据里能搜到,读不到任何文件不算通过(`scripts/canary.py`)[impl:wp/canary 4cdeb24、54ff98e]。`joint-a2a.sh` + `tools/a2aprobe`(另查 provider 侧取消、凭据分离、重启后的 SI-1)[impl:wp/jointa2a 702b235、92fbe7a]。`joint-official.sh` 8 段(五层防护、hub/admin 无 canary、deny 后与陌生人同答复、同名冒充不标 `anet.official` 且以 `official-testbin.sh` 做正向对照、`demo.digest.paid` 同任务流)[impl:wp/jointofficial 27b9a24、afbc35b]。联调层 mutation 以补丁文件给出(`scripts/mutations/`:`si1-plaintext-envelope.patch`、`si1-restore-official-harvest.patch`、`si4-skip-step7.patch`、`official-no-quota.patch`、`official-no-backend-token.patch`),`mutations/mutate.sh` 在临时副本里应用并构建,产品二进制不加开关;C32 例外,见补充用例表 [impl:wp/canary 4cdeb24、7c5709c][impl:wp/jointbase 6042901][impl:wp/jointofficial 27b9a24]。测试网首轮(lab 岛两台主机)记录在 `docs/notes/0021`。 |
| 实网 | `prodtest.sh` 在双 hub 上重跑(部署后,需同意) |
| 安全对抗评审 | 实现完成后按 SI 逐条尝试反驳 |

补充用例(r3;每条 mutation 验证):

| 来源 | 用例 |
|---|---|
| C16/C3 | 高水位三分支:TTL 过期后重取未变 keyset 成功;首次联系连续三条消息全部送达;同一卡片重复发现成功;同 seq 不同载荷判为分叉并计数;新 keyset 之后到达的旧 keyset 消息照常处理;内层 KEL 短于已存且 ts 早于轮换时接受,分叉拒收(mutation:`==` 分支改为拒绝)。hub:`POST /agents/{aid}/keys` 与 `/register` 以相同 keyset 重发返回 200,同 seq 不同字节 409;daemon 重启后以相同 keyset 重新注册不失败 |
| C1 | 在第 9 步与第 10 步提交之间杀掉 daemon 后重启:短能力调用恰好处理一次;长能力调用置 failed/interrupted 并送达请求方 |
| C12 | 600 个新 AID 委派、以及 600 个新 AID 调用公开能力之后,允许名单对端的截断 KEL 均被拒 |
| m | ix 碰撞:B 以 A 的出站 ix 发 delegate(能力与文本各一例),A 不执行、不改变该交互的 role/state/result |
| C19 | 未知 ix 的 message/cancel 在 10 分钟窗口内不 ack,delegate 随后到达时正常处理;超窗回 TaskNotFound;陌生人立即回 TaskNotFound,有关系的发送方至多 32 条在等待 [redteam:F25] |
| redteam | 直连误投回落 hub(F22);取消撤回未送出的委派、cancel 不先于 delegate(F23);持续暂扣流不钉住信箱游标、大量暂扣页不饿死新信(F24);附件/报价/收据写入失败后不 ack、重投写全(F27、F28);启动中的直连投递等恢复完成(F30);可能已送达的放弃判 UNVERIFIED(F12);直连在提交时 ack、超时对齐(Q29) |
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
| C32 | `scenario.sh`:关闭 `/fed/v2/keys` 查询后,新请求方首次联系 hub-local provider 失败(hub 对 keys 答 404);超时限回复的密钥复核失败(`keys_checked_at` 不前进),但回复仍以已存密钥送达——r3 写的"两例均失败"与 §3.5 第 1 步 [C2](hub 失败或 404 时继续使用已存密钥)相矛盾,按 [C2] 改。mutation 用 hub 的仅测试开关 `-test-no-fed-key-lookup`(同一套预编译二进制在同一次运行里做 mutation 前后,误开是失败关闭),不用补丁 [impl:wp/scenario 2ae1181、28808b8][impl:ANetHub wp/scenario 380551f] |
| m | p2p:无 `V` 的旧帧得到 error,daemon `Receive` 未被调用 |
| m | errorReason → `x402.payment.error` 映射表两侧钉字符串 |

---

## 18. 迁移与版本

- 破坏性升级:ANet v0.2.0 与 ANetHub wire 2 同时部署。旧 daemon 连新 hub 得到 426;新 daemon 连旧 hub 拒绝工作。hub 首次以 wire 2 启动即执行不可逆迁移:中继表重建时丢弃全部 wire-1 行(计数写 `hub_meta`,§3.7)[Q17],评价内容列删除并 VACUUM(需约等于库大小的空闲磁盘与独占锁)——部署本身就是数据清理的一部分 [impl:ANetHub wp/hubops 030f22d][impl:0014 §9]。
- ANetCore v0.15.0:新增 `seal`、`a2acard`、`identity.ExtendsKEL`、`Replay` 修正、`relayauth` v2、`delegation` 增量;不新增依赖;`docs/scope.md` 登记 `crypto/hpke`(标准库)与 `a2acard` 的归属理由。
- 开发期:专用 `GOWORK=/data/projs/anet-dev/.anet-work/go.work`(只含三仓,不放在共享目录根,不含 ANetLink/ANetMock);各工作树另有自己的 `go.work` 与 `env.sh`,只含该工作树的三仓检出 [C37][impl:0014 B6-02]。a2a-go 在 ANet 中用 `go get github.com/a2aproject/a2a-go/v2@v2.6.0` 写入 go.mod/go.sum;`go mod tidy` 延后到 Core 打 tag 之后。
- CI 策略:发布前不向 ANet、ANetHub 的 main 推送;ANetCore 打 tag 后,两仓的 go.mod 升级、hub ref 固定与全部改动同批推送(推送前征求同意)。本地以 `GOWORK=off` + 临时 replace 复核单仓构建。
- 下游消费者(ai-studio anetbridge、Research-Galaxy agent-runtime、ANetOS-Web hubkeeper)本期只列影响清单,见 `docs/notes/0020`(`/end-accept` 410、`/accept on` 400、`/threads` 状态取值、默认 closed、`/pull` 子目录、控制台票据、MCP 旧名失效等,逐项给出迁移做法);迁移由各自维护方在 v0.2 部署前完成,通知属于阶段 G [impl:wp/docs 1f59722]。
- 构建 tag:配置里有 `modules.taskboard` 的节点,新默认二进制拒绝启动(§16);MCP 旧工具名失效,客户端里按旧名写的权限规则(如 `mcp__anet__task_delegate`)随之失效,发布说明写明 [impl:wp/tagsci 8b4b20a][impl:wp/mcp 9309ea7]。
- 部署前置:hub 前置 nginx 的请求体上限改为 129m(§3.7)[impl:ANetHub wp/hubops 030f22d]。
- p2p 路径上,新 anetpeer 拒收无版本帧并回错误(§3.10)。
- `modules.x402.voucher_url` 的主机不是回环地址时只接受 https;违反时 daemon 启动失败,错误信息说明兑付口需由运营者前置 TLS 终端。`docs/GUIDE-zh.md:230` 与 `docs/site/guide.html:177` 的 http 示例改为 https。升级 dmax 前检查其 `voucher_url`:为 http 时改为经 TLS 终端的 https 地址或删除该键(生产变更,执行前征求同意)。

---

## 19. 对 A2A 社区的贡献(草稿,`ANet/docs/a2a/`)

1. 中继绑定规范草案:tenant=AID 路由、E2E 信封、发送方签名认证、store-and-forward 下的流式语义;写明经中继的操作(SendMessage、CancelTask)、由请求方 daemon 以本地状态回答的操作(GetTask、ListTasks、SubscribeToTask)、不支持的操作(推送 4 个、GetExtendedAgentCard);服务参数经 `a2a.serviceParameters` metadata 携带。
2. 注册表 API 草案。
3. `anet-credit` x402 scheme 文档(托管性质、payload、签名、nonce 与窗口、`hub:<aid>` 与 CAIP-2 的偏离;本地签名服务接受未签名的所选项这一偏离;另须写明 x402 v2 对象层与 `payment-verified` 表示已扣款两处与规范/参考实现的差异,§8.2)[impl:简报 04 §9]。
4. a2a-go issue 草稿;5. a2a-x402 issue 草稿;6. `SecurityScheme` 新变体提议。

现状:六份英文草稿与中文索引已在 `docs/a2a/`,每份标注"DRAFT — not submitted; requires product owner approval before any external submission"与"license: to be decided";草稿写成时未实现的规则标为 **(designed)**。其中本机 A2A 面、`anet.cancel_requested`、同任务付款流、商户核对与映射表等此后已落地,草稿需按 r4 逐处复核并去掉或保留 (designed) 标记,这是对外提交前的一步(`docs/a2a/README.md` 第 2 条)[impl:wp/a2adocs 2ef4bfb、15cbff3]。注册表草案已加 `withdrawn` 行 [impl:wp/proj 78b4963]。

前置条件:贡献部分(规范文本与参考实现)的许可证**留待产品负责人决定**(草稿标注"许可证待定");对外提交前征求同意 [Q20]。

---

## 20. 实施阶段

| 阶段 | 内容 | 依赖 |
|---|---|---|
| A | ANetCore:`seal`、`a2acard`、`ExtendsKEL`、`Replay` 修正、`relayauth` v2、`delegation` 增量与新向量 | — |
| B | ANetHub:relay v2 + 认证发送 + 限额 + keys + `/fed/v2/keys` + KEL 延伸 + 联邦转发;内容移除(访客、全部采集、评价、completed_task、admin 官方 ops、恢复与留存脚本、webui);facilitator 拆分与核对;账本读取鉴权;注册表 + JWKS + fed v2;taskboard 加法;llms.txt;CI | A |
| C | ANet 内核:密钥环、封装/解封与接收流水线、`peer_identity`、传输接口、p2p/anetpeer;任务模型与状态机、完成/取消、结果重试;入站策略、准入接缝、待批;自动回复加固与沙箱;控制面与 console.html 改造、`/attachment`、`/pull`;支付同任务流、AdmitSpend、支出三档;`a2ashape` 与控制面任务路由;评价无内容;nonce 与对话记录 v2;证据事件;A2A 网络卡片;现有脚本与测试同批改动 | A |
| D | ANet 北向:`module/a2a`、`TaskSeam`、事件总线、代理卡片;MCP 重组;`agentwire`、`init`、`doctor`、`audit`、`update`、`pay`、`peers`、`inbound`;install.sh 与发布签名 | C |
| E | 官方 agent 后端与配置;文档与对外陈述更正表中"E2E 部署后"各行;贡献草稿;新联调脚本;CI 矩阵;tag 列表以 README 与 `docs/DISTRIBUTIONS-zh.md` 为准(不新建 `CLAUDE.md`)[Q20] | C、D |
| F | 全量验证:gofmt/vet/test/race、tag 矩阵、全部联调、a2a-tck;安全对抗评审与修复 | 全部 |
| G | 需征求同意:推送与发布、部署两台 hub 与官方 agent、生产数据清理、对外提交 | F |

B 与 C 在 A 完成后并行;同一仓库内按文件归属串行推进。

r4 时点的进度(剩余阶段按 `docs/notes/0014` 的批次推进,已定决定见 `0017`)[B6-02]:A–E 的实现已合入三仓 `integ/round4b`(ANet `17fe0b6`),`wp/fix5`(Q24、Q27、Q18 余项、x402 hub 身份缓存、`/redeem` 带确认的收款方)随后合入 `integ/round4c`(ANet `a2a-redesign-wip` `82e1e9f`);测试网首轮联调(F 的提前段)记录在 `0021`。已定而未落地的:Q4(A2A 面的 `anet.end_request`)、Q12(本机 A2A 接口的附件内联上限;控制面"一律引用"与 Q12 字面不符,待确认,§12)[B6-02 复核]、Q24 补充(v1 `/find`);"E2E 部署后"的对外陈述更正要等部署。F(计划批次 7:全量验证矩阵与按 SI 的对抗评审)与发布准备(批次 8)未开始;G 全部待征得同意,且 Q16(发布签名私钥保管)、Q20(贡献许可证)留待产品负责人决定,清单见 `0022`。

对外陈述更正表(E2E 部署前不出现"hub 看不到内容"的表述)。"立即"各行已改(另补同类的 DESIGN 两处、PAYMENT 一处、ARCHITECTURE:99、SUITE-TODO:169);"E2E 部署后"各行的位置与改后文本、检索到的同类表述及联调依赖的生产配置前提记在 `docs/notes/0016`;v0.2 用户文档改为"v0.2 起"的条件表述(0.1.x 明文中继、两代不互通、官方 hub 随 v0.2 同批切换),不写对现网的"hub 看不到内容" [impl:wp/scripts 4b6175e、1b5294e、5834043][impl:wp/docs 1f59722]:

| 位置 | 现有表述 | 改后表述 | 时机 |
|---|---|---|---|
| `ANet/README.md:192` | "The Hub relays bytes and keeps an index — it is *not* a trusted party." | 立即:"In v0.1 the hub relays task contracts, chat messages and results unencrypted and can read them; signatures let either party detect forgery but do not stop the hub from reading. End-to-end encryption is planned for v0.2." E2E 部署后:"The hub relays encrypted envelopes and cannot read task content; it still sees sender, recipient, time and size (see Known limitations)." | 立即单独改;E2E 部署后再改 |
| `ANet/docs/ARCHITECTURE-zh.md:313` | "Hub 只搬运不透明字节" | 立即:"Hub 搬运的 delegate 与 result 带签名,可检测伪造;v0.1 中这些字节对 hub 可读"。E2E 后:"Hub 搬运 HPKE 密文,可见元数据见已知局限" | 同上 |
| `ARCHITECTURE-zh.md:38` | "普通聊天消息则以明文中继" | "daemon 之间全部消息以 HPKE 封装" | E2E 部署后 |
| `ARCHITECTURE-zh.md:170`、`:316` | `/relay/send` "开放"、"刻意不鉴权" | "relayauth v2 发送方认证,按发送方限流(X1)" | E2E 部署后 |
| 官网、hub web UI、llms.txt、skill.md 中的同类表述 | 阶段 E 检索后补入本表 | — | — |

---

## 21. 已知局限(写入对外文档)

第 1–25 条已写入 `docs/KNOWN-LIMITATIONS-zh.md` 与英文版,编号一一对应:第 1–14 条 [impl:wp/scripts 5834043],第 15–25 条由发布准备同步,原在对外文档"另外需要知道的"一节的第 21、24 条移入编号 [B8-01]。第 15–20 条为 r4 新增(来自实现与复核记录、`0021` 测试网首轮)[B6-02];第 21 条此前只在对外文档中 [B6-02 复核];第 22–24 条为红队修复轮新增(拒收表下限、陌生人未知任务消息、文本回执的覆盖范围)[redteam:F5][redteam:F25][redteam:F15];第 25 条来自 `0017` Q34(`0024` L2)。

1. hub 在发送时刻知道"谁发给谁"、何时、多大;来源 IP 可见。hub 主机上的反向代理若保留访问日志,会把来源地址、时间、请求行(多数含 AID)与响应大小写到磁盘,足以在 ack 删行之后重建社交图;随仓库下发的 nginx 配置不保留 hub 的访问日志、错误日志只记 `crit`,由主机 logrotate 至多保留 14 天,运营者换用别的配置则可能保留。daemon 在给对端写之前查的密钥、卡片与验卡用的 KEL(`POST /agents/keys:lookup`、`/a2a/v1/agents/card:lookup`、`/agents/kel:lookup`)请求行都不含对端——A2A 客户端经 daemon 与对端通信时先取代理卡片,daemon 为此取对端卡片与 KEL,只改密钥查询时这两行仍点名对端;p2p 地址查询(anetpeer `GET /agents/{aid}/p2p`)与旧版 daemon 仍在请求行里带对端 AID [redteam:F3]。
2. 前向保密以加密密钥生命周期为界:一条消息在发出后至多 29 天内,可被取得收件人磁盘的一方解开。
3. 大附件整体缓冲、单次 AEAD。
4. 官方公共 agent 是端点,能看到调用内容;其保存策略公开写明。
5. 沙箱内的本地 agent 仍可联网,可外发工作目录内容或自身凭据;宿主回环 TCP 服务与抽象 Unix socket 在沙箱内可达。
6. 对端 KEL 首次信任:第一次看到某 AID 时接受其自证明 KEL;回退防护只覆盖已有持久记录的对端。hub 可以提供旧卡片与截断 KEL,daemon 只拒绝回退到本机已见状态之前。
7. `hub:<aid>` 不满足 CAIP-2。
8. `return_immediately=false` 的 SendMessage 在对端离线时可能等待很久;客户端超时不取消任务,重试会建第二个任务。按 `(contextId, messageId)` 的去重对每次调用生成新 messageId 的客户端(如 Hermes)无效;只带 contextId 的续写有一处偏离规范的宽松处理(§11.5)[Q22]。
9. hub 能按付款方、收款方与时间把结算与公开评价关联;公开发放链显示每笔跨 hub 付款、清算与兑付的金额、时间与 AID。经 hub 网关购买的凭证与报价含能力 id 与收款方,hub 可见。结算请求不带 `resource`,但带收款方与准确金额;收款方在卡片上公开逐 skill 价格(anet-pricing/v1、ADP 卡片价格表,§10.1)时,由"收款方 + 金额"即可推出所买 skill,也就是报价里的 `resource`:hub 对每笔结算都能做到,跨 hub 付款则任何读公开发放链的人都能做到;价格相同的 skill 之间无法区分,只标价一个 skill 的节点本来就只有这一个可买。取舍:`payments.publish_prices=false` 时卡片不发布逐 skill 价格,价格只在 E2E 报价中给出,结算金额不再经公开价格指向 skill;代价是调用前看不到价格、hub 网关无法出售该节点的能力(网关只按签名卡片上的价格出售),卡片仍声明 a2a-x402(全部 skill 收费时 `required`)。缺省 `true`,因为公开签名价格让 hub 无法以卡片外的价格报价,也让调用方事先知道价格。profile 的自由文本 `pricing` 若写了逐能力价格,同样公开 [redteam:F1]。
10. 通过 curl|sh 从 agentnetwork.org.cn 或 hub 域名首次安装时,信任提供脚本的主机(当前与官方 hub 同机);按 hub 的 llms.txt 行事的 agent 执行的是该 hub 提供的指令。安装后 `anet update` 只依赖发布密钥。
11. KEL 轮换没有产品触发路径;轮换宽限默认 1 小时,超过宽限仍在信箱中的旧密钥消息会被拒收。
12. `A2A-Version` 缺省按 1.0 处理,偏离规范的"缺省按 0.3"。
13. TTY 门槛(`anet pay`、`anet peers allow`、`anet inbound approve`、修改支出上限)在 CLI 进程内检查,对应的控制面路由凭控制令牌即可调用。它只约束只能经 MCP 工具或本机 A2A 接口行事的 agent。任何能以本用户身份执行命令的 agent(包括 Claude Code 等工具的 Bash,不论有无 TTY),都可以读取控制令牌直接调用这些路由,或直接改 `peers.*`、`config.json` 并重启 daemon。同 uid 下不存在更强的边界,文档如实写明。对**其他本机用户**(不同 uid),anet 自己的客户端在发送控制令牌前核实监听者(§7 第 10 条),票据与邀请码不进命令行(§7 第 2 条、§13.2);但本机 A2A 接口的第三方客户端(Hermes 等)按写入的地址发送 Bearer 令牌、不核实监听者:daemon 停机期间别的本机用户占住记录的端口、收集令牌、再在 daemon 启动前放开端口,daemon 无从察觉(占用期间运行 `anet doctor` 可以发现;daemon 启动时发现端口被占则不启动接口并轮换令牌,§11.1)[redteam:F18]。
14. 本期不处理(归属与理由):ANetLink `c1.sock` 权限与 `SO_PEERCRED`、按 `caller_aid` 授权(跨仓,ANetLink 单独立项);联邦按卡片 home hub 定向转发(当前按对等表顺序尝试,功能正确;被尝试到的对等 hub 也会收到这封密文及其收件人 [impl:wp/scripts 5834043][B6-02 复核]);交互级临时密钥;大附件分块;sealed sender;发放链隐私格式;沙箱网络隔离;非 Linux 沙箱。
15. 没有持久身份记录的对端,每条消息都按首次信任处理:第 6 条的回退防护只覆盖 `peer_identity` 中有记录的对端(允许/信任名单、本节点主动联系过的、经人工批准的)。陌生人的 KEL 与 keys 只进有界内存缓存,仅用于加密拒绝回复,不参与回退比较;`trust=public`/`public_cap` 交互的对端只存在该交互行里、终态后删除(§3.8)。所以对这些对端,第 6 步对每一条消息都重新接受信封内自带的自证明 KEL:KEL 在发送方签名的密文里,hub 改不了;但持有该 AID 已被轮换取代的旧签名钥的一方(旧钥泄露),可以附上截断到旧钥仍为顶端态的 KEL 冒充它,本节点没有已见状态可比(公开能力调用与 `open` 策略下的陌生人任务因此不能依赖对端的轮换历史)[B6-02 复核]。同理,这些对端的卡片高水位只在内存,重启即忘,hub 可以在重启后重放更旧的卡 [impl:wp/proj 78b4963]。
16. 证据链的完整性有边界:`anet verify --chain` 通过不表示末尾没有被截掉,只有 `--head` 给出从别处得知的记录时才能排除截尾;证据写在业务事务之后,两者之间崩溃会丢事件且不留缺口记录。`anet audit` 不能把"链上没有"当作"没发生"(§14)[impl:wp/cli 971543a][impl:0014 §10 风险 13]。
17. 官方标注只按 AID,且只来自随二进制内嵌的签名清单:清单过期后不标任何人,直到升级到带新清单的版本;没有独立的吊销通道(靠发布新版本与 `expires_at`),`seq` 不防回退(靠 `anet update` 拒绝降级);标注不给准入、信任、付款或通道。发现结果里只有 VERIFIED 条目的内容来自 agent 签名;`include_uncarded` 列出的 NONE 条目除 AID 外全是 hub 的陈述(§10.5、§15)[Q24][Q27][impl:wp/manifest 76b3ffe]。
18. 网络卡片的撤回只随注册发出:节点失去全部 skill 时 hub 不可达,撤回不单独重试,等下一次注册(重启、再次 `hub-register` 或卡片输入变化);在此之前 hub 与联邦对端继续列出旧卡 [Q6][impl:wp/proj 78b4963]。
19. 与 A2A 和 a2a-x402 规范的其他偏离(除第 7、12 条外):只带 contextId 的宽松续写(§11.5)[Q22];本机 A2A 接口不按卡片的 input modes 检查 raw part 的 mediaType(§11.5);`payment-verified` 在 anet 表示已结算扣款,规范与官方参考实现表示验过未扣(§8.2);`x402.payment.required` 是 x402 v2 对象,只认规范示例的 v1 形状的客户端读不懂,需经 anet daemon 签名付款(§2、§8.7)。
20. deny 不撤销已提交付款的工作:对端进入 deny 时,`pay_state ∈ {submitted, completed}` 的交互照常完成并交付,只在 `anet.policy.changed` 的 `skipped_paid` 中列出(§5.1)[Q10]。
21. 端到端加密只覆盖 daemon 与 daemon 之间:经本机 A2A 接口接入的客户端自己保存什么不在 anet 的保护范围内(例如 Hermes 把每次往来以明文写进 `~/.hermes/a2a_conversations/` 与 `a2a_audit.jsonl`);§11.6 后端同理。本机 A2A 令牌(`a2a_token.txt`)可以以本节点身份向任何 agent 发任务并在 agent 档上限内付款,读不到发给本节点的任务;`anet agents wire hermes --a2a` 把它写进 0600 的 Hermes 配置。对外文档已在"另外需要知道的"一节写明这两点 [impl:wp/docs 1f59722][B6-02 复核]。
22. 被拒 delegate 的持久拒收表有界(§3.6):某发送方 15 天内被拒超过 1024 次,或全节点被拒超过 10 万次(如 sybil 洪泛)时,被挤出的拒收转为该发送方自己按发送时间的下限;此后该发送方晚于这些拒收才到达、发送时间却更早的首次投递(例如在信箱里压了很久的 delegate)按拒收丢弃且不回复。下限只作用于其发送方本身,别的发送方的拒收抬不动它 [redteam:F5]。下限表也有界:15 天内有超过 10 万个不同发送方的拒收被挤出(大规模 sybil)时,最早到期的下限被忘掉,对应发送方被挤出的拒收若再被重放,会按当时策略重新判定。
23. 陌生人(与本节点没有持久记录、名单或出站往来)发往未知任务的消息在 hub 信箱中不等待其委派,立即得到 TaskNotFound [redteam:F25]。发送方的外发按任务顺序投递,直连路径上的未知 ix 消息不 ack、转 hub(§3.6 第 9 步),所以后续消息只会排在已进信箱的委派之后;仍会得到 TaskNotFound 的只有委派本身在本节点暂时失败(存储错误)而同页其后的消息先被处理这一种情形 [redteam:F25 复核]。
24. 文本任务的回执(对话记录 v2)由 provider 签名,覆盖 provider 交付的那份记录;记录中"请求方说的话"是 provider 的记录,requester 不逐条比对自己的消息日志,`receipt_verified=verified` 不证明请求方说过这些话(§2 X4)[redteam:F15]。
25. 只有 profile、没有公开能力的节点在别的 hub 上看不到:联邦搬运签名卡片等签名对象,profile 不在其中;无公开能力的节点卡片里没有能力,对端 hub 据此不列出它(`include_uncarded` 跨 hub 同样看不到)。不改,建议要被跨 hub 发现的 agent 发布公开能力 [Q34]。
