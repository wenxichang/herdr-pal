# 服务端输入限速与 OTLP 审计 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. 本项目明确不使用 subagent-driven-development。

**Goal:** 为 herdr-pal-server 增加可配置的按用户输入限速，以及不会阻塞企业微信业务链路的终端文本与用户输入 OTLP Logs 审计。

**Architecture:** 配置层把“字段缺省”和“显式为 0”解析为确定的运行时值；`UserRateLimiter` 在消息去重后、进入单用户执行队列前执行滚动窗口判断。审计采用独立 `internal/audit` 包，Router 只产生平台中立的审计事件，stderr 与 OTLP 各自通过有界异步 worker 输出，任何审计失败都只记录脱敏运行日志而不改变业务结果。

**Tech Stack:** Go 1.26、标准库 HTTP/TLS/JSON、OTLP/HTTP protobuf、`go.opentelemetry.io/proto/otlp`、`google.golang.org/protobuf`、现有 slog 与企业微信 Router。

---

## 文件结构

- `internal/config/server.go`：新增限速与审计配置，完成默认值、范围和端点校验。
- `internal/config/relay_test.go`：覆盖服务端配置缺省、禁用、非法值和 OTLP 环境 Header。
- `server-config.example.json`：展示生产配置格式。
- `internal/server/rate_limiter.go`：实现按用户滚动窗口限速器。
- `internal/server/rate_limiter_test.go`：覆盖双窗口、禁用、拒绝不续期和时钟回拨。
- `internal/audit/event.go`：定义稳定的版本化审计事件与接口。
- `internal/audit/redactor.go`：执行最小强制凭据脱敏。
- `internal/audit/async.go`：实现每个真实输出独立的有界非阻塞队列、批处理和关闭刷新。
- `internal/audit/stderr.go`：输出 JSON Lines 调试审计。
- `internal/audit/otlp.go`：编码并发送 OTLP/HTTP protobuf Logs。
- `internal/audit/*_test.go`：覆盖脱敏、队列、重试、部分成功和 OTLP 负载。
- `internal/server/router.go`：接入用户输入限速、输入审计和终端输出审计。
- `internal/server/router_test.go`：覆盖 Router 的限速顺序、审计时机和图片降级结果。
- `internal/serverapp/app.go`：装配并关闭审计器，向 Router 注入限速器。
- `internal/serverapp/app_test.go`：覆盖审计工厂与启动配置错误。
- `README.md`：补充服务端限速与审计配置说明。
- `go.mod`、`go.sum`：加入官方 OTLP protobuf 依赖。

### Task 1: 配置模型和校验

**Files:**
- Modify: `internal/config/server.go`
- Modify: `internal/config/relay_test.go`
- Modify: `server-config.example.json`

- [ ] **Step 1: 写配置失败测试**

在 `internal/config/relay_test.go` 增加表驱动测试，断言：字段缺省得到 `1/20`；显式 `0/0` 保持禁用；负数和大于 `10000` 拒绝；`audit.type` 只接受 `none`、`otlp`；OTLP 端点必须是无 userinfo/query/fragment 且路径为 `/v1/logs` 的 HTTP(S) URL；`skip_verify` 只能用于 HTTPS；`type=none` 时配置 endpoint 或 skip_verify 会报错；Header 从 `OTEL_EXPORTER_OTLP_LOGS_HEADERS` 读取并拒绝非法键值。

- [ ] **Step 2: 运行配置测试并确认失败**

Run: `go test ./internal/config -run 'TestLoadServer.*(RateLimit|Audit)' -count=1`

Expected: FAIL，缺少 `RateLimit`、`Audit` 字段或相应校验。

- [ ] **Step 3: 实现配置类型与解析**

在 `internal/config/server.go` 增加运行时配置：

```go
type RateLimitConfig struct {
	PerSecond int `json:"per_second"`
	PerMinute int `json:"per_minute"`
}

type AuditConfig struct {
	Type       string            `json:"type"`
	Endpoint   string            `json:"endpoint"`
	SkipVerify bool              `json:"skip_verify"`
	Stderr     bool              `json:"stderr"`
	Headers    map[string]string `json:"-"`
}
```

使用仅供 JSON 解码的指针字段区分缺省与显式零，再解析到 `ServerConfig.RateLimit`。`LoadServerAdmin` 负责静态字段校验；`LoadServer` 使用传入的 `getenv` 解析 OTLP Header，避免测试读取真实进程环境。

