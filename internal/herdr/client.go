package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

const defaultRequestTimeout = 10 * time.Second

// Dialer 定义 Client 建立本地 Socket 连接所需的拨号能力。
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Client 是使用单请求单连接策略的 Herdr 本地 Socket 客户端。
type Client struct {
	socketPath string
	dialer     Dialer
	timeout    time.Duration
	nextID     atomic.Uint64
}

// NewClient 创建使用 socketPath 的 Herdr 客户端。
func NewClient(socketPath string, dialer Dialer, timeout time.Duration) *Client {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	return &Client{socketPath: socketPath, dialer: dialer, timeout: timeout}
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	requestContext, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	request := requestEnvelope{
		ID:     fmt.Sprintf("pal:%d", c.nextID.Add(1)),
		Method: method,
		Params: params,
	}
	encodedRequest, err := encodeRequest(request)
	if err != nil {
		return err
	}

	conn, err := c.dialer.DialContext(requestContext, "unix", c.socketPath)
	if err != nil {
		return unavailableContextError(requestContext, err)
	}
	defer conn.Close()

	deadline, _ := requestContext.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return unavailableError(err)
	}
	stopClose := context.AfterFunc(requestContext, func() {
		_ = conn.Close()
	})
	defer stopClose()

	if err := writeFrame(conn, encodedRequest); err != nil {
		return unavailableContextError(requestContext, err)
	}
	line, err := readLine(bufio.NewReader(conn))
	if err != nil {
		if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrProtocol) && !errors.Is(err, io.EOF) {
			return err
		}
		return unavailableContextError(requestContext, err)
	}
	return parseResponse(line, request.ID, result)
}

func unavailableError(err error) error {
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func unavailableContextError(ctx context.Context, err error) error {
	if contextError := ctx.Err(); contextError != nil {
		err = errors.Join(err, contextError)
	} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		err = errors.Join(err, context.DeadlineExceeded)
	}
	return unavailableError(err)
}

// CheckCompatible 检查已连接 Herdr 的协议版本是否与客户端精确兼容。
func (c *Client) CheckCompatible(ctx context.Context) error {
	var result pongResult
	if err := c.call(ctx, "ping", map[string]any{}, &result); err != nil {
		return err
	}
	if err := validateResultType(result.Type, "pong"); err != nil {
		return err
	}
	if result.Version == nil || strings.TrimSpace(*result.Version) == "" {
		return protocolError("pong 响应缺少 version")
	}
	if result.Protocol == nil {
		return protocolError("pong 响应缺少 protocol")
	}
	if *result.Protocol != RequiredProtocol {
		return fmt.Errorf("%w: expected %d, got %d", ErrProtocolMismatch, RequiredProtocol, *result.Protocol)
	}
	return nil
}

// Snapshot 读取当前 Herdr 会话的最小快照。
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	var result snapshotResult
	if err := c.call(ctx, "session.snapshot", map[string]any{}, &result); err != nil {
		return Snapshot{}, err
	}
	if err := validateResultType(result.Type, "session_snapshot"); err != nil {
		return Snapshot{}, err
	}
	var wire wireSnapshot
	if err := decodeRequiredPayload(result.Snapshot, "snapshot", &wire); err != nil {
		return Snapshot{}, err
	}
	return snapshotFromWire(wire)
}

// GetAgent 查询 target 指向的 Agent 信息。
func (c *Client) GetAgent(ctx context.Context, target string) (AgentInfo, error) {
	if err := validateTarget(target); err != nil {
		return AgentInfo{}, err
	}
	var result agentResult
	if err := c.call(ctx, "agent.get", agentTargetParams{Target: target}, &result); err != nil {
		return AgentInfo{}, err
	}
	if err := validateResultType(result.Type, "agent_info"); err != nil {
		return AgentInfo{}, err
	}
	var wire wireAgentInfo
	if err := decodeRequiredPayload(result.Agent, "agent", &wire); err != nil {
		return AgentInfo{}, err
	}
	return agentInfoFromWire(wire)
}

