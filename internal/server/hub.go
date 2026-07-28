package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
)

var (
	ErrInvalidHubDependency = errors.New("ClientHub 依赖无效")
	ErrClientUnavailable    = errors.New("Relay 客户端不可用")
	ErrClientQueueFull      = errors.New("Relay 客户端发送队列已满")
	ErrInflightFull         = errors.New("Relay 客户端在途请求已满")
	ErrInvalidHubRequest    = errors.New("Relay Hub 请求无效")
)

const (
	defaultHelloTimeout               = 10 * time.Second
	defaultFirstSnapshotTimeout       = 10 * time.Second
	defaultHubHeartbeatInterval       = 20 * time.Second
	defaultHubHeartbeatTimeout        = 60 * time.Second
	defaultSnapshotInterval           = 30 * time.Second
	defaultHubSendCapacity            = 128
	defaultHubMaxInflight             = 128
	defaultCommandRouteTTL            = 10 * time.Minute
	defaultCommandRouteCapacity       = 4096
	defaultNotificationDedupeCapacity = 4096
	defaultNotificationDedupeTTL      = 10 * time.Minute
	defaultIdempotencyWindow          = 10 * time.Minute
)

var serverCapabilities = []string{hprp.CapabilityCommandOutputV1}

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

// ClientHub 接受 HPRP/1 WSS 连接并把请求路由到当前在线机器。
type ClientHub struct {
	catalog  *SessionCatalog
	verifier credential.Verifier
	config   HubConfig
	logger   *slog.Logger

	mu      sync.RWMutex
	clients map[ClientKey]*clientConnection

	outboundMu sync.RWMutex
	outbound   HubOutboundSink
}

// HubOutboundSink 接收 Pal 主动发送的命令输出和通知事件。
type HubOutboundSink interface {
	SendCommandOutput(ctx context.Context, userID string, output hprp.CommandOutput) error
	SendNotification(ctx context.Context, userID, machineID string, notification hprp.NotificationEvent) error
}

// NewClientHub 创建需要 Bearer 凭据验证的 HPRP/1 连接中心。
func NewClientHub(catalog *SessionCatalog, verifier credential.Verifier, config HubConfig, logger *slog.Logger) (*ClientHub, error) {
	if catalog == nil || verifier == nil || logger == nil {
		return nil, ErrInvalidHubDependency
	}
	config = normalizedHubConfig(config)
	return &ClientHub{catalog: catalog, verifier: verifier, config: config, logger: logger, clients: make(map[ClientKey]*clientConnection)}, nil
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

// ServeHTTP 在 Upgrade 前完成 TLS、子协议、Bearer Key 和机器唯一连接校验。
func (hub *ClientHub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if hub == nil || request.TLS == nil {
		if hub != nil {
			hub.logger.Warn("HPRP 连接被拒绝", "stage", "tls_upgrade", "error_type", "tls_required", "reason", "HPRP/1 只接受 TLS 连接")
		}
		http.Error(writer, "HPRP requires TLS", http.StatusUpgradeRequired)
		return
	}
	if !requestOffersSubprotocol(request, hprp.Subprotocol) {
		hub.logger.Warn("HPRP 连接被拒绝", "stage", "subprotocol", "error_type", "subprotocol_required", "reason", "客户端未声明 HPRP/1 WebSocket 子协议")
		http.Error(writer, "HPRP subprotocol required", http.StatusUpgradeRequired)
		return
	}
	identity, err := credential.VerifyRequest(request, hub.verifier)
	if err != nil {
		args := []any{"stage", "authorization", "error_type", "unauthenticated", "reason", "Bearer Key 无效、禁用、过期或来源不符"}
		parts := strings.Fields(request.Header.Get("Authorization"))
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if credentialID, parseErr := credential.BearerCredentialID(parts[1]); parseErr == nil {
				args = append(args, "credential_id", credentialID)
			}
		}
		if source, sourceErr := credential.RequestSourceAddr(request); sourceErr == nil {
			args = append(args, "source_ip", source.String())
		}
		hub.logger.Warn("HPRP 连接认证失败", args...)
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "Unauthorized", http.StatusUnauthorized)
		return
	}
	key := ClientKey{UserID: identity.PrincipalID, MachineID: identity.MachineID}
	connectionID := randomHubID()
	if _, err := hub.catalog.Attach(connectionID, key); err != nil {
		args := []any{"stage", "reserve_identity", "credential_id", identity.CredentialID, "user_hash", routerHash(key.UserID), "machine_id", safeLogValue(key.MachineID)}
		hub.logger.Warn("HPRP 客户端连接被拒绝", append(args, serverErrorLogArgs(err)...)...)
		status := http.StatusConflict
		if !errors.Is(err, ErrDuplicateClient) {
			status = http.StatusForbidden
		}
		http.Error(writer, http.StatusText(status), status)
		return
	}
	reserved := true
	defer func() {
		if reserved {
			hub.catalog.Detach(connectionID)
		}
	}()

	socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{hprp.Subprotocol}})
	if err != nil {
		hub.logger.Warn("HPRP WebSocket 升级失败", append([]any{"stage", "websocket_upgrade", "credential_id", identity.CredentialID}, serverErrorLogArgs(err)...)...)
		return
	}
	if socket.Subprotocol() != hprp.Subprotocol {
		_ = socket.Close(websocket.StatusPolicyViolation, "HPRP subprotocol required")
		return
	}
	socket.SetReadLimit(hprp.MaxMessageBytes)
	reserved = false
	source, _ := credential.RequestSourceAddr(request)
	hub.serveConnection(request.Context(), socket, connectionID, identity, key, source)
}

// Select 复核目标仍存在于已就绪连接；HPRP/1 不发送远端选择消息。
func (hub *ClientHub) Select(_ context.Context, userID string, target hprp.Target) error {
	if hprp.ValidateTarget(target) != nil || target.MachineID == "" {
		return ErrTargetChanged
	}
	if hub.readyClient(ClientKey{UserID: userID, MachineID: target.MachineID}) == nil {
		return ErrClientUnavailable
	}
	_, err := hub.catalog.ResolveTarget(userID, target)
	return err
}

// Execute 发送 HPRP command.execute 并等待唯一 command.result。
func (hub *ClientHub) Execute(ctx context.Context, userID string, target hprp.Target, message im.IncomingText) (RelayExecution, error) {
	if hprp.ValidateTarget(target) != nil || strings.TrimSpace(message.MessageID) == "" {
		return RelayExecution{}, ErrTargetChanged
	}
	connection := hub.readyClient(ClientKey{UserID: userID, MachineID: target.MachineID})
	if connection == nil {
		return RelayExecution{}, ErrClientUnavailable
	}
	if _, err := hub.catalog.ResolveTarget(userID, target); err != nil {
		return RelayExecution{}, err
	}
	commandID := randomHubID()
	if err := connection.registerCommand(commandID, target, defaultCommandRouteTTL); err != nil {
		return RelayExecution{}, err
	}
	keepRoute := false
	defer func() {
		if !keepRoute {
			connection.removeCommand(commandID)
		}
	}()
	command := hprp.CommandExecute{
		IdempotencyKey: message.MessageID,
		Target:         target,
		Content:        hprp.TextContent{Type: hprp.ContentTypeText, Text: message.Content},
	}
	if err := hprp.ValidateCommandExecute(command); err != nil {
		return RelayExecution{}, err
	}
	envelope, err := hprp.NewEnvelope(hprp.TypeCommandExecute, commandID, "", true, command)
	if err != nil {
		return RelayExecution{}, err
	}
	response, err := connection.request(ctx, envelope, hprp.TypeCommandResult)
	if err != nil {
		return RelayExecution{}, err
	}
	result, err := hprp.DecodePayload[hprp.CommandResult](response)
	if err == nil {
		err = hprp.ValidateCommandResult(result)
	}
	if err != nil {
		return RelayExecution{}, err
	}
	if result.ReplacementTarget != nil && (result.ReplacementTarget.MachineID != target.MachineID || result.ReplacementTarget.SlotID != target.SlotID) {
		return RelayExecution{}, ErrTargetChanged
	}
	if result.Outcome != hprp.OutcomeOK {
		return RelayExecution{}, mapCommandFailure(result)
	}
	keepRoute = true
	content := ""
	if result.Content != nil {
		content = result.Content.Text
	}
	return RelayExecution{Content: content, SelectedTarget: result.ReplacementTarget}, nil
}

