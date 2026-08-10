# 企业微信机器注册审批实施计划

> **执行方式：** 使用 `superpowers:executing-plans` 在当前会话逐项执行；根据项目约束，不使用 subagent-driven development。

**目标：** 在不修改 Pal、HPRP/1、HPAP/1 和 Herdr 的前提下，为服务端增加企业微信管理员轮转通知、待审批列表、批量批准和批量驳回功能。

**架构：** 新增独立的 `RegistrationApprovalCoordinator`，负责管理员身份、轮转通知、管理员私有列表快照和稳定 ID 审批。`ConversationRouter` 只解析命令并转发，`machinereg.Service` 继续负责签发、交付、回滚与注册存储。Web 管理审批保持原有路径，不依赖企业微信编号快照。

**技术栈：** Go、企业微信智能机器人长连接、现有 `machinereg.Service`、`slog`、Go 标准测试框架、项目 fake WeCom 集成测试。

---

## 任务 1：增加注册审批管理员配置

**文件：**

- 修改：`internal/config/server.go`
- 修改：`internal/config/relay_test.go`

### 步骤 1：编写失败测试

在 `internal/config/relay_test.go` 增加以下测试：

```go
func TestLoadServerNormalizesRegistrationAdminIDs(t *testing.T) {
	configPath := writeServerConfig(t, `{
		"listen": "127.0.0.1:0",
		"wecom": {
			"bot_id": "bot",
			"secret": "secret",
			"registration_admin_ids": [" admin-a ", "admin-b"]
		}
	}`)

	config, err := LoadServer(configPath)
	if err != nil {
		t.Fatalf("LoadServer() error = %v", err)
	}
	want := []string{"admin-a", "admin-b"}
	if !reflect.DeepEqual(config.WeCom.RegistrationAdminIDs, want) {
		t.Fatalf("RegistrationAdminIDs = %#v, want %#v", config.WeCom.RegistrationAdminIDs, want)
	}
}

func TestLoadServerRejectsInvalidRegistrationAdminIDs(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "blank", value: `["   "]`},
		{name: "duplicate after trim", value: `["admin-a", " admin-a "]`},
		{name: "invalid principal", value: `["admin/a"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := writeServerConfig(t, fmt.Sprintf(`{
				"listen": "127.0.0.1:0",
				"wecom": {
					"bot_id": "bot",
					"secret": "secret",
					"registration_admin_ids": %s
				}
			}`, tt.value))

			_, err := LoadServer(configPath)
			if err == nil || !strings.Contains(err.Error(), "wecom.registration_admin_ids") {
				t.Fatalf("LoadServer() error = %v", err)
			}
		})
	}
}
```

若现有测试辅助函数名称不同，沿用同文件已有的临时配置写入方式，不复制第二套辅助设施。

### 步骤 2：确认测试失败

运行：

```bash
go test ./internal/config -run 'TestLoadServer(Normalizes|RejectsInvalid)RegistrationAdminIDs' -count=1
```

预期：因 `ServerWeComConfig` 尚无 `RegistrationAdminIDs` 字段或没有校验而失败。

### 步骤 3：实现配置字段和规范化

在 `internal/config/server.go` 中扩展配置：

```go
type ServerWeComConfig struct {
	BotID                string   `json:"bot_id"`
	Secret               string   `json:"secret"`
	RegistrationAdminIDs []string `json:"registration_admin_ids"`
}
```

增加纯函数，并在 `LoadServerAdmin` 完成 JSON 解析后调用：

```go
func normalizeRegistrationAdminIDs(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}

	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		adminID := strings.TrimSpace(value)
		if adminID == "" {
			return nil, errors.New("wecom.registration_admin_ids 包含空白用户 ID")
		}
		if err := credential.ValidatePrincipalID(adminID); err != nil {
			return nil, fmt.Errorf("wecom.registration_admin_ids 包含无效用户 ID: %w", err)
		}
		if _, ok := seen[adminID]; ok {
			return nil, fmt.Errorf("wecom.registration_admin_ids 包含重复用户 ID: %s", adminID)
		}
		seen[adminID] = struct{}{}
		normalized = append(normalized, adminID)
	}
	return normalized, nil
}
```

错误中只包含用户提供的管理员 ID；配置加载错误会直接打印给本机管理员，不进入普通运行日志。若现有 `credential.ValidatePrincipalID` 的允许字符不符合企业微信 ID，先增加独立的、等价于现有 principal ID 约束的本地校验函数，并补充边界测试，不放宽到路径分隔符或控制字符。

### 步骤 4：运行配置测试

```bash
go test ./internal/config -run 'RegistrationAdminIDs' -count=1
```

预期：通过。

### 步骤 5：完成提交前验证

```bash
./unittest.sh
./build.sh
git diff --check
```

预期：全部通过。

### 步骤 6：提交

```bash
git add internal/config/server.go internal/config/relay_test.go
git commit -m "feat: 配置企业微信注册审批管理员"
```

---

## 任务 2：实现注册审批协调器

**文件：**

- 新增：`internal/server/registration_approval.go`
- 新增：`internal/server/registration_approval_test.go`

### 步骤 1：先定义可测试接口并编写失败测试

在测试文件内提供线程安全 fake，分别记录：

- `ListPending` 返回的稳定顺序。
- `Approve`、`Reject` 收到的 `registration_id` 和管理员身份。
- 主动消息接收者和正文。
- 每个稳定 ID 可注入的领域错误。

测试函数固定为：

```go
func TestRegistrationApprovalCoordinatorNotifiesAdminsRoundRobin(t *testing.T)
func TestRegistrationApprovalCoordinatorNotificationFailureDoesNotRetryAdmin(t *testing.T)
func TestRegistrationApprovalCoordinatorListStoresPrivateSnapshots(t *testing.T)
func TestRegistrationApprovalCoordinatorAllowsGlobalListGrowth(t *testing.T)
func TestRegistrationApprovalCoordinatorRejectsChangedSelectedPosition(t *testing.T)
func TestRegistrationApprovalCoordinatorInvalidatesSnapshotAfterAttempt(t *testing.T)
func TestRegistrationApprovalCoordinatorContinuesBatchAfterItemFailure(t *testing.T)
func TestRegistrationApprovalCoordinatorConcurrentSnapshotsDoNotApproveShiftedItem(t *testing.T)
```

关键竞争测试使用如下顺序：

```go
func TestRegistrationApprovalCoordinatorConcurrentSnapshotsDoNotApproveShiftedItem(t *testing.T) {
	manager := newFakeRegistrationApprovalManager(
		registrationRequest("reg-a", "user-a", "machine-a"),
		registrationRequest("reg-b", "user-b", "machine-b"),
	)
	coordinator := newTestRegistrationApprovalCoordinator(t, manager, "admin-a", "admin-b")

	if _, err := coordinator.List("admin-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.List("admin-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Approve(context.Background(), "admin-a", []int{1}); err != nil {
		t.Fatal(err)
	}

	_, err := coordinator.Approve(context.Background(), "admin-b", []int{1})
	if !errors.Is(err, ErrRegistrationApprovalSnapshotChanged) {
		t.Fatalf("Approve() error = %v", err)
	}
	if got := manager.approvedIDs(); !reflect.DeepEqual(got, []string{"reg-a"}) {
		t.Fatalf("approved IDs = %#v", got)
	}
}
```

### 步骤 2：确认测试失败

```bash
go test ./internal/server -run 'TestRegistrationApprovalCoordinator' -count=1
```

预期：协调器类型和错误尚不存在，测试不能通过。

### 步骤 3：实现协调器公开边界

在 `internal/server/registration_approval.go` 中定义：

```go
var (
	ErrRegistrationApprovalUnauthorized    = errors.New("当前用户不是注册审批管理员")
	ErrRegistrationApprovalSnapshotMissing = errors.New("尚未获取注册审批列表，请先执行 /ls-reg")
	ErrRegistrationApprovalSnapshotChanged = errors.New("注册审批列表已变化，本次未执行")
)

const registrationApprovalSnapshotReminder = "列表快照已失效，请重新执行 /ls-reg 核实当前条目顺序。"

type RegistrationApprovalManager interface {
	ListPending() []machinereg.Request
	Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error)
	Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error)
}

type RegistrationApprovalHandler interface {
	IsAdmin(string) bool
	NotifyPending(context.Context, machinereg.Request) error
	List(string) (string, error)
	Approve(context.Context, string, []int) (string, error)
	Reject(context.Context, string, []int) (string, error)
}

type RegistrationApprovalCoordinatorConfig struct {
	AdminIDs          []string
	Registrations     RegistrationApprovalManager
	Gateway           WeComGateway
	KeyDelivery       machinereg.KeyDeliveryFunc
	RejectionDelivery machinereg.RejectionDeliveryFunc
	Logger            *slog.Logger
}

type RegistrationApprovalCoordinator struct {
	adminIDs          []string
	adminSet          map[string]struct{}
	registrations     RegistrationApprovalManager
	gateway           WeComGateway
	keyDelivery       machinereg.KeyDeliveryFunc
	rejectionDelivery machinereg.RejectionDeliveryFunc
	logger            *slog.Logger

	mu        sync.Mutex
	nextAdmin int
	snapshots map[string][]string
}
```

为构造器和所有公开方法添加 Go 文档注释。构造器复制管理员列表，避免外部修改配置切片影响运行状态。

### 步骤 4：实现轮转通知

通知管理员的选择只在锁内完成：

```go
func (c *RegistrationApprovalCoordinator) NotifyPending(ctx context.Context, request machinereg.Request) error {
	c.mu.Lock()
	if len(c.adminIDs) == 0 {
		c.mu.Unlock()
		return nil
	}
	adminID := c.adminIDs[c.nextAdmin]
	c.nextAdmin = (c.nextAdmin + 1) % len(c.adminIDs)
	c.mu.Unlock()

	content := formatRegistrationNotification(request)
	if err := c.gateway.SendMarkdownTo(ctx, adminID, content); err != nil {
		c.logger.Warn("机器注册审批通知失败",
			"admin_hash", routerHash(adminID),
			"registration_id", request.RegistrationID,
			"machine_id", request.MachineID,
			"error_type", safeRouterLabel(errorType(err)),
			"reason", safeRouterLabel(err.Error()),
		)
		return err
	}
	return nil
}
```

使用项目现有的安全日志辅助函数；若现有错误分类函数名称不同，复用 `serverErrorLogArgs`，不创建重复分类体系。

### 步骤 5：实现列表和快照复核

`List` 先鉴权，获取稳定全局列表，再复制 ID 顺序：

```go
func (c *RegistrationApprovalCoordinator) List(adminID string) (string, error) {
	if !c.IsAdmin(adminID) {
		return "", ErrRegistrationApprovalUnauthorized
	}
	requests := c.registrations.ListPending()
	ids := make([]string, len(requests))
	for index, request := range requests {
		ids[index] = request.RegistrationID
	}
	c.mu.Lock()
	c.snapshots[adminID] = ids
	c.mu.Unlock()
	return formatPendingRegistrations(requests), nil
}
```

审批前立即取走快照，再验证所选位置：

```go
func (c *RegistrationApprovalCoordinator) resolveSelection(adminID string, indexes []int) ([]registrationSelection, error) {
	if !c.IsAdmin(adminID) {
		return nil, ErrRegistrationApprovalUnauthorized
	}
	c.mu.Lock()
	snapshot, ok := c.snapshots[adminID]
	delete(c.snapshots, adminID)
	c.mu.Unlock()
	if !ok {
		return nil, ErrRegistrationApprovalSnapshotMissing
	}

	for _, index := range indexes {
		if index < 1 || index > len(snapshot) {
			return nil, ErrRegistrationApprovalSnapshotChanged
		}
	}
	current := c.registrations.ListPending()
	selections := make([]registrationSelection, 0, len(indexes))
	for _, index := range indexes {
		position := index - 1
		if position >= len(current) || current[position].RegistrationID != snapshot[position] {
			return nil, ErrRegistrationApprovalSnapshotChanged
		}
		selections = append(selections, registrationSelection{
			Index:          index,
			RegistrationID: snapshot[position],
			MachineID:      current[position].MachineID,
		})
	}
	return selections, nil
}
```

### 步骤 6：实现批量批准和驳回

两个方法复用 `resolveSelection`，但逐项调用领域服务并继续处理后续稳定 ID。管理员审计身份固定为：

```go
actor := "wecom:" + adminID
```

响应格式至少包含：

```text
1. office-pc：已批准，Key 已发送给申请人。
2. lab-mac：批准失败，Key 交付失败，申请仍保留。

列表快照已失效，请重新执行 /ls-reg 核实当前条目顺序。
```

列表缺失、编号变化等批量前错误也必须通过统一函数附加快照失效提醒：

```go
func registrationApprovalErrorMessage(err error) string {
	return err.Error() + "\n\n" + registrationApprovalSnapshotReminder
}
```

### 步骤 7：运行协调器测试和 race 测试

```bash
go test ./internal/server -run 'TestRegistrationApprovalCoordinator' -count=1
go test -race ./internal/server -run 'TestRegistrationApprovalCoordinator' -count=1
```

预期：全部通过，没有数据竞争。

### 步骤 8：完成提交前验证并提交

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/server/registration_approval.go internal/server/registration_approval_test.go
git commit -m "feat: 实现企业微信注册审批协调器"
```

---

## 任务 3：接入企业微信命令路由

**文件：**

- 修改：`internal/server/router.go`
- 修改：`internal/server/router_test.go`

### 步骤 1：编写命令解析失败测试

增加表驱动测试，覆盖：

```go
func TestParseServerActionRegistrationApprovalCommands(t *testing.T) {
	tests := []struct {
		input       string
		wantKind    serverActionKind
		wantIndexes []int
		wantErr     bool
	}{
		{input: "/ls-reg", wantKind: serverActionListRegistrations},
		{input: "/apr 1 2 3", wantKind: serverActionApproveRegistrations, wantIndexes: []int{1, 2, 3}},
		{input: "/rej 3 1", wantKind: serverActionRejectRegistrations, wantIndexes: []int{3, 1}},
		{input: "/apr", wantErr: true},
		{input: "/rej 0", wantErr: true},
		{input: "/apr 1 1", wantErr: true},
		{input: "/apr 1,2", wantErr: true},
		{input: "/apr 1-2", wantErr: true},
		{input: "/rej １", wantErr: true},
		{input: "/1 /ls-reg", wantErr: true},
		{input: "#1 /apr 1", wantErr: true},
	}

	for _, tt := range tests {
		action, err := parseServerAction(tt.input)
		if (err != nil) != tt.wantErr {
			t.Fatalf("parseServerAction(%q) error = %v", tt.input, err)
		}
		if err == nil && (action.kind != tt.wantKind || !reflect.DeepEqual(action.indexes, tt.wantIndexes)) {
			t.Fatalf("parseServerAction(%q) = %#v", tt.input, action)
		}
	}
}
```

