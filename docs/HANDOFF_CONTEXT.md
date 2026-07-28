# Herdr Pal 交接上下文

## 1. 当前状态

Herdr Pal 是 Herdr 与企业微信之间的独立 sidecar bridge：

- `herdr-pal-server` 独占企业微信智能机器人长连接，并监听 HPRP/1 WSS。
- 每台运行 Herdr 的机器启动一个 `herdr-pal`，只访问本机 Herdr 公共 Socket。
- Server 通过每机独立 Bearer Key 认证 Pal；Key 在服务端绑定企业微信用户 ID 和机器标识。
- Pal 上报本机完整 Agent 会话快照，Server 为同一用户聚合多台机器并负责企业微信路由。
- `herdr-pal -i` 保留不经过网络的本机控制台模式。

网络模式默认配置：

- Server：`~/.config/herdr-pal/server-config.json`
- Pal：`~/.config/herdr-pal/config.json`
- 凭据存储：默认 `<state_dir>/credentials.json`

`build.sh` 使用 `CGO_ENABLED=0` 生成 Darwin/Linux AMD64、ARM64 客户端和服务端，以及
Windows AMD64 客户端 Beta。`unittest.sh` 运行全部单元和本地集成测试。

## 2. 固定产品决策

- 企业微信 Bot ID 和 Secret 只由 Server 持有；Secret 来自
  `HERDR_PAL_WECOM_SECRET`。
- 企业微信应用可见范围是用户入口边界；Router 只处理单聊。
- `/userid` 只用于向管理员提供企业微信 principal ID，不再写入 Pal 配置。
- 管理员使用 `herdr-pal-server key issue` 为每台机器签发独立 `hpk_...` Key。
- Key 在服务端绑定 `(principal_id, machine_id)`；Pal 不能自行声明或覆盖身份。
- 同一用户、同一机器标识只允许一条在线连接；后来连接返回 HTTP 409。
- 不同用户可以使用相同机器标识。连接断开后立即移除该机器和全部会话。
- Pal 与 Server 使用公开 HPRP/1；不兼容的主版本通过 WebSocket 子协议隔离。
- 网络只允许 WSS。`skip_verify` 默认开启仅用于受信任内网的自签名证书部署。
- Herdr 只接受精确 protocol 17，只使用公共 Local Socket API、CLI 和公开 Schema。
- 不持久化选择、在线目录、分页、通知或在途命令，不提供离线任务和通知回放。
- 终端内容只描述为近期快照，不承诺完整对话或结构化 LLM transcript。
- 不自动批准权限请求；按键必须由用户显式触发并通过本地策略。

## 3. 部署与身份流程

### 3.1 Server

1. `config.LoadServer` 读取 Bot ID、监听地址、`addr_hint`、TLS 和凭据文件路径。
2. `EnsureTLS` 加载外部证书；未配置时在状态目录生成并复用自签名证书。
3. `credential.LoadStore` 加载仅保存摘要的 HPRP 机器凭据。
4. 创建 `SessionCatalog`、`ClientHub`、`UserExecutor` 和 `ConversationRouter`。
5. 启动企业微信连接与 TLS HTTP/WebSocket 监听。
6. `/help` 使用 `addr_hint + listen 端口` 生成 Pal 的 WSS 配置示例。

用户在企业微信发送 `/userid` 后，管理员执行：

```sh
herdr-pal-server key issue \
  -principal-id '企业微信用户 ID' \
  -machine-id 'office-pc'
```

明文 Key 只在签发时输出一次。服务端保存 `credential_id`、绑定身份和 Secret SHA-256
摘要，不保存可直接使用的明文 Key。

### 3.2 Pal

1. `config.LoadClient` 严格校验 `wss://` 和 `relay.key`，并从 Key 派生安全日志使用的
   `credential_id`。
2. 解析本机 Herdr Socket，并按 Socket endpoint 获取单实例进程锁。
3. 启动 `EventSupervisor`，以 Herdr 权威 `session.snapshot` 接管本机会话。
4. 使用 `Authorization: Bearer ...` 和子协议 `herdr-pal-relay.v1` 建立 WSS。
5. 完成 `hello.client/hello.server`，从服务端确认 Key 所绑定的 `machine_id`。
6. 上报并等待确认首个完整 `session.snapshot`，之后连接才进入 READY。
7. Registry 变化后最多约 250ms 上报快照；同时按校准周期强制上报完整快照。
8. 断线时不缓存 prompt、按键或通知，指数退避并从 hello、完整快照重新开始。

## 4. HPRP/1 数据流

### 4.1 会话目录

HPRP 稳定目标为：

```text
machine_id + slot_id + session_id
```

- `machine_id` 来自服务端凭据绑定。
- `slot_id` 对应 Herdr pane。
- `session_id` 对应 pane 当前 Agent occupant。
- `/ls` 数字只是用户展示快照，实际路由始终保存完整稳定目标。

