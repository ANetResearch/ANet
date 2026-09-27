# 0026 · 验证:SI 测试的 mutation

日期:2026-09-27。对应:设计 §20 阶段 F(全量验证)之一,设计 §1"每条 SI 的测试都做 mutation 验证"与 §17 补充用例表
"每条 mutation 验证"。代码:工作树 `wt/f-mut`,分支 `wp/f-mut`,基于三仓标签 `integ/round4c`(ANetCore `735e7db`、
ANet `82e1e9f`、ANetHub `56f9243`)。本机(8 核,与其他阶段 F 工作包共用),go1.26.6,`GOWORK` 为工作树的 go.work。

## 1. 结果一览

| 项 | 数量 | 结果 |
|---|---|---|
| 单元级 mutation,SI-1..SI-10 | 77(每条 SI 5–12 个) | 首轮变红 70,存活 7 |
| 单元级 mutation,§17 补充用例 19 行 | 54(每行 1–5 个) | 首轮变红 41,存活 13 |
| 存活 20 个的归类 | 13 缺口 + 7 等价 | 13 个缺口各补用例,补后 13/13 变红;7 个等价逐条写明理由,其中 3 处另做"两层同时去掉"的组合 mutation,均变红 |
| 联调级 mutation(joint.sh) | 3 个补丁 | 基线段 C 39/0、全量 95/0;`si1-plaintext-envelope` 与 `si1-restore-official-harvest` 均被段 C 抓到;`si4-skip-step7`(全量)8/11、9/11 变红,均被抓到 |
| 产品代码缺陷 | 0 | 本轮没有发现要改产品代码的问题;全部存活项都是测试缺口或等价 mutation |

补测试的提交(均未推送):

- ANet `a203d33` test: 补 SI/§17 mutation 验证中存活的缺口(10 个新用例、1 处新断言,另给 fake hub 加 settle 故障 `foreign-receipt`)
- ANetHub `26e79d6` test(aghub): 发送不在任何表里留下发送方(SI-2)
- ANet `faaeac6` scripts/mutations: `unitmut.py` 与两份清单 `unit/si.py`、`unit/supp.py`(本轮 131 个 mutation,可重跑)

补的用例在未改动的代码上全绿;`./internal/daemon`、`./scripts`、`./module/x402` 整包、ANetHub `./internal/aghub` 整包
(`-race`)、新用例 `-race -count=3` 与 `-tags no_x402` 均绿。

每条 SI 在补测试后至少 3 个变红的 mutation(SI-4 的 si4-4 为等价,其余 5 个变红);§17 每一行至少 1 个变红
(C34 行靠补测试后的 c34-1,C2 行靠补测试后的 c2-1、c2-2)。

## 2. 方法

- 工具:`ANet/scripts/mutations/unitmut.py`(本轮后入库)。每个 mutation 是对一个仓库的一处精确文本替换(或一个
  `git apply` 补丁),替换串必须恰好出现一次。流程:确认三仓 `git status` 干净 → 应用 → 跑列出的关键测试
  (`go test -count=1 -run …`,或 `tagcheck.sh`、`-race` 这类命令)→ 任一失败即"变红";关键测试仍绿时再跑所在包全部
  测试 → 还原:`git stash push -u -m unitmut-<id>`,确认工作树干净,再按消息找到并 `git stash drop` 恰好那一条
  (没有用裸 `git stash pop`)。本轮 131 个 mutation 与 13 个复核全部还原干净,三仓 stash 列表为空。
- 编译不过的 mutation 记 BUILD-ERR、替换串不唯一记 APPLY-ERR,都算工具错误、不计结果:si7-1、si8-1、si8-4、si9-5、
  si9-12、c25-2、c2-2、c27-2 首版属此类,改写后重跑,表中是重跑的结果。
- 关键测试的选取:先找声明承担该 SI/该行的用例(测试名、注释里的 SI-n / Cn),再加上 mutation 所在函数的直接用例;
  存活时整包兜底,所以"存活"一律指整包也绿。
- "等价"只用于在当前代码里观察不到差别的 mutation(另一层防线完全覆盖,或该分支不可达),并写明是哪一层;
  另一层也去掉的组合 mutation 用来证明这条防线本身有测试。能观察到差别但没有测试的,一律算缺口并补测试。
- `--check` 只检查清单是否还能套上代码:本轮提交后 131 个全部能套上并还原干净。

## 3. SI-1..SI-10

位置是 `integ/round4c` 上的行号(补测试的提交没有动这些文件)。结果列只列前三个变红的用例。

