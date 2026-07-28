# HPAP 本地管理面实施计划

> **状态：** 已完成。本文保留为实现过程和测试门禁记录，当前行为以
> `docs/HPAP_ADMIN_DESIGN.md` 与代码为准。

> **执行要求：** 使用 `superpowers:executing-plans` 按任务逐项实施；本项目禁止使用
> subagent-driven。所有功能先写失败测试，再实现最小代码；每次提交前必须执行
> `./unittest.sh && ./build.sh`。

**目标：** 将 HPRP 机器 Key 的签发和运行管理收归正在运行的 `herdr-pal-server`，新增通过
Unix Domain Socket 使用 HPAP/1 的 `hp-cli`，并提供 Key、连接、会话和 Server 运行状态的
安全动态管理能力。

**架构：** 新增独立的 `adminproto`、`adminclient` 和 `adminserver` 包。`AdminServer` 只依赖
Credential、Connection、Session 和 Runtime 四组窄接口；凭据文件仍由 `credential.Store`
独占持久化，HPRP Hub 和会话目录只暴露线程安全快照与明确的断连动作。HPAP 与 HPRP 完全
隔离，不通过管理面触发 Agent 操作或读取终端内容。

**技术栈：** Go 1.26、UTF-8 NDJSON、Unix Domain Socket、`golang.org/x/sys/unix`、标准库
`flag`/`encoding/json`/`net/netip`/`log/slog`，现有 HPRP、WeCom 和本地 fake 测试设施。

---

## 通用执行约定

- 每项任务中的“运行测试并确认失败”必须在写实现前完成；失败原因应是缺少当前任务能力，
  不能是编译环境或无关测试故障。
- 每项任务完成后先运行列出的聚焦测试，再运行完整门禁：

  ```bash
  ./unittest.sh && ./build.sh
  ```

- 只有完整门禁通过后才能执行该任务列出的提交命令。
- 凭据 Token、企业微信 Secret、终端输出和 prompt 不得进入测试日志、快照或错误文本。
- 所有新增对外类型和方法补充中文 Go 文档注释；变量、函数、类型和 JSON 字段使用英文命名。

## 任务 1：建立 HPAP/1 协议模型和严格 JSON 编解码

**文件：**

- 新建：`internal/adminproto/protocol.go`
- 新建：`internal/adminproto/methods.go`
- 新建：`internal/adminproto/error.go`
- 新建：`internal/adminproto/codec.go`
- 新建：`internal/adminproto/codec_test.go`
- 新建：`internal/adminproto/messages_test.go`

- [ ] 先为请求/响应互斥、未知协议、未知方法、重复字段、尾随 JSON、非法 UTF-8、超长帧和
  请求 ID 关联编写表驱动测试。
- [ ] 运行并确认测试因包或类型尚不存在而失败：

  ```bash
  go test ./internal/adminproto -run 'Test(Decode|Encode|Validate|Methods)' -count=1
  ```

- [ ] 定义首版稳定信封和错误模型：

  ```go
  const Protocol = "HPAP/1"
  const MaxFrameBytes = 1 << 20

  type Request struct {
      Protocol string          `json:"protocol"`
      ID       string          `json:"id"`
      Method   Method          `json:"method"`
      Params   json.RawMessage `json:"params,omitempty"`
  }

  type Response struct {
      Protocol string          `json:"protocol"`
      ID       string          `json:"id"`
      Result   json.RawMessage `json:"result,omitempty"`
      Error    *Error          `json:"error,omitempty"`
  }
  ```

- [ ] 固化设计文档中的方法名和错误码；错误 `code` 是程序判断依据，中文 `message` 只用于
  人工展示。
- [ ] 实现拒绝未知字段、重复字段和尾随内容的严格解码器。重复字段不能只依赖
  `json.Decoder.DisallowUnknownFields`，需要先扫描 object token 并维护字段集合。
- [ ] 实现有界按行读取：一帧超过 1 MiB 返回 `protocol.limit_exceeded`，协议级破坏允许调用方
  关闭连接。
- [ ] 运行聚焦测试并确认通过。
- [ ] 运行完整门禁后提交：

  ```bash
  git add internal/adminproto
  git commit -m "feat: 添加 HPAP 协议模型"
  ```

## 任务 2：实现来源地址规则纯模块

**文件：**

- 新建：`internal/credential/source.go`
- 新建：`internal/credential/source_test.go`

