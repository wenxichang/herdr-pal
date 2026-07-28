package relayclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
	"github.com/wenxichang/herdr-pal/internal/session"
)

var (
	// ErrInvalidConfig 表示 Relay 客户端配置无效。
	ErrInvalidConfig = errors.New("Relay 客户端配置无效")
	// ErrUnavailable 表示当前没有可写的 Relay 连接。
	ErrUnavailable = errors.New("Relay 连接不可用")
	// ErrInvalidExecutor 表示本地执行器缺失或重复设置。
	ErrInvalidExecutor = errors.New("Relay 本地执行器无效")
)

type relayStageError struct {
	stage string
	err   error
}

func (err *relayStageError) Error() string { return err.err.Error() }

func (err *relayStageError) Unwrap() error { return err.err }

func withRelayStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var staged *relayStageError
	if errors.As(err, &staged) {
		return err
	}
	return &relayStageError{stage: stage, err: err}
}

const defaultPollInterval = 250 * time.Millisecond

// Config 是 Relay 客户端连接和快照上报配置。
type Config struct {
	URL              string
	UserID           string
	MachineID        string
	SkipVerify       bool
	Version          string
	PollInterval     time.Duration
	SnapshotInterval time.Duration
	BackoffMin       time.Duration
	BackoffMax       time.Duration
	Logger           *slog.Logger
}

// Executor 是 Relay 请求所需的本地 Bridge 能力。
type Executor interface {
	CurrentTargets() []session.Target
	SelectedTarget() (session.Target, error)
	SelectTarget(paneID, occupantHash string) error
	HandleMessage(ctx context.Context, message im.IncomingText)
}

// Client 维护 Relay WSS、执行服务端请求并实现 Bridge 消息 sink。
type Client struct {
	config Config
	logger *slog.Logger

	executorMu sync.RWMutex
	executor   Executor
	runStarted bool

	currentMu sync.RWMutex
	current   *clientSession

	executionMu     sync.Mutex
	activeRequestID string
	activeTarget    relayproto.SessionRef
}

type clientSession struct {
	ctx              context.Context
	cancel           context.CancelFunc
	connection       *websocket.Conn
	snapshotRequests chan chan error
	writeMu          sync.Mutex
}

// New 创建尚未启动的 Relay 客户端；Run 前必须设置 Executor。
func New(config Config) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return nil, fmt.Errorf("%w: URL 必须是 wss://", ErrInvalidConfig)
	}
	if err := relayproto.ValidateClientHello(relayproto.ClientHello{UserID: config.UserID, MachineID: config.MachineID, ClientVersion: config.Version}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.SnapshotInterval <= 0 {
		config.SnapshotInterval = 30 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	config.URL = endpoint.String()
	return &Client{config: config, logger: config.Logger}, nil
}

// SetExecutor 设置本地 Bridge 执行器；只能在 Run 前成功调用一次。
func (client *Client) SetExecutor(executor Executor) error {
	if client == nil || executor == nil {
		return ErrInvalidExecutor
	}
	client.executorMu.Lock()
	defer client.executorMu.Unlock()
	if client.executor != nil || client.runStarted {
		return ErrInvalidExecutor
	}
	client.executor = executor
	return nil
}

// Run 持续维护 Relay 连接，直到 context 取消。
func (client *Client) Run(ctx context.Context) error {
	if client == nil || ctx == nil {
		return ErrInvalidConfig
	}
	client.executorMu.Lock()
	if client.executor == nil || client.runStarted {
		client.executorMu.Unlock()
		return ErrInvalidExecutor
	}
	client.runStarted = true
	client.executorMu.Unlock()
	backoff := newReconnectBackoff(client.config.BackoffMin, client.config.BackoffMax)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		client.logger.Info("Relay 连接中", "machine_id", client.config.MachineID, "endpoint", relayLogEndpoint(client.config.URL), "skip_verify", client.config.SkipVerify)
		err := client.runSession(ctx, backoff.Reset)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := backoff.Next()
		args := []any{"machine_id", client.config.MachineID, "endpoint", relayLogEndpoint(client.config.URL)}
		args = append(args, relayErrorLogArgs(err, client.config.URL, client.config.UserID)...)
		args = append(args, "retry_delay", delay)
		client.logger.Warn("Relay 连接已断开", args...)
		if err := waitReconnect(ctx, delay); err != nil {
			return err
		}
	}
}

// RespondMarkdown 把 Bridge 首段回复写成 execute_response。
func (client *Client) RespondMarkdown(ctx context.Context, requestID, content string) error {
	response := relayproto.ExecuteResponse{Content: content}
	client.executionMu.Lock()
	activeRequestID := client.activeRequestID
	activeTarget := client.activeTarget
	client.executionMu.Unlock()
	if activeRequestID == requestID {
		if selected, err := client.selectedSessionRef(); err == nil &&
			selected.MachineID == activeTarget.MachineID && selected.PaneID == activeTarget.PaneID &&
			selected.OccupantHash != activeTarget.OccupantHash {
			response.SelectedTarget = &selected
			client.executionMu.Lock()
			if client.activeRequestID == requestID && client.activeTarget == activeTarget {
				client.activeTarget = selected
			}
			client.executionMu.Unlock()
			if err := client.reportCurrentSnapshot(ctx); err != nil {
				return err
			}
		}
	}
	return client.writeCurrent(ctx, relayproto.TypeExecuteResponse, requestID, response)
}

// SendMarkdown 把当前执行产生的后续分段写成 execute_push。
func (client *Client) SendMarkdown(ctx context.Context, content string) error {
	client.executionMu.Lock()
	requestID := client.activeRequestID
	target := client.activeTarget
	client.executionMu.Unlock()
	if requestID == "" {
		return ErrUnavailable
	}
	return client.writeCurrent(ctx, relayproto.TypeExecutePush, requestID, relayproto.ExecutePush{Target: target, Content: content})
}

// SendNotification 发送携带稳定本机目标的主动通知；断线时直接失败且不缓存。
func (client *Client) SendNotification(ctx context.Context, target im.NotificationTarget, content string) error {
	localIndex := 0
	for index, current := range client.localExecutor().CurrentTargets() {
		if current.PaneID == target.PaneID && current.OccupantKey == target.OccupantHash {
			localIndex = index + 1
			break
		}
	}
	if localIndex == 0 {
		return session.ErrListSnapshotExpired
	}
	ref := relayproto.SessionRef{
		MachineID: client.config.MachineID, LocalIndex: localIndex,
		PaneID: target.PaneID, OccupantHash: target.OccupantHash,
	}
	return client.writeCurrent(ctx, relayproto.TypeNotification, "", relayproto.Notification{Target: ref, Content: content})
}

