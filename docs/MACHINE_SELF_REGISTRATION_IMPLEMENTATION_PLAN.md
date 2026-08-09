# 企业微信机器自主注册实施计划

> **执行要求：** 必须使用 `superpowers:executing-plans` 按任务顺序实施。项目 `AGENTS.md` 明确禁止 subagent-driven，因此不要使用 `superpowers:subagent-driven-development`。每个任务使用下方复选框跟踪。

**目标：** 在 `herdr-pal-server` 中增加 `/reg <machine_id> <source1,source2,...>` 自主注册、首台自动签发、后续机器 Web 审批和 Loki 审计能力，同时保持 Pal 与 HPRP 不变。

**架构：** 新增 `internal/machinereg`，由版本化待审批 Store 和共享协调 Service 组成。Router 只负责企微信令与当前请求回复，AdminService/WebAdmin 只负责管理员审批入口，所有签发路径通过同一协调器串行化，原始 CredentialStore 仍作为 HPRP Bearer 验证器。

**技术栈：** Go 1.26、标准库 JSON/文件原子替换、现有 CredentialStore、`net/http` Web 管理台、企业微信长连接客户端、OTLP/HTTP protobuf Logs、Go `testing` 与 race detector。

---

## 文件结构

新增文件：

- `internal/machinereg/model.go`：待审批记录、注册结果、交付载荷和稳定错误。
- `internal/machinereg/store.go`：只保存 pending 项目的版本化 JSON Store。
- `internal/machinereg/store_test.go`：持久化、权限、随机 ID、冲突和并发测试。
- `internal/machinereg/service.go`：首台判断、审批、回滚、凭据管理包装和并发协调。
- `internal/machinereg/service_test.go`：注册、审批、拒绝、回滚和人工签发并发测试。
- `internal/machinereg/audit.go`：机器注册审计事件构造和脱敏。
- `internal/webadmin/registration_handler.go`：注册审批 JSON API。
- `internal/webadmin/registration_handler_test.go`：认证、CSRF、分页、批准和拒绝测试。
- `internal/webadmin/assets/templates/registrations.html`：当前待审批列表页面。

修改文件：

- `internal/audit/event.go`、`internal/audit/event_test.go`、`internal/audit/otlp.go`、`internal/audit/otlp_test.go`：增加机器注册事件。
- `internal/credential/key.go`、`internal/credential/key_test.go`：导出 principal ID 校验供注册 Store 复用。
- `internal/server/router.go`、`internal/server/router_test.go`：解析和执行 `/reg`，放行无会话注册。
- `internal/adminservice/model.go`、`internal/adminservice/errors.go`、`internal/adminservice/service.go`、`internal/adminservice/service_test.go`：增加待审批管理领域接口。
- `internal/webadmin/management_handler.go`：注册审批路由与错误码映射。
- `internal/webadmin/assets.go`、`internal/webadmin/assets/templates/layout.html`、`internal/webadmin/assets/static/app.js`、`internal/webadmin/assets_test.go`：增加审批页面和前端交互。
- `internal/webadmin/server_test.go`：测试装配共享注册服务。
- `internal/serverapp/app.go`、`internal/serverapp/app_test.go`：装配 Store、协调器和企业微信交付回调。
- `internal/integration/admin_test.go`、`internal/integration/relay_test.go`：覆盖真实本地 Server 组合链路。
- `internal/server/default_help.md`、`internal/server/help_test.go`、`README.md`：增加自主注册使用说明。
- `docs/HANDOFF_CONTEXT.md`、`docs/BRIDGE_ARCHITECTURE.md`：同步身份与审批架构。

## Task 1：扩展审计事件模型

**Files:**

- Modify: `internal/audit/event.go`
- Modify: `internal/audit/event_test.go`
- Modify: `internal/audit/otlp.go`
- Modify: `internal/audit/otlp_test.go`

- [ ] **Step 1：先写机器注册审计事件失败测试**

在 `internal/audit/event_test.go` 增加：

```go
func TestPrepareEventAcceptsMachineRegistration(t *testing.T) {
	event, err := PrepareEvent(Event{
		EventName: EventNameMachineRegistration,
		PrincipalID: "user-a", MachineID: "office-laptop",
		Action: "request", Outcome: "pending",
		Body: "机器注册申请 office-laptop",
	}, time.Unix(30, 0), bytes.NewReader(bytes.Repeat([]byte{0xcd}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if event.EventName != EventNameMachineRegistration || event.SchemaVersion != 1 || event.EventID == "" {
		t.Fatalf("event = %#v", event)
	}
}
```

在 `internal/audit/otlp_test.go` 增加：

```go
func TestMachineRegistrationRollbackFailureUsesWarningSeverity(t *testing.T) {
	record := eventLogRecord(Event{EventName: EventNameMachineRegistration, Outcome: "rollback_failed"})
	if record.SeverityNumber != logsv1.SeverityNumber_SEVERITY_NUMBER_WARN {
		t.Fatalf("severity = %v", record.SeverityNumber)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```sh
go test ./internal/audit -run 'TestPrepareEventAcceptsMachineRegistration|TestMachineRegistrationRollbackFailureUsesWarningSeverity' -count=1
```

Expected: FAIL，提示 `EventNameMachineRegistration` 未定义或事件名无效。

- [ ] **Step 3：实现最小审计扩展**

在 `internal/audit/event.go` 增加：

```go
// EventNameMachineRegistration 表示机器自主注册和管理员审批生命周期。
const EventNameMachineRegistration = "herdr_pal.machine_registration"

func validEventName(value string) bool {
	switch value {
	case EventNameUserInput, EventNameTerminalOutput, EventNameMachineRegistration:
		return true
	default:
		return false
	}
}
```

`PrepareEvent` 改用 `validEventName`。`internal/audit/otlp.go` 的告警判断改为：

```go
if event.Outcome == "rate_limited" || event.Outcome == "delivery_failed" || event.Outcome == "rollback_failed" {
	severity = logsv1.SeverityNumber_SEVERITY_NUMBER_WARN
	severityText = "WARN"
}
```

- [ ] **Step 4：运行审计测试**

```sh
go test ./internal/audit -count=1
```

Expected: PASS。

- [ ] **Step 5：提交审计扩展**

```sh
git add internal/audit/event.go internal/audit/event_test.go internal/audit/otlp.go internal/audit/otlp_test.go
git commit -m "feat: 增加机器注册审计事件"
```

## Task 2：实现待审批 RegistrationStore

**Files:**

- Modify: `internal/credential/key.go`
- Modify: `internal/credential/key_test.go`
- Create: `internal/machinereg/model.go`
- Create: `internal/machinereg/store.go`
- Create: `internal/machinereg/store_test.go`

- [ ] **Step 1：为公开 principal ID 校验写失败测试**

```go
func TestValidatePrincipalID(t *testing.T) {
	for _, value := range []string{"user-a", "企业微信用户"} {
		if err := ValidatePrincipalID(value); err != nil {
			t.Fatalf("ValidatePrincipalID(%q) = %v", value, err)
		}
	}
	for _, value := range []string{"", " user-a", "user\n"} {
		if err := ValidatePrincipalID(value); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("ValidatePrincipalID(%q) = %v", value, err)
		}
	}
}
```

- [ ] **Step 2：为 Store 写失败测试**

创建 `internal/machinereg/store_test.go`，核心用例：

```go
func TestStoreCreatesPersistsAndDeletesPendingRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registrations.json")
	store, err := LoadStore(path, StoreOptions{
		Now: func() time.Time { return time.Unix(100, 0).UTC() },
		Random: bytes.NewReader(bytes.Repeat([]byte{0xab}, registrationIDRandomBytes)),
	})
	if err != nil { t.Fatal(err) }
	request, created, err := store.Create("user-a", "office-laptop", []credential.SourceRule{"192.168.0.1"})
	if err != nil || !created || !strings.HasPrefix(request.RegistrationID, "reg_") {
		t.Fatalf("request=%#v created=%t err=%v", request, created, err)
	}
	duplicate, created, err := store.Create("user-a", "office-laptop", []credential.SourceRule{"10.0.0.1"})
	if err != nil || created || duplicate.RegistrationID != request.RegistrationID || duplicate.AllowedSources[0] != "192.168.0.1" {
		t.Fatalf("duplicate=%#v created=%t err=%v", duplicate, created, err)
	}
	reloaded, err := LoadStore(path, StoreOptions{})
	if err != nil || len(reloaded.List()) != 1 || !reloaded.HasPrincipal("user-a") {
		t.Fatalf("list=%#v err=%v", reloaded.List(), err)
	}
	if _, err := reloaded.Delete(request.RegistrationID); err != nil || len(reloaded.List()) != 0 {
		t.Fatalf("delete err=%v list=%#v", err, reloaded.List())
	}
}
```

同一文件增加：权限过宽拒绝、未知字段/错误版本拒绝、非法 principal/machine/source、随机源耗尽、稳定排序、并发 Create 只生成一个同用户同机器申请。

- [ ] **Step 3：运行测试确认失败**

```sh
go test ./internal/credential ./internal/machinereg -count=1
```

Expected: FAIL，提示 `ValidatePrincipalID`、`LoadStore` 和相关类型未定义。

- [ ] **Step 4：实现 principal 校验与模型**

`internal/credential/key.go` 增加：

```go
// ValidatePrincipalID 校验企业微信 principal ID 是否可安全持久化和用于身份绑定。
func ValidatePrincipalID(value string) error {
	if !validPrincipalID(value) {
		return ErrInvalidRecord
	}
	return nil
}
```

`Issue` 改为调用 `ValidatePrincipalID`。创建 `internal/machinereg/model.go`：

```go
package machinereg

