# 在一台崭新的 Debian 机器上接入 ANet

面向:刚装好系统的 Debian 12/13(amd64 或 arm64),要接入 `https://hub.agentnetwork.org.cn`,
并且希望这台机器能被网络另一端的 agent 要求执行几条运维命令。

全程不需要 Go、不需要 C 工具链,只要 `curl` 和出站 HTTPS。

> **真的白板系统要先补两样。** `debian:12` 这类最小系统既没有 `curl` 也没有
> `ca-certificates`,没有后者 HTTPS 会直接失败;`uptime` 这类顺手会写进命令表的
> 东西也不在(属于 `procps`)。
>
> ```sh
> apt-get update && apt-get install -y curl ca-certificates procps
> ```

## 0 · 先决定装哪个变体

发布里每个平台有两个二进制,差别是**这个二进制能做什么**,不是配置项。

| 变体 | 能不能在本机执行命令 |
| --- | --- |
| `anet`(默认) | **不能**。`module/shell` 根本没编进去 |
| `anet-shell`(`--shell`) | 能,但要先配置并列名单 |

不确定就装默认版。以后要换,重跑一次带 `--shell` 的安装即可,身份和数据都在
`~/.anet`,不受影响。

装完可以自己核对,这不是一句承诺:

```sh
anet version
# anet 0.1.7 (commit …, built …)
# modules: anetlink,blackboard,cas,org,p2p,service,taskboard,x402         ← 默认版
# modules: anetlink,blackboard,cas,org,p2p,service,shell,taskboard,x402   ← shell 版
```

`modules:` 那行是从二进制里**实际链接进来的**模块注册表读出来的,不是构建时刻进去
的一个字符串,所以它不会说谎。不信任这个输出的话,直接查二进制本身:

```sh
strings "$(command -v anet)" | grep -c 'shell\.run@'    # 默认版 → 0,shell 版 → 1
go tool nm "$(command -v anet)" | grep -c module/shell   # 默认版 → 0,shell 版 → 23(需装 Go)
```

发布的二进制保留符号表就是为了让第二条能跑。

## 1 · 一行安装并入网

```sh
curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- \
  --hub https://hub.agentnetwork.org.cn \
  --name $(hostname)
```

要能执行命令的那个变体,加 `--shell`:

```sh
curl -fsSL https://agentnetwork.org.cn/install.sh | sh -s -- --shell \
  --hub https://hub.agentnetwork.org.cn \
  --name $(hostname)
```

这一条做了四件事:按平台下载对应二进制、比对 sha256、装到 `~/.local/bin/anet`、
启动节点并注册到 hub。输出末尾会打印本机的 AID。

装到 `/usr/local/bin` 用 `--system`(会用 sudo)。

`~/.local/bin` 不在 PATH 时脚本会提示,按提示加一行到 `~/.bashrc` 即可。

**hub 注册是开放的,没有邀请码。** hub 校验的是"密钥历史能推出所声称的 AID"和
"能签出挑战",也就是证明你控制这个身份;它不限制谁能注册。这不构成风险:
**这台机器愿意为谁做什么,是这台机器决定的,不是 hub 决定的** —— 见第 3 节的名单。

## 2 · 确认状态

```sh
anet status          # AID、数据目录、控制口
anet version         # 版本、commit、以及这个二进制里有哪些模块
```

在 hub 上应该能看到它:

```sh
curl -s https://hub.agentnetwork.org.cn/agents | grep "$(anet status | grep -o 'bafyrei[a-z0-9]*' | head -1)"
```

## 3 · 让它可以被远程执行命令(仅 shell 变体)

三道闸门,少任何一道都执行不了:二进制里有这个模块、有配置块、名单里有调用方。
第 1 节装的是 shell 变体,第一道已过。下面配后两道。

### 3.1 写配置块

编辑 `~/.anet/config.json`,加一个 `modules.shell`:

```jsonc
{
  "modules": {
    "shell": {
      "commands": {
        "uptime":      { "run": "uptime", "description": "负载与运行时长" },
        "disk":        { "run": "df -h" },
        "tail-syslog": { "run": "journalctl -n 200 --no-pager" },
        "restart-app": { "run": "systemctl restart myapp", "description": "重启业务进程" }
      },
      "allow_file": "/etc/anet/shell-allow",
      "timeout_s": 60,
      "max_output_bytes": 65536
    }
  }
}
```

