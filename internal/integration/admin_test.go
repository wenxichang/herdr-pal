package integration_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/hprp"
	"github.com/wenxichang/herdr-pal/internal/serverapp"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

func TestHPAPEndToEnd(t *testing.T) {
	harness := newHPAPHarness(t)
	defer harness.stop(t)

	wrongSource := harness.issueKey(t, "user-wrong", "blocked", []string{"192.0.2.10"})
	harness.tokens = append(harness.tokens, wrongSource.Token)
	assertHPRPUnauthorized(t, harness.relayURL, wrongSource.Token)

	homeKey := harness.issueKey(t, "user-a", "home", []string{"127.0.0.1"})
	officeKey := harness.issueKey(t, "user-a", "office", []string{"127.0.0.1"})
	otherKey := harness.issueKey(t, "user-b", "home", []string{"127.0.0.1"})
	harness.tokens = append(harness.tokens, homeKey.Token, officeKey.Token, otherKey.Token)

	homeHerdr := testkit.NewHerdrServer(t, integrationSnapshot("session-home", herdr.AgentStatusWorking))
	officeHerdr := testkit.NewHerdrServer(t, integrationSnapshot("session-office", herdr.AgentStatusBlocked))
	otherHerdr := testkit.NewHerdrServer(t, integrationSnapshot("session-other", herdr.AgentStatusDone))
	homeHerdr.SetOutput([]string{"terminal-sensitive-home"})
	officeHerdr.SetOutput([]string{"terminal-sensitive-office"})
	otherHerdr.SetOutput([]string{"terminal-sensitive-other"})
	harness.pals = append(harness.pals,
		startHPAPPal(t, harness.relayURL, homeKey.Token, homeHerdr.SocketPath()),
		startHPAPPal(t, harness.relayURL, officeKey.Token, officeHerdr.SocketPath()),
		startHPAPPal(t, harness.relayURL, otherKey.Token, otherHerdr.SocketPath()),
	)

	connections := harness.waitConnections(t, 3)
	if len(connections.Items) != 3 {
		t.Fatalf("connection list = %#v", connections)
	}
	sessions := harness.waitSessions(t, 3)
	assertHPAPSessionStates(t, sessions)
	for name, fake := range map[string]*testkit.HerdrServer{"home": homeHerdr, "office": officeHerdr, "other": otherHerdr} {
		if calls := fake.Calls("agent.read"); len(calls) != 0 {
			t.Fatalf("session list triggered %s agent.read: %#v", name, calls)
		}
	}
	if encoded, err := json.Marshal(sessions); err != nil || bytes.Contains(encoded, []byte("terminal-sensitive")) {
		t.Fatalf("session JSON contains terminal output: %s, error=%v", encoded, err)
	}

	harness.exerciseConcurrentQueriesAndMutations(t)

	homeCredentialID := homeKey.Credential.CredentialID
	var disabled adminproto.CredentialMutationResult
	harness.call(t, adminproto.MethodKeyDisable, adminproto.CredentialIDParams{CredentialID: homeCredentialID}, &disabled)
	if disabled.DisconnectedConnections != 1 || disabled.Credential.Status != "disabled" {
		t.Fatalf("key.disable result = %#v", disabled)
	}
	harness.waitCredentialConnection(t, homeCredentialID, "", false)

	var enabled adminproto.CredentialMutationResult
	harness.call(t, adminproto.MethodKeyEnable, adminproto.CredentialIDParams{CredentialID: homeCredentialID}, &enabled)
	if enabled.Credential.Status != "enabled" {
		t.Fatalf("key.enable result = %#v", enabled)
	}
	reconnected := harness.waitCredentialConnection(t, homeCredentialID, "", true)

	var disconnected adminproto.ConnectionDisconnectResult
	harness.call(t, adminproto.MethodConnectionDisconnect, adminproto.ConnectionIDParams{ConnectionID: reconnected.ConnectionID}, &disconnected)
	if !disconnected.Disconnected {
		t.Fatalf("connection.disconnect result = %#v", disconnected)
	}
	harness.waitCredentialConnection(t, homeCredentialID, reconnected.ConnectionID, true)
	var shown adminproto.CredentialResult
	harness.call(t, adminproto.MethodKeyShow, adminproto.CredentialIDParams{CredentialID: homeCredentialID}, &shown)
	if shown.Credential.Status != "enabled" {
		t.Fatalf("connection.disconnect changed credential = %#v", shown)
	}

	var restricted adminproto.CredentialMutationResult
	harness.call(t, adminproto.MethodKeySourceSet, adminproto.KeySourceMutationParams{
		CredentialID: homeCredentialID, Sources: []string{"192.0.2.20-192.0.2.30"},
	}, &restricted)
	if restricted.DisconnectedConnections != 1 {
		t.Fatalf("source.set restricted result = %#v", restricted)
	}
	harness.waitCredentialConnection(t, homeCredentialID, "", false)
	var restored adminproto.CredentialMutationResult
	harness.call(t, adminproto.MethodKeySourceSet, adminproto.KeySourceMutationParams{
		CredentialID: homeCredentialID, Sources: []string{"127.0.0.1/32"},
	}, &restored)
	harness.waitCredentialConnection(t, homeCredentialID, "", true)

	var deleted adminproto.KeyDeleteResult
	harness.call(t, adminproto.MethodKeyDelete, adminproto.KeyDeleteParams{CredentialID: homeCredentialID, Confirm: true}, &deleted)
	if !deleted.Deleted || deleted.DisconnectedConnections != 1 {
		t.Fatalf("key.delete result = %#v", deleted)
	}
	harness.waitCredentialConnection(t, homeCredentialID, "", false)
	assertHPAPStable(t, "deleted credential remains offline", 1500*time.Millisecond, func() bool {
		result, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
		if err != nil {
			return false
		}
		for _, item := range result.Items {
			if item.CredentialID == homeCredentialID {
				return false
			}
		}
		return true
	})

	keyList, err := harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{})
	if err != nil {
		t.Fatal(err)
	}
	connectionList, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
	if err != nil {
		t.Fatal(err)
	}
	sessionList, err := harness.admin.ListSessions(context.Background(), adminproto.SessionListParams{})
	if err != nil {
		t.Fatal(err)
	}
	managementJSON, err := json.Marshal([]any{keyList, connectionList, sessionList})
	if err != nil {
		t.Fatal(err)
	}
	assertNoHPAPSecrets(t, "management JSON", managementJSON, harness.tokens, harness.secret)

	var stopResult adminproto.ServerStopResult
	harness.call(t, adminproto.MethodServerStop, adminproto.EmptyParams{}, &stopResult)
	if !stopResult.Stopping {
		t.Fatalf("server.stop result = %#v", stopResult)
	}
	harness.waitServerStopped(t)
	harness.wecom.WaitConnectionCount(t, 0)
	if _, err := os.Lstat(harness.adminSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin socket still exists after stop: %v", err)
	}

	for _, pal := range harness.pals {
		pal.stop(t)
	}
	credentialData, err := os.ReadFile(filepath.Join(harness.stateDir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoHPAPSecrets(t, "credential file", credentialData, harness.tokens, harness.secret)
	assertNoHPAPSecrets(t, "server logs", harness.logs.Bytes(), harness.tokens, harness.secret)
	for index, pal := range harness.pals {
		assertNoHPAPSecrets(t, fmt.Sprintf("pal logs %d", index), pal.logs.Bytes(), harness.tokens, harness.secret)
	}
}

type hpapHarness struct {
	admin       *adminclient.Client
	adminSocket string
	relayURL    string
	stateDir    string
	secret      string
	wecom       *testkit.WeComServer
	logs        *lockedHPAPBuffer
	cancel      context.CancelFunc
	done        chan error
	stopOnce    sync.Once
	serverDone  bool
	pals        []*hpapPal
	tokens      []string
}

func newHPAPHarness(t *testing.T) *hpapHarness {
	t.Helper()
	stateDir, err := os.MkdirTemp("/tmp", "hp-hpap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listenAddress := reserveHPAPAddress(t)
	botID := fmt.Sprintf("bot-hpap-%d", time.Now().UnixNano())
	secret := fmt.Sprintf("secret-hpap-%d", time.Now().UnixNano())
	weComServer := testkit.NewWeComServer(t, botID, secret)
	configPath := filepath.Join(t.TempDir(), "server.json")
	raw := fmt.Sprintf(`{"wecom":{"bot_id":%q},"server":{"listen":%q,"addr_hint":"127.0.0.1","state_dir":%q},"log":{"level":"debug"}}`, botID, listenAddress, stateDir)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &lockedHPAPBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serverapp.Run(ctx, serverapp.Options{
			ConfigPath: configPath, Getenv: func(string) string { return secret },
			Stderr: logs, WeComEndpoint: weComServer.Endpoint(),
		})
	}()
	adminSocket := filepath.Join(stateDir, "admin.sock")
	admin, err := adminclient.New(adminclient.Config{SocketPath: adminSocket, Timeout: time.Second})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	harness := &hpapHarness{
		admin: admin, adminSocket: adminSocket, relayURL: "wss://" + listenAddress,
		stateDir: stateDir, secret: secret, wecom: weComServer, logs: logs,
		cancel: cancel, done: done,
	}
	t.Cleanup(func() { harness.stop(t) })
	harness.waitServerReady(t)
	weComServer.WaitSubscribeCount(t, 1)
	return harness
}

func (harness *hpapHarness) waitServerReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status adminproto.ServerStatusResult
		if err := harness.admin.Call(context.Background(), adminproto.MethodServerStatus, adminproto.EmptyParams{}, &status); err == nil {
			if status.HPAP != adminproto.Protocol || status.HPRP != hprp.ProtocolVersion || status.AdminSocket != harness.adminSocket {
				t.Fatalf("server.status = %#v", status)
			}
			return
		}
		select {
		case err := <-harness.done:
			t.Fatalf("server stopped before HPAP became ready: %v\n%s", err, harness.logs.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("HPAP did not become ready:\n%s", harness.logs.String())
}

func (harness *hpapHarness) issueKey(t *testing.T, principalID, machineID string, sources []string) adminproto.KeyIssueResult {
	t.Helper()
	var result adminproto.KeyIssueResult
	harness.call(t, adminproto.MethodKeyIssue, adminproto.KeyIssueParams{
		PrincipalID: principalID, MachineID: machineID, Sources: sources,
	}, &result)
	if result.Token == "" || result.Credential.CredentialID == 0 {
		t.Fatalf("key.issue result = %#v", result)
	}
	return result
}

func (harness *hpapHarness) call(t *testing.T, method adminproto.Method, params, result any) {
	t.Helper()
	if err := harness.admin.Call(context.Background(), method, params, result); err != nil {
		t.Fatalf("HPAP %s error = %v", method, err)
	}
}

func (harness *hpapHarness) waitConnections(t *testing.T, count int) adminproto.ConnectionListResult {
	t.Helper()
	var result adminproto.ConnectionListResult
	eventuallyHPAP(t, fmt.Sprintf("connection count %d", count), func() bool {
		current, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
		if err != nil || len(current.Items) != count {
			return false
		}
		result = current
		return true
	})
	return result
}

func (harness *hpapHarness) waitSessions(t *testing.T, count int) adminproto.SessionListResult {
	t.Helper()
	var result adminproto.SessionListResult
	eventuallyHPAP(t, fmt.Sprintf("session count %d", count), func() bool {
		current, err := harness.admin.ListSessions(context.Background(), adminproto.SessionListParams{})
		if err != nil || len(current.Items) != count {
			return false
		}
		result = current
		return true
	})
	return result
}

func (harness *hpapHarness) waitCredentialConnection(t *testing.T, credentialID uint64, oldConnectionID string, want bool) adminproto.Connection {
	t.Helper()
	var found adminproto.Connection
	eventuallyHPAP(t, fmt.Sprintf("credential %d connection=%t", credentialID, want), func() bool {
		result, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{})
		if err != nil {
			return false
		}
		for _, item := range result.Items {
			if item.CredentialID == credentialID && (oldConnectionID == "" || item.ConnectionID != oldConnectionID) {
				found = item
				return want
			}
		}
		return !want
	})
	return found
}

func (harness *hpapHarness) exerciseConcurrentQueriesAndMutations(t *testing.T) {
	t.Helper()
	var wait sync.WaitGroup
	errorsChannel := make(chan error, 24)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := harness.admin.ListKeys(context.Background(), adminproto.KeyListParams{Limit: 2}); err != nil {
				errorsChannel <- err
			}
			if _, err := harness.admin.ListConnections(context.Background(), adminproto.ConnectionListParams{Limit: 2}); err != nil {
				errorsChannel <- err
			}
			if _, err := harness.admin.ListSessions(context.Background(), adminproto.SessionListParams{Limit: 2}); err != nil {
				errorsChannel <- err
			}
		}()
	}
	for index := range 4 {
		issued := harness.issueKey(t, fmt.Sprintf("query-user-%d", index), fmt.Sprintf("query-machine-%d", index), []string{"127.0.0.1"})
		harness.tokens = append(harness.tokens, issued.Token)
		var mutation adminproto.CredentialMutationResult
		harness.call(t, adminproto.MethodKeyDisable, adminproto.CredentialIDParams{CredentialID: issued.Credential.CredentialID}, &mutation)
		harness.call(t, adminproto.MethodKeyEnable, adminproto.CredentialIDParams{CredentialID: issued.Credential.CredentialID}, &mutation)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent HPAP query failed: %v", err)
	}
}

