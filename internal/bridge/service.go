// Package bridge 编排企业微信入站命令与 Herdr 的受限交互。
package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/wenxichang/herdr-pal/internal/command"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
	"github.com/wenxichang/herdr-pal/internal/wecom"
)

var (
	// ErrInvalidServiceDependency 表示 BridgeService 缺少运行所需的依赖。
	ErrInvalidServiceDependency = errors.New("BridgeService 依赖无效")
)

// HerdrAPI 是入站命令所需的最小 Herdr 公共 API。
type HerdrAPI interface {
	// GetAgent 查询目标当前的 Agent occupant。
	GetAgent(ctx context.Context, target string) (herdr.AgentInfo, error)
	// ReadRecent 读取目标的 recent_unwrapped 终端快照。
	ReadRecent(ctx context.Context, target string, lines int) (herdr.ReadResult, error)
	// Prompt 向目标发送普通文本。
	Prompt(ctx context.Context, target, text string) error
	// SendKey 向目标发送已经过策略校验的单个按键。
	SendKey(ctx context.Context, target, key string) error
}

// IMAdapter 是入站命令回复所需的最小企业微信能力。
type IMAdapter interface {
	// RespondMarkdown 使用回调 req_id 发送第一段回复。
	RespondMarkdown(ctx context.Context, callbackRequestID, content string) error
	// SendMarkdown 发送同一用户可见的后续 Markdown 分段。
	SendMarkdown(ctx context.Context, content string) error
}

type clientHolder struct{ client HerdrAPI }

// Service 处理企业微信单聊文本命令。
//
// panel 及其页码只由 stateMu 保护。Herdr 与 IM 调用均在释放 Service 锁后进行，避免
// 传输层回调服务时出现锁重入；每次外部读取返回后都会用 generation 再次确认缓存仍有效。
type Service struct {
	registry *session.Registry
	panel    *panel.Buffer
	guard    *policy.Guard
	deduper  *policy.Deduper
	im       IMAdapter

	client atomic.Pointer[clientHolder]

	stateMu    sync.Mutex
	panelReady bool
	page       int
	generation uint64

	inputMu      sync.Mutex
	inputCond    *sync.Cond
	activeInputs int
	invalidating bool
}

// NewService 创建入站命令服务并校验全部依赖。
func NewService(registry *session.Registry, buffer *panel.Buffer, guard *policy.Guard, deduper *policy.Deduper, im IMAdapter) (*Service, error) {
	if registry == nil || buffer == nil || guard == nil || deduper == nil || im == nil {
		return nil, ErrInvalidServiceDependency
	}
	service := &Service{registry: registry, panel: buffer, guard: guard, deduper: deduper, im: im}
	service.inputCond = sync.NewCond(&service.inputMu)
	return service, nil
}

// SetHerdr 原子替换可用的 Herdr 客户端；nil 表示服务处于 degraded 状态。
func (s *Service) SetHerdr(client HerdrAPI) {
	if s == nil {
		return
	}
	s.client.Store(&clientHolder{client: client})
}

// InvalidateSelection 清空当前选择及手工终端分页缓存。
func (s *Service) InvalidateSelection() {
	if s == nil {
		return
	}
	s.inputMu.Lock()
	s.invalidating = true
	for s.activeInputs > 0 {
		s.inputCond.Wait()
	}
	s.inputMu.Unlock()
	s.registry.ClearSelection()
	s.resetPanel()
	s.inputMu.Lock()
	s.invalidating = false
	s.inputCond.Broadcast()
	s.inputMu.Unlock()
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
	targets := s.registry.CreateListSnapshot()
	if len(targets) == 0 {
		s.reply(ctx, message.RequestID, "当前没有可选择的 Agent。")
		return
	}
	selected, selectedErr := s.registry.ValidateSelected()
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
		fmt.Fprintf(&content, "\n%d. %s%s\n   工作区：%s / %s\n   状态：%s\n   面板：%s", index+1, safeLabel(name), marker, safeLabel(target.Workspace), safeLabel(target.Tab), safeLabel(string(target.Status)), safeLabel(target.PaneID))
	}
	content.WriteString("\n使用 /sel N 选择目标。")
	s.reply(ctx, message.RequestID, content.String())
}

