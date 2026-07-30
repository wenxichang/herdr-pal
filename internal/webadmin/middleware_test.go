package webadmin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserHandlerRejectsMissingCSRF(t *testing.T) {
	web, bootstrap, _ := newTestWebServer(t)
	login := loginWebAdmin(t, web, "admin", bootstrap.InitialPassword)
	cookie := findSessionCookie(t, login)
	handler := web.middleware(web.browserHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = writeAPIData(writer, request, http.StatusOK, map[string]bool{"ok": true})
	}), browserPolicy{AllowMustChange: true, RequireCSRF: true}))
	request := newTLSRequest(http.MethodPost, "/admin/api/v1/test", strings.NewReader(`{}`))
	request.Header.Set("Origin", "https://admin.example.test:4001")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
