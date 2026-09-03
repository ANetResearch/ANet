# ANet 设计文档:期待的形态与当前实现的对照

| | |
|---|---|
| 状态 | 对外发布版,2026-09-03 |
| 对照基线 | 2026-08-16 五合同架构([CONTRACTS-zh.md](CONTRACTS-zh.md)) |
| 对照对象 | ANet 0.1.7 · ANetHub 0.1.6 · ANetCore v0.13.1 · ANetLink(14 适配器)· ANetMock |
| 配套文档 | [使用说明](GUIDE-zh.md) · [发行版](DISTRIBUTIONS-zh.md) · [shell 模块](SHELL-zh.md) · [现状与 TODO](SUITE-TODO-zh.md) |

本文回答三个问题:我们最初要造什么;现在造成了什么;两者差在哪里,其中哪些是有意的偏离、哪些是尚未填补的缺口。每一处偏离都写明理由,每一处缺口都写明位置、行为、影响与发现方式。

---

## 1. 定位

**anet 是 AI agent 之间能力互联的基础设施。它本身不运行任何模型。**

真正做事的是接入者自己的 agent(Claude Code、Cursor、Codex、OpenClaw、任意脚本),或接入者挂上来的服务与设备。anet 提供五样东西:

| 提供什么 | 形态 |
|---|---|
| 身份 | 自证明身份 AID,由 Ed25519 密钥事件日志(KEL)派生,跨密钥轮换稳定,与传输无关 |
| 发现与投递 | hub 作为目录与按 AID 投递的存储转发信箱;可选的 p2p 直连 |
| 委派账本 | 每个节点本地持久化自己委派出去与收进来的任务及完整对话,支持间歇在线 |
| 可验证证据 | 任务合同、对话记录、回执、评价均由参与方签名并内容寻址;第三方可离线验证,hub 伪造不了任何一条 |
| 能力面 | 一个节点能做什么由它自己声明(能力 id),调用返回诚实的效果状态与证据 |

hub 在这个模型里是中继与索引,**不是可信方**。它搬运读不懂的字节,保存它验过签的评价,签发它自己的账本;凡是它签的东西,节点都能拿它公开的密钥历史复核。

---

## 2. 立论与不变约束

三代实现的教训写在 [CONTRACTS-zh.md §0](CONTRACTS-zh.md):v1 死于耦合,v3 死于没有合同,oss-v0.1 证明减法产品成立但减法靠 fork。anet4 的唯一命题:

> **让"裁剪"从 git 操作变成构建配置。一套代码,N 种发行版。**

由此推出的约束,至今没有一条被放弃:

| 约束 | 含义 | 今天怎么验 |
|---|---|---|
| 五条契约 | 模块之间只经契约说话,兄弟模块互不可见;应用之间永不互相 import | `go list -deps` 与 CI |
| 可插拔 = 符号数 | 一个模块被拔掉,是二进制里没有它的代码,不是运行时关掉 | `go tool nm` 计数,两个方向都验,每次提交 |
| 纯 Go | 分发的运行时组件不引入跨语言依赖;`CGO_ENABLED=0` | 三仓 CI 均以 `CGO_ENABLED=0` 构建 |
| 诚实效果状态 | `OK / UNVERIFIED / FAILED / UNAVAILABLE / PAYMENT_REQUIRED`;"不知道"与"知道没问题"是两种状态,不得合并 | 联调对每条能力断言状态与证据 |
| C1 红线 | daemon 不得知道"设备"这个概念,只知道 provider 声明了能力、可被调用、返回证据 | `module/anetlink` 用反射守住 |
| 证据面 | 每个节点有自己的仅追加哈希链;每次效果、每次拒绝在应答前入链 | 重启后链可加载并验证 |

---

## 3. 仓库拓扑

