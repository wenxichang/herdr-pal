package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/testkit"
)

const interactivePrompt = "interactive-sensitive-prompt"

func TestInteractiveBridgeEndToEnd(t *testing.T) {
	cacheRoot, cacheDir := isolateUserCache(t)
	herdrServer := testkit.NewHerdrServer(t, integrationSnapshot("interactive-session", herdr.AgentStatusIdle))
	configPath := writeInteractiveConfig(t, herdrServer.SocketPath())
	application := startInteractiveApplication(t, configPath)

	waitForConsoleOutput(t, application.stdout, 0, "交互模式 banner", func(output string) bool {
		return strings.Contains(output, "Herdr Pal 交互模式") && strings.Contains(output, "herdr-pal> ")
	})
	assertIsolatedLockDirectory(t, cacheRoot, cacheDir)
	herdrServer.WaitCallCount(t, "ping", 1)
	herdrServer.WaitCallCount(t, "session.snapshot", 2)
	herdrServer.WaitSubscription(t, herdr.LifecycleSubscriptions())
	herdrServer.WaitSubscription(t, []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}})

	listReply := sendInteractiveLine(t, application, "/ls")
	assertReplyBlock(t, listReply)
	if !strings.Contains(listReply, "pane-1") {
		t.Fatalf("/ls 回复未包含 pane-1：%q", listReply)
	}

	selectReply := sendInteractiveLine(t, application, "/1")
	assertReplyBlock(t, selectReply)
	if !strings.Contains(selectReply, "[终端输出]") || !strings.Contains(selectReply, "页码:[1/1]") {
		t.Fatalf("/1 回复 = %q, want terminal content", selectReply)
	}

	herdrServer.SetOutput(numberedLines(250))
	contentReply := sendInteractiveLine(t, application, "/con")
	assertReplyBlock(t, contentReply)
	if !strings.Contains(contentReply, "line-151") || !strings.Contains(contentReply, "line-250") || strings.Contains(contentReply, "line-150") {
		t.Fatalf("/con 回复不代表最后 100 行：%q", contentReply)
	}
	readCalls := herdrServer.WaitCallCount(t, "agent.read", 2)
	assertCallParams(t, readCalls[1], map[string]any{"target": "pane-1", "lines": float64(100)})

	promptReply := sendInteractiveLine(t, application, interactivePrompt)
	assertReplyBlock(t, promptReply)
	if !strings.Contains(promptReply, "已发送") {
		t.Fatalf("普通 prompt 回复 = %q, want 已发送", promptReply)
	}
	promptCalls := herdrServer.WaitCallCount(t, "agent.prompt", 1)
	assertCallParams(t, promptCalls[0], map[string]any{
		"target": "pane-1",
		"text":   interactivePrompt,
		"wait": map[string]any{
			"until": []any{"idle", "working", "blocked", "done", "unknown"},
		},
	})

	keyReply := sendInteractiveLine(t, application, "/key sp")
	assertReplyBlock(t, keyReply)
	if !strings.Contains(keyReply, "按键已发送") || !strings.Contains(keyReply, "line-250") {
		t.Fatalf("/key sp 回复 = %q, want 按键结果和控制台内容", keyReply)
	}
	keyCalls := herdrServer.WaitCallCount(t, "agent.send_keys", 1)
	assertCallParams(t, keyCalls[0], map[string]any{"target": "pane-1", "keys": []any{"space"}})
	readCalls = herdrServer.WaitCallCount(t, "agent.read", 3)
	assertCallParams(t, readCalls[2], map[string]any{"target": "pane-1", "lines": float64(100)})
	waitForConsoleOutput(t, application.stderr, 0, "显式按键审计", func(output string) bool {
		return strings.Contains(output, "user_hash=") &&
			!strings.Contains(output, "interactive-local") &&
			strings.Contains(output, "pane_id=pane-1") &&
			strings.Contains(output, "key=space") &&
			strings.Contains(output, "result=sent")
	})

	herdrServer.SetOutput(numberedLines(180))
	notificationStart := application.stdout.Len()
	agent := "codex"
	if delivered := herdrServer.EmitStatus(herdr.AgentStatusEvent{
		PaneID: "pane-1", WorkspaceID: "workspace-1", AgentStatus: herdr.AgentStatusBlocked, Agent: &agent,
	}); delivered != 1 {
		t.Fatalf("blocked 事件写入订阅数 = %d, want 1", delivered)
	}
	notification := waitForConsoleOutput(t, application.stdout, notificationStart, "blocked 主动通知", func(output string) bool {
		return strings.Count(output, "\n[通知]\n") == 1 && strings.Contains(output, "Agent 已阻塞") &&
			strings.HasSuffix(output, "herdr-pal> ")
	})
	if strings.Contains(notification, "line-180") {
		t.Fatalf("blocked 状态通知包含终端内容：%q", notification)
	}
	assertCompleteConsoleBlocks(t, notification, "通知", 1)

	assertStableCount(t, func() int { return len(herdrServer.Calls("agent.prompt")) }, 1)
	assertStableCount(t, func() int { return len(herdrServer.Calls("agent.send_keys")) }, 1)
	if calls := herdrServer.Calls("agent.read"); len(calls) != 3 {
		t.Fatalf("agent.read 调用数 = %d, want 3", len(calls))
	}

	stdout := application.stdout.String()
	for _, fragment := range []string{"herdr-pal> ", "[回复]", "[通知]"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("stdout 未包含 %q：%q", fragment, stdout)
		}
	}
	stderr := application.stderr.String()
	for _, secret := range []string{interactivePrompt, "line-151", "line-180", "Secret", "Cookie"} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("stderr 泄漏敏感内容 %q：%q", secret, stderr)
		}
	}

	application.closeInputAndWait(t)
	if output := application.stdout.String(); strings.Contains(output, "启动失败") {
		t.Fatalf("EOF 正常退出却输出启动失败：%q", output)
	}

	second := startInteractiveApplication(t, configPath)
	waitForConsoleOutput(t, second.stdout, 0, "第二实例 banner", func(output string) bool {
		return strings.Contains(output, "Herdr Pal 交互模式") && strings.Contains(output, "herdr-pal> ")
	})
	herdrServer.WaitCallCount(t, "ping", 2)
	herdrServer.WaitCallCount(t, "session.snapshot", 4)
	herdrServer.WaitSubscriptionCount(t, herdr.LifecycleSubscriptions(), 2)
	herdrServer.WaitSubscriptionCount(t, []herdr.SubscriptionSpec{{Type: "pane.agent_status_changed", PaneID: "pane-1"}}, 2)
	second.closeInputAndWait(t)
}

