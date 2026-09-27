# 10 · 官方公共 agent / 工具:现状与第一批设计

调查日期 2026-09-26。本篇只读代码与生产环境,不改任何仓库文件。

标注约定:
- 带 `文件:行` 出处的陈述是读代码核实过的。
- 带「实测」的陈述是本次对生产环境做只读查询的结果(HTTP GET、ssh 只读命令、`sqlite3 -readonly`),命令写在对应段落里。
- `[推断]` 是由已核实事实推出、未直接验证的结论。
- `[建议]` 是设计提案,不是现状。

相关篇目:`03-e2e-encryption.md`(信封设计)、`04-hub-content.md`(hub 内容暴露面)、`05-secure-defaults.md`(入站策略 `inbound` 块)、`06-x402.md`(a2a-x402 映射)。本篇的设计以这些篇目的结论为前提,不重复论证。

---

## 0. 结论摘要

1. **代码里没有"公共能力"这个概念。** 入站委派只看一个全局开关 `accept_delegations`(`ANet/internal/daemon/delegation.go:488-490`),默认开(`ANet/internal/daemon/config.go:94`、`:114-117`)。开着时,任何能通过签名校验的 AID 可以调用本节点注册的任何能力:能力分派路径 `tryCapabilityPaid`(`ANet/internal/daemon/capability.go:284-385`)不按调用方做任何判断,只把 `CallerAID` 透传给 provider(`:374`)。唯一自带调用方白名单的是 shell 模块(`ANet/module/shell/provider.go:85-110`)。
2. **没有任何按调用方的限流或配额。** daemon 侧唯一的并发上限是长任务 4 个(`capability.go:67`);自动回复只有"每个 interaction 最多 30 条"(`ANet/internal/daemon/autoreply.go:327-331`);hub 的 `/relay/send` 无认证、无限流(`ANetHub/internal/aghub/server.go:882-931`,另见 `04-hub-content.md` §3)。
3. **生产目录里现有的"可被任何人调用"的能力,多数不适合作为公共能力。** 实测 emax/fmax 目录中,`dmax-services` 对外列出 247 个能力,包括 `cas.put`、`blackboard.add`、`system.reboot@…/camera-*`、`switch.onoff@mqttbridge/lock-*`;`cmax-anet4` 列出 `cas.put`、`blackboard.add`。这些能力在当前代码下对任何 AID 开放(见 §3.3)。
4. **hub 运营面正在读取中继内容。** 实测 emax 上 `anet-hub-admin` 以默认参数运行,`--harvest-every` 默认 30 分钟(`ANetHub/cmd/anet-hub-admin/main.go:46`),harvester 解码 `relay_message` 的 DelegateReq/ChatMsg/ResultResp 写成数据集(`ANetHub/internal/admin/harvest.go:24-35`);`/data/projs/anet-hub/admin/datasets/hub-relay` 38 MB,最新文件日期 2026-09-26。这与"hub 只做传输"直接冲突,官方 agent 的流量上线后同样会被收录。
5. **现有公共入口"访客模式"由 hub 终结内容。** hub 以自己的 broker 身份替访客签委派,访客原文进入 TaskDoc(`ANetHub/internal/aghub/guest.go:1-16`、`:592-604`)。决策 (2)(3) 下它需要移除或改造(选项见 `04-hub-content.md` §4.4)。
6. **能力卡片不携带描述。** 签名卡片只有名称、能力 id 列表、兑付端点和价格(`ANet/internal/daemon/card.go:185-211`);service 模块的 `description` 字段被解析后没有任何引用(`ANet/module/service/service.go:91`,全仓 grep `.Description` 无结果)。A2A 的 `AgentSkill` 要求 `id/name/description/tags`(`Refs/a2a/specification/a2a.proto:436-452`),所以任何公共 agent 要被 A2A 客户端理解,先要补一条"技能描述进签名卡片"的通道。
7. **提议第一批 5 个身份、4 类能力**:连通性 `net.echo`(两个身份,分别归属 emax 与 fmax)、确定性文本/JSON/A2A 校验工具、文档检索 `docs.search`(无模型、无外网)、付费演示(a2a-x402 端到端)。全部为确定性纯计算,不执行命令、不访问外网、不写共享存储。部署在 dmax,以非 root 用户和 systemd 沙箱运行;防滥用分五层,内容只在 daemon 与后端之间出现,hub 侧只做与内容无关的流量计量(§5)。

---

## 1. 范围与方法

读过的代码:`ANet/module/{service,cas,blackboard,org,taskboard,x402,shell}`、`ANet/module/module.go`、`ANet/provider/*`、`ANet/internal/daemon/{config,delegation,capability,card,paylink,autoreply,autoreply_exec,relay}.go`、`ANet/scripts/{onboard,scenario,prodtest}.sh`、`ANet/tools/{anetfixture,anetpeer}`、`ANetHub/internal/aghub/{server,guest,facilitator,invite}.go`、`ANetHub/internal/admin/{harvest,store,manifest,server}.go`、`ANetLink/northbound/c1serv/c1serv.go`、`Refs/a2a/specification/a2a.proto`、`Refs/a2a-x402-spec-v0.2.md`。

读过的运维记录:memory `anet-prod-topology.md`、`anet-servers.md`。

本次对生产环境的只读查询:
- `curl https://hub.agentnetwork.org.cn/agents`、`curl http://39.107.76.243:4001/agents`、两个 `/agents/{aid}/card`、`/graph`、`/stats`。
- `ssh dmax`:读 `/data/anet-node/home/.anet/config.json` 中的非密钥字段、`systemctl list-units`、`ss -ltnp`、`/usr/local/bin/anet version`、`/data/anet-node/svc/serve.py` 的函数与路由行。
- `ssh root@8.149.141.115`(emax,直连):`anet-hub-admin.service` 的 ExecStart、datasets 目录大小与文件日期、`sqlite3 -readonly hub.db` 查询 `agent` 表。未读取任何 relay 内容或数据集文件内容。
- 从 ink93 与 emax 分别探测 `http://210.45.70.176:4002/x402/redeem`。

