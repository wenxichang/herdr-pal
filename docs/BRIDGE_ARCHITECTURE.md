# Herdr Pal Bridge 架构

## 1. 架构目标

Herdr Pal 由中央 Server 与每机 Pal sidecar 组成。Server 集中持有企业微信机器人连接；Pal
只访问本机 Herdr 公共 Socket，并通过 HPRP/1 WSS 上报会话与执行用户功能。

```text
企业微信智能机器人 ──▶ herdr-pal-server
                       ├── WeComClient / ConversationRouter / UserExecutor
hp-cli ── HPAP/1 ────▶ ├── AdminServer / CredentialStore
                       └── ClientHub / SessionCatalog
                                  │ HPRP/1 WSS
                     ┌────────────┴────────────┐
                     ▼                         ▼
             herdr-pal: office-pc      herdr-pal: home-mac
             RelayClient               RelayClient
             Service / Notifier        Service / Notifier
             EventSupervisor           EventSupervisor
                     │ Local Socket             │ Local Socket
                     ▼                          ▼
                   Herdr                      Herdr
```

固定边界：

- 不修改 Herdr，不使用 MCP、plugin startup hook、私有 TUI socket 或内部 Rust 模块。
- 不把 Herdr Socket、任意 Herdr RPC、本地路径或凭据暴露到远程网络。
- 只允许 WSS；终端内容是近期快照，不是结构化 LLM 消息或完整对话。
- 不自动批准权限请求。

## 2. 身份与连接模型

每台机器使用一把协议外签发的 Bearer Key。服务端凭据记录绑定：

```text
credential_id + principal_id + machine_id + secret_digest + allowed_sources
```

- `principal_id` 是企业微信回调中的用户 ID。
- `machine_id` 是管理员签发 Key 时确定的逻辑机器标识。
- Pal 配置只包含 `relay.url` 和 `relay.key`，不声明用户或机器身份。
- WebSocket Upgrade 使用 `Authorization: Bearer ...` 和
  `Sec-WebSocket-Protocol: herdr-pal-relay.v1`。
- 同一 `(principal_id, machine_id)` 只允许一条活动连接；重复连接返回 HTTP 409。
- HTTP Upgrade 成功后，Server 先保留身份，再完成 HPRP hello 与首快照同步。
- Key 无效、过期、禁用或来源不匹配统一返回 HTTP 401；日志只记录 `credential_id` 和身份摘要。

## 3. Server 模块

### 3.1 WeComClient

独占企业微信智能机器人长连接，处理订阅、心跳、请求关联、单聊文本回调和主动发送。它只
转换 `im.IncomingText`，不理解 Herdr 或 HPRP。

### 3.2 ConversationRouter 与 UserExecutor

Router 以企业微信用户 ID 为隔离边界：

- 直接处理 `/userid`、`/ls`、独立 `/N`/`/sel N` 和 `/help`。
- 把 `/N 内容`、`#N 内容` 的全局编号解析成 HPRP 稳定目标。
- 将其他输入路由到当前选择，并在成功后按命令语义更新选择。
- 为输出补充机器、全局序号、Workspace/Tab、pane 和状态信息。
- 维护同一用户 2 分钟双向活跃窗口，减少后台会话的大段输出打断。

每个用户由 `UserExecutor` 串行处理，不同用户并行；队列容量有界。

### 3.3 CredentialStore

保存每机 Key 的摘要、绑定身份和来源地址规则。所有变更由运行中的 Server 通过 HPAP 完成，
管理命令只在签发时输出一次明文 Key：

```sh
hp-cli key issue --principal-id USERID --machine-id MACHINE --source 192.168.1.20
```

凭据存储默认位于 `state_dir/credentials.json`，文件权限受限。验证使用常量时间摘要比较，并
同时核验 TLS 连接的真实来源地址。`hp-cli` 只连接 `<state_dir>/admin.sock`，不直接修改文件。

### 3.4 AdminServer

AdminServer 通过 HPAP/1 Unix Socket 向同一系统用户提供 Key CRUD、来源策略、连接与会话
查询、动态 debug 和优雅停止。它不提供远程管理端口，不读取终端内容，也不能发送 Agent
输入。Windows 当前不构建 Server 或 `hp-cli`。

### 3.5 ClientHub

ClientHub 实现 HPRP/1 Server 状态机：

```text
TLS/Upgrade 认证
  → hello.client / hello.server
  → session.snapshot / session.snapshot.result
  → READY
```

职责：

- 验证 TLS、子协议、Bearer Key、hello 和首个完整快照。
- 维护有界发送队列、在途请求、命令输出路由和 WebSocket heartbeat。
- `Select` 只复核在线目录，不向 Pal 发送远端选择消息。
- `Execute` 发送 `command.execute`，通过 `reply_to` 等待 `command.result`。
- 验证 `command.output` 与 `notification.event` 的机器身份、稳定目标、序号和幂等键。
- 连接结束时立即撤下机器及其全部会话。

### 3.5 SessionCatalog

目录保存每条连接最近确认的完整会话快照。稳定目标为：

```text
Target
  machine_id
  slot_id
  session_id
```

全局 `/ls` 数字只属于最近列表快照；真正选择保存完整 `Target`。快照序号相等是幂等重发，
较小序号被拒绝。pane 消失、Agent occupant 变化或连接断开都会使旧目标失效。

## 4. Pal 模块

### 4.1 RelayClient

