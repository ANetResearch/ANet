简报 · hub 内容移除/注册表/联邦(B 阶段核查) · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §9 全部、§3.7 hub 侧、§3.9、§10.5、§10.6、SI-1、SI-2、§16 ANetHub CI、§13.2 llms.txt Step 0。
代码:ANetHub 全仓(`fa98276`),交叉核对 ANetCore `b4d069f`、ANet `6295665`。只读核查,未改任何仓库文件。
行号以本分支为准。"已完成"只在读过代码且找到对应测试时才算。

---

## 0. 结论

1. **B1–B3 与 §9 内容移除在代码和单测层面基本到位**:relay v2(认证发送、限额、只存封装信封、ack 即删、
   `secure_delete`)、`/agents/{aid}/keys`、`/fed/v2/keys`、KEL 只接受延伸、联邦转发 v2、评价无内容、
   访客模式删除、admin 采集与官方 agent 运维路由删除、`completed_task` 删除、taskboard 改加法 tag、
   静态护栏测试、llms.txt Step 0。`go test ./...`、`-tags taskboard`、`-tags no_federation` 均过。
2. **B4 注册表实际上没有做**。0013 写"A2A 卡片准入与注册表 API、JWKS、联邦 v2 卡片"已完成,代码里只有
   `/register` 把 `a2a_card` 原样存进 `agent_a2a_card`,状态恒为 `unverified`
   (`internal/aghub/a2acard.go:9-36`,注释原话 "Nothing is verified or indexed here")。
   `GET /a2a/v1/agents`、`GET /a2a/v1/agents/{aid}/card`、`GET /agents/{aid}/jwks.json`、`GET /fed/v2/cards`、
   `agent_skill`/`agent_tag` 索引在全仓都不存在(`grep -rn "a2a/v1\|jwks\|fed/v2/cards" --include=*.go` 无命中)。
   hub 也没有 import `ANetCore/a2acard`。这是本领域最大的剩余工作(§3 H1–H4)。
3. **上线前还缺的几件事**:
   - 迁移会把 wire-1 **未投递的明文行**复制进新 relay 表(`relay.go:290-294`),在 TTL 到期前一直在盘上。
   - 清理脚本与周备份脚本**从未跑过**:本机没有 `sqlite3`,也没有测试覆盖。
   - `deploy/nginx-hub.conf:58` 的 `client_max_body_size 96m` 小于 hub 新的发送体上限(约 128 MiB)。
   - hub 主机上旧的 ssh 密钥与 monitor 令牌通道不在清理清单里。
   - CI 在不使用 go.work 的情况下必然失败(go.mod 仍钉 ANetCore v0.14.0)。

---

## 1. 已完成(核实过)

