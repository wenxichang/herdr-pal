package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	// muxTestTimeout 是等待 Mux 状态变化或退出的上限，避免测试无限阻塞。
	muxTestTimeout = 3 * time.Second
	// muxTestPollInterval 是轮询 Mux 快照的间隔。
	muxTestPollInterval = 5 * time.Millisecond
)

// muxTestHost 提供 fake /api/remote.mux；每条接受的连接交给脚本驱动。
//
// 脚本额外可用 stop 感知测试结束，避免 handler 阻塞 httptest.Server.Close。
type muxTestHost struct {
	server  *httptest.Server
	host    Host
	cookies chan string
	stop    chan struct{}
	once    sync.Once
}

func newMuxTestHost(t *testing.T, handle func(conn *websocket.Conn, stop <-chan struct{})) *muxTestHost {
	t.Helper()
	fake := &muxTestHost{
		cookies: make(chan string, 16),
		stop:    make(chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/remote.mux" {
			http.NotFound(w, r)
			return
		}
		select {
		case fake.cookies <- r.Header.Get("Cookie"):
		default:
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handle(conn, fake.stop)
	}))
	t.Cleanup(func() {
		fake.once.Do(func() { close(fake.stop) })
		fake.server.Close()
	})

	_, portText, err := net.SplitHostPort(strings.TrimPrefix(fake.server.URL, "http://"))
	if err != nil {
		t.Fatalf("解析测试服务器地址 %q: %v", fake.server.URL, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("测试服务器端口无效: %q", portText)
	}

	home := t.TempDir()
	credentialsPath := filepath.Join(home, ".credentials.yaml")
	_, encoded := testZeroSecret()
	if err := os.WriteFile(credentialsPath, []byte(testCredentialYAML(encoded)), 0o600); err != nil {
		t.Fatalf("写入测试认证文件: %v", err)
	}
	if err := os.Chmod(credentialsPath, 0o600); err != nil {
		t.Fatalf("设置测试认证文件权限: %v", err)
	}

	fake.host = Host{
		Port:    uint16(port),
		Version: "explicit",
		Variant: variantWeb,
		Home:    home,
	}
	return fake
}

// newTestMux 构造注入固定时钟的 Mux，便于断言握手 Cookie。
func newTestMux(t *testing.T, fake *muxTestHost) *Mux {
	t.Helper()
	mux := newMux(fake.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if mux == nil {
		t.Fatal("newMux 返回 nil")
	}
	mux.auth.now = func() time.Time { return time.UnixMilli(testCookieNowMs) }
	return mux
}

// muxRun 管理一个后台 Mux.Run，并保证测试结束时退出。
type muxRun struct {
	mux    *Mux
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startTestMux(t *testing.T, mux *Mux) *muxRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	run := &muxRun{mux: mux, cancel: cancel, done: make(chan struct{})}
	go func() {
		run.err = mux.Run(ctx)
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(muxTestTimeout):
			t.Error("Mux.Run 未在超时内退出")
		}
	})
	return run
}

// wait 等待 Run 返回并给出错误。
func (run *muxRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-run.done:
		return run.err
	case <-time.After(muxTestTimeout):
		t.Fatal("Mux.Run 未在超时内返回")
		return nil
	}
}

// stillRunning 断言 Run 尚未退出。
func (run *muxRun) stillRunning(t *testing.T) {
	t.Helper()
	select {
	case <-run.done:
		t.Fatalf("Mux.Run 不应退出，实际错误 %v", run.err)
	default:
	}
}

// waitForMux 轮询快照直到条件成立，超时即失败。
func waitForMux(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(muxTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(muxTestPollInterval)
	}
	t.Fatalf("等待 %s 超时", what)
}

func readMuxOpen(conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), muxTestTimeout)
	defer cancel()
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("读取 open 帧失败: %w", err)
	}
	if messageType != websocket.MessageText {
		return fmt.Errorf("open 帧类型 = %v，期望文本帧", messageType)
	}
	const want = `{"type":"open","streamId":"events","endpoint":"$events","payload":{"args":{}}}`
	if !jsonEquivalent(data, []byte(want)) {
		return fmt.Errorf("open 帧 = %s，期望 %s", data, want)
	}
	return nil
}

func writeMuxRaw(conn *websocket.Conn, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), muxTestTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