import (
	"errors"
	"time"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

var (
	ErrInvalidRequest  = errors.New("机器注册申请无效")
	ErrRequestNotFound = errors.New("机器注册申请不存在")
	ErrMachineExists   = errors.New("用户机器凭据已存在")
	ErrDeliveryFailed  = errors.New("机器 Key 交付失败")
	ErrRollbackFailed  = errors.New("机器凭据回滚失败")
	ErrCleanupFailed   = errors.New("待审批申请清理失败")
)

// Request 是只在等待管理员审批期间持久化的机器注册申请。
type Request struct {
	RegistrationID string                  `json:"registration_id"`
	PrincipalID    string                  `json:"principal_id"`
	MachineID      string                  `json:"machine_id"`
	AllowedSources []credential.SourceRule `json:"allowed_sources"`
	RequestedAt    time.Time               `json:"requested_at"`
}
```

- [ ] **Step 5：实现版本化原子 Store**

`internal/machinereg/store.go` 固定接口：

```go
const storeVersion = 1
const registrationIDRandomBytes = 16

type StoreOptions struct {
	Now func() time.Time
	Random io.Reader
}

func LoadStore(path string, options StoreOptions) (*Store, error)
func (store *Store) Create(principalID, machineID string, sources []credential.SourceRule) (Request, bool, error)
func (store *Store) List() []Request
func (store *Store) Show(registrationID string) (Request, error)
func (store *Store) Find(principalID, machineID string) (Request, bool)
func (store *Store) HasPrincipal(principalID string) bool
func (store *Store) Delete(registrationID string) (Request, error)
```

`Create` 在同一写锁内检查 `(principal_id,machine_id)`、生成 `reg_` 加 128 位 Base64URL 随机 ID、构造候选 map、原子持久化后再替换内存。`List` 按 `requested_at`、`registration_id` 稳定排序。文件写入完整复用 CredentialStore 的 `0700/0600`、临时文件、`Sync`、`Rename` 和目录同步语义。

- [ ] **Step 6：运行 Store 测试和竞态测试**

```sh
go test ./internal/credential ./internal/machinereg -count=1
go test -race ./internal/machinereg -count=1
```

Expected: PASS，race detector 无报告。

- [ ] **Step 7：提交 Store**

```sh
git add internal/credential/key.go internal/credential/key_test.go internal/machinereg/model.go internal/machinereg/store.go internal/machinereg/store_test.go
git commit -m "feat: 添加机器注册待审批存储"
```

## Task 3：实现 MachineRegistrationService 与凭据协调

**Files:**

- Create: `internal/machinereg/service.go`
- Create: `internal/machinereg/audit.go`
- Create: `internal/machinereg/service_test.go`

- [ ] **Step 1：写首台与 pending 失败测试**

```go
func TestServiceAutoIssuesOnlyForPrincipalWithoutAnyRecords(t *testing.T) {
	harness := newServiceHarness(t)
	var delivered KeyDelivery
	result, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "office", Sources: []string{"192.168.0.1"},
	}, func(_ context.Context, value KeyDelivery) error { delivered = value; return nil })
	if err != nil || result.Disposition != DispositionAutoIssued || delivered.Token == "" || result.CredentialID == 0 {
		t.Fatalf("result=%#v delivery=%#v err=%v", result, delivered, err)
	}
	second, err := harness.service.Register(context.Background(), RegisterInput{
		PrincipalID: "user-a", MachineID: "mobile", Sources: []string{"192.168.0.2"},
	}, nil)
	if err != nil || second.Disposition != DispositionPending || second.Request == nil {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}
```

同时覆盖 disabled/expired 凭据、任意 pending、同名凭据、重复 pending 保留原来源。

- [ ] **Step 2：写审批、拒绝和回滚失败测试**

```go
func TestApproveRollsBackCredentialAndKeepsRequestWhenDeliveryFails(t *testing.T) {
	harness := newServiceHarness(t)
	request := harness.createPending(t, "user-a", "office", "192.168.0.1")
	_, err := harness.service.Approve(context.Background(), request.RegistrationID, "admin", func(context.Context, KeyDelivery) error {
		return errors.New("wecom unavailable")
	})
	if !errors.Is(err, ErrDeliveryFailed) { t.Fatalf("error=%v", err) }
	if _, err := harness.requests.Show(request.RegistrationID); err != nil { t.Fatalf("pending removed: %v", err) }
	if hasMachine(harness.credentials.List(), "user-a", "office") { t.Fatal("credential was not rolled back") }
}
```

再覆盖批准成功删除申请、拒绝删除申请、拒绝通知失败仍生效、回滚删除失败、批准时同名凭据冲突、人工 Issue 与 Register 并发只能有一个首台结果。

- [ ] **Step 3：运行测试确认失败**

```sh
go test ./internal/machinereg -run 'TestService|TestApprove' -count=1
```

Expected: FAIL，提示 Service 和交付模型未定义。

- [ ] **Step 4：补齐公开模型**

`model.go` 增加：

```go
type Disposition string
const (
	DispositionAutoIssued Disposition = "auto_issued"
	DispositionPending Disposition = "pending"
	DispositionAlreadyPending Disposition = "already_pending"
)

