# 0033 · 验证:模糊测试(daemon 范围)

日期:2026-09-28。对应:收尾阶段的模糊测试,范围 daemon——ANet 中来自不可信方的输入:对端封好的信封与其中每个
字段、A2A 投影里对端可写的字段、本机 A2A 接口的请求、控制面与 MCP 的请求体、agentwire 读到的既有配置文件、发布包
与官方清单、x402 报价与收据、hub 的应答。同时进行的 core、hub 两个范围另有记录。代码:工作树 `wt/fz-daemon`,分支
`wp/fz-daemon`,基于三仓标签 `integ/round6`(ANetCore `7b43f46`、ANet `b2ad2c8`、ANetHub `cf58c50`)。本机 8 核、
与其他任务共用,go1.26.6,`GOWORK` 为工作树的 go.work;Go 原生模糊测试,每个目标 `-parallel=2 -fuzztime=5m`,
同一时刻只跑一个模糊进程。

## 1. 结论一览

| 项 | 结果 |
|---|---|
| 目标 | 17 个 fuzz 函数,7 个包(§2),随本分支提交 |
| 运行 | 32 次(每次上限 5 min,失败的到失败为止),合计约 1,455 万次执行(§3) |
| 产品缺陷 | 6 处,均已修、各有普通单测回归(去掉修复即红)与保存的失败输入(§4):D1 x402 报价显示的金额/收款方与本节点签出的不同;D2 投影按对端交付物的"另一种读法"报 `anet.effect_status=OK`;D3–D6 agentwire 在工具读得了的配置上写出读不了的文件,或把工具读法下不是 anet 的条目当成 anet 的 |
| 崩溃(panic、越界、挂起、内存暴涨) | 0 |
| harness 自身的问题 | 3 个(判据的类型、"内核收据"的取值范围、worker 临时目录进了种子),已改,不算发现 |
| 修复后确认 | 每个修过的目标再跑 5 min 通过(§3 标"确认运行"的行);全量 `go test ./internal/... ./module/... ./cmd/...` 32 包通过;`go vet` 在默认、`no_mcp`、`no_a2a`、全部 `no_*`、`shell,taskboard` 下干净,`scripts/tagcheck.sh deps`(SI-8)通过 |

D1–D3 同根:encoding/json 按结构体解码时字段名不区分大小写、同名取最后一个,而客户端、a2a-go、JSON.parse、
本仓的 map 读法都按原样读。对端(或损坏的配置)写成两种读法的 JSON,本节点显示的是一个值、据以行事的是另一个。
修复是一个新的小包 `internal/jsonread`(`ReadsAlike`):据以付款、陈述效果、判定"这是 anet 的条目"之前,要求两种
读法一致。

## 2. 目标清单

17 个 fuzz 函数,分布在 7 个包;都随本分支提交,在普通 `go test` 下只跑种子与 `testdata/fuzz` 语料。