| # | 组 | 仓 | mutation | 位置(HEAD 行号) | 结果 |
|---|---|---|---|---|---|
| si1-1 | SI-1 | Core | 关闭加密: 密文即明文(scripts/mutations/si1-plaintext-envelope.patch) | `seal/envelope.go:288` | 变红:TestOpenRecipientAndKey, TestVEC_SEALED_1, TestTheHubHoldsOnlyCiphertext |
| si1-2 | SI-1 | ANet | 商户送 hub 的 paymentRequirements 带上报价 extra(描述工作) | `module/x402/check.go:171` | 变红:TestSettleSendsRequirementsAndNothingAboutTheWork |
| si1-3 | SI-1 | Hub | hub /reviews 不再拒收 request_doc/deliverable | `internal/aghub/server.go:807` | 变红:TestAReviewUploadCarryingContentIsRefused |
| si1-4 | SI-1 | Hub | hub CheckEnvelope 恒放行(明文/错投的载荷也入库) | `internal/aghub/relay.go:57` | 变红:TestTheRelayRefusesAnythingButASealedEnvelopeForTheRecipient |
| si1-5 | SI-1 | Hub | 恢复 admin 官方 agent 采集与路由(scripts/mutations/si1-restore-official-harvest.patch) | `internal/admin/harvest.go:1, internal/admin/manifest.go:34, internal/admin/server.go:46 …` | 变红:TestAManifestWithARemovedSectionIsRefused |
| si1-6 | SI-1 | ANet | canary.py 不再生成 base64url 形式(扫描器漏检) | `scripts/canary.py:127` | **存活 → 缺口**:lib.sh 的 canary 只含字母数字与 `-`,其标准与 URL-safe base64 相同,URL-safe 针被去重。补 `TestTheCanarySearchSeesURLSafeBase64`,复核变红 |
| si1-7 | SI-1 | ANet | canary.py settle 检查不查工作字段(resource/description/extra) | `scripts/canary.py:462` | 变红:TestTheSettlementCheck |
| si1-8 | SI-1 | Hub | admin 恢复 /api/official/{id}/insights 路由 | `internal/admin/server.go:117` | 变红:TestOfficialAgentOperationsAreGone |
| si2-1 | SI-2 | Hub | RelayAck 不删行(ack 即删 失效) | `internal/aghub/relay.go:191` | 变红:TestAnAckDeletesTheRow, TestAnAckedEnvelopeLeavesNoBytesOnDisk |
| si2-2 | SI-2 | Hub | hub 库关闭 secure_delete | `internal/aghub/aghub.go:163` | 变红:TestAnAckedEnvelopeLeavesNoBytesOnDisk |
| si2-3 | SI-2 | Hub | relay_message 表加回 from_aid 列 | `internal/aghub/relay.go:236` | 变红:TestTheRelayDoesNotStoreTheSender |
| si2-4 | SI-2 | Hub | hRelaySend 把每条信封的发送方写进 hub_meta(换个表存 from) | `internal/aghub/server.go:1173` | **存活 → 缺口**:原用例只看 relay_message。补 `TestASendLeavesTheSenderInNoTable`(ANetHub),复核变红 |
| si2-5 | SI-2 | Hub | PurgeExpiredRelay 从不删除过期行 | `internal/aghub/relay.go:219` | 变红:TestUndeliveredEnvelopesExpire |
| si2-6 | SI-2 | ANet | requester 授权的 InteractionID 填 ix 而不是 pay_bind(hub 存交互 id) | `internal/daemon/x402task.go:1302` | 变红:TestAQuotedTaskIsPaidAndCompletedOnTheSameTask |
| si3-1 | SI-3 | Core | seal.Open: 不比较外层 to 与本节点 AID(收件人不符照收) | `seal/envelope.go:392` | 变红:TestOpenRecipientAndKey, TestEachReceiveStepRefusesWithItsClass |
| si3-2 | SI-3 | Core | seal.CheckTime: 去掉 now > exp 检查(过期照收) | `seal/envelope.go:519` | 变红:TestCheckTime, TestEachReceiveStepRefusesWithItsClass |
| si3-3 | SI-3 | Core | seal.decodeInner: 放过 0–63 未知内层键 | `seal/envelope.go:446` | 变红:TestInnerKeySpace |
| si3-4 | SI-3 | Core | seal.VerifyInnerSig: 忽略签名校验失败 | `seal/envelope.go:574` | 变红:TestSignedInnerFieldMutations, TestOuterFieldsAreSigned, TestEachReceiveStepRefusesWithItsClass … |
| si3-5 | SI-3 | Core | seal.Open: 不比较内层 to 与外层 to | `seal/envelope.go:426` | 变红:TestSignedInnerFieldMutations |
| si3-6 | SI-3 | Core | seal.ParseOuter: 不检查信封版本 v | `seal/envelope.go:153` | 变红:TestParseOuter |
| si3-7 | SI-3 | ANet | daemon process: 跳过持久重放表查询(seen 恒 false) | `internal/daemon/receive.go:559` | 变红:TestARedeliveredDelegateResendsTheAnswer |
| si3-8 | SI-3 | ANet | daemon commitRx: 重放行已存在仍提交业务写入(去掉 claim 结果判断) | `internal/daemon/receive.go:675` | **存活 → 缺口**(进程内与 (from, mid) 锁互为冗余)。补 `TestCommitRxRunsTheBusinessWriteOncePerMessage`,复核变红 |
| si4-1 | SI-4 | ANet | daemon 第 7 步: 签名不符(bad-sig)仍放行(= scripts/mutations/si4-skip-step7.patch) | `internal/daemon/receive.go:183` | 变红:TestEachReceiveStepRefusesWithItsClass, TestAForgedSenderInjectedAtTheHubIsDropped |
| si4-2 | SI-4 | ANet | authorizeMessage: 不核对交互对端 == from | `internal/daemon/receive.go:463` | 变红:TestEachReceiveStepRefusesWithItsClass, TestAForgedSenderInjectedAtTheHubIsDropped |
| si4-3 | SI-4 | ANet | authorizeReply: 不核对 status/result 发送方是该交互的 provider | `internal/daemon/receive.go:536` | 变红:TestEachReceiveStepRefusesWithItsClass |
| si4-4 | SI-4 | ANet | authorizeDelegate: TaskDoc 签名方不必是 inner.from | `internal/daemon/receive.go:348` | 存活,**等价**:TaskDoc 以已解析的发送方 KEL 验签,`identity.VerifyObject` 要求 `SignerAID` 等于该 KEL 的 AID,signer 恒等于 from |
| si4-5 | SI-4 | Core | VerifyEncKeySet: 去掉预期 AID(以 KEL 推出的 AID 代替) | `seal/enckeys.go:245` | 变红:TestVerifyEncKeySetRecipientSubstitution, TestAHubSubstitutingKeysForTheRecipientIsRefused |
| si4-6 | SI-4 | ANet | resolveSenderKEL: 内层 KEL 推出的 AID 不必等于 inner.from | `internal/daemon/receive.go:267` | 变红:TestEachReceiveStepRefusesWithItsClass |
| si5-1 | SI-5 | ANet | defaultInbound: 全新安装 inbound.policy=open | `internal/daemon/inbound.go:126` | 变红:TestAFreshInstallIsClosed, TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestAFreshInitIsSafeByDoctorJSON … |
| si5-2 | SI-5 | ANet | InboundConfig.normalize: 未写 policy 的配置按 open | `internal/daemon/inbound.go:135` | **存活 → 缺口**:全新安装的配置都显式写 policy。补 `TestAnInboundBlockWithoutAPolicyIsClosed`,复核变红 |
| si5-3 | SI-5 | ANet | defaultInbound: public_capabilities 默认非空 | `internal/daemon/inbound.go:126` | 变红:TestAFreshInstallIsClosed, TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestAFreshInitIsSafeByDoctorJSON … |
| si5-4 | SI-5 | ANet | UntrustedMode: auto_reply 块未写 untrusted 时按 sandbox | `internal/daemon/config.go:119` | 变红:TestExecRunsOnlyForTrustedPeers, TestOpenAndExecForUntrustedPeersConflict, TestAFreshInstallIsClosed |
| si5-5 | SI-5 | ANet | defaultPayments: auto_max=5 | `internal/daemon/spend.go:104` | 变红:TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestPaymentsBlockDefaults, TestAConfigWithoutPaymentsIsGivenTheDefaults … |
| si5-6 | SI-5 | ANet | defaultPayments: agent_max=5 | `internal/daemon/spend.go:104` | 变红:TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestInitOnlyAddsMissingKeys, TestPaymentsBlockDefaults … |
| si5-7 | SI-5 | ANet | defaultPayments: agent_daily_max=20 | `internal/daemon/spend.go:104` | 变红:TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestPaymentsBlockDefaults, TestAFreshInitIsSafeByDoctorJSON … |
| si5-8 | SI-5 | ANet | defaultPayments: payees_file 为空串(收款方白名单关闭) | `internal/daemon/spend.go:103` | 变红:TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent, TestInitOnlyAddsMissingKeys, TestPaymentsBlockDefaults … |
| si5-9 | SI-5 | ANet | PaymentsConfig.limits: 无 payments 块时收款方白名单关闭 | `internal/daemon/spend.go:81` | 变红:TestPaymentsBlockDefaults |
| si5-10 | SI-5 | ANet | doctor SI5(): agent_daily_max 改动不报告 | `internal/daemon/setup.go:440` | 变红:TestDoctorReportsEachSI5KeyThatChanged |
| si6-1 | SI-6 | ANet | 投影: completed 文本任务(无回执)不带 anet.receipt_verified | `internal/a2ashape/project.go:777` | 变红:TestSI6Sweep, TestTasksThroughMCPAreA2ATasksThatKeepSI6 |
| si6-2 | SI-6 | ANet | effectStatus: 终态能力任务无答复时报 OK(不知道→没问题) | `internal/a2ashape/project.go:871` | 变红:TestCapabilityWithoutAnswer, TestSI6Sweep |
| si6-3 | SI-6 | ANet | receiptVerified: unverified 投影为 verified | `internal/a2ashape/project.go:845` | 变红:TestUnverifiedReceiptStaysCompleted |
| si6-4 | SI-6 | ANet | metadata: UNVERIFIED 时不写 anet.effect_status | `internal/a2ashape/project.go:794` | 变红:TestCapabilityUnverifiedIsCompletedNotOK, TestEffectStatesTable, TestSI6Sweep … |
| si6-5 | SI-6 | ANet | Project: 效果 UNVERIFIED 或回执未验证时 state 改为 FAILED(合并两字段) | `internal/a2ashape/project.go:238` | 变红:TestUnverifiedReceiptStaysCompleted, TestCapabilityUnverifiedIsCompletedNotOK, TestSI6Sweep … |
| si7-1 | SI-7 | ANet | loopguard.AllowedHost: 只核端口,任何主机名都放行(DNS 重绑定) | `internal/loopguard/loopguard.go:56` | 变红:TestHostAllowlistRefusesOtherNames, TestRequestChecks, TestEachLocalSurfaceTakesOnlyItsOwnCredential … |
| si7-2 | SI-7 | ANet | authGate: 不在会话白名单的路由也放行会话 | `internal/daemon/ctlsec.go:223` | 变红:TestEveryRouteIsAllowlistedOrRefusedForASession, TestASessionCannotReachABearerOnlyHandler |
| si7-3 | SI-7 | ANet | sessionRoutes: 白名单放宽加入 POST /pull | `internal/daemon/ctlsec.go:83` | 变红:TestEveryRouteIsAllowlistedOrRefusedForASession |
| si7-4 | SI-7 | ANet | authGate: 任意 Bearer 都当作控制令牌 | `internal/daemon/ctlsec.go:209` | 变红:TestTheLocalA2ATokenIsRefusedByTheControlPlane, TestAuthenticatedRoutesNeedACredential |
| si7-5 | SI-7 | ANet | module/a2a authorized: 任意非空 Bearer 都放行(含控制令牌) | `module/a2a/server.go:158` | 变红:TestRequestChecks, TestEachLocalSurfaceTakesOnlyItsOwnCredential |
| si7-6 | SI-7 | ANet | 控制台页面 HTML 末尾带控制令牌 | `internal/daemon/ctlsec.go:136` | 变红:TestConsolePageHasNoTokenAndStrictHeaders |
| si7-7 | SI-7 | ANet | loopguard.LoopbackName: 0.0.0.0 也算回环(控制口可绑全网卡) | `internal/loopguard/loopguard.go:41` | 变红:TestTheControlTokenIsResolvedOnlyForALoopbackAddress, TestNonLoopbackControlAddrIsRefused, TestLoopbackName … |
| si8-1 | SI-8 | ANet | cmd/anet 无 tag 文件直接引入 a2a-go(no_a2a 构建仍带 SDK) | `cmd/anet/zz_mut.go(新文件)` | 变红:`tagcheck.sh check no_a2a`(no_a2a 构建出现 a2a-go 符号);Go 用例 TestABuildWithoutA2ADoesNotClaimIt 不看 SDK 符号,仍绿 |
| si8-2 | SI-8 | ANet | internal/mcpserv 引入 a2a-go | `internal/mcpserv/zz_mut.go(新文件)` | 变红:TestSI8NoA2AGoInTheClosure, TestSharedPackagesStayFreeOfTheDaemonAndTheSDK |
| si8-3 | SI-8 | ANet | internal/daemon 引入 a2a-go | `internal/daemon/zz_mut.go(新文件)` | 变红:TestSI8NoA2AGoInTheClosure, TestSharedPackagesStayFreeOfTheDaemonAndTheSDK |
| si8-4 | SI-8 | ANet | taskboard 改回减法 tag(module_taskboard.go 与 module/taskboard 默认编入) | `cmd/anet/module_taskboard.go:1, module/taskboard/taskboard.go:1` | 变红:TestTheDefaultBuildHasNoTaskboard, (exit 1) |
| si8-5 | SI-8 | Hub | anet-hub wire_taskboard.go 改回默认编入 | `cmd/anet-hub/wire_taskboard.go:1 等 3 处` | 变红:TestTheDefaultBuildHasNoTaskboard |
| si8-6 | SI-8 | ANet | internal/a2ashape 引入 a2a-go(控制面/MCP 经它间接依赖) | `internal/a2ashape/zz_mut.go(新文件)` | 变红:TestNoSDKImport, TestSI8NoA2AGoInTheClosure |
| si8-7 | SI-8 | ANet | module/a2a 各文件去掉 !no_a2a tag,且 cmd/anet/module_a2a.go 恒编入 | `cmd/anet/module_a2a.go:1 等 9 处` | 变红:TestABuildWithoutA2ADoesNotClaimIt, (exit 1) |
| si9-1 | SI-9 | Hub | CheckRequirements: 少付 1 仍通过 | `internal/aghub/facilitator.go:208` | 变红:TestSettleRefusesPaymentsThatDoNotMeetTheRequirements |
| si9-2 | SI-9 | Hub | CheckRequirements: 不比较授权收款方与 requirements | `internal/aghub/facilitator.go:195` | 变红:TestSettleRefusesPaymentsThatDoNotMeetTheRequirements, TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded, TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce |
| si9-3 | SI-9 | Hub | 入口 hub 转发前不核对 requirements | `internal/aghub/facilitator.go:379` | 变红:TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded, TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce |
| si9-4 | SI-9 | Hub | 账本 hub 不核对 requirements | `internal/aghub/facilitator.go:390` | 变红:TestSettleRefusesPaymentsThatDoNotMeetTheRequirements, TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce |
| si9-5 | SI-9 | Hub | 新结算不查授权有效期与当前密钥(settle 路径的 currentAuth) | `internal/aghub/facilitator.go:400` | 变红:TestAChargedAuthorizationGetsItsReceiptAfterItsWindow |
| si9-6 | SI-9 | Hub | 绑定唯一索引改为普通索引(同一绑定可两次扣款) | `internal/aghub/facilitator.go:739` | 变红:TestUpgradingAHubWithDuplicateBindingsKeepsTheFirstAsHolder, TestASecondAuthorizationForOneBindingMovesNothing |
| si9-7 | SI-9 | ANet | 商户核对: 不查收款方是本节点 | `module/x402/check.go:138` | 变红:TestTheMerchantCheckRefusesEachWrongTerm |
| si9-8 | SI-9 | ANet | 商户核对: 不查绑定值 pay_bind | `module/x402/check.go:144` | 变红:TestTheMerchantCheckRefusesEachWrongTerm, TestAWrongPaymentIsRefusedBeforeSettlement |
| si9-9 | SI-9 | ANet | 商户核对: 不查金额 ≥ 报价 | `module/x402/check.go:156` | 变红:TestTheMerchantCheckRefusesEachWrongTerm, TestAWrongPaymentIsRefusedBeforeSettlement |
| si9-10 | SI-9 | ANet | 商户核对: 不查授权过期 | `module/x402/check.go:159` | 变红:TestTheMerchantCheckRefusesEachWrongTerm |
| si9-11 | SI-9 | ANet | 商户核对: 不查报价过期 | `module/x402/check.go:134` | 变红:TestTheMerchantCheckRefusesEachWrongTerm |
| si9-12 | SI-9 | ANet | provider 接受他笔授权的结算回执(不比 AuthID) | `internal/daemon/x402task.go:476` | **存活 → 缺口**:没有 fake 会拿他笔回执冒充重放。fake hub 加 `foreign-receipt` 故障,补 `TestAReplayedSettlementOfAnotherPaymentIsRefused`,复核变红 |
| si10-1 | SI-10 | ANet | rxResult.ack 恒 true(暂时性失败也 ack) | `internal/daemon/receive.go:64` | 变红:TestAStoreFailureThenRedeliveryIsProcessedOnce, TestAPeerRecordReadErrorIsTemporary, TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce |
| si10-2 | SI-10 | ANet | p2p deliverInbound: Receive 失败仍回 ack | `module/p2p/transport.go:222` | 变红:TestATemporaryRefusalIsNotAcked |
| si10-3 | SI-10 | ANet | p2p ack 键改常量(设计 §3.10 指定的 mutation) | `module/p2p/transport.go:225` | 变红:TestConcurrentDeliveriesAreAckedByTheirOwnID |
| si10-4 | SI-10 | ANet | process: 去掉 (from, mid) 进程内锁 | `internal/daemon/receive.go:553` | **存活 → 缺口**(聊天消息靠事务内 claim 即可去重,锁保护的是提交后的副作用)。补 `TestASecondCopyWaitsForTheFirstUnderTheMessageLock`,复核变红 |
| si10-5 | SI-10 | ANet | commitRx: 存储错误按永久失败(ack 并丢弃) | `internal/daemon/receive.go:695` | 变红:TestAStoreFailureThenRedeliveryIsProcessedOnce, TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce |
| si10-6 | SI-10 | ANet | pollOnce: 暂缓的信封也 ack | `internal/daemon/relay.go:511` | 变红:TestAMessageForAnUnknownTaskGetsTaskNotFound, TestAStoreFailureThenRedeliveryIsProcessedOnce, TestHeldBackEnvelopesDoNotBlockNewerMail … |
| si10-7 | SI-10 | ANet | p2p 第 0 步限速返回 nil(被限速的投递被 ack) | `internal/daemon/transport.go:184` | 变红:TestTheP2PRateLimitIsTemporary |
| si10-8 | SI-10 | ANet | anetpeer: 收到任一 ack 释放全部等待中的投递 | `tools/anetpeer/main.go:274` | 变红:TestConcurrentDeliveriesGetTheirOwnOutcome |