- [ ] **Step 4: 更新示例配置并运行测试**

Run: `gofmt -w internal/config/server.go internal/config/relay_test.go && go test ./internal/config -count=1`

Expected: PASS。

- [ ] **Step 5: 提交配置变更**

Run: `git add internal/config/server.go internal/config/relay_test.go server-config.example.json && git commit -m "feat: 添加服务端限速与审计配置"`

### Task 2: 按用户滚动窗口限速器

**Files:**
- Create: `internal/server/rate_limiter.go`
- Create: `internal/server/rate_limiter_test.go`

- [ ] **Step 1: 写限速器失败测试**

覆盖以下公开行为：

```go
type RateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

func (limiter *UserRateLimiter) Allow(userID string) RateLimitDecision
```

测试每秒第 2 条被拒、60 秒窗口第 21 条被拒、任一窗口为 0 时单独禁用、拒绝项不写入时间戳、不同用户互不影响、时间回拨不产生负等待时间、过期用户状态可清理。

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/server -run TestUserRateLimiter -count=1`

Expected: FAIL，限速器尚不存在。

- [ ] **Step 3: 实现最小滚动窗口算法**

每个用户仅保存仍处于 60 秒内的已接受时间戳；判断两个窗口时计算需要等待的最大值。只有 `Allowed=true` 才追加当前时间；`RetryAfter` 向上取整为秒且最少 1 秒。构造函数接收 `now func() time.Time`，便于确定性测试。

- [ ] **Step 4: 运行限速器测试**

Run: `gofmt -w internal/server/rate_limiter.go internal/server/rate_limiter_test.go && go test -race ./internal/server -run TestUserRateLimiter -count=1`

Expected: PASS。

- [ ] **Step 5: 提交限速器**

Run: `git add internal/server/rate_limiter.go internal/server/rate_limiter_test.go && git commit -m "feat: 添加按用户滚动窗口限速"`

### Task 3: 审计事件模型与凭据脱敏

**Files:**
- Create: `internal/audit/event.go`
- Create: `internal/audit/redactor.go`
- Create: `internal/audit/event_test.go`
- Create: `internal/audit/redactor_test.go`

- [ ] **Step 1: 写事件和脱敏失败测试**

定义测试断言：事件 ID 是 32 位小写十六进制且创建一次后保持不变；用户 ID 保留原值；message/request/session 标识只保存 SHA-256 短哈希；正文中的机器人 Secret、OTLP Header 值、`hpk_` Key、Bearer token、Cookie 与 Set-Cookie 值被替换为 `[REDACTED]`，普通自然语言保持不变。

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/audit -run 'Test(NewEvent|Redactor)' -count=1`

Expected: FAIL，包或符号不存在。

- [ ] **Step 3: 实现版本化事件与审计接口**

```go
type Event struct {
	SchemaVersion int               `json:"schema_version"`
	EventID       string            `json:"event_id"`
	EventName     string            `json:"event_name"`
	OccurredAt    time.Time         `json:"occurred_at"`
	PrincipalID   string            `json:"principal_id"`
	Body          string            `json:"body"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

type Auditor interface {
	Emit(Event)
	Shutdown(context.Context) error
}
```

增加 `NoopAuditor`，并为所有导出类型和方法写适合 `go doc` 的中文注释。脱敏器预编译规则，显式 Secret/Header 值按长度从大到小替换，避免短值破坏长值匹配。

- [ ] **Step 4: 运行审计核心测试**

Run: `gofmt -w internal/audit && go test -race ./internal/audit -count=1`

Expected: PASS。

- [ ] **Step 5: 提交审计核心**

Run: `git add internal/audit && git commit -m "feat: 添加审计事件与凭据脱敏"`

### Task 4: 异步输出与 stderr 审计

**Files:**
- Create: `internal/audit/async.go`
- Create: `internal/audit/stderr.go`
- Create: `internal/audit/async_test.go`
- Create: `internal/audit/stderr_test.go`

- [ ] **Step 1: 写异步队列失败测试**

用可阻塞 fake exporter 验证 `Emit` 不等待网络；队列按 `1024` 条或 `16 MiB` 任一上限丢弃；批次最多 `64` 条或约 `512 KiB`；1 秒触发刷新；`Shutdown` 刷新已接收事件并尊重 context；stderr 每行是一个合法 JSON 事件且不受 slog level 影响。

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/audit -run 'Test(Async|Stderr)' -count=1`

Expected: FAIL，异步和 stderr 实现不存在。