| 要求 | 证据(实现) | 证据(测试) |
|---|---|---|
| §3.7 `X-ANet-Wire: 2`,`/relay/*` 对 <2 回 426 | `internal/aghub/server.go:205-252` `wireContract` | `wire2_test.go:68` |
| relayauth v2:头部认证、原始体哈希、绑定 hubAID、`(aid,sig)` 重放缓存 | `auth2.go:17-267` | `wire2_test.go:111`、`replaycache_test.go:13`、`wire2_refusals_test.go:491` |
| `/relay/send` 认证、发送方须已注册、令牌桶 20/s 突发 200、429+Retry-After、413、507、400、404 | `server.go:1091-1182`,`limits.go:57-71` | `wire2_test.go:221,264,274,306,342,933` |
| 不存 `from_aid/kind/interaction_id`;表只有 `id,to_aid,size,created_at,payload` | `relay.go:222-228`、`RelayEnqueue` `relay.go:88-125` | `wire2_test.go:364`(SI-2) |
| ack 即删、TTL 14 天(janitor 每 10 分钟)、`secure_delete=ON` | `relay.go:166-214`,`aghub.go:157-163`,`cmd/anet-hub/main.go:283` | `wire2_test.go:405,446,476` |
| wire-1 表迁移(删已投递行、清 WAL、保留自增) | `relay.go:263-322` | `wire2_test.go:528` |
| `/register`:删 `guest_messages`,KEL 须延伸(事务内复核),`enc_keys`/`a2a_card` 按字段报告,按 IP 限速 | `server.go:566-736`,`aghub.go:474-541` | `wire2_test.go:623,768,899`,`wire2_refusals_test.go:346,392` |
| `GET/POST /agents/{aid}/keys`(本地 → 联邦卡片 → `/fed/v2/keys`;发布方 409 规则) | `keys.go:87-356` | `wire2_test.go:696,815,828`,`wire2_refusals_test.go:171,238,421` |
| ledger/balance/redemptions 本人签名 GET | `server.go:1454`、`facilitator.go:1168`、`redemption.go:481` | `facilitator_test.go:565-580` |
| §3.9 `/fed/v1/forward` v2:信封无发送方元数据、原像去 4/5/6 键、`ForwardVersion=2` | `internal/federation/federation.go:163-205,358-...` | `federation/wire2_test.go:30,77,106,126` |
| §3.9 `/fed/v2/keys/{aid}`:发起 hub 签名、只接受对等表、重放集合有界;请求方不缓存、不入目录 | `federation/keys.go`,`aghub/keys.go:233-280` | `federation/wire2_test.go:162,189`,`keys_refusals_test.go`,`aghub/fedwire2_test.go:111,178,193` |
| §3.9 联邦卡片条目带 `keys`;`fed_card.kel` 只接受延伸 | `card.go:492-556,564-663` | `federation/wire2_test.go:267`,`fedwire2_test.go:214,299` |
| §3.9/§9 `FedReview` 删内容字段;空串变非 nil 切片的问题已修 | `reputation.go:56-62` | `nocontent_test.go:234` |
| §9 评价:上传带内容即 400;验证传 nil;表重建 + VACUUM + 截断 WAL;`content_binding:"UNVERIFIED"`;评语 ≤280 | `server.go:786-846`,`content_v2.go:83-165`,`aghub.go:85-125` | `nocontent_test.go:76,136,165,375` |
| §9 `completed_task` 删除;`/stats.tasks_completed` = 本 hub 已公开评价中按 `receipt_cid` 去重的回执数;文档写明含义 | `aghub.go:999-1045`,`README.md:12`,`docs/DATA-ASSETS.md §3`,`Hero.tsx:65,90-96` | `nocontent_test.go:269`,`admin_test.go:727` |
| §9 访客模式:`guest.go` 删,路由 404/405,`guest_quota` 列删(迁移),`AgentView.GuestQuota` 删,admin `/quota` 删 | `server.go:330-334`,`content_v2.go:142-146` | `nocontent_test.go:317`,`wirecontract_test.go:544`,`admin_test.go:202` |
| §9 recovery 列清单 + 从旧 schema 备份恢复 | `internal/admin/recovery.go:90-167` | `admin_test.go:776` |
| §9 admin 采集删除(`RunAll` 空实现)、insights/monitor/ops/acl 路由删除、Manifest 拒收 `runtime/monitor/ops/datasets` | `admin/harvest.go:24-53`,`admin/server.go:106-142`,`admin/manifest.go:26-78` | `admin_test.go:114,159,202,355` |
| §9 admin 其余读取方改为行存在计数 | `admin/hubread.go:79-220` | `admin_test.go:727` |
| §9 webui:删 `ChatDialog`/`lib/guest.ts`、"内容已验证"、首页文案;`TasksSection` 按 `/stats.modules` 隐藏;嵌入包已重建(含 "PUBLISHED RECEIPTS",无 guest) | `webui/src/App.tsx:50-56`,`AgentDetailDialog.tsx:11-50` | `webui/src/**/__tests__`(AgentDetail/Chrome/Tasks/App) |
| §9 taskboard 加法 tag(hub 侧) | `internal/taskboard/*.go:1`(`//go:build taskboard`),`cmd/anet-hub/optin_tags.go` | `cmd/anet-hub/optin_default_test.go:15`、`optin_taskboard_test.go:14` |
| §9 静态护栏:aghub/admin/两个 cmd 的 `go list -deps` 闭包不含 `delegation`/`tsir` | — | `internal/aghub/importguard_test.go:28` |
| §9 `hub-db-roll.sh` 不再用 `delivered_at`,周备份排除 relay 表,每次 WAL checkpoint | `deploy/hub-db-roll.sh` 全文 | **无**(见 H6) |
| §9 生产清理脚本(默认只报告,`--apply` 需停服) | `deploy/cleanup-content-v0.2.sh` 全文 | **无**(见 H6) |
| §9 对外文档:仍可见的元数据 | `docs/DATA-ASSETS.md §2` | — |
| §13.2 llms.txt Step 0(已装先 `anet update`,新机 curl\|sh + 手动验签路径) | `internal/aghub/web/llms.txt:84-133` | `llms_test.go:15` |
| §16 ANetHub CI:`no_federation` 减法、`taskboard` 加法,各自 `go test -tags` + 符号数双向检查 | `.github/workflows/ci.yml:24-104` | CI 本身(见 H9 的缺口) |

---

## 2. 与进度记录不一致之处

- 0013 "阶段 B1–B4 … A2A 卡片准入与注册表 API、JWKS、联邦 v2 卡片":**不成立**,见 H1–H4。
  `aghub.go:393-397` 的表注释也写着 "Not verified or indexed yet; admission is added separately"。
- 0013 "taskboard 改加法 tag":只有 hub 侧改了。daemon 侧仍是减法(`ANet/module/taskboard/*.go:1` 为
  `//go:build !no_taskboard`,ANet CI 矩阵 `ci.yml:108-114` 仍列 `no_taskboard`),见 H11。

---

## 3. 剩余工作(按优先级;P0 挡住 C5/D1/D2/E/F)

### H1 [P0] A2A 卡片准入(`/register` 的 `a2a_card`)— missing

