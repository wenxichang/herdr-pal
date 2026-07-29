# 终端图片显示模式实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本项目受 `AGENTS.md` 约束，实施时必须选择 `superpowers:executing-plans`，不得使用 subagent-driven。

**Goal:** 在 Pal 本机把安全 ANSI 终端页渲染为 16px、256 色 PNG8，由 Server 按会话保存 `txt/img` 模式、按状态事件决定是否拉取内容，并通过企业微信发送文字或图片。

**Architecture:** HPRP/1 直接加入终端内容联合类型、状态事件和无副作用快照请求。Pal 始终从同一份 ANSI 快照派生纯文本和图片，交互分页缓存保存配对文本/ANSI；Server 保存模式、生成标题与通知策略，企微适配层负责临时素材上传。状态通知快照与 `/con` 分页缓存完全隔离。

**Tech Stack:** Go 1.26、HPRP/1 JSON over WSS、Herdr protocol 17、`github.com/jiro4989/textimg/v3`、`golang.org/x/image`、`github.com/mattn/go-runewidth`、企业微信智能机器人长连接临时素材接口。

---

## 文件结构

新增文件：

- `internal/terminalimage/renderer.go`：嵌入字体、加载 textimg、限制终端尺寸并生成 PNG8。
- `internal/terminalimage/quantize.go`：确定性的直方图/median-cut 256 色量化器。
- `internal/terminalimage/renderer_test.go`：字体覆盖、ANSI 样式、PNG8、尺寸和上限测试。
- `internal/terminalimage/quantize_test.go`：调色板和索引合法性测试。
- `internal/terminalimage/assets/NotoSansMonoCJKsc-Regular.otf`：Pal 专用内嵌字体。
- `internal/terminalimage/assets/OFL.txt`：Noto 字体许可证。
- `internal/terminalimage/assets/SOURCE.md`：字体来源、版本和 SHA256。

主要修改文件：

- `internal/hprp/messages.go`、`validate.go`、`error.go`：HPRP/1 终端内容、模式、状态事件和快照请求。
- `internal/herdr/client.go`、`types.go`：读取 `recent_unwrapped` ANSI。
- `internal/panel/normalize.go`、`buffer.go`：安全 SGR 与纯文本配对分页。
- `internal/im/model.go`：平台中立终端内容和结构化状态事件。
- `internal/bridge/service.go`：按单次请求模式返回终端内容；提供无副作用快照读取。
- `internal/bridge/notifier.go`：只发送状态/失效事件，不读取终端正文。
- `internal/relayclient/client.go`、`idempotency.go`：HPRP 图片能力、终端结果、快照请求和状态事件。
- `internal/server/connection.go`、`hub.go`：Server→Pal 快照 RPC 和终端联合内容。
- `internal/server/catalog.go`、`router.go`：会话模式、`/mode`、默认模式、状态通知策略和图片发送。
- `internal/wecom/protocol.go`、`client.go`：临时素材三阶段上传和图片消息。
- `internal/app/relay.go`、`internal/serverapp/app.go`：渲染器与图片 Gateway 接线。
- `internal/command/parser.go`、`README.md`：帮助文本和使用说明。

每个提交都执行同一门禁，任一步失败都不得提交：

```bash
./unittest.sh
./build.sh
git diff --check
```

任务中列出的包级测试用于 TDD 的红/绿循环，不能替代上述提交门禁。

## Task 1：实现 HPRP/1 终端内容与快照消息

**Files:**

- Modify: `internal/hprp/messages.go`
- Modify: `internal/hprp/validate.go`
- Modify: `internal/hprp/error.go`
- Modify: `internal/hprp/messages_test.go`
- Modify: `internal/hprp/validate_test.go`
- Modify: `internal/hprp/negotiate_test.go`

- [ ] **Step 1：先写协议类型和严格校验失败测试**

在 `internal/hprp/validate_test.go` 增加表驱动测试，至少覆盖：

```go
func TestValidateCommandExecuteRequiresOutputMode(t *testing.T) {
	command := validCommandExecute()
	command.OutputMode = ""
	if err := ValidateCommandExecute(command); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("ValidateCommandExecute() error = %v", err)
	}
}

func TestValidateTerminalContentRequiresPairedTextAndValidPNG(t *testing.T) {
	content := validTerminalImageContent(t)
	content.Text = string([]byte{0xff})
	if err := ValidateContent(content); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid text error = %v", err)
	}
	content = validTerminalImageContent(t)
	content.Image.Data = base64.StdEncoding.EncodeToString([]byte("not png"))
	if err := ValidateContent(content); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("invalid png error = %v", err)
	}
}

func TestValidateNotificationEventRejectsTerminalBody(t *testing.T) {
	event := validStatusEvent()
	event.Kind = NotificationKindAgentStatusChanged
	if err := ValidateNotificationEvent(event); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTerminalSnapshotResultAllowsSameReadFallback(t *testing.T) {
	result := TerminalSnapshotResult{
		Outcome: OutcomeFailed,
		Target: validTarget(),
		FallbackContent: pointerTo(validTerminalTextContent()),
		Error: &Error{Code: CodeTerminalImageFailed, Retryable: false},
	}
	if err := ValidateTerminalSnapshotResult(result); err != nil {
		t.Fatal(err)
	}
}
```

同步增加非法模式、图片超 512 KiB、文本超 256 KiB、宽高与 PNG 实际尺寸不一致、非法
Base64、`snapshot_sequence == 0`、状态未变化、非法 `purpose/max_lines` 和失败结果错误码
组合测试。

- [ ] **Step 2：运行 HPRP 测试，确认因新类型不存在而失败**

Run:

```bash
go test ./internal/hprp -run 'TestValidate(CommandExecuteRequiresOutputMode|TerminalContent|NotificationEvent|TerminalSnapshotResult)' -count=1
```

Expected: FAIL，错误包含未定义的 `OutputMode`、`TerminalSnapshotResult` 或校验函数。

- [ ] **Step 3：增加 HPRP 类型和常量**

在 `internal/hprp/messages.go` 增加以下公共合同，字段名必须与协议文档一致：

```go
const (
	TypeTerminalSnapshotGet    Type = "terminal.snapshot.get"
	TypeTerminalSnapshotResult Type = "terminal.snapshot.result"

	CapabilityTerminalSnapshotV1 = "terminal.snapshot.v1"
	CapabilityTerminalImageV1    = "terminal.image.v1"

	ContentTypeTerminal = "terminal.snapshot"

	NotificationKindAgentStatusChanged = "agent.status.changed"
	NotificationKindTargetInvalidated   = "target.invalidated"
	TerminalSnapshotPurposeNotification = "notification"
)

type OutputMode string

const (
	OutputModeText  OutputMode = "txt"
	OutputModeImage OutputMode = "img"
)

type TerminalImage struct {
	MediaType string `json:"media_type"`
	Encoding  string `json:"encoding"`
	Data      string `json:"data"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	ColorMode string `json:"color_mode"`
}

type TerminalPage struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

type Content struct {
	Type       string         `json:"type"`
	Text       string         `json:"text"`
	Mode       OutputMode     `json:"mode,omitempty"`
	Image      *TerminalImage `json:"image,omitempty"`
	Page       *TerminalPage  `json:"page,omitempty"`
	CapturedAt *time.Time     `json:"captured_at,omitempty"`
}

type StatusChangeData struct {
	PreviousStatus string `json:"previous_status"`
	Status         string `json:"status"`
}

type NotificationEvent struct {
	EventKey        string           `json:"event_key"`
	Sequence        uint64           `json:"sequence"`
	Kind            string           `json:"kind"`
	Target          Target           `json:"target"`
	SnapshotSequence uint64          `json:"snapshot_sequence"`
	OccurredAt      time.Time        `json:"occurred_at"`
	Data            *StatusChangeData `json:"data,omitempty"`
}

type TerminalSnapshotGet struct {
	Target   Target     `json:"target"`
	Mode     OutputMode `json:"mode"`
	Purpose  string     `json:"purpose"`
	MaxLines int        `json:"max_lines"`
}

type TerminalSnapshotResult struct {
	Outcome         Outcome  `json:"outcome"`
	Target          Target   `json:"target"`
	Content         *Content `json:"content,omitempty"`
	FallbackContent *Content `json:"fallback_content,omitempty"`
	Error           *Error   `json:"error,omitempty"`
}
```

把 `CommandExecute.OutputMode` 加入消息，把 `CommandResult.Content` 和
`CommandOutput.Content` 改为 `Content`。为保证分阶段提交始终可编译，先把
`TextContent` 定义为 `Content` 的类型别名；文本输入校验必须拒绝 mode、image、page 和
captured_at。`ServerLimits` 增加 `MaxTerminalTextBytes` 与 `MaxTerminalImageBytes`。

Task 1 只提供可迁移的协议骨架：`output_mode` 和新的通知字段先允许缺省，旧
`notification.event.content` 暂时保留。Task 6、7 完成 Pal/Server 双端迁移，Task 8 再启用
严格必填校验并删除旧字段；最终代码不保留兼容分支。终端 `text` 字段必须是合法 UTF-8，
但允许空字符串，以正确表达空白终端页。

- [ ] **Step 4：实现严格校验**

在 `internal/hprp/validate.go` 增加：

```go
const (
	MaxTerminalTextBytes  = 1 << 18
	MaxTerminalImageBytes = 1 << 19
	MaxTerminalDimension  = 16384
)

func ValidateContent(content Content) error
func ValidateTerminalSnapshotGet(request TerminalSnapshotGet) error
func ValidateTerminalSnapshotResult(result TerminalSnapshotResult) error
func validOutputMode(mode OutputMode) bool
```

`ValidateContent` 对 `text/plain` 复用文本限制；对 `terminal.snapshot` 必须验证 UTF-8、
实际模式、分页、RFC3339 对应的非零 `time.Time`、Base64 解码前长度、PNG 签名以及
`png.DecodeConfig` 得到的宽高。失败结果只有 `terminal.image_failed` 可以携带
`mode: txt` 的 `FallbackContent`，且不能同时携带成功 `Content`。

`agent.status.changed` 必须携带非空 `data`，且旧/新状态不同；`target.invalidated` 不携带
状态 data，也不允许终端正文。迁移阶段 validator 可识别旧通知，严格删除点仍在 Task 8。

在 `internal/hprp/error.go` 增加四个已确认错误码：

```go
CodeTerminalSnapshotUnsupported ErrorCode = "terminal.snapshot_unsupported"
CodeTerminalSnapshotFailed      ErrorCode = "terminal.snapshot_failed"
CodeTerminalImageUnsupported    ErrorCode = "terminal.image_unsupported"
CodeTerminalImageFailed         ErrorCode = "terminal.image_failed"
```

- [ ] **Step 5：运行 HPRP 全包测试**

Run:

```bash
go test ./internal/hprp -count=1
```

Expected: PASS。

- [ ] **Step 6：提交协议模型**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/hprp
git commit -m "feat: 扩展 HPRP 终端内容协议"
```

## Task 2：读取安全 ANSI 并维护配对分页

**Files:**

- Modify: `internal/herdr/client.go`
- Modify: `internal/herdr/client_test.go`
- Modify: `internal/herdr/types.go`
- Modify: `internal/panel/normalize.go`
- Modify: `internal/panel/normalize_test.go`
- Modify: `internal/panel/buffer.go`
- Modify: `internal/panel/buffer_test.go`

- [ ] **Step 1：写 ANSI 请求和安全清理失败测试**

增加以下核心测试：

```go
func TestClientReadRecentANSIUsesPublicAgentRead(t *testing.T) {
	client := newBusinessTestClient(t, ansiPaneReadResponse(), businessRequestCheck("agent.read", map[string]any{
		"target": "p1", "source": "recent_unwrapped", "lines": float64(42),
		"format": "ansi", "strip_ansi": false,
	}))
	result, err := client.ReadRecentANSI(context.Background(), "p1", 42)
	if err != nil || !strings.Contains(result.Text, "\x1b[31m") {
		t.Fatalf("ReadRecentANSI() = %#v, %v", result, err)
	}
}

func TestNormalizeANSIKeepsSGRAndRemovesUnsafeControls(t *testing.T) {
	raw := "\x1b]0;secret\x07\x1b[31m红色\x1b[0m\x1b[2J\n" + strings.Repeat("─", 10)
	lines := NormalizeANSI(raw)
	if lines[0].Text != "红色" || lines[0].ANSI != "\x1b[31m红色\x1b[0m" {
		t.Fatalf("line = %#v", lines[0])
	}
	if lines[1].Text != "──────" || strings.Contains(lines[1].ANSI, "]0;") {
		t.Fatalf("line = %#v", lines[1])
	}
}

func TestBufferPagesTextAndANSIWithSameAnchor(t *testing.T) {
	var buffer Buffer
	buffer.RefreshTerminal("session", numberedTerminalLines(101, 200))
	if err := buffer.ExpandTerminal("session", numberedTerminalLines(1, 200)); err != nil {
		t.Fatal(err)
	}
	page := buffer.RenderTerminal()
	if page.Lines[0].Text != "line-001" || !strings.Contains(page.Lines[0].ANSI, "line-001") {
		t.Fatalf("page = %#v", page)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/herdr ./internal/panel -run 'Test(ClientReadRecentANSI|NormalizeANSI|BufferPagesTextAndANSI)' -count=1
```

Expected: FAIL，新 API 尚不存在。

- [ ] **Step 3：增加 Herdr ANSI 读取**

在 `internal/herdr/client.go` 增加 `ReadRecentANSI`，复用现有参数与响应校验，仅固定：

```go
params := agentReadParams{
	Target: target, Source: "recent_unwrapped", Lines: lines,
	Format: "ansi", StripANSI: false,
}
```

响应必须仍为 `source == recent_unwrapped`、`format == ansi`。保留现有 `ReadRecent`，直到
Bridge 迁移完成，避免把协议改造和调用迁移混成一步。

- [ ] **Step 4：实现配对行和安全 ANSI 清理**

在 `internal/panel/normalize.go` 定义：

```go
type Line struct {
	Text string
	ANSI string
}

func NormalizeANSI(raw string) []Line
func JoinText(lines []Line) string
func JoinANSI(lines []Line) string
```

清理器只保留最终字节为 `m` 的 CSI SGR，移除 OSC、DCS、APC、PM、SOS、光标移动、擦屏、
超链接和 NUL。处理 CRLF 后，每个逻辑行只保留最后一次 `\r` 重绘段，并在 ANSI 行首补
`\x1b[0m`，避免丢弃前段时继承未知样式。横线折叠必须同时作用于 Text 与 ANSI 可见字符。

- [ ] **Step 5：扩展 Buffer 而不破坏文本调用方**

`Buffer` 内部改存 `[]Line`，保留已有 `Refresh(string, []string)`、`Expand(string, []string)`
和 `Render() []string` 兼容包装，同时增加：

```go
type Page struct {
	Lines   []Line
	Current int
	Total   int
}

func (b *Buffer) RefreshTerminal(targetKey string, lines []Line)
func (b *Buffer) ExpandTerminal(targetKey string, snapshot []Line) error
func (b *Buffer) RenderTerminal() Page
```

锚点和滚动前移判断只比较 `Line.Text`，返回页同时复制 Text 与 ANSI，防止调用方修改缓存。

- [ ] **Step 6：运行 Herdr 与 Panel 全包测试**

```bash
go test ./internal/herdr ./internal/panel -count=1
```

Expected: PASS，原有分页测试继续通过。

- [ ] **Step 7：提交 ANSI 与分页模型**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/herdr internal/panel
git commit -m "feat: 保存配对的终端文本与 ANSI"
```

## Task 3：实现 Pal 端 textimg PNG8 渲染器

**Files:**

- Create: `internal/terminalimage/renderer.go`
- Create: `internal/terminalimage/quantize.go`
- Create: `internal/terminalimage/renderer_test.go`
- Create: `internal/terminalimage/quantize_test.go`
- Create: `internal/terminalimage/assets/NotoSansMonoCJKsc-Regular.otf`
- Create: `internal/terminalimage/assets/OFL.txt`
- Create: `internal/terminalimage/assets/SOURCE.md`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1：加入字体资产和来源记录**

从已验证实验资产复制：

```bash
mkdir -p internal/terminalimage/assets
cp /tmp/herdr-pal-term-candidates.RuXtmu/mono-bench/NotoSansMonoCJKsc-Regular.otf internal/terminalimage/assets/
shasum -a 256 internal/terminalimage/assets/NotoSansMonoCJKsc-Regular.otf
```

Expected SHA256:

```text
ec04cc376b34887cedbdf84074e2e226ed2761eeabdcb9173fc1dd7bfd153ef7
```

`SOURCE.md` 写明字体名称、Google Noto CJK 官方来源 URL、文件 SHA256；`OFL.txt` 使用官方
OFL 1.1 正文。

- [ ] **Step 2：写渲染器失败测试**

```go
func TestRendererProducesIndexedPNGWithCJKAndANSI(t *testing.T) {
	renderer, err := New()
	if err != nil { t.Fatal(err) }
	result, err := renderer.Render(context.Background(), "\x1b[7m当前选项 中文 ABC 123\x1b[0m")
	if err != nil { t.Fatal(err) }
	decoded, err := png.Decode(bytes.NewReader(result.PNG))
	if err != nil { t.Fatal(err) }
	if _, ok := decoded.(*image.Paletted); !ok {
		t.Fatalf("decoded = %T", decoded)
	}
	if result.Width <= 0 || result.Height != 17 {
		t.Fatalf("size = %dx%d", result.Width, result.Height)
	}
}

func TestRendererRejectsUnboundedScreen(t *testing.T) {
	renderer, _ := New()
	_, err := renderer.Render(context.Background(), strings.Repeat("x", 301))
	if !errors.Is(err, ErrScreenTooLarge) { t.Fatalf("error = %v", err) }
}
```

- [ ] **Step 3：运行测试确认失败**

```bash
go test ./internal/terminalimage -count=1
```

Expected: FAIL，包尚不存在。

- [ ] **Step 4：添加依赖并实现 Renderer**

```bash
go get github.com/jiro4989/textimg/v3@v3.2.0
go get github.com/mattn/go-runewidth@v0.0.27
go get golang.org/x/image@v0.44.0
```

`renderer.go` 对外接口固定为：

```go
var (
	ErrScreenTooLarge = errors.New("终端图片尺寸超过限制")
	ErrImageTooLarge  = errors.New("终端 PNG 超过限制")
)

type Result struct {
	PNG    []byte
	Width  int
	Height int
}

type Renderer struct {
	mu   sync.Mutex
	face font.Face
}

func New() (*Renderer, error)
func (r *Renderer) Render(ctx context.Context, safeANSI string) (Result, error)
```

使用 `//go:embed assets/NotoSansMonoCJKsc-Regular.otf`，16px 字体、8px cell width、17px
cell height、最多 300 列和 100 行。调用 `parser.Parse` 与 `textimage.Image.Draw` 后先编码
中间 PNG、再解码为 `image.Image`，交给本地量化器生成最多 256 色 `image.Paletted`，最后
使用 `png.DefaultCompression`。输出超过 512 KiB 返回 `ErrImageTooLarge`。

- [ ] **Step 5：实现确定性 256 色量化器**

从已验证实验代码提取以下纯函数到 `quantize.go`：

```go
func histogramQuantize(input image.Image, maxColors int) *image.Paletted
func medianCutPalette(samples []colorSample, maxColors int) color.Palette
func nearestPaletteIndex(value color.RGBA, palette color.Palette) uint8
```

颜色盒按 `range * pixel count` 选择，按累计像素中位数切分，代表色按像素权重平均。相同
输入必须得到相同调色板顺序，禁止依赖 map 遍历顺序；构建 samples 后先按 RGBA key 排序。

- [ ] **Step 6：运行渲染测试和基准**

```bash
go test ./internal/terminalimage -count=1
go test ./internal/terminalimage -run '^$' -bench BenchmarkRenderer -benchmem -count=3
```

Expected: 单测 PASS；基准输出耗时和分配，不设置易受机器影响的硬耗时断言。

- [ ] **Step 7：提交渲染器与字体**

```bash
./unittest.sh
./build.sh
git diff --check
git add go.mod go.sum internal/terminalimage
git commit -m "feat: 添加 Pal 终端 PNG8 渲染器"
```

## Task 4：让 Bridge 返回结构化终端内容

**Files:**

- Modify: `internal/im/model.go`
- Modify: `internal/im/model_test.go`
- Modify: `internal/session/registry.go`
- Modify: `internal/session/registry_test.go`
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/service_test.go`

- [ ] **Step 1：写模式输出、分页不变和无副作用快照测试**

测试必须覆盖：

```go
func TestServiceImageConReturnsSamePageTextAndPNG(t *testing.T)
func TestServiceModeSwitchDoesNotResetPageDownCache(t *testing.T)
func TestServiceNotificationSnapshotDoesNotChangeInteractivePage(t *testing.T)
func TestServiceImageRenderFailureReturnsExplicitCommandError(t *testing.T)
func TestRegistryResolveTargetDoesNotChangeSelection(t *testing.T)
```

测试 fake renderer 记录收到的 ANSI，并返回固定 PNG；fake IM 同时实现 Markdown 与终端
接口。断言 `TerminalContent.Text` 等于当前页纯文本、图片由当前页 ANSI 生成、页码一致，
调用 `ReadTerminalSnapshot` 前后 `/pagedn` 结果不变。

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/im ./internal/session ./internal/bridge -run 'Test(ServiceImage|ServiceModeSwitch|ServiceNotificationSnapshot|RegistryResolveTarget)' -count=1
```

Expected: FAIL，新接口尚不存在。

- [ ] **Step 3：定义平台中立终端内容**

在 `internal/im/model.go` 增加：

```go
type OutputMode string

const (
	OutputModeText  OutputMode = "txt"
	OutputModeImage OutputMode = "img"
)

type TerminalImage struct {
	MediaType string
	Data      []byte
	Width     int
	Height    int
	ColorMode string
}

type TerminalPage struct {
	Current int
	Total   int
}

type TerminalContent struct {
	Mode       OutputMode
	Text       string
	Image      *TerminalImage
	Page       *TerminalPage
	CapturedAt time.Time
}

type TerminalReplySink interface {
	RespondTerminal(ctx context.Context, requestID string, content TerminalContent) error
	SendTerminal(ctx context.Context, content TerminalContent) error
}
```

`IncomingText` 增加 `OutputMode OutputMode`。没有填写时按文本处理。

- [ ] **Step 4：增加只读目标解析与 Bridge 渲染依赖**

`session.Registry` 增加：

```go
func (r *Registry) ResolveTarget(paneID, occupantKey string) (Target, error)
```

该方法只读复核目标，不修改 `selectedKey`、`listSnapshot` 或分页状态。

`bridge.Service` 增加：

```go
type TerminalRenderer interface {
	Render(ctx context.Context, safeANSI string) (terminalimage.Result, error)
}

func (s *Service) SetTerminalRenderer(renderer TerminalRenderer) error

func (s *Service) ReadTerminalSnapshot(
	ctx context.Context,
	paneID, occupantKey string,
	mode im.OutputMode,
	maxLines int,
) (im.TerminalContent, error)
```

保持 `NewService` 原有签名，避免 Bridge 与应用接线必须在同一提交完成。只有 Relay 应用在
Task 10 调用 `SetTerminalRenderer`；重复设置或 nil renderer 返回明确依赖错误。

`ReadTerminalSnapshot` 使用 `Registry.ResolveTarget`，调用 `ReadRecentANSI`，读取后再次复核
目标，使用 `panel.NormalizeANSI`，截取最后 `maxLines`，按模式渲染；整个过程不访问
`s.panel`、`panelReady`、`page` 或 `generation`。

- [ ] **Step 5：迁移交互分页读取**

把 `/con`、`/pageup` 和 `/key` 后刷新改为 `ReadRecentANSI`、`NormalizeANSI`、
`RefreshTerminal/ExpandTerminal`。`/pagedn` 只调用 `RenderTerminal`。

`replyPage` 构造 `im.TerminalContent`：

```go
content := im.TerminalContent{
	Mode: mode,
	Text: panel.JoinText(page.Lines),
	Page: &im.TerminalPage{Current: page.Current, Total: page.Total},
	CapturedAt: time.Now().UTC(),
}
```

图片模式使用 `panel.JoinANSI(page.Lines)` 调 renderer。IM 实现 `TerminalReplySink` 时，文本
和图片模式都走结构化接口；普通本地 IM 只在文本模式回退到现有 Markdown 页面。图片模式
缺少 renderer 或终端 sink 时返回“当前连接不支持终端图片”，不修改任何状态。

- [ ] **Step 6：运行 Bridge 全包测试**

```bash
go test ./internal/im ./internal/session ./internal/bridge -count=1
```

Expected: PASS。

- [ ] **Step 7：提交结构化终端输出**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/im internal/session internal/bridge
git commit -m "feat: 让 Bridge 返回结构化终端页"
```

## Task 5：把 Pal 通知改成纯状态事件

**Files:**

- Modify: `internal/im/model.go`
- Modify: `internal/bridge/notifier.go`
- Modify: `internal/bridge/notifier_test.go`
- Modify: `internal/bridge/supervisor_test.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/app/relay.go`
- Modify: `internal/app/relay_test.go`

- [ ] **Step 1：写“不读取终端正文”的失败测试**

```go
func TestNotifierStatusTransitionSendsEventWithoutReadingTerminal(t *testing.T) {
	sink := &statusEventRecorder{}
	notifier, err := NewNotifier(sink, matchingNotificationAgent)
	if err != nil { t.Fatal(err) }
	err = notifier.HandleTransition(context.Background(), session.Transition{
		Target: targetWithStatus(herdr.AgentStatusDone),
		Previous: herdr.AgentStatusWorking,
		Current: herdr.AgentStatusDone,
	})
	if err != nil { t.Fatal(err) }
	event := sink.SingleEvent(t)
	if event.Kind != im.NotificationKindAgentStatusChanged || event.PreviousStatus != "working" || event.Status != "done" {
		t.Fatalf("event = %#v", event)
	}
}
```

删除测试 fake reader 作为必要依赖，并增加 `target.invalidated` 结构化事件测试。

- [ ] **Step 2：运行测试确认旧实现仍读取快照或构造文本**

```bash
go test ./internal/bridge -run 'TestNotifierStatusTransitionSendsEventWithoutReadingTerminal' -count=1
```

Expected: FAIL。

- [ ] **Step 3：定义结构化通知事件接口**

在 `internal/im/model.go` 增加：

```go
const (
	NotificationKindAgentStatusChanged = "agent.status.changed"
	NotificationKindTargetInvalidated   = "target.invalidated"
)

type NotificationEvent struct {
	Kind           string
	PreviousStatus string
	Status         string
	OccurredAt     time.Time
}

type NotificationSink interface {
	SendNotification(ctx context.Context, target NotificationTarget, event NotificationEvent) error
}
```

- [ ] **Step 4：简化 Notifier**

`NewNotifier` 改为只接收 `adapter` 和 `GetAgentFunc`。`HandleTransition` 保留 occupant 前置
复核、去重、Dispatcher 合并和重试，但不调用 `agent.read`，不计算 `snapshotHash`，不构造
终端分页。Server 展示文案所需的旧/新状态全部放入 `im.NotificationEvent`。

`TargetInvalidated` 发送 `target.invalidated`；非 Relay fallback sink 可在本地把事件渲染为
现有简短 Markdown，确保 `-i` 和旧的本地运行路径仍可观察状态。

同一任务内同步修改所有 `NewNotifier` 调用点，使该提交完成后全仓仍可构建；Renderer 的
Relay 注入留到 Task 10。

- [ ] **Step 5：运行 Notifier 与 Supervisor 测试**

```bash
go test ./internal/bridge -run 'Test(Notifier|NotificationDispatcher|Supervisor.*Notification)' -count=1
```

Expected: PASS，所有测试 fake 不再检查终端读取次数或多段快照进度。

- [ ] **Step 6：提交状态事件改造**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/im internal/bridge internal/app
git commit -m "refactor: 让 Pal 只上报 Agent 状态事件"
```

## Task 6：改造 HPRP Pal 客户端

**Files:**

- Modify: `internal/relayclient/client.go`
- Modify: `internal/relayclient/idempotency.go`
- Modify: `internal/relayclient/client_hprp_test.go`
- Modify: `internal/relayclient/client_test.go`
- Modify: `internal/relayclient/idempotency_test.go`

- [ ] **Step 1：写能力、终端结果、状态事件和快照请求失败测试**

增加以下端到端 fake WebSocket 测试：

```go
func TestClientAdvertisesTerminalCapabilities(t *testing.T)
func TestClientMapsImageTerminalReplyToCommandResult(t *testing.T)
func TestClientStatusEventContainsConfirmedSnapshotSequenceAndNoContent(t *testing.T)
func TestClientHandlesTerminalSnapshotGetWithoutChangingSelection(t *testing.T)
func TestClientTerminalSnapshotImageFailureReturnsSameReadFallback(t *testing.T)
```

断言图片 HPRP `Content.Text` 与 fake executor 返回文本一致，`Image.Data` 可解码；状态事件
JSON 不含 `content`；快照请求前后 `SelectedTarget()` 不变。

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/relayclient -run 'TestClient(AdvertisesTerminal|MapsImage|StatusEvent|HandlesTerminal|TerminalSnapshot)' -count=1
```

Expected: FAIL。

- [ ] **Step 3：扩展 Executor 和命令缓存**

```go
type Executor interface {
	CurrentTargets() []session.Target
	SelectedTarget() (session.Target, error)
	SelectTarget(paneID, occupantHash string) error
	HandleMessage(ctx context.Context, message im.IncomingText)
	ReadTerminalSnapshot(ctx context.Context, paneID, occupantHash string, mode im.OutputMode, maxLines int) (im.TerminalContent, error)
}
```

`cachedCommandResult.outputs` 改为 `[]hprp.Content`。Client 实现 `RespondTerminal` 和
`SendTerminal`，把二进制 PNG 编码为 Base64；Markdown 回复映射为 `Content{Type:
text/plain}`。

- [ ] **Step 4：协商能力和传递单次 output_mode**

`hello.client` 声明：

```go
[]string{
	hprp.CapabilityCommandOutputV1,
	hprp.CapabilityTerminalSnapshotV1,
	hprp.CapabilityTerminalImageV1,
}
```

`handleCommand` 把 `command.OutputMode` 转换为 `im.IncomingText.OutputMode`。图片命令未协商
能力时返回 `terminal.image_unsupported`，Pal 不保存该模式。

- [ ] **Step 5：发送结构化状态事件**

`clientSession` 增加原子 `snapshotSequence`，初始快照和后续快照收到确认后更新。Relay
Client 的 `SendNotification` 把 `im.NotificationEvent` 映射成 HPRP 事件，使用当前已确认
序号、UTC 时间和稳定 Target，不再读取或传输 Markdown 正文。

- [ ] **Step 6：处理 Server 发起的无副作用快照请求**

`readLoop` 新增：

```go
case hprp.TypeTerminalSnapshotGet:
	go client.handleTerminalSnapshot(current, envelope)
```

handler 校验 capability、Target、purpose 和 max lines，调用 Executor 的只读方法，返回
一次 `terminal.snapshot.result`。图片渲染失败且 Executor 返回同次读取的文本时，使用
`OutcomeFailed + CodeTerminalImageFailed + FallbackContent`。

本任务删除 Pal 发出的旧通知正文；接收命令时仍把缺省 `output_mode` 暂按 txt 处理，直到
Task 7 完成 Server 发送侧迁移。`target.invalidated` 使用已确认 snapshot sequence，但不带
状态 data。

- [ ] **Step 7：运行 Relay Client 全包测试**

```bash
go test ./internal/relayclient -count=1
```

Expected: PASS。

- [ ] **Step 8：提交 Pal HPRP 改造**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/relayclient
git commit -m "feat: 支持 HPRP 终端图片与快照请求"
```

## Task 7：实现 Server 快照 RPC 和会话模式存储

**Files:**

- Modify: `internal/server/connection.go`
- Modify: `internal/server/connection_test.go`
- Modify: `internal/server/hub.go`
- Modify: `internal/server/hub_hprp_test.go`
- Modify: `internal/server/hub_test.go`
- Modify: `internal/server/catalog.go`
- Modify: `internal/server/catalog_test.go`

- [ ] **Step 1：写快照 RPC、模式生命周期和替换迁移失败测试**

```go
func TestHPRPHubFetchTerminalSnapshotRoutesAndValidatesTarget(t *testing.T)
func TestHPRPHubRejectsImageSnapshotWithoutCapability(t *testing.T)
func TestCatalogStoresOutputModePerFullTarget(t *testing.T)
func TestCatalogMigratesExplicitModeAcrossSameSlotReplacement(t *testing.T)
func TestCatalogDropsModesOnSessionRemovalAndDetach(t *testing.T)
```

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/server -run 'Test(HPRPHubFetchTerminal|HPRPHubRejectsImage|Catalog.*Mode)' -count=1
```

Expected: FAIL。

- [ ] **Step 3：增加连接能力与 Server→Pal 请求**

Server capabilities 加入 `terminal.snapshot.v1` 和 `terminal.image.v1`；hello limits 填写
256 KiB 文本与 512 KiB 图片。

`ClientHub` 增加：

```go
func (hub *ClientHub) SupportsCapability(userID string, target hprp.Target, capability string) bool

func (hub *ClientHub) FetchTerminalSnapshot(
	ctx context.Context,
	userID string,
	target hprp.Target,
	mode hprp.OutputMode,
	maxLines int,
) (hprp.TerminalSnapshotResult, error)
```

它复核目录和 capability，使用 `clientConnection.request` 发送
`terminal.snapshot.get`。`readLoop` 收到 `terminal.snapshot.result` 时严格校验 Target、
能力和 payload，再调用 `connection.deliver`。未匹配响应视为协议错误并断开。

- [ ] **Step 4：把 command.execute 和结果改成联合内容**

`Execute` 从 `im.IncomingText.OutputMode` 生成 HPRP `OutputMode`；空值按 txt。为保证 Router
在下一任务迁移前仍可编译，先保留旧文本字段并增加结构化字段：

```go
type RelayExecution struct {
	Content           string
	StructuredContent *hprp.Content
	SelectedTarget    *hprp.Target
}
```

`SendCommandOutput` 继续复核稳定目标，但把完整 `hprp.Content` 交给 Router；Task 8 完成
Router 迁移后删除旧 `Content string`。

`target.invalidated` 允许目录中的目标已经消失：Hub 仍须验证 machine 与当前连接一致、
snapshot sequence 已确认且事件幂等，但不得要求 `ResolveTarget` 成功，也不得发起终端快照。
增加会话已经从目录移除时仍可转发失效事件的测试。

- [ ] **Step 5：保存并迁移显式模式**

`routingState` 增加：

```go
outputModes map[hprp.Target]hprp.OutputMode
```

`SessionCatalog` 增加：

```go
func (catalog *SessionCatalog) SetOutputMode(userID string, target hprp.Target, mode hprp.OutputMode) error
func (catalog *SessionCatalog) OutputMode(userID string, target hprp.Target) (hprp.OutputMode, bool, error)
```

`bool` 表示是否有显式覆盖。应用新快照时，同一 machine/slot 恰好从旧 session 替换为新
session，就迁移显式模式；会话消失、机器 Detach 时清理。删除后不保留孤立条目。

- [ ] **Step 6：运行 Server 连接与目录测试**

```bash
go test ./internal/server -run 'Test(HPRPHub|ClientConnection|Catalog)' -count=1
```

Expected: PASS。

- [ ] **Step 7：提交 Server RPC 与模式存储**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/server/connection.go internal/server/connection_test.go internal/server/hub.go internal/server/hub_hprp_test.go internal/server/hub_test.go internal/server/catalog.go internal/server/catalog_test.go
git commit -m "feat: 添加服务端终端快照与会话模式"
```

## Task 8：实现 `/mode`、终端展示和服务端通知策略

**Files:**

- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`
- Modify: `internal/server/activity.go`
- Modify: `internal/server/activity_test.go`
- Modify: `internal/panel/split.go`
- Modify: `internal/panel/split_test.go`

- [ ] **Step 1：写模式命令和通知拉取失败测试**

```go
func TestRouterModeUsesSelectedSessionOnly(t *testing.T)
func TestRouterDirectedModeSwitchesOnlyAfterSuccess(t *testing.T)
func TestRouterHashDirectedModeKeepsSelection(t *testing.T)
func TestRouterDefaultsOpenCodeToImageAndOthersToText(t *testing.T)
func TestRouterStatusDoneFetchesSnapshotUsingCurrentMode(t *testing.T)
func TestRouterRecentBackgroundActivitySendsShortNoticeWithoutFetching(t *testing.T)
func TestRouterSnapshotFailureStillSendsStatusNotification(t *testing.T)
func TestRouterImageNotificationUsesSameReadTextFallback(t *testing.T)
func TestRouterPageUpAndPageDownKeepModeAndPageMetadata(t *testing.T)
func TestRouterTargetInvalidatedDoesNotFetchSnapshot(t *testing.T)
```

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/server -run 'TestRouter(Mode|DirectedMode|HashDirectedMode|DefaultsOpenCode|StatusDone|RecentBackground|SnapshotFailure|ImageNotification|PageUp)' -count=1
```

Expected: FAIL。

- [ ] **Step 3：解析并执行模式命令**

新增 `serverActionMode`，`serverAction` 增加 `mode hprp.OutputMode`。解析规则：

```text
/mode img
/mode txt
/N /mode img
#N /mode txt
```

定向解析只额外允许嵌套 mode；仍禁止 `/N`、`/sel`、`/ls`、`/help` 和 `/userid`。显式
`img` 必须检查目标连接已协商 `terminal.image.v1`。`/N` 只有设置成功后才切换；`#N`
始终不切换。模式命令只返回确认，不自动执行 `/con`。

- [ ] **Step 4：计算每次命令的有效模式**

实现：

```go
func (router *ConversationRouter) effectiveOutputMode(userID string, entry CatalogEntry) hprp.OutputMode
```

优先使用 Catalog 显式覆盖；没有覆盖时，Agent 或 DisplayAgent 与 `opencode` 大小写无关
相等则默认 img，否则 txt。默认 img 遇到未协商图片能力时静默使用 txt。每次 Select 后
自动 `/con`、普通执行和 directed execute 都把有效模式写入 `IncomingText.OutputMode`。

- [ ] **Step 5：统一发送文本与图片终端内容**

保持现有 `WeComGateway` 不变，另外定义可选图片能力，避免 Router 迁移时要求所有旧 fake
立即实现图片上传：

```go
type WeComImageGateway interface {
	SendImageTo(ctx context.Context, userID string, png []byte) error
}
```

Router 增加 `sendTerminalReply`、`sendTerminalPush` 和 `terminalHeader`。文本内容使用
`panel.RenderPageWithTotal` 生成现有 Markdown，再执行全局编号装饰；图片内容先发送
`[终端输出#N]`、机器、工作区、Agent、pane、页码和选择告警文字，再调用 `SendImageTo`。
图片不包含 Server 标题。

图片发送时对 `router.gateway` 做 `WeComImageGateway` 类型断言；缺少能力时使用同页文本
降级。Task 9 的真实 `wecom.Client` 实现该可选接口。

输出与当前选择不一致时，图片标题追加：

```text
⚠️⚠️⚠️ 你的输入不会发送到该输出会话，使用 /N 切换到当前输出的会话。
```

- [ ] **Step 6：把状态通知策略移到 Server**

`SendNotification` 只接收状态元数据。策略固定为：

- working：发送状态文字，不拉取终端；
- blocked、done、working/blocked→idle：需要内容；
- unknown：发送状态文字，不拉取终端；
- target.invalidated：发送会话失效文字，不拉取终端，即使目录已删除该目标；
- 非当前会话且同一用户最近两分钟活跃：只发“有新的输出”短提示，不拉取内容；
- 其他需要内容的事件：按有效模式调用 `FetchTerminalSnapshot(..., 100)`。

快照读取失败时仍发送状态文字；图片结果失败但包含 `FallbackContent` 时发送该文本；企微
图片上传失败时发送同页 `Content.Text`，但不改变 Catalog 模式。用户输入、成功终端输出
和状态事件继续刷新同一用户的 activity 时间。

本任务删除 `RelayExecution.Content string`、旧通知正文处理和其他迁移脚手架；新的
`CommandExecute.output_mode`、状态事件字段及 Server limits 同时改为严格必填，并修复全仓
测试 fixtures。最终协议不接受旧通知正文。

- [ ] **Step 7：运行 Router、Panel 与 Activity 测试**

```bash
go test ./internal/server ./internal/panel -count=1
```

Expected: PASS。

- [ ] **Step 8：提交服务端用户体验**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/server internal/panel
git commit -m "feat: 添加终端显示模式与服务端通知策略"
```

## Task 9：实现企业微信图片上传与发送

**Files:**

- Modify: `internal/wecom/protocol.go`
- Modify: `internal/wecom/protocol_test.go`
- Modify: `internal/wecom/client.go`
- Modify: `internal/wecom/client_test.go`
- Modify: `internal/testkit/wecom_server.go`
- Modify: `internal/testkit/wecom_server_test.go`

- [ ] **Step 1：写官方三阶段上传协议失败测试**

测试固定以下字段：

```go
func TestEncodeUploadMediaInitIncludesImageMetadataAndMD5(t *testing.T)
func TestEncodeUploadMediaChunkUsesZeroBasedIndexAndBase64(t *testing.T)
func TestEncodeUploadMediaFinishUsesUploadID(t *testing.T)
func TestDecodeResponsePreservesTypedBody(t *testing.T)
func TestClientSendImageToUploadsAndSendsMediaID(t *testing.T)
func TestClientUploadImageSplitsAt512KiBAndStopsOnChunkFailure(t *testing.T)
```

fake socket 依次返回 `body.upload_id` 和 `body.media_id`，断言最终
`aibot_send_msg` 为 `msgtype:image`。

- [ ] **Step 2：运行测试确认失败**

```bash
go test ./internal/wecom ./internal/testkit -run 'Test(EncodeUpload|DecodeResponsePreserves|ClientSendImage|ClientUploadImage)' -count=1
```

Expected: FAIL。

- [ ] **Step 3：扩展企业微信响应和编码器**

`Response` 增加 `Body json.RawMessage`，`decodeResponse` 原样复制 body。新增编码器：

```go
func EncodeUploadMediaInit(requestID, filename string, totalSize, totalChunks int, md5Hex string) ([]byte, error)
func EncodeUploadMediaChunk(requestID, uploadID string, chunkIndex int, chunk []byte) ([]byte, error)
func EncodeUploadMediaFinish(requestID, uploadID string) ([]byte, error)
func EncodeSendImage(requestID, userID, mediaID string) ([]byte, error)
```

对应官方命令：`aibot_upload_media_init`、`aibot_upload_media_chunk`、
`aibot_upload_media_finish`、`aibot_send_msg`。图片类型固定 `image`，文件名固定
`herdr-terminal.png`。

- [ ] **Step 4：让请求返回完整 Response**

把 `session.request` 和 `Client.request` 改为返回 `(Response, error)`；订阅、心跳和 Markdown
调用忽略成功 Response。上传 init/finish 使用严格局部 struct 解码 `upload_id/media_id`，
拒绝空值、未知类型和非法 JSON。

- [ ] **Step 5：实现 UploadImage 与 SendImageTo**

```go
const mediaChunkSize = 512 * 1024

func (c *Client) UploadImage(ctx context.Context, png []byte) (string, error)
func (c *Client) SendImageTo(ctx context.Context, userID string, png []byte) error
```

校验 PNG 最少 5 字节、最多 10 MiB，计算 MD5，按最多 512 KiB 原始数据切分且不超过
100 片。init 返回 upload ID 后按 0 开始顺序上传，finish 返回 media ID，最后发送图片。
任何阶段失败立即停止；日志只记录图片字节数、分片数、阶段和安全错误，不记录 Base64、
upload ID 或 media ID。

- [ ] **Step 6：运行 WeCom 全包测试**

```bash
go test ./internal/wecom ./internal/testkit -count=1
```

Expected: PASS。

- [ ] **Step 7：提交企业微信图片支持**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/wecom internal/testkit
git commit -m "feat: 支持企业微信终端图片消息"
```

## Task 10：完成应用接线、帮助和端到端测试

**Files:**

- Modify: `internal/app/relay.go`
- Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/serverapp/app.go`
- Modify: `internal/serverapp/app_test.go`
- Modify: `internal/integration/bridge_test.go`
- Modify: `internal/command/parser.go`
- Modify: `internal/command/parser_test.go`
- Modify: `internal/buildscript/build_test.go`
- Modify: `README.md`

- [ ] **Step 1：写应用接线和完整链路失败测试**

增加：

```go
func TestRelayAppBuildsTerminalRendererAndAdvertisesImage(t *testing.T)
func TestServerAppRoutesImageGateway(t *testing.T)
func TestIntegrationImageConCarriesAuditTextAndUploadsPNG(t *testing.T)
func TestIntegrationPageUpPageDownAcrossModeChanges(t *testing.T)
func TestIntegrationDoneEventLetsServerFetchSnapshot(t *testing.T)
func TestServerBinaryDoesNotDependOnTerminalImagePackage(t *testing.T)
```

完整链路使用 fake Herdr、真实 HPRP 本地 WSS、真实 Router 和 fake WeCom，不访问公网。

- [ ] **Step 2：运行集成测试确认失败**

```bash
go test ./internal/app ./internal/serverapp ./internal/integration ./internal/buildscript -run 'Test(RelayAppBuilds|ServerAppRoutesImage|IntegrationImage|IntegrationPage|IntegrationDone|ServerBinaryDoesNot)' -count=1
```

Expected: FAIL。

- [ ] **Step 3：在 Pal Relay 模式创建 Renderer**

`internal/app/relay.go` 调用 `terminalimage.New()`，失败时带明确阶段返回启动错误；把 renderer
注入 Bridge Service。直接企业微信单机旧模式和 `-i` 使用 nil renderer，仍保持文本行为。
`NewNotifier` 调用删除 `ReadRecent` 参数。

- [ ] **Step 4：完成 Server Gateway 接线**

`wecom.Client` 直接满足新增 `SendImageTo`；`serverapp` 不持有渲染器。增加构建依赖测试：

```bash
go list -deps ./cmd/herdr-pal-server
```

输出不得包含 `internal/terminalimage`、`textimg/v3` 或字体包，确保 16 MiB 字体只进入 Pal
二进制。

- [ ] **Step 5：更新帮助与 README**

Server `/help` 增加：

```text
/mode img          当前会话使用终端图片，保留颜色和选中样式
/mode txt          当前会话使用纯文本
OpenCode 默认使用图片模式，其他 Agent 默认使用文本模式；模式只在 Server 本次运行期间保存。
```

Pal 本地 `command.HelpText()` 不增加 `/mode`，因为模式是 Server 全局命令，不能转发到 Pal。
README 增加图片模式、默认规则、PNG 与同页文本、企微临时素材及图片失败降级说明。

- [ ] **Step 6：运行端到端和全包测试**

```bash
go test ./internal/app ./internal/serverapp ./internal/integration ./internal/buildscript -count=1
./unittest.sh
```

Expected: 全部 PASS。

- [ ] **Step 7：提交接线与文档**

```bash
./unittest.sh
./build.sh
git diff --check
git add internal/app internal/serverapp internal/integration internal/command internal/buildscript README.md
git commit -m "feat: 完成终端图片模式端到端接入"
```

## Task 11：协议一致性、自审和发布前验证

**Files:**

- Modify: `docs/HPRP_PROTOCOL_DESIGN.md`
- Modify: `docs/superpowers/specs/2026-07-29-terminal-image-mode-design.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`

- [ ] **Step 1：检查实现与规范字段一致**

```bash
rg -n 'terminal\.snapshot|terminal\.image|output_mode|agent\.status\.changed|fallback_content' internal docs/HPRP_PROTOCOL_DESIGN.md
```

逐项确认消息名、JSON 字段、错误码、能力名、大小限制和模式默认值完全一致。实现中不得
保留旧 `notification.event.content` 兼容分支。

- [ ] **Step 2：检查日志与敏感内容**

```bash
rg -n 'Base64|base64_data|media_id|upload_id|Content\.Text|FallbackContent\.Text' internal | rg 'logger|slog|Log'
```

Expected: 日志不输出终端正文、Base64、upload ID 或 media ID；只记录尺寸、耗时、目标哈希、
页码、模式和错误类型。

- [ ] **Step 3：格式化和静态检查**

```bash
gofmt -w internal
go vet ./...
git diff --check
```

Expected: 全部成功，无格式问题。

- [ ] **Step 4：执行完整单测和多平台构建**

```bash
./unittest.sh
./build.sh
```

Expected: 当前系统、Linux AMD64/ARM64、macOS AMD64/ARM64、Windows AMD64 构建成功；
全部单元测试和本地 fake 集成测试通过。

- [ ] **Step 5：检查制品内容和体积**

```bash
ls -lh dist
go test ./internal/buildscript -run TestServerBinaryDoesNotDependOnTerminalImagePackage -count=1
```

Expected: Pal 因字体增大；Server 和 `hp-cli` 不包含 renderer 依赖。生成的测试 PNG 不写入
仓库，运行时也不落盘。

- [ ] **Step 6：提交最终文档勘误**

把 HPRP 字段、默认模式、状态事件拉取流程、企微图片上传和日志边界同步为经过测试确认的
最终实现，然后执行：

```bash
./unittest.sh
./build.sh
git diff --check
git add docs README.md
git commit -m "docs: 同步终端图片模式实现说明"
```

- [ ] **Step 7：确认工作树与提交序列**

```bash
git status --short
git log --oneline --decorate -15
```

Expected: 工作树为空；每个提交范围单一，未创建 tag、未 push、未发布 Release。
