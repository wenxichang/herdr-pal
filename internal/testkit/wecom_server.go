package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// WeComRequest 记录 fake 企业微信收到的一次客户端请求。
type WeComRequest struct {
	Command   string
	RequestID string
	ChatID    string
	ChatType  int
	Content   string
}

// WeComServer 是通过 httptest 提供的企业微信智能机器人 WebSocket fake。
type WeComServer struct {
	server *httptest.Server
	botID  string
	secret string

	mu             sync.Mutex
	connections    map[*weComConnection]struct{}
	current        *weComConnection
	requests       []WeComRequest
	completed      []WeComRequest
	responseErrors map[string]int
	subscribeCount int
	changed        chan struct{}
	nextCallback   atomic.Uint64
}

type weComConnection struct {
	socket     *websocket.Conn
	writeMu    sync.Mutex
	subscribed bool
}

type weComWireRequest struct {
	Command string `json:"cmd"`
	Headers struct {
		RequestID string `json:"req_id"`
	} `json:"headers"`
	Body json.RawMessage `json:"body"`
}

// NewWeComServer 启动一个校验订阅凭据并自动按 req_id 响应的 WebSocket fake。
func NewWeComServer(t testing.TB, botID, secret string) *WeComServer {
	t.Helper()
	server := &WeComServer{
		botID: botID, secret: secret, connections: make(map[*weComConnection]struct{}),
		responseErrors: make(map[string]int), changed: make(chan struct{}, 1),
	}
	server.server = httptest.NewServer(http.HandlerFunc(server.accept))
	t.Cleanup(server.Close)
	return server
}

// SetResponseError 配置指定命令后续响应的企业微信错误码；code 为 0 时恢复成功响应。
func (s *WeComServer) SetResponseError(command string, code int) {
	s.mu.Lock()
	if code == 0 {
		delete(s.responseErrors, command)
	} else {
		s.responseErrors[command] = code
	}
	s.mu.Unlock()
	s.signal()
}

// Endpoint 返回可交给 WeComClient 的 ws:// 测试地址。
func (s *WeComServer) Endpoint() string {
	return strings.Replace(s.server.URL, "http://", "ws://", 1)
}

// WaitSubscribeCount 等待成功订阅连接累计达到 count 条。
func (s *WeComServer) WaitSubscribeCount(t testing.TB, count int) {
	t.Helper()
	s.wait(t, fmt.Sprintf("企业微信订阅达到 %d 次", count), func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.subscribeCount >= count
	})
}

// ConnectionCount 返回当前仍保持打开的企业微信 WebSocket 数量。
func (s *WeComServer) ConnectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.connections)
}

// WaitConnectionCount 等待当前企业微信 WebSocket 数量等于 count。
func (s *WeComServer) WaitConnectionCount(t testing.TB, count int) {
	t.Helper()
	s.wait(t, fmt.Sprintf("企业微信连接数等于 %d", count), func() bool {
		return s.ConnectionCount() == count
	})
}

// Requests 返回指定命令的请求记录副本；command 为空时返回全部。
func (s *WeComServer) Requests(command string) []WeComRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]WeComRequest, 0, len(s.requests))
	for _, request := range s.requests {
		if command == "" || request.Command == command {
			result = append(result, request)
		}
	}
	return result
}

// WaitRequestCount 等待指定企业微信命令至少收到 count 次。
func (s *WeComServer) WaitRequestCount(t testing.TB, command string, count int) []WeComRequest {
	t.Helper()
	var requests []WeComRequest
	s.wait(t, fmt.Sprintf("企业微信 %s 请求达到 %d 次", command, count), func() bool {
		requests = s.Requests(command)
		return len(requests) >= count
	})
	return requests
}

// CompletedRequests 返回已成功写回 req_id 响应的指定命令记录。
func (s *WeComServer) CompletedRequests(command string) []WeComRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]WeComRequest, 0, len(s.completed))
	for _, request := range s.completed {
		if command == "" || request.Command == command {
			result = append(result, request)
		}
	}
	return result
}

// WaitCompletedRequestCount 等待指定命令至少有 count 个请求已写回响应。
func (s *WeComServer) WaitCompletedRequestCount(t testing.TB, command string, count int) []WeComRequest {
	t.Helper()
	var requests []WeComRequest
	s.wait(t, fmt.Sprintf("企业微信 %s 响应完成达到 %d 次", command, count), func() bool {
		requests = s.CompletedRequests(command)
		return len(requests) >= count
	})
	return requests
}

// InjectText 向当前已订阅连接注入文本回调，并返回回调 req_id。
func (s *WeComServer) InjectText(t testing.TB, messageID, userID, chatType, content string) string {
	t.Helper()
	requestID := fmt.Sprintf("callback-%d", s.nextCallback.Add(1))
	payload, err := json.Marshal(map[string]any{
		"cmd": "aibot_msg_callback", "headers": map[string]any{"req_id": requestID},
		"body": map[string]any{
			"msgid": messageID, "aibotid": s.botID, "chattype": chatType,
			"from": map[string]any{"userid": userID}, "msgtype": "text",
			"text": map[string]any{"content": content},
		},
	})
	if err != nil {
		t.Fatalf("编码企业微信回调：%v", err)
	}
	connection := s.currentConnection(t)
	if err := connection.write(payload); err != nil {
		t.Fatalf("注入企业微信回调：%v", err)
	}
	return requestID
}

