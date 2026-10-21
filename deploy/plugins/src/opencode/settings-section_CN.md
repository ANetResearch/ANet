## 安装与设置

OpenCode 没有插件市场。插件是一个在 `opencode.json` 的 `plugin` 列表中列出的包：OpenCode 自行从 npm 安装包，目录则按原样加载。OpenCode 启动时，插件会注册 MCP 服务、skill 与命令。

将该包安装到全局配置（`~/.config/opencode/opencode.json`）。省略 `--global` 则只对当前项目安装：

```sh
opencode plugin @agentnetwork/opencode-anet --global
```

本插件的目录（例如 ANet 仓库副本中的 `deploy/plugins/dist/opencode`）用同样的方式安装，以其绝对路径代替包名。若要手工添加插件，或要设置下列选项，请编辑该文件中的条目；条目的选项即为设置项：

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugin": [
    ["@agentnetwork/opencode-anet", { "anetPath": "/usr/local/bin/anet", "identity": "work" }]
  ]
}
```

OpenCode 在启动时读取配置：修改后须重启。若已存在 `mcp.anet` 条目（例如 `anet agents wire opencode` 写入的条目），插件不会改动它；此时选项只作用于 skill 与命令。运行 `anet agents unwire opencode` 后，MCP 服务由插件提供。

| 选项 | 默认值 | 含义 |
| --- | --- | --- |
| `identity` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| `anetPath` | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |
