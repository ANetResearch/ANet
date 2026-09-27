# anet4 五合同与模块架构

> 原为 anet3 单体仓的 `docs/K207-anet4-module-architecture.md`。搬到这里,是因为
> 它是 anet4 五个仓的规范架构文档,而 Go 源码里(`provider/provider.go`、
> `internal/daemon/capability.go`、`daemon.go`)直接引用它 —— 让一个正在退役的
> 仓库定义在建系统的合同,是把地基放在别人家。
>
> **原文保留,包括写于 2026-08-16 的决策记录和里程碑。** 那是当时的判断,不是
> 今天的状态;今天的状态在 [SUITE-TODO-zh.md](SUITE-TODO-zh.md) 和
> [CAPABILITIES-zh.md](CAPABILITIES-zh.md)。两者有出入时,以那两份为准 ——
> 这份是"我们打算怎么切",不是"我们切到哪了"。

> 2026-08-16，与 ink 两轮逐项拍板后成文（第一轮 A1–A4，第二轮 D41/D42/D46 + A3′ 修正）。前置阅读：K206（思维长卷）、design3/README、
> ANetLink/DocsANetLink/00-README。本文是 anet4 的模块级总纲；协议不动（A2），
> 代码未动——这是动手前的合同。

## 0. 立论

三代教训的综合：v1 死于耦合（org 长进 35 个 daemon 文件），1.3.0-v3 死于没有合同
（开源只能砍 683,852 行），oss-v0.1 证明减法产品成立但减法用的是 fork（内核逐字
拷贝、两月即漂移）。故 anet4 的唯一命题：

> **让"裁剪"从 git 操作变成构建配置。一套代码，N 种发行版。**

anet4 不是 design4：协议正典（design3）保持规范基线，anet4 是实现的第四代组织方式。

## 1. 决策记录（2026-08-16 四项拍板）

| # | 议题 | 裁决 |
|---|---|---|
| **A1** | 协议内核归属 | **独立仓 `ANetResearch/ANetCore`**。纯协议库 + 黄金向量，独立 semver。D-16 阻塞就此终解。 |
| **A2** | 与 design3 关系 | **design3 不动，只加扩展轨 spec**：`federation`（C3，K208 起草）与 `deviceprofile`。黄金向量全部延续。 |
| **A3** | 可插拔机制 | **全部编译期组合（build tag + Go 接口 + 模块注册表）**。**严格纯 Go**：不引入任何跨语言运行时组件（matter.js、zigbee-herdsman、python 栈一律出局）。缺某生态适配 = 按协议标准自己用 Go 写，绝不盲目引入外语包。 |
| **A4** | v3 大件处置 | **kanban 随首发**（落 hub 侧 taskboard 模块）；org / ASCP 高档耦合 / p2p / 分布式存储全部后置，按测试成熟度排队（brain 43% 优先）作为后续模块经合同回归。 |
| **A5** | 覆盖深度与后端策略（第三轮，2026-08-16） | **Zigbee/Matter/Thread/蓝牙Mesh/HA 等全部进 v4.0 第一版**，不留"长线"缓冲。落地采用**双后端制**：每个协议适配器 = 前端（协议逻辑/Profile 映射）+ 多后端；**探测优先**（宿主 BlueZ/已装 OTBR/已跑 z2m/HA 存在且可调用则直接用），**自研兜底强制**（宿主什么都没有时靠自己）。ink 原话："如果 BlueZ 存在，能调用，那么就调用。但如果没有的话，就靠自己。永远都要兜底。" |
| **A3′** | A3 第二轮修正（随 D41 裁决） | 绝对纯 Go 放宽为三原则：① **零 OS 假定**——不得依赖宿主机预装 BlueZ/OTBR 等服务（但存在时应探测并利用，见 A5）；② **Go 优先，尽量少跨语言**——确需跨语言的组件必须自带打包、作为受管子进程运行；③ **对外必须是一个通用服务接口**——内部栈的复杂性绝不外泄，用户和 daemon 只见统一能力面。 |

