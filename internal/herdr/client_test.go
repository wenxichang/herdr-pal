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
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		for _, part := range []string{
			`{"id":"` + id,
			`","result":{"status":"ok"}}`,
			"\n",
		} {
			if _, err := io.WriteString(conn, part); err != nil {
				return err
			}
		}
		return nil
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
	dialer := &pipeDialer{handler: func(net.Conn, map[string]any) error { return nil }}
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
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		_, _ = conn.Read(make([]byte, 1))
		return nil
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
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		close(started)
		_, _ = conn.Read(make([]byte, 1))
		return nil
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
	handler func(net.Conn, map[string]any) error

	mu          sync.Mutex
	connections []net.Conn
	handlerErrs []error
	wg          sync.WaitGroup
}

func (d *pipeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	client, server := net.Pipe()
	d.mu.Lock()
	d.connections = append(d.connections, client)
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer server.Close()
		requestLine, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(requestLine, &request); err != nil {
			d.recordHandlerError(err)
			return
		}
		if d.handler != nil {
			d.recordHandlerError(d.handler(server, request))
			return
		}
		params, ok := request["params"].(map[string]any)
		if !ok {
			d.recordHandlerError(errors.New("请求 params 不是对象"))
			return
		}
		index, ok := params["index"].(float64)
		if !ok {
			d.recordHandlerError(errors.New("请求缺少 index"))
			return
		}
		_, err = io.WriteString(server, `{"id":"`+request["id"].(string)+`","result":{"value":`+strconv.Itoa(int(index))+"}}\n")
		d.recordHandlerError(err)
	}()

	return client, nil
}

func (d *pipeDialer) recordHandlerError(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlerErrs = append(d.handlerErrs, err)
}

func (d *pipeDialer) assertNoHandlerError(t *testing.T) {
	t.Helper()
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.handlerErrs) != 0 {
		t.Errorf("测试服务端处理请求失败：%v", d.handlerErrs[0])
	}
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
	client := newBusinessTestClient(t, `{"type":"pong","version":"0.17.0","protocol":17,"unknown":true}`, businessRequestCheck("ping", map[string]any{}))

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
	result := snapshotResultJSON(t, nil)
	client := newBusinessTestClient(t, result, businessRequestCheck("session.snapshot", map[string]any{}))

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
	client := newBusinessTestClient(t, snapshotResultJSON(t, func(snapshot map[string]any) {
		snapshot["protocol"] = 0
		snapshot["workspaces"] = []any{}
		snapshot["tabs"] = []any{}
		snapshot["panes"] = []any{}
		snapshot["agents"] = []any{}
	}), nil)

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() 返回错误：%v", err)
	}
	if snapshot.Protocol != 0 {
		t.Fatalf("Snapshot().Protocol = %d，期望 0", snapshot.Protocol)
	}
}

func TestClientSnapshotAcceptsZeroNumbersAndEmptyLabels(t *testing.T) {
	client := newBusinessTestClient(t, snapshotResultJSON(t, func(snapshot map[string]any) {
		snapshot["protocol"] = 0
		workspace := snapshot["workspaces"].([]any)[0].(map[string]any)
		workspace["number"] = 0
		workspace["label"] = ""
		tab := snapshot["tabs"].([]any)[0].(map[string]any)
		tab["number"] = 0
		tab["label"] = ""
		snapshot["panes"] = []any{}
		snapshot["agents"] = []any{}
	}), nil)

	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() 返回错误：%v", err)
	}
	if snapshot.Workspaces[0].Number != 0 || snapshot.Workspaces[0].Label != "" || snapshot.Tabs[0].Number != 0 || snapshot.Tabs[0].Label != "" {
		t.Fatalf("Snapshot() = %+v，未保留合法零值", snapshot)
	}
}

