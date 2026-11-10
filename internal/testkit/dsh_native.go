package testkit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	// dshHostDefaultVersion 是 fake 宿主报告的 dsh 版本。
	dshHostDefaultVersion = "0.2.2-rc.1"
	// dshHostDefaultVariant 是 fake 宿主默认的变体。
	dshHostDefaultVariant = "web"
	// dshHostDefaultClientID 是 ready 帧使用的默认 clientId。
	dshHostDefaultClientID = "test-client"
	// dshHostMuxPath 是 dsh 原生 Mux 的固定路径。
	dshHostMuxPath = "/api/remote.mux"
	// dshHostMaxBodyBytes 限制 fake 宿主读取的请求体，与真实 RPC 上限一致。
	dshHostMaxBodyBytes = 1 << 20
)

// dshRPCEndpoints 是 fake 宿主接受的固定 endpoint 白名单。
var dshRPCEndpoints = map[string]struct{}{
	"session/list":         {},
	"session/projections":  {},
	"session/page":         {},
	"session/prompt":       {},
	"session/create":       {},
	"session/rename":       {},
	"userQuestions/answer": {},
	"$events/result":       {},
}

// DSHCall 记录 fake dsh 宿主收到的一次 HTTP RPC 调用。
type DSHCall struct {
	// Method 是 RPC endpoint，例如 session/prompt。
	Method string
	// Args 是 payload.args 的原始 JSON；请求未提供时为 null。
	Args json.RawMessage
}

// DSHContentBlock 是原生消息内容块，用于构造只含文本、推理或工具块的助手消息。
type DSHContentBlock struct {
	// Type 是内容块类型，例如 text、reasoning、toolCall。
	Type string
	// Text 是文本内容；推理和工具块也可以携带可识别的占位文本。
	Text string
}

// TextBlock 返回一个助手文本块。
func TextBlock(text string) DSHContentBlock { return DSHContentBlock{Type: "text", Text: text} }

// ReasoningBlock 返回一个推理块；聊天正文投影必须丢弃它。
func ReasoningBlock(text string) DSHContentBlock {
	return DSHContentBlock{Type: "reasoning", Text: text}
}

// ToolCallBlock 返回一个工具调用块；聊天正文投影必须丢弃它。
func ToolCallBlock(name string) DSHContentBlock {
	return DSHContentBlock{Type: "toolCall", Text: name}
}

// DSHEvent 是 fake 会话中的一条原始事件记录。
type DSHEvent struct {
	// Seq 是事件序号，用于分页与 surfaceOp。
	Seq uint64
	// Type 是事件类型，例如 user/message 或 assistant/message。
	Type string
	// Data 是原生 data 载荷；测试可直接构造 map。
	Data any
	// SurfaceOp 是表面操作；nil 表示 append。
	SurfaceOp any
}

// UserMessage 构造一条真实用户输入事件。
func UserMessage(seq uint64, text string) DSHEvent {
	return userMessageFrom(seq, "user", text)
}

// InjectedUserMessage 构造一条非用户来源的注入事件，投影时必须丢弃。
func InjectedUserMessage(seq uint64, text string) DSHEvent {
	return userMessageFrom(seq, "system", text)
}

