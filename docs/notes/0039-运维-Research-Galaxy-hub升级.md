# 0039 · 运维:Research-Galaxy 的 docker hub 与 agent-runtime 升级到 0.2.1

日期:2026-09-29(主机时间 CST 07:47–08:30;UTC 09-28 23:47 – 09-29 00:30)。主机 `root@emax.chatchat.space`。
授权:产品负责人决定升级 emax 上 Research-Galaxy 自带的 docker hub(0.1.x,含已公开的金额溢出缺陷,0034 §10 第 7 条、0031 §9 第 7 条),
数据保留、由 v0.2.1 首启迁移按设计处理;其 daemon 客户端同步升级到 anet 0.2.1,并按 0020 §三调整配置与应用代码(最小修改)。
边界:改动前后快照比对;只动 `anet-hub`、`agent-runtime` 两个 compose 服务、它们的镜像与构建文件、`hub-db-roll-docker.sh`;
nginx、防火墙、其他容器与服务未动;按 PID/路径停进程;报告与提交中不含令牌、密钥、口令。

## 结果一览

| 项 | 结果 |
|---|---|
| hub | `research-galaxy-anet-hub-1`:0.1.5 → **ANetHub 0.2.1**(`0b7f493`,GitHub release 二进制,`SHA256SUMS` 核对);首启迁移丢弃 18580 行已投递的 wire-1 明文中继、删 `completed_task`(35 行),`agent` 120 行保留;`X-ANet-Wire: 2`,无 wire 头 / wire 1 → **426**,wire 2 未签名 → **401** |
| 客户端 | `research-galaxy-agent-runtime-1` 内 103 个 daemon(2 个 requester、101 个 researcher):0.1.5 → **anet 0.2.1**(`20d58b1`,release.json 验签 Good,二进制 sha256 与签名清单一致);身份、对话数据原地保留,配置按 0.2 迁移(`accept_delegations` 删除,`inbound.policy=closed`);103 个都在 hub 上重新登记并发布了加密公钥 |
| 应用代码 | agent-runtime `app.py` 最小修改(peers.allow、暂停、改人设、线程只取文本、`/stream` 缺失的回退);两个 Dockerfile 改为钉死 0.2.1 发布二进制;compose 给 hub 加注册限速参数。补丁见 `0039-assets/rg/` |
| 备份脚本 | `/usr/local/sbin/hub-db-roll-docker.sh` 依赖 `delivered_at`(wire 2 已无此列,旧脚本会在今晚 04:20 失败);换成 ANetHub 0.2.1 `deploy/hub-db-roll.sh` 的逻辑(只改数据目录与日志路径两处缺省),手动触发一次 `Result=success` |
| 预演 | 本机(发布二进制 + mock LLM)与 emax 隔离网络(`--internal`,生产数据副本,mock LLM)两轮;发现并修掉三处会在生产出错的问题(§2) |
| 冒烟 | 两个 requester 各向一个真实 researcher 委派一次(真实 DeepSeek,回复"收到。",约 2 s),其中一次走平台后端自己的客户端代码(`app.anet.client`);改人设(同值)、暂停/恢复后再委派一次;均 `completed`。`https://anet.chat/` 200、`/healthz` 200、需登录的 API 401 |
| 停机 | anet 链路约 2.5 分钟(08:21:11 停 daemon → 08:23:36 全部 researcher 起来);其间无进行中的任务(hub 最后一条中继 09-26 11:15 UTC) |

## 1. 只读盘点

**容器与构建。** compose 项目 `research-galaxy`,文件 `/data/projs/anet-chat/Research-Galaxy/platform/docker-compose.yml`
(`docker compose ls` 还列出 `docker-compose.override.yml`,该文件已不存在;`edge`、`redis`、`postgres`、`anet-hub` 是带它创建的)。

