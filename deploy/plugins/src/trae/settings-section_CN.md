## 安装与设置

Trae 没有可由代码仓库提供的插件包格式。因此本插件包通过脚本 `install.sh` 安装：脚本把文件复制到 Trae 的配置目录，并输出 MCP 配置。脚本不启动任何进程，不建立任何连接，也不读取 Trae 的任何文件。它创建的唯一一个 Trae 文件，是尚不存在的项目级 `mcp.json`。

```sh
sh install.sh                    # 用户级：~/.trae
sh install.sh --cn               # Trae CN 的用户级：~/.trae-cn
sh install.sh --project DIR      # 项目级：DIR/.trae
```

| 包内文件 | 安装位置 | 用途 |
| --- | --- | --- |
| `skills/anet`、`skills/anet-setup` | `<trae-dir>/skills/` | 两个 skill。Trae 在所有项目中加载 `~/.trae/skills`（Trae CN 为 `~/.trae-cn/skills`）下的 skill，并在对应项目中加载 `.trae/skills` 下的 skill。 |
| `commands/anet-status.md` | `<trae-dir>/commands/` | 命令 `/anet-status`。 |
| `anet/bin/anet-run.sh`、`anet/bin/anet-mcp.sh` | `<trae-dir>/anet/bin/` | 运行脚本与 MCP 服务的启动脚本。Skill 通过 `<skill-dir>/../../anet/bin/anet-run.sh` 找到运行脚本。 |
| `mcp.json` | 仅项目级 | 项目的 MCP 配置，仅在 `.trae/mcp.json` 不存在时写入。 |

如需手动安装，请把这些目录复制到相同位置，并使用 `mcp.json` 的内容（项目级）或 `install.sh` 的输出（用户级）配置 MCP 服务。

**MCP 服务。** Trae 通过 `/bin/sh` 和启动脚本启动该服务，因此缺少可执行位也不受影响；桌面应用并不总是继承登录 shell 的 `PATH`，启动脚本仍可在 `~/.local/bin` 中找到 `anet`。

- 用户级：脚本输出配置。请在“设置”“MCP”“添加”“手动创建”（或“原始配置”）中填入，或在浏览器中打开脚本输出的 `trae://` 链接（仅适用于 Trae），并确认 Trae 的提示。Trae 未公开全局 MCP 文件的路径，因此脚本不写入该文件。Trae 的文档要求 MCP 服务的命令不得含空格，但未说明其参数中的空格如何处理，因此含空格的主目录会被拒绝。
- 项目级：脚本写入 `.trae/mcp.json`，其中通过 `${workspaceFolder}` 指向启动脚本。请在 Trae 的设置中开启“项目 MCP”并确认提示。已有的 `.trae/mcp.json` 不会被修改，脚本会输出需要添加的成员。

**设置项。** 本插件有两个可选设置，需要时传给 `install.sh`；未给出的选项沿用上次安装时的值。

| 选项 | 默认值 | 含义 |
| --- | --- | --- |
| `--identity NAME` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `--anet PATH` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

脚本把取值保存在已安装插件包的 `anet/settings` 文件中，供安装 skill 与状态命令所用的 `bin/anet-run.sh` 读取；同时写入所输出 MCP 条目的 `env`（`ANET_PLUGIN_IDENTITY`、`ANET_PLUGIN_BIN`），供启动脚本读取。修改后请重新运行脚本，并按新的输出更新 Trae 中的 MCP 条目，使两处一致。

**卸载。** `sh install.sh --remove`（带上相同的 `--cn` 或 `--project`）会删除已安装的文件。同时请在 Trae 中删除 MCP 服务 `anet`。

**验证状态。** 文件位置与格式依据 Trae 文档。脚本已在 Linux 上运行，MCP 启动脚本已通过通用 MCP 客户端驱动。这些文件尚未在 Trae 应用中实际加载。
