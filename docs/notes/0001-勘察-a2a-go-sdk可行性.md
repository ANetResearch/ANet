# 01 · a2a-go SDK 嵌入 anet 的可行性调查

调查对象:`/data/projs/anet-oss/Refs/a2a-go`(模块 `github.com/a2aproject/a2a-go/v2`,本地克隆位于 tag `v2.6.0`,HEAD `ebf17c56`,2026-09-25)。
协议基线:`Refs/a2a/docs/specification.md` 标注最新发布版本为 1.0.0(specification.md:3);SDK 实现的协议版本常量为 `a2a.Version = "1.0"`(a2a/core.go:33)。

标注约定:
- 未加标注的陈述均由阅读源码或实际运行确认,并给出 `文件:行`。
- **[推断]** 表示基于代码结构的推论,未经运行验证。
- **[实测]** 表示在 scratchpad 中实际编译或运行得到的结果;探针代码位于 `scratchpad/a2ago-work/sizeprobe/`(`probe/*.go`、`rest/`、`full/`、`grpc/`、`compat/`、`tagged/`)与 `scratchpad/a2ago-work/modprobe2/`。仓库 `Refs/a2a-go` 未被修改,所有构建均在其副本 `scratchpad/a2ago-work/a2a-go/` 上进行。

---

## 0. 结论摘要

1. HTTP+JSON(REST)与 JSON-RPC 服务端都在 `a2asrv` 包内(`NewRESTHandler` a2asrv/rest.go:42,`NewJSONRPCHandler` a2asrv/jsonrpc.go:39),gRPC 单独在 `a2agrpc/v1`。只导入 `a2asrv`、`a2aclient`、`a2acrypto`、`a2aext` 时,链接产物中 grpc/protobuf 符号数为 0 **[实测]**,`CGO_ENABLED=0` 可构建,stripped 二进制增量约 0.96 MiB(仅服务端)到 1.63 MiB(服务端+客户端+签名+扩展)**[实测]**。
2. 导入 `a2agrpc/v1` 或 `a2acompat/a2av0`(A2A 0.3 兼容层)会拉入 grpc + protobuf,增量约 7.3 MiB / 8.2 MiB;`a2av0` 还会拉入旧模块 `github.com/a2aproject/a2a-go v0.3.15` **[实测]**。两者应放在独立的 build tag 后面。
3. 实现方需要提供的核心接口只有 `AgentExecutor`(a2asrv/agentexec.go:97-122);`taskstore.Store`、`push.ConfigStore`、`push.Sender`、卡片生产者、调用拦截器均可选,都有内存默认实现或可省略。`RequestHandler` 本身是公开接口(a2asrv/handler.go:41-74),可以由 anet 自行实现(例如代理到远端 daemon),再套用 SDK 的 JSON-RPC/REST 线协。
4. `a2acrypto` 实现 AgentCard 的 JWS(RFC 7515)+ JCS(RFC 8785)签名与验签,支持 EdDSA/Ed25519(a2acrypto/alg.go:37-38, verify.go:125-129),并有一条由 a2a-python 生成的 Ed25519 跨 SDK 金标向量测试(a2acrypto/a2acrypto_test.go:27,39,68)。`ANetCore/identity` 的 `CurrentPrivateKey()` 返回 `ed25519.PrivateKey`(ANetCore/identity/identity.go:233),可直接作为 `crypto.Signer` 传入 `a2acrypto.NewSigner`。
5. 发现 7 处与规范或 anet 安全要求不符的行为,其中 4 处经实测确认:逗号分隔的 `A2A-Extensions` 请求头不被识别、服务端不回显已激活扩展、服务端忽略 `A2A-Version`、请求体无大小上限;另有 `text/plain` 跨源 POST 可触发执行、卡片解析器在未签名卡片上不做校验(fail-open)、Go 端对 nil 切片输出 `null` 影响签名规范化。这些都可以在 anet 侧用中间件规避,同时适合作为向上游提交的修复(符合"贡献而非竞争"的决定)。
6. 一致性测试:`e2e/` 是 SDK 自身的进程内 Go 测试,不能对外部服务运行;`e2e/tck/` 提供了驱动外部 Python 套件 `a2aproject/a2a-tck` 的脚本,可以对任意 SUT URL 运行;`itk/` 是依赖 `a2aproject/a2a-itk` + Docker/Podman 的跨 SDK 互通矩阵,接入门槛较高。上游 CI 中 ACTS 一致性任务目前为 advisory,注释写明"no SDK passes yet"(.github/workflows/acts.yaml:67-70)。
7. 许可证 Apache-2.0(LICENSE,README.md:149-151),无 NOTICE 文件;v2 语义化版本,2026-03 发布 1.0.0,之后约每月一个 minor(CHANGELOG.md:3-142)。签名功能在 v2.6.0(2026-09-25)才加入(CHANGELOG.md:8,14),实际使用时间很短。

---

## 1. 包结构与职责

