# 付费:x402、凭证、兑付

> 付费是一个**可插拔模块**(`module/x402`)。`-tags no_x402` 编出来的 daemon 里没有 facilitator 客户端、没有兑付路径、也没有那个公开监听口 —— 符号数为 0,不是"关掉了"。这样的节点遇到标价的能力会**拒绝并说明原因**,而不是免费替人干活。
>
> 模块从内核只拿一样东西:以本节点身份签一次名,外加它结算所在 hub 的身份。签名**就是**付款(付款方没签的授权不算授权),所以这个口子小不下去 —— 因此它有名字(`module.PaymentSeam`)而不是隐含在别处。

写给要把能力卖出去、或者要花钱买别人能力的人。也写给想知道"钱到底在谁手里"的人 —— 这一节的答案是明确的,而且不好听:**在 hub 手里**。

## 一句话

能力可以标价。买家签一张只对这一次工作有效的授权,hub 移动额度并签字,双方各自把这件事记进自己的证据链。**余额是 hub 的,记录是双方的。**

v0.2 起,经中继的付费调用按 [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) v0.2 在**同一个 A2A 任务**里完成:报价是任务的 `input-required`,付款、结算收据与结果都落在同一个任务上。付款对象是 x402 v2 的 `PaymentRequired` / `PaymentPayload` / `SettlementResponse`,承载在 `x402.payment.*` 元数据键下;daemon 之间的这些消息与任务内容一样在端到端加密的信封里传递。

## 托管:先把不好听的说清楚

credit 记在 hub 的库里。hub 可以增发,可以在你不知情时改一个数字,可以收了钱不给你兑付。协议阻止不了这些,代码也阻止不了。

它能做到的是另一件事:**hub 改不掉双方手里已经签好的记录**。

- 付款方手里有自己签的授权(金额、收款方、账本、这一次交互,全在签名里)
- 收付双方手里都有 hub 签的结算收据
- 两边的证据链上各有一条,链是仅追加、可验分叉的

所以一笔账对不上时,这是一场**双方都拿得出记录的争执**,而不是一方的说法。你注册到哪个 hub,就是选择信任谁 —— 这句话写在 `ANetCore/payment` 的包注释里、写在 hub 的代码里,也写在这里。

### 供应量是可核验的,发放是可追责的

`GET /x402/supply` 公开 hub 的负债:已发行、已兑付、未清偿。`outstanding == balances` 是任何人都能自己重算的等式。

但这只证明**内部一致** —— 这些行是 hub 自己写的。转账不存在这个问题(结算会留下 hub 签名的收据给付款方,并写进双方的证据链),发放没有对手方,所以此前没有任何 hub 之外的东西为它作证。

现在每一笔供应变化都进一条**仅追加、哈希前向链接、hub 签名**的链:

```
GET /x402/issuance          整条链,每条都带可独立验签的记录
GET /x402/issuance/head     当前链头,见证者签的就是它
GET /x402/witnesses         别人对这个 hub 签过的见证
```

`/x402/supply` 同时公布链上的合计与账表的合计,以及两者是否一致 —— **比对本身才是重点**。链之前就已流通的额度记为一条 `chain_opening`,单独列出而不并入 `chain_issued`:前者是 hub 关于自己过去的自述,后者每一条都有签名记录可被见证者钉住。

节点侧对应两条命令:

```
anet audit-hub    拉取并验证发放链, 与本节点此前记录的链头比对
anet reconcile    把本节点签过/收到的付款与 hub 记的这个账户的流水比对
```

**见证**是让这一切对新读者也有意义的部分。链只对"已经持有旧副本"的人可验分叉;从未被观察过的 hub 可以出示任意一条链。见证者定期取链头并签名"某时刻这条链的头是 N,id 是 X"。之后链在同一位置出现不同记录,两份签名(一份 hub 的记录、一份见证者的声明)就构成改写的证据,**不需要任何一方承认**。

见证品存在**见证者**手里才有意义 —— 只由被审计方保管的证据不是证据。hub 也接收并公布别人对它的见证,那是给读者一个起点:hub 可以扣着不发,但伪造不了。

- **hub 之间**:默认开,`federation.json` 里 `"witness":"off"` 关掉
- **agent**:默认关,`modules.x402.witness_hub: true` 打开。精简版节点不该被要求为网络干活;打开的节点收益最直接 —— 它拿到了关于自己余额所在账本的独立证据

**仍然做不到的**:阻止发放。hub 是发行方,可以随时创造 credit。变的是它无法**追溯地**或**无声地**做这件事。

## 三条路

### 一、同一任务里的付款(a2a-x402,最常用)

```
anet delegate <provider-aid> --capability text.digest.paid --args '{"text":"hi"}'          # 看报价,自己决定
anet delegate <provider-aid> --capability text.digest.paid --args '{"text":"hi"}' --pay    # 报价就付(网关档)
```

