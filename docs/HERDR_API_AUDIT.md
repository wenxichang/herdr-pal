# Herdr 本地 API 审计

## 1. 审计范围

本文记录 Herdr Pal 创建前对 Herdr 本地 API 的源码审计结果。

- Herdr 源码目录：`/Users/wxc/Code/herdr`
- 基线提交：`2a20e90 fix: preserve physical escape on windows`
- 审计日期：`2026-07-23`
- Herdr Pal 实现复核：`2026-07-24`
- 本文中的源码路径均相对于 Herdr 仓库根目录。

结论只覆盖该基线源码。运行时应通过 `herdr api schema --json` 检查已安装二进制
实际支持的协议。

## 2. 协议和传输

Herdr 提供本地 Socket API：

- Unix 使用本地 Socket 文件；服务端将权限限制为 `0600`。
- Windows 使用 Named Pipe；Herdr 把 CLI 返回的 marker 文件路径作为 namespaced 名称，
  实际端点为 `\\.\pipe\<marker path>`。
- 普通请求和响应使用换行分隔 JSON（NDJSON）。
- 请求格式是 Herdr 自定义的 `{id, method, params}`，不是 JSON-RPC。
- 普通连接通常发送一个请求并接收一个响应。
- `events.subscribe` 保持连接并持续推送事件 JSON 行。

请求示例：

```json
{"id":"req-1","method":"agent.list","params":{}}
```

成功响应使用相同的 `id`：

```json
{"id":"req-1","result":{"type":"agent_list","agents":[]}}
```

错误响应包含错误码和消息：

```json
{"id":"req-1","error":{"code":"agent_not_found","message":"..."}}
```

主要源码依据：

- `src/api/schema.rs:34`：请求结构和方法枚举。
- `src/api/server.rs:139`：读取首个 JSON 行并分派请求。
- `src/api/server.rs:645`：长连接事件订阅。
- `src/api/server.rs:28`：Unix Socket 权限模式。
- `src/api/client.rs:31`：Herdr 内部可复用 NDJSON 客户端。
- `src/ipc.rs:31`：Windows `GenericNamespaced` Named Pipe 映射。

## 3. 会话初始化

外部客户端应首先调用：

```json
{"id":"bootstrap","method":"session.snapshot","params":{}}
```

返回内容包括：

- Herdr 版本和协议版本。
- 当前聚焦的 workspace、tab 和 pane。
- workspace、tab、pane 列表。
- tab 布局快照。
- Agent 列表及其当前状态。

`SessionSnapshot` 定义在 `src/api/schema/session.rs:8`。

快照不是订阅。客户端读取快照后仍需建立事件订阅；断线或本地缓存可能过期时，
必须重新读取快照。

## 4. API 功能概览

### 4.1 Server 和 Session

- `ping`
- `server.stop`
- `server.live_handoff`
- `server.reload_config`
- `server.agent_manifests`
- `server.reload_agent_manifests`
- `session.snapshot`

### 4.2 Workspace、Worktree 和 Tab

- Workspace：create、list、get、focus、rename、move、metadata、close。
- Worktree：list、create、open、remove。
- Tab：create、list、get、focus、rename、move、close。

这些接口可用于展示上下文和维护 pane 身份，但 Herdr Pal 第一版不必提供完整
终端管理 UI。

### 4.3 Pane

主要能力包括：

- list、current、get、process_info。
- split、swap、move、zoom、layout、neighbor、edges、resize。
- read、wait_for_output。
- send_text、send_keys、send_input。
- report_agent、report_agent_session、report_metadata。
- created、updated、focused、moved、exited、closed 等生命周期。

### 4.4 Agent

- `agent.list`
- `agent.get`
- `agent.read`
- `agent.explain`
- `agent.send_keys`
- `agent.prompt`
- `agent.wait`
- `agent.rename`
- `agent.focus`
- `agent.start`
- `agent.view.set`
- `agent.view.clear`

### 4.5 Events

- `events.subscribe`：长连接、多订阅事件流。
- `events.wait`：一次性等待；当前服务端实际上只支持 Agent 状态匹配。

完整方法枚举位于 `src/api/schema.rs:45`，用户文档位于
`docs/next/website/src/content/docs/socket-api.mdx:93`。

## 5. Agent 状态感知

### 5.1 状态集合

公共 `AgentStatus` 包含：

- `working`：Agent 正在工作。
- `blocked`：检测到审批、提问或需要用户介入的界面。
- `idle`：Agent 可接受输入，并且对应标签页已经被用户查看。
- `done`：底层同样为空闲，但后台完成后尚未被查看。
- `unknown`：识别到 Agent，但无法可靠分类其生命周期。