| 容器 | 镜像(当时) | 端口 | 与 anet 的关系 |
|---|---|---|---|
| `research-galaxy-anet-hub-1` | `research-galaxy-anet-hub:latest` `5b226ec492a9`(08-10 构建),`anet-hub 0.1.5` | `8899/tcp` 仅 compose 网络内 | 本次对象;数据卷 `research-galaxy_anethubdata`(`hub.db` 346 MB、`guest_identity.kel`) |
| `research-galaxy-agent-runtime-1` | `research-galaxy-agent-runtime:latest` `f78fb267564f`(08-16),`anet 0.1.5` | `8700/tcp` 仅内网 | 本次对象;数据卷 `research-galaxy_anetdata`(`ids/` 下 103 个身份、`aid_index.json`,64 MB) |
| `research-galaxy-backend-1` / `worker-1` | `research-galaxy-app` | 内网 | 只经 agent-runtime 的 `/control/*`、`/agents/*` 使用 anet;不直连 hub(`ANET_HUB_URL` 只是配置项,代码未用) |
| `research-galaxy-minio-1` | `minio/minio` | **`0.0.0.0:9001`** | 已知的 9001 对外端口是 MinIO 控制台,不是 hub(见 §9 第 8 条) |
| `research-galaxy-edge-1` | `nginx:1.27-alpine` | `127.0.0.1:8080` | 前门;nginx 站点 `anet.chat` 反代到它 |

- 两个镜像原本都从兄弟目录 `/data/projs/anet-chat/ANetHub` 编译(context `../..`)。该目录现在是纯 hub 仓(HEAD `7567974`,是 v0.2.1 的祖先),
  已没有 `cmd/anet`:**agent-runtime 已无法按原 Dockerfile 重建**,hub 重建会得到一个未发布的旧线 hub。
- emax 上没有 buildx,`docker build` 需 `DOCKER_BUILDKIT=0`(经典构建器),`ADD --checksum` 等 BuildKit 语法不可用。
- nginx:没有任何站点反代到 hub。`research.agentnetwork.org.cn` 是静态论文页(`/var/www/research`),`galaxy.agentnetwork.org.cn` 301 到
  `anet.chat`,`anet.chat` 的 `/`、`/api/stream/` 反代 `127.0.0.1:8080`(edge)。
- `hub-db-roll-docker.{service,timer}`(每日 04:20,root)+ `/usr/local/sbin/hub-db-roll-docker.sh`:按 `delivered_at`/`created_at`(文本时间)
  删 wire-1 中继行,日志 `/var/log/hub-db-roll-docker.log`。`/root/hub-docker-backup-20260914.db.gz`(733 MB,0.1.x 库,含 wire-1 明文中继)。
- `anet-research-hub.service`(`127.0.0.1:8899`,容器化之前的同一 hub):disabled、inactive,未动。
- 客户端:agent-runtime 里 103 个 `anet daemon`(`ANET_HOME=/data/anet`,`hub_url=http://anet-hub:8899`),`requester-0/1`(`ANET_REQUESTER_POOL_SIZE=2`,
  只委派)与 101 个 researcher(openai 自动回复,DeepSeek `deepseek-v4-flash`,`api_timeout 900`,全部 `accept_delegations=true`,无暂停)。
- 应用代码在主机上:Research-Galaxy 工作树(`origin` = `github.com/ANetResearch/Research-Galaxy`,main `6555d57`)有大量未提交改动,
  但本次要改的 `agent-runtime/app.py`、`agent-runtime/Dockerfile`、`infra/anet/Dockerfile.hub` 与 HEAD 相同,运行中镜像里的 `app.py` 也与之相同(md5)。
  `docker-compose.yml` 在主机上本来就是已修改状态。
- 活动:hub 最后一条中继 09-26 11:15 UTC,之后无委派;`jobs` 表另有 40 条 09-26 起的 `queued`(与本次无关,未动)。

## 2. 兼容性:0020 §三 对照与预演中发现的问题

0020 §三 的行号出自旧版;按主机上的现行 `app.py`(829 行)逐项核对,并在本机用发布二进制实测:

