# ANet 套件 TODO 与依赖关系

**这是活文档,不是一次性报告。** 它跟着代码走。

住在 ANet 仓里,不在 AgentNetwork 里 —— 后者是 anet3 的单体仓,是**被重构掉的那个**
(见 [REWRITE-from-anet3-zh.md](REWRITE-from-anet3-zh.md))。一份活文档放在正在退役的
仓库里,既推不上去也不会有人改。

- **最后核准**:2026-08-22(C-3/C-6 当日完成)(逐仓跑了 `git log`、`go test`、目录与导出面扫描)
- **条目编号**:`C-n` ANetCore · `D-n` daemon · `H-n` Hub · `L-n` Link · `M-n` Mock。
  编号只增不减 —— 完成的条目移到 DONE 并保留编号,这样跨文档引用不会失效。
- **维护约定**:改动落地后同时改这里。一份只列已完成项的清单,读起来永远像完成了;
  **"未做"和"刻意不做"必须写下来**,否则下一个人会重做或者以为漏了。

---

## 一、五仓一览

| 仓 | 角色 | 实现 / 测试 | 测试数 | 依赖 |
|---|---|---|---|---|
| **ANetCore** | 协议内核,纯逻辑零 I/O | 4,269 / 3,056 | 102 | 无(只有四个外部家族) |
| **ANet** (daemon) | 唯一有生命周期的进程 | ~11,000 / ~5,400 | 143 | ANetCore |
| **ANetHub** | 中继与目录 | 6,949 / 1,362 (+2,316 webui) | 35 | ANetCore |
| **ANetLink** | 设备能力来源 | 32,938 / 16,546 | 317 | ANetCore |
| **ANetMock** | 忠实假件(被控硬件) | 5,910 / 831 (+2,060 web) | 29 | 无(刻意不说 ANetCore 类型) |

**依赖方向**:三个应用 → ANetCore,**永不反向,应用之间永不互相 import**。
ANetMock 连 ANetCore 都不依赖 —— 它若发出成品 `Effect`,就把被测的东西本身抹掉了
(见 ANetMock/DECISIONS.md)。

```
                    ┌──────────────┐
                    │   ANetCore   │  纯逻辑 · 零 I/O · golden 向量钉死
                    └──────┬───────┘
          ┌────────────────┼────────────────┐
          ▼                ▼                ▼
     ┌─────────┐     ┌──────────┐     ┌──────────┐
     │ ANetHub │◄───►│   ANet   │◄───►│ ANetLink │
     │  中继   │ C2  │  daemon  │ C1  │  设备    │
     └─────────┘     └──────────┘     └────┬─────┘
                                            │ 真实协议
                                       ┌────▼─────┐
                                       │ ANetMock │
                                       └──────────┘
```

---

## 二、ANetCore · 协议内核

### DONE

| 包 | 内容 |
|---|---|
| `coredet` | CoreDet-CBOR,全栈唯一编码器 |
| `anetcid` | CIDv1·dag-cbor·sha2-256,前缀冻结 |
| `aobj` | AObjEnvelope,签名绑定 CID,先验证后使用 |
| `identity` | KEL/AID,KERI 预轮换,**AID 跨轮换稳定**,撤销时间闸 |
| `tsir` | TaskDoc + 封闭谓词演算(全函数/无副作用/有界/非图灵完备) |
| `adp` | AgentCard、AdmitCard 闸、CardTombstone、CardStore |
| `agenturi` | `agent://` 规范形式 |
| `ael` | 按 DID 的防分叉哈希链 |
| `effect` | 诚实效果信封 + 双轴信任 + Quirk |
| `evidence` | Receipt + Review(v0.5.x 从 ANet `internal/` 搬入) |
| `delegation` | 委派线 + `VerifyDelegateReq` + `VerifyResult`(v0.5.3) |
| `relayauth` | 中继鉴权挑战,四 action 域分离,5 分钟窗口 |
| `golden` | 一致性向量 |

### TODO

| # | 条目 | 阻塞 | 备注 |
|---|---|---|---|
| **C-1** | 治理纪元需要的 `GovernanceCert` | **阻塞 D-6** | **不要整包搬 `ascpevo`**:那是 631 行实验性协议(composite 内核、k*(T)、EVoI、Agent DNA),在 anet4 里**零消费者**。只有 `GovernanceCert` + `VerifyGovernanceCert` 约 120 行有用途。按 scope.md,单一消费者需要配一条 golden 向量才够格。等 D-6 真要做时再取那 120 行 |
| ~~C-3~~ | ~~golden 向量未覆盖新增包~~ | — | **已完成 v0.6.0**:7 条 wire 向量 + 冻结的一致性身份 `identity.SuiteController` |
| ~~C-6~~ | ~~评价互锁只在 Hub 内部~~ | — | **已完成 v0.6.1**:`evidence.VerifyInterlock`,10 项检查,第三方可独立复核 |
| ~~C-4~~ | ~~`K207` 被三个 README 引用但不存在~~ | — | **不成立**。`K207-anet4-module-architecture.md` 存在于 anet3 单体仓的 `docs/`。真正的问题变成:anet4 的架构文档留在被退役的仓库里 → 见 **C-5** |
| ~~C-5~~ | ~~anet4 架构文档仍在 anet3 单体仓~~ | — | **部分完成**:K207 → `ANet/docs/CONTRACTS-zh.md`,源码引用已改指本仓。K208(federation 草案)待 H-4 时迁入 ANetHub |
| **C-2** | design3 剩余协议包是否收录 | — | **默认答案是"不"**,除非出现第二个消费者或一条 golden 向量。ascpevo 的判例:631 行实验协议只为 120 行有用途的部分而整包搬入,是用臃肿修臃肿 |

