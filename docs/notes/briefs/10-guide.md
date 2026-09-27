简报 · 实现者指南(代码库约定与测试夹具) · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:不对照某一节,而是给之后所有实现 agent 的"在这三个仓库里干活"说明。内容包括包结构、daemon 生命周期与 `module.Host`、`interactions.db` 迁移写法、daemon/hub 测试夹具、金标向量、代码与提交风格、构建 tag、mutation 验证,以及 C1/C2/C4 引入了哪些接口。
行号以本分支 HEAD 为准(ANetCore `b4d069f` / ANet `6295665` / ANetHub `fa98276`)。
本次实测:三仓 `go test ./...` 全绿(daemon 包不走缓存约 28 s);`go vet ./...`、`go vet -tags shell ./...` 干净;`gofmt -l .` 三仓都为空;ANet 若干减法 tag 组合 `go build` 通过;**ANet `go test -race ./internal/daemon/` 失败**(见 §0 第 1 条);ANetHub 与 ANetCore 的 `-race` 通过。

---

## 0. 动手前必读(陷阱清单)

1. **daemon 包在 `-race` 下是红的。** `TestTheKeyRingRotatesAndRetires`(`enckeys_test.go:67`)在 `New` 已经启动 `outboxLoop` 之后才写 `d.clock`,而 `outboxLoop` 经 `nowMS()`(`seal_send.go:292`)读这个字段,构成数据竞争。一共有 19 处测试在 `New` 之后直接改 `d.clock` 或 `d.rxFault`,都有同样的隐患。ANet CI 的 `race` job 会因此失败。修法:把这两个钩子改成 `atomic.Pointer[func…]`,或加一个 `setClock` 并在测试里统一用它。新写的测试不要再照这种方式直接赋值。
2. **离开 go.work 就编不过。** ANet 与 ANetHub 的 `go.mod:6` 钉的是 `ANetCore v0.14.0`,这个版本里没有 `seal`(实测 `GOWORK=off go build ./...` 报 `does not contain package …/seal`)。两仓 CI 只认 `go.mod`,所以在本分支上 CI 必红。在 ANetCore 打出 v0.15.0 tag、两仓 go.mod 随之升级之前:
   - 一律先 `source /data/projs/anet-dev/.anet-env.sh`(在 worktree 里则 source `wt/<id>/env.sh`,见 §1);
   - **不要在 ANet/ANetHub 里跑 `go mod tidy`**,它会忽略 go.work,去 v0.14.0 里解析 `seal`,然后失败;
   - 加依赖用 `go get <mod>@<ver>`。例如 `github.com/a2aproject/a2a-go/v2@v2.6.0` 已经在模块缓存里,`wt/a2ashape` 就是这样加的。
3. **`Module.Stop` 在正常关停时从来不被调用。** `Daemon.Close()`(`daemon.go:327`)的顺序是 cancel `d.ctx` → 等待 `bgWG` → 等待长调用 → 关 ledger → 关 ix,中间没有 `stopModules`。`stopModules` 只在 `validate` 失败时调用(`daemon.go:221`),这个问题 main 上就已存在。模块的监听与 goroutine 必须挂在 `Start(ctx, …)` 传进来的 `ctx` 上,也就是 `d.ctx`。另外 `serveModuleFaces`(`paylink.go:366`)只调用 `Payer.Serve`。D1 的 `module/a2a` 需要自己的监听生命周期,要么扩展这个接缝,要么在 `Start` 内监听并用 ctx 收尾。
4. **`module.Host` 每加一个方法,就要同步改 8 个测试桩。** 它们分别是 `module/{blackboard,cas,org,service,shell,taskboard,x402}/*_test.go` 与 `tools/anetpeer/peer_test.go` 里实现 Host 的桩(搜 `DeclareUntrustedBackend()` 就能全部找到;`shell` 的桩带 `//go:build shell`,只有 `-tags shell` 才会编译)。现在 `wt/a2ashape`(加 `StateDir`、`TaskSeam`)和 `wt/x402d`(加 `AdmitSpend`,并改 `Payer.Authorize/Settle` 签名)同时在改 Host,合并时这 8 个文件会逐个冲突。
5. **fake hub 和真 hub 不一样的地方**(改 hub 端点时,必须在同一次改动里同步改 `hubfake_test.go`):
   - fake 的 `GET /agents/{aid}/balance|ledger`(`hubfake_test.go:1113/1124`)不验签,真 hub 要求本人签名;
   - fake 没有 `/x402/verify`、`/x402/supported`、`/agents/{aid}/card`、`/agents/{aid}/redemptions`、`/fed/*`;
   - `hSettle` 不核对 `paymentRequirements`;
   - `fakeHub.peers`(联邦查钥)没有任何测试用到。
6. **"全绿"里有被跳过的用例。** 本机没有 `bwrap`,所以沙箱集成测试 `sandbox_linux_test.go:219` 被跳过;没有 headless Chromium,`console_test.go:433` 也被跳过。凡是改了沙箱或控制台页面,要在有 `bwrap`/Chromium 的机器上跑,或者设置 `ANET_TEST_CHROME`。
7. **联调脚本没法在这里原样跑。** `scripts/joint.sh` 依赖 ANetLink/ANetMock 两个仓库,本工作区里没有;wire 2 之后三仓也没跑过任何一个联调脚本(0013 的说法)。能在本机跑的只有 `scenario.sh` 的默认层(不接外网、只用回环)和 `joint-fleet.sh`,它们会自己构建 anet 与 anet-hub。
8. **`d.clock` 与 `ix.SetClock` 是两个互不相干的时钟。** 前者是 uint64 毫秒,用于线协、keyring、outbox、pending;后者是 int64 毫秒,只用于 `state_at`。测试如果要固定时间,两个都得设。

---

## 1. 工作区与环境

- 主检出:`/data/projs/anet-dev/{ANetCore,ANet,ANetHub}`,分支都是 `a2a-redesign-wip`。env 文件是 `/data/projs/anet-dev/.anet-env.sh`,它设置 `GOWORK=/data/projs/anet-dev/.anet-work/go.work`(`use ../ANetCore ../ANet ../ANetHub`)、`GOTOOLCHAIN=auto`、`GOPROXY=proxy.golang.org`。Go 版本 1.26.6,本机有 gcc,可以跑 `-race`。
- 并行 worktree:`/data/projs/anet-dev/mkwt.sh <id>` 为三仓各建一个 `wt/<id>/<repo>`(分支 `wp/<id>`,基于 `a2a-redesign-wip`),并生成各自的 `go.work` 与 `env.sh`。**在 worktree 里必须 source `wt/<id>/env.sh`。** 如果误用了主 env,go.work 的 use 列表里没有当前目录,go 命令会直接报错。
- 参考材料:A2A 规范在 `/data/projs/anet-dev/a2a`;`Refs/a2a-go`(v2.6.0 源码)和 `Refs/a2a-x402/spec/v0.2/spec.md`。
- **仓库里提交了二进制**:`ANet/anetfixture`、`ANet/anetpeer`、`ANetHub/anet-hub`、`ANetHub/anet-hub-admin` 都被 git 跟踪,WIP 提交还更新过前两个。`ANetHub/scripts/build.sh` 默认输出到 `./anet-hub`,构建一次就会让工作树变脏。提交前用 `git status` 检查,不要把重新构建的二进制带进提交。
- 耗时参考:daemon 包约 28 s;`-race` 下 daemon 包 237 s,hub `internal/aghub` 113 s。

