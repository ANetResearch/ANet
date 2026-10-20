# ANet 0.3.4 发布说明

[English](RELEASE-NOTES-0.3.4.md)

发布日期：2026-10-20。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.1（未变更） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.4 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3（未变更） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

在 Windows 上安装：https://docs.agentnetwork.org.cn/docs/zh-cn/latest/intro/install.html#intro-install-windows

## 升级

0.3.4 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.3 的节点与 0.3.4 的节点可协同工作，Hub 无需升级。节点使用 `anet update` 更新并重启其守护进程；数据目录、身份和配置均保留。通过 npm 安装的节点使用 `npm update --global @agentnetwork/anet` 更新。

在 Linux 与 macOS 上有一项检查发生变化：`anet doctor` 仅在恢复文件可被其他账户读取时才在 `identity.recovery` 下报告该文件，因此权限为 0400 的恢复文件不再被报告。在 Windows 上，`anet` 通过 PowerShell 脚本 `install.ps1` 安装。

## 变更

### Windows
- `anet` 原生运行于 Windows 10 1809 或更高版本以及 Windows Server 2019 或更高版本，支持 amd64 与 arm64，包括缺省版本和含 `shell` 模块的版本。无需 WSL2。
- PowerShell 脚本 `install.ps1` 仅借助 Windows PowerShell 5.1 或 PowerShell 7 安装 `anet`，不依赖其他工具。该脚本带有签名（`install.ps1.sig`），并载有四个 Windows 版本的 SHA-256 摘要。下载的二进制文件须与其摘要一致，随后由其自身校验已签名的发布清单，之后才会安装任何内容。安装程序拒绝在提升权限的窗口中运行，除非指定 `-Elevated`；它检查安装目录仅对该用户私有，将该目录加入用户的 `PATH`，并通过该用户的 `Run` 值在登录时启动节点。指定 `-Controller <AID>` 时，它安装含 `shell` 模块的版本，在控制台上确认一次后仅向该 AID 开放此模块，并加入 `https://hub.agentnetwork.org.cn`（指定 `-Hub` 时除外）。
- 守护进程的数据保存在 `%USERPROFILE%\.anet`，运行时文件保存在 `%LOCALAPPDATA%\anet\run`。私有文件和目录带有仅允许该用户、`SYSTEM` 与 `Administrators` 访问的访问控制列表。机密文件自创建之时起即为私有，打开文件时不跟随重解析点。`anet up` 使守护进程脱离控制台启动；它以已登录用户的身份运行，从不作为 Windows 服务运行。
- `shell` 模块缺省使用 Windows PowerShell 执行命令；可配置 `pwsh`、`cmd` 或受支持解释器的绝对路径。命令作为作业对象中的进程树运行，并整体结束。包含字符 `" % ! ^ & | < > ( )`、回车或换行的参数将被拒绝，因为批处理文件会再次解释这些字符。
- `anet update` 通过将正在运行的 `anet.exe` 改名移开来替换它，后续步骤失败时将其恢复，并在之后的运行中删除已移开的副本。
- `anet pull` 将接收的文件标记为从 Internet 下载（`Zone.Identifier`），并为资源管理器会对其执行操作的文件名添加后缀 `.txt`，例如 `.lnk`、`.url`、`.scf` 与 `desktop.ini`。
- 自动回复在 Windows 上没有沙箱，拒绝所有需要沙箱的运行；它同样拒绝作为批处理文件的智能体程序。
- Unix 套接字上的后端、同一主机上的 TCP 后端、`p2p` 模块和 ANetLink 套接字均以明确原因被拒绝，因为节点无法确认对端进程。通过 HTTPS 访问其他主机上的后端不受影响。
- 安装、shell、沙箱与安全页面、各 README 以及 `SECURITY.md` 以中英文描述 Windows 版本。

