# Herdr Pal Relay Protocol 设计

## 1. 文档状态

本文定义 Herdr Pal Relay Protocol（HPRP）的首个公开协议设计。它用于连接
Herdr Pal 客户端与多租户 Relay Server，使两端能够独立发布、升级和替换实现。

首个公开主版本命名为 `HPRP/1`。Herdr Pal 参考实现已经切换到该协议，不再包含内部
Relay Protocol 3 的运行时兼容分支。旧 Pal 必须与旧 Server 配套使用，不能把两套协议
混在同一条连接中。

截至 2026-07-29，HPRP/1 尚未发布。本次状态事件、无副作用终端快照和图片内容结构直接
纳入 HPRP/1 基线，不为此前开发版本保留运行时兼容分支。HPRP/1 正式发布后再遵守第 3 节
的主版本兼容规则。

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
- Server 可以根据用户策略按需获取文本或图片终端快照，Pal 不保存 IM 显示模式。
- 允许以用户功能为单位增加扩展，由 Pal 吸收 Herdr API 和实现版本差异。

### 2.2 非目标

- HPRP 不暴露 Herdr 本地 Socket，也不替代 Herdr 公共本地 API。
- HPRP 不提供任意 Herdr `{method, params}` 透传，也不逐项复制 Herdr Socket 方法。
- HPRP 不把终端快照定义为结构化 LLM 消息或完整对话历史。
- HPRP 不定义 IM 平台 webhook、用户领钥页面、计费或组织管理接口。
- HPRP 不提供自动审批权限请求的能力。
- HPRP/1 不提供离线通知存储和保证送达。
- HPRP/1 不定义终端正文的审计存储、保留周期或脱敏策略。

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

双方在 hello 中交换技术 capability 和用户 feature。Capability 表示协议机制，Feature
表示用户可以直接发起并观察结果的功能。两者都必须经过协商后才能使用。

技术 Capability 名称带有不可变版本，例如：

```text
session.delta.v1
notification.ack.v1
command.output.v1
terminal.snapshot.v1
terminal.image.v1
feature.invoke.v1
blob.reference.v1
```

用户 Feature 同样使用带版本的名字，例如：

```text
agent.interact.v1
terminal.inspect.v1
workspace.prepare.v1
task.monitor.v1
```

Capability 和 Feature 名称都使用小写点分段，并以 `.vN` 结尾；`N` 是从 1 开始的正
整数。去掉版本后缀后的名称称为 family，例如 `workspace.prepare`。

`.v2` 表示新版本，不得改变 `.v1` 的语义。对于同一 family 的技术 Capability 或用户
Feature，Pal 可以同时声明多个版本；Server 必须选择双方支持的最高版本，并且只返回
最终选择的一个版本。`.v2` 必须是自包含合同，不得要求对端同时启用 `.v1`。语义独立、
可以同时使用的能力必须使用不同 family，不能仅靠版本号表达组合关系。

实现停止支持某项可选能力或 Feature 时，可以不再声明它，但同一主版本的基础能力不得
删除。某个 Feature 没有共同版本时，只禁用该 Feature，不影响 HPRP 基础连接和其他
Feature。

以下属于兼容变更：

- 增加可选字段；
- 增加稳定错误码；
- 增加需要协商的消息类型或 capability；
- 增加新的独立 Feature；
- 在 Feature 参数中增加默认关闭或需要显式协商的可选模式；
- 增加未知时可以降级为 `unknown` 的枚举值。

以下变更必须提升 HPRP 主版本：

- 修改已有字段、错误码或消息的语义；
- 增加所有实现都必须理解的新必填字段；
- 改变已有结果消息允许的 `outcome` 集合；
- 改变认证、安全、消息顺序或幂等保证；
- 删除或弱化当前主版本的基础能力。

### 3.3 Feature Package

每个公开 Feature 必须作为独立、不可变的 Feature Package 发布。Package 至少定义：

- Feature family、版本和用户目标；
- 输入、结果和事件的 JSON Schema；
- 可用目标类型和目标失效语义；
- 明确的成功条件和用户可观察结果；
- 幂等窗口、重试和结果不确定语义；
- 是否可取消，以及取消后的最终状态；
- 输出、事件、并发和数据量限制；
- Feature 不可用或版本不相交时的降级行为。