type RegisterInput struct { PrincipalID, MachineID string; Sources []string }
type RegisterResult struct { Disposition Disposition; Request *Request; CredentialID uint64 }
type KeyDelivery struct { PrincipalID, MachineID string; CredentialID uint64; Token string }
type KeyDeliveryFunc func(context.Context, KeyDelivery) error
type RejectionDelivery struct { PrincipalID, MachineID, RegistrationID, Reason string }
type RejectionDeliveryFunc func(context.Context, RejectionDelivery) error
type ApprovalResult struct { Request Request; CredentialID uint64 }
type RejectionResult struct { Request Request; NotificationSent bool }

const MaxRejectionReasonBytes = 512

// OperationError 提供不含 Key 和文件路径的失败阶段及关联凭据。
type OperationError struct {
	Kind error
	Stage string
	CredentialID uint64
	Cause error
}

func (err *OperationError) Error() string {
	if err == nil { return "机器注册操作失败" }
	if err.CredentialID != 0 { return fmt.Sprintf("%v（credential_id=%d）", err.Kind, err.CredentialID) }
	return err.Kind.Error()
}
func (err *OperationError) Unwrap() []error {
	if err == nil { return nil }
	if err.Cause == nil { return []error{err.Kind} }
	return []error{err.Kind, err.Cause}
}
func CredentialIDFromError(value error) (uint64, bool) {
	var operation *OperationError
	if !errors.As(value, &operation) || operation.CredentialID == 0 { return 0, false }
	return operation.CredentialID, true
}
```

- [ ] **Step 5：实现协调 Service**

`service.go` 使用 64 个固定 principal 锁，避免无界锁 map：

```go
const principalLockCount = 64

type CredentialManager interface {
	Issue(string, string, []string, *time.Time) (string, credential.Record, error)
	List() []credential.Record
	Show(uint64) (credential.Record, error)
	Enable(uint64) (credential.Record, error)
	Disable(uint64) (credential.Record, error)
	Delete(uint64) (credential.Record, error)
	AddSources(uint64, []string) (credential.Record, error)
	RemoveSources(uint64, []string) (credential.Record, error)
	SetSources(uint64, []string) (credential.Record, error)
}

type Service struct {
	credentials CredentialManager
	requests *Store
	auditor audit.Auditor
	redactor *audit.Redactor
	logger *slog.Logger
	botIDHash string
	now func() time.Time
	locks [principalLockCount]sync.Mutex
}

type Config struct {
	Credentials CredentialManager
	Requests *Store
	Auditor audit.Auditor
	Redactor *audit.Redactor
	Logger *slog.Logger
	BotIDHash string
	Now func() time.Time
}