增加路由行为测试：

```go
func TestConversationRouterAllowsAdminApprovalCommandsWithoutSessions(t *testing.T)
func TestConversationRouterRejectsApprovalCommandsFromNonAdmin(t *testing.T)
func TestConversationRouterNotifiesAdminOnlyForNewPendingRegistration(t *testing.T)
func TestConversationRouterKeepsPendingReplyWhenAdminNotificationFails(t *testing.T)
```

### 步骤 2：确认测试失败

```bash
go test ./internal/server -run 'RegistrationApprovalCommands|AdminApprovalCommands|NotifiesAdminOnly|KeepsPendingReply' -count=1
```

### 步骤 3：扩展动作结构和解析

在 `serverActionKind` 增加：

```go
serverActionListRegistrations
serverActionApproveRegistrations
serverActionRejectRegistrations
```

在 `serverAction` 增加：

```go
indexes []int
```

新增严格编号解析：

```go
func parseRegistrationIndexes(command string, fields []string) ([]int, error) {
	if len(fields) < 2 {
		return nil, fmt.Errorf("用法：/%s 1 2 3", command)
	}
	indexes := make([]int, 0, len(fields)-1)
	seen := make(map[int]struct{}, len(fields)-1)
	for _, field := range fields[1:] {
		if field == "" || strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return nil, fmt.Errorf("用法：/%s 1 2 3", command)
		}
		index, err := strconv.Atoi(field)
		if err != nil || index < 1 {
			return nil, fmt.Errorf("用法：/%s 1 2 3", command)
		}
		if _, ok := seen[index]; ok {
			return nil, fmt.Errorf("编号不能重复，请重新执行 /ls-reg")
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	return indexes, nil
}
```

