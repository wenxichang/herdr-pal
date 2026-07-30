package webadmin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
)

func TestAdministratorLifecycleReturnsSecretsOnceAndRevokesSessions(t *testing.T) {
	web, cookie, csrf, logs := authenticatedManagementServer(t, webTestDependencies{})
	createdResponse := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/administrators", `{"username":"Ops.Team"}`)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created adminauth.CreatedAdmin
	decodeManagementData(t, createdResponse, &created)
	if created.Admin.Username != "ops.team" || created.InitialPassword == "" || created.AutomationToken == "" {
		t.Fatalf("created = %#v", created)
	}
	operatorSession, err := web.sessions.Create("ops.team")
	if err != nil {
		t.Fatal(err)
	}
	resetResponse := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/administrators/ops.team/reset-password", `{"confirm":true}`)
	if resetResponse.Code != http.StatusOK {
		t.Fatalf("reset status=%d body=%s", resetResponse.Code, resetResponse.Body.String())
	}
	var reset struct {
		Username        string `json:"username"`
		InitialPassword string `json:"initial_password"`
	}
	decodeManagementData(t, resetResponse, &reset)
	if reset.InitialPassword == "" || reset.InitialPassword == created.InitialPassword {
		t.Fatalf("reset = %#v", reset)
	}
	if _, ok := web.sessions.Get(operatorSession.ID); ok {
		t.Fatal("password reset did not revoke target sessions")
	}
	if _, err := web.auth.VerifyAutomationBearer(created.AutomationToken); err != nil {
		t.Fatalf("password reset rotated token: %v", err)
	}

	rotateResponse := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/administrators/ops.team/token/rotate", `{"confirm":true}`)
	if rotateResponse.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotateResponse.Code, rotateResponse.Body.String())
	}
	var rotated struct {
		AutomationToken string                    `json:"automation_token"`
		Token           adminauth.AutomationToken `json:"token"`
	}
	decodeManagementData(t, rotateResponse, &rotated)
	if rotated.AutomationToken == "" || rotated.AutomationToken == created.AutomationToken || !rotated.Token.Enabled {
		t.Fatalf("rotated = %#v", rotated)
	}
	if _, err := web.auth.VerifyAutomationBearer(created.AutomationToken); !errors.Is(err, adminauth.ErrAuthenticationFailed) {
		t.Fatalf("old token error = %v", err)
	}
	for _, test := range []struct {
		path    string
		enabled bool
	}{
		{"/admin/api/v1/administrators/ops.team/token/disable", false},
		{"/admin/api/v1/administrators/ops.team/token/enable", true},
	} {
		response := managementRequest(t, web, cookie, csrf, http.MethodPost, test.path, `{}`)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		admin, err := web.auth.Admin("ops.team")
		if err != nil || admin.AutomationToken.Enabled != test.enabled {
			t.Fatalf("admin = %#v, %v", admin, err)
		}
	}

	listResponse := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/administrators", "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	for _, secret := range []string{created.InitialPassword, created.AutomationToken, reset.InitialPassword, rotated.AutomationToken} {
		if strings.Contains(listResponse.Body.String(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("administrator list or log leaked secret")
		}
	}

	currentDelete := managementRequest(t, web, cookie, csrf, http.MethodDelete, "/admin/api/v1/administrators/admin", `{"confirm":true}`)
	assertManagementError(t, currentDelete, http.StatusForbidden, "cannot_delete_current_admin")
	deleteSession, err := web.sessions.Create("ops.team")
	if err != nil {
		t.Fatal(err)
	}
	deleted := managementRequest(t, web, cookie, csrf, http.MethodDelete, "/admin/api/v1/administrators/ops.team", `{"confirm":true}`)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if _, ok := web.sessions.Get(deleteSession.ID); ok {
		t.Fatal("administrator deletion did not revoke target sessions")
	}
	if _, err := web.auth.Admin("ops.team"); !errors.Is(err, adminauth.ErrAdminNotFound) {
		t.Fatalf("deleted admin error = %v", err)
	}
}
