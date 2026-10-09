# C1 — CapabilityProvider 合同（能力提供者）

> anet4 五合同之一（总纲见 docs/CONTRACTS-zh.md）。Go 包：`provider/`。本文按 anet 0.3.0 与
> ANetLink 0.2.0 的 C1 v1 写成。

## 一句话

**daemon 获得可调用能力的唯一门。** 原生 agent、外部 agent 连接器、物理世界
runtime（ANetLink）都从这一个接口进来。

## 红线（org 教训）

daemon 不得知道 provider 背后是什么——尤其**不得知道"设备"这个概念**。
provider 声明能力、可被调用、返回效果，仅此三件事过界。能力 id（ANetLink 的形如
`<能力>@<适配器>/<设备>`）在 daemon 里是不透明的字符串。

## 形态

进程内：实现 `provider.CapabilityProvider`，`Registry.Register` 注册。
进程外（独立部署的 anetlinkd）：daemon 侧的客户端 `provider/anetlink` 实现同一接口，
内部说 C1 v1（下一节）——对 daemon 其余代码零差别。效果类型来自 ANetCore
（`effect.Effect`、`effect.Evidence`、`tsir.EffectRecord`），杜绝跨仓类型漂移。

## 语义要点

- `Invoke` 返回 `effect.Effect`：**UNVERIFIED 不是失败**——"命令已送达但物理
  效果在当前信任级不可验证"与 FAILED 严格区分（信任记账依赖这个区分）。
- 调用已发出、答复丢失或超时：`provider.OutcomeUnknownError`，任务以 failed、效果
  UNVERIFIED 结束（"不知道是否生效"），绝不报成 FAILED。
- 对端拒绝或失败、能用用户的话说出原因时：`provider.RefusalError{Reason}`，`Reason` 是原因表
  里的用户码（如 `provider_policy`、`capability_not_served`、`args_too_large`、`unavailable`、
  `backend_failed`）。任务按该码那一行结束：对端肯定没有执行的（R 类）为 rejected、效果
  UNAVAILABLE；其余为 failed。请求方只收到码，句子由它自己的节点渲染；对端的原话只进本机日志。
- `Call.VerifiedCaller()` 是 provider 唯一可以转交出去的调用方：只有经中继、由 daemon
  核实过的调用才有值，其他入口一律为空。provider 不读 `Call.CallerAID`。
- 可选接口 `provider.RemotePolicy`：由对端决定哪些能力允许远端调用（ANetLink 的
  `c1.remote`）。daemon 只把允许远端调用的公开能力写进卡片与 hub 注册，其余公开能力不发布，
  `anet doctor` 的 `anetlink.publish` 逐个说明。
- 能力解析：精确匹配优先，然后按 `.` 边界向上走（注册 `light` 可服务
  `light.onoff`；`lightning` 永不匹配 `light`）。
- 注册原子性：任何冲突（重复 ID / 能力已被他人持有）导致整次注册不生效。
- `Describe` 返回 CAS 描述对象的 CID，daemon 原样挂上卡片，不解释内容。

## C1 v1：daemon 与 anetlinkd 之间

### 传输

- Unix socket 上的 HTTP/1.1，请求头 `Host: anetlink`。请求与响应体只用 CoreDet-CBOR
  （`Content-Type: application/cbor`），没有 JSON 形态。
- socket 缺省为 `$XDG_RUNTIME_DIR/anetlink/c1.sock`，两边相同，都不配置也能连上。
- daemon 每次连接前先核实路径（路径上每一级只能由可信账户改动），连上后由内核读出监听者账户，
  必须是期望的账户（缺省为 socket 文件的属主）；不符时什么都不发送。

| 方法与路径 | 成功 | 说明 |
|---|---|---|
| `GET /v1/health` | 204 | |
| `GET /v1/capabilities` | 200 `CapList` | |
| `GET /v1/describe` | 200 `Describe` | |
| `POST /v1/invoke` | 200 `InvokeResp` | 拒绝时 4xx/5xx `Refusal` |
| 任何 `/v0/*` | 426 `Refusal{c1_version}` | 旧客户端明确失败 |
| 其他 | 404 `Refusal{not_found}` | |

请求体 ≤ 1 MiB（超出 413 `too_large`；daemon 发现参数超限时不发送，直接以 `args_too_large`
结束任务）；非 CBOR 为 415 `unsupported_media`；daemon 读响应体同样 ≤ 1 MiB。设备离线仍是
200 `InvokeResp{status: UNAVAILABLE}`。

### 对象（整数键）

