# Herdr Pal Bridge 推荐架构

## 1. 架构目标

Herdr Pal 作为独立常驻进程运行。它不修改 Herdr，也不要求 IM 平台理解 Herdr
协议。bridge 在本机连接 Herdr Socket，在网络侧连接一个或多个 IM API。

```text
┌─────────────────────┐
│ Herdr Server        │
│ local Socket / NDJSON│
└──────────┬──────────┘
           │ snapshot / events / read / prompt
           ▼
┌──────────────────────────────────────────────┐
│ Herdr Pal                                    │
│                                              │
│ HerdrClient ─ SessionRegistry ─ EventSupervisor
│      │               │              │         │
│      └──── OutputExtractor ─ ConversationRouter
│                            │          │         │
│                       PolicyGuard ─ StateStore │
│                                   │            │
│                               IMAdapter        │
└───────────────────────────────────┬────────────┘
                                    │ HTTPS/Webhook
                                    ▼
                              ┌────────────┐
                              │ IM Platform│
                              └────────────┘
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

职责：

- 在 `blocked`、`done`、`idle` 等状态边界调用 `agent.read`。
- 默认读取 `recent_unwrapped` 文本快照。
- 规范化换行和 ANSI/TUI 噪音。
- 与上次 checkpoint 做差分。
- 限制 IM 消息长度并安全截断。
- 生成可审计的输出摘要元数据。

初始策略建议：

1. 每个 pane 保留最近一次规范化快照和 hash。
2. 先尝试最长公共前缀或稳定后缀差分。
3. 差分不可信时发送带说明的短快照，而不是伪造新增内容。
4. 对完全相同的快照不重复通知。
5. 对 alternate-screen Agent 明确标记“终端仅保留当前可见/近期内容”。

输出提取规则必须按 Agent 类型可配置，但通用逻辑不能硬编码到 IM Adapter。

### 2.5 ConversationRouter

职责：

- 管理 IM channel/thread/user 与 Herdr pane/agent 的绑定。
- 将 Herdr 通知路由到正确的 IM 会话。
- 将 IM 回复解析为普通 prompt 或显式控制动作。
- 防止旧 IM thread 向已经被替换的 Agent occupant 发送输入。

建议使用稳定的内部绑定记录：

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

- 校验 IM 用户和会话是否被授权。
- 限制哪些绑定可以接收输入。
- 区分普通文本、导航按键、终止操作和审批操作。
- 对高风险动作要求明确按钮、二次确认或管理员权限。
- 生成不包含密钥的审计记录。

默认策略：

- 普通文本允许映射到 `agent.prompt`。
- 导航按键只允许预定义动作，不接受任意按键字符串。
- `ctrl+c`、退出、关闭 pane 等动作默认需要额外确认。
- Agent 权限审批永不自动执行。

### 2.7 IMAdapter

职责：

- 验证 webhook 或长连接来源。
- 接收消息、按钮和交互事件。
- 发送、更新、分段和撤回平台消息。
- 将平台对象转换为统一内部事件。

推荐抽象接口：

```text
receive() -> IMEvent
send_message(target, message) -> MessageRef
update_message(message_ref, message)
send_actions(target, message, actions) -> MessageRef
```

第一版只实现一个 IM Adapter，接口保持可扩展但不提前实现多平台公共最小公倍数。

### 2.8 StateStore

职责：

- 保存 IM 与 pane 的绑定。
- 保存输出 checkpoint 和去重 hash。
- 保存 webhook 幂等键。
- 保存最近状态及通知记录。
- 支持进程重启后的恢复。

Herdr 会话快照不应原样永久保存；只存恢复 bridge 所需的最小数据。

## 3. 数据流

### 3.1 启动

```text
启动 bridge
  → ping / session.snapshot
  → 重建 SessionRegistry
  → 恢复本地 Binding
  → 校验 binding 的 pane 和 occupant
  → 建立生命周期订阅
  → 建立当前 pane 状态订阅
  → 启动 IM Adapter
```

如果 Herdr 未运行，bridge 可以等待并重试，但不应未经明确产品决定自动启动或停止
Herdr。

### 3.2 状态通知

```text
pane.agent_status_changed
  → 校验当前 pane/occupant
  → 和最近状态比较
  → 应用通知策略
  → 必要时 agent.read
  → OutputExtractor 差分
  → IMAdapter 发送或更新消息
  → 保存状态和 checkpoint
