package relayclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/session"
)

var (
	// ErrInvalidConfig 表示 Relay 客户端配置无效。
	ErrInvalidConfig = errors.New("Relay 客户端配置无效")
	// ErrUnavailable 表示当前没有可写的 HPRP 连接。
	ErrUnavailable = errors.New("HPRP 连接不可用")
	// ErrInvalidExecutor 表示本地执行器缺失或重复设置。
	ErrInvalidExecutor = errors.New("Relay 本地执行器无效")
)

const (
	defaultPollInterval        = 250 * time.Millisecond
	defaultSnapshotInterval    = 30 * time.Second
	defaultSnapshotAckTimeout  = 20 * time.Second
	defaultIdempotencyWindow   = 10 * time.Minute
	defaultIdempotencyCapacity = 1024
)

type relayStageError struct {
	stage string
	err   error
}

type relayHTTPStatusError struct {
	statusCode int
	err        error
}

func (err *relayHTTPStatusError) Error() string { return err.err.Error() }

func (err *relayHTTPStatusError) Unwrap() error { return err.err }

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

// Config 是 Pal 的 HPRP/1 连接和快照上报配置。
type Config struct {
	URL              string
	Key              string
	CredentialID     uint64
	SkipVerify       bool
	Version          string
	PollInterval     time.Duration
	SnapshotInterval time.Duration
	BackoffMin       time.Duration
	BackoffMax       time.Duration
	Logger           *slog.Logger
}

// Executor 是 HPRP 命令所需的本地 Bridge 能力。
type Executor interface {
	CurrentTargets() []session.Target
	SelectedTarget() (session.Target, error)
	SelectTarget(paneID, occupantHash string) error
	HandleMessage(ctx context.Context, message im.IncomingText)
	ReadTerminalSnapshot(ctx context.Context, paneID, occupantHash string, mode im.OutputMode, maxLines int) (im.TerminalContent, error)
}

// Client 维护 HPRP/1 WSS、执行服务端命令并实现 Bridge 消息 sink。
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
	activeTarget    hprp.Target
	activeCommand   hprp.CommandExecute
	activeOutputs   []hprp.Content
	activeResult    *hprp.CommandResult

	notificationSequence atomic.Uint64
	resultCache          *commandResultCache
}

type clientPendingRequest struct {
	expected hprp.Type
	result   chan hprp.Envelope
}

type clientSession struct {
	ctx              context.Context
	cancel           context.CancelFunc
	connection       *websocket.Conn
	machineID        string
	capabilities     map[string]struct{}
	snapshotRequests chan chan error
	writeMu          sync.Mutex
	pendingMu        sync.Mutex
	pending          map[string]clientPendingRequest
	snapshotSequence atomic.Uint64
}

type remoteProtocolError struct {
	payload hprp.ProtocolError
}

func (err *remoteProtocolError) Error() string {
	if err == nil {
		return "HPRP 远端协议错误"
	}
	return fmt.Sprintf("HPRP 远端协议错误: %s", err.payload.Error.Code)
}

// New 创建尚未启动的 HPRP 客户端；Run 前必须设置 Executor。
func New(config Config) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return nil, fmt.Errorf("%w: URL 必须是 wss://", ErrInvalidConfig)
	}
	credentialID, err := credential.BearerCredentialID(strings.TrimSpace(config.Key))
	if err != nil {
		return nil, fmt.Errorf("%w: Key 格式无效", ErrInvalidConfig)
	}
	if config.CredentialID != 0 && config.CredentialID != credentialID {
		return nil, fmt.Errorf("%w: credential_id 与 Key 不匹配", ErrInvalidConfig)
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.SnapshotInterval <= 0 {
		config.SnapshotInterval = defaultSnapshotInterval
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	config.URL = endpoint.String()
	config.Key = strings.TrimSpace(config.Key)
	config.CredentialID = credentialID
	return &Client{
		config: config, logger: config.Logger,
		resultCache: newCommandResultCache(defaultIdempotencyCapacity, defaultIdempotencyWindow, time.Now),
	}, nil
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

// Run 持续维护 HPRP/1 连接，直到 context 取消。
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
		client.logger.Info("HPRP 连接中", "credential_id", client.config.CredentialID, "endpoint", relayLogEndpoint(client.config.URL), "skip_verify", client.config.SkipVerify)
		err := client.runSession(ctx, backoff.Reset)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := backoff.Next()
		args := []any{"credential_id", client.config.CredentialID, "endpoint", relayLogEndpoint(client.config.URL)}
		args = append(args, relayErrorLogArgs(err, client.config.URL, client.config.Key)...)
		args = append(args, "retry_delay", delay)
		client.logger.Warn("HPRP 连接已断开", args...)
		if err := waitReconnect(ctx, delay); err != nil {
			return err
		}
	}
}