现状:`hRegister` 在写入注册行之后调用 `s.store.putA2ACard(req.AID, req.A2ACard)`(`server.go:724-726`),
只检查"是 JSON 对象、≤64 KiB",随后原样入库并返回 `unverified`。

要做:
1. 在 `internal/aghub/a2acard.go` 中实现准入,替换 `putA2ACard`。建议签名:
   ```go
   func (s *Store) AdmitA2ACard(aid string, raw []byte, kel []identity.SignedEvent, now time.Time) (status, detail string)
   ```
   - 调用 `a2acard.Verify(raw, resolve, uint64(now.UnixMilli()))`。`resolve` 只对 `a == aid` 返回传入的 `kel`
     (注册时用本次提交、已校验延伸的 KEL),其他 AID 一律返回错误。随后核对 `v.AID == aid`。
   - 用 `SELECT seq, payload_hash FROM agent_a2a_card WHERE aid=?` 构造 `*a2acard.Mark`,调用
     `a2acard.CheckHighWater(stored, v.Mark())`:
     - `Advance`:在**同一事务**内 upsert 原字节,以及 `seq`、`payload_hash`、`verified_at`、`fed_seq`
       (取 `MAX+1`,供 H4 使用),并重建该 AID 的 `agent_skill`/`agent_tag`。
     - `Same`:原字节不同(换钥重签)时替换字节,否则只刷新 `verified_at`。做法与 `keys.go:126-138` 相同。
     - `a2acard.IsCode(err, CodeSeqRollback|CodeSeqFork)` → `conflict`;其余错误 → `invalid`,不入库。
       本地注册不会出现 `CodeKELUnavailable`。
2. 表结构(`aghub.go:398-402`)用 `ALTER TABLE ... ADD COLUMN` 加 `seq INTEGER NOT NULL DEFAULT 0`、
   `payload_hash BLOB`、`verified_at TEXT`、`fed_seq INTEGER NOT NULL DEFAULT 0`。沿用 `aghub.go:409-433`
   "忽略 duplicate column name" 的写法。新增
   `agent_skill(aid, skill_id, name, PRIMARY KEY(aid, skill_id))`,索引 `skill_id`;
   新增 `agent_tag(aid, tag, PRIMARY KEY(aid, tag))`,索引 `tag`。
   存量行(只可能来自开发库,wire-2 hub 从未上线)在 `migrate()` 末尾补验:验证不过的删除。
3. 状态常量(`server.go:543-547`):删 `CardStatusUnverified`,新增 `ok`/`unchanged`/`conflict`,
   与 `KeysStatus*` 同名同义。以下三处要**同批**改:
   - hub `wirecontract_test.go:395`
   - `wire2_test.go:784`(现在断言 `unverified`)
   - ANet `internal/hubapi/hubapi.go:193-223`:补 `CardStatus*` 常量;`RegisterRequest` 加
     `A2ACard json.RawMessage \`json:"a2a_card,omitempty"\``;`hubapi_test.go:56` 的字段清单同步
4. KEL 轮换后的重验:`hRegister` 收到延伸后的新 KEL 时,对已存卡重新 `Verify`;
   出现 `CodeKeyNotCurrent` 就把行标为不可列出(或删除),等待重签。否则注册表会继续列出一张用已退役
   钥签的卡。
5. §10.5 "有 A2A 卡的条目的 name/caps 从已验证卡片派生":准入成功时,用卡片 `name` 和 skill id 覆盖
   `agent.name` 与 `agent_cap`。可以复用 `putAgent` 的 cap 写入段(`aghub.go:489-541`,
   `validateCaps` 同样适用),`/agents` 的形状不变。

测试(新文件 `internal/aghub/registry_test.go`,外部包 `aghub_test`):
- 用 `a2acard.SignWithController(cardJSON, c, jku)` 签卡。card JSON 可参照
  `ANetCore/a2acard/helpers_test.go:23-62` 的 `baseCard`;它不导出,需要在 hub 测试里复制一份。
- 另用固定向量:`identity.SuiteController()` 注册,提交 `ANetCore/a2acard/testdata/golden-card.json`,
  断言 `ok`。
- 负例逐条(每条做 mutation):卡签给他人 AID、`tenant≠aid`、seq 回退、同 seq 不同载荷、未签名、
  超 64 KiB。另测"同卡重发 → `unchanged`"。
- 注册辅助函数:`registerBody(t, c, name, caps)` + `signedDo(t, srv, c, relayauth.ActionRegister, "POST", "/register", body)`,
  见 `wire2_test.go:771-781`。

陷阱:
- 0012 的默认值剥离缺陷在 `ANetCore/a2acard` 里,还没修。hub 只调 `a2acard.Verify`,C5/D1 修好后会自动
  跟上。hub 测试**不要**钉规范化字节,只做"签→验"往返,并使用 ANetCore 自带的金标。