发生了什么(同一个任务,`interaction_id` 就是 A2A task id):

1. 先照常投一次。提供方**报价而不是拒绝**:任务进入 `input-required`,元数据 `x402.payment.status: payment-required`,`x402.payment.required` 里是报价(收款方、金额、网络、有效期)。报价 24 小时过期。一句没人能回头指认的报价不算报价,所以它记进提供方的证据链(`anet.payment.quoted`)。
2. 你的节点按**支出策略**(见下节)决定:在自动档之内,签一张授权并以 `payment-submitted` 发回同一任务;否则任务在本机停在 `input-required`,`anet.reason=needs_operator_approval`,等你或你的 agent 决定(MCP `submit_payment` / `reject_payment`,或终端上 `anet pay <ix>` / `anet pay <ix> --reject`)。拒付时双方都置 `canceled`。
3. 授权把这一次工作钉住:授权里的交互绑定值是 `pay_bind = SHA-256("anet/x402-bind/v1" ‖ ix ‖ task_nonce)`,只对这个任务有效,hub 也无法由它反推交互 id。
4. 提供方**结算前先核对**:收款方是自己、金额不低于报价、绑定值对得上、付款方式在报价选项内、授权与报价都未过期。不符就回 `payment-failed`,不结算、不执行。
5. 核对通过,提供方拿授权去 hub 结算,**先结算后干活**。顺序是有意的:后结算意味着干完才发现收不到钱;先结算意味着活失败了钱已经付了。选第二个,因为第二种情况证据模型说得清楚 —— 效果和付款都在两条链上,退款是一场有记录的商量。结算成功后提供方先发 `payment-verified`,再执行。
6. 结果回来时带 `payment-completed` 与 `x402.payment.receipts`(hub 签的结算收据在收据的 `extensions["anet.settlement.receipt"]`)。你的节点核对收据的授权 id 属于本任务、收款方是对端、金额与授权一致,才记 `anet.payment.settled{verified:true}`。

**`payment-verified` 的含义与规范不同。** a2a-x402 规范与参考实现里,`payment-verified` 表示"付款已验过、尚未扣款"(先验、执行、再结算);anet 里它表示**已经扣款**(先结算、再执行)。原因同第 5 步。只按规范理解这个状态的客户端会低估已发生的事:看到 `payment-verified` 时钱已经动了。

付款提交之后:

- 取消不能撤回付款。付款已提交后你再取消,任务仍是 `working`,带 `anet.cancel_requested=true`,等提供方交付;提供方在结算成功后不接受取消,完成并交付,交付不了时回 `failed` 并附结算收据。
- 结算结果未知(网络错误、超时、hub 回 `settlement_pending`)时,提供方用同一份授权重试到有确定结果;hub 对同一授权按 id 幂等,已扣款的授权总是回原收据。每个任务在得到确定结果之前只有一个未决授权。自动档对一份报价只签一次授权(对方重新报价时重发同一授权);付款确定失败之后不再自动重签,由你或你的 agent 决定,免得对方靠反复报价耗光自动额度。

看余额:`anet balance`。取钱:见下。

### 谁能花钱:三档支出上限

```json
"payments": { "auto_max": 0, "agent_max": 0, "agent_daily_max": 0,
              "explicit_max": 10, "daily_max": 50, "payees_file": "payees.allow" }
```

| 档 | 谁触发 | 单笔上限 | 日累计 |
|---|---|---|---|
| 自动(`task-auto`) | daemon 收到报价时自己付 | `auto_max` | 计入 `agent_daily_max` 与 `daily_max` |
| agent(`task-agent`) | MCP `submit_payment`;本机 A2A 客户端在同一任务上发 `payment-submitted` | `agent_max` | 计入 `agent_daily_max` 与 `daily_max` |
| 人工(`task-manual`) | `anet pay <ix>`,在终端上确认 | `explicit_max` | `daily_max` |
| 网关(`gateway`) | `anet delegate --pay`、`anet x402-authorize` | `explicit_max` | `daily_max` |
| 兑付(`redeem`) | `anet redeem` | `explicit_max` | `daily_max`,不受收款方名单约束 |

