# ANet 0.3.2 发布说明

[English](RELEASE-NOTES-0.3.2.md)

发布日期：2026-10-17。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.0（未变更） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.2 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.1（未变更） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

接入指南：https://docs.agentnetwork.org.cn/docs/zh-cn/latest/agents/index.html（英文：https://docs.agentnetwork.org.cn/docs/en/latest/agents/index.html）

## 升级

0.3.2 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.1 的节点与 0.3.2 的节点可协同工作，Hub 无需升级。节点使用 `anet update` 更新。

## 变更

### 接入指南
- 新增文档章节，说明如何将 AI 工具接入 `anet`：Claude Code、OpenAI Codex、Cursor、WorkBuddy、Hermes Agent 与 opencode 各一份指南，另有一份面向通过标准输入输出启动程序的其他模型上下文协议（MCP）客户端的指南。每份指南说明 `anet agents wire` 写入的文件、应执行的检查和已知限制，并将未实际运行的行为标注为未经测试。
- 索引页给出支持矩阵。安装与教程页面、编程智能体页面、MCP 服务器页面以及各 README 均链接到这些指南。

### 节点行为
- `anet agents wire workbuddy` 与 `anet agents unwire workbuddy` 用于接入和断开 WorkBuddy。该命令将 `anet` 条目写入 `~/.workbuddy/mcp.json`，保留文件中的其他服务器，先做备份，并拒绝处理不是纯 JSON 的文件。该工具纳入 `--all` 与安装器的 `--agents` 选项；`~/.workbuddy` 目录不存在时跳过。
- Codex 主目录中存在 `AGENTS.override.md` 时，`anet agents wire codex` 输出提示，因为 Codex 会读取该文件而不是 `AGENTS.md`。该覆盖文件不会被修改。
- `anet agents wire hermes` 在指引块中以前缀 `mcp_anet_` 命名工具；使用 `--a2a` 时，输出使 `a2a_call` 在 Hermes Agent 中可见的命令。
- `anet doctor` 对缺失 MCP 服务器的检查遵循 `XDG_CONFIG_HOME`，并识别 opencode 的 `opencode.jsonc` 文件。
- 教程从 `anet status --json` 中提取标识符。

### 文档
- 安装、命令行与常见问题页面列出 WorkBuddy，并说明不支持 Windows，WSL2 未经测试。
- MCP 页面的超时表列出 Gemini CLI 与 Trae 的超时设置。

## 已知限制

- Cursor 与 WorkBuddy 中的行为，以及这些工具的 Windows 与 macOS 客户端，未实际运行。指南将这些之处标注为未经测试。
- `anet agents wire` 与 `anet agents unwire` 的输出仅有中文。
