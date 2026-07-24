package relayclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
}

type clientSession struct {
	ctx        context.Context
	cancel     context.CancelFunc
	connection *websocket.Conn
	writeMu    sync.Mutex
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
		err := client.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client.logger.Warn("Relay 连接已断开", "error_type", relayErrorType(err))
		delay := backoff.Next()
		if err := waitReconnect(ctx, delay); err != nil {
			return err
		}
	}
}

// RespondMarkdown 把 Bridge 首段回复写成 execute_response。
func (client *Client) RespondMarkdown(ctx context.Context, requestID, content string) error {
	return client.writeCurrent(ctx, relayproto.TypeExecuteResponse, requestID, relayproto.ExecuteResponse{Content: content})
}

// SendMarkdown 把当前执行产生的后续分段写成 execute_push。
func (client *Client) SendMarkdown(ctx context.Context, content string) error {
	client.executionMu.Lock()
	requestID := client.activeRequestID
	client.executionMu.Unlock()
	if requestID == "" {
		return ErrUnavailable
	}
	return client.writeCurrent(ctx, relayproto.TypeExecutePush, requestID, relayproto.ExecutePush{Content: content})
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

func (client *Client) runSession(parent context.Context) error {
	connection, response, err := websocket.Dial(parent, client.config.URL, &websocket.DialOptions{HTTPClient: client.httpClient()})
	if err != nil && response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return err
	}
	connection.SetReadLimit(relayproto.MaxFrameBytes)
	sessionContext, cancelSession := context.WithCancel(parent)
	current := &clientSession{ctx: sessionContext, cancel: cancelSession, connection: connection}
	client.setCurrent(current)
	defer client.clearCurrent(current)
	defer current.close()

	helloID := randomClientID()
	if err := current.write(parent, relayproto.TypeClientHello, helloID, relayproto.ClientHello{
		UserID: client.config.UserID, MachineID: client.config.MachineID, ClientVersion: client.config.Version,
	}); err != nil {
		return err
	}
	helloFrame, err := current.read(parent)
	if err != nil {
		return err
	}
	if helloFrame.Type != relayproto.TypeServerHello {
		return fmt.Errorf("%w: 缺少 server_hello", ErrInvalidConfig)
	}
	serverHello, err := relayproto.DecodePayload[relayproto.ServerHello](helloFrame)
	if err != nil || serverHello.ConnectionID == "" {
		return fmt.Errorf("%w: server_hello 无效", ErrInvalidConfig)
	}
	sequence := uint64(1)
	initial := BuildSnapshot(sequence, client.localExecutor().CurrentTargets())
	if err := current.write(parent, relayproto.TypeSessionSnapshot, "", initial); err != nil {
		return err
	}
	fingerprint := SnapshotFingerprint(initial)

	readResult := make(chan error, 1)
	go func() { readResult <- client.readLoop(current) }()
	pollTicker := time.NewTicker(client.config.PollInterval)
	defer pollTicker.Stop()
	snapshotInterval := client.config.SnapshotInterval
	if serverHello.SnapshotIntervalSecs > 0 {
		snapshotInterval = time.Duration(serverHello.SnapshotIntervalSecs) * time.Second
	}
	calibrationTicker := time.NewTicker(snapshotInterval)
	defer calibrationTicker.Stop()
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case err := <-readResult:
			return err
		case <-pollTicker.C:
			candidate := BuildSnapshot(sequence+1, client.localExecutor().CurrentTargets())
			candidateFingerprint := SnapshotFingerprint(candidate)
			if candidateFingerprint == fingerprint {
				continue
			}
			if err := current.write(parent, relayproto.TypeSessionSnapshot, "", candidate); err != nil {
				return err
			}
			sequence++
			fingerprint = candidateFingerprint
		case <-calibrationTicker.C:
			candidate := BuildSnapshot(sequence+1, client.localExecutor().CurrentTargets())
			if err := current.write(parent, relayproto.TypeSessionSnapshot, "", candidate); err != nil {
				return err
			}
			sequence++
			fingerprint = SnapshotFingerprint(candidate)
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
			if err := client.handleSelect(current, frame); err != nil {
				return err
			}
		case relayproto.TypeExecuteRequest:
			if err := client.handleExecute(current, frame); err != nil {
				return err
			}
		case relayproto.TypePing:
			heartbeat, err := relayproto.DecodePayload[relayproto.Heartbeat](frame)
			if err != nil {
				return err
			}
			if err := current.write(current.ctx, relayproto.TypePong, frame.RequestID, heartbeat); err != nil {
				return err
			}
		case relayproto.TypeProtocolError:
			return ErrUnavailable
		default:
			return fmt.Errorf("%w: 服务端帧类型 %s", relayproto.ErrInvalidFrame, frame.Type)
		}
	}
}

func (client *Client) handleSelect(current *clientSession, frame relayproto.Frame) error {
	request, err := relayproto.DecodePayload[relayproto.SelectRequest](frame)
	if err != nil {
		return err
	}
	result := relayproto.SelectResult{OK: true}
	if request.Target.MachineID != client.config.MachineID {
		result = relayproto.SelectResult{Code: relayproto.CodeTargetNotFound, Message: "目标机器不匹配"}
	} else if err := client.localExecutor().SelectTarget(request.Target.PaneID, request.Target.OccupantHash); err != nil {
		result = relayproto.SelectResult{Code: relayproto.CodeTargetChanged, Message: "目标 occupant 已变化"}
	}
	return current.write(current.ctx, relayproto.TypeSelectResult, frame.RequestID, result)
}

func (client *Client) handleExecute(current *clientSession, frame relayproto.Frame) error {
	request, err := relayproto.DecodePayload[relayproto.ExecuteRequest](frame)
	if err != nil {
		return err
	}
	if request.Target.MachineID != client.config.MachineID || client.localExecutor().SelectTarget(request.Target.PaneID, request.Target.OccupantHash) != nil {
		return current.write(current.ctx, relayproto.TypeExecuteResponse, frame.RequestID, relayproto.ExecuteResponse{Content: "目标 Agent 已变化，请重新执行 /ls 和 /N。"})
	}
	message := im.IncomingText{
		RequestID: frame.RequestID, MessageID: request.MessageID, UserID: request.UserID,
		ChatType: "single", Content: request.Content,
	}
	client.executionMu.Lock()
	client.activeRequestID = frame.RequestID
	client.executionMu.Unlock()
	client.localExecutor().HandleMessage(current.ctx, message)
	client.executionMu.Lock()
	if client.activeRequestID == frame.RequestID {
		client.activeRequestID = ""
	}
	client.executionMu.Unlock()
	return nil
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
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context"
	case errors.Is(err, relayproto.ErrProtocolMismatch), errors.Is(err, relayproto.ErrInvalidFrame):
		return "protocol"
	default:
		return "transport"
	}
}