- 签名覆盖的是卡片原字节。库里存、对外发都用原字节,不要用 `encoding/json` 重编码。
- 卡片自报的 relay `url` 可能不是本 hub 的公网地址:hub 只能从 `requestOrigin(r)` 推断公网地址,反代
  之后不可靠。所以不要据此拒收,只记日志。§10.6 对联邦的规则是"以卡片为准"。

### H2 [P0] 注册表 API `GET /a2a/v1/agents`、`GET /a2a/v1/agents/{aid}/card` — missing

放置位置:内核新文件 `internal/aghub/registry.go`,路由注册在 `Server.Handler()`(`server.go:260-352`)。
自动套上 `cors`/`wireContract`/`limitBody`;非 `/relay/` 路径不会被 426,外部 A2A 客户端不发
`X-ANet-Wire` 也能用。

- `GET /a2a/v1/agents?skill=&tag=&q=&cursor=&limit=`
  - 只返回满足以下三条的条目:卡片验证 OK;`AgentView.Browsable()`(`aghub.go:885`);存在
    `agent` 行(用 JOIN)。
  - 条目格式:`{aid, card: json.RawMessage(原字节), cardVerification:"ok", verifiedAt, homeHub, lastSeen, quiet, reviewCount, avgRating}`。
    后四项从 `ListAgents`/`LivenessOf`(`liveness.go:83`)取。H4 做完后并入联邦学到的 A2A 卡,
    `homeHub` 取卡片 relay url 或条目的 `home`。
  - `skill`/`tag` 走 H1 建的索引。`q` 对 name/description/skill name 做子串匹配(§10.5:只供 web UI;
    daemon 不发自由文本)。`cursor` 用不透明的 base64(最后一个 aid 或 rowid)。`limit` 默认 50,上限 200。
  - 包装层是 hub 的陈述,卡片本体原样放进 `card`。
- `GET /a2a/v1/agents/{aid}/card`:返回原字节,`Content-Type: application/json`。
  `ETag` 取 `"`+hex(sha256(raw))+`"`,另加 `Cache-Control: max-age=300`。`If-None-Match` 命中时回 304。
  验证未通过或无卡时回 404。
- 契约:hub `wirecontract_test.go` 钉条目字段名。ANet `internal/hubapi` 增加对应类型并钉字段
  (§17 "契约 … 注册表")。消费方是 D2 的 `list_agents`,按 skill/tag 取卡后在本地做子串匹配。

陷阱:
- admin 的 `HubDB.DeleteAgent`(`admin/hubread.go:226-237`)只删 `agent` 行,`agent_keys`、`agent_a2a_card`、
  `agent_cap`、`agent_card` 会留下孤儿行。注册表查询必须 JOIN `agent`,否则会列出被运营者删掉的 agent。
  deregister(`deregister.go:109-118`)删得是完整的。
- `hAgentCard`(`GET /agents/{aid}/card`)返回的是 ADP 卡,不要复用这个路径名。

### H3 [P0] `GET /agents/{aid}/jwks.json` — missing

- 用 `a2acard.JWKS(kel)`(`ANetCore/a2acard/jwks.go:20`)生成。它只包含当前密钥态,输出已是 RFC 8785 规范
  JSON,可以直接对字节算 ETag。KEL 取 `s.store.AgentKEL(aid)` 再 `identity.UnmarshalKEL`。未注册回 404。
  已停用的 AID 由库返回 `{"keys":[]}`。
- 路由写 `mux.HandleFunc("GET /agents/{aid}/jwks.json", s.hJWKS)`。Go 1.22 路由会按更具体的模式匹配,
  不会与 `GET /agents/{aid}` 冲突。
- 测试:与 `ANetCore/a2acard/testdata/golden-jwks.json` 比对(同样用 `identity.SuiteController()` 注册)。
  mutation:换钥之后旧 kid 不应再出现。

### H4 [P0] §10.6 `GET /fed/v2/cards` 与准入 — missing

- 发布端(federation 模块):在 `federation.go:343-350` 的 `Handler()` 加 `GET /fed/v2/cards`。
  `cmd/anet-hub/wire_federation.go:30` 已经把 `/fed/v2/` 整体挂到 `fed.Handler()`,不用改接线。
  条目格式为 `{format:"a2a-card/1", card, kel, keys, home, fed_seq}`。
- `Directory` 接缝(`federation.go:560-575`)新增两个方法,由 `aghub.FedDirectory`(`card.go:738`)实现:
  ```go
  A2ACardsSince(cursor int64, limit int, home string) ([]FedCardView, int64, error)
  AdmitFedA2ACard(peerAID string, format string, card, kel, keys []byte, home string) error
  ```
  发布侧只发 `visibility ∈ {federated, public}` 的本地卡,参照 `CardsSince`(`card.go:492-556`)的
  LEFT JOIN 写法,包括撤回。撤回的做法复用 `withdrawCard`/`fedWithdrawal`(`card.go:276-455`),
  否则对端永远删不掉。
