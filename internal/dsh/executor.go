package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

const (
	// dshAgent 是 HPRP 目标中的 Agent 标识。
	dshAgent = "dsh"
	// dshDisplayAgent 是面向用户展示的 Agent 名称。
	dshDisplayAgent = "DeepSeek Harness"
	// hostEntryTitle 是没有可控制会话时宿主入口的标题。
	hostEntryTitle = "新建会话"
	// hostEntryTab 是宿主入口的标签页名称，Server 用它识别 dsh 宿主入口。
	hostEntryTab = "dsh-host"
	// defaultWorkspace 是会话缺少工作目录时的展示名称。
	defaultWorkspace = "dsh"
	// refreshInterval 是目录与待答状态的刷新间隔。
	refreshInterval = time.Second
)

// API 是单宿主执行器所需的 dsh 能力。
//
// 生产实现是 Client；测试注入 fake，不依赖 Mux 或网络。
type API interface {
	// Sessions 返回当前宿主持有控制权的可列表会话。
	Sessions(ctx context.Context) ([]SessionSummary, error)
	// Pending 返回当前 Mux 待答事件快照。
	Pending(ctx context.Context) (PendingSnapshot, error)
	// Read 返回会话的聊天正文。
	Read(ctx context.Context, sessionID string) (string, error)
	// Prompt 以排队模式向会话发送用户文本。
	Prompt(ctx context.Context, sessionID, requestID, text string) error
	// Create 在指定目录创建会话并设置标题。
	Create(ctx context.Context, cwd, title string) (CreatedSession, error)
	// Decisions 返回会话当前的审批与问题决策。
	Decisions(ctx context.Context, sessionID string) ([]Decision, error)
	// SubmitAnswer 提交一个已校验的决策答案。
	SubmitAnswer(ctx context.Context, decision Decision, value any) (AnswerReceipt, error)
}

// Sink 接收命令回复及轻量状态通知。
type Sink interface {
	// RespondMarkdown 使用上游请求标识发送一条 Markdown 回复。
	RespondMarkdown(ctx context.Context, requestID, content string) error
	// SendNotification 发送一条属于稳定目标的状态通知。
	SendNotification(ctx context.Context, target im.NotificationTarget, event im.NotificationEvent) error
}

// 编译期确认 Client 覆盖执行器所需的全部能力。
var _ API = (*Client)(nil)

// executorSession 是执行器对单个会话的观测状态。
type executorSession struct {
	summary     SessionSummary
	target      session.Target
	blocked     bool
	decisionKey string
}

// status 按 blocked > working > idle 推导会话状态。
func (s executorSession) status() herdr.AgentStatus {
	if s.blocked {
		return herdr.AgentStatusBlocked
	}
	if s.summary.Running {
		return herdr.AgentStatusWorking
	}
	return herdr.AgentStatusIdle
}

// executorNotification 是待发送的一条状态通知。
type executorNotification struct {
	target im.NotificationTarget
	event  im.NotificationEvent
}

// Executor 把一个 dsh 宿主映射为 HPRP 稳定目标并维护状态通知。
//
// Executor 不启动 Mux 或网络连接；Client.Run 与 Executor.Run 由多宿主 Backend
// 并行运行。会话目标身份包含宿主引用，宿主重启后旧目标自动失效。
type Executor struct {
	host   Host
	api    API
	sink   Sink
	guard  *policy.Guard
	logger *slog.Logger

	mu        sync.RWMutex
	sessions  map[string]executorSession
	catalogOK bool
	selected  session.Target

	inputMu sync.Mutex
	buffer  panel.Buffer
}

// NewExecutor 创建目录为空的单宿主执行器；Run 负责周期性同步。
func NewExecutor(host Host, api API, sink Sink, guard *policy.Guard) *Executor {
	return &Executor{
		host:     host,
		api:      api,
		sink:     sink,
		guard:    guard,
		logger:   slog.Default(),
		sessions: make(map[string]executorSession),
	}
}