// RespondMarkdown 把 Bridge 首段回复写成 command.result。
func (client *Client) RespondMarkdown(ctx context.Context, requestID, content string) error {
	return client.respondContent(ctx, requestID, hprp.Content{Type: hprp.ContentTypeText, Text: content})
}

// RespondTerminal 把 Bridge 首段结构化终端回复写成 command.result。
func (client *Client) RespondTerminal(ctx context.Context, requestID string, content im.TerminalContent) error {
	encoded, err := encodeTerminalContent(content)
	if err != nil {
		return err
	}
	return client.respondContent(ctx, requestID, encoded)
}

func (client *Client) respondContent(ctx context.Context, requestID string, content hprp.Content) error {
	client.executionMu.Lock()
	if client.activeRequestID != requestID || requestID == "" {
		client.executionMu.Unlock()
		return ErrUnavailable
	}
	activeTarget := client.activeTarget
	client.executionMu.Unlock()

	result := hprp.CommandResult{
		Outcome: hprp.OutcomeOK,
		Content: &content,
	}
	if selected, err := client.selectedTarget(); err == nil && selected.MachineID == activeTarget.MachineID &&
		selected.SlotID == activeTarget.SlotID && selected.SessionID != activeTarget.SessionID {
		if err := client.reportCurrentSnapshot(ctx); err != nil {
			return err
		}
		result.ReplacementTarget = &selected
		client.executionMu.Lock()
		if client.activeRequestID == requestID && client.activeTarget == activeTarget {
			client.activeTarget = selected
		}
		client.executionMu.Unlock()
	}
	client.executionMu.Lock()
	if client.activeRequestID == requestID {
		resultCopy := result
		client.activeResult = &resultCopy
		client.executionMu.Unlock()
		return nil
	}
	client.executionMu.Unlock()
	return ErrUnavailable
}

// SendMarkdown 缓冲当前命令的后续输出，在本地处理结束后按 HPRP 顺序发送。
func (client *Client) SendMarkdown(_ context.Context, content string) error {
	return client.sendContent(hprp.Content{Type: hprp.ContentTypeText, Text: content})
}

// SendTerminal 缓冲当前命令的后续结构化终端输出。
func (client *Client) SendTerminal(_ context.Context, content im.TerminalContent) error {
	encoded, err := encodeTerminalContent(content)
	if err != nil {
		return err
	}
	return client.sendContent(encoded)
}

func (client *Client) sendContent(content hprp.Content) error {
	client.executionMu.Lock()
	defer client.executionMu.Unlock()
	if client.activeRequestID == "" {
		return ErrUnavailable
	}
	client.activeOutputs = append(client.activeOutputs, cloneHPRPContent(content))
	return nil
}

// SendNotification 发送携带稳定本机目标的主动通知；断线时直接失败且不缓存。
func (client *Client) SendNotification(ctx context.Context, target im.NotificationTarget, event im.NotificationEvent) error {
	current := client.currentSession()
	if current == nil {
		return ErrUnavailable
	}
	if _, err := client.resolveLocalTarget(current.machineID, target.PaneID, target.OccupantHash); err != nil {
		return err
	}
	sequence := client.notificationSequence.Add(1)
	occurredAt := event.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	notification := hprp.NotificationEvent{
		EventKey: randomClientID(), Sequence: sequence,
		Target:           hprp.Target{MachineID: current.machineID, SlotID: target.PaneID, SessionID: target.OccupantHash},
		SnapshotSequence: current.snapshotSequence.Load(), OccurredAt: occurredAt,
	}
	switch event.Kind {
	case im.NotificationKindAgentStatusChanged:
		notification.Kind = hprp.NotificationKindAgentStatusChanged
		notification.Data = &hprp.StatusChangeData{PreviousStatus: event.PreviousStatus, Status: event.Status}
	case im.NotificationKindTargetInvalidated:
		notification.Kind = hprp.NotificationKindTargetInvalidated
	default:
		return hprp.ErrInvalidMessage
	}
	if err := hprp.ValidateNotificationEvent(notification); err != nil {
		return err
	}
	return current.write(ctx, hprp.TypeNotificationEvent, randomClientID(), "", false, notification)
}