---

## 2. 三仓包结构与职责

### ANetCore(纯协议逻辑,无 I/O)
规则写在 `docs/scope.md`:只放确定性、无 I/O、至少两个应用都要用的代码,或者被金标钉住的代码。外部依赖冻结为四族(cbor、edwards25519、x/crypto、x/text),标准库新用到的包要登记在 scope.md(v0.15.0 这一段已登记 `crypto/hpke`、`a2acard`)。**不得引入 a2a-go。**

| 包 | 职责(本分支新增或改动的标 ★) |
|---|---|
| `coredet` `anetcid` `aobj` | 确定性 CBOR、CID、分离签名 |
| `identity` | KEL/AID;★`ExtendsKEL(old, next []SignedEvent) error`(`identity/extend.go:39`),★`Replay` 的 `SupersededAt` 修正 |
| `seal` ★ | HPKE 封装信封、`EncKeySet`、高水位(见 §11) |
| `a2acard` ★ | A2A AgentCard 的 RFC 8785 JCS + JWS EdDSA,只用标准库;**默认值剥离缺陷未修**(0012) |
| `relayauth` | ★v2:`PreimageV2(action, aid, hubAID string, ts uint64, method, pathAndQuery string, body []byte) []byte`、`EncodeSig`/`DecodeSig`、`Header*`;v1 的 `Preimage` 已标 Deprecated,但还有人在用(§13) |
| `delegation` | 线协正文;★`StatusMsg`、`ContextID`(7)/`Metadata`(8)、`ResultResp.Metadata`(5)、`ChatMsg.Metadata`(10)、`KindCancel`、`VerifyDelegateReqWithKEL`、`VerifyResultWithKEL` |
| `payment` | x402 线协对象 + `anet-credit` 方案 |
| `golden` | 跨实现金标(§7) |
| `evidence` `ael` `effect` `tsir` `adp` `agenturi` | 回执/评价、事件链、效果、TaskDoc、旧 ADP 卡、URI |

### ANet(daemon + CLI + 模块)
| 目录 | 职责 |
|---|---|
| `cmd/anet` | CLI 分发(`main.go`)、模块注册文件 `module_<name>.go`(每个带 tag,只做 blank import)、`optin_tags.go`(`DeclareOptIn("shell")`)、`mcp.go`/`mcp_stub.go`、`peers.go`(★)、`verify.go` |
| `internal/daemon` | 内核,77 个文件。按主题划分:线协收发(`seal_send`、`receive`、`retry`、`transport`、`enckeys`、`peerkel`、`hub_client`)、任务(`delegation`、`capability`、`relay`、`eventbus`、`pending`)、入站策略(`inbound`、`inbound_api`)、控制面(`control_api`、`ctlsec`、`session`、`console`、`attachments`)、自动回复与沙箱(`autoreply*`、`sandbox_*`)、支付接缝(`paylink`)、证据(`ledger`)、配置与路径(`config`、`paths`) |
| `internal/runtime/interactions` | SQLite 存储 `interactions.db`:`interactions.go`(交互/消息/附件)、`peers.go`(★replay、peer_identity、`Update/Tx`)、`outbox.go`★、`pending.go`★ |
| `internal/hubapi` | 与 hub 共享的 JSON 线协类型和常量(`WireVersion=2`、`Header*`),字段名由 `hubapi_test.go` 钉住。**包注释里说 hub"闭源",这已经过时** |
| `internal/transcript` | ★对话记录 v2(`EncodeV2(nonce, msgs)`、`Parse`) |
| `internal/mcpserv` | MCP 北向:短生命周期的 stdio 进程,通过 `Control.Call(ctx, path, body, out)` 调用控制面 |
| `internal/golden` | 钉 org/CogUnit/credential/blob 的 CID |
| `module` | `module.go`(Host 等接口,§4)、`transport.go`(Transport/Inbound/TransportHost) |
| `module/<name>` | 可选模块:p2p、x402、blackboard、cas、org、service、anetlink、taskboard,以及加法 tag 的 shell;`inv1`/`inv2` 是内核用的发布守卫 |
| `provider` | C1 能力注册表;★`Call.Via`(`ViaRelay`/`ViaVoucher`) |
| `tools/anetfixture` `tools/anetpeer` | 联调用的签名夹具与 p2p 对端进程(§6.5) |
| `scripts/` | 联调与实网脚本,公共函数在 `lib.sh` |

### ANetHub
| 目录 | 职责 |
|---|---|
| `cmd/anet-hub` | 入口;`mounts.go`(`hubDeps`/`mount`,可选模块在这里接线)、`optin_tags.go`(`optInMounts = {"taskboard"}`)、`wire_federation.go`(`!no_federation`)、`wire_taskboard.go`(`taskboard`) |
| `cmd/anet-hub-admin` `internal/admin` | 运营面(第二个二进制) |
| `internal/aghub` | 内核:`server.go`(路由,约第 257 行起)、`auth2.go`(relayauth v2)、`relay.go`、`keys.go`、`limits.go`、`facilitator.go`、`gateway.go`、`card.go`、`a2acard.go`(只把 A2A 卡片原样存下,**不验证**)、`content_v2.go`(内容清除迁移) |
| `internal/federation` | `/fed/v1/*`、`/fed/v2/keys/{aid}`;跨模块用的哨兵错误放在 `internal/seamerr`,避免 `no_federation` 构建把联邦符号链接进来 |
| `internal/hubid` | hub 自身身份与 `/hub/identity` |
| `internal/taskboard` | 加法 tag |
| `webui/` | React/TS,经 `scripts/build.sh` 构建后嵌入 `internal/aghub/web/index.html` |

依赖方向:应用 → ANetCore,反向不允许,应用之间也不允许。hub 内核与 admin 不得依赖 `ANetCore/delegation`、`ANetCore/tsir`,由 `internal/aghub/importguard_test.go` 用 `go list -deps` 强制。

