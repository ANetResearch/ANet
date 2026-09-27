简报 · A2A 卡片签名与网络卡片(C5 卡片部分) · 2026-09-27 · 基于 a2a-redesign-wip 分支

范围:设计 §10.1–10.4、§11.3(卡片层约束)、note 0012 修正方案五点、A2A 规范 `docs/specification.md` §5.7/§8.4 与 `specification/a2a.proto`(a2a 仓 72b3761b)。
依据:三仓 `a2a-redesign-wip` 当前代码(行号以本日为准)、a2a-go v2.6.0(`Refs/a2a-go`,ebf17c5)、a2a-python main@0d5473c(只下载 `signing.py`/`_jcs.py`/`card_resolver.py` 到 scratchpad 阅读,未安装)。

---

## 0. 结论速览

1. **ANetCore `a2acard` 本体大部分已完成且测试扎实**(RFC 8785、JWS EdDSA、kid/KEL 当前钥规则、anet-card 绑定、tenant、notBefore、尺寸、大小写折叠重名、base64 严格性、高水位、JWKS、a2a-python 金标可复现)。**缺陷正是 0012 所述:签名与验签都只对原字节做 JCS,不按 proto 存在性去默认值**(`a2acard.go:26-31` 自己也写明了)。另有 REQUIRED 检查不全(0012 第 4 点)。
2. **0012 对 a2a-python 的建模不准确**:a2a-python 实际算法是 `MessageToDict` + `_clean_empty`(全树删除 `""`、`[]`、`{}`、`null`,**包括 Struct 内部与 REQUIRED 位置**),既不等于规范 §8.4.1,也不等于 a2a-go。规范、a2a-go、a2a-python 对规范自己的示例给出三种不同载荷(§2.2)。唯一可移植的做法是:**签名方只签"发布形"(三种算法结果逐字节相同的卡片)**,验签方三种形式都试。
3. **daemon 侧网络卡片完全没做**:`internal/daemon/card.go` 只签 ADP 卡;`provider.Described`、`module.CardContributor`、service 模块的 skill 元数据、x402/p2p 的卡片贡献都不存在;ANet `hubapi.RegisterRequest` 没有 `a2a_card` 字段,daemon 从不发送 A2A 卡。
4. **进度记录 0013 称 B4 已完成"A2A 卡片准入与注册表 API、JWKS、联邦 v2 卡片",代码不支持这一说法**:ANetHub 没有任何文件 import `ANetCore/a2acard`;`/register` 的 `a2a_card` 只做"是 JSON 对象且 ≤64 KiB"检查后原样入 `agent_a2a_card` 表,状态 `unverified`(`ANetHub/internal/aghub/a2acard.go:9-36`,表注释 `aghub.go:394-402` 写明"admission is added separately");没有 `/a2a/v1/agents*`、`/agents/{aid}/jwks.json`、`/fed/v2/cards` 路由(`server.go:272-329`、`federation.go:346-348`)。daemon 发卡后,卡里的 `jku` 会指向 404。
5. 设计 §10.4 的 `CardContributor` 无参签名无法同时满足 §8.1(全部公开 skill 收费才 `required:true`)与 §10.2(只发布公开能力):模块不知道哪些能力是公开的。需要给接口加 `CardContext`(§4.1)。
6. 本机环境:`python3` 3.12.3、`protobuf` 7.35.1、`cryptography` 41.0.7、`PyJWT` 2.7.0 可用;**a2a-python(a2a-sdk)未安装,protoc/grpc_tools 不可用**;GitHub raw 可访问(curl 200)。已验证:**用动态 descriptor 构造 AgentCard 子集 + MessageToDict + 抄写的 `_clean_empty` + JCS,能逐字节复现 a2a-go 自带的 a2a-python 金标签名**,因此无需安装 a2a-sdk 即可生成金标向量(§3.7)。

---

## 1. 现状核对(done 项简列,缺口见 §3–§5 与结构化结果)

| 要求 | 判定 | 证据 |
|---|---|---|
| §10.3 JWS EdDSA、保护头 `{"alg","jku","kid","typ"}` 排序无空白,与 a2a-go 同字节 | done | `ANetCore/a2acard/jws.go:163-208`;`jws_test.go:68-96`(复现 a2a-python 签名)、`:98-112` |
| §10.3 RFC 8785(含 ECMAScript 数字、UTF-16 键序、I-JSON 拒绝) | done | `jcs.go`;`jcs_test.go` 附录 B 向量 |
| §10.3 规范化前去掉 `signatures`;存储转发用原字节 | done | `jws.go:130-136`;`Sign` 输出整卡 JCS |
| §10.3 规范化前按 proto 存在性去默认值 | **diverged** | `a2acard.go:26-31`;`jws.go:109-136`、`:218-231`;`verify.go:110,121` |
| §10.3 验证:signatures 空即拒、kid 解析、KEL 回放、当前钥、anet-card 绑定、tenant、notBefore、尺寸 | done | `verify.go:82-132`、`:196-245`、`:358-424` |
| §10.3 "只接受顶端活跃密钥态" | done(解释性差异) | `verify.go:229-245`:接受"最后一次轮换以来"的全部键态(ixn/drt 不换钥),注释给出理由;与"顶端"字面不同,见风险 |
| §10.3 `params.seq` 三分支高水位 | done | `highwater.go:47-64`;`highwater_test.go` |
| §10.3 必填项(0012 第 4 点) | **partial** | `verify.go:282`(description 允许空)、`:289`(version 允许空)、`:300`(protocolVersion 允许空)、`:309-313`(两个 default*Modes 允许空数组);未查 provider 子字段与可选成员类型 |
| §10.3 JWKS(只含活跃键态) | done(库) | `jwks.go:17-49`;hub 路由缺(见 0.4) |
| §10.3 a2a-go 自带 Ed25519 金标由 a2acard 验证 | done | `testdata/a2a-python-golden.json`;`jws_test.go:43-66` |
| §10.3 a2a-go 验证 a2acard 签出的卡;a2a-go 解析再序列化后仍有效 | missing | ANet `go.mod` 无 a2a-go;无契约测试 |
| §10.3 `module/a2a/kelresolver`(实现 `a2acrypto.KeyResolver`) | missing | 无 `module/a2a` |
| 0012 第 3 点:a2a-python 生成的金标(扩展 params、多 skill、线上带默认值变体) | missing | `testdata/` 只有 3 个文件 |
| §10.1 daemon 生成网络卡片 | missing | `internal/daemon/card.go:60-63,185-220` 只有 ADP |
| §10.2 `provider.Described`/`SkillInfo` | missing | `provider/provider.go:54-112` |
| §10.2 service 模块 name/description/tags/examples + `Described` | partial | `module/service/service.go:82-95` 只有 `Description` |
| §10.4 `CardContributor` | missing | `module/module.go` 无此接口 |
| §8.1 x402 模块声明 a2a-x402 与 anet-pricing | missing | `module/x402/x402.go:232`(`Price`)、`pay.go:82`(`HomeNetwork`)已具备原料 |
| §10.1 p2p 直连条目 | missing(且设计欠规格) | `module/p2p/p2p.go:54-63` 无地址;`hub_client.go:530-543` 地址不落盘 |
| `/register` 的 `a2a_card`(ANet 侧) | missing | `internal/hubapi/hubapi.go:200-212`;`hubapi_test.go:55-57` 钉的字段无 `a2a_card`;hub 侧 `server.go:524-527` 已有 |
| §3.8 消费方卡片高水位 | partial | `internal/runtime/interactions/peers.go:188-189` 只有 `card_seq`,无载荷哈希,三分支的"同 seq 比载荷"做不了;且无写入方 |
| §11.3 代理卡片 | missing(D1) | 无 `module/a2a` |

