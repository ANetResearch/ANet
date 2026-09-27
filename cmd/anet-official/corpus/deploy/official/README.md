# 官方公共 agent:部署样例

本目录是 anet 官方公共 agent(A2A-DESIGN §15)的部署样例:五个身份的 daemon 配置、
后端实例参数、两个 systemd 单元模板。后端程序是 `cmd/anet-official`。

部署到生产机器属于生产变更,执行前须经产品负责人同意(A2A-DESIGN §2"对外提交与发布")。
本目录只是样例,不含任何密钥;令牌与身份在目标机器上生成,不进仓库。

## 1. 五个身份

| 代号 | 身份 | 归属 hub | 后端组 | 能力 | 后端端口 | 控制口 | 构建档 |
|---|---|---|---|---|---|---|---|
| A1 | `anet-echo-e` | emax `https://hub.agentnetwork.org.cn` | `echo` | `net.echo` | 8611 | 39821 | `anet-standard` |
| A2 | `anet-echo-f` | fmax `http://39.107.76.243:4001` | `echo` | `net.echo` | 8612 | 39822 | `anet-standard` |
| B | `anet-tools` | emax | `tools` | `text.stats` `text.digest` `text.diff` `json.validate` `a2a.card.validate` `a2a.x402.check` | 8613 | 39823 | `anet-standard` |
| C | `anet-docs` | emax | `docs` | `docs.search` `docs.get` | 8614 | 39824 | `anet-standard` |
| E | `anet-paid-demo` | emax | `paid` | `demo.digest.paid`(标价 2 credit) | 8615 | 39825 | `anet-paid` |

每个身份独立:独立 AID、独立数据目录、独立卡片与证据链、独立限流预算、独立后端实例
与令牌。付费演示单独一个身份,是因为 a2a-x402 建议付费 agent 在卡片里把扩展声明为
`required: true`,与免费能力放在一起会让不支持 x402 的客户端连免费能力也用不了。

每个身份的目录里有:

- `config.json`:daemon 配置(放到 `/var/lib/anet-official/<身份>/config.json`)。
  `inbound.policy=closed`,只有 `public_capabilities` 里的能力对陌生人开放;
  `modules.service` 把这些能力挂到本机后端。
- `backend.env`:后端实例参数 `LISTEN`、`CAP_GROUPS`(放到 `/etc/anet-official/<身份>/backend.env`)。

`public_capabilities` 与 `modules.service` 两段由程序生成,不要手改:

```sh
anet-official service-config -groups tools -url http://127.0.0.1:8613
```

能力表(名称、描述、标签、示例、参数上限、超时、建议配额、建议价格)只在
`cmd/anet-official/caps.go` 一处定义;`cmd/anet-official` 的测试逐项比对本目录的样例与
生成结果,改了能力表而没重新生成样例,测试会失败。

## 2. 谁能看到什么

内容只出现在请求方 daemon、官方 agent 的 daemon、以及它 127.0.0.1 上的后端之间。

| 层 | 看得到 | 看不到 |
|---|---|---|
| hub | 发送方、收件方、时间、密文大小(A2A-DESIGN §21 第 1 条) | 能力参数与结果(端到端加密信封) |
| 官方 agent 的 daemon | 调用方 AID、能力 id、参数、结果 | — |
| 后端 `anet-official` | 同上(daemon 转交) | 不保存,见 §5 |

五层防护(docs/notes/0010 §5.4):

1. **准入**(daemon 内核):`closed` 策略,非公开能力与自然语言委派一律拒绝,不写交互、不存内容。
2. **配额**(daemon 内核,`public_capabilities` 每项):按调用方每分钟/每天、全局每分钟、
   并发、参数字节数。配额为 0 或缺省时取内核默认值(每调用方 60/分、2000/天,
   全局 1200/分,并发 16,参数 4096 字节);付费能力只写了全局与并发,按调用方的
   两项因此取默认值。超限回 `rejected` 并带 `anet.retry_after_ms`。
