package adminserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

func TestServerRuntimeHandlerStatusDebugAndStopAfterWrite(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	runtime := &fakeRuntimeInspector{status: adminproto.ServerStatusResult{
		ObservedAt: now, Version: "v1.0.0", PID: 1234, GOOS: "linux", GOARCH: "amd64",
		HPAP: adminproto.Protocol, HPRP: "HPRP/1", RelayListen: "0.0.0.0:9443", AdminSocket: "/state/admin.sock",
		WeCom:        adminproto.WeComStatus{State: "reconnecting", ChangedAt: now.Add(-time.Minute), LastErrorType: "dns"},
		TLS:          adminproto.TLSStatus{Mode: "automatic", SHA256Fingerprint: strings.Repeat("a", 64)},
		BaseLogLevel: "warn",
	}}
	handler, err := NewRuntimeHandler(newAdminServiceForTest(t, nil, nil, nil, runtime, func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	statusResponse := handleRuntimeRequest(t, handler, adminproto.MethodServerStatus, adminproto.EmptyParams{})
	var status adminproto.ServerStatusResult
	decodeKeyResult(t, statusResponse.Response, &status)
	if status != runtime.status {
		t.Fatalf("server status = %#v, want %#v", status, runtime.status)
	}
	encoded, err := adminproto.EncodeResponse(statusResponse.Response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "token", "terminal", "raw upstream response"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("server status leaked %q: %s", forbidden, encoded)
		}
	}

	enabled := handleRuntimeRequest(t, handler, adminproto.MethodServerDebugEnable, adminproto.EmptyParams{})
	var enabledResult adminproto.ServerDebugResult
	decodeKeyResult(t, enabled.Response, &enabledResult)
	if !enabledResult.DebugEnabled || runtime.enableCalls != 1 {
		t.Fatalf("debug enable result=%#v calls=%d", enabledResult, runtime.enableCalls)
	}
	disabled := handleRuntimeRequest(t, handler, adminproto.MethodServerDebugDisable, adminproto.EmptyParams{})
	var disabledResult adminproto.ServerDebugResult
	decodeKeyResult(t, disabled.Response, &disabledResult)
	if disabledResult.DebugEnabled || runtime.disableCalls != 1 {
		t.Fatalf("debug disable result=%#v calls=%d", disabledResult, runtime.disableCalls)
	}

	stop := handleRuntimeRequest(t, handler, adminproto.MethodServerStop, adminproto.EmptyParams{})
	var stopResult adminproto.ServerStopResult
	decodeKeyResult(t, stop.Response, &stopResult)
	if !stopResult.Stopping || runtime.stopCalls != 0 || stop.AfterWrite == nil {
		t.Fatalf("stop before write: result=%#v calls=%d action_set=%t", stopResult, runtime.stopCalls, stop.AfterWrite != nil)
	}
	stop.AfterWrite()
	if runtime.stopCalls != 1 {
		t.Fatalf("stop calls after write = %d", runtime.stopCalls)
	}
	busy := handleRuntimeRequest(t, handler, adminproto.MethodServerStop, adminproto.EmptyParams{})
	assertKeyError(t, busy.Response, adminproto.CodeServerBusy)
}

func TestServerRuntimeHandlerRejectsUnexpectedParams(t *testing.T) {
	runtime := &fakeRuntimeInspector{}
	handler, err := NewRuntimeHandler(newAdminServiceForTest(t, nil, nil, nil, runtime, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: adminproto.MethodServerStatus, Params: json.RawMessage(`{"extra":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	assertKeyError(t, result.Response, adminproto.CodeArgumentInvalid)
}

func TestServerRuntimeHandlerReleasesStopReservationWhenResponseWriteFails(t *testing.T) {
	runtime := &fakeRuntimeInspector{}
	handler, err := NewRuntimeHandler(newAdminServiceForTest(t, nil, nil, nil, runtime, time.Now))
	if err != nil {
		t.Fatal(err)
	}
	first := handleRuntimeRequest(t, handler, adminproto.MethodServerStop, adminproto.EmptyParams{})
	if first.AfterWriteFailure == nil {
		t.Fatal("server.stop did not provide write failure rollback")
	}
	first.AfterWriteFailure()
	second := handleRuntimeRequest(t, handler, adminproto.MethodServerStop, adminproto.EmptyParams{})
	if second.Response.Error != nil || second.AfterWrite == nil {
		t.Fatalf("server.stop remained busy after write failure: %#v", second.Response)
	}
	second.AfterWrite()
	if runtime.stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", runtime.stopCalls)
	}
}

func handleRuntimeRequest(t *testing.T, handler *RuntimeHandler, method adminproto.Method, params any) HandleResult {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.Handle(t.Context(), adminproto.Request{Protocol: adminproto.Protocol, ID: "req-1", Method: method, Params: encoded})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

type fakeRuntimeInspector struct {
	status       adminservice.ServerStatus
	enableCalls  int
	disableCalls int
	stopCalls    int
}

func (runtime *fakeRuntimeInspector) Status() adminservice.ServerStatus { return runtime.status }
func (runtime *fakeRuntimeInspector) EnableDebug() {
	runtime.enableCalls++
	runtime.status.DebugEnabled = true
}
func (runtime *fakeRuntimeInspector) DisableDebug() {
	runtime.disableCalls++
	runtime.status.DebugEnabled = false
}
func (runtime *fakeRuntimeInspector) RequestStop() bool {
	runtime.stopCalls++
	return runtime.stopCalls == 1
}
