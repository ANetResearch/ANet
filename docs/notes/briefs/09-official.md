简报 · 官方公共 agent/tag 与 CI/联调脚本/文档陈述 · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §15、§16、§17(联调行与"现有脚本改动"行)、§18(与本领域相关部分)、§19、§20 对外陈述更正表、§21、SI-8。
代码:ANet `module/service`、`provider/`、`cmd/`、`build.sh`、`.github/workflows/ci.yml`、`scripts/*`、`deploy/`、`README.md`、`docs/*`;ANetHub `.github/workflows/ci.yml`、`internal/admin`、`cmd/anet-hub/wire_*.go`;ANetCore CI。
行号均为本分支 HEAD(ANetCore b4d069f / ANet 6295665 / ANetHub fa98276)。"wt/xxx" 指 `/data/projs/anet-dev/wt/<xxx>/` 下的并行工作区(未提交、不在本分支上)。

---

## 0. 一页结论

| 章节 | 判定 | 一句话 |
|---|---|---|
| §15 官方公共 agent | **缺失**(前置件部分完成) | 内核侧前置件已就绪:`inbound.public_capabilities` 与配额(`internal/daemon/inbound.go:66-113`)、`Host.Admit`(`module/module.go:117-136`)、hub admin 官方登记只剩 `id/aid/hub/caps`(`ANetHub/internal/admin/manifest.go:26-40`,`server.go:110-117`)。`cmd/anet-official` 不存在;service 模块不转发调用方、不做后端令牌、只有整体超时(`module/service/service.go:106-110,157-161`);公共能力"只记 CID"的证据模式没有;官方清单签名与 `anet.official` 标注没有;部署产物没有 |
| §16 可插拔与 CI | **ANetHub 完成,ANet 基本未动** | ANetHub CI 已有 `no_federation` 双向符号检查 + `taskboard` 加法 optin + race(`ANetHub/.github/workflows/ci.yml`)。ANet `ci.yml` 与 main 完全相同:`taskboard` 仍是减法(默认二进制含 23 个 `module/taskboard` 符号,本机 `go tool nm` 实测),组合行只查第一个 tag(`ci.yml:144`),无 `no_a2a` 行、无 `go list -deps` 检查,`joint.sh`/`joint-shell.sh` 不在 CI,hub checkout 无 ref(`ci.yml:180-183,211-215`);`build.sh --check` 不验减法方向 |
| §17 现有脚本改动 | **大部分完成** | 允许名单写文件、`accept_delegations` 移除、`/end-accept` 移除、`4-guest.sh` 删除、prodtest 9c 删除、9n 改数 `delivered`、joint-shell/container-shell 加名单都已做。未做:MCP 探针新工具名(等 D2)、prodtest 9f 改走 dmax 控制面、prodtest 9r(hub taskboard 已默认不编入)与 9k/9d 的连带失效 |
| §17 联调 | **缺失** | `joint.sh` 无 SI-1 canary、无 admin、无官方 agent、无陌生节点被拒、无伪造注入、无 p2p 并发;`joint-a2a.sh`、`joint-official.sh` 不存在;`scenario.sh` 无 §17 列出的跨 hub 加密/`/fed/v2/keys`/缓存时限/付费负面用例 |
| §18(本领域部分) | 部分 | 版本号仍 `0.1.10`(`internal/version/version.go:13`)/ hub `0.1.7`;go.mod 仍 `ANetCore v0.14.0`,脱离 GOWORK 编译失败;`voucher_url` https 强制已在代码(`module/x402/x402.go:154-163`),文档示例未改;下游影响清单未写 |
| §19 贡献草稿 | **缺失** | `ANet/docs/a2a/` 不存在 |
| §20 对外陈述更正表 | **缺失** | README/ARCHITECTURE 的"立即"与"E2E 后"两档均未改;另发现十余处同类过时表述(见 §7) |
| §21 已知局限 | **缺失** | 没有任何对外文档含"已知局限"章节;只有 hub `llms.txt:165-168` 写了第 1 条 |
| SI-8 | **不满足** | daemon 侧 `taskboard` 仍减法;`no_a2a` 与 a2a-go 尚未引入(D1);依赖闭包检查不存在于 CI(当前本机 `go list -deps ./internal/mcpserv ./internal/daemon \| grep -c a2aproject` = 0,因为 a2a-go 还没进 go.mod) |

---

## 1. 本机实测(2026-09-27,本机 ink,go1.26.6,`source /data/projs/anet-dev/.anet-env.sh`)

未运行:`prodtest.sh`、`prodperf.sh`、`realworld.sh`、`mstransit.sh`、`ship*.sh`、`2-provider.sh`/`3-requester.sh`(默认 `HUB_URL` 是生产)、`container-shell-test.sh`(从线上 install.sh 安装)——都访问生产主机或外网。