---

## 3. daemon 生命周期

`daemon.New(layout)`(`daemon.go:164`)的顺序:
1. `EnsureRoot` → `LoadConfig`:迁移旧的 `accept_delegations`,标记 `migratedInbound`/`rewriteConfig` → `LoadOrGenerateIdentity` → `interactions.Open(layout.InteractionsDir())`。
2. 组装 `Daemon`,创建 `ctx`/`cancel`;如需要,`SaveConfig` 写回迁移后的配置;`notices.configure`。
3. `setupKeyRing()`(`enckeys.go:319`,读写 `enc_keys.cbor`)→ `openEvidenceLedger` → `purgeReplay` → `goBackground(receiveMaintenance)` → `loadCardSeq` → `provider.NewRegistry()`。
4. `startModules(ctx, cfg)`(`modules.go:26`):`module.Build(raw)` → 对每个模块调用 `m.Start(ctx, moduleHost{d})`;实现了 `module.Payer` 的模块成为 `d.pay`。
5. `validate(cfg)`(`inbound.go:251`)**放在模块启动之后**,因为模块会在 `Start` 里调用 `DeclareUntrustedBackend`。
6. `recoverInterrupted()` → `goBackground(outboxLoop)` → 如果配置了 hub:`startRelayLoop` + `refreshRegistration` → 如果配置了自动回复:`startAutoReply`。

`ServeControl(ctx)`(`control_api.go:291`)依次做这些事:取控制令牌、监听回环地址、写 daemon pointer 与注册表、`serveModuleFaces`、启动 http.Server;在 ctx 结束或 `RequestStop()` 时退出。`cmd/anet/main.go:351 runDaemon` 负责 `New` → `defer Close` → `ServeControl`。

`Close()`(`daemon.go:327`)的顺序:cancel → `bgWG.Wait()` → 最多等 10 s 让长调用排空 → 关 ledger → 关 ix(没排空时不把 `d.ix` 置 nil)。

需要遵守的规则:
- 凡是要用存储的后台 goroutine,一律通过 `d.goBackground(f)` 启动(`daemon.go:305`)。Close 之后它会拒绝启动新任务。
- 长调用用 `longCalls` 限流、`longCallsWG` 计数;关停时会取消它们,已知的 SI-10 缺陷见 01 简报 G5。
- `New` 的错误路径会泄漏:ledger 打开失败时不 cancel、不关 ix;`startModules` 失败时不关 ix,也不停已经启动的模块(`daemon.go:204-215`)。在 daemon 进程里无害,但在测试里会泄漏句柄。

---

## 4. `module.Host` 接口全貌(`module/module.go`)

| 方法 | 授予什么 | 内核实现 | 现在谁用 |
|---|---|---|---|
| `AID() string` | 本节点 AID | `modules.go:151` | 各模块 |
| `Providers() *provider.Registry` | 注册能力 | `modules.go:152` | 各模块 |
| `RecordEvidence(eventType string, payload any) error` | 往本节点证据链追加 | `modules.go:193` | 各模块 |
| `ResolveKEL(aid string) ([]identity.SignedEvent, bool)` | 已验证过的对端 KEL,自己的也能查;查到的行会被 pin 为 `issuer` | `modules.go:178` | blackboard、org |
| `PaymentSeam() (PaymentSeam, bool)` | Sign + HubIdentity + HubURL + ReadEvidence | `paylink.go:113` | x402 |
| `HubSeam() (HubSeam, bool)` | Sign + HubURL | `paylink.go:129` | taskboard |
| ★`Admit(callerAID, capID string, argsLen int) (release func(), refusal string)` | 内核准入(deny/public/配额/在飞/参数大小) | `inbound.go:546` | x402 兑付口 |
| ★`DeclareUntrustedBackend()` | 声明"会把非信任对端的工作转给本地后端",用于 §5.1 的冲突校验 | `inbound.go:552` | 暂无(留给 module/a2a) |

- `TransportHost = Host + Inbound() Inbound + RegisterTransport(Transport)`(`module/transport.go:65`,实现在 `transport.go:198/201`)。p2p 模块对 Host 做类型断言,得到的就是这个接口。
- 可选接口:`Confidential{ForbiddenTokens() []string}` 由 `screenPublication` 使用;`Payer`(`module.go:235`)由 `startModules` 做类型断言。设计 §10.4 的 `CardContributor` 还没有实现,`wt/cardgen` 正在做。
- 注册相关:`Register(name, Factory)` 在模块包的 `init()` 里调用,重名会 panic;`Factory` 返回 `(nil, nil)` 表示"编译进来了但没配置"。`Build(cfg)` 对"配置里有、却没编译进来"的名字分三种情况报错;`BuildOne(name, cfg)`;`DeclareCompiled`(mcp)与 `DeclareOptIn`(shell)用于 `anet version` 的输出。
- 设计里待加的方法:§11.1 的 `StateDir(module string) string`、`TaskSeam() (TaskSeam, bool)`,§8.6 的 `AdmitSpend`。

**给 Host 加方法的做法:**
1. 在 `module.go` 的接口上写清为什么加。这是仓库约定,现有每个方法都附了一段"为什么值得扩大这个接口"。
2. 在 `internal/daemon` 里给 `moduleHost` 实现它。`moduleHost` 是值接收者 `struct{ d *Daemon }`,末尾有 `var _ module.Host = moduleHost{}` 做编译期检查。
3. 同步改 8 个测试桩(§0 第 4 条)。建议顺手新建 `module/moduletest` 包,放一个 `NopHost`,让各桩嵌入它,以后加方法只改一处。
4. 用 `-tags shell` 再跑一次 vet/test,否则 shell 的桩根本不参与编译。

**新增模块的做法**(以 `module/a2a` 为例):
- 包内文件加 `//go:build !no_a2a`,`init()` 里调用 `module.Register("a2a", New)`;
- 新建 `cmd/anet/module_a2a.go`(`//go:build !no_a2a`),只做 blank import;
- 在 CI 的 pluggable 矩阵里加一行并写好符号模式。设计 §16 规定 `no_a2a` 的模式是 `module/a2a|a2aproject/a2a-go`,另外还要检查 `go list -deps ./internal/mcpserv ./internal/daemon | grep a2aproject` 为空;
- 模块单测用桩 Host。

**daemon 测试怎么接入真模块**:参考 `internal/daemon/pay_test.go`。给测试文件加 `//go:build !no_<m>`,并 import 模块包。`init()` 完成注册后,这个测试二进制里的每一个 daemon 都会去 `Build` 这个模块。陷阱在于:像 x402 那样"不配置也会启动"的模块,会在**每一个** `newTestDaemon` 里启动;`module/a2a` 按设计也是缺省启用,所以谁要是在 daemon 测试里 import 了它,所有测试 daemon 都会去绑 A2A 端口。需要带配置时,`newTestDaemon` 不接受配置,得另写一个能往 `config.json` 写 `"modules"` 块的构造函数。