各条 SI 的关键测试(按 mutation 实际变红的用例归纳):

| SI | 承担的测试 | 脚本断言依赖的代码路径 |
|---|---|---|
| SI-1 | seal 金标/往返(`TestVEC_SEALED_1`、`TestOpenRecipientAndKey`)、daemon `TestTheHubHoldsOnlyCiphertext`、hub `TestTheRelayRefusesAnythingButASealedEnvelopeForTheRecipient`、`TestAReviewUploadCarryingContentIsRefused`、admin `TestOfficialAgentOperationsAreGone`/`TestAManifestWithARemovedSectionIsRefused`、x402 `TestSettleSendsRequirementsAndNothingAboutTheWork`、scripts `canary_test.go` | joint.sh 段 C:`canary.py` 的编码针与 settle 体检查(si1-6、si1-7)、admin manifest 拒收与路由 404(si1-5、si1-8);整条链路见 §5 |
| SI-2 | hub `TestTheRelayDoesNotStoreTheSender`、`TestAnAckDeletesTheRow`、`TestAnAckedEnvelopeLeavesNoBytesOnDisk`、`TestUndeliveredEnvelopesExpire`、新 `TestASendLeavesTheSenderInNoTable`;daemon 付款绑定(`TestAQuotedTaskIsPaidAndCompletedOnTheSameTask`) | — |
| SI-3 | seal `TestSignedInnerFieldMutations`、`TestInnerKeySpace`、`TestCheckTime`、`TestParseOuter`;daemon `TestEachReceiveStepRefusesWithItsClass`、重投用例、新 `TestCommitRxRunsTheBusinessWriteOncePerMessage` | joint.sh 9/11 |
| SI-4 | daemon `TestAForgedSenderInjectedAtTheHubIsDropped`、`TestEachReceiveStepRefusesWithItsClass`、`TestAHubSubstitutingKeysForTheRecipientIsRefused`;seal `TestVerifyEncKeySetRecipientSubstitution` | joint.sh 8/11(§5) |
| SI-5 | daemon `TestAFreshInstallIsClosed`、`TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent`、`TestPaymentsBlockDefaults`、新 `TestAnInboundBlockWithoutAPolicyIsClosed`;cmd/anet `TestAFreshInitIsSafeByDoctorJSON`、`TestDoctorReportsEachSI5KeyThatChanged` | — |
| SI-6 | a2ashape `TestSI6Sweep`、`TestUnverifiedReceiptStaysCompleted`、`TestCapabilityUnverifiedIsCompletedNotOK`、`TestEffectStatesTable`;mcpserv 契约 `TestTasksThroughMCPAreA2ATasksThatKeepSI6` | — |
| SI-7 | daemon `TestHostAllowlistRefusesOtherNames`、`TestEveryRouteIsAllowlistedOrRefusedForASession`、`TestTheLocalA2ATokenIsRefusedByTheControlPlane`、`TestConsolePageHasNoTokenAndStrictHeaders`;module/a2a `TestRequestChecks`、`TestEachLocalSurfaceTakesOnlyItsOwnCredential`;loopguard 单测 | — |
| SI-8 | cmd/anet `TestABuildWithoutA2ADoesNotClaimIt`(-tags no_a2a)、`TestTheDefaultBuildHasNoTaskboard`;mcpserv `TestSI8NoA2AGoInTheClosure`;loopguard `TestSharedPackagesStayFreeOfTheDaemonAndTheSDK`;a2ashape `TestNoSDKImport`;hub `TestTheDefaultBuildHasNoTaskboard` | `scripts/tagcheck.sh`(si8-1 只有它能抓到) |
| SI-9 | hub `TestSettleRefusesPaymentsThatDoNotMeetTheRequirements`、`TestAForeignLedgerPaymentIsCheckedBeforeItIsForwarded`、`TestASecondAuthorizationForOneBindingMovesNothing`、cmd/anet-hub `TestACrossHubPaymentIsCheckedAtBothHubsAndCreditedOnce`;x402 `TestTheMerchantCheckRefusesEachWrongTerm`;daemon `TestAWrongPaymentIsRefusedBeforeSettlement`、新 `TestAReplayedSettlementOfAnotherPaymentIsRefused` | scenario.sh §8 跨 hub 付费负面用例(本轮未跑) |
| SI-10 | daemon `TestAStoreFailureThenRedeliveryIsProcessedOnce`、`TestAStoreFailureOnTheP2PCopyThenRedeliveryThroughTheHubIsProcessedOnce`、`TestTheP2PRateLimitIsTemporary`、新 `TestASecondCopyWaitsForTheFirstUnderTheMessageLock`;module/p2p `TestATemporaryRefusalIsNotAcked`、`TestConcurrentDeliveriesAreAckedByTheirOwnID`;anetpeer `TestConcurrentDeliveriesGetTheirOwnOutcome` | joint.sh 10/11 |

