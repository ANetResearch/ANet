# ANet 0.3.8 发布说明

[English](RELEASE-NOTES-0.3.8.md)

发布日期：尚未发布。本文件在 0.3.8 准备期间汇总各项变更。

文档：https://docs.agentnetwork.org.cn/docs/

## 变更

### MCP 服务启动节点
- 当所选身份没有守护进程应答且其数据目录已设置时，`anet mcp` 会启动该身份的节点。它按 `anet up` 的方式启动守护进程（后台分离运行，执行相同的检查，不会把控制令牌发往被其他进程占用的端口），工作目录为用户主目录而不是会话的工作目录，并最多等待 20 秒。同时启动的两个会话只会启动一个守护进程。它的所有提示写入标准错误；标准输出只承载 MCP 会话。
- 节点无法运行时，`anet mcp` 不再以状态 1 退出，而是照常提供服务：列出全部工具；`guide` 照常工作；`node_status` 返回 `running: false`，附原因代码、说明语句，并在 `next` 中给出修复命令；其他工具以同一原因作为工具错误应答；服务的说明也以该原因开头。服务在每次调用时重新查找守护进程，因此节点运行后，工具在同一会话中即可使用。
- 这些原因沿用已有代码：`daemon_not_running`、`daemon_not_answering`，以及数据目录或配置被拒绝时的相应代码（例如 `config_key_removed`）。没有节点的数据目录使用新的语句键 `daemon_not_running.not_set_up`，修复命令为 `anet up`（命名身份为 `anet --id <name> up`）。`anet mcp` 自身不创建身份：它随编程 agent 的每个会话启动，插件设置中拼错的身份名不应因此创建并启动一个新身份。
- `anet mcp` 只启动用户 anet 主目录中的身份（`~/.anet` 与 `~/.anet/ids/<name>`），也不启动配置中启用了 `shell` 模块的节点。`ANET_DATA_DIR` 或 `ANET_HOME` 指向其他位置的数据目录（例如宿主转发了工作区设置的变量时，工作区中的数据目录），以及启用了 `shell` 模块的节点，需要明确运行 `anet up` 启动（语句键 `daemon_not_running.outside_home` 与 `daemon_not_running.shell_enabled`）。已在运行的守护进程在任何情况下都照常使用。
- `anet mcp` 告诉模型的内容只包含原因和命令，不包含任何路径：数据目录、拒绝所涉及的文件以及守护进程的日志只出现在其错误输出中（语句键 `daemon_not_running.mcp` 与 `daemon_not_answering.mcp`；`daemon_not_running.not_set_up` 不再给出数据目录）。
- 在 `config.json` 中关闭 `mcp` 模块时，`anet mcp` 仍以 `module_absent` 拒绝，并以状态 2 退出。
- 指南要求 agent 在回答关于 anet 本身的问题（例如“anet 能做什么”）时调用 `node_status`，展示节点的实际状态并给出具体的下一步操作；`node_status` 给出 `anet up` 时，agent 直接运行该命令。

### 编程 agent 的插件
- 插件升级到 0.3.0，即其最终版本。启用插件即可安装 anet 并运行其节点：MCP 启动器找不到 anet 可执行文件且设置项 `anetPath` 为空时，会不带参数运行官方安装程序（Linux 和 macOS 上为 `install.sh`；Windows 上用系统目录中的 Windows PowerShell 运行文档给出的 `install.ps1` 命令，Windows 上的 ZCode 同样适用）。安装程序校验已签名的发布清单和 SHA-256，为当前用户安装 anet，启动节点并加入公共 hub。
- 安装程序在用户主目录中运行，输入已关闭，输出写入 MCP 服务的错误输出。在 Linux 和 macOS 上，它在独立会话中运行，没有控制终端，因此不会在宿主的终端上提示输入；宿主因启动过慢而放弃时，安装仍会完成，日志保存在 `~/.cache/anet-plugin/install.log`。同一用户同一时间只运行一个安装（锁位于 `~/.cache/anet-plugin`，Windows 上为 `%LOCALAPPDATA%\anet-plugin`）；另一个宿主会等待其完成，不会重复安装。
- 不会运行会话工作目录中的任何程序：插件的 POSIX 脚本在运行第一个程序（`sh`、`sed`、`uname`、`curl`）之前只保留 `PATH` 中的绝对路径目录，Windows 脚本以完整路径调用每个程序；运行脚本向 Windows 查询用于运行安装程序的 PowerShell 所在的系统目录。安装锁以进程 ID 和启动时间标识持有者，被系统复用的进程 ID 不会使锁继续有效；在 Windows 上，锁标识安装程序本身，该程序在宿主结束 MCP 服务器后仍会继续运行，下一个宿主会等待其完成。
- 在工具的运行环境中设置 `ANET_PLUGIN_NO_INSTALL=1` 可关闭自动安装。设置项 `anetPath` 指向的不是可执行文件时仍报告错误，不会改为安装其他副本。只有 MCP 启动器会安装；运行脚本、skill 和命令都不会安装。
- Skill 把安装插件视为用户的同意：会话中没有 anet 工具时，agent 不再询问而直接运行设置；关于 anet 的问题依据指南和节点的实际状态回答，而不是依据插件自带的文字。
