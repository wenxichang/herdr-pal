package wecom

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	defaultRequestTimeout    = 15 * time.Second
	defaultEventsCapacity    = 64
)

// ErrEventQueueFull 表示已校验的入站回调暂时无法写入有界事件队列。
var ErrEventQueueFull = fmt.Errorf("%w: 入站事件队列已满", ErrUnavailable)

// Socket 是企业微信 WebSocket 连接的最小抽象，便于本地测试会话行为。
type Socket interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, data []byte) error
	Close(code websocket.StatusCode, reason string) error
}

// DialFunc 建立一条到企业微信长连接端点的 WebSocket 连接。
type DialFunc func(ctx context.Context, endpoint string) (Socket, error)

// ClientConfig 是创建企业微信智能机器人连接客户端所需的配置。
type ClientConfig struct {
	Endpoint          string
	BotID             string
	Secret            string
	AllowedUserID     string
	Dial              DialFunc
	RequestID         func() string
	HeartbeatInterval time.Duration
	RequestTimeout    time.Duration
	EventsCapacity    int
	Backoff           *Backoff
	Wait              func(context.Context, time.Duration) error
	Logger            *slog.Logger
}

// String 返回不包含订阅密钥、原始机器人标识和用户标识的配置摘要。
func (config ClientConfig) String() string {
	return fmt.Sprintf("ClientConfig{Endpoint:%q BotHash:%q UserHash:%q}",
		weComLogEndpoint(config.Endpoint), wecomShortHash(config.BotID), wecomShortHash(config.AllowedUserID))
}

// GoString 返回不包含订阅密钥、原始机器人标识和用户标识的配置摘要。
func (config ClientConfig) GoString() string { return config.String() }

// Client 持续维护一条已订阅的企业微信智能机器人长连接。
//
// Run 只能调用一次；Run 退出后 Events 会被关闭。
type Client struct {
	endpoint      string
	botID         string
	secret        string
	allowedUserID string

	dial              DialFunc
	requestID         func() string
	heartbeatInterval time.Duration
	requestTimeout    time.Duration
	backoff           *Backoff
	wait              func(context.Context, time.Duration) error
	events            chan IncomingText
	logger            *slog.Logger

	mu         sync.Mutex
	current    *session
	runStarted bool
	closeOnce  sync.Once
}

// String 返回不包含订阅密钥、原始机器人标识和用户标识的客户端摘要。
func (c *Client) String() string {
	if c == nil {
		return "Client<nil>"
	}
	return fmt.Sprintf("Client{Endpoint:%q BotHash:%q UserHash:%q}",
		weComLogEndpoint(c.endpoint), wecomShortHash(c.botID), wecomShortHash(c.allowedUserID))
}

// GoString 返回不包含订阅密钥、原始机器人标识和用户标识的客户端摘要。
func (c *Client) GoString() string { return c.String() }

// NewClient 根据配置创建企业微信智能机器人客户端。
//
// 校验错误不会包含订阅密钥或待发送内容。
func NewClient(config ClientConfig) (*Client, error) {
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.BotID) == "" || strings.TrimSpace(config.Secret) == "" {
		return nil, newProtocolError("", 0, "连接配置缺失")
	}
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || (endpoint.Scheme != "ws" && endpoint.Scheme != "wss") || endpoint.Host == "" {
		return nil, newProtocolError("", 0, "连接地址无效")
	}
	if config.Dial == nil {
		config.Dial = productionDial
	}
	if config.RequestID == nil {
		config.RequestID = randomRequestID
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaultHeartbeatInterval
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.EventsCapacity <= 0 {
		config.EventsCapacity = defaultEventsCapacity
	}
	if config.Backoff == nil {
		config.Backoff = NewBackoff(defaultBackoffMin, defaultBackoffMax, nil)
	}
	if config.Wait == nil {
		config.Wait = WaitBackoff
	}
	return &Client{
		endpoint: config.Endpoint, botID: config.BotID, secret: config.Secret, allowedUserID: config.AllowedUserID,
		dial: config.Dial, requestID: config.RequestID, heartbeatInterval: config.HeartbeatInterval,
		requestTimeout: config.RequestTimeout, events: make(chan IncomingText, config.EventsCapacity),
		backoff: config.Backoff, wait: config.Wait, logger: config.Logger,
	}, nil
}

