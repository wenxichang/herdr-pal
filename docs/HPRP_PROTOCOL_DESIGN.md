# Herdr Pal Relay Protocol 设计

## 1. 文档状态

本文定义 Herdr Pal Relay Protocol（HPRP）的首个公开协议设计。它用于连接
Herdr Pal 客户端与多租户 Relay Server，使两端能够独立发布、升级和替换实现。

首个公开主版本命名为 `HPRP/1`。该版本不承诺兼容 Herdr Pal 当前内部使用的
Relay Protocol 3；正式实现应通过独立端点、显式配置或迁移窗口完成切换，不能把
两套协议混在同一条连接中。

本文使用“必须”“禁止”“应该”“可以”表达规范性要求，其含义遵循
[RFC 8174](https://www.rfc-editor.org/rfc/rfc8174.html)。WebSocket 和 JSON 分别遵循
[RFC 6455](https://www.rfc-editor.org/rfc/rfc6455.html) 与
[RFC 8259](https://www.rfc-editor.org/rfc/rfc8259.html)。

## 2. 目标与非目标

### 2.1 目标

- Pal 与 Server 可以独立升级，新增非核心能力不要求同步部署。
- 通过稳定目标引用避免命令误发到已经替换的 Agent 会话。
- 在断线、重连、重复投递和执行结果不确定时保持保守、可诊断的行为。
- 允许第三方实现 Pal 或 Server，并能通过公开测试验证互操作性。
- 认证、路由、终端文本和状态通知具有清晰边界。

### 2.2 非目标

- HPRP 不暴露 Herdr 本地 Socket，也不替代 Herdr 公共本地 API。
- HPRP 不把终端快照定义为结构化 LLM 消息或完整对话历史。
- HPRP 不定义 IM 平台 webhook、用户领钥页面、计费或组织管理接口。
- HPRP 不提供自动审批权限请求的能力。
- HPRP/1 不提供离线通知存储和保证送达。

## 3. 兼容性模型

HPRP 采用“稳定核心 + 能力协商”模型。

### 3.1 主版本协商

WebSocket Upgrade 使用 `Sec-WebSocket-Protocol` 协商不兼容的主版本：

```http
Sec-WebSocket-Protocol: herdr-pal-relay.v2, herdr-pal-relay.v1
```

客户端按偏好从高到低列出支持版本，服务端选择双方共同支持的最高主版本。没有
共同版本时，服务端拒绝 Upgrade。WebSocket 子协议一旦选定，连接期间不得改变。

软件版本与协议版本相互独立。例如 Pal `v3.2.0` 可以继续实现 `HPRP/1`。

### 3.2 主版本内扩展

双方在 hello 中交换 capability，仅允许使用交集中的可选能力。Capability 名称
带有不可变版本，例如：

```text
session.delta.v1
notification.ack.v1
command.output.v1
```

`.v2` 表示新能力，不得改变 `.v1` 的语义。实现停止支持某项可选能力时，可以不再
声明它，但同一主版本的基础能力不得删除。

以下属于兼容变更：

- 增加可选字段；
- 增加稳定错误码；
- 增加需要协商的消息类型或 capability；
- 增加未知时可以降级为 `unknown` 的枚举值。

以下变更必须提升 HPRP 主版本：

- 修改已有字段、错误码或消息的语义；
- 增加所有实现都必须理解的新必填字段；
- 改变认证、安全、消息顺序或幂等保证；
- 删除或弱化当前主版本的基础能力。

## 4. 传输与编码

- HPRP/1 只允许通过 WSS 传输，不定义明文 WS 部署模式。
- 每条 WebSocket text message 必须包含一个完整 JSON object。
- Binary message 不属于 HPRP/1，收到后应以“不支持的数据类型”关闭连接。
- JSON 必须是有效 UTF-8；同一 object 中存在重复字段名时必须拒绝。
- 接收方必须忽略未知的可选字段；发送方只能发送规范字段或已经协商的扩展字段。
- 消息大小、快照会话数、并发请求数和输出分段大小由 `hello.server` 公布上限。
- 超过服务端硬限制的消息必须拒绝，不能通过截断后继续解释。

连接应同时使用 WebSocket ping/pong 和应用侧 watchdog。任一方长时间收不到有效帧或
pong 时都应主动断开，避免服务端保留已经失效的机器会话。

## 5. 公共消息信封

每条 HPRP 消息使用以下信封：

```json
{
  "protocol": "HPRP/1",
  "type": "session.snapshot",
  "id": "msg-01J...",
  "reply_to": "msg-01H...",
  "must_understand": false,
  "payload": {}
}
```

字段规则：

- `protocol`：固定为当前连接选择的协议，例如 `HPRP/1`。
- `type`：小写点分名称，发布后语义不得改变。
- `id`：发送方生成的非空唯一消息标识；在其幂等和审计窗口内不得复用。
- `reply_to`：响应消息引用原请求 `id`，非响应消息省略。
- `must_understand`：可选布尔值，默认 `false`。未知消息类型在其为 `false` 时忽略，
  为 `true` 时返回错误。
- `payload`：始终为 JSON object；无参数消息也使用空 object。

HPRP 遵循“宽容读取、严格写入”：未知可选字段不得导致失败，但缺少基础必填字段、
字段类型错误或违反当前状态机必须被明确拒绝。

## 6. 终端凭据认证

HPRP 应用消息中不存在登录、领钥、刷新 token 或 `userid` 声明。认证在 WebSocket
HTTP Upgrade 阶段完成：

```http
Authorization: Bearer hpk_<key-id>_<secret>
Sec-WebSocket-Protocol: herdr-pal-relay.v1
```

Key 由协议外的管理平面签发，每把 Key 唯一绑定一个用户和一台逻辑机器：

```json
{
  "credential_id": "cred_7F2Q9",
  "principal_id": "user-internal-id",
  "machine_id": "office-pc",
  "status": "active",
  "expires_at": null
}
```

认证规则：

- Secret 至少包含 256 位安全随机熵，只在签发时展示一次。
- 服务端保存可定位的 Key ID、Secret 摘要和绑定信息，不保存可直接使用的明文 Secret。
- 服务端必须使用常量时间比较验证摘要。
- Key 无效、过期或已吊销时统一返回 HTTP `401`，避免泄露凭据是否存在。
- 凭据有效但没有连接权限时返回 `403`，限流返回 `429`，临时不可用返回 `503`。
- 若 `(principal_id, machine_id)` 已有活动连接，返回 `409` 并拒绝新连接。
- 旧连接只有在 heartbeat/watchdog 判定失效并移除后，才能接受新连接。
- 吊销活动 Key 时，服务端应立即关闭对应连接并移除该机器的全部会话。
- 允许为同一机器签发轮换 Key，但同一时刻仍只能存在一条活动连接。
- 日志不得记录完整 Key；审计使用 `credential_id`、`principal_id` 和 `machine_id`。

Pal 应从权限受限的配置文件或环境变量读取 Key。Bearer Key 的“每机绑定”属于管理
意义上的逻辑绑定；复制 Key 仍可以在其他物理设备上使用。HPRP/1 不提供硬件证明、
终端私钥签名或 mTLS 设备身份。

## 7. 连接状态机

连接依次经历以下状态：

```text
WSS_CONNECTED
    -> NEGOTIATING
    -> SYNCHRONIZING
    -> READY
    -> CLOSING / DISCONNECTED
```

HTTP Upgrade 成功即表示终端凭据已经认证，因此 HPRP 帧内没有额外的
`AUTHENTICATING` 阶段。

### 7.1 Hello

Pal 必须把 `hello.client` 作为首条 HPRP 消息：

```json
{
  "protocol": "HPRP/1",
  "type": "hello.client",
  "id": "hello-1",
  "payload": {
    "implementation": {
      "name": "herdr-pal",
      "version": "0.2.0",
      "os": "linux",
      "arch": "amd64"
    },
    "capabilities": ["command.output.v1"],
    "limits": {
      "max_receive_message_bytes": 1048576,
      "max_inflight_commands": 8,
      "idempotency_window_ms": 600000
    },
    "diagnostics": {
      "hostname": "devbox"
    }
  }
}
```

`diagnostics` 只用于日志，不构成可信身份。客户端不得通过 hello 覆盖 Key 绑定的用户或
机器。

服务端返回 `hello.server`：

```json
{
  "protocol": "HPRP/1",
  "type": "hello.server",
  "id": "hello-2",
  "reply_to": "hello-1",
  "payload": {
    "connection_id": "conn-01J...",
    "machine_id": "office-pc",
    "capabilities": ["command.output.v1"],
    "limits": {
      "max_message_bytes": 1048576,
      "max_sessions": 256,
      "max_inflight_commands": 8,
      "max_output_bytes": 262144,
      "idempotency_window_ms": 600000
    },
    "heartbeat": {
      "ping_interval_ms": 20000,
      "idle_timeout_ms": 60000
    }
  }
}
```

返回的 capability 是双方能力交集，`limits` 是结合双方声明和服务端策略计算出的
连接有效限制。客户端必须遵守服务端返回的有效值。`idempotency_window_ms` 表示 Pal
承诺识别重复 `idempotency_key` 的最短窗口，服务端不得假定更长的保护时间。

### 7.2 初始同步

hello 完成后，Pal 必须发送一份完整 `session.snapshot`。服务端确认该快照前，连接不
进入 `READY`，双方不得发送业务命令或通知。空机器也必须上报 `sessions: []`。

任一时刻断线，服务端立即从可路由目录中移除该连接的全部会话。重连后必须重新从
hello 和完整快照开始，不能假定服务端保留了旧连接状态。

## 8. 会话与目标模型

协议明确区分以下概念：

- `machine_id`：服务端凭据记录中的逻辑机器标识。
- `slot_id`：机器内稳定的物理执行位置，对应 Herdr pane，使用不透明字符串。
- `session_id`：slot 当前承载的逻辑 Agent 会话标识，使用不透明字符串。
- `display_index`：面向用户的当前列表序号，只用于展示，禁止参与命令路由。

执行 `/clear`、替换 Agent 或发生其他逻辑会话变化时，`session_id` 必须变化，即使
`slot_id` 没有变化。`session_id` 不要求使用哈希，也不是安全凭据。

稳定命令目标同时包含三层身份：

```json
{
  "machine_id": "office-pc",
  "slot_id": "w1:p1",
  "session_id": "session-opaque-id"
}
```

服务端和 Pal 必须同时校验三者，禁止根据 `display_index` 猜测或重建目标。

## 9. 会话快照

```json
{
  "protocol": "HPRP/1",
  "type": "session.snapshot",
  "id": "snapshot-12",
  "payload": {
    "sequence": 12,
    "sessions": [
      {
        "slot_id": "w1:p1",
        "session_id": "session-opaque-id",
        "display": {
          "index": 1,
          "agent": "codex",
          "workspace": "test",
          "title": "main-codex"
        },
        "status": "working"
      }
    ]
  }
}
```

快照规则：

- 快照是当前连接所绑定机器的原子、权威、完整会话视图。
- 会话项不能声明其他 `machine_id`，机器身份由连接隐含。
- `sequence` 在单次连接内严格递增；允许跳号，因为每份快照都是完整替换。
- 相同序号视为幂等重发，服务端返回相同应用结果；较小序号返回
  `sync.stale_snapshot`。
- 服务端返回 `session.snapshot.result` 和 `applied_sequence`。
- Pal 必须等待确认后，才能引用新出现或身份已经变化的会话。
- 快照中消失的会话立即从该机器的路由目录移除。
- 基础状态为 `idle`、`working`、`blocked`、`done` 和 `unknown`。未知状态按
  `unknown` 处理，不应导致连接失败。

未来可以协商 `session.delta.v1` 发送增量。增量必须包含 `base_sequence`；服务端
基准不一致时返回 `sync.resync_required`，Pal 随后发送完整快照。完整快照始终是
HPRP/1 的恢复机制。

## 10. 命令执行

服务端使用 `command.execute` 向指定会话发送文本指令：

```json
{
  "protocol": "HPRP/1",
  "type": "command.execute",
  "id": "command-42",
  "payload": {
    "idempotency_key": "im-message-01J...",
    "target": {
      "machine_id": "office-pc",
      "slot_id": "w1:p1",
      "session_id": "session-opaque-id"
    },
    "content": {
      "type": "text/plain",
      "text": "继续处理"
    }
  }
}
```

`content.text` 表示传给 Pal 命令处理层的原始文本。Server 可以先处理全局命令，Pal
继续负责需要访问本地 Herdr 或终端状态的命令。终端按键仍必须经过 Pal 的策略检查，
协议不会把普通 prompt 自动提升为审批或高风险操作。

Pal 返回一次 `command.result`：

```json
{
  "protocol": "HPRP/1",
  "type": "command.result",
  "id": "result-42",
  "reply_to": "command-42",
  "payload": {
    "outcome": "ok",
    "content": {
      "type": "text/plain",
      "text": "消息已发送。"
    }
  }
}
```

`outcome` 只能为：

- `ok`：Pal 已经按命令定义完成接收或本地处理；对于 prompt，这不表示 Agent 已经
  完成后续工作；
- `rejected`：命令未执行，例如权限或目标校验失败；
- `failed`：已确定执行失败；
- `indeterminate`：无法确认是否已经产生副作用。

协商 `command.output.v1` 后，`command.result` 之后可以发送零条或多条
`command.output`。同一命令的输出 `sequence` 必须递增，重复序号必须去重，最后
一段设置 `final: true`。长输出不能阻塞 heartbeat 或其他命令结果。未协商该能力时，
Pal 只能通过 `command.result` 返回当前命令的同步结果。

服务端不得因超时或 `indeterminate` 自动创建新命令重试。需要安全重发时必须继续
使用原 `idempotency_key`；Pal 应在 hello 确认的幂等窗口内返回原结果或继续同一
执行，而不是重复产生副作用。超过该窗口后，服务端必须把重发视为可能产生重复
副作用的操作，并要求上层重新确认。

### 10.1 会话替换

如果命令导致同一 slot 产生新逻辑会话，Pal 必须：

1. 上报包含新 `session_id` 的快照；
2. 等待 `session.snapshot.result`；
3. 在 `command.result` 中返回 `replacement_target`。

```json
{
  "replacement_target": {
    "machine_id": "office-pc",
    "slot_id": "w1:p1",
    "session_id": "new-session-id"
  }
}
```

服务端只允许自动接受同一 `machine_id` 和同一 `slot_id` 的替换目标。其他变化返回
`target.session_changed`，不能自动改选。

## 11. 状态与终端通知

Pal 使用 `notification.event` 上报 Agent 状态变化和需要用户关注的终端文本：

```json
{
  "protocol": "HPRP/1",
  "type": "notification.event",
  "id": "event-01J...",
  "payload": {
    "event_key": "office-pc:w1:p1:89",
    "target": {
      "machine_id": "office-pc",
      "slot_id": "w1:p1",
      "session_id": "session-opaque-id"
    },
    "kind": "agent.status",
    "sequence": 89,
    "status": "blocked",
    "content": {
      "type": "text/plain",
      "text": "终端最近输出"
    }
  }
}
```

通知规则：

- 服务端必须确认目标存在于该连接最近确认的快照中。
- 新会话的通知只能在对应快照确认后发送。
- `event_key` 在去重窗口内保持唯一；`sequence` 用于同一目标事件排序和去重。
- 未知 `kind` 在 `must_understand` 为 `false` 时忽略。
- 通知内容是终端快照或状态说明，不是结构化 LLM 对话记录。
- HPRP/1 基础通知采用连接在线期间的尽力投递，不提供断线后的离线回放。
- 双方协商 `notification.ack.v1` 后，可以使用相同 `event_key` 确认和重试。

## 12. 统一结果与错误模型

失败结果使用稳定机器码和可选展示信息：

```json
{
  "protocol": "HPRP/1",
  "type": "command.result",
  "id": "result-42",
  "reply_to": "command-42",
  "payload": {
    "outcome": "rejected",
    "error": {
      "code": "target.session_changed",
      "message": "目标会话已经变化",
      "retryable": false,
      "details": {}
    }
  }
}
```

- 程序只能根据 `code`、`outcome`、`retryable` 和可选 `retry_after_ms` 决策。
- `message` 只用于日志和用户展示，可以本地化，禁止解析其文本决定行为。
- `details` 是可选扩展 object，未知字段必须忽略。
- 未知错误码按 `outcome` 保守处理；不得默认重试。
- 错误码发布后语义不得改变。

首批稳定错误码：

```text
protocol.invalid_message
protocol.unsupported_type
protocol.required_extension_unsupported
protocol.limit_exceeded
sync.stale_snapshot
sync.resync_required
target.not_found
target.session_changed
target.not_ready
command.unsupported
command.denied
command.timeout
command.execution_failed
server.busy
server.internal
```

业务错误不得关闭连接。非法 JSON、重复字段、消息超过硬限制、协议状态机违规或持续
发送无法理解的必需消息属于连接级错误。服务端应在可以安全解析信封时先发送
`protocol.error`，随后使用合适的 WebSocket close code 关闭连接。

## 13. 顺序、背压与恢复

- WebSocket 保证单连接内帧顺序，但业务处理可以并发；响应必须通过 `reply_to` 关联。
- hello 协商得到 `max_inflight_commands`，超过限制时返回 `server.busy`，不能无限
  排队。
- 输出和通知队列必须有界；任何队列溢出都要产生可诊断错误或指标，禁止静默丢弃
  命令结果。
- heartbeat、关闭帧和请求结果优先于大块终端输出。
- 断线时所有未完成请求进入 `indeterminate`，除非实现能够证明其尚未执行。
- 重连创建新 `connection_id`，重新执行 hello 和完整会话快照。
- Server 不把旧连接的选择、快照序号或在途命令直接移植到新连接。

## 14. 安全边界

- 所有部署必须使用加密的 WSS；证书信任策略属于产品部署配置，不参与 HPRP 消息
  兼容性判断。
- 实现应该验证服务端证书。为兼容当前内网自签名部署，Herdr Pal 产品可以保留跳过
  证书链验证的兼容默认值，但必须支持开启严格验证，并在跳过验证时输出明确告警。
- 服务端必须限制认证失败频率、消息大小、快照数量、并发命令和输出速率。
- 日志不得包含完整凭据、Cookie 或未经处理的大段敏感终端内容。
- 服务端只能向 Key 所属用户和机器的连接路由命令。
- Pal 必须继续执行本地 PolicyGuard；服务器发来的内容不是自动授权。
- 未绑定目标、已经变化的 `session_id` 和未知命令动作默认拒绝。

## 15. 公开制品与一致性测试

HPRP/1 发布时应同时提供：

- 中文规范正文及明确版本号；
- 核心消息 JSON Schema；
- 错误码和 capability 注册表；
- 合法与非法消息的固定测试向量；
- hello、快照、命令、会话替换、断线重连的状态机测试轨迹；
- 不依赖 Herdr 和 IM 网络的内存参考客户端与服务端；
- 跨版本兼容测试矩阵。

一致性测试至少覆盖：

- 未知可选字段被忽略；
- 未知可选消息不导致断线；
- 未协商 capability 不会被使用；
- 重复消息、重复结果和重复通知能够去重；
- 过期快照和基准不一致增量触发明确错误；
- 目标会话替换后旧命令不会误发；
- 超时和不确定结果不会被不安全地自动重试；
- 新旧实现只共享 HPRP/1 基础能力时仍可互操作。

任何实现只要通过 HPRP/1 基础一致性测试，就应该能够与其他合规 HPRP/1 实现建立
连接。可选扩展不一致时必须降级，不能影响基础会话上报和命令执行。

## 16. 标准治理

- HPRP 主版本规范一经发布即保持不可变；勘误只能澄清，不得改变线上语义。
- 错误码、消息类型和 capability 由公开注册表管理，新增项需要说明降级行为。
- 第三方私有扩展应使用明确的厂商命名空间，禁止占用公共名称。
- 实验性能力不得在未协商时发送，也不得成为同一主版本的隐含必需依赖。
- 参考实现的缺陷不能自动改变规范；如需改变规范性行为，应发布新主版本。

## 17. 实施边界

后续实施应按以下独立模块推进：

- `hprp`：信封、消息类型、JSON Schema、校验和错误注册表；
- `credential`：Upgrade 认证、Key 摘要、吊销和轮换；
- `connection`：状态机、heartbeat、能力协商和背压；
- `catalog`：完整快照、稳定目标和会话替换；
- `command`：请求关联、幂等、结果和分段输出；
- `notification`：状态事件、去重和可选确认；
- `conformance`：测试向量、fake client/server 和跨版本验证。

当前内部 Relay Protocol 3 应保持冻结，仅用于迁移期间兼容既有 Pal。HPRP/1 使用新的
类型和连接状态机实现，不在旧结构上继续堆叠条件分支。