func writeMuxFrame(conn *websocket.Conn, frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("编码 Mux 帧失败: %w", err)
	}
	return writeMuxRaw(conn, data)
}

func muxItem(value any) map[string]any {
	return map[string]any{"streamId": "events", "type": "item", "value": value}
}

func muxReady(clientID string) map[string]any {
	return muxItem(map[string]any{"type": "ready", "clientId": clientID})
}

func muxWaterfall(eventID, agentID string, request any) map[string]any {
	return muxItem(map[string]any{
		"type":    "waterfall",
		"eventId": eventID,
		"agentId": agentID,
		"event":   "approval/request",
		"request": request,
	})
}

func muxCancel(eventID string) map[string]any {
	return muxItem(map[string]any{"type": "cancel", "eventId": eventID})
}

// jsonEquivalent 判断两段 JSON 语义是否一致，忽略对象字段顺序。
func jsonEquivalent(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码测试 JSON: %v", err)
	}
	return data
}

func TestMuxTracksReadyWaterfallAndCancel(t *testing.T) {
	request := map[string]any{"kind": "approval", "reason": "写入配置文件"}
	cancelRequested := make(chan struct{})
	readyAgain := make(chan struct{})

	fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
		if err := readMuxOpen(conn); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxWaterfall("event-1", "agent-1", request)); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxWaterfall("event-2", "agent-2", map[string]any{"kind": "question"})); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-cancelRequested:
			if err := writeMuxFrame(conn, muxCancel("event-1")); err != nil {
				t.Error(err)
				return
			}
		case <-stop:
			return
		}
		select {
		case <-readyAgain:
			if err := writeMuxFrame(conn, muxReady("client-two")); err != nil {
				t.Error(err)
				return
			}
		case <-stop:
			return
		}
		<-stop
	})

	mux := newTestMux(t, fake)
	run := startTestMux(t, mux)

	select {
	case cookie := <-fake.cookies:
		wantCookie, err := cookieForCredentials(
			filepath.Join(fake.host.Home, ".credentials.yaml"),
			fmt.Sprintf("127.0.0.1:%d", fake.host.Port),
			time.UnixMilli(testCookieNowMs),
		)
		if err != nil {
			t.Fatalf("生成期望 Cookie: %v", err)
		}
		if cookie != wantCookie {
			t.Error("Mux 握手 Cookie 与认证记录生成的期望值不一致")
		}
	case <-time.After(muxTestTimeout):
		t.Fatal("未收到 Mux 握手请求")
	}

	waitForMux(t, "ready 与 waterfall 入表", func() bool {
		snapshot := mux.Snapshot()
		return snapshot.ClientID == "client-one" && len(snapshot.Items) == 2
	})

	snapshot := mux.Snapshot()
	first := snapshot.Items[0]
	if first.EventID != "event-1" || first.AgentID != "agent-1" || first.Event != "approval/request" {
		t.Errorf("待答项 = %+v，期望 event-1/agent-1/approval/request", first)
	}
	if !jsonEquivalent(first.Request, mustMarshal(t, request)) {
		t.Errorf("待答项 request = %s，期望 %s", first.Request, mustMarshal(t, request))
	}
	if !json.Valid(first.Raw) || !strings.Contains(string(first.Raw), `"waterfall"`) {
		t.Errorf("待答项 Raw 应保留完整 waterfall 帧，实际 %s", first.Raw)
	}

	close(cancelRequested)
	waitForMux(t, "cancel 删除待答项", func() bool {
		snapshot := mux.Snapshot()
		return len(snapshot.Items) == 1 && snapshot.Items[0].EventID == "event-2"
	})

	close(readyAgain)
	waitForMux(t, "ready 清空待答表", func() bool {
		snapshot := mux.Snapshot()
		return snapshot.ClientID == "client-two" && len(snapshot.Items) == 0
	})

	run.cancel()
	if err := run.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mux.Run 退出错误 = %v，期望 context.Canceled", err)
	}
}

