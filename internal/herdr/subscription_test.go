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
	"sync"
	"testing"
	"time"
)

func TestLifecycleSubscriptionsReturnsStableIndependentSlice(t *testing.T) {
	want := []SubscriptionSpec{
		{Type: "pane.created"},
		{Type: "pane.closed"},
		{Type: "pane.updated"},
		{Type: "pane.exited"},
		{Type: "pane.agent_detected"},
	}
	got := LifecycleSubscriptions()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LifecycleSubscriptions() = %#v，期望 %#v", got, want)
	}
	got[0].Type = "mutated"
	if again := LifecycleSubscriptions(); !reflect.DeepEqual(again, want) {
		t.Fatalf("LifecycleSubscriptions() 返回共享底层状态：%#v", again)
	}
}

func TestStatusSubscriptionsSortsDeduplicatesAndPreservesPaneID(t *testing.T) {
	input := []string{" p2 ", "p1", "", "  ", "p2", "p3"}
	// 字典序以原值比较，空白值仅用于排除。
	want := []SubscriptionSpec{
		{Type: "pane.agent_status_changed", PaneID: " p2 "},
		{Type: "pane.agent_status_changed", PaneID: "p1"},
		{Type: "pane.agent_status_changed", PaneID: "p2"},
		{Type: "pane.agent_status_changed", PaneID: "p3"},
	}
	got := StatusSubscriptions(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusSubscriptions() = %#v，期望 %#v", got, want)
	}
	if input[0] != " p2 " {
		t.Fatalf("StatusSubscriptions() 修改了输入：%#v", input)
	}
	got[0].PaneID = "mutated"
	if again := StatusSubscriptions(input); !reflect.DeepEqual(again, want) {
		t.Fatalf("StatusSubscriptions() 返回共享底层状态：%#v", again)
	}
}

func TestSubscriptionSpecJSON(t *testing.T) {
	encoded, err := json.Marshal(SubscriptionSpec{Type: "pane.agent_status_changed", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Marshal() 返回错误：%v", err)
	}
	if got, want := string(encoded), `{"type":"pane.agent_status_changed","pane_id":"w1:p1"}`; got != want {
		t.Fatalf("JSON = %s，期望 %s", got, want)
	}
	encoded, err = json.Marshal(SubscriptionSpec{Type: "pane.created"})
	if err != nil {
		t.Fatalf("Marshal() 返回错误：%v", err)
	}
	if got, want := string(encoded), `{"type":"pane.created"}`; got != want {
		t.Fatalf("JSON = %s，期望 %s", got, want)
	}
}

func TestClientSubscribeSendsRequestAndReceivesEvents(t *testing.T) {
	dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
		if request["method"] != "events.subscribe" {
			return errors.New("method 错误")
		}
		params, ok := request["params"].(map[string]any)
		if !ok {
			return errors.New("params 错误")
		}
		subscriptions, ok := params["subscriptions"].([]any)
		if !ok || len(subscriptions) != 1 {
			return errors.New("subscriptions 错误")
		}
		id, _ := request["id"].(string)
		_, err := io.WriteString(conn, `{"id":"`+id+`","result":{"type":"subscription_started"}}`+"\n"+
			`{"event":"pane_created","data":{"type":"pane","pane":{"pane_id":"p1"}}}`+"\n"+
			`{"event":"pane.agent_status_changed","data":{"pane_id":"p1","workspace_id":"w1","agent_status":"working"}}`+"\n")
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, conn)
		return err
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	stream, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
	if err != nil {
		t.Fatalf("Subscribe() 返回错误：%v", err)
	}

	first, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("第一次 Recv() 返回错误：%v", err)
	}
	if first.Kind != "pane_created" || !json.Valid(first.Data) {
		t.Fatalf("第一次事件 = %#v", first)
	}
	second, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("第二次 Recv() 返回错误：%v", err)
	}
	decoded, err := DecodeAgentStatusEvent(second)
	if err != nil {
		t.Fatalf("DecodeAgentStatusEvent() 返回错误：%v", err)
	}
	if decoded.PaneID != "p1" || decoded.WorkspaceID != "w1" || decoded.AgentStatus != AgentStatusWorking || len(decoded.StateLabels) != 0 {
		t.Fatalf("状态事件 = %#v", decoded)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close() 返回错误：%v", err)
	}
	dialer.assertNoHandlerError(t)
}