```
                    ┌──────────────┐
                    │   ANetCore   │  纯协议库 · 零 I/O · 黄金向量钉死
                    └──────┬───────┘
          ┌────────────────┼────────────────┐
          ▼                ▼                ▼
     ┌─────────┐     ┌──────────┐     ┌──────────┐
     │ ANetHub │◄───►│   ANet   │◄───►│ ANetLink │
     │ 目录/中继│ C2  │  daemon  │ C1  │  设备    │
     └─────────┘     └──────────┘     └────┬─────┘
                                            │ 真实协议
                                       ┌────▼─────┐
                                       │ ANetMock │  忠实假件(被控硬件)
                                       └──────────┘
```

| 仓 | 角色 | 版本 | 测试 | 依赖 |
|---|---|---|---|---|
| ANetCore | 协议内核:CoreDet-CBOR、CID、AObj 信封、KEL/AID、TSIR、AgentCard、效果、证据、委派线、中继鉴权、账本链、黄金向量 | v0.13.1 | 136 | 无 |
| ANet | daemon:唯一有生命周期的进程,内核 + 十个可插拔模块 | 0.1.7 | 224 | ANetCore |
| ANetHub | 目录、中继、评价验证、结算、联邦、任务板、运营面 | 0.1.6 | 139 | ANetCore |
| ANetLink | 物理世界运行时 + 14 个协议适配器 + AdapterSDK,北向 MCP 与 C1 | — | 324 | ANetCore |
| ANetMock | 用真实线协(ONVIF SOAP、ISAPI、大华 CGI、Modbus TCP、MQTT、z2m)模拟 148 台设备 | — | 43 | 无,刻意不说 ANetCore 类型 |

依赖方向唯一合法形态:三个应用 → ANetCore,永不反向,应用之间永不 import。三仓钉在同一个 ANetCore 版本。这与 2026-08-16 的设计一致,没有偏离。

---

## 4. 五条契约:设计与实现

| # | 契约 | 设计(2026-08-16) | 实现 | 偏离 |
|---|---|---|---|---|
| C1 | CapabilityProvider | Go 接口(进程内)+ UDS 远程实现,`Describe` 返回 CID | `provider.CapabilityProvider`:`ID / Capabilities / Describe / Invoke / Health`;进程内为主,daemon ↔ anetlinkd 走 UDS/HTTP | `Describe` 返回字符串而非 CID —— CAS 目录尚未成为描述的载体。`Call` 增加 `CallerAID`(验签后的调用方身份),是设计时没有的,shell 模块的授权建立在它上面 |
| C2 | Hub Wire API | HTTP + AObj 签名,版本化 | 41 个公开端点(见 §6),`internal/hubapi` 契约测试钉住跨仓字段名,wire contract 版本头 | 面比设计宽得多:x402 结算与发放链、p2p 会合、注销、可见性、准入,均为设计时不存在的需求 |
| C3 | Federation | delivery 与 discovery 两个独立子面 | `/fed/v1/forward`(投递)、`/fed/v1/cards`(目录游标同步)、`/fed/v1/reviews`(评价证据同步);三档可见性;一跳纪律;拉取而非推送 | 三个子面都在,但只有**一个** tag `no_federation`;设计中"可通信、不可见"的拆分靠可见性档位实现,不靠构建 |
| C4 | Adapter 接口 | Go 接口 AdapterSDK 编译期组合;ADAP/UDS 降格为逃生舱 | AdapterSDK + 14 个编译期适配器;ADAP 进程外适配器随 `-tags adap` 编入,`cmd/adapdemo` 是照线协手写的第三方示例 | 与设计一致。ADAP 首次真跑查出三个运行时缺陷(SUITE-TODO L-5),说明"逃生舱"在有人走之前并不存在 |
| C5 | 证据面 | ANetCore 类型,design3 原样 | `effect` 双轴信任 + Quirk;`ael` 防分叉链;`evidence` 收据与评价互锁,10 项检查可第三方复核;hub 自身的发放链与见证 | 超出设计:hub 的账本也上了链,并接受第三方见证。这是 D-5/H-17/H-18 的结果 |

