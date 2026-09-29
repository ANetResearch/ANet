# A2A 社区贡献草稿(`docs/a2a/`)

对应设计文档 `docs/A2A-DESIGN-zh.md`(r3)§19。本目录是 anet 回馈 A2A 生态的八份草稿,面向上游社区,
正文一律英文;本文件是中文索引。

> **全部是草稿,均未提交(暂缓:负责人决定"继续完善、仍不提交",见 `docs/notes/0032`,2026-09-29)。**
> 任何一份对外提交(开 issue、发 PR、发安全通告、在社区讨论区贴出)之前,都要先得到产品负责人逐项同意
> (设计 §2 "对外提交与发布"、§20 阶段 G)。每份草稿文首标 "NOT SUBMITTED — on hold per product owner
> (more testing first)"。
> 复现程序在 `submissions/repro/`(每个是独立小程序,用上游 SDK 本身复现,不依赖 anet);两条安全问题的
> advisory 草稿是 `submissions/advisory-a2a-go-A3.md`、`advisory-a2a-go-A9.md`、`advisory-a2a-js-J1.md`;
> 三份规范提案另有 ADR 格式草稿 `submissions/adr-relay-binding.md`、`adr-registry-api.md`,
> SecurityScheme 提案 `proposal-securityscheme.md` 本身即 ADR 格式。
> **许可证:Apache-2.0。** 本目录全部文件(规范文本、绑定草案、提案、scheme 文档、issue 草稿及其中的
> 测试向量与代码片段)只按 Apache License 2.0 授权,不带 ANet 开源许可证的附加条件(ANet `LICENSE`
> 附加条件第 3 条;本目录另放 Apache-2.0 原文 `LICENSE`)。参考实现(ANetCore、ANetHub、ANet 中实现这些草稿的代码)
> 在各仓仍按 ANet 开源许可证授权;由 Agent Network Research(或经其书面许可)提交给 A2A 上游(`a2aproject/*`、
> `google-agentic-commerce/a2a-x402`)的部分,以提交时的形态按 Apache-2.0 授权(`LICENSE` 附加条件 3b),
> 满足 A2A *官方* 扩展与绑定强制 Apache-2.0 的要求(A2A `docs/topics/extension-and-binding-governance.md`)。

## 索引与状态

