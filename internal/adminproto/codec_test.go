package adminproto

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeRequestAcceptsStrictHPAPRequest(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"protocol":"HPAP/1","id":"req-1","method":"key.disable","params":{"credential_id":12}}`))
	if err != nil {
		t.Fatalf("DecodeRequest() error = %v", err)
	}
	if request.Protocol != Protocol || request.ID != "req-1" || request.Method != MethodKeyDisable {
		t.Fatalf("DecodeRequest() = %#v", request)
	}
	var params struct {
		CredentialID uint64 `json:"credential_id"`
	}
	if err := DecodeParams(request.Params, &params); err != nil {
		t.Fatalf("DecodeParams() error = %v", err)
	}
	if params.CredentialID != 12 {
		t.Fatalf("credential_id = %d, want 12", params.CredentialID)
	}
}

func TestDecodeRequestRejectsInvalidJSONAndEnvelope(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		code ErrorCode
	}{
		{name: "重复顶层字段", data: []byte(`{"protocol":"HPAP/1","id":"req-1","id":"req-2","method":"server.status"}`), code: CodeProtocolInvalidRequest},
		{name: "重复嵌套字段", data: []byte(`{"protocol":"HPAP/1","id":"req-1","method":"key.show","params":{"credential_id":1,"credential_id":2}}`), code: CodeProtocolInvalidRequest},
		{name: "未知字段", data: []byte(`{"protocol":"HPAP/1","id":"req-1","method":"server.status","extra":true}`), code: CodeProtocolInvalidRequest},
		{name: "尾随 JSON", data: []byte(`{"protocol":"HPAP/1","id":"req-1","method":"server.status"} {}`), code: CodeProtocolInvalidRequest},
		{name: "非法 UTF8", data: append([]byte(`{"protocol":"HPAP/1","id":"`), append([]byte{0xff}, []byte(`","method":"server.status"}`)...)...), code: CodeProtocolInvalidRequest},
		{name: "协议版本", data: []byte(`{"protocol":"HPAP/2","id":"req-1","method":"server.status"}`), code: CodeProtocolUnsupportedVersion},
		{name: "未知方法", data: []byte(`{"protocol":"HPAP/1","id":"req-1","method":"key.rotate"}`), code: CodeProtocolUnsupportedMethod},
		{name: "空请求 ID", data: []byte(`{"protocol":"HPAP/1","id":" ","method":"server.status"}`), code: CodeProtocolInvalidRequest},
		{name: "换行请求 ID", data: []byte(`{"protocol":"HPAP/1","id":"req\n1","method":"server.status"}`), code: CodeProtocolInvalidRequest},
		{name: "非对象", data: []byte(`[]`), code: CodeProtocolInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRequest(test.data)
			var codecErr *CodecError
			if !errors.As(err, &codecErr) || codecErr.Code != test.code {
				t.Fatalf("DecodeRequest() error = %v, want code %q", err, test.code)
			}
		})
	}
}

func TestDecodeParamsRejectsUnknownDuplicateAndTrailingFields(t *testing.T) {
	tests := []string{
		`{"credential_id":1,"unknown":true}`,
		`{"credential_id":1,"credential_id":2}`,
		`{"credential_id":1} {}`,
	}
	for _, data := range tests {
		var params struct {
			CredentialID uint64 `json:"credential_id"`
		}
		if err := DecodeParams(json.RawMessage(data), &params); err == nil {
			t.Fatalf("DecodeParams(%q) should fail", data)
		}
	}
}

func TestResponseRequiresExactlyOneResultOrError(t *testing.T) {
	valid := []string{
		`{"protocol":"HPAP/1","id":"req-1","result":{}}`,
		`{"protocol":"HPAP/1","id":"req-1","error":{"code":"credential.not_found","message":"Key 不存在","details":{}}}`,
	}
	for _, data := range valid {
		if _, err := DecodeResponse([]byte(data)); err != nil {
			t.Fatalf("DecodeResponse(%s) error = %v", data, err)
		}
	}
	invalid := []string{
		`{"protocol":"HPAP/1","id":"req-1"}`,
		`{"protocol":"HPAP/1","id":"req-1","result":{},"error":{"code":"server.internal","message":"失败"}}`,
		`{"protocol":"HPAP/1","id":"req-1","error":{"code":"unknown.code","message":"失败"}}`,
	}
	for _, data := range invalid {
		if _, err := DecodeResponse([]byte(data)); err == nil {
			t.Fatalf("DecodeResponse(%s) should fail", data)
		}
	}
}

func TestEncodeRequestAndResponseValidateBeforeEncoding(t *testing.T) {
	encoded, err := EncodeRequest(Request{Protocol: Protocol, ID: "req-1", Method: MethodServerStatus})
	if err != nil || !bytes.HasSuffix(encoded, []byte("\n")) {
		t.Fatalf("EncodeRequest() = %q, %v", encoded, err)
	}
	if _, err := EncodeRequest(Request{Protocol: Protocol, ID: "req-1", Method: "key.rotate"}); err == nil {
		t.Fatal("EncodeRequest() accepted unsupported method")
	}
	response, err := NewResultResponse("req-1", struct {
		Status string `json:"status"`
	}{Status: "ok"})
	if err != nil {
		t.Fatalf("NewResultResponse() error = %v", err)
	}
	encoded, err = EncodeResponse(response)
	if err != nil || !bytes.Contains(encoded, []byte(`"status":"ok"`)) {
		t.Fatalf("EncodeResponse() = %q, %v", encoded, err)
	}
	if _, err := EncodeResponse(Response{Protocol: Protocol, ID: "req-1"}); err == nil {
		t.Fatal("EncodeResponse() accepted response without result or error")
	}
}

func TestReadFrameEnforcesLimitAndPreservesFollowingFrame(t *testing.T) {
	first := strings.Repeat("a", MaxFrameBytes+1) + "\n"
	second := `{"protocol":"HPAP/1","id":"req-2","method":"server.status"}` + "\n"
	reader := bufio.NewReaderSize(strings.NewReader(first+second), 128)
	if _, err := ReadFrame(reader); !IsCode(err, CodeProtocolLimitExceeded) {
		t.Fatalf("ReadFrame(oversized) error = %v", err)
	}
	frame, err := ReadFrame(reader)
	if err != nil {
		t.Fatalf("ReadFrame(second) error = %v", err)
	}
	if string(frame) != strings.TrimSuffix(second, "\n") {
		t.Fatalf("ReadFrame(second) = %q", frame)
	}
}

func TestReadFrameAcceptsEOFWithoutNewline(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(`{"protocol":"HPAP/1","id":"req-1","method":"server.status"}`))
	frame, err := ReadFrame(reader)
	if err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("ReadFrame() returned empty frame")
	}
}