---

## 三、ANet daemon

### DONE

**内核**:身份与多身份 · 能力注册表 C1 · 委派生命周期 · 委派验签(8 种冒充被拒) ·
**收据验证**(7 种"持有效签名仍撒谎"被拒) · **第三方验证 `anet verify`**(无 daemon/hub/网络) ·
证据链 P6/C5(含 `receipt_verified`) · 收据+评价 · 传输列表 · 对端 KEL 缓存 ·
控制面 21 端点 · CLI 28 个子命令 · 自动回复(exec + vision)

**七个可插拔模块 + MCP 北向**(`-tags no_<name>` 后符号数为 0,`go tool nm` 验证):
`anetlink` · `cas` · `blackboard` · `org` · `p2p` · `inv1` · `inv2` · `mcp`

**anet3 迁移完成**,`internal/golden` 对 anet3 钉死四个规范 id(org_id、CogUnit id、
凭证 CID、blob CID),四个全等。

**联调 20/20**(`scripts/joint.sh`,四仓六进程)。
**实网**:hub.agentnetwork.org.cn 已换成 anet4 的 hub(2026-08-22),
ink93(普通用户)+ emax(纯 hub)+ dmax(服务节点)三节点跨公网跑通。
**场景 24/24**(`scripts/scenario.sh`,一个谁也不认识的 hub + 三个节点加入;`--live` 加本地 caption 模型与租用前沿模型)。

### TODO