| 文件 | 内容 | 目标去处 | 草稿状态 | 所依赖的实现 |
|---|---|---|---|---|
| `relay-binding.md`(ADR 草稿 `submissions/adr-relay-binding.md`) | 中继协议绑定规范草案(`https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1`):tenant=AID 路由、`SealedEnvelope`/`SealedInner` 的 CDDL、签名原像、Padmé、HPKE 参数、relayauth v2、hub 端点与错误码、操作映射(经中继 / 本地回答 / 不支持)、服务参数、错误映射、store-and-forward 下的流式语义、安全考虑与已知局限 | `a2aproject/A2A` 提案 issue → 有 maintainer 赞助后建 `experimental-cpb-anet-relay` 仓库 | **暂缓**;已按 v0.2.1 实现复核 | 信封、密钥集、relayauth v2、hub 中继端点、任务镜像与事件总线、本机 A2A 面、`a2a.serviceParameters`、卡片 `url`=hub 基址+`/relay`、`anet.cancel_requested`、同任务付款流均已实现;仅 provider 侧 `ExtensionSupportRequired`/`VersionNotSupported` 两条回复仍 **(designed)** |
| `registry-api.md`(ADR 草稿 `submissions/adr-registry-api.md`) | 注册表 API 草案:`GET /a2a/v1/agents`(查询参数、条目形状、包装层是 hub 陈述)、卡片原字节与 ETag、JWKS、`/fed/v2/cards`、卡片准入规则与高水位 | `a2aproject/A2A` 讨论 issue(规范目前明言不规定注册表 API) | **暂缓**;字段拼写/ETag/`card_status` 已按 v0.2.1 对齐 | 列表、卡片(含 `card:lookup`)、JWKS、`/fed/v2/cards`、`card_status`(`ok`/`unchanged`/`conflict`/`invalid`/`absent`/`withdrawn`)、准入与高水位均已在 ANetHub 实现并在生产两 hub 运行 |
| `x402-scheme-anet-credit.md` | `anet-credit` x402 scheme 文档(按 `a2a-x402/schemes/` 格式):托管性质、PaymentRequirements/PaymentPayload、授权对象 CDDL、签名、nonce 与窗口、绑定值、facilitator 职责、收据、跨 hub、`errorReason` 常量与 a2a-x402 错误码映射、两处偏离(`hub:<aid>` 与 CAIP-2;本地签名服务接受未签名的所选项) | `google-agentic-commerce/a2a-x402` 的 `schemes/`(实验性 scheme;提交时改名 `scheme_anet_credit.md`) | **暂缓**;已按 v0.2.1 实现复核 | facilitator、授权/收据、`pay_bind`、商户核对、`errorReason`→错误码映射、§8.7 本地签名流程均已实现(soak 中 676 笔付费任务各结算一次) |
| `issue-a2a-go.md` | a2a-go v2.6.0 的 13 条 issue 草稿(A1–A13),每条附复现代码、观察结果、影响、修复建议;2026-09-29 上游复核后 A1 改为在已有的 a2a-go #445 下评论、A11 按 A2A v1.0.1 撤回、A12 暂缓、A13 不提交(归 TCK),见 `docs/notes/0032` | `a2aproject/a2a-go`;**A3、A9 走 GitHub Security Advisories(`submissions/advisory-a2a-go-A3.md`、`-A9.md`)** | **暂缓**;A1–A10、A12 各有独立复现 `submissions/repro/a2a-go/`,2026-09-29 用 `ebf17c5` 复跑 | — |
| `issue-a2a-python.md` | a2a-sdk(Python)1.1.5 的 2 条 issue 草稿(P1 JSON-RPC 流式调用的非 200 错误读不出 A2A 错误名,附三个 SDK 的对照表;P2 `TaskUpdater.submit()` 作首个事件使任务失败),来自 `docs/notes/0035` 的真实客户端互通 | `a2aproject/a2a-python` | **暂缓**;复现 `submissions/repro/a2a-python/`,2026-09-29 复跑 | — |
| `issue-a2a-js.md` | @a2a-js/sdk 1.2.1 的 3 条 issue 草稿(J1 卡片规范化丢掉已解析卡片的 oneof 成员、签名不覆盖 `securitySchemes`;J2 卡片解析器按 URL 引用拼接 well-known 路径;J3 Node 默认 fetch 300 s 使长阻塞调用失败),来自 `docs/notes/0035` | `a2aproject/a2a-js`;**J1 走 Security Advisories(`submissions/advisory-a2a-js-J1.md`);上游同缺陷已公开 #663、修复 PR #664 未合** | **暂缓**;复现 `submissions/repro/a2a-js/`,2026-09-29 复跑 | — |
| `issue-a2a-x402.md` | a2a-x402 v0.2 规范的 13 条 issue 草稿(X1–X12 加 X13:x402 对象应为 I-JSON):激活头、x402 版本、A2A 1.0 示例、状态机缺口、一个任务付两次、签名服务委托、错误码、收据出现时机、传输安全措辞、对 facilitator 的数据最小化、`required: true`、非链网络标识、大小写异名跨语言读法不一 | `google-agentic-commerce/a2a-x402` | **暂缓**;X1、X13 有独立复现 `submissions/repro/a2a-{go,x402}/` | — |
| `submissions/` | 待提交文本(`01-a2a-go.md`、`02-a2a-x402.md`、`03-a2a-partners.md`)、三份 advisory 草稿、两份 ADR 草稿、复现程序 `repro/` | 各上游 | **暂缓**;每一份需 PO 逐项同意 | — |
| `proposal-securityscheme.md`(ADR 格式,即三份规范提案之一) | `SecurityScheme` 新变体提议 `SenderSignatureSecurityScheme`(按 A2A ADR 模板):为何现有五种 scheme 都不适用、候选方案比较、proto 改动、规范文字、profile 要求、兼容性、与 #1829 等开放提案的关系 | `a2aproject/A2A` 规范变更提案(先在 #1829 评论) | 完整初稿 | anet 当前网络卡片不声明 `securitySchemes`(设计 §10.1);本提议是上游补齐的路径 |

a2a-go 的 A1–A10 复现是在 scratchpad 里用独立 Go module(`replace` 指向本地 a2a-go 检出)完成的,
代码片段已写入草稿;未把复现程序放进本仓库,以免给 ANet 引入 a2a-go 之外的构建目标。

## 对外提交的前置条件

1. **产品负责人同意**(每一份提交各一次)。许可证已定(2026-09-28,`docs/notes/0027` G1.2):规范文本、
   scheme 文档与 issue 草稿是 Apache-2.0;参考实现代码提交上游时按 Apache-2.0 贡献。提交时去掉各草稿文首的
   "NOT SUBMITTED" 与许可证说明块;官方扩展/绑定仓库的贡献许可声明(governance "Contributor License Grant")由提交人确认。
