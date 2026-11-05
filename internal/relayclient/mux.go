package relayclient

import (
	"context"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/im"
	"github.com/wenxichang/herdr-pal/internal/session"
)

// ExecutorMux 合并多个本机后端，HPRP 仍只维护一条机器连接。
type ExecutorMux struct {
	backends []Executor
	mu       sync.RWMutex
	selected Executor
}

// NewExecutorMux 按传入顺序合并目录；各后端必须使用互不冲突的目标 ID。
func NewExecutorMux(backends ...Executor) *ExecutorMux { return &ExecutorMux{backends: backends} }

// CurrentTargets 返回所有健康后端的会话。
func (m *ExecutorMux) CurrentTargets() []session.Target {
	var result []session.Target
	for _, b := range m.backends {
		result = append(result, b.CurrentTargets()...)
	}
	if len(result) > hprp.MaxSessions {
		result = result[:hprp.MaxSessions]
	}
	return result
}
func (m *ExecutorMux) owner(paneID, occupant string) Executor {
	for _, b := range m.backends {
		for _, t := range b.CurrentTargets() {
			if t.PaneID == paneID && t.OccupantKey == occupant {
				return b
			}
		}
	}
	return nil
}

// SelectTarget 通过完整稳定身份定位后端并保留选择。
func (m *ExecutorMux) SelectTarget(paneID, occupant string) error {
	b := m.owner(paneID, occupant)
	if b == nil {
		return session.ErrSelectionInvalid
	}
	if err := b.SelectTarget(paneID, occupant); err != nil {
		return err
	}
	m.mu.Lock()
	m.selected = b
	m.mu.Unlock()
	return nil
}

// SelectedTarget 由被选后端重新核验目标。
func (m *ExecutorMux) SelectedTarget() (session.Target, error) {
	m.mu.RLock()
	b := m.selected
	m.mu.RUnlock()
	if b == nil {
		return session.Target{}, session.ErrNoSelection
	}
	return b.SelectedTarget()
}

// HandleMessage 仅转发到已选择的后端，授权由原后端策略执行。
func (m *ExecutorMux) HandleMessage(ctx context.Context, message im.IncomingText) {
	m.mu.RLock()
	b := m.selected
	m.mu.RUnlock()
	if b != nil {
		b.HandleMessage(ctx, message)
	}
}

// ReadTerminalSnapshot 按请求目标读取，不修改当前选择或手动分页。
func (m *ExecutorMux) ReadTerminalSnapshot(ctx context.Context, paneID, occupant string, mode im.OutputMode, maxLines int) (im.TerminalContent, error) {
	b := m.owner(paneID, occupant)
	if b == nil {
		return im.TerminalContent{}, session.ErrSelectionInvalid
	}
	return b.ReadTerminalSnapshot(ctx, paneID, occupant, mode, maxLines)
}