| # | 现象(0.2.1 下) | 实测 | 处理(`app.py`) |
|---|---|---|---|
| 1 | `provision` 的 `hub-register … --accept-delegations true --guest-messages 0` | CLI 直接 `unknown flag --guest-messages`,退出码 2;`check=False` 吞掉 → 新 researcher 永远不注册 | 去掉两个参数;注册后写 `peers.allow` |
| 2 | 入站默认 closed | 不在 allow 的 requester 委派 → `rejected`(`anet.reason=not_accepting`) | `_sync_allow`:把 requester 池的 AID 写进每个 researcher 的 `peers.allow`(daemon 每次判定重读);`ensure`、`provision` 时同步 |
| 3 | `/accept {"enabled":true}` → 400;`accept_delegations` 被 daemon 删除 | 恢复(resume)失败;`ensure` 读不到键,永远显示 online | 暂停 = 标记文件 `.galaxy-paused` + 从 `peers.allow` 移除 requester 池;恢复反之;`ensure` 按标记报 `paused`;首启前把旧配置里 `accept_delegations=false` 的 researcher 转成标记(生产上 0 个) |
| 4 | 线程里出现 `status` 消息 | provider 模型超过 1 分钟未答时发 `working`(`from=them, kind=status, body=""`);被拒时有一条 `status` 拒绝说明 | `_normalize_thread` 只保留 `kind=="text"`。不改的话:任何超过 1 分钟的评审(实测 215 s)会把空的 status 当成回复交给后端,评审失败 |
| 5 | `/stream`、`/autoreply-prompt` 不存在(容器里那份 0.1.5 构建带有这两个路由) | 404 `page not found` | 流式适配:路由缺失即关闭 overlay,改走 `/thread` 回退(心跳照常);线程进入终态(rejected/failed/canceled/completed)且无回复时结束流,后端立即报"未回复",不再空等 900 s |
| 6 | 改人设:只能 `POST /autoreply` 整块替换 | 实测替换会**取消正在进行的模型调用,并给请求方发一条"服务出错"的回复** | `update_lens`:读现有 `auto_reply` 块只换 `system_prompt`;先等该 researcher 没有欠答的对话(最长 30 s),否则 409"researcher is replying",不动它 |
| 7 | hub 按客户端 IP 限 `/register`(10/分钟,突发 20);每个 daemon 启动时刷新注册并借此发布加密公钥 | emax 预演一次起 101 个 researcher:77 个 429,公钥未发布;向它们委派 → `400 … has published no encryption key set`,直到每小时的钥环维护才补发 | compose 给 hub 加 `--register-rate 600 --register-burst 200`(这个 hub 只在 compose 网络内,所有节点共用 agent-runtime 一个源地址);重测 101 个 23 s 全部发布,0 次 429 |

不需要改的:`/delegate`、`/message`、`/thread`、`/end` 形状不变;requester `/end` = 请求完成,provider 自动完成(实测 `completed`);
`/threads` 仍返回 `aid`;`anet id new/up/--id/id rm --purge`、`autoreply set` 全部参数照旧;控制面 `127.0.0.1` 满足回环 Host;
`hub-register --accept-delegations false`(requester 用)仍被接受并设为 closed。旧对话(0.1.5 的 interactions 库)0.2.1 可读,
启动后**没有**对旧对话自动补答(预演 mock LLM 计数与生产 daemon 日志都证实)。

## 3. 制品与验签

- ANetHub:`anet-hub-0.2.1-linux-amd64.gz` 与 `SHA256SUMS` 自 GitHub release 下载;`.gz` `96a89821…` 与解压后 `d81e1bc0…` 均 OK,
  与 0037 §3 记录的值相同;静态链接;`--version` = `0.2.1 … commit 0b7f493`。
- anet:`release.json(.sig)` 用 `internal/release/allowed_signers` 验签 Good(`SHA256:/4FMm/jgZcBII3z3O3r81Y8SxFfugdLRu3zj2gnclD4`);
  `anet-linux-amd64.gz` `7c3aa30c…`、二进制 `3d52e333…`、大小 19626735 与签名清单一致;default 变体。上传 emax 后再核一遍。