// Run 每秒刷新目录和待答状态，直到 ctx 结束。
func (e *Executor) Run(ctx context.Context) error {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		e.refresh(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// refresh 同步会话目录、待答 blocking 状态和通知。
//
// 目录失败时撤下全部目标（含宿主入口）；Pending 单独失败时保留上次 blocking
// 判断，避免 Mux 短暂断线造成误报 done。
func (e *Executor) refresh(ctx context.Context) {
	list, sessionsErr := e.api.Sessions(ctx)
	pending, pendingErr := e.api.Pending(ctx)

	e.mu.RLock()
	previous := e.sessions
	e.mu.RUnlock()

	next := make(map[string]executorSession, len(list))
	if sessionsErr == nil {
		for _, summary := range list {
			if !summary.Available {
				continue
			}
			target := e.sessionTarget(summary)
			next[target.PaneID] = executorSession{summary: summary, target: target}
		}
	} else {
		e.logger.Warn("读取 dsh 会话目录失败，撤下本宿主目标", "host", e.host.Reference(), "error", sessionsErr)
	}

	if sessionsErr == nil {
		if pendingErr != nil {
			e.logger.Warn("读取 dsh 待答事件失败，保留上次 blocking 判断", "host", e.host.Reference(), "error", pendingErr)
			carryForwardBlocking(next, previous)
		} else {
			applyPending(next, pending)
		}
	}

	e.mu.Lock()
	notifications := collectNotifications(previous, next)
	e.sessions = next
	e.catalogOK = sessionsErr == nil
	e.mu.Unlock()

	if e.sink == nil {
		return
	}
	for _, notification := range notifications {
		_ = e.sink.SendNotification(ctx, notification.target, notification.event)
	}
}

// carryForwardBlocking 在 Pending 不可用时沿用同一会话上次的 blocking 身份。
func carryForwardBlocking(next, previous map[string]executorSession) {
	for key, state := range next {
		old, ok := previous[key]
		if !ok {
			continue
		}
		state.blocked = old.blocked
		state.decisionKey = old.decisionKey
		if state.blocked {
			state.target.Status = herdr.AgentStatusBlocked
		}
		next[key] = state
	}
}

// applyPending 按 agentId 聚合待答项，并把 blocking 会话标记为 blocked。
func applyPending(next map[string]executorSession, pending PendingSnapshot) {
	blocking := make(map[string][]string)
	for _, item := range pending.Items {
		if !blockingPending(item) {
			continue
		}
		blocking[item.AgentID] = append(blocking[item.AgentID], item.EventID+":"+item.Event)
	}
	for key, state := range next {
		keys, ok := blocking[state.summary.ID]
		if !ok {
			continue
		}
		state.blocked = true
		state.decisionKey = shortHash(keys...)
		state.target.Status = herdr.AgentStatusBlocked
		next[key] = state
	}
}

// blockingPending 判断一条 Mux 待答事件是否使 Agent 进入 blocked。
//
// approval 一律 blocking；问题只有在 request.wait.timed 不为 true 时才 blocking，
// 定时问题允许原生界面先行超时。未知事件保留事实但不影响状态；请求无法解析时
// 保守按 blocking 处理，避免静默漏掉等待用户的交互。
func blockingPending(item PendingItem) bool {
	switch item.Event {
	case "approval/request":
		return true
	case "user-questions/request":
		request := bytes.TrimSpace(item.Request)
		if len(request) == 0 {
			return true
		}
		var payload userQuestionsRequestPayload
		if err := json.Unmarshal(request, &payload); err != nil {
			return true
		}
		return payload.Wait == nil || !payload.Wait.Timed
	default:
		return false
	}
}

// collectNotifications 比较前后两次观测，生成至多一条/会话的状态通知。
//
// 首次观察到 blocking 决策也必须通知；相同 idle 状态只有 UpdatedAt 变化时才再次
// 通知，避免每秒重复。
func collectNotifications(previous, next map[string]executorSession) []executorNotification {
	now := time.Now()
	notifications := make([]executorNotification, 0)
	for key, current := range next {
		old, seen := previous[key]
		if !seen {
			if current.blocked {
				// 首次观察即 blocked 时没有旧状态；HPRP 要求前后状态非空且不同，
				// 用 unknown 作为保守前态。
				notifications = append(notifications, notificationFor(current,
					string(herdr.AgentStatusUnknown), string(herdr.AgentStatusBlocked), now))
			}
			continue
		}
		if !notificationNeeded(old, current) {
			continue
		}
		previousStatus := string(old.status())
		status := notificationStatus(current.status())
		if previousStatus == status {
			// HPRP 要求前后状态不同；同一 blocked 状态下决策被替换时用 unknown 作为保守前态。
			previousStatus = string(herdr.AgentStatusUnknown)
		}
		notifications = append(notifications, notificationFor(current, previousStatus, status, now))
	}
	return notifications
}

// notificationNeeded 判断一次刷新是否产生了值得通知的状态身份变化。
func notificationNeeded(previous, current executorSession) bool {
	if previous.status() != current.status() {
		return true
	}
	if current.blocked && previous.decisionKey != current.decisionKey {
		return true
	}
	// 状态相同且都是 idle 时，只有活动时间戳变化才重新通知。
	return current.status() == herdr.AgentStatusIdle && previous.summary.UpdatedAt != current.summary.UpdatedAt
}

// notificationFor 构造一条 agent.status.changed 通知。
func notificationFor(state executorSession, previousStatus, status string, now time.Time) executorNotification {
	return executorNotification{
		target: im.NotificationTarget{
			PaneID:       state.target.PaneID,
			OccupantHash: state.target.OccupantKey,
			Agent:        dshAgent,
			DisplayAgent: dshDisplayAgent,
			Title:        state.target.Title,
		},
		event: im.NotificationEvent{
			Kind:           im.NotificationKindAgentStatusChanged,
			PreviousStatus: previousStatus,
			Status:         status,
			OccurredAt:     now,
		},
	}
}

// notificationStatus 把内部状态映射为通知状态；idle 转换通知使用 done。
func notificationStatus(status herdr.AgentStatus) string {
	if status == herdr.AgentStatusIdle {
		return string(herdr.AgentStatusDone)
	}
	return string(status)
}

// CurrentTargets 返回当前目标的独立副本。
//
// 宿主入口只在目录可用且没有任何可控制会话时出现并排在首位；会话按标题、
// 会话 ID 升序。
func (e *Executor) CurrentTargets() []session.Target {
	e.mu.RLock()
	defer e.mu.RUnlock()
	sessions := make([]executorSession, 0, len(e.sessions))
	for _, state := range e.sessions {
		sessions = append(sessions, state)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].target.Title != sessions[j].target.Title {
			return sessions[i].target.Title < sessions[j].target.Title
		}
		return sessions[i].summary.ID < sessions[j].summary.ID
	})
	targets := make([]session.Target, 0, len(sessions)+1)
	if e.catalogOK && len(e.sessions) == 0 {
		targets = append(targets, e.hostTarget())
	}
	for _, state := range sessions {
		targets = append(targets, state.target)
	}
	return targets
}

