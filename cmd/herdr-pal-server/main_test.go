package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/serverapp"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestRunServerParsesConfigAndVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })
	version.Version = "v1.0.0"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--version"}, &stdout, &stderr, func(context.Context, serverapp.Options) error { return nil }); code != 0 {
		t.Fatalf("version code = %d", code)
	}
	if stdout.String() == "" {
		t.Fatal("version output is empty")
	}

	stdout.Reset()
	stderr.Reset()
	var gotDefault serverapp.Options
	code := run(context.Background(), nil, &stdout, &stderr, func(_ context.Context, options serverapp.Options) error {
		gotDefault = options
		return nil
	})
	wantDefault := filepath.Join(home, ".config", "herdr-pal-server", "server.json")
	if code != 0 || gotDefault.ConfigPath != wantDefault {
		t.Fatalf("default run() = %d, config path %q, want %q", code, gotDefault.ConfigPath, wantDefault)
	}
	if gotDefault.Stdout != &stdout {
		t.Fatal("default run did not pass stdout to serverapp")
	}

	stdout.Reset()
	stderr.Reset()
	var got serverapp.Options
	code = run(context.Background(), []string{"-config", "/tmp/server.json"}, &stdout, &stderr, func(_ context.Context, options serverapp.Options) error {
		got = options
		return nil
	})
	if code != 0 || got.ConfigPath != "/tmp/server.json" {
		t.Fatalf("run() = %d, options %#v", code, got)
	}

	stdout.Reset()
	stderr.Reset()
	var gotVerbose serverapp.Options
	code = run(context.Background(), []string{"--verbose", "-config", "/tmp/server.json"}, &stdout, &stderr, func(_ context.Context, options serverapp.Options) error {
		gotVerbose = options
		return nil
	})
	if code != 0 || !gotVerbose.Verbose || gotVerbose.ConfigPath != "/tmp/server.json" {
		t.Fatalf("verbose run() = %d, options %#v", code, gotVerbose)
	}
}

func TestRunServerRejectsLegacyKeyIssueCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"key", "issue", "-principal-id", "user-a", "-machine-id", "office-pc"}, &stdout, &stderr, func(context.Context, serverapp.Options) error {
		t.Fatal("executor should not be called")
		return nil
	})
	if code != 2 || !strings.Contains(stderr.String(), "hp-cli key issue") {
		t.Fatalf("run(key issue) = %d, stderr %q", code, stderr.String())
	}
}

func TestRunServerMapsConfigAndRuntimeErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		code          int
		wantParts     []string
		forbiddenPart string
	}{
		{
			name: "config detail", err: fmt.Errorf("%w: 缺少必填字段 bot_id", serverapp.ErrConfig), code: 2,
			wantParts: []string{"配置错误（/tmp/server.json）", "缺少必填字段 bot_id"},
		},
		{
			name: "runtime detail", err: errors.New("监听 Relay 地址: bind: address already in use"), code: 1,
			wantParts: []string{"Herdr Pal Server 启动或运行失败", "监听 Relay 地址: bind: address already in use"},
		},
		{
			name: "credential redaction", err: errors.New("认证失败: hpk_12_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), code: 1,
			wantParts: []string{"认证失败", "[REDACTED]"}, forbiddenPart: "hpk_12_",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"-config", "/tmp/server.json"}, &stdout, &stderr, func(context.Context, serverapp.Options) error { return test.err })
			if code != test.code {
				t.Fatalf("code = %d", code)
			}
			for _, want := range test.wantParts {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want to contain %q", stderr.String(), want)
				}
			}
			if test.forbiddenPart != "" && strings.Contains(stderr.String(), test.forbiddenPart) {
				t.Errorf("stderr contains forbidden value %q: %q", test.forbiddenPart, stderr.String())
			}
		})
	}
}
