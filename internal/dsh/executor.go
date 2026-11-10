package dsh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/command"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
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
	// defaultNewSessionTitle 是 /new-session 未提供标题时使用的默认标题。
	defaultNewSessionTitle = "默认标题"
	// createdSessionGrace 是新建会话在 session/list 追上之前的本地保留时长。
	createdSessionGrace = 5 * time.Second
	// terminalSnapshotUTF16Limit 限制正文与待答提示合计的 UTF-16 代码单元数。
	terminalSnapshotUTF16Limit = 20000
	// snapshotSeparator 分隔聊天正文与待答提示。
	snapshotSeparator = "\n\n"
)

const (
	// executorSelectionInvalidMessage 表示当前选择已失效。
	executorSelectionInvalidMessage = "dsh 会话已失效，请重新 /ls 并选择。"
	// executorHostEntryMessage 表示宿主入口只接受 /new-session。
	executorHostEntryMessage = "这是 dsh 宿主入口，仅支持 /new-session 新建会话。"
	// executorBlockedPromptMessage 阻止把普通文本误当审批回答。
	executorBlockedPromptMessage = "dsh 正在等待审批或回答，请先执行 /answer 处理待处理事项。"
	// executorPromptQueuedMessage 是排队发送成功的固定回复。
	executorPromptQueuedMessage = "已发送给 dsh（排队）。"
	// executorKeyUnsupportedMessage 说明 dsh 不支持终端按键。
	executorKeyUnsupportedMessage = "dsh 不支持终端按键；审批请使用 /answer。"
	// executorUnsupportedCommandMessage 是未在 dsh 执行器实现的命令回复。
	executorUnsupportedCommandMessage = "请使用 /ls、/N、/con、/answer 或普通文本操作 dsh。"
	// executorDecisionChangedMessage 表示待答内容已变化，必须重新读取。
	executorDecisionChangedMessage = "待处理事项已变化，请重新执行 /con 查看并回答。"
	// executorNoDecisionMessage 表示当前没有可回答的交互。
	executorNoDecisionMessage = "当前没有可通过 /answer 回答的 dsh 审批或问题。"
	// executorDecisionNotShownMessage 要求先查看当前待答事项，避免凭旧提示作答。
	executorDecisionNotShownMessage = "请先执行 /con 查看当前待答事项，再使用 /answer 回答。"
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
	// pendingUntil 非零时表示本地新建会话在 session/list 追上之前的保留期限。
	pendingUntil time.Time
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

// shownDecision 是用户最近一次实际看到的待答决策身份。
//
// /answer 只允许回答该决策：用户的编号选择针对的是展示时的题目，不能在待答
// 集合变化后被套用到另一条审批上。
type shownDecision struct {
	nativeID string
	revision string
}

// AnswerState 记录一个 dsh 会话中逐题收集但尚未提交的答案。
//
// 每次 /answer 都会按 DecisionID 与 Revision 复核决策；待答内容变化、被取消或
// 版本不一致时丢弃全部已收集答案，避免把旧答案提交到新问题。
type AnswerState struct {
	// DecisionID 是 dsh 原生交互标识。
	DecisionID string
	// Revision 是决策原始帧的摘要。
	Revision string
	// QuestionIx 是下一道待回答问题的下标。
	QuestionIx int
	// Answers 是按问题 ID 保存的已校验答案。
	Answers map[string]Answer
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

	mu             sync.RWMutex
	sessions       map[string]executorSession
	catalogOK      bool
	selected       session.Target
	createdTarget  session.Target
	answerStates   map[string]AnswerState
	shownDecisions map[string]shownDecision

	inputMu sync.Mutex
	buffer  panel.Buffer
}

// NewExecutor 创建目录为空的单宿主执行器；Run 负责周期性同步。
func NewExecutor(host Host, api API, sink Sink, guard *policy.Guard) *Executor {
	return &Executor{
		host:           host,
		api:            api,
		sink:           sink,
		guard:          guard,
		logger:         slog.Default(),
		sessions:       make(map[string]executorSession),
		answerStates:   make(map[string]AnswerState),
		shownDecisions: make(map[string]shownDecision),
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
// 目录失败时撤下普通目标与宿主入口，但保留仍在宽限期内的本地新建会话；
// Pending 单独失败时保留上次 blocking 判断，避免 Mux 短暂断线造成误报 done。
func (e *Executor) refresh(ctx context.Context) {
	list, sessionsErr := e.api.Sessions(ctx)
	pending, pendingErr := e.api.Pending(ctx)

	// 复制上一次目录，避免后续在锁外读取时与 /new-session 的写入并发。
	e.mu.RLock()
	previous := make(map[string]executorSession, len(e.sessions))
	for key, state := range e.sessions {
		previous[key] = state
	}
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
	// 新建会话可能尚未出现在 session/list：无论本次目录读取是否成功，都在
	// 5 秒宽限期内保留本地条目，避免一次列表失败立即丢失刚创建的会话；到期后
	// 仍未出现在目录中才移除。
	now := time.Now()
	for key, state := range e.sessions {
		if state.pendingUntil.IsZero() || !now.Before(state.pendingUntil) {
			continue
		}
		if _, exists := next[key]; exists {
			continue
		}
		next[key] = state
	}
	e.pruneShownDecisionsLocked(next, pending, pendingErr, sessionsErr == nil)
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
		// 目标切换后，旧会话展示过的决策不能用于新目标的 /answer；新目标也必须
		// 重新 /con 展示一次。
		e.clearShownDecisionsForTargetLocked(e.selected)
		e.clearShownDecisionsForTargetLocked(target)
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
// Title/Workspace 来自 dsh 宿主，进入 HPRP 展示标签前必须清除 ANSI/C0/C1。
func (e *Executor) sessionTarget(s SessionSummary) session.Target {
	title := sanitizeDisplayLabel(s.Title)
	if title == "" {
		title = s.ID
	}
	workspace := defaultWorkspace
	if cwd := strings.TrimSpace(s.Cwd); cwd != "" {
		if base := sanitizeDisplayLabel(filepath.Base(cwd)); base != "" {
			workspace = base
		}
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
		Title:        truncateLabel(title, hprp.MaxLabelBytes),
		Status:       status,
		Workspace:    truncateLabel(workspace, hprp.MaxLabelBytes),
		Tab:          e.host.Variant,
	}
}

// sanitizeDisplayLabel 清除 ANSI/C0/C1 控制字符，并把换行/制表折叠为空格。
//
// dsh 标题与工作目录的字符集不受 HPRP 约束；控制字符进入 Server 展示标签
// 会污染 /ls 与通知文本，必须在生成目标时清理。
func sanitizeDisplayLabel(value string) string {
	cleaned := stripTranscriptControl(value)
	cleaned = strings.Map(func(current rune) rune {
		if current == '\n' || current == '\t' {
			return ' '
		}
		return current
	}, cleaned)
	return strings.TrimSpace(cleaned)
}

// truncateLabel 按 UTF-8 边界把展示标签截断到字节上限。
//
// dsh 标题长度不受 HPRP 约束；超过 MaxLabelBytes 会让 Server 拒绝整个会话快照，
// 导致 Pal 反复重连并连带撤下其他目标，因此必须在生成目标时截断。
func truncateLabel(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// shortHash 返回以 NUL 连接各段后 SHA-256 的前 16 个十六进制字符。
func shortHash(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])[:16]
}

// HandleMessage 处理一条已完成 IM 协议校验的 dsh 目标命令。
//
// 身份与消息标识在产生任何外部动作前校验；每次调用开始都会清除上一次
// /new-session 留下的待领取目标，避免旧目标被错误关联到后续命令结果。
func (e *Executor) HandleMessage(ctx context.Context, message im.IncomingText) {
	if e == nil {
		return
	}
	e.clearCreatedTarget()
	if e.guard.Authorize(policy.Identity{UserID: message.UserID, ChatType: message.ChatType}) != nil {
		if e.logger != nil {
			e.logger.Warn("dsh 消息被策略拒绝",
				"user_hash", shortHash(message.UserID),
				"chat_type", message.ChatType,
			)
		}
		return
	}
	reply := func(content string) { e.respond(ctx, message.RequestID, content) }
	if strings.TrimSpace(message.MessageID) == "" {
		reply("消息标识缺失，未执行任何操作。")
		return
	}

	e.inputMu.Lock()
	defer e.inputMu.Unlock()

	action, err := command.Parse(message.Content)
	if err != nil {
		reply(err.Error())
		return
	}
	target, state, hostEntry, err := e.selectedSessionTarget()
	if err != nil {
		reply(executorSelectionInvalidMessage)
		return
	}
	if hostEntry && action.Kind != command.KindNewSession {
		reply(executorHostEntryMessage)
		return
	}

	switch action.Kind {
	case command.KindPrompt:
		// 待答期间拒绝普通文本，避免把消息误当成审批或问题答案。
		if state.blocked {
			reply(executorBlockedPromptMessage)
			return
		}
		if err := e.api.Prompt(ctx, state.summary.ID, message.MessageID, action.Text); err != nil {
			reply("发送失败，请稍后重试。")
			return
		}
		reply(executorPromptQueuedMessage)
	case command.KindContent:
		e.handleRead(ctx, reply, target, state.summary.ID, executorReadRefresh)
	case command.KindPageUp:
		e.handleRead(ctx, reply, target, state.summary.ID, executorReadPageUp)
	case command.KindPageDown:
		e.handleRead(ctx, reply, target, state.summary.ID, executorReadPageDown)
	case command.KindAnswer:
		e.handleAnswer(ctx, reply, state.summary.ID, action)
	case command.KindNewSession:
		e.handleNewSession(ctx, reply, action)
	case command.KindKey:
		reply(executorKeyUnsupportedMessage)
	default:
		reply(executorUnsupportedCommandMessage)
	}
}

// respond 通过 Sink 发送一条命令回复。
func (e *Executor) respond(ctx context.Context, requestID, content string) {
	if e.sink == nil || strings.TrimSpace(requestID) == "" {
		return
	}
	_ = e.sink.RespondMarkdown(ctx, requestID, content)
}

// selectedSessionTarget 按完整身份解析当前选择。
//
// hostEntry 为 true 时 state 为零值；其他情况下 state 是会话的最新观测状态。
func (e *Executor) selectedSessionTarget() (session.Target, executorSession, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.selected.PaneID == "" {
		return session.Target{}, executorSession{}, false, session.ErrNoSelection
	}
	target, err := e.targetLocked(e.selected.PaneID, e.selected.OccupantKey)
	if err != nil {
		return session.Target{}, executorSession{}, false, err
	}
	if target.Tab == hostEntryTab {
		return target, executorSession{}, true, nil
	}
	state, ok := e.sessions[target.PaneID]
	if !ok {
		return session.Target{}, executorSession{}, false, session.ErrSelectionInvalid
	}
	return target, state, false, nil
}

// executorReadMode 区分聊天正文刷新、向上翻页和向下翻页。
type executorReadMode uint8

const (
	executorReadRefresh executorReadMode = iota
	executorReadPageUp
	executorReadPageDown
)

// handleRead 读取聊天正文并复用 panel.Buffer 分页，末尾追加当前待答提示。
func (e *Executor) handleRead(ctx context.Context, reply func(string), target session.Target, sessionID string, mode executorReadMode) {
	key := target.OccupantKey
	switch mode {
	case executorReadRefresh, executorReadPageUp:
		text, err := e.api.Read(ctx, sessionID)
		if err != nil {
			reply("读取 dsh 对话失败，请稍后重试。")
			return
		}
		lines := strings.Split(text, "\n")
		if mode == executorReadRefresh {
			e.buffer.Refresh(key, lines)
		} else if err := e.buffer.Expand(key, lines); err != nil {
			reply(executorReadErrorMessage(err))
			return
		}
	case executorReadPageDown:
		if err := e.buffer.PageDown(); err != nil {
			reply(executorReadErrorMessage(err))
			return
		}
	}
	transcript := strings.Join(e.buffer.Render(), "\n")
	if strings.TrimSpace(transcript) == "" {
		transcript = "dsh 暂无对话文本。"
	}
	prompt, _ := e.pendingDecisionPrompt(ctx, sessionID)
	// 正文与决策提示都可能超过 HPRP 的 MaxContentBytes；超限会让 Server 关闭
	// 整条连接，因此 reply 前统一按终端快照预算截断并保留提示尾部。
	reply(boundedSnapshotText(transcript, prompt))
}

// executorReadErrorMessage 把分页错误映射为固定提示。
func executorReadErrorMessage(err error) string {
	switch {
	case errors.Is(err, panel.ErrPanelChanged):
		return "对话内容已变化，请重新执行 /con。"
	case errors.Is(err, panel.ErrOldestPage):
		return "已经是最早可读取内容。"
	case errors.Is(err, panel.ErrNewestPage):
		return "已经是最新内容。"
	default:
		return "分页不可用，请重新执行 /con。"
	}
}

// pendingDecisionPrompt 返回当前应提示给用户的决策文本。
func (e *Executor) pendingDecisionPrompt(ctx context.Context, sessionID string) (string, bool) {
	decisions, err := e.api.Decisions(ctx, sessionID)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("读取 dsh 待处理事项失败",
				"session_hash", shortHash(sessionID),
				"error", err,
			)
		}
		return "", false
	}
	decision, ok := selectPendingDecision(decisions)
	if !ok {
		return "", false
	}
	// 记录真正展示给用户的决策；后续 /answer 只能回答这一条。
	e.rememberShownDecision(sessionID, decision)
	return formatDecision(decision, e.decisionQuestionIndex(sessionID, decision)), true
}

// rememberShownDecision 记录会话最近展示的待答决策身份。
func (e *Executor) rememberShownDecision(sessionID string, decision Decision) {
	e.mu.Lock()
	if e.shownDecisions == nil {
		e.shownDecisions = make(map[string]shownDecision)
	}
	e.shownDecisions[sessionID] = shownDecision{nativeID: decision.NativeID, revision: decision.Revision}
	e.mu.Unlock()
}

// shownDecisionFor 返回会话最近展示的待答决策。
func (e *Executor) shownDecisionFor(sessionID string) (shownDecision, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	shown, ok := e.shownDecisions[sessionID]
	return shown, ok
}

// clearShownDecision 清除会话最近展示的待答决策。
func (e *Executor) clearShownDecision(sessionID string) {
	e.mu.Lock()
	delete(e.shownDecisions, sessionID)
	e.mu.Unlock()
}

// clearShownDecisionsForTargetLocked 清除目标会话的展示记录；调用方必须持有 e.mu。
func (e *Executor) clearShownDecisionsForTargetLocked(target session.Target) {
	if target.PaneID == "" {
		return
	}
	if state, ok := e.sessions[target.PaneID]; ok {
		delete(e.shownDecisions, state.summary.ID)
	}
}

// pruneShownDecisionsLocked 清除会话已消失或待答事件已取消的展示记录。
//
// 目录或待答表读取失败时无法确认取消事实，保留记录；/answer 时仍会复核当前
// 决策身份与 revision，不会把答案提交给用户没见过的决策。
func (e *Executor) pruneShownDecisionsLocked(next map[string]executorSession, pending PendingSnapshot, pendingErr error, catalogOK bool) {
	if !catalogOK || len(e.shownDecisions) == 0 {
		return
	}
	live := make(map[string]struct{}, len(next))
	for _, state := range next {
		live[state.summary.ID] = struct{}{}
	}
	var pendingEvents map[string]struct{}
	if pendingErr == nil {
		pendingEvents = make(map[string]struct{}, len(pending.Items))
		for _, item := range pending.Items {
			pendingEvents[item.AgentID+"\x00"+item.EventID] = struct{}{}
		}
	}
	for sessionID, shown := range e.shownDecisions {
		if _, ok := live[sessionID]; !ok {
			delete(e.shownDecisions, sessionID)
			continue
		}
		if pendingEvents == nil || !strings.HasPrefix(shown.nativeID, "event:") {
			continue
		}
		eventID := strings.TrimPrefix(shown.nativeID, "event:")
		if _, ok := pendingEvents[sessionID+"\x00"+eventID]; !ok {
			delete(e.shownDecisions, sessionID)
		}
	}
}

// decisionQuestionIndex 返回多题收集期间当前应展示的问题下标。
//
// 只有收集状态属于同一决策和 revision 时才采用其 QuestionIx；否则显示第一题。
func (e *Executor) decisionQuestionIndex(sessionID string, decision Decision) int {
	state, ok := e.answerState(sessionID)
	if !ok || state.DecisionID != decision.NativeID || state.Revision != decision.Revision ||
		state.QuestionIx < 0 || state.QuestionIx >= len(decision.Questions) {
		return 0
	}
	return state.QuestionIx
}

// selectPendingDecision 按设计文档的规则选择待答项。
//
// 优先第一个 blocking 决策；没有 blocking 时回退到第一个 supported 决策。
// 定时 waterfall 问题（event:<eventId>）与持续问题（call:<callId>）都符合回退条件；
// 持续问题本身不进入 blocked 状态。
func selectPendingDecision(decisions []Decision) (Decision, bool) {
	for _, decision := range decisions {
		if decision.Blocking {
			return decision, true
		}
	}
	for _, decision := range decisions {
		if decision.Supported {
			return decision, true
		}
	}
	return Decision{}, false
}

// decisionByID 在最新决策列表中按原生 ID 查找。
func decisionByID(decisions []Decision, nativeID string) (Decision, bool) {
	for _, decision := range decisions {
		if decision.NativeID == nativeID {
			return decision, true
		}
	}
	return Decision{}, false
}

// handleAnswer 逐题收集答案，最后一题完成后一次性提交。
//
// 每次回答前重新读取决策；DecisionID、Revision 或待答表不一致时丢弃状态并
// 要求重新 /con。提交失败不自动重试。
func (e *Executor) handleAnswer(ctx context.Context, reply func(string), sessionID string, action command.Action) {
	decisions, err := e.api.Decisions(ctx, sessionID)
	if err != nil {
		reply("读取 dsh 待处理事项失败，请稍后重试。")
		return
	}

	state, active := e.answerState(sessionID)
	var decision Decision
	if active {
		current, ok := decisionByID(decisions, state.DecisionID)
		if !ok || current.Revision != state.Revision ||
			state.QuestionIx < 0 || state.QuestionIx >= len(current.Questions) {
			e.clearAnswerState(sessionID)
			e.clearShownDecision(sessionID)
			reply(executorDecisionChangedMessage)
			return
		}
		decision = current
	} else {
		current, ok := selectPendingDecision(decisions)
		if !ok || len(current.Questions) == 0 {
			e.clearShownDecision(sessionID)
			reply(executorNoDecisionMessage)
			return
		}
		// 编号答案只对用户实际看到的决策有效：待答集合变化后绝不能把
		// 「/answer 1」套用到另一条审批上。
		shown, displayed := e.shownDecisionFor(sessionID)
		if !displayed {
			reply(executorDecisionNotShownMessage)
			return
		}
		if shown.nativeID != current.NativeID || shown.revision != current.Revision {
			e.clearShownDecision(sessionID)
			reply(executorDecisionChangedMessage)
			return
		}
		decision = current
		state = AnswerState{
			DecisionID: decision.NativeID,
			Revision:   decision.Revision,
			Answers:    make(map[string]Answer, len(decision.Questions)),
		}
	}

	question := decision.Questions[state.QuestionIx]
	answer, err := parseAnswer(action.Indexes, action.Text, question)
	if err != nil {
		reply(boundedDecisionReply(fmt.Sprintf("%v", err), decision, state.QuestionIx))
		return
	}
	state.Answers[question.ID] = answer

	if state.QuestionIx+1 < len(decision.Questions) {
		state.QuestionIx++
		e.storeAnswerState(sessionID, state)
		reply(boundedDecisionReply(
			fmt.Sprintf("已记录第 %d/%d 题答案。", state.QuestionIx, len(decision.Questions)),
			decision, state.QuestionIx))
		return
	}

	value, err := buildAnswerValue(decision, state.Answers)
	if err != nil {
		e.clearAnswerState(sessionID)
		reply("回答无效，请重新执行 /con 查看并回答。")
		return
	}
	receipt, err := e.api.SubmitAnswer(ctx, decision, value)
	e.clearAnswerState(sessionID)
	if err != nil {
		message := strings.TrimSpace(receipt.Message)
		if message == "" {
			message = "提交 dsh 回答失败，请核对 dsh 原生界面。"
		}
		reply(message)
		return
	}
	// 已提交成功；后续 /answer 必须看到新的 /con 展示才能再次回答。
	e.clearShownDecision(sessionID)
	if message := strings.TrimSpace(receipt.Message); message != "" {
		reply(message)
		return
	}
	reply("已提交。")
}

// answerState 读取会话未提交的答案收集进度。
//
// 读取来自终端快照 goroutine，写入来自命令处理 goroutine，必须使用 e.mu 同步。
func (e *Executor) answerState(sessionID string) (AnswerState, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	state, ok := e.answerStates[sessionID]
	return state, ok
}

// storeAnswerState 记录会话的答案收集进度。
func (e *Executor) storeAnswerState(sessionID string, state AnswerState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.answerStates[sessionID] = state
}

// clearAnswerState 丢弃会话的答案收集进度。
func (e *Executor) clearAnswerState(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.answerStates, sessionID)
}

// handleNewSession 在当前宿主上创建会话并立即切换选择。
func (e *Executor) handleNewSession(ctx context.Context, reply func(string), action command.Action) {
	title := sanitizeDecisionText(action.Title, decisionTextMaxBytes)
	if title == "" {
		title = defaultNewSessionTitle
	}
	directory, err := resolveSessionDirectory(action.Directory)
	if err != nil {
		reply("目录不存在或不是目录，请提供已存在的目录。")
		return
	}

	created, err := e.api.Create(ctx, directory, title)
	if err != nil || strings.TrimSpace(created.ID) == "" {
		reply("新建 dsh 会话失败，请稍后重试。")
		return
	}
	effectiveTitle := sanitizeDecisionText(created.Title, decisionTextMaxBytes)
	if effectiveTitle == "" {
		effectiveTitle = title
	}
	summary := SessionSummary{
		ID:        created.ID,
		Title:     effectiveTitle,
		Cwd:       directory,
		Available: true,
		UpdatedAt: time.Now().UnixMilli(),
	}
	target := e.sessionTarget(summary)

	e.mu.Lock()
	if e.sessions == nil {
		e.sessions = make(map[string]executorSession)
	}
	e.sessions[target.PaneID] = executorSession{
		summary:      summary,
		target:       target,
		pendingUntil: time.Now().Add(createdSessionGrace),
	}
	// 新建会话切换了目标，新目标必须重新 /con 展示待答事项。
	e.clearShownDecisionsForTargetLocked(e.selected)
	e.clearShownDecisionsForTargetLocked(target)
	e.selected = target
	e.createdTarget = target
	e.mu.Unlock()
	// 新会话目标不同，旧会话的终端分页缓存不能继续展示。
	e.buffer.Reset()

	var content strings.Builder
	fmt.Fprintf(&content, "已在 %s 新建 dsh 会话「%s」。", directory, effectiveTitle)
	if created.RenameFailed {
		content.WriteString("\n标题设置失败，当前标题可能为会话 ID。")
	}
	content.WriteString("\n已切换到新会话。")
	reply(content.String())
}