未连 cmax(其网段自 2026-09-11 起从 ink93 不可达,见 memory `anet-servers.md` 2026-09-12 条);cmax 的状态依据 hub 目录的 `last_seen` 判断。

---

## 2. 现状:代码层

### 2.1 能力如何上网:`service` 模块

- 一个能力是一个 id 加一个 URL;daemon 把调用参数 JSON POST 过去,读回 JSON(`ANet/module/service/service.go:11-15`、`:149-161`)。
- 配置项:`id`、`url`、`price`(credit,0 为免费,`:85-89`)、`description`(`:90-91`)、`protocol`(`:92-94`),以及整体 `timeout_ms`(默认 2 分钟,`:105-111`)。
- 回复上限 1 MiB(`:224`)。回复整体作为 `ObservedState` 返回(`:205`),可信度封顶 V1(`:206-209`、`:226-244`)。
- **调用方身份不传给后端。** 请求体只有 `call.Args`,头部只有 `Content-Type`(`:149-161`)。`provider.Call.CallerAID` 存在(`ANet/provider/provider.go:29-31`),但 service 模块不转发,所以后端无法按调用方记录或限流。
- **`description` 未被使用。** 全仓 grep `\.Description` 在 `module/service`、`internal/daemon`、`provider` 下无结果;`Describe()` 返回空串(`service.go:129`)。卡片不含它(§2.5)。

构建档位:`standard` 含 service,`paid` 另含 x402(`ANet/docs/DISTRIBUTIONS-zh.md:198-200`;`ANet/scripts/onboard.sh:37-41` 用同样的 tag 组合构建)。

### 2.2 入站委派的准入与限流

| 项 | 现状 | 出处 |
|---|---|---|
| 是否接受委派 | 全局布尔,未设即 true | `config.go:41-44`、`:94`、`:114-117` |
| 准入判定位置 | `ingestDelegate` 第一行,只看这个布尔 | `delegation.go:488-490` |
| 能力调用识别 | TaskDoc `Requires[].Type=="capability"` | `capability.go:90-114`、`delegation.go:561-563` |
| 按调用方授权 | 无;`CallerAID` 透传给 provider,由 provider 自行决定 | `capability.go:374` |
| 未注册的能力 | 有自动回复时转交自动回复,否则答 `UNAVAILABLE` | `capability.go:288-322` |
| 标价能力 | 无 payer 时 `UNAVAILABLE`;无付款时答 `PAYMENT_REQUIRED`;先结算后执行 | `capability.go:339-372` |
| 单次调用时限 | 默认 60 s;provider 可经 `LongRunning` 声明更长 | `capability.go:51`、`:69-81` |
| 并发 | 长任务最多 4 个,超出答 `UNAVAILABLE`;短任务在轮询循环内串行 | `capability.go:67`、`delegation.go:593-630` |
| 按调用方的速率/配额 | 无 | — |
| 访客消息配额 | 每个 agent 默认 5 条,发布到 hub | `config.go:45-48`、`:98-106` |

自动回复(`auto_reply`)对所有活动线程逐一服务,不区分对端(`autoreply.go:272-300`);唯一上限是每个 interaction 最多 30 条(`:53-56`、`:327-331`)。`exec` 后端的提示词允许本地编码 agent 执行 shell 命令(`autoreply_exec.go:16-17`)。详细分析见 `05-secure-defaults.md` §4。

### 2.3 各现有模块能否作为公共能力

| 模块 / 能力 | 对陌生调用方的行为 | 作为公共能力的判断 | 出处 |
|---|---|---|---|
| `service`(HTTP 能力) | 任何调用方可调;后端不知调用方 | **可以**,前提是后端是确定性、有界的纯计算,且 daemon 侧补上准入与限流 | `service.go:137-222` |
| `cas.put` | 任何调用方可写,单个 blob 默认上限 32 MiB,无总量或次数上限 | **不可以**:任何人可以填满磁盘 | `ANet/module/cas/module.go:73-76`、`:147-183` |
| `cas.get/has/stat` | 只读 | 可以,但对 coding agent 价值低 | `cas/module.go:39-42` |
| `blackboard.add` | 合并任何作者签名的贡献,内存中增长 | **不可以**:无上限的状态写入 | `ANet/module/blackboard/module.go:43-45`、`:133` 起 |
| `org.verify/info` | 回答成员资格 | 不适用:org id 按 INV-2 是保密值(`ForbiddenTokens`),公共节点不应承载组织 | `ANet/module/org/module.go:49-50`、`:111-122` |
| `taskboard` `task.create/claim` | 以**本节点**身份向 hub 签名写看板 | **不可以**:任何调用方可以让节点以自己的名义建卡、认领 | `ANet/module/taskboard/taskboard.go:46-50`、`:114-127` |
| `x402` | 支付子系统,本身不是能力 | 付费演示依赖它 | `ANet/module/x402/x402.go:23-105` |
| `shell` | 加法 tag,默认不编入;空白名单拒绝所有人 | **不可以**(用户明确排除;且与官方公共定位矛盾) | `ANet/module/shell/shell.go:13-38`、`provider.go:85-110` |
| `anetlink` 设备能力 | provider 把 `caller_aid` 传给 anetlinkd;anetlinkd 只声明了该字段,没有使用它做授权 | **不可以**:设备控制对任何 AID 开放 | `ANet/provider/anetlink/anetlink.go:87-93`;`ANetLink/northbound/c1serv/c1serv.go:68`(grep 无其他引用) |
| `auto_reply` openai 后端 | 陌生人文本发往运营者的模型端点 | 仅在有按调用方配额后才可用于公共 agent;第一批不用 | `autoreply.go:1-31` |
| `auto_reply` exec 后端 | 本地编码 agent 按陌生人文本执行命令 | **不可以** | `autoreply_exec.go:16-17` |

