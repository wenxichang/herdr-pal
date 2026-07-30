# Herdr Pal Web 管理台实施计划

> **执行要求：** REQUIRED SUB-SKILL: 使用 `superpowers:executing-plans` 按任务逐项实施。项目明确禁止 subagent-driven development，因此全部任务由主 Agent 内联完成。每个步骤使用 `- [ ]` 跟踪。

**目标：** 在 `herdr-pal-server` 中内嵌 HTTPS Web 管理台，复用 hp-cli 的运行管理能力，并增加多管理员认证、外部自动化凭据 API 和 Loki 审计查询。

**架构：** 新建协议无关的 `internal/adminservice`，让 HPAP Handler 和 Web JSON Handler 调用同一套管理规则。浏览器认证由 `internal/adminauth` 管理，Web 传输、页面和中间件位于 `internal/webadmin`，Loki 查询位于 `internal/lokiquery`；`internal/serverapp` 只负责装配、监听和统一关闭。

**技术栈：** Go 1.26、标准库 `net/http`/`html/template`/`embed`、`golang.org/x/crypto/argon2`、现有 slog、Loki HTTP `query_range` API、原生 HTML/CSS/JavaScript。

---

## 文件结构

新增文件：

```text
internal/adminservice/
  errors.go              管理领域稳定错误
  model.go               协议无关的管理 DTO
  service.go             共享管理规则和延迟停止动作
  service_test.go

internal/adminauth/
  password.go            Argon2id PHC 编解码
  token.go               hpa_ 自动化 Token
  store.go               server-auth.json 原子持久化
  session.go             内存 Session 和 CSRF
  login_guard.go         登录失败锁定
  *_test.go

internal/lokiquery/
  client.go              受控 LogQL 和 Loki HTTP 调用
  model.go               查询条件、日志条目和游标
  client_test.go

internal/webadmin/
  server.go              路由注册和依赖装配
  middleware.go          TLS、认证、CSRF、安全头和日志
  json.go                严格 JSON 和统一响应
  pagination.go          Web 列表游标
  auth_handler.go        登录、退出、改密
  management_handler.go  Server、凭据、连接和会话 API
  admin_handler.go       管理员和 Token API
  automation_handler.go  外部 IT API 和限速
  audit_handler.go       Loki 审计 API
  assets.go
  assets/templates/*.html
  assets/static/app.css
  assets/static/app.js
  *_test.go

internal/integration/web_admin_test.go
```

重点修改：

```text
internal/config/server.go
internal/config/path.go
internal/adminproto/key.go
internal/adminproto/management.go
internal/adminserver/*.go
internal/serverapp/app.go
internal/serverapp/runtime.go
internal/credential/store.go
server-config.example.json
README.md
docs/BRIDGE_ARCHITECTURE.md
docs/HPAP_ADMIN_DESIGN.md
docs/AUDIT_SERVICE_DEPLOYMENT.md
go.mod
go.sum
```

## 通用提交规则

每个任务提交前都必须执行：

```sh
./unittest.sh
./build.sh
```

预期两个命令退出码均为 `0`。任何失败都先修复，不得带失败提交。

---

### Task 1：增加 Web 管理配置和固定认证文件路径

**Files:**

- Modify: `internal/config/server.go`
- Modify: `internal/config/path.go`
- Modify: `internal/config/relay_test.go`
- Modify: `server-config.example.json`

- [ ] **Step 1：编写配置失败测试**

在 `internal/config/relay_test.go` 增加：

```go
func TestLoadServerAppliesWebAdminDefaults(t *testing.T) {
	loaded, err := LoadServer(writeConfig(t, `{
  "wecom":{"bot_id":"bot"},
  "server":{"listen":"127.0.0.1:9443"},
  "log":{}
}`), func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Admin.Listen != "0.0.0.0:4001" {
		t.Fatalf("admin listen = %q", loaded.Admin.Listen)
	}
	if !strings.HasSuffix(loaded.Admin.AuthFile, filepath.Join(".config", "herdr-pal", "server-auth.json")) {
		t.Fatalf("auth file = %q", loaded.Admin.AuthFile)
	}
}

func TestLoadServerValidatesWebAdminLokiURL(t *testing.T) {
	for _, value := range []string{"ftp://127.0.0.1", "http://user@127.0.0.1", "http://127.0.0.1?a=1"} {
		raw := fmt.Sprintf(`{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443"},"admin":{"loki_url":%q},"log":{}}`, value)
		if _, err := LoadServer(writeConfig(t, raw), func(string) string { return "secret" }); err == nil {
			t.Fatalf("LoadServer accepted admin.loki_url %q", value)
		}
	}
}
```

- [ ] **Step 2：确认测试因字段不存在而失败**

Run:

```sh
go test ./internal/config -run 'TestLoadServer(AppliesWebAdminDefaults|ValidatesWebAdminLokiURL)' -count=1
```

Expected: FAIL，提示 `ServerConfig` 没有 `Admin` 字段或配置未知。

- [ ] **Step 3：实现配置模型和校验**

在 `internal/config/server.go` 增加：

```go
const defaultWebAdminListen = "0.0.0.0:4001"

type AdminConfig struct {
	Listen   string `json:"listen"`
	LokiURL  string `json:"loki_url"`
	AuthFile string `json:"-"`
}
```

把 `Admin AdminConfig` 同时加入 `ServerConfig` 和 `serverConfigFile`。`LoadServerAdmin` 中应用
默认监听，调用 `DefaultServerAuthPath`，并校验监听地址。`loki_url` 允许空值；非空时只允许
HTTP(S) 绝对 URL，拒绝 userinfo、query、fragment，path 只能为空或 `/`。

在 `internal/config/path.go` 增加：

```go
// DefaultServerAuthPath 返回 Web 管理员摘要文件的固定路径。
func DefaultServerAuthPath() (string, error) {
	return defaultPath("server-auth.json")
}
```

在示例配置增加：

```json
"admin": {
  "listen": "0.0.0.0:4001",
  "loki_url": "http://127.0.0.1:3100"
}
```

- [ ] **Step 4：运行配置测试**

Run: `go test ./internal/config -count=1`

Expected: PASS。

- [ ] **Step 5：完整验证并提交**

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/config/server.go internal/config/path.go internal/config/relay_test.go server-config.example.json
git commit -m "feat: 添加 Web 管理配置"
```