| 目标 | 文件 | 不可信输入 | 性质断言(除"不 panic、不挂起"外) |
|---|---|---|---|
| FuzzReceiveInner | `internal/daemon/fuzz_receive_test.go` | 真实发送方封好、签好的信封,内层每个字段由模糊器定:消息类型(delegate/message/status/result 与未知 ix)、发送方(对端/陌生人/第三个已注册节点)、ChatMsg 的 kind、正文、msgID、metadata JSON、附件(名/类型/数据/CID 对错/只给 CID)、StatusMsg 的 state 与 metadata、ResultResp 的 status、交付物、带不带有效回执、TaskDoc(目标、能力 id、args)、DelegateReq 的 Payment 与 ContextID、内层附带的 KEL 与密钥集字节、时间偏移 | 结果属三类之一且拒绝必有原因;非任务对端的信封不改动任务(状态、state_seq、消息数、回执、付款状态);请求方的消息不能把 provider 侧任务变成 completed/failed/rejected(end_request 除外);同一信封第二次不改任何东西、被拒的第二次不被接受;任务投影可编码、a2a-go 能读、SI-6 成立;存为 verified 的回执能在存下的交付物上重新验过 |
| FuzzReceiveEnvelope | 同上 | 信封位置上的任意字节(hub 信箱与直连两条路径),种子为真实信封 | 三类之一;打不开的信封在信箱路径为 P、直连路径为 T(F22) |
| FuzzHubAnswers | `internal/daemon/fuzz_hub_test.go` | hub 对以下请求的任意应答(状态码、wire 头、正文):信箱轮询、密钥查询、hub 身份、A2A 注册表、无卡目录、find、余额、单个卡片、注册 | 采信的密钥集属于所查 AID,存下的 KEL 能回放到该 AID;钉住的 hub 身份 KEL 回放到其 AID;未验证的卡片条目不带卡片、名称与 hub 的陈述 |
| FuzzControlRoutes | `internal/daemon/fuzz_control_test.go` | 控制面 47 条 JSON 路由的请求体(CLI、控制台、`anet mcp` 替模型转发的参数) | 标为 JSON 的应答是合法 JSON;5xx 必带 error 对象 |
| FuzzX402Quote | `internal/daemon/fuzz_x402_test.go` | provider 的报价 `x402.payment.required`、客户端所选选项、home 网络、字母大小写翻转位 | 入库的报价对"按原样读"和"本节点签名时读"是同一个(D1);所选选项与某个报价选项逐字段相同;`payableFirst` 只重排选项;canonicalJSON 是不动点 |
| FuzzX402Receipts | 同上 | provider 的收据列表 `x402.payment.receipts`、本节点核实过的交易 | 只有本节点核实过的结算以"自己的"身份陈述;provider 写的 verdict 不被采信;重写后仍是 JSON;截短不丢已核实项 |
| FuzzProject | `internal/a2ashape/fuzz_test.go` | 存储的任务里对端可写的一切:消息 metadata 与正文、对端 messageId、交付物、结果 metadata、附件名与类型、报价与收据、状态/付款状态/角色组合、大小写翻转位 | 投影可编码、a2a-go 能读、SI-6 成立;本节点陈述的键(角色、对端、state_seq、effect_status、receipt_verified、x402 状态与收据、skill)与对端消息 metadata 无关;effect_status 必是交付物或结果 metadata 按原样读出的值、或本节点的兜底(D2);对端的 messageId 不被采用;对端未经本节点核实的 payment-completed/收据不出现在 status.message;文件名经 SafeName |
| FuzzMessageJSON | 同上 | 本机客户端写的 A2A Message JSON | 读进来的 Part 恰有一种内容;能写回且往返不变 |
| FuzzServer | `module/a2a/fuzz_test.go` | 本机 A2A 接口的任意请求:方法、路径、Content-Type、A2A-Version、A2A-Extensions、正文、有无令牌(JSON-RPC 与 REST 两种绑定,含流式) | 无令牌一律 401 且不读正文;非 `application/json` 的 POST 正文从不被服务;没有 5xx;标为 JSON 的应答、SSE 的每个 data 都是 JSON;服务器日志里没有被 recover 的 panic |
| FuzzTools | `internal/mcpserv/fuzz_test.go` | 模型给 14 个 MCP 工具的任意参数,daemon 的任意应答 | 只调白名单路由;只有 submit_payment 付款、decision 为 submit、不带 purpose;任何工具不带 pay 标志;inbound_pending 只传出它点名的元数据字段 |
| FuzzCodexConfig | `internal/agentwire/fuzz_test.go` | 既有 `~/.codex/config.toml` | 以 tomllib 为判据:wire 前能读则 wire 后能读,且 `mcp_servers.anet` 是 anet 的条目;再 wire 不变;unwire 后仍能读(D6) |
| FuzzHermesConfig | 同上 | 既有 `~/.hermes/config.yaml` | 以 PyYAML safe_load 为判据:同上,另含 `wire --a2a` 的 a2a_agents 条目与两块的 unwire(D4、D5) |
| FuzzJSONConfig | 同上 | 既有 Cursor `mcp.json` / opencode `opencode.json`、大小写翻转位 | 以精确字段名(JSON.parse 语义)为判据:wire 后能读且条目在工具找的位置;"无事可做"时条目按工具的读法确是 anet 的(D3) |
| FuzzParseSignature | `internal/release/fuzz_test.go` | release.json.sig(armored SSHSIG) | 解析成功的签名重新编码再解析相同;通过信任校验的签名其公钥在信任集中、命名空间正确、ed25519 验证成立 |
| FuzzVerifyManifest | 同上 | release.json 与签名 | 解析成功的清单 JSON 往返不变、版本可比较、每个 asset 能被 AssetFor 找到;验证通过的清单由受信钥签且 key_fingerprint 与签名钥一致 |
| FuzzCompareVersions | 同上 | 两个版本串 | 反对称;相等当且仅当解析后相同 |
| FuzzManifest | `internal/official/fuzz_test.go` | 官方清单 manifest.json(用本轮生成的钥签名,占位符替换出正确指纹) | 每个条目通过 validate 且只按 AID 可查、到期即查不到;JSON 往返不变;发布命名空间的签名不能验官方清单 |

