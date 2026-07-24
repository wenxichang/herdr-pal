# 多用户 Relay Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 Herdr Pal 改造成一个企微机器人服务多个用户、每个用户连接多台 Herdr 机器的 Relay Server 架构，同时保留本机交互模式。

**Architecture:** `herdr-pal-server` 独占企业微信长连接并维护用户级目录与选择；每台 `herdr-pal` 通过 WSS 上报本机完整会话快照，并在本机执行选择、prompt、分页和按键。Relay 协议使用严格、带版本的 JSON 文本帧，服务端状态只保存在内存，断线立即移除。

**Tech Stack:** Go 1.26、`github.com/coder/websocket`、标准库 `crypto/tls`/`crypto/x509`、现有 Herdr NDJSON 客户端、`go test` 与 race detector。

---

## 文件结构

- `internal/im/model.go`：平台中立入站消息、回复和结构化通知接口。
- `internal/relayproto/frame.go`：Relay 帧、负载和稳定错误码。
- `internal/relayproto/codec.go`：严格 JSON 编解码、大小和字段校验。
- `internal/relayproto/validate.go`：身份、目标和快照校验。
- `internal/server/catalog.go`：在线连接与完整会话目录。
- `internal/server/router.go`：企微全局命令、编号快照、选择和转发。
- `internal/server/executor.go`：按 userid 串行且有界的任务执行器。
- `internal/server/hub.go`：Relay WSS 握手、心跳、请求关联和连接生命周期。
- `internal/server/tls.go`：外部证书加载和自签名证书持久化。
- `internal/server/runtime.go`：企微、Hub 和 Router 的服务端生命周期装配。
- `internal/relayclient/client.go`：客户端 WSS、握手、重连与心跳。
- `internal/relayclient/adapter.go`：Bridge 回复/通知到 Relay 帧的适配。
- `internal/relayclient/snapshot.go`：会话快照合并和周期校准上报。
- `internal/bridge/service.go`：改用平台中立消息，并提供稳定目标选择入口。
- `internal/bridge/notifier.go`：发送携带目标元数据的通知。
- `internal/session/registry.go`：导出当前稳定排序目标和按稳定身份选择。
- `internal/config/client.go`、`server.go`、`common.go`：客户端、服务端严格配置。
- `internal/app/client.go`、`interactive.go`、`assembly.go`：客户端模式装配。
- `internal/serverapp/app.go`：服务端配置、进程锁和生命周期。
- `cmd/herdr-pal-server/main.go`：服务端 CLI。
- `cmd/herdr-pal/main.go`：移除企微直连与用户发现参数。
- `internal/testkit/relay_server.go`：本地 TLS Relay fake。
- `build.sh`：同时生成两个静态二进制。

### Task 1: 引入平台中立 IM 模型

**Files:**
- Create: `internal/im/model.go`
- Create: `internal/im/model_test.go`
- Modify: `internal/wecom/protocol.go`
- Modify: `internal/wecom/client.go`
- Modify: `internal/interactive/adapter.go`
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/notifier.go`
- Modify: corresponding `*_test.go` files

- [ ] **Step 1: 写平台中立模型与动态企微发送的失败测试**

```go
func TestNotificationTargetPreservesStableIdentity(t *testing.T) {
	target := im.NotificationTarget{PaneID: "pane-1", OccupantHash: "occ-1", Title: "构建服务端"}
	data, err := json.Marshal(target)
	if err != nil || !bytes.Contains(data, []byte(`"occupant_hash":"occ-1"`)) {
		t.Fatalf("Marshal() = %s, %v", data, err)
	}
}

