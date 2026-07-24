# Herdr Pal 交互模式与真实 Herdr 联调 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 Herdr Agent 公共 API 错用 terminal ID 的问题，新增无需企业微信凭据的 `herdr-pal -i` 本地交互模式，并用当前 protocol 17 Herdr 完成只读与显式授权 prompt 联调。

**Architecture:** 所有 `agent.get`、`agent.read`、`agent.prompt`、`agent.send_keys` 统一以 pane ID 路由，terminal ID 仅保留用于 occupant 身份校验。新增 `ConsoleAdapter` 实现现有 IM 运行时接口，把 stdin 行转换为本地单聊消息，把回复和通知串行写到 stdout；应用层抽取通用 IM 运行时装配，企业微信模式与交互模式共享 `Service`、`Notifier`、`Supervisor`、策略、幂等和审计。交互模式通过公共 Herdr CLI 或显式配置解析 Socket，并以最终 Socket 摘要获取独立进程锁。

**Tech Stack:** Go 1.26、标准库 `bufio`/`context`/`io`/`log/slog`/Unix Socket、现有 Herdr NDJSON Client、现有 fake Herdr 与 Go testing。

---

## 实施基线与约束

- 设计依据：`docs/superpowers/specs/2026-07-24-interactive-mode-live-herdr-design.md`。
- 真实 Herdr 基线：`/Users/wxc/Code/herdr/target/debug/herdr` 0.7.5，protocol 17。
- 当前已观察到的真实 pane ID 是 `w1:p1`；执行实时输入测试前仍须通过公共 `session.snapshot` 再确认，不能依赖历史值。
- 不修改 `/Users/wxc/Code/herdr`，不读取私有 TUI socket 或内部状态。
- 不新增 `-socket` 参数，不扫描 `~/.config/herdr-dev`；debug Herdr 通过 PATH 前缀或配置中的公共 Socket override 使用。
- 真实按键永远不进入自动测试；真实 prompt 只在双重环境变量门禁和明确 pane ID 下执行一次。
- `build.sh` 与 `unittest.sh` 已存在，本次继续复用；每个提交前都必须运行二者。
- 本计划中的测试均按 TDD 顺序：先写或收紧测试，确认预期失败，再写最小实现。

## 目标文件结构

```text
.
├── cmd/herdr-pal/main.go
├── cmd/herdr-pal/main_test.go
├── internal/app/app.go
├── internal/app/app_test.go
├── internal/bridge/service.go
├── internal/bridge/service_test.go
├── internal/bridge/notifier.go
├── internal/bridge/notifier_test.go
├── internal/config/config.go
├── internal/config/config_test.go
├── internal/interactive/adapter.go
├── internal/interactive/adapter_test.go
├── internal/integration/bridge_test.go
├── internal/integration/interactive_test.go
├── internal/integration/real_herdr_test.go
├── internal/testkit/herdr_server.go
├── internal/testkit/herdr_server_test.go
├── README.md
├── docs/BRIDGE_ARCHITECTURE.md
├── docs/HANDOFF_CONTEXT.md
└── docs/HERDR_API_AUDIT.md
```

## Task 1：收紧 fake Herdr 并把全部 Agent API target 改为 pane ID

**Files:**

- Modify: `internal/testkit/herdr_server.go`
- Modify: `internal/testkit/herdr_server_test.go`
- Modify: `internal/bridge/service_test.go`
- Modify: `internal/bridge/notifier_test.go`
- Modify: `internal/integration/bridge_test.go`
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/notifier.go`

- [ ] **Step 1: 先让 fake Herdr 拒绝 terminal ID**

把 `findAgent` 改成只接受 pane ID，或非空且在当前快照中唯一的 `AgentInfo.Name`。pane ID 优先；同名 Agent 多于一个时必须返回未找到，避免 fake 接受真实服务不会可靠接受的模糊名称。

在 `internal/testkit/herdr_server_test.go` 新增表驱动测试，分别向四个公共 Agent API 发送 `terminal-1`：

```go
func TestHerdrServerRejectsTerminalIDAsAgentTarget(t *testing.T) {
    methods := []struct {
        name   string
        method string
        params map[string]any
    }{
        {name: "get", method: "agent.get", params: map[string]any{"target": "terminal-1"}},
        {name: "read", method: "agent.read", params: map[string]any{
            "target": "terminal-1", "source": "recent_unwrapped", "lines": 1,
            "format": "text", "strip_ansi": true,
        }},
        {name: "prompt", method: "agent.prompt", params: map[string]any{"target": "terminal-1", "text": "test"}},
        {name: "send keys", method: "agent.send_keys", params: map[string]any{"target": "terminal-1", "keys": []string{"enter"}}},
    }
    // 每个请求都应返回 agent_not_found；同样参数换成 pane-1 时应成功。
}
```

再增加一个唯一 `Name` 成功、重复 `Name` 失败的测试，确保 fake 与最新 Herdr 语义一致。

- [ ] **Step 2: 将现有 Bridge 测试的预期 target 改为 pane ID**

至少覆盖以下调用：

- `Service` 的 occupant `GetAgent`、prompt、按键、`/con`、`/pageup`。
- `Notifier` 的 blocked/done 前后两次 `GetAgent` 和一次 `ReadRecent`。
- fake 端到端的 `agent.prompt`、`agent.send_keys`、`agent.read`。

例如把现有 prompt 断言改成：

```go
if got := fake.prompts(); len(got) != 1 || got[0].text != prompt || got[0].target != "pane-1" {
    t.Fatalf("prompt calls = %#v", got)
}
if got := fake.gets(); len(got) != 1 || got[0] != "pane-1" {
    t.Fatalf("GetAgent targets = %#v, want pane-1", got)
}
```

给 `Notifier` 的 getter/reader 测试夹具增加线程安全的 target 记录，并断言顺序为：

```text
get(pane-1) -> read(pane-1, 100) -> get(pane-1)
```

- [ ] **Step 3: 运行回归测试并确认失败**

Run:

```sh
go test ./internal/testkit ./internal/bridge ./internal/integration
```

Expected: FAIL。失败信息应显示生产代码仍把 `terminal-1` 传给 fake，或调用 target 与新断言不一致；不得通过放宽 fake 修复测试。

- [ ] **Step 4: 修改 Service 使用 `Target.PaneID`**

在 `internal/bridge/service.go` 中把以下六处调用参数从 `target.TerminalID` 改为 `target.PaneID`：

```go
current, err := client.GetAgent(ctx, target.PaneID)
err = client.Prompt(ctx, target.PaneID, text)