---

## 5. anet daemon

### 5.1 内核

内核是"拔掉就不是 anet daemon"的部分,没有构建标签:

身份与多身份 · 能力注册表(C1)· 委派生命周期(find → delegate → 多轮消息 → end → review)· 委派验签 · 收据验证 · 第三方验证 `anet verify` · 证据链 · 中继客户端 · 传输列表(hub 是兜底不是唯一)· 对端 KEL 缓存 · 回环控制面(29 端点)· CLI · 自动回复(exec 与 OpenAI 兼容后端)· 本地控制台。

### 5.2 模块:设计清单与实现清单

2026-08-16 的模块清单与今天的实现,逐项对照:

| 设计中的模块 | 设计 tag | 今天 | 说明 |
|---|---|---|---|
| net.hub | `anet_hub` | 内核 | 中继客户端没有独立成模块。没有 hub 就没有发现,不存在"不带 hub 客户端的 daemon" |
| zooid 原生 agent | `anet_zooid` | **未做** | daemon 不跑模型是定位。"原生 agent"改由自动回复 harness 承担:把外部 CLI agent 或 OpenAI 兼容端点变成常驻服务 |
| connector | `anet_connector` | 内核(自动回复) | 同上 |
| anetlink | `anet_link` | `module/anetlink`,`no_anetlink` | 一致。daemon 经 UDS 连 anetlinkd,把设备能力 id 折进注册 |
| delegate | `anet_delegate` | **内核** | 有意偏离。委派生命周期是 C2 的一半,拔掉后节点既不能求助也不能被求助。设计中的开放决策 D45("lite 是否含 delegate")由此关闭:最小发行版也能委派 |
| surface.mcp | `anet_mcp` | `internal/mcpserv`,`no_mcp` | 一致。9 个工具 |

设计里没有、今天存在的模块,以及它们进来的理由:

| 模块 | tag | 来源 | 为什么是模块而不是内核 |
|---|---|---|---|
| service | `no_service` | 新 | 把本机 HTTP 服务以能力 id 挂上网络。信任等级由被调方实测给出,不由运营者声明 |
| x402 | `no_x402` | D-17 | 付费曾是 838 行无 tag 的内核代码,外加一个公开监听口。拆出后内核只留"驱动委派",模块拿到具名窄口 `PaymentSeam`。无付费构建对标价能力答 `UNAVAILABLE`,不会免费干 |
| p2p | `no_p2p` | anet3 | 直连投递;hub 仍用于发现与密钥历史。会合点走 hub 的签名地址目录 |
| cas / blackboard / org | `no_cas` 等 | anet3 迁移 | 内容寻址存储、共脑、组织凭证。`internal/golden` 对 anet3 钉死四个规范 id |
| taskboard | `no_taskboard` | H-6b | hub 任务板的客户端:读板、建卡、领取。`module.Host` 为它新增 `HubSeam`,比 `PaymentSeam` 小 |
| **shell** | **`shell`(加法)** | 2026-09-03 | 在宿主机执行运营者批准的命令。见 §5.5 |
| inv1 / inv2 | — | anet3 | 不变式守卫:组织范围对象不得进入第三方可读路径;模块声明的机密 token 不得出现在任何发布 |

**tag 方向的偏离。** 设计写的是加法 tag(`anet_<name>`,加了才有)。实现选了减法(`no_<name>`,默认在,去掉才没有),唯一例外是 `shell`。理由:两种写错的代价不对称。减法 tag 漏了,是某人想去掉的模块没去掉;加法 tag 漏了,是某种能力进了所有没要过它的构建。对绝大多数模块,前者代价小;对"在宿主机执行命令"这一个,后者代价大到必须反过来。所以规则是:**默认构建应当是完整的,除非某个能力进错构建的后果比缺失更重。**

