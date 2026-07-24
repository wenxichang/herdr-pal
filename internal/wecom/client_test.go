package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestClientSubscribesBeforeDeliveringCallbacksAndReplies(t *testing.T) {
	socket := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return socket, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)

	subscribe := socket.nextWrite(t)
	if commandOf(t, subscribe).Cmd != "aibot_subscribe" {
		t.Fatalf("first command = %q, want aibot_subscribe", commandOf(t, subscribe).Cmd)
	}
	socket.push(responseJSON(requestIDOf(t, subscribe), 0))
	socket.push([]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":"callback-1"},"body":{"msgid":"message-1","aibotid":"bot-1","chattype":"single","from":{"userid":"user-1"},"msgtype":"text","text":{"content":"/ls"}}}`))

	select {
	case event := <-client.Events():
		if event.RequestID != "callback-1" || event.Content != "/ls" {
			t.Fatalf("event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Events() did not receive callback")
	}

	replyDone := make(chan error, 1)
	go func() { replyDone <- client.RespondMarkdown(context.Background(), "callback-1", "收到") }()
	reply := socket.nextWrite(t)
	if commandOf(t, reply).Cmd != "aibot_respond_msg" || requestIDOf(t, reply) != "callback-1" {
		t.Fatalf("reply = %s, want callback request id", reply)
	}
	socket.push(responseJSON("callback-1", 0))
	if err := <-replyDone; err != nil {
		t.Fatalf("RespondMarkdown() error = %v", err)
	}

	cancel()
	awaitDone(t, done)
}

func TestClientBuffersCallbackUntilSubscribeResponse(t *testing.T) {
	socket := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return socket, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	subscribe := socket.nextWrite(t)
	socket.push(textCallbackJSON("callback-before-subscribe"))
	select {
	case event := <-client.Events():
		t.Fatalf("event delivered before subscribe confirmation: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
	socket.push(responseJSON(requestIDOf(t, subscribe), 0))
	select {
	case event := <-client.Events():
		if event.RequestID != "callback-before-subscribe" {
			t.Fatalf("event = %+v, want buffered callback", event)
		}
	case <-time.After(time.Second):
		t.Fatal("buffered callback was not delivered")
	}
	select {
	case event := <-client.Events():
		t.Fatalf("callback delivered more than once: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	awaitDone(t, done)
}

func TestClientPreservesPreReadyOverflowAfterSubscribeConfirmation(t *testing.T) {
	socket := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return socket, nil })
	client.events = make(chan IncomingText, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	subscribe := socket.nextWrite(t)
	socket.push(textCallbackJSON("before-1"))
	socket.push(textCallbackJSON("before-2"))
	if len(client.events) != 0 {
		t.Fatalf("event queue len = %d before confirmation, want zero", len(client.events))
	}
	socket.push(responseJSON(requestIDOf(t, subscribe), 0))
	for _, requestID := range []string{"before-1", "before-2"} {
		select {
		case event := <-client.Events():
			if event.RequestID != requestID {
				t.Fatalf("event = %q, want %q", event.RequestID, requestID)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing retained event %q", requestID)
		}
	}
	cancel()
	awaitDone(t, done)
}

func TestClientStopsPreReadyOverflowWithoutPublishingEvents(t *testing.T) {
	socket := newFakeSocket()
	attempts := 0
	client := newTestClient(t, func(context.Context, string) (Socket, error) { attempts++; return socket, nil })
	client.events = make(chan IncomingText, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := runClient(t, client, ctx)
	_ = socket.nextWrite(t)
	socket.push(textCallbackJSON("private-1"))
	socket.push(textCallbackJSON("private-2"))
	socket.push(textCallbackJSON("private-3"))
	err := <-done
	if !errors.Is(err, ErrEventQueueFull) || strings.Contains(err.Error(), "private") {
		t.Fatalf("Run() = %v, want safe ErrEventQueueFull", err)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
	if len(client.events) != 0 {
		t.Fatalf("Events published before subscribe confirmation")
	}
}

func TestClientWaitsBackoffAfterSubscribedSessionEnds(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	calls := 0
	client := newTestClient(t, func(context.Context, string) (Socket, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	client.backoff = NewBackoff(time.Second, 30*time.Second, func() float64 { return 0.5 })
	waits := make(chan time.Duration, 2)
	releaseWait := make(chan struct{})
	client.wait = func(_ context.Context, delay time.Duration) error { waits <- delay; <-releaseWait; return nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, first)
	first.push([]byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"event-1"},"body":{"msgid":"event-message-1","create_time":1720000000,"aibotid":"bot-1","msgtype":"event","event":{"eventtype":"disconnected_event"}}}`))
	select {
	case got := <-waits:
		if got != time.Second {
			t.Fatalf("backoff wait = %s, want 1s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscribed session end did not wait backoff")
	}
	select {
	case write := <-second.writes:
		t.Fatalf("redial happened before backoff was released: %s", write)
	default:
	}
	close(releaseWait)
	if commandOf(t, second.nextWrite(t)).Cmd != "aibot_subscribe" {
		t.Fatal("did not reconnect after wait")
	}
	cancel()
	awaitDone(t, done)
}

func TestClientSendMarkdownToTargetsRequestedUserAndWaitsResponse(t *testing.T) {
	socket := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return socket, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, socket)

	result := make(chan error, 1)
	go func() { result <- client.SendMarkdownTo(context.Background(), "user-2", "主动通知") }()
	write := socket.nextWrite(t)
	var request struct {
		Cmd     string  `json:"cmd"`
		Headers Headers `json:"headers"`
		Body    struct {
			ChatID   string `json:"chatid"`
			ChatType int    `json:"chat_type"`
		} `json:"body"`
	}
	if err := json.Unmarshal(write, &request); err != nil {
		t.Fatal(err)
	}
	if request.Cmd != "aibot_send_msg" || request.Headers.RequestID == "" || request.Body.ChatID != "user-2" || request.Body.ChatType != 1 {
		t.Fatalf("send request = %s", write)
	}
	socket.push(responseJSON(request.Headers.RequestID, 0))
	if err := <-result; err != nil {
		t.Fatalf("SendMarkdownTo() error = %v", err)
	}
	cancel()
	awaitDone(t, done)
}

func TestClientHeartbeatTimeoutReconnects(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	var calls int
	client := newTestClient(t, func(context.Context, string) (Socket, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	client.heartbeatInterval = 10 * time.Millisecond
	client.requestTimeout = 15 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, first)
	ping := first.nextWrite(t)
	if commandOf(t, ping).Cmd != "ping" {
		t.Fatalf("command = %q, want ping", commandOf(t, ping).Cmd)
	}
	if commandOf(t, second.nextWrite(t)).Cmd != "aibot_subscribe" {
		t.Fatal("second session did not subscribe")
	}
	cancel()
	awaitDone(t, done)
}

func TestClientDisconnectAndFullEventQueueReconnect(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	var calls int
	client := newTestClient(t, func(context.Context, string) (Socket, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	})
	client.events = make(chan IncomingText, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, first)
	firstSession := client.getCurrent()
	first.push(textCallbackJSON("callback-1"))
	first.push(textCallbackJSON("callback-2"))
	select {
	case <-firstSession.done:
	case <-time.After(time.Second):
		t.Fatal("event overflow did not finish session")
	}
	select {
	case event := <-client.Events():
		if event.RequestID != "callback-1" {
			t.Fatalf("first event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("first queued event missing")
	}
	select {
	case event := <-client.Events():
		if event.RequestID != "callback-2" {
			t.Fatalf("overflow event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("overflow event missing")
	}
	if commandOf(t, second.nextWrite(t)).Cmd != "aibot_subscribe" {
		t.Fatal("full event queue did not reconnect")
	}
	cancel()
	awaitDone(t, done)
}

func TestClientDisconnectedAndBinaryFramesReconnect(t *testing.T) {
	for _, frame := range []fakeRead{
		{typ: websocket.MessageText, data: []byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"event-1"},"body":{"msgid":"event-message-1","create_time":1720000000,"aibotid":"bot-1","msgtype":"event","event":{"eventtype":"disconnected_event"}}}`)},
		{typ: websocket.MessageBinary, data: []byte("binary")},
	} {
		t.Run(fmt.Sprintf("type-%d", frame.typ), func(t *testing.T) {
			first := newFakeSocket()
			second := newFakeSocket()
			calls := 0
			client := newTestClient(t, func(context.Context, string) (Socket, error) {
				calls++
				if calls == 1 {
					return first, nil
				}
				return second, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := runClient(t, client, ctx)
			completeSubscribe(t, client, first)
			first.reads <- frame
			if commandOf(t, second.nextWrite(t)).Cmd != "aibot_subscribe" {
				t.Fatal("invalid session frame did not reconnect")
			}
			cancel()
			awaitDone(t, done)
		})
	}
}

func TestClientReplacingSessionCancelsOldPendingRequest(t *testing.T) {
	first := newFakeSocket()
	second := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return first, nil })
	session := newSession(context.Background(), first, client.events)
	client.install(session)
	result := make(chan error, 1)
	go func() { result <- client.SendMarkdown(context.Background(), "content") }()
	_ = first.nextWrite(t)
	next := newSession(context.Background(), second, client.events)
	client.install(next)
	if err := <-result; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("pending request = %v, want ErrUnavailable", err)
	}
	next.finish(ErrUnavailable)
}

func TestClientUnavailableTimeoutAndNoSecretLeak(t *testing.T) {
	secret := "super-secret"
	client, err := NewClient(ClientConfig{Endpoint: "ws://fake", BotID: "bot-1", Secret: secret, AllowedUserID: "user-1", Dial: func(context.Context, string) (Socket, error) { return nil, errors.New("dial failed") }, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendMarkdown(context.Background(), "content"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("SendMarkdown() = %v, want unavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if err != nil && contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestClientRejectsInvalidEndpointAndPermanentSubscribeError(t *testing.T) {
	for _, endpoint := range []string{"", "https://example.test", "ws:///missing-host"} {
		if _, err := NewClient(ClientConfig{Endpoint: endpoint, BotID: "bot", Secret: "secret", AllowedUserID: "user"}); !errors.Is(err, ErrProtocol) {
			t.Fatalf("NewClient(%q) error = %v, want ErrProtocol", endpoint, err)
		}
	}
	socket := newFakeSocket()
	attempts := 0
	client := newTestClient(t, func(context.Context, string) (Socket, error) { attempts++; return socket, nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := runClient(t, client, ctx)
	subscribe := socket.nextWrite(t)
	socket.push(responseJSON(requestIDOf(t, subscribe), 40001))
	if err := <-done; !errors.Is(err, ErrProtocol) {
		t.Fatalf("Run() = %v, want permanent ErrProtocol", err)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
}

func TestClientAllowsReceiveOnlyDiscoveryWithoutAllowedUser(t *testing.T) {
	socket := newFakeSocket()
	client, err := NewClient(ClientConfig{
		Endpoint: "ws://fake", BotID: "bot-1", Secret: "secret",
		Dial:      func(context.Context, string) (Socket, error) { return socket, nil },
		RequestID: sequentialIDs(), HeartbeatInterval: time.Hour, RequestTimeout: 100 * time.Millisecond,
		EventsCapacity: 4, Wait: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.SendMarkdown(context.Background(), "content"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("SendMarkdown() = %v, want protocol error for missing target", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, socket)
	socket.push(textCallbackJSON("discovery-callback"))
	select {
	case event := <-client.Events():
		if event.UserID != "user-1" {
			t.Fatalf("event user = %q", event.UserID)
		}
	case <-time.After(time.Second):
		t.Fatal("发现模式未收到回调")
	}
	cancel()
	awaitDone(t, done)
}

func TestClientLogsConnectionLifecycleAndUnsupportedCallbacksWithoutSensitiveValues(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	socket := newFakeSocket()
	ctx, cancel := context.WithCancel(context.Background())
	client, err := NewClient(ClientConfig{
		Endpoint: "ws://fake", BotID: "bot-sensitive", Secret: "secret-sensitive", AllowedUserID: "user-1",
		Dial: func(context.Context, string) (Socket, error) { return socket, nil }, Logger: logger,
		RequestID: sequentialIDs(), HeartbeatInterval: time.Hour, RequestTimeout: 100 * time.Millisecond,
		EventsCapacity: 4, Wait: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, socket)
	socket.push([]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":"unsupported-1"},"body":{"msgid":"message-sensitive","aibotid":"bot-sensitive","chattype":"single","from":{"userid":"raw-user-sensitive"},"msgtype":"image"}}`))
	waitForLog(t, &logs, "企业微信不支持的消息类型已忽略")
	socket.push([]byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"event-1"},"body":{"msgid":"event-message-1","create_time":1720000000,"aibotid":"bot-sensitive","msgtype":"event","event":{"eventtype":"disconnected_event"}}}`))
	awaitDone(t, done)

	output := logs.String()
	for _, want := range []string{"企业微信连接中", "企业微信订阅成功", "企业微信不支持的消息类型已忽略", "企业微信连接已断开"} {
		if !strings.Contains(output, want) {
			t.Fatalf("日志缺少 %q：%q", want, output)
		}
	}
	for _, forbidden := range []string{"secret-sensitive", "raw-user-sensitive", "message-sensitive"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("日志泄露敏感值 %q：%q", forbidden, output)
		}
	}
}

func TestClientLogsSafeHandshakeFailureDetails(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client, err := NewClient(ClientConfig{
		Endpoint: "ws://fake", BotID: "bot-1", Secret: "secret-sensitive", AllowedUserID: "user-1",
		Dial: func(context.Context, string) (Socket, error) {
			return nil, &weComDialError{kind: "handshake", statusCode: http.StatusNotFound}
		},
		Logger: logger,
		Wait:   func(ctx context.Context, _ time.Duration) error { return context.Canceled },
	})
	if err != nil {
		t.Fatal(err)
	}

	err = client.Run(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	output := logs.String()
	for _, want := range []string{"企业微信连接失败", "error_type=handshake", "http_status=404"} {
		if !strings.Contains(output, want) {
			t.Fatalf("日志缺少 %q：%q", want, output)
		}
	}
	if strings.Contains(output, "secret-sensitive") {
		t.Fatalf("日志泄露订阅密钥：%q", output)
	}
}

func TestClientCancellationDoesNotLogReconnect(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	socket := newFakeSocket()
	client, err := NewClient(ClientConfig{
		Endpoint: "ws://fake", BotID: "bot-1", Secret: "secret", AllowedUserID: "user-1",
		Dial: func(context.Context, string) (Socket, error) { return socket, nil }, Logger: logger,
		RequestID: sequentialIDs(), HeartbeatInterval: time.Hour, RequestTimeout: 100 * time.Millisecond,
		EventsCapacity: 4, Wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runClient(t, client, ctx)
	completeSubscribe(t, client, socket)
	cancel()
	awaitDone(t, done)
	if output := logs.String(); strings.Contains(output, "企业微信准备重连") {
		t.Fatalf("正常取消记录了重连日志：%q", output)
	}
}

func TestClientFormattingNeverLeaksSecret(t *testing.T) {
	secret := "format-secret"
	config := ClientConfig{Endpoint: "ws://example.test", BotID: "bot", Secret: secret, AllowedUserID: "user"}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprint(config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config), fmt.Sprint(client), fmt.Sprintf("%+v", client), fmt.Sprintf("%#v", client)} {
		if strings.Contains(value, secret) {
			t.Fatalf("formatted value leaked secret: %s", value)
		}
	}
}

func TestWeComWebSocketTransportUsesGatewayCompatibleHeaderSpelling(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://openws.work.weixin.qq.com", nil)
	request.Header["Sec-Websocket-Key"] = []string{"test-key"}
	request.Header["Sec-Websocket-Version"] = []string{"13"}

	base := roundTripperFunc(func(got *http.Request) (*http.Response, error) {
		if _, ok := got.Header["Sec-WebSocket-Key"]; !ok {
			t.Fatal("缺少企微网关兼容的 Sec-WebSocket-Key 头名")
		}
		if _, ok := got.Header["Sec-WebSocket-Version"]; !ok {
			t.Fatal("缺少企微网关兼容的 Sec-WebSocket-Version 头名")
		}
		if _, ok := got.Header["Sec-Websocket-Key"]; ok {
			t.Fatal("仍保留 Go 规范化后的 Sec-Websocket-Key 头名")
		}
		if _, ok := got.Header["Sec-Websocket-Version"]; ok {
			t.Fatal("仍保留 Go 规范化后的 Sec-Websocket-Version 头名")
		}
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: http.NoBody}, nil
	})

	if _, err := (weComWebSocketTransport{base: base}).RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
}

func TestClientWriteFailureDoesNotLeakSecret(t *testing.T) {
	secret := "write-secret"
	socket := newFakeSocket()
	socket.writeErr = errors.New(secret)
	client, err := NewClient(ClientConfig{Endpoint: "ws://fake", BotID: "bot-1", Secret: secret, AllowedUserID: "user-1", Dial: func(context.Context, string) (Socket, error) { return socket, nil }})
	if err != nil {
		t.Fatal(err)
	}
	client.install(newSession(context.Background(), socket, client.events))
	err = client.SendMarkdown(context.Background(), "content")
	if !errors.Is(err, ErrUnavailable) || contains(err.Error(), secret) {
		t.Fatalf("SendMarkdown() error = %v, want safe ErrUnavailable", err)
	}
}

func TestSessionResponseOwnershipWinsOverCancellationAndFinish(t *testing.T) {
	socket := newFakeSocket()
	session := newSession(context.Background(), socket, make(chan IncomingText, 1))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- session.request(ctx, "request-1", []byte(`{"cmd":"ping","headers":{"req_id":"request-1"}}`))
	}()
	_ = socket.nextWrite(t)
	socket.push(responseJSON("request-1", 0))
	waitPendingGone(t, &session.pending, "request-1")
	cancel()
	session.finish(ErrUnavailable)
	if err := <-result; err != nil {
		t.Fatalf("request result = %v, want resolved success", err)
	}
}

func TestSessionWriteErrorAfterResponseOwnershipReturnsResponse(t *testing.T) {
	socket := newFakeSocket()
	socket.writeErr = errors.New("transport write failure")
	session := newSession(context.Background(), socket, make(chan IncomingText, 1))
	socket.writeHook = func(data []byte) {
		socket.push(responseJSON(requestIDOfJSON(data), 0))
		waitPendingGone(t, &session.pending, requestIDOfJSON(data))
	}
	if err := session.request(context.Background(), "request-1", []byte(`{"cmd":"ping","headers":{"req_id":"request-1"}}`)); err != nil {
		t.Fatalf("request result = %v, want response success", err)
	}
	session.finish(ErrUnavailable)
}

func TestClientRequestContextCancelsWithoutEndingSession(t *testing.T) {
	socket := newFakeSocket()
	client := newTestClient(t, func(context.Context, string) (Socket, error) { return socket, nil })
	runCtx, stop := context.WithCancel(context.Background())
	done := runClient(t, client, runCtx)
	completeSubscribe(t, client, socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- client.SendMarkdown(ctx, "content") }()
	_ = socket.nextWrite(t)
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendMarkdown() = %v", err)
	}
	second := make(chan error, 1)
	go func() { second <- client.SendMarkdown(context.Background(), "content") }()
	write := socket.nextWrite(t)
	socket.push(responseJSON(requestIDOf(t, write), 0))
	if err := <-second; err != nil {
		t.Fatalf("second SendMarkdown() = %v, want healthy session", err)
	}
	stop()
	awaitDone(t, done)
}

func TestClientProductionWebSocketReconnectsAndReplacesPendingSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var connections atomic.Int32
	firstSubscribed := make(chan struct{}, 1)
	releaseSubscribe := make(chan struct{})
	pendingSeen := make(chan struct{}, 1)
	secondSubscribed := make(chan struct{}, 1)
	secondSend := make(chan string, 1)
	serverErrors := make(chan error, 4)
	recordError := func(err error) {
		select {
		case serverErrors <- err:
		default:
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			recordError(err)
			return
		}
		defer connection.CloseNow()
		index := connections.Add(1)
		_, subscribe, err := connection.Read(ctx)
		if err != nil {
			recordError(err)
			return
		}
		if commandOfJSON(subscribe) != "aibot_subscribe" {
			recordError(fmt.Errorf("first frame = %s", subscribe))
			return
		}
		requestID := requestIDOfJSON(subscribe)
		if index == 1 {
			if err := connection.Write(ctx, websocket.MessageText, textCallbackJSON("real-callback")); err != nil {
				recordError(err)
				return
			}
			firstSubscribed <- struct{}{}
			select {
			case <-releaseSubscribe:
			case <-ctx.Done():
				return
			}
			if err := connection.Write(ctx, websocket.MessageText, responseJSON(requestID, 0)); err != nil {
				recordError(err)
				return
			}
			_, pending, err := connection.Read(ctx)
			if err != nil {
				recordError(err)
				return
			}
			if commandOfJSON(pending) != "aibot_send_msg" {
				recordError(fmt.Errorf("pending command = %s", pending))
				return
			}
			pendingSeen <- struct{}{}
			_ = connection.Close(websocket.StatusNormalClosure, "test disconnect")
			return
		}
		if index == 2 {
			if err := connection.Write(ctx, websocket.MessageText, responseJSON(requestID, 0)); err != nil {
				recordError(err)
				return
			}
			secondSubscribed <- struct{}{}
			_, send, err := connection.Read(ctx)
			if err != nil {
				recordError(err)
				return
			}
			if commandOfJSON(send) != "aibot_send_msg" {
				recordError(fmt.Errorf("second command = %s", send))
				return
			}
			secondSend <- requestIDOfJSON(send)
			if err := connection.Write(ctx, websocket.MessageText, responseJSON(requestIDOfJSON(send), 0)); err != nil {
				recordError(err)
				return
			}
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: strings.Replace(server.URL, "http://", "ws://", 1), BotID: "bot-1", Secret: "secret", AllowedUserID: "user-1", Dial: productionDial, RequestID: sequentialIDs(), RequestTimeout: 500 * time.Millisecond, HeartbeatInterval: time.Hour, Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	runDone := runClient(t, client, ctx)
	select {
	case <-firstSubscribed:
	case <-ctx.Done():
		t.Fatal("first real websocket did not subscribe")
	}
	select {
	case event := <-client.Events():
		t.Fatalf("event delivered before subscribe response: %+v", event)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseSubscribe)
	select {
	case event := <-client.Events():
		if event.RequestID != "real-callback" {
			t.Fatalf("event = %+v", event)
		}
	case <-ctx.Done():
		t.Fatal("buffered real callback missing")
	}

	pendingResult := make(chan error, 1)
	go func() { pendingResult <- client.SendMarkdown(ctx, "first pending") }()
	select {
	case <-pendingSeen:
	case <-ctx.Done():
		t.Fatal("first real session did not receive pending send")
	}
	select {
	case err := <-pendingResult:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("pending result = %v, want ErrUnavailable", err)
		}
	case <-ctx.Done():
		t.Fatal("pending request was not cancelled")
	}
	select {
	case <-secondSubscribed:
	case <-ctx.Done():
		t.Fatal("second real websocket did not subscribe")
	}
	waitForCurrent(t, ctx, client)
	secondResult := make(chan error, 1)
	go func() { secondResult <- client.SendMarkdown(ctx, "second send") }()
	select {
	case <-secondSend:
	case <-ctx.Done():
		t.Fatal("new session did not receive send")
	}
	if err := <-secondResult; err != nil {
		t.Fatalf("new session SendMarkdown() = %v", err)
	}
	cancel()
	awaitDone(t, runDone)
	select {
	case err := <-serverErrors:
		t.Fatalf("websocket server error: %v", err)
	default:
	}
}

type fakeSocket struct {
	reads     chan fakeRead
	writes    chan []byte
	closed    chan struct{}
	once      sync.Once
	writeErr  error
	writeHook func([]byte)
}
type fakeRead struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{reads: make(chan fakeRead, 16), writes: make(chan []byte, 32), closed: make(chan struct{})}
}
func (s *fakeSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case value := <-s.reads:
		return value.typ, value.data, value.err
	case <-s.closed:
		return 0, nil, ErrUnavailable
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}
func (s *fakeSocket) Write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	if typ != websocket.MessageText {
		return fmt.Errorf("unexpected message type")
	}
	if s.writeErr != nil {
		if s.writeHook != nil {
			s.writeHook(data)
		}
		return s.writeErr
	}
	select {
	case s.writes <- append([]byte(nil), data...):
		return nil
	case <-s.closed:
		return ErrUnavailable
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitPendingGone(t *testing.T, pending *pendingRequests, requestID string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		pending.mu.Lock()
		_, exists := pending.waits[requestID]
		pending.mu.Unlock()
		if !exists {
			return
		}
		select {
		case <-deadline:
			t.Fatal("response did not take pending ownership")
		case <-time.After(time.Millisecond):
		}
	}
}
func (s *fakeSocket) Close(websocket.StatusCode, string) error {
	s.once.Do(func() { close(s.closed) })
	return nil
}
func (s *fakeSocket) push(data []byte) { s.reads <- fakeRead{typ: websocket.MessageText, data: data} }
func (s *fakeSocket) nextWrite(t *testing.T) []byte {
	t.Helper()
	select {
	case value := <-s.writes:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket write")
		return nil
	}
}

func newTestClient(t *testing.T, dial DialFunc) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{Endpoint: "ws://fake", BotID: "bot-1", Secret: "secret", AllowedUserID: "user-1", Dial: dial, RequestID: sequentialIDs(), HeartbeatInterval: time.Hour, RequestTimeout: 100 * time.Millisecond, EventsCapacity: 4, Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func sequentialIDs() func() string {
	var mu sync.Mutex
	next := 0
	return func() string { mu.Lock(); defer mu.Unlock(); next++; return fmt.Sprintf("request-%d", next) }
}
func runClient(t *testing.T, client *Client, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return done
}
func awaitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop")
	}
}
func waitForCurrent(t *testing.T, ctx context.Context, client *Client) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if client.getCurrent() != nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("current session was not installed")
		case <-ticker.C:
		}
	}
}

func waitForLog(t *testing.T, logs *lockedBuffer, fragment string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if strings.Contains(logs.String(), fragment) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("日志未出现 %q：%q", fragment, logs.String())
		case <-time.After(time.Millisecond):
		}
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
func completeSubscribe(t *testing.T, client *Client, socket *fakeSocket) {
	t.Helper()
	subscribe := socket.nextWrite(t)
	if commandOf(t, subscribe).Cmd != "aibot_subscribe" {
		t.Fatalf("command = %q, want subscribe", commandOf(t, subscribe).Cmd)
	}
	socket.push(responseJSON(requestIDOf(t, subscribe), 0))
	deadline := time.After(time.Second)
	for {
		if client.getCurrent() != nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("subscription did not install current session")
		case <-time.After(time.Millisecond):
		}
	}
}
func responseJSON(requestID string, code int) []byte {
	return []byte(fmt.Sprintf(`{"headers":{"req_id":%q},"errcode":%d,"errmsg":"ok"}`, requestID, code))
}
func textCallbackJSON(requestID string) []byte {
	return []byte(fmt.Sprintf(`{"cmd":"aibot_msg_callback","headers":{"req_id":%q},"body":{"msgid":"message-%s","aibotid":"bot-1","chattype":"single","from":{"userid":"user-1"},"msgtype":"text","text":{"content":"/ls"}}}`, requestID, requestID))
}

type wireCommand struct {
	Cmd string `json:"cmd"`
}

func commandOf(t *testing.T, data []byte) wireCommand {
	t.Helper()
	var command wireCommand
	if err := json.Unmarshal(data, &command); err != nil {
		t.Fatal(err)
	}
	return command
}
func requestIDOf(t *testing.T, data []byte) string {
	t.Helper()
	var request struct {
		Headers Headers `json:"headers"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	return request.Headers.RequestID
}
func commandOfJSON(data []byte) string {
	var command wireCommand
	if json.Unmarshal(data, &command) != nil {
		return ""
	}
	return command.Cmd
}
func requestIDOfJSON(data []byte) string {
	var request struct {
		Headers Headers `json:"headers"`
	}
	if json.Unmarshal(data, &request) != nil {
		return ""
	}
	return request.Headers.RequestID
}
func contains(value, fragment string) bool {
	return len(fragment) > 0 && len(value) >= len(fragment) && (value == fragment || (len(value) > len(fragment) && (containsAt(value, fragment))))
}
func containsAt(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
