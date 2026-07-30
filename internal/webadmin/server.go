// Package webadmin 提供内嵌于 herdr-pal-server 的 HTTPS 管理入口。
package webadmin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

var ErrInvalidConfig = errors.New("Web 管理台配置无效")

const sessionCookieName = "herdr_pal_admin_session"

// Config 指定 Web 管理台共享服务、认证状态和运行依赖。
type Config struct {
	Admin      *adminservice.Service
	Auth       *adminauth.Store
	Sessions   *adminauth.SessionManager
	LoginGuard *adminauth.LoginGuard
	Logger     *slog.Logger
	Random     io.Reader
	Now        func() time.Time
}

// Server 提供同源 HTTPS 管理页面和版本化 JSON API。
type Server struct {
	admin           *adminservice.Service
	auth            *adminauth.Store
	sessions        *adminauth.SessionManager
	loginGuard      *adminauth.LoginGuard
	automationLimit *automationLimiter
	logger          *slog.Logger
	random          io.Reader
	now             func() time.Time

	randomMu sync.Mutex
	handler  http.Handler
	routes   map[string]*methodRouter
}

type methodRouter struct {
	route    string
	handlers map[string]http.Handler
}

func (router *methodRouter) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	setRequestRoute(request, router.route)
	handler := router.handlers[request.Method]
	if handler == nil {
		methods := make([]string, 0, len(router.handlers))
		for method := range router.handlers {
			methods = append(methods, method)
		}
		sort.Strings(methods)
		writer.Header().Set("Allow", strings.Join(methods, ", "))
		_ = writeAPIError(writer, request, http.StatusMethodNotAllowed, "method_not_allowed", "HTTP 方法不受支持")
		return
	}
	handler.ServeHTTP(writer, request)
}

// New 创建 Web 管理 Server 并注册认证路由。
func New(config Config) (*Server, error) {
	if config.Admin == nil || config.Auth == nil || config.Sessions == nil || config.LoginGuard == nil || config.Logger == nil {
		return nil, ErrInvalidConfig
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	server := &Server{
		admin: config.Admin, auth: config.Auth, sessions: config.Sessions,
		loginGuard: config.LoginGuard, logger: config.Logger, random: config.Random, now: config.Now,
		automationLimit: newAutomationLimiter(), routes: make(map[string]*methodRouter),
	}
	mux := http.NewServeMux()
	server.registerAuthRoutes(mux)
	server.registerManagementRoutes(mux)
	server.registerAdminRoutes(mux)
	server.registerAutomationRoutes(mux)
	mux.HandleFunc("/admin/api/v1/", func(writer http.ResponseWriter, request *http.Request) {
		setRequestRoute(request, "/admin/api/v1/*")
		_ = writeAPIError(writer, request, http.StatusNotFound, "not_found", "管理接口不存在")
	})
	server.handler = server.middleware(mux)
	return server, nil
}

func (server *Server) handleMethod(mux *http.ServeMux, route, method string, handler http.Handler) {
	router := server.routes[route]
	if router == nil {
		router = &methodRouter{route: route, handlers: make(map[string]http.Handler)}
		server.routes[route] = router
		mux.Handle(route, router)
	}
	router.handlers[method] = handler
}

// Handler 返回包含完整安全中间件链的 HTTP Handler。
func (server *Server) Handler() http.Handler {
	if server == nil || server.handler == nil {
		return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "Web 管理台不可用", http.StatusServiceUnavailable)
		})
	}
	return server.handler
}

func (server *Server) nextRequestID() string {
	value := make([]byte, 16)
	server.randomMu.Lock()
	_, err := io.ReadFull(server.random, value)
	server.randomMu.Unlock()
	if err != nil {
		fallback := server.now().UTC().AppendFormat(nil, "20060102150405.000000000")
		copy(value, fallback)
	}
	return hex.EncodeToString(value)
}