| 脚本 | 结果 | 根因 / 备注 |
|---|---|---|
| `joint.sh`(原样,全新 `J=/tmp/joint`) | 3 通过 / 17 失败 | ① 脚本缺陷:`joint.sh:82` 在任何 daemon 启动前调用 `anetfixture aid --home $REQ/.anet`,身份文件还不存在 → `read …/.anet: is a directory`,REQ_AID 与 org genesis 为空,provider 因 `org: genesis is required` 起不来。这是 main 上就有的缺陷,不是 C 阶段引入 |
| `joint.sh`(原样,第二次,身份已存在) | 3 / 17 | ② 本机没有 ANetLink/ANetMock(不在授权克隆的四个仓里),provider 的 `anetlink` 模块注册失败是致命的(`module/anetlink/anetlink.go:70-77`)→ provider 起不来。3 个"通过"里 2 个是假阳性:`joint.sh:231-233`(provider 不在时空响应 → 判为"普通资料仍可发布")与 6/6(0 条记录也算通过) |
| `joint.sh` 去掉 anetlink 与 1/6 的变体(scratchpad 副本,`J=/tmp/joint-nolink`,跑两次) | 第二次 **18 / 0** | cas/blackboard/org/INV-2/p2p/证据重启全部走通 wire 2(封装信封 + `peers.allow`)。第一次同样死在 ① |
| 附带 canary 粗查 | — | 对 `/tmp/joint/run/hub`(含 WAL)字节搜索:能力参数里的 nonce、cogunit 正文命中 0;能力 id(`cas.put` 等)与资料 summary 命中,属于卡片/资料公开元数据。**SI-1 canary 必须用随机串,不能用能力名或资料文本** |
| `scenario.sh`(默认 hermetic) | 52 / 8 | 8 个失败全在 6.5 付费闭环,都是 **C3 未做**:(a) `module/x402/pay.go:277-316` 以无签名 GET 读 `/agents/{aid}/balance`、`/ledger`,hub 已改为 `authSelf(ActionBalance)`(`ANetHub/internal/aghub/facilitator.go:1167-1178`)→ 401,balance=-1;(b) daemon 结算不带 `paymentRequirements`,hub `readFacilitatorRequest` 拒绝 `invalid_payment_requirements`(`facilitator.go:1133-1135`)。6.6 网关/凭证、6.7 兑付、7 联邦与跨 hub 评价全过 |
| `joint-fleet.sh` | **24 / 0** | 新完成语义断言(`joint-fleet.sh:264-272`)通过;MCP 7/7 仍按旧 9 个工具名(D2 前正确) |
| `joint-shell.sh` | **22 / 0** | 另有一条黄色"已知缺口"提示是脚本自身错位:`joint-shell.sh:256-265` 检查的 `$EFF` 是 8/8 里 `shell.run@slow` 的结果,不是 7/8 的 `shell.exec`;内核现在对未服务能力已回 `UNAVAILABLE`(`internal/daemon/capability.go:337-350`),该检查应移回 7/8 并改为硬断言 |
| `joint-invite.sh` | **15 / 0** | |
| `onboard.sh`(本地 hub) | 27 / 1 | 失败"读不到余额"= 同 scenario (a) |

**本机复现方法**(给后续实现者):

```bash
source /data/projs/anet-dev/.anet-env.sh            # GOWORK=三仓;脱离它 ANet/ANetHub 编不过(见 §5)
# joint.sh:它不自己构建、不自己起 hub
mkdir -p /tmp/joint/run && cd /data/projs/anet-dev/ANet
go build -o /tmp/joint/anet ./cmd/anet && go build -o /tmp/joint/anetfixture ./tools/anetfixture \
  && go build -o /tmp/joint/anetpeer ./tools/anetpeer && go build -C ../ANetHub -o /tmp/joint/anet-hub ./cmd/anet-hub
(cd /tmp/joint && setsid ./anet-hub --addr 127.0.0.1:29088 --data run/hub >run/hub.log 2>&1 </dev/null &)
J=/tmp/joint bash scripts/joint.sh; J=/tmp/joint bash scripts/joint.sh   # 第一次只为生成身份(缺陷①)
# 无 ANetLink/ANetMock 时:删掉 provider 配置里的 "anetlink" 行与 1/6 段(副本),否则 provider 起不来
# 其余自包含:scenario.sh(先把 anet/anetfixture/anet-hub 放进 /tmp/anet-scenario/bin)、joint-fleet.sh、joint-shell.sh、joint-invite.sh、onboard.sh
```

**并发陷阱**:`joint.sh:64`、`scenario.sh:80-81,579`、`joint-fleet.sh:35` 按进程名 `pgrep -x anet`/`pkill -x anet(-hub)` 杀**全机**进程;`joint-shell.sh` 与 `joint-fleet.sh` 共用 hub 端口 29188;`scenario.sh` 结束不清理(需手工 `pkill -f '^/tmp/anet-scenario/bin/'`)。在本机多工作区并行开发时,一个 agent 跑联调会杀掉别人的 daemon。`joint-shell.sh`/`joint-invite.sh`/`onboard.sh` 的清理已按路径限定,可作样板。

---

## 2. §15 官方公共 agent

### 2.1 已有(本分支,可直接用)

- 入站:`InboundConfig.PublicCapabilities []PublicCapability{ID, PerCallerPerMin, PerCallerPerDay, GlobalPerMin, MaxInflight, MaxArgsBytes}`(`internal/daemon/inbound.go:66-80`),缺省值 60/2000/1200/16/4096(`:108-113`);`SetPublicCapabilities`(`:804`)与控制面 `/inbound`(`inbound_api.go:97-110`)。§5.2 六分支在 `inbound.go:7-14` 注释与接收流水线第 9 步实现。
- 凭证门同样经 `Host.Admit(callerAID, capID, argsLen) (release func(), refusal string)`(`module/module.go:117-136`,`module/x402/voucher.go:268-280`)。
- `provider.Call.Via`(`ViaRelay`/`ViaVoucher`)已存在(`provider/provider.go:33-51`),中继路径以 `Via: provider.ViaRelay` 调用(`internal/daemon/capability.go:395-396`)。
- hub admin:官方登记只收 `id/name/tier/product_line/aid/hub/caps/summary/maintainer`,带 `runtime/monitor/ops/datasets` 的旧清单被拒(`ANetHub/internal/admin/manifest.go:26-56`);`/ops`、`/monitor/*`、`/insights`、`/acl` 走兜底 404(`server.go:110-142`);`Harvester.RunAll` 返回空(`harvest.go:51-53`)。