---

## 2. 实现者必须先知道的规范事实

### 2.1 AgentCard 各字段的存在性类别(a2a.proto:336-640,无 `json_name` 覆盖,JSON 名一律 lowerCamelCase)

| message | REQUIRED | `optional`(显式存在) | 隐式存在标量 | 非 REQUIRED repeated / map | message / oneof / Struct |
|---|---|---|---|---|---|
| AgentCard | name, description, supportedInterfaces, version, capabilities, defaultInputModes, defaultOutputModes, skills | documentationUrl, iconUrl | — | securityRequirements(rep)、securitySchemes(map→SecurityScheme) | provider(msg);signatures 不入载荷 |
| AgentInterface | url, protocolBinding, protocolVersion | — | tenant | — | — |
| AgentProvider | url, organization | — | — | — | — |
| AgentCapabilities | — | streaming, pushNotifications, extendedAgentCard(bool) | — | extensions(rep) | — |
| AgentExtension | — | — | uri, description(string);required(bool) | — | params(**Struct**) |
| AgentSkill | id, name, description, tags | — | — | examples, inputModes, outputModes, securityRequirements | — |
| SecurityRequirement | — | — | — | schemes(map→StringList) | — |
| StringList | — | — | — | list | — |
| SecurityScheme | — | — | — | — | oneof: apiKeySecurityScheme / httpAuthSecurityScheme / oauth2SecurityScheme / openIdConnectSecurityScheme / mtlsSecurityScheme |
| APIKeySecurityScheme | location, name | — | description | — | — |
| HTTPAuthSecurityScheme | scheme | — | description, bearerFormat | — | — |
| OAuth2SecurityScheme | flows(msg) | — | description, oauth2MetadataUrl | — | — |
| OpenIdConnectSecurityScheme | openIdConnectUrl | — | description | — | — |
| MutualTlsSecurityScheme | — | — | description | — | — |
| OAuthFlows | — | — | — | — | oneof: authorizationCode / clientCredentials / implicit / password / deviceCode |
| AuthorizationCodeOAuthFlow | authorizationUrl, tokenUrl, scopes(map<string,string>) | — | refreshUrl;pkceRequired(bool) | — | — |
| ClientCredentialsOAuthFlow | tokenUrl, scopes | — | refreshUrl | — | — |
| ImplicitOAuthFlow / PasswordOAuthFlow | — | — | authorizationUrl/tokenUrl, refreshUrl | scopes(map) | — |
| DeviceCodeOAuthFlow | deviceAuthorizationUrl, tokenUrl, scopes | — | refreshUrl | — | — |

规范 §5.7:REQUIRED 字段"MUST be present and set",**REQUIRED 数组至少一项**。

### 2.2 三种规范化实测(同一输入:规范 §8.4.1 的示例片段)

| 算法 | 规则 | 输出 |
|---|---|---|
| 规范 §8.4.1(= 0012 方案 2,下称 **F_proto**) | 隐式默认值去掉;REQUIRED 与 `optional` 保留;Struct 内不动 | `{"capabilities":{"pushNotifications":false,"streaming":false},"description":"","name":"Example Agent","skills":[]}` |
| a2a-go v2.6.0(**F_raw**,`a2acrypto/canonical.go:31-39`) | 只去顶层 `signatures`,原样 JCS | `{"capabilities":{"extensions":[],"pushNotifications":false,"streaming":false},"description":"","name":"Example Agent","skills":[]}` |
| a2a-python main(**F_py**,`signing.py` `_canonicalize_agent_card`) | `ParseDict(ignore_unknown_fields=True)` → `MessageToDict` → pop signatures → `_clean_empty`(全树删 `""` `[]` `{}` `None`,级联) | `{"capabilities":{"pushNotifications":false,"streaming":false},"name":"Example Agent"}` |

另测得(scratchpad 探针):MessageToDict 保留 Struct 内的 `""`、`false`、`0`、`[]`、`{}`、`null`,`_clean_empty` 再删掉其中 `""`、`[]`、`{}`、`null`(保留 `false`、`0`);MessageToDict 删掉 `tenant:""`、`required:false`、扩展 `description:""`、`examples:[]`;显式设置的 `optional` bool `false` 保留。

### 2.3 a2a-go 解析再序列化的固定点(`a2a/agent.go:18-182`、`a2a/auth.go:49-121`)

