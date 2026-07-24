package relayproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// NewFrame 将具体负载编码为当前协议版本的帧。
func NewFrame(frameType Type, requestID string, payload any) (Frame, error) {
	if !validType(frameType) {
		return Frame{}, fmt.Errorf("%w: 未知类型", ErrInvalidFrame)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, fmt.Errorf("%w: 编码负载", ErrInvalidFrame)
	}
	frame := Frame{Protocol: ProtocolVersion, Type: frameType, RequestID: requestID, Payload: raw}
	if err := validateEnvelope(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// Encode 严格校验并编码一条 Relay 帧。
func Encode(frame Frame) ([]byte, error) {
	if err := validateEnvelope(frame); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码 envelope", ErrInvalidFrame)
	}
	if len(encoded) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	return encoded, nil
}

// Decode 严格解码并校验一条 Relay 帧 envelope。
func Decode(data []byte) (Frame, error) {
	if len(data) > MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var frame Frame
	if err := decoder.Decode(&frame); err != nil {
		return Frame{}, fmt.Errorf("%w: 解码 envelope", ErrInvalidFrame)
	}
	if err := requireEOF(decoder); err != nil {
		return Frame{}, err
	}
	if err := validateEnvelope(frame); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

// DecodePayload 使用严格字段规则将帧负载解码为指定类型。
func DecodePayload[T any](frame Frame) (T, error) {
	var value T
	if len(frame.Payload) == 0 {
		return value, fmt.Errorf("%w: 缺少 payload", ErrInvalidFrame)
	}
	decoder := json.NewDecoder(bytes.NewReader(frame.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("%w: 解码 payload", ErrInvalidFrame)
	}
	if err := requireEOF(decoder); err != nil {
		return value, err
	}
	return value, nil
}

func validateEnvelope(frame Frame) error {
	if frame.Protocol != ProtocolVersion {
		return fmt.Errorf("%w: 收到版本 %d", ErrProtocolMismatch, frame.Protocol)
	}
	if !validType(frame.Type) {
		return fmt.Errorf("%w: 未知类型", ErrInvalidFrame)
	}
	if len(frame.Payload) == 0 || !json.Valid(frame.Payload) {
		return fmt.Errorf("%w: payload 无效", ErrInvalidFrame)
	}
	return nil
}

func validType(frameType Type) bool {
	switch frameType {
	case TypeClientHello, TypeServerHello, TypeSessionSnapshot, TypeSelectRequest,
		TypeSelectResult, TypeExecuteRequest, TypeExecuteResponse, TypeExecutePush,
		TypeNotification, TypePing, TypePong, TypeProtocolError:
		return true
	default:
		return false
	}
}

func requireEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: 包含尾随内容", ErrInvalidFrame)
	}
	return nil
}
