package bridge

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/panel"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// ErrInvalidNotifierDependency 表示 Notifier 缺少必需依赖。
var ErrInvalidNotifierDependency = errors.New("Notifier 依赖无效")

// ReadRecentFunc 读取目标的 recent_unwrapped 终端快照。
type ReadRecentFunc func(ctx context.Context, target string, lines int) (herdr.ReadResult, error)

// GetAgentFunc 查询目标当前的 Agent occupant，供自动快照读取前后校验。
type GetAgentFunc func(ctx context.Context, target string) (herdr.AgentInfo, error)

type notificationKey struct {
	paneID            string
	occupantKey       string
	kind              string
	status            herdr.AgentStatus
	snapshotHash      string
	snapshotAvailable bool
}

// Notifier 根据 Agent 状态迁移向企业微信发送主动通知。
//
// 自动快照只保留独立的通知去重状态，不读取或修改手工 PanelBuffer。内部锁只保护去重
// 元数据，不跨越 Herdr 读取或企业微信发送调用。
type Notifier struct {
	im   IMAdapter
	get  GetAgentFunc
	read ReadRecentFunc

	mu       sync.Mutex
	recent   map[string]notificationKey
	inflight map[string]chan struct{}
	epoch    uint64
}

// NewNotifier 创建状态通知器，并要求自动读取前后可实时校验 Agent occupant。
func NewNotifier(im IMAdapter, get GetAgentFunc, read ReadRecentFunc) (*Notifier, error) {
	if im == nil || get == nil || read == nil {
		return nil, ErrInvalidNotifierDependency
	}
	return &Notifier{
		im:       im,
		get:      get,
		read:     read,
		recent:   make(map[string]notificationKey),
		inflight: make(map[string]chan struct{}),
	}, nil
}

// Reset 清空当前进程内的通知去重基线，供 Herdr 重连后的新健康周期使用。
func (n *Notifier) Reset() {
	if n == nil {
		return
	}
	n.mu.Lock()
	n.epoch++
	n.recent = make(map[string]notificationKey)
	n.mu.Unlock()
}

// HandleTransition 根据状态迁移发送主动通知。
func (n *Notifier) HandleTransition(ctx context.Context, transition session.Transition) error {
	if n == nil {
		return ErrInvalidNotifierDependency
	}
	title, includeSnapshot, notify := notificationPolicy(transition)
	if !notify {
		return nil
	}

	key := notificationKey{
		paneID:      transition.Target.PaneID,
		occupantKey: transition.Target.OccupantKey,
		kind:        "status",
		status:      transition.Current,
	}
	parts := renderStatusTitleParts(title, transition.Target)
	if includeSnapshot {
		before, err := n.get(ctx, transition.Target.TerminalID)
		if err == nil && session.MatchesAgent(transition.Target, before) {
			result, readErr := n.read(ctx, transition.Target.TerminalID, panel.PageSize)
			after, getErr := n.get(ctx, transition.Target.TerminalID)
			if readErr == nil && getErr == nil && session.MatchesAgent(transition.Target, after) && result.PaneID == transition.Target.PaneID {
				lines := lastNotificationLines(panel.Normalize(result.Text))
				key.snapshotAvailable = true
				key.snapshotHash = notificationHash(lines)
				parts = append(parts, renderNotificationSnapshot(transition.Target, lines)...)
			}
		}
	}
	return n.deliverOnce(ctx, transition.Target.PaneID, key, parts)
}

// TargetInvalidated 通知 pane 关闭或 occupant 替换。
func (n *Notifier) TargetInvalidated(ctx context.Context, target session.Target) error {
	if n == nil {
		return ErrInvalidNotifierDependency
	}
	key := notificationKey{paneID: target.PaneID, occupantKey: target.OccupantKey, kind: "invalidated"}
	return n.deliverOnce(ctx, target.PaneID, key, renderStatusTitleParts("Agent 目标已失效，请重新执行 /ls 和 /sel。", target))
}

func notificationPolicy(transition session.Transition) (title string, includeSnapshot, notify bool) {
	if transition.Previous == transition.Current {
		return "", false, false
	}
	switch transition.Current {
	case herdr.AgentStatusWorking:
		return "Agent 开始工作。", false, true
	case herdr.AgentStatusBlocked:
		return "Agent 已阻塞，需要你的处理。", true, true
	case herdr.AgentStatusDone:
		return "Agent 已完成。", true, true
	case herdr.AgentStatusIdle:
		if transition.Previous == herdr.AgentStatusWorking || transition.Previous == herdr.AgentStatusBlocked {
			return "Agent 已空闲。", true, true
		}
		return "", false, false
	case herdr.AgentStatusUnknown:
		return "Agent 状态无法可靠识别，请在 Herdr 中确认。", false, true
	default:
		return "", false, false
	}
}

func (n *Notifier) deliverOnce(ctx context.Context, paneID string, key notificationKey, parts []string) error {
	for {
		n.mu.Lock()
		if n.recent[paneID] == key {
			n.mu.Unlock()
			return nil
		}
		if pending := n.inflight[paneID]; pending != nil {
			n.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-pending:
				continue
			}
		}
		completed := make(chan struct{})
		n.inflight[paneID] = completed
		deliveryEpoch := n.epoch
		n.mu.Unlock()

		err := n.sendParts(ctx, parts)

		n.mu.Lock()
		if err == nil && n.epoch == deliveryEpoch {
			n.recent[paneID] = key
		}
		delete(n.inflight, paneID)
		close(completed)
		n.mu.Unlock()
		return err
	}
}

func (n *Notifier) sendParts(ctx context.Context, parts []string) error {
	for _, part := range parts {
		if err := n.im.SendMarkdown(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

func renderStatusTitle(title string, target session.Target) string {
	name := target.DisplayAgent
	if name == "" {
		name = target.Agent
	}
	if name == "" {
		name = target.PaneID
	}
	content := fmt.Sprintf("%s\n目标：%s（%s）", title, safeLabel(name), safeLabel(target.PaneID))
	if target.Title != "" {
		content += "\n标题：" + safeLabel(target.Title)
	}
	return content
}

func renderStatusTitleParts(title string, target session.Target) []string {
	content := renderStatusTitle(title, target)
	if len(content) <= panel.WeComContentLimit {
		return []string{content}
	}
	return panel.SplitMarkdown(content, panel.WeComContentLimit)
}

func renderNotificationSnapshot(target session.Target, lines []string) []string {
	content := panel.RenderPage(target, 0, lines)
	content = strings.Replace(content, "第 0 页", "范围：最近最多 100 行", 1)
	return panel.SplitMarkdown(content, panel.WeComContentLimit)
}

func lastNotificationLines(lines []string) []string {
	if len(lines) > panel.PageSize {
		lines = lines[len(lines)-panel.PageSize:]
	}
	return append([]string(nil), lines...)
}

func notificationHash(lines []string) string {
	digest := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return fmt.Sprintf("%x", digest)
}