func TestClientSendMarkdownToUsesRequestedUser(t *testing.T) {
	client, session := newSubscribedClient(t)
	if err := client.SendMarkdownTo(context.Background(), "user-b", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := session.LastSend().UserID; got != "user-b" {
		t.Fatalf("userid = %q", got)
	}
}
```

- [ ] **Step 2: 运行定向测试并确认编译失败**

Run: `go test ./internal/im ./internal/wecom ./internal/interactive ./internal/bridge`

Expected: FAIL，提示 `internal/im`、`SendMarkdownTo` 或新接口尚不存在。

- [ ] **Step 3: 实现模型并替换 Bridge 对企微类型的依赖**

```go
package im

import "context"

type IncomingText struct {
	RequestID string
	MessageID string
	UserID    string
	ChatType  string
	Content   string
}

type NotificationTarget struct {
	PaneID       string `json:"pane_id"`
	OccupantHash string `json:"occupant_hash"`
	Agent        string `json:"agent"`
	DisplayAgent string `json:"display_agent"`
	Title        string `json:"title"`
}

type ReplySink interface {
	RespondMarkdown(context.Context, string, string) error
	SendMarkdown(context.Context, string) error
}

type NotificationSink interface {
	SendNotification(context.Context, NotificationTarget, string) error
}
```

`wecom.Client.Events()` 改为 `<-chan im.IncomingText`，新增 `SendMarkdownTo(ctx, userID, content)`；`bridge.Service` 使用 `im.IncomingText` 和 `im.ReplySink`；`bridge.Notifier` 使用 `im.NotificationSink` 并从 `session.Target` 构造 `NotificationTarget`。交互适配器同时实现两个 sink，通知目标仅用于展示头部，不改变交互命令语义。

- [ ] **Step 4: 运行受影响模块测试**

Run: `go test ./internal/im ./internal/wecom ./internal/interactive ./internal/bridge ./internal/integration`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/im internal/wecom internal/interactive internal/bridge internal/integration
git commit -m "refactor: 解耦 Bridge 与企业微信消息模型"
```

### Task 2: 实现严格 Relay 协议纯模块

**Files:**
- Create: `internal/relayproto/frame.go`
- Create: `internal/relayproto/codec.go`
- Create: `internal/relayproto/validate.go`
- Create: `internal/relayproto/codec_test.go`
- Create: `internal/relayproto/validate_test.go`

- [ ] **Step 1: 写帧解析、身份、快照和限制的失败测试**

```go
func TestDecodeRejectsUnknownFieldAndTrailingJSON(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"protocol":1,"type":"ping","payload":{},"extra":1}`),
		[]byte(`{"protocol":1,"type":"ping","payload":{}} {}`),
	} {
		if _, err := relayproto.Decode(raw); !errors.Is(err, relayproto.ErrInvalidFrame) {
			t.Fatalf("Decode() error = %v", err)
		}
	}
}

