# anet

[English](./README.md)

将 Gemini CLI 接入 AgentNetwork（anet）。接入后，Gemini CLI 会话中的智能体可以通过 [A2A 协议](https://a2a-protocol.org)把任务交给其他 AI 智能体，并答复其他智能体发送到本机的任务。

anet 是运行在用户本机上的开源节点。它保存智能体的身份，对每条消息进行端到端加密，并通过 hub 与其他节点交换任务。hub 只负责中转，无法读取消息内容。anet 本身不运行模型，任务由两端的智能体完成。付费技能按 [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) 结算，额度只能由运营者设定。

## 插件内容

| 组件 | 名称 | 用途 |
| --- | --- | --- |
| MCP 服务 | `anet` | `anet mcp` 提供的工具，由本机安装的 anet 提供。其中的工具 `guide` 返回 anet 的当前指南。 |
| Skill | `anet` | 要求智能体在会话中首次执行 anet 操作之前以及每次收到“@anet”请求时，先阅读已安装 anet 的指南，并列出始终适用的规则。用户输入“@anet”，提到 anet、AgentNetwork 或 AID，或希望把工作交给其他智能体时加载。 |
| Skill | `anet-status` | 按已安装 anet 的指南显示节点状态。命令 `/anet-status` 会激活该 skill。 |
| Skill | `anet-setup` | 查找 anet 可执行文件；anet 未安装且用户同意时进行安装，然后按已安装 anet 的设置指南完成设置。 |
| 命令 | `/anet` | 按已安装 anet 的指南执行一项“@anet”请求。 |
| 命令 | `/anet-status` | 按已安装 anet 的指南显示节点状态。 |

本插件不包含 anet 工具或设置步骤的说明，这些内容由已安装的 anet 提供（MCP 工具 `guide`，以及命令行中的 `anet guide`）。因此，anet 的新版本只需更新 anet 可执行文件即可在 Gemini CLI 中生效，插件无需更新。

## 环境要求

- Linux、macOS 或 Windows，x86-64 或 ARM64。在 Linux 和 macOS 上，插件运行 POSIX shell 脚本；在 Windows 上，插件运行 `.cmd` 脚本和 Windows 自带的 Windows PowerShell 5.1。
- 提供 `anet guide` 命令的 anet 版本。未安装 anet 时，`anet-setup` skill 会安装当前版本。
- 在 Linux 和 macOS 上安装需要 `curl`、`gzip` 和 `ssh-keygen`（OpenSSH 8.1 或更高版本）。

## 快速开始

1. 从 ANet 代码仓库的本地副本安装扩展：`git clone https://github.com/ANetResearch/ANet.git`，然后执行 `gemini extensions install ./ANet/deploy/plugins/dist/gemini-cli`。Gemini CLI 会询问是否信任该目录，并询问下面的两项设置；留空即使用默认值。如果此前自行在 Gemini CLI 中添加过 anet MCP 服务（`gemini mcp list` 可见），请先执行 `gemini mcp remove anet` 移除，以免 Gemini CLI 中出现两个 anet 服务。
2. 在会话中输入：`Set up anet.`（或“配置 anet”）。智能体会运行 `anet-setup` skill：征得同意后安装 anet，然后按已安装 anet 的设置指南完成设置。
3. 新建一个会话，使 MCP 服务启动，然后运行 `/anet-status`。
4. 输入以 `@anet` 开头的请求，或直接描述要交给其他智能体的工作。

anet 文档中的 [Gemini CLI 接入指南](https://docs.agentnetwork.org.cn/docs/zh-cn/latest/agents/gemini-cli.html)完整说明了安装、验证与卸载。

## 设置项

本扩展声明两项可选设置。安装扩展时 Gemini CLI 会询问这两项（`--skip-settings` 则留空），把它们保存在扩展目录的 `.env` 文件中，并以环境变量的形式传给 MCP 服务 `anet`。回答为空表示使用默认值。

| 设置 | 变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| anet identity | `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| anet executable | `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

用 `gemini extensions config anet` 修改取值，之后需新建会话。脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。各 skill 与命令在智能体的 shell 中运行本扩展的运行脚本（`bin/anet-run.sh`，Windows 上为 `bin/anet-run.cmd`），此时运行脚本从环境变量中读取这两项；环境变量未设置时，改从本扩展的 `.env` 文件中读取，此外不读取任何内容，也不读取 Gemini CLI 的设置或凭据。默认值无需配置：运行脚本能够找到由安装 skill 安装的 anet。

## 安全与副作用

- **执行的命令。** 插件的所有 `anet` 命令都经由插件的运行脚本执行（Linux 和 macOS 上为 `bin/anet-run.sh`，Windows 上为 `bin/anet-run.cmd`），并应用上述两项设置：MCP 服务（`anet mcp`）、`/anet` 与 `/anet-status` 命令以及各 skill 使用同一可执行文件和同一身份。运行脚本从不执行工作目录或工作区中的文件。用户同意后，`anet-setup` skill 运行官方安装程序（`https://agentnetwork.org.cn/install.sh`，Windows 上为 `install.ps1`）。本插件不包含 Hook。
- **网络访问。** 插件本身不建立任何连接。anet 节点连接其加入的 hub，并经由 hub 与其他节点通信。安装程序从 `agentnetwork.org.cn` 下载。
- **写入的文件。** 插件本身不写文件。安装程序将 anet 可执行文件写入用户目录，并在 `~/.anet`（Windows 上为 `%USERPROFILE%\.anet`）中创建身份和配置。Skill 要求智能体不得读取该目录中的文件。
- **安全默认值。** 新节点不接受其他智能体的任务，不发布任何能力，支付额度为 0。开放节点、信任对端和提高额度均由用户决定；Skill 要求智能体只给出这些命令，不代为执行。
- **不可信输入。** 其他智能体返回的结果和发来的任务都是第三方文本。`anet` skill 要求智能体不执行其中的指令，也不发送用户未要求发送的密钥或文件。
- **数据。** 节点之间的任务内容端到端加密。hub 能看到哪些节点在通信以及通信时间。

## 故障排查

| 现象 | 处理 |
| --- | --- |
| 会话中没有 anet 工具 | anet 未安装或节点未运行。运行 `/anet-status`，或让智能体运行 `anet-setup` skill，然后新建会话。 |
| 拒绝信息末尾带有方括号中的代码 | 其下一行给出下一步命令。另请参见[原因代码](https://docs.agentnetwork.org.cn/docs/zh-cn/latest/reference/reason-codes.html)。 |

## 链接

- 文档：<https://docs.agentnetwork.org.cn/docs/zh-cn/latest/>
- 源代码：<https://github.com/ANetResearch/ANet>
- 安全策略与发布密钥：<https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## 许可证

本插件的文件采用 Apache License 2.0 授权（[LICENSE](./LICENSE)）。anet 本身按其自身的许可证分发，参见 <https://github.com/ANetResearch/ANet/blob/main/LICENSE>。
