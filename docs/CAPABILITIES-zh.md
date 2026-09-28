# anet daemon 功能清单与完备程度

对照的是**当前仓库里跑得起来的东西**,不是设计意图。每一项的"完备程度"
按四档给,判据写在后面:

| 档位 | 判据 |
|---|---|
| **联调验证** | 有单测,且在联调脚本(`scripts/joint*.sh`、`scenario.sh`、`onboard.sh`)里跨进程真的跑通 |
| **单测覆盖** | 有针对性的单测(含否定用例),但没进联调 |
| **仅可用** | 代码在跑,没有直接测试,靠上层间接覆盖 |
| **未做** | 不存在,或只有占位 |

**当前状态(v0.2 开发分支,2026-09-27)**:ANet 约 52,000 行实现 / 35,000 行测试,726 个测试 + 13 个基准,
`go.sum` 116 条。v0.2 的 wire 2 改动之后,联调脚本已按新语义改写,但**全量联调(设计 §20 阶段 F)还没跑完**:
下表里 v0.2 新增的项一律按"单测覆盖"记,0.1.x 已在联调里验证过、v0.2 未改语义的项记"联调验证(0.1.x)",
阶段 F 跑完后再逐项更新。阶段 F 已于 2026-09-28 跑完(见第四节);下表的逐项档位尚未按其结果改写。

---

## 一、内核(不可插拔)

内核是"拔掉就不是 anet daemon"的部分,没有构建标签。

| 能力 | 位置 | 完备程度 | 说明 |
|---|---|---|---|
| 身份(KEL/AID) | ANetCore `identity` | 联调验证(0.1.x) | 一个 runtime = 一个 agent = 一个 AID;v0.2 加 `ExtendsKEL` 只接受延伸 |
| 加密密钥环 | `internal/daemon/enckeys.go`、ANetCore `seal` | 单测覆盖 | 每 7 天新建,有效 14 天,失效后保留 15 天;签名的 `EncKeySet` 注册到 hub |
| 封装发送 / 解封接收 | `seal_send.go`、`receive.go` | 单测覆盖 | 先签后封;只收封装信封;逐字段变异全部拒收;暂时性失败不 ack |
| 对端身份持久记录 | `peerkel.go`、`interactions` `peer_identity` | 单测覆盖 | 首次见面信任,之后只接受延伸;回退与分叉拒收 |
| outbox 可靠投递 | `retry.go` | 单测覆盖 | 持久化信封字节,指数退避最长 24 小时,重启恢复 |
| 能力注册表 (C1) | `provider/` | 联调验证(0.1.x) | daemon 只认 provider/capability/evidence,**不认"设备"** |
| 任务状态机 | `delegation.go`、`interactions` | 单测覆盖 | A2A 七态;事件显式写入,`state_seq` 单调;终态互不覆盖;提供方单方完成 |
| 能力调用五态 | `capability.go` | 单测覆盖 | `OK`/`UNVERIFIED`/`FAILED`/`UNAVAILABLE`/`PAYMENT_REQUIRED`;中断记 `UNVERIFIED` + `interrupted` |
| 入站策略与名单 | `inbound.go`、`pending.go` | 单测覆盖 | `closed`/`approve`/`open`;allow/trust/deny 每次重读;公开能力准入与配额;待批队列 |
| 委派验签 | ANetCore `delegation` | 单测覆盖 | 8 种冒充方式逐一被拒 |
| **收据验证** | ANetCore `delegation.VerifyResult` | 联调验证(0.1.x) | 请求方接受结果前逐项绑定;7 种"持有效签名仍撒谎"的方式被拒 |
| **第三方验证** | `anet verify` | 联调验证(0.1.x) | 无 daemon、无 hub、无网络;v0.2 加 `--chain` 核验导出的证据链 |
| 中继鉴权 v2 | ANetCore `relayauth` | 单测覆盖 | 发送、取信、确认、账本读取域分离;原像字节钉死 |
| 证据链 (P6/C5) | `ledger.go`、`audit.go` | 联调验证(0.1.x) | verify-before-use;v0.2 加 `anet audit`、`--export`、新事件类型 |
| 收据 + 评价 | ANetCore `evidence` | 联调验证(0.1.x) | v0.2 评价不带内容;对话记录 v2(带随机数) |
| 支出策略 | `spend.go`、`x402task.go` | 单测覆盖 | `AdmitSpend` 唯一检查点;自动 / agent / 人工 / 网关 / 兑付五种用途;收款方名单 |
| a2a-x402 同任务流 | `x402task.go`、`internal/x402a2a` | 单测覆盖 | 报价、付款、商户核对、结算、`payment-verified`、`payment-completed` + 收据 |
| A2A 投影 | `internal/a2ashape`、`taskview.go` | 单测覆盖 | 控制面与 MCP 直接输出;契约测试把输出反序列化为 a2a-go `a2a.Task` |
| `TaskSeam` 与 `/tasks/*` | `taskseam*.go`、`tasks_api.go` | 单测覆盖 | 按对端 AID 作用域;阻塞等待比较 `state_seq` |
| A2A 网络卡片 | `a2a_card.go`、`internal/netcard` | 单测覆盖 | 只含公开能力;发布形;高水位 |
| 传输列表 | `transport.go` | 联调验证(0.1.x) | hub 是兜底,不是唯一;dispatch 约 500ns |
| 控制面 | `control_api.go`、`ctlsec.go`、`session.go` | 单测覆盖 | 约 55 个路由;回环 Host 白名单、Bearer、控制台票据与会话白名单(测试遍历全部路由) |
| CLI | `cmd/anet` | 单测覆盖 | 约 45 个子命令;帮助里承诺的每个参数都被参数检查接受(测试钉住) |
| `init` / `doctor` | `cmd/anet`、`setup.go` | 单测覆盖 | 幂等写出 SI-5 默认值;`doctor --json` 按键输出 |
| `update` 与发布签名 | `internal/release` | 单测覆盖 | 编入的发布公钥验清单,原子替换 |
| 自动回复 | `internal/daemon/autoreply*.go` | 单测覆盖 | exec 只为信任对端运行;沙箱(Linux,bubblewrap)失败闭合;环境变量白名单 |
| 身份管理(多身份) | `internal/daemon/identities.go` | 仅可用 | `anet id ls/new/use/rm` |

