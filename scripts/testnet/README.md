# 联调测试网(scripts/testnet)

把 anet 的 daemon、hub、anetpeer、official 部署到四台授权测试主机上,做跨主机、跨 hub 的联调。
主机勘察见 [`docs/notes/0015-测试网主机勘察.md`](../../docs/notes/0015-测试网主机勘察.md);本文件讲怎么用、
保证什么、以及现有联调脚本要怎么改才能指向它。

| 文件 | 作用 |
|---|---|
| `topology.env` | 拓扑:主机表、节点表(角色/主机/端口/所属 hub/p2p 端口/二进制)、联邦对、端口段。其余脚本都读它 |
| `build.sh` | 本机交叉编译 `anet`(及 `anet-<变体>`)、`anet-hub`、`anet-hub-admin`、`anetpeer`、`anetfixture`、`anet-official`(存在时),出 `SHA256SUMS` 与 `MANIFEST` |
| `deploy.sh` | `deploy.sh <host> <role>` 部署一个角色;另有 `federate`、`everything`、`status`、`restart`、`stop`、`plan` |
| `teardown.sh` | `teardown.sh <host>` 停掉 `anet-testnet-*` 单元与测试网进程,删除测试网目录(只删它) |
| `bridge.sh` | 编排机(ink88)上的本地 ssh 转发:远端控制口原号映射到本机、跨岛联邦转发、把节点配置与令牌镜像到本机 |
| `common.sh` / `remote.sh` | 本机侧公共函数 / 经 ssh stdin 送到远端执行的函数库(远端不落这个文件) |

## 拓扑

```
            lab 岛 10.2.2.0/24(任意端口互通)                         campus 岛 210.45.70.0/23 + zerotier 10.253.20.0/24
  ┌──────────────── Ink89 ────────────────┐                     ┌───────────── dmax (gpu2) ─────────────┐
  │ hub1 :47101   d1a :47111 (p2p :47131) │◄──── 联邦 ────┐       │ hub3 10.253.20.5:47301                │
  │               d1b :47112 (p2p :47132) │              │       │ d3a :47311 (p2p :47331,可入站)         │
  └───────────────────────────────────────┘              │       └───────────────▲──────────────────────┘
  ┌──────────────── Ink90 ────────────────┐              │                       │ zerotier
  │ hub2 :47201   d2a :47211 (p2p :47231) │◄─────────────┘       ┌───────────── cmax ───────────────────┐
  │               d2b :47212              │                      │ d4a :47411 (p2p :47431,ufw 挡入站)    │
  │               off2 :47241 (+后端 :47242)│                      │ d4b :47412          → hub3            │
  └───────────────────────────────────────┘                      └──────────────────────────────────────┘
                     ▲  局域网                                              ▲ zerotier(10.253.20.2)
                     └──────────────── ink88(编排机,本机)───────────────────┘
                              跨岛联邦只能经它转发:bridge.sh fed-up + FEDERATE_BRIDGED=1
```

- **lab 岛**里 hub1↔hub2 是真跨主机联邦,daemon 在两个 hub 上,p2p 走局域网 TCP。
- **campus 岛**里 hub3 在 dmax;cmax 的 daemon 经 zerotier 注册到 hub3。cmax 的 ufw(INPUT DROP,47xxx 未放行)
  挡住所有入站 —— 这是**有意保留**的真实"对端入站不可达"路径:dmax→cmax 的 p2p 应失败并回落到 hub 中继。
  **不要为此去改 cmax 的 ufw**(那是改生产主机配置,需另行征得同意)。
- **两岛之间不通**:Ink89/90 连不到 cmax/dmax 的任何地址,反之亦然。跨岛联邦只能经 ink88 转发,见下文。
- 端口段 47100–47499,每台主机一个百位段,整个测试网内唯一。勘察时四台主机与 ink88 的这些 TCP 端口全部空闲。
- 想测"非覆盖网地址"路径:把 `topology.env` 里 dmax 的拨号/绑定地址改成 `210.45.70.176`。注意 cmax 与 dmax
  同在 210.45.70.0/23,这条路径不经过 NAT;授权主机里**没有**任何一对能走真实的公网 NAT 穿越。

## 用法