### 2.2 缺口清单

1. `ANet/cmd/anet-official` 不存在(`cmd/` 只有 `anet`)。
2. service 模块(`module/service/service.go`):请求只带 `Content-Type`(`:157-161`),无 `X-ANet-Caller`/`X-ANet-Call`,无后端令牌;超时只有模块级 `timeout_ms`(`:106-110`);`Describe` 返回空、`description` 不外露(`:129`)。
3. 公共能力证据"只记 `result_cid` 与指标"的模式不存在:`deliverCapabilityResult` 把 `res.Evidence`(含 service 后端完整回复 `ObservedState`,`service.go:205`)原样写链(`capability.go:543-552`);interactions 保存 TaskDoc 与交付物,无按天清理。
4. 官方清单签名、客户端验证、`anet.official: true` 标注:全仓无任何实现(grep `anet.official`/`manifest.sig`/发布密钥均无)。发布签名本身(§13.2,D2)也未做:`deploy/release/build-release.sh` 只有 sha256。
5. 部署产物(专用非 root 账户、systemd 沙箱单元、每身份 HOME 与配置模板)不存在;`deploy/` 只有 prodtest/realworld 定时器。
6. `docs/CONTRACTS-zh.md` C5 未写"两种证据模式"。

### 2.3 实施方案(按依赖顺序)

**A. service 模块接缝(先做;wt/official 已有未提交实现,应接手而不是重写)**

wt/official/ANet 的未提交改动(`module/service/service.go` +323 行、`provider/provider.go` 加 `Call.VerifiedCaller()`、新 `provider/skill.go`)已覆盖:`Authorization: Bearer`(`token_file`,模块级与按能力覆盖,非回环只许 https)、`X-ANet-Caller`(仅 `Via==relay` 时)、`X-ANet-Call`、`X-ANet-Via`、`X-ANet-Capability`、按能力 `timeout_ms`、`name/tags/examples/input_modes/output_modes`。落地时注意:

- **陷阱 1(会让测试红)**:service 实现了 `Price` → 是 Priced provider,`provider/priced_caller_test.go:23-77` 结构性禁止该包出现 `.CallerAID` 选择子。取调用方只能经 `call.VerifiedCaller()`(定义在 provider 包里),不能在 service 包里写 `call.CallerAID`。
- **陷阱 2(与 wt/cardgen 冲突)**:wt/cardgen 另有 `provider/described.go`,同样定义 `type SkillInfo`、`type Described`(函数叫 `SkillInfoFor`/`DerivedTags`),wt/official 的 `provider/skill.go` 定义同名类型(函数叫 `SkillInfoOf`/`DeriveSkillInfo`)。两个工作区合并必然重复定义,需先定一份。
- **陷阱 3(超时)**:`provider.LongRunning.InvokeTimeout` 只在 >60s 时生效(`capability.go:70-81` 的 `invokeBound`),短于 60s 的按能力超时(net.echo 1s、docs.get 1s)只能靠 service 自己给 HTTP 请求设 `context.WithTimeout`。
- `X-ANet-Call` 取 `call.CallID`(中继路径 = interaction id,凭证门 = voucher id,`voucher.go:318`)。

**B. `cmd/anet-official`(新二进制,不链入 daemon)**

建议文件:

| 文件 | 内容 |
|---|---|
| `cmd/anet-official/main.go` | 旗标 `--addr 127.0.0.1:<port>`(拒绝非回环)、`--group echo\|tools\|docs\|paid`(可多次)、每组 `--token-file-<group>`;启动时打印语料 CID 与版本 |
| `auth.go` | 每组一个令牌,`subtle.ConstantTimeCompare`;缺/错 → 401;记录 `X-ANet-Caller`、`X-ANet-Call` 到结构化日志(只记 id 与字节数,不记正文) |
| `echo.go` | `POST /echo/net.echo`:≤4 KiB 任意 JSON 对象,原样返回 + `received_at` + `version` |
| `text.go` | `/tools/text.stats`、`/tools/text.digest`(sha256/sha512)、`/tools/text.diff`(unified,各 ≤256 KiB,输出截断到 256 KiB 并置 `truncated:true`) |
| `jsonvalidate.go` | `/tools/json.validate`:JSON Schema 2020-12。可用 `github.com/google/jsonschema-go`(已作为 MCP SDK 的间接依赖在 `go.mod` 里,v0.4.3,无新依赖);**禁止远程 `$ref`**:不给 loader,并对 schema 预扫描 `$ref` 以 `http(s)://` 开头即拒,要有测试 |
| `a2acheck.go` | `/tools/a2a.card.validate`:按 a2a.proto 必填项逐项报告;签名校验复用 ANetCore `a2acard`,**必须先等 0012 的默认值剥离修正**(去默认值后 JCS,失败再按原始字节回退,并报告用了哪种形式),否则会把 a2a-python 签的卡判为坏签名。若用 a2a-go v2.6.0 类型解析,只能在 `cmd/anet-official` 内 import(见 SI-8) |
| `x402check.go` | `/tools/a2a.x402.check`:按 a2a-x402 v0.2 §7 键(`x402.payment.status/required/payload/receipts/error`)与状态迁移逐项报告。键名常量应与 C3 在 `module/x402` 定义的常量共用或由契约测试两侧钉住 |
| `docs.go` + `corpus/` | `/docs/docs.search`(`query`≤2 KiB,`k≤10`,片段≤1.5 KiB,带 `source`/`lines`/`corpus_cid`)、`/docs/docs.get`(≤16 KiB)。`//go:embed corpus`;`go generate` 脚本从 ANet `docs/*.md`、`a2a/docs/specification.md`、`Refs/a2a-x402/spec/v0.2/spec.md`、a2a-go README 拷入并写 `NOTICE`(三者均 Apache-2.0,已核);CID = 对"排序后的(路径,sha256)清单"做 ANetCore CID;搜索用确定性 BM25/词频,同语料同查询同结果 |
| `paid.go` | `/paid/demo.digest.paid`:≤4 KiB,输出与 `text.digest` 逐字节相同 |