- 新节点**什么都不自动花**:自动档与 agent 档都是 0。经 MCP 或本机 A2A 接口提交的付款属于 agent 档,不视为你本人的同意。
- 收款方名单 `payees.allow`(数据目录下,一行一个 AID,手工编辑):键非空即启用,文件缺失等于空表;`anet init` 建一个空文件。名单外的收款方一律拒绝(兑付除外,它的收款方是 hub)。
- 改上限:`anet payments set auto_max=… agent_max=… agent_daily_max=… explicit_max=… daily_max=…`,要在终端上确认;`anet payments` 显示当前上限与最近 24 小时签过的授权额。日累计按**已签授权额**计,不是已结算额。
- 控制台不能授权付款。终端确认在 CLI 进程里做,挡得住只经 MCP 或 A2A 接口行事的 agent,挡不住能读控制令牌的本机程序([已知局限](KNOWN-LIMITATIONS-zh.md)第 13 条)。
- 付费演示要先把演示 agent 的 AID 写进 `payees.allow`,并在终端上放开 agent 档上限。

### 二、hub 上的 x402 门面(买家不必是 daemon)

```
GET /x402/resource/{aid}/{capability}
```

第一次访问回 `402` + `PAYMENT-REQUIRED` 头,body 里有价钱和**取货地址**。带上 `PAYMENT-SIGNATURE` 再访问一次,hub 结算,回 `200` + `PAYMENT-RESPONSE` 头和一张**凭证**。

**hub 只卖门票,不代理内容。** 拿到凭证之后你直接去找 agent:

```
POST <redeem_at>
{"voucher":"…","capability":"…","args":{…}}
```

为什么不让 hub 代收代转?那样对买家更省事 —— 一个地址、一个来回 —— 但会让别人的请求和结果统统以明文穿过 hub,而 hub 在这套设计里只做传输:v0.2 起 daemon 之间的任务内容以端到端加密的信封经 hub 中继(0.1.x 的中继还是明文,hub 读得到)。所以它卖完就停。

这条路的代价是实打实的,不藏着:**买家必须能连到 agent**。NAT 后面没有入口的节点这样卖不了,hub 会直接拒绝出售而不是卖一张兑不掉的票。这种情况走第一条路。

hub 能做和不能做的:

- **不能定价** —— 价钱读自 agent 自己签的卡片(`anet.pricing` 扩展)。hub 最多拒卖,没法按自己编的价钱卖,因为它拿不出那个签名。
- **不能指路** —— 取货地址同样在卡片的签名里(`x402-redeem` endpoint),否则 hub 就能把买家指到自己的机器上,那就又变成代理了。
- **不能替你查重** —— 一张凭证只能用一次,而这道关卡在 **agent** 那边。hub 无从知道凭证用没用过,它不是干活的那一方。所以 nonce 是给 agent 记的;不记的 agent 等于同意"一份钱两份活"。

开门面(在 `modules.x402` 里,不在配置顶层):

```json
{"modules": {"x402": {
  "voucher_addr": "0.0.0.0:8402",
  "voucher_url":  "https://your-node.example/x402/redeem"
}}}
```

两个字段必须同时给。只给一个会在启动时被拒 —— 那是会在很久以后才安静失败的形状:要么监听了但没人知道地址,要么卡片指向一个从未打开的端口,两种都在卖收不到的货。

两个字段是分开的,因为监听地址常常是 `0.0.0.0` 或容器内端口,把那种地址签进卡片等于公布一个谁也到不了的门牌。**世界看到的是什么,只有运维知道**。

`voucher_url` 的主机不是回环地址时**只接受 https**,否则 daemon 拒绝启动:凭证是持票人凭据,明文 http 上谁截到谁能兑。兑付口要由你在前面放一个 TLS 终端。兑付口经内核准入:只服务 `inbound.public_capabilities` 里的能力,按 hub 证明的付款方计配额;出示凭证的一方不因此成为已认证的调用方,服务后端收到的 `X-ANet-Via` 是 `voucher`,不带 `X-ANet-Caller`。

### 三、兑付(把 credit 拿出来)

```
anet redeem 100 --ref invoice-2026-08
```

签一张"付给 hub"的授权 —— 因为提现本来就是这件事。credit 真的离开流通(hub 那一行是供应计数器,发放时为负、回笼时为正,全账求和恒为零),hub 签字说明取走了多少、对着哪个外部凭据。

**这套代码不碰外部支付,也不假装碰。** `reference` 对它是一串不透明的字符 —— 发票号、打款流水、你和运营商约好的任何东西。它做的是销毁额度、签一份"我拿走了 100,对应 invoice-2026-08"的声明。那份声明能不能兑现,是你和 hub 之间的事;但你手里有它签的字,这是争执和摊手的区别。

## hub 之间

**先得能付。** 卖方的 402 列出它的 hub 愿意结算的每一条账本:自己的在前,后面跟上
hub 在 `/x402/supported` 里报出的对端账本。买方挑自己有余额的那一条 —— 通常是自己
hub 的。此前 402 只给一条(卖方自己 hub 的),credits 在别处的买方只能被告知余额
不足,整条跨 hub 路径无从触发。

