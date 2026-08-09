package webadmin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
)

func TestRegistrationManagementListPaginationApproveAndReject(t *testing.T) {
	requestedAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	manager := &webRegistrationManager{pending: []machinereg.Request{
		{RegistrationID: "reg_a", PrincipalID: "user-a", MachineID: "office", AllowedSources: []credential.SourceRule{"127.0.0.1"}, RequestedAt: requestedAt},
		{RegistrationID: "reg_b", PrincipalID: "user-a", MachineID: "mobile", AllowedSources: []credential.SourceRule{"10.0.0.0/24"}, RequestedAt: requestedAt},
	}}
	web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: manager})

	first := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/registrations?limit=1", "")
	var page struct {
		Items         []adminservice.Registration `json:"items"`
		NextPageToken string                      `json:"next_page_token"`
	}
	decodeManagementData(t, first, &page)
	if len(page.Items) != 1 || page.Items[0].RegistrationID != "reg_a" || page.NextPageToken == "" {
		t.Fatalf("first page=%#v", page)
	}
	second := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/registrations?limit=1&page_token="+page.NextPageToken, "")
	page = struct {
		Items         []adminservice.Registration `json:"items"`
		NextPageToken string                      `json:"next_page_token"`
	}{}
	decodeManagementData(t, second, &page)
	if len(page.Items) != 1 || page.Items[0].RegistrationID != "reg_b" || page.NextPageToken != "" {
		t.Fatalf("second page=%#v", page)
	}

	manager.approval = machinereg.ApprovalResult{Request: manager.pending[0], CredentialID: 7}
	approved := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_a/approve", `{}`)
	if approved.Code != http.StatusOK || strings.Contains(approved.Body.String(), "hpk_") || !strings.Contains(approved.Body.String(), `"credential_id":7`) {
		t.Fatalf("status=%d body=%s", approved.Code, approved.Body.String())
	}
	if manager.approveAdmin != "admin" {
		t.Fatalf("approve admin=%q", manager.approveAdmin)
	}

	manager.rejection = machinereg.RejectionResult{Request: manager.pending[1], NotificationSent: false}
	rejected := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_b/reject", `{"reason":"来源范围不符合要求"}`)
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"rejected":true`) || !strings.Contains(rejected.Body.String(), `"notification_sent":false`) {
		t.Fatalf("status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if manager.rejectAdmin != "admin" || manager.rejectReason != "来源范围不符合要求" {
		t.Fatalf("reject admin=%q reason=%q", manager.rejectAdmin, manager.rejectReason)
	}
}

func TestRegistrationManagementRejectsForgeryCSRFAndInvalidBodies(t *testing.T) {
	manager := &webRegistrationManager{}
	web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: manager})
	foreign, err := encodeWebPageToken(credentialPageResource, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/admin/api/v1/registrations?page_token=forged",
		"/admin/api/v1/registrations?page_token=" + foreign,
		"/admin/api/v1/registrations?unknown=value",
	} {
		response := managementRequest(t, web, cookie, csrf, http.MethodGet, target, "")
		assertManagementError(t, response, http.StatusBadRequest, "invalid_pagination")
	}
	missingCSRF := managementRequest(t, web, cookie, "", http.MethodPost, "/admin/api/v1/registrations/reg_one/approve", `{}`)
	assertManagementError(t, missingCSRF, http.StatusForbidden, "csrf_failed")
	badApprove := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_one/approve", `{"unexpected":true}`)
	assertManagementError(t, badApprove, http.StatusBadRequest, "invalid_json")
	badReject := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_one/reject", `{"reason":"ok","extra":true}`)
	assertManagementError(t, badReject, http.StatusBadRequest, "invalid_json")
	wrongMethod := managementRequest(t, web, cookie, csrf, http.MethodDelete, "/admin/api/v1/registrations/reg_one/approve", "")
	assertManagementError(t, wrongMethod, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestRegistrationManagementMapsDomainErrors(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   adminservice.ErrorCode
	}{
		{err: machinereg.ErrRequestNotFound, status: http.StatusNotFound, code: adminservice.CodeRegistrationNotFound},
		{err: machinereg.ErrMachineExists, status: http.StatusConflict, code: adminservice.CodeRegistrationConflict},
		{err: machinereg.ErrDeliveryFailed, status: http.StatusBadGateway, code: adminservice.CodeRegistrationDeliveryFailed},
		{err: &machinereg.OperationError{Kind: machinereg.ErrRollbackFailed, CredentialID: 9, Cause: errors.New("disk/path")}, status: http.StatusInternalServerError, code: adminservice.CodeRegistrationRollbackFailed},
		{err: &machinereg.OperationError{Kind: machinereg.ErrCleanupFailed, CredentialID: 10, Cause: errors.New("disk/path")}, status: http.StatusInternalServerError, code: adminservice.CodeRegistrationCleanupFailed},
	}
	for _, test := range tests {
		manager := &webRegistrationManager{approveErr: test.err}
		web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: manager})
		response := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_one/approve", `{}`)
		assertManagementError(t, response, test.status, string(test.code))
		if strings.Contains(response.Body.String(), "disk/path") || strings.Contains(response.Body.String(), "hpk_") {
			t.Fatalf("response leaked details: %s", response.Body.String())
		}
	}
}

func TestRegistrationManagementRejectMapsDomainErrors(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   adminservice.ErrorCode
	}{
		{err: machinereg.ErrRequestNotFound, status: http.StatusNotFound, code: adminservice.CodeRegistrationNotFound},
		{err: machinereg.ErrInvalidRequest, status: http.StatusBadRequest, code: adminservice.CodeInvalidArgument},
		{err: errors.New("disk/path"), status: http.StatusInternalServerError, code: adminservice.CodeInternal},
	}
	for _, test := range tests {
		manager := &webRegistrationManager{rejectErr: test.err}
		web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: manager})
		response := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/reg_one/reject", `{"reason":"来源不符"}`)
		assertManagementError(t, response, test.status, string(test.code))
		if strings.Contains(response.Body.String(), "disk/path") || strings.Contains(response.Body.String(), "hpk_") {
			t.Fatalf("response leaked details: %s", response.Body.String())
		}
	}
}

func TestRegistrationManagementRequiresAuthentication(t *testing.T) {
	web, _, _, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: &webRegistrationManager{}})
	for _, test := range []struct {
		method string
		target string
		body   string
	}{
		{method: http.MethodGet, target: "/admin/api/v1/registrations"},
		{method: http.MethodPost, target: "/admin/api/v1/registrations/reg_one/approve", body: `{}`},
		{method: http.MethodPost, target: "/admin/api/v1/registrations/reg_one/reject", body: `{"reason":"来源不符"}`},
	} {
		request := newTLSRequest(test.method, test.target, strings.NewReader(test.body))
		if test.method != http.MethodGet {
			request.Header.Set("Origin", "https://admin.example.test:4001")
		}
		response := httptest.NewRecorder()
		web.Handler().ServeHTTP(response, request)
		assertManagementError(t, response, http.StatusUnauthorized, "unauthenticated")
	}
}

func TestRegistrationManagementRejectsInvalidRegistrationID(t *testing.T) {
	web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Registrations: &webRegistrationManager{}})
	for _, registrationID := range []string{"bad%01id", strings.Repeat("a", 257)} {
		response := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/registrations/"+registrationID+"/approve", `{}`)
		assertManagementError(t, response, http.StatusBadRequest, "invalid_registration_id")
	}
}

type webRegistrationManager struct {
	pending      []machinereg.Request
	approval     machinereg.ApprovalResult
	approveErr   error
	approveAdmin string
	rejection    machinereg.RejectionResult
	rejectErr    error
	rejectAdmin  string
	rejectReason string
}

func (manager *webRegistrationManager) ListPending() []machinereg.Request {
	return append([]machinereg.Request(nil), manager.pending...)
}

func (manager *webRegistrationManager) Approve(_ context.Context, _ string, admin string, _ machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error) {
	manager.approveAdmin = admin
	return manager.approval, manager.approveErr
}

func (manager *webRegistrationManager) Reject(_ context.Context, _ string, admin, reason string, _ machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error) {
	manager.rejectAdmin = admin
	manager.rejectReason = reason
	return manager.rejection, manager.rejectErr
}