### 5.3 发行版

设计的四档 preset 与实现的六档加一个正交开关:

| 设计 preset | 设计组合 | 今天对应 | 差异 |
|---|---|---|---|
| anet-lite | 内核 + hub + MCP | `anet-min`(去掉全部九个减法模块) | min **能委派**(见 delegate 留内核);MCP 在 `anet-agent` 档 |
| anet-standard | lite + delegate + zooid | `anet-standard`(保留 service) | zooid 未做;standard 的含义变成"能被调用" |
| anet-edge | lite + delegate + anetlink | 无独立命名 | 设备能力包含在 `anet-full`;边缘专用档尚未单独发布 |
| anet-full | 全部 | `anet-full` | 一致 |
| — | — | `anet-paid`(service + x402) | 新增。第一个需要认真读文档的档:可选公开端口、余额托管在 hub |
| — | — | `anet-agent`(service + mcp) | 新增。唯一体积理由成立的模块(+1.9 MB,+34 依赖) |
| — | — | `anet-p2p`(service + p2p) | 新增。接受入站对等连接 |
| — | — | `+shell` 孪生版 | 新增。任意一档加 `-tags shell`,同一 commit 一个能执行命令一个连代码都没有 |

实测(2026-09-03,`-trimpath -ldflags -w`):

| 形态 | 体积 | 依赖包 | 公开端口 |
|---|---|---|---|
| min | 12,132 K | 265 | 无 |
| standard | 12,144 K | 266 | 无 |
| paid | 12,264 K | 267 | 可选(`voucher_addr`) |
| agent | 14,044 K | 300 | 无(MCP 走 stdio) |
| p2p | 12,172 K | 267 | **有** |
| full | 14,320 K | 307 | 可选 + 有 |
| 任意档 +shell | +28 K | +1 | 不变 |

按体积分档站不住(除 MCP 外没有模块有实质体积影响),真正的分档维度是**攻击面**、**可被要求做什么**、**运维复杂度**。详见 [DISTRIBUTIONS-zh.md](DISTRIBUTIONS-zh.md)。

分发口径两条,内容相同:GitHub release(`ANetResearch/ANet/releases`)与 `agentnetwork.org.cn/dl`(`curl | sh` 一行安装)。release 二进制保留符号表(`-w` 不用 `-s -w`),多 1.4 MB,换来下游能在下载到的文件上自己核对模块是否存在。

### 5.4 可插拔的判据

不是"配置关掉",是**符号数**:

```
go build -o full ./cmd/anet;   go tool nm full | grep -c module/<m>     # > 0
go build -tags no_<m> -o lean ./cmd/anet;   go tool nm lean | grep -c module/<m>   # == 0
```

两个方向都验。只验 lean==0 会让一个从未被链接的模块看起来"可插拔"。CI 的 `pluggable` job 对九个减法 tag 逐个及组合验;`optin` job 对加法 tag 反向验(默认 == 0,带 tag > 0),并单独跑 `go test -tags shell ./...`——加法 tag 下的代码 `go test ./...` 根本看不见。开发者本地 `./build.sh --check` 做同样的事。

### 5.5 shell 模块:三道独立的闸门

这是套件里唯一在宿主机上执行命令的模块,所以它的设计目标是"难以无意间打开":

| 闸门 | 默认 | 谁决定 |
|---|---|---|
| 编译期 | 不在二进制里(加法 tag) | 发布方 |
| `modules.shell` 配置块 | 缺席则不注册任何能力 | 机器运营者 |
| 调用方名单 `allow_file` | 空则拒绝所有远程调用 | 机器运营者,每次调用重读,不重启 |