current, err := client.GetAgent(ctx, target.PaneID)
err = client.SendKey(ctx, target.PaneID, key)

result, err := client.ReadRecent(ctx, target.PaneID, panel.PageSize)
result, err := client.ReadRecent(ctx, target.PaneID, linesToRead)
```

不要删除 `Target.TerminalID`。`session.MatchesAgent` 必须继续比较实时返回的 pane ID、terminal ID、Agent 和 occupant key，保证 pane 内 Agent 被替换时旧选择失效。

- [ ] **Step 5: 修改 Notifier 使用 `Target.PaneID`**

在 `internal/bridge/notifier.go` 中把 blocked/done/idle 快照的三次 Agent API target 统一改为 pane ID：

```go
before, err := n.get(ctx, transition.Target.PaneID)
result, readErr := n.read(ctx, transition.Target.PaneID, panel.PageSize)
after, getErr := n.get(ctx, transition.Target.PaneID)
```

保留前后 occupant 校验、`result.PaneID` 校验和最近 100 行策略。

- [ ] **Step 6: 运行目标测试并确认转绿**

Run:

```sh
go test ./internal/testkit ./internal/bridge ./internal/integration
```

Expected: PASS，且 fake 记录到的四类 Agent API target 都是 `pane-1`。

- [ ] **Step 7: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/testkit internal/bridge internal/integration/bridge_test.go
git commit -m "fix: 使用面板标识调用 Herdr Agent API"
```

Expected: 构建、单测、race test 和提交全部成功。

## Task 2：增加不读取企业微信 Secret 的交互配置加载

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: 先写 `LoadInteractive` 失败测试**

新增测试覆盖：

1. 空路径返回空 Herdr override 和 `log.level=info`。
2. 只有 `herdr`、`log` 的 JSON 可以加载。
3. `wecom` 缺失或字段为空不会报错。
4. 未知字段、多个 JSON 值和尾随内容仍被拒绝。
5. 普通 `Load` 仍要求 bot ID、allowed user 和环境变量 Secret。

建议对外接口：

```go
// LoadInteractive 加载交互模式配置。空 path 使用公共 CLI 自动发现 Herdr。
// 该函数不读取企业微信 Secret，也不校验企业微信字段。
func LoadInteractive(path string) (Config, error)
```

测试必须通过签名本身保证不存在 `getenv` 回调，从而不能误读 `HERDR_PAL_WECOM_SECRET`。

- [ ] **Step 2: 运行测试并确认失败**

Run:

```sh
go test ./internal/config -run 'TestLoadInteractive|TestLoad'
```

Expected: FAIL，提示 `LoadInteractive` 未定义。

- [ ] **Step 3: 抽取严格 JSON 解析并实现两种校验路径**

把文件读取、`DisallowUnknownFields` 和尾随内容检查抽成私有函数：

```go
func loadFile(path string) (Config, error)
```

保持 `Load` 的行为和错误边界不变：

```go
func Load(path string, getenv func(string) string) (Config, error) {
    loaded, err := loadFile(path)
    if err != nil {
        return Config{}, err
    }
    loaded.WeCom.Secret = getenv(SecretEnvName)
    // 原有三个必填值校验保持不变。
    return loaded, nil
}
```

交互配置实现：

```go
func LoadInteractive(path string) (Config, error) {
    if strings.TrimSpace(path) == "" {
        return Config{Log: LogConfig{Level: "info"}}, nil
    }
    loaded, err := loadFile(path)
    if err != nil {
        return Config{}, err
    }
    if strings.TrimSpace(loaded.Log.Level) == "" {
        loaded.Log.Level = "info"
    }
    loaded.WeCom.Secret = ""
    return loaded, nil
}
```

`log.level` 的允许值继续由应用层 `newLogger` 统一校验，避免配置包复制规则。

- [ ] **Step 4: 验证普通模式无回归**

Run:

```sh
go test ./internal/config
```

Expected: PASS；原有 Secret、未知字段和尾随 JSON 测试继续通过。

- [ ] **Step 5: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/config
git commit -m "feat: 添加交互模式配置加载"
```

## Task 3：实现 ConsoleAdapter 的输入、输出和错误传播

**Files:**

- Create: `internal/interactive/adapter.go`
- Create: `internal/interactive/adapter_test.go`

- [ ] **Step 1: 先写 ConsoleAdapter 行输入测试**

测试使用 `io.Pipe`，启动 `Adapter.Run` 后写入：

```text
/ls
hello agent

```

断言三行各产生一条 `wecom.IncomingText`，字段满足：

```go
UserID:   interactive.UserID
ChatType: "single"
BotID:    interactive.BotID
```

三条消息的 `RequestID` 和 `MessageID` 必须非空、单调递增且互不重复，`Content` 保留行内字节但不包含换行结束符。

- [ ] **Step 2: 先写输出格式和并发完整性测试**

覆盖：

- 启动输出包含标题、退出提示和 `herdr-pal> `。
- `RespondMarkdown` 输出 `[回复]`。
- `SendMarkdown` 输出 `[通知]`。
- 32 个 goroutine 并发写不同标记时，每个标记只出现一次，标签、正文和提示符作为完整块写入，不发生字节交错。

固定输出格式：

```text
Herdr Pal 交互模式
输入 /ls 查看 Agent，按 Ctrl+C 或 Ctrl+D 退出。

herdr-pal> 

[回复]
...

herdr-pal> 
```

通知使用相同块结构，只把标签改为 `[通知]`。

- [ ] **Step 3: 先写退出和失败测试**

覆盖：

- stdin EOF 使 `Run` 返回 `ErrInputClosed`。
- context 取消时，即使底层 reader 仍阻塞，`Run` 也在测试上限内返回 `context.Canceled`。
- 初始 banner 写失败时 `Run` 返回错误。
- `RespondMarkdown` 或 `SendMarkdown` 写失败时，写方法返回错误，同时运行中的 `Run` 通过内部 fatal channel 返回同一错误。
- `bufio.Scanner` 读错误必须返回安全包装错误，不包含已读完整消息。

- [ ] **Step 4: 运行测试并确认失败**

Run:

```sh
go test ./internal/interactive
```

Expected: FAIL，提示包或 Adapter 尚不存在。

- [ ] **Step 5: 实现最小 ConsoleAdapter**

公开边界：

```go
// Package interactive 提供把本机 stdin/stdout 适配为单用户 IM 会话的运行时。
package interactive

const (
    // UserID 是交互模式唯一允许的本地用户标识。
    UserID = "interactive-local"
    // BotID 是交互模式写入入站消息的本地适配器标识。
    BotID  = "interactive-local"
)

// ErrInputClosed 表示 stdin 已到达 EOF，应用应执行正常退出。
var ErrInputClosed = errors.New("交互输入已关闭")

type Adapter struct {
    input  io.Reader
    output io.Writer
    events chan wecom.IncomingText
    fatal  chan error

    writeMu sync.Mutex
    stop    chan struct{}
    // 其余一次性状态使用 sync.Once，消息序号只由 Run goroutine更新。
}

// NewAdapter 创建本地交互适配器。
func NewAdapter(input io.Reader, output io.Writer) (*Adapter, error)

// Run 读取 stdin 并产生本地单聊消息，直到取消、EOF 或 I/O 失败。
func (a *Adapter) Run(ctx context.Context) error

// Events 返回单向入站消息流。
func (a *Adapter) Events() <-chan wecom.IncomingText

// RespondMarkdown 输出带“回复”标签的回调消息。
func (a *Adapter) RespondMarkdown(ctx context.Context, requestID, content string) error

// SendMarkdown 输出带“通知”标签的主动消息。
func (a *Adapter) SendMarkdown(ctx context.Context, content string) error
```

实现要点：

- reader 放入独立 goroutine，`Run` 在 reader 结果、fatal channel 和 `ctx.Done()` 之间 select，保证信号退出不等待阻塞 stdin。
- reader 向结果 channel 发送时同时 select `stop`，避免 `Run` 返回后继续阻塞发送；底层阻塞 Read 无法强制中断，进程退出时由操作系统回收。
- `Scanner.Buffer` 设置明确上限，例如 1 MiB；超过上限返回输入读取错误，不回显内容。
- 不关闭 `events` channel。EOF 由 `Run` 的 `ErrInputClosed` 通知应用层，随后应用层取消消息消费者，避免 events 关闭与 EOF 根因竞态。
- 每个输出块先完整构造，再在 `writeMu` 内执行一次 `io.WriteString`。
- 写失败通过容量为 1 的 fatal channel 首次上报，使 `Service.reply` 即使忽略适配器错误，交互运行循环仍会失败并停止应用。
- 不在 stderr 打印输入、回复正文或终端快照。

- [ ] **Step 6: 验证 ConsoleAdapter**

Run:

```sh
go test -race ./internal/interactive
```

Expected: PASS，无数据竞争或 goroutine 等待超时。

- [ ] **Step 7: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/interactive
git commit -m "feat: 添加本地控制台适配器"
```

## Task 4：把应用运行时从企业微信专用抽成通用 IM 运行时

**Files:**

- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`

- [ ] **Step 1: 先补通用运行时的根因优先级测试**

在现有生命周期测试基础上新增或改写断言，确保：

- IM 连接循环与 Herdr Supervisor 是 primary component。
- 消息消费循环不是 primary component。
- primary component 的真实错误优先于由取消派生的消息循环结果。
- events 独立关闭仍返回 `ErrLoopStopped`。
- 父 context 先取消仍是正常退出。

不要再通过组件名字字符串 `wecom` 判断优先级；测试应针对 `primary` 角色。

- [ ] **Step 2: 运行应用层测试并记录基线**

Run:

```sh
go test ./internal/app
```

Expected: 新测试 FAIL；原有测试 PASS。

- [ ] **Step 3: 重命名运行时接口和字段**

把企业微信专用运行接口改成通用接口，但入站消息类型暂时继续复用 `wecom.IncomingText`，避免无价值的大范围协议重命名：

```go
type imRuntime interface {
    bridge.IMAdapter
    Run(ctx context.Context) error
    Events() <-chan wecom.IncomingText
}

type applicationRuntime struct {
    im         imRuntime
    supervisor runtimeRunner
    handler    messageHandler

    normalIMExit func(error) bool

    herdr    bridge.ManagedHerdr
    factory  bridge.HerdrFactory
    service  *bridge.Service
    notifier *bridge.Notifier
}
```

`weComRuntime`、`applicationRuntime.wecom` 和测试 fake 名称可在本任务中机械改为 `imRuntime`、`runtime.im` 和 `fakeIM`；企业微信客户端本身仍保留 `wecom` 包名。

- [ ] **Step 4: 抽取共享 Bridge 装配**

把 registry、buffer、guard、deduper、service、notifier、factory、supervisor 的创建抽成：

```go
func assembleBridgeRuntime(
    im imRuntime,
    allowedUserID string,
    client bridge.ManagedHerdr,
    logger *slog.Logger,
) (*applicationRuntime, error)
```

原 `assembleRuntime` 只负责创建企业微信客户端，再调用共享函数。保持以下保证：

- 一个 Herdr Client 实例被 Service、Notifier 和 Supervisor 共享。
- `PolicyGuard` 使用传入的 allowed user。
- 按键审计仍使用不受普通日志级别过滤的 `slogKeyAuditSink`。
- 企业微信官方 endpoint 默认逻辑不变。

- [ ] **Step 5: 用角色而不是名字冻结运行根因**

给 `componentResult` 增加 `primary bool`，启动时：

```go
start("im", true, runtime.im.Run)
start("herdr", true, runtime.supervisor.Run)
go runComponent(messageContext, "messages", false, messageLoop, results)
```

`runtimeRootError` 第一轮只检查 `primary && err != nil && !shutdownDerived`，第二轮检查意外 nil，第三轮兜底其它非取消错误。错误文本可以变为 `im 运行失败`，但 `errors.Is` 根因必须保持。

- [ ] **Step 6: 运行应用层与全仓库测试**

Run:

```sh
go test -race ./internal/app
go test ./...
```

Expected: PASS；普通企业微信模式的装配、锁顺序、日志和退出测试无回归。

- [ ] **Step 7: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/app
git commit -m "refactor: 抽取通用 IM 应用运行时"
```

## Task 5：在应用层装配交互模式、独立锁和 EOF 正常退出

**Files:**

- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`

- [ ] **Step 1: 先写交互模式应用测试**

给 `Options` 预期增加：

```go
// Interactive 选择本地 stdin/stdout 交互模式。
Interactive bool
// Stdin 是交互输入；nil 时使用 os.Stdin。
Stdin io.Reader
```

新增测试覆盖：

1. `Interactive=true` 且 `ConfigPath=""` 时调用 `LoadInteractive`，不会调用普通 `Load` 或 `Getenv`。
2. 交互模式先解析最终 Socket，再以 `interactive-<socket hash>.lock` 获取锁。
3. 普通模式仍以 `<bot hash>.lock` 获取锁，并在锁冲突时不解析 Socket。
4. 同一 Socket 的两个交互实例冲突；交互锁名不等于企业微信 bot 锁名。
5. `ErrInputClosed` 作为 IM 组件第一个结果时正常退出并释放锁。
6. EOF 后其它组件退出超时仍返回 `ErrShutdownTimeout`，不能被正常 EOF 吞掉，且锁继续强持有到组件结束。
7. ConsoleAdapter 写失败是 fatal，取消 Herdr 和消息循环并返回包装错误。
8. 父 context 取消仍正常退出。

- [ ] **Step 2: 运行新增测试并确认失败**

Run:

```sh
go test ./internal/app -run 'TestRunInteractive|TestInteractive'
```

Expected: FAIL，提示 `Options.Interactive`、交互配置依赖或交互装配尚不存在。

- [ ] **Step 3: 扩展依赖注入边界**

`appDependencies` 增加：

```go
loadInteractiveConfig func(string) (config.Config, error)
assembleInteractive   func(config.Config, string, io.Reader, io.Writer, *slog.Logger) (*applicationRuntime, error)
```

`assemblyDependencies` 增加可替换的 ConsoleAdapter 构造器：

```go
newInteractive func(io.Reader, io.Writer) (imRuntime, error)
```

默认依赖分别使用 `config.LoadInteractive` 和 `interactive.NewAdapter`。

由于 Go 的函数返回值不协变，默认构造器必须显式包装：

```go
newInteractive: func(input io.Reader, output io.Writer) (imRuntime, error) {
    return interactive.NewAdapter(input, output)
},
```

- [ ] **Step 4: 保持普通模式路径不变并新增交互路径**

`Run` 只负责参数分流：

```go
func Run(ctx context.Context, options Options) error {
    if options.Interactive {
        return runInteractive(ctx, options)
    }
    return runWeCom(ctx, options)
}
```

把现有实现移动到 `runWeCom`，保留“配置必填、先锁 bot 再解析 Socket”的顺序和既有日志字段。

`runInteractive` 顺序固定为：

```text
校验 context
  -> LoadInteractive（config 可空）
  -> 创建 stderr logger
  -> 通过显式配置或公共 CLI ResolveSocket
  -> 创建 cache/herdr-pal 目录
  -> 获取 interactive-<socket hash>.lock
  -> 创建 ConsoleAdapter 与共享 Bridge
  -> 运行三条循环
  -> 正常 EOF/Ctrl+C 或错误退出
  -> 释放或按超时策略继续持有锁
```

交互模式的 nil 默认值：stdin 使用 `os.Stdin`，stdout 使用 `os.Stdout`，stderr 使用 `os.Stderr`。

- [ ] **Step 5: 装配本地单用户策略**

交互装配创建 ConsoleAdapter 后调用共享装配：

```go
runtime, err := assembleBridgeRuntime(
    adapter,
    interactive.UserID,
    client,
    logger,
)
runtime.normalIMExit = func(err error) bool {
    return errors.Is(err, interactive.ErrInputClosed)
}
```

不要为交互模式跳过 Guard、Deduper、occupant 校验或按键审计。

- [ ] **Step 6: 让 `runRuntime` 将选中的正常 IM 退出视为正常根因**

第一次 select 得到结果后计算：

```go
normalShutdown := parentTriggeredShutdown(parent, selectedParent, selectedResult)
if selectedResult != nil && selectedResult.name == "im" && runtime.normalIMExit != nil {
    normalShutdown = normalShutdown || runtime.normalIMExit(selectedResult.err)
}
```

把该布尔值传给 `runtimeRootError`。因为 ConsoleAdapter EOF 不关闭 events channel，正常情况下 EOF 结果一定先触发取消；消息循环随后只返回派生的 `context.Canceled`。

若 drain 超时，即使 `normalShutdown=true` 也必须返回 `ErrShutdownTimeout`。不要把所有包含 `ErrInputClosed` 的联合错误一概吞掉。

- [ ] **Step 7: 验证应用生命周期**

Run:

```sh
go test -race ./internal/app
```

Expected: PASS；EOF、Ctrl+C、fatal 输出错误和退出超时均保持确定根因。

- [ ] **Step 8: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/app
git commit -m "feat: 装配交互模式应用生命周期"
```

## Task 6：为命令行增加 `-i` 并保持普通模式兼容

**Files:**

- Modify: `cmd/herdr-pal/main.go`
- Modify: `cmd/herdr-pal/main_test.go`

- [ ] **Step 1: 先扩展 CLI 表驱动测试**

把测试入口改为可注入 stdin：

```go
func run(
    ctx context.Context,
    args []string,
    stdin io.Reader,
    stdout, stderr io.Writer,
    execute appExecutor,
) int
```

新增用例：

| 参数 | 预期 |
| --- | --- |
| `-i` | code 0，执行一次，`Options.Interactive=true`，config 为空 |
| `-i -config local.json` | code 0，交互模式收到配置路径 |
| 无参数 | code 2，普通模式不执行 |
| `-config config.json` | 保持现有普通模式行为 |
| `-i --version` | code 2，不执行、不输出版本 |
| `--version` | code 0，只输出版本 |
| `-i extra` | code 2，不执行 |

同时断言传给 `Options.Stdin`、`Stdout`、`Stderr` 的对象就是 CLI 收到的对象。

- [ ] **Step 2: 运行 CLI 测试并确认失败**

Run:

```sh
go test ./cmd/herdr-pal
```

Expected: FAIL，`-i` 尚未定义或 config 仍被强制要求。

- [ ] **Step 3: 实现参数规则和帮助文本**

新增：

```go
interactiveMode := flags.Bool("i", false, "进入本地交互模式")
```

规则按以下顺序处理：

1. flags 解析错误或多余位置参数：code 2。
2. `-i` 与 `--version` 同时出现：code 2。
3. 单独 `--version`：code 0。
4. 非交互模式缺少 `-config`：code 2。
5. 其余调用 `app.Run`，传入 `Interactive` 和 `Stdin`。

帮助文本：

```text
用法: herdr-pal -i [-config /path/to/config.json]
      herdr-pal -config /path/to/config.json
      herdr-pal --version
```

`main` 调用改为：

```go
os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, app.Run))
```

- [ ] **Step 4: 验证退出码和构建产物**

Run:

```sh
go test ./cmd/herdr-pal
./build.sh
./dist/herdr-pal --version
```

Expected: CLI 测试和构建通过；`-i --version` 不启动应用。

- [ ] **Step 5: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add cmd/herdr-pal
git commit -m "feat: 添加交互模式命令行入口"
```

## Task 7：增加 fake Herdr + ConsoleAdapter 端到端测试

**Files:**

- Create: `internal/integration/interactive_test.go`
- Modify: `internal/integration/bridge_test.go`

- [ ] **Step 1: 建立并发安全的交互应用测试夹具**

测试夹具使用：

- `testkit.NewHerdrServer` 提供 protocol 17 fake Socket。
- `io.Pipe` 向 ConsoleAdapter 写行，并用关闭 writer 模拟 Ctrl+D。
- 线程安全 output buffer，允许测试在 app goroutine 写入时轮询 stdout/stderr。
- 不创建 `testkit.WeComServer`，不设置 Secret。
- 临时配置只包含 `herdr.socket_path` 和 `log.level`。

建议接口：

```go
type interactiveHarness struct {
    input  *io.PipeWriter
    stdout *safeBuffer
    stderr *safeBuffer
    run    *applicationRun
    herdr  *testkit.HerdrServer
}
```

- [ ] **Step 2: 先写完整交互流程测试**

依次执行并等待每一步完成：

```text
/ls
/sel 1
/con
继续交互模式端到端测试
/space
```

断言：

- stdout 出现 `herdr-pal> `、`[回复]` 和 pane ID。
- `/con` 只显示最后 100 行。
- fake 只收到一次 `agent.prompt(target=pane-1)`。
- fake 只收到一次 `agent.send_keys(target=pane-1, keys=[space])`。
- stderr 的显式按键审计包含 `user_id=interactive-local`、`pane_id=pane-1`、`key=space` 和 `result=sent`。
- stderr 不包含普通 prompt 正文或终端行。

随后发出 blocked 状态事件，断言 stdout 出现 `[通知]`，通知快照仍只包含最后 100 行。

- [ ] **Step 3: 先写 EOF 正常退出与锁释放测试**

关闭 `io.PipeWriter` 后：

- `app.Run` 在 3 秒内返回 nil。
- 重新启动同 Socket 的第二个交互实例能够获取锁。
- stdout 不打印“启动失败”类内部错误。

- [ ] **Step 4: 运行测试并确认失败**

Run:

```sh
go test ./internal/integration -run TestInteractiveBridgeEndToEnd -v
```

Expected: 如果前序任务尚有装配或输出问题则 FAIL；不得改用直接调用 `HerdrClient` 绕过完整 Bridge。

- [ ] **Step 5: 只做满足端到端测试所需的最小修正**

允许修正范围：ConsoleAdapter 输出边界、应用测试夹具、交互锁释放和共享 Bridge 装配。不要在本任务增加新命令或改变现有命令语义。

- [ ] **Step 6: 验证 fake 端到端与现有企业微信端到端**

Run:

```sh
go test -race ./internal/integration -run 'TestInteractiveBridgeEndToEnd|TestBridgeEndToEnd' -v
```

Expected: 两种 IM 适配器均通过同一 Bridge 行为测试。

- [ ] **Step 7: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/integration
git commit -m "test: 覆盖交互模式端到端流程"
```

## Task 8：扩展真实 Herdr 只读测试和双重门禁 prompt 测试

**Files:**

- Create: `internal/integration/real_herdr_test.go`
- Modify: `internal/integration/bridge_test.go`

- [ ] **Step 1: 抽取真实 Herdr 公共门禁 helper**

把现有 `TestRealHerdr` 中的 CLI 状态解析抽到新文件，返回 context、Socket、Client 和 snapshot。helper 只执行：

```text
herdr status server --json
  -> running=true
  -> protocol=17
  -> socket 非空
  -> client.CheckCompatible
  -> client.Snapshot
```

环境变量 `HERDR_PAL_INTEGRATION` 未设置、CLI 不可用、服务未运行、协议不匹配时 Skip；门禁通过后的 API 错误必须 Fail，不能继续 Skip。

- [ ] **Step 2: 扩展默认只读测试**

从 snapshot 中选择一个带 Agent 的 pane；没有 Agent 时明确 Skip。使用 pane ID 调用：

```go
current, err := client.GetAgent(ctx, target.PaneID)
result, err := client.ReadRecent(ctx, target.PaneID, 1)
```

断言 `session.MatchesAgent(target, current)` 且 `result.PaneID == target.PaneID`。

然后启动真实 `app.Run` 交互模式，配置显式指向门禁得到的 Socket，通过 stdin 依次发送 `/ls`、`/sel N`、`/con`。这里的 `N` 由 `session.Registry.CreateListSnapshot()` 中目标 pane 的位置计算，不解析展示文本。测试只在内存检查输出包含 `[回复]` 且不含“暂不可用”或 `agent_not_found`，不打印完整终端快照。

- [ ] **Step 3: 增加实时 prompt 双重门禁测试**

测试名固定为：

```go
func TestRealHerdrLivePrompt(t *testing.T)
```

仅在以下三个条件同时满足时运行：

```text
HERDR_PAL_INTEGRATION=1
HERDR_PAL_LIVE_INPUT=1
HERDR_PAL_LIVE_PANE_ID 非空
```

测试步骤：

1. 从公共 snapshot 找到精确 pane ID，找不到则 Fail。
2. 用 `session.Registry` 构造目标，再用 `client.GetAgent(paneID)` 确认 occupant 匹配。
3. 先读取 `recent_unwrapped, 100`，记录 `HERDR_PAL_LIVE_OK` 的出现次数。
4. 只调用一次：

```go
const livePrompt = "这是 Herdr Pal 实时集成测试。请只回复由 HERDR、PAL、LIVE、OK 四个词使用下划线连接成的字符串；不要调用工具，不要修改文件。"
if err := client.Prompt(ctx, paneID, livePrompt); err != nil {
    t.Fatalf("真实 Herdr Prompt() error = %v", err)
}
```

5. 每 500 ms 读取最近 100 行，最多等待 90 秒；当 marker 出现次数大于基线时成功。
6. 超时时只报告 pane ID、等待时间和 marker 名称，不输出终端正文。
7. 不重试 prompt，不调用 `SendKey`。

- [ ] **Step 4: 运行无门禁测试并确认安全跳过**

Run:

```sh
go test ./internal/integration -run 'TestRealHerdr' -v
```

Expected: 未设置环境变量时两个真实测试都 Skip，其它测试 PASS。

- [ ] **Step 5: 使用当前 debug Herdr 运行只读测试**

Run:

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
go test ./internal/integration -run '^TestRealHerdr$' -count=1 -v
```

Expected: PASS；`GetAgent`、`ReadRecent`、`/ls`、`/sel`、`/con` 都使用真实 protocol 17 Socket，且不会向 Agent 输入。

- [ ] **Step 6: 重新确认当前 pane 并运行一次实时 prompt 测试**

先使用公共 CLI/Socket snapshot 确认测试目标仍为当前已授权 Agent。若仍为 `w1:p1`，Run:

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH \
HERDR_PAL_INTEGRATION=1 \
HERDR_PAL_LIVE_INPUT=1 \
HERDR_PAL_LIVE_PANE_ID='w1:p1' \
go test ./internal/integration -run '^TestRealHerdrLivePrompt$' -count=1 -v
```

若 snapshot 显示 pane ID 已变化，只替换上述环境变量值，不修改或硬编码测试源码。

Expected: 只发送一次 prompt，并在新增输出中观察到 `HERDR_PAL_LIVE_OK`。

- [ ] **Step 7: 执行提交门禁并提交**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git add internal/integration
git commit -m "test: 添加真实 Herdr 交互联调门禁"
```

## Task 9：更新项目文档并完成最终手工验收

**Files:**

- Modify: `README.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/HERDR_API_AUDIT.md`

- [ ] **Step 1: 更新 README 的兼容状态与交互用法**

删除“当前开发机只有 protocol 14、真实联调会跳过”的过期描述，改为说明：

- Homebrew Herdr 0.7.1 与源码 debug Herdr 使用不同配置目录。
- 当前真实联调基线是 debug Herdr 0.7.5/protocol 17。
- 所有 Agent API 使用 pane ID。

新增运行示例：

```sh
./dist/herdr-pal -i
./dist/herdr-pal -i -config /绝对路径/interactive.json
PATH=/Users/wxc/Code/herdr/target/debug:$PATH ./dist/herdr-pal -i
```

说明交互配置不需要 `wecom` 字段和 `HERDR_PAL_WECOM_SECRET`，stdout 用于聊天内容，stderr 用于结构化日志与按键审计，Ctrl+C/Ctrl+D 都能退出。

- [ ] **Step 2: 更新架构、交接和 API 审计文档**

`docs/BRIDGE_ARCHITECTURE.md`：

- 增加 ConsoleAdapter 与 WeComClient 并列接入共享 Bridge 的结构。
- 明确交互模式不绕过 Guard、Deduper、Service、Notifier 或 Supervisor。

`docs/HANDOFF_CONTEXT.md`：

- 记录真实 Herdr 0.7.5/protocol 17 已联调。
- 记录 PATH 中 Homebrew 与 debug 二进制的区别。
- 记录只读测试和实时 prompt 测试命令。

`docs/HERDR_API_AUDIT.md`：

- 记录最新 Agent API target 为 pane ID 或唯一 Agent 名。
- 说明 terminal ID 对真实 protocol 17 返回 `agent_not_found`，只用于 occupant 校验。
- 保留 protocol 精确匹配、revision 固定 0、无公共 output stream 等既有结论。

- [ ] **Step 3: 运行全量自动验证**

Run:

```sh
gofmt -w cmd internal
git diff --check
go vet ./...
go test -count=1 ./...
go test -race ./...
./unittest.sh
./build.sh
```

Expected: 全部 PASS，`dist/herdr-pal` 为 CGO 关闭的单文件构建。

- [ ] **Step 4: 使用当前真实 Herdr 手工验收交互模式**

Run:

```sh
PATH=/Users/wxc/Code/herdr/target/debug:$PATH ./dist/herdr-pal -i
```

在提示符中依次输入：

```text
/ls
/sel 1
/con
请只回复 HERDR_PAL_MANUAL_OK，不要调用工具或修改文件。
```

验收：

- `/con` 不出现 `agent_not_found`。
- 普通文本通过 `agent.prompt` 发送并看到 `[回复] 已发送。`。
- stdout 能看到 working/blocked/done 的 `[通知]`，blocked/done 只附最近 100 行。
- 不向真实 Agent发送 `/enter`、`/space` 或其它按键。
- Ctrl+C 后进程在 10 秒内退出。

- [ ] **Step 5: 检查安全日志**

确认 stderr：

- 不包含企业微信 Secret、完整 prompt、完整终端快照或 Cookie。
- fake 交互按键测试的审计字段完整。
- 手工真实验收未触发按键，因此没有真实按键审计记录。

- [ ] **Step 6: 执行最终提交门禁并提交文档**

Run:

```sh
./unittest.sh
./build.sh
git diff --check
git status --short
git add README.md docs/BRIDGE_ARCHITECTURE.md docs/HANDOFF_CONTEXT.md docs/HERDR_API_AUDIT.md
git commit -m "docs: 补充交互模式与真实 Herdr 联调说明"
```

Expected: 工作树只保留用户已有且与本任务无关的改动；本任务全部提交均位于 `main`，不自动 push。

## 最终验收清单

- [ ] fake Herdr 明确拒绝 terminal ID，四个 Agent API 均以 pane ID 调用。
- [ ] terminal ID 仍参与 occupant 替换校验。
- [ ] `herdr-pal -i` 无配置、无 Bot ID、无 user ID、无 Secret 时可启动。
- [ ] `-i -config` 严格拒绝未知字段，但允许缺失 `wecom`。
- [ ] ConsoleAdapter 的回复、通知和提示符不会并发交错。
- [ ] ConsoleAdapter 的输出错误能停止应用，而不是被 Service 静默吞掉。
- [ ] EOF、Ctrl+C、组件 fatal 和退出超时的根因语义均有测试。
- [ ] 交互模式与企业微信模式使用独立进程锁。
- [ ] fake 端到端覆盖 `/ls`、`/sel`、`/con`、prompt、受限按键、状态通知和审计。
- [ ] 真实只读测试覆盖 `GetAgent`、`ReadRecent` 和交互命令，不向 Agent 输入。
- [ ] 真实 prompt 测试有双重门禁、精确 pane ID、单次发送和新增 marker 判断。
- [ ] 真实测试与日志都不输出完整终端内容。
- [ ] 普通企业微信 CLI、配置校验、WebSocket 重连和现有安全边界无回归。
- [ ] `go vet ./...`、`go test -count=1 ./...`、`go test -race ./...`、`./unittest.sh`、`./build.sh` 全部通过。