定义位于 `src/api/schema/common.rs:142`。

### 5.2 状态订阅

```json
{
  "id": "status-sub",
  "method": "events.subscribe",
  "params": {
    "subscriptions": [
      {
        "type": "pane.agent_status_changed",
        "pane_id": "w1:p1"
      }
    ]
  }
}
```

服务端先返回订阅确认，随后推送：

```json
{
  "event": "pane.agent_status_changed",
  "data": {
    "pane_id": "w1:p1",
    "workspace_id": "w1",
    "agent_status": "blocked",
    "agent": "claude",
    "title": "需要确认",
    "display_agent": "Claude",
    "state_labels": {}
  }
}
```

事件结构位于 `src/api/schema/events.rs:393`，事件生成逻辑位于
`src/app/api.rs:554`。

### 5.3 状态订阅限制

- `pane.agent_status_changed` 必须指定 `pane_id`，没有全局通配订阅。
- 一个订阅连接的订阅列表在连接建立后不能动态追加。
- pane 创建、关闭或 Agent 替换后，bridge 必须重建订阅或新建连接。
- 状态相同但 title、display_agent、state_labels 变化时也可能收到该事件。
- 如果只关心状态迁移，bridge 必须保存并比较上一次状态。

建议同时订阅：

- `pane.created`
- `pane.closed`
- `pane.exited`
- `pane.agent_detected`
- `pane.updated`

### 5.4 事件恢复限制

`EventHub` 只在内存中保留最近 512 个事件，内部 sequence 没有暴露给外部客户端。
不同订阅实现的起点不能混为一谈：

- 专用 `pane.agent_status_changed` 订阅在创建时记录 EventHub 的 `current_sequence`，从
  该位置开始接收后续状态事件；它不会从 sequence 0 重放订阅前保留的状态事件。
- 通用 pane lifecycle 订阅内部从 sequence 0 读取当前保留队列，因此连接建立后可能
  先看到保留的 `pane.created`、`pane.updated` 等事件，不能假定全部都是严格意义上的
  “连接后新事件”。

断线恢复必须：

1. 重新调用 `session.snapshot`。
2. 用快照替换本地运行时状态。
3. 重建 pane 级状态订阅。
4. 对可能重复的通知做幂等去重。

相关实现位于 `src/api/event_hub.rs:1` 和 `src/api/subscriptions.rs:328`。

## 6. 输入能力

### 6.1 Agent target 语义

最新源码与真实 Herdr 0.7.5/protocol 17 联调确认，Agent API 的 `target` 支持：

- 当前 session 中的公共 pane ID。
- 能唯一匹配一个 Agent pane 的 Agent name。

Agent name 出现多个匹配时会返回歧义错误。terminal ID 虽然出现在 snapshot 和
`AgentInfo` 中，但不是 Agent API target；把 terminal ID 传给 `agent.get`、
`agent.read`、`agent.prompt` 或 `agent.send_keys` 会返回 `agent_not_found`。

因此 Herdr Pal 对上述四个 Agent API 一律传递 pane ID。terminal ID 仍与 Agent name、
display name 和 agent session reference 一起构成 occupant 身份，只用于在输入或输出
前后判断 pane 中的 Agent 是否已被替换，不能替代 pane ID 发起调用。

解析逻辑位于 `src/app/terminal_targets.rs:75`。其中 `resolve_agent_target` 只尝试公共
pane ID 和 Agent name；允许 terminal ID 的 `resolve_terminal_target` 是另一套目标解析
逻辑，不能据此推断 Agent API 接受 terminal ID。

### 6.2 普通 IM 文本

推荐映射到：

```json
{
  "id": "prompt-1",
  "method": "agent.prompt",
  "params": {
    "target": "w1:p1",
    "text": "请继续处理并告诉我测试结果"
  }
}
```

`agent.prompt` 会：

- 解析并验证目标 Agent。
- 检查该 Agent 是否仍控制 pane 的前台进程。
- 根据实时 bracketed-paste 模式编码文本。
- 自动追加 Enter。
- 可选地在同一请求中等待 `idle`、`done`、`blocked` 等状态。

实现位于 `src/app/api/agents.rs:58` 和 `src/app/api_helpers.rs:25`。

带等待的请求示例：

```json
{
  "id": "prompt-2",
  "method": "agent.prompt",
  "params": {
    "target": "w1:p1",
    "text": "运行测试",
    "wait": {
      "until": ["idle", "working", "blocked", "done", "unknown"]
    }
  }
}
```