全部路由:`http.MaxBytesReader` 按上表限长、只接 `POST application/json`、不接受 URL 参数、不出网、不写盘。

**C. 每个身份的 daemon 配置(模板放 `deploy/official/<id>.config.json`)**

```json
{"hub_url": "https://hub.agentnetwork.org.cn", "name": "anet-tools",
 "inbound": {"policy": "closed",
   "public_capabilities": [{"id": "text.digest", "per_caller_per_min": 20, "max_args_bytes": 262144}, …]},
 "modules": {"service": {"token_file": "${CREDENTIALS_DIRECTORY}/tools.token",
   "capabilities": [{"id": "text.digest", "url": "http://127.0.0.1:8610/tools/text.digest",
                     "timeout_ms": 2000, "name": "Text digest", "description": "…", "tags": ["hash"]}]}}}
```

E(`anet-paid-demo`)另需 `x402` 模块、`price`,**不配 `voucher_addr`**;它的 a2a-x402 同任务流依赖 C3,卡片 `required:true` 扩展声明依赖 C5/D1。注意 `max_args_bytes` 缺省 4096,tools/docs 的 256 KiB 参数要显式放大。

**D. 公共能力证据模式**

改 `capability.go:543-552`:当交互 trust=`public_cap` 且配置为 `cid` 模式时,`ev["evidence"]` 只保留 `protocol/verify_trust/latency_ms/native_ack`,去掉 `observed_state`(`result_cid` 已在事件里)。配置键建议挂在 `PublicCapability` 上(`"evidence": "full"|"cid"`,缺省 `full` 以免改变现有语义),并在 `docs/CONTRACTS-zh.md` C5 写两种模式;官方身份一律 `cid`。§21 第 4 条要求的"保存策略公开写明"还需要 interactions 的按天清理(新函数,如 `interactions.Store.PruneTerminal(before time.Time, trust string)`)或在文档中如实写"不清理"。

**E. 官方清单与 `anet.official`**

- 新包 `internal/official`(无 build tag、不 import a2a-go):`//go:embed manifest.json manifest.json.sig`;`type Entry{ID, AID, Hub string; Caps []string; Maintainer, Contact string}`;`Load() (Manifest, error)` 用发布公钥验签、检查 `expires`;`Lookup(aid string) (Entry, bool)`。
- 签名格式与 §13.2 的发布签名共用(`ssh-keygen -Y sign`,SSHSIG),验签器放一个公共包(如 `internal/relsig`)供 `anet update`(D2)与本包共用,不要写两份。发布私钥归属由产品负责人定(§13.2 末条)。
- 消费方:控制面 `/find` 的响应(`control_api.go:562-583`)在 daemon 层包一层加 `official: true`(不要改 `hubapi.AgentView`,那是 hub 线协类型);MCP `list_agents`(D2)与本机 A2A 代理卡片(D1)用同一 `Lookup`,键名 `anet.official` 由契约测试钉住。名称不作判据,只看 AID。

**F. 部署产物(只写文件;部署需征求同意,§15 末条/§20 G)**

`deploy/official/`:`anet-official.service`(`User=anet-official`、`NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome`、`PrivateTmp`、`IPAddressDeny=any`+`IPAddressAllow=localhost`、`LoadCredential=` 放令牌)、`anet-official@.service` 模板(每身份一个 HOME)、五个配置模板、`README.md` 运维手册(dmax 专用账户、升级、清单更新)。

**G. 测试**

- 后端单测:每路由限长与 413、令牌 401、`json.validate` 远程 `$ref` 被拒、`text.diff` 截断标记、`a2a.card.validate` 对每个必填字段缺失报错且对"线上带默认值"的合法卡给出正确签名结论、`docs.search` 两次同结果同 `corpus_cid`、`demo.digest.paid` 与 `text.digest` 逐字节相同。
- service 单测:`X-ANet-Caller` 在 `Via=voucher` 时为空(mutation:改为读 `CallerAID`)、令牌只发往回环/https。
- `scripts/joint-official.sh`:见 §4.2。

---

## 3. §16 可插拔编译与 CI

### 3.1 现状

