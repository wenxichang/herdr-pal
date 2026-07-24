// Package wecom 提供企业微信智能机器人长连接所需的协议模型。
package wecom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/im"
)

const (
	// DefaultEndpoint 是企业微信智能机器人长连接的默认服务地址。
	DefaultEndpoint = "wss://openws.work.weixin.qq.com"
	// MarkdownByteLimit 是企业微信 Markdown 正文允许的最大 UTF-8 字节数。
	MarkdownByteLimit   = 20480
	logFieldByteLimit   = 80
	safeProtocolMessage = "企业微信请求失败"
)

var (
	// ErrProtocol 表示企业微信消息不符合本模块支持的协议约定。
	ErrProtocol = errors.New("企业微信协议错误")
	// ErrUnavailable 表示企业微信长连接已不可用。
	ErrUnavailable = errors.New("企业微信连接不可用")
)

// Headers 是企业微信帧中用于关联请求与响应的公共头部。
type Headers struct {
	RequestID string `json:"req_id"`
}

// IncomingText 是平台中立文本消息的兼容别名。
type IncomingText = im.IncomingText

// Response 是企业微信对单个请求返回的响应。
type Response struct {
	Headers Headers `json:"headers"`
	ErrCode int     `json:"errcode"`
	ErrMsg  string  `json:"errmsg"`
}

// ProtocolError 表示可关联到企业微信请求的协议或业务错误。
//
// Error 文本不会回显订阅密钥或 Markdown 正文；Message 只保留固定的安全摘要。
type ProtocolError struct {
	RequestID string
	ErrCode   int
	Message   string
}

// Error 返回适合日志和用户提示的安全错误摘要。
func (e *ProtocolError) Error() string {
	if e == nil {
		return ErrProtocol.Error()
	}
	requestID := sanitizeLogField(e.RequestID)
	if requestID == "" {
		return ErrProtocol.Error()
	}
	if e.ErrCode == 0 {
		return fmt.Sprintf("%s: 请求 %s", ErrProtocol, requestID)
	}
	return fmt.Sprintf("%s: 请求 %s 失败（错误码 %d）", ErrProtocol, requestID, e.ErrCode)
}

// Unwrap 使 ProtocolError 可被 errors.Is 识别为 ErrProtocol。
func (e *ProtocolError) Unwrap() error { return ErrProtocol }

// FrameKind 表示已解码企业微信帧的受限类别。
type FrameKind string

const (
	// FrameResponse 表示普通请求响应。
	FrameResponse FrameKind = "response"
	// FrameIncomingText 表示可安全交给业务层的文本回调。
	FrameIncomingText FrameKind = "incoming_text"
	// FrameUnsupportedCallback 表示已识别但当前不支持的非文本回调。
	FrameUnsupportedCallback FrameKind = "unsupported_callback"
	// FrameDisconnected 表示企业微信服务端要求当前连接断开。
	FrameDisconnected FrameKind = "disconnected"
	// FrameUnknown 表示未知命令，调用方只能将其作为原始协议帧记录。
	FrameUnknown FrameKind = "unknown"
)

// UnsupportedCallback 保存非文本回调的最小关联信息，避免它被误作为空 prompt。
type UnsupportedCallback struct {
	RequestID   string
	MessageID   string
	BotID       string
	UserID      string
	ChatType    string
	MessageType string
}

// Frame 是 DecodeFrame 对企业微信单帧的安全分类结果。
//
// 仅当 Kind 为 FrameIncomingText 时 IncomingText 才非空；未知帧保留 Raw 供有限审计。
type Frame struct {
	Kind         FrameKind
	Headers      Headers
	Response     *Response
	IncomingText *IncomingText
	Unsupported  *UnsupportedCallback
	Raw          json.RawMessage
}