---

## 5. `interactions.db` 的 schema 与迁移写法

- `Open(dir)`(`interactions.go:231`)的 DSN pragma 是 `journal_mode(WAL)&synchronous(NORMAL)&busy_timeout(15000)`。写操作由 `Store.mu` 串行化;读不加锁。
- **不使用 `user_version`,迁移全部是幂等的"探测式"迁移**,每次 `Open` 都会从头跑一遍 `migrate()`(`interactions.go:291`):
  1. `CREATE TABLE/INDEX IF NOT EXISTS`;
  2. `addColumn(db, table, name, decl)`:ALTER ADD COLUMN,遇到 "duplicate column" 就忽略;
  3. 结构性迁移用 `hasColumn` 探测旧列,在事务里改写,再 DROP 旧列,参考 `migrateStatusToState`(`:425`);
  4. 各子系统自己的表放在各自文件里,由 `migrate()` 末尾依次调用:`migrateWire2`(peers.go:333)、`migratePending`(pending.go:66)、`migrateOutbox`(outbox.go:34)。
- **给 interaction 表加一列要改 6 处**:`interactionAdded`(`:266`)→ `ixColumns`(`:707`,注释写着要与 scanRows 同步)→ `scanRows`(`:1101`)→ `Interaction` 结构体 → 需要在创建时写入的,还要改 `New` 结构体与 `create()` 的 INSERT(`:534`)→ 需要事务内写入的,在 `peers.go` 给 `Tx` 加对应的包装方法。列一律写成 `NOT NULL DEFAULT …` 或可空 BLOB,保证旧行可读。
- **新表**:写一个 `migrateXxx()`,只用 `CREATE … IF NOT EXISTS`,在 `migrate()` 里调用;凡是需要与接收流水线同事务写入的数据,都必须放在 `interactions.db` 里(§3.6 第 10 步)。
- **事务**:`Store.Update(func(*Tx) error)`(`peers.go:51`)。fn 里只允许操作数据库,不能联网、签名或等待,因为整个 fn 执行期间都持有写锁。接收路径通过 `d.commitRx(m, fn)`(`receive.go:656`)进入,它在业务写入之后调用 `rxFault` 测试钩子。
- **状态写入**只能走 `setState`/`finish`,它们是带守卫的 UPDATE,拒绝离开终态,并把 `state_seq+1`;不要手写 `UPDATE interaction SET state=`。
- 支付列 `pay_state/pay_required/pay_auth_ids/pay_payload/pay_receipts/quote_expires_at` 已经建好,也会被读(`delegation.go:345/633/1223`),**但还没有任何写入方**,留给 C3(`wt/x402d` 正在加 `interactions/payment.go`)。
- **迁移测试的写法**,参考 `state_test.go:179 TestStatusColumnMigratesToState`:先用裸 `sql.Open("sqlite", …)` 按旧 schema 建表、插入数据,然后 `interactions.Open` 触发迁移,最后断言结果,并用 `pragma_table_info`/`sqlite_master` 确认旧列和旧索引已经删掉。
- ANetHub 的 `aghub.Store.migrate()`(`aghub.go:189`)是同一种风格:ALTER 失败时忽略 "duplicate column name",用 `tableColumns` 探测结构,子迁移为 `migrateRelayV2/ContentV2/Settlement/Gateway`;hub 的 DSN 额外带 `secure_delete(ON)`。

---

## 6. 测试夹具

### 6.1 daemon 包(`internal/daemon`,package daemon,白盒测试)
- `main_test.go`:把 `XDG_CACHE_HOME` 指向临时目录;如果参数里带 `sandboxProbeMarker`,就执行沙箱探针,不跑测试。
- `newTestDaemon(t, hubURL string, accept bool) *Daemon`(`relay_test.go:32`):数据目录放在 `t.TempDir()`,只写 `control_addr=127.0.0.1:0` 与 `hub_url`,然后 `New`,**停掉 relay 轮询循环**(测试里手动调用 `d.pollOnce(ctx)`),并注册 `t.Cleanup(Close)`。`accept=true` 会借助每个测试一份的 `testGroup`,把本测试其它 daemon 的 AID 写进它的 `peers.allow`,无论那些 daemon 在它之前还是之后创建;入站策略仍然是 closed。**注意 outboxLoop 和 receiveMaintenance 仍在后台运行。**
- 其它构造函数:`registered(t, url, name)`(`inbound_test.go:28`,建好并注册到 hub,停掉轮询)、`registeredPair(t) (srv, req, prov)`(`sealtest_helpers_test.go:176`)、`twoRegistered(t, srv)`(`transport_test.go:142`)、`newBareDaemon(t)`(`ctlsec_test.go:21`,不连 hub);自动回复用 `newAutoReplyFixture(t, cfg, api)` 加 `fakeOpenAI`,exec 桩用 `execStub`/`setExecCommand`/`writeStub`。
- 名单与策略:`allowPeers`/`trustPeers`/`denyPeers(t, d, aids...)` 直接追加名单文件,因为 CLI 要求 TTY;`setPolicy(t, d, p)`。
- 等待与断言:`waitUntil(t, why, cond)`(5 s)、`awaitState(t, d, ix, want)`(边轮询边等)、`lastStatusMeta`、`stateOf`、`countMsgs`、`counter(d, reason)`(读 `ReceiveStats()`)、`lastLedgerPayload(t, d, kind)`、`chainEvents`。
- 控制面:`newPlane(t)` 或 `newPlaneFor(t, d, token)` 返回 `*plane`,提供 `req(t, method, path, body, mod)`、`bearer`、`ticket`、`session`。**新增控制面路由时**,要么把它加进 `ctlsec.go` 的 `sessionRoutes`(并给出 `jsonFields`),要么明确只允许 bearer 访问;`TestEveryRouteIsAllowlistedOrRefusedForASession`(`ctlsec_test.go:193`)会遍历全部路由模式来检查。
- 传输桩:`fakeTransport{name, reachable, fail}`(`transport_test.go:15`),用 `d.RegisterTransport(ft)` 注入。