Herdr Pal 不显式传 `timeout_ms`，使用 Herdr prompt effect 固定的约 5 秒窗口。成功响应
必须是包含完整 `AgentInfo` 的 `agent_prompted`，其中 protocol 17 的
`state_change_seq` 用于确认请求提交后确实发生了 Agent 生命周期变化；无变化时 Herdr
返回 `agent_prompt_stalled`。

bridge 只在初始实时状态为 `idle` 或 `done` 时提交普通文本。收到 stalled 后先重新调用
`agent.get`：occupant、序列或状态不符合预期时不发送按键；仍为原 occupant、原序列且
仍是 `idle`/`done` 时，只补发一次受审计的 `enter`，随后最多轮询 5 秒
`state_change_seq`。`blocked` 不会触发自动 Enter。

### 6.3 交互式 UI 按键

```json
{
  "id": "keys-1",
  "method": "agent.send_keys",
  "params": {
    "target": "w1:p1",
    "keys": ["down", "enter"]
  }
}
```

适用于：

- 选择 Agent 提供的选项。
- `esc`、方向键、Enter、`ctrl+c` 等 UI 操作。
- 用户明确点击 IM 中的审批或控制按钮。

安全要求：不能把 `blocked` 自动等同于“发送 Enter”。不同 Agent 和不同页面的
默认选项可能具有完全不同的风险。

### 6.4 原始 Pane 输入

- `pane.send_text`：发送原始文本，不追加 Enter。
- `pane.send_keys`：发送按键序列。
- `pane.send_input`：一次发送文本和按键。

这些接口不验证当前 pane occupant，应该作为高级或受控能力，不作为普通 IM 文本的
默认通路。实现位于 `src/app/api/panes.rs:1463`。

## 7. 输出能力

### 7.1 快照读取

`agent.read` 和 `pane.read` 返回解析后的终端屏幕或 scrollback 快照。

```json
{
  "id": "read-1",
  "method": "agent.read",
  "params": {
    "target": "w1:p1",
    "source": "recent_unwrapped",
    "lines": 120,
    "format": "text"
  }
}
```

可用 source：

| Source | 含义 |
| --- | --- |
| `visible` | 当前可见终端页面 |
| `recent` | 最近的屏幕和 scrollback，保留软换行 |
| `recent_unwrapped` | 最近的屏幕和 scrollback，去除软换行 |
| `detection` | Agent 状态检测使用的底部缓冲快照 |

格式支持 `text` 和 `ansi`。默认读取 80 行，单次最多请求 1000 行。实现位于
`src/app/api_helpers.rs:104`。

Herdr 默认每个 pane 保留 10,000,000 字节 scrollback，但实际可读历史仍受终端
alternate screen 影响。配置定义位于 `src/config/model.rs:855`。

### 7.2 输出不是 LLM 消息

快照可能包含：

- 用户 prompt。
- Agent 回复。
- 工具调用日志。
- spinner 和状态栏。
- 权限请求或问题界面。
- TUI 边框、局部重绘和重复内容。

因此输出只能称为“终端快照”，不能直接标注为结构化 assistant message。

Claude Code、OpenCode 等全屏 Agent 可能使用 alternate screen。离开页面后，旧内容
不一定进入 Herdr scrollback；增加 `lines` 无法恢复已经丢失的 alternate-screen 行。
相关说明位于 `docs/next/website/src/content/docs/agent-automation.mdx:84`。

### 7.3 当前没有通用实时输出流

虽然 Schema 中定义了 `PaneOutputChanged`，但当前基线中：

- `events.subscribe` 没有 `pane.output_changed` 订阅类型。
- 没有发现生产代码发布该事件。
- plugin hook 明确排除了高频 output-change 事件。
- 原始 PTY 字节只进入终端解析器，没有通过公共 API 暴露。

PTY 入口位于 `src/pane.rs:1913`；事件类型位于
`src/api/schema/events.rs:171`。

### 7.4 当前没有可用增量游标

`PaneReadResult` 定义了：

```text
revision: u64
truncated: bool
```

但 `agent.read` 和 `pane.read` 当前固定返回：

```text
revision: 0
truncated: false
```

实现位置：

- `src/app/api/agents.rs:127`
- `src/app/api/panes.rs:1189`

因此不能用服务端 revision 做输出 checkpoint 或断点续传。Herdr Pal 当前也不实现可靠
增量差分：主动通知只规范化最近 100 行，并对整份规范化快照做 hash 去重。未来若需要
增量输出，可以在本地保存上一份规范化快照并进行保守差分，但差分结果不能伪装成
Herdr 提供的精确增量游标。

### 7.5 输出匹配

`pane.output_matched` 和 `pane.wait_for_output` 可以搜索已知字符串或单行正则。

