package wecom

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestProtocolEncodeSubscribe(t *testing.T) {
	data, err := EncodeSubscribe("subscribe-1", "bot-1", "subscribe-secret")
	if err != nil {
		t.Fatalf("EncodeSubscribe() error = %v", err)
	}

	var got struct {
		Cmd     string  `json:"cmd"`
		Headers Headers `json:"headers"`
		Body    struct {
			BotID  string `json:"bot_id"`
			Secret string `json:"secret"`
		} `json:"body"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Cmd != "aibot_subscribe" || got.Headers.RequestID != "subscribe-1" || got.Body.BotID != "bot-1" || got.Body.Secret != "subscribe-secret" {
		t.Fatalf("EncodeSubscribe() = %+v, want complete subscribe request", got)
	}
}

func TestProtocolEncodeMarkdownRequests(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "respond reuses callback request id",
			data: encodeRespondMarkdown(t, "callback-1", "已收到"),
			want: "aibot_respond_msg",
		},
		{
			name: "send targets single user",
			data: encodeSendMarkdown(t, "send-1", "user-1", "主动通知"),
			want: "aibot_send_msg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Cmd     string  `json:"cmd"`
				Headers Headers `json:"headers"`
				Body    struct {
					ChatID   string `json:"chatid"`
					ChatType int    `json:"chat_type"`
					MsgType  string `json:"msgtype"`
					Markdown struct {
						Content string `json:"content"`
					} `json:"markdown"`
				} `json:"body"`
			}
			if err := json.Unmarshal(tt.data, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if got.Cmd != tt.want || got.Body.MsgType != "markdown" || got.Body.Markdown.Content == "" {
				t.Fatalf("request = %+v, want markdown %q", got, tt.want)
			}
			if got.Cmd == "aibot_respond_msg" && got.Headers.RequestID != "callback-1" {
				t.Fatalf("respond request id = %q, want callback-1", got.Headers.RequestID)
			}
			if got.Cmd == "aibot_send_msg" && (got.Headers.RequestID != "send-1" || got.Body.ChatID != "user-1" || got.Body.ChatType != 1) {
				t.Fatalf("send request = %+v, want user-1 single chat", got)
			}
		})
	}
}

func TestProtocolEncodePing(t *testing.T) {
	data, err := EncodePing("ping-1")
	if err != nil {
		t.Fatalf("EncodePing() error = %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(got) != 2 || got["cmd"] == nil || got["headers"] == nil || got["body"] != nil {
		t.Fatalf("EncodePing() top-level keys = %v, want only cmd and headers", mapsKeys(got))
	}
	var headerFields map[string]json.RawMessage
	if err := json.Unmarshal(got["headers"], &headerFields); err != nil {
		t.Fatalf("Unmarshal(header fields) error = %v", err)
	}
	if len(headerFields) != 1 || headerFields["req_id"] == nil {
		t.Fatalf("EncodePing() header keys = %v, want only req_id", mapsKeys(headerFields))
	}
	var command string
	var headers Headers
	if err := json.Unmarshal(got["cmd"], &command); err != nil {
		t.Fatalf("Unmarshal(cmd) error = %v", err)
	}
	if err := json.Unmarshal(got["headers"], &headers); err != nil {
		t.Fatalf("Unmarshal(headers) error = %v", err)
	}
	if command != "ping" || headers.RequestID != "ping-1" {
		t.Fatalf("EncodePing() = cmd=%q headers=%+v, want ping-1", command, headers)
	}
}

func TestProtocolEncodeRejectsInvalidAndOversizeValuesWithoutLeakingSecretOrContent(t *testing.T) {
	secret := "subscribe-secret"
	content := "private markdown content"
	tests := []struct {
		name string
		call func() ([]byte, error)
	}{
		{"empty subscribe id", func() ([]byte, error) { return EncodeSubscribe("", "bot-1", secret) }},
		{"empty bot id", func() ([]byte, error) { return EncodeSubscribe("subscribe-1", "", secret) }},
		{"empty secret", func() ([]byte, error) { return EncodeSubscribe("subscribe-1", "bot-1", "") }},
		{"empty callback id", func() ([]byte, error) { return EncodeRespondMarkdown("", content) }},
		{"empty user id", func() ([]byte, error) { return EncodeSendMarkdown("send-1", "", content) }},
		{"empty ping id", func() ([]byte, error) { return EncodePing("") }},
		{"oversize content", func() ([]byte, error) {
			return EncodeSendMarkdown("send-1", "user-1", strings.Repeat("界", MarkdownByteLimit/len("界")+1))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.call()
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), content) {
				t.Fatalf("error leaked sensitive value: %v", err)
			}
		})
	}

	boundary := strings.Repeat("a", MarkdownByteLimit)
	if _, err := EncodeSendMarkdown("send-1", "user-1", boundary); err != nil {
		t.Fatalf("EncodeSendMarkdown() at byte boundary error = %v", err)
	}
	if _, err := EncodeSendMarkdown("send-1", "user-1", boundary+"a"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("EncodeSendMarkdown() above byte boundary error = %v, want ErrProtocol", err)
	}
}

func TestProtocolDecodeTextCallbackFixture(t *testing.T) {
	data := readFixture(t, "testdata/message_text.json")
	frame, err := DecodeFrame(data)
	if err != nil {
		t.Fatalf("DecodeFrame() error = %v", err)
	}
	if frame.Kind != FrameIncomingText || frame.IncomingText == nil {
		t.Fatalf("frame = %+v, want incoming text", frame)
	}
	got := *frame.IncomingText
	if got != (IncomingText{RequestID: "callback-1", MessageID: "message-1", BotID: "bot-1", UserID: "user-1", ChatType: "single", Content: "/ls"}) {
		t.Fatalf("IncomingText = %+v", got)
	}
}

func TestProtocolDecodeUnsupportedCallbackDoesNotProduceText(t *testing.T) {
	frame, err := DecodeFrame([]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":"callback-2"},"body":{"msgid":"message-2","aibotid":"bot-1","chattype":"single","from":{"userid":"user-1"},"msgtype":"image"}}`))
	if err != nil {
		t.Fatalf("DecodeFrame() error = %v", err)
	}
	if frame.Kind != FrameUnsupportedCallback || frame.IncomingText != nil || frame.Unsupported == nil {
		t.Fatalf("frame = %+v, want unsupported non-text callback", frame)
	}
	if frame.Unsupported.MessageType != "image" || frame.Unsupported.RequestID != "callback-2" || frame.Unsupported.UserID != "user-1" {
		t.Fatalf("unsupported callback = %+v", frame.Unsupported)
	}
}

