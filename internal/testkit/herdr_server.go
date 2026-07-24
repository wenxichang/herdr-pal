// Package testkit 提供只实现公开协议的本地集成测试服务。
package testkit

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
)

const testWaitTimeout = 5 * time.Second

// HerdrCall 记录 fake Herdr 收到的一次公开 API 调用。
type HerdrCall struct {
	Method string
	Params json.RawMessage
}

// HerdrServer 是只实现 protocol 17 必要公开方法的 Unix NDJSON fake。
type HerdrServer struct {
	listener net.Listener
	path     string

	mu               sync.Mutex
	snapshot         herdr.Snapshot
	output           []string
	calls            []HerdrCall
	subscriptions    map[*herdrSubscription]struct{}
	subscribeCount   map[string]int
	connections      map[net.Conn]struct{}
	promptWaitStalls int
	enterTransition  *herdr.AgentStatus
	closed           bool
	available        bool
	changed          chan struct{}
}

type herdrSubscription struct {
	conn  net.Conn
	specs []herdr.SubscriptionSpec
	mu    sync.Mutex
}

type herdrRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// NewHerdrServer 启动一个测试生命周期内可用的 protocol 17 Unix Socket fake。
func NewHerdrServer(t testing.TB, snapshot herdr.Snapshot) *HerdrServer {
	t.Helper()
	if snapshot.Protocol != herdr.RequiredProtocol {
		t.Fatalf("fake Herdr snapshot protocol = %d, want %d", snapshot.Protocol, herdr.RequiredProtocol)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "herdr.sock")
	if len(path) >= 96 {
		digest := sha256.Sum256([]byte(directory))
		alias := filepath.Join("/tmp", fmt.Sprintf("herdr-pal-%x", digest[:8]))
		if err := os.Symlink(directory, alias); err != nil {
			t.Fatalf("创建 fake Herdr 短路径：%v", err)
		}
		t.Cleanup(func() { _ = os.Remove(alias) })
		path = filepath.Join(alias, "herdr.sock")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("启动 fake Herdr：%v", err)
	}
	server := &HerdrServer{
		listener: listener, path: path, snapshot: cloneSnapshot(snapshot),
		subscriptions: make(map[*herdrSubscription]struct{}), connections: make(map[net.Conn]struct{}),
		subscribeCount: make(map[string]int), available: true, changed: make(chan struct{}, 1),
	}
	t.Cleanup(func() { _ = server.Close() })
	go server.acceptLoop()
	return server
}

// WaitSubscriptionCount 等待与 specs 等价的成功订阅累计达到 count 代。
func (s *HerdrServer) WaitSubscriptionCount(t testing.TB, specs []herdr.SubscriptionSpec, count int) {
	t.Helper()
	key := subscriptionKey(specs)
	s.wait(t, fmt.Sprintf("Herdr 订阅 %v 累计达到 %d 代", canonicalSpecs(specs), count), func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.subscribeCount[key] >= count
	})
}

// SetAvailable 控制新请求是否能获得响应；关闭时连接会在记录调用后直接断开。
func (s *HerdrServer) SetAvailable(available bool) {
	s.mu.Lock()
	s.available = available
	s.mu.Unlock()
	s.signal()
}

// SocketPath 返回 fake Herdr 的 Unix Socket 路径。
func (s *HerdrServer) SocketPath() string { return s.path }

// SetSnapshot 原子替换后续 session.snapshot 与 agent.get 使用的状态。
func (s *HerdrServer) SetSnapshot(snapshot herdr.Snapshot) {
	if snapshot.Protocol != herdr.RequiredProtocol {
		panic("fake Herdr 仅支持 protocol 17")
	}
	s.mu.Lock()
	s.snapshot = cloneSnapshot(snapshot)
	s.mu.Unlock()
	s.signal()
}

// SetOutput 设置 agent.read 按请求行数从尾部截取的终端行。
func (s *HerdrServer) SetOutput(lines []string) {
	s.mu.Lock()
	s.output = append([]string(nil), lines...)
	s.mu.Unlock()
	s.signal()
}

// SetPromptWaitStalls 设置后续带 wait 的 agent.prompt 返回 stalled 的次数。
func (s *HerdrServer) SetPromptWaitStalls(count int) {
	if count < 0 {
		panic("prompt stalled 次数不能为负数")
	}
	s.mu.Lock()
	s.promptWaitStalls = count
	s.mu.Unlock()
	s.signal()
}

// SetEnterTransition 设置收到 enter 后 Agent 应迁移到的状态。
func (s *HerdrServer) SetEnterTransition(status herdr.AgentStatus) {
	s.mu.Lock()
	s.enterTransition = &status
	s.mu.Unlock()
	s.signal()
}