// EncodeSubscribe 编码企业微信机器人订阅请求。
func EncodeSubscribe(requestID, botID, secret string) ([]byte, error) {
	if err := validateRequired(requestID, "请求标识"); err != nil {
		return nil, err
	}
	if err := validateRequired(botID, "机器人标识"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(secret) == "" {
		return nil, newProtocolError(requestID, 0, "订阅凭据缺失")
	}
	return encodeRequest("aibot_subscribe", requestID, struct {
		BotID  string `json:"bot_id"`
		Secret string `json:"secret"`
	}{BotID: botID, Secret: secret})
}

// EncodeRespondMarkdown 编码对回调消息的 Markdown 回复，并复用回调 req_id。
func EncodeRespondMarkdown(callbackRequestID, content string) ([]byte, error) {
	if err := validateRequired(callbackRequestID, "回调请求标识"); err != nil {
		return nil, err
	}
	if err := validateMarkdown(content, callbackRequestID); err != nil {
		return nil, err
	}
	return encodeMarkdownRequest("aibot_respond_msg", callbackRequestID, "", 0, content)
}

// EncodeSendMarkdown 编码面向指定单聊用户的主动 Markdown 消息。
func EncodeSendMarkdown(requestID, userID, content string) ([]byte, error) {
	if err := validateRequired(requestID, "请求标识"); err != nil {
		return nil, err
	}
	if err := validateRequired(userID, "用户标识"); err != nil {
		return nil, err
	}
	if err := validateMarkdown(content, requestID); err != nil {
		return nil, err
	}
	return encodeMarkdownRequest("aibot_send_msg", requestID, userID, 1, content)
}

// EncodePing 编码企业微信长连接心跳请求。
func EncodePing(requestID string) ([]byte, error) {
	if err := validateRequired(requestID, "请求标识"); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Command string  `json:"cmd"`
		Headers Headers `json:"headers"`
	}{Command: "ping", Headers: Headers{RequestID: requestID}})
}

func encodeMarkdownRequest(command, requestID, chatID string, chatType int, content string) ([]byte, error) {
	body := struct {
		ChatID   string `json:"chatid,omitempty"`
		ChatType int    `json:"chat_type,omitempty"`
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}{ChatID: chatID, ChatType: chatType, MsgType: "markdown"}
	body.Markdown.Content = content
	return encodeRequest(command, requestID, body)
}

func encodeRequest(command, requestID string, body any) ([]byte, error) {
	return json.Marshal(struct {
		Command string  `json:"cmd"`
		Headers Headers `json:"headers"`
		Body    any     `json:"body"`
	}{Command: command, Headers: Headers{RequestID: requestID}, Body: body})
}

func validateRequired(value, label string) error {
	if strings.TrimSpace(value) == "" {
		return newProtocolError("", 0, label+"缺失")
	}
	return nil
}

func validateMarkdown(content, requestID string) error {
	if !utf8.ValidString(content) {
		return newProtocolError(requestID, 0, "Markdown 正文不是有效 UTF-8")
	}
	if len(content) > MarkdownByteLimit {
		return newProtocolError(requestID, 0, "Markdown 正文超过字节限制")
	}
	return nil
}

// DecodeFrame 解码一条企业微信 WebSocket 文本帧。
//
// 已知帧严格校验必要字段并忽略未知 JSON 字段；未知 cmd 作为 FrameUnknown 安全返回。
func DecodeFrame(data []byte) (Frame, error) {
	var values map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil || values == nil {
		return Frame{}, newProtocolError("", 0, "JSON 帧无效")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Frame{}, newProtocolError("", 0, "JSON 帧包含尾随内容")
	}

	if _, hasCode := values["errcode"]; hasCode {
		return decodeResponse(values, data)
	}
	command, hasCommand, err := optionalString(values, "cmd")
	if err != nil {
		return Frame{}, newProtocolError("", 0, "命令字段无效")
	}
	if !hasCommand || command == "" {
		return Frame{}, newProtocolError("", 0, "命令字段缺失")
	}
	headers, err := decodeHeaders(values)
	if err != nil {
		return Frame{}, err
	}

	switch command {
	case "aibot_msg_callback":
		return decodeCallback(headers, values)
	case "aibot_event_callback":
		return decodeEventCallback(headers, values, data)
	default:
		return Frame{Kind: FrameUnknown, Headers: headers, Raw: append(json.RawMessage(nil), data...)}, nil
	}
}