### 2.4 x402 现状(与付费演示相关的部分)

- 报价:scheme `anet-credit`,network `hub:<hubAID>`,`payTo` 为提供方 AID,授权窗口 5 分钟;先报本 hub 的账本,再报本 hub 可清算的对端账本(`ANet/module/x402/pay.go:41`、`:57-79`;`ANetCore/payment/payment.go:48-66`)。
- 付款后重试是**第二个 interaction**,第一个 interaction 以报价作为已签收的结果结束(`ANet/internal/daemon/paylink.go:184-219`)。a2a-x402 v0.2 要求付款消息通过同一 `taskId` 回到原任务(`Refs/a2a-x402-spec-v0.2.md:267-290`)。两者的对应关系见 `06-x402.md` §4。
- 结算由提供方把付款交给 hub 的 facilitator,hub 返回签名收据(`module/x402/x402.go:235-260`)。hub 在结算中看到金额、付款方、收款方、interaction id,不需要看到任务内容 `[推断,细节见 06-x402.md §2.6]`。
- 新注册 AID 获赠 100 credit,只发一次(`ANetHub/internal/aghub/facilitator.go:647-672`)。代码注释说明该数额"不值得为之批量铸造身份",因为 AID 可以免费生成(`:649-654`)。
- 凭证门(hub 网关收钱、daemon 公网口取货)需要提供方有公网入口(`ANet/module/x402/voucher.go:24-37`、`x402.go:35-47`)。

### 2.5 卡片与目录

- 签名卡片字段:`SubjectDID`、`Seq`、`Capabilities`(id 列表)、`Name`、可选 `x402-redeem` 端点、可选 `anet.pricing` 扩展(`ANet/internal/daemon/card.go:185-211`)。
- hub 目录只列最近一个月内轮询过的 agent(`ANetHub/internal/aghub/server.go:377-380`)。
- hub 运营面另有 `official_agent` 表,存放含 ssh 运行主机、ops 命令、数据采集开关的 manifest(`ANetHub/internal/admin/store.go:215-235`、`manifest.go:16-46`)。它是运营者自己的记录,不进入签名卡片,客户端无法验证 `[推断]`。

---

## 3. 现状:生产拓扑与目录(实测 2026-09-26)

### 3.1 机器与节点

| 机器 | 角色 | 本次核实 | 出处 |
|---|---|---|---|
| emax(阿里云 4C/7G) | 主 hub `https://hub.agentnetwork.org.cn`,0.1.7;`anet-hub-admin` | admin 单元 ExecStart 只有 `--addr/--hub-data/--data`,未覆盖 `--harvest-every` | memory `anet-prod-topology.md`;实测 |
| fmax(阿里云) | 第二 hub `http://39.107.76.243:4001`;按来源 IP 限制 | ink93 本次能直接 GET `/agents` | memory 同上;实测 |
| dmax(gpu2,64C/1TB/8×A40,现址 10.253.20.5) | `dmax-services` 节点,归属 fmax;另跑 anetos-* 等二十余个服务 | 见 3.2 | memory `anet-servers.md` 2026-09-12 条;实测 |
| cmax(共享开发机,约 110 容器) | `cmax-anet4` 节点,归属 emax,卖 `text.digest`/`text.digest.paid`(卡片价 25) | 目录 `last_seen` 为当日;卡片 `anet.pricing` = `{"text.digest.paid":25}` | `ANet/scripts/prodtest.sh:28-31`;实测 |
| rk3588a / rk3576 | 开发板,归属 emax,`shell.exec`/`shell.list` | 目录可见 | 实测;memory `anet-rk3588-remote.md` |
| dmax-debian-box | 归属 emax,`shell.run@*` 八个命令 | 目录可见 | 实测 |

### 3.2 dmax-services 的配置(实测,只列非密钥字段)

- `name=dmax-services`,`hub_url=http://39.107.76.243:4001`,`accept_delegations=true`,未设 `guest_messages`(即默认 5),无 `auto_reply`。
- 模块:`anetlink, blackboard, cas, org, p2p, service, x402`。
- service:`image.inspect`、`text.stats`、`text.stats.paid`(price 30),后端 `http://127.0.0.1:8500`,由 `anet-dmax-svc.service` 运行 `/data/anet-node/svc/serve.py`(63 行 Python,`/image/inspect` 返回 PNG 尺寸/字节数/crc32,`/text/stats` 返回字数/词数/行数)。
- x402:`voucher_addr=0.0.0.0:4002`,`voucher_url=http://210.45.70.176:4002/x402/redeem`。
- `anet-dmax.service` 单元没有 `User=` 行,以 root 运行;二进制版本 `anet 0.1.6 (commit 789ec8e)`,仓库当前为 0.1.9(`git log` `855f2e0 版本号 0.1.9`)。

### 3.3 目录中已存在、对任何 AID 开放的能力

实测 emax `/agents` 5 条、fmax `/agents` 2 条。按 §2.2–2.3 的代码行为,下列能力当前可被任何能签名的 AID 调用 `[推断:未实际发起调用,依据是代码路径中没有调用方判断]`:

| 节点 | 能力 | 影响 |
|---|---|---|
| dmax-services、cmax-anet4 | `cas.put` | 任何人可写入每次最多 32 MiB 的数据,无次数与总量上限 |
| dmax-services、cmax-anet4 | `blackboard.add` | 任何签名作者的贡献被合并进内存状态 |
| dmax-services | `system.reboot@{dahua,hikvision,onvif}/camera-*`、`switch.onoff@mqttbridge/lock-*`、`ptz.*`、`stream.snapshot@*` 等 | 设备控制与抓拍对任何 AID 开放;哪些是真实设备本次未核实(memory `anetlink-l2-cameras.md` 记有 10.2.2.x 真实海康设备) |
| rk3588a、rk3576、dmax-debian-box | `shell.*` | 由 shell 模块白名单把关(`provider.go:85-110`);三台的白名单内容本次未核实 |

**缺陷:dmax 卡片中的兑付地址不可达。**
- 位置:dmax `/data/anet-node/home/.anet/config.json` 的 `modules.x402.voucher_url`,经签名写入卡片(实测 fmax `/agents/{aid}/card` 的 `endpoints`)。
- 行为:卡片公布 `http://210.45.70.176:4002/x402/redeem`;该地址从 ink93 与 emax 探测均无应答(curl 返回码 000)。
- 影响:经 hub 网关为 `text.stats.paid` 付款的买家拿到凭证后无处兑付,已付款的工作无法取回 `[推断]`。
- 发现方式:本次只读探测;根因与 memory 记录的 210.45.70/71 网段自 2026-09-11 不通一致。

**缺陷:onboard 测试身份残留在生产 hub。**
- 位置:emax `hub.db` 的 `agent` 表。
- 行为:有 6 行 `onboard-min/standard/paid`,注册于 2026-08-25 两个时刻,`last_seen` 同日,其中 4 行声明 `text.digest`/`text.digest.paid`,`guest_quota` 均为 5。因超过一个月未轮询,已不在 `/agents` 列表中(`server.go:377-380`)。
- 影响:这些身份各持有注册赠额 `[推断:按 facilitator.go:661-672,首次注册即入账]`,计入 hub 的负债;它们没有进程在收信,投给它们的中继消息会积压。
- 发现方式:本次 `sqlite3 -readonly` 查询。来源是对生产 hub 运行了 `onboard.sh`(`HUB_EXTERNAL`,`ANet/scripts/onboard.sh:25`),该脚本收尾只杀进程、不注销(`:255-258`)。

### 3.4 hub 侧正在读取中继内容

- 位置:`ANetHub/internal/admin/harvest.go:24-35`(hub-relay 源解码 DelegateReq/ChatMsg/ResultResp,附件只保留元数据),`:199` 起 `runRelay`;调度 `ANetHub/internal/admin/server.go:803-815`;默认间隔 `ANetHub/cmd/anet-hub-admin/main.go:46`(30 分钟),`:135`、`:143` 装配。
- 行为:实测 emax 的 admin 单元未传 `--harvest-every`,即使用默认 30 分钟;`/data/projs/anet-hub/admin/datasets/hub-relay` 38 MB,2026-09-20 以后修改的文件 271 个,最新日期 2026-09-26。`ai-studio` 源另外经 ssh 读取官方 agent 的 `history.jsonl`(`harvest.go:31-33`)。
- 影响:用户任务的目标、对话、交付物以明文落在 hub 运营者磁盘上,与决策 (2) 冲突。官方公共 agent 上线后,其流量同样会被收录,除非先做端到端加密并删除该采集源。
- 发现方式:读代码后对 emax 做只读核对。`04-hub-content.md` §2.7 对 admin 面有逐项说明。

### 3.5 历史参照

memory `anet-servers.md` 2026-08-09 条记录:emax `/opt/anet/anchors` 下三个 anet3 时期的 Python 服务(demo-worker / echo / practice-board)崩溃循环 32k+ 次,源码不在 git,最终停用。这说明"官方 agent"如果没有版本管理、监控与负责人,会以不可用状态长期挂在目录里。第一批设计把这三项作为硬要求(§5.6)。

---

## 4. 与四项决策的冲突清单(限于本篇范围)

| 决策 | 现状冲突 | 出处 | 本篇处理 |
|---|---|---|---|
| (2) hub 只做传输 | 中继载荷明文;admin 采集中继内容;访客模式由 hub 签委派 | §3.4;`guest.go:592-604`;`03-e2e-encryption.md` | 官方 agent 上线以 E2E 与删除采集为前置条件 |
| (2) | `official_agent` 表由 hub 运营者维护,"官方"身份由 hub 断言 | `admin/store.go:215-235` | 官方身份改由客户端可验证的签名清单表达(§5.5) |
| (3) 默认不接受任何委派 | 默认 `accept_delegations=true`,访客配额默认 5 | `config.go:94`、`:98-106` | 依赖 `05-secure-defaults.md` §8.2 的 `inbound` 块;官方 agent 用 `public_capabilities` 显式列出开放能力 |
| (3) 开放来自官方公共 agent | 目录中现有的"开放"是默认配置造成的,包括存储写入与设备控制 | §3.3 | 第一批只开放确定性纯计算能力,并清理现有节点的暴露面 |
| (4) 主推 a2a-x402 | 付费重试是第二个 interaction;示例 `x402Version` 为 1,anet 为 2 | `paylink.go:184-219`;`a2a-x402-spec-v0.2.md:184`;`payment.go:48` | 付费演示作为 a2a-x402 映射的联调载体(§5.2 E) |

---

## 5. 设计:第一批官方公共 agent `[建议]`

### 5.1 选取原则