// Calls 返回指定方法的调用记录副本；method 为空时返回全部调用。
func (s *HerdrServer) Calls(method string) []HerdrCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]HerdrCall, 0, len(s.calls))
	for _, call := range s.calls {
		if method == "" || call.Method == method {
			call.Params = append(json.RawMessage(nil), call.Params...)
			result = append(result, call)
		}
	}
	return result
}

// WaitCallCount 等待指定方法至少收到 count 次调用。
func (s *HerdrServer) WaitCallCount(t testing.TB, method string, count int) []HerdrCall {
	t.Helper()
	var calls []HerdrCall
	s.wait(t, fmt.Sprintf("Herdr %s 调用达到 %d 次", method, count), func() bool {
		calls = s.Calls(method)
		return len(calls) >= count
	})
	return calls
}

// WaitSubscription 等待出现一条与 specs 等价的公开事件订阅。
func (s *HerdrServer) WaitSubscription(t testing.TB, specs []herdr.SubscriptionSpec) {
	t.Helper()
	want := canonicalSpecs(specs)
	s.wait(t, fmt.Sprintf("Herdr 订阅 %v", want), func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for subscription := range s.subscriptions {
			if reflect.DeepEqual(canonicalSpecs(subscription.specs), want) {
				return true
			}
		}
		return false
	})
}

// EmitStatus 向匹配 pane_id 的专用状态订阅注入事件，并返回成功写入的订阅数。
func (s *HerdrServer) EmitStatus(event herdr.AgentStatusEvent) int {
	data := map[string]any{
		"pane_id": event.PaneID, "workspace_id": event.WorkspaceID,
		"agent_status": event.AgentStatus, "state_labels": event.StateLabels,
	}
	if event.StateLabels == nil {
		data["state_labels"] = map[string]string{}
	}
	if event.Agent != nil {
		data["agent"] = *event.Agent
	}
	if event.Title != nil {
		data["title"] = *event.Title
	}
	if event.DisplayAgent != nil {
		data["display_agent"] = *event.DisplayAgent
	}
	return s.emit("pane.agent_status_changed", event.PaneID, data)
}

// EmitLifecycle 向匹配类型的通用生命周期订阅注入事件，并返回成功写入的订阅数。
func (s *HerdrServer) EmitLifecycle(kind, paneID string) int {
	return s.emit(kind, paneID, map[string]any{"pane_id": paneID})
}

// DisconnectSubscriptions 主动关闭当前全部订阅连接，模拟 Herdr 事件流断线。
func (s *HerdrServer) DisconnectSubscriptions() {
	s.mu.Lock()
	subscriptions := make([]*herdrSubscription, 0, len(s.subscriptions))
	for subscription := range s.subscriptions {
		subscriptions = append(subscriptions, subscription)
	}
	s.mu.Unlock()
	for _, subscription := range subscriptions {
		_ = subscription.conn.Close()
	}
}

// Close 停止 fake Herdr 并关闭全部连接。
func (s *HerdrServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	connections := make([]net.Conn, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	for subscription := range s.subscriptions {
		connections = append(connections, subscription.conn)
	}
	s.mu.Unlock()
	err := s.listener.Close()
	for _, connection := range connections {
		_ = connection.Close()
	}
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (s *HerdrServer) acceptLoop() {
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = connection.Close()
			return
		}
		s.connections[connection] = struct{}{}
		s.mu.Unlock()
		go s.handleConnection(connection)
	}
}

func (s *HerdrServer) handleConnection(connection net.Conn) {
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = connection.Close()
			s.mu.Lock()
			delete(s.connections, connection)
			s.mu.Unlock()
		}
	}()
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		return
	}
	var request herdrRequest
	if err := json.Unmarshal(line, &request); err != nil || request.ID == "" || request.Method == "" {
		return
	}
	s.record(HerdrCall{Method: request.Method, Params: append(json.RawMessage(nil), request.Params...)})
	s.mu.Lock()
	available := s.available
	s.mu.Unlock()
	if !available {
		return
	}
	if request.Method == "events.subscribe" {
		var params struct {
			Subscriptions []herdr.SubscriptionSpec `json:"subscriptions"`
		}
		if json.Unmarshal(request.Params, &params) != nil || validateFakeSubscriptionSpecs(params.Subscriptions) != nil {
			s.writeError(connection, request.ID, "invalid_params", "subscriptions required")
			return
		}
		if err := writeHerdrJSON(connection, map[string]any{"id": request.ID, "result": map[string]any{"type": "subscription_started"}}); err != nil {
			return
		}
		subscription := &herdrSubscription{conn: connection, specs: append([]herdr.SubscriptionSpec(nil), params.Subscriptions...)}
		s.mu.Lock()
		s.subscriptions[subscription] = struct{}{}
		s.subscribeCount[subscriptionKey(params.Subscriptions)]++
		s.mu.Unlock()
		s.signal()
		keepOpen = true
		go s.watchSubscription(subscription)
		return
	}
	s.handleRequest(connection, request)
}