func (client *Client) runSession(parent context.Context, onReady func()) error {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+client.config.Key)
	connection, response, err := websocket.Dial(parent, client.config.URL, &websocket.DialOptions{
		HTTPClient: client.httpClient(), HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if err != nil && response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil {
			err = &relayHTTPStatusError{statusCode: response.StatusCode, err: err}
		}
		return withRelayStage("dial", err)
	}
	if connection.Subprotocol() != hprp.Subprotocol {
		_ = connection.Close(websocket.StatusPolicyViolation, "HPRP subprotocol missing")
		return withRelayStage("subprotocol", fmt.Errorf("%w: 服务端未选择 %s", ErrInvalidConfig, hprp.Subprotocol))
	}
	connection.SetReadLimit(hprp.MaxMessageBytes)
	sessionContext, cancelSession := context.WithCancel(parent)
	current := &clientSession{
		ctx: sessionContext, cancel: cancelSession, connection: connection,
		snapshotRequests: make(chan chan error), pending: make(map[string]clientPendingRequest),
	}
	defer current.close()

	helloID := randomClientID()
	if err := current.write(parent, hprp.TypeHelloClient, helloID, "", true, hprp.ClientHello{
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: client.config.Version, OS: runtime.GOOS, Arch: runtime.GOARCH},
		Capabilities: []string{
			hprp.CapabilityCommandOutputV1,
			hprp.CapabilityTerminalSnapshotV1,
			hprp.CapabilityTerminalImageV1,
		}, Features: map[string]hprp.FeatureOffer{},
		Limits: hprp.ClientLimits{
			MaxReceiveMessageBytes: hprp.MaxMessageBytes, MaxInflightCommands: 1, MaxInflightFeatures: 0,
			IdempotencyWindowMS: defaultIdempotencyWindow.Milliseconds(),
		},
	}); err != nil {
		return withRelayStage("hello.client_send", err)
	}
	helloEnvelope, err := current.read(parent)
	if err != nil {
		return withRelayStage("hello.server_read", err)
	}
	if helloEnvelope.Type == hprp.TypeProtocolError {
		return withRelayStage("hello.server", decodeHPRPProtocolError(helloEnvelope))
	}
	if helloEnvelope.Type != hprp.TypeHelloServer || helloEnvelope.ReplyTo != helloID {
		return withRelayStage("hello.server", fmt.Errorf("%w: hello.server 关联无效", ErrInvalidConfig))
	}
	serverHello, err := hprp.DecodePayload[hprp.ServerHello](helloEnvelope)
	if err == nil {
		err = hprp.ValidateServerHello(serverHello)
	}
	if err != nil {
		return withRelayStage("hello.server_decode", err)
	}
	current.machineID = serverHello.MachineID
	current.setCapabilities(serverHello.Capabilities)
	client.logger.Info("HPRP 握手成功",
		"credential_id", client.config.CredentialID, "machine_id", current.machineID,
		"endpoint", relayLogEndpoint(client.config.URL), "connection_id", serverHello.ConnectionID,
		"heartbeat_interval", time.Duration(serverHello.Heartbeat.PingIntervalMS)*time.Millisecond,
		"heartbeat_timeout", time.Duration(serverHello.Heartbeat.IdleTimeoutMS)*time.Millisecond,
		"snapshot_interval", client.config.SnapshotInterval,
	)

	sequence := uint64(1)
	initial := BuildSnapshot(sequence, client.localExecutor().CurrentTargets())
	initialID := randomClientID()
	if err := current.write(parent, hprp.TypeSessionSnapshot, initialID, "", true, initial); err != nil {
		return withRelayStage("initial_snapshot_send", err)
	}
	snapshotEnvelope, err := current.read(parent)
	if err != nil {
		return withRelayStage("initial_snapshot_result", err)
	}
	if err := validateSnapshotAcknowledgement(snapshotEnvelope, initialID, initial.Sequence); err != nil {
		return withRelayStage("initial_snapshot_result", err)
	}
	current.snapshotSequence.Store(initial.Sequence)
	fingerprint := SnapshotFingerprint(initial)
	readResult := make(chan error, 1)
	go func() { readResult <- client.readLoop(current) }()
	client.setCurrent(current)
	defer client.clearCurrent(current)
	if onReady != nil {
		onReady()
	}
	client.logger.Info("HPRP 连接成功", "credential_id", client.config.CredentialID, "machine_id", current.machineID, "endpoint", relayLogEndpoint(client.config.URL), "connection_id", serverHello.ConnectionID, "snapshot_sequence", initial.Sequence, "session_count", len(initial.Sessions))

	pollTicker := time.NewTicker(client.config.PollInterval)
	defer pollTicker.Stop()
	calibrationTicker := time.NewTicker(client.config.SnapshotInterval)
	defer calibrationTicker.Stop()
	previousSessionCount := len(initial.Sessions)
	sendSnapshot := func(force bool, trigger string) error {
		candidate := BuildSnapshot(sequence+1, client.localExecutor().CurrentTargets())
		candidateFingerprint := SnapshotFingerprint(candidate)
		changed := candidateFingerprint != fingerprint
		if !force && !changed {
			return nil
		}
		requestContext, cancel := context.WithTimeout(parent, defaultSnapshotAckTimeout)
		snapshotID := randomClientID()
		response, err := current.request(requestContext, hprp.TypeSessionSnapshot, snapshotID, candidate, hprp.TypeSessionSnapshotResult)
		cancel()
		if err != nil {
			return withRelayStage("snapshot_send", err)
		}
		if err := validateSnapshotAcknowledgement(response, snapshotID, candidate.Sequence); err != nil {
			return withRelayStage("snapshot_result", err)
		}
		sequence++
		current.snapshotSequence.Store(candidate.Sequence)
		fingerprint = candidateFingerprint
		client.logger.Debug("HPRP 会话快照已确认",
			"credential_id", client.config.CredentialID, "machine_id", current.machineID, "connection_id", serverHello.ConnectionID,
			"trigger", trigger, "snapshot_sequence", candidate.Sequence, "previous_session_count", previousSessionCount,
			"session_count", len(candidate.Sessions), "changed", changed, "forced", force,
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
		envelope, err := current.read(current.ctx)
		if err != nil {
			return err
		}
		switch envelope.Type {
		case hprp.TypeSessionSnapshotResult:
			if !current.deliver(envelope) {
				return fmt.Errorf("%w: 未匹配的 session.snapshot.result", hprp.ErrInvalidMessage)
			}
		case hprp.TypeCommandExecute:
			go client.handleCommand(current, envelope)
		case hprp.TypeTerminalSnapshotGet:
			go client.handleTerminalSnapshot(current, envelope)
		case hprp.TypeProtocolError:
			return decodeHPRPProtocolError(envelope)
		default:
			if envelope.MustUnderstand {
				_ = current.write(current.ctx, hprp.TypeProtocolError, randomClientID(), envelope.ID, false, hprp.ProtocolError{
					Error: hprp.Error{
						Code: hprp.CodeProtocolRequiredExtensionUnsupported, Message: "消息类型不受支持", Retryable: false,
					},
					Close: true,
				})
				return fmt.Errorf("%w: 不支持的必需消息 %s", hprp.ErrInvalidMessage, envelope.Type)
			}
			client.logger.Debug("HPRP 未知可选消息已忽略", "credential_id", client.config.CredentialID, "machine_id", current.machineID, "event_type", envelope.Type)
		}
	}
}

