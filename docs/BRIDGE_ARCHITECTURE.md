# Herdr Pal Bridge 推荐架构

## 1. 架构目标

Herdr Pal 作为独立进程运行。它不修改 Herdr，也不要求消息入口理解 Herdr 协议。
bridge 在本机连接 Herdr Socket，消息侧既可以连接企业微信，也可以使用本机控制台。

```text
┌──────────────────────┐
│ Herdr Server         │
│ Local Socket / NDJSON│
└──────────┬───────────┘
           │ snapshot / events / read / prompt / send_keys
           ▼
┌─────────────────────────────────────────────────────────┐
│ Herdr Pal 共享 Bridge                                   │
│                                                         │
│ HerdrClient ─ SessionRegistry ─ EventSupervisor         │
│      │               │              │                    │
│      └──── OutputExtractor ─ Service / Notifier         │
│                            │                             │
│           PolicyGuard / Deduper / 内存运行时状态        │
│                            │                             │
│                         IMAdapter                       │
└────────────────────────────┬────────────────────────────┘
                             │
                 ┌───────────┴───────────┐
                 ▼                       ▼
       ┌──────────────────┐    ┌──────────────────┐
       │ WeComClient      │    │ ConsoleAdapter   │
       │ WebSocket 长连接 │    │ stdin / stdout   │
       └──────────────────┘    └──────────────────┘
```

## 2. 模块边界

### 2.1 HerdrClient

职责：

- 解析 Herdr Socket 路径和 session。
- 编码、发送和解析 NDJSON 请求。
- 维护 `events.subscribe` 长连接。
- 区分请求响应、订阅确认和事件行。
- 将协议错误转换为稳定的项目内部错误类型。
- 启动时记录 Herdr version、protocol 和 capabilities。

该模块不包含 IM 逻辑、输出清理规则或会话路由。

### 2.2 SessionRegistry

职责：

- 使用 `session.snapshot` 建立本地运行时视图。
- 按 `pane_id`、`terminal_id` 和 Agent name 建立索引。
- 保存 workspace、tab、pane、agent 的展示信息。
- 应用 pane 生命周期事件。
- 在 Agent occupant 变化时使旧绑定失效或进入待确认状态。

缓存是可重建状态，不是事实来源；Herdr 重连后以新快照为准。

### 2.3 EventSupervisor

职责：

- 维护 workspace/tab/pane 生命周期订阅。
- 为当前 Agent pane 建立 `pane.agent_status_changed` 订阅。
- pane 集合变化后重建 pane 级订阅。
- 将事件转化为内部的状态迁移记录。
- 处理 Socket 关闭、超时、Herdr 重启和指数退避重连。

由于状态订阅必须指定 `pane_id`，推荐第一版采用：

- 一个生命周期订阅连接。
- 一个包含当前所有 pane 状态订阅的批量连接。
- pane 集合变化时关闭并重建批量状态连接。

如果重建造成明显抖动，再演进为每个 pane 一个状态连接或可复用连接池。

### 2.4 OutputExtractor

这是输出处理的逻辑边界；当前实现由 `Notifier`、`panel` 和消息分段函数共同完成，而非
一个独立的持久化模块。当前职责和边界是：

- 在 `blocked`、`done`，以及从 `working`/`blocked` 转入 `idle` 的状态边界调用
  `agent.read(recent_unwrapped, 100)`。
- 只保留最近 100 行，规范化换行与 ANSI/TUI 噪音。
- 对整份规范化快照计算 hash，参与相同状态通知的内存去重。
- 按消息大小做 UTF-8 安全分段和截断。
- 对 alternate-screen Agent 明确标记内容只是当前可见或近期终端快照。

当前没有 revision checkpoint，也没有可靠的增量差分；主动通知发送的是受限的近期
快照，不能表述为“自上次以来的新增 assistant 内容”。`/pageup` 使用连续重叠锚点扩展
手工分页缓存，是另一条内存分页路径，不等同于通知增量提取。

