# Prompt 状态确认与 Enter 恢复设计

## 1. 背景

Herdr Pal 当前在 `agent.prompt` 返回 `agent_prompted` 后立即回复“已发送”。该响应只表示
Herdr 已接受输入字节，不表示 Agent 已经开始处理。Claude Code 等 TUI 偶尔会留下已粘贴
文本，但没有产生可观察的状态变化。

本次修改让普通文本发送成为一个有结果确认的流程：先确认 Agent 可以接收文本，发送后
等待状态变化；首次等待超时后只补发一次 Enter，并再次等待。只有观察到状态变化才报告
发送成功。

## 2. 范围与边界

- 普通文本仅允许发送给当前状态为 `idle` 或 `done` 的 Agent。
- `working`、`blocked`、`unknown` 均拒绝普通文本，不调用 `agent.prompt`。
- 继续使用 Herdr 公共 `agent.prompt`、`agent.get` 和 `agent.send_keys`，不使用私有接口。
- 不修改 Herdr，不改用原始 pane 输入。
- 自动恢复最多补发一次 `enter`，不会自动发送空格、选择项或其他按键。
- `blocked` 状态永远不会触发自动 Enter，避免把恢复逻辑变成权限审批动作。
- 两段等待各为 5 秒；第二次超时后不再重试。

## 3. 方案选择

采用“原子 prompt 等待 + Enter 后序列轮询”方案。

首次发送使用带 wait 的 `agent.prompt`。Herdr 在处理同一请求时记录提交前的
`state_change_seq`，因此不会漏掉紧随 prompt 发生的快速状态变化。`wait.until` 包含协议
定义的全部 Agent 状态，使请求在首次可观察状态变化后立即返回，而不是继续等待任务完成。
不显式设置 `timeout_ms`，使用 Herdr 对 prompt effect 固定的 5 秒检测窗口；无变化时返回
`agent_prompt_stalled`。

补发 Enter 后不串行调用 `agent.wait`。该调用会在请求抵达 Herdr 时才建立基线，可能漏掉
Enter 与等待请求之间已经发生的状态变化。恢复阶段改为每 100 毫秒调用一次 `agent.get`，
比较提交前的 `state_change_seq`，最长等待 5 秒。

未采用以下方案：

- 在 `EventSupervisor` 中增加临时等待器：可以减少查询，但会把同步消息处理与订阅重连、
  快照收敛和事件去重耦合起来。
- 固定延迟后无条件发送 Enter：正常 prompt 已生效时可能产生重复输入。

## 4. HerdrClient 接口

### 4.1 AgentInfo

`AgentInfo` 增加公开只读字段 `StateChangeSeq uint64`，并严格解析 protocol 17 响应中的
`state_change_seq`。该字段用于判断 Agent 生命周期是否在本次发送后发生过变化，不使用
固定为零的 `PaneReadResult.revision`。

### 4.2 PromptUntilStateChange

新增一个职责明确的客户端方法：

```go
PromptUntilStateChange(ctx context.Context, target, text string) (AgentInfo, error)
```

它发送：

```json
{
  "target": "w1:p1",
  "text": "用户文本",
  "wait": {
    "until": ["idle", "working", "blocked", "done", "unknown"]
  }
}
```

成功响应必须是 `agent_prompted`，方法返回发生变化后的 `AgentInfo`。Herdr 返回
`agent_prompt_stalled` 时保留为可通过 `errors.As` 检查的 `APIError`，由业务层决定是否
补发 Enter。其他 API、协议和连接错误不进入恢复流程。

现有无等待 `Prompt` 方法仅在仍有明确调用方时保留；业务 Service 改用新方法。

## 5. 发送状态机

### 5.1 初始门禁

普通文本处理依次执行：

1. 取得当前选择。
2. 调用 `agent.get`，校验 pane 中仍是选定 occupant。
3. 检查状态：仅 `idle`、`done` 继续；其他状态返回可操作错误。
4. 记录初始 `state_change_seq` 和 occupant 身份。

拒绝状态的回复应包含当前状态，例如：

```text
Agent 当前状态为 working，暂不接受普通文本。
```

### 5.2 首次提交

调用 `PromptUntilStateChange`：