func TestMuxReconnectReplacesClientIDAndPending(t *testing.T) {
	var attempts atomic.Int32
	firstSent := make(chan struct{})
	closeFirst := make(chan struct{})

	fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
		if err := readMuxOpen(conn); err != nil {
			t.Error(err)
			return
		}
		if attempts.Add(1) == 1 {
			if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxFrame(conn, muxWaterfall("event-1", "agent-1", map[string]any{"kind": "approval"})); err != nil {
				t.Error(err)
				return
			}
			close(firstSent)
			select {
			case <-closeFirst:
				_ = conn.CloseNow()
			case <-stop:
			}
			return
		}
		if err := writeMuxFrame(conn, muxReady("client-two")); err != nil {
			t.Error(err)
			return
		}
		<-stop
	})

	mux := newTestMux(t, fake)
	// 跳过真实退避等待，同时仍响应 context 取消。
	mux.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	run := startTestMux(t, mux)

	<-firstSent
	waitForMux(t, "首个连接的待答项", func() bool {
		snapshot := mux.Snapshot()
		return snapshot.ClientID == "client-one" && len(snapshot.Items) == 1
	})

	close(closeFirst)
	waitForMux(t, "重连后的新 clientId", func() bool {
		return mux.Snapshot().ClientID == "client-two"
	})
	if got := attempts.Load(); got < 2 {
		t.Fatalf("连接次数 = %d，期望至少 2 次", got)
	}
	if snapshot := mux.Snapshot(); len(snapshot.Items) != 0 {
		t.Errorf("重连后待答表应清空，实际 %d 项", len(snapshot.Items))
	}
	run.stillRunning(t)
}

