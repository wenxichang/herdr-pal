package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/adminservice"
)

func TestMachineRegistrationEndToEndAutoIssueApproveAndRollback(t *testing.T) {
	harness := newHPAPHarness(t)
	defer harness.stop(t)
	const principalID = "registration-user"

	harness.wecom.InjectText(t, "registration-first", principalID, "single", "/reg office 127.0.0.1")
	firstResponses := harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 1)
	firstKey := registrationKeyFromContent(t, firstResponses[len(firstResponses)-1].Content)
	if !strings.Contains(firstResponses[len(firstResponses)-1].Content, "/help") {
		t.Fatalf("first registration response=%q", firstResponses[len(firstResponses)-1].Content)
	}

	harness.wecom.InjectText(t, "registration-second", principalID, "single", "/reg mobile 127.0.0.2")
	secondResponses := harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 2)
	if !strings.Contains(secondResponses[len(secondResponses)-1].Content, "等待管理员审批") {
		t.Fatalf("second registration response=%q", secondResponses[len(secondResponses)-1].Content)
	}

	admin := newIntegrationWebAdmin(t, harness)
	pending := admin.listRegistrations(t)
	if len(pending) != 1 || pending[0].PrincipalID != principalID || pending[0].MachineID != "mobile" {
		t.Fatalf("pending=%#v", pending)
	}
	approved := admin.approve(t, pending[0].RegistrationID, http.StatusOK)
	if approved.CredentialID == 0 || !approved.Approved {
		t.Fatalf("approved=%#v", approved)
	}
	pushes := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 1)
	secondKey := registrationKeyFromContent(t, pushes[len(pushes)-1].Content)
	if firstKey == secondKey || pushes[len(pushes)-1].ChatID != principalID || !strings.Contains(pushes[len(pushes)-1].Content, "/help") {
		t.Fatalf("approval push=%#v", pushes[len(pushes)-1])
	}
	if pending = admin.listRegistrations(t); len(pending) != 0 {
		t.Fatalf("pending after approval=%#v", pending)
	}
	keys, err := harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{})
	if err != nil || len(keys.Items) != 2 {
		t.Fatalf("keys=%#v err=%v", keys, err)
	}

	harness.wecom.InjectText(t, "registration-third", principalID, "single", "/reg laptop 127.0.0.3")
	harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 3)
	pending = admin.listRegistrations(t)
	if len(pending) != 1 || pending[0].MachineID != "laptop" {
		t.Fatalf("third pending=%#v", pending)
	}
	harness.wecom.SetResponseError("aibot_send_msg", 93000)
	failed := admin.approve(t, pending[0].RegistrationID, http.StatusBadGateway)
	if failed.Approved {
		t.Fatalf("failed approval=%#v", failed)
	}
	harness.wecom.WaitRequestCount(t, "aibot_send_msg", 2)
	if pending = admin.listRegistrations(t); len(pending) != 1 || pending[0].MachineID != "laptop" {
		t.Fatalf("pending after failed delivery=%#v", pending)
	}
	keys, err = harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{})
	if err != nil || len(keys.Items) != 2 {
		t.Fatalf("keys after rollback=%#v err=%v", keys, err)
	}
	harness.wecom.SetResponseError("aibot_send_msg", 0)
}

func TestMachineRegistrationAutoIssueResponseFailureRollsBackCredential(t *testing.T) {
	harness := newHPAPHarness(t)
	defer harness.stop(t)
	harness.wecom.SetResponseError("aibot_respond_msg", 93000)
	harness.wecom.InjectText(t, "registration-auto-failed", "registration-user", "single", "/reg office 127.0.0.1")
	harness.wecom.WaitRequestCount(t, "aibot_respond_msg", 1)
	eventuallyHPAP(t, "首台注册响应失败后凭据回滚", func() bool {
		keys, err := harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{})
		return err == nil && len(keys.Items) == 0
	})
	admin := newIntegrationWebAdmin(t, harness)
	if pending := admin.listRegistrations(t); len(pending) != 0 {
		t.Fatalf("pending after auto issue delivery failure=%#v", pending)
	}
}