- 接收端:新表 `fed_a2a_card(aid PK, seq, payload_hash, card, kel, keys, home, peer_aid, stored_at)`。
  准入顺序照抄 `AdmitFedCard`(`card.go:564-663`):
  1. 本地已注册则返回 `federation.ErrRefusedForNow`(sync 循环会停住游标)。
  2. `identity.ExtendsKEL(stored, kel)`。
  3. `a2acard.Verify`,resolver 返回条目里的 KEL。
  4. `CheckHighWater`。
  5. keys 走 `fedKeysToStore`。
  6. `home` 与卡片 relay url 不一致时以卡片为准并记录日志。
  7. 未知 `format` 直接跳过,游标照常前进。
- 同步:`SyncOnce`(`federation.go:739-...`)为 v2 另建游标表(例如 `fed_cursor_v2`,放在 federation.db)。
  对端返回 404 时视为"对端还没升级",不计 refused。v1 端点保留(§10.6)。
- `hKeysGet`(`keys.go:233-280`)第二来源要同时查 `fed_a2a_card.keys`。
  `Store.FederatedAgents`(`card.go:805`)与 H2 的列表要并入这些卡。
- 测试:在 `aghub/fedwire2_test.go:55-107` 的 `twoFederatedHubs` 上加 discovery 配置,可参照同文件
  `TestKeysTravelWithTheCardAndAreVerifiedOnArrival`(`:214`)的写法。覆盖:卡片跨 hub 出现在
  `/a2a/v1/agents` 且 `homeHub` 正确;KEL 回退/分叉被拒;同 seq 不同载荷被拒;撤回能传播;
  `no_federation` 构建的符号检查仍为 0。

### H5 [P1] wire-1 未投递明文行在迁移后留存 — partial(§9 relay 存储、SI-1)

现状:`migrateRelayV2` 把 `delivered_at IS NULL` 的 wire-1 明文行复制进新表(`relay.go:290-294`),
测试也把这当作预期(`wire2_test.go:528-570`,断言 "waiting" 行保留)。这些行对 wire-2 daemon 没有用处:
§3.6 第 1 步会按永久失败处理后 ack。它们只会在收件人上线,或 `created_at` 过了 14 天后才消失。
清理脚本删它们需要运营者手工给出 `--relay-before <升级时刻>`(`cleanup-content-v0.2.sh:129-139`)。
不给的话,SI-1 canary 在生产上最长会失败 14 天。

两种做法选一:
- (a)迁移时不复制,直接丢弃。改 `relay.go:290-294`,同步改测试断言。wire-2 daemon 本来也打不开这些行。
- (b)迁移时写一张元数据表,例如 `hub_meta(key TEXT PK, value TEXT)`,记下 `relay_v2_migrated_at`
  (unix ms)。清理脚本在没有 `--relay-before` 时默认取这个值。

(a) 更干净,也更符合决定 2;(b) 保留"迁移只做结构变更"的语义。选定后在 0013 里记一笔。

### H6 [P1] 清理脚本与周备份脚本不可测、未跑过 — partial(§9 末)

现状:
- `deploy/cleanup-content-v0.2.sh`、`deploy/hub-db-roll.sh` 都依赖 `sqlite3` CLI。本机没有这个命令
  (`which sqlite3` 返回 127),两份脚本都没有任何测试,CI 也不跑。
- `hub-db-roll.sh` 把路径写死成 `/data/projs/anet-hub/data`(`:30-31`),没法指向夹具目录。
- 仓库里没有 `hub-db-roll.service/.timer` 单元,脚本注释却引用了它(`:27`)。

对照 §9 清单,脚本覆盖齐全:relay 明文行、周备份、`admin/datasets/*`、`session`、`harvest_state`、
`taskboard.db*`、评价内容列、官方清单四段,另加 `guest_identity.kel`。缺的是可验证性。

要做:
1. `hub-db-roll.sh` 支持环境变量覆盖,例如 `HUB_DATA_DIR`,默认值不变。
2. 新增 Go 测试(建议 `cmd/anet-hub-admin/cleanup_script_test.go` 或 `deploy/` 旁的测试包):
   - 没有 `sqlite3` 时 `t.Skip`。
   - 用 `modernc.org/sqlite` 造旧 schema 夹具:参照 `internal/aghub/nocontent_test.go:346-367` 的
     `oldSchema`,以及 `wire2_test.go:532-548` 的 wire-1 relay 表。admin 侧造 `session`、`harvest_state`、
     带 `runtime/monitor` 的 `official_agent`,以及 `officials.json`、`datasets/hub-relay/**`、
     `taskboard.db*`、`hub-backup-*.db`。
   - 先 dry-run:断言有输出、没有删除、输出中不含 canary。
   - 再 `--apply`:systemctl 不存在时 `is-active` 返回非 0,脚本照常继续。断言所有文件里都搜不到 canary
     字节,WAL 已截断。
   - `hub-db-roll.sh` 伪造周日(可加 `FORCE_WEEKLY=1`),断言备份里 `relay_message` 为 0 行且不含 canary。
