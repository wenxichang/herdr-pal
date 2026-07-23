// Package session 保存可由 Herdr snapshot 重建的运行时索引。
package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

// Target 表示可被企业微信用户选择的 Agent 面板。
type Target struct {
	// PaneID 是 Herdr 面板标识。
	PaneID string
	// TerminalID 是承载 Agent 的终端标识。
	TerminalID string
	// OccupantKey 是稳定的 Agent occupant 身份摘要。
	OccupantKey string
	// Agent 是 Herdr 检测到的 Agent 标识。
	Agent string
	// DisplayAgent 是面向用户展示的 Agent 名称。
	DisplayAgent string
	// Title 是 Agent 当前展示标题。
	Title string
	// Status 是 Agent 当前生命周期状态。
	Status herdr.AgentStatus
	// Workspace 是面向用户展示的工作区名称。
	Workspace string
	// Tab 是面向用户展示的标签页名称。
	Tab string
}

// ChangeSet 描述完整快照替换后 Agent 集合和选择状态的变化。
type ChangeSet struct {
	// AgentSetChanged 表示 Agent pane 集合或 occupant 身份发生变化。
	AgentSetChanged bool
	// SelectionInvalidated 表示当前选择已被清空。
	SelectionInvalidated bool
	// RemovedTargets 是此次快照中消失的旧目标。
	RemovedTargets []Target
	// ReplacedTargets 是此次快照中 occupant 被替换的旧目标。
	ReplacedTargets []Target
}

// Transition 描述一个 Agent 状态事件应用前后的状态。
type Transition struct {
	// Target 是更新后的目标副本。
	Target Target
	// Previous 是事件应用前的状态。
	Previous herdr.AgentStatus
	// Current 是事件中的当前状态。
	Current herdr.AgentStatus
}

// Registry 保存当前 Agent 面板、最近一次列表编号和用户选择。
//
// Registry 的零值可以直接使用。
type Registry struct {
	mu           sync.RWMutex
	targets      map[string]Target
	orders       map[string]targetOrder
	listSnapshot []listEntry
	selectedKey  string
	selectedPane string
}

type targetOrder struct {
	workspaceNumber int
	tabNumber       int
}

type listEntry struct {
	paneID      string
	occupantKey string
}

// Replace 用完整 snapshot 替换当前 Agent 视图，并返回集合变化。
func (r *Registry) Replace(snapshot herdr.Snapshot, reconnect bool) ChangeSet {
	nextTargets, nextOrders := targetsFromSnapshot(snapshot)

	r.mu.Lock()
	defer r.mu.Unlock()

	changes := ChangeSet{}
	previousOrders := r.orders
	for paneID, current := range r.targets {
		next, exists := nextTargets[paneID]
		if !exists {
			changes.AgentSetChanged = true
			changes.RemovedTargets = append(changes.RemovedTargets, current)
			continue
		}
		if current.OccupantKey != next.OccupantKey {
			changes.AgentSetChanged = true
			changes.ReplacedTargets = append(changes.ReplacedTargets, current)
		}
	}
	for paneID := range nextTargets {
		if _, exists := r.targets[paneID]; !exists {
			changes.AgentSetChanged = true
		}
	}

	r.targets = nextTargets
	r.orders = nextOrders
	if reconnect {
		if r.selectedKey != "" {
			changes.SelectionInvalidated = true
		}
		r.selectedKey = ""
		r.selectedPane = ""
		r.listSnapshot = nil
	} else if r.selectedKey != "" {
		selected, exists := r.targets[r.selectedPane]
		if !exists || selected.OccupantKey != r.selectedKey {
			changes.SelectionInvalidated = true
			r.selectedKey = ""
			r.selectedPane = ""
		}
	}
	sortTargetsByOrder(changes.RemovedTargets, previousOrders)
	sortTargetsByOrder(changes.ReplacedTargets, previousOrders)
	return changes
}

// CreateListSnapshot 返回当前 Agent 的稳定列表，并替换旧编号快照。
func (r *Registry) CreateListSnapshot() []Target {
	r.mu.Lock()
	defer r.mu.Unlock()

	targets := sortedTargets(r.targets, r.orders)
	r.listSnapshot = make([]listEntry, len(targets))
	for index, target := range targets {
		r.listSnapshot[index] = listEntry{paneID: target.PaneID, occupantKey: target.OccupantKey}
	}
	return targets
}

// Select 从最近一次列表快照按一开始的编号选择目标。
func (r *Registry) Select(index int) (Target, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.listSnapshot) == 0 {
		return Target{}, fmt.Errorf("尚无 Agent 列表，请先执行 /ls")
	}
	if index < 1 || index > len(r.listSnapshot) {
		return Target{}, fmt.Errorf("选择编号超出列表范围：1 到 %d", len(r.listSnapshot))
	}
	entry := r.listSnapshot[index-1]
	target, found := r.targets[entry.paneID]
	if !found || target.OccupantKey != entry.occupantKey {
		return Target{}, fmt.Errorf("列表快照已过期，请重新执行 /ls")
	}
	r.selectedKey = target.OccupantKey
	r.selectedPane = target.PaneID
	return target, nil
}