| # | 条目 | 依赖 | 优先级 |
|---|---|---|---|
| ~~D-1~~ | ~~MCP 接入面~~ | — | **已完成**。`anet mcp` stdio 服务,7 个工具(agents_find / task_delegate / task_results / task_inbox / task_message / task_end / node_status),`no_mcp` 可减。依赖代价 24 条(82 → 100) |
| ~~D-2~~ | ~~C5 证据链查询接口~~ | — | **已完成**。`anet evidence`、控制面 `POST /evidence`、MCP `evidence_read`;每条带 id / prev_id / 签名,可核而非可信;`head.state` 暴露 QUARANTINED |
| ~~D-3~~ | ~~按 C1 能力 id 发现~~ | — | **已完成**。`anet find --cap <id>`、控制面 `capability` 字段、MCP `agents_find.capability`;支持 `ptz.*` 族查询 |
| ~~D-12~~ | ~~广告的 caps 与实际服务的能力 id 是两份清单~~ | — | **已完成**。注册时由 daemon 把 provider 真正提供的 id 折进去 —— 精确查找一上线就把这个错位暴露了:节点报 `digest`、实际服务 `text.digest` |
| ~~D-4~~ | ~~federation 感知~~ | — | **已完成**。投递本就由 hub 转发(子面 A);缺的是付费能力的跨 hub 结算,现由两 hub 互相清算解决 |
| ~~D-5~~ | ~~结算~~ | — | **已完成**。能力可标价,报价是 PAYMENT_REQUIRED 完整答案,授权绑定金额/收款方/账本/交互,双方各自上链 |
| ~~D-13~~ | ~~付费闭环从未真的跑通~~ | — | **已完成**。`PayAndRetry` 写好了却**零调用点零测试**——编译得过的接缝不等于接上了的接缝,这是本月第三次同类错误。现有 `anet delegate --pay`、控制面 `pay:true`、MCP `task_delegate.pay` + `credit_balance`,scenario 第 6.5 节把报价→授权→结算→干活→双链→余额整条断言了一遍。联调还顺带查出两个单测看不见的真 bug(见下) |
| ~~D-14~~ | ~~结算收据生成后被丢掉~~ | — | **已完成**。hub 签了收据,`ExtReceipt` 在生产代码里**零引用**——付款方拿到一串自己无法核验的 transaction,等于信"对方说它收到钱了"。现在收据随结果回到付款方,付款方对着 hub 的 KEL 验签后才记 `anet.payment.settled{verified:true}` |
| ~~D-21~~ | ~~网关付款只有 fixture 能签~~ | — | **已完成**。联调一直用 `anetfixture x402-authorize` 签网关付款,意味着**真实买家会走的那条路,恰恰是没有任何东西走过的那条**。现在是 `anet x402-authorize`,只打印 PAYMENT-SIGNATURE 的值,可直接管进 curl。签名不等于花钱:网关不去结算就什么都没动,测试断言了余额未变 |
| ~~D-22~~ | ~~实网从没真的买过一张凭证~~ | — | **已完成**。prodtest 第 7 节此前只验"回了 402",那只证明报价。dmax 上那个公开兑付口在 loopback 之外**一次都没用过**。现在真在 fmax 付、把凭证带到 dmax 直兑、断言活跑了、断言第二次被拒、断言上了 dmax 自己的链。40 → 45 项 |
| ~~D-19~~ | ~~换 hub 之后旧 hub 仍在收活,且静默吞掉~~ | — | **已完成**。只有"加入",没有"离开":节点改指到第二个 hub 之后,第一个 hub 仍把它列为本地、仍接受投递、仍排进一个**再也不会被轮询的信箱**。活被接收、排队、然后无声无息地消失。实网跑 prodtest 时抓到 —— 一次跨 hub 调用被投进死信箱而不是跨过去,没有任何报错,请求方就那么等着。现在有 `anet hub-leave <hub>` / `POST /agents/{aid}/deregister`,**由 agent 自己签名**(谁替你说话是你的事,能被运营商赶走的注册不算注册)。删的是路由(registry 行、能力索引、卡片),**证据一律不动** —— 评价和账本记的是发生过的事,为了让目录好看而删掉它们等于改历史。未投递的信件会报数:有人发了活正在等 |
| ~~D-20~~ | ~~129 毫秒的时钟差就能作废一笔付款~~ | — | **已完成**(ANetCore v0.12.0)。`now < IssuedAt` 是零容差的:付款方在 T 签名,结算 hub 的钟慢了 0.1 秒,于是一张"签发于 hub 的未来"的授权被判为尚未生效。付款本身毫无问题,只是两台机器对"现在"的看法不同 —— 而两台机器永远如此。所有单进程测试都不可能造出这个偏差,是两个机房的真机跑出来的。现在 `ClockSkew = 2 分钟`,两端都给。窗口仍然有意义:一小时是"留着的授权或伪造的钟",两分钟是时钟差,线画在这里 |
| ~~D-17~~ | ~~付费是内核代码,不可插拔~~ | — | **已完成**。上一轮我往内核塞了 838 行无 tag 的付费代码(内核 11.6%),外加一个**公开监听口** —— 内核里其他任何东西都没有这个安全姿态。现在是 `module/x402`,`-tags no_x402` 后符号数 0。内核只留"驱动委派"那部分(那本来就是内核干的活),模块拿到的是一个具名窄口 `module.PaymentSeam`:以本节点身份签一次名 + 结算 hub 的身份。签名就是付款,所以口子小不下去 |
| ~~D-18~~ | ~~没有付费模块时会免费干标价的活~~ | — | **已完成**。拆模块时发现的:内核若直接跳过付费逻辑,一个标价 25 credits 的能力在无付费构建上会**照干不误**,而且全绿。现在答 UNAVAILABLE 并说明原因 —— "我收不了你的钱,所以我不干"是真话,"给你,不要钱"是没人做过的决定 |
| ~~D-15~~ | ~~x402 凭证兑付面~~ | — | **已完成**。`voucher_addr` 开一个**公开**监听口,买家拿 hub 签的凭证直接来兑;一次性由 daemon 把关(hub 无从知道凭证用没用过),spent 集合从证据链回读所以重启不失效。代价写在注释里:NAT 后的节点不能这样卖 |
| ~~D-16~~ | ~~兑付/提现~~ | — | **已完成**。`anet redeem <amount> --ref <ref>` 签一笔"付给 hub"的授权,credit 真的离开流通;hub 签字说明取走了多少,节点验签后上链 `anet.credit.redeemed` |
| **D-6** | 治理纪元 `govepoch` | **C-1** | 低。org 目前只接受 epoch 0 |
| ~~D-7~~ | ~~崩溃恢复语义未验证~~ | — | **已完成**。中继在处理**之后**才 ack(对的:先 ack 会丢工作),代价是崩在中间会重投。查出并修掉两处:重投的委派会**再执行一次**(第二次物理效果、第二张收据、第二条链记录),重投的结果会**在证据链上多记一条**。投递是 at-least-once 且只能如此;执行不是 |
| ~~D-11~~ | ~~重投的聊天消息会在记录里重复~~ | — | **已完成**。ANetCore v0.7.0 给 `ChatMsg` 加了发送方铸造的 `MsgID`(key 9,可选),接收端用 `(interaction_id, msg_id)` 的**部分**唯一索引去重 —— 部分是因为旧发送方不铸 id,而"未知"不是一种身份 |
| ~~D-8~~ | ~~自动回复未进联调~~ | — | **已完成**。`scripts/scenario.sh --live` 里 B 用 OpenRouter 真的回答了 C |
| ~~D-24~~ | ~~`ANET_HOME` 会让命令操作到别的身份~~ | — | **已完成**。`SelectionIsExplicit` 的清单是 `--id/ANET_ID/ANET_DATA_DIR/current`,漏了 `ANET_HOME`。设置它会移动身份容器,但容器里还没有 config 时,解析回落到 uid 指针,操作到当时正在跑的那个 daemon 上。实际后果:`ANET_HOME=/tmp/x anet hub-register <hub> --name throwaway` 把线上节点的 name 与 caps 在真 hub 上改成了 throwaway 那一套。这正是 strict 路径存在要防的事,由清单里唯一漏掉的那个变量造成。发现于用临时身份给 hub-leave 写生产断言 |
| ~~D-25~~ | ~~自动回复只在 scenario 里跑过~~ | — | **已完成**。没有任何生产节点配过它,所以"节点无人值守也能作答"这条路径从未跨过真网络。现在 cmax 上接 OpenAI 兼容后端,prodtest 9s 断言:委派到达 → 调模型 → 答复经真 hub 回到发起方。缺凭据时跳过而不是失败 —— 为缺 key 报红会教运营者忽略红色 |
| ~~D-26~~ | ~~INV-1 的运行时守卫零调用点~~ | — | **已完成**。`inv1.GuardCommonsPublish` 在本仓无任何调用点,而它的 doc.go 写着"每个发布边界都调用它" —— 它为之而写的 gossip/DHT 边界属于上一代,拆分时没带过来,不变式一直空成立。现接到 `screenPublication`(注册与档案发布),那是 anet4 真正会被第三方读到的路径。**p2p 发送路径刻意不接**:点对点直连一个具名对端,暴露面与 hub 中继相同,而中继本就按设计承载 CogUnit;接上去会拦掉正当路径,且那里载荷已是字节而守卫读静态类型。守卫本身也补了值遍历 —— 本仓发布的一切都是 `map[string]any`,只走类型图会在 `interface{}` 处通过,看不见运行时值的运行时绊线不是绊线 |
| **D-9** | 分发形态 | 无 | 中。今天需 `go build` + 终端。桌面 app / 浏览器扩展 / 托管三选待议 |
| ~~D-10~~ | ~~三处无直接测试~~ | — | **已完成**。`internal/hubapi` 钉住跨仓库字段名(第一次跑就抓到 `home_hub` 漂移:hub 一直在发,daemon 结构体里没有,于是每个联邦来的 agent 都被悄悄抹掉了"该去哪找它");`module/anetlink` 测工厂校验 + 用反射守住 C1 红线(daemon 永远不该知道"设备"是什么);`tools/anetfixture` 现在是联调网关付款的依赖,测它签出来的授权 hub 真的会认、两次不同 nonce、缺参数报得清楚 |