3. CI 在 `test` job 加 `sudo apt-get install -y sqlite3`,让上面的测试不被跳过。
4. 加固:`manifests_step` 里 `readfile()` 返回 BLOB,SQLite ≥3.45 会优先按 JSONB 解释 BLOB。
   现在能工作靠的是 3.45.1 恢复的兼容行为,建议改成 `json_each(CAST(readfile('$file') AS TEXT))`。
   `--apply` 前可以打印一行"已获产品负责人同意?"并要求交互确认,或另加 `--yes`。

### H7 [P1] hub 主机上旧的官方 agent 运维凭证通道 — partial(§9 admin 官方 agent、§15、[C39])

现状:代码层面的通道已删。旧版 admin 以 root 运行(unit 没有 `User=`,`deploy/anet-hub-admin.service`),
用 root 的 ssh 私钥登录官方 agent 主机(旧 `ops.go` 的 `sshArgs`,默认 `ssh_user=root`),
并用 `ADMIN_MONITOR_TOKEN` 登录官方 agent 控制台。这个令牌默认等于 `ADMIN_TOKEN`(旧 `cmd/anet-hub-admin/main.go:108`)。
删掉代码不会让这些凭证消失,§9 的清单和清理脚本也都没提。

要做(属于运维清单,执行前征求同意,归入阶段 G):
- 从官方 agent 主机的 `authorized_keys` 移除 hub 主机的公钥。hub 主机上如有专用私钥,一并删除。
- 从 admin unit 或 drop-in 删除 `ADMIN_MONITOR_TOKEN`,轮换各官方 agent 的控制台令牌;
  若 `ADMIN_TOKEN` 曾与控制台令牌相同,两者都换。
- admin 改用非 root 账户运行,unit 注释里已经提到。
- 建议在 `cleanup-content-v0.2.sh` 末尾的报告里列出这三项(只提示,不自动执行),
  并在 `docs/ADMIN.md` 写成检查清单。

### H8 [P1] nginx 请求体上限与新信封上限不一致 — diverged(§3.7 hub 侧)

- `Limits.sendBodyLimit()` 为 `base64Len(96 MiB) + 64 KiB`,约 128.06 MiB(`limits.go:95-98`)。
  `anet-hub -relay-max-envelope` 的帮助文字也要求反代上限 ≥ 4/3 倍。
- `deploy/nginx-hub.conf:58` 仍是 `client_max_body_size 96m`,注释引用的 `maxHubBody` 已经不存在。
  后果:约 72 MiB 以上的信封被 nginx 以 HTML 413 拒掉,daemon 拿不到 JSON 错误。按 Padmé 填充估算,
  接近 64 MiB 的附件就会碰到这条线。
- 改为 `client_max_body_size 129m`(或更大),并更新注释。`nginx-hub.conf.example` 是 512m,不受影响。
  可以加一个静态测试,读取 conf 数值并与 `aghub.DefaultLimits()` 比较,写法参照
  `cmd/anet-hub-admin/start_test.go:125` 读部署脚本的方式。
- 联邦 `/fed/v1/forward` 走对端 hub 的 nginx,同样受这个上限约束。

### H9 [P1] ANetHub CI 缺口 — partial(§16、§18)

- `go.mod` 仍 `require github.com/ANetResearch/ANetCore v0.14.0`。`GOWORK=off go build ./...` 实测失败:
  `module ... ANetCore@latest found (v0.14.0), but does not contain package .../seal`。按 §18,这要等 Core
  打 v0.15.0 之后与推送同批升级。在此之前,若有人在 GitHub 上开 PR,CI 必红。可以在 README 注明,
  或在 CI 里临时 checkout ANetCore 分支并写 `go.work`。
- `on.push.branches: [main, "anet4/**"]` 不含 `a2a-redesign-wip`,推送这个分支不会触发 CI。
- 缺 H6 的 `sqlite3` 安装步骤与脚本测试。
- `importguard_test.go` 里 `exec.Command("go","list","-deps",...)` 不带 `-tags`。optin job 用
  `-tags taskboard` 跑测试时,护栏检查的仍是默认构建。建议该测试读取 `testing` 的构建标签,或 CI 在
  optin job 额外跑一次 `go list -tags taskboard -deps ./cmd/anet-hub | grep -E 'ANetCore/(delegation|tsir)'`,
  断言为空。实测目前为空。
- webui job 只构建前端,不比对 `webui/dist/index.html` 与已提交的 `internal/aghub/web/index.html`。
  `scripts/build.sh:52-55` 本地有这个 cmp,CI 没有。建议加一步 `cmp`,防止源码改了而嵌入页没更新
  (§9 的 web 删改依赖嵌入页)。
- `joint.sh`/`joint-shell.sh` 进 CI、固定 ANetHub ref 都是 ANet CI 的事(§16),不在本仓。

### H10 [P2] web UI 接入指引、llms.txt 残留旧命令 — partial(§13.2、§20 对外陈述更正表)