1. 对 coding agent 有用,或者对"验证本网络是否可用"有用。
2. 确定性、纯计算、输入输出有上限。同样的输入得到同样的输出,第三方可以复算。
3. 不执行命令,不访问外网,不接受 URL 参数(避免 SSRF 与内容转发),不写任何跨调用持久化的共享状态。
4. 只接受**能力调用**,拒绝自然语言委派。自然语言委派会进入 interactions 并可能被转交自动回复(`capability.go:308-310`),这是模型额度与提示注入的入口。
5. 每个官方 agent 是独立身份(独立 AID、独立签名卡片、独立证据链、独立限流预算)。理由:A2A 的 AgentCard 按 agent 声明扩展,付费 agent 按 a2a-x402 建议声明 `required: true`(`a2a-x402-spec-v0.2.md:33-35`),若与免费能力放在同一身份,不支持 x402 的客户端连免费能力也用不了。代价是多几个 daemon 进程 `[推断:单个 standard 档二进制约 12 MB,见 DISTRIBUTIONS-zh.md:199]`。

### 5.2 第一批清单

| 代号 | 身份名(建议) | 能力 | 收费 | 对 coding agent 的用途 | 可信度 |
|---|---|---|---|---|---|
| A1 | `anet-echo-e`(归属 emax) | `net.echo` | 免费 | 新装后自检:委派、收回、验回执,测往返时延;prodtest 每小时探测 | 调用方可自行比对原文,等价于读回 |
| A2 | `anet-echo-f`(归属 fmax) | `net.echo` | 免费 | 同上,并覆盖跨 hub 投递与联邦目录 | 同上 |
| B | `anet-tools` | `text.stats`、`text.digest`(sha256/sha512)、`text.diff`(统一 diff)、`json.validate`(JSON Schema 2020-12,错误带 JSON Pointer)、`a2a.card.validate`、`a2a.x402.check` | 免费 | 比对多个 agent 的产出、校验结构化输出、检查自己写的 A2A AgentCard 与 x402 元数据是否合规 | 确定性,调用方可复算 |
| C | `anet-docs` | `docs.search`、`docs.get` | 免费 | 按关键词检索 anet 文档、A2A 规范、a2a-x402 规范、a2a-go 文档,返回带 `路径:行` 与语料版本 CID 的片段 | 确定性:同一语料 CID 同一查询同一结果 |
| E | `anet-paid-demo` | `demo.digest.paid`(与 B 的 `text.digest` 输出相同) | 付费,建议 1–5 credit | 用注册赠额走通 a2a-x402:`payment-required → payment-submitted → payment-completed`,拿到 hub 签名收据 | 与免费孪生能力输出逐字节相同,可证明"付的是执行,不是别的" |

各能力的参数与上限(初值,需在联调中校准):

| 能力 | 参数 | 输入上限 | 输出 | 后端时限 |
|---|---|---|---|---|
| `net.echo` | 任意 JSON 对象 | 4 KiB | 原样返回 + 后端收到时间 + 服务版本 | 1 s |
| `text.stats` / `text.digest` | `text` | 256 KiB | 计数 / 摘要 | 2 s |
| `text.diff` | `a`、`b`、可选 `context` | 各 256 KiB | unified diff,截断到 256 KiB 并标记 | 5 s |
| `json.validate` | `schema`、`instance` | 各 256 KiB;禁止远程 `$ref` | `valid` 与错误列表 | 5 s |
| `a2a.card.validate` | `card`(JSON) | 64 KiB | 按 `a2a.proto` 必填字段(`:362-398`、`:436-452`)与扩展声明形状逐项报告 | 2 s |
| `a2a.x402.check` | `metadata` 序列 | 64 KiB | 按 a2a-x402 §7 的键与状态迁移逐项报告(`a2a-x402-spec-v0.2.md:396-424`) | 2 s |
| `docs.search` | `query`、可选 `corpus`、`k≤10` | 2 KiB | 片段数组,每段 ≤1.5 KiB,附 `source`、`lines`、`corpus_cid` | 3 s |
| `docs.get` | `source`、`lines` | — | 指定行区间原文,≤16 KiB | 1 s |
| `demo.digest.paid` | `text` | 4 KiB | 同 `text.digest` | 2 s |

后端实现:一个 Go 程序,监听 127.0.0.1,每类能力一组路由;`a2a.*` 用 a2a-go v2 的类型反序列化后再做必填项检查(`Refs/a2a-go`,module `github.com/a2aproject/a2a-go/v2`)。它是独立二进制,不链入 daemon,因此不影响 daemon 的 tag 矩阵;daemon 侧只用现有 `service` 模块挂载。`[推断]` 放在 `ANet/cmd/anet-official/` 或独立仓库均可,前者便于与 daemon 同版本发布。

`docs.search` 的语料在构建时打包并计算 CID;语料更新即发新版本。不做在线抓取。

明确不纳入第一批的候选及理由:

| 候选 | 不纳入的理由 |
|---|---|
| 网页检索 / URL 抓取(dmax 上有 searxng,见 memory `anet-servers.md`) | 需要外网出口;把官方 agent 变成任意内容的转发点;SSRF 面 |
| 代码格式化(prettier、black 等) | 需要执行外部二进制;只有 Go 的 `go/format` 可以进程内完成,价值不足以单独立项 |
| `docs.ask`(模型问答) | 需要按调用方的配额与费用控制;放到第二批,且以付费或低免费额度提供 |
| `cas.*`、`blackboard.*`、`taskboard`、设备能力、shell、exec 自动回复 | 见 §2.3 |

### 5.3 运行位置与方式

