# 企业微信注册审批安全加固实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本项目明确禁止使用 subagent-driven development。

**Goal:** 修复注册审批快照未送达仍可生效、批量操作无上限和跨用户互相阻塞的问题，并补齐审批失败与安全边界测试。

**Architecture:** `RegistrationApprovalCoordinator` 将列表生成与快照提交拆成两个阶段，Router 仅在企微完整回复成功后提交快照。审批命令最多接受 10 个编号；企业微信事件循环按接收顺序调用 Router 的非阻塞派发入口，由现有 `UserExecutor` 保证同一用户串行、不同用户并发。消息 ID 在入队前完成幂等预留，队列满通知使用同一用户的有序保留槽。

**Tech Stack:** Go、企业微信 WebSocket fake、`go test`、race detector、现有 HPAP 集成测试框架。

---

### Task 1: 两阶段提交管理员列表快照

**Files:**
- Modify: `internal/server/registration_approval.go`
- Modify: `internal/server/registration_approval_test.go`
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [x] **Step 1: 编写候选快照尚未提交的失败测试**

新增 `TestRegistrationApprovalCoordinatorOnlyUsesCommittedListSnapshot`：调用 `PrepareList` 后直接 `/apr 1` 必须返回 `ErrRegistrationApprovalSnapshotMissing`；调用 `CommitList` 后同一操作成功。

```go
candidate, err := coordinator.PrepareList("admin-a")
if err != nil { t.Fatal(err) }
if _, err := coordinator.Approve(ctx, "admin-a", []int{1}); !errors.Is(err, ErrRegistrationApprovalSnapshotMissing) {
    t.Fatalf("Approve() error = %v", err)
}
if err := coordinator.CommitList("admin-a", candidate); err != nil { t.Fatal(err) }
```

- [x] **Step 2: 运行测试并确认因 API 尚不存在而失败**

Run: `go test ./internal/server -run TestRegistrationApprovalCoordinatorOnlyUsesCommittedListSnapshot -count=1`

- [x] **Step 3: 实现候选快照 API**

用不可由外部构造有效内容的候选值替换 `List` 的直接写入行为：

```go
type RegistrationApprovalList struct {
    content         string
    registrationIDs []string
}

func (coordinator *RegistrationApprovalCoordinator) PrepareList(adminID string) (RegistrationApprovalList, error)
func (coordinator *RegistrationApprovalCoordinator) CommitList(adminID string, list RegistrationApprovalList) error
```

`PrepareList` 只读取并格式化；`CommitList` 复制 ID 后写入管理员快照，同时更新
`RegistrationApprovalHandler` 和 Router 测试 fake 的方法集合。

- [x] **Step 4: 编写 Router 首段和后续分段回复失败测试**

新增：

```text
TestConversationRouterInvalidatesRegistrationSnapshotWhenListReplyFails
TestConversationRouterInvalidatesRegistrationSnapshotWhenListReplyPartiallyFails
```

测试先成功展示旧列表，再让全局列表变化并令第二次 `/ls-reg` 的首段或后续段失败；随后 `/apr 1` 必须得到快照缺失，审批调用次数保持为零。

- [x] **Step 5: 运行测试并确认快照仍错误生效**

Run: `go test ./internal/server -run 'TestConversationRouterInvalidatesRegistrationSnapshotWhenListReply' -count=1`

- [x] **Step 6: Router 完整发送成功后提交候选快照**

`handleRegistrationApproval` 对 `/ls-reg` 单独处理：先 `PrepareList`，再调用 `reply`；成功才 `CommitList`，失败则 `Invalidate` 并记录安全日志。批准和驳回继续使用普通回复路径。

- [x] **Step 7: 运行相关测试**

Run: `go test ./internal/server -run 'RegistrationApproval|ConversationRouter.*Approval' -count=1`

### Task 2: 限制单批审批数量并补齐协调器边界

**Files:**
- Modify: `internal/server/registration_approval.go`
- Modify: `internal/server/registration_approval_test.go`
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [x] **Step 1: 编写超过 10 项的解析失败测试**

