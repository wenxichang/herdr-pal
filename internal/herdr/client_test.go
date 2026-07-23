package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientRequestUsesIndependentConnectionsAndClosesThem(t *testing.T) {
	dialer := &pipeDialer{}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	for index := 1; index <= 2; index++ {
		var result struct {
			Value int `json:"value"`
		}
		if err := client.call(context.Background(), "test.method", map[string]int{"index": index}, &result); err != nil {
			t.Fatalf("第 %d 次 call() 返回错误：%v", index, err)
		}
		if result.Value != index {
			t.Fatalf("第 %d 次结果 = %+v", index, result)
		}
	}

	if got := dialer.Count(); got != 2 {
		t.Fatalf("DialContext() 调用次数 = %d，期望 2", got)
	}
	for _, conn := range dialer.ClientConnections() {
		if err := conn.SetDeadline(time.Now()); err == nil {
			t.Fatal("请求返回后客户端连接未关闭")
		}
	}
}

func TestClientRequestReadsResponseWrittenInParts(t *testing.T) {
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) {
		id, _ := request["id"].(string)
		for _, part := range []string{
			`{"id":"` + id,
			`","result":{"status":"ok"}}`,
			"\n",
		} {
			if _, err := io.WriteString(conn, part); err != nil {
				return
			}
		}
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	var result struct {
		Status string `json:"status"`
	}
	if err := client.call(context.Background(), "test.method", map[string]any{}, &result); err != nil {
		t.Fatalf("call() 返回错误：%v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("result = %+v", result)
	}
}

func TestClientRequestWrapsDialFailureAsUnavailable(t *testing.T) {
	client := NewClient("/tmp/herdr.sock", failingDialer{err: context.DeadlineExceeded}, time.Second)
	err := client.call(context.Background(), "test.method", map[string]any{}, nil)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call() 错误 = %v，期望同时匹配 ErrUnavailable 和原始错误", err)
	}
}

func TestClientRequestClassifiesDialTimeoutAsDeadlineExceeded(t *testing.T) {
	dialError := errors.New("拨号被中止")
	client := NewClient("/tmp/herdr.sock", waitingDialer{err: dialError}, 20*time.Millisecond)

	err := client.call(context.Background(), "test.method", map[string]any{}, nil)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, dialError) {
		t.Fatalf("call() 错误 = %v，期望匹配 ErrUnavailable、context.DeadlineExceeded 和底层错误", err)
	}
}

func TestClientRequestClassifiesEmptyEOFAsUnavailable(t *testing.T) {
	dialer := &pipeDialer{handler: func(net.Conn, map[string]any) {}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	err := client.call(context.Background(), "test.method", map[string]any{}, nil)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, io.EOF) {
		t.Fatalf("call() 错误 = %v，期望匹配 ErrUnavailable 和 io.EOF", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("call() 错误 = %v，不应匹配 ErrProtocol", err)
	}
}

func TestClientRequestRejectsUnencodableParamsBeforeDialing(t *testing.T) {
	dialer := &pipeDialer{}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	err := client.call(context.Background(), "test.method", make(chan int), nil)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("call() 错误 = %v，期望 ErrProtocol", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("call() 错误 = %v，不应匹配 ErrUnavailable", err)
	}
	if got := dialer.Count(); got != 0 {
		t.Fatalf("DialContext() 调用次数 = %d，期望 0", got)
	}
}

func TestClientRequestDeadlineTerminatesBlockedRead(t *testing.T) {
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) {
		_, _ = conn.Read(make([]byte, 1))
	}}
	client := NewClient("/tmp/herdr.sock", dialer, 20*time.Millisecond)

	started := time.Now()
	err := client.call(context.Background(), "test.method", map[string]any{}, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("call() 错误 = %v，期望 ErrUnavailable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call() 错误 = %v，期望 context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("call() 阻塞过久：%s", elapsed)
	}
}

func TestClientRequestCancellationTerminatesBlockedRead(t *testing.T) {
	started := make(chan struct{})
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) {
		close(started)
		_, _ = conn.Read(make([]byte, 1))
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.call(ctx, "test.method", map[string]any{}, nil)
	}()
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("服务端未收到请求")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("call() 错误 = %v，期望匹配 ErrUnavailable 和 context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("call() 未在 context 取消后及时返回")
	}
}

