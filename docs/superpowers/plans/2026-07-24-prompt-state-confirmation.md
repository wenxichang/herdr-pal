# Prompt 状态确认与 Enter 恢复实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 普通文本只在 Agent 可接收输入时发送，并在观察到状态变化后才报告成功；首次 5 秒无变化时安全补发一次 Enter，再等待 5 秒。

**Architecture:** `internal/herdr` 负责 protocol 17 的 `state_change_seq`、原子 prompt wait 和按序列轮询；`internal/bridge.Service` 负责状态门禁、occupant 复核、一次性恢复与审计。fake Herdr 只模拟公开协议行为，事件订阅仍负责异步通知，不参与同步等待。

**Tech Stack:** Go 1.26、Unix NDJSON Socket、Herdr protocol 17、标准库 `context`/`errors`/`time`、现有 fake Herdr 与 Go testing。

---

## 文件结构

- `internal/herdr/types.go`：公开 `StateChangeSeq`，严格解析 protocol 17 AgentInfo。
- `internal/herdr/errors.go`：定义本地等待超时错误。
- `internal/herdr/client.go`：发送带 wait 的 prompt，并按序列轮询 `agent.get`。
- `internal/herdr/client_test.go`：协议参数、响应和等待行为单测。
- `internal/bridge/service.go`：状态门禁、首次确认、Enter 恢复、回复和审计。
- `internal/bridge/service_test.go`：完整发送状态机和并发屏障单测。
- `internal/bridge/supervisor_test.go`、`internal/app/app_test.go`：补齐接口测试替身。
- `internal/testkit/herdr_server.go`：公开协议 fake 支持 prompt wait 和状态序列。
- `internal/testkit/herdr_server_test.go`：验证 fake 的公开协议行为。
- `internal/integration/interactive_test.go`、`internal/integration/bridge_test.go`：端到端确认成功路径和拒绝路径。
- `README.md`、`docs/HANDOFF_CONTEXT.md`、`docs/HERDR_API_AUDIT.md`、`docs/BRIDGE_ARCHITECTURE.md`：更新产品和协议语义。

### Task 1: 严格解析状态变化序列

**Files:**
- Modify: `internal/herdr/types.go`
- Test: `internal/herdr/client_test.go`

- [ ] **Step 1: 编写失败测试**

在 `validAgentInfo` 返回的完整 Agent 中加入 `state_change_seq`，并新增断言：

```go
func TestClientGetAgentDecodesStateChangeSequence(t *testing.T) {
    agent := validAgentInfo(t)
    agent["state_change_seq"] = float64(27)
    client := newBusinessTestClient(t,
        `{"type":"agent_info","agent":`+mustJSON(t, agent)+`}`,
        nil,
    )

    got, err := client.GetAgent(context.Background(), "p1")
    if err != nil {
        t.Fatalf("GetAgent() 返回错误：%v", err)
    }
    if got.StateChangeSeq != 27 {
        t.Fatalf("StateChangeSeq = %d, want 27", got.StateChangeSeq)
    }
}
```

在现有无效 AgentInfo 表格中加入“缺 `state_change_seq`”用例。

- [ ] **Step 2: 验证 RED**

Run:

```sh
go test ./internal/herdr -run 'TestClientGetAgentDecodesStateChangeSequence|TestClientGetAgentAndPromptRejectInvalidAgentInfo' -count=1
```

Expected: FAIL，`AgentInfo` 尚无 `StateChangeSeq` 或缺字段尚未被拒绝。

- [ ] **Step 3: 实现最小解析**

在公开类型和 wire 类型中加入：

```go
// StateChangeSeq 是 Herdr 为 Agent 生命周期变化维护的单调序列。
StateChangeSeq uint64 `json:"state_change_seq"`
```

```go
StateChangeSeq *uint64 `json:"state_change_seq"`
```

`agentInfoFromWire` 将该字段列为必填，并复制到 `AgentInfo`。不把 `revision` 当作替代品。

- [ ] **Step 4: 验证 GREEN**

Run:

```sh
go test ./internal/herdr -run 'TestClientGetAgentDecodesStateChangeSequence|TestClientGetAgentAndPromptRejectInvalidAgentInfo' -count=1
```

Expected: PASS。

### Task 2: 增加原子 prompt wait 和序列等待

**Files:**
- Modify: `internal/herdr/errors.go`
- Modify: `internal/herdr/client.go`
- Test: `internal/herdr/client_test.go`

- [ ] **Step 1: 编写 `PromptUntilStateChange` 失败测试**

新增测试要求请求精确包含全部状态且不包含 `timeout_ms`：

```go
func TestClientPromptUntilStateChangeSendsWaitAndDecodesAgent(t *testing.T) {
    agent := validAgentInfo(t)
    agent["agent_status"] = "working"
    agent["state_change_seq"] = float64(8)
    client := newBusinessTestClient(t,
        `{"type":"agent_prompted","agent":`+mustJSON(t, agent)+`}`,
        businessRequestCheck("agent.prompt", map[string]any{
            "target": "p1",
            "text":   "运行测试",
            "wait": map[string]any{
                "until": []any{"idle", "working", "blocked", "done", "unknown"},
            },
        }),
    )

    got, err := client.PromptUntilStateChange(context.Background(), "p1", "运行测试")
    if err != nil || got.AgentStatus != AgentStatusWorking || got.StateChangeSeq != 8 {
        t.Fatalf("PromptUntilStateChange() = %+v, %v", got, err)
    }
}
```

再验证 `agent_prompt_stalled` 可通过 `errors.As` 取得 `*APIError` 和稳定 Code。

- [ ] **Step 2: 验证 prompt RED**

Run:

```sh
go test ./internal/herdr -run 'TestClientPromptUntilStateChange' -count=1
```

Expected: FAIL，方法和 wait 参数尚不存在。

- [ ] **Step 3: 实现 prompt wait**

增加 wire 参数：

```go
type agentPromptWaitOptions struct {
    Until []AgentStatus `json:"until"`
}

type agentPromptParams struct {
    Target string                  `json:"target"`
    Text   string                  `json:"text"`
    Wait   *agentPromptWaitOptions `json:"wait,omitempty"`
}
```

增加稳定顺序的全部状态切片，并实现：

```go
func (c *Client) PromptUntilStateChange(ctx context.Context, target, text string) (AgentInfo, error)
```

成功 result type 必须为 `agent_prompted`；返回 payload 中的完整 AgentInfo 用于确认状态变化。

- [ ] **Step 4: 编写 `WaitForStateChange` 失败测试**

测试至少覆盖：

```go
func TestClientWaitForStateChangeReturnsImmediatelyForNewSequence(t *testing.T)
func TestClientWaitForStateChangeTimesOutWithoutNewSequence(t *testing.T)
func TestClientWaitForStateChangePropagatesGetAgentError(t *testing.T)
```

期望 API：

```go
WaitForStateChange(ctx context.Context, target string, baseline uint64, timeout time.Duration) (AgentInfo, error)
```

超时通过 `errors.Is(err, ErrAgentStateChangeTimeout)` 判断。

- [ ] **Step 5: 验证等待 RED**

Run:

```sh
go test ./internal/herdr -run 'TestClientWaitForStateChange' -count=1
```

Expected: FAIL，等待方法和超时错误尚不存在。

- [ ] **Step 6: 实现最小轮询**

在 `errors.go` 增加：

```go
// ErrAgentStateChangeTimeout 表示指定窗口内未观察到 Agent 生命周期变化。
var ErrAgentStateChangeTimeout = errors.New("等待 Agent 状态变化超时")
```

实现规则：

```go
const agentStatePollInterval = 100 * time.Millisecond
```

- 拒绝空 target、零 timeout 或负 timeout。
- `context.WithTimeout(ctx, timeout)` 限定整个等待窗口。
- 立即执行第一次 `GetAgent`，只要 `StateChangeSeq != baseline` 就返回。
- 未变化时等待最多 100 毫秒再查询。
- 上游 context 取消返回其错误；内部 deadline 返回 `ErrAgentStateChangeTimeout`。
- 任意 `GetAgent` 业务、协议或连接错误立即返回。

- [ ] **Step 7: 验证 herdr 包并提交**

Run:

```sh
gofmt -w internal/herdr/types.go internal/herdr/errors.go internal/herdr/client.go internal/herdr/client_test.go
go test ./internal/herdr -count=1
./unittest.sh
./build.sh
git diff --check
```

Expected: 全部 PASS。

Commit:

```sh
git add internal/herdr/types.go internal/herdr/errors.go internal/herdr/client.go internal/herdr/client_test.go
git commit -m "feat: 增加 prompt 状态变化等待"
```

### Task 3: 为 Service 增加发送状态门禁

**Files:**
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/service_test.go`
- Modify: `internal/bridge/supervisor_test.go`
- Modify: `internal/app/app_test.go`

- [ ] **Step 1: 修改测试替身的期望接口并观察编译失败**

`HerdrAPI` 的普通文本能力改为：

```go
PromptUntilStateChange(context.Context, string, string) (herdr.AgentInfo, error)
WaitForStateChange(context.Context, string, uint64, time.Duration) (herdr.AgentInfo, error)
```

测试 fake 保存 prompt 返回 AgentInfo、等待返回 AgentInfo 和对应错误。先只修改测试，使生产接口仍旧不匹配。

- [ ] **Step 2: 验证接口 RED**

Run:

```sh
go test ./internal/bridge ./internal/app -run 'TestServicePrompt|TestRun' -count=1
```

Expected: FAIL，`HerdrAPI` 尚未声明新方法或 Service 仍调用旧 `Prompt`。

- [ ] **Step 3: 编写状态门禁测试**

新增表格测试：

```go
func TestServicePromptOnlyAcceptsIdleAndDone(t *testing.T) {
    tests := []struct {
        status herdr.AgentStatus
        sent   bool
    }{
        {herdr.AgentStatusIdle, true},
        {herdr.AgentStatusDone, true},
        {herdr.AgentStatusWorking, false},
        {herdr.AgentStatusBlocked, false},
        {herdr.AgentStatusUnknown, false},
    }
    // 每个用例选择目标、设置实时 Agent 状态并发送普通文本。
    // sent=false 时断言没有 prompt/key/wait 调用，回复包含稳定状态值。
}
```

更新原“保留原始文本”测试，使 prompt fake 返回同一 occupant、`working`、更大的
`StateChangeSeq`，并断言回复包含 `working`。

- [ ] **Step 4: 验证门禁 RED**

Run:

```sh
go test ./internal/bridge -run 'TestServicePromptOnlyAcceptsIdleAndDone|TestServicePromptPreservesBytesAndChecksLiveAgent' -count=1
```

Expected: FAIL，旧 Service 允许全部状态且不检查返回状态变化。

- [ ] **Step 5: 实现状态门禁和首次成功路径**

增加纯函数：

```go
func canAcceptPrompt(status herdr.AgentStatus) bool {
    return status == herdr.AgentStatusIdle || status == herdr.AgentStatusDone
}

func promptStateChanged(before, after herdr.AgentInfo) bool {
    return before.AgentStatus != after.AgentStatus || before.StateChangeSeq != after.StateChangeSeq
}
```

`handlePrompt` 在 occupant 校验后拒绝非 ready 状态；调用
`PromptUntilStateChange`，校验返回 occupant 和状态确实变化，然后回复：

```go
fmt.Sprintf("已发送，Agent 状态已变为 %s。", safeLabel(string(agent.AgentStatus)))
```

首次非 stalled 错误保持“发送失败，请稍后重试”，且不调用 Enter。

- [ ] **Step 6: 验证首次路径 GREEN**

Run:

```sh
gofmt -w internal/bridge/service.go internal/bridge/service_test.go internal/bridge/supervisor_test.go internal/app/app_test.go
go test ./internal/bridge ./internal/app -run 'TestServicePrompt|TestServiceSelectWaitsForInFlightPrompt|TestServiceReplaceSnapshotPreservingStatusUsesBarrier' -count=1
```

Expected: PASS。

### Task 4: 实现一次性 Enter 恢复和审计

**Files:**
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/service_test.go`

- [ ] **Step 1: 编写恢复成功失败测试**

新增独立测试：

```go
func TestServicePromptStallSendsOneEnterAndWaitsForChange(t *testing.T)
func TestServicePromptStallSkipsEnterWhenRecheckAlreadyChanged(t *testing.T)
func TestServicePromptStallTimesOutAfterOneEnter(t *testing.T)
func TestServicePromptStallDoesNotRetryOtherErrors(t *testing.T)
func TestServicePromptStallInvalidatesReplacedOccupant(t *testing.T)
func TestServicePromptStallStopsWhenContextIsCanceled(t *testing.T)
```

恢复成功的调用顺序必须为：

```text
get,prompt,get,key,wait
```

并断言只发送一个 `enter`。

- [ ] **Step 2: 编写审计失败测试**

覆盖：

- Enter 成功发送记录 `sent`。
- 补发前 occupant 或状态不允许记录 `rejected`。
- 补发前查询失败或 SendKey 失败记录 `failed`。
- 审计不包含 prompt；现有 `KeyAudit` 结构只含安全字段。

- [ ] **Step 3: 验证恢复 RED**

Run:

```sh
go test ./internal/bridge -run 'TestServicePromptStall' -count=1
```

Expected: FAIL，Service 尚未识别 `agent_prompt_stalled` 或补发 Enter。

- [ ] **Step 4: 实现恢复状态机**

增加：

```go
const promptRecoveryTimeout = 5 * time.Second

func isPromptStalled(err error) bool {
    var apiErr *herdr.APIError
    return errors.As(err, &apiErr) && apiErr.Code == "agent_prompt_stalled"
}
```

恢复流程严格执行：

1. 为 `enter` 准备现有三态审计。
2. 再次 `GetAgent` 并校验 occupant。
3. 若序列或状态已变化，记录 `rejected`，不发送 Enter，报告当前状态。
4. 若状态不再是 `idle`/`done`，记录 `rejected`，不发送 Enter。
5. `SendKey(..., "enter")`：失败记录 `failed`；成功记录 `sent`。
6. `WaitForStateChange(..., initial.StateChangeSeq, 5*time.Second)`。
7. 再次校验 occupant 和变化；成功才报告状态。
8. `ErrAgentStateChangeTimeout` 返回“发送未生效，请检查 Agent 界面。”。

所有分支在释放 operation 租约后回复，避免持锁调用 IM。

- [ ] **Step 5: 验证恢复 GREEN 和并发回归**

Run:

```sh
gofmt -w internal/bridge/service.go internal/bridge/service_test.go
go test ./internal/bridge -count=1
go test -race ./internal/bridge -count=1
```

Expected: PASS，无 race。

- [ ] **Step 6: 提交 Service 状态机**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
```

Expected: 全部 PASS。

Commit:

```sh
git add internal/bridge/service.go internal/bridge/service_test.go internal/bridge/supervisor_test.go internal/app/app_test.go
git commit -m "fix: 确认 prompt 状态变化并恢复 Enter"
```

### Task 5: 扩展公开协议 fake 和端到端测试

**Files:**
- Modify: `internal/testkit/herdr_server.go`
- Modify: `internal/testkit/herdr_server_test.go`
- Modify: `internal/integration/interactive_test.go`
- Modify: `internal/integration/bridge_test.go`

- [ ] **Step 1: 编写 fake 协议失败测试**

验证 `agent.prompt` 带 wait 时：

- 参数 `until` 必须是完整状态集合。
- 成功返回 `agent_prompted`，并将 fake Agent 从 `idle` 切换为 `working`、递增
  `state_change_seq`。
- 无 wait 的既有路径仍返回 `agent_prompted`。

fake 的 `agentWire` 始终返回完整 `state_change_seq`，避免不完整 mock。

- [ ] **Step 2: 验证 fake RED**

Run:

```sh
go test ./internal/testkit -run 'TestHerdrServer.*Prompt' -count=1
```

Expected: FAIL，fake 尚不理解 wait，也不返回序列。

- [ ] **Step 3: 实现最小 fake 行为**

`agent.prompt` 解析：

```go
Wait *struct {
    Until []herdr.AgentStatus `json:"until"`
} `json:"wait"`
```

带 wait 的默认行为在锁内更新匹配 Agent 和对应 Pane：

```go
agent.AgentStatus = herdr.AgentStatusWorking
agent.StateChangeSeq++
```

随后返回 `{"type":"agent_prompted","agent":...}`。不模拟 PTY、私有 AppState 或真实事件
检测。

- [ ] **Step 4: 更新端到端失败测试**

交互和企业微信普通 prompt 场景从 `idle` 启动，并要求：

- 请求包含 wait.until。
- 回复包含 `已发送` 和 `working`。
- 没有额外 `agent.send_keys`。

增加一个 `working` 初始状态拒绝普通文本的端到端子测试，断言没有 `agent.prompt`。

- [ ] **Step 5: 验证集成 GREEN**

Run:

```sh
gofmt -w internal/testkit/herdr_server.go internal/testkit/herdr_server_test.go internal/integration/interactive_test.go internal/integration/bridge_test.go
go test ./internal/testkit ./internal/integration -count=1
```

Expected: PASS。

- [ ] **Step 6: 提交 fake 与集成测试**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
```

Expected: 全部 PASS。

Commit:

```sh
git add internal/testkit/herdr_server.go internal/testkit/herdr_server_test.go internal/integration/interactive_test.go internal/integration/bridge_test.go
git commit -m "test: 覆盖 prompt 状态确认与恢复"
```

### Task 6: 更新文档并执行最终验证

**Files:**
- Modify: `README.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/HERDR_API_AUDIT.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`

- [ ] **Step 1: 更新产品语义**

四份文档统一写明：

- 普通文本只允许 `idle`、`done`。
- `agent.prompt` 使用原子 wait，最多等待 5 秒状态变化。
- stalled 后复核 occupant 和状态，只补发一次 Enter。
- Enter 后再等待最多 5 秒，仍无变化则报告未生效。
- 成功回复包含确认后的协议状态。
- 自动 Enter 不在 `blocked` 执行，并进入按键审计。

- [ ] **Step 2: 检查文档一致性**

Run:

```sh
rg -n '已发送|agent.prompt|普通文本|blocked|Enter|状态变化' README.md docs/HANDOFF_CONTEXT.md docs/HERDR_API_AUDIT.md docs/BRIDGE_ARCHITECTURE.md
git diff --check
```

Expected: 不再把 `agent_prompted` 入队响应描述成最终发送成功。

- [ ] **Step 3: 执行最终验证**

Run:

```sh
gofmt -w internal/herdr/types.go internal/herdr/errors.go internal/herdr/client.go internal/herdr/client_test.go internal/bridge/service.go internal/bridge/service_test.go internal/bridge/supervisor_test.go internal/app/app_test.go internal/testkit/herdr_server.go internal/testkit/herdr_server_test.go internal/integration/interactive_test.go internal/integration/bridge_test.go
go vet ./...
go test -count=1 ./...
go test -race ./...
./unittest.sh
./build.sh
git diff --check
```

Expected: 全部退出码为 0，输出无失败、panic 或 race。

- [ ] **Step 4: 可选真实 Herdr 手工验证**

只在用户明确允许实时输入且目标 pane 重新确认后执行。使用 `idle` 或 `done` 的测试 Agent，
验证正常 prompt 进入 `working`；不得原样运行带占位符的实时测试命令。

- [ ] **Step 5: 提交文档并检查工作区**

Commit:

```sh
git add README.md docs/HANDOFF_CONTEXT.md docs/HERDR_API_AUDIT.md docs/BRIDGE_ARCHITECTURE.md
git commit -m "docs: 更新 prompt 状态确认语义"
git status --short
git log -5 --oneline
```

Expected: 工作区干净，最近提交包含协议能力、Service 修复、集成测试和文档更新。