| 对象 | 键 | 名 | 类型 | 必选 |
|---|---|---|---|---|
| CapList | 1 | `caps` | [CapEntry] | 必 |
| CapEntry | 1 | `id` | tstr | 必 |
| | 2 | `remote` | uint：0 = 不接受远端调用；1 = 接受 | 必 |
| Describe | 1 | `cid` | tstr，空串表示没有描述对象 | 必 |
| InvokeReq | 1 | `capability` | tstr | 必 |
| | 2 | `args` | map | 可 |
| | 3 | `call_id` | tstr | 可 |
| | 4 | —— | 退役（C1 v0 的 `caller_aid`），出现即拒收 | |
| | 5 | `caller` | Caller | 必 |
| Caller | 1 | `via` | uint：0 = 本机；1 = 中继；2 退役 | 必 |
| | 2 | `aid` | tstr：`via = 1` 时必带且非空，`via = 0` 时禁带 | 条件 |
| | 3 | —— | 保留，出现即拒收 | |
| InvokeResp | 1 | `status` | tstr（效果状态） | 必 |
| | 2 | `metrics` | map | 可 |
| | 3 | `message` | tstr | 可 |
| | 4 | `evidence` | Evidence（键 1–8：`requested`、`protocol`、`native_ack`、`observed_state`、`latency_ms`、`verify_trust`、`auth_trust`、`quirk`） | 可 |
| Refusal | 1 | `reason` | tstr（下表） | 必 |

两端都严格解码：未登记的键、退役与保留的键、重复的映射键、CBOR 标签、非法 UTF-8 文本、对象后
多余的字节一律拒收；必选成员、`via` 的取值与 `via`/`aid` 的搭配在同一个入口里接着检查。

### 调用方

- 经中继到达的委派：`caller = {via: 1, aid: <daemon 核实过的请求方 AID>}`。daemon 只为通过了
  完整接收流水线（含见证通告核验）的消息发起这种调用，所以 ANetLink 不必再问调用方的见证是否新鲜。
- 本机入口（CLI、MCP、控制面）：`caller = {via: 0}`，不带 `aid`。
- 其他任何入口不拨号，任务以 `backend_failed` 结束；退役的 `via = 2` 永远不会发出。
- 远端调用能否执行由 ANetLink 判定：只有匹配 `c1.remote` 的能力接受 `via = 1`，缺省一个都不接受。

### 拒绝码与用户码

C1 的码是 ANetLink 的命名空间，不直接给人看；daemon 映射为自己的用户码：

| C1 码（HTTP） | 用户码 | 任务 |
|---|---|---|
| `not_permitted`（403） | `provider_policy` | rejected |
| `unknown_capability`（404，含无法对应任何设备的畸形能力 id）、`not_found`（404） | `capability_not_served` | rejected |
| `too_large`（413） | `args_too_large` | rejected |
| `bad_request`（400）、`unsupported_media`（415）、`internal`（500） | `backend_failed` | failed |
| `c1_version`（426） | `backend_failed`；`anet doctor` 的 `anetlink.socket` 报 fail | failed |

另外：连不上 anetlinkd、或监听者没有通过核实时，什么都没有发出，任务以 `unavailable` 拒绝，
可稍后重发；不是 C1 v1 的答复（类型、码与状态不配套、无法解码）一律按 `backend_failed`。

### 文件不经 C1

C1 只传参数与效果。声明了 `files` 的公开能力只能由 service 模块提供（`anet doctor` 的
`capabilities.files`）。

### daemon 侧配置（`config.json` 的 `modules.anetlink`）

| 键 | 缺省 | 说明 |
|---|---|---|
| `socket` | `$XDG_RUNTIME_DIR/anetlink/c1.sock` | 没有 `XDG_RUNTIME_DIR` 时必填 |
| `expected_user` | 不设（socket 文件的属主） | anetlinkd 以另一账户运行时填写；接受用户名或数字 uid |
| `socket_group` | 不设 | 允许写 socket 所在目录的组 |
| `peer_check` | `""`（严格） | `"path-only"` 只在读不到对端账户的平台上使用，doctor 会警告 |
| `allow_tcp` | 不设 | 不得为 `true`：C1 只经 Unix socket |

`"anetlink": {}` 就是完整配置。注册握手的时限是常量 10 秒；`timeout_s`、`expected_uid` 与旧的
`providers.anetlink` 已删除，出现在配置里时 daemon 拒绝启动并说明替代写法。

### 金标

`provider/anetlink/testdata/c1v1/` 与 ANetLink 的 `northbound/c1serv/testdata/c1v1/` 是逐字节
相同的一组 `.cbor` 文件及 `SHA256SUMS`；`ANETLINK_REF` 记录它们来自 ANetLink 的哪个发布。两仓各用
自己依赖的 ANetCore 编码正例并与文件逐字节比较、解码正例得到同一个值、拒收全部负例；CI 作业
`c1-golden-xrepo` 按 `ANETLINK_REF` 检出 ANetLink，逐文件比较两份。任何一边的差异都是线协变更，
要两仓一起改，不能拿一边的文件覆盖另一边。
