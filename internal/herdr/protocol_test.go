package herdr

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAPIErrorFormatsMessage(t *testing.T) {
	err := &APIError{Code: "agent_not_found", Message: "目标不存在"}
	if got, want := err.Error(), "Herdr API agent_not_found: 目标不存在"; got != want {
		t.Fatalf("APIError.Error() = %q，期望 %q", got, want)
	}
}

func TestNDJSONWriteRequestWritesOneJSONLine(t *testing.T) {
	var output bytes.Buffer
	err := writeRequest(&output, requestEnvelope{
		ID:     "req-1",
		Method: "ping",
		Params: map[string]any{},
	})
	if err != nil {
		t.Fatalf("writeRequest() 返回错误：%v", err)
	}

	if got, want := output.String(), "{\"id\":\"req-1\",\"method\":\"ping\",\"params\":{}}\n"; got != want {
		t.Fatalf("writeRequest() = %q，期望 %q", got, want)
	}
}

func TestNDJSONReadLineConsumesOneLineAtATime(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("first\nsecond\n"))

	first, err := readLine(reader)
	if err != nil {
		t.Fatalf("第一次 readLine() 返回错误：%v", err)
	}
	second, err := readLine(reader)
	if err != nil {
		t.Fatalf("第二次 readLine() 返回错误：%v", err)
	}
	if got, want := string(first), "first"; got != want {
		t.Fatalf("第一行 = %q，期望 %q", got, want)
	}
	if got, want := string(second), "second"; got != want {
		t.Fatalf("第二行 = %q，期望 %q", got, want)
	}
}

func TestNDJSONReadLineRemovesOnlyTrailingCR(t *testing.T) {
	line, err := readLine(bufio.NewReader(strings.NewReader("body\\r\r\n")))
	if err != nil {
		t.Fatalf("readLine() 返回错误：%v", err)
	}
	if got, want := string(line), "body\\r"; got != want {
		t.Fatalf("行内容 = %q，期望 %q", got, want)
	}
}

func TestNDJSONReadLineRejectsEmptyLineAndEmptyEOF(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "空行", input: "\n"},
		{name: "空 EOF", input: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readLine(bufio.NewReader(strings.NewReader(test.input)))
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("readLine() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestNDJSONReadLineRejectsFrameLargerThanEightMiB(t *testing.T) {
	input := strings.Repeat("x", 8*1024*1024+1) + "\n"
	_, err := readLine(bufio.NewReaderSize(strings.NewReader(input), 1024))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readLine() 错误 = %v，期望 ErrFrameTooLarge", err)
	}
}

func TestNDJSONParseResponseDecodesSuccessResult(t *testing.T) {
	var result struct {
		Type string `json:"type"`
		OK   bool   `json:"ok"`
	}
	err := parseResponse([]byte(`{"id":"req-1","result":{"type":"pong","ok":true}}`), "req-1", &result)
	if err != nil {
		t.Fatalf("parseResponse() 返回错误：%v", err)
	}
	if result.Type != "pong" || !result.OK {
		t.Fatalf("result = %+v，期望 pong/true", result)
	}
}

func TestNDJSONParseResponseReturnsAPIError(t *testing.T) {
	err := parseResponse([]byte(`{"id":"req-1","error":{"code":"agent_not_found","message":"目标不存在"}}`), "req-1", nil)
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("parseResponse() 错误 = %T %[1]v，期望 *APIError", err)
	}
	if apiError.Code != "agent_not_found" || apiError.Message != "目标不存在" {
		t.Fatalf("APIError = %+v", apiError)
	}
}

func TestNDJSONParseResponseRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "无效 JSON", line: `{`},
		{name: "缺少 ID", line: `{"result":{}}`},
		{name: "ID 不匹配", line: `{"id":"req-2","result":{}}`},
		{name: "同时包含结果和错误", line: `{"id":"req-1","result":{},"error":{"code":"bad","message":"bad"}}`},
		{name: "缺少结果和错误", line: `{"id":"req-1"}`},
		{name: "空结果", line: `{"id":"req-1","result":null}`},
		{name: "错误码为空", line: `{"id":"req-1","error":{"code":"","message":"bad"}}`},
		{name: "错误消息为空", line: `{"id":"req-1","error":{"code":"bad","message":""}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := parseResponse([]byte(test.line), "req-1", nil)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("parseResponse() 错误 = %v，期望 ErrProtocol", err)
			}
		})
	}
}

func TestNDJSONParseResponseLeavesNilResultTargetUntouched(t *testing.T) {
	err := parseResponse([]byte(`{"id":"req-1","result":{"type":"pong"}}`), "req-1", nil)
	if err != nil {
		t.Fatalf("parseResponse() 返回错误：%v", err)
	}
}

func TestNDJSONResponseEnvelopeJSONTags(t *testing.T) {
	var response responseEnvelope
	if err := json.Unmarshal([]byte(`{"id":"req-1","result":{}}`), &response); err != nil {
		t.Fatalf("解码 responseEnvelope 失败：%v", err)
	}
	if response.ID != "req-1" || response.Result == nil || response.Error != nil {
		t.Fatalf("responseEnvelope = %+v", response)
	}
}