---

## 四、ANetHub

### DONE

中继邮箱(store-and-forward) · KEL 签名鉴权 · 注册/档案/评价 ·
**收据与评价验证**(全网唯一在验的地方,这是"Hub 伪造不了一条评分"的支点) ·
guest 模式 · taskboard(真 KEL 集成测试) · federation(K208 集连 sub-plane A:hub 间转发) ·
admin 面(manifest / OKF 数据集) · webui 入网 runbook · C2 wire contract 版本头 ·
可插拔构建标签(`no_taskboard` / `no_federation`)

### TODO

| # | 条目 | 阻塞 | 备注 |
|---|---|---|---|
| ~~H-1~~ | ~~发现是 `LIKE %q%` 子串匹配~~ | — | **已完成**。`agent_cap` 索引表 + `?cap=` 精确/前缀查询,保留原有逗号 OR 语义;旧库自动 backfill(生产上已验:升级前注册的 dmax 升级后可按 id 查到) |
| ~~H-2~~ | ~~不发布 KEL~~ | — | **已完成**。`GET /agents/{aid}/kel`;`anet verify --receipt X --hub URL` 自己取密钥历史。第三方验证闭环打通:只有收据 + hub 地址即可核验,通过与拒绝各实测一次 |
| ~~H-3~~ | ~~无结算~~ | — | **已完成**。hub 自 host x402 facilitator()、anet-credit 记账轨、注册赠额 + 运营商授予、跨 hub 互相清算 |
| ~~H-4~~ | ~~federation 只做了投递面~~ | — | **子面 B 已完成**:`GET /fed/v1/cards` 游标同步、三档可见性(默认 hub-local)、拉取而非推送、一跳纪律。**信誉联邦仍未做** → H-7 |
| ~~H-20~~ | ~~网页改动永远到不了生产~~ | — | **已完成**。hub 把 webui 打包进二进制,而 `build.sh` 不重建前端。嵌进去的是 2026-08-18 的构建,之后五次 hub 部署都带着它 —— 每一次网页改动都进了仓库、过了测试、没上线。现在 build.sh 在容器里重建并复制,复制后不一致就拒绝构建;prodtest 断言线上页面含当前构建的标记 |
| ~~H-21~~ | ~~hub 无法清偿欠款~~ | — | **已完成**。`IssueOwedSettlement` 无调用点,`hub_owed` 只升不降。实网上 `hub_owed` 空、`hub_cleared` 0 条,看着像"还没欠过",实际同时是"欠了也没法结"。现在 `anet-hub -clear <peer-aid> -amount <n> -peer-endpoint <url>` 签字并投递 —— 投递而非打印,因为只放在债务人自己盘上的清偿声明清偿不了任何东西 |
| ~~H-17~~ | ~~发放事件无外部见证,供应量声明不可证伪~~ | — | **已完成**。`/x402/supply` 从 hub 自己写的行计算,只能证明内部一致。转账有对手方(结算收据 + 双方证据链),发放没有。现在每笔供应变化进一条仅追加、哈希链接、hub 签名的链(`ael`),`/x402/issuance` 公开;`/x402/supply` 同时给出链与账表两种推导及是否一致。链之前的存量记为 `chain_opening` 单列,不并入 `chain_issued` —— 前者是 hub 关于自己过去的自述,后者可被见证 |
| ~~H-18~~ | ~~链只对已有旧副本的读者可验~~ | — | **已完成**。见证:第三方取链头并签名"某时刻头是 N/id X",之后同位置出现不同记录即构成改写证据,不需任何一方承认。见证品存在**见证者**手里才算数,hub 也收并公布但只是给读者起点(能扣不能伪造)。hub 之间默认开(`"witness":"off"` 可关),agent 默认关(`witness_hub: true` 可开)—— 精简版节点不该被要求为网络干活 |
| ~~H-19~~ | ~~结算移动余额但不写流水~~ | — | **已完成**。`SettlePayment` 只改 `credit_balance`,`/agents/{aid}/ledger` 因此只显示赠额,agent 看不到自己付了什么收了什么,余额与流水差额恰等于经支付流动的部分。`ClearFromPeer` 收款侧同样缺失。由 `anet reconcile` 在生产上第一次运行时查出 |
| ~~D-23~~ | ~~节点无法自查 hub 的账~~ | — | **已完成**。`anet audit-hub`(验发放链 + 与本节点此前记录的链头比对)、`anet reconcile`(本节点签过/收到的付款 vs hub 的流水)。两者都在 `module/x402` 内,`no_x402` 后符号数 0。合计从签名记录重算,不采信 hub 渲染的摘要 |
| ~~H-15~~ | ~~节点"死掉"之后 hub 仍在收活并静默吞掉~~ | — | **已完成**。`hub-leave` 修的是搬家,死亡没修 —— 机器关了、进程被杀、人走了,agent 照样列在目录里、照样可投递,活进一个永不被轮询的信箱。信号取"取信"这个动作(不是心跳端点:能取信的节点才会干活,而且它本来就在做,不用为证明存活再造一个东西)。**什么都不删**:网络抖一下不是注销,会把每次故障变成集体注销,还会让所有人的评价和余额成为孤儿。两档 —— 一小时未取信在目录里标记、投递时警告发送方;一个月未露面退出可浏览列表,仅此而已,一次取信就回来。没有记录的行读作"未知"而非"已死" |
| ~~H-16~~ | ~~webui 少了三个 hub 一直在发的字段~~ | — | **已完成**。`api.ts` 注释写着"字段与 aghub 的 JSON 一一对应",但没人检查。它早就漂了:`home_hub`、`last_seen`、`quiet` 一个都没有 —— **页面分不清本地和联邦来的 agent,也看不出谁已经不再应答**。TypeScript 乐于描述一个比实际更窄的形状,多出来的字段在边界上无声消失。跟 daemon 读 `balance` 而 hub 发 `credits`、跟 `hubapi` 少 `home_hub` 是同一个病。现在有跨语言契约测试(放在 Go 侧,因为线协归 Go 管),两个方向都会咬 |
| ~~H-13~~ | ~~拒收一次的卡片会被游标永久越过~~ | — | **已完成**。同步循环对所有拒收一视同仁:记日志、跳过、推进游标。对畸形卡片和坏签名这是对的(那种错是永久的,卡在上面会让一个坏卡片堵死整条流);对"这个 agent 注册在本地"就是错的 —— 那是**关于今天的事实**。实网上 dmax 后来搬走了,卡片变得可收了,而 emax 再也不会去要它:agent 就那么从目录里消失了,而且**任何地方都没有一行日志说为什么**。现在拒收分两类,`ErrRefusedForNow` 挂在 Directory 契约上(要据此行动的是同步循环,不是存储);瞬时拒收会把流停在那张卡片上并每轮报一次 |
| ~~H-14~~ | ~~已经越过去的游标无法自愈~~ | — | **已完成**。停住游标只能防住以后,救不回已经跳过的 —— 而实网上正好已经跳过了一张。现在每 15 轮(稳态节奏下约半小时)从头重读一次 peer 的目录。收录是 upsert,所以一次全量只花一次重读、不改任何已经正确的东西。它能修的不只是这个 bug,还有下一个我还没想到的 |
| ~~H-11~~ | ~~跨 hub 的交互根本无法被评价~~ | — | **已完成**。评价上传要求"双方都注册在本 hub",而跨 hub 交互的双方**按定义**分处两地 —— 联邦一直在产出没有任何人能评分的工作。现在评价方必须是本地用户(这是第一手的),主体只要本 hub 能识别即可(本地或联邦卡片);互锁仍在同一份 KEL 上验,所以放宽的是"我认识谁",不是"要不要验"。同步流也有同一个假设(JOIN 本地 agent 表),一并修掉 |
| ~~H-12~~ | ~~联邦评价里"本地主体一律拒收"是错的~~ | — | **已完成**。我上一轮的规矩看着稳妥,代价是跨 hub 工作的 agent 永远攒不到那一半的评分。peer **伪造不了**对我方 agent 的评价 —— 锚点是我方 agent 自己签的收据 —— 所以传过来的要么是真交互要么什么都不是。真正兜住风险的是已经在的按来源分列:peer 的评价进 peer 那一列,永不并入本地 |
| ~~H-7~~ | ~~信誉联邦未做~~ | — | **已完成**。`GET /fed/v1/reviews` 游标同步**签名证据本身**而非聚合值,收方用与本地上传完全相同的 `VerifyInterlock` 复核。**不合并成一个数**:peer 伪造不了评价(每条都是评价方签名与提供方收据的互锁),但 peer 可以自己注册账号给自己人刷分——那些评价条条为真。所以按来源分开记,并公布 `concentration`(最大单一来源占比),旁边写明合并值不能证明什么 |
| ~~H-8~~ | ~~hub 从不公布自己的密钥历史~~ | — | **已完成**。它给所有 agent 发 KEL,唯独不发自己的——而它签结算、签兑付收据、签凭证。"托管方做了什么你可以自己验"于是对对象成立、对系统不成立。**这个洞只有联调能发现**:我写的 fake hub 把自己注册进了 registry,真 hub 没有,fake 比真货更完整 |
| ~~H-9~~ | ~~credit 只进不出~~ | — | **已完成**。`POST /x402/redeem` 销毁额度并签字;`GET /x402/supply` 公布已发行/已兑付/未清偿,且 `outstanding == balances` 是任何人都能自己算的等式——发放同时记 hub 自己那一行的负数,全账求和恒为零。`POST /federation/clear` 让 `hub_owed` 能降下来,不再只升不降 |
| ~~H-10~~ | ~~hub 只是 facilitator,不是 resource server~~ | — | **已完成**。`GET /x402/resource/{aid}/{capability}`:未付款回 402 + `PAYMENT-REQUIRED`,付款后回 `PAYMENT-RESPONSE` 与一张**凭证**。**网关只卖门票不代理内容**——hub 全程见不到请求与结果,这和中继"只搬读不懂的字节"是同一条性质。价钱与取货地址都读自 agent 自己签的卡片,所以 hub 能拒卖、不能改价、不能把买家指到自己的机器上 |
| **H-22** | 静默两档的时间跨越只在单测里 | — | 一小时标记静默、一个月退出可浏览列表,两个阈值在实网上无法产生 —— 要么等,要么改生产数据。prodtest 9q 断言的是单测覆盖不到的那一半:信号确实取自真实取信而不是心跳端点、"无记录"不被当成"已静默"、`hub-leave` 删路由留证据。**跨越本身仍然只有单测**,这是有意的取舍,不是漏测 |
| ~~H-6b~~ | ~~taskboard 在套件里没有客户端~~ | — | 九个变更端点都要 KEL 签名的挑战,而包自身测试之外没有任何东西能产生一个 —— 板子可读不可用,实网上从未被碰过。**已完成**。现在有 `module/taskboard`(`no_taskboard`,符号数 22 → 0,CI 矩阵已同步),三个能力:读板、建卡、领取。不是九个 —— 读板、放活、接活是 agent 参与所需,move/block/reject 是人在 UI 里做的协调。`module.Host` 为此新增 `HubSeam`(只有 Sign 与 HubURL),它比 `PaymentSeam` **小**而不是重复:一个只需向自己 hub 认证的模块拿到付费口,等于白拿 hub 的密钥历史与本节点的证据。prodtest 9r 两侧都测:`anetfixture relay-sign` 驱动完整流转(created→ready→claimed→submitted→accepted,乱序与未签名被拒),模块侧证明 agent 不用 fixture 也能参与 |
| **H-5** | 测试密度偏低 | — | Go 侧持续增长。webui 2,737 行从 42 → 54 个测试:导出 `Transcript`/`Review`、拆出 `Column` 之后,详情弹窗与任务板可测了。仍未覆盖:ChatDialog(283 行)、JoinSection(235 行)、Header/Hero/Footer/Starfield/Toast |
| ~~H-6~~ | ~~部署链路上有三层体积上限~~ | — | **已完成**。 把决定性的那层放进仓库,并在文件头写明三层的名字与位置 |

---

## 五、ANetLink

### DONE

**14 个适配器**:onvif · hikvision · dahua · modbus · opcua · bacnet · can · zigbee ·
btmesh · ble · thread · mqttbridge · habridge(19 个 HA domain) · sim

**quirk 厂商偏差修正层**(D03,设计了很久没人建) · C1 北向(UDS + HTTP,CBOR/JSON) ·
**MCP server** · **ADAP**(C4 进程外适配器逃生舱) · 指纹目录 ·
15 个可插拔构建标签 · 317 个测试

### TODO

| # | 条目 | 依赖 | 备注 |
|---|---|---|---|
| ~~L-5~~ | ~~ADAP(C4)从未有适配器说过它~~ | — | **已完成**。实现与插件接线俱全,而包自身测试之外零使用 —— "第三方进程能服务设备、运行时分辨不出差别"是一条没有实况证据的设计属性。新增 `cmd/adapdemo`(照线协手写、不 import adap 包,那是第三方作者的处境)。第一次真跑查出三个缺陷:① `runtime.Invoke` 对 Profile 为 nil 的设备解引用,整个进程 panic,而 claim 与 describe 之间的窗口每个适配器都要经过、且从 C1 socket 可达;② `c1serv`/`mcpserv` 用 `Profile.Protocol` 拼设备 key 而运行时用 `Adapter.Info().Name` 存,编译进来的适配器两者恰好相同所以一直没暴露,ADAP 让它们天然不同,于是这类适配器的能力全部广告得出去、调不到;③ `onDescribe` 对一个解码干净但零能力的 profile 回 `accepted: true`(`DeviceProfile` 无 json tag,写 `capabilities` 而非 `Caps` 会让整列表消失且不报错)。实网:`climate.setpoint` 报 OK/verify_trust 2,`climate.boost` 报 UNVERIFIED |
| ~~L-4~~ | ~~ANetLink 从未进过生产,依赖落后九个小版本~~ | — | **已完成**。ANetCore 钉在 v0.4.2 而其余三仓在 v0.13.1,daemon 与 ANetLink 之间的 C1 线协从未被验证过是否还对得上。换到 v0.13.1 后无需任何改动、317 测试全绿 —— 漂的是版本钉不是线协,但这一点在跑之前无从得知。现在 dmax 上跑 anetlinkd(sim 适配器),prodtest 9p 断言整条链:适配器发布设备 → 运行时上 C1 口 → daemon 把真实能力 id 折进注册 → hub 索引 → 另一台机器另一个 hub 的节点委派它。实网 `light.onoff@sim/lamp-1`,`power_state=1`。能力 id 里带着设备,而 daemon 仍然不知道"设备"是什么 —— 这就是 C1 |
| **L-1** | L2 真机测试 | 真机 + 凭据 | PTZ / 事件 / JPEG 抓拍(onvif-server 只实现 Profile S);海康 ISAPI 与大华 CGI **不存在模拟器** |
| **L-2** | 厂商云适配器:一个都没有 | — | 生态缺口。对标同类产品这是主要差距 |
| **L-3** | 自动发现只有 ONVIF WS-Discovery | — | 其余协议靠配置 |

