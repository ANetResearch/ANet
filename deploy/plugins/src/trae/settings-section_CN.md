## 安装与设置

Trae 没有可由代码仓库提供的插件包格式。因此本插件包通过安装脚本安装：脚本把文件复制到 Trae 的配置目录，并输出 MCP 配置。Linux 和 macOS 上使用 `install.sh`；Windows 上使用 `install.cmd`，它以 Windows PowerShell 5.1 运行 `install.ps1`。安装脚本不启动任何进程，不建立任何连接，也不读取 Trae 的任何文件。它们创建的唯一一个 Trae 文件，是尚不存在的项目级 `mcp.json`。

```sh
sh install.sh                    # 用户级：~/.trae
sh install.sh --cn               # Trae CN 的用户级：~/.trae-cn
sh install.sh --project DIR      # 项目级：DIR/.trae
```

```bat
install.cmd                      & rem 用户级：%USERPROFILE%\.trae
install.cmd -Cn                  & rem Trae CN 的用户级：%USERPROFILE%\.trae-cn
install.cmd -Project DIR         & rem 项目级：DIR\.trae
```

| 包内文件 | 安装位置 | 用途 |
| --- | --- | --- |
| `skills/anet`、`skills/anet-setup` | `<trae-dir>/skills/` | 两个 skill。Trae 在所有项目中加载 `~/.trae/skills`（Trae CN 为 `~/.trae-cn/skills`）下的 skill，并在对应项目中加载 `.trae/skills` 下的 skill。 |
| `commands/anet.md`、`commands/anet-status.md` | `<trae-dir>/commands/` | 命令 `/anet` 与 `/anet-status`。 |
| `anet/bin/` | `<trae-dir>/anet/bin/` | 运行脚本与 MCP 服务的启动脚本：Linux 和 macOS 上为 `anet-run.sh` 与 `anet-mcp.sh`，Windows 上为 `anet-run.cmd`、`anet-run.ps1` 与 `anet-mcp.cmd`。Skill 通过 `<skill-dir>/../../anet/bin/` 找到运行脚本。 |
| `mcp.json` | 仅项目级 | 项目的 MCP 配置，仅在 `.trae/mcp.json` 不存在时写入。 |

如需手动安装，请把这些目录复制到相同位置，并使用安装脚本的输出配置 MCP 服务。

**MCP 服务。** 在 Linux 和 macOS 上，Trae 通过 `/bin/sh` 和启动脚本 `anet-mcp.sh` 启动该服务，因此缺少可执行位也不受影响；桌面应用并不总是继承登录 shell 的 `PATH`，启动脚本仍可在 `~/.local/bin` 中找到 `anet`。在 Windows 上，命令为 `anet-mcp.cmd` 的完整路径。Trae 的文档要求 MCP 服务的命令不得含空格：在 Linux 和 macOS 上，含空格的主目录会被拒绝；在 Windows 上，含空格的路径会替换为其短路径（8.3 格式），若该卷不提供短路径则拒绝安装。

- 用户级：安装脚本输出配置。请在“设置”“MCP”“添加”“手动创建”（或“原始配置”）中填入，或在浏览器中打开安装脚本输出的 `trae://` 链接（仅适用于 Trae），并确认 Trae 的提示。Trae 未公开全局 MCP 文件的路径，因此安装脚本不写入该文件。
- 项目级：安装脚本写入 `.trae/mcp.json`。在 Linux 和 macOS 上，其中通过 `${workspaceFolder}` 指向启动脚本；在 Windows 上，其中写入启动脚本的完整路径。请在 Trae 的设置中开启“项目 MCP”并确认提示。已有的 `.trae/mcp.json` 不会被修改，安装脚本会输出需要添加的成员。

**设置项。** 本插件有两个可选设置，需要时传给安装脚本；未给出的选项沿用上次安装时的值。

| 选项 | 默认值 | 含义 |
| --- | --- | --- |
| `--identity NAME`（`-Identity NAME`；`-ClearIdentity` 清除） | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| `--anet PATH`（`-Anet PATH`；`-ClearAnet` 清除） | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

安装脚本把取值保存在已安装插件包的 `anet/settings` 文件中，供 skill 与命令所用的运行脚本读取；同时写入所输出 MCP 条目的 `env`（`ANET_PLUGIN_IDENTITY`、`ANET_PLUGIN_BIN`），供启动脚本读取。修改后请重新运行安装脚本，并按新的输出更新 Trae 中的 MCP 条目，使两处一致。

**卸载。** `sh install.sh --remove` 或 `install.cmd -Remove`（带上相同的范围选项）会删除已安装的文件。同时请在 Trae 中删除 MCP 服务 `anet`。

**验证状态。** 文件位置与格式依据 Trae 文档。脚本已在 Linux 上运行，Windows 脚本已用 PowerShell 测试，MCP 启动脚本已通过通用 MCP 客户端驱动。这些文件尚未在 Trae 应用中实际加载。
