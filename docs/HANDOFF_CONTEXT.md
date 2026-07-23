# Herdr Pal 交接上下文

## 1. 用户目标

用户希望把 Herdr 与传统 IM 软件结合，实现：

1. Agent 状态变化通知到 IM。
2. 在 IM 中查看 Agent 的阶段性输出。
3. 从 IM 向 Agent 发送输入。
4. 在可控和安全的前提下处理 Agent 的 blocked 交互。

Herdr 是一个类似 tmux、针对 AI Agent 优化的终端管理工具。目标不是替代 Herdr，
而是在 Herdr 之外增加一个常驻 bridge。

## 2. 已完成的判断过程

### 2.1 Plugin 路线

Herdr plugin 可以接收事件 hook，并通过 `HERDR_PLUGIN_EVENT_JSON` 获得事件内容。
但是 plugin startup hook 是一次性初始化命令，不是受 Herdr 监管的常驻 daemon。

结论：plugin 可用于简单的单向通知或安装辅助，但不适合承担长期 IM 连接、双向路由、
断线恢复和持久化。

### 2.2 MCP 路线

Herdr 当前不是 MCP Server，但它已经有完整的本地 Socket API。可以额外适配 MCP，
但传统 IM 平台通常不是 MCP Client，MCP 也不能替代 IM webhook、bot SDK 和通知路由。

用户明确决定：本项目不走 MCP 路线。

### 2.3 本地 API 路线

源码审计确认本地 API 已经覆盖：

- `session.snapshot` 初始化。
- Agent 和 pane 查询。
- Agent 状态长连接订阅。
- pane 生命周期事件。
- Agent/pane 终端快照读取。
- Agent prompt、按键和原始 pane 输入。
- 状态等待和输出文本匹配。

结论：第一版 Herdr Pal 不需要修改 Herdr。

## 3. 当前正式决策

- 新工程目录为 `/Users/wxc/Code/herdr-pal`。
- Herdr 源码目录为 `/Users/wxc/Code/herdr`。
- Herdr Pal 是独立进程，不在 Herdr 仓库内开发。
- Herdr Pal 使用 Herdr 公共本地 Socket API。
- 不使用 MCP 作为 Herdr 集成层。
- 不使用 plugin startup hook 承载常驻 bridge。
- 第一版采用“状态事件触发输出快照读取”。
- 第一版使用 `agent.prompt` 处理普通 IM 文本。
- 第一版只对显式动作使用 `agent.send_keys`。
- 第一版不自动批准权限请求。
- 当前只创建文档，不初始化代码、依赖、Git、build 脚本或测试脚本。
- IM 平台和技术栈尚未决定。

## 4. 关键技术事实

### 4.1 状态事件可用

`events.subscribe` 可以长连接推送 `pane.agent_status_changed`。状态包括
`working`、`blocked`、`idle`、`done`、`unknown`。

限制：状态订阅必须指定 pane，没有全局通配符。状态相同但展示元数据变化时也可能
收到事件。

### 4.2 输入通路可用

`agent.prompt` 会验证当前 Agent occupant、处理 bracketed paste 并追加 Enter，适合
普通 IM 消息。

`agent.send_keys` 适合交互 UI。`pane.send_input` 等原始接口不验证 Agent occupant，
不应作为普通消息的默认通路。

### 4.3 输出只能读取终端快照

`agent.read` 和 `pane.read` 可以读取 visible、recent、recent_unwrapped、detection。
这不是结构化 LLM output，内容可能混合 prompt、回复、工具日志、spinner 和 TUI。

全屏 Agent 的 alternate-screen 历史可能丢失。

### 4.4 没有实时增量输出

当前没有可订阅的通用 `pane.output_changed` 流，也没有公开原始 PTY 字节。
`PaneReadResult.revision` 当前固定为 0，bridge 必须自行差分和去重。

### 4.5 事件不能断点续传

Herdr EventHub 只保留有限内存事件，并且不公开事件 cursor。重连后必须重新获取
`session.snapshot`，重建订阅并处理重复通知。

## 5. 推荐 MVP 行为

```text
启动
  → session.snapshot
  → 找到当前 Agent pane
  → 订阅 pane 生命周期
  → 为 Agent pane 订阅状态

working
  → 更新 IM 状态

blocked
  → 读取 recent_unwrapped
  → 发送阻塞通知和近期输出
  → 展示安全的显式操作入口

done / idle
  → 读取 recent_unwrapped
  → 和上次快照做差分
  → 发送新增内容

IM 普通回复
  → 身份和绑定校验
  → agent.prompt

IM 控制按钮
  → 风险校验
  → agent.send_keys
```

## 6. 明确不承诺的能力

- LLM token streaming。
- 完整原生对话 transcript。
- 精确区分 user/assistant/tool message。
- exactly-once 事件交付。
- 自动处理所有 Agent 的权限 UI。
- 在没有显式绑定时允许任意 IM 用户控制本机终端。

## 7. 尚未决定的问题

开始代码设计前，应逐项确认：

1. 第一版 IM：企业微信、飞书、钉钉、Slack、Telegram 或其他平台。
2. 运行时语言：Rust、Go、TypeScript/Node.js 或其他。
3. IM 接入模式：公网 webhook、WebSocket/长连接、轮询或内网代理。
4. 部署模式：用户登录进程、systemd/launchd、容器或手工运行。
5. 绑定 UX：配置文件、IM 命令、Herdr pane 元数据或本地管理页。
6. 是否需要多用户和角色权限。
7. 是否需要 SQLite 等本地持久化。
8. 输出通知采用新消息、更新同一消息还是 thread 回复。

## 8. 新会话建议起点

在 `/Users/wxc/Code/herdr-pal` 启动新的开发会话后，建议先让 Agent 阅读：

```text
请先阅读 README.md、AGENTS.md、docs/HANDOFF_CONTEXT.md、
docs/HERDR_API_AUDIT.md 和 docs/BRIDGE_ARCHITECTURE.md。
本项目不修改 ../herdr，通过 Herdr 本地 Socket API 实现 IM bridge。
在写代码前，先和我确认第一版 IM 平台、技术栈和 MVP 范围。
```

确定技术栈后，应再次从实际运行的 Herdr 二进制导出 Schema：

```bash
herdr api schema --json
herdr api snapshot
```

不要仅根据本文手写所有协议类型；应根据 Schema 生成或校验客户端模型，并为关键
兼容性行为保留 fixture 测试。

## 9. 源码审计依据

Herdr 基线：

```text
/Users/wxc/Code/herdr
commit 2a20e90
protocol 17
audited 2026-07-23
```

重点文件：

- `src/api/schema.rs`
- `src/api/schema/events.rs`
- `src/api/schema/agents.rs`
- `src/api/schema/panes.rs`
- `src/api/schema/session.rs`
- `src/api/server.rs`
- `src/api/client.rs`
- `src/api/subscriptions.rs`
- `src/api/wait.rs`
- `src/app/api.rs`
- `src/app/api/agents.rs`
- `src/app/api/panes.rs`
- `src/app/api_helpers.rs`
- `src/api/event_hub.rs`
- `src/pane.rs`
- `docs/next/website/src/content/docs/socket-api.mdx`
- `docs/next/website/src/content/docs/agent-automation.mdx`
- `docs/next/website/src/content/docs/plugins.mdx`

详细结论见 `docs/HERDR_API_AUDIT.md`。