不提权:命令以 daemon 自己的用户身份运行;要 root 就让 daemon 以 root 运行,那是 unit 文件里看得见的安装期决定。调用方参数逐个引号包裹,命令行归运营者、参数归网络。超时杀整个进程组。回显有上限且截断时写明。退出码非零报 `FAILED` 带 stderr,不报 OK。每次执行与每次拒绝在应答前入链。不带调用方身份的调用同样默认拒绝(`allow_local`),因为 `CallerAID` 的零值是空字符串,把空当作"本机、可信"会让将来任何一处漏填变成静默绕过。

授权压在 `CallerAID` 上,而它是否真是验签后的身份只有真委派能证明,所以 `scripts/joint-shell.sh` 起一个 hub 加两个 daemon 验 18 项。

---

## 6. ANetHub

### 6.1 内核

registry(注册表)· relay(存储转发信箱,KEL 签名鉴权)· reviews(收据与评价的互锁验证——全网唯一在验的地方,这是"hub 伪造不了一条评分"的支点)· hub 自己的 AID/KEL(公开,`GET /hub/identity`)· guest 模式 · 静默两档(一小时未取信标记,一个月退出可浏览列表,什么都不删)· 内嵌公开 SPA。

### 6.2 模块:设计与实现

| 设计模块 | 设计 tag | 今天 | 说明 |
|---|---|---|---|
| taskboard | `hub_taskboard` | `no_taskboard` | 一致。卡片持 TaskDoc CID,七列 FSM,九个变更端点全部要 KEL 签名 |
| federation.delivery | `hub_fed_delivery` | `no_federation` | 合并成一个 tag。三个子面(投递、目录、评价证据)都在 |
| federation.discovery | `hub_fed_discovery` | 同上 | "可通信、不可见"靠三档可见性(默认 hub-local)实现 |
| reviews | `hub_reviews` | **内核** | 有意偏离。评价验证是 hub 存在的支点,一个不验评价的 hub 是普通消息队列 |
| console | `hub_console` | **内核** | 有意偏离。webui 打包进二进制,`build.sh` 在容器里重建并断言一致 |

设计里没有、今天存在的:

| 部分 | 来源 | 说明 |
|---|---|---|
| x402 facilitator + credit 账本 | H-3 / D-5 | hub 只卖门票不代理内容:`GET /x402/resource/{aid}/{cap}` 未付款回 402,付款后给凭证,hub 全程见不到请求与结果。价钱读自 agent 自己签的卡片,hub 能拒卖不能改价 |
| 发放链与见证 | H-17 / H-18 | 每笔供应变化进一条仅追加、哈希链接、hub 签名的链;第三方见证链头,之后同位置出现不同记录即构成改写证据 |
| 跨 hub 清算 | H-9 / H-21 | `hub_owed` 能升也能降;`anet-hub -clear` 签字并投递 |
| 运营面 `anet-hub-admin` | 新 | 与公网 hub 进程隔离的第二个二进制(:8078),25 个鉴权路由;归档删除可恢复 |
| **准入** | **2026-09-03** | 见 §6.4 |

### 6.3 发行形态

| 设计 preset | 今天 |
|---|---|
| hub-private(registry + relay + console + taskboard,联邦全关) | `-tags no_federation` |
| hub-open(private + 联邦) | 默认构建 |

### 6.4 准入

hub 的 `/register` 校验两件事:密钥历史能推出所声称的 AID;调用方能签出挑战。这证明调用方**控制**这个身份,不证明 hub **想要**它。2026-09-03 之前没有更多可说:任何人都能注册。

现在有一道可选的闸门:

- **默认仍是开放注册。** 升级到这个版本的 hub 行为不变;从未配置的 hub 与以前一样。
- 打开后(`anet-hub -invite-required true`),**只有 hub 不认识的 AID** 的首次注册需要邀请码。已注册的节点在重启和能力变化时照常重注册,不需要码——否则运营者打开开关的那一刻就把自己的网络锁死了。
- 邀请码有次数上限与有效期,可撤销;hub 只存其 SHA-256,明文只在铸造时打印一次。谁凭哪个码进来的有记录。
- 邀请码在签名校验**之后**、写入**之前**消耗:签不出挑战的人烧不掉码;写入失败会烧掉一次而不是留下一个无人可账的注册。
- 撤销关的是门,不逐出人。要移除一个已注册的节点是运营面的另一个动作。

