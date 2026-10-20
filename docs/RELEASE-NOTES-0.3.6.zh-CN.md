# ANet 0.3.6 发布说明（草稿）

[English](RELEASE-NOTES-0.3.6.md)

草稿：尚未发布。发布日期：待定。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.1（未变更） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.6 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3（未变更） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

在 Linux 与 macOS 上安装：https://docs.agentnetwork.org.cn/docs/zh-cn/latest/intro/install.html#intro-install-script

## 升级

0.3.6 为补丁版本，未更改线路格式、身份格式与数据格式。0.3.5 节点与 0.3.6 节点可协同工作，hub 无需升级。使用 `anet update` 更新节点并重启其守护进程。在 Linux 与 macOS 上，安装命令现为单独一行 `curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh`：安装程序自行启动节点并加入 `https://hub.agentnetwork.org.cn`，安装后无需再运行 `anet up` 与 `anet hub-register`。不应加入 hub 的脚本或代理请传入 `--no-join`。`Upgraded from anet <version>` 一节现写入安装日志，不再出现在输出中。`ANET_INSTALL_BASE` 现在取代 `anet update` 的全部下载源，而不再是最先尝试的下载源。参见文档中的《升级到 0.3.6》。

## 变更

### Windows 安装
<!-- TODO（Windows 安装程序变更）：install.ps1 的变更。 -->

### Linux 与 macOS 安装
- 安装只需一行命令，安装程序从不读取其输入：

  ```shell
  curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh
  ```

  默认情况下，安装程序验证发布并将 `anet` 安装为 `~/.local/bin/anet`，接管较早的版本，运行 `anet init`，以 `anet up` 启动节点并等待节点响应（最多 30 秒），然后以 `anet hub-register <hub> --name <主机名> --passphrase-later` 加入 `https://hub.agentnetwork.org.cn`；该命令不进行任何询问，恢复口令之后以 `anet identity protect` 设置。正在运行的节点以新的二进制文件重启。已加入 hub 的节点保留该 hub，除非 `--hub` 指定了另一个 hub。在按 AID 准入的 hub 上，摘要给出由 hub 运营者运行的 `anet-hub admit <AID>` 命令；运营者运行后，加入过程自动完成。
- 新选项：`--no-join` 启动节点但不加入 hub；`--no-start` 既不启动节点也不加入 hub。`--hub URL` 与 `--name NAME` 指定加入的 hub 与名称。选项 `--system`、`--user`、`--prefix`、`--base`、`--shell`、`--without`、`--agents[=LIST]`、`--no-modify-path` 与 `--help` 保持不变。
- 输出为每个阶段（下载、接管较早版本、启动节点）一行简短文字，以及一段摘要：

  ```text
  Downloaded anet 0.3.6 for darwin-arm64 from npmmirror (8.6 MB in 2 s)
  Starting the node and joining https://hub.agentnetwork.org.cn

  anet 0.3.6 is installed and running.
    AID   bafyrei...
    Hub   https://hub.agentnetwork.org.cn (joined)
  Run 'anet' to get started.  Details: ~/.anet/logs/install-20261020T151644Z.log
  ```

  标准错误为终端时，下载以一行进度显示（进度条、已接收与预期的兆字节数、速度、剩余时间），约每秒刷新五次。进度条为红色；设置 `NO_COLOR` 时不使用颜色，非 UTF-8 区域设置下进度条以 ASCII 字符绘制。没有终端时，以一行 `Downloaded ...` 代替。接管较早版本时增加 `Replacing <版本>` 与 `Replaced <版本>; its data is kept in <目录>` 两行。每个留给用户的步骤为一行 `Note:`，附确切命令。使用 `--no-join` 时，hub 一行为 `Hub   not joined (--no-join)`；使用 `--no-start` 时，摘要为 `anet 0.3.6 is installed.` 与 `Run 'anet up' to get started.`。