Feature 的合同只描述用户意图和可观察结果。Pal 是领域适配器，可以把一次 Feature
调用展开为任意数量的 Herdr snapshot、read、prompt、wait、keys 或其他公共 API 调用。
底层调用顺序、Herdr protocol 版本、重试和兼容分支都不得成为 HPRP 对端依赖。

参考实现中的 Pal 是独立 sidecar，只通过 Herdr 公共本地 Socket API、CLI 和公开 Schema
完成编排。Feature Package 不得要求访问 Herdr 私有 TUI socket、内部状态或未公开模块。

Pal 只有在当前 Herdr 环境中能够完整实现某个 Feature 合同时，才能声明支持该 Feature。
以后从多次 Herdr 调用改为一个原子调用，不需要提升 Feature 版本；只有输入、输出、
成功条件或其他用户可观察语义发生不兼容变化时，才发布新版本。

## 4. 传输与编码

- HPRP/1 只允许通过 WSS 传输，不定义明文 WS 部署模式。
- 每条 WebSocket text message 必须包含一个完整 JSON object。
- Binary message 不属于 HPRP/1，收到后应以“不支持的数据类型”关闭连接。
- JSON 必须是有效 UTF-8；同一 object 中存在重复字段名时必须拒绝。
- 接收方必须忽略未知的可选字段；发送方只能发送规范字段或已经协商的扩展字段。
- 消息大小、快照会话数、并发请求数和输出分段大小由 `hello.server` 公布上限。
- 超过服务端硬限制的消息必须拒绝，不能通过截断后继续解释。

HPRP/1 的普通 JSON 消息适合文本和小型结构化数据。需要传输文件、图片或大型日志的
Feature 不得绕过帧大小限制，也不应把大对象直接内嵌为无界 base64。此类能力应协商
独立的 `blob.reference.v1` 等技术 capability，由 HPRP 消息携带对象标识、大小、摘要和
内容类型，具体数据通过该 capability 定义的有界分块或独立数据通道传输。

`terminal.image.v1` 是上述规则的一个有界例外：它允许在终端内容对象中内嵌一张 PNG，
但解码后的图片、配对纯文本和完整 JSON 帧必须分别满足 hello 协商的限制。参考实现把
单张 PNG 限制为 512 KiB、配对纯文本限制为 256 KiB，完整帧仍不得超过 1 MiB。超过限制
的图片不得拆成多条没有原子关系的终端结果；未来如需更大内容，应另行协商
`blob.reference.v1`。

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
- `reply_to`：响应消息引用原请求 `id`；与请求关联的进度或输出事件也可以引用该请求。
  不关联其他消息时省略。
- `must_understand`：可选布尔值，默认 `false`。未知消息类型在其为 `false` 时忽略，
  为 `true` 时返回错误。
- `payload`：始终为 JSON object；无参数消息也使用空 object。

HPRP 遵循“宽容读取、严格写入”：未知可选字段不得导致失败，但缺少基础必填字段、
字段类型错误或违反当前状态机必须被明确拒绝。

