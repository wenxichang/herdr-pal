package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

var lifecycleSubscriptionTypes = []string{
	"pane.created",
	"pane.closed",
	"pane.updated",
	"pane.exited",
	"pane.agent_detected",
}

var allowedSubscriptionTypes = map[string]bool{
	"pane.created":              true,
	"pane.closed":               true,
	"pane.updated":              true,
	"pane.exited":               true,
	"pane.agent_detected":       true,
	"pane.agent_status_changed": true,
}

// LifecycleSubscriptions 返回 bridge 用于跟踪 pane 生命周期的标准订阅列表。
func LifecycleSubscriptions() []SubscriptionSpec {
	specs := make([]SubscriptionSpec, len(lifecycleSubscriptionTypes))
	for index, eventType := range lifecycleSubscriptionTypes {
		specs[index] = SubscriptionSpec{Type: eventType}
	}
	return specs
}

// StatusSubscriptions 为非空 pane ID 创建稳定排序且去重的状态订阅列表。
func StatusSubscriptions(paneIDs []string) []SubscriptionSpec {
	unique := make(map[string]struct{}, len(paneIDs))
	for _, paneID := range paneIDs {
		if strings.TrimSpace(paneID) != "" {
			unique[paneID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(unique))
	for paneID := range unique {
		ids = append(ids, paneID)
	}
	sort.Strings(ids)
	specs := make([]SubscriptionSpec, len(ids))
	for index, paneID := range ids {
		specs[index] = SubscriptionSpec{Type: "pane.agent_status_changed", PaneID: paneID}
	}
	return specs
}

type subscribeParams struct {
	Subscriptions []SubscriptionSpec `json:"subscriptions"`
}

type subscriptionStartedResult struct {
	Type string `json:"type"`
}

// Subscribe 建立独立的 Herdr 事件订阅连接，并在确认成功后返回事件流。
func (c *Client) Subscribe(ctx context.Context, specs []SubscriptionSpec) (SubscriptionStream, error) {
	if err := validateSubscriptionSpecs(specs); err != nil {
		return nil, err
	}
	request := requestEnvelope{
		ID:     fmt.Sprintf("pal:%d", c.nextID.Add(1)),
		Method: "events.subscribe",
		Params: subscribeParams{Subscriptions: specs},
	}
	encodedRequest, err := encodeRequest(request)
	if err != nil {
		return nil, err
	}

	handshakeContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	conn, err := c.dialer.DialContext(handshakeContext, "unix", c.socketPath)
	if err != nil {
		return nil, unavailableContextError(handshakeContext, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = conn.Close()
		}
	}()

	deadline, _ := handshakeContext.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, unavailableContextError(handshakeContext, err)
	}
	stopHandshakeClose := context.AfterFunc(handshakeContext, func() {
		_ = conn.Close()
	})
	defer stopHandshakeClose()
	if err := writeFrame(conn, encodedRequest); err != nil {
		return nil, unavailableContextError(handshakeContext, err)
	}
	reader := bufio.NewReader(conn)
	line, err := readLine(reader)
	if err != nil {
		if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrProtocol) && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return nil, unavailableContextError(handshakeContext, err)
	}
	var acknowledgement subscriptionStartedResult
	if err := parseResponse(line, request.ID, &acknowledgement); err != nil {
		return nil, err
	}
	if err := validateResultType(acknowledgement.Type, "subscription_started"); err != nil {
		return nil, err
	}
	if !stopHandshakeClose() {
		if err := handshakeContext.Err(); err != nil {
			return nil, unavailableContextError(handshakeContext, err)
		}
		return nil, unavailableError(net.ErrClosed)
	}
	if err := conn.SetDeadline(timeZero); err != nil {
		return nil, unavailableContextError(handshakeContext, err)
	}

	stream := &subscriptionStream{conn: conn, reader: reader, closeDone: make(chan struct{})}
	stopParent := context.AfterFunc(ctx, func() {
		_ = stream.Close()
	})
	stream.stateMu.Lock()
	closed := stream.closed
	parentErr := ctx.Err()
	if !closed {
		stream.stopParent = stopParent
	}
	stream.stateMu.Unlock()
	if closed || parentErr != nil {
		stopParent()
		if closed {
			<-stream.closeDone
		}
		return nil, unavailableContextError(ctx, net.ErrClosed)
	}
	closeOnError = false
	return stream, nil
}

var timeZero = time.Time{}

func validateSubscriptionSpecs(specs []SubscriptionSpec) error {
	if len(specs) == 0 {
		return protocolError("事件订阅不能为空")
	}
	for _, spec := range specs {
		if strings.TrimSpace(spec.Type) == "" {
			return protocolError("事件订阅 type 不能为空")
		}
		if !allowedSubscriptionTypes[spec.Type] {
			return protocolError("事件订阅 type 不受当前 bridge 支持")
		}
		if spec.Type == "pane.agent_status_changed" && strings.TrimSpace(spec.PaneID) == "" {
			return protocolError("pane.agent_status_changed 订阅必须指定 pane_id")
		}
		if spec.Type != "pane.agent_status_changed" && spec.PaneID != "" {
			return protocolError("pane 生命周期订阅不能指定 pane_id")
		}
	}
	return nil
}

