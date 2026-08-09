// Package bridge 编排企业微信入站命令与 Herdr 的受限交互。
package bridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/wenxichang/herdr-pal/internal/command"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/terminalimage"
)

// ErrInvalidServiceDependency 表示 BridgeService 缺少必需依赖。
var (
	ErrInvalidServiceDependency = errors.New("BridgeService 依赖无效")
	ErrTerminalImageUnsupported = errors.New("当前连接不支持终端图片")
)

const (
	promptRecoveryTimeout = 5 * time.Second
	keySequenceInterval   = 100 * time.Millisecond
	keyReadbackDelay      = 200 * time.Millisecond
)

type keyIntervalWaiter func(context.Context, time.Duration) error

// HerdrAPI 是入站命令所需的最小 Herdr 公共 API。
type HerdrAPI interface {
	// Snapshot 读取当前 Herdr 会话的权威结构快照。
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
	// GetAgent 查询目标当前的 Agent occupant。
	GetAgent(ctx context.Context, target string) (herdr.AgentInfo, error)
	// ReadRecent 读取目标的 recent_unwrapped 终端快照。
	ReadRecent(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
	// ReadRecentANSI 读取目标保留物理渲染行的 recent ANSI 终端快照。
	ReadRecentANSI(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
	// ReadVisibleANSI 读取目标当前可见终端页面的 ANSI 快照。
	ReadVisibleANSI(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
	// PromptUntilStateChange 向目标发送普通文本并等待首次状态变化。
	PromptUntilStateChange(ctx context.Context, target, text string) (herdr.AgentInfo, error)
	// WaitForStateChange 等待目标的状态变化序列离开 baseline。
	WaitForStateChange(ctx context.Context, target string, baseline uint64, timeout time.Duration) (herdr.AgentInfo, error)
	// SendKey 向目标发送一个已校验的 UI 按键。
	SendKey(ctx context.Context, target, key string) error
}

// IMAdapter 是入站命令回复所需的平台中立能力。
type IMAdapter interface {
	im.ReplySink
}

// TerminalRenderer 把安全 ANSI 终端页渲染为 PNG8。
type TerminalRenderer interface {
	Render(ctx context.Context, safeANSI string) (terminalimage.Result, error)
}

// KeyAuditSink 同步接收已经过安全字段校验的显式按键审计记录。
type KeyAuditSink interface {
	// RecordKeyAudit 记录一次按键处理结果。
	RecordKeyAudit(audit policy.KeyAudit)
}

type clientHolder struct{ client HerdrAPI }

// Service 处理企业微信单聊文本命令。
//
// 锁顺序始终为 transitionMu → opMu → stateMu。transitionMu 仅串行失效、选择和
// 客户端切换；opMu 只登记租约与等待在途调用；stateMu 只保护 Registry 与 Panel 的复合
// 本地状态。三把锁均不会跨越 Herdr 或企业微信调用。
type Service struct {
	registry *session.Registry
	panel    *panel.Buffer
	guard    *policy.Guard
	deduper  *policy.Deduper
	im       IMAdapter
	keyAudit KeyAuditSink
	logger   *slog.Logger
	renderer TerminalRenderer

	waitKeyInterval keyIntervalWaiter
	waitKeyReadback keyIntervalWaiter

	client atomic.Pointer[clientHolder]

	transitionMu sync.Mutex
	opMu         sync.Mutex
	opCond       *sync.Cond
	opsBlocked   int
	inputBlocked int
	activeOps    int
	activeInputs int

	stateMu    sync.Mutex
	panelReady bool
	page       int
	generation uint64

	// 仅供同包并发回归测试在关键临界区建立确定性同步点；nil 时没有运行时行为。
	beforeInvalidateStateChange func()
	beforePageDownReply         func()
}

// SetTerminalRenderer 设置 Relay 模式使用的终端图片渲染器。
func (s *Service) SetTerminalRenderer(renderer TerminalRenderer) error {
	if s == nil || renderer == nil {
		return ErrInvalidServiceDependency
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.renderer != nil {
		return ErrInvalidServiceDependency
	}
	s.renderer = renderer
	return nil
}

// NewService 创建入站命令服务并校验全部依赖。
func NewService(registry *session.Registry, buffer *panel.Buffer, guard *policy.Guard, deduper *policy.Deduper, im IMAdapter, keyAudit KeyAuditSink, logger *slog.Logger) (*Service, error) {
	if registry == nil || buffer == nil || guard == nil || deduper == nil || im == nil || keyAudit == nil || logger == nil {
		return nil, ErrInvalidServiceDependency
	}
	service := &Service{
		registry: registry, panel: buffer, guard: guard, deduper: deduper,
		im: im, keyAudit: keyAudit, logger: logger,
		waitKeyInterval: waitKeyInterval, waitKeyReadback: waitKeyInterval,
	}
	service.opCond = sync.NewCond(&service.opMu)
	return service, nil
}

// SetHerdr 原子替换可用客户端；nil 会线性化为 degraded，并等待旧客户端操作结束。
func (s *Service) SetHerdr(client HerdrAPI) {
	if s == nil {
		return
	}
	s.transitionMu.Lock()
	defer s.transitionMu.Unlock()
	if client != nil {
		s.opMu.Lock()
		s.client.Store(&clientHolder{client: client})
		s.opMu.Unlock()
		return
	}
	s.opMu.Lock()
	s.opsBlocked++
	s.client.Store(&clientHolder{})
	for s.activeOps > 0 {
		s.opCond.Wait()
	}
	s.opsBlocked--
	s.opCond.Broadcast()
	s.opMu.Unlock()
}

// InvalidateSelection 清空当前选择及手工终端分页缓存。
func (s *Service) InvalidateSelection() {
	if s == nil {
		return
	}
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	if s.beforeInvalidateStateChange != nil {
		s.beforeInvalidateStateChange()
	}
	s.registry.ClearSelection()
	s.resetPanelLocked()
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
}

// CurrentTargets 返回当前可上报给 Relay Server 的稳定排序目标副本。
func (s *Service) CurrentTargets() []session.Target {
	if s == nil {
		return nil
	}
	holder := s.client.Load()
	if holder == nil || holder.client == nil {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.registry.CurrentTargets()
}

// SelectedTarget 返回当前仍有效的本机稳定选择。
func (s *Service) SelectedTarget() (session.Target, error) {
	if s == nil {
		return session.Target{}, session.ErrNoSelection
	}
	return s.selectedTarget()
}

// SelectTarget 按 pane 和 occupant 稳定身份选择本机目标。
func (s *Service) SelectTarget(paneID, occupantKey string) error {
	if s == nil {
		return session.ErrListSnapshotExpired
	}
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	previous, previousErr := s.registry.ValidateSelected()
	selected, err := s.registry.SelectTarget(paneID, occupantKey)
	if err == nil && (previousErr != nil || !sameTarget(previous, selected)) {
		s.resetPanelLocked()
	}
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
	return err
}

// ReadTerminalSnapshot 无副作用读取稳定目标的一次终端快照。
func (s *Service) ReadTerminalSnapshot(
	ctx context.Context,
	paneID, occupantKey string,
	mode im.OutputMode,
	maxLines int,
) (im.TerminalContent, error) {
	if s == nil || maxLines < 1 || maxLines > panel.MaxLines {
		return im.TerminalContent{}, ErrInvalidServiceDependency
	}
	mode = normalizedOutputMode(mode)
	if mode == "" {
		return im.TerminalContent{}, ErrInvalidServiceDependency
	}
	client, release, ok := s.beginOperation(false)
	if !ok {
		return im.TerminalContent{}, herdr.ErrUnavailable
	}
	defer release()
	target, err := s.registry.ResolveTarget(paneID, occupantKey)
	if err != nil {
		return im.TerminalContent{}, err
	}
	if mode == im.OutputModeImage {
		capture, captureErr := s.captureDirectTerminalImage(ctx, client, target, maxLines)
		if _, err := s.registry.ResolveTarget(paneID, occupantKey); err != nil {
			return im.TerminalContent{}, err
		}
		return capture.content, captureErr
	}
	result, err := s.readLatestANSI(ctx, client, target.PaneID, maxLines, mode)
	if err != nil {
		return im.TerminalContent{}, err
	}
	if result.PaneID != target.PaneID {
		return im.TerminalContent{}, session.ErrListSnapshotExpired
	}
	if _, err := s.registry.ResolveTarget(paneID, occupantKey); err != nil {
		return im.TerminalContent{}, err
	}
	lines := normalizeTerminalANSI(result.Text, mode, target.Columns)
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	content, renderErr := s.buildTerminalContent(ctx, panel.Page{Lines: lines, Current: 1, Total: 1}, mode)
	if _, err := s.registry.ResolveTarget(paneID, occupantKey); err != nil {
		return im.TerminalContent{}, err
	}
	return content, renderErr
}

// ReplaceSnapshot 用新的 Herdr 会话快照原子替换目标索引。
//
// reconnect 为 true 或当前选择因 occupant 变化失效时，会同时清空手工终端分页缓存。
// 调用期间会阻止新输入并等待已开始的 Prompt 或 SendKey 完成。
func (s *Service) ReplaceSnapshot(snapshot herdr.Snapshot, reconnect bool) session.ChangeSet {
	return s.replaceSnapshot(snapshot, reconnect, false)
}

// ReplaceSnapshotPreservingStatus 原子替换结构快照，并为相同 occupant 保留当前状态。
//
// 仅当现有 status stream 连续有效时使用；新增或替换 occupant 仍采用 snapshot 状态。
func (s *Service) ReplaceSnapshotPreservingStatus(snapshot herdr.Snapshot) session.ChangeSet {
	return s.replaceSnapshot(snapshot, false, true)
}

func (s *Service) replaceSnapshot(snapshot herdr.Snapshot, reconnect, preserveStatus bool) session.ChangeSet {
	if s == nil {
		return session.ChangeSet{}
	}
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	var changes session.ChangeSet
	if preserveStatus {
		changes = s.registry.ReplacePreservingStatus(snapshot)
	} else {
		changes = s.registry.Replace(snapshot, reconnect)
	}
	if reconnect || changes.SelectionInvalidated {
		s.resetPanelLocked()
	}
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
	return changes
}

// HandleMessage 处理一条已完成企业微信协议校验的文本回调。
func (s *Service) HandleMessage(ctx context.Context, message im.IncomingText) {
	if s == nil {
		return
	}
	if s.guard.Authorize(policy.Identity{UserID: message.UserID, ChatType: message.ChatType}) != nil {
		reason := "user_mismatch"
		if message.ChatType != "single" {
			reason = "chat_type"
		} else if message.UserID == "" {
			reason = "user_missing"
		}
		s.logger.Warn("企业微信消息被策略拒绝",
			"reason", reason,
			"user_hash", bridgeShortHash(message.UserID),
			"chat_type", message.ChatType,
		)
		return
	}
	if strings.TrimSpace(message.MessageID) == "" {
		s.logger.Warn("企业微信消息标识缺失", "user_hash", bridgeShortHash(message.UserID))
		s.reply(ctx, message.RequestID, "消息标识缺失，未执行任何操作。")
		return
	}
	if !s.deduper.AddIfNew(message.MessageID) {
		s.logger.Info("企业微信重复消息已忽略",
			"user_hash", bridgeShortHash(message.UserID),
			"message_hash", bridgeShortHash(message.MessageID),
		)
		return
	}
	s.logger.Info("企业微信消息已接收",
		"user_hash", bridgeShortHash(message.UserID),
		"message_hash", bridgeShortHash(message.MessageID),
	)
	action, err := command.Parse(message.Content)
	if err != nil {
		s.reply(ctx, message.RequestID, safeCommandError(err))
		return
	}
	switch action.Kind {
	case command.KindList:
		s.handleList(ctx, message)
	case command.KindSelect:
		s.handleSelect(ctx, message, action.Index)
	case command.KindContent:
		s.handleContent(ctx, message)
	case command.KindPageUp:
		s.handlePageUp(ctx, message)
	case command.KindPageDown:
		s.handlePageDown(ctx, message)
	case command.KindHelp:
		s.reply(ctx, message.RequestID, command.HelpText())
	case command.KindPrompt:
		s.handlePrompt(ctx, message, action.Text)
	case command.KindKey:
		s.handleKeys(ctx, message, action.Keys)
	default:
		s.reply(ctx, message.RequestID, "命令暂不支持。")
	}
}

func bridgeShortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:8])
}

func (s *Service) handleList(ctx context.Context, message im.IncomingText) {
	client, release, availability := s.beginOperationDetailed(false)
	if availability == operationReady {
		snapshot, err := client.Snapshot(ctx)
		release()
		if err != nil || !herdr.IsSupportedProtocol(snapshot.Protocol) {
			s.reply(ctx, message.RequestID, unavailableMessage)
			return
		}
		s.ReplaceSnapshotPreservingStatus(snapshot)
	}
	s.stateMu.Lock()
	targets := s.registry.CreateListSnapshot()
	selected, selectedErr := s.registry.ValidateSelected()
	s.stateMu.Unlock()
	if len(targets) == 0 {
		s.reply(ctx, message.RequestID, "当前没有可选择的 Agent。")
		return
	}
	var content strings.Builder
	content.WriteString("可选择的 Agent：")
	for index, target := range targets {
		marker := ""
		if selectedErr == nil && sameTarget(target, selected) {
			marker = "（当前选择）"
		}
		name := target.DisplayAgent
		if name == "" {
			name = target.Agent
		}
		fmt.Fprintf(&content, "\n%d. %s%s\n   标题：%s\n   工作区：%s\n   状态：%s\n   面板：%s", index+1, safeLabel(name), marker, safeLabel(target.Title), safeLabel(panel.WorkspaceLabel(target.Workspace, target.Tab)), safeLabel(panel.AgentStatusLabel(target.Status)), safeLabel(target.PaneID))
	}
	content.WriteString("\n使用 /N 选择目标。")
	s.reply(ctx, message.RequestID, content.String())
}

func (s *Service) handleSelect(ctx context.Context, message im.IncomingText, index int) {
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	_, err := s.registry.Select(index)
	if err == nil {
		s.resetPanelLocked()
	} else if errors.Is(err, session.ErrListSnapshotExpired) {
		s.registry.ClearSelection()
		s.resetPanelLocked()
	}
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
	if err != nil {
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	s.handleContent(ctx, message)
}

func (s *Service) handlePrompt(ctx context.Context, message im.IncomingText, text string) {
	client, release, ok := s.beginOperation(true)
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, err := s.selectedTarget()
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	current, err := client.GetAgent(ctx, target.PaneID)
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if !session.MatchesAgent(target, current) {
		release()
		s.invalidateExpected(target)
		s.reply(ctx, message.RequestID, targetChangedMessage)
		return
	}
	if !canAcceptPrompt(current.AgentStatus) {
		release()
		s.reply(ctx, message.RequestID, fmt.Sprintf("Agent 当前状态为 %s，暂不接受普通文本。", safeLabel(string(current.AgentStatus))))
		return
	}
	changed, err := client.PromptUntilStateChange(ctx, target.PaneID, text)
	if err == nil && !session.MatchesAgent(target, changed) {
		release()
		if s.rebindExpected(target, changed) {
			s.reply(ctx, message.RequestID, fmt.Sprintf("消息已发送，Agent 会话已切换并已自动选择新会话。当前状态：%s。", safeLabel(string(changed.AgentStatus))))
		} else {
			s.invalidateExpected(target)
			s.reply(ctx, message.RequestID, targetChangedAfterPromptMessage)
		}
		return
	}
	if err != nil && !isPromptStalled(err) {
		release()
		s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
		return
	}
	if err != nil {
		audits, auditErr := prepareKeyAudits(message.UserID, target, "enter", time.Now().UTC())
		if auditErr != nil {
			release()
			s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
			return
		}
		rechecked, getErr := client.GetAgent(ctx, target.PaneID)
		if getErr != nil {
			s.keyAudit.RecordKeyAudit(audits.failed)
			release()
			s.reply(ctx, message.RequestID, unavailableMessage)
			return
		}
		if !session.MatchesAgent(target, rechecked) {
			s.keyAudit.RecordKeyAudit(audits.rejected)
			release()
			s.invalidateExpected(target)
			s.reply(ctx, message.RequestID, targetChangedMessage)
			return
		}
		if promptStateChanged(current, rechecked) {
			s.keyAudit.RecordKeyAudit(audits.rejected)
			release()
			s.replyPromptSuccess(ctx, message.RequestID, rechecked.AgentStatus)
			return
		}
		if !canAcceptPrompt(rechecked.AgentStatus) {
			s.keyAudit.RecordKeyAudit(audits.rejected)
			release()
			s.reply(ctx, message.RequestID, fmt.Sprintf("Agent 当前状态为 %s，未补发 Enter。", safeLabel(string(rechecked.AgentStatus))))
			return
		}
		if sendErr := client.SendKey(ctx, target.PaneID, "enter"); sendErr != nil {
			s.keyAudit.RecordKeyAudit(audits.failed)
			release()
			s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
			return
		}
		s.keyAudit.RecordKeyAudit(audits.sent)
		changed, err = client.WaitForStateChange(ctx, target.PaneID, current.StateChangeSeq, promptRecoveryTimeout)
		if err != nil {
			release()
			if errors.Is(err, herdr.ErrAgentStateChangeTimeout) {
				s.reply(ctx, message.RequestID, "消息已送入终端，但未检测到 Agent 状态变化，请检查 Agent 界面。")
			} else {
				s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
			}
			return
		}
		if !session.MatchesAgent(target, changed) {
			release()
			s.invalidateExpected(target)
			s.reply(ctx, message.RequestID, targetChangedMessage)
			return
		}
	}
	release()
	if !promptStateChanged(current, changed) {
		s.reply(ctx, message.RequestID, "消息已送入终端，但未检测到 Agent 状态变化，请检查 Agent 界面。")
		return
	}
	s.replyPromptSuccess(ctx, message.RequestID, changed.AgentStatus)
}

func canAcceptPrompt(status herdr.AgentStatus) bool {
	return status == herdr.AgentStatusIdle || status == herdr.AgentStatusDone
}

func promptStateChanged(before, after herdr.AgentInfo) bool {
	return before.AgentStatus != after.AgentStatus || before.StateChangeSeq != after.StateChangeSeq
}

func isPromptStalled(err error) bool {
	var apiErr *herdr.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "agent_prompt_stalled"
}

func (s *Service) replyPromptSuccess(ctx context.Context, requestID string, status herdr.AgentStatus) {
	s.reply(ctx, requestID, fmt.Sprintf("已发送，Agent 状态已变为 %s。", safeLabel(string(status))))
}

func (s *Service) handleKeys(ctx context.Context, message im.IncomingText, keys []string) {
	if len(keys) == 0 {
		s.reply(ctx, message.RequestID, "按键请求无效，未执行任何操作。")
		return
	}
	for _, key := range keys {
		if err := s.guard.ValidateKey(key); err != nil {
			s.reply(ctx, message.RequestID, "按键不受支持。")
			return
		}
	}
	client, release, availability := s.beginOperationDetailed(true)
	if availability != operationReady {
		if availability == operationUnavailable {
			s.auditUnavailableKeys(message.UserID, keys)
		}
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, err := s.selectedTarget()
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}

	prepared := make([]preparedKeyAudits, len(keys))
	for index, key := range keys {
		prepared[index], err = prepareKeyAudits(message.UserID, target, key, time.Now().UTC())
		if err != nil {
			release()
			s.reply(ctx, message.RequestID, "按键请求无效，未执行任何操作。")
			return
		}
	}

	sent := 0
	failureMessage := ""
	for index, key := range keys {
		current, getErr := client.GetAgent(ctx, target.PaneID)
		if getErr != nil {
			s.keyAudit.RecordKeyAudit(prepared[index].failed)
			release()
			s.reply(ctx, message.RequestID, keySequenceSummary(sent, len(keys), fmt.Sprintf("第 %d 个按键执行前无法确认 Agent，后续未执行。", index+1)))
			return
		}
		if !session.MatchesAgent(target, current) {
			s.keyAudit.RecordKeyAudit(prepared[index].rejected)
			release()
			s.invalidateExpected(target)
			s.reply(ctx, message.RequestID, keySequenceSummary(sent, len(keys), targetChangedMessage))
			return
		}
		if sendErr := client.SendKey(ctx, target.PaneID, key); sendErr != nil {
			s.keyAudit.RecordKeyAudit(prepared[index].failed)
			failureMessage = fmt.Sprintf("第 %d 个按键发送失败，后续未执行。", index+1)
			break
		}
		s.keyAudit.RecordKeyAudit(prepared[index].sent)
		sent++
		if index+1 < len(keys) {
			if waitErr := s.waitKeyInterval(ctx, keySequenceInterval); waitErr != nil {
				failureMessage = "按键序列已取消，后续未执行。"
				break
			}
		}
	}
	if sent > 0 {
		if waitErr := s.waitKeyReadback(ctx, keyReadbackDelay); waitErr != nil {
			release()
			s.reply(ctx, message.RequestID, keySequenceSummary(sent, len(keys), failureMessage)+"\n\n控制台刷新已取消，请重新执行 /con。")
			return
		}
	}

	refreshTarget, generation, err := s.captureContentTarget()
	if err != nil || !sameTarget(refreshTarget, target) {
		release()
		s.reply(ctx, message.RequestID, keySequenceSummary(sent, len(keys), failureMessage)+"\n\n控制台刷新失败，请重新执行 /con。")
		return
	}
	summary := keySequenceSummary(sent, len(keys), failureMessage)
	if normalizedOutputMode(message.OutputMode) == im.OutputModeImage {
		capture, captureErr := s.captureDirectTerminalImage(ctx, client, target, panel.PageSize)
		release()
		if captureErr != nil {
			s.reply(ctx, message.RequestID, summary+"\n\n控制台读取失败，请执行 /con 重试。")
			return
		}
		page, applyErr := s.applyRefresh(target, generation, capture.imageLines)
		if applyErr != nil {
			s.reply(ctx, message.RequestID, summary+"\n\n控制台刷新失败："+readApplyErrorMessage(applyErr))
			return
		}
		capture.content.Page = &im.TerminalPage{Current: page.Current, Total: page.Total}
		s.replyTerminalContent(ctx, message.RequestID, target, capture.content, summary)
		return
	}
	result, readErr := s.readLatestANSI(ctx, client, target.PaneID, panel.PageSize, message.OutputMode)
	release()
	if readErr != nil {
		s.reply(ctx, message.RequestID, summary+"\n\n控制台读取失败，请执行 /con 重试。")
		return
	}
	if result.PaneID != target.PaneID {
		s.invalidateExpected(target)
		s.reply(ctx, message.RequestID, summary+"\n\n控制台目标已变化，请重新执行 /ls 并使用 /N 选择目标。")
		return
	}
	page, applyErr := s.applyRefresh(target, generation, normalizeTerminalANSI(result.Text, message.OutputMode, target.Columns))
	if applyErr != nil {
		s.reply(ctx, message.RequestID, summary+"\n\n控制台刷新失败："+readApplyErrorMessage(applyErr))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, message.OutputMode, summary)
}

func keySequenceSummary(sent, total int, failureMessage string) string {
	if failureMessage == "" {
		return fmt.Sprintf("按键已发送（%d/%d）。", sent, total)
	}
	return fmt.Sprintf("已发送 %d/%d 个按键；%s", sent, total, failureMessage)
}

func waitKeyInterval(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) auditUnavailableKeys(userID string, keys []string) {
	target, err := s.selectedTarget()
	if err != nil {
		return
	}
	for _, key := range keys {
		audits, auditErr := prepareKeyAudits(userID, target, key, time.Now().UTC())
		if auditErr != nil {
			return
		}
		s.keyAudit.RecordKeyAudit(audits.failed)
	}
}

type preparedKeyAudits struct {
	sent     policy.KeyAudit
	rejected policy.KeyAudit
	failed   policy.KeyAudit
}

func prepareKeyAudits(userID string, target session.Target, key string, at time.Time) (preparedKeyAudits, error) {
	results := []policy.AuditResult{policy.AuditResultSent, policy.AuditResultRejected, policy.AuditResultFailed}
	audits := make([]policy.KeyAudit, len(results))
	for index, result := range results {
		audit, err := policy.NewKeyAudit(userID, target.PaneID, target.OccupantKey, key, at, result)
		if err != nil {
			return preparedKeyAudits{}, err
		}
		audits[index] = audit
	}
	return preparedKeyAudits{sent: audits[0], rejected: audits[1], failed: audits[2]}, nil
}

func (s *Service) handleContent(ctx context.Context, message im.IncomingText) {
	client, release, ok := s.beginOperation(false)
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, generation, err := s.captureContentTarget()
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	if normalizedOutputMode(message.OutputMode) == im.OutputModeImage {
		capture, captureErr := s.captureDirectTerminalImage(ctx, client, target, panel.PageSize)
		release()
		if captureErr != nil {
			s.reply(ctx, message.RequestID, safeOperationError(captureErr))
			return
		}
		page, applyErr := s.applyRefresh(target, generation, capture.imageLines)
		if applyErr != nil {
			s.reply(ctx, message.RequestID, readApplyErrorMessage(applyErr))
			return
		}
		capture.content.Page = &im.TerminalPage{Current: page.Current, Total: page.Total}
		s.replyTerminalContent(ctx, message.RequestID, target, capture.content, "")
		return
	}
	result, err := s.readLatestANSI(ctx, client, target.PaneID, panel.PageSize, message.OutputMode)
	release()
	if err != nil {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if result.PaneID != target.PaneID {
		s.invalidateExpected(target)
		s.reply(ctx, message.RequestID, targetChangedMessage)
		return
	}
	page, err := s.applyRefresh(target, generation, normalizeTerminalANSI(result.Text, message.OutputMode, target.Columns))
	if err != nil {
		s.reply(ctx, message.RequestID, readApplyErrorMessage(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, message.OutputMode, "")
}

func (s *Service) handlePageUp(ctx context.Context, message im.IncomingText) {
	client, release, ok := s.beginOperation(false)
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, linesToRead, generation, err := s.capturePageUpTarget()
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, readApplyErrorMessage(err))
		return
	}
	result, err := client.ReadRecentANSI(ctx, target.PaneID, linesToRead)
	release()
	if err != nil {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if result.PaneID != target.PaneID {
		s.invalidateExpected(target)
		s.reply(ctx, message.RequestID, targetChangedMessage)
		return
	}
	page, err := s.applyExpand(target, generation, normalizeTerminalANSI(result.Text, message.OutputMode, target.Columns))
	if err != nil {
		s.reply(ctx, message.RequestID, readApplyErrorMessage(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, message.OutputMode, "")
}

func (s *Service) handlePageDown(ctx context.Context, message im.IncomingText) {
	s.stateMu.Lock()
	target, err := s.registry.ValidateSelected()
	if err == nil && s.beforePageDownReply != nil {
		s.beforePageDownReply()
	}
	if err == nil && !s.panelReady {
		err = panel.ErrPanelChanged
	}
	var page panel.Page
	if err == nil {
		err = s.panel.PageDown()
		if err == nil {
			s.page--
			s.generation++
			page = s.panel.RenderTerminal()
		}
	}
	s.stateMu.Unlock()
	if err != nil {
		if errors.Is(err, session.ErrNoSelection) || errors.Is(err, session.ErrSelectionInvalid) {
			s.reply(ctx, message.RequestID, safeOperationError(err))
		} else {
			s.reply(ctx, message.RequestID, pageErrorMessage(err))
		}
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, message.OutputMode, "")
}

func (s *Service) selectedTarget() (session.Target, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.registry.ValidateSelected()
}

// invalidateExpected 仅在旧请求的选择仍是当前选择时才清理状态。不能以分页 generation
// 判断，因为翻页不代表 Agent occupant 已替换。
func (s *Service) invalidateExpected(expected session.Target) bool {
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	current, err := s.registry.ValidateSelected()
	invalidated := err == nil && sameTarget(current, expected)
	if invalidated {
		s.registry.ClearSelection()
		s.resetPanelLocked()
	}
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
	return invalidated
}

func (s *Service) rebindExpected(expected session.Target, current herdr.AgentInfo) bool {
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	_, err := s.registry.RebindSelected(expected, current)
	if err == nil {
		s.resetPanelLocked()
	}
	s.stateMu.Unlock()
	s.endInputBarrierLocked()
	s.transitionMu.Unlock()
	return err == nil
}

func (s *Service) captureContentTarget() (session.Target, uint64, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	return target, s.generation, err
}

func (s *Service) capturePageUpTarget() (session.Target, int, uint64, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	if err != nil {
		return session.Target{}, 0, s.generation, err
	}
	if !s.panelReady {
		return session.Target{}, 0, s.generation, panel.ErrPanelChanged
	}
	lines, err := s.panel.NextReadSize()
	return target, lines, s.generation, err
}

func (s *Service) applyRefresh(expected session.Target, generation uint64, normalized []panel.Line) (panel.Page, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	if err != nil {
		return panel.Page{}, err
	}
	if s.generation != generation || !sameTarget(target, expected) {
		return panel.Page{}, panel.ErrPanelChanged
	}
	s.panel.RefreshTerminal(target.OccupantKey, normalized)
	s.panelReady, s.page = true, 0
	s.generation++
	return s.panel.RenderTerminal(), nil
}

func (s *Service) applyExpand(expected session.Target, generation uint64, normalized []panel.Line) (panel.Page, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	if err != nil {
		return panel.Page{}, err
	}
	if s.generation != generation || !sameTarget(target, expected) || !s.panelReady {
		return panel.Page{}, panel.ErrPanelChanged
	}
	if err := s.panel.ExpandTerminal(target.OccupantKey, normalized); err != nil {
		if errors.Is(err, panel.ErrPanelChanged) {
			s.resetPanelLocked()
		}
		return panel.Page{}, err
	}
	s.page++
	s.generation++
	return s.panel.RenderTerminal(), nil
}

func (s *Service) beginOperation(input bool) (HerdrAPI, func(), bool) {
	client, release, availability := s.beginOperationDetailed(input)
	return client, release, availability == operationReady
}

type operationAvailability uint8

const (
	operationReady operationAvailability = iota
	operationUnavailable
	operationTransitioning
)

func (s *Service) beginOperationDetailed(input bool) (HerdrAPI, func(), operationAvailability) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	holder := s.client.Load()
	if input && s.inputBlocked > 0 {
		return nil, nil, operationTransitioning
	}
	if s.opsBlocked > 0 || holder == nil || holder.client == nil {
		return nil, nil, operationUnavailable
	}
	s.activeOps++
	if input {
		s.activeInputs++
	}
	return holder.client, func() { s.endOperation(input) }, operationReady
}

func (s *Service) endOperation(input bool) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.activeOps--
	if input {
		s.activeInputs--
	}
	s.opCond.Broadcast()
}

// beginInputBarrierLocked 在 transitionMu 已锁定时阻止新输入并等待已有输入结束。
func (s *Service) beginInputBarrierLocked() {
	s.opMu.Lock()
	s.inputBlocked++
	for s.activeInputs > 0 {
		s.opCond.Wait()
	}
	s.opMu.Unlock()
}

func (s *Service) endInputBarrierLocked() {
	s.opMu.Lock()
	s.inputBlocked--
	s.opCond.Broadcast()
	s.opMu.Unlock()
}

func (s *Service) resetPanelLocked() {
	s.panel.Reset()
	s.panelReady = false
	s.page = 0
	s.generation++
}

func (s *Service) replyPage(ctx context.Context, requestID string, target session.Target, page panel.Page, mode im.OutputMode, note string) {
	mode = normalizedOutputMode(mode)
	content, err := s.buildTerminalContent(ctx, page, mode)
	if err != nil {
		s.reply(ctx, requestID, safeOperationError(err))
		return
	}
	s.replyTerminalContent(ctx, requestID, target, content, note)
}

func (s *Service) replyTerminalContent(ctx context.Context, requestID string, target session.Target, content im.TerminalContent, note string) {
	if sink, ok := s.im.(im.TerminalReplySink); ok {
		if err := sink.RespondTerminal(ctx, requestID, content); err != nil {
			s.logIMDeliveryFailure("IM 终端回复发送失败", requestID, 1, 1, len(content.Text), err)
			return
		}
		if note != "" {
			if err := s.im.SendMarkdown(ctx, note); err != nil {
				s.logIMDeliveryFailure("IM 终端说明发送失败", requestID, 1, 1, len(note), err)
			}
		}
		return
	}
	if content.Mode == im.OutputModeImage {
		s.reply(ctx, requestID, ErrTerminalImageUnsupported.Error())
		return
	}
	current, total := 1, 1
	if content.Page != nil {
		current, total = content.Page.Current, content.Page.Total
	}
	markdown := panel.RenderPageWithTotal(target, current, total, strings.Split(content.Text, "\n"))
	if note != "" {
		markdown = panel.AppendRenderedPageNote(markdown, note)
	}
	s.reply(ctx, requestID, markdown)
}

type directTerminalImageCapture struct {
	content    im.TerminalContent
	imageLines []panel.Line
}

func (s *Service) captureDirectTerminalImage(
	ctx context.Context,
	client HerdrAPI,
	expected session.Target,
	maxLines int,
) (directTerminalImageCapture, error) {
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		s.logger.Warn("终端图片几何快照读取失败", "pane_id", expected.PaneID, "reason", safeOperationError(err))
		return directTerminalImageCapture{}, err
	}
	if err := herdr.ValidateProtocol(snapshot.Version, snapshot.Protocol); err != nil {
		s.logger.Warn("终端图片几何快照协议不兼容", "pane_id", expected.PaneID, "reason", safeOperationError(err))
		return directTerminalImageCapture{}, err
	}
	freshRegistry := &session.Registry{}
	freshRegistry.Replace(snapshot, false)
	freshTarget, err := freshRegistry.ResolveTarget(expected.PaneID, expected.OccupantKey)
	if err != nil {
		return directTerminalImageCapture{}, err
	}
	s.logger.Info("终端图片几何快照已读取",
		"pane_id", freshTarget.PaneID,
		"columns", freshTarget.Columns,
		"rows", freshTarget.Rows,
		"geometry_source", "session.snapshot",
	)

	visible, err := s.readLatestANSI(ctx, client, freshTarget.PaneID, maxLines, im.OutputModeImage)
	if err != nil {
		return directTerminalImageCapture{}, err
	}
	if visible.PaneID != freshTarget.PaneID {
		return directTerminalImageCapture{}, session.ErrListSnapshotExpired
	}
	imageLines := normalizeTerminalANSI(visible.Text, im.OutputModeImage, freshTarget.Columns)
	safeANSI := panel.JoinANSI(imageLines)
	capturedAt := time.Now().UTC()
	image, renderErr := s.renderTerminalImage(ctx, safeANSI, 1, 1, len(imageLines))

	audit, auditErr := client.ReadRecent(ctx, freshTarget.PaneID, maxLines)
	if auditErr != nil {
		s.logger.Warn("终端图片审计文本读取失败",
			"pane_id", freshTarget.PaneID,
			"source", "recent_unwrapped",
			"lines_requested", maxLines,
			"reason", safeOperationError(auditErr),
		)
		if renderErr != nil {
			return directTerminalImageCapture{}, errors.Join(renderErr, auditErr)
		}
		return directTerminalImageCapture{}, auditErr
	}
	if audit.PaneID != freshTarget.PaneID {
		return directTerminalImageCapture{}, session.ErrListSnapshotExpired
	}
	auditLines := panel.Normalize(audit.Text)
	if len(auditLines) > maxLines {
		auditLines = auditLines[len(auditLines)-maxLines:]
	}
	s.logger.Info("终端图片审计文本已读取",
		"pane_id", freshTarget.PaneID,
		"source", "recent_unwrapped",
		"lines_requested", maxLines,
		"line_count", len(auditLines),
		"text_bytes", len(audit.Text),
		"truncated", audit.Truncated,
	)

	content := im.TerminalContent{
		Mode: im.OutputModeImage, Text: strings.Join(auditLines, "\n"),
		Page: &im.TerminalPage{Current: 1, Total: 1}, CapturedAt: capturedAt,
		Image: image,
	}
	if renderErr != nil {
		content.Mode = im.OutputModeText
		content.Image = nil
	}
	return directTerminalImageCapture{content: content, imageLines: imageLines}, renderErr
}

func (s *Service) renderTerminalImage(
	ctx context.Context,
	safeANSI string,
	pageCurrent, pageTotal, lineCount int,
) (*im.TerminalImage, error) {
	s.stateMu.Lock()
	renderer := s.renderer
	s.stateMu.Unlock()
	if renderer == nil {
		return nil, ErrTerminalImageUnsupported
	}
	result, err := renderer.Render(ctx, safeANSI)
	if err != nil {
		s.logger.Warn("终端图片生成失败",
			"output_mode", im.OutputModeImage,
			"page_current", pageCurrent,
			"page_total", pageTotal,
			"line_count", lineCount,
			"ansi_bytes", len(safeANSI),
			"reason", safeOperationError(err),
		)
		return nil, err
	}
	s.logger.Info("终端图片已生成",
		"output_mode", im.OutputModeImage,
		"page_current", pageCurrent,
		"page_total", pageTotal,
		"line_count", lineCount,
		"ansi_bytes", len(safeANSI),
		"image_width", result.Width,
		"image_height", result.Height,
		"image_bytes", len(result.PNG),
	)
	return &im.TerminalImage{
		MediaType: "image/png", Data: append([]byte(nil), result.PNG...),
		Width: result.Width, Height: result.Height, ColorMode: "indexed-256",
	}, nil
}

func (s *Service) buildTerminalContent(ctx context.Context, page panel.Page, mode im.OutputMode) (im.TerminalContent, error) {
	mode = normalizedOutputMode(mode)
	if mode == "" {
		return im.TerminalContent{}, ErrInvalidServiceDependency
	}
	if page.Current <= 0 || page.Total <= 0 {
		page.Current, page.Total = 1, 1
	}
	content := im.TerminalContent{
		Mode: mode, Text: panel.JoinText(page.Lines),
		Page: &im.TerminalPage{Current: page.Current, Total: page.Total}, CapturedAt: time.Now().UTC(),
	}
	if mode == im.OutputModeText {
		return content, nil
	}
	safeANSI := panel.JoinANSI(page.Lines)
	image, err := s.renderTerminalImage(ctx, safeANSI, page.Current, page.Total, len(page.Lines))
	if err != nil {
		content.Mode = im.OutputModeText
		return content, err
	}
	content.Image = image
	return content, nil
}

func normalizedOutputMode(mode im.OutputMode) im.OutputMode {
	if mode == "" {
		return im.OutputModeText
	}
	if mode == im.OutputModeText || mode == im.OutputModeImage {
		return mode
	}
	return ""
}

func (s *Service) readLatestANSI(ctx context.Context, client HerdrAPI, target string, lines int, mode im.OutputMode) (herdr.ReadResult, error) {
	mode = normalizedOutputMode(mode)
	source := "recent"
	read := client.ReadRecentANSI
	if mode == im.OutputModeImage {
		source = "visible"
		read = client.ReadVisibleANSI
	}
	result, err := read(ctx, target, lines)
	if err != nil {
		s.logger.Warn("终端 ANSI 快照读取失败",
			"pane_id", target,
			"output_mode", mode,
			"source", source,
			"lines_requested", lines,
			"reason", safeOperationError(err),
		)
		return herdr.ReadResult{}, err
	}
	s.logger.Info("终端 ANSI 快照已读取",
		"pane_id", target,
		"returned_pane_id", result.PaneID,
		"output_mode", mode,
		"source", source,
		"lines_requested", lines,
		"text_bytes", len(result.Text),
		"truncated", result.Truncated,
	)
	return result, nil
}

func normalizeTerminalANSI(text string, mode im.OutputMode, columns int) []panel.Line {
	if normalizedOutputMode(mode) == im.OutputModeImage && columns > 0 {
		return panel.NormalizeANSIWidth(text, columns)
	}
	return panel.NormalizeANSI(text)
}

func (s *Service) reply(ctx context.Context, requestID, content string) {
	if strings.TrimSpace(requestID) == "" {
		return
	}
	parts := panel.SplitMarkdown(content, panel.WeComContentLimit)
	if len(parts) == 0 {
		parts = []string{"操作未完成，请稍后重试。"}
	}
	if err := s.im.RespondMarkdown(ctx, requestID, parts[0]); err != nil {
		s.logIMDeliveryFailure("IM 回复发送失败", requestID, 1, len(parts), len(parts[0]), err)
		return
	}
	for index, part := range parts[1:] {
		if err := s.im.SendMarkdown(ctx, part); err != nil {
			s.logIMDeliveryFailure("IM 后续消息发送失败", requestID, index+2, len(parts), len(part), err)
			return
		}
	}
}

func (s *Service) logIMDeliveryFailure(message, requestID string, partIndex, partCount, contentLength int, err error) {
	s.logger.Warn(message,
		"request_hash", bridgeShortHash(requestID),
		"part_index", partIndex,
		"part_count", partCount,
		"content_length", contentLength,
		"error_type", notificationDeliveryErrorType(err),
		"reason", safeNotificationErrorReason(err),
	)
}

const unavailableMessage = "Herdr 暂不可用，操作未执行，请稍后重试。"
const targetChangedMessage = "目标 Agent 已变化，请重新执行 /ls 并使用 /N 选择目标。"
const targetChangedAfterPromptMessage = "消息已发送，但 Agent 会话在发送过程中发生变化。为避免后续消息投递到错误会话，请重新执行 /ls 并使用 /N 选择目标。"

func safeCommandError(err error) string {
	if errors.Is(err, command.ErrInvalidCommand) {
		return err.Error()
	}
	return "命令无法识别，请使用 /ls 查看可用操作。"
}

func safeOperationError(err error) string {
	if err == nil {
		return "操作未完成，请稍后重试。"
	}
	return safeLabel(err.Error())
}

func pageErrorMessage(err error) string {
	switch {
	case errors.Is(err, panel.ErrPanelChanged):
		return "终端内容已变化，请重新执行 /con。"
	case errors.Is(err, panel.ErrOldestPage):
		return "已经是最早可读取内容。"
	case errors.Is(err, panel.ErrNewestPage):
		return "已经是最新内容。"
	default:
		return "分页不可用，请重新执行 /con。"
	}
}

func readApplyErrorMessage(err error) string {
	if errors.Is(err, session.ErrNoSelection) || errors.Is(err, session.ErrSelectionInvalid) {
		return safeOperationError(err)
	}
	return pageErrorMessage(err)
}

func sameTarget(left, right session.Target) bool {
	return left.PaneID == right.PaneID && left.OccupantKey == right.OccupantKey
}

func safeLabel(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "", "```", "``\u200b`").Replace(value)
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value))
}
