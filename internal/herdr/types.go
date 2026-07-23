package herdr

import (
	"context"
	"encoding/json"
	"strings"
)

// RequiredProtocol 是 Herdr Pal 支持的 Herdr Socket API 协议版本，必须与审计的 Schema 精确匹配。
const RequiredProtocol uint32 = 17

// AgentStatus 表示 Herdr 识别到的 Agent 生命周期状态。
type AgentStatus string

const (
	// AgentStatusIdle 表示 Agent 可以接收输入。
	AgentStatusIdle AgentStatus = "idle"
	// AgentStatusWorking 表示 Agent 正在执行任务。
	AgentStatusWorking AgentStatus = "working"
	// AgentStatusBlocked 表示 Agent 正在等待用户处理。
	AgentStatusBlocked AgentStatus = "blocked"
	// AgentStatusDone 表示 Agent 已完成且尚未被查看。
	AgentStatusDone AgentStatus = "done"
	// AgentStatusUnknown 表示 Herdr 无法可靠识别 Agent 状态。
	AgentStatusUnknown AgentStatus = "unknown"
)

// SubscriptionSpec 描述一个 Herdr 公开事件订阅。
type SubscriptionSpec struct {
	// Type 是 Herdr 定义的事件订阅类型。
	Type string `json:"type"`
	// PaneID 是 pane.agent_status_changed 所需的面板标识。
	PaneID string `json:"pane_id,omitempty"`
}

// Event 表示 Herdr 事件流中的一条原始事件。
type Event struct {
	// Kind 是 Herdr 推送的事件名称。
	Kind string
	// Data 是未经业务解释的事件对象。
	Data json.RawMessage
}

// AgentStatusEvent 表示 pane.agent_status_changed 事件。
type AgentStatusEvent struct {
	// PaneID 是状态变化的面板标识。
	PaneID string
	// WorkspaceID 是该面板所在工作区标识。
	WorkspaceID string
	// AgentStatus 是 Agent 当前生命周期状态。
	AgentStatus AgentStatus
	// Agent 是可选的 Agent 标识。
	Agent *string
	// Title 是可选的展示标题。
	Title *string
	// DisplayAgent 是可选的面向用户展示的 Agent 名称。
	DisplayAgent *string
	// StateLabels 是状态的可选展示标签。
	StateLabels map[string]string
}

// SubscriptionStream 表示一个保持打开的 Herdr 事件订阅连接。
type SubscriptionStream interface {
	// Recv 阻塞直到接收到下一条事件或 context 被取消。
	Recv(ctx context.Context) (Event, error)
	// Close 关闭订阅连接；可重复调用。
	Close() error
}

// AgentSession 表示 Agent 提供的可恢复会话标识。
type AgentSession struct {
	// Source 是会话标识的来源。
	Source string `json:"source"`
	// Agent 是提供会话标识的 Agent 名称。
	Agent string `json:"agent"`
	// Kind 是会话标识的类别。
	Kind string `json:"kind"`
	// Value 是会话标识的值。
	Value string `json:"value"`
}

// AgentInfo 表示 Herdr 中一个被检测到的 Agent。
type AgentInfo struct {
	// TerminalID 是承载 Agent 的终端标识。
	TerminalID string `json:"terminal_id"`
	// Name 是 Agent 的用户命名。
	Name *string `json:"name"`
	// Agent 是 Herdr 检测到的 Agent 标识。
	Agent *string `json:"agent"`
	// Title 是 Agent 当前展示标题。
	Title *string `json:"title"`
	// TerminalTitle 是原始终端标题。
	TerminalTitle *string `json:"terminal_title"`
	// TerminalTitleStripped 是清理控制字符后的终端标题。
	TerminalTitleStripped *string `json:"terminal_title_stripped"`
	// DisplayAgent 是面向用户展示的 Agent 名称。
	DisplayAgent *string `json:"display_agent"`
	// AgentStatus 是当前 Agent 生命周期状态。
	AgentStatus AgentStatus `json:"agent_status"`
	// AgentSession 是可选的 Agent 恢复会话标识。
	AgentSession *AgentSession `json:"agent_session"`
	// WorkspaceID 是 Agent 所在工作区标识。
	WorkspaceID string `json:"workspace_id"`
	// TabID 是 Agent 所在标签页标识。
	TabID string `json:"tab_id"`
	// PaneID 是 Agent 所在面板标识。
	PaneID string `json:"pane_id"`
}