func (client *Client) runSession(parent context.Context, onReady func()) error {
	connection, response, err := websocket.Dial(parent, client.config.URL, &websocket.DialOptions{HTTPClient: client.httpClient()})
	if err != nil && response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return withRelayStage("dial", err)
	}
	connection.SetReadLimit(relayproto.MaxFrameBytes)
	sessionContext, cancelSession := context.WithCancel(parent)
	current := &clientSession{
		ctx: sessionContext, cancel: cancelSession, connection: connection,
		snapshotRequests: make(chan chan error),
	}
	client.setCurrent(current)
	defer client.clearCurrent(current)
	defer current.close()

	helloID := randomClientID()
	if err := current.write(parent, relayproto.TypeClientHello, helloID, relayproto.ClientHello{
		UserID: client.config.UserID, MachineID: client.config.MachineID, ClientVersion: client.config.Version,
	}); err != nil {
		return withRelayStage("client_hello_send", err)
	}
	helloFrame, err := current.read(parent)
	if err != nil {
		return withRelayStage("server_hello_read", err)
	}
	if helloFrame.Type == relayproto.TypeProtocolError {
		return withRelayStage("server_hello", decodeRelayProtocolError(helloFrame))
	}
	if helloFrame.Type != relayproto.TypeServerHello {
		return withRelayStage("server_hello", fmt.Errorf("%w: 缺少 server_hello，收到 %s", ErrInvalidConfig, helloFrame.Type))
	}
	serverHello, err := relayproto.DecodePayload[relayproto.ServerHello](helloFrame)
	if err != nil || serverHello.ConnectionID == "" {
		if err == nil {
			err = fmt.Errorf("%w: server_hello 缺少 connection_id", ErrInvalidConfig)
		}
		return withRelayStage("server_hello_decode", err)
	}
	snapshotInterval := client.config.SnapshotInterval
	if serverHello.SnapshotIntervalSecs > 0 {
		snapshotInterval = time.Duration(serverHello.SnapshotIntervalSecs) * time.Second
	}
	client.logger.Info("Relay 握手成功",
		"machine_id", client.config.MachineID,
		"endpoint", relayLogEndpoint(client.config.URL),
		"connection_id", serverHello.ConnectionID,
		"heartbeat_interval", time.Duration(serverHello.HeartbeatIntervalSecs)*time.Second,
		"heartbeat_timeout", time.Duration(serverHello.HeartbeatTimeoutSecs)*time.Second,
		"snapshot_interval", snapshotInterval,
	)
	sequence := uint64(1)
	initial := BuildSnapshot(sequence, client.localExecutor().CurrentTargets())
	if err := current.write(parent, relayproto.TypeSessionSnapshot, "", snapshotToLegacy(initial)); err != nil {
		return withRelayStage("initial_snapshot_send", err)
	}
	fingerprint := SnapshotFingerprint(initial)
	if onReady != nil {
		onReady()
	}
	client.logger.Info("Relay 连接成功", "machine_id", client.config.MachineID, "endpoint", relayLogEndpoint(client.config.URL), "connection_id", serverHello.ConnectionID, "snapshot_sequence", initial.Sequence, "session_count", len(initial.Sessions))

	readResult := make(chan error, 1)
	go func() { readResult <- client.readLoop(current) }()
	pollTicker := time.NewTicker(client.config.PollInterval)
	defer pollTicker.Stop()
	calibrationTicker := time.NewTicker(snapshotInterval)
	defer calibrationTicker.Stop()
	previousSessionCount := len(initial.Sessions)
	sendSnapshot := func(force bool, trigger string) error {
		candidate := BuildSnapshot(sequence+1, client.localExecutor().CurrentTargets())
		candidateFingerprint := SnapshotFingerprint(candidate)
		changed := candidateFingerprint != fingerprint
		if !force && !changed {
			return nil
		}
		if err := current.write(parent, relayproto.TypeSessionSnapshot, "", snapshotToLegacy(candidate)); err != nil {
			return withRelayStage("snapshot_send", err)
		}
		sequence++
		fingerprint = candidateFingerprint
		client.logger.Debug("Relay 会话快照已上报",
			"machine_id", client.config.MachineID,
			"connection_id", serverHello.ConnectionID,
			"trigger", trigger,
			"snapshot_sequence", candidate.Sequence,
			"previous_session_count", previousSessionCount,
			"session_count", len(candidate.Sessions),
			"changed", changed,
			"forced", force,
		)
		previousSessionCount = len(candidate.Sessions)
		return nil
	}
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case err := <-readResult:
			return withRelayStage("read_loop", err)
		case <-pollTicker.C:
			if err := sendSnapshot(false, "poll_change"); err != nil {
				return err
			}
		case <-calibrationTicker.C:
			if err := sendSnapshot(true, "calibration"); err != nil {
				return err
			}
		case result := <-current.snapshotRequests:
			err := sendSnapshot(true, "on_demand")
			result <- err
			if err != nil {
				return err
			}
		}
	}
}

func (client *Client) readLoop(current *clientSession) error {
	for {
		frame, err := current.read(current.ctx)
		if err != nil {
			return err
		}
		switch frame.Type {
		case relayproto.TypeSelectRequest:
			client.logger.Debug("Relay 选择请求已接收", "machine_id", client.config.MachineID, "request_hash", relayLogHash(frame.RequestID))
			if err := client.handleSelect(current, frame); err != nil {
				return withRelayStage("select_request", err)
			}
		case relayproto.TypeExecuteRequest:
			client.logger.Debug("Relay 执行请求已接收", "machine_id", client.config.MachineID, "request_hash", relayLogHash(frame.RequestID))
			if err := client.handleExecute(current, frame); err != nil {
				return withRelayStage("execute_request", err)
			}
		case relayproto.TypePing:
			heartbeat, err := relayproto.DecodePayload[relayproto.Heartbeat](frame)
			if err != nil {
				return withRelayStage("heartbeat_decode", err)
			}
			if err := current.write(current.ctx, relayproto.TypePong, frame.RequestID, heartbeat); err != nil {
				return withRelayStage("heartbeat_pong_send", err)
			}
			client.logger.Debug("Relay 心跳已响应", "machine_id", client.config.MachineID, "request_hash", relayLogHash(frame.RequestID))
		case relayproto.TypeProtocolError:
			return withRelayStage("protocol_error", decodeRelayProtocolError(frame))
		default:
			return withRelayStage("unexpected_frame", fmt.Errorf("%w: 服务端帧类型 %s", relayproto.ErrInvalidFrame, frame.Type))
		}
	}
}