func (client *Client) handleCommand(current *clientSession, envelope hprp.Envelope) {
	command, err := hprp.DecodePayload[hprp.CommandExecute](envelope)
	if err == nil {
		err = hprp.ValidateCommandExecute(command)
	}
	if err != nil {
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), envelope.ID, false, rejectedCommand(hprp.CodeProtocolInvalidMessage, "命令格式无效"))
		return
	}
	mode := command.OutputMode
	if mode == "" {
		mode = hprp.OutputModeText
	}
	if mode == hprp.OutputModeImage && !current.supportsCapability(hprp.CapabilityTerminalImageV1) {
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), envelope.ID, false,
			rejectedCommand(hprp.CodeTerminalImageUnsupported, "当前连接未协商终端图片能力"))
		client.logCommandResult(envelope.ID, command, "image_unsupported")
		return
	}
	switch cached, state := client.resultCache.Lookup(command); state {
	case commandCacheHit:
		client.writeCachedCommand(current, envelope.ID, cached)
		client.logCommandResult(envelope.ID, command, "idempotent_replay")
		return
	case commandCacheConflict:
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), envelope.ID, false,
			rejectedCommand(hprp.CodeCommandIdempotencyConflict, "幂等键已用于其他命令"))
		client.logCommandResult(envelope.ID, command, "idempotency_conflict")
		return
	}
	if command.Target.MachineID != current.machineID || client.localExecutor().SelectTarget(command.Target.SlotID, command.Target.SessionID) != nil {
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), envelope.ID, false, rejectedCommand(hprp.CodeTargetSessionChanged, "目标 Agent 已变化"))
		client.logCommandResult(envelope.ID, command, "target_changed")
		return
	}
	client.executionMu.Lock()
	if client.activeRequestID != "" {
		client.executionMu.Unlock()
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), envelope.ID, false, rejectedCommand(hprp.CodeServerBusy, "本地命令正在执行"))
		client.logCommandResult(envelope.ID, command, "busy")
		return
	}
	client.activeRequestID = envelope.ID
	client.activeTarget = command.Target
	client.activeCommand = command
	client.activeOutputs = nil
	client.activeResult = nil
	client.executionMu.Unlock()

	message := im.IncomingText{
		RequestID: envelope.ID, MessageID: command.IdempotencyKey, UserID: strconv.FormatUint(client.config.CredentialID, 10),
		ChatType: "single", Content: command.Content.Text, OutputMode: im.OutputMode(mode),
	}
	client.localExecutor().HandleMessage(current.ctx, message)
	client.finishCommand(current, envelope.ID)
	client.logCommandResult(envelope.ID, command, "success")
}

