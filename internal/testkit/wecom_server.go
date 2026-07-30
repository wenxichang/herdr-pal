package testkit

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
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
	Command     string
	RequestID   string
	ChatID      string
	ChatType    int
	Content     string
	MediaType   string
	Filename    string
	TotalSize   int
	TotalChunks int
	MD5         string
	UploadID    string
	ChunkIndex  int
	Chunk       []byte
	MediaID     string
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
	nextUpload     atomic.Uint64
	uploads        map[string]*weComUpload
}

type weComUpload struct {
	mediaType   string
	filename    string
	totalSize   int
	totalChunks int
	md5         string
	chunks      map[int][]byte
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
		responseErrors: make(map[string]int), uploads: make(map[string]*weComUpload), changed: make(chan struct{}, 1),
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
		var responseBody any
		if code == 0 {
			responseBody, valid = s.completeRequest(record)
			if !valid {
				code, message = 40001, "invalid request"
			}
		}
		responseValue := map[string]any{
			"headers": map[string]any{"req_id": wire.Headers.RequestID}, "errcode": code, "errmsg": message,
		}
		if responseBody != nil && code == 0 {
			responseValue["body"] = responseBody
		}
		response, err := json.Marshal(responseValue)
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
			MsgType  string `json:"msgtype"`
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
			Image struct {
				MediaID string `json:"media_id"`
			} `json:"image"`
		}
		if json.Unmarshal(wire.Body, &body) != nil {
			return record, false
		}
		record.ChatID, record.ChatType, record.Content = body.ChatID, body.ChatType, body.Markdown.Content
		record.MediaType, record.MediaID = body.MsgType, body.Image.MediaID
		switch body.MsgType {
		case "markdown":
			return record, body.Markdown.Content != ""
		case "image":
			return record, wire.Command == "aibot_send_msg" && body.ChatID != "" && body.Image.MediaID != ""
		default:
			return record, false
		}
	case "aibot_upload_media_init":
		var body struct {
			Type        string `json:"type"`
			Filename    string `json:"filename"`
			TotalSize   int    `json:"total_size"`
			TotalChunks int    `json:"total_chunks"`
			MD5         string `json:"md5"`
		}
		if json.Unmarshal(wire.Body, &body) != nil {
			return record, false
		}
		record.MediaType, record.Filename, record.TotalSize = body.Type, body.Filename, body.TotalSize
		record.TotalChunks, record.MD5 = body.TotalChunks, body.MD5
		return record, body.Type == "image" && body.Filename != "" && body.TotalSize > 0 && body.TotalChunks > 0 && body.TotalChunks <= 100 && body.MD5 != ""
	case "aibot_upload_media_chunk":
		var body struct {
			UploadID   string `json:"upload_id"`
			ChunkIndex int    `json:"chunk_index"`
			Base64Data string `json:"base64_data"`
		}
		if json.Unmarshal(wire.Body, &body) != nil || body.UploadID == "" || body.ChunkIndex < 0 {
			return record, false
		}
		chunk, err := base64.StdEncoding.DecodeString(body.Base64Data)
		if err != nil || len(chunk) == 0 || len(chunk) > 512*1024 {
			return record, false
		}
		record.UploadID, record.ChunkIndex, record.Chunk = body.UploadID, body.ChunkIndex, chunk
		return record, s.validUploadChunk(record)
	case "aibot_upload_media_finish":
		var body struct {
			UploadID string `json:"upload_id"`
		}
		if json.Unmarshal(wire.Body, &body) != nil || body.UploadID == "" {
			return record, false
		}
		record.UploadID = body.UploadID
		return record, s.validUploadFinish(body.UploadID)
	case "ping":
		return record, true
	default:
		return record, false
	}
}

func (s *WeComServer) completeRequest(request WeComRequest) (any, bool) {
	switch request.Command {
	case "aibot_upload_media_init":
		uploadID := fmt.Sprintf("upload-%d", s.nextUpload.Add(1))
		s.mu.Lock()
		s.uploads[uploadID] = &weComUpload{
			mediaType: request.MediaType, filename: request.Filename, totalSize: request.TotalSize,
			totalChunks: request.TotalChunks, md5: request.MD5, chunks: make(map[int][]byte),
		}
		s.mu.Unlock()
		return map[string]any{"upload_id": uploadID}, true
	case "aibot_upload_media_chunk":
		s.mu.Lock()
		upload := s.uploads[request.UploadID]
		if upload != nil {
			upload.chunks[request.ChunkIndex] = append([]byte(nil), request.Chunk...)
		}
		s.mu.Unlock()
		return nil, upload != nil
	case "aibot_upload_media_finish":
		s.mu.Lock()
		upload := s.uploads[request.UploadID]
		delete(s.uploads, request.UploadID)
		s.mu.Unlock()
		if upload == nil {
			return nil, false
		}
		return map[string]any{
			"type": "file", "media_id": "media-" + request.UploadID, "created_at": time.Now().Unix(),
		}, true
	default:
		return nil, true
	}
}

func (s *WeComServer) validUploadChunk(request WeComRequest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload := s.uploads[request.UploadID]
	return upload != nil && request.ChunkIndex < upload.totalChunks
}

func (s *WeComServer) validUploadFinish(uploadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload := s.uploads[uploadID]
	if upload == nil || len(upload.chunks) != upload.totalChunks {
		return false
	}
	var content bytes.Buffer
	for index := 0; index < upload.totalChunks; index++ {
		chunk, exists := upload.chunks[index]
		if !exists {
			return false
		}
		content.Write(chunk)
	}
	digest := md5.Sum(content.Bytes())
	return content.Len() == upload.totalSize && fmt.Sprintf("%x", digest) == upload.md5
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
