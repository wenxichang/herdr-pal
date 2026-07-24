package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/app"
	"github.com/wenxichang/herdr-pal/internal/processlock"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestRunParsesCLIAndMapsExitCodes(t *testing.T) {
	originalVersion, originalCommit, originalBuiltAt := version.Version, version.Commit, version.BuiltAt
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuiltAt = originalVersion, originalCommit, originalBuiltAt
	})
	version.Version = "v1.2.3"
	version.Commit = "abc123"
	version.BuiltAt = "2026-07-23T00:00:00Z"

	tests := []struct {
		name            string
		args            []string
		execute         appExecutor
		cancelContext   bool
		wantCode        int
		wantStdout      string
		wantStderrPart  string
		forbidStderr    []string
		wantConfigPath  string
		wantInteractive bool
		wantExecutions  int
	}{
		{name: "显示版本", args: []string{"--version"}, wantStdout: version.String() + "\n"},
		{
			name: "交互模式启动成功", args: []string{"-i"}, wantInteractive: true, wantExecutions: 1,
			execute: func(context.Context, app.Options) error { return nil },
		},
		{
			name: "交互模式允许配置路径", args: []string{"-i", "-config", "local.json"}, wantConfigPath: "local.json", wantInteractive: true, wantExecutions: 1,
			execute: func(context.Context, app.Options) error { return nil },
		},
		{name: "没有参数", wantCode: 2, wantStderrPart: "-config"},
		{name: "未知参数", args: []string{"--unknown"}, wantCode: 2, wantStderrPart: "参数错误"},
		{name: "交互模式和版本互斥", args: []string{"-i", "--version"}, wantCode: 2, wantStderrPart: "用法"},
		{name: "版本参数后有额外内容", args: []string{"--version", "extra"}, wantCode: 2, wantStderrPart: "用法"},
		{name: "交互模式有额外内容", args: []string{"-i", "extra"}, wantCode: 2, wantStderrPart: "用法"},
		{
			name: "配置路径启动成功", args: []string{"-config", "/tmp/pal.json"},
			execute: func(context.Context, app.Options) error { return nil }, wantConfigPath: "/tmp/pal.json", wantExecutions: 1,
		},
		{name: "用户发现模式已移除", args: []string{"-discover-user", "-config", "/tmp/pal.json"}, wantCode: 2, wantStderrPart: "参数错误"},
		{
			name: "配置错误退出二", args: []string{"-config", "/tmp/bad.json"}, wantCode: 2,
			execute: func(context.Context, app.Options) error {
				return errors.Join(app.ErrConfig, errors.New("secret-sensitive"))
			}, wantStderrPart: "配置错误", forbidStderr: []string{"secret-sensitive"}, wantConfigPath: "/tmp/bad.json", wantExecutions: 1,
		},
		{
			name: "运行错误退出一", args: []string{"-config", "/tmp/pal.json"}, wantCode: 1,
			execute: func(context.Context, app.Options) error {
				return errors.New("secret-sensitive complete-prompt complete-terminal")
			}, wantStderrPart: "启动或运行失败", forbidStderr: []string{"secret-sensitive", "complete-prompt", "complete-terminal"}, wantConfigPath: "/tmp/pal.json", wantExecutions: 1,
		},
		{
			name: "锁冲突保留清晰提示", args: []string{"-config", "/tmp/pal.json"}, wantCode: 1,
			execute: func(context.Context, app.Options) error { return processlock.ErrAlreadyRunning }, wantStderrPart: "已在运行", wantConfigPath: "/tmp/pal.json", wantExecutions: 1,
		},
		{
			name: "正常取消退出零", args: []string{"-config", "/tmp/pal.json"}, cancelContext: true,
			execute: func(ctx context.Context, _ app.Options) error { return ctx.Err() }, wantConfigPath: "/tmp/pal.json", wantExecutions: 1,
		},
		{
			name: "交互模式配置错误退出二", args: []string{"-i"}, wantCode: 2, wantInteractive: true,
			execute: func(context.Context, app.Options) error { return app.ErrConfig }, wantStderrPart: "配置错误", wantExecutions: 1,
		},
		{
			name: "交互模式运行错误退出一", args: []string{"-i"}, wantCode: 1, wantInteractive: true,
			execute: func(context.Context, app.Options) error { return errors.New("runtime") }, wantStderrPart: "启动或运行失败", wantExecutions: 1,
		},
		{
			name: "交互模式锁冲突退出一", args: []string{"-i"}, wantCode: 1, wantInteractive: true,
			execute: func(context.Context, app.Options) error { return processlock.ErrAlreadyRunning }, wantStderrPart: "已在运行", wantExecutions: 1,
		},
		{
			name: "交互模式正常取消退出零", args: []string{"-i"}, cancelContext: true, wantInteractive: true,
			execute: func(ctx context.Context, _ app.Options) error { return ctx.Err() }, wantExecutions: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			stdin := bytes.NewBufferString("interactive input")
			t.Setenv("HERDR_PAL_TEST_GETENV", "expected")
			ctx, cancel := context.WithCancel(context.Background())
			if test.cancelContext {
				cancel()
			} else {
				defer cancel()
			}
			execute := test.execute
			if execute == nil {
				execute = func(context.Context, app.Options) error {
					t.Fatal("application should not start")
					return nil
				}
			}
			var gotOptions app.Options
			executions := 0
			wrapped := func(ctx context.Context, options app.Options) error {
				executions++
				gotOptions = options
				return execute(ctx, options)
			}

			if got := run(ctx, test.args, stdin, &stdout, &stderr, wrapped); got != test.wantCode {
				t.Errorf("run() = %d, want %d", got, test.wantCode)
			}
			if got := stdout.String(); got != test.wantStdout {
				t.Errorf("stdout = %q, want %q", got, test.wantStdout)
			}
			if test.wantStderrPart != "" && !strings.Contains(stderr.String(), test.wantStderrPart) {
				t.Errorf("stderr = %q, want to contain %q", stderr.String(), test.wantStderrPart)
			}
			for _, forbidden := range test.forbidStderr {
				if strings.Contains(stderr.String(), forbidden) {
					t.Errorf("stderr contains sensitive value %q: %q", forbidden, stderr.String())
				}
			}
			if gotOptions.ConfigPath != test.wantConfigPath {
				t.Errorf("ConfigPath = %q, want %q", gotOptions.ConfigPath, test.wantConfigPath)
			}
			if gotOptions.Interactive != test.wantInteractive {
				t.Errorf("Interactive = %t, want %t", gotOptions.Interactive, test.wantInteractive)
			}
			if executions != test.wantExecutions {
				t.Errorf("execute() calls = %d, want %d", executions, test.wantExecutions)
			}
			if executions == 1 {
				if gotOptions.Stdin != stdin {
					t.Error("Stdin was not passed to app options")
				}
				if gotOptions.Getenv == nil || gotOptions.Getenv("HERDR_PAL_TEST_GETENV") != "expected" {
					t.Error("Getenv was not passed to app options")
				}
				if gotOptions.Stdout != &stdout {
					t.Error("Stdout was not passed to app options")
				}
				if gotOptions.Stderr != &stderr {
					t.Error("Stderr was not passed to app options")
				}
			}
		})
	}
}
