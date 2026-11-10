# dsh 免插件原生 API 接入实施计划

> 已完成（2026-11-10）——这是实施记录，不是待执行计划；过渡期的 `internal/dshlegacy` 已删除，Task 1/17 中的过渡步骤不再适用。

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用 Go 直接接入 dsh 自带 Remote API，替换 Node 插件，支持多宿主会话、读取、排队发送、审批回答和新建会话。

**Architecture:** 在 `internal/dsh` 内实现认证、宿主发现、HTTP RPC、Mux 待答、聊天正文投影、决策/答案和执行器；`Backend` 聚合 web/Desktop 多个宿主；现有 HPRP、Server Router 和企微链路保持不变，只新增 `/new-session` 的 `created_target` 选择切换。

**Tech Stack:** Go 1.26、`github.com/coder/websocket`、`gopkg.in/yaml.v3`、现有 HPRP/Relay/企微测试夹具。

**Spec:** `docs/DSH_NATIVE_API_DESIGN.md`

## Global Constraints

- 只支持 dsh；不移植其他 Agent 适配器。
- 不启动、停止或重启 dsh；只连接已运行宿主。
- 认证材料、Cookie 和完整审批正文不得进入日志或 Git。
- 默认拒绝未知宿主、未知会话、未选中目标和未授权用户。
- 审批只允许 `allowed-once` / `rejected`，必须由用户显式 `/answer` 触发。
- 普通文本使用 `session/prompt` 的 `queue` 模式；待审批时拒绝普通文本。
- 写入不自动重试；IM 消息 ID 作为 dsh `requestId`。
- `/con` 只输出真实用户输入和助手文本，不输出工具、推理、系统注入或压缩摘要。
- 只列 `agentAvailable=true` 的会话；web 与 Desktop 分开列，不合并同名会话。
- dsh 版本门禁：`0.2.1-rc.5`、`0.2.2-rc.1`；其他版本跳过并记录不兼容。
- 所有新代码注释和文档使用中文，标识符使用英文。
- 每个任务结束前必须通过本任务测试；提交信息使用中文 conventional commit。

## Review Focus

1. 认证文件格式变化或权限不正确时是否失败关闭，且错误/日志不含密钥。
2. 进程发现是否会把非 dsh 的本机监听误认成宿主；监听归属与 401 fence 是否同时校验。
3. 同一会话在 web/Desktop 共享历史时是否不会串目标；目标 ID 是否包含宿主身份。
4. 审批答案在 revision 变化、事件取消或并发 UI 回答时是否不会错发或重发。
5. `/new-session` 的新目标在快照确认前是否不会被选择，旧 Server/Pal 是否仍可用。
6. 宿主退出、Cookie 轮换或 RPC 401 时是否撤下目标并使旧选择失效。
7. 读取是否会泄露 `source.kind!=user` 的注入内容、工具结果或推理。
8. 运行中发送是否只进入一次队列，超时是否不会自动重发。

---

### Task 1: 配置迁移与旧插件隔离

**Files:**
- Move: `internal/dsh/` → `internal/dshlegacy/`（旧 Node 插件客户端，过渡期保留）
- Modify: `internal/app/dsh.go`、`internal/app/relay.go`（导入与 nil 安全）
- Modify: `internal/config/client.go`
- Modify: `internal/config/dsh_test.go`
- Modify: `config.example.json`
- Test: `internal/config/dsh_test.go`

**Interfaces:**
- Produces: `type DSHConfig struct { URL, Home, Variant, Token string }`；`ClientConfig.DSH *DSHConfig`。
  `Token` 在本任务仅作过渡兼容，Task 17 移除并改为报错。
- Produces: `func validateDSHConfig(cfg DSHConfig) error`（包内）。
- Produces: `internal/dshlegacy` 保留旧 `ValidateConfig`/`NewClient`/`NewExecutor`，旧插件路径不中断。

- [ ] **Step 1: 写失败测试**

在 `internal/config/dsh_test.go` 增加：

```go
func TestDSHConfigNativeMode(t *testing.T)
func TestDSHConfigKeepsLegacyTokenDuringMigration(t *testing.T)
func TestDSHConfigValidatesURLHomeAndVariant(t *testing.T)
```

断言：
- `{"dsh":{}}` 可加载且 `DSH != nil`。
- `{"herdr":{"disabled":true},"dsh":{}}` 可加载。
- `{"dsh":{"token":"x"}}` 当前仍可加载（过渡兼容，Task 17 改为报错）。
- `{"dsh":{"url":"http://example.com:1"}}`、相对 `home`、`variant:"other"` 均失败。
- 无 `dsh` 且 `herdr.disabled=true` 失败。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/config -run 'TestDSHConfig' -count=1`
Expected: FAIL（字段不存在或仍要求 token）。

- [ ] **Step 3: 隔离旧插件客户端**

```bash
mkdir -p internal/dshlegacy
git mv internal/dsh/client.go internal/dshlegacy/client.go
git mv internal/dsh/client_test.go internal/dshlegacy/client_test.go
git mv internal/dsh/executor.go internal/dshlegacy/executor.go
git mv internal/dsh/executor_test.go internal/dshlegacy/executor_test.go
```

把四个文件的 `package dsh` 改为 `package dshlegacy`，并把 `internal/app/dsh.go`、
`internal/app/relay.go` 的 `internal/dsh` 导入改为 `internal/dshlegacy`。
旧 `dsh.NewClient`/`dsh.NewExecutor`/`dsh.ValidateConfig` 调用保持不变，只改包名和导入别名。

- [ ] **Step 4: 修改配置类型与校验**

`internal/config/client.go`：

```go
type ClientConfig struct {
    Relay RelayConfig `json:"relay"`
    Herdr HerdrConfig `json:"herdr"`
    Log   LogConfig   `json:"log"`
    DSH   *DSHConfig  `json:"dsh,omitempty"`
}

type DSHConfig struct {
    URL     string `json:"url,omitempty"`
    Home    string `json:"home,omitempty"`
    Variant string `json:"variant,omitempty"`
    Token   string `json:"token,omitempty"` // 过渡兼容，Task 17 移除
}
```

`UnmarshalJSON` 使用 `decodeStrict` 读取 `url/home/variant/token`；`Token` 暂时保留。
`LoadClient` 在 `loaded.DSH != nil` 时调用 `validateDSHConfig`；`herdr.disabled=true` 要求
`loaded.DSH != nil`。旧 `dshlegacy.ValidateConfig` 调用改为在 `Token` 非空时执行，保证过渡期
旧插件配置仍可用。`internal/app` 中所有 `loaded.DSH.URL/Token` 访问前先判断
`loaded.DSH != nil`。

- [ ] **Step 5: 更新 `config.example.json`**

`dsh` 改为：

```json
"dsh": {
  "url": "",
  "home": "",
  "variant": "auto"
}
```

- [ ] **Step 6: 运行测试与全仓编译检查**

Run:
```sh
go test ./internal/config ./internal/dshlegacy ./internal/app -count=1
go test ./... -run '^$'
```
Expected: PASS；全仓测试二进制可编译。

- [ ] **Step 7: 提交**

```bash
git add -A internal/config internal/app internal/dshlegacy config.example.json
git commit -m "refactor: 隔离旧 dsh 插件客户端并迁移配置"
```

---

### Task 2: 命令解析与 Herdr 拒绝

**Files:**
- Modify: `internal/command/parser.go`
- Modify: `internal/command/parser_test.go`
- Modify: `internal/bridge/service.go`
- Test: `internal/bridge/service_test.go`

**Interfaces:**
- Produces: `KindAnswer`、`KindNewSession`；`Action.Indexes []int`、`Action.Title string`、`Action.Directory string`。
- Produces: `/answer` 前导数字解析为 `Indexes`，其余为 `Text`。
- Produces: `/new-session` 按“最后一段路径参数视为目录”解析。

- [ ] **Step 1: 写失败测试**

`internal/command/parser_test.go` 增加表驱动测试：

```go
func TestParseAnswer(t *testing.T)      // /answer 1,2,3 -> Indexes [1,2,3], Text ""
                                        // /answer 5 自定义答案 -> [5], "自定义答案"
                                        // /answer 纯文本答案 -> nil, "纯文本答案"
                                        // /answer -> 用法错误
