package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	// muxStreamID 是待答事件使用的固定流 ID。
	muxStreamID = "events"
	// muxOpenMessage 是订阅 $events 的固定打开帧。
	muxOpenMessage = `{"type":"open","streamId":"events","endpoint":"$events","payload":{"args":{}}}`
	// muxMaxReadBytes 限制单条 WebSocket 消息大小，与 RPC 响应上限一致。
	muxMaxReadBytes = 4 << 20
	// muxMaxPendingItems 限制待答表条数，防止异常宿主耗尽内存。
	muxMaxPendingItems = 256
	// muxMaxItemBytes 限制单条待答事件的原始 JSON 大小。
	muxMaxItemBytes = 128 << 10
	// muxMaxPendingBytes 限制待答表的总原始 JSON 占用。
	muxMaxPendingBytes = 1 << 20
	// muxBackoffInitial 是断线后的首次重连延迟。
	muxBackoffInitial = time.Second
	// muxBackoffMax 是重连延迟上限。
	muxBackoffMax = 30 * time.Second
	// muxCancelWaitDefault 是提交答案后等待 cancel 的默认上限。
	muxCancelWaitDefault = 2 * time.Second
	// muxIdentifierMaxBytes 限制 clientId/eventId/agentId 长度。
	muxIdentifierMaxBytes = 512
	// muxCanceledHistory 限制用于立即确认 cancel 的历史记录条数。
	muxCanceledHistory = 256
)

var (
	// errMuxUnavailable 表示 Mux 尚未就绪或已断开，待答表不可用。
	errMuxUnavailable = errors.New("dsh Mux 尚未就绪")
	// errMuxProtocol 表示 dsh 发送了无法识别的 Mux 帧；必须失败关闭而不是猜测兼容。
	errMuxProtocol = errors.New("dsh Mux 协议错误")
	// errMuxCapacity 表示待答表超过条数上限。
	errMuxCapacity = fmt.Errorf("%w: 待答事件超过 %d 条", errMuxProtocol, muxMaxPendingItems)
	// errMuxItemTooLarge 表示单条待答事件超过大小上限。
	errMuxItemTooLarge = fmt.Errorf("%w: 单条待答事件超过 %d KiB", errMuxProtocol, muxMaxItemBytes>>10)
	// errMuxTotalTooLarge 表示待答表总量超过内存预算。
	errMuxTotalTooLarge = fmt.Errorf("%w: 待答事件总量超过 %d KiB", errMuxProtocol, muxMaxPendingBytes>>10)
	// errMuxIdentifier 表示本地待答事件标识无效。
	errMuxIdentifier = errors.New("dsh 待答事件标识无效")
	// errMuxAlreadyRunning 表示同一个 Mux 被并发运行。
	errMuxAlreadyRunning = errors.New("dsh Mux 已在运行")
	// errMuxNonTextMessage 表示订阅流上出现二进制消息。
	errMuxNonTextMessage = fmt.Errorf("%w: 收到非文本帧", errMuxProtocol)
)

// PendingItem 是一条等待用户回答的 dsh 原生交互。
//
// Request 保留原始 request 载荷供决策解析；Raw 保留完整 waterfall value，
// 用于构造 revision 并识别重复重放。
type PendingItem struct {
	// EventID 是 dsh 分配给该交互的事件 ID。
	EventID string
	// AgentID 是产生该交互的 Agent。
	AgentID string
	// Event 是原生事件名，例如 approval/request。
	Event string
	// Request 是原始 request 载荷 JSON。
	Request json.RawMessage
	// Raw 是完整 waterfall value 的原始 JSON。
	Raw json.RawMessage
}

// PendingSnapshot 是某一时刻的待答表副本。
//
// ClientID 为空表示当前没有已就绪的 Mux 连接，此时不能提交审批答案。
type PendingSnapshot struct {
	// ClientID 是当前 Mux 连接由 dsh 分配的客户端 ID。
	ClientID string
	// Items 按事件到达顺序排列。
	Items []PendingItem
}