x402 报价与收据的解析同时经 FuzzReceiveInner(provider 的 status/result 携带)到达,FuzzX402* 是其中纯函数部分的
高速版本。hub 应答里的 hubapi 类型经 FuzzHubAnswers 到达。

agentwire 的判据:测试启动一个 python3 进程(tomllib、PyYAML),整个运行复用;机器上没有时解析判据跳过、其余仍跑。

## 3. 执行次数与覆盖增长

Go 的模糊测试不报语句覆盖率,只报"新覆盖输入"(new interesting,即带来新覆盖边的输入,是覆盖增长的代理指标)。
下表每行是一次 5 分钟运行(失败的运行到失败为止);"基线"是开跑时的语料数(种子 + `testdata/fuzz` + 之前运行
留在 GOCACHE 里的语料),"新增"是本次运行加入的覆盖输入数。同一目标多行时,后一行接着前一行积累的语料跑。

| 目标 | 开始 | 时长 | 执行次数 | 基线语料 | 新增覆盖输入 | 结果 |
|---|---|---|---|---|---|---|
| FuzzParseSignature | 09:30 | 5m1s | 3,819,212 | 6 | 22 | 通过 |
| FuzzManifest | 09:36 | 5m1s | 1,327,544 | 4 | 73 | 通过 |
| FuzzCompareVersions | 09:42 | 5m0s | 1,920,176 | 4 | 164 | 通过 |
| FuzzCodexConfig | 09:47 | 5m2s | 230,277 | 22 | 140 | 通过(D6 未修;未找到 D6) |
| FuzzX402Quote | 09:52 | 5m0s | 509,743 | 4 | 251 | 通过(D1 未修、尚无大小写翻转参数;未找到) |
| FuzzX402Quote | 09:58 | 5m0s | 463,939 | 256 | 122 | 去掉 D1 修复的记录运行(尚无大小写翻转参数):未找到 |
| FuzzX402Receipts | 10:03 | 5m0s | 569,933 | 5 | 289 | 通过 |
| FuzzCodexConfig | 10:09 | 11s | 3,528 | 167 | 1 | 失败 → `c4dde32b82242d32`:harness 自身的问题(判据把 `mcp_servers.anet` 当成必为表,文件里它是字符串时判据出错),改 harness,输入留作种子 |
| FuzzHermesConfig | 10:09 | 2s | 33 | 19 | 0 | 失败 → `771e938e4458e983`(D4,顶层是标量 `0`) |
| FuzzJSONConfig | 10:09 | 5m1s | 477,086 | 7 | 225 | 通过(D3 未修、尚无大小写翻转参数;未找到) |
| FuzzCodexConfig | 10:14 | 5m1s | 166,574 | 169 | 34 | 通过(D6 未修,加了真实 Codex 配置作种子;仍未找到 D6) |
| FuzzHermesConfig | 10:19 | 14s | 5,018 | 20 | 11 | 失败 → `0e833cad612c5598`(D5) |
| FuzzJSONConfig | 10:20 | 5m0s | 351,799 | 234 | 55 | 去掉 D3 修复的记录运行(尚无大小写翻转参数):未找到 |
| FuzzHermesConfig | 10:25 | 5m2s | 91,887 | 32 | 109 | 通过(D4、D5 修复后的确认运行) |
| FuzzCodexConfig | 10:30 | 5m1s | 176,132 | 207 | 36 | 通过(D6 修复后的确认运行) |
| FuzzJSONConfig | 10:36 | 5m1s | 398,728 | 9 | 194 | 去掉 D3 修复的记录运行(有大小写翻转参数):未找到——种子里的数据目录取自 `f.TempDir()`,各 worker 进程不同,"已是最新"路径走不到;改为固定路径后见 11:03 行 |
| FuzzProject | 10:41 | 6s | 702 | 10 | 2 | 失败 → `6fbc2ce209665083`(D2);大小写翻转参数加入后的第一轮 |
| FuzzX402Quote | 10:41 | 8s | — | 5 | 0 | 去掉 D1 修复、移开回归语料的记录运行:8 s 失败 → `d81bd632aa74aef9`(D1) |
| FuzzMessageJSON | 10:41 | 5m0s | 1,015,473 | 5 | 187 | 通过 |
| FuzzProject | 10:46 | 21s | 20,442 | 14 | 41 | 失败 → harness 自身的问题(把"内核的收据列表"设成了非数组;内核只给列表),改 harness,输入删除 |
| FuzzX402Quote | 10:47 | 5m1s | 557,629 | 8 | 158 | 通过(D1 修复后的确认运行) |
| FuzzProject | 10:52 | 5m1s | 99,116 | 55 | 368 | 通过(D2 修复后的确认运行) |
| FuzzServer | 10:57 | 5m1s | 292,116 | 22 | 209 | 通过 |
| FuzzJSONConfig | 11:03 | 26s | 220 | 203 | 0 | 去掉 D3 修复的记录运行(数据目录已改为固定路径):26 s 失败 → `25bf2032fdbd4357`(D3) |
| FuzzVerifyManifest | 11:03 | 5m1s | 438,297 | 5 | 17 | 通过 |
| FuzzReceiveInner | 11:08 | 5m2s | 20,149 | 18 | 22 | 通过 |
| FuzzReceiveEnvelope | 11:13 | 5m1s | 216,130 | 6 | 65 | 通过 |
| FuzzTools | 11:19 | 5m1s | 408,520 | 29 | 309 | 通过 |
| FuzzHubAnswers | 11:24 | 5m1s | 69,290 | 12 | 79 | 通过 |
| FuzzControlRoutes | 11:29 | 5m2s | 210,460 | 30 | 212 | 通过 |
| FuzzJSONConfig | 11:34 | 5m1s | 683,858 | 206 | 65 | 通过(D3 修复后的确认运行) |
| FuzzReceiveInner | 11:40 | 5m1s | 7,220 | 42 | 18 | 通过(第二轮,加了请求方真实 KEL/密钥集作种子) |