3. **后端自身**:只监听回环地址、只应答回环 Host、只认本身份 daemon 的令牌;
   每个能力有参数上限(与 `max_args_bytes` 相同)与超时;进程级并发上限 64;
   只做确定性纯计算,不执行命令、不访问网络、不读文件、不接受 URL。Go 无法中途终止一个
   计算,所以超时靠计算自己的预算兑现:模式匹配、枚举比较、大数运算、签名验证都按工作量
   计入预算并定期看截止时间,一次调用不会在 daemon 放弃之后继续占着 CPU。
4. **hub**:只做与内容无关的流量计量(认证发送后按发送方限流)。
5. **处置**:官方 agent 本地按证据链与后端调用日志统计,把滥用的 AID 写进该身份的
   `peers.deny`(每次判定重读,无需重启)。

## 3. 部署步骤

以下命令在目标机器上以 root 执行(`<id>` 为身份名)。

```sh
# 1) 二进制
install -m 0755 anet-standard anet-paid anet-official /usr/local/bin/

# 2) 专用账户(daemon 用;后端用 systemd 的 DynamicUser,不需要账户)
useradd --system --home-dir /var/lib/anet-official --shell /usr/sbin/nologin anet-official

# 3) 每个身份:令牌、后端参数、daemon 配置
for id in anet-echo-e anet-echo-f anet-tools anet-docs anet-paid-demo; do
  install -d -m 0700 /etc/anet-official/$id
  (umask 077; head -c 32 /dev/urandom | base64 | tr -d '\n=' | tr '+/' '-_' > /etc/anet-official/$id/token)
  install -m 0644 deploy/official/$id/backend.env /etc/anet-official/$id/backend.env
  install -d -o anet-official -g anet-official -m 0700 /var/lib/anet-official/$id
  install -o anet-official -g anet-official -m 0600 deploy/official/$id/config.json /var/lib/anet-official/$id/config.json
done

# 4) 单元
install -m 0644 deploy/official/anet-official-backend@.service deploy/official/anet-official-daemon@.service /etc/systemd/system/
mkdir -p /etc/systemd/system/anet-official-daemon@anet-paid-demo.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/anet-paid daemon\n' \
  > /etc/systemd/system/anet-official-daemon@anet-paid-demo.service.d/paid.conf
systemctl daemon-reload
for id in anet-echo-e anet-echo-f anet-tools anet-docs anet-paid-demo; do
  systemctl enable --now anet-official-backend@$id anet-official-daemon@$id
done

# 5) 注册到各自的 hub(A2 注册到 fmax)
sudo -u anet-official env ANET_DATA_DIR=/var/lib/anet-official/anet-tools \
  anet-standard hub-register https://hub.agentnetwork.org.cn --name anet-tools
```

`config.json` 里的 `hub_url` 与第 5 步的 hub 要一致。能力清单在 `hub-register` 那一刻
折进注册(GUIDE §6.2),改了能力表要重新注册。

## 4. 令牌

后端以每后端令牌认证 daemon(A2A-DESIGN §6、§15)。本机回环端口对本机所有进程可达,
包括自动回复沙箱里的本地 agent;没有令牌,后端分不清 daemon 与其他进程,
`X-ANet-Caller` 头也就谁都能写。

- 令牌文件 `/etc/anet-official/<id>/token`,root 所有、0600。两个单元都用
  `LoadCredential=token:…` 取得它:systemd 把它复制到只有该单元可读的凭据目录。
  daemon 配置里写 `"token_file": "${CREDENTIALS_DIRECTORY}/token"`(service 模块展开
  环境变量);后端命令行写 `-token-file %d/token`。
- 两端都要求:路径中的环境变量已设置、绝对路径、普通文件、不可被其他用户读取、
  至少 16 字节、不含空白。
- service 模块只把令牌发往回环地址或 https 地址;请求不跟随重定向。
- 后端对令牌做常数时间比较(两侧先取 SHA-256 再比),错误回 401,不说明原因;
  令牌检查在路由之前,没有令牌的请求无论路径一律 401,探不出本实例开了哪些能力。
- 轮换:写新令牌,依次重启该身份的后端与 daemon。

daemon 发给后端的请求头(`module/service`):

| 头 | 值 |
|---|---|
| `Authorization` | `Bearer <令牌>` |
| `X-ANet-Caller` | 已验证的调用方 AID;只有经中继、发送方签名已验证的调用才有。凭证兑付口(`Via=voucher`)不带:那里的 AID 只说明谁付了钱,不说明谁在调用 |
| `X-ANet-Call` | 交互 id(兑付口为凭证 id) |
| `X-ANet-Via` | `relay` 或 `voucher` |
| `X-ANet-Capability` | 能力 id |

