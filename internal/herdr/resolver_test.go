package herdr

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveSocketFallsBackToDefaultHomeSocketWhenCLIFails(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "hp-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "herdr", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runner := &fakeCommandRunner{err: errors.New("herdr CLI unavailable")}

	resolved, err := ResolveSocket(context.Background(), "", "", runner)

	if err != nil {
		t.Fatalf("ResolveSocket() error = %v", err)
	}
	if resolved != path {
		t.Fatalf("ResolveSocket() = %q, want %q", resolved, path)
	}
	assertCommandCall(t, runner, "herdr", "status", "server", "--json")
}

func TestResolveSocketDoesNotInferNamedHomeSocketWhenCLIFails(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "hp-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "herdr", "sessions", "work", "herdr.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	runner := &fakeCommandRunner{err: errors.New("herdr CLI unavailable")}

	_, err = ResolveSocket(context.Background(), "", "work", runner)

	if err == nil {
		t.Fatal("ResolveSocket() should not infer a named HOME socket")
	}
	assertCommandCall(t, runner, "herdr", "session", "list", "--json")
}

type fakeCommandRunner struct {
	output []byte
	err    error
	calls  []commandCall
}

type commandCall struct {
	name string
	args []string
}

func (r *fakeCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, commandCall{name: name, args: append([]string(nil), args...)})
	return r.output, r.err
}

func TestResolveSocketReturnsExplicitPathWithoutRunningCLI(t *testing.T) {
	runner := &fakeCommandRunner{}

	path, err := ResolveSocket(context.Background(), " /configured/herdr.sock ", "named", runner)

	if err != nil {
		t.Fatalf("ResolveSocket() 返回错误：%v", err)
	}
	if path != " /configured/herdr.sock " {
		t.Fatalf("ResolveSocket() = %q，期望原样返回显式路径", path)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("CLI 调用次数 = %d，期望 0", len(runner.calls))
	}
}

func TestResolveSocketResolvesDefaultSessionFromServerStatus(t *testing.T) {
	runner := &fakeCommandRunner{output: []byte(`{"running":true,"socket":"/tmp/herdr.sock","future":1}`)}

	path, err := ResolveSocket(context.Background(), "", "default", runner)

	if err != nil {
		t.Fatalf("ResolveSocket() 返回错误：%v", err)
	}
	if path != "/tmp/herdr.sock" {
		t.Fatalf("ResolveSocket() = %q，期望 /tmp/herdr.sock", path)
	}
	assertCommandCall(t, runner, "herdr", "status", "server", "--json")
}

func TestResolveSocketResolvesEmptySessionAsDefault(t *testing.T) {
	runner := &fakeCommandRunner{output: []byte(`{"running":true,"socket":" /tmp/herdr.sock "}`)}

	path, err := ResolveSocket(context.Background(), "", "", runner)

	if err != nil {
		t.Fatalf("ResolveSocket() 返回错误：%v", err)
	}
	if path != " /tmp/herdr.sock " {
		t.Fatalf("ResolveSocket() = %q，期望原样返回 CLI 路径", path)
	}
	assertCommandCall(t, runner, "herdr", "status", "server", "--json")
}

func TestResolveSocketResolvesNamedRunningSession(t *testing.T) {
	runner := &fakeCommandRunner{output: []byte(`{
  "sessions": [
    {"name":"other","running":true,"socket_path":"/tmp/other.sock"},
    {"name":"bridge","running":true,"socket_path":"/tmp/bridge.sock"}
  ]
}`)}

	path, err := ResolveSocket(context.Background(), "", "bridge", runner)

	if err != nil {
		t.Fatalf("ResolveSocket() 返回错误：%v", err)
	}
	if path != "/tmp/bridge.sock" {
		t.Fatalf("ResolveSocket() = %q，期望 /tmp/bridge.sock", path)
	}
	assertCommandCall(t, runner, "herdr", "session", "list", "--json")
}