2. **标记为 (designed) 的条目落地或删除**:v0.2.1 发布后,`relay-binding.md` 只剩 §11 provider 侧
   `ExtensionSupportRequired`/`VersionNotSupported` 两条回复仍 (designed);`registry-api.md`、
   `x402-scheme-anet-credit.md` 的 (designed) 已全部落地(2026-09-29 复核)。提交前再核对一遍这两条。
3. **注册表实现到位**:`registry-api.md` §3 的列表、卡片、`card:lookup`、JWKS、`/fed/v2/cards` 与
   `card_status`(`ok`/`unchanged`/`conflict`/`invalid`/`absent`/`withdrawn`)已在 ANetHub 实现并在生产两
   hub 运行;字段拼写、ETag(十六进制 SHA-256)、分页(`nextCursor` 仅在有下一页时出现)已按实现回填。
4. **补齐测试向量**:`relay-binding.md` §14 列出的缺口 —— `DelegateReq`/`ChatMsg`/`StatusMsg`/`ResultResp`
   全字段向量、固定密钥下的端到端交换记录(delegate → status → result)。
5. **跨 SDK 验证**:A1(默认值剥离)已用独立复现核实。`submissions/repro/a2a-go/a1x-cross-sdk-vectors`
   用 a2a-go 签十份卡,`../a2a-python/a1x_verify.py`、`../a2a-js/a1x-verify.mjs` 验:a2a-python 1.1.5 与
   a2a-js 1.2.1 都按 §8.4.1 rule 1 处理默认值(与 a2a-go 不同),两者又都多剥了 rule 1 保留的两处(REQUIRED
   的空 `description`、`Struct` 内的空串);结果已写入 A1 的对照表(2026-09-29)。
6. **按各仓库流程提交**:a2a-go 的 PR 按其 `CONTRIBUTING.md`(先开 issue 讨论方案;本地检出里没有
   写明 CLA 要求,提交前到 GitHub 上再确认一次);A2A 主仓库要求 Conventional Commits 与 markdownlint,
   官方扩展/绑定仓库另有 governance 文档里的贡献许可声明;安全类(A3、A9)只走 Security Advisories。
7. **URI 命名空间**:`agentnetwork.org.cn/a2a/...` 下的绑定与扩展 URI 按 A2A 惯例只是标识符,不要求可访问;
   若产品负责人希望这些 URI 可解析为文档,需要在官网安排路径。进入 A2A 官方层级后 URI 会改为
   `https://a2a-protocol.org/bindings/...`,届时绑定需要出 v2 标识(URI 是签名卡片的一部分)。
8. **敏感信息检查**:草稿中不含私钥、内部主机名、生产数据;提交前再检查一遍示例 AID(示例里的
   `bafyreicg3p…` 是 ANetCore 的冻结测试身份 `identity.SuiteController()`,不是生产身份)。

建议的提交顺序:先 a2a-go 的小而明确的公开 issue(A5–A8、A10),再 a2a-x402 的规范更正(X1–X4),
然后是安全通告(A3、A9);三份提案(中继绑定、注册表 API、SecurityScheme)最后,且在参考实现完成、
全量验证(阶段 F)之后。

## 与其他工作包的接口点

这些是草稿里写成规范、但由其他工作包实现的约定;实现时如有改动,需要回改草稿。

