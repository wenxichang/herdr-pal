package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/installer"
)

func TestRunSetupPassesValidatedRequest(t *testing.T) {
	directory := t.TempDir()
	clientPath := filepath.Join(directory, "pal.json")
	herdrPath := filepath.Join(directory, "herdr.toml")
	herdrBinary := filepath.Join(directory, "herdr")
	var got installer.Request
	calls := 0
	execute := func(_ context.Context, request installer.Request, _ installer.Options) (installer.Result, error) {
		calls++
		got = request
		return installer.Result{
			ClientBackupPath: clientPath + ".bak",
			HerdrBackupPath:  herdrPath + ".bak",
		}, nil
	}
	var stdout, stderr bytes.Buffer

	code := runSetup(context.Background(), []string{
		"--url", "wss://relay.example/hprp",
		"--config", clientPath,
		"--herdr-config", herdrPath,
		"--herdr-bin", herdrBinary,
	}, strings.NewReader(validSetupKey+"\n"), &stdout, &stderr, execute)

	if code != 0 {
		t.Fatalf("runSetup() = %d, stderr = %q", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("execute calls = %d, want 1", calls)
	}
	if got.ClientConfigPath != clientPath || got.HerdrConfigPath != herdrPath || got.HerdrBinaryPath != herdrBinary || got.RelayURL != "wss://relay.example/hprp" || got.RelayKey != validSetupKey {
		t.Fatalf("request = %+v", got)
	}
	for _, want := range []string{clientPath, herdrPath, "备份"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want %q", stdout.String(), want)
		}
	}
	if strings.Contains(stdout.String(), validSetupKey) || strings.Contains(stderr.String(), validSetupKey) {
		t.Fatal("output exposes key")
	}
}

func TestRunSetupUsesDefaultConfigPaths(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	var got installer.Request
	var stdout, stderr bytes.Buffer

	code := runSetup(context.Background(), []string{
		"--url", "wss://relay.example/hprp",
		"--herdr-bin", "/tmp/herdr",
	}, strings.NewReader(validSetupKey+"\n"), &stdout, &stderr, func(_ context.Context, request installer.Request, _ installer.Options) (installer.Result, error) {
		got = request
		return installer.Result{}, nil
	})

	if code != 0 {
		t.Fatalf("runSetup() = %d, stderr = %q", code, stderr.String())
	}
	if got.ClientConfigPath != filepath.Join(home, ".config", "herdr-pal", "config.json") {
		t.Fatalf("client path = %q", got.ClientConfigPath)
	}
	if got.HerdrConfigPath != filepath.Join(xdg, "herdr", "config.toml") {
		t.Fatalf("herdr path = %q", got.HerdrConfigPath)
	}
}

func TestRunSetupRejectsInvalidArgumentsAndInput(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{name: "missing url", args: []string{"--herdr-bin", "/tmp/herdr"}, stdin: validSetupKey + "\n"},
		{name: "missing herdr binary", args: []string{"--url", "wss://relay.example/hprp"}, stdin: validSetupKey + "\n"},
		{name: "empty key", args: []string{"--url", "wss://relay.example/hprp", "--herdr-bin", "/tmp/herdr"}, stdin: "\n"},
		{name: "extra input", args: []string{"--url", "wss://relay.example/hprp", "--herdr-bin", "/tmp/herdr"}, stdin: validSetupKey + "\nextra\n"},
		{name: "unknown flag", args: []string{"--unknown"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			code := runSetup(context.Background(), test.args, strings.NewReader(test.stdin), &stdout, &stderr, func(context.Context, installer.Request, installer.Options) (installer.Result, error) {
				calls++
				return installer.Result{}, nil
			})
			if code != 2 {
				t.Fatalf("runSetup() = %d, want 2; stderr = %q", code, stderr.String())
			}
			if calls != 0 {
				t.Fatalf("execute calls = %d, want 0", calls)
			}
		})
	}
}

func TestRunSetupRedactsKeyFromExecutorError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := runSetup(context.Background(), []string{
		"--url", "wss://relay.example/hprp",
		"--herdr-bin", "/tmp/herdr",
	}, strings.NewReader(validSetupKey+"\n"), &stdout, &stderr, func(context.Context, installer.Request, installer.Options) (installer.Result, error) {
		return installer.Result{}, errors.New("failed for " + validSetupKey)
	})

	if code != 1 {
		t.Fatalf("runSetup() = %d, want 1", code)
	}
	if strings.Contains(stderr.String(), validSetupKey) || !strings.Contains(stderr.String(), "[已隐藏]") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

const validSetupKey = "hpk_7_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