func TestMuxBoundsPendingAndRejectsUnknownFrames(t *testing.T) {
	t.Run("超过256条", func(t *testing.T) {
		allSent := make(chan struct{})
		sendExtra := make(chan struct{})
		fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
			if err := readMuxOpen(conn); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
				t.Error(err)
				return
			}
			for index := 0; index < 256; index++ {
				if err := writeMuxFrame(conn, muxWaterfall(fmt.Sprintf("event-%03d", index), "agent-1", map[string]any{"kind": "approval"})); err != nil {
					t.Error(err)
					return
				}
			}
			close(allSent)
			select {
			case <-sendExtra:
				if err := writeMuxFrame(conn, muxWaterfall("event-extra", "agent-1", map[string]any{"kind": "approval"})); err != nil {
					t.Error(err)
				}
			case <-stop:
			}
			<-stop
		})
		mux := newTestMux(t, fake)
		run := startTestMux(t, mux)

		<-allSent
		waitForMux(t, "256 条待答项", func() bool {
			return len(mux.Snapshot().Items) == 256
		})
		close(sendExtra)
		if err := run.wait(t); !errors.Is(err, errMuxCapacity) {
			t.Fatalf("超过 256 条应返回 errMuxCapacity，实际 %v", err)
		}
	})

	t.Run("单条超过128KiB", func(t *testing.T) {
		fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
			if err := readMuxOpen(conn); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
				t.Error(err)
				return
			}
			oversized := map[string]any{"kind": "approval", "padding": strings.Repeat("x", 129<<10)}
			if err := writeMuxFrame(conn, muxWaterfall("event-big", "agent-1", oversized)); err != nil {
				t.Error(err)
			}
			<-stop
		})
		mux := newTestMux(t, fake)
		run := startTestMux(t, mux)
		if err := run.wait(t); !errors.Is(err, errMuxItemTooLarge) {
			t.Fatalf("单条超过 128 KiB 应返回 errMuxItemTooLarge，实际 %v", err)
		}
	})

	t.Run("总量超过1MiB", func(t *testing.T) {
		fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
			if err := readMuxOpen(conn); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
				t.Error(err)
				return
			}
			request := map[string]any{"kind": "approval", "padding": strings.Repeat("x", 120<<10)}
			for index := 0; index < 9; index++ {
				if err := writeMuxFrame(conn, muxWaterfall(fmt.Sprintf("event-%d", index), "agent-1", request)); err != nil {
					t.Error(err)
					return
				}
			}
			<-stop
		})
		mux := newTestMux(t, fake)
		run := startTestMux(t, mux)
		if err := run.wait(t); !errors.Is(err, errMuxTotalTooLarge) {
			t.Fatalf("总量超过 1 MiB 应返回 errMuxTotalTooLarge，实际 %v", err)
		}
	})

	cases := []struct {
		name  string
		frame []byte
	}{
		{"未知顶层帧类型", []byte(`{"streamId":"events","type":"mystery","value":{}}`)},
		{"未知事件类型", []byte(`{"streamId":"events","type":"item","value":{"type":"mystery"}}`)},
		{"畸形JSON", []byte(`not-json`)},
		{"缺少value", []byte(`{"streamId":"events","type":"item"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
				if err := readMuxOpen(conn); err != nil {
					t.Error(err)
					return
				}
				if err := writeMuxRaw(conn, tc.frame); err != nil {
					t.Error(err)
				}
				<-stop
			})
			mux := newTestMux(t, fake)
			run := startTestMux(t, mux)
			if err := run.wait(t); !errors.Is(err, errMuxProtocol) {
				t.Fatalf("未知帧应返回 errMuxProtocol，实际 %v", err)
			}
		})
	}

	t.Run("忽略其他streamId", func(t *testing.T) {
		fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
			if err := readMuxOpen(conn); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxRaw(conn, []byte(`{"streamId":"other","type":"mystery","value":{"type":"mystery"}}`)); err != nil {
				t.Error(err)
				return
			}
			if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
				t.Error(err)
			}
			<-stop
		})
		mux := newTestMux(t, fake)
		run := startTestMux(t, mux)
		waitForMux(t, "ready", func() bool { return mux.Snapshot().ClientID == "client-one" })
		run.stillRunning(t)
	})
}

func TestMuxWaitCancel(t *testing.T) {
	cancelFirst := make(chan struct{})
	addSecond := make(chan struct{})
	fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
		if err := readMuxOpen(conn); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxWaterfall("event-1", "agent-1", map[string]any{"kind": "approval"})); err != nil {
			t.Error(err)
			return
		}
		select {
		case <-cancelFirst:
			if err := writeMuxFrame(conn, muxCancel("event-1")); err != nil {
				t.Error(err)
				return
			}
		case <-stop:
			return
		}
		select {
		case <-addSecond:
			if err := writeMuxFrame(conn, muxWaterfall("event-2", "agent-1", map[string]any{"kind": "question"})); err != nil {
				t.Error(err)
			}
		case <-stop:
			return
		}
		<-stop
	})

	mux := newTestMux(t, fake)
	mux.cancelWait = 100 * time.Millisecond
	run := startTestMux(t, mux)

	waitForMux(t, "event-1", func() bool { return len(mux.Snapshot().Items) == 1 })

	// 先启动等待，再让服务端发送 cancel，验证等待被广播唤醒。
	waited := make(chan bool, 1)
	go func() { waited <- mux.WaitCancel(context.Background(), "event-1") }()
	time.Sleep(20 * time.Millisecond)
	close(cancelFirst)
	select {
	case canceled := <-waited:
		if !canceled {
			t.Fatal("观察到 cancel 后 WaitCancel 应返回 true")
		}
	case <-time.After(muxTestTimeout):
		t.Fatal("WaitCancel 未被 cancel 唤醒")
	}

	if !mux.WaitCancel(context.Background(), "event-1") {
		t.Fatal("已记录的 cancel 应让后续 WaitCancel 立即返回 true")
	}
	if mux.WaitCancel(context.Background(), "unknown-event") {
		t.Fatal("未知事件不应被判定为已取消")
	}

	close(addSecond)
	waitForMux(t, "event-2", func() bool {
		return len(mux.Snapshot().Items) == 1 && mux.Snapshot().Items[0].EventID == "event-2"
	})
	started := time.Now()
	if mux.WaitCancel(context.Background(), "event-2") {
		t.Fatal("未收到 cancel 时 WaitCancel 应返回 false")
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Errorf("WaitCancel 等待时间 = %v，期望至少接近 cancelWait", elapsed)
	}

	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitCancel()
	if mux.WaitCancel(waitCtx, "event-2") {
		t.Fatal("context 已取消时 WaitCancel 应返回 false")
	}
	run.stillRunning(t)
}

func TestSubmitEventAnswerUsesCorrelatedRPC(t *testing.T) {
	type captured struct {
		method string
		path   string
		args   json.RawMessage
	}
	capturedCh := make(chan captured, 1)
	var hits atomic.Int32
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体: %v", err)
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		var request struct {
			RPCID   string `json:"rpcId"`
			Method  string `json:"method"`
			Payload struct {
				Args json.RawMessage `json:"args"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("解析请求帧: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		select {
		case capturedCh <- captured{method: request.Method, path: r.URL.Path, args: request.Payload.Args}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type":   "server-response",
			"rpcId":  request.RPCID,
			"result": map[string]any{"ok": true, "value": map[string]any{"accepted": true}},
		})
	})

	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client

	value := map[string]any{"answers": []any{map[string]any{"id": "q1", "selected": []any{"allowed-once"}}}}
	if err := client.SubmitEventAnswer(context.Background(), "client-one", "event-1", value); err != nil {
		t.Fatalf("SubmitEventAnswer: %v", err)
	}

	got := <-capturedCh
	if got.method != "$events/result" {
		t.Errorf("RPC method = %q，期望 $events/result", got.method)
	}
	if got.path != "/api/$events/result" {
		t.Errorf("RPC 路径 = %q，期望 /api/$events/result", got.path)
	}
	wantArgs := map[string]any{
		"clientId": "client-one",
		"eventId":  "event-1",
		"outcome": map[string]any{
			"kind":  "result",
			"value": value,
		},
	}
	var args map[string]any
	if err := json.Unmarshal(got.args, &args); err != nil {
		t.Fatalf("解析 payload.args: %v", err)
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("payload.args = %s，期望 %s", got.args, mustMarshal(t, wantArgs))
	}

	if err := client.SubmitEventAnswer(context.Background(), "", "event-1", value); err == nil {
		t.Fatal("空 clientId 应在发出请求前被拒绝")
	}
	if err := client.SubmitEventAnswer(context.Background(), "client-one", "", value); err == nil {
		t.Fatal("空 eventId 应在发出请求前被拒绝")
	}
	if hits.Load() != 1 {
		t.Fatalf("无效参数不应发出 HTTP 请求，实际 %d 次", hits.Load())
	}
}

