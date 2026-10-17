# ANet 0.3.3 发布说明

[English](RELEASE-NOTES-0.3.3.md)

发布日期：2026-10-17。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.1 | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.3 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3 | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

原因代码：https://docs.agentnetwork.org.cn/docs/zh-cn/latest/reference/reason-codes.html

## 升级

0.3.3 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.2 的节点与 0.3.3 的节点可协同工作。节点使用 `anet update` 更新；Hub 原地升级并保留其数据。ANetHub 跳过版本号 0.3.2，使 `anet` 与 ANetHub 的版本再次一致。

有一项行为变化：所有系统上的输出均为英文。0.3.3 之前，当 `LC_ALL`、`LC_MESSAGES` 或 `LANG` 以 `zh` 开头时，`anet` 与 `anet-hub` 输出中文。自 0.3.3 起，仅 `ANET_LANG` 可选择中文（以 `zh` 开头的取值，例如 `ANET_LANG=zh-CN`），不再读取区域设置变量。按中文语句匹配的脚本需要设置该变量，或改为匹配英文语句或原因代码。

## 变更

### 语言
- `anet` 与 `anet-hub` 输出的每一条语句，以及交给模型的每一段文本，默认均为英文。中文须通过 `ANET_LANG` 选择。
- exec 后端的提示词、自动回复的自检提示词和审计结论在两种语言下均为英文。Hub 的管理界面以英文应答。

### 智能体接入
- `anet agents`、`anet agents wire` 与 `anet agents unwire` 的输出来自原因表，默认英文，在 `ANET_LANG=zh-CN` 下为中文。`wire` 为各工具写入的技能、人设和指南文件为英文。退出状态不变。
- 这些命令的拒绝新增 22 个以 `wire_` 为前缀的代码，例如 `wire_unknown_tool`、`wire_conflict` 与 `wire_io_failed`。
- `anet agents unwire` 在无法解析 JSON 文件时，不再输出需手动添加的条目；它指明该文件并保持其不变。

### 原因代码
- 命令行与 MCP 工具在每一条拒绝语句之后显示原因代码，例如 `... send it again in 2 minutes. [busy]`。Hub 以其自有代码返回的拒绝显示为 `[hub_refused:<hub 代码>]`。JSON 输出不变。
- 原因代码作为稳定接口管理。登记表 `internal/a2ashape/codes.txt`（ANetHub 中为 `internal/wire3/codes.txt`）列出每个已发布的代码及其首次发出的版本，并由测试与原因表核对。代码的含义永不改变，代码永不改名或复用。原因表页面新增“Since”列、每个代码的锚点和已停用代码表；本版本未停用任何代码。

### 修复
- 当预览帧在事件流启动期间到达时，任务事件流不再重复发送该帧。

### ANetCore v0.16.1
- 线路注册表登记 22 个 `wire_` 代码；`coredet` 包的注释改为英文。行为不变。

## 已知限制

- 在 Cursor 与 WorkBuddy 中的行为，以及各工具在 Windows 与 macOS 客户端上的行为，未实际运行。各指南将这些要点标注为未经测试。
