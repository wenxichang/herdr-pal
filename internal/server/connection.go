package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

type pendingRequest struct {
	expected hprp.Type
	target   *hprp.Target
	result   chan hprp.Envelope
}

type commandRoute struct {
	target       hprp.Target
	nextSequence uint64
	expiresAt    time.Time
}

type notificationRecord struct {
	target    hprp.Target
	sequence  uint64
	expiresAt time.Time
}

type clientConnection struct {
	id           string
	credentialID uint64
	key          ClientKey
	socket       *websocket.Conn
	logger       *slog.Logger
	ctx          context.Context
	cancel       context.CancelFunc

	sendQueue     chan hprp.Envelope
	writerDone    chan struct{}
	outboundQueue chan hprp.Envelope
	outboundDone  chan struct{}
	ready         atomic.Bool
	sessionCount  atomic.Int64

	metadataMu       sync.RWMutex
	implementation   hprp.Implementation
	source           netip.Addr
	connectedAt      time.Time
	lastHeartbeatAt  time.Time
	lastSnapshotAt   time.Time
	snapshotSequence uint64
	capabilities     map[string]struct{}

	pendingMu  sync.Mutex
	pending    map[string]pendingRequest
	maxPending int

	commandMu     sync.Mutex
	commandRoutes map[string]commandRoute
	maxCommands   int

	notificationMu       sync.Mutex
	notifications        map[string]notificationRecord
	notificationSequence map[hprp.Target]uint64
}

type clientConnectionConfig struct {
	ID             string
	CredentialID   uint64
	Key            ClientKey
	Source         netip.Addr
	Implementation hprp.Implementation
	Socket         *websocket.Conn
	SendCapacity   int
	MaxPending     int
	Logger         *slog.Logger
}

func newClientConnection(parent context.Context, config clientConnectionConfig) *clientConnection {
	ctx, cancel := context.WithCancel(parent)
	return &clientConnection{
		id: config.ID, credentialID: config.CredentialID, key: config.Key, socket: config.Socket, logger: config.Logger, ctx: ctx, cancel: cancel,
		sendQueue: make(chan hprp.Envelope, config.SendCapacity), writerDone: make(chan struct{}),
		outboundQueue: make(chan hprp.Envelope, config.SendCapacity), outboundDone: make(chan struct{}),
		pending: make(map[string]pendingRequest), maxPending: config.MaxPending,
		commandRoutes: make(map[string]commandRoute), maxCommands: defaultCommandRouteCapacity,
		notifications: make(map[string]notificationRecord), notificationSequence: make(map[hprp.Target]uint64),
		capabilities: make(map[string]struct{}), implementation: config.Implementation, source: normalizeConnectionSource(config.Source), connectedAt: time.Now().UTC(),
	}
}

func (connection *clientConnection) setCapabilities(capabilities []string) {
	connection.metadataMu.Lock()
	defer connection.metadataMu.Unlock()
	for _, capability := range capabilities {
		connection.capabilities[capability] = struct{}{}
	}
}

func (connection *clientConnection) supportsCapability(capability string) bool {
	connection.metadataMu.RLock()
	defer connection.metadataMu.RUnlock()
	_, supported := connection.capabilities[capability]
	return supported
}

func (connection *clientConnection) confirmsSnapshot(sequence uint64) bool {
	connection.metadataMu.RLock()
	defer connection.metadataMu.RUnlock()
	return sequence > 0 && sequence <= connection.snapshotSequence
}

func (connection *clientConnection) recordSnapshot(sequence uint64, sessionCount int, observedAt time.Time) {
	connection.sessionCount.Store(int64(sessionCount))
	connection.metadataMu.Lock()
	connection.snapshotSequence = sequence
	connection.lastSnapshotAt = observedAt.UTC()
	connection.metadataMu.Unlock()
}

func (connection *clientConnection) recordHeartbeat(observedAt time.Time) {
	connection.metadataMu.Lock()
	connection.lastHeartbeatAt = observedAt.UTC()
	connection.metadataMu.Unlock()
}

