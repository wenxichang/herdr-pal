package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

var (
	ErrInvalidHubDependency = errors.New("ClientHub 依赖无效")
	ErrClientUnavailable    = errors.New("Relay 客户端不可用")
	ErrClientQueueFull      = errors.New("Relay 客户端发送队列已满")
	ErrInflightFull         = errors.New("Relay 客户端在途请求已满")
	ErrInvalidHubRequest    = errors.New("Relay Hub 请求无效")
)

const (
	defaultHelloTimeout         = 10 * time.Second
	defaultFirstSnapshotTimeout = 10 * time.Second
	defaultHubHeartbeatInterval = 10 * time.Second
	defaultHubHeartbeatTimeout  = 30 * time.Second
	defaultSnapshotInterval     = 30 * time.Second
	defaultHubSendCapacity      = 128
	defaultHubMaxInflight       = 128
)

// HubConfig 是服务端连接状态机的时间和资源限制。
type HubConfig struct {
	HelloTimeout         time.Duration
	FirstSnapshotTimeout time.Duration
	HeartbeatInterval    time.Duration
	HeartbeatTimeout     time.Duration
	SnapshotInterval     time.Duration
	SendQueueCapacity    int
	MaxInflight          int
}

// ClientHub 接受 Relay WSS 连接并把请求路由到当前在线机器。
type ClientHub struct {
	catalog *SessionCatalog
	config  HubConfig
	logger  *slog.Logger

	mu      sync.RWMutex
	clients map[ClientKey]*clientConnection

	outboundMu sync.RWMutex
	outbound   HubOutboundSink
}

// HubOutboundSink 接收客户端后续分段和结构化主动通知。
type HubOutboundSink interface {
	SendPush(ctx context.Context, userID, content string) error
	SendNotification(ctx context.Context, userID, machineID string, notification relayproto.Notification) error
}

// NewClientHub 创建空的 Relay WSS 连接中心。
func NewClientHub(catalog *SessionCatalog, config HubConfig, logger *slog.Logger) (*ClientHub, error) {
	if catalog == nil || logger == nil {
		return nil, ErrInvalidHubDependency
	}
	config = normalizedHubConfig(config)
	return &ClientHub{catalog: catalog, config: config, logger: logger, clients: make(map[ClientKey]*clientConnection)}, nil
}

// Catalog 返回 Hub 使用的在线目录。
func (hub *ClientHub) Catalog() *SessionCatalog {
	if hub == nil {
		return nil
	}
	return hub.catalog
}

// SetOutboundSink 设置企业微信出站接收器；已有接收器时拒绝替换。
func (hub *ClientHub) SetOutboundSink(sink HubOutboundSink) error {
	if hub == nil || sink == nil {
		return ErrInvalidHubDependency
	}
	hub.outboundMu.Lock()
	defer hub.outboundMu.Unlock()
	if hub.outbound != nil {
		return ErrInvalidHubDependency
	}
	hub.outbound = sink
	return nil
}

// ServeHTTP 接受一条只允许 TLS 的 Relay WebSocket 连接。
func (hub *ClientHub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if hub == nil || request.TLS == nil {
		http.Error(writer, "Relay requires TLS", http.StatusUpgradeRequired)
		return
	}
	socket, err := websocket.Accept(writer, request, nil)
	if err != nil {
		return
	}
	socket.SetReadLimit(relayproto.MaxFrameBytes)
	hub.serveConnection(request.Context(), socket)
}

// Select 向目标机器发送稳定目标复核请求。
func (hub *ClientHub) Select(ctx context.Context, userID string, target relayproto.SessionRef) error {
	if err := relayproto.ValidateSessionRef(target); err != nil {
		return ErrTargetChanged
	}
	connection := hub.readyClient(ClientKey{UserID: userID, MachineID: target.MachineID})
	if connection == nil {
		return ErrClientUnavailable
	}
	frame, err := relayproto.NewFrame(relayproto.TypeSelectRequest, randomHubID(), relayproto.SelectRequest{Target: target})
	if err != nil {
		return err
	}
	response, err := connection.request(ctx, frame, relayproto.TypeSelectResult)
	if err != nil {
		return err
	}
	result, err := relayproto.DecodePayload[relayproto.SelectResult](response)
	if err != nil {
		return err
	}
	return mapSelectResult(result)
}

