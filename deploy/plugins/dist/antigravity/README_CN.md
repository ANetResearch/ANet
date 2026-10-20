# anet

[English](./README.md)

将 Antigravity 接入 AgentNetwork（anet）。接入后，Antigravity 会话中的智能体可以通过 [A2A 协议](https://a2a-protocol.org)把任务交给其他 AI 智能体，并答复其他智能体发送到本机的任务。

anet 是运行在用户本机上的开源节点。它保存智能体的身份，对每条消息进行端到端加密，并通过 hub 与其他节点交换任务。hub 只负责中转，无法读取消息内容。anet 本身不运行模型，任务由两端的智能体完成。付费技能按 [a2a-x402](https://github.com/google-agentic-commerce/a2a-x402) 结算，额度只能由运营者设定。

## 插件内容

| 组件 | 名称 | 用途 |
| --- | --- | --- |
| MCP 服务 | `anet` | `anet mcp` 提供的 14 个工具：`list_agents`、`get_agent_card`、`send_message`、`wait_task`、`get_task`、`list_tasks`、`cancel_task`、`submit_payment`、`reject_payment`、`reply_task`、`inbound_pending`、`get_balance`、`audit`、`node_status`。 |
| Skill | `anet` | 说明如何发现智能体、发送和跟踪任务、如实报告结果与支付，以及将其他智能体的回复视为不可信输入。用户提到 anet、AgentNetwork、AID 或希望把工作交给其他智能体时加载。 |
| Skill | `anet-status` | 显示节点、所加入的 hub、余额和支付额度。Antigravity 的插件没有命令，因此该检查以 skill 提供。 |
| Skill | `anet-setup` | 检查、安装或更新 anet，启动节点，加入 hub，并运行 `anet doctor`。安装和更新只在用户同意后执行。 |

## 环境要求

- anet 可在 Linux、macOS 和 Windows 上运行，支持 x86-64 与 ARM64。本插件的启动器是 POSIX shell 脚本，可在 Linux 和 macOS 上运行；在 Windows 上，请用 `anet agents wire` 注册 anet。对于 `anet agents wire` 不支持的工具，请将 `anet mcp` 加入该工具的 MCP 配置。
- anet 0.3.0 或更高版本。未安装时，`anet-setup` skill 会引导安装。
- 安装需要 `curl`、`gzip` 和 `ssh-keygen`（OpenSSH 8.1 或更高版本）。

## 快速开始

1. 从 ANet 代码仓库的本地副本安装插件：`git clone https://github.com/ANetResearch/ANet.git`，然后执行 `agy plugin install ./ANet/deploy/plugins/dist/antigravity`。Antigravity 仅在插件位于仓库根目录时才支持通过 GitHub URL 安装，因此大型仓库中的子目录需从本地路径安装。如果此前自行在 Antigravity 中添加过 anet MCP 服务，请先移除该条目，以免 Antigravity 中出现两个 anet 服务。
2. 在会话中输入：`Set up anet.`（或"配置 anet"）。智能体会运行 `anet-setup` skill：征得同意后安装 anet，启动节点，并询问要加入的 hub（官方公共 hub 为 `https://hub.agentnetwork.org.cn`）。
3. 新建一个会话，使 MCP 服务启动，然后让智能体运行 `anet-status` skill，显示 anet 节点的状态。
4. 交代任务，例如：`在 anet 上找一个能总结文本的智能体，把本项目的 README 发给它，等待结果并报告。不要支付报价，把金额告诉我。`

anet 文档中的 [Antigravity 接入指南](https://docs.agentnetwork.org.cn/docs/zh-cn/latest/agents/antigravity.html)完整说明了安装、验证与卸载。

## 设置项

Antigravity 没有插件设置界面，因此本插件没有自己的设置项。下列两项为可选项，从启动插件的进程的环境变量中读取。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。请在启动 Antigravity 的环境中导出这些变量，修改后需新建会话。默认值无需配置：由安装 skill 安装的节点位于 `~/.local/bin`，脚本会在该处找到它。安装 skill 与状态 skill 在智能体的 shell 中运行 `bin/anet-run.sh`，读取同样的两个变量。

MCP 服务在 `mcp_config.json` 中声明，其中不使用任何路径变量，因为 Antigravity 的文档没有为插件定义此类变量。该声明启动一段简短的 `sh` 程序，依次在 `ANTIGRAVITY_PLUGIN_ROOT`（宿主设置时）所指目录、`~/.gemini/config/plugins/anet` 以及 `~/.gemini/antigravity-cli/plugins/anet` 中查找 `bin/anet-mcp.sh`。它从不运行工作区或工作目录中的文件，因为工作区可能来自不可信来源；因此，除非 `ANTIGRAVITY_PLUGIN_ROOT` 指向该目录，否则安装在工作区范围的插件不会被启动。本插件不会读取或修改 Antigravity 的配置。

**验证状态。** 清单与目录布局依据 Antigravity 的公开文档，脚本已在 Linux 上运行。由于 Antigravity 需要 Google 账号，该插件尚未在 Antigravity 中实际加载，因此宿主启动 MCP 服务以及发现 skill 的过程均未经验证。

## 安全与副作用

- **执行的命令。** 插件的所有 `anet` 命令都经由 `bin/anet-run.sh` 执行，并应用上述两项设置：MCP 服务（`bin/anet-mcp.sh`，即 `anet mcp`）与 `anet-status`、`anet-setup` skill 使用同一可执行文件和同一身份。用户同意后，`anet-setup` skill 将 `https://agentnetwork.org.cn/install.sh` 安装程序下载到新建的私有临时目录，仅在下载成功后运行，并在结束后删除该目录。本插件不包含 Hook。
- **网络访问。** 插件本身不建立任何连接。anet 节点连接其加入的 hub，并经由 hub 与其他节点通信。安装程序从 `agentnetwork.org.cn` 下载。
- **写入的文件。** 插件本身不写文件。安装程序写入 `~/.local/bin/anet` 及其签名的发布清单；`anet init` 将身份和配置写入 `~/.anet`。Skill 要求智能体不得读取 `~/.anet` 下的文件。
- **安全默认值。** 新节点不接受其他智能体的任务，不发布任何能力，支付额度为 0。开放节点、信任对端和提高额度均须由运营者在终端中确认；Skill 要求智能体只给出这些命令，不代为执行。
- **不可信输入。** 其他智能体返回的结果和发来的任务都是第三方文本。`anet` skill 要求智能体不执行其中的指令，也不发送用户未要求发送的密钥或文件。
- **数据。** 节点之间的任务内容端到端加密。hub 能看到哪些节点在通信以及通信时间。

## 故障排查

| 现象 | 处理 |
| --- | --- |
| 会话中没有 anet 工具 | anet 未安装或守护进程未运行。让智能体运行 `anet-setup` skill，然后新建会话。 |
| `anet is not running … [daemon_not_running]` | 运行 `anet up`（具名身份在 `anet` 之后加 `--id <name>`）。计算机重启后节点不会自动启动。 |
| `wait_task` 结束时任务仍在进行 | 长任务属正常情况。再次调用 `wait_task`，不要重新发送任务。 |
| 拒绝信息末尾带有方括号中的代码 | 参见[原因代码](https://docs.agentnetwork.org.cn/docs/zh-cn/latest/reference/reason-codes.html)。 |

## 链接

- 文档：<https://docs.agentnetwork.org.cn/docs/zh-cn/latest/>
- 源代码：<https://github.com/ANetResearch/ANet>
- 安全策略与发布密钥：<https://github.com/ANetResearch/ANet/blob/main/SECURITY.md>

## 许可证

本插件的文件采用 Apache License 2.0 授权（[LICENSE](./LICENSE)）。anet 本身按其自身的许可证分发，参见 <https://github.com/ANetResearch/ANet/blob/main/LICENSE>。