职责：

- 携带 Bearer Key 建立 HPRP/1 WSS，并从 `hello.server` 学习可信 `machine_id`。
- 上报完整会话快照并等待 `session.snapshot.result`，确认后才能引用新目标。
- 默认每 250ms 检测本地目录变化，并发送周期性完整校准快照。
- 接收 `command.execute`，校验稳定目标后交给 Bridge Service。
- 先发送唯一 `command.result`，再发送可选、有序的 `command.output`。
- 使用 `idempotency_key` 的有界 TTL 缓存避免重复本地副作用。
- 上报带稳定目标的 `notification.event`；断线时不缓存任何输入、输出或通知。

### 4.2 HerdrClient、SessionRegistry 与 EventSupervisor

`HerdrClient` 只使用公共 NDJSON API。普通文本使用 `agent.prompt`，UI 控制使用
`agent.send_keys`。每次操作都以 pane ID 调用，并复核 occupant。

`SessionRegistry` 是可重建缓存，保存 pane、occupant、workspace、tab、Agent、标题和状态。
Herdr 不可用时向 HPRP 暴露空会话。

`EventSupervisor` 每个健康周期执行：

1. `ping` 并要求 protocol 精确为 17。
2. 读取 discovery snapshot。
3. 建立 pane lifecycle 和 `pane.agent_status_changed` 订阅。
4. 再读权威 snapshot 消除订阅窗口。
5. pane/occupant 变化后重建订阅。
6. 每 10 秒重读权威 snapshot。

### 4.3 Service、PolicyGuard 与 Notifier

Service 解析 `/con`、分页、`/key`、`/enter`、`/slash` 和普通 prompt。普通文本只允许
实时 `idle` 或 `done`；prompt stalled 时最多补发一次经过目标复核的 Enter。

按键只允许 `up/down/esc/space/enter` 与单个 ASCII 字母或数字。多键间隔 100ms，Enter
只能单独发送；每个按键前重新复核目标。

Notifier 对 `blocked`、`done` 和需要关注的 `idle` 读取最近 100 行，执行 ANSI/TUI 清理、
快照去重与 UTF-8 安全分段。它不改变用户手工分页位置。

## 5. HPRP/1 核心语义

HPRP/1 使用严格 JSON 文本信封：

```text
hello.client / hello.server
session.snapshot / session.snapshot.result
command.execute / command.result / command.output
notification.event
protocol.error
```

- 每个请求、响应和输出通过 `id/reply_to` 关联。
- 未知可选字段被忽略；未知必需消息返回协议错误并关闭连接。
- `command.execute` 是基础文本交互，不是通用 Herdr RPC。
- 相同命令幂等键和等价输入重放原结果；冲突输入明确拒绝。
- 输出和通知队列有界；序号重复被去重，跳号或跨机器目标被拒绝。
- 同一 pane 产生新 Agent 会话时，Pal 先确认快照，再返回同机器、同 slot 的替换目标。
- 超时或 `indeterminate` 不触发新的自动执行。

完整规范见 [HPRP/1 协议设计](HPRP_PROTOCOL_DESIGN.md)。

## 6. 重连与一致性

- Herdr 重连：新 snapshot 完全替换 Registry；旧目标和分页失效。
- HPRP 重连：重新认证、hello 和完整快照，不继承旧连接的服务端目录。
- HPRP 断开：Server 立即删除该机器全部会话。
- 企业微信重连：恢复平台连接，不补发断线期间的状态通知。
- 企业微信 `msgid`、HPRP 命令和通知分别在对应边界去重。
- 进程重启后选择、在线目录、分页和幂等缓存全部清空。

## 7. 安全边界

- Bot Secret 只在 Server 环境变量中；机器 Key 只在对应 Pal 配置中。
- Server 只能向 Key 所属用户与机器路由命令；Pal 仍执行本地 `PolicyGuard`。
- 日志不记录完整 Key、Secret、Cookie、prompt 或终端快照。
- 默认拒绝未知用户、未知 IM 会话、未绑定目标、失效 occupant 和 degraded Herdr。
- 不提供 `server.stop`、`pane.close`、任意 `pane.send_input`、通用 Herdr RPC 或自动审批。
- `skip_verify=true` 只适合受信任内网；正式公网部署必须验证证书并增加认证限流。

## 8. 测试策略

至少覆盖：

- HPRP 严格信封、重复 JSON 字段、版本、大小限制和错误解析。
- Bearer Key 签发、摘要存储、过期/吊销、HTTP 401 与重复机器 HTTP 409。
- hello、首快照、快照幂等/乱序、断线撤下和 heartbeat。
- 稳定目标、会话替换、命令关联、命令幂等、输出与通知去重。
- 多用户多机器目录隔离和企业微信路由。
- Herdr NDJSON、订阅、重连、prompt 门禁、按键策略和终端清理。

真实 Herdr 集成测试不得依赖企业微信网络；写入测试必须显式指定测试 pane。

## 9. 后续演进

HPRP Feature 应以用户功能命名，例如工作区准备或任务监控，由 Pal 展开为多个 Herdr 公共
API 操作。Feature 不得暴露底层 method 列表或要求对端依赖具体调用顺序。

后续优先项包括凭据吊销/轮换 CLI、证书 pinning 或内部 CA、可靠通知、可选 StateStore、
健康检查、第二个 IM Adapter 和公开 HPRP 一致性测试向量。
