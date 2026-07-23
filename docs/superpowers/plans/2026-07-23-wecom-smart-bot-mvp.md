# Herdr Pal 企业微信智能机器人 MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** 用 Go 实现一个可手工启动、单文件分发的 Herdr Pal，在企业微信智能机器人单聊中完成 Agent 列表与选择、终端分页查看、普通 prompt、受限按键控制和状态主动通知。

**Architecture:** 进程以 BridgeService 为编排中心，企业微信侧由 WeComClient 维护 WebSocket、请求关联和心跳，Herdr 侧由 HerdrClient 与 EventSupervisor 维护 Local Socket 请求和两类订阅。SessionRegistry、PanelBuffer、CommandRouter 与 PolicyGuard 均保持为可独立单测的纯模块；所有运行状态只存内存，Herdr 重连后以 session.snapshot 完全重建。

**Tech Stack:** Go 1.26、标准库 net/unix socket 与 encoding/json、github.com/coder/websocket v1.8.15、github.com/gofrs/flock v0.13.0、Go testing/httptest。

---

## 实现前提与兼容门禁

- 审计基线固定为 Herdr commit 2a20e90、公共 API protocol 17。
- Herdr 的协议版本按其自身语义是精确匹配，不按“大于等于”兼容。启动和每次重连都先执行 ping，只有 protocol == 17 才能继续 snapshot、订阅、读取或输入。
- 当前开发机安装的 herdr 0.7.1 运行 protocol 14，且不提供 herdr api schema/snapshot CLI。单元测试和 fake 集成测试按 protocol 17 实现；真实 Herdr 联调在升级本机已安装 Herdr 后再启用。本计划不修改 /Users/wxc/Code/herdr。
- 企业微信单聊主动推送使用 allowed_user_id 作为 aibot_send_msg.body.chatid，并设置 chat_type: 1。平台仍要求该用户曾向机器人发过消息；投递失败不做持久化补发。
- 企业微信 Markdown content 最大 20480 字节；业务层按 20000 字节切段，为页眉和分段标记留出余量。

## 目标文件结构

~~~text
.
├── build.sh
├── unittest.sh
├── config.example.json
├── go.mod
├── go.sum
├── cmd/herdr-pal/main.go
├── internal/app/app.go
├── internal/version/version.go
├── internal/config/config.go
├── internal/config/config_test.go
├── internal/processlock/lock.go
├── internal/processlock/lock_test.go
├── internal/herdr/client.go
├── internal/herdr/client_test.go
├── internal/herdr/errors.go
├── internal/herdr/protocol.go
├── internal/herdr/protocol_test.go
├── internal/herdr/resolver.go
├── internal/herdr/resolver_test.go
├── internal/herdr/subscription.go
├── internal/herdr/subscription_test.go
├── internal/herdr/types.go
├── internal/command/parser.go
├── internal/command/parser_test.go
├── internal/session/registry.go
├── internal/session/registry_test.go
├── internal/panel/buffer.go
├── internal/panel/buffer_test.go
├── internal/panel/normalize.go
├── internal/panel/normalize_test.go
├── internal/panel/split.go
├── internal/panel/split_test.go
├── internal/policy/dedupe.go
├── internal/policy/dedupe_test.go
├── internal/policy/guard.go
├── internal/policy/guard_test.go
├── internal/wecom/client.go
├── internal/wecom/client_test.go
├── internal/wecom/protocol.go
├── internal/wecom/protocol_test.go
├── internal/wecom/reconnect.go
├── internal/wecom/reconnect_test.go
├── internal/wecom/testdata/message_text.json
├── internal/wecom/testdata/response_ok.json
├── internal/bridge/service.go
├── internal/bridge/service_test.go
├── internal/bridge/notifier.go
├── internal/bridge/notifier_test.go
├── internal/bridge/supervisor.go
├── internal/bridge/supervisor_test.go
├── internal/testkit/herdr_server.go
├── internal/testkit/wecom_server.go
└── internal/integration/bridge_test.go
~~~

## Task 1：建立 Go 工程、版本信息和统一构建入口

**Files:**

- Create: go.mod
- Create: cmd/herdr-pal/main.go
- Create: internal/version/version.go
- Create: internal/version/version_test.go
- Create: build.sh
- Create: unittest.sh
- Create: config.example.json
- Modify: .gitignore

- [ ] **Step 1: 创建 Go module**

go.mod 使用以下内容：

~~~go
module github.com/wenxichang/herdr-pal

go 1.26.0
~~~

- [ ] **Step 2: 先写版本信息失败测试**

internal/version/version_test.go：

~~~go
package version

import "testing"

func TestStringIncludesBuildFields(t *testing.T) {
    oldVersion, oldCommit, oldBuiltAt := Version, Commit, BuiltAt
    t.Cleanup(func() {
        Version, Commit, BuiltAt = oldVersion, oldCommit, oldBuiltAt
    })
    Version, Commit, BuiltAt = "v1.2.3", "abc123", "2026-07-23T00:00:00Z"

    got := String()
    want := "v1.2.3 commit=abc123 built_at=2026-07-23T00:00:00Z"
    if got != want {
        t.Fatalf("String() = %q, want %q", got, want)
    }
}
~~~

- [ ] **Step 3: 运行测试并确认失败**

Run: go test ./internal/version -run TestStringIncludesBuildFields

Expected: FAIL，提示 internal/version 尚不存在或 String 未定义。

- [ ] **Step 4: 实现版本模块和最小入口**

internal/version/version.go：

~~~go
// Package version 提供构建时注入的版本信息。
package version

import "fmt"

var (
    Version = "dev"
    Commit  = "unknown"
    BuiltAt = "unknown"
)

// String 返回适合命令行展示的完整版本。
func String() string {
    return fmt.Sprintf("%s commit=%s built_at=%s", Version, Commit, BuiltAt)
}
~~~

cmd/herdr-pal/main.go 第一阶段只支持 --version，并将实际启动委托给后续的 app.Run：

~~~go
package main

import (
    "fmt"
    "os"

    "github.com/wenxichang/herdr-pal/internal/version"
)

func main() {
    if len(os.Args) == 2 && os.Args[1] == "--version" {
        fmt.Println(version.String())
        return
    }
    fmt.Fprintln(os.Stderr, "用法: herdr-pal --version")
    os.Exit(2)
}
~~~

- [ ] **Step 5: 创建 build.sh 和 unittest.sh**

两个脚本都使用 set -eu，先检查 gofmt -l 输出为空，再执行 go vet ./... 和 go test。build.sh 额外执行：

~~~sh
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-X github.com/wenxichang/herdr-pal/internal/version.Version=${VERSION} -X github.com/wenxichang/herdr-pal/internal/version.Commit=${COMMIT} -X github.com/wenxichang/herdr-pal/internal/version.BuiltAt=${BUILT_AT}" \
  -o dist/herdr-pal ./cmd/herdr-pal
~~~

unittest.sh 在 go env CGO_ENABLED 为 1 时追加 go test -race ./...。脚本通过 chmod +x 标记可执行。

- [ ] **Step 6: 创建安全的示例配置**

config.example.json：

~~~json
{
  "wecom": {
    "bot_id": "BOT_ID",
    "allowed_user_id": "USER_ID"
  },
  "herdr": {
    "session": "",
    "socket_path": ""
  },
  "log": {
    "level": "info"
  }
}
~~~

.gitignore 增加 dist/、config.json、*.lock 和常见临时文件，但不得忽略 config.example.json。

- [ ] **Step 7: 整理依赖并验证**

Run: go mod tidy

Run: ./unittest.sh

Run: ./build.sh

Expected: 全部通过，并生成 dist/herdr-pal；执行 dist/herdr-pal --version 能输出三个构建字段。

- [ ] **Step 8: 提交**

~~~sh
git add go.mod cmd internal/version build.sh unittest.sh config.example.json .gitignore
git commit -m "chore: 初始化 Go 构建与测试入口"
~~~

## Task 2：实现配置加载和单实例进程锁

**Files:**

- Create: internal/config/config.go
- Create: internal/config/config_test.go
- Create: internal/processlock/lock.go
- Create: internal/processlock/lock_test.go
- Modify: go.mod
- Create: go.sum

- [ ] **Step 1: 添加进程锁依赖**

Run: go get github.com/gofrs/flock@v0.13.0

Expected: go.mod 和 go.sum 只新增 gofrs/flock 及其必要间接依赖。

- [ ] **Step 2: 先写配置失败测试**

表驱动测试至少覆盖：

- 正常读取 bot_id、allowed_user_id、session、socket_path 和 log.level。
- Secret 只来自 HERDR_PAL_WECOM_SECRET。
- 缺少 bot_id、allowed_user_id 或 Secret 时返回字段明确的错误。
- JSON 未知字段和尾随内容被拒绝。
- 配置与错误文本不包含 Secret。

核心测试调用约定：

~~~go
cfg, err := Load(path, func(key string) string {
    if key == SecretEnvName {
        return "secret-value"
    }
    return ""
})
~~~

- [ ] **Step 3: 运行测试并确认失败**

Run: go test ./internal/config

Expected: FAIL，提示 Load、Config 或 SecretEnvName 未定义。

- [ ] **Step 4: 实现严格配置解析**

internal/config/config.go 对外接口：

~~~go
// Package config 负责加载和校验 Herdr Pal 本地配置。
package config

const SecretEnvName = "HERDR_PAL_WECOM_SECRET"

type Config struct {
    WeCom WeComConfig `json:"wecom"`
    Herdr HerdrConfig `json:"herdr"`
    Log   LogConfig   `json:"log"`
}

type WeComConfig struct {
    BotID         string `json:"bot_id"`
    AllowedUserID string `json:"allowed_user_id"`
    Secret        string `json:"-"`
}

type HerdrConfig struct {
    Session    string `json:"session"`
    SocketPath string `json:"socket_path"`
}

type LogConfig struct {
    Level string `json:"level"`
}

// Load 从 JSON 文件和环境变量加载配置。
func Load(path string, getenv func(string) string) (Config, error)
~~~

使用 json.Decoder.DisallowUnknownFields，解码后再确认第二次 Decode 返回 io.EOF。字段校验错误只包含字段名，不回显值。

- [ ] **Step 5: 先写进程锁失败测试**

测试在 t.TempDir() 下对同一路径连续 Acquire：

1. 第一次成功。
2. 第二次返回 ErrAlreadyRunning。
3. 第一次 Release 后第三次成功。

- [ ] **Step 6: 实现进程锁**

internal/processlock/lock.go：

~~~go
// Package processlock 防止同一个机器人配置同时建立多个长连接。
package processlock

var ErrAlreadyRunning = errors.New("herdr-pal 已在运行")

type Lock struct {
    file *flock.Flock
}

// Acquire 尝试获取非阻塞文件锁。
func Acquire(path string) (*Lock, error)

// Release 释放文件锁。
func (l *Lock) Release() error
~~~

锁文件默认路径由 app 层使用 os.UserCacheDir()、bot_id 的 SHA-256 前 16 个十六进制字符生成，不将 Secret 写入路径。

- [ ] **Step 7: 验证**

Run: go test ./internal/config ./internal/processlock

Expected: PASS。

- [ ] **Step 8: 提交**

~~~sh
git add internal/config internal/processlock go.mod go.sum
git commit -m "feat: 添加配置校验与单实例锁"
~~~

## Task 3：实现 Herdr NDJSON 请求传输和错误模型

**Files:**

- Create: internal/herdr/errors.go
- Create: internal/herdr/protocol.go
- Create: internal/herdr/protocol_test.go
- Create: internal/herdr/client.go
- Create: internal/herdr/client_test.go

- [ ] **Step 1: 先写 NDJSON 编解码失败测试**

使用 net.Pipe 模拟 Local Socket，覆盖：

- 请求恰好写成一行 JSON 加换行。
- 响应被拆成多次 Write 时仍能完整读取。
- 同一读取缓冲内出现多行时只消费当前响应。
- 超过 8 MiB 的单行返回 ErrFrameTooLarge。
- success、error、空行、无效 JSON 和响应 id 不匹配。

请求断言示例：

~~~go
want := "{"id":"req-1","method":"ping","params":{}}\n"
if got := <-serverReceived; got != want {
    t.Fatalf("request = %q, want %q", got, want)
}
~~~

- [ ] **Step 2: 运行测试并确认失败**

Run: go test ./internal/herdr -run 'TestNDJSON|TestRequest'

Expected: FAIL，协议和客户端类型未定义。

- [ ] **Step 3: 实现稳定错误类型**

internal/herdr/errors.go 定义：

~~~go
var (
    ErrUnavailable    = errors.New("Herdr 不可用")
    ErrFrameTooLarge  = errors.New("Herdr NDJSON 帧过大")
    ErrProtocol       = errors.New("Herdr 协议错误")
    ErrProtocolMismatch = errors.New("Herdr 协议版本不匹配")
)

type APIError struct {
    Code    string
    Message string
}

func (e *APIError) Error() string {
    return fmt.Sprintf("Herdr API %s: %s", e.Code, e.Message)
}
~~~

- [ ] **Step 4: 实现协议 envelope**

internal/herdr/protocol.go：

~~~go
type requestEnvelope struct {
    ID     string
    Method string
    Params any
}

func (r requestEnvelope) MarshalJSON() ([]byte, error) {
    return json.Marshal(struct {
        ID     string `json:"id"`
        Method string `json:"method"`
        Params any    `json:"params"`
    }{ID: r.ID, Method: r.Method, Params: r.Params})
}

type responseEnvelope struct {
    ID     string          `json:"id"`
    Result json.RawMessage `json:"result"`
    Error  *errorBody      `json:"error"`
}
~~~

readLine 使用 bufio.Reader.ReadSlice 循环累积并执行 8 MiB 上限，不使用默认 64 KiB Scanner 限制。

- [ ] **Step 5: 实现每请求一连接的 Client**

公开接口：

~~~go
type Dialer interface {
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Client struct {
    socketPath string
    dialer     Dialer
    timeout    time.Duration
    nextID     atomic.Uint64
}

// NewClient 创建 Herdr 公共 Local Socket API 客户端。
func NewClient(socketPath string, dialer Dialer, timeout time.Duration) *Client

func (c *Client) call(ctx context.Context, method string, params any, result any) error
~~~

call 为每个普通请求打开独立 Unix socket，设置 context deadline，写入一行请求，只读取一行响应并关闭连接。请求 ID 使用 pal:<递增序号>，响应必须匹配同一 ID。

- [ ] **Step 6: 验证**

Run: go test ./internal/herdr

Expected: PASS。

- [ ] **Step 7: 提交**

~~~sh
git add internal/herdr
git commit -m "feat: 添加 Herdr NDJSON 客户端"
~~~

## Task 4：实现 Herdr Socket 解析、协议门禁和业务 API

**Files:**

- Create: internal/herdr/types.go
- Create: internal/herdr/resolver.go
- Create: internal/herdr/resolver_test.go
- Modify: internal/herdr/client.go
- Modify: internal/herdr/client_test.go

- [ ] **Step 1: 先写 Socket 解析失败测试**

通过注入 CommandRunner 覆盖：

- 显式 socket_path 优先，不执行 CLI。
- 默认 session 解析 herdr status server --json 的 socket。
- 命名 session 解析 herdr session list --json。
- session 不存在或未运行返回可操作错误。
- CLI 输出未知字段可忽略，缺少 socket/running 类型错误必须失败。

CommandRunner 接口：

~~~go
type CommandRunner interface {
    Output(ctx context.Context, name string, args ...string) ([]byte, error)
}
~~~

- [ ] **Step 2: 实现 Resolver**

公开函数：

~~~go
// ResolveSocket 使用显式路径或 Herdr 公共 CLI 解析 Local Socket。
func ResolveSocket(
    ctx context.Context,
    explicitPath string,
    sessionName string,
    runner CommandRunner,
) (string, error)
~~~

默认 session 执行 herdr status server --json；命名 session 执行 herdr session list --json。不得读取 Herdr 私有状态文件推断 socket。

- [ ] **Step 3: 先写业务 API 和 protocol 17 门禁失败测试**

fake server 依次返回：

- pong protocol 17：CheckCompatible 成功。
- pong protocol 14 或 18：errors.Is(err, ErrProtocolMismatch)。
- session_snapshot：完整转换 agents/panes/tabs/workspaces。
- pane_read：保留 text 和 truncated，但不依赖 revision。
- agent_info：可用于输入前重新核对 pane、terminal 和 occupant。
- agent_prompted 与 ok/agent_info 等成功结果均按预期类型校验。

- [ ] **Step 4: 定义最小公开模型**

internal/herdr/types.go 至少包含：

~~~go
const RequiredProtocol uint32 = 17

type AgentStatus string

const (
    StatusIdle    AgentStatus = "idle"
    StatusWorking AgentStatus = "working"
    StatusBlocked AgentStatus = "blocked"
    StatusDone    AgentStatus = "done"
    StatusUnknown AgentStatus = "unknown"
)

type AgentSession struct {
    Source string `json:"source"`
    Agent  string `json:"agent"`
    Kind   string `json:"kind"`
    Value  string `json:"value"`
}

type AgentInfo struct {
    TerminalID  string        `json:"terminal_id"`
    Agent       *string       `json:"agent"`
    Name        *string       `json:"name"`
    Title       *string       `json:"title"`
    DisplayAgent *string      `json:"display_agent"`
    AgentStatus AgentStatus   `json:"agent_status"`
    AgentSession *AgentSession `json:"agent_session"`
    WorkspaceID string        `json:"workspace_id"`
    TabID       string        `json:"tab_id"`
    PaneID      string        `json:"pane_id"`
}

type Snapshot struct {
    Version    string      `json:"version"`
    Protocol   uint32      `json:"protocol"`
    Workspaces []Workspace `json:"workspaces"`
    Tabs       []Tab       `json:"tabs"`
    Panes      []Pane      `json:"panes"`
    Agents     []AgentInfo `json:"agents"`
}

type ReadResult struct {
    PaneID    string `json:"pane_id"`
    Text      string `json:"text"`
    Truncated bool   `json:"truncated"`
}
~~~

只建模 MVP 实际使用字段；未知 JSON 字段由 encoding/json 自然忽略。

- [ ] **Step 5: 实现业务方法**

~~~go
// CheckCompatible 校验 Herdr API protocol 必须精确等于 RequiredProtocol。
func (c *Client) CheckCompatible(ctx context.Context) error

// Snapshot 读取当前 Herdr 会话完整快照。
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error)

// GetAgent 通过 agent.get 获取输入前的实时 Agent 信息。
func (c *Client) GetAgent(ctx context.Context, target string) (AgentInfo, error)

// ReadRecent 读取 Agent terminal 最近的未折行文本，lines 必须在 1..1000。
func (c *Client) ReadRecent(ctx context.Context, target string, lines int) (ReadResult, error)

// Prompt 通过 agent.prompt 发送普通用户文本。
func (c *Client) Prompt(ctx context.Context, target, text string) error

// SendKey 通过 agent.send_keys 发送单个已规范化按键。
func (c *Client) SendKey(ctx context.Context, target, key string) error
~~~

对应请求必须精确为：

~~~json
{"method":"agent.read","params":{"target":"terminal-id","source":"recent_unwrapped","lines":100,"format":"text","strip_ansi":true}}
{"method":"agent.prompt","params":{"target":"terminal-id","text":"用户输入"}}
{"method":"agent.send_keys","params":{"target":"terminal-id","keys":["enter"]}}
~~~

- [ ] **Step 6: 验证当前本机门禁行为**

Run: go test ./internal/herdr

Run: herdr status server --json

Expected: 单元测试 PASS；当前本机输出 protocol 14，记录为“真实联调暂不可运行”，但不修改 Herdr 安装或源码。

- [ ] **Step 7: 提交**

~~~sh
git add internal/herdr
git commit -m "feat: 添加 Herdr 协议门禁与业务接口"
~~~

## Task 5：实现 Herdr 长连接订阅与事件解析

**Files:**

- Create: internal/herdr/subscription.go
- Create: internal/herdr/subscription_test.go
- Modify: internal/herdr/types.go

- [ ] **Step 1: 先写订阅握手失败测试**

fake Unix server 接收 events.subscribe 后分别返回：

- subscription_started：Subscribe 成功。
- Herdr error response：Subscribe 返回 APIError。
- 非 subscription_started success：返回 ErrProtocol。
- 连接在确认前关闭：返回 ErrUnavailable。

生命周期请求必须包含：

~~~json
{
  "subscriptions": [
    {"type":"pane.created"},
    {"type":"pane.closed"},
    {"type":"pane.updated"},
    {"type":"pane.exited"},
    {"type":"pane.agent_detected"}
  ]
}
~~~

状态请求为当前 Agent pane 构造一个批量数组，每项为：

~~~json
{"type":"pane.agent_status_changed","pane_id":"pane-1"}
~~~

- [ ] **Step 2: 先写事件解析失败测试**

覆盖：

- 通用生命周期 envelope：event 为 pane_created，data.type 为 pane_created。
- 状态订阅 envelope：event 为 pane.agent_status_changed，data 含 pane_id、workspace_id、agent_status、agent、title、display_agent。
- 一次 socket Read 中多行事件逐条返回。
- 未知 event 保留原始 data，不使连接崩溃。
- 必要字段缺失和无效 JSON 返回 ErrProtocol。
- context 取消和 Close 均能解除阻塞 Recv。

- [ ] **Step 3: 运行测试并确认失败**

Run: go test ./internal/herdr -run 'TestSubscribe|TestSubscription'

Expected: FAIL。

- [ ] **Step 4: 实现订阅模型**

~~~go
type SubscriptionSpec struct {
    Type   string `json:"type"`
    PaneID string `json:"pane_id,omitempty"`
}

type Event struct {
    Kind string
    Data json.RawMessage
}

type AgentStatusEvent struct {
    PaneID       string            `json:"pane_id"`
    WorkspaceID  string            `json:"workspace_id"`
    AgentStatus  AgentStatus       `json:"agent_status"`
    Agent        *string           `json:"agent"`
    Title        *string           `json:"title"`
    DisplayAgent *string           `json:"display_agent"`
    StateLabels  map[string]string `json:"state_labels"`
}

type SubscriptionStream interface {
    Recv(ctx context.Context) (Event, error)
    Close() error
}
~~~

- [ ] **Step 5: 实现 Subscribe**

Subscribe 使用一条独立 Unix socket：

1. 写 events.subscribe 请求。
2. 同步读取并校验 subscription_started。
3. 返回持有 bufio.Reader 和 net.Conn 的 stream。
4. Recv 每次只读取下一行并返回 Event。
5. 不把 PaneReadResult.revision 用作增量游标。

公开辅助构造：

~~~go
func LifecycleSubscriptions() []SubscriptionSpec
func StatusSubscriptions(paneIDs []string) []SubscriptionSpec
func (c *Client) Subscribe(
    ctx context.Context,
    specs []SubscriptionSpec,
) (SubscriptionStream, error)
~~~

- [ ] **Step 6: 验证**

Run: go test ./internal/herdr

Expected: PASS。

- [ ] **Step 7: 提交**

~~~sh
git add internal/herdr
git commit -m "feat: 添加 Herdr 事件订阅支持"
~~~

## Task 6：实现严格命令解析器

**Files:**

- Create: internal/command/parser.go
- Create: internal/command/parser_test.go

- [ ] **Step 1: 写完整表驱动失败测试**

必须逐项覆盖：

| 输入 | 结果 |
| --- | --- |
| /ls | List |
| /sel 1 | Select，Index=1 |
| /con | Content |
| /pageup | PageUp |
| /pagedn | PageDown |
| /keyup、/key up | Key，Key=up |
| /keydn、/key down | Key，Key=down |
| /enter、/key enter | Key，Key=enter |
| /esc、/key esc | Key，Key=esc |
| /space、/key space | Key，Key=space |
| /key A、/key z、/key 7 | Key，保留原字符 |
| 普通文本 | Prompt，保留原文本 |

非法测试至少包含：

- /sel、/sel 0、/sel -1、/sel 1 2
- /key、/key tab、/key ctrl+c、/key 中、/key aa
- /key UP、/KEY up、/unknown
- 只包含空白的输入
- 所有未知斜杠命令不得退化为 Prompt

- [ ] **Step 2: 运行测试并确认失败**

Run: go test ./internal/command

Expected: FAIL。

- [ ] **Step 3: 实现纯解析模块**

~~~go
// Package command 将企业微信文本严格转换为内部动作。
package command

type Kind uint8

const (
    KindList Kind = iota + 1
    KindSelect
    KindContent
    KindPageUp
    KindPageDown
    KindKey
    KindPrompt
)

type Action struct {
    Kind  Kind
    Index int
    Key   string
    Text  string
}

// Parse 严格解析内置命令；未知斜杠命令返回错误。
func Parse(input string) (Action, error)
~~~

实现规则：

- 命令允许首尾空白，但命令 token 之间只按 strings.Fields 解析。
- 普通 Prompt 不做 TrimSpace 后重写，除检查全空白外保留原始内容。
- 特殊 key name 只接受小写 up/down/enter/esc/space。
- 单个 ASCII 字母或数字保留大小写。

- [ ] **Step 4: 验证**

Run: go test ./internal/command

Expected: PASS。

- [ ] **Step 5: 提交**

~~~sh
git add internal/command
git commit -m "feat: 添加企业微信命令解析器"
~~~

## Task 7：实现 SessionRegistry、列表快照和目标失效

**Files:**

- Create: internal/session/registry.go
- Create: internal/session/registry_test.go

- [ ] **Step 1: 先写 Registry 失败测试**

覆盖：

- Replace 从 snapshot 建立 workspace/tab/pane/agent 索引。
- ListAgents 只返回存在 Agent occupant 的 pane，按 workspace.number、tab.number、pane_id 稳定排序。
- CreateListSnapshot 生成从 1 开始的编号。
- Select 只能使用最近一次列表快照。
- pane 关闭、terminal_id 变化或 occupant fingerprint 变化使选择失效。
- 普通状态变化不使选择失效。
- Herdr reconnect 的 Replace(..., true) 无条件清空选择和列表快照。
- ValidateSelected 每次返回当前数据副本，不暴露内部 map。
- MatchesAgent 能用 agent.get 的实时结果核对 pane、terminal 和 occupant。

- [ ] **Step 2: 定义目标和变化模型**

~~~go
type Target struct {
    PaneID       string
    TerminalID   string
    OccupantKey  string
    Agent        string
    DisplayAgent string
    Title        string
    Status       herdr.AgentStatus
    Workspace    string
    Tab          string
}

type ChangeSet struct {
    AgentSetChanged     bool
    SelectionInvalidated bool
    RemovedTargets      []Target
    ReplacedTargets     []Target
}

type Transition struct {
    Target   Target
    Previous herdr.AgentStatus
    Current  herdr.AgentStatus
}
~~~

OccupantKey 生成规则：

1. 有 agent_session 时，使用 terminal_id、agent、session.source、session.kind、session.value 的 SHA-256。
2. 无 agent_session 时，使用 terminal_id、agent、display_agent 的 SHA-256。
3. 不使用 PaneReadResult.revision，也不使用会随状态变化增长的 state_change_seq。

- [ ] **Step 3: 运行测试并确认失败**

Run: go test ./internal/session

Expected: FAIL。

- [ ] **Step 4: 实现并发安全 Registry**

~~~go
// Package session 保存可由 Herdr snapshot 重建的运行时索引。
package session

type Registry struct {
    mu           sync.RWMutex
    targets      map[string]Target
    listSnapshot []string
    selectedKey  string
}

// Replace 用 snapshot 完全替换运行时视图。
func (r *Registry) Replace(snapshot herdr.Snapshot, reconnect bool) ChangeSet

// CreateListSnapshot 返回稳定编号列表并替换旧编号快照。
func (r *Registry) CreateListSnapshot() []Target

// Select 从最近一次列表快照选择目标。
func (r *Registry) Select(index int) (Target, error)

// ValidateSelected 重新校验 pane、terminal 和 occupant。
func (r *Registry) ValidateSelected() (Target, error)

// ApplyStatus 更新状态事件并返回前后状态。
func (r *Registry) ApplyStatus(event herdr.AgentStatusEvent) (Transition, error)

// AgentPaneIDs 返回当前全部 Agent pane ID 的稳定排序副本。
func (r *Registry) AgentPaneIDs() []string

// MatchesAgent 判断实时 agent.get 结果是否仍对应已选择目标。
func MatchesAgent(target Target, current herdr.AgentInfo) bool

// ClearSelection 清空选择和列表编号快照。
func (r *Registry) ClearSelection()
~~~

ApplyStatus 不凭事件中的 agent 字段创建未知 pane；未知 pane 由 supervisor 触发 snapshot 刷新。

- [ ] **Step 5: 验证**

Run: go test ./internal/session

Expected: PASS。

- [ ] **Step 6: 提交**

~~~sh
git add internal/session
git commit -m "feat: 添加 Agent 会话索引与选择管理"
~~~

## Task 8：实现终端内容规范化、分页缓存和 UTF-8 分段

**Files:**

- Create: internal/panel/normalize.go
- Create: internal/panel/normalize_test.go
- Create: internal/panel/buffer.go
- Create: internal/panel/buffer_test.go
- Create: internal/panel/split.go
- Create: internal/panel/split_test.go

- [ ] **Step 1: 先写终端规范化失败测试**

覆盖：

- CSI 和 OSC ANSI 序列清除。
- CRLF 转 LF。
- 同一物理行中的回车重绘只保留最后一段。
- NUL 被移除。
- 每行尾部空白去除，但行首缩进保留。
- 最多去除结尾空行，不删除中间空行。
- 中文、emoji 和无效 UTF-8 的处理可预测；无效 UTF-8 替换为 RuneError。

- [ ] **Step 2: 实现 Normalize**

~~~go
// Normalize 将终端近期快照转换为稳定的逻辑行。
func Normalize(text string) []string
~~~

清理保持保守：不根据 Agent 文案猜测并删除语义内容，不把快照解释成结构化 LLM 消息。

- [ ] **Step 3: 先写分页失败测试**

至少覆盖：

- Refresh 只保存最新 100 行并将 Page 设为 0。
- NextReadSize 从 200、300 递增到 1000。
- Expand 在新快照中找到旧缓存的最后一次完整连续重叠，向前追加内容并忽略锚点后的新输出。
- Render 对第 0 至第 9 页切片正确。
- PageDown 只使用缓存，不调用刷新。
- 无法对齐时清空缓存、页码归零并返回 ErrPanelChanged。
- 到 1000 行或没有更早前缀时返回 ErrOldestPage。
- target key 变化时旧缓存不可复用。

- [ ] **Step 4: 实现 PanelBuffer**

~~~go
const (
    PageSize = 100
    MaxLines = 1000
)

var (
    ErrPanelChanged = errors.New("终端内容已变化")
    ErrNewestPage   = errors.New("已经是最新内容")
    ErrOldestPage   = errors.New("已经是最早可读取内容")
)

type Buffer struct {
    targetKey string
    lines     []string
    page      int
}

// Refresh 替换为最新 100 行并回到第 0 页。
func (b *Buffer) Refresh(targetKey string, lines []string)

// NextReadSize 返回下一次 pageup 需要的 agent.read lines。
func (b *Buffer) NextReadSize() (int, error)

// Expand 用旧缓存作为连续重叠锚点扩充更早内容。
func (b *Buffer) Expand(targetKey string, snapshot []string) error

// PageDown 向更新内容移动一页。
func (b *Buffer) PageDown() error

// Render 返回当前页的副本。
func (b *Buffer) Render() []string

// Reset 清空全部分页状态。
func (b *Buffer) Reset()
~~~

Expand 的最小算法：

~~~go
index := lastContiguousIndex(snapshot, b.lines)
if index < 0 {
    b.Reset()
    return ErrPanelChanged
}
prefix := snapshot[:index]
room := MaxLines - len(b.lines)
if len(prefix) > room {
    prefix = prefix[len(prefix)-room:]
}
if len(prefix) == 0 {
    return ErrOldestPage
}
b.lines = append(append([]string(nil), prefix...), b.lines...)
b.page++
return nil
~~~

- [ ] **Step 5: 先写 Markdown 分段失败测试**

覆盖：

- 每段不超过传入字节上限。
- 不在 UTF-8 rune 中间截断。
- 优先在换行处分段，超长单行才按 rune 切分。
- 终端内容中的三个反引号被安全中和。
- 每一段都有页码、分段序号和独立闭合的代码块。

- [ ] **Step 6: 实现渲染与分段**

~~~go
const WeComContentLimit = 20000

// RenderPage 将终端行标记为“终端近期快照”，而非 assistant 回复。
func RenderPage(target session.Target, page int, lines []string) string

// SplitMarkdown 按 UTF-8 字节上限切分企业微信 Markdown。
func SplitMarkdown(content string, limit int) []string
~~~

- [ ] **Step 7: 验证**

Run: go test ./internal/panel

Expected: PASS。

- [ ] **Step 8: 提交**

~~~sh
git add internal/panel
git commit -m "feat: 添加终端快照分页与消息分段"
~~~

## Task 9：实现用户、按键和消息幂等策略

**Files:**

- Create: internal/policy/guard.go
- Create: internal/policy/guard_test.go
- Create: internal/policy/dedupe.go
- Create: internal/policy/dedupe_test.go

- [ ] **Step 1: 先写 Guard 失败测试**

覆盖：

- 只允许 chattype == single 且 userid == allowed_user_id。
- group、空 userid 和其他用户全部拒绝，拒绝后不得触发 Herdr 调用。
- up/down/enter/esc/space 和单个 ASCII 字母数字通过。
- tab、ctrl+c、中文、空串和多字符被拒绝。
- Key 审计对象只保存用户、pane、occupant hash、规范化 key、时间和结果，不保存 Secret 或终端内容。

- [ ] **Step 2: 实现 Guard**

~~~go
// Package policy 实现外部输入进入 Herdr 前的安全边界。
package policy

type Identity struct {
    UserID   string
    ChatType string
}

type Guard struct {
    allowedUserID string
}

// Authorize 校验唯一允许用户和单聊边界。
func (g *Guard) Authorize(identity Identity) error

// ValidateKey 对命令解析后的按键再次执行白名单校验。
func (g *Guard) ValidateKey(key string) error
~~~

- [ ] **Step 3: 先写 TTL 幂等缓存失败测试**

使用注入时钟覆盖：

- 第一次 Add 返回 true。
- TTL 内同 key 返回 false。
- TTL 后可再次处理。
- 超过容量时淘汰最旧条目。
- 空 msgid 被拒绝，不得绕过幂等。
- 并发添加同一 msgid 只有一个 goroutine 成功。

- [ ] **Step 4: 实现 Deduper**

~~~go
type Deduper struct {
    mu       sync.Mutex
    entries  map[string]time.Time
    order    []string
    ttl      time.Duration
    capacity int
    now      func() time.Time
}

// AddIfNew 原子地登记幂等键；仅首次有效调用返回 true。
func (d *Deduper) AddIfNew(key string) bool
~~~

默认由 app 创建 24 小时 TTL、10000 容量的实例。幂等记录在执行任何 prompt 或 key 前写入；即使后续外部调用失败，也不因 webhook 重试而重复终端输入。

- [ ] **Step 5: 验证**

Run: go test ./internal/policy

Expected: PASS。

- [ ] **Step 6: 提交**

~~~sh
git add internal/policy
git commit -m "feat: 添加输入策略与消息幂等保护"
~~~

## Task 10：实现企业微信协议编码、回调解析和 req_id 关联

**Files:**

- Create: internal/wecom/protocol.go
- Create: internal/wecom/protocol_test.go
- Create: internal/wecom/testdata/message_text.json
- Create: internal/wecom/testdata/response_ok.json
- Modify: go.mod
- Modify: go.sum

- [ ] **Step 1: 添加 WebSocket 依赖**

Run: go get github.com/coder/websocket@v1.8.15

Expected: go.mod 和 go.sum 新增 coder/websocket。

- [ ] **Step 2: 写入官方协议最小 fixture**

message_text.json 使用单聊文本回调：

~~~json
{
  "cmd": "aibot_msg_callback",
  "headers": {"req_id": "callback-1"},
  "body": {
    "msgid": "message-1",
    "aibotid": "bot-1",
    "chattype": "single",
    "from": {"userid": "user-1"},
    "msgtype": "text",
    "text": {"content": "/ls"},
    "future_field": true
  }
}
~~~

response_ok.json：

~~~json
{
  "headers": {"req_id": "request-1"},
  "errcode": 0,
  "errmsg": "ok"
}
~~~

- [ ] **Step 3: 先写协议失败测试**

覆盖：

- aibot_subscribe 编码包含 bot_id、secret 和新 req_id。
- aibot_msg_callback 文本解析。
- 非 text 回调被识别为不支持类型，不误当空 prompt。
- aibot_respond_msg 透传 callback req_id，并编码 Markdown。
- aibot_send_msg 使用 allowed_user_id、chat_type: 1 和 Markdown。
- ping 编码。
- 响应按 headers.req_id 关联，errcode 非 0 返回 ProtocolError。
- 未知 JSON 字段不会失败，必要字段缺失必须失败。
- content 超过 20480 字节在协议层再次拒绝。

- [ ] **Step 4: 运行测试并确认失败**

Run: go test ./internal/wecom -run TestProtocol

Expected: FAIL。

- [ ] **Step 5: 实现最小协议模型**

~~~go
const (
    DefaultEndpoint   = "wss://openws.work.weixin.qq.com"
    MarkdownByteLimit = 20480
)

type Headers struct {
    RequestID string `json:"req_id"`
}

type IncomingText struct {
    RequestID string
    MessageID string
    BotID     string
    UserID    string
    ChatType  string
    Content   string
}

type Response struct {
    Headers Headers `json:"headers"`
    ErrCode int     `json:"errcode"`
    ErrMsg  string  `json:"errmsg"`
}

func EncodeSubscribe(requestID, botID, secret string) ([]byte, error)
func EncodeRespondMarkdown(callbackRequestID, content string) ([]byte, error)
func EncodeSendMarkdown(requestID, userID, content string) ([]byte, error)
func EncodePing(requestID string) ([]byte, error)
func DecodeFrame(data []byte) (Frame, error)
~~~

EncodeSendMarkdown 的 body 精确包含：

~~~json
{
  "chatid": "USER_ID",
  "chat_type": 1,
  "msgtype": "markdown",
  "markdown": {"content": "CONTENT"}
}
~~~

- [ ] **Step 6: 实现 pending request 表**

在 protocol.go 内建立并发安全 pendingRequests：

~~~go
type requestResult struct {
    Response Response
    Err      error
}

type pendingRequests struct {
    mu    sync.Mutex
    waits map[string]chan requestResult
}

func (p *pendingRequests) register(requestID string) (<-chan requestResult, error)
func (p *pendingRequests) resolve(response Response) bool
func (p *pendingRequests) cancelAll(err error)
~~~

已超时请求的迟到响应 resolve 返回 false，仅供客户端记录，不触发业务动作。

- [ ] **Step 7: 验证**

Run: go test ./internal/wecom

Expected: PASS。

- [ ] **Step 8: 提交**

~~~sh
git add internal/wecom go.mod go.sum
git commit -m "feat: 添加企业微信长连接协议模型"
~~~

## Task 11：实现企业微信 WebSocket 会话、心跳和重连

**Files:**

- Create: internal/wecom/client.go
- Create: internal/wecom/client_test.go
- Create: internal/wecom/reconnect.go
- Create: internal/wecom/reconnect_test.go

- [ ] **Step 1: 先写退避算法失败测试**

使用固定随机源验证：

- 第 1 次约 1 秒。
- 指数增长且不超过 30 秒。
- 抖动范围在基准值的 80% 至 120%。
- Reset 后回到第 1 次。
- context 取消时等待立即结束。

- [ ] **Step 2: 实现可测试的 Backoff**

~~~go
type Backoff struct {
    attempt int
    min     time.Duration
    max     time.Duration
    random  func() float64
}

func (b *Backoff) Next() time.Duration
func (b *Backoff) Reset()
~~~

- [ ] **Step 3: 先写本地 WebSocket 会话失败测试**

用 httptest.Server 和 websocket.Accept 建立 fake 企业微信：

1. 客户端连接后首先发送 aibot_subscribe。
2. fake 返回 errcode 0 后 Events 才开始交付回调。
3. 回调解析为 IncomingText。
4. RespondMarkdown 复用 callback req_id。
5. SendMarkdown 使用新 req_id，等待对应响应。
6. 每 30 秒心跳；测试注入 10ms ticker，不真实等待。
7. disconnected_event、读错误和关闭都会结束当前 session。
8. 新连接成功订阅后替换旧 session，pending 请求收到 ErrUnavailable。
9. Secret 不出现在错误文本和测试日志捕获中。

- [ ] **Step 4: 定义连接抽象和 Client 对外接口**

~~~go
type Socket interface {
    Read(ctx context.Context) (websocket.MessageType, []byte, error)
    Write(ctx context.Context, typ websocket.MessageType, data []byte) error
    Close(code websocket.StatusCode, reason string) error
}

type DialFunc func(ctx context.Context, endpoint string) (Socket, error)

type Client struct {
    endpoint      string
    botID         string
    secret        string
    allowedUserID string
    events        chan IncomingText
}

// Run 持续维护唯一 WebSocket 连接，直到 context 取消。
func (c *Client) Run(ctx context.Context) error

// Events 返回已经完成协议校验的文本消息流。
func (c *Client) Events() <-chan IncomingText

// RespondMarkdown 回复当前消息回调。
func (c *Client) RespondMarkdown(ctx context.Context, callbackRequestID, content string) error

// SendMarkdown 主动向唯一允许用户推送消息。
func (c *Client) SendMarkdown(ctx context.Context, content string) error
~~~

- [ ] **Step 5: 实现单连接 session**

每个 session 由一个读循环和串行写锁组成：

- 写请求前 register req_id。
- 写失败立即移除 pending。
- 读到普通 response 时 resolve。
- 读到 aibot_msg_callback 时写入有界 events channel；队列满视为当前 session 不健康并触发重连，不静默丢弃用户命令。
- 收到 disconnected_event 返回 ErrUnavailable，交给 Run 重连。
- 心跳 request timeout 视为连接失效。
- Run 在成功订阅后 Reset backoff；同一时刻 c.current 只能指向一个 session。

- [ ] **Step 6: 验证**

Run: go test ./internal/wecom

Expected: PASS。

- [ ] **Step 7: 提交**

~~~sh
git add internal/wecom
git commit -m "feat: 添加企业微信连接与自动重连"
~~~

## Task 12：实现 BridgeService 的入站命令执行

**Files:**

- Create: internal/bridge/service.go
- Create: internal/bridge/service_test.go

- [ ] **Step 1: 定义并实现测试 fake**

service_test.go 内定义：

~~~go
type fakeHerdr struct {
    promptCalls []promptCall
    keyCalls    []keyCall
    readLines   []int
    readResult  herdr.ReadResult
}

type fakeIM struct {
    replies []string
    pushes  []string
}
~~~

所有 fake 方法都记录调用，测试失败时能明确指出是否产生了意外终端输入。

- [ ] **Step 2: 先写身份和幂等失败测试**

覆盖：

- 未授权用户和群聊不调用 parser 之后的任何 Herdr 方法。
- 同一 msgid 重复回调只执行一次 prompt 或 key。
- 空 msgid 被拒绝。
- 未知斜杠命令回复正确用法，不调用 Herdr。
- Herdr degraded 时所有 prompt、key、read 都被暂停。

- [ ] **Step 3: 先写命令执行失败测试**

覆盖完整行为：

- /ls 调用 CreateListSnapshot 并标注当前选择。
- /sel N 使用最近列表快照，成功后清空 PanelBuffer。
- 普通文本先 ValidateSelected，再调用 Prompt(target.TerminalID, text)，成功回复“已发送”。
- 每个 key 别名最终只调用一次 SendKey，且不二次确认。
- /con 调用 ReadRecent(..., 100)，Refresh 后显示第 0 页。
- /pageup 依次读取 200 到 1000，Expand 后显示更早页。
- /pagedn 只读缓存。
- PanelChanged 后缓存重置并提示执行 /con。
- Prompt 和 Key 在调用前先 GetAgent(target.TerminalID)，再用 MatchesAgent 核对 pane、terminal 和 occupant。
- 实时目标不匹配时清空选择和缓存，并提示重新 /ls、/sel。
- 多段 Markdown 第一段使用 RespondMarkdown，其余段使用 SendMarkdown。

- [ ] **Step 4: 运行测试并确认失败**

Run: go test ./internal/bridge -run TestService

Expected: FAIL。

- [ ] **Step 5: 实现 BridgeService**

~~~go
type HerdrAPI interface {
    GetAgent(ctx context.Context, target string) (herdr.AgentInfo, error)
    ReadRecent(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
    Prompt(ctx context.Context, target, text string) error
    SendKey(ctx context.Context, target, key string) error
}

type IMAdapter interface {
    RespondMarkdown(ctx context.Context, callbackRequestID, content string) error
    SendMarkdown(ctx context.Context, content string) error
}

type Service struct {
    registry *session.Registry
    panel    *panel.Buffer
    guard    *policy.Guard
    deduper  *policy.Deduper
}

// SetHerdr 原子设置可用客户端；nil 表示 degraded。
func (s *Service) SetHerdr(client HerdrAPI)

// HandleMessage 处理一条已经解析的企业微信文本回调。
func (s *Service) HandleMessage(ctx context.Context, message wecom.IncomingText)

// InvalidateSelection 清空当前选择和手工分页缓存。
func (s *Service) InvalidateSelection()
~~~

处理顺序必须固定：

1. Authorize。
2. 校验 msgid 并 AddIfNew。
3. Parse。
4. 对需要 Herdr 的动作检查可用状态。
5. 每次输入前 ValidateSelected。
6. 调用 GetAgent 并以 MatchesAgent 重新核对实时目标。
7. key 再执行 ValidateKey。
8. 调用 Prompt 或 SendKey。
9. 发送不含 Secret、完整 prompt 的结果回复。

- [ ] **Step 6: 验证**

Run: go test ./internal/bridge -run TestService

Expected: PASS。

- [ ] **Step 7: 提交**

~~~sh
git add internal/bridge
git commit -m "feat: 添加机器人命令执行服务"
~~~

## Task 13：实现状态通知策略和 Herdr EventSupervisor

**Files:**

- Create: internal/bridge/notifier.go
- Create: internal/bridge/notifier_test.go
- Create: internal/bridge/supervisor.go
- Create: internal/bridge/supervisor_test.go

- [ ] **Step 1: 先写 Notifier 失败测试**

覆盖：

- idle -> working：简短通知，不读取终端。
- working -> blocked：只 ReadRecent(..., 100)。
- working -> done：只 ReadRecent(..., 100)。
- working/blocked -> idle：最多读取 100 行并通知。
- done/idle -> idle：不通知。
- 任意 -> unknown：通知不可靠状态，不宣称完成，不读全部输出。
- 相同 pane、occupant、status 和 100 行快照 hash 不重复发送。
- 自动通知不调用 PanelBuffer，也不改变手工 page。
- ReadRecent 失败仍发送状态标题，但不伪造内容。

- [ ] **Step 2: 实现 Notifier**

~~~go
type Notifier struct {
    im       IMAdapter
    recent   map[string]notificationKey
    read     func(context.Context, string, int) (herdr.ReadResult, error)
}

// HandleTransition 根据状态迁移发送主动通知。
func (n *Notifier) HandleTransition(
    ctx context.Context,
    transition session.Transition,
) error

// TargetInvalidated 通知 pane 关闭或 occupant 替换。
func (n *Notifier) TargetInvalidated(ctx context.Context, target session.Target) error
~~~

通知正文明确写“终端近期快照（最近最多 100 行）”。

- [ ] **Step 3: 先写 Supervisor 失败测试**

使用 fake Herdr API 和可控 SubscriptionStream，覆盖：

- 每次连接先 CheckCompatible，再 Snapshot。
- 初始 snapshot 只建立基线，不发送历史通知。
- 建立一个生命周期 stream 和一个包含全部 Agent pane 的状态 stream。
- pane.created/closed/updated/exited/agent_detected 触发重新 Snapshot。
- Agent pane 集合变化时关闭并重建状态 stream。
- 重复状态事件被 Registry/Notifier 抑制。
- socket 关闭后 SetHerdr(nil)、清空选择与 PanelBuffer，再按退避重连。
- 重连 Replace(..., true)，不恢复选择、分页和旧通知。
- protocol mismatch 不执行 snapshot/subscribe/input，只以较慢退避继续探测。
- context 取消关闭两个 stream。

- [ ] **Step 4: 定义 Supervisor 所需接口**

~~~go
type ManagedHerdr interface {
    HerdrAPI
    CheckCompatible(ctx context.Context) error
    Snapshot(ctx context.Context) (herdr.Snapshot, error)
    Subscribe(ctx context.Context, specs []herdr.SubscriptionSpec) (herdr.SubscriptionStream, error)
}

type HerdrFactory interface {
    Connect(ctx context.Context) (ManagedHerdr, error)
}

type Supervisor struct {
    factory  HerdrFactory
    registry *session.Registry
    service  *Service
    notifier *Notifier
}

// Run 维护 Herdr snapshot、订阅和重连状态机。
func (s *Supervisor) Run(ctx context.Context) error
~~~

- [ ] **Step 5: 实现重连循环**

单次健康周期：

1. factory.Connect。
2. CheckCompatible。
3. Snapshot。
4. registry.Replace(snapshot, true)。
5. service.SetHerdr(client)。
6. 建立 lifecycle stream。
7. 按 registry 当前 pane IDs 建立 status stream。
8. 并发读取两个 stream。
9. lifecycle 事件经 100ms debounce 后重新 Snapshot；Replace(snapshot, false) 返回的目标失效交给 Service 和 Notifier。
10. status 事件先 ApplyStatus，再交给 Notifier。
11. 任一 stream 终止即关闭两者、SetHerdr(nil)、InvalidateSelection 并重连。

protocol mismatch 属于不可进行业务的 degraded 状态，但 supervisor 每 30 秒重新执行一次探测，以便用户升级并重启 Herdr 后自动恢复。

- [ ] **Step 6: 验证**

Run: go test ./internal/bridge

Expected: PASS。

- [ ] **Step 7: 提交**

~~~sh
git add internal/bridge
git commit -m "feat: 添加 Herdr 状态监督与通知"
~~~

## Task 14：组装应用入口、日志和优雅退出

**Files:**

- Create: internal/app/app.go
- Create: internal/app/app_test.go
- Modify: cmd/herdr-pal/main.go

- [ ] **Step 1: 先写 app 组装失败测试**

通过依赖注入覆盖：

- 缺少 -config 返回退出码 2。
- 配置错误不启动任何连接。
- 锁冲突返回清晰错误。
- ResolveSocket 后正确构造 Herdr Client。
- WeCom Run、Herdr Supervisor Run 和消息消费循环共同启动。
- 任一不可恢复错误取消其他 goroutine。
- SIGINT/SIGTERM 对应 context 取消后，先停止收消息，再关闭连接并释放锁。
- 日志只记录 bot/user/pane 的 hash 或非敏感 ID，不出现 Secret、完整 prompt 和完整终端内容。

- [ ] **Step 2: 定义 AppOptions 和 Run**

~~~go
type Options struct {
    ConfigPath string
    Getenv     func(string) string
    Runner     herdr.CommandRunner
    Stdout     io.Writer
    Stderr     io.Writer
}

// Run 加载配置、获取进程锁并运行 bridge，直到 context 取消。
func Run(ctx context.Context, options Options) error
~~~

app.Run 负责：

- config.Load。
- 由 os.UserCacheDir() 生成锁目录和锁文件。
- herdr.ResolveSocket。
- 组装 Registry、Buffer、Guard、Deduper、WeComClient、BridgeService、Notifier 和 Supervisor。
- 使用 log/slog；日志字段不记录 Secret、完整内容和 Cookie。
- 启动三个受同一 context 控制的循环：WeCom、Supervisor、从 WeCom.Events 调用 Service.HandleMessage。
- 最长 10 秒优雅退出，之后返回 timeout 错误。

- [ ] **Step 3: 改造命令入口**

main.go 使用 flag.FlagSet 支持：

~~~text
herdr-pal -config /path/to/config.json
herdr-pal --version
~~~

使用 signal.NotifyContext 捕获 SIGINT/SIGTERM。错误写 stderr，正常取消退出码为 0，配置/参数错误为 2，运行错误为 1。

- [ ] **Step 4: 验证**

Run: go test ./internal/app ./cmd/herdr-pal

Run: ./build.sh

Expected: PASS，dist/herdr-pal 为 CGO_ENABLED=0 的单文件。

- [ ] **Step 5: 提交**

~~~sh
git add internal/app cmd/herdr-pal
git commit -m "feat: 组装 Herdr Pal 应用入口"
~~~

## Task 15：完成 fake 端到端测试、使用文档和最终验收

**Files:**

- Create: internal/testkit/herdr_server.go
- Create: internal/testkit/wecom_server.go
- Create: internal/integration/bridge_test.go
- Modify: README.md
- Modify: docs/HANDOFF_CONTEXT.md
- Modify: docs/HERDR_API_AUDIT.md
- Modify: docs/superpowers/specs/2026-07-23-wecom-smart-bot-mvp-design.md

- [ ] **Step 1: 实现 fake Herdr server**

fake server 监听 t.TempDir() 下的 Unix socket，支持：

- ping。
- session.snapshot。
- agent.get。
- agent.read。
- agent.prompt。
- agent.send_keys。
- events.subscribe 确认。
- 主动注入 lifecycle 和 status 事件。
- 主动断开订阅。
- 记录所有输入调用供断言。

该 fake 只实现公开 protocol 17，不导入或复制 Herdr 私有 Rust 模块。

- [ ] **Step 2: 实现 fake WeCom server**

httptest WebSocket server 支持：

- 校验 aibot_subscribe。
- 返回 req_id 对应的成功/错误响应。
- 注入单聊和群聊回调。
- 记录 aibot_respond_msg、aibot_send_msg 和 ping。
- 主动发送 disconnected_event 或关闭连接。

- [ ] **Step 3: 先写端到端失败测试**

至少实现以下场景：

1. 启动 -> Herdr snapshot -> 企业微信订阅成功。
2. /ls -> /sel 1 -> 普通 prompt，仅产生一次 agent.prompt。
3. /key enter 和 /space 分别只产生一次 agent.send_keys。
4. /con 返回最后 100 行；/pageup 读取 200；/pagedn 不访问 Herdr。
5. blocked 和 done 每次只读取 100 行。
6. 重复 msgid 不重复 prompt/key。
7. 未授权用户和群聊零 Herdr 输入。
8. pane occupant 替换使选择失效。
9. Herdr 断线后输入暂停，重连 snapshot 后要求重新 /ls、/sel。
10. 企业微信断线重连后不重放旧消息或通知。

- [ ] **Step 4: 运行测试并确认失败，再补齐最小实现**

Run: go test ./internal/integration -run TestBridgeEndToEnd

Expected: 首次 FAIL；只修复暴露出的模块边界或编排缺口，不把 fake 专用逻辑放入生产代码。

- [ ] **Step 5: 增加可选真实 Herdr 集成测试入口**

真实测试以环境变量显式启用：

~~~sh
HERDR_PAL_INTEGRATION=1 go test ./internal/integration -run TestRealHerdr
~~~

测试开始先执行 herdr status server --json 并要求 protocol == 17；否则调用 t.Skip，提示升级已安装 Herdr。企业微信侧始终使用 fake server，因此不需要外网和真实 Secret。

- [ ] **Step 6: 更新 README**

README 必须包含：

- 产品边界和安全模型。
- 企业微信智能机器人长连接配置步骤。
- config.json 示例和 HERDR_PAL_WECOM_SECRET。
- 手工启动、停止和 --version。
- 全部内置命令与严格 key 范围。
- /con、/pageup、/pagedn 的 100 行/页、最多 1000 行语义。
- blocked/done 只读取最近 100 行。
- protocol 17 精确兼容要求和检查命令。
- build.sh、unittest.sh 和可选真实集成测试。
- 不将终端快照描述为完整对话或 LLM transcript。

- [ ] **Step 7: 执行完整静态与动态验证**

Run: gofmt -w cmd internal

Run: ./unittest.sh

Run: ./build.sh

Run: git diff --check

Run: go list -deps ./... >/dev/null

Expected:

- 全部单元测试、race test 和 fake 集成测试通过。
- build.sh 生成 dist/herdr-pal。
- git diff --check 无输出。
- 当前若仍为 Herdr protocol 14，只允许真实集成测试 Skip，不得宣称真实 Herdr 联调通过。

- [ ] **Step 8: 手工安全审计**

用 rg 检查：

~~~sh
rg -n 'SECRET|HERDR_PAL_WECOM_SECRET|prompt|terminal content|Cookie' . +  -g '!docs/**' -g '!README.md' -g '!config.example.json'
rg -n 'pane\.send_(text|input)|server\.stop|pane\.close|auto.*approve' internal cmd
~~~

Expected:

- 第一条只命中配置字段名、测试假值或受控错误，不出现真实凭据。
- 第二条无生产代码命中。
- 日志测试证明不会输出完整 prompt 或终端快照。

- [ ] **Step 9: 提交最终实现**

~~~sh
git add README.md docs internal cmd build.sh unittest.sh config.example.json go.mod go.sum
git commit -m "feat: 完成企业微信智能机器人 MVP"
~~~

- [ ] **Step 10: 推送并核对远端**

~~~sh
git push origin main
git status --short --branch
git rev-parse HEAD
git ls-remote origin refs/heads/main
~~~

Expected: 工作区干净，本地 HEAD 与 origin/main 哈希一致。

## 计划自检清单

- [ ] 所有设计范围均映射到至少一个测试：命令、分页、状态、重连、幂等、安全和构建。
- [ ] 没有通过 MCP、plugin startup hook、私有 TUI socket 或 Herdr 私有 Rust 模块实现功能。
- [ ] 所有普通文本只走 agent.prompt，所有 UI 控制只走 agent.send_keys。
- [ ] 所有状态订阅均显式包含 pane_id。
- [ ] 启动和重连总是先 protocol gate，再 session.snapshot。
- [ ] blocked、done 和自动 idle 内容永远最多读取最近 100 行。
- [ ] /con 每次读取最后 100 行并重置 page；/pageup 最多扩大到 1000 行。
- [ ] 按键无需二次确认，但身份、幂等、选择、terminal、occupant 和白名单校验一个不少。
- [ ] 未知 JSON 字段兼容，必要字段和错误响应严格处理。
- [ ] 文档、注释、提交信息符合中文要求；标识符符合英文习惯。
- [ ] 最终提交前 build.sh 与 unittest.sh 均已成功执行。
- [ ] 使用 rg 检查常见未决标记，确保实现和文档中没有遗留项。
