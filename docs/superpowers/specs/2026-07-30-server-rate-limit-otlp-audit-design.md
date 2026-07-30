# Server 输入限速与 OTLP 审计设计

## 1. 目标

本设计为 `herdr-pal-server` 增加两个相互独立但共享企业微信入口上下文的能力：

1. 按企业微信用户限制输入处理频率，默认每秒最多接受 1 条、滚动 60 秒内最多接受
   20 条。
2. 对用户输入和终端文本输出生成结构化审计事件，可通过 OTLP Logs 发送到外部审计系统，
   并可选同步输出到 stderr 便于调试。

两个能力都只在 Server 中实现。Pal、Herdr、本地 HPRP 身份和企业微信机器人连接方式不变。

## 2. 非目标

- 不限制 Pal 状态通知、终端快照结果或 Server 主动发送消息的频率。
- 不把普通运行日志、HPAP 管理日志全部迁移到 OTLP 审计流。
- 不审计 PNG、base64 图片数据或其他二进制内容。
- 不提供磁盘审计队列、离线补发或 exactly-once 保证。
- 不因审计器不可用而拒绝、延迟或回滚用户操作。
- 不在首版支持 OTLP/gRPC、CloudEvents、Syslog、OCSF 或多个 OTLP 目标。
- 不通过 HPAP 动态修改限速或审计配置；变更配置后需要重启 Server。

## 3. 总体架构

```text
企业微信单聊文本
  → 基础身份与消息 ID 校验
  → 企业微信消息幂等去重
  → UserRateLimiter
  → user_input 审计事件
  → UserExecutor
  → ConversationRouter / ClientHub / Pal

终端内容准备发送
  → 企业微信回复或推送
  → terminal_output 审计事件

AuditEvent
  ├── AsyncAuditor → StderrAuditor
  └── AsyncAuditor → OTLPAuditor → OTLP/HTTP Protobuf Collector
```

限速器、审计事件模型、审计输出和 Router 集成保持单一职责：

- `UserRateLimiter` 只判断用户是否可以接受一条新输入。
- `AuditEvent` 只描述发生的审计事实，不包含传输行为。
- `StderrAuditor` 和 `OTLPAuditor` 只负责各自的编码与发送。
- `ConversationRouter` 只决定何时生成输入或终端输出事件。

## 4. Server 配置

服务端配置增加两个顶层对象：

```json
{
  "rate_limit": {
    "per_second": 1,
    "per_minute": 20
  },
  "audit": {
    "type": "otlp",
    "endpoint": "https://otel-collector.example:4318/v1/logs",
    "skip_verify": false,
    "stderr": true
  }
}
```

### 4.1 限速配置

| 字段 | 缺省值 | 含义 |
| --- | ---: | --- |
| `rate_limit.per_second` | `1` | 同一用户滚动 1 秒内最多接受的唯一输入数 |
| `rate_limit.per_minute` | `20` | 同一用户滚动 60 秒内最多接受的唯一输入数 |

配置规则：

- 字段缺失时使用缺省值。
- 显式配置为 `0` 时关闭对应窗口。
- 两个字段都是 `0` 时使用无操作限速器。
- 负数或大于 `10000` 的值属于配置错误。
- 配置结构必须使用可区分“缺失”和“显式零值”的可选整数，不能用普通 `int` 的零值推断。

### 4.2 审计配置

| 字段 | 缺省值 | 含义 |
| --- | --- | --- |
| `audit.type` | `none` | 只接受 `none` 或 `otlp` |
| `audit.endpoint` | 空 | `otlp` 使用的完整 `/v1/logs` HTTP(S) 地址 |
| `audit.skip_verify` | `false` | HTTPS 时跳过服务端证书验证 |
| `audit.stderr` | `false` | 同时向 stderr 输出一行一个事件的 JSON |

组合语义：