所有期待响应的可选扩展请求必须设置 `must_understand: true`。未知的必需请求必须立即
返回 `protocol.required_extension_unsupported`，不能静默忽略并等待发送方超时。仅供
观察、丢失后可由快照恢复的可选事件可以使用默认值 `false`。

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
  "credential_id": 7,
  "principal_id": "user-internal-id",
  "machine_id": "office-pc",
  "status": "enabled",
  "allowed_sources": ["192.168.1.20"],
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
    "capabilities": [
      "command.output.v1",
      "terminal.snapshot.v1",
      "terminal.image.v1",
      "feature.invoke.v1"
    ],
    "features": {
      "terminal.inspect.v1": {
        "parameters": {
          "max_lines": 500,
          "supports_paging": true
        }
      },
      "workspace.prepare.v1": {
        "parameters": {
          "supported_agents": ["codex", "claude"],
          "supports_worktree": true
        }
      }
    },
    "limits": {
      "max_receive_message_bytes": 1048576,
      "max_inflight_commands": 8,
      "max_inflight_features": 4,
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
    "capabilities": [
      "command.output.v1",
      "terminal.snapshot.v1",
      "terminal.image.v1",
      "feature.invoke.v1"
    ],
    "features": {
      "terminal.inspect.v1": {
        "parameters": {
          "max_lines": 300,
          "supports_paging": true
        }
      },
      "workspace.prepare.v1": {
        "parameters": {
          "supported_agents": ["codex"],
          "supports_worktree": true
        }
      }
    },
    "limits": {
      "max_message_bytes": 1048576,
      "max_sessions": 256,
      "max_inflight_commands": 8,
      "max_inflight_features": 4,
      "max_output_bytes": 262144,
      "max_terminal_text_bytes": 262144,
      "max_terminal_image_bytes": 524288,
      "idempotency_window_ms": 600000
    },
    "heartbeat": {
      "ping_interval_ms": 20000,
      "idle_timeout_ms": 60000
    }
  }
}
```

返回的 capability 是双方技术能力交集；`features` 对每个 family 只包含最终选择的最高
共同版本。Feature 参数是结合 Pal 实际能力、Server 实现和服务端策略计算出的有效值，
只能描述用户层功能和限制，禁止列出 Herdr Socket 方法或内部调用步骤。

`terminal.snapshot.v1` 表示 Server 可以向 Pal 请求指定稳定目标的无副作用终端快照。
`terminal.image.v1` 表示该快照或命令终端输出可以包含有界 PNG；它必须与
`terminal.snapshot.v1` 一起协商。没有协商图片能力时，显式图片请求必须返回错误，默认
图片策略则由 Server 降级为文本请求。

Feature Package 必须为每个可协商参数定义类型、默认值和合并规则。未知参数必须忽略，
缺失参数使用 Package 默认值；`hello.server` 只返回本连接实际生效的参数。hello 完成后，
本连接的 capability、Feature 版本和参数保持不变；运行时能力变化必须断线重连并重新
协商，除非未来另行协商专用的动态更新 capability。

`limits` 是连接级有效限制。客户端必须遵守服务端返回的值。
`max_terminal_text_bytes` 和 `max_terminal_image_bytes` 分别限制终端内容对象中的 UTF-8
纯文本字节数和 Base64 解码后的 PNG 字节数；它们不替代 `max_message_bytes`。
`idempotency_window_ms` 表示 Pal 承诺识别重复 `idempotency_key` 的最短窗口，服务端
不得假定更长的保护时间。

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
    "output_mode": "txt",
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

`output_mode` 是必填字段，只能为 `txt` 或 `img`，表示本次命令产生终端内容时使用的格式。它不在
Pal 中保存模式，也不影响普通文本确认。`img` 必须已经协商 `terminal.image.v1`；没有
协商时返回 `terminal.image_unsupported`。Server 必须为每次命令显式发送当前模式。
`command.result` 和 `command.output` 中的 `text/plain` 确认不受该字段约束；一旦返回
`terminal.snapshot`，其 `mode` 必须与原命令的 `output_mode` 完全一致，接收方必须按
`reply_to` 保存的请求上下文校验，不能只校验内容对象自身是否合法。

`command.execute` 是 HPRP/1 基础 Agent 交互的专用消息，不是未来 Feature 的通用 RPC
入口。新增用户功能不应继续向该消息加入与文本交互无关的参数，而应使用第 11 节的
Feature 调用模型。

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

```json
{
  "protocol": "HPRP/1",
  "type": "command.output",
  "id": "output-42-1",
  "reply_to": "command-42",
  "payload": {
    "target": {
      "machine_id": "office-pc",
      "slot_id": "w1:p1",
      "session_id": "session-opaque-id"
    },
    "sequence": 1,
    "final": true,
    "content": {
      "type": "text/plain",
      "text": "终端后续输出"
    }
  }
}
```

`reply_to` 必须引用原始 `command.execute` 的 `id`。`target` 必须与该命令当前有效目标一致；
发生第 10.2 节的合法会话替换后，后续输出使用已经确认的替换目标。首段序号为 1，之后
必须连续递增。重复分段可以忽略，跳号、未知命令、过期命令或目标不一致必须拒绝。

服务端不得因超时或 `indeterminate` 自动创建新命令重试。需要安全重发时必须继续
使用原 `idempotency_key`；Pal 应在 hello 确认的幂等窗口内返回原结果或继续同一
执行，而不是重复产生副作用。超过该窗口后，服务端必须把重发视为可能产生重复
副作用的操作，并要求上层重新确认。

Pal 的实现必须把轻量幂等索引与大块响应正文分离：在承诺窗口内不得因图片或输出正文
占满缓存而驱逐 Key。响应正文可以在明确的字节预算下省略可选内容，但必须保留等价输入
指纹、原始 outcome、错误码和替换目标，从而识别重复和冲突并禁止再次执行。幂等索引达到
实现容量时，Pal 必须在产生本地副作用前以 `server.busy` 拒绝新 Key，不能通过删除窗口内
旧 Key 腾出空间。

Server 本地等待超时后，可以忽略与近期已过期 `reply_to` 精确匹配的迟到
`command.result` 和 `command.output`；未知关联仍是协议错误。迟到结果不得导致整条 Pal
连接断开，也不得触发新的自动执行。

### 10.1 终端内容

普通提示、确认和错误继续使用 `text/plain`。`/con`、`/pageup`、`/pagedn`、按键后自动
刷新等终端输出使用以下终端内容对象；该对象可以出现在 `command.result.content`、
`command.output.content` 和第 12 节的 `terminal.snapshot.result.content` 中：

```json
{
  "type": "terminal.snapshot",
  "mode": "img",
  "text": "同一页的规范化纯文本",
  "image": {
    "media_type": "image/png",
    "encoding": "base64",
    "data": "iVBORw0KGgo...",
    "width": 1280,
    "height": 720,
    "color_mode": "indexed-256"
  },
  "page": {
    "current": 1,
    "total": 5
  },
  "captured_at": "2026-07-29T10:00:00Z"
}
```

终端内容规则：

- `text` 始终必填，必须是有效 UTF-8，并与可选图片来自同一目标、同一次采集和同一页；
- `mode: txt` 时禁止携带 `image`；
- `mode: img` 时必须携带 `image`，且连接必须已经协商 `terminal.image.v1`；
- 图片固定使用 `media_type: image/png` 和 `encoding: base64`，解码结果必须是合法的索引色
  PNG8，而不只是具有 PNG 签名；
- `width`、`height` 必须为正整数且与 PNG 头一致，单边不得超过 16384，总像素不得超过
  4,080,000；接收方必须在完整解码前使用 PNG 配置执行该检查；`color_mode` 当前只能为
  `indexed-256`；
- `page` 只在内容属于交互分页缓存时出现，`current` 和 `total` 从 1 开始且
  `current <= total`；
- `captured_at` 使用 RFC 3339 UTC 时间；
- 文本、图片和完整消息不得超过 hello 限制，接收方必须在分配大块内存前检查 Base64
  编码长度；
- Server 可以把 `text` 交给审计边界或用于图片失败降级，但 HPRP/1 不要求持久化终端
  正文。

终端标题、全局列表序号、机器标识和当前选择不匹配提示属于 IM 展示信息，由 Server
根据会话目录生成，不绘制进 Pal 返回的终端图片。

### 10.2 会话替换

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

## 11. 用户 Feature 调用

双方协商 `feature.invoke.v1`，并且 hello 已选择具体 Feature 版本后，Server 可以使用
`feature.invoke` 发起用户功能：

```json
{
  "protocol": "HPRP/1",
  "type": "feature.invoke",
  "id": "feature-42",
  "must_understand": true,
  "payload": {
    "feature": "workspace.prepare.v1",
    "idempotency_key": "im-message-01J...",
    "target": {
      "machine_id": "office-pc"
    },
    "input": {
      "repository": "herdr-pal",
      "branch": "feature-x",
      "agent": "codex"
    }
  }
}
```

- `feature` 必须是 hello 最终选择的 Feature 版本。
- `target` 的具体结构由 Feature Package 定义，可以引用机器、稳定 Agent 会话或该
  Feature 自己定义的用户层资源。
- `input` 必须通过对应 Feature Package 的 JSON Schema 校验。
- Server 不得在 `input` 中发送 Herdr method、原始 Socket 请求或要求 Pal 按指定的
  Herdr 调用顺序执行。
- Pal 必须先完成 Feature 的前置条件检查，再开始产生本地副作用。

### 11.1 Feature 结果

每个 `feature.invoke` 必须产生且只产生一个最终 `feature.result`：

```json
{
  "protocol": "HPRP/1",
  "type": "feature.result",
  "id": "feature-result-42",
  "reply_to": "feature-42",
  "payload": {
    "feature": "workspace.prepare.v1",
    "outcome": "ok",
    "result": {
      "content": {
        "type": "text/plain",
        "text": "工作区与 Agent 已准备完成。"
      },
      "data": {
        "workspace": "herdr-pal/feature-x"
      },
      "observed_state": {}
    }
  }
}
```

基础 `outcome` 为：

- `ok`：Feature 定义的用户目标已经达到；
- `rejected`：前置条件、目标或策略校验失败，Feature 未开始执行；
- `failed`：已确定用户目标没有达到；可能已经产生的局部效果必须通过
  `observed_state` 或 Feature 定义的数据字段说明；
- `cancelled`：Pal 已确认停止后续执行；不表示已经撤销此前产生的效果；
- `indeterminate`：Pal 无法确认用户目标或局部效果的最终状态。

`result.data` 和 `observed_state` 的结构由 Feature Package 定义。实现不得用
`indeterminate` 掩盖已知的部分状态，也不得把“部分步骤成功”错误报告为完整 `ok`。

### 11.2 Feature 事件

Feature 执行期间可以发送零条或多条 `feature.event`：

```json
{
  "protocol": "HPRP/1",
  "type": "feature.event",
  "id": "feature-event-42-1",
  "reply_to": "feature-42",
  "payload": {
    "feature": "workspace.prepare.v1",
    "sequence": 1,
    "kind": "progress",
    "content": {
      "type": "text/plain",
      "text": "正在启动 Agent。"
    },
    "data": {}
  }
}
```

- `sequence` 在单次 Feature 调用内从 1 开始严格递增，用于排序和去重。
- `kind`、`content` 和 `data` 的具体语义由 Feature Package 定义。
- Feature Package 可以定义 `accepted`、`progress`、`output`、`attention` 等事件，
  但不能把事件当作最终结果。
- `feature.result` 是与原始 `feature.invoke` 关联的最后一条消息；发送最终结果后禁止
  继续发送该调用的事件。
- Feature 事件不能阻塞 heartbeat、取消请求或其他调用的最终结果。

### 11.3 Feature 取消

Server 使用 `feature.cancel` 请求停止仍在执行的调用：

```json
{
  "protocol": "HPRP/1",
  "type": "feature.cancel",
  "id": "feature-cancel-42",
  "must_understand": true,
  "payload": {
    "invocation_id": "feature-42"
  }
}
```

Pal 必须为每个取消请求返回且只返回一个 `feature.cancel.result`，其 `reply_to` 引用
`feature.cancel` 的 `id`：

```json
{
  "protocol": "HPRP/1",
  "type": "feature.cancel.result",
  "id": "feature-cancel-result-42",
  "reply_to": "feature-cancel-42",
  "payload": {
    "outcome": "ok"
  }
}
```

取消结果只允许 `ok`、`rejected` 和 `failed`。`outcome: ok` 只表示取消请求已被接受，
不代表本地效果已经回滚。Pal 停止继续执行新的步骤后，仍必须为原始
`feature.invoke` 发送最终 `feature.result`；确认停止时使用 `cancelled`，无法确认时使用
`indeterminate`。取消与完成发生竞态时，如果原调用已经产生最终结果，取消结果必须为
`rejected` 并使用 `feature.not_running`，不得改写原调用结果。

Feature Package 必须明确是否可取消、哪些阶段可以停止，以及取消后如何描述已经产生的
用户可观察状态。已经完成、未知或不可取消的调用必须返回稳定 Feature 错误码。

### 11.4 Feature 幂等与恢复

`idempotency_key` 标识一次用户功能调用，而不是某个 Herdr Socket 请求。Pal 展开出的
多个本地操作共享同一个幂等边界：

- 幂等窗口内收到相同 Key 和等价输入时，Pal 必须返回原调用、继续原调用或返回原结果；
- 相同 Key 但 Feature、目标或输入不同，必须返回冲突错误；
- Pal 不得因传输重试重新执行已经完成的底层步骤；
- Pal 重启或 Herdr 断线后，如果无法恢复或核实本地效果，必须返回 `indeterminate`；
- Feature Package 可以定义可核实的恢复步骤，但不能承诺 Herdr 本身没有提供的事务性。

## 12. 状态事件与终端快照

### 12.1 状态事件

Pal 使用 `notification.event` 上报 Agent 状态变化，但不主动附带终端正文：

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
    "kind": "agent.status.changed",
    "sequence": 89,
    "snapshot_sequence": 12,
    "occurred_at": "2026-07-29T10:00:00Z",
    "data": {
      "previous_status": "working",
      "status": "done"
    }
  }
}
```