- **ANet**:`cmd/anet/module_taskboard.go:1` 与 `module/taskboard/*.go:1` 仍是 `//go:build !no_taskboard`;`optin_tags.go:16` 只 `DeclareOptIn("shell")`;`optin_default_test.go` 只防 shell。0013 所说"`-tags taskboard` 加法构建 ANet 过"只证明未知 tag 能编译,并非加法方向。`.github/workflows/ci.yml` 与 main 一字未改。`build.sh:24` `OPTIN="shell"`,`:52-62` 只验加法。
- **ANetHub**:已完成。`wire_taskboard.go:1` `//go:build taskboard`,`internal/taskboard/*.go` 带 tag;CI `unplug`(build+vet+test+双向符号)、`optin`(taskboard 双向)、`race` 含 `-tags taskboard`。本机实测 hub 默认二进制 `internal/taskboard` 符号 0。残留:CI `on.push.branches` 为 `[main, "anet4/**"]`,本分支名 `a2a-redesign-wip` 不触发。
- **ANetCore**:CI 仅 gofmt/vet/test/race,本领域无需改;可选加"go.mod 不新增 require"检查以兑现 §18"不新增依赖"。
- **CLAUDE.md**:三仓及其上级目录都不存在项目 `CLAUDE.md`(本机全盘 `find -maxdepth 4` 无)。设计 §16 末条与 0009 引用的"CLAUDE.md tag 列表"在本检出里没有载体;仓内实际承载 tag 列表的是 `ANet/README.md:118-127`、`docs/DISTRIBUTIONS-zh.md:17-33,49-59`、`docs/DESIGN-zh.md:122,196`、`ANetHub/README.md:63-64`。

### 3.2 ANet 改造方案

**(1) taskboard 改加法(E 阶段,也可提前单独做)**

- `cmd/anet/module_taskboard.go:1` → `//go:build taskboard`;`module/taskboard/taskboard.go:1`、`taskboard_test.go:1` → `//go:build taskboard`。
- `cmd/anet/optin_tags.go:16` → `module.DeclareOptIn("shell"); module.DeclareOptIn("taskboard")`(或变参);`optin_default_test.go` 同时防 `taskboard`;新增 `cmd/anet/optin_taskboard_test.go`(`//go:build taskboard`,断言 `Compiled()` 含 taskboard),仿 `optin_shell_test.go`。
- 连带:`scripts/onboard.sh:38-40` 三档去掉 `no_taskboard`;`ci.yml` 所有行去掉 `no_taskboard`;`README.md:126`、`DISTRIBUTIONS-zh.md:27`、`DESIGN-zh.md:122,196` 改到加法;`build.sh` `OPTIN="shell taskboard"`。
- **迁移陷阱**:任何生产节点 config 里有 `modules.taskboard` 块,新默认二进制会以 "it needs -tags taskboard" 拒绝启动(`module/module.go` `Build` 的 optIn 分支)。升级前要查 ink93/cmax/dmax 的 config(prodtest 9r 在 ink93 上探测过 `module/taskboard`,`prodtest.sh:1717,1741`)。

**(2) `ci.yml` `pluggable` job**

- 组合行逐 tag 检查(替换 `ci.yml:144-155`):

```bash
go build -o /tmp/anet-full ./cmd/anet
go build -tags "$TAGS" -o /tmp/anet-lean ./cmd/anet
for t in ${TAGS//,/ }; do
  m=${t#no_}
  case $m in
    a2a) pat='module/a2a|a2aproject/a2a-go' ;;
    mcp) pat='internal/mcpserv|internal/agentwire|modelcontextprotocol/go-sdk' ;;
    *)   pat="provider/$m|module/$m" ;;
  esac
  full=$(go tool nm /tmp/anet-full | grep -cE "$pat" || true)
  lean=$(go tool nm /tmp/anet-lean | grep -cE "$pat" || true)
  echo "$m full=$full lean=$lean"; test "$full" -gt 0 && test "$lean" -eq 0
done
```

  把这段放进 `scripts/tagcheck.sh`,CI 与 `build.sh --check` 共用,免得两份规则漂移。`internal/agentwire` 目前不存在(D2,wt/agentwire 在做),加入模式前 `full>0` 对它不成立——按包是否存在分别断言,或等 D2 合入后再加。
- 新增单行 `"no_a2a"`(与 D1 同批;单行保留 MCP)。发行档位行(`ci.yml:113-116`)是否含 `no_a2a` 取决于 DISTRIBUTIONS 的决定。
- `test` job 加:`test -z "$(go list -deps ./internal/mcpserv ./internal/daemon | grep a2aproject)"`,并用 `-tags no_a2a` 再跑一次(SI-8)。

**(3) `optin` job**:`matrix.tag: ["shell", "taskboard"]`;`race` job 增 `go test -race -tags taskboard ./module/taskboard/... ./cmd/anet/...`。

**(4) 联调 job**

- 跨仓 checkout 固定 ref(§16 [C37]):`scenario`、`fleet` 两处 `uses: actions/checkout@v4 with: repository: ANetResearch/ANetHub, path: .hub, ref: <wire-2 hub 的 tag 或 commit>`,与第一个 wire-2 推送同批。
- 新增 `joint-shell`(自包含,`HUB_SRC=.hub`)、`joint`(见 §4.1 改造后)、`joint-a2a`(D1 后)、`joint-official`(E)。
- **ANetCore 未打 tag 期间 CI 必红**:本机实测 `GOWORK=off go build ./...` 在 ANet(`internal/daemon/capability.go:30` 找不到 `ANetCore/seal`)与 ANetHub(`internal/aghub/card.go:16`)都失败。按 §18 这是预期(推送前先打 Core tag 再升 go.mod);若要在推 PR 分支时让 CI 可用,只能加临时步骤 checkout ANetCore 固定 commit 并 `go work init . ./.core [./.hub]`,打 tag 后删除。
- **沙箱测试在 CI 会静默跳过**:`sandbox_linux_test.go:219,222` 在无 bwrap 或无法建命名空间时 `t.Skip`。ubuntu-latest(24.04)默认没有 bubblewrap,且 AppArmor 限制非特权 userns。需在一个 job 里 `sudo apt-get install -y bubblewrap && sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`,并对 `go test -run Sandbox -v` 的输出断言无 `SKIP`,否则"沙箱失败闭合两方向"(§17 daemon 单测行)在 CI 里从未执行。

