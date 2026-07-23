package herdr

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