在 `TestParseServerActionRegistrationApprovalCommands` 中加入 10 项成功和 11 项失败用例，并验证格式错误仍会调用 `Invalidate`。

- [x] **Step 2: 运行解析测试并确认 11 项当前被接受**

Run: `go test ./internal/server -run TestParseServerActionRegistrationApprovalCommands -count=1`

- [x] **Step 3: 实现固定批量上限**

```go
const maxRegistrationApprovalBatch = 10
```

`parseRegistrationApprovalIndexes` 和 `validRegistrationApprovalIndexes` 都执行上限校验，确保 Router 与协调器直接调用共享同一安全边界。

- [x] **Step 4: 补齐直接调用边界和驳回部分失败测试**

新增：

```text
TestRegistrationApprovalCoordinatorRejectsInvalidSelectionIndexes
TestRegistrationApprovalCoordinatorContinuesRejectBatchAfterItemFailure
TestRegistrationApprovalCoordinatorListEmptyStoresCommittedSnapshot
```

覆盖空切片、0、负数、重复值、越界、11 项，以及驳回中间项失败后继续处理后续稳定 ID。

- [x] **Step 5: 运行协调器测试**

Run: `go test ./internal/server -run TestRegistrationApprovalCoordinator -count=1`

### Task 3: 不同用户并发派发且同一用户保持串行

**Files:**
- Modify: `internal/serverapp/app.go`
- Modify: `internal/serverapp/app_test.go`

- [x] **Step 1: 编写跨用户不阻塞测试**

新增 `TestRunWeComEventLoopDoesNotBlockOtherUsers`：第一个用户的 Router 处理被通道阻塞，第二个用户消息仍须在短时间内开始处理。

- [x] **Step 2: 编写同一用户顺序测试**

新增 `TestRunWeComEventLoopKeepsSameUserSerialized`：同一用户第二条消息在第一条释放前不得进入业务处理。

- [x] **Step 3: 运行测试并确认当前同步事件循环失败**

Run: `go test ./internal/serverapp -run TestRunWeComEventLoop -count=1`

- [x] **Step 4: 实现非阻塞派发**

事件循环按接收顺序调用 `ConversationRouter.Dispatch`；该入口只负责幂等预留和提交用户队列，不等待业务完成。实际同用户串行继续由 `ConversationRouter.UserExecutor` 保证，避免为每条消息创建无界 goroutine。测试使用真实 Router 和可阻塞审批 handler，不复制业务串行逻辑到 fake。

- [x] **Step 5: 运行 race 测试**

Run: `go test -race ./internal/server ./internal/serverapp -run 'UserExecutor|RunWeComEventLoop|RegistrationApproval' -count=1`

### Task 4: 补齐幂等、错误脱敏和配置类型

**Files:**
- Modify: `internal/server/registration_approval_test.go`
- Modify: `internal/server/router_test.go`
- Modify: `internal/config/server.go`
- Modify: `internal/config/relay_test.go`

- [x] **Step 1: 编写破坏性命令幂等测试**

新增 `/apr` 与 `/rej` 表驱动测试，相同 `message_id` 连续交给 Router 两次，领域 handler 只能调用一次且只产生一次回复。

- [x] **Step 2: 编写完整错误映射表测试**

新增 `TestRegistrationApprovalFailureText`，覆盖：

```go
machinereg.ErrRequestNotFound
machinereg.ErrMachineExists
machinereg.ErrDeliveryFailed
&machinereg.OperationError{Kind: machinereg.ErrRollbackFailed}
&machinereg.OperationError{Kind: machinereg.ErrCleanupFailed}
credential.ErrCredentialConflict
errors.New("token hpk_secret /private/path")
```

未知错误只能返回通用文本，不得包含输入错误内容。

- [x] **Step 3: 编写错误 JSON 类型测试**

`TestLoadServerRejectsNonArrayRegistrationAdminIDs` 覆盖 string、object、number、null；缺失字段和空数组继续成功。

