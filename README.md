# Herdr Pal

Herdr Pal 是一个独立于 Herdr 的 IM bridge。它通过 Herdr 已有的本地
Socket API 感知 Coding Agent 的生命周期状态、读取阶段性终端输出，并将
来自 IM 的消息安全地转发给目标 Agent。

当前目录只包含项目交接文档，尚未选择 IM 平台、编程语言、持久化方案或部署方式。

## 核心目标

- 将 Agent 的 `working`、`blocked`、`idle`、`done`、`unknown` 状态变化通知到 IM。
- 在 Agent 阻塞或完成时，读取最近的终端输出并发送到对应 IM 会话。
- 将普通 IM 文本通过 `agent.prompt` 发送给目标 Agent。
- 将明确的交互动作通过 `agent.send_keys` 发送给 Agent UI。
- 在断线、Herdr 重启和 pane 生命周期变化后恢复正确映射。

## 已确认结论

| 能力 | Herdr 当前支持情况 |
| --- | --- |
| 状态事件通知 | 直接支持 `events.subscribe` 和 `pane.agent_status_changed` |
| IM 文本输入 | 直接支持 `agent.prompt` |
| 交互式按键输入 | 直接支持 `agent.send_keys` |
| 完成或阻塞时读取最近输出 | 支持 `agent.read`、`pane.read` |
| 已知文本或正则触发 | 支持 `pane.output_matched`、`pane.wait_for_output` |
| 实时逐行或逐 token 输出 | 当前不支持 |
| 结构化 LLM assistant 消息 | 当前不支持 |
| 输出增量游标和断点续传 | 当前不支持 |

第一版不需要修改 Herdr。推荐使用“状态事件触发终端快照读取”的方式完成双向
IM 对接；bridge 自行负责文本清理、差分、去重和重连恢复。

## 项目边界

- Herdr 是外部运行时依赖，不在本项目中修改。
- 本项目不采用 MCP 作为 Herdr 集成层。
- 本项目通过 Herdr 本地 Socket API 工作，不直接接触 Herdr 的 `AppState`、PTY
  或私有 TUI client socket。
- 本项目默认只允许本机同一用户访问 Herdr Socket，不把原始 Socket 暴露到网络。
- 第一版不承诺恢复完整 LLM 对话记录，只发送能够从终端快照中可靠提取的内容。
- 第一版不自动批准 Agent 的权限请求。

## 文档阅读顺序

1. [交接上下文](docs/HANDOFF_CONTEXT.md)
2. [Herdr 本地 API 审计](docs/HERDR_API_AUDIT.md)
3. [Bridge 推荐架构](docs/BRIDGE_ARCHITECTURE.md)
4. [Agent 开发约束](AGENTS.md)

## Herdr 审计基线

- 本地源码：`/Users/wxc/Code/herdr`
- 审计提交：`2a20e90 fix: preserve physical escape on windows`
- 审计日期：`2026-07-23`
- 协议常量：`src/protocol/wire.rs::PROTOCOL_VERSION = 17`

正式实现前，应以运行中的 Herdr 二进制输出为最终协议依据：

```bash
herdr api schema --json
herdr api snapshot
```

## 下一步

开始实现前需要确定：

1. 第一版接入的 IM 平台。
2. bridge 的编程语言和运行方式。
3. IM 会话与 Herdr pane/agent 的绑定方式。
4. 是否需要本地持久化、命令权限和多用户控制。

这些选择确定后，再编写实现计划、`build.sh`、`unittest.sh` 和代码。