func (client *Client) handleTerminalSnapshot(current *clientSession, envelope hprp.Envelope) {
	request, err := hprp.DecodePayload[hprp.TerminalSnapshotGet](envelope)
	if err == nil {
		err = hprp.ValidateTerminalSnapshotGet(request)
	}
	if err != nil {
		client.writeProtocolError(current, envelope.ID, hprp.CodeProtocolInvalidMessage, "终端快照请求格式无效")
		return
	}
	if !current.supportsCapability(hprp.CapabilityTerminalSnapshotV1) {
		client.writeTerminalSnapshotResult(current, envelope.ID, hprp.TerminalSnapshotResult{
			Outcome: hprp.OutcomeRejected, Target: request.Target,
			Error: &hprp.Error{Code: hprp.CodeTerminalSnapshotUnsupported, Message: "当前连接未协商终端快照能力", Retryable: false},
		})
		return
	}
	if request.Mode == hprp.OutputModeImage && !current.supportsCapability(hprp.CapabilityTerminalImageV1) {
		client.writeTerminalSnapshotResult(current, envelope.ID, hprp.TerminalSnapshotResult{
			Outcome: hprp.OutcomeRejected, Target: request.Target,
			Error: &hprp.Error{Code: hprp.CodeTerminalImageUnsupported, Message: "当前连接未协商终端图片能力", Retryable: false},
		})
		return
	}
	if request.Target.MachineID != current.machineID {
		client.writeTerminalSnapshotResult(current, envelope.ID, terminalTargetChangedResult(request.Target))
		return
	}
	if _, err := client.resolveLocalTarget(current.machineID, request.Target.SlotID, request.Target.SessionID); err != nil {
		client.writeTerminalSnapshotResult(current, envelope.ID, terminalTargetChangedResult(request.Target))
		return
	}

	content, readErr := client.localExecutor().ReadTerminalSnapshot(
		current.ctx,
		request.Target.SlotID,
		request.Target.SessionID,
		im.OutputMode(request.Mode),
		request.MaxLines,
	)
	if readErr == nil {
		encoded, encodeErr := encodeTerminalContent(content)
		if encodeErr == nil {
			client.writeTerminalSnapshotResult(current, envelope.ID, hprp.TerminalSnapshotResult{
				Outcome: hprp.OutcomeOK, Target: request.Target, Content: &encoded,
			})
			return
		}
		readErr = encodeErr
	}
	if request.Mode == hprp.OutputModeImage && content.Text != "" {
		content.Mode = im.OutputModeText
		content.Image = nil
		fallback, fallbackErr := encodeTerminalContent(content)
		if fallbackErr == nil {
			client.writeTerminalSnapshotResult(current, envelope.ID, hprp.TerminalSnapshotResult{
				Outcome: hprp.OutcomeFailed, Target: request.Target, FallbackContent: &fallback,
				Error: &hprp.Error{Code: hprp.CodeTerminalImageFailed, Message: "终端图片生成失败", Retryable: false},
			})
			return
		}
	}
	if errors.Is(readErr, session.ErrListSnapshotExpired) || errors.Is(readErr, session.ErrSelectionInvalid) {
		client.writeTerminalSnapshotResult(current, envelope.ID, terminalTargetChangedResult(request.Target))
		return
	}
	client.writeTerminalSnapshotResult(current, envelope.ID, hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeFailed, Target: request.Target,
		Error: &hprp.Error{Code: hprp.CodeTerminalSnapshotFailed, Message: "终端快照读取失败", Retryable: true},
	})
}

