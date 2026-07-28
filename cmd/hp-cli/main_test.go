package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminclient"
	"github.com/wenxichang/herdr-pal/internal/adminproto"
	"github.com/wenxichang/herdr-pal/internal/version"
)

func TestParseCommandsCoversManagementSurface(t *testing.T) {
	tests := []struct {
		args   []string
		method adminproto.Method
	}{
		{[]string{"server", "status"}, adminproto.MethodServerStatus},
		{[]string{"server", "stop"}, adminproto.MethodServerStop},
		{[]string{"server", "debug", "enable"}, adminproto.MethodServerDebugEnable},
		{[]string{"server", "debug", "disable"}, adminproto.MethodServerDebugDisable},
		{[]string{"key", "issue", "--principal-id", "user", "--machine-id", "home", "--source", "10.0.0.1"}, adminproto.MethodKeyIssue},
		{[]string{"key", "list"}, adminproto.MethodKeyList},
		{[]string{"key", "show", "1"}, adminproto.MethodKeyShow},
		{[]string{"key", "enable", "1"}, adminproto.MethodKeyEnable},
		{[]string{"key", "disable", "1"}, adminproto.MethodKeyDisable},
		{[]string{"key", "delete", "1", "--yes"}, adminproto.MethodKeyDelete},
		{[]string{"key", "source", "list", "1"}, adminproto.MethodKeySourceList},
		{[]string{"key", "source", "add", "1", "10.0.0.1"}, adminproto.MethodKeySourceAdd},
		{[]string{"key", "source", "remove", "1", "10.0.0.1"}, adminproto.MethodKeySourceRemove},
		{[]string{"key", "source", "set", "1", "10.0.0.0/24"}, adminproto.MethodKeySourceSet},
		{[]string{"connection", "list"}, adminproto.MethodConnectionList},
		{[]string{"connection", "show", "c-1"}, adminproto.MethodConnectionShow},
		{[]string{"connection", "disconnect", "c-1"}, adminproto.MethodConnectionDisconnect},
		{[]string{"session", "list", "--principal-id", "user", "--machine-id", "home"}, adminproto.MethodSessionList},
	}
	for _, test := range tests {
		options, err := parseArgs(test.args)
		if err != nil {
			t.Fatalf("parseArgs(%v) error = %v", test.args, err)
		}
		if options.Invocation.Method != test.method {
			t.Fatalf("parseArgs(%v) method = %s, want %s", test.args, options.Invocation.Method, test.method)
		}
	}
}

func TestParseIssueSourcesExpiryJSONAndConfig(t *testing.T) {
	options, err := parseArgs([]string{
		"-config", "/tmp/server.json", "key", "issue", "--principal-id", "user", "--machine-id", "home",
		"--source", "10.0.0.1,10.0.0.2", "--source", "192.168.1.0/24", "--expires-at", "2026-08-01T00:00:00+08:00", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	params := options.Invocation.Params.(adminproto.KeyIssueParams)
	if options.ConfigPath != "/tmp/server.json" || !options.JSON || len(params.Sources) != 2 || params.Sources[0] != "10.0.0.1,10.0.0.2" || params.ExpiresAt == nil {
		t.Fatalf("parse options = %#v params=%#v", options, params)
	}
}

func TestParseRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		{"key", "show", "0"},
		{"key", "show", "0x10"},
		{"key", "show", "01"},
		{"key", "delete", "1"},
		{"key", "issue", "--principal-id", "user", "--machine-id", "home", "--source", "10.0.0.1", "--expires-at", "tomorrow"},
		{"key", "source", "add", "1"},
		{"session", "list", "--unknown"},
		{"unknown"},
	}
	for _, args := range tests {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("parseArgs(%v) should fail", args)
		}
	}
}

func TestRunUsesDefaultConfigVersionJSONAndExitCodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	originalVersion := version.Version
	t.Cleanup(func() { version.Version = originalVersion })
	version.Version = "v9.9.9"

	var stdout, stderr bytes.Buffer
	var gotPath string
	var gotInvocation Invocation
	code := run(context.Background(), []string{"key", "list", "--json"}, &stdout, &stderr, func(_ context.Context, configPath string, invocation Invocation) (any, error) {
		gotPath, gotInvocation = configPath, invocation
		return adminproto.KeyListResult{}, nil
	})
	wantPath := filepath.Join(home, ".config", "herdr-pal", "server-config.json")
	var jsonResult adminproto.KeyListResult
	jsonErr := json.Unmarshal(stdout.Bytes(), &jsonResult)
	if code != 0 || gotPath != wantPath || gotInvocation.Method != adminproto.MethodKeyList || jsonErr != nil {
		t.Fatalf("run default = code:%d path:%q invocation:%#v stdout:%q stderr:%q", code, gotPath, gotInvocation, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"--version"}, &stdout, &stderr, nil); code != 0 || !strings.Contains(stdout.String(), "v9.9.9") {
		t.Fatalf("version run = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	tests := []struct {
		name string
		err  error
		code int
	}{
		{"business", &adminclient.ServerError{Code: adminproto.CodeCredentialNotFound, Message: "Key 不存在"}, 1},
		{"config", errLocalConfig, 2},
		{"transport", adminclient.ErrTransport, 3},
		{"protocol", adminclient.ErrProtocol, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			code := run(context.Background(), []string{"server", "status"}, &stdout, &stderr, func(context.Context, string, Invocation) (any, error) { return nil, test.err })
			if code != test.code {
				t.Fatalf("run error code = %d, want %d", code, test.code)
			}
		})
	}
	if code := run(context.Background(), []string{"key", "show", "bad"}, &stdout, &stderr, func(context.Context, string, Invocation) (any, error) {
		t.Fatal("executor should not run")
		return nil, nil
	}); code != 2 {
		t.Fatalf("parse error code = %d", code)
	}
}

func TestExecuteInvocationUsesExpectedTypedResult(t *testing.T) {
	invocation := Invocation{Method: adminproto.MethodKeyShow, Params: adminproto.CredentialIDParams{CredentialID: 1}}
	if !reflect.DeepEqual(invocation.Params, adminproto.CredentialIDParams{CredentialID: 1}) {
		t.Fatalf("invocation params = %#v", invocation.Params)
	}
	if !errors.Is(classifyExecutionError(adminclient.ErrTransport), adminclient.ErrTransport) {
		t.Fatal("transport error classification changed")
	}
}