func TestParseNewSession(t *testing.T)  // 无参数 -> 默认标题、空目录
                                        // /new-session 修复登录 -> Title "修复登录"
                                        // /new-session 修复登录 /Users/me/work -> Title/Directory
                                        // /new-session /Users/me/work -> 默认标题/Directory
func TestParseUnknownSlashStillRejected(t *testing.T)
```

`internal/bridge/service_test.go` 增加：Herdr 目标收到 `/answer 1` 和 `/new-session` 时回复
“当前目标不支持”且不发送 prompt/按键。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/command ./internal/bridge -run 'TestParseAnswer|TestParseNewSession|TestBridgeRejectsDSHCommands' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现解析**

`internal/command/parser.go`：

```go
const (
    KindAnswer Kind = iota + 9
    KindNewSession
)

type Action struct {
    Kind      Kind
    Index    int
    Keys     []string
    Text     string
    Indexes  []int
    Title    string
    Directory string
}
```

- `parseAnswer(raw string)`：按逗号/空白切分前导纯数字；遇到第一个非数字 token 后，剩余原文
  作为 `Text`；数字必须 >0，最多 32 个，去重。
- `parseNewSession(raw string)`：无参数时 `Title="默认标题"`；最后一个 token 以 `/`、`~/`、
  `./`、`../` 开头或等于 `~` 时为 `Directory`，其余 token 用空格连接为 `Title`；标题为空时
  使用默认标题；标题最多 256 个 rune。
- 更新 `helpText`，加入 `/answer` 和 `/new-session` 两行。

`internal/bridge/service.go` 的 `HandleMessage` switch 增加：

```go
case command.KindAnswer, command.KindNewSession:
    reply("当前目标不支持该 dsh 命令；请在 dsh 会话中使用。")
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/command ./internal/bridge -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/command/parser.go internal/command/parser_test.go internal/bridge/service.go internal/bridge/service_test.go
git commit -m "feat: 添加 dsh 审批回答与新建会话命令"
```

---

### Task 3: 认证 Cookie

**Files:**
- Create: `internal/dsh/auth.go`
- Create: `internal/dsh/auth_unix.go`
- Create: `internal/dsh/auth_windows.go`
- Test: `internal/dsh/auth_test.go`
- Modify: `go.mod`（`gopkg.in/yaml.v3` 改为直接依赖）

**Interfaces:**
- Produces: `func cookieForCredentials(path, authority string, now time.Time) (string, error)`。
- Produces: `func readCredentialSecret(path string) ([]byte, error)`（包内）。
- Produces: `func openCredentialFile(path string) (*os.File, error)`（平台文件）。

- [ ] **Step 1: 写失败测试**

`internal/dsh/auth_test.go`：

```go
func TestCookieIsBoundToAuthorityAndExpiry(t *testing.T)
func TestCookieRejectsPublicFileAndSymlink(t *testing.T)
func TestCookieRejectsUnknownCredentialVersions(t *testing.T)
func TestCookieErrorDoesNotContainSecret(t *testing.T)
```

测试写入临时 `.credentials.yaml`，使用 32 字节全零密钥；断言：
- 同一 authority/now 生成相同 Cookie。
- 不同 authority 的 Cookie 不同，name 也不同。
- 解码 payload 后 `expiresAt == now+300000`。
- 0644 文件和符号链接失败。
- `version:2`、`kind:other`、密钥长度错误失败。
- 错误字符串不包含 `secret` 内容。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestCookie' -count=1`
Expected: FAIL（函数未定义）。

- [ ] **Step 3: 实现**

`auth.go`：

```go
func cookieForCredentials(path, authority string, now time.Time) (string, error)
```

步骤：
1. 校验 authority 为 `127.0.0.1:<port>`。
2. `openCredentialFile` 打开文件；Unix 实现使用 `O_NOFOLLOW`，检查 `Stat().Uid()` 等于
   `os.Geteuid()` 且 `Mode()&0o077 == 0`，文件大小 <= 256 KiB。
3. `yaml.Unmarshal` 到：

```go
type credentialsFile struct {
    Version uint8 `yaml:"version"`
    Records struct {
        Session struct {
            Kind    string `yaml:"kind"`
            Payload struct {
                Version uint8  `yaml:"version"`
                Secret  string `yaml:"secret"`
            } `yaml:"payload"`
        } `yaml:"client-connection/browser-session"`
    } `yaml:"records"`
}
```

4. 校验 `Version==1`、`Kind=="grant"`、payload `Version==1`；base64url 无填充解码，长度 32。
5. 生成 payload JSON、HMAC-SHA256、Cookie name/value，返回 `name=value`。

`auth_windows.go` 使用 `os.Open` + `os.Lstat` 拒绝符号链接，大小限制相同；文件 ACL 校验为
best-effort，文档说明 Windows 只支持显式配置。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestCookie' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add go.mod go.sum internal/dsh/auth.go internal/dsh/auth_unix.go internal/dsh/auth_windows.go internal/dsh/auth_test.go
git commit -m "feat: 实现 dsh 本机认证 Cookie 生成"
```

---

### Task 4: Host 模型与版本门禁

**Files:**
- Create: `internal/dsh/host.go`
- Test: `internal/dsh/host_test.go`

**Interfaces:**
- Produces: `type Host struct { PID uint32; Port uint16; Version, Variant, InstallDir, Home, Executable, Generation string }`。
- Produces: `func (h Host) Reference() string`、`func (h Host) URL() string`、`func (h Host) Validate() error`。

- [ ] **Step 1: 写失败测试**

```go
func TestHostReferenceIncludesGeneration(t *testing.T)
func TestHostValidateAcceptsCheckedVersions(t *testing.T)
func TestHostValidateRejectsUnknownVersionAndBadPort(t *testing.T)
```

断言 `Reference() == "harness:<pid>:<port>:<generation>"`；版本仅接受 `0.2.1-rc.5`、
`0.2.2-rc.1`，以及显式配置使用的内部值 `explicit`；端口 1..65535；Variant 仅
`web|desktop`；PID 允许为 0（显式配置）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestHost' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`host.go` 定义 `Host`、`supportedVersions`、`Reference`、`URL`、`Validate`。`URL()` 固定
`http://127.0.0.1:<port>/`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestHost' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/host.go internal/dsh/host_test.go
git commit -m "feat: 定义 dsh 宿主身份与版本门禁"
```

---

### Task 5: 宿主发现

**Files:**
- Create: `internal/dsh/discover.go`
- Create: `internal/dsh/discover_darwin.go`
- Create: `internal/dsh/discover_linux.go`
- Create: `internal/dsh/discover_other.go`
- Test: `internal/dsh/discover_test.go`

**Interfaces:**
- Produces: `type DiscoveryConfig struct { URL, Home, Variant string }`。
- Produces: `type Prober interface { Run(ctx, name string, args ...string) ([]byte, error); ReadFile(string) ([]byte, error); Stat(string) (os.FileInfo, error); LocalListeners(pid uint32) ([]uint16, error); Fence(port uint16) bool }`。
- Produces: `func discoverHosts(ctx context.Context, cfg DiscoveryConfig, p Prober, goos string) ([]Host, error)`。

- [ ] **Step 1: 写失败测试**

`discover_test.go` 使用 fake `Prober` 和固定输出：

```go
func TestDiscoverRecognizesWebAndDesktop(t *testing.T)
func TestDiscoverRejectsNonDSHListener(t *testing.T)
func TestDiscoverDedupesHostsAndAppliesVariantFilter(t *testing.T)
func TestDiscoverExplicitURLSkipsProcessScan(t *testing.T)
func TestDiscoverLinuxCmdline(t *testing.T)
```

fixture 覆盖：
- macOS `ps` 行、`lsof` 输出、web `package.json`、Desktop `Info.plist`。
- Linux `/proc/<pid>/cmdline`、`ss` 输出。
- 401 fence 缺失时跳过。
- `variant=web` 只保留 web。
- 相同 `Reference` 只保留一个。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestDiscover' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现发现**

`discover.go`：
- `DiscoveryConfig.URL != ""` 时构造显式 `Host`，`Generation=sha256("explicit|"+url+"|"+home)[:24]`，
  跳过进程扫描；`Variant` 为空时默认 `web`；版本门禁跳过（显式配置由用户负责）。
  `Home` 为空时使用 `~/.dsh`（`os.UserHomeDir`）。
- 否则按 `goos` 调用平台实现：
  - `discover_darwin.go`：`ps -U <uid> -o pid=,uid=,comm=` 找 node/DeepSeek Harness；
    `lsof -nP -a -p <pid> -iTCP -sTCP:LISTEN -Fn` 找 `127.0.0.1` 端口；
    `ps -ww -p <pid> -o args=` 识别 web/Desktop；web 的脚本参数先 `filepath.EvalSymlinks`
    （`/opt/homebrew/bin/dsh` 可能是指向 `lib/bin.js` 的符号链接）；`ps -p <pid> -o uid=,lstart=`
    取启动时间；`KERN_PROCARGS2` 尽力读取 `DSH_HOME=`（失败不报错）；读取 `package.json` 或
    `Info.plist` 版本（`plutil -extract ... raw`）；`Fence(port)` 必须为 true。
  - `discover_linux.go`：扫描 `/proc/<pid>/cmdline` 与 `/proc/<pid>/environ`，用 `ss -H -ltnp`
    找监听；只识别 web。
  - `discover_other.go`：返回 `errors.New("当前平台需要显式配置 dsh.url 与 dsh.home")`。
- 若 `cfg.Home` 非空，发现到的 `Host.Home` 统一改为 `cfg.Home`（web/Desktop 可能共享凭据目录）。
- 过滤 `Host.Validate()` 失败的版本。
- 结果按 `Reference()` 去重并排序。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestDiscover' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/discover.go internal/dsh/discover_darwin.go internal/dsh/discover_linux.go internal/dsh/discover_other.go internal/dsh/discover_test.go
git commit -m "feat: 自动发现本机 dsh web 与 Desktop 宿主"
```