// ValidateSelected 返回仍有效的当前选择；失效选择会被清除。
func (r *Registry) ValidateSelected() (Target, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.selectedKey == "" {
		return Target{}, fmt.Errorf("尚未选择 Agent，请先执行 /ls 和 /sel")
	}
	target, found := r.targets[r.selectedPane]
	if !found || target.OccupantKey != r.selectedKey {
		r.selectedKey = ""
		r.selectedPane = ""
		return Target{}, fmt.Errorf("已选择的 Agent 已失效，请重新执行 /ls 和 /sel")
	}
	return target, nil
}

// ApplyStatus 将状态事件应用到已存在的 Agent 面板。
func (r *Registry) ApplyStatus(event herdr.AgentStatusEvent) (Transition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	target, found := r.targets[event.PaneID]
	if !found {
		return Transition{}, fmt.Errorf("未知 Agent 面板：%s", event.PaneID)
	}
	previous := target.Status
	target.Status = event.AgentStatus
	r.targets[event.PaneID] = target
	return Transition{Target: target, Previous: previous, Current: event.AgentStatus}, nil
}

// AgentPaneIDs 返回当前全部 Agent pane ID 的稳定排序副本。
func (r *Registry) AgentPaneIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	targets := sortedTargets(r.targets, r.orders)
	paneIDs := make([]string, len(targets))
	for index, target := range targets {
		paneIDs[index] = target.PaneID
	}
	return paneIDs
}

// MatchesAgent 判断实时 agent.get 结果是否仍代表同一 Agent occupant。
func MatchesAgent(target Target, current herdr.AgentInfo) bool {
	if current.Agent == nil || target.PaneID != current.PaneID || target.TerminalID != current.TerminalID {
		return false
	}
	if target.Agent != *current.Agent {
		return false
	}
	return target.OccupantKey == occupantKey(
		current.TerminalID,
		*current.Agent,
		stringValue(current.DisplayAgent),
		current.AgentSession,
	)
}

// ClearSelection 清空当前选择和列表编号快照。
func (r *Registry) ClearSelection() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.selectedKey = ""
	r.selectedPane = ""
	r.listSnapshot = nil
}

func targetsFromSnapshot(snapshot herdr.Snapshot) (map[string]Target, map[string]targetOrder) {
	workspaces := make(map[string]herdr.Workspace, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		workspaces[workspace.WorkspaceID] = workspace
	}
	tabs := make(map[string]herdr.Tab, len(snapshot.Tabs))
	for _, tab := range snapshot.Tabs {
		tabs[tab.TabID] = tab
	}

	targets := make(map[string]Target, len(snapshot.Panes))
	orders := make(map[string]targetOrder, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		if pane.Agent == nil {
			continue
		}
		workspace := workspaces[pane.WorkspaceID]
		tab := tabs[pane.TabID]
		targets[pane.PaneID] = Target{
			PaneID:       pane.PaneID,
			TerminalID:   pane.TerminalID,
			OccupantKey:  occupantKey(pane.TerminalID, *pane.Agent, stringValue(pane.DisplayAgent), pane.AgentSession),
			Agent:        *pane.Agent,
			DisplayAgent: stringValue(pane.DisplayAgent),
			Title:        stringValue(pane.Title),
			Status:       pane.AgentStatus,
			Workspace:    labelOrID(workspace.Label, pane.WorkspaceID),
			Tab:          labelOrID(tab.Label, pane.TabID),
		}
		orders[pane.PaneID] = targetOrder{workspaceNumber: workspace.Number, tabNumber: tab.Number}
	}
	return targets, orders
}

func sortedTargets(targets map[string]Target, orders map[string]targetOrder) []Target {
	result := make([]Target, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	sortTargetsByOrder(result, orders)
	return result
}

func sortTargetsByOrder(targets []Target, orders map[string]targetOrder) {
	sort.Slice(targets, func(left, right int) bool {
		leftOrder := orders[targets[left].PaneID]
		rightOrder := orders[targets[right].PaneID]
		if leftOrder.workspaceNumber != rightOrder.workspaceNumber {
			return leftOrder.workspaceNumber < rightOrder.workspaceNumber
		}
		if leftOrder.tabNumber != rightOrder.tabNumber {
			return leftOrder.tabNumber < rightOrder.tabNumber
		}
		return targets[left].PaneID < targets[right].PaneID
	})
}

func occupantKey(terminalID, agent, displayAgent string, session *herdr.AgentSession) string {
	identity := struct {
		TerminalID   string
		Agent        string
		DisplayAgent string
		HasSession   bool
		Source       string
		Kind         string
		Value        string
	}{
		TerminalID:   terminalID,
		Agent:        agent,
		DisplayAgent: displayAgent,
	}
	if session != nil {
		identity.DisplayAgent = ""
		identity.HasSession = true
		identity.Source = session.Source
		identity.Kind = session.Kind
		identity.Value = session.Value
	}
	encoded, _ := json.Marshal(identity)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest)
}

func labelOrID(label, id string) string {
	if label != "" {
		return label
	}
	return id
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
