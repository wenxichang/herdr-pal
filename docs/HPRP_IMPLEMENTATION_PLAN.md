# HPRP/1 Pal 与 Server 改造实施计划

> **状态：** HPRP/1 改造已完成。本文保留为历史实施记录；其中旧的 Server Key 签发子命令
> 已由 HPAP/1 和 `hp-cli` 替代，当前行为以 README、HPRP 与 HPAP 设计文档为准。

> 本计划在当前会话内按任务顺序执行，禁止使用 subagent-driven development。每个行为变更
> 都先添加失败测试，再编写最小实现，最后运行相关包测试和完整回归。

**目标：** 将 `herdr-pal` 与 `herdr-pal-server` 从内部 Relay Protocol 3 迁移到公开的
HPRP/1，使两端可以独立升级，并使用每台机器独立 Bearer Key 完成连接认证。

**架构：** 新建 `internal/hprp` 承担信封、消息、协商和稳定错误模型；新建
`internal/credential` 承担 Key 签发、摘要存储和 Upgrade 认证。Server 在 WebSocket
Upgrade 前把 Key 解析为 `(principal_id, machine_id)`，Pal 只持有 Bearer Key，不在 HPRP
消息中声明 userid 或 machine_id。现有目录、企业微信路由和本地 Bridge 保持业务职责，
通过 HPRP 的稳定 `machine_id + slot_id + session_id` 目标适配。

**技术栈：** Go、`github.com/coder/websocket`、JSON、WSS、SHA-256、现有 fake Herdr 与
本地 TLS 集成测试。

---

## 1. 实施范围和迁移原则

- HPRP/1 使用 WebSocket 子协议 `herdr-pal-relay.v1`，不与 Relay Protocol 3 共用连接。
- 当前版本直接迁移 Pal 和 Server，不保留同端口双协议分支。
- `principal_id` 使用企业微信回调中的 userid；`machine_id` 由机器 Key 在服务端绑定。
- Pal 本地 PolicyGuard 使用 Bearer Key 中的 `credential_id` 作为已认证审计身份，不依赖
  自声明 userid。
- `slot_id` 对应 Herdr pane ID，`session_id` 使用当前 occupant hash；展示序号只存在于
  `display.index`，不参与路由。
- `/N` 不再发送协议级选择请求。Server 先用在线目录验证并保存选择，随后 `/con` 或普通
  命令仍由 Pal 按完整稳定目标复核。
- 当前命令首段映射为 `command.result`，后续分段映射为 `command.output`，主动状态映射为
  `notification.event`。
- 首批连接协商 `command.output.v1`；实现 Feature 通用消息和协商框架，但不声明尚未有
  Feature Package 的用户 Feature。
- 旧的 `internal/relayproto` 在所有调用方迁移后删除。

## 2. 文件结构

新增文件：

- `internal/hprp/envelope.go`：HPRP 信封、严格编码、重复 JSON 字段检测。
- `internal/hprp/messages.go`：hello、快照、命令、通知和 Feature 公共消息。
- `internal/hprp/negotiate.go`：Capability/Feature family 版本选择及参数协商。
- `internal/hprp/validate.go`：名称、目标、快照、结果和资源限制校验。
- `internal/hprp/error.go`：稳定错误码、结果和协议错误。
- `internal/credential/key.go`：Bearer Key 生成、解析、摘要和常量时间验证。
- `internal/credential/store.go`：凭据文件加载、签发、吊销状态和原子保存。
- `internal/credential/http.go`：HTTP Authorization 解析和身份验证接口。
- `internal/server/command_routes.go`：已完成命令后续输出的有界路由表。

主要修改：

