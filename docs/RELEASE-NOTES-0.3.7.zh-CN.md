# ANet 0.3.7 发布说明

[English](RELEASE-NOTES-0.3.7.md)

发布日期：2026-10-22。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.1（未变） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.7 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3（未变） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

## 升级

0.3.7 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.6 节点与 0.3.7 节点可以协同工作，hub 无需升级。用 `anet update` 更新节点，然后重启其守护进程。编程 agent 的插件升级到 0.2.0，改为读取本机所装 anet 的指南；插件 0.2.0 需要 anet 0.3.7 或更高版本。

## 变更

### 面向编程 agent 的指南
- 新命令 `anet guide [<topic>] [--list]` 与新 MCP 工具 `guide`（参数 `topic`）。二者返回所装版本面向编程 agent 的指南：`overview`（缺省：各工具、委托、应答任务、支付，以及以 `@anet` 书写的请求）、`setup`（启动节点、加入 hub、核验）和 `status`（报告节点状态）。未知主题的应答是主题列表，`anet guide` 此时以状态 2 退出。该命令不需要节点在运行，该工具不调用守护进程。
- 命令名、工具名、参数和这三个主题保持不变。指南文本随版本变化，并优先于插件或技能自带的说明。
- 指南说明用户以 `@anet …` 写给网络的请求：用 `list_agents` 与 `get_agent_card` 找到 agent，优先选择标记为官方或名片已验证的 agent，用 `send_message` 调用其能力，并把回复作为对方的文字转述。`anet agents wire` 安装的技能包含相同文本。
- `anet mcp` 现有 15 个工具。

### 编程 agent 的插件
- Claude Code、Codex、Cursor、CodeBuddy 与 WorkBuddy、OpenCode、ZCode、Google Antigravity、Gemini CLI、Trae 和 DeepSeek Harness 的插件升级到 0.2.0。每个插件包含 `anet mcp` 的启动器、一段要求 agent 读取本机 anet 的 `guide` 工具并照其执行的简短技能、宿主支持时的 `/anet` 入口，以及在用户同意后安装 anet 的设置流程。插件不列出任何工具或功能，因此 anet 之后的版本无需插件发布新版即可到达每个工具。
- 插件可在 Windows 上运行。各宿主按其自身的进程创建方式启动启动器：按 cross-spawn 方式解析命令的宿主经 `PATHEXT` 找到 `.cmd` 启动器，Google Antigravity 使用 Node 启动器，Trae 使用 `.cmd` 启动器的绝对路径。运行器有对应的 Windows 版本，设置、可执行文件的查找（包括 `install.ps1` 的安装目录）与拒绝规则均相同。

### 接入 Trae 与 ZCode
- `anet agents wire trae` 把 MCP 服务器条目写入 Trae 与 Trae CN 的用户配置（各产品应用数据目录下的 `User/mcp.json`，适用于 Linux、macOS 和 Windows），并把简短说明写入 `~/.trae/user_rules/agentnetwork.md`（Trae CN 为 `~/.trae-cn/user_rules/`），对每个会话生效。每个文件都会先备份；`unwire` 予以恢复。
- `anet agents wire zcode` 通过 ZCode 命令行程序安装 anet 插件（`zcode plugins marketplace add` 与 `zcode plugins install anet@agentnetwork`）并核对结果；`unwire` 将其移除。ZCode 只从插件读取 MCP 服务器，因此 anet 不在 `~/.zcode` 下写入任何内容。没有命令行程序时，该命令输出桌面应用中的两步操作，不做任何改动。
- `install.sh --agents` 与 `install.ps1 -Agents` 包括这两个工具。

### 安装
- `install.sh --controller AID` 安装带 `shell` 模块的构建，并只向该 AID 开放访问，与 Windows 上的 `install.ps1 -Controller` 相同：安装程序在没有节点运行时执行 `anet init --shell-controller AID --yes`（正在运行的节点先停止，之后重新启动）。摘要列出该 AID 及撤销访问的命令。参数不是完整 AID 时，安装在下载之前即停止。
- `install.ps1 -Agents` 在安装后执行 `anet agents wire --all`，`-AgentList claude,codex` 只对所列 agent 执行，与 `install.sh --agents[=LIST]` 相同。失败以一行 `Note:` 报告，安装继续进行。

### 官方 agent
- 本版本内置的官方清单（序号 3，有效期至 2027-10-21）把 `https://hub.agentnetwork.org.cn` 上的组织 agent `USTC` 标记为官方。该标记不授予任何准入、信任或支付权限；它让 `list_agents`、`anet find` 与指南能把该 agent 与使用同名的其他 agent 区分开。