- 生产镜像(不重装 Python 依赖,只换二进制与 `app.py`,见 `0039-assets/rg/Dockerfile.*.overlay`):
  `research-galaxy-anet-hub:latest` = `:anet-0.2.1` `0d5059264955`;`research-galaxy-agent-runtime:latest` = `:anet-0.2.1` `fc36ee1605db`;
  回滚锚点 `:pre-anet-0.2.1`(`5b226ec492a9`、`f78fb267564f`)。构建时在镜像内再做一次 `sha256sum -c`。
- 仓库 Dockerfile(给维护者重建用)改为从 GitHub release `ADD` 同一资产并在 `RUN` 里核两次 sha256;在 emax 上用经典构建器各试建一次
  通过(`anet-hub 0.2.1`、`anet 0.2.1`、`import app` 正常),试建镜像与为此拉取的两个基础镜像已删除。注意:按仓库 Dockerfile 全量重建会重新解析
  未钉版本的 `requirements.txt`(试建得到 pydantic 2.13.5、uvicorn 0.53.0,运行中为 2.13.4、0.52.3)。

## 4. 预演

**本机**(发布二进制、本地 hub、mock OpenAI 接口、按容器依赖版本构建的 agent-runtime 测试镜像):§2 各项的现象与修复逐一验证;
agent-runtime 全接口(`/agents` 创建、`ensure`、`lens` 忙时 409 且进行中的回复不受影响 / 空闲时生效、`metadata`、`pause`/`resume`、
`/control/delegate|stream|thread|message|end`、`DELETE /agents`、容器重启后继续工作),75 s 慢回复经流适配只交付最终文本。

**emax 隔离预演**(`docker network create --internal`,不能出网):hub 数据用 `sqlite3 .backup` 在线复制、身份目录 `cp -a`,
副本里所有 researcher 的 `api_base` 改指 mock、`api_key` 清空;依次起 mock、redis、hub、agent-runtime(新镜像)。

- hub 首启迁移 11 s:`dropped 0 undelivered and 18580 delivered wire-1 message(s)`,库从 346 MB 变为 0.3 MB,`agent` 120 保留,新建 `hub_identity.kel`;426/401 与上表一致。
- 101 个 researcher 在 0.2.1 下起在 0.1.5 数据上:0 个失败、daemon 日志无 error,`accept_delegations` 全部移除、`peers.allow` 全部写好;
  mock 只收到测试本身的调用(无旧对话补答)。发现 §2 第 7 条(429 → 无公钥 → 委派 400),加参数后整组重启复测通过。
- 平台后端镜像 `research-galaxy-app` 起一次性容器,用 `app.anet.client.AnetControlClient` 委派 70 s 慢回复:心跳 7 次后收到最终文本,`/thread` 2 条、`end` ok。
- 新 `hub-db-roll-docker.sh` 对副本 `FORCE_WEEKLY=1` 运行:checkpoint、周备份(不含中继行)正常;旧脚本在新库上报 `no such column: delivered_at`。
- ANetHub `deploy/cleanup-content-v0.2.sh` 对副本干跑:[1] 迁移已丢弃全部 wire-1 行,无可删;[3] 评价内容列已无;**[8] `guest_identity.kel`(guest broker 私钥)仍在**(见 §9)。
- 预演容器、网络、数据副本(含私钥与明文中继)、测试镜像、一个预演 redis 留下的匿名卷(空)均已删除。

## 5. 切换(CST)

