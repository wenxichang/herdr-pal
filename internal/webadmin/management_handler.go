package webadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/hprp"
)

const (
	credentialPageResource = "credentials"
	connectionPageResource = "connections"
	sessionPageResource    = "sessions"
)

var statusByCode = map[adminservice.ErrorCode]int{
	adminservice.CodeInvalidArgument:    http.StatusBadRequest,
	adminservice.CodeCredentialNotFound: http.StatusNotFound,
	adminservice.CodeCredentialConflict: http.StatusConflict,
	adminservice.CodeSourceRequired:     http.StatusBadRequest,
	adminservice.CodeSourceInvalid:      http.StatusBadRequest,
	adminservice.CodeConnectionNotFound: http.StatusNotFound,
	adminservice.CodeServerBusy:         http.StatusConflict,
	adminservice.CodeInternal:           http.StatusInternalServerError,
}

type credentialIssueRequest struct {
	PrincipalID string   `json:"principal_id"`
	MachineID   string   `json:"machine_id"`
	Sources     []string `json:"sources"`
	ExpiresAt   *string  `json:"expires_at,omitempty"`
}

type sourceMutationRequest struct {
	Sources []string `json:"sources"`
}

type confirmRequest struct {
	Confirm bool `json:"confirm"`
}

type debugRequest struct {
	Enabled *bool `json:"enabled"`
}

type pageResponse struct {
	ObservedAt    time.Time `json:"observed_at"`
	Items         any       `json:"items"`
	NextPageToken string    `json:"next_page_token,omitempty"`
}

func (server *Server) registerManagementRoutes(mux *http.ServeMux) {
	server.managementRoute(mux, "/admin/api/v1/overview", http.MethodGet, http.HandlerFunc(server.overview), false)
	server.managementRoute(mux, "/admin/api/v1/credentials", http.MethodGet, http.HandlerFunc(server.listCredentials), false)
	server.managementRoute(mux, "/admin/api/v1/credentials", http.MethodPost, http.HandlerFunc(server.issueCredential), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}", http.MethodGet, http.HandlerFunc(server.showCredential), false)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}", http.MethodDelete, http.HandlerFunc(server.deleteCredential), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/enable", http.MethodPost, http.HandlerFunc(server.enableCredential), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/disable", http.MethodPost, http.HandlerFunc(server.disableCredential), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/sources", http.MethodGet, http.HandlerFunc(server.listCredentialSources), false)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/sources", http.MethodPost, http.HandlerFunc(server.addCredentialSources), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/sources", http.MethodPut, http.HandlerFunc(server.setCredentialSources), true)
	server.managementRoute(mux, "/admin/api/v1/credentials/{id}/sources", http.MethodDelete, http.HandlerFunc(server.removeCredentialSources), true)
	server.managementRoute(mux, "/admin/api/v1/connections", http.MethodGet, http.HandlerFunc(server.listConnections), false)
	server.managementRoute(mux, "/admin/api/v1/connections/{id}", http.MethodGet, http.HandlerFunc(server.showConnection), false)
	server.managementRoute(mux, "/admin/api/v1/connections/{id}/disconnect", http.MethodPost, http.HandlerFunc(server.disconnectConnection), true)
	server.managementRoute(mux, "/admin/api/v1/sessions", http.MethodGet, http.HandlerFunc(server.listSessions), false)
	server.managementRoute(mux, "/admin/api/v1/server/status", http.MethodGet, http.HandlerFunc(server.serverStatus), false)
	server.managementRoute(mux, "/admin/api/v1/server/debug", http.MethodPost, http.HandlerFunc(server.setServerDebug), true)
	server.managementRoute(mux, "/admin/api/v1/server/stop", http.MethodPost, http.HandlerFunc(server.stopServer), true)
}

func (server *Server) managementRoute(mux *http.ServeMux, route, method string, handler http.Handler, requireCSRF bool) {
	server.handleMethod(mux, route, method, server.browserHandler(handler, browserPolicy{RequireCSRF: requireCSRF}))
}

func (server *Server) overview(writer http.ResponseWriter, request *http.Request) {
	_ = writeAPIData(writer, request, http.StatusOK, server.admin.Status())
}

