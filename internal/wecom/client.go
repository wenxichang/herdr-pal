package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
}

// String 返回不包含订阅密钥的配置摘要。
func (config ClientConfig) String() string {
	return fmt.Sprintf("ClientConfig{Endpoint:%q BotID:%q AllowedUserID:%q}", config.Endpoint, config.BotID, config.AllowedUserID)
}

// GoString 返回不包含订阅密钥的配置摘要。
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

	mu         sync.Mutex
	current    *session
	runStarted bool
	closeOnce  sync.Once
}

// String 返回不包含订阅密钥的客户端摘要。
func (c *Client) String() string {
	if c == nil {
		return "Client<nil>"
	}
	return fmt.Sprintf("Client{Endpoint:%q BotID:%q AllowedUserID:%q}", c.endpoint, c.botID, c.allowedUserID)
}

// GoString 返回不包含订阅密钥的客户端摘要。
func (c *Client) GoString() string { return c.String() }

// NewClient 根据配置创建企业微信智能机器人客户端。
//
// 校验错误不会包含订阅密钥或待发送内容。
func NewClient(config ClientConfig) (*Client, error) {
	if strings.TrimSpace(config.Endpoint) == "" || strings.TrimSpace(config.BotID) == "" || strings.TrimSpace(config.Secret) == "" || strings.TrimSpace(config.AllowedUserID) == "" {
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
		backoff: config.Backoff, wait: config.Wait,
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
		socket, err := c.dial(ctx, c.endpoint)
		if err != nil {
			if err := c.wait(ctx, c.backoff.Next()); err != nil {
				return err
			}
			continue
		}
		session := newSession(ctx, socket, c.events)
		if err := c.subscribe(ctx, session); err != nil {
			session.finish(ErrUnavailable)
			session.wait()
			if errors.Is(err, ErrProtocol) {
				return err
			}
			if err := c.wait(ctx, c.backoff.Next()); err != nil {
				return err
			}
			continue
		}
		c.install(session)
		if err := session.activate(); err != nil {
			session.finish(ErrUnavailable)
			session.wait()
			c.clearIfCurrent(session)
			if err := c.wait(ctx, c.backoff.Next()); err != nil {
				return err
			}
			continue
		}
		c.backoff.Reset()
		session.startHeartbeat(c)
		<-session.done
		session.wait()
		c.clearIfCurrent(session)
		if errors.Is(session.reason(), ErrProtocol) {
			return session.reason()
		}
		if err := session.flushQueued(ctx); err != nil {
			return err
		}
		if err := c.wait(ctx, c.backoff.Next()); err != nil {
			return err
		}
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
				session.finish(ErrUnavailable)
				return
			}
			ctx, cancel := context.WithTimeout(session.ctx, c.requestTimeout)
			err = session.request(ctx, requestID, payload)
			cancel()
			if err != nil {
				session.finish(ErrUnavailable)
				return
			}
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
	if c == nil {
		return ErrUnavailable
	}
	requestID := c.requestID()
	payload, err := EncodeSendMarkdown(requestID, c.allowedUserID, content)
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
	connection, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return connection, nil
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

	readyMu sync.Mutex
	ready   bool
	queued  []IncomingText
}

func newSession(parent context.Context, socket Socket, events chan<- IncomingText) *session {
	ctx, cancel := context.WithCancel(parent)
	session := &session{ctx: ctx, cancel: cancel, socket: socket, events: events, done: make(chan struct{})}
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
			s.finish(ErrUnavailable)
			return
		}
		if messageType != websocket.MessageText {
			s.finish(ErrUnavailable)
			return
		}
		frame, err := DecodeFrame(data)
		if err != nil {
			s.finish(err)
			return
		}
		switch frame.Kind {
		case FrameResponse:
			s.pending.resolve(*frame.Response)
		case FrameIncomingText:
			if err := s.enqueue(*frame.IncomingText); err != nil {
				s.finish(ErrUnavailable)
				return
			}
		case FrameDisconnected:
			s.finish(ErrUnavailable)
			return
		}
	}
}

func (s *session) enqueue(event IncomingText) error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.ready {
		if len(s.queued) >= cap(s.events)+1 {
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
