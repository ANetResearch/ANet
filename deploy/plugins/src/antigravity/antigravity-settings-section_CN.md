## 设置项

Antigravity 没有插件设置界面，因此本插件没有自己的设置项。下列两项为可选项，从启动插件的进程的环境变量中读取。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

运行脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。请在启动 Antigravity 的环境中导出这些变量，修改后需新建会话。默认值无需配置：运行脚本能找到由安装 skill 安装的节点。各 skill 在智能体的 shell 中运行插件的运行脚本（`bin/anet-run.sh`，Windows 上为 `bin/anet-run.cmd`），读取同样的两个变量。

MCP 服务在 `mcp_config.json` 中声明为 `node ${PLUGIN_ROOT}/bin/anet-mcp.cjs`：Antigravity 只在 MCP 服务的参数中展开插件目录，不在命令中展开，并在未安装 Node.js 时自带 Node.js。该脚本从其所在目录启动启动脚本：在 Linux 和 macOS 上用 `/bin/sh` 启动 `bin/anet-mcp.sh`，在 Windows 上用 `cmd.exe` 启动 `bin/anet-mcp.cmd`。它从不运行工作区或工作目录中的文件。本插件不会读取或修改 Antigravity 的配置。

**验证状态。** 清单与目录布局依据 Antigravity 的公开文档以及 `agy` 程序对 MCP 的处理方式；脚本已在 Linux 上运行，Windows 脚本已用 PowerShell 测试。由于 Antigravity 需要 Google 账号，该插件尚未在 Antigravity 中实际加载，因此宿主启动 MCP 服务以及发现 skill 的过程均未经验证。
