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
		name           string
		args           []string
		execute        appExecutor
		cancelContext  bool
		wantCode       int
		wantStdout     string
		wantStderrPart string
		forbidStderr   []string
		wantConfigPath string
	}{
		{name: "显示版本", args: []string{"--version"}, wantStdout: version.String() + "\n"},
		{name: "没有参数", wantCode: 2, wantStderrPart: "-config"},
		{name: "未知参数", args: []string{"--unknown"}, wantCode: 2, wantStderrPart: "参数错误"},
		{name: "版本参数后有额外内容", args: []string{"--version", "extra"}, wantCode: 2, wantStderrPart: "用法"},
		{
			name: "配置路径启动成功", args: []string{"-config", "/tmp/pal.json"},
			execute: func(context.Context, app.Options) error { return nil }, wantConfigPath: "/tmp/pal.json",
		},
		{
			name: "配置错误退出二", args: []string{"-config", "/tmp/bad.json"}, wantCode: 2,
			execute: func(context.Context, app.Options) error {
				return errors.Join(app.ErrConfig, errors.New("secret-sensitive"))
			}, wantStderrPart: "配置错误", forbidStderr: []string{"secret-sensitive"}, wantConfigPath: "/tmp/bad.json",
		},
		{
			name: "运行错误退出一", args: []string{"-config", "/tmp/pal.json"}, wantCode: 1,
			execute: func(context.Context, app.Options) error {
				return errors.New("secret-sensitive complete-prompt complete-terminal")
			}, wantStderrPart: "启动或运行失败", forbidStderr: []string{"secret-sensitive", "complete-prompt", "complete-terminal"}, wantConfigPath: "/tmp/pal.json",
		},
		{
			name: "锁冲突保留清晰提示", args: []string{"-config", "/tmp/pal.json"}, wantCode: 1,
			execute: func(context.Context, app.Options) error { return processlock.ErrAlreadyRunning }, wantStderrPart: "已在运行", wantConfigPath: "/tmp/pal.json",
		},
		{
			name: "正常取消退出零", args: []string{"-config", "/tmp/pal.json"}, cancelContext: true,
			execute: func(ctx context.Context, _ app.Options) error { return ctx.Err() }, wantConfigPath: "/tmp/pal.json",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
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
			wrapped := func(ctx context.Context, options app.Options) error {
				gotOptions = options
				return execute(ctx, options)
			}

			if got := run(ctx, test.args, &stdout, &stderr, wrapped); got != test.wantCode {
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
		})
	}
}
