// Package hprp 定义 Herdr Pal 与 Relay Server 之间的公开 HPRP/1 协议。
package hprp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// ProtocolVersion 是 HPRP 信封使用的固定主版本。
	ProtocolVersion = "HPRP/1"
	// Subprotocol 是 WebSocket Upgrade 使用的 HPRP/1 子协议名称。
	Subprotocol = "herdr-pal-relay.v1"
	// MaxMessageBytes 是单条 HPRP 文本消息的默认硬限制。
	MaxMessageBytes = 1 << 20
	// MaxIDBytes 是消息关联标识的最大 UTF-8 字节数。
	MaxIDBytes = 256
)

var (
	// ErrInvalidMessage 表示 HPRP 消息结构、字段或 JSON 编码无效。
	ErrInvalidMessage = errors.New("HPRP 消息无效")
	// ErrProtocolMismatch 表示消息声明的 HPRP 主版本不受支持。
	ErrProtocolMismatch = errors.New("HPRP 协议版本不兼容")
	// ErrMessageTooLarge 表示消息超过连接允许的硬限制。
	ErrMessageTooLarge = errors.New("HPRP 消息过大")
	// ErrDuplicateField 表示 JSON object 中出现重复字段名。
	ErrDuplicateField = errors.New("HPRP JSON 字段重复")
)

var messageTypePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$`)

// Type 是发布后语义不可改变的小写点分消息类型。
type Type string

const (
	// TypeHelloClient 是 Pal 发出的连接协商首帧。
	TypeHelloClient Type = "hello.client"
)

// Envelope 是所有 HPRP/1 WebSocket 文本消息的公共信封。
type Envelope struct {
	Protocol       string          `json:"protocol"`
	Type           Type            `json:"type"`
	ID             string          `json:"id"`
	ReplyTo        string          `json:"reply_to,omitempty"`
	MustUnderstand bool            `json:"must_understand,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

// NewEnvelope 把结构化 payload 编码成一条经过基础校验的 HPRP 信封。
func NewEnvelope(messageType Type, id, replyTo string, mustUnderstand bool, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: 编码 payload", ErrInvalidMessage)
	}
	envelope := Envelope{
		Protocol: ProtocolVersion, Type: messageType, ID: id, ReplyTo: replyTo,
		MustUnderstand: mustUnderstand, Payload: raw,
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// Encode 校验并编码一条 HPRP/1 信封。
func Encode(envelope Envelope) ([]byte, error) {
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码信封", ErrInvalidMessage)
	}
	if len(encoded) > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	return encoded, nil
}

// Decode 解码 HPRP/1 信封，忽略未知可选字段并拒绝任意层级的重复 JSON 字段。
func Decode(data []byte) (Envelope, error) {
	if len(data) > MaxMessageBytes {
		return Envelope{}, ErrMessageTooLarge
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return Envelope{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var envelope Envelope
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("%w: 解码信封", ErrInvalidMessage)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Envelope{}, err
	}
	if err := validateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// DecodePayload 将信封 payload 解码为指定公开消息结构，并忽略未知可选字段。
func DecodePayload[T any](envelope Envelope) (T, error) {
	var value T
	if err := validatePayload(envelope.Payload); err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("%w: 解码 payload", ErrInvalidMessage)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return value, err
	}
	return value, nil
}

func validateEnvelope(envelope Envelope) error {
	if envelope.Protocol != ProtocolVersion {
		return fmt.Errorf("%w: 收到 %q", ErrProtocolMismatch, envelope.Protocol)
	}
	if !messageTypePattern.MatchString(string(envelope.Type)) {
		return fmt.Errorf("%w: type 无效", ErrInvalidMessage)
	}
	if !validMessageID(envelope.ID) || envelope.ReplyTo != "" && !validMessageID(envelope.ReplyTo) {
		return fmt.Errorf("%w: 消息 ID 无效", ErrInvalidMessage)
	}
	return validatePayload(envelope.Payload)
}

func validatePayload(payload json.RawMessage) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' || !json.Valid(trimmed) {
		return fmt.Errorf("%w: payload 必须是 JSON object", ErrInvalidMessage)
	}
	if err := rejectDuplicateJSONFields(trimmed); err != nil {
		return err
	}
	return nil
}

func validMessageID(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && len(value) <= MaxIDBytes
}

func requireJSONEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: 包含尾随内容", ErrInvalidMessage)
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: 解析 JSON", ErrInvalidMessage)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		fields := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("%w: 解析 object 字段", ErrInvalidMessage)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%w: object 字段名无效", ErrInvalidMessage)
			}
			if _, exists := fields[key]; exists {
				return fmt.Errorf("%w: %s", ErrDuplicateField, key)
			}
			fields[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("%w: object 未结束", ErrInvalidMessage)
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("%w: array 未结束", ErrInvalidMessage)
		}
	default:
		return fmt.Errorf("%w: JSON delimiter 无效", ErrInvalidMessage)
	}
	return nil
}