// Run 持续维护唯一的已订阅 WebSocket 会话，直到 context 被取消。
func (c *Client) Run(ctx context.Context) error {
	if c == nil {
		return ErrUnavailable
	}
	c.mu.Lock()
	if c.runStarted {
		c.mu.Unlock()
		return ErrUnavailable
	}
	c.runStarted = true
	c.mu.Unlock()
	defer c.closeEvents()
	defer c.clearCurrentAndClose()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connectionFields := c.connectionLogFields()
		c.logInfo("企业微信连接中", connectionFields...)
		socket, err := c.dial(ctx, c.endpoint)
		if err != nil {
			delay := c.backoff.Next()
			fields := append(connectionFields, weComDialLogFields(err)...)
			fields = append(fields, "retry_delay", delay)
			c.logWarn("企业微信连接失败", fields...)
			if err := c.wait(ctx, delay); err != nil {
				return err
			}
			continue
		}
		session := newSession(ctx, socket, c.events, c.logger)
		if err := c.subscribe(ctx, session); err != nil {
			session.finish(ErrUnavailable)
			session.wait()
			if errors.Is(err, ErrProtocol) || errors.Is(err, ErrEventQueueFull) {
				c.logError("企业微信订阅失败", wecomErrorLogFields(err)...)
				return err
			}
			delay := c.backoff.Next()
			c.logWarn("企业微信订阅暂时失败", append(wecomErrorLogFields(err), "retry_delay", delay)...)
			if err := c.wait(ctx, delay); err != nil {
				return err
			}
			continue
		}
		c.install(session)
		if err := session.activate(); err != nil {
			session.finish(ErrUnavailable)
			session.wait()
			c.clearIfCurrent(session)
			if flushErr := session.flushQueued(ctx); flushErr != nil {
				c.logError("企业微信待处理消息恢复失败", wecomErrorLogFields(flushErr)...)
				return flushErr
			}
			delay := c.backoff.Next()
			c.logWarn("企业微信会话激活失败", append(wecomErrorLogFields(err), "retry_delay", delay)...)
			if err := c.wait(ctx, delay); err != nil {
				return err
			}
			continue
		}
		c.backoff.Reset()
		c.logInfo("企业微信订阅成功", connectionFields...)
		session.startHeartbeat(c)
		<-session.done
		session.wait()
		c.clearIfCurrent(session)
		if err := ctx.Err(); err != nil {
			return err
		}
		disconnectedFields := append(connectionFields, wecomErrorLogFields(session.reason())...)
		c.logWarn("企业微信连接已断开", disconnectedFields...)
		if errors.Is(session.reason(), ErrProtocol) {
			return session.reason()
		}
		if err := session.flushQueued(ctx); err != nil {
			c.logError("企业微信待处理消息恢复失败", wecomErrorLogFields(err)...)
			return err
		}
		delay := c.backoff.Next()
		c.logInfo("企业微信准备重连", "retry_delay", delay)
		if err := c.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) logInfo(message string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(message, args...)
	}
}

func (c *Client) logDebug(message string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Debug(message, args...)
	}
}

func (c *Client) logWarn(message string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(message, args...)
	}
}

func (c *Client) logError(message string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Error(message, args...)
	}
}

func (c *Client) connectionLogFields() []any {
	if c == nil {
		return nil
	}
	return []any{"endpoint", weComLogEndpoint(c.endpoint), "bot_hash", wecomShortHash(c.botID)}
}

func wecomErrorType(err error) string {
	switch {
	case errors.Is(err, ErrEventQueueFull):
		return "event_queue_full"
	case errors.Is(err, ErrProtocol):
		return "protocol"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	default:
		return "transport"
	}
}

func wecomErrorLogFields(err error) []any {
	fields := []any{"error_type", wecomErrorType(err), "reason", safeWeComErrorReason(err)}
	var protocolError *ProtocolError
	if errors.As(err, &protocolError) && protocolError.ErrCode != 0 {
		fields = append(fields, "error_code", protocolError.ErrCode)
	}
	return fields
}

func safeWeComErrorReason(err error) string {
	switch {
	case err == nil:
		return "未提供断开原因"
	case errors.Is(err, ErrEventQueueFull):
		return "企业微信入站事件队列已满"
	case errors.Is(err, ErrProtocol):
		return "企业微信返回协议或业务错误"
	case errors.Is(err, context.DeadlineExceeded):
		return "企业微信请求等待响应超时"
	case errors.Is(err, context.Canceled):
		return "企业微信操作上下文已取消"
	case errors.Is(err, ErrUnavailable):
		return "企业微信 WebSocket 连接不可用"
	default:
		reason := strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(err.Error(), "�"))
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return "企业微信操作失败但未提供具体原因"
		}
		if len(reason) > logFieldByteLimit {
			reason = reason[:logFieldByteLimit] + "…"
		}
		return reason
	}
}