| 包 | 职责 | 依据 |
|---|---|---|
| `a2a` | 核心类型(Task、Message、Part、AgentCard、事件)与构造函数 | docs/ai/OVERVIEW.md:14;a2a/core.go、a2a/agent.go |
| `a2asrv` | 传输无关的 `RequestHandler`、JSON-RPC 与 REST 的 `http.Handler`、卡片 handler、调用拦截器、签名卡片生产者 | a2asrv/doc.go:15-48;docs/ai/OVERVIEW.md:16 |
| `a2asrv/taskstore` | `Store` 接口与内存实现 | a2asrv/taskstore/api.go:79-91 |
| `a2asrv/push` | 推送配置存储与 HTTP 推送发送器(默认带 SSRF 防护) | a2asrv/push/api.go:25-48;sender.go:70 |
| `a2asrv/eventqueue`、`workqueue`、`limiter` | 事件队列、集群模式工作队列、并发限额 | a2asrv/eventqueue/manager.go:24-33;workqueue/queue.go:78-83;limiter/limiter.go |
| `a2aclient`、`a2aclient/agentcard` | 客户端工厂、`Transport` 接口、JSON-RPC/REST 传输、卡片解析器 | a2aclient/transport.go:30-72;factory.go;agentcard/resolver.go |
| `a2acrypto` | AgentCard JWS 签名/验签、JCS 规范化、JWKS 解析 | a2acrypto/doc.go:15 |
| `a2aext` | 扩展激活(客户端)与元数据/请求头传播(两端)的拦截器 | a2aext/doc.go:15;activator.go;propagator.go |
| `a2agrpc/v0`、`a2agrpc/v1`、`a2apb/*` | gRPC 绑定与 protobuf 生成代码 | docs/ai/OVERVIEW.md:17 |
| `a2acompat/a2av0` | A2A 0.3 线协兼容(JSON-RPC/REST 服务端与客户端、旧卡片) | a2acompat/a2av0/doc.go;jsonrpc_server.go:41;rest_server.go:48 |
| `a2aevent` | 事件应用工具(把事件应用到 Task) | a2aevent/event.go:15-40 |
| `itk`(独立模块)、`e2e`、`acts` | 互通测试代理、进程内端到端测试、ACTS 行为清单 | itk/go.mod;e2e/*.go;acts/sut-behaviors.yaml |

---

## 2. 依赖图、二进制体积与 build tag 可插拔性

### 2.1 各包的依赖闭包 [实测]

命令:在副本中执行 `CGO_ENABLED=0 go list -deps <pkg>`,统计 grpc/protobuf/genproto 包数和非标准库、非本模块依赖。

| 包 | deps 总数(含 std) | grpc/pb 包数 | 其余外部依赖 |
|---|---|---|---|
| `a2a` | 120 | 0 | `github.com/google/uuid` |
| `a2asrv` | 213 | 0 | uuid、`golang.org/x/sync/errgroup` |
| `a2aclient` | 201 | 0 | uuid、`golang.org/x/mod/semver` |
| `a2aclient/agentcard` | 197 | 0 | uuid |
| `a2acrypto` | 192 | 0 | uuid |
| `a2aext` | 216 | 0 | uuid、x/mod、x/sync |
| `a2agrpc/v1` | 348 | 104 | 另含 x/net/http2、x/text、x/sys、genproto |
| `a2acompat/a2av0` | 364 | 103 | 另含旧模块 `github.com/a2aproject/a2a-go`(a2a、a2aclient、a2asrv、a2apb)|

`a2av0` 拉入 grpc 的原因:它直接导入旧模块的 `a2aclient`、`a2asrv` 和本模块的 `a2apb/v0/pbjson`(`go list -f '{{.Imports}}' ./a2acompat/a2av0` 结果;源码见 a2acompat/a2av0/jsonrpc_client.go:27、conversions.go:29)。

### 2.2 链接产物体积与符号数 [实测]

构建:`CGO_ENABLED=0 go build -trimpath`,stripped 版本加 `-ldflags="-s -w"`,Go 1.26.7 linux/amd64。符号数用 `go tool nm | grep -c`。

| 变体 | 内容 | 未 strip | stripped | 相对 base 的 stripped 增量 | grpc 符号 | protobuf 符号 | a2a-go 符号 |
|---|---|---|---|---|---|---|---|
| base | 仅 `net/http` + `encoding/json` | 8,599,967 | 5,951,650 | — | 0 | 0 | 0 |
| rest | `a2asrv.NewHandler` + JSON-RPC + REST + 静态卡片 | 10,033,004 | 6,955,170 | +1,003,520 | 0 | 0 | 557 |
| full | rest + 签名卡片(`a2acrypto`)+ `a2aclient` + 卡片解析器 + `a2aext` | 11,045,245 | 7,655,586 | +1,703,936 | 0 | 0 | 699 |
| grpc | `a2asrv` + `a2agrpc/v1` gRPC 服务 | 19,700,108 | 13,607,074 | +7,655,424 | 2,115 | 3,807 | 1,364 |
| compat | `a2asrv` + `a2av0.NewJSONRPCHandler` | 21,026,309 | 14,524,578 | +8,572,928 | 2,231 | 3,798 | 1,733(含旧模块 165) |

注:anet daemon 已经链接大量标准库(crypto、net/http 等),实际增量应小于表中数值 **[推断]**。

### 2.3 build tag 两个方向的验证 [实测]

`sizeprobe/tagged/`:`main.go` 无 tag,只遍历一个 hook 列表;`a2a_on.go` 带 `//go:build !no_a2a`,在 `init` 中注册 `a2asrv.NewJSONRPCHandler`,并引用 `a2aclient`、`a2acrypto`。

| 构建 | 大小 | a2a-go 符号 | uuid 符号 | grpc 符号 |
|---|---|---|---|---|
| 默认 | 9,993,288 | 513 | 29 | 0 |
| `-tags no_a2a` | 8,279,969 | 0 | 0 | 0 |

满足 CLAUDE.md "完整构建 >0、裁剪构建 ==0" 的判据。SDK 的包级 `init`(例如 a2asrv/taskstore/inmemory.go:59-62 的 `gob.Register`、a2a/core.go:662 的 `init`)只在包被链接时执行,不会泄漏到裁剪构建。

### 2.4 go.mod / go.sum 足迹 [实测]

`scratchpad/a2ago-work/modprobe2/`:新模块从 goproxy.cn 拉取已发布的 `v2.6.0`,只导入 `a2asrv`、`a2aclient`、`a2acrypto`、`a2aext`,执行 `go mod tidy` 后:
- go.mod 新增 indirect 依赖只有 `github.com/google/uuid v1.6.0`、`golang.org/x/mod v0.38.0`、`golang.org/x/sync v0.22.0`;
- go.sum 中不出现 grpc、protobuf、genproto 或旧模块 v0.3.15。

对 anet 的影响:ANet 已有 `github.com/google/uuid v1.6.0`(ANet/go.mod:24);`golang.org/x/sync` 会由 v0.21.0(ANet/go.mod:39)经 MVS 升到 v0.22.0;`golang.org/x/mod` 为新增。SDK 的 `go 1.26.0`(go.mod:3)低于 anet 的 `go 1.26.6`(ANet/go.mod:3),兼容。README 写的最低版本是 Go 1.25(README.md:34),与 go.mod 不一致,以 go.mod 为准。

---

## 3. 服务端 `a2asrv`

### 3.1 组装方式

```go
h := a2asrv.NewHandler(executor, opts...)          // a2asrv/handler.go:197
mux.Handle("/a2a/jsonrpc", a2asrv.NewJSONRPCHandler(h, tOpts...))   // jsonrpc.go:39
mux.Handle("/a2a/rest/",   a2asrv.NewRESTHandler(h, tOpts...))      // rest.go:42
mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewAgentCardHandler(producer)) // agentcard.go:27,109
```

- `NewHandler` 返回的是 `*InterceptedHandler`,它包住默认实现并负责建立 `CallContext`、执行拦截器、检查必需扩展(handler.go:198-249;intercepted_handler.go:32-42,308-326)。
- 未提供 `WithTaskStore` 时使用内存存储,并装配 `NewTaskStoreAuthenticator()` 作为属主判定(handler.go:233-238)。
- `NewTenantRESTHandler(template, h)` 按路径模板提取 tenant 并剥离前缀(rest.go:76-98);普通 `NewRESTHandler` 内有 `// TODO: handle tenant`(rest.go:50)。
- 传输选项只有两项:SSE keep-alive 间隔与 panic 处理器(a2asrv/transport.go:22-43)。
- JSON-RPC 方法名为 1.0 形式:`SendMessage`、`SendStreamingMessage`、`GetTask`、`ListTasks`、`CancelTask`、`SubscribeToTask`、`*TaskPushNotificationConfig*`、`GetExtendedAgentCard`(internal/jsonrpc/jsonrpc.go:38-48)。
- 包文档中的示例 `task.WithTaskStore`、`a2asrv.WithCallInterceptor`(a2asrv/doc.go:29-35)与实际 API `a2asrv.WithTaskStore`(handler.go:169)、`a2asrv.WithCallInterceptors`(middleware.go:116)不一致,属于文档漂移。

### 3.2 实现方提供的接口(签名原文)

**必需:执行器**(a2asrv/agentexec.go:97-122)

```go
type AgentExecutor interface {
    Execute(ctx context.Context, execCtx *ExecutorContext) iter.Seq2[a2a.Event, error]
    Cancel(ctx context.Context, execCtx *ExecutorContext) iter.Seq2[a2a.Event, error]
}
// 便捷形式:默认 Cancel 只发 TaskStateCanceled(agentexec.go:126-140)
type AgentExecutorFunc func(context.Context, *ExecutorContext) iter.Seq2[a2a.Event, error]
// 可选:执行结束后的清理钩子(agentexec.go:143-146)
type AgentExecutionCleaner interface {
    Cleanup(ctx context.Context, execCtx *ExecutorContext, result a2a.SendMessageResult, err error)
}
```

执行器输入 `ExecutorContext`(a2asrv/exectx.go:40-59)含 `Message`、`TaskID`、`StoredTask`、`RelatedTasks`、`ContextID`、`Metadata`、`User`、`ServiceParams`、`Tenant`。服务端在收到 `Message`、终态、`input-required` 事件后停止处理(agentexec.go:45-48)。每次 `Execute` 在独立 goroutine 中运行(agentexec.go:100)。

**可选:任务存储**(a2asrv/taskstore/api.go:56-91)

```go
type Store interface {
    Create(ctx context.Context, task *a2a.Task) (TaskVersion, error)
    Update(ctx context.Context, update *UpdateRequest) (TaskVersion, error) // PrevVersion 不符时必须返回 ErrConcurrentModification
    Get(ctx context.Context, taskID a2a.TaskID) (*StoredTask, error)
    List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error)
}
type StoredTask struct { Task *a2a.Task; Version TaskVersion; User string }
```

内存实现 `taskstore.NewInMemory(&InMemoryStoreConfig{Authenticator, TimeProvider})`(inmemory.go:40-85);`Authenticator` 类型为 `func(context.Context) (string, error)`(inmemory.go:40)。

**可选:推送**(a2asrv/push/api.go:25-48)

```go
type Sender interface {
    SendPush(ctx context.Context, config *a2a.PushConfig, event a2a.Event) error
}
type ConfigStore interface {
    Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error)
    Get(ctx context.Context, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error)
    List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error)
    Delete(ctx context.Context, taskID a2a.TaskID, configID string) error
    DeleteAll(ctx context.Context, taskID a2a.TaskID) error
}
```

通过 `WithPushNotifications(store, sender)` 启用(handler.go:160-165);未配置时推送相关方法返回 `ErrPushNotificationNotSupported`(handler.go:511-526)。默认 `NewHTTPPushSender` 拒绝回环/私有地址(push/sender.go:66-69,134-136),v2.4.0 起默认开启(CHANGELOG.md:49)。

**可选:卡片生产者**(a2asrv/agentcard.go:30-65)

```go
type AgentCardProducer interface { Card(ctx context.Context) (*a2a.AgentCard, error) }
type AgentCardJSONProducer interface { CardJSON(ctx context.Context) ([]byte, error) } // 可直接返回原始 JSON
type ExtendedAgentCardProducer interface {
    ExtendedCard(ctx context.Context, req *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error)
}
```

`NewAgentCardHandler` 优先使用 `AgentCardJSONProducer`(agentcard.go:122-131)。签名包装:`NewSignedCardProducer(wrapped AgentCardProducer, sr SignerResolverFunc)`,`SignerResolverFunc func(context.Context) ([]*a2acrypto.Signer, error)`,每次请求重新解析签名者以支持轮换(a2asrv/signed_card.go:27-72)。

**可选:拦截器**(a2asrv/middleware.go:93-103;exectx.go:27-30)

```go
type CallInterceptor interface {
    Before(ctx context.Context, callCtx *CallContext, req *Request) (context.Context, any, error)
    After(ctx context.Context, callCtx *CallContext, resp *Response) error
}
type ExecutorContextInterceptor interface {
    Intercept(ctx context.Context, execCtx *ExecutorContext) (context.Context, error)
}
```

`CallInterceptor.Before` 返回非 nil 结果或错误时跳过实际处理(middleware.go:95-98);`CallContext.User` 由认证中间件写入(middleware.go:48-50),`NewAuthenticatedUser(name, attrs)` 构造已认证用户(auth.go:28-34)。

**集群模式(实验性,anet 单机 daemon 不需要)**:`WithClusterMode(ClusterConfig{QueueManager, WorkQueue, TaskStore, ContextCodec})`(handler.go:179-194,注释 "experimental" 在 handler.go:186);`eventqueue.Manager`(eventqueue/manager.go:24-33)、`workqueue.Queue`(workqueue/queue.go:78-83)、`ContextCodec`(internal/taskexec/api.go:110-115)。

**替代路径:自行实现 `RequestHandler`**(handler.go:41-74,11 个方法)。`InterceptedHandler` 的字段 `Handler`、`Interceptors`、`Logger` 是导出的(intercepted_handler.go:32-39),可以包住自定义实现;但 `capabilities` 字段未导出(intercepted_handler.go:41),因此自定义实现无法借用 `checkRequiredExtensions`,需要自己检查必需扩展。

### 3.3 `RequestHandlerOption` 一览(handler.go:101-194;agentcard.go:68-81;middleware.go:116;exectx.go:33)

`WithCapabilityChecks`、`WithLogger`、`WithEventQueueManager`、`WithExecutionPanicHandler`、`WithAgentInactivityTimeout`、`WithConcurrencyConfig`、`WithPushNotifications`、`WithTaskStore`、`WithClusterMode`、`WithExtendedAgentCard`、`WithExtendedAgentCardProducer`、`WithCallInterceptors`、`WithExecutorContextInterceptor`。

### 3.4 身份与任务隔离 [实测]

- 内存存储在 `Get` 时把属主不匹配映射为 `ErrTaskNotFound`(inmemory.go:171-178),`List` 对空用户名返回 `ErrUnauthenticated`(inmemory.go:191-194)。属主来自 `CallContext.User.Name`(middleware.go:106-113)。
- 探针 `probe/owner_test.go`:
  - 未配置认证拦截器时,调用方 A 创建的任务可以被任意其他调用方通过 `GetTask` 读取(所有匿名调用方的用户名都是空串)。
  - 配置一个把 `X-Caller` 写入 `callCtx.User` 的拦截器后,跨调用方 `GetTask` 返回 `-32001 task not found`。
  - 匿名 `ListTasks` 在两种配置下都返回 `-31401 unauthenticated`。
- 对 anet 的含义:127.0.0.1 接口和来自远端 daemon 的入站请求都必须在 `CallInterceptor.Before` 中写入已认证身份(本地为本机令牌对应的主体,远端为 E2E 会话对端 AID),否则任务在调用方之间不隔离 **[推断]**。

### 3.5 已发现的行为缺口

| # | 位置 | 行为 | 对 anet 的影响 | 发现方式 |
|---|---|---|---|---|
| S1 | a2asrv/extensions.go:64-70;jsonrpc.go:50;rest.go:64 | `RequestedURIs()` 直接返回请求头原值,不按逗号拆分。规范规定 `A2A-Extensions` 为逗号分隔列表(specification.md:485, 2258, 3326)。`A2A-Extensions: A,B` 时 `Requested(A)` 与 `Requested(B)` 均为 false;只有多行同名头才被识别 | 其他 SDK 或手写客户端按规范发逗号形式时,x402 扩展不会被激活;若卡片声明 `required: true` 且启用 `WithCapabilityChecks`,请求会被 `ErrExtensionSupportRequired` 拒绝(intercepted_handler.go:328-342)| 实测 `probe/probe_test.go` `TestCommaSeparatedExtensions`:逗号形式 `requestedA=false requestedB=false`,多行形式均为 true |
| S2 | a2asrv/jsonrpc.go:136,379;rest.go:63,493 | JSON-RPC/REST 响应不写 `A2A-Extensions` 头;只有 gRPC 在 trailer 中回显已激活扩展(a2agrpc/v1/handler.go:313-319)。扩展主题文档要求响应 SHOULD 列出已激活扩展(Refs/a2a/docs/topics/extensions.md:177-179)| 客户端无法得知 x402 是否已被服务端激活 | 代码阅读;实测 `respHeaderExt=[]` |
| S3 | a2asrv 全包无 `A2A-Version` 校验(grep `SvcParamVersion` 仅见于客户端 a2aclient/client.go:270)| 服务端不检查版本。规范要求不支持的版本 MUST 返回 `VersionNotSupportedError`(specification.md:737)| 协议演进时无法对旧/新客户端给出明确错误 | 实测 `probe/version_test.go`:`A2A-Version: 9.9` 返回 200 与正常结果 |
| S4 | a2asrv/jsonrpc.go:63-64;rest.go:103;全仓库 grep `MaxBytes`/`LimitReader` 无结果 | 请求体无大小上限 | 本机或远端入站均可用超大请求消耗内存 | 实测 `TestNoBodyLimit`:64 MiB 请求体被完整解析并返回 200 |
| S5 | a2asrv/jsonrpc.go:48-84 | 不检查 `Content-Type`,不处理 CORS。浏览器可对 127.0.0.1 发出无预检的 `text/plain` 跨源 POST,请求会被执行,响应因无 `Access-Control-Allow-Origin` 而不可读 | 用户浏览的任意网页可以向本机 daemon 的 A2A 接口投递任务(CSRF 类)| 实测 `probe/csrf_test.go`:`Content-Type: text/plain` + `Origin: https://evil.example` 返回 200 且执行 |
| S6 | a2asrv/agentcard.go:174-183 | 卡片 handler 回显任意 `Origin` 并设置 `Access-Control-Allow-Credentials: true` | 卡片按设计是公开的;但若本机卡片包含本机信息,任意网页可读取 **[推断]** | 代码阅读 |
| S7 | a2asrv/rest.go:50;e2e/tck/sut.go:75,169 | REST handler 的 tenant 处理与 TCK SUT 的 REST 端点标为 TODO | 若用 tenant 承载"目标 agent",REST 需用 `NewTenantRESTHandler` | 代码阅读 |

S1–S5 在 anet 侧的规避方式(均为一层 `http.Handler` 中间件,位于 SDK handler 之前)**[推断]**:
- 把 `A2A-Extensions` 与 x402 v0.2 使用的旧名 `X-A2A-Extensions`(a2a-x402-spec-v0.2.md:426-428)统一改写为多个 `A2A-Extensions` 值;SDK 已有同类转换 `a2av0.ToServiceParams`(a2acompat/a2av0/conversions.go:34-47),但导入它会拉入 grpc,应自行实现约 15 行等价代码。
- 在响应头写入已激活扩展:可在 `CallInterceptor.After` 中取 `callCtx.Extensions().ActivatedURIs()`,但拦截器拿不到 `http.ResponseWriter`,需要中间件与拦截器通过 context 协作,或直接向上游提交修复。
- 校验 `A2A-Version`(缺省按 0.3 处理,规范 specification.md:712)。
- `http.MaxBytesHandler`。
- 本机接口要求 bearer 令牌,并校验 `Host` 为 127.0.0.1/localhost(防 DNS rebinding)、拒绝带非空 `Origin` 的请求或仅接受 `application/json`。

---

## 4. `a2acrypto`:AgentCard 签名

### 4.1 能力(代码阅读 + 已有测试)

- **格式**:JWS Flattened 结构,`protected` 头含 `alg`、`kid`、`typ: "JOSE"`,可选 `jku`(a2acrypto/sign.go:86-93);签名输入为 `b64url(protected) + "." + b64url(payload)`(sign.go:100-102);与规范 8.4.2 一致(specification.md:2068-2100)。
- **规范化**:`canonicalizeJSON` 解析原始 JSON(`UseNumber`),删除顶层 `signatures`,按 RFC 8785 输出:键按 UTF-16 码元排序(canonical.go:185-195)、数字按 ECMAScript `Number::toString`(canonical.go:218-240)、字符串仅转义 `"`、`\` 与控制字符(canonical.go:129-183)。
- **算法**:由私钥类型推断,Ed25519 → `EdDSA`,P-256/384/521 → `ES256/384/512`,RSA → `RS256`(alg.go:26-43);EdDSA 不做预哈希(sign.go:105-106;alg.go:53-54);ECDSA 的 DER 签名转换为 JWS 的 R||S(sign.go:116-122;ecdsa.go:29-39)。
- **验签**:`Verifier.Verify(ctx, raw json.RawMessage, sig)` 要求 `alg`、`kid` 非空(verify.go:75-80),通过 `KeyResolver.ResolveKey(ctx, kid, untrustedJKU)` 取公钥(verify.go:86;keyresolver.go:44-48),支持 Ed25519、ECDSA、RSA PKCS#1 v1.5(verify.go:107-145)。
- **信任根**:`KeyResolverFunc` 以 kid 查本地可信密钥(keyresolver.go:51-60);`JWKSKeyResolver` 仅从调用方给出的 URL 白名单拉取 JWKS,拒绝白名单外的 `jku`(keyresolver.go:62-100)。JWKS 解析支持 `OKP/Ed25519`(jwks.go:42-53),EC 点做曲线校验(jwks.go:92-102)。
- **跨 SDK 金标**:`testdata/golden.json` 由 a2a-python 参考 SDK 生成(a2acrypto_test.go:27;testdata/gen_golden.py),用固定 Ed25519 种子逐字节比对 `protected` 与 `signature`(a2acrypto_test.go:39-66),并验证经 Go 结构体往返后签名不变(a2acrypto_test.go:68-94)。另有 `TestSignAndVerifyEd25519`(sign_test.go:101)。副本中 `go test ./...` 全部通过 **[实测]**。
- **服务端集成**:`NewSignedCardProducer`(signed_card.go:35);**客户端集成**:`agentcard.Resolver.Verifier`(a2aclient/agentcard/resolver.go:69),对接收到的原始字节验签(resolver.go:214)。

### 4.2 与 anet 身份的对接

- `identity.Controller.CurrentPrivateKey()` 返回 `ed25519.PrivateKey`(ANetCore/identity/identity.go:233),该类型实现 `crypto.Signer`,可直接用于 `a2acrypto.NewSigner(SignerConfig{PrivateKey: k, KeyID: ...})`(sign.go:30-70),算法自动推断为 EdDSA。
- **[推断]** `kid` 可编码为 `did:anet:<AID>#<key_state_seq>`(`DID()` 见 identity.go:220),验签端的 `KeyResolverFunc` 通过 KEL `Replay`(identity.go:369)取对应序号的公钥,从而与 KEL 轮换对齐,不依赖 `jku`。
- **[推断]** identity.go:228-232 的注释已经指出该私钥被 anet 对象签名与传输层复用;再用于 JWS 会增加一个使用同一密钥的协议。JWS 签名输入以 base64url 头部开头,与 anet 的 CBOR 预映像在结构上不同,碰撞风险低但非零。可选方案是用 `Controller.Delegate`(identity.go:248)委托一把专用于卡片签名的子密钥。

### 4.3 缺口

| # | 位置 | 行为 | 影响 | 发现方式 |
|---|---|---|---|---|
| C1 | — | 已撤下:安全问题,按协调披露处理,细节不在此公开。 | — | — |
| C2 | a2acrypto/verify.go:99-104,107-145 | `alg` 只用来选哈希,实际验签按公钥类型分派;头部 `alg` 与公钥类型不做一致性检查(RFC 8725 §3.1 建议检查)| 公钥来自验签方可信根,无法据此伪造签名 **[推断]**;属于实现规范性问题 | 代码阅读 |
| C3 | a2acrypto 全包无 `crit` 处理 | 未拒绝包含未知 `crit` 头的签名(RFC 7515 §4.1.11 为 MUST)| 规范性问题 | 代码阅读(grep 无结果)|
| C4 | a2acrypto/keyresolver.go:120;a2aclient/agentcard/resolver.go:173 | JWKS 与卡片响应用 `io.ReadAll` 读取,无大小上限 | 恶意端点可返回超大响应 | 代码阅读 |
| C5 | a2a/agent.go:46,53,57,86(无 `omitempty`);signed_card.go:50 | Go 对 nil 切片输出 `null`:`{"supportedInterfaces":null,...,"skills":null}` **[实测]** `cardjson/main.go`。签名生产者对 `json.Marshal(card)` 的结果做规范化,因此 `null` 进入签名载荷;规范示例要求必需字段为 `[]`(specification.md:2040-2066)| 其他 SDK 若从解析后的 protobuf 重建规范形式,会得到不同载荷,验签失败 **[推断]**。anet 构造卡片时所有必需切片应初始化为非 nil | 实测 + 代码阅读 |
| C6 | a2aclient/agentcard/resolver.go:106-112,180-198 | `Resolve` 接受 `file://` URL 并读取本地文件 | 若 anet 把来自对端的 URL 直接交给解析器,可读取本机文件。调用前需限定 scheme | 代码阅读 |

---

## 5. `a2aext`:扩展声明与激活

- **声明**:扩展写在 `AgentCard.Capabilities.Extensions []AgentExtension{URI, Description, Params, Required}`(a2a/agent.go:18-30,106-119)。
- **客户端激活**:`a2aext.NewActivator(uris...)` 是 `a2aclient.CallInterceptor`,仅当目标卡片声明了该扩展时把 URI 追加到 `A2A-Extensions`(a2aext/activator.go:26-53)。卡片为 nil(客户端由 `AgentInterface` 直接创建)时视为支持全部扩展(a2aext/utils.go:23-28)。
- **服务端激活**:在 `CallInterceptor` 中调用 `a2asrv.ExtensionsFrom(ctx)`,用 `Requested(ext)` 判断,`Activate(ext)` 登记(a2asrv/extensions.go:31-70)。完整示例是 `e2e/extensions_durations_test.go:30-66`:拦截器在 `Before` 中激活并记录时间,在 `After` 中通过 `a2a.MetadataCarrier` 写入响应元数据。
- **必需扩展**:`WithCapabilityChecks(&caps)` 后,卡片中 `Required: true` 的扩展未被请求时返回 `ErrExtensionSupportRequired`(intercepted_handler.go:328-342)。
- **传播**:`NewServerPropagator` / `NewClientPropagator` 把入站请求中与扩展相关的元数据和请求头带到出站调用(a2aext/propagator.go:84-137,148-206),适用于"本 agent 收到请求后再委托给下游 agent"的链路。

x402 的落地方式 **[推断]**:x402 v0.2 的负载放在 `task.status.message.metadata` 与 `message.metadata` 的 `x402.payment.*` 键中(a2a-x402-spec-v0.2.md:42-43,398-408),状态机借用 `input-required`(同文件 161)。这与 SDK 的执行器/拦截器模型直接对应:商家侧执行器在未付款时发出 `TaskStateInputRequired` 状态事件并在消息元数据中带 `x402.payment.required`;客户端侧用 `NewActivator(x402URI)` 激活,用 `a2aclient.CallInterceptor` 处理 `payment-required` 并提交 `payment-submitted`。需要处理两处不匹配:
1. x402 v0.2 规定激活头为 `X-A2A-Extensions`(a2a-x402-spec-v0.2.md:428),而 1.0 服务端只读 `A2A-Extensions`(a2a/svcparams.go:23;extensions.go:65),见 3.5 S1 的规避方式。
2. x402 规范示例中的状态字符串 `input-required` 是 0.3 形式;1.0 的 JSON 线协使用 ProtoJSON 枚举名(specification.md:1215),即 `TASK_STATE_INPUT_REQUIRED`。

---

## 6. `a2aclient`:出站客户端

- **创建**:`a2aclient.NewFromCard(ctx, card, opts...)` / `NewFromEndpoints`(factory.go:68-76)。默认启用 JSON-RPC 与 REST 两种传输,JSON-RPC 优先(factory.go:62-64)。按协议版本(先新后旧)与 `Config.PreferredTransports` 排序候选,主版本相同可回退(factory.go:179-230)。
- **传输接口**(a2aclient/transport.go:30-72):

```go
type Transport interface {
    GetTask(context.Context, ServiceParams, *a2a.GetTaskRequest) (*a2a.Task, error)
    ListTasks(context.Context, ServiceParams, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error)
    CancelTask(context.Context, ServiceParams, *a2a.CancelTaskRequest) (*a2a.Task, error)
    SendMessage(context.Context, ServiceParams, *a2a.SendMessageRequest) (a2a.SendMessageResult, error)
    SubscribeToTask(context.Context, ServiceParams, *a2a.SubscribeToTaskRequest) iter.Seq2[a2a.Event, error]
    SendStreamingMessage(context.Context, ServiceParams, *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error]
    GetTaskPushConfig(...); ListTaskPushConfigs(...); CreateTaskPushConfig(...); DeleteTaskPushConfig(...)
    GetExtendedAgentCard(context.Context, ServiceParams, *a2a.GetExtendedAgentCardRequest) (*a2a.AgentCard, error)
    Destroy() error
}
type TransportFactory interface {
    Create(ctx context.Context, card *a2a.AgentCard, iface *a2a.AgentInterface) (Transport, error)
}
```

- **自定义绑定**:`WithTransport(protocol, factory)` / `WithCompatTransport(version, protocol, factory)` 注册任意协议名(factory.go:250-260);`TransportProtocol` 明确是开放字符串,"MUST NOT be treated as an enum"(a2a/agent.go:185-187)。因此 anet 可以在卡片中声明一个自定义绑定,并为它注册工厂。
- **拦截器**:`a2aclient.CallInterceptor{Before, After}`(middleware.go:100-109),请求结构带 `Card`、`ServiceParams`、`Payload`(middleware.go:63-76);`AuthInterceptor` + `CredentialsService` 按卡片安全方案注入凭据(auth.go:56-150)。
- **HTTP 客户端**:`WithJSONRPCTransport(httpClient)` / `WithRESTTransport(httpClient)`(jsonrpc.go:40-47;rest.go:57-68)。传 nil 时使用 `http.Client{Timeout: 3 * time.Minute}`(transport.go:26;jsonrpc.go:63-64;rest.go:50-52)。`http.Client.Timeout` 覆盖读取响应体的时间,因此默认客户端会在 3 分钟时中断 SSE 流 **[推断,依据 net/http 文档语义]**;长时流应传入无 `Timeout` 的客户端,用 context 控制期限。SSE 单行上限 10 MiB(internal/sse/sse.go:37-39)。
- **卡片信任**:`CreateFromCard` 假定卡片可信(factory.go:85-87);anet 的卡片验证由自己的验证器负责,要求签名存在且验证通过。
- **经 anet E2E 通道复用 JSON-RPC 客户端 [推断]**:`jsonrpcTransport` 只调用 `httpClient.Do`(jsonrpc.go:114,148)。给它一个自定义 `http.RoundTripper`,把 `*http.Request` 封装进 E2E 加密信封、把远端响应以流式 `Body` 返回,即可让标准 JSON-RPC 客户端(包括 SSE 流)经 hub 中转而 hub 看不到内容,无需重新实现 11 个方法。

---

## 7. 嵌入 anet 的方式(与"hub 仅做传输"约束对照)

以下为基于上述已验证接口的设计推论,均为 **[推断]**。

1. **本机 A2A 接口(127.0.0.1)**:`a2asrv.NewJSONRPCHandler` + `NewRESTHandler` + 卡片 handler,前置 anet 中间件(令牌、Host/Origin 校验、`MaxBytesHandler`、扩展头规范化、版本校验)。外部 A2A 客户端(编码工具)只与本机 daemon 通信。
2. **本机 agent 作为服务方(入站)**:远端 daemon 发来的 E2E 信封在本机解密后,有两种投递方式:
   - (a) 直接调用 `RequestHandler` 方法。调用前用 `a2asrv.NewCallContext(ctx, a2asrv.NewServiceParams(hdrs))` 建立调用上下文并写入 `callCtx.User = NewAuthenticatedUser(peerAID, ...)`;`InterceptedHandler` 会复用已存在的 `CallContext`(intercepted_handler.go:290-306;middleware.go:36-39)。
   - (b) 把解密后的 JSON-RPC 字节交给 `NewJSONRPCHandler(h).ServeHTTP`,使用实现了 `http.Flusher` 的内存 `ResponseWriter`(SSE 写入要求 `Flusher`,internal/sse/sse.go:49-55)。该方式与本机接口共用同一套线协代码,语义差异最小。
3. **远端 agent 作为服务方(出站)**:`a2aclient` + 第 6 节的自定义 `RoundTripper`,或注册自定义 `TransportFactory`。
4. **路由到"哪一个远端 agent"**:SDK 的 `Tenant` 字段贯穿所有请求类型(a2a/core.go:786-908;a2a/tenant.go:24-32;AgentInterface.Tenant 在 a2a/agent.go:133),REST 可用 `NewTenantRESTHandler` 从路径提取(rest.go:69-98)。可以为每个远端 agent 生成一张本机代理卡片,其 `supportedInterfaces[].url` 指向 `http://127.0.0.1:<port>/agents/<AID>/...` 或在 `tenant` 中携带 AID。该方案未经验证,是否与 tenant 的原意("agent owner")冲突需要在设计阶段确认。
5. **build tag**:A2A 接口本身、`a2acrypto` 签名、0.3 兼容层、gRPC 应分属不同 tag。按第 2 节数据,HTTP 形式的 A2A 约 1.0–1.7 MiB 且无 grpc;0.3 兼容层和 gRPC 各约 7–8 MiB 且引入 grpc/protobuf。0.3 兼容层适合做加法 tag 或独立减法 tag,由产品决定默认是否包含。新 tag 需同步 `.github/workflows/ci.yml` 矩阵(CLAUDE.md)。
6. **默认安全**:SDK 本身不提供"拒绝全部委托"的开关;默认内存存储 + 匿名调用即可执行任务(3.4)。anet 的"新装不接受任何委托"需要在 anet 层实现(例如入站 `CallInterceptor.Before` 对未授权对端直接返回 `ErrUnauthenticated`)。

---

## 8. 一致性与互通测试

### 8.1 `e2e/`(进程内)

- `e2e/*.go` 为 SDK 自身的 Go 测试:流式、panic、并发取消、扩展、tenant 传播、推送配置往返(e2e/jsonrpc_test.go:33-104;cancellation_test.go:33-178;extensions_durations_test.go:68;tenant_propagation_test.go:30;push_configs_test.go:36)。它们用 `httptest` 在进程内启动 SDK 服务端,不能指向外部服务。
- 副本中 `go test -count=1 ./...` 全部通过,无失败 **[实测]**。上游 CI 以 `-race -shuffle=on` 运行,nightly 另跑 `-count=100`(.github/workflows/go.yaml:44;nightly.yaml:21-24)。

### 8.2 `e2e/tck/` + 外部 `a2aproject/a2a-tck`(可对 anet 运行)

- `run_tck.sh` 调用 `orchestrate_tck.py`(e2e/tck/run_tck.sh:17-20)。脚本克隆 `https://github.com/a2aproject/a2a-tck.git`,用 uv 建虚拟环境并安装 pytest、grpcio 等依赖,启动 Go SUT(`go run . --mode <protocol>`),轮询 `http://localhost:9999/.well-known/agent-card.json` 直到 200,然后执行
  `./run_tck.py --sut-url http://localhost:9999 --category mandatory|capabilities --transports jsonrpc|grpc`,结束时 POST `/quit`(orchestrate_tck.py 中 `TCK_REPO`、`start_and_test`、`main`)。
- TCK 本身是黑盒 HTTP 测试,只需要 SUT URL 与卡片,因此可以直接指向 anet daemon 的本机接口 **[推断]**。TCK 仓库未在 `Refs/` 中,本次未运行。
- Go SUT 只提供 JSON-RPC 与 gRPC,REST 为 TODO(e2e/tck/sut.go:75,169);其执行器固定为 submitted → working → completed,各间隔 1 秒(e2e/tck/sut_agent_executor.go)。
- 本仓库 CI 没有运行 TCK 的 workflow(`.github/workflows/` 中无 tck 任务)。

### 8.3 `itk/` + `a2aproject/a2a-itk`(跨 SDK 互通矩阵)

- `itk` 是独立 Go 模块,`replace` 指向上级目录(itk/go.mod),同时依赖旧模块 v0.3.15 与 grpc。
- 运行:安装 Podman,设置 `A2A_ITK_REVISION`,执行 `itk/run_itk.sh`;脚本克隆 `a2a-itk`、生成 protobuf 桩、构建镜像并运行共享场景(itk/README.md:1-70;run_itk.sh:12-53)。由于 v2 与 v0 模块在全局 protobuf 注册表中注册同名消息,需要设置 `GOLANG_PROTOBUF_REGISTRATION_CONFLICT=warn`(run_itk.sh:18-20)。
- ITK 代理从消息中解析 protobuf `Instruction` 并转调其他 SDK 的代理(itk/main.go 中 `V10AgentExecutor`、`handleInstruction`)。anet 若要进入该矩阵,需要实现一个 ITK 代理并向 `a2a-itk` 注册,属于上游贡献范围 **[推断]**。
- **ACTS**:`acts/sut-behaviors.yaml` 列出 `tck-*` 前缀行为(完成、input-required、auth-required、拒绝、失败、直接 Message、多轮、取消、长任务、文本/数据/文件/URL 工件、流式),按消息首段文本前缀分派(itk/acts_behaviors.go:96-109)。上游 CI 中该任务 `continue-on-error: true`,注释为 "the driver gates on `must` conformance here, which no SDK passes yet"(.github/workflows/acts.yaml:66-71)。

### 8.4 对 anet 联调的建议 [推断]

在 anet 中实现一个只用于测试的执行器,覆盖 `acts/sut-behaviors.yaml` 的行为前缀,挂在 daemon B 上;由 daemon A 的本机接口对外暴露 B 的代理卡片;用 a2a-tck 对 daemon A 的 127.0.0.1 URL 运行。这样一次测试同时覆盖:本机 A2A 线协一致性、E2E 加密通道、hub 仅转发。该测试可以归入 `scripts/joint.sh` 或 `scripts/scenario.sh`。

---

## 9. 许可证、版本、稳定性

- **许可证**:Apache License 2.0(LICENSE;README.md:3,149-151),源文件头为 "Copyright 2025 The A2A Authors"(例如 a2asrv/doc.go:1)。仓库根目录无 NOTICE 文件 **[实测]**。直接依赖 `google/uuid`、`golang.org/x/sync`、`golang.org/x/mod` 为 BSD-3-Clause。anet 采用 "ANet Community License"(ANet/LICENSE:1-4),再分发时需要保留 Apache-2.0 许可文本与版权声明(Apache-2.0 §4)。与 anet 许可证的兼容性判断属于法务问题,本报告不下结论。
- **版本**:模块路径带 `/v2`(CHANGELOG.md:148);1.0.0 于 2026-03-17 发布(CHANGELOG.md:142),随后 2.0.1(03-24)、2.1.0(04-01)、2.2.0(04-10)、2.2.1(04-29)、2.3.0(05-12)、2.3.1(05-13)、2.4.0(07-28)、2.5.0(08-18)、2.6.0(09-25)(CHANGELOG.md:3-123)。发布由 release-please 驱动,tag 形如 `vX.Y.Z`(docs/ai/RELEASE.md:15-40)。
- **稳定性**:
  - minor 版本中出现过默认行为变化,例如 v2.4.0 默认开启推送 SSRF 防护(CHANGELOG.md:49)。
  - `WithClusterMode` 标注为实验性(handler.go:186)。
  - `a2acrypto` 在 v2.6.0 首次发布,同版本即有一次规范化修正(CHANGELOG.md:8,14)。
  - 上游 ACTS 一致性尚未作为合并门槛(8.3)。
  - anet 应固定到具体版本,并在升级时重跑 a2a-tck 与本仓库的契约测试 **[推断]**。
- **安全报告渠道**:GitHub Security Advisories(SECURITY.md:3-6)。
- **贡献流程**:较大改动需先开 issue 讨论(CONTRIBUTING.md;README.md:141-145)。

---

## 10. 可向上游提交的修复

与"贡献 A2A、不另起炉灶"的决定一致,以下问题在 anet 侧规避后,适合作为上游 issue/PR:

1. 服务端按逗号拆分 `A2A-Extensions`(S1),并兼容旧名 `X-A2A-Extensions`(x402 v0.2 使用)。
2. JSON-RPC/REST 响应回显已激活扩展(S2),与 gRPC 行为对齐。
3. 服务端校验 `A2A-Version` 并返回 `VersionNotSupportedError`(S3)。
4. `TransportOption` 增加请求体上限(S4);JSON-RPC 要求 `Content-Type: application/json`(S5)。
5. 验签检查 `alg` 与密钥类型一致并处理 `crit`(C2、C3);JWKS/卡片读取加上限(C4)。
6. 签名生产者在规范化前按规范 8.4.1 处理 nil 必需切片(C5),或在文档中说明必须初始化。
7. `a2asrv/doc.go` 示例中的 API 名称修正(3.1)。
8. 为 TCK SUT 补上 REST 端点(S7)。
9. 提供一个 anet 的 ITK 代理,进入 a2a-itk 跨 SDK 矩阵(8.3)。

---

## 11. 未决问题

1. 通过本机代理卡片 + tenant 表达"目标远端 agent"是否符合 tenant 的设计意图,还是应使用一个自定义 `protocolBinding`(第 7 节第 4 点)。
2. 0.3 兼容层是否进入默认构建:它带来约 8.2 MiB 与 grpc 依赖,但现存 0.3 客户端(包括 x402 v0.2 示例所用的 `X-A2A-Extensions` 形式)需要它才能直接连接。
3. AgentCard 签名密钥使用 AID 当前密钥还是委托子密钥(4.2)。
4. a2a-tck 的当前版本对 1.0 REST 绑定的覆盖范围(本次未克隆运行)。
5. 其他语言 SDK 的验签实现是对原始 JSON 规范化,还是先解析为 protobuf 再规范化;这决定 C5 是否会在实际互通中导致验签失败。