// Workspace 表示快照中的最小工作区信息。
type Workspace struct {
	// WorkspaceID 是工作区标识。
	WorkspaceID string `json:"workspace_id"`
	// Number 是工作区显示编号。
	Number int `json:"number"`
	// Label 是工作区显示标签。
	Label string `json:"label"`
}

// Tab 表示快照中的最小标签页信息。
type Tab struct {
	// TabID 是标签页标识。
	TabID string `json:"tab_id"`
	// WorkspaceID 是标签页所属工作区标识。
	WorkspaceID string `json:"workspace_id"`
	// Number 是标签页显示编号。
	Number int `json:"number"`
	// Label 是标签页显示标签。
	Label string `json:"label"`
}

// Pane 表示快照中的最小终端面板信息。
type Pane struct {
	// PaneID 是面板标识。
	PaneID string `json:"pane_id"`
	// TerminalID 是终端标识。
	TerminalID string `json:"terminal_id"`
	// WorkspaceID 是面板所在工作区标识。
	WorkspaceID string `json:"workspace_id"`
	// TabID 是面板所在标签页标识。
	TabID string `json:"tab_id"`
	// Agent 是 Herdr 检测到的 Agent 标识。
	Agent *string `json:"agent"`
	// Title 是面板展示标题。
	Title *string `json:"title"`
	// DisplayAgent 是面向用户展示的 Agent 名称。
	DisplayAgent *string `json:"display_agent"`
	// AgentStatus 是面板当前 Agent 生命周期状态。
	AgentStatus AgentStatus `json:"agent_status"`
	// AgentSession 是可选的 Agent 恢复会话标识。
	AgentSession *AgentSession `json:"agent_session"`
}

// Snapshot 表示 Herdr session.snapshot 返回的最小会话视图。
type Snapshot struct {
	// Version 是 Herdr 版本。
	Version string `json:"version"`
	// Protocol 是 Herdr Socket API 协议版本。
	Protocol uint32 `json:"protocol"`
	// Workspaces 是当前工作区列表。
	Workspaces []Workspace `json:"workspaces"`
	// Tabs 是当前标签页列表。
	Tabs []Tab `json:"tabs"`
	// Panes 是当前面板列表。
	Panes []Pane `json:"panes"`
	// Agents 是当前 Agent 列表。
	Agents []AgentInfo `json:"agents"`
}

// ReadResult 表示 recent_unwrapped 文本读取结果。
type ReadResult struct {
	// PaneID 是读取来源面板标识。
	PaneID string `json:"pane_id"`
	// WorkspaceID 是读取来源工作区标识。
	WorkspaceID string `json:"workspace_id"`
	// TabID 是读取来源标签页标识。
	TabID string `json:"tab_id"`
	// Text 是 Herdr 返回的终端文本快照。
	Text string `json:"text"`
	// Truncated 表示文本已被 Herdr 截断。
	Truncated bool `json:"truncated"`
}

type wireSnapshot struct {
	Version    *string            `json:"version"`
	Protocol   *uint32            `json:"protocol"`
	Workspaces []wireWorkspace    `json:"workspaces"`
	Tabs       []wireTab          `json:"tabs"`
	Panes      []wirePane         `json:"panes"`
	Layouts    *[]json.RawMessage `json:"layouts"`
	Agents     []wireAgentInfo    `json:"agents"`
}

type wireWorkspace struct {
	WorkspaceID *string `json:"workspace_id"`
	Number      *uint64 `json:"number"`
	Label       *string `json:"label"`
	Focused     *bool   `json:"focused"`
	PaneCount   *uint64 `json:"pane_count"`
	TabCount    *uint64 `json:"tab_count"`
	ActiveTabID *string `json:"active_tab_id"`
	AgentStatus *string `json:"agent_status"`
}

type wireTab struct {
	TabID       *string `json:"tab_id"`
	WorkspaceID *string `json:"workspace_id"`
	Number      *uint64 `json:"number"`
	Label       *string `json:"label"`
	Focused     *bool   `json:"focused"`
	PaneCount   *uint64 `json:"pane_count"`
	AgentStatus *string `json:"agent_status"`
}

