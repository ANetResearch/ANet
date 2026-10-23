# ANet 0.3.9 发布说明

[English](RELEASE-NOTES-0.3.9.md)

发布日期：2026-10-23。

| 组件 | 版本 | 仓库 |
|---|---|---|
| ANetCore | v0.16.2 | https://github.com/ANetResearch/ANetCore |
| ANet（`anet`） | v0.3.9 | https://github.com/ANetResearch/ANet |
| ANetHub（`anet-hub`） | v0.3.3（未变） | https://github.com/ANetResearch/ANetHub |
| ANetLink | v0.2.0（未变） | https://github.com/ANetResearch/ANetLink |

文档：https://docs.agentnetwork.org.cn/docs/

## 升级

0.3.9 是补丁版本，不改变线路格式、身份格式和数据格式。0.3.8 节点与 0.3.9 节点可以协同工作，hub 无需升级。用 `anet update` 更新节点并重启守护进程即可。所有新设置默认关闭；不使用它们的节点，行为与 0.3.8 相同。ANetCore v0.16.2 在线路注册表中加入了新的元数据键和原因码。

## 变更

### 组织运行在 agent 之中

节点现在可以让一个 agent 应答本地服务（例如一个组织）的成员发来的文本任务，并以发送该任务的成员的权限行事。以下四项设置共同实现这一点；每一项都是通用的，不针对任何具体服务。

- **由本地服务准入**（`inbound.admit`）。对不在任何名单上的对端发来的文本任务，节点询问自身服务模块中一个标记为 `"internal": true` 的能力是否准入。回答 `{"admit": true}` 即准入；其他任何回答、出错或 5 秒内无回答均不准入。回答按对端缓存（`cache_seconds`，默认 30，最大 300），并在每一轮重新询问，因此服务不再准入的对端，从其下一条消息起即不再获得服务。deny 名单始终优先。内部能力从不公开，也无法从网络调用。`auto_reply.admitted`（`off`、`direct` 或 `sandbox`）决定 exec 后端如何为已准入的对端提供服务。命令：`anet inbound admit set <capability>`、`anet inbound admit off`、`anet autoreply set --admitted direct|sandbox|off`（`direct` 需在终端上确认）。
- **以另一个本地账户运行 agent**（`anet runner serve`、`auto_reply.runner`）。运行器（runner）以另一个 Unix 账户运行直接运行的 agent，使 agent 始终无法读取节点的身份密钥、控制令牌和数据目录。节点通过 Unix 套接字连接运行器，并按对端凭据和服务套接字的规则进行检查；每个连接一个作业，关闭连接即取消该作业。`auto_reply.runner.max_jobs`（默认 4）限制同时进行的运行数；每个对端同一时间只有一个运行。支持 Linux 和 macOS。
- **模型密钥留在节点**（`auto_reply.model_proxy`）。节点的回环代理把 agent 的模型请求转发到具名上游，并附上上游的密钥；每次运行只持有一个仅在运行期间有效的随机密钥。在 Linux 上，来自其他本地账户的连接会被拒绝。`auto_reply.admitted_budget` 限制已准入对端每天、每个对端的运行次数，以及每次运行的模型请求数；超出每日预算的运行在启动前即被拒绝。单个请求的输出上限为上游的 `max_output_tokens`（默认 32768）；沙箱运行仍为 8192。
- **回合工具**（`anet turn-mcp`）。受信任或已准入对端的直接运行，可以调用服务模块中标记为 `"agent_tool": true` 的能力，调用身份始终是该对端（`X-ANet-Caller`），并标记 `X-ANet-Via: agent`。`anet turn-mcp` 是提供这些工具的 stdio MCP 服务器，无需数据目录，读取 `ANET_TURN_URL` 和 `ANET_TURN_TOKEN`。

参见新增的操作指南“以另一个账户运行 agent”以及主题“委派与自动回复”。

### `anet uninstall`

`anet uninstall` 停止节点，并删除安装程序创建的内容：二进制文件及其发布记录、指向它的链接、shell 配置文件中带标记的 `PATH` 段落和 fish 配置文件；在 Windows 上，还删除 `Run` 下的 `AgentNetwork` 值以及用户 `PATH` 中的安装目录。不属于已安装 anet 的文件和链接会保留。除非指定 `--delete-identity`，否则保留 `~/.anet`；该选项始终需要在终端上确认，并列出相应的 AID。已接入的编程 agent 以 `anet agents unwire <name>` 的形式列出；通过 npm 安装时，提示使用 `npm uninstall -g @agentnetwork/anet`。

### 下载源

`install.sh`、`install.ps1` 和 `anet update` 遇到发布清单验证不通过的下载源时会跳过并尝试下一个，只有在没有任何下载源通过验证时才失败。jsDelivr 按版本读取，不会混用两个版本的文件。

### 官方 agent

USTC 组织的官方条目不再列出 `ask` 能力：问题和任务以文本任务的形式发送给组织，由其 agent 应答。指南中已相应说明。
