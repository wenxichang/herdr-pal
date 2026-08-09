package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/herdr"
	"github.com/wenxichang/herdr-pal/internal/session"
)

const (
	realInteractiveReadyLimit = 10 * time.Second
	realInteractiveRetryDelay = 50 * time.Millisecond
	realInteractiveReplyLimit = 10 * time.Second
	liveMarker                = "HERDR_PAL_LIVE_OK"
	livePrompt                = "这是 Herdr Pal 实时集成测试。请只回复由 HERDR、PAL、LIVE、OK 四个词使用下划线连接成的字符串；不要调用工具，不要修改文件。"
)

type realHerdrFixture struct {
	ctx        context.Context
	socketPath string
	client     *herdr.Client
	snapshot   herdr.Snapshot
}

type realHerdrStatus struct {
	Running  bool
	Protocol uint32
	Socket   string
}

func TestEvaluateRealHerdrStatus(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantReady  bool
		wantErr    bool
		wantSocket string
	}{
		{name: "完整兼容状态通过", output: `{"running":true,"protocol":17,"socket":" /tmp/herdr.sock "}`, wantReady: true, wantSocket: "/tmp/herdr.sock"},
		{name: "Herdr 0.8 兼容状态通过", output: `{"running":true,"protocol":19,"socket":" /tmp/herdr-08.sock "}`, wantReady: true, wantSocket: "/tmp/herdr-08.sock"},
		{name: "服务未运行不要求后续字段", output: `{"running":false}`},
		{name: "协议不兼容不要求 socket", output: `{"running":true,"protocol":14}`},
		{name: "未审计中间协议不要求 socket", output: `{"running":true,"protocol":18}`},
		{name: "成功命令返回非法 JSON", output: `{`, wantErr: true},
		{name: "缺少 running", output: `{"protocol":17,"socket":"/tmp/herdr.sock"}`, wantErr: true},
		{name: "缺少 protocol", output: `{"running":true,"socket":"/tmp/herdr.sock"}`, wantErr: true},
		{name: "缺少 socket", output: `{"running":true,"protocol":17}`, wantErr: true},
		{name: "兼容运行状态的 socket 为空", output: `{"running":true,"protocol":17,"socket":"  "}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, ready, err := evaluateRealHerdrStatus([]byte(test.output))
			if (err != nil) != test.wantErr {
				t.Fatalf("evaluateRealHerdrStatus() error = %v, wantErr %t", err, test.wantErr)
			}
			if ready != test.wantReady {
				t.Fatalf("evaluateRealHerdrStatus() ready = %t, want %t", ready, test.wantReady)
			}
			if test.wantReady && status.Socket != test.wantSocket {
				t.Fatalf("evaluateRealHerdrStatus() socket = %q, want %q", status.Socket, test.wantSocket)
			}
		})
	}
}

func TestWaitForRealInteractiveListRetriesUntilTargetAppears(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replies := []string{"Herdr 暂不可用", "当前没有可选择的 Agent", "pane-real"}
	attempts := 0
	got, err := waitForRealInteractiveList(ctx, "pane-real", func(context.Context) (string, error) {
		reply := replies[attempts]
		attempts++
		return reply, nil
	})
	if err != nil {
		t.Fatalf("waitForRealInteractiveList() error = %v", err)
	}
	if got != "pane-real" || attempts != 3 {
		t.Fatalf("waitForRealInteractiveList() = %q, attempts = %d", got, attempts)
	}
}

func TestWaitForRealInteractiveListDeadlineDoesNotLeakReplies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	const sensitiveReply = "terminal-sensitive-output"
	_, err := waitForRealInteractiveList(ctx, "pane-missing", func(context.Context) (string, error) {
		return sensitiveReply, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForRealInteractiveList() error = %v, want deadline", err)
	}
	if strings.Contains(err.Error(), sensitiveReply) {
		t.Fatalf("waitForRealInteractiveList() error 泄漏回复内容：%v", err)
	}
}

func TestRealHerdr(t *testing.T) {
	fixture := requireRealHerdr(t)
	paneID := ""
	for _, pane := range fixture.snapshot.Panes {
		if pane.Agent != nil {
			paneID = pane.PaneID
			break
		}
	}
	if paneID == "" {
		t.Skip("真实 Herdr 当前没有带 Agent 的 pane")
	}

	registry := &session.Registry{}
	registry.Replace(fixture.snapshot, false)
	targets := registry.CreateListSnapshot()
	var target session.Target
	selectionIndex := 0
	for index, candidate := range targets {
		if candidate.PaneID == paneID {
			target = candidate
			selectionIndex = index + 1
			break
		}
	}
	if selectionIndex == 0 {
		t.Fatalf("真实 Herdr Registry 未索引 snapshot Agent pane %s", paneID)
	}

	current, err := fixture.client.GetAgent(fixture.ctx, target.PaneID)
	if err != nil {
		t.Fatalf("真实 Herdr GetAgent(%s) error = %v", target.PaneID, err)
	}
	if !session.MatchesAgent(target, current) {
		t.Fatalf("真实 Herdr pane %s 的 occupant 与 snapshot 不一致", target.PaneID)
	}
	result, err := fixture.client.ReadRecent(fixture.ctx, target.PaneID, 1)
	if err != nil {
		t.Fatalf("真实 Herdr ReadRecent(%s, 1) error = %v", target.PaneID, err)
	}
	if result.PaneID != target.PaneID {
		t.Fatalf("真实 Herdr ReadRecent pane = %s, want %s", result.PaneID, target.PaneID)
	}

	isolateUserCache(t)
	configPath := writeInteractiveConfig(t, fixture.socketPath)
	application := startInteractiveApplication(t, configPath)
	waitForRealInteractiveBanner(t, application)
	readinessContext, cancelReadiness := context.WithTimeout(fixture.ctx, realInteractiveReadyLimit)
	listReply, err := waitForRealInteractiveList(readinessContext, target.PaneID, func(ctx context.Context) (string, error) {
		return sendRealInteractiveLineContext(ctx, application, "/ls")
	})
	cancelReadiness()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("等待目标 pane %s 在 %s 内出现在真实交互 /ls 超时", target.PaneID, realInteractiveReadyLimit)
		}
		t.Fatalf("等待真实交互 /ls 就绪失败，目标 pane %s：%v", target.PaneID, err)
	}
	selectReply := sendRealInteractiveLine(t, application, fmt.Sprintf("/%d", selectionIndex))
	if !strings.Contains(selectReply, target.PaneID) {
		t.Fatalf("真实交互 /N 未选择目标 pane %s", target.PaneID)
	}
	contentReply := sendRealInteractiveLine(t, application, "/con")
	joined := listReply + selectReply + contentReply
	if strings.Count(joined, "[回复]") < 3 {
		t.Fatal("真实交互命令未全部返回 [回复] 块")
	}
	if strings.Contains(joined, "暂不可用") || strings.Contains(joined, "agent_not_found") {
		t.Fatalf("真实交互只读流程返回不可用错误，目标 pane %s", target.PaneID)
	}
	application.closeInputAndWait(t)
}

func TestRealHerdrLivePrompt(t *testing.T) {
	if os.Getenv("HERDR_PAL_INTEGRATION") != "1" {
		t.Skip("设置 HERDR_PAL_INTEGRATION=1 后才运行真实 Herdr 集成测试")
	}
	if os.Getenv("HERDR_PAL_LIVE_INPUT") != "1" {
		t.Skip("设置 HERDR_PAL_LIVE_INPUT=1 后才运行真实 Herdr 输入测试")
	}
	paneID := strings.TrimSpace(os.Getenv("HERDR_PAL_LIVE_PANE_ID"))
	if paneID == "" {
		t.Skip("设置 HERDR_PAL_LIVE_PANE_ID 后才运行真实 Herdr 输入测试")
	}

	fixture := requireRealHerdr(t)
	registry := &session.Registry{}
	registry.Replace(fixture.snapshot, false)
	var target session.Target
	found := false
	for _, candidate := range registry.CreateListSnapshot() {
		if candidate.PaneID == paneID {
			target = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("真实 Herdr snapshot 中不存在指定 Agent pane %s", paneID)
	}

	current, err := fixture.client.GetAgent(fixture.ctx, paneID)
	if err != nil {
		t.Fatalf("真实 Herdr GetAgent(%s) error = %v", paneID, err)
	}
	if !session.MatchesAgent(target, current) {
		t.Fatalf("真实 Herdr pane %s 的 occupant 与 snapshot 不一致", paneID)
	}
	baseline, err := fixture.client.ReadRecent(fixture.ctx, paneID, 100)
	if err != nil {
		t.Fatalf("真实 Herdr ReadRecent(%s, 100) error = %v", paneID, err)
	}
	baselineCount := strings.Count(baseline.Text, liveMarker)

	if err := fixture.client.Prompt(fixture.ctx, paneID, livePrompt); err != nil {
		t.Fatalf("真实 Herdr Prompt() error = %v", err)
	}

	waitForLiveMarker(t, fixture, paneID, baselineCount)
}

func requireRealHerdr(t *testing.T) realHerdrFixture {
	t.Helper()
	if os.Getenv("HERDR_PAL_INTEGRATION") != "1" {
		t.Skip("设置 HERDR_PAL_INTEGRATION=1 后才运行真实 Herdr 集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	output, err := exec.CommandContext(ctx, "herdr", "status", "server", "--json").Output()
	if err != nil {
		t.Skipf("Herdr 公共 CLI 状态不可用：%v", err)
	}
	status, ready, err := evaluateRealHerdrStatus(output)
	if err != nil {
		t.Fatalf("Herdr 公共 CLI 状态响应无效：%v", err)
	}
	if !ready {
		t.Skipf("已安装 Herdr 尚未满足真实联调门禁：running=%t protocol=%d，需要已审计协议 17 或 19", status.Running, status.Protocol)
	}

	client := herdr.NewClient(status.Socket, nil, 5*time.Second)
	if err := client.CheckCompatible(ctx); err != nil {
		t.Fatalf("真实 Herdr CheckCompatible() error = %v", err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatalf("真实 Herdr Snapshot() error = %v", err)
	}
	if !herdr.IsSupportedProtocol(snapshot.Protocol) {
		t.Fatalf("真实 Herdr snapshot protocol = %d，未通过兼容门禁", snapshot.Protocol)
	}
	return realHerdrFixture{ctx: ctx, socketPath: status.Socket, client: client, snapshot: snapshot}
}

func evaluateRealHerdrStatus(output []byte) (realHerdrStatus, bool, error) {
	var wire struct {
		Running  *bool   `json:"running"`
		Protocol *uint32 `json:"protocol"`
		Socket   *string `json:"socket"`
	}
	if err := json.Unmarshal(output, &wire); err != nil {
		return realHerdrStatus{}, false, fmt.Errorf("JSON 无法解析: %w", err)
	}
	if wire.Running == nil {
		return realHerdrStatus{}, false, errors.New("缺少 running 必填字段")
	}
	status := realHerdrStatus{Running: *wire.Running}
	if !status.Running {
		return status, false, nil
	}
	if wire.Protocol == nil {
		return realHerdrStatus{}, false, errors.New("运行中的服务缺少 protocol 必填字段")
	}
	status.Protocol = *wire.Protocol
	if !herdr.IsSupportedProtocol(status.Protocol) {
		return status, false, nil
	}
	if wire.Socket == nil {
		return realHerdrStatus{}, false, errors.New("兼容且运行中的服务缺少 socket 必填字段")
	}
	status.Socket = strings.TrimSpace(*wire.Socket)
	if status.Socket == "" {
		return realHerdrStatus{}, false, errors.New("兼容且运行中的服务缺少 socket 路径")
	}
	return status, true, nil
}

func waitForRealInteractiveBanner(t *testing.T, application *interactiveApplication) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(application.stdout.String(), "herdr-pal> ") {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("等待真实交互模式命令提示符超时")
		}
	}
}

func sendRealInteractiveLine(t *testing.T, application *interactiveApplication, line string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), realInteractiveReplyLimit)
	defer cancel()
	chunk, err := sendRealInteractiveLineContext(ctx, application, line)
	if err != nil {
		t.Fatalf("等待真实交互命令 %q 回复失败：%v", line, err)
	}
	return chunk
}

func sendRealInteractiveLineContext(ctx context.Context, application *interactiveApplication, line string) (string, error) {
	start := application.stdout.Len()
	if _, err := io.WriteString(application.stdinW, line+"\n"); err != nil {
		return "", err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		output := application.stdout.String()
		if len(output) >= start {
			chunk := output[start:]
			if strings.Contains(chunk, "\n[回复]\n") && strings.HasSuffix(chunk, "herdr-pal> ") {
				return chunk, nil
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func waitForRealInteractiveList(ctx context.Context, paneID string, send func(context.Context) (string, error)) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		reply, err := send(ctx)
		if err != nil {
			return "", err
		}
		if strings.Contains(reply, paneID) {
			return reply, nil
		}
		timer := time.NewTimer(realInteractiveRetryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", ctx.Err()
		}
	}
}

func waitForLiveMarker(t *testing.T, fixture realHerdrFixture, paneID string, baselineCount int) {
	t.Helper()
	const waitLimit = 90 * time.Second
	pollContext, cancel := context.WithTimeout(fixture.ctx, waitLimit)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-pollContext.Done():
			t.Fatalf("等待 pane %s 在 %s 内新增 marker %s 超时", paneID, waitLimit, liveMarker)
		case <-ticker.C:
			result, err := fixture.client.ReadRecent(pollContext, paneID, 100)
			if err != nil {
				if pollContext.Err() != nil {
					t.Fatalf("等待 pane %s 在 %s 内新增 marker %s 超时", paneID, waitLimit, liveMarker)
				}
				t.Fatalf("轮询真实 Herdr pane %s marker 失败：%v", paneID, err)
			}
			if strings.Count(result.Text, liveMarker) > baselineCount {
				return
			}
		}
	}
}