### 6.2 fake hub(`hubfake_test.go`)
- `newFakeHub(t) *httptest.Server`;`fakeHubAt(t, url) *fakeHub` 取出内部状态(改字段前先 `fake.mu.Lock()`)。
- 可调的字段:`wire`(设为 1 模拟旧 hub)、`maxEnvelope`(413)、`senderLimit`(429)、`mailboxCap`(507)、`keysOverride[aid]`(模拟恶意 hub 替换密钥)、`peers`(联邦查钥,尚未使用)。可观察的字段:`relaySends`、`authFailures`、`registerKeys`、`sendsBy`。
- 辅助函数:`queuedFor`、`onlyQueuedEnvelope`、`injectEnvelope`(模拟有人直接往信箱塞字节)、`clearMailbox`、`backdateLastSeen`、`grantOn`、`balanceOf`、`hubAIDOf`、`hubKELOf`、`relayCountFor`。
- 已实现的端点(`handler()`,`:197`):register、deregister、visibility、p2p、profile、agents、agent、reviews、relay send/poll/ack、keys GET/POST、hub/identity、agents/{aid}/kel、x402/settle、balance、ledger。它会校验 relayauth v2、对 `/relay/*` 检查 wire 头并回 426、用 `seal.ParseOuter` 检查信封结构。**它从不打开信封**,这是刻意的,新增端点也必须保持这一点。
- 原则(代码注释里反复出现):fake 必须**验证正在被测试的那个性质**。例如 `hSettle` 会真的验签,否则去掉签名的实现也能通过测试。只实现正常路径的 fake 会让任何实现都显得正确。

### 6.3 封装与收信辅助(`sealtest_helpers_test.go`)
- `keySetOf(t, d)` 取得别的节点能学到的 d 的密钥集;`sealFrom(t, from, to, typ, ix, body)` 走发送路径封装,但不投递;`receive(t, d, env) rxResult` 把信封送进收信流水线的唯一入口 `receiveEnvelope`。
- 构造恶意或异常的发送方:`sender`/`senderOf(d)`/`newStranger(t)`/`signedKeysFor(t, c, seq)`/`retiredSigner(c)`,以及 `craft(t, s, to, typ, ix, body, edit func(*seal.SealedInner))`,可以在签名之前改任意字段。
- 构造消息体:`delegateBody(t, c, ix, goal, capID)`、`chatBody(t, text, msgID)`、`mustMarshal`、`mustKEL`、`signedKeysWith`。
- 测试钩子:`d.clock`(线协时钟)、`d.rxFault func(typ string) error`(在收信事务的业务写入之后注入错误,触发回滚)、`d.ix.SetClock`。**写这些字段有竞争,见 §0 第 1 条。**

### 6.4 多 daemon + 假 hub 端到端单测的骨架
```go
srv := newFakeHub(t)
ctx := context.Background()
req := newTestDaemon(t, srv.URL, false)
prov := newTestDaemon(t, srv.URL, true)           // 接受本测试里其它 daemon
if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil { t.Fatal(err) }
if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil { t.Fatal(err) }
id, err := req.Delegate(ctx, prov.AID(), "goal", nil)     // 或 DelegateIn(…, contextID) / DelegateCapabilityIn
_ = prov.pollOnce(ctx)                                     // 手动推进每一跳
_ = prov.SendMessage(ctx, id, "answer", nil)
_ = req.pollOnce(ctx)
_ = req.RequestEnd(ctx, id); _ = prov.pollOnce(ctx)
awaitState(t, req, id, interactions.StateCompleted)
```
完整的例子是 `relay_test.go:127 TestRelayDelegationRoundTrip`。需要 p2p 与 hub 并发送达的场景,参考 `receive_test.go:553`;要让第二个 hub 参与,就再起一个 `newFakeHub`,并把 `fakeHub.peers` 串起来。

### 6.5 进程外工具
- `tools/anetfixture`:以某个 daemon 的身份(`--home` 指向数据目录)签出联调用的对象,子命令有 `cogunit`、`org-genesis`、`org-credential`、`aid`、`x402-authorize`、`relay-sign`、`org-id`,每个输出一行 base64 或 JSON。**`relay-sign` 仍在用 v1 `relayauth.Preimage`**(`main.go:321`),只对 taskboard 这类还收 v1 的路径有效。C3 要把 x402 夹具改成 a2a-x402 的消息形态,§17 要求"夹具构造 §8.7 的付款消息"。
- `tools/anetpeer`:p2p 对端进程,换行分隔的 JSON 帧 `{op, v, id, to, envelope, …}`,`V=2`,没有版本字段的帧一律拒收。用法 `anetpeer --socket … --peer … --rendezvous …`,也支持 TCP 或把 hub 当会合点。它的单测(`peer_test.go`)用桩 `host` 驱动真的 `module/p2p`。
- 脚本:`scripts/scenario.sh` 的默认层(一个空 hub 加三个节点,只用回环)、`joint-fleet.sh`(群控,worker 用桩)、`joint.sh`(需要 ANetLink/ANetMock)、`joint-shell.sh`、`prodtest.sh`(实网,需要同意)。设计 §17 要求 C 阶段的脚本改动随内核一起提交;名单一律直接写 `peers.allow`/`peers.trust` 文件,对应 `lib.sh:100/104` 的 `peer_allow`/`peer_trust`。

### 6.6 ANetHub 的夹具(`internal/aghub`,大多是 package `aghub_test`)
- 构造 hub:`newHub(t)`、`newHubWithStore(t)`(`aghub_test.go:39`,带 hub 签名密钥,模拟 main.go 的行为)、`hubInDir(t)`(`nocontent_test.go`,可以在数据目录里搜内容)、`newHubWithDir`。按 `srv.URL` 可以从全局 `sync.Map` 取到 `testHubAID`、`testHubStores`、`testHubCtrl`、`testHubServers`;`serverOf(t, srv)` 用来装接缝。
- wire 2 客户端(`wire2_helpers_test.go`):`signedDo(t, srv, c, action, method, path, body)`、`signedRequest`、`signV2`、`newRequest`(带 `X-ANet-Wire: 2`)、`send`;`relaySend`/`relayPoll`/`relayAck`、`testEnvelope(t, to, ct)`(结构合法的假信封)、`sealFrom`(真信封)、`mintKeySet`、`publishKeys`。
- 注册与支付:`register`/`registerLegacy`/`registerWithCard`/`registerBody`/`mintCard`;`fundAgent`、`signedAuth`、`creditPayload`、`authFor`、`settleCall`、`verifyCall`、`ownerGet`(本人签名 GET)、`balanceOf`。
- 联邦:`twoFederatedHubs(t, keyLookup bool) (a, b *fedHubNode)`(`fedwire2_test.go:55`)、`newFedHub`/`federate`(`card_test.go`)。
- `cmd/anet-hub`:`wiredHub(t)`(`modules_test.go`)按 main 的方式接上全部已编译进来的 mount;`settle_test.go` 做跨 hub 结算。
- 守护测试(改 hub 时必须保持绿):`importguard_test.go`(hub 不得依赖内容编解码包)、`nocontent_test.go`(数据目录里搜不到内容)、`wirecontract_test.go`(字段名、路由、版本头都钉成字符串;另有一个测试检查 webui TS 接口的字段与 Go 一致)。