---

### Task 6: HTTP RPC 客户端

**Files:**
- Create: `internal/dsh/rpc.go`
- Test: `internal/dsh/rpc_test.go`

**Interfaces:**
- Produces: `type rpcClient struct { host Host; http *http.Client; cookie func(string, time.Time) (string, error); now func() time.Time }`。
- Produces: `func newRPCClient(host Host) *rpcClient`。
- Produces: `func (c *rpcClient) call(ctx context.Context, endpoint string, args any) (json.RawMessage, error)`。
- Produces: `type rpcError struct { Code, Message string }`，实现 `error`。

- [ ] **Step 1: 写失败测试**

`rpc_test.go` 使用 `httptest.NewServer` 和临时凭据文件：

```go
func TestRPCSendsCorrelatedRequest(t *testing.T)
func TestRPCRejectsMismatchedRPCID(t *testing.T)
func TestRPCRejectsUnknownEndpoint(t *testing.T)
func TestRPCBoundsResponseAndDisablesRedirect(t *testing.T)
func TestRPCErrorCarriesCode(t *testing.T)
```

断言请求方法/路径/Cookie/JSON 结构；响应 `result.value` 正确返回；错误码映射；超过 4 MiB
失败；302 不跟随。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestRPC' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`rpc.go`：
- `allowedEndpoints` map 固定 8 个 endpoint。
- `call` 生成 `rpcId`（`crypto/rand` 16 字节 hex），构造请求，Cookie 由
  `cookieForCredentials(filepath.Join(host.Home, ".credentials.yaml"), "127.0.0.1:"+port, now)`。