func (client *Client) writeTerminalSnapshotResult(current *clientSession, replyTo string, result hprp.TerminalSnapshotResult) {
	if err := hprp.ValidateTerminalSnapshotResult(result); err != nil {
		client.writeProtocolError(current, replyTo, hprp.CodeServerInternal, "终端快照结果无效")
		return
	}
	_ = current.write(current.ctx, hprp.TypeTerminalSnapshotResult, randomClientID(), replyTo, false, result)
}

func (client *Client) writeProtocolError(current *clientSession, replyTo string, code hprp.ErrorCode, message string) {
	_ = current.write(current.ctx, hprp.TypeProtocolError, randomClientID(), replyTo, false, hprp.ProtocolError{
		Error: hprp.Error{Code: code, Message: message, Retryable: false}, Close: false,
	})
}

func terminalTargetChangedResult(target hprp.Target) hprp.TerminalSnapshotResult {
	return hprp.TerminalSnapshotResult{
		Outcome: hprp.OutcomeRejected, Target: target,
		Error: &hprp.Error{Code: hprp.CodeTargetSessionChanged, Message: "目标 Agent 已变化", Retryable: false},
	}
}

func (client *Client) finishCommand(current *clientSession, requestID string) {
	client.executionMu.Lock()
	if client.activeRequestID != requestID {
		client.executionMu.Unlock()
		return
	}
	target := client.activeTarget
	command := client.activeCommand
	outputs := cloneHPRPContents(client.activeOutputs)
	result := client.activeResult
	client.activeRequestID = ""
	client.activeTarget = hprp.Target{}
	client.activeCommand = hprp.CommandExecute{}
	client.activeOutputs = nil
	client.activeResult = nil
	client.executionMu.Unlock()
	if result == nil {
		failed := hprp.CommandResult{
			Outcome: hprp.OutcomeFailed,
			Error:   &hprp.Error{Code: hprp.CodeCommandExecutionFailed, Message: "本地命令未返回结果", Retryable: false},
		}
		client.resultCache.Store(command, cachedCommandResult{result: failed})
		_ = current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), requestID, false, failed)
		return
	}
	client.resultCache.Store(command, cachedCommandResult{result: *result, outputs: outputs})
	if err := current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), requestID, false, *result); err != nil {
		return
	}
	if !current.supportsCapability(hprp.CapabilityCommandOutputV1) {
		return
	}
	for index, output := range outputs {
		if err := current.write(current.ctx, hprp.TypeCommandOutput, randomClientID(), requestID, false, hprp.CommandOutput{
			Target: target, Sequence: uint64(index + 1), Final: index == len(outputs)-1,
			Content: output,
		}); err != nil {
			return
		}
	}
}

func (client *Client) writeCachedCommand(current *clientSession, requestID string, cached cachedCommandResult) {
	if err := current.write(current.ctx, hprp.TypeCommandResult, randomClientID(), requestID, false, cached.result); err != nil {
		return
	}
	if !current.supportsCapability(hprp.CapabilityCommandOutputV1) {
		return
	}
	target := cached.command.Target
	if cached.result.ReplacementTarget != nil {
		target = *cached.result.ReplacementTarget
	}
	for index, output := range cached.outputs {
		if err := current.write(current.ctx, hprp.TypeCommandOutput, randomClientID(), requestID, false, hprp.CommandOutput{
			Target: target, Sequence: uint64(index + 1), Final: index == len(cached.outputs)-1,
			Content: output,
		}); err != nil {
			return
		}
	}
}

func (client *Client) logCommandResult(requestID string, command hprp.CommandExecute, result string) {
	client.logger.Debug("HPRP 命令已处理",
		"credential_id", client.config.CredentialID,
		"request_hash", relayLogHash(requestID), "message_hash", relayLogHash(command.IdempotencyKey),
		"target_machine", command.Target.MachineID, "pane_id", command.Target.SlotID,
		"session_hash", relayLogHash(command.Target.SessionID), "content_length", len(command.Content.Text), "result", result,
	)
}

func rejectedCommand(code hprp.ErrorCode, message string) hprp.CommandResult {
	return hprp.CommandResult{Outcome: hprp.OutcomeRejected, Error: &hprp.Error{Code: code, Message: message, Retryable: false}}
}