// Execute 向目标机器转发一条用户输入并等待首段回复。
func (hub *ClientHub) Execute(ctx context.Context, userID string, target relayproto.SessionRef, message im.IncomingText) (string, error) {
	if err := relayproto.ValidateSessionRef(target); err != nil {
		return "", ErrTargetChanged
	}
	connection := hub.readyClient(ClientKey{UserID: userID, MachineID: target.MachineID})
	if connection == nil {
		return "", ErrClientUnavailable
	}
	frame, err := relayproto.NewFrame(relayproto.TypeExecuteRequest, randomHubID(), relayproto.ExecuteRequest{
		Target: target, MessageID: message.MessageID, UserID: userID, Content: message.Content,
	})
	if err != nil {
		return "", err
	}
	response, err := connection.request(ctx, frame, relayproto.TypeExecuteResponse)
	if err != nil {
		return "", err
	}
	result, err := relayproto.DecodePayload[relayproto.ExecuteResponse](response)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func (hub *ClientHub) serveConnection(parent context.Context, socket *websocket.Conn) {
	helloContext, cancelHello := context.WithTimeout(parent, hub.config.HelloTimeout)
	helloFrame, err := connectionReadFrame(helloContext, socket)
	cancelHello()
	if err != nil || helloFrame.Type != relayproto.TypeClientHello {
		hub.writeProtocolError(socket, relayproto.CodeInvalidFrame, "首帧必须是 client_hello", true)
		_ = socket.Close(websocket.StatusPolicyViolation, "invalid hello")
		return
	}
	hello, err := relayproto.DecodePayload[relayproto.ClientHello](helloFrame)
	if err != nil || relayproto.ValidateClientHello(hello) != nil {
		hub.writeProtocolError(socket, relayproto.CodeInvalidIdentity, "客户端身份无效", true)
		_ = socket.Close(websocket.StatusPolicyViolation, "invalid identity")
		return
	}
	key := ClientKey{UserID: hello.UserID, MachineID: hello.MachineID}
	connectionID := randomHubID()
	if _, err := hub.catalog.Attach(connectionID, key); err != nil {
		code := relayproto.CodeInvalidIdentity
		if errors.Is(err, ErrDuplicateClient) {
			code = relayproto.CodeDuplicateClient
		}
		hub.writeProtocolError(socket, code, "客户端连接被拒绝", true)
		_ = socket.Close(websocket.StatusPolicyViolation, "client rejected")
		return
	}
	connection := newClientConnection(parent, connectionID, key, socket, hub.config.SendQueueCapacity, hub.config.MaxInflight)
	hub.install(connection)
	go connection.runWriter()
	defer connection.close(websocket.StatusNormalClosure, "connection ended")
	defer hub.remove(connection)

	serverHello, _ := relayproto.NewFrame(relayproto.TypeServerHello, helloFrame.RequestID, relayproto.ServerHello{
		ConnectionID:          connectionID,
		HeartbeatIntervalSecs: durationSeconds(hub.config.HeartbeatInterval),
		HeartbeatTimeoutSecs:  durationSeconds(hub.config.HeartbeatTimeout),
		SnapshotIntervalSecs:  durationSeconds(hub.config.SnapshotInterval),
	})
	if err := connection.send(parent, serverHello); err != nil {
		return
	}
	firstContext, cancelFirst := context.WithTimeout(connection.ctx, hub.config.FirstSnapshotTimeout)
	firstFrame, err := connectionReadFrame(firstContext, socket)
	cancelFirst()
	if err != nil || firstFrame.Type != relayproto.TypeSessionSnapshot {
		return
	}
	firstSnapshot, err := relayproto.DecodePayload[relayproto.SessionSnapshot](firstFrame)
	if err != nil || hub.catalog.ApplySnapshot(connectionID, firstSnapshot) != nil {
		return
	}
	connection.ready.Store(true)

	heartbeatDone := make(chan struct{})
	go hub.runOutbound(connection)
	go func() {
		defer close(heartbeatDone)
		hub.runHeartbeat(connection)
	}()
	hub.readLoop(connection)
	connection.cancel()
	<-heartbeatDone
	<-connection.outboundDone
}

func (hub *ClientHub) readLoop(connection *clientConnection) {
	for {
		frame, err := connectionReadFrame(connection.ctx, connection.socket)
		if err != nil {
			if !isNormalSocketEnd(err) {
				hub.logger.Warn("Relay 客户端连接结束", "error_type", "read")
			}
			return
		}
		switch frame.Type {
		case relayproto.TypeSessionSnapshot:
			snapshot, err := relayproto.DecodePayload[relayproto.SessionSnapshot](frame)
			if err != nil || hub.catalog.ApplySnapshot(connection.id, snapshot) != nil {
				return
			}
		case relayproto.TypeSelectResult, relayproto.TypeExecuteResponse:
			connection.deliver(frame)
		case relayproto.TypePong:
			if _, err := relayproto.DecodePayload[relayproto.Heartbeat](frame); err != nil {
				return
			}
			connection.lastPong.Store(time.Now().UnixNano())
		case relayproto.TypePing:
			heartbeat, err := relayproto.DecodePayload[relayproto.Heartbeat](frame)
			if err != nil {
				return
			}
			pong, _ := relayproto.NewFrame(relayproto.TypePong, frame.RequestID, heartbeat)
			if err := connection.send(connection.ctx, pong); err != nil {
				return
			}
		case relayproto.TypeExecutePush, relayproto.TypeNotification:
			select {
			case connection.outboundQueue <- frame:
			case <-connection.ctx.Done():
				return
			default:
				return
			}
		default:
			return
		}
	}
}

func (hub *ClientHub) runOutbound(connection *clientConnection) {
	defer close(connection.outboundDone)
	for {
		select {
		case <-connection.ctx.Done():
			return
		case frame := <-connection.outboundQueue:
			hub.outboundMu.RLock()
			sink := hub.outbound
			hub.outboundMu.RUnlock()
			if sink == nil {
				continue
			}
			var err error
			switch frame.Type {
			case relayproto.TypeExecutePush:
				var push relayproto.ExecutePush
				push, err = relayproto.DecodePayload[relayproto.ExecutePush](frame)
				if err == nil {
					err = sink.SendPush(connection.ctx, connection.key.UserID, push.Content)
				}
			case relayproto.TypeNotification:
				var notification relayproto.Notification
				notification, err = relayproto.DecodePayload[relayproto.Notification](frame)
				if err == nil && notification.Target.MachineID != connection.key.MachineID {
					err = ErrTargetChanged
				}
				if err == nil {
					err = sink.SendNotification(connection.ctx, connection.key.UserID, connection.key.MachineID, notification)
				}
			}
			if err != nil {
				hub.logger.Warn("Relay 出站消息发送失败", "error_type", "outbound")
			}
		}
	}
}

func (hub *ClientHub) runHeartbeat(connection *clientConnection) {
	ticker := time.NewTicker(hub.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-connection.ctx.Done():
			return
		case now := <-ticker.C:
			lastPong := time.Unix(0, connection.lastPong.Load())
			if now.Sub(lastPong) >= hub.config.HeartbeatTimeout {
				connection.cancel()
				return
			}
			ping, _ := relayproto.NewFrame(relayproto.TypePing, randomHubID(), relayproto.Heartbeat{Nonce: randomHubID()})
			if err := connection.send(connection.ctx, ping); err != nil {
				connection.cancel()
				return
			}
		}
	}
}

