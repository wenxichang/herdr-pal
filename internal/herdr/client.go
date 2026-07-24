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

const agentStatePollInterval = 100 * time.Millisecond

var allAgentStatuses = []AgentStatus{
	AgentStatusIdle,
	AgentStatusWorking,
	AgentStatusBlocked,
	AgentStatusDone,
	AgentStatusUnknown,
}

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
	decoded, err := readResultFromWire(wire)
	if err != nil {
		return ReadResult{}, err
	}
	if decoded.Source != "recent_unwrapped" || decoded.Format != "text" {
		return ReadResult{}, protocolError("pane_read 返回来源或格式与请求不一致")
	}
	return decoded.Result, nil
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

// PromptUntilStateChange 向 target 发送普通文本，并等待首次可观察的 Agent 状态变化。
func (c *Client) PromptUntilStateChange(ctx context.Context, target, text string) (AgentInfo, error) {
	if err := validateTarget(target); err != nil {
		return AgentInfo{}, err
	}
	params := agentPromptParams{
		Target: target,
		Text:   text,
		Wait:   &agentPromptWaitOptions{Until: append([]AgentStatus(nil), allAgentStatuses...)},
	}
	var result agentResult
	if err := c.call(ctx, "agent.prompt", params, &result); err != nil {
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

// WaitForStateChange 轮询 target，直到 state_change_seq 不再等于 baseline。
func (c *Client) WaitForStateChange(ctx context.Context, target string, baseline uint64, timeout time.Duration) (AgentInfo, error) {
	if err := validateTarget(target); err != nil {
		return AgentInfo{}, err
	}
	if timeout <= 0 {
		return AgentInfo{}, protocolError("等待 Agent 状态变化失败：timeout 必须大于零")
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		agent, err := c.GetAgent(waitContext, target)
		if err != nil {
			if contextError := ctx.Err(); contextError != nil {
				return AgentInfo{}, contextError
			}
			if waitContext.Err() != nil {
				return AgentInfo{}, ErrAgentStateChangeTimeout
			}
			return AgentInfo{}, err
		}
		if agent.StateChangeSeq != baseline {
			return agent, nil
		}

		timer := time.NewTimer(agentStatePollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return AgentInfo{}, ctx.Err()
		case <-waitContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if contextError := ctx.Err(); contextError != nil {
				return AgentInfo{}, contextError
			}
			return AgentInfo{}, ErrAgentStateChangeTimeout
		case <-timer.C:
		}
	}
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
	Target string                  `json:"target"`
	Text   string                  `json:"text"`
	Wait   *agentPromptWaitOptions `json:"wait,omitempty"`
}

type agentPromptWaitOptions struct {
	Until []AgentStatus `json:"until"`
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

func protocolError(message string) error {
	return fmt.Errorf("%w: %s", ErrProtocol, message)
}