daemon 侧 `anet hub-register <url> --token <码>`,`install.sh --token`。码不落盘。

### 6.5 hub 能做什么、不能做什么

| hub 能 | hub 不能 |
|---|---|
| 决定谁能注册(准入)、谁在目录里可见 | 伪造任何一条评价、收据、任务合同 |
| 转发它读不懂的字节 | 读懂经它中继的签名对象的信任关键内容 |
| 记账、签发凭证、签结算收据 | 事后改写自己的发放链而不被已取过链头的读者发现 |
| 拒绝出售某项能力 | 改价、把买家指到别的机器 |
| 标记一个月未露面的节点 | 删除任何人的证据 |

---

## 7. ANetLink

设计的适配器清单(modbus、mqtt、ha、zigbee、ble、btmesh、matter、thread)全部存在,并额外有 onvif、hikvision、dahua、opcua、bacnet、can、sim,共 14 个。tag 方向:多数减法(`no_<name>`),onvif / hikvision / dahua / adap 为加法。北向两个面:MCP(单机即可用)与 C1(供 daemon)。ADAP 进程外逃生舱随 `-tags adap`。

双后端制(探测宿主设施优先,自研兜底强制)与"零 OS 假定"约束不变。Matter 纯 Go 栈是独立树,是 GA 最长杆;真实 mDNS 上的死循环(L-7)已修。已知缺口:真机 PTZ/事件/抓拍未测(L-1)、厂商云适配器为零(L-2)、自动发现只有 ONVIF(L-3)。

---

## 8. 里程碑对照

| 设计里程碑 | 内容 | 状态 |
|---|---|---|
| M0 地基 | ANetCore 抽仓 + 黄金向量;C1/C2 定稿;与现网字节级互操作 | 完成。v0.13.1,7 条 wire 向量,冻结的一致性身份 |
| M1 三件套 α | daemon 重组;hub 四件;anetlinkd + 适配器 + MCP;单机闭环 | 完成。`scripts/joint.sh` 四仓六进程 20/20 |
| M2 集连 β | hub AID 转正;联邦投递 + 目录;自研兜底第一批;拔插头 CI | 完成。双 hub 实网(emax + fmax)`prodtest.sh` 45 项;CI 矩阵两个方向 |
| M3 兜底收口 | 蓝牙 Mesh、Matter 纯 Go、Thread OTBR 全部真实可用 | **进行中**。Matter 是最长杆 |
| M4 发行 v4.0.0 | preset 矩阵产物 + README + 安全清扫 + tag | **部分**:2026-09-03 发布第一个 GitHub release(v0.1.7,四平台两变体);一行安装可入网;版本号仍是 0.1.x 系 |

M2 之后实现超出了设计:付费(D-5/D-17)、见证(H-18)、静默(H-15)、注销(D-19)、准入,都是实网跑出来的需求,设计时没有。

---

## 9. 验证体系

单元测试对着 fake 跑不够。每一层各有覆盖:

| 层 | 工具 | 覆盖什么 |
|---|---|---|
| 单元 + mutation | `go test`,改坏实现确认断言会红 | 每条新断言 |
| 跨进程 | `scripts/joint.sh`(四仓六进程)、`joint-shell.sh`(hub + 两 daemon)、`joint-invite.sh`(准入) | 两边各自伪造对方时全绿的那类缺陷 |
| 单机多节点 | `scripts/scenario.sh`(一个谁也不认识的 hub + 三个节点;`--live` 接真模型) | 新用户路径、付费闭环 |
| 实网 | `scripts/prodtest.sh`(emax + fmax 两 hub,cmax / ink93 / dmax 三 daemon,每小时) | 只有双 hub 才能造出的缺陷 |
| 真实服务 | `scripts/realworld.sh`(docker 起 mosquitto / Modbus / HA / OPC UA / Matter) | 七个适配器对着真协议 |
| 拔插头 | CI `pluggable` + `optin` | 符号数两个方向 |

