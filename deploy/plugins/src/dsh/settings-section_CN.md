## 安装

DSH 通过 `dsh plugin` 把插件安装到某个 profile，该命令转交给 `pnpm` 执行，因此 `pnpm` 须在 `PATH` 中。本插件是无需构建的纯 JavaScript bundle，pnpm 不会要求批准构建脚本。选用下列来源之一，然后重启该 profile：bundle 列表的变更在 profile 启动时生效。

| 来源 | 命令 |
| --- | --- |
| 本地目录（即本目录） | `dsh plugin --profile <profile> add ./<目录>` |
| ANet 仓库的 Git 子目录 | `dsh plugin --profile <profile> add "git+https://github.com/ANetResearch/ANet.git#<commit>&path:/deploy/plugins/dist/dsh"` |

`<commit>` 是包含本插件的 ANet 仓库提交的完整哈希。命令 `dsh plugin --profile <profile> remove dsh-anet` 可移除本插件。本插件声明支持的 DSH 版本为 `>=0.2.0-0 <0.3.0`；DSH 目前为开发者预览版，升级 DSH 前请先查看本插件的发布说明。

## 设置项

DSH 没有面向插件的声明式设置界面。两项设置是 `cordis.patch.yml` 中 `anet-plugin` 行的配置。如需修改，请在 profile 的补丁（`$DSH_HOME/profiles/<profile>/cordis.patch.yml`）中重述该行；该行会整体替换插件原有行的配置，因此两个键都要给出：

```yaml
- id: anet-plugin
  config:
    anetPath: /usr/local/bin/anet
    identity: work
```

| 设置项 | 默认值 | 含义 |
| --- | --- | --- |
| `identity` | 空 | 指定的命名身份（`anet id ls`）。为空时使用默认身份。 |
| `anetPath` | 空 | `anet` 可执行文件（Windows 上为 `anet.exe`）的绝对路径。为空时先查找 `PATH`，然后在 Linux 和 macOS 上查找 `~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`，在 Windows 上查找 Windows 安装程序的安装位置（`%LOCALAPPDATA%\Programs\anet`、`%LOCALAPPDATA%\anet\bin`、`%USERPROFILE%\.anet-bin`）。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |

某个键为空时，使用启动 DSH 的进程的环境变量 `ANET_PLUGIN_IDENTITY` 和 `ANET_PLUGIN_BIN`。生效的取值同时作用于 MCP 服务，以及智能体为本插件的 skill 所运行的命令。MCP 服务随 profile 启动，因此修改后需重启 profile。

MCP 服务 `anet` 对一次工具调用最多等待 15 分钟（`toolCallTimeoutMs`），使等待任务结果的长时间调用不被中断；DSH 对其他 MCP 调用默认 60 秒后中止。DSH 会先启动一次 stdio MCP 服务用于协商，再启动一次用于会话，因此启动时 `anet mcp` 会运行两次。