### 持续约束(不是 TODO,是红线)

**ANetLink 永不读取、永不分发 nmap 数据。** NPSL §3 衍生作品条款。
OUI 数据来自 IEEE MA-L。

---

## 六、ANetMock

### DONE

**6 个协议前端**:onvif · isapi(海康) · dahua · modbus · mqtt · z2m —— 真 SOAP、
真 ISAPI/CGI 线格式、真 multipart 事件流、真 UDP/TCP socket ·
29 个设备型号 · 3D 场景与 DOM 标记 · `planimport`(平面图 → 场景) ·
f6 资产拆解为语义数据

### 刻意不做

**ANetMock 不说 ANetCore 的类型。** 它若发出一个成品 `Effect`,就把被测的东西本身
抹掉了 —— 见 `ANetMock/DECISIONS.md`。这不是欠债。

### TODO

| # | 条目 | 备注 |
|---|---|---|
| ~~M-3~~ | ~~ANetMock 从未真的驱动过 ANetLink~~ | **已完成**。它存在就是为了用真 SOAP / ISAPI / Dahua CGI 线格式测适配器,`joint.sh` 的注释里画着这条链而从未真跑过 —— 于是适配器一直只对着写适配器的人自己写的 fake 被测。现在 dmax 上跑 office 场景(148 台设备、10 个 ONVIF 端点),`anetlinkd -tags adap,onvif` 接上其中两台,41 个能力上了实网 hub。跨机委派实测:`ptz.move` 报 OK 且带真实读回(`pan 0→0.03`),`stream.rtsp` 报 UNVERIFIED 并说明"URI 已给出、流未探测"。**L-1 的性质因此变了**:PTZ 与抓拍不再是"不存在模拟器",而是"没有对真机测过",那是更小的缺口 |
| **M-1** | 只有 office 一个场景 | 回滚后的刻意选择:打磨一个胜过五个都丑。`-venue` 帮助曾仍在宣传另外四个,2026-08-22 已改正 |
| **M-2** | 测试密度偏低 | 5,910 行对 29 个测试 |