| `type` | `stderr` | 生效输出 |
| --- | --- | --- |
| `none` | `false` | 不审计 |
| `none` | `true` | 只输出 stderr JSON Lines |
| `otlp` | `false` | 只发送 OTLP |
| `otlp` | `true` | 同时发送 OTLP 和 stderr |

严格校验规则：

- `type=otlp` 时 `endpoint` 必填，必须是没有 URL userinfo、query 或 fragment 的绝对
  `http` 或 `https` URL，且路径必须是 `/v1/logs`。HTTPS 是正式部署默认选择；HTTP 只用于
  受控本机或内网 Collector。
- `type=none` 时配置非空 `endpoint` 或 `skip_verify=true` 属于错误，避免产生“已经配置但未生效”
  的误解。
- `skip_verify=true` 只允许用于 HTTPS。
- OTLP Header 从标准环境变量 `OTEL_EXPORTER_OTLP_LOGS_HEADERS` 读取，格式遵循
  OpenTelemetry 的逗号分隔 `key=value` 约定。解析失败时 Server 启动失败。
- 配置错误和运行日志不得输出 Header 值、URL userinfo 或其他审计认证信息。

## 5. 用户输入限速

### 5.1 检查顺序

`ConversationRouter.Handle` 使用以下固定顺序：

1. 校验单聊类型、`principal_id` 和 `message_id`。
2. 使用现有企业微信消息幂等器忽略重复 `message_id`。
3. 调用 `UserRateLimiter.Allow(principalID, now)`。
4. 生成一条 `herdr_pal.user_input` 审计事件。
5. 超限时立即回复等待时间；允许时提交到 `UserExecutor`。

重复投递不是新的用户输入，不占限速额度，也不重复生成正文审计事件。

所有唯一输入都受同一规则限制，包括：

- `/userid`、`/help` 和 `/ls`。
- `/N`、`/sel N`、`/mode` 和定向前缀。
- `/con`、分页、按键和 `/slash`。
- 普通 Agent prompt。
- 语法错误或当前没有可用会话的输入。

### 5.2 滚动窗口算法

每个用户保存一组已经接受的时间戳。一次判断在同一互斥区内完成：

1. 删除不再属于任一启用窗口的时间戳。
2. 分别统计 `[now-1s, now]` 和 `[now-60s, now]` 中的已接受记录。
3. 任一启用窗口达到上限时拒绝本次输入。
4. 只有允许的输入才追加时间戳。

拒绝的尝试不追加时间戳，避免用户重试不断延长等待时间。如果两个窗口同时超限，
`retry_after` 取两个窗口恢复时间的较大值，并向上取整到秒，最少显示 1 秒。

用户状态按最后一个仍有效时间戳清理。限速器每分钟至多执行一次全表惰性清理，删除已经没有
有效时间戳的用户，不启动独立后台 goroutine。

系统时钟向后跳动时，未来时间戳仍视为窗口内记录；限速器不得产生负的等待时间或绕过限制。

### 5.3 用户提示和运行日志

统一提示：

```text
输入过于频繁，请在 N 秒后重试。
```

运行日志只记录用户摘要、消息摘要、触发窗口、当前限制和等待毫秒数，不记录输入正文。

## 6. 审计事件模型

审计模型版本固定为 `1`。内部使用强类型 `AuditEvent`，stderr 使用等价 JSON 表示，OTLP
输出映射为一个 LogRecord。

正文事件只有两类：

| EventName | Body | 产生时机 |
| --- | --- | --- |
| `herdr_pal.user_input` | 脱敏后的完整用户输入 | 完成身份、幂等和限速判断后，进入执行队列前 |
| `herdr_pal.terminal_output` | 脱敏后的完整终端审计文本 | 企业微信终端内容发送流程完成后 |

### 6.1 公共字段