type failingDialer struct {
	err error
}

func (d failingDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, d.err
}

type waitingDialer struct {
	err error
}

func (d waitingDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	<-ctx.Done()
	return nil, d.err
}

type pipeDialer struct {
	handler func(net.Conn, map[string]any)

	mu          sync.Mutex
	connections []net.Conn
}

func (d *pipeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	client, server := net.Pipe()
	d.mu.Lock()
	d.connections = append(d.connections, client)
	d.mu.Unlock()

	go func() {
		defer server.Close()
		requestLine, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(requestLine, &request); err != nil {
			return
		}
		if d.handler != nil {
			d.handler(server, request)
			return
		}
		index := int(request["params"].(map[string]any)["index"].(float64))
		_, _ = io.WriteString(server, `{"id":"`+request["id"].(string)+`","result":{"value":`+strconv.Itoa(index)+"}}\n")
	}()

	return client, nil
}

func (d *pipeDialer) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.connections)
}

func (d *pipeDialer) ClientConnections() []net.Conn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]net.Conn(nil), d.connections...)
}

func TestClientCheckCompatibleSendsPingAndAcceptsRequiredProtocol(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"pong","version":"0.17.0","protocol":17,"unknown":true}`, func(request map[string]any) {
		assertBusinessRequest(t, request, "ping", map[string]any{})
	})

	if err := client.CheckCompatible(context.Background()); err != nil {
		t.Fatalf("CheckCompatible() 返回错误：%v", err)
	}
}

func TestClientCheckCompatibleRejectsInvalidPong(t *testing.T) {
	tests := []struct {
		name   string
		result string
		match  error
		parts  []string
	}{
		{name: "协议过低", result: `{"type":"pong","version":"0.14.0","protocol":14}`, match: ErrProtocolMismatch, parts: []string{"expected 17", "got 14"}},
		{name: "协议过高", result: `{"type":"pong","version":"0.18.0","protocol":18}`, match: ErrProtocolMismatch, parts: []string{"expected 17", "got 18"}},
		{name: "缺少 type", result: `{"version":"0.17.0","protocol":17}`, match: ErrProtocol},
		{name: "type 不匹配", result: `{"type":"ok","version":"0.17.0","protocol":17}`, match: ErrProtocol},
		{name: "缺少 protocol", result: `{"type":"pong","version":"0.17.0"}`, match: ErrProtocol},
		{name: "protocol 类型错误", result: `{"type":"pong","version":"0.17.0","protocol":"17"}`, match: ErrProtocol},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newBusinessTestClient(t, test.result, nil)
			err := client.CheckCompatible(context.Background())
			if !errors.Is(err, test.match) {
				t.Fatalf("CheckCompatible() 错误 = %v，期望匹配 %v", err, test.match)
			}
			if errors.Is(err, ErrProtocolMismatch) && errors.Is(err, ErrUnavailable) {
				t.Fatalf("CheckCompatible() 错误 = %v，不应同时匹配 ErrUnavailable", err)
			}
			for _, part := range test.parts {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("CheckCompatible() 错误 = %q，缺少 %q", err, part)
				}
			}
		})
	}
}

func TestClientSnapshotDecodesMinimalSnapshot(t *testing.T) {
	result := `{"type":"session_snapshot","snapshot":{"version":"0.17.0","protocol":17,"workspaces":[{"workspace_id":"w1","number":1,"label":"主工作区","ignored":true}],"tabs":[{"tab_id":"t1","workspace_id":"w1","number":2,"label":"终端"}],"panes":[{"pane_id":"p1","terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent":"codex","title":"实现","display_agent":"Codex","agent_status":"working","agent_session":{"source":"codex","agent":"codex","kind":"thread","value":"session-1"}}],"agents":[{"terminal_id":"term1","name":"任务","agent":"codex","title":"实现","terminal_title":"term","terminal_title_stripped":"term","display_agent":"Codex","agent_status":"working","agent_session":{"source":"codex","agent":"codex","kind":"thread","value":"session-1"},"workspace_id":"w1","tab_id":"t1","pane_id":"p1"}],"layouts":[]}}`
	client := newBusinessTestClient(t, result, func(request map[string]any) {
		assertBusinessRequest(t, request, "session.snapshot", map[string]any{})
	})

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() 返回错误：%v", err)
	}
	if snapshot.Version != "0.17.0" || snapshot.Protocol != 17 || len(snapshot.Workspaces) != 1 || snapshot.Workspaces[0].WorkspaceID != "w1" {
		t.Fatalf("Snapshot() = %+v，未解码快照最小字段", snapshot)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].AgentSession == nil || snapshot.Agents[0].AgentSession.Value != "session-1" {
		t.Fatalf("Snapshot() agents = %+v，未解码 AgentSession", snapshot.Agents)
	}
}

func TestClientSnapshotRejectsInvalidResult(t *testing.T) {
	for _, result := range []string{
		`{"type":"ok","snapshot":{}}`,
		`{"type":"session_snapshot"}`,
		`{"type":"session_snapshot","snapshot":"invalid"}`,
	} {
		client := newBusinessTestClient(t, result, nil)
		if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrProtocol) {
			t.Fatalf("Snapshot() result %s 错误 = %v，期望 ErrProtocol", result, err)
		}
	}
}

func TestClientSnapshotRejectsMissingRequiredWireFields(t *testing.T) {
	tests := []struct {
		name     string
		fragment string
	}{
		{name: "缺 protocol", fragment: `"workspaces":[],"tabs":[],"panes":[],"agents":[]`},
		{name: "Workspace 缺 workspace_id", fragment: `"protocol":0,"workspaces":[{"number":0,"label":""}],"tabs":[],"panes":[],"agents":[]`},
		{name: "Workspace 缺 number", fragment: `"protocol":0,"workspaces":[{"workspace_id":"w1","label":""}],"tabs":[],"panes":[],"agents":[]`},
		{name: "Workspace 缺 label", fragment: `"protocol":0,"workspaces":[{"workspace_id":"w1","number":0}],"tabs":[],"panes":[],"agents":[]`},
		{name: "Tab 缺 tab_id", fragment: `"protocol":0,"workspaces":[],"tabs":[{"workspace_id":"w1","number":0,"label":""}],"panes":[],"agents":[]`},
		{name: "Tab 缺 workspace_id", fragment: `"protocol":0,"workspaces":[],"tabs":[{"tab_id":"t1","number":0,"label":""}],"panes":[],"agents":[]`},
		{name: "Tab 缺 number", fragment: `"protocol":0,"workspaces":[],"tabs":[{"tab_id":"t1","workspace_id":"w1","label":""}],"panes":[],"agents":[]`},
		{name: "Tab 缺 label", fragment: `"protocol":0,"workspaces":[],"tabs":[{"tab_id":"t1","workspace_id":"w1","number":0}],"panes":[],"agents":[]`},
		{name: "Pane 缺 pane_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent_status":"idle"}],"agents":[]`},
		{name: "Pane 缺 terminal_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","workspace_id":"w1","tab_id":"t1","agent_status":"idle"}],"agents":[]`},
		{name: "Pane 缺 workspace_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","terminal_id":"term1","tab_id":"t1","agent_status":"idle"}],"agents":[]`},
		{name: "Pane 缺 tab_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","terminal_id":"term1","workspace_id":"w1","agent_status":"idle"}],"agents":[]`},
		{name: "Pane 缺 agent_status", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","terminal_id":"term1","workspace_id":"w1","tab_id":"t1"}],"agents":[]`},
		{name: "Pane 非法 agent_status", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent_status":"running"}],"agents":[]`},
		{name: "Pane 会话缺 source", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[{"pane_id":"p1","terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent_status":"idle","agent_session":{"agent":"codex","kind":"id","value":"session-1"}}],"agents":[]`},
		{name: "Agent 缺 terminal_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle"}]`},
		{name: "Agent 缺 workspace_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","tab_id":"t1","pane_id":"p1","agent_status":"idle"}]`},
		{name: "Agent 缺 tab_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","workspace_id":"w1","pane_id":"p1","agent_status":"idle"}]`},
		{name: "Agent 缺 pane_id", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent_status":"idle"}]`},
		{name: "Agent 缺 agent_status", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1"}]`},
		{name: "Agent 非法 agent_status", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"running"}]`},
		{name: "Agent 会话缺 source", fragment: `"protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"agent":"codex","kind":"id","value":"session-1"}}]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := fmt.Sprintf(`{"type":"session_snapshot","snapshot":{"version":"0.17.0",%s}}`, test.fragment)
			client := newBusinessTestClient(t, result, nil)
			if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Snapshot() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestClientSnapshotAcceptsExplicitZeroProtocol(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"session_snapshot","snapshot":{"version":"0.17.0","protocol":0,"workspaces":[],"tabs":[],"panes":[],"agents":[]}}`, nil)

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() 返回错误：%v", err)
	}
	if snapshot.Protocol != 0 {
		t.Fatalf("Snapshot().Protocol = %d，期望 0", snapshot.Protocol)
	}
}

func TestClientSnapshotAcceptsZeroNumbersAndEmptyLabels(t *testing.T) {
	result := `{"type":"session_snapshot","snapshot":{"version":"0.17.0","protocol":0,"workspaces":[{"workspace_id":"w1","number":0,"label":""}],"tabs":[{"tab_id":"t1","workspace_id":"w1","number":0,"label":""}],"panes":[],"agents":[]}}`
	client := newBusinessTestClient(t, result, nil)

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() 返回错误：%v", err)
	}
	if snapshot.Workspaces[0].Number != 0 || snapshot.Workspaces[0].Label != "" || snapshot.Tabs[0].Number != 0 || snapshot.Tabs[0].Label != "" {
		t.Fatalf("Snapshot() = %+v，未保留合法零值", snapshot)
	}
}

func TestClientGetAgentSendsTargetAndDecodesAgent(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":{"terminal_id":"term1","agent_status":"blocked","workspace_id":"w1","tab_id":"t1","pane_id":"p1","unknown":1}}`, func(request map[string]any) {
		assertBusinessRequest(t, request, "agent.get", map[string]any{"target": "p1"})
	})

	agent, err := client.GetAgent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetAgent() 返回错误：%v", err)
	}
	if agent.TerminalID != "term1" || agent.PaneID != "p1" || agent.AgentStatus != AgentStatusBlocked {
		t.Fatalf("GetAgent() = %+v", agent)
	}
}