func (client *Client) handleSelect(current *clientSession, frame relayproto.Frame) error {
	request, err := relayproto.DecodePayload[relayproto.SelectRequest](frame)
	if err != nil {
		return err
	}
	result := relayproto.SelectResult{OK: true}
	resultName := "success"
	if request.Target.MachineID != client.config.MachineID {
		result = relayproto.SelectResult{Code: relayproto.CodeTargetNotFound, Message: "目标机器不匹配"}
		resultName = string(relayproto.CodeTargetNotFound)
	} else if err := client.localExecutor().SelectTarget(request.Target.PaneID, request.Target.OccupantHash); err != nil {
		result = relayproto.SelectResult{Code: relayproto.CodeTargetChanged, Message: "目标 occupant 已变化"}
		resultName = string(relayproto.CodeTargetChanged)
	}
	if err := current.write(current.ctx, relayproto.TypeSelectResult, frame.RequestID, result); err != nil {
		return err
	}
	client.logger.Debug("Relay 选择请求已处理",
		"machine_id", client.config.MachineID,
		"request_hash", relayLogHash(frame.RequestID),
		"target_machine", request.Target.MachineID,
		"local_index", request.Target.LocalIndex,
		"pane_id", request.Target.PaneID,
		"occupant_hash", relayLogHash(request.Target.OccupantHash),
		"result", resultName,
	)
	return nil
}

func (client *Client) handleExecute(current *clientSession, frame relayproto.Frame) error {
	request, err := relayproto.DecodePayload[relayproto.ExecuteRequest](frame)
	if err != nil {
		return err
	}
	if request.Target.MachineID != client.config.MachineID || client.localExecutor().SelectTarget(request.Target.PaneID, request.Target.OccupantHash) != nil {
		if err := current.write(current.ctx, relayproto.TypeExecuteResponse, frame.RequestID, relayproto.ExecuteResponse{Content: "目标 Agent 已变化，请重新执行 /ls 和 /N。"}); err != nil {
			return err
		}
		client.logExecuteResult(frame.RequestID, request, "target_changed")
		return nil
	}
	message := im.IncomingText{
		RequestID: frame.RequestID, MessageID: request.MessageID, UserID: request.UserID,
		ChatType: "single", Content: request.Content,
	}
	client.executionMu.Lock()
	client.activeRequestID = frame.RequestID
	client.activeTarget = request.Target
	client.executionMu.Unlock()
	client.localExecutor().HandleMessage(current.ctx, message)
	client.executionMu.Lock()
	if client.activeRequestID == frame.RequestID {
		client.activeRequestID = ""
		client.activeTarget = relayproto.SessionRef{}
	}
	client.executionMu.Unlock()
	client.logExecuteResult(frame.RequestID, request, "success")
	return nil
}

func (client *Client) logExecuteResult(requestID string, request relayproto.ExecuteRequest, result string) {
	client.logger.Debug("Relay 执行请求已处理",
		"machine_id", client.config.MachineID,
		"request_hash", relayLogHash(requestID),
		"message_hash", relayLogHash(request.MessageID),
		"target_machine", request.Target.MachineID,
		"local_index", request.Target.LocalIndex,
		"pane_id", request.Target.PaneID,
		"occupant_hash", relayLogHash(request.Target.OccupantHash),
		"content_length", len(request.Content),
		"result", result,
	)
}

func (client *Client) writeCurrent(ctx context.Context, frameType relayproto.Type, requestID string, payload any) error {
	client.currentMu.RLock()
	current := client.current
	client.currentMu.RUnlock()
	if current == nil {
		return ErrUnavailable
	}
	return current.write(ctx, frameType, requestID, payload)
}

func (client *Client) localExecutor() Executor {
	client.executorMu.RLock()
	defer client.executorMu.RUnlock()
	return client.executor
}

func (client *Client) selectedSessionRef() (relayproto.SessionRef, error) {
	executor := client.localExecutor()
	selected, err := executor.SelectedTarget()
	if err != nil {
		return relayproto.SessionRef{}, err
	}
	for index, current := range executor.CurrentTargets() {
		if current.PaneID == selected.PaneID && current.OccupantKey == selected.OccupantKey {
			return relayproto.SessionRef{
				MachineID: client.config.MachineID, LocalIndex: index + 1,
				PaneID: selected.PaneID, OccupantHash: selected.OccupantKey,
			}, nil
		}
	}
	return relayproto.SessionRef{}, session.ErrListSnapshotExpired
}

func (client *Client) reportCurrentSnapshot(ctx context.Context) error {
	client.currentMu.RLock()
	current := client.current
	client.currentMu.RUnlock()
	if current == nil {
		return ErrUnavailable
	}
	result := make(chan error, 1)
	select {
	case current.snapshotRequests <- result:
	case <-ctx.Done():
		return ctx.Err()
	case <-current.ctx.Done():
		return ErrUnavailable
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-current.ctx.Done():
		return ErrUnavailable
	}
}

func (client *Client) setCurrent(current *clientSession) {
	client.currentMu.Lock()
	client.current = current
	client.currentMu.Unlock()
}