按目标合计(不含 8 s 即失败、未报执行数的那一行;另有两轮无效运行不计:FuzzX402Quote 在未修代码上因回归语料种子即红、
FuzzVerifyManifest 被人为中断,见 §6):

| 目标 | 运行次数 | 执行次数 | 新增覆盖输入 |
|---|---|---|---|
| FuzzParseSignature | 1 | 3,819,212 | 22 |
| FuzzCompareVersions | 1 | 1,920,176 | 164 |
| FuzzJSONConfig | 5 | 1,911,691 | 539 |
| FuzzX402Quote | 3 | 1,531,311 | 531 |
| FuzzManifest | 1 | 1,327,544 | 73 |
| FuzzMessageJSON | 1 | 1,015,473 | 187 |
| FuzzCodexConfig | 4 | 576,511 | 211 |
| FuzzX402Receipts | 1 | 569,933 | 289 |
| FuzzVerifyManifest | 1 | 438,297 | 17 |
| FuzzTools | 1 | 408,520 | 309 |
| FuzzServer | 1 | 292,116 | 209 |
| FuzzReceiveEnvelope | 1 | 216,130 | 65 |
| FuzzControlRoutes | 1 | 210,460 | 212 |
| FuzzProject | 3 | 120,260 | 411 |
| FuzzHermesConfig | 3 | 96,938 | 120 |
| FuzzHubAnswers | 1 | 69,290 | 79 |
| FuzzReceiveInner | 2 | 27,369 | 40 |
| 合计 | 31 | 14,551,231 | 3,478 |