func (c *Client) heartbeat(session *session) {
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-session.done:
			return
		case <-ticker.C:
			requestID := c.requestID()
			payload, err := EncodePing(requestID)
			if err != nil {
				c.logWarn("企业微信心跳编码失败", wecomErrorLogFields(err)...)
				session.finish(ErrUnavailable)
				return
			}
			ctx, cancel := context.WithTimeout(session.ctx, c.requestTimeout)
			err = session.request(ctx, requestID, payload)
			cancel()
			if err != nil {
				c.logWarn("企业微信心跳失败", append([]any{"request_hash", wecomShortHash(requestID)}, wecomErrorLogFields(err)...)...)
				session.finish(ErrUnavailable)
				return
			}
			c.logDebug("企业微信心跳成功", "request_hash", wecomShortHash(requestID))
		}
	}
}

// Events 返回已完成协议校验且订阅成功后接收到的文本回调流。
func (c *Client) Events() <-chan IncomingText {
	if c == nil {
		return nil
	}
	return c.events
}

// RespondMarkdown 回复指定企业微信回调请求，并复用其 req_id。
func (c *Client) RespondMarkdown(ctx context.Context, callbackRequestID, content string) error {
	payload, err := EncodeRespondMarkdown(callbackRequestID, content)
	if err != nil {
		return err
	}
	return c.request(ctx, callbackRequestID, payload)
}

// SendMarkdown 向配置中唯一允许的单聊用户主动发送 Markdown 消息。
func (c *Client) SendMarkdown(ctx context.Context, content string) error {
	return c.SendMarkdownTo(ctx, c.allowedUserID, content)
}

// SendMarkdownTo 向指定单聊用户主动发送 Markdown 消息。
func (c *Client) SendMarkdownTo(ctx context.Context, userID, content string) error {
	if c == nil {
		return ErrUnavailable
	}
	requestID := c.requestID()
	payload, err := EncodeSendMarkdown(requestID, userID, content)
	if err != nil {
		return err
	}
	return c.request(ctx, requestID, payload)
}

func (c *Client) subscribe(parent context.Context, session *session) error {
	requestID := c.requestID()
	payload, err := EncodeSubscribe(requestID, c.botID, c.secret)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, c.requestTimeout)
	defer cancel()
	return session.request(ctx, requestID, payload)
}

func (c *Client) request(parent context.Context, requestID string, payload []byte) error {
	if c == nil {
		return ErrUnavailable
	}
	session := c.getCurrent()
	if session == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, c.requestTimeout)
	defer cancel()
	return session.request(ctx, requestID, payload)
}

func (c *Client) install(next *session) {
	c.mu.Lock()
	previous := c.current
	c.current = next
	c.mu.Unlock()
	if previous != nil && previous != next {
		previous.finish(ErrUnavailable)
		previous.wait()
	}
}

func (c *Client) getCurrent() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *Client) clearIfCurrent(candidate *session) {
	c.mu.Lock()
	if c.current == candidate {
		c.current = nil
	}
	c.mu.Unlock()
}

func (c *Client) clearCurrentAndClose() {
	c.mu.Lock()
	current := c.current
	c.current = nil
	c.mu.Unlock()
	if current != nil {
		current.finish(ErrUnavailable)
		current.wait()
	}
}

func (c *Client) closeEvents() { c.closeOnce.Do(func() { close(c.events) }) }

func productionDial(ctx context.Context, endpoint string) (Socket, error) {
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: weComWebSocketTransport{base: http.DefaultTransport}},
	})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, newWeComDialError(err, response)
	}
	return connection, nil
}

type weComDialError struct {
	kind       string
	statusCode int
	cause      error
}

func (err *weComDialError) Error() string {
	if err == nil {
		return "企业微信连接失败"
	}
	return "企业微信连接失败: " + err.kind
}