[SUITE-TODO-zh.md §八](SUITE-TODO-zh.md) 记录了十一个只有联调或双 hub 实网才能发现的缺陷。结论不变:`joint.sh` 与 `prodtest.sh` 是仓库的一部分,不是脚手架。

---

## 10. 已知缺口与开放决策

按影响排序。每条给位置、行为、影响、发现方式。

| # | 位置 | 行为 | 影响 | 发现方式 | 状态 |
|---|---|---|---|---|---|
| 1 | ANetHub `AdmitCard` 高水位检查 | 同一节点短时间内第二次注册时 card.seq 未超过高水位,被当作回滚拒绝(`STALE_SEQ`) | 节点快速重复注册失败;与准入无关,准入关着也发生 | `joint-invite.sh` 第 4 步 | 待修 |
| 2 | ANet `tryCapabilityPaid` | 能力解析不出时返回 false,委派落到 auto-reply;未配 auto-reply 的节点永不作答 | 请求方拿到超时,与"节点宕了"无法区分,而不是 `UNAVAILABLE` | `joint-shell.sh` 第 7 步 | 待定:改它影响所有模块的应答语义 |
| 3 | 准入的运营面 | 只有 hub CLI(`-invite-*`),admin 面与 webui 无对应页 | 运营者需登录机器 | 本轮 | 待做 |
| 4 | `anet-edge` 档 | 设计有,未单独发布;设备能力在 full 里 | 边缘盒子拿到的是 full | §5.3 | 待定 |
| 5 | p2p 强制模式 | 现为"能直连就直连,不能就走 hub"的自动回退,无开关 | 无法表达"注册只为发现、投递全走 p2p" | DISTRIBUTIONS | 待定 |
| 6 | L-1 / L-2 / L-3 | 真机未测;厂商云为零;自动发现只有 ONVIF | 设备生态覆盖 | SUITE-TODO | 待做 |
| 7 | H-22 | 静默两档的时间跨越只在单测里 | 有意取舍 | SUITE-TODO | 不做 |
| 8 | D-6 / C-1 | 治理纪元;org 只接受 epoch 0 | 低 | SUITE-TODO | 等第二个消费者 |
| 9 | D43 / D44 | ADAP 逃生舱是否随 v4.0;taskboard 权限模型 | 设计期开放决策 | CONTRACTS §8 | 待拍板 |

### 本轮修掉的文档漂移

写这份文档时对照代码查出并修正的:

- `ANet/build.sh` 与 `deploy/release/build-release.sh` 仍按"需要 CGO 与 sqlite_fts5"写,而驱动早已是纯 Go 的 modernc.org/sqlite,该 tag 在源码中零处引用;release 路径还只能在 macOS 跑,这是 `/dl` 停在 7 月 12 日的直接原因
- `ANetHub/README.md` 的构建命令同样写着 `CGO_ENABLED=1 -tags sqlite_fts5`,其 CI 实际是 `CGO_ENABLED=0`
- `DISTRIBUTIONS-zh.md` 的体积表出自更早的树(完整构建写 20,849 K,实测 14,320 K)
- `CONTRACTS-zh.md` 的模块清单与 tag 命名(`anet_<name>`)与实现不符;该文按约定保留原文,以本文为对照

---

## 11. 一句话总结

期待的形态是"一套代码,N 种发行版,每种都能说清自己不能做什么"。今天的实现做到了这一句:六档加一个开关,符号数两个方向可验,第一个 release 已发,一行安装可入网可入准入。偏离设计的地方有理由且已写明;缺口按影响排了序。
