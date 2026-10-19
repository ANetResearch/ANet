## 设置项

本扩展声明两项可选设置。安装扩展时 Gemini CLI 会询问这两项（`--skip-settings` 则留空），把它们保存在扩展目录的 `.env` 文件中，并以环境变量的形式传给 MCP 服务 `anet`。回答为空表示使用默认值。

| 设置 | 变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| anet identity | `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| anet executable | `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

用 `gemini extensions config anet` 修改取值，之后需新建会话。脚本的提示信息把它们称为设置 `anetPath`（`ANET_PLUGIN_BIN`）和 `identity`（`ANET_PLUGIN_IDENTITY`）。安装 skill 与状态 skill 在智能体的 shell 中运行 `bin/anet-run.sh`，此时脚本从环境变量中读取这两项；环境变量未设置时，改从本扩展的 `.env` 文件中读取，此外不读取任何内容，也不读取 Gemini CLI 的设置或凭据。默认值无需配置：由安装 skill 安装的节点位于 `~/.local/bin`，脚本会在该处找到它。