- [ ] **Step 3: 实现输出边界**

定义内部 `BatchExporter` 接口，让每个真实输出拥有独立 worker。`MultiAuditor.Emit` 依次调用各子审计器的非阻塞 `Emit`；队列溢出仅通过注入的脱敏 logger 汇总报告丢弃数，不写事件正文。正常关闭最多由调用方提供 5 秒 context。

- [ ] **Step 4: 运行并发测试**

Run: `gofmt -w internal/audit && go test -race ./internal/audit -run 'Test(Async|Stderr)' -count=1`

Expected: PASS 且 race detector 无告警。

- [ ] **Step 5: 提交异步审计**

Run: `git add internal/audit && git commit -m "feat: 添加异步审计输出"`

### Task 5: OTLP/HTTP protobuf Logs 输出

**Files:**
- Create: `internal/audit/otlp.go`
- Create: `internal/audit/otlp_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: 添加官方 protobuf 依赖并写失败测试**

Run: `go get go.opentelemetry.io/proto/otlp@v1.11.0 google.golang.org/protobuf@v1.36.11`

使用 `httptest.Server` 解码 `ExportLogsServiceRequest`，断言事件正文进入 LogRecord body，稳定字段进入 attributes，时间戳正确，Content-Type 为 `application/x-protobuf`，自定义 Header 被发送且不会进入日志。

- [ ] **Step 2: 运行 OTLP 测试并确认失败**

Run: `go test ./internal/audit -run TestOTLP -count=1`

Expected: FAIL，OTLP exporter 尚不存在。

- [ ] **Step 3: 实现 OTLP exporter 与重试分类**

直接编码官方 OTLP protobuf；HTTP `429/502/503/504` 和网络临时错误标记为可重试，遵守 `Retry-After`，否则使用带抖动指数退避，总重试寿命不超过 30 秒、单请求超时 5 秒。HTTP 成功但响应含 `partial_success` 时记录脱敏警告且不重试。

- [ ] **Step 4: 覆盖失败和恢复路径**

新增测试验证永久 4xx 不重试、503 后成功、Retry-After、生存期截止、partial success 不重试、TLS `skip_verify` 仅影响专用 transport。运行：

Run: `gofmt -w internal/audit && go test -race ./internal/audit -run TestOTLP -count=1`

Expected: PASS。

- [ ] **Step 5: 提交 OTLP 输出**

Run: `git add internal/audit go.mod go.sum && git commit -m "feat: 添加 OTLP Logs 审计输出"`

### Task 6: Router 用户输入限速与输入审计

**Files:**
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [ ] **Step 1: 写 Router 顺序测试**

测试相同 `message_id` 的重复投递在 dedupe 后直接忽略，不占额度也不重复审计；唯一输入在进入 `UserExecutor` 前限速；命令、非法语法、普通 prompt、无会话输入都计数；拒绝回复 `输入过于频繁，请在 N 秒后重试。`；accepted 与 rate_limited 各产生一条 `herdr_pal.user_input`，正文为脱敏后的完整原始输入。

- [ ] **Step 2: 运行 Router 测试并确认失败**

Run: `go test ./internal/server -run 'TestConversationRouter.*(RateLimit|InputAudit)' -count=1`

Expected: FAIL，Router 尚未接收 limiter/auditor。

- [ ] **Step 3: 扩展 Router 配置和 Handle**

在 `ConversationRouterConfig` 增加 `RateLimiter *UserRateLimiter` 与 `Auditor audit.Auditor`。保持现有 `NewConversationRouter` 默认禁用限速并使用 Noop，以免破坏已有单元测试；生产装配显式注入。`Handle` 的固定顺序为：基础字段校验 → message ID 去重 → 限速判定 → 输入审计 → accepted 输入提交执行队列或 rate_limited 立即回复。

- [ ] **Step 4: 运行 Router 测试**

Run: `gofmt -w internal/server/router.go internal/server/router_test.go && go test -race ./internal/server -count=1`

Expected: PASS。

- [ ] **Step 5: 提交 Router 输入链路**

Run: `git add internal/server/router.go internal/server/router_test.go && git commit -m "feat: 接入用户输入限速与审计"`

### Task 7: Router 终端输出审计

**Files:**
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`

- [ ] **Step 1: 写终端输出审计失败测试**