func TestResolveSocketRejectsUnavailableOrMalformedCLIResults(t *testing.T) {
	tests := []struct {
		name        string
		sessionName string
		output      string
		err         error
	}{
		{name: "runner 为空", sessionName: "default"},
		{name: "CLI 失败", sessionName: "default", err: errors.New("command failed")},
		{name: "非法 JSON", sessionName: "default", output: "{"},
		{name: "默认会话未运行", sessionName: "default", output: `{"running":false,"socket":"/tmp/hidden.sock"}`},
		{name: "默认会话缺少 socket", sessionName: "default", output: `{"running":true}`},
		{name: "默认会话空白 socket", sessionName: "default", output: `{"running":true,"socket":" \t "}`},
		{name: "默认会话字段类型错误", sessionName: "default", output: `{"running":"yes","socket":"/tmp/hidden.sock"}`},
		{name: "命名会话不存在", sessionName: "named", output: `{"sessions":[]}`},
		{name: "命名会话未运行", sessionName: "named", output: `{"sessions":[{"name":"named","running":false,"socket_path":"/tmp/hidden.sock"}]}`},
		{name: "命名会话缺少 socket", sessionName: "named", output: `{"sessions":[{"name":"named","running":true}]}`},
		{name: "命名会话空白 socket", sessionName: "named", output: `{"sessions":[{"name":"named","running":true,"socket_path":" "}]}`},
		{name: "命名会话字段类型错误", sessionName: "named", output: `{"sessions":[{"name":"named","running":"yes","socket_path":"/tmp/hidden.sock"}]}`},
		{name: "sessions 类型错误", sessionName: "named", output: `{"sessions":{}}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var runner CommandRunner
			if test.name != "runner 为空" {
				runner = &fakeCommandRunner{output: []byte(test.output), err: test.err}
			}

			_, err := ResolveSocket(context.Background(), "", test.sessionName, runner)

			if err == nil {
				t.Fatal("ResolveSocket() 未返回错误")
			}
			if strings.Contains(err.Error(), "/tmp/hidden.sock") {
				t.Fatalf("ResolveSocket() 错误泄露 socket 路径：%v", err)
			}
		})
	}
}

func TestResolveSocketPreservesCanceledRunnerContextWithoutLeakingOutput(t *testing.T) {
	sentinel := errors.New("runner failed with /tmp/sensitive.sock")
	runner := waitingCommandRunner{err: sentinel}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ResolveSocket(ctx, "", "default", runner)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveSocket() 错误 = %v，期望匹配 context.Canceled", err)
	}
	if strings.Contains(err.Error(), "/tmp/sensitive.sock") {
		t.Fatalf("ResolveSocket() 错误泄露 runner 信息：%v", err)
	}
}

func TestResolveSocketPreservesDeadlineRunnerContextWithoutLeakingOutput(t *testing.T) {
	sentinel := errors.New("runner failed with /tmp/sensitive.sock")
	runner := waitingCommandRunner{err: sentinel}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	_, err := ResolveSocket(ctx, "", "default", runner)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ResolveSocket() 错误 = %v，期望匹配 context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "/tmp/sensitive.sock") {
		t.Fatalf("ResolveSocket() 错误泄露 runner 信息：%v", err)
	}
}

type waitingCommandRunner struct {
	err error
}

func (r waitingCommandRunner) Output(ctx context.Context, _ string, _ ...string) ([]byte, error) {
	<-ctx.Done()
	return []byte(`{"socket":"/tmp/sensitive.sock"}`), r.err
}

func assertCommandCall(t *testing.T, runner *fakeCommandRunner, name string, args ...string) {
	t.Helper()
	if len(runner.calls) != 1 {
		t.Fatalf("CLI 调用次数 = %d，期望 1", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != name || strings.Join(call.args, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("CLI 调用 = %q %q，期望 %q %q", call.name, call.args, name, args)
	}
}