**(5) `build.sh --check`**:调用 `scripts/tagcheck.sh`,覆盖减法方向与 `taskboard`。另:`scripts/build.sh:35` 用 `-s -w` 去掉符号表,其产物无法做符号检查(0009 §1.4),文档体积表(`DISTRIBUTIONS-zh.md:10`)也按此口径,且已过时(本机 onboard 实测 min/standard/paid ≈18.8–19.0 MB,表中 14.3 MB)。

---

## 4. §17 脚本改造清单

### 4.1 逐脚本

| 脚本 | C 阶段已改(本分支) | 仍是旧语义 / 缺陷 | 要做 |
|---|---|---|---|
| `joint.sh` | 去掉 `accept_delegations`(`:76-77`);provider 写 `peers.allow`(`:87`);头注释写明 GOWORK | 缺陷①(`:82` 先取 AID 后起 daemon);不自建、不起 hub;全机 `pgrep -x anet` 杀进程(`:64`、`:94`);强依赖 ANetLink/ANetMock;`:231-233` 与 6/6 在 provider 挂掉时假阳性;§17 要求的全部新段缺失 | 见下方"joint.sh 改造" |
| `joint-fleet.sh` | allow+trust 写文件(`:117-125`);`/end` 后断言 `completed`(`:264-272`) | MCP 7/7 用旧 9 个工具名(`:294`,`mcp-probe.py:60,68,77`);全机 `pkill -x`(`:35`) | D2 合入时改新工具名(`list_agents`、`send_message`…,§12);清理改按 `$J` 路径 |
| `scenario.sh` | A/B/C/D 去掉 `accept_delegations`;互写 `peers.allow`(`:195-202`,D `:597-598`);A 的 `text.digest.paid` 列为公开能力供凭证门(`:160-163`);去掉 `/end-accept`(`:278-280`) | 6.5 在 C3 前必红(§1);全机 kill(`:80-81,579`);不清理进程 | C3 后回绿;按 §17 补:两 hub 加密委派(含只允许名单、hub-local 可见性的 provider 经 `/fed/v2/keys`)、provider 重启后超缓存时限回复、跨 hub 付费(少付、错收款方负面用例,正确付款只产一份 credit)、C2 approve 超时限后批准仍送达、C32 关 `/fed/v2/keys` 的 mutation、C34 三种取消顺序 |
| `onboard.sh` | 去掉 `accept_delegations`(`:78`);standard 以 `public_capabilities` 公开两能力(`:136-138`) | 三档 tag 串仍含 `no_taskboard`(`:38-40`);paid 档读余额失败(C3);`HUB_EXTERNAL` 收尾不注销(`:255-260`,0010 §3.3 生产残留身份的来源) | 去 `no_taskboard`,按 DISTRIBUTIONS 决定是否加 `no_a2a`;外部 hub 模式收尾调 `/hub-leave` |
| `lib.sh` | `peer_allow`/`peer_trust` 助手(`:96-104`);删除访客说明 | — | 其他脚本可改用这两个助手 |
| `2-provider.sh`/`3-requester.sh` | 提示改为允许名单;去掉 `--guest-messages` | 默认连生产 hub | — |
| `4-guest.sh` | 已删除 | — | — |
| `joint-shell.sh` | provider 写 `peers.allow`(`:79-85`) | `:256-265` "已知缺口"检查的是错的 `$EFF`(见 §1) | 移到 7/8,断言 `status=UNAVAILABLE` 且 reason=`capability_not_served`(`capability.go:347`) |
| `container-shell-test.sh` | 容器内写 `/root/.anet/peers.allow`(`:57-61`) | 从线上 install.sh 安装,测的是已发布版本 | 发布后再跑 |
| `joint-invite.sh` | 无需改 | — | 实测 15/15 |
| `prodtest.sh`(生产,需同意才可跑) | `allow_on` 写生产节点名单(`:339-358`,**本身是生产变更**);9b 改单方完成;9c 删除;9n 数 `delivered`;9q 去 `accept_delegations` | **9f** 仍无签名读 `viafmax /agents/$DMAX_AID/redemptions`(`:836-842`),hub 已改为 `authSelf(ActionRedemptions)`(`ANetHub/internal/aghub/redemption.go:480-484`)→ 必 401;daemon 没有列兑付记录的控制面路由,"改走 dmax 控制面"需先加路由(如 `POST /redemptions`,由 x402 模块签名读 hub)。**9r** 驱动 emax 的 `/tasks/*`(`:1699,1776,1785`),hub 默认构建已不含 taskboard → 需改为"默认 404"断言或按 `/stats.modules` 条件跳过。**9k** MCP 旧工具名(`:1160-1195`,D2)。**9d** "llms.txt 教的每条命令这个构建都有"(`:773`)会失败:hub `llms.txt` 已教 `anet update`、`anet agents`,当前 CLI 没有(D2)。6/7/9j 付费段依赖 C3。缺官方 agent 只读探测(0010 §5.3:每小时调 A1/A2/C、比对 `corpus_cid`) | 按左列逐项改;部署后在双 hub 重跑(§17 实网行,需同意) |

**joint.sh 改造(§17 联调行)**——建议拆两步,先修脚本本身再加断言:

