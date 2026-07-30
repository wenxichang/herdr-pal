package webadmin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxJSONBodyBytes = 64 * 1024
const maxJSONDepth = 128

// APIError 是 Web 管理 API 对外稳定的错误码和脱敏说明。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type successEnvelope struct {
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

type errorEnvelope struct {
	Error     APIError `json:"error"`
	RequestID string   `json:"request_id"`
}

func decodeJSON(_ http.ResponseWriter, request *http.Request, destination any) *APIError {
	if request == nil || request.Body == nil || destination == nil {
		return &APIError{Code: "invalid_json", Message: "JSON 请求体无效"}
	}
	if request.ContentLength > maxJSONBodyBytes {
		return &APIError{Code: "request_too_large", Message: "JSON 请求体超过 64 KiB 限制"}
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, maxJSONBodyBytes+1))
	if err != nil {
		return &APIError{Code: "invalid_json", Message: "读取 JSON 请求体失败"}
	}
	if len(data) == 0 || len(data) > maxJSONBodyBytes {
		code := "invalid_json"
		message := "JSON 请求体无效"
		if len(data) > maxJSONBodyBytes {
			code = "request_too_large"
			message = "JSON 请求体超过 64 KiB 限制"
		}
		return &APIError{Code: code, Message: message}
	}
	if err := validateUniqueJSON(data); err != nil {
		return &APIError{Code: "invalid_json", Message: "JSON 请求体包含重复字段或无效结构"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &APIError{Code: "invalid_json", Message: "JSON 请求字段无效"}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return &APIError{Code: "invalid_json", Message: "JSON 请求体只能包含一个值"}
	}
	return nil
}

func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON 包含第二个值")
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON 嵌套层级过深")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
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
				return errors.New("JSON 对象键无效")
			}
			if _, exists := seen[key]; exists {
				return errors.New("JSON 对象字段重复")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("JSON 对象未闭合")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("JSON 数组未闭合")
		}
	default:
		return errors.New("JSON 分隔符无效")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON 尾部数据无效")
	}
	return nil
}

func writeAPIData(writer http.ResponseWriter, request *http.Request, status int, data any) error {
	return writeJSONResponse(writer, status, successEnvelope{Data: data, RequestID: requestIDFrom(request)})
}

func writeAPIError(writer http.ResponseWriter, request *http.Request, status int, code, message string) error {
	setRequestError(request, code)
	return writeJSONResponse(writer, status, errorEnvelope{
		Error: APIError{Code: code, Message: message}, RequestID: requestIDFrom(request),
	})
}

func writeJSONResponse(writer http.ResponseWriter, status int, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, err = writer.Write(encoded)
	return err
}