func (s *Service) handleSelect(ctx context.Context, message wecom.IncomingText, index int) {
	target, err := s.registry.Select(index)
	if err != nil {
		s.InvalidateSelection()
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	s.resetPanel()
	name := target.DisplayAgent
	if name == "" {
		name = target.Agent
	}
	s.reply(ctx, message.RequestID, fmt.Sprintf("已选择 %s（面板：%s）。", safeLabel(name), safeLabel(target.PaneID)))
}

func (s *Service) handlePrompt(ctx context.Context, message wecom.IncomingText, text string) {
	client, ok := s.currentClient()
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, release, ok := s.selectedInputTarget(ctx, message, client)
	if !ok {
		return
	}
	defer release()
	if err := client.Prompt(ctx, target.TerminalID, text); err != nil {
		s.reply(ctx, message.RequestID, "发送失败，请稍后重试。")
		return
	}
	s.reply(ctx, message.RequestID, "已发送。")
}

func (s *Service) handleKey(ctx context.Context, message wecom.IncomingText, key string) {
	client, ok := s.currentClient()
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, release, ok := s.selectedInputTarget(ctx, message, client)
	if !ok {
		return
	}
	defer release()
	if err := s.guard.ValidateKey(key); err != nil {
		s.reply(ctx, message.RequestID, "按键不受支持。")
		return
	}
	if err := client.SendKey(ctx, target.TerminalID, key); err != nil {
		s.reply(ctx, message.RequestID, "按键发送失败，请稍后重试。")
		return
	}
	s.reply(ctx, message.RequestID, "按键已发送。")
}

func (s *Service) selectedInputTarget(ctx context.Context, message wecom.IncomingText, client HerdrAPI) (session.Target, func(), bool) {
	if !s.acquireInput() {
		s.reply(ctx, message.RequestID, "目标 Agent 已变化，请重新执行 /ls 和 /sel。")
		return session.Target{}, nil, false
	}
	target, err := s.registry.ValidateSelected()
	if err != nil {
		s.releaseInput()
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return session.Target{}, nil, false
	}
	current, err := client.GetAgent(ctx, target.TerminalID)
	if err != nil {
		s.releaseInput()
		s.reply(ctx, message.RequestID, unavailableMessage)
		return session.Target{}, nil, false
	}
	if !session.MatchesAgent(target, current) {
		s.releaseInput()
		s.InvalidateSelection()
		s.reply(ctx, message.RequestID, "目标 Agent 已变化，请重新执行 /ls 和 /sel。")
		return session.Target{}, nil, false
	}
	return target, s.releaseInput, true
}

func (s *Service) handleContent(ctx context.Context, message wecom.IncomingText) {
	client, ok := s.currentClient()
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, err := s.registry.ValidateSelected()
	if err != nil {
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	generation := s.currentGeneration()
	result, err := client.ReadRecent(ctx, target.TerminalID, panel.PageSize)
	if err != nil {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if result.PaneID != target.PaneID {
		s.InvalidateSelection()
		s.reply(ctx, message.RequestID, "终端目标已变化，请重新执行 /ls 和 /sel。")
		return
	}
	lines, page, applied := s.refreshPanel(target, generation, panel.Normalize(result.Text))
	if !applied {
		s.reply(ctx, message.RequestID, "选择已变化，请重新执行 /con。")
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, lines)
}

func (s *Service) handlePageUp(ctx context.Context, message wecom.IncomingText) {
	client, ok := s.currentClient()
	if !ok {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	target, err := s.registry.ValidateSelected()
	if err != nil {
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	linesToRead, generation, err := s.nextRead()
	if err != nil {
		s.reply(ctx, message.RequestID, pageErrorMessage(err))
		return
	}
	result, err := client.ReadRecent(ctx, target.TerminalID, linesToRead)
	if err != nil {
		s.reply(ctx, message.RequestID, unavailableMessage)
		return
	}
	if result.PaneID != target.PaneID {
		s.InvalidateSelection()
		s.reply(ctx, message.RequestID, "终端目标已变化，请重新执行 /ls 和 /sel。")
		return
	}
	lines, page, err := s.expandPanel(target, generation, panel.Normalize(result.Text))
	if err != nil {
		s.reply(ctx, message.RequestID, pageErrorMessage(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, lines)
}

func (s *Service) handlePageDown(ctx context.Context, message wecom.IncomingText) {
	if _, err := s.registry.ValidateSelected(); err != nil {
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	lines, page, err := s.pageDown()
	if err != nil {
		s.reply(ctx, message.RequestID, pageErrorMessage(err))
		return
	}
	target, err := s.registry.ValidateSelected()
	if err != nil {
		s.reply(ctx, message.RequestID, safeOperationError(err))
		return
	}
	s.replyPage(ctx, message.RequestID, target, page, lines)
}

func (s *Service) currentClient() (HerdrAPI, bool) {
	holder := s.client.Load()
	if holder == nil || holder.client == nil {
		return nil, false
	}
	return holder.client, true
}

func (s *Service) currentGeneration() uint64 {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.generation
}

func (s *Service) refreshPanel(target session.Target, generation uint64, normalized []string) ([]string, int, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.generation != generation {
		return nil, 0, false
	}
	s.panel.Refresh(target.OccupantKey, normalized)
	s.panelReady = true
	s.page = 0
	s.generation++
	return s.panel.Render(), s.page, true
}

func (s *Service) nextRead() (int, uint64, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.panelReady {
		return 0, s.generation, panel.ErrPanelChanged
	}
	lines, err := s.panel.NextReadSize()
	return lines, s.generation, err
}

func (s *Service) expandPanel(target session.Target, generation uint64, normalized []string) ([]string, int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.generation != generation || !s.panelReady {
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

func (s *Service) pageDown() ([]string, int, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.panelReady {
		return nil, 0, panel.ErrPanelChanged
	}
	if err := s.panel.PageDown(); err != nil {
		return nil, 0, err
	}
	s.page--
	s.generation++
	return s.panel.Render(), s.page, nil
}

func (s *Service) resetPanel() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.resetPanelLocked()
}

func (s *Service) resetPanelLocked() {
	s.panel.Reset()
	s.panelReady = false
	s.page = 0
	s.generation++
}

// acquireInput 建立一次输入操作的无锁外部调用租约。InvalidateSelection 会先阻止新租约，
// 再等待已开始的 Prompt/SendKey 返回，因此选择失效不会与危险输入交叉。
func (s *Service) acquireInput() bool {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if s.invalidating {
		return false
	}
	s.activeInputs++
	return true
}

func (s *Service) releaseInput() {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.activeInputs--
	if s.activeInputs == 0 {
		s.inputCond.Broadcast()
	}
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
