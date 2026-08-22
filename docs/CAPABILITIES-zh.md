# anet daemon 功能清单与完备程度

对照的是**当前仓库里跑得起来的东西**,不是设计意图。每一项的"完备程度"
按四档给,判据写在后面:

| 档位 | 判据 |
|---|---|
| **联调验证** | 有单测,且在 `scripts/joint.sh` 里跨进程真的跑通 |
| **单测覆盖** | 有针对性的单测(含否定用例),但没进联调 |
| **仅可用** | 代码在跑,没有直接测试,靠上层间接覆盖 |
| **未做** | 不存在,或只有占位 |

**当前规模**:约 11,000 行实现 / 5,400 行测试(三个 wire 包已移入 ANetCore),143 个测试 + 13 个基准,
`go.sum` 82 条,race 干净。联调 20/20。

---

## 一、内核(不可插拔)

内核是"拔掉就不是 anet daemon"的部分,没有构建标签。

| 能力 | 位置 | 完备程度 | 说明 |
|---|---|---|---|
| 身份(KEL/AID) | ANetCore `identity` | 联调验证 | 一个 runtime = 一个 agent = 一个 AID |
| 能力注册表 (C1) | `provider/` | 联调验证 | daemon 只认 provider/capability/evidence,**不认"设备"** |
| 委派生命周期 | `internal/daemon/relay.go` `delegation.go` | 联调验证 | find → delegate → 多轮消息 → end → review |
| 委派验签 | ANetCore `delegation` | 单测覆盖 | 8 种冒充方式逐一被拒(换 KEL、冒名、改任务、无签名、缺 ix、坏 TaskDoc、空任务、坏 KEL) |
| **收据验证** | ANetCore `delegation.VerifyResult` | 联调验证 | 请求方接受结果前逐项绑定;7 种"持有效签名仍撒谎"的方式被拒 |
| **第三方验证** | `anet verify` | 联调验证 | 无 daemon、无 hub、无网络;实测通过与拒绝各一次 |
| 中继鉴权 | ANetCore `relayauth` | 单测覆盖 | 四种 action 域分离、5 分钟重放窗口、preimage 字节钉死 |
| 证据链 (P6/C5) | `internal/daemon/ledger.go` | 联调验证 | base64 CoreDet-CBOR、verify-before-use、88 条记录重启通过 |
| 收据 + 评价 | `internal/protocol/evidence` | 联调验证 | ResultCID 覆盖交付物,provider 签发 |
| 传输列表 | `internal/daemon/transport.go` | 联调验证 | hub 是兜底,不是唯一;dispatch 513ns |
| 对端 KEL 缓存 | `internal/daemon/peerkel.go` | 单测覆盖 | 只记住自己验过的,上限 512 |
| 控制面 (20 端点) | `internal/daemon/control_api.go` | 联调验证 | bearer + loopback;console 单独放在 bearer 外 |
| CLI (33 子命令) | `cmd/anet` | 单测覆盖 | 参数解析刚补测试并修了 `--key=value` |
| 自动回复 | `internal/daemon/autoreply*.go` | 单测覆盖 | 含 vision;未进联调 |
| 身份管理(多身份) | `internal/daemon/identities.go` | 仅可用 | `anet id ls/new/use/rm` |

## 二、可插拔模块

`-tags no_<name>` 后二进制里**符号数为 0**,CI 用 `go tool nm` 计数,不是"配置关掉"。

| 模块 | 能力 id | 行数 | 测试 | 完备程度 |
|---|---|---|---|---|
| `anetlink` | 由 ANetLink 提供(`ptz.absolute@onvif/camera-006` 等) | 81+168 | 3 | 联调验证 |
| `cas` | `cas.put` `cas.get` `cas.has` `cas.stat` | 477 | 19+4基准 | 联调验证 |
| `blackboard` | `blackboard.add` `blackboard.snapshot` `blackboard.conclude` | 772 | 16+3基准 | 联调验证 |
| `org` | `org.verify` `org.info` | 737 | 21+3基准 | 联调验证 |
| `p2p` | (传输,不是能力) | 416 | 8 | 联调验证 |
| `inv1` | (不变式守卫) | 92 | 1 | 单测覆盖 |
| `inv2` | (不变式守卫) | 102 | 3 | 联调验证 |

### 性能实测(Xeon E5-2603 v4 @1.70GHz)

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

## 三、联调覆盖(`scripts/joint.sh`,四仓六进程)

```
ANetMock ← ANetLink ← anet daemon(provider) → ANetHub ← anet daemon(requester)
                            ↕                              ↕
                        anetpeer ←─── rendezvous ───→ anetpeer
```

20 项检查:设备 PTZ 真实移动 + 携带 provenance;CAS 按内容寻址往返 +
未知 CID 诚实失败;黑板签名合并 + 快照 + 幂等 + 篡改被拒;org 凭证验证 +
过期被拒 + INV-2 拒绝泄露;p2p 双向直连;证据链重启。

**联调是唯一发现这五个缺陷的手段**——两边单测各自伪造对方,全绿:

1. 能力委派路径无人可达(`--capability` 落进 goal 文本)
2. 读能力跨线只写不读(CID/blob/列表放在 `ObservedState`,不上线)
3. ANetLink 的 C1 线丢弃 quirk 标签
4. 证据链用无法表达自身 id 的编码落盘
5. p2p 传输送不回一个回复(入站处理占住读循环)

## 四、已知缺口(按重要性)

| 缺口 | 影响 | 状态 |
|---|---|---|
| ~~三个 wire 包各有两份副本~~ | ~~`delegation` 已实质分叉 28 行~~ | **已解决**:三个包收进 ANetCore v0.5.3,两仓共用一份 |
| 治理纪元 `govepoch` | org 只接受 epoch 0 | 等 ANetCore 的 `ascpevo.GovernanceCert` |
| C5 证据链无查询接口 | 链只写不读,运维看不到自己的证据 | 未做(`anet verify` 已让收据可查,链本身仍不可查) |
| `internal/hubapi` 无测试 | 纯类型,风险低 | 仅可用 |
| `module/anetlink` 无直接测试 | 81 行薄封装,`provider/anetlink` 有 3 个测试 | 仅可用 |
| 自动回复未进联调 | 只在单测里跑过 | 单测覆盖 |
| `anetfixture` 无单测 | 是联调工具本身,由 joint.sh 端到端使用 | 仅可用 |

## 五、刻意不做

- **libp2p commons 织物**(anet3 的 2,352 行):进程外。构建标签移除代码,
  从不移除 `go.mod` 依赖;82 条 go.sum 对上千模块树不值。
  `tools/anetpeer` 是那份契约的参考实现(302 行,3 个跨真实传输的测试)。
- **org-central 及六个卫星**(anet3 的 5,033 行):`module/org` 只回答
  "这份凭证是不是这个组织的有效成员"。看板、任务循环、群密钥若要存在,
  是 org-central 自己通过 C1 提供能力,不是 daemon 里多七个包。
- 细节见 [REWRITE-from-anet3-zh.md](REWRITE-from-anet3-zh.md)。