| 约定 | 草稿位置 | 负责工作包 |
|---|---|---|
| 网络卡片中继接口 `url` = hub 基础 URL(https 源 + 可选路径前缀,无尾斜杠),端点相对它解析;`tenant` = AID;`protocolVersion` = `"1.0"` | relay-binding §4 | C5(daemon 生成网络卡片) |
| 网络卡片不输出任何取默认值的非必填字段(如扩展的 `"required": false`),必填列表非空 | registry-api §5.1、issue-a2a-go A1 | C5、D1(代理卡片) |
| 只含中继接口的卡片 `capabilities.streaming` 建议为 `true`(草稿列为开放问题 Q4) | relay-binding §12、§17 | C5 |
| `a2a.serviceParameters`:JSON 对象;`A2A-Extensions` 为 URI 数组,`A2A-Version` 为字符串;缺省版本按 `1.0`;放在 `DelegateReq.Metadata` / `ChatMsg.Metadata` | relay-binding §10.4 | D1(`module/a2a`)、C5 |
| provider 以 `status{rejected, anet.a2aError: "<错误名去掉 Error 后缀>"}` 报告 `ExtensionSupportRequired`、`VersionNotSupported`(与已实现的 `"TaskNotFound"` 命名一致) | relay-binding §11 | D1、C 系列 daemon 接收侧 |
| `GetExtendedAgentCard` → `UnsupportedOperationError`(卡片不声明 `capabilities.extendedAgentCard`,规范 §3.3.4 对此是 MUST);推送 4 个操作及带推送配置的 SendMessage → `PushNotificationNotSupportedError` | relay-binding §10.1、§11 | D1 |
| 付款已提交后的取消:本地状态不变,任务 metadata 带 `anet.cancel_requested: true` | relay-binding §10.6 | D1、C5 |
| 注册表列表响应 `{agents, nextCursor}`、`limit` 默认 50 上限 100、`cardVerification` 取值 `VERIFIED`(保留 `STALE`)、ETag 形如 `"sha256-<base64url>"` —— 均为草稿提议,以实现为准 | registry-api §3 | hub 注册表(B4 余项) |
| `errorReason → x402.payment.error` 映射表,含 hub 实际会发出而设计表未列的 `malformed_payment`、`invalid_payment_requirements`(草稿映射为 `SETTLEMENT_FAILED` + `anet.reason`) | x402-scheme §errorReason | C3(`module/x402`) |
| 商户调用 `/x402/settle` 必须带 `paymentRequirements`:hub 已强制要求,**当前 `module/x402` 的 `settle` 只发 `{x402Version, paymentPayload}`,在 wire-2 hub 上会被 400 `invalid_payment_requirements` 拒绝** | x402-scheme §Facilitator | C3(需修复) |
| 授权 `InteractionID` 填 `pay_bind` 而非 ix(当前 `module/x402.Authorize` 填的是 ix) | x402-scheme §Binding | C3 |
| 本机签名服务流程:`payment-submitted` 不带 payload、`anet.payment.accept`、`client_payload_unsupported`/`option_not_offered`、代理卡片 params `{signer:"anet-daemon", clientPayload:false}` | x402-scheme 偏离 2 | C3、D1 |

## 与设计文本不同或设计未写明、草稿自行确定之处

- **GetExtendedAgentCard 的错误**:设计只写"不支持";草稿取 `UnsupportedOperationError`。规范 §3.3.4
  规定卡片未声明 `capabilities.extendedAgentCard` 时必须返回它,`ExtendedAgentCardNotConfiguredError`
  只用于声明了支持却没配置的情形(a2a-go 在设置了 capabilities 时也是这样处理的)。
- **`anet.a2aError` 的取值**:设计写"取 A2A §3.3.2 错误名";代码已实现的是 `"TaskNotFound"`(去掉
  `Error` 后缀)。草稿以代码为准,规定一律去掉后缀。
- **`/relay/*` 版本头与 relayauth v2 动作**:草稿按代码列出 `keys` 动作(设计 §3.7 的动作表没有它),
  以及"声明更高 wire 版本得 400"。
- **`hub:<aid>` 与 CAIP-2**:ANetCore `payment` 包注释认为 `hub:<aid>` 符合 CAIP-2;设计 §2 与 §21 第 7 条
  认为不符合。草稿核对了 CAIP-2:命名空间 `hub` 合法,但 reference 限 32 字符,而 AID 是 59 字符,
  所以按设计写为偏离,并写明具体原因。
- **服务参数键名**:设计定为 `a2a.serviceParameters`;A2A 自定义绑定指南举例用 `a2a-service-parameters`。
  草稿沿用设计,并在 relay-binding §17 Q2 把统一键名作为开放问题提给社区。
- **请求层 `SendMessageRequest.metadata`**:设计未涉及;v1 线上只承载 `Message.metadata`(加保留键)。
  草稿未把它写成规则,只在 §10.4 描述现状。
- **流式能力声明**:设计未规定网络卡片 `streaming` 的值;草稿建议 `true` 并列为开放问题。
- **a2a-go issue 增加 A9、A10**:任务单点名的三项(canonicalizeJSON、A2A-Version、X-A2A-Extensions)之外,
  复核源码时又发现并复现了 A2(null 必填列表 / optional 存在性)、A3(重复成员名)、A4(base64 换行)、
  A6(逗号分隔不拆分)、A8(不回显激活扩展)、A9(配置了 Verifier 仍接受无签名卡片)、
  A10(未知 SecurityScheme 使整张卡片解析失败);A10 同时是 `proposal-securityscheme.md` 的兼容性前提。
