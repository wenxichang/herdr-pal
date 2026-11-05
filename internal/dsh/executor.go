package dsh

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/command"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// API 是插件提供的最小能力，测试不需要 IM 或模型网络。
type API interface {
	Sessions(context.Context) ([]Session, error)
	Prompt(context.Context, Session, string, string) error
	Read(context.Context, Session) (string, error)
}

// Sink 接收命令回复及轻量状态通知。
type Sink interface {
	RespondMarkdown(context.Context, string, string) error
	SendNotification(context.Context, im.NotificationTarget, im.NotificationEvent) error
}

// Executor 将 dsh 会话映射为 HPRP 稳定目标，不提供按键和审批能力。
type Executor struct {
	api      API
	sink     Sink
	guard    *policy.Guard
	mu       sync.RWMutex
	sessions map[string]Session
	selected session.Target
	inputMu  sync.Mutex
	buffer   panel.Buffer
}

// NewExecutor 创建目录为空的执行器；Run 负责同步和故障恢复。
func NewExecutor(api API, sink Sink, guard *policy.Guard) *Executor {
	return &Executor{api: api, sink: sink, guard: guard, sessions: map[string]Session{}}
}

func targetOf(s Session) session.Target {
	return session.Target{PaneID: fmt.Sprintf("dsh:%x", sha256.Sum256([]byte(s.ID))), OccupantKey: s.Incarnation,
		Agent: "dsh", DisplayAgent: "DeepSeek Harness", Title: s.Title, Workspace: s.Workspace, Tab: "dsh", Status: herdr.AgentStatus(s.Status)}
}

// Run 定期同步目录；本地断线后立即撤下目录，下次成功读取恢复。
func (e *Executor) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
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

func (e *Executor) refresh(ctx context.Context) {
	list, err := e.api.Sessions(ctx)
	next := map[string]Session{}
	if err == nil {
		for _, s := range list {
			next[targetOf(s).PaneID] = s
		}
	}
	type notification struct {
		target im.NotificationTarget
		event  im.NotificationEvent
	}
	var notifications []notification
	e.mu.Lock()
	for key, s := range next {
		previous, ok := e.sessions[key]
		if !ok || previous.Incarnation != s.Incarnation {
			continue
		}
		if previous.Status == s.Status && (s.Status != "idle" || previous.Revision == s.Revision) {
			continue
		}
		status := s.Status
		if status == "idle" {
			status = "done"
		}
		notifications = append(notifications, notification{im.NotificationTarget{PaneID: key, OccupantHash: s.Incarnation, Agent: "dsh", DisplayAgent: "DeepSeek Harness", Title: s.Title}, im.NotificationEvent{Kind: im.NotificationKindAgentStatusChanged, PreviousStatus: previous.Status, Status: status, OccurredAt: time.Now()}})
	}
	e.sessions = next
	e.mu.Unlock()
	for _, n := range notifications {
		_ = e.sink.SendNotification(ctx, n.target, n.event)
	}
}

// CurrentTargets 返回独立副本，排序不依赖 map 遍历顺序。
func (e *Executor) CurrentTargets() []session.Target {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]session.Target, 0, len(e.sessions))
	for _, s := range e.sessions {
		result = append(result, targetOf(s))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PaneID < result[j].PaneID })
	return result
}

// SelectTarget 只接受目录中的精确身份，防止复用失效编号。
func (e *Executor) SelectTarget(paneID, occupant string) error {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[paneID]
	if !ok || s.Incarnation != occupant {
		return session.ErrSelectionInvalid
	}
	if e.selected.PaneID != paneID || e.selected.OccupantKey != occupant {
		e.buffer.Reset()
	}
	e.selected = targetOf(s)
	return nil
}

// SelectedTarget 每次都复核当前目录，不返回旧对象。
func (e *Executor) SelectedTarget() (session.Target, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sessions[e.selected.PaneID]
	if !ok || s.Incarnation != e.selected.OccupantKey {
		return session.Target{}, session.ErrSelectionInvalid
	}
	return targetOf(s), nil
}

func (e *Executor) lookup(paneID, occupant string) (Session, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, ok := e.sessions[paneID]
	if !ok || s.Incarnation != occupant {
		return Session{}, session.ErrSelectionInvalid
	}
	return s, nil
}

// ReadTerminalSnapshot 复用 HPRP 文本读取槽；内容来自 dsh 对话而非终端。
func (e *Executor) ReadTerminalSnapshot(ctx context.Context, paneID, occupant string, _ im.OutputMode, maxLines int) (im.TerminalContent, error) {
	s, err := e.lookup(paneID, occupant)
	if err != nil {
		return im.TerminalContent{}, err
	}
	text, err := e.api.Read(ctx, s)
	if err != nil {
		return im.TerminalContent{}, err
	}
	lines := strings.Split(text, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return im.TerminalContent{Mode: im.OutputModeText, Text: strings.Join(lines, "\n"), CapturedAt: time.Now()}, nil
}

// HandleMessage 复用命令语法和身份策略；普通文本只提交一次。
func (e *Executor) HandleMessage(ctx context.Context, message im.IncomingText) {
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.guard.Authorize(policy.Identity{UserID: message.UserID, ChatType: message.ChatType}) != nil {
		return
	}
	reply := func(text string) { _ = e.sink.RespondMarkdown(ctx, message.RequestID, text) }
	if message.MessageID == "" {
		reply("消息标识缺失，未执行操作。")
		return
	}
	action, err := command.Parse(message.Content)
	if err != nil {
		reply(err.Error())
		return
	}
	target, err := e.SelectedTarget()
	if err != nil {
		reply("dsh 会话已失效，请重新 /ls 并选择。")
		return
	}
	s, err := e.lookup(target.PaneID, target.OccupantKey)
	if err != nil {
		reply(err.Error())
		return
	}
	switch action.Kind {
	case command.KindPrompt:
		if s.Status != "idle" {
			reply("dsh 正在处理任务或等待本地操作，请稍后发送。")
			return
		}
		if err := e.api.Prompt(ctx, s, message.MessageID, action.Text); err != nil {
			reply(err.Error())
			return
		}
		reply("已发送给 dsh。")
	case command.KindContent, command.KindPageUp:
		text, err := e.api.Read(ctx, s)
		if err != nil {
			reply(err.Error())
			return
		}
		lines := strings.Split(text, "\n")
		key := target.PaneID + target.OccupantKey
		if action.Kind == command.KindContent {
			e.buffer.Refresh(key, lines)
		} else if err := e.buffer.Expand(key, lines); err != nil {
			reply(err.Error())
			return
		}
		output := strings.Join(e.buffer.Render(), "\n")
		if output == "" {
			output = "dsh 暂无对话文本。"
		}
		reply(output)
	case command.KindPageDown:
		if err := e.buffer.PageDown(); err != nil {
			reply(err.Error())
			return
		}
		reply(strings.Join(e.buffer.Render(), "\n"))
	case command.KindKey:
		reply("dsh 不支持终端按键或远程审批，请在 dsh 界面处理。")
	default:
		reply("请使用 /ls、/N、/con 或普通文本操作 dsh。")
	}
}