## 二、可插拔模块

减法 tag(`-tags no_<name>`)后二进制里**符号数为 0**,加法 tag(`shell`、`taskboard`)不加就没有;
CI 用 `go tool nm` 双向计数,不是"配置关掉"。

| 模块 | 能力 / 作用 | 完备程度 |
|---|---|---|
| `a2a` | 本机 A2A 接口(JSON-RPC 与 HTTP+JSON、代理卡片、独立令牌);可选提供侧后端 | 单测覆盖(`joint-a2a.sh` 与 a2a-tck 记录在阶段 F) |
| `mcp`(`internal/mcpserv`) | 14 个按 A2A 概念命名的工具;真客户端探针在联调脚本里 | 单测覆盖 |
| `agentwire` | `anet agents wire` / `unwire`:Claude Code、Codex、Cursor、opencode、Hermes | 单测覆盖(假 HOME 往返) |
| `x402` | 标价、报价、结算、兑付、对账、见证 | 联调验证(0.1.x);同任务流单测覆盖 |
| `service` | 本机 HTTP 服务挂成能力;后端推荐 Unix socket(核对路径与监听者),TCP 须 `allow_tcp`;每后端令牌;`X-ANet-Caller` | 单测覆盖 |
| `anetlink` | 由 ANetLink 提供(`ptz.absolute@onvif/camera-006` 等) | 联调验证(0.1.x) |
| `cas` | `cas.put` `cas.get` `cas.has` `cas.stat` | 联调验证(0.1.x) |
| `blackboard` | `blackboard.add` `blackboard.snapshot` `blackboard.conclude` | 联调验证(0.1.x) |
| `org` | `org.verify` `org.info` | 联调验证(0.1.x) |
| `p2p` | 传输(不是能力);帧版本化 | 联调验证(0.1.x) |
| `taskboard`(加法) | hub 公共任务板客户端 | 单测覆盖 |
| `shell`(加法) | 运营者批准的命令;三道闸门 | 联调验证(0.1.x,`joint-shell.sh`) |
| `inv1` / `inv2` | 不变式守卫 | 单测覆盖 / 联调验证(0.1.x) |

### 性能实测(Xeon E5-2603 v4 @1.70GHz,0.1.x)