func TestSubmitEventAnswerRequiresOK(t *testing.T) {
	fixture := newRPCTestFixture(t, func(w http.ResponseWriter, r *http.Request) {
		rpcID := readRPCID(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"type":"server-response","rpcId":%q,"result":{"ok":false,"error":{"code":"event-not-found","message":"expired"}}}`,
			rpcID,
		)
	})
	client := NewClient(fixture.client.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.rpc = fixture.client

	err := client.SubmitEventAnswer(context.Background(), "client-one", "event-1", "allowed-once")
	if err == nil {
		t.Fatal("ok=false 应返回错误")
	}
	var rpcErr *rpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != "event-not-found" {
		t.Fatalf("错误 = %v，期望包含事件不存在错误码", err)
	}
}

func TestClientPendingAndRunWireMux(t *testing.T) {
	fake := newMuxTestHost(t, func(conn *websocket.Conn, stop <-chan struct{}) {
		if err := readMuxOpen(conn); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxReady("client-one")); err != nil {
			t.Error(err)
			return
		}
		if err := writeMuxFrame(conn, muxWaterfall("event-1", "agent-1", map[string]any{"kind": "approval"})); err != nil {
			t.Error(err)
			return
		}
		<-stop
	})

	client := NewClient(fake.host, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if client.mux == nil {
		t.Fatal("NewClient 应创建 Mux")
	}
	if _, err := client.Pending(context.Background()); !errors.Is(err, errMuxUnavailable) {
		t.Fatalf("Mux 未就绪时 Pending 应返回 errMuxUnavailable，实际 %v", err)
	}
	if err := (&Client{}).Run(context.Background()); !errors.Is(err, errMuxUnavailable) {
		t.Fatalf("缺少 Mux 的 Client.Run 应返回 errMuxUnavailable，实际 %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()

	waitForMux(t, "Client.Pending 返回快照", func() bool {
		snapshot, err := client.Pending(context.Background())
		return err == nil && snapshot.ClientID == "client-one" && len(snapshot.Items) == 1
	})
	snapshot, err := client.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if snapshot.Items[0].EventID != "event-1" || snapshot.Items[0].AgentID != "agent-1" {
		t.Errorf("Pending 快照 = %+v，期望 event-1/agent-1", snapshot)
	}

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Client.Run 退出错误 = %v，期望 context.Canceled", err)
		}
	case <-time.After(muxTestTimeout):
		t.Fatal("Client.Run 未在超时内退出")
	}

	if _, err := client.Pending(context.Background()); !errors.Is(err, errMuxUnavailable) {
		t.Fatalf("Mux 退出后 Pending 应返回 errMuxUnavailable，实际 %v", err)
	}
}
