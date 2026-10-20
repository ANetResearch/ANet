# ANet 0.3.5 发布说明（草案）

[English](RELEASE-NOTES-0.3.5.md)

发布日期：发布时确定。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.1（未变更） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.5 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3（未变更） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

在 Windows 上安装：https://docs.agentnetwork.org.cn/docs/zh-cn/latest/intro/install.html#intro-install-windows

## 升级

0.3.5 为补丁版本，未更改线路格式、身份格式与数据格式。0.3.4 节点与 0.3.5 节点可协同工作，hub 无需升级。使用 `anet update` 更新节点并重启其守护进程。在 Windows 上，再次运行安装命令同样会更新安装；若原安装目录通过检查，则继续使用该目录。安装程序不再为 `-Controller` 请求确认：依赖该确认作为第二步的脚本或文档，现在会在命令运行时即授予访问权限。在 Linux 与 macOS 上，再次运行 `install.sh` 命令会在同一次运行中替换 AgentNetwork 1.x、anet 0.1 与 anet 0.2，参见文档中的《升级》。

## 变更

### Windows 安装
- 安装只需一行命令，该命令在 `cmd.exe`、Windows PowerShell 5.1、PowerShell 7、WSL 1 与 WSL 2 中的 `bash` 和 `zsh` 以及 Git Bash 中完全相同：

  ```text
  powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol=3072; & ([scriptblock]::Create((irm -UseBasicParsing https://agentnetwork.org.cn/install.ps1))) -Controller CONTROLLER_AID"
  ```

  不带 `-Controller CONTROLLER_AID` 时，该命令仅安装默认版本。由于 `cmd.exe` 将 `<` 与 `>` 解释为重定向，占位符采用普通单词；安装程序拒绝未替换的占位符。在 WSL 中运行时，该命令以 Windows 用户身份在 Windows 主机上安装 Windows 版本；在 Linux 发行版内部安装请使用 `install.sh`。须启用 WSL 互操作（默认已启用）。
- 安装程序不进行任何询问，也不从控制台读取任何内容。命令行中的 `-Controller` 即为对授权的同意，0.3.4 中的控制台确认已移除。安装结束时，安装程序列出从 `config.json` 与 `peers.allow` 读回的已授予权限，以及撤销权限的两个步骤。在没有控制台的环境中（如代理的工具调用、CI 作业或 WSL 互操作）安装同样可以完成。
- `powershell.exe` 的退出状态即为安装结果。在已打开的 PowerShell 窗口中运行时，脚本不会关闭该窗口，并设置 `$LASTEXITCODE`。
- 不再拒绝提升权限的窗口，`-Elevated` 仍被接受但不产生任何作用。ANet 为当前账户安装，使用该账户自己的目录、`PATH` 与 `Run` 值。节点通过一次性的任务计划程序任务启动，该任务使用交互式令牌与最低权限，因此节点不以提升后的令牌运行。若 Windows 未为该账户提供标准用户令牌（用户账户控制已关闭，或为内置 Administrator 账户），或该任务无法运行，则节点以窗口的权限启动，安装程序以一行文字说明这一情况。
- 未指定 `-Prefix` 时，安装程序自动选择安装目录：已有 0.3 或更高版本安装所在的目录、`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin` 或 `%USERPROFILE%\.anet-bin`，取第一个通过检查的目录。每个被跳过的目录均附原因输出，并写明相关账户的名称；Windows 无法解析名称的账户以其 SID 加 `(unresolved)` 表示。缺失的各级目录在创建时设置各自的私有访问控制列表。
- 私有性检查忽略仅向其他账户授予同步、遍历与读取属性权限的访问控制项，例如 Windows 在用户配置文件目录上授予应用容器功能的访问控制项。授予读取、写入、创建、删除、读取或更改权限、取得所有权或通用权限的访问控制项仍不能通过检查，`CodexSandboxUsers` 等组的“修改”权限亦然。对安装目录上级目录的检查保持不变。
- 安装程序接管该用户较早的安装。位于 `PATH` 或全局 npm 前缀中的 1.x npm 包、0.1 或 0.2 版本的 ANet，会在时间限制内以其自身的 `stop` 命令停止；其 npm 包通过 `npm uninstall -g @agentnetwork/anet` 卸载，无法卸载时输出该命令；守护进程停止后，其位于 `%USERPROFILE%\.anet` 的数据被重命名为 `%USERPROFILE%\.anet-legacy-<版本>-<UTC 时间>`。若该目录中已有 0.3 或更高版本的身份，则仅移出较早版本的条目。不删除任何内容。其他 0.3 或更高版本的安装保留原处，并从用户的 `PATH` 中移除；0.3 或更高版本的 npm 启动器不受影响。
- 安装目录置于用户 `PATH` 的首位。安装程序以新窗口所得的 `PATH` 启动一个新的 `powershell.exe`，确认实际运行的 `anet`；若计算机 `PATH` 中存在较早的 `anet`，则予以报告。
- 新参数 `-NoJoin` 启动节点但不注册到 hub。

