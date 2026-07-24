// Package bridge 编排企业微信入站命令与 Herdr 的受限交互。
package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/wenxichang/herdr-pal/internal/command"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

// ErrInvalidServiceDependency 表示 BridgeService 缺少必需依赖。
var ErrInvalidServiceDependency = errors.New("BridgeService 依赖无效")

// HerdrAPI 是入站命令所需的最小 Herdr 公共 API。
type HerdrAPI interface {
	// GetAgent 查询目标当前的 Agent occupant。
	GetAgent(ctx context.Context, target string) (herdr.AgentInfo, error)
	// ReadRecent 读取目标的 recent_unwrapped 终端快照。
	ReadRecent(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
	// Prompt 向目标发送普通文本输入。
	Prompt(ctx context.Context, target, text string) error
	// SendKey 向目标发送一个已校验的 UI 按键。
	SendKey(ctx context.Context, target, key string) error
}

// IMAdapter 是入站命令回复所需的最小企业微信能力。
type IMAdapter interface {
	// RespondMarkdown 使用回调 req_id 发送首段 Markdown 回复。
	RespondMarkdown(ctx context.Context, callbackRequestID, content string) error
	// SendMarkdown 发送后续 Markdown 分段。
	SendMarkdown(ctx context.Context, content string) error
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

// NewService 创建入站命令服务并校验全部依赖。
func NewService(registry *session.Registry, buffer *panel.Buffer, guard *policy.Guard, deduper *policy.Deduper, im IMAdapter, keyAudit KeyAuditSink) (*Service, error) {
	if registry == nil || buffer == nil || guard == nil || deduper == nil || im == nil || keyAudit == nil {
		return nil, ErrInvalidServiceDependency
	}
	service := &Service{registry: registry, panel: buffer, guard: guard, deduper: deduper, im: im, keyAudit: keyAudit}
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
func (s *Service) HandleMessage(ctx context.Context, message wecom.IncomingText) {
	if s == nil || s.guard.Authorize(policy.Identity{UserID: message.UserID, ChatType: message.ChatType}) != nil {
		return
	}
	if strings.TrimSpace(message.MessageID) == "" {
		s.reply(ctx, message.RequestID, "消息标识缺失，未执行任何操作。")
		return
	}
	if !s.deduper.AddIfNew(message.MessageID) {
		return
	}
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
	case command.KindPrompt:
		s.handlePrompt(ctx, message, action.Text)
	case command.KindKey:
		s.handleKey(ctx, message, action.Key)
	default:
		s.reply(ctx, message.RequestID, "命令暂不支持。")
	}
}

func (s *Service) handleList(ctx context.Context, message wecom.IncomingText) {
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
		fmt.Fprintf(&content, "\n%d. %s%s\n   标题：%s\n   工作区：%s / %s\n   状态：%s\n   面板：%s", index+1, safeLabel(name), marker, safeLabel(target.Title), safeLabel(target.Workspace), safeLabel(target.Tab), safeLabel(string(target.Status)), safeLabel(target.PaneID))
	}
	content.WriteString("\n使用 /sel N 选择目标。")
	s.reply(ctx, message.RequestID, content.String())
}

func (s *Service) handleSelect(ctx context.Context, message wecom.IncomingText, index int) {
	s.transitionMu.Lock()
	s.beginInputBarrierLocked()
	s.stateMu.Lock()
	target, err := s.registry.Select(index)
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
	name := target.DisplayAgent
	if name == "" {
		name = target.Agent
	}
	s.reply(ctx, message.RequestID, fmt.Sprintf("已选择 %s（面板：%s）。", safeLabel(name), safeLabel(target.PaneID)))
}

func (s *Service) handlePrompt(ctx context.Context, message wecom.IncomingText, text string) {
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
	err = client.Prompt(ctx, target.PaneID, text)
	release()
	if err != nil {
		s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
		return
	}
	s.reply(ctx, message.RequestID, "已发送。")
}

func (s *Service) handleKey(ctx context.Context, message wecom.IncomingText, key string) {
	if err := s.guard.ValidateKey(key); err != nil {
		s.reply(ctx, message.RequestID, "按键不受支持。")
		return
	}
	client, release, availability := s.beginOperationDetailed(true)
	if availability != operationReady {
		if availability == operationUnavailable {
			s.auditUnavailableKey(message.UserID, key)
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
	audits, err := prepareKeyAudits(message.UserID, target, key, time.Now().UTC())
	if err != nil {
		release()
		s.reply(ctx, message.RequestID, "按键请求无效，未执行任何操作。")
		return
	}
	current, err := client.GetAgent(ctx, target.PaneID)
	if err != nil {
		s.keyAudit.RecordKeyAudit(audits.failed)
		release()
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if !session.MatchesAgent(target, current) {
		s.keyAudit.RecordKeyAudit(audits.rejected)
		release()
		s.invalidateExpected(target)
		s.reply(ctx, message.RequestID, targetChangedMessage)
		return
	}
	err = client.SendKey(ctx, target.PaneID, key)
	if err != nil {
		s.keyAudit.RecordKeyAudit(audits.failed)
	} else {
		s.keyAudit.RecordKeyAudit(audits.sent)
	}
	release()
	if err != nil {
		s.reply(ctx, message.RequestID, "按键发送失败，请稍后重试。")
		return
	}
	s.reply(ctx, message.RequestID, "按键已发送。")
}

func (s *Service) auditUnavailableKey(userID, key string) {
	target, err := s.selectedTarget()
	if err != nil {
		return
	}
	audits, err := prepareKeyAudits(userID, target, key, time.Now().UTC())
	if err != nil {
		return
	}
	s.keyAudit.RecordKeyAudit(audits.failed)
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

func (s *Service) handleContent(ctx context.Context, message wecom.IncomingText) {
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
	result, err := client.ReadRecent(ctx, target.PaneID, panel.PageSize)
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
	lines, page, err := s.applyRefresh(target, generation, panel.Normalize(result.Text))
	if err != nil {
		s.reply(ctx, message.RequestID, readApplyErrorMessage(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, lines)
}

func (s *Service) handlePageUp(ctx context.Context, message wecom.IncomingText) {
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
	result, err := client.ReadRecent(ctx, target.PaneID, linesToRead)
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
	lines, page, err := s.applyExpand(target, generation, panel.Normalize(result.Text))
	if err != nil {
		s.reply(ctx, message.RequestID, readApplyErrorMessage(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, lines)
}

func (s *Service) handlePageDown(ctx context.Context, message wecom.IncomingText) {
	s.stateMu.Lock()
	target, err := s.registry.ValidateSelected()
	if err == nil && s.beforePageDownReply != nil {
		s.beforePageDownReply()
	}
	if err == nil && !s.panelReady {
		err = panel.ErrPanelChanged
	}
	var lines []string
	page := 0
	if err == nil {
		err = s.panel.PageDown()
		if err == nil {
			s.page--
			s.generation++
			lines, page = s.panel.Render(), s.page
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
	s.replyPage(ctx, message.RequestID, target, page, lines)
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

func (s *Service) applyRefresh(expected session.Target, generation uint64, normalized []string) ([]string, int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	if err != nil {
		return nil, 0, err
	}
	if s.generation != generation || !sameTarget(target, expected) {
		return nil, 0, panel.ErrPanelChanged
	}
	s.panel.Refresh(target.OccupantKey, normalized)
	s.panelReady, s.page = true, 0
	s.generation++
	return s.panel.Render(), s.page, nil
}

func (s *Service) applyExpand(expected session.Target, generation uint64, normalized []string) ([]string, int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	target, err := s.registry.ValidateSelected()
	if err != nil {
		return nil, 0, err
	}
	if s.generation != generation || !sameTarget(target, expected) || !s.panelReady {
		return nil, 0, panel.ErrPanelChanged
	}
	if err := s.panel.Expand(target.OccupantKey, normalized); err != nil {
		if errors.Is(err, panel.ErrPanelChanged) {
			s.resetPanelLocked()
		}
		return nil, 0, err
	}
	s.page++
	s.generation++
	return s.panel.Render(), s.page, nil
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

func (s *Service) replyPage(ctx context.Context, requestID string, target session.Target, page int, lines []string) {
	s.reply(ctx, requestID, panel.RenderPage(target, page, lines))
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
		return
	}
	for _, part := range parts[1:] {
		if err := s.im.SendMarkdown(ctx, part); err != nil {
			return
		}
	}
}

const unavailableMessage = "Herdr 暂不可用，操作未执行，请稍后重试。"
const targetChangedMessage = "目标 Agent 已变化，请重新执行 /ls 和 /sel。"

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