- [x] **Step 4: 运行测试并确认 null 当前被接受**

Run: `go test ./internal/config -run RegistrationAdminIDs -count=1`

- [x] **Step 5: 自定义企业微信配置解码**

`ServerWeComConfig.UnmarshalJSON` 使用 `json.RawMessage` 区分字段缺失与 `null`，对存在的字段仅接受 JSON 数组，同时继续拒绝未知字段。

- [x] **Step 6: 运行定向测试**

Run: `go test ./internal/config ./internal/server -run 'RegistrationAdminIDs|RegistrationApproval|Duplicate.*Decision' -count=1`

### Task 5: 补齐真实注册审批集成失败路径

**Files:**
- Modify: `internal/integration/registration_test.go`
- Modify: `internal/integration/admin_test.go`

- [x] **Step 1: 编写审批 Key 交付失败回滚测试**

新增 `TestMachineRegistrationWeComApprovalDeliveryFailureKeepsPending`，断言凭据数量不增加、申请保留、管理员收到安全失败原因，并且下一次审批前必须重新 `/ls-reg`。

- [x] **Step 2: 编写驳回通知失败测试**

新增 `TestMachineRegistrationWeComRejectNotificationFailureStillRemovesPending`，断言申请删除、凭据数量不变、管理员看到“已驳回但通知失败”。

- [x] **Step 3: 强化列表移位测试**

将 Web 并发测试改为两个 pending：Web 批准第一项后，企微旧 `/apr 1` 必须整批拒绝，第二项保持 pending 且没有新凭据。

- [x] **Step 4: 运行集成测试**

Run: `go test ./internal/integration -run MachineRegistration -count=1`

### Task 6: 文档、完整验证和最终审核

**Files:**
- Modify: `README.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `internal/server/default_help.md`
- Modify: `help.md`

- [x] **Step 1: 更新管理员命令说明**

说明每批最多 10 项、`/ls-reg` 只有完整送达才产生可审批快照，并保留全局列表可追加的原规则。

- [x] **Step 2: 运行格式和静态检查**

Run: `gofmt -w internal/config/server.go internal/config/relay_test.go internal/server/registration_approval.go internal/server/registration_approval_test.go internal/server/router.go internal/server/router_test.go internal/serverapp/app.go internal/serverapp/app_test.go internal/integration/registration_test.go`

Run: `go vet ./...`

- [x] **Step 3: 运行完整 race、单测和构建**

Run: `go test -race ./internal/config ./internal/machinereg ./internal/server ./internal/serverapp ./internal/integration -count=1`

Run: `./unittest.sh`

Run: `./build.sh`

- [x] **Step 4: 检查最终差异**

Run: `git diff --check`

Run: `git status --short`

逐项核对设计约束，确认未修改 Pal、Herdr、HPRP/HPAP 和客户端配置。

### Task 7: 修复队列满场景的幂等抢占竞态

**Files:**
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [x] **Step 1: 编写首条已入队、重复副本遇到队列满的回归测试**

新增 `TestConversationRouterDispatchDuplicateCannotClaimAcceptedMessage`，用阻塞任务和容量为 2 的用户队列复现重复副本抢先写入 deduper，断言首条消息最终恰好执行一次。

- [x] **Step 2: 运行测试并确认旧逻辑执行零次**

Run: `go test ./internal/server -run '^TestConversationRouterDispatchDuplicateCannotClaimAcceptedMessage$' -count=1`

- [x] **Step 3: 将非空消息 ID 的幂等预留统一移动到入队前**

Router 在通过单聊和用户身份校验后、提交 `UserExecutor` 前调用 deduper。队列满消息保留幂等键并记录 `queue_full` 审计；后续重复投递直接忽略，不能抢占已接受消息的幂等键。

- [x] **Step 4: 连续运行回归和队列相关测试**

Run: `go test ./internal/server -run '^TestConversationRouterDispatchDuplicateCannotClaimAcceptedMessage$' -count=20`