func (harness *hpapHarness) waitServerStopped(t *testing.T) {
	t.Helper()
	select {
	case err := <-harness.done:
		harness.serverDone = true
		if err != nil {
			t.Fatalf("server.stop error = %v\n%s", err, harness.logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("server.stop timeout\n%s", harness.logs.String())
	}
}

func (harness *hpapHarness) stop(t *testing.T) {
	t.Helper()
	harness.stopOnce.Do(func() {
		for _, pal := range harness.pals {
			pal.stop(t)
		}
		if harness.serverDone {
			return
		}
		harness.cancel()
		select {
		case err := <-harness.done:
			harness.serverDone = true
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("HPAP server cleanup error = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("HPAP server cleanup timeout")
		}
	})
}

type hpapPal struct {
	logs     *lockedHPAPBuffer
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
}

func startHPAPPal(t *testing.T, relayURL, token, herdrSocket string) *hpapPal {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw := fmt.Sprintf(`{"relay":{"url":%q,"key":%q,"skip_verify":true},"herdr":{"socket_path":%q},"log":{"level":"debug"}}`, relayURL, token, herdrSocket)
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &lockedHPAPBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.RunRelay(ctx, app.Options{ConfigPath: configPath, Stderr: logs, Stdout: io.Discard})
	}()
	return &hpapPal{logs: logs, cancel: cancel, done: done}
}

func (pal *hpapPal) stop(t *testing.T) {
	t.Helper()
	pal.stopOnce.Do(func() {
		pal.cancel()
		select {
		case err := <-pal.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Pal cleanup error = %v\n%s", err, pal.logs.String())
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Pal cleanup timeout\n%s", pal.logs.String())
		}
	})
}

func assertHPRPUnauthorized(t *testing.T, endpoint, token string) {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: httpClient, HTTPHeader: header, Subprotocols: []string{hprp.Subprotocol},
	})
	if connection != nil {
		connection.CloseNow()
	}
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-source HPRP dial = response:%v error:%v", response, err)
	}
}