func (err *weComDialError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newWeComDialError(cause error, response *http.Response) error {
	if response != nil {
		return &weComDialError{kind: "handshake", statusCode: response.StatusCode, cause: cause}
	}
	kind := "transport"
	var dnsError *net.DNSError
	var networkError net.Error
	message := strings.ToLower(fmt.Sprint(cause))
	if errors.Is(cause, context.Canceled) {
		kind = "context_canceled"
	} else if errors.Is(cause, context.DeadlineExceeded) {
		kind = "timeout"
	} else if errors.As(cause, &dnsError) {
		kind = "dns"
	} else if strings.Contains(message, "tls:") || strings.Contains(message, "x509:") || strings.Contains(message, "certificate") {
		kind = "tls"
	} else if errors.As(cause, &networkError) && networkError.Timeout() {
		kind = "timeout"
	}
	return &weComDialError{kind: kind, cause: cause}
}

func weComDialLogFields(err error) []any {
	var dialErr *weComDialError
	if !errors.As(err, &dialErr) {
		return []any{"error_type", "transport", "reason", safeWeComErrorReason(err)}
	}
	reason := safeWeComErrorReason(dialErr.cause)
	switch dialErr.kind {
	case "handshake":
		if dialErr.cause == nil {
			reason = "企业微信 WebSocket 握手被 HTTP 服务拒绝"
		}
	case "context_canceled":
		reason = "企业微信连接操作已取消"
	case "timeout":
		reason = "企业微信连接超时"
	}
	fields := []any{"error_type", dialErr.kind, "reason", reason}
	if dialErr.statusCode > 0 {
		fields = append(fields, "http_status", dialErr.statusCode)
	}
	return fields
}

func weComLogEndpoint(raw string) string {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "invalid-endpoint"
	}
	return endpoint.Scheme + "://" + endpoint.Host
}

type weComWebSocketTransport struct {
	base http.RoundTripper
}

func (transport weComWebSocketTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	rewriteHeaderSpelling(clone.Header, "Sec-Websocket-Key", "Sec-WebSocket-Key")
	rewriteHeaderSpelling(clone.Header, "Sec-Websocket-Version", "Sec-WebSocket-Version")
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func rewriteHeaderSpelling(header http.Header, source, target string) {
	values, ok := header[source]
	if !ok {
		return
	}
	header[target] = values
	delete(header, source)
}

func randomRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "request-fallback-" + formatUint(fallbackRequestID.Add(1))
	}
	return hex.EncodeToString(bytes[:])
}

var fallbackRequestID atomic.Uint64

