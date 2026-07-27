# Herdr Pal Bridge 架构

## 1. 架构目标

Herdr Pal 由中央服务端和本机客户端组成。服务端集中持有企业微信 Bot ID、Secret 和唯一
长连接；客户端只访问本机 Herdr 公共 Socket，并通过加密 Relay 上报会话和执行用户请求。

```text
┌───────────────────────┐
│ 企业微信智能机器人    │
│ 单个 API 长连接       │
└───────────┬───────────┘
            ▼
┌──────────────────────────────────────────────────────┐
│ herdr-pal-server                                     │
│ WeComClient ─ ConversationRouter ─ UserExecutor      │
│                         │                            │
│ SessionCatalog ───── ClientHub ─ TLS/WSS             │
└───────────────┬───────────────────────┬──────────────┘
                │                       │
                ▼                       ▼
┌──────────────────────────┐  ┌──────────────────────────┐
│ herdr-pal: home-mac      │  │ herdr-pal: office-pc    │
│ RelayClient              │  │ RelayClient              │
│ Service / Notifier       │  │ Service / Notifier       │
│ SessionRegistry          │  │ SessionRegistry          │
│ EventSupervisor          │  │ EventSupervisor          │
│ HerdrClient              │  │ HerdrClient              │
└────────────┬─────────────┘  └────────────┬─────────────┘
             │ Local Socket                │ Local Socket
             ▼                             ▼
          Herdr                         Herdr
```

另有独立的 `herdr-pal -i` 入口，把 stdin/stdout 适配为本机聊天框，直接复用客户端 Bridge
核心，不启动企业微信或 Relay。

固定边界：

- 不修改 Herdr，不使用 MCP、plugin startup hook、私有 TUI socket 或内部 Rust 模块。
- 不把 Herdr 原始 Socket、任意 RPC 或本地文件路径暴露到 Relay。
- 网络只允许 WSS，不实现明文 WS。
- 不把终端快照描述成结构化 LLM 消息或完整对话记录。
- 不自动批准权限请求。

## 2. 服务端模块

### 2.1 WeComClient

职责：

- 独占企业微信智能机器人的 API 长连接。
- 完成订阅、心跳、`req_id` 关联、文本回调解析、主动发送和重连。
- 把平台消息转换为 `im.IncomingText`，不理解 Herdr 或 Relay 协议。

服务端不配置 `allowed_user_id`。企业微信应用可见范围决定哪些用户能给机器人发消息，
Router 只接受单聊文本。Bot Secret 只从 `HERDR_PAL_WECOM_SECRET` 读取。

### 2.2 ConversationRouter

职责：

- 以 userid 为路由和隔离边界。
- 直接处理 `/userid`、`/ls`、`/N`/`/sel N` 和 `/help`。
- 选择成功后向目标客户端执行一次 `/con`，在同一条回复中返回最新终端页。
- 将其他输入转发到当前稳定选择所在的机器。
- 接收客户端后续回复和状态通知，按稳定来源补充机器、Workspace/Tab 和 pane 后发送给正确用户。
- 在主动通知前使用最新目录复核目标，补充机器、本地序号和 panel 标题。
- 为企业微信终端页建立或复用全局编号，在消息开头和末尾显示 `[终端输出#N]`；输出来源与
  当前选择不同时，提示用户使用对应 `/N` 切换。

Router 不解析 `/con`、`/key` 或普通 prompt 的本地语义。这些内容必须原样交给目标机器，
由已有 Bridge 策略处理。

### 2.3 UserExecutor

每个 userid 使用独立串行队列，保证该用户的 `/ls`、选择和执行请求有一致顺序；不同用户
可以并行。队列容量有界，满载时明确拒绝，不创建无界 goroutine。

### 2.4 SessionCatalog

目录只保存在线状态：

```text
ClientKey = userid + machine_id

SessionRef
  machine_id
  local_index
  pane_id
  occupant_hash
```