状态事件规则：

- `agent.status.changed` 的目标必须存在于该连接最近确认的快照中；
- `agent.status.changed` 的 `snapshot_sequence` 必须引用本连接已经确认、且包含该目标的会话
  快照；新会话的状态事件只能在对应快照确认后发送；
- `target.invalidated` 可以在旧目标已经离开本地当前快照后发送，Server 不得要求它仍存在于
  在线目录；其 `snapshot_sequence` 只需引用本连接已确认的同步边界；
- `event_key` 在去重窗口内保持唯一；`sequence` 在同一目标内严格递增，用于排序和去重；
- `previous_status` 和 `status` 使用第 9 节定义的基础状态；重复状态不得产生新事件；
- `notification.event` 不携带终端 `content`，Server 必须自行决定是否需要获取内容；
- `kind` 是当前消息类型的必填枚举；HPRP/1 只接受 `agent.status.changed` 和
  `target.invalidated`，新增事件语义应使用协商能力或新的消息类型；
- HPRP/1 基础事件采用连接在线期间的尽力投递，不提供断线后的离线回放；
- 双方协商 `notification.ack.v1` 后，可以使用相同 `event_key` 确认和重试。

Server 可以根据状态、当前选择、最近交互时间、用户策略和 IM 能力决定忽略事件、只发送
状态说明，或者继续请求终端快照。Pal 不负责这些通知策略。

