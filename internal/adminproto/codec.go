package adminproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const maxRequestIDBytes = 128

// DecodeRequest 严格解析并校验一条不含换行分隔符的 HPAP 请求帧。
func DecodeRequest(data []byte) (Request, error) {
	var request Request
	if err := decodeStrict(data, &request); err != nil {
		return Request{}, err
	}
	if request.Protocol != Protocol {
		return Request{}, newCodecError(CodeProtocolUnsupportedVersion, "不支持的 HPAP 协议版本", nil)
	}
	if err := validateRequestID(request.ID); err != nil {
		return Request{}, err
	}
	if !IsKnownMethod(request.Method) {
		return Request{}, newCodecError(CodeProtocolUnsupportedMethod, "不支持的 HPAP 方法", nil)
	}
	return request, nil
}

// DecodeResponse 严格解析并校验一条不含换行分隔符的 HPAP 响应帧。
func DecodeResponse(data []byte) (Response, error) {
	var response Response
	if err := decodeStrict(data, &response); err != nil {
		return Response{}, err
	}
	if err := validateResponse(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// DecodeParams 使用与信封相同的严格规则解析具体方法参数。
func DecodeParams(data json.RawMessage, destination any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		data = json.RawMessage(`{}`)
	}
	return decodeStrict(data, destination)
}

// EncodeRequest 校验请求并编码为单行 HPAP NDJSON。
func EncodeRequest(request Request) ([]byte, error) {
	if request.Protocol != Protocol {
		return nil, newCodecError(CodeProtocolUnsupportedVersion, "不支持的 HPAP 协议版本", nil)
	}
	if err := validateRequestID(request.ID); err != nil {
		return nil, err
	}
	if !IsKnownMethod(request.Method) {
		return nil, newCodecError(CodeProtocolUnsupportedMethod, "不支持的 HPAP 方法", nil)
	}
	return encodeFrame(request)
}

// EncodeResponse 校验响应并编码为单行 HPAP NDJSON。
func EncodeResponse(response Response) ([]byte, error) {
	if err := validateResponse(response); err != nil {
		return nil, err
	}
	return encodeFrame(response)
}

// NewResultResponse 创建携带成功结果的 HPAP 响应。
func NewResultResponse(requestID string, result any) (Response, error) {
	if err := validateRequestID(requestID); err != nil {
		return Response{}, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return Response{}, newCodecError(CodeServerInternal, "响应结果无法编码", err)
	}
	response := Response{Protocol: Protocol, ID: requestID, Result: encoded}
	if err := validateResponse(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// NewErrorResponse 创建携带稳定错误码的 HPAP 响应。
func NewErrorResponse(requestID string, responseError Error) (Response, error) {
	response := Response{Protocol: Protocol, ID: requestID, Error: &responseError}
	if err := validateResponse(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// ReadFrame 从缓冲流读取一条 HPAP NDJSON 帧并移除行尾分隔符。
//
// 超限帧会被完整消费，使调用方可以按策略继续读取下一帧或关闭连接。
func ReadFrame(reader *bufio.Reader) ([]byte, error) {
	if reader == nil {
		return nil, newCodecError(CodeProtocolInvalidRequest, "HPAP 输入流不可用", nil)
	}
	frame := make([]byte, 0, 1024)
	overLimit := false
	for {
		fragment, err := reader.ReadSlice('\n')
		payloadFragment := fragment
		if err == nil && len(payloadFragment) > 0 {
			payloadFragment = payloadFragment[:len(payloadFragment)-1]
		}
		if !overLimit {
			if len(frame)+len(payloadFragment) > MaxFrameBytes {
				overLimit = true
				frame = nil
			} else {
				frame = append(frame, payloadFragment...)
			}
		}

		switch {
		case err == nil:
			if overLimit {
				return nil, newCodecError(CodeProtocolLimitExceeded, "HPAP 帧超过大小限制", nil)
			}
			return bytes.TrimSuffix(frame, []byte{'\r'}), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if overLimit {
				return nil, newCodecError(CodeProtocolLimitExceeded, "HPAP 帧超过大小限制", nil)
			}
			if len(frame) == 0 {
				return nil, io.EOF
			}
			return frame, nil
		default:
			return nil, fmt.Errorf("读取 HPAP 帧: %w", err)
		}
	}
}

func encodeFrame(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, newCodecError(CodeServerInternal, "HPAP 帧无法编码", err)
	}
	if len(encoded) > MaxFrameBytes {
		return nil, newCodecError(CodeProtocolLimitExceeded, "HPAP 帧超过大小限制", nil)
	}
	return append(encoded, '\n'), nil
}

func decodeStrict(data []byte, destination any) error {
	if destination == nil || !utf8.Valid(data) {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP JSON 无效", nil)
	}
	if len(data) > MaxFrameBytes {
		return newCodecError(CodeProtocolLimitExceeded, "HPAP 帧超过大小限制", nil)
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP JSON 结构无效", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP JSON 无效或包含重复字段", err)
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("尾随 token %v", token)
		}
		return newCodecError(CodeProtocolInvalidRequest, "HPAP JSON 包含尾随内容", err)
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key 不是字符串")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("JSON 字段 %q 重复", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("JSON object 未闭合")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("JSON array 未闭合")
		}
	default:
		return errors.New("JSON delimiter 无效")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP JSON 包含尾随内容", err)
	}
	return nil
}

func validateResponse(response Response) error {
	if response.Protocol != Protocol {
		return newCodecError(CodeProtocolUnsupportedVersion, "不支持的 HPAP 协议版本", nil)
	}
	if err := validateRequestID(response.ID); err != nil {
		return err
	}
	hasResult := len(response.Result) > 0
	hasError := response.Error != nil
	if hasResult == hasError {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP 响应必须且只能包含 result 或 error", nil)
	}
	if hasResult {
		if !utf8.Valid(response.Result) || !json.Valid(response.Result) {
			return newCodecError(CodeProtocolInvalidRequest, "HPAP result 无效", nil)
		}
		return nil
	}
	if !IsKnownErrorCode(response.Error.Code) || strings.TrimSpace(response.Error.Message) == "" || !utf8.ValidString(response.Error.Message) {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP error 无效", nil)
	}
	if len(response.Error.Details) > 0 && (!utf8.Valid(response.Error.Details) || !json.Valid(response.Error.Details)) {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP error details 无效", nil)
	}
	return nil
}

func validateRequestID(requestID string) error {
	if strings.TrimSpace(requestID) == "" || len(requestID) > maxRequestIDBytes || !utf8.ValidString(requestID) {
		return newCodecError(CodeProtocolInvalidRequest, "HPAP 请求 ID 无效", nil)
	}
	for _, character := range requestID {
		if character < 0x20 || character == 0x7f {
			return newCodecError(CodeProtocolInvalidRequest, "HPAP 请求 ID 无效", nil)
		}
	}
	return nil
}