A3 的直接后果：① DocsANetLink `08-协议栈选型.md` **整篇作废**（结论建立在边车方案上）；
② ADAP 降格（见 C4）；③ Matter 纯 Go 栈成为独立长线轨道（§6）。

## 2. 仓库拓扑（四仓）

```
ANetResearch/ANetCore   纯协议库（无 daemon 依赖，无 main）
ANetResearch/ANet       daemon（已发 v0.1.5，在其上重组）
ANetResearch/ANetHub    hub（registry/relay/federation/taskboard/console）
ANetResearch/ANetLink   物理世界 runtime + 适配器 + AdapterSDK
```

- ANetCore 内容：`coredet`(CBOR) · `cid` · `aobj`(信封/签名) · `identity`(KEL/AID) ·
  `tsir`(谓词/EffectRecord) · `adp`(AgentCard) · `agenturi` · conformance/黄金向量。
  来源 = 1.3.0-v3 `internal/v3/{coredet,aobj,identity,tsir,adp,agenturi}` 原样上提。
- 依赖方向唯一合法形态：三应用 → ANetCore。应用之间**永不互相 import**；
  daemon 全量版可 import ANetLink 的 `pkg/runtime`（见 C1，编译期内嵌形态）。
- 三仓自洽验收：ANet=`anet init` 单机有身份、Claude 经 MCP 可用；ANetHub=`docker run`
  即得组织内网注册+中继+控制台+任务板；ANetLink=anetlinkd+一个 adapter 即可让
  Claude 控制真实设备（不需要 daemon 和 hub）。三个独立冷启动入口。

## 3. 五个合同（可插拔的脊柱）

模块间只许通过合同说话；模块只 import ANetCore 与合同包，兄弟模块互不可见。

| # | 合同 | 连接 | 形态（A3 修订后） |
|---|---|---|---|
| C1 | **CapabilityProvider** | daemon ↔ 一切能力来源 | Go 接口（进程内，主形态）+ UDS 远程实现（仅用于 daemon↔独立部署的 anetlinkd 这一应用间边界） |
| C2 | **Hub Wire API** | daemon ↔ hub | HTTP + AObj 签名，版本化；v4.0 扩面：taskboard 端点（A4） |
| C3 | **Federation API** | hub ↔ hub | HTTP + AObj 签名；delivery 与 discovery 两个独立子面；spec = K208 |
| C4 | **Adapter 接口** | anetlink runtime ↔ 协议适配器 | **Go 接口（AdapterSDK），编译期组合**。原 ADAP/UDS 降格为第三方 out-of-tree 逃生舱（extension，随 v4.0 与否见 D43） |
| C5 | **证据面** | 所有人 → EffectRecord/AEL | ANetCore 类型，design3 原样 |

C1 红线（org 教训成文）：**daemon 不得知道"设备"概念**——只知道 provider 声明了
能力、可被调用、返回证据。接口签名（草案，M0 定稿）：

```go
type CapabilityProvider interface {
    ID() string
    Capabilities(ctx context.Context) ([]string, error)      // ADP 卡聚合
    Describe(ctx context.Context) (cid.CID, error)           // 指向 CAS 目录
    Invoke(ctx context.Context, call CapabilityCall) (Effect, error)
    Health(ctx context.Context) error
}
```

## 4. 模块清单与 build tag

### ANet daemon

| 模块 | tag | 可拔 | 来源 |
|---|---|---|---|
| identity / keystore | — | ❌ 内核 | v3 identity 原样（经 ANetCore） |
| ledger（AEL 本地防分叉链） | — | ❌ 内核 | v3 ael 815 行 |
| net.hub / net.loopback | `anet_hub` / 内置 | 🔄 可换 | oss daemon |
| providers 注册表 | — | 框架内置 | 新（薄） |
| — zooid 原生 agent | `anet_zooid` | ✅ | 1.3.0-v3 agent 包 |
| — connector（外部 agent） | `anet_connector` | ✅ | agentbackend |
| — anetlink（内嵌 runtime 或 UDS shim） | `anet_link` | ✅ | ANetLink pkg/runtime |
| delegate（收委派→验谓词→执行→证据） | `anet_delegate` | ✅ | oss delegation/evidence/relayauth 508 行 |
| surface.mcp（暴露给 Claude/IDE） | `anet_mcp` | ✅ | 新——冷启动钩子 |
| surface.cli / surface.api | — | 内置 | 现有 |