- 始终输出(无 omitempty):`streaming`、`pushNotifications`、`description`、`name`、`version`、`supportedInterfaces`、`defaultInputModes`、`defaultOutputModes`、`skills`、skill 的 `id/name/description/tags`(nil 切片会变成 `null`)、interface 的 `url/protocolBinding/protocolVersion`、provider 的两个字段、HTTP auth 的 `scheme`。
- omitempty(显式 `false`/`""`/`[]` 会被丢):`extensions`、`extendedAgentCard`、`documentationUrl`、`iconUrl`、`provider`、`securityRequirements`、`securitySchemes`、`signatures`、扩展的 `uri/description/params/required`、`tenant`、`examples/inputModes/outputModes`。
- 空 scope 列表序列化为 `{}`,`{"list":[]}` 会被改写成 `{}`。
- 未知成员在解析时丢弃;`params` 是 `map[string]any`(数字变 float64,重新序列化后 JCS 不变)。
- `a2asrv` 的卡片处理器用 `json.Marshal(card)` 重新序列化(`a2asrv/agentcard.go:88,139`);`a2aclient/agentcard/resolver.go:210-214` 对**原响应体**验签。

---

## 3. a2acard 默认值剥离修正:精确实施方案(ANetCore)

### 3.1 "发布形"(publish form)定义——签名方只签这种卡

卡片(去掉 `signatures` 后)满足全部条件:

- **P1** 已知位置只出现 §2.1 表中的成员(Struct 子树除外),成员名 camelCase。
- **P2** 任何位置(含 Struct)不出现 `null`、`""`、`[]`、`{}`。
- **P3** 隐式存在的 bool 只能为 `true`(`AgentExtension.required`、`AuthorizationCodeOAuthFlow.pkceRequired`)——§8.7 的"x402 `required` 强制 false"因此写成**省略**。
- **P4** `capabilities.streaming`、`capabilities.pushNotifications` 必须显式出现;`extendedAgentCard` 只能缺省或为 `true`。
- **P5** REQUIRED 字符串非空;REQUIRED 数组 ≥1 且字符串元素非空;REQUIRED message 出现。
- 判据:`F_raw == F_proto == F_py`(字节)且 P4、P5 成立。满足即 a2a-go(含解析再序列化)、规范、a2a-python 三方算出同一载荷。
- 核实:现有 `testdata/golden-card.json` 已是发布形(Python 探针算得 raw==proto==py,SHA-256 = `3802f58c…fe98` = `golden_test.go:41` 的 `goldenPayloadHash`),改造后该金标不动。
- 数值一律字符串是 anet 自己的约定,放在 daemon 构建器里断言,不进 a2acard 通用检查(Struct 内数字三种算法结果一致)。

### 3.2 新文件 `ANetCore/a2acard/presence.go`

```go
// Form 是签名载荷的规范化方式。
type Form uint8
const (
	FormRaw    Form = iota + 1 // JCS(卡片去 signatures);a2a-go v2.6.0
	FormProto                  // 按 a2a.proto 去隐式默认值,REQUIRED/optional 保留,Struct 原样;规范 §8.4.1
	FormPython                 // FormProto 且连 REQUIRED 位置默认值一起去,再全树删 "" [] {} null;a2a-python
)
func (f Form) String() string // "raw" | "proto" | "python"

type presence uint8 // fReq, fOpt, fImplicit, fRepeated, fMap, fMsg(含 oneof 成员), fStruct
type fieldSpec struct {
	p    presence
	json kind      // 期望 JSON 类型:kindString/kindBool/kindArray/kindObject
	elem *msgSpec  // message、repeated message、map<string,Message> 的值类型;map<string,string> 为 nil
}
type msgSpec map[string]fieldSpec
var cardSpec msgSpec // 在 init() 中按 §2.1 建表(有递归引用)

// stripProto 返回新树(不改输入)。在已知位置:null 去掉(proto JSON 语义);fImplicit 为 ""/false 去掉;
// fRepeated 为 [] 去掉;fMap 为 {} 去掉;fReq/fOpt/fMsg 保留;fStruct 原样不下钻;
// 未知成员原样保留(见 3.5)。dropRequiredDefaults=true 时 REQUIRED 位置的 ""/[] 也去掉(F_py 用)。
func stripProto(v *value, s msgSpec, dropRequiredDefaults bool) *value
// cleanEmpty 复刻 a2a-python signing._clean_empty:返回 nil 表示应删除;深度沿用 maxDepth。
func cleanEmpty(v *value, depth int) (*value, error)
// payloadForms 对已解析卡片计算三种载荷(都不含顶层 signatures)。
func payloadForms(card *value) (raw, proto, py []byte, err error)
// CheckPublishForm 检查 3.1 的 P1–P5 与三形等值。失败返回 CodeNotPublishForm,Detail 给 JSON 路径与规则名,
// 例如 `capabilities.extensions[1].required: implicit bool at default (omit it)`。
func CheckPublishForm(cardJSON []byte) error
```

- `errors.go` 新增 `CodeNotPublishForm Code = "NOT_PUBLISH_FORM"`。
- 实现建议:先按规则逐节点检查,产出可读路径;最后再做三形字节比较作兜底(防表漏项)。

### 3.3 `Sign` 改为严格,测试用非严格内部函数

- `jws.go:163` `Sign`:在 `parseCard` 之后先做"通用 REQUIRED 检查 + `CheckPublishForm`",再签。`SignWithController` 随之严格。不要求 anet-card 扩展(D1 代理卡也用它)。
- 现有签名逻辑移到未导出的 `signUnchecked(card *value, priv, kid, jku)`。**大量负向用例依赖签出非法卡**(`verify_test.go:68-273` 的 "name empty"、"skills empty"、"params.seq as a JSON number"、"extension without uri"(带 `"required": false`)等;`review_test.go`、`highwater_test.go` 同理),所以 `helpers_test.go:90-107` 的 `signCard`/`signAs` 改走 `signUnchecked`;再加正向用例证明导出 `Sign` 拒绝每条 P 规则。
- `TestSignReproducesCrossSDKVector`(`jws_test.go:68`)继续用导出 `Sign`:该卡是发布形(已核实),不受影响。
- 目前没有其他仓库 import `a2acard`(ANet、ANetHub 均无),改签名无外部影响。
- 同步改 `a2acard.go:26-31` 的包注释。

### 3.4 验签回退策略