未来可以把 Agent 特定清理规则、上一快照 checkpoint、最长公共前缀或稳定后缀差分演进
为独立模块。差分不可信时仍应回退为带说明的短快照，不能伪造精确增量。

### 2.5 ConversationRouter

职责：

- 管理 IM channel/thread/user 与 Herdr pane/agent 的绑定。
- 将 Herdr 通知路由到正确的 IM 会话。
- 将 IM 回复解析为普通 prompt 或显式控制动作。
- 防止旧 IM thread 向已经被替换的 Agent occupant 发送输入。

当前版本只有一个授权企业微信用户或固定的 `interactive-local` 身份，并在内存中保存
当前 `/ls` 编号快照、`/sel` 选择和分页缓存。Herdr 断线、pane/occupant 变化或进程重启
都会清空相关状态，不存在可跨重启恢复的持久 Binding。

未来需要多会话或持久恢复时，可以引入稳定的内部绑定记录：

```text
Binding
  id
  im_adapter
  im_conversation_id
  im_thread_id?
  pane_id
  terminal_id
  agent_session_ref?
  created_at
  last_verified_at
  enabled
```

`pane_id` 用于 API 调用；`terminal_id` 和可用的 agent session reference 用于判断
pane 中的 Agent 是否已经被替换。

### 2.6 PolicyGuard

职责：

- 校验企业微信用户或固定本地身份是否被授权，并且消息类型为单聊。
- 区分普通文本、导航按键和不受支持的命令。
- 只允许预定义的按键白名单。
- 生成不包含密钥的审计记录。

目标选择与 occupant 实时校验由 `Service`、`SessionRegistry` 和 `HerdrClient` 共同完成，
但属于同一条输入策略边界，适配器不能绕过。

默认策略：

- 普通文本允许映射到 `agent.prompt`。
- 白名单按键命令本身就是用户的显式操作，不二次确认。
- `ctrl+c`、组合键、退出、关闭 pane 等动作当前不提供入口。
- Agent 权限审批永不自动执行。

### 2.7 IMAdapter

职责：

- 按各自信任边界接收文本消息。
- 将来源对象转换为统一内部文本事件。
- 发送首段回复和后续通知分段。

当前共享能力可以概括为：

```text
receive() -> IMEvent
respond(callback_ref, message)
send_message(target, message)
```

当前有两个并列适配器：

- `WeComClient` 负责企业微信智能机器人 WebSocket 长连接、回调接收和消息发送。
- `ConsoleAdapter` 把 stdin 的逐行输入转换为本地单聊事件，把回复和通知串行写入 stdout；
  它不是 TUI，也不解析 Herdr 协议。

两种适配器进入同一套 Bridge 装配。交互模式不会绕过 `PolicyGuard`、`Deduper`、
`Service`、`Notifier` 或 `EventSupervisor`，因此身份检查、消息幂等、occupant 校验、状态
订阅、输出限制和按键审计与企业微信模式保持一致。接口保持可扩展，但不为尚未接入的
平台提前设计公共最小公倍数；消息更新、撤回或交互按钮属于未来适配能力。

### 2.8 StateStore（未来模块）

当前没有 StateStore。会话选择、分页缓存、状态通知 hash、occupant 路由状态和有容量、
TTL 上限的 `msgid` 幂等键全部只保存在进程内存中；进程重启后不会恢复，也不会补发旧
通知。

未来若需要跨重启恢复，StateStore 可以保存 IM 与 pane 的最小绑定、通知去重元数据、
输出 checkpoint 和消息幂等键。Herdr 会话快照不应原样永久保存，并且恢复出的绑定仍须
用最新 `session.snapshot` 和 occupant 信息重新验证。

## 3. 数据流

### 3.1 启动