type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *safeBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(value)
}

func (b *safeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

type interactiveApplication struct {
	cancel context.CancelFunc
	stdinR *io.PipeReader
	stdinW *io.PipeWriter
	stdout *safeBuffer
	stderr *safeBuffer
	done   chan struct{}

	mu         sync.Mutex
	err        error
	panicValue any
	reportOnce sync.Once
}

func startInteractiveApplication(t *testing.T, configPath string) *interactiveApplication {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	application := &interactiveApplication{
		cancel: cancel, stdinR: stdinR, stdinW: stdinW,
		stdout: &safeBuffer{}, stderr: &safeBuffer{}, done: make(chan struct{}),
	}
	go func() {
		defer close(application.done)
		defer func() {
			if recovered := recover(); recovered != nil {
				application.mu.Lock()
				application.panicValue = recovered
				application.mu.Unlock()
			}
		}()
		err := app.Run(ctx, app.Options{
			Interactive: true,
			ConfigPath:  configPath,
			Stdin:       stdinR,
			Stdout:      application.stdout,
			Stderr:      application.stderr,
			Getenv: func(name string) string {
				panic(fmt.Sprintf("交互模式不应读取环境变量 %s", name))
			},
		})
		application.mu.Lock()
		application.err = err
		application.mu.Unlock()
	}()
	t.Cleanup(func() { application.cleanup(t) })
	return application
}

func (a *interactiveApplication) closeInputAndWait(t *testing.T) {
	t.Helper()
	if err := a.stdinW.Close(); err != nil {
		t.Fatalf("关闭交互输入 writer：%v", err)
	}
	a.wait(t)
}

func (a *interactiveApplication) cleanup(t *testing.T) {
	t.Helper()
	a.cancel()
	_ = a.stdinW.Close()
	_ = a.stdinR.Close()
	a.wait(t)
}

func (a *interactiveApplication) wait(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	timedOut := false
	select {
	case <-a.done:
	case <-timer.C:
		timedOut = true
	}

	a.mu.Lock()
	err := a.err
	panicValue := a.panicValue
	a.mu.Unlock()
	a.reportOnce.Do(func() {
		if timedOut {
			t.Errorf("app.Run() 未在 3 秒内退出")
			return
		}
		if panicValue != nil {
			t.Errorf("app.Run() panic: %v", panicValue)
			return
		}
		if err != nil {
			t.Errorf("app.Run() error = %v, want nil", err)
		}
	})
}

func isolateUserCache(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("解析测试私有用户缓存目录：%v", err)
	}
	assertPathWithin(t, root, cacheDir, "用户缓存目录")
	return root, cacheDir
}