1. 自包含:仿 `joint-fleet.sh:40-50` 自己构建 anet/anetfixture/anetpeer/anet-hub/anet-hub-admin,自己起 hub 与 admin;清理按 `$J` 路径(仿 `joint-shell.sh:30-40` 的 `KIDS` + trap);先各启动一次 daemon 生成身份再取 AID(仿 `joint-shell.sh:67-76`);`JOINT_DEVICES=0`(或检测不到 `$J/anetlinkd`)时不配 anetlink、跳过 1/6,CI 默认此模式。
2. 新段:
   - **SI-1 canary**:`ADMIN_TOKEN=<随机32字节> anet-hub-admin --addr 127.0.0.1:<p> --hub-data run/hub --data run/admin --snapshot-every 2s --harvest-every 2s`(旗标见 `ANetHub/cmd/anet-hub-admin/main.go:41-50`);把随机 canary 放进散文委派 goal、聊天消息、附件字节、能力参数,以及**一次对官方 agent 测试实例(anet-official + 一个 closed+public_capabilities 的 daemon)的调用**;等两个采集周期后对 `run/hub`(含 `-wal`)、`run/admin` 做 `grep -a -r -F`,对 `GET /agents/{aid}`、`/fed/v1/reviews`、admin `/api/sessions`、`/api/sessions/{source}/{id}` 做字节搜索,均须 0;admin `/api/official/{id}/insights`、`/acl`、`/monitor/x`、`/ops` 须 404(兜底在 `server.go:136-141`);反向断言收件方 `/thread` 里能读出 canary。mutation(关加密、恢复采集源)以补丁文件形式放在 `scripts/mutations/`,在临时工作区应用后须命中,不要在产品二进制里加开关。注意:能力 id 与资料文本本来就在卡片里,不能当 canary(§1 实测)。
   - **陌生节点被拒**:第三个 daemon(不在 `peers.allow`)委派 → 得到 `rejected`、`anet.reason=not_accepting`,provider `/inbox` 无该条。
   - **hub 伪造注入被丢弃**(SI-4):需要 anetfixture 新子命令,例如 `anetfixture seal --home X --to AID --from-claim Y --type delegate …` 产出以 X 签名、内层声称 Y 的信封,再 `anetfixture relay-send --home X --envelope FILE`(relayauth v2 以 X 认证)投递;断言 provider 丢弃且链上无执行事件。
   - **p2p 并发**(SI-10):给 `tools/anetpeer` 加测试用 `--tee-dir`,把经手的帧落盘;脚本取出同一信封再经 hub 投一次(`anetfixture relay-send`),断言 provider 只处理一次(交互数、证据事件数不变)。

### 4.2 新脚本

- `scripts/joint-official.sh`(E):hub(+admin)+ `anet-official` + 三个官方身份(A1 `net.echo`、B tools、E paid)+ 两个全新请求方(只做 `anet init` 默认值)。五层:① 准入——自然语言委派与非公开能力 → `rejected`,官方节点 interactions 无记录;② 配额——同一调用方超 `per_caller_per_min` 被拒且另一调用方不受影响,`global_per_min` 生效时两者都受限;③ 后端——不带令牌直连后端 401,后端日志中的 `X-ANet-Caller` 等于请求方 AID;④ hub 不见内容——canary 同上;⑤ 身份与处置——对端写进 `peers.deny` 后与陌生人同答复;同名冒充节点不被标 `anet.official`。付费段(E)依赖 C3。mutation:去掉配额检查、把非公开能力加入 `public_capabilities`、去掉后端令牌校验。
- `scripts/joint-a2a.sh`(D1 之后;属 D1/08-mcpcli 简报范围,这里只列 CI 位置)。

---

## 5. §18 与本领域相关的部分

- 版本:`ANet/internal/version/version.go:13` 仍为 `"0.1.10"`,ANetHub `internal/version` 为 `"0.1.7"`;hub 426 文案要求 "anet >= 0.2.0"(`ANetHub/internal/aghub/server.go:244-249`),实际门槛是 wire 版本头。发布前需升到 v0.2.0。
- go.mod:两仓仍 `ANetCore v0.14.0`,脱离 GOWORK 编不过(§3.2(4));a2a-go 未 `go get`(D1)。
- 开发期 GOWORK:设计写 `/data/projs/anet-oss/.anet-work/go.work`,本机实际是 `/data/projs/anet-dev/.anet-work/go.work`(`.anet-env.sh`);`/data/projs/anet-oss` 是另一个项目。设计该处应改。
- `voucher_url` 非回环强制 https 已在代码(`module/x402/x402.go:154-163`);`docs/GUIDE-zh.md:230` 与 `docs/site/guide.html:177` 仍是 `http://1.2.3.4:4002…`(后者还缺 `/x402/redeem` 路径,会被 `checkVoucherURL` 以另一条规则拒绝)。
- 下游影响清单(ai-studio anetbridge、Research-Galaxy agent-runtime、ANetOS-Web hubkeeper):除设计本身外无任何文档提及。
- 仓库卫生:`ANet/anetfixture`、`ANet/anetpeer` 两个二进制被 git 跟踪(`.gitignore` 虽列出但已入库),检查点提交把 `anetpeer` 从 4 MB 换成 9 MB。建议 `git rm --cached` 两者。

---

## 6. §19 贡献草稿

`ANet/docs/a2a/` 不存在,六项(中继绑定、注册表 API、`anet-credit` scheme、a2a-go issue、a2a-x402 issue、`SecurityScheme` 提议)全部未写。前置:许可证。ANet 当前是 "ANet Community License"(`ANet/LICENSE:1`),而 A2A 规范、a2a-go、a2a-x402 均为 Apache-2.0;规范文本与参考实现以何种许可贡献需产品负责人先定,否则草稿无法对外。wt/a2adocs 工作区存在但无改动。