- 其余内容，包括尝试过的下载源、每项检查、接管的每个步骤、已修改的 shell 配置文件，以及 `anet init` 与 `anet hub-register` 的输出，均写入 `<数据目录>/logs/install-<UTC 时间>.log`。数据目录为 `ANET_DATA_DIR`，未设置时为 `ANET_HOME`，再未设置时为 `~/.anet`；`logs` 目录的权限为 `0700`，日志文件为 `0600`。输出的最后一行给出日志路径。`ANET_LANG` 以 `zh` 开头时，各行以中文输出。
- 失败时依次输出 `Error:` 及原因与处理方法、`Nothing was installed or changed.`（接管过程中则为已完成的步骤），以及附日志路径的 `Details:`。
- 在终端中，安装程序最多询问一次 `sudo` 密码，适用于以下情形：较早的版本（AgentNetwork 1.x、anet 0.1 或 0.2）位于只有超级用户才能删除的位置，例如属于 `root` 的 `/usr/local/bin/anet`，或位于属于 `root` 的前缀下的 1.x npm 包；需要停用 AgentNetwork 1.x 的系统单元；或下文所述的命令链接需要超级用户权限。安装程序先输出一行说明原因，例如 `Removing AgentNetwork 1.1.5 from /usr/local/bin requires administrator rights; macOS will ask for your password once.`，然后运行 `sudo -v`；`sudo` 从终端读取密码，从不从脚本的输入读取。此后需要超级用户权限的步骤在该会话内以 `sudo -n` 运行。`sudo` 被拒绝或失败时，安装继续进行，摘要中以一行 `Note` 给出相应命令。没有终端时（如编程代理的工具调用或 CI 作业），不进行任何询问，`sudo` 只以 `sudo -n` 的形式使用。`--system` 保持不变，只使用 `sudo -n`。
- 二进制文件保留在用户可替换的 `~/.local/bin` 中，因此 `anet update` 无需管理员权限。当 `~/.local/bin` 不在运行安装程序的 shell 的搜索路径中、`/usr/local/bin` 在其中且有终端可用时，安装程序创建命令链接 `/usr/local/bin/anet`，指向 `~/.local/bin/anet`（`/usr/local/bin` 属于 `root` 时，在同一会话内使用 `sudo`；该位置的较早版本被替换），使 `anet` 在该终端中立即可用。没有终端时，最后一行提示在新终端中运行 `anet`，或在当前终端中先执行 `export PATH="$HOME/.local/bin:$PATH"`。除非指定 `--no-modify-path`，shell 配置文件中的标记段落仍照常写入。
- 若 AgentNetwork 1.x 的守护进程在其二进制文件已不存在时仍占用数据目录（例如二进制文件被手动删除，或守护进程由 MCP 客户端启动），安装程序会自行停止它：先通过 1.x 在 `127.0.0.1:3998` 上的关闭接口，使用数据目录中的 API 令牌，该令牌通过私有的 `curl` 配置文件传递，从不出现在命令行中；若守护进程仍在运行，则仅在根据其可执行文件确认该进程为 AgentNetwork 1.x 之后，向 `daemon.pid` 所指进程发送 `SIGTERM`。随后数据照常移至别处；若目录中同时含有 0.3 身份，则仅移出 1.x 条目。本版本以 0.3 身份运行的守护进程绝不会被当作较早版本，也绝不会收到信号。手动执行 `mv` 的提示已移除；只有无法停止的 1.x 守护进程才会留下一行 `Note`。
- 名为 `anet-daemon`、其 `ExecStart` 运行 AgentNetwork 1.x 二进制文件的 systemd 单元（即 1.x 提示其用户安装、带有 `Restart=always` 的单元），在停止守护进程之前被停用：用户单元使用 `systemctl --user disable --now anet-daemon`，系统单元在 `sudo` 会话内使用 `sudo -n systemctl disable --now anet-daemon`，否则以一行 `Note` 给出命令。单元文件不被删除。AgentNetwork 1.x 的 npm 包本身不注册任何服务或 launchd 代理。

### 下载源与 `anet update`
- `install.sh` 与 `anet update` 使用相同的下载源与相同的顺序。npmmirror（`registry.npmmirror.com`；npm 包 `@agentnetwork/<anet|anet-shell>-<linux|darwin>-<x64|arm64>`，以注册表的 tarball 下载，其 `package/dl/` 目录含 `release.json`、`release.json.sig` 与该版本的 `.gz`）与 GitHub（`github.com/ANetResearch/ANet/releases/latest/download/<文件>`；仅重定向至 `github.com` 与 `*.githubusercontent.com`）同时探测，时限 3 秒，按响应速度从快到慢尝试；无法连接的下载源被跳过。其后依次为 `registry.npmjs.org`（同一 tarball）、jsDelivr（`cdn.jsdelivr.net/npm/<包>@latest/dl/<文件>`），最后为项目主机 `agentnetwork.org.cn/dl` 与 `hub.agentnetwork.org.cn/dl`。不再使用 unpkg 路径与 npmmirror 的逐文件路径。
- `--base URL` 与 `ANET_INSTALL_BASE` 取代全部下载源，对 `anet update` 同样适用：`ANET_INSTALL_BASE` 现在取代下载源，而不再是最先尝试的下载源。
- 信任机制不变：由已签名的 `release.json`（发布密钥）以及 `.gz` 与二进制文件的 SHA-256 摘要决定。未提供发布的下载源（HTTP 错误、超时、HTML 页面、文件缺失、重定向至其不使用的主机、tarball 格式错误）被跳过；所提供的发布未通过验证的下载源使安装或更新终止。所提供的发布早于安装程序自身版本或已安装的 `anet` 的下载源视为滞后的镜像，只要另有下载源提供当前发布，即被跳过。
- 重定向由安装程序自行跟随，最多五次，仅限 `https://`，且仅限该下载源的主机。
- `anet update` 以有界大小读取 npm tarball，仅使用 `package/dl/` 下的普通文件，拒绝绝对路径或含 `..` 的条目名称，并在包元数据提供 sha512 完整性值时进行核对。其输出给出所用下载源，例如 `source    npmmirror (https://registry.npmmirror.com/@agentnetwork/anet-darwin-arm64)`。

### 持续集成
- 作业 `unix-upgrade` 在 macOS 与 Ubuntu 上，针对一个守护进程正在运行的 AgentNetwork 1.1.15 真实安装，以 `--no-join` 运行当前提交的 `install.sh`。它检查输出不超过 12 行，含下载行、接管行、摘要，最后一行给出日志；标准错误无任何输出；日志文件权限为 `0600`，所在目录为 `0700`，并记录各项检查与接管；第二次运行不再报告接管。
- 一项测试在临时主目录中以管道方式将完整的 `install.sh` 交给 `sh` 运行，所用发布以测试生成的密钥签名，由通过 TLS 提供服务的本地替身充当各下载源。该测试不连接任何 hub。

## 已知限制

- 0.3.5 所列各项限制依然存在。