func (hub *ClientHub) readyClient(key ClientKey) *clientConnection {
	if hub == nil {
		return nil
	}
	hub.mu.RLock()
	connection := hub.clients[key]
	hub.mu.RUnlock()
	if connection == nil || !connection.ready.Load() || connection.ctx.Err() != nil {
		return nil
	}
	return connection
}

func (hub *ClientHub) install(connection *clientConnection) {
	hub.mu.Lock()
	hub.clients[connection.key] = connection
	hub.mu.Unlock()
}

func (hub *ClientHub) remove(connection *clientConnection) {
	hub.mu.Lock()
	if hub.clients[connection.key] == connection {
		delete(hub.clients, connection.key)
	}
	hub.mu.Unlock()
	hub.catalog.Detach(connection.id)
}

func (hub *ClientHub) writeProtocolError(socket *websocket.Conn, code relayproto.ErrorCode, message string, closeConnection bool) {
	frame, err := relayproto.NewFrame(relayproto.TypeProtocolError, "", relayproto.NewProtocolError(code, message, closeConnection).Payload())
	if err != nil {
		return
	}
	encoded, err := relayproto.Encode(frame)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = socket.Write(ctx, websocket.MessageText, encoded)
}

func normalizedHubConfig(config HubConfig) HubConfig {
	if config.HelloTimeout <= 0 {
		config.HelloTimeout = defaultHelloTimeout
	}
	if config.FirstSnapshotTimeout <= 0 {
		config.FirstSnapshotTimeout = defaultFirstSnapshotTimeout
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = defaultHubHeartbeatInterval
	}
	if config.HeartbeatTimeout <= 0 {
		config.HeartbeatTimeout = defaultHubHeartbeatTimeout
	}
	if config.SnapshotInterval <= 0 {
		config.SnapshotInterval = defaultSnapshotInterval
	}
	if config.SendQueueCapacity <= 0 {
		config.SendQueueCapacity = defaultHubSendCapacity
	}
	if config.MaxInflight <= 0 {
		config.MaxInflight = defaultHubMaxInflight
	}
	return config
}

func durationSeconds(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func randomHubID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
}
