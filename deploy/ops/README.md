# 运营节点:dmax-services 与 cmax-anet4(0.2.0)

`scripts/prodtest.sh` 检查的两个常驻节点。0.1.x 时它们以 root 运行、后端在回环 TCP 口上;
2026-09-29 按 0.2.0 全新部署(新身份、旧数据不保留,记录见 `docs/notes/0034` §G7.3)。
部署到生产机器属于生产变更,执行前须经产品负责人同意。本目录不含任何密钥。

| 节点 | 主机 | hub | 单元(daemon / 后端) | 账户 | 数据目录 | 公开能力 |
|---|---|---|---|---|---|---|
| `dmax-services` | dmax(gpu2) | `https://hub2.agentnetwork.org.cn`,可见性 `federated` | `anet-dmax.service` / `anet-dmax-svc.service` | `anet-dmax` | `/var/lib/anet-dmax/.anet` | `text.stats`、`text.stats.paid`(30 credit);`image.inspect` 只对名单内对端 |
| `cmax-anet4` | cmax | `https://hub.agentnetwork.org.cn` | `anet4.service` / `anet4-svc.service` | `anet-cmax` | `/var/lib/anet-cmax/.anet` | `text.digest`、`text.digest.paid`(25 credit);自动回复(openai 后端) |

两台的控制口都是 `127.0.0.1:29610`(prodtest 按此连接)。

## 文件

- `anet-dmax.service`、`anet4.service`:daemon,按 `deploy/official/anet-official-daemon@.service` 加固——专用
  非 root 账户、`StateDirectory` 0700、自己的 `RuntimeDirectory`(`XDG_RUNTIME_DIR=/run/anet-<节点>`,不写 root 其他
  daemon 共用的 `/tmp/anet-0`)、`ProtectSystem=strict` 只写数据目录、空能力集、系统调用过滤。
- `anet-dmax-svc.service`、`anet4-svc.service`:后端,以节点自己的账户运行,只监听
  `/run/anet-dmax-svc/backend.sock`、`/run/anet4-svc/backend.sock`(目录 0700、socket 0600),`PrivateNetwork=yes`。
  0.2.0 的 service 模块默认只接 Unix socket,连接前核对 socket 路径与监听者 uid(`internal/backendconn`)。
- `dmax-svc.py`、`cmax-svc.py`、`uds_http.py`:后端程序(与 0.1.x 的 TCP 版计算相同),装到 `/opt/anet/ops/`。
- `dmax-services.config.json`、`cmax-anet4.config.json`:`anet init` 的安全默认 + 节点角色。cmax 的 `auto_reply`
  含模型 API key,不在仓库里,部署时从原节点配置搬过来。
- `anet-dmax`、`anet-cmax`:CLI 包装,装到 `/opt/anet/bin/`。CLI 只把控制令牌发给同 uid 的 daemon
  (`internal/localpeer`),root 直接运行 `anet` 驱动不了这两个节点,须经包装:`anet-dmax status`。

没有部署的:p2p(`anetpeer` 不在签名发布里)、ANetLink(`anetlinkd` 的 C1 socket 属 root;dmax 上的
`anetlinkd`/`anetmock`/`adapdemo` 仍按原样运行,未接入新节点)、公开兑付口(`voucher_url` 须 https,dmax 前面没有
TLS 终端)。prodtest 相应各节会跳过或失败,见 `docs/notes/0034` §G7.3。

## 部署(每台主机,root)

```sh
# 1) 二进制:签名发布,走手动核验路径;allowed_signers 那一行见 README.md / SECURITY.md
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh
curl --proto '=https' --tlsv1.2 -fsSLO https://agentnetwork.org.cn/install.sh.sig
ssh-keygen -Y verify -f allowed_signers -I anet-release@agentnetwork.org.cn \
  -n anet-release@agentnetwork.org.cn -s install.sh.sig < install.sh
# 安装脚本会在 ANET_DATA_DIR 里跑 anet init:指向一个临时目录,别让它碰 root 的 ~/.anet
HOME=$tmp ANET_DATA_DIR=$tmp/.anet XDG_RUNTIME_DIR=$tmp sh install.sh --prefix /opt/anet/bin
# 2) 账户、后端、包装、单元(dmax 为例;cmax 把 dmax/anet-dmax 换成 cmax/anet-cmax/anet4)
useradd --system --user-group --home-dir /var/lib/anet-dmax --no-create-home --shell /usr/sbin/nologin anet-dmax
install -d -m 0755 /opt/anet/ops && install -m 0644 uds_http.py dmax-svc.py /opt/anet/ops/
install -m 0755 anet-dmax /opt/anet/bin/
install -m 0644 anet-dmax.service anet-dmax-svc.service /etc/systemd/system/ && systemctl daemon-reload
# 3) 数据目录:init 的安全默认,再换成角色配置(init 复查只报公开能力一项与默认不同)
install -d -o anet-dmax -g anet-dmax -m 0700 /var/lib/anet-dmax
anet-dmax init && install -o anet-dmax -g anet-dmax -m 0600 dmax-services.config.json /var/lib/anet-dmax/.anet/config.json
anet-dmax init
# 4) 启动、注册
systemctl enable --now anet-dmax-svc anet-dmax
anet-dmax hub-register https://hub2.agentnetwork.org.cn --name dmax-services
anet-dmax visibility federated           # cmax 不需要
anet-dmax doctor                         # 版本签名 verified、backend.transport 通过、无 ✗
```