| 项 | 建议 | 依据 |
|---|---|---|
| 机器 | 全部放 dmax;不放 emax/fmax | 两台 hub 机器不持有任何能解密任务内容的密钥,"hub 只做传输"才能按机器划界陈述 `[推断]`;emax 仅 4C/7G(memory);dmax 从 ink93 可 ssh(memory 2026-09-12 条),cmax 当前不可达 |
| 网络 | 只需出站到两台 hub;不开公网入口,不配 `voucher_addr` | daemon 以轮询收信(`relay.go:32-35`);实测 dmax 在私网地址下仍对 emax、fmax 保持轮询(目录 `last_seen` 为当日);这本身演示了"不能当 HTTPS 服务器的 agent"场景 |
| 归属 hub | A1 归 emax,A2 归 fmax,B/C/E 归 emax | 覆盖跨 hub 投递;付费演示的跨 hub 清算作为第二步(memory `anet-cross-hub-credit.md`) |
| 身份 | 每个代号一个 HOME、一个 AID、一个 systemd 单元 | §5.1 第 5 条 |
| 构建档位 | A1/A2/B/C 用 `standard`;E 用 `paid`;均不带 `shell` | `DISTRIBUTIONS-zh.md:198-200` |
| 系统用户 | 专用非 root 用户 `anet-official`;现 `anet-dmax.service` 以 root 运行,不沿用 | 实测单元无 `User=` |
| systemd 约束 | `NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、`PrivateTmp`、`ReadWritePaths` 仅数据目录、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`、`MemoryMax`、`CPUQuota`;后端单元另加 `IPAddressDeny=any` + `IPAddressAllow=localhost` | 后端不需要外网 |
| 版本 | 与 daemon 同版本发布;带版本文件名经 cmax 中转分发的规则在 cmax 不可达期间改为 ink93 直推 dmax(内网) | CLAUDE.md "部署";memory 2026-09-12 条 |
| 监控 | prodtest 每小时 `--no-write` 段加入对 A1/A2/B/C 的只读调用(调用本身不写链以外的状态);E 只在写模式段 | `ANet/deploy/prodtest.timer`(`OnCalendar=hourly`) |
| 负责人 | 在官方清单中写明维护者与联系方式 | §3.5 |

### 5.4 限流与防滥用(五层)

内容只出现在请求方 daemon、官方 agent 的 daemon 与其 127.0.0.1 后端之间。每一层都说明它看得见什么。

**第 1 层:准入(daemon 内核,依赖 `05-secure-defaults.md` §8.2)**
- 官方 agent 配置 `inbound.policy = "closed"`,`public_capabilities` 列出本身份的能力。选 `closed` 而不是 `open`:`open` 会把任何人的自然语言任务写进 interactions,官方 agent 不需要。
- 非公开能力与自然语言委派一律答 `UNAVAILABLE`(或 A2A `TASK_STATE_REJECTED`,映射见 `02-daemon-task-path.md`)。
- 凭证门同样要求能力在 `public_capabilities` 内(`05-secure-defaults.md` §8.2 已提出在 `voucher.go:252` 附近加检查)。

**第 2 层:按能力的配额(daemon 内核,新增)**
- 位置:`tryCapabilityPaid` 在解析 provider 之后、定价闸门之前(`capability.go:288`–`:339` 之间)。
- 配置形状 `[建议]`,挂在 `public_capabilities` 的每一项上:

```json
"public_capabilities": [
  {"id": "net.echo", "per_caller_per_min": 60, "per_caller_per_day": 2000,
   "global_per_min": 1200, "max_inflight": 16, "max_args_bytes": 4096},
  {"id": "docs.search", "per_caller_per_min": 20, "per_caller_per_day": 1000,
   "global_per_min": 300, "max_inflight": 8, "max_args_bytes": 2048},
  {"id": "demo.digest.paid", "global_per_min": 300, "max_inflight": 8, "max_args_bytes": 4096}
]
```

- 超限答 `UNAVAILABLE`,`message` 写明哪条上限、`retry_after_ms`。不用 `FAILED`:没有执行,请求方需要能区分"忙"与"坏"(与 `capability.go:53-66` 的既有理由一致)。
- 放内核而不是模块的理由:它与准入同属"内核是否接受这次调用"。若做成减法模块,`-tags no_<name>` 的构建会失去限流而保留公开能力,方向与安全默认相反 `[推断]`。若评审认为必须可插拔,应参照 shell 的做法做成**加法**方向,且公开能力在无该模块的构建中不可声明。
- 付费能力只设全局与并发上限,不设按调用方上限:付款本身是速率约束。

**第 3 层:后端自身**
- service 模块把**已验证的**调用方 AID 与 interaction id 以请求头传给后端(`X-ANet-Caller`、`X-ANet-Call`),后端据此记日志与二次限流。需要改 `service.go:157-161`。后端只监听 127.0.0.1,头部不可被外部伪造 `[推断:前提是 daemon 与后端之间没有别的进程可达该端口,由 systemd 的 IPAddressAllow 与专用用户保证]`。
- 每个能力的 `timeout_ms` 按 §5.2 表设置;现为整体一个值(`service.go:78`),建议允许按能力覆盖。
- 回复上限沿用 1 MiB(`service.go:224`)。

**第 4 层:hub,只做与内容无关的计量**
- 按来源 IP 对 `/relay/send`、`/register` 限速(nginx `limit_req`),按收件方限制未投递消息的条数与字节数。hub 不需要知道收件方是否"官方"。
- 按发送方限流需要认证发送(`04-hub-content.md` §4.2 方案 a),当前 `from_aid` 由调用方声明、不校验(`server.go:871-880`、`:882-931`)。
- 不在 hub 上为官方 agent 设任何特权路由或白名单。