- `internal/config/client.go`：将 `userid/machine_id` 替换为 `key`。
- `internal/config/server.go`：增加 `credentials_file`，默认位于 `state_dir`。
- `cmd/herdr-pal-server/main.go`：增加 `key issue` 管理命令。
- `internal/server/catalog.go`：目录类型改用 HPRP target/session。
- `internal/server/connection.go`：使用 `id/reply_to` 请求关联和 HPRP 状态。
- `internal/server/hub.go`：Upgrade 认证、子协议、hello、快照确认、命令和通知。
- `internal/relayclient/client.go`：Bearer Upgrade、HPRP 协商、快照确认及命令处理。
- `internal/relayclient/snapshot.go`：生成 HPRP 会话快照。
- `internal/server/router.go`：移除远端 select 依赖并适配 HPRP 输出。
- `internal/app/relay.go`：以 credential ID 建立 PolicyGuard 和安全日志。
- `config.example.json`、`server-config.example.json`、`README.md`：替换部署流程。
- `docs/HPRP_PROTOCOL_DESIGN.md`：补齐 `command.output` 结构和已确认的实施勘误。

## 3. 分步任务

### 任务 1：HPRP 信封和严格 JSON 编解码

**测试：** `internal/hprp/envelope_test.go`

1. 添加失败测试，覆盖：
   - `protocol` 必须为 `HPRP/1`；
   - `id` 非空且 `reply_to` 只用于关联消息；
   - `payload` 必须是 object；
   - 未知信封字段可以忽略；
   - 同一 object 的重复字段必须拒绝；
   - 超过 1 MiB、binary frame 和尾随 JSON 必须拒绝。
2. 运行：

   ```sh
   go test ./internal/hprp -run 'TestEnvelope|TestDecode' -count=1
   ```

   预期因 `internal/hprp` 尚不存在而失败。
3. 实现以下核心接口：

   ```go
   type Envelope struct {
       Protocol       string          `json:"protocol"`
       Type           Type            `json:"type"`
       ID             string          `json:"id"`
       ReplyTo        string          `json:"reply_to,omitempty"`
       MustUnderstand bool            `json:"must_understand,omitempty"`
       Payload        json.RawMessage `json:"payload"`
   }

   func NewEnvelope(messageType Type, id, replyTo string, mustUnderstand bool, payload any) (Envelope, error)
   func Encode(Envelope) ([]byte, error)
   func Decode([]byte) (Envelope, error)
   func DecodePayload[T any](Envelope) (T, error)
   ```

4. 运行整个 `internal/hprp` 测试并提交：

   ```sh
   git commit -m "feat: 添加 HPRP 严格消息信封"
   ```

### 任务 2：HPRP 消息、目标、协商和错误模型

**测试：** `internal/hprp/messages_test.go`、`internal/hprp/negotiate_test.go`、
`internal/hprp/validate_test.go`

1. 先写失败测试，覆盖 hello family 最高共同版本、未知可选参数、稳定 target、快照序号、
   outcome 子集、Feature 名称和稳定错误码。
2. 定义：

   ```go
   const Subprotocol = "herdr-pal-relay.v1"

   type Target struct {
       MachineID string `json:"machine_id"`
       SlotID    string `json:"slot_id"`
       SessionID string `json:"session_id"`
   }

   type ClientHello struct { /* implementation/capabilities/features/limits/diagnostics */ }
   type ServerHello struct { /* connection/machine/capabilities/features/limits/heartbeat */ }
   type SessionSnapshot struct { Sequence uint64; Sessions []Session }
   type SnapshotResult struct { Outcome Outcome; AppliedSequence uint64; Error *Error }
   type CommandExecute struct { IdempotencyKey string; Target Target; Content TextContent }
   type CommandResult struct { Outcome Outcome; Content *TextContent; ReplacementTarget *Target; Error *Error }
   type CommandOutput struct { Target Target; Sequence uint64; Final bool; Content TextContent }
   type NotificationEvent struct { EventKey string; Sequence uint64; Kind string; Target Target; Content TextContent }
   ```

3. Feature 通用结构包含 `FeatureInvoke`、`FeatureResult`、`FeatureEvent`、`FeatureCancel` 和
   `FeatureCancelResult`，仅提供传输与 Schema 边界，不实现具体用户 Feature。
4. 运行包测试并提交：

   ```sh
   git commit -m "feat: 定义 HPRP 核心消息与能力协商"
   ```

### 任务 3：机器 Key 和凭据存储

**测试：** `internal/credential/key_test.go`、`internal/credential/store_test.go`、
`internal/credential/http_test.go`

