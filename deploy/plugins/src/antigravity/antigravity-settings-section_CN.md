## 设置项

Antigravity 没有插件设置界面，因此本插件没有自己的设置项。下列两项为可选项，从启动插件的进程的环境变量中读取。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。请在启动 Antigravity 的环境中导出这些变量，修改后需新建会话。默认值无需配置：由安装 skill 安装的节点位于 `~/.local/bin`，脚本会在该处找到它。安装 skill 与状态 skill 在智能体的 shell 中运行 `bin/anet-run.sh`，读取同样的两个变量。

MCP 服务在 `mcp_config.json` 中声明，其中不使用任何路径变量，因为 Antigravity 的文档没有为插件定义此类变量。该声明启动一段简短的 `sh` 程序，依次在 `ANTIGRAVITY_PLUGIN_ROOT`（宿主设置时）所指目录、`~/.gemini/config/plugins/anet` 以及 `~/.gemini/antigravity-cli/plugins/anet` 中查找 `bin/anet-mcp.sh`。它从不运行工作区或工作目录中的文件，因为工作区可能来自不可信来源；因此，除非 `ANTIGRAVITY_PLUGIN_ROOT` 指向该目录，否则安装在工作区范围的插件不会被启动。本插件不会读取或修改 Antigravity 的配置。

**验证状态。** 清单与目录布局依据 Antigravity 的公开文档，脚本已在 Linux 上运行。由于 Antigravity 需要 Google 账号，该插件尚未在 Antigravity 中实际加载，因此宿主启动 MCP 服务以及发现 skill 的过程均未经验证。