## 4. 发现与修复

六处产品缺陷,分两类:按 Go 的读法与按原样的读法不一致(D1–D3),以及 agentwire 在工具读得了的配置上写出工具
读不了的文件(D4–D6)。行号是 `integ/round6` 上的;每条都有普通单元测试回归,用文件备份去掉修复后变红(不用
git stash),模糊器保存的失败输入留在 `testdata/fuzz/FuzzXxx/` 作回归种子。

| # | 位置(根因) | 输入 → 后果 | 发现 | 修复 | 回归 |
|---|---|---|---|---|---|
| D1 | `internal/daemon/x402task.go:902`(报价入库只要 `json.Unmarshal` 成 `payment.PaymentRequired` 就收);`:91`、`:1356` 付款时从同一结构体取选项 | provider 的报价选项 `{"amount":"1","AMOUNT":"900"}`(或 `payTo`/`PayTo`、`network`/`Network`、`accepts`/`ACCEPTS`):客户端、a2a-go、投影与 `quoteSummary` 按原样读成 1,本节点 PayTask 从结构体签 900——Go 按字段名解码时不分大小写、取最后一个 | FuzzX402Quote 在去掉修复的代码上 8 s(`d81bd632aa74aef9`,`ACCepts`/`pAYTO`) | 报价入库改为 `quoteOf`:读法不一致(`jsonread.ReadsAlike`)的报价与"没有可付方式"一样不入库;新包 `internal/jsonread` | `TestAQuoteThatReadsDifferentlyIsNotTaken`(5 个反例、6 个正例);语料 `case-variant-amount`、`d81bd632aa74aef9` |
| D2 | `internal/a2ashape/project.go:337`(`capResult` 按结构体读能力交付物) | 交付物 `{"status":"FAILED","Status":"OK"}`:客户端和核对回执(回执覆盖的正是这些字节)的人读到 FAILED,投影报 `anet.effect_status=OK`;`{"STATUS":"OK"}` 同样报 OK | FuzzProject 6 s(`6fbc2ce209665083`,`{"StAtus":"0"}`) | 读法不一致的交付物不给出自己的效果状态:任务按不看它时所知的说,终态为 UNVERIFIED(SI-6 的"不知道") | `TestADeliverableThatReadsTwoWaysDoesNotStateItsEffect` |
| D3 | `internal/agentwire/jsonobj.go:181`(`decodeLoose` 即 `json.Unmarshal`,`matches`/`ours` 用它) | Cursor/opencode/Claude 的 `{"Command":"<anet>",…}` 被报"已是最新"而工具找不到 `command`;`{"command":"x","Command":"<anet>","args":["mcp"]}` 被当成 anet 的条目改写(工具实际运行 x) | FuzzJSONConfig 在去掉修复的代码上 26 s(`25bf2032fdbd4357`) | `decodeLoose` 只认读法一致的条目;这样的条目按"不是 anet 写的"报冲突 | `TestAnEntryTheToolReadsDifferentlyIsNotAnets` |
| D4 | `internal/agentwire/yamlblock.go:158`(键不存在时一律 `appendBlock` 到文件尾) | Hermes 的 config.yaml 顶层不是映射(`0`、`~`、列表、流式映射)或以文档结束符 `...` 收尾:追加的 `mcp_servers:` 让 PyYAML 读不了整个文件 | `...`:搭 FuzzHermesConfig 时种子即红;顶层标量:FuzzHermesConfig 2 s(`771e938e4458e983`) | 顶层第一行内容不是 `键:` 时报冲突(`topMapping`);有结束符时插在结束符前(`appendYAMLBlock`,unwire 仍还原原文) | `TestHermesKeyIsNotAppendedWhereYAMLCannotTakeIt` |
| D5 | `internal/agentwire/yamlblock.go:170`(`mcp_servers:` 的值只排除了列表) | `mcp_servers:\n  0`、`mcp_servers:\n  {fs: …}`:anet 的条目被插进标量/流式映射之下,文件读不了 | FuzzHermesConfig 14 s(`0e833cad612c5598`) | 首个子行不是 `键:` 时报冲突 | 同上(3 个用例) |
| D6 | `internal/agentwire/codex.go:151`、`:184`(按行数 `"""`/`'''` 的奇偶跟踪多行串);`:194`、`:207`(键不反转义) | `a = '"""'`、`x = 1 # """` 之后的真 `[mcp_servers.anet]` 被当成串内容跳过;`[mcp_servers."an\u0065t"]` 不被认作 anet;结果都是追加第二张 `[mcp_servers.anet]`,Codex 读不了整个配置。反向:多行串里的 `\"""` 被当成结束,把串里的表头误报冲突 | 以 tomllib 判据对 harness 的已知弱点做定点探测;两轮 5 min(第二轮加了真实 Codex 配置作种子)模糊器均未自行变异出 | 逐字符跟踪字符串与注释(`tomlScan`、`closeMulti`),键按 TOML 规则反转义(`tomlUnescape`),`splitDotted` 跳过转义 | `TestCodexConflictBehindStringsAndEscapes`(6 反例、3 正例);语料 4 条 |