```bash
cd ANet                                   # 本仓根目录
bash scripts/testnet/build.sh             # 按 topology.env 的架构交叉编译(可加 -t / --hub-tags / --variant)
bash scripts/testnet/deploy.sh plan       # 只打印拓扑,不连主机

# 逐个角色部署(顺序:hub 先于挂在它上面的 daemon)
bash scripts/testnet/deploy.sh ink89 hub1
bash scripts/testnet/deploy.sh ink90 hub2
bash scripts/testnet/deploy.sh dmax  hub3
bash scripts/testnet/deploy.sh federate   # hub1↔hub2 互写 federation.json 并重启
bash scripts/testnet/deploy.sh ink89 daemon
bash scripts/testnet/deploy.sh ink90 daemon
bash scripts/testnet/deploy.sh dmax  daemon
bash scripts/testnet/deploy.sh cmax  daemon
bash scripts/testnet/deploy.sh ink90 official     # 需要 ANet/cmd/anet-official 已存在并已构建
# 或一次全部:bash scripts/testnet/deploy.sh everything

bash scripts/testnet/deploy.sh status     # 各主机上跑着什么
bash scripts/testnet/bridge.sh ctl-up     # 远端控制口映射到本机同号端口
bash scripts/testnet/bridge.sh mirror     # 节点 config/令牌镜像到 $TESTNET_STATE/nodes/,并写 nodes.env

bash scripts/testnet/deploy.sh stop d2a   # 让一个对端离线;restart d2a 拉起
bash scripts/testnet/bridge.sh down
bash scripts/testnet/teardown.sh all      # 或 teardown.sh <host> [--run tn1] [--dry-run]
```

`build.sh` 选项:`-t TAGS`(daemon 与 official 的 tag)、`--hub-tags TAGS`、`--variant NAME=TAGS`(可重复,出
`anet-NAME`,在节点表"二进制"列里引用)、`--arch ARCH`、`-o DIR`。它用工作树旁的 `env.sh`(否则
`/data/projs/anet-dev/.anet-env.sh`)设 GOWORK,并核对 GOWORK 里 use 的 ANet 就是本仓——在工作树里用主检出的
`go.work` 会静悄悄地编进另一份源码。hub 的网页用仓里已提交的嵌入副本,不重建 webui(MANIFEST 里写明)。

`deploy.sh` 的环境变量:`TESTNET_RUN_ID`(默认 tn1)、`REGISTER=1`、`HUB_ADMIN=0`、`REWRITE_CONFIG=0`、
`FEDERATE_BRIDGED=0`、`TESTNET_MODE=auto`。重部署同一个 run-id 保留身份与 `config.json`(测试可能往里写过东西),
只传 sha256 变了的二进制。

### 两种运行模式

| | 系统模式 | 用户模式 |
|---|---|---|
| 何时 | ssh 用户是 root 且有 systemd-run(cmax、dmax) | 其余(Ink89、Ink90:root 不接受本机密钥,ink 的 sudo 要密码) |
| 目录 | `/opt/anet-testnet/<run-id>/` | `~/anet-testnet/<run-id>/` |
| 进程 | systemd **瞬态**单元 `anet-testnet-<run-id>-<节点>[-peer\|-admin\|-backend]`,不在 `/etc/systemd` 写任何文件 | `setsid` 起,pid 记在节点目录;无 linger,不用 `systemctl --user` |
| 重启后 | 不自动恢复(瞬态单元),重跑 deploy | 同左 |

若产品负责人把本机公钥加进 Ink89/90 的 root,把 `topology.env` 的 ssh 目标改成 `root@…`,自动切到系统模式。

每个节点目录下有 `run-<名>.sh`(启动器,两种模式执行的是同一条命令)、`<名>.log`、`home/`(`HOME` 与
`ANET_DATA_DIR=home/.anet`)、`xdg/`(私有 `XDG_RUNTIME_DIR`)。hub 的数据在 `data/`,`federation.json` 也在那里。

## 安全边界:不触碰现有服务的保证

cmax 与 dmax 上跑着生产与准生产服务(见下节)。脚本的保证,以及每条保证靠什么成立:

1. **只写测试网目录。** 远端所有写操作都在 `/opt/anet-testnet/<run>/`(或 `~/anet-testnet/<run>/`)之下。系统模式下
   每个进程还在 systemd 沙箱里:`ProtectSystem=strict` + `ReadWritePaths=<run 目录>` + `ProtectHome=yes` +
   `PrivateTmp=yes` + `NoNewPrivileges=yes` —— 即便二进制有缺陷,内核也不让它写别处。