1. 写失败测试验证：256 位随机 Secret、`hpk_<id>_<secret>` 格式、只保存 SHA-256 摘要、
   常量时间比较、无效/过期/吊销统一未认证、凭据文件权限和并发原子更新。
2. 实现：

   ```go
   type Identity struct {
       CredentialID string
       PrincipalID  string
       MachineID    string
   }

   type Verifier interface {
       VerifyBearer(context.Context, string) (Identity, error)
   }

   func Issue(principalID, machineID string, now time.Time, random io.Reader) (token string, record Record, err error)
   func LoadStore(path string) (*Store, error)
   func (s *Store) VerifyBearer(context.Context, string) (Identity, error)
   func (s *Store) Issue(principalID, machineID string) (string, Record, error)
   ```

3. 凭据文件只保存 `credential_id/principal_id/machine_id/secret_sha256/status/expires_at`。
4. 运行包测试并提交：

   ```sh
   git commit -m "feat: 添加 HPRP 机器凭据存储"
   ```

### 任务 4：配置和 Key 签发 CLI

**测试：** `internal/config/relay_test.go`、`internal/config/config_test.go`、
`cmd/herdr-pal-server/main_test.go`

1. 先写失败测试，要求客户端配置只接受非空 `relay.key`，服务端默认
   `credentials_file=<state_dir>/credentials.json`。
2. 客户端配置调整为：

   ```json
   {
     "relay": {
       "url": "wss://relay.example:9443",
       "key": "hpk_xxx_secret",
       "skip_verify": true
     }
   }
   ```

3. 当前管理面使用运行中 Server 的 HPAP 接口签发 Key：

   ```sh
   hp-cli -config /path/server-config.json key issue \
     --principal-id USERID --machine-id office-pc --source 192.168.1.20
   ```

   Server 把摘要记录写入凭据文件，只向 stdout 展示一次完整 Key；`hp-cli` 不要求企业微信
   Secret，也不直接修改凭据文件。
4. 配置错误必须指出具体字段，日志和错误不得回显完整 Key。
5. 运行相关测试并提交：

   ```sh
   git commit -m "feat: 支持配置和签发 HPRP 机器 Key"
   ```

### 任务 5：迁移目录领域模型

**测试：** `internal/server/catalog_test.go`、`internal/relayclient/snapshot_test.go`

1. 先把测试改为 HPRP `Target/SessionSnapshot`，确认编译失败。
2. 将目录内部稳定引用迁移为：

   ```go
   hprp.Target{
       MachineID: key.MachineID,
       SlotID: session.SlotID,
       SessionID: session.SessionID,
   }
   ```

3. `display.index` 仅用于列表展示；目录比较和自动会话替换只比较
   `machine_id/slot_id/session_id`。
4. 相同 snapshot sequence 返回原 applied 结果，较小 sequence 返回
   `sync.stale_snapshot`；断线仍立即撤下全部会话。
5. 运行目录和快照测试并提交：

   ```sh
   git commit -m "refactor: 将会话目录迁移到 HPRP 目标模型"
   ```

### 任务 6：改造 Server HPRP 连接状态机

**测试：** `internal/server/hub_test.go`、`internal/server/connection_test.go`

1. 写失败测试覆盖 HTTP `401/409`、子协议协商、首帧 `hello.client`、hello 交集、首次快照
   确认前不可执行、snapshot result、未知必需扩展错误和 WebSocket ping 超时。
2. `ClientHub` 注入 `credential.Verifier`：

   ```go
   func NewClientHub(catalog *SessionCatalog, verifier credential.Verifier, config HubConfig, logger *slog.Logger) (*ClientHub, error)
   ```

3. `ServeHTTP` 在 `websocket.Accept` 前完成 Bearer 校验和 ClientKey 预占；使用
   `websocket.AcceptOptions{Subprotocols: []string{hprp.Subprotocol}}`。
4. `clientConnection.request` 改用 envelope `id/reply_to` 关联。发送 hello 后接收完整快照，
   应用成功后发送 `session.snapshot.result`，再进入 READY。
5. `Select` 只验证在线目录和连接状态，不发送 HPRP 消息；`Execute` 发送
   `command.execute` 并等待 `command.result`。
