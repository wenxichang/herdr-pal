package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthLoginSessionPasswordAndLogoutFlow(t *testing.T) {
	web, bootstrap, _ := newTestWebServer(t)
	loginResponse := loginWebAdmin(t, web, "admin", bootstrap.InitialPassword)
	cookie := findSessionCookie(t, loginResponse)
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" || cookie.MaxAge != 0 {
		t.Fatalf("login cookie = %#v", cookie)
	}
	login := decodeWebEnvelope(t, loginResponse)
	csrf := login["data"].(map[string]any)["csrf_token"].(string)
	if login["data"].(map[string]any)["must_change_password"] != true || csrf == "" {
		t.Fatalf("login data = %#v", login["data"])
	}

	protected := web.middleware(web.browserHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = writeAPIData(writer, request, http.StatusOK, map[string]bool{"ok": true})
	}), browserPolicy{}))
	protectedRequest := newTLSRequest(http.MethodGet, "/admin/api/v1/protected", nil)
	protectedRequest.AddCookie(cookie)
	protectedResponse := httptest.NewRecorder()
	protected.ServeHTTP(protectedResponse, protectedRequest)
	if protectedResponse.Code != http.StatusForbidden {
		t.Fatalf("must-change protected status=%d body=%s", protectedResponse.Code, protectedResponse.Body.String())
	}

	sessionRequest := newTLSRequest(http.MethodGet, "/admin/api/v1/auth/session", nil)
	sessionRequest.AddCookie(cookie)
	sessionResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	rotatedCookie := findSessionCookie(t, sessionResponse)
	sessionData := decodeWebEnvelope(t, sessionResponse)["data"].(map[string]any)
	csrf = sessionData["csrf_token"].(string)

	passwordRequest := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/password", strings.NewReader(`{"current_password":"`+bootstrap.InitialPassword+`","new_password":"replacement password"}`))
	passwordRequest.Header.Set("Origin", "https://admin.example.test:4001")
	passwordRequest.Header.Set("X-CSRF-Token", csrf)
	passwordRequest.AddCookie(rotatedCookie)
	passwordResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(passwordResponse, passwordRequest)
	if passwordResponse.Code != http.StatusOK {
		t.Fatalf("password status=%d body=%s", passwordResponse.Code, passwordResponse.Body.String())
	}
	passwordCookie := findSessionCookie(t, passwordResponse)
	passwordData := decodeWebEnvelope(t, passwordResponse)["data"].(map[string]any)
	if passwordData["must_change_password"] != false || passwordData["csrf_token"] == "" {
		t.Fatalf("password data = %#v", passwordData)
	}

	logoutRequest := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/logout", strings.NewReader(`{}`))
	logoutRequest.Header.Set("Origin", "https://admin.example.test:4001")
	logoutRequest.Header.Set("X-CSRF-Token", passwordData["csrf_token"].(string))
	logoutRequest.AddCookie(passwordCookie)
	logoutResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(logoutResponse, logoutRequest)
	if logoutResponse.Code != http.StatusOK || findSessionCookie(t, logoutResponse).MaxAge >= 0 {
		t.Fatalf("logout status=%d cookie=%#v body=%s", logoutResponse.Code, findSessionCookie(t, logoutResponse), logoutResponse.Body.String())
	}
}

func TestAuthLoginLockAndOriginCSRFChecks(t *testing.T) {
	web, bootstrap, _ := newTestWebServer(t)
	for attempt := 1; attempt <= 5; attempt++ {
		response := loginWebAdmin(t, web, "admin", "wrong password")
		if attempt < 5 && response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", attempt, response.Code)
		}
		if attempt == 5 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("fifth attempt status = %d body=%s", response.Code, response.Body.String())
		}
	}
	locked := loginWebAdmin(t, web, "admin", bootstrap.InitialPassword)
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("locked login status = %d", locked.Code)
	}

	web, bootstrap, _ = newTestWebServer(t)
	missingOrigin := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"`+bootstrap.InitialPassword+`"}`))
	missingResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(missingResponse, missingOrigin)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing origin status = %d", missingResponse.Code)
	}
	wrongOrigin := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"`+bootstrap.InitialPassword+`"}`))
	wrongOrigin.Header.Set("Origin", "https://evil.example")
	wrongResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(wrongResponse, wrongOrigin)
	if wrongResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong origin status = %d", wrongResponse.Code)
	}
}

func loginWebAdmin(t *testing.T, web *Server, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	request := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/login", strings.NewReader(string(encoded)))
	request.Header.Set("Origin", "https://admin.example.test:4001")
	response := httptest.NewRecorder()
	web.Handler().ServeHTTP(response, request)
	return response
}

func findSessionCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatalf("session cookie missing: %v", response.Header())
	return nil
}

func decodeWebEnvelope(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