func assertHPAPSessionStates(t *testing.T, result adminproto.SessionListResult) {
	t.Helper()
	want := map[string]string{
		"user-a/home":   hprp.StatusWorking,
		"user-a/office": hprp.StatusBlocked,
		"user-b/home":   hprp.StatusDone,
	}
	for _, item := range result.Items {
		key := item.PrincipalID + "/" + item.Target.MachineID
		status, exists := want[key]
		if !exists || item.Status != status || item.StatusLabel == "" || item.Target.SessionID == "" {
			t.Fatalf("session item = %#v", item)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing session states = %#v", want)
	}
}

func assertNoHPAPSecrets(t *testing.T, name string, data []byte, tokens []string, secret string) {
	t.Helper()
	for _, forbidden := range append(append([]string(nil), tokens...), secret, "terminal-sensitive") {
		if forbidden != "" && bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("%s leaked %q", name, forbidden)
		}
	}
}

func reserveHPAPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func eventuallyHPAP(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", description)
}

func assertHPAPStable(t *testing.T, description string, duration time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if !condition() {
			t.Fatalf("状态不稳定：%s", description)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type lockedHPAPBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedHPAPBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedHPAPBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

func (buffer *lockedHPAPBuffer) String() string {
	return string(buffer.Bytes())
}