// ReadRecent 读取 target 的 recent_unwrapped 纯文本终端快照。
func (c *Client) ReadRecent(ctx context.Context, target string, lines int) (ReadResult, error) {
	if err := validateTarget(target); err != nil {
		return ReadResult{}, err
	}
	if lines < 1 || lines > 1000 {
		return ReadResult{}, protocolError("recent_unwrapped 行数必须在 1 到 1000 之间")
	}
	var result paneReadResult
	params := agentReadParams{Target: target, Source: "recent_unwrapped", Lines: lines, Format: "text", StripANSI: true}
	if err := c.call(ctx, "agent.read", params, &result); err != nil {
		return ReadResult{}, err
	}
	if err := validateResultType(result.Type, "pane_read"); err != nil {
		return ReadResult{}, err
	}
	var wire wireReadResult
	if err := decodeRequiredPayload(result.Read, "read", &wire); err != nil {
		return ReadResult{}, err
	}
	return readResultFromWire(wire)
}

// Prompt 向 target 对应的 Agent 发送普通文本输入。
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	var result agentResult
	if err := c.call(ctx, "agent.prompt", agentPromptParams{Target: target, Text: text}, &result); err != nil {
		return err
	}
	if err := validateResultType(result.Type, "agent_prompted"); err != nil {
		return err
	}
	var wire wireAgentInfo
	if err := decodeRequiredPayload(result.Agent, "agent", &wire); err != nil {
		return err
	}
	_, err := agentInfoFromWire(wire)
	return err
}

// SendKey 向 target 对应的 Agent 发送一个显式 UI 控制按键。
func (c *Client) SendKey(ctx context.Context, target, key string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	if strings.TrimSpace(key) == "" {
		return protocolError("发送 Herdr 按键失败：key 不能为空")
	}
	var result typedResult
	if err := c.call(ctx, "agent.send_keys", agentSendKeysParams{Target: target, Keys: []string{key}}, &result); err != nil {
		return err
	}
	return validateResultType(result.Type, "ok")
}

type typedResult struct {
	Type string `json:"type"`
}

type pongResult struct {
	Type     string  `json:"type"`
	Version  *string `json:"version"`
	Protocol *uint32 `json:"protocol"`
}

type snapshotResult struct {
	Type     string          `json:"type"`
	Snapshot json.RawMessage `json:"snapshot"`
}

type agentResult struct {
	Type  string          `json:"type"`
	Agent json.RawMessage `json:"agent"`
}

type paneReadResult struct {
	Type string          `json:"type"`
	Read json.RawMessage `json:"read"`
}

type wireSnapshot struct {
	Version    *string         `json:"version"`
	Protocol   *uint32         `json:"protocol"`
	Workspaces []wireWorkspace `json:"workspaces"`
	Tabs       []wireTab       `json:"tabs"`
	Panes      []wirePane      `json:"panes"`
	Agents     []wireAgentInfo `json:"agents"`
}

type wireWorkspace struct {
	WorkspaceID *string `json:"workspace_id"`
	Number      *int    `json:"number"`
	Label       *string `json:"label"`
}

type wireTab struct {
	TabID       *string `json:"tab_id"`
	WorkspaceID *string `json:"workspace_id"`
	Number      *int    `json:"number"`
	Label       *string `json:"label"`
}

type wirePane struct {
	PaneID       *string           `json:"pane_id"`
	TerminalID   *string           `json:"terminal_id"`
	WorkspaceID  *string           `json:"workspace_id"`
	TabID        *string           `json:"tab_id"`
	Agent        *string           `json:"agent"`
	Title        *string           `json:"title"`
	DisplayAgent *string           `json:"display_agent"`
	AgentStatus  *string           `json:"agent_status"`
	AgentSession *wireAgentSession `json:"agent_session"`
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
	Text        *string `json:"text"`
	Truncated   *bool   `json:"truncated"`
}

type agentTargetParams struct {
	Target string `json:"target"`
}

type agentReadParams struct {
	Target    string `json:"target"`
	Source    string `json:"source"`
	Lines     int    `json:"lines"`
	Format    string `json:"format"`
	StripANSI bool   `json:"strip_ansi"`
}

type agentPromptParams struct {
	Target string `json:"target"`
	Text   string `json:"text"`
}

