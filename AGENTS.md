# Herdr Pal Agent Instructions

## 项目目标

Herdr Pal 是 Herdr 与传统 IM 软件之间的独立 bridge。它通过 Herdr 公共本地
Socket API 接收 Agent 状态、读取终端快照并发送输入，不修改 Herdr 源码。

开始任务前依次阅读：

1. `README.md`
2. `docs/HANDOFF_CONTEXT.md`
3. `docs/HERDR_API_AUDIT.md`
4. `docs/BRIDGE_ARCHITECTURE.md`

## 固定边界

- 不在 `/Users/wxc/Code/herdr` 中实现 Herdr Pal 功能。
- 不通过 Herdr plugin startup hook 承载常驻 IM 连接。
- 不采用 MCP 作为 Herdr 与 bridge 的协议层。
- 只使用 Herdr 公共本地 Socket API、CLI 和生成的 JSON Schema。
- 不依赖 Herdr 私有 TUI client socket、内部 `AppState` 或未公开 Rust 模块。
- 不把 Herdr 原始本地 Socket 直接暴露到远程网络。
- 不将终端快照描述成结构化 LLM 消息或完整对话记录。
- 不自动批准权限请求；审批动作必须由用户显式触发并经过策略检查。

## 开发偏好

- 对话和项目文档使用中文。
- 变量、函数、类、接口和协议字段使用符合英文习惯的命名。
- 代码注释使用中文，且只解释模块原理、边界或非显然逻辑。
- 对外接口必须有适合语言文档工具生成文档的注释。
- 单元测试是必须的；新增逻辑应优先设计为可独立测试的纯模块。
- 进入代码实现阶段时，在项目根目录创建统一的 `build.sh` 和 `unittest.sh`。
- 提交前必须完成构建和全部单元测试。
- Git 提交使用 conventional commit 前缀，描述使用中文，例如
  `feat: 添加 Herdr 事件订阅客户端`。

## 架构约束

模块应保持单一职责，推荐边界如下：

- `HerdrClient`：Socket 连接、请求响应、订阅协议和 Schema 兼容性。
- `SessionRegistry`：会话快照、pane/agent 索引和动态生命周期。
- `EventSupervisor`：订阅管理、重连和状态迁移检测。
- `OutputExtractor`：终端快照清理、差分和去重。
- `ConversationRouter`：IM 会话与 Herdr 目标之间的映射。
- `PolicyGuard`：输入权限、显式审批和危险动作限制。
- `IMAdapter`：不同 IM 平台的统一适配接口。
- `StateStore`：绑定、checkpoint、幂等键和恢复数据。

IM SDK、Herdr 协议、业务路由和内容提取不得混在一个 god object 中。

## Herdr API 使用规则

- 启动和重连后首先调用 `session.snapshot`，不要只依赖事件恢复状态。
- 状态订阅使用 `pane.agent_status_changed`；该订阅必须指定 `pane_id`。
- pane 创建、关闭、退出或 Agent 变化后，重新计算需要维护的状态订阅。
- 普通用户文本优先使用 `agent.prompt`，不要默认使用原始 pane 输入。
- 只有明确的 UI 控制动作才使用 `agent.send_keys`。
- 输出读取优先使用 `agent.read` 和 `recent_unwrapped`。
- 不依赖 `PaneReadResult.revision`：当前 Herdr 实现固定返回 `0`。
- 不依赖 `pane.output_changed`：当前只有 Schema 类型，没有可用的公共输出流。
- 对重放、重复状态事件和重复 IM webhook 必须做幂等处理。

## 安全要求

- IM token、签名密钥和用户凭据不得提交到仓库。
- 日志不得记录完整密钥、Cookie 或未经处理的敏感终端内容。
- 输入命令必须经过身份、会话绑定和动作权限校验。
- 默认拒绝未知用户、未知 IM 会话和未绑定的 Herdr pane。
- 高风险按键或审批操作应包含可审计的用户身份和动作记录。

## 验证要求

至少覆盖以下测试类型：

- Herdr NDJSON 请求、响应和错误解析。
- 长连接订阅确认与事件解析。
- 状态迁移和重复事件去重。
- 输出快照差分、ANSI/TUI 清理和截断策略。
- IM webhook 重复投递的幂等性。
- pane 创建、关闭、Agent 替换和 Herdr 重连恢复。
- 普通 prompt 与审批按键的权限隔离。

涉及真实 Herdr 的集成测试应能在没有 IM 网络访问时运行；IM 平台调用使用 mock
或本地 fake adapter。