func New(config Config) (*Service, error)
func (service *Service) Register(context.Context, RegisterInput, KeyDeliveryFunc) (RegisterResult, error)
func (service *Service) ListPending() []Request
func (service *Service) Approve(context.Context, string, string, KeyDeliveryFunc) (ApprovalResult, error)
func (service *Service) Reject(context.Context, string, string, string, RejectionDeliveryFunc) (RejectionResult, error)
```

`Register` 锁内顺序：规范化来源 → 同名凭据 → 重复 pending → 判断用户是否零凭据且零 pending → 自动签发或创建 pending。自动签发时 KeyDeliveryFunc 必须非 nil；交付失败删除新 credential，删除失败包装 `ErrRollbackFailed`。

`Approve` 顺序：Show pending → 锁 principal → 再次 Show → 同名检查 → 签发 → 交付 → 删除 pending。交付失败后 credential 删除失败返回带 `credential_id` 的 `ErrRollbackFailed`；交付成功后 pending 删除失败返回带 `credential_id` 的 `ErrCleanupFailed`。使用包含 `Kind/Stage/CredentialID/Cause` 的领域错误承载这些安全诊断，不把 Token 放入错误文本。`Reject` 锁内删除 pending 后通知；通知失败返回 `RejectionResult{NotificationSent:false}` 和 nil error，不重建申请，同时写 WARN 日志和审计投递状态。

Service 实现 AdminService 当前 CredentialManager 的全部方法。人工 `Issue` 发现同一 principal/machine 已有 pending 时返回 credential conflict，要求通过 Web 审批或先拒绝申请，不能留下“已有凭据 + stale pending”。按 credential ID 修改时先 Show 取得 principal，再加锁并重新读取后执行，确保人工签发/删除与 `/reg` 不跨越首台判断。

拒绝原因先 `strings.TrimSpace`，必须是合法 UTF-8、最多 512 字节且不含 C0/C1 控制字符；不合法时不删除 pending。发送给企微前继续执行 Markdown 安全转义。

- [ ] **Step 6：实现审计构造**

`audit.go` 集中生成 `audit.EventNameMachineRegistration`，属性固定为：

```text
registration.id
registration.sources
admin.username
credential.id
error.stage
error.type
```

Action 固定为 `request/auto_issue/approve/reject/rollback`，Outcome 固定为 `pending/delivered/rejected/delivery_failed/rollback_failed`。拒绝通知是否成功写入 `audit.Event.Delivery`。Body 必须先经过 Redactor，禁止包含 Token；审计事件构造失败通过注入的 Logger 记录安全错误类型，但不影响业务结果。

- [ ] **Step 7：运行测试和竞态测试**

```sh
go test ./internal/machinereg -count=1
go test -race ./internal/credential ./internal/machinereg -count=1
```

Expected: PASS，race detector 无报告。

- [ ] **Step 8：提交协调服务**

```sh
git add internal/machinereg
git commit -m "feat: 实现机器自主注册与审批协调"
```

## Task 4：在 ConversationRouter 中增加 `/reg`

**Files:**

- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [ ] **Step 1：写解析失败测试**

```go
func TestParseServerActionRegistration(t *testing.T) {
	tests := []struct { input, machine string; sources []string; wantErr bool }{
		{input: "/reg office 192.168.0.1", machine: "office", sources: []string{"192.168.0.1"}},
		{input: "/reg office 192.168.0.1,10.0.0.0/24", machine: "office", sources: []string{"192.168.0.1", "10.0.0.0/24"}},
		{input: "/reg", wantErr: true},
		{input: "/reg office", wantErr: true},
		{input: "/reg office 192.168.0.1 extra", wantErr: true},
		{input: "/reg office 192.168.0.1,", wantErr: true},
	}
	for _, test := range tests {
		action, err := parseServerAction(test.input)
		if test.wantErr { if err == nil { t.Fatalf("%q accepted", test.input) }; continue }
		if err != nil || action.kind != serverActionRegister || action.machineID != test.machine || !reflect.DeepEqual(action.sources, test.sources) {
			t.Fatalf("%q action=%#v err=%v", test.input, action, err)
		}
	}
}
```

- [ ] **Step 2：写无会话注册和结果文案测试**

为 Router 注入 fake `RegistrationRequester`，覆盖无 Session 放行、自动签发通过当前 `RespondMarkdown` 返回 Key 与 `/help`、pending/重复 pending 文案、同名机器错误、其他无会话输入仍被拒绝、普通日志不包含 Key。

额外覆盖无会话时非法 `/reg` 仍返回 `/reg` 用法而不是通用“当前没有可用会话”，以及 `/N /reg ...`、`#N /reg ...` 继续被定向前缀规则拒绝。

- [ ] **Step 3：运行测试确认失败**

```sh
go test ./internal/server -run 'TestParseServerActionRegistration|TestRouter.*Registration' -count=1
```

Expected: FAIL，提示注册 action 或依赖未定义。

- [ ] **Step 4：增加显式依赖和 action**

```go
type RegistrationRequester interface {
	Register(context.Context, machinereg.RegisterInput, machinereg.KeyDeliveryFunc) (machinereg.RegisterResult, error)
}
```

将该接口作为 `NewConversationRouter` 与 `NewConversationRouterWithConfig` 的必填参数，并更新全部调用点。`serverAction` 增加 `serverActionRegister`、`machineID string`、`sources []string`，并让 `serverActionName` 返回 `register`，使现有用户输入审计能识别该命令。

`/reg` 必须 `len(fields)==3`；来源用 `strings.Split(fields[2], ",")`，任何 trim 后空项都返回：

```text
/reg 用法: /reg <机器标识> <来源1,来源2>
```

- [ ] **Step 5：实现注册处理**

无会话门禁放行 help 和 register。解析失败路径在第一字段精确等于 `/reg` 时优先返回 `/reg` 用法，不被无会话通用提示覆盖。新增 `handleRegister`，调用 `RegistrationRequester.Register`；交付回调使用当前 `router.reply`。自动签发回复格式固定包含机器标识、一次性 Key 和“发送 `/help` 查看安装步骤”，不包含 URL。若该交付回调失败，Service 已执行凭据回滚，Router 只记录失败，不再尝试发送第二条错误回复；其他注册校验错误仍通过当前请求明确回复。