```text
启动 bridge
  → ping / session.snapshot
  → 重建 SessionRegistry
  → 建立生命周期订阅
  → 建立当前 pane 状态订阅
  → 用订阅后的权威 snapshot 收敛当前基线
  → 启动 WeComClient 或 ConsoleAdapter
  → 等待用户重新 /ls、/sel，不恢复上次选择或分页
```

如果 Herdr 未运行，bridge 可以等待并重试，但不应未经明确产品决定自动启动或停止
Herdr。

### 3.2 状态通知

```text
pane.agent_status_changed
  → 校验当前 pane/occupant
  → 和最近状态比较
  → 应用通知策略
  → 必要时 agent.read(recent_unwrapped, 100)
  → 规范化最近 100 行并计算整快照 hash
  → 内存去重、分段和安全截断
  → IMAdapter 发送通知
```

推荐状态策略：

| 状态 | 默认动作 |
| --- | --- |
| `working` | 更新状态，默认不发送大段输出 |
| `blocked` | 立即通知，读取近期输出，提供安全交互入口 |
| `done` | 通知完成，读取并发送最近 100 行快照 |
| `idle` | 仅在从 working/blocked 转入时读取输出 |
| `unknown` | 标记状态不确定，不宣称任务成功 |

### 3.3 IM 普通回复

```text
WeComClient：已认证并订阅的 WebSocket → 解码企业微信文本回调 ┐
ConsoleAdapter：进程 stdin → 固定 interactive-local 单聊身份   ├→ 共享 Bridge
                                                              ┘
共享 Bridge
  → PolicyGuard 校验身份和单聊类型
  → Deduper 登记 msgid
  → 解析普通 prompt
  → 校验当前选择和 Agent occupant
  → agent.prompt
  → 返回“已送达”或错误
  → 等待后续状态事件
```

正常聊天消息不要调用 `pane.send_text`，因为该接口不会验证当前 pane 中是否仍为原
Agent。

### 3.4 显式控制动作

```text
WeComClient
  → 只接收已认证、已订阅 WebSocket 上通过协议校验的企业微信回调

ConsoleAdapter
  → 只接收当前进程 stdin
  → 写入固定 user_id=interactive-local、chat_type=single

两种入口汇合到共享 Bridge
  → PolicyGuard 校验身份和单聊类型
  → Deduper 登记 msgid，拒绝重复投递
  → command.Parse 识别显式按键命令
  → PolicyGuard.ValidateKey 校验按键白名单
  → Service 校验当前选择、pane 和 Agent occupant
  → agent.send_keys
  → 同步记录用户、pane、occupant 摘要、动作和结果
```

ConsoleAdapter 不验证平台签名；它的信任边界是本机进程 stdin 和固定本地身份。任一消息
入口都不能把外部字符串直接当作任意 Herdr key string。

## 4. 重连和一致性

### 4.1 Herdr 连接断开

1. 将连接状态改为 degraded。
2. 暂停所有 IM → Herdr 输入。
3. 使用有上限的指数退避重连。
4. 重连后执行 `ping` 和 `session.snapshot`。
5. 用新快照替换 SessionRegistry。
6. 清空当前选择和分页缓存。
7. 重建订阅并用订阅后的 snapshot 收敛基线。
8. 不恢复旧选择，不重放断线期间的旧状态通知。

### 4.2 幂等和去重

当前 Herdr 事件没有对外 cursor，企业微信回调也可能重复投递。当前内存去重包括：

- 入站 `msgid`：容量和 TTL 有上限，prompt 与按键只接受第一次投递。
- 状态通知 key：pane + occupant + status + 最近 100 行规范化快照 hash。
- pane 失效通知 key：pane + occupant。

进程重启后这些键不会恢复。未来 webhook adapter 或持久化 StateStore 需要另行定义签名
验证、事件 ID 和跨重启幂等策略。

exactly-once 不作为第一版承诺；目标是 at-least-once 输入接收配合业务幂等，以及
best-effort 去重通知。

## 5. 错误处理

错误应分为：