func (s *HerdrServer) watchSubscription(subscription *herdrSubscription) {
	buffer := make([]byte, 1)
	for {
		if _, err := subscription.conn.Read(buffer); err != nil {
			break
		}
	}
	s.mu.Lock()
	delete(s.subscriptions, subscription)
	delete(s.connections, subscription.conn)
	s.mu.Unlock()
	_ = subscription.conn.Close()
	s.signal()
}

func (s *HerdrServer) handleRequest(connection net.Conn, request herdrRequest) {
	s.mu.Lock()
	snapshot := cloneSnapshot(s.snapshot)
	output := append([]string(nil), s.output...)
	s.mu.Unlock()

	var result any
	switch request.Method {
	case "ping":
		result = map[string]any{"type": "pong", "version": snapshot.Version, "protocol": snapshot.Protocol}
	case "session.snapshot":
		result = map[string]any{"type": "session_snapshot", "snapshot": snapshotWire(snapshot)}
	case "agent.get":
		target, ok := requiredTarget(request.Params)
		if !ok {
			s.writeError(connection, request.ID, "invalid_params", "invalid target")
			return
		}
		agent, ok := findAgent(snapshot, target)
		if !ok {
			s.writeError(connection, request.ID, "agent_not_found", "agent not found")
			return
		}
		result = map[string]any{"type": "agent_info", "agent": agentWire(agent)}
	case "agent.read":
		var params struct {
			Target    string `json:"target"`
			Source    string `json:"source"`
			Lines     int    `json:"lines"`
			Format    string `json:"format"`
			StripANSI *bool  `json:"strip_ansi"`
		}
		if json.Unmarshal(request.Params, &params) != nil || strings.TrimSpace(params.Target) == "" ||
			params.Source != "recent_unwrapped" || params.Lines < 1 || params.Lines > 1000 ||
			params.Format != "text" || params.StripANSI == nil || !*params.StripANSI {
			s.writeError(connection, request.ID, "invalid_params", "invalid read")
			return
		}
		agent, ok := findAgent(snapshot, params.Target)
		if !ok {
			s.writeError(connection, request.ID, "agent_not_found", "agent not found")
			return
		}
		if len(output) > params.Lines {
			output = output[len(output)-params.Lines:]
		}
		result = map[string]any{"type": "pane_read", "read": map[string]any{
			"pane_id": agent.PaneID, "workspace_id": agent.WorkspaceID, "tab_id": agent.TabID,
			"source": "recent_unwrapped", "format": "text", "text": strings.Join(output, "\n"),
			"revision": 0, "truncated": false,
		}}
	case "agent.prompt":
		var params struct {
			Target string  `json:"target"`
			Text   *string `json:"text"`
			Wait   *struct {
				Until []herdr.AgentStatus `json:"until"`
			} `json:"wait"`
		}
		if json.Unmarshal(request.Params, &params) != nil || strings.TrimSpace(params.Target) == "" || params.Text == nil {
			s.writeError(connection, request.ID, "invalid_params", "invalid prompt")
			return
		}
		agent, ok := findAgent(snapshot, params.Target)
		if !ok {
			s.writeError(connection, request.ID, "agent_not_found", "agent not found")
			return
		}
		if params.Wait == nil {
			result = map[string]any{"type": "agent_prompted", "agent": agentWire(agent)}
			break
		}
		wantStatuses := []herdr.AgentStatus{
			herdr.AgentStatusIdle,
			herdr.AgentStatusWorking,
			herdr.AgentStatusBlocked,
			herdr.AgentStatusDone,
			herdr.AgentStatusUnknown,
		}
		if !reflect.DeepEqual(params.Wait.Until, wantStatuses) {
			s.writeError(connection, request.ID, "invalid_params", "invalid prompt wait")
			return
		}
		s.mu.Lock()
		if s.promptWaitStalls > 0 {
			s.promptWaitStalls--
			s.mu.Unlock()
			s.writeError(connection, request.ID, "agent_prompt_stalled", "agent status did not change")
			return
		}
		agent, _ = s.transitionAgentLocked(agent.PaneID, herdr.AgentStatusWorking)
		s.mu.Unlock()
		s.signal()
		result = map[string]any{"type": "agent_info", "agent": agentWire(agent)}
	case "agent.send_keys":
		var params struct {
			Target string   `json:"target"`
			Keys   []string `json:"keys"`
		}
		if json.Unmarshal(request.Params, &params) != nil || strings.TrimSpace(params.Target) == "" ||
			len(params.Keys) != 1 || strings.TrimSpace(params.Keys[0]) == "" {
			s.writeError(connection, request.ID, "invalid_params", "invalid keys")
			return
		}
		agent, ok := findAgent(snapshot, params.Target)
		if !ok {
			s.writeError(connection, request.ID, "agent_not_found", "agent not found")
			return
		}
		if params.Keys[0] == "enter" {
			s.mu.Lock()
			if s.enterTransition != nil {
				s.transitionAgentLocked(agent.PaneID, *s.enterTransition)
			}
			s.mu.Unlock()
			s.signal()
		}
		result = map[string]any{"type": "ok"}
	default:
		s.writeError(connection, request.ID, "method_not_found", "unsupported method")
		return
	}
	_ = writeHerdrJSON(connection, map[string]any{"id": request.ID, "result": result})
}