| 字段 | 含义 |
| --- | --- |
| `schema_version` | 审计事件模型版本，当前为 `1` |
| `event_id` | 启用 `crypto/rand` 生成的 128 位十六进制 ID |
| `event_name` | `herdr_pal.user_input` 或 `herdr_pal.terminal_output` |
| `timestamp` | 业务事件发生时间 |
| `observed_timestamp` | Server 构造审计事件的时间 |
| `principal_id` | 真实企业微信用户 ID |
| `bot_id_hash` | Bot ID 的安全摘要 |
| `message_id_hash` | 企业微信消息 ID 摘要；无对应消息时为空 |
| `request_id_hash` | 企业微信回复关联 ID 摘要；无对应请求时为空 |
| `action` | Router 解析出的操作类型或输出来源 |
| `outcome` | 输入准入或终端投递结果 |
| `machine_id` | 稳定目标机器标识；尚未解析目标时为空 |
| `pane_id` | 稳定目标 pane ID；尚未解析目标时为空 |
| `session_id_hash` | Agent occupant/session 摘要 |
| `presentation` | `txt` 或 `img`；输入事件为空 |
| `delivery` | `reply` 或 `push`；输入事件为空 |
| `content_bytes` | 脱敏后正文 UTF-8 字节数 |
| `body` | 脱敏后的完整用户输入或终端审计文本 |

`event_id` 在事件第一次构造时生成。OTLP 重试和 stderr 并行输出必须复用同一个 ID，使外部
系统可以去重；重新执行同一业务操作会产生新的 ID。

### 6.2 用户输入事件

`event_name` 固定为 `herdr_pal.user_input`，`body` 保存用户输入正文。

`outcome` 只使用：

- `accepted`：通过限速并准备进入用户队列。
- `rate_limited`：被秒级或分钟级窗口拒绝。

限速事件额外提供：

- `limit.window`：`second`、`minute` 或 `second,minute`。
- `limit.retry_after_ms`。
- `limit.per_second` 和 `limit.per_minute` 的生效值。

输入事件发生在业务执行之前，因此 `accepted` 只表示 Server 接受处理，不表示 Pal 已成功
执行。后续用户可见终端输出由独立输出事件记录。

### 6.3 终端输出事件

`event_name` 固定为 `herdr_pal.terminal_output`，`body` 始终使用 HPRP 终端内容中的文本：

- 文本模式记录 `Content.Text`。
- 图片模式忽略 PNG/base64，只记录图片配对的审计文本 `Content.Text`。
- 图片失败降级为文本时仍只生成一个输出事件。
- Markdown 分片前生成一个逻辑事件，不按企业微信分段重复记录。

`outcome` 只使用：

- `delivered`：全部必需企业微信发送步骤成功。
- `delivery_failed`：首段、后续 Markdown、图片或降级文本的最终发送失败。

图片先发送标题再发送 PNG。如果 PNG 失败但文本降级发送成功，最终结果为 `delivered`，
`presentation` 记录最终的 `txt`；如果降级也失败，则为 `delivery_failed`。

`action` 标明终端内容来源，例如 `select_console`、`command_result`、`command_output` 或
`status_notification`。它不保存普通帮助、确认、配置错误或其他非终端 Markdown。

## 7. 内容安全

审计的目标是保留用户输入和终端文本，而不是复制 Server 持有的凭据。正文进入审计事件前
经过统一的最小凭据脱敏：

- 精确替换当前 Bot Secret 和当前 OTLP Header 值。
- 替换符合 Herdr Pal 机器 Key 格式的 `hpk_...` token。
- 替换常见 `Authorization: Bearer ...`、`Cookie: ...` 和 `Set-Cookie: ...` 头值。
- 不执行自然语言级语义脱敏，不尝试猜测普通密码、源码内容或业务数据。

因此审计流仍包含敏感的用户和终端正文，必须限制 Collector、stderr 和下游存储的访问权限。
`audit.stderr=true` 只用于受控调试，不应在共享日志平台长期启用。

普通 `slog` 运行日志继续遵守现有规则，不记录 prompt、终端正文、完整用户 ID 或审计 Header。

## 8. Auditor 接口与异步投递

### 8.1 接口

业务层只依赖：