2. **不碰生产 daemon 的发现文件。** anet 会把"当前 daemon"指针与多 daemon 注册表写进 `$XDG_RUNTIME_DIR/anet` 或
   `/tmp/anet-<uid>`;cmax、dmax 上 root 的生产 daemon 正在用 `/tmp/anet-0`(dmax 还有 `/run/user/0/anet`)。
   测试网进程的 `XDG_RUNTIME_DIR` 指向节点自己的 `xdg/`,系统模式另有 `PrivateTmp` 与 `ProtectHome`(遮住 `/run/user`)。
   否则一个 root 的测试 daemon 会覆盖生产指针,之后在那台机器上不带 `ANET_DATA_DIR` 的 `anet` 命令会连到测试节点。
3. **环境从零重建。** 启动器 `unset` 掉 `ANET_HOME`/`ANET_ID`/各种 proxy 变量,显式设 `HOME`、`ANET_DATA_DIR`、
   `XDG_RUNTIME_DIR`;用户模式用 `env -i` 起。不会因为继承了登录环境而选中 `~/.anet` 或 `/root/.anet/ids/*`。
4. **只起停自己的单元与进程。** 单元名固定前缀 `anet-testnet-`(`common.sh` 拒绝别的前缀);用户模式只杀 pid 文件里、
   且 `/proc/<pid>/exe` 位于测试网目录之下的进程。teardown 的兜底也按可执行文件路径核对,不按进程名。
5. **端口被占即拒绝。** 每次启动前检查端口;被非本测试网的进程占着就报错退出,绝不去杀那个监听者。
   端口段 47100–47499 在勘察时全部空闲,节点表校验端口不出段、不重复。
6. **不绑 0.0.0.0。** hub 与 anetpeer 只绑主机表里的那一个地址(dmax 默认绑 zerotier 地址,不暴露到公网网卡);
   daemon 控制面只绑 127.0.0.1。
7. **资源上限。** 系统模式每个单元 `MemoryMax=2G`、`CPUQuota=200%`、`TasksMax=512`、`Nice=5`;用户模式 `nice -n 5`。
8. **teardown 只删测试网目录。** 删除前把路径解析成真实路径,必须是 `/opt/anet-testnet[/<run>]` 或
   `~/anet-testnet[/<run>]`,否则拒绝;`rm -rf --one-file-system`。注意 dmax 上已有的 `/opt/anet-test`(旧 QA 目录,
   少一个 `net`)不在匹配范围内。
9. **秘密不上命令行。** hub-admin 的 `ADMIN_TOKEN` 在远端生成、存 0600 的 `secret.env`、由启动器读入环境;
   `/hub-register` 与 `/status` 的 bearer 令牌经 0600 头文件交给 curl(`-H @file`),不出现在进程参数里
   (cmax、dmax 是多用户机器)。`bridge.sh mirror` 拷到本机的令牌文件 0600、目录 0700。

**不保证的:** CPU、内存、网络带宽与生产服务共享(上限见第 7 条);cmax 根分区已用 91%(剩 165G),测试网本身
只占几百 MB,但测试产生大量 CAS/日志时要留意;dmax 若改绑公网地址,测试 hub 在测试期间对 campus 网段可见。

## 生产服务警示

- **dmax(gpu2)**:`anet-dmax.service`(生产 `dmax-services` 节点,root,监听 `*:4002` 为 **x402 兑付口/voucher 网关**,
  控制口 127.0.0.1:29610,`HOME=/data/anet-node/home`)、`anet-dmax-svc.service`(127.0.0.1:8500 能力后端)、
  `anet-hub.service`(:8799 debug relay)、anetlinkd + anetmock(29xxx/30xxx,office 场景)、`realworld.timer`(每日),
  另有手工起的 hub(:18088 `/data/projs/anet-live`、172.17.0.1:29388 `/opt/anet-test`)与多个 `anet`/`anet-full` 进程,
  以及 anetos-*、smart-campus-*、vLLM、llama-server 等二十余个非 anet 服务。设计文档里的"官方 agent 运维账户"
  **尚不存在**(没有匹配 anet/agent/official/ops 的用户)。
- **cmax**:`anet4.service`(生产 `cmax-anet4` 节点,归属 emax hub)、`anet-face-daemon`/`anet-vision-daemon`
  (root,`/root/.anet/ids/*`)、`prodtest.timer`(**每小时**对生产双 hub 跑 prodtest)、anetcraft-*、约 88 个容器
  (其中 hermes/anetorg 容器里跑着 `anet daemon`,占 4001–4004)。