// muxCancelRecord 记录一次 cancel 的序号，使取消历史可以按 FIFO 淘汰。
type muxCancelRecord struct {
	id  string
	seq uint64
}

// Mux 维护 dsh $events WebSocket 订阅和本机待答表。
//
// Mux 断线后按指数退避重连；重连后的 ready 会换发新的 clientId 并清空旧的
// 待答表，因此审批提交必须使用当前快照中的 clientId。
type Mux struct {
	host   Host
	auth   *rpcClient
	logger *slog.Logger
	dial   func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	wait   func(context.Context, time.Duration) error
	http   *http.Client

	// cancelWait 是 WaitCancel 的等待上限，测试可缩短。
	cancelWait time.Duration

	runMu   sync.Mutex
	running bool

	mu           sync.Mutex
	clientID     string
	items        map[string]PendingItem
	order        []string
	totalBytes   int
	canceled     map[string]uint64
	canceledRing []muxCancelRecord
	cancelSeq    uint64
	cancelSignal chan struct{}
}

// newMux 为宿主构造 Mux；此时不会建立连接，连接由 Run 维护。
//
// logger 可为 nil；Mux 只记录连接生命周期，不记录待答正文或认证材料。
func newMux(host Host, logger *slog.Logger) *Mux {
	return &Mux{
		host:       host,
		auth:       newRPCClient(host),
		logger:     logger,
		dial:       websocket.Dial,
		wait:       waitMuxRetry,
		cancelWait: muxCancelWaitDefault,
		http: &http.Client{
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		items:        make(map[string]PendingItem),
		canceled:     make(map[string]uint64),
		cancelSignal: make(chan struct{}),
	}
}

// Run 持续维护 $events WebSocket 订阅，直到 ctx 被取消或出现不可恢复错误。
//
// 连接级错误（拨号失败、断线）按 1s 起、最大 30s 的指数退避重连；协议错误
// 或宿主配置错误直接返回，由调用方重新发现宿主，不猜测兼容。
func (m *Mux) Run(ctx context.Context) error {
	if m == nil || ctx == nil {
		return errMuxUnavailable
	}
	m.runMu.Lock()
	if m.running {
		m.runMu.Unlock()
		return errMuxAlreadyRunning
	}
	m.running = true
	m.runMu.Unlock()
	defer func() {
		m.runMu.Lock()
		m.running = false
		m.runMu.Unlock()
	}()

	backoff := muxBackoffInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := m.runSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !isMuxRetryable(err) {
			return err
		}
		if ready {
			backoff = muxBackoffInitial
		}
		if m.logger != nil {
			if err != nil {
				m.logger.Warn("dsh Mux 连接已断开，准备重连", "retry_delay", backoff, "error", err)
			} else {
				m.logger.Warn("dsh Mux 连接已关闭，准备重连", "retry_delay", backoff)
			}
		}
		if waitErr := m.wait(ctx, backoff); waitErr != nil {
			return waitErr
		}
		backoff = min(backoff*2, muxBackoffMax)
	}
}

// isMuxRetryable 区分可重试的连接错误与必须失败关闭的协议/配置错误。
func isMuxRetryable(err error) bool {
	return !errors.Is(err, errMuxProtocol) &&
		!errors.Is(err, errRPCMissingHome) &&
		!errors.Is(err, errRPCHomeNotAbsolute)
}