func encodeTerminalContent(content im.TerminalContent) (hprp.Content, error) {
	mode := hprp.OutputMode(content.Mode)
	if mode != hprp.OutputModeText && mode != hprp.OutputModeImage {
		return hprp.Content{}, hprp.ErrInvalidMessage
	}
	capturedAt := content.CapturedAt.UTC()
	result := hprp.Content{
		Type: hprp.ContentTypeTerminal, Text: content.Text, Mode: mode, CapturedAt: &capturedAt,
	}
	if content.Page != nil {
		result.Page = &hprp.TerminalPage{Current: content.Page.Current, Total: content.Page.Total}
	}
	if content.Image != nil {
		result.Image = &hprp.TerminalImage{
			MediaType: content.Image.MediaType, Encoding: "base64",
			Data: base64.StdEncoding.EncodeToString(content.Image.Data), Width: content.Image.Width,
			Height: content.Image.Height, ColorMode: content.Image.ColorMode,
		}
	}
	if err := hprp.ValidateContent(result); err != nil {
		return hprp.Content{}, err
	}
	return result, nil
}

func cloneHPRPContents(contents []hprp.Content) []hprp.Content {
	result := make([]hprp.Content, len(contents))
	for index, content := range contents {
		result[index] = cloneHPRPContent(content)
	}
	return result
}

func cloneHPRPContent(content hprp.Content) hprp.Content {
	if content.Image != nil {
		imageCopy := *content.Image
		content.Image = &imageCopy
	}
	if content.Page != nil {
		pageCopy := *content.Page
		content.Page = &pageCopy
	}
	if content.CapturedAt != nil {
		capturedAtCopy := *content.CapturedAt
		content.CapturedAt = &capturedAtCopy
	}
	return content
}

func (client *Client) writeCurrent(ctx context.Context, messageType hprp.Type, id, replyTo string, mustUnderstand bool, payload any) error {
	current := client.currentSession()
	if current == nil {
		return ErrUnavailable
	}
	return current.write(ctx, messageType, id, replyTo, mustUnderstand, payload)
}

func (client *Client) currentSession() *clientSession {
	client.currentMu.RLock()
	defer client.currentMu.RUnlock()
	return client.current
}

func (client *Client) localExecutor() Executor {
	client.executorMu.RLock()
	defer client.executorMu.RUnlock()
	return client.executor
}

func (client *Client) selectedTarget() (hprp.Target, error) {
	current := client.currentSession()
	if current == nil {
		return hprp.Target{}, ErrUnavailable
	}
	selected, err := client.localExecutor().SelectedTarget()
	if err != nil {
		return hprp.Target{}, err
	}
	return client.resolveLocalTarget(current.machineID, selected.PaneID, selected.OccupantKey)
}

func (client *Client) resolveLocalTarget(machineID, paneID, sessionID string) (hprp.Target, error) {
	for _, current := range client.localExecutor().CurrentTargets() {
		if current.PaneID == paneID && current.OccupantKey == sessionID {
			return hprp.Target{MachineID: machineID, SlotID: paneID, SessionID: sessionID}, nil
		}
	}
	return hprp.Target{}, session.ErrListSnapshotExpired
}