```sh
W=/root/rgal-upgrade-20260929-080728            # 0700
docker tag research-galaxy-{anet-hub,agent-runtime}:latest …:pre-anet-0.2.1            # 08:08,回滚锚点
cp -a <compose、两个 Dockerfile、app.py、hub-db-roll-docker.sh> $W/orig/; docker inspect 两个容器 > $W/orig/
docker exec research-galaxy-agent-runtime-1 anet stop --all      # 08:21:11,103 个 0.1.5 daemon 正常退出
docker compose stop agent-runtime anet-hub                        # 08:21:15
tar -C <两个卷的 _data> -czf $W/backup/{anethubdata,anetdata}-pre.tgz .    # 08:21:26–41,停服一致副本
cat <新文件> > <原文件>                                            # 保留属主 1001:1001 / root 与权限
docker tag …:anet-0.2.1 …:latest
docker compose up -d --no-deps --no-build anet-hub                # 08:21:52 → 08:22:03 healthy(迁移同预演)
docker compose up -d --no-deps --no-build agent-runtime           # 08:22:23 → 08:22:39 healthy
docker exec research-galaxy-redis-1 redis-cli del gx:anet:requesters   # 08:23:13,见下
逐个 POST /agents/<aid>/ensure(101 个,23 s)                       # 08:23:13–36
```

- 备份:`anethubdata-pre.tgz` 90346551 B `5ea4da38…`,`anetdata-pre.tgz` 5144243 B `f7eb7f8d…`(`$W/backup/SHA256SUMS`,0600)。
- 只有 `anet-hub` 的 compose config-hash 变了(加了 `command:`);`agent-runtime` 的 hash 与原容器相同,只因镜像变化重建;其他服务的 hash 未变,未动。
- requester 缓存:agent-runtime 把 requester 列表放在 Redis `gx:anet:requesters`(TTL 120 s,每 45 s 续期)。新容器起来时旧键未过期,刷新线程只续期、
  不重新拉起 requester,于是容器里 0 个 daemon。这是原有逻辑(第一次委派失败时 `_repair_requester` 会拉起),为让 requester 立即在新 hub 上登记,
  删了这个缓存键,由服务自己重新 provision(两个 requester 起在 0.2.1、登记、发布公钥)。

## 6. 验证

| 项 | 结果 |
|---|---|
| hub `/healthz` | `0.2.1` / `0b7f493` / `2026-09-28T22:38:54Z`;响应头 `X-Anet-Wire: 2`;`llms.txt` 写明 `requires anet >= 0.2.0` |
| 426/401 | `POST /relay/poll` 无 wire 头 426(正文 `requires anet >= 0.2.0`)、`X-ANet-Wire: 1` 426、`X-ANet-Wire: 2` 未签名 401;`/relay/send` wire 2 未签名 401 |
| 数据 | 前:`agent 120`、`completed_task 35`、`relay_message 18580`、`review 0`;后:`agent 120`、`completed_task` 已删、`relay_message 0`、`review 0`、`agent_keys 103`;`hub_meta.relay_v2_dropped_delivered=18580`、`…undelivered=0` |
| 客户端 | 容器内 103 个 `anet daemon`,`anet version` 0.2.1;103 个配置无 `accept_delegations`,`inbound.policy=closed`;101 个 `peers.allow` 各含 2 个 requester AID;0 个暂停标记;启动后 daemon 日志无 error/fail/429 |
| 注册与委派 | 两个 requester 与 101 个 researcher 均在 hub 登记并发布公钥;requester-0 → Knuth、requester-1 → Liskov(后者经平台后端客户端代码)各一次,真实 DeepSeek 回复"收到。"约 2 s,两边 `end` 后 `completed` |
| 管理路径 | Knuth:改人设(同值)200 且 `auto_reply` 块哈希不变;暂停 → `peers.allow` 0 行、`ensure` 报 `paused`;恢复 → 2 行、`online`;随后委派一次正常 |
| 前端 | `https://anet.chat/` 200(`<title>ANet Research</title>`),`/healthz` `{"status":"ok","env":"production"}`,`/api/researchers` 等 401(后端在线、需登录);全部 research-galaxy 容器 healthy,两容器 `RestartCount=0` |
| 备份脚本 | `systemctl start hub-db-roll-docker.service` → `Result=success`,日志 `relay_backlog=0`、checkpoint;timer 下次 09-30 04:2x |
| 未做 | 用浏览器登录走一遍评审/圆桌(没有也不应使用用户凭据);以上平台后端客户端代码 + agent-runtime 全路径的冒烟代替 |