- [ ] 先覆盖 IPv4/IPv6 单地址、CIDR、闭区间、边界命中、IPv4-mapped IPv6、规范化、去重、
  非法 IP、跨地址族范围、反向范围和空列表。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/credential -run 'Test(Source|NormalizeSource|MatchSource)' -count=1
  ```

- [ ] 定义可持久化且可比较的规则：

  ```go
  type SourceRule string

  func ParseSourceRule(value string) (SourceRule, error)
  func NormalizeSourceRules(values []string) ([]SourceRule, error)
  func MatchSource(rules []SourceRule, source netip.Addr) bool
  ```

- [ ] 单地址使用 `netip.ParseAddr`，CIDR 使用 `netip.ParsePrefix(...).Masked()`，范围解析后要求
  同地址族且 start 不大于 end；所有 IPv4-mapped IPv6 在入库和匹配前调用 `Unmap()`。
- [ ] 规范化后按 kind/value 稳定排序并去重，不合并重叠范围；空规则返回
  `ErrSourceRequired`。
- [ ] `SourceRule` 的 JSON 表示保持为规范化字符串，使凭据文件中的 `allowed_sources` 与设计
  文档一致；匹配时再解析为 `netip.Addr`/`netip.Prefix`/闭区间，不把派生结构持久化。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/credential/source.go internal/credential/source_test.go
  git commit -m "feat: 添加凭据来源地址规则"
  ```

## 任务 3：重构凭据记录、递增 ID 和 Token 格式

**文件：**

- 修改：`internal/credential/key.go`
- 修改：`internal/credential/key_test.go`

- [ ] 先把测试改为确认：ID 是从 1 开始的 `uint64`、Token 为
  `hpk_<credential_id>_<secret>`、状态只有 enabled/disabled、来源非空、过期时间必须晚于
  签发时刻、`UpdatedAt` 存在、Secret 至少 256 位随机熵。
- [ ] 增加格式错误、ID 为 0、ID 溢出边界、摘要不匹配、disabled、expired、source mismatch
  统一认证失败的测试。
- [ ] 运行并确认旧随机字符串 ID 实现不符合测试：

  ```bash
  go test ./internal/credential -run 'Test(Issue|BearerCredentialID|VerifyRecord)' -count=1
  ```

- [ ] 将核心类型调整为：

  ```go
  type Record struct {
      CredentialID  uint64       `json:"credential_id"`
      PrincipalID   string       `json:"principal_id"`
      MachineID     string       `json:"machine_id"`
      SecretSHA256  string       `json:"secret_sha256"`
      Status        Status       `json:"status"`
      AllowedSources []SourceRule `json:"allowed_sources"`
      ExpiresAt     *time.Time   `json:"expires_at"`
      CreatedAt     time.Time    `json:"created_at"`
      UpdatedAt     time.Time    `json:"updated_at"`
  }
  ```

- [ ] `Issue` 接收已分配 ID、规范化来源和可选过期时间；完整 Token 只由该调用返回一次。
- [ ] `VerifyRecord` 同时校验 Token、状态、有效期和来源 IP，但对外统一返回
  `ErrUnauthenticated`；持久化记录结构损坏仍返回 `ErrInvalidRecord` 供启动诊断。
- [ ] 删除旧随机 credential ID 的 base32/正则逻辑和 active/revoked 状态。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/credential/key.go internal/credential/key_test.go
  git commit -m "feat: 改用递增凭据标识"
  ```

## 任务 4：实现 CredentialStore 动态 CRUD 和原子持久化

**文件：**

- 修改：`internal/credential/store.go`
- 修改：`internal/credential/store_test.go`

- [ ] 先覆盖启动时 `max(existing)+1`、空存储从 1 开始、运行期删除不回收、重启回收尾部、
  中间空洞不填补、`math.MaxUint64` 溢出拒绝、稳定排序和并发签发无重复 ID。
- [ ] 为 `List`、`Show`、`Enable`、`Disable`、`Delete`、`AddSources`、`RemoveSources`、
  `SetSources` 编写成功、not found、空来源和持久化失败回滚测试。
- [ ] 添加测试，确认凭据文件权限为 0600、目录为 0700、文件无明文 Token，并拒绝当前尚未
  发布的旧字符串 ID 记录。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/credential -run 'Test(Store|LoadStore)' -count=1
  ```