连接关闭、快照删除 pane 或同一 pane 更换 Agent 后，旧目标立即失效。

### 4.2 企业微信输入

- `/userid`、`/ls`、独立 `/N`/`/sel N` 和 `/help` 由 Server 处理。
- `/N 内容` 与 `#N 内容` 先由 Server 把全局编号解析为稳定目标；前者仅在成功后切换，
  后者不改变当前选择。
- 其余内容要求已有稳定选择，Server 发送 `command.execute`。
- Pal 再次校验 Key 绑定机器、pane 和 occupant，之后才进入本地 Bridge Service。
- Pal 返回唯一 `command.result`；协商 `command.output.v1` 后再发送有序后续输出。
- 命令导致同一 pane 创建新 Agent 会话时，Pal 先上报并确认新快照，再在结果中返回
  `replacement_target`。
- Server 超时不自动生成新命令重试；相同 IM `msgid` 作为 `idempotency_key`。

Pal 在 10 分钟有界窗口内缓存已完成命令结果。相同 Key 与等价命令会重放结果而不重复
产生本地副作用；同一 Key 对应不同目标或内容时返回 `command.idempotency_conflict`。

### 4.3 输出和通知

- `command.output` 必须引用原 `command.execute`，携带稳定目标和从 1 开始的连续序号。
- `notification.event` 使用 `event_key + sequence + target` 去重和排序。
- Server 只接受连接绑定机器且仍存在于最新快照的目标，跨机器上报会断开连接。
- 本地 Notifier 对 `blocked`、`done` 和需要关注的 `idle` 只读取最近 100 行。
- 同一用户最近 2 分钟有过任一方向交互时，其他会话通知降级为简短提醒。
- 断线期间的输出和通知直接失败，不排队等待下次连接。

## 5. Herdr API 关键事实

每个健康周期：

1. `ping`，要求 protocol 精确等于 17。
2. discovery `session.snapshot`。
3. 建立 pane lifecycle 订阅。
4. 为当前 Agent pane 建立 `pane.agent_status_changed` 订阅。
5. 再读取权威 snapshot，消除订阅建立窗口。
6. pane/occupant 变化时重建订阅直到稳定。
7. 每 10 秒重新读取权威 snapshot，弥补生命周期事件缺失或延迟。
8. 用基线替换 Registry，开放输入和 HPRP 上报。

不要依赖：

- `PaneReadResult.revision`：当前固定为 `0`。
- `pane.output_changed`：只有 Schema 类型，没有可用公共输出流。
- 外部订阅 cursor 或 exactly-once 重放：当前不存在。

## 6. 代码边界

- `internal/hprp`：HPRP/1 信封、公开消息、校验、协商和错误码。
- `internal/credential`：机器 Key 生成、摘要存储和 HTTP Bearer 验证。
- `internal/server`：在线目录、HPRP Hub、用户执行器和企业微信路由。
- `internal/relayclient`：Pal 的 HPRP 连接、快照、命令幂等和输出上报。
- `internal/herdr`：公共 NDJSON 请求、订阅和平台传输。
- `internal/session`：本机会话索引、选择和 occupant 身份。
- `internal/bridge`：Service、Notifier 和 EventSupervisor。
- `internal/command`、`internal/panel`、`internal/policy`：命令、终端和安全策略。
- `internal/wecom`、`internal/im`：企业微信协议与平台无关消息模型。

旧 `internal/relayproto` 已删除。不要重新引入客户端自声明 userid/machine_id 或协议 3
兼容分支。

## 7. 验证

提交前必须执行：

```sh
./unittest.sh
./build.sh
```

高风险网络改动额外执行：

```sh
go test -race ./internal/hprp ./internal/credential ./internal/server \
  ./internal/relayclient ./internal/integration
```

覆盖重点包括严格 JSON、Bearer 认证、hello/首快照、稳定目标、快照乱序、命令关联和
幂等、输出/通知去重、多用户多机器隔离、Herdr 重连恢复与本地输入策略。

## 8. 安全与后续工作

- Herdr Socket 永远只在本机访问，不通过 HPRP 原样代理。
- 日志不记录完整 Key、Secret、Cookie、prompt 或终端快照；用户、消息和 session 使用摘要。
- 服务器发来的内容仍必须经过 Pal 的 `PolicyGuard`，不构成自动授权。
- 当前 Key 是逻辑机器绑定，不提供硬件证明；复制 Key 仍可能在其他设备使用。
- 当前适合受信任内网。互联网部署应补充证书信任、凭据吊销命令、认证限流和审计存储。
- 后续 Feature 必须按用户功能建模，由 Pal 展开为多个 Herdr 公共 API 操作，不提供通用
  Herdr RPC 透传。