### 分发
- 每个版本均发布到 npm。`@agentnetwork/anet` 是适用于 Node.js 18 或更高版本的启动器。在某一版本首次运行之前，它校验发布清单的签名以及归档与二进制文件的 SHA-256 摘要，并从用户的缓存目录运行该二进制文件。各版本的构建位于 `@agentnetwork/anet-<os>-<cpu>` 与 `@agentnetwork/anet-shell-<os>-<cpu>` 包中，Windows 对应 `win32`。
- `install.sh` 与 `anet update` 在项目主机之后，通过 npmmirror、jsDelivr 与 unpkg 尝试该构建的 npm 包。无论由哪台主机提供，每个文件都按已签名的清单进行校验。
- `anet` 插件由同一来源为 Claude Code、OpenAI Codex、Cursor、CodeBuddy、opencode、DeepSeek Harness、Trae、Google Antigravity、Gemini CLI 与 ZCode 生成。目录与归档由 `https://agentnetwork.org.cn/plugins/` 与 `https://agentnetwork.org.cn/zcode/` 提供。Antigravity 与 Gemini CLI 从镜像仓库 `ANetResearch/anet-plugins` 安装，opencode 从 npm 仓库安装 `@agentnetwork/opencode-anet`。新增的指南介绍 CodeBuddy、DeepSeek Harness、Trae、Antigravity、Gemini CLI 与 ZCode。
- 插件 0.1.3 版说明 `anet` 可在 Windows 上运行。插件的启动脚本为 POSIX shell 脚本，仅在 Linux 与 macOS 上运行；在 Windows 上，由 `anet agents wire` 注册节点。

### 命令行
- `anet init --shell-controller <AID>` 使该 AID 成为 `shell` 模块唯一的调用方，并将其加入 `peers.allow`；需在终端上确认一次，且须在没有守护进程运行时执行。该命令需要含 `shell` 模块的版本。
- `anet hub-register <url> --passphrase-later` 加入 Hub 时不询问恢复口令，口令之后通过 `anet identity protect` 设置。该选项不能与 `--recovery-file` 或 `--new-recovery` 同时使用。
- `anet update --verify-dir <dir>` 不下载也不写入任何内容。它使用编译进二进制文件的发布密钥校验该目录中的发布清单，并核对正在运行的二进制文件是否为该版本针对其变体与平台的构建：SHA-256 摘要、大小、版本和模块。指定 `--installed <version>` 时，早于该版本的发布将被拒绝。`install.ps1` 使用该命令核对即将安装的二进制文件。

### 修复
- `anet doctor` 仅在恢复文件可被其他账户读取时才在 `identity.recovery` 下报告该文件。权限为 0400 的恢复文件不再被报告。
- `identity.recovery_perm` 的语句给出本平台使恢复文件变为私有的命令：在 Linux 与 macOS 上为 `chmod 600`，在 Windows 上为一条 `icacls` 命令。代码不变。
- 除上述两点、`anet update` 的下载主机以及现在同时给出 Windows 命令的语句之外，Linux 与 macOS 上的行为不变。

## 已知限制

- Windows 版本与 Linux、macOS 版本的差异列于 https://docs.agentnetwork.org.cn/docs/zh-cn/latest/reference/known-limitations.html#reference-known-limitations-windows ：自动回复没有沙箱；本地后端、`p2p` 模块与 ANetLink 套接字被拒绝；节点仅在用户登录期间运行；自动回复拒绝以 `.cmd` 或 `.bat` 结尾的智能体启动脚本；私有访问控制列表不带强制完整性标签；插件启动脚本无法运行；二进制文件不带 Authenticode 签名，因此可能被智能应用控制或终端策略阻止。
- Windows 版本经单元测试验证，并在持续集成的 `windows-latest` 运行器上，经移植所涉及各包的 Windows 测试以及 `install.ps1` 在 Windows PowerShell 5.1 与 PowerShell 7 中的测试验证。尚未在实体 Windows 计算机上测试。在实体 Windows 计算机上的端到端安装将于发布后验证。
- 各集成指南的步骤未在 Windows 上实际运行。各指南标注了未经测试的要点。