- `webui/src/components/JoinSection.tsx:115-116` 写着"已装过也再跑一次,脚本会原地更新",
  与 §13.2 / llms.txt Step 0("已安装执行 `anet update`,新机器才 curl|sh")直接矛盾。
- `JoinSection.tsx:140,235` 以及 llms.txt 第 326、357 行仍用 `anet install --agent …`,而 §13.1 的命令是
  `anet agents wire`(llms.txt 第 132 行已经这么写,同一文件前后不一致)。
- "自动接单"示例没有提 `closed` 默认值、`peers allow`/`trust`。SI-5 下 exec 后端只对 trust 对端生效,
  照抄的用户会以为配好了。
- llms.txt 第 243、253 行"when both end, your signed receipt is issued / end once both sides agree",
  与 §2 "provider 单方完成"不一致。
- 依赖:`anet update`、`anet agents wire` 在 ANet 里还不存在(`cmd/anet/main.go` 只有 `install`、`peers`),
  要等 D2。**hub 的 llms.txt/web UI 必须与 D2 同批上线**,否则 Step 0 会指示一条不存在的命令。
- 做法:D2 落地后用 `joinPrompt` 的可测结构(`JoinSection.tsx:230`)把命令钉进 vitest。
  `llms_test.go` 增加"不含 `anet install --agent`"的断言。

### H11 [P2] daemon 侧 taskboard 仍是减法 tag — partial(§9 taskboard、§16)

- `ANet/module/taskboard/*.go:1` 为 `//go:build !no_taskboard`。要改成 `//go:build taskboard`,
  连同 ANet 的模块接线文件(`optin`/`mounts` 一类,找 `module/taskboard` 的 import 点)、
  `ANet/.github/workflows/ci.yml:108-114` 的矩阵(从减法行移到加法 job)、`scripts/build.sh --check`、
  CLAUDE.md 的 tag 列表(§16 末条)一起改。
- 理由:默认 hub 已经不编入 `/tasks/*`,默认 daemon 带着看板模块只会得到 404。
- hub 侧可以参照的写法:`cmd/anet-hub/optin_tags.go`、`optin_default_test.go`、`optin_taskboard_test.go`,
  以及 CI `optin` job。

### H12 [P2] SI-1 联调 canary — missing(属 E/F,ANet 仓)

- hub 单测已经覆盖 hub/admin 数据目录与响应的字节搜索:`nocontent_test.go`、`admin_test.go:159`、
  `wire2_test.go:476`。§1 的验收要求由 `ANet/scripts/joint.sh` 完成,那里目前没有 canary,
  也不启动 `anet-hub-admin`(`grep -i canary scripts/joint.sh` 无命中)。
- hub 侧已有的前提:admin 的 `-snapshot-every`、`-harvest-every` 可以调短
  (`cmd/anet-hub-admin/main.go:45-46`);四条已删路由由 mux 统一返回 JSON 404(`admin/server.go:132-142`)。
- 要补的断言:
  - 对 hub 数据目录、admin 数据目录、`/agents/{aid}`、`/fed/v1/reviews`、admin `/api/sessions*` 做字节搜索。
  - admin `/api/official/{id}/insights|acl|monitor/*|ops` 返回 404。
  - 结算路径请求体里 `resource/description/extra` 为空的结构化断言。hub 目前不校验也不存这三项,
    结算行只存 `receipt`(`facilitator.go:717-744`)。可选的纵深防御:hub 对非空值记计数或拒收,
    但设计没要求,不要擅自拒收。
  - mutation:关闭加密、恢复任一采集源,canary 必须命中;反向断言收件方能解出 canary。

### H13 [P2] 两份 install.sh — diverged(§13.2)

`ANetHub/deploy/install.sh`(166 行,较旧)与 `ANet/deploy/release/install.sh`(254 行)已经分叉。
§13.2 的加固(`main()`、只用 https、验 `release.json` 签名、防降级、`anet init`、`--agents`)
要落在唯一的源上。建议删掉 ANetHub 这份,或改成"镜像,来源见 ANet"的说明,免得 D2 加固后 hub 仍分发旧脚本。
`nginx-hub.conf:36-45` 只提供 `/install.sh` 与 `/dl/`,没有 `install.sh.sig`。llms.txt 用的是 apex 域名,
所以目前不冲突,但 hub 域名作为镜像时要补 `location = /install.sh.sig`。

---

## 4. 风险与需要设计确认的点

- **进度记录失真**:0013 把 B4 记为完成。后续任务依赖注册表(C5 网络卡片的发布、D2 `list_agents`、
  E 的官方 agent 标注),应先完成 H1–H4 再推进。建议修正 0013。
- **部署即删除**:wire-2 hub 首次启动时,`migrateContentV2` 会不可逆地删除评价内容列并 VACUUM
  (`content_v2.go:83-165`),`migrateRelayV2` 会删除已投递的中继行。所以"部署 hub"本身就是生产数据清理的
  一部分,征求同意(阶段 G)时要一并说明。首次启动还需要约等于库大小的空闲磁盘,并持有独占锁。
