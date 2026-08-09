package webadmin

import (
	"net/http"
	"strings"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

const registrationPageResource = "registrations"

type registrationRejectRequest struct {
	Reason string `json:"reason"`
}

func (server *Server) registerRegistrationRoutes(mux *http.ServeMux) {
	server.managementRoute(mux, "/admin/api/v1/registrations", http.MethodGet, http.HandlerFunc(server.listRegistrations), false)
	server.managementRoute(mux, "/admin/api/v1/registrations/{id}/approve", http.MethodPost, http.HandlerFunc(server.approveRegistration), true)
	server.managementRoute(mux, "/admin/api/v1/registrations/{id}/reject", http.MethodPost, http.HandlerFunc(server.rejectRegistration), true)
}

func (server *Server) listRegistrations(writer http.ResponseWriter, request *http.Request) {
	limit, anchor, err := parsePagination(request.URL.Query(), registrationPageResource)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "注册申请分页参数无效")
		return
	}
	anchorTime, anchorID, err := parseRegistrationAnchor(anchor)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "注册申请分页游标无效")
		return
	}
	all := server.admin.ListRegistrations()
	items := make([]adminservice.Registration, 0, limit)
	var last adminservice.Registration
	more := false
	for _, item := range all {
		if anchor != "" && registrationAtOrBefore(item, anchorTime, anchorID) {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, item)
		last = item
	}
	result := pageResponse{ObservedAt: server.admin.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodeWebPageToken(registrationPageResource, registrationAnchor(last))
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "生成注册申请分页游标失败")
			return
		}
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) approveRegistration(writer http.ResponseWriter, request *http.Request) {
	registrationID, ok := registrationIDFromPath(writer, request)
	if !ok || !decodeEmptyObject(writer, request) {
		return
	}
	identity, ok := browserIdentityFrom(request)
	if !ok {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	result, err := server.admin.ApproveRegistration(request.Context(), registrationID, identity.Username)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) rejectRegistration(writer http.ResponseWriter, request *http.Request) {
	registrationID, ok := registrationIDFromPath(writer, request)
	if !ok {
		return
	}
	var input registrationRejectRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	identity, ok := browserIdentityFrom(request)
	if !ok {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	result, err := server.admin.RejectRegistration(request.Context(), registrationID, identity.Username, input.Reason)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func registrationIDFromPath(writer http.ResponseWriter, request *http.Request) (string, bool) {
	registrationID := strings.TrimSpace(request.PathValue("id"))
	if registrationID == "" || len(registrationID) > 256 || !validOptionalWebLabel(registrationID) {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_registration_id", "registration_id 无效")
		return "", false
	}
	setRequestTarget(request, "registration_id="+safeTargetValue(registrationID))
	return registrationID, true
}

func registrationAnchor(item adminservice.Registration) string {
	return item.RequestedAt.UTC().Format(time.RFC3339Nano) + "\t" + item.RegistrationID
}

func parseRegistrationAnchor(anchor string) (time.Time, string, error) {
	if anchor == "" {
		return time.Time{}, "", nil
	}
	timestamp, registrationID, found := strings.Cut(anchor, "\t")
	if !found || registrationID == "" || strings.Contains(registrationID, "\t") || !validOptionalWebLabel(registrationID) {
		return time.Time{}, "", errInvalidPagination
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != timestamp {
		return time.Time{}, "", errInvalidPagination
	}
	return parsed.UTC(), registrationID, nil
}

func registrationAtOrBefore(item adminservice.Registration, anchorTime time.Time, anchorID string) bool {
	requestedAt := item.RequestedAt.UTC()
	return requestedAt.Before(anchorTime) || requestedAt.Equal(anchorTime) && item.RegistrationID <= anchorID
}