func (s *HerdrServer) transitionAgentLocked(paneID string, status herdr.AgentStatus) (herdr.AgentInfo, bool) {
	for index := range s.snapshot.Agents {
		if s.snapshot.Agents[index].PaneID != paneID {
			continue
		}
		s.snapshot.Agents[index].AgentStatus = status
		s.snapshot.Agents[index].StateChangeSeq++
		agent := s.snapshot.Agents[index]
		for paneIndex := range s.snapshot.Panes {
			if s.snapshot.Panes[paneIndex].PaneID == paneID {
				s.snapshot.Panes[paneIndex].AgentStatus = status
				break
			}
		}
		return agent, true
	}
	return herdr.AgentInfo{}, false
}

func (s *HerdrServer) writeError(connection net.Conn, requestID, code, message string) {
	_ = writeHerdrJSON(connection, map[string]any{"id": requestID, "error": map[string]any{"code": code, "message": message}})
}

func (s *HerdrServer) emit(kind, paneID string, data map[string]any) int {
	frame, err := json.Marshal(map[string]any{"event": kind, "data": data})
	if err != nil {
		panic(err)
	}
	frame = append(frame, '\n')
	s.mu.Lock()
	subscriptions := make([]*herdrSubscription, 0, len(s.subscriptions))
	for subscription := range s.subscriptions {
		if subscriptionMatches(subscription.specs, kind, paneID) {
			subscriptions = append(subscriptions, subscription)
		}
	}
	s.mu.Unlock()
	deliveries := 0
	for _, subscription := range subscriptions {
		subscription.mu.Lock()
		_, err := subscription.conn.Write(frame)
		subscription.mu.Unlock()
		if err == nil {
			deliveries++
		}
	}
	return deliveries
}

func (s *HerdrServer) record(call HerdrCall) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
	s.signal()
}

func (s *HerdrServer) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *HerdrServer) wait(t testing.TB, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(testWaitTimeout)
	defer deadline.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-s.changed:
		case <-deadline.C:
			t.Fatalf("等待超时：%s", description)
		}
	}
}

func writeHerdrJSON(connection net.Conn, value any) error {
	return json.NewEncoder(connection).Encode(value)
}

func subscriptionMatches(specs []herdr.SubscriptionSpec, kind, paneID string) bool {
	for _, spec := range specs {
		if spec.Type == kind && (kind != "pane.agent_status_changed" || spec.PaneID == paneID) {
			return true
		}
	}
	return false
}

func canonicalSpecs(specs []herdr.SubscriptionSpec) []herdr.SubscriptionSpec {
	result := append([]herdr.SubscriptionSpec(nil), specs...)
	sort.Slice(result, func(left, right int) bool {
		if result[left].Type == result[right].Type {
			return result[left].PaneID < result[right].PaneID
		}
		return result[left].Type < result[right].Type
	})
	return result
}

