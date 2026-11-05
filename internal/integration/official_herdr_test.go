//go:build !windows

package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/installer"
)

// TestOfficialHerdrCompatibility 使用独立目录和进程验证真实公开 API，不接入 IM 或用户现有会话。
func TestOfficialHerdrCompatibility(t *testing.T) {
	binary := os.Getenv("HERDR_PAL_TEST_HERDR")
	if binary == "" {
		t.Skip("设置 HERDR_PAL_TEST_HERDR 为官方 Herdr 二进制绝对路径后运行")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("HERDR_PAL_TEST_HERDR 必须为绝对路径")
	}
	// Unix Socket 的路径长度有限，使用短临时根目录。
	root, err := os.MkdirTemp("/tmp", "pal-herdr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	configRoot := filepath.Join(root, "config")
	herdrConfig := filepath.Join(configRoot, "herdr", "config.toml")
	clientConfig := filepath.Join(root, "pal.json")
	if err := os.MkdirAll(filepath.Dir(herdrConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := "onboarding = false\n\n# BEGIN HERDR PAL MANAGED SIDECAR\n[[sidecar]]\ncommand = [\"herdr-pal\"]\n# END HERDR PAL MANAGED SIDECAR\n"
	if err := os.WriteFile(herdrConfig, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := installer.Apply(ctx, installer.Request{
		ClientConfigPath: clientConfig, HerdrConfigPath: herdrConfig, HerdrBinaryPath: binary,
		RelayURL: "wss://relay.invalid/hprp", RelayKey: "hpk_7_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, installer.Options{})
	if err != nil {
		t.Fatalf("官方 Herdr 安装配置迁移失败: %v", err)
	}
	backup, err := os.ReadFile(result.HerdrBackupPath)
	if err != nil || string(backup) != previous {
		t.Fatalf("旧配置备份不完整: %v", err)
	}
	server := exec.CommandContext(ctx, binary, "server")
	server.Env = os.Environ()
	for key, value := range map[string]string{
		"HOME": root, "SHELL": "/bin/sh", "XDG_CONFIG_HOME": configRoot,
		"XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
		"HERDR_CONFIG_PATH": herdrConfig,
	} {
		server.Env = replaceTestEnvironment(server.Env, key, value)
	}
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = server.Process.Kill()
		_ = server.Wait()
	}()
	socketPath := filepath.Join(configRoot, "herdr", "herdr.sock")
	client := herdr.NewClient(socketPath, nil, 2*time.Second)
	for {
		err := client.CheckCompatible(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("独立 Herdr 启动失败: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Panes) != 0 {
		t.Fatal("隔离实例意外包含现有 pane")
	}
	t.Logf("真实 Herdr version=%s protocol=%d", snapshot.Version, snapshot.Protocol)
	lifecycle, err := client.Subscribe(ctx, herdr.LifecycleSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	callIsolatedHerdr(t, ctx, socketPath, "workspace.create", map[string]any{"cwd": root, "label": "Pal compatibility test"})
	if _, err := lifecycle.Recv(ctx); err != nil {
		t.Fatalf("生命周期事件读取失败: %v", err)
	}
	snapshot, err = client.Snapshot(ctx)
	if err != nil || len(snapshot.Panes) != 1 {
		t.Fatalf("新 pane 快照解析失败: %v, count=%d", err, len(snapshot.Panes))
	}
	pane := snapshot.Panes[0]
	if pane.Columns <= 0 || pane.Rows <= 0 {
		t.Fatal("未解析出公开布局中的终端尺寸")
	}
	statuses, err := client.Subscribe(ctx, herdr.StatusSubscriptions([]string{pane.PaneID}))
	if err != nil {
		t.Fatal(err)
	}
	defer statuses.Close()
	callIsolatedHerdr(t, ctx, socketPath, "pane.report_agent", map[string]any{
		"pane_id": pane.PaneID, "source": "pal-test", "agent": "codex", "state": "idle",
	})
	event, err := statuses.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := herdr.DecodeAgentStatusEvent(event)
	if err != nil || status.PaneID != pane.PaneID || status.AgentStatus != herdr.AgentStatusIdle {
		t.Fatalf("状态事件不兼容: %+v, %v", status, err)
	}
	agent, err := client.GetAgent(ctx, pane.PaneID)
	if err != nil || agent.PaneID != pane.PaneID || agent.AgentStatus != herdr.AgentStatusIdle {
		t.Fatalf("Agent 信息不兼容: %+v, %v", agent, err)
	}
	for _, read := range []func(context.Context, string, int) (herdr.ReadResult, error){
		client.ReadRecent, client.ReadRecentANSI, client.ReadVisibleANSI,
	} {
		result, err := read(ctx, pane.PaneID, 10)
		if err != nil || result.PaneID != pane.PaneID {
			t.Fatalf("终端快照不兼容: pane=%s, %v", result.PaneID, err)
		}
	}
	snapshot, err = client.Snapshot(ctx)
	if err != nil || len(snapshot.Agents) != 1 {
		t.Fatalf("包含 Agent 的快照不兼容: %v", err)
	}
	isolateUserCache(t)
	application := startInteractiveApplication(t, writeInteractiveConfig(t, socketPath))
	if _, err := waitForRealInteractiveList(ctx, pane.PaneID, func(context.Context) (string, error) {
		return sendInteractiveLine(t, application, "/ls"), nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/1", "/con"} {
		if reply := sendInteractiveLine(t, application, command); !strings.Contains(reply, "[终端输出]") {
			t.Fatalf("%s 未返回终端快照: %s", command, reply)
		}
	}
}

func callIsolatedHerdr(t *testing.T, ctx context.Context, socketPath, method string, params any) {
	t.Helper()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	if err := json.NewEncoder(connection).Encode(map[string]any{"id": "compatibility", "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error json.RawMessage
	}
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Error) > 0 {
		t.Fatalf("%s 失败: %s", method, response.Error)
	}
}

func replaceTestEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