- **wire-1 明文行**(H5)未决时,SI-1 在生产上最长 14 天不成立。
- **官方清单字段**:设计写"只登记 `id/aid/hub/caps`",`Manifest` 另外保留了
  `schema/name/tier/product_line/summary/maintainer`(`admin/manifest.go:26-37`)。这些是描述性标签,
  不构成通道。建议设计接受,不必改代码。
- **taskboard 编入后的认证**:看板仍用 wire-1 挑战签名(`server.go:1241-1275` `VerifyAgentChallenge`/`verifyChallenge`,
  原像不绑定 hubAID,也没有重放缓存)。只有 `-tags taskboard` 构建才会暴露,而且编入本身就违背 SI-1,
  文档已写明。维持现状即可,但不要把它当作 v2 认证的范例。
- **admin 删除与恢复不完整**:`DeleteAgent` 留孤儿行(H2 陷阱)。`FullAgentRow`/`RestoreDeletedAgent`
  不含 `visibility`、`last_seen_at`、`agent_keys`、`agent_a2a_card`,恢复后可见性回到 `hub-local`。
  这是存量问题,不在 §9 范围,记录备查。
- **0012 卡片默认值剥离缺陷**:hub 准入(H1/H4)与 daemon 签名(C5)共用 `a2acard`。只要 ANetCore 没修,
  a2a-python 签的第三方卡在 hub 上会验签失败。H1 可以先做,但互通测试要等 C5 修完再加。
- **llms.txt / web UI 与 D2 的耦合**(H10):hub 的对外文案指向尚不存在的 CLI 命令。
- **session 浏览器残留**:`/api/sessions*`、`/api/harvest`、`harvest_state`/`session` 表、
  `-harvest-every` 在生产清理之后都是死代码(`admin/store.go:100-129`、`admin/server.go:124-126`)。
  可以在清理完成后的版本里删除,删除时同步 `admin_test.go:159` 的路由列表。

---

## 5. 速查

环境与命令:
```bash
source /data/projs/anet-dev/.anet-env.sh          # GOWORK=三仓 go.work
cd /data/projs/anet-dev/ANetHub
go test ./...                                      # 默认构建
go test -tags taskboard ./internal/taskboard/... ./cmd/anet-hub/...
go vet -tags no_federation ./... && go build -tags no_federation -o /tmp/lean ./cmd/anet-hub \
  && go tool nm /tmp/lean | grep -c internal/federation   # 必须为 0
GOWORK=off go build ./...                          # 目前必失败(H9),Core 打 tag 后应通过
```

测试夹具(均在 `internal/aghub/*_test.go`,包 `aghub_test`):
- `newHub(t)` / `newHubWithStore(t)`(`aghub_test.go:32,39`);`newHubAt(t, dir)`(`wire2_test.go:30`),
  返回 `(srv, store, *Server)`,数据目录可以检查。`openDB(t, dir)` 以直连方式打开 hub.db。
- `twoAgents(t)`(`aghub_test.go:1295`);`register(t, srv, c, name, caps)`、`registerBody`(`aghub_test.go:120,149`);
  `signedDo(t, srv, c, action, method, path, body)`、`signedRequest`、`signV2`
  (`wire2_helpers_test.go:40-89`);`hubAIDOf`(`aghub_test.go:1087`)、`getJSON`(`deregister_test.go:267`)。
- `testEnvelope(t, to, ct)`(结构合法的假信封)、`sealFrom(...)`(真信封)、`relaySend/relayPoll/relayAck`、
  `mintKeySet(t, c, seq)`、`publishKeys`(`wire2_helpers_test.go:95-201`)。
- `twoFederatedHubs(t, keyLookup)`(`fedwire2_test.go:55`),两个 hub 已按 main.go 的方式互联。
- `ownerGet(t, srv, c, action, path)`(`facilitator_test.go:26`),用于本人签名的 GET。
- 旧 schema 夹具:`nocontent_test.go:346` 的 `oldSchema`;wire-1 relay 表见 `wire2_test.go:532-548`。
- admin:`newTestServer(t)`(`admin/admin_test.go:70`,返回 `(*Server, *Store, *Harvester, provAID, reqAID)`)、`doReq/doReqFrom`,`testCanary` 常量(`:27`)。
- 卡片:`a2acard.SignWithController`、`a2acard.Verify`、`a2acard.CheckHighWater`、`a2acard.JWKS`、
  `a2acard.IsCode`。固定向量 `ANetCore/a2acard/testdata/{golden-card,golden-jwks,a2a-python-golden}.json`,
  配合 `identity.SuiteController()`。

需要同批修改的跨仓契约:
- hub `wirecontract_test.go`(`/register` 字段与 card_status 值,注册表条目字段)
- ANet `internal/hubapi/hubapi.go` 与 `hubapi_test.go`(`RegisterRequest.A2ACard`、`CardStatus*`、注册表类型)
- ANet `internal/daemon/hub_client.go:46-60` `RegisterWithHub`,在 C5 中带上 `a2a_card`
