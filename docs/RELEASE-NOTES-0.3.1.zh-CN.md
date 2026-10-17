# ANet 0.3.1 发布说明

[English](RELEASE-NOTES-0.3.1.md)

发布日期：2026-10-17。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.0（未变更） | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.1 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.1 | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变更） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

## 升级

0.3.1 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.0 的节点可与 0.3.1 的 Hub 协同工作，反之亦然。节点使用 `anet update` 更新；Hub 替换二进制文件并保留数据目录即可升级。

## 变更

### macOS 沙箱
- macOS 上的 Seatbelt 沙箱已启用。在 0.3.0 中，其自检在 macOS 14 与 macOS 15 上中止：在该 profile 下启动的程序会读取根目录本身，而 profile 未允许这一次读取。现在 profile 允许该读取，且仅限目录本身，不包括其下的目录树。profile 仍然默认拒绝。
- 不受信任对端的任务，以及 `trusted` 为 `sandbox` 时的所有任务，在 macOS 上于沙箱中运行。文件隔离、带密钥的出口端点以及对钥匙串的拒绝，已在持续集成中于 macOS 14 与 macOS 15 上验证。
- `claude` Agent 将其临时文件保存在本次运行的临时目录中。

### 联邦与目录
- 对不久前加入另一个 Hub 的接收方进行首次查找时，不再以 `recipient_unknown` 失败。Hub 缓存给出空答复后，随即进行一次绕过缓存的查询。
- 另一个 Hub 为其自身账本签发的支付回执，按该 Hub 的密钥历史验证。签发方未知或不匹配的回执将被拒绝。

### 节点行为
- 加入 Hub 的两次尝试之间的等待时间最长增长到一分钟。节点等待准入期间，`anet status` 会标明下一次尝试的时间。
- `anet status` 不再将已停用的身份报告为已注册。
- 当本地 A2A 接口记录的端口仅被某个出站连接占用时，接口最多等待五分钟，随后在该端口上启动。
- `--max-auto-replies` 与 `--poll-interval` 现以 `cli_removed` 及替代说明被拒绝，而不再提示 `unknown flag`。
- `anet agents wire` 的帮助文本只列出该命令支持的工具。
- 原因表中的语句改为以节点或 Hub 为主语，不再称呼读者。

### ANetHub
- `anet-hub admission` 可在首次启动之前设置准入模式（`--init`）。

### 文档与验证
- 升级指南说明 `auto_reply.api_base` 必须使用 https 或回环地址上的 http，并列出已移除的自动回复参数。
- 场景、集群和联合 shell 测试按 0.3 流程运行，并作为持续集成的阻塞项。0.3.0 中暂缓的模块测试现默认运行。

## 已知限制

- 在 macOS 上，沙箱中的任务可以读取根目录的列表（顶层名称），但不能读取其下的目录树。
- 当缓存的答复早于五秒时，对未知接收方的查找可能多一次对对端 Hub 的查询。
