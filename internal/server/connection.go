package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/relayproto"
)

type pendingRequest struct {
	expected relayproto.Type
	result   chan relayproto.Frame
}

type clientConnection struct {
	id     string
	key    ClientKey
	socket *websocket.Conn
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	sendQueue     chan relayproto.Frame
	writerDone    chan struct{}
	outboundQueue chan relayproto.Frame
	outboundDone  chan struct{}
	ready         atomic.Bool
	lastPong      atomic.Int64
	sessionCount  atomic.Int64

	pendingMu  sync.Mutex
	pending    map[string]pendingRequest
	maxPending int
}

func newClientConnection(parent context.Context, id string, key ClientKey, socket *websocket.Conn, sendCapacity, maxPending int, logger *slog.Logger) *clientConnection {
	ctx, cancel := context.WithCancel(parent)
	connection := &clientConnection{
		id: id, key: key, socket: socket, logger: logger, ctx: ctx, cancel: cancel,
		sendQueue: make(chan relayproto.Frame, sendCapacity), writerDone: make(chan struct{}),
		outboundQueue: make(chan relayproto.Frame, sendCapacity), outboundDone: make(chan struct{}),
		pending: make(map[string]pendingRequest), maxPending: maxPending,
	}
	connection.lastPong.Store(time.Now().UnixNano())
	return connection
}

func (connection *clientConnection) runWriter() {
	defer close(connection.writerDone)
	for {
		select {
		case <-connection.ctx.Done():
			return
		case frame := <-connection.sendQueue:
			encoded, err := relayproto.Encode(frame)
			if err != nil {
				if connection.logger != nil {
					args := append(connectionLogArgs(connection), "event_type", frame.Type, "stage", "encode_frame")
					connection.logger.Warn("Relay 发送队列帧编码失败", append(args, serverErrorLogArgs(err)...)...)
				}
				connection.cancel()
				return
			}
			if err := connection.socket.Write(connection.ctx, websocket.MessageText, encoded); err != nil {
				if connection.logger != nil {
					args := append(connectionLogArgs(connection), "event_type", frame.Type, "stage", "write_frame")
					connection.logger.Warn("Relay WebSocket 帧发送失败", append(args, serverErrorLogArgs(err)...)...)
				}
				connection.cancel()
				return
			}
		}
	}
}

func (connection *clientConnection) send(ctx context.Context, frame relayproto.Frame) error {
	if connection == nil || !connection.ready.Load() && frame.Type != relayproto.TypeServerHello {
		return ErrClientUnavailable
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-connection.ctx.Done():
		return ErrClientUnavailable
	case connection.sendQueue <- frame:
		return nil
	default:
		return ErrClientQueueFull
	}
}

func (connection *clientConnection) request(ctx context.Context, frame relayproto.Frame, expected relayproto.Type) (relayproto.Frame, error) {
	if frame.RequestID == "" {
		return relayproto.Frame{}, ErrInvalidHubRequest
	}
	request := pendingRequest{expected: expected, result: make(chan relayproto.Frame, 1)}
	connection.pendingMu.Lock()
	if len(connection.pending) >= connection.maxPending {
		connection.pendingMu.Unlock()
		return relayproto.Frame{}, ErrInflightFull
	}
	if _, exists := connection.pending[frame.RequestID]; exists {
		connection.pendingMu.Unlock()
		return relayproto.Frame{}, ErrInvalidHubRequest
	}
	connection.pending[frame.RequestID] = request
	connection.pendingMu.Unlock()
	defer connection.removePending(frame.RequestID)
	if err := connection.send(ctx, frame); err != nil {
		return relayproto.Frame{}, err
	}
	select {
	case <-ctx.Done():
		return relayproto.Frame{}, ctx.Err()
	case <-connection.ctx.Done():
		return relayproto.Frame{}, ErrClientUnavailable
	case response := <-request.result:
		return response, nil
	}
}

func (connection *clientConnection) deliver(frame relayproto.Frame) bool {
	if frame.RequestID == "" {
		return false
	}
	connection.pendingMu.Lock()
	request, exists := connection.pending[frame.RequestID]
	if !exists || request.expected != frame.Type {
		connection.pendingMu.Unlock()
		return false
	}
	delete(connection.pending, frame.RequestID)
	connection.pendingMu.Unlock()
	request.result <- frame
	return true
}

func (connection *clientConnection) removePending(requestID string) {
	connection.pendingMu.Lock()
	delete(connection.pending, requestID)
	connection.pendingMu.Unlock()
}

func (connection *clientConnection) close(code websocket.StatusCode, reason string) {
	if connection == nil {
		return
	}
	connection.cancel()
	_ = connection.socket.Close(code, reason)
	<-connection.writerDone
}

func mapSelectResult(result relayproto.SelectResult) error {
	if result.OK {
		return nil
	}
	switch result.Code {
	case relayproto.CodeTargetChanged, relayproto.CodeTargetNotFound:
		return ErrTargetChanged
	case relayproto.CodeClientUnavailable:
		return ErrClientUnavailable
	case relayproto.CodeQueueFull:
		return ErrClientQueueFull
	case relayproto.CodeRequestTimeout:
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("%w: 客户端拒绝请求", ErrClientUnavailable)
	}
}

func connectionReadFrame(ctx context.Context, socket *websocket.Conn) (relayproto.Frame, error) {
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		return relayproto.Frame{}, err
	}
	if messageType != websocket.MessageText {
		return relayproto.Frame{}, fmt.Errorf("%w: 只接受文本帧", relayproto.ErrInvalidFrame)
	}
	return relayproto.Decode(data)
}

func isNormalSocketEnd(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || errors.Is(err, context.Canceled)
}