### Linux 与 macOS 安装
- `install.sh` 在同一次运行中替换早期版本，不要求任何输入：AgentNetwork 1.x（npm 包 `@agentnetwork/anet` 1.x）、anet 0.1 与 anet 0.2。安装脚本用早期版本自身的 `stop` 命令停止其守护进程；用 npm 删除 npm 包 `@agentnetwork/anet` 1.x；将早期的 `anet` 二进制文件替换为指向新二进制文件的链接，在只有超级用户可写的目录中则予以删除。早期的数据目录改名为 `~/.anet-legacy-<版本>-<UTC 时间>`，不删除任何内容，其中已有的 0.3 身份保留原处。0.3 及更高版本的 `anet` 与 npm 启动器保持不变。
- 安装脚本在 shell 配置文件（zsh、bash、fish、sh）中加入一段带标记的内容，把安装目录放在搜索路径最前，用新的登录 shell 确认 `anet` 即新的二进制文件，并打印适用于当前终端的命令。`--no-modify-path` 不修改配置文件。
- `sudo` 只以 `sudo -n` 的形式使用。需要密码的步骤不会执行，报告给出确切的命令。`install.sh --system` 不再询问密码，请先运行 `sudo -v`。
- 输出末尾的 `Upgraded from anet <version>` 一节列出每一步。
- 守护进程也拒绝在没有 0.3 身份的 AgentNetwork 1.x 数据目录中启动，报告 `identity_v02`，其提示语句同时指明两种早期版本。

### 命令行
- `anet init --shell-controller <AID> --yes` 无需确认即写入控制方授权，命令行即为确认。不带 `--yes` 时，与以往相同，在终端上确认授权；没有终端的进程以 `tty_required` 拒绝。`--yes` 仅可与 `--shell-controller` 一起使用。

### 持续集成
- 作业 `windows-install` 在 `windows-latest` 上以发布的命令，分别从 `cmd.exe`、Windows PowerShell 5.1、PowerShell 7 与 Git Bash 端到端运行 `install.ps1`。它安装由作业 `windows-dist` 构建、并以本次运行生成的密钥签名的当前提交的发布版本，接管预先放置的 1.1.15 npm 包安装，并检查授权、`Run` 值、`PATH`、私有安装目录与 `anet version`。不连接任何 hub。

## 已知限制

- WSL 不在持续集成范围内；该命令在 WSL 1 与 WSL 2 中的行为在物理 Windows 机器上验证。
- 从提升权限的窗口经任务计划程序启动的节点在任务结束后是否持续运行、是否不具有管理员权限，须在启用用户账户控制的物理 Windows 机器上验证。
- 0.3.4 所列 Windows 版本的各项限制依然存在。