type agentSendKeysParams struct {
	Target string   `json:"target"`
	Keys   []string `json:"keys"`
}

func validateResultType(actual, expected string) error {
	if strings.TrimSpace(actual) == "" {
		return protocolError("Herdr result 缺少 type")
	}
	if actual != expected {
		return protocolError(fmt.Sprintf("Herdr result type 为 %q，期望 %q", actual, expected))
	}
	return nil
}

func decodeRequiredPayload(raw json.RawMessage, name string, destination any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return protocolError(fmt.Sprintf("Herdr result 缺少 %s", name))
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("%w: 解码 Herdr result.%s: %w", ErrProtocol, name, err)
	}
	return nil
}

func validateTarget(target string) error {
	if strings.TrimSpace(target) == "" {
		return protocolError("Herdr target 不能为空")
	}
	return nil
}

func snapshotFromWire(wire wireSnapshot) (Snapshot, error) {
	if wire.Version == nil || strings.TrimSpace(*wire.Version) == "" {
		return Snapshot{}, protocolError("session_snapshot 缺少 version")
	}
	if wire.Protocol == nil {
		return Snapshot{}, protocolError("session_snapshot 缺少 protocol")
	}
	if wire.Workspaces == nil || wire.Tabs == nil || wire.Panes == nil || wire.Agents == nil {
		return Snapshot{}, protocolError("session_snapshot 缺少资源列表")
	}
	snapshot := Snapshot{
		Version:    *wire.Version,
		Protocol:   *wire.Protocol,
		Workspaces: make([]Workspace, 0, len(wire.Workspaces)),
		Tabs:       make([]Tab, 0, len(wire.Tabs)),
		Panes:      make([]Pane, 0, len(wire.Panes)),
		Agents:     make([]AgentInfo, 0, len(wire.Agents)),
	}
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
	if wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.Number == nil || wire.Label == nil {
		return Workspace{}, protocolError("workspace 信息缺少必填字段")
	}
	return Workspace{WorkspaceID: *wire.WorkspaceID, Number: *wire.Number, Label: *wire.Label}, nil
}

func tabFromWire(wire wireTab) (Tab, error) {
	if wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.Number == nil || wire.Label == nil {
		return Tab{}, protocolError("tab 信息缺少必填字段")
	}
	return Tab{TabID: *wire.TabID, WorkspaceID: *wire.WorkspaceID, Number: *wire.Number, Label: *wire.Label}, nil
}

func paneFromWire(wire wirePane) (Pane, error) {
	if wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.TerminalID == nil || strings.TrimSpace(*wire.TerminalID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" {
		return Pane{}, protocolError("pane 信息缺少标识")
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
	if wire.TerminalID == nil || strings.TrimSpace(*wire.TerminalID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" {
		return AgentInfo{}, protocolError("agent 信息缺少标识")
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
	if wire.Source == nil || strings.TrimSpace(*wire.Source) == "" || wire.Agent == nil || strings.TrimSpace(*wire.Agent) == "" || wire.Kind == nil || strings.TrimSpace(*wire.Kind) == "" || wire.Value == nil || strings.TrimSpace(*wire.Value) == "" {
		return nil, protocolError("agent_session 缺少必填字段")
	}
	return &AgentSession{Source: *wire.Source, Agent: *wire.Agent, Kind: *wire.Kind, Value: *wire.Value}, nil
}

func readResultFromWire(wire wireReadResult) (ReadResult, error) {
	if wire.PaneID == nil || strings.TrimSpace(*wire.PaneID) == "" || wire.WorkspaceID == nil || strings.TrimSpace(*wire.WorkspaceID) == "" || wire.TabID == nil || strings.TrimSpace(*wire.TabID) == "" || wire.Text == nil || wire.Truncated == nil {
		return ReadResult{}, protocolError("pane_read 缺少必填字段")
	}
	return ReadResult{PaneID: *wire.PaneID, WorkspaceID: *wire.WorkspaceID, TabID: *wire.TabID, Text: *wire.Text, Truncated: *wire.Truncated}, nil
}

func protocolError(message string) error {
	return fmt.Errorf("%w: %s", ErrProtocol, message)
}