func TestClientSubscribeRejectsInvalidAcknowledgement(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		match error
	}{
		{name: "错误响应", line: `{"id":"%s","error":{"code":"denied","message":"no"}}`, match: &APIError{}},
		{name: "错误类型", line: `{"id":"%s","result":{"type":"ok"}}`, match: ErrProtocol},
		{name: "缺少类型", line: `{"id":"%s","result":{}}`, match: ErrProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
				id, _ := request["id"].(string)
				_, err := io.WriteString(conn, sprintf(test.line, id)+"\n")
				return err
			}}
			client := NewClient("/tmp/herdr.sock", dialer, time.Second)
			_, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
			if test.name == "错误响应" {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Code != "denied" {
					t.Fatalf("Subscribe() 错误 = %v，期望 APIError", err)
				}
			} else if !errors.Is(err, test.match) {
				t.Fatalf("Subscribe() 错误 = %v，期望匹配 %v", err, test.match)
			}
		})
	}
}

func TestClientSubscribeClassifiesCloseBeforeAcknowledgementAsUnavailable(t *testing.T) {
	dialer := &subscriptionDialer{handler: func(net.Conn, map[string]any) error {
		return nil
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	_, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, io.EOF) {
		t.Fatalf("Subscribe() 错误 = %v，期望同时匹配 ErrUnavailable 和 io.EOF", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("Subscribe() 错误 = %v，不应匹配 ErrProtocol", err)
	}
}

func TestClientSubscribeRejectsInvalidSpecBeforeDialing(t *testing.T) {
	tests := []struct {
		name  string
		specs []SubscriptionSpec
	}{
		{name: "空列表"},
		{name: "空类型", specs: []SubscriptionSpec{{}}},
		{name: "状态事件缺 pane", specs: []SubscriptionSpec{{Type: "pane.agent_status_changed"}}},
		{name: "生命周期事件含 pane", specs: []SubscriptionSpec{{Type: "pane.created", PaneID: "p1"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialer := &subscriptionDialer{}
			client := NewClient("/tmp/herdr.sock", dialer, time.Second)
			_, err := client.Subscribe(context.Background(), test.specs)
			if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("Subscribe() 错误 = %v，期望仅匹配 ErrProtocol", err)
			}
			if dialer.Count() != 0 {
				t.Fatalf("DialContext() 调用次数 = %d，期望 0", dialer.Count())
			}
		})
	}
}

func TestClientSubscribeClearsHandshakeDeadline(t *testing.T) {
	dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		if _, err := io.WriteString(conn, `{"id":"`+id+`","result":{"type":"subscription_started"}}`+"\n"); err != nil {
			return err
		}
		time.Sleep(150 * time.Millisecond)
		_, err := io.WriteString(conn, `{"event":"pane_created","data":{}}`+"\n")
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, conn)
		return err
	}}
	client := NewClient("/tmp/herdr.sock", dialer, 100*time.Millisecond)
	stream, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
	if err != nil {
		t.Fatalf("Subscribe() 返回错误：%v", err)
	}
	defer stream.Close()
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv() 在握手超时后返回错误：%v", err)
	}
	if event.Kind != "pane_created" {
		t.Fatalf("事件 Kind = %q，期望 pane_created", event.Kind)
	}
}