// SelectTarget 只接受当前目录中的完整身份，防止复用失效编号。
func (e *Executor) SelectTarget(paneID, occupant string) error {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	target, err := e.targetLocked(paneID, occupant)
	if err != nil {
		return err
	}
	if e.selected.PaneID != target.PaneID || e.selected.OccupantKey != target.OccupantKey {
		e.buffer.Reset()
	}
	e.selected = target
	return nil
}

// SelectedTarget 每次复核当前目录，不返回已失效的旧选择。
func (e *Executor) SelectedTarget() (session.Target, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.selected.PaneID == "" {
		return session.Target{}, session.ErrNoSelection
	}
	target, err := e.targetLocked(e.selected.PaneID, e.selected.OccupantKey)
	if err != nil {
		return session.Target{}, err
	}
	return target, nil
}

// targetLocked 按完整身份解析目标；调用方必须持有 e.mu。
func (e *Executor) targetLocked(paneID, occupant string) (session.Target, error) {
	if e.catalogOK && len(e.sessions) == 0 {
		hostTarget := e.hostTarget()
		if paneID == hostTarget.PaneID && occupant == hostTarget.OccupantKey {
			return hostTarget, nil
		}
	}
	state, ok := e.sessions[paneID]
	if !ok || state.target.OccupantKey != occupant {
		return session.Target{}, session.ErrSelectionInvalid
	}
	return state.target, nil
}

// hostTarget 返回没有可控制会话时使用的宿主入口目标。
func (e *Executor) hostTarget() session.Target {
	slot := "dsh-host:" + shortHash(e.host.Reference())
	return session.Target{
		PaneID:       slot,
		OccupantKey:  slot,
		Agent:        dshAgent,
		DisplayAgent: dshDisplayAgent,
		Title:        hostEntryTitle,
		Status:       herdr.AgentStatusIdle,
		Workspace:    e.host.Variant,
		Tab:          hostEntryTab,
	}
}

// sessionTarget 把会话映射为稳定目标。
//
// PaneID 含宿主引用，避免 web/Desktop 共享历史时同会话 ID 冲突；OccupantKey
// 只取决于宿主 Generation 和会话 ID，宿主内会话消失再出现时保持稳定。
func (e *Executor) sessionTarget(s SessionSummary) session.Target {
	title := s.Title
	if strings.TrimSpace(title) == "" {
		title = s.ID
	}
	workspace := strings.TrimSpace(s.Cwd)
	if workspace == "" {
		workspace = defaultWorkspace
	} else {
		workspace = filepath.Base(workspace)
	}
	status := herdr.AgentStatusIdle
	if s.Running {
		status = herdr.AgentStatusWorking
	}
	return session.Target{
		PaneID:       "dsh:" + shortHash(e.host.Reference(), s.ID),
		OccupantKey:  "session:" + shortHash(e.host.Generation, s.ID),
		Agent:        dshAgent,
		DisplayAgent: dshDisplayAgent,
		Title:        title,
		Status:       status,
		Workspace:    workspace,
		Tab:          e.host.Variant,
	}
}

// shortHash 返回以 NUL 连接各段后 SHA-256 的前 16 个十六进制字符。
func shortHash(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])[:16]
}