---

## 7. 金标向量

- **ANetCore/golden**,以 `a2a_wire_test.go` 为模板,包括:
  - 测试函数命名为 `TestVEC_<NAME>_<n>`,编号沿用设计文档;
  - **每个字段都要填上**,并用 `assertKeys(t, what, b, []uint64{…})` 断言顶层键集合。这样字段丢失时,报错里会直接写出是哪个键,而不仅仅是一个哈希变了;
  - 不含签名的对象钉完整 hex(`wantWire`),内嵌签名或 KEL 的对象钉 CID;
  - 做一次往返(`Unmarshal` 后 `reflect.DeepEqual`);
  - 签名方用冻结的 `identity.SuiteController()`(Ed25519 是确定性的,每次跑结果相同);
  - 可能的话,用独立实现交叉验证,并在注释里写明,例如"Cross-checked against an independent deterministic-CBOR encoder (Python)"。
  - 新字段一律 `omitempty`,这样 v1 的向量(`wire_test.go`)保持逐字节不变。
- **包内金标加生成器**,参考 `seal/golden_test.go`:向量存为 `testdata/*.json`,正常测试读取并逐层验证;生成器 `TestWriteVEC_SEALED_1` 只有在设置 `SEAL_WRITE_GOLDEN=1` 时才运行。注释里明确写着"测试失败不是重新生成的理由",只有有意改格式时才重新生成,种子都写在注释里。随机的封装一侧不钉字节,只钉解封一侧。
- **a2acard**:`testdata/golden-card.json`、`golden-jwks.json`、`a2a-python-golden.json`(`loadCrossSDKVector`)。后者的生成方式仓库里没有记录。按 0012 的要求,还要补一组"经 proto → `MessageToDict` → 签名"的 a2a-python 向量(带默认值和不带默认值两个变体),同时把生成脚本或生成命令写进注释。
- **ANet/internal/golden**:只钉 CID 常量,并说明钉的理由。
- **跨仓契约**不共享类型,由两侧各自把字符串钉住:daemon 的 `internal/hubapi/hubapi_test.go`、hub 的 `internal/aghub/wirecontract_test.go`。新增线协字段时两边都要改;x402 的扩展 URI 与 metadata 键也要两侧钉字符串(§17)。

---

## 8. 代码风格

- **注释用英文**(本分支新增的 Go 注释约 7000 行,含中文的只有 22 行,散落在 hub 的 `internal/admin` 与 `internal/aghub` 里,多数只是引用中文的表名或文案)。shell 脚本里的注释中英文都有。注释密度高,主要写**为什么**:背景、曾经出过什么事("Found by the release matrix")、代价取舍。每个函数和类型都有 doc comment。涉及设计的地方写 `A2A-DESIGN §x.y`(已有 242 处),必要时附评审编号 `[Cnn]`。
- **错误处理**:
  - 包前缀:daemon 用 `anet: …`,存储用 `interactions: …`,hub 用 `hub: …`,联邦用 `federation: …`;用 `fmt.Errorf("…: %w", err)` 包装;哨兵错误写成 `var ErrXxx = errors.New("anet: …")`,例如 `ErrPolicyConflict`、`ErrNotPending`、`interactions.ErrTerminal`。
  - 协议层的错误带原因码:`seal.Error{Reason}` 配合 `seal.ReasonOf(err)`/`IsPermanent`,`a2acard.IsCode(err, Code…)`。**测试断言具体的原因码,不能只断言有错**。
  - 收信流水线把结果分为三类:`rxAccepted`、永久失败(`drop(reason, err)` 或 `rxPermanent`)、暂时失败(`transient(reason, err)`)。暂时失败不 ack。
  - HTTP 错误体统一为 `{"error": "…"}`;状态码语义固定为:401 认证失败、409 冲突或回退、413/429/507 超限、421/403 控制面的 Host/Origin 检查。
  - 日志用 `log.Printf("anet: …")`;需要防刷屏时用 `d.logOnce(key, …)`。
- **命名**:
  - 测试名写成一句话,例如 `TestAHubSubstitutingKeysForTheRecipientIsRefused`、`TestTheInboundDecisionOrder`,并在上方注释里写清要证明什么、对应设计的哪一条。
  - 证据事件用常量 `Ev… = "anet.<域>.<事件>"`,在用到它的文件里声明,核心的几个在 `ledger.go`。
  - pin 原因常量 `interactions.Pin*`;配置字段用 snake_case JSON。
  - 时间统一用毫秒:线协是 uint64,存储是 int64。
- 发布前核查发现的问题,其回归测试放在 `release_qa_*_test.go`。

---

## 9. 提交信息风格(看 `git log main`)

- **ANet 与 ANetHub 用中文**。
  - 标题是一句陈述,写"现在怎样了"或"修掉了什么",多件事用";"分隔,例如"能力调用的时长由 provider 决定,长任务移出轮询循环;--cap/--capability 统一"。
  - 正文按"一、二、三"分节,每节依次写 **位置**(文件和函数)、**行为**、**影响**、**发现方式**、**修法**。
  - 然后是"测试"一节,列出新增的断言,并**写明 mutation 验证的结果**(见 §11);最后给出总数,例如"274 项测试全绿……race 干净……no_* 九个减法 tag 与 shell 加法 tag 均编译通过"。
- **ANetCore 用英文**,格式为 `pkg: 小写开头的一句话`,正文用散文讲清原因,最后一行是 `N tests.`。
- 版本号单独提交("版本号 0.1.9")。检查点提交用 `wip: …`。
- 结尾按本会话给出的署名要求写(当前为 `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`)。旧提交里还有 `Claude-Session:` 行,不必照抄。
- 按设计 §20 阶段 G,推送与发布需要征得同意。本地提交检查点不受这个限制。

---

## 10. 构建 tag 与检查

| 仓库 | 减法 tag(默认编译进来,`-tags no_<m>` 去掉) | 加法 tag(默认没有,`-tags <m>` 加入) |
|---|---|---|
| ANet | `no_anetlink no_p2p no_blackboard no_org no_cas no_service no_mcp no_x402 no_taskboard` | `shell` |
| ANetHub | `no_federation` | `taskboard` |