pending 回复申请编号；重复 pending 回复原编号；同名凭据回复“该机器已注册，请联系管理员处理”。无会话默认提示改为：

```text
当前没有可用会话，可使用 /reg <机器标识> <来源地址> 注册机器；使用 /help 获取安装帮助。
```

- [ ] **Step 6：运行 Router 全量测试**

```sh
go test ./internal/server -count=1
```

Expected: PASS。

- [ ] **Step 7：提交 Router 支持**

```sh
git add internal/server/router.go internal/server/router_test.go
git commit -m "feat: 支持企微机器自主注册命令"
```

## Task 5：扩展 AdminService 的审批领域接口

**Files:**

- Modify: `internal/adminservice/model.go`
- Modify: `internal/adminservice/errors.go`
- Modify: `internal/adminservice/service.go`
- Modify: `internal/adminservice/service_test.go`

- [ ] **Step 1：写失败测试**

使用 fake RegistrationManager 测试 List、Approve、Reject、not found、machine conflict、delivery failed、rollback failed、cleanup failed 和非法拒绝原因。批准结果断言不包含任何 Token：

```go
func TestServiceApprovesRegistrationWithoutReturningToken(t *testing.T) {
	service := newTestServiceWithRegistrations(t, populatedRegistrationManager())
	result, err := service.ApproveRegistration(context.Background(), "reg_one", "admin")
	if err != nil || result.CredentialID != 7 || strings.Contains(fmt.Sprintf("%#v", result), "hpk_") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```sh
go test ./internal/adminservice -run 'TestService.*Registration' -count=1
```

Expected: FAIL，提示注册管理类型和方法未定义。

- [ ] **Step 3：增加安全视图和错误码**

```go
type Registration struct {
	RegistrationID string `json:"registration_id"`
	PrincipalID string `json:"principal_id"`
	MachineID string `json:"machine_id"`
	AllowedSources []string `json:"allowed_sources"`
	RequestedAt time.Time `json:"requested_at"`
}
type RegistrationApprovalResult struct { RegistrationID string `json:"registration_id"`; CredentialID uint64 `json:"credential_id"`; Approved bool `json:"approved"` }
type RegistrationRejectionResult struct { RegistrationID string `json:"registration_id"`; Rejected bool `json:"rejected"`; NotificationSent bool `json:"notification_sent"` }
```

错误码增加 `registration_not_found`、`registration_conflict`、`registration_delivery_failed`、`registration_rollback_failed`、`registration_cleanup_failed`。rollback/cleanup 错误消息允许包含安全的 `credential_id`，不得包含 Key 或底层文件路径。

- [ ] **Step 4：实现管理方法**

`Config` 增加 RegistrationManager、KeyDeliveryFunc、RejectionDeliveryFunc，全部作为必填依赖。RegistrationManager 的 Reject 返回 `machinereg.RejectionResult`。实现 `ListRegistrations`、`ApproveRegistration`、`RejectRegistration`。拒绝通知失败必须返回 `Rejected=true, NotificationSent=false` 和 nil error，不能让 Web 误判为决定未生效。

RegistrationManager 接口固定为：

```go
type RegistrationManager interface {
	ListPending() []machinereg.Request
	Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error)
	Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error)
}
```

- [ ] **Step 5：更新测试装配并运行全量测试**

```sh
go test ./internal/adminservice -count=1
```

Expected: PASS。

- [ ] **Step 6：提交管理领域扩展**

```sh
git add internal/adminservice
git commit -m "feat: 添加机器注册审批管理接口"
```

## Task 6：增加 Web 管理审批页面和 API

**Files:**

- Create: `internal/webadmin/registration_handler.go`
- Create: `internal/webadmin/registration_handler_test.go`
- Create: `internal/webadmin/assets/templates/registrations.html`
- Modify: `internal/webadmin/management_handler.go`
- Modify: `internal/webadmin/assets.go`
- Modify: `internal/webadmin/assets/templates/layout.html`
- Modify: `internal/webadmin/assets/static/app.js`
- Modify: `internal/webadmin/assets_test.go`
- Modify: `internal/webadmin/server_test.go`

- [ ] **Step 1：写 API 和资源失败测试**

测试 GET 分页、伪造 page token、CSRF、批准响应无 Key、拒绝 reason、错误映射、HTTP 方法和非管理员访问。关键断言：

```go
approved := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_one/approve", `{}`)
if approved.Code != http.StatusOK || strings.Contains(approved.Body.String(), "hpk_") || !strings.Contains(approved.Body.String(), `"credential_id":7`) {
	t.Fatalf("status=%d body=%s", approved.Code, approved.Body.String())
}
```

`assets_test.go` 断言 `/admin/registrations`、导航、表格 ID、JS approve/reject endpoint 均存在，且审批代码不调用 `showSecret`。

- [ ] **Step 2：运行测试确认失败**

```sh
go test ./internal/webadmin -run 'TestRegistration|TestEmbeddedAssets' -count=1
```

Expected: FAIL，返回 404 或缺少资源。

- [ ] **Step 3：实现 JSON API**

注册：

```go
server.managementRoute(mux, "/admin/api/v1/registrations", http.MethodGet, http.HandlerFunc(server.listRegistrations), false)
server.managementRoute(mux, "/admin/api/v1/registrations/{id}/approve", http.MethodPost, http.HandlerFunc(server.approveRegistration), true)
server.managementRoute(mux, "/admin/api/v1/registrations/{id}/reject", http.MethodPost, http.HandlerFunc(server.rejectRegistration), true)
```

列表复用 `pageResponse`，资源名固定 `registrations`。列表按 `(requested_at, registration_id)` 排序，page token 的 anchor 编码为 `RFC3339Nano + "\t" + registration_id`；解析后按同一元组继续，避免随机 ID 与时间排序不一致。批准只接受空对象；拒绝请求为 `struct{ Reason string }`。批准和拒绝从 `browserIdentityFrom` 读取管理员用户名。请求日志 target 只记录申请 ID。

- [ ] **Step 4：实现页面和前端**

增加 `/admin/registrations` 页面、导航和模板分支。模板列为申请编号、用户、机器、来源、申请时间、操作。

`app.js` 增加 cursor、loader、刷新和：

```js
async function approveRegistration(item) {
  if (!window.confirm(`确认批准 ${item.principal_id}/${item.machine_id}？`)) return;
  const result = await api(`/admin/api/v1/registrations/${encodeURIComponent(item.registration_id)}/approve`, { method: "POST", body: {} });
  showToast(`已批准，凭据 ID：${result.credential_id}`);
  await loadRegistrations(true);
}

