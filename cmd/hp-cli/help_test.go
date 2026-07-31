package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestRootHelpListsAllCommandsWithoutExecutor(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	called := false
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr,
		func(context.Context, string, Invocation) (any, error) {
			called = true
			return nil, nil
		})
	if code != 0 || called || stderr.Len() != 0 {
		t.Fatalf("root help = code:%d called:%t stdout:%q stderr:%q", code, called, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"server status", "server debug enable", "key issue", "key source add",
		"connection disconnect", "session list", "--config", "--json", "--version", "显示帮助", "显示版本",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("root help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRootHelpCommandTraversalIncludesGroupsAndLeaves(t *testing.T) {
	root := newRootCommand(&commandState{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard})
	got := make(map[string]bool)
	for _, command := range availableHelpCommands(root) {
		got[command.CommandPath()] = true
	}
	for _, want := range []string{
		"hp-cli server", "hp-cli server debug", "hp-cli server status",
		"hp-cli key", "hp-cli key source", "hp-cli key source add",
		"hp-cli connection", "hp-cli connection list", "hp-cli session", "hp-cli session list",
	} {
		if !got[want] {
			t.Fatalf("help command traversal missing %q: %#v", want, got)
		}
	}
}

func TestAllBusinessCommandsHaveDetailedHelpAndExamples(t *testing.T) {
	root := newRootCommand(&commandState{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard})
	commands := append([]*cobra.Command{root}, availableHelpCommands(root)...)
	for _, command := range commands {
		if strings.TrimSpace(command.Short) == "" || strings.TrimSpace(command.Long) == "" || strings.TrimSpace(command.Example) == "" {
			t.Fatalf("command %q has incomplete help: short=%q long=%q example=%q", command.CommandPath(), command.Short, command.Long, command.Example)
		}
	}
}

func TestEveryBusinessCommandHelpDoesNotExecute(t *testing.T) {
	metadataRoot := newRootCommand(&commandState{ctx: context.Background(), stdout: io.Discard, stderr: io.Discard})
	for _, metadata := range availableHelpCommands(metadataRoot) {
		args := append(strings.Fields(metadata.CommandPath())[1:], "--help")
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		called := false
		code := run(context.Background(), args, &stdout, &stderr, func(context.Context, string, Invocation) (any, error) {
			called = true
			return nil, nil
		})
		if code != 0 || called || stderr.Len() != 0 || !strings.Contains(stdout.String(), metadata.Long) ||
			!strings.Contains(stdout.String(), "用法：") || !strings.Contains(stdout.String(), "示例：") {
			t.Fatalf("help %v = code:%d called:%t stdout:%q stderr:%q", args, code, called, stdout.String(), stderr.String())
		}
	}
}

func TestCobraExitCodesAndUsageScope(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		executeErr error
		code       int
		want       string
		wantHelp   string
	}{
		{"invalid argument", []string{"key", "show", "bad"}, nil, 2, "参数错误：", "hp-cli key show"},
		{"unknown key command", []string{"key", "unknown"}, nil, 2, "参数错误：", "可用子命令"},
		{"business", []string{"server", "status"}, &adminclient.ServerError{Code: adminproto.CodeCredentialNotFound}, 1, "请求失败：", ""},
		{"config", []string{"server", "status"}, errLocalConfig, 2, "配置错误：", ""},
		{"transport", []string{"server", "status"}, adminclient.ErrTransport, 3, "Admin Socket 请求失败：", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(context.Background(), test.args, &stdout, &stderr,
				func(_ context.Context, _ string, invocation Invocation) (any, error) {
					if test.executeErr != nil {
						return nil, test.executeErr
					}
					return emptyCLIResult(invocation.Method), nil
				})
			if code != test.code || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("run() = code:%d stdout:%q stderr:%q", code, stdout.String(), stderr.String())
			}
			if test.wantHelp != "" && !strings.Contains(stderr.String(), test.wantHelp) {
				t.Fatalf("stderr missing nearest help %q: %s", test.wantHelp, stderr.String())
			}
			if test.wantHelp == "" && strings.Contains(stderr.String(), "用法：") {
				t.Fatalf("non-usage error printed help: %s", stderr.String())
			}
		})
	}
}

func TestHierarchicalHelpForms(t *testing.T) {
	tests := []struct {
		args  []string
		wants []string
	}{
		{[]string{"help", "key"}, []string{"签发、查看、启用、禁用和删除", "issue", "delete", "source"}},
		{[]string{"key", "--help"}, []string{"issue", "delete", "source"}},
		{[]string{"help", "key", "issue"}, []string{"--principal-id", "--machine-id", "--source", "RFC3339", "可以重复"}},
		{[]string{"key", "source", "add", "-h"}, []string{"<credential-id> <source>...", "单 IP", "CIDR", "闭区间"}},
	}
	for _, test := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		called := false
		code := run(context.Background(), test.args, &stdout, &stderr,
			func(context.Context, string, Invocation) (any, error) {
				called = true
				return nil, nil
			})
		if code != 0 || called || stderr.Len() != 0 {
			t.Fatalf("help %v = code:%d called:%t stderr:%q", test.args, code, called, stderr.String())
		}
		for _, want := range test.wants {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("help %v missing %q:\n%s", test.args, want, stdout.String())
			}
		}
	}
}

func TestLegacyGlobalOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })
	version.Version = "v9.9.9"

	for _, args := range [][]string{
		{"-config", "/tmp/server-a.json", "server", "status"},
		{"-config=/tmp/server-b.json", "server", "status"},
	} {
		var gotPath string
		code := run(context.Background(), args, io.Discard, io.Discard,
			func(_ context.Context, configPath string, _ Invocation) (any, error) {
				gotPath = configPath
				return adminproto.ServerStatusResult{}, nil
			})
		wantPath := strings.TrimPrefix(strings.TrimPrefix(args[0], "-config="), "-config")
		if wantPath == "" {
			wantPath = args[1]
		}
		if code != 0 || gotPath != wantPath {
			t.Fatalf("legacy config %v = code:%d path:%q want:%q", args, code, gotPath, wantPath)
		}
	}

	var stdout bytes.Buffer
	if code := run(context.Background(), []string{"-version"}, &stdout, io.Discard, nil); code != 0 || !strings.Contains(stdout.String(), "v9.9.9") {
		t.Fatalf("legacy version = code:%d stdout:%q", code, stdout.String())
	}

	stdout.Reset()
	code := run(context.Background(), []string{"server", "status", "--json"}, &stdout, io.Discard,
		func(_ context.Context, configPath string, _ Invocation) (any, error) {
			if configPath != filepath.Join(home, ".config", "herdr-pal-server", "server.json") {
				t.Fatalf("default config path = %q", configPath)
			}
			return adminproto.ServerStatusResult{}, nil
		})
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(stdout.String()), "{") {
		t.Fatalf("trailing global flag = code:%d stdout:%q", code, stdout.String())
	}
}
