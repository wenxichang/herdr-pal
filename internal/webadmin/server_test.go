package webadmin

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminauth"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/machinereg"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestServerRequiresTLSAndAddsSecurityHeaders(t *testing.T) {
	web, _, logs := newTestWebServer(t)
	plain := httptest.NewRequest(http.MethodGet, "/admin/api/v1/auth/session", nil)
	plain.RemoteAddr = "192.0.2.10:4321"
	plainResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(plainResponse, plain)
	if plainResponse.Code != http.StatusBadRequest {
		t.Fatalf("plain status = %d", plainResponse.Code)
	}

	request := newTLSRequest(http.MethodGet, "/admin/api/v1/auth/session", nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	response := httptest.NewRecorder()
	web.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("TLS status = %d body=%s", response.Code, response.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if !strings.Contains(response.Header().Get(header), want) {
			t.Fatalf("%s = %q", header, response.Header().Get(header))
		}
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected CORS header = %q", response.Header().Get("Access-Control-Allow-Origin"))
	}
	requestID := response.Header().Get("X-Request-ID")
	if len(requestID) != 32 || !strings.Contains(response.Body.String(), requestID) {
		t.Fatalf("request id = %q body=%s", requestID, response.Body.String())
	}
	if !strings.Contains(logs.String(), "192.0.2.10") || strings.Contains(logs.String(), "203.0.113.99") {
		t.Fatalf("logs used forwarded source: %s", logs.String())
	}
	methodRequest := newTLSRequest(http.MethodPost, "/admin/api/v1/auth/session", strings.NewReader(`{}`))
	methodResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(methodResponse, methodRequest)
	if methodResponse.Code != http.StatusMethodNotAllowed || !strings.Contains(methodResponse.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("method status=%d content-type=%q body=%s", methodResponse.Code, methodResponse.Header().Get("Content-Type"), methodResponse.Body.String())
	}
}

func TestServerRecoversPanicWithoutLeakingDetails(t *testing.T) {
	web, _, logs := newTestWebServer(t)
	handler := web.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret panic detail")
	}))
	request := newTLSRequest(http.MethodGet, "/admin/api/v1/test", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret panic detail") || strings.Contains(logs.String(), "secret panic detail") {
		t.Fatalf("status=%d body=%q logs=%q", response.Code, response.Body.String(), logs.String())
	}
}

func TestServerLogsCanonicalRoutesAndHashesMachineTargets(t *testing.T) {
	web, cookie, csrf, logs := authenticatedManagementServer(t, webTestDependencies{})
	pathToken := "hpa_0123456789abcdef_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	unknown := newTLSRequest(http.MethodGet, "/admin/"+pathToken, nil)
	unknownResponse := httptest.NewRecorder()
	web.Handler().ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", unknownResponse.Code, unknownResponse.Body.String())
	}
	machineToken := "hpk_1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	body := `{"principal_id":"user-a","machine_id":"` + machineToken + `","sources":["127.0.0.1"]}`
	issued := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", body)
	if issued.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", issued.Code, issued.Body.String())
	}
	duplicate := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", body)
	if duplicate.Code == http.StatusCreated {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	output := logs.String()
	for _, secret := range []string{pathToken, machineToken} {
		if strings.Contains(output, secret) {
			t.Fatalf("management logs leaked %q: %s", secret, output)
		}
	}
	if !strings.Contains(output, "route=unmatched") || !strings.Contains(output, "machine_id_hash=") {
		t.Fatalf("management logs missing safe metadata: %s", output)
	}
}

func newTestWebServer(t *testing.T) (*Server, adminauth.Bootstrap, *strings.Builder) {
	return newTestWebServerWithDependencies(t, webTestDependencies{})
}

type webTestDependencies struct {
	Connections       adminservice.ConnectionManager
	Sessions          adminservice.SessionInspector
	Runtime           adminservice.RuntimeController
	Registrations     adminservice.RegistrationManager
	KeyDelivery       machinereg.KeyDeliveryFunc
	RejectionDelivery machinereg.RejectionDeliveryFunc
	Audit             AuditQuerier
	Now               func() time.Time
}