## 4. §17 补充用例

| # | 组 | 仓 | mutation | 位置(HEAD 行号) | 结果 |
|---|---|---|---|---|---|
| c16-1 | C16/C3 | Core | DecideHighWater: 同 seq 同字节(==)判为分叉(设计指定 mutation) | `seal/highwater.go:70` | 变红:TestDecideHighWater, TestDecideHighWaterSigned, TestDecideHighWaterSignedUsesSetSeq … |
| c16-2 | C16/C3 | Core | DecideHighWater: 旧 seq 判为替换(回退) | `seal/highwater.go:68` | 变红:TestDecideHighWater, TestDecideHighWaterSigned, TestDecideHighWaterSignedUsesSetSeq … |
| c16-3 | C16/C3 | ANet | mergeKEL: 内层 KEL 短于已存(回退)时拒收而不是用已存 | `internal/daemon/peerkel.go:99` | 变红:TestPeerKELIsExtendedNeverRolledBack, TestATruncatedKELIsRefusedAfterTableChurnAndRestart, TestRotationGrace |
| c16-4 | C16/C3 | ANet | mergeKEL: 与已存等长(==)的 KEL 判为分叉 | `internal/daemon/peerkel.go:97` | 变红:TestAnUnchangedKeySetIsAcceptedAgain, TestRelayDelegationRoundTrip |
| c1-1 | C1 | ANet | leftoverAction: working 的长能力调用也重跑(破坏至多一次) | `internal/daemon/wire_edges.go:105` | **存活 → 缺口**:恢复用例要么重启时没注册 provider,要么长调用只到 submitted。补 `TestALongCallLeftWorkingIsNotRunAgainWhenItsProviderIsBack`,复核变红 |
| c1-2 | C1 | ANet | recoverInterrupted: 长调用不报 interrupted | `internal/daemon/capability.go:675` | 变红:TestAnInterruptedLongCallIsReportedAtStart, TestAStopLeavesALongCallToStartupRecovery, TestALongCallRecordedButNeverStartedIsReportedAtStart |
| c1-3 | C1 | ANet | redeliveredDelegate: 短调用不重跑(破坏至少一次) | `internal/daemon/delegation.go:1046` | 变红:TestAStopDoesNotAnswerAShortCallItCutOff |
| c12-1 | C12 | ANet | recordSenderIdentity: public_cap 请求方写入 peer_identity | `internal/daemon/receive.go:619` | 变红:TestATruncatedKELIsRefusedAfterTableChurnAndRestart |
| c12-2 | C12 | ANet | mergeKEL: 已存延伸内层(截断 KEL)时改用内层 | `internal/daemon/peerkel.go:99` | 变红:TestATruncatedKELIsRefusedAfterTableChurnAndRestart, TestRotationGrace |
| c12-3 | C12 | ANet | evictPeers: 淘汰不看 pinned_reason 与活动交互 | `internal/runtime/interactions/peers.go:330` | 变红:TestPeerIdentityPinsAndEviction |
| m-ix-1 | m(ix 碰撞) | ANet | authorizeDelegate: ix 碰撞只看对端不看角色 | `internal/daemon/receive.go:356` | **存活 → 缺口**(第 10 步事务内另有同一检查,计数相同)。补 `TestACollidingDelegationIsRefusedBeforeAnyWrite`(含能力调用一例),复核变红;两处都去掉见 m-ix-3 |
| m-ix-2 | m(ix 碰撞) | ANet | authorizeDelegate: 完全去掉 ix 碰撞检查 | `internal/daemon/receive.go:356` | **存活 → 缺口**,同上;补同一用例,复核变红 |
| m-ix-3 | m(ix 碰撞) | ANet | ix 碰撞两处检查(第 9 步与第 10 步事务内)都去掉 | `internal/daemon/receive.go:356, internal/daemon/delegation.go:935` | 变红:TestEachReceiveStepRefusesWithItsClass |
| c19-1 | C19 | ANet | 未知 ix 的 message 不等待,立即 TaskNotFound | `internal/daemon/receive.go:455` | 变红:TestAMessageForAnUnknownTaskGetsTaskNotFound, TestHeldBackEnvelopesDoNotBlockNewerMail, TestAHeldEnvelopeIsRetriedAtOnePollPerRound |
| c19-2 | C19 | ANet | 未知 ix 等待窗口常量 10 分钟 → 100 分钟 | `internal/daemon/receive.go:118` | **存活 → 缺口**:其他用例用 `unknownIXWait` 本身算窗口。补 `TestTheUnknownTaskWindowIsTenMinutes`,复核变红 |
| c6-1 | C6/C29 | ANet | public_cap 交互上的 payment-submitted 文本消息被当作文本拒收 | `internal/daemon/receive.go:493` | 变红:TestAPublicCapabilityCallTakesNoText, TestAStrangerPaysForAPublicCapability |
| c6-2 | C6/C29 | ANet | 能力调用上的 end_request 走文本任务的完成路径(签对话记录回执) | `internal/daemon/delegation.go:1322` | 变红:TestAQuotedTaskEndedByTheRequesterIsCanceledWithoutAReceipt |
| c6-3 | C6/C29 | ANet | 自动回复循环不再跳过能力调用与 public_cap 交互 | `internal/daemon/autoreply.go:285` | 存活,**等价**:`ActiveThreads` 已在 SQL 里排除能力调用与 public_cap;两层都去掉见 c6-4 |
| c6-4 | C6/C29 | ANet | 自动回复两层过滤都去掉: ActiveThreads 与循环都不排除能力调用/public_cap | `internal/daemon/delegation.go:165, internal/daemon/autoreply.go:285` | 变红:TestAPublicCapabilityCallTakesNoText, TestExecAutoReplyOnBothSidesStaysOutOfAPaidCall |
| c8-1 | C8/C9 | ANet | validatePolicy: open 与 exec untrusted=sandbox 不再冲突 | `internal/daemon/inbound.go:251` | 变红:TestOpenAndExecForUntrustedPeersConflict |
| c9-1 | C8/C9 | ANet | backendTakes: 未声明 untrusted 的后端也接 allow 但不在 trust 的对端 | `internal/daemon/inbound_tasks.go:164` | 变红:TestInboundTasksReachTheBackendOnlyFromTrustedPeers, TestBackendTakes |
| c9-2 | C8/C9 | ANet | exec 自动回复: untrusted=off 时仍为非信任对端运行 | `internal/daemon/autoreply.go:322` | 变红:TestExecRunsOnlyForTrustedPeers |
| c9-3 | C8/C9 | ANet | backendTakes: open 下的陌生人(trust=public)也交给声明了 untrusted 的后端 | `internal/daemon/inbound_tasks.go:163` | 变红:TestInboundTasksWithAnUntrustedBackend, TestBackendTakes |
| c22-1 | C22 | ANet | SendMessage: 客户端给的 contextId 被新生成的覆盖 | `internal/daemon/taskseam.go:319` | 变红:TestAContextOnlyMessageContinuesTheTaskWaitingForIt, TestClientContextIsKeptAndRetriesAreDeduplicated |
| c25-1 | C25 | ANet | settlement_pending 当作已结算(未知 → 成功) | `internal/daemon/x402task.go:457` | 变红:TestAnUnknownSettlementIsRetriedAndChargedOnce |
| c25-2 | C25 | ANet | 结算结果未知时不启动重试循环 | `internal/daemon/x402task.go:462` | 变红:TestAnUnknownSettlementIsRetriedAndChargedOnce |
| c25-3 | C25 | ANet | ErrorCode: settlement_pending 映射为终结的 SETTLEMENT_FAILED | `module/x402/check.go:75` | 变红:TestTheErrorReasonTableIsPinned, TestAnUnknownSettlementIsRetriedAndChargedOnce |
| c33-1 | C33 | ANet | 重投的 delegate 不重发已签回执的结果 | `internal/daemon/delegation.go:1024` | 变红:TestARedeliveredDelegateResendsTheAnswer, TestAnEndedPublicCallAnswersARedeliveredDelegateOnce, TestARedeliveredPaidCallResendsItsReceipts |
| c33-2 | C33 | ANet | resendResult: 队列里已有结果时仍再封装一份(重发两次) | `internal/daemon/delegation.go:1622` | 存活,**等价**:outbox 按 (ix, typ, digest) 去重,再排一份得到同一行;两层都去掉见 c33-3 |
| c33-3 | C33 | ANet | 重投时结果已在队列: 既催队列又另排一份(且 outbox 不去重) | `internal/daemon/delegation.go:1622, internal/runtime/interactions/outbox.go:133` | 变红:TestAQueuedAnswerIsNotQueuedAgainForARedelivery |
| c34-1 | C34 | ANet | 能力调用: payment-submitted/已结算后收到取消仍取消 | `internal/daemon/delegation.go:1399` | **存活 → 缺口**:测试 provider 被叫停时照样报 OK。`meteredWork` 记录 ctx 结束,`TestACancelAfterASettledPaymentLeavesTheTaskOpen` 断言未被叫停,复核变红 |
| c34-2 | C34 | ANet | 文本任务: payment-submitted/已结算后收到取消仍取消 | `internal/daemon/delegation.go:1377` | 存活,**等价**:provider 侧文本任务不产生付款状态(报价只对能力调用),该分支不可达 |
| c35-1 | C35 | ANet | finish: 终态行也可被改写(取消后 SetResult 改变状态) | `internal/runtime/interactions/interactions.go:648` | 变红:TestStateWritesAreGuarded |
| c35-2 | C35 | ANet | SendMessage: 对终态任务不返回 UnsupportedOperation | `internal/daemon/taskseam.go:424` | 存活,**等价**:`sendMessage` 对终态任务返回 `ErrTaskTerminal`,`appendTask` 同样映射为 UnsupportedOperation |
| c35-3 | C35 | ANet | 阻塞式追问不等 provider 的新回复(after 置 0) | `internal/daemon/taskseam.go:452` | 存活,**等价**:追问在等待前已同步写入新的 state_seq,after=0 与 after=N 等到的是同一事件 |
| c35-4 | C35 | ANet | 自动回复循环不跳过终态(canceled/rejected)交互 | `internal/daemon/autoreply.go:285` | 存活,**等价**:`ActiveThreads` 以 `Active:true` 在 SQL 里排除终态;两层都去掉见 c35-5 |
| c35-5 | C35 | ANet | 自动回复两层过滤都去掉: ActiveThreads 与循环都不排除终态交互 | `internal/daemon/delegation.go:165, internal/daemon/autoreply.go:285` | 关键测试绿;整包由 `TestSandboxUnavailableFailsClosed` 变红(被拒的沙箱轮次每次扫描都重记:4 条调用事件,应为 2) |
| c27-1 | C27 | ANet | agentTier: auto 档不计入 agent_daily_max | `internal/daemon/spend.go:170` | 变红:TestTheSpendingPolicyTiers |
| c27-2 | C27 | ANet | AdmitSpend: 检查与记录不在一把锁下 | `internal/daemon/spend.go:206` | 只在 `-race` 下变红:TestConcurrentSpendsDoNotBothPass(DATA RACE);普通运行仍绿 |
| c27-3 | C27 | ANet | AdmitSpend: 通过后不记账(日累计永不增长) | `internal/daemon/spend.go:233` | 变红:TestTheSpendingPolicyTiers, TestConcurrentSpendsDoNotBothPass |
| c27-4 | C27 | ANet | x402 Authorize: 忽略 AdmitSpend 的拒绝 | `module/x402/pay.go:166` | 变红:TestARefusedSpendSignsAndRecordsNothing, TestEverySigningSurfaceIsHeldToTheSpendingPolicy, TestTheSpendingPolicyTiers |
| m-deny-1 | m(deny) | ANet | cancelForPolicy: deny 后不取消活动交互 | `internal/daemon/inbound.go:1048` | 变红:TestDenyingAPeerCancelsItsInteractions |
| m-deny-2 | m(deny) | ANet | authorizeMessage: 已有交互上 deny 对端的后续消息不丢弃 | `internal/daemon/receive.go:463` | 变红:TestDenyingAPeerCancelsItsInteractions |
| m-deny-3 | m(deny) | ANet | 自动回复不再每轮读 deny 名单 | `internal/daemon/autoreply.go:310` | 变红:TestTheOpenAIBackendReadsTheDenyListEveryTurn |
| c42-1 | C42 | ANet | safeName: 不中和前导点文件名 | `internal/daemon/attachments.go:445` | 变红:TestPullNeutralizesDotfilesAndRepullIsANoOp, TestSafeName |
| c42-2 | C42 | ANet | pullForbiddenRoots: 不保护数据目录与 exec 工作目录 | `internal/daemon/attachments.go:360` | 变红:TestPullRefusesEmptyRelativeAndProtectedOutDirs |
| c2-1 | C2 | ANet | recipientKeys: 超过缓存时限即只信 hub(hub 404 时丢掉仍有效的已存密钥) | `internal/daemon/seal_send.go:210` | **存活 → 缺口**:复核用例的 hub 总能答出密钥。补 `TestAStoredKeySetOutlivesAHubThatNoLongerAnswersForIt`,复核变红 |
| c2-2 | C2 | ANet | ApprovePending: 批准时不记录待批项携带的密钥 | `internal/daemon/pending.go:225` | **存活 → 缺口**:批准用例的 hub 总能答出请求方密钥。补 `TestAnApprovedTaskIsAnsweredWithTheKeysItWasHeldWith`,复核变红 |
| c32-1 | C32 | Hub | hKeysGet: 不做 /fed/v2/keys 联邦查询 | `internal/aghub/keys.go:266` | 变红:TestAHubLocalAgentOnAPeerCanBeReachedEncrypted |
| c32-2 | C32 | Hub | -test-no-fed-key-lookup 开关失效(scenario.sh 8.6 的 mutation 不再生效) | `cmd/anet-hub/wire_federation.go:41` | 变红:TestTheTestSwitchTurnsOffOnlyTheFederatedKeyLookup |
| m-p2p-1 | m(p2p 旧帧) | ANet | anetpeer: 无 V 的旧帧也交给 daemon | `tools/anetpeer/main.go:328` | 变红:TestAnUnversionedDeliveryIsRefused |
| m-p2p-2 | m(p2p 旧帧) | ANet | module/p2p: 无 V 的投递帧也调用 Receive | `module/p2p/transport.go:204` | 变红:TestAnUnversionedDeliveryIsNotHandedToTheDaemon |
| m-err-1 | m(errorReason 映射) | ANet | 映射表: duplicate_binding → SETTLEMENT_FAILED | `module/x402/check.go:53` | 变红:TestTheErrorReasonTableIsPinned |
| m-err-2 | m(errorReason 映射) | ANet | x402.payment.error 字符串 DUPLICATE_NONCE 改拼写 | `internal/x402a2a/x402a2a.go:68` | 变红:TestTheErrorReasonTableIsPinned, TestTheWireStringsArePinned |