D1 最要紧:它让"客户端同意付的金额"与"本节点签出的金额"分开。自动档、代理档的限额仍按签出金额判(`holdForOperator`
与限额检查用的是结构体里的值),所以越过限额的部分仍会交给运营者;在限额以内,客户端看到 1、签出的是限额内的任意值。

发现的方式。D1–D3 在 Go 的字节变异下很难出现:把 `payTo` 改成 `PayTo` 只在一个位置、一种取值上成立,且不带来新覆盖,
模糊器不会保留中间结果——FuzzX402Quote 在未修代码上两轮 5 min、FuzzJSONConfig 一轮 5 min 都没有找到。给这三个目标
加了一个 `flip uint16` 参数(翻转输入里第 flip 个字母的大小写)后,三者都在 30 s 内被找到;这个参数随 fuzz 函数一起
提交。FuzzJSONConfig 起初还有一个 harness 问题:种子里的数据目录取自 `f.TempDir()`,而每个 fuzz worker 进程的临时
目录不同,种子里的"已是最新"条目到了 worker 里就不再是最新,永远走不到 D3 的路径;数据目录改为固定路径后才找到。

同时记下(未改):

- `offeredOption`(`x402task.go:1649`)只读客户端所选选项的第一个 JSON 值,`{"a":1} 垃圾` 与 `{"a":1}` 匹配同一选项。
  付的是入库报价里下标为 i 的那个选项,不是客户端的字节,无害。
- daemon 的 `resultState`(`delegation.go:1612`)仍按结构体读能力交付物的 `status` 定任务状态。D2 之后投影不再据此报
  OK;状态(completed / failed)本就是 provider 的说法,SI-6 下 completed 不蕴含效果 OK,故未改。
- 本机 A2A 接口对 `application/a2a+json`(a2a-tck 附带的 A2A RC v1.0 规范示例用的 REST 媒体类型)答 415,是设计
  §11.4 的规定(只收 `application/json`),不是缺陷;与新版客户端互通时需要注意。
- `release.json` 与官方清单同样按结构体(不分大小写)解码,而 install.sh 按原样读;两者都由发布钥签名,不是不可信
  输入,未改。
- 已知的 SI-6 红队项 F3(结果元数据 `anet.effect_status=OK` 在交付物不说时被采用,`si6_redteam_test.go:142`)按设计
  §395"有结果时一律取结果……其次结果元数据"仍然成立;FuzzProject 的性质断言把它当作允许的来源,没有重复报告。
- 跑全量单测时 `TestRedteamClosedNodeKELReplayExhaustion`(按耗时比例断言"每个信封 ≥3 次 KEL 回放")在模糊进程同时
  占用 CPU 时算出 1.5 而失败,单独重跑两次均通过;与本次修改无关,是计时类用例在负载下的不稳定。

## 5. 未能覆盖的部分