## 7. 前后快照比对

方法同 0037(`systemctl list-units --all`、`list-unit-files`、`list-timers`、`ss -tln/-uln`、`docker ps -a`、镜像、卷、网络、`nginx -T` 哈希、相关文件哈希、
Research-Galaxy `git status`),基线 07:47,结束 08:29:

- 监听端口(TCP/UDP)集合相同;service 单元状态相同;timer 相同;nginx 配置哈希不变。
- 容器集合相同(两个被重建,ID 变了,相应的 scope/netns/veth 变化);卷集合相同;网络相同(预演网络已删)。
- 镜像:多出 `research-galaxy-{anet-hub,agent-runtime}:{anet-0.2.1,pre-anet-0.2.1}` 标签(`latest` 指向新镜像)。
- 文件:`hub-db-roll-docker.sh`、`docker-compose.yml`、两个 Dockerfile、`app.py` 五处(哈希见 §8);Research-Galaxy `git status` 多出 3 个 `M`
  (`agent-runtime/app.py`、`agent-runtime/Dockerfile`、`infra/anet/Dockerfile.hub`;compose 原本就是 `M`)。
- `research-galaxy-worker-1` 使用的镜像 `71a63c6a356d` 在基线时已不在镜像列表里(`docker system df` 14 个镜像均在用),不是本次造成。

## 8. 改动清单

emax:

| 对象 | 改动 | 之后的 sha256 |
|---|---|---|
| `platform/agent-runtime/app.py` | §2 第 1–6 条(+137/−25 行) | `2d16fcee…` |
| `platform/agent-runtime/Dockerfile` | Go 源码构建阶段 → 下载 anet 0.2.1 发布资产并核两次 sha256 | `c1795dcb…` |
| `platform/infra/anet/Dockerfile.hub` | 同上,ANetHub 0.2.1 | `3f3e7d77…` |
| `platform/docker-compose.yml` | `anet-hub` 加 `command:`(原参数 + `--register-rate 600 --register-burst 200`) | `ae71b250…` |
| `/usr/local/sbin/hub-db-roll-docker.sh` | 换成 ANetHub 0.2.1 `hub-db-roll.sh`(缺省数据目录 = 该卷,日志仍是 `/var/log/hub-db-roll-docker.log`) | `3fb53793…` |
| 镜像标签 | 见 §3 | |
| 卷数据 | hub 首启迁移(不可逆);103 个身份目录的配置迁移、`peers.allow`、`enc_keys.cbor` 等 0.2 文件 | |
| Redis | 删一次缓存键 `gx:anet:requesters`(服务随即重写) | |
| `$W=/root/rgal-upgrade-20260929-080728/`(0700,92 MB) | `backup/`(两卷停服副本)、`orig/`(五个原文件与两容器 inspect)、新文件、快照、行数、试建日志、验签用的 `release.json(.sig)`/`SHA256SUMS` | |

本仓(`wp/rgal`):本记录;`0039-assets/rg/research-galaxy-anet-0.2.1.patch`(四个 Research-Galaxy 文件相对切换前的补丁,在 Research-Galaxy 仓库根目录
`patch -p1` 可复现上表哈希;compose 部分以主机上的版本为底)、`0039-assets/rg/Dockerfile.*.overlay`(生产镜像的构建文件)、
`0039-assets/hub-db-roll-docker.sh`。

## 9. 回滚