- `HerdrUnavailable`：Socket 不可用或重连中。
- `ProtocolMismatch`：Schema 或响应结构不兼容。
- `TargetNotFound`：pane/agent 不存在。
- `OccupantChanged`：pane 仍存在但 Agent 已替换。
- `InputRejected`：Herdr 拒绝 prompt 或按键。
- `Unauthorized`：IM 用户或会话没有权限。
- `OutputUnavailable`：终端近期快照为空、丢失或无法安全处理。
- `IMDeliveryFailed`：平台限流、网络错误或消息格式错误。

面向用户的错误消息应可操作，例如“Agent 已被替换，请重新绑定”，而不是只展示底层
Socket 错误。

## 6. 安全边界

- Herdr Socket 只在本机访问。
- 企业微信当前使用客户端主动建立的 WebSocket 长连接，并通过 Bot ID 与 Secret 完成订阅；
  Secret 只从 `HERDR_PAL_WECOM_SECRET` 环境变量读取，配置文件拒绝 Secret 字段。
- 当前没有 webhook 入口。未来若新增 webhook adapter，必须验证平台签名和时间戳。
- 企业微信模式只允许配置中的一个用户和单聊；交互模式只允许固定的本地身份。
- 终端输出可能包含源码、密钥和日志，转发前要支持内容过滤和最大长度限制。
- 日志默认只记录 hash、长度和目标标识，不记录完整 prompt 和输出。
- 不允许 IM 用户选择任意本地 Socket 路径。
- 不允许通过 IM 直接调用 Herdr 的 server.stop、pane.close 等高风险 API。

## 7. 测试策略

### 7.1 单元测试

- NDJSON framing 和部分读取。
- Herdr success/error/event 数据解析。
- 状态迁移通知规则。
- 输出规范化、整快照 hash 去重、分页重叠和截断。
- Binding occupant 校验。
- PolicyGuard 动作矩阵。
- 企业微信回调 `msgid` 内存幂等。

### 7.2 集成测试

- 使用 fake Herdr Socket Server 模拟 snapshot、订阅和断线。
- 使用 fake IM Adapter 验证发送、更新和失败重试。
- 测试 pane 创建后状态订阅重建。
- 测试 Herdr 重启后 pane id/terminal id 改变。
- 测试相同事件或企业微信回调重放不会产生重复危险输入。

### 7.3 手工联调

- Agent working → done。
- Agent working → blocked → 用户选择 → working。
- Agent 使用 alternate screen 时的输出表现。
- bridge 和 Herdr 分别重启。
- IM 限流和网络断开。

## 8. 分阶段交付

### 阶段一：首版 MVP

- 一个网络 IM Adapter 和一个共享核心的本地 ConsoleAdapter。
- 手工建立一个 IM 会话到 pane 的绑定。
- snapshot + pane 生命周期 + Agent 状态订阅。
- blocked/done 时读取 recent_unwrapped 并通知。
- 普通 IM 文本通过 agent.prompt 发送。
- Herdr 重连、订阅自动重建和断线后重新选择。
- `msgid`、状态通知和失效通知的内存幂等。
- 白名单按键、occupant 校验、结构化审计和安全日志。

### 阶段二：可靠性

- 本地 StateStore。
- 跨重启绑定、选择和幂等恢复。
- 上一快照 checkpoint、保守增量差分和 Agent 特定清理规则。
- 未来 webhook adapter 的验签与持久事件幂等。
- 更丰富的显式交互按钮和权限策略。

### 阶段三：产品化

- 多 Agent、多会话管理。
- 管理命令和绑定生命周期。
- 消息更新、线程化展示和通知聚合。
- 可观测性、审计和配置迁移。
- 根据实际需求评估第二个网络 IM 平台 Adapter。

整个路线都不要求修改 Herdr。若未来需要实时结构化输出，应作为独立的 Herdr
公共 API 提案处理，而不是让 Herdr Pal 依赖私有内部实现。