## 5. 内容保存策略(A2A-DESIGN §21 第 4 条)

官方公共 agent 是端点,不是传输层:它必须读到调用内容才能计算结果。我们公开写明它
保存什么。

**后端 `anet-official`**:不保存参数与结果。每次调用在 journald 写一行:时间、能力 id、
HTTP 状态、调用方 AID、交互 id、入口(`relay`/`voucher`)、入出字节数、耗时。
不写参数与结果的任何部分。这行日志随 journald 的轮转策略保留。

**daemon(官方身份的数据目录)**,现状:

- 交互库保存请求的签名 TaskDoc(含能力参数)与结果交付物,目前没有自动清理。
- 证据链的 `anet.capability.effect` 事件记录 provenance,其中 `observed_state` 就是后端的
  完整回复;证据链只追加。

**目标策略**(依赖 daemon 内核的两项尚未实现的配置,见 §7):

1. 公共能力的证据事件只记 `result_cid` 与指标(`metrics`、状态、调用方、能力 id),
   不记 `observed_state`。`result_cid` 足以让持有结果的一方证明结果与收据相符。
2. `public_cap` 交互的参数与结果按天数清理(建议 7 天),收据与 `result_cid` 保留。

在这两项实现之前,运营者定期清理官方身份的交互库,并如实对外说明现状。

**hub**:只见密文信封与路由元数据(§2)。hub admin 对官方 agent 只登记
`id/aid/hub/caps`,不采集运行数据(A2A-DESIGN §2 "hub admin 采集")。

## 6. 公共能力的证据模式

C5 证据面对公共能力有两种模式,由各节点配置(契约文档 C5 写明两种模式):

| 模式 | `anet.capability.effect` 事件内容 | 用途 |
|---|---|---|
| 完整(默认) | 调用方、能力、状态、指标、`result_cid`、provenance(含 `observed_state`) | 私有能力;事后可从证据链复原结果 |
| 只记 CID | 调用方、能力、状态、指标、`result_cid` | 公共能力;证据链不再是调用内容的第二份拷贝 |

官方公共 agent 用"只记 CID"。结果本身已经随签名收据交给调用方;调用方要证明结果,
出示结果与收据,任何人重算 CID 即可核对,不需要官方 agent 的证据链里有原文。

## 7. 尚未就绪的依赖

- daemon 内核:公共能力证据"只记 CID"模式的配置项与实现(§6),`public_cap` 交互的
  保存期限清理(§5)。样例配置里没有写这两个键:它们还不存在,写了也不会生效。
- 官方清单:发布签名密钥签署的官方 AID 清单,随二进制打包,`list_agents` 与代理卡片据此
  标注 `anet.official: true`(A2A-DESIGN §15)。
- 卡片:daemon 生成 A2A 网络卡片时经 `provider.Described` 读取 service 模块配置的
  `name/description/tags/examples/input_modes/output_modes`(A2A-DESIGN §10.2)。
- 构建:`no_a2a` 落地后,官方身份的构建档加上它(官方 agent 不需要本机 A2A 接口)。

## 8. 运维

- 版本与语料:`anet-official version` 打印版本与语料 CID;`GET /healthz`(无需令牌,
  只回版本与语料 CID)。
- 语料:`cmd/anet-official/refresh-corpus.sh` 把公开文档复制进 `corpus/` 并随二进制
  编入;发布前 `refresh-corpus.sh --check` 确认语料与文档一致。语料 CID 是清单
  (每行 `<cid> <路径>`,按路径排序)的 CID,每篇文档的 CID 是其字节的 CIDv1 raw
  sha2-256,任何人可复算。
- 监控:prodtest 的只读段调用 A1、A2、B 的一个确定性样例、C 的固定查询(比对
  `corpus_cid` 与首条结果);E 只在写模式段。
- 负责人:官方清单写明维护者与联系方式(docs/notes/0010 §3.5:没有负责人的官方
  agent 会以不可用状态长期挂在目录里)。
