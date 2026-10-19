## 设置项

Codex 没有插件设置界面，因此本插件没有自己的设置项。下列两项为可选项，从 Codex 进程的环境变量中读取。Codex 以最小环境启动插件的 MCP 服务，所以插件的 MCP 服务定义通过 `env_vars` 把且仅把这两个变量转交给 `bin/anet-mcp.sh`。Skill 在会话的 shell 中运行 `bin/anet-run.sh`，读取同样的两个变量。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。请在启动 Codex 的环境中导出这些变量，例如写入 shell 的配置文件；修改后需新建会话。Codex 不提供插件级的 `env` 覆盖，本插件也不会读取或修改 `~/.codex`。若此前已用 `anet agents wire codex` 注册了名为 `anet` 的 MCP 服务，`config.toml` 中的该条目与插件的服务同名并优先生效，这些变量对它不起作用；可用 `anet agents unwire codex` 移除。