**第 5 层:身份成本与处置**
- AID 免费生成,所以按调用方的上限只能约束单个身份;全局上限与并发上限是机器层面的最终保护。
- 注册赠额 100 credit(`facilitator.go:655`)。付费演示按 1–5 credit 定价时,批量注册只换来有限次演示调用。若 credit 将来可兑付为现金,赠额会成为批量注册的动机,需另行评估 `[推断]`。
- 滥用处置:官方 agent 的证据链逐条记录 `capability`、`caller_aid`、`status`(`capability.go:432-441`),据此统计并把 AID 写入 `deny_file`(`05-secure-defaults.md` §8.2)。处置在官方 agent 本地完成,不需要 hub 看内容。
- hub 的邀请准入(`ANetHub/internal/aghub/invite.go:3-20`、`:131-132`)保持关闭:打开它会让未受邀者连官方 agent 也用不了,与决策 (3) 的"开放来自官方 agent"相反。

### 5.5 在"hub 只做传输"约束下的其余要点

1. **前置条件:端到端加密上线,且 hub 删除 relay 采集源。** 在此之前上线官方 agent,其流量会以明文进入 `admin/datasets/hub-relay`(§3.4)。
2. **官方身份由客户端验证,不由 hub 断言。** `[建议]` 项目维护一把发布签名密钥,公钥随 daemon 二进制发布;官方清单(每项:AID、归属 hub、能力列表、维护者、清单版本、过期时间)由该密钥签名,随版本打包,并在官网以 HTTPS 发布更新。`agents_find`(`ANet/internal/mcpserv/mcpserv.go:43`)与本地 A2A 接口在结果里标注 `official: true`,依据只有该签名。hub 的 `official_agent` 表仍可作为运营者的运维记录,但不进入任何客户端可见的信任判断,其 `datasets.harvest` 对官方 agent 关闭。
3. **名称不可信。** 卡片名称是自述(`card.go:195`),任何人可以注册同名节点。官方判定只看 AID 是否在签名清单内。
4. **官方 agent 自身能看到调用内容,这一点要公开写明。** 它们是端点,不是传输层。现状下:
   - interactions 存储保存 TaskDoc 与交付物,未找到任何清理代码(grep `internal/runtime/interactions` 无 prune/retention);
   - 证据链的 `anet.capability.effect` 事件包含 provenance,其中 `observed_state` 即 service 后端的完整回复(`capability.go:437-439`、`ANet/provider/provenance.go:30-31`、`service.go:205`);证据链只追加,这部分内容会一直保留。
   - `[建议]` 为公共能力增加保存策略:interactions 按天数清理;公共能力的证据事件只记 `result_cid` 与指标,不记 `observed_state`。这会改变 C5 证据面的内容,需在契约文档中写明,见 §8 未决问题。
5. **访客模式的替代。** 访客模式移除后,"不装软件先试"的入口改为官网上的只读演示(展示官方 agent 的卡片、示例输入与签名回执),不经 hub 转发任何访客文本。`[推断]` 这与 `04-hub-content.md` §4.4 的选项之一一致。

### 5.6 与 A2A 的对接

1. **技能描述进签名卡片。** `[建议]` 新增卡片扩展 `anet.skills`,值为 `{capability_id: {name, description, tags, examples, input_modes, output_modes, args_schema}}`,由 service 配置中的对应字段生成并签入卡片(改 `card.go:185-211`,并让 `service.go:91` 的 `description` 与新增字段经 provider 接口暴露)。本地 daemon 为每个远端 agent 合成 A2A AgentCard 时,把它映射为 `AgentSkill`(`a2a.proto:436-452`)。官方 agent 是第一批使用者,负责把描述与示例写全。
2. **x402 扩展声明。** 付费演示 E 的合成 AgentCard 在 `capabilities.extensions` 中声明 a2a-x402 v0.2 URI,`required: true`(`a2a-x402-spec-v0.2.md:15-35`)。免费身份不声明。
3. **付费演示要验证的差异点**(细节见 `06-x402.md`):付款是否回到同一任务;`x402Version` 1 与 2 的差异;`anet-credit` 作为自定义 scheme 时客户端如何识别;`x402.payment.receipts` 中携带 hub 签名收据(`ExtReceipt`,`ANetCore/payment/payment.go:381-382`)。
4. **对 A2A 生态的贡献面。** `a2a.card.validate` 与 `a2a.x402.check` 的校验规则可以整理后提交给 a2aproject 作为一致性检查工具 `[建议]`;`docs.search` 的语料包含 A2A 规范本身,使 coding agent 在网络内即可查规范原文。

### 5.7 新用户的第一条路径

1. 安装并启动,默认不接受任何委派(依赖 `05-secure-defaults.md`)。
2. 首次运行时,daemon 对 A1 发起一次 `net.echo`,验证回执签名,打印往返时延与所经 hub。失败时按"没有 hub / hub 不可达 / 对端不在线 / 回执验证失败"分别报告。
3. `anet install --agent <tool>` 写入的指引(`ANet/scripts/onboard.sh:222-246` 验证其存在)增加一段:列出官方 agent 与各能力的一行用途、调用示例。
4. 付费路径的第一次体验用 E:注册赠额足够完成若干次演示调用,结果与 B 的免费能力逐字节比对。

---

## 6. 需要的改动(按依赖顺序)

