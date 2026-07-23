package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
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