`target.invalidated` 表示 Pal 已确认目标 pane 关闭或 occupant 被替换。该事件不携带
`data` 或终端正文；Server 应撤销旧目标的选择和显示模式，不能为已失效目标请求快照。

### 12.2 无副作用终端快照

协商 `terminal.snapshot.v1` 后，Server 可以发送 `terminal.snapshot.get`：

```json
{
  "protocol": "HPRP/1",
  "type": "terminal.snapshot.get",
  "id": "terminal-get-01J...",
  "must_understand": true,
  "payload": {
    "target": {
      "machine_id": "office-pc",
      "slot_id": "w1:p1",
      "session_id": "session-opaque-id"
    },
    "mode": "img",
    "purpose": "notification",
    "max_lines": 100
  }
}
```

字段规则：

- 信封 `reply_to` 必须为空，`must_understand` 必须为 `true`；
- `target` 必须是当前连接最近确认快照中的完整稳定目标；
- `mode` 只能为 `txt` 或 `img`，`img` 还要求已经协商 `terminal.image.v1`；
- `purpose` 在 v1 中只能为 `notification`；
- `max_lines` 必须为正整数，且不得超过 hello 中 `terminal.inspect.v1` 的有效限制；
- 请求是只读操作，不使用命令幂等键，但必须与 `command.execute` 合并计入
  `max_inflight_commands`，并受本地执行截止时间和消息大小限制；