- **接收流水线的吞吐**。FuzzReceiveInner 每次执行要封装、签名、解封、回放 KEL、写 SQLite、再投递一次并投影,约
  60–70 次/秒,两轮 5 min 合计 27,369 次,远少于纯解析目标的百万级;带有效回执的结果、带报价的状态、
  带 x402 载荷的请求方消息都在种子里,但离这些种子较远的组合(比如付款流程中途的各状态迁移)覆盖有限。
- **seal.Seal 不肯封的内层**。KEL 解码不了、时间窗超限的内层,经 ANetCore 的 API 封不出带有效签名的信封,
  FuzzReceiveInner 遇到时跳过;接收方 `seal.Open` 按同样的规则拒绝,这部分属于 core 范围的目标。原始字节目标
  (FuzzReceiveEnvelope)的变异几乎都止于外层解码与 AEAD,不会到达内层——这是预期的,内层由 FuzzReceiveInner 负责。
- **控制面的六条路由**:`/shutdown`、`/hub-register`、`/hub-leave`、`/autoreply`、`/autoreply-test`、`/pull`
  没有纳入——它们停掉 daemon、换 hub、配置要执行的程序、或按请求体写本机路径;请求体里有名字含 path、dir、file、
  attach、url、exec、command、cmd、hub 的成员时也跳过(不让模糊器读写本机文件或向任意地址发请求)。阻塞调用
  (`/tasks/send` 不带 return_immediately、`/tasks/wait`)以 1 s 截止,只覆盖到解析与首段处理。
- **本机 A2A 接口**跑在 fake TaskSeam 上(`module/a2a/fake_test.go`),覆盖 HTTP、两种绑定与 a2a-go 的请求解析;
  内核一侧的 TaskSeam(taskseam*.go)由 FuzzControlRoutes 的 `/tasks/*` 与 FuzzReceiveInner 间接覆盖,没有经 A2A
  接口直通内核的目标。长连接流只在任务到终态或 3 s 截止前读取。
- **agentwire**:Claude Code 的条目经 `claude mcp add` 命令写入(`Options.Run`),没有模糊;AGENTS.md、SOUL.md、
  CLAUDE.md 的 Markdown 托管块不被工具解析,只做了往返,没有判据。YAML 的判据是 PyYAML(Hermes 用的库),其他 YAML
  实现(1.2 语义)的差异未覆盖。
- **release**:内嵌的 allowed_signers 不是不可信输入,未模糊;install.sh 的 awk 解析未覆盖(release.json 与官方
  清单都要先过发布钥签名)。
- **不在本范围**:module/p2p 的传输帧(core/hub 范围之外、也未列入本范围)、ANetHub 的服务端解析(hub 范围)、
  ANetCore 的 seal/identity/a2acard(core 范围)。
- **覆盖率数字**:Go 原生模糊测试只报"新覆盖输入"计数,不报语句覆盖率;本记录以它作为覆盖增长的代理,未另做
  覆盖率剖面。
- **变异的盲区**:D1–D3 说明,字段名的大小写变体这类"不带来新覆盖、只在一个位置一个取值上成立"的输入,Go 的
  字节变异几乎碰不到;本轮只在三个目标上加了大小写翻转参数。其他以 Go 结构体解码对端 JSON 并据以行事的地方
  (hubapi 应答、PaymentPayload 在 provider 侧的解析)没有这个参数,也没有"按原样读"的判据——前者只有本节点一个
  读者,后者由 hub 按签名的授权结算,未见双读者分歧的后果,但没有用模糊测试证明。

## 6. 提交

ANet 分支 `wp/fz-daemon`(未推送;ANetCore、ANetHub 无改动):

- `f06ceb8` fix(daemon, a2ashape): 对端 JSON 只在"各方读法一致"时才据以付款与陈述效果(D1、D2,新包 `internal/jsonread`)
- `2dda32a` fix(agentwire): 损坏或刁钻的用户配置不再被 wire 写成工具读不了的文件(D3–D6)
- `cf3ae4b` test: daemon 范围的模糊测试目标(17 个)与发现的回归语料
- `c70990f` fix(agentwire): tomlPath 注释里的转义键例子写回原样(2dda32a 的注释与说明里 `\u0065` 被吞)
- 本记录