// runSession 建立一次订阅并读取帧，直到连接断开。
//
// 返回值 ready 表示本次连接曾收到 ready；调用方据此在重连成功后重置退避。
// 无论以何种方式结束，都会清空待答表并广播等待者，避免沿用失效的 clientId。
func (m *Mux) runSession(ctx context.Context) (ready bool, err error) {
	credentialsPath, err := m.auth.credentialsPath()
	if err != nil {
		return false, err
	}
	cookie, err := m.auth.cookie(credentialsPath, m.auth.now())
	if err != nil {
		return false, fmt.Errorf("生成 dsh Mux 认证 Cookie 失败: %w", err)
	}

	dialContext, cancelDial := context.WithTimeout(ctx, rpcTimeout)
	defer cancelDial()
	endpoint := fmt.Sprintf("ws://127.0.0.1:%d/api/remote.mux", m.host.Port)
	connection, _, err := m.dial(dialContext, endpoint, &websocket.DialOptions{
		HTTPClient: m.http,
		HTTPHeader: http.Header{"Cookie": []string{cookie}},
	})
	if err != nil {
		return false, fmt.Errorf("连接 dsh Mux 失败: %w", err)
	}
	defer func() {
		// 直接关闭底层连接，避免 close 握手在退出或重连时阻塞。
		_ = connection.CloseNow()
		m.reset()
	}()

	connection.SetReadLimit(muxMaxReadBytes)
	if err := connection.Write(ctx, websocket.MessageText, []byte(muxOpenMessage)); err != nil {
		return false, fmt.Errorf("订阅 dsh $events 失败: %w", err)
	}

	for {
		messageType, data, err := connection.Read(ctx)
		if err != nil {
			return ready, fmt.Errorf("读取 dsh Mux 帧失败: %w", err)
		}
		if messageType != websocket.MessageText {
			return ready, errMuxNonTextMessage
		}
		if err := m.handleFrame(data); err != nil {
			return ready, err
		}
		if m.isReady() {
			ready = true
		}
	}
}

// muxFrame 是 Mux 顶层帧；只有 events 流的 item 帧会解析 value。
type muxFrame struct {
	StreamID string          `json:"streamId"`
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
}

// handleFrame 解析并应用一帧。
//
// 其他 streamId 的帧直接忽略；events 流上只接受 item 帧，未知帧类型、
// 未知事件类型和超限事件都返回 errMuxProtocol 的包装错误并终止订阅。
func (m *Mux) handleFrame(data []byte) error {
	var frame muxFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("%w: 帧不是合法 JSON", errMuxProtocol)
	}
	if frame.StreamID != muxStreamID {
		return nil
	}
	if frame.Type != "item" {
		return fmt.Errorf("%w: 未知帧类型 %q", errMuxProtocol, truncateRPCMessage(frame.Type))
	}
	if len(frame.Value) == 0 {
		return fmt.Errorf("%w: item 帧缺少 value", errMuxProtocol)
	}
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Value, &header); err != nil || header.Type == "" {
		return fmt.Errorf("%w: item value 格式无效", errMuxProtocol)
	}

	switch header.Type {
	case "ready":
		return m.handleReady(frame.Value)
	case "waterfall":
		return m.handleWaterfall(frame.Value)
	case "cancel":
		return m.handleCancel(frame.Value)
	case "emit":
		// emit 是状态事件；状态以 session/list 为准，这里只丢弃。
		return nil
	default:
		return fmt.Errorf("%w: 未知事件类型 %q", errMuxProtocol, truncateRPCMessage(header.Type))
	}
}

// handleReady 记录新的 clientId 并清空待答表和取消历史。
func (m *Mux) handleReady(raw json.RawMessage) error {
	var value struct {
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: ready 帧格式无效", errMuxProtocol)
	}
	if err := validateMuxIdentifier("clientId", value.ClientID); err != nil {
		return fmt.Errorf("%w: %v", errMuxProtocol, err)
	}

	m.mu.Lock()
	m.clientID = value.ClientID
	m.items = make(map[string]PendingItem)
	m.order = nil
	m.totalBytes = 0
	m.canceled = make(map[string]uint64)
	m.canceledRing = nil
	signal := m.replaceCancelSignalLocked()
	m.mu.Unlock()
	close(signal)
	return nil
}