func formatUint(value uint64) string {
	return hex.EncodeToString([]byte{byte(value >> 56), byte(value >> 48), byte(value >> 40), byte(value >> 32), byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

type session struct {
	ctx        context.Context
	cancel     context.CancelFunc
	socket     Socket
	events     chan<- IncomingText
	pending    pendingRequests
	writeMu    sync.Mutex
	done       chan struct{}
	finishOnce sync.Once
	wg         sync.WaitGroup
	reasonMu   sync.Mutex
	finalErr   error

	readyMu    sync.Mutex
	ready      bool
	overflowed bool
	queued     []IncomingText
	logger     *slog.Logger
}

func newSession(parent context.Context, socket Socket, events chan<- IncomingText, loggers ...*slog.Logger) *session {
	ctx, cancel := context.WithCancel(parent)
	var logger *slog.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	session := &session{ctx: ctx, cancel: cancel, socket: socket, events: events, done: make(chan struct{}), logger: logger}
	session.wg.Add(1)
	go func() { defer session.wg.Done(); session.readLoop() }()
	return session
}

func (s *session) request(ctx context.Context, requestID string, payload []byte) error {
	if s == nil {
		return ErrUnavailable
	}
	wait, err := s.pending.register(requestID)
	if err != nil {
		return err
	}
	if err := s.write(ctx, payload); err != nil {
		owned := s.pending.cancel(requestID)
		if !owned {
			return (<-wait).Err
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.finish(ErrUnavailable)
			return ErrUnavailable
		}
		return err
	}
	select {
	case result := <-wait:
		return result.Err
	case <-ctx.Done():
		if s.pending.cancel(requestID) {
			return ctx.Err()
		}
		return (<-wait).Err
	case <-s.done:
		if s.pending.cancel(requestID) {
			if reason := s.reason(); reason != nil {
				return reason
			}
			return ErrUnavailable
		}
		return (<-wait).Err
	}
}

func (s *session) write(ctx context.Context, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-s.done:
		return ErrUnavailable
	default:
	}
	return s.socket.Write(ctx, websocket.MessageText, payload)
}

func (s *session) activate() error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	select {
	case <-s.done:
		return ErrUnavailable
	default:
	}
	for index, event := range s.queued {
		select {
		case s.events <- event:
		default:
			s.queued = append([]IncomingText(nil), s.queued[index:]...)
			return ErrEventQueueFull
		}
	}
	s.queued = nil
	s.ready = true
	return nil
}

func (s *session) readLoop() {
	for {
		messageType, data, err := s.socket.Read(s.ctx)
		if err != nil {
			if s.logger != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("企业微信 WebSocket 读取失败", wecomErrorLogFields(err)...)
			}
			s.finish(ErrUnavailable)
			return
		}
		if messageType != websocket.MessageText {
			if s.logger != nil {
				s.logger.Warn("企业微信 WebSocket 消息类型无效", "error_type", "invalid_message_type", "reason", "企业微信连接只接受文本帧", "message_type", messageType)
			}
			s.finish(ErrUnavailable)
			return
		}
		frame, err := DecodeFrame(data)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("企业微信消息解析失败", wecomErrorLogFields(err)...)
			}
			s.finish(err)
			return
		}
		switch frame.Kind {
		case FrameResponse:
			matched := s.pending.resolve(*frame.Response)
			if s.logger != nil {
				if matched {
					s.logger.Debug("企业微信请求响应已接收", "request_hash", wecomShortHash(frame.Response.Headers.RequestID), "error_code", frame.Response.ErrCode)
				} else {
					s.logger.Warn("企业微信请求响应未匹配", "request_hash", wecomShortHash(frame.Response.Headers.RequestID), "error_code", frame.Response.ErrCode, "error_type", "unmatched_request", "reason", "响应没有对应的在途请求")
				}
			}
		case FrameIncomingText:
			if s.logger != nil {
				s.logger.Debug("企业微信文本消息已接收",
					"user_hash", wecomShortHash(frame.IncomingText.UserID),
					"message_hash", wecomShortHash(frame.IncomingText.MessageID),
					"request_hash", wecomShortHash(frame.IncomingText.RequestID),
					"chat_type", frame.IncomingText.ChatType,
					"content_bytes", len([]byte(frame.IncomingText.Content)),
				)
			}
			if err := s.enqueue(*frame.IncomingText); err != nil {
				if s.logger != nil {
					s.logger.Warn("企业微信文本消息入队失败", append([]any{"user_hash", wecomShortHash(frame.IncomingText.UserID), "message_hash", wecomShortHash(frame.IncomingText.MessageID)}, wecomErrorLogFields(err)...)...)
				}
				s.finish(err)
				return
			}
		case FrameUnsupportedCallback:
			if s.logger != nil && frame.Unsupported != nil {
				s.logger.Warn("企业微信不支持的消息类型已忽略",
					"message_type", frame.Unsupported.MessageType,
					"chat_type", frame.Unsupported.ChatType,
					"user_hash", wecomShortHash(frame.Unsupported.UserID),
					"message_hash", wecomShortHash(frame.Unsupported.MessageID),
					"reason", "当前版本只支持企业微信文本单聊消息",
				)
			}
		case FrameDisconnected:
			if s.logger != nil {
				s.logger.Warn("企业微信服务端要求断开连接", "error_type", "remote_disconnect", "reason", "收到企业微信 disconnected_event")
			}
			s.finish(ErrUnavailable)
			return
		}
	}
}

func wecomShortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}

func (s *session) enqueue(event IncomingText) error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.ready {
		if s.overflowed {
			return ErrUnavailable
		}
		if len(s.queued) >= cap(s.events) {
			s.queued = append(s.queued, event)
			s.overflowed = true
			return ErrEventQueueFull
		}
		s.queued = append(s.queued, event)
		return nil
	}
	select {
	case s.events <- event:
		return nil
	default:
		s.queued = append(s.queued, event)
		return ErrEventQueueFull
	}
}

func (s *session) isReady() bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	return s.ready
}

func (s *session) finish(err error) {
	if err == nil {
		err = ErrUnavailable
	}
	s.finishOnce.Do(func() {
		s.reasonMu.Lock()
		s.finalErr = err
		s.reasonMu.Unlock()
		s.pending.cancelAll(err)
		s.cancel()
		if closer, ok := s.socket.(interface{ CloseNow() error }); ok && !errors.Is(err, context.Canceled) {
			_ = closer.CloseNow()
		} else {
			_ = s.socket.Close(websocket.StatusNormalClosure, "connection unavailable")
		}
		close(s.done)
	})
}

func (s *session) startHeartbeat(client *Client) {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); client.heartbeat(s) }()
}

func (s *session) wait() { s.wg.Wait() }

func (s *session) reason() error {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	return s.finalErr
}

func (s *session) flushQueued(ctx context.Context) error {
	s.readyMu.Lock()
	queued := append([]IncomingText(nil), s.queued...)
	s.queued = nil
	s.readyMu.Unlock()
	for _, event := range queued {
		select {
		case s.events <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