func subscriptionKey(specs []herdr.SubscriptionSpec) string {
	encoded, err := json.Marshal(canonicalSpecs(specs))
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func validateFakeSubscriptionSpecs(specs []herdr.SubscriptionSpec) error {
	if len(specs) == 0 {
		return errors.New("empty subscriptions")
	}
	allowed := map[string]bool{
		"pane.created": true, "pane.closed": true, "pane.updated": true,
		"pane.exited": true, "pane.agent_detected": true, "pane.agent_status_changed": true,
	}
	for _, spec := range specs {
		if !allowed[spec.Type] {
			return errors.New("unsupported subscription")
		}
		if spec.Type == "pane.agent_status_changed" && strings.TrimSpace(spec.PaneID) == "" {
			return errors.New("status pane required")
		}
		if spec.Type != "pane.agent_status_changed" && spec.PaneID != "" {
			return errors.New("lifecycle pane forbidden")
		}
	}
	return nil
}

func requiredTarget(raw json.RawMessage) (string, bool) {
	var params struct {
		Target string `json:"target"`
	}
	if json.Unmarshal(raw, &params) != nil || strings.TrimSpace(params.Target) == "" {
		return "", false
	}
	return params.Target, true
}

func findAgent(snapshot herdr.Snapshot, target string) (herdr.AgentInfo, bool) {
	for _, agent := range snapshot.Agents {
		if target == agent.PaneID {
			return agent, true
		}
	}
	var match herdr.AgentInfo
	matched := false
	for _, agent := range snapshot.Agents {
		if agent.Name == nil || strings.TrimSpace(*agent.Name) == "" || target != *agent.Name {
			continue
		}
		if matched {
			return herdr.AgentInfo{}, false
		}
		match = agent
		matched = true
	}
	return match, matched
}

func snapshotWire(snapshot herdr.Snapshot) map[string]any {
	workspaces := make([]any, 0, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		activeTab := "tab-unavailable"
		paneCount, tabCount := 0, 0
		for _, tab := range snapshot.Tabs {
			if tab.WorkspaceID == workspace.WorkspaceID {
				if tabCount == 0 {
					activeTab = tab.TabID
				}
				tabCount++
			}
		}
		for _, pane := range snapshot.Panes {
			if pane.WorkspaceID == workspace.WorkspaceID {
				paneCount++
			}
		}
		workspaces = append(workspaces, map[string]any{
			"workspace_id": workspace.WorkspaceID, "number": workspace.Number, "label": workspace.Label,
			"focused": false, "pane_count": paneCount, "tab_count": tabCount,
			"active_tab_id": activeTab, "agent_status": string(herdr.AgentStatusUnknown),
		})
	}
	tabs := make([]any, 0, len(snapshot.Tabs))
	for _, tab := range snapshot.Tabs {
		paneCount := 0
		for _, pane := range snapshot.Panes {
			if pane.TabID == tab.TabID {
				paneCount++
			}
		}
		tabs = append(tabs, map[string]any{
			"tab_id": tab.TabID, "workspace_id": tab.WorkspaceID, "number": tab.Number, "label": tab.Label,
			"focused": false, "pane_count": paneCount, "agent_status": string(herdr.AgentStatusUnknown),
		})
	}
	panes := make([]any, 0, len(snapshot.Panes))
	for _, pane := range snapshot.Panes {
		panes = append(panes, paneWire(pane))
	}
	agents := make([]any, 0, len(snapshot.Agents))
	for _, agent := range snapshot.Agents {
		agents = append(agents, agentWire(agent))
	}
	return map[string]any{
		"version": snapshot.Version, "protocol": snapshot.Protocol,
		"workspaces": workspaces, "tabs": tabs, "panes": panes, "layouts": []any{}, "agents": agents,
	}
}

func paneWire(pane herdr.Pane) map[string]any {
	return map[string]any{
		"pane_id": pane.PaneID, "terminal_id": pane.TerminalID, "workspace_id": pane.WorkspaceID,
		"tab_id": pane.TabID, "focused": false, "agent": pane.Agent, "title": pane.Title,
		"display_agent": pane.DisplayAgent, "agent_status": pane.AgentStatus,
		"agent_session": pane.AgentSession, "revision": 0,
	}
}

func agentWire(agent herdr.AgentInfo) map[string]any {
	return map[string]any{
		"terminal_id": agent.TerminalID, "name": agent.Name, "agent": agent.Agent, "title": agent.Title,
		"terminal_title": agent.TerminalTitle, "terminal_title_stripped": agent.TerminalTitleStripped,
		"display_agent": agent.DisplayAgent, "agent_status": agent.AgentStatus, "agent_session": agent.AgentSession,
		"workspace_id": agent.WorkspaceID, "tab_id": agent.TabID, "pane_id": agent.PaneID,
		"focused": false, "revision": 0, "state_change_seq": agent.StateChangeSeq,
	}
}

func cloneSnapshot(snapshot herdr.Snapshot) herdr.Snapshot {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		panic(err)
	}
	var clone herdr.Snapshot
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}
