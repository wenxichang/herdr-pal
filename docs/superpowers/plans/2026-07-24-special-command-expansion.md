# 特殊命令扩展 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 增加 Agent 选择简写、批量按键、帮助和斜杠 prompt，并在按键完成后自动展示最近 100 行控制台。

**Architecture:** `internal/command` 负责纯解析、规范化和整组校验；`internal/bridge` 负责逐键 occupant 校验、100ms 间隔、逐键审计和一次控制台刷新。Herdr 客户端接口保持单键发送，避免把业务节奏混入协议层。

**Tech Stack:** Go 1.24、标准库 `strings`/`unicode`/`time`、现有 Herdr NDJSON 客户端、Go `testing`。

---

## 文件结构

- 修改 `internal/command/parser.go`：扩展动作模型、命令解析、帮助文本和按键队列规范化。
- 修改 `internal/command/parser_test.go`：覆盖所有新语法、边界和被移除的快捷命令。
- 修改 `internal/bridge/service.go`：分发帮助命令，顺序发送按键并自动刷新控制台。
- 修改 `internal/bridge/service_test.go`：覆盖调用顺序、间隔、审计、部分失败和控制台刷新。
- 修改 `internal/integration/bridge_test.go`：通过公开 Socket 协议验证批量按键和自动读取。
- 修改 `internal/integration/interactive_test.go`：验证交互模式的新按键语法和自动控制台输出。
- 修改 `README.md`、`docs/HANDOFF_CONTEXT.md`：更新当前命令说明。

### Task 1：扩展纯命令解析器

**Files:**
- Modify: `internal/command/parser_test.go`
- Modify: `internal/command/parser.go`

- [ ] **Step 1: 写选择简写、帮助、斜杠 prompt 和按键队列的失败测试**

将 `Action` 断言改为 `reflect.DeepEqual`，并加入以下核心用例：

```go
{name: "select shorthand", input: "/2", want: Action{Kind: KindSelect, Index: 2}},
{name: "help", input: "/help", want: Action{Kind: KindHelp}},
{name: "slash prompt", input: "/slash clear", want: Action{Kind: KindPrompt, Text: "/clear"}},
{name: "key mixed separators", input: "/key down,sp dn space,", want: Action{
    Kind: KindKey, Keys: []string{"down", "space", "down", "space"},
}},
{name: "enter alias", input: "/enter", want: Action{Kind: KindKey, Keys: []string{"enter"}}},
```

非法表加入：`/0`、`/slash`、包含 `enter` 的多键队列、33 个按键、`/keyup`、`/keydn`、
`/space`、`/esc`。另写 `TestHelpTextDocumentsSupportedCommands`，检查 `/help`、`/N`、
`/key`、`dn`、`sp`、`/enter`、`/slash` 均被展示。

- [ ] **Step 2: 运行解析器测试并确认因功能缺失而失败**

Run: `go test ./internal/command -run 'TestParse|TestHelp' -count=1`

Expected: FAIL，原因包括 `KindHelp`、`Action.Keys` 或新语法尚不存在。

- [ ] **Step 3: 实现最小解析逻辑**

将动作改为按键切片并增加帮助动作：

```go
const (
    KindList Kind = iota + 1
    KindSelect
    KindContent
    KindPageUp
    KindPageDown
    KindKey
    KindHelp
    KindPrompt
)

type Action struct {
    Kind  Kind
    Index int
    Keys  []string
    Text  string
}
```

`Parse` 在普通 switch 前识别只有一个字段的 `/<NUM>`，并增加：

```go
case "/help":
    return parseAlias(fields, command, Action{Kind: KindHelp})
case "/enter":
    return parseAlias(fields, command, Action{Kind: KindKey, Keys: []string{"enter"}})
case "/slash":
    return parseSlash(trimmed)
case "/key":
    return parseKeys(strings.TrimSpace(strings.TrimPrefix(trimmed, "/key")))
```

按键解析使用逗号或任意空白分隔，忽略连续及末尾分隔符，展开缩写并执行整组校验：

```go
const maxKeySequence = 32

func parseKeys(raw string) (Action, error) {
    values := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
    if len(values) == 0 || len(values) > maxKeySequence {
        return Action{}, invalidCommand(keyUsage)
    }
    keys := make([]string, len(values))
    for index, value := range values {
        keys[index] = normalizeKey(value)
        if !isAllowedKey(keys[index]) {
            return Action{}, invalidCommand(keyUsage)
        }
    }
    if len(keys) > 1 && slices.Contains(keys, "enter") {
        return Action{}, invalidCommand(keyUsage)
    }
    return Action{Kind: KindKey, Keys: keys}, nil
}
```

实现带文档注释的 `HelpText() string`，并让通用错误提示指向 `/help`。

- [ ] **Step 4: 运行解析器测试并确认通过**

Run: `go test ./internal/command -count=1`

Expected: PASS。

- [ ] **Step 5: 提交解析器变更**

提交前运行 `./build.sh && ./unittest.sh`，然后：

```bash
git add internal/command/parser.go internal/command/parser_test.go
git commit -m "feat: 扩展特殊命令解析"
```

### Task 2：接入帮助、选择简写和斜杠 prompt

**Files:**
- Modify: `internal/bridge/service_test.go`
- Modify: `internal/bridge/service.go`

- [ ] **Step 1: 写服务分发失败测试**

新增测试验证：

```go
service.HandleMessage(context.Background(), incoming("help", "/help"))
// 回复包含 command.HelpText() 的关键命令，且 fakeHerdr.callCount() == 0。

service.HandleMessage(context.Background(), incoming("list", "/ls"))
service.HandleMessage(context.Background(), incoming("short-select", "/1"))
// 当前选择为 pane-1。

service.HandleMessage(context.Background(), incoming("slash", "/slash clear"))
// fake.prompts()[0].text == "/clear"。
```

- [ ] **Step 2: 运行服务定向测试并确认失败**

Run: `go test ./internal/bridge -run 'TestService(Help|SelectShorthand|SlashPrompt)' -count=1`

Expected: FAIL，帮助动作尚未分发，或新动作字段尚未接入。

- [ ] **Step 3: 实现最小服务分发**

更新动作 switch：

```go
case command.KindHelp:
    s.reply(ctx, message.RequestID, command.HelpText())
case command.KindKey:
    s.handleKeys(ctx, message, action.Keys)
```

`/slash` 已被解析为 `KindPrompt`，继续复用 `handlePrompt` 的状态和 occupant 校验，不新增旁路。
`handleList` 末尾提示更新为“使用 /N 或 /sel N 选择目标”。

- [ ] **Step 4: 运行服务定向测试并确认通过**

Run: `go test ./internal/bridge -run 'TestService(Help|SelectShorthand|SlashPrompt)' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交服务分发变更**

提交前运行 `./build.sh && ./unittest.sh`，然后：

```bash
git add internal/bridge/service.go internal/bridge/service_test.go
git commit -m "feat: 接入帮助与命令简写"
```

### Task 3：实现连续按键、逐键审计和自动控制台刷新

**Files:**
- Modify: `internal/bridge/service_test.go`
- Modify: `internal/bridge/service.go`

- [ ] **Step 1: 写连续按键执行的失败测试**

覆盖以下行为：

```go
service.waitKeyInterval = func(_ context.Context, duration time.Duration) error {
    intervals = append(intervals, duration)
    return nil
}
fake.setRead(herdr.ReadResult{PaneID: "pane-1", Text: "console-after-keys"}, nil)
service.HandleMessage(context.Background(), incoming("keys", "/key down,sp dn space,"))
```

断言：

- `SendKey` 顺序为 `down, space, down, space`。
- 每个按键前都调用 `GetAgent`，调用顺序为 `get,key,get,key,get,key,get,key,read`。
- 三次间隔均为 `100*time.Millisecond`，最后一个按键后不等待。
- 四条审计均为 `sent`。
- `ReadRecent(pane-1, 100)` 只调用一次，回复包含 `console-after-keys`。

再增加：第二个按键发送失败时停止第三个按键、记录 `sent/failed` 两条审计、仍读取一次控制台；
第二次 occupant 校验变化时停止剩余按键、记录 `sent/rejected` 且不读取错误 occupant；控制台读取
失败时回复明确区分“按键已发送”和“控制台读取失败”；单独 `/enter` 不等待但会读取一次控制台。

- [ ] **Step 2: 运行连续按键定向测试并确认失败**

Run: `go test ./internal/bridge -run 'TestServiceKey' -count=1`

Expected: FAIL，现有实现只接受一个字符串且不会延时或自动读取。

- [ ] **Step 3: 增加可测试的 100ms 等待器**

在 `Service` 中加入内部依赖，并由 `NewService` 设置真实实现：

```go
const keySequenceInterval = 100 * time.Millisecond

type keyIntervalWaiter func(context.Context, time.Duration) error

func waitKeyInterval(ctx context.Context, duration time.Duration) error {
    timer := time.NewTimer(duration)
    defer timer.Stop()
    select {
    case <-timer.C:
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

测试直接替换 `service.waitKeyInterval`，避免真实睡眠。

- [ ] **Step 4: 实现连续按键与自动 `/con`**

`handleKeys` 先逐项调用 `Guard.ValidateKey`，再获取输入操作租约和选中目标。循环中每个按键都：

```go
sent := 0
failedIndex := -1
failureMessage := ""
targetChanged := false
for index, key := range keys {
    audits, auditErr := prepareKeyAudits(message.UserID, target, key, time.Now().UTC())
    if auditErr != nil {
        failedIndex, failureMessage = index, "按键请求无效，后续未执行。"
        break
    }
    current, getErr := client.GetAgent(ctx, target.PaneID)
    if getErr != nil {
        s.keyAudit.RecordKeyAudit(audits.failed)
        failedIndex, failureMessage = index, unavailableMessage
        break
    }
    if !session.MatchesAgent(target, current) {
        s.keyAudit.RecordKeyAudit(audits.rejected)
        failedIndex, failureMessage, targetChanged = index, targetChangedMessage, true
        break
    }
    if sendErr := client.SendKey(ctx, target.PaneID, key); sendErr != nil {
        s.keyAudit.RecordKeyAudit(audits.failed)
        failedIndex, failureMessage = index, "按键发送失败，后续未执行。"
        break
    }
    s.keyAudit.RecordKeyAudit(audits.sent)
    sent++
    if index+1 < len(keys) {
        if waitErr := s.waitKeyInterval(ctx, keySequenceInterval); waitErr != nil {
            failedIndex, failureMessage = index + 1, "按键序列已取消，后续未执行。"
            break
        }
    }
}
```

仅在还有下一个按键时调用 `s.waitKeyInterval(ctx, keySequenceInterval)`。发送循环完成后读取：

```go
result, readErr := client.ReadRecent(ctx, target.PaneID, panel.PageSize)
```

随后释放操作租约，通过现有 `applyRefresh` 将页码重置为 0，并用一次回复组合发送结果和
`panel.RenderPage`。失败回复必须包含成功数量；读取失败不得改写为“按键发送失败”。

将 `auditUnavailableKey` 改为处理按键切片；预检阶段失败时不调用 Herdr，逐项生成安全审计。
扩展 `fakeHerdr` 以支持逐次 `keyErrors`，并为测试默认提供 pane-1 的空读取结果。

- [ ] **Step 5: 运行 Bridge 全量测试并确认通过**

Run: `go test ./internal/bridge -count=1`

Expected: PASS。

- [ ] **Step 6: 提交连续按键变更**

提交前运行 `./build.sh && ./unittest.sh`，然后：

```bash
git add internal/bridge/service.go internal/bridge/service_test.go
git commit -m "feat: 添加连续按键与自动控制台刷新"
```

### Task 4：更新公开集成测试和当前文档

**Files:**
- Modify: `internal/integration/bridge_test.go`
- Modify: `internal/integration/interactive_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `README.md`
- Modify: `docs/HANDOFF_CONTEXT.md`

- [ ] **Step 1: 写或更新集成测试并确认旧期望失败**

企业微信端到端测试改为发送 `/key enter` 和 `/key down,sp`，断言每个键对应一次
`agent.get`/`agent.send_keys`，并且每条 `/key` 只新增一次 `agent.read(lines=100)`。
交互模式把 `/space` 改为 `/key sp`，断言按键回复直接包含最近控制台内容；调整后续状态通知的
`agent.read` 基线计数。应用层审计测试为自动读取提供合法 `ReadRecent` 结果。

Run: `go test ./internal/integration ./internal/app -count=1`

Expected: 在生产逻辑尚未全部适配时 FAIL，失败来自旧快捷命令或读取次数期望。

- [ ] **Step 2: 更新集成测试夹具和当前用户文档**

README 命令表更新为：

```text
/N              等同 /sel N
/help           显示输入帮助
/key KEYS       逗号或空白分隔；dn/sp 为缩写；最多 32 个；相邻按键间隔 100ms
/enter          等同 /key enter
/slash TEXT     将 /TEXT 作为普通 prompt 发送
```

明确移除 `/keyup`、`/keydn`、`/space`、`/esc`，多键不得包含 `enter`，按键命令执行后自动
读取最近 100 行并重置分页。同步更新 `docs/HANDOFF_CONTEXT.md` 的当前命令和模块状态。

- [ ] **Step 3: 运行集成和应用测试并确认通过**

Run: `go test ./internal/integration ./internal/app -count=1`

Expected: PASS。

- [ ] **Step 4: 运行格式化、构建、全量单测和差异检查**

Run:

```bash
gofmt -w internal/command/parser.go internal/command/parser_test.go internal/bridge/service.go internal/bridge/service_test.go internal/integration/bridge_test.go internal/integration/interactive_test.go internal/app/app_test.go
git diff --check
./build.sh
./unittest.sh
```

Expected: 所有命令退出码为 0，Go 测试无失败。

- [ ] **Step 5: 提交集成测试与文档**

```bash
git add README.md docs/HANDOFF_CONTEXT.md internal/integration/bridge_test.go internal/integration/interactive_test.go internal/app/app_test.go
git commit -m "docs: 更新特殊命令使用说明"
```

## 最终验收

- [ ] `/<NUM>` 与 `/sel <NUM>` 等价。
- [ ] `/key` 支持逗号、空白、混合分隔、`dn`/`sp` 缩写和最多 32 个按键。
- [ ] 多键包含 `enter` 时整组拒绝，单独 `/enter` 正常。
- [ ] `/keyup`、`/keydn`、`/space`、`/esc` 已移除。
- [ ] 相邻按键发送至少间隔 100ms，每个按键前重新校验 occupant。
- [ ] 按键命令结束后只执行一次等价 `/con` 的最近 100 行读取。
- [ ] `/help` 和 `/slash` 可用，未知斜杠命令仍不会降级为 prompt。
- [ ] 构建、全量单测、race 测试和差异检查全部通过。