func (connection *clientConnection) view() ConnectionView {
	connection.metadataMu.RLock()
	capabilities := make([]string, 0, len(connection.capabilities))
	for capability := range connection.capabilities {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	view := ConnectionView{
		ConnectionID: connection.id, CredentialID: connection.credentialID,
		PrincipalID: connection.key.UserID, MachineID: connection.key.MachineID,
		Implementation: connection.implementation, ConnectedAt: connection.connectedAt,
		LastHeartbeatAt: connection.lastHeartbeatAt, LastSnapshotAt: connection.lastSnapshotAt,
		SnapshotSequence: connection.snapshotSequence, SessionCount: int(connection.sessionCount.Load()),
		Capabilities: capabilities, Ready: connection.ready.Load(),
	}
	if connection.source.IsValid() {
		view.SourceIP = connection.source.String()
	}
	connection.metadataMu.RUnlock()
	return view
}

func normalizeConnectionSource(source netip.Addr) netip.Addr {
	if !source.IsValid() {
		return netip.Addr{}
	}
	if source.Zone() != "" {
		source = source.WithZone("")
	}
	return source.Unmap()
}

func (connection *clientConnection) acceptNotification(event hprp.NotificationEvent) bool {
	connection.notificationMu.Lock()
	defer connection.notificationMu.Unlock()
	now := time.Now()
	for eventKey, record := range connection.notifications {
		if !now.Before(record.expiresAt) {
			delete(connection.notifications, eventKey)
		}
	}
	if _, exists := connection.notifications[event.EventKey]; exists {
		return false
	}
	if last := connection.notificationSequence[event.Target]; event.Sequence <= last {
		return false
	}
	if len(connection.notifications) >= defaultNotificationDedupeCapacity {
		connection.evictOldestNotificationLocked()
	}
	connection.notifications[event.EventKey] = notificationRecord{
		target: event.Target, sequence: event.Sequence, expiresAt: now.Add(defaultNotificationDedupeTTL),
	}
	connection.notificationSequence[event.Target] = event.Sequence
	return true
}

func (connection *clientConnection) evictOldestNotificationLocked() {
	var oldestKey string
	var oldestExpiry time.Time
	for eventKey, record := range connection.notifications {
		if oldestKey == "" || record.expiresAt.Before(oldestExpiry) {
			oldestKey = eventKey
			oldestExpiry = record.expiresAt
		}
	}
	delete(connection.notifications, oldestKey)
}

func (connection *clientConnection) runWriter() {
	defer close(connection.writerDone)
	for {
		select {
		case <-connection.ctx.Done():
			return
		case envelope := <-connection.sendQueue:
			encoded, err := hprp.Encode(envelope)
			if err != nil {
				if connection.logger != nil {
					args := append(connectionLogArgs(connection), "event_type", envelope.Type, "stage", "encode_message")
					connection.logger.Warn("HPRP 发送队列消息编码失败", append(args, serverErrorLogArgs(err)...)...)
				}
				connection.cancel()
				return
			}
			if err := connection.socket.Write(connection.ctx, websocket.MessageText, encoded); err != nil {
				if connection.logger != nil {
					args := append(connectionLogArgs(connection), "event_type", envelope.Type, "stage", "write_message")
					connection.logger.Warn("HPRP WebSocket 消息发送失败", append(args, serverErrorLogArgs(err)...)...)
				}
				connection.cancel()
				return
			}
		}
	}
}

func (connection *clientConnection) send(ctx context.Context, envelope hprp.Envelope) error {
	if connection == nil {
		return ErrClientUnavailable
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-connection.ctx.Done():
		return ErrClientUnavailable
	case connection.sendQueue <- envelope:
		return nil
	default:
		return ErrClientQueueFull
	}
}

func (connection *clientConnection) request(ctx context.Context, envelope hprp.Envelope, expected hprp.Type) (hprp.Envelope, error) {
	return connection.requestTarget(ctx, envelope, expected, nil)
}

func (connection *clientConnection) requestTarget(
	ctx context.Context,
	envelope hprp.Envelope,
	expected hprp.Type,
	target *hprp.Target,
) (hprp.Envelope, error) {
	if connection == nil || !connection.ready.Load() || envelope.ID == "" || envelope.ReplyTo != "" {
		return hprp.Envelope{}, ErrInvalidHubRequest
	}
	request := pendingRequest{expected: expected, result: make(chan hprp.Envelope, 1)}
	if target != nil {
		targetCopy := *target
		request.target = &targetCopy
	}
	connection.pendingMu.Lock()
	if len(connection.pending) >= connection.maxPending {
		connection.pendingMu.Unlock()
		return hprp.Envelope{}, ErrInflightFull
	}
	if _, exists := connection.pending[envelope.ID]; exists {
		connection.pendingMu.Unlock()
		return hprp.Envelope{}, ErrInvalidHubRequest
	}
	connection.pending[envelope.ID] = request
	connection.pendingMu.Unlock()
	defer connection.removePending(envelope.ID)
	if err := connection.send(ctx, envelope); err != nil {
		return hprp.Envelope{}, err
	}
	select {
	case <-ctx.Done():
		return hprp.Envelope{}, ctx.Err()
	case <-connection.ctx.Done():
		return hprp.Envelope{}, ErrClientUnavailable
	case response := <-request.result:
		return response, nil
	}
}

func (connection *clientConnection) deliverTarget(envelope hprp.Envelope, target hprp.Target) (bool, error) {
	if envelope.ReplyTo == "" {
		return false, ErrInvalidHubRequest
	}
	connection.pendingMu.Lock()
	request, exists := connection.pending[envelope.ReplyTo]
	if !exists || request.expected != envelope.Type {
		connection.pendingMu.Unlock()
		return false, ErrInvalidHubRequest
	}
	if request.target == nil || *request.target != target {
		connection.pendingMu.Unlock()
		return false, ErrTargetChanged
	}
	delete(connection.pending, envelope.ReplyTo)
	connection.pendingMu.Unlock()
	request.result <- envelope
	return true, nil
}

func (connection *clientConnection) deliver(envelope hprp.Envelope) bool {
	if envelope.ReplyTo == "" {
		return false
	}
	connection.pendingMu.Lock()
	request, exists := connection.pending[envelope.ReplyTo]
	if !exists || request.expected != envelope.Type {
		connection.pendingMu.Unlock()
		return false
	}
	delete(connection.pending, envelope.ReplyTo)
	connection.pendingMu.Unlock()
	request.result <- envelope
	return true
}

func (connection *clientConnection) removePending(requestID string) {
	connection.pendingMu.Lock()
	delete(connection.pending, requestID)
	connection.pendingMu.Unlock()
}

func (connection *clientConnection) registerCommand(commandID string, target hprp.Target, ttl time.Duration) error {
	if commandID == "" || hprp.ValidateTarget(target) != nil {
		return ErrInvalidHubRequest
	}
	connection.commandMu.Lock()
	defer connection.commandMu.Unlock()
	connection.pruneCommandsLocked(time.Now())
	if len(connection.commandRoutes) >= connection.maxCommands {
		return ErrInflightFull
	}
	if _, exists := connection.commandRoutes[commandID]; exists {
		return ErrInvalidHubRequest
	}
	connection.commandRoutes[commandID] = commandRoute{target: target, nextSequence: 1, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (connection *clientConnection) removeCommand(commandID string) {
	connection.commandMu.Lock()
	delete(connection.commandRoutes, commandID)
	connection.commandMu.Unlock()
}

func (connection *clientConnection) replaceCommandTarget(commandID string, target hprp.Target) error {
	if hprp.ValidateTarget(target) != nil {
		return ErrTargetChanged
	}
	connection.commandMu.Lock()
	defer connection.commandMu.Unlock()
	route, exists := connection.commandRoutes[commandID]
	if !exists || route.target.MachineID != target.MachineID || route.target.SlotID != target.SlotID {
		return ErrTargetChanged
	}
	route.target = target
	route.expiresAt = time.Now().Add(defaultCommandRouteTTL)
	connection.commandRoutes[commandID] = route
	return nil
}

func (connection *clientConnection) acceptCommandOutput(envelope hprp.Envelope, output hprp.CommandOutput) (bool, error) {
	if !connection.supportsCapability(hprp.CapabilityCommandOutputV1) {
		return false, fmt.Errorf("%w: command.output.v1 未协商", hprp.ErrInvalidMessage)
	}
	if envelope.ReplyTo == "" {
		return false, ErrInvalidHubRequest
	}
	connection.commandMu.Lock()
	defer connection.commandMu.Unlock()
	now := time.Now()
	connection.pruneCommandsLocked(now)
	route, exists := connection.commandRoutes[envelope.ReplyTo]
	if !exists || route.target != output.Target {
		return false, ErrTargetChanged
	}
	if output.Sequence < route.nextSequence {
		return false, nil
	}
	if output.Sequence > route.nextSequence {
		return false, fmt.Errorf("%w: command.output sequence 不连续", hprp.ErrInvalidMessage)
	}
	if output.Final {
		delete(connection.commandRoutes, envelope.ReplyTo)
	} else {
		route.nextSequence++
		route.expiresAt = now.Add(defaultCommandRouteTTL)
		connection.commandRoutes[envelope.ReplyTo] = route
	}
	return true, nil
}

func (connection *clientConnection) pruneCommandsLocked(now time.Time) {
	for commandID, route := range connection.commandRoutes {
		if !now.Before(route.expiresAt) {
			delete(connection.commandRoutes, commandID)
		}
	}
}

func (connection *clientConnection) close(code websocket.StatusCode, reason string) {
	if connection == nil {
		return
	}
	connection.cancel()
	_ = connection.socket.Close(code, reason)
	<-connection.writerDone
}

func connectionReadEnvelope(ctx context.Context, socket *websocket.Conn) (hprp.Envelope, error) {
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		return hprp.Envelope{}, err
	}
	if messageType != websocket.MessageText {
		return hprp.Envelope{}, fmt.Errorf("%w: HPRP/1 只接受文本消息", hprp.ErrInvalidMessage)
	}
	return hprp.Decode(data)
}

func isNormalSocketEnd(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || errors.Is(err, context.Canceled)
}