- `http.Client{Timeout: 5s, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2}, CheckRedirect: ...}`。
- 读取 `io.LimitReader(body, 4<<20+1)`；超限失败。
- 解析 `server-response`，校验 `type`、`rpcId`、`result.ok`。
- `ok=false` 时返回 `*rpcError{Code: result.error.code, Message: ...}`；消息截断 512 字符。
- 响应大小、请求体大小、endpoint 白名单错误使用固定中文消息，不包含 Cookie。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestRPC' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/rpc.go internal/dsh/rpc_test.go
git commit -m "feat: 实现 dsh Remote RPC 客户端"
```

---

### Task 7: 会话目录

**Files:**
- Rewrite: `internal/dsh/client.go`（替换旧插件的 `/v1/*` 客户端）
- Rewrite: `internal/dsh/client_test.go`
- Delete: `internal/dsh/executor_test.go` 中依赖旧 `API` 的用例（由 Task 12/13 重写）

**Interfaces:**
- Produces: `type Client struct { host Host; rpc *rpcClient; logger *slog.Logger }`。
- Produces: `func NewClient(host Host, logger *slog.Logger) *Client`。
- Produces: `type SessionSummary struct { ID, Title, Cwd string; Running, Available bool; UpdatedAt int64 }`。
- Produces: `func (c *Client) Sessions(ctx context.Context) ([]SessionSummary, error)`。

- [ ] **Step 1: 写失败测试**

```go
func TestSessionsFiltersUncontrolledAndSubagent(t *testing.T)
func TestSessionsCopiesTitleAndWorkspace(t *testing.T)
func TestSessionsBoundsAndValidatesIDs(t *testing.T)
```

fixture 包含 `agentAvailable=false`、`origin=subagent`、`formatStatus=migration-required`、
正常会话；断言只保留正常会话；标题缺省为 session ID；ID 为空/超长/控制字符报错；超过
256 条报错。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestSessions' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`client.go`：
- `Sessions` 调 `rpc.call(ctx, "session/list", map[string]any{"_request": map[string]any{}})`。
- 解析 `result.value.items`；最多 8192 条原始行；过滤后最多 256 条。
- `Title` 取 `projections.values.title`；`Cwd` 保留原始值；`UpdatedAt` 为 int64。
- 返回按 `UpdatedAt` 降序、ID 升序的稳定顺序。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestSessions' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/client.go internal/dsh/client_test.go
git commit -m "feat: 读取并过滤 dsh 会话目录"
```

---

### Task 8: 聊天正文读取

**Files:**
- Create: `internal/dsh/transcript.go`
- Test: `internal/dsh/transcript_test.go`
- Modify: `internal/dsh/client.go`

**Interfaces:**
- Produces: `func (c *Client) Read(ctx context.Context, sessionID string) (string, error)`。
- Produces: `func conversationText(records []json.RawMessage) (string, error)`（包内）。

- [ ] **Step 1: 写失败测试**

```go
func TestTranscriptKeepsOnlyHumanAndAssistantText(t *testing.T)
func TestTranscriptAppliesSurfaceReplacement(t *testing.T)
func TestTranscriptStripsANSIAndControl(t *testing.T)
func TestTranscriptKeepsLast100MessagesAnd16000UTF16(t *testing.T)
func TestReadRejectsMigrationOrMissingCut(t *testing.T)
```

断言：
- `user/message` 仅 `source.kind=="user"`；`assistant/message` 仅 text 块。
- `developer/message`、`tool/result`、reasoning、image 不出现。
- `surfaceOp.replace` 删除范围内旧消息。
- 输出格式 `用户：...` / `助手：...`，消息间空行。
- 101 条消息只保留最后 100 条。
- 16000 个 UTF-16 代码单元截断（emoji 边界）。
- `projections.kind != "sequenced"` 或 `asOfSeq < -1` 返回错误。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestTranscript|TestRead' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`transcript.go`：
- `conversationText` 遍历记录，维护 `messages []message{seq, role, text}`。
- 先处理 `surfaceOp` replace 删除旧消息。
- 只接受 `user/message`（`data.source.kind=="user"`）与 `assistant/message`（text 块）。
- 清理 ANSI/控制字符，截取最后 100 条，再截取最后 16000 个 UTF-16 代码单元。

`client.go` 的 `Read`：
1. `session/projections` 请求 `{"request":{"sessionId":...}}`。
2. 校验 `kind=="sequenced"` 且 `asOfSeq >= -1`。
3. 快路径：`session/page` 请求 `throughSeq=asOfSeq, maxMessages=100`。
4. 若响应超过 RPC 上限、分页失败，或尾页仍有更早历史但可见消息不足 100 条，降级为
   `maxMessages=32`、向上 `beforeSeq` 分页，最多 16 页或 100 条消息；仍超限则返回
   “历史过大，无法读取”，不返回半截正文。
5. 调 `conversationText(page.records)`；跨页时保留 `surfaceOp.replace` 范围。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestTranscript|TestRead' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/transcript.go internal/dsh/transcript_test.go internal/dsh/client.go
git commit -m "feat: 投影 dsh 聊天正文"
```

---

### Task 9: 发送、新建与重命名

**Files:**
- Modify: `internal/dsh/client.go`
- Test: `internal/dsh/client_test.go`

**Interfaces:**
- Produces: `func (c *Client) Prompt(ctx context.Context, sessionID, requestID, text string) error`。
- Produces: `type CreatedSession struct { ID, Title string; RenameFailed bool }`。
- Produces: `func (c *Client) Create(ctx context.Context, cwd, title string) (CreatedSession, error)`。

- [ ] **Step 1: 写失败测试**

```go
func TestPromptUsesQueueAndRequestID(t *testing.T)
func TestPromptRejectsEmptyAndOverlong(t *testing.T)
func TestCreateRenamesSession(t *testing.T)
func TestCreateKeepsSessionWhenRenameFails(t *testing.T)
```

断言 prompt 请求 `mode:"queue"`、`requestId`、文本；空/超过 16 KiB 失败；create 后调用
rename；rename 失败返回 `RenameFailed=true` 且 ID 保留。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestPrompt|TestCreate' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`Prompt`：
- `strings.TrimSpace(text)==""` -> 错误；`len([]byte(text)) > 16384` -> 错误。
- `requestID` 非空且长度 <= 256，否则拒绝，避免 dsh 拒绝后才暴露问题。
- `session/prompt` args 如设计文档；响应 `accepted != true` -> 错误。

`Create`：
- `session/create` args `{"request":{"cwd":cwd}}`，取 `value.sessionId`。
- 标题非空时 `session/rename`；成功取 `value.title`；失败返回已创建 ID + `RenameFailed=true`，
  不把错误当成完全失败。
- 校验 session ID <= 512。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestPrompt|TestCreate' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/client.go internal/dsh/client_test.go
git commit -m "feat: 支持 dsh 排队发送和新建会话"
```

---

### Task 10: Mux 待答事件

**Files:**
- Create: `internal/dsh/mux.go`
- Test: `internal/dsh/mux_test.go`

**Interfaces:**
- Produces: `type PendingItem struct { EventID, AgentID, Event string; Request json.RawMessage; Raw json.RawMessage }`。
- Produces: `type PendingSnapshot struct { ClientID string; Items []PendingItem }`。
- Produces: `type Mux struct`；`func newMux(host Host, logger *slog.Logger) *Mux`。
- Produces: `func (m *Mux) Run(ctx context.Context) error`、`func (m *Mux) Snapshot() PendingSnapshot`、`func (m *Mux) WaitCancel(ctx context.Context, eventID string) bool`。
- Produces: `func (c *Client) SubmitEventAnswer(ctx context.Context, clientID, eventID string, value any) error`。
- Modifies: `Client` 持有 `mux *Mux`，`NewClient(host, logger)` 创建 Mux，`Client.Run(ctx)` 启动 Mux；
  `Client.Pending(ctx)` 返回 Mux 快照。Mux 不可用时 `Pending` 返回错误，目录和读取仍可用。

- [ ] **Step 1: 写失败测试**

使用 `httptest.NewServer` + `coder/websocket` 实现 fake `/api/remote.mux`：

```go
func TestMuxTracksReadyWaterfallAndCancel(t *testing.T)
func TestMuxReconnectReplacesClientIDAndPending(t *testing.T)
func TestMuxBoundsPendingAndRejectsUnknownFrames(t *testing.T)
func TestMuxWaitCancel(t *testing.T)
func TestSubmitEventAnswerUsesCorrelatedRPC(t *testing.T)
```

断言 `ready` 后 clientId；waterfall 入表；cancel 删除；重连清空；超过 256 条失败；未知
frame 报错；`SubmitEventAnswer` 发送 `$events/result` 的 args 结构。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestMux|TestSubmitEvent' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`mux.go`：
- 建立 `ws://127.0.0.1:<port>/api/remote.mux`，Cookie 头；`SetReadLimit(4<<20)`。
- 发送 `{"type":"open","streamId":"events","endpoint":"$events","payload":{"args":{}}}`。
- 读取 `{"streamId":"events","type":"item","value":...}`；只处理 `streamId=="events"`。
- `ready` 记录 clientId 并清空；`waterfall` 校验 eventId/agentId 后入表（最多 256 条、
  单条 <=128 KiB、总量 <=1 MiB）；`cancel` 删除并广播；`emit` 忽略。
- `Done()` channel 在收到 cancel 时广播；`WaitCancel` 最多等 2 秒。
- 断线后 1s 起始指数退避重连（最大 30s）；退出时关闭 socket。
- `SubmitEventAnswer` 调 `rpc.call(ctx, "$events/result", map[string]any{...})`，校验 `ok`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestMux|TestSubmitEvent' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/mux.go internal/dsh/mux_test.go internal/dsh/client.go
git commit -m "feat: 订阅 dsh Mux 待答事件"
```

---

### Task 11: 决策模型与答案

**Files:**
- Create: `internal/dsh/decisions.go`
- Create: `internal/dsh/answers.go`
- Test: `internal/dsh/decisions_test.go`
- Test: `internal/dsh/answers_test.go`

**Interfaces:**
- Produces: `type Decision struct { NativeID, SessionID, Kind, Title string; Questions []Question; Revision string; Blocking, Supported bool; Reason string }`。
- Produces: `type Question struct { ID, Title, Description, Kind string; Options []Option; Required bool }`。
- Produces: `type Option struct { Value, Label, Description string }`。
- Produces: `func decisionsFrom(sessionID string, pending PendingSnapshot, projections json.RawMessage) ([]Decision, error)`。
- Produces: `func formatDecision(decision Decision, questionIndex int) string`。
- Produces: `type Answer struct { Selected []string; Custom string }`。
- Produces: `func parseAnswer(indexes []int, text string, question Question) (Answer, error)`。
- Produces: `func buildAnswerValue(decision Decision, answers map[string]Answer) (any, error)`。
- Produces: `type AnswerReceipt struct { Confirmed bool; Message string }`。
- Produces: `func (c *Client) Decisions(ctx context.Context, sessionID string) ([]Decision, error)`：
  调用 `session/projections` 并与 `c.Pending(ctx)` 合并。
- Produces: `func (c *Client) SubmitAnswer(ctx context.Context, decision Decision, value any) (AnswerReceipt, error)`。

- [ ] **Step 1: 写失败测试**

`decisions_test.go`：

```go
func TestDecisionsCombineWaterfallAndContinued(t *testing.T)
func TestApprovalIsBlockingAndOnlyAllowsTwoOutcomes(t *testing.T)
func TestTimedQuestionIsNotBlocking(t *testing.T)
func TestUnknownWaterfallIsUnsupported(t *testing.T)
func TestContinuedQuestionRequiresContinuedState(t *testing.T)
func TestDecisionRevisionChangesWithFrame(t *testing.T)
```

`answers_test.go`：

```go
func TestParseAnswerNumbersAndCustomText(t *testing.T)
func TestParseAnswerRejectsOutOfRangeAndDuplicate(t *testing.T)
func TestApprovalRejectsCustomText(t *testing.T)
func TestBuildAnswerValueForApprovalAndForm(t *testing.T)
func TestSubmitAnswerUsesEventOrCallEndpoint(t *testing.T)
```

断言：
- 审批 `NativeID=event:<id>`，选项 `allowed-once/rejected`，`Blocking=true`。
- 定时问题 `Blocking=false`。
- 未知交互 `Supported=false`、`Blocking=false`。
- 持续问题仅 `state=="continued"` 可答，且保持 `Blocking=false`（Task 13 的选择规则是
  “第一个 blocking，否则第一个 supported 的非 blocking 决策，包括 timed 问题和持续问题”）。
- `/answer 1,2 自定义` 映射到 selected+custom。
- 审批提交 `$events/result`，持续问题提交 `userQuestions/answer`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestDecision|TestApproval|TestParseAnswer|TestBuildAnswer|TestSubmitAnswer' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`decisions.go`：
- 遍历 `PendingSnapshot.Items`：
  - `event=="approval/request"` -> approval，`Blocking=true`，选项固定。
  - `event=="user-questions/request"` -> form；`request.wait.timed==true` 时 `Blocking=false`。
  - 其他 `type=="waterfall"` -> unsupported。
  - 校验问题数量 1..32、ID 唯一、选项 label 非空、文本长度。
- 合并 `projections.values.userQuestions.active`；`state=="continued"` 的 `call:<id>` 可答，
  否则 unsupported；若 waterfall 已包含同一 `callId`（timed），跳过投影重复。
- `Revision = sha256(canonical raw frame)`。
- `formatDecision` 输出选项编号、标题、描述和 `/answer` 提示；多题显示 `(i/n)`。

`answers.go`：
- `parseAnswer` 按 `Question.Kind` 校验：
  - approval：仅 1/2，无自定义。
  - single：最多一个编号 + 可选自定义；至少一个。
  - multiple：编号可多个 + 可选自定义；至少一个。
  - text：无编号，自定义非空。
  - 编号越界、重复、自定义超过 16384 字符失败。
- `buildAnswerValue`：approval -> 字符串；form -> `{"answers":[{"id","selected","custom"}]}`。
- `SubmitAnswer`：
  - approval / waterfall form（`event:`）-> 先用 `c.Pending(ctx)` 复核 eventId 仍存在且
    `clientId` 非空，再 `SubmitEventAnswer`；成功后调用 `c.mux.ConfirmSubmitted(clientID,
    eventID)` 本地注销待答事件并返回 `Confirmed=true`（dsh 不向获胜客户端发送原生 cancel）；
    本地无法确认时 `Confirmed=false`。
  - continued（`call:`）-> 重新读取 `session/projections`，确认同一 `callId` 仍处于可答状态且
    revision 未变化，再调用 `userQuestions/answer`；`true` 视为已进入原生队列。
  - 失败/超时返回 `Confirmed=false` 和固定提示，不重试。
- `Client.Decisions` 每次先 `session/projections`，再把 `c.Pending(ctx)` 与投影合并；Mux
  不可用时仍返回投影里的持续问题，投影不可用时仍返回 Mux 里的审批；两者都失败才返回错误。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestDecision|TestApproval|TestParseAnswer|TestBuildAnswer|TestSubmitAnswer' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/decisions.go internal/dsh/answers.go internal/dsh/decisions_test.go internal/dsh/answers_test.go
git commit -m "feat: 解析 dsh 审批问题并构造答案"
```

---

### Task 12: 单宿主执行器核心

**Files:**
- Rewrite: `internal/dsh/executor.go`
- Modify: `internal/dsh/executor_test.go`

**Interfaces:**
- Produces: `type API interface { Sessions(ctx) ([]SessionSummary, error); Pending(ctx) (PendingSnapshot, error); Read(ctx, sessionID string) (string, error); Prompt(ctx, sessionID, requestID, text string) error; Create(ctx, cwd, title string) (CreatedSession, error); Decisions(ctx, sessionID string) ([]Decision, error); SubmitAnswer(ctx, decision Decision, value any) (AnswerReceipt, error) }`。
- 执行器不启动 `Client` 的 Mux；由 Task 14 的 `Backend` 同时运行 `client.Run` 和
  `executor.Run`。执行器测试使用 fake `API`，不依赖 Mux。
- Produces: `type Executor struct`；`func NewExecutor(host Host, api API, sink Sink, guard *policy.Guard) *Executor`。
- Produces: `func (e *Executor) Run(ctx context.Context) error`、`CurrentTargets`、`SelectTarget`、`SelectedTarget`。
- Produces: `func (e *Executor) hostTarget() session.Target`、`func (e *Executor) sessionTarget(s SessionSummary) session.Target`。

- [ ] **Step 1: 写失败测试**

```go
func TestExecutorListsOnlyAvailableSessionsAndHostEntry(t *testing.T)
func TestExecutorTargetIDsIncludeHostIdentity(t *testing.T)
func TestExecutorBlockedStatusAndNotification(t *testing.T)
func TestExecutorOfflineWithdrawsTargets(t *testing.T)
func TestExecutorSelectionRejectsStaleOccupant(t *testing.T)
func TestExecutorNotifiesOncePerTransition(t *testing.T)
```

断言：
- 有会话时不出现宿主入口；无会话时出现一个 `dsh-host` 目标。
- 同一 session ID 在不同 Host 上生成不同 PaneID。
- `running=true` -> working；blocking decision -> blocked；否则 idle。
- API 错误 -> `CurrentTargets()` 为空。
- 宿主 generation 变化或会话消失 -> 旧 occupant 不再可选。
- 相同状态连续刷新只发一次通知；idle 状态只有 `UpdatedAt` 变化时才再次通知。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestExecutor' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- 保留现有 `Sink` 和 `panel.Buffer` 结构。
- `refresh` 调 `Sessions` 和 `Pending`：
  - 构建 `map[paneID]SessionSummary`。
  - 按 `Pending.Items` 的 `agentId` 分组，判断 blocking：`approval/request`，或
    `user-questions/request` 且 `request.wait.timed != true`。
  - `Decisions` 只在 `/con` 和 `/answer` 时按会话调用，避免每秒为每个会话读投影。
  - 状态/id 变化时生成通知，`PreviousStatus` 取上次状态；idle 转换通知状态用 `done`。
    相同 idle 状态只有 `UpdatedAt` 变化时才再次通知，避免每秒重复。
  - 首次观察到 blocking 决策也必须通知，不能因为没有旧状态而漏掉。
  - API 出错时 `sessions` 清空并保留 selected 失效；`Pending` 单独失败只记录日志，不撤下目录。
- `CurrentTargets()` 排序：宿主入口（仅无会话时）在前，其余按标题、ID 升序。
- `SelectTarget`/`SelectedTarget` 复核完整身份。
- `targetOf` 使用设计文档中的 hash 规则；会话的 `Workspace` 取 `filepath.Base(Cwd)`（为空时
  `dsh`），`Tab` 取 `host.Variant`（`web`/`desktop`）；宿主入口的 `Title="新建会话"`，
  `Workspace=host.Variant`，`Tab="dsh-host"`；`DisplayAgent` 固定 `DeepSeek Harness`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestExecutor' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/executor.go internal/dsh/executor_test.go
git commit -m "feat: 实现 dsh 单宿主执行器目录与状态"
```

---

### Task 13: 执行器命令

**Files:**
- Modify: `internal/dsh/executor.go`
- Modify: `internal/dsh/executor_test.go`

**Interfaces:**
- Produces: `func (e *Executor) HandleMessage(ctx context.Context, message im.IncomingText)` 支持 `KindPrompt`、`KindContent`、`KindPageUp`、`KindPageDown`、`KindAnswer`、`KindNewSession`、`KindKey`。
- Produces: `func (e *Executor) ReadTerminalSnapshot(ctx context.Context, paneID, occupant string, mode im.OutputMode, maxLines int) (im.TerminalContent, error)`。
- Produces: `func (e *Executor) TakeCreatedTarget() (session.Target, bool)`。

- [ ] **Step 1: 写失败测试**

```go
func TestExecutorPromptQueuesWhileRunning(t *testing.T)
func TestExecutorPromptRejectedWhenBlocked(t *testing.T)
func TestExecutorReadAppendsPendingApproval(t *testing.T)
func TestExecutorAnswerCollectsMultipleQuestions(t *testing.T)
func TestExecutorAnswerRejectsStaleRevision(t *testing.T)
func TestExecutorNewSessionSelectsCreatedTarget(t *testing.T)
func TestExecutorHostEntryOnlySupportsNewSession(t *testing.T)
```

断言：
- running 时 prompt 调用一次，回复“已发送”。
- blocked 时 prompt 不调用 API。
- `/con` 文本包含 `1. 允许本次` 和 `/answer`。
- 两题表单第一次 `/answer` 不提交，第二次提交完整答案。
- revision 变化时提交失败并清空状态。
- `/new-session 标题 /tmp` 调 Create，selected 变为新目标，`TakeCreatedTarget` 返回新目标。
- 宿主入口收到普通文本只提示 `/new-session`。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestExecutor' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `HandleMessage` 先校验 `policy.Guard` 和 `MessageID`；每次开始时清空 `createdTarget`，只在
  `/new-session` 成功后设置。
- `KindPrompt`：blocked -> 拒绝；否则 `api.Prompt`；成功回复“已发送给 dsh（排队）”。
- `KindContent`/`KindPageUp`/`KindPageDown`：调 `Read`，使用 `panel.Buffer` 分页；在文本末尾
  追加当前决策提示（`formatDecision`）。
- `KindAnswer`：用 `answerStates` 管理逐题答案；没有活动状态时选择“第一个 blocking 决策，
  否则第一个 supported 的非 blocking 决策（timed 问题或持续问题）”；调用
  `parseAnswer`/`buildAnswerValue`；多题回复下一题；最后一题提交并处理 receipt。
- `KindNewSession`：锚点必须是本宿主目标；解析 `Title/Directory`；默认目录
  `os.UserHomeDir()`；`filepath.Abs` + `os.Stat` 校验目录；调 `api.Create`；把新会话插入
  `sessions`，设置 `selected` 和 `createdTarget`；回复标题/目录/失败信息。新建会话在
  `session/list` 出现前保留在本地目录最多 5 秒，避免创建后立即消失；超过 5 秒仍未出现则移除。
- `KindKey`：回复“dsh 不支持终端按键；审批请使用 /answer”。
- `ReadTerminalSnapshot` 对宿主入口返回说明；对普通会话读取正文 + 待处理提示；按 `maxLines`
  截断。正文与提示合计不超过 20000 个 UTF-16 单元，超长时先截断正文、保留提示。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/executor.go internal/dsh/executor_test.go
git commit -m "feat: 支持 dsh 读取、排队发送、审批与新建会话"
```

---

### Task 14: 多宿主 Backend

**Files:**
- Create: `internal/dsh/backend.go`
- Test: `internal/dsh/backend_test.go`

**Interfaces:**
- Produces: `type Backend struct`；`func NewBackend(cfg DiscoveryConfig, sink Sink, guard *policy.Guard, logger *slog.Logger) (*Backend, error)`。
- Produces: `func (b *Backend) Run(ctx context.Context) error`，实现 `relayclient.Executor`。
- Produces: `func (b *Backend) CurrentTargets() []session.Target`、`SelectTarget`、`SelectedTarget`、`HandleMessage`、`ReadTerminalSnapshot`、`TakeCreatedTarget`。
- Produces: 测试注入点 `type discoverFunc func(context.Context, DiscoveryConfig) ([]Host, error)` 和
  `type hostFactory func(Host) (*hostWorker, error)`；`hostWorker` 包含 `client *Client` 和
  `executor *Executor`。`newBackend` 接收两者。
- Modifies: `ExecutorMux` 实现 `TakeCreatedTarget()`，委托给当前选中的 dsh Backend；Herdr
  backend 不实现该接口。

- [ ] **Step 1: 写失败测试**

使用 fake discoverer + fake host factory：

```go
func TestBackendAddsAndRemovesHosts(t *testing.T)
func TestBackendRoutesSelectionAndMessagesByTarget(t *testing.T)
func TestBackendKeepsOtherHostsWhenOneFails(t *testing.T)
func TestBackendTakeCreatedTargetDelegatesToSelectedHost(t *testing.T)
```

断言：新增宿主出现目标；移除宿主目标消失；选择路由到正确执行器；一个宿主错误不影响另一个；
`TakeCreatedTarget` 从选中执行器取。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestBackend' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `Backend` 持有 `hosts map[string]*hostWorker`、`selected *hostWorker`。
  `hostWorker` 包含 `hostCtx`、`cancel`、`client`、`executor`。
- `Run` 每 2 秒调 `discoverFunc`；新增宿主创建 `Client`（含 Mux）+ `Executor`，在独立
  goroutine 中分别运行 `client.Run` 和 `executor.Run`；消失宿主 `cancel`、等待退出、从 map 删除。
- `CurrentTargets` 汇总并限制 256；排序稳定。
- `SelectTarget` 按完整目标遍历宿主；`HandleMessage`/`ReadTerminalSnapshot` 路由到 owner。
- `TakeCreatedTarget` 遍历宿主执行器的 `TakeCreatedTarget()`，返回并清除第一个创建目标；
  不依赖消息解析，避免与 relay 的读取顺序耦合。
- `ExecutorMux` 增加 `TakeCreatedTarget`，若选中 backend 实现该接口则委托，否则返回 false。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/dsh -run 'TestBackend' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/backend.go internal/dsh/backend_test.go
git commit -m "feat: 聚合多个 dsh 宿主"
```

---

### Task 15: HPRP `created_target`

**Files:**
- Modify: `internal/hprp/messages.go`
- Modify: `internal/hprp/validate.go`
- Modify: `internal/hprp/messages_test.go`
- Modify: `internal/hprp/validate_test.go`
- Modify: `internal/relayclient/client.go`
- Modify: `internal/relayclient/mux.go`
- Modify: `internal/relayclient/client_test.go`
- Modify: `internal/server/hub.go`
- Modify: `internal/server/hub_test.go`
- Modify: `internal/server/catalog.go`
- Modify: `internal/server/catalog_test.go`
- Modify: `internal/server/router.go`

**Interfaces:**
- Produces: `CommandResult.CreatedTarget *Target `json:"created_target,omitempty"``。
- Produces: `relayclient.CreatedTargetProvider interface { TakeCreatedTarget() (session.Target, bool) }`。
- Produces: `RelayExecution.CreatedTarget *hprp.Target`。
- Produces: `func (catalog *SessionCatalog) RebindCreated(ctx context.Context, userID string, expected, created hprp.Target) error`。

- [ ] **Step 1: 写失败测试**

```go
func TestCommandResultRoundTripsCreatedTarget(t *testing.T)
func TestValidateCommandResultRejectsInvalidCreatedTarget(t *testing.T)
func TestRelayClientIncludesCreatedTargetAfterSnapshot(t *testing.T)
func TestHubRejectsCreatedTargetForAnotherMachineOrMissingSnapshot(t *testing.T)
func TestCatalogRebindCreatedAllowsDifferentSlot(t *testing.T)
func TestCatalogRebindCreatedWaitsForSnapshot(t *testing.T)
```

断言：字段编解码；非法 target 失败；Pal 返回 created_target 前上报快照；Hub 要求同机器且
目标已在确认快照；`RebindCreated` 允许不同 SlotID、拒绝不同机器/不存在目标。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/hprp ./internal/relayclient ./internal/server -run 'CreatedTarget|RebindCreated' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `hprp.CommandResult` 增加字段；`ValidateCommandResult` 校验 `ValidateTarget`。
- `relayclient.respondContent`：如果执行器实现 `CreatedTargetProvider` 且返回目标，先
  `reportCurrentSnapshot`，再把 `session.Target` 转成
  `hprp.Target{MachineID: current.machineID, SlotID: t.PaneID, SessionID: t.OccupantKey}`，
  放进 `CreatedTarget`，并校验目标存在于 `CurrentTargets()`。
- `server.ClientHub.Execute`：校验 `CreatedTarget.MachineID == target.MachineID`，且
  `catalog.ResolveTarget` 成功；返回 `RelayExecution.CreatedTarget`。
- `SessionCatalog.RebindCreated`：校验同机器、目标存在；等待目录更新；切换选择；不改现有
  `RebindSelection`。
- `ConversationRouter.RebindSelectedExecution` 优先处理 `CreatedTarget`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/hprp ./internal/relayclient ./internal/server -run 'CreatedTarget|RebindCreated' -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/hprp internal/relayclient/client.go internal/relayclient/client_test.go internal/server/hub.go internal/server/hub_test.go internal/server/catalog.go internal/server/catalog_test.go internal/server/router.go
git commit -m "feat: 支持新建会话后的稳定目标切换"
```

---

### Task 16: Server `/new-session` 路由与帮助

**Files:**
- Modify: `internal/server/router.go`
- Modify: `internal/server/router_test.go`
- Modify: `internal/server/default_help.md`
- Modify: `help.md`

**Interfaces:**
- Produces: `serverActionNewSession`，解析 `/new-session [标题] [目录]`。
- Produces: `func (router *ConversationRouter) resolveDSHAnchor(userID string, selected CatalogEntry, hasSelection bool) (CatalogEntry, error)`。
- Produces: `func (router *ConversationRouter) handleNewSession(ctx, message, action)`。

- [ ] **Step 1: 写失败测试**

```go
func TestParseServerActionNewSessionBypassesEmptyCatalog(t *testing.T)
func TestNewSessionUsesSelectedDSHSessionHost(t *testing.T)
func TestNewSessionUsesOnlyDSHHostEntryWhenNoSessions(t *testing.T)
func TestNewSessionRequiresSelectionForMultipleHosts(t *testing.T)
func TestNewSessionRebindsToCreatedTarget(t *testing.T)
func TestNewSessionWithoutDSHReportsUnavailable(t *testing.T)
```

断言：无会话时 `/new-session` 不被 `noAvailableSessionsMessage` 拦截；有选中 dsh 会话时发给
同机器宿主入口；只有一个 dsh 宿主入口时直接使用；多个宿主无选择时提示 `/ls`/`/N`；返回
`created_target` 后选择切换；无 dsh 时回复“没有在线的 dsh 宿主”。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server -run 'TestNewSession|TestParseServerActionNewSession' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `parseServerAction` 增加 `/new-session` 和 `/answer`，在 `serverAction` 保存原始 `content`；
  `handleAuthorized` 的 `HasSessions` 检查对 `serverActionNewSession` 放行。
  `serverActionName` 增加 `new_session` 和 `answer`；`serverActionAnswer` 路由到
  `handleExecute`，保持 `/answer` 原样转发给 Pal。`userInputAuditSource` 把
  `serverActionAnswer` 当作当前选择目标。
- `resolveDSHAnchor`：
  - 选中目标 `Display.Agent=="dsh"`：找同机器、`Display.Tab=="dsh-host"` 的入口；没有则用同机器
    第一个 dsh 会话作为锚点。
  - 未选中：收集用户所有 dsh 入口；恰好一个则用；否则收集 dsh 会话，只有一个机器则用该机器
    第一个；否则返回“请先 /ls 和 /N 选择 dsh 宿主”。
- `handleNewSession` 使用 `relay.Execute` 发送原命令到锚点；结果有 `CreatedTarget` 时调用
  `catalog.RebindCreated`，回复包含“已切换到新会话”；否则提示 `/ls`。
- 更新两处帮助文本：`/answer`、`/new-session` 说明和 dsh-only 限制。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/server -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/router.go internal/server/router_test.go internal/server/default_help.md help.md
git commit -m "feat: 支持企微 /new-session 路由"
```

---

### Task 17: 应用接线与移除旧 token 配置

**Files:**
- Modify: `internal/app/dsh.go`
- Modify: `internal/app/relay.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/config/client.go`
- Modify: `internal/config/dsh_test.go`
- Modify: `internal/installer/client_config.go`
- Modify: `internal/installer/client_config_test.go`
- Modify: `config.example.json`

**Interfaces:**
- Produces: `func runDSHRelay(ctx, loaded, logger)` 使用新的 `internal/dsh.NewBackend`。
- Produces: `loaded.DSH != nil` 时把 `dsh.Backend` 接入 `relayclient.NewExecutorMux`。
- Removes: `DSHConfig.Token`；`UnmarshalJSON` 遇到 `token` 返回迁移错误。

- [ ] **Step 1: 写失败测试**

```go
func TestAppStartsDSHOnlyBackendFromEmptyConfig(t *testing.T)
func TestAppCombinesHerdrAndDSHBackends(t *testing.T)
func TestAppRejectsLegacyDSHToken(t *testing.T)
```

`internal/config/dsh_test.go` 恢复 `TestDSHConfigRejectsLegacyToken`，断言
`{"dsh":{"token":"x"}}` 返回“dsh.token 已移除”。应用测试使用 fake relay server / fake Herdr，
断言 dsh 后端启动、组合执行器包含 dsh 目标、配置迁移错误在启动前返回。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/app ./internal/config -run 'TestApp.*DSH|TestDSHConfigRejectsLegacyToken' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

- `internal/config/client.go`：删除 `DSHConfig.Token`；`UnmarshalJSON` 读取 `token` 后非空即
  返回 `errors.New("dsh.token 已移除：Pal 现在直接连接 dsh Remote API")`；删除
  `dshlegacy` 导入。
- `internal/app/dsh.go`、`relay.go`：导入从 `dshlegacy` 改为新的 `internal/dsh`；`runDSHRelay`
  创建 `dsh.NewBackend(DiscoveryConfig{...}, relay, guard, logger)`；`relay.SetExecutor(backend)`；
  `runRelayComponents(ctx, relay.Run, backend.Run, ...)`。
- `internal/installer/client_config.go`：顶层字段白名单加入 `dsh`，安装器合并配置时保留
  `dsh` 对象；`client_config_test.go` 增加“已有 dsh 节点在安装后仍保留”的测试。
- `relay.go`：`if loaded.DSH != nil { backend, err := dsh.NewBackend(...); executor = NewExecutorMux(service, backend); runSupervisor = pair(...) }`。
- 进程锁：自动发现使用 `shortHash("dsh:auto")`；显式 URL 使用 `shortHash("dsh:"+url)`。
- 日志只记录宿主数量、variant、版本，不记录 Cookie/凭据。

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/app ./internal/config ./internal/dsh -count=1
go test ./... -run '^$'
```
Expected: PASS；`internal/dshlegacy` 仍编译但已不被应用引用。

- [ ] **Step 5: 提交**

```bash
git add internal/app/dsh.go internal/app/relay.go internal/app/app_test.go internal/config/client.go internal/config/dsh_test.go internal/installer/client_config.go internal/installer/client_config_test.go config.example.json
git commit -m "feat: 接入 dsh 免插件后端并移除旧 token"
```

---

### Task 18: 移除 Node 插件并补集成测试

**Files:**
- Delete: `plugins/dsh-herdr-pal/`（整个目录）
- Delete: `internal/dshlegacy/`（旧插件客户端过渡包）
- Modify: `build.sh`
- Modify: `unittest.sh`
- Modify: `README.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/DSH_INTEGRATION_PLAN.md`
- Modify: `docs/DSH_CODE_REVIEW.md`
- Modify: `internal/buildscript/documentation_test.go`
- Modify: `internal/buildscript/build_test.go`
- Modify: `packaging/bundle/README.md`
- Delete/Replace: `internal/integration/dsh_test.go`
- Create: `internal/testkit/dsh_native.go`
- Create: `internal/integration/dsh_native_test.go`

**Interfaces:**
- Produces: `testkit.NewDSHHost(t, testkit.DSHHostOptions) *testkit.DSHHost`，提供：
  - `URL() string`、`Home() string`、`Close()`。
  - HTTP `/api/session/list|projections|page|prompt|create|rename|userQuestions/answer|$events/result`。
  - WebSocket `/api/remote.mux`，可注入 `ready/waterfall/cancel` 帧。
  - 可配置 `Version/Variant/Running/Sessions/Decisions`。

- [ ] **Step 1: 写 fake 宿主**

`internal/testkit/dsh_native.go` 使用 `httptest.NewServer` 和 `coder/websocket`：
- 生成临时 `.credentials.yaml`（0600），按 Task 3 的 Cookie 规则校验请求。
- 记录收到的 RPC 方法和 args，供断言。
- 支持注入会话、投影、页面、待答事件和 RPC 错误。
- WebSocket 在收到 `open $events` 后发送 `ready`，随后发送配置的 waterfall/cancel。

- [ ] **Step 2: 写集成测试**

`internal/integration/dsh_native_test.go`：

```go
func TestDSHNativeThroughProductionAppAndWeComRouter(t *testing.T)
func TestDSHNativeApprovalAndNewSession(t *testing.T)
```

覆盖：
- 单 fake 宿主通过生产 Pal/HPRP/Router：`/ls`、`/N`、`/con`、普通文本、审批、`/new-session`。
- `/con` 只出现用户/助手文本，不出现工具/推理/内部注入。
- 普通文本在 running 时进入 queue；重复 IM 消息只提交一次。
- 审批：blocked 通知内容包含工具名/原因；`/answer 1` 提交 `allowed-once`；多题逐题收集后
  一次提交。
- `/new-session 新任务 /tmp` 创建并自动切换；新目标出现在快照后旧入口消失。
- web/Desktop 双宿主不重叠由 `internal/dsh/backend_test.go` 的 fake 双宿主测试覆盖；
  `Backend` 的 `newBackend` 注入点允许测试直接构造两个宿主。
- 关闭 fake 宿主后该宿主目标撤下，Pal 目录清空但进程继续运行。

- [ ] **Step 3: 移除插件与构建步骤**

- 删除 `plugins/dsh-herdr-pal/` 与 `internal/dshlegacy/`；确认没有残留导入。
- `build.sh` 删除：
  ```sh
  npm --prefix plugins/dsh-herdr-pal run build
  node --test plugins/dsh-herdr-pal/test/*.test.mjs
  npm pack ./plugins/dsh-herdr-pal ...
  ```
- `unittest.sh` 删除 Node 测试行。
- 更新文档：删除插件安装步骤，改为“自动发现 dsh web/Desktop；旧 `dsh.token` 配置会报错，
  请删除 token 字段”；说明只列有控制权会话和 `/answer`/`/new-session`。已安装旧插件的用户
  可从 dsh profile 移除 `herdr-pal-bridge`；不卸载也不会被新 Pal 使用，但仍会监听旧端口。
- `internal/buildscript/documentation_test.go` 更新发布说明断言，不再要求 dsh 插件 `.tgz`。

- [ ] **Step 4: 运行集成测试**

Run: `go test ./internal/integration -run 'TestDSHNative' -count=1 -v`
Expected: PASS；测试不要求 Node.js，不访问模型或企微网络。

- [ ] **Step 5: 提交**

```bash
git add -A
git commit -m "refactor: 移除 dsh Node 插件并改用原生 API"
```

注意：提交前确认没有把临时凭据、Cookie 或 `.tmp` 文件加入暂存区。

---

### Task 19: 全量验证与真实 dsh 只读核对

**Files:**
- Create: `internal/dsh/real_darwin_test.go`（opt-in，默认 skip）
- Modify: `docs/DSH_NATIVE_API_PLAN.md`（顶部加“已完成/历史实施记录”横幅，说明过渡期
  `internal/dshlegacy` 已删除，不要把 Task 1/17 的过渡步骤当作待执行操作）
- No production code changes.

- [ ] **Step 1: 格式与静态检查**

Run:
```sh
gofmt -l .
go vet ./...
```
Expected: `gofmt -l .` 无输出；`go vet` 成功。

- [ ] **Step 2: 全量单元/集成/竞态测试**

Run: `./unittest.sh`
Expected: 全部 PASS，包括 race（CGO 启用时）。

- [ ] **Step 3: 全量构建**

Run: `./build.sh`
Expected: 成功生成 Darwin/Linux/Windows 二进制和 SHA256SUMS；不再生成 dsh 插件 `.tgz`。

- [ ] **Step 4: 真实 dsh 只读核对（opt-in，需要用户在场）**

新增 `internal/dsh/real_darwin_test.go`：

```go
func TestRealDSHHostReadOnly(t *testing.T) {
    if os.Getenv("HERDR_PAL_REAL_DSH") != "1" { t.Skip("设置 HERDR_PAL_REAL_DSH=1 后运行") }
    hosts, err := discoverHosts(context.Background(), DiscoveryConfig{}, systemProber{}, "darwin")
    // 至少 1 个宿主；对每个宿主 NewClient、Sessions、对第一个会话 Read
    // 只断言不报错、正文可读、日志不含 Cookie/secret；不调用 Prompt/SubmitAnswer
}
```

Run:
```sh
HERDR_PAL_REAL_DSH=1 go test ./internal/dsh -run TestRealDSHHostReadOnly -count=1 -v
```
Expected: 发现本机 dsh 宿主，目录和正文读取成功；不发送 prompt、不回答审批。若版本/认证格式
不兼容，测试失败并输出固定错误，不尝试绕过。

- [ ] **Step 5: 最终提交**

```bash
git status --short
git add -A
git commit -m "chore: 完成 dsh 免插件接入验证"
```

---

### Task 20: 同步参考实现的答案确认与持续问题复核

参考仓库提交 `c0c656958` 修正了两个答案边界：
- dsh 不会向提交成功的客户端发送原生 `cancel`，宿主在 `$events/result` 成功后本地注销待答
  事件并注入 cancel；否则提交成功却一直显示“未确认”，且 `/con` 继续展示已回答的审批。
- 持续问题提交前必须重新读取 `session/projections`，确认同一 `callId` 仍可答且 revision
  未变化；直接调用 `userQuestions/answer` 可能回答已修订或已结束的问题。

本任务把这两点合入当前 Go 实现。参考的 completion/metadata、流式尾部、WASM v1 缓存和
`pending_revision` 不在本次范围；当前轮询模型不需要这些结构。

**Files:**
- Modify: `internal/dsh/mux.go`
- Modify: `internal/dsh/mux_test.go`
- Modify: `internal/dsh/answers.go`
- Modify: `internal/dsh/answers_test.go`
- Modify: `docs/DSH_NATIVE_API_DESIGN.md`（已更新提交语义）

**Interfaces:**
- Produces: `func (m *Mux) ConfirmSubmitted(clientID, eventID string) bool`：仅当 `clientID`
  仍是当前 Mux 身份且 `eventID` 仍在待答表中时，移除该事件、记录取消事实、广播等待者并返回
  true；身份已换代或事件已不在时返回 false。
- Modifies: `Client.SubmitAnswer` 的 `event:` 路径在 `SubmitEventAnswer` 成功后调用
  `ConfirmSubmitted` 并据其返回 `Confirmed`；不再依赖服务器 cancel。
- Modifies: `Client.SubmitAnswer` 的 `call:` 路径在提交前重新读取 projections，找到同一
  `callId`，要求 `Supported=true` 且 revision 与传入 decision 一致，否则返回 stale 回执且
  不调用 `userQuestions/answer`。

- [ ] **Step 1: 写失败测试**

`internal/dsh/mux_test.go`：

```go
func TestConfirmSubmittedRemovesPendingAndConfirms(t *testing.T)
func TestConfirmSubmittedRejectsStaleClientOrEvent(t *testing.T)
```

断言：ready/waterfall 后 `ConfirmSubmitted(current,event)` 返回 true，`Snapshot` 不再含该事件，
`WaitCancel` 立即 true；旧 clientId 或未知 eventId 返回 false 且不改变待答表。

`internal/dsh/answers_test.go`：

```go
func TestSubmitAnswerConfirmsLocallyAfterAcceptedEvent(t *testing.T)
func TestSubmitAnswerRejectsStaleContinuedQuestion(t *testing.T)
```

断言：事件提交成功后 `Confirmed=true` 且待答项被移除，不要求服务器 cancel；持续问题在
projections 中 revision 变化/state 非 continued 时不调用 `userQuestions/answer` 并返回
未确认。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/dsh -run 'TestConfirmSubmitted|TestSubmitAnswerConfirmsLocally|TestSubmitAnswerRejectsStaleContinued' -count=1`
Expected: FAIL（新方法/新行为未实现）。

- [ ] **Step 3: 实现**

`mux.go`：
- 在锁内校验 `m.clientID == clientID` 且 `pending[eventID]` 存在；删除 pending、记录 cancel
  事实、换出并关闭等待 channel，返回 true；否则返回 false。
- 复用现有 cancel 历史/FIFO 逻辑，避免重复确认造成误删新事件。

`answers.go`：
- event 路径：`SubmitEventAnswer` 成功后调用 `c.mux.ConfirmSubmitted(clientID, eventID)`；
  true -> `AnswerReceipt{Confirmed:true, Message:"已确认..."}`；false -> 未确认提示。
- call 路径：先 `session/projections`，调用现有 `decisionsFromProjections`/`continuedDecision`
  找到同一 `callId`；要求 `Supported` 且 `Revision == decision.Revision`，否则返回
  `Confirmed=false` 的 stale 提示；通过后再 `userQuestions/answer`。

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/dsh -run 'TestConfirmSubmitted|TestSubmitAnswer|TestMux' -count=1
go test ./internal/dsh -count=1
go test -race ./internal/dsh -count=1
```
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/dsh/mux.go internal/dsh/mux_test.go internal/dsh/answers.go internal/dsh/answers_test.go docs/DSH_NATIVE_API_DESIGN.md
git commit -m "fix: 同步参考实现的答案确认与持续问题复核"
```

---

## 自检

- [ ] 每个设计目标都有对应任务：发现、认证、目录、读取、发送、审批、新建、多宿主、移除插件。
- [ ] 任务接口名称前后一致：`SessionSummary`、`Decision`、`Question`、`Answer`、`CreatedTarget`。
- [ ] 每个任务都有失败测试、实现、通过测试和提交步骤。
- [ ] Review Focus 的 8 个风险分别由 Task 3、5、12/14、11/13、15/16、12/14、8、13 覆盖。
- [ ] 没有把 Node 插件保留为隐式后备；旧 `dsh.token` 明确报错。
- [ ] 真实 dsh 验证只读，不发送 prompt、不回答审批。