func decodeEventCallback(headers Headers, values map[string]json.RawMessage, data []byte) (Frame, error) {
	body, err := objectField(values, "body")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件正文无效")
	}
	if _, err := requiredString(body, "msgid"); err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件消息标识无效")
	}
	if createTime, ok, err := optionalInt(body, "create_time"); err != nil || !ok || createTime < 0 {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件创建时间无效")
	}
	if _, err := requiredString(body, "aibotid"); err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件机器人标识无效")
	}
	messageType, err := requiredString(body, "msgtype")
	if err != nil || messageType != "event" {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件消息类型无效")
	}
	event, err := objectField(body, "event")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件内容无效")
	}
	eventType, err := requiredString(event, "eventtype")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "事件类型无效")
	}
	if eventType == "disconnected_event" {
		return Frame{Kind: FrameDisconnected, Headers: headers}, nil
	}
	return Frame{Kind: FrameUnknown, Headers: headers, Raw: append(json.RawMessage(nil), data...)}, nil
}

func decodeResponse(values map[string]json.RawMessage, data []byte) (Frame, error) {
	headers, err := decodeHeaders(values)
	if err != nil {
		return Frame{}, err
	}
	errCode, ok, err := optionalInt(values, "errcode")
	if err != nil || !ok {
		return Frame{}, newProtocolError(headers.RequestID, 0, "响应错误码无效")
	}
	errMsg, ok, err := optionalString(values, "errmsg")
	if err != nil || !ok {
		return Frame{}, newProtocolError(headers.RequestID, errCode, "响应说明无效")
	}
	response := Response{Headers: headers, ErrCode: errCode, ErrMsg: errMsg}
	frame := Frame{Kind: FrameResponse, Headers: headers, Response: &response, Raw: append(json.RawMessage(nil), data...)}
	return frame, nil
}

func decodeCallback(headers Headers, values map[string]json.RawMessage) (Frame, error) {
	body, err := objectField(values, "body")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "回调正文无效")
	}
	messageID, err := requiredString(body, "msgid")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "消息标识无效")
	}
	botID, err := requiredString(body, "aibotid")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "机器人标识无效")
	}
	chatType, err := requiredString(body, "chattype")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "会话类型无效")
	}
	messageType, err := requiredString(body, "msgtype")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "消息类型无效")
	}
	from, err := objectField(body, "from")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "发送者无效")
	}
	userID, err := requiredString(from, "userid")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "发送者标识无效")
	}
	if messageType != "text" {
		unsupported := &UnsupportedCallback{RequestID: headers.RequestID, MessageID: messageID, BotID: botID, UserID: userID, ChatType: chatType, MessageType: messageType}
		return Frame{Kind: FrameUnsupportedCallback, Headers: headers, Unsupported: unsupported}, nil
	}
	text, err := objectField(body, "text")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "文本正文无效")
	}
	content, err := requiredString(text, "content")
	if err != nil {
		return Frame{}, newProtocolError(headers.RequestID, 0, "文本内容无效")
	}
	incoming := &IncomingText{RequestID: headers.RequestID, MessageID: messageID, BotID: botID, UserID: userID, ChatType: chatType, Content: content}
	return Frame{Kind: FrameIncomingText, Headers: headers, IncomingText: incoming}, nil
}

func decodeHeaders(values map[string]json.RawMessage) (Headers, error) {
	headers, err := objectField(values, "headers")
	if err != nil {
		return Headers{}, newProtocolError("", 0, "请求头无效")
	}
	requestID, err := requiredString(headers, "req_id")
	if err != nil {
		return Headers{}, newProtocolError("", 0, "请求标识无效")
	}
	return Headers{RequestID: requestID}, nil
}