func TestProtocolDecodeResponseAndErrors(t *testing.T) {
	frame, err := DecodeFrame(readFixture(t, "testdata/response_ok.json"))
	if err != nil {
		t.Fatalf("DecodeFrame(response_ok) error = %v", err)
	}
	if frame.Kind != FrameResponse || frame.Response == nil || frame.Response.Headers.RequestID != "request-1" || frame.Response.ErrCode != 0 {
		t.Fatalf("response frame = %+v", frame)
	}

	_, err = DecodeFrame([]byte(`{"headers":{"req_id":"request-2"},"errcode":40001,"errmsg":"rejected"}`))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("DecodeFrame(error response) error = %v, want ErrProtocol", err)
	}
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.RequestID != "request-2" || protocolErr.ErrCode != 40001 || protocolErr.Message != "rejected" {
		t.Fatalf("ProtocolError = %#v", protocolErr)
	}

	for _, data := range [][]byte{
		[]byte(`{"headers":{},"errcode":0,"errmsg":"ok"}`),
		[]byte(`{"headers":null,"errcode":0,"errmsg":"ok"}`),
		[]byte(`{"headers":{"req_id":null},"errcode":0,"errmsg":"ok"}`),
		[]byte(`{"headers":{"req_id":"request-1"},"errcode":null,"errmsg":"ok"}`),
		[]byte(`{"headers":{"req_id":"request-1"},"errcode":0,"errmsg":null}`),
		[]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":"callback-1"},"body":{"msgid":"m","aibotid":"b","chattype":"single","from":{"userid":"u"},"msgtype":"text","text":{}}}`),
		[]byte(`{"cmd":"aibot_msg_callback","headers":{"req_id":3},"body":{}}`),
		[]byte(`{`),
		[]byte(`{"cmd":"future_command","headers":{"req_id":"future-1"}} {}`),
	} {
		if _, err := DecodeFrame(data); !errors.Is(err, ErrProtocol) {
			t.Fatalf("DecodeFrame(%s) error = %v, want ErrProtocol", data, err)
		}
	}
}