// handleWaterfall 校验并写入一条待答事件；同一 eventId 的重放按更新处理。
func (m *Mux) handleWaterfall(raw json.RawMessage) error {
	var value struct {
		EventID string          `json:"eventId"`
		AgentID string          `json:"agentId"`
		Event   string          `json:"event"`
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: waterfall 帧格式无效", errMuxProtocol)
	}
	if err := validateMuxIdentifier("eventId", value.EventID); err != nil {
		return fmt.Errorf("%w: %v", errMuxProtocol, err)
	}
	if err := validateMuxIdentifier("agentId", value.AgentID); err != nil {
		return fmt.Errorf("%w: %v", errMuxProtocol, err)
	}

	size := len(raw)
	if size > muxMaxItemBytes {
		return errMuxItemTooLarge
	}
	item := PendingItem{
		EventID: value.EventID,
		AgentID: value.AgentID,
		Event:   value.Event,
		Request: append(json.RawMessage(nil), value.Request...),
		Raw:     append(json.RawMessage(nil), raw...),
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	total := m.totalBytes
	if existing, ok := m.items[value.EventID]; ok {
		total -= len(existing.Raw)
	} else if len(m.items) >= muxMaxPendingItems {
		return errMuxCapacity
	}
	total += size
	if total > muxMaxPendingBytes {
		return errMuxTotalTooLarge
	}

	if _, ok := m.items[value.EventID]; !ok {
		m.order = append(m.order, value.EventID)
	}
	m.items[value.EventID] = item
	m.totalBytes = total
	// 同一 eventId 重新出现时，旧的 cancel 记录必须失效。
	delete(m.canceled, value.EventID)
	return nil
}

// handleCancel 按 eventId 删除待答项，记录取消事实并广播等待者。
func (m *Mux) handleCancel(raw json.RawMessage) error {
	var value struct {
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%w: cancel 帧格式无效", errMuxProtocol)
	}
	if err := validateMuxIdentifier("eventId", value.EventID); err != nil {
		return fmt.Errorf("%w: %v", errMuxProtocol, err)
	}

	m.mu.Lock()
	if item, ok := m.items[value.EventID]; ok {
		delete(m.items, value.EventID)
		m.totalBytes -= len(item.Raw)
		for index, id := range m.order {
			if id == value.EventID {
				m.order = append(m.order[:index], m.order[index+1:]...)
				break
			}
		}
	}
	m.recordCanceledLocked(value.EventID)
	signal := m.replaceCancelSignalLocked()
	m.mu.Unlock()
	close(signal)
	return nil
}

// recordCanceledLocked 记录最近取消的 eventId，供提交答案后的竞态确认。
func (m *Mux) recordCanceledLocked(eventID string) {
	m.cancelSeq++
	m.canceled[eventID] = m.cancelSeq
	m.canceledRing = append(m.canceledRing, muxCancelRecord{id: eventID, seq: m.cancelSeq})
	for len(m.canceledRing) > muxCanceledHistory {
		oldest := m.canceledRing[0]
		m.canceledRing = m.canceledRing[1:]
		if current, ok := m.canceled[oldest.id]; ok && current == oldest.seq {
			delete(m.canceled, oldest.id)
		}
	}
}

// replaceCancelSignalLocked 换出广播 channel；换出的 channel 由调用方在锁外关闭。
func (m *Mux) replaceCancelSignalLocked() chan struct{} {
	previous := m.cancelSignal
	m.cancelSignal = make(chan struct{})
	return previous
}

// reset 在连接断开后清空待答表：旧 clientId 已失效，未提交的答案不能沿用。
func (m *Mux) reset() {
	m.mu.Lock()
	m.clientID = ""
	m.items = make(map[string]PendingItem)
	m.order = nil
	m.totalBytes = 0
	m.canceled = make(map[string]uint64)
	m.canceledRing = nil
	signal := m.replaceCancelSignalLocked()
	m.mu.Unlock()
	close(signal)
}

// isReady 判断当前连接是否已经收到 ready。
func (m *Mux) isReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clientID != ""
}