- ANet 的开关文件是 `cmd/anet/module_<m>.go`,内核不 import 可选模块(`internal/daemon/modules.go` 只 import `inv1`/`inv2`);模块包自身的文件是否也打 tag 并不统一(见 0009)。
- `ANet/build.sh --check` 依次做:`gofmt -l -w internal cmd`(**会直接改文件,而且只覆盖 internal 与 cmd**,CI 检查的却是整个仓库)→ `go vet ./...` 与 `-tags shell` → `go test ./...` 与 `-tags shell` → 默认构建里 `module/shell` 的符号数必须为 0 → 构建出 `./anet`。它**不跑 `-race`,也不跑减法 tag 矩阵**;设计 §16 要求补上减法方向。在它补齐之前,提交前手动补跑:`gofmt -l .`、`go test -race ./internal/daemon/`,以及你改动所涉及的 `no_<m>` 组合的 build/vet/test。
- `ANetHub/scripts/build.sh`:会一并构建 webui(需要 docker 或 npm)与两个二进制;`SKIP_WEBUI=1` 表示只构建 Go。hub 没有 `--check`,对应的检查项以 CI(`ANetHub/.github/workflows/ci.yml`)为准。
- ANet CI(`.github/workflows/ci.yml`)的 job:`test`、`race`、`optin`(shell 双向符号检查)、`pluggable`(18 行 tag 组合)、`scenario`/`fleet`(checkout ANetHub 时**没有固定 ref**,§16 要求固定)。在本分支上所有 job 都会卡在 go.mod(§0 第 2 条)。
- 设计里待改的 tag:加 `no_a2a`(减法,符号模式为特例);**ANet 的 `module/taskboard` 仍然是减法 `!no_taskboard`**,§16/SI-8 要求改成加法,hub 那边已经改了。

---

## 11. mutation 验证是怎么做的

它是这个项目在"测试全绿"之外额外要求的证据,确认测试真能抓住它声称要抓的缺陷。0013 里写的流程是:一个 agent 实现,另一个 agent 独立复核,复核方至少做 6–8 处 mutation。

做法:
1. 找到被测的那一行判断,例如 `VerifyEncKeySet` 对 `expectAID` 的比较、`validatePolicy` 的冲突分支、`setState` 的终态守卫;
2. 临时改坏它:删掉条件、反转比较、`return nil` 提前放行、把 `==` 分支改为拒绝;
3. 只跑对应的测试,并加上 `-count=1`,例如 `go test -count=1 -run 'TestAHubSubstituting' ./internal/daemon/`,确认它变红,而且失败信息指向这个缺陷;
4. 用 `git checkout -- <file>` 还原;
5. 在提交信息的"测试"一节写明做了几处、改的是什么、各自让哪个测试变红。

历史例子:
- `1b4b337`:"五条 mutation 全部被杀。其中两条(改回同步、去掉并发上限)让测试直接挂到超时"。
- `840b8ea`:"去掉位置判断改为一律放行,中间行那条失败;去掉 gap 追加,计数那条失败"。
- `09dcb09`:"退回'测完即放开'、对固定端口也搬家、不持久化新地址,三条各让对应测试变红"。
- `0cc20c4`:"把 opencode 的落点改成 codex 的目录"。
- 设计 §17 的"补充用例"表要求**每一条都做 mutation 验证**,并给出了若干指定的 mutation,例如"去掉预期 AID"、"`==` 分支改为拒绝"、"关闭 `/fed/v2/keys` 查询"。

能让 mutation 被抓住的测试写法(照做):
- 断言具体的原因码,例如 `seal.ReasonOf(err) == seal.ReasonAIDMismatch`(`receive_test.go:354` 起);
- 同时断言"没发生"的副作用:hub 上没有排队的信封、`PeerIdentity` 里没有记录、证据链里没有该事件;
- 断言错误信息的具体内容,例如 "corrupt line 2 of 3"。只断言"报错"的话,把两种形态一起放行的实现也会变绿;
- 加前置条件守卫,例如 `nocontent_test.go:423` 的 "the test would pass for the wrong reason";
- 逐字段变异用表驱动,参考 `seal/envelope_test.go:77 TestSignedInnerFieldMutations`;
- 时间和并发相关的断言直接计时,不要靠间接现象推断。

**只读任务不要做 mutation**,因为会改工作树;要做就在自己的 worktree 里做,并且确认还原。

---

## 12. C1 / C2 / C4 改了什么(`git diff main..a2a-redesign-wip`)

改动规模:ANetCore 50 个文件,+9387/-46;ANet 123 个文件,+24396/-2450,其中文档约 5000 行;ANetHub 109 个文件,+11639/-5660。细节见 01/02/03/05 各份简报,这里只列接缝与签名。

**C1 端到端线协**
- ANetCore `seal`:
  - `Seal(inner *SealedInner, recipient *EncKey, sign SignFunc) ([]byte, error)`、`Open(envelope []byte, selfAID string, ring KeyRing) (*Opened, error)`、`ParseOuter(envelope []byte) (*SealedEnvelope, error)`;
  - `VerifyInnerSig(inner, preimage, kel, now, rotationGrace)`、`VerifyInnerKeys(inner, kel, now) (*SignedEncKeySet, *EncKeySet, error)`、`CheckTime(inner, now)`;
  - `SignEncKeySet(set, sign)`、`VerifyEncKeySet(signed *SignedEncKeySet, expectAID string, kel []identity.SignedEvent, now uint64) (*EncKeySet, error)`、`SelectKey(set, now) (*EncKey, error)`、`GenerateKeyPair`/`DeriveKeyPair(suite, ikm, nb, na)`、`DecideHighWaterSigned(seen *Seen, incoming *SignedEncKeySet) (Decision, error)`、`NewMID()`、`Padme(n)`。
- daemon 的发送一侧:`relaySend`、`sealEnvelope`、`sealWith(toAID, typ, ix string, body []byte, set *seal.EncKeySet)`、`recipientKeys(ctx, toAID, pin)`、`sendNotice`(`seal_send.go`);`queueSend(ctx, toAID, typ, ix string, body []byte, write func(*interactions.Tx) error) (int64, error)` 与 `deliverQueued(ctx, id)`、`outboxLoop`(`retry.go`)。
- daemon 的传输:`hubTransport`、`transports()`、`RegisterTransport(module.Transport)`、`deliverEnvelope(ctx, toAID, env)`、`Inbound() module.Inbound`(`transport.go`)。
- daemon 的接收一侧:`receiveEnvelope(ctx, env) rxResult`,其中 `rxResult.ack()` 在暂时失败时为 false;`rxMsg`;`commitRx(m, fn func(*interactions.Tx) error) rxResult`;`ReceiveStats()`(`receive.go`)。
- 密钥与对端身份:`encKeyRing`(`enc_keys.cbor`,`maintain`/`SignedSet`/`Key`)、`peerKEL`/`pinPeerKEL`/`notePeer`/`PinPeer(ctx, aid, reason)`/`strangerCache`(`peerkel.go`)。
- 存储:`Store.Update(fn func(*Tx) error)`、`Tx.ClaimReplay(from, mid, exp)`、`PeerIdentity`/`UpdatePeerIdentity`、`EnqueueOutbox`/`DueOutbox`。
- `module/transport.go`:`Transport{Name, Reachable, Send}`、`Inbound{Receive}` 返回 nil 就 ack,返回非 nil 表示暂时失败;`TransportHost`。p2p 帧版本号为 `V=2`。