func TestProtocolDecodeDisconnectedAndUnknownFrames(t *testing.T) {
	assertDisconnectedFixtureShape(t, readFixture(t, "testdata/event_disconnected.json"))
	disconnected, err := DecodeFrame(readFixture(t, "testdata/event_disconnected.json"))
	if err != nil {
		t.Fatalf("DecodeFrame(disconnected) error = %v", err)
	}
	if disconnected.Kind != FrameDisconnected || disconnected.Headers.RequestID != "event-callback-1" {
		t.Fatalf("disconnected frame = %+v", disconnected)
	}

	event, err := DecodeFrame([]byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"event-2"},"body":{"msgid":"event-message-2","create_time":1720000000,"aibotid":"bot-1","msgtype":"event","event":{"eventtype":"future_event"}}}`))
	if err != nil {
		t.Fatalf("DecodeFrame(future event) error = %v", err)
	}
	if event.Kind != FrameUnknown || event.IncomingText != nil {
		t.Fatalf("future event frame = %+v, want non-text unknown frame", event)
	}
	if _, err := DecodeFrame([]byte(`{"cmd":"aibot_event_callback","headers":{"req_id":"event-3"},"body":{"msgid":"event-message-3","aibotid":"bot-1","msgtype":"event","event":{"eventtype":"disconnected_event"}}}`)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("DecodeFrame(invalid event) error = %v, want ErrProtocol", err)
	}
	legacy, err := DecodeFrame([]byte(`{"cmd":"disconnected_event","headers":{"req_id":"legacy-1"},"body":{}}`))
	if err != nil {
		t.Fatalf("DecodeFrame(legacy disconnected command) error = %v", err)
	}
	if legacy.Kind == FrameDisconnected {
		t.Fatalf("legacy disconnected command = %+v, must not be treated as official disconnected event", legacy)
	}

	unknown, err := DecodeFrame([]byte(`{"cmd":"future_command","headers":{"req_id":"future-1"},"body":{"later":true}}`))
	if err != nil {
		t.Fatalf("DecodeFrame(unknown) error = %v", err)
	}
	if unknown.Kind != FrameUnknown || unknown.Raw == nil || unknown.IncomingText != nil {
		t.Fatalf("unknown frame = %+v", unknown)
	}
}

func TestProtocolPendingRequestsResolveAndCancel(t *testing.T) {
	var pending pendingRequests
	wait, err := pending.register("request-1")
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if _, err := pending.register("request-1"); !errors.Is(err, ErrProtocol) {
		t.Fatalf("duplicate register error = %v, want ErrProtocol", err)
	}
	if !pending.resolve(Response{Headers: Headers{RequestID: "request-1"}, ErrCode: 0}) {
		t.Fatal("resolve() = false, want true")
	}
	result := <-wait
	if result.Err != nil || result.Response.Headers.RequestID != "request-1" {
		t.Fatalf("result = %+v", result)
	}
	if pending.resolve(Response{Headers: Headers{RequestID: "request-1"}}) {
		t.Fatal("late resolve() = true, want false")
	}

	wait, err = pending.register("request-2")
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if !pending.cancel("request-2") {
		t.Fatal("cancel() = false, want true")
	}
	if pending.resolve(Response{Headers: Headers{RequestID: "request-2"}}) {
		t.Fatal("resolve after cancel = true, want false")
	}
	select {
	case <-wait:
		t.Fatal("cancel() must remove without delivering a result")
	default:
	}
}

func TestProtocolPendingRequestsReturnResponseErrorsAndCancelAll(t *testing.T) {
	var pending pendingRequests
	failedWait, err := pending.register("request-failed")
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if !pending.resolve(Response{Headers: Headers{RequestID: "request-failed"}, ErrCode: 42, ErrMsg: "not allowed"}) {
		t.Fatal("resolve() = false, want true")
	}
	failed := <-failedWait
	if !errors.Is(failed.Err, ErrProtocol) {
		t.Fatalf("failed result = %+v, want protocol error", failed)
	}

	first, err := pending.register("first")
	if err != nil {
		t.Fatalf("register(first) error = %v", err)
	}
	second, err := pending.register("second")
	if err != nil {
		t.Fatalf("register(second) error = %v", err)
	}
	stopErr := errors.New("连接已断开")
	pending.cancelAll(stopErr)
	for _, wait := range []<-chan requestResult{first, second} {
		result := <-wait
		if !errors.Is(result.Err, stopErr) {
			t.Fatalf("cancelAll result = %+v, want %v", result, stopErr)
		}
	}
	if pending.resolve(Response{Headers: Headers{RequestID: "first"}}) {
		t.Fatal("resolve after cancelAll = true, want false")
	}
}

func TestProtocolPendingRequestsConcurrentResolveAndCancelAll(t *testing.T) {
	const requests = 200
	var pending pendingRequests
	for index := 0; index < requests; index++ {
		if _, err := pending.register("request-" + string(rune('a'+index%26)) + "-" + strings.Repeat("x", index%3)); err == nil && index >= 78 {
			// 仅用于确保测试不会依赖重复 id 注册成功。
			break
		}
	}

	// 用唯一 ID 建立可竞争的等待表。
	var races pendingRequests
	for index := 0; index < requests; index++ {
		if _, err := races.register(testRequestID(index)); err != nil {
			t.Fatalf("register(%d) error = %v", index, err)
		}
	}
	var group sync.WaitGroup
	for index := 0; index < requests; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			races.resolve(Response{Headers: Headers{RequestID: testRequestID(index)}})
		}(index)
	}
	group.Add(1)
	go func() {
		defer group.Done()
		races.cancelAll(errors.New("关闭"))
	}()
	group.Wait()
	if races.resolve(Response{Headers: Headers{RequestID: testRequestID(0)}}) {
		t.Fatal("all entries must be removed after racing resolve/cancelAll")
	}
}

func encodeRespondMarkdown(t *testing.T, requestID, content string) []byte {
	t.Helper()
	data, err := EncodeRespondMarkdown(requestID, content)
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	return data
}

func encodeSendMarkdown(t *testing.T, requestID, userID, content string) []byte {
	t.Helper()
	data, err := EncodeSendMarkdown(requestID, userID, content)
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	return data
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return data
}

func testRequestID(index int) string {
	return "request-" + strings.Repeat("x", index+1)
}

func mapsKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func assertDisconnectedFixtureShape(t *testing.T, data []byte) {
	t.Helper()
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("Unmarshal(event fixture) error = %v", err)
	}
	if len(frame) != 3 || frame["cmd"] == nil || frame["headers"] == nil || frame["body"] == nil {
		t.Fatalf("event fixture keys = %v, want cmd/headers/body", mapsKeys(frame))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(frame["body"], &body); err != nil {
		t.Fatalf("Unmarshal(event body) error = %v", err)
	}
	for _, key := range []string{"msgid", "create_time", "aibotid", "msgtype", "event"} {
		if body[key] == nil {
			t.Fatalf("event fixture body missing %q", key)
		}
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(body["event"], &event); err != nil {
		t.Fatalf("Unmarshal(event payload) error = %v", err)
	}
	if len(event) != 1 || event["eventtype"] == nil {
		t.Fatalf("event fixture payload keys = %v, want only eventtype", mapsKeys(event))
	}
}