定向前缀解析发现审批命令时，返回“注册审批命令不能使用 /N 或 #N 前缀”。

### 步骤 4：把审批协调器接入 Router

扩展 `ConversationRouterConfig`：

```go
RegistrationApproval RegistrationApprovalHandler
```

Router 保存一个禁用态实现，避免主路径散布 nil 判断。禁用态 `IsAdmin` 返回 false，通知为空操作，审批方法返回权限错误。

在检查“当前没有可用会话”之前处理审批动作：

```go
switch action.kind {
case serverActionListRegistrations,
	serverActionApproveRegistrations,
	serverActionRejectRegistrations:
	return r.handleRegistrationApproval(ctx, userID, action)
}
```

`handleRegistrationApproval` 先调用 `IsAdmin`，然后调用对应方法，使用当前企业微信请求正常回复。协调器返回的领域错误转成安全、可操作的文本，不暴露存储路径或内部错误。

### 步骤 5：在新 pending 后发送通知

修改 `handleRegistration`：

```go
case machinereg.DispositionPending:
	if err := r.reply(ctx, userID, requestID, pendingMessage(result.Request)); err != nil {
		return err
	}
	if err := r.registrationApproval.NotifyPending(ctx, result.Request); err != nil {
		r.logger.Warn("机器注册审批通知未送达", append(
			[]any{"registration_id", result.Request.RegistrationID},
			serverErrorLogArgs(err)...,
		)...)
	}
	return nil
```

`DispositionAlreadyPending` 只回复申请人，不调用通知。

### 步骤 6：运行测试并提交

```bash
go test ./internal/server -run 'RegistrationApproval|Registration' -count=1
go test -race ./internal/server -run 'RegistrationApproval|Registration' -count=1
./unittest.sh
./build.sh
git diff --check
git add internal/server/router.go internal/server/router_test.go
git commit -m "feat: 接入企业微信注册审批命令"
```

---

## 任务 4：连接服务启动流程并增加端到端测试

**文件：**

- 修改：`internal/serverapp/app.go`
- 修改：`internal/serverapp/app_test.go`
- 修改：`internal/integration/admin_test.go`
- 修改：`internal/integration/registration_test.go`

### 步骤 1：扩展集成测试 Harness

保留现有 `newHPAPHarness(t)` 行为，并增加：

```go
func newHPAPHarnessWithRegistrationAdmins(t *testing.T, adminIDs []string) *hpapHarness {
	t.Helper()
	// 使用 json.Marshal 生成测试配置，避免手工拼接管理员 ID。
	// 其余 fake WeCom、TLS、Loki 和临时目录配置与 newHPAPHarness 完全一致。
}

func newHPAPHarness(t *testing.T) *hpapHarness {
	return newHPAPHarnessWithRegistrationAdmins(t, nil)
}
```

测试配置必须使用结构或 `map[string]any` 后 `json.Marshal`，禁止把管理员 ID 直接拼到 JSON 字符串。

### 步骤 2：编写端到端失败测试

增加：

```go
func TestHPAPWeComRegistrationApprovalFlow(t *testing.T)
func TestHPAPWeComRegistrationRejectionFlow(t *testing.T)
func TestHPAPWeComApprovalRejectsSnapshotChangedByWeb(t *testing.T)
```

批准流程：

1. 用户注册首台机器，验证自动签发仍正常。
2. 用户注册第二台机器，验证申请人收到 pending 回复。
3. 验证只有轮转选中的管理员收到主动通知。
4. 管理员在无 Agent 会话时执行 `/ls-reg`，验证返回用户、机器、来源和编号。
5. 管理员执行 `/apr 1`，验证申请人收到 Key，管理员收到逐项结果和重新列表提示。
6. 管理员不重新列表再次执行 `/apr 1`，验证拒绝且没有重复签发。

驳回流程：

1. 创建新的 pending。
2. 下一位管理员收到轮转通知。
3. 该管理员执行 `/ls-reg`、`/rej 1`。
4. 申请人收到驳回通知，管理员收到结果和快照失效提示。

Web 并发流程：

1. 企微管理员执行 `/ls-reg` 保存快照。
2. Web 管理后台批准该申请。
3. 企微管理员执行 `/apr 1`。
4. 验证整批被拒绝为列表变化，没有审批其他条目。

### 步骤 3：确认集成测试失败

```bash
go test ./internal/integration -run 'TestHPAPWeComRegistration(Approval|Rejection)|TestHPAPWeComApprovalRejects' -count=1
```

### 步骤 4：在 serverapp 中实例化协调器

在创建 `machinereg.Service` 后先复用现有交付函数：

```go
keyDelivery := registrationKeyDelivery(weComClient)
rejectionDelivery := registrationRejectionDelivery(weComClient)

registrationApproval, err := server.NewRegistrationApprovalCoordinator(
	server.RegistrationApprovalCoordinatorConfig{
		AdminIDs:          loaded.Config.WeCom.RegistrationAdminIDs,
		Registrations:     registrationService,
		Gateway:           weComClient,
		KeyDelivery:       keyDelivery,
		RejectionDelivery: rejectionDelivery,
		Logger:            logger,
	},
)
if err != nil {
	return fmt.Errorf("创建企业微信注册审批协调器: %w", err)
}
```

传给 Router：

```go
RegistrationApproval: registrationApproval,
```

现有 Web `AdminService` 同样复用 `keyDelivery` 和 `rejectionDelivery`，避免企业微信与 Web 路径产生不同交付逻辑。

启动日志只增加管理员数量：

```go
"registration_admin_count", len(loaded.Config.WeCom.RegistrationAdminIDs)
```

不得记录完整管理员 ID。

### 步骤 5：补充 serverapp 接线测试

在 `internal/serverapp/app_test.go` 增加合法管理员配置可启动、空列表可启动、非法列表在配置层拒绝的覆盖。测试不访问真实企业微信网络。

### 步骤 6：运行集成、race 和回归测试

```bash
go test ./internal/serverapp ./internal/integration -run 'Registration' -count=1
go test -race ./internal/config ./internal/machinereg ./internal/server ./internal/serverapp ./internal/integration -count=1
```

预期：全部通过，fake WeCom 能观察到申请人回复、管理员主动通知和审批结果。

### 步骤 7：提交

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/serverapp/app.go internal/serverapp/app_test.go internal/integration/admin_test.go internal/integration/registration_test.go
git commit -m "test: 覆盖企业微信注册审批流程"
```

---

## 任务 5：更新帮助、示例配置和架构文档

**文件：**

- 修改：`internal/server/default_help.md`
- 修改：`internal/server/help_test.go`
- 修改：`server-config.example.json`
- 修改：`README.md`
- 修改：`docs/BRIDGE_ARCHITECTURE.md`
- 修改：`docs/HANDOFF_CONTEXT.md`

### 步骤 1：先增加帮助内容测试

在 `internal/server/help_test.go` 增加：

```go
func TestDefaultHelpIncludesRegistrationApprovalCommands(t *testing.T) {
	help, err := NewFileHelpProvider("").Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/ls-reg", "/apr 1 2 3", "/rej 1 2 3"} {
		if !strings.Contains(help, command) {
			t.Fatalf("默认帮助缺少 %q", command)
		}
	}
}
```

### 步骤 2：确认测试失败

```bash
go test ./internal/server -run 'TestDefaultHelpIncludesRegistrationApprovalCommands' -count=1
```

### 步骤 3：更新用户帮助

在默认帮助末尾增加简短章节：

```text
[管理员审批]
/ls-reg：列出待审批机器注册并保存当前编号快照
/apr 1 2 3：批准指定编号
/rej 1 2 3：驳回指定编号
审批后编号快照立即失效，请重新执行 /ls-reg。
```

注明只对服务端配置的注册审批管理员有效，不增加 Web 服务部署说明。

### 步骤 4：更新示例配置与 README

`server-config.example.json` 的 `wecom` 节增加：

```json
"registration_admin_ids": [
  "zhangsan",
  "lisi"
]
```

README 增加：

- ID 来源是企业微信回调用户 ID。
- 空列表表示关闭企微审批。
- 新 pending 按数组顺序轮转通知一位管理员。
- 所有配置管理员均可 `/ls-reg`、`/apr`、`/rej`。
- 编号是管理员私有快照，任何审批尝试后都必须重新列表。

### 步骤 5：更新架构和交接文档

`docs/BRIDGE_ARCHITECTURE.md` 明确：

- `RegistrationApprovalCoordinator` 是 Server 内部业务协调层。
- Router 不保存审批编号。
- Web 管理审批不使用企业微信快照。
- 通知失败不影响申请人的 pending 状态。

`docs/HANDOFF_CONTEXT.md` 记录固定产品语义：

- 只通知一位轮转管理员。
- 全部管理员均可审批。
- `/ls-reg` 私有快照只在内存中。
- `/apr`、`/rej` 后强制重新列表。
- 批量单项失败继续执行。

### 步骤 6：验证并提交文档

```bash
go test ./internal/server -run 'TestDefaultHelpIncludesRegistrationApprovalCommands' -count=1
./unittest.sh
./build.sh
git diff --check
git add internal/server/default_help.md internal/server/help_test.go server-config.example.json README.md docs/BRIDGE_ARCHITECTURE.md docs/HANDOFF_CONTEXT.md
git commit -m "docs: 说明企业微信注册审批用法"
```

---

## 任务 6：最终验证和代码审核

**文件：**

- 审核本计划涉及的全部文件

### 步骤 1：运行定向测试

```bash
go test ./internal/config -run 'RegistrationAdminIDs' -count=1
go test ./internal/server -run 'RegistrationApproval|Registration' -count=1
go test ./internal/serverapp ./internal/integration -run 'Registration' -count=1
```

### 步骤 2：运行 race 测试

```bash
go test -race ./internal/config ./internal/machinereg ./internal/server ./internal/serverapp ./internal/integration -count=1
```

### 步骤 3：运行项目统一验证

```bash
./unittest.sh
./build.sh
git diff --check
git status --short
```

预期：测试、race、构建全部通过，工作区没有未提交修改。

### 步骤 4：审核安全和并发边界

逐项确认：

- 普通日志没有完整管理员 ID、Bot Secret 或机器 Key。
- 管理员命令在无 Agent 会话时可用，非管理员不可用。
- `/N`、`#N` 不能定向执行管理员命令。
- 通知失败没有尝试下一管理员，也不改变申请人的 pending 回复。
- `AlreadyPending` 不重复通知管理员。
- 私有快照在每次审批尝试时立即失效。
- 全局列表新增尾部条目不会错误地使旧编号失效。
- 全局列表删除或重排不会把旧编号应用到其他申请。
- 批量操作只使用校验后的稳定 `registration_id`。
- Web 管理审批路径和现有注册存储格式没有被改变。
- Pal、Herdr、HPRP/1、HPAP/1 没有代码或协议变更。

### 步骤 5：输出交付结果

向用户报告：

- 新增配置字段和管理员命令。
- 通知轮转与快照并发语义。
- 单元、集成、race 和构建结果。
- 提交列表。
- 未执行部署、推送或发布，除非用户另行要求。
