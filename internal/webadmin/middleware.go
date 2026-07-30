package webadmin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/credential"
)

type requestContextKey struct{}
type browserContextKey struct{}

type requestMetadata struct {
	requestID string
	actor     string
	route     string
	target    string
	errorCode string
}

type browserIdentity struct {
	Username  string
	SessionID string
	Admin     adminauth.Admin
}

type browserPolicy struct {
	AllowMustChange bool
	RequireCSRF     bool
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (writer *statusWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Write(value []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(value)
}

func (server *Server) middleware(next http.Handler) http.Handler {
	handler := next
	handler = server.managementLog(handler)
	handler = securityHeaders(handler)
	handler = requireHTTPS(handler)
	handler = server.requestID(handler)
	handler = server.recoverPanic(handler)
	return handler
}

func (server *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer func() {
			if recover() == nil {
				return
			}
			server.logger.Error("Web 管理请求发生 panic", "request_id", requestIDFrom(request), "error_type", "panic")
			_ = writeAPIError(writer, request, http.StatusInternalServerError, "internal", "管理服务内部错误")
		}()
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		metadata := &requestMetadata{requestID: server.nextRequestID(), route: request.URL.Path}
		writer.Header().Set("X-Request-ID", metadata.requestID)
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), requestContextKey{}, metadata)))
	})
}

func requireHTTPS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.TLS == nil {
			_ = writeAPIError(writer, request, http.StatusBadRequest, "https_required", "Web 管理台只接受 HTTPS 请求")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "same-origin")
		writer.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) managementLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		startedAt := server.now()
		captured := &statusWriter{ResponseWriter: writer}
		next.ServeHTTP(captured, request)
		status := captured.status
		if status == 0 {
			status = http.StatusOK
		}
		metadata := requestMetadataFrom(request)
		source := "unknown"
		if address, err := credential.RequestSourceAddr(request); err == nil {
			source = address.String()
		}
		outcome := "success"
		if status >= http.StatusBadRequest {
			outcome = "error"
		}
		server.logger.Info("Web 管理请求",
			"request_id", metadata.requestID,
			"actor", metadata.actor,
			"source_ip", source,
			"method", request.Method,
			"route", metadata.route,
			"target", metadata.target,
			"outcome", outcome,
			"error_code", metadata.errorCode,
			"status", status,
			"duration_ms", server.now().Sub(startedAt).Milliseconds(),
		)
	})
}

func (server *Server) browserHandler(next http.Handler, policy browserPolicy) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		cookie, err := request.Cookie(sessionCookieName)
		if err != nil {
			_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
			return
		}
		session, ok := server.sessions.Get(cookie.Value)
		if !ok {
			_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
			return
		}
		admin, err := server.auth.Admin(session.Username)
		if err != nil {
			server.sessions.Delete(cookie.Value)
			_ = writeAPIError(writer, request, http.StatusUnauthorized, "unauthenticated", "管理员会话无效")
			return
		}
		setRequestActor(request, admin.Username)
		if admin.MustChangePassword && !policy.AllowMustChange {
			_ = writeAPIError(writer, request, http.StatusForbidden, "password_change_required", "请先修改初始密码")
			return
		}
		if policy.RequireCSRF {
			if !sameOrigin(request) || !server.sessions.VerifyCSRF(cookie.Value, request.Header.Get("X-CSRF-Token")) {
				_ = writeAPIError(writer, request, http.StatusForbidden, "csrf_failed", "请求来源或 CSRF Token 无效")
				return
			}
		}
		identity := browserIdentity{Username: admin.Username, SessionID: cookie.Value, Admin: admin}
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), browserContextKey{}, identity)))
	})
}

func sameOrigin(request *http.Request) bool {
	if request == nil || request.TLS == nil || strings.TrimSpace(request.Host) == "" {
		return false
	}
	value := strings.TrimSpace(request.Header.Get("Origin"))
	if value == "" {
		value = strings.TrimSpace(request.Header.Get("Referer"))
	}
	if value == "" {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, request.Host) {
		return false
	}
	if request.Header.Get("Origin") != "" && (parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "") {
		return false
	}
	return true
}

func requestMetadataFrom(request *http.Request) *requestMetadata {
	if request == nil {
		return &requestMetadata{}
	}
	metadata, _ := request.Context().Value(requestContextKey{}).(*requestMetadata)
	if metadata == nil {
		return &requestMetadata{}
	}
	return metadata
}

func requestIDFrom(request *http.Request) string {
	return requestMetadataFrom(request).requestID
}

func setRequestRoute(request *http.Request, route string) {
	requestMetadataFrom(request).route = route
}

func setRequestActor(request *http.Request, actor string) {
	requestMetadataFrom(request).actor = actor
}

func setRequestError(request *http.Request, code string) {
	requestMetadataFrom(request).errorCode = code
}

func browserIdentityFrom(request *http.Request) (browserIdentity, bool) {
	if request == nil {
		return browserIdentity{}, false
	}
	identity, ok := request.Context().Value(browserContextKey{}).(browserIdentity)
	return identity, ok
}

func safeHandlerError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