// Snapshot 返回当前待答表的副本；Mux 为 nil 时返回空快照。
func (m *Mux) Snapshot() PendingSnapshot {
	if m == nil {
		return PendingSnapshot{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]PendingItem, 0, len(m.order))
	for _, eventID := range m.order {
		if item, ok := m.items[eventID]; ok {
			items = append(items, item)
		}
	}
	return PendingSnapshot{ClientID: m.clientID, Items: items}
}

// WaitCancel 等待 eventID 在等待上限内被 dsh 取消。
//
// 已记录过的 cancel 立即返回 true；事件从未出现且未取消时立即返回 false；
// 事件仍在待答表中时最多等待 cancelWait 或 ctx 结束。
func (m *Mux) WaitCancel(ctx context.Context, eventID string) bool {
	if m == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := m.cancelWait
	if timeout <= 0 {
		timeout = muxCancelWaitDefault
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		m.mu.Lock()
		_, pending := m.items[eventID]
		_, canceled := m.canceled[eventID]
		signal := m.cancelSignal
		m.mu.Unlock()

		if canceled {
			return true
		}
		if !pending {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-signal:
		}
	}
}

// waitMuxRetry 等待下一次重连；ctx 取消时立即返回。
func waitMuxRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// validateMuxIdentifier 校验 clientId/eventId/agentId 非空、长度有界且不含控制字符。
func validateMuxIdentifier(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s 为空", errMuxIdentifier, field)
	}
	if len(value) > muxIdentifierMaxBytes {
		return fmt.Errorf("%w: %s 超过 %d 字节", errMuxIdentifier, field, muxIdentifierMaxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s 不是合法 UTF-8", errMuxIdentifier, field)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: %s 含控制字符", errMuxIdentifier, field)
	}
	return nil
}

// Run 持续维护 dsh Mux 订阅，直到 ctx 被取消。
//
// 目录、读取和发送不依赖 Mux；Mux 断开只影响待答事件和审批回答。
func (c *Client) Run(ctx context.Context) error {
	if c == nil || c.mux == nil {
		return errMuxUnavailable
	}
	return c.mux.Run(ctx)
}

// Pending 返回当前 Mux 待答表快照。
//
// Mux 未启动、未就绪或已断开时返回 errMuxUnavailable；调用方仍可继续使用
// 目录、读取和发送功能。ctx 只用于进入前的取消检查。
func (c *Client) Pending(ctx context.Context) (PendingSnapshot, error) {
	if c == nil || c.mux == nil {
		return PendingSnapshot{}, errMuxUnavailable
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return PendingSnapshot{}, err
		}
	}
	snapshot := c.mux.Snapshot()
	if snapshot.ClientID == "" {
		return PendingSnapshot{}, errMuxUnavailable
	}
	return snapshot, nil
}

// SubmitEventAnswer 用当前 Mux 连接回答一条 waterfall 审批或问题。
//
// clientID 必须取自同一时刻 Pending 快照；事件已失效或连接已重连时 dsh 会
// 返回业务错误。提交不做自动重试，错误信息不包含 Cookie 或认证材料。
func (c *Client) SubmitEventAnswer(ctx context.Context, clientID, eventID string, value any) error {
	if err := validateMuxIdentifier("clientId", clientID); err != nil {
		return err
	}
	if err := validateMuxIdentifier("eventId", eventID); err != nil {
		return err
	}
	_, err := c.rpc.call(ctx, "$events/result", map[string]any{
		"clientId": clientID,
		"eventId":  eventID,
		"outcome": map[string]any{
			"kind":  "result",
			"value": value,
		},
	})
	if err != nil {
		return fmt.Errorf("提交 dsh 审批回答失败: %w", err)
	}
	return nil
}
