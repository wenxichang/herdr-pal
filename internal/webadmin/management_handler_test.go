package webadmin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestManagementCredentialLifecycleConflictPaginationAndConfirmation(t *testing.T) {
	web, cookie, csrf, logs := authenticatedManagementServer(t, webTestDependencies{})
	first := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", `{"principal_id":"user-a","machine_id":"home","sources":["127.0.0.1"]}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", first.Code, first.Body.String())
	}
	var issued adminservice.CredentialIssueResult
	decodeManagementData(t, first, &issued)
	if issued.Credential.CredentialID != 1 || !strings.HasPrefix(issued.Token, "hpk_1_") {
		t.Fatalf("issued = %#v", issued)
	}
	if strings.Contains(logs.String(), issued.Token) {
		t.Fatalf("management log leaked machine key: %s", logs.String())
	}
	duplicate := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", `{"principal_id":"user-a","machine_id":"home","sources":["10.0.0.1"]}`)
	assertManagementError(t, duplicate, http.StatusConflict, string(adminservice.CodeCredentialConflict))
	second := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/credentials", `{"principal_id":"user-a","machine_id":"office","sources":["127.0.0.1"]}`)
	if second.Code != http.StatusCreated {
		t.Fatalf("second issue status=%d body=%s", second.Code, second.Body.String())
	}

	firstPage := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials?limit=1", "")
	var page struct {
		Items         []adminservice.Credential `json:"items"`
		NextPageToken string                    `json:"next_page_token"`
	}
	decodeManagementData(t, firstPage, &page)
	if len(page.Items) != 1 || page.Items[0].CredentialID != 1 || page.NextPageToken == "" {
		t.Fatalf("first page = %#v", page)
	}
	secondPage := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials?limit=1&page_token="+page.NextPageToken, "")
	page = struct {
		Items         []adminservice.Credential `json:"items"`
		NextPageToken string                    `json:"next_page_token"`
	}{}
	decodeManagementData(t, secondPage, &page)
	if len(page.Items) != 1 || page.Items[0].CredentialID != 2 || page.NextPageToken != "" {
		t.Fatalf("second page = %#v", page)
	}

	show := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials/1", "")
	var shown adminservice.Credential
	decodeManagementData(t, show, &shown)
	if shown.MachineID != "home" {
		t.Fatalf("shown = %#v", shown)
	}
	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/admin/api/v1/credentials/1/disable", `{}`},
		{http.MethodPost, "/admin/api/v1/credentials/1/enable", `{}`},
		{http.MethodPost, "/admin/api/v1/credentials/1/sources", `{"sources":["10.0.0.1"]}`},
		{http.MethodPut, "/admin/api/v1/credentials/1/sources", `{"sources":["10.0.0.1","10.0.0.2"]}`},
		{http.MethodDelete, "/admin/api/v1/credentials/1/sources", `{"sources":["10.0.0.2"]}`},
	} {
		response := managementRequest(t, web, cookie, csrf, test.method, test.path, test.body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
	sources := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials/1/sources", "")
	var sourceResult struct {
		Sources []string `json:"sources"`
	}
	decodeManagementData(t, sources, &sourceResult)
	if len(sourceResult.Sources) != 1 || sourceResult.Sources[0] != "10.0.0.1" {
		t.Fatalf("sources = %#v", sourceResult)
	}

	unconfirmed := managementRequest(t, web, cookie, csrf, http.MethodDelete, "/admin/api/v1/credentials/1", `{"confirm":false}`)
	assertManagementError(t, unconfirmed, http.StatusBadRequest, "confirmation_required")
	deleted := managementRequest(t, web, cookie, csrf, http.MethodDelete, "/admin/api/v1/credentials/1", `{"confirm":true}`)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	missing := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/credentials/1", "")
	assertManagementError(t, missing, http.StatusNotFound, string(adminservice.CodeCredentialNotFound))
}

func TestManagementConnectionSessionRuntimeAndStopWriteOrdering(t *testing.T) {
	connections := &managementConnections{views: []server.ConnectionView{managementWebConnection("c-1", "user-a", "home")}}
	sessions := &managementSessions{views: []server.SessionView{managementWebSession("user-a", 1, "home", "w1:p1", "s-1")}}
	runtime := &managementRuntime{status: adminservice.ServerStatus{Version: "v-test", BaseLogLevel: "info"}}
	web, cookie, csrf, _ := authenticatedManagementServer(t, webTestDependencies{Connections: connections, Sessions: sessions, Runtime: runtime})

	status := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/server/status", "")
	var statusResult adminservice.ServerStatus
	decodeManagementData(t, status, &statusResult)
	if statusResult.Version != "v-test" {
		t.Fatalf("status = %#v", statusResult)
	}
	connection := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/connections/c-1", "")
	var connectionResult adminservice.Connection
	decodeManagementData(t, connection, &connectionResult)
	if connectionResult.ConnectionID != "c-1" {
		t.Fatalf("connection = %#v", connectionResult)
	}
	unconfirmed := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/connections/c-1/disconnect", `{"confirm":false}`)
	assertManagementError(t, unconfirmed, http.StatusBadRequest, "confirmation_required")
	disconnected := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/connections/c-1/disconnect", `{"confirm":true}`)
	if disconnected.Code != http.StatusOK || len(connections.disconnected) != 1 {
		t.Fatalf("disconnect status=%d calls=%v body=%s", disconnected.Code, connections.disconnected, disconnected.Body.String())
	}

	sessionResponse := managementRequest(t, web, cookie, csrf, http.MethodGet, "/admin/api/v1/sessions?userid=user-a&machine_id=home", "")
	var sessionPage struct {
		Items []adminservice.Session `json:"items"`
	}
	decodeManagementData(t, sessionResponse, &sessionPage)
	if len(sessionPage.Items) != 1 || sessions.lastFilter != (server.SessionFilter{PrincipalID: "user-a", MachineID: "home"}) {
		t.Fatalf("sessions=%#v filter=%#v", sessionPage, sessions.lastFilter)
	}
	debug := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/server/debug", `{"enabled":true}`)
	if debug.Code != http.StatusOK || !runtime.status.DebugEnabled {
		t.Fatalf("debug status=%d runtime=%#v", debug.Code, runtime.status)
	}

	failing := &managementFailWriter{header: make(http.Header)}
	stopRequest := authenticatedManagementHTTPRequest(t, cookie, csrf, http.MethodPost, "/admin/api/v1/server/stop", `{"confirm":true}`)
	web.Handler().ServeHTTP(failing, stopRequest)
	if runtime.stopCalls != 0 {
		t.Fatalf("stop committed after failed response write: %d", runtime.stopCalls)
	}
	stopped := managementRequest(t, web, cookie, csrf, http.MethodPost, "/admin/api/v1/server/stop", `{"confirm":true}`)
	if stopped.Code != http.StatusOK || runtime.stopCalls != 1 {
		t.Fatalf("stop status=%d calls=%d body=%s", stopped.Code, runtime.stopCalls, stopped.Body.String())
	}
}

func authenticatedManagementServer(t *testing.T, dependencies webTestDependencies) (*Server, *http.Cookie, string, *strings.Builder) {
	t.Helper()
	web, bootstrap, logs := newTestWebServerWithDependencies(t, dependencies)
	if err := web.auth.ChangePassword("admin", bootstrap.InitialPassword, "replacement password"); err != nil {
		t.Fatal(err)
	}
	login := loginWebAdmin(t, web, "admin", "replacement password")
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	data := decodeWebEnvelope(t, login)["data"].(map[string]any)
	return web, findSessionCookie(t, login), data["csrf_token"].(string), logs
}

func managementRequest(t *testing.T, web *Server, cookie *http.Cookie, csrf, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := authenticatedManagementHTTPRequest(t, cookie, csrf, method, target, body)
	response := httptest.NewRecorder()
	web.Handler().ServeHTTP(response, request)
	return response
}

func authenticatedManagementHTTPRequest(t *testing.T, cookie *http.Cookie, csrf, method, target, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := newTLSRequest(method, target, reader)
	request.AddCookie(cookie)
	if method != http.MethodGet {
		request.Header.Set("Origin", "https://admin.example.test:4001")
		request.Header.Set("X-CSRF-Token", csrf)
	}
	return request
}

func decodeManagementData(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *APIError       `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil {
		t.Fatalf("response error = %#v", envelope.Error)
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		t.Fatal(err)
	}
}

func assertManagementError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != status || envelope.Error == nil || envelope.Error.Code != code {
		t.Fatalf("status=%d error=%#v body=%s", response.Code, envelope.Error, response.Body.String())
	}
}

type managementConnections struct {
	views        []server.ConnectionView
	disconnected []string
}

func (connections *managementConnections) Connections() []server.ConnectionView {
	return append([]server.ConnectionView(nil), connections.views...)
}
func (connections *managementConnections) Connection(id string) (server.ConnectionView, bool) {
	for _, view := range connections.views {
		if view.ConnectionID == id {
			return view, true
		}
	}
	return server.ConnectionView{}, false
}
func (connections *managementConnections) DisconnectConnection(id, _ string) bool {
	if _, ok := connections.Connection(id); !ok {
		return false
	}
	connections.disconnected = append(connections.disconnected, id)
	return true
}
func (*managementConnections) DisconnectCredential(uint64, string) int { return 1 }
func (*managementConnections) RevalidateCredentialSource(uint64, []credential.SourceRule, string) int {
	return 1
}

type managementSessions struct {
	views      []server.SessionView
	lastFilter server.SessionFilter
}

func (sessions *managementSessions) ManagementSessions(filter server.SessionFilter) []server.SessionView {
	sessions.lastFilter = filter
	return append([]server.SessionView(nil), sessions.views...)
}

type managementRuntime struct {
	status    adminservice.ServerStatus
	stopCalls int
}

func (runtime *managementRuntime) Status() adminservice.ServerStatus { return runtime.status }
func (runtime *managementRuntime) EnableDebug()                      { runtime.status.DebugEnabled = true }
func (runtime *managementRuntime) DisableDebug()                     { runtime.status.DebugEnabled = false }
func (runtime *managementRuntime) RequestStop() bool {
	runtime.stopCalls++
	return runtime.stopCalls == 1
}

type managementFailWriter struct {
	header http.Header
	status int
}

func (writer *managementFailWriter) Header() http.Header { return writer.header }
func (writer *managementFailWriter) WriteHeader(status int) {
	writer.status = status
}
func (*managementFailWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func managementWebConnection(id, principalID, machineID string) server.ConnectionView {
	return server.ConnectionView{
		ConnectionID: id, CredentialID: 1, PrincipalID: principalID, MachineID: machineID,
		Implementation: hprp.Implementation{Name: "herdr-pal", Version: "v-test", OS: "linux", Arch: "amd64"},
		SourceIP:       "192.0.2.20", ConnectedAt: time.Unix(10, 0), LastHeartbeatAt: time.Unix(20, 0),
		LastSnapshotAt: time.Unix(30, 0), Ready: true,
	}
}

func managementWebSession(principalID string, number int, machineID, slotID, sessionID string) server.SessionView {
	return server.SessionView{
		PrincipalID: principalID, Number: number,
		Target: hprp.Target{MachineID: machineID, SlotID: slotID, SessionID: sessionID},
		Session: hprp.Session{SlotID: slotID, SessionID: sessionID, Status: hprp.StatusIdle,
			Display: hprp.SessionDisplay{Index: number, Agent: "codex", DisplayAgent: "Codex", Workspace: "test", Tab: "main"}},
		WorkspaceLabel: "test/main", StatusLabel: "idle 💤",
	}
}