### ANetHub

| 模块 | tag | 可拔 | 来源 |
|---|---|---|---|
| registry + relay | — | ❌ 内核 | oss aghub |
| hub 自有 AID/KEL | — | ❌ 内核 | 新（guest broker 先例转正；联邦前提） |
| taskboard（任务板，A4） | `hub_taskboard` | ✅ | 1.3.0-v3 kanban（9,770 行 / 50% 测试）移植：卡片持 TaskDoc CID（D3 遗训：卡片是视图不是真相），7 列 FSM 保留，存储从 daemon 本地改 hub SQLite |
| federation.delivery | `hub_fed_delivery` | ✅ | 新 |
| federation.discovery | `hub_fed_discovery` | ✅ 独立于 delivery | 新——封闭组织"可通信、不可见"靠此拆分 |
| reviews | `hub_reviews` | ✅ | aghub |
| console（Go 二进制内嵌静态前端） | `hub_console` | ✅ | 重做（清除 anetpw2077 一类 P0） |
| store 后端 | 接口 | 🔄 sqlite 默认 | aghub |

（console 的浏览器前端是 HTML/JS——A3 纯 Go 约束的对象是**分发的运行时组件**，
不含浏览器侧静态资源。）

### ANetLink

| 模块 | tag | 说明 |
|---|---|---|
| runtime（DeviceProfile/CAS、T0–T4、Quirk、身份认领） | — | 内核；同时以 `pkg/runtime` 纯库形态供 daemon 内嵌 |
| AdapterSDK（C4 Go 接口 + EffectBuilder + conformance） | — | 内核 |
| adapter: modbus | `link_modbus` | 成熟纯 Go 库（D46） |
| adapter: mqtt-bridge | `link_mqtt` | 纯 Go 说 MQTT wire（对接既存 zigbee2mqtt/Tasmota 实例） |
| adapter: ha-bridge | `link_ha` | 纯 Go 说 Home Assistant WebSocket/REST——**杠杆最大的桥**：HA 两千余集成即刻成为可调能力 |
| adapter: zigbee | `link_zigbee` | 纯 Go 串口协调器栈（Z-Stack MT / deCONZ；shimmeringbee 类纯 Go 库可用，D46） |
| adapter: ble | `link_ble` | **纯 Go 直连内核 HCI socket，不假定 BlueZ 存在**（A3′①） |
| adapter: btmesh | `link_btmesh` | 蓝牙 Mesh，纯 Go（provisioning + model 层自写，量大，长线） |
| adapter: matter | `link_matter` | **纯 Go Matter 栈，长线轨道**（§6），不阻塞 v4.0 |
| adapter: thread | `link_thread` | **跨语言豁免 #1**：OTBR 自带打包为受管子进程（A3′②），随 Matter-over-Thread 需求排期 |
| 北向: mcp / daemon-shim | `link_mcp` / `link_daemon` | MCP 直通 = 单机价值；shim = C1 远程版 |

## 5. 发行版 preset（构建矩阵）

| preset | 组合 | 面向 |
|---|---|---|
| anet-lite | 内核 + net.hub + surface.mcp | "给 Claude 一个网络身份"，最小可爱形态 |
| anet-standard（默认） | lite + delegate + zooid | 正常参与网络协作 |
| anet-edge | lite + delegate + anet_link（内嵌 runtime + 若干 adapter） | 边缘/IoT 盒子 |
| anet-full | 全部 tag | 实验室 |
| hub-private | registry+relay+console+taskboard | 组织内网，联邦全关 |
| hub-open | private + fed_delivery（+可选 fed_discovery） | 集连节点 |