- [ ] 将 Store 索引改为 `map[uint64]Record`，增加只驻留内存的 `nextID uint64`。所有变更在
  Store 写锁内构造候选快照，先写临时文件、`fsync`、原子 rename，成功后才替换内存状态。
- [ ] 给查询结果做深拷贝，避免调用方修改 `AllowedSources`；列表按 credential ID 升序。
- [ ] `Disable`、`Delete` 和来源变更只返回持久化结果及记录快照，不在 Store 内直接依赖 Hub。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/credential/store.go internal/credential/store_test.go
  git commit -m "feat: 添加凭据动态管理存储"
  ```

## 任务 5：把真实 TCP 来源纳入 HPRP Upgrade 认证

**文件：**

- 修改：`internal/credential/http.go`
- 修改：`internal/credential/http_test.go`
- 修改：`internal/server/hub.go`
- 修改：`internal/server/hub_test.go`
- 修改：`internal/server/hub_hprp_test.go`

- [ ] 先测试从 `request.RemoteAddr` 解析 IPv4、IPv6 和 zone 后地址；伪造
  `X-Forwarded-For`/`Forwarded` 不影响认证；RemoteAddr 非法时统一 401。
- [ ] 调整 fake verifier 测试，确认 verifier 能收到规范化来源地址。
- [ ] 运行并确认接口签名不匹配：

  ```bash
  go test ./internal/credential ./internal/server -run 'Test(VerifyRequest|HPRPHub.*Auth)' -count=1
  ```

- [ ] 将认证接口改为：

  ```go
  type Verifier interface {
      VerifyBearer(context.Context, string, netip.Addr) (Identity, error)
  }
  ```

- [ ] `VerifyRequest` 仅从 `RemoteAddr` 提取真实 peer，Hub 的外部 HTTP 结果继续统一为 401；
  安全日志可记录 credential ID（若 Token 格式可解析）、来源 IP 和稳定失败类别，但不记录
  Secret。
- [ ] 将已认证来源地址传入连接对象，为后续连接查询和来源收紧复核保存。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/credential/http.go internal/credential/http_test.go internal/server/hub.go internal/server/hub_test.go internal/server/hub_hprp_test.go
  git commit -m "feat: 校验 HPRP 客户端来源地址"
  ```

## 任务 6：增加不改变用户路由状态的会话管理快照

**文件：**

- 修改：`internal/server/catalog.go`
- 修改：`internal/server/catalog_test.go`
- 新建：`internal/server/session_view.go`
- 新建：`internal/server/session_view_test.go`

- [ ] 先测试管理快照可按 principal/machine 过滤，沿用 `/ls` 的 machine、display index、slot、
  session 排序，包含当前用户内编号和稳定 HPRP target。
- [ ] 测试调用管理快照前后 `ResolveNumbered`、`Selected` 的状态完全不变，且不会创建用户的
  `/ls` 缓存。
- [ ] 覆盖 done/working/blocked/idle/unknown 的 emoji 展示；避免把 HPRP 状态强转成 Herdr
  私有类型，可抽取一个接受 string 的共享展示函数供 `/ls` 和管理面共用。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/server ./internal/panel -run 'Test(ManagementSessions|AgentStatusLabel)' -count=1
  ```

- [ ] 新增只读快照方法，例如：

  ```go
  type SessionView struct {
      PrincipalID string
      Number      int
      Target      hprp.Target
      Session     hprp.Session
  }

  func (catalog *SessionCatalog) ManagementSessions(filter SessionFilter) []SessionView
  ```

- [ ] 编号在每个 principal 内按当前完整列表从 1 开始计算；方法只持读锁并返回深拷贝。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/server/catalog.go internal/server/catalog_test.go internal/server/session_view.go internal/server/session_view_test.go internal/panel/status.go
  git commit -m "feat: 添加会话管理快照"
  ```

## 任务 7：增加 HPRP 连接元数据、查询和定向断连

**文件：**

- 修改：`internal/server/connection.go`
- 修改：`internal/server/connection_test.go`
- 修改：`internal/server/hub.go`
- 修改：`internal/server/hub_test.go`
- 修改：`internal/server/logging.go`

- [ ] 先测试连接快照包含 connection ID、credential ID、principal、machine、Pal 版本/OS/arch、
  source IP、connected_at、last_heartbeat_at、last_snapshot_at、snapshot sequence/session count 和
  排序后的 capabilities。