func TestClientGetAgentRejectsInvalidInputOrResult(t *testing.T) {
	dialer := &pipeDialer{}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	if _, err := client.GetAgent(context.Background(), " \t "); !errors.Is(err, ErrProtocol) {
		t.Fatalf("GetAgent() 空 target 错误 = %v，期望 ErrProtocol", err)
	}
	if dialer.Count() != 0 {
		t.Fatalf("GetAgent() 无效 target 仍拨号 %d 次", dialer.Count())
	}
	for _, result := range []string{`{"type":"ok","agent":{}}`, `{"type":"agent_info"}`, `{"type":"agent_info","agent":"invalid"}`} {
		client := newBusinessTestClient(t, result, nil)
		if _, err := client.GetAgent(context.Background(), "p1"); !errors.Is(err, ErrProtocol) {
			t.Fatalf("GetAgent() result %s 错误 = %v，期望 ErrProtocol", result, err)
		}
	}
}

func TestClientGetAgentAndPromptRejectInvalidAgentInfo(t *testing.T) {
	tests := []struct {
		name  string
		agent string
	}{
		{name: "缺 terminal_id", agent: `{"workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle"}`},
		{name: "缺 workspace_id", agent: `{"terminal_id":"term1","tab_id":"t1","pane_id":"p1","agent_status":"idle"}`},
		{name: "缺 tab_id", agent: `{"terminal_id":"term1","workspace_id":"w1","pane_id":"p1","agent_status":"idle"}`},
		{name: "缺 pane_id", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","agent_status":"idle"}`},
		{name: "缺 agent_status", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1"}`},
		{name: "非法 agent_status", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"running"}`},
		{name: "会话缺 source", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"agent":"codex","kind":"id","value":"session-1"}}`},
		{name: "会话缺 agent", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"source":"codex","kind":"id","value":"session-1"}}`},
		{name: "会话缺 kind", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"source":"codex","agent":"codex","value":"session-1"}}`},
		{name: "会话缺 value", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"source":"codex","agent":"codex","kind":"id"}}`},
		{name: "会话空白必填字段", agent: `{"terminal_id":"term1","workspace_id":"w1","tab_id":"t1","pane_id":"p1","agent_status":"idle","agent_session":{"source":" ","agent":"codex","kind":"id","value":"session-1"}}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			getClient := newBusinessTestClient(t, `{"type":"agent_info","agent":`+test.agent+`}`, nil)
			if _, err := getClient.GetAgent(context.Background(), "p1"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("GetAgent() 错误 = %v，期望 ErrProtocol", err)
			}
			promptClient := newBusinessTestClient(t, `{"type":"agent_prompted","agent":`+test.agent+`}`, nil)
			if err := promptClient.Prompt(context.Background(), "p1", "text"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Prompt() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestClientReadRecentSendsExactParametersAndDecodesRead(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"pane_read","read":{"pane_id":"p1","workspace_id":"w1","tab_id":"t1","text":"输出","truncated":true,"revision":0}}`, func(request map[string]any) {
		assertBusinessRequest(t, request, "agent.read", map[string]any{"target": "p1", "source": "recent_unwrapped", "lines": float64(42), "format": "text", "strip_ansi": true})
	})

	read, err := client.ReadRecent(context.Background(), "p1", 42)
	if err != nil {
		t.Fatalf("ReadRecent() 返回错误：%v", err)
	}
	if read.PaneID != "p1" || read.Text != "输出" || !read.Truncated {
		t.Fatalf("ReadRecent() = %+v", read)
	}
}

func TestClientReadRecentRejectsInvalidInputOrResult(t *testing.T) {
	dialer := &pipeDialer{}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	if _, err := client.ReadRecent(context.Background(), " \t ", 1); !errors.Is(err, ErrProtocol) {
		t.Fatalf("ReadRecent() 空 target 错误 = %v，期望 ErrProtocol", err)
	}
	if dialer.Count() != 0 {
		t.Fatalf("ReadRecent() 空 target 仍拨号")
	}
	for _, lines := range []int{0, -1, 1001} {
		dialer := &pipeDialer{}
		client := NewClient("/tmp/herdr.sock", dialer, time.Second)
		if _, err := client.ReadRecent(context.Background(), "p1", lines); !errors.Is(err, ErrProtocol) {
			t.Fatalf("ReadRecent() lines %d 错误 = %v，期望 ErrProtocol", lines, err)
		}
		if dialer.Count() != 0 {
			t.Fatalf("ReadRecent() 无效 lines %d 仍拨号", lines)
		}
	}
	for _, result := range []string{`{"type":"ok","read":{}}`, `{"type":"pane_read"}`, `{"type":"pane_read","read":"invalid"}`} {
		client := newBusinessTestClient(t, result, nil)
		if _, err := client.ReadRecent(context.Background(), "p1", 1); !errors.Is(err, ErrProtocol) {
			t.Fatalf("ReadRecent() result %s 错误 = %v，期望 ErrProtocol", result, err)
		}
	}
}

func TestClientReadRecentDistinguishesMissingFieldsFromZeroValues(t *testing.T) {
	for _, read := range []string{
		`{"pane_id":"p1","workspace_id":"w1","tab_id":"t1","truncated":false}`,
		`{"pane_id":"p1","workspace_id":"w1","tab_id":"t1","text":""}`,
	} {
		client := newBusinessTestClient(t, `{"type":"pane_read","read":`+read+`}`, nil)
		if _, err := client.ReadRecent(context.Background(), "p1", 1); !errors.Is(err, ErrProtocol) {
			t.Fatalf("ReadRecent() 缺字段错误 = %v，期望 ErrProtocol", err)
		}
	}

	client := newBusinessTestClient(t, `{"type":"pane_read","read":{"pane_id":"p1","workspace_id":"w1","tab_id":"t1","text":"","truncated":false}}`, nil)
	read, err := client.ReadRecent(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("ReadRecent() 返回错误：%v", err)
	}
	if read.Text != "" || read.Truncated {
		t.Fatalf("ReadRecent() = %+v，期望空 text 和 truncated=false", read)
	}
}

func TestClientPromptSendsExactParameters(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"agent_prompted","agent":{"terminal_id":"term1","agent_status":"working","workspace_id":"w1","tab_id":"t1","pane_id":"p1"}}`, func(request map[string]any) {
		assertBusinessRequest(t, request, "agent.prompt", map[string]any{"target": "p1", "text": ""})
	})

	if err := client.Prompt(context.Background(), "p1", ""); err != nil {
		t.Fatalf("Prompt() 返回错误：%v", err)
	}
}