func TestMachineRegistrationEndToEndRejectsPendingRequest(t *testing.T) {
	harness := newHPAPHarness(t)
	defer harness.stop(t)
	const principalID = "registration-reject-user"

	harness.wecom.InjectText(t, "registration-reject-first", principalID, "single", "/reg office 127.0.0.1")
	harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 1)
	harness.wecom.InjectText(t, "registration-reject-second", principalID, "single", "/reg mobile 127.0.0.2")
	harness.wecom.WaitCompletedRequestCount(t, "aibot_respond_msg", 2)

	admin := newIntegrationWebAdmin(t, harness)
	pending := admin.listRegistrations(t)
	if len(pending) != 1 || pending[0].MachineID != "mobile" {
		t.Fatalf("pending=%#v", pending)
	}
	rejected := admin.reject(t, pending[0].RegistrationID, "来源地址不符合要求", http.StatusOK)
	if !rejected.Rejected || !rejected.NotificationSent || rejected.RegistrationID != pending[0].RegistrationID {
		t.Fatalf("rejected=%#v", rejected)
	}
	pushes := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 1)
	last := pushes[len(pushes)-1]
	if last.ChatID != principalID || !strings.Contains(last.Content, "来源地址不符合要求") || !strings.Contains(last.Content, pending[0].RegistrationID) {
		t.Fatalf("rejection push=%#v", last)
	}
	if pending = admin.listRegistrations(t); len(pending) != 0 {
		t.Fatalf("pending after rejection=%#v", pending)
	}
	keys, err := harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{})
	if err != nil || len(keys.Items) != 1 {
		t.Fatalf("keys after rejection=%#v err=%v", keys, err)
	}
}

func TestMachineRegistrationWeComApprovalAndRejectionFlow(t *testing.T) {
	harness := newHPAPHarnessWithRegistrationAdmins(t, []string{"registration-admin-a", "registration-admin-b"})
	defer harness.stop(t)
	const principalID = "registration-wecom-user"
	if logs := harness.logs.String(); !strings.Contains(logs, "registration_admin_count=2") {
		t.Fatalf("startup logs = %q, want registration admin count", logs)
	}

	firstReply := harness.sendText(t, "registration-wecom-first", principalID, "/reg office 127.0.0.1")
	registrationKeyFromContent(t, firstReply)

	pendingReply := harness.sendText(t, "registration-wecom-second", principalID, "/reg mobile 127.0.0.2")
	if !strings.Contains(pendingReply, "等待管理员审批") {
		t.Fatalf("pending reply = %q", pendingReply)
	}
	pushes := harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 1)
	if push := pushes[len(pushes)-1]; push.ChatID != "registration-admin-a" || !strings.Contains(push.Content, "mobile") || !strings.Contains(push.Content, "/ls-reg") {
		t.Fatalf("first admin notification = %#v", push)
	}

	listReply := harness.sendText(t, "registration-wecom-list-a", "registration-admin-a", "/ls-reg")
	for _, want := range []string{"1. 用户：" + principalID, "机器：mobile", "来源：127.0.0.2"} {
		if !strings.Contains(listReply, want) {
			t.Fatalf("list reply = %q, want %q", listReply, want)
		}
	}
	approveReply := harness.sendText(t, "registration-wecom-approve", "registration-admin-a", "/apr 1")
	if !strings.Contains(approveReply, "已批准") || !strings.Contains(approveReply, registrationSnapshotReminderForTest) {
		t.Fatalf("approve reply = %q", approveReply)
	}
	pushes = harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 2)
	if push := pushes[len(pushes)-1]; push.ChatID != principalID || !strings.Contains(push.Content, "机器 Key") {
		t.Fatalf("approval key push = %#v", push)
	}
	staleReply := harness.sendText(t, "registration-wecom-approve-stale", "registration-admin-a", "/apr 1")
	if !strings.Contains(staleReply, "先执行 /ls-reg") || !strings.Contains(staleReply, registrationSnapshotReminderForTest) {
		t.Fatalf("stale approve reply = %q", staleReply)
	}

	thirdReply := harness.sendText(t, "registration-wecom-third", principalID, "/reg laptop 127.0.0.3")
	if !strings.Contains(thirdReply, "等待管理员审批") {
		t.Fatalf("third registration reply = %q", thirdReply)
	}
	pushes = harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 3)
	if push := pushes[len(pushes)-1]; push.ChatID != "registration-admin-b" || !strings.Contains(push.Content, "laptop") {
		t.Fatalf("second admin notification = %#v", push)
	}
	if reply := harness.sendText(t, "registration-wecom-list-b", "registration-admin-b", "/ls-reg"); !strings.Contains(reply, "机器：laptop") {
		t.Fatalf("second list reply = %q", reply)
	}
	rejectReply := harness.sendText(t, "registration-wecom-reject", "registration-admin-b", "/rej 1")
	if !strings.Contains(rejectReply, "已驳回") || !strings.Contains(rejectReply, registrationSnapshotReminderForTest) {
		t.Fatalf("reject reply = %q", rejectReply)
	}
	pushes = harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 4)
	if push := pushes[len(pushes)-1]; push.ChatID != principalID || !strings.Contains(push.Content, "已驳回") || !strings.Contains(push.Content, "laptop") {
		t.Fatalf("rejection push = %#v", push)
	}
}