- 请求不得重置或修改 `/con`、`/pageup`、`/pagedn` 使用的交互分页缓存。

Pal 返回且只返回一个 `terminal.snapshot.result`：

```json
{
  "protocol": "HPRP/1",
  "type": "terminal.snapshot.result",
  "id": "terminal-result-01J...",
  "reply_to": "terminal-get-01J...",
  "payload": {
    "outcome": "ok",
    "target": {
      "machine_id": "office-pc",
      "slot_id": "w1:p1",
      "session_id": "session-opaque-id"
    },
    "content": {
      "type": "terminal.snapshot",
      "mode": "img",
      "text": "终端最近输出",
      "image": {
        "media_type": "image/png",
        "encoding": "base64",
        "data": "iVBORw0KGgo...",
        "width": 1280,
        "height": 720,
        "color_mode": "indexed-256"
      },
      "captured_at": "2026-07-29T10:00:01Z"
    }
  }
}
```

`outcome` 只允许 `ok`、`rejected` 和 `failed`。成功结果中的 `target` 必须与请求完全一致，
`content.type` 必须是 `terminal.snapshot`，且 `content.mode` 必须与请求的 `mode` 完全一致；
普通 `text/plain` 不能冒充成功快照。其余字段遵循第 10.1 节。状态事件到读取之间目标已经
变化、消失或尚未同步时，Pal 必须返回稳定目标错误，不能把新会话内容标记为旧 Target。