---

## 七、跨仓依赖链

只有四条真链,其余条目彼此独立、可并行:

```
C-1 (ascpevo)  ────────────────────►  D-6 (govepoch)
H-1 (能力索引) ────────────────────►  D-3 (按能力 id 发现)
H-3 (结算)     ────────────────────►  D-5 (daemon 结算)
H-4 (目录联邦) ────────────────────►  D-4 (daemon federation 感知)
H-2 (发布 KEL) ────────────────────►  第三方验证闭环(无编号,跨仓能力)
```

**已知未解 · ANetLink 线协可能已不兼容**:它的 ANetCore 依赖停在 v0.4.2,当前 v0.13.1,中间九个版本改了 delegation(`MsgID`)、加了整套 payment、ael 见证。它自己 297 个用例全过,但**与 daemon 之间的线协没有任何东西在检查**。本轮决定继续挂着,风险如实记在这里。

**已知未解**:见证覆盖取决于有人愿意见证。一个只有一个 peer、且没有 agent 开启见证的 hub,其发放链只对已经拉取过它的读者可验分叉。公开锚定(OpenTimestamps / Rekor)能去掉这个依赖,本轮评估后决定不做 —— 它引入外部依赖,而当前的信任模型(联邦 peer 互为见证 + agent 可选见证)已经把"信任 hub"降到了"不是所有见证者都与 hub 合谋"。

