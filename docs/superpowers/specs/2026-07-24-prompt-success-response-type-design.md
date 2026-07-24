# Prompt 成功响应类型修复设计

## 1. 问题

Herdr Pal 当前向 Herdr protocol 17 发送带 wait 的 `agent.prompt` 后，要求成功响应的
result type 为 `agent_info`。真实 Herdr 的 `prompt_agent` 等待流程最终调用
`agent_prompt_success`，返回的实际 result type 是 `agent_prompted`。

因此 Herdr 已把文本和 Enter 写入 pane、Agent 也已经开始工作时，Herdr Pal 仍会在
`validateResultType` 中产生协议错误，业务层随后回复“发送失败，请稍后重试”。Herdr
服务日志中对应请求的 outcome 为 `ok`，与该假失败路径一致。

## 2. 决策

严格按照当前审计的 protocol 17 修复：`PromptUntilStateChange` 只接受
`agent_prompted`，继续从响应中的完整 `agent` 字段解析 occupant、状态和
`state_change_seq`。

不同时接受 `agent_info`，避免把 fake 或未来协议漂移静默当成兼容；也不把所有 prompt
错误都交给业务层重新查询状态，避免掩盖真实连接、协议和 API 错误。

## 3. 修改范围

- `internal/herdr/client.go`：修正 `PromptUntilStateChange` 的成功 result type。
- `internal/herdr/client_test.go`：使用真实 `agent_prompted` 响应建立回归测试，并确认
  `agent_info` 被拒绝。
- `internal/testkit/herdr_server.go`：带 wait 的 fake `agent.prompt` 返回
  `agent_prompted`，与真实 Herdr 一致。
- `internal/testkit` 和 `internal/integration`：保持正常 prompt 与 stalled 恢复链路覆盖。
- README、API 审计、交接与架构文档：删除“成功响应为 `agent_info`”的错误描述。

## 4. 不变行为

- 普通文本仍只允许 `idle`、`done`。
- `agent_prompt_stalled` 仍进入 occupant、状态和序列复核，再最多补发一次 Enter。
- 其他 API、协议或连接错误仍报告发送失败，不自动猜测请求已生效。
- 成功仍必须解析完整 AgentInfo，并确认 `state_change_seq` 相对发送前发生变化。
- 不修改 Herdr 源码，不放宽 protocol 17 门禁。

## 5. 验证

回归测试必须证明：

1. 真实形状的 `agent_prompted` 响应可以返回状态变化后的 AgentInfo。
2. 错误的 `agent_info` result type 被判为协议错误。
3. fake Herdr 的正常 prompt 端到端不再产生假失败。
4. stalled、一次性 Enter 恢复、状态门禁和 occupant 替换测试继续通过。
5. `./unittest.sh`、`./build.sh` 和 `git diff --check` 全部通过。