type wirePane struct {
	PaneID       *string           `json:"pane_id"`
	TerminalID   *string           `json:"terminal_id"`
	WorkspaceID  *string           `json:"workspace_id"`
	TabID        *string           `json:"tab_id"`
	Focused      *bool             `json:"focused"`
	Agent        *string           `json:"agent"`
	Title        *string           `json:"title"`
	DisplayAgent *string           `json:"display_agent"`
	AgentStatus  *string           `json:"agent_status"`
	AgentSession *wireAgentSession `json:"agent_session"`
	Revision     *uint64           `json:"revision"`
}

type wireAgentInfo struct {
	TerminalID            *string           `json:"terminal_id"`
	Name                  *string           `json:"name"`
	Agent                 *string           `json:"agent"`
	Title                 *string           `json:"title"`
	TerminalTitle         *string           `json:"terminal_title"`
	TerminalTitleStripped *string           `json:"terminal_title_stripped"`
	DisplayAgent          *string           `json:"display_agent"`
	AgentStatus           *string           `json:"agent_status"`
	AgentSession          *wireAgentSession `json:"agent_session"`
	WorkspaceID           *string           `json:"workspace_id"`
	TabID                 *string           `json:"tab_id"`
	PaneID                *string           `json:"pane_id"`
	Focused               *bool             `json:"focused"`
	Revision              *uint64           `json:"revision"`
}

type wireAgentSession struct {
	Source *string `json:"source"`
	Agent  *string `json:"agent"`
	Kind   *string `json:"kind"`
	Value  *string `json:"value"`
}

type wireReadResult struct {
	PaneID      *string `json:"pane_id"`
	WorkspaceID *string `json:"workspace_id"`
	TabID       *string `json:"tab_id"`
	Source      *string `json:"source"`
	Format      *string `json:"format"`
	Text        *string `json:"text"`
	Revision    *uint64 `json:"revision"`
	Truncated   *bool   `json:"truncated"`
}

type decodedReadResult struct {
	Result ReadResult
	Source string
	Format string
}

func snapshotFromWire(wire wireSnapshot) (Snapshot, error) {
	if wire.Version == nil || strings.TrimSpace(*wire.Version) == "" || wire.Protocol == nil {
		return Snapshot{}, protocolError("session_snapshot 缺少必填字段")
	}
	if wire.Workspaces == nil || wire.Tabs == nil || wire.Panes == nil || wire.Layouts == nil || wire.Agents == nil {
		return Snapshot{}, protocolError("session_snapshot 缺少资源列表")
	}
	snapshot := Snapshot{Version: *wire.Version, Protocol: *wire.Protocol, Workspaces: make([]Workspace, 0, len(wire.Workspaces)), Tabs: make([]Tab, 0, len(wire.Tabs)), Panes: make([]Pane, 0, len(wire.Panes)), Agents: make([]AgentInfo, 0, len(wire.Agents))}
	for _, workspace := range wire.Workspaces {
		converted, err := workspaceFromWire(workspace)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Workspaces = append(snapshot.Workspaces, converted)
	}
	for _, tab := range wire.Tabs {
		converted, err := tabFromWire(tab)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Tabs = append(snapshot.Tabs, converted)
	}
	for _, pane := range wire.Panes {
		converted, err := paneFromWire(pane)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Panes = append(snapshot.Panes, converted)
	}
	for _, agent := range wire.Agents {
		converted, err := agentInfoFromWire(agent)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Agents = append(snapshot.Agents, converted)
	}
	return snapshot, nil
}

func workspaceFromWire(wire wireWorkspace) (Workspace, error) {
	if wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.Number == nil || wire.Label == nil || wire.Focused == nil || wire.PaneCount == nil || wire.TabCount == nil || wire.ActiveTabID == nil || strings.TrimSpace(*wire.ActiveTabID) == "" {
		return Workspace{}, protocolError("workspace 信息缺少必填字段")
	}
	if _, err := agentStatusFromWire(wire.AgentStatus); err != nil {
		return Workspace{}, err
	}
	number, err := uint64ToInt(*wire.Number)
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{WorkspaceID: *wire.WorkspaceID, Number: number, Label: *wire.Label}, nil
}

func tabFromWire(wire wireTab) (Tab, error) {
	if wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.Number == nil || wire.Label == nil || wire.Focused == nil || wire.PaneCount == nil {
		return Tab{}, protocolError("tab 信息缺少必填字段")
	}
	if _, err := agentStatusFromWire(wire.AgentStatus); err != nil {
		return Tab{}, err
	}
	number, err := uint64ToInt(*wire.Number)
	if err != nil {
		return Tab{}, err
	}
	return Tab{TabID: *wire.TabID, WorkspaceID: *wire.WorkspaceID, Number: number, Label: *wire.Label}, nil
}