// resolveSessionDirectory 展开用户主目录并校验目录存在；不会创建目录。
func resolveSessionDirectory(raw string) (string, error) {
	directory := strings.TrimSpace(raw)
	if directory == "" || directory == "~" || strings.HasPrefix(directory, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		switch {
		case directory == "", directory == "~":
			directory = home
		default:
			directory = filepath.Join(home, strings.TrimPrefix(directory, "~/"))
		}
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("dsh 会话目录不是目录")
	}
	return absolute, nil
}

// ReadTerminalSnapshot 读取一个 dsh 目标的文本快照。
//
// 会话目标返回聊天正文并在末尾附上当前待处理审批或问题；宿主入口返回操作说明。
// 正文与提示合计不超过 20000 个 UTF-16 代码单元，超长时只截断正文，保留提示。
func (e *Executor) ReadTerminalSnapshot(ctx context.Context, paneID, occupant string, _ im.OutputMode, maxLines int) (im.TerminalContent, error) {
	if e == nil {
		return im.TerminalContent{}, session.ErrSelectionInvalid
	}
	e.mu.RLock()
	target, err := e.targetLocked(paneID, occupant)
	var summary SessionSummary
	if err == nil && target.Tab != hostEntryTab {
		state, ok := e.sessions[target.PaneID]
		if !ok {
			err = session.ErrSelectionInvalid
		} else {
			summary = state.summary
		}
	}
	e.mu.RUnlock()
	if err != nil {
		return im.TerminalContent{}, err
	}

	capturedAt := time.Now().UTC()
	if target.Tab == hostEntryTab {
		return im.TerminalContent{
			Mode:       im.OutputModeText,
			Text:       executorHostEntryMessage,
			Page:       &im.TerminalPage{Current: 1, Total: 1},
			CapturedAt: capturedAt,
		}, nil
	}

	text, err := e.api.Read(ctx, summary.ID)
	if err != nil {
		return im.TerminalContent{}, err
	}
	prompt, _ := e.pendingDecisionPrompt(ctx, summary.ID)
	return im.TerminalContent{
		Mode:       im.OutputModeText,
		Text:       boundedSnapshotText(transcriptTail(text, maxLines), prompt),
		Page:       &im.TerminalPage{Current: 1, Total: 1},
		CapturedAt: capturedAt,
	}, nil
}

// TakeCreatedTarget 返回并清除最近一次 /new-session 创建的目标。
func (e *Executor) TakeCreatedTarget() (session.Target, bool) {
	if e == nil {
		return session.Target{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	target := e.createdTarget
	e.createdTarget = session.Target{}
	if target.PaneID == "" {
		return session.Target{}, false
	}
	return target, true
}

// clearCreatedTarget 清除待领取的新建会话目标。
func (e *Executor) clearCreatedTarget() {
	e.mu.Lock()
	e.createdTarget = session.Target{}
	e.mu.Unlock()
}

// transcriptTail 按 maxLines 截取末尾若干行。
func transcriptTail(text string, maxLines int) string {
	lines := strings.Split(text, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

// boundedDecisionReply 组合前缀与决策提示，并限制到 HPRP 安全的 UTF-16 预算。
//
// 选项 label/description 与问题正文来自 dsh 宿主，长度不受 HPRP 约束；直接
// 拼接可能超过 MaxContentBytes 并导致 Server 关闭连接。
func boundedDecisionReply(prefix string, decision Decision, questionIndex int) string {
	return boundedSnapshotText(prefix, formatDecision(decision, questionIndex))
}

// boundedSnapshotText 组合正文与待答提示，总长不超过 UTF-16 上限。
//
// 超长时先截断正文（保留最新内容），提示完整保留；提示本身异常超长时保留其
// 末尾，因为 /answer 使用提示位于文本尾部。
func boundedSnapshotText(transcript, prompt string) string {
	prompt = truncateUTF16Tail(prompt, terminalSnapshotUTF16Limit)
	if strings.TrimSpace(prompt) == "" {
		return truncateUTF16Tail(transcript, terminalSnapshotUTF16Limit)
	}
	budget := terminalSnapshotUTF16Limit - utf16Units(snapshotSeparator) - utf16Units(prompt)
	if budget < 0 {
		budget = 0
	}
	transcript = strings.TrimRight(truncateUTF16Tail(transcript, budget), "\n")
	if transcript == "" {
		return prompt
	}
	return transcript + snapshotSeparator + prompt
}

// utf16Units 返回字符串按 UTF-16 代码单元计的长度。
func utf16Units(text string) int {
	units := 0
	for _, current := range text {
		width := utf16.RuneLen(current)
		if width < 1 {
			width = 1
		}
		units += width
	}
	return units
}