适合：

- 已知权限提示。
- 构建完成标识。
- 固定错误文本。
- 特定 CLI 问题。

它们不等于输出流。订阅实现会轮询终端快照，并在“未匹配到 → 匹配到”时发出事件。
实现位于 `src/api/subscriptions.rs:340`。

## 8. 对 Herdr Pal 的直接结论

第一版可以完全通过现有 API 实现：

```text
状态变化事件
    → 读取 recent_unwrapped 快照
    → 取最近 100 行并规范化
    → 对整份快照做 hash 去重、分段和安全截断
    → 发送终端近期快照到消息入口

IM 普通文本
    → 校验用户和会话绑定
    → agent.get 校验 occupant 与 idle/done
    → 带 wait 的 agent.prompt 确认状态变化
    → stalled 时复核后最多补发一次 Enter
    → 再按 state_change_seq 确认结果

IM 显式控制按钮
    → 策略校验
    → agent.send_keys
```

暂时无法只依靠 Herdr API 实现：

- 逐 token 实时输出。
- 精确的 assistant turn 边界。
- 完整的原生 Agent transcript。
- 基于服务端 revision 的输出断点续传。
- exactly-once 事件交付。

这些限制应由产品文案、输出提取策略和重连逻辑明确处理，而不是在第一版中隐式承诺。

## 9. Herdr Pal 当前实现与验证

Herdr Pal 客户端固定 `RequiredProtocol = 17`，每次启动和重连都先调用 `ping` 做精确
协议门禁。只有门禁通过后才会读取 discovery snapshot、建立 lifecycle/status 订阅并
读取订阅后的权威 snapshot；pane/occupant 集合不稳定时继续重建状态订阅和 snapshot，
直到订阅计划与快照一致。

当前实现不依赖事件重放恢复状态：

1. 通用 lifecycle 保留事件只作为触发重新 snapshot 的信号。
2. 专用状态订阅只消费创建后的状态事件。
3. 启动和重连的通知基线来自订阅后的权威 snapshot，不发送历史状态通知。
4. 外部没有 cursor，重复 lifecycle、重复状态展示和重复企业微信回调分别由 snapshot
   收敛、状态迁移去重和 `msgid` 幂等处理。

测试目录中的 fake Herdr Server 只实现本文列出的公开 NDJSON protocol 17 子集：
`ping`、`session.snapshot`、`agent.get`、`agent.read`、`agent.prompt`、
`agent.send_keys`、`events.subscribe`、事件注入和订阅断线。它不导入、复制或模拟 Herdr
私有 `AppState`、PTY 或 Rust 模块。

fake Herdr 还可按公开响应模拟 `agent_prompt_stalled`，并在收到恢复 Enter 后更新
`state_change_seq`。端到端测试覆盖正常 prompt、working 状态拒绝，以及 stalled 后只
补发一次 Enter 并等待状态变化。

可选真实测试命令：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
go test ./internal/integration -run '^TestRealHerdr$' -count=1 -v
```

测试先执行公共 CLI `herdr status server --json`。只有运行中服务精确返回 protocol 17
才继续调用真实 `ping`、`session.snapshot`、`agent.get` 和 `agent.read`，并覆盖
`herdr-pal -i` 的 `/ls`、`/sel`、`/con` 只读路径；企业微信侧不参与，也不需要 Secret。

实时 prompt 另有三重显式门禁：

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
HERDR_PAL_LIVE_INPUT=1 \
HERDR_PAL_LIVE_PANE_ID='<必须替换为当前 pane ID>' \
go test ./internal/integration -run '^TestRealHerdrLivePrompt$' -count=1 -v
```

必须先从最新公共 `session.snapshot` 取得目标 pane ID 并替换占位符，禁止原样执行；未
替换或目标不存在时，测试应在 `agent.prompt` 前失败。测试只对通过校验的 pane ID 发送
一次固定 prompt，并通过最近 100 行快照等待新增 marker。

截至 2026-07-24，源码 debug Herdr 0.7.5/protocol 17 已完成上述只读与实时 prompt
联调。它位于 `/Users/wxc/Code/herdr/target/debug/herdr`，使用
`~/.config/herdr-dev`；Homebrew Herdr 0.7.1 位于 `/opt/homebrew/bin/herdr`，使用
`~/.config/herdr`。两套 CLI 的配置目录和 Socket 不同，真实测试必须通过 `PATH` 明确
选择 debug 二进制。所有测试和运行时仍保持 protocol 精确等于 17 的门禁，不接受“17
或更高”；输出侧也仍按本文既有结论处理：`revision` 固定为 0，且没有公共
`pane.output_changed` 输出流。