func TestClientPromptRejectsEmptyTargetAndInvalidResult(t *testing.T) {
	dialer := &pipeDialer{}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	if err := client.Prompt(context.Background(), " ", "text"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Prompt() 空 target 错误 = %v，期望 ErrProtocol", err)
	}
	if dialer.Count() != 0 {
		t.Fatalf("Prompt() 无效 target 仍拨号")
	}
	for _, result := range []string{`{"type":"ok","agent":{}}`, `{"type":"agent_prompted"}`, `{"type":"agent_prompted","agent":"invalid"}`} {
		client := newBusinessTestClient(t, result, nil)
		if err := client.Prompt(context.Background(), "p1", "text"); !errors.Is(err, ErrProtocol) {
			t.Fatalf("Prompt() result %s 错误 = %v，期望 ErrProtocol", result, err)
		}
	}
}

func TestClientSendKeySendsExactParameters(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"ok","ignored":true}`, func(request map[string]any) {
		assertBusinessRequest(t, request, "agent.send_keys", map[string]any{"target": "p1", "keys": []any{"enter"}})
	})

	if err := client.SendKey(context.Background(), "p1", "enter"); err != nil {
		t.Fatalf("SendKey() 返回错误：%v", err)
	}
}

func TestClientSendKeyRejectsInvalidInputAndResult(t *testing.T) {
	for _, input := range []struct{ target, key string }{{"", "enter"}, {"p1", " \t "}} {
		dialer := &pipeDialer{}
		client := NewClient("/tmp/herdr.sock", dialer, time.Second)
		if err := client.SendKey(context.Background(), input.target, input.key); !errors.Is(err, ErrProtocol) {
			t.Fatalf("SendKey(%q, %q) 错误 = %v，期望 ErrProtocol", input.target, input.key, err)
		}
		if dialer.Count() != 0 {
			t.Fatalf("SendKey() 无效输入仍拨号")
		}
	}
	client := newBusinessTestClient(t, `{"type":"agent_prompted"}`, nil)
	if err := client.SendKey(context.Background(), "p1", "enter"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("SendKey() type 不匹配错误 = %v，期望 ErrProtocol", err)
	}
}

func newBusinessTestClient(t *testing.T, result string, check func(map[string]any)) *Client {
	t.Helper()
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) {
		if check != nil {
			check(request)
		}
		_, _ = io.WriteString(conn, `{"id":"`+request["id"].(string)+`","result":`+result+"}\n")
	}}
	return NewClient("/tmp/herdr.sock", dialer, time.Second)
}

func assertBusinessRequest(t *testing.T, request map[string]any, method string, params map[string]any) {
	t.Helper()
	if request["method"] != method {
		t.Fatalf("method = %q，期望 %q", request["method"], method)
	}
	actual, ok := request["params"].(map[string]any)
	if !ok {
		t.Fatalf("params 类型 = %T，期望 object", request["params"])
	}
	if !reflect.DeepEqual(actual, params) {
		t.Fatalf("params = %#v，期望 %#v", actual, params)
	}
}