- 成功：再次校验返回的 Agent 仍是原 occupant，然后按返回状态报告成功。
- `agent_prompt_stalled`：进入一次性 Enter 恢复。
- 其他错误：返回“发送失败，请稍后重试”，不发送 Enter。

### 5.3 Enter 恢复

补发前再次调用 `agent.get`：

- occupant 已替换：使当前选择失效并返回目标变化错误。
- `state_change_seq` 已不同于初始值：说明等待结束附近已经发生变化，直接按当前状态报告
  成功，不再发送 Enter。
- 状态已经变为 `working`、`blocked` 或 `unknown`：视为已发生状态变化，报告当前状态，
  不发送 Enter。
- 仍是原 occupant、序列未变化且状态为 `idle` 或 `done`：调用
  `agent.send_keys(["enter"])`。

补发成功后，每 100 毫秒调用 `agent.get`：

- occupant 改变：选择失效并返回目标变化错误。
- `state_change_seq` 改变：按当前状态报告成功。
- 查询失败：立即返回发送失败，不把连接错误伪装成等待超时。
- 5 秒内始终没有变化：返回“发送未生效，请检查 Agent 界面。”

等待遵守上游 `context` 取消；关闭或 Herdr 重连时应尽快结束，不继续补发或轮询。

## 6. 回复与审计

成功回复包含确认后的状态：

```text
已发送，Agent 状态已变为 working。
```

状态名称使用协议稳定值，避免把 `blocked`、`done` 等不同结果统一描述为“已经开始工作”。
第二次等待超时不返回“已发送”。

恢复阶段的 Enter 使用现有按键审计模型，记录用户、pane、occupant 摘要、规范化按键、
结果和时间。补发前状态拒绝、发送失败和成功分别记录 `rejected`、`failed`、`sent`。
日志和审计均不记录完整 prompt。

## 7. 并发与一致性

普通文本流程继续占用当前 Service 输入操作屏障，直到确认成功或失败。这样 `/sel`、Herdr
客户端替换和当前 prompt 不会混用不同目标。状态订阅与通知仍可并发处理；同步回复只依据
每次 `agent.get` 返回的公共状态和序列。

每次跨越可能改变 pane 的操作后都重新校验 occupant。不能仅凭 pane ID 或旧
`SessionRegistry` 缓存补发 Enter。

## 8. 测试策略

### 8.1 HerdrClient 单元测试

- 请求包含完整 `wait.until`，且不发送 `timeout_ms`。
- 成功解析 `agent_prompted` 和 `state_change_seq`。
- 拒绝缺失或非法 `state_change_seq` 的 protocol 17 响应。
- 保留 `agent_prompt_stalled` 的 `APIError.Code`。
- 拒绝错误 result type 和不完整 Agent 信息。

### 8.2 Service 单元测试

- `idle`、`done` 可以提交。
- `working`、`blocked`、`unknown` 在调用 prompt 前被拒绝。
- 首次等待观察到状态变化时不发送额外 Enter。
- 首次 stalled、补发前序列已经变化时不发送 Enter。
- 首次 stalled、补发 Enter 后序列变化时报告成功。
- 第二次等待 5 秒仍无变化时报告未生效，且只发送一次 Enter。
- 补发前或轮询中 occupant 替换时使选择失效。
- 首次非 stalled 错误不会补发 Enter。
- 自动 Enter 的 `sent`、`failed`、`rejected` 审计完整且不含 prompt。
- `context` 取消会停止等待和轮询。

测试通过可注入的时钟或等待函数推进轮询，不使用真实 5 秒睡眠。

### 8.3 集成测试

扩展 fake Herdr，使带 wait 的 `agent.prompt` 可以返回状态变化或
`agent_prompt_stalled`，并允许测试在 `agent.send_keys` 后更新 `state_change_seq`。覆盖：

- 普通 prompt 首次确认成功。
- stalled 后补发一次 Enter 并确认成功。
- 不适合文本输入的状态没有产生写调用。

## 9. 文档更新

README、API 审计、架构文档和交接上下文更新以下语义：

- “已发送”代表已经观察到 Agent 状态变化，不再代表字节入队。
- 普通文本仅允许 `idle`、`done`。
- 首次 5 秒无变化时会在状态和 occupant 复核后补发一次 Enter。
- 第二次 5 秒无变化返回错误，不继续重试。
- 自动恢复 Enter 不会在 `blocked` 状态执行，并写入按键审计。