func (hub *ClientHub) serveConnection(parent context.Context, socket *websocket.Conn, connectionID string, identity credential.Identity, key ClientKey, source netip.Addr) {
	helloContext, cancelHello := context.WithTimeout(parent, hub.config.HelloTimeout)
	helloEnvelope, err := connectionReadEnvelope(helloContext, socket)
	cancelHello()
	if err == nil && (helloEnvelope.Type != hprp.TypeHelloClient || helloEnvelope.ReplyTo != "") {
		err = fmt.Errorf("%w: 首条消息必须是 hello.client", hprp.ErrInvalidMessage)
	}
	var hello hprp.ClientHello
	if err == nil {
		hello, err = hprp.DecodePayload[hprp.ClientHello](helloEnvelope)
	}
	if err == nil {
		err = hprp.ValidateClientHello(hello)
	}
	if err != nil {
		hub.logger.Warn("HPRP 客户端握手失败", append([]any{"stage", "hello.client", "credential_id", identity.CredentialID, "user_hash", routerHash(key.UserID), "machine_id", safeLogValue(key.MachineID)}, serverErrorLogArgs(err)...)...)
		hub.writeProtocolError(socket, helloEnvelope.ID, hprp.CodeProtocolInvalidMessage, "hello.client 无效", true)
		_ = socket.Close(websocket.StatusPolicyViolation, "invalid hello")
		hub.catalog.Detach(connectionID)
		return
	}

	connection := newClientConnection(parent, clientConnectionConfig{
		ID: connectionID, CredentialID: identity.CredentialID, Key: key, Source: source,
		Implementation: hello.Implementation, Socket: socket, SendCapacity: hub.config.SendQueueCapacity,
		MaxPending: minInt(hub.config.MaxInflight, hello.Limits.MaxInflightCommands), Logger: hub.logger,
	})
	hub.install(connection)
	go connection.runWriter()
	defer connection.close(websocket.StatusNormalClosure, "connection ended")
	defer hub.remove(connection)

	serverHello := hub.negotiateHello(connectionID, key.MachineID, hello)
	connection.setCapabilities(serverHello.Capabilities)
	helloResponse, _ := hprp.NewEnvelope(hprp.TypeHelloServer, randomHubID(), helloEnvelope.ID, false, serverHello)
	if err := connection.send(parent, helloResponse); err != nil {
		hub.logger.Warn("HPRP 服务端握手响应失败", append(append(connectionLogArgs(connection), "stage", "hello.server"), serverErrorLogArgs(err)...)...)
		return
	}

	firstContext, cancelFirst := context.WithTimeout(connection.ctx, hub.config.FirstSnapshotTimeout)
	firstEnvelope, err := connectionReadEnvelope(firstContext, socket)
	cancelFirst()
	if err == nil && (firstEnvelope.Type != hprp.TypeSessionSnapshot || firstEnvelope.ReplyTo != "") {
		err = fmt.Errorf("%w: hello 后必须发送完整 session.snapshot", hprp.ErrInvalidMessage)
	}
	if err != nil {
		hub.logger.Warn("HPRP 客户端首次快照失败", append(append(connectionLogArgs(connection), "stage", "first_snapshot"), serverErrorLogArgs(err)...)...)
		return
	}
	firstSnapshot, err := hub.applySnapshot(connection, firstEnvelope)
	if err != nil {
		hub.logger.Warn("HPRP 客户端首次快照失败", append(append(connectionLogArgs(connection), "stage", "first_snapshot"), serverErrorLogArgs(err)...)...)
		return
	}
	connection.ready.Store(true)
	hub.logger.Info("HPRP 客户端已就绪",
		"credential_id", identity.CredentialID, "user_hash", routerHash(key.UserID), "machine_id", safeLogValue(key.MachineID),
		"connection_id", connectionID, "client_name", safeLogValue(hello.Implementation.Name), "client_version", safeLogValue(hello.Implementation.Version),
		"snapshot_sequence", firstSnapshot.Sequence, "session_count", len(firstSnapshot.Sessions))

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

func (hub *ClientHub) negotiateHello(connectionID, machineID string, hello hprp.ClientHello) hprp.ServerHello {
	maxMessageBytes := minInt(hprp.MaxMessageBytes, hello.Limits.MaxReceiveMessageBytes)
	maxInflight := minInt(hub.config.MaxInflight, hello.Limits.MaxInflightCommands)
	return hprp.ServerHello{
		ConnectionID: connectionID,
		MachineID:    machineID,
		Capabilities: hprp.SelectHighestCommonVersions(hello.Capabilities, serverCapabilities),
		Features:     hprp.NegotiateFeatures(hello.Features, map[string]hprp.FeatureOffer{}),
		Limits: hprp.ServerLimits{
			MaxMessageBytes: maxMessageBytes, MaxSessions: hprp.MaxSessions, MaxInflightCommands: maxInflight,
			MaxInflightFeatures: 0, MaxOutputBytes: hprp.MaxContentBytes, IdempotencyWindowMS: defaultIdempotencyWindow.Milliseconds(),
		},
		Heartbeat: hprp.HeartbeatConfig{PingIntervalMS: hub.config.HeartbeatInterval.Milliseconds(), IdleTimeoutMS: hub.config.HeartbeatTimeout.Milliseconds()},
	}
}

func (hub *ClientHub) applySnapshot(connection *clientConnection, envelope hprp.Envelope) (hprp.SessionSnapshot, error) {
	snapshot, err := hprp.DecodePayload[hprp.SessionSnapshot](envelope)
	if err == nil {
		err = hub.catalog.ApplySnapshot(connection.id, snapshot)
		if err == nil {
			connection.recordSnapshot(snapshot.Sequence, len(snapshot.Sessions), time.Now())
		}
	}
	result := hprp.SnapshotResult{Outcome: hprp.OutcomeOK, AppliedSequence: snapshot.Sequence}
	if err != nil {
		result = hprp.SnapshotResult{Outcome: hprp.OutcomeRejected, Error: &hprp.Error{Code: snapshotErrorCode(err), Message: "会话快照未应用", Retryable: false}}
	}
	response, buildErr := hprp.NewEnvelope(hprp.TypeSessionSnapshotResult, randomHubID(), envelope.ID, false, result)
	if buildErr == nil {
		buildErr = connection.send(connection.ctx, response)
	}
	if buildErr != nil {
		return snapshot, buildErr
	}
	return snapshot, err
}

func (hub *ClientHub) readLoop(connection *clientConnection) {
	for {
		envelope, err := connectionReadEnvelope(connection.ctx, connection.socket)
		if err != nil {
			if !isNormalSocketEnd(err) {
				hub.logger.Warn("HPRP 客户端连接结束", append(append(connectionLogArgs(connection), "stage", "read_message"), serverErrorLogArgs(err)...)...)
			} else {
				hub.logger.Debug("HPRP 客户端正常断开", connectionLogArgs(connection)...)
			}
			return
		}
		switch envelope.Type {
		case hprp.TypeSessionSnapshot:
			previous := connection.sessionCount.Load()
			snapshot, err := hub.applySnapshot(connection, envelope)
			if err != nil && !errors.Is(err, ErrSnapshotStale) {
				hub.logger.Warn("HPRP 客户端快照上报失败", append(append(connectionLogArgs(connection), "stage", "session.snapshot", "snapshot_sequence", snapshot.Sequence, "session_count", len(snapshot.Sessions)), serverErrorLogArgs(err)...)...)
				return
			}
			hub.logger.Debug("HPRP 客户端快照已处理", append(connectionLogArgs(connection), "snapshot_sequence", snapshot.Sequence, "previous_session_count", previous, "session_count", len(snapshot.Sessions), "applied", err == nil)...)
		case hprp.TypeCommandResult:
			result, resultErr := hprp.DecodePayload[hprp.CommandResult](envelope)
			if resultErr == nil {
				resultErr = hprp.ValidateCommandResult(result)
			}
			if resultErr == nil && result.ReplacementTarget != nil {
				if result.ReplacementTarget.MachineID != connection.key.MachineID {
					resultErr = ErrTargetChanged
				} else {
					_, resultErr = hub.catalog.ResolveTarget(connection.key.UserID, *result.ReplacementTarget)
				}
				if resultErr == nil {
					resultErr = connection.replaceCommandTarget(envelope.ReplyTo, *result.ReplacementTarget)
				}
			}
			if resultErr != nil {
				hub.logger.Warn("HPRP 命令结果无效", append(append(connectionLogArgs(connection), "request_hash", routerHash(envelope.ReplyTo)), serverErrorLogArgs(resultErr)...)...)
				return
			}
			if connection.deliver(envelope) {
				hub.logger.Debug("HPRP 命令结果已接收", append(connectionLogArgs(connection), "request_hash", routerHash(envelope.ReplyTo))...)
			} else {
				hub.logger.Warn("HPRP 命令结果未匹配", append(connectionLogArgs(connection), "request_hash", routerHash(envelope.ReplyTo), "error_type", "unmatched_request", "reason", "结果没有对应的在途 command.execute")...)
			}
		case hprp.TypeCommandOutput:
			output, err := hprp.DecodePayload[hprp.CommandOutput](envelope)
			if err == nil {
				err = hprp.ValidateCommandOutput(output)
			}
			accepted := false
			if err == nil {
				if output.Target.MachineID != connection.key.MachineID {
					err = ErrTargetChanged
				} else {
					_, err = hub.catalog.ResolveTarget(connection.key.UserID, output.Target)
				}
			}
			if err == nil {
				accepted, err = connection.acceptCommandOutput(envelope, output)
			}
			if err != nil {
				hub.logger.Warn("HPRP 命令输出无效", append(append(connectionLogArgs(connection), targetLogArgs(output.Target)...), serverErrorLogArgs(err)...)...)
				return
			}
			if !accepted {
				hub.logger.Debug("HPRP 重复命令输出已忽略", append(connectionLogArgs(connection), "request_hash", routerHash(envelope.ReplyTo), "sequence", output.Sequence)...)
				continue
			}
			if !hub.enqueueOutbound(connection, envelope) {
				return
			}
		case hprp.TypeNotificationEvent:
			notification, err := hprp.DecodePayload[hprp.NotificationEvent](envelope)
			if err == nil {
				err = hprp.ValidateNotificationEvent(notification)
			}
			if err == nil {
				if notification.Target.MachineID != connection.key.MachineID {
					err = ErrTargetChanged
				} else {
					_, err = hub.catalog.ResolveTarget(connection.key.UserID, notification.Target)
				}
			}
			if err != nil {
				hub.logger.Warn("HPRP 通知事件无效", append(append(connectionLogArgs(connection), targetLogArgs(notification.Target)...), serverErrorLogArgs(err)...)...)
				return
			}
			if !connection.acceptNotification(notification) {
				hub.logger.Debug("HPRP 重复或乱序通知已忽略", append(connectionLogArgs(connection), "event_key_hash", routerHash(notification.EventKey), "sequence", notification.Sequence)...)
				continue
			}
			if !hub.enqueueOutbound(connection, envelope) {
				return
			}
		default:
			if envelope.MustUnderstand {
				hub.logger.Warn("HPRP 客户端发送了不支持的必需消息", append(connectionLogArgs(connection), "event_type", envelope.Type, "error_type", "unsupported_required_type", "reason", "must_understand 消息类型不受支持")...)
				hub.writeProtocolError(connection.socket, envelope.ID, hprp.CodeProtocolUnsupportedType, "消息类型不受支持", true)
				return
			}
			hub.logger.Debug("HPRP 未知可选消息已忽略", append(connectionLogArgs(connection), "event_type", envelope.Type)...)
		}
	}
}

func (hub *ClientHub) enqueueOutbound(connection *clientConnection, envelope hprp.Envelope) bool {
	select {
	case connection.outboundQueue <- envelope:
		hub.logger.Debug("HPRP 客户端上报已接收", append(connectionLogArgs(connection), "event_type", envelope.Type, "message_hash", routerHash(envelope.ID))...)
		return true
	case <-connection.ctx.Done():
		return false
	default:
		hub.logger.Warn("HPRP 客户端上报队列已满", append(connectionLogArgs(connection), "event_type", envelope.Type, "error_type", "outbound_queue_full", "reason", "服务端出站处理队列已满，连接将断开以避免静默丢失")...)
		return false
	}
}

func (hub *ClientHub) runOutbound(connection *clientConnection) {
	defer close(connection.outboundDone)
	for {
		select {
		case <-connection.ctx.Done():
			return
		case envelope := <-connection.outboundQueue:
			hub.outboundMu.RLock()
			sink := hub.outbound
			hub.outboundMu.RUnlock()
			if sink == nil {
				hub.logger.Warn("HPRP 出站消息无法发送", append(connectionLogArgs(connection), "event_type", envelope.Type, "error_type", "outbound_sink_unavailable", "reason", "企业微信出站接收器尚未配置")...)
				continue
			}
			var err error
			var target hprp.Target
			switch envelope.Type {
			case hprp.TypeCommandOutput:
				var output hprp.CommandOutput
				output, err = hprp.DecodePayload[hprp.CommandOutput](envelope)
				target = output.Target
				if err == nil {
					err = sink.SendCommandOutput(connection.ctx, connection.key.UserID, output)
				}
			case hprp.TypeNotificationEvent:
				var notification hprp.NotificationEvent
				notification, err = hprp.DecodePayload[hprp.NotificationEvent](envelope)
				target = notification.Target
				if err == nil {
					err = sink.SendNotification(connection.ctx, connection.key.UserID, connection.key.MachineID, notification)
				}
			}
			args := append(connectionLogArgs(connection), "event_type", envelope.Type)
			args = append(args, targetLogArgs(target)...)
			if err != nil {
				hub.logger.Warn("HPRP 出站消息发送失败", append(args, serverErrorLogArgs(err)...)...)
			} else {
				hub.logger.Debug("HPRP 出站消息发送成功", args...)
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
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(connection.ctx, hub.config.HeartbeatTimeout)
			err := connection.socket.Ping(ctx)
			cancel()
			if err != nil {
				hub.logger.Warn("HPRP WebSocket 心跳失败", append(append(connectionLogArgs(connection), "stage", "websocket_ping"), serverErrorLogArgs(err)...)...)
				connection.cancel()
				return
			}
			connection.recordHeartbeat(time.Now())
			hub.logger.Debug("HPRP WebSocket 心跳成功", connectionLogArgs(connection)...)
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
	hub.logger.Info("HPRP 客户端已移除", "credential_id", connection.credentialID, "user_hash", routerHash(connection.key.UserID), "machine_id", safeLogValue(connection.key.MachineID), "connection_id", connection.id, "reason", "连接生命周期结束，机器及会话已从在线目录移除")
}

func (hub *ClientHub) writeProtocolError(socket *websocket.Conn, replyTo string, code hprp.ErrorCode, message string, closeConnection bool) {
	envelope, err := hprp.NewEnvelope(hprp.TypeProtocolError, randomHubID(), replyTo, false, hprp.ProtocolError{
		Error: hprp.Error{Code: code, Message: message, Retryable: false}, Close: closeConnection,
	})
	if err != nil {
		return
	}
	encoded, err := hprp.Encode(envelope)
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

func requestOffersSubprotocol(request *http.Request, required string) bool {
	for _, value := range request.Header.Values("Sec-WebSocket-Protocol") {
		for _, candidate := range strings.Split(value, ",") {
			if strings.TrimSpace(candidate) == required {
				return true
			}
		}
	}
	return false
}

func mapCommandFailure(result hprp.CommandResult) error {
	if result.Error != nil {
		switch result.Error.Code {
		case hprp.CodeTargetNotFound, hprp.CodeTargetSessionChanged, hprp.CodeTargetNotReady:
			return ErrTargetChanged
		case hprp.CodeCommandTimeout:
			return context.DeadlineExceeded
		case hprp.CodeServerBusy:
			return ErrClientQueueFull
		}
	}
	return ErrClientUnavailable
}

func snapshotErrorCode(err error) hprp.ErrorCode {
	if errors.Is(err, ErrSnapshotStale) {
		return hprp.CodeSyncStaleSnapshot
	}
	return hprp.CodeProtocolInvalidMessage
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func randomHubID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
}