## 5. 联调级 mutation(joint.sh)

本机跑,端口段由 joint.sh 随机取空闲块,`JOINT_DEVICES=0`(1/11 设备链按设计跳过),工作目录 `/tmp/fmj/*`
(`J` 放在 scratchpad 会超过 peer socket 的路径长度上限,joint.sh 拒绝)。`mutate.sh` 从三仓工作树复制到临时目录再打补丁,
不碰检出。

| 运行 | 结果 |
|---|---|
| 基线,`JOINT_CANARY_ONLY=1` | 39 / 0;段 C 各面 0 命中,自对照(provider/requester 自己的数据)找到全部 canary |
| 基线,全量 | 95 / 0 |
| `mutate.sh canary si1-plaintext-envelope.patch`(关闭加密) | 37 / 2,**被抓到**:hub 数据目录 WAL 9 个 canary、tap 记录的 hub 流量 15 个;`mutate.sh check` 判 caught(24 处命中) |
| `mutate.sh canary si1-restore-official-harvest.patch`(恢复采集源) | 37 / 2,**被抓到**:admin 对带 monitor/harvest 的 manifest 答 200;admin.db 的 `session.goal` 等 3 处命中 official-args |
| `si4-skip-step7.patch` 全量(第 7 步 bad-sig 放行) | 90 / 5,**被抓到**:8/11 四条红(bad-sig 计数不动、p2p 同样放行、伪造文本落进请求方的任务、provider 证据链多出 2 条),9/11 重放的伪造也不再被拒;from-mismatch(第 6 步)与 not-peer(第 9 步)仍绿,与补丁头部的预期一致 |