| 操作 | 耗时 | 备注 |
|---|---|---|
| 传输 dispatch | 513 ns | 每次委派都走,必须可忽略 |
| 黑板 add(首次,验签) | 281 µs | 成本是验签 |
| 黑板 add(重复) | 3.4 µs | **83× 快**:先算 id 提前返回,且严格更安全 |
| 黑板 snapshot(4096 单元) | 2.5 ms | 线性 |
| CAS put 1MB | 6.4 ms | 164 MB/s |
| CAS get 1MB(读回重哈希) | 6.7 ms | 158 MB/s |
| org 验证(founder 签发) | 307 µs | 一次签名验证 |
| org 验证(admin 签发) | 614 µs | 两跳链,两次验证 |
| 凭证序列化 | 3.5 µs | |

v0.2 的封装/解封开销没有单独测,阶段 F 补测。

## 三、联调覆盖

| 脚本 | 覆盖 |
|---|---|
| `scripts/joint.sh` | 四仓六进程:设备 PTZ、CAS、黑板、org、p2p 双向直连、证据链重启;v0.2 起提供方经允许名单接单。SI-1 canary(hub 与运营面字节搜索任务内容)与陌生节点被拒在计划 0014 B5-02 |
| `scripts/joint-fleet.sh` | 多节点经 hub 委派、MCP 真客户端握手与工具调用 |
| `scripts/joint-shell.sh` | shell 模块三道闸门 |
| `scripts/scenario.sh` | 两 hub 跨 hub 加密委派、付费、跨 hub 结算 |
| `scripts/joint-a2a.sh`、`joint-official.sh`(待写,计划 0014 B5-03、B6-01) | 本机 A2A 接口(未修改的 a2a-go 客户端)、官方 agent 五层防护 |

0.1.x 联调发现的五个单测看不见的缺陷(两边单测各自伪造对方,全绿):

1. 能力委派路径无人可达(`--capability` 落进 goal 文本)
2. 读能力跨线只写不读(CID/blob/列表放在 `ObservedState`,不上线)
3. ANetLink 的 C1 线丢弃 quirk 标签
4. 证据链用无法表达自身 id 的编码落盘
5. p2p 传输送不回一个回复(入站处理占住读循环)

## 四、已知缺口(按重要性)

| 缺口 | 影响 | 状态 |
|---|---|---|
| v0.2 全量联调与 a2a-tck | 已跑完:本机与实验室测试网的联调脚本全绿;a2a-tck 余下的失败项逐项归因(0029 §4) | 已完成(阶段 F):本机全量 [0023](notes/0023-验证-本机全量.md);红队修复后实验室复跑 [0028](notes/0028-验证-红队修复后实验室复跑.md)、本机全量 [0029](notes/0029-验证-红队修复后本机全量.md) |
| daemon 包 `-race` | 原为测试钩子直接赋值构成数据竞争;钩子已改为原子指针,三仓 `-race` 全绿。该包在 `-race` 下单包约 11–12 分钟,全仓 `-race` 须带 `-timeout 45m`(`CONTRIBUTING.md`、CI) | 已修(`wp/hyg`,计划 0014 B2-01) |
| MCP 与本机 A2A 接口的"结束请求" | 这两个面上只能取消、不能请求完成(CLI 与控制台可以) | 设计已定(0017 Q4),未接入 |
| 治理纪元 `govepoch` | org 只接受 epoch 0 | 等 ANetCore 的 `ascpevo.GovernanceCert` |
| `module/anetlink` 无直接测试 | 81 行薄封装,`provider/anetlink` 有 3 个测试 | 仅可用 |
| `anetfixture` 无单测 | 是联调工具本身,由联调脚本端到端使用 | 仅可用 |

hub 与他人仍可见的元数据、前向保密窗口等设计上的局限不在本表,见 [已知局限](KNOWN-LIMITATIONS-zh.md)。

## 五、刻意不做

- **libp2p commons 织物**(anet3 的 2,352 行):进程外。构建标签移除代码,
  从不移除 `go.mod` 依赖。`tools/anetpeer` 是那份契约的参考实现。
- **org-central 及六个卫星**(anet3 的 5,033 行):`module/org` 只回答
  "这份凭证是不是这个组织的有效成员"。看板、任务循环、群密钥若要存在,
  是 org-central 自己通过 C1 提供能力,不是 daemon 里多七个包。
- **hub 侧 A2A 网关**:外部 A2A 客户端经本机 daemon 的 127.0.0.1 接口接入,hub 只做传输(设计 §0)。
- 细节见 [REWRITE-from-anet3-zh.md](REWRITE-from-anet3-zh.md)。
