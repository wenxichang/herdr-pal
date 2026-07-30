package webadmin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type passwordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type authResponse struct {
	Username           string `json:"username"`
	MustChangePassword bool   `json:"must_change_password"`
	CSRFToken          string `json:"csrf_token"`
}

func (server *Server) registerAuthRoutes(mux *http.ServeMux) {
	server.handleMethod(mux, "/admin/api/v1/auth/login", http.MethodPost, http.HandlerFunc(server.login))
	server.handleMethod(mux, "/admin/api/v1/auth/session", http.MethodGet, server.browserHandler(http.HandlerFunc(server.currentSession), browserPolicy{AllowMustChange: true}))
	server.handleMethod(mux, "/admin/api/v1/auth/logout", http.MethodPost, server.browserHandler(http.HandlerFunc(server.logout), browserPolicy{AllowMustChange: true, RequireCSRF: true}))
	server.handleMethod(mux, "/admin/api/v1/auth/password", http.MethodPost, server.browserHandler(http.HandlerFunc(server.changePassword), browserPolicy{AllowMustChange: true, RequireCSRF: true}))
}

func (server *Server) handleMethod(mux *http.ServeMux, route, method string, handler http.Handler) {
	mux.Handle(route, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		setRequestRoute(request, route)
		if request.Method != method {
			writer.Header().Set("Allow", method)
			_ = writeAPIError(writer, request, http.StatusMethodNotAllowed, "method_not_allowed", "HTTP 方法不受支持")
			return
		}
		handler.ServeHTTP(writer, request)
	}))
}

func (server *Server) login(writer http.ResponseWriter, request *http.Request) {
	setRequestRoute(request, "/admin/api/v1/auth/login")
	if !sameOrigin(request) {
		_ = writeAPIError(writer, request, http.StatusForbidden, "origin_failed", "请求来源无效")
		return
	}
	source, err := credential.RequestSourceAddr(request)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, "source_invalid", "请求来源地址无效")
		return
	}
	var input loginRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	setRequestActor(request, safeLoginActor(input.Username))
	if !server.loginGuard.Allow(input.Username, source) {
		_ = writeAPIError(writer, request, http.StatusTooManyRequests, "login_locked", "登录失败次数过多，请稍后重试")
		return
	}
	admin, err := server.auth.Authenticate(input.Username, input.Password)
	if err != nil {
		server.loginGuard.Failure(input.Username, source)
		if !server.loginGuard.Allow(input.Username, source) {
			_ = writeAPIError(writer, request, http.StatusTooManyRequests, "login_locked", "登录失败次数过多，请稍后重试")
			return
		}
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "invalid_credentials", "用户名或密码错误")
		return
	}
	server.loginGuard.Success(admin.Username, source)
	credentials, err := server.sessions.Create(admin.Username)
	if err != nil {
		server.logger.Error("创建 Web 管理会话失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
		_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "创建管理员会话失败")
		return
	}
	setRequestActor(request, admin.Username)
	setSessionCookie(writer, credentials.ID)
	_ = writeAPIData(writer, request, http.StatusOK, authResponse{
		Username: admin.Username, MustChangePassword: admin.MustChangePassword, CSRFToken: credentials.CSRFToken,
	})
}

func safeLoginActor(username string) string {
	username = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(username, "�")))
	if len(username) < 3 || len(username) > 32 {
		return "invalid"
	}
	for index, character := range username {
		if index == 0 && character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && (character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-') {
			continue
		}
		return "invalid"
	}
	return username
}

func (server *Server) currentSession(writer http.ResponseWriter, request *http.Request) {
	setRequestRoute(request, "/admin/api/v1/auth/session")
	identity, ok := browserIdentityFrom(request)
	if !ok {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	credentials, err := server.sessions.Rotate(identity.SessionID)
	if err != nil {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	setSessionCookie(writer, credentials.ID)
	_ = writeAPIData(writer, request, http.StatusOK, authResponse{
		Username: identity.Username, MustChangePassword: identity.Admin.MustChangePassword, CSRFToken: credentials.CSRFToken,
	})
}

func (server *Server) logout(writer http.ResponseWriter, request *http.Request) {
	setRequestRoute(request, "/admin/api/v1/auth/logout")
	var input struct{}
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	identity, _ := browserIdentityFrom(request)
	server.sessions.Delete(identity.SessionID)
	clearSessionCookie(writer)
	_ = writeAPIData(writer, request, http.StatusOK, map[string]bool{"logged_out": true})
}

func (server *Server) changePassword(writer http.ResponseWriter, request *http.Request) {
	setRequestRoute(request, "/admin/api/v1/auth/password")
	var input passwordRequest
	if apiErr := decodeJSON(writer, request, &input); apiErr != nil {
		_ = writeAPIError(writer, request, http.StatusBadRequest, apiErr.Code, apiErr.Message)
		return
	}
	identity, ok := browserIdentityFrom(request)
	if !ok {
		_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
		return
	}
	if err := server.auth.ChangePassword(identity.Username, input.CurrentPassword, input.NewPassword); err != nil {
		switch {
		case errors.Is(err, adminauth.ErrAuthenticationFailed):
			_ = writeAPIError(writer, request, http.StatusUnauthorized, "invalid_credentials", "当前密码错误")
		case errors.Is(err, adminauth.ErrInvalidPassword):
			_ = writeAPIError(writer, request, http.StatusBadRequest, "invalid_password", "新密码长度必须为 12 至 256 字节")
		default:
			server.logger.Error("修改 Web 管理员密码失败", "request_id", requestIDFrom(request), "actor", identity.Username, "error_type", safeHandlerError(err))
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "修改管理员密码失败")
		}
		return
	}
	credentials, err := server.sessions.Rotate(identity.SessionID)
	if err != nil {
		server.sessions.RevokeUser(identity.Username)
		clearSessionCookie(writer)
		_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "更新管理员会话失败，请重新登录")
		return
	}
	server.sessions.RevokeUserExcept(identity.Username, credentials.ID)
	setSessionCookie(writer, credentials.ID)
	_ = writeAPIData(writer, request, http.StatusOK, authResponse{
		Username: identity.Username, MustChangePassword: false, CSRFToken: credentials.CSRFToken,
	})
}

func setSessionCookie(writer http.ResponseWriter, value string) {
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/admin",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/admin",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: -1,
	})
}