因此在 cmax、dmax 上:只用本目录的脚本部署;**不要直接运行 `scripts/joint.sh`、`scripts/scenario.sh`、
`scripts/joint-fleet.sh`**——见下一节第 1 条。

## 在测试网上跑 joint.sh / scenario.sh

这些脚本目前按"一台机器、多进程、全在 loopback"写。**本工作包不改它们**(别的工作包正在改),下面列出指向远端
需要的改造点(行号以 commit 6295665 为准),以及测试网这边已经提供的对接面。

> 现状(B5-04 之后):`scenario.sh` 已不按进程名停进程(第 1 条),接受 `JOINT_BIN`(本目录 `build.sh` 的产出),
> 端口段由 `SCENARIO_PORT_BASE` 给出(第 3 条,例如 47155,占 +0…+43;端口被占即退出,不停占用者),第 8 节
> 两 hub 跨 hub 用例可以放在一台主机的两个端口段(`XHUB_PORT_BASE2`,缺省 +50…+53),或经 ssh 把第二个 hub
> 与它的 provider 放到另一台主机(`XHUB_HOST2` + `XHUB_ADDR1/2`,例如 Ink89 上跑、Ink90 做第二边;需要 Ink89
> 能免交互 ssh/scp 到 Ink90)。
> 它仍自己起全部进程,不接入 `deploy.sh` 部署的节点。用法见 `scenario.sh` 文件头。

### 测试网提供的对接面

- `bridge.sh ctl-up`:每个远端 daemon 的控制口在本机同号可达(`127.0.0.1:47111` 就是 Ink89 上 d1a 的控制口)。
- `bridge.sh mirror`:`$TESTNET_STATE/nodes/<名>/.anet/{config.json,control_token.txt}` —— 与脚本里
  `home_of <node>`/`$HOME/.anet` 的布局一致,`ctl()` 这类"读 config.json 的 control_addr + 读令牌 + curl"的函数可原样用;
  `$TESTNET_STATE/nodes.env` 给出 `TN_<名>_{HOST,CTL,HUB,AID,HOME}` 与 `TN_<hub>_URL`。
- `deploy.sh stop|restart <节点>`:让对端离线/重启(对应 scenario 里"provider 重启后超过缓存时限回复")。
- `deploy.sh federate`:替代 scenario 第 7 节"写 federation.json 后重启两个 hub"。

### 改造点

1. **按进程名杀进程(必须先改,否则会杀生产)。** `joint.sh:64` `pgrep -x anet | xargs kill`、`joint.sh:94`
   (anetpeer)、`joint-fleet.sh:35` `pkill -x anet; pkill -x anet-hub`、`scenario.sh:80-82`、`scenario.sh:579`
   (anet-hub)。在 cmax/dmax 上以 root 运行会杀掉 `anet-dmax`、`anet4`、face/vision daemon、dmax 的 `anet-hub.service`
   乃至容器里的 anet(宿主机 root 看得见并能杀容器进程)。改为只杀自己 pid 文件里的进程,或按自己根目录下的可执行
   文件路径匹配(`joint-shell.sh`/`joint-invite.sh` 已是 `pkill -f "^$J/anet"` 的写法)。
2. **自起进程 → 可接入已部署节点。** 两个脚本都自己 `setsid` 起 hub、daemon、anetpeer、服务后端(`joint.sh:96-97,124-125,255`;
   `scenario.sh:86,135,178,345,565,580-581,593`)。需要一个开关(建议 `ANET_EXTERNAL=1` 或 `TESTNET_ENV=<nodes.env>`)跳过
   第 0 节的起停,改用 `nodes.env` 里的节点;重启类步骤改调 `deploy.sh restart <节点>`。
3. **地址与端口参数化。** `joint.sh:36` `RC/PC/MOCK` 常量、`joint.sh:131,133` 硬编码 `http://127.0.0.1:29088`;
   `scenario.sh:36-37` `HUB_PORT/HUB`、`:94` daemon 端口从 29510 递增、`:559-560` `HUB2=$HUB_PORT+1`、
   `:135,147,153` 服务后端 29520、`:158-159` x402 兑付口 29530。建议读 `HUB_URL`/`HUB2_URL`/`<节点>_CTL` 环境变量,
   默认值保持现状。注意 hub 的 URL 对不同观察者不同:lab 岛的 hub3 要走 ink88 的转发地址(`tn_hub_url` 已处理)。