func userMessageFrom(seq uint64, sourceKind, text string) DSHEvent {
	return DSHEvent{
		Seq: seq, Type: "user/message",
		Data: map[string]any{
			"source":  map[string]any{"kind": sourceKind},
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

// AssistantMessage 构造一条助手消息事件；只有 text 块会进入聊天正文。
func AssistantMessage(seq uint64, blocks ...DSHContentBlock) DSHEvent {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		value := map[string]any{"type": block.Type}
		if block.Text != "" {
			value["text"] = block.Text
		}
		content = append(content, value)
	}
	return DSHEvent{
		Seq: seq, Type: "assistant/message",
		Data: map[string]any{"message": map[string]any{"content": content}},
	}
}

// DSHOption 是原生问题选项；dsh 只提供 label。
type DSHOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// DSHQuestion 是原生提问中的一道题。
type DSHQuestion struct {
	ID          string      `json:"id"`
	Question    string      `json:"question"`
	Detail      string      `json:"detail,omitempty"`
	Header      string      `json:"header,omitempty"`
	Options     []DSHOption `json:"options,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// DSHContinuedQuestion 是投影中的持续问题条目。
type DSHContinuedQuestion struct {
	CallID    string        `json:"callId"`
	State     string        `json:"state"`
	Questions []DSHQuestion `json:"questions"`
}

// DSHDecision 是一条通过 Mux waterfall 推送的原生交互。
type DSHDecision struct {
	EventID string
	AgentID string
	Event   string
	Request any
}

// ApprovalDecision 构造工具调用审批 waterfall。
func ApprovalDecision(eventID, agentID, toolName, reason string) DSHDecision {
	return DSHDecision{
		EventID: eventID, AgentID: agentID, Event: "approval/request",
		Request: map[string]any{"toolName": toolName, "reason": reason},
	}
}

// UserQuestionsDecision 构造结构化问题 waterfall；未设置 wait 时按 blocking 处理。
func UserQuestionsDecision(eventID, agentID string, questions ...DSHQuestion) DSHDecision {
	return DSHDecision{
		EventID: eventID, AgentID: agentID, Event: "user-questions/request",
		Request: map[string]any{"questions": questions},
	}
}

// DSHSession 是 fake 宿主中的初始会话定义。
type DSHSession struct {
	// ID 是原生会话 ID；为空时 AddSession 会拒绝。
	ID string
	// Title 是投影标题；为空时回退为 ID。
	Title string
	// Cwd 是工作目录。
	Cwd string
	// Running 表示 Agent 是否正在执行。
	Running bool
	// UpdatedAt 是活动时间戳；为零时使用当前时间。
	UpdatedAt int64
	// Unavailable 为 true 时 session/list 返回 agentAvailable=false。
	Unavailable bool
	// Origin 可设为 subagent，用于验证过滤。
	Origin string
	// FormatStatus 可设为 migration-required，用于验证过滤。
	FormatStatus string
	// Transcript 是初始事件记录。
	Transcript []DSHEvent
	// Projections 是合并到 projections.values 的额外字段。
	Projections map[string]any
	// ActiveQuestions 是 projections.values.userQuestions.active 的内容。
	ActiveQuestions []DSHContinuedQuestion
}

// DSHHostOptions 配置 fake dsh 宿主的初始状态。
type DSHHostOptions struct {
	// Version 是宿主版本，默认 0.2.2-rc.1；显式配置场景下只作记录。
	Version string
	// Variant 是 web 或 desktop，默认 web。
	Variant string
	// Running 为 true 时把初始会话全部标记为运行中。
	Running bool
	// ClientID 是 ready 帧使用的 clientId，默认 test-client。
	ClientID string
	// Sessions 是初始会话。
	Sessions []DSHSession
	// Decisions 是在每个 Mux 连接 ready 后重放的 waterfall。
	Decisions []DSHDecision
}

// DSHHost 是只实现 dsh 原生 Remote API 必要行为的 fake 宿主。
//
// 它校验与 dsh 相同的短期 Cookie，记录 RPC 调用，并通过 WebSocket 推送
// ready/waterfall/cancel。所有状态变更都由测试显式触发，不调用模型或外部网络。
type DSHHost struct {
	server    *httptest.Server
	home      string
	version   string
	variant   string
	authority string
	secret    []byte
	clientID  string

	mu          sync.Mutex
	closed      bool
	sessions    map[string]*dshFakeSession
	order       []string
	calls       []DSHCall
	responses   map[string]json.RawMessage
	errors      map[string]DSHHostError
	decisions   []DSHDecision
	connections map[*dshMuxConnection]struct{}
	readyCount  int
	createSeq   int
	changed     chan struct{}
	lastCreated string
	readyClient string
}

// DSHHostError 是 fake 宿主按 endpoint 返回的 RPC 业务错误。
type DSHHostError struct {
	Code    string
	Message string
}

type dshFakeSession struct {
	id              string
	title           string
	cwd             string
	running         bool
	updatedAt       int64
	unavailable     bool
	origin          string
	formatStatus    string
	events          []DSHEvent
	projections     map[string]any
	activeQuestions []DSHContinuedQuestion
}

type dshMuxConnection struct {
	socket  *websocket.Conn
	writeMu sync.Mutex
	ready   bool
}

// NewDSHHost 启动一个临时 Home、真实认证文件与 HTTP/WebSocket fake 宿主。
func NewDSHHost(t testing.TB, options DSHHostOptions) *DSHHost {
	t.Helper()
	if options.Version == "" {
		options.Version = dshHostDefaultVersion
	}
	if options.Variant == "" {
		options.Variant = dshHostDefaultVariant
	}
	if options.ClientID == "" {
		options.ClientID = dshHostDefaultClientID
	}

	home := t.TempDir()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("生成 fake dsh 认证密钥：%v", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(secret)
	credentials := fmt.Sprintf(`version: 1
records:
  client-connection/browser-session:
    kind: grant
    payload:
      version: 1
      secret: %s
`, encoded)
	credentialsPath := filepath.Join(home, ".credentials.yaml")
	if err := os.WriteFile(credentialsPath, []byte(credentials), 0o600); err != nil {
		t.Fatalf("写入 fake dsh 认证文件：%v", err)
	}
	if err := os.Chmod(credentialsPath, 0o600); err != nil {
		t.Fatalf("设置 fake dsh 认证文件权限：%v", err)
	}

	host := &DSHHost{
		home: home, version: options.Version, variant: options.Variant, secret: secret,
		clientID: options.ClientID, sessions: make(map[string]*dshFakeSession),
		responses: make(map[string]json.RawMessage), errors: make(map[string]DSHHostError),
		decisions:   append([]DSHDecision(nil), options.Decisions...),
		connections: make(map[*dshMuxConnection]struct{}), changed: make(chan struct{}, 1),
	}
	host.server = httptest.NewServer(http.HandlerFunc(host.serveHTTP))
	host.authority = strings.TrimPrefix(host.server.URL, "http://")

	for _, session := range options.Sessions {
		if options.Running {
			session.Running = true
		}
		host.AddSession(session)
	}
	t.Cleanup(host.Close)
	return host
}

// URL 返回可写入 dsh.url 的回环地址。
func (h *DSHHost) URL() string { return h.server.URL }

// Home 返回包含 .credentials.yaml 的临时 DSH_HOME。
func (h *DSHHost) Home() string { return h.home }

// Version 返回宿主版本。
func (h *DSHHost) Version() string { return h.version }

// Variant 返回 web 或 desktop。
func (h *DSHHost) Variant() string { return h.variant }

// ClientID 返回 ready 帧使用的 clientId。
func (h *DSHHost) ClientID() string { return h.clientID }

// ReadyClientID 返回最近一次 ready 使用的 clientId；没有连接时为空。
func (h *DSHHost) ReadyClientID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readyClient
}

// AddSession 增加一个可控制会话；ID 为空时直接 panic，表示测试夹具错误。
func (h *DSHHost) AddSession(session DSHSession) {
	if strings.TrimSpace(session.ID) == "" {
		panic("fake dsh 会话 ID 不能为空")
	}
	state := &dshFakeSession{
		id: session.ID, title: session.Title, cwd: session.Cwd,
		running: session.Running, updatedAt: session.UpdatedAt,
		unavailable: session.Unavailable, origin: session.Origin, formatStatus: session.FormatStatus,
		events:          append([]DSHEvent(nil), session.Transcript...),
		projections:     cloneMap(session.Projections),
		activeQuestions: append([]DSHContinuedQuestion(nil), session.ActiveQuestions...),
	}
	if state.title == "" {
		state.title = state.id
	}
	if state.updatedAt == 0 {
		state.updatedAt = time.Now().UnixMilli()
	}
	h.mu.Lock()
	if _, exists := h.sessions[state.id]; !exists {
		h.order = append(h.order, state.id)
	}
	h.sessions[state.id] = state
	h.mu.Unlock()
	h.signal()
}

// RemoveSession 从后续 session/list 中移除会话，模拟宿主内会话消失。
func (h *DSHHost) RemoveSession(id string) {
	h.mu.Lock()
	if _, exists := h.sessions[id]; exists {
		delete(h.sessions, id)
		for index, current := range h.order {
			if current == id {
				h.order = append(h.order[:index], h.order[index+1:]...)
				break
			}
		}
	}
	h.mu.Unlock()
	h.signal()
}

// SetRunning 更新会话的运行状态，并把 updatedAt 推进到当前时间。
func (h *DSHHost) SetRunning(id string, running bool) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.running = running
		session.updatedAt = time.Now().UnixMilli()
	}
	h.mu.Unlock()
	h.signal()
}

// SetUpdatedAt 更新会话活动时间戳。
func (h *DSHHost) SetUpdatedAt(id string, updatedAt int64) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.updatedAt = updatedAt
	}
	h.mu.Unlock()
	h.signal()
}

// SetTitle 更新会话投影标题。
func (h *DSHHost) SetTitle(id, title string) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.title = title
	}
	h.mu.Unlock()
	h.signal()
}

// SetTranscript 替换会话的原始事件记录。
func (h *DSHHost) SetTranscript(id string, events []DSHEvent) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.events = append([]DSHEvent(nil), events...)
	}
	h.mu.Unlock()
	h.signal()
}

// AppendTranscript 向会话追加原始事件记录。
func (h *DSHHost) AppendTranscript(id string, events ...DSHEvent) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.events = append(session.events, events...)
	}
	h.mu.Unlock()
	h.signal()
}

// SetProjections 替换合并到 projections.values 的额外字段。
func (h *DSHHost) SetProjections(id string, values map[string]any) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.projections = cloneMap(values)
	}
	h.mu.Unlock()
	h.signal()
}

// SetActiveQuestions 替换 projections.values.userQuestions.active。
func (h *DSHHost) SetActiveQuestions(id string, questions []DSHContinuedQuestion) {
	h.mu.Lock()
	if session := h.sessions[id]; session != nil {
		session.activeQuestions = append([]DSHContinuedQuestion(nil), questions...)
	}
	h.mu.Unlock()
	h.signal()
}

// Sessions 返回当前会话定义的副本。
func (h *DSHHost) Sessions() []DSHSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]DSHSession, 0, len(h.order))
	for _, id := range h.order {
		session := h.sessions[id]
		if session == nil {
			continue
		}
		result = append(result, DSHSession{
			ID: session.id, Title: session.title, Cwd: session.cwd,
			Running: session.running, UpdatedAt: session.updatedAt,
			Unavailable: session.unavailable, Origin: session.origin, FormatStatus: session.formatStatus,
			Transcript:      append([]DSHEvent(nil), session.events...),
			Projections:     cloneMap(session.projections),
			ActiveQuestions: append([]DSHContinuedQuestion(nil), session.activeQuestions...),
		})
	}
	return result
}

// LastCreatedSession 返回最近一次 session/create 创建的会话；没有时返回 false。
func (h *DSHHost) LastCreatedSession() (DSHSession, bool) {
	h.mu.Lock()
	id := h.lastCreated
	h.mu.Unlock()
	for _, session := range h.Sessions() {
		if session.ID == id {
			return session, true
		}
	}
	return DSHSession{}, false
}

// SetResponse 覆盖指定 endpoint 的成功返回值。
func (h *DSHHost) SetResponse(method string, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("fake dsh 响应无法编码：%v", err))
	}
	h.mu.Lock()
	h.responses[method] = encoded
	h.mu.Unlock()
}

// SetError 让指定 endpoint 返回 ok=false 的业务错误；空 endpoint 表示清除。
func (h *DSHHost) SetError(method string, failure DSHHostError) {
	h.mu.Lock()
	if method == "" {
		h.errors = make(map[string]DSHHostError)
	} else if failure.Code == "" && failure.Message == "" {
		delete(h.errors, method)
	} else {
		h.errors[method] = failure
	}
	h.mu.Unlock()
}

// Calls 返回全部已记录的 RPC 调用副本。
func (h *DSHHost) Calls() []DSHCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]DSHCall(nil), h.calls...)
}

// CallsFor 返回指定 endpoint 的调用副本。
func (h *DSHHost) CallsFor(method string) []DSHCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]DSHCall, 0, len(h.calls))
	for _, call := range h.calls {
		if call.Method == method {
			result = append(result, call)
		}
	}
	return result
}

// WaitCallCount 等待指定 endpoint 至少收到 count 次调用。
func (h *DSHHost) WaitCallCount(t testing.TB, method string, count int) []DSHCall {
	t.Helper()
	var calls []DSHCall
	h.wait(t, fmt.Sprintf("dsh RPC %s 调用达到 %d 次", method, count), func() bool {
		calls = h.CallsFor(method)
		return len(calls) >= count
	})
	return calls
}

// WaitReady 等待至少一条 Mux 连接完成 ready 握手。
func (h *DSHHost) WaitReady(t testing.TB) {
	t.Helper()
	h.wait(t, "dsh Mux 完成 ready 握手", func() bool { return h.ReadyCount() > 0 })
}

// ReadyCount 返回累计完成的 ready 握手次数。
func (h *DSHHost) ReadyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.readyCount
}

// InjectDecision 向所有已就绪的 Mux 连接推送一条 waterfall，并记录用于重连重放。
func (h *DSHHost) InjectDecision(t testing.TB, decision DSHDecision) {
	t.Helper()
	if decision.EventID == "" || decision.AgentID == "" || decision.Event == "" {
		t.Fatal("fake dsh waterfall 缺少 eventId、agentId 或 event")
	}
	h.WaitReady(t)
	frame, err := marshalMuxItem(map[string]any{
		"type": "waterfall", "eventId": decision.EventID, "agentId": decision.AgentID,
		"event": decision.Event, "request": decision.Request,
	})
	if err != nil {
		t.Fatalf("编码 fake dsh waterfall：%v", err)
	}
	h.mu.Lock()
	h.decisions = append(h.decisions, decision)
	connections := h.readyConnectionsLocked()
	h.mu.Unlock()
	if len(connections) == 0 {
		t.Fatal("fake dsh 没有就绪的 Mux 连接")
	}
	for _, connection := range connections {
		if err := connection.write(frame); err != nil {
			t.Fatalf("推送 fake dsh waterfall：%v", err)
		}
	}
}

// InjectCancel 向所有已就绪的 Mux 连接推送 cancel，并移除重放列表中的同一事件。
func (h *DSHHost) InjectCancel(t testing.TB, eventID string) {
	t.Helper()
	h.WaitReady(t)
	frame, err := marshalMuxItem(map[string]any{"type": "cancel", "eventId": eventID})
	if err != nil {
		t.Fatalf("编码 fake dsh cancel：%v", err)
	}
	h.mu.Lock()
	pending := h.decisions[:0]
	for _, decision := range h.decisions {
		if decision.EventID != eventID {
			pending = append(pending, decision)
		}
	}
	h.decisions = pending
	connections := h.readyConnectionsLocked()
	h.mu.Unlock()
	for _, connection := range connections {
		if err := connection.write(frame); err != nil {
			t.Fatalf("推送 fake dsh cancel：%v", err)
		}
	}
}

// Close 关闭全部 Mux 连接并停止 HTTP server；可重复调用。
func (h *DSHHost) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	connections := make([]*dshMuxConnection, 0, len(h.connections))
	for connection := range h.connections {
		connections = append(connections, connection)
	}
	h.connections = nil
	h.mu.Unlock()
	for _, connection := range connections {
		connection.close()
	}
	h.server.Close()
	h.signal()
}

func (h *DSHHost) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == dshHostMuxPath {
		h.serveMux(writer, request)
		return
	}
	h.serveRPC(writer, request)
}

func (h *DSHHost) serveRPC(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.authorized(request) {
		http.Error(writer, "dsh web authentication required", http.StatusUnauthorized)
		return
	}
	endpoint := strings.TrimPrefix(request.URL.Path, "/api/")
	if _, ok := dshRPCEndpoints[endpoint]; !ok || endpoint == "" {
		http.Error(writer, "unknown endpoint", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, dshHostMaxBodyBytes+1))
	if err != nil || len(body) > dshHostMaxBodyBytes {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Type    string `json:"type"`
		RPCID   string `json:"rpcId"`
		Method  string `json:"method"`
		Payload struct {
			Args json.RawMessage `json:"args"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Type != "client-request" ||
		envelope.RPCID == "" || envelope.Method != endpoint {
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	args := envelope.Payload.Args
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	h.recordCall(endpoint, args)

	h.mu.Lock()
	configuredError, hasError := h.errors[endpoint]
	h.mu.Unlock()
	if hasError {
		h.writeRPCError(writer, envelope.RPCID, configuredError)
		return
	}
	value, err := h.rpcValue(endpoint, args)
	if err != nil {
		h.writeRPCError(writer, envelope.RPCID, DSHHostError{Code: "fake-error", Message: err.Error()})
		return
	}
	h.writeRPCSuccess(writer, envelope.RPCID, value)
}

func (h *DSHHost) rpcValue(endpoint string, args json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	configured, hasConfigured := h.responses[endpoint]
	h.mu.Unlock()
	if hasConfigured {
		return configured, nil
	}

	switch endpoint {
	case "session/list":
		return marshalValue(map[string]any{"items": h.sessionItems()})
	case "session/projections":
		var request struct {
			Request struct {
				SessionID string `json:"sessionId"`
			} `json:"request"`
		}
		if err := json.Unmarshal(args, &request); err != nil {
			return nil, errors.New("session/projections 参数无效")
		}
		return h.projectionsValue(request.Request.SessionID)
	case "session/page":
		return h.pageValue(args)
	case "session/prompt":
		return marshalValue(map[string]any{"accepted": true})
	case "session/create":
		return h.createValue(args)
	case "session/rename":
		return h.renameValue(args)
	case "userQuestions/answer":
		return json.RawMessage("true"), nil
	case "$events/result":
		return json.RawMessage("{}"), nil
	default:
		return nil, errors.New("不支持的 endpoint")
	}
}

func (h *DSHHost) sessionItems() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	items := make([]map[string]any, 0, len(h.order))
	for _, id := range h.order {
		session := h.sessions[id]
		if session == nil {
			continue
		}
		item := map[string]any{
			"sessionId":      session.id,
			"agentAvailable": !session.unavailable,
			"running":        session.running,
			"updatedAt":      session.updatedAt,
			"cwd":            session.cwd,
			"projections":    map[string]any{"values": map[string]any{"title": session.title}},
		}
		if session.origin != "" {
			item["origin"] = session.origin
		}
		if session.formatStatus != "" {
			item["formatStatus"] = session.formatStatus
		}
		items = append(items, item)
	}
	return items
}

func (h *DSHHost) projectionsValue(sessionID string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	session := h.sessions[sessionID]
	if session == nil {
		return nil, errors.New("会话不存在")
	}
	active := make([]DSHContinuedQuestion, 0, len(session.activeQuestions))
	active = append(active, session.activeQuestions...)
	values := map[string]any{
		"title":         session.title,
		"userQuestions": map[string]any{"active": active},
	}
	for key, value := range session.projections {
		values[key] = value
	}
	return marshalValue(map[string]any{
		"kind": "sequenced", "asOfSeq": session.asOfSeq(), "values": values,
	})
}

func (h *DSHHost) pageValue(args json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Request struct {
			Address struct {
				Kind      string `json:"kind"`
				SessionID string `json:"sessionId"`
			} `json:"address"`
			ThroughSeq  int64   `json:"throughSeq"`
			BeforeSeq   *uint64 `json:"beforeSeq"`
			MaxMessages int     `json:"maxMessages"`
		} `json:"request"`
	}
	if err := json.Unmarshal(args, &request); err != nil || request.Request.Address.Kind != "session" {
		return nil, errors.New("session/page 参数无效")
	}
	h.mu.Lock()
	session := h.sessions[request.Request.Address.SessionID]
	if session == nil {
		h.mu.Unlock()
		return nil, errors.New("会话不存在")
	}
	events := append([]DSHEvent(nil), session.events...)
	h.mu.Unlock()

	filtered := make([]DSHEvent, 0, len(events))
	for _, event := range events {
		if int64(event.Seq) > request.Request.ThroughSeq {
			continue
		}
		if request.Request.BeforeSeq != nil && event.Seq >= *request.Request.BeforeSeq {
			continue
		}
		filtered = append(filtered, event)
	}
	limit := request.Request.MaxMessages
	if limit <= 0 {
		limit = 100
	}
	start := len(filtered) - limit
	if start < 0 {
		start = 0
	}
	records := make([]json.RawMessage, 0, len(filtered)-start)
	for _, event := range filtered[start:] {
		record, err := marshalValue(map[string]any{"event": eventRecord(event)})
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return marshalValue(map[string]any{"records": records, "hasMore": start > 0})
}

func eventRecord(event DSHEvent) map[string]any {
	surfaceOp := event.SurfaceOp
	if surfaceOp == nil {
		surfaceOp = "append"
	}
	record := map[string]any{"seq": event.Seq, "type": event.Type, "surfaceOp": surfaceOp}
	if event.Data != nil {
		record["data"] = event.Data
	}
	return record
}

func (h *DSHHost) createValue(args json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Request struct {
			Cwd string `json:"cwd"`
		} `json:"request"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return nil, errors.New("session/create 参数无效")
	}
	h.mu.Lock()
	h.createSeq++
	id := fmt.Sprintf("native-session-%d", h.createSeq)
	h.lastCreated = id
	h.mu.Unlock()
	h.AddSession(DSHSession{ID: id, Title: id, Cwd: request.Request.Cwd})
	return marshalValue(map[string]any{"sessionId": id})
}

func (h *DSHHost) renameValue(args json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Request struct {
			SessionID string `json:"sessionId"`
			Title     string `json:"title"`
		} `json:"request"`
	}
	if err := json.Unmarshal(args, &request); err != nil || request.Request.SessionID == "" {
		return nil, errors.New("session/rename 参数无效")
	}
	h.mu.Lock()
	session := h.sessions[request.Request.SessionID]
	if session != nil {
		session.title = request.Request.Title
	}
	h.mu.Unlock()
	if session == nil {
		return nil, errors.New("会话不存在")
	}
	return marshalValue(map[string]any{"title": request.Request.Title})
}

func (h *DSHHost) serveMux(writer http.ResponseWriter, request *http.Request) {
	if !h.authorized(request) {
		http.Error(writer, "dsh web authentication required", http.StatusUnauthorized)
		return
	}
	socket, err := websocket.Accept(writer, request, nil)
	if err != nil {
		return
	}
	connection := &dshMuxConnection{socket: socket}
	defer socket.CloseNow()
	if !h.registerConnection(connection) {
		return
	}
	defer h.unregisterConnection(connection)

	readContext, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	_, data, err := socket.Read(readContext)
	cancel()
	if err != nil {
		return
	}
	var open struct {
		Type     string `json:"type"`
		StreamID string `json:"streamId"`
		Endpoint string `json:"endpoint"`
	}
	if json.Unmarshal(data, &open) != nil || open.Type != "open" || open.StreamID != "events" || open.Endpoint != "$events" {
		return
	}

	readyFrame, err := marshalMuxItem(map[string]any{"type": "ready", "clientId": h.clientID})
	if err != nil || connection.write(readyFrame) != nil {
		return
	}
	h.markReady(connection)

	h.mu.Lock()
	decisions := append([]DSHDecision(nil), h.decisions...)
	h.mu.Unlock()
	for _, decision := range decisions {
		frame, err := marshalMuxItem(map[string]any{
			"type": "waterfall", "eventId": decision.EventID, "agentId": decision.AgentID,
			"event": decision.Event, "request": decision.Request,
		})
		if err != nil || connection.write(frame) != nil {
			return
		}
	}

	for {
		if _, _, err := socket.Read(context.Background()); err != nil {
			return
		}
	}
}

func (h *DSHHost) registerConnection(connection *dshMuxConnection) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.connections[connection] = struct{}{}
	return true
}

func (h *DSHHost) unregisterConnection(connection *dshMuxConnection) {
	h.mu.Lock()
	delete(h.connections, connection)
	h.mu.Unlock()
	h.signal()
}

func (h *DSHHost) markReady(connection *dshMuxConnection) {
	h.mu.Lock()
	connection.ready = true
	h.readyCount++
	h.readyClient = h.clientID
	h.mu.Unlock()
	h.signal()
}

func (h *DSHHost) readyConnectionsLocked() []*dshMuxConnection {
	result := make([]*dshMuxConnection, 0, len(h.connections))
	for connection := range h.connections {
		if connection.ready {
			result = append(result, connection)
		}
	}
	return result
}

func (h *DSHHost) recordCall(method string, args json.RawMessage) {
	h.mu.Lock()
	h.calls = append(h.calls, DSHCall{Method: method, Args: append(json.RawMessage(nil), args...)})
	h.mu.Unlock()
	h.signal()
}

func (h *DSHHost) authorized(request *http.Request) bool {
	for _, cookie := range request.Cookies() {
		if !strings.HasPrefix(cookie.Name, "dsh-auth-") {
			continue
		}
		if h.validCookie(cookie.Name, cookie.Value) {
			return true
		}
	}
	return false
}

// validCookie 按 dsh 的短期 Cookie 规则校验 authority、HMAC、版本和有效期。
func (h *DSHHost) validCookie(name, value string) bool {
	expectedHash := sha256.Sum256([]byte(h.authority))
	if name != "dsh-auth-"+base64.RawURLEncoding.EncodeToString(expectedHash[:]) {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, h.secret)
	mac.Write([]byte(parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return false
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var payload struct {
		Version   int    `json:"version"`
		Authority string `json:"authority"`
		IssuedAt  int64  `json:"issuedAt"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if json.Unmarshal(payloadJSON, &payload) != nil || payload.Version != 1 ||
		payload.Authority != h.authority || payload.IssuedAt <= 0 {
		return false
	}
	return payload.ExpiresAt > time.Now().UnixMilli()
}

func (h *DSHHost) writeRPCSuccess(writer http.ResponseWriter, rpcID string, value json.RawMessage) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "server-response", "rpcId": rpcID,
		"result": map[string]any{"ok": true, "value": value},
	})
}

func (h *DSHHost) writeRPCError(writer http.ResponseWriter, rpcID string, failure DSHHostError) {
	if failure.Code == "" {
		failure.Code = "fake-error"
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"type": "server-response", "rpcId": rpcID,
		"result": map[string]any{
			"ok": false, "error": map[string]any{"code": failure.Code, "message": failure.Message},
		},
	})
}

func (h *DSHHost) wait(t testing.TB, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(testWaitTimeout)
	defer deadline.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-h.changed:
		case <-deadline.C:
			t.Fatalf("等待超时：%s", description)
		}
	}
}

func (h *DSHHost) signal() {
	select {
	case h.changed <- struct{}{}:
	default:
	}
}

func (c *dshMuxConnection) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.socket.Write(ctx, websocket.MessageText, payload)
}

func (c *dshMuxConnection) close() { _ = c.socket.CloseNow() }

func marshalMuxItem(value any) ([]byte, error) {
	return json.Marshal(map[string]any{"streamId": "events", "type": "item", "value": value})
}

func marshalValue(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func (s *dshFakeSession) asOfSeq() int64 {
	var maximum int64 = -1
	for _, event := range s.events {
		if int64(event.Seq) > maximum {
			maximum = int64(event.Seq)
		}
	}
	return maximum
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