- `Verify`(`verify.go:82`):结构检查通过后调用 `payloadForms`,按 **proto → raw → python** 的顺序(0012 的首选在前)**去重**得到至多 3 份载荷。对每个签名项:解析头、kid 绑定、KEL(仍然最多取一次),再对每份载荷做 `ed25519.Verify`,第一次成功即返回。成本上界 `MaxSignatures(8) × 3` 次验签,64 KiB 卡三次 JCS,可接受。全部失败仍返回第一个签名的错误(保持 `CodeKELUnavailable` 优先的现有语义,`verify.go:128-131`)。
- `Verified` 增加 `Form Form`(验证通过所用的形式)与 `Portable bool`(三形相等且 P4/P5 成立)。
- **`PayloadHash` 改为 `SHA-256(F_proto)`**(语义同一性:同一张卡注入默认值后仍算 `Same`,而不是 `SEQ_FORK`)。对发布形卡片数值不变,`goldenPayloadHash` 不用改。
- `VerifySignature`(`jws.go:218`)同样回退,签名改为 `(*Header, Form, error)`;`SigningPayload`(`jws.go:111`)改为返回 F_proto,另加 `Payloads(cardJSON) (raw, proto, py []byte, err error)` 供诊断与 D1 使用。
- 建议(需确认):hub 准入要求 `v.Portable`,否则 `card_status=invalid`、`card_error="not in publish form"`,保证 hub 转发的字节任何 SDK 都能验;daemon 作为消费方接受任意形式,`Form != raw` 时记日志。

### 3.5 回退带来的安全面与对策

- 回退意味着"只差默认值(proto 形)或只差空值/`null`(python 形,包括 Struct 内部)"的不同字节会被同一签名接受。对策:
  1. `stripProto`/`cleanEmpty` **不丢未知成员**(与 SDK 解析行为不同,属刻意收紧):否则第三方可以在已签名卡上注入未签名的旧版成员(如 0.3 的顶层 `url`、`preferredTransport`、`security`,`card_resolver.py:40-110` 仍会读取这些成员)而不破坏签名。合法签名方(a2a-go 签原字节;a2a-python 从 proto 出发)从不产生未签名的未知成员,所以不影响互通。
  2. `Verify` 读取的成员保持严格的类型与非空检查(3.6),消费方只从 `Verified` 取值,不从原字节里读被规范化掉的成员。
  3. anet 自定义扩展的 params(anet-card、anet-pricing、anet-origin)的读取方把 `""`/`[]`/`{}`/`null` 当作缺省处理,并对必需参数做非空校验(`readCardExtension` 已如此)。

### 3.6 REQUIRED 检查补齐(0012 第 4 点)——改 `verify.go` 的 `checkRequired`

- `:282` `str(card,"description",false)` → `true`;`:289` version → 非空;`:300` `protocolVersion` 非空。
- `:309-313` `strList(card, f, false)` → `true`(数组 ≥1、元素非空)。
- 新增:`provider` 若出现须是对象,且 `url`、`organization` 非空;`documentationUrl`、`iconUrl` 若出现须为字符串(`null` 拒绝);`streaming`/`pushNotifications`/`extendedAgentCard` 若出现须为 bool;扩展的 `uri` 须为非空字符串、`description` 为字符串、`required` 为 bool、`params` 为对象;skill 的 `examples/inputModes/outputModes` 若出现须为非空字符串数组;`securityRequirements`/`securitySchemes` 若出现须形状正确(网络卡片按 §10.1 本就省略,可以直接拒绝出现在 anet 网络卡片里)。
- 新增用例(`TestVerifyRejections` 表内追加):description 空、version 空、protocolVersion 空、defaultInputModes `[]`、defaultOutputModes 含 `""`、provider.url 空、iconUrl 为 null、streaming 为字符串、extension uri 为空串。每条用 mutation 确认(把对应检查改回 `false` 后用例必须失败)。

### 3.7 金标向量与 Python 生成方法

需要的向量(放 `ANetCore/a2acard/testdata/`):

| 名称 | 内容 | 期望 |
|---|---|---|
| V1 `a2a-python-golden.json` | 已有(a2a-go 附带,真实 a2a-python 生成) | 保持现有两个测试 |
| V2 `golden-card.json` | 已有 | 追加断言 `Form` 与 `Portable==true` |
| V3 `py-anet-card.json` | anet 网络卡:anet-card params、anet-pricing `prices` 数组、a2a-x402(`required:true`)、anet-evidence、2 个 skill(含 examples 与中文),用**套件身份**签名 | `Verify` 通过,`Portable`;Go 侧 `Sign` 复现同一签名 |
| V4 `py-anet-card-wire-defaults.json` | V3 的签名不变,线上 JSON 注入默认值:anet-card 扩展 `"required":false`、anet-evidence `"description":""`、skill 2 `"examples":[]`、`"inputModes":[]`、顶层 `"securityRequirements":[]` | 只有 proto 形通过;`Portable==false`;`PayloadHash` 与 V3 相同(`CheckHighWater` 得 `Same`) |
| V5 `py-card-struct-empties.json` | V3 再加一个扩展 `{"uri":"https://example.org/ext/probe","params":{"s":"","a":[],"o":{},"n":null,"f":false,"z":"0"}}`,由 Python 签名 | 只有 python 形通过 |
| V6 `spec-8.4.1-example.json` | 规范示例输入 + 三种期望输出(§2.2 表,不签名) | 单测 `stripProto`/`cleanEmpty`/raw |
| V7 `raw-defaults-card.json` | 带默认值(如 `"required":false`)按 raw 签名(a2a-go 行为;Go 用 `signUnchecked` 生成,Ed25519 确定性) | 只有 raw 形通过 |

Python 生成(**不需安装 a2a-sdk**;本机已验证可行):

