package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/lokiquery"
	"github.com/wenxichang/herdr-pal/internal/serverapp"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

func TestWebAdminEndToEnd(t *testing.T) {
	stateDir := secureWebAdminTempDir(t)
	relayAddress := reserveHPAPAddress(t)
	webAddress := reserveHPAPAddress(t)
	botID := fmt.Sprintf("bot-web-admin-%d", time.Now().UnixNano())
	secret := fmt.Sprintf("secret-web-admin-%d", time.Now().UnixNano())
	weComServer := testkit.NewWeComServer(t, botID, secret)
	lokiServer, lokiQuery := newWebAdminLoki(t)
	authFile := filepath.Join(stateDir, "server-auth.json")
	configPath := filepath.Join(t.TempDir(), "server.json")
	rawConfig := fmt.Sprintf(`{
		"wecom":{"bot_id":%q},
		"server":{"listen":%q,"addr_hint":"127.0.0.1","state_dir":%q},
		"admin":{"listen":%q,"loki_url":%q},
		"log":{"level":"debug"}
	}`, botID, relayAddress, stateDir, webAddress, lokiServer.URL)
	if err := os.WriteFile(configPath, []byte(rawConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout := &lockedHPAPBuffer{}
	stderr := &lockedHPAPBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serverapp.Run(ctx, serverapp.Options{
			ConfigPath: configPath, Getenv: func(string) string { return secret },
			Stdout: stdout, Stderr: stderr, AuthFile: authFile, WeComEndpoint: weComServer.Endpoint(),
		})
	}()
	serverStopped := false
	t.Cleanup(func() {
		if serverStopped {
			return
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("Web Admin Server cleanup timeout\n%s", stderr.String())
		}
	})

	adminSocket := filepath.Join(stateDir, "admin.sock")
	admin, err := adminclient.New(adminclient.Config{SocketPath: adminSocket, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var status adminproto.ServerStatusResult
	eventuallyHPAP(t, "Web 管理 Server 就绪", func() bool {
		return admin.Call(context.Background(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &status) == nil
	})
	weComServer.WaitSubscribeCount(t, 1)
	if status.WebAdminListen != webAddress {
		t.Fatalf("server status WebAdminListen = %q, want %q", status.WebAdminListen, webAddress)
	}
	bootstrap := parseWebAdminBootstrap(t, stdout.String())
	if bootstrap.username != "admin" || bootstrap.password == "" || bootstrap.token == "" {
		t.Fatalf("bootstrap = %#v", bootstrap)
	}
	if strings.Count(stdout.String(), "初始密码：") != 1 || strings.Count(stdout.String(), "自动化 Token：") != 1 {
		t.Fatalf("bootstrap output = %q", stdout.String())
	}

	webFingerprint := tlsFingerprint(t, webAddress)
	relayFingerprint := tlsFingerprint(t, relayAddress)
	if webFingerprint != relayFingerprint || webFingerprint != status.TLS.SHA256Fingerprint {
		t.Fatalf("TLS fingerprints web=%s relay=%s status=%s", webFingerprint, relayFingerprint, status.TLS.SHA256Fingerprint)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Jar: jar, Timeout: 5 * time.Second}
	baseURL := "https://" + webAddress
	login := webAdminJSON[webAuthResult](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/auth/login", baseURL, "", "", map[string]any{
		"username": bootstrap.username, "password": bootstrap.password,
	})
	if !login.MustChangePassword || login.CSRFToken == "" {
		t.Fatalf("login = %#v", login)
	}
	const replacementPassword = "replacement password 2026"
	passwordChanged := webAdminJSON[webAuthResult](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/auth/password", baseURL, login.CSRFToken, "", map[string]any{
		"current_password": bootstrap.password, "new_password": replacementPassword,
	})
	if passwordChanged.MustChangePassword || passwordChanged.CSRFToken == "" {
		t.Fatalf("password result = %#v", passwordChanged)
	}
	csrf := passwordChanged.CSRFToken

	issued := webAdminJSON[adminservice.CredentialIssueResult](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/credentials", baseURL, csrf, "", map[string]any{
		"principal_id": "web-user", "machine_id": "web-home", "sources": []string{"127.0.0.1"},
	})
	if issued.Token == "" || issued.Credential.CredentialID == 0 {
		t.Fatalf("issued credential = %#v", issued)
	}
	keys, err := admin.ListKeys(context.Background(), adminproto.KeyListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys.Items) != 1 || keys.Items[0].CredentialID != issued.Credential.CredentialID {
		t.Fatalf("HPAP keys = %#v", keys)
	}

	herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("web-admin-session", herdr.AgentStatusDone))
	pal := startHPAPPal(t, "wss://"+relayAddress, issued.Token, herdrServer.SocketPath())
	t.Cleanup(func() { pal.stop(t) })
	eventuallyHPAP(t, "Web 签发凭据建立 HPRP 连接", func() bool {
		connections, err := admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
		return err == nil && len(connections.Items) == 1 && connections.Items[0].CredentialID == issued.Credential.CredentialID
	})

	createdAdmin := webAdminJSON[webCreatedAdmin](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/administrators", baseURL, csrf, "", map[string]any{"username": "ops.team"})
	if createdAdmin.InitialPassword == "" || createdAdmin.AutomationToken == "" || createdAdmin.Admin.Username != "ops.team" {
		t.Fatalf("created admin = %#v", createdAdmin)
	}
	automationIssued := webAdminJSON[adminservice.CredentialIssueResult](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/automation/credentials", "", "", createdAdmin.AutomationToken, map[string]any{
		"principal_id": "automation-user", "machine_id": "office", "sources": []string{"127.0.0.1"},
	})
	if automationIssued.Token == "" || automationIssued.Credential.CredentialID == issued.Credential.CredentialID {
		t.Fatalf("automation issued = %#v", automationIssued)
	}

	start := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	end := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	auditURL := baseURL + "/admin/api/v1/audit/logs?userid=web-user&machine_id=HOME&keyword=NeedleSecret&start=" + url.QueryEscape(start) + "&end=" + url.QueryEscape(end)
	auditPage := webAdminJSON[lokiquery.Page](t, httpClient, http.MethodGet, auditURL, "", "", "", nil)
	if len(auditPage.Items) != 2 || auditPage.Items[0].PrincipalID != "web-user" || !strings.Contains(auditPage.Items[0].Body, "audit-body-sensitive") {
		t.Fatalf("audit page = %#v", auditPage)
	}
	receivedLogQL := lokiQuery.String()
	for _, want := range []string{`herdr_pal_audit_schema_version="1"`, `herdr_pal_audit_principal_id="web-user"`, `herdr_pal_audit_machine_id=~"(?i).*HOME.*"`, `|~ "(?i)NeedleSecret"`} {
		if !strings.Contains(receivedLogQL, want) {
			t.Fatalf("Loki query = %q, want %q", receivedLogQL, want)
		}
	}

	webAdminJSON[adminservice.CredentialDeleteResult](t, httpClient, http.MethodDelete, baseURL+"/admin/api/v1/automation/credentials/"+strconv.FormatUint(issued.Credential.CredentialID, 10), "", "", createdAdmin.AutomationToken, nil)
	eventuallyHPAP(t, "自动化删除凭据后撤下连接", func() bool {
		connections, err := admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
		return err == nil && len(connections.Items) == 0
	})

	webAdminJSON[map[string]bool](t, httpClient, http.MethodPost, baseURL+"/admin/api/v1/server/stop", baseURL, csrf, "", map[string]any{"confirm": true})
	select {
	case err := <-done:
		serverStopped = true
		if err != nil {
			t.Fatalf("Web stop error = %v\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Web stop timeout\n%s", stderr.String())
	}

	cookies := jar.Cookies(mustParseURL(t, baseURL))
	forbidden := []string{
		bootstrap.password, bootstrap.token, issued.Token, automationIssued.Token,
		createdAdmin.InitialPassword, createdAdmin.AutomationToken, csrf, "NeedleSecret", "audit-body-sensitive",
	}
	for _, cookie := range cookies {
		forbidden = append(forbidden, cookie.Value)
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(stderr.Bytes(), []byte(value)) {
			t.Fatalf("stderr leaked sensitive value %q: %s", value, stderr.String())
		}
	}
	authContent, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{bootstrap.password, bootstrap.token, createdAdmin.InitialPassword, createdAdmin.AutomationToken} {
		if bytes.Contains(authContent, []byte(value)) {
			t.Fatalf("auth file leaked plaintext %q", value)
		}
	}
}

type webAuthResult struct {
	Username           string `json:"username"`
	MustChangePassword bool   `json:"must_change_password"`
	CSRFToken          string `json:"csrf_token"`
}

type webCreatedAdmin struct {
	Admin struct {
		Username string `json:"username"`
	} `json:"admin"`
	InitialPassword string `json:"initial_password"`
	AutomationToken string `json:"automation_token"`
}

type webBootstrap struct {
	username string
	password string
	token    string
}

func parseWebAdminBootstrap(t *testing.T, output string) webBootstrap {
	t.Helper()
	result := webBootstrap{}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case strings.HasPrefix(line, "管理员："):
			result.username = strings.TrimPrefix(line, "管理员：")
		case strings.HasPrefix(line, "初始密码："):
			result.password = strings.TrimPrefix(line, "初始密码：")
		case strings.HasPrefix(line, "自动化 Token："):
			result.token = strings.TrimPrefix(line, "自动化 Token：")
		}
	}
	return result
}