6. 将 `command.output` 和 `notification.event` 投递给 Router；命令输出通过有界路由表按
   原调用 ID 找回 userid 和稳定 target，收到 `final` 或过期时清理。
7. 运行 Server 测试并提交：

   ```sh
   git commit -m "feat: 将 Server 连接中心迁移到 HPRP"
   ```

### 任务 7：改造 Pal HPRP 客户端

**测试：** `internal/relayclient/client_test.go`

1. 写失败测试覆盖 Authorization、子协议、hello、快照确认、重连重新同步、命令 target
   拒绝、replacement target、output sequence 和通知。
2. WebSocket Dial 使用：

   ```go
   &websocket.DialOptions{
       HTTPClient: client.httpClient(),
       HTTPHeader: http.Header{"Authorization": {"Bearer " + config.Key}},
       Subprotocols: []string{hprp.Subprotocol},
   }
   ```

3. hello 不再发送 userid/machine_id；从 `hello.server.machine_id` 建立当前连接身份，并验证
   它在连接期间不变。
4. 初始和后续快照都等待 `session.snapshot.result`；未确认的新 session 不允许被命令输出
   或通知引用。
5. `command.execute` 直接复核 target 并进入 Bridge；首段、后续段和通知分别转换成 HPRP
   result/output/event。Key 中的 credential ID 作为本地 `IncomingText.UserID`。
6. 运行客户端测试并提交：

   ```sh
   git commit -m "feat: 将 Pal Relay 客户端迁移到 HPRP"
   ```

### 任务 8：服务装配、Router 和集成测试

**测试：** `internal/server/router_test.go`、`internal/serverapp/app_test.go`、
`internal/app/app_test.go`、`internal/integration/relay_test.go`

1. 先修改测试，使服务端注入真实文件凭据 Store，客户端仅持有对应 Key。
2. `serverapp.Run` 加载凭据 Store 并注入 Hub；Pal 装配层从 Key 解析 credential ID 创建
   PolicyGuard。
3. Router 保持 `/ls`、`/N`、定向前缀、2 分钟通知降噪和会话替换行为；只替换
   HPRP 传输类型，不改变企业微信文案。
4. 端到端覆盖同一用户两台机器、不同用户同名机器、重复 Key 连接拒绝、快照确认、选择、
   prompt、后续 output、通知和断线撤下。
5. 运行：

   ```sh
   go test -race ./internal/integration -run TestRelayEndToEnd -count=3
   ```

6. 提交：

   ```sh
   git commit -m "test: 覆盖 HPRP 多用户多机器链路"
   ```

### 任务 9：移除旧协议并更新使用文档

1. 删除 `internal/relayproto`，用 `rg relayproto` 确认生产代码和测试均无引用。
2. 修订 `docs/HPRP_PROTOCOL_DESIGN.md`：补充 `command.output` payload、认证身份用于本地
   审计的说明，并修复重复标题。
3. 更新示例配置和 README：管理员使用可信账户信息中的 principal ID 签发机器 Key；Pal
   只配置 URL、Key、TLS 和 Herdr endpoint。
4. 更新 `docs/HANDOFF_CONTEXT.md` 和 `docs/BRIDGE_ARCHITECTURE.md`，明确 HPRP/1 与凭据文件。
5. 运行文档 JSON 示例解析和 `git diff --check`，提交：

   ```sh
   git commit -m "docs: 更新 HPRP 部署与迁移说明"
   ```

### 任务 10：完整验证

依次运行：

```sh
./unittest.sh
./build.sh
go test -race ./...
go vet ./...
git diff --check
```

验收条件：

- 所有命令退出码为 0；
- Pal 配置或 HPRP 帧中不再出现自声明 userid/machine_id；
- Server 未认证、重复机器和协议不相交分别产生明确 HTTP/协议错误；
- Server 重启后 Pal 可以重新认证、hello、同步快照并恢复 `/ls`；
- 当前企业微信交互、终端分页、通知降噪和会话自动替换行为不回退；
- 日志不包含完整 Bearer Key、prompt 或终端正文。
