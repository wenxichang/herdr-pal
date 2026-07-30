package webadmin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/lokiquery"
)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 500
	defaultAuditRange = 24 * time.Hour
	maxAuditRange     = 31 * 24 * time.Hour
)

var auditQueryParameters = map[string]struct{}{
	"userid": {}, "machine_id": {}, "keyword": {}, "start": {}, "end": {}, "limit": {}, "cursor": {},
}

func (server *Server) registerAuditRoutes(mux *http.ServeMux) {
	server.managementRoute(mux, "/admin/api/v1/audit/logs", http.MethodGet, http.HandlerFunc(server.auditLogs), false)
}

func (server *Server) auditLogs(writer http.ResponseWriter, request *http.Request) {
	query, err := parseAuditQuery(request.URL.Query(), server.now().UTC())
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_audit_query", "审计日志查询参数无效")
		return
	}
	setRequestTarget(request, fmt.Sprintf(
		"userid_present=%t machine_id_present=%t keyword_present=%t keyword_length=%d",
		query.PrincipalID != "", query.MachineID != "", query.Keyword != "", utf8.RuneCountInString(query.Keyword),
	))
	if server.audit == nil {
		_ = writeAPIError(writer, request, http.StatusBadGateway, "audit_unavailable", "审计日志服务未配置或暂不可用")
		return
	}
	page, err := server.audit.Query(request.Context(), query)
	if err != nil {
		if errors.Is(err, lokiquery.ErrInvalidQuery) {
			_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_audit_query", "审计日志查询参数无效")
			return
		}
		server.logger.Warn("Web 审计日志查询失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
		_ = writeAPIError(writer, request, http.StatusBadGateway, "audit_unavailable", "审计日志服务未配置或暂不可用")
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, page)
}

func parseAuditQuery(values url.Values, now time.Time) (lokiquery.Query, error) {
	for name, entries := range values {
		if _, ok := auditQueryParameters[name]; !ok || len(entries) != 1 {
			return lokiquery.Query{}, lokiquery.ErrInvalidQuery
		}
	}
	query := lokiquery.Query{
		PrincipalID: values.Get("userid"),
		MachineID:   values.Get("machine_id"),
		Keyword:     values.Get("keyword"),
		Cursor:      values.Get("cursor"),
		Limit:       defaultAuditLimit,
	}
	if err := lokiquery.ValidateFilters(query); err != nil {
		return lokiquery.Query{}, lokiquery.ErrInvalidQuery
	}
	var err error
	if raw := strings.TrimSpace(values.Get("end")); raw != "" {
		query.End, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return lokiquery.Query{}, lokiquery.ErrInvalidQuery
		}
	} else {
		query.End = now
	}
	if raw := strings.TrimSpace(values.Get("start")); raw != "" {
		query.Start, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return lokiquery.Query{}, lokiquery.ErrInvalidQuery
		}
	} else {
		query.Start = query.End.Add(-defaultAuditRange)
	}
	query.Start = query.Start.UTC()
	query.End = query.End.UTC()
	if !query.Start.Before(query.End) || query.End.Sub(query.Start) > maxAuditRange {
		return lokiquery.Query{}, lokiquery.ErrInvalidQuery
	}
	if raw, exists := values["limit"]; exists {
		query.Limit, err = strconv.Atoi(raw[0])
		if err != nil || query.Limit < 1 || query.Limit > maxAuditLimit {
			return lokiquery.Query{}, lokiquery.ErrInvalidQuery
		}
	}
	return query, nil
}