func (server *Server) listCredentials(writer http.ResponseWriter, request *http.Request) {
	limit, anchor, err := parsePagination(request.URL.Query(), credentialPageResource)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "凭据分页参数无效")
		return
	}
	var anchorID uint64
	if anchor != "" {
		anchorID, err = strconv.ParseUint(anchor, 10, 64)
		if err != nil || anchorID == 0 {
			_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "凭据分页游标无效")
			return
		}
	}
	all := server.admin.ListCredentials()
	items := make([]adminservice.Credential, 0, limit)
	var lastID uint64
	more := false
	for _, item := range all {
		if item.CredentialID <= anchorID {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, item)
		lastID = item.CredentialID
	}
	result := pageResponse{ObservedAt: server.admin.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodeWebPageToken(credentialPageResource, strconv.FormatUint(lastID, 10))
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "生成凭据分页游标失败")
			return
		}
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) issueCredential(writer http.ResponseWriter, request *http.Request) {
	var input credentialIssueRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	var expiresAt *time.Time
	if input.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, *input.ExpiresAt)
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_expiry", "expires_at 必须是 RFC3339 时间")
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}
	setRequestTarget(request, "machine_id="+safeTargetValue(input.MachineID))
	result, err := server.admin.IssueCredential(adminservice.IssueCredentialInput{
		PrincipalID: input.PrincipalID, MachineID: input.MachineID, Sources: input.Sources, ExpiresAt: expiresAt,
	})
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	setRequestTarget(request, fmt.Sprintf("credential_id=%d", result.Credential.CredentialID))
	_ = writeAPIData(writer, request, http.StatusCreated, result)
}

func (server *Server) showCredential(writer http.ResponseWriter, request *http.Request) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok {
		return
	}
	result, err := server.admin.ShowCredential(id)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) enableCredential(writer http.ResponseWriter, request *http.Request) {
	server.setCredentialEnabled(writer, request, true)
}

func (server *Server) disableCredential(writer http.ResponseWriter, request *http.Request) {
	server.setCredentialEnabled(writer, request, false)
}

func (server *Server) setCredentialEnabled(writer http.ResponseWriter, request *http.Request, enabled bool) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok || !decodeEmptyObject(writer, request) {
		return
	}
	result, err := server.admin.SetCredentialEnabled(id, enabled)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) deleteCredential(writer http.ResponseWriter, request *http.Request) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok {
		return
	}
	var input confirmRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	if !input.Confirm {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "confirmation_required", "删除凭据必须显式确认")
		return
	}
	result, err := server.admin.DeleteCredential(id)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) listCredentialSources(writer http.ResponseWriter, request *http.Request) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok {
		return
	}
	sources, err := server.admin.ListSources(id)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"credential_id": id, "sources": sources})
}

func (server *Server) addCredentialSources(writer http.ResponseWriter, request *http.Request) {
	server.mutateCredentialSources(writer, request, adminservice.SourceAdd)
}

func (server *Server) setCredentialSources(writer http.ResponseWriter, request *http.Request) {
	server.mutateCredentialSources(writer, request, adminservice.SourceSet)
}

func (server *Server) removeCredentialSources(writer http.ResponseWriter, request *http.Request) {
	server.mutateCredentialSources(writer, request, adminservice.SourceRemove)
}

func (server *Server) mutateCredentialSources(writer http.ResponseWriter, request *http.Request, operation adminservice.SourceOperation) {
	id, ok := credentialIDFromPath(writer, request)
	if !ok {
		return
	}
	var input sourceMutationRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	result, err := server.admin.MutateSources(id, operation, input.Sources)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) listConnections(writer http.ResponseWriter, request *http.Request) {
	limit, anchor, err := parsePagination(request.URL.Query(), connectionPageResource)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "连接分页参数无效")
		return
	}
	all := server.admin.ListConnections()
	items := make([]adminservice.Connection, 0, limit)
	lastKey := ""
	more := false
	for _, item := range all {
		key := webConnectionSortKey(item)
		if anchor != "" && key <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, item)
		lastKey = key
	}
	result := pageResponse{ObservedAt: server.admin.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodeWebPageToken(connectionPageResource, lastKey)
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "生成连接分页游标失败")
			return
		}
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) showConnection(writer http.ResponseWriter, request *http.Request) {
	id, ok := connectionIDFromPath(writer, request)
	if !ok {
		return
	}
	result, err := server.admin.ShowConnection(id)
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) disconnectConnection(writer http.ResponseWriter, request *http.Request) {
	id, ok := connectionIDFromPath(writer, request)
	if !ok {
		return
	}
	var input confirmRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	if !input.Confirm {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "confirmation_required", "断开连接必须显式确认")
		return
	}
	if err := server.admin.DisconnectConnection(id); err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"connection_id": id, "disconnected": true, "observed_at": server.admin.ObservedAt()})
}