- [ ] 测试 `DisconnectConnection`、`DisconnectCredential`：先从可路由集合撤下，再取消连接，
  Catalog 会立即删除机器；不存在目标返回稳定 not found，不改变凭据状态。
- [ ] 测试来源规则收紧时按 credential ID 复核活动连接，只断开来源不再匹配的连接。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/server -run 'Test(ClientHub.*(Snapshot|Disconnect|Credential|Source)|Connection.*Metadata)' -count=1
  ```

- [ ] 在 `clientConnection` 中保存 hello implementation、协商能力、规范化 source、连接时间及
  原子更新时间；首次和后续 snapshot 成功时更新 sequence/time/count，心跳成功时更新时间。
- [ ] 给 Hub 增加线程安全方法：

  ```go
  func (hub *ClientHub) Connections() []ConnectionView
  func (hub *ClientHub) Connection(id string) (ConnectionView, bool)
  func (hub *ClientHub) DisconnectConnection(id, reason string) bool
  func (hub *ClientHub) DisconnectCredential(id uint64, reason string) int
  func (hub *ClientHub) RevalidateCredentialSource(id uint64, rules []credential.SourceRule) int
  ```

- [ ] 断连原因只使用固定安全文本；关闭 WebSocket 时不能在持有 Hub 锁期间等待 writer 退出。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/server/connection.go internal/server/connection_test.go internal/server/hub.go internal/server/hub_test.go internal/server/logging.go
  git commit -m "feat: 添加 HPRP 连接管理能力"
  ```

## 任务 8：增加企业微信和 TLS 的安全运行快照

**文件：**

- 修改：`internal/wecom/client.go`
- 修改：`internal/wecom/client_test.go`
- 修改：`internal/server/tls.go`
- 修改：`internal/server/tls_test.go`

- [ ] 先测试 WeCom 状态按 connecting/connected/reconnecting/stopped 迁移，保存最近变化时间、
  稳定错误类别和安全错误码，不保留 Secret 或底层敏感响应。
- [ ] 先测试 TLS 返回 external/automatic 模式、证书 NotAfter、SHA-256 指纹；不得暴露私钥路径
  或内容。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/wecom ./internal/server -run 'Test(ClientStatus|EnsureTLS.*Info|TLSInfo)' -count=1
  ```

- [ ] 在 WeCom Client 状态锁中更新：拨号前 connecting，首次失败后 reconnecting，订阅成功后
  connected，Run 退出时 stopped；暴露 `Status() StatusSnapshot` 深拷贝。
- [ ] 将 TLS 装配返回值扩展为包含 `*tls.Config` 和 `TLSInfo`，从 leaf certificate 计算到期时间
  和 DER SHA-256 指纹；现有调用和测试一起迁移。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/wecom/client.go internal/wecom/client_test.go internal/server/tls.go internal/server/tls_test.go
  git commit -m "feat: 添加服务依赖运行快照"
  ```

## 任务 9：实现动态日志级别和 Server RuntimeInspector

**文件：**

- 新建：`internal/serverapp/runtime.go`
- 新建：`internal/serverapp/runtime_test.go`
- 修改：`internal/serverapp/app.go`
- 修改：`internal/serverapp/app_test.go`

- [ ] 先测试基础日志级别保留、debug enable/disable 即时切换、重启语义恢复基础值；verbose 启动
  时 debug 打开，但仍能 disable 回基础值。
- [ ] 先测试 runtime status 包含版本、PID、启动时间、uptime、GOOS/GOARCH、HPAP/HPRP 版本、
  listener/admin socket、TLS、WeCom、debug、principal/connection/session 和凭据统计。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/serverapp -run 'Test(Runtime|DynamicLogger|ServerStatus)' -count=1
  ```

- [ ] 用 `slog.LevelVar` 替换固定 handler level，并封装：

  ```go
  type RuntimeLogger struct {
      Logger    *slog.Logger
      baseLevel slog.Level
      level     slog.LevelVar
  }
  ```

- [ ] `RuntimeInspector` 只聚合各组件线程安全快照，返回统一 `ObservedAt`；统计不得为了查询而读取
  终端或改变 Catalog 路由状态。
- [ ] Server stop 回调只触发根 context cancel，一次性且可审计；响应先写出由 AdminServer 保证。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/serverapp/runtime.go internal/serverapp/runtime_test.go internal/serverapp/app.go internal/serverapp/app_test.go
  git commit -m "feat: 添加服务运行状态管理"
  ```

