package webadmin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
)

type createAdminRequest struct {
	Username string `json:"username"`
}

func (server *Server) registerAdminRoutes(mux *http.ServeMux) {
	server.managementRoute(mux, "/admin/api/v1/administrators", http.MethodGet, http.HandlerFunc(server.listAdministrators), false)
	server.managementRoute(mux, "/admin/api/v1/administrators", http.MethodPost, http.HandlerFunc(server.createAdministrator), true)
	server.managementRoute(mux, "/admin/api/v1/administrators/{username}/reset-password", http.MethodPost, http.HandlerFunc(server.resetAdministratorPassword), true)
	server.managementRoute(mux, "/admin/api/v1/administrators/{username}", http.MethodDelete, http.HandlerFunc(server.deleteAdministrator), true)
	server.managementRoute(mux, "/admin/api/v1/administrators/{username}/token/rotate", http.MethodPost, http.HandlerFunc(server.rotateAdministratorToken), true)
	server.managementRoute(mux, "/admin/api/v1/administrators/{username}/token/enable", http.MethodPost, http.HandlerFunc(server.enableAdministratorToken), true)
	server.managementRoute(mux, "/admin/api/v1/administrators/{username}/token/disable", http.MethodPost, http.HandlerFunc(server.disableAdministratorToken), true)
}

func (server *Server) listAdministrators(writer http.ResponseWriter, request *http.Request) {
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"items": server.auth.ListAdmins(), "observed_at": server.now().UTC()})
}

func (server *Server) createAdministrator(writer http.ResponseWriter, request *http.Request) {
	var input createAdminRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	setRequestTarget(request, "administrator="+safeTargetValue(input.Username))
	created, err := server.auth.CreateAdmin(input.Username)
	if err != nil {
		server.writeAdminAuthError(writer, request, err)
		return
	}
	setRequestTarget(request, "administrator="+created.Admin.Username)
	_ = writeAPIData(writer, request, http.StatusCreated, created)
}

func (server *Server) resetAdministratorPassword(writer http.ResponseWriter, request *http.Request) {
	username, ok := administratorFromPath(writer, request)
	if !ok || !decodeConfirmation(writer, request, "重置管理员密码必须显式确认") {
		return
	}
	password, err := server.auth.ResetPassword(username)
	if err != nil {
		server.writeAdminAuthError(writer, request, err)
		return
	}
	server.sessions.RevokeUser(username)
	identity, _ := browserIdentityFrom(request)
	if identity.Username == username {
		clearSessionCookie(writer)
	}
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"username": username, "initial_password": password})
}

func (server *Server) deleteAdministrator(writer http.ResponseWriter, request *http.Request) {
	username, ok := administratorFromPath(writer, request)
	if !ok || !decodeConfirmation(writer, request, "删除管理员必须显式确认") {
		return
	}
	identity, ok := browserIdentityFrom(request)
	if !ok {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	if identity.Username == username {
		_ = writeAPIError(writer, request, http.StatusForbidden, "cannot_delete_current_admin", "不能删除当前登录管理员")
		return
	}
	if err := server.auth.DeleteAdmin(identity.Username, username); err != nil {
		server.writeAdminAuthError(writer, request, err)
		return
	}
	server.sessions.RevokeUser(username)
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"username": username, "deleted": true})
}

func (server *Server) rotateAdministratorToken(writer http.ResponseWriter, request *http.Request) {
	username, ok := administratorFromPath(writer, request)
	if !ok || !decodeConfirmation(writer, request, "轮换自动化 Token 必须显式确认") {
		return
	}
	token, view, err := server.auth.RotateAutomationToken(username)
	if err != nil {
		server.writeAdminAuthError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"username": username, "automation_token": token, "token": view})
}

func (server *Server) enableAdministratorToken(writer http.ResponseWriter, request *http.Request) {
	server.setAdministratorTokenEnabled(writer, request, true)
}

func (server *Server) disableAdministratorToken(writer http.ResponseWriter, request *http.Request) {
	server.setAdministratorTokenEnabled(writer, request, false)
}

func (server *Server) setAdministratorTokenEnabled(writer http.ResponseWriter, request *http.Request, enabled bool) {
	username, ok := administratorFromPath(writer, request)
	if !ok || !decodeEmptyObject(writer, request) {
		return
	}
	view, err := server.auth.SetAutomationTokenEnabled(username, enabled)
	if err != nil {
		server.writeAdminAuthError(writer, request, err)
		return
	}
	_ = writeAPIData(writer, request, http.StatusOK, map[string]any{"username": username, "token": view})
}

func administratorFromPath(writer http.ResponseWriter, request *http.Request) (string, bool) {
	username := strings.ToLower(strings.TrimSpace(request.PathValue("username")))
	if safeLoginActor(username) == "invalid" {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_username", "管理员用户名无效")
		return "", false
	}
	setRequestTarget(request, "administrator="+username)
	return username, true
}

func decodeConfirmation(writer http.ResponseWriter, request *http.Request, message string) bool {
	var input confirmRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return false
	}
	if !input.Confirm {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "confirmation_required", message)
		return false
	}
	return true
}

func (server *Server) writeAdminAuthError(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, adminauth.ErrInvalidUsername):
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_username", "管理员用户名无效")
	case errors.Is(err, adminauth.ErrAdminExists):
		_ = writeAPIError(writer, request, http.StatusConflict, "admin_exists", "管理员已存在")
	case errors.Is(err, adminauth.ErrAdminNotFound):
		_ = writeAPIError(writer, request, http.StatusNotFound, "admin_not_found", "管理员不存在")
	case errors.Is(err, adminauth.ErrLastAdmin):
		_ = writeAPIError(writer, request, http.StatusConflict, "last_admin", "不能删除最后一个管理员")
	case errors.Is(err, adminauth.ErrInvalidPassword):
		_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_password", "管理员密码无效")
	default:
		server.logger.Error("Web 管理员操作失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
		_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "管理员操作失败")
	}
}