4. **直接写节点目录里的文件。** `joint.sh:73-80,87,101-122`(config.json、peers.allow)、`scenario.sh:97-102,140-175,
   200-201,332-343,574-577,588-592,598`(config.json、peers.allow、federation.json)。节点在远端,这些写入要经 ssh
   落到远端节点目录(`<base>/<run>/nodes/<名>/home/.anet/`),或改走控制 API(若有对应端点)。写 config.json 的步骤之后
   需要 `deploy.sh restart <节点>`(daemon 只在启动时读模块配置)。
5. **本地读身份/私钥的 anetfixture。** `scenario.sh:57` `anetfixture aid --home` 可改读 `nodes.env` 的 `TN_<名>_AID`
   (或控制 API `/status`);`scenario.sh:520` `x402-authorize --home "$(home_of C)/.anet"` 需要 C 的私钥,必须在 C 所在主机上
   经 ssh 执行(测试网已把 `anetfixture` 装到每台主机的 `<run>/bin/`)。`joint.sh:82,88,127,187,209,214` 的
   `aid`/`org-genesis`/`cogunit`/`org-credential` 同理。
6. **能力后端要跑在 provider 那台机器上。** scenario 的 `scenario-svc.py`(`:110-135`)与 x402 兑付口按 `127.0.0.1` 配给 provider;
   在测试网上需在 provider 所在主机起(经 ssh,放进 `<run>/nodes/<名>/` 下,用 47x6x–47x9x 段的空闲端口)。
   daemon 规定非 loopback 的 `voucher_url` 只接受 https(A2A-DESIGN §18),跨主机兑付需要 TLS 终端;先把兑付测在
   provider 本机 loopback 上。
7. **joint.sh 的设备链(anetmock + anetlinkd)。** 测试网不部署它们。dmax 上现有的 anetmock(29xxx/30xxx)与 anetlinkd
   属于生产 office 场景,**不要复用**;要测就在 provider 主机上另起一套,`-base-port` 用 47x6x 以上的段。
8. **p2p 的 rendezvous。** `joint.sh:96-97` 用共享目录 `run/rv`,两台机器没有共享目录。测试网里 anetpeer 以 hub URL 作
   rendezvous、`--peer <地址>:<端口> --advertise <拨号地址>:<端口>`,脚本里对 p2p 的断言要按这个拓扑理解:
   Ink89↔Ink90、cmax→dmax 可直连;dmax→cmax 被 ufw 挡,应回落到 hub。
9. **路径与代理。** `joint.sh:34` `J=/tmp/joint`、`scenario.sh:34` `ROOT=/tmp/anet-scenario` → 指向
   `$TESTNET_STATE`;`NO_PROXY` 只列了 127.0.0.1/localhost,跨主机时需加 `10.2.2.0/24,10.253.20.0/24`(或 curl 用
   `--noproxy '*'`)。
10. **时延与等待。** 多处 `sleep 2..5` 与"轮询 40×0.5s"按 loopback 调的;跨 zerotier 与跨 hub 联邦(目录同步、
    `/fed/v2/keys` 拉取)更慢,等待要按条件轮询并放宽上限。
11. **跨岛联邦**(若要让 lab 的 hub 与 dmax 的 hub3 联邦):先 `bridge.sh fed-up`(在 ink88 上把 hub3 映射到
    `10.2.2.88:47301`、把 hub1 映射到 `10.253.20.2:47101`),再 `FEDERATE_BRIDGED=1 deploy.sh federate`。卡片里 hub 的
    `home` 仍是对方岛内地址,依赖 home 直连的路径在跨岛时会失败 —— 这是转发拓扑的限制,不是被测代码的缺陷。

## 已知限制

- `anet-official` 还没有源码(`ANet/cmd/anet-official` 不存在),`official` 角色部署会在传二进制时拒绝;其启动参数
  `TESTNET_OFFICIAL_ARGS` 与 daemon 配置模板 `TESTNET_OFFICIAL_CONFIG` 是占位,以 official 工作包为准。
- 瞬态单元与 setsid 进程都不跨重启;主机重启后重跑 deploy(身份与数据保留在目录里)。
- 用户模式没有自动重启;进程崩溃看 `deploy.sh status` 与节点目录里的日志。
- deploy 的系统模式沙箱参数只在本机用 `systemd-run --user` 校验过能被解析;首次在 cmax/dmax 部署时先部署一个 hub,
  看 `deploy.sh status` 与日志确认沙箱下能正常运行,再部署其余节点。
