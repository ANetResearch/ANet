## 设置项

本插件声明了两个可选变量。Cursor 会把它们的值作为同名环境变量传给 MCP 服务 `anet`。未设置值的变量为空，空值表示使用默认行为。

| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| `ANET_PLUGIN_IDENTITY` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| `ANET_PLUGIN_BIN` | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

Cursor 从插件配置中取得插件变量的值，该配置由团队管理员在 Cursor 控制台中设置。Cursor 没有面向单个用户的插件设置文件，本插件也不读取 Cursor 的配置。各 skill 与命令在智能体的 shell 中运行本插件的运行脚本（`bin/anet-run.sh`，Windows 上为 `bin/anet-run.cmd`），运行脚本从此 shell 的环境变量中读取同名的两个变量。如需使用非默认值，请在两处同时设置。默认值无需任何配置：运行脚本能够找到由安装 skill 安装的 anet。