## 任务 10：实现 Unix Admin Socket 安全监听

**文件：**

- 新建：`internal/adminserver/listener.go`
- 新建：`internal/adminserver/listener_unix.go`
- 新建：`internal/adminserver/peeruid_linux.go`
- 新建：`internal/adminserver/peeruid_darwin.go`
- 新建：`internal/adminserver/peeruid_other.go`
- 新建：`internal/adminserver/listener_test.go`
- 修改：`go.mod`
- 修改：`go.sum`

- [ ] 先测试 state directory 0700、socket 0600、拒绝符号链接/普通文件/非 Socket、删除确定无人
  监听的陈旧 Socket、拒绝已被监听的路径、关闭后清理 Socket。
- [ ] 通过可注入 peer UID 查询函数测试 UID 不一致立即拒绝；同 UID 才进入请求处理。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/adminserver -run 'Test(AdminListener|PeerUID|StaleSocket)' -count=1
  ```

- [ ] Linux 使用 `unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)`；macOS 使用
  `unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)`，并核验返回 UID 等于
  `os.Geteuid()`。通过 `SyscallConn.Control` 读取，不泄漏 fd。
- [ ] state dir 已存在时也必须 `lstat`、拒绝 symlink，并校验当前用户所有权和权限；Socket
  listen 后显式 chmod 0600。
- [ ] 连接探测只针对现有 Unix Socket：连接成功说明实例正在运行，拒绝覆盖；得到明确的
  connection refused/no such file 才清理。其他错误保持原路径并返回诊断。
- [ ] `golang.org/x/sys` 从 indirect 调整为直接依赖。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/adminserver go.mod go.sum
  git commit -m "feat: 添加安全本地管理监听"
  ```

## 任务 11：实现 AdminServer 请求循环、限流和审计

**文件：**

- 新建：`internal/adminserver/server.go`
- 新建：`internal/adminserver/server_test.go`
- 新建：`internal/adminserver/audit.go`
- 新建：`internal/adminserver/audit_test.go`

- [ ] 先测试一个连接顺序处理多个请求、多个连接并发、有界连接数、5 秒读写超时、超长帧关闭、
  业务错误不关闭、protocol error 关联原 request ID。
- [ ] 测试审计字段包含 peer UID、method、请求 ID 摘要、目标和耗时，不包含 Token、摘要、Bot
  Secret、prompt 或 terminal content。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/adminserver -run 'Test(AdminServer|Audit)' -count=1
  ```

- [ ] 定义窄接口和 Handler：

  ```go
  type CredentialManager interface { /* issue/list/show/status/source CRUD */ }
  type ConnectionManager interface { /* list/show/disconnect/revalidate */ }
  type SessionInspector interface { /* management session snapshot */ }
  type RuntimeInspector interface { /* status/debug/stop */ }
  ```

- [ ] Server 使用固定容量 semaphore 控制连接数；每条连接单 goroutine、串行请求，不再为每个
  request 启 goroutine。响应编码也受 1 MiB 上限约束。
- [ ] `server.stop` handler 先构造并写出成功响应，flush/关闭当前请求后再异步调用 stop 回调。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/adminserver/server.go internal/adminserver/server_test.go internal/adminserver/audit.go internal/adminserver/audit_test.go
  git commit -m "feat: 添加 HPAP 管理服务核心"
  ```

## 任务 12：实现 Key 管理方法和断连联动

**文件：**

- 新建：`internal/adminserver/key_handler.go`
- 新建：`internal/adminserver/key_handler_test.go`
- 修改：`internal/adminproto/methods.go`
- 修改：`internal/adminproto/messages_test.go`

- [ ] 先为 `key.issue/list/show/enable/disable/delete` 和
  `key.source.list/add/remove/set` 编写 handler 测试，覆盖分页、RFC3339 过期时间、必填 source、
  `--yes` 对应的确认字段、稳定错误码和只返回一次明文 Token。
