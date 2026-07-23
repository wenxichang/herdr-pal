package herdr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const maxFrameBytes = 8 * 1024 * 1024

type requestEnvelope struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type responseEnvelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *errorBody      `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeRequest(writer io.Writer, request requestEnvelope) error {
	if err := json.NewEncoder(writer).Encode(request); err != nil {
		return fmt.Errorf("编码 Herdr 请求: %w", err)
	}
	return nil
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		content := fragment
		if err == nil {
			content = fragment[:len(fragment)-1]
		}
		if len(line)+len(content) > maxFrameBytes {
			return nil, fmt.Errorf("%w: 最大允许 %d 字节", ErrFrameTooLarge, maxFrameBytes)
		}
		line = append(line, content...)

		switch err {
		case nil:
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 {
				return nil, fmt.Errorf("%w: 收到空行", ErrProtocol)
			}
			return line, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, fmt.Errorf("%w: %w", ErrProtocol, io.EOF)
			}
			return line, nil
		default:
			return nil, err
		}
	}
}

func parseResponse(line []byte, expectedID string, result any) error {
	var response responseEnvelope
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("%w: 解码响应: %w", ErrProtocol, err)
	}
	if response.ID == "" {
		return fmt.Errorf("%w: 响应缺少 id", ErrProtocol)
	}
	if response.ID != expectedID {
		return fmt.Errorf("%w: 响应 id %q 与请求 id %q 不匹配", ErrProtocol, response.ID, expectedID)
	}
	if response.Error != nil && response.Result != nil {
		return fmt.Errorf("%w: 响应不能同时包含 result 和 error", ErrProtocol)
	}
	if response.Error != nil {
		if strings.TrimSpace(response.Error.Code) == "" || strings.TrimSpace(response.Error.Message) == "" {
			return fmt.Errorf("%w: 响应 error 缺少 code 或 message", ErrProtocol)
		}
		return &APIError{Code: response.Error.Code, Message: response.Error.Message}
	}
	if len(response.Result) == 0 || bytes.Equal(bytes.TrimSpace(response.Result), []byte("null")) {
		return fmt.Errorf("%w: 响应缺少 result", ErrProtocol)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("%w: 解码 result: %w", ErrProtocol, err)
	}
	return nil
}