// SendDisconnectedEvent 向当前连接注入 disconnected_event。
func (s *WeComServer) SendDisconnectedEvent(t testing.TB) {
	t.Helper()
	requestID := fmt.Sprintf("disconnect-%d", s.nextCallback.Add(1))
	payload, err := json.Marshal(map[string]any{
		"cmd": "aibot_event_callback", "headers": map[string]any{"req_id": requestID},
		"body": map[string]any{
			"msgid": "event-" + requestID, "create_time": time.Now().Unix(), "aibotid": s.botID,
			"msgtype": "event", "event": map[string]any{"eventtype": "disconnected_event"},
		},
	})
	if err != nil {
		t.Fatalf("编码企业微信断开事件：%v", err)
	}
	connection := s.currentConnection(t)
	if err := connection.write(payload); err != nil {
		t.Fatalf("发送企业微信断开事件：%v", err)
	}
}

// Disconnect 主动关闭当前 WebSocket 连接。
func (s *WeComServer) Disconnect() {
	s.mu.Lock()
	connection := s.current
	s.mu.Unlock()
	if connection != nil {
		_ = connection.socket.CloseNow()
	}
}

// Close 停止 HTTP server 并关闭当前全部 WebSocket。
func (s *WeComServer) Close() {
	s.mu.Lock()
	connections := make([]*weComConnection, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		_ = connection.socket.CloseNow()
	}
	s.server.Close()
}

func (s *WeComServer) accept(writer http.ResponseWriter, request *http.Request) {
	socket, err := websocket.Accept(writer, request, nil)
	if err != nil {
		return
	}
	connection := &weComConnection{socket: socket}
	s.mu.Lock()
	s.connections[connection] = struct{}{}
	s.mu.Unlock()
	s.signal()
	defer func() {
		s.mu.Lock()
		delete(s.connections, connection)
		if s.current == connection {
			s.current = nil
		}
		s.mu.Unlock()
		s.signal()
		_ = socket.Close(websocket.StatusNormalClosure, "fake handler completed")
	}()

	for {
		messageType, data, err := socket.Read(request.Context())
		if err != nil {
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var wire weComWireRequest
		if err := json.Unmarshal(data, &wire); err != nil || wire.Command == "" || wire.Headers.RequestID == "" {
			return
		}
		record, valid := s.decodeRequest(wire)
		s.record(record)
		code, message := 0, "ok"
		if !valid {
			code, message = 40001, "invalid request"
		} else if configured := s.responseError(wire.Command); configured != 0 {
			code, message = configured, "configured error"
		}
		response, err := json.Marshal(map[string]any{
			"headers": map[string]any{"req_id": wire.Headers.RequestID}, "errcode": code, "errmsg": message,
		})
		if err != nil || connection.write(response) != nil {
			return
		}
		s.mu.Lock()
		s.completed = append(s.completed, record)
		if wire.Command == "aibot_subscribe" && valid && code == 0 {
			connection.subscribed = true
			s.current = connection
			s.subscribeCount++
		}
		s.mu.Unlock()
		s.signal()
	}
}

func (s *WeComServer) responseError(command string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.responseErrors[command]
}

func (s *WeComServer) decodeRequest(wire weComWireRequest) (WeComRequest, bool) {
	record := WeComRequest{Command: wire.Command, RequestID: wire.Headers.RequestID}
	switch wire.Command {
	case "aibot_subscribe":
		var body struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		}
		return record, json.Unmarshal(wire.Body, &body) == nil && body.BotID == s.botID && body.Secret == s.secret
	case "aibot_respond_msg", "aibot_send_msg":
		var body struct {
			ChatID   string `json:"chatid"`
			ChatType int    `json:"chat_type"`
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if json.Unmarshal(wire.Body, &body) != nil {
			return record, false
		}
		record.ChatID, record.ChatType, record.Content = body.ChatID, body.ChatType, body.Markdown.Content
		return record, true
	case "ping":
		return record, true
	default:
		return record, false
	}
}

func (s *WeComServer) record(request WeComRequest) {
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	s.signal()
}

func (s *WeComServer) currentConnection(t testing.TB) *weComConnection {
	t.Helper()
	var connection *weComConnection
	s.wait(t, "企业微信已订阅连接", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		connection = s.current
		return connection != nil && connection.subscribed
	})
	return connection
}

func (s *WeComServer) wait(t testing.TB, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(testWaitTimeout)
	defer deadline.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-s.changed:
		case <-deadline.C:
			t.Fatalf("等待超时：%s", description)
		}
	}
}

func (s *WeComServer) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (c *weComConnection) write(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.socket.Write(ctx, websocket.MessageText, payload)
}
