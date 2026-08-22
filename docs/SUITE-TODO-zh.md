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

### TODO

| # | 条目 | 依赖 | 优先级 |
|---|---|---|---|
| ~~D-1~~ | ~~MCP 接入面~~ | — | **已完成**。`anet mcp` stdio 服务,7 个工具(agents_find / task_delegate / task_results / task_inbox / task_message / task_end / node_status),`no_mcp` 可减。依赖代价 24 条(82 → 100) |
| ~~D-2~~ | ~~C5 证据链查询接口~~ | — | **已完成**。`anet evidence`、控制面 `POST /evidence`、MCP `evidence_read`;每条带 id / prev_id / 签名,可核而非可信;`head.state` 暴露 QUARANTINED |
| **D-3** | 按 C1 能力 id 发现 | **H-1** | 中。能力 id 精确、结构化、机器可解析,而查找路径完全不用它 |
| **D-4** | federation 感知 | **H-4** | 中。daemon 侧 grep 不到一个 federation 字样 |
| **D-5** | 结算 | **H-3** | 中。`pricing` 只是展示字符串 |
| **D-6** | 治理纪元 `govepoch` | **C-1** | 低。org 目前只接受 epoch 0 |
| **D-7** | 崩溃恢复语义未验证 | 无 | 中。daemon 中途挂掉,in-flight 委派处于什么状态,没测过 |
| **D-8** | 自动回复未进联调 | 无 | 中。只在单测里跑过 |
| **D-9** | 分发形态 | 无 | 中。今天需 `go build` + 终端。桌面 app / 浏览器扩展 / 托管三选待议 |
| **D-10** | 三处无直接测试 | 无 | 低。`module/anetlink`(81 行薄封装)、`internal/hubapi`(纯类型)、`tools/anetfixture`(联调工具本身) |

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
| **H-1** | 发现是 `LIKE %q%` 子串匹配 | **D-3** | `aid LIKE ? OR name LIKE ? OR caps LIKE ? OR summary LIKE ? OR readme LIKE ?`。C1 能力 id 完全没用上 |
| **H-2** | **不发布 KEL** | 第三方验证闭环 | 库里存了(`SELECT kel FROM agent`),`GET /agents/{aid}` 不返回。第三方拿到收据后无处取验证密钥 —— 目前只能由请求方转发。**这是下一个** |
| **H-3** | 无结算 | **D-5** | 一个委派网络没有结算是 demo |
| **H-4** | federation 只做了投递面 | **D-4** | 目录联邦、信誉联邦未做 |
| **H-5** | 测试密度偏低 | — | 6,949 行实现对 35 个测试;webui 2,316 行基本无测试 |

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