**没有依赖、可以立刻开工的高价值项**:`D-1`(MCP)、`D-2`(证据链查询)、
`H-2`(发布 KEL)、`C-3`(golden 向量)、`L-2`(厂商云)。

---

## 八、经验:哪些缺陷只有联调能发现

这一节不是清单,是给后来者的:**下列五个缺陷,两边单测各自伪造对方时全部全绿。**

1. 能力委派路径无人可达 —— `--capability` 落进 goal 文本,没有解析器看那里
2. 读能力跨线只写不读 —— CID/blob/列表放在 `ObservedState`,而交付物只带
   `map[string]float64`,装不下
3. ANetLink 的 C1 线上没有 quirk 字段 —— 修正标签在自己的北向边界就停了
4. 证据链用无法表达自身 id 的编码落盘 —— 首次接受结果后重启,daemon 拒绝加载自己的历史
5. p2p 传输送不回一个回复 —— 入站处理占住了那条必须送回复的读循环

再加一个不是联调发现、而是**清点时发现**的:收据从来没人验过。
`Receipt.Verify` 在 daemon 里调用点为零,且不可能不为零 —— 回程根本不带 provider 的 KEL。

还有一类,只有**双 hub 实网**能发现,单 hub 联调造不出来:

7. 跨 hub 付款在两个账本上各造一份 credit —— 付款方 hub 不分本地与否给 payee 记贷,
   收款方 hub 凭收据又记一次。两个 hub 各自内部一致(各自 balances == outstanding),
   所以任何一侧的供给检查都看不到;能看到的只有两侧之和
8. 跨 hub 的 credit 变动两侧都不入发放链 —— `chain_outstanding == outstanding` 在
   跨 hub 付款那一刻在两侧同时失效,失效原因是漏记而非账本有问题
9. 卖方 402 只报自己 hub 的账本 —— credits 在别处的买方只能收到 insufficient funds,
   跨 hub 清算路径存在而无法到达。`ClearFromPeer` / `SettleOwed` / `hub_owed` 因此
   写好之后长期零触发
10. `/agents/{aid}/ledger` 默认 100 条且不标记 —— `anet reconcile` 拿一页之和对全额
    余额,凡历史超过一页的账户都报出一个由上限而非账本产生的差异
11. p2p 会合点是共享文件系统目录 —— 两台主机没有共享目录,唯一为绕开 hub 而建的
    模块只能在同一台机器上的节点之间使用,恰好是不需要它的那种情况

结论:`scripts/joint.sh` 是仓库的一部分,不是脚手架。`scripts/prodtest.sh` 同理 ——
上面第 7 到 11 条,没有一条能在单机上造出来。