```go
type Auditor interface {
	Emit(event AuditEvent)
	Shutdown(ctx context.Context) error
}
```

`Emit` 必须非阻塞且不返回业务错误。具体实现自行记录丢弃和传输故障。

实现包括：

- `NoopAuditor`：没有任何输出。
- `StderrAuditor`：把一个事件编码成一行 JSON。
- `OTLPAuditor`：把事件映射为 OTLP LogRecord 并发送。
- `MultiAuditor`：向多个独立异步输出复制同一个不可变事件。

每个实际输出由独立 `AsyncAuditor` 包装，具有自己的有界队列和 worker。OTLP 的网络阻塞或
重试不能阻塞 stderr，也不能阻塞 Router。

### 8.2 固定资源限制

首版不暴露性能调优配置，使用以下固定值：

- 每个输出最多排队 1024 条事件。
- 每个输出最多占用 16 MiB 排队字节；达到任一上限即丢弃新事件。
- 最长每 1 秒刷新一次。
- 单批最多 64 条或约 512 KiB；单个大事件可以独占一批。
- 单次 OTLP HTTP 请求超时 5 秒。
- 一个批次的最大重试存活时间为 30 秒。
- Server 关闭时最多等待 5 秒刷新审计输出。

队列字节按事件 JSON 等价大小保守估算，必须包含正文，不能只统计结构体固定开销。

### 8.3 故障处理

审计始终 fail-open：

- 事件 ID 生成失败、正文编码失败、队列满、网络错误、TLS 错误、OTLP 拒绝或关闭超时都不
  改变用户操作结果。
- 每个输出维护累计入队、成功、重试、丢弃和最后错误原因。
- 普通日志以限频方式报告累计丢弃数和稳定错误类型，不包含事件正文或认证 Header。
- 故障恢复后记录一条恢复日志及故障期间累计丢弃数。
- 不把失败事件写入本地磁盘，也不在 Server 重启后补发。

## 9. OTLP Logs 映射

`OTLPAuditor` 使用官方 `opentelemetry-proto` 生成的 Go 类型，直接实现 OTLP/HTTP
Protobuf，不把业务层绑定到当前仍处于 Beta 的 OpenTelemetry Go Logs SDK。

固定映射：

- HTTP 方法：`POST`。
- Content-Type：`application/x-protobuf`。
- URL：配置的完整 `/v1/logs` endpoint。
- Resource：`service.name=herdr-pal-server`、`service.version`、`host.name`、`process.pid`。
- Instrumentation Scope：`github.com/wenxichang/herdr-pal/audit`，版本使用当前程序版本。
- LogRecord `EventName`：审计事件 `event_name`。
- LogRecord `Body`：审计事件 `body` 字符串。
- `TimeUnixNano` 和 `ObservedTimeUnixNano`：对应两个时间字段。
- `SeverityNumber`：正常准入和投递为 INFO，限速和投递失败为 WARN。
- 其余字段使用 `herdr_pal.audit.*` 命名空间写入 Attributes。
- 不设置 TraceID、SpanID 或 TraceFlags。

HTTP 行为遵循 OTLP 规范：

- 2xx 完整成功后确认整个批次。
- partial success 不重试，记录 `rejected_log_records` 和安全错误摘要。
- 429、502、503、504 和连接级临时错误可以重试。
- 遵循合法的 `Retry-After`，否则使用带随机抖动的指数退避。
- 其他 4xx 视为不可重试并丢弃该批次。
- 重试复用原始事件及 `event_id`，不得重新生成审计记录。

## 10. Router 集成点

### 10.1 输入

输入审计只在 `ConversationRouter.Handle` 的幂等和限速边界生成一次。Router 不把正文传给
普通 logger，也不要求 `UserExecutor` 或具体命令处理器重复审计。

限速拒绝仍使用企业微信请求关联回复；审计入队成功与否不影响回复。

### 10.2 终端输出

终端输出审计集中在结构化终端内容发送边界：