图片请求已经成功读取纯文本但渲染失败时，结果使用 `outcome: failed` 和
`terminal.image_failed`，并可以携带 `fallback_content`。`fallback_content` 必须是同一次
读取产生的 `mode: txt` 终端内容，不得重新读取后拼接成不同快照。Server 可以只在通知
场景使用该内容降级；空白终端页也是合法的同次读取结果，不能用 `text` 是否为空判断能否
降级。用户主动执行图片终端命令时仍应展示明确错误。

该快照表示 Pal 处理请求时能够读取到的最新终端视图，不承诺冻结状态事件发生瞬间的
屏幕。Server 获取内容失败时仍应保留状态事件，并可以发送不含终端正文的通知。图片生成
或 IM 上传失败时，Server 可以使用 `content.text` 降级，但不得修改用户保存的模式。
Server 超时后可以忽略与近期已过期 `reply_to` 精确匹配的迟到结果；未知关联仍是协议错误，
不得为了容忍迟到消息而忽略任意未匹配结果。

## 13. 统一结果与错误模型

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
- 每种结果消息只能使用其消息规范明确列出的 `outcome` 子集；接收方不得把未知值推断为
  成功。Feature 的 `cancelled` 不自动扩展到其他结果消息。
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
command.idempotency_conflict
command.timeout
command.execution_failed
terminal.snapshot_unsupported
terminal.snapshot_failed
terminal.image_unsupported
terminal.image_failed
feature.unsupported
feature.invalid_input
feature.idempotency_conflict
feature.not_cancellable
feature.not_running
feature.execution_failed
server.busy
server.internal
```

业务错误不得关闭连接。非法 JSON、重复字段、消息超过硬限制、协议状态机违规或持续
发送无法理解的必需消息属于连接级错误。服务端应在可以安全解析信封时先发送
`protocol.error`，随后使用合适的 WebSocket close code 关闭连接。

## 14. 顺序、背压与恢复

- WebSocket 保证单连接内帧顺序，但业务处理可以并发；响应必须通过 `reply_to` 关联。
- hello 协商得到 `max_inflight_commands`，超过限制时返回 `server.busy`，不能无限
  排队。
- hello 协商得到 `max_inflight_features`，执行中的 Feature 在发送最终结果前持续占用一个
  名额；取消请求不得被该限制阻塞。
- `terminal.snapshot.get` 计入 `max_inflight_commands`，其读取和渲染不得阻塞 heartbeat、
  状态事件或其他请求的最终结果。
- 输出、Feature 事件和通知队列必须有界；任何队列溢出都要产生可诊断错误或指标，
  禁止静默丢弃最终结果。
- heartbeat、关闭帧、取消请求和最终结果优先于大块终端输出或进度事件。
- 断线时所有未完成请求进入 `indeterminate`，除非实现能够证明其尚未执行。
- 重连创建新 `connection_id`，重新执行 hello 和完整会话快照。
- Server 不把旧连接的选择、快照序号或在途命令直接移植到新连接。
- Feature Package 可以在相同 `idempotency_key` 下定义重连恢复，但 Server 不能假定所有
  Feature 都支持恢复。

## 15. 安全边界

- 所有部署必须使用加密的 WSS；证书信任策略属于产品部署配置，不参与 HPRP 消息
  兼容性判断。
- 实现应该验证服务端证书。为兼容当前内网自签名部署，Herdr Pal 产品可以保留跳过
  证书链验证的兼容默认值，但必须支持开启严格验证，并在跳过验证时输出明确告警。
- 服务端必须限制认证失败频率、消息大小、快照数量、并发命令和输出速率。
- 日志不得包含完整凭据、Cookie 或未经处理的大段敏感终端内容。
- 终端图片的 Base64、配对纯文本和企业微信临时媒体标识不得写入常规运行日志；日志只
  记录目标、模式、尺寸、耗时、页码和错误类别。
- 服务端只能向 Key 所属用户和机器的连接路由命令。
- Pal 必须继续执行本地 PolicyGuard；服务器发来的内容不是自动授权。
- 未绑定目标、已经变化的 `session_id`、未协商 Feature 和未知用户功能默认拒绝。
- Pal 不得提供绕过 Feature Package 校验的原始 Herdr RPC、任意 Socket method 或内部
  调试入口。

## 16. 公开制品与一致性测试

HPRP/1 发布时应同时提供：

- 中文规范正文及明确版本号；
- 核心消息 JSON Schema；
- 错误码、技术 capability 和 Feature Package 注册表；
- 合法与非法消息的固定测试向量；
- hello、快照、命令、Feature、会话替换、取消和断线重连的状态机测试轨迹；
- 不依赖 Herdr 和 IM 网络的内存参考客户端与服务端；
- 跨版本兼容测试矩阵。

一致性测试至少覆盖：

- 未知可选字段被忽略；
- 未知可选消息不导致断线；
- 未协商 capability 不会被使用；
- 同一 Feature family 只选择最高共同版本；
- 未协商 Feature 不会被调用，Feature 参数只包含用户功能限制；
- Feature 输入通过对应 Package Schema 校验；
- 相同 Feature 幂等键不会重复展开底层操作；
- Feature 事件有序且最终结果后不再产生事件；
- 取消只停止后续执行，不会被误报为已撤销已有效果；
- 重复消息、重复结果和重复通知能够去重；
- 过期快照和基准不一致增量触发明确错误；
- 目标会话替换后旧命令不会误发；
- 状态事件不携带终端正文，由 Server 决定是否请求无副作用快照；
- 无副作用快照不会改变 `/con`、`/pageup` 和 `/pagedn` 的交互分页状态；
- 图片终端内容始终包含同目标、同次采集、同一页的纯文本；
- 非法 Base64、PNG、尺寸、页码和超限图片被明确拒绝；
- 超时和不确定结果不会被不安全地自动重试；
- 新旧实现只共享 HPRP/1 基础能力时仍可互操作。

任何实现只要通过 HPRP/1 基础一致性测试，就应该能够与其他合规 HPRP/1 实现建立
连接。可选扩展不一致时必须降级，不能影响基础会话上报和命令执行。

## 17. 标准治理

- HPRP 主版本规范一经发布即保持不可变；勘误只能澄清，不得改变线上语义。
- 错误码、消息类型、技术 capability 和 Feature Package 由公开注册表管理，新增项需要
  说明降级行为。
- Feature Package 必须以用户目标命名，不得使用 Herdr Socket method 列表充当 Feature
  合同。底层适配说明可以放在参考实现文档中，但不是 HPRP 规范的一部分。
- 第三方私有扩展应使用明确的厂商命名空间，禁止占用公共名称。
- 实验性能力不得在未协商时发送，也不得成为同一主版本的隐含必需依赖。
- 参考实现的缺陷不能自动改变规范；如需改变规范性行为，应发布新主版本。

## 18. 实施边界

后续实施应按以下独立模块推进：

- `hprp`：信封、消息类型、JSON Schema、校验和错误注册表；
- `credential`：Upgrade 认证、Key 摘要、吊销和轮换；
- `connection`：状态机、heartbeat、能力协商和背压；
- `catalog`：完整快照、稳定目标和会话替换；
- `command`：请求关联、幂等、结果和分段输出；
- `feature`：Feature 协商、Schema 校验、调用编排、事件、取消和恢复；
- `notification`：状态事件、去重和可选确认；
- `terminal`：无副作用快照、配对文本与安全 ANSI、分页和图片渲染；
- `im`：终端标题、文本降级、临时媒体上传和图片消息发送；
- `conformance`：测试向量、fake client/server 和跨版本验证。

Herdr Pal 参考实现中的 Server 与 Pal 已统一使用 HPRP/1，并删除旧 `relayproto` 运行时
实现。后续兼容性改动必须遵守第 3 节：主版本内只增加可选字段、协商能力、Feature 或稳定
错误码；不兼容变更发布新的 WebSocket 子协议和 HPRP 主版本。