func TestClientSnapshotRejectsMissingProtocol17RequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "顶层缺 version", mutate: func(snapshot map[string]any) { delete(snapshot, "version") }},
		{name: "顶层空白 version", mutate: func(snapshot map[string]any) { snapshot["version"] = " \t " }},
		{name: "顶层缺 protocol", mutate: func(snapshot map[string]any) { delete(snapshot, "protocol") }},
		{name: "顶层缺 workspaces", mutate: func(snapshot map[string]any) { delete(snapshot, "workspaces") }},
		{name: "顶层缺 tabs", mutate: func(snapshot map[string]any) { delete(snapshot, "tabs") }},
		{name: "顶层缺 panes", mutate: func(snapshot map[string]any) { delete(snapshot, "panes") }},
		{name: "顶层缺 layouts", mutate: func(snapshot map[string]any) { delete(snapshot, "layouts") }},
		{name: "顶层缺 agents", mutate: func(snapshot map[string]any) { delete(snapshot, "agents") }},
		{name: "Workspace 缺 workspace_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "workspace_id") }},
		{name: "Workspace 缺 number", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "number") }},
		{name: "Workspace 缺 label", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "label") }},
		{name: "Workspace 缺 focused", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "focused") }},
		{name: "Workspace 缺 pane_count", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "pane_count") }},
		{name: "Workspace 缺 tab_count", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "tab_count") }},
		{name: "Workspace 缺 active_tab_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "workspaces"), "active_tab_id") }},
		{name: "Workspace 负 number", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "workspaces")["number"] = -1 }},
		{name: "Workspace 负 pane_count", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "workspaces")["pane_count"] = -1 }},
		{name: "Workspace 负 tab_count", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "workspaces")["tab_count"] = -1 }},
		{name: "Workspace 非法状态", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "workspaces")["agent_status"] = "invalid" }},
		{name: "Tab 缺 tab_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "tab_id") }},
		{name: "Tab 缺 workspace_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "workspace_id") }},
		{name: "Tab 缺 number", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "number") }},
		{name: "Tab 缺 label", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "label") }},
		{name: "Tab 缺 focused", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "focused") }},
		{name: "Tab 缺 pane_count", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "tabs"), "pane_count") }},
		{name: "Tab 负 number", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "tabs")["number"] = -1 }},
		{name: "Tab 负 pane_count", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "tabs")["pane_count"] = -1 }},
		{name: "Tab 非法状态", mutate: func(snapshot map[string]any) { firstSnapshotObject(snapshot, "tabs")["agent_status"] = "invalid" }},
		{name: "Pane 缺 pane_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "pane_id") }},
		{name: "Pane 缺 terminal_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "terminal_id") }},
		{name: "Pane 缺 workspace_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "workspace_id") }},
		{name: "Pane 缺 tab_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "tab_id") }},
		{name: "Pane 缺 focused", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "focused") }},
		{name: "Pane 缺 agent_status", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "agent_status") }},
		{name: "Pane 缺 revision", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "panes"), "revision") }},
		{name: "Agent 缺 terminal_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "terminal_id") }},
		{name: "Agent 缺 workspace_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "workspace_id") }},
		{name: "Agent 缺 tab_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "tab_id") }},
		{name: "Agent 缺 pane_id", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "pane_id") }},
		{name: "Agent 缺 focused", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "focused") }},
		{name: "Agent 缺 agent_status", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "agent_status") }},
		{name: "Agent 缺 revision", mutate: func(snapshot map[string]any) { delete(firstSnapshotObject(snapshot, "agents"), "revision") }},
		{name: "会话非法 kind", mutate: func(snapshot map[string]any) {
			firstSnapshotObject(snapshot, "agents")["agent_session"].(map[string]any)["kind"] = "thread"
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newBusinessTestClient(t, snapshotResultJSON(t, test.mutate), nil)
			if _, err := client.Snapshot(context.Background()); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Snapshot() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestClientReadRecentRejectsMismatchedRequiredWireFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "缺 pane_id", mutate: func(read map[string]any) { delete(read, "pane_id") }},
		{name: "缺 workspace_id", mutate: func(read map[string]any) { delete(read, "workspace_id") }},
		{name: "缺 tab_id", mutate: func(read map[string]any) { delete(read, "tab_id") }},
		{name: "缺 source", mutate: func(read map[string]any) { delete(read, "source") }},
		{name: "非法 source", mutate: func(read map[string]any) { read["source"] = "other" }},
		{name: "source 与请求不符", mutate: func(read map[string]any) { read["source"] = "recent" }},
		{name: "缺 format", mutate: func(read map[string]any) { delete(read, "format") }},
		{name: "非法 format", mutate: func(read map[string]any) { read["format"] = "html" }},
		{name: "format 与请求不符", mutate: func(read map[string]any) { read["format"] = "ansi" }},
		{name: "缺 text", mutate: func(read map[string]any) { delete(read, "text") }},
		{name: "缺 revision", mutate: func(read map[string]any) { delete(read, "revision") }},
		{name: "缺 truncated", mutate: func(read map[string]any) { delete(read, "truncated") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			read := validReadResult(t)
			test.mutate(read)
			client := newBusinessTestClient(t, `{"type":"pane_read","read":`+mustJSON(t, read)+`}`, nil)
			if _, err := client.ReadRecent(context.Background(), "p1", 1); !errors.Is(err, ErrProtocol) {
				t.Fatalf("ReadRecent() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestClientGetAgentSendsTargetAndDecodesAgent(t *testing.T) {
	agentPayload := validAgentInfo(t)
	agentPayload["agent_status"] = "blocked"
	agentJSON := mustJSON(t, agentPayload)
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":`+agentJSON+`}`, businessRequestCheck("agent.get", map[string]any{"target": "p1"}))

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

func TestClientGetAgentDecodesStateChangeSequence(t *testing.T) {
	agent := validAgentInfo(t)
	agent["state_change_seq"] = float64(27)
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":`+mustJSON(t, agent)+`}`, nil)

	got, err := client.GetAgent(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetAgent() 返回错误：%v", err)
	}
	if got.StateChangeSeq != 27 {
		t.Fatalf("StateChangeSeq = %d, want 27", got.StateChangeSeq)
	}
}

func TestClientGetAgentAndPromptRejectInvalidAgentInfo(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "缺 terminal_id", mutate: func(agent map[string]any) { delete(agent, "terminal_id") }},
		{name: "缺 workspace_id", mutate: func(agent map[string]any) { delete(agent, "workspace_id") }},
		{name: "缺 tab_id", mutate: func(agent map[string]any) { delete(agent, "tab_id") }},
		{name: "缺 pane_id", mutate: func(agent map[string]any) { delete(agent, "pane_id") }},
		{name: "缺 focused", mutate: func(agent map[string]any) { delete(agent, "focused") }},
		{name: "缺 revision", mutate: func(agent map[string]any) { delete(agent, "revision") }},
		{name: "缺 state_change_seq", mutate: func(agent map[string]any) { delete(agent, "state_change_seq") }},
		{name: "缺 agent_status", mutate: func(agent map[string]any) { delete(agent, "agent_status") }},
		{name: "非法 agent_status", mutate: func(agent map[string]any) { agent["agent_status"] = "running" }},
		{name: "会话缺 source", mutate: func(agent map[string]any) { delete(agent["agent_session"].(map[string]any), "source") }},
		{name: "会话缺 agent", mutate: func(agent map[string]any) { delete(agent["agent_session"].(map[string]any), "agent") }},
		{name: "会话缺 kind", mutate: func(agent map[string]any) { delete(agent["agent_session"].(map[string]any), "kind") }},
		{name: "会话缺 value", mutate: func(agent map[string]any) { delete(agent["agent_session"].(map[string]any), "value") }},
		{name: "会话非法 kind", mutate: func(agent map[string]any) { agent["agent_session"].(map[string]any)["kind"] = "thread" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := validAgentInfo(t)
			test.mutate(agent)
			payload := mustJSON(t, agent)
			getClient := newBusinessTestClient(t, `{"type":"agent_info","agent":`+payload+`}`, nil)
			if _, err := getClient.GetAgent(context.Background(), "p1"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("GetAgent() 错误 = %v，期望 ErrProtocol", err)
			}
			promptClient := newBusinessTestClient(t, `{"type":"agent_prompted","agent":`+payload+`}`, nil)
			if err := promptClient.Prompt(context.Background(), "p1", "text"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Prompt() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestClientReadRecentSendsExactParametersAndDecodesRead(t *testing.T) {
	readPayload := validReadResult(t)
	readPayload["text"] = "输出"
	readPayload["truncated"] = true
	client := newBusinessTestClient(t, `{"type":"pane_read","read":`+mustJSON(t, readPayload)+`}`, businessRequestCheck("agent.read", map[string]any{"target": "p1", "source": "recent_unwrapped", "lines": float64(42), "format": "text", "strip_ansi": true}))

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

	valid := validReadResult(t)
	client := newBusinessTestClient(t, `{"type":"pane_read","read":`+mustJSON(t, valid)+`}`, nil)
	read, err := client.ReadRecent(context.Background(), "p1", 1)
	if err != nil {
		t.Fatalf("ReadRecent() 返回错误：%v", err)
	}
	if read.Text != "" || read.Truncated {
		t.Fatalf("ReadRecent() = %+v，期望空 text 和 truncated=false", read)
	}
}

func TestClientPromptSendsExactParameters(t *testing.T) {
	client := newBusinessTestClient(t, `{"type":"agent_prompted","agent":`+validAgentInfoJSON(t)+`}`, businessRequestCheck("agent.prompt", map[string]any{"target": "p1", "text": ""}))

	if err := client.Prompt(context.Background(), "p1", ""); err != nil {
		t.Fatalf("Prompt() 返回错误：%v", err)
	}
}

func TestClientPromptUntilStateChangeSendsWaitAndDecodesAgent(t *testing.T) {
	agent := validAgentInfo(t)
	agent["agent_status"] = "working"
	agent["state_change_seq"] = float64(8)
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":`+mustJSON(t, agent)+`}`, businessRequestCheck("agent.prompt", map[string]any{
		"target": "p1",
		"text":   "运行测试",
		"wait": map[string]any{
			"until": []any{"idle", "working", "blocked", "done", "unknown"},
		},
	}))

	got, err := client.PromptUntilStateChange(context.Background(), "p1", "运行测试")
	if err != nil {
		t.Fatalf("PromptUntilStateChange() 返回错误：%v", err)
	}
	if got.AgentStatus != AgentStatusWorking || got.StateChangeSeq != 8 {
		t.Fatalf("PromptUntilStateChange() = %+v", got)
	}
}

func TestClientPromptUntilStateChangePreservesStalledAPIError(t *testing.T) {
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		_, err := io.WriteString(conn, `{"id":"`+id+`","error":{"code":"agent_prompt_stalled","message":"no state change"}}`+"\n")
		return err
	}}
	t.Cleanup(func() { dialer.assertNoHandlerError(t) })
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	_, err := client.PromptUntilStateChange(context.Background(), "p1", "text")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "agent_prompt_stalled" {
		t.Fatalf("PromptUntilStateChange() 错误 = %T %[1]v", err)
	}
}

func TestClientWaitForStateChangeReturnsImmediatelyForNewSequence(t *testing.T) {
	agent := validAgentInfo(t)
	agent["state_change_seq"] = float64(2)
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":`+mustJSON(t, agent)+`}`, businessRequestCheck("agent.get", map[string]any{"target": "p1"}))

	got, err := client.WaitForStateChange(context.Background(), "p1", 1, time.Second)
	if err != nil {
		t.Fatalf("WaitForStateChange() 返回错误：%v", err)
	}
	if got.StateChangeSeq != 2 {
		t.Fatalf("WaitForStateChange() = %+v", got)
	}
}

func TestClientWaitForStateChangeTimesOutWithoutNewSequence(t *testing.T) {
	agent := validAgentInfo(t)
	agent["state_change_seq"] = float64(1)
	client := newBusinessTestClient(t, `{"type":"agent_info","agent":`+mustJSON(t, agent)+`}`, businessRequestCheck("agent.get", map[string]any{"target": "p1"}))

	_, err := client.WaitForStateChange(context.Background(), "p1", 1, time.Millisecond)
	if !errors.Is(err, ErrAgentStateChangeTimeout) {
		t.Fatalf("WaitForStateChange() 错误 = %v，期望 ErrAgentStateChangeTimeout", err)
	}
}

func TestClientWaitForStateChangePropagatesGetAgentError(t *testing.T) {
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		_, err := io.WriteString(conn, `{"id":"`+id+`","error":{"code":"agent_not_found","message":"missing"}}`+"\n")
		return err
	}}
	t.Cleanup(func() { dialer.assertNoHandlerError(t) })
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)

	_, err := client.WaitForStateChange(context.Background(), "p1", 1, time.Second)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "agent_not_found" {
		t.Fatalf("WaitForStateChange() 错误 = %T %[1]v", err)
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
	client := newBusinessTestClient(t, `{"type":"ok","ignored":true}`, businessRequestCheck("agent.send_keys", map[string]any{"target": "p1", "keys": []any{"enter"}}))

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

func newBusinessTestClient(t *testing.T, result string, check func(map[string]any) error) *Client {
	t.Helper()
	dialer := &pipeDialer{handler: func(conn net.Conn, request map[string]any) error {
		if check != nil {
			if err := check(request); err != nil {
				return err
			}
		}
		id, ok := request["id"].(string)
		if !ok {
			return errors.New("请求缺少 id")
		}
		_, err := io.WriteString(conn, `{"id":"`+id+`","result":`+result+"}\n")
		return err
	}}
	t.Cleanup(func() { dialer.assertNoHandlerError(t) })
	return NewClient("/tmp/herdr.sock", dialer, time.Second)
}

func businessRequestCheck(method string, params map[string]any) func(map[string]any) error {
	return func(request map[string]any) error {
		if request["method"] != method {
			return fmt.Errorf("method = %q，期望 %q", request["method"], method)
		}
		actual, ok := request["params"].(map[string]any)
		if !ok {
			return fmt.Errorf("params 类型 = %T，期望 object", request["params"])
		}
		if !reflect.DeepEqual(actual, params) {
			return fmt.Errorf("params = %#v，期望 %#v", actual, params)
		}
		return nil
	}
}

func snapshotResultJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	snapshot := validSnapshot(t)
	if mutate != nil {
		mutate(snapshot)
	}
	return `{"type":"session_snapshot","snapshot":` + mustJSON(t, snapshot) + `}`
}

func validSnapshot(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"version":    "0.17.0",
		"protocol":   17,
		"workspaces": []any{validWorkspace()},
		"tabs":       []any{validTab()},
		"panes":      []any{validPane()},
		"layouts":    []any{},
		"agents":     []any{validAgentInfo(t)},
	}
}

