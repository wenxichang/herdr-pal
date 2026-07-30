package webadmin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStrictJSONRejectsDuplicatesUnknownTrailingAndOversizedBodies(t *testing.T) {
	web, _, logs := newTestWebServer(t)
	type payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	handler := web.middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var decoded payload
		if apiErr := decodeJSON(writer, request, &decoded); apiErr != nil {
			writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
			return
		}
		_ = writeAPIData(writer, request, http.StatusOK, decoded)
	}))
	tests := []string{
		`{"username":"admin","username":"root","password":"secret-value"}`,
		`{"username":"admin","password":"secret-value","unknown":true}`,
		`{"username":"admin","password":"secret-value"}{"username":"root","password":"other"}`,
		`{"username":"admin","password":{"nested":1,"nested":2}}`,
		`{"username":"admin","password":"` + strings.Repeat("x", 70*1024) + `"}`,
	}
	for _, body := range tests {
		request := newTLSRequest(http.MethodPost, "/admin/api/v1/test", strings.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	validRequest := newTLSRequest(http.MethodPost, "/admin/api/v1/test", strings.NewReader(`{"username":"admin","password":"valid secret"}`))
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, validRequest)
	if validResponse.Code != http.StatusOK {
		t.Fatalf("valid status=%d body=%s", validResponse.Code, validResponse.Body.String())
	}
	if strings.Contains(logs.String(), "secret-value") || strings.Contains(logs.String(), strings.Repeat("x", 64)) {
		t.Fatalf("logs leaked JSON body: %q", logs.String())
	}
}