func (client *Client) clearCurrent(expected *clientSession) {
	client.currentMu.Lock()
	if client.current == expected {
		client.current = nil
	}
	client.currentMu.Unlock()
}

func (client *Client) httpClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// 该选项只在可信内网兼容自签名证书，不提供服务端身份认证。
		InsecureSkipVerify: client.config.SkipVerify,
	}
	return &http.Client{Transport: transport}
}

func (current *clientSession) write(ctx context.Context, frameType relayproto.Type, requestID string, payload any) error {
	frame, err := relayproto.NewFrame(frameType, requestID, payload)
	if err != nil {
		return err
	}
	encoded, err := relayproto.Encode(frame)
	if err != nil {
		return err
	}
	current.writeMu.Lock()
	defer current.writeMu.Unlock()
	if current.ctx.Err() != nil {
		return ErrUnavailable
	}
	return current.connection.Write(ctx, websocket.MessageText, encoded)
}

func (current *clientSession) read(ctx context.Context) (relayproto.Frame, error) {
	messageType, data, err := current.connection.Read(ctx)
	if err != nil {
		return relayproto.Frame{}, err
	}
	if messageType != websocket.MessageText {
		return relayproto.Frame{}, relayproto.ErrInvalidFrame
	}
	return relayproto.Decode(data)
}

func (current *clientSession) close() {
	current.cancel()
	_ = current.connection.Close(websocket.StatusNormalClosure, "client session ended")
}

func randomClientID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
}

func relayErrorType(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case relayTLSError(err):
		return "tls"
	case relayDNSError(err):
		return "dns"
	case relayProtocolError(err) != nil:
		return "protocol_error"
	case errors.Is(err, relayproto.ErrProtocolMismatch), errors.Is(err, relayproto.ErrInvalidFrame):
		return "protocol"
	case websocket.CloseStatus(err) != -1:
		return "remote_close"
	default:
		return "transport"
	}
}

func relayErrorLogArgs(err error, rawURL, userID string) []any {
	stage := "session"
	var staged *relayStageError
	if errors.As(err, &staged) && staged.stage != "" {
		stage = staged.stage
	}
	args := []any{"stage", stage, "error_type", relayErrorType(err), "reason", safeRelayReason(err, rawURL, userID)}
	if protocolError := relayProtocolError(err); protocolError != nil {
		args = append(args, "error_code", protocolError.Code, "close_connection", protocolError.Close)
		if message := safeRelayLogValue(redactRelaySensitive(protocolError.Message, rawURL, userID), 240); message != "" {
			args = append(args, "server_message", message)
		}
	}
	if status := websocket.CloseStatus(err); status != -1 {
		args = append(args, "close_status", status)
	}
	return args
}

func decodeRelayProtocolError(frame relayproto.Frame) error {
	payload, err := relayproto.DecodePayload[relayproto.ProtocolErrorPayload](frame)
	if err != nil {
		return err
	}
	return relayproto.NewProtocolError(payload.Code, payload.Message, payload.Close)
}

func relayProtocolError(err error) *relayproto.ProtocolError {
	var protocolError *relayproto.ProtocolError
	if errors.As(err, &protocolError) {
		return protocolError
	}
	return nil
}

func relayTLSError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "tls:") || strings.Contains(message, "x509:") || strings.Contains(message, "certificate")
}

func relayDNSError(err error) bool {
	var dnsError *net.DNSError
	return errors.As(err, &dnsError)
}

func safeRelayReason(err error, rawURL, userID string) string {
	if err == nil {
		return "未提供断开原因"
	}
	return safeRelayLogValue(redactRelaySensitive(err.Error(), rawURL, userID), 512)
}

func redactRelaySensitive(value, rawURL, userID string) string {
	if userID = strings.TrimSpace(userID); userID != "" {
		value = strings.ReplaceAll(value, userID, "[userid]")
	}
	if rawURL != "" {
		value = strings.ReplaceAll(value, rawURL, relayLogEndpoint(rawURL))
		if endpoint, parseErr := url.Parse(rawURL); parseErr == nil {
			httpsURL := *endpoint
			httpsURL.Scheme = "https"
			value = strings.ReplaceAll(value, httpsURL.String(), relayLogEndpoint(rawURL))
		}
	}
	return value
}

func safeRelayLogValue(value string, limit int) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(value, "�"))
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit] + "…"
	}
	return value
}

func relayLogEndpoint(rawURL string) string {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Host == "" {
		return "invalid-endpoint"
	}
	return endpoint.Scheme + "://" + endpoint.Host
}

func relayLogHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}