```

推荐状态策略：

| 状态 | 默认动作 |
| --- | --- |
| `working` | 更新状态，默认不发送大段输出 |
| `blocked` | 立即通知，读取近期输出，提供安全交互入口 |
| `done` | 通知完成，读取并发送新增输出 |
| `idle` | 仅在从 working/blocked 转入时读取输出 |
| `unknown` | 标记状态不确定，不宣称任务成功 |

### 3.3 IM 普通回复

```text
IM message
  → webhook/connection 验证
  → 查找 Binding
  → PolicyGuard 校验用户和 occupant
  → agent.prompt
  → 返回“已送达”或错误
  → 等待后续状态事件
```

正常聊天消息不要调用 `pane.send_text`，因为该接口不会验证当前 pane 中是否仍为原
Agent。

### 3.4 IM 控制按钮

```text
IM action
  → 验证签名和幂等键
  → 查找预定义动作
  → 风险和权限检查
  → agent.send_keys
  → 记录操作人、pane、动作和结果
```

IM 前端传来的值只能是项目定义的 action id，不能直接成为任意 Herdr key string。

## 4. 重连和一致性

### 4.1 Herdr 连接断开

1. 将连接状态改为 degraded。
2. 暂停所有 IM → Herdr 输入。
3. 使用有上限的指数退避重连。
4. 重连后执行 `ping` 和 `session.snapshot`。
5. 用新快照替换 SessionRegistry。
6. 验证现有 Binding 的 pane、terminal 和 Agent occupant。
7. 重建订阅。
8. 仅发送必要的恢复通知，避免重放旧状态。

### 4.2 幂等和去重

当前 Herdr 事件没有对外 cursor，IM webhook 也可能重复投递。因此至少需要：

- IM 入站 event id 去重。
- 状态通知 key：binding + pane + occupant + status + presentation hash。
- 输出通知 key：binding + normalized output hash。
- 控制动作 key：IM action event id。

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
- `OutputUnavailable`：终端快照为空、丢失或无法可靠差分。
- `IMDeliveryFailed`：平台限流、网络错误或消息格式错误。

面向用户的错误消息应可操作，例如“Agent 已被替换，请重新绑定”，而不是只展示底层
Socket 错误。

## 6. 安全边界

- Herdr Socket 只在本机访问。
- IM token 存放在受限配置或系统密钥存储中。
- Webhook 必须验证平台签名和时间戳。
- 默认使用用户、群组、channel allowlist。
- 终端输出可能包含源码、密钥和日志，转发前要支持内容过滤和最大长度限制。
- 日志默认只记录 hash、长度和目标标识，不记录完整 prompt 和输出。
- 不允许 IM 用户选择任意本地 Socket 路径。
- 不允许通过 IM 直接调用 Herdr 的 server.stop、pane.close 等高风险 API。

## 7. 测试策略

### 7.1 单元测试

- NDJSON framing 和部分读取。
- Herdr success/error/event 数据解析。
- 状态迁移通知规则。
- 输出差分、重复文本、重绘和截断。
- Binding occupant 校验。
- PolicyGuard 动作矩阵。
- IM webhook 幂等。

### 7.2 集成测试

- 使用 fake Herdr Socket Server 模拟 snapshot、订阅和断线。
- 使用 fake IM Adapter 验证发送、更新和失败重试。
- 测试 pane 创建后状态订阅重建。
- 测试 Herdr 重启后 pane id/terminal id 改变。
- 测试相同事件或 webhook 重放不会产生重复危险输入。

### 7.3 手工联调

- Agent working → done。
- Agent working → blocked → 用户选择 → working。
- Agent 使用 alternate screen 时的输出表现。
- bridge 和 Herdr 分别重启。
- IM 限流和网络断开。

## 8. 分阶段交付

### 阶段一：单平台 MVP

- 单个 IM Adapter。
- 手工建立一个 IM 会话到 pane 的绑定。
- snapshot + pane 生命周期 + Agent 状态订阅。
- blocked/done 时读取 recent_unwrapped 并通知。
- 普通 IM 文本通过 agent.prompt 发送。
- 内存去重和基础日志。

### 阶段二：可靠性

- 本地 StateStore。
- 重连、绑定验证和订阅自动重建。
- webhook 幂等。
- 输出差分质量和 Agent 特定清理规则。
- 显式交互按钮和权限策略。

### 阶段三：产品化

- 多 Agent、多会话管理。
- 管理命令和绑定生命周期。
- 消息更新、线程化展示和通知聚合。
- 可观测性、审计和配置迁移。
- 根据实际需求评估第二个 IM Adapter。

整个路线都不要求修改 Herdr。若未来需要实时结构化输出，应作为独立的 Herdr
公共 API 提案处理，而不是让 Herdr Pal 依赖私有内部实现。