func (client *Client) reportCurrentSnapshot(ctx context.Context) error {
	current := client.currentSession()
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

func (current *clientSession) write(ctx context.Context, messageType hprp.Type, id, replyTo string, mustUnderstand bool, payload any) error {
	envelope, err := hprp.NewEnvelope(messageType, id, replyTo, mustUnderstand, payload)
	if err != nil {
		return err
	}
	encoded, err := hprp.Encode(envelope)
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

func (current *clientSession) setCapabilities(capabilities []string) {
	current.capabilities = make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		current.capabilities[capability] = struct{}{}
	}
}

func (current *clientSession) supportsCapability(capability string) bool {
	_, supported := current.capabilities[capability]
	return supported
}

func (current *clientSession) request(ctx context.Context, messageType hprp.Type, id string, payload any, expected hprp.Type) (hprp.Envelope, error) {
	pending := clientPendingRequest{expected: expected, result: make(chan hprp.Envelope, 1)}
	current.pendingMu.Lock()
	if _, exists := current.pending[id]; exists {
		current.pendingMu.Unlock()
		return hprp.Envelope{}, hprp.ErrInvalidMessage
	}
	current.pending[id] = pending
	current.pendingMu.Unlock()
	defer current.removePending(id)
	if err := current.write(ctx, messageType, id, "", true, payload); err != nil {
		return hprp.Envelope{}, err
	}
	select {
	case response := <-pending.result:
		return response, nil
	case <-ctx.Done():
		return hprp.Envelope{}, ctx.Err()
	case <-current.ctx.Done():
		return hprp.Envelope{}, ErrUnavailable
	}
}

func (current *clientSession) deliver(envelope hprp.Envelope) bool {
	if envelope.ReplyTo == "" {
		return false
	}
	current.pendingMu.Lock()
	pending, exists := current.pending[envelope.ReplyTo]
	if !exists || pending.expected != envelope.Type {
		current.pendingMu.Unlock()
		return false
	}
	delete(current.pending, envelope.ReplyTo)
	current.pendingMu.Unlock()
	pending.result <- envelope
	return true
}

func (current *clientSession) removePending(id string) {
	current.pendingMu.Lock()
	delete(current.pending, id)
	current.pendingMu.Unlock()
}

func (current *clientSession) read(ctx context.Context) (hprp.Envelope, error) {
	messageType, data, err := current.connection.Read(ctx)
	if err != nil {
		return hprp.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return hprp.Envelope{}, hprp.ErrInvalidMessage
	}
	return hprp.Decode(data)
}

func (current *clientSession) close() {
	current.cancel()
	_ = current.connection.Close(websocket.StatusNormalClosure, "client session ended")
}

func validateSnapshotAcknowledgement(envelope hprp.Envelope, requestID string, sequence uint64) error {
	if envelope.Type == hprp.TypeProtocolError {
		return decodeHPRPProtocolError(envelope)
	}
	if envelope.Type != hprp.TypeSessionSnapshotResult || envelope.ReplyTo != requestID {
		return fmt.Errorf("%w: session.snapshot.result 关联无效", hprp.ErrInvalidMessage)
	}
	result, err := hprp.DecodePayload[hprp.SnapshotResult](envelope)
	if err != nil {
		return err
	}
	if result.Outcome != hprp.OutcomeOK || result.AppliedSequence != sequence || result.Error != nil {
		if result.Error != nil {
			return fmt.Errorf("HPRP 快照被拒绝: %s", result.Error.Code)
		}
		return fmt.Errorf("%w: 快照未确认", hprp.ErrInvalidMessage)
	}
	return nil
}

func decodeHPRPProtocolError(envelope hprp.Envelope) error {
	payload, err := hprp.DecodePayload[hprp.ProtocolError](envelope)
	if err != nil {
		return err
	}
	return &remoteProtocolError{payload: payload}
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
	case relayHTTPStatus(err) != 0:
		return "http_status"
	case relayProtocolError(err) != nil:
		return "protocol_error"
	case errors.Is(err, hprp.ErrProtocolMismatch), errors.Is(err, hprp.ErrInvalidMessage), errors.Is(err, hprp.ErrMessageTooLarge):
		return "protocol"
	case websocket.CloseStatus(err) != -1:
		return "remote_close"
	default:
		return "transport"
	}
}

func relayErrorLogArgs(err error, rawURL, secret string) []any {
	stage := "session"
	var staged *relayStageError
	if errors.As(err, &staged) && staged.stage != "" {
		stage = staged.stage
	}
	args := []any{"stage", stage, "error_type", relayErrorType(err), "reason", safeRelayReason(err, rawURL, secret)}
	if protocolError := relayProtocolError(err); protocolError != nil {
		args = append(args, "error_code", protocolError.payload.Error.Code, "close_connection", protocolError.payload.Close)
		if message := safeRelayLogValue(redactRelaySensitive(protocolError.payload.Error.Message, rawURL, secret), 240); message != "" {
			args = append(args, "server_message", message)
		}
	}
	if statusCode := relayHTTPStatus(err); statusCode != 0 {
		args = append(args, "status_code", statusCode)
	}
	if status := websocket.CloseStatus(err); status != -1 {
		args = append(args, "close_status", status)
	}
	return args
}

func relayProtocolError(err error) *remoteProtocolError {
	var protocolError *remoteProtocolError
	if errors.As(err, &protocolError) {
		return protocolError
	}
	return nil
}

func relayHTTPStatus(err error) int {
	var statusError *relayHTTPStatusError
	if errors.As(err, &statusError) {
		return statusError.statusCode
	}
	return 0
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

func safeRelayReason(err error, rawURL, secret string) string {
	if err == nil {
		return "未提供断开原因"
	}
	return safeRelayLogValue(redactRelaySensitive(err.Error(), rawURL, secret), 512)
}

func redactRelaySensitive(value, rawURL, secret string) string {
	if secret = strings.TrimSpace(secret); secret != "" {
		value = strings.ReplaceAll(value, secret, "[relay-key]")
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