func objectField(values map[string]json.RawMessage, key string) (map[string]json.RawMessage, error) {
	raw, ok := values[key]
	if !ok {
		return nil, errors.New("field missing")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("field invalid")
	}
	return object, nil
}

func requiredString(values map[string]json.RawMessage, key string) (string, error) {
	value, ok, err := optionalString(values, key)
	if err != nil || !ok || value == "" {
		return "", errors.New("field invalid")
	}
	return value, nil
}

func optionalString(values map[string]json.RawMessage, key string) (string, bool, error) {
	raw, ok := values[key]
	if !ok {
		return "", false, nil
	}
	if isJSONNull(raw) {
		return "", true, errors.New("field is null")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, err
	}
	return value, true, nil
}

func optionalInt(values map[string]json.RawMessage, key string) (int, bool, error) {
	raw, ok := values[key]
	if !ok {
		return 0, false, nil
	}
	if isJSONNull(raw) {
		return 0, true, errors.New("field is null")
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, true, err
	}
	return value, true, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func sanitizeLogField(value string) string {
	var cleaned strings.Builder
	for _, character := range value {
		if !unicode.IsPrint(character) || character == '\u2028' || character == '\u2029' {
			continue
		}
		cleaned.WriteRune(character)
	}
	return truncateUTF8(strings.TrimSpace(cleaned.String()), logFieldByteLimit)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	const suffix = "…"
	if limit <= len(suffix) {
		return ""
	}
	var truncated strings.Builder
	for _, character := range value {
		encoded := string(character)
		if truncated.Len()+len(encoded)+len(suffix) > limit {
			break
		}
		truncated.WriteString(encoded)
	}
	return truncated.String() + suffix
}

func newProtocolError(requestID string, errCode int, _ string) *ProtocolError {
	return &ProtocolError{RequestID: sanitizeLogField(requestID), ErrCode: errCode, Message: safeProtocolMessage}
}

type requestResult struct {
	Response Response
	Err      error
}

// pendingRequests 为单条 WebSocket 连接维护尚未收到响应的请求。
//
// 每个等待通道容量为一，条目先从表中删除再投递，避免 resolve、超时取消和断连清理重复触发业务。
type pendingRequests struct {
	mu    sync.Mutex
	waits map[string]chan requestResult
}

func (p *pendingRequests) register(requestID string) (<-chan requestResult, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, newProtocolError("", 0, "请求标识缺失")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waits == nil {
		p.waits = make(map[string]chan requestResult)
	}
	if _, exists := p.waits[requestID]; exists {
		return nil, newProtocolError(requestID, 0, "请求标识重复")
	}
	wait := make(chan requestResult, 1)
	p.waits[requestID] = wait
	return wait, nil
}

func (p *pendingRequests) resolve(response Response) bool {
	requestID := response.Headers.RequestID
	if strings.TrimSpace(requestID) == "" {
		return false
	}
	wait := p.take(requestID)
	if wait == nil {
		return false
	}
	result := requestResult{Response: response}
	if response.ErrCode != 0 {
		result.Err = newProtocolError(requestID, response.ErrCode, response.ErrMsg)
	}
	wait <- result
	return true
}

func (p *pendingRequests) cancel(requestID string) bool {
	if strings.TrimSpace(requestID) == "" {
		return false
	}
	return p.take(requestID) != nil
}

func (p *pendingRequests) cancelAll(err error) {
	if err == nil {
		err = ErrUnavailable
	}
	p.mu.Lock()
	waits := p.waits
	p.waits = make(map[string]chan requestResult)
	p.mu.Unlock()
	for _, wait := range waits {
		wait <- requestResult{Err: err}
	}
}

func (p *pendingRequests) take(requestID string) chan requestResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waits == nil {
		return nil
	}
	wait := p.waits[requestID]
	delete(p.waits, requestID)
	return wait
}