---

## 7. §20 对外陈述更正表

设计表内四行全部未改(本分支与 main 相同):

| 位置 | 现文 | 状态 |
|---|---|---|
| `ANet/README.md:192` | "The Hub relays bytes and keeps an index — it is *not* a trusted party." | 未改 |
| `ANet/docs/ARCHITECTURE-zh.md:313` | "Hub 只搬运不透明字节" | 未改 |
| `ARCHITECTURE-zh.md:38` | "普通聊天消息则以明文中继" | 未改 |
| `ARCHITECTURE-zh.md:170`、`:316` | `/relay/send` "开放"、"刻意不鉴权" | 未改 |

"立即"那一档应在 **main** 上单独提交(v0.1 现状),推送需同意;本分支上的文档应直接写"E2E 部署后"那一档,但分支文档不得在部署前发布到官网(`docs/site` 由 `docs/site/build.py`/`build-all.sh` 从 md 生成,改 md 后要重建,且与部署同步)。

"阶段 E 检索后补入"的同类表述(本次检索结果,可直接补入表中):

| 位置 | 过时内容 | 应改为 |
|---|---|---|
| `ARCHITECTURE-zh.md:99` | `delegation` 载荷描述为明文 DelegateReq/ChatMsg/ResultResp | 内层对象经 HPKE 封装(§3.3) |
| `ARCHITECTURE-zh.md:120`、`:129`、`:314` | `accept_delegations` 默认开 | `inbound.policy=closed` 与三份名单(§5) |
| `ARCHITECTURE-zh.md:132`、`:225`;`docs/AUTO-REPLY-zh.md:226` | 结束协商 `end_request`/`end_accept`、`/end-accept` | provider 单方完成;`/end-accept` 回 410(`control_api.go:850`) |
| `ARCHITECTURE-zh.md:324-338`;`docs/GUIDE-zh.md:371`;`docs/CONTRACTS-zh.md:106,221` | 访客模式与 `/guest/*` | 删除(§9) |
| `README.md:124-127`;`DISTRIBUTIONS-zh.md:27`;`DESIGN-zh.md:122,196` | `no_taskboard` 为减法 | `taskboard` 加法;减法列加入 `no_a2a`(D1 后);DISTRIBUTIONS 攻击面表(`:49-59`)加 a2a 回环 HTTP 监听一行 |
| `docs/GUIDE-zh.md:230`;`docs/site/guide.html:177` | `voucher_url` http 示例 | https(§18) |
| `ANetHub/docs/CHAT.md:6` | guest broker 以 hub 身份代签、转发明文 | 删除或标注历史 |
| `cmd/anet/main.go:227` 等 CLI 帮助 | "v0.1, centralized via the official Hub" | 版本与描述随 v0.2 更新 |

ANetHub 侧 `llms.txt:165-168`、`README.md:63-64`、`docs/DATA-ASSETS.md`、`docs/POSITIONING.md` 已在 B 阶段改为如实表述(llms.txt 写的是"0.2.0 的目标是…",措辞可在部署后去掉"目标")。

---

## 8. §21 已知局限

14 条均未写入任何对外文档。建议落点:ANet `README.md` 新增 "Known limitations" 小节(英文,逐条)、`docs/GUIDE-zh.md` 同内容中文、hub `llms.txt` 保留第 1 条并链到 README;第 4 条(官方 agent 能看到调用内容、保存策略)与 §2.3 D 的证据模式、清理策略一起写;第 13 条(TTY 门槛不是同 uid 边界)要写进 `anet peers`/`anet pay` 的帮助文本(`cmd/anet/peers.go` 的 usage)。

---

## 9. SI-8 验收现状与测试方法

| 判据 | 现状 | 怎么验 |
|---|---|---|
| `no_a2a`:`module/a2a` 与 a2a-go 符号 0,完整构建 >0 | 无 `module/a2a`(D1) | `pluggable` 单行 `no_a2a`,模式 `module/a2a\|a2aproject/a2a-go`(a2a-go v2 依赖 v1 模块 `a2aproject/a2a-go v0.3.x`,此模式同时覆盖) |
| `taskboard` 默认 0、`-tags taskboard` >0 | hub 满足;**daemon 不满足**(默认 23 个符号) | §3.2(1)+ `optin` 行 |
| `internal/mcpserv`、`internal/daemon` 依赖闭包不含 `a2aproject` | 当前为 0(a2a-go 未引入,检查无意义) | `test` job 加 `go list -deps` 检查;**`cmd/anet-official` 若 import a2a-go,只能在该 cmd 目录内;不要把 a2a-go 相关校验放进 `internal/` 下会被 daemon 引用的共享包** |

---

## 10. 其他陷阱与协作注意

- 并行工作区:wt/official 有 service/provider 的未提交实现(§2.3 A);wt/cardgen 有 `internal/daemon/a2a_card.go`、`module/card.go`、`provider/described.go` 等未提交实现,与 wt/official 的 `provider/skill.go` 类型重名;wt/agentwire 删除了 `internal/daemon/install.go` 并新增 `internal/agentwire`(影响 `no_mcp` 符号模式)。合并顺序需先协调。
- `scenario.sh`/`onboard.sh` 付费段在 C3 前必红,E 的付费演示与 prodtest 付费段同样依赖 C3;不要把这些失败当成本领域的回归。
- 联调脚本的全机 kill 与共用端口会在多工作区并行时互相打断(§1)。
- `prodtest.sh` 的 `allow_on` 会改生产节点的 `peers.allow`,整次运行需同意(脚本注释 `:339-344` 已写明)。
