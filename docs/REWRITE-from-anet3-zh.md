# anet4 是全量重构 —— 从 anet3 继承了什么,没继承什么

**先把定位说清楚,因为这份文档以前把它说反了。**

anet4 不是 anet3 的迁移版本。anet3(AgentNetwork 单体仓)的 daemon 一个就有
**45,903 行**;anet4 的 daemon 是 **11,437 行**,套件五仓合计 61,506 行,而且拆成了
互不依赖的五个仓 + 一个零 I/O 的协议内核。这不是把旧东西搬过来。

从 anet3 **刻意复用**的是 1,796 行纯逻辑算法包 —— 占 anet4 全部代码的约 **3%**:

| 复用的包 | 行数 | 为什么值得复用 |
|---|---|---|
| `blackboard` | 498 | add-wins OR-Set + HLC,已在三节点实测中收敛过 |
| `org` | 697 | 单签凭证链 founder → admin → member,规范已冻结 |
| `cas` | 225 | 内容寻址,读回重新哈希 |
| `aelstore` | 254 | P6 证据链的落盘 |
| `inv1` / `inv2` | 122 | 两条不变式守卫 |

复用的判据只有一条:**确定性、无 I/O、且线格式已被规范钉死**。这类代码重写一遍
只会换来新 bug 和一次不兼容。其余一切 —— 内核、模块接缝、五个契约(C1–C5)、
传输列表、证据面、CLI、Hub、设备层 —— 全部是新写的。

**线格式没有变**,这是可验证的而不是声称的:`internal/golden` 把 org_id、CogUnit id、
凭证 CID、blob CID 四个承重标识钉在 anet3 算出的值上,四个逐字节相同。复用旧算法
而悄悄改了线,等于把家族劈成两半。

> 上一版这份文档的开头写的是"anet4 的 daemon 不是重写,是把 anet3 里已经跑通的
> 东西搬过来"。那句话是错的,而且错得有害 —— 它让一次 4 倍瘦身的重构听起来像一次
> 端口移植,也让"为什么不直接用 anet3"这个问题没有答案。答案是:anet3 过于臃肿,
> 而臃肿不是可以搬运掉的东西。

这份文档的其余部分是账目:**哪些搬了、哪些故意没搬、为什么。**
"没搬"必须写下来 —— 一份只列已完成项的迁移清单,读起来永远像完成了。

## 一、已迁移：daemon 内的可插拔模块

每个模块都带自己的构建标签，`-tags no_<name>` 之后二进制里**符号级为零**
（CI 用 `go tool nm` 计数验证，不是"配置关掉"）。

| anet3 (`internal/v3rt/`) | anet4 | 行数 | 说明 |
|---|---|---|---|
| `blackboard` | `module/blackboard` | 498 | 共脑协同黑板：add-wins OR-Set + HLC + 任务相位机 |
| `cas` | `module/cas` | 225 | 分布式存储：内容寻址，读回重新哈希 |
| `org` | `module/org` | 697 | 组织成员：genesis + 单签凭证链（founder → admin → member） |
| `inv1` | `module/inv1` | 83 | INV-1：org 数据不得进入 p2p |
| `inv2` | `module/inv2` | 39 | INV-2：公开发布不得携带 org 数据 |
| `transport` | `module/transport.go` + `module/p2p` | 246 | 传输列表；p2p 走进程外 |
| `aelstore` | `internal/daemon/ledger.go` | 254 | P6 证据链的落盘与 verify-before-use |

**互操作已验证**：`internal/golden` 把四个承重对象的规范 id 钉在 anet3
算出来的值上——org_id、CogUnit id、凭证 CID、blob CID，逐个相同。搬运
没有悄悄改线。

## 二、故意留在进程外：p2p commons 织物