每个连接只能更新自己的 `ClientKey`。同一用户的全局 `/ls` 将该用户所有机器的当前会话
排序并建立临时编号快照；`/N` 解析后保存完整 `SessionRef`，后续执行不依赖会变化的全局
数字。

以下事件立即使目录项或选择失效：

- Relay 连接关闭或心跳超时。
- 客户端快照删除 pane。
- pane 中 occupant 被替换。
- Herdr 进入 degraded，客户端上报空快照。

不同用户可以使用相同 `machine_id`；同一 `(userid, machine_id)` 的后来连接被拒绝。

### 2.5 ClientHub

职责：

- 只在 TLS 请求上接受 WebSocket upgrade。
- 校验 `client_hello`、首个完整快照、帧版本、大小、字段和身份。
- 维护每个客户端的有界发送队列、在途请求和心跳状态。
- 将选择与执行请求关联到对应响应。
- 复核 `execute_push` 和 `notification` 携带的稳定目标，再交给 Router。
- 连接结束时同步撤下目录，不保留离线消息或执行队列。

默认心跳间隔 10 秒，30 秒未收到 pong 关闭连接；完整会话快照校准间隔 30 秒。

### 2.6 TLS 管理

服务端可以加载显式 `cert_file`/`key_file`。二者都留空时，在 `state_dir` 生成并复用
ECDSA P-256 自签名证书，私钥写入权限为 `0600`。TLS 最低版本为 1.2。

自动证书只覆盖 localhost、本机 hostname 和 loopback 地址。客户端默认
`skip_verify=true` 是当前受信任内网的产品决定，不构成服务端身份认证。

## 3. 客户端模块

### 3.1 RelayClient

职责：

- 使用配置的 userid 和 machine_id 建立 WSS；machine_id 留空时由配置加载器使用系统
  hostname。
- 完成握手并立即上报本机完整会话快照。
- 默认每 250ms 检查会话变化，变化时发送新快照；按服务端间隔发送完整校准快照。
- 接收稳定目标选择和用户执行请求。
- 把 Bridge 首段回复、后续分段和结构化通知发回服务端。
- 断线后指数退避重连，成功握手后重置退避。

客户端不缓存离线 prompt、按键、回复或通知。断线期间产生的内容直接失败，避免重连后
执行过时动作。

### 3.2 HerdrClient

职责：

- 按“显式路径、公共 CLI、平台默认路径”的顺序解析 Herdr endpoint；Unix 默认回退为
  `$HOME/.config/herdr/herdr.sock`，Windows 按 Herdr 的环境变量优先级推导 marker 路径，
  命名 session 均不猜测路径。
- Unix 使用 Unix Domain Socket；Windows 将 marker 路径映射为
  `\\.\pipe\<marker path>` 并使用 Named Pipe。协议层不感知平台差异。
- 编码、发送和解析公共 NDJSON 请求。
- 维护 `events.subscribe` 长连接。
- 解析 protocol 17 的 `state_change_seq`，执行 prompt wait 和状态序列轮询。
- 将协议错误转换为稳定的内部错误类型。

普通文本使用 `agent.prompt`，UI 控制使用 `agent.send_keys`。所有 Agent API 使用 pane ID；
terminal ID 和 occupant 只用于身份复核。

### 3.3 SessionRegistry

职责：

- 使用 `session.snapshot` 建立本地运行时视图。
- 按 pane、terminal 和 Agent occupant 建立索引。
- 保存 workspace、tab、Agent、panel 标题和状态等展示字段。
- 应用 pane 生命周期事件，使被关闭或替换的目标失效。

Registry 是可重建缓存，不是事实来源。Herdr 不可用时，对 Relay 暴露空目标；重连后以
新快照完全替换旧状态。

### 3.4 EventSupervisor

每个健康周期：