---

### Task 2：建立协议无关的共享 AdminService

**Files:**

- Create: `internal/adminservice/errors.go`
- Create: `internal/adminservice/model.go`
- Create: `internal/adminservice/service.go`
- Create: `internal/adminservice/service_test.go`
- Modify: `internal/credential/store.go`
- Modify: `internal/credential/store_test.go`

- [ ] **Step 1：覆盖重复机器和共享管理规则**

在 `credential` 测试中增加同一用户和机器重复签发冲突；在 `adminservice` 测试中增加：

```go
func TestServiceDisablesCredentialBeforeDisconnect(t *testing.T) {
	store, record := seededStore(t)
	connections := &fakeConnections{}
	service := newService(t, store, connections)
	result, err := service.SetCredentialEnabled(record.CredentialID, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Credential.Status != "disabled" || result.DisconnectedConnections != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestServicePrepareStopCommitsOnlyAfterResponse(t *testing.T) {
	runtime := &fakeRuntime{}
	service := newServiceWithRuntime(t, runtime)
	action, err := service.PrepareStop()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.stopCalls != 0 {
		t.Fatal("PrepareStop triggered stop before commit")
	}
	action.Commit()
	if runtime.stopCalls != 1 {
		t.Fatalf("stop calls = %d", runtime.stopCalls)
	}
}
```

- [ ] **Step 2：确认测试失败**

Run: `go test ./internal/credential ./internal/adminservice -count=1`

Expected: FAIL，`adminservice` 不存在，重复签发尚未冲突。

- [ ] **Step 3：定义共享模型和错误**

`errors.go` 定义：

```go
type ErrorCode string

const (
	CodeInvalidArgument    ErrorCode = "invalid_argument"
	CodeCredentialNotFound ErrorCode = "credential_not_found"
	CodeCredentialConflict ErrorCode = "credential_conflict"
	CodeSourceRequired     ErrorCode = "source_required"
	CodeSourceInvalid      ErrorCode = "source_invalid"
	CodeConnectionNotFound ErrorCode = "connection_not_found"
	CodeServerBusy         ErrorCode = "server_busy"
	CodeInternal           ErrorCode = "internal"
)

type Error struct {
	Code    ErrorCode
	Message string
	Cause   error
}
```

实现 `Error`、`Unwrap` 和 `ErrorCodeOf`。`model.go` 定义与现有 HPAP JSON 字段一致的
`Credential`、`Connection`、`Session`、`ServerStatus`、`TLSStatus`、`WeComStatus`、
`CredentialCounts`、`DebugStatus`、`CredentialIssueResult`、`CredentialMutationResult`、
`CredentialDeleteResult`。模型不得包含 Secret 摘要、明文 Token 或终端正文。

- [ ] **Step 4：实现 Service 公共接口**

```go
type Config struct {
	Credentials CredentialManager
	Connections ConnectionManager
	Sessions    SessionInspector
	Runtime     RuntimeController
	Now         func() time.Time
}

func New(config Config) (*Service, error)
func (service *Service) IssueCredential(input IssueCredentialInput) (CredentialIssueResult, error)
func (service *Service) ListCredentials() []Credential
func (service *Service) ShowCredential(id uint64) (Credential, error)
func (service *Service) SetCredentialEnabled(id uint64, enabled bool) (CredentialMutationResult, error)
func (service *Service) DeleteCredential(id uint64) (CredentialDeleteResult, error)
func (service *Service) ListSources(id uint64) ([]string, error)
func (service *Service) MutateSources(id uint64, operation SourceOperation, values []string) (CredentialMutationResult, error)
func (service *Service) ListConnections() []Connection
func (service *Service) ShowConnection(id string) (Connection, error)
func (service *Service) DisconnectConnection(id string) error
func (service *Service) ListSessions(filter SessionFilter) []Session
func (service *Service) Status() ServerStatus
func (service *Service) SetDebug(enabled bool) DebugStatus
func (service *Service) PrepareStop() (*StopAction, error)
```

`StopAction.Commit` 调用一次 `RuntimeController.RequestStop`；`Rollback` 只释放停止预留。第二个
并发 `PrepareStop` 返回 `CodeServerBusy`。

在 `credential.Store.Issue` 持锁期间扫描现有记录，同一 `PrincipalID` 和 `MachineID` 返回
`ErrCredentialConflict`，保证 Web 和 hp-cli 都不能绕过重复约束。

- [ ] **Step 5：运行领域测试**

Run: `go test ./internal/credential ./internal/adminservice -count=1`

Expected: PASS。

