// Package webadmin 提供内嵌于 herdr-pal-server 的 HTTPS 管理入口。
package webadmin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
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
	admin      *adminservice.Service
	auth       *adminauth.Store
	sessions   *adminauth.SessionManager
	loginGuard *adminauth.LoginGuard
	logger     *slog.Logger
	random     io.Reader
	now        func() time.Time

	randomMu sync.Mutex
	handler  http.Handler
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
	}
	mux := http.NewServeMux()
	server.registerAuthRoutes(mux)
	mux.HandleFunc("/admin/api/v1/", func(writer http.ResponseWriter, request *http.Request) {
		setRequestRoute(request, "/admin/api/v1/*")
		_ = writeAPIError(writer, request, http.StatusNotFound, "not_found", "管理接口不存在")
	})
	server.handler = server.middleware(mux)
	return server, nil
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
