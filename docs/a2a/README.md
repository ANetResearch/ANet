# A2A 社区贡献草稿(`docs/a2a/`)

对应设计文档 `docs/A2A-DESIGN-zh.md`(r3)§19。本目录是 anet 回馈 A2A 生态的六份草稿,面向上游社区,
正文一律英文;本文件是中文索引。

> **全部是草稿,均未提交。** 任何一份对外提交(开 issue、发 PR、发安全通告、在社区讨论区贴出)之前,
> 必须先得到产品负责人同意(设计 §2 "对外提交与发布"、§20 阶段 G)。
> **许可证:Apache-2.0。** 本目录全部文件(规范文本、绑定草案、提案、scheme 文档、issue 草稿及其中的
> 测试向量与代码片段)只按 Apache License 2.0 授权,不带 ANet 开源许可证的附加条件(ANet `LICENSE`
> 第 3 节;本目录另放 Apache-2.0 原文 `LICENSE`)。参考实现(ANetCore、ANetHub、ANet 中实现这些草稿的代码)
> 在各仓仍按 ANet 开源许可证授权;由 Agent Network Research(或经其书面许可)提交给 A2A 上游(`a2aproject/*`、
> `google-agentic-commerce/a2a-x402`)的部分,以提交时的形态按 Apache-2.0 授权(`LICENSE` 第 3 节 b),
> 满足 A2A *官方* 扩展与绑定强制 Apache-2.0 的要求(A2A `docs/topics/extension-and-binding-governance.md`)。

## 索引与状态

| 文件 | 内容 | 目标去处 | 草稿状态 | 所依赖的实现 |
|---|---|---|---|---|
| `relay-binding.md` | 中继协议绑定规范草案(`https://agentnetwork.org.cn/a2a/bindings/anet-relay/v1`):tenant=AID 路由、`SealedEnvelope`/`SealedInner` 的 CDDL、签名原像、Padmé、HPKE 参数、relayauth v2、hub 端点与错误码、操作映射(经中继 / 本地回答 / 不支持)、服务参数、错误映射、store-and-forward 下的流式语义、安全考虑与已知局限 | `a2aproject/A2A` 提案 issue → 有 maintainer 赞助后建 `experimental-cpb-anet-relay` 仓库 | 完整初稿 | 信封、密钥集、relayauth v2、hub 中继端点、任务镜像与事件总线已实现并逐项核对;`a2a.serviceParameters`、`ExtensionSupportRequired`/`VersionNotSupported` 回复、卡片 `url` 规则、本机 A2A 面(本地回答与流式响应)、`anet.cancel_requested`、同任务付款流标为 **(designed)** |
| `registry-api.md` | 注册表 API 草案:`GET /a2a/v1/agents`(查询参数、条目形状、包装层是 hub 陈述)、卡片原字节与 ETag、JWKS、`/fed/v2/cards`、卡片准入规则与高水位 | `a2aproject/A2A` 讨论 issue(规范目前明言不规定注册表 API) | 完整初稿,字段拼写待与实现对齐 | 卡片验证、JWKS、高水位已实现(ANetCore `a2acard`);**列表、卡片与 JWKS 端点在 ANetHub 中尚未实现**(目前只把 `a2a_card` 原样存下并回报 `unverified`) |
| `x402-scheme-anet-credit.md` | `anet-credit` x402 scheme 文档(按 `a2a-x402/schemes/` 格式):托管性质、PaymentRequirements/PaymentPayload、授权对象 CDDL、签名、nonce 与窗口、绑定值、facilitator 职责、收据、跨 hub、`errorReason` 常量与 a2a-x402 错误码映射、两处偏离(`hub:<aid>` 与 CAIP-2;本地签名服务接受未签名的所选项) | `google-agentic-commerce/a2a-x402` 的 `schemes/`(实验性 scheme;该目录现有文件名形如 `scheme_exact_lightning.md`,提交时改名,例如 `scheme_anet_credit.md`) | 完整初稿 | facilitator、授权/收据对象已实现并核对;`pay_bind`、商户核对、映射表、§8.7 本地签名流程标为 **(designed)**(C3 未实现) |
| `issue-a2a-go.md` | a2a-go v2.6.0 的 10 条 issue 草稿(A1–A10),每条附复现代码、观察结果、影响、修复建议 | `a2aproject/a2a-go`;**A3、A9 走 GitHub Security Advisories 私下报告,不开公开 issue** | 完整;全部在 2026-09-27 用 a2a-go `ebf17c5` 实际复现 | — |
| `issue-a2a-x402.md` | a2a-x402 v0.2 规范的 12 条 issue 草稿(X1–X12):激活头、x402 版本、A2A 1.0 示例、状态机缺口、一个任务付两次、签名服务委托、错误码、收据出现时机、传输安全措辞、对 facilitator 的数据最小化、`required: true`、非链网络标识 | `google-agentic-commerce/a2a-x402` | 完整初稿 | — |
| `proposal-securityscheme.md` | `SecurityScheme` 新变体提议 `SenderSignatureSecurityScheme`(按 A2A ADR 模板):为何现有五种 scheme 都不适用、候选方案比较、proto 改动、规范文字、profile 要求、兼容性 | `a2aproject/A2A` 规范变更提案 | 完整初稿 | anet 当前网络卡片不声明 `securitySchemes`(设计 §10.1);本提议是上游补齐的路径 |

a2a-go 的 A1–A10 复现是在 scratchpad 里用独立 Go module(`replace` 指向本地 a2a-go 检出)完成的,
代码片段已写入草稿;未把复现程序放进本仓库,以免给 ANet 引入 a2a-go 之外的构建目标。

## 对外提交的前置条件

1. **产品负责人同意**(每一份提交各一次)。许可证已定(2026-09-28,`docs/notes/0027` G1.2):规范文本、
   scheme 文档与 issue 草稿是 Apache-2.0;参考实现代码提交上游时按 Apache-2.0 贡献。提交时去掉各草稿文首的
   DRAFT 与许可证说明块;官方扩展/绑定仓库的贡献许可声明(governance "Contributor License Grant")由提交人确认。
2. **标记为 (designed) 的条目落地或删除**:提交前每一处 **(designed)** 必须要么已有实现并与文字一致,
   要么从草稿中拿掉。对应工作包见下节。
3. **注册表实现到位**:`registry-api.md` 的 §3.1–§3.2、`card_status` 的 `ok/unchanged/conflict`、
   `/fed/v2/cards` 在 ANetHub 中实现后,按实现回填字段拼写、分页形状、ETag 格式,再去掉文首的"未实现"说明。
4. **补齐测试向量**:`relay-binding.md` §14 列出的缺口 —— `DelegateReq`/`ChatMsg`/`StatusMsg`/`ResultResp`
   全字段向量、固定密钥下的端到端交换记录(delegate → status → result)。
5. **跨 SDK 验证**:A1(默认值剥离)需要用 a2a-python 实际验证一次行为后再提交,草稿中已避免对
   a2a-python 行为下未经验证的断言。
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