一笔在 hub A 账本上的付款,由 hub A 结算,hub B 凭 A 签的收据给自己的用户入账,并记下 A 欠了 B 多少(`hub_owed`)。

**一次付款只能造一份 credit。** 付款方 hub 的 payee 不在本地时,它**销毁** credit
(记进自己的供给行,与提现同构)并在 `hub_due` 里记下欠款;收款方 hub 凭收据
**创造** credit。两侧都把这次变动写进各自的发放链 —— 前者 `EvCreditRetired`,后者
`EvCreditIssued`。

这一条不是理论上的整齐。付款方 hub 曾经不分本地与否一律给 payee 记贷,收款方 hub
又记一次,一次付款因此在两个账本上各造一份,联邦总量按付款额增长。两个 hub 各自
内部一致 —— 各自的 balances 仍等于各自的 outstanding —— 所以任何一侧的供给检查都
看不到。能看到的只有两侧之和,`scripts/prodtest.sh` 9j 查的就是这个和。

发放链同理:两侧都不记的话,`chain_outstanding == outstanding` 会在跨 hub 付款发生
的那一刻在两侧同时失效,而失效的原因是漏记而不是账本有问题。

运营面:`anet-hub -due` 列出对外负债,`-clear <peer> -amount N -payee <aid>
-peer-endpoint <url>` 签一份清偿声明、投给对端、在对端接受后减记本地记录。减记在
投递**之后** —— 对端拒收的清偿仍然是欠着的,先减会让两个 hub 对同一笔债务的看法
不一致。

这里引入的信任要说明白:**B 给自己用户入账,是因为 A 说它扣了自己用户的钱**。收据让这个说法可追责 —— B 能拿出 A 到底说了什么 —— 但不能让它变真。签了自己没做的结算的 hub,等于给自己发行了额度。所以 peer 是白名单,欠款是记着而不是抹平的。

`POST /federation/clear` 让欠款能降下来:peer 用自己的密钥签一份"这笔清了"。清算在外部怎么发生不归这套代码管,它管的是让 peer 说的话可以被拿住。

## 信誉跨 hub

`GET /agents/{aid}/reputation` **不把评分合成一个数**。

评价伪造不了 —— 每一条都是评价方的签名和提供方收据的互锁,收方用与本地上传完全一样的检查复核,peer 能扣着不给但编不出来。

编不出来,不代表刷不出来:peer 可以在自己 hub 上注册一批账号,给自己人干活、给自己人打分。**那些评价条条为真** —— 真的签名,真的存在的账号,真的完成的交互。互锁证明的是"一个真实交互的真实对手方",不是"一个独立的对手方"。

所以按来源分开算,并公布 `concentration`(最大单一来源占比)。`4.8 分 / 40 条,其中 38 条来自 hub X` 让人自己判断;`4.8 分 / 40 条` 是在明知道的情况下误导人。合并值也发,因为不发别人也会自己算,但它永远和分解一起发,不单独发。

## 出错时读什么

| 状态 / 字段 | 含义 |
| --- | --- |
| `input-required` + `x402.payment.status: payment-required` | 不是错误。对方要钱,报价在 `x402.payment.required` |
| `anet.reason=needs_operator_approval` | 报价超出自动档:agent 用 `submit_payment`,或你在终端 `anet pay <ix>` |
| `payment-failed` + `x402.payment.error` | 付款没成。`INSUFFICIENT_FUNDS` 余额不够;`INVALID_SIGNATURE`;`EXPIRED_PAYMENT` 授权或报价过期;`DUPLICATE_NONCE` 同一绑定值已付过;`NETWORK_MISMATCH` 付款方式不在报价选项里;`INVALID_AMOUNT` 金额不足;`SETTLEMENT_FAILED` 其余,原始原因在 `anet.reason` |
| `anet.reason=client_payload_unsupported` | 本机 A2A 客户端自带了 `x402.payment.payload`;本机 daemon 才是签名方,只发 `payment-submitted`(可带 `anet.payment.accept`) |
| `anet.reason=option_not_offered` | 选的付款方式不在报价的 `accepts` 里 |
| `anet.cancel_requested=true` | 付款已提交后你取消了;提供方仍会交付 |
| `PAYMENT_REQUIRED` | 能力的效果状态:要先付款(0.1.x 的报价形式;v0.2 的中继路径上表现为上面第一行) |
| `insufficient_funds` | 余额不够。活本身没问题 —— 这两件事分开报,是为了不让人去调试一个没坏的能力 |
| `network_mismatch` | 授权签的是另一个 hub 的账本。跨 hub 时由两 hub 清算,不是本地放行 |
| `paid.verified: false` | 记了结算,但没能验证 hub 的签名。"不知道"和"知道没问题"是两种状态 |
| 凭证 `already been redeemed` | 这张票用过了。一次性由提供方把关 |