func paneFromWire(wire wirePane) (Pane, error) {
	if wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.TerminalID == nil || strings.TrimSpace(*wire.TerminalID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.Focused == nil || wire.Revision == nil {
		return Pane{}, protocolError("pane 信息缺少必填字段")
	}
	status, err := agentStatusFromWire(wire.AgentStatus)
	if err != nil {
		return Pane{}, err
	}
	session, err := agentSessionFromWire(wire.AgentSession)
	if err != nil {
		return Pane{}, err
	}
	return Pane{PaneID: *wire.PaneID, TerminalID: *wire.TerminalID, WorkspaceID: *wire.WorkspaceID, TabID: *wire.TabID, Agent: wire.Agent, Title: wire.Title, DisplayAgent: wire.DisplayAgent, AgentStatus: status, AgentSession: session}, nil
}

func agentInfoFromWire(wire wireAgentInfo) (AgentInfo, error) {
	if wire.TerminalID == nil || strings.TrimSpace(*wire.TerminalID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.Focused == nil || wire.Revision == nil {
		return AgentInfo{}, protocolError("agent 信息缺少必填字段")
	}
	status, err := agentStatusFromWire(wire.AgentStatus)
	if err != nil {
		return AgentInfo{}, err
	}
	session, err := agentSessionFromWire(wire.AgentSession)
	if err != nil {
		return AgentInfo{}, err
	}
	return AgentInfo{TerminalID: *wire.TerminalID, Name: wire.Name, Agent: wire.Agent, Title: wire.Title, TerminalTitle: wire.TerminalTitle, TerminalTitleStripped: wire.TerminalTitleStripped, DisplayAgent: wire.DisplayAgent, AgentStatus: status, AgentSession: session, WorkspaceID: *wire.WorkspaceID, TabID: *wire.TabID, PaneID: *wire.PaneID}, nil
}

func agentStatusFromWire(status *string) (AgentStatus, error) {
	if status == nil || !isValidAgentStatus(*status) {
		return "", protocolError("agent 信息缺少有效状态")
	}
	return AgentStatus(*status), nil
}

func isValidAgentStatus(status string) bool {
	switch AgentStatus(status) {
	case AgentStatusIdle, AgentStatusWorking, AgentStatusBlocked, AgentStatusDone, AgentStatusUnknown:
		return true
	default:
		return false
	}
}

func agentSessionFromWire(wire *wireAgentSession) (*AgentSession, error) {
	if wire == nil {
		return nil, nil
	}
	if wire.Source == nil || strings.TrimSpace(*wire.Source) == "" || wire.Agent == nil || strings.TrimSpace(*wire.Agent) == "" || wire.Kind == nil || (*wire.Kind != "id" && *wire.Kind != "path") || wire.Value == nil || strings.TrimSpace(*wire.Value) == "" {
		return nil, protocolError("agent_session 缺少有效必填字段")
	}
	return &AgentSession{Source: *wire.Source, Agent: *wire.Agent, Kind: *wire.Kind, Value: *wire.Value}, nil
}

func readResultFromWire(wire wireReadResult) (decodedReadResult, error) {
	if wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.Source == nil || !isValidReadSource(*wire.Source) || wire.Format == nil || !isValidReadFormat(*wire.Format) || wire.Text == nil || wire.Revision == nil || wire.Truncated == nil {
		return decodedReadResult{}, protocolError("pane_read 缺少有效必填字段")
	}
	return decodedReadResult{Result: ReadResult{PaneID: *wire.PaneID, WorkspaceID: *wire.WorkspaceID, TabID: *wire.TabID, Text: *wire.Text, Truncated: *wire.Truncated}, Source: *wire.Source, Format: *wire.Format}, nil
}

func isValidReadSource(source string) bool {
	switch source {
	case "visible", "recent", "recent_unwrapped", "detection":
		return true
	default:
		return false
	}
}

func isValidReadFormat(format string) bool {
	return format == "text" || format == "ansi"
}

func uint64ToInt(value uint64) (int, error) {
	if value > uint64(^uint(0)>>1) {
		return 0, protocolError("数值超出本机 int 范围")
	}
	return int(value), nil
}