**拔插头 CI**：构建矩阵逐 tag 禁用，编译必须通过、启动必须成功、`GET /healthz`
必须 200。可插拔每次提交被验证，不是口头承诺。

## 6. 适配器战略（A3′ / D41 / D42 展开）

五条原则：

1. **双后端制（A5）**：每个适配器 = 前端 + 多后端。后端按优先级探测：宿主设施
   （BlueZ、已装 OTBR、已跑 zigbee2mqtt/HA）存在且健康 → 用之；否则落到自研
   兜底路径。探测框架直接借 1.3.0-v3 `agentbackend.Probe` 先例；配置可强制指定后端。
2. **兜底强制**：每个协议必须有零外部依赖的自带路径——"永远都要兜底"。
   兜底缺席 = 该适配器不算完成。
3. **Go 优先，豁免受管**：跨语言豁免逐个登记（目前仅 Thread/OTBR 兜底一项），
   豁免件必须自带打包、以受管子进程运行、崩溃可重启、对外不可见。
4. **统一服务接口**：无论后端是宿主设施、纯 Go 栈还是子进程，北向只有一个能力面
   （MCP / C1 / REST）。用户见设备与能力，永远不见适配器与后端。
5. **广度是产品需求（D42/A5）**：常见智能家居协议**全部随 v4.0 第一版发布**；
   桥类后端（mqtt/ha）是合法且优先的覆盖手段，wire 协议互通不算引入组件。

**覆盖矩阵（全部 v4.0，双后端逐行列明）：**

| 协议/生态 | 探测后端（有则用） | 自研兜底（永远存在） | 兜底难度 |
|---|---|---|---|
| Modbus | —（直连） | 成熟纯 Go 库 | 低 |
| MQTT 生态（z2m/Tasmota） | 探测 broker/topic 结构 | 本身即桥；设备侧兜底由 Zigbee 原生行承担 | 低 |
| Home Assistant | HA WebSocket/REST（桥即探测型后端） | 同上——HA 缺席时由各原生行兜底 | 低 |
| Zigbee | 已跑 zigbee2mqtt → MQTT 桥 | 纯 Go 串口协调器栈（Z-Stack MT/deCONZ） | 中 |
| BLE | BlueZ 存在 → D-Bus 后端 | 纯 Go 直连内核 HCI socket | 中 |
| 蓝牙 Mesh | BlueZ bluetooth-meshd 存在 → 用 | 纯 Go provisioning + model 层 | 高 |
| Matter (IP) | 宿主已有 matter 控制器/HA → 借道 | 纯 Go 栈（SPAKE2+/CASE 自写 + 集群 codegen） | **最高（GA 最长杆）** |
| Thread | 宿主已装 OTBR → REST 调用 | 豁免 #1：自带打包 OTBR 受管子进程 | 中（打包工程） |

**生态知识转译**：把 matter.js 的 model JSON、CSA 规范 XML、
zigbee-herdsman-converters 设备库（382 厂商/5,129 型号）、ZCL 规范当作
**构建期数据源**，codegen 生成 Go 集群库与 DeviceProfile/Quirk 库。知识不丢，
依赖不进运行时。（原 PoC-2 "matter.js→DeviceProfile 生成器" 改造为
"model 数据→Go codegen"。）

**复杂度归置（ink 判断记录）**：anetlink 会比预想复杂得多——接受。此复杂度
全部收进 ANetLink 一仓消化，不外溢：daemon/hub 侧永远只见 C1 合同（红线不变）。

诚实成本账（Matter 纯 Go 栈，按模块）：