1. `ping` 并要求 protocol 精确等于 17。
2. 读取 discovery `session.snapshot`。
3. 建立通用 pane lifecycle 订阅。
4. 为当前 Agent pane 建立批量 `pane.agent_status_changed` 订阅，每项显式指定 `pane_id`。
5. 再读取权威 snapshot，消除 snapshot 与订阅确认之间的窗口。
6. pane/occupant 集合变化时重建状态订阅并再次收敛。
7. 健康周期内每 10 秒主动读取一次权威 snapshot，弥补 lifecycle 事件缺失或延迟。
8. 设置健康 HerdrClient，开放输入和 Relay 目录上报。

断线时立即进入 degraded、撤下 Relay 会话并暂停输入。重连不恢复旧选择、分页或通知。

### 3.5 Service 与 PolicyGuard

Service 处理从 Relay 或 ConsoleAdapter 进入的本地命令：

- 每条消息先做 userid、单聊和 `msgid` 幂等校验。
- `/ls` 和 `/sel` 在交互模式中使用本机编号；网络模式由服务端先完成全局选择，再用稳定
  目标建立本地选择。
- `/con`、分页、按键和 `/slash` 都复用同一命令解析和安全策略。
- 每次 prompt 或按键前重新确认 pane、terminal 和 occupant。

普通 prompt 只接受实时 `idle` 或 `done`：

1. 调用带 wait 的 `agent.prompt`。
2. 只有状态或 `state_change_seq` 变化才报告成功。
3. 若返回 `agent_prompt_stalled`，再次复核 occupant、状态和序列。
4. 仍为原目标和可输入状态时，只补发一次受审计 Enter。
5. 再等待最多约 5 秒；无变化则报告未生效，不继续重试。

显式按键不二次确认，但只允许预定义白名单，并在每个按键前复核目标。多键间隔 100ms，
`enter` 不能出现在多键队列。

### 3.6 Notifier 与面板输出

Notifier 在 `blocked`、`done`，以及需要输出的 `idle` 状态读取
`agent.read(recent_unwrapped, 100)`：

- 只保留最近 100 行，不读取全部新增历史。
- 规范化换行与 ANSI/TUI 噪音。
- 使用状态、目标和快照 hash 做内存去重。
- 按企业微信限制进行 UTF-8 安全分段。
- 不改变用户手工 `/con`、`/pageup`、`/pagedn` 的分页位置。

`PaneReadResult.revision` 当前固定为 0，且没有可用的 `pane.output_changed` 公共流，因此
不能宣称通知内容是精确增量。

## 4. Relay 协议

Relay protocol 3 使用严格 JSON WebSocket 文本帧，包含固定版本和类型。核心消息：

```text
client_hello       client → server
server_hello       server → client
session_snapshot   client → server
ping / pong        server ↔ client
select_request     server → client
select_result      client → server
execute_request    server → client
execute_response   client → server
execute_push       client → server
notification       client → server
protocol_error     双向错误报告
```

`execute_push` 与 `notification` 都携带稳定 `SessionRef`，使服务端在用户切换选择后仍能
正确标注消息来源。`execute_response` 可以携带可选的 `selected_target`：仅用于 prompt
成功后同一机器、同一 pane 内的 Agent 会话切换。客户端必须先上报包含新 occupant 的完整
快照，服务端再校验旧选择没有被用户改到其他会话，并原子重绑。

协议不传递 Herdr Socket 路径，不提供通用 Herdr RPC，也不允许客户端在连接内切换 userid
或 machine_id。未知字段、未知类型、非法 SessionRef 和超限帧均拒绝。

Relay 请求语义是 at-most-one automatic attempt：服务端超时时提示“操作可能已经提交”，
不会自动重试可能产生副作用的执行。协议不承诺 exactly-once；业务层仍依赖 `msgid` 幂等、
稳定 occupant 校验和有界队列。

## 5. 关键数据流

### 5.1 用户发现和全局列表

```text
企业微信 /userid
  → WeComClient 提供回调 userid
  → Router 原样回复给当前单聊

企业微信 /ls
  → UserExecutor 按 userid 串行化
  → SessionCatalog 汇总该用户全部在线 ClientKey
  → 创建全局编号快照
  → 返回 [machine/local] Agent — panel title
```