- [ ] 测试 disable/delete 持久化成功后按 credential ID 断连；持久化失败不断连；source
  remove/set 只在当前来源不再允许时断连；source add 不断连。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/adminserver ./internal/adminproto -run 'Test(Key|Credential|Source).*Handler' -count=1
  ```

- [ ] 在 `adminproto` 中定义具体 params/result DTO，禁止 handler 传递 `map[string]any`；列表结果
  包含 `observed_at` 和可选 `next_page_token`。
- [ ] page token 使用服务端生成的不透明、带方法和排序锚点的编码；非法或跨方法使用返回
  `argument.invalid`。动态列表不承诺跨页快照。
- [ ] 所有错误映射到设计文档稳定 code；内部错误只向客户端返回安全消息，完整可诊断原因写
  Server 日志。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/adminserver/key_handler.go internal/adminserver/key_handler_test.go internal/adminproto
  git commit -m "feat: 添加 HPAP Key 管理方法"
  ```

## 任务 13：实现 Server、Connection 和 Session 管理方法

**文件：**

- 新建：`internal/adminserver/runtime_handler.go`
- 新建：`internal/adminserver/runtime_handler_test.go`
- 新建：`internal/adminserver/connection_handler.go`
- 新建：`internal/adminserver/connection_handler_test.go`
- 新建：`internal/adminserver/session_handler.go`
- 新建：`internal/adminserver/session_handler_test.go`
- 修改：`internal/adminproto/methods.go`
- 修改：`internal/adminproto/messages_test.go`

- [ ] 先测试 `server.status/stop/debug.enable/debug.disable`、
  `connection.list/show/disconnect` 和 `session.list` 的 params/result、分页、过滤和错误映射。
- [ ] 测试 session JSON 含完整 target，人工展示数据含 workspace/tab、agent/display agent、pane、
  title、状态 emoji；不调用任何输出读取接口且不修改 `/ls` 编号缓存。
- [ ] 测试 connection disconnect 不修改 Key 状态；Server status 不返回 secret、token、终端内容
  或未经清理的底层 WeCom 错误。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/adminserver ./internal/adminproto -run 'Test(Server|Connection|Session).*Handler' -count=1
  ```

- [ ] 实现 handlers 并统一为 `ObservedAt` 时间戳；所有列表稳定排序后再分页。
- [ ] `server.busy` 只用于明确的资源上限或 stop 已开始，不把普通 not found 混入该错误。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/adminserver internal/adminproto
  git commit -m "feat: 添加 HPAP 运行管理方法"
  ```

## 任务 14：实现 hp-cli 的 Unix 客户端和自动分页

**文件：**

- 新建：`internal/adminclient/client.go`
- 新建：`internal/adminclient/client_unix.go`
- 新建：`internal/adminclient/client_other.go`
- 新建：`internal/adminclient/client_test.go`
- 新建：`internal/adminclient/pagination.go`
- 新建：`internal/adminclient/pagination_test.go`

- [ ] 先测试连接/读/写超时、多个顺序请求、响应 ID 不匹配、协议版本错误、Server 业务错误与传输
  错误分类、超限响应和 socket 不可用。
- [ ] 测试 list 方法默认自动遍历 page token 并聚合，检测重复 token 防止无限循环。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/adminclient -count=1
  ```

- [ ] Client 每次 CLI 执行建立一条 Unix 连接，在同一连接内完成该命令的所有分页请求；默认 5 秒
  timeout，可由测试注入 dialer/clock，但首版不增加用户可配超时参数。
- [ ] 将 HPAP error 保存为带稳定 code 的类型，供 CLI 映射退出码 1；本地配置和 transport 分别
  由 CLI 映射退出码 2、3。
- [ ] 非 Unix 平台返回明确 unsupported 错误，避免影响现有 Windows Pal 构建。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/adminclient
  git commit -m "feat: 添加 HPAP 本地客户端"
  ```

## 任务 15：实现 hp-cli 命令解析和人工/JSON 输出

**文件：**

- 新建：`cmd/hp-cli/main.go`
- 新建：`cmd/hp-cli/main_test.go`
- 新建：`internal/adminclient/format.go`
- 新建：`internal/adminclient/format_test.go`

- [ ] 先为设计文档全部命令编写解析测试，覆盖重复 `--source`、逗号不作为 source 分隔、RFC3339
  校验、十进制 credential ID、`key delete --yes`、未知参数、默认 config path 和退出码 0/1/2/3。