func assertIsolatedLockDirectory(t *testing.T, cacheRoot, cacheDir string) {
	t.Helper()
	lockDir := filepath.Join(cacheDir, "herdr-pal")
	assertPathWithin(t, cacheRoot, lockDir, "进程锁目录")
	info, err := os.Stat(lockDir)
	if err != nil {
		t.Fatalf("读取测试私有进程锁目录：%v", err)
	}
	if !info.IsDir() {
		t.Fatalf("测试私有进程锁路径不是目录：%q", lockDir)
	}
}

func assertPathWithin(t *testing.T, root, path, description string) {
	t.Helper()
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("%s %q 不在测试私有目录 %q 内：relative=%q err=%v", description, path, root, relative, err)
	}
}

func writeInteractiveConfig(t *testing.T, socketPath string) string {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"herdr": map[string]any{"socket_path": socketPath},
		"log":   map[string]any{"level": "error"},
	})
	if err != nil {
		t.Fatalf("编码交互集成测试配置：%v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("写入交互集成测试配置：%v", err)
	}
	return path
}

func sendInteractiveLine(t *testing.T, application *interactiveApplication, line string) string {
	t.Helper()
	start := application.stdout.Len()
	if _, err := io.WriteString(application.stdinW, line+"\n"); err != nil {
		t.Fatalf("写入交互命令 %q：%v", line, err)
	}
	return waitForConsoleOutput(t, application.stdout, start, "交互命令回复", func(output string) bool {
		return strings.Contains(output, "\n[回复]\n") && strings.HasSuffix(output, "herdr-pal> ")
	})
}

func waitForConsoleOutput(t *testing.T, buffer *safeBuffer, start int, description string, condition func(string) bool) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		output := buffer.String()
		if len(output) >= start {
			chunk := output[start:]
			if condition(chunk) {
				return chunk
			}
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatalf("等待超时：%s；当前输出 %q", description, output)
			return ""
		}
	}
}

func assertReplyBlock(t *testing.T, output string) {
	t.Helper()
	assertCompleteConsoleBlocks(t, output, "回复", 1)
}

func assertCompleteConsoleBlocks(t *testing.T, output, label string, count int) {
	t.Helper()
	delimiter := "\n\nherdr-pal> "
	parts := strings.SplitAfter(output, delimiter)
	if len(parts) != count+1 || parts[len(parts)-1] != "" {
		t.Fatalf("%s 输出不是 %d 个完整块：%q", label, count, output)
	}
	for index := 0; index < count; index++ {
		if !strings.HasPrefix(parts[index], "\n["+label+"]\n") || !strings.HasSuffix(parts[index], delimiter) {
			t.Fatalf("%s 输出块 %d 不完整：%q", label, index+1, parts[index])
		}
	}
}