func TestValidateSnapshotRejectsStaleOrOversizedSnapshot(t *testing.T) {
	if err := relayproto.ValidateSnapshot(relayproto.SessionSnapshot{Sequence: 0}); err == nil {
		t.Fatal("sequence 0 should fail")
	}
	snapshot := relayproto.SessionSnapshot{Sequence: 1, Sessions: make([]relayproto.Session, 257)}
	if !errors.Is(relayproto.ValidateSnapshot(snapshot), relayproto.ErrLimitExceeded) {
		t.Fatal("257 sessions should fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/relayproto`

Expected: FAIL，包或类型尚不存在。

- [ ] **Step 3: 实现公开协议类型和严格编解码**

```go
const ProtocolVersion = 1
const MaxFrameBytes = 1 << 20

type Frame struct {
	Protocol  int             `json:"protocol"`
	Type      Type            `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

type SessionRef struct {
	MachineID    string `json:"machine_id"`
	LocalIndex   int    `json:"local_index"`
	PaneID       string `json:"pane_id"`
	OccupantHash string `json:"occupant_hash"`
}

type Session struct {
	LocalIndex      int    `json:"local_index"`
	PaneID          string `json:"pane_id"`
	TerminalID      string `json:"terminal_id"`
	OccupantHash    string `json:"occupant_hash"`
	AgentSessionRef string `json:"agent_session_ref,omitempty"`
	Agent           string `json:"agent"`
	DisplayAgent    string `json:"display_agent"`
	Title           string `json:"title"`
	Workspace       string `json:"workspace"`
	Tab             string `json:"tab"`
	Status          string `json:"status"`
}
```

为设计中的 11 类帧定义负载结构、稳定错误码和 `Encode`/`Decode`/`DecodePayload`。所有 decoder 使用 `DisallowUnknownFields` 并检查第二次 Decode 必须为 `io.EOF`；所有字符串按 UTF-8 字节限制校验，`machine_id` 使用固定正则，快照最多 256 项且一次性整体验证。

- [ ] **Step 4: 运行协议测试和 race 测试**

Run: `go test -race ./internal/relayproto`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/relayproto
git commit -m "feat: 添加严格 Relay 消息协议"
```

### Task 3: 拆分客户端与服务端配置

**Files:**
- Create: `internal/config/common.go`
- Create: `internal/config/client.go`
- Create: `internal/config/server.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.example.json`
- Create: `server-config.example.json`

- [ ] **Step 1: 写严格配置、WSS 和 skip_verify 缺省行为测试**

```go
func TestLoadClientDefaultsSkipVerifyAndRejectsWS(t *testing.T) {
	config := writeConfig(t, `{"relay":{"url":"wss://relay:9443","userid":"u","machine_id":"m"},"herdr":{},"log":{"level":"info"}}`)
	got, err := LoadClient(config)
	if err != nil || !got.Relay.SkipVerify {
		t.Fatalf("LoadClient() = %#v, %v", got, err)
	}
	config = writeConfig(t, `{"relay":{"url":"ws://relay:9443","userid":"u","machine_id":"m"},"herdr":{}}`)
	if _, err := LoadClient(config); err == nil {
		t.Fatal("ws:// should fail")
	}
}

func TestLoadServerRequiresListenAndCertificatePair(t *testing.T) {
	config := writeConfig(t, `{"wecom":{"bot_id":"bot"},"server":{"listen":"127.0.0.1:9443","cert_file":"cert.pem"}}`)
	if _, err := LoadServer(config, func(string) string { return "secret" }); err == nil {
		t.Fatal("unpaired certificate should fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/config`

Expected: FAIL，新的加载函数和类型尚不存在。

- [ ] **Step 3: 实现严格配置类型**

```go
type ClientConfig struct {
	Relay RelayConfig `json:"relay"`
	Herdr HerdrConfig `json:"herdr"`
	Log   LogConfig   `json:"log"`
}

type RelayConfig struct {
	URL        string `json:"url"`
	UserID     string `json:"userid"`
	MachineID  string `json:"machine_id"`
	SkipVerify bool   `json:"-"`
	RawSkipVerify *bool `json:"skip_verify,omitempty"`
}

type ServerConfig struct {
	WeCom  ServerWeComConfig `json:"wecom"`
	Server ListenerConfig    `json:"server"`
	Log    LogConfig         `json:"log"`
}
```

`LoadClient` 只接受 relay/herdr/log，拒绝旧 `wecom`；`LoadServer` 从 `HERDR_PAL_WECOM_SECRET` 注入 Secret，要求 listen、bot_id、secret，并补齐默认 state dir；`LoadInteractive` 只解析 herdr/log。保留统一严格 JSON helper，删除 `Load` 与 `LoadDiscovery`。

- [ ] **Step 4: 运行配置测试**

Run: `go test ./internal/config`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/config config.example.json server-config.example.json
git commit -m "feat: 添加 Relay 客户端与服务端配置"
```

### Task 4: 实现内存 SessionCatalog 与用户串行执行器

**Files:**
- Create: `internal/server/catalog.go`
- Create: `internal/server/catalog_test.go`
- Create: `internal/server/executor.go`
- Create: `internal/server/executor_test.go`

- [ ] **Step 1: 写唯一连接、完整替换和断线失效测试**

```go
func TestCatalogRejectsDuplicateCompositeKey(t *testing.T) {
	catalog := server.NewSessionCatalog()
	first, err := catalog.Attach("conn-1", server.ClientKey{UserID: "u", MachineID: "m"})
	if err != nil || !first {
		t.Fatalf("Attach(first) = %v, %v", first, err)
	}
	if _, err := catalog.Attach("conn-2", server.ClientKey{UserID: "u", MachineID: "m"}); !errors.Is(err, server.ErrDuplicateClient) {
		t.Fatalf("Attach(second) error = %v", err)
	}
	if _, err := catalog.Attach("conn-3", server.ClientKey{UserID: "other", MachineID: "m"}); err != nil {
		t.Fatalf("different user should reuse machine id: %v", err)
	}
}

func TestDetachInvalidatesNumberingAndSelection(t *testing.T) {
	catalog := populatedCatalog(t)
	catalog.CreateNumberedSnapshot("u")
	catalog.SetSelection("u", relayproto.SessionRef{MachineID: "m", PaneID: "p", OccupantHash: "o"})
	catalog.Detach("conn-1")
	if _, err := catalog.Selected("u"); !errors.Is(err, server.ErrNoSelection) {
		t.Fatalf("Selected() error = %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server -run 'TestCatalog|TestUserExecutor'`

Expected: FAIL，目录包尚不存在。

- [ ] **Step 3: 实现目录与每用户有界执行队列**

```go
type ClientKey struct {
	UserID    string
	MachineID string
}

type SessionCatalog struct {
	mu          sync.RWMutex
	connections map[string]ClientKey
	byKey       map[ClientKey]string
	machines    map[ClientKey]machineState
	routing     map[string]routingState
}

type UserExecutor struct {
	mu       sync.Mutex
	capacity int
	users    map[string]*userQueue
}
```

`ApplySnapshot(connectionID, sequence, sessions)` 必须确认 connection ID 仍是当前连接并只接受递增 sequence；`Detach` 原子删除机器和该用户全部引用。`CreateNumberedSnapshot` 按 machine/local index/稳定字段排序。`UserExecutor.Submit` 对同一用户串行、不同用户并行，单用户最多 64 条，空闲队列自动回收。

- [ ] **Step 4: 运行 race 测试**

Run: `go test -race ./internal/server -run 'TestCatalog|TestUserExecutor'`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/catalog.go internal/server/catalog_test.go internal/server/executor.go internal/server/executor_test.go
git commit -m "feat: 添加服务端会话目录与用户执行器"
```

### Task 5: 实现 ConversationRouter 全局命令与转发

**Files:**
- Create: `internal/server/router.go`
- Create: `internal/server/router_test.go`
- Modify: `internal/command/parser.go`
- Modify: `internal/command/parser_test.go`

- [ ] **Step 1: 写 `/userid`、聚合列表、稳定选择和转发失败测试**

```go
func TestRouterListsMachinesAndPanelTitles(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	router.Handle(context.Background(), im.IncomingText{RequestID: "r", MessageID: "m", UserID: "u", ChatType: "single", Content: "/ls"})
	got := gateway.LastReply()
	for _, want := range []string{"[home-mac/1]", "Codex", "实现 Relay", "workspace", "working"} {
		if !strings.Contains(got, want) { t.Fatalf("reply %q lacks %q", got, want) }
	}
	if relay.RequestCount() != 0 { t.Fatal("/ls must stay on server") }
}

func TestRouterSelectsThenForwardsUsingStableTarget(t *testing.T) {
	router, gateway, relay := newRouterHarness(t)
	router.Handle(ctx, message("u", "/ls"))
	router.Handle(ctx, message("u", "/1"))
	router.Handle(ctx, message("u", "继续实现"))
	if got := relay.LastExecute().Target; got.PaneID != "pane-1" || got.OccupantHash != "occ-1" {
		t.Fatalf("target = %#v", got)
	}
	_ = gateway
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server ./internal/command -run 'TestRouter|TestParseUserID'`

Expected: FAIL，Router 尚不存在。

- [ ] **Step 3: 实现服务端路由接口**

```go
type WeComGateway interface {
	RespondMarkdown(context.Context, string, string) error
	SendMarkdownTo(context.Context, string, string) error
}

type RelayRequester interface {
	Select(context.Context, string, relayproto.SessionRef) error
	Execute(context.Context, string, relayproto.SessionRef, im.IncomingText) error
}

type ConversationRouter struct {
	catalog  *SessionCatalog
	executor *UserExecutor
	gateway  WeComGateway
	relay    RelayRequester
	deduper  *policy.Deduper
	logger   *slog.Logger
}
```

Router 在排队前拒绝群聊和空 userid；直接处理 `/userid`、`/ls`、`/N`、`/sel N`、`/help`，其余输入只转发到已选择目标。选择必须依次执行目录复核、Relay `select_request`、成功后保存选择。执行超时只回复“操作可能已经提交，请先检查目标会话”，不重发。

- [ ] **Step 4: 运行 Router race 测试**

Run: `go test -race ./internal/server ./internal/command`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/router.go internal/server/router_test.go internal/command
git commit -m "feat: 添加跨机器会话路由"
```

### Task 6: 实现 TLS 证书管理

**Files:**
- Create: `internal/server/tls.go`
- Create: `internal/server/tls_test.go`

- [ ] **Step 1: 写自签名持久化、权限和外部证书测试**

```go
func TestEnsureTLSGeneratesPersistentCertificateAndPrivateKey0600(t *testing.T) {
	dir := t.TempDir()
	first, err := server.EnsureTLS(server.TLSConfig{StateDir: dir})
	if err != nil { t.Fatal(err) }
	info, err := os.Stat(filepath.Join(dir, "relay-key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", info.Mode().Perm(), err)
	}
	second, err := server.EnsureTLS(server.TLSConfig{StateDir: dir})
	if err != nil || !bytes.Equal(first.Certificates[0].Certificate[0], second.Certificates[0].Certificate[0]) {
		t.Fatal("certificate should be reused")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server -run TestEnsureTLS`

Expected: FAIL，`EnsureTLS` 尚不存在。

- [ ] **Step 3: 实现证书加载与生成**

```go
type TLSConfig struct {
	CertFile string
	KeyFile  string
	StateDir string
}

func EnsureTLS(config TLSConfig) (*tls.Config, error)
```

使用 ECDSA P-256、随机 128 位 serial、`localhost`/回环 IP/主机名 SAN、一年有效期；证书和私钥先写同目录临时文件，再 chmod 私钥为 `0600` 并原子 rename。外部证书只读加载，不修改权限；证书对配置必须同时提供。

- [ ] **Step 4: 运行测试**

Run: `go test -race ./internal/server -run TestEnsureTLS`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server/tls.go internal/server/tls_test.go
git commit -m "feat: 添加 Relay TLS 证书管理"
```

### Task 7: 实现 WSS ClientHub 与请求关联

**Files:**
- Create: `internal/server/hub.go`
- Create: `internal/server/connection.go`
- Create: `internal/server/hub_test.go`
- Create: `internal/testkit/relay_server.go`
- Create: `internal/testkit/relay_server_test.go`

- [ ] **Step 1: 写握手、重复连接、首快照、心跳与请求响应测试**

```go
func TestHubRejectsNewDuplicateConnectionWithoutReplacingOld(t *testing.T) {
	hub, endpoint := startHub(t)
	first := dialRelay(t, endpoint, hello("u", "m"))
	second := dialRelay(t, endpoint, hello("u", "m"))
	if got := second.ReadProtocolError(t).Code; got != relayproto.CodeDuplicateClient {
		t.Fatalf("code = %q", got)
	}
	first.SendSnapshot(t, snapshot(1))
	if !hub.Catalog().HasMachine("u", "m") { t.Fatal("old connection was displaced") }
}

func TestHubRemovesMachineImmediatelyWhenSocketCloses(t *testing.T) {
	hub, endpoint := startHub(t)
	client := dialReadyRelay(t, endpoint, "u", "m")
	client.Close()
	eventually(t, func() bool { return !hub.Catalog().HasMachine("u", "m") })
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/server ./internal/testkit -run 'TestHub|TestRelayServer'`

Expected: FAIL，Hub 尚不存在。

- [ ] **Step 3: 实现连接状态机和有界写队列**

```go
type HubConfig struct {
	HelloTimeout       time.Duration
	FirstSnapshotLimit time.Duration
	HeartbeatInterval  time.Duration
	HeartbeatTimeout   time.Duration
	SnapshotInterval   time.Duration
	SendQueueCapacity  int
	MaxInflight        int
}

type ClientHub struct {
	catalog *SessionCatalog
	config  HubConfig
	logger  *slog.Logger
	mu      sync.RWMutex
	clients map[ClientKey]*clientConnection
	pending map[string]*pendingRequest
}
```

HTTP handler 只接受 TLS 上的 WebSocket 文本帧；10 秒内完成 hello 和首快照。每连接一个 reader、一个 writer、有界 128 帧发送队列；应用层 ping 每 10 秒，30 秒未 pong 关闭。`Select`/`Execute` 使用随机 request ID 和最多 128 个在途关联，超时移除关联且从不重发。连接退出先从 Hub 当前记录移除，再调用 Catalog.Detach。

- [ ] **Step 4: 运行 Hub race 测试**

Run: `go test -race ./internal/server ./internal/testkit -run 'TestHub|TestRelayServer'`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/server internal/testkit/relay_server.go internal/testkit/relay_server_test.go
git commit -m "feat: 添加 Relay WSS 连接中心"
```

### Task 8: 实现 Relay Client、Bridge 远程选择和快照上报

**Files:**
- Create: `internal/relayclient/client.go`
- Create: `internal/relayclient/adapter.go`
- Create: `internal/relayclient/snapshot.go`
- Create: `internal/relayclient/client_test.go`
- Create: `internal/relayclient/adapter_test.go`
- Create: `internal/relayclient/snapshot_test.go`
- Modify: `internal/session/registry.go`
- Modify: `internal/session/registry_test.go`
- Modify: `internal/bridge/service.go`
- Modify: `internal/bridge/service_test.go`

- [ ] **Step 1: 写稳定目标选择、WSS 默认跳过校验和快照时序测试**

```go
func TestRegistrySelectTargetRequiresPaneAndOccupant(t *testing.T) {
	registry := populatedRegistry(t)
	selected, err := registry.SelectTarget("pane-1", "occ-1")
	if err != nil || selected.PaneID != "pane-1" { t.Fatalf("SelectTarget() = %#v, %v", selected, err) }
	if _, err := registry.SelectTarget("pane-1", "old"); !errors.Is(err, session.ErrListSnapshotExpired) {
		t.Fatalf("stale occupant error = %v", err)
	}
}

func TestSnapshotPublisherDebouncesChangesAndCalibrates(t *testing.T) {
	clock, sink, publisher := newPublisherHarness(t)
	publisher.Changed(targets("one"))
	publisher.Changed(targets("two"))
	clock.Advance(time.Second)
	if got := sink.Snapshots(); len(got) != 1 || got[0].Sessions[0].Title != "two" { t.Fatalf("snapshots = %#v", got) }
	clock.Advance(30 * time.Second)
	if got := sink.Snapshots(); len(got) != 2 || got[1].Sequence != 2 { t.Fatalf("snapshots = %#v", got) }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/relayclient ./internal/session ./internal/bridge`

Expected: FAIL，新包和选择入口尚不存在。

- [ ] **Step 3: 实现客户端连接和本地执行入口**

```go
type Config struct {
	URL        string
	UserID     string
	MachineID  string
	SkipVerify bool
	Version    string
	Logger     *slog.Logger
}

type Executor interface {
	SelectTarget(context.Context, relayproto.SessionRef) error
	HandleMessage(context.Context, im.IncomingText)
	CurrentTargets() []session.Target
}

type Client struct {
	config    Config
	executor  Executor
	adapter   *Adapter
	publisher *SnapshotPublisher
}
```

`session.Registry.CurrentTargets` 返回稳定排序副本但不修改本地 `/ls` 编号；`SelectTarget` 按 pane+occupant 选择并重置 panel。Relay client 仅接受 `wss://`，SkipVerify 为 true 时设置 `tls.Config.InsecureSkipVerify`；握手成功后 sequence 从 1 上报，变更 1 秒合并、30 秒全量校准。`execute_request` 转成 `im.IncomingText`，Adapter 将首段/后续回复和结构化通知写回当前连接；断线时立即失败，不缓存。

- [ ] **Step 4: 运行客户端 race 测试**

Run: `go test -race ./internal/relayclient ./internal/session ./internal/bridge`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/relayclient internal/session internal/bridge
git commit -m "feat: 添加 Relay 客户端与本地目标执行"
```

### Task 9: 装配 herdr-pal-server 和新的客户端网络模式

**Files:**
- Create: `internal/server/runtime.go`
- Create: `internal/server/runtime_test.go`
- Create: `internal/serverapp/app.go`
- Create: `internal/serverapp/app_test.go`
- Create: `cmd/herdr-pal-server/main.go`
- Create: `cmd/herdr-pal-server/main_test.go`
- Split/Modify: `internal/app/app.go`
- Modify: `internal/app/app_test.go`
- Modify: `cmd/herdr-pal/main.go`
- Modify: `cmd/herdr-pal/main_test.go`
- Modify: `build.sh`

- [ ] **Step 1: 写双入口、模式迁移和构建产物失败测试**

```go
func TestClientCLIRejectsRemovedDiscoverUserFlag(t *testing.T) {
	code := run(context.Background(), []string{"-discover-user", "-config", "x.json"}, strings.NewReader(""), io.Discard, io.Discard, fakeRun)
	if code != 2 { t.Fatalf("code = %d", code) }
}

func TestServerCLIRequiresConfigAndSupportsVersion(t *testing.T) {
	if code := runServer(ctx, []string{"--version"}, stdout, stderr, fakeRun); code != 0 { t.Fatalf("version code = %d", code) }
	if code := runServer(ctx, nil, stdout, stderr, fakeRun); code != 2 { t.Fatalf("missing config code = %d", code) }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./cmd/... ./internal/app ./internal/serverapp`

Expected: FAIL，服务端入口与新装配尚不存在。

- [ ] **Step 3: 实现两个独立生命周期**

```go
// client
func Run(ctx context.Context, options Options) error

// server
func Run(ctx context.Context, options Options) error
```

客户端普通模式：加载 ClientConfig、解析规范 Herdr socket、按 socket 摘要加锁、装配 Service/Notifier/Supervisor/Relay Client 并共同运行；`-i` 沿用本地交互装配。服务端：加载 ServerConfig、按 bot ID 摘要加锁、启动 WeCom Client、TLS HTTP Server、Hub 与 Router；任一关键循环异常时取消全部组件并优雅退出。删除直连企微与 discovery 装配代码。

`build.sh` 使用 `CGO_ENABLED=0 go build` 依次生成 `dist/herdr-pal` 和 `dist/herdr-pal-server`。

- [ ] **Step 4: 运行入口测试和本地构建**

Run: `go test -race ./cmd/... ./internal/app ./internal/serverapp && ./build.sh`

Expected: PASS，且两个 dist 文件存在并可执行。

- [ ] **Step 5: 提交**

```bash
git add cmd internal/app internal/serverapp internal/server/runtime.go internal/server/runtime_test.go build.sh
git commit -m "feat: 装配 Relay 服务端与客户端网络模式"
```

### Task 10: 完成跨用户端到端行为与通知展示

**Files:**
- Create: `internal/integration/relay_test.go`
- Modify: `internal/testkit/wecom_server.go`
- Modify: `internal/testkit/wecom_server_test.go`
- Modify: `internal/bridge/notifier_test.go`
- Modify: `internal/server/router_test.go`

- [ ] **Step 1: 写多用户、多机器、同 machine ID 隔离和通知测试**

```go
func TestRelayEndToEndRoutesTwoUsersWithSameMachineID(t *testing.T) {
	env := startRelayEnvironment(t)
	userA := env.AddClient("user-a", "home-mac", fakeHerdrWithTitle("A 的任务"))
	userB := env.AddClient("user-b", "home-mac", fakeHerdrWithTitle("B 的任务"))
	env.WeCom.Send("user-a", "/ls")
	env.WeCom.Send("user-a", "/1")
	env.WeCom.Send("user-a", "只发给 A")
	if got := userA.Herdr.LastPrompt(); got != "只发给 A" { t.Fatalf("A prompt = %q", got) }
	if got := userB.Herdr.LastPrompt(); got != "" { t.Fatalf("B prompt = %q", got) }
}

func TestRelayNotificationIncludesMachineIndexAndPanelTitle(t *testing.T) {
	env := startRelayEnvironment(t)
	client := env.AddClient("user-a", "office-pc", fakeHerdrWithTitle("修复登录"))
	client.Herdr.EmitStatus("pane-1", "blocked")
	got := env.WeCom.LastMessageTo("user-a")
	for _, want := range []string{"[office-pc/1]", "修复登录", "Agent 已阻塞"} {
		if !strings.Contains(got, want) { t.Fatalf("notification %q lacks %q", got, want) }
	}
}
```

- [ ] **Step 2: 运行集成测试确认失败**

Run: `go test ./internal/integration -run Relay`

Expected: FAIL，尚有路由或通知展示缺口。

- [ ] **Step 3: 补齐结构化通知和动态企微目标发送**

服务端收到 `notification` 后以 `(userid,machine_id,pane_id,occupant_hash)` 查询最新目录，只在目标仍存在时发送：

```go
func renderNotification(machineID string, localIndex int, session relayproto.Session, content string) string {
	name := session.DisplayAgent
	if name == "" { name = session.Agent }
	header := fmt.Sprintf("[%s/%d] %s", safeLabel(machineID), localIndex, safeLabel(name))
	if session.Title != "" { header += " — " + safeLabel(session.Title) }
	return header + "\n" + content
}
```

机器断线、occupant 替换和 server 重启时不发送旧通知、不恢复选择、不重放请求。确保 blocked/done/必要 idle 仍只读取最近 100 行。

- [ ] **Step 4: 运行全部集成测试**

Run: `go test -race ./internal/integration ./internal/server ./internal/relayclient`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/integration internal/testkit internal/bridge/notifier_test.go internal/server
git commit -m "test: 覆盖多用户 Relay 端到端路由"
```

### Task 11: 更新用户文档并完成发布验证

**Files:**
- Modify: `README.md`
- Modify: `docs/HANDOFF_CONTEXT.md`
- Modify: `docs/BRIDGE_ARCHITECTURE.md`
- Modify: `unittest.sh` only if new package selection requires adjustment

- [ ] **Step 1: 更新 README 配置和联调流程**

README 必须包含：服务端 Secret 环境变量、自动证书目录、客户端默认跳过证书校验的安全警告、`/userid` 获取方式、多个 machine ID 示例、两个二进制启动命令、旧企微直连配置不再支持。

- [ ] **Step 2: 更新架构与交接文档**

明确 server 不连接 Herdr、不持久化路由状态、不认证 userid；客户端不暴露 Herdr socket，断线不缓存通知或命令。列出 Relay 协议帧与资源限制。

- [ ] **Step 3: 扫描旧模式和敏感字段**

Run: `rg -n 'discover-user|allowed_user_id|直连企业微信|ws://' README.md docs internal cmd config.example.json server-config.example.json`

Expected: 只出现迁移说明和“拒绝 ws://”的测试，不存在运行时旧模式字段。

- [ ] **Step 4: 完整验证**

Run: `git diff --check && ./build.sh && ./unittest.sh && go test -race ./...`

Expected: 全部退出码为 0；`dist/herdr-pal --version` 与 `dist/herdr-pal-server --version` 均成功；`git status --short` 只包含本任务预期文档变化。

- [ ] **Step 5: 提交文档与发布收尾**

```bash
git add README.md docs/HANDOFF_CONTEXT.md docs/BRIDGE_ARCHITECTURE.md unittest.sh
git commit -m "docs: 更新多用户 Relay 部署说明"
```