func validWorkspace() map[string]any {
	return map[string]any{
		"workspace_id":  "w1",
		"number":        1,
		"label":         "工作区",
		"focused":       false,
		"pane_count":    1,
		"tab_count":     1,
		"active_tab_id": "t1",
		"agent_status":  "working",
	}
}

func firstSnapshotObject(snapshot map[string]any, key string) map[string]any {
	return snapshot[key].([]any)[0].(map[string]any)
}

func validTab() map[string]any {
	return map[string]any{
		"tab_id":       "t1",
		"workspace_id": "w1",
		"number":       1,
		"label":        "标签页",
		"focused":      false,
		"pane_count":   1,
		"agent_status": "working",
	}
}

func validPane() map[string]any {
	return map[string]any{
		"pane_id":       "p1",
		"terminal_id":   "term1",
		"workspace_id":  "w1",
		"tab_id":        "t1",
		"focused":       false,
		"agent":         "codex",
		"title":         "实现",
		"display_agent": "Codex",
		"agent_status":  "working",
		"agent_session": validAgentSession(),
		"revision":      0,
	}
}

func validAgentInfo(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"terminal_id":             "term1",
		"name":                    "任务",
		"agent":                   "codex",
		"title":                   "实现",
		"terminal_title":          "term",
		"terminal_title_stripped": "term",
		"display_agent":           "Codex",
		"agent_status":            "working",
		"agent_session":           validAgentSession(),
		"workspace_id":            "w1",
		"tab_id":                  "t1",
		"pane_id":                 "p1",
		"focused":                 false,
		"revision":                0,
		"state_change_seq":        1,
	}
}

func validAgentInfoJSON(t *testing.T) string {
	t.Helper()
	return mustJSON(t, validAgentInfo(t))
}

func validAgentSession() map[string]any {
	return map[string]any{
		"source": "codex",
		"agent":  "codex",
		"kind":   "id",
		"value":  "session-1",
	}
}

func validReadResult(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"pane_id":      "p1",
		"workspace_id": "w1",
		"tab_id":       "t1",
		"source":       "recent_unwrapped",
		"format":       "text",
		"text":         "",
		"revision":     0,
		"truncated":    false,
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码测试 fixture 失败：%v", err)
	}
	return string(encoded)
}