async function rejectRegistration(item) {
  const reason = window.prompt(`拒绝 ${item.principal_id}/${item.machine_id}，可填写原因`, "");
  if (reason === null) return;
  const result = await api(`/admin/api/v1/registrations/${encodeURIComponent(item.registration_id)}/reject`, { method: "POST", body: { reason } });
  showToast(result.notification_sent ? "已拒绝并通知用户" : "已拒绝，但通知发送失败");
  await loadRegistrations(true);
}
```

- [ ] **Step 5：运行 WebAdmin 全量测试**

```sh
go test ./internal/webadmin -count=1
```

Expected: PASS。

- [ ] **Step 6：提交 Web 审批界面**

```sh
git add internal/webadmin
git commit -m "feat: 添加机器注册审批页面"
```

## Task 7：装配 Server 与本地集成链路

**Files:**

- Modify: `internal/serverapp/app.go`
- Modify: `internal/serverapp/app_test.go`
- Modify: `internal/integration/admin_test.go`
- Modify: `internal/integration/relay_test.go`

- [ ] **Step 1：写装配和集成失败测试**

使用临时 `state_dir` 验证 registrations 路径可加载，损坏/权限过宽文件让 Server 明确失败且不泄漏 Secret。集成流程固定覆盖：首台 `/reg` 返回 Key → 第二台 pending → AdminService 批准 → fake WeCom 收到第二把 Key → pending 清空 → 两条 credential；另测主动发送失败后 credential 回滚、pending 保留。

- [ ] **Step 2：运行测试确认失败**

```sh
go test ./internal/serverapp ./internal/integration -run 'Test.*Registration' -count=1
```

Expected: FAIL，Server 尚未装配注册服务。

- [ ] **Step 3：装配共享协调器**

`serverapp.Run` 创建：

```go
credentialStore, err := credential.LoadStore(loaded.Server.CredentialsFile)
registrationStore, err := machinereg.LoadStore(filepath.Join(loaded.Server.StateDir, "registrations.json"), machinereg.StoreOptions{})
registrationService, err := machinereg.New(machinereg.Config{
	Credentials: credentialStore,
	Requests: registrationStore,
	Auditor: businessAuditor,
	Redactor: auditRedactor,
	Logger: logger,
	BotIDHash: shortHash(loaded.WeCom.BotID),
	Now: time.Now,
})
```

`ClientHub` 继续使用原始 credentialStore 验证 Bearer；Router 注入 registrationService；AdminService 的 Credentials 和 Registrations 都注入 registrationService。

审批 Key 回调使用 `weComClient.SendMarkdownTo`，文案包含机器、Key 和 `/help`，不包含 URL；拒绝回调包含申请编号、机器和可选原因。普通日志不得记录回调正文。

- [ ] **Step 4：更新全部构造调用点**

更新 `router_test.go`、`relay_test.go`、Web helper 和 AdminService helper，注入真实临时 service 或明确 fake。生产构造函数不允许 nil/noop 注册依赖。

- [ ] **Step 5：运行 Server、集成和竞态测试**

```sh
go test ./internal/serverapp ./internal/integration -count=1
go test -race ./internal/credential ./internal/machinereg ./internal/server ./internal/adminservice ./internal/webadmin ./internal/serverapp ./internal/integration -count=1
```

Expected: PASS，race detector 无报告。

- [ ] **Step 6：提交 Server 装配**

```sh
git add internal/serverapp internal/integration
git commit -m "feat: 接通机器注册审批链路"
```

## Task 8：更新帮助和文档并完成全量验证

**Files:**

- Modify: `internal/server/default_help.md`
- Modify: `internal/server/help_test.go`
- Modify: `README.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`

- [ ] **Step 1：写默认帮助失败测试**

```go
func TestDefaultHelpIncludesMachineRegistration(t *testing.T) {
	content := DefaultHelpText()
	for _, fragment := range []string{"/reg", "机器标识", "来源地址", "首台", "等待管理员审批", "/help"} {
		if !strings.Contains(content, fragment) { t.Fatalf("default help missing %q", fragment) }
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```sh
go test ./internal/server -run TestDefaultHelpIncludesMachineRegistration -count=1
```

Expected: FAIL。

- [ ] **Step 3：更新默认帮助与 README**

默认帮助增加：

```text
【首次接入机器】
`/reg 当前运行 Herdr 的机器标识 来源地址`
例如：`/reg office-laptop 192.168.0.1`
多个来源使用逗号分隔，例如：`/reg office-laptop 192.168.0.1,10.0.0.0/24`

用户没有任何机器记录时会直接收到本机 Key；已有机器时申请进入管理员审批。收到 Key 后发送 `/help`，按本帮助中的地址、下载和安装步骤部署 Herdr Pal。
```

README 把“管理员签发机器 Key”调整为优先说明 `/reg`，保留管理员人工签发作为备用路径；明确 Server 不覆盖现有 `help.md`。

- [ ] **Step 4：同步维护文档**

`HANDOFF_CONTEXT.md` 和 `BRIDGE_ARCHITECTURE.md` 写明首台自动签发、后续 pending、Web-only 审批、registrations.json 只保存 pending、历史进入 `herdr_pal.machine_registration`、Pal/HPRP/HPAP 不变。

- [ ] **Step 5：运行关联测试和差异检查**

```sh
go test ./internal/server ./internal/webadmin ./internal/serverapp -count=1
git diff --check
```

Expected: PASS，`git diff --check` 无输出。

- [ ] **Step 6：运行项目完整验证**

```sh
./unittest.sh
./build.sh
```

Expected: 两个命令 exit 0；所有单元、集成、竞态测试和跨平台构建通过。

- [ ] **Step 7：执行安全扫描**

```sh
rg -n 'hpk_[0-9]+_' internal/machinereg internal/server internal/adminservice internal/webadmin internal/serverapp --glob '*.go'
rg -n 'TBD|TODO|FIXME' docs/MACHINE_SELF_REGISTRATION_DESIGN.md docs/MACHINE_SELF_REGISTRATION_IMPLEMENTATION_PLAN.md
git status --short
```

Expected：Key 模式只出现在测试固定数据、Redactor 测试或用户交付文案断言；计划与设计无占位；工作区只包含本功能修改。

- [ ] **Step 8：提交文档并再次验证**

```sh
git add README.md docs/HANDOFF_CONTEXT.md docs/BRIDGE_ARCHITECTURE.md internal/server/default_help.md internal/server/help_test.go docs/MACHINE_SELF_REGISTRATION_IMPLEMENTATION_PLAN.md
git commit -m "docs: 更新机器自主注册使用说明"
./unittest.sh
./build.sh
git status --short --branch
```

Expected: 测试与构建 exit 0，工作区干净。

## 实施完成条件

- `/reg` 在没有在线会话时可用，参数和来源规则严格校验。
- 首台机器只在用户完全没有凭据和 pending 时自动签发。
- 后续机器进入 pending，Web 管理员可以批准或拒绝。
- 明文 Key 只通过企业微信返回一次，不进入 JSON、浏览器响应、普通日志或 Loki。
- Key 发送失败时凭据回滚并保留 pending；回滚失败有明确诊断。
- 批准和拒绝完成后 pending 被删除；历史仅进入 OTLP/Loki。
- Pal、HPRP、HPAP 和自动化 Token API 的协议表面不变。
- `./unittest.sh`、`./build.sh` 和指定 race 测试全部通过。