### 5.2 选择和执行

```text
/N
  → 从最近全局编号快照解析 SessionRef
  → ClientHub 向目标机器发送 select_request
  → 客户端用 pane + occupant 建立本地稳定选择
  → 服务端保存用户选择
  → execute_request(/con)
  → 返回最新 100 行终端页

普通文本或本地命令
  → 服务端取得已选 SessionRef
  → execute_request
  → 客户端再次选择同一稳定目标
  → Service 执行状态、权限和 occupant 校验
  → 若 prompt 后同 pane 会话切换，立即上报快照并回传 selected_target
  → 服务端等待新目标进入目录，并在旧选择未被覆盖时原子重绑
  → execute_response / execute_push
  → 企业微信回复
```

### 5.3 状态通知

```text
pane.agent_status_changed
  → Notifier 复核本地 occupant
  → 按策略读取最近 100 行
  → Relay notification(SessionRef, content)
  → 服务端用最新目录复核
  → 补充 [machine/local] Agent — title
  → 主动发送给连接所属 userid
```

## 6. 重连、一致性和幂等

- Herdr 重连：新 snapshot 完全替换本地 Registry，旧选择和分页失效。
- Relay 重连：重新握手并发送完整会话快照，不恢复服务端旧选择。
- Relay 断开：服务端立即删除该 ClientKey 的全部会话。
- 企业微信重连：只恢复当前连接，不补发断线期间的状态通知。
- 入站 `msgid`：带 TTL 和容量上限的内存去重。
- 状态通知：目标、状态和最近快照 hash 的内存去重。
- 进程重启：所有选择、目录、分页和幂等状态清空。

## 7. 安全边界

- 企业微信 Secret 只在服务端环境变量中；客户端永远不持有。
- WSS 只提供链路加密。当前客户端可以自行声明 userid，且默认忽略证书验证，因此部署
  必须限制在受信任内网和受控主机。
- 日志仅记录错误类别、长度、连接 ID、machine_id 和 userid/occupant 摘要，不记录完整
  Secret、Cookie、prompt 或终端快照。
- 外部字符串不能映射成任意 Herdr key；只允许固定按键白名单。
- 未知用户会话、群聊、未知 pane、失效 occupant、degraded Herdr 和断线客户端都不能
  产生输入。
- 不提供 `server.stop`、`pane.close`、`pane.send_text`、通用 `pane.send_input` 或自动
  审批入口。

## 8. 测试策略

单元测试至少覆盖：

- Relay 严格帧、版本、身份、SessionRef 和大小限制。
- 重复 ClientKey 拒绝、首快照、心跳、断线撤下和有界队列。
- 多用户目录隔离、全局编号快照、稳定选择和过期目标。
- 客户端快照变化、校准、重连、选择和执行响应。
- Herdr NDJSON、订阅确认、事件解析、状态迁移和重连恢复。
- prompt 状态门禁、stalled 单次 Enter、按键白名单和审计。
- 100 行通知、分页、ANSI/TUI 清理和 UTF-8 分段。
- 企业微信重复回调幂等和不同用户串行/并行行为。

集成测试使用本地 TLS Relay、三个客户端和 fake Bridge 执行器验证：

- 同一用户两台机器的列表聚合。
- 不同用户使用相同 machine_id。
- 选择和 prompt 不跨用户、不跨机器。
- 通知包含机器、本地序号、Agent 和 panel 标题。

真实 Herdr 集成测试必须能在不访问企业微信外网时运行；写入测试需要显式 pane ID 和额外
环境门禁。

## 9. 后续演进

优先级较高的后续能力：

- Relay 客户端认证、证书 pinning 或内部 CA。
- 可选 StateStore 和跨重启最小绑定恢复。
- 可靠通知队列与明确的投递状态。
- 服务管理、健康检查和配置迁移。
- 第二个网络 IM Adapter。

任何实时结构化输出需求都应先形成 Herdr 公共 API 提案，不能回退到私有 TUI 状态或内部
Rust 模块。