- [ ] 测试 `--json` 输出单个合法 JSON document，包含聚合后的全部分页结果；人工表格的列顺序和
  空列表提示稳定，Token 只在 issue 成功结果打印。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./cmd/hp-cli ./internal/adminclient -run 'Test(Run|Format|Parse)' -count=1
  ```

- [ ] CLI 默认用 `config.DefaultServerPath()` 和 `config.LoadServerAdmin()` 推导
  `<state_dir>/admin.sock`；保留全局 `-config`，并支持 `--version`。
- [ ] 人工输出实现设计中的命令层级：server、key、connection、session。不要在每个 subcommand
  自行复制 transport 和错误处理逻辑。
- [ ] `session list` 状态字符串原样使用服务端已经统一的 emoji label，CLI 不重复推导状态。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add cmd/hp-cli internal/adminclient/format.go internal/adminclient/format_test.go
  git commit -m "feat: 添加 hp-cli 管理工具"
  ```

## 任务 16：接入 Server 生命周期并移除旧 key issue 子命令

**文件：**

- 修改：`internal/config/server.go`
- 修改：`internal/config/relay_test.go`
- 修改：`internal/serverapp/app.go`
- 修改：`internal/serverapp/app_test.go`
- 修改：`cmd/herdr-pal-server/main.go`
- 修改：`cmd/herdr-pal-server/main_test.go`

- [ ] 先测试 `LoadServerAdmin` 推导 `state_dir`，由共享 `AdminSocketPath(stateDir)` 生成
  `<state_dir>/admin.sock`；state dir 或派生 Socket 路径错误时输出具体原因，不新增多实例发现
  或任意 Socket 路径配置。
- [ ] 先测试 Server 启动时 Admin Socket 失败会整体失败，四个组件（WeCom、event loop、HPRP、
  Admin）任一异常会触发统一收尾；context/SIGTERM/HPAP stop 共用同一根取消路径。
- [ ] 先测试 `herdr-pal-server key issue` 被拒绝并提示改用 `hp-cli key issue`；删除
  `serverapp.Options.KeyIssue`、`KeyIssueOptions` 和 `issueMachineKey`。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/config ./internal/serverapp ./cmd/herdr-pal-server -run 'Test(LoadServerAdmin|RunServer|Run.*Key)' -count=1
  ```

- [ ] 在 Server 装配中构造 runtime、admin handler 和 admin listener，再启动并纳入统一组件结果
  通道；Admin Socket listen 完成后才记录“Server 启动”。
- [ ] 将固定容量的 `runServerComponents` 改成明确组件集合，等待首个退出后 cancel，并在 10 秒内
  回收其余组件；正常 stop 不打印伪异常。
- [ ] stop 时先禁止新 Admin/HPRP 连接，再关闭 HPRP HTTP、断开当前 Pal、停止 WeCom，最后清理
  Admin Socket；实际顺序通过 context 和各组件 Shutdown 保证。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add internal/config/server.go internal/config/relay_test.go internal/serverapp/app.go internal/serverapp/app_test.go cmd/herdr-pal-server/main.go cmd/herdr-pal-server/main_test.go
  git commit -m "feat: 接入 HPAP 服务生命周期"
  ```

## 任务 17：增加真实本地管理集成测试

**文件：**

- 新建：`internal/integration/admin_test.go`
- 修改：`internal/testkit/wecom_server.go`
- 修改：`internal/testkit/wecom_server_test.go`
- 视需要修改：`internal/testkit/herdr_server.go`

- [ ] 用临时 Unix Socket、fake WeCom 和本地 HPRP Pal 建立完整测试，覆盖：运行中 issue 后新 Pal
  可立即认证；错误来源收到 401；disable/delete/收紧来源立即撤下会话；enable 后允许重连。
- [ ] 覆盖 connection disconnect 不禁用 Key、session list 多用户多机器状态、并发 CLI 查询与串行
  Key 变更、Server stop 后 socket 清理。
- [ ] 明确断言 session list 不触发 Herdr `agent.read`，日志和 HPAP JSON 均不包含测试 Token。
- [ ] 运行并确认测试失败：

  ```bash
  go test ./internal/integration -run TestHPAP -count=1
  ```

- [ ] 只补充完成集成测试所需的 fake 观测点，不把生产管理逻辑放入 testkit。
- [ ] 运行集成测试至少三次排查时序不稳定：

  ```bash
  go test ./internal/integration -run TestHPAP -count=3
  ```

