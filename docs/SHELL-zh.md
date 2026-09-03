# shell 模块:在宿主机上执行运营者批准的命令

**编译 tag:`shell`(加法)。默认构建不含本模块。**

面向的场景:一台开发板或远程机器上跑着 anet daemon,需要从网络另一端触发
重启服务、读日志、烧固件这类操作,并拿到回显;权限随时可收回。

## 与其它模块相反的 tag 方向

其余可选子系统一律"默认在、`-tags no_<name>` 去掉"。本模块相反:默认不在,
`-tags shell` 才编进去。

理由是两种 tag 出错的代价不对称。减法 tag 写错,后果是某人想去掉的模块没去掉;
加法 tag 写错,后果是命令执行能力进了所有没要过它的构建。需要靠"记得去掉"来
保护的人,恰好是没听说过这个 tag 的人。

判据同样是符号数,方向反过来:

```
go build -o /tmp/a ./cmd/anet            && go tool nm /tmp/a | grep -c module/shell  # 必须 0
go build -tags shell -o /tmp/b ./cmd/anet && go tool nm /tmp/b | grep -c module/shell  # 必须 > 0
```

CI 的 `optin` job 每次提交两个方向都验。

## 编进去 ≠ 开着

带 `shell` 的构建在没有 `modules.shell` 配置块时不注册任何能力,不执行任何命令。
"这个二进制有这个能力"和"这台机器现在接受远程命令"是两个独立决定。

第三个决定是名单:配置块写了、命令也定义了,但 `allow` / `allow_file` 为空时,
**所有远程调用一律拒绝**。空名单是拒绝所有,不是放行所有。

不带调用方身份的调用(`CallerAID` 为空)同样默认拒绝,需要显式 `allow_local: true`。
这条不是为了防谁,是为了防字段忘填:`CallerAID` 的零值就是空字符串,把空当作
"本机、可信"意味着将来任何一处新调用点漏填,都会静默绕过整份名单。默认拒绝
让同一个疏忽表现为一次看得见的拒绝。

## 配置

```json
{"modules": {"shell": {
  "commands": {
    "restart-app":  {"run": "systemctl restart myapp", "description": "重启业务进程"},
    "tail-log":     {"run": "journalctl -u myapp -n 200 --no-pager"},
    "flash":        {"run": "/opt/tools/flash.sh", "args": true, "timeout_s": 600}
  },
  "allow_file": "/etc/anet/shell-allow",
  "timeout_s": 30,
  "max_output_bytes": 65536
}}}
```

| 字段 | 含义 |
| --- | --- |
| `commands.<name>.run` | 命令行,经 `/bin/sh -c` 执行 |
| `commands.<name>.args` | 允许调用方追加参数(默认 false) |
| `commands.<name>.dir` | 工作目录 |
| `commands.<name>.timeout_s` | 该命令的超时,覆盖全局值 |
| `allow` | 允许调用的 AID 内联列表 |
| `allow_local` | 放行不带调用方身份的调用,默认 false |
| `allow_file` | 允许调用的 AID 文件,一行一个,`#` 起注释 |
| `allow_arbitrary` | 开放 `shell.exec`(任意命令),默认 false |
| `timeout_s` | 全局超时,默认 30,上限 1800 |
| `max_output_bytes` | 回显上限,默认 65536,上限 4 MiB |
| `shell` | 解释器,默认 `/bin/sh` |

`allow` 与 `allow_file` 互斥,同时给出在加载期报错。

## 暴露的能力

| 能力 id | 行为 |
| --- | --- |
| `shell.list` | 报告本节点定义了哪些命令、谁在名单上 |
| `shell.run@<name>` | 执行一条已定义命令 |
| `shell.exec` | 执行调用方给的任意命令;仅在 `allow_arbitrary` 为 true 时存在 |

`shell.exec` 不开时既不出现在能力卡上,也不接受按名调用。

## 收权

`allow_file` **每次调用重读**,不缓存。删掉一行、或删掉整个文件,下一次调用即生效,
不需要重启 daemon。文件不存在等同空名单(拒绝所有),不是错误、也不是放行。

`allow` 内联列表改完需要重载配置,所以生产上建议用 `allow_file`。

## 行为约定

- **不提权。** 命令以 daemon 自己的用户身份运行。要 root 就要让 daemon 以 root 运行,
  那是安装期的决定,写在 unit 文件里、看得见。本模块不调用 sudo、不 setuid。
- **参数不会变成命令。** `args: true` 时调用方给的 `argv` 逐个单引号包裹后追加,
  命令行归运营者、参数归网络,两者不能混成一个东西。
- **超时杀整个进程组。** 只杀 shell 会把它启动的子进程留在机器上,没有句柄能再找到它们。
- **回显有上限。** 超出部分截断,并在输出里写明截断,不静默截。
- **诚实效果状态。** 退出码 0 → `OK`;非 0 → `FAILED` 并带退出码与 stderr;
  超时被杀 → `FAILED` 且 message 说明被杀。不会出现"被杀了但报 OK、输出为空"。
- **每次执行都上证据链。** 事件 `anet.shell.command` 记录谁调的、跑了什么、退出码、
  耗时;被拒的调用记 `anet.shell.refused`,含调用方 AID。回显本身不入链(可能很大,
  也可能含命令打印的任意内容)。

## 联调覆盖

`scripts/joint-shell.sh` 起一个 hub 加两个 daemon,跨进程走真实委派路径,验 18 项。
单元测试自己构造 `provider.Call`,断言的是测试文件自己写进去的 `CallerAID`;
整套授权都压在这个字段上,而"到达 Invoke 的是验签后的身份、不是调用方自称的值"
只有真委派能证明。脚本里 3/7 就是这一条。

其余在实网路径上验的:名单文件不存在时拒绝、参数元字符不被解释、非零退出回 FAILED
且 stderr 完整、删名单行后下一次调用即被拒(不重启)、加回后即恢复、`shell.exec`
不出现在 hub 目录里且按名调用不执行。

## 已知取舍

`run` 经 shell 执行,运营者可以直接写平时敲的命令(管道、重定向、`&&`),代价是
运营者自己写进 `run` 的内容不受任何限制。这条线划在"运营者写的"和"网络传来的"之间,
后者才是被引号隔离的那一侧。

**命令名会进 hub 目录。** 节点向 hub 通告的是它实际会应答的能力 id,所以
`shell.run@restart-app` 这类名字对任何能浏览目录的人可见。这不给出访问权(名单仍然拦着),
但确实公开了"这台机器可以被要求做什么"。不希望公开的命令,名字就不要带信息。

`shell.list` 只把**名单条数**回给远程调用方,不回 AID 列表 —— 一个被接受的调用方
不该顺带拿到本节点信任的其余机器名单。本机调用(CallerAID 为空)拿到完整列表,
那是运营者在读自己的配置。