覆盖首段回复、主动 push、命令后续输出和状态通知拉取四类来源；纯文本成功记 `delivered/txt`；图片成功记 `delivered/img` 且 body 为 `Content.Text`；图片失败但文本降级成功记 `delivered/txt`；最终发送失败记 `delivery_failed`；Markdown 帮助和普通错误不产生 terminal 事件；分页拆分也只产生一个逻辑事件。

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/server -run 'TestConversationRouter.*TerminalAudit' -count=1`

Expected: FAIL，终端发送 helper 尚未记录最终发送结果。

- [ ] **Step 3: 在统一发送边界产生事件**

让 `sendContentReply`、`sendContentPush` 接收明确 action，例如 `select_console`、`command_result`、`command_output`、`status_notification`。在图片发送与降级全部结束后统一构造 `herdr_pal.terminal_output`，body 始终取原始 `content.Text`，属性包含 target、page、requested/final presentation、delivery outcome 和 content_bytes；`Emit` 不影响原发送错误。

- [ ] **Step 4: 运行 Router 全部测试**

Run: `gofmt -w internal/server/router.go internal/server/router_test.go && go test -race ./internal/server -count=1`

Expected: PASS。

- [ ] **Step 5: 提交终端审计**

Run: `git add internal/server/router.go internal/server/router_test.go && git commit -m "feat: 添加终端文本输出审计"`

### Task 8: 服务装配、关闭和文档

**Files:**
- Modify: `internal/serverapp/app.go`
- Modify: `internal/serverapp/app_test.go`
- Modify: `README.md`

- [ ] **Step 1: 写装配失败测试**

验证 `audit.type=none, stderr=false` 生成 Noop；`none,true` 只生成 stderr；`otlp,false` 只生成 OTLP；`otlp,true` 生成两个独立异步输出；构造失败返回 `ErrConfig`；server shutdown 在 5 秒 context 内调用审计器 Shutdown；审计失败不导致业务组件退出。

- [ ] **Step 2: 运行测试并确认失败**

Run: `go test ./internal/serverapp -run 'Test.*Audit' -count=1`

Expected: FAIL，审计工厂和生命周期尚不存在。

- [ ] **Step 3: 实现生产装配**

在 `serverapp.Run` 创建 Redactor、Auditor 和 `UserRateLimiter`，把 Bot Secret 与 OTLP Header 值传给脱敏器，再注入 Router。shutdown 顺序先停止入口、断开 Relay，再用独立 `context.WithTimeout(..., 5*time.Second)` 刷新审计器；刷新失败仅写脱敏 warning。

- [ ] **Step 4: 更新 README**

记录默认 `1 条/秒、20 条/分钟`、显式 0 禁用、OTLP endpoint 必须包含 `/v1/logs`、Header 使用 `OTEL_EXPORTER_OTLP_LOGS_HEADERS`、stderr 会打印含正文的敏感调试审计、审计故障 fail-open、图片只审计配套文本。

- [ ] **Step 5: 运行模块测试**

Run: `gofmt -w internal/serverapp && go test -race ./internal/serverapp -count=1`

Expected: PASS。

- [ ] **Step 6: 提交装配和文档**

Run: `git add internal/serverapp README.md && git commit -m "feat: 装配服务端限速与审计"`

### Task 9: 全量验证

**Files:**
- Modify only if verification exposes defects.

- [ ] **Step 1: 检查格式、占位符和工作区**

Run: `git diff --check && rg -n 'TODO|TBD|implement later|fill in details' internal/audit internal/server internal/config internal/serverapp README.md server-config.example.json || true`

Expected: `git diff --check` 无输出；不出现本功能遗留占位符。

- [ ] **Step 2: 运行统一单元测试**

Run: `./unittest.sh`

Expected: 全部 Go 单元测试和 race 检查通过。

- [ ] **Step 3: 运行统一构建**

Run: `./build.sh`

Expected: 当前系统、Linux AMD64/ARM64 和 Windows AMD64 等既有目标全部构建成功。

- [ ] **Step 4: 检查最终差异与敏感信息**

Run: `git diff --stat HEAD~1 && git grep -nE 'HERDR_PAL_WECOM_SECRET=|hpk_[A-Za-z0-9_-]{12,}|Authorization: Bearer [^[]' -- ':!docs/superpowers/specs/*' ':!docs/superpowers/plans/*' || true`

Expected: 不存在真实 Secret、Key 或 Bearer token。

- [ ] **Step 5: 如有验证修复则提交**

Run: `git add -A && git commit -m "test: 完善限速与审计验证"`

Expected: 仅当验证阶段实际产生修复时执行；否则保持工作区干净。