- [ ] 运行完整门禁后提交：

  ```bash
  git add internal/integration/admin_test.go internal/testkit
  git commit -m "test: 添加 HPAP 本地管理集成测试"
  ```

## 任务 18：接入构建产物和校验和

**文件：**

- 修改：`build.sh`
- 修改：`internal/buildscript/build_test.go`

- [ ] 先扩展构建脚本测试，要求当前平台生成 `dist/hp-cli`，发布矩阵生成：

  ```text
  dist/hp-cli-darwin-amd64
  dist/hp-cli-darwin-arm64
  dist/hp-cli-linux-amd64
  dist/hp-cli-linux-arm64
  ```

  Windows 只继续构建现有 `herdr-pal-windows-amd64.exe`，不生成 Server 或 hp-cli。
- [ ] 运行并确认测试因缺少 hp-cli 产物失败：

  ```bash
  go test ./internal/buildscript -run TestBuildScriptProducesReleaseArchitectureMatrix -count=1
  ```

- [ ] 给 `build.sh` 增加 native 和四个交叉编译 hp-cli，全部使用既有版本 ldflags，并把四个发布
  产物按稳定顺序加入 `SHA256SUMS`。
- [ ] 运行聚焦测试和完整门禁后提交：

  ```bash
  git add build.sh internal/buildscript/build_test.go
  git commit -m "build: 增加 hp-cli 构建产物"
  ```

## 任务 19：更新使用文档和最终安全审计

**文件：**

- 修改：`README.md`
- 修改：`docs/HPAP_ADMIN_DESIGN.md`
- 修改：`docs/HANDOFF_CONTEXT.md`
- 视实际实现修改：`docs/HPRP_PROTOCOL.md`

- [ ] 在 README 增加管理员最短路径：启动 Server、用 source 规则签发 Key、把一次性 Token 填入
  Pal、查询连接/会话、disable 与断连的区别；不加入 Server Secret 示例值。
- [ ] 将设计文档状态改为“已实现”，记录任何经过测试确认的实现细节偏差；HPAP 不属于 HPRP，
  不把管理方法写成远程协议扩展。
- [ ] 更新 HANDOFF，注明旧 `herdr-pal-server key issue` 已移除、凭据文件格式未向后兼容、
  credential ID 的重启尾部复用语义和 Windows 管理面边界。
- [ ] 执行敏感信息和占位内容扫描：

  ```bash
  rg -n 'TODO|TBD|FIXME|HERDR_PAL_WECOM_SECRET=.*[^<]|hpk_[A-Za-z0-9_-]+' README.md docs internal cmd --glob '!**/*_test.go'
  ```

- [ ] 审核管理查询路径不调用 `agent.read`，所有 Key 变更只经 Store，所有断连先撤路由再 cancel，
  `X-Forwarded-For` 不参与认证，日志没有 Token/SecretSHA256。
- [ ] 运行最终验证：

  ```bash
  ./unittest.sh
  ./build.sh
  git status --short
  ```

- [ ] 人工确认 `git status --short` 只包含本任务文档修改和预期构建忽略项，再提交：

  ```bash
  git add README.md docs
  git commit -m "docs: 添加 HPAP 管理工具使用说明"
  ```

## 最终验收清单

- [ ] `hp-cli key issue` 的新 Key 无需重启 Server 即可用于 HPRP 认证。
- [ ] credential ID 按运行期 max+1 分配，删除尾部后仅在 Server 重启时可能复用。
- [ ] disabled、expired、错误 Secret、未知 ID 和来源不符对 Pal 均只表现为 HTTP 401。
- [ ] disable/delete/来源收紧能立即撤下对应在线机器和全部会话；持久化失败不改变内存或连接。
- [ ] connection disconnect 不改变 Key，Pal 可以使用相同 Key 重连。
- [ ] session list 与 `/ls` 排序和状态展示一致，但不改变用户编号缓存或选择。
- [ ] HPAP Socket 权限、路径类型和 peer UID 校验在 Linux/macOS 测试覆盖范围内通过。
- [ ] server.stop 的成功响应先于根 context 取消，Server 退出后 `admin.sock` 被清理。
- [ ] Server、hp-cli、Pal 的查询和日志中没有 Token、Bot Secret、Secret SHA-256 或终端内容。
- [ ] `./unittest.sh && ./build.sh` 全部通过，发布矩阵包含四个 hp-cli Unix 产物。