- 新脚本 `ANetCore/a2acard/testdata/gen_py_vectors.py`,不被 `go test` 调用;依赖 `protobuf`(本机 7.35.1)与 `cryptography`(本机 41.0.7)。
- 用 `descriptor_pb2.FileDescriptorProto` 手工构造 §2.1 的 AgentCard 子集(`optional` 字段设 `proto3_optional=True` 并挂一个合成 oneof),依赖 `google/protobuf/struct.proto`(`pool.AddSerializedFile(struct_pb2.DESCRIPTOR.serialized_pb)`),`message_factory.GetMessageClass` 得到类。V3–V5 要用到 SecurityRequirement 时把它也加上。
- 规范化 = `canonical(clean_empty(MessageToDict(ParseDict(card, AgentCard(), ignore_unknown_fields=True)) 去掉 "signatures"))`。`clean_empty` 照抄 a2a-python `signing.py` 的 10 行(注明出处 a2a-python@0d5473c,Apache-2.0)。JCS:向量里不放数字、键全是 ASCII 时 `json.dumps(sort_keys=True, separators=(",",":"), ensure_ascii=False)` 与 RFC 8785 相同(`golden_test.go:37-40` 已用过同一论证);脚本里断言树中无数字、无非 ASCII 键、无控制字符。
- 保护头与 PyJWT 相同:`json.dumps(hdr, separators=(",",":"), sort_keys=True)`,`hdr={"alg":"EdDSA","jku":…,"kid":…,"typ":"JOSE"}`;签名输入 `b64url(hdr)+"."+b64url(payload)`,`Ed25519PrivateKey.from_private_bytes(seed).sign(...)`。
- 套件身份:seed = `sha256("anet-suite-identity-v1/cur")`(`golden_test.go:91`);AID = `bafyreicg3paeuo2nt4n575adgnovtr2y7aizti7643fxhk6zbmiaaa7q7y`(`golden-card.json` 的 params.aid);kid `did:anet:<AID>#0`;jku `https://hub.example.org/agents/<AID>/jwks.json`(`helpers_test.go:19`)。Go 侧 `Verify(b, newResolver(identity.SuiteController()).resolve, t0)` 即可跑完整路径。
- **自检(必须)**:脚本先用同一流程对 V1 的 `card_json_unsigned` 签名,比对 `protected_b64` 与 `signature_b64`,不一致即退出。本次勘察已在 scratchpad 跑通:两者均一致。
- 线上 JSON 用 `MessageToDict(card)` 后 `json.dumps(..., ensure_ascii=False)` 写出(即 a2a-python 服务端会发出的形状);V4 在签名之后往 dict 里注入默认值再写出。
- 若产品负责人允许安装,可改用 venv + `pip install "a2a-sdk[signing]"` 调真实 `a2a.utils.signing.create_agent_card_signer`,并保留上述自检作为交叉核对。

骨架(可直接扩展):

```python
from google.protobuf import descriptor_pb2, descriptor_pool, struct_pb2, message_factory
from google.protobuf.json_format import MessageToDict, ParseDict
F = descriptor_pb2.FieldDescriptorProto
fdp = descriptor_pb2.FileDescriptorProto(name="a2a_subset.proto", package="lf.a2a.v1", syntax="proto3",
                                         dependency=["google/protobuf/struct.proto"])
def msg(name, fields):               # fields: (json_snake_name, type, label, type_name, is_optional)
    m = fdp.message_type.add(name=name)
    for num, (n, t, l, tn, opt) in enumerate(fields, 1):
        f = m.field.add(name=n, number=num, type=t, label=l)
        if tn: f.type_name = tn
        if opt:
            f.proto3_optional = True; f.oneof_index = len(m.oneof_decl); m.oneof_decl.add(name="_" + n)
# msg("AgentInterface", ...), msg("AgentExtension", [..., ("params", F.TYPE_MESSAGE, F.LABEL_OPTIONAL, ".google.protobuf.Struct", 0)]), ...
pool = descriptor_pool.DescriptorPool(); pool.AddSerializedFile(struct_pb2.DESCRIPTOR.serialized_pb); pool.Add(fdp)
AgentCard = message_factory.GetMessageClass(pool.FindMessageTypeByName("lf.a2a.v1.AgentCard"))
def clean_empty(d):                  # a2a-python signing._clean_empty
    if isinstance(d, dict):
        c = {k: cv for k, v in d.items() if (cv := clean_empty(v)) is not None}; return c or None
    if isinstance(d, list):
        c = [cv for v in d if (cv := clean_empty(v)) is not None]; return c or None
    return None if isinstance(d, str) and not d else d
```

### 3.8 a2acard 测试与 mutation 清单

- `TestPublishFormRules`:每条 P 规则一个违反例,导出 `Sign` 返回 `CodeNotPublishForm`。mutation:逐条关闭规则,对应用例必须失败。
- `TestFormsSpecExample`(V6):三种输出逐字节相等。mutation:让 `stripProto` 去掉 REQUIRED 空值,或下钻 Struct,用例必须失败。
- `TestPythonVectors`(V3–V5)、`TestRawDefaultsVector`(V7):断言 `Form` 与 `Portable`。mutation:从尝试列表里删掉 proto 形,V4 必须失败;删掉 python 形,V5 必须失败。
- `TestPayloadHashIsProtoForm`:V3 与 V4 的 `PayloadHash` 相同,`CheckHighWater` 得 `Same`。mutation:哈希改回用 raw,用例必须失败。
- `TestVerifyRejectsUnknownMemberInjection`:签名后注入顶层 `"url"`,三种形式都拒(`INVALID_SIGNATURE`)。mutation:让规范化丢弃未知成员,用例必须失败。
- 3.6 的必填项用例。
- 全部改完后 `go test ./a2acard/ -run Golden -update` **不应**改动 `golden-card.json`、`golden-jwks.json`。

---

## 4. daemon 生成并签发网络卡片:实施方案(ANet)

### 4.1 接缝

**provider(`provider/provider.go`,与 `Priced`、`LongRunning` 同一模式)**

```go
// Described is implemented by a provider that can describe a capability as an A2A skill (A2A-DESIGN §10.2).
type Described interface {
	SkillInfo(capability string) (SkillInfo, bool)
}
type SkillInfo struct {
	Name, Description string
	Tags, Examples    []string // 空即省略;内核会校验与截断
}
```

**module(`module/module.go`,放在 `Confidential`/`Payer` 旁,内核按类型断言发现)**