- [ ] **Step 6：完整验证并提交**

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/adminservice internal/credential/store.go internal/credential/store_test.go
git commit -m "refactor: 提取共享管理服务"
```

---

### Task 3：让 HPAP Handler 使用 AdminService

**Files:**

- Modify: `internal/adminproto/key.go`
- Modify: `internal/adminproto/management.go`
- Modify: `internal/adminserver/dependencies.go`
- Modify: `internal/adminserver/key_handler.go`
- Modify: `internal/adminserver/connection_handler.go`
- Modify: `internal/adminserver/session_handler.go`
- Modify: `internal/adminserver/runtime_handler.go`
- Modify: `internal/adminserver/*_test.go`
- Modify: `internal/serverapp/runtime.go`

- [ ] **Step 1：增加 HPAP 兼容测试**

保留全部现有 HPAP 测试，并增加停止写出失败回滚测试，以及重复
`principal_id + machine_id` 返回 `adminproto.CodeCredentialConflict` 的测试。

- [ ] **Step 2：运行重构前基线**

Run: `go test ./internal/adminproto ./internal/adminserver ./internal/serverapp -count=1`

Expected: PASS。

- [ ] **Step 3：把 HPAP DTO 指向共享模型**

```go
type Credential = adminservice.Credential
type Connection = adminservice.Connection
type Session = adminservice.Session
type ServerStatusResult = adminservice.ServerStatus
type ServerDebugResult = adminservice.DebugStatus
```

分页结果和 HPAP 参数继续保留在 `adminproto`，保证 HPAP/1 JSON 不变。

- [ ] **Step 4：把四类 Handler 改成薄适配器**

```go
func NewKeyHandler(service *adminservice.Service, logger *slog.Logger) (*KeyHandler, error)
func NewConnectionHandler(service *adminservice.Service) (*ConnectionHandler, error)
func NewSessionHandler(service *adminservice.Service) (*SessionHandler, error)
func NewRuntimeHandler(service *adminservice.Service) (*RuntimeHandler, error)
```

Handler 只负责解码 HPAP 参数、时间和分页 Token，调用 AdminService，映射领域错误并编码结果。
停止方法把 `StopAction.Commit` 放入 `AfterWrite`，把 `Rollback` 放入
`AfterWriteFailure`。`RuntimeInspector.Status` 改为返回 `adminservice.ServerStatus`。

- [ ] **Step 5：运行 HPAP 和 CLI 测试**

Run:

```sh
go test ./internal/adminproto ./internal/adminserver ./internal/adminclient ./cmd/hp-cli ./internal/serverapp -count=1
```

Expected: PASS，hp-cli JSON 和文本输出不变。

- [ ] **Step 6：完整验证并提交**

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/adminproto internal/adminserver internal/serverapp/runtime.go
git commit -m "refactor: 让 HPAP 复用管理服务"
```

---

### Task 4：实现管理员密码、自动化 Token 和原子 AuthStore

**Files:**

- Create: `internal/adminauth/password.go`
- Create: `internal/adminauth/password_test.go`
- Create: `internal/adminauth/token.go`
- Create: `internal/adminauth/token_test.go`
- Create: `internal/adminauth/store.go`
- Create: `internal/adminauth/store_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1：编写密码和 Token 测试**

```go
func TestArgon2idCodecHashesAndVerifiesPassword(t *testing.T) {
	codec := NewArgon2idCodec()
	hash, err := codec.Hash("correct horse battery", bytes.NewReader(bytes.Repeat([]byte{1}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("hash = %q", hash)
	}
	if ok, err := codec.Verify(hash, "correct horse battery"); err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
}

func TestAutomationTokenRoundTrip(t *testing.T) {
	token, record, err := GenerateAutomationToken(bytes.NewReader(bytes.Repeat([]byte{2}, 40)), time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "hpa_") || !VerifyAutomationToken(record, token) {
		t.Fatal("token did not verify")
	}
}
```

- [ ] **Step 2：编写 AuthStore 生命周期测试**

覆盖：文件不存在时创建 `admin`；权限 `0600`；文件只含摘要；Bootstrap 明文只返回一次；
重新加载不返回明文；创建、认证、改密、重置、删除、最后管理员保护；密码重置不轮换
Token；Token 轮换、启禁用、管理员删除后失效；损坏 JSON 和未知版本拒绝加载。

- [ ] **Step 3：确认测试失败并增加依赖**

Run: `go test ./internal/adminauth -count=1`

Expected: FAIL，包不存在。

实现文件导入 `golang.org/x/crypto/argon2` 后运行 `go mod tidy`。

- [ ] **Step 4：实现密码和 Token 原语**

```go
type Argon2idCodec struct{}

func NewArgon2idCodec() Argon2idCodec
func (Argon2idCodec) Hash(password string, random io.Reader) (string, error)
func (Argon2idCodec) Verify(encoded, password string) (bool, error)
```

密码长度限制为 12 至 256。PHC 参数固定为 `m=65536,t=3,p=2`，salt 16 字节、输出 32
字节；解析时限制参数上限，防止恶意认证文件触发资源耗尽。Token 格式固定为：

```text
hpa_<16 个小写十六进制 token_id>_<32 字节 base64url secret>
```

- [ ] **Step 5：实现 AuthStore**

```go
type Bootstrap struct {
	Created         bool
	Username        string
	InitialPassword string
	AutomationToken string
}

func Load(path string, options Options) (*Store, Bootstrap, error)
func (store *Store) Authenticate(username, password string) (Admin, error)
func (store *Store) Admin(username string) (Admin, error)
func (store *Store) ListAdmins() []Admin
func (store *Store) CreateAdmin(username string) (CreatedAdmin, error)
func (store *Store) ChangePassword(username, currentPassword, newPassword string) error
func (store *Store) ResetPassword(username string) (string, error)
func (store *Store) DeleteAdmin(requester, target string) error
func (store *Store) RotateAutomationToken(username string) (string, AutomationToken, error)
func (store *Store) SetAutomationTokenEnabled(username string, enabled bool) (AutomationToken, error)
func (store *Store) VerifyAutomationBearer(value string) (AutomationIdentity, error)
```

用户名转小写并匹配 `[a-z][a-z0-9._-]{2,31}`。所有变更持锁复制状态，成功完成临时文件
写入、`Sync`、`Close`、`Rename` 后才替换内存状态。随机初始密码使用 24 字节随机数的
base64url 编码；认证文件版本固定为 `1`、模式 `0600`，父目录模式 `0700`。创建管理员和
轮换 Token 时必须检测 `token_id` 冲突并重新生成，不能覆盖其他管理员记录。

- [ ] **Step 6：运行认证测试**

Run: `go test ./internal/adminauth -count=1`

Expected: PASS。

- [ ] **Step 7：完整验证并提交**

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/adminauth go.mod go.sum
git commit -m "feat: 添加管理员认证存储"
```

---

### Task 5：实现浏览器 Session、CSRF 和登录失败锁定

**Files:**

- Create: `internal/adminauth/session.go`
- Create: `internal/adminauth/session_test.go`
- Create: `internal/adminauth/login_guard.go`
- Create: `internal/adminauth/login_guard_test.go`

- [ ] **Step 1：用可控时间编写 Session 测试**

```go
func TestSessionManagerEnforcesIdleExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	manager := newTestSessionManager(&now)
	created, err := manager.Create("admin")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Minute)
	if _, ok := manager.Get(created.ID); !ok {
		t.Fatal("session expired before idle timeout")
	}
	now = now.Add(31 * time.Minute)
	if _, ok := manager.Get(created.ID); ok {
		t.Fatal("session survived idle timeout")
	}
}
```

另外覆盖：12 小时绝对过期；轮换 ID 和 CSRF；撤销指定用户除当前外的 Session；删除全部
Session；Cookie ID 和 CSRF 格式错误统一拒绝。

- [ ] **Step 2：编写 LoginGuard 测试**

同一用户名或来源 IP 第 5 次失败后锁定 15 分钟；成功登录清除两个桶；不同用户和 IP 互不
影响；时间推进后恢复。

- [ ] **Step 3：运行测试确认失败**

Run: `go test ./internal/adminauth -run 'Test(Session|LoginGuard)' -count=1`

Expected: FAIL，类型尚不存在。

- [ ] **Step 4：实现内存状态机**

`session.go` 的接口：

```go
func NewSessionManager(config SessionConfig) (*SessionManager, error)
func (manager *SessionManager) Create(username string) (SessionCredentials, error)
func (manager *SessionManager) Get(id string) (Session, bool)
func (manager *SessionManager) Rotate(id string) (SessionCredentials, error)
func (manager *SessionManager) Delete(id string)
func (manager *SessionManager) RevokeUser(username string)
func (manager *SessionManager) RevokeUserExcept(username, keepID string)
```

Session ID 与 CSRF 各使用 32 字节随机数。Map 使用 Session ID 的 SHA-256 作为 Key，CSRF
也只在内存保存摘要并常量时间比较。`SessionConfig` 缺省空闲超时 30 分钟、绝对有效期
12 小时；`Get` 在有效时更新最后活动时间。

`login_guard.go` 的接口：

```go
func NewLoginGuard(now func() time.Time) *LoginGuard
func (guard *LoginGuard) Allow(username string, source netip.Addr) bool
func (guard *LoginGuard) Failure(username string, source netip.Addr)
func (guard *LoginGuard) Success(username string, source netip.Addr)
```

- [ ] **Step 5：运行测试并提交**

Run: `go test ./internal/adminauth -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/adminauth/session.go internal/adminauth/session_test.go internal/adminauth/login_guard.go internal/adminauth/login_guard_test.go
git commit -m "feat: 添加管理台会话安全机制"
```

---

### Task 6：建立 HTTPS Web 核心、统一 JSON 和登录 API

**Files:**

- Create: `internal/webadmin/server.go`
- Create: `internal/webadmin/middleware.go`
- Create: `internal/webadmin/json.go`
- Create: `internal/webadmin/auth_handler.go`
- Create: `internal/webadmin/server_test.go`
- Create: `internal/webadmin/middleware_test.go`
- Create: `internal/webadmin/json_test.go`
- Create: `internal/webadmin/auth_handler_test.go`

- [ ] **Step 1：编写安全中间件和严格 JSON 测试**

覆盖：非 TLS 请求拒绝；CSP、`nosniff`、Referrer Policy；不返回 CORS 允许头；来源地址只
使用 TCP `RemoteAddr` 而忽略 `X-Forwarded-For`；API 响应含 `request_id`；未知字段、重复
字段、第二个 JSON 和超过 64 KiB 的 body 返回 `400`；panic 返回脱敏 `500`；日志不含请求
body。

严格 JSON 用例：

```go
request := httptest.NewRequest(http.MethodPost, "/admin/api/v1/auth/login",
	strings.NewReader(`{"username":"admin","username":"root","password":"secret-value"}`))
request.TLS = &tls.ConnectionState{}
response := httptest.NewRecorder()
handler.ServeHTTP(response, request)
if response.Code != http.StatusBadRequest || strings.Contains(logs.String(), "secret-value") {
	t.Fatalf("status=%d logs=%q", response.Code, logs.String())
}
```

- [ ] **Step 2：编写登录流程测试**

覆盖：成功登录设置 Secure/HttpOnly/SameSite Cookie；失败 5 次锁定；`GET auth/session`；退出；
首次改密前其他 API 返回 `403`；改密后轮换 Cookie 与 CSRF；Origin 不同源或缺失时写请求
返回 `403`。

- [ ] **Step 3：运行测试确认失败**

Run: `go test ./internal/webadmin -count=1`

Expected: FAIL，包不存在。

- [ ] **Step 4：实现 Web Server 和中间件**

公共构造接口：

```go
type Config struct {
	Admin      *adminservice.Service
	Auth       *adminauth.Store
	Sessions   *adminauth.SessionManager
	LoginGuard *adminauth.LoginGuard
	Logger     *slog.Logger
	Random     io.Reader
	Now        func() time.Time
}

func New(config Config) (*Server, error)
func (server *Server) Handler() http.Handler
```

中间件顺序固定为：恢复 panic → request ID → HTTPS 检查 → 安全头 → HTTP 管理日志 →
ServeMux。API Session 验证、首次改密和 CSRF 是路由级包装；自动化 API 不进入浏览器
Session/CSRF 中间件。

HTTP 管理日志固定记录 `request_id`、actor、真实来源地址、method、规范化 route、目标摘要、
outcome、稳定 error code 和 duration；不记录完整 URL query、Cookie 或请求/响应 body。
request ID 使用 16 字节随机数编码为 32 位小写十六进制。

`json.go` 使用 Token 扫描器先递归检查对象内重复字段，再使用 `DisallowUnknownFields` 解码
目标结构，并要求 EOF。成功和错误信封严格按设计文档输出；响应先完整编码到内存缓冲区，
再设置 Header 并执行一次 `Write`，便于延迟停止动作判断写出结果。

- [ ] **Step 5：实现登录 API**

注册：

```text
POST /admin/api/v1/auth/login
GET  /admin/api/v1/auth/session
POST /admin/api/v1/auth/logout
POST /admin/api/v1/auth/password
```

Cookie 名固定为 `herdr_pal_admin_session`，Path 为 `/admin`，设置 `Secure`、`HttpOnly`、
`SameSite=Strict` 且不写持久化 Max-Age。登录响应只返回用户名、`must_change_password` 和
CSRF Token；密码和 Cookie 不进入 JSON。写请求使用 `X-CSRF-Token`。改密成功后调用
`SessionManager.Rotate`，撤销该用户其他 Session，并清除 `must_change_password`。

- [ ] **Step 6：运行 Web 核心测试**

Run: `go test ./internal/webadmin -count=1`

Expected: PASS。

- [ ] **Step 7：完整验证并提交**

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/webadmin
git commit -m "feat: 添加 Web 管理台登录接口"
```

---

### Task 7：实现 Server、凭据、连接和会话 JSON API

**Files:**

- Create: `internal/webadmin/pagination.go`
- Create: `internal/webadmin/management_handler.go`
- Create: `internal/webadmin/pagination_test.go`
- Create: `internal/webadmin/management_handler_test.go`
- Modify: `internal/webadmin/server.go`

- [ ] **Step 1：编写管理 API 测试表**

按路由覆盖成功、无效参数、未找到、冲突、确认缺失和 CSRF：

```text
GET    /admin/api/v1/overview
GET    /admin/api/v1/credentials
POST   /admin/api/v1/credentials
GET    /admin/api/v1/credentials/{id}
POST   /admin/api/v1/credentials/{id}/enable
POST   /admin/api/v1/credentials/{id}/disable
DELETE /admin/api/v1/credentials/{id}
GET    /admin/api/v1/credentials/{id}/sources
POST   /admin/api/v1/credentials/{id}/sources
PUT    /admin/api/v1/credentials/{id}/sources
DELETE /admin/api/v1/credentials/{id}/sources
GET    /admin/api/v1/connections
GET    /admin/api/v1/connections/{id}
POST   /admin/api/v1/connections/{id}/disconnect
GET    /admin/api/v1/sessions
GET    /admin/api/v1/server/status
POST   /admin/api/v1/server/debug
POST   /admin/api/v1/server/stop
```

停止测试必须证明 JSON 成功写出后才触发 `StopAction.Commit`；模拟失败 Writer 时调用
`Rollback`。

- [ ] **Step 2：运行测试确认路由不存在**

Run: `go test ./internal/webadmin -run 'TestManagement|TestPagination' -count=1`

Expected: FAIL 或路由返回 `404`。

- [ ] **Step 3：实现 Web 分页和领域错误映射**

Web 分页使用版本化 base64url JSON 游标，默认 100、最大 500。凭据锚点是 credential ID；
连接锚点为 `principal_id\x00machine_id\x00connection_id`；会话锚点与 hp-cli 的稳定排序键
一致。游标必须携带资源名，不能跨接口复用。

```go
var statusByCode = map[adminservice.ErrorCode]int{
	adminservice.CodeInvalidArgument:    http.StatusBadRequest,
	adminservice.CodeCredentialNotFound: http.StatusNotFound,
	adminservice.CodeCredentialConflict: http.StatusConflict,
	adminservice.CodeSourceRequired:     http.StatusBadRequest,
	adminservice.CodeSourceInvalid:      http.StatusBadRequest,
	adminservice.CodeConnectionNotFound: http.StatusNotFound,
	adminservice.CodeServerBusy:         http.StatusConflict,
	adminservice.CodeInternal:           http.StatusInternalServerError,
}
```

- [ ] **Step 4：实现管理 Handler**

签发请求的 `expires_at` 只接受 RFC3339。删除凭据、断开连接、停止服务必须解码
`{"confirm":true}`。来源 `POST` 表示 add、`PUT` 表示 set、`DELETE` 表示 remove。Session
查询仅接受 `userid` 精确过滤和 `machine_id` 精确过滤，列表内容来自 AdminService，不读取
终端。

HTTP 管理日志目标只写 credential ID、connection ID、machine ID 或 userid 短摘要，不写
完整签发 Token。

- [ ] **Step 5：运行测试并提交**

Run: `go test ./internal/webadmin ./internal/adminservice ./internal/adminserver -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/webadmin
git commit -m "feat: 添加 Web 运行管理接口"
```

---

### Task 8：实现管理员管理和外部自动化 API

**Files:**

- Create: `internal/webadmin/admin_handler.go`
- Create: `internal/webadmin/admin_handler_test.go`
- Create: `internal/webadmin/automation_handler.go`
- Create: `internal/webadmin/automation_handler_test.go`
- Modify: `internal/webadmin/server.go`

- [ ] **Step 1：编写管理员接口测试**

注册并测试：

```text
GET    /admin/api/v1/administrators
POST   /admin/api/v1/administrators
POST   /admin/api/v1/administrators/{username}/reset-password
DELETE /admin/api/v1/administrators/{username}
POST   /admin/api/v1/administrators/{username}/token/rotate
POST   /admin/api/v1/administrators/{username}/token/enable
POST   /admin/api/v1/administrators/{username}/token/disable
```

创建和重置的随机密码、创建和轮换的 Token 只出现在一次成功响应；后续列表只有 Token ID、
启用状态和时间。测试当前管理员不可删除、最后管理员不可删除、重置和删除撤销目标 Session。

- [ ] **Step 2：编写自动化 API 测试**

```text
POST   /admin/api/v1/automation/credentials
DELETE /admin/api/v1/automation/credentials/{credential_id}
```

覆盖无 Token、错误 Token、禁用 Token、轮换后的旧 Token、重复签发 `409`、来源为空、删除
任意凭据并断开连接，以及每秒第 6 次和一分钟第 101 次返回 `429`。

- [ ] **Step 3：运行测试确认失败**

Run: `go test ./internal/webadmin -run 'TestAdmin|TestAutomation' -count=1`

Expected: FAIL 或 `404`。

- [ ] **Step 4：实现管理员 Handler**

管理员 Handler 从 Session Actor 取得当前用户名。删除、重置密码和 Token 轮换使用
`{"confirm":true}`；启禁用 Token 使用各自固定路由。操作成功后调用 SessionManager 撤销
规则，不把明文加入审计上下文或 slog 字段。

- [ ] **Step 5：实现自动化认证和限速**

`Authorization` 只接受一个 `Bearer hpa_...`。认证成功后 Actor 记录为管理员用户名和
`token_id`。限速器按 Token ID 使用内存时间队列，固定每秒 5、滚动一分钟 100；Server
重启清空。自动化路由只注册签发和删除，不能复用浏览器路由绕过能力限制。

- [ ] **Step 6：运行测试并提交**

Run: `go test ./internal/webadmin ./internal/adminauth ./internal/adminservice -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/webadmin
git commit -m "feat: 添加管理员和自动化凭据接口"
```

---

### Task 9：实现 Loki 受控查询和审计 API

**Files:**

- Create: `internal/lokiquery/model.go`
- Create: `internal/lokiquery/client.go`
- Create: `internal/lokiquery/client_test.go`
- Create: `internal/webadmin/audit_handler.go`
- Create: `internal/webadmin/audit_handler_test.go`
- Modify: `internal/webadmin/server.go`

- [ ] **Step 1：编写 LogQL 构造测试**

```go
func TestBuildLogQLUsesOnlyEscapedControlledFilters(t *testing.T) {
	query, err := buildLogQL(Query{
		PrincipalID: `u"} |= "leak`,
		MachineID:   `HOME.*`,
		Keyword:     `Error[0-9]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{service_name="herdr-pal-server"}`,
		`herdr_pal_audit_principal_id="u\"} |= \"leak"`,
		`herdr_pal_audit_machine_id=~"(?i).*HOME\\.\\*.*"`,
		`|~ "(?i)Error\\[0-9\\]"`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query = %q, want %q", query, want)
		}
	}
}
```

- [ ] **Step 2：编写 Fake Loki HTTP 测试**

验证请求路径 `/loki/api/v1/query_range`，参数 `direction=backward`、纳秒 start/end、limit；
解析两元素和带第三个 structured metadata 对象的 `values`；结果按时间倒序；非 2xx、
`status!=success`、非 streams、超过 16 MiB、超时均返回稳定 `ErrQuery`，错误不包含 Loki
body。

- [ ] **Step 3：实现 Loki Client**

`model.go`：

```go
type Query struct {
	PrincipalID string
	MachineID   string
	Keyword     string
	Start       time.Time
	End         time.Time
	Limit       int
	Cursor      string
}

type Entry struct {
	Timestamp     time.Time `json:"timestamp"`
	EventName     string    `json:"event_name"`
	PrincipalID   string    `json:"userid"`
	MachineID     string    `json:"machine_id,omitempty"`
	PaneID        string    `json:"pane_id,omitempty"`
	SessionIDHash string    `json:"session_id_hash,omitempty"`
	Action        string    `json:"action,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	Body          string    `json:"body"`
}

type Page struct {
	Items      []Entry `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}
```

`New` 校验 Loki base URL；`Query` 默认最近 24 小时、最大 31 天、默认 100、最大 500、10 秒
请求超时。LogQL 使用 `strconv.Quote` 和 `regexp.QuoteMeta`，绝不接受调用方提供的 LogQL。
游标编码下一页 `end` 纳秒值，取本页最旧时间减 1ns。

官方 Loki `query_range` 的 streams 响应允许每行第三个结构化元数据对象，解析器兼容两种
长度；字段优先从第三个对象读取，再回退到 stream labels。实现时以
`https://grafana.com/docs/loki/latest/reference/loki-http-api/` 的当前响应格式为准。

- [ ] **Step 4：实现审计 Handler**

给 `webadmin.Config` 增加：

```go
type AuditQuerier interface {
	Query(context.Context, lokiquery.Query) (lokiquery.Page, error)
}

Audit AuditQuerier
```

注册 `GET /admin/api/v1/audit/logs`。查询参数：`userid`、`machine_id`、`keyword`、`start`、
`end`、`limit`、`cursor`。start/end 只接受 RFC3339 或 RFC3339Nano；空值使用最近 24 小时。
userid 精确，machine 和 keyword 不区分大小写包含。Loki 未配置或不可用返回 `502`；管理
日志只记录 keyword 是否存在和长度。

- [ ] **Step 5：运行测试并提交**

Run: `go test ./internal/lokiquery ./internal/webadmin -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/lokiquery internal/webadmin/audit_handler.go internal/webadmin/audit_handler_test.go internal/webadmin/server.go
git commit -m "feat: 添加 Loki 审计查询接口"
```

---

### Task 10：内嵌经典侧栏管理页面

**Files:**

- Create: `internal/webadmin/assets.go`
- Create: `internal/webadmin/assets/templates/layout.html`
- Create: `internal/webadmin/assets/templates/login.html`
- Create: `internal/webadmin/assets/templates/overview.html`
- Create: `internal/webadmin/assets/templates/credentials.html`
- Create: `internal/webadmin/assets/templates/connections.html`
- Create: `internal/webadmin/assets/templates/sessions.html`
- Create: `internal/webadmin/assets/templates/audit.html`
- Create: `internal/webadmin/assets/templates/administrators.html`
- Create: `internal/webadmin/assets/templates/system.html`
- Create: `internal/webadmin/assets/static/app.css`
- Create: `internal/webadmin/assets/static/app.js`
- Create: `internal/webadmin/assets_test.go`
- Modify: `internal/webadmin/server.go`

- [ ] **Step 1：编写页面和资源测试**

验证：未登录页面重定向 `/admin/login`；已登录可以访问全部页面；首次改密用户只能看到改密
界面；模板包含固定侧栏七个入口；页面没有内联 `<script>`、外部 CDN 或 localStorage；静态
资源有正确 Content-Type、`nosniff` 和缓存策略；审计正文不写入浏览器存储。

- [ ] **Step 2：运行测试确认资源不存在**

Run: `go test ./internal/webadmin -run 'TestAssets|TestPages' -count=1`

Expected: FAIL 或页面 `404`。

- [ ] **Step 3：实现 embed.FS 和模板**

`assets.go`：

```go
//go:embed assets/templates/*.html assets/static/*
var embeddedAssets embed.FS

func loadTemplates() (*template.Template, error) {
	return template.New("layout.html").Funcs(template.FuncMap{
		"statusClass": statusClass,
	}).ParseFS(embeddedAssets, "assets/templates/*.html")
}
```

页面使用选择方案 A：深色固定侧栏、顶部 Server 状态和管理员菜单、主体列表/详情。窄屏侧栏
折叠。危险操作只在详情区域显示红色按钮，并由 JavaScript 弹出包含目标名称的确认框。

- [ ] **Step 4：实现原生 JavaScript 客户端**

`app.js` 只维护内存状态：启动时请求 `auth/session` 获取 CSRF；统一 `api()` 设置
`X-CSRF-Token`；按 `data-page` 加载当前列表；列表使用游标“下一页”；签发、创建管理员、
重置密码和 Token 轮换使用一次性结果对话框，关闭后从 DOM 清除明文。审计详情展开时只写
当前 DOM，切页或关闭立即清空，不使用 localStorage、sessionStorage 或 IndexedDB。

- [ ] **Step 5：运行页面测试并提交**

Run: `go test ./internal/webadmin -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/webadmin
git commit -m "feat: 内嵌 Web 管理页面"
```

---

### Task 11：装配 Web 管理台到 herdr-pal-server 生命周期

**Files:**

- Modify: `internal/serverapp/app.go`
- Modify: `internal/serverapp/runtime.go`
- Modify: `internal/serverapp/app_test.go`
- Modify: `internal/serverapp/runtime_test.go`
- Modify: `cmd/herdr-pal-server/main.go`
- Modify: `cmd/herdr-pal-server/main_test.go`

- [ ] **Step 1：编写启动和关闭测试**

覆盖：Web 监听失败导致完整启动失败并释放 Relay/HPAP；Web 使用与 Relay 相同证书指纹；
AuthStore 损坏错误显示具体路径但不泄露内容；初始 admin 密码和 Token 各打印一次；Server
状态含 Web 管理监听地址；任一组件失败时 Web、Relay、HPAP 和 HPRP 连接都关闭；Web stop
被视为正常停止。

`waitingServerComponents` 增加 `web_admin`，所有测试期待 5 个组件。

- [ ] **Step 2：先运行 serverapp 测试确认失败**

Run: `go test ./internal/serverapp ./cmd/herdr-pal-server -count=1`

Expected: FAIL，尚未装配 Web listener。

- [ ] **Step 3：调整装配顺序并创建共享 Service**

`Run` 中按以下顺序装配：

```text
加载配置并取得现有进程锁
  → 加载 AuthStore
  → 创建包含 Bootstrap 明文的日志脱敏器
  → 向 stdout 打印一次 Bootstrap 密码和 Token
  → 创建 TLS、WeCom、CredentialStore、Catalog、Hub、RuntimeInspector
  → 创建 AdminService
  → 用 AdminService 创建 HPAP handlers
  → 创建 SessionManager、LoginGuard、LokiQueryClient、WebAdmin Handler
  → 绑定 Relay、HPAP 和 Web Admin 三个 Listener
  → 启动五个组件
```

`serverapp.Options` 增加 `Stdout io.Writer`，CLI 将现有 stdout 传入；AuthStore 返回的初始
密码和 Token 加入日志脱敏 secret 集合后才创建 logger，一次性凭据使用
`fmt.Fprintf(stdout, ...)` 明确打印，普通运行日志继续只写 stderr。

- [ ] **Step 4：创建 HTTPS 管理 Server**

```go
webHTTPServer := &http.Server{
	Handler:           webAdmin.Handler(),
	TLSConfig:         tlsBundle.Config.Clone(),
	ReadHeaderTimeout: 5 * time.Second,
	ReadTimeout:       15 * time.Second,
	WriteTimeout:      30 * time.Second,
	IdleTimeout:       60 * time.Second,
}
```

监听 `loaded.Admin.Listen`，外层使用 `tls.NewListener`。组件名固定 `web_admin`。Shutdown 时
先关闭 HPAP listener 和 Web HTTP Server，再 `hub.BeginShutdown`、关闭 Relay、撤下连接并
等待 HPRP Handler；统一 10 秒总关闭窗口，Web 单独最多等待 5 秒。

`RuntimeConfig` 增加 `WebAdminListen`，`ServerStatus` JSON 增加
`web_admin_listen`，HPAP 只新增向后兼容字段。

- [ ] **Step 5：避免测试端口和认证文件冲突**

所有调用 `serverapp.Run` 的测试配置显式使用：

```json
"admin":{"listen":"127.0.0.1:0","loki_url":""}
```

在 `serverapp.Options` 增加仅供测试注入的 `AuthFile` 字段；CLI 不暴露该入口。生产运行仍
固定使用 `~/.config/herdr-pal/server-auth.json`。

- [ ] **Step 6：运行生命周期测试并提交**

Run: `go test ./internal/serverapp ./cmd/herdr-pal-server -count=1`

Run: `./unittest.sh && ./build.sh`

Commit:

```sh
git add internal/serverapp cmd/herdr-pal-server/main.go cmd/herdr-pal-server/main_test.go
git commit -m "feat: 启动内嵌 Web 管理服务"
```

---

### Task 12：完成本机集成测试和使用文档

**Files:**

- Create: `internal/integration/web_admin_test.go`
- Modify: `README.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`
- Modify: `docs/HPAP_ADMIN_DESIGN.md`
- Modify: `docs/AUDIT_SERVICE_DEPLOYMENT.md`
- Modify: `server-config.example.json`

- [ ] **Step 1：编写 TLS 集成测试**

使用临时证书、临时认证文件、真实 `credential.Store`、Fake Runtime/Hub 和 Fake Loki，完成：

```text
启动 HTTPS Handler
  → 使用初始 admin 登录
  → 首次改密
  → Web 签发机器 Key
  → HPAP Handler 能看到同一凭据
  → 创建第二管理员并验证一次性 Token
  → 自动化 API 签发另一机器 Key
  → Fake Loki 返回用户输入与终端输出
  → 按 userid、machine_id、日期和关键字查询
  → 自动化 API 删除凭据并验证连接撤下
```

测试分别捕获 stdout 和 stderr：stdout 只允许出现一次 Bootstrap 初始密码和 Token；stderr
普通日志不得包含初始密码、自动化 Token、机器 Key、Cookie、CSRF、查询关键字和 Loki
审计正文。

- [ ] **Step 2：更新 README 配置和操作步骤**

README 增加：

- `admin.listen`、`admin.loki_url` 示例。
- 首次启动控制台密码和 Token 的保存提醒。
- 访问 `https://SERVER:4001/admin/`，自签名证书告警说明。
- 登录后先改密、签发机器 Key、查看连接/会话、查询日志。
- 自动化 API 的两条 curl 示例，强调 Bearer Token 和一次性机器 Key。
- `server-auth.json` 只存摘要、权限 `0600`，损坏时 Server 拒绝启动。

- [ ] **Step 3：更新架构和运维文档**

`BRIDGE_ARCHITECTURE.md` 把现有“AdminServer 不提供远程管理端口”修订为 HPAP 本地入口与
HTTPS Web 入口共同复用 AdminService；继续强调 Web 不发送 Agent 输入。

`HPAP_ADMIN_DESIGN.md` 说明 HPAP/1 保持兼容，业务规则移入 AdminService，Web 不通过 Unix
Socket 自调用。

`AUDIT_SERVICE_DEPLOYMENT.md` 增加 `admin.listen` 和 `admin.loki_url`，说明 Loki 不可用只
影响管理台查询，不影响 OTLP 写入和企业微信交互。

- [ ] **Step 4：运行最终验证**

Run:

```sh
gofmt -w internal/adminservice/*.go internal/adminauth/*.go internal/lokiquery/*.go internal/webadmin/*.go internal/serverapp/*.go internal/integration/web_admin_test.go
./unittest.sh
./build.sh
git diff --check
```

Expected: 全部退出码为 `0`；`dist/herdr-pal-server*` 均包含内嵌页面，不依赖外部静态目录。

- [ ] **Step 5：人工烟雾检查**

使用临时配置启动 Server 后验证：

```sh
curl -k -I https://127.0.0.1:4001/admin/login
curl -k https://127.0.0.1:4001/admin/assets/app.css
```

Expected: 登录页返回 `200`，CSS 返回 `200` 和 `text/css`，HTTP 明文访问失败。

- [ ] **Step 6：提交最终集成和文档**

```sh
git add internal/integration/web_admin_test.go README.md docs/BRIDGE_ARCHITECTURE.md docs/HPAP_ADMIN_DESIGN.md docs/AUDIT_SERVICE_DEPLOYMENT.md server-config.example.json
git commit -m "docs: 完善 Web 管理台部署与验证"
```

---

## 最终验收清单

- [ ] `hp-cli` 全部既有命令和 HPAP/1 JSON 兼容。
- [ ] Server 默认在 `0.0.0.0:4001` 提供 HTTPS 管理台。
- [ ] 管理台和 HPRP 复用同一证书，明文 HTTP 不可用。
- [ ] 初始管理员、首次改密、多管理员和 Session 撤销符合设计。
- [ ] 密码、自动化 Token 和机器 Key 都只持久化摘要，明文只显示一次。
- [ ] Web 覆盖 Server、凭据、来源、连接、会话和管理员管理。
- [ ] 自动化 Token 只能签发、删除凭据，且支持限速和立即吊销。
- [ ] 审计查询支持 userid 精确、machine_id 包含、日期范围和关键字忽略大小写。
- [ ] Loki 故障不影响企业微信、HPRP、HPAP 和其他 Web 页面。
- [ ] HTTP 管理日志可定位请求且不进入 Loki，不泄露敏感正文。
- [ ] `./unittest.sh`、`./build.sh` 和 `git diff --check` 全部通过。
