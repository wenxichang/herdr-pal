package webadmin

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
)

//go:embed assets/templates/*.html assets/static/*
var embeddedAssets embed.FS

type pageView struct {
	Title              string
	Page               string
	Username           string
	MustChangePassword bool
}

var adminPages = []struct {
	Path  string
	Page  string
	Title string
}{
	{Path: "/admin", Page: "overview", Title: "概览"},
	{Path: "/admin/credentials", Page: "credentials", Title: "机器凭据"},
	{Path: "/admin/registrations", Page: "registrations", Title: "机器注册审批"},
	{Path: "/admin/connections", Page: "connections", Title: "在线连接"},
	{Path: "/admin/sessions", Page: "sessions", Title: "Agent 会话"},
	{Path: "/admin/audit", Page: "audit", Title: "审计日志"},
	{Path: "/admin/administrators", Page: "administrators", Title: "管理员"},
	{Path: "/admin/system", Page: "system", Title: "系统"},
}

func loadTemplates() (*template.Template, error) {
	return template.New("layout.html").Funcs(template.FuncMap{
		"active": func(current, candidate string) bool { return current == candidate },
	}).ParseFS(embeddedAssets, "assets/templates/*.html")
}

func (server *Server) registerPageRoutes(mux *http.ServeMux) {
	server.handleMethod(mux, "/admin/login", http.MethodGet, http.HandlerFunc(server.loginPage))
	server.handleMethod(mux, "/admin/{$}", http.MethodGet, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.adminPage(writer, request, "overview", "概览")
	}))
	for _, configured := range adminPages {
		view := configured
		server.handleMethod(mux, view.Path, http.MethodGet, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			server.adminPage(writer, request, view.Page, view.Title)
		}))
	}
	mux.Handle("/admin/static/", server.staticAssetHandler())
}

func (server *Server) loginPage(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.pageAdministrator(request); ok {
		http.Redirect(writer, request, "/admin", http.StatusSeeOther)
		return
	}
	server.renderPage(writer, request, "login.html", pageView{Title: "管理员登录", Page: "login"})
}

func (server *Server) adminPage(writer http.ResponseWriter, request *http.Request, page, title string) {
	admin, ok := server.pageAdministrator(request)
	if !ok {
		http.Redirect(writer, request, "/admin/login", http.StatusSeeOther)
		return
	}
	setRequestActor(request, admin.Username)
	server.renderPage(writer, request, "layout.html", pageView{
		Title: title, Page: page, Username: admin.Username, MustChangePassword: admin.MustChangePassword,
	})
}

func (server *Server) pageAdministrator(request *http.Request) (adminauth.Admin, bool) {
	cookie, err := request.Cookie(sessionCookieName)
	if err != nil {
		return adminauth.Admin{}, false
	}
	session, ok := server.sessions.Get(cookie.Value)
	if !ok {
		return adminauth.Admin{}, false
	}
	admin, err := server.auth.Admin(session.Username)
	if err != nil {
		server.sessions.Delete(cookie.Value)
		return adminauth.Admin{}, false
	}
	return admin, true
}

func (server *Server) renderPage(writer http.ResponseWriter, request *http.Request, name string, view pageView) {
	var rendered bytes.Buffer
	if err := server.templates.ExecuteTemplate(&rendered, name, view); err != nil {
		server.logger.Error("渲染 Web 管理页面失败", "request_id", requestIDFrom(request), "error_type", safeHandlerError(err))
		http.Error(writer, "管理页面暂不可用", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(rendered.Bytes())
}

func (server *Server) staticAssetHandler() http.Handler {
	staticFS, err := fs.Sub(embeddedAssets, "assets/static")
	if err != nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			http.Error(writer, "HTTP 方法不受支持", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(request.URL.Path, "/admin/static/")
		if name == "" || name != path.Base(name) {
			http.NotFound(writer, request)
			return
		}
		content, err := fs.ReadFile(staticFS, name)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		switch path.Ext(name) {
		case ".css":
			writer.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".js":
			writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		default:
			writer.Header().Set("Content-Type", "application/octet-stream")
		}
		writer.Header().Set("Cache-Control", "public, max-age=3600")
		writer.Header().Set("Content-Length", strconv.Itoa(len(content)))
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_, _ = writer.Write(content)
		}
	})
}