```go
// CardContext is what a module may know when it contributes to this node's network card.
type CardContext struct {
	AID    string
	HubURL string   // 规范化后(无尾斜杠)
	Skills []string // 本卡将发布的 skill id(= 公开且在服务中),已排序
}
type CardContributor interface {
	CardExtensions(c CardContext) []map[string]any // 内核不 import a2a-go
	CardInterfaces(c CardContext) []map[string]any
}
```

- 与设计 §10.4 的偏离:多了 `CardContext` 参数。理由:x402 需要知道哪些 skill 公开,才能判断"全部收费则 `required:true`"(§8.1),并且只为公开 skill 报价(§10.2)。无参版本只能遍历 `Host.Providers()`,会把非公开的收费能力和价格写进公开卡片。另一种做法是给 `Host` 加 `PublicCapabilities()`,但那是扩大 Host 接口,比传参宽。
- 内核发现方式:在 `internal/daemon/modules.go` 仿 `:96-101` 遍历 `d.modules` 做 `m.(module.CardContributor)`。

**内核过滤模块贡献**(模块提供的内容不直接信任):

- 扩展:`uri` 须为非空字符串,且不得与内核自有 URI(anet-card、anet-evidence)重复;`required` 只保留 `true`;`params` 须为对象;按 `uri` 排序。
- 接口:`url` 须是绝对 URL,拒绝回环、`localhost`、`unix://`;`protocolBinding` 非空;`protocolVersion` 固定 `"1.0"`;anet 绑定的 `tenant` 强制等于 AID;排在中继条目之后,按 (protocolBinding, url) 排序。
- 最后整卡过 `a2acard.CheckPublishForm`。

### 4.2 构建器放在哪

- 新包 `ANet/internal/netcard`(无 build tag,纯函数,**不 import a2a-go**,只 import `ANetCore/a2acard`),内核与将来 D1 的契约测试都能调用:

```go
type Skill struct{ ID, Name, Description string; Tags, Examples []string }
type Input struct {
	AID, Name, Description, Version, HubURL string
	Seq, IssuedAtMs                        uint64
	Skills                                 []Skill          // 已排序、已校验
	Extensions, Interfaces                 []map[string]any // 已过滤的模块贡献
}
func Build(in Input) ([]byte, error)      // 未签名 JSON;内部调用 a2acard.CheckPublishForm
func DeriveSkill(capID string) Skill      // Described 缺席或返回 false 时使用
func RelayURL(hubURL string) string       // strings.TrimRight(hubURL,"/") + "/relay"(见风险:需钉死)
func JKU(hubURL, aid string) string       // TrimRight(hubURL,"/") + "/agents/" + aid + "/jwks.json"
```

- daemon 胶水,新文件 `internal/daemon/netcard.go`:

```go
// signedNetworkCard returns (nil, nil) when this node publishes no card (no public skill, §10.1).
func (d *Daemon) signedNetworkCard(cfg Config) (json.RawMessage, error)
```

  流程:
  1. skills = `cfg.inbound().PublicCapabilities` 的 id ∩ `d.providers.Resolve(id)` 成功者(公开能力按精确 id 准入,见 `inbound.go:192-199`),排序去重;为空则返回 nil。
  2. 每个 skill:provider 实现 `Described` 且返回 true 时取之;否则 `DeriveSkill`。校验并截断:name ≤128 字节、description ≤4096 且**非空**(0012 第 5 点,缺省用 `"anet capability " + id`)、tags 1–16 个且非空、按 `@` 前 `.` 的各级前缀派生(`text.stats` → `["text","text.stats"]`)并入声明值,小写去重。超过 256 个 skill 时拒绝构建并记日志(不静默截断)。
  3. 调用 `CardContributor`,并按 4.1 过滤。
  4. `seq := d.cardSeq()`(`card.go:144`,持久化、严格递增);`issuedAt = time.Now().UnixMilli()`,`notBefore = issuedAt - 60_000`(毫秒,a2acard 约定)。
  5. `netcard.Build` → `a2acard.SignWithController(raw, d.self, netcard.JKU(hub, aid))`。
  6. 自检:`a2acard.Verify(signed, func(string) ([]identity.SignedEvent, error) { return d.self.KEL(), nil }, nowMs)`,失败即不发送并记日志。代价很小,能在发给 hub 之前发现构建器错误。

### 4.3 字段取值(设计未写死的项给出建议,见风险)

| 字段 | 取值 |
|---|---|
| name | `cfg.Name`;空则 `"anet agent " + AID[:12]`;≤128 字节,按 rune 截断 |
| description | `cfg.Summary`(profile 摘要;0007 §8.2 的建议),空则固定文本 `"anet agent reachable through the anet relay"`;≤4096 字节 |
| version | `internal/version.V` |
| supportedInterfaces[0] | `{url: RelayURL(hub), protocolBinding: a2acard.BindingRelayURI, protocolVersion: "1.0", tenant: AID}` |
| capabilities | `streaming:false`、`pushNotifications:false`(显式输出);不写 `extendedAgentCard` |
| capabilities.extensions | 依次为 anet-card `{uri: ExtCardURI, params:{aid, seq, issuedAt, notBefore}}`(全部是十进制字符串,**不写 `required`**);anet-evidence `{uri}`;然后是模块贡献 |
| defaultInputModes / defaultOutputModes | `["text/plain","application/json"]`(文本任务 + DataPart 能力调用) |
| skills[] | `{id, name, description, tags[, examples]}`;不写空的 `examples`/`inputModes`/`outputModes` |
| securitySchemes / securityRequirements / provider / iconUrl / documentationUrl | 省略 |

### 4.4 模块怎么贡献

- **x402**(新文件 `module/x402/card.go`,`//go:build !no_x402` 与包内其他文件一致):`CardExtensions(c)`:当 `m.HomeNetwork()==""`(无 hub 账本)或 `c.Skills` 中没有 `m.Price(id)` 为 true 的能力时返回 nil;否则返回
  - a2a-x402:`{"uri":"https://github.com/google-agentic-commerce/a2a-x402/blob/main/spec/v0.2","description":"…"}`,只有当 `c.Skills` 全部收费时才加 `"required": true`(不写 false);
  - anet-pricing:`{"uri": ExtPricingURI, "params": {"network": m.HomeNetwork(), "prices": [{"skillId": id, "amount": strconv.FormatUint(p, 10)}…]}}`,按 skillId 排序,只含收费的公开 skill。
  - `CardInterfaces` 返回 nil。
  - 测试:x402 tag 下断言两条扩展及 `required` 的两种情况;新增 `//go:build no_x402` 的测试文件,断言卡片字节中不含 `x402` 与 `anet-pricing`。
- **p2p**(`module/p2p`):现状模块刻意不知道地址(`p2p.go:22` 的红线说明);`/p2p-advertise` 由内核直接发给 hub,不落盘(`hub_client.go:530-543`)。最小实现建议:`Config` 增加运营者声明的 `advertise`(只接受 `tcp://host:port`,拒绝回环与 `unix://`),`CardInterfaces` 返回 `{url: advertise, protocolBinding: BindingP2PURI, protocolVersion:"1.0", tenant: AID}`;同时把 a2acard 的 tenant 绑定检查(`verify.go:411-424`,目前只查中继绑定)扩展到 p2p 绑定。设计没有定义 p2p 绑定 URI 与 URL 形式,需要先定;否则本期可以不做 p2p 条目。
- **service**(`module/service/service.go:82-95`):`Capability` 增加 `Name string`、`Tags []string`、`Examples []string`(均为 `json:",omitempty"`;`Description` 已有);工厂(`:46-71`)校验 tags ≤16 且非空;`svcProvider` 实现 `provider.Described`:只有当 Name/Description/Tags 至少一项非空时才返回 true。

### 4.5 何时发布、如何撤回

- 在 `registerWithHubLocked`(`hub_client.go:60`)中、ADP 卡签完之后(`:85-89`)调用 `d.signedNetworkCard(cfg)`,结果放进 `body.A2ACard`;这条路径同时覆盖显式 `hub-register` 和启动时的 `refreshRegistration`(`daemon.go:259-280`)。`screenPublication`(`:90`)会随 body 一起筛查卡片,不需要另做。
- 目前 `SetPublicCapabilities`(`inbound.go:804-821`)写入配置后**不会**重新注册,需要在写入成功、且 `HubURL` 非空时调用 `d.refreshRegistration()`。`PublishProfile` 修改 Summary 后同理(description 取自 Summary)。
- 撤回:公开能力清空后,hub 仍保留旧卡(`putA2ACard` 只做 upsert;缺少 `a2a_card` 字段时不删除)。需要 hub 新增语义,例如 `"a2a_card": null` 表示撤回(删行、删索引,返回 `card_status: "withdrawn"`),或提供签名的 `DELETE` 端点。此事需要设计决定,并与 hub B4 的实现一起做。
- 读 `out.CardStatus`:为 `invalid`/`conflict` 时记日志(目前完全忽略)。hub 现在只会回 `unverified`,daemon 应把它视为"已收下"。

### 4.6 `/register` 的 `a2a_card` 字段现状

- hub 侧:请求字段 `A2ACard json.RawMessage \`json:"a2a_card,omitempty"\``(`ANetHub/internal/aghub/server.go:524-527`),处理在 `:724-726`;存储为原字节、不验证(`a2acard.go:18-36`);状态常量只有 `absent`/`unverified`/`invalid`(`server.go:543-547`);`wirecontract_test.go:374` 已钉住 `a2a_card`。
- ANet 侧:`hubapi.RegisterRequest`(`internal/hubapi/hubapi.go:200-212`)**没有**这个字段,daemon 也从不发送。需要:加字段并同步改 `hubapi_test.go:55-57` 的钉字段列表(加 `"a2a_card"`);加 `CardStatus*` 常量;`hubfake_test.go` 的 `hRegister`(`:312` 起,ADP 闸门在 `:360-380`)增加 A2A 闸门,规则与将来的 hub 一致:用请求里的 KEL 做 `a2acard.Verify`,再用 `h.a2aMarks[aid]` 做 `CheckHighWater`,回 `ok`/`unchanged`/`invalid`/`conflict`,并把原字节存进 `h.a2aCards[aid]` 供断言。fake 必须实现拒绝路径(`hubfake_test.go:352-359` 的注释讲过为什么)。
- hub 真正的准入、索引、`/a2a/v1/agents*`、JWKS、`/fed/v2/cards` 都没做(见 0.4)。这属于 hub 任务,但它阻塞"发现"链路,也导致卡片里的 `jku` 失效。

### 4.7 daemon 测试夹具怎么用

- `newFakeHub(t)`(`hubfake_test.go:139`)+ `newTestDaemon(t, h.URL, accept)`(`relay_test.go:32`);注册用 `d.RegisterWithHub(ctx, h.URL, name, caps, "")`(`card_seq_test.go:25` 是现成范例)。
- 提供能力:`lampProvider`(`capability_test.go:25`,注册方式见 `:77`)、`pricedProvider`(`pay_test.go:45`,文件带 `//go:build !no_x402`,import 了 x402 模块,所以同一测试二进制里每个 daemon 都有 payer;`withoutPayments(d)` 用来模拟 no_x402)。
- 公开能力:`d.SetPublicCapabilities([]PublicCapability{{ID: lampCap}})`(`inbound_test.go:103`)。
- 建议用例:
  1. 默认(SI-5)不发卡,fake 收到的 `card_status` 为 `absent`;
  2. 公开一个在服务中的能力后发卡,fake 用 `a2acard.Verify` 验证通过,skills 恰好等于"公开 ∩ 在服务中"(mutation:把非公开能力放进卡片,用例必须失败);
  3. 第二次注册得到 `Advance`;
  4. Described 的元数据与派生回退;
  5. x402 的两种 `required`,以及 no_x402;
  6. `CheckPublishForm` 通过(mutation:构建器写出 `"required": false`,用例必须失败);
  7. `SetPublicCapabilities` 触发重新注册;
  8. 贡献的回环接口被丢弃。

### 4.8 契约测试(a2a-go 交叉验证)与 KeyResolver

- ANet 需要按设计 §18 执行 `go get github.com/a2aproject/a2a-go/v2@v2.6.0`(本机模块缓存没有,需联网,proxy.golang.org 可达;go.sum 会引入 grpc、protobuf、genproto)。
- `module/a2a/kelresolver/kelresolver.go`(`//go:build !no_a2a`):