- `sendContentReply` 覆盖命令首段终端回复、选择后 `/con` 和按键回显。
- `sendContentPush` 覆盖 `command.output` 和状态通知拉取的终端内容。
- 图片发送和文本降级在同一发送流程结束后确定最终 `presentation` 和 `outcome`。

审计事件使用发送前的完整 `Content.Text`，而不是装饰后的 `[终端输出#N]` 标题，也不包含
当前会话告警和企业微信 Markdown 分片标记。

## 11. 生命周期

Server 启动顺序增加：

1. 严格加载并校验限速和审计配置。
2. 创建审计内容脱敏器。
3. 按配置创建 OTLP、stderr 或 no-op Auditor。
4. 创建限速器和 Router。
5. 启动企业微信、Relay 和 Admin 组件。

Auditor 创建失败属于配置或启动失败；运行后发送失败不影响服务。

Server 关闭时先停止接收新企业微信输入，再在总关闭窗口内调用 `Auditor.Shutdown`。审计
刷新超时只记录告警，不改变最终关闭流程。

## 12. 测试策略

### 12.1 配置

- 缺省限速得到 `1/20`。
- 显式零关闭单个或全部窗口。
- 负数、超大数、未知审计类型和无效 endpoint 被拒绝。
- `type=otlp` 缺少 endpoint 被拒绝。
- `type=none` 携带 OTLP 专用字段被拒绝。
- `OTEL_EXPORTER_OTLP_LOGS_HEADERS` 正确解析且错误信息不泄漏值。

### 12.2 限速器

- 秒级与分钟级滚动窗口边界。
- 两个窗口同时超限时选择较长等待时间。
- 不同用户互相隔离。
- 拒绝记录不增加窗口计数。
- 时钟回退不绕过限制。
- 过期用户惰性清理。
- 并发调用和 `-race`。

### 12.3 Router 限速集成

- `/userid`、`/help`、普通 prompt、定向命令和无效命令都计入限速。
- 重复 `message_id` 不计数、不重复审计。
- 限速发生在 `UserExecutor` 前，超限输入不会调用 Relay。
- 超限提示包含正确的向上取整秒数。
- Auditor 队列满或失败时用户操作仍正常。

### 12.4 审计模型和安全

- 输入正文和终端文本完整进入事件。
- 图片只记录配对文本，不包含 PNG、base64 或图片字节。
- 终端 Markdown 分片只生成一个事件。
- 图片失败降级记录最终展示形式和投递结果。
- Bot Secret、`hpk_...`、Authorization 和 Cookie 头被脱敏。
- 普通日志不出现审计正文、完整 principal ID 或 OTLP Header。

### 12.5 StderrAuditor

- 每个事件严格输出一行合法 JSON。
- 换行和控制字符正确转义。
- stderr 和 OTLP 使用相同 `event_id`。
- stderr 写入失败不会阻塞 Router。

### 12.6 OTLPAuditor

- 本地 fake Collector 能解码 Protobuf 并验证 Resource、Scope、EventName、Body 和 Attributes。
- Header、HTTPS、自签名证书和 `skip_verify`。
- 成功、partial success、不可重试 4xx、可重试状态、`Retry-After` 和连接失败。
- 批量事件数和字节边界。
- 队列满、重试过期和关闭刷新。
- OTLP 不可用时企业微信输入与 Relay 执行继续成功。

真实 Herdr 集成测试不需要外部 OTLP 服务，统一使用本地 fake Collector；企业微信继续使用
现有 fake gateway。

## 13. 文档与兼容性

- 更新 `server-config.example.json` 和 README，说明默认限速、零值关闭、OTLP endpoint、
  Header 环境变量和 stderr 敏感性。
- 更新 Bridge 架构和维护交接文档，增加 Server 输入策略和审计边界。
- HPRP/1 消息结构不变；审计只消费 Server 已经拥有的输入和终端文本。
- HPAP/1 不增加动态限速或审计方法。
- 老配置缺少新字段时自动使用限速默认值并关闭审计，因此可以直接升级。