复现:`source wt/fz-daemon/env.sh`,在包目录下 `go test -run '^$' -fuzz '^FuzzXxx$' -fuzztime 5m -parallel=2 .`;
agentwire 的三个目标需要 python3 带 tomllib 与 PyYAML(没有时解析判据跳过)。一次运行被人为中断(给卡住的
worker 发 SIGQUIT 看栈,实为它在最小化新输入)而记成"挂起",其保存的输入重跑通过、已删除,该运行不计入 §3。
另一轮 FuzzX402Quote 记录运行(10:35)没有移开 `testdata/fuzz` 里的回归语料,在未修代码上种子即红,无效;移开后重做,
即 §3 的 10:41 行。

## 7. 复核(同日)

复核者逐条核对了 §4 的修复提交,方法与结论如下。

- **去掉修复是否变红。** 用文件备份做替换,跑对应回归与 fuzz 种子,跑完用 `cmp` 核对已还原;没有用 git stash。
  - D1、D2、D3:分别把 `quoteOf`、`newProjector`、`decodeLoose` 里的 `ReadsAlike` 短路掉。三者各自的单测和
    `FuzzX402Quote`、`FuzzProject`、`FuzzJSONConfig` 的种子都变红。
  - D4、D5:yamlblock.go 整文件换回,`TestHermesKeyIsNotAppendedWhereYAMLCannotTakeIt` 的 12 个子测试和
    `FuzzHermesConfig` 的语料都变红。
  - D6:codex.go 整文件换回,`TestCodexConflictBehindStringsAndEscapes` 的 6 个子测试和 `FuzzCodexConfig` 的语料都变红。
- **D1–D3 的共同修复有一处漏洞,已补。** `ReadsAlike` 只比较 Go 重新编码出来的成员。encoding/json 会把同一字段的
  每个大小写拼写都读一遍,以最后一个为准;而 omitempty 字段读成零值后不会被编码,于是没有东西去比它。例:
  `{"type":"sse", …, "TYPE":""}`,Go 读成 stdio 条目,Claude Code 读成 SSE,wire 却当它是已是最新的 anet 条目,只补写了
  技能文件。报价里的 `maxTimeoutSeconds`、`extra` 同理,但这两个字段不涉及付多少、付给谁。修复(ANet `0a7c08f`):
  - 比完 want 的成员之后再检查 got。如果有两个以上成员的名字按 encoding/json 的规则折叠后相同(ASCII 转大写,其余
    码点取折叠环里最小的,所以 KELVIN SIGN 与 k 同组),其中没有一个出现在 want 里,且至少一个值非零,就判为读法不一致。
  - 只有一个这样的成员时,说明 Go 根本不读它,照旧放过。
  - 两个拼写都是 Go 不认识的名字时也会被拒:只看字节分不出它们是不是被藏起来的字段,取保守一侧。
  - 回归:`TestReadsAlike` 增加 7 例;新增 `TestAnOmitemptyMemberSpelledTwiceIsNotAnets`,断言 Claude 条目报冲突、
    文件不动。用文件备份把新检查短路,两者都变红。修复后 `FuzzX402Quote`、`FuzzJSONConfig`、`FuzzProject` 各跑
    30 秒,无发现。
- **普通 go test 下的 fuzz 函数。** 17 个目标只跑种子和 testdata 语料,连跑两次都通过。
  - 最慢的是 `FuzzControlRoutes`,约 1.6 s,来自阻塞类路由的 1 s 上下文截止;其次 `FuzzReceiveInner` 约 1.3 s。
    daemon 包合计约 3.8 s。
  - agentwire 的三个目标依赖 python3 以及 tomllib、PyYAML;没有这些时,解析判据会跳过。D4–D6 的普通单测不依赖
    python,所以在没有 python 的 CI 上这三处修复仍有覆盖。
- **复核后的全量。** ANet `./internal/... ./module/... ./cmd/...` 全部通过;`go vet` 无问题。

复核新增提交(未推送):ANet `0a7c08f`(fix(jsonread):omitempty 字段被后一个大小写变体读成零值时不再算"读法一致")。