func (server *Server) listSessions(writer http.ResponseWriter, request *http.Request) {
	limit, anchor, err := parsePagination(request.URL.Query(), sessionPageResource, "userid", "machine_id")
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_pagination", "会话分页参数无效")
		return
	}
	principalID := request.URL.Query().Get("userid")
	machineID := request.URL.Query().Get("machine_id")
	if !validOptionalWebLabel(principalID) || machineID != "" && hprp.ValidateMachineID(machineID) != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_filter", "会话过滤参数无效")
		return
	}
	targetParts := make([]string, 0, 2)
	if principalID != "" {
		targetParts = append(targetParts, "userid_hash="+shortTargetHash(principalID))
	}
	if machineID != "" {
		targetParts = append(targetParts, "machine_id="+safeTargetValue(machineID))
	}
	setRequestTarget(request, strings.Join(targetParts, " "))
	all := server.admin.ListSessions(adminservice.SessionFilter{PrincipalID: principalID, MachineID: machineID})
	items := make([]adminservice.Session, 0, limit)
	lastKey := ""
	more := false
	for _, item := range all {
		key := webSessionSortKey(item)
		if anchor != "" && key <= anchor {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, item)
		lastKey = key
	}
	result := pageResponse{ObservedAt: server.admin.ObservedAt(), Items: items}
	if more {
		result.NextPageToken, err = encodeWebPageToken(sessionPageResource, lastKey)
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "生成会话分页游标失败")
			return
		}
	}
	_ = writeAPIData(writer, request, http.StatusOK, result)
}

func (server *Server) serverStatus(writer http.ResponseWriter, request *http.Request) {
	_ = writeAPIData(writer, request, http.StatusOK, server.admin.Status())
}

func (server *Server) setServerDebug(writer http.ResponseWriter, request *http.Request) {
	var input debugRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	if input.Enabled == nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_debug_state", "enabled 字段不能为空")
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, server.admin.SetDebug(*input.Enabled))
}

func (server *Server) stopServer(writer http.ResponseWriter, request *http.Request) {
	var input confirmRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	if !input.Confirm {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "confirmation_required", "停止服务必须显式确认")
		return
	}
	action, err := server.admin.PrepareStop()
	if err != nil {
		server.writeServiceError(writer, request, err)
		return
	}
	if err := writeAPIData(writer, request, http.StatusOK, map[string]bool{"stopping": true}); err != nil {
		action.Rollback()
		server.logger.Error("写出 Web 停止响应失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
		return
	}
	action.Commit()
}

func (server *Server) writeServiceError(writer http.ResponseWriter, request *http.Request, err error) {
	code := adminservice.ErrorCodeOf(err)
	status := statusByCode[code]
	if status == 0 {
		status = http.StatusInternalServerError
		code = adminservice.CodeInternal
	}
	message := err.Error()
	if code == adminservice.CodeInternal {
		message = "管理操作失败"
		server.logger.Error("Web 管理操作失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
	}
	_ = writeAPIError(writer, request, status, string(code), message)
}

func credentialIDFromPath(writer http.ResponseWriter, request *http.Request) (uint64, bool) {
	id, err := strconv.ParseUint(request.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_credential_id", "credential_id 无效")
		return 0, false
	}
	setRequestTarget(request, fmt.Sprintf("credential_id=%d", id))
	return id, true
}

func connectionIDFromPath(writer http.ResponseWriter, request *http.Request) (string, bool) {
	id := strings.TrimSpace(request.PathValue("id"))
	if id == "" || len(id) > 256 {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_connection_id", "connection_id 无效")
		return "", false
	}
	setRequestTarget(request, "connection_id="+safeTargetValue(id))
	return id, true
}

func decodeEmptyObject(writer http.ResponseWriter, request *http.Request) bool {
	var input struct{}
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return false
	}
	return true
}

func webConnectionSortKey(view adminservice.Connection) string {
	return view.PrincipalID + "\x00" + view.MachineID + "\x00" + view.ConnectionID
}

func webSessionSortKey(view adminservice.Session) string {
	return fmt.Sprintf("%s\x00%020d\x00%s\x00%s\x00%s", view.PrincipalID, view.Number, view.Target.MachineID, view.Target.SlotID, view.Target.SessionID)
}

func validOptionalWebLabel(value string) bool {
	if value == "" {
		return true
	}
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) || len(value) > hprp.MaxLabelBytes {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func safeTargetValue(value string) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(strings.ToValidUTF8(value, "�"))
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return strings.ToValidUTF8(value[:128], "�") + "…"
	}
	return value
}

func shortTargetHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}