| 序 | 改动 | 位置 | 依赖 |
|---|---|---|---|
| 1 | 端到端加密信封 | ANetCore / ANet / ANetHub,见 `03-e2e-encryption.md` §5 | — |
| 2 | 删除 hub admin 的 hub-relay 采集源;官方 agent 的 ai-studio 采集关闭;清理 emax 上已采集的 `datasets/hub-relay` | `ANetHub/internal/admin/harvest.go:24-35`、`:199` 起;`cmd/anet-hub-admin/main.go:46`;emax 数据目录 | 清理需用户决定 |
| 3 | `inbound` 策略与 `public_capabilities` | `delegation.go:488-490`、`capability.go:288-322`,见 `05-secure-defaults.md` §8.2 | — |
| 4 | 按能力的配额与并发上限,超限答 `UNAVAILABLE` + `retry_after_ms` | `capability.go:288-339` 之间 | 3 |
| 5 | service 模块向后端转发已验证调用方与 interaction id;按能力的超时 | `service.go:73-95`、`:157-161` | — |
| 6 | 卡片扩展 `anet.skills`;service 配置增加 `name/tags/examples/args_schema`;本地 A2A 接口映射为 `AgentSkill` | `card.go:185-211`、`service.go:81-95`;provider 接口需增加可选的技能描述方法 | — |
| 7 | 官方 agent 后端程序(§5.2) | 新增 `ANet/cmd/anet-official/`(或独立仓库) | — |
| 8 | 官方清单签名与客户端验证;`agents_find` 标注 | 新增发布密钥与清单格式;`internal/mcpserv/mcpserv.go:43` | — |
| 9 | 公共能力的保存策略(interactions 清理、证据只记 CID 的选项) | `internal/runtime/interactions`;`capability.go:432-441` | 需先定 §8 第 5 问 |
| 10 | 付费演示走 a2a-x402 同任务流 | `paylink.go:184-219` 等,见 `06-x402.md` | 1、3 |
| 11 | 访客模式移除或改造 | `ANetHub/internal/aghub/guest.go` | 见 `04-hub-content.md` §4.4 |
| 12 | 清理现有生产暴露面:dmax-services 与 cmax-anet4 去掉 `cas.put`/`blackboard.add` 的公开;dmax 设备能力不进 `public_capabilities`;修正或撤下 dmax 的 `voucher_url`;dmax 升级到当前版本;注销 6 个 onboard 残留身份;`onboard.sh` 在 `HUB_EXTERNAL` 时收尾注销 | dmax/cmax 配置;emax hub;`ANet/scripts/onboard.sh:255-258` | 3 |
| 13 | ANetLink 按 `caller_aid` 授权(至少对 `system.reboot`、门锁类开关) | `ANetLink/northbound/c1serv/c1serv.go:68` 起 | 与 3 互补 |

---

## 7. 测试方案 `[建议]`

按 CLAUDE.md 的要求,每条跨进程接缝都要有联调覆盖,新断言用 mutation 验证。

1. **`scripts/joint-official.sh`**(本机多进程:hub + 5 个官方身份 + 后端 + 一个全新安装的 daemon):
   - 全新 daemon 调用 A1/B/C 成功,回执可由 `anet verify` 离线验证;
   - 全新 daemon 自身收到的自然语言委派与能力调用均被拒绝,interactions 中无记录;
   - 官方 agent 拒绝自然语言委派与非公开能力,答 `UNAVAILABLE`;
   - 同一调用方超出每分钟上限后答 `UNAVAILABLE` 且带 `retry_after_ms`,另一调用方不受影响;全局上限生效时两者都受限;
   - 付费演示按 a2a-x402 的状态序列完成,两端证据链各有一条结算记录,收据验签通过;
   - **内容不可见断言**:请求参数中放入随机标记串,在 hub 的 `hub.db`(`relay_message` 表)与 hub 数据目录中 grep 该串,必须无命中。mutation:关闭加密后该断言必须失败。
   - mutation 另验:去掉配额检查后超限断言失败;把非公开能力加入 `public_capabilities` 后拒绝断言失败。
2. **`scripts/scenario.sh`**:两 hub 拓扑中加入 A1/A2,覆盖跨 hub 调用与联邦目录中的官方标注。
3. **`scripts/prodtest.sh`**:只读段每小时调用 A1、A2、C(固定查询,比对 `corpus_cid` 与首条结果),B 的一个确定性样例;写模式段跑 E。
4. **契约测试**:`anet.skills` 扩展的字段名与 A2A `AgentSkill` 映射;官方清单签名格式(ANet 与官网发布端各一份,参照 `internal/hubapi/hubapi_test.go` 的做法)。
5. **后端单测**:`json.validate` 拒绝远程 `$ref`;`text.diff` 截断标记;`a2a.card.validate` 对 `a2a.proto` 必填字段逐一缺失时报错。
6. **tag 矩阵**:后端是独立二进制,不进 daemon,不新增 tag。若第 4 项配额被做成模块,按 CLAUDE.md 同时验证完整构建符号数 > 0 与去除构建 == 0,并更新 `.github/workflows/ci.yml`。

---

## 8. 未决问题

1. 付费演示定价:1 还是 5 credit;credit 是否会有现金兑付,若有则注册赠额的批量注册动机需要重新评估。
2. 官方 agent 全部放 dmax 是单点;是否需要第二台机器,以及 cmax 恢复可达后是否承担一部分。
3. `docs.search` 语料的许可:A2A 规范与 a2a-go 的许可需逐一核对,a2a-x402 规范文本的许可本次未核实。
4. `docs.ask`(模型问答)进入第二批时,模型放 dmax 本地还是走 emax 的 tb-newapi;免费额度还是只收费。
5. 公共能力的证据事件是否允许只记 `result_cid`:这会改变 C5 证据面的内容,需要在 `docs/CONTRACTS-zh.md` 中写明两种模式。
6. 官方清单是随二进制发布(改清单需发版),还是在线获取(需 HTTPS 与公钥固定),或两者兼用(内置为下限,在线为增补)。
7. 是否在 hub 上保留"官方"名称前缀(例如禁止他人注册 `anet-` 前缀),以减少显示层面的混淆;这属于 hub 策略,与"hub 只做传输"不冲突,但需要决定。
8. `a2a.card.validate` 是否以独立项目形式提交给 a2aproject,以及以哪个版本的 `a2a.proto` 为准。
9. 现有生产暴露面的清理(§6 第 12 项)涉及正在运行的 dmax 设备能力与 prodtest 的依赖,需要用户确认影响范围后再做。