**C2 任务模型、入站策略、自动回复加固**
- interactions:
  - `State` 共 7 个取值,`IsTerminal`;带守卫的写入 `SetState`/`Finish`/`SetResult`/`SetFailed`/`SetLateResult`;
  - `New{ID, Role, PeerAID, Goal, RequestCID, RequestDoc, ContextID, Trust, IsCapability, TaskNonce, PeerKEL, PeerKeys}` 配合 `Create`;
  - `ListFilter`/`ListPage`/`Count`(游标为 `state_at,seq`);`MessageRecord{…, MsgID, Metadata}`;`pending` 表。
- daemon 的任务 API:
  - `DelegateIn(ctx, providerAID, goal string, atts []delegation.Attachment, contextID string) (string, error)`、`DelegateCapabilityIn(ctx, providerAID, capID string, args map[string]any, contextID string)`;
  - `SendMessageOpts(ctx, ix, body string, atts, meta map[string]any) error`、`RequestEnd`、`CompleteTask`、`CancelTask(ctx, ix) (*interactions.Interaction, error)`、`SendStatus(ctx, ix, state, text, meta)`;
  - `stateOnMessage(fromRequester bool, kind string, meta []byte, payState string) interactions.State`;
  - `Watch(ix) (*interactions.Interaction, <-chan Event, func(), error)`(`eventbus.go`,先取快照再订阅,两步是原子的)。
- 入站策略(`inbound.go`):`InboundConfig`/`PublicCapability`、`decideDelegate(from, capID, argsLen) inboundDecision`、`admit`/`Admit`、`validatePolicy(c Config, untrustedBackend bool) error`、`SetInboundPolicy`、`SetPublicCapabilities`、`AllowPeer`/`DenyPeer`/`RemovePeer`、`InboundStatus`。
- 待批队列(`pending.go`):`holdDelegate`、`PendingList`、`ApprovePending(ix) (*interactions.Interaction, error)`、`RejectPending`、`expirePendingAt`。
- 其它:`transcript.EncodeV2(nonce, msgs)`;`provider.Call.Via`。

**C4 控制面与沙箱**
- `ctlsec.go`:`secureControlPlane(token, api *routeMux) *controlPlane`、`hostGuard`、`authGate`、`sessionRoutes`/`publicRoutes`、`routeMux`(记录所有注册过的 pattern)、`restrictJSONFields`、`checkLoopbackControlAddr`。
- `session.go`:控制台票据与会话。
- `attachments.go`:`Pull(ix, outDir) ([]PullResult, error)`、`checkPullOutDir`、`AttachmentBytes`、`safeName`。
- `sandbox_linux.go`(bwrap):`sandboxPlan.wrap(workDir, bin, args)`;`autoreply_exec.go:648 sandboxPlan`,沙箱不可用时拒绝执行。
- 路由与 CLI:`inbound_api.go`(`/peers/*`、`/inbound/*` 路由)、`cmd/anet/peers.go`。

---

## 13. 已知技术债(按对后续实现的阻碍程度排序)

1. daemon 在 `-race` 下失败(§0 第 1 条)。
2. 单仓构建与 CI 依赖一个不存在的 ANetCore tag(§0 第 2 条)。
3. `Module.Stop` 从不被调用,`serveModuleFaces` 只认 Payer(§0 第 3 条)。D1 首先会碰到这个。
4. 8 个测试桩 Host 要逐一维护,而且当前有两个 worktree 同时在扩展 Host(§0 第 4 条)。
5. daemon 测试里没有"带模块配置的构造函数";缺省启用的模块一旦被 import,会在所有测试 daemon 里启动(§4)。
6. fake hub 与真 hub 有差异(§0 第 5 条)。
7. `build.sh --check` 覆盖不全(§10)。
8. ANet 的 taskboard 仍是减法 tag,与 §16/SI-8 不一致。
9. 二进制被提交进了 git(§1)。
10. relayauth v1 仍在使用:`module/taskboard/taskboard.go:121`、`tools/anetfixture/main.go:321`;hub 那边 `server.go:1254-1272` 也还保留着 v1 验签路径。
11. `New` 的错误路径会泄漏资源(§3)。
12. 文档过时:`CONTRIBUTING.md` 写着"CGO required",而 build.sh 的实际做法是关闭 CGO;`hubapi` 包注释说 hub 闭源;ANetCore 的 README 包表里没有 `seal`/`a2acard`;`internal/version.V` 还是 `0.1.10`,但 wire 已经是 2,hub 返回 426 时写的是 ">= 0.2.0"。

## 14. 并行 worktree 快照(2026-09-27,供合并时参考)

`wt/*/ANet` 里有未提交改动的工作包:`a2ashape`、`agentwire`、`cardgen`、`official`、`tasksd`、`testnet`、`x402d`;所有 worktree 里 ANetCore/ANetHub 都没有改动。以下文件被多个工作包同时修改,是合并热点:

| 文件 | 修改它的工作包 |
|---|---|
| `module/module.go` | a2ashape、x402d |
| 8 个测试桩 Host | a2ashape |
| `internal/daemon/delegation.go` | a2ashape、tasksd、x402d |
| `internal/daemon/control_api.go` | cardgen、tasksd、x402d |
| `internal/daemon/hubfake_test.go` | cardgen、x402d |
| `internal/daemon/daemon.go` | cardgen、x402d |
| `internal/daemon/capability.go` | a2ashape、x402d |
| `cmd/anet/main.go` | agentwire、x402d |
| `internal/a2ashape/` | a2ashape 是正式实现;tasksd 放了一个 `provisional_tasksd.go` 占位,合并时要删掉 |

`wt/a2ashape` 的 go.mod 已经加入 `github.com/a2aproject/a2a-go/v2 v2.6.0`,并且让 `module/module.go` import 了 `internal/a2ashape`。建议按 A2A 相关包 → 任务 → 支付 → 卡片 → 接入的顺序合并。每合并一个,就跑一次 `go vet`、`go test`,再加 `-tags shell` 跑一遍,以及跑 `-race`。