```sh
cd /data/projs/anet-chat/Research-Galaxy/platform
docker exec research-galaxy-agent-runtime-1 anet stop --all; docker compose stop agent-runtime anet-hub
# 卷还原:清空两个 _data 后 tar -xzf $W/backup/{anethubdata,anetdata}-pre.tgz(先 sha256sum -c SHA256SUMS)
docker tag research-galaxy-anet-hub:pre-anet-0.2.1 research-galaxy-anet-hub:latest
docker tag research-galaxy-agent-runtime:pre-anet-0.2.1 research-galaxy-agent-runtime:latest
cat $W/orig/{docker-compose.yml,Dockerfile.hub,Dockerfile,app.py} > 各原位置; cat $W/orig/hub-db-roll-docker.sh > /usr/local/sbin/hub-db-roll-docker.sh
docker compose up -d --no-deps --no-build anet-hub agent-runtime
```

hub 迁移不可逆,回滚只能用停服副本;回滚后切换以来 hub 上的新登记与对话都会丢失。

## 10. 遗留与需要负责人的事项

1. **流式预览没有了。** 容器里那份 0.1.5 构建带有 `/stream`(推理/正文预览)与 `/autoreply-prompt`,anet 0.2.1 两者都没有。评审、聊天仍正常完成,
   但前端在模型作答期间只看到等待(心跳),直到整条回复到达。要恢复需在 anet 里提供等价的预览接口(需要 PO 决定是否列入路线),或接受。
2. **改人设在 researcher 忙时返回 409。** 为避免取消进行中的回复(§2 第 6 条)。用户在其 researcher 评审中改人设会看到失败提示,稍后重试即可;
   管理端批量重渲染桂冠人设时,忙的那几个会记为失败,需再跑一次。
3. **Research-Galaxy 的代码改动只在 emax 工作树里**(未提交、未推送,仓库本身还有大量他人的未提交改动)。需要其维护方把
   `0039-assets/rg/research-galaxy-anet-0.2.1.patch` 合入上游;GitHub 上的 Research-Galaxy 仍会构建出与 0.2.1 hub 不兼容的旧组合。
   同时建议钉死 `agent-runtime/requirements.txt`。`setup/lab_up.py`(开发用实验室脚本,未部署)仍按旧 `/threads` 语义,未改。
4. **明文与私钥备份(需 PO 决定删除时点)。** `$W/backup/*.tgz` 含 18580 行 wire-1 明文中继与 103 个身份私钥,是回滚所需,建议观察期后删除;
   `/root/hub-docker-backup-20260914.db.gz`(733 MB,0.1.x 库,含 wire-1 明文中继)未动。按 A2A-DESIGN §9 两者都属应清除的内容。
5. **`guest_identity.kel`**(hub 卷内,旧 guest broker 身份的私钥)仍在;`cleanup-content-v0.2.sh` 的第 8 项,删除需 PO 批准(未删)。
6. **compose 文件与运行中的容器已有漂移(本次之前就有)。** `docker-compose.override.yml` 已不存在,而 `edge` 等是带它创建的;
   按现有文件做一次全量 `docker compose up -d` 会把 edge 改成 `0.0.0.0:8080`、并新起 prometheus(`9090`)与 grafana(`3001`)。维护时只对具体服务
   `--no-deps` 操作,或先恢复 override。另:`worker-1` 跑的镜像已不存在且与 backend 不同(compose 注释要求二者同镜像)。
7. **原有逻辑的两个小问题(未改)。** agent-runtime 重启后 Redis 里的旧 requester 列表会被续期,requester 要等第一次委派失败后才被拉起(§5);
   删除 researcher(`id rm --purge`)不会从 hub 注销,hub 上留下失联条目。
8. **MinIO 控制台 `0.0.0.0:9001` 对公网监听**(docker-proxy,compose 里注明"optional")。与 hub 无关,本次未动(不改防火墙、端口);建议评估是否关闭。
9. **平台后端对"被拒"的提示。** researcher 不接受时(如暂停、allow 缺失),流适配立即结束,后端报的是"did not reply within Xs",文字不准确但不再空等。
10. hub 二进制未签名(同 0037 §10 第 4 条);这台 hub 不联邦、不对外,`--public-url` 为空(按请求来源)。`apt`/`pip` 未升级,Python 环境与切换前相同。