func webAdminJSON[T any](t *testing.T, client *http.Client, method, target, origin, csrf, bearer string, body any) T {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("%s %s invalid response status=%d body=%s", method, target, response.StatusCode, encoded)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || envelope.Error != nil {
		t.Fatalf("%s %s status=%d error=%#v body=%s", method, target, response.StatusCode, envelope.Error, encoded)
	}
	var result T
	if err := json.Unmarshal(envelope.Data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func newWebAdminLoki(t *testing.T) (*httptest.Server, *lockedHPAPBuffer) {
	t.Helper()
	receivedQuery := &lockedHPAPBuffer{}
	oldest := time.Now().UTC().Add(-2 * time.Minute)
	newest := oldest.Add(time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(writer, request)
			return
		}
		_, _ = receivedQuery.Write([]byte(request.URL.Query().Get("query")))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"herdr-pal-server"},"values":[["%d","audit-body-sensitive old",{"event_name":"herdr_pal.user_input","herdr_pal_audit_principal_id":"web-user","herdr_pal_audit_machine_id":"web-home","herdr_pal_audit_outcome":"accepted"}],["%d","audit-body-sensitive new",{"event_name":"herdr_pal.terminal_output","herdr_pal_audit_principal_id":"web-user","herdr_pal_audit_machine_id":"web-home","herdr_pal_audit_pane_id":"w1:p1","herdr_pal_audit_outcome":"accepted"}]]}]}}`, oldest.UnixNano(), newest.UnixNano())
	}))
	t.Cleanup(server.Close)
	return server, receivedQuery
}

func tlsFingerprint(t *testing.T, address string) string {
	t.Helper()
	connection, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	certificates := connection.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		t.Fatal("TLS peer certificate missing")
	}
	digest := sha256.Sum256(certificates[0].Raw)
	return hex.EncodeToString(digest[:])
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func secureWebAdminTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "hp-web-admin-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}