func TestMachineRegistrationWeComApprovalRejectsSnapshotChangedByWeb(t *testing.T) {
	harness := newHPAPHarnessWithRegistrationAdmins(t, []string{"registration-admin"})
	defer harness.stop(t)
	const principalID = "registration-race-user"

	registrationKeyFromContent(t, harness.sendText(t, "registration-race-first", principalID, "/reg office 127.0.0.1"))
	if reply := harness.sendText(t, "registration-race-second", principalID, "/reg mobile 127.0.0.2"); !strings.Contains(reply, "等待管理员审批") {
		t.Fatalf("pending reply = %q", reply)
	}
	harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 1)
	if reply := harness.sendText(t, "registration-race-list", "registration-admin", "/ls-reg"); !strings.Contains(reply, "机器：mobile") {
		t.Fatalf("list reply = %q", reply)
	}

	admin := newIntegrationWebAdmin(t, harness)
	pending := admin.listRegistrations(t)
	if len(pending) != 1 {
		t.Fatalf("pending = %#v", pending)
	}
	approved := admin.approve(t, pending[0].RegistrationID, http.StatusOK)
	if !approved.Approved {
		t.Fatalf("approved = %#v", approved)
	}
	harness.wecom.WaitCompletedRequestCount(t, "aibot_send_msg", 2)

	reply := harness.sendText(t, "registration-race-stale-approve", "registration-admin", "/apr 1")
	if !strings.Contains(reply, "列表已变化") || !strings.Contains(reply, registrationSnapshotReminderForTest) {
		t.Fatalf("stale approval reply = %q", reply)
	}
	if pending = admin.listRegistrations(t); len(pending) != 0 {
		t.Fatalf("pending after stale approval = %#v", pending)
	}
}

const registrationSnapshotReminderForTest = "列表快照已失效，请重新执行 /ls-reg 核实当前条目顺序。"

var integrationRegistrationKeyPattern = regexp.MustCompile(`\bhpk_[0-9]+_[A-Za-z0-9_-]{20,}\b`)

func registrationKeyFromContent(t *testing.T, content string) string {
	t.Helper()
	token := integrationRegistrationKeyPattern.FindString(content)
	if token == "" {
		t.Fatalf("content does not contain machine key: %q", content)
	}
	return token
}

