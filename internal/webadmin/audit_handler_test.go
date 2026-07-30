package webadmin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/lokiquery"
)

func TestAuditLogsParsesControlledFiltersAndDoesNotLogKeyword(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	querier := &auditQuerier{page: lokiquery.Page{Items: []lokiquery.Entry{{
		Timestamp: now.Add(-time.Minute), PrincipalID: "user-a", MachineID: "home", Body: "Sensitive Error",
	}}}}
	web, cookie, _, logs := authenticatedManagementServer(t, webTestDependencies{Audit: querier, Now: func() time.Time { return now }})
	response := managementRequest(t, web, cookie, "", http.MethodGet, "/admin/api/v1/audit/logs?userid=user-a&machine_id=HOME&keyword=Sensitive%20Error&limit=25", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var page lokiquery.Page
	decodeManagementData(t, response, &page)
	if len(page.Items) != 1 || page.Items[0].Body != "Sensitive Error" {
		t.Fatalf("page = %#v", page)
	}
	want := lokiquery.Query{
		PrincipalID: "user-a", MachineID: "HOME", Keyword: "Sensitive Error",
		Start: now.Add(-24 * time.Hour), End: now, Limit: 25,
	}
	if querier.query != want {
		t.Fatalf("query = %#v, want %#v", querier.query, want)
	}
	if strings.Contains(logs.String(), "Sensitive Error") || !strings.Contains(logs.String(), "keyword_present=true") || !strings.Contains(logs.String(), "keyword_length=15") {
		t.Fatalf("management logs = %s", logs.String())
	}
}

func TestAuditLogsAcceptsRFC3339NanoAndCursor(t *testing.T) {
	querier := &auditQuerier{page: lokiquery.Page{NextCursor: "next"}}
	web, cookie, _, _ := authenticatedManagementServer(t, webTestDependencies{Audit: querier})
	start := "2026-07-01T01:02:03Z"
	end := "2026-07-02T04:05:06.123456789Z"
	response := managementRequest(t, web, cookie, "", http.MethodGet, "/admin/api/v1/audit/logs?start="+start+"&end="+end+"&cursor=cursor-value", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if querier.query.Start.Format(time.RFC3339Nano) != start || querier.query.End.Format(time.RFC3339Nano) != end || querier.query.Cursor != "cursor-value" || querier.query.Limit != 100 {
		t.Fatalf("query = %#v", querier.query)
	}
}

func TestAuditLogsRejectsInvalidQueryParameters(t *testing.T) {
	web, cookie, _, _ := authenticatedManagementServer(t, webTestDependencies{Audit: &auditQuerier{}})
	targets := []string{
		"/admin/api/v1/audit/logs?start=not-time",
		"/admin/api/v1/audit/logs?start=2026-07-02T00:00:00Z&end=2026-07-01T00:00:00Z",
		"/admin/api/v1/audit/logs?start=2026-06-01T00:00:00Z&end=2026-07-30T00:00:00Z",
		"/admin/api/v1/audit/logs?limit=0",
		"/admin/api/v1/audit/logs?limit=501",
		"/admin/api/v1/audit/logs?userid=a&userid=b",
		"/admin/api/v1/audit/logs?unknown=value",
		"/admin/api/v1/audit/logs?userid=" + strings.Repeat("u", lokiquery.MaxPrincipalIDBytes+1),
		"/admin/api/v1/audit/logs?machine_id=" + strings.Repeat("m", lokiquery.MaxMachineIDBytes+1),
		"/admin/api/v1/audit/logs?keyword=" + strings.Repeat("k", lokiquery.MaxKeywordBytes+1),
	}
	for _, target := range targets {
		response := managementRequest(t, web, cookie, "", http.MethodGet, target, "")
		assertManagementError(t, response, http.StatusBadRequest, "invalid_audit_query")
	}
}

func TestAuditLogsReturnsBadGatewayWhenLokiIsMissingOrFails(t *testing.T) {
	tests := []struct {
		name  string
		audit AuditQuerier
	}{
		{name: "not configured"},
		{name: "query failure", audit: &auditQuerier{err: errors.New("sensitive loki response")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			web, cookie, _, logs := authenticatedManagementServer(t, webTestDependencies{Audit: test.audit})
			response := managementRequest(t, web, cookie, "", http.MethodGet, "/admin/api/v1/audit/logs?keyword=secret-search", "")
			assertManagementError(t, response, http.StatusBadGateway, "audit_unavailable")
			if strings.Contains(response.Body.String(), "sensitive") || strings.Contains(logs.String(), "sensitive loki response") || strings.Contains(logs.String(), "secret-search") {
				t.Fatalf("leaked Loki detail: body=%s logs=%s", response.Body.String(), logs.String())
			}
		})
	}
}

func TestAuditLogsMapsInvalidClientQueryToBadRequest(t *testing.T) {
	web, cookie, _, _ := authenticatedManagementServer(t, webTestDependencies{Audit: &auditQuerier{err: lokiquery.ErrInvalidQuery}})
	response := managementRequest(t, web, cookie, "", http.MethodGet, "/admin/api/v1/audit/logs?cursor=bad-cursor", "")
	assertManagementError(t, response, http.StatusBadRequest, "invalid_audit_query")
}

type auditQuerier struct {
	query lokiquery.Query
	page  lokiquery.Page
	err   error
}

func (querier *auditQuerier) Query(_ context.Context, query lokiquery.Query) (lokiquery.Page, error) {
	querier.query = query
	return querier.page, querier.err
}