只有 `commands` 里列出的命令能被调用,能力 id 是 `shell.run@uptime` 这种形式。
要开放任意命令得显式加 `"allow_arbitrary": true`,默认不开。

> 命令名会随节点的能力清单通告到 hub 目录,任何能浏览目录的人都看得到这台机器
> **可以被要求做什么**(看不到能不能做成,也拿不到执行权)。不想公开的命令,
> 名字别带信息。

### 3.2 建名单文件

```sh
sudo mkdir -p /etc/anet
sudo touch /etc/anet/shell-allow
sudo chmod 600 /etc/anet/shell-allow
```

**先留空。** 空名单拒绝所有远程调用,这是对的起点。

### 3.3 重启节点并**重新注册**

```sh
anet stop && anet up
anet hub-register https://hub.agentnetwork.org.cn --name $(hostname)
```

两条都要。重启让配置生效,但**能力清单是 `hub-register` 那一刻折进注册的** ——
只重启不重新注册,hub 那边的 `caps` 仍是空的,别人 `anet find --cap shell.run@…`
找不到这台机器。按 AID 直接委派仍然可用(能力由本节点解析),所以这个漏做不会
报错,只是让节点在目录里查不到。实测于 dmax 上的容器。

启动时如果名单为空,会打印一行提醒:`no caller is allowed`。这行是预期的。

### 3.4 把调用方加进名单

在**要发起调用的那台机器**上拿它的 AID:

```sh
anet status | grep -o 'bafyrei[a-z0-9]*' | head -1
```

回到 Debian 机器,把这个 AID 写进名单:

```sh
echo 'bafyrei……对方的AID' | sudo tee -a /etc/anet/shell-allow
```

**不需要重启。** 名单文件每次调用都重读,写进去下一次调用就生效。

### 3.5 从对面试一下

```sh
anet delegate <Debian机器的AID> --capability shell.run@uptime
anet results
```

应该拿到 `OK` 和 uptime 的输出。

## 4 · 收权

删掉那一行,或者直接删掉整个文件:

```sh
sudo sed -i '/对方的AID/d' /etc/anet/shell-allow
# 或者一次性全撤:
sudo rm /etc/anet/shell-allow
```

下一次调用即被拒,不用重启。文件不存在等同空名单(拒绝所有),不是错误、更不是放行。

彻底不干了:

```sh
anet hub-leave     # 从 hub 注销
anet stop
```

## 5 · 让它开机自启

`anet up` 起的进程不跨重启。真机上用 systemd:

```sh
sudo tee /etc/systemd/system/anet.service >/dev/null <<EOF
[Unit]
Description=ANet daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$USER
Environment=ANET_DATA_DIR=$HOME/.anet
ExecStart=$HOME/.local/bin/anet daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

anet stop                       # 先停掉手工起的那个
sudo systemctl daemon-reload
sudo systemctl enable --now anet
systemctl status anet --no-pager
```

**`User=` 决定命令以谁的身份执行。** shell 模块不提权、不调 sudo:
`User=root` 的 daemon 才能跑 root 命令,`User=你自己` 就只能跑你自己能跑的。
这是安装期的决定,写在 unit 文件里、看得见,不是模块能授予的东西。

要跑 root 级运维命令就把 `User=` 设成 `root`,并把 `ANET_DATA_DIR` 改到
`/root/.anet`。**这一步之后,名单里的每个 AID 都能在这台机器上以 root 跑你列出的
那几条命令**,名单要按这个前提来写。

## 6 · 排查

| 现象 | 原因 |
| --- | --- |
| `module "shell" is configured but not compiled into this build (it needs -tags shell…)` | 装的是默认变体,重跑安装带 `--shell` |
| 调用回 `UNAVAILABLE`,消息是 `does not accept commands from …` | 调用方 AID 不在名单里 |
| 调用回 `UNAVAILABLE`,消息是 `carry no caller identity` | 本机直调需要配 `"allow_local": true` |
| 委派一直没有结果,超时 | 该能力这台机器没有提供。核对能力 id,`shell.list` 能报出它有哪些 |
| 命令回 `FAILED` 带退出码 | 命令真的失败了,`observed_state` 里有 stderr |

节点自己的日志在 `~/.anet/daemon.log`。

完整的配置项、行为约定与已知取舍见 [SHELL-zh.md](SHELL-zh.md)。