func TestSubscriptionStreamRejectsMalformedEvent(t *testing.T) {
	for _, line := range []string{
		`{}`,
		`{"event":"pane_created"}`,
		`{"event":"pane_created","data":null}`,
		`{"event":"pane_created","data":[]}`,
		`not-json`,
		``,
	} {
		t.Run(line, func(t *testing.T) {
			dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
				id, _ := request["id"].(string)
				if _, err := io.WriteString(conn, `{"id":"`+id+`","result":{"type":"subscription_started"}}`+"\n"); err != nil {
					return err
				}
				if _, err := io.WriteString(conn, line+"\n"); err != nil {
					return err
				}
				_, err := io.Copy(io.Discard, conn)
				return err
			}}
			client := NewClient("/tmp/herdr.sock", dialer, time.Second)
			stream, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
			if err != nil {
				t.Fatalf("Subscribe() 返回错误：%v", err)
			}
			defer stream.Close()
			if _, err := stream.Recv(context.Background()); !errors.Is(err, ErrProtocol) {
				t.Fatalf("Recv() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestSubscriptionStreamCloseIsIdempotent(t *testing.T) {
	dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		if _, err := io.WriteString(conn, `{"id":"`+id+`","result":{"type":"subscription_started"}}`+"\n"); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, conn)
		return err
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	stream, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
	if err != nil {
		t.Fatalf("Subscribe() 返回错误：%v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("第一次 Close() 返回错误：%v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("第二次 Close() 返回错误：%v", err)
	}
}

func TestDecodeAgentStatusEventValidatesRequiredFields(t *testing.T) {
	valid := Event{Kind: "pane.agent_status_changed", Data: json.RawMessage(`{"pane_id":"p1","workspace_id":"w1","agent_status":"blocked","agent":"claude","title":"确认","display_agent":"Claude","state_labels":{"permission":"required"},"future":true}`)}
	got, err := DecodeAgentStatusEvent(valid)
	if err != nil {
		t.Fatalf("DecodeAgentStatusEvent() 返回错误：%v", err)
	}
	if got.Agent == nil || *got.Agent != "claude" || got.StateLabels["permission"] != "required" {
		t.Fatalf("DecodeAgentStatusEvent() = %#v", got)
	}
	for _, event := range []Event{
		{Kind: "pane.created", Data: valid.Data},
		{Kind: valid.Kind, Data: json.RawMessage(`{"pane_id":"p1","workspace_id":"w1","agent_status":"running"}`)},
		{Kind: valid.Kind, Data: json.RawMessage(`{"pane_id":"","workspace_id":"w1","agent_status":"idle"}`)},
		{Kind: valid.Kind, Data: json.RawMessage(`{"pane_id":"p1","workspace_id":"w1","agent_status":"idle","state_labels":{"n":1}}`)},
	} {
		if _, err := DecodeAgentStatusEvent(event); !errors.Is(err, ErrProtocol) {
			t.Fatalf("DecodeAgentStatusEvent(%s) 错误 = %v，期望 ErrProtocol", event.Data, err)
		}
	}
}

func TestSubscriptionStreamRecvsContextCancellationAndClose(t *testing.T) {
	started := make(chan struct{})
	dialer := &subscriptionDialer{handler: func(conn net.Conn, request map[string]any) error {
		id, _ := request["id"].(string)
		if _, err := io.WriteString(conn, `{"id":"`+id+`","result":{"type":"subscription_started"}}`+"\n"); err != nil {
			return err
		}
		close(started)
		_, err := io.Copy(io.Discard, conn)
		return err
	}}
	client := NewClient("/tmp/herdr.sock", dialer, time.Second)
	stream, err := client.Subscribe(context.Background(), []SubscriptionSpec{{Type: "pane.created"}})
	if err != nil {
		t.Fatalf("Subscribe() 返回错误：%v", err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { _, receiveErr := stream.Recv(ctx); errCh <- receiveErr }()
	cancel()
	select {
	case receiveErr := <-errCh:
		if !errors.Is(receiveErr, ErrUnavailable) || !errors.Is(receiveErr, context.Canceled) {
			t.Fatalf("Recv() 错误 = %v，期望匹配 ErrUnavailable 和 context.Canceled", receiveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Recv() 没有在取消后退出")
	}
	if err := stream.Close(); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Close() 返回错误：%v", err)
	}
}

type subscriptionDialer struct {
	handler func(net.Conn, map[string]any) error

	mu          sync.Mutex
	connections []net.Conn
	handlerErrs []error
	wg          sync.WaitGroup
}

func (d *subscriptionDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	d.mu.Lock()
	d.connections = append(d.connections, client)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer server.Close()
		line, err := bufio.NewReader(server).ReadBytes('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if err := json.Unmarshal(line, &request); err != nil {
			d.recordHandlerError(err)
			return
		}
		if d.handler != nil {
			d.recordHandlerError(d.handler(server, request))
		}
	}()
	return client, nil
}

func (d *subscriptionDialer) Count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.connections)
}

func (d *subscriptionDialer) recordHandlerError(err error) {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlerErrs = append(d.handlerErrs, err)
}

func (d *subscriptionDialer) assertNoHandlerError(t *testing.T) {
	t.Helper()
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.handlerErrs) > 0 {
		t.Fatalf("服务端处理失败：%v", d.handlerErrs[0])
	}
}

func sprintf(format string, values ...any) string {
	return fmt.Sprintf(format, values...)
}
