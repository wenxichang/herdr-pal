package webadmin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPagesRedirectUnauthenticatedUsersAndRenderLogin(t *testing.T) {
	web, _, _ := newTestWebServer(t)
	protected := newTLSRequest(http.MethodGet, "/admin", nil)
	protectedResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(protectedResponse, protected)
	if protectedResponse.Code != http.StatusSeeOther || protectedResponse.Header().Get("Location") != "/admin/login" {
		t.Fatalf("protected status=%d location=%q body=%s", protectedResponse.Code, protectedResponse.Header().Get("Location"), protectedResponse.Body.String())
	}

	login := newTLSRequest(http.MethodGet, "/admin/login", nil)
	loginResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(loginResponse, login)
	body := loginResponse.Body.String()
	if loginResponse.Code != http.StatusOK || !strings.Contains(body, `id="login-form"`) || !strings.Contains(body, `/admin/static/app.js`) {
		t.Fatalf("login status=%d body=%s", loginResponse.Code, body)
	}
	if strings.Contains(body, "<script>") || strings.Contains(body, "cdn.") || strings.Contains(body, "http://") {
		t.Fatalf("login page contains unsafe inline or external asset: %s", body)
	}
}

func TestPagesRenderSevenNavigationEntriesForAuthenticatedAdmin(t *testing.T) {
	web, cookie, _, _ := authenticatedManagementServer(t, webTestDependencies{})
	pages := []string{"/admin", "/admin/credentials", "/admin/connections", "/admin/sessions", "/admin/audit", "/admin/administrators", "/admin/system"}
	for _, target := range pages {
		request := newTLSRequest(http.MethodGet, target, nil)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		web.Handler().ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", target, response.Code, body)
		}
		if strings.Count(body, `class="nav-link`) != 7 || !strings.Contains(body, `data-page=`) || !strings.Contains(body, `id="logout-button"`) {
			t.Fatalf("%s navigation/body invalid: %s", target, body)
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, "localStorage") || strings.Contains(body, "sessionStorage") || strings.Contains(body, "indexedDB") {
			t.Fatalf("%s contains forbidden browser state or inline script", target)
		}
	}
}

func TestPagesForceInitialAdminToPasswordChangeView(t *testing.T) {
	web, bootstrap, _ := newTestWebServer(t)
	login := loginWebAdmin(t, web, "admin", bootstrap.InitialPassword)
	cookie := findSessionCookie(t, login)
	request := newTLSRequest(http.MethodGet, "/admin/credentials", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	web.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `id="password-change-form"`) || strings.Contains(body, `id="credentials-page"`) || strings.Contains(body, `class="nav-link`) {
		t.Fatalf("must-change page status=%d body=%s", response.Code, body)
	}
}

func TestAssetsHaveContentTypesCachePolicyAndNoPersistentAuditStorage(t *testing.T) {
	web, _, _ := newTestWebServer(t)
	tests := []struct {
		path        string
		contentType string
	}{
		{path: "/admin/static/app.css", contentType: "text/css"},
		{path: "/admin/static/app.js", contentType: "text/javascript"},
	}
	for _, test := range tests {
		request := newTLSRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		web.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), test.contentType) || !strings.Contains(response.Header().Get("Cache-Control"), "max-age") || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s status=%d headers=%v", test.path, response.Code, response.Header())
		}
		if test.path == "/admin/static/app.js" {
			body := response.Body.String()
			for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("app.js contains %s", forbidden)
				}
			}
			if !strings.Contains(body, "audit-detail") || !strings.Contains(body, "replaceChildren") {
				t.Fatalf("app.js does not clear audit DOM details")
			}
		}
	}
}
