## 设置项

本插件有两个选项，由 CodeBuddy 与用户的其他设置一并保存在用户设置文件中，均为可选。请把它们加入 `~/.codebuddy/settings.json`（WorkBuddy 为 `~/.workbuddy/settings.json`；若设置了 `CODEBUDDY_CONFIG_DIR` 或 `WORKBUDDY_CONFIG_DIR`，则为该变量所指目录）中本插件的 `pluginConfigs` 条目：

```json
{
  "pluginConfigs": {
    "anet@agentnetwork": {
      "options": { "identity": "work", "anetPath": "/opt/anet/bin/anet" }
    }
  }
}
```

修改在新会话启动时对 MCP 服务生效。

| 选项 | 默认值 | 含义 |
| --- | --- | --- |
| `identity` | 空 | 指定的命名身份（`anet id ls`）。为空时使用 `~/.anet` 中的默认身份。 |
| `anetPath` | 空 | `anet` 可执行文件的绝对路径。为空时依次查找 `PATH`、`~/.local/bin`、`/usr/local/bin` 和 `/opt/homebrew/bin`。若该路径不是可执行文件，则报告错误；不会改用其他副本，安装 skill 也不会另行安装。 |