```go
type Resolver struct{ KEL func(ctx context.Context, aid string) ([]identity.SignedEvent, error) }
func (r Resolver) ResolveKey(ctx context.Context, kid, untrustedJKU string) (crypto.PublicKey, error) {
	aid, _, err := a2acard.ParseKID(kid)            // 不信任 jku
	if err != nil { return nil, err }
	kel, err := r.KEL(ctx, aid)
	if err != nil { return nil, err }
	return a2acard.CurrentKey(kid, kel)              // 返回 ed25519.PublicKey(值类型,正好命中 a2a-go verify.go 的 case)
}
var _ a2acrypto.KeyResolver = Resolver{}
```

  KEL 来源:`peer_identity` 中已固定的 KEL,或经 hub `GET /agents/{aid}/keys`(返回 `kel`)/`GET /agents/{aid}/kel` 取回,再以期望 AID 回放。
- 契约测试(同一目录,`//go:build !no_a2a`):用 `netcard.Build` 构建一张卡,用 `a2acard.SignWithController` 签名,然后 `a2acrypto.NewVerifier(VerifierConfig{KeyResolver: Resolver{…}}).Verify(ctx, raw, &sig)` 必须通过;`json.Unmarshal` 进 `a2a.AgentCard` 再 `json.Marshal` 后重新验证,仍须通过。注意 a2a-go 只验签名,不检查 anet 的绑定规则,不能替代 `a2acard.Verify`。
- SI-8 检查:`go list -deps ./internal/daemon ./internal/netcard | grep a2aproject` 必须为空。

---

## 5. §11.3 代理卡片对卡片层的要求(给 D1)

- 代理卡用导出 `Sign`(严格)签名,由本机密钥签、不带 jku。它没有 anet-card 扩展,`a2acard.Verify` 不适用。
- `securityRequirements: [{"schemes": {"anetLocal": {}}}]` **违反 P2**:a2a-go 与规范形都保留 `{}`,但 a2a-python 的 `_clean_empty` 会级联删掉整个 `securityRequirements`,因此 a2a-python 验证方不可能验过这张卡。这同时是 a2a-python 的一个语义缺陷:签名不再覆盖"需要 Bearer"这一陈述。可选做法:
  - (a) a2acard 提供只放行这一种模式的选项 `SignOptions{AllowEmptyScopes: true}`,并在 D1 文档中写明;
  - (b) 给 scope 填一个角色名,如 `{"list":["anet.local"]}`(OpenAPI 允许非 OAuth scheme 带角色名,但语义上是凑出来的);
  - (c) 向 a2a-python 提 issue(§19 贡献项)。
  建议 (a)+(c)。
- `anet-origin/v1` 的 `originCard` 用**字符串**(远端原字节的 base64url,与 0007 §8.4 一致),不要嵌成对象:嵌对象会把第三方卡里的空值带进 Struct,破坏可移植性,也会让签名覆盖范围变得不清楚。
- x402 声明写成 `{"uri": …, "params": {"signer":"anet-daemon","clientPayload":false}}`,**省略 `required`**(§8.7 的"强制 false")。Struct 内的 `false` 在三种算法中都会保留,没有问题。
- `streaming:true`、`pushNotifications:false` 显式输出;description 非空;"无网络卡片"的占位 skill `chat` 也要满足 P5。
- 发出去的字节必须就是签名覆盖的字节:用自己的 handler 直接写出签名结果,不要交给 `a2asrv` 的卡片处理器重新 `json.Marshal`(发布形下重新序列化结果相同,但没有必要冒这个险)。
- 消费方高水位:`peer_identity` 只有 `card_seq`,需要新增 `card_hash BLOB`(存 `Verified.PayloadHash`),三分支规则才完整(`interactions/peers.go:188-189,212,349` 与迁移)。

---

## 6. 需要新增的常量(两侧钉字符串)

放在 `ANetCore/a2acard/a2acard.go:39-46` 旁边(daemon、hub、D1 共用):

- `ExtPricingURI = "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"`
- `ExtEvidenceURI = ".../ext/anet-evidence/v1"`
- `ExtOriginURI = ".../ext/anet-origin/v1"`
- `BindingP2PURI = "https://agentnetwork.org.cn/a2a/bindings/anet-p2p/v1"`(需先定)
- a2a-x402 URI 放 `module/x402`,在 `internal/a2ashape` 侧另声明一份,由测试钉住两边字符串相同(§17 "扩展 URI 与 metadata 键两侧钉字符串")。

---

## 7. 风险与矛盾(摘要,完整条目见结构化结果)

1. 0012 把 a2a-python 当作"按 proto 语义重建"的验证方,实测它还会删掉 REQUIRED 位置的空值以及 Struct 内的空值与 null;按 0012 原方案只做"proto 形 + raw 回退",验不过 V5 这类卡片。
2. 规范、a2a-go、a2a-python 三方不一致,是可以向上游贡献的内容:一组一致性向量,外加规范与两个 SDK 的 issue。建议列入 §19 草稿,对外提交前征求同意。
3. 进度记录 0013 关于 B4 的描述与 ANetHub 代码不符(没有准入、注册表 API、JWKS、fed v2 卡片)。
4. `CardContributor` 签名需要偏离设计原文(加 `CardContext`)。
5. 以下几项设计欠规格,需要决定:中继接口 URL(`/relay` 还是测试夹具里的 `/relay/v2`;hub 实际路径是 `/relay/send|poll|ack`,线协版本在 `X-ANet-Wire` 头里)、p2p 绑定、撤回语义、网络卡片的 streaming 与 default modes 取值。"127.0.0.1 不进入网络卡片"是否也适用于中继 URL,影响本机联调:hub 在回环地址时卡片可能发不出去。
6. 注册的 `caps` 与 ADP 卡仍然发布**全部**在服务中的能力,包括非公开的(`relay.go:427-445`)。这与默认关闭的姿态、以及 §10.5"有 A2A 卡时从已验证卡片派生"不一致。
7. 多形式回退扩大了"同一签名接受哪些字节"的范围;对策见 3.5,建议 hub 准入只接受可移植卡片。