func newTestWebServerWithDependencies(t *testing.T, dependencies webTestDependencies) (*Server, adminauth.Bootstrap, *strings.Builder) {
	t.Helper()
	now := dependencies.Now
	if now == nil {
		fixed := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		now = func() time.Time { return fixed }
	}
	credentialStore, err := credential.LoadStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if dependencies.Connections == nil {
		dependencies.Connections = emptyWebConnections{}
	}
	if dependencies.Sessions == nil {
		dependencies.Sessions = emptyWebSessions{}
	}
	if dependencies.Runtime == nil {
		dependencies.Runtime = &emptyWebRuntime{}
	}
	if dependencies.Registrations == nil {
		dependencies.Registrations = emptyWebRegistrationManager{}
	}
	if dependencies.KeyDelivery == nil {
		dependencies.KeyDelivery = func(context.Context, machinereg.KeyDelivery) error { return nil }
	}
	if dependencies.RejectionDelivery == nil {
		dependencies.RejectionDelivery = func(context.Context, machinereg.RejectionDelivery) error { return nil }
	}
	adminService, err := adminservice.New(adminservice.Config{
		Credentials:       credentialStore,
		Connections:       dependencies.Connections,
		Sessions:          dependencies.Sessions,
		Runtime:           dependencies.Runtime,
		Registrations:     dependencies.Registrations,
		KeyDelivery:       dependencies.KeyDelivery,
		RejectionDelivery: dependencies.RejectionDelivery,
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	authStore, bootstrap, err := adminauth.Load(filepath.Join(t.TempDir(), "server-auth.json"), adminauth.Options{
		Now: now, Random: &webIncrementingReader{next: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := adminauth.NewSessionManager(adminauth.SessionConfig{
		Now: now, Random: &webIncrementingReader{next: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	web, err := New(Config{
		Admin: adminService, Auth: authStore, Sessions: sessions,
		LoginGuard: adminauth.NewLoginGuard(now),
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		Random:     &webIncrementingReader{next: 160},
		Now:        now,
		Audit:      dependencies.Audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return web, bootstrap, logs
}

func newTLSRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.TLS = &tls.ConnectionState{}
	request.Host = "admin.example.test:4001"
	request.RemoteAddr = "192.0.2.10:4321"
	return request
}

type webIncrementingReader struct {
	next byte
}

func (reader *webIncrementingReader) Read(value []byte) (int, error) {
	for index := range value {
		value[index] = reader.next
		reader.next++
	}
	return len(value), nil
}

type emptyWebConnections struct{}

func (emptyWebConnections) Connections() []server.ConnectionView { return nil }
func (emptyWebConnections) Connection(string) (server.ConnectionView, bool) {
	return server.ConnectionView{}, false
}
func (emptyWebConnections) DisconnectConnection(string, string) bool { return false }
func (emptyWebConnections) DisconnectCredential(uint64, string) int  { return 0 }
func (emptyWebConnections) RevalidateCredentialSource(uint64, []credential.SourceRule, string) int {
	return 0
}

type emptyWebSessions struct{}

func (emptyWebSessions) ManagementSessions(server.SessionFilter) []server.SessionView { return nil }

type emptyWebRuntime struct{}

func (*emptyWebRuntime) Status() adminservice.ServerStatus { return adminservice.ServerStatus{} }
func (*emptyWebRuntime) EnableDebug()                      {}
func (*emptyWebRuntime) DisableDebug()                     {}
func (*emptyWebRuntime) RequestStop() bool                 { return true }

type emptyWebRegistrationManager struct{}

func (emptyWebRegistrationManager) ListPending() []machinereg.Request { return nil }
func (emptyWebRegistrationManager) Approve(context.Context, string, string, machinereg.KeyDeliveryFunc) (machinereg.ApprovalResult, error) {
	return machinereg.ApprovalResult{}, machinereg.ErrRequestNotFound
}
func (emptyWebRegistrationManager) Reject(context.Context, string, string, string, machinereg.RejectionDeliveryFunc) (machinereg.RejectionResult, error) {
	return machinereg.RejectionResult{}, machinereg.ErrRequestNotFound
}