| anet3 包 | 行数 | 去向 |
|---|---|---|
| `alpnet` / `axpnet` / `psrouter` | 782 | peer 进程 |
| `discovery` | 429 | peer 进程 |
| `commonsboard` / `commonscards` / `commonsaet` | 773 | peer 进程 |
| `commonsmatch` / `commonsengage` | 368 | peer 进程 |

**理由是算术，不是偏好。** anet4 内核解析 82 条 `go.sum`、两个直接依赖；
anet3 的 libp2p 栈带进 32 个 libp2p 模块、上千条依赖树。构建标签移除的是
**代码**，从来不是 `go.mod` 里的**依赖**：每次构建照样解析，CI 照样下载，
供应链审计照样要覆盖。为一个默认关闭的模块把内核依赖面扩大十二倍，是错的
交易。

所以 daemon 侧只有一个 `module/p2p` 客户端，用一行一个 JSON 帧的小协议
跟 peer 进程说话，把它当成普通 `module.Transport`。daemon 学不到 peer id、
multiaddr、pubsub topic —— C1 对设备划的那条线，在传输上再划一次。

`tools/anetpeer` 是这条线的参考实现：同机两个 daemon 通过一个 rendezvous
目录互相找到、直连投递，端到端确认。它**不是** libp2p peer 的替代品（没有
NAT 穿透、没有目录之外的发现），它是那份契约的完整陈述，两百行可读完。

## 三、故意不搬：org-central 服务端

| anet3 包 | 行数 | 为什么不搬 |
|---|---|---|
| `orgcentral` | 2791 | 中心化 org 服务端，不是 daemon 的能力 |
| `board` | 1022 | org 看板 / DAG |
| `orgstore` | 375 | org 产物归集 |
| `loops` | 338 | 循环任务定义 |
| `keychain` | 243 | org 群组密钥 |
| `im` | 155 | 成员间点对点消息 |
| `svcreg` | 109 | org 内服务注册 |

**这正是 v1 的教训本身。** 组织功能在 v1 里长进了三十五个 daemon 文件，
1.3.0-v3 开源时需要一把斧头和 683,852 行删除。缺的从来不是纪律，是接缝。
anet4 的 `module/org` 只回答一件事：**这份凭证是不是这个组织的有效成员**。
它不持有看板、不排任务、不存产物、不发群密钥。这些若要存在，是 org-central
自己的服务，通过 C1 提供能力，而不是 daemon 里多出七个包。

## 四、暂缓，有明确的解锁条件

| 项 | 阻塞在 | 说明 |
|---|---|---|
| 治理纪元 `govepoch` | ANetCore 的 `ascpevo.GovernanceCert` | 需要 design3 协议包落地。把协议类型抄进模块以免等待，正是上次造出两个分叉内核的做法 |
| `hybridsearch` (712) | 无技术阻塞 | FTS5 双语混合检索。是个合理的候选模块，但它是**功能**，不是 anet3 意义上的"可插拔能力"；没有需求前不搬 |
| `node` (261) | — | anet3 的集成装配层，已被 anet4 自己的 daemon 取代 |

## 五、验证方式

- **单元**：每个模块自带测试；`inv1`/`inv2` 用反射钉结构，新增字段会让测试失败而不是悄悄通过。
- **性能**：`module` 基准。黑板重复 add 曾与首次 add 同价（315µs）且**不安全**——验证跑在存在性检查之前，随后覆盖已存对象；提前返回后 3.3µs，快 95 倍且严格更安全。
- **互操作**：`internal/golden` 对 anet3 钉死四个规范 id。
- **联调**：四仓六进程（ANetMock ← ANetLink ← daemon → ANetHub ← daemon），
  逐项跑设备控制、CAS、黑板、org、p2p、证据链重启。当前 18/18。
  联调是唯一发现下列缺陷的手段——两边的单测各自伪造对方，全绿：
  能力委派路径无人可达、读能力跨线只写不读、C1 丢弃 quirk 标签、
  证据链用无法表达自身 id 的编码落盘、p2p 传输无法送回一个回复。