## 6. 发现与遗留问题

1. **没有产品代码缺陷**。本轮 20 个存活 mutation 里 13 个是测试缺口(已补,见 §1 提交),7 个是等价 mutation。
2. **靠冗余防线的检查**(已补测试,记在这里供评审知道哪些检查是"第二道"):第 9 步与第 10 步事务内各有一道 ix 碰撞检查
   (m-ix-1/2);`(from, mid)` 进程内锁与重放行的事务内 claim 互为冗余(si3-8、si10-4);autoreply 循环对终态、能力调用、
   public_cap 的过滤与 `ActiveThreads` 的 SQL 过滤重复(c6-3、c35-4);outbox 按 digest 去重使 resendResult 的"只催不再排"
   成为冗余(c33-2)。
3. **只在非 Go 测试或 `-race` 下能抓到的 mutation**:si8-1(无 tag 文件直接引入 a2a-go)只有 `scripts/tagcheck.sh`
   抓得到,Go 用例 `TestABuildWithoutA2ADoesNotClaimIt` 只看模块注册表;c27-2(AdmitSpend 检查与记录不在一把锁下)只在
   `-race` 下变红。两者都依赖 CI 真的跑 `tagcheck.sh` 与 `-race`,门禁配置时要保留这两步。
4. **scripts/canary.py 的 URL-safe base64 针在 joint 的 canary 字母表下是死代码**(lib.sh 生成 `anet-canary-<label>-<hex>`,
   标准与 URL-safe 编码相同)。已补用含 `?~>` 的 canary 的用例钉住这条路径;是否让 lib.sh 的 canary 带上这些字符以真正覆盖
   URL-safe 形态,留给 canary 工作包决定(改 canary 字母表会影响 joint 其余段的字符串处理)。