type integrationWebAdmin struct {
	baseURL string
	client  *http.Client
	cookie  *http.Cookie
	csrf    string
}

func newIntegrationWebAdmin(t *testing.T, harness *hpapHarness) *integrationWebAdmin {
	t.Helper()
	var status adminproto.ServerStatusResult
	harness.call(t, adminproto.MethodServerStatus, adminproto.EmptyParams{}, &status)
	baseURL := "https://" + status.WebAdminListen
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	bootstrap, err := os.ReadFile(filepath.Join(harness.stateDir, "bootstrap.txt"))
	if err != nil {
		t.Fatal(err)
	}
	initialPassword := integrationBootstrapValue(string(bootstrap), "初始密码：")
	if initialPassword == "" {
		t.Fatalf("bootstrap=%q", bootstrap)
	}
	admin := &integrationWebAdmin{baseURL: baseURL, client: client}
	admin.login(t, initialPassword)
	admin.request(t, http.MethodPost, "/admin/api/v1/auth/password", map[string]any{
		"current_password": initialPassword,
		"new_password":     "integration replacement password",
	}, http.StatusOK, nil)
	admin.login(t, "integration replacement password")
	return admin
}

func (admin *integrationWebAdmin) login(t *testing.T, password string) {
	t.Helper()
	var result struct {
		Username  string `json:"username"`
		CSRFToken string `json:"csrf_token"`
	}
	response := admin.request(t, http.MethodPost, "/admin/api/v1/auth/login", map[string]any{
		"username": "admin", "password": password,
	}, http.StatusOK, &result)
	cookies := response.Cookies()
	if len(cookies) == 0 || result.CSRFToken == "" {
		t.Fatalf("login cookies=%#v result=%#v", cookies, result)
	}
	admin.cookie = cookies[0]
	admin.csrf = result.CSRFToken
}

func (admin *integrationWebAdmin) listRegistrations(t *testing.T) []adminservice.Registration {
	t.Helper()
	var page struct {
		Items []adminservice.Registration `json:"items"`
	}
	admin.request(t, http.MethodGet, "/admin/api/v1/registrations", nil, http.StatusOK, &page)
	return page.Items
}

func (admin *integrationWebAdmin) approve(t *testing.T, registrationID string, wantStatus int) adminservice.RegistrationApprovalResult {
	t.Helper()
	var result adminservice.RegistrationApprovalResult
	admin.request(t, http.MethodPost, "/admin/api/v1/registrations/"+registrationID+"/approve", map[string]any{}, wantStatus, &result)
	return result
}

func (admin *integrationWebAdmin) reject(t *testing.T, registrationID, reason string, wantStatus int) adminservice.RegistrationRejectionResult {
	t.Helper()
	var result adminservice.RegistrationRejectionResult
	admin.request(t, http.MethodPost, "/admin/api/v1/registrations/"+registrationID+"/reject", map[string]any{"reason": reason}, wantStatus, &result)
	return result
}

func (admin *integrationWebAdmin) request(t *testing.T, method, path string, body any, wantStatus int, destination any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, admin.baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", admin.baseURL)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if admin.cookie != nil {
		request.AddCookie(admin.cookie)
	}
	if admin.csrf != "" && method != http.MethodGet {
		request.Header.Set("X-CSRF-Token", admin.csrf)
	}
	response, err := admin.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, response.StatusCode, wantStatus, payload)
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, payload)
	}
	if response.StatusCode >= http.StatusBadRequest {
		if envelope.Error == nil {
			t.Fatalf("missing API error: %s", payload)
		}
		return response
	}
	if destination != nil && json.Unmarshal(envelope.Data, destination) != nil {
		t.Fatalf("decode data: %s", envelope.Data)
	}
	return response
}

func integrationBootstrapValue(content, prefix string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
