## 设置项

本插件有两个选项，由 Claude Code 与用户的其他设置一并保存，均为可选。可在 Claude Code 中通过 `/plugin`（选择 `anet`，再进入配置）修改，也可在终端中执行：

```sh
echo '{"identity": "work", "anetPath": "/opt/anet/bin/anet"}' | claude plugin configure anet@agentnetwork --values-stdin
```

修改在新会话启动时对 MCP 服务生效。所选身份的节点必须处于运行状态（`anet --id <name> up`）；否则 `anet` 服务只提供 `connect` 工具，直到节点运行。

| 选项 | 默认值 | 含义 |
| --- | --- | --- |
| `identity` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `anetPath` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |
