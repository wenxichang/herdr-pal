package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/credential"
	"github.com/wenxichang/herdr-pal/internal/policy"
	"github.com/wenxichang/herdr-pal/internal/server"
)

func TestDSHPluginThroughProductionAppAndWeComRouter(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("dsh 插件集成测试需要 Node.js 22+")
	}
	token := strings.Repeat("integration-secret-", 3)
	node := exec.Command("node", "../../plugins/dsh-herdr-pal/test/serve-fixture.mjs")
	node.Env = append(os.Environ(), "DSH_PAL_TOKEN="+token)
	stdout, err := node.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	node.Stderr = os.Stderr
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Process.Kill(); _ = node.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("plugin did not start")
	}
	endpoint := scanner.Text()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := server.NewSessionCatalog()
	key, identity := issueIntegrationKey(t, "user-a", "dsh-machine", 31)
	hub, err := server.NewClientHub(catalog, integrationVerifier{identities: map[string]credential.Identity{key: identity}}, server.HubConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	gateway := &relayGateway{replies: map[string]string{}, pushes: map[string][]string{}}
	deduper, _ := policy.NewDeduper(time.Hour, 100, time.Now)
	router, err := server.NewConversationRouter(catalog, server.NewUserExecutor(64), gateway, hub, deduper, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.SetOutboundSink(router); err != nil {
		t.Fatal(err)
	}
	relayServer := httptest.NewTLSServer(hub)
	defer relayServer.Close()
	configPath := filepath.Join(t.TempDir(), "client.json")
	data, _ := json.Marshal(map[string]any{"relay": map[string]any{"url": "wss" + strings.TrimPrefix(relayServer.URL, "https"), "key": key, "skip_verify": true}, "herdr": map[string]bool{"disabled": true}, "dsh": map[string]string{"url": endpoint, "token": token}})
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunRelay(ctx, app.Options{ConfigPath: configPath, Stderr: io.Discard}) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Pal did not stop")
		}
	}()
	eventuallyRelay(t, func() bool { return len(catalog.CreateNumberedSnapshot("user-a")) == 1 })
	router.Handle(ctx, relayIncoming("dsh-list", "user-a", "/ls"))
	if reply := gateway.Reply("request-dsh-list"); !strings.Contains(reply, "两周内的历史会话") || strings.Contains(reply, "old-session") {
		t.Fatal(reply)
	}
	router.Handle(ctx, relayIncoming("dsh-select", "user-a", "/1"))
	eventuallyRelay(t, func() bool { return strings.Contains(gateway.Reply("request-dsh-select"), "历史回答") })
	message := relayIncoming("dsh-prompt", "user-a", "继续处理")
	router.Handle(ctx, message)
	router.Handle(ctx, message)
	eventuallyRelay(t, func() bool { return strings.Contains(gateway.Reply("request-dsh-prompt"), "已发送") })
	eventuallyRelay(t, func() bool {
		return strings.Contains(strings.Join(gateway.Pushes("user-a"), "\n"), "已完成：继续处理")
	})
	router.Handle(ctx, relayIncoming("dsh-read", "user-a", "/con"))
	eventuallyRelay(t, func() bool { return strings.Contains(gateway.Reply("request-dsh-read"), "已完成：继续处理") })
	if reply := gateway.Reply("request-dsh-read"); strings.Count(reply, "用户：继续处理") != 1 {
		t.Fatalf("duplicate prompt: %s", reply)
	}
	for _, content := range append(gateway.Pushes("user-a"), gateway.Reply("request-dsh-select"), gateway.Reply("request-dsh-read")) {
		if strings.Contains(content, "INTERNAL_INSTRUCTIONS") || strings.Contains(content, "HIDDEN_REASONING") {
			t.Fatal("internal dsh content escaped into chat output")
		}
	}
	_ = node.Process.Kill()
	eventuallyRelay(t, func() bool { return !catalog.HasSessions("user-a") })
}