| 件 | 难度 | 备注 |
|---|---|---|
| TLV 编解码 / mDNS 发现 | 低 | Go 生态成熟 |
| 集群库 | 中（量大） | 全部 codegen，从 CSA XML 生成 |
| PASE（SPAKE2+）/ CASE 会话 | 高 | Go 无现成 SPAKE2+，需按 RFC 自实现并过向量 |
| Interaction Model | 中高 | 状态机为主 |
| BLE 调试入网 | 中 | 依 D41（BlueZ） |
| Thread 承载 | 极高 | 依 OTBR 边界裁决（D41）；首期只做 Matter-over-Wi-Fi/Ethernet |

排序（每步独立有用）：modbus → mqtt-bridge → zigbee → ble → matter(IP) → thread。
v4.0 发行含前两个，zigbee 进 M2。

## 7. 里程碑

- **M0 地基**：ANetCore 抽仓 + 黄金向量 CI；C1/C2 合同定稿；PoC-1（与 anet 现网
  字节级互操作：CID/签名/EffectRecord 三向量全绿）。
- **M1 三件套 α**：daemon 按 §4 重组；hub = registry/relay/console/taskboard；
  anetlinkd = runtime + 后端探测框架 + 全部适配器前端 + 低难度后端（modbus/mqtt/ha + BlueZ 探测路径）+ MCP 直通。单机闭环演示：
  Claude→MCP→anetlinkd 控真设备；daemon 经 hub 收委派→执行→交证据。
- **M2 集连 β**：hub AID 转正；federation.delivery + discovery（K208 spec 同步）；
  自研兜底第一批落地（zigbee 协调器栈、BLE HCI）；拔插头 CI 全绿。
- **M3 兜底收口**：蓝牙 Mesh、Matter 纯 Go 栈、Thread OTBR 打包——覆盖矩阵
  每行"自研兜底"列全部真实可用。**Matter 自研栈是 v4.0 GA 的最长杆**，
  探测后端已可先行服务用户，但 GA 门 = 兜底齐活（A5）。
- **M4 发行 v4.0.0**：preset 矩阵产物 + 三仓 README/quickstart + 安全清扫
  （anetpw2077 等）+ tag。

## 8. 开放决策登记

| # | 议题 | 建议默认 | 状态 |
|---|---|---|---|
| D41 | OS 服务边界 | — | ✅ 已裁决 = A3′ + A5 双后端制：零假定、探测优先、兜底强制。ink 原话（第二轮）："不能假定当前安装的 Linux 所在操作系统有这些可调用的接口……对外表现上一定是一个通用服务接口"；（第三轮）："如果 BlueZ 存在，能调用，那么就调用。但如果没有的话，就靠自己。永远都要兜底。" |
| D42 | 桥类适配器合法性 | — | ✅ 已裁决：广度是一等需求——"适配器一定要多。Zigbee、Matter、Thread、蓝牙Mesh、HA 等，要完全覆盖当前智能家居生态内会出现的常见协议。" 桥与原生栈并行推进 |
| D43 | ADAP/UDS 第三方逃生舱随 v4.0 发布还是推迟 | 推迟到 v4.1（先证内需再开外门） | 待 ink |
| D44 | taskboard 权限模型（建卡/认领资格） | registry 内注册身份可建可认领；guest 只读 | 待 ink |
| D45 | anet-lite 是否含 delegate | 不含（lite 只做身份+MCP；能干活是 standard 的事） | 待 ink |
| D46 | 纯 Go 第三方库 | — | ✅ 已裁决：成熟纯 Go 库可用（license 合规）；自写仅在无纯 Go 选项时触发（如 SPAKE2+） |

## 9. 对既有文档的影响

- DocsANetLink `08-协议栈选型.md`：**作废**，头部加弃用注记 → 结论并入本文 §6。
- DocsANetLink `04-ADAP适配器协议.md`：按 C4 降格重写（Go 接口为主，UDS 为扩展）。
- DocsANetLink `01/06/09`：既有勘误（evidence/delegation/relayauth 包基线错标）
  随本次重写一并修正。
- K208（下一篇）：hub federation 协议草案（C3），extension 轨。

—— 记录：Claude（Fable 5）· 拍板：ink · 2026-08-16