升级:以 root 运行 `/opt/anet/bin/anet update`(二进制属 root;`update` 只换二进制与旁边的
`anet.release.json(.sig)`,不碰数据目录),然后 `systemctl restart anet-dmax`。

## 另外三个常驻节点:CMax Face、CMax Vision、gpu2(0.2.1)

原为 0.1.x、以 root 运行、连 wire-2 hub 只得 426 的节点;2026-09-29 按 0.2.1 全新部署(新身份、旧数据删除,
记录见 `docs/notes/0040`)。prodtest 不检查它们。二进制与上面两个节点共用 `/opt/anet/bin/anet`(签名发布,
`anet update` 升级后逐个 `systemctl restart`)。

| 节点 | 主机 | hub | 单元 | 账户(uid) | 数据目录 | 控制口 | 谁能交任务 |
|---|---|---|---|---|---|---|---|
| `CMax Face · 轻量人脸识别` | cmax | `https://hub.agentnetwork.org.cn` | `anet-face-daemon.service` | `anet-face`(990) | `/var/lib/anet-face/.anet` | 127.0.0.1:39813 | 任何人(`inbound.policy=open`),自动回复(openai 后端 → `anet-face-api` 127.0.0.1:8093) |
| `CMax Vision · 轻量视觉小模型` | cmax | 同上 | `anet-vision-daemon.service` | `anet-vision`(989) | `/var/lib/anet-vision/.anet` | 127.0.0.1:39812 | 任何人(`open`),自动回复(openai 后端 → ollama 127.0.0.1:11434,`qwen3-vl:4b`) |
| `gpu2` | dmax | 同上 | `anet-gpu2.service` | `anet-gpu2`(990) | `/var/lib/anet-gpu2/.anet` | 127.0.0.1:39811 | 无人(`closed`,名单空);无能力 |

- 单元与 `anet4.service` 同一套加固;face/vision 的单元名沿用原名。自动回复的 openai 后端只向本机 HTTP API 发请求,
  不在本机运行程序,所以 `open` 不打开任何执行面;公开能力(`public_capabilities`)为空——两者原本就不是能力调用,
  是带图片的对话。改回只服务名单内对端:`anet-face inbound policy closed`(终端确认)或改配置后重启。
- 配置:`cmax-face.config.json`、`cmax-vision.config.json`、`dmax-gpu2.config.json`(`anet init` 安全默认 + 原节点的
  名称、caps、自述与 `auto_reply` 块;均不含密钥)。hub 地址由 `hub-register` 写入。
- **账户 uid 要避开容器。** 这两台主机上的容器与宿主共用 uid 空间,容器镜像里的系统账户占着 993–999 一带的 uid/gid
  (`useradd --system` 会从 999 往下挑,只看宿主的 `/etc/passwd`)。anet 的若干检查按 uid 认人(CLI 只把控制令牌发给
  同 uid 的 daemon、后端监听者核对),所以新账户取宿主与各容器镜像都没用的号:先列出
  `docker inspect -f '{{.GraphDriver.Data.MergedDir}}'` 下各镜像 `/etc/passwd`、`/etc/group` 的系统号与 `ps -eo uid=`,
  再 `useradd --system --uid N --user-group …`(或事后 `groupmod -g N`、`usermod -u N -g N` 并 `chown -R` 数据目录)。

```sh
# cmax(dmax 的 gpu2 同理:anet-gpu2 / anet-gpu2.service / dmax-gpu2.config.json)
groupadd --system -g 990 anet-face && useradd --system -u 990 -g 990 --home-dir /var/lib/anet-face \
  --no-create-home --shell /usr/sbin/nologin anet-face
install -m 0755 anet-face /opt/anet/bin/ && install -m 0644 anet-face-daemon.service /etc/systemd/system/
systemctl daemon-reload
install -d -o anet-face -g anet-face -m 0700 /var/lib/anet-face
anet-face init && install -o anet-face -g anet-face -m 0600 cmax-face.config.json /var/lib/anet-face/.anet/config.json
anet-face init                      # 只报 inbound.policy "open" 一项与默认不同
systemctl enable --now anet-face-daemon
anet-face hub-register https://hub.agentnetwork.org.cn --name "CMax Face · 轻量人脸识别" \
  --caps face-recognition,face-detection,face-verification
anet-face doctor                    # version.signature verified;inbound 一行为 "!"(open),无 ✗
```

0.2 的目录里,入站策略为 `open` 的节点在 A2A 卡片上只有一个 `chat` 技能,hub 的 `/agents` 列表显示 `caps: ["chat"]`;
按原来的 caps 标签(如 `vision`)`?cap=` 查不到,按名称或自述的关键词 `?q=vision`、`?q=人脸` 查得到。
