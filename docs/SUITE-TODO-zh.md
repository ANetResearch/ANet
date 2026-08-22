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
| ~~D-17~~ | ~~付费是内核代码,不可插拔~~ | — | **已完成**。上一轮我往内核塞了 838 行无 tag 的付费代码(内核 11.6%),外加一个**公开监听口** —— 内核里其他任何东西都没有这个安全姿态。现在是 `module/x402`,`-tags no_x402` 后符号数 0。内核只留"驱动委派"那部分(那本来就是内核干的活),模块拿到的是一个具名窄口 `module.PaymentSeam`:以本节点身份签一次名 + 结算 hub 的身份。签名就是付款,所以口子小不下去 |
| ~~D-18~~ | ~~没有付费模块时会免费干标价的活~~ | — | **已完成**。拆模块时发现的:内核若直接跳过付费逻辑,一个标价 25 credits 的能力在无付费构建上会**照干不误**,而且全绿。现在答 UNAVAILABLE 并说明原因 —— "我收不了你的钱,所以我不干"是真话,"给你,不要钱"是没人做过的决定 |
| ~~D-15~~ | ~~x402 凭证兑付面~~ | — | **已完成**。`voucher_addr` 开一个**公开**监听口,买家拿 hub 签的凭证直接来兑;一次性由 daemon 把关(hub 无从知道凭证用没用过),spent 集合从证据链回读所以重启不失效。代价写在注释里:NAT 后的节点不能这样卖 |
| ~~D-16~~ | ~~兑付/提现~~ | — | **已完成**。`anet redeem <amount> --ref <ref>` 签一笔"付给 hub"的授权,credit 真的离开流通;hub 签字说明取走了多少,节点验签后上链 `anet.credit.redeemed` |
| **D-6** | 治理纪元 `govepoch` | **C-1** | 低。org 目前只接受 epoch 0 |
| ~~D-7~~ | ~~崩溃恢复语义未验证~~ | — | **已完成**。中继在处理**之后**才 ack(对的:先 ack 会丢工作),代价是崩在中间会重投。查出并修掉两处:重投的委派会**再执行一次**(第二次物理效果、第二张收据、第二条链记录),重投的结果会**在证据链上多记一条**。投递是 at-least-once 且只能如此;执行不是 |
| ~~D-11~~ | ~~重投的聊天消息会在记录里重复~~ | — | **已完成**。ANetCore v0.7.0 给 `ChatMsg` 加了发送方铸造的 `MsgID`(key 9,可选),接收端用 `(interaction_id, msg_id)` 的**部分**唯一索引去重 —— 部分是因为旧发送方不铸 id,而"未知"不是一种身份 |
| ~~D-8~~ | ~~自动回复未进联调~~ | — | **已完成**。`scripts/scenario.sh --live` 里 B 用 OpenRouter 真的回答了 C |
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
| ~~H-11~~ | ~~跨 hub 的交互根本无法被评价~~ | — | **已完成**。评价上传要求"双方都注册在本 hub",而跨 hub 交互的双方**按定义**分处两地 —— 联邦一直在产出没有任何人能评分的工作。现在评价方必须是本地用户(这是第一手的),主体只要本 hub 能识别即可(本地或联邦卡片);互锁仍在同一份 KEL 上验,所以放宽的是"我认识谁",不是"要不要验"。同步流也有同一个假设(JOIN 本地 agent 表),一并修掉 |
| ~~H-12~~ | ~~联邦评价里"本地主体一律拒收"是错的~~ | — | **已完成**。我上一轮的规矩看着稳妥,代价是跨 hub 工作的 agent 永远攒不到那一半的评分。peer **伪造不了**对我方 agent 的评价 —— 锚点是我方 agent 自己签的收据 —— 所以传过来的要么是真交互要么什么都不是。真正兜住风险的是已经在的按来源分列:peer 的评价进 peer 那一列,永不并入本地 |
| ~~H-7~~ | ~~信誉联邦未做~~ | — | **已完成**。`GET /fed/v1/reviews` 游标同步**签名证据本身**而非聚合值,收方用与本地上传完全相同的 `VerifyInterlock` 复核。**不合并成一个数**:peer 伪造不了评价(每条都是评价方签名与提供方收据的互锁),但 peer 可以自己注册账号给自己人刷分——那些评价条条为真。所以按来源分开记,并公布 `concentration`(最大单一来源占比),旁边写明合并值不能证明什么 |
| ~~H-8~~ | ~~hub 从不公布自己的密钥历史~~ | — | **已完成**。它给所有 agent 发 KEL,唯独不发自己的——而它签结算、签兑付收据、签凭证。"托管方做了什么你可以自己验"于是对对象成立、对系统不成立。**这个洞只有联调能发现**:我写的 fake hub 把自己注册进了 registry,真 hub 没有,fake 比真货更完整 |
| ~~H-9~~ | ~~credit 只进不出~~ | — | **已完成**。`POST /x402/redeem` 销毁额度并签字;`GET /x402/supply` 公布已发行/已兑付/未清偿,且 `outstanding == balances` 是任何人都能自己算的等式——发放同时记 hub 自己那一行的负数,全账求和恒为零。`POST /federation/clear` 让 `hub_owed` 能降下来,不再只升不降 |
| ~~H-10~~ | ~~hub 只是 facilitator,不是 resource server~~ | — | **已完成**。`GET /x402/resource/{aid}/{capability}`:未付款回 402 + `PAYMENT-REQUIRED`,付款后回 `PAYMENT-RESPONSE` 与一张**凭证**。**网关只卖门票不代理内容**——hub 全程见不到请求与结果,这和中继"只搬读不懂的字节"是同一条性质。价钱与取货地址都读自 agent 自己签的卡片,所以 hub 能拒卖、不能改价、不能把买家指到自己的机器上 |
| **H-5** | 测试密度偏低 | — | 本轮 35 → 48 个测试(卡片、能力索引、目录联邦)。webui 2,316 行仍基本无测试 |
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

结论:`scripts/joint.sh` 是仓库的一部分,不是脚手架。