type subscriptionStream struct {
	conn   net.Conn
	reader *bufio.Reader

	readMu     sync.Mutex
	stateMu    sync.Mutex
	closed     bool
	closeErr   error
	closeDone  chan struct{}
	stopParent func() bool
}

func (s *subscriptionStream) Recv(ctx context.Context) (Event, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.isClosed() {
		return Event{}, unavailableError(net.ErrClosed)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := s.conn.SetDeadline(deadline); err != nil {
			return Event{}, unavailableContextError(ctx, err)
		}
	} else if err := s.conn.SetDeadline(timeZero); err != nil {
		return Event{}, unavailableContextError(ctx, err)
	}
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = s.Close()
	})
	line, readErr := readLine(s.reader)
	stopCancellation()
	clearErr := s.conn.SetDeadline(timeZero)
	if contextErr := ctx.Err(); contextErr != nil {
		_ = s.Close()
		if readErr == nil {
			readErr = contextErr
		}
		return Event{}, unavailableContextError(ctx, readErr)
	}
	if clearErr != nil {
		return Event{}, unavailableContextError(ctx, clearErr)
	}
	if readErr != nil {
		if errors.Is(readErr, ErrFrameTooLarge) || errors.Is(readErr, ErrProtocol) && !errors.Is(readErr, io.EOF) {
			return Event{}, readErr
		}
		return Event{}, unavailableContextError(ctx, readErr)
	}
	return parseEvent(line)
}

func (s *subscriptionStream) Close() error {
	s.stateMu.Lock()
	if s.closed {
		done := s.closeDone
		s.stateMu.Unlock()
		<-done
		s.stateMu.Lock()
		err := s.closeErr
		s.stateMu.Unlock()
		return err
	}
	s.closed = true
	stopParent := s.stopParent
	s.stopParent = nil
	s.stateMu.Unlock()

	if stopParent != nil {
		stopParent()
	}
	err := s.conn.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		err = unavailableError(err)
	} else {
		err = nil
	}
	s.stateMu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.stateMu.Unlock()
	return err
}

func (s *subscriptionStream) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

type eventEnvelope struct {
	Event *string         `json:"event"`
	Data  json.RawMessage `json:"data"`
}

func parseEvent(line []byte) (Event, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return Event{}, fmt.Errorf("%w: 解码事件: %w", ErrProtocol, err)
	}
	if envelope.Event == nil || strings.TrimSpace(*envelope.Event) == "" {
		return Event{}, protocolError("事件缺少 event")
	}
	data := bytes.TrimSpace(envelope.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) || !bytes.HasPrefix(data, []byte("{")) {
		return Event{}, protocolError("事件缺少对象 data")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return Event{}, fmt.Errorf("%w: 解码事件 data: %w", ErrProtocol, err)
	}
	return Event{Kind: *envelope.Event, Data: append(json.RawMessage(nil), envelope.Data...)}, nil
}

// DecodeAgentStatusEvent 将 pane.agent_status_changed 原始事件转换为严格的状态事件。
func DecodeAgentStatusEvent(event Event) (AgentStatusEvent, error) {
	if event.Kind != "pane.agent_status_changed" {
		return AgentStatusEvent{}, protocolError("事件不是 pane.agent_status_changed")
	}
	var wire struct {
		PaneID       *string         `json:"pane_id"`
		WorkspaceID  *string         `json:"workspace_id"`
		AgentStatus  *string         `json:"agent_status"`
		Agent        *string         `json:"agent"`
		Title        *string         `json:"title"`
		DisplayAgent *string         `json:"display_agent"`
		StateLabels  json.RawMessage `json:"state_labels"`
	}
	if err := json.Unmarshal(event.Data, &wire); err != nil {
		return AgentStatusEvent{}, fmt.Errorf("%w: 解码状态事件: %w", ErrProtocol, err)
	}
	if wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.AgentStatus == nil {
		return AgentStatusEvent{}, protocolError("状态事件缺少必填字段")
	}
	status, err := agentStatusFromWire(wire.AgentStatus)
	if err != nil {
		return AgentStatusEvent{}, err
	}
	labels := make(map[string]string)
	if len(wire.StateLabels) != 0 {
		if err := json.Unmarshal(wire.StateLabels, &labels); err != nil {
			return AgentStatusEvent{}, fmt.Errorf("%w: 状态事件 state_labels 必须为字符串映射: %w", ErrProtocol, err)
		}
		if labels == nil {
			return AgentStatusEvent{}, protocolError("状态事件 state_labels 必须为对象")
		}
	}
	return AgentStatusEvent{
		PaneID: *wire.PaneID, WorkspaceID: *wire.WorkspaceID, AgentStatus: status,
		Agent: wire.Agent, Title: wire.Title, DisplayAgent: wire.DisplayAgent, StateLabels: labels,
	}, nil
}
