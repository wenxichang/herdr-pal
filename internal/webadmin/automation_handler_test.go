package webadmin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

func TestAutomationAuthenticationCredentialLifecycleAndTokenState(t *testing.T) {
	connections := &managementConnections{}
	web, bootstrap, logs := newTestWebServerWithDependencies(t, webTestDependencies{Connections: connections})
	missing := automationRequest(t, web, "", http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	assertManagementError(t, missing, http.StatusUnauthorized, "unauthenticated")
	wrong := automationRequest(t, web, "hpa_0000000000000000_invalid", http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	assertManagementError(t, wrong, http.StatusUnauthorized, "unauthenticated")

	issuedResponse := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{"principal_id":"user-a","machine_id":"home","sources":["127.0.0.1"]}`)
	if issuedResponse.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", issuedResponse.Code, issuedResponse.Body.String())
	}
	var issued adminservice.CredentialIssueResult
	decodeManagementData(t, issuedResponse, &issued)
	if issued.Credential.CredentialID != 1 || strings.Contains(logs.String(), issued.Token) || strings.Contains(logs.String(), bootstrap.AutomationToken) {
		t.Fatalf("issued=%#v logs=%s", issued, logs.String())
	}
	duplicate := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{"principal_id":"user-a","machine_id":"home","sources":["127.0.0.1"]}`)
	assertManagementError(t, duplicate, http.StatusConflict, string(adminservice.CodeCredentialConflict))
	emptySources := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{"principal_id":"user-a","machine_id":"office","sources":[]}`)
	assertManagementError(t, emptySources, http.StatusBadRequest, string(adminservice.CodeSourceRequired))
	deleted := automationRequest(t, web, bootstrap.AutomationToken, http.MethodDelete, "/admin/api/v1/automation/credentials/1", "")
	if deleted.Code != http.StatusOK || connections.credentialDisconnects != 1 {
		t.Fatalf("delete status=%d disconnects=%d body=%s", deleted.Code, connections.credentialDisconnects, deleted.Body.String())
	}

	if _, err := web.auth.SetAutomationTokenEnabled("admin", false); err != nil {
		t.Fatal(err)
	}
	disabled := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	assertManagementError(t, disabled, http.StatusUnauthorized, "unauthenticated")
	if _, err := web.auth.SetAutomationTokenEnabled("admin", true); err != nil {
		t.Fatal(err)
	}
	rotatedToken, _, err := web.auth.RotateAutomationToken("admin")
	if err != nil {
		t.Fatal(err)
	}
	old := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	assertManagementError(t, old, http.StatusUnauthorized, "unauthenticated")
	newToken := automationRequest(t, web, rotatedToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	if newToken.Code != http.StatusBadRequest {
		t.Fatalf("rotated token status=%d body=%s", newToken.Code, newToken.Body.String())
	}
}

func TestAutomationRateLimitsPerSecondAndRollingMinute(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	web, bootstrap, _ := newTestWebServerWithDependencies(t, webTestDependencies{Now: func() time.Time { return now }})
	for attempt := 1; attempt <= 6; attempt++ {
		response := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
		if attempt <= automationPerSecond && response.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d status=%d", attempt, response.Code)
		}
		if attempt == automationPerSecond+1 {
			assertManagementError(t, response, http.StatusTooManyRequests, "rate_limited")
		}
	}

	web, bootstrap, _ = newTestWebServerWithDependencies(t, webTestDependencies{Now: func() time.Time { return now }})
	for batch := 0; batch < 20; batch++ {
		for requestIndex := 0; requestIndex < automationPerSecond; requestIndex++ {
			response := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("batch=%d request=%d status=%d body=%s", batch, requestIndex, response.Code, response.Body.String())
			}
		}
		now = now.Add(time.Second)
	}
	response := automationRequest(t, web, bootstrap.AutomationToken, http.MethodPost, "/admin/api/v1/automation/credentials", `{}`)
	assertManagementError(t, response, http.StatusTooManyRequests, "rate_limited")
}

func automationRequest(t *testing.T, web *Server, token, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := newTLSRequest(method, target, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	web.Handler().ServeHTTP(response, request)
	return response
}