5. **设计 §17 C32 行与实现不一致(已知)**:设计(integ/round4c 所含 r3)写"关闭 /fed/v2/keys 查询后 hub-local provider 与超时限回复
   两例均失败",scenario.sh 8.6 按 §3.5 [C2] 断言"超时限回复复核失败但仍送达";0022(r4 修订清单,wp/designsync)已把 C32 行
   改为后者,尚未合入本基线。本轮单元级 C32 两个 mutation(c32-1 hub 不做联邦查询、c32-2 测试开关失效)均变红;
   c2-1 的新用例正是"hub 404 时继续用已存"这一面。
6. 本轮没有跑 scenario.sh(§8 跨 hub 付费负面、C2/C32 的联调面由 scenario 工作包与测试网段负责);C2、C32、C34 的
   联调级 mutation 未在本记录内复核。

## 7. 复现

```bash
source wt/f-mut/env.sh; cd wt/f-mut/ANet
python3 scripts/mutations/unitmut.py --check scripts/mutations/unit/si.py     # 清单是否还套得上
python3 scripts/mutations/unitmut.py scripts/mutations/unit/si.py              # SI-1..SI-10,约 25 分钟
python3 scripts/mutations/unitmut.py scripts/mutations/unit/supp.py            # §17 补充,约 25 分钟
python3 scripts/mutations/unitmut.py scripts/mutations/unit/supp.py c34-1 c2-1 # 指定几个
# 结果:${UNITMUT_OUT:-$TMPDIR/anet-unitmut-<uid>}/results.json 与 logs/<id>.log
# 联调级:
J=/tmp/fmj/x JOINT_DEVICES=0 bash scripts/mutations/mutate.sh canary scripts/mutations/si1-plaintext-envelope.patch
bash scripts/mutations/mutate.sh build -o /tmp/fmj/bin scripts/mutations/si4-skip-step7.patch
JOINT_BIN=/tmp/fmj/bin J=/tmp/fmj/si4 JOINT_DEVICES=0 bash scripts/joint.sh
```

跑时工作树必须干净(工具用 `git stash push -u` 还原,未提交的文件会被一起收走再丢弃);同一工作树不要同时跑两个。
